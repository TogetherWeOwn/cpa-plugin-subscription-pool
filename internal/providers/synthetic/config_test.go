package synthetic

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	for _, raw := range []string{"", "null", "{}"} {
		cfg, err := parseConfig(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("parseConfig(%q) error = %v", raw, err)
		}
		if !cfg.Enabled || cfg.ThresholdPercent != 95 || cfg.QuotaRefresh != 2*time.Minute {
			t.Fatalf("defaults = %+v", cfg)
		}
		if len(cfg.PaidProviders) != 1 || cfg.PaidProviders[0] != "synthetic" || len(cfg.InfiniteProviders) != 1 || cfg.InfiniteProviders[0] != "synthetic-infinite" {
			t.Fatalf("default providers = %v / %v", cfg.PaidProviders, cfg.InfiniteProviders)
		}
		if cfg.MaxConcurrent != 0 || cfg.InfiniteMaxConcurrent != 0 {
			t.Fatalf("default concurrency caps must be off, got %d/%d", cfg.MaxConcurrent, cfg.InfiniteMaxConcurrent)
		}
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"enabled":false,"threshold-percent":90,"quota-refresh-interval":"0","failure-cooldown":"5m","infinite-cooldown":"10s","in-flight-ttl":"45m","max-concurrent":3,"infinite-max-concurrent":8,"paid-providers":["Synthetic"," synthetic "],"infinite-providers":["Synthetic-Infinite"],"quota-url":"http://mock:8080/v2/quotas"}`))
	if err != nil {
		t.Fatalf("parseConfig error = %v", err)
	}
	if cfg.Enabled || cfg.ThresholdPercent != 90 || cfg.QuotaRefresh != 0 || cfg.FailureCooldown != 5*time.Minute || cfg.InfiniteCooldown != 10*time.Second || cfg.InFlightTTL != 45*time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.MaxConcurrent != 3 || cfg.InfiniteMaxConcurrent != 8 {
		t.Fatalf("caps = %d/%d", cfg.MaxConcurrent, cfg.InfiniteMaxConcurrent)
	}
	if len(cfg.PaidProviders) != 1 || cfg.PaidProviders[0] != "synthetic" || cfg.InfiniteProviders[0] != "synthetic-infinite" {
		t.Fatalf("providers not normalized: %v %v", cfg.PaidProviders, cfg.InfiniteProviders)
	}
	if cfg.QuotaURL != "http://mock:8080/v2/quotas" {
		t.Fatalf("quota url = %q", cfg.QuotaURL)
	}
}

func TestParseConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown field":        `{"thresold-percent":90}`,
		"threshold zero":       `{"threshold-percent":0}`,
		"threshold over 100":   `{"threshold-percent":101}`,
		"refresh too short":    `{"quota-refresh-interval":"5s"}`,
		"bad duration":         `{"failure-cooldown":"soon"}`,
		"zero cooldown":        `{"failure-cooldown":"0"}`,
		"negative concurrency": `{"max-concurrent":-1}`,
		"overlapping names":    `{"paid-providers":["synthetic"],"infinite-providers":["synthetic"]}`,
		"no providers":         `{"paid-providers":[],"infinite-providers":[]}`,
		"relative quota url":   `{"quota-url":"/v2/quotas"}`,
	}
	for name, raw := range cases {
		if _, err := parseConfig(json.RawMessage(raw)); err == nil || !strings.HasPrefix(err.Error(), "synthetic_invalid_config") {
			t.Fatalf("%s: err = %v, want synthetic_invalid_config", name, err)
		}
	}
}

func TestQuotaURLForDerivesOrigin(t *testing.T) {
	cfg := defaultConfig()
	if got := quotaURLFor(cfg, "https://api.synthetic.new/openai/v1"); got != "https://api.synthetic.new/v2/quotas" {
		t.Fatalf("quotaURLFor = %q", got)
	}
	if got := quotaURLFor(cfg, "not a url"); got != "" {
		t.Fatalf("quotaURLFor(invalid) = %q, want empty", got)
	}
	cfg.QuotaURL = "http://override/v2/quotas"
	if got := quotaURLFor(cfg, "https://api.synthetic.new/openai/v1"); got != cfg.QuotaURL {
		t.Fatalf("override ignored: %q", got)
	}
}
