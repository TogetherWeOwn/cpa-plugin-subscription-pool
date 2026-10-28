package coordinator

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

const zaiIntegrationFixtureKey = "test-only-coordinator-zai-key"

// writeCoordinatorCPAConfigFixture writes a host CLIProxyAPI config in the
// coordinator's own nested shape: plugins.configs.subscription-pool, with
// the zai provider's single credential pair registered.
func writeCoordinatorCPAConfigFixture(t *testing.T, path, authDir, key string) {
	t.Helper()
	raw := "auth-dir: " + authDir + "\n" +
		"plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    subscription-pool:\n" +
		"      enabled: true\n" +
		"      priority: 1000\n" +
		"claude-api-key:\n" +
		"  - api-key: " + key + "\n" +
		"    base-url: https://api.z.ai/api/anthropic\n" +
		"openai-compatibility:\n" +
		"  - name: zai-coding-plan\n" +
		"    base-url: https://api.z.ai/api/coding/paas/v4\n" +
		"    api-key-entries:\n" +
		"      - api-key: " + key + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// These two tests wire the real zai module through the coordinator to prove
// invariant 5 (recognized-but-unmanaged / all-impaired fails closed via the
// module's own error, propagated verbatim rather than a coordinator
// substitute) end-to-end, not just via fakeModule.
func TestCoordinatorPropagatesZaiUnmanagedCandidateError(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCoordinatorCPAConfigFixture(t, configPath, authDir, zaiIntegrationFixtureKey)

	c := New(zai.NewModule())
	rawConfig := []byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n")
	if err := c.reconfigure(rawConfig); err != nil {
		t.Fatalf("reconfigure() error = %v", err)
	}

	_, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "unrecognized-zai-auth-id", Attributes: map[string]string{"base_url": "https://api.z.ai/api/anthropic"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "zai_unmanaged_candidate") {
		t.Fatalf("pick() error = %v, want zai_unmanaged_candidate propagated verbatim", err)
	}
}

func TestCoordinatorPropagatesZaiNoCapacityError(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCoordinatorCPAConfigFixture(t, configPath, authDir, zaiIntegrationFixtureKey)

	c := New(zai.NewModule())
	rawConfig := []byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n")
	if err := c.reconfigure(rawConfig); err != nil {
		t.Fatalf("reconfigure() error = %v", err)
	}

	ownedAuthIDs := c.modules[0].module.OwnedAuthIDs(context.Background())
	if len(ownedAuthIDs) == 0 {
		t.Fatal("zai module reported no owned auth ids after reconfigure")
	}

	for _, authID := range ownedAuthIDs {
		if err := c.handleUsage(pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}}); err != nil {
			t.Fatalf("handleUsage(%q) error = %v", authID, err)
		}
	}

	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(ownedAuthIDs))
	for _, authID := range ownedAuthIDs {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:         authID,
			Attributes: map[string]string{"base_url": "https://api.z.ai/api/anthropic"},
		})
	}
	_, err := c.pick(pluginapi.SchedulerPickRequest{Candidates: candidates})
	if err == nil || !strings.Contains(err.Error(), "zai_no_capacity") {
		t.Fatalf("pick() error = %v, want zai_no_capacity propagated verbatim", err)
	}
}
