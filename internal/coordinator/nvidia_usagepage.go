package coordinator

import (
	"encoding/json"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/nvidianim"
)

// NVIDIA has not published a universal monthly credit grant. Show local
// reservations and the operator's verified RPM, not a fabricated credit bar.
// All configured keys share one conservative account budget, so do not multiply
// the displayed allowance by the number of credentials.
func nvidiaQuotaCards(raw json.RawMessage) []quotaCard {
	var s nvidianim.StatusView
	if json.Unmarshal(raw, &s) != nil || len(s.Accounts) == 0 {
		return nil
	}
	card := quotaCard{Provider: "nvidia-nim", ProviderLabel: providerLabel("nvidia-nim"), Account: "NVIDIA shared allowance", Plan: "Free-only guard on (prototyping)", Health: "unknown", Windows: []quotaWindow{}}
	if !s.StayWithinFree {
		card.Plan = "Paid use allowed (free guard off)"
	}
	for _, a := range s.Accounts {
		switch a.Health {
		case "healthy", "resting", "exhausted", "unbound", "unknown":
			card.Health = a.Health
		default:
			card.Health = "unknown"
		}
		card.Reason = nvidiaReasonCode(a.Reason)
		if card.Health == "healthy" {
			break
		}
	}
	w := quotaWindow{ID: "local_rpm", Label: "Local rolling RPM (shared)", Mode: "rolling", Known: false, Source: "local_reservation", WindowSeconds: 60}
	if s.RequestsPerMinute > 0 {
		w = resetWindow("local_rpm", "Local rolling RPM (shared)", time.Minute, float64(s.RateUsed)/float64(s.RequestsPerMinute), s.RateResetsAt, "local_reservation", false, false, s.RateUsed >= s.RequestsPerMinute)
		w.Mode = "rolling" // resets_at is the next slot release, not a full bucket reset
	}
	card.Windows = append(card.Windows, w)
	if s.StayWithinFree && s.FreeRequestLimit > 0 {
		used := 100 * float64(s.ReservedRequests) / float64(s.FreeRequestLimit)
		card.Windows = append(card.Windows, quotaWindow{ID: "local_request_ceiling", Label: "Local lifetime request ceiling (no auto-reset)", Mode: "lifetime", Known: true, UsedPercent: percentPtr(used), RemainingPercent: percentPtr(100 - used), Source: "local_reservation", Exhausted: s.ReservedRequests >= s.FreeRequestLimit})
	}
	card.Metrics = []quotaMetric{{Key: "reserved_requests", Label: "Local reservations", Value: float64(s.ReservedRequests)}, {Key: "rpm_limit", Label: "RPM cap", Value: float64(s.RequestsPerMinute)}}
	return []quotaCard{card}
}

func nvidiaReasonCode(s string) string {
	switch s {
	case "", "placeholder_key", "state_unavailable", "rate_limited", "allowance_unverified", "unverified_endpoint", "free_exhausted", "trial_expired", "unverified_model":
		return s
	default:
		return "other"
	}
}
