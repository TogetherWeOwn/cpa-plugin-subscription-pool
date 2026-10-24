package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testHost(dir string) providermodule.HostConfig {
	return providermodule.HostConfig{AuthDir: dir, OpenAICompatibility: []providermodule.OpenAICompatibility{{
		Name: providerID, BaseURL: "https://api.cloudflare.com/client/v4/accounts/" + testAccount + "/ai/v1",
		APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: "test-token-one"}, {APIKey: "test-token-two"}},
		Models: []providermodule.OpenAICompatibilityModel{
			{Name: "@cf/qwen/qwen3-30b-a3b-fp8", Alias: providerID + "/qwen3-30b-a3b-fp8"},
			{Name: "@cf/zai-org/glm-4.7-flash", Alias: "glm-4.7-flash"},
		},
	}}}
}

func configured(t *testing.T, raw string) (*Module, *time.Time, providermodule.HostConfig) {
	t.Helper()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	m := NewModule()
	m.noPoller = true
	m.now = func() time.Time { return now }
	h := testHost(t.TempDir())
	h.HTTPClient = doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(analyticsBody(`[{"sum":{"totalNeurons":0}}]`)))}, nil
	})
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, &now, h
}

func pickRequest(m *Module, which int) pluginapi.SchedulerPickRequest {
	ids := m.OwnedAuthIDs(context.Background())
	return pluginapi.SchedulerPickRequest{Model: providerID + "/qwen3-30b-a3b-fp8", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: ids[which], Provider: "openai-compatible-" + providerID}}}
}

func TestIncludedOnlyUnknownStaleAndBoundary(t *testing.T) {
	m, now, _ := configured(t, `{}`)
	r := pickRequest(m, 0)
	if _, err := m.Pick(context.Background(), r); err == nil {
		t.Fatal("unknown balance admitted")
	}
	m.pollOnce(context.Background(), true)
	cost, _ := reservationFor(r.Model)
	m.mu.Lock()
	a := m.accounts[testAccount]
	a.remote = includedDailyNeurons - m.cfg.SafetyMargin - cost
	m.mu.Unlock()
	if got, err := m.Pick(context.Background(), r); err != nil || !got.Handled {
		t.Fatalf("exact remaining reservation should fit: %+v %v", got, err)
	}
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err == nil {
		t.Fatal("second key bypassed shared account reservation")
	}
	*now = now.Add(3 * time.Minute)
	if _, err := m.Pick(context.Background(), r); err == nil {
		t.Fatal("stale balance admitted")
	}
}

func TestTogglePAYGBypassesIncludedGateButNotUnknownCredential(t *testing.T) {
	m, _, h := configured(t, `{}`)
	r := pickRequest(m, 0)
	m.pollOnce(context.Background(), true)
	m.mu.Lock()
	m.accounts[testAccount].remote = 12000
	m.mu.Unlock()
	if _, err := m.Pick(context.Background(), r); err == nil {
		t.Fatal("overage admitted")
	}
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"stayWithinIncluded":false}`)); err != nil {
		t.Fatal(err)
	}
	r.Model = "unpriced-new-model"
	if got, err := m.Pick(context.Background(), r); err != nil || !got.Handled {
		t.Fatalf("PAYG blocked: %v", err)
	}
	r.Candidates[0].ID = "unbound-after-key-change"
	if !m.Recognize(r.Candidates[0]) {
		t.Fatal("must claim and fail closed, not fall through to host")
	}
	if _, err := m.Pick(context.Background(), r); err == nil {
		t.Fatal("unbound key admitted")
	}
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("reenabling guard lost exhausted balance")
	}
}

func TestNoAutoResetAtMidnightUntilFreshReading(t *testing.T) {
	m, now, _ := configured(t, `{}`)
	m.pollOnce(context.Background(), true)
	r := pickRequest(m, 0)
	if _, err := m.Pick(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	*now = utcDay(*now).Add(24 * time.Hour)
	if _, err := m.Pick(context.Background(), r); err == nil {
		t.Fatal("yesterday reading became today's allowance")
	}
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	b := m.budgets[testAccount]
	cost, _ := reservationFor(r.Model)
	if b.Committed != 2*cost {
		t.Errorf("pending midnight request not reserved: %+v", b)
	}
	m.mu.Unlock()
}

func TestConcurrentReservationsAndRestartCannotOverspend(t *testing.T) {
	m, now, h := configured(t, `{}`)
	m.pollOnce(context.Background(), true)
	var wg sync.WaitGroup
	success := 0
	var mu sync.Mutex
	r := pickRequest(m, 0)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Pick(context.Background(), r); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	cost, _ := reservationFor(r.Model)
	if success != int(9500/cost) {
		t.Fatalf("admitted %d, reserve %v", success, cost)
	}
	_ = m.Close(context.Background())
	m2 := NewModule()
	m2.noPoller = true
	m2.now = func() time.Time { return *now }
	defer func() {
		if err := m2.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := m2.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	m2.pollOnce(context.Background(), true)
	if _, err := m2.Pick(context.Background(), pickRequest(m2, 1)); err == nil {
		t.Fatal("restart or second key erased reservations")
	}
	infoErr := false
	state, err := filepath.Glob(filepath.Join(h.AuthDir, "providers", providerID, "*.json"))
	if err != nil || len(state) != 1 {
		infoErr = true
	}
	if infoErr {
		t.Fatal("missing durable budget")
	}
}

func TestPollFailureAndUnknownModelFailClosed(t *testing.T) {
	m, _, h := configured(t, `{}`)
	m.pollOnce(context.Background(), true)
	r := pickRequest(m, 0)
	for _, model := range []string{"new-unpriced-model", providerID + "/glm-5.3", providerID + "/gpt-oss-120b"} {
		r.Model = model
		if _, err := m.Pick(context.Background(), r); err == nil {
			t.Fatalf("unbounded or too-large request admitted: %s", model)
		}
	}
	h.HTTPClient = doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("SECRET"))}, nil
	})
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("API error became capacity")
	}
	raw, err := m.Status(context.Background())
	if err != nil || strings.Contains(string(raw), "test-token") || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("unsafe status %s %v", raw, err)
	}
}

func TestAbsentDisabledAndInvalidReconfigure(t *testing.T) {
	m := NewModule()
	m.noPoller = true
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := m.Reconfigure(context.Background(), providermodule.HostConfig{}, nil); err != nil || len(m.OwnedAuthIDs(context.Background())) != 0 {
		t.Fatalf("absent provider breaks other modules: %v", err)
	}
	h := testHost(t.TempDir())
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	ids := m.OwnedAuthIDs(context.Background())
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"safety-margin-neurons":-1}`)); err == nil {
		t.Fatal("invalid config accepted")
	}
	if len(m.OwnedAuthIDs(context.Background())) != len(ids) {
		t.Fatal("invalid reconfigure was not atomic")
	}
	h.OpenAICompatibility[0].Disabled = true
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil || len(m.OwnedAuthIDs(context.Background())) != 0 {
		t.Fatalf("disabled provider active: %v", err)
	}
}
