package opencodego

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusContractDoesNotExportCooldownFromWindowReset(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	cooldownAt := now.Add(5 * time.Minute)
	// Synthetic state, no dashboard credential, host auth IDs, or network client.
	// Status reads cached state only; it never polls.
	m := &Module{
		clock: &fakeModuleClock{now: now},
		state: &moduleState{Accounts: map[string]*accountState{
			"synthetic": {Name: "synthetic", Windows: map[windowKind]windowState{
				windowMonthly: {Known: true, Exhausted: true, ResetAt: resetAt, CooldownAt: cooldownAt, Source: "synthetic observation"},
			}},
		}},
	}
	raw, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Provider string `json:"provider"`
		Accounts []struct {
			Windows map[windowKind]struct {
				Exhausted bool      `json:"exhausted"`
				ResetsAt  time.Time `json:"resets_at"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Provider != "opencode-go" || len(body.Accounts) != 1 {
		t.Fatal("expected OpenCode Go status account")
	}
	window := body.Accounts[0].Windows[windowMonthly]
	if !window.Exhausted || !window.ResetsAt.Equal(resetAt) {
		t.Fatal("quota reset semantics changed")
	}
	for _, forbidden := range []string{"\"identity\"", "\"cooldown\"", "cooldown_at", cooldownAt.Format(time.RFC3339)} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("OpenCode Go status unexpectedly exposes Z.ai cooldown contract")
		}
	}
}
