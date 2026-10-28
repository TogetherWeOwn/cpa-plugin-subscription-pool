package opencodego

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

func configuredModule(t *testing.T, threshold int) *Module {
	t.Helper()
	module := NewModule()
	raw := json.RawMessage(`{"threshold-percent":` + jsonNumber(threshold) + `,"accounts":[{"name":"go-a","auth-ids":["go-a-1"]},{"name":"go-b","auth-ids":["go-b-1"]}]}`)
	if err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, raw); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	return module
}

func jsonNumber(value int) string { return strconv.Itoa(value) }

func TestModuleImplementsCurrentInterface(t *testing.T) {
	var module providermodule.Module = NewModule()
	if module.ID() != providerID {
		t.Fatalf("ID() = %q, want %q", module.ID(), providerID)
	}
}

func TestRecognizeOpenCodeButNeverClaimsUnqualifiedGLM(t *testing.T) {
	module := configuredModule(t, 97)
	if !module.Recognize(pluginapi.SchedulerAuthCandidate{Provider: providerID}) {
		t.Fatal("Recognize() = false for OpenCode Go candidate")
	}
	for _, candidate := range []pluginapi.SchedulerAuthCandidate{
		{Provider: "zai", Attributes: map[string]string{"model": "glm-5.3"}},
		{Attributes: map[string]string{"provider_key": "zai", "model_id": "glm-5.3"}},
		{Attributes: map[string]string{"compat_name": "zai-coding-plan", "requested_model": "GLM_5"}},
	} {
		if module.Recognize(candidate) {
			t.Fatalf("Recognize(%#v) = true, want false for non-OpenCode GLM", candidate)
		}
	}
}

func TestOpenCodeProviderIdentityAllowsObservedGLMFamily(t *testing.T) {
	module := configuredModule(t, 97)
	candidate := pluginapi.SchedulerAuthCandidate{ID: "go-a-1", Provider: providerID, Attributes: map[string]string{"model": "opencode-go/glm-5.3"}}
	if !module.Recognize(candidate) {
		t.Fatal("Recognize() = false for provider-qualified OpenCode Go GLM candidate")
	}
	if module.Recognize(pluginapi.SchedulerAuthCandidate{ID: "go-a-1", Provider: "zai", Attributes: map[string]string{"model": "glm-5.3"}}) {
		t.Fatal("Recognize() claimed a managed auth ID when the candidate explicitly belongs to Z.ai")
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Provider: providerID, Model: "glm-5.3", Candidates: []pluginapi.SchedulerAuthCandidate{candidate}})
	if err != nil || !resp.Handled || resp.AuthID != "go-a-1" {
		t.Fatalf("Pick(provider-qualified GLM) = %#v, %v", resp, err)
	}
}

func TestUnqualifiedGLMRequestIsNotClaimedByOpenCodeModule(t *testing.T) {
	module := configuredModule(t, 97)
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Model:      "glm-5.3",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "foreign-glm", Provider: "zai"}},
	})
	if err != nil || resp.Handled {
		t.Fatalf("Pick(unqualified GLM) = %#v, %v, want unhandled", resp, err)
	}
}

func TestRecognizedUnmanagedAndAllImpairedFailClosed(t *testing.T) {
	module := configuredModule(t, 97)
	_, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "missing", Provider: providerID}}})
	if err == nil || !strings.Contains(err.Error(), "opencode_go_unmanaged_candidate") {
		t.Fatalf("unmanaged Pick() error = %v", err)
	}

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, authID := range []string{"go-a-1", "go-b-1"} {
		record := monthlyLimitRecord(authID, now, 3600)
		if err := module.HandleUsage(context.Background(), record); err != nil {
			t.Fatalf("HandleUsage() error = %v", err)
		}
	}
	_, err = module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "opencode_go_no_capacity") {
		t.Fatalf("all-impaired Pick() error = %v", err)
	}
}

func TestReconfigureFailureRetainsPriorSnapshot(t *testing.T) {
	module := configuredModule(t, 97)
	err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, json.RawMessage(`{"unknown":true}`))
	if err == nil {
		t.Fatal("invalid Reconfigure() = nil error")
	}
	if got := module.OwnedAuthIDs(context.Background()); len(got) != 2 || got[0] != "go-a-1" || got[1] != "go-b-1" {
		t.Fatalf("OwnedAuthIDs() after rejected config = %#v, want prior snapshot", got)
	}
	resp, pickErr := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}}})
	if pickErr != nil || !resp.Handled || resp.AuthID != "go-a-1" {
		t.Fatalf("Pick() after rejected config = %#v, %v", resp, pickErr)
	}
}

func TestSuccessfulReconfigureCarriesForwardLiveWindowState(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecord("go-a-1", now, 3600)); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"threshold-percent":97,"accounts":[{"name":"go-a","auth-ids":["go-a-1"]},{"name":"go-b","auth-ids":["go-b-1"]}]}`)
	if err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err != nil || resp.AuthID != "go-b-1" {
		t.Fatalf("Pick() after same-account reconfigure = %#v, %v", resp, err)
	}
}

func TestQuotaWindowWithoutResetUsesCooldownButReportsResetUnknown(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		AuthID:      "go-a-1",
		RequestedAt: now,
		Failed:      true,
		Failure: pluginapi.UsageFailure{
			StatusCode: http.StatusTooManyRequests,
			Body:       `[opencode-go/qwen] [429]: Monthly usage limit reached.`,
		},
	}
	if err := module.HandleUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if !window.Exhausted || !window.ResetAt.IsZero() || !window.CooldownAt.Equal(now.Add(defaultFailureCooldown)) {
		t.Fatalf("window = %#v, want cooldown with unknown reset", window)
	}
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Accounts []struct {
			Windows map[string]json.RawMessage `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	monthly := string(status.Accounts[0].Windows[string(windowMonthly)])
	if strings.Contains(monthly, "resets_at") {
		t.Fatalf("Status() fabricated reset timestamp: %s", monthly)
	}
}

func TestWindowSignalsImpairOnlyTheirGoverningWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		body   string
		window windowKind
	}{
		{"five-hour", `[opencode-go/qwen] [429]: Five-hour usage limit reached. Resets in 4hr 59min. To continue using this model now, enable us (reset after 4h 59m 1s)`, windowFiveHour},
		{"weekly", `[opencode-go/qwen] [429]: Weekly usage limit reached. Resets in 6d 2hr. To continue using this model now, enable us (reset after 6d 6h 1m 2s)`, windowWeekly},
		{"monthly", `[opencode-go/qwen] [429]: Monthly usage limit reached. Resets in 2hr 37min. To continue using this model now, enable us (reset after 2h 37m 57s)`, windowMonthly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			module := configuredModule(t, 97)
			record := pluginapi.UsageRecord{
				AuthID:          "go-a-1",
				RequestedAt:     now,
				Failed:          true,
				Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: tc.body},
				ResponseHeaders: http.Header{"Retry-After": []string{"9477"}},
			}
			if err := module.HandleUsage(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			module.mu.Lock()
			account := module.state.Accounts["go-a"]
			for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
				got := account.Windows[kind]
				if kind == tc.window && (!got.Exhausted || !got.ResetAt.Equal(now.Add(9477*time.Second))) {
					t.Fatalf("window %s = %#v, want exhausted at Retry-After reset", kind, got)
				}
				if kind != tc.window && got.Exhausted {
					t.Fatalf("window %s was impaired by %s response", kind, tc.window)
				}
			}
			module.mu.Unlock()
		})
	}
}

func TestRetryAfterLookupIsCaseInsensitive(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resetAt, ok := retryAfterReset(http.Header{"retry-after": []string{"9477"}}, now)
	if !ok || !resetAt.Equal(now.Add(9477*time.Second)) {
		t.Fatalf("retryAfterReset() = %v, %v", resetAt, ok)
	}
}

func TestBodyResetParsesDayHourMinuteSecondShape(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resetAt, ok := bodyReset(`Weekly usage limit reached (reset after 6d 6h 1m 2s)`, now)
	if !ok || !resetAt.Equal(now.Add((6*24+6)*time.Hour+time.Minute+2*time.Second)) {
		t.Fatalf("bodyReset() = %v, %v", resetAt, ok)
	}
}

func TestMicrothrottleDoesNotFeedPacingState(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		AuthID:      "go-a-1",
		RequestedAt: now,
		Failed:      true,
		Failure: pluginapi.UsageFailure{
			StatusCode: http.StatusForbidden,
			Body:       `[opencode-go/qwen] [403]: <!doctype html> (reset after 2s)`,
		},
	}
	if err := module.HandleUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	defer module.mu.Unlock()
	for kind, window := range module.state.Accounts["go-a"].Windows {
		if window.Exhausted || window.Utilization != nil || !window.ResetAt.IsZero() || !window.CooldownAt.IsZero() {
			t.Fatalf("microthrottle changed %s state: %#v", kind, window)
		}
	}
}

func TestDegradedPickExcludesExhaustedAccount(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecord("go-a-1", now, 3600)); err != nil {
		t.Fatal(err)
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "go-b-1" || resp.DelegateBuiltin != "" {
		t.Fatalf("Pick() = %#v, want healthy account go-b-1", resp)
	}
}

func TestStatusDeclaresObservationGaps(t *testing.T) {
	module := configuredModule(t, 97)
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "five-hour and weekly enforcement") || !strings.Contains(text, "no dashboard credential is configured") || !strings.Contains(text, `"credential_bound":false`) {
		t.Fatalf("Status() omitted clean-room observation gaps: %s", raw)
	}
}

func TestStatusOmitsUnknownUtilizationAndReset(t *testing.T) {
	module := configuredModule(t, 97)
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Accounts []struct {
			Windows map[string]json.RawMessage `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	for _, account := range status.Accounts {
		for kind, rawWindow := range account.Windows {
			window := string(rawWindow)
			if strings.Contains(window, "utilization") || strings.Contains(window, "resets_at") {
				t.Fatalf("Status() reported unknown %s telemetry as known: %s", kind, window)
			}
		}
	}
}

func TestStatusClearsExpiredWindows(t *testing.T) {
	module := configuredModule(t, 97)
	module.mu.Lock()
	module.state.Accounts["go-a"].Windows[windowMonthly] = windowState{
		Utilization: float64Pointer(1),
		Exhausted:   true,
		ResetAt:     time.Now().UTC().Add(-time.Minute),
		CooldownAt:  time.Now().UTC().Add(-time.Minute),
	}
	module.mu.Unlock()
	if _, err := module.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if window.Exhausted || window.Utilization != nil || !window.ResetAt.IsZero() || !window.CooldownAt.IsZero() {
		t.Fatalf("Status() retained expired window: %#v", window)
	}
}

const dashboardFixtureKey = "test-only-e2e-dashboard-key"

type fakeModuleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeModuleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeModuleClock) Sleep(context.Context, time.Duration) error { return nil }

func (c *fakeModuleClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func configuredModuleWithCredential(t *testing.T, threshold int, apiKey string, client providermodule.HTTPDoer, clock providermodule.Clock) *Module {
	t.Helper()
	module := NewModule()
	raw := json.RawMessage(`{"threshold-percent":` + jsonNumber(threshold) + `,"dashboard-api-key":"` + apiKey + `","accounts":[{"name":"go-a","auth-ids":["go-a-1"]},{"name":"go-b","auth-ids":["go-b-1"]}]}`)
	host := providermodule.HostConfig{Clock: clock, HTTPClient: client}
	if err := module.Reconfigure(context.Background(), host, raw); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	return module
}

func TestStatusWithoutCredentialReportsUnknownNotGuessed(t *testing.T) {
	module := configuredModule(t, 97)
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"credential_bound":false`) {
		t.Fatalf("Status() = %s, want credential_bound:false", text)
	}
	if strings.Contains(text, "resets_at") {
		t.Fatalf("Status() fabricated a reset time without a credential: %s", text)
	}
	if !strings.Contains(text, `"known":false`) {
		t.Fatalf("Status() did not report unknown windows: %s", text)
	}
}

func TestStatusPollsAndSurfacesRealResetTimes(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour)
	weeklyReset := now.Add(5 * 24 * time.Hour)
	monthlyReset := now.Add(20 * 24 * time.Hour)
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+dashboardFixtureKey {
			t.Fatalf("authorization = %q", got)
		}
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 30, fiveHourReset),
			usageWindowFixture("ok", 60, weeklyReset),
			usageWindowFixture("ok", 90, monthlyReset),
		)), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		CredentialBound bool `json:"credential_bound"`
		Accounts        []struct {
			Windows map[windowKind]struct {
				Known         bool       `json:"known"`
				Utilization   float64    `json:"utilization"`
				ResetAt       *time.Time `json:"resets_at"`
				Authoritative bool       `json:"authoritative"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Status() invalid JSON: %v, raw = %s", err, raw)
	}
	if !parsed.CredentialBound {
		t.Fatalf("Status() = %s, want credential_bound:true", raw)
	}
	for _, account := range parsed.Accounts {
		fiveHour := account.Windows[windowFiveHour]
		if !fiveHour.Known || !fiveHour.Authoritative || fiveHour.Utilization != 0.30 || fiveHour.ResetAt == nil || !fiveHour.ResetAt.Equal(fiveHourReset) {
			t.Fatalf("five_hour window = %#v, want real polled reset", fiveHour)
		}
		weekly := account.Windows[windowWeekly]
		if weekly.ResetAt == nil || !weekly.ResetAt.Equal(weeklyReset) {
			t.Fatalf("weekly window = %#v, want real polled reset", weekly)
		}
		monthly := account.Windows[windowMonthly]
		if monthly.ResetAt == nil || !monthly.ResetAt.Equal(monthlyReset) {
			t.Fatalf("monthly window = %#v, want real polled reset", monthly)
		}
	}
	if strings.Contains(string(raw), "no dashboard credential is configured") {
		t.Fatalf("Status() kept the no-credential gap after a credential was bound: %s", raw)
	}
}

func TestStatusFailsClosedOnUnauthorizedPoll(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusUnauthorized, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"credential_bound":true`) {
		t.Fatalf("Status() = %s, want credential_bound:true even though the poll failed", text)
	}
	if strings.Contains(text, "resets_at") {
		t.Fatalf("Status() fabricated a reset time from a failed poll: %s", text)
	}
	if !strings.Contains(text, `"known":false`) {
		t.Fatalf("Status() did not stay unknown after a failed poll: %s", text)
	}
}

func TestStatusFailsClosedOnMalformedPollResponse(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusOK, `{not json`), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resets_at") {
		t.Fatalf("Status() fabricated a reset time from a malformed poll: %s", raw)
	}
}

// TestStalePollNeverOverwritesFresherInferredExhaustion and its sibling
// prove the mergeWindowSignal precedence rule end to end: a poll observed
// strictly before an existing 429-inferred exhaustion can never clobber it,
// but a genuinely fresher poll does update the lane (rules 2 and 4 of
// mergeWindowSignal's doc comment).
func TestStalePollNeverOverwritesFresherInferredExhaustion(t *testing.T) {
	later := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)
	farFutureReset := later.Add(20 * 24 * time.Hour)
	clock := &fakeModuleClock{now: later}
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 1, later.Add(time.Hour)),
			usageWindowFixture("ok", 1, later.Add(7*24*time.Hour)),
			usageWindowFixture("ok", 1, later.Add(30*24*time.Hour)),
		)), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, clock)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecordAt("go-a-1", later, farFutureReset)); err != nil {
		t.Fatal(err)
	}
	// Roll the clock backward before polling, simulating a poll response
	// whose observation time is older than the 429 already recorded --
	// e.g. a delayed/retried fetch racing the live event.
	clock.set(earlier)
	if _, err := module.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if !window.Exhausted || !window.ResetAt.Equal(farFutureReset) {
		t.Fatalf("window = %#v, want the fresher 429 exhaustion preserved over the stale poll", window)
	}
}

func TestFresherPollUpdatesStaleInferredExhaustion(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	clock := &fakeModuleClock{now: now}
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 1, now.Add(time.Hour)),
			usageWindowFixture("ok", 1, now.Add(7*24*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(30*24*time.Hour)),
		)), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, clock)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecordAt("go-a-1", now.Add(-time.Hour), now.Add(30*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := module.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if window.Exhausted || !window.Authoritative || !window.ResetAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("window = %#v, want the fresher poll to override the stale, already-expiring 429 signal", window)
	}
}

func TestStatusPollsEachAccountWithItsOwnDashboardKey(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	const (
		go1Key = "test-only-go-1-key"
		go2Key = "test-only-go-2-key"
		go3Key = "test-only-go-3-key"
	)
	responses := map[string]string{
		go1Key: usageFixture(
			usageWindowFixture("ok", 1, now.Add(2*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(5*24*time.Hour)),
			usageWindowFixture("ok", 3, now.Add(29*24*time.Hour)),
		),
		go2Key: usageFixture(
			usageWindowFixture("ok", 1, now.Add(2*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(5*24*time.Hour)),
			usageWindowFixture("rate-limited", 100, now.Add(10*24*time.Hour)),
		),
		go3Key: usageFixture(
			usageWindowFixture("ok", 1, now.Add(2*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(5*24*time.Hour)),
			usageWindowFixture("ok", 90, now.Add(23*24*time.Hour)),
		),
	}
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		body, ok := responses[key]
		if !ok {
			t.Fatalf("unexpected dashboard key on request: %q", key)
		}
		return usageHTTPResponse(http.StatusOK, body), nil
	})
	module := NewModule()
	raw := json.RawMessage(`{
		"threshold-percent": 97,
		"accounts": [
			{"name": "go-1", "auth-ids": ["go-1-1"], "dashboard-api-key": "` + go1Key + `"},
			{"name": "go-2", "auth-ids": ["go-2-1"], "dashboard-api-key": "` + go2Key + `"},
			{"name": "go-3", "auth-ids": ["go-3-1"], "dashboard-api-key": "` + go3Key + `"}
		]
	}`)
	host := providermodule.HostConfig{Clock: &fakeModuleClock{now: now}, HTTPClient: client}
	if err := module.Reconfigure(context.Background(), host, raw); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	raw2, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Accounts []struct {
			Name    string `json:"name"`
			Windows map[windowKind]struct {
				Utilization float64 `json:"utilization"`
				Exhausted   bool    `json:"exhausted"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw2, &parsed); err != nil {
		t.Fatalf("Status() invalid JSON: %v, raw = %s", err, raw2)
	}
	monthly := make(map[string]float64, len(parsed.Accounts))
	exhausted := make(map[string]bool, len(parsed.Accounts))
	for _, account := range parsed.Accounts {
		monthly[account.Name] = account.Windows[windowMonthly].Utilization
		exhausted[account.Name] = account.Windows[windowMonthly].Exhausted
	}
	if monthly["go-1"] != 0.03 || exhausted["go-1"] {
		t.Fatalf("go-1 monthly = %v exhausted=%v, want 0.03 not exhausted", monthly["go-1"], exhausted["go-1"])
	}
	if !exhausted["go-2"] {
		t.Fatalf("go-2 monthly exhausted = %v, want true (rate-limited)", exhausted["go-2"])
	}
	if monthly["go-3"] != 0.90 || exhausted["go-3"] {
		t.Fatalf("go-3 monthly = %v exhausted=%v, want 0.90 not exhausted", monthly["go-3"], exhausted["go-3"])
	}
	if monthly["go-1"] == monthly["go-3"] {
		t.Fatalf("go-1 and go-3 reported identical monthly utilization: %v", monthly["go-1"])
	}
}

func TestAccountDashboardKeyFallsBackToModuleLevelKey(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var requestedKeys []string
	var mu sync.Mutex
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		requestedKeys = append(requestedKeys, request.Header.Get("Authorization"))
		mu.Unlock()
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 1, now.Add(2*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(5*24*time.Hour)),
			usageWindowFixture("ok", 1, now.Add(20*24*time.Hour)),
		)), nil
	})
	module := NewModule()
	raw := json.RawMessage(`{
		"threshold-percent": 97,
		"dashboard-api-key": "` + dashboardFixtureKey + `",
		"accounts": [
			{"name": "go-a", "auth-ids": ["go-a-1"]},
			{"name": "go-b", "auth-ids": ["go-b-1"], "dashboard-api-key": "test-only-go-b-key"}
		]
	}`)
	host := providermodule.HostConfig{Clock: &fakeModuleClock{now: now}, HTTPClient: client}
	if err := module.Reconfigure(context.Background(), host, raw); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	if _, err := module.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"Bearer " + dashboardFixtureKey: false, "Bearer test-only-go-b-key": false}
	for _, got := range requestedKeys {
		if _, ok := want[got]; !ok {
			t.Fatalf("unexpected key polled: %q", got)
		}
		want[got] = true
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("expected key %q to be polled, requested = %#v", key, requestedKeys)
		}
	}
}

func TestPollNeverLeaksCredentialFromStatusOrError(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusUnauthorized, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`), nil
	})
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	lastErr := module.lastErr
	module.mu.Unlock()
	if strings.Contains(string(raw), dashboardFixtureKey) || strings.Contains(lastErr, dashboardFixtureKey) {
		t.Fatalf("credential leaked: status = %s, lastErr = %q", raw, lastErr)
	}
}

func monthlyLimitRecordAt(authID string, requestedAt, resetAt time.Time) pluginapi.UsageRecord {
	seconds := int(resetAt.Sub(requestedAt) / time.Second)
	return pluginapi.UsageRecord{
		AuthID:          authID,
		RequestedAt:     requestedAt,
		Failed:          true,
		Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `[opencode-go/qwen] [429]: Monthly usage limit reached. Resets in 1hr. To continue using this model now, enable us (reset after 1h)`},
		ResponseHeaders: http.Header{"Retry-After": []string{strconv.Itoa(seconds)}},
	}
}

func monthlyLimitRecord(authID string, now time.Time, seconds int) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		AuthID:          authID,
		RequestedAt:     now,
		Failed:          true,
		Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `[opencode-go/qwen] [429]: Monthly usage limit reached. Resets in 1hr. To continue using this model now, enable us (reset after 1h)`},
		ResponseHeaders: http.Header{"Retry-After": []string{strconv.Itoa(seconds)}},
	}
}

func TestDisabledModuleDoesNotClaimOpenCodeGoTraffic(t *testing.T) {
	m := NewModule()
	if err := m.Reconfigure(context.Background(), providermodule.HostConfig{}, json.RawMessage(`{"enabled":false,"accounts":[{"name":"go-1","auth-ids":["openai-compatibility:opencode-go:a"]}]}`)); err != nil {
		t.Fatal(err)
	}
	candidate := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:opencode-go:a", Provider: "openai-compatible-opencode-go", Attributes: map[string]string{"compat_name": "opencode-go"}}
	if m.Recognize(candidate) {
		t.Fatal("disabled OpenCode Go module recognized its own provider's candidate; Pick would then fail every request as unmanaged")
	}
}

func TestUsageAccountNamesTheOwningAccount(t *testing.T) {
	module := configuredModule(t, 97)
	if got := module.UsageAccount(pluginapi.UsageRecord{AuthID: "go-b-1"}); got != "go-b" {
		t.Fatalf("UsageAccount(go-b-1) = %q, want go-b", got)
	}
	if got := module.UsageAccount(pluginapi.UsageRecord{AuthID: "other"}); got != "" {
		t.Fatalf("UsageAccount(other) = %q, want empty", got)
	}
	if got := NewModule().UsageAccount(pluginapi.UsageRecord{AuthID: "go-b-1"}); got != "" {
		t.Fatalf("UsageAccount on an unconfigured module = %q, want empty", got)
	}
}
