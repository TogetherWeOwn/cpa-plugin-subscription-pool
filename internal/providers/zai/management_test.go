package zai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementRegistrationDeclaresAllRoutes(t *testing.T) {
	registration := managementRegistration()
	want := map[string]string{
		http.MethodGet + " " + managementStatusPath:         "",
		http.MethodPost + " " + managementRefreshPath:       "",
		http.MethodPost + " " + managementUnblockPath:       "",
		http.MethodPost + " " + managementAccountConfigPath: "",
	}
	for _, route := range registration.Routes {
		delete(want, route.Method+" "+route.Path)
	}
	if len(registration.Routes) != 4 || len(want) != 0 {
		t.Fatalf("routes = %#v, missing = %#v", registration.Routes, want)
	}
}

func TestResourceStatusPathNoLongerServedByZaiModule(t *testing.T) {
	runtime := &pluginRuntime{}
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + pluginID + "/status",
	})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("zai-level resource status = %d, want 404 now that the coordinator serves the dashboard", response.StatusCode)
	}
}

func TestManagementStatusGoldenAuthoritativeAndFallback(t *testing.T) {
	now := time.Date(2026, time.September, 8, 8, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("golden"), Name: "zai-pro-1", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	state := runtime.snapshot.Quota[item.Identity]
	state.Authoritative = &quotaSnapshot{
		Plan:       "pro",
		FiveHour:   quotaWindow{ConsumedMicrocredits: 5_040 * creditScale, BucketMicrocredits: 12_000 * creditScale, ResetsAt: now.Add(42 * time.Minute)},
		Weekly:     quotaWindow{ConsumedMicrocredits: 10_800 * creditScale, BucketMicrocredits: 60_000 * creditScale, ResetsAt: now.Add(6*24*time.Hour + 19*time.Hour + 21*time.Minute)},
		ObservedAt: now.Add(-12 * time.Second),
	}
	runtime.snapshot.Quota[item.Identity] = state
	assertManagementGolden(t, runtime.managementStatus("registered"), "testdata/status_authoritative.golden.json")

	state.Authoritative = nil
	state.LastPollAttempt = now.Add(-20 * time.Second)
	state.LastPollError = "quota request failed"
	state.Events = []creditEvent{{At: now.Add(-time.Hour), Microcredits: 1_200 * creditScale, Model: "glm-5.3"}}
	runtime.snapshot.Quota[item.Identity] = state
	assertManagementGolden(t, runtime.managementStatus("registered"), "testdata/status_fallback.golden.json")
}

func assertManagementGolden(t *testing.T, value managementStatusBody, path string) {
	t.Helper()
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("status mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestManagementStatusOffpeakUTCBoundaries(t *testing.T) {
	item := account{Identity: accountIdentity("offpeak"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"monday_before", time.Date(2026, time.September, 7, 5, 59, 59, 0, time.UTC), true},
		{"monday_start", time.Date(2026, time.September, 7, 6, 0, 0, 0, time.UTC), false},
		{"friday_end_minus", time.Date(2026, time.September, 11, 9, 59, 59, 0, time.UTC), false},
		{"friday_end", time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC), true},
		{"saturday", time.Date(2026, time.September, 12, 8, 0, 0, 0, time.UTC), true},
		{"sunday", time.Date(2026, time.September, 13, 8, 0, 0, 0, time.UTC), true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runtime := quotaTestRuntime(t, test.at, []account{item})
			got := runtime.managementStatus("registered").Accounts[0].Offpeak
			if got != test.want {
				t.Fatalf("offpeak(%s) = %t, want %t", test.at, got, test.want)
			}
		})
	}
}

func TestManagementRefreshCoalescesAndRedactsFailure(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("refresh"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return quotaHTTPResponse(http.StatusBadGateway, quotaFixtureKey), nil
	})
	first := make(chan pluginapi.ManagementResponse, 1)
	go func() {
		first <- runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRefreshPath})
	}()
	<-started
	secondResult := make(chan pluginapi.ManagementResponse, 1)
	go func() {
		secondResult <- runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRefreshPath})
	}()
	select {
	case <-secondResult:
		t.Fatal("coalesced refresh returned before the in-flight request completed")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	failed := <-first
	second := <-secondResult
	if failed.StatusCode != http.StatusBadGateway || second.StatusCode != http.StatusBadGateway {
		t.Fatalf("coalesced statuses = %d / %d", failed.StatusCode, second.StatusCode)
	}
	for _, response := range []pluginapi.ManagementResponse{failed, second} {
		if strings.Contains(string(response.Body), quotaFixtureKey) || strings.Contains(string(response.Body), "Authorization") {
			t.Fatalf("refresh failure leaked sensitive data: %s", response.Body)
		}
	}
}

func TestManagementRefreshFollowersReceiveTheirGenerationResult(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("refresh-generations"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})

	first, leader, err := runtime.beginRefresh()
	if err != nil || !leader {
		t.Fatalf("begin first refresh: leader=%t err=%v", leader, err)
	}
	follower, leader, err := runtime.beginRefresh()
	if err != nil || leader || follower != first {
		t.Fatalf("join first refresh: leader=%t err=%v follower=%p first=%p", leader, err, follower, first)
	}
	firstErr := errors.New("first generation failed")
	runtime.endRefresh(first, firstErr)

	runtime.mu.Lock()
	replacement := cloneRuntimeSnapshot(runtime.snapshot)
	replacement.Generation++
	runtime.snapshot = replacement
	runtime.mu.Unlock()
	second, leader, err := runtime.beginRefresh()
	if err != nil || !leader || second == first {
		t.Fatalf("begin second refresh: leader=%t err=%v second=%p first=%p", leader, err, second, first)
	}
	secondErr := errors.New("second generation failed")
	runtime.endRefresh(second, secondErr)

	if got := runtime.waitRefresh(context.Background(), follower); !errors.Is(got, firstErr) {
		t.Fatalf("first-generation follower error = %v, want %v", got, firstErr)
	}
	if got := runtime.waitRefresh(context.Background(), second); !errors.Is(got, secondErr) {
		t.Fatalf("second-generation error = %v, want %v", got, secondErr)
	}
}

func TestManagementConcurrentAccountConfigPreservesBothUpdates(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	first := account{Identity: accountIdentity("first-config"), Name: "first", KeySuffix: "first", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	second := account{Identity: accountIdentity("second-config"), Name: "second", KeySuffix: "second", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{first, second})

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, input := range []managementAccountConfigRequest{{Account: "first", Name: "ONE"}, {Account: "second", Name: "TWO"}} {
		input := input
		go func() {
			<-start
			results <- runtime.updateAccountConfig(input)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	settings, err := runtime.snapshot.Store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Accounts[first.Identity].Name != "ONE" || settings.Accounts[second.Identity].Name != "TWO" {
		t.Fatalf("concurrent settings lost an acknowledged update: %#v", settings.Accounts)
	}
}

func TestManagementAccountConfigClearAppliesBaseConfigurationLive(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("clear-live"), Name: "account", KeySuffix: "clear", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	disabled := true
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Name: "renamed", Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "renamed", Clear: true}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Accounts[0].Name != "account" || snapshot.Accounts[0].Disabled {
		t.Fatalf("clear did not restore base configuration live: %#v", snapshot.Accounts[0])
	}
	settings, err := snapshot.Store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := settings.Accounts[item.Identity]; exists {
		t.Fatalf("clear left persisted override: %#v", settings.Accounts)
	}
}

func TestManagementAccountPlanAndClearSurviveCarryForward(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("managed-plan"), Name: "account", KeySuffix: "plan", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Accounts[0].Plan = "lite"
	runtime.snapshot.Accounts[0].FiveHourCredits = planBuckets["lite"].FiveHour
	runtime.snapshot.Accounts[0].WeeklyCredits = planBuckets["lite"].Weekly

	five := int64(20_000)
	weekly := int64(100_000)
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Plan: "custom", FiveHourCredits: &five, WeeklyCredits: &weekly}); err != nil {
		t.Fatal(err)
	}
	configured, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if configured.Accounts[0].Plan != "custom" || configured.Accounts[0].FiveHourCredits != five || configured.Accounts[0].WeeklyCredits != weekly {
		t.Fatalf("managed custom plan was overwritten by carry-forward: %#v", configured.Accounts[0])
	}
	if !configured.Accounts[0].planExplicit || !configured.Accounts[0].fiveHourCreditsExplicit || !configured.Accounts[0].weeklyCreditsExplicit {
		t.Fatalf("managed plan did not retain explicit flags: %#v", configured.Accounts[0])
	}

	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Clear: true}); err != nil {
		t.Fatal(err)
	}
	cleared, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Accounts[0].Plan != "pro" || cleared.Accounts[0].FiveHourCredits != 12_000 || cleared.Accounts[0].WeeklyCredits != 60_000 {
		t.Fatalf("clear did not preserve base plan through carry-forward: %#v", cleared.Accounts[0])
	}
}

func TestManagementNamedPlanClearsInheritedCustomBuckets(t *testing.T) {
	for _, plan := range []string{"lite", "pro"} {
		t.Run(plan, func(t *testing.T) {
			now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
			item := account{Identity: accountIdentity("named-plan-" + plan), Name: "account", KeySuffix: "plan", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
			runtime := quotaTestRuntime(t, now, []account{item})
			five := int64(20_000)
			weekly := int64(100_000)
			if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Plan: "custom", FiveHourCredits: &five, WeeklyCredits: &weekly}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Plan: plan}); err != nil {
				t.Fatal(err)
			}

			snapshot, err := runtime.current()
			if err != nil {
				t.Fatal(err)
			}
			buckets := planBuckets[plan]
			if got := snapshot.Accounts[0]; got.Plan != plan || got.FiveHourCredits != buckets.FiveHour || got.WeeklyCredits != buckets.Weekly {
				t.Fatalf("named plan retained custom buckets live: %#v", got)
			}
			settings, err := snapshot.Store.loadSettings()
			if err != nil {
				t.Fatal(err)
			}
			stored := settings.Accounts[item.Identity]
			if stored.Plan != plan || stored.FiveHourCredits != 0 || stored.WeeklyCredits != 0 {
				t.Fatalf("named plan retained persisted custom buckets: %#v", stored)
			}
		})
	}
}

func TestManagementAccountConfigPersistenceFailureLeavesRuntimeUnchanged(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("persist-failure"), Name: "account", KeySuffix: "persist", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Generation = 7
	runtime.saveSettings = func(*secureStore, settingsFile) error { return errors.New("disk unavailable") }

	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Name: "failed"}); err == nil {
		t.Fatal("persistence failure unexpectedly succeeded")
	}
	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 7 || snapshot.Accounts[0].Name != "account" {
		t.Fatalf("failed settings became live: generation=%d account=%#v", snapshot.Generation, snapshot.Accounts[0])
	}
}

func TestManagementAccountConfigPublishesRecoverablePendingCommit(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("pending-settings"), Name: "account", KeySuffix: "pending", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Generation = 7
	runtime.saveSettings = func(*secureStore, settingsFile) error {
		return &settingsRecoveryPendingError{err: errors.New("directory durability unavailable")}
	}

	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Name: "desired"}); err != nil {
		t.Fatalf("recoverable settings commit was rejected: %v", err)
	}
	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 8 || snapshot.Accounts[0].Name != "desired" {
		t.Fatalf("recoverable settings did not become live: generation=%d account=%#v", snapshot.Generation, snapshot.Accounts[0])
	}
}

func TestManagementAccountConfigShutdownJoinsWriteAndRejectsNewWrites(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("shutdown-write"), Name: "account", KeySuffix: "write", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.saveSettings = func(*secureStore, settingsFile) error {
		close(started)
		<-release
		return nil
	}

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Name: "saved"})
	}()
	<-started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.shutdown() }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before settings write completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join completed settings write")
	}
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Name: "rejected"}); err == nil {
		t.Fatal("post-stop settings write unexpectedly succeeded")
	}
}

func TestManagementAccountConfigShutdownErrorDoesNotPersist(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)
	rawConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")

	var runtime pluginRuntime
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	if err := runtime.shutdown(); err != nil {
		t.Fatal(err)
	}
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementAccountConfigPath,
		Body:   []byte(`{"account":"zai-pro-1","name":"must-not-persist"}`),
	})
	if response.StatusCode == http.StatusOK {
		t.Fatalf("post-shutdown config update succeeded: %s", response.Body)
	}

	var reloaded pluginRuntime
	if err := reloaded.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.shutdown() }()
	snapshot, err := reloaded.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Accounts[0].Name == "must-not-persist" {
		t.Fatal("error response persisted account settings into the next runtime")
	}
}

func TestManagementAccountConfigSerializesWithReconfigure(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)
	rawConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")

	var runtime pluginRuntime
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.shutdown() }()

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- runtime.reconfigure(rawConfig)
	}()
	go func() {
		<-start
		results <- runtime.updateAccountConfig(managementAccountConfigRequest{Account: "zai-pro-1", Name: "managed"})
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Accounts[0].Name != "managed" {
		t.Fatalf("acknowledged account config lost to reconfigure: %#v", snapshot.Accounts[0])
	}
	settings, err := snapshot.Store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Accounts[snapshot.Accounts[0].Identity].Name != "managed" {
		t.Fatalf("stored account config lost: %#v", settings.Accounts)
	}
}

func TestManagementGlobalSettingsSurviveReconfigure(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)
	rawConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")

	var runtime pluginRuntime
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	threshold := 91
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{
		Account:             "zai-pro-1",
		ThresholdPercent:    &threshold,
		PollingInterval:     "1m",
		AuthoritativeMaxAge: "2m",
		Timeout:             "5s",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.shutdown() }()

	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.ThresholdPercent != 91 || snapshot.Config.QuotaRefresh != time.Minute || snapshot.Config.AuthoritativeMaxAge != 2*time.Minute || snapshot.Config.QuotaTimeout != 5*time.Second {
		t.Fatalf("global management settings did not survive reconfigure: %#v", snapshot.Config)
	}
}

func TestManagementAccountConfigRestartsPollersWithLiveSettings(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	item := account{Identity: accountIdentity("live-config"), Name: "account", KeySuffix: "live", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.clock = clock
	polls := make(chan struct{}, 4)
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		polls <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	runtime.cancel = cancel
	runtime.startPollers(ctx, runtime.snapshot)
	<-polls

	disabled := true
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Disabled: &disabled, PollingInterval: "1m", AuthoritativeMaxAge: "2m"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polls:
		t.Fatal("disabled account started another authenticated poll")
	case <-time.After(20 * time.Millisecond):
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("disabled worker slept again: %#v", clock.sleeps)
	}

	disabled = false
	if err := runtime.updateAccountConfig(managementAccountConfigRequest{Account: "account", Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polls:
	case <-time.After(time.Second):
		t.Fatal("enabled account did not start a poller")
	}
	runtime.mu.RLock()
	base := runtime.snapshot.Config.QuotaRefresh
	runtime.mu.RUnlock()
	if base != time.Minute {
		t.Fatalf("live polling interval = %s, want 1m", base)
	}
	_ = runtime.shutdown()
}

func TestManagementRequestValidationAndAccountConfig(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("config"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})

	oversized := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: make([]byte, maxManagementRequestBody+1)})
	if oversized.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", oversized.StatusCode)
	}
	invalid := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: []byte(`{"account":"account","threshold_percent":0}`)})
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid config status = %d", invalid.StatusCode)
	}
	invalid = runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: []byte(`{"account":"account","polling_interval":"30s"}`)})
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid polling status = %d", invalid.StatusCode)
	}
	invalid = runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: []byte(`{"account":"account","timeout":"31s"}`)})
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid timeout status = %d", invalid.StatusCode)
	}
	invalid = runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: []byte(`{"account":"missing","name":"renamed"}`)})
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing account status = %d", invalid.StatusCode)
	}

	threshold := 95
	five := int64(20_000)
	weekly := int64(100_000)
	body, err := json.Marshal(managementAccountConfigRequest{Account: "account", Name: "renamed", Plan: "custom", FiveHourCredits: &five, WeeklyCredits: &weekly, ThresholdPercent: &threshold, PollingInterval: "1m", AuthoritativeMaxAge: "2m", Timeout: "5s"})
	if err != nil {
		t.Fatal(err)
	}
	valid := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementAccountConfigPath, Body: body})
	if valid.StatusCode != http.StatusOK {
		t.Fatalf("valid config status = %d body=%s", valid.StatusCode, valid.Body)
	}
	if runtime.snapshot.Accounts[0].Name != "renamed" || runtime.snapshot.Config.ThresholdPercent != threshold || runtime.snapshot.Config.QuotaTimeout != 5*time.Second {
		t.Fatalf("runtime config not updated: %#v %#v", runtime.snapshot.Accounts[0], runtime.snapshot.Config)
	}
	raw, err := os.ReadFile(filepath.Join(runtime.snapshot.Store.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "config") {
		t.Fatal("settings exposed account selector instead of hashed identity")
	}
}

func TestManagementUnblockRecomputesWithoutManufacturingCapacity(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("unblock"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 100, WeeklyCredits: 100}
	runtime := quotaTestRuntime(t, now, []account{item})
	state := runtime.snapshot.Quota[item.Identity]
	state.Events = []creditEvent{{At: now.Add(-time.Hour), Microcredits: 98 * creditScale, Model: "glm-5.3"}}
	runtime.snapshot.Quota[item.Identity] = state
	before := runtime.managementStatus("registered").Accounts[0]
	if before.Health != "exhausted" {
		t.Fatalf("before health = %s", before.Health)
	}
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementUnblockPath, Body: []byte(`{"account":"account"}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unblock status = %d body=%s", response.StatusCode, response.Body)
	}
	after := runtime.managementStatus("registered").Accounts[0]
	if after.Health != "exhausted" || after.FiveHourUtilization != before.FiveHourUtilization {
		t.Fatalf("unblock manufactured capacity: before=%#v after=%#v", before, after)
	}
}

func TestManagementUnblockPreservesAuthenticationSuspension(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("unblock-suspended"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 100, WeeklyCredits: 100}
	runtime := quotaTestRuntime(t, now, []account{item})
	health := runtime.snapshot.Health[item.Identity]
	health.SuspendedUntil = now.Add(time.Hour)
	health.ExhaustedUntil = now.Add(30 * time.Minute)
	health.ExhaustedReason = "upstream quota response"
	runtime.snapshot.Health[item.Identity] = health

	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementUnblockPath, Body: []byte(`{"account":"account"}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unblock status = %d body=%s", response.StatusCode, response.Body)
	}
	got := runtime.snapshot.Health[item.Identity]
	if !got.SuspendedUntil.Equal(health.SuspendedUntil) {
		t.Fatalf("unblock cleared authentication suspension: before=%s after=%s", health.SuspendedUntil, got.SuspendedUntil)
	}
	if !got.ExhaustedUntil.IsZero() || got.ExhaustedReason != "" {
		t.Fatalf("unblock retained upstream quota block: %#v", got)
	}
	if assessed, ok := runtime.health(item.Identity); !ok || assessed.Status != healthSuspended {
		t.Fatalf("unblock changed suspended account health: %#v ok=%v", assessed, ok)
	}
}

func TestManagementUnblockRejectsAmbiguousSelector(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	accounts := []account{
		{Identity: accountIdentity("first-redacted"), Name: "first", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 100, WeeklyCredits: 100},
		{Identity: accountIdentity("second-redacted"), Name: "second", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 100, WeeklyCredits: 100},
	}
	runtime := quotaTestRuntime(t, now, accounts)
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementUnblockPath, Body: []byte(`{"account":"redacted"}`)})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "ambiguous") {
		t.Fatalf("ambiguous unblock response = %d %s", response.StatusCode, response.Body)
	}
	response = runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementUnblockPath, Body: []byte(`{}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit all-accounts unblock response = %d %s", response.StatusCode, response.Body)
	}
}

func TestManagementUnknownRouteReturnsHTTPNotFound(t *testing.T) {
	runtime := &pluginRuntime{}
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/other"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestManagementRefreshBoundedTimeout(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("timeout"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Config.QuotaTimeout = time.Millisecond
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	started := time.Now()
	response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRefreshPath})
	if response.StatusCode != http.StatusBadGateway || time.Since(started) > time.Second {
		t.Fatalf("bounded refresh status=%d elapsed=%s", response.StatusCode, time.Since(started))
	}
}

// The refresh route takes no parameters. A body carrying fields must be
// refused before it spends a quota poll with the account's key, while an empty
// body or an empty object still refreshes.
func TestManagementRefreshRejectsNonEmptyBodyBeforePolling(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("refresh-body"), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	polls := 0
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		polls++
		return quotaHTTPResponse(http.StatusBadGateway, ""), nil
	})
	for _, body := range []string{`{"account":"refresh-body"}`, `{"force":true}`, `[]`, `not-json`, `{} {}`} {
		response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRefreshPath, Body: []byte(body)})
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "invalid_request") {
			t.Fatalf("body %q: status = %d body = %s, want 400 invalid_request", body, response.StatusCode, response.Body)
		}
	}
	if polls != 0 {
		t.Fatalf("rejected refresh requests polled quota %d times", polls)
	}
	for _, body := range []string{"", " \n", "{}"} {
		before := polls
		response := runtime.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRefreshPath, Body: []byte(body)})
		if response.StatusCode == http.StatusBadRequest || polls == before {
			t.Fatalf("body %q: status = %d polls %d -> %d, want a quota poll", body, response.StatusCode, before, polls)
		}
	}
}
