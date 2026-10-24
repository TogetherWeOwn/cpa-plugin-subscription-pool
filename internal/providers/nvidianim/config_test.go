package nvidianim

import (
	"encoding/json"
	"testing"
)

func TestFreeGuardDefaultsOnWithNoInventedAllowance(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || !cfg.StayWithinFree || cfg.FreeTierConfirmed || cfg.RequestsPerMinute != 0 || cfg.FreeRequestLimit != 0 || len(cfg.FreeModels) != 0 {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
}

func TestExplicitPaygOptOutAndFreeSettings(t *testing.T) {
	for _, setting := range []string{`"stayWithinFree":false`, `"stay-within-free":false`} {
		cfg, err := parseConfig(json.RawMessage(`{` + setting + `,"requests-per-minute":40,"free-request-limit":100,"free-models":["moonshotai/kimi-k3"],"trial-expires-at":"2026-11-01T00:00:00Z"}`))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StayWithinFree || cfg.RequestsPerMinute != 40 || cfg.FreeRequestLimit != 100 || cfg.TrialExpiresAt.IsZero() || !cfg.FreeModels["moonshotai/kimi-k3"] {
			t.Fatalf("config = %+v", cfg)
		}
	}
}

func TestInvalidConfigFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`{"stayWithinFree":"false"}`, `{"stayWithinFree":false,"stay-within-free":true}`,
		`{"requests-per-minute":-1}`, `{"requests-per-minute":100001}`, `{"free-request-limit":-1}`,
		`{"trial-expires-at":"tomorrow"}`, `{"free-models":[""]}`, `{"free-models":["foo\nbar"]}`,
		`{"enabled":true,"unknown":1}`, `{} {}`, `[]`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseConfig(json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}
