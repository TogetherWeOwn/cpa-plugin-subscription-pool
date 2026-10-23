package zai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

var _ providermodule.Module = (*zaiModule)(nil)

func TestNewModuleID(t *testing.T) {
	module := NewModule()
	if module.ID() != "zai" {
		t.Fatalf("ID() = %q, want %q", module.ID(), "zai")
	}
}

// TestRecognizeManagedAccountByBareAuthID pins the coordinator dispatch
// contract: the host constructs scheduler candidates for already-known auth
// records as bare {ID: authID} (see schedulerRequest in scheduler_test.go
// and the real pluginapi.SchedulerAuthCandidate the CLIProxyAPI host sends),
// carrying none of the attributes recognizedZAICandidate inspects. The
// pre-coordinator pick() treated snapshot membership as recognition in its
// own right; Recognize must not narrow that or the coordinator's dispatch
// (internal/coordinator.pick) sees zero recognizing modules and answers
// Handled:false for perfectly healthy, managed traffic.
func TestRecognizeManagedAccountByBareAuthID(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	module := &zaiModule{runtime: runtime}

	for _, authID := range []string{account.ClaudeAuthID, account.OpenAIAuthID} {
		if !module.Recognize(pluginapi.SchedulerAuthCandidate{ID: authID}) {
			t.Fatalf("Recognize(%q) = false, want true for a managed bare-ID candidate", authID)
		}
	}

	if module.Recognize(pluginapi.SchedulerAuthCandidate{ID: "unmanaged-auth-id"}) {
		t.Fatal("Recognize() = true for an auth ID absent from the snapshot")
	}
}
