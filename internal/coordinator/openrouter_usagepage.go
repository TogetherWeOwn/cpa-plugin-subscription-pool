package coordinator

import (
	"encoding/json"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/openrouterfree"
)

// The public page shows local admissions against OpenRouter's documented free
// limits and the size of the tracked free list; the list itself, change log
// and sync diagnostics stay on the authenticated status route.
func openrouterQuotaCards(raw json.RawMessage) []quotaCard {
	var s openrouterfree.StatusView
	if json.Unmarshal(raw, &s) != nil || len(s.Accounts) == 0 {
		return nil
	}
	card := quotaCard{Provider: "openrouter-free", ProviderLabel: providerLabel("openrouter-free"), Account: "OpenRouter free models (shared)", Plan: "Free models only (:free, auto-synced)", Health: "unknown", Windows: []quotaWindow{}}
	if !s.StayWithinFree {
		card.Plan = "Free models only (local limits off)"
	}
	for _, a := range s.Accounts {
		switch a.Health {
		case "healthy", "resting", "exhausted", "unbound", "unknown":
			card.Health = a.Health
		default:
			card.Health = "unknown"
		}
		card.Reason = openrouterReasonCode(a.Reason)
		if card.Health == "healthy" {
			break
		}
	}
	if s.DailyLimit > 0 {
		used := float64(s.UsedToday) / float64(s.DailyLimit)
		reset := s.DayResetsAt
		card.Windows = append(card.Windows, resetWindow("local_daily", "Local daily free requests (UTC, shared)", 24*time.Hour, used, &reset, "local_reservation", false, false, s.UsedToday >= s.DailyLimit))
	}
	rpm := resetWindow("local_rpm", "Local rolling RPM (shared)", time.Minute, float64(s.RateUsed)/float64(s.RequestsPerMinute), s.RateResetsAt, "local_reservation", false, false, s.RateUsed >= s.RequestsPerMinute)
	rpm.Mode = "rolling" // resets_at is the next slot release, not a full bucket reset
	card.Windows = append(card.Windows, rpm)
	card.Metrics = []quotaMetric{
		{Key: "free_models", Label: "Free models tracked", Value: float64(s.ModelCount)},
		{Key: "daily_limit", Label: "Daily free-request limit", Value: float64(s.DailyLimit)},
		{Key: "rpm_limit", Label: "RPM cap", Value: float64(s.RequestsPerMinute)},
	}
	return []quotaCard{card}
}

func openrouterReasonCode(s string) string {
	switch s {
	case "", "placeholder_key", "state_unavailable", "rate_limited", "unverified_endpoint", "catalog_unverified", "stale_catalog", "daily_limit", "unverified_model", "model_not_free":
		return s
	default:
		return "other"
	}
}
