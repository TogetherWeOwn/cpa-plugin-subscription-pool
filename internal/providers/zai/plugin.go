package zai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const pluginID = "zai-coding-plan"

// pluginVersion is stamped at build time via -ldflags -X main.pluginVersion in
// src/, mirrored here by coordinator.PluginVersion so the registered plugin
// metadata and this module's own status body never disagree on version.
var pluginVersion = "0.0.0-dev"

// runtimeState is this module's own runtime instance, driven by zaiModule
// (see module.go) as the coordinator's provider-module boundary.
var runtimeState = &pluginRuntime{}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string { return e.Code + ": " + e.Message }

// Coded implements providermodule.Coded so errors raised inside this package
// (e.g. by newSchedulerError) carry their wire code, message, and HTTP status
// across the coordinator boundary unchanged, rather than being downgraded to
// a generic error by a package-local type assertion on the other side.
func (e *envelopeError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.Code, Message: e.Message, Retryable: e.Retryable, HTTPStatus: e.HTTPStatus}
}

func newSchedulerError(code, message string) error {
	return &envelopeError{Code: code, Message: message, Retryable: false}
}

// newRateLimitedError is pick's answer when every remaining managed account is
// only in a short request-rate backoff. It is a retryable 429, not the
// zai_no_capacity 500: the router quarantines a lane on "no healthy managed
// ... capacity remains", and a few seconds of request-rate backoff must not
// evacuate the lane. The message must never contain that phrase.
func newRateLimitedError(wait time.Duration) error {
	seconds := max(1, int((wait+time.Second-1)/time.Second))
	return &envelopeError{
		Code:       "zai_rate_limited",
		Message:    fmt.Sprintf("managed Z.ai accounts are briefly request-rate limited; retry in %ds", seconds),
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

// usageHandle consumes a lossy best-effort usage observation. Persistence
// failures are surfaced in estimator integrity state, but the response remains
// successful because CPA discards usage-plugin RPC errors.
func usageHandle(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err != nil {
		return nil, fmt.Errorf("decode usage record")
	}
	_ = runtimeState.handleUsage(record)
	return okEnvelope(struct{}{})
}

// managementStatusBody is the redacted JSON served at the status route.
type managementStatusBody struct {
	Plugin          string                    `json:"plugin"`
	Status          string                    `json:"status"`
	Version         string                    `json:"version"`
	GeneratedAt     time.Time                 `json:"generated_at"`
	ValidationError string                    `json:"validation_error,omitempty"`
	Accounts        []managementAccountStatus `json:"accounts,omitempty"`
}

// managementCooldownStatus describes only the runtime's transient 429 cooldown,
// not quota exhaustion, suspension, or overall scheduler eligibility.
type managementCooldownStatus struct {
	Active bool       `json:"active"`
	Until  *time.Time `json:"until"`
	Reason string     `json:"reason"`
	Source string     `json:"source"`
}

type managementAccountStatus struct {
	Identity               string                   `json:"identity"`
	Cooldown               managementCooldownStatus `json:"cooldown"`
	Name                   string                   `json:"name"`
	KeySuffix              string                   `json:"key_suffix"`
	Plan                   string                   `json:"plan"`
	FiveHourUtilization    float64                  `json:"five_hour_utilization"`
	WeeklyUtilization      float64                  `json:"weekly_utilization"`
	FiveHourResetsAt       *time.Time               `json:"five_hour_resets_at"`
	WeeklyResetsAt         *time.Time               `json:"weekly_resets_at"`
	QuotaSource            string                   `json:"quota_source"`
	QuotaObservedAt        time.Time                `json:"quota_observed_at,omitempty"`
	QuotaAgeSeconds        int64                    `json:"quota_age_seconds"`
	QuotaStale             bool                     `json:"quota_stale"`
	QuotaError             string                   `json:"quota_error,omitempty"`
	FiveHourError          string                   `json:"five_hour_error,omitempty"`
	Offpeak                bool                     `json:"offpeak"`
	Health                 string                   `json:"health"`
	EstimatorCompleteSince time.Time                `json:"estimator_complete_since,omitempty"`
	DeliveryWarning        bool                     `json:"delivery_warning"`
	PersistenceWarning     bool                     `json:"persistence_warning"`
	UnknownModelWarning    bool                     `json:"unknown_model_warning"`
	HeuristicDedupWarning  bool                     `json:"heuristic_dedup_warning"`
	DedupMode              string                   `json:"dedup_mode"`
}

func managementCooldown(health accountHealthState, now time.Time) managementCooldownStatus {
	result := managementCooldownStatus{Source: "zai_runtime_health_v1"}
	if !health.ExhaustedUntil.After(now) {
		return result
	}
	result.Active = true
	result.Until = nullableTime(health.ExhaustedUntil)
	// Export a closed vocabulary, never arbitrary persisted text or upstream data.
	switch health.ExhaustedReason {
	case "retry-after header":
		result.Reason = "retry_after"
	case "x-ratelimit-reset header", "x-rate-limit-reset header", "ratelimit-reset header":
		result.Reason = "reset_header"
	case "rate-limit response body":
		result.Reason = "reset_body"
	case "authoritative quota reset":
		result.Reason = "quota_reset_fallback"
	case "conservative rate-limit cooldown":
		result.Reason = "configured_fallback"
	default:
		result.Reason = "upstream_rate_limit"
	}
	return result
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}
