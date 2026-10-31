package zai

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var schedulerNow = time.Date(2026, time.September, 9, 1, 0, 0, 0, time.UTC)

func TestUsageFailureImpairsBothPairedCredentials(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})

	for _, authID := range []string{account.ClaudeAuthID, account.OpenAIAuthID} {
		_, err := runtime.pick(schedulerRequest(authID))
		assertSchedulerError(t, err, "zai_no_capacity")
	}
	health, ok := runtime.health(account.Identity)
	if !ok || health.Status != healthSuspended || !health.ResetAt.Equal(schedulerNow.Add(defaultSuspend)) {
		t.Fatalf("health = %#v, ok = %v", health, ok)
	}
}

func TestRepeatedFailuresExtendNeverShortenBlocks(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"3600"}}})
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"60"}}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(time.Hour)) {
		t.Fatalf("rate-limit reset = %v, want %v", health.ResetAt, schedulerNow.Add(time.Hour))
	}

	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"7200"}}})
	health, _ = runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(2 * time.Hour)) {
		t.Fatalf("extended reset = %v, want %v", health.ResetAt, schedulerNow.Add(2*time.Hour))
	}
}

func TestResetHintsAreBoundedAndUseAuthoritativeThenFallback(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	generation := runtime.snapshot.Generation
	if !runtime.updateCapacity(generation, account.Identity, capacityUpdate{ResetAt: schedulerNow.Add(3 * time.Hour), Source: "authoritative quota"}) {
		t.Fatal("capacity update rejected")
	}
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"retry_after":999999999999}`}, ResponseHeaders: http.Header{"Retry-After": []string{"999999999999"}}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(3*time.Hour)) || health.Reason != "authoritative quota reset" {
		t.Fatalf("authoritative reset not chosen: %#v", health)
	}

	other := schedulerAccount("two", "claude-two", "openai-two")
	runtime = schedulerTestRuntime(schedulerNow, other)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: other.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: strings.Repeat("x", maxFailureBodyBytes+1)}})
	health, _ = runtime.health(other.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(defaultFallback)) || health.Reason != "conservative rate-limit cooldown" {
		t.Fatalf("fallback reset = %#v", health)
	}
}

func TestRetryAfterMillisecondsUsesRelativeDuration(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"retry_after_ms":1800000}`}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(30 * time.Minute)) {
		t.Fatalf("rate-limit reset = %v, want %v", health.ResetAt, schedulerNow.Add(30*time.Minute))
	}
}

func TestAuthSuspensionPrecedesExhaustionAndBothRecover(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"7200"}}})
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}})
	health, _ := runtime.health(account.Identity)
	if health.Status != healthSuspended {
		t.Fatalf("health = %#v, want suspension precedence", health)
	}

	runtime.now = func() time.Time { return schedulerNow.Add(defaultSuspend + time.Second) }
	health, _ = runtime.health(account.Identity)
	if health.Status != healthExhausted {
		t.Fatalf("health = %#v, want remaining exhaustion", health)
	}
	runtime.now = func() time.Time { return schedulerNow.Add(2*time.Hour + time.Second) }
	health, _ = runtime.health(account.Identity)
	if health.Status != healthHealthy {
		t.Fatalf("health = %#v, want recovered", health)
	}
}

func TestExhaustedCapacityWithStaleResetRemainsFailClosed(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := &pluginRuntime{now: func() time.Time { return schedulerNow }}
	snapshot := schedulerSnapshot(account)
	snapshot.Quota = nil
	if err := runtime.commitSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	generation := runtime.snapshot.Generation
	if !runtime.updateCapacity(generation, account.Identity, capacityUpdate{Exhausted: true, ResetAt: schedulerNow.Add(-time.Second), Source: "authoritative"}) {
		t.Fatal("exhausted capacity update rejected")
	}
	for _, authID := range []string{account.ClaudeAuthID, account.OpenAIAuthID} {
		_, err := runtime.pick(schedulerRequest(authID))
		assertSchedulerError(t, err, "zai_no_capacity")
	}
	health, ok := runtime.health(account.Identity)
	if !ok || health.Status != healthExhausted || !health.ResetAt.IsZero() {
		t.Fatalf("health = %#v, ok = %v, want fail-closed exhaustion without stale reset hint", health, ok)
	}
	if !runtime.updateCapacity(generation, account.Identity, capacityUpdate{Exhausted: false, Source: "fresh authoritative"}) {
		t.Fatal("fresh recovery update rejected")
	}
	response, err := runtime.pick(schedulerRequest(account.ClaudeAuthID, account.OpenAIAuthID))
	if err != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("fresh non-exhausted observation did not recover scheduler: %#v err=%v", response, err)
	}
}

func TestCapacityGenerationBoundaryAndRecovery(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	oldGeneration := runtime.snapshot.Generation
	staged := schedulerSnapshot(account)
	if err := runtime.commitSnapshot(staged); err != nil {
		t.Fatal(err)
	}
	if runtime.updateCapacity(oldGeneration, account.Identity, capacityUpdate{Exhausted: true, ResetAt: schedulerNow.Add(time.Hour), Source: "stale"}) {
		t.Fatal("stale generation mutated replacement snapshot")
	}
	if !runtime.updateCapacity(runtime.snapshot.Generation, account.Identity, capacityUpdate{Exhausted: true, ResetAt: schedulerNow.Add(time.Hour), Source: "authoritative"}) {
		t.Fatal("current generation update rejected")
	}
	if health, _ := runtime.health(account.Identity); health.Status != healthExhausted {
		t.Fatalf("health = %#v, want exhausted", health)
	}
	if !runtime.updateCapacity(runtime.snapshot.Generation, account.Identity, capacityUpdate{Exhausted: false, Source: "authoritative"}) {
		t.Fatal("recovery update rejected")
	}
	if health, _ := runtime.health(account.Identity); health.Status != healthHealthy {
		t.Fatalf("health = %#v, want healthy", health)
	}
}

func TestSchedulerHealthyDelegatesBuiltinRoundRobin(t *testing.T) {
	accounts := []account{
		schedulerAccount("one", "claude-one", "openai-one"),
		schedulerAccount("two", "claude-two", "openai-two"),
	}
	runtime := schedulerTestRuntime(schedulerNow, accounts...)
	response, err := runtime.pick(schedulerRequest("claude-one", "claude-two"))
	if err != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin || response.AuthID != "" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestSchedulerAcceptsBothClaudeHostIDRecipes(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	item := accounts[0]
	if len(item.claudeAuthIDs) != 2 {
		t.Fatalf("Claude auth IDs = %q, want legacy and current host recipes", item.claudeAuthIDs)
	}
	for _, authID := range item.claudeAuthIDs {
		runtime := schedulerTestRuntime(schedulerNow, item)
		response, errPick := runtime.pick(schedulerRequest(authID))
		if errPick != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
			t.Fatalf("host auth ID %q was not managed: %#v err=%v", authID, response, errPick)
		}
		if errUsage := runtime.handleUsage(pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}}); errUsage != nil {
			t.Fatal(errUsage)
		}
		_, errPick = runtime.pick(schedulerRequest(item.OpenAIAuthID))
		assertSchedulerError(t, errPick, "zai_no_capacity")
	}
}

func TestSchedulerRecognizesNamespacedOpenAICompatibilityProvider(t *testing.T) {
	runtime := schedulerTestRuntime(schedulerNow, schedulerAccount("one", "claude-one", "openai-one"))
	candidate := pluginapi.SchedulerAuthCandidate{
		ID:       "unknown-zai",
		Provider: "openai-compatible-" + zaiCompatName,
	}
	_, err := runtime.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{candidate}})
	assertSchedulerError(t, err, "zai_unmanaged_candidate")
}

func TestSchedulerDegradedExcludesSiblingsAndRoundRobinsHealthy(t *testing.T) {
	impaired := schedulerAccount("impaired", "claude-bad", "openai-bad")
	first := schedulerAccount("first", "claude-one", "openai-one")
	second := schedulerAccount("second", "claude-two", "openai-two")
	runtime := schedulerTestRuntime(schedulerNow, impaired, first, second)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: impaired.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	req := schedulerRequest(impaired.ClaudeAuthID, impaired.OpenAIAuthID, first.ClaudeAuthID, second.ClaudeAuthID)

	one, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	two, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	three, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	if one.AuthID != first.ClaudeAuthID || two.AuthID != second.ClaudeAuthID || three.AuthID != first.ClaudeAuthID {
		t.Fatalf("round robin = %q, %q, %q", one.AuthID, two.AuthID, three.AuthID)
	}
}

func TestSchedulerStickinessSurvivesUntilCandidateIsImpaired(t *testing.T) {
	bad := schedulerAccount("bad", "claude-bad", "openai-bad")
	first := schedulerAccount("first", "claude-first", "openai-first")
	second := schedulerAccount("second", "claude-second", "openai-second")
	sticky := schedulerAccount("sticky", "claude-sticky", "openai-sticky")
	runtime := schedulerTestRuntime(schedulerNow, bad, first, second, sticky)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: bad.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}})
	req := schedulerRequest(bad.ClaudeAuthID, first.ClaudeAuthID, second.ClaudeAuthID, sticky.ClaudeAuthID)
	req.Options.Headers = map[string][]string{"session_id": []string{"s6"}}

	one, err := runtime.pick(req)
	if err != nil || one.AuthID != sticky.ClaudeAuthID {
		t.Fatalf("initial sticky pick = %#v, err = %v", one, err)
	}
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: first.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	two, err := runtime.pick(req)
	if err != nil || two.AuthID != sticky.ClaudeAuthID {
		t.Fatalf("unrelated impairment changed sticky pick = %#v, err = %v", two, err)
	}
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: sticky.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	three, err := runtime.pick(req)
	if err != nil || three.AuthID == sticky.ClaudeAuthID || three.AuthID == bad.ClaudeAuthID || three.AuthID == first.ClaudeAuthID {
		t.Fatalf("failover pick = %#v, err = %v", three, err)
	}
}

func TestSchedulerUnmanagedAndAllImpairedPaths(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	response, err := runtime.pick(schedulerRequest("unmanaged"))
	if err != nil || response.Handled {
		t.Fatalf("unmanaged response = %#v, err = %v", response, err)
	}

	_, err = runtime.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unknown-zai", Attributes: map[string]string{"base_url": zaiAnthropicBaseURL}}}})
	assertSchedulerError(t, err, "zai_unmanaged_candidate")

	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}})
	response, err = runtime.pick(schedulerRequest(account.ClaudeAuthID, "unmanaged"))
	if response.Handled {
		t.Fatalf("hard error returned handled response: %#v", response)
	}
	assertSchedulerError(t, err, "zai_no_capacity")
}

func TestSchedulerPickNeverCallsExternalDependencies(t *testing.T) {
	bad := schedulerAccount("bad", "claude-bad", "openai-bad")
	good := schedulerAccount("good", "claude-good", "openai-good")
	runtime := schedulerTestRuntime(schedulerNow, bad, good)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: bad.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})

	done := make(chan error, 1)
	go func() {
		_, err := runtime.pick(schedulerRequest(bad.ClaudeAuthID, good.ClaudeAuthID))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler critical section blocked on an external dependency")
	}
}

func TestUsageCrossingQuotaThresholdImmediatelyBlocksScheduler(t *testing.T) {
	now := schedulerNow
	item := schedulerAccount("one", "claude-one", "openai-one")
	item.FiveHourCredits = 1
	item.WeeklyCredits = 100
	runtime := quotaTestRuntime(t, now, []account{item})
	current := now
	runtime.now = func() time.Time { return current }

	if err := runtime.handleUsage(pluginapi.UsageRecord{
		AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now,
		Detail: pluginapi.UsageDetail{InputTokens: 4_000},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := runtime.pick(schedulerRequest(item.ClaudeAuthID, item.OpenAIAuthID))
	if response.Handled {
		t.Fatalf("hard error returned handled response: %#v", response)
	}
	assertSchedulerError(t, err, "zai_no_capacity")

	current = now.Add(fiveHourWindow + time.Second)
	response, err = runtime.pick(schedulerRequest(item.ClaudeAuthID, item.OpenAIAuthID))
	if err != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("recovered response = %#v, err = %v", response, err)
	}
}

func TestFailureHealthPersistsAcrossReconfigure(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)
	rawConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")

	first := &pluginRuntime{now: func() time.Time { return schedulerNow }}
	if err := first.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	account := first.snapshot.Accounts[0]
	if err := first.handleUsage(pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}}); err != nil {
		t.Fatal(err)
	}

	second := &pluginRuntime{now: func() time.Time { return schedulerNow }}
	if err := second.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	for _, authID := range []string{account.ClaudeAuthID, account.OpenAIAuthID} {
		_, err := second.pick(schedulerRequest(authID))
		assertSchedulerError(t, err, "zai_no_capacity")
	}
}

func TestManagementHealthMatchesSchedulerHealth(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}}); err != nil {
		t.Fatal(err)
	}
	status := runtime.managementStatus("registered")
	if len(status.Accounts) != 1 || status.Accounts[0].Health != healthSuspended {
		t.Fatalf("management health = %#v, want suspended", status.Accounts)
	}
	_, err := runtime.pick(schedulerRequest(account.OpenAIAuthID))
	assertSchedulerError(t, err, "zai_no_capacity")
}

func TestValidateSchedulerDeploymentFailsClosed(t *testing.T) {
	enabled := true
	disabled := false
	tests := []struct {
		name    string
		plugins cpaPluginsProjection
	}{
		{name: "plugins disabled", plugins: cpaPluginsProjection{}},
		{name: "plugin disabled", plugins: cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &disabled, Priority: requiredPluginPriority}}}},
		{name: "wrong priority", plugins: cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &enabled, Priority: 999}}}},
		{name: "lower competitor", plugins: schedulerPlugins(&enabled, "zz-lower", 1)},
		{name: "higher competitor", plugins: schedulerPlugins(&enabled, "aa-higher", 2000)},
		{name: "equal competitor wins tie", plugins: schedulerPlugins(&enabled, "aa-equal", requiredPluginPriority)},
		{name: "equal competitor loses tie", plugins: schedulerPlugins(&enabled, "zz-equal", requiredPluginPriority)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSchedulerDeployment(tt.plugins); err == nil {
				t.Fatal("non-exclusive scheduler deployment succeeded")
			}
		})
	}
	valid := cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &enabled, Priority: requiredPluginPriority}, "disabled": {Enabled: &disabled, Priority: 5000}}}
	if err := validateSchedulerDeployment(valid); err != nil {
		t.Fatalf("valid exclusive deployment rejected: %v", err)
	}
}

func schedulerPlugins(enabled *bool, competitor string, priority int) cpaPluginsProjection {
	return cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{
		pluginID:   {Enabled: enabled, Priority: requiredPluginPriority},
		competitor: {Enabled: enabled, Priority: priority},
	}}
}

func schedulerAccount(name, claudeID, openAIID string) account {
	return account{Identity: accountIdentity(name), Name: name, Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: claudeID, OpenAIAuthID: openAIID}
}

func schedulerSnapshot(accounts ...account) *runtimeSnapshot {
	return newRuntimeSnapshot(pluginConfig{SuspendDuration: defaultSuspend, FallbackCooldown: defaultFallback}, accounts, nil)
}

func schedulerTestRuntime(now time.Time, accounts ...account) *pluginRuntime {
	runtime := &pluginRuntime{now: func() time.Time { return now }}
	if err := runtime.commitSnapshot(schedulerSnapshot(accounts...)); err != nil {
		panic(err)
	}
	return runtime
}

func schedulerRequest(authIDs ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Model: "glm-5.3"}
	for _, authID := range authIDs {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: authID})
	}
	return req
}

func mustHandleUsage(t *testing.T, runtime *pluginRuntime, record pluginapi.UsageRecord) {
	t.Helper()
	if err := runtime.handleUsage(record); err != nil {
		t.Fatal(err)
	}
}

func assertSchedulerError(t *testing.T, err error, code string) {
	t.Helper()
	var schedulerErr *envelopeError
	if !errors.As(err, &schedulerErr) || schedulerErr.Code != code || schedulerErr.Retryable {
		t.Fatalf("error = %T %v, want non-retryable %s", err, err, code)
	}
}

func TestSchedulerMixedForeignCandidateNeverPicksForeignOrImpairedLane(t *testing.T) {
	impaired := schedulerAccount("impaired", "claude-bad", "openai-bad")
	healthy := schedulerAccount("healthy", "claude-good", "openai-good")
	runtime := schedulerTestRuntime(schedulerNow, impaired, healthy)
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: impaired.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	for i := 0; i < 3; i++ {
		response, err := runtime.pick(schedulerRequest(impaired.ClaudeAuthID, "foreign-auth", healthy.ClaudeAuthID))
		if err != nil || response.AuthID != healthy.ClaudeAuthID {
			t.Fatalf("mixed pick = %#v, err = %v; want %s", response, err, healthy.ClaudeAuthID)
		}
	}
}
