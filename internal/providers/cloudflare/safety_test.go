package cloudflare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAnalyticsCannotEraseOutstandingReservations(t *testing.T) {
	m, _, _ := configured(t, `{}`)
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.accounts[testAccount].remote = 7800
	m.mu.Unlock()
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err == nil {
		t.Fatal("new analytics erased an in-flight reservation")
	}
}

func TestPAYGSwitchBackCannotUseDelayedAnalytics(t *testing.T) {
	m, now, h := configured(t, `{"stayWithinIncluded":false}`)
	*now = utcDay(*now).Add(23 * time.Hour) // still in flight at midnight
	m.pollOnce(context.Background(), true)
	r := pickRequest(m, 0)
	r.Model = "unpriced-model"
	if _, err := m.Pick(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err == nil {
		t.Fatal("PAYG-to-included transition trusted delayed zero")
	}
	*now = utcDay(*now).Add(24 * time.Hour)
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err == nil {
		t.Fatal("unfinished PAYG request escaped at midnight")
	}
	if err := m.HandleUsage(context.Background(), pluginapi.UsageRecord{AuthID: r.Candidates[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err == nil {
		t.Fatal("PAYG completion day trusted delayed zero analytics")
	}
	*now = utcDay(*now).Add(24 * time.Hour)
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 1)); err != nil {
		t.Fatalf("completed PAYG should no longer fence the next fresh day: %v", err)
	}
}

func TestPAYGFenceIsDurableBeforeDispatch(t *testing.T) {
	m, _, _ := configured(t, `{"stayWithinIncluded":false}`)
	m.pollOnce(context.Background(), true)
	if err := m.store.Close(); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, m, pickModel(m, 0, "unpriced-model"), "state_unavailable")
	m.mu.Lock()
	b := *m.budgetLocked(testAccount, m.now())
	m.mu.Unlock()
	if b.UnsafeDay != "" || b.UnboundedBatch || len(b.Pending) != 0 {
		t.Fatalf("refused PAYG admission left an in-memory fence: %+v", b)
	}
}

func TestPAYGFenceSurvivesRestart(t *testing.T) {
	m, now, h := configured(t, `{"stayWithinIncluded":false}`)
	mustAdmit(t, m, pickModel(m, 0, "unpriced-model"))
	_ = m.Close(context.Background())
	next := restarted(t, h, now, `{}`)
	next.pollOnce(context.Background(), true)
	mustRefuse(t, next, pickRequest(next, 1), "recent_payg_usage")
}

func TestOutOfOrderCompletionsCarryEntireActiveBatch(t *testing.T) {
	m, now, _ := configured(t, `{}`)
	*now = utcDay(*now).Add(23 * time.Hour) // still in flight at midnight
	m.pollOnce(context.Background(), true)
	large := pickRequest(m, 0)
	large.Model = "glm-4.7-flash"
	small := pickRequest(m, 0)
	if _, err := m.Pick(context.Background(), large); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pick(context.Background(), small); err != nil {
		t.Fatal(err)
	}
	// Same auth ID has no request identifier in the callback ABI. The smaller
	// request can finish first; FIFO subtraction must not under-reserve midnight.
	_ = m.HandleUsage(context.Background(), pluginapi.UsageRecord{AuthID: small.Candidates[0].ID})
	*now = utcDay(*now).Add(24 * time.Hour)
	m.pollOnce(context.Background(), true)
	m.mu.Lock()
	got := m.budgetLocked(testAccount, *now).Committed
	m.mu.Unlock()
	a, _ := reservationFor(large.Model)
	b, _ := reservationFor(small.Model)
	if got < a+b {
		t.Fatalf("out-of-order completion reduced carried batch: %v < %v", got, a+b)
	}
}

func TestGuardCannotBeDisabledBehindActiveHostProvider(t *testing.T) {
	m, _, h := configured(t, `{}`)
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"enabled":false}`)); err == nil {
		t.Fatal("disabling the guard silently enables paid host fallthrough")
	}
	h.OpenAICompatibility[0].Disabled = true
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidDurableBudgetRejectsActivation(t *testing.T) {
	for _, raw := range []string{
		`{"version":999,"accounts":{}}`,
		`{}`,
		`null`,
		`{"accounts":{}}`,
		`{"version":1}`,
	} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "providers", providerID)
			if err := os.MkdirAll(stateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, "budget.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			m := NewModule()
			m.noPoller = true
			defer func() {
				if err := m.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			if err := m.Reconfigure(context.Background(), testHost(dir), nil); err == nil {
				t.Fatal("corrupt state treated as a new zero budget")
			}
		})
	}
}

func TestDurableBudgetMustCoverItsOutstandingBatch(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for name, b := range map[string]*budget{
		"missing carry":          {Day: "2026-10-24", Committed: 1151, Pending: map[string][]pendingEntry{"auth": {{1151, at}}}},
		"missing committed":      {Day: "2026-10-24", Carry: 1151, Pending: map[string][]pendingEntry{"auth": {{1151, at}}}},
		"orphaned carry":         {Day: "2026-10-24", Committed: 1151, Carry: 1151},
		"missing PAYG latch":     {Day: "2026-10-24", UnboundedBatch: true, Pending: map[string][]pendingEntry{"auth": {{0, at}}}},
		"missing PAYG batch":     {Day: "2026-10-24", UnsafeDay: "2026-10-24", Pending: map[string][]pendingEntry{"auth": {{0, at}}}},
		"empty pending queue":    {Day: "2026-10-24", Pending: map[string][]pendingEntry{"auth": {}}},
		"missing admission time": {Day: "2026-10-24", Committed: 1151, Carry: 1151, Pending: map[string][]pendingEntry{"auth": {{Neurons: 1151}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if validState(diskState{Version: 1, Accounts: map[string]*budget{testAccount: b}}) {
				t.Fatal("inconsistent durable state accepted")
			}
		})
	}
}

func TestDurableLabelsMustBeUniquePositiveAccountIDs(t *testing.T) {
	other := strings.Repeat("0", 32)
	for name, labels := range map[string]map[string]int{
		"zero":        {testAccount: 0},
		"negative":    {testAccount: -1},
		"duplicate":   {testAccount: 1, other: 1},
		"bad account": {"not-an-account": 1},
	} {
		t.Run(name, func(t *testing.T) {
			if validState(diskState{Version: 1, Accounts: map[string]*budget{}, Labels: labels}) {
				t.Fatal("invalid label state accepted")
			}
		})
	}
	if !validState(diskState{Version: 1, Accounts: map[string]*budget{}, Labels: map[string]int{testAccount: 1, other: 3}}) {
		t.Fatal("labels with a gap left by a removed account rejected")
	}
}
