// Package opencodego implements quota-aware scheduling for OpenCode Go
// credentials from the proxy-observable behavior captured in the first-party
// first-party observed-behavior fixture corpus.
package opencodego

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const (
	providerID             = "opencode-go"
	defaultThreshold       = 97
	defaultMicroRetry      = 3
	defaultFailureCooldown = 10 * time.Minute
	maxFailureBody         = 16 << 10
	maxResetFuture         = 35 * 24 * time.Hour
	minPollInterval        = 30 * time.Second
)

type windowKind string

const (
	windowFiveHour windowKind = "five_hour"
	windowWeekly   windowKind = "weekly"
	windowMonthly  windowKind = "monthly"
)

type moduleConfig struct {
	Enabled          bool
	ThresholdPercent int
	DashboardAPIKey  string
	Accounts         []accountConfig
}

type accountConfig struct {
	Name            string
	AuthIDs         []string
	DashboardAPIKey string
	Disabled        bool
}

type rawModuleConfig struct {
	Enabled          *bool              `json:"enabled"`
	ThresholdPercent *int               `json:"threshold-percent"`
	DashboardAPIKey  string             `json:"dashboard-api-key"`
	Accounts         []rawAccountConfig `json:"accounts"`
}

type rawAccountConfig struct {
	Name            string   `json:"name"`
	AuthIDs         []string `json:"auth-ids"`
	ConnectionID    string   `json:"connection-id"`
	DashboardAPIKey string   `json:"dashboard-api-key"`
	Disabled        bool     `json:"disabled"`
}

type accountState struct {
	Name            string
	AuthIDs         []string
	Disabled        bool
	DashboardAPIKey string
	Windows         map[windowKind]windowState
	cursor          uint64
	lastPollAt      time.Time
}

type windowState struct {
	Utilization *float64
	Exhausted   bool
	ResetAt     time.Time
	CooldownAt  time.Time
	Source      string

	// Known distinguishes "no signal has ever arrived" from a confirmed
	// healthy 0%-utilization reading -- the zero value of this struct must
	// never be mistaken for a validated observation.
	Known         bool
	Authoritative bool
	ObservedAt    time.Time
}

type moduleState struct {
	Threshold int
	Accounts  map[string]*accountState
	ByAuthID  map[string]string
}

type Module struct {
	mu         sync.Mutex
	state      *moduleState
	lastErr    string
	closed     bool
	clock      providermodule.Clock
	httpClient providermodule.HTTPDoer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
func (realClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var (
	_ providermodule.Module          = (*Module)(nil)
	_ providermodule.UsageAttributor = (*Module)(nil)
)

func NewModule() *Module { return &Module{} }

func (m *Module) ID() string { return providerID }

func (m *Module) Recognize(candidate pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	state := m.state
	inactive := state == nil || len(state.Accounts) == 0
	_, managed := stateAccountByAuthID(state, candidate.ID)
	m.mu.Unlock()
	if inactive {
		// A disabled module (enabled: false) or one with no configured
		// accounts manages nothing. Claiming OpenCode Go candidates here
		// would make Pick fail every OpenCode Go request as "unmanaged", so
		// leave them to the host's own scheduler instead.
		return false
	}
	if managed {
		return candidateClaimsOpenCodeGo(candidate) || !candidateClaimsOtherProvider(candidate)
	}
	return candidateClaimsOpenCodeGo(candidate)
}

func (m *Module) Reconfigure(_ context.Context, host providermodule.HostConfig, providerConfig json.RawMessage) error {
	cfg, err := parseConfig(providerConfig)
	if err != nil {
		m.recordError(err)
		return err
	}
	staged, err := buildState(cfg)
	if err != nil {
		m.recordError(err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("opencode_go_closed: module is closed")
	}
	carryForwardState(staged, m.state)
	m.state = staged
	m.lastErr = ""
	m.clock = host.Clock
	m.httpClient = host.HTTPClient
	return nil
}

func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return nil
	}
	ids := make([]string, 0, len(m.state.ByAuthID))
	for id := range m.state.ByAuthID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Module) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	now := requestTime(req)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state == nil {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_not_configured", "OpenCode Go scheduler is not configured")
	}

	recognized := false
	healthyAccounts := make(map[string]*accountState)
	for _, candidate := range req.Candidates {
		name, managed := stateAccountByAuthID(m.state, candidate.ID)
		claims := candidateClaimsOpenCodeGo(candidate)
		if claims {
			recognized = true
			if !managed {
				return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_unmanaged_candidate", "recognized OpenCode Go credential is absent from the validated account snapshot")
			}
		}
		if !managed {
			continue
		}
		if candidateClaimsOtherProvider(candidate) {
			continue
		}
		recognized = true
		account := m.state.Accounts[name]
		refreshWindows(account, now)
		if accountHealthy(account, m.state.Threshold, now) {
			healthyAccounts[name] = account
		}
	}
	if !recognized {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if len(healthyAccounts) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_no_capacity", "no healthy managed OpenCode Go capacity remains")
	}

	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidateClaimsOtherProvider(candidate) {
			continue
		}
		name, managed := stateAccountByAuthID(m.state, candidate.ID)
		if managed && healthyAccounts[name] != nil {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_no_capacity", "no healthy managed OpenCode Go capacity remains")
	}

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	accountName, _ := stateAccountByAuthID(m.state, candidates[0].ID)
	account := m.state.Accounts[accountName]
	selected := candidates[int(account.cursor%uint64(len(candidates)))].ID
	account.cursor++
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected}, nil
}

// UsageAccount names the OpenCode Go account a usage record belongs to, for
// the coordinator's usage ledger.
func (m *Module) UsageAccount(record pluginapi.UsageRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, _ := stateAccountByAuthID(m.state, record.AuthID)
	return name
}

func (m *Module) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state == nil {
		return nil
	}
	name, managed := stateAccountByAuthID(m.state, record.AuthID)
	if !managed || !record.Failed {
		return nil
	}
	account := m.state.Accounts[name]
	now := record.RequestedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if isMicrothrottle(record) {
		return nil
	}
	window, ok := quotaWindow(record.Failure.Body)
	if !ok || record.Failure.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	resetAt, resetKnown := retryAfterReset(record.ResponseHeaders, now)
	if !resetKnown {
		resetAt, resetKnown = bodyReset(record.Failure.Body, now)
	}
	current := account.Windows[window]
	var incomingResetAt time.Time
	if resetKnown {
		incomingResetAt = resetAt
	}
	cooldownAt := now.Add(defaultFailureCooldown)
	if resetKnown {
		cooldownAt = resetAt
	}
	incoming := windowState{
		Utilization: float64Pointer(1),
		Exhausted:   true,
		ResetAt:     incomingResetAt,
		CooldownAt:  cooldownAt,
		Source:      "proxy-observed " + string(window) + " quota response",
		Known:       true,
		ObservedAt:  now,
	}
	merged := mergeWindowSignal(current, incoming)
	if cooldownAt.After(merged.CooldownAt) {
		merged.CooldownAt = cooldownAt
	}
	account.Windows[window] = merged
	return nil
}

func (m *Module) Status(ctx context.Context) (json.RawMessage, error) {
	m.pollUsageOnce(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	type statusWindow struct {
		Known         bool       `json:"known"`
		Utilization   *float64   `json:"utilization,omitempty"`
		Exhausted     bool       `json:"exhausted"`
		ResetAt       *time.Time `json:"resets_at,omitempty"`
		Source        string     `json:"source,omitempty"`
		Authoritative bool       `json:"authoritative,omitempty"`
	}
	type statusAccount struct {
		Name     string                      `json:"name"`
		Disabled bool                        `json:"disabled,omitempty"`
		Windows  map[windowKind]statusWindow `json:"windows"`
	}
	credentialBound := false
	if m.state != nil {
		for _, account := range m.state.Accounts {
			if account.DashboardAPIKey != "" {
				credentialBound = true
				break
			}
		}
	}
	gaps := []string{"five-hour and weekly enforcement are generalized from the observed monthly 429 shape"}
	if !credentialBound {
		gaps = append(gaps, "no dashboard credential is configured -- five_hour/weekly/monthly reset times are unknown until dashboard-api-key is set")
	}
	status := struct {
		Provider        string          `json:"provider"`
		Status          string          `json:"status"`
		ValidationError string          `json:"validation_error,omitempty"`
		CredentialBound bool            `json:"credential_bound"`
		Accounts        []statusAccount `json:"accounts,omitempty"`
		ObservationGaps []string        `json:"observation_gaps"`
	}{
		Provider:        providerID,
		Status:          "registered",
		ValidationError: m.lastErr,
		CredentialBound: credentialBound,
		ObservationGaps: gaps,
	}
	if m.lastErr != "" {
		status.Status = "reconfigure_rejected"
	}
	if m.state != nil {
		now := m.runtimeClock().Now().UTC()
		names := make([]string, 0, len(m.state.Accounts))
		for name := range m.state.Accounts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			account := m.state.Accounts[name]
			refreshWindows(account, now)
			item := statusAccount{Name: account.Name, Disabled: account.Disabled, Windows: make(map[windowKind]statusWindow, 3)}
			for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
				window := account.Windows[kind]
				if !window.Known {
					item.Windows[kind] = statusWindow{Known: false, Source: "unknown"}
					continue
				}
				var resetAt *time.Time
				if !window.ResetAt.IsZero() {
					value := window.ResetAt.UTC()
					resetAt = &value
				}
				item.Windows[kind] = statusWindow{
					Known:         true,
					Utilization:   copyFloat64Pointer(window.Utilization),
					Exhausted:     window.Exhausted,
					ResetAt:       resetAt,
					Source:        window.Source,
					Authoritative: window.Authoritative,
				}
			}
			status.Accounts = append(status.Accounts, item)
		}
	}
	return json.Marshal(status)
}

func (m *Module) Close(context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.state = nil
	m.mu.Unlock()
	return nil
}

func (m *Module) recordError(err error) {
	m.mu.Lock()
	m.lastErr = bounded(err.Error(), 256)
	m.mu.Unlock()
}

func (m *Module) runtimeClock() providermodule.Clock {
	if m.clock != nil {
		return m.clock
	}
	return realClock{}
}

func (m *Module) runtimeHTTPClient() httpDoer {
	if m.httpClient != nil {
		return m.httpClient
	}
	return newUsageHTTPClient(0)
}

// pollUsageOnce best-effort refreshes every window of every account that has
// its own dashboard-api-key from that account's own usage endpoint response.
// Each account is throttled independently by minPollInterval. A poll failure
// for one account never blocks or corrupts another account's result, and
// never returns an error to the caller: a failure simply leaves that
// account's existing window state (429-derived or previously polled) in
// place rather than failing Status().
func (m *Module) pollUsageOnce(ctx context.Context) {
	m.mu.Lock()
	if m.closed || m.state == nil {
		m.mu.Unlock()
		return
	}
	now := m.runtimeClock().Now().UTC()
	client := m.runtimeHTTPClient()
	type pollTarget struct {
		name   string
		apiKey string
	}
	var targets []pollTarget
	for name, account := range m.state.Accounts {
		if account.DashboardAPIKey == "" {
			continue
		}
		if !account.lastPollAt.IsZero() && now.Sub(account.lastPollAt) < minPollInterval {
			continue
		}
		account.lastPollAt = now
		targets = append(targets, pollTarget{name: name, apiKey: account.DashboardAPIKey})
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return
	}

	type pollResult struct {
		name     string
		snapshot usageSnapshot
		err      error
	}
	results := make(chan pollResult, len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target pollTarget) {
			defer wg.Done()
			snapshot, err := fetchUsage(ctx, client, usageEndpoint, target.apiKey, now)
			results <- pollResult{name: target.name, snapshot: snapshot, err: err}
		}(target)
	}
	wg.Wait()
	close(results)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state == nil {
		return
	}
	for result := range results {
		if result.err != nil {
			continue
		}
		account := m.state.Accounts[result.name]
		if account == nil {
			continue
		}
		for kind, window := range snapshotWindows(result.snapshot) {
			current := account.Windows[kind]
			incoming := windowState{
				Utilization:   float64Pointer(window.Utilization),
				Exhausted:     window.LimitReached,
				ResetAt:       window.ResetAt,
				Source:        "dashboard usage poll",
				Known:         true,
				Authoritative: true,
				ObservedAt:    result.snapshot.ObservedAt,
			}
			account.Windows[kind] = mergeWindowSignal(current, incoming)
		}
	}
}

func snapshotWindows(snapshot usageSnapshot) map[windowKind]usageWindow {
	return map[windowKind]usageWindow{
		windowFiveHour: snapshot.FiveHour,
		windowWeekly:   snapshot.Weekly,
		windowMonthly:  snapshot.Monthly,
	}
}

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := moduleConfig{Enabled: true, ThresholdPercent: defaultThreshold}
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return cfg, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var input rawModuleConfig
	if err := decoder.Decode(&input); err != nil {
		return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: decode provider config: %w", err)
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.ThresholdPercent != nil {
		cfg.ThresholdPercent = *input.ThresholdPercent
	}
	if cfg.ThresholdPercent < 1 || cfg.ThresholdPercent > 100 {
		return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: threshold-percent must be between 1 and 100")
	}
	// An empty dashboard-api-key is valid: it means no credential is bound
	// and the module must fail closed to "unknown" rather than error.
	cfg.DashboardAPIKey = strings.TrimSpace(input.DashboardAPIKey)
	for i, rawAccount := range input.Accounts {
		name := strings.TrimSpace(rawAccount.Name)
		if name == "" {
			return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: accounts[%d].name is required", i)
		}
		ids := append([]string(nil), rawAccount.AuthIDs...)
		if connectionID := strings.TrimSpace(rawAccount.ConnectionID); connectionID != "" {
			ids = append(ids, connectionID)
		}
		ids = normalizeIDs(ids)
		if len(ids) == 0 {
			return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: accounts[%d] requires auth-ids or connection-id", i)
		}
		// Falls back to the module-level key when an account does not set
		// its own -- accounts sharing one dashboard key still poll correctly
		// as long as that key actually reports that account's own usage.
		apiKey := strings.TrimSpace(rawAccount.DashboardAPIKey)
		if apiKey == "" {
			apiKey = cfg.DashboardAPIKey
		}
		cfg.Accounts = append(cfg.Accounts, accountConfig{Name: name, AuthIDs: ids, DashboardAPIKey: apiKey, Disabled: rawAccount.Disabled})
	}
	return cfg, nil
}

func buildState(cfg moduleConfig) (*moduleState, error) {
	state := &moduleState{Threshold: cfg.ThresholdPercent, Accounts: make(map[string]*accountState), ByAuthID: make(map[string]string)}
	if !cfg.Enabled {
		return state, nil
	}
	seenNames := make(map[string]struct{}, len(cfg.Accounts))
	for _, item := range cfg.Accounts {
		key := strings.ToLower(item.Name)
		if _, exists := seenNames[key]; exists {
			return nil, fmt.Errorf("opencode_go_invalid_config: duplicate account name %q", item.Name)
		}
		seenNames[key] = struct{}{}
		account := &accountState{Name: item.Name, AuthIDs: append([]string(nil), item.AuthIDs...), DashboardAPIKey: item.DashboardAPIKey, Disabled: item.Disabled, Windows: map[windowKind]windowState{windowFiveHour: {}, windowWeekly: {}, windowMonthly: {}}}
		for _, authID := range item.AuthIDs {
			if owner := state.ByAuthID[authID]; owner != "" {
				return nil, fmt.Errorf("opencode_go_invalid_config: auth id %q belongs to both %q and %q", authID, owner, item.Name)
			}
			state.ByAuthID[authID] = item.Name
		}
		state.Accounts[item.Name] = account
	}
	return state, nil
}

func carryForwardState(staged, previous *moduleState) {
	if staged == nil || previous == nil {
		return
	}
	for name, account := range staged.Accounts {
		old := previous.Accounts[name]
		if old == nil || !sameIDs(account.AuthIDs, old.AuthIDs) {
			continue
		}
		account.cursor = old.cursor
		account.lastPollAt = old.lastPollAt
		for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
			account.Windows[kind] = old.Windows[kind]
		}
	}
}

func sameIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func normalizeIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stateAccountByAuthID(state *moduleState, authID string) (string, bool) {
	if state == nil {
		return "", false
	}
	name := state.ByAuthID[strings.TrimSpace(authID)]
	return name, name != ""
}

func candidateClaimsOpenCodeGo(candidate pluginapi.SchedulerAuthCandidate) bool {
	for _, value := range []string{candidate.Provider, candidate.Attributes["provider_key"], candidate.Attributes["compat_name"], candidate.Attributes["provider"]} {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == providerID || normalized == "openai-compatible-"+providerID {
			return true
		}
	}
	return false
}

func candidateClaimsOtherProvider(candidate pluginapi.SchedulerAuthCandidate) bool {
	for _, value := range []string{candidate.Provider, candidate.Attributes["provider_key"], candidate.Attributes["compat_name"], candidate.Attributes["provider"]} {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if normalized != providerID && normalized != "openai-compatible-"+providerID {
			return true
		}
	}
	return false
}

func requestTime(req pluginapi.SchedulerPickRequest) time.Time {
	if req.Options.Metadata != nil {
		switch value := req.Options.Metadata["now"].(type) {
		case time.Time:
			return value.UTC()
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Now().UTC()
}

func refreshWindows(account *accountState, now time.Time) {
	for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
		window := account.Windows[kind]
		if !window.CooldownAt.IsZero() && !window.CooldownAt.After(now) {
			window = windowState{}
			account.Windows[kind] = window
		}
	}
}

// mergeWindowSignal decides whether incoming should replace current.
// Precedence:
//  1. current windowState.Known == false means there is nothing to
//     reconcile against -- incoming always wins.
//  2. A newer authoritative (poll) observation is direct account ground
//     truth and always wins, whatever it reports.
//  3. An inferred (429) signal only overrides current state if current is
//     not itself a still-live, later-resetting exhaustion -- an older or
//     weaker 429 can never clobber a fresher, more urgent block from
//     either source.
//  4. A stale authoritative poll (older ObservedAt than current) never
//     overwrites fresher state of either kind.
func mergeWindowSignal(current, incoming windowState) windowState {
	if !current.Known {
		return incoming
	}
	if incoming.Authoritative && !incoming.ObservedAt.Before(current.ObservedAt) {
		return incoming
	}
	if !incoming.Authoritative {
		if current.Exhausted && current.ResetAt.After(incoming.ObservedAt) && current.ResetAt.After(incoming.ResetAt) {
			return current
		}
		return incoming
	}
	return current
}

func accountHealthy(account *accountState, threshold int, now time.Time) bool {
	if account.Disabled {
		return false
	}
	limit := float64(threshold) / 100
	for _, window := range account.Windows {
		if window.Exhausted && (window.CooldownAt.IsZero() || window.CooldownAt.After(now)) {
			return false
		}
		if window.Utilization != nil && *window.Utilization >= limit {
			return false
		}
	}
	return true
}

func float64Pointer(value float64) *float64 { return &value }

func copyFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return float64Pointer(*value)
}

func quotaWindow(body string) (windowKind, bool) {
	if len(body) == 0 || len(body) > maxFailureBody {
		return "", false
	}
	text := strings.ToLower(body)
	switch {
	case strings.Contains(text, "five-hour usage limit reached"), strings.Contains(text, "5-hour usage limit reached"), strings.Contains(text, "five hour usage limit reached"):
		return windowFiveHour, true
	case strings.Contains(text, "weekly usage limit reached"):
		return windowWeekly, true
	case strings.Contains(text, "monthly usage limit reached"):
		return windowMonthly, true
	default:
		return "", false
	}
}

func isMicrothrottle(record pluginapi.UsageRecord) bool {
	if record.Failure.StatusCode != http.StatusForbidden || len(record.Failure.Body) > maxFailureBody {
		return false
	}
	text := strings.ToLower(record.Failure.Body)
	if !strings.Contains(text, "<!doctype html>") || !strings.Contains(text, "reset after ") {
		return false
	}
	seconds, ok := parseResetAfterSeconds(text)
	return ok && seconds >= 1 && seconds <= defaultMicroRetry
}

func retryAfterReset(headers http.Header, now time.Time) (time.Time, bool) {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), "Retry-After") {
			continue
		}
		for _, raw := range values {
			value := strings.TrimSpace(raw)
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err == nil && seconds > 0 && seconds <= int64(maxResetFuture/time.Second) {
				return now.Add(time.Duration(seconds) * time.Second), true
			}
			if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) && parsed.Before(now.Add(maxResetFuture)) {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func bodyReset(body string, now time.Time) (time.Time, bool) {
	seconds, ok := parseResetAfterSeconds(strings.ToLower(body))
	if !ok || seconds <= 0 || seconds > int(maxResetFuture/time.Second) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func parseResetAfterSeconds(text string) (int, bool) {
	marker := "reset after "
	index := strings.LastIndex(text, marker)
	if index < 0 {
		return 0, false
	}
	text = text[index+len(marker):]
	if end := strings.IndexByte(text, ')'); end >= 0 {
		text = text[:end]
	}
	text = strings.TrimSpace(text)
	var total time.Duration
	var number int
	digits := false
	seen := false
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			number = number*10 + int(r-'0')
			digits = true
		case r == 'd' || r == 'h' || r == 'm' || r == 's':
			if !digits {
				return 0, false
			}
			seen = true
			switch r {
			case 'd':
				total += time.Duration(number) * 24 * time.Hour
			case 'h':
				total += time.Duration(number) * time.Hour
			case 'm':
				total += time.Duration(number) * time.Minute
			case 's':
				total += time.Duration(number) * time.Second
			}
			number = 0
			digits = false
		case r == ' ':
			if digits {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	return int(total / time.Second), seen && !digits && total > 0
}

type schedulerError struct {
	code    string
	message string
}

func newSchedulerError(code, message string) error {
	return &schedulerError{code: code, message: message}
}
func (e *schedulerError) Error() string { return e.code + ": " + e.message }
func (e *schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.code, Message: e.message, Retryable: false}
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return value[:limit-17] + "#" + hex.EncodeToString(digest[:8])
}
