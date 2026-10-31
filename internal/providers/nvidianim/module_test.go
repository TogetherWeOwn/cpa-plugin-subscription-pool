package nvidianim

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var start = time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)

const freeConfig = `{"free-tier-confirmed":true,"requests-per-minute":2,"free-models":["moonshotai/kimi-k3"]}`

func hostConfig(t *testing.T) providermodule.HostConfig {
	t.Helper()
	return providermodule.HostConfig{AuthDir: t.TempDir(), OpenAICompatibility: []providermodule.OpenAICompatibility{{Name: "nvidia-nim", BaseURL: "https://integrate.api.nvidia.com/v1", APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: "test-only-nvidia-key"}}, Models: []providermodule.OpenAICompatibilityModel{{Name: "moonshotai/kimi-k3", Alias: "nvidia-nim/kimi-k3"}}}}}
}
func setup(t *testing.T, host providermodule.HostConfig, cfg string) *Module {
	t.Helper()
	m := NewModule()
	m.now = func() time.Time { return start }
	if err := m.Reconfigure(context.Background(), host, json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}
func req(m *Module) pluginapi.SchedulerPickRequest {
	ids := m.OwnedAuthIDs(context.Background())
	return pluginapi.SchedulerPickRequest{Model: "nvidia-nim/kimi-k3", Stream: true, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: ids[0], Provider: "openai-compatible-nvidia-nim", Attributes: map[string]string{"compat_name": "nvidia-nim"}}}}
}
func assertPick(t *testing.T, m *Module, r pluginapi.SchedulerPickRequest, want bool) {
	t.Helper()
	p, e := m.Pick(context.Background(), r)
	if want && (e != nil || !p.Handled || p.AuthID == "") {
		t.Fatalf("expected admission, got %+v %v", p, e)
	}
	if !want && e == nil {
		t.Fatalf("expected fail-closed, got %+v", p)
	}
}
func TestVerifiedFreeEndpointReservesRateAtPick(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	assertPick(t, m, r, true)
	assertPick(t, m, r, true)
	assertPick(t, m, r, false)
	m.now = func() time.Time { return start.Add(time.Minute) }
	assertPick(t, m, r, true)
}
func TestUnknownFreeEntitlementOrModelNeverAdmitted(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"requests-per-minute":2}`, `{"free-tier-confirmed":true,"free-models":["moonshotai/kimi-k3"]}`, `{"free-tier-confirmed":true,"requests-per-minute":2}`} {
		m := setup(t, hostConfig(t), cfg)
		assertPick(t, m, req(m), false)
	}
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	r.Model = "paid-model"
	assertPick(t, m, r, false)
}
func TestFreeOnlyRejectsWrongEndpointPlaceholderAndUnknownAuth(t *testing.T) {
	for _, kind := range []string{"endpoint", "placeholder", "unknown-auth"} {
		t.Run(kind, func(t *testing.T) {
			h := hostConfig(t)
			if kind == "endpoint" {
				h.OpenAICompatibility[0].BaseURL = "https://paid.example/v1"
			}
			if kind == "placeholder" {
				h.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "<API KEY PLACEHOLDER>"
			}
			m := setup(t, h, freeConfig)
			r := req(m)
			if kind == "unknown-auth" {
				r.Candidates[0].ID = "new-unknown-credential"
			}
			assertPick(t, m, r, false)
		})
	}
}
func TestPaygOptOutSkipsOnlyTheFreeGuard(t *testing.T) {
	h := hostConfig(t)
	h.OpenAICompatibility[0].BaseURL = "https://paid.example/v1"
	m := setup(t, h, `{"stayWithinFree":false,"requests-per-minute":1,"free-request-limit":1,"trial-expires-at":"2020-01-01T00:00:00Z"}`)
	r := req(m)
	r.Model = "paid-model"
	assertPick(t, m, r, true)
	assertPick(t, m, r, false)
}
func TestExpiredTrialAndLocalCeilingDoNotResetOnMinuteBoundary(t *testing.T) {
	m := setup(t, hostConfig(t), strings.TrimSuffix(freeConfig, "}")+`,"free-request-limit":1}`)
	r := req(m)
	assertPick(t, m, r, true)
	m.now = func() time.Time { return start.Add(2 * time.Minute) }
	assertPick(t, m, r, false)
	exp := setup(t, hostConfig(t), strings.TrimSuffix(freeConfig, "}")+`,"trial-expires-at":"2026-10-24T01:00:00Z"}`)
	assertPick(t, exp, req(exp), false)
}
func TestRateReservationsAreAtomicAcrossConcurrentPicks(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := m.Pick(context.Background(), r)
			if e == nil && p.Handled {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("admitted %d, want exactly 2", ok)
	}
}
func TestMixedForeignCandidateFailsClosed(t *testing.T) {
	m := setup(t, hostConfig(t), freeConfig)
	r := req(m)
	r.Candidates = append(r.Candidates, pluginapi.SchedulerAuthCandidate{ID: "foreign-auth", Provider: "openai-compatible-other"})
	assertPick(t, m, r, false)
}
