package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const (
	managementBasePath          = "/v0/management/plugins/zai-coding-plan"
	managementStatusPath        = managementBasePath + "/status"
	managementRefreshPath       = managementBasePath + "/refresh"
	managementUnblockPath       = managementBasePath + "/unblock"
	managementAccountConfigPath = managementBasePath + "/account-config"
	managementContentType       = "application/json"
	maxManagementRequestBody    = 64 << 10
	minQuotaTimeout             = time.Second
	maxQuotaTimeout             = 30 * time.Second
)

type managementRoutes struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

func managementRegistration() managementRoutes {
	return managementRoutes{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: managementStatusPath, Description: "Reports redacted Z.ai coding-plan quota status."},
			{Method: http.MethodPost, Path: managementRefreshPath, Description: "Forces a bounded quota refresh."},
			{Method: http.MethodPost, Path: managementUnblockPath, Description: "Recomputes account availability without erasing usage."},
			{Method: http.MethodPost, Path: managementAccountConfigPath, Description: "Updates validated non-secret account and polling settings."},
		},
	}
}

func (r *pluginRuntime) handleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > maxManagementRequestBody {
		return managementError(http.StatusRequestEntityTooLarge, "request_too_large", "management request body exceeds 64 KiB")
	}
	switch {
	case req.Method == http.MethodGet && req.Path == managementStatusPath:
		return r.statusResponse(http.StatusOK)
	case req.Method == http.MethodPost && req.Path == managementRefreshPath:
		if err := requireEmptyJSONBody(req.Body); err != nil {
			return managementError(http.StatusBadRequest, "invalid_request", err.Error())
		}
		if err := r.forceRefresh(ctx); err != nil {
			return managementError(http.StatusBadGateway, "refresh_failed", boundedStatus(err.Error()))
		}
		return r.statusResponse(http.StatusOK)
	case req.Method == http.MethodPost && req.Path == managementUnblockPath:
		var input managementAccountSelector
		if err := decodeManagementJSON(req.Body, &input); err != nil {
			return managementError(http.StatusBadRequest, "invalid_request", err.Error())
		}
		if err := r.unblock(input.Account); err != nil {
			return managementError(http.StatusBadRequest, "unblock_rejected", err.Error())
		}
		return r.statusResponse(http.StatusOK)
	case req.Method == http.MethodPost && req.Path == managementAccountConfigPath:
		var input managementAccountConfigRequest
		if err := decodeManagementJSON(req.Body, &input); err != nil {
			return managementError(http.StatusBadRequest, "invalid_request", err.Error())
		}
		if err := r.updateAccountConfig(input); err != nil {
			return managementError(http.StatusBadRequest, "invalid_account_config", err.Error())
		}
		return r.statusResponse(http.StatusOK)
	default:
		return managementError(http.StatusNotFound, "not_found", "unknown management route")
	}
}

func requireEmptyJSONBody(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var value map[string]json.RawMessage
	if err := decodeManagementJSON(body, &value); err != nil {
		return err
	}
	if len(value) != 0 {
		return fmt.Errorf("request body must be empty")
	}
	return nil
}

func decodeManagementJSON(body []byte, target any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON request body")
	}
	if err := securestore.EnsureJSONEOF(decoder); err != nil {
		return fmt.Errorf("invalid JSON request body")
	}
	return nil
}

func managementJSON(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return managementError(http.StatusInternalServerError, "encode_failed", "could not encode management response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{managementContentType}},
		Body:       body,
	}
}

func managementError(status int, code, message string) pluginapi.ManagementResponse {
	return managementJSONUnsafe(status, managementErrorBody{Error: managementErrorDetail{Code: code, Message: boundedStatus(message)}})
}

func managementJSONUnsafe(status int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{managementContentType}},
		Body:       body,
	}
}

type managementErrorBody struct {
	Error managementErrorDetail `json:"error"`
}

type managementErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type managementAccountSelector struct {
	Account string `json:"account,omitempty"`
}

type managementAccountConfigRequest struct {
	Account             string `json:"account"`
	Clear               bool   `json:"clear,omitempty"`
	Name                string `json:"name,omitempty"`
	Plan                string `json:"plan,omitempty"`
	Disabled            *bool  `json:"disabled,omitempty"`
	FiveHourCredits     *int64 `json:"five_hour_credits,omitempty"`
	WeeklyCredits       *int64 `json:"weekly_credits,omitempty"`
	ThresholdPercent    *int   `json:"threshold_percent,omitempty"`
	PollingInterval     string `json:"polling_interval,omitempty"`
	AuthoritativeMaxAge string `json:"authoritative_max_age,omitempty"`
	Timeout             string `json:"timeout,omitempty"`
}

func (r *pluginRuntime) statusResponse(statusCode int) pluginapi.ManagementResponse {
	status := "registered"
	if r.validationStatus() != "" {
		status = "reconfigure_rejected"
	}
	return managementJSON(statusCode, r.managementStatus(status))
}

func (r *pluginRuntime) forceRefresh(ctx context.Context) error {
	refresh, leader, err := r.beginRefresh()
	if err != nil {
		return err
	}
	if !leader {
		return r.waitRefresh(ctx, refresh)
	}
	defer r.refreshWorkers.Done()

	r.mu.RLock()
	base := r.refreshContext
	r.mu.RUnlock()
	if base == nil {
		base = context.Background()
	}
	refreshCtx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	refreshErr := r.runRefresh(refreshCtx, refresh)
	r.endRefresh(refresh, refreshErr)
	return refreshErr
}

func (r *pluginRuntime) waitRefresh(ctx context.Context, refresh *refreshGeneration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-refresh.done:
		return refresh.err
	}
}

func (r *pluginRuntime) beginRefresh() (*refreshGeneration, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil {
		return nil, false, fmt.Errorf("plugin is not configured")
	}
	if r.refresh != nil && r.refresh.generation == r.snapshot.Generation {
		return r.refresh, false, nil
	}
	if r.refreshContext == nil {
		r.refreshContext, r.refreshCancel = context.WithCancel(context.Background())
	}
	r.refreshWorkers.Add(1)
	r.refresh = &refreshGeneration{
		done:       make(chan struct{}),
		generation: r.snapshot.Generation,
		accounts:   append([]account(nil), r.snapshot.Accounts...),
		timeout:    quotaTimeout(r.snapshot.Config.QuotaTimeout),
	}
	return r.refresh, true, nil
}

func (r *pluginRuntime) runRefresh(ctx context.Context, refresh *refreshGeneration) error {
	refreshCtx, cancel := context.WithTimeout(ctx, refresh.timeout)
	defer cancel()
	type pollResult struct{ err error }
	results := make(chan pollResult, len(refresh.accounts))
	launched := 0
	for _, item := range refresh.accounts {
		if item.Disabled {
			continue
		}
		launched++
		go func(item account) {
			results <- pollResult{err: r.pollOnce(refreshCtx, item.Identity, item.key, refresh.generation, refresh.timeout)}
		}(item)
	}
	var failures int
	var firstError error
	for range launched {
		result := <-results
		if result.err != nil {
			failures++
			if firstError == nil {
				firstError = result.err
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("quota refresh failed for %d account(s): %s", failures, boundedStatus(firstError.Error()))
	}
	return nil
}

func (r *pluginRuntime) endRefresh(refresh *refreshGeneration, err error) {
	r.mu.Lock()
	refresh.err = err
	if r.refresh == refresh {
		r.refresh = nil
	}
	r.mu.Unlock()
	close(refresh.done)
}

func (r *pluginRuntime) unblock(accountName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil {
		return fmt.Errorf("plugin is not configured")
	}
	selector := strings.TrimSpace(accountName)
	matches := make([]string, 0, len(r.snapshot.Accounts))
	for _, item := range r.snapshot.Accounts {
		if selector != "" && !strings.EqualFold(selector, item.Name) && selector != item.KeySuffix {
			continue
		}
		matches = append(matches, item.Identity)
	}
	if len(matches) == 0 {
		return fmt.Errorf("account does not match a configured account")
	}
	if selector != "" && len(matches) != 1 {
		return fmt.Errorf("account selector is ambiguous")
	}
	now := r.runtimeNow()
	for _, identity := range matches {
		state := r.snapshot.Quota[identity]
		state.compact(now, r.snapshot.Config.StateRetention)
		r.snapshot.Quota[identity] = state
		health := r.snapshot.Health[identity]
		health.ExhaustedUntil = time.Time{}
		health.ExhaustedReason = ""
		health.clearThrottle()
		r.snapshot.Health[identity] = health
		r.refreshCapacityLocked(identity, now)
	}
	return nil
}

func (r *pluginRuntime) updateAccountConfig(input managementAccountConfigRequest) error {
	accountName := strings.TrimSpace(input.Account)
	if accountName == "" {
		return fmt.Errorf("account is required")
	}

	r.mu.Lock()
	if r.stopped || r.snapshot == nil || r.snapshot.Store == nil {
		r.mu.Unlock()
		return fmt.Errorf("plugin is not configured")
	}
	r.operations.Add(1)
	r.mu.Unlock()
	defer r.operations.Done()

	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	r.mu.RLock()
	if r.stopped || r.snapshot == nil || r.snapshot.Store == nil {
		r.mu.RUnlock()
		return fmt.Errorf("plugin is not configured")
	}
	updated := cloneRuntimeSnapshot(r.snapshot)
	r.mu.RUnlock()

	index := -1
	for i := range updated.Accounts {
		if strings.EqualFold(accountName, updated.Accounts[i].Name) || accountName == updated.Accounts[i].KeySuffix {
			if index >= 0 {
				return fmt.Errorf("account selector is ambiguous")
			}
			index = i
		}
	}
	if index < 0 {
		return fmt.Errorf("account does not match a configured account")
	}
	item := &updated.Accounts[index]

	settings, err := updated.Store.recoverSettings()
	if err != nil {
		return err
	}
	if settings.Version == 0 {
		settings.Version = 1
	}
	if settings.Accounts == nil {
		settings.Accounts = make(map[string]accountSetting)
	}
	setting := settings.Accounts[item.Identity]
	if input.Clear {
		setting = accountSetting{}
	} else {
		if input.Name != "" {
			setting.Name = strings.TrimSpace(input.Name)
		}
		if input.Plan != "" {
			previousPlan := normalizePlan(setting.Plan)
			setting.Plan = normalizePlan(input.Plan)
			if setting.Plan == "" {
				return fmt.Errorf("plan must be lite, pro, max, or custom")
			}
			if setting.Plan != "custom" && setting.Plan != previousPlan {
				if input.FiveHourCredits == nil {
					setting.FiveHourCredits = 0
				}
				if input.WeeklyCredits == nil {
					setting.WeeklyCredits = 0
				}
			}
		}
		if input.Disabled != nil {
			setting.Disabled = input.Disabled
		}
		if input.FiveHourCredits != nil {
			setting.FiveHourCredits = *input.FiveHourCredits
		}
		if input.WeeklyCredits != nil {
			setting.WeeklyCredits = *input.WeeklyCredits
		}
		if err := validateStoredSetting(setting); err != nil {
			return err
		}
	}

	cfg := updated.Config
	if input.ThresholdPercent != nil {
		if *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
			return fmt.Errorf("threshold_percent must be between 1 and 100")
		}
		cfg.ThresholdPercent = *input.ThresholdPercent
		value := *input.ThresholdPercent
		settings.ThresholdPercent = &value
	}
	if strings.TrimSpace(input.PollingInterval) != "" {
		cfg.QuotaRefresh, err = parsePositiveDuration("polling_interval", input.PollingInterval, cfg.QuotaRefresh)
		if err != nil || cfg.QuotaRefresh < time.Minute || cfg.QuotaRefresh > 3*time.Minute {
			return fmt.Errorf("polling_interval must be between one and three minutes")
		}
		settings.PollingInterval = cfg.QuotaRefresh.String()
	}
	if strings.TrimSpace(input.AuthoritativeMaxAge) != "" {
		cfg.AuthoritativeMaxAge, err = parsePositiveDuration("authoritative_max_age", input.AuthoritativeMaxAge, cfg.AuthoritativeMaxAge)
		if err != nil {
			return err
		}
		settings.AuthoritativeMaxAge = cfg.AuthoritativeMaxAge.String()
	}
	if cfg.AuthoritativeMaxAge <= maxPollInterval(cfg.QuotaRefresh) {
		return fmt.Errorf("authoritative_max_age must exceed maximum polling jitter")
	}
	if strings.TrimSpace(input.Timeout) != "" {
		cfg.QuotaTimeout, err = parsePositiveDuration("timeout", input.Timeout, defaultQuotaTimeout)
		if err != nil || cfg.QuotaTimeout < minQuotaTimeout || cfg.QuotaTimeout > maxQuotaTimeout {
			return fmt.Errorf("timeout must be between one and thirty seconds")
		}
		settings.Timeout = cfg.QuotaTimeout.String()
	}

	candidate := *item
	if input.Clear {
		if index >= len(updated.BaseAccounts) || updated.BaseAccounts[index].Identity != item.Identity {
			return fmt.Errorf("base account configuration is unavailable")
		}
		candidate = updated.BaseAccounts[index]
		delete(settings.Accounts, item.Identity)
	} else {
		applySettingToAccount(&candidate, setting)
		settings.Accounts[item.Identity] = setting
	}
	updated.Accounts[index] = candidate
	if updated.byIdentity == nil {
		updated.byIdentity = make(map[string]account, len(updated.Accounts))
	}
	updated.byIdentity[candidate.Identity] = candidate
	updated.Config = cfg
	if err := validateAccounts(updated.Accounts); err != nil {
		return err
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	r.mu.Unlock()
	return r.commitSnapshotAfter(updated, func() error {
		save := r.saveSettings
		if save == nil {
			save = func(store *secureStore, settings settingsFile) error { return store.saveSettings(settings) }
		}
		if err := save(updated.Store, settings); err != nil {
			if settingsRecoveryPending(err) {
				return nil
			}
			return fmt.Errorf("save account settings: %w", err)
		}
		return nil
	})
}

func applySettingToAccount(item *account, setting accountSetting) {
	if setting.Name != "" {
		item.Name = setting.Name
	}
	if setting.Plan != "" {
		item.Plan = setting.Plan
		item.planExplicit = true
		if buckets, ok := planBuckets[setting.Plan]; ok {
			item.FiveHourCredits = buckets.FiveHour
			item.WeeklyCredits = buckets.Weekly
		}
	}
	if setting.Disabled != nil {
		item.Disabled = *setting.Disabled
	}
	if setting.FiveHourCredits > 0 {
		item.FiveHourCredits = setting.FiveHourCredits
		item.fiveHourCreditsExplicit = true
	}
	if setting.WeeklyCredits > 0 {
		item.WeeklyCredits = setting.WeeklyCredits
		item.weeklyCreditsExplicit = true
	}
}
