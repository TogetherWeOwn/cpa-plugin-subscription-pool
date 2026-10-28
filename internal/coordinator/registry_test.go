package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

// fakeModule is a table-configurable providermodule.Module for exercising
// the coordinator's dispatch invariants without any real provider logic.
type fakeModule struct {
	id          string
	recognize   func(pluginapi.SchedulerAuthCandidate) bool
	ownedIDs    []string
	pick        func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error)
	handleUsage func(pluginapi.UsageRecord) error
	usageCalls  []pluginapi.UsageRecord
}

func (m *fakeModule) ID() string { return m.id }

func (m *fakeModule) Recognize(candidate pluginapi.SchedulerAuthCandidate) bool {
	if m.recognize == nil {
		return false
	}
	return m.recognize(candidate)
}

func (m *fakeModule) Reconfigure(context.Context, providermodule.HostConfig, json.RawMessage) error {
	return nil
}

func (m *fakeModule) OwnedAuthIDs(context.Context) []string { return m.ownedIDs }

func (m *fakeModule) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if m.pick == nil {
		return pluginapi.SchedulerPickResponse{Handled: true}, nil
	}
	return m.pick(req)
}

func (m *fakeModule) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	m.usageCalls = append(m.usageCalls, record)
	if m.handleUsage == nil {
		return nil
	}
	return m.handleUsage(record)
}

func (m *fakeModule) Status(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (m *fakeModule) Close(context.Context) error { return nil }

func recognizeID(prefix string) func(pluginapi.SchedulerAuthCandidate) bool {
	return func(candidate pluginapi.SchedulerAuthCandidate) bool {
		return strings.HasPrefix(candidate.ID, prefix)
	}
}

// Invariant 1: overlapping ownership / duplicate auth IDs across providers
// is rejected, and the registry stays untouched.
func TestBuildRegistryRejectsOverlappingOwnership(t *testing.T) {
	_, err := buildRegistry(map[string][]string{
		"alpha": {"shared-auth"},
		"beta":  {"shared-auth"},
	})
	if err == nil {
		t.Fatal("buildRegistry() with overlapping auth id = nil error, want conflict")
	}
}

func TestBuildRegistryAcceptsDisjointOwnership(t *testing.T) {
	byAuth, err := buildRegistry(map[string][]string{
		"alpha": {"alpha-1", "alpha-2"},
		"beta":  {"beta-1"},
	})
	if err != nil {
		t.Fatalf("buildRegistry() error = %v", err)
	}
	if byAuth["alpha-1"].providerID != "alpha" || byAuth["beta-1"].providerID != "beta" {
		t.Fatalf("byAuth = %#v", byAuth)
	}
}

// Invariant 2: zero recognized families yields Handled:false, and no
// module's Pick is ever called.
func TestCoordinatorPickNoRecognizersYieldsUnhandled(t *testing.T) {
	alpha := &fakeModule{id: "alpha", recognize: recognizeID("alpha-")}
	c := New(alpha)
	resp, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other-auth"}}})
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.Handled {
		t.Fatalf("pick() = %#v, want Handled:false", resp)
	}
}

// Invariant 3/6: more than one recognizing family fails closed and never
// blends results from multiple modules' Pick.
func TestCoordinatorPickMixedProvidersFailsClosed(t *testing.T) {
	alphaPicked := false
	betaPicked := false
	alpha := &fakeModule{id: "alpha", recognize: recognizeID("shared-"), pick: func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		alphaPicked = true
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "alpha-pick"}, nil
	}}
	beta := &fakeModule{id: "beta", recognize: recognizeID("shared-"), pick: func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		betaPicked = true
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "beta-pick"}, nil
	}}
	c := New(alpha, beta)
	_, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "shared-auth"}}})
	if err == nil {
		t.Fatal("pick() with candidates recognized by two providers = nil error, want fail-closed")
	}
	if alphaPicked || betaPicked {
		t.Fatal("pick() called a module's Pick despite mixed-provider recognition")
	}
}

// A single recognizing module's Pick result (success or error) propagates
// verbatim — this also covers invariant 5 (recognized-but-unmanaged/
// all-impaired fails closed via the module's own error, not a coordinator
// substitute).
func TestCoordinatorPickSingleRecognizerPropagatesResult(t *testing.T) {
	alpha := &fakeModule{id: "alpha", recognize: recognizeID("alpha-"), pick: func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "alpha-1"}, nil
	}}
	c := New(alpha)
	resp, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}})
	if err != nil || !resp.Handled || resp.AuthID != "alpha-1" {
		t.Fatalf("pick() = %#v, %v", resp, err)
	}
}

func TestCoordinatorPickPropagatesModuleError(t *testing.T) {
	alpha := &fakeModule{id: "alpha", recognize: recognizeID("alpha-"), pick: func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		return pluginapi.SchedulerPickResponse{}, errAllImpaired
	}}
	c := New(alpha)
	_, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "alpha-1"}}})
	if err != errAllImpaired {
		t.Fatalf("pick() error = %v, want %v", err, errAllImpaired)
	}
}

// Invariant 4: usage dispatch is strictly by auth ID via the registry,
// never by record.Provider.
func TestCoordinatorHandleUsageDispatchesByAuthIDOnly(t *testing.T) {
	alpha := &fakeModule{id: "alpha"}
	beta := &fakeModule{id: "beta"}
	c := New(alpha, beta)
	c.registry.swap(map[string]registryEntry{"alpha-1": {providerID: "alpha"}})

	if err := c.handleUsage(pluginapi.UsageRecord{AuthID: "alpha-1", Provider: "beta"}); err != nil {
		t.Fatalf("handleUsage() error = %v", err)
	}
	if len(alpha.usageCalls) != 1 || len(beta.usageCalls) != 0 {
		t.Fatalf("alpha calls = %d, beta calls = %d, want dispatch by auth id despite Provider:\"beta\"", len(alpha.usageCalls), len(beta.usageCalls))
	}
}

func TestCoordinatorHandleUsageUnmappedAuthIDIsNoop(t *testing.T) {
	alpha := &fakeModule{id: "alpha"}
	c := New(alpha)
	if err := c.handleUsage(pluginapi.UsageRecord{AuthID: "unknown"}); err != nil {
		t.Fatalf("handleUsage() error = %v", err)
	}
	if len(alpha.usageCalls) != 0 {
		t.Fatalf("usageCalls = %d, want 0 for an unmapped auth id", len(alpha.usageCalls))
	}
}

var errAllImpaired = fakeSchedulerError("all_impaired")

type fakeSchedulerError string

func (e fakeSchedulerError) Error() string { return string(e) }

// recognizingFakeModule adds the optional UsageRecognizer to fakeModule.
type recognizingFakeModule struct {
	fakeModule
	claims func(pluginapi.UsageRecord) bool
}

func (m *recognizingFakeModule) RecognizeUsage(record pluginapi.UsageRecord) bool {
	return m.claims != nil && m.claims(record)
}

// The registry still wins; the UsageRecognizer fallback only applies to
// auth IDs no module owns, and only when exactly one module claims them.
func TestCoordinatorHandleUsageFallsBackToSingleUsageRecognizer(t *testing.T) {
	alpha := &fakeModule{id: "alpha"}
	beta := &recognizingFakeModule{fakeModule: fakeModule{id: "beta"}, claims: func(r pluginapi.UsageRecord) bool { return r.Provider == "beta" }}
	c := New(alpha, beta)
	c.registry.swap(map[string]registryEntry{"alpha-1": {providerID: "alpha"}})

	_ = c.handleUsage(pluginapi.UsageRecord{AuthID: "alpha-1", Provider: "beta"})
	_ = c.handleUsage(pluginapi.UsageRecord{AuthID: "rekeyed", Provider: "beta"})
	_ = c.handleUsage(pluginapi.UsageRecord{AuthID: "other", Provider: "gamma"})
	if len(alpha.usageCalls) != 1 || len(beta.usageCalls) != 1 || beta.usageCalls[0].AuthID != "rekeyed" {
		t.Fatalf("alpha calls = %d, beta calls = %v", len(alpha.usageCalls), beta.usageCalls)
	}
}

func TestCoordinatorHandleUsageAmbiguousRecognizersIsNoop(t *testing.T) {
	claimAll := func(pluginapi.UsageRecord) bool { return true }
	alpha := &recognizingFakeModule{fakeModule: fakeModule{id: "alpha"}, claims: claimAll}
	beta := &recognizingFakeModule{fakeModule: fakeModule{id: "beta"}, claims: claimAll}
	c := New(alpha, beta)
	_ = c.handleUsage(pluginapi.UsageRecord{AuthID: "unknown"})
	if len(alpha.usageCalls) != 0 || len(beta.usageCalls) != 0 {
		t.Fatalf("ambiguous usage dispatched: alpha %d beta %d", len(alpha.usageCalls), len(beta.usageCalls))
	}
}
