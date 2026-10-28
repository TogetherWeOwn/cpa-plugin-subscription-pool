package openrouterfree

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

func TestReplacementCredentialCannotInheritElevatedAllowance(t *testing.T) {
	f := newFake(idA)
	host := hostWith(t, f, entry(idA))
	m := setup(t, host, `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	host.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "test-only-replacement-key"
	if err := m.Reconfigure(context.Background(), host, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if s := status(t, m); s.DailyLimit != 50 || s.DailyLimitSource != "assumed_lowest" || s.UsedToday != 1 {
		t.Fatalf("replacement inherited entitlement or lost usage: %+v", s)
	}
	for i := 1; i < 50; i++ {
		m.now = func() time.Time { return start.Add(time.Duration(i) * time.Minute) }
		assertPick(t, m, req(m, aliasPrefix+idA), true)
	}
	m.now = func() time.Time { return start.Add(51 * time.Minute) }
	assertPick(t, m, req(m, aliasPrefix+idA), false)
}

func TestRestartMustReverifyElevatedAllowance(t *testing.T) {
	f := newFake(idA)
	host := hostWith(t, f, entry(idA))
	m := setup(t, host, `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := setup(t, host, `{}`)
	if s := status(t, again); s.DailyLimit != 50 || s.DailyLimitSource != "assumed_lowest" || s.UsedToday != 1 {
		t.Fatalf("restart trusted unbound persisted entitlement: %+v", s)
	}
	syncNow(t, again)
	if s := status(t, again); s.DailyLimit != 1000 || s.UsedToday != 1 {
		t.Fatalf("fresh quota lookup did not restore entitlement: %+v", s)
	}
}

type gatedKeyHTTP struct {
	inner   *fakeHTTP
	started chan struct{}
	release chan struct{}
}

func (g *gatedKeyHTTP) Do(r *http.Request) (*http.Response, error) {
	if r.URL.String() == keyURL {
		close(g.started)
		select {
		case <-g.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return g.inner.Do(r)
}

func TestObsoleteQuotaResponseCannotCommitAfterReconfigure(t *testing.T) {
	f := newFake(idA)
	g := &gatedKeyHTTP{inner: f, started: make(chan struct{}), release: make(chan struct{})}
	host := hostWith(t, f, entry(idA))
	host.HTTPClient = g
	m := setup(t, host, `{}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.syncOnce(context.Background())
	}()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old quota lookup did not start")
	}
	host.OpenAICompatibility = []providermodule.OpenAICompatibility{entry(idA)}
	host.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "test-only-replacement-key"
	host.HTTPClient = f
	if err := m.Reconfigure(context.Background(), host, json.RawMessage(`{}`)); err != nil {
		close(g.release)
		<-done
		t.Fatal(err)
	}
	close(g.release)
	<-done
	if s := status(t, m); s.DailyLimit != 50 || s.DailyLimitSource != "assumed_lowest" || s.ModelCount != 0 {
		t.Fatalf("obsolete sync committed into replacement configuration: %+v", s)
	}
	syncNow(t, m)
	if s := status(t, m); s.DailyLimit != 1000 || s.ModelCount != 1 {
		t.Fatalf("current-generation sync did not apply: %+v", s)
	}
}
