package openrouterfree

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	maxChanges   = 50
	kickDebounce = time.Minute
)

// change is one entry of the model-list audit trail (public catalog ids only).
type change struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"` // "added" | "removed"
	Model  string    `json:"model"`
}

// Persist admission, not completion: usage notifications are best-effort and
// the free-model limits are per account, so every admitted request counts
// whether or not it completes. Models/CatalogAt are the last verified free
// list, kept across restarts so a restart can neither widen nor lose it.
type state struct {
	Version       int         `json:"version"`
	Day           string      `json:"day"`
	Used          int64       `json:"used"`
	Recent        []time.Time `json:"recent"`
	CooldownUntil time.Time   `json:"cooldown_until"`
	Models        []string    `json:"models"`
	CatalogAt     time.Time   `json:"catalog_at"`
	DailyLimit    int64       `json:"daily_limit"`
	LimitAt       time.Time   `json:"limit_at"`
	Changes       []change    `json:"changes"`
}

// syncInfo is the in-memory outcome of the latest sync pass.
type syncInfo struct {
	LastAttempt time.Time
	LastError   string // closed vocabulary, never upstream text
	Reconcile   string
	PushedAt    time.Time
}

type Module struct {
	mu         sync.Mutex
	syncMu     sync.Mutex
	cfg        moduleConfig
	view       hostView
	generation uint64 // invalidates sync results across successful reconfigures
	state      state
	free       map[string]bool // ModelKey-folded state.Models
	sync       syncInfo
	store      *securestore.Store
	client     providermodule.HTTPDoer
	stateErr   bool
	closed     bool
	cursor     uint64
	now        func() time.Time
	logf       func(format string, args ...any)

	started bool
	noLoop  bool
	cancel  context.CancelFunc
	done    chan struct{}
	kick    chan struct{}
}

var _ providermodule.Module = (*Module)(nil)
var _ providermodule.UsageAttributor = (*Module)(nil)

func NewModule() *Module {
	return &Module{now: func() time.Time { return time.Now().UTC() }, kick: make(chan struct{}, 1), free: map[string]bool{}, logf: logPrintf}
}
func (m *Module) ID() string   { return providerID }
func (m *Module) active() bool { return !m.closed && m.cfg.Enabled && len(m.view.lanes) > 0 }

func (m *Module) Recognize(c pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return false
	}
	if _, ok := m.view.lanes[c.ID]; ok {
		return true
	}
	for _, s := range []string{c.Provider, c.Attributes["compat_name"], c.Attributes["provider_key"]} {
		if strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "openai-compatible-") == providerID {
			return true
		}
	}
	return false
}

func (m *Module) Reconfigure(_ context.Context, host providermodule.HostConfig, raw json.RawMessage) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	view := discover(host)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("openrouter_free_closed")
	}
	st := state{Version: 1}
	var store *securestore.Store
	if cfg.Enabled && len(view.lanes) > 0 {
		if !filepath.IsAbs(host.AuthDir) {
			return fmt.Errorf("openrouter_free_state_unavailable: absolute auth-dir required")
		}
		store, err = securestore.New(filepath.Join(host.AuthDir, "providers", providerID))
		if err != nil {
			return fmt.Errorf("openrouter_free_state_unavailable: cannot open allowance store")
		}
		_, err = store.ReadJSONIfExists("allowance.state", &st)
		if err != nil || !validState(st) {
			_ = store.Close()
			return fmt.Errorf("openrouter_free_state_unavailable: invalid allowance state")
		}
	}
	credentialsChanged := view.apiKey != m.view.apiKey || len(view.lanes) != len(m.view.lanes)
	for id := range view.lanes {
		if _, ok := m.view.lanes[id]; !ok {
			credentialsChanged = true
		}
	}
	if credentialsChanged || !m.active() {
		// Entitlement is credential-specific, unlike the shared conservative
		// usage debit. A restart or reenable has no verified active binding.
		st.DailyLimit, st.LimitAt = 0, time.Time{}
	} else {
		// Only this process's live binding may carry entitlement forward.
		// Reloading it from disk could undo an unpersisted invalidation.
		st.DailyLimit, st.LimitAt = m.state.DailyLimit, m.state.LimitAt
	}
	if m.store != nil {
		_ = m.store.Close()
	}
	m.cfg, m.view, m.store, m.stateErr = cfg, view, store, false
	m.generation++
	m.setState(st)
	if host.Clock != nil {
		m.now = host.Clock.Now
	}
	m.client = host.HTTPClient
	if m.client == nil {
		m.client = newHTTPClient()
	}
	if m.active() && !m.noLoop {
		if !m.started {
			ctx, cancel := context.WithCancel(context.Background())
			m.cancel, m.done, m.started = cancel, make(chan struct{}), true
			go m.syncLoop(ctx)
		} else if credentialsChanged || m.now().Sub(m.sync.LastAttempt) >= kickDebounce {
			select {
			case m.kick <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func validState(s state) bool {
	if s.Version != 1 || s.Used < 0 || s.DailyLimit < 0 || len(s.Recent) > 100000 || len(s.Models) > 10000 || len(s.Changes) > maxChanges {
		return false
	}
	for _, id := range s.Models {
		if !strings.HasSuffix(strings.ToLower(id), freeSuffix) {
			return false
		}
	}
	return true
}

// setState installs s and rebuilds the folded free set; callers hold mu.
func (m *Module) setState(s state) {
	m.state = s
	m.free = make(map[string]bool, len(s.Models))
	for _, id := range s.Models {
		m.free[providermodule.ModelKey(strings.TrimSpace(id))] = true
	}
}

func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return nil
	}
	ids := make([]string, 0, len(m.view.lanes))
	for id := range m.view.lanes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ModelKey folds to the smallest rune of each case class, so :free is :FREE.
var foldedFreeSuffix = providermodule.ModelKey(freeSuffix)

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// prune drops RPM slots older than a minute and rolls the daily counter at
// 00:00 UTC; callers hold mu.
func (m *Module) prune(now time.Time) {
	out := m.state.Recent[:0]
	for _, t := range m.state.Recent {
		if t.After(now.Add(-time.Minute)) {
			out = append(out, t)
		}
	}
	m.state.Recent = out
	if day := utcDay(now); m.state.Day != day {
		m.state.Day, m.state.Used = day, 0
	}
}

func (m *Module) dailyLimit() (int64, string) {
	limit, source := m.state.DailyLimit, "openrouter_key"
	if limit <= 0 {
		limit, source = lowestDailyLimit, "assumed_lowest"
	}
	if m.cfg.DailyRequestLimit > 0 && m.cfg.DailyRequestLimit < limit {
		limit, source = m.cfg.DailyRequestLimit, "operator_cap"
	}
	return limit, source
}

func (m *Module) save() error {
	if m.store == nil || m.store.WriteJSON("allowance.state", m.state) != nil {
		m.stateErr = true
		return fmt.Errorf("openrouter_free_state_unavailable: reservation not durable")
	}
	m.stateErr = false
	return nil
}

func (m *Module) Pick(_ context.Context, r pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	now := m.now().UTC()
	m.prune(now)
	if m.stateErr {
		return pluginapi.SchedulerPickResponse{}, noCapacity("state_unavailable")
	}
	if strings.TrimSpace(r.Model) == "" {
		return pluginapi.SchedulerPickResponse{}, noCapacity("unverified_model")
	}
	ids := []string{}
	var firstReason string
	for _, c := range r.Candidates {
		l, ok := m.view.lanes[c.ID]
		if !ok {
			return pluginapi.SchedulerPickResponse{}, noCapacity("unbound_credential")
		}
		reason := m.reason(l, r.Model, now)
		if reason == "" {
			ids = append(ids, c.ID)
		} else if firstReason == "" {
			firstReason = reason
		}
	}
	if len(ids) == 0 {
		if firstReason == "" {
			firstReason = "free_or_rate_boundary"
		}
		return pluginapi.SchedulerPickResponse{}, noCapacity(firstReason)
	}
	if m.state.Used == math.MaxInt64 {
		return pluginapi.SchedulerPickResponse{}, noCapacity("request_counter_full")
	}
	sort.Strings(ids)
	id := ids[m.cursor%uint64(len(ids))]
	m.state.Used++
	m.state.Recent = append(m.state.Recent, now)
	if err := m.save(); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	m.cursor++
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, nil
}

// reason is "" when l may serve model now. The free-model checks hold even
// with stayWithinFree off, and so is catalog freshness: nothing but a verified
// :free id ever goes through this provider. stayWithinFree only governs the
// local rate and daily limits.
func (m *Module) reason(l lane, model string, now time.Time) string {
	if !l.bound {
		return "placeholder_key"
	}
	if m.stateErr {
		return "state_unavailable"
	}
	if m.state.CooldownUntil.After(now) {
		return "rate_limited"
	}
	if !l.freeEndpoint {
		return "unverified_endpoint"
	}
	if len(m.free) == 0 || m.state.CatalogAt.IsZero() {
		return "catalog_unverified"
	}
	if model != "" {
		upstreams := l.routes[providermodule.RouteKey(providerID, model)]
		if len(upstreams) == 0 {
			return "unverified_model"
		}
		for _, u := range upstreams {
			if !m.free[u] || !strings.HasSuffix(u, foldedFreeSuffix) {
				return "model_not_free"
			}
		}
	}
	// A stale list may name a model OpenRouter has since repriced, so this
	// holds even with stayWithinFree off.
	if now.Sub(m.state.CatalogAt) > m.cfg.CatalogMaxAge {
		return "stale_catalog"
	}
	if !m.cfg.StayWithinFree {
		return ""
	}
	if len(m.state.Recent) >= m.cfg.RequestsPerMinute {
		return "rate_limited"
	}
	if limit, _ := m.dailyLimit(); m.state.Used >= limit {
		return "daily_limit"
	}
	return ""
}

type schedulerError struct{ reason string }

func (e schedulerError) Error() string { return "openrouter_free_no_capacity: " + e.reason }
func (e schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: "openrouter_free_no_capacity", Message: e.Error(), Retryable: true, HTTPStatus: 429}
}
func noCapacity(reason string) error { return schedulerError{reason: reason} }

func (m *Module) UsageAccount(r pluginapi.UsageRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view.lanes[r.AuthID].name
}

func (m *Module) Close(context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancel, done, store := m.cancel, m.done, m.store
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if store != nil {
		return store.Close()
	}
	return nil
}
