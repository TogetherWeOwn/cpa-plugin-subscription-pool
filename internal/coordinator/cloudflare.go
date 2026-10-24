package coordinator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/cloudflare"
)

var workersAIName = regexp.MustCompile(`^workers-ai#[1-9][0-9]*$`)

// The public page gets only fixed labels/codes and numeric quota data, never
// analytics diagnostics, account IDs, auth IDs or a credential from status.
func cloudflareQuotaCards(raw json.RawMessage) []quotaCard {
	var status cloudflare.StatusView
	if len(raw) == 0 || json.Unmarshal(raw, &status) != nil {
		return nil
	}
	cards := make([]quotaCard, 0, len(status.Accounts))
	for i, a := range status.Accounts {
		name := a.Name
		if !workersAIName.MatchString(name) {
			name = fmt.Sprintf("workers-ai#%d", i+1)
		}
		card := quotaCard{Provider: "cloudflare-workers-ai", ProviderLabel: providerLabel("cloudflare-workers-ai"), Account: name, Plan: "Included-only (daily; no rollover)", Health: "unknown"}
		if !status.StayWithinIncluded {
			card.Plan = "PAYG allowed"
		}
		switch a.Health {
		case "healthy", "disabled", "unbound", "unknown", "exhausted":
			card.Health = a.Health
		}
		switch a.Reason {
		case "":
		case "included_exhausted", "usage_unknown_or_stale", "credential_unbound", "state_unavailable", "recent_payg_usage":
			card.Reason = a.Reason
		default:
			card.Reason = "other"
		}
		w := a.Windows["daily"]
		window := quotaWindow{ID: "daily", Label: "Daily neurons (UTC)", Mode: "reset", Known: w.Known && w.Utilization != nil, WindowSeconds: 86400, Source: "analytics_reservations", Exhausted: w.Exhausted, Stale: !w.Known}
		if !w.ResetAt.IsZero() {
			window.ResetsAt = timePtr(w.ResetAt)
			window.StartsAt = timePtr(w.ResetAt.Add(-24 * time.Hour))
		}
		if window.Known {
			// Guard exhaustion is not necessarily 100% utilization: headroom must
			// also fit the next worst-case reservation and safety margin.
			window.UsedPercent = percentPtr(*w.Utilization * 100)
			window.RemainingPercent = percentPtr(100 - *w.Utilization*100)
		}
		card.Windows = []quotaWindow{window}
		card.Metrics = []quotaMetric{
			{Key: "included_daily_neurons", Label: "Daily included", Value: status.IncludedDaily, Unit: "neurons"},
			{Key: "safety_margin_neurons", Label: "Safety margin", Value: status.SafetyMargin, Unit: "neurons"},
			{Key: "reserved_neurons", Label: "Local debits (conservative)", Value: a.ReservedNeurons, Unit: "neurons"},
			{Key: "calendar_daily_allowances", Label: "Calendar daily allowances (not pooled)", Value: status.Quote.IncludedNeurons, Unit: "neurons"},
			{Key: "calendar_daily_value_usd", Label: "Calendar allowance value (not a credit)", Value: status.Quote.IncludedValueUSD, Unit: "USD"},
		}
		if a.MeteredNeurons != nil {
			card.Metrics = append(card.Metrics, quotaMetric{Key: "metered_neurons", Label: "Analytics usage (may lag)", Value: *a.MeteredNeurons, Unit: "neurons"})
		}
		if a.EstimatedDailyOverageUSD != nil {
			card.Metrics = append(card.Metrics, quotaMetric{Key: "estimated_daily_overage_usd", Label: "Estimated daily AI overage", Value: *a.EstimatedDailyOverageUSD, Unit: "USD"})
		}
		cards = append(cards, card)
	}
	return cards
}
