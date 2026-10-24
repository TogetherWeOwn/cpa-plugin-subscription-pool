package cloudflare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type lane struct {
	account      string
	bound        bool
	busyUntil    time.Time
	reservations map[string]float64
}
type account struct {
	token      string
	remote     float64
	readingDay string
	observed   time.Time
	pollAt     time.Time
	pollError  string
}

type budget struct {
	Day            string               `json:"day"`
	Committed      float64              `json:"committed_neurons"`
	Carry          float64              `json:"in_flight_batch_neurons"`
	UnsafeDay      string               `json:"payg_day,omitempty"`
	UnboundedBatch bool                 `json:"unbounded_batch"`
	Pending        map[string][]float64 `json:"pending_reservations"`
}
type diskState struct {
	Version  int                `json:"version"`
	Accounts map[string]*budget `json:"accounts"`
}

// Module is a local routing guard, NOT a Cloudflare billing/spend-cap API.
// Adaptive/delayed analytics and callers outside this plugin prevent an
// absolute zero-charge guarantee on a shared Workers Paid account.
type Module struct {
	mu         sync.Mutex
	pollMu     sync.Mutex
	cfg        moduleConfig
	lanes      map[string]*lane
	accounts   map[string]*account
	budgets    map[string]*budget
	providers  map[string]bool
	store      *securestore.Store
	stateErr   string
	now        func() time.Time
	client     providermodule.HTTPDoer
	generation uint64
	cursor     uint64
	closed     bool
	noPoller   bool
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
}

var _ providermodule.Module = (*Module)(nil)
var _ providermodule.UsageAttributor = (*Module)(nil)

func NewModule() *Module             { return &Module{now: func() time.Time { return time.Now().UTC() }} }
func (m *Module) ID() string         { return providerID }
func (m *Module) activeLocked() bool { return !m.closed && m.cfg.Enabled && len(m.lanes) > 0 }
func (m *Module) ownsLocked(c pluginapi.SchedulerAuthCandidate) bool {
	if m.lanes[c.ID] != nil {
		return true
	}
	for _, v := range []string{c.Provider, c.Attributes["compat_name"], c.Attributes["provider_key"]} {
		if m.providers[canonicalProvider(v)] {
			return true
		}
	}
	return false
}
func (m *Module) Recognize(c pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeLocked() && m.ownsLocked(c)
}

func (m *Module) Reconfigure(_ context.Context, host providermodule.HostConfig, raw json.RawMessage) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	providers := map[string]bool{}
	for _, name := range cfg.Providers {
		providers[name] = true
	}
	lanes := map[string]*lane{}
	accounts := map[string]*account{}
	counters := map[string]int{}
	for _, compat := range host.OpenAICompatibility {
		name := strings.ToLower(strings.TrimSpace(compat.Name))
		if compat.Disabled || !providers[name] {
			continue
		}
		id, err := accountID(compat.BaseURL)
		if err != nil {
			return err
		}
		if len(compat.APIKeyEntries) == 0 {
			return fmt.Errorf("cloudflare_invalid_config: Workers AI requires an API key entry")
		}
		reservations := configuredReservations(name, compat.Models)
		for _, entry := range compat.APIKeyEntries {
			key := strings.TrimSpace(entry.APIKey)
			bound := key != "" && !strings.ContainsAny(key, "<>$ {}") && !strings.Contains(strings.ToLower(key), "placeholder")
			kind := "openai-compatibility:" + name
			h := sha256.Sum256([]byte(kind + "\x00" + key + "\x00" + strings.TrimSpace(compat.BaseURL) + "\x00" + strings.TrimSpace(entry.ProxyURL))) // codeql[go/weak-sensitive-data-hashing] Upstream non-security ID contract.
			auth := kind + ":" + hex.EncodeToString(h[:])[:12]
			n := counters[auth]
			counters[auth]++
			if n > 0 {
				auth += fmt.Sprintf("-%d", n)
			}
			lanes[auth] = &lane{account: id, bound: bound, reservations: reservations}
			if accounts[id] == nil || accounts[id].token == "" {
				token := ""
				if bound {
					token = key
				}
				accounts[id] = &account{token: token}
			}
		}
	}
	if !cfg.Enabled && len(lanes) > 0 {
		return fmt.Errorf("cloudflare_invalid_config: disable the host provider before disabling its guard; use stayWithinIncluded=false for PAYG")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("cloudflare_closed")
	}
	if cfg.Enabled && len(lanes) > 0 {
		dir := filepath.Join(host.AuthDir, "providers", providerID)
		if host.AuthDir == "" || !filepath.IsAbs(host.AuthDir) {
			return fmt.Errorf("cloudflare_invalid_config: absolute auth-dir is required")
		}
		if m.store != nil && m.store.Dir() != dir {
			return fmt.Errorf("cloudflare_invalid_config: auth-dir cannot change while module is running")
		}
		if m.store == nil {
			store, err := securestore.New(dir)
			if err != nil {
				return fmt.Errorf("cloudflare_state_open_failed")
			}
			var state diskState
			exists, readErr := store.ReadJSONIfExists("budget.json", &state)
			if readErr != nil || exists && !validState(state) {
				_ = store.Close()
				return fmt.Errorf("cloudflare_state_read_failed")
			}
			if !exists {
				state = diskState{Version: 1, Accounts: map[string]*budget{}}
			}
			m.store = store
			m.budgets = state.Accounts
		}
	}
	for id, a := range accounts {
		if old := m.accounts[id]; old != nil {
			a.remote = old.remote
			a.readingDay = old.readingDay
			a.observed = old.observed
			a.pollError = old.pollError
		}
	}
	for id, l := range lanes {
		if old := m.lanes[id]; old != nil {
			l.busyUntil = old.busyUntil
		}
	}
	if m.budgets == nil {
		m.budgets = map[string]*budget{}
	}
	m.cfg = cfg
	m.providers = providers
	m.lanes = lanes
	m.accounts = accounts
	m.generation++
	if host.Clock != nil {
		m.now = host.Clock.Now
	}
	m.client = host.HTTPClient
	if m.client == nil {
		m.client = newAnalyticsClient()
	}
	if m.activeLocked() && !m.noPoller && !m.started {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.done = make(chan struct{})
		m.started = true
		go m.pollLoop(ctx)
	}
	return nil
}

func validState(s diskState) bool {
	if s.Version != 1 || s.Accounts == nil {
		return false
	}
	for id, b := range s.Accounts {
		if !accountRE.MatchString(id) || b == nil || !finiteNonnegative(b.Committed) || !finiteNonnegative(b.Carry) {
			return false
		}
		if b.UnsafeDay != "" {
			day, err := time.Parse("2006-01-02", b.UnsafeDay)
			if err != nil || day.Format("2006-01-02") != b.UnsafeDay {
				return false
			}
		}
		day, err := time.Parse("2006-01-02", b.Day)
		if err != nil || day.Format("2006-01-02") != b.Day {
			return false
		}
		if b.Committed < b.Carry || b.UnboundedBatch && b.UnsafeDay == "" || len(b.Pending) == 0 && (b.Carry != 0 || b.UnboundedBatch) {
			return false
		}
		pending := 0.0
		for auth, queue := range b.Pending {
			if auth == "" || len(queue) == 0 {
				return false
			}
			for _, n := range queue {
				if !finiteNonnegative(n) || n == 0 && !b.UnboundedBatch {
					return false
				}
				pending += n
			}
		}
		if !finiteNonnegative(pending) || b.Carry < pending {
			return false
		}
	}
	return true
}
func finiteNonnegative(n float64) bool { return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func (m *Module) budgetLocked(id string, now time.Time) *budget {
	day := utcDay(now).Format("2006-01-02")
	b := m.budgets[id]
	if b == nil {
		b = &budget{Day: day, Pending: map[string][]float64{}}
		m.budgets[id] = b
	}
	if b.Pending == nil {
		b.Pending = map[string][]float64{}
	}
	if b.Day != day {
		// Callbacks contain no unique request identifier and may finish out of
		// order. Carry the ENTIRE active batch until every callback has arrived,
		// rather than subtracting the wrong request's reservation at midnight.
		b.Day = day
		b.Committed = b.Carry
	}
	return b
}

func (m *Module) saveLocked() bool {
	if m.store == nil {
		m.stateErr = "state_unavailable"
		return false
	}
	if err := m.store.WriteJSON("budget.json", diskState{Version: 1, Accounts: m.budgets}); err != nil {
		m.stateErr = "state_write_failed"
		return false
	}
	m.stateErr = ""
	return true
}

func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return nil
	}
	ids := make([]string, 0, len(m.lanes))
	for id := range m.lanes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Module) freshLocked(a *account, now time.Time) bool {
	return a != nil && a.pollError == "" && a.readingDay == utcDay(now).Format("2006-01-02") && !a.observed.IsZero() && !a.observed.After(now) && now.Sub(a.observed) <= m.cfg.MaxAge
}
func (m *Module) effectiveLocked(id string, now time.Time) float64 {
	a := m.accounts[id]
	b := m.budgetLocked(id, now)
	if a == nil || a.readingDay != b.Day {
		return b.Committed
	}
	// Deliberately double-count our already-metered work. Without an exact
	// billing/request join, max(remote, local) can erase in-flight reservations
	// when another consumer raises the analytics reading.
	return a.remote + b.Committed
}

func (m *Module) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() || len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	for _, c := range req.Candidates {
		if !m.ownsLocked(c) {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
	}
	now := m.now().UTC()
	model := req.Model
	for p := range m.providers {
		model = strings.TrimPrefix(model, p+"/")
	}
	ids := make([]string, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	reason := "credential_unbound"
	for offset := range ids {
		id := ids[(int(m.cursor%uint64(len(ids)))+offset)%len(ids)]
		l := m.lanes[id]
		if l == nil || !l.bound {
			continue
		}
		if l.busyUntil.After(now) {
			reason = "provider_cooldown"
			continue
		}
		a := m.accounts[l.account]
		b := m.budgetLocked(l.account, now)
		reserve := l.reservations[model]
		if m.cfg.StayWithinIncluded {
			switch {
			case b.UnsafeDay == b.Day || b.UnboundedBatch:
				reason = "recent_payg_usage"
				continue
			case m.stateErr != "":
				reason = "state_unavailable"
				continue
			case !m.freshLocked(a, now):
				reason = "usage_unknown_or_stale"
				continue
			case reserve <= 0:
				reason = "request_not_bounded"
				continue
			}
			used := m.effectiveLocked(l.account, now)
			ceiling := includedDailyNeurons - m.cfg.SafetyMargin
			if used >= ceiling || used+reserve > ceiling {
				reason = "included_exhausted"
				continue
			}
			// Never refund local debits based on delayed analytics or token usage.
			b.Committed += reserve
			b.Carry += reserve
			b.Pending[id] = append(b.Pending[id], reserve)
			if !m.saveLocked() {
				return pluginapi.SchedulerPickResponse{}, capacityError("state_unavailable")
			}
		} else {
			// Unbounded PAYG work can be absent from delayed analytics. Switching
			// back cannot unlock included traffic until the UTC day after the
			// whole active batch completes. Persist across plugin restarts.
			b.UnsafeDay = b.Day
			b.UnboundedBatch = true
			b.Pending[id] = append(b.Pending[id], 0)
			m.saveLocked() // PAYG is allowed even if metering/storage is unavailable.
		}
		m.cursor++
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, nil
	}
	return pluginapi.SchedulerPickResponse{}, capacityError(reason)
}

type schedulerError struct{ reason string }

func capacityError(reason string) error { return &schedulerError{reason: reason} }
func (e *schedulerError) Error() string { return "cloudflare_no_capacity: " + e.reason }
func (e *schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: "cloudflare_no_capacity", Message: e.Error(), Retryable: true, HTTPStatus: 429}
}

func (m *Module) UsageAccount(r pluginapi.UsageRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.lanes[r.AuthID]; l != nil {
		return m.accountNameLocked(l.account)
	}
	return ""
}
func (m *Module) accountNameLocked(id string) string {
	ids := make([]string, 0, len(m.accounts))
	for id := range m.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, value := range ids {
		if value == id {
			return fmt.Sprintf("workers-ai#%d", i+1)
		}
	}
	return ""
}

func (m *Module) HandleUsage(_ context.Context, r pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return nil
	}
	l := m.lanes[r.AuthID]
	if l == nil {
		return nil
	}
	b := m.budgetLocked(l.account, m.now())
	if queue := b.Pending[r.AuthID]; len(queue) > 0 {
		if b.UnboundedBatch {
			// A PAYG request can finish after midnight and be absent from the
			// new day's analytics. Fence its completion day too; callback order
			// cannot identify which member of a mixed batch was unbounded.
			b.UnsafeDay = b.Day
		}
		if len(queue) == 1 {
			delete(b.Pending, r.AuthID)
		} else {
			b.Pending[r.AuthID] = queue[1:]
		}
		if len(b.Pending) == 0 {
			b.Carry = 0
			b.UnboundedBatch = false
		}
		m.saveLocked()
	}
	if r.Failed {
		// A 429 can be the model's RPM limit, not the daily neuron budget. Do not
		// invent a midnight quota reset from that status or upstream error text.
		switch r.Failure.StatusCode {
		case 401, 403:
			l.busyUntil = m.now().Add(5 * time.Minute)
		case 402, 429:
			l.busyUntil = m.now().Add(30 * time.Second)
		}
	}
	return nil
}

func (m *Module) pollLoop(ctx context.Context) {
	defer close(m.done)
	m.pollOnce(ctx, true)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx, false)
		}
	}
}

type pollTarget struct{ id, token string }

func (m *Module) pollOnce(ctx context.Context, force bool) {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	m.mu.Lock()
	if !m.activeLocked() {
		m.mu.Unlock()
		return
	}
	now := m.now().UTC()
	generation := m.generation
	client := m.client
	var targets []pollTarget
	for id, a := range m.accounts {
		if a.token == "" || !force && !a.pollAt.IsZero() && now.Sub(a.pollAt) < m.cfg.Refresh {
			continue
		}
		a.pollAt = now
		targets = append(targets, pollTarget{id, a.token})
	}
	m.mu.Unlock()
	for _, target := range targets {
		used, err := fetchAnalytics(ctx, client, target.id, target.token, now)
		m.mu.Lock()
		if !m.closed && m.generation == generation {
			a := m.accounts[target.id]
			if err != nil {
				a.pollError = err.Error()
			} else {
				// A poll started before midnight does not become a fresh next-day zero.
				a.remote = used
				a.readingDay = utcDay(now).Format("2006-01-02")
				a.observed = m.now().UTC()
				a.pollError = ""
				m.budgetLocked(target.id, m.now())
				m.saveLocked()
			}
		}
		m.mu.Unlock()
	}
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
