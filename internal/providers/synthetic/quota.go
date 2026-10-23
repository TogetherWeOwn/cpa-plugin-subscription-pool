package synthetic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxQuotaResponse    = 1 << 20
	defaultQuotaTimeout = 15 * time.Second
	quotaHeader         = "X-Synthetic-Quotas"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// quotaReading is one parsed weekly-credit observation. PercentRemaining is
// 0-100 as Synthetic reports it. Every field is optional on the wire; the
// parser never fabricates a value that was not reported.
type quotaReading struct {
	PercentRemaining *float64
	NextRegenAt      time.Time
	NextRegenPercent *float64
	Shape            string
}

func (q quotaReading) utilization() *float64 {
	if q.PercentRemaining == nil {
		return nil
	}
	used := (100 - clamp(*q.PercentRemaining, 0, 100)) / 100
	return &used
}

// quotaWire covers every shape Synthetic has published for GET /v2/quotas
// and the x-synthetic-quotas response header:
//   - current plans: planWeeklyLimit {percentRemaining, nextRegenAt, nextRegenPercent}
//   - legacy token plans: weeklyTokenLimit {percentRemaining, nextRegenAt, ...credits}
//   - legacy request packs: rollingFiveHourLimit {remaining, max, nextTickAt, tickPercent}
//   - the documented example: subscription {limit, requests, renewsAt}
type quotaWire struct {
	PlanWeeklyLimit *struct {
		PercentRemaining *float64 `json:"percentRemaining"`
		NextRegenAt      string   `json:"nextRegenAt"`
		NextRegenPercent *float64 `json:"nextRegenPercent"`
	} `json:"planWeeklyLimit"`
	WeeklyTokenLimit *struct {
		PercentRemaining *float64 `json:"percentRemaining"`
		NextRegenAt      string   `json:"nextRegenAt"`
	} `json:"weeklyTokenLimit"`
	RollingFiveHourLimit *struct {
		Remaining   *float64 `json:"remaining"`
		Max         *float64 `json:"max"`
		NextTickAt  string   `json:"nextTickAt"`
		TickPercent *float64 `json:"tickPercent"`
	} `json:"rollingFiveHourLimit"`
	Subscription *struct {
		Limit    *float64 `json:"limit"`
		Requests *float64 `json:"requests"`
		RenewsAt string   `json:"renewsAt"`
	} `json:"subscription"`
}

var errNoQuotaShape = errors.New("quota payload has no recognized limit")

func parseQuota(raw []byte) (quotaReading, error) {
	var wire quotaWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return quotaReading{}, fmt.Errorf("decode quota payload")
	}
	switch {
	case wire.PlanWeeklyLimit != nil && wire.PlanWeeklyLimit.PercentRemaining != nil:
		w := wire.PlanWeeklyLimit
		return quotaReading{PercentRemaining: floatPtr(*w.PercentRemaining), NextRegenAt: parseTime(w.NextRegenAt), NextRegenPercent: copyFloat(w.NextRegenPercent), Shape: "planWeeklyLimit"}, nil
	case wire.WeeklyTokenLimit != nil && wire.WeeklyTokenLimit.PercentRemaining != nil:
		w := wire.WeeklyTokenLimit
		return quotaReading{PercentRemaining: floatPtr(*w.PercentRemaining), NextRegenAt: parseTime(w.NextRegenAt), Shape: "weeklyTokenLimit"}, nil
	case wire.RollingFiveHourLimit != nil && wire.RollingFiveHourLimit.Remaining != nil && wire.RollingFiveHourLimit.Max != nil && *wire.RollingFiveHourLimit.Max > 0:
		w := wire.RollingFiveHourLimit
		reading := quotaReading{PercentRemaining: floatPtr(*w.Remaining / *w.Max * 100), NextRegenAt: parseTime(w.NextTickAt), Shape: "rollingFiveHourLimit"}
		if w.TickPercent != nil {
			reading.NextRegenPercent = floatPtr(*w.TickPercent * 100)
		}
		return reading, nil
	case wire.Subscription != nil && wire.Subscription.Limit != nil && wire.Subscription.Requests != nil && *wire.Subscription.Limit > 0:
		w := wire.Subscription
		return quotaReading{PercentRemaining: floatPtr((*w.Limit - *w.Requests) / *w.Limit * 100), NextRegenAt: parseTime(w.RenewsAt), Shape: "subscription"}, nil
	default:
		return quotaReading{}, errNoQuotaShape
	}
}

// quotaFromHeaders reads the x-synthetic-quotas header Synthetic attaches to
// inference responses. It returns ok=false when the header is absent or
// unparseable.
func quotaFromHeaders(headers http.Header) (quotaReading, bool) {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), quotaHeader) {
			continue
		}
		for _, value := range values {
			if reading, err := parseQuota([]byte(value)); err == nil {
				return reading, true
			}
		}
	}
	return quotaReading{}, false
}

func newQuotaHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: defaultQuotaTimeout}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// fetchQuota performs one bounded GET of the quota endpoint. Errors are
// generic: neither the API key nor the response body is ever included.
func fetchQuota(ctx context.Context, client httpDoer, endpoint, apiKey string) (quotaReading, error) {
	if client == nil || endpoint == "" || apiKey == "" {
		return quotaReading{}, fmt.Errorf("quota poll not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultQuotaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaReading{}, fmt.Errorf("build quota request")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return quotaReading{}, fmt.Errorf("quota request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaResponse+1))
	if err != nil {
		return quotaReading{}, fmt.Errorf("read quota response")
	}
	if len(body) > maxQuotaResponse {
		return quotaReading{}, fmt.Errorf("quota response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return quotaReading{}, fmt.Errorf("quota endpoint returned HTTP %d", resp.StatusCode)
	}
	reading, err := parseQuota(body)
	if err != nil {
		return quotaReading{}, err
	}
	return reading, nil
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func floatPtr(value float64) *float64 { return &value }

func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return floatPtr(*value)
}

func clamp(value, lo, hi float64) float64 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
