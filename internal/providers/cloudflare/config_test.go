package cloudflare

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConfigSafeDefaults(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{}`))
	if err != nil || !cfg.StayWithinIncluded || cfg.SafetyMargin != 500 || cfg.Refresh != 30*time.Second {
		t.Fatalf("unsafe defaults: %+v, %v", cfg, err)
	}
	cfg, err = parseConfig(json.RawMessage(`{"stayWithinIncluded":false}`))
	if err != nil || cfg.StayWithinIncluded {
		t.Fatalf("PAYG toggle ignored: %+v, %v", cfg, err)
	}
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	for _, raw := range []string{
		`{"stayWithinIncluded":"false"}`, `{"stayWithinIncluded":null}`,
		`{"safety-margin-neurons":-1}`, `{"safety-margin-neurons":10000}`,
		`{"usage-refresh-interval":"0"}`, `{"usage-max-age":"5s"}`,
		`{"providers":["synthetic"]}`, `{"unknown":true}`, `{} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseConfig(json.RawMessage(raw)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestAccountURLStrict(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	if got, err := accountID("https://api.cloudflare.com/client/v4/accounts/" + id + "/ai/v1"); err != nil || got != id {
		t.Fatalf("%q %v", got, err)
	}
	for _, u := range []string{
		"http://api.cloudflare.com/client/v4/accounts/" + id + "/ai/v1",
		"https://api.cloudflare.com.evil.test/client/v4/accounts/" + id + "/ai/v1",
		"https://token@api.cloudflare.com/client/v4/accounts/" + id + "/ai/v1",
		"https://api.cloudflare.com/client/v4/accounts/no-account/ai/v1",
		"https://api.cloudflare.com/client/v4/accounts/" + id + "/ai/v1?token=x",
	} {
		if _, err := accountID(u); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
}

func TestCalendarQuoteUsesDailyNotMonthlyPool(t *testing.T) {
	for _, tc := range []struct {
		at   string
		days int
	}{{"2026-10-24T12:00:00Z", 31}, {"2026-02-01T12:00:00Z", 28}, {"2028-02-01T12:00:00Z", 29}} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		q := calendarQuote(at)
		if q.Days != tc.days || q.IncludedNeurons != float64(tc.days)*10000 || q.IncludedValueUSD != float64(tc.days)*0.11 {
			t.Fatalf("bad quote: %+v", q)
		}
	}
}
