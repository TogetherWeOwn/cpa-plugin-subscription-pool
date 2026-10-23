package opencodego

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const (
	usageEndpoint       = "https://opencode.ai/zen/go/v1/usage"
	maxUsageResponse    = 1 << 20
	defaultUsageTimeout = 15 * time.Second
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// usageWindow is one parsed zen/go usage window: real utilization and a
// real reset time, or nothing at all -- this package never fabricates one.
type usageWindow struct {
	Utilization  float64
	LimitReached bool
	ResetAt      time.Time
}

type usageSnapshot struct {
	FiveHour   usageWindow
	Weekly     usageWindow
	Monthly    usageWindow
	ObservedAt time.Time
}

type usageWireResponse struct {
	Usage usageWireUsage `json:"usage"`
}

type usageWireUsage struct {
	Rolling usageWireWindow `json:"rolling"`
	Weekly  usageWireWindow `json:"weekly"`
	Monthly usageWireWindow `json:"monthly"`
}

type usageWireWindow struct {
	Status   string      `json:"status"`
	Percent  json.Number `json:"percent"`
	ResetsAt string      `json:"resetsAt"`
}

func newUsageHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: usageTimeout(timeout)}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func usageTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultUsageTimeout
	}
	return timeout
}

// fetchUsage authenticates with a bounded, strictly-validated GET against
// the OpenCode Go usage endpoint. Every error path is generic: neither the
// API key nor the raw response body is ever included in a returned error.
func fetchUsage(ctx context.Context, client httpDoer, endpoint, apiKey string, now time.Time) (usageSnapshot, error) {
	if client == nil {
		return usageSnapshot{}, fmt.Errorf("usage client unavailable")
	}
	if err := validateUsageEndpoint(endpoint, endpoint != usageEndpoint); err != nil {
		return usageSnapshot{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("create usage request")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return usageSnapshot{}, err
		}
		return usageSnapshot{}, fmt.Errorf("usage request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return usageSnapshot{}, fmt.Errorf("usage endpoint returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUsageResponse+1))
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("read usage response")
	}
	if len(body) > maxUsageResponse {
		return usageSnapshot{}, fmt.Errorf("usage response exceeds maximum size")
	}
	snapshot, err := parseUsageResponse(body, now)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("invalid usage response: %w", err)
	}
	return snapshot, nil
}

func validateUsageEndpoint(endpoint string, allowTestEndpoint bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid usage endpoint")
	}
	if allowTestEndpoint && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost") {
		return nil
	}
	if parsed.Scheme != "https" || parsed.Host != "opencode.ai" || parsed.Path != "/zen/go/v1/usage" {
		return fmt.Errorf("invalid usage endpoint")
	}
	return nil
}

func parseUsageResponse(raw []byte, observedAt time.Time) (usageSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var response usageWireResponse
	if err := decoder.Decode(&response); err != nil {
		return usageSnapshot{}, fmt.Errorf("malformed JSON")
	}
	if err := securestore.EnsureJSONEOF(decoder); err != nil {
		return usageSnapshot{}, fmt.Errorf("malformed JSON")
	}

	observedAt = observedAt.UTC()
	fiveHour, err := parseUsageWindow(response.Usage.Rolling, observedAt)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("rolling window: %w", err)
	}
	weekly, err := parseUsageWindow(response.Usage.Weekly, observedAt)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("weekly window: %w", err)
	}
	monthly, err := parseUsageWindow(response.Usage.Monthly, observedAt)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("monthly window: %w", err)
	}
	return usageSnapshot{FiveHour: fiveHour, Weekly: weekly, Monthly: monthly, ObservedAt: observedAt}, nil
}

func parseUsageWindow(wire usageWireWindow, observedAt time.Time) (usageWindow, error) {
	status := strings.ToLower(strings.TrimSpace(wire.Status))
	if status != "ok" && status != "rate-limited" {
		return usageWindow{}, fmt.Errorf("unrecognized status %q", status)
	}
	if wire.Percent == "" {
		return usageWindow{}, fmt.Errorf("missing percent")
	}
	percent, err := wire.Percent.Float64()
	if err != nil || percent < 0 || percent > 100 {
		return usageWindow{}, fmt.Errorf("percent out of range")
	}

	utilization := percent / 100
	limitReached := percent == 100
	if status == "rate-limited" {
		utilization = 1
		limitReached = true
	}

	if strings.TrimSpace(wire.ResetsAt) == "" {
		return usageWindow{}, fmt.Errorf("missing resetsAt")
	}
	resetAt, err := time.Parse(time.RFC3339, wire.ResetsAt)
	if err != nil {
		return usageWindow{}, fmt.Errorf("unparseable resetsAt")
	}
	resetAt = resetAt.UTC()
	if resetAt.After(observedAt.Add(maxResetFuture)) {
		return usageWindow{}, fmt.Errorf("resetsAt too far in the future")
	}
	if !limitReached && !resetAt.After(observedAt) {
		return usageWindow{}, fmt.Errorf("resetsAt is not after observedAt for a non-exhausted window")
	}

	return usageWindow{Utilization: utilization, LimitReached: limitReached, ResetAt: resetAt}, nil
}
