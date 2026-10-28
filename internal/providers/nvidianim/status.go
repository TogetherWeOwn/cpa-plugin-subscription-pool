package nvidianim

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// StatusView is also the usage-page contract. No keys, hashes, auth IDs,
// model lists or upstream error text are exposed even on authenticated status.
// ReservedRequests and RateUsed are LOCAL admissions, not an authoritative
// NVIDIA balance: out-of-band account traffic is not observable here.
type StatusView struct {
	Provider          string        `json:"provider"`
	Status            string        `json:"status"`
	StayWithinFree    bool          `json:"stay_within_free"`
	FreeTierConfirmed bool          `json:"free_tier_confirmed"`
	AllowanceSource   string        `json:"allowance_source"`
	ReservedRequests  int64         `json:"reserved_requests"`
	FreeRequestLimit  int64         `json:"free_request_limit"`
	RequestsPerMinute int           `json:"requests_per_minute"`
	RateUsed          int           `json:"rate_used"`
	RateResetsAt      *time.Time    `json:"rate_resets_at,omitempty"`
	CooldownUntil     *time.Time    `json:"cooldown_until,omitempty"`
	TrialExpiresAt    *time.Time    `json:"trial_expires_at,omitempty"`
	FreeExhausted     bool          `json:"free_exhausted"`
	StateError        bool          `json:"state_error"`
	Accounts          []AccountView `json:"accounts"`
}
type AccountView struct {
	Name   string `json:"name"`
	Health string `json:"health"`
	Reason string `json:"reason,omitempty"`
}

func (m *Module) Status(context.Context) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.prune(now)
	s := StatusView{Provider: providerID, Status: "inactive", StayWithinFree: m.cfg.StayWithinFree, FreeTierConfirmed: m.cfg.FreeTierConfirmed, AllowanceSource: "unknown", ReservedRequests: m.state.Reserved, FreeRequestLimit: m.cfg.FreeRequestLimit, RequestsPerMinute: m.cfg.RequestsPerMinute, RateUsed: len(m.state.Recent), FreeExhausted: m.state.FreeExhausted, StateError: m.stateErr, Accounts: []AccountView{}}
	if m.cfg.FreeTierConfirmed {
		s.AllowanceSource = "operator_confirmed"
	}
	if !m.cfg.TrialExpiresAt.IsZero() {
		v := m.cfg.TrialExpiresAt
		s.TrialExpiresAt = &v
	}
	if m.state.CooldownUntil.After(now) {
		v := m.state.CooldownUntil
		s.CooldownUntil = &v
	}
	for _, at := range m.state.Recent {
		v := at.Add(time.Minute)
		if s.RateResetsAt == nil || v.Before(*s.RateResetsAt) {
			s.RateResetsAt = &v
		}
	}
	if m.active() {
		s.Status = "registered"
		ids := make([]string, 0, len(m.lanes))
		for id := range m.lanes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			l := m.lanes[id]
			reason := m.reason(l, "", now)
			health := "healthy"
			switch reason {
			case "placeholder_key":
				health = "unbound"
			case "rate_limited":
				health = "resting"
			case "free_exhausted", "trial_expired":
				health = "exhausted"
			case "":
			default:
				health = "unknown"
			}
			s.Accounts = append(s.Accounts, AccountView{Name: l.name, Health: health, Reason: reason})
		}
	}
	return json.Marshal(s)
}
