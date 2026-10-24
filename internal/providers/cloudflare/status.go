package cloudflare

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"
)

// StatusView also supplies the coordinator's redacted quota-page projection.
// Codes and generated display labels only: never a token, account ID, auth ID,
// URL, upstream error body, or file path, even on the authenticated route.
type StatusView struct {
	Provider           string          `json:"provider"`
	Status             string          `json:"status"`
	Enabled            bool            `json:"enabled"`
	StayWithinIncluded bool            `json:"stayWithinIncluded"`
	IncludedDaily      float64         `json:"included_daily_neurons"`
	SafetyMargin       float64         `json:"safety_margin_neurons"`
	Quote              monthQuote      `json:"calendar_month_quote"`
	Accounts           []AccountStatus `json:"accounts"`
	ObservationGaps    []string        `json:"observation_gaps"`
}

type AccountStatus struct {
	Name                     string                  `json:"name"`
	Health                   string                  `json:"health"`
	Reason                   string                  `json:"reason,omitempty"`
	CredentialBound          bool                    `json:"credential_bound"`
	MeteredNeurons           *float64                `json:"metered_neurons,omitempty"`
	ReservedNeurons          float64                 `json:"reserved_neurons"`
	EstimatedDailyOverageUSD *float64                `json:"estimated_daily_overage_usd,omitempty"`
	Windows                  map[string]WindowStatus `json:"windows"`
	LastPollError            string                  `json:"last_poll_error,omitempty"`
}

type WindowStatus struct {
	Known         bool       `json:"known"`
	Utilization   *float64   `json:"utilization,omitempty"`
	Exhausted     bool       `json:"exhausted"`
	ResetAt       time.Time  `json:"resets_at"`
	Source        string     `json:"source"`
	Authoritative bool       `json:"authoritative"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
}

func (m *Module) Status(context.Context) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	view := StatusView{Provider: providerID, Status: "registered", Enabled: m.cfg.Enabled, StayWithinIncluded: m.cfg.StayWithinIncluded, IncludedDaily: includedDailyNeurons, SafetyMargin: m.cfg.SafetyMargin, Quote: calendarQuote(now), Accounts: []AccountStatus{}, ObservationGaps: []string{
		"Workers AI includes 10,000 neurons per account per UTC day; no separate monthly pool or daily rollover",
		"Adaptive analytics may be sampled or delayed; this local guard cannot guarantee zero charges from other account consumers",
		"Included-only mode reserves a full text-model context for both input and output before dispatch; large, multimodal and unknown models are refused",
		"Reservations are not refunded from delayed analytics or token estimates; unfinished requests carry over midnight",
		"One active scheduler instance per account is required; independent CLIProxy replicas do not share atomic reservations",
		"GraphQL schema and permissions must be verified with the deployment's scoped token before activation",
	}}
	if !m.activeLocked() {
		view.Status = "inactive"
	}
	ids := make([]string, 0, len(m.accounts))
	for id := range m.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		minReserve := math.Inf(1)
		for _, lane := range m.lanes {
			if lane.account == id && lane.bound {
				for _, reserve := range lane.reservations {
					if reserve > 0 {
						minReserve = math.Min(minReserve, reserve)
					}
				}
			}
		}
		a := m.accounts[id]
		fresh := m.freshLocked(a, now)
		used := m.effectiveLocked(id, now)
		b := m.budgetLocked(id, now)
		exhausted := m.cfg.StayWithinIncluded && (used >= includedDailyNeurons-m.cfg.SafetyMargin || used+minReserve > includedDailyNeurons-m.cfg.SafetyMargin)
		w := WindowStatus{Known: fresh, Exhausted: exhausted, ResetAt: utcDay(now).Add(24 * time.Hour), Source: "analytics_and_conservative_reservations"}
		entry := AccountStatus{Name: m.accountNameLocked(id), Health: "healthy", CredentialBound: a.token != "", ReservedNeurons: b.Committed, LastPollError: a.pollError, Windows: map[string]WindowStatus{}}
		if a.readingDay == utcDay(now).Format("2006-01-02") {
			remote := a.remote
			overage := math.Max(remote-includedDailyNeurons, 0) * pricePerThousandNeurons / 1000
			entry.MeteredNeurons = &remote
			entry.EstimatedDailyOverageUSD = &overage
			if fresh {
				util := used / includedDailyNeurons
				w.Utilization = &util
			}
		}
		if !a.observed.IsZero() {
			observed := a.observed
			w.ObservedAt = &observed
		}
		switch {
		case !m.activeLocked():
			entry.Health = "disabled"
		case !entry.CredentialBound:
			entry.Health = "unbound"
			entry.Reason = "credential_unbound"
		case m.cfg.StayWithinIncluded && (b.UnsafeDay == b.Day || b.UnboundedBatch):
			entry.Health = "unknown"
			entry.Reason = "recent_payg_usage"
		case m.cfg.StayWithinIncluded && m.stateErr != "":
			entry.Health = "unknown"
			entry.Reason = "state_unavailable"
		case m.cfg.StayWithinIncluded && !fresh:
			entry.Health = "unknown"
			entry.Reason = "usage_unknown_or_stale"
		case exhausted:
			entry.Health = "exhausted"
			entry.Reason = "included_exhausted"
		}
		entry.Windows["daily"] = w
		view.Accounts = append(view.Accounts, entry)
	}
	return json.Marshal(view)
}
