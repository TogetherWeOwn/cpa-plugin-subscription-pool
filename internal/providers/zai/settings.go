package zai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const (
	maxPersistedClockSkew       = 5 * time.Minute
	settingsRecoveryVersion     = 1
	persistedStateVersion       = 2
	legacyPersistedStateVersion = 1
)

type settingsRecoveryPendingError struct {
	err error
}

func (e *settingsRecoveryPendingError) Error() string { return e.err.Error() }
func (e *settingsRecoveryPendingError) Unwrap() error { return e.err }

func settingsRecoveryPending(err error) bool {
	var pending *settingsRecoveryPendingError
	return errors.As(err, &pending)
}

type settingsFile struct {
	Version             int                       `json:"version"`
	Accounts            map[string]accountSetting `json:"accounts,omitempty"`
	ThresholdPercent    *int                      `json:"threshold_percent,omitempty"`
	PollingInterval     string                    `json:"polling_interval,omitempty"`
	AuthoritativeMaxAge string                    `json:"authoritative_max_age,omitempty"`
	Timeout             string                    `json:"timeout,omitempty"`
}

type accountSetting struct {
	Name            string `json:"name,omitempty"`
	Plan            string `json:"plan,omitempty"`
	Disabled        *bool  `json:"disabled,omitempty"`
	FiveHourCredits int64  `json:"five_hour_credits,omitempty"`
	WeeklyCredits   int64  `json:"weekly_credits,omitempty"`
}

type persistedState struct {
	Version           int                             `json:"version"`
	Accounts          map[string]accountQuotaState    `json:"accounts,omitempty"`
	Health            map[string]persistedHealthState `json:"health,omitempty"`
	Generation        uint64                          `json:"-"`
	RuntimeGeneration uint64                          `json:"-"`
}

type persistedHealthState struct {
	SuspendedUntil  time.Time `json:"suspended_until,omitempty"`
	ExhaustedUntil  time.Time `json:"exhausted_until,omitempty"`
	ExhaustedReason string    `json:"exhausted_reason,omitempty"`
}

type settingsRecovery struct {
	Version        int           `json:"version"`
	PreviousDigest string        `json:"previous_digest"`
	Desired        *settingsFile `json:"desired,omitempty"`
}

// secureStore wraps the generic securestore.Store with the settings/state
// two-phase-commit protocol this module owns: the recovery-marker dance and
// the settings/state JSON shapes are zai-specific and stay here rather than
// in the shared securestore package.
type secureStore struct {
	*securestore.Store
	dir string
}

const settingsRecoveryName = "settings.recovery.json"

// newSecureStore opens this module's own secure storage directory. authDir
// is already namespaced by the coordinator (i.e. "<auth-dir>/subscription-pool");
// this module appends only its own "providers/zai" segment.
func newSecureStore(authDir string) (*secureStore, error) {
	base := filepath.Clean(strings.TrimSpace(authDir))
	if base == "." || !filepath.IsAbs(base) {
		return nil, fmt.Errorf("auth-dir must be an absolute path")
	}
	dir := filepath.Join(base, "providers", "zai")
	store, err := securestore.New(dir)
	if err != nil {
		return nil, err
	}
	return &secureStore{Store: store, dir: dir}, nil
}

func (s *secureStore) close() error { return s.Close() }

func (s *secureStore) flush() error { return s.Flush() }

func (s *secureStore) validateDirectory() error { return s.ValidateDirectory() }

func (s *secureStore) writeJSON(name string, value any) error { return s.WriteJSON(name, value) }

func (s *secureStore) readJSONIfExists(name string, dst any) (bool, error) {
	return s.ReadJSONIfExists(name, dst)
}

func (s *secureStore) loadSettings() (settingsFile, error) {
	var settings settingsFile
	if err := s.ReadJSON("settings.json", &settings); err != nil {
		return settingsFile{}, err
	}
	if err := validateSettingsFile(settings); err != nil {
		return settingsFile{}, err
	}
	return settings, nil
}

func validateSettingsFile(settings settingsFile) error {
	if settings.Version == 0 && settingsEmpty(settings) {
		return nil
	}
	if settings.Version != 1 {
		return fmt.Errorf("settings.json has unsupported version")
	}
	if _, err := applyStoredConfig(pluginConfig{
		QuotaRefresh:        defaultQuotaRefresh,
		AuthoritativeMaxAge: defaultAuthoritativeMaxAge,
		QuotaTimeout:        defaultQuotaTimeout,
		ThresholdPercent:    defaultThreshold,
	}, settings); err != nil {
		return err
	}
	return nil
}

func (s *secureStore) saveSettings(settings settingsFile) error {
	previous, err := s.loadSettings()
	if err != nil {
		if strings.Contains(err.Error(), "too many levels of symbolic links") {
			return fmt.Errorf("refusing symlink target")
		}
		return err
	}
	recovery := settingsRecovery{
		Version:        settingsRecoveryVersion,
		PreviousDigest: settingsDigest(previous),
	}
	if err := s.WriteJSON(settingsRecoveryName, recovery); err != nil {
		return fmt.Errorf("stage settings recovery: %w", err)
	}
	recovery.Desired = &settings
	if err := s.WriteJSON(settingsRecoveryName, recovery); err != nil {
		if securestore.OutcomeOf(err) != securestore.WriteNeedsRecovery {
			return fmt.Errorf("prepare settings recovery: %w", err)
		}
		if flushErr := s.Flush(); flushErr != nil {
			current, readErr := s.loadSettings()
			if readErr == nil && settingsDigest(current) == settingsDigest(settings) {
				return nil
			}
			// The rename outcome is indeterminate. Publish the update as pending rather
			// than rejecting a desired marker that recovery may later observe.
			return &settingsRecoveryPendingError{err: fmt.Errorf("prepare settings recovery: %w", err)}
		}
	}
	if err := s.WriteJSON("settings.json", settings); err != nil {
		if securestore.OutcomeOf(err) == securestore.WriteNeedsRecovery {
			if flushErr := s.Flush(); flushErr == nil {
				return nil
			}
		}
		// A durable desired marker makes this update logically committed even when
		// settings.json itself still needs recovery.
		return &settingsRecoveryPendingError{err: fmt.Errorf("commit settings: %w", err)}
	}
	if err := s.RemoveJSON(settingsRecoveryName); err != nil {
		if securestore.OutcomeOf(err) != securestore.WriteNeedsRecovery {
			return fmt.Errorf("clear settings recovery: %w", err)
		}
	}
	return nil
}

func (s *secureStore) recoverSettings() (settingsFile, error) {
	settings, err := s.loadSettings()
	if err != nil {
		return settingsFile{}, err
	}
	var recovery settingsRecovery
	found, err := s.ReadJSONIfExists(settingsRecoveryName, &recovery)
	if err != nil {
		return settingsFile{}, err
	}
	if !found {
		return settings, nil
	}
	if recovery.Version != settingsRecoveryVersion {
		return settingsFile{}, fmt.Errorf("settings recovery marker is invalid")
	}
	currentDigest := settingsDigest(settings)
	if recovery.Desired == nil {
		if currentDigest != recovery.PreviousDigest {
			return settingsFile{}, fmt.Errorf("settings recovery marker does not match settings.json")
		}
		_ = s.RemoveJSON(settingsRecoveryName)
		return settings, nil
	}
	if validateSettingsFile(*recovery.Desired) != nil {
		return settingsFile{}, fmt.Errorf("settings recovery marker is invalid")
	}
	desiredDigest := settingsDigest(*recovery.Desired)
	switch currentDigest {
	case recovery.PreviousDigest:
		if err := s.WriteJSON("settings.json", *recovery.Desired); err != nil && securestore.OutcomeOf(err) != securestore.WriteNeedsRecovery {
			return settingsFile{}, fmt.Errorf("roll forward settings recovery: %w", err)
		}
		_ = s.RemoveJSON(settingsRecoveryName)
		return *recovery.Desired, nil
	case desiredDigest:
		_ = s.RemoveJSON(settingsRecoveryName)
		return *recovery.Desired, nil
	default:
		return settingsFile{}, fmt.Errorf("settings recovery marker does not match settings.json")
	}
}

func settingsDigest(settings settingsFile) string {
	raw, _ := json.Marshal(settings)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (s *secureStore) loadState() (persistedState, error) {
	return s.loadStateAt(time.Now().UTC())
}

func (s *secureStore) loadStateAt(now time.Time) (persistedState, error) {
	var state persistedState
	if err := s.ReadJSON("state.json", &state); err != nil {
		return persistedState{}, err
	}
	if err := validatePersistedStateAt(state, now); err != nil {
		return persistedState{}, err
	}
	return state, nil
}

func (s *secureStore) saveState(state persistedState) error {
	if err := validatePersistedState(state); err != nil {
		return err
	}
	return s.WriteJSON("state.json", state)
}

func validatePersistedState(state persistedState) error {
	return validatePersistedStateAt(state, time.Now().UTC())
}

func validatePersistedStateAt(state persistedState, now time.Time) error {
	if state.Version == 0 && len(state.Accounts) == 0 && len(state.Health) == 0 {
		return nil
	}
	if state.Version != legacyPersistedStateVersion && state.Version != persistedStateVersion {
		return fmt.Errorf("state.json has unsupported version")
	}
	if state.Version == legacyPersistedStateVersion && len(state.Health) != 0 {
		return fmt.Errorf("state.json has unsupported health data")
	}
	for identity, accountState := range state.Accounts {
		if err := validateAccountIdentity(identity); err != nil {
			return err
		}
		if err := validateAccountQuotaState(accountState, now); err != nil {
			return fmt.Errorf("state.json contains invalid account state")
		}
	}
	for identity, healthState := range state.Health {
		if err := validateAccountIdentity(identity); err != nil {
			return err
		}
		if err := validatePersistedHealthState(healthState); err != nil {
			return fmt.Errorf("state.json contains invalid health state")
		}
	}
	return nil
}

func validateAccountIdentity(identity string) error {
	if len(identity) != sha256.Size*2 {
		return fmt.Errorf("state.json contains invalid account identity")
	}
	if _, err := hex.DecodeString(identity); err != nil {
		return fmt.Errorf("state.json contains invalid account identity")
	}
	return nil
}

func validatePersistedHealthState(state persistedHealthState) error {
	const maxPersistedHealthYear = 9999
	if !state.SuspendedUntil.IsZero() && (state.SuspendedUntil.Year() < 2000 || state.SuspendedUntil.Year() > maxPersistedHealthYear) {
		return fmt.Errorf("invalid suspension time")
	}
	if !state.ExhaustedUntil.IsZero() && (state.ExhaustedUntil.Year() < 2000 || state.ExhaustedUntil.Year() > maxPersistedHealthYear) {
		return fmt.Errorf("invalid exhaustion time")
	}
	if !state.ExhaustedUntil.IsZero() && strings.TrimSpace(state.ExhaustedReason) == "" {
		return fmt.Errorf("missing exhaustion reason")
	}
	if len(state.ExhaustedReason) > 96 {
		return fmt.Errorf("invalid exhaustion reason")
	}
	return nil
}

func validateAccountQuotaState(state accountQuotaState, now time.Time) error {
	futureLimit := now.UTC().Add(maxPersistedClockSkew)
	if state.ConsecutiveFailures < 0 || len(state.DedupHashes) > maxDedupHashes {
		return fmt.Errorf("invalid polling metadata")
	}
	if state.Authoritative != nil {
		if state.Authoritative.ObservedAt.IsZero() || state.Authoritative.ObservedAt.Year() < 2000 || state.Authoritative.ObservedAt.After(futureLimit) {
			return fmt.Errorf("invalid authoritative observation time")
		}
		if len(state.Authoritative.FiveHourError) > 240 {
			return fmt.Errorf("invalid authoritative quota")
		}
		windows := []quotaWindow{state.Authoritative.Weekly}
		// A FiveHour window that failed to parse (TOG-2497) is left
		// zero-valued on purpose and must not be validated as if it were a
		// real, unused window — that's the "known absent" case, distinct
		// from a genuinely unused window with BucketMicrocredits > 0.
		if state.Authoritative.FiveHourError == "" {
			windows = append(windows, state.Authoritative.FiveHour)
		} else if state.Authoritative.FiveHour != (quotaWindow{}) {
			return fmt.Errorf("invalid authoritative quota")
		}
		for _, window := range windows {
			if window.ConsumedMicrocredits < 0 || window.BucketMicrocredits <= 0 || window.ConsumedMicrocredits > window.BucketMicrocredits {
				return fmt.Errorf("invalid authoritative quota")
			}
			// A zero ResetsAt means "unused window, no reset pending"
			// (TOG-2490); only a present-but-stale reset is invalid.
			if !window.ResetsAt.IsZero() && !window.ResetsAt.After(state.Authoritative.ObservedAt) {
				return fmt.Errorf("invalid authoritative quota")
			}
		}
	}
	last := time.Time{}
	for _, event := range state.Events {
		if event.At.IsZero() || event.At.After(futureLimit) || event.Microcredits <= 0 || normalizeModelName(event.Model) == "" || (!last.IsZero() && event.At.Before(last)) {
			return fmt.Errorf("invalid credit event")
		}
		last = event.At
	}
	for _, hash := range state.DedupHashes {
		if len(hash) != sha256.Size*2 {
			return fmt.Errorf("invalid dedup hash")
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return fmt.Errorf("invalid dedup hash")
		}
	}
	return nil
}

func applyStoredSettings(accounts []account, settings settingsFile) error {
	if settings.Version == 0 && settingsEmpty(settings) {
		return nil
	}
	if settings.Version != 1 {
		return fmt.Errorf("settings.json has unsupported version")
	}
	if _, err := applyStoredConfig(pluginConfig{
		QuotaRefresh:        defaultQuotaRefresh,
		AuthoritativeMaxAge: defaultAuthoritativeMaxAge,
		QuotaTimeout:        defaultQuotaTimeout,
		ThresholdPercent:    defaultThreshold,
	}, settings); err != nil {
		return err
	}
	for identity, stored := range settings.Accounts {
		if len(identity) != sha256.Size*2 {
			return fmt.Errorf("settings.json contains invalid account identity")
		}
		if _, err := hex.DecodeString(identity); err != nil {
			return fmt.Errorf("settings.json contains invalid account identity")
		}
		if err := validateStoredSetting(stored); err != nil {
			return fmt.Errorf("settings.json contains invalid account settings")
		}
	}
	for i := range accounts {
		stored, ok := settings.Accounts[accounts[i].Identity]
		if !ok {
			continue
		}
		if name := strings.TrimSpace(stored.Name); name != "" {
			accounts[i].Name = name
		}
		if plan := normalizePlan(stored.Plan); plan != "" {
			accounts[i].Plan = plan
			accounts[i].planExplicit = true
			if buckets, known := planBuckets[plan]; known {
				accounts[i].FiveHourCredits = buckets.FiveHour
				accounts[i].WeeklyCredits = buckets.Weekly
			}
		}
		if stored.Disabled != nil {
			accounts[i].Disabled = *stored.Disabled
		}
		if stored.FiveHourCredits > 0 {
			accounts[i].FiveHourCredits = stored.FiveHourCredits
			accounts[i].fiveHourCreditsExplicit = true
		}
		if stored.WeeklyCredits > 0 {
			accounts[i].WeeklyCredits = stored.WeeklyCredits
			accounts[i].weeklyCreditsExplicit = true
		}
	}
	return validateAccounts(accounts)
}

func settingsEmpty(settings settingsFile) bool {
	return len(settings.Accounts) == 0 && settings.ThresholdPercent == nil && settings.PollingInterval == "" && settings.AuthoritativeMaxAge == "" && settings.Timeout == ""
}

func applyStoredConfig(cfg pluginConfig, settings settingsFile) (pluginConfig, error) {
	if settings.ThresholdPercent != nil {
		if *settings.ThresholdPercent < 1 || *settings.ThresholdPercent > 100 {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid threshold_percent")
		}
		cfg.ThresholdPercent = *settings.ThresholdPercent
	}
	var err error
	if settings.PollingInterval != "" {
		cfg.QuotaRefresh, err = parsePositiveDuration("polling_interval", settings.PollingInterval, cfg.QuotaRefresh)
		if err != nil || cfg.QuotaRefresh < time.Minute || cfg.QuotaRefresh > 3*time.Minute {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid polling_interval")
		}
	}
	if settings.AuthoritativeMaxAge != "" {
		cfg.AuthoritativeMaxAge, err = parsePositiveDuration("authoritative_max_age", settings.AuthoritativeMaxAge, cfg.AuthoritativeMaxAge)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid authoritative_max_age")
		}
	}
	if cfg.AuthoritativeMaxAge <= maxPollInterval(cfg.QuotaRefresh) {
		return pluginConfig{}, fmt.Errorf("settings.json authoritative_max_age must exceed maximum polling jitter")
	}
	if settings.Timeout != "" {
		cfg.QuotaTimeout, err = parsePositiveDuration("timeout", settings.Timeout, cfg.QuotaTimeout)
		if err != nil || cfg.QuotaTimeout < minQuotaTimeout || cfg.QuotaTimeout > maxQuotaTimeout {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid timeout")
		}
	}
	return cfg, nil
}

func validateStoredSetting(stored accountSetting) error {
	plan := normalizePlan(stored.Plan)
	if stored.Plan != "" && plan == "" {
		return fmt.Errorf("invalid plan")
	}
	if stored.FiveHourCredits < 0 || stored.WeeklyCredits < 0 {
		return fmt.Errorf("negative credit bucket")
	}
	if stored.FiveHourCredits > 0 && !validCreditBucket(stored.FiveHourCredits) || stored.WeeklyCredits > 0 && !validCreditBucket(stored.WeeklyCredits) {
		return fmt.Errorf("credit bucket exceeds supported range")
	}
	if plan == "custom" && (stored.FiveHourCredits <= 0 || stored.WeeklyCredits <= 0) {
		return fmt.Errorf("custom plan requires both credit buckets")
	}
	return nil
}

func validateAccounts(accounts []account) error {
	seenNames := make(map[string]struct{}, len(accounts))
	for i := range accounts {
		if strings.TrimSpace(accounts[i].Name) == "" {
			return fmt.Errorf("account has no name")
		}
		if normalizePlan(accounts[i].Plan) == "" {
			return fmt.Errorf("account has invalid plan")
		}
		if !validCreditBucket(accounts[i].FiveHourCredits) || !validCreditBucket(accounts[i].WeeklyCredits) {
			return fmt.Errorf("account has non-positive or out-of-range credit bucket")
		}
		nameKey := strings.ToLower(strings.TrimSpace(accounts[i].Name))
		if _, exists := seenNames[nameKey]; exists {
			return fmt.Errorf("duplicate account name")
		}
		seenNames[nameKey] = struct{}{}
	}
	return nil
}
