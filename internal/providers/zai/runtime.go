package zai

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
func (realClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type runtimeSnapshot struct {
	Config       pluginConfig
	BaseAccounts []account
	Accounts     []account
	Store        *secureStore
	Quota        map[string]accountQuotaState
	Health       map[string]accountHealthState
	Routing      routingState
	byAuthID     map[string]string
	byIdentity   map[string]account
	Generation   uint64
}

type refreshGeneration struct {
	done       chan struct{}
	err        error
	generation uint64
	accounts   []account
	timeout    time.Duration
}

type pluginRuntime struct {
	mu             sync.RWMutex
	snapshot       *runtimeSnapshot
	lastErr        error
	stopped        bool
	reconfigures   sync.WaitGroup
	workers        sync.WaitGroup
	usage          sync.WaitGroup
	refreshWorkers sync.WaitGroup
	operations     sync.WaitGroup
	cancel         context.CancelFunc
	refreshContext context.Context
	refreshCancel  context.CancelFunc
	clock          clock
	httpClient     httpDoer
	endpoint       string
	persist        func(*secureStore, persistedState) error
	saveSettings   func(*secureStore, settingsFile) error
	settingsMu     sync.Mutex
	pollersMu      sync.Mutex
	persistMu      sync.Mutex
	persistWrite   func(*secureStore, persistedState) error
	persistNext    atomic.Uint64
	persisted      atomic.Uint64
	refresh        *refreshGeneration
	shutdownDone   chan struct{}
	shutdownErr    error
	now            func() time.Time
}

func (r *pluginRuntime) reconfigure(rawConfig []byte) error {
	cfg, err := parsePluginConfig(rawConfig)
	if err != nil {
		return r.recordError(err)
	}
	configPath, err := resolveCPAConfigPath(cfg.CPAConfigPath)
	if err != nil {
		return r.recordError(err)
	}
	cpa, err := loadCPAConfig(configPath)
	if err != nil {
		return r.recordError(err)
	}
	providerKeys := cpaProviderKeys(cpa)
	if err = validateSchedulerDeployment(cpa.Plugins); err != nil {
		return r.recordError(err, providerKeys...)
	}
	authDir, err := resolveAuthDir(cpa.AuthDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	return r.reconfigureWithAccounts(cfg, authDir, cpa, providerKeys)
}

// reconfigureWithAccounts is the shared tail of reconfigure, factored out so
// zaiModule.Reconfigure can drive it directly with a coordinator-resolved
// auth dir and CPA projection, skipping the CPA-config-file load and
// scheduler-slot-exclusivity check the coordinator now owns.
func (r *pluginRuntime) reconfigureWithAccounts(cfg pluginConfig, authDir string, cpa cpaConfigProjection, providerKeys []string) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	r.reconfigures.Add(1)
	r.mu.Unlock()
	defer r.reconfigures.Done()

	store, err := newSecureStore(authDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	keepStore := false
	defer func() {
		if !keepStore {
			_ = store.close()
		}
	}()
	accounts, err := discoverAccounts(cpa, cfg)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	baseAccounts := append([]account(nil), accounts...)
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	settings, err := store.recoverSettings()
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	if err = applyStoredSettings(accounts, settings); err != nil {
		return r.recordError(err, providerKeys...)
	}
	cfg, err = applyStoredConfig(cfg, settings)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	persisted, err := store.loadStateAt(r.runtimeNow())
	if err != nil {
		return r.recordError(err, providerKeys...)
	}

	// Build the staged quota view before taking the publish lock. The swap
	// below runs under r.mu, and handleUsage serializes on the same mutex
	// with a generation check, so usage accepted while staging either lands
	// in the old snapshot first (and is re-read from persisted state only if
	// it was durably saved) or lands in the new snapshot after the swap —
	// never silently dropped by an overwrite.
	now := r.runtimeNow()
	quota := make(map[string]accountQuotaState, len(accounts))
	health := make(map[string]accountHealthState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*2)
	byIdentity := make(map[string]account, len(accounts))
	for i := range accounts {
		state := persisted.Accounts[accounts[i].Identity]
		if state.CompleteSince.IsZero() {
			state.CompleteSince = now
		}
		state.compact(now, cfg.StateRetention)
		quota[accounts[i].Identity] = state
		if state.Authoritative != nil {
			syncPlanFromUpstream(accounts, accounts[i].Identity, state.Authoritative.Plan)
		}
		healthState := restorePersistedHealth(persisted.Health[accounts[i].Identity], now)
		health[accounts[i].Identity] = quotaCapacityHealth(healthState, state.view(now, accounts[i], cfg), cfg.ThresholdPercent)
		indexAccountAuthIDs(byAuthID, accounts[i])
		byIdentity[accounts[i].Identity] = accounts[i]
	}
	staged := &runtimeSnapshot{
		Config: cfg, BaseAccounts: baseAccounts, Accounts: accounts, Store: store, Quota: quota, Health: health,
		byAuthID: byAuthID, byIdentity: byIdentity,
	}
	if err := r.commitSnapshot(staged); err != nil {
		return err
	}
	keepStore = true
	return nil
}

func (r *pluginRuntime) recordError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	redacted := redactError(err, secrets...)
	r.mu.Lock()
	if !r.stopped {
		r.lastErr = redacted
	}
	r.mu.Unlock()
	return redacted
}

func (r *pluginRuntime) current() (*runtimeSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		if r.lastErr != nil {
			return nil, r.lastErr
		}
		return nil, fmt.Errorf("plugin is not configured")
	}
	return cloneRuntimeSnapshot(r.snapshot), nil
}

// accountNameForAuthID returns the display name of the account owning
// authID, or "". It reads the live snapshot under the read lock without
// cloning it, because it runs once per usage record.
func (r *pluginRuntime) accountNameForAuthID(authID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return ""
	}
	identity := r.snapshot.byAuthID[strings.TrimSpace(authID)]
	if identity == "" {
		return ""
	}
	return r.snapshot.byIdentity[identity].Name
}

func sameSecureStore(left, right *secureStore) bool {
	return left != nil && right != nil && left.dir == right.dir
}

func cloneRuntimeSnapshot(source *runtimeSnapshot) *runtimeSnapshot {
	copySnapshot := *source
	copySnapshot.BaseAccounts = append([]account(nil), source.BaseAccounts...)
	copySnapshot.Accounts = append([]account(nil), source.Accounts...)
	copySnapshot.byAuthID = make(map[string]string, len(source.byAuthID))
	for authID, identity := range source.byAuthID {
		copySnapshot.byAuthID[authID] = identity
	}
	copySnapshot.byIdentity = make(map[string]account, len(source.byIdentity))
	for identity, item := range source.byIdentity {
		copySnapshot.byIdentity[identity] = item
	}
	copySnapshot.Health = make(map[string]accountHealthState, len(source.Health))
	for identity, state := range source.Health {
		copySnapshot.Health[identity] = state
	}
	copySnapshot.Routing = routingState{cursors: make(map[string]uint64, len(source.Routing.cursors))}
	for scope, cursor := range source.Routing.cursors {
		copySnapshot.Routing.cursors[scope] = cursor
	}
	copySnapshot.Quota = make(map[string]accountQuotaState, len(source.Quota))
	for identity, state := range source.Quota {
		state.Events = append([]creditEvent(nil), state.Events...)
		state.DedupHashes = append([]string(nil), state.DedupHashes...)
		if state.Authoritative != nil {
			copyQuota := *state.Authoritative
			state.Authoritative = &copyQuota
		}
		copySnapshot.Quota[identity] = state
	}
	return &copySnapshot
}

// hasSnapshot is called from the linux/cgo ABI boundary, which is excluded
// from the default non-CGO lint build.
//
//nolint:unused
func (r *pluginRuntime) hasSnapshot() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot != nil
}

func (r *pluginRuntime) validationStatus() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastErr == nil {
		return ""
	}
	return boundedStatus(r.lastErr.Error())
}

func (r *pluginRuntime) handleUsage(record pluginapi.UsageRecord) error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	now := r.runtimeNow()
	resetAt, resetReason, hasResetHint := parseRateLimitHint(record, now)

	r.mu.Lock()
	if r.stopped || r.snapshot == nil {
		r.mu.Unlock()
		return fmt.Errorf("plugin is not configured")
	}
	// Add while holding the same lock shutdown uses to close admission. Once
	// stopped is set no new handler can increment this group, so Wait cannot
	// race an Add from zero.
	r.usage.Add(1)
	defer r.usage.Done()
	identity := r.snapshot.byAuthID[strings.TrimSpace(record.AuthID)]
	if identity == "" {
		r.mu.Unlock()
		return nil
	}

	if r.snapshot.Health == nil {
		r.snapshot.Health = make(map[string]accountHealthState)
	}
	health := r.snapshot.Health[identity]
	if record.Failed {
		switch record.Failure.StatusCode {
		case 401, 403:
			health.suspendUntil(now.Add(r.snapshot.Config.SuspendDuration))
		case 429:
			// A documented request-rate code with no long reset hint, on an
			// account that still has allowance, backs off only this account
			// so CPA retries a sibling and pick does not report the whole lane
			// lost. A long hint, a sustained streak, or any other 429 keeps the
			// conservative exhaustion path.
			escalate := true
			if transientRateLimit(record) && !health.CapacityExhausted && (!hasResetHint || !resetAt.After(now.Add(requestRateBackoffCap))) {
				hintedAt := time.Time{}
				if hasResetHint {
					hintedAt = resetAt
				}
				escalate = health.throttle(now, hintedAt, r.snapshot.Config.FallbackCooldown)
				if escalate {
					hasResetHint = false
				}
			}
			if escalate {
				if !hasResetHint {
					resetAt, resetReason = rateLimitReset(now, health, r.snapshot.Config.FallbackCooldown)
				}
				health.exhaustUntil(resetAt, resetReason)
			}
		}
		r.snapshot.Health[identity] = health
	} else if health.ThrottleStreak > 0 && !health.ThrottledUntil.After(now) {
		// A served request after the backoff expired ends the streak.
		health.clearThrottle()
		r.snapshot.Health[identity] = health
	}
	if r.snapshot.Quota == nil || r.snapshot.Store == nil {
		r.mu.Unlock()
		return nil
	}

	state := r.snapshot.Quota[identity]
	if state.CompleteSince.IsZero() {
		state.CompleteSince = now
	}
	if record.Failed && usageDetailEmpty(record.Detail) {
		state.DeliveryWarning = true
	} else {
		eventAt := usageTimestamp(record, now)
		if eventAt.After(now.Add(maxPersistedClockSkew)) {
			state.DeliveryWarning = true
		} else if hash := usageDedupHash(record); state.seenDedup(hash) {
			// Heuristic dedup hit: keep the warning but add no event.
		} else if estimate, err := estimateUsageCredits(record, eventAt); err != nil {
			state.UnknownModelWarning = true
			state.DeliveryWarning = true
		} else {
			state.addEvent(creditEvent{At: eventAt, Microcredits: estimate.Microcredits, Model: estimate.Model})
			state.compact(now, r.snapshot.Config.StateRetention)
		}
	}
	r.snapshot.Quota[identity] = state
	r.refreshCapacityLocked(identity, now)
	persisted := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	return r.persistStateLocked(store, persisted)
}

// updateCapacity is the in-memory adapter used by quota polling. Results are
// generation-bound so a late poll cannot mutate a replacement snapshot.
func (r *pluginRuntime) updateCapacity(generation uint64, identity string, update capacityUpdate) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil || r.snapshot.Generation != generation {
		return false
	}
	state, exists := r.snapshot.Health[identity]
	if !exists {
		return false
	}
	state.CapacityExhausted = update.Exhausted
	state.CapacityResetAt = update.ResetAt.UTC()
	state.CapacitySource = boundedHealthReason(update.Source)
	r.snapshot.Health[identity] = state
	return true
}

func (r *pluginRuntime) refreshCapacityLocked(identity string, now time.Time) accountHealthState {
	state := r.snapshot.Health[identity]
	quota, exists := r.snapshot.Quota[identity]
	item, managed := r.snapshot.byIdentity[identity]
	if !exists || !managed {
		return state
	}
	state = quotaCapacityHealth(state, quota.view(now, item, r.snapshot.Config), r.snapshot.Config.ThresholdPercent)
	r.snapshot.Health[identity] = state
	return state
}

func (r *pluginRuntime) health(identity string) (accountHealth, bool) {
	now := r.runtimeNow()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapshot == nil {
		return accountHealth{}, false
	}
	if _, exists := r.snapshot.Health[identity]; !exists {
		return accountHealth{}, false
	}
	state := r.refreshCapacityLocked(identity, now)
	return state.assess(r.snapshot.byIdentity[identity], now), true
}

func (r *pluginRuntime) runtimeNow() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return r.runtimeClock().Now().UTC()
}

func usageDetailEmpty(detail pluginapi.UsageDetail) bool {
	return detail.InputTokens == 0 && detail.OutputTokens == 0 && detail.CacheReadTokens == 0 && detail.CacheCreationTokens == 0
}

func usageTimestamp(record pluginapi.UsageRecord, fallback time.Time) time.Time {
	if record.RequestedAt.IsZero() {
		return fallback.UTC()
	}
	return record.RequestedAt.UTC()
}

func (r *pluginRuntime) startPollers(ctx context.Context, snapshot *runtimeSnapshot) {
	for i := range snapshot.Accounts {
		item := snapshot.Accounts[i]
		if item.Disabled {
			continue
		}
		r.workers.Add(1)
		go r.pollAccount(ctx, item.Identity, item.key, snapshot.Generation, snapshot.Config.QuotaRefresh, snapshot.Config.QuotaTimeout)
	}
}

func (r *pluginRuntime) commitSnapshot(staged *runtimeSnapshot) error {
	return r.commitSnapshotAfter(staged, nil)
}

// commitSnapshotAfter is the lifecycle boundary shared with management: the
// persisted setting is committed under the same locks that publish its runtime
// generation, then superseded pollers and stores are retired.
func (r *pluginRuntime) commitSnapshotAfter(staged *runtimeSnapshot, beforeCommit func() error) error {
	r.pollersMu.Lock()
	defer r.pollersMu.Unlock()
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("plugin is shutting down")
	}
	previousCancel := r.cancel
	var previousStore *secureStore
	if r.snapshot != nil {
		previousStore = r.snapshot.Store
		staged.Generation = r.snapshot.Generation + 1
		if sameSecureStore(r.snapshot.Store, staged.Store) {
			carryForwardNamedPlans(r.snapshot, staged)
			for identity, state := range r.snapshot.Quota {
				if _, exists := staged.Quota[identity]; exists {
					staged.Quota[identity] = state
				}
			}
			for identity, state := range r.snapshot.Health {
				if _, exists := staged.Health[identity]; exists {
					staged.Health[identity] = state
				}
			}
			if previousStore != staged.Store {
				_ = staged.Store.close()
				staged.Store = previousStore
			}
		}
	} else {
		staged.Generation = 1
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			r.mu.Unlock()
			cancel()
			return err
		}
	}
	staged.Routing = routingState{}
	staged.Routing.initialize()
	r.cancel = cancel
	if r.refreshContext == nil {
		r.refreshContext, r.refreshCancel = context.WithCancel(context.Background())
	}
	r.snapshot = staged
	r.lastErr = nil
	pollerSnapshot := cloneRuntimeSnapshot(staged)
	r.mu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	r.startPollers(ctx, pollerSnapshot)
	if previousStore != nil && previousStore != staged.Store {
		if err := previousStore.close(); err != nil {
			return fmt.Errorf("close superseded secure store: %w", err)
		}
	}
	return nil
}

func (r *pluginRuntime) pollAccount(ctx context.Context, identity, key string, generation uint64, base, timeout time.Duration) {
	defer r.workers.Done()
	attempt := 0
	for {
		if err := r.pollOnce(ctx, identity, key, generation, timeout); err != nil && errors.Is(err, context.Canceled) {
			return
		}
		attempt++
		delay := jitteredPollInterval(base, identity, attempt)
		if failures := r.pollFailureCount(identity); failures > 0 {
			delay = pollBackoff(delay, failures)
		}
		if err := r.runtimeClock().Sleep(ctx, delay); err != nil {
			return
		}
	}
}

func (r *pluginRuntime) pollOnce(ctx context.Context, identity, key string, generation uint64, timeout time.Duration) error {
	now := r.runtimeClock().Now()
	attemptCtx, cancel := context.WithTimeout(ctx, quotaTimeout(timeout))
	defer cancel()
	snapshot, err := fetchQuota(attemptCtx, r.quotaClient(timeout), r.quotaEndpoint(), key, now)
	// A cancelled parent (shutdown or reconfigure) is not a poll failure; an
	// attemptCtx timeout with a live parent is.
	if err != nil && ctx.Err() != nil {
		return context.Canceled
	}
	r.mu.Lock()
	// Bind the result to the snapshot generation the poll was launched for:
	// a reconfigure (or shutdown) that replaced the snapshot in between must
	// not have this result mutate the new configuration's state.
	if r.stopped || r.snapshot == nil || r.snapshot.Generation != generation {
		r.mu.Unlock()
		return context.Canceled
	}
	state, exists := r.snapshot.Quota[identity]
	if !exists {
		r.mu.Unlock()
		return nil
	}
	state.LastPollAttempt = now
	if err != nil {
		state.ConsecutiveFailures++
		state.LastPollError = boundedStatus(err.Error())
	} else {
		if state.Authoritative == nil {
			state.LastSourceTransition = "estimated_to_authoritative"
		} else {
			view := state.view(now, accountByIdentity(r.snapshot.Accounts, identity), r.snapshot.Config)
			state.LastDivergence = formatQuotaDivergence(view, snapshot)
		}
		state.Authoritative = &snapshot
		state.LastPollError = ""
		state.ConsecutiveFailures = 0
		// Keep the fallback buckets aligned with the plan the upstream
		// actually reports (ARCHITECTURE.md: "server-reported plan level
		// selects the normal Lite/Pro/Max defaults"): a configured Max
		// account that upstream reports as Lite must fall back to Lite
		// capacity during an outage, not the stale configured buckets.
		if syncPlanFromUpstream(r.snapshot.Accounts, identity, snapshot.Plan) {
			r.snapshot.byIdentity[identity] = accountByIdentity(r.snapshot.Accounts, identity)
		}
	}
	r.snapshot.Quota[identity] = state
	r.refreshCapacityLocked(identity, now)
	persisted := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	persistErr := r.persistState(store, persisted)
	if err != nil {
		return err
	}
	return persistErr
}

// syncPlanFromUpstream aligns an account's plan and fallback buckets with a
// plan reported by the quota endpoint. Explicit custom credit buckets are
// never overridden — only named-plan defaults follow the upstream plan.
func syncPlanFromUpstream(accounts []account, identity, plan string) bool {
	if plan == "" {
		return false
	}
	buckets, known := planBuckets[plan]
	if !known {
		return false
	}
	for i := range accounts {
		if accounts[i].Identity == identity {
			// Explicit account or stored-setting choices take precedence over
			// upstream-derived named-plan defaults, including persisted snapshots
			// loaded during reconfigure.
			if accounts[i].Plan == "custom" || accounts[i].planExplicit || accounts[i].fiveHourCreditsExplicit || accounts[i].weeklyCreditsExplicit {
				return false
			}
			if accounts[i].Plan != plan {
				accounts[i].Plan = plan
				accounts[i].FiveHourCredits = buckets.FiveHour
				accounts[i].WeeklyCredits = buckets.Weekly
				return true
			}
			return false
		}
	}
	return false
}

// carryForwardNamedPlans preserves an upstream-discovered named plan across a
// same-store reconfigure that staged before the poll completed. Explicit plan
// or bucket overrides on either side remain authoritative and are never
// mistaken for upstream-derived state.
func carryForwardNamedPlans(live, staged *runtimeSnapshot) {
	liveAccounts := make(map[string]account, len(live.Accounts))
	for _, item := range live.Accounts {
		liveAccounts[item.Identity] = item
	}
	for i := range staged.Accounts {
		liveAccount, exists := liveAccounts[staged.Accounts[i].Identity]
		if !exists || liveAccount.Plan == "custom" || liveAccount.planExplicit || liveAccount.fiveHourCreditsExplicit || liveAccount.weeklyCreditsExplicit {
			continue
		}
		if staged.Accounts[i].planExplicit || staged.Accounts[i].fiveHourCreditsExplicit || staged.Accounts[i].weeklyCreditsExplicit {
			continue
		}
		buckets, known := planBuckets[liveAccount.Plan]
		if !known {
			continue
		}
		staged.Accounts[i].Plan = liveAccount.Plan
		staged.Accounts[i].FiveHourCredits = buckets.FiveHour
		staged.Accounts[i].WeeklyCredits = buckets.Weekly
		staged.byIdentity[staged.Accounts[i].Identity] = staged.Accounts[i]
	}
}

// persistState commits a persistence snapshot. Commits are serialized and
// versioned: an older generation that reaches the mutex after a newer
// generation's commit must never overwrite it (persistMu orders the write,
// the monotonic generation check rejects stale ones).
func (r *pluginRuntime) persistState(store *secureStore, state persistedState) error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	return r.persistStateLocked(store, state)
}

func (r *pluginRuntime) persistStateLocked(store *secureStore, state persistedState) error {
	if state.Generation != 0 && state.Generation <= r.persisted.Load() {
		return nil
	}
	persist := r.persist
	if r.persistWrite != nil {
		persist = r.persistWrite
	}
	if persist == nil {
		persist = func(store *secureStore, state persistedState) error { return store.saveState(state) }
	}
	if err := persist(store, state); err != nil {
		r.mu.Lock()
		if r.snapshot != nil && r.snapshot.Generation == state.RuntimeGeneration && r.snapshot.Store == store {
			for identity := range state.Accounts {
				accountState, exists := r.snapshot.Quota[identity]
				if !exists {
					continue
				}
				accountState.PersistenceWarning = true
				accountState.DeliveryWarning = true
				r.snapshot.Quota[identity] = accountState
			}
		}
		r.mu.Unlock()
		return fmt.Errorf("persist quota state: %w", err)
	}
	if state.Generation != 0 {
		r.persisted.Store(state.Generation)
	}
	return nil
}

func (r *pluginRuntime) persistenceSnapshotLocked() persistedState {
	state := persistedState{
		Version: persistedStateVersion, Accounts: make(map[string]accountQuotaState, len(r.snapshot.Quota)),
		Health: make(map[string]persistedHealthState, len(r.snapshot.Health)), Generation: r.persistNext.Add(1), RuntimeGeneration: r.snapshot.Generation,
	}
	for identity, accountState := range r.snapshot.Quota {
		accountState.Events = append([]creditEvent(nil), accountState.Events...)
		accountState.DedupHashes = append([]string(nil), accountState.DedupHashes...)
		if accountState.Authoritative != nil {
			copyQuota := *accountState.Authoritative
			accountState.Authoritative = &copyQuota
		}
		state.Accounts[identity] = accountState
	}
	now := r.runtimeNow()
	for identity, health := range r.snapshot.Health {
		if persisted, active := health.persisted(now); active {
			state.Health[identity] = persisted
		}
	}
	return state
}

func (r *pluginRuntime) pollFailureCount(identity string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return 0
	}
	return r.snapshot.Quota[identity].ConsecutiveFailures
}

func (r *pluginRuntime) quotaView(identity string) (accountQuotaView, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return accountQuotaView{}, false
	}
	state, ok := r.snapshot.Quota[identity]
	if !ok {
		return accountQuotaView{}, false
	}
	return state.view(r.runtimeClock().Now(), accountByIdentity(r.snapshot.Accounts, identity), r.snapshot.Config), true
}

func (r *pluginRuntime) managementStatus(status string) managementStatusBody {
	now := r.runtimeNow()
	r.mu.Lock()
	validationError := ""
	if r.lastErr != nil {
		validationError = boundedStatus(r.lastErr.Error())
	}
	result := managementStatusBody{
		Plugin:          pluginID,
		Status:          status,
		Version:         pluginVersion,
		GeneratedAt:     now,
		ValidationError: validationError,
	}
	defer r.mu.Unlock()
	if r.snapshot == nil {
		return result
	}
	result.Accounts = make([]managementAccountStatus, 0, len(r.snapshot.Accounts))
	for _, item := range r.snapshot.Accounts {
		view := r.snapshot.Quota[item.Identity].view(now, item, r.snapshot.Config)
		health := r.refreshCapacityLocked(item.Identity, now)
		accountStatus := managementAccountStatus{
			Identity:               item.Identity,
			Cooldown:               managementCooldown(health, now),
			Name:                   item.Name,
			KeySuffix:              "redacted",
			Plan:                   item.Plan,
			FiveHourUtilization:    utilization(view.FiveHour),
			WeeklyUtilization:      utilization(view.Weekly),
			FiveHourResetsAt:       nullableTime(view.FiveHour.ResetsAt),
			WeeklyResetsAt:         nullableTime(view.Weekly.ResetsAt),
			QuotaSource:            managementQuotaSource(view.Source),
			QuotaObservedAt:        view.ObservedAt,
			QuotaStale:             view.Stale,
			QuotaError:             view.Warning,
			FiveHourError:          view.FiveHourError,
			Offpeak:                isOffpeak(now),
			Health:                 health.assess(item, now).Status,
			EstimatorCompleteSince: view.CompleteSince,
			DeliveryWarning:        view.DeliveryWarning,
			PersistenceWarning:     view.PersistenceWarning,
			UnknownModelWarning:    view.UnknownModelWarning,
			HeuristicDedupWarning:  view.DedupCollisionWarn,
			DedupMode:              "bounded_hash_heuristic",
		}
		if !view.ObservedAt.IsZero() {
			accountStatus.QuotaAgeSeconds = maxInt64(0, int64(now.Sub(view.ObservedAt)/time.Second))
		}
		result.Accounts = append(result.Accounts, accountStatus)
	}
	return result
}

func utilization(window quotaWindow) float64 {
	if window.BucketMicrocredits <= 0 {
		return 1
	}
	return float64(window.ConsumedMicrocredits) / float64(window.BucketMicrocredits)
}

func nullableTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func managementQuotaSource(source string) string {
	if source == "authoritative" {
		return "quota_api"
	}
	return "estimate"
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (r *pluginRuntime) runtimeClock() clock {
	if r.clock != nil {
		return r.clock
	}
	return realClock{}
}

func (r *pluginRuntime) quotaClient(timeout time.Duration) httpDoer {
	if r.httpClient != nil {
		return r.httpClient
	}
	return newQuotaHTTPClient(timeout)
}

func (r *pluginRuntime) quotaEndpoint() string {
	if r.endpoint != "" {
		return r.endpoint
	}
	return quotaEndpoint
}

func (r *pluginRuntime) shutdown() error {
	r.pollersMu.Lock()
	r.mu.Lock()
	if r.shutdownDone != nil {
		done := r.shutdownDone
		r.mu.Unlock()
		r.pollersMu.Unlock()
		<-done
		r.mu.RLock()
		err := r.shutdownErr
		r.mu.RUnlock()
		return err
	}
	if r.stopped {
		err := r.shutdownErr
		r.mu.Unlock()
		r.pollersMu.Unlock()
		return err
	}
	r.stopped = true
	r.shutdownDone = make(chan struct{})
	done := r.shutdownDone
	cancel := r.cancel
	refreshCancel := r.refreshCancel
	r.cancel = nil
	r.refreshCancel = nil
	r.mu.Unlock()
	r.pollersMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if refreshCancel != nil {
		refreshCancel()
	}
	r.reconfigures.Wait()
	r.workers.Wait()
	r.usage.Wait()
	r.refreshWorkers.Wait()
	r.operations.Wait()

	r.persistMu.Lock()
	r.mu.RLock()
	snapshot := r.snapshot
	var persisted persistedState
	if snapshot != nil {
		persisted = r.persistenceSnapshotLocked()
	}
	r.mu.RUnlock()
	var err error
	if snapshot != nil && snapshot.Store != nil {
		persist := r.persist
		if r.persistWrite != nil {
			persist = r.persistWrite
		}
		if persist == nil {
			persist = func(store *secureStore, state persistedState) error { return store.saveState(state) }
		}
		if persisted.Generation == 0 || persisted.Generation > r.persisted.Load() {
			if err = persist(snapshot.Store, persisted); err != nil {
				err = fmt.Errorf("persist quota state: %w", err)
			} else if persisted.Generation != 0 {
				r.persisted.Store(persisted.Generation)
			}
		}
		if err == nil {
			err = snapshot.Store.flush()
		}
	}
	r.persistMu.Unlock()
	if snapshot != nil && snapshot.Store != nil {
		if closeErr := snapshot.Store.close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}

	r.mu.Lock()
	r.shutdownErr = err
	close(done)
	r.mu.Unlock()
	return err
}

func quotaTimeout(timeout time.Duration) time.Duration {
	if timeout == 0 {
		return defaultQuotaTimeout
	}
	return timeout
}

func maxPollInterval(base time.Duration) time.Duration {
	return base + base/2
}

func jitteredPollInterval(base time.Duration, identity string, attempt int) time.Duration {
	if base <= 0 {
		base = defaultQuotaRefresh
	}
	minimum := base / 2
	span := base
	var seed uint64
	if len(identity) >= 16 {
		seed = binary.LittleEndian.Uint64([]byte(identity[:8])) ^ binary.LittleEndian.Uint64([]byte(identity[8:16]))
	} else {
		for i := range identity {
			seed = seed*131 + uint64(identity[i])
		}
	}
	seed ^= uint64(attempt) * 0x9e3779b97f4a7c15
	return minimum + time.Duration(seed%uint64(span+1))
}

func pollBackoff(interval time.Duration, failures int) time.Duration {
	if failures <= 1 {
		return interval
	}
	shift := failures - 1
	if shift > 4 {
		shift = 4
	}
	delay := interval * time.Duration(1<<shift)
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func accountByIdentity(accounts []account, identity string) account {
	for _, item := range accounts {
		if item.Identity == identity {
			return item
		}
	}
	return account{}
}

func formatQuotaDivergence(view accountQuotaView, authoritative quotaSnapshot) string {
	if authoritative.FiveHourError != "" {
		// The new poll's five-hour window is known-absent, not zero usage —
		// comparing it against the prior view would report a bogus swing.
		return boundedStatus(fmt.Sprintf("weekly_microcredits=%d five_hour_error=%s", authoritative.Weekly.ConsumedMicrocredits-view.Weekly.ConsumedMicrocredits, authoritative.FiveHourError))
	}
	return boundedStatus(fmt.Sprintf("five_hour_microcredits=%d weekly_microcredits=%d", authoritative.FiveHour.ConsumedMicrocredits-view.FiveHour.ConsumedMicrocredits, authoritative.Weekly.ConsumedMicrocredits-view.Weekly.ConsumedMicrocredits))
}

func cpaProviderKeys(cpa cpaConfigProjection) []string {
	keys := make([]string, 0, len(cpa.ClaudeKeys)+len(cpa.OpenAICompatibility))
	for i := range cpa.ClaudeKeys {
		keys = append(keys, cpa.ClaudeKeys[i].APIKey)
	}
	for i := range cpa.OpenAICompatibility {
		for j := range cpa.OpenAICompatibility[i].APIKeyEntries {
			keys = append(keys, cpa.OpenAICompatibility[i].APIKeyEntries[j].APIKey)
		}
	}
	return keys
}

func redactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return errors.New(message)
}

func boundedStatus(message string) string {
	return boundedText(message, 240)
}

func boundedText(message string, limit int) string {
	clean := strings.Join(strings.Fields(message), " ")
	if len(clean) > limit {
		clean = clean[:limit]
	}
	return clean
}
