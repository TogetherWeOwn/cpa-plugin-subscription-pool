package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/synthetic"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

// writeSyntheticHostConfig mirrors a production host after the Z.ai plan was
// cancelled: no Z.ai credential, OpenCode Go credentials still present, and
// the synthetic.new paid + infinite providers sharing one key. Host-only
// fields the plugin's SDK does not know (priority, thinking, modalities)
// must not break parsing.
func writeSyntheticHostConfig(t *testing.T, path, authDir, key string) {
	t.Helper()
	raw := "auth-dir: " + authDir + "\n" +
		"plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    subscription-pool:\n" +
		"      enabled: true\n" +
		"      priority: 1000\n" +
		"    zai-coding-plan:\n" +
		"      enabled: false\n" +
		"openai-compatibility:\n" +
		"  - name: opencode-go\n" +
		"    base-url: http://relay:8399/zen/go/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: test-only-go-key\n" +
		"    models:\n" +
		"      - name: deepseek-v4-flash\n" +
		"        alias: opencode-go/deepseek-v4-flash\n" +
		"  - name: synthetic-infinite\n" +
		"    priority: -1\n" +
		"    base-url: https://api.synthetic.new/openai/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: " + key + "\n" +
		"    models:\n" +
		"      - name: infinite:small:vision\n" +
		"        alias: synthetic/deepseek-v4.1-flash\n" +
		"        max-context-length: 524288\n" +
		"        input-modalities: [text, image]\n" +
		"        thinking: {levels: [none, low, high, xhigh, max]}\n" +
		"  - name: synthetic\n" +
		"    base-url: https://api.synthetic.new/openai/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: " + key + "\n" +
		"    models:\n" +
		"      - name: hf:deepseek-ai/DeepSeek-V4.1-Flash\n" +
		"        alias: synthetic/deepseek-v4.1-flash\n" +
		"        force-mapping: true\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorRegistersWithoutZaiAndRoutesSynthetic(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeSyntheticHostConfig(t, configPath, authDir, "<API KEY PLACEHOLDER>")

	c := New(zai.NewModule(), opencodego.NewModule(), synthetic.NewModule())
	defer func() { _ = c.Shutdown() }()
	rawConfig := []byte("cpa-config-path: " + configPath + "\n" +
		"providers:\n" +
		"  zai:\n" +
		"    default-plan: pro\n" +
		"  opencode-go:\n" +
		"    enabled: false\n" +
		"  synthetic:\n" +
		"    threshold-percent: 95\n" +
		"    quota-refresh-interval: \"0\"\n")
	if err := c.Reconfigure(rawConfig); err != nil {
		t.Fatalf("Reconfigure() error = %v (missing Z.ai credentials must not unregister the plugin)", err)
	}
	if got := c.OwnedAuthIDs("zai"); len(got) != 0 {
		t.Fatalf("zai owns %v without credentials", got)
	}
	owned := c.OwnedAuthIDs("synthetic")
	if len(owned) != 2 {
		t.Fatalf("synthetic owns %v, want paid + infinite", owned)
	}

	// OpenCode Go is disabled: its traffic goes back to the host scheduler.
	resp, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "openai-compatibility:opencode-go:abc", Provider: "openai-compatible-opencode-go", Attributes: map[string]string{"compat_name": "opencode-go"}},
	}})
	if err != nil || resp.Handled {
		t.Fatalf("OpenCode Go pick = %+v, %v; want Handled:false", resp, err)
	}

	paid := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:synthetic:08e39ede6b45", Provider: "openai-compatible-synthetic", Attributes: map[string]string{"compat_name": "synthetic"}}
	infinite := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:synthetic-infinite:x", Provider: "openai-compatible-synthetic-infinite", Priority: -1, Attributes: map[string]string{"compat_name": "synthetic-infinite"}}
	resp, err = c.Pick(pluginapi.SchedulerPickRequest{Stream: true, Candidates: []pluginapi.SchedulerAuthCandidate{paid, infinite}})
	if err != nil || !resp.Handled || resp.AuthID != paid.ID {
		t.Fatalf("synthetic pick = %+v, %v; want paid %s", resp, err, paid.ID)
	}

	// A quota 429 learned through the coordinator's usage dispatch moves the
	// shared alias to infinite mode.
	header := http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2099-01-01T00:00:00Z","nextRegenPercent":2}}`}}
	if err := c.HandleUsage(pluginapi.UsageRecord{AuthID: paid.ID, Provider: "openai-compatible-synthetic", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":"limit"}`}, ResponseHeaders: header}); err != nil {
		t.Fatal(err)
	}
	resp, err = c.Pick(pluginapi.SchedulerPickRequest{Stream: true, Candidates: []pluginapi.SchedulerAuthCandidate{paid, infinite}})
	if err != nil || resp.AuthID != infinite.ID {
		t.Fatalf("pick after quota 429 = %+v, %v; want infinite", resp, err)
	}

	status := c.managementStatus()
	if status.ValidationError != "" || status.Status != "registered" {
		t.Fatalf("status = %+v", status)
	}
	var syn map[string]any
	if err := json.Unmarshal(status.Providers["synthetic"], &syn); err != nil {
		t.Fatal(err)
	}
	if syn["status"] != "registered" || syn["credential_bound"] != false {
		t.Fatalf("synthetic status = %v", syn)
	}
	page := string(c.resourceStatusResponse().Body)
	if !strings.Contains(page, "synthetic.new paid") || !strings.Contains(page, "synthetic.new infinite") || strings.Contains(page, "PLACEHOLDER") {
		t.Fatalf("dashboard missing synthetic lanes or leaked the key field")
	}
}

func TestCoordinatorRegistersWithNoProvidersAtAll(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	raw := "auth-dir: " + authDir + "\nplugins:\n  enabled: true\n  configs:\n    subscription-pool:\n      enabled: true\n      priority: 1000\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(zai.NewModule(), opencodego.NewModule(), synthetic.NewModule())
	defer func() { _ = c.Shutdown() }()
	if err := c.Reconfigure([]byte("cpa-config-path: " + configPath + "\n")); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	resp, err := c.Pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "claude:x", Provider: "claude"}}})
	if err != nil || resp.Handled {
		t.Fatalf("pick = %+v, %v", resp, err)
	}
	_ = context.Background()
}
