package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCloudflareQuotaPageProjectsDailyGuardNotMonthlyBank(t *testing.T) {
	raw := json.RawMessage(`{"provider":"cloudflare-workers-ai","status":"registered","stayWithinIncluded":true,"included_daily_neurons":10000,"safety_margin_neurons":500,"calendar_month_quote":{"days_in_calendar_month":31,"included_neurons_across_daily_windows":310000,"included_value_usd":3.41,"usd_per_thousand_neurons":0.011},"accounts":[{"name":"workers-ai#1","credential_bound":true,"health":"exhausted","reason":"included_exhausted","metered_neurons":7400,"reserved_neurons":1151,"windows":{"daily":{"known":true,"utilization":0.8551,"exhausted":true,"resets_at":"2026-10-25T00:00:00Z","authoritative":false}}}],"account_id":"PRIVATE-ACCOUNT","token":"PRIVATE-TOKEN","observation_gaps":["PRIVATE-UPSTREAM"]}`)
	m := &fakeStatusModule{fakeModule: fakeModule{id: "cloudflare-workers-ai"}, status: raw}
	c := New(m)
	data := c.buildUsagePageData("24h", false)
	if len(data.Quota) != 1 || len(data.Providers) != 1 || data.Providers[0].Label != "Cloudflare Workers AI" {
		t.Fatalf("missing Cloudflare quota cards: %+v", data.Quota)
	}
	card := data.Quota[0]
	if card.Plan != "Included-only (daily; no rollover)" || card.Reason != "included_exhausted" || len(card.Windows) != 1 {
		t.Fatalf("wrong plan: %+v", card)
	}
	w := card.Windows[0]
	if w.ID != "daily" || !w.Known || !w.Exhausted || w.Authoritative || w.UsedPercent == nil || *w.UsedPercent != 85.51 {
		t.Fatalf("exhaustion fabricated 100%% usage: %+v", w)
	}
	if w.ResetsAt == nil || !w.ResetsAt.Equal(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)) || w.WindowSeconds != 86400 {
		t.Fatalf("wrong daily window: %+v", w)
	}
	metrics := map[string]float64{}
	for _, x := range card.Metrics {
		metrics[x.Key] = x.Value
	}
	if metrics["metered_neurons"] != 7400 || metrics["reserved_neurons"] != 1151 || metrics["calendar_daily_allowances"] != 310000 || metrics["calendar_daily_value_usd"] != 3.41 {
		t.Fatalf("missing distinct usage/calendar values: %+v", metrics)
	}
	public, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE-ACCOUNT", "PRIVATE-TOKEN", "PRIVATE-UPSTREAM", "account_id", "observation_gaps"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public page leaked %s", secret)
		}
	}
}

func TestCloudflareUnknownPAYGAndClosedPageCodes(t *testing.T) {
	cards := cloudflareQuotaCards(json.RawMessage(`{"stayWithinIncluded":false,"accounts":[{"name":"workers-ai#1","health":"PRIVATE-ERROR","reason":"PRIVATE-REASON","windows":{"daily":{"known":false,"utilization":0.1,"resets_at":"2026-10-25T00:00:00Z"}}}]}`))
	if len(cards) != 1 || cards[0].Plan != "PAYG allowed" || cards[0].Health != "unknown" || cards[0].Reason != "other" || cards[0].Windows[0].Known || cards[0].Windows[0].UsedPercent != nil {
		t.Fatalf("unsafe or misleading unknown PAYG projection: %+v", cards)
	}
	raw, _ := json.Marshal(cards)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("free upstream text leaked")
	}
}
