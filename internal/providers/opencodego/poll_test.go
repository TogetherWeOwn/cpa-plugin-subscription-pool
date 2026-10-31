package opencodego

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

// countingUsageClient answers every dashboard poll with body and counts them.
type countingUsageClient struct {
	mu    sync.Mutex
	hits  int
	body  string
	calls chan struct{}
}

func newCountingUsageClient(body string) *countingUsageClient {
	return &countingUsageClient{body: body, calls: make(chan struct{}, 16)}
}

func (c *countingUsageClient) Do(*http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	select {
	case c.calls <- struct{}{}:
	default:
	}
	return usageHTTPResponse(http.StatusOK, c.body), nil
}

func (c *countingUsageClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func pickAt(module *Module, now time.Time, ids ...string) (pluginapi.SchedulerPickResponse, error) {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: id})
	}
	return module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: candidates,
	})
}

// TestStatusNeverPolls is a regression test: Status, which the unauthenticated
// usage resource reaches, reads cached state and sends no dashboard request.
func TestStatusNeverPolls(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	client := newCountingUsageClient(usageFixture(
		usageWindowFixture("ok", 1, now.Add(time.Hour)),
		usageWindowFixture("ok", 1, now.Add(24*time.Hour)),
		usageWindowFixture("ok", 1, now.Add(20*24*time.Hour)),
	))
	module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
	for i := 0; i < 3; i++ {
		if _, err := module.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := client.count(); got != 0 {
		t.Fatalf("Status() sent %d dashboard requests, want 0", got)
	}
}

// TestBackgroundPollerEnforcesThresholdWithoutStatus is a regression test: the
// poller refreshes usage on its own, so an exhausted account is refused
// although nobody has viewed status. It waits on poll events, not elapsed
// time: the poller polls one round at a time, so a second request (forced by
// moving the injected clock past the interval and waking the poller with a
// Reconfigure) proves the first round's result has been applied.
func TestBackgroundPollerEnforcesThresholdWithoutStatus(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	clock := &fakeModuleClock{now: now}
	client := newCountingUsageClient(usageFixture(
		usageWindowFixture("ok", 10, now.Add(time.Hour)),
		usageWindowFixture("ok", 10, now.Add(24*time.Hour)),
		usageWindowFixture("rate-limited", 100, now.Add(20*24*time.Hour)),
	))
	module := NewModule()
	host := providermodule.HostConfig{Clock: clock, HTTPClient: client}
	raw := json.RawMessage(`{"dashboard-api-key":"` + dashboardFixtureKey + `","accounts":[{"name":"go-a","auth-ids":["go-a-1"]}]}`)
	if err := module.Reconfigure(context.Background(), host, raw); err != nil {
		t.Fatal(err)
	}
	waitForPoll := func(what string) {
		t.Helper()
		select {
		case <-client.calls:
		case <-time.After(10 * time.Second): // guard against a hang only
			t.Fatalf("background poller never sent the %s poll", what)
		}
	}
	waitForPoll("first")
	clock.set(now.Add(defaultPollInterval + time.Second))
	if err := module.Reconfigure(context.Background(), host, raw); err != nil {
		t.Fatal(err)
	}
	waitForPoll("second")
	if _, err := pickAt(module, now.Add(time.Minute), "go-a-1"); err == nil || !strings.Contains(err.Error(), "opencode_go_no_capacity") {
		t.Fatalf("Pick() error = %v after a rate-limited poll, want opencode_go_no_capacity", err)
	}
	if err := module.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPollWindowsExpireAtTheirReset is a regression test: an exhausted or
// over-threshold reading never parks the account past the window's reset.
func TestPollWindowsExpireAtTheirReset(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"exhausted monthly": usageFixture(
			usageWindowFixture("ok", 10, now.Add(4*time.Hour)),
			usageWindowFixture("ok", 10, now.Add(24*time.Hour)),
			usageWindowFixture("rate-limited", 100, now.Add(time.Hour)),
		),
		"five-hour over threshold": usageFixture(
			usageWindowFixture("ok", 99, now.Add(time.Hour)),
			usageWindowFixture("ok", 10, now.Add(24*time.Hour)),
			usageWindowFixture("ok", 10, now.Add(20*24*time.Hour)),
		),
	} {
		t.Run(name, func(t *testing.T) {
			client := newCountingUsageClient(body)
			module := configuredModuleWithCredential(t, 97, dashboardFixtureKey, client, &fakeModuleClock{now: now})
			module.pollUsageOnce(context.Background())
			if _, err := pickAt(module, now, "go-a-1"); err == nil || !strings.Contains(err.Error(), "opencode_go_no_capacity") {
				t.Fatalf("Pick() before reset error = %v, want opencode_go_no_capacity", err)
			}
			resp, err := pickAt(module, now.Add(time.Hour+time.Second), "go-a-1")
			if err != nil || !resp.Handled || resp.AuthID != "go-a-1" {
				t.Fatalf("Pick() after reset = %#v, %v; want go-a-1", resp, err)
			}
		})
	}
}

func TestQuotaRefreshIntervalConfig(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{}`))
	if err != nil || cfg.PollInterval != defaultPollInterval {
		t.Fatalf("default poll interval = %s, %v", cfg.PollInterval, err)
	}
	cfg, err = parseConfig(json.RawMessage(`{"quota-refresh-interval":"5m"}`))
	if err != nil || cfg.PollInterval != 5*time.Minute {
		t.Fatalf("poll interval = %s, %v", cfg.PollInterval, err)
	}
	for _, raw := range []string{`{"quota-refresh-interval":"10s"}`, `{"quota-refresh-interval":"soon"}`, `{"quota-refresh-interval":"-1m"}`} {
		if _, err := parseConfig(json.RawMessage(raw)); err == nil || !strings.HasPrefix(err.Error(), "opencode_go_invalid_config") {
			t.Fatalf("parseConfig(%s) error = %v, want opencode_go_invalid_config", raw, err)
		}
	}

	client := newCountingUsageClient(usageFixture(
		usageWindowFixture("ok", 1, time.Now().Add(time.Hour)),
		usageWindowFixture("ok", 1, time.Now().Add(24*time.Hour)),
		usageWindowFixture("ok", 1, time.Now().Add(20*24*time.Hour)),
	))
	module := NewModule()
	raw := json.RawMessage(`{"quota-refresh-interval":"0","dashboard-api-key":"` + dashboardFixtureKey + `","accounts":[{"name":"go-a","auth-ids":["go-a-1"]}]}`)
	if err := module.Reconfigure(context.Background(), providermodule.HostConfig{HTTPClient: client}, raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })
	module.mu.Lock()
	started := module.pollerStarted
	module.mu.Unlock()
	if started {
		t.Fatal(`quota-refresh-interval "0" started the poller`)
	}
}

// TestPollResultIsDroppedWhenTheAccountIsReplaced is a regression test: a
// poll still in flight when Reconfigure rebinds the account to other
// credentials must not write the old key's usage into the new binding.
func TestPollResultIsDroppedWhenTheAccountIsReplaced(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 10, now.Add(time.Hour)),
			usageWindowFixture("ok", 10, now.Add(24*time.Hour)),
			usageWindowFixture("ok", 10, now.Add(20*24*time.Hour)),
		)), nil
	})
	host := providermodule.HostConfig{Clock: &fakeModuleClock{now: now}, HTTPClient: client}
	module := &Module{noPoller: true}
	if err := module.Reconfigure(context.Background(), host, json.RawMessage(`{"accounts":[{"name":"go-a","auth-ids":["go-a-1"],"dashboard-api-key":"test-only-old-key"}]}`)); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{})
	go func() { module.pollUsageOnce(context.Background()); close(polled) }()
	<-started
	if err := module.Reconfigure(context.Background(), host, json.RawMessage(`{"accounts":[{"name":"go-a","auth-ids":["go-a-2"],"dashboard-api-key":"test-only-new-key"}]}`)); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-polled
	module.mu.Lock()
	defer module.mu.Unlock()
	for kind, window := range module.state.Accounts["go-a"].Windows {
		if window.Known {
			t.Fatalf("%s window = %#v after the account was rebound mid-poll, want unknown", kind, window)
		}
	}
}
