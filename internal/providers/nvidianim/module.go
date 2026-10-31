package nvidianim

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

// Persist admission, not completion: usage notifications are best-effort and
// cannot protect money. Failed, retried and interrupted requests consume a
// conservative reservation too. Only the rolling RPM window refills; the
// optional lifetime request ceiling and an observed credit exhaustion do not.
type allowanceState struct {
	Version       int         `json:"version"`
	Reserved      int64       `json:"reserved"`
	Recent        []time.Time `json:"recent"`
	CooldownUntil time.Time   `json:"cooldown_until"`
	FreeExhausted bool        `json:"free_exhausted"`
}

type Module struct {
	mu       sync.Mutex
	cfg      moduleConfig
	lanes    map[string]lane
	state    allowanceState
	store    *securestore.Store
	stateErr bool
	closed   bool
	cursor   uint64
	now      func() time.Time
}

var _ providermodule.Module = (*Module)(nil)
var _ providermodule.UsageAttributor = (*Module)(nil)
var _ providermodule.ConfigValidator = (*Module)(nil)

func NewModule() *Module       { return &Module{now: func() time.Time { return time.Now().UTC() }} }
func (m *Module) ID() string   { return providerID }
func (m *Module) active() bool { return !m.closed && m.cfg.Enabled && len(m.lanes) > 0 }
func (m *Module) Recognize(c pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return false
	}
	if _, ok := m.lanes[c.ID]; ok {
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
	lanes := discover(host, cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("nvidia_nim_closed")
	}
	state := allowanceState{Version: 1}
	var store *securestore.Store
	if cfg.Enabled && len(lanes) > 0 {
		if !filepath.IsAbs(host.AuthDir) {
			return fmt.Errorf("nvidia_nim_state_unavailable: absolute auth-dir required")
		}
		store, err = securestore.New(filepath.Join(host.AuthDir, "providers", providerID))
		if err != nil {
			return fmt.Errorf("nvidia_nim_state_unavailable: cannot open allowance store")
		}
		_, err = store.ReadJSONIfExists("allowance.state", &state)
		if err != nil || state.Version != 1 || state.Reserved < 0 || len(state.Recent) > 100000 {
			_ = store.Close()
			return fmt.Errorf("nvidia_nim_state_unavailable: invalid allowance state")
		}
	}
	if m.store != nil {
		_ = m.store.Close()
	}
	m.cfg, m.lanes, m.state, m.store, m.stateErr = cfg, lanes, state, store, false
	if host.Clock != nil {
		m.now = host.Clock.Now
	}
	return nil
}

// ValidateConfig implements providermodule.ConfigValidator: it runs the
// parse and lane discovery Reconfigure would, without opening the
// allowance store.
func (m *Module) ValidateConfig(host providermodule.HostConfig, raw json.RawMessage) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	if cfg.Enabled && len(discover(host, cfg)) > 0 && !filepath.IsAbs(host.AuthDir) {
		return fmt.Errorf("nvidia_nim_state_unavailable: absolute auth-dir required")
	}
	return nil
}
func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return nil
	}
	ids := make([]string, 0, len(m.lanes))
	for id := range m.lanes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (m *Module) prune(now time.Time) {
	out := m.state.Recent[:0]
	for _, t := range m.state.Recent {
		if t.After(now.Add(-time.Minute)) {
			out = append(out, t)
		}
	}
	m.state.Recent = out
}
func (m *Module) save() error {
	if m.store == nil || m.store.WriteJSON("allowance.state", m.state) != nil {
		m.stateErr = true
		return fmt.Errorf("nvidia_nim_state_unavailable: reservation not durable")
	}
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
	if m.cfg.StayWithinFree && strings.TrimSpace(r.Model) == "" {
		return pluginapi.SchedulerPickResponse{}, noCapacity("unverified_model")
	}
	ids := []string{}
	for _, c := range r.Candidates {
		l, ok := m.lanes[c.ID]
		if !ok {
			return pluginapi.SchedulerPickResponse{}, noCapacity("unbound_credential")
		}
		if m.reason(l, r.Model, now) == "" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return pluginapi.SchedulerPickResponse{}, noCapacity("free_or_rate_boundary")
	}
	if m.state.Reserved == math.MaxInt64 {
		return pluginapi.SchedulerPickResponse{}, noCapacity("request_counter_full")
	}
	sort.Strings(ids)
	id := ids[m.cursor%uint64(len(ids))]
	m.state.Reserved++
	m.state.Recent = append(m.state.Recent, now)
	if err := m.save(); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	m.cursor++
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, nil
}
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
	if m.cfg.RequestsPerMinute > 0 && len(m.state.Recent) >= m.cfg.RequestsPerMinute {
		return "rate_limited"
	}
	if !m.cfg.StayWithinFree {
		return ""
	}
	if !m.cfg.FreeTierConfirmed || m.cfg.RequestsPerMinute == 0 {
		return "allowance_unverified"
	}
	if !l.freeEndpoint {
		return "unverified_endpoint"
	}
	if m.state.FreeExhausted {
		return "free_exhausted"
	}
	if !m.cfg.TrialExpiresAt.IsZero() && !now.Before(m.cfg.TrialExpiresAt) {
		return "trial_expired"
	}
	if m.cfg.FreeRequestLimit > 0 && m.state.Reserved >= m.cfg.FreeRequestLimit {
		return "free_exhausted"
	}
	if len(l.models) == 0 || (model != "" && !l.models[providermodule.RouteKey(providerID, model)]) {
		return "unverified_model"
	}
	return ""
}

type schedulerError struct{ reason string }

func (e schedulerError) Error() string { return "nvidia_nim_no_capacity: " + e.reason }
func (e schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: "nvidia_nim_no_capacity", Message: e.Error(), Retryable: true, HTTPStatus: 429}
}
func noCapacity(reason string) error { return schedulerError{reason: reason} }

func (m *Module) UsageAccount(r pluginapi.UsageRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lanes[r.AuthID].name
}
func (m *Module) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.store != nil {
		return m.store.Close()
	}
	return nil
}
