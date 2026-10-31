// Package synthetic implements quota-aware scheduling for synthetic.new
// subscription credentials, including the plan's free "infinite mode".
//
// Synthetic plans have one limit: a weekly credit budget that regenerates 2%
// of the weekly limit every 202 minutes (no five-hour window, no lockout).
// Usage beyond it is billed at usage-based prices when the account allows
// extra usage. Infinite mode (Big plan) is the model alias
// "infinite:small:vision": it never counts against the weekly budget, runs
// much slower, and requires streaming.
//
// The host config carries two OpenAI-compatible providers that share one API
// key: a paid provider (default name "synthetic") whose models spend weekly
// credits, and an infinite provider (default "synthetic-infinite") at a lower
// priority. When one alias is served by both, this module keeps new requests
// on the paid credential while the plan is under threshold-percent used, and
// moves them to infinite mode once it is at or above the threshold, exhausted
// (429/402), or at its local concurrency cap -- before the host would spill
// into usage-based billing or fail the request.
package synthetic

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
	providerID          = "synthetic"
	compatProviderLabel = "openai-compatible-"
	maxTPSSamples       = 200
	maxFailureBody      = 16 << 10
	minGenerationSample = 100 * time.Millisecond
)

type laneKind string

const (
	kindPaid     laneKind = "paid"
	kindInfinite laneKind = "infinite"
)

// lane is one host credential: one api-key entry of one paid or infinite
// openai-compatibility provider. Name is public ("synthetic#1", see
// labels.go); AuthID and Account are credential-derived and stay on the
// authenticated status route.
type lane struct {
	Name     string
	Provider string
	Kind     laneKind
	AuthID   string
	Account  string
	learned  bool
	ordinal  int

	inFlight  []time.Time
	busyUntil time.Time
	busyWhy   string
	stats     laneStats

	peakInFlight int
	peakAt       time.Time
}

type laneStats struct {
	Requests     int64
	Failures     int64
	OutputTokens int64
	genSeconds   float64
	tpsSamples   []float64
	ttftTotal    time.Duration
	ttftCount    int64
	LastStatus   int
	LastAt       time.Time
}

// account is one synthetic.new API key (shared by its paid and infinite
// lanes). The key never leaves this struct: status shows only a fingerprint.
type account struct {
	Fingerprint      string
	key              string
	QuotaURL         string
	Placeholder      bool
	Weekly           windowState
	NextRegenPercent *float64
	exhaustedUntil   time.Time
	exhaustedWhy     string
	lastPollAt       time.Time
	lastPollErr      string
	polls            int64
	pollFailures     int64
}

type windowState struct {
	Known         bool
	Utilization   *float64
	ResetAt       time.Time
	Source        string
	Authoritative bool
	ObservedAt    time.Time
}

type routingStats struct {
	PaidPicks          int64
	InfinitePicks      int64
	Fallbacks          int64
	Rejections         int64
	InfiniteNonStream  int64
	LastFallbackReason string
	LastFallbackAt     time.Time
}

// Module is the synthetic.new provider module.
type Module struct {
	mu          sync.Mutex
	cfg         moduleConfig
	lanes       map[string]*lane
	accounts    map[string]*account
	paidSet     map[string]struct{}
	infiniteSet map[string]struct{}
	cursor      uint64
	routing     routingStats
	lastErr     string
	configured  bool
	closed      bool

	// labelsMu serializes lane naming and its persistence across
	// Reconfigure calls; it is never taken while holding mu.
	labelsMu     sync.Mutex
	labels       map[string]laneLabel
	labelsDir    string
	labelsLoaded bool
	labelsSaved  bool
	// labelsErr (under mu) is the last lane-labels failure, for status.
	labelsErr  string
	learnedSeq map[string]int

	now        func() time.Time
	httpClient httpDoer

	pollerStarted bool
	pollStop      chan struct{}
	pollDone      chan struct{}
	pollWake      chan struct{}
	noPoller      bool
}

var (
	_ providermodule.Module          = (*Module)(nil)
	_ providermodule.UsageRecognizer = (*Module)(nil)
	_ providermodule.UsageAttributor = (*Module)(nil)
	_ providermodule.ConfigValidator = (*Module)(nil)
)

// NewModule constructs the synthetic module with a real clock and HTTP client.
func NewModule() *Module {
	return &Module{now: func() time.Time { return time.Now().UTC() }}
}

func (m *Module) ID() string { return providerID }

func (m *Module) clock() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func (m *Module) client() httpDoer {
	if m.httpClient != nil {
		return m.httpClient
	}
	return newQuotaHTTPClient()
}

// Recognize claims candidates whose host provider is one of the configured
// paid or infinite provider names. An inactive module claims nothing, so the
// host's own scheduler keeps serving those credentials unchanged.
func (m *Module) Recognize(candidate pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return false
	}
	_, ok := m.kindOfLocked(candidate)
	return ok
}

func (m *Module) activeLocked() bool {
	return m.configured && !m.closed && m.cfg.Enabled && len(m.lanes) > 0
}

func (m *Module) kindOfLocked(candidate pluginapi.SchedulerAuthCandidate) (laneKind, bool) {
	if existing := m.lanes[strings.TrimSpace(candidate.ID)]; existing != nil {
		return existing.Kind, true
	}
	for _, value := range []string{candidate.Attributes["compat_name"], candidate.Attributes["provider_key"], candidate.Provider} {
		if kind, ok := m.kindOfProviderLocked(value); ok {
			return kind, true
		}
	}
	return "", false
}

func (m *Module) kindOfProviderLocked(raw string) (laneKind, bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimPrefix(name, compatProviderLabel)
	if name == "" {
		return "", false
	}
	if _, ok := m.paidSet[name]; ok {
		return kindPaid, true
	}
	if _, ok := m.infiniteSet[name]; ok {
		return kindInfinite, true
	}
	return "", false
}

// Reconfigure rebuilds lanes from the host's openai-compatibility entries.
// It never fails because synthetic credentials are absent: an operator
// without a synthetic.new plan simply gets an inactive module.
func (m *Module) Reconfigure(_ context.Context, host providermodule.HostConfig, providerConfig json.RawMessage) error {
	cfg, err := parseConfig(providerConfig)
	if err != nil {
		m.mu.Lock()
		m.lastErr = bounded(err.Error(), 256)
		m.mu.Unlock()
		return err
	}
	paidSet := toSet(cfg.PaidProviders)
	infiniteSet := toSet(cfg.InfiniteProviders)
	lanes, order, accounts := discoverLanes(cfg, host.OpenAICompatibility, paidSet, infiniteSet)

	m.labelsMu.Lock()
	defer m.labelsMu.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return fmt.Errorf("synthetic_closed: module is closed")
	}
	labelsErr := m.nameLanesLocked(host.AuthDir, order, lanes)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("synthetic_closed: module is closed")
	}
	m.labelsErr = labelsErr
	for id, next := range lanes {
		if previous := m.lanes[id]; previous != nil && !previous.learned && previous.Kind == next.Kind {
			next.inFlight = previous.inFlight
			next.busyUntil = previous.busyUntil
			next.busyWhy = previous.busyWhy
			next.stats = previous.stats
			next.peakInFlight = previous.peakInFlight
			next.peakAt = previous.peakAt
		}
	}
	for fingerprint, next := range accounts {
		if previous := m.accounts[fingerprint]; previous != nil {
			next.Weekly = previous.Weekly
			next.NextRegenPercent = previous.NextRegenPercent
			next.exhaustedUntil = previous.exhaustedUntil
			next.exhaustedWhy = previous.exhaustedWhy
			next.lastPollAt = previous.lastPollAt
			next.lastPollErr = previous.lastPollErr
			next.polls = previous.polls
			next.pollFailures = previous.pollFailures
		}
	}
	m.cfg = cfg
	m.paidSet = paidSet
	m.infiniteSet = infiniteSet
	m.lanes = lanes
	m.accounts = accounts
	m.configured = true
	m.lastErr = ""
	startPoller := cfg.Enabled && cfg.QuotaRefresh > 0 && !m.noPoller && !m.pollerStarted && len(accounts) > 0
	if startPoller {
		m.pollerStarted = true
		m.pollStop = make(chan struct{})
		m.pollDone = make(chan struct{})
		m.pollWake = make(chan struct{}, 1)
	}
	wake := m.pollWake
	m.mu.Unlock()

	if startPoller {
		go m.pollLoop()
	} else if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// ValidateConfig implements providermodule.ConfigValidator. Reconfigure
// only rejects a config it cannot parse, so that is all this checks, and
// the rejection is recorded exactly as Reconfigure records it.
func (m *Module) ValidateConfig(_ providermodule.HostConfig, providerConfig json.RawMessage) error {
	_, err := parseConfig(providerConfig)
	if err != nil {
		m.mu.Lock()
		m.lastErr = bounded(err.Error(), 256)
		m.mu.Unlock()
	}
	return err
}

// discoverLanes mirrors the host's credential synthesis for
// openai-compatibility entries: disabled providers are skipped, and each
// api-key entry becomes one auth whose ID is
// "openai-compatibility:<lower name>:" + sha256(kind \0 key \0 base \0 proxy)[:12]
// with a -N suffix for duplicates. The module also recognizes candidates by
// provider name, so a host whose ID derivation differs still schedules
// correctly; only usage attribution falls back to provider matching. order
// lists the auth IDs in config order; lanes are named by assignLaneNames.
func discoverLanes(cfg moduleConfig, compats []providermodule.OpenAICompatibility, paidSet, infiniteSet map[string]struct{}) (map[string]*lane, []string, map[string]*account) {
	lanes := make(map[string]*lane)
	var order []string
	accounts := make(map[string]*account)
	idGen := newStableIDGenerator()
	for _, compat := range compats {
		if compat.Disabled {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(compat.Name))
		var kind laneKind
		if _, ok := paidSet[name]; ok {
			kind = kindPaid
		} else if _, ok := infiniteSet[name]; ok {
			kind = kindInfinite
		} else {
			continue
		}
		base := strings.TrimSpace(compat.BaseURL)
		idKind := "openai-compatibility:" + name
		entries := compat.APIKeyEntries
		if len(entries) == 0 {
			entries = []providermodule.OpenAICompatibilityAPIKey{{}}
		}
		for _, entry := range entries {
			key := strings.TrimSpace(entry.APIKey)
			var id string
			if len(compat.APIKeyEntries) == 0 {
				id = idGen.next(idKind, base)
			} else {
				id = idGen.next(idKind, key, base, strings.TrimSpace(entry.ProxyURL))
			}
			item := &lane{
				Provider: name,
				Kind:     kind,
				AuthID:   id,
			}
			if key != "" {
				fingerprint := keyFingerprint(key)
				item.Account = fingerprint
				existing := accounts[fingerprint]
				if existing == nil {
					existing = &account{Fingerprint: fingerprint, key: key, Placeholder: isPlaceholderKey(key)}
					accounts[fingerprint] = existing
				}
				if existing.QuotaURL == "" {
					existing.QuotaURL = quotaURLFor(cfg, base)
				}
			}
			lanes[id] = item
			order = append(order, id)
		}
	}
	return lanes, order, accounts
}

func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return nil
	}
	ids := make([]string, 0, len(m.lanes))
	for id, item := range m.lanes {
		if item.learned {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Pick chooses among the recognized candidates:
//  1. a healthy paid credential (under threshold, not exhausted, under its
//     concurrency cap), round-robin;
//  2. otherwise an available infinite-mode credential (fallback);
//  3. otherwise a 429-coded rejection, so the request never spills into
//     usage-based billing and never trips the host's own cooldowns.
//
// Candidates belonging to another provider make the pick ambiguous, so the
// module declines (Handled:false) and the host's scheduler decides.
func (m *Module) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	now := requestTime(req, m.clock())
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	var paid, infinite []*lane
	for _, candidate := range req.Candidates {
		kind, ok := m.kindOfLocked(candidate)
		if !ok {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		item := m.laneForCandidateLocked(candidate, kind)
		if kind == kindPaid {
			paid = append(paid, item)
		} else {
			infinite = append(infinite, item)
		}
	}
	if len(paid) == 0 && len(infinite) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	sortLanes(paid)
	sortLanes(infinite)

	healthyPaid := make([]*lane, 0, len(paid))
	paidReason := ""
	for _, item := range paid {
		if ok, why := m.paidAvailableLocked(item, now); ok {
			healthyPaid = append(healthyPaid, item)
		} else if paidReason == "" {
			paidReason = why
		}
	}
	if len(healthyPaid) > 0 {
		chosen := healthyPaid[int(m.cursor%uint64(len(healthyPaid)))]
		m.cursor++
		m.admitLocked(chosen, now)
		m.routing.PaidPicks++
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: chosen.AuthID}, nil
	}

	infiniteReason := ""
	for _, item := range infinite {
		ok, why := m.infiniteAvailableLocked(item, now)
		if !ok {
			if infiniteReason == "" {
				infiniteReason = why
			}
			continue
		}
		m.admitLocked(item, now)
		m.routing.InfinitePicks++
		if !req.Stream {
			m.routing.InfiniteNonStream++
		}
		if len(paid) > 0 {
			m.routing.Fallbacks++
			m.routing.LastFallbackReason = paidReason
			m.routing.LastFallbackAt = now
		}
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: item.AuthID}, nil
	}

	m.routing.Rejections++
	if len(paid) > 0 {
		message := "synthetic.new paid plan unavailable (" + paidReason + ")"
		if len(infinite) > 0 {
			message += " and infinite mode unavailable (" + infiniteReason + ")"
		} else {
			message += "; this model has no infinite-mode fallback"
		}
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("synthetic_no_capacity", message)
	}
	return pluginapi.SchedulerPickResponse{}, newSchedulerError("synthetic_infinite_busy", "synthetic.new infinite mode unavailable ("+infiniteReason+")")
}

func (m *Module) laneForCandidateLocked(candidate pluginapi.SchedulerAuthCandidate, kind laneKind) *lane {
	id := strings.TrimSpace(candidate.ID)
	if item := m.lanes[id]; item != nil {
		return item
	}
	provider := ""
	for _, value := range []string{candidate.Attributes["compat_name"], candidate.Attributes["provider_key"], candidate.Provider} {
		if _, ok := m.kindOfProviderLocked(value); ok {
			provider = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), compatProviderLabel)
			break
		}
	}
	if m.learnedSeq == nil {
		m.learnedSeq = make(map[string]int)
	}
	m.learnedSeq[provider]++
	ordinal := m.learnedSeq[provider]
	item := &lane{Name: provider + "#learned-" + strconv.Itoa(ordinal), Provider: provider, Kind: kind, AuthID: id, learned: true, ordinal: ordinal}
	// A learned credential most likely belongs to the only account its
	// provider uses; bind it so quota still protects it.
	if only := m.soleAccountForProviderLocked(provider); only != "" {
		item.Account = only
	}
	m.lanes[id] = item
	return item
}

func (m *Module) soleAccountForProviderLocked(provider string) string {
	found := ""
	for _, item := range m.lanes {
		if item.learned || item.Provider != provider || item.Account == "" {
			continue
		}
		if found != "" && found != item.Account {
			return ""
		}
		found = item.Account
	}
	return found
}

// unboundPaidReason explains why a learned paid credential is refused: its
// spend cannot be checked against any account's weekly credits.
const unboundPaidReason = "credential not bound to a known synthetic.new account (re-keyed or added after the last reconfigure), so its weekly credits cannot be checked"

func (m *Module) paidAvailableLocked(item *lane, now time.Time) (bool, string) {
	if busy, why := laneBusy(item, now); busy {
		return false, why
	}
	if item.learned && m.accounts[item.Account] == nil {
		// Fail closed: a learned paid credential that could not be bound to
		// exactly one known account (two or more accounts, or none) has no
		// quota to check, and admitting it could spill into usage-based
		// billing past the threshold. Infinite mode, or a 429-coded
		// rejection, takes the request until the next Reconfigure binds it.
		return false, unboundPaidReason
	}
	if acct := m.accounts[item.Account]; acct != nil {
		if acct.exhaustedUntil.After(now) {
			return false, "exhausted until " + acct.exhaustedUntil.Format(time.RFC3339) + ": " + acct.exhaustedWhy
		}
		if used, known := effectiveUtilization(acct, now); known && used*100 >= float64(m.cfg.ThresholdPercent) {
			return false, fmt.Sprintf("weekly credits %.1f%% used, at or above the %d%% threshold", used*100, m.cfg.ThresholdPercent)
		}
	}
	if limit := m.cfg.MaxConcurrent; limit > 0 && m.inFlightLocked(item, now) >= limit {
		return false, fmt.Sprintf("paid concurrency cap %d reached", limit)
	}
	return true, ""
}

func (m *Module) infiniteAvailableLocked(item *lane, now time.Time) (bool, string) {
	if busy, why := laneBusy(item, now); busy {
		return false, why
	}
	if limit := m.cfg.InfiniteMaxConcurrent; limit > 0 && m.inFlightLocked(item, now) >= limit {
		return false, fmt.Sprintf("infinite concurrency cap %d reached", limit)
	}
	return true, ""
}

func laneBusy(item *lane, now time.Time) (bool, string) {
	if item.busyUntil.After(now) {
		return true, "busy until " + item.busyUntil.Format(time.RFC3339) + ": " + item.busyWhy
	}
	return false, ""
}

func (m *Module) admitLocked(item *lane, now time.Time) {
	m.pruneInFlightLocked(item, now)
	item.inFlight = append(item.inFlight, now)
	if len(item.inFlight) > item.peakInFlight {
		item.peakInFlight = len(item.inFlight)
		item.peakAt = now
	}
}

func (m *Module) inFlightLocked(item *lane, now time.Time) int {
	m.pruneInFlightLocked(item, now)
	return len(item.inFlight)
}

func (m *Module) pruneInFlightLocked(item *lane, now time.Time) {
	ttl := m.cfg.InFlightTTL
	if ttl <= 0 {
		ttl = defaultInFlightTTL
	}
	keep := item.inFlight[:0]
	for _, started := range item.inFlight {
		if now.Sub(started) < ttl {
			keep = append(keep, started)
		}
	}
	item.inFlight = keep
}

// RecognizeUsage claims usage records from the configured providers whose
// auth ID the coordinator registry does not know (e.g. a credential re-keyed
// after the last Reconfigure).
func (m *Module) RecognizeUsage(record pluginapi.UsageRecord) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return false
	}
	if m.lanes[strings.TrimSpace(record.AuthID)] != nil {
		return true
	}
	_, ok := m.kindOfProviderLocked(record.Provider)
	return ok
}

// UsageAccount names the lane a usage record belongs to (for example
// "synthetic#1" or "synthetic-infinite#2") for the coordinator's usage
// ledger. HandleUsage has already created a learned lane for an unknown auth
// ID of a configured provider, so every accepted record resolves.
func (m *Module) UsageAccount(record pluginapi.UsageRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item := m.lanes[strings.TrimSpace(record.AuthID)]; item != nil {
		return item.Name
	}
	return ""
}

// HandleUsage releases the in-flight slot, records throughput, and learns
// quota from the x-synthetic-quotas header and from 429/402 failures.
func (m *Module) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked() {
		return nil
	}
	id := strings.TrimSpace(record.AuthID)
	item := m.lanes[id]
	if item == nil {
		kind, ok := m.kindOfProviderLocked(record.Provider)
		if !ok || id == "" {
			return nil
		}
		item = m.laneForCandidateLocked(pluginapi.SchedulerAuthCandidate{ID: id, Provider: record.Provider}, kind)
	}
	now := record.RequestedAt.UTC()
	if now.IsZero() {
		now = m.clock()
	}
	if record.Latency > 0 {
		now = now.Add(record.Latency)
	}
	if len(item.inFlight) > 0 {
		item.inFlight = item.inFlight[1:]
	}
	recordStats(item, record, now)

	acct := m.accounts[item.Account]
	if acct != nil {
		// Infinite responses carry the same account's header, so both kinds
		// keep the paid quota reading fresh at no cost.
		if reading, ok := quotaFromHeaders(record.ResponseHeaders); ok {
			applyReading(acct, reading, "response header", false, now)
		}
	}
	if !record.Failed {
		return nil
	}
	status := record.Failure.StatusCode
	if status != http.StatusTooManyRequests && status != http.StatusPaymentRequired {
		return nil
	}
	if item.Kind == kindInfinite {
		item.busyUntil = now.Add(m.cfg.InfiniteCooldown)
		item.busyWhy = "infinite mode returned HTTP " + strconv.Itoa(status)
		return nil
	}
	if acct == nil {
		item.busyUntil = now.Add(m.cfg.FailureCooldown)
		item.busyWhy = "HTTP " + strconv.Itoa(status) + " on an unbound credential"
		return nil
	}
	reading, haveReading := quotaFromHeaders(record.ResponseHeaders)
	if status == http.StatusTooManyRequests && haveReading && reading.PercentRemaining != nil && *reading.PercentRemaining > 100-float64(m.cfg.ThresholdPercent) {
		// Credits remain, so this 429 is a rate or concurrency limit, not
		// the weekly budget: rest this credential briefly instead.
		item.busyUntil = now.Add(m.cfg.InfiniteCooldown)
		item.busyWhy = "HTTP 429 with credits remaining (rate or concurrency limit)"
		return nil
	}
	until := now.Add(m.cfg.FailureCooldown)
	if haveReading && reading.NextRegenAt.After(now) {
		until = reading.NextRegenAt
	} else if acct.Weekly.ResetAt.After(now) {
		until = acct.Weekly.ResetAt
	}
	if until.After(acct.exhaustedUntil) {
		acct.exhaustedUntil = until
	}
	acct.exhaustedWhy = "HTTP " + strconv.Itoa(status) + " " + summarizeBody(record.Failure.Body)
	return nil
}

// regenInterval is Synthetic's documented regeneration cadence: 2% of the
// weekly limit returns every 202 minutes.
const regenInterval = 202 * time.Minute

// effectiveUtilization projects the last weekly reading forward through the
// regeneration ticks that have passed since it was taken, so a stale reading
// never parks the paid credential longer than the plan itself would.
func effectiveUtilization(acct *account, now time.Time) (float64, bool) {
	if acct == nil || !acct.Weekly.Known || acct.Weekly.Utilization == nil {
		return 0, false
	}
	used := *acct.Weekly.Utilization
	if acct.NextRegenPercent != nil && *acct.NextRegenPercent > 0 && !acct.Weekly.ResetAt.IsZero() && !now.Before(acct.Weekly.ResetAt) {
		ticks := 1 + int64(now.Sub(acct.Weekly.ResetAt)/regenInterval)
		used -= float64(ticks) * *acct.NextRegenPercent / 100
		if used < 0 {
			used = 0
		}
	}
	return used, true
}

func recordStats(item *lane, record pluginapi.UsageRecord, now time.Time) {
	stats := &item.stats
	stats.Requests++
	stats.LastAt = now
	if record.Failed {
		stats.Failures++
		stats.LastStatus = record.Failure.StatusCode
		return
	}
	stats.LastStatus = http.StatusOK
	out := record.Detail.OutputTokens
	stats.OutputTokens += out
	if record.TTFT > 0 {
		stats.ttftTotal += record.TTFT
		stats.ttftCount++
	}
	generation := record.Latency - record.TTFT
	// Ignore responses whose generation time is too short to measure (for
	// example a single buffered chunk): they would report absurd speeds.
	if out >= 20 && generation >= minGenerationSample {
		seconds := generation.Seconds()
		stats.genSeconds += seconds
		stats.tpsSamples = append(stats.tpsSamples, float64(out)/seconds)
		if len(stats.tpsSamples) > maxTPSSamples {
			stats.tpsSamples = stats.tpsSamples[len(stats.tpsSamples)-maxTPSSamples:]
		}
	}
}

func applyReading(acct *account, reading quotaReading, source string, authoritative bool, observedAt time.Time) {
	if acct.Weekly.Known && observedAt.Before(acct.Weekly.ObservedAt) {
		return
	}
	acct.Weekly = windowState{
		Known:         reading.PercentRemaining != nil,
		Utilization:   reading.utilization(),
		ResetAt:       reading.NextRegenAt,
		Source:        source + " (" + reading.Shape + ")",
		Authoritative: authoritative,
		ObservedAt:    observedAt,
	}
	acct.NextRegenPercent = copyFloat(reading.NextRegenPercent)
	if reading.PercentRemaining != nil && *reading.PercentRemaining > 0 && !acct.exhaustedUntil.IsZero() {
		// Fresh evidence that credits remain clears a learned exhaustion;
		// the threshold check still applies to the reported level.
		acct.exhaustedUntil = time.Time{}
		acct.exhaustedWhy = ""
	}
}

// pollLoop polls every account's quota endpoint on the configured interval
// until Close. Each poll is best effort: failures are recorded in status and
// never block scheduling.
func (m *Module) pollLoop() {
	m.mu.Lock()
	stop, done, wake := m.pollStop, m.pollDone, m.pollWake
	m.mu.Unlock()
	defer close(done)
	m.pollOnce(context.Background(), false)
	for {
		m.mu.Lock()
		interval := m.cfg.QuotaRefresh
		m.mu.Unlock()
		if interval <= 0 {
			interval = defaultQuotaRefresh
		}
		timer := time.NewTimer(minDuration(interval, minQuotaRefresh))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
		m.pollOnce(context.Background(), false)
	}
}

type pollTarget struct {
	fingerprint string
	key         string
	endpoint    string
}

// pollOnce polls every due account. force ignores the interval (tests and
// the first poll after a key change).
func (m *Module) pollOnce(ctx context.Context, force bool) {
	m.mu.Lock()
	if !m.activeLocked() || m.cfg.QuotaRefresh <= 0 && !force {
		m.mu.Unlock()
		return
	}
	now := m.clock()
	var targets []pollTarget
	for fingerprint, acct := range m.accounts {
		if acct.Placeholder || acct.QuotaURL == "" {
			continue
		}
		if !force && !acct.lastPollAt.IsZero() && now.Sub(acct.lastPollAt) < m.cfg.QuotaRefresh {
			continue
		}
		acct.lastPollAt = now
		targets = append(targets, pollTarget{fingerprint: fingerprint, key: acct.key, endpoint: acct.QuotaURL})
	}
	client := m.client()
	m.mu.Unlock()

	for _, target := range targets {
		reading, err := fetchQuota(ctx, client, target.endpoint, target.key)
		m.mu.Lock()
		acct := m.accounts[target.fingerprint]
		if acct != nil {
			acct.polls++
			if err != nil {
				acct.pollFailures++
				acct.lastPollErr = bounded(err.Error(), 160)
			} else {
				acct.lastPollErr = ""
				applyReading(acct, reading, "quota poll", true, m.clock())
			}
		}
		m.mu.Unlock()
	}
}

// Status reports lanes, quota, routing counters and measured throughput.
func (m *Module) Status(context.Context) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()

	type statusWindow struct {
		Known         bool       `json:"known"`
		Utilization   *float64   `json:"utilization,omitempty"`
		Exhausted     bool       `json:"exhausted"`
		ResetAt       *time.Time `json:"resets_at,omitempty"`
		Source        string     `json:"source,omitempty"`
		Authoritative bool       `json:"authoritative,omitempty"`
	}
	type statusAccount struct {
		Name             string                  `json:"name"`
		Provider         string                  `json:"provider"`
		Kind             laneKind                `json:"kind"`
		AuthID           string                  `json:"auth_id"`
		Account          string                  `json:"account,omitempty"`
		Learned          bool                    `json:"learned,omitempty"`
		CredentialBound  bool                    `json:"credential_bound"`
		Health           string                  `json:"health"`
		HealthReason     string                  `json:"health_reason,omitempty"`
		Windows          map[string]statusWindow `json:"windows"`
		NextRegenPercent *float64                `json:"next_regen_percent,omitempty"`
		InFlight         int                     `json:"in_flight"`
		PeakInFlight     int                     `json:"peak_in_flight"`
		PeakInFlightAt   *time.Time              `json:"peak_in_flight_at,omitempty"`
		MaxConcurrent    int                     `json:"max_concurrent"`
		Stats            statsView               `json:"stats"`
		LastPollError    string                  `json:"last_poll_error,omitempty"`
	}
	type statusRouting struct {
		PaidPicks          int64      `json:"paid_picks"`
		InfinitePicks      int64      `json:"infinite_picks"`
		Fallbacks          int64      `json:"fallbacks_to_infinite"`
		Rejections         int64      `json:"rejections"`
		InfiniteNonStream  int64      `json:"infinite_non_stream_picks"`
		LastFallbackReason string     `json:"last_fallback_reason,omitempty"`
		LastFallbackAt     *time.Time `json:"last_fallback_at,omitempty"`
	}
	body := struct {
		Provider          string          `json:"provider"`
		Status            string          `json:"status"`
		ValidationError   string          `json:"validation_error,omitempty"`
		Enabled           bool            `json:"enabled"`
		ThresholdPercent  int             `json:"threshold_percent"`
		PaidProviders     []string        `json:"paid_providers"`
		InfiniteProviders []string        `json:"infinite_providers"`
		CredentialBound   bool            `json:"credential_bound"`
		LaneLabelsError   string          `json:"lane_labels_error,omitempty"`
		Accounts          []statusAccount `json:"accounts"`
		Routing           statusRouting   `json:"routing"`
		ObservationGaps   []string        `json:"observation_gaps"`
	}{
		Provider:          providerID,
		Status:            "registered",
		ValidationError:   m.lastErr,
		LaneLabelsError:   m.labelsErr,
		Enabled:           m.cfg.Enabled,
		ThresholdPercent:  m.cfg.ThresholdPercent,
		PaidProviders:     append([]string{}, m.cfg.PaidProviders...),
		InfiniteProviders: append([]string{}, m.cfg.InfiniteProviders...),
		Accounts:          []statusAccount{},
		ObservationGaps: []string{
			"Synthetic publishes no concurrency limit; max-concurrent is an operator setting, not a provider fact",
			"resets_at is the next 2% regeneration tick (every 202 minutes), not a full weekly reset",
		},
	}
	switch {
	case m.lastErr != "":
		body.Status = "reconfigure_rejected"
	case !m.activeLocked():
		body.Status = "inactive"
	}
	ids := make([]string, 0, len(m.lanes))
	for id := range m.lanes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := m.lanes[ids[i]], m.lanes[ids[j]]
		switch {
		case left.Kind != right.Kind:
			return left.Kind == kindPaid
		case left.Provider != right.Provider:
			return left.Provider < right.Provider
		case left.learned != right.learned:
			return !left.learned
		case left.ordinal != right.ordinal:
			return left.ordinal < right.ordinal
		}
		return left.Name < right.Name
	})
	for _, id := range ids {
		item := m.lanes[id]
		acct := m.accounts[item.Account]
		bound := acct != nil && !acct.Placeholder
		if bound {
			body.CredentialBound = true
		}
		entry := statusAccount{
			Name: item.Name, Provider: item.Provider, Kind: item.Kind, AuthID: item.AuthID,
			Account: item.Account, Learned: item.learned, CredentialBound: bound,
			Windows: map[string]statusWindow{}, InFlight: m.inFlightLocked(item, now),
			PeakInFlight: item.peakInFlight,
		}
		if !item.peakAt.IsZero() {
			value := item.peakAt
			entry.PeakInFlightAt = &value
		}
		if item.Kind == kindPaid {
			entry.MaxConcurrent = m.cfg.MaxConcurrent
			window := statusWindow{Known: false, Source: "unknown"}
			if used, known := effectiveUtilization(acct, now); known {
				window = statusWindow{Known: true, Utilization: floatPtr(round4(used)), Source: acct.Weekly.Source, Authoritative: acct.Weekly.Authoritative}
				if !acct.Weekly.ResetAt.IsZero() {
					value := acct.Weekly.ResetAt
					window.ResetAt = &value
				}
				entry.NextRegenPercent = copyFloat(acct.NextRegenPercent)
			}
			if acct != nil && acct.exhaustedUntil.After(now) {
				window.Exhausted = true
			}
			entry.Windows["weekly"] = window
			if acct != nil {
				entry.LastPollError = acct.lastPollErr
			}
			ok, why := m.paidAvailableLocked(item, now)
			entry.Health, entry.HealthReason = healthLabel(ok, why)
		} else {
			entry.MaxConcurrent = m.cfg.InfiniteMaxConcurrent
			entry.Windows["weekly"] = statusWindow{Known: true, Utilization: floatPtr(0), Source: "infinite mode (free, not metered)"}
			ok, why := m.infiniteAvailableLocked(item, now)
			entry.Health, entry.HealthReason = healthLabel(ok, why)
		}
		if !bound {
			entry.Health = "unbound"
			switch {
			case item.learned && item.Kind == kindPaid:
				entry.HealthReason = unboundPaidReason
			case item.learned:
				entry.HealthReason = "credential not bound to a known synthetic.new account (re-keyed or added after the last reconfigure)"
			default:
				entry.HealthReason = "no API key yet (placeholder): paste the synthetic.new key in the CLIProxy panel"
			}
		}
		entry.Stats = summarizeStats(item.stats)
		body.Accounts = append(body.Accounts, entry)
	}
	body.Routing = statusRouting{
		PaidPicks: m.routing.PaidPicks, InfinitePicks: m.routing.InfinitePicks, Fallbacks: m.routing.Fallbacks,
		Rejections: m.routing.Rejections, InfiniteNonStream: m.routing.InfiniteNonStream,
		LastFallbackReason: m.routing.LastFallbackReason,
	}
	if !m.routing.LastFallbackAt.IsZero() {
		value := m.routing.LastFallbackAt
		body.Routing.LastFallbackAt = &value
	}
	return json.Marshal(body)
}

func healthLabel(ok bool, why string) (string, string) {
	if ok {
		return "healthy", ""
	}
	switch {
	case strings.HasPrefix(why, "exhausted"), strings.HasPrefix(why, "weekly credits"):
		return "exhausted", why
	default:
		return "throttled", why
	}
}

// statsView is the measured-throughput block of each lane's status. Output
// tokens per second exclude time to first token, so infinite-mode queueing
// shows up in mean_ttft_ms and generation speed in the TPS fields.
type statsView struct {
	Requests        int64    `json:"requests"`
	Failures        int64    `json:"failures"`
	OutputTokens    int64    `json:"output_tokens"`
	TPSSamples      int      `json:"tps_samples"`
	MeanOutputTPS   *float64 `json:"mean_output_tps,omitempty"`
	MedianOutputTPS *float64 `json:"median_output_tps,omitempty"`
	MeanTTFTMillis  *float64 `json:"mean_ttft_ms,omitempty"`
	LastStatus      int      `json:"last_status,omitempty"`
}

func summarizeStats(stats laneStats) statsView {
	out := statsView{Requests: stats.Requests, Failures: stats.Failures, OutputTokens: stats.OutputTokens, TPSSamples: len(stats.tpsSamples), LastStatus: stats.LastStatus}
	if n := len(stats.tpsSamples); n > 0 {
		var sum float64
		for _, sample := range stats.tpsSamples {
			sum += sample
		}
		out.MeanOutputTPS = floatPtr(round1(sum / float64(n)))
		sorted := append([]float64(nil), stats.tpsSamples...)
		sort.Float64s(sorted)
		median := sorted[n/2]
		if n%2 == 0 {
			median = (sorted[n/2-1] + sorted[n/2]) / 2
		}
		out.MedianOutputTPS = floatPtr(round1(median))
	}
	if stats.ttftCount > 0 {
		out.MeanTTFTMillis = floatPtr(round1(float64(stats.ttftTotal.Milliseconds()) / float64(stats.ttftCount)))
	}
	return out
}

// Close stops the poller and releases state.
func (m *Module) Close(context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	stop, done, started := m.pollStop, m.pollDone, m.pollerStarted
	m.mu.Unlock()
	if started {
		close(stop)
		<-done
	}
	m.mu.Lock()
	m.lanes = nil
	m.accounts = nil
	m.mu.Unlock()
	return nil
}

// --- helpers ---

type stableIDGenerator struct{ counters map[string]int }

func newStableIDGenerator() *stableIDGenerator {
	return &stableIDGenerator{counters: make(map[string]int)}
}

// next reproduces CLIProxyAPI's StableIDGenerator.Next (a non-security
// interoperability identifier): sha256 over kind and each trimmed part,
// NUL-separated, first 12 hex characters, -N suffix for repeats.
func (g *stableIDGenerator) next(kind string, parts ...string) string {
	hasher := sha256.New() // codeql[go/weak-sensitive-data-hashing] Upstream non-security ID contract.
	hasher.Write([]byte(kind))
	for _, part := range parts {
		hasher.Write([]byte{0})
		hasher.Write([]byte(strings.TrimSpace(part)))
	}
	short := hex.EncodeToString(hasher.Sum(nil))[:12]
	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return kind + ":" + short
}

func keyFingerprint(key string) string {
	digest := sha256.Sum256([]byte("synthetic-account\x00" + key))
	return "acct-" + hex.EncodeToString(digest[:])[:10]
}

// isPlaceholderKey reports keys an operator has not filled in yet, such as
// "<API KEY PLACEHOLDER>". Those are never sent to the quota endpoint.
func isPlaceholderKey(key string) bool {
	key = strings.TrimSpace(key)
	return key == "" || strings.ContainsAny(key, "<> ") || strings.Contains(strings.ToLower(key), "placeholder")
}

func toSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func sortLanes(items []*lane) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].AuthID < items[j].AuthID })
}

func requestTime(req pluginapi.SchedulerPickRequest, fallback time.Time) time.Time {
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
	return fallback
}

// summarizeBody keeps the human-readable error text of a failure body:
// Synthetic answers {"error":"..."}; OpenAI-style {"error":{"message":"..."}}
// is accepted too. Anything else is whitespace-collapsed and bounded.
func summarizeBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) > maxFailureBody {
		body = body[:maxFailureBody]
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(body), &envelope) == nil && len(envelope.Error) > 0 {
		var text string
		if json.Unmarshal(envelope.Error, &text) == nil && text != "" {
			body = text
		} else {
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
				body = nested.Message
			}
		}
	}
	body = strings.Join(strings.Fields(body), " ")
	return bounded(body, 160)
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func round4(value float64) float64 {
	return float64(int64(value*10000+0.5)) / 10000
}

func round1(value float64) float64 {
	return float64(int64(value*10+0.5)) / 10
}

type schedulerError struct {
	code    string
	message string
}

func newSchedulerError(code, message string) error {
	return &schedulerError{code: code, message: message}
}

func (e *schedulerError) Error() string { return e.code + ": " + e.message }

// Coded marks rejections retryable with HTTP 429 so clients back off and
// retry instead of treating the request as invalid.
func (e *schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.code, Message: e.message, Retryable: true, HTTPStatus: http.StatusTooManyRequests}
}
