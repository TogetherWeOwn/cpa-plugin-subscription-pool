package zai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

const quotaFixtureKey = "test-only-quota-key"

var quotaObservedAt = time.Date(2026, time.September, 8, 5, 0, 0, 0, time.UTC)

func TestParseQuotaResponseMapsProLimitsIndependentlyOfOrder(t *testing.T) {
	raw := quotaFixture("pro", []string{
		quotaLimitFixture(6, 1, 60_000, 10_800, 49_200, quotaObservedAt.Add(7*24*time.Hour).UnixMilli()),
		quotaLimitFixture(3, 5, 12_000, 5_040, 6_960, quotaObservedAt.Add(5*time.Hour).UnixMilli()),
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan != "pro" || snapshot.FiveHour.ConsumedMicrocredits != 5_040*creditScale || snapshot.Weekly.BucketMicrocredits != 60_000*creditScale {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !snapshot.FiveHour.ResetsAt.Equal(quotaObservedAt.Add(5*time.Hour)) || !snapshot.Weekly.ResetsAt.Equal(quotaObservedAt.Add(7*24*time.Hour)) {
		t.Fatalf("reset conversion failed: %#v", snapshot)
	}
}

// TestParseQuotaResponseAcceptsFloatFormattedResetTime is a regression test
// for TOG-2473: Z.ai's quota endpoint has been observed serializing
// nextResetTime (and other CREDIT_LIMIT numbers) with a trailing ".0" even
// though the value is a whole-number epoch-millisecond timestamp. A strict
// int64 parse of "1789347583607.0" fails, which previously rejected a valid
// weekly quota window and stuck the provider on "estimate".
func TestParseQuotaResponseAcceptsFloatFormattedResetTime(t *testing.T) {
	raw := quotaFixture("pro", []string{
		`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000.0,"currentValue":0.0,"remaining":12000.0,"nextResetTime":` + stringNumber(quotaObservedAt.Add(5*time.Hour).UnixMilli()) + `.0}`,
		`{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":60000.0,"currentValue":54728.0,"remaining":5272.0,"nextResetTime":` + stringNumber(quotaObservedAt.Add(7*24*time.Hour).UnixMilli()) + `.0}`,
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatalf("float-formatted numbers rejected: %v", err)
	}
	if snapshot.Weekly.ConsumedMicrocredits != 54_728*creditScale || snapshot.Weekly.BucketMicrocredits != 60_000*creditScale {
		t.Fatalf("weekly window = %#v", snapshot.Weekly)
	}
	if !snapshot.Weekly.ResetsAt.Equal(quotaObservedAt.Add(7 * 24 * time.Hour)) {
		t.Fatalf("weekly reset = %v", snapshot.Weekly.ResetsAt)
	}
}

// TestParseQuotaResponseAcceptsNullResetOnUnusedWindow is a regression test
// for TOG-2490: Z.ai reports nextResetTime: null on a CREDIT_LIMIT window
// with zero usage (no reset scheduled yet), most commonly right after a
// rollover with no traffic since. That must parse as utilization 0 with no
// pending reset rather than discarding the whole response — including the
// unrelated, valid weekly window — as it did when treated as a hard error.
func TestParseQuotaResponseAcceptsNullResetOnUnusedWindow(t *testing.T) {
	raw := quotaFixture("max", []string{
		`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":28000,"currentValue":0,"remaining":28000,"nextResetTime":null}`,
		quotaLimitFixture(6, 1, 140_000, 127_400, 12_600, quotaObservedAt.Add(38*time.Hour+9*time.Minute+5*time.Second).UnixMilli()),
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatalf("null nextResetTime on unused window rejected: %v", err)
	}
	if snapshot.FiveHour.ConsumedMicrocredits != 0 || !snapshot.FiveHour.ResetsAt.IsZero() {
		t.Fatalf("five-hour window = %#v", snapshot.FiveHour)
	}
	if snapshot.Weekly.ConsumedMicrocredits != 127_400*creditScale || snapshot.Weekly.BucketMicrocredits != 140_000*creditScale {
		t.Fatalf("weekly window = %#v", snapshot.Weekly)
	}
	if !snapshot.Weekly.ResetsAt.Equal(quotaObservedAt.Add(38*time.Hour + 9*time.Minute + 5*time.Second)) {
		t.Fatalf("weekly reset = %v", snapshot.Weekly.ResetsAt)
	}
}

// TestParseQuotaResponseRejectsNullResetOnConsumedWindow ensures the
// null-nextResetTime allowance from TOG-2490 is narrowly scoped: a window
// that has actually been consumed must still carry a valid reset time. This
// is a five-hour-window failure, so per TOG-2497 it no longer discards the
// response outright — it surfaces as FiveHourError alongside a good weekly.
func TestParseQuotaResponseRejectsNullResetOnConsumedWindow(t *testing.T) {
	raw := quotaFixture("max", []string{
		`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":28000,"currentValue":1,"remaining":27999,"nextResetTime":null}`,
		quotaLimitFixture(6, 1, 140_000, 127_400, 12_600, quotaObservedAt.Add(24*time.Hour).UnixMilli()),
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatalf("weekly-governed response rejected: %v", err)
	}
	if snapshot.FiveHourError == "" || snapshot.FiveHour != (quotaWindow{}) {
		t.Fatalf("five-hour failure not isolated: %#v", snapshot)
	}
	if snapshot.Weekly.ConsumedMicrocredits != 127_400*creditScale {
		t.Fatalf("weekly window discarded: %#v", snapshot.Weekly)
	}
}

// TestParseQuotaResponseRejectsNonWholeFloatAndOutOfRangeNumbers covers two
// distinct ways a five-hour nextResetTime can fail to parse. Per TOG-2497
// neither is fatal to the response as a whole: the weekly (governing) window
// still parses and the failure is recorded on FiveHourError instead. (A
// literal "NaN" is not valid JSON syntax at all, so that case belongs with
// the whole-document malformed-JSON failures instead — see
// TestParseQuotaResponseRejectsWhenWeeklyUnusable.)
func TestParseQuotaResponseRejectsNonWholeFloatAndOutOfRangeNumbers(t *testing.T) {
	validWeek := quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli())
	tests := []struct {
		name string
		raw  string
	}{
		{name: "fractional nextResetTime", raw: quotaFixture("pro", []string{
			`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000,"currentValue":0,"remaining":12000,"nextResetTime":` + stringNumber(quotaObservedAt.Add(time.Hour).UnixMilli()) + `.5}`,
			validWeek,
		})},
		{name: "float exceeds int64 range", raw: quotaFixture("pro", []string{
			`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000,"currentValue":0,"remaining":12000,"nextResetTime":1e300}`,
			validWeek,
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, err := parseQuotaResponse([]byte(tt.raw), quotaObservedAt)
			if err != nil {
				t.Fatalf("weekly-governed response rejected: %v", err)
			}
			if snapshot.FiveHourError == "" || snapshot.FiveHour != (quotaWindow{}) {
				t.Fatalf("five-hour failure not isolated: %#v", snapshot)
			}
			if snapshot.Weekly.ConsumedMicrocredits != 2*creditScale {
				t.Fatalf("weekly window discarded: %#v", snapshot.Weekly)
			}
		})
	}
}

func TestParseQuotaResponseAcceptsPlanName(t *testing.T) {
	raw := strings.Replace(quotaFixture("", []string{
		quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli()),
		quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli()),
	}), `"level":""`, `"level":"","planName":"Pro Plan"`, 1)
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil || snapshot.Plan != "pro" {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
}

// TestParseQuotaResponseRejectsWhenWeeklyUnusable covers failures on the
// governing weekly window (TOG-2497): these remain fatal to the whole
// response — the caller falls back to "estimate" exactly as before this fix,
// since there is no good weekly number to report as authoritative.
func TestParseQuotaResponseRejectsWhenWeeklyUnusable(t *testing.T) {
	validFive := quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli())
	validWeek := quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli())
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing weekly", raw: quotaFixture("pro", []string{validFive})},
		{name: "duplicate weekly", raw: quotaFixture("pro", []string{validFive, validWeek, validWeek})},
		{name: "invalid reset", raw: quotaFixture("pro", []string{validFive, strings.Replace(validWeek, stringNumber(quotaObservedAt.Add(24*time.Hour).UnixMilli()), "1", 1)})},
		{name: "reset before observation", raw: quotaFixture("pro", []string{validFive, strings.Replace(validWeek, stringNumber(quotaObservedAt.Add(24*time.Hour).UnixMilli()), stringNumber(quotaObservedAt.Add(-time.Millisecond).UnixMilli()), 1)})},
		{name: "malformed", raw: `{`},
		{name: "NaN literal is invalid JSON", raw: quotaFixture("pro", []string{
			`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000,"currentValue":0,"remaining":12000,"nextResetTime":NaN}`,
			validWeek,
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseQuotaResponse([]byte(tt.raw), quotaObservedAt); err == nil {
				t.Fatal("invalid quota payload succeeded")
			}
		})
	}
}

// TestParseQuotaResponseIsolatesFiveHourFailures is the TOG-2497 regression:
// a bad five-hour window must never discard a good weekly window. Each case
// asserts the weekly window survives intact and the five-hour failure is
// reported via FiveHourError rather than silently zero-filled.
func TestParseQuotaResponseIsolatesFiveHourFailures(t *testing.T) {
	validFive := quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli())
	validWeek := quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli())
	tests := []struct {
		name string
		raw  string
	}{
		{name: "duplicate five hour", raw: quotaFixture("pro", []string{validFive, validFive, validWeek})},
		{name: "fractional current value", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"currentValue":1`, `"currentValue":1.5`, 1), validWeek})},
		{name: "negative usage", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"usage":12000`, `"usage":-1`, 1), validWeek})},
		{name: "current exceeds usage", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"currentValue":1`, `"currentValue":12001`, 1), validWeek})},
		{name: "reset at observation", raw: quotaFixture("pro", []string{strings.Replace(validFive, stringNumber(quotaObservedAt.Add(time.Hour).UnixMilli()), stringNumber(quotaObservedAt.UnixMilli()), 1), validWeek})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, err := parseQuotaResponse([]byte(tt.raw), quotaObservedAt)
			if err != nil {
				t.Fatalf("weekly-governed response rejected: %v", err)
			}
			if snapshot.FiveHourError == "" || snapshot.FiveHour != (quotaWindow{}) {
				t.Fatalf("five-hour failure not isolated: %#v", snapshot)
			}
			if snapshot.Weekly.ConsumedMicrocredits != 2*creditScale || snapshot.Weekly.BucketMicrocredits != 60_000*creditScale {
				t.Fatalf("weekly window discarded: %#v", snapshot.Weekly)
			}
		})
	}
}

// TestParseQuotaResponseToleratesUnknownPlanWithGoodWindows: an unrecognized
// plan label is account metadata, not part of either window's data, and must
// not discard a response whose windows both parsed fine (TOG-2497).
func TestParseQuotaResponseToleratesUnknownPlanWithGoodWindows(t *testing.T) {
	validFive := quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli())
	validWeek := quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli())
	raw := quotaFixture("enterprise", []string{validFive, validWeek})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatalf("unknown plan rejected: %v", err)
	}
	if snapshot.Plan != "" {
		t.Fatalf("plan = %q, want empty for unrecognized label", snapshot.Plan)
	}
	if snapshot.FiveHourError != "" || snapshot.Weekly.ConsumedMicrocredits != 2*creditScale {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

// TestParseQuotaResponseTOG2490PayloadShapeGovernsOnWeeklyAlone is the
// TOG-2497 acceptance test: the exact TOG-2490 payload shape (unit 3
// five-hour window reporting 0% usage with nextResetTime: null, unit 6
// weekly window at 91% with a concrete nextResetTime) must, once the
// five-hour side is made unparseable by any means, still surface the weekly
// number as quota_api-eligible with a named five-hour diagnostic — not
// silently zero-filled, and not discarded wholesale.
func TestParseQuotaResponseTOG2490PayloadShapeGovernsOnWeeklyAlone(t *testing.T) {
	const weeklyResetMillis = 1789445345983
	raw := quotaFixture("max", []string{
		`{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":28000,"currentValue":1,"remaining":27999,"nextResetTime":null}`,
		`{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":100000,"currentValue":91000,"remaining":9000,"nextResetTime":` + stringNumber(weeklyResetMillis) + `}`,
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatalf("weekly-governed response rejected: %v", err)
	}
	if snapshot.FiveHourError == "" {
		t.Fatalf("expected a named five-hour diagnostic, got none: %#v", snapshot)
	}
	if snapshot.FiveHour != (quotaWindow{}) {
		t.Fatalf("five-hour window was zero-filled instead of left absent: %#v", snapshot.FiveHour)
	}
	weeklyUtilization := utilization(snapshot.Weekly)
	if math.Abs(weeklyUtilization-0.91) > 0.0001 {
		t.Fatalf("weekly_utilization = %v, want ~0.91", weeklyUtilization)
	}
	wantResetsAt := time.Date(2026, time.September, 15, 4, 9, 5, 983_000_000, time.UTC)
	if !snapshot.Weekly.ResetsAt.Equal(wantResetsAt) {
		t.Fatalf("weekly_resets_at = %v, want %v", snapshot.Weekly.ResetsAt, wantResetsAt)
	}
}

func TestFetchQuotaUsesBearerWithoutLeakingSecret(t *testing.T) {
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+quotaFixtureKey {
			t.Fatalf("authorization = %q", got)
		}
		return quotaHTTPResponse(http.StatusOK, quotaFixture("max", []string{
			quotaLimitFixture(3, 5, 28_000, 3_341, 24_659, quotaObservedAt.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 140_000, 25_224, 114_776, quotaObservedAt.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	snapshot, err := fetchQuota(context.Background(), client, quotaEndpoint, quotaFixtureKey, quotaObservedAt)
	if err != nil || snapshot.Plan != "max" {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), quotaFixtureKey) || strings.Contains(errString(err), quotaFixtureKey) {
		t.Fatalf("quota result leaked key: %s %v", serialized, err)
	}
}

func TestFetchQuotaErrorsAreRedactedAndBounded(t *testing.T) {
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failed with " + quotaFixtureKey)
	})
	_, err := fetchQuota(context.Background(), client, quotaEndpoint, quotaFixtureKey, quotaObservedAt)
	if err == nil || strings.Contains(err.Error(), quotaFixtureKey) {
		t.Fatalf("error = %v", err)
	}
}

func TestFetchQuotaSelectsFallbackOnNon200MalformedAndTimeout(t *testing.T) {
	tests := []struct {
		name   string
		client httpDoer
	}{
		{name: "non-200", client: roundTripDoer(func(*http.Request) (*http.Response, error) { return quotaHTTPResponse(503, quotaFixtureKey), nil })},
		{name: "malformed", client: roundTripDoer(func(*http.Request) (*http.Response, error) { return quotaHTTPResponse(200, `{`), nil })},
		{name: "timeout", client: roundTripDoer(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			if _, err := fetchQuota(ctx, tt.client, quotaEndpoint, quotaFixtureKey, quotaObservedAt); err == nil || strings.Contains(err.Error(), quotaFixtureKey) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestQuotaHTTPClientRejectsRedirectsAndAmbientProxy(t *testing.T) {
	client := newQuotaHTTPClient(0)
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, ambient proxy must be disabled", client.Transport)
	}
}

func TestQuotaHTTPClientHonorsConfiguredTimeoutAboveDefault(t *testing.T) {
	client := newQuotaHTTPClient(30 * time.Second)
	if client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %s, want 30s", client.Timeout)
	}
}

func TestJitterAndBackoffAreDeterministicAndBounded(t *testing.T) {
	for _, base := range []time.Duration{time.Minute, 2 * time.Minute} {
		first := jitteredPollInterval(base, "account-a", 7)
		second := jitteredPollInterval(base, "account-a", 7)
		if first != second || first < base/2 || first > maxPollInterval(base) {
			t.Fatalf("jitter = %v/%v for base %v", first, second, base)
		}
	}
	if pollBackoff(time.Minute, 1) != time.Minute || pollBackoff(time.Minute, 6) != 15*time.Minute {
		t.Fatalf("unexpected backoff")
	}
}

func quotaFixture(level string, limits []string) string {
	return `{"code":200,"success":true,"data":{"limits":[` + strings.Join(limits, ",") + `],"level":"` + level + `"}}`
}

func quotaLimitFixture(unit, number, usage, current, remaining, reset int64) string {
	return `{"type":"CREDIT_LIMIT","unit":` + stringNumber(unit) + `,"number":` + stringNumber(number) + `,"usage":` + stringNumber(usage) + `,"currentValue":` + stringNumber(current) + `,"remaining":` + stringNumber(remaining) + `,"nextResetTime":` + stringNumber(reset) + `}`
}

func stringNumber(value int64) string { return fmt.Sprintf("%d", value) }

func quotaHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type roundTripDoer func(*http.Request) (*http.Response, error)

func (function roundTripDoer) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The quota poller sends the account's API key as a bearer token to whatever
// endpoint it is given. Anything other than the production Z.ai endpoint or a
// plain-HTTP loopback test server must be refused before a request is built.
func TestFetchQuotaRefusesToSendKeyToUnapprovedEndpoint(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	rejected := []string{
		"https://evil.example/api/monitor/usage/quota/limit",
		"https://api.z.ai.evil.example/api/monitor/usage/quota/limit",
		"http://api.z.ai/api/monitor/usage/quota/limit",
		"https://api.z.ai:8443/api/monitor/usage/quota/limit",
		"https://user:pass@api.z.ai/api/monitor/usage/quota/limit",
		"https://api.z.ai/api/monitor/usage/quota/limit?next=https://evil.example",
		"https://api.z.ai/api/monitor/usage/quota/limit#fragment",
		"https://api.z.ai/api/other",
		"https://127.0.0.1/api/monitor/usage/quota/limit",
		"http://localhost.evil.example/api/monitor/usage/quota/limit",
		"http://10.0.0.1/api/monitor/usage/quota/limit",
		"http://127.0.0.1:8080/quota?key=1",
		"://bad",
	}
	for _, endpoint := range rejected {
		t.Run(endpoint, func(t *testing.T) {
			client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("request sent to %s with Authorization %q", request.URL, request.Header.Get("Authorization"))
				return nil, nil
			})
			_, err := fetchQuota(context.Background(), client, endpoint, quotaFixtureKey, now)
			if err == nil || err.Error() != "invalid quota endpoint" {
				t.Fatalf("err = %v, want invalid quota endpoint", err)
			}
		})
	}

	for _, endpoint := range []string{quotaEndpoint, "http://127.0.0.1:8080/quota", "http://localhost:8080/quota"} {
		t.Run(endpoint, func(t *testing.T) {
			var sent *http.Request
			client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
				sent = request
				return quotaHTTPResponse(http.StatusBadGateway, ""), nil
			})
			_, _ = fetchQuota(context.Background(), client, endpoint, quotaFixtureKey, now)
			if sent == nil || sent.URL.String() != endpoint || sent.Header.Get("Authorization") != "Bearer "+quotaFixtureKey {
				t.Fatalf("approved endpoint not requested correctly: %#v", sent)
			}
		})
	}
}
