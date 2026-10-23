package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const (
	quotaEndpoint       = "https://api.z.ai/api/monitor/usage/quota/limit"
	maxQuotaResponse    = 1 << 20
	defaultQuotaTimeout = 15 * time.Second
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type quotaWindow struct {
	ConsumedMicrocredits int64
	BucketMicrocredits   int64
	ResetsAt             time.Time
}

// quotaSnapshot's windows are parsed independently. Weekly is the governing
// number (TOG-2497): parseQuotaResponse still returns a hard error when
// Weekly fails to parse, so a Weekly failure keeps today's all-or-nothing
// fallback to "estimate" and never needs its own error field here. A
// FiveHour failure is not fatal — FiveHourError is non-empty exactly when
// FiveHour could not be parsed, in which case FiveHour stays zero-valued
// rather than being filled with a number that would read as "unused" or
// "full capacity available."
type quotaSnapshot struct {
	Plan          string
	FiveHour      quotaWindow
	FiveHourError string
	Weekly        quotaWindow
	ObservedAt    time.Time
}

type quotaWireResponse struct {
	Code    json.Number   `json:"code"`
	Success *bool         `json:"success"`
	Data    quotaWireData `json:"data"`
}

type quotaWireData struct {
	Level    string           `json:"level"`
	PlanName string           `json:"planName"`
	Limits   []quotaWireLimit `json:"limits"`
}

type quotaWireLimit struct {
	Type          string      `json:"type"`
	Unit          json.Number `json:"unit"`
	Number        json.Number `json:"number"`
	Usage         json.Number `json:"usage"`
	CurrentValue  json.Number `json:"currentValue"`
	Remaining     json.Number `json:"remaining"`
	NextResetTime json.Number `json:"nextResetTime"`
}

func newQuotaHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: quotaTimeout(timeout)}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func fetchQuota(ctx context.Context, client httpDoer, endpoint, key string, now time.Time) (quotaSnapshot, error) {
	if client == nil {
		return quotaSnapshot{}, fmt.Errorf("quota client unavailable")
	}
	if err := validateQuotaEndpoint(endpoint, endpoint != quotaEndpoint); err != nil {
		return quotaSnapshot{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("create quota request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return quotaSnapshot{}, err
		}
		return quotaSnapshot{}, fmt.Errorf("quota request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return quotaSnapshot{}, fmt.Errorf("quota endpoint returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxQuotaResponse+1))
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("read quota response")
	}
	if len(body) > maxQuotaResponse {
		return quotaSnapshot{}, fmt.Errorf("quota response exceeds maximum size")
	}
	snapshot, err := parseQuotaResponse(body, now)
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("invalid quota response: %w", err)
	}
	return snapshot, nil
}

func validateQuotaEndpoint(endpoint string, allowTestEndpoint bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid quota endpoint")
	}
	if allowTestEndpoint && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost") {
		return nil
	}
	if parsed.Scheme != "https" || parsed.Host != "api.z.ai" || parsed.Path != "/api/monitor/usage/quota/limit" {
		return fmt.Errorf("invalid quota endpoint")
	}
	return nil
}

func parseQuotaResponse(raw []byte, observedAt time.Time) (quotaSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var response quotaWireResponse
	if err := decoder.Decode(&response); err != nil {
		return quotaSnapshot{}, fmt.Errorf("malformed JSON")
	}
	if err := securestore.EnsureJSONEOF(decoder); err != nil {
		return quotaSnapshot{}, fmt.Errorf("malformed JSON")
	}
	if response.Success != nil && !*response.Success {
		return quotaSnapshot{}, fmt.Errorf("upstream marked response unsuccessful")
	}
	if response.Code != "" {
		code, err := response.Code.Int64()
		if err != nil || code != http.StatusOK {
			return quotaSnapshot{}, fmt.Errorf("invalid response code")
		}
	}

	var fiveHour, weekly *quotaWindow
	var fiveHourErr, weeklyErr string
	var unmatchedErrs []string
	for i := range response.Data.Limits {
		limit := response.Data.Limits[i]
		if strings.ToUpper(strings.TrimSpace(limit.Type)) != "CREDIT_LIMIT" {
			continue
		}
		unit, errUnit := strictInt64(limit.Unit)
		number, errNumber := strictInt64(limit.Number)
		if errUnit != nil || errNumber != nil {
			// We cannot tell which window this limit was meant for, so the
			// failure cannot be attributed to either one specifically.
			unmatchedErrs = append(unmatchedErrs, "limit has invalid unit or number")
			continue
		}
		var target **quotaWindow
		var targetErr *string
		switch {
		case unit == 3 && number == 5:
			target, targetErr = &fiveHour, &fiveHourErr
		case unit == 6 && number == 1:
			target, targetErr = &weekly, &weeklyErr
		default:
			continue
		}
		if *target != nil {
			// Ambiguous which of the duplicates is correct: fail closed on
			// this window rather than trusting either value.
			*target = nil
			*targetErr = "duplicate quota limit"
			continue
		}
		window, err := parseQuotaWindow(limit, observedAt)
		if err != nil {
			*targetErr = err.Error()
			continue
		}
		*target = &window
	}
	// The weekly window is governing (TOG-2497): if it never parsed, the
	// whole response is unusable and the caller falls back to estimate, same
	// as before this fix. A five-hour failure alone must not do the same —
	// it is recorded on the snapshot instead of discarding a good weekly.
	if weekly == nil {
		reason := weeklyErr
		if reason == "" {
			reason = "missing required weekly quota limit"
		}
		return quotaSnapshot{}, fmt.Errorf("%s", reason)
	}
	// An unrecognized plan label no longer discards a good weekly window: it
	// is metadata about the account, not part of either window's own data,
	// and downstream plan-sync (syncPlanFromUpstream) already tolerates an
	// empty Plan by leaving the configured plan/buckets untouched.
	plan := normalizeUpstreamPlan(response.Data.Level)
	if plan == "" {
		plan = normalizeUpstreamPlan(response.Data.PlanName)
	}
	snapshot := quotaSnapshot{Plan: plan, Weekly: *weekly, ObservedAt: observedAt.UTC()}
	if fiveHour != nil {
		snapshot.FiveHour = *fiveHour
	} else {
		reason := fiveHourErr
		if reason == "" && len(unmatchedErrs) > 0 {
			reason = strings.Join(unmatchedErrs, "; ")
		}
		if reason == "" {
			reason = "missing required five-hour quota limit"
		}
		snapshot.FiveHourError = reason
	}
	return snapshot, nil
}

func parseQuotaWindow(limit quotaWireLimit, observedAt time.Time) (quotaWindow, error) {
	usage, err := strictNonnegativeInt64(limit.Usage)
	if err != nil || usage <= 0 {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid usage")
	}
	current, err := strictNonnegativeInt64(limit.CurrentValue)
	if err != nil || current > usage {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid currentValue")
	}
	if limit.Remaining != "" {
		if _, err = strictNonnegativeInt64(limit.Remaining); err != nil {
			return quotaWindow{}, fmt.Errorf("quota limit has invalid remaining")
		}
	}
	if usage > int64(^uint64(0)>>1)/creditScale || current > int64(^uint64(0)>>1)/creditScale {
		return quotaWindow{}, fmt.Errorf("quota credits exceed supported range")
	}
	// Z.ai reports nextResetTime: null on a window with no consumption yet
	// (e.g. right after a rollover with no traffic since) — that means
	// "unused, no reset pending", not a malformed response. A reset time
	// that is present but invalid, or absent on a window that has actually
	// been consumed, is still rejected.
	var reset time.Time
	if limit.NextResetTime == "" {
		if current != 0 {
			return quotaWindow{}, fmt.Errorf("quota limit has invalid nextResetTime")
		}
	} else {
		resetMilliseconds, errReset := strictNonnegativeInt64(limit.NextResetTime)
		if errReset != nil || resetMilliseconds <= 0 {
			return quotaWindow{}, fmt.Errorf("quota limit has invalid nextResetTime")
		}
		reset = time.UnixMilli(resetMilliseconds).UTC()
		if reset.Year() < 2000 || reset.Year() > 2200 || !reset.After(observedAt.UTC()) {
			return quotaWindow{}, fmt.Errorf("quota limit has invalid nextResetTime")
		}
	}
	return quotaWindow{ConsumedMicrocredits: current * creditScale, BucketMicrocredits: usage * creditScale, ResetsAt: reset}, nil
}

// maxSafeFloatInt bounds the float64 fallback in strictInt64 well below the
// point where converting to int64 becomes lossy or undefined, leaving ample
// room for any real quota usage or epoch-millisecond timestamp.
const maxSafeFloatInt = float64(1 << 62)

func strictInt64(number json.Number) (int64, error) {
	if number == "" {
		return 0, fmt.Errorf("missing number")
	}
	if value, err := number.Int64(); err == nil {
		return value, nil
	}
	// Z.ai's quota endpoint sometimes serializes whole numbers with a
	// trailing ".0" (e.g. nextResetTime); json.Number.Int64() rejects any
	// literal containing a decimal point, so fall back to a float parse and
	// accept it only if it is an exact, in-range whole number.
	value, err := number.Float64()
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value ||
		value < -maxSafeFloatInt || value > maxSafeFloatInt {
		return 0, fmt.Errorf("invalid number")
	}
	return int64(value), nil
}

func strictNonnegativeInt64(number json.Number) (int64, error) {
	value, err := strictInt64(number)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid number")
	}
	return value, nil
}

func normalizeUpstreamPlan(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.TrimSpace(strings.TrimSuffix(value, "plan"))
	switch {
	case strings.Contains(value, "lite"):
		return "lite"
	case strings.Contains(value, "pro"):
		return "pro"
	case strings.Contains(value, "max"):
		return "max"
	default:
		return ""
	}
}
