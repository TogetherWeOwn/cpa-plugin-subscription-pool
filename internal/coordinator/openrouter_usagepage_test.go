package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

const openrouterStatus = `{"provider":"openrouter-free","status":"registered","stay_within_free":true,
"models":["google/gemma-4-31b-it:free","nvidia/nemotron-3.5-lightning:free"],"model_count":2,
"daily_limit":50,"daily_limit_source":"openrouter_key","used_today":50,"day_resets_at":"2026-10-26T00:00:00Z",
"requests_per_minute":20,"rate_used":3,"last_sync_error":"catalog_http_503","host_sync":"synced",
"recent_changes":[{"at":"2026-10-25T01:00:00Z","action":"added","model":"google/gemma-4-31b-it:free"}],
"accounts":[{"name":"openrouter-free#1","health":"exhausted","reason":"daily_limit"}]}`

func TestOpenRouterCardShowsLimitsButNotTheModelListOrDiagnostics(t *testing.T) {
	cards := openrouterQuotaCards(json.RawMessage(openrouterStatus))
	if len(cards) != 1 {
		t.Fatalf("cards = %+v", cards)
	}
	c := cards[0]
	if c.ProviderLabel != "OpenRouter free models" || c.Health != "exhausted" || c.Reason != "daily_limit" {
		t.Fatalf("card = %+v", c)
	}
	if len(c.Windows) != 2 || c.Windows[0].ID != "local_daily" || !c.Windows[0].Exhausted || c.Windows[0].Authoritative || c.Windows[1].Mode != "rolling" {
		t.Fatalf("windows = %+v", c.Windows)
	}
	raw, _ := json.Marshal(c)
	for _, leaked := range []string{"gemma", "nemotron", "catalog_http_503", "recent_changes"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("public card leaked %q: %s", leaked, raw)
		}
	}
	var count float64
	for _, m := range c.Metrics {
		if m.Key == "free_models" {
			count = m.Value
		}
	}
	if count != 2 {
		t.Fatalf("free_models metric = %v", count)
	}
}

func TestOpenRouterCardCollapsesUnknownReasonsAndSkipsInactiveModule(t *testing.T) {
	if got := openrouterReasonCode("some upstream text"); got != "other" {
		t.Fatalf("reason = %q", got)
	}
	if cards := openrouterQuotaCards(json.RawMessage(`{"status":"inactive","accounts":[]}`)); cards != nil {
		t.Fatalf("inactive module produced cards: %+v", cards)
	}
}
