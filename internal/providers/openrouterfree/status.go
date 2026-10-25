package openrouterfree

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// StatusView is also the usage-page contract. The model list is OpenRouter's
// public catalog, so it is safe on the authenticated status route; keys,
// hashes and auth IDs are never exposed. UsedToday and RateUsed are LOCAL
// admissions, not an OpenRouter balance.
type StatusView struct {
	Provider          string        `json:"provider"`
	Status            string        `json:"status"`
	StayWithinFree    bool          `json:"stay_within_free"`
	Models            []string      `json:"models"`
	ModelCount        int           `json:"model_count"`
	CatalogAt         *time.Time    `json:"catalog_at,omitempty"`
	CatalogFresh      bool          `json:"catalog_fresh"`
	SyncIntervalSec   int64         `json:"sync_interval_seconds"`
	LastSyncAttemptAt *time.Time    `json:"last_sync_attempt_at,omitempty"`
	LastSyncError     string        `json:"last_sync_error,omitempty"`
	HostSync          string        `json:"host_sync"`
	HostSyncedAt      *time.Time    `json:"host_synced_at,omitempty"`
	DailyLimit        int64         `json:"daily_limit"`
	DailyLimitSource  string        `json:"daily_limit_source"`
	UsedToday         int64         `json:"used_today"`
	DayResetsAt       time.Time     `json:"day_resets_at"`
	RequestsPerMinute int           `json:"requests_per_minute"`
	RateUsed          int           `json:"rate_used"`
	RateResetsAt      *time.Time    `json:"rate_resets_at,omitempty"`
	CooldownUntil     *time.Time    `json:"cooldown_until,omitempty"`
	RecentChanges     []change      `json:"recent_changes"`
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
	limit, source := m.dailyLimit()
	s := StatusView{
		Provider: providerID, Status: "inactive", StayWithinFree: m.cfg.StayWithinFree,
		Models: append([]string{}, m.state.Models...), ModelCount: len(m.state.Models),
		CatalogFresh:    !m.state.CatalogAt.IsZero() && now.Sub(m.state.CatalogAt) <= m.cfg.CatalogMaxAge,
		SyncIntervalSec: int64(m.cfg.SyncInterval / time.Second),
		HostSync:        m.sync.Reconcile, DailyLimit: limit, DailyLimitSource: source,
		UsedToday: m.state.Used, DayResetsAt: nextUTCMidnight(now),
		RequestsPerMinute: m.cfg.RequestsPerMinute, RateUsed: len(m.state.Recent),
		RecentChanges: append([]change{}, m.state.Changes...), StateError: m.stateErr, Accounts: []AccountView{},
		LastSyncError: m.sync.LastError,
	}
	if s.HostSync == "" {
		s.HostSync = "pending"
	}
	if !m.state.CatalogAt.IsZero() {
		v := m.state.CatalogAt
		s.CatalogAt = &v
	}
	if !m.sync.LastAttempt.IsZero() {
		v := m.sync.LastAttempt
		s.LastSyncAttemptAt = &v
	}
	if !m.sync.PushedAt.IsZero() {
		v := m.sync.PushedAt
		s.HostSyncedAt = &v
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
		ids := make([]string, 0, len(m.view.lanes))
		for id := range m.view.lanes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			l := m.view.lanes[id]
			reason := m.reason(l, "", now)
			health := "healthy"
			switch reason {
			case "placeholder_key":
				health = "unbound"
			case "rate_limited":
				health = "resting"
			case "daily_limit":
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
