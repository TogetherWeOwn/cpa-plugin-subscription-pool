package synthetic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const (
	testKey  = "test-synthetic-key"
	testBase = "http://mock:8080/openai/v1"
	// Golden IDs produced by a live CLIProxyAPI v8.0.38 instance for testKey
	// and testBase; they pin discoverLanes to the host's derivation.
	paidAuthID     = "openai-compatibility:synthetic:35470c92a63b"
	infiniteAuthID = "openai-compatibility:synthetic-infinite:c5e9e38ce3e1"
)

var testStart = time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func hostWith(compats ...providermodule.OpenAICompatibility) providermodule.HostConfig {
	return providermodule.HostConfig{OpenAICompatibility: compats}
}

func syntheticCompats(key string) []providermodule.OpenAICompatibility {
	return []providermodule.OpenAICompatibility{
		{Name: "opencode-go", BaseURL: "http://relay/zen/go/v1", APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: "go-key"}}},
		{Name: "synthetic-infinite", BaseURL: testBase, APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: key}}},
		{Name: "synthetic", BaseURL: testBase, APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: key}}},
	}
}

func newTestModule(t *testing.T, providerConfig string, compats ...providermodule.OpenAICompatibility) (*Module, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: testStart}
	m := &Module{now: clock.Now, noPoller: true}
	if len(compats) == 0 {
		compats = syntheticCompats(testKey)
	}
	if err := m.Reconfigure(context.Background(), hostWith(compats...), json.RawMessage(providerConfig)); err != nil {
		t.Fatalf("Reconfigure error = %v", err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, clock
}

func candidate(id, compat string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{
		ID:         id,
		Provider:   "openai-compatible-" + compat,
		Attributes: map[string]string{"compat_name": compat, "provider_key": "openai-compatible-" + compat, "base_url": testBase},
	}
}

func pickReq(stream bool, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "", Model: "synthetic/deepseek-v4.1-flash", Stream: stream, Candidates: candidates}
}

func both() []pluginapi.SchedulerAuthCandidate {
	return []pluginapi.SchedulerAuthCandidate{candidate(paidAuthID, "synthetic"), candidate(infiniteAuthID, "synthetic-infinite")}
}

func mustPick(t *testing.T, m *Module, req pluginapi.SchedulerPickRequest) string {
	t.Helper()
	resp, err := m.Pick(context.Background(), req)
	if err != nil {
		t.Fatalf("Pick error = %v", err)
	}
	if !resp.Handled {
		t.Fatalf("Pick not handled: %+v", resp)
	}
	return resp.AuthID
}

func usage(authID string, status int, headers http.Header) pluginapi.UsageRecord {
	record := pluginapi.UsageRecord{AuthID: authID, Provider: "openai-compatible-synthetic", RequestedAt: testStart, ResponseHeaders: headers}
	if status != http.StatusOK {
		record.Failed = true
		record.Failure = pluginapi.UsageFailure{StatusCode: status, Body: `{"error":"Weekly limit reached"}`}
	}
	return record
}

func quotaHeaderValue(remaining float64, regenAt time.Time) http.Header {
	raw, _ := json.Marshal(map[string]any{"planWeeklyLimit": map[string]any{"percentRemaining": remaining, "nextRegenAt": regenAt.Format(time.RFC3339), "nextRegenPercent": 2}})
	return http.Header{"X-Synthetic-Quotas": {string(raw)}}
}

func statusOf(t *testing.T, m *Module) map[string]any {
	t.Helper()
	raw, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiscoverLanesMatchesHostAuthIDs(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	got := m.OwnedAuthIDs(context.Background())
	if len(got) != 2 || got[0] != infiniteAuthID || got[1] != paidAuthID {
		t.Fatalf("OwnedAuthIDs = %v, want [%s %s]", got, infiniteAuthID, paidAuthID)
	}
}

func TestInactiveWithoutSyntheticProviders(t *testing.T) {
	m, _ := newTestModule(t, "{}", providermodule.OpenAICompatibility{Name: "opencode-go", BaseURL: "http://relay", APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: "k"}}})
	if m.Recognize(candidate(paidAuthID, "synthetic")) {
		t.Fatal("inactive module recognized a candidate")
	}
	if ids := m.OwnedAuthIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("inactive module owns %v", ids)
	}
	resp, err := m.Pick(context.Background(), pickReq(true, both()...))
	if err != nil || resp.Handled {
		t.Fatalf("inactive Pick = %+v, %v", resp, err)
	}
	if status := statusOf(t, m)["status"]; status != "inactive" {
		t.Fatalf("status = %v, want inactive", status)
	}
}

func TestDisabledModuleRecognizesNothing(t *testing.T) {
	m, _ := newTestModule(t, `{"enabled":false}`)
	if m.Recognize(candidate(paidAuthID, "synthetic")) {
		t.Fatal("disabled module recognized a candidate")
	}
}

func TestRecognizeByProviderAttributes(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	if !m.Recognize(candidate("openai-compatibility:synthetic:ffffffffffff", "synthetic")) {
		t.Fatal("unknown synthetic auth ID not recognized by compat_name")
	}
	if m.Recognize(candidate("openai-compatibility:opencode-go:abc", "opencode-go")) {
		t.Fatal("opencode-go candidate recognized")
	}
}

func TestPaidPreferredWhileHealthy(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	for i := 0; i < 3; i++ {
		if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
			t.Fatalf("pick %d = %s, want paid", i, got)
		}
	}
}

func TestThresholdFallsBackToInfinite(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	if err := m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(4, testStart.Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick at 96%% used = %s, want infinite", got)
	}
	routing := statusOf(t, m)["routing"].(map[string]any)
	if routing["fallbacks_to_infinite"].(float64) != 1 || !strings.Contains(routing["last_fallback_reason"].(string), "threshold") {
		t.Fatalf("routing = %v", routing)
	}
	// Regeneration lifts the plan back under the threshold.
	if err := m.HandleUsage(context.Background(), usage(infiniteAuthID, http.StatusOK, quotaHeaderValue(8, testStart.Add(4*time.Hour)))); err != nil {
		t.Fatal(err)
	}
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after regeneration = %s, want paid", got)
	}
}

func TestPaidOnlyModelRejectedAtThreshold(t *testing.T) {
	m, _ := newTestModule(t, `{"threshold-percent":90}`)
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(9, testStart.Add(time.Hour))))
	_, err := m.Pick(context.Background(), pickReq(true, candidate(paidAuthID, "synthetic")))
	var coded providermodule.Coded
	if !errors.As(err, &coded) {
		t.Fatalf("Pick error = %v, want coded rejection", err)
	}
	wire := coded.Coded()
	if wire.Code != "synthetic_no_capacity" || wire.HTTPStatus != http.StatusTooManyRequests || !wire.Retryable {
		t.Fatalf("wire error = %+v", wire)
	}
	if !strings.Contains(wire.Message, "no infinite-mode fallback") {
		t.Fatalf("message = %q", wire.Message)
	}
}

func TestQuota429ExhaustsUntilRegeneration(t *testing.T) {
	m, clock := newTestModule(t, "{}")
	regen := testStart.Add(90 * time.Minute)
	if err := m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusTooManyRequests, quotaHeaderValue(0, regen))); err != nil {
		t.Fatal(err)
	}
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick after quota 429 = %s, want infinite", got)
	}
	account := statusOf(t, m)["accounts"].([]any)[0].(map[string]any)
	if account["kind"] != "paid" || account["health"] != "exhausted" {
		t.Fatalf("paid lane status = %v", account)
	}
	clock.Advance(91 * time.Minute)
	// A fresh reading after the regeneration tick (2% back) is still above the
	// threshold, so paid stays parked; only a reading under it reopens paid.
	_ = m.HandleUsage(context.Background(), usage(infiniteAuthID, http.StatusOK, quotaHeaderValue(2, testStart.Add(5*time.Hour))))
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick at 98%% used = %s, want infinite", got)
	}
}

func Test402WithoutHeaderUsesFailureCooldown(t *testing.T) {
	m, clock := newTestModule(t, `{"failure-cooldown":"10m"}`)
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusPaymentRequired, nil))
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick after 402 = %s, want infinite", got)
	}
	clock.Advance(11 * time.Minute)
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after cooldown = %s, want paid", got)
	}
}

func TestRateLimit429WithCreditsOnlyRestsLane(t *testing.T) {
	m, clock := newTestModule(t, `{"infinite-cooldown":"30s"}`)
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusTooManyRequests, quotaHeaderValue(60, testStart.Add(time.Hour))))
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick during rate-limit rest = %s, want infinite", got)
	}
	account := statusOf(t, m)["accounts"].([]any)[0].(map[string]any)
	if account["health"] != "throttled" {
		t.Fatalf("paid health = %v, want throttled (not exhausted)", account["health"])
	}
	clock.Advance(31 * time.Second)
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after rest = %s, want paid", got)
	}
}

func TestBothUnavailableRejectsWithBothReasons(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusTooManyRequests, quotaHeaderValue(0, testStart.Add(time.Hour))))
	infinite429 := usage(infiniteAuthID, http.StatusTooManyRequests, nil)
	infinite429.Provider = "openai-compatible-synthetic-infinite"
	_ = m.HandleUsage(context.Background(), infinite429)
	_, err := m.Pick(context.Background(), pickReq(true, both()...))
	if err == nil || !strings.Contains(err.Error(), "synthetic_no_capacity") || !strings.Contains(err.Error(), "infinite mode unavailable") {
		t.Fatalf("Pick error = %v", err)
	}
	_, err = m.Pick(context.Background(), pickReq(true, candidate(infiniteAuthID, "synthetic-infinite")))
	if err == nil || !strings.Contains(err.Error(), "synthetic_infinite_busy") {
		t.Fatalf("infinite-only Pick error = %v", err)
	}
}

func TestConcurrencyCapOverflowsToInfinite(t *testing.T) {
	m, _ := newTestModule(t, `{"max-concurrent":1}`)
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("first pick = %s", got)
	}
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("second pick at cap = %s, want infinite", got)
	}
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, nil))
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after release = %s, want paid", got)
	}
}

func TestInFlightSlotsExpire(t *testing.T) {
	m, clock := newTestModule(t, `{"max-concurrent":1,"in-flight-ttl":"10m"}`)
	mustPick(t, m, pickReq(true, both()...))
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick at cap = %s", got)
	}
	clock.Advance(11 * time.Minute)
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after TTL = %s, want paid (lost usage record must not wedge the lane)", got)
	}
}

func TestInfiniteConcurrencyCap(t *testing.T) {
	m, _ := newTestModule(t, `{"infinite-max-concurrent":1}`)
	only := candidate(infiniteAuthID, "synthetic-infinite")
	mustPick(t, m, pickReq(true, only))
	if _, err := m.Pick(context.Background(), pickReq(true, only)); err == nil || !strings.Contains(err.Error(), "infinite concurrency cap 1") {
		t.Fatalf("second infinite pick error = %v", err)
	}
}

func TestForeignCandidateDeclines(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	resp, err := m.Pick(context.Background(), pickReq(true, candidate(paidAuthID, "synthetic"), candidate("openai-compatibility:opencode-go:x", "opencode-go")))
	if err != nil || resp.Handled {
		t.Fatalf("mixed Pick = %+v, %v; want Handled:false", resp, err)
	}
}

func TestLearnedCredentialBindsToSoleAccount(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(1, testStart.Add(time.Hour))))
	rekeyed := candidate("openai-compatibility:synthetic:aaaaaaaaaaaa", "synthetic")
	// The re-keyed paid credential inherits the account's exhausted quota.
	if got := mustPick(t, m, pickReq(true, rekeyed, candidate(infiniteAuthID, "synthetic-infinite"))); got != infiniteAuthID {
		t.Fatalf("pick = %s, want infinite", got)
	}
}

// twoAccountCompats configures two synthetic.new keys on both the paid and the
// infinite provider, so a learned paid credential has no sole account to bind.
func twoAccountCompats() []providermodule.OpenAICompatibility {
	keys := []providermodule.OpenAICompatibilityAPIKey{{APIKey: testKey}, {APIKey: testKey + "-second"}}
	return []providermodule.OpenAICompatibility{
		{Name: "synthetic-infinite", BaseURL: testBase, APIKeyEntries: keys},
		{Name: "synthetic", BaseURL: testBase, APIKeyEntries: keys},
	}
}

func discoveredLanes(t *testing.T, m *Module, kind laneKind) []string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, item := range m.lanes {
		if !item.learned && item.Kind == kind {
			ids = append(ids, id)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("discovered %s lanes = %v, want 2", kind, ids)
	}
	return ids
}

func TestUnboundLearnedPaidCredentialFailsClosed(t *testing.T) {
	m, _ := newTestModule(t, "{}", twoAccountCompats()...)
	for _, id := range discoveredLanes(t, m, kindPaid) {
		_ = m.HandleUsage(context.Background(), usage(id, http.StatusOK, quotaHeaderValue(0, testStart.Add(time.Hour))))
	}
	infinite := discoveredLanes(t, m, kindInfinite)
	rekeyed := candidate("openai-compatibility:synthetic:bbbbbbbbbbbb", "synthetic")

	// Both known paid accounts are at 100% used, and the re-keyed paid
	// credential cannot be tied to either of them: it must not be admitted.
	got := mustPick(t, m, pickReq(true, rekeyed, candidate(infinite[0], "synthetic-infinite")))
	if got != infinite[0] {
		t.Fatalf("pick = %s, want infinite %s (unbound learned paid lane must fail closed)", got, infinite[0])
	}
	if _, err := m.Pick(context.Background(), pickReq(true, rekeyed)); err == nil || !strings.Contains(err.Error(), "not bound to a known synthetic.new account") {
		t.Fatalf("paid-only pick error = %v, want synthetic_no_capacity naming the unbound credential", err)
	}
	for _, item := range statusOf(t, m)["accounts"].([]any) {
		lane := item.(map[string]any)
		if lane["learned"] == true && (lane["health"] != "unbound" || !strings.Contains(lane["health_reason"].(string), "not bound")) {
			t.Fatalf("learned lane status = %v", lane)
		}
	}
}

func TestUnboundLearnedPaidCredentialFailsClosedEvenWithCreditsLeft(t *testing.T) {
	m, _ := newTestModule(t, "{}", twoAccountCompats()...)
	rekeyed := candidate("openai-compatibility:synthetic:cccccccccccc", "synthetic")
	known := discoveredLanes(t, m, kindPaid)
	// A known paid credential still schedules; only the unbound one is refused.
	got := mustPick(t, m, pickReq(true, rekeyed, candidate(known[0], "synthetic")))
	if got != known[0] {
		t.Fatalf("pick = %s, want bound paid credential %s", got, known[0])
	}
}

func TestNonStreamInfinitePicksAreCounted(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(0, testStart.Add(time.Hour))))
	mustPick(t, m, pickReq(false, both()...))
	routing := statusOf(t, m)["routing"].(map[string]any)
	if routing["infinite_non_stream_picks"].(float64) != 1 {
		t.Fatalf("routing = %v", routing)
	}
}

func TestRecognizeUsageByProvider(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	if !m.RecognizeUsage(pluginapi.UsageRecord{AuthID: "openai-compatibility:synthetic:zzzzzzzzzzzz", Provider: "openai-compatible-synthetic"}) {
		t.Fatal("usage from a re-keyed synthetic credential not recognized")
	}
	if m.RecognizeUsage(pluginapi.UsageRecord{AuthID: "x", Provider: "openai-compatible-opencode-go"}) {
		t.Fatal("foreign usage recognized")
	}
}

func TestThroughputStats(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	for _, out := range []int64{100, 200, 300} {
		record := usage(infiniteAuthID, http.StatusOK, nil)
		record.Provider = "openai-compatible-synthetic-infinite"
		record.TTFT = 2 * time.Second
		record.Latency = 12 * time.Second
		record.Detail.OutputTokens = out
		_ = m.HandleUsage(context.Background(), record)
	}
	accounts := statusOf(t, m)["accounts"].([]any)
	infinite := accounts[1].(map[string]any)
	stats := infinite["stats"].(map[string]any)
	if infinite["kind"] != "infinite" || stats["requests"].(float64) != 3 || stats["median_output_tps"].(float64) != 20 || stats["mean_ttft_ms"].(float64) != 2000 {
		t.Fatalf("infinite stats = %v", stats)
	}
}

func TestPlaceholderKeyIsUnboundAndNeverPolled(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer server.Close()
	m, _ := newTestModule(t, `{"quota-url":"`+server.URL+`/v2/quotas"}`, syntheticCompats("<API KEY PLACEHOLDER>")...)
	m.httpClient = server.Client()
	m.pollOnce(context.Background(), true)
	if hits != 0 {
		t.Fatalf("placeholder key was sent to the quota endpoint %d times", hits)
	}
	status := statusOf(t, m)
	if status["credential_bound"] != false {
		t.Fatalf("credential_bound = %v", status["credential_bound"])
	}
	for _, item := range status["accounts"].([]any) {
		if item.(map[string]any)["health"] != "unbound" {
			t.Fatalf("lane health = %v, want unbound", item.(map[string]any)["health"])
		}
	}
	// Scheduling still works (requests then fail upstream with 401 until the key is pasted).
	if got := mustPick(t, m, pickReq(true, candidate("openai-compatibility:synthetic:08e39ede6b45", "synthetic"))); got == "" {
		t.Fatal("no pick with placeholder key")
	}
}

func TestPollUpdatesQuota(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":25,"nextRegenAt":"2026-10-23T08:00:00Z","nextRegenPercent":2}}`))
	}))
	defer server.Close()
	m, _ := newTestModule(t, `{"quota-url":"`+server.URL+`/v2/quotas"}`)
	m.httpClient = server.Client()
	m.pollOnce(context.Background(), true)
	if auth != "Bearer "+testKey {
		t.Fatalf("poll authorization = %q", auth)
	}
	paid := statusOf(t, m)["accounts"].([]any)[0].(map[string]any)
	weekly := paid["windows"].(map[string]any)["weekly"].(map[string]any)
	if weekly["utilization"].(float64) != 0.75 || weekly["authoritative"] != true || paid["next_regen_percent"].(float64) != 2 {
		t.Fatalf("paid lane after poll = %v", paid)
	}
	raw, _ := m.Status(context.Background())
	if strings.Contains(string(raw), testKey) {
		t.Fatal("status leaked the API key")
	}
}

func TestReconfigureCarriesStateAndRekeys(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(1, testStart.Add(time.Hour))))
	if err := m.Reconfigure(context.Background(), hostWith(syntheticCompats(testKey)...), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("quota state lost across Reconfigure: pick = %s", got)
	}
	if err := m.Reconfigure(context.Background(), hostWith(syntheticCompats("another-real-key")...), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	ids := m.OwnedAuthIDs(context.Background())
	if len(ids) != 2 || ids[0] == paidAuthID {
		t.Fatalf("re-keyed IDs = %v", ids)
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	m := &Module{noPoller: true}
	if err := m.Reconfigure(context.Background(), hostWith(syntheticCompats(testKey)...), json.RawMessage(`{"threshold-percent":0}`)); err == nil {
		t.Fatal("invalid config accepted")
	}
	if status := statusOf(t, m)["status"]; status != "reconfigure_rejected" {
		t.Fatalf("status = %v", status)
	}
}

func TestPollerStartsAndStops(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":50,"nextRegenAt":"2026-10-23T08:00:00Z","nextRegenPercent":2}}`))
	}))
	defer server.Close()
	m := NewModule()
	m.httpClient = server.Client()
	if err := m.Reconfigure(context.Background(), hostWith(syntheticCompats(testKey)...), json.RawMessage(`{"quota-url":"`+server.URL+`/v2/quotas"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := hits
		mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller never polled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { _ = m.Close(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the poller")
	}
}

func TestStaleReadingProjectsRegeneration(t *testing.T) {
	m, clock := newTestModule(t, "{}")
	// 1% remaining, next +2% tick in 10 minutes.
	_ = m.HandleUsage(context.Background(), usage(paidAuthID, http.StatusOK, quotaHeaderValue(1, testStart.Add(10*time.Minute))))
	if got := mustPick(t, m, pickReq(true, both()...)); got != infiniteAuthID {
		t.Fatalf("pick at 99%% used = %s", got)
	}
	// After the tick plus two more 202-minute ticks, 7% is back (93% used < 95%).
	clock.Advance(10*time.Minute + 2*regenInterval)
	if got := mustPick(t, m, pickReq(true, both()...)); got != paidAuthID {
		t.Fatalf("pick after projected regeneration = %s, want paid", got)
	}
}

func TestSummarizeBodyExtractsErrorText(t *testing.T) {
	cases := map[string]string{
		`{"error":"Weekly usage limit reached."}`:         "Weekly usage limit reached.",
		`{"error":{"message":"Rate limited","type":"x"}}`: "Rate limited",
		"plain\n text  body":                              "plain text body",
		`{"other":true}`:                                  `{"other":true}`,
	}
	for in, want := range cases {
		if got := summarizeBody(in); got != want {
			t.Fatalf("summarizeBody(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsageAccountAndPeakInFlight(t *testing.T) {
	m, _ := newTestModule(t, "{}")
	if got := m.UsageAccount(pluginapi.UsageRecord{AuthID: paidAuthID}); got != "synthetic#1" {
		t.Fatalf("UsageAccount(paid) = %q, want the ordinal lane name synthetic#1", got)
	}
	if got := m.UsageAccount(pluginapi.UsageRecord{AuthID: "unknown"}); got != "" {
		t.Fatalf("UsageAccount(unknown) = %q, want empty", got)
	}
	only := candidate(infiniteAuthID, "synthetic-infinite")
	mustPick(t, m, pickReq(true, only))
	mustPick(t, m, pickReq(true, only))
	_ = m.HandleUsage(context.Background(), usage(infiniteAuthID, http.StatusOK, nil))
	for _, item := range statusOf(t, m)["accounts"].([]any) {
		lane := item.(map[string]any)
		if lane["kind"] == "infinite" && (lane["peak_in_flight"].(float64) != 2 || lane["in_flight"].(float64) != 1 || lane["peak_in_flight_at"] == nil) {
			t.Fatalf("infinite lane = %v, want peak 2 and in-flight 1", lane)
		}
	}
}

func TestLaneNamesStayWithTheirCredentialWhenEntriesAreReordered(t *testing.T) {
	m, _ := newTestModule(t, "{}", twoAccountCompats()...)
	before := map[string]string{}
	m.mu.Lock()
	for id, item := range m.lanes {
		before[id] = item.Name
	}
	m.mu.Unlock()
	reordered := twoAccountCompats()
	for i := range reordered {
		entries := reordered[i].APIKeyEntries
		reordered[i].APIKeyEntries = []providermodule.OpenAICompatibilityAPIKey{entries[1], entries[0]}
	}
	if err := m.Reconfigure(context.Background(), hostWith(reordered...), json.RawMessage("{}")); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.lanes) != len(before) {
		t.Fatalf("lanes = %d, want %d", len(m.lanes), len(before))
	}
	for id, item := range m.lanes {
		if before[id] != item.Name {
			t.Fatalf("lane %s renamed %q -> %q after reordering the key entries; usage history would follow the wrong credential", id, before[id], item.Name)
		}
	}
}

func laneNames(t *testing.T, m *Module) map[string]string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	names := map[string]string{}
	for id, item := range m.lanes {
		names[id] = item.Name
	}
	return names
}

// shortHash is the 8-hex SHA-256 prefix a credential-derived name would use.
func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:8]
}

// credentialDerived lists every value computable from a credential and the
// public endpoint settings: the auth IDs, their hashes and the account
// fingerprint. None may appear in a lane name.
func credentialDerived(m *Module) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, item := range m.lanes {
		out = append(out, id, id[strings.LastIndex(id, ":")+1:], shortHash(id)[:6], shortHash(id))
		if item.Account != "" {
			out = append(out, item.Account, strings.TrimPrefix(item.Account, "acct-")[:6])
		}
	}
	return out
}

func TestPublicLaneNamesAreOrdinalsNotCredentialDerived(t *testing.T) {
	m, _ := newTestModule(t, "{}", twoAccountCompats()...)
	keys := twoAccountCompats()[1].APIKeyEntries
	ids := map[string]string{}
	gen := newStableIDGenerator()
	for _, provider := range []string{"synthetic-infinite", "synthetic"} {
		for i, entry := range keys {
			ids[gen.next("openai-compatibility:"+provider, entry.APIKey, testBase, "")] = fmt.Sprintf("%s#%d", provider, i+1)
		}
	}
	names := laneNames(t, m)
	if !reflect.DeepEqual(names, ids) {
		t.Fatalf("lane names = %v, want ordinals in config order %v", names, ids)
	}
	derived := credentialDerived(m)
	for _, name := range names {
		for _, value := range derived {
			if strings.Contains(name, value) {
				t.Fatalf("lane name %q contains credential-derived %q", name, value)
			}
		}
	}
	// Learned lanes are ordinals too, never a hash of the auth ID.
	learnedID := "openai-compatibility:synthetic:feedfacecafe"
	_ = m.HandleUsage(context.Background(), pluginapi.UsageRecord{AuthID: learnedID, Provider: "openai-compatible-synthetic"})
	if got := m.UsageAccount(pluginapi.UsageRecord{AuthID: learnedID}); got != "synthetic#learned-1" {
		t.Fatalf("learned lane name = %q, want synthetic#learned-1", got)
	}
	// The authenticated status still carries the private references.
	for _, item := range statusOf(t, m)["accounts"].([]any) {
		lane := item.(map[string]any)
		if lane["auth_id"] == "" || lane["auth_id"] == nil {
			t.Fatalf("authenticated status lost auth_id: %v", lane)
		}
	}
}

func newPersistentModule(t *testing.T, authDir string, compats ...providermodule.OpenAICompatibility) *Module {
	t.Helper()
	clock := &fakeClock{now: testStart}
	m := &Module{now: clock.Now, noPoller: true}
	host := hostWith(compats...)
	host.AuthDir = authDir
	if err := m.Reconfigure(context.Background(), host, json.RawMessage("{}")); err != nil {
		t.Fatalf("Reconfigure error = %v", err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func reorderedCompats() []providermodule.OpenAICompatibility {
	compats := twoAccountCompats()
	for i := range compats {
		entries := compats[i].APIKeyEntries
		compats[i].APIKeyEntries = []providermodule.OpenAICompatibilityAPIKey{entries[1], entries[0]}
	}
	return compats
}

func TestLaneNamesSurviveARestartAfterEntriesAreReordered(t *testing.T) {
	authDir := t.TempDir()
	first := newPersistentModule(t, authDir, twoAccountCompats()...)
	before := laneNames(t, first)
	_ = first.Close(context.Background())

	labels := filepath.Join(authDir, "providers", providerID, laneLabelsName)
	raw, err := os.ReadFile(labels)
	if err != nil {
		t.Fatalf("lane labels not saved: %v", err)
	}
	if info, _ := os.Stat(labels); info.Mode().Perm() != 0o600 {
		t.Fatalf("lane labels mode = %v, want 0600", info.Mode().Perm())
	}
	if strings.Contains(string(raw), testKey) {
		t.Fatal("lane labels file contains the API key")
	}

	restarted := newPersistentModule(t, authDir, reorderedCompats()...)
	if after := laneNames(t, restarted); !reflect.DeepEqual(before, after) {
		t.Fatalf("names after reorder + restart = %v, want %v", after, before)
	}
}

func TestANewCredentialTakesTheFreedOrdinal(t *testing.T) {
	authDir := t.TempDir()
	m := newPersistentModule(t, authDir, twoAccountCompats()...)
	before := laneNames(t, m)
	// Rotate the first key: the second keeps #2, the new key takes #1.
	rotated := twoAccountCompats()
	for i := range rotated {
		rotated[i].APIKeyEntries = []providermodule.OpenAICompatibilityAPIKey{{APIKey: testKey + "-rotated"}, rotated[i].APIKeyEntries[1]}
	}
	host := hostWith(rotated...)
	host.AuthDir = authDir
	if err := m.Reconfigure(context.Background(), host, json.RawMessage("{}")); err != nil {
		t.Fatal(err)
	}
	after := laneNames(t, m)
	want := map[string]bool{"synthetic#1": true, "synthetic#2": true, "synthetic-infinite#1": true, "synthetic-infinite#2": true}
	for id, name := range after {
		if !want[name] {
			t.Fatalf("lane %s named %q, want an ordinal 1 or 2", id, name)
		}
		delete(want, name)
		if old, kept := before[id]; kept && old != name {
			t.Fatalf("kept credential %s renamed %q -> %q", id, old, name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("ordinals not reused: %v left", want)
	}
}

func labelsPath(authDir string) string {
	return filepath.Join(authDir, "providers", providerID, laneLabelsName)
}

func writeLabels(t *testing.T, authDir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(labelsPath(authDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labelsPath(authDir), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func reconfigureIn(t *testing.T, m *Module, authDir string, compats ...providermodule.OpenAICompatibility) {
	t.Helper()
	host := hostWith(compats...)
	host.AuthDir = authDir
	if err := m.Reconfigure(context.Background(), host, json.RawMessage("{}")); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableLaneLabelsAreKeptAndRetried(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":        "{not json",
		"newer version":  `{"version":2,"lanes":{}}`,
		"unknown fields": `{"version":1,"lanes":{},"extra":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			writeLabels(t, authDir, content)
			m := newPersistentModule(t, authDir, twoAccountCompats()...)
			names := map[string]bool{}
			for _, lane := range laneNames(t, m) {
				names[lane] = true
			}
			if len(names) != 4 || !names["synthetic#1"] || !names["synthetic#2"] || !names["synthetic-infinite#1"] || !names["synthetic-infinite#2"] {
				t.Fatalf("names = %v, want ordinals while the file is unreadable", names)
			}
			if got, _ := statusOf(t, m)["lane_labels_error"].(string); !strings.Contains(got, "read lane labels") {
				t.Fatalf("lane_labels_error = %q, want the read failure on the authenticated status", got)
			}
			reconfigureIn(t, m, authDir, reorderedCompats()...)
			if raw, _ := os.ReadFile(labelsPath(authDir)); string(raw) != content {
				t.Fatalf("an unreadable labels file was overwritten with %q", raw)
			}
			// Once the operator removes it, the next reconfigure saves.
			if err := os.Remove(labelsPath(authDir)); err != nil {
				t.Fatal(err)
			}
			reconfigureIn(t, m, authDir, twoAccountCompats()...)
			if got, _ := statusOf(t, m)["lane_labels_error"].(string); got != "" {
				t.Fatalf("lane_labels_error = %q after the file was removed", got)
			}
			if _, err := os.Stat(labelsPath(authDir)); err != nil {
				t.Fatalf("labels not saved after recovery: %v", err)
			}
		})
	}
}

func TestSavedLaneLabelsAreValidated(t *testing.T) {
	authDir := t.TempDir()
	gen := newStableIDGenerator()
	first := gen.next("openai-compatibility:synthetic", testKey, testBase, "")
	second := gen.next("openai-compatibility:synthetic", testKey+"-second", testBase, "")
	infinite := newStableIDGenerator().next("openai-compatibility:synthetic-infinite", testKey, testBase, "")
	// second claims #7; first claims #7 too (duplicate) and the infinite lane
	// claims the paid provider (mismatch); a zero ordinal and an empty ID
	// are invalid.
	writeLabels(t, authDir, fmt.Sprintf(`{"version":1,"lanes":{%q:{"provider":"synthetic","ordinal":7},%q:{"provider":"synthetic","ordinal":7},%q:{"provider":"synthetic","ordinal":3},"x":{"provider":"synthetic","ordinal":0},"":{"provider":"synthetic","ordinal":4}}}`, second, first, infinite))
	m := newPersistentModule(t, authDir, twoAccountCompats()...)
	names := laneNames(t, m)
	// Config order lists first before second, so first keeps the contested
	// #7 and second takes the lowest free ordinal.
	if names[first] != "synthetic#7" || names[second] != "synthetic#1" || names[infinite] != "synthetic-infinite#1" {
		t.Fatalf("names = %v", names)
	}
	var saved laneLabelsFile
	raw, _ := os.ReadFile(labelsPath(authDir))
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Lanes) != 4 || saved.Lanes[first].Ordinal != 7 || saved.Lanes[second].Ordinal != 1 || saved.Lanes[infinite].Provider != "synthetic-infinite" {
		t.Fatalf("saved labels = %+v", saved.Lanes)
	}
}

func TestFailedLaneLabelsSaveIsRetried(t *testing.T) {
	authDir := t.TempDir()
	// A regular file where the module's directory belongs breaks the store.
	if err := os.MkdirAll(filepath.Join(authDir, "providers"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(authDir, "providers", providerID)
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newPersistentModule(t, authDir, twoAccountCompats()...)
	if got, _ := statusOf(t, m)["lane_labels_error"].(string); got == "" {
		t.Fatal("a broken label store reported no error")
	}
	before := laneNames(t, m)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	// Same config: nothing changed, but the assignment was never saved.
	reconfigureIn(t, m, authDir, twoAccountCompats()...)
	if got, _ := statusOf(t, m)["lane_labels_error"].(string); got != "" {
		t.Fatalf("lane_labels_error = %q after the store recovered", got)
	}
	restarted := newPersistentModule(t, authDir, reorderedCompats()...)
	if after := laneNames(t, restarted); !reflect.DeepEqual(before, after) {
		t.Fatalf("names after recovery + restart = %v, want %v", after, before)
	}
}

func TestANewAuthDirectorysSavedLabelsWin(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	m := newPersistentModule(t, first, twoAccountCompats()...)
	before := laneNames(t, m)
	var swapped []string
	for id, name := range before {
		if strings.HasPrefix(name, "synthetic#") {
			swapped = append(swapped, id)
		}
	}
	if len(swapped) != 2 {
		t.Fatalf("paid lanes = %v", before)
	}
	// The second directory saved the two paid credentials the other way round.
	writeLabels(t, second, fmt.Sprintf(`{"version":1,"lanes":{%q:{"provider":"synthetic","ordinal":%d},%q:{"provider":"synthetic","ordinal":%d}}}`,
		swapped[0], 3-mustOrdinal(t, before[swapped[0]]), swapped[1], 3-mustOrdinal(t, before[swapped[1]])))
	reconfigureIn(t, m, second, twoAccountCompats()...)
	after := laneNames(t, m)
	for _, id := range swapped {
		if want := fmt.Sprintf("synthetic#%d", 3-mustOrdinal(t, before[id])); after[id] != want {
			t.Fatalf("lane %s = %q in the new directory, want its saved %q", id, after[id], want)
		}
	}
}

func mustOrdinal(t *testing.T, name string) int {
	t.Helper()
	var ordinal int
	if _, err := fmt.Sscanf(name[strings.LastIndex(name, "#")+1:], "%d", &ordinal); err != nil {
		t.Fatalf("lane name %q has no ordinal", name)
	}
	return ordinal
}
