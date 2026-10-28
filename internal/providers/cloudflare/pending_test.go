package cloudflare

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestLostCallbacksExpireInsteadOfPinningTheBudget(t *testing.T) {
	m, now, h := configured(t, `{}`)
	*now = utcDay(*now).Add(23 * time.Hour)
	m.pollOnce(context.Background(), true)
	admitted := 0
	for ; admitted < 20; admitted++ {
		if _, err := m.Pick(context.Background(), pickRequest(m, admitted%2)); err != nil {
			break
		}
	}
	if admitted != 8 {
		t.Fatalf("admitted %d before exhaustion", admitted)
	}
	_ = m.Close(context.Background()) // the process dies; no callback ever arrives
	*now = now.Add(2 * time.Hour)
	next := restarted(t, h, now, `{}`)
	next.pollOnce(context.Background(), true)
	mustRefuse(t, next, pickRequest(next, 0), "included_exhausted") // still possibly running
	*now = utcDay(*now).Add(24 * time.Hour)
	next.pollOnce(context.Background(), true)
	mustAdmit(t, next, pickRequest(next, 0))
	next.mu.Lock()
	b := *next.budgetLocked(testAccount, *now)
	next.mu.Unlock()
	if cost, _ := reservationFor(pickRequest(next, 0).Model); b.Committed != cost || b.Carry != cost {
		t.Fatalf("expired entries still counted: %+v", b)
	}
}

func TestExpiredPAYGEntryFencesTheDayItMayHaveFinished(t *testing.T) {
	m, now, h := configured(t, `{"stayWithinIncluded":false}`)
	*now = utcDay(*now).Add(22 * time.Hour)
	mustAdmit(t, m, pickModel(m, 0, "unpriced-model"))
	reconfigure(t, m, h, `{}`)
	*now = utcDay(*now).Add(24*time.Hour + 5*time.Hour)
	m.pollOnce(context.Background(), true)
	mustRefuse(t, m, pickRequest(m, 1), "recent_payg_usage")
	*now = utcDay(*now).Add(24 * time.Hour)
	m.pollOnce(context.Background(), true)
	mustAdmit(t, m, pickRequest(m, 1))
}

func TestRotatedKeyCallbacksStillDrainTheirReservations(t *testing.T) {
	for _, restart := range []bool{false, true} {
		m, now, h := configured(t, `{}`)
		m.pollOnce(context.Background(), true)
		old := pickRequest(m, 0)
		mustAdmit(t, m, old)
		h.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "rotated-test-token"
		reconfigure(t, m, h, `{}`)
		if restart {
			_ = m.Close(context.Background())
			m = restarted(t, h, now, `{}`)
		}
		record := pluginapi.UsageRecord{AuthID: old.Candidates[0].ID}
		for _, id := range m.OwnedAuthIDs(context.Background()) {
			if id == record.AuthID {
				t.Fatal("key change kept the old auth ID")
			}
		}
		if !m.RecognizeUsage(record) {
			t.Fatalf("restart=%v: callback for the retired auth ID would be dropped", restart)
		}
		if err := m.HandleUsage(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if got := m.UsageAccount(record); got != "workers-ai#1" {
			t.Fatalf("restart=%v: retired usage attributed to %q", restart, got)
		}
		m.mu.Lock()
		b := *m.budgetLocked(testAccount, *now)
		m.mu.Unlock()
		if len(b.Pending) != 0 || b.Carry != 0 {
			t.Fatalf("restart=%v: retired reservation not drained: %+v", restart, b)
		}
		*now = utcDay(*now).Add(24 * time.Hour)
		m.pollOnce(context.Background(), true)
		mustAdmit(t, m, pickRequest(m, 0))
		if m.RecognizeUsage(pluginapi.UsageRecord{AuthID: "openai-compatibility:other:000000000000"}) {
			t.Fatal("claimed a callback this module never admitted")
		}
	}
}
