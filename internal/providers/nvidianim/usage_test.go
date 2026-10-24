package nvidianim

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestReservationsSurviveReloadRestartAndKeyReplacement(t *testing.T) {
	h := hostConfig(t)
	cfg := strings.TrimSuffix(freeConfig, "}") + `,"free-request-limit":1}`
	m := setup(t, h, cfg)
	assertPick(t, m, req(m), true)
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	assertPick(t, m, req(m), false)
	_ = m.Close(context.Background())
	h.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "another-test-only-key"
	next := setup(t, h, cfg)
	next.now = func() time.Time { return start.Add(time.Hour) }
	assertPick(t, next, req(next), false)
}
func TestAllKeysShareRateAllowance(t *testing.T) {
	h := hostConfig(t)
	h.OpenAICompatibility[0].APIKeyEntries = append(h.OpenAICompatibility[0].APIKeyEntries, providermodule.OpenAICompatibilityAPIKey{APIKey: "second-test-only-key"})
	m := setup(t, h, freeConfig)
	r := req(m)
	assertPick(t, m, r, true)
	r.Candidates[0].ID = m.OwnedAuthIDs(context.Background())[1]
	assertPick(t, m, r, true)
	assertPick(t, m, r, false)
}
func TestRate429RetryAfterAndCreditFailureAreDifferent(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	failure := pluginapi.UsageRecord{AuthID: r.Candidates[0].ID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":"Too many requests"}`}, ResponseHeaders: http.Header{"Retry-After": {"120"}}}
	if err := m.HandleUsage(context.Background(), failure); err != nil {
		t.Fatal(err)
	}
	assertPick(t, m, r, false)
	m.now = func() time.Time { return start.Add(119 * time.Second) }
	assertPick(t, m, r, false)
	m.now = func() time.Time { return start.Add(120 * time.Second) }
	assertPick(t, m, r, true)
	failure.Failure = pluginapi.UsageFailure{StatusCode: 402, Body: `{"error":"Credit balance exhausted"}`}
	if err := m.HandleUsage(context.Background(), failure); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return start.Add(24 * time.Hour) }
	assertPick(t, m, r, false)
}
func TestCreditOrTrialWorded429LatchesUntilOperatorClears(t *testing.T) {
	for _, body := range []string{`{"error":"Insufficient credits"}`, `{"detail":"Your TRIAL has ended"}`, `{"error":"daily quota of free credits used"}`} {
		h := hostConfig(t)
		m := setup(t, h, freeConfig)
		r := req(m)
		failure := pluginapi.UsageRecord{AuthID: r.Candidates[0].ID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: body}, ResponseHeaders: http.Header{"Retry-After": {"5"}}}
		if err := m.HandleUsage(context.Background(), failure); err != nil {
			t.Fatal(err)
		}
		_ = m.Close(context.Background())
		next := setup(t, h, freeConfig)
		next.now = func() time.Time { return start.Add(30 * 24 * time.Hour) }
		if got := next.reason(next.lanes[r.Candidates[0].ID], r.Model, next.now()); got != "free_exhausted" {
			t.Fatalf("%s: reason = %q, want sticky free_exhausted", body, got)
		}
		assertPick(t, next, req(next), false)
	}
}

func TestQuotaWorded429BacksOffDurablyWithoutPermanentLatch(t *testing.T) {
	for _, body := range []string{`{"error":"Rate limit exceeded: 40 requests per minute quota"}`, `{"error":"daily limit reached"}`, `monthly quota`} {
		h := hostConfig(t)
		m := setup(t, h, freeConfig)
		r := req(m)
		failure := pluginapi.UsageRecord{AuthID: r.Candidates[0].ID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: body}, ResponseHeaders: http.Header{"Retry-After": {"5"}}}
		if err := m.HandleUsage(context.Background(), failure); err != nil {
			t.Fatal(err)
		}
		m.now = func() time.Time { return start.Add(time.Hour - time.Second) }
		assertPick(t, m, r, false)
		_ = m.Close(context.Background())
		next := setup(t, h, freeConfig)
		next.now = func() time.Time { return start.Add(time.Hour - time.Second) }
		assertPick(t, next, req(next), false) // the backoff is durable across restart
		next.now = func() time.Time { return start.Add(time.Hour) }
		assertPick(t, next, req(next), true) // ...but bounded: no permanent latch
		if next.state.FreeExhausted {
			t.Fatal("worded 429 latched free exhaustion")
		}
	}
}

func TestLongerRetryAfterWinsOverQuotaBackoff(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	if err := m.HandleUsage(context.Background(), pluginapi.UsageRecord{AuthID: r.Candidates[0].ID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `daily quota`}, ResponseHeaders: http.Header{"Retry-After": {"7200"}}}); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return start.Add(2*time.Hour - time.Second) }
	assertPick(t, m, r, false)
	m.now = func() time.Time { return start.Add(2 * time.Hour) }
	assertPick(t, m, r, true)
}
func TestPublicStatusIsRedactedAndHonestAboutLocalCounts(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	assertPick(t, m, r, true)
	b, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		StayWithinFree  bool   `json:"stay_within_free"`
		Reserved        int64  `json:"reserved_requests"`
		RateUsed        int    `json:"rate_used"`
		AllowanceSource string `json:"allowance_source"`
		Accounts        []any  `json:"accounts"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if !s.StayWithinFree || s.Reserved != 1 || s.RateUsed != 1 || s.AllowanceSource != "operator_confirmed" || len(s.Accounts) != 1 {
		t.Fatalf("status = %s", b)
	}
	for _, secret := range []string{"test-only-nvidia-key", r.Candidates[0].ID, "fingerprint", "api_key", "moonshotai"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("status leaked sensitive/config identity %q", secret)
		}
	}
}
func TestInvalidReconfigureKeepsPriorGuardAndCorruptStateBlocksStartup(t *testing.T) {
	h := hostConfig(t)
	m := setup(t, h, freeConfig)
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"stayWithinFree":"bad"}`)); err == nil {
		t.Fatal("accepted invalid config")
	}
	assertPick(t, m, req(m), true)
	_ = m.Close(context.Background())
	p := filepath.Join(h.AuthDir, "providers", providerID, "allowance.state")
	if err := os.WriteFile(p, []byte(`{"version":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	n := NewModule()
	defer func() {
		if err := n.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := n.Reconfigure(context.Background(), h, json.RawMessage(freeConfig)); err == nil {
		t.Fatal("accepted corrupt allowance state")
	}
}
func TestEmptyModelCannotBypassAllowlist(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	r.Model = ""
	assertPick(t, m, r, false)
}

func TestUnexpandedEnvironmentKeyIsNotBound(t *testing.T) {
	h := hostConfig(t)
	h.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "${NVIDIA_NIM_API_KEY}"
	m := setup(t, h, freeConfig)
	assertPick(t, m, req(m), false)
}

func TestDurableWriteFailureBlocksBothFreeAndPaid(t *testing.T) {
	for _, cfg := range []string{freeConfig, `{"stayWithinFree":false}`} {
		h := hostConfig(t)
		m := setup(t, h, cfg)
		p := filepath.Join(h.AuthDir, "providers", providerID, "allowance.state")
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		assertPick(t, m, req(m), false)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		assertPick(t, m, req(m), false) // do not silently recover after an unpersisted admission
	}
}

func TestObservedCreditExhaustionSurvivesRestart(t *testing.T) {
	h := hostConfig(t)
	m := setup(t, h, freeConfig)
	r := req(m)
	if err := m.HandleUsage(context.Background(), pluginapi.UsageRecord{AuthID: r.Candidates[0].ID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 402}}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close(context.Background())
	next := setup(t, h, freeConfig)
	next.now = func() time.Time { return start.Add(40 * 24 * time.Hour) }
	assertPick(t, next, req(next), false)
}
