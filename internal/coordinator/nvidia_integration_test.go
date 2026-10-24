package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/nvidianim"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNvidiaAliasGuardUsageAndRedactedPage(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	host := `auth-dir: ` + filepath.Join(root, "auth") + `
plugins:
  enabled: true
  configs:
    subscription-pool: {enabled: true, priority: 1000}
openai-compatibility:
  - name: nvidia-nim
    base-url: https://integrate.api.nvidia.com/v1
    api-key-entries:
      - api-key: test-only-nvidia-key
    models:
      - name: moonshotai/kimi-k3
        alias: nvidia-nim/kimi-k3
`
	if err := os.WriteFile(configPath, []byte(host), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(nvidianim.NewModule())
	defer func() {
		if err := c.Shutdown(); err != nil {
			t.Error(err)
		}
	}()
	cfg := []byte("cpa-config-path: " + configPath + "\nproviders:\n  nvidia-nim:\n    free-tier-confirmed: true\n    requests-per-minute: 2\n    free-request-limit: 1\n    free-models: [moonshotai/kimi-k3]\n")
	if err := c.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	ids := c.OwnedAuthIDs("nvidia-nim")
	if len(ids) != 1 {
		t.Fatalf("owned = %v", ids)
	}
	r := pluginapi.SchedulerPickRequest{Model: "nvidia-nim/kimi-k3", Stream: true, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: ids[0], Provider: "openai-compatible-nvidia-nim"}}}
	p, err := c.Pick(r)
	if err != nil || !p.Handled {
		t.Fatalf("alias not projected: %+v %v", p, err)
	}
	if err := c.HandleUsage(pluginapi.UsageRecord{AuthID: ids[0], Provider: "openai-compatible-nvidia-nim", Model: "nvidia-nim/kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pick(r); err == nil {
		t.Fatal("local ceiling bypassed")
	}
	resp := c.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: resourcePluginBasePath + resourceUsageDataPath})
	if resp.StatusCode != 200 {
		t.Fatalf("usage response %d", resp.StatusCode)
	}
	var data usagePageData
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Quota) != 1 || data.Quota[0].ProviderLabel != "NVIDIA NIM" || data.Quota[0].Plan != "Free-only guard on (prototyping)" || data.Quota[0].Health != "exhausted" {
		t.Fatalf("quota cards = %+v", data.Quota)
	}
	if len(data.Quota[0].Windows) != 2 || data.Quota[0].Windows[0].Authoritative || data.Quota[0].Windows[1].ResetsAt != nil {
		t.Fatalf("invented allowance: %+v", data.Quota[0])
	}
	for _, secret := range []string{"test-only-nvidia-key", ids[0], "fingerprint"} {
		if strings.Contains(string(resp.Body), secret) {
			t.Fatalf("public data leaked %q", secret)
		}
	}
	if len(data.Usage.Totals) != 1 || data.Usage.Totals[0].Provider != "nvidia-nim" {
		t.Fatalf("usage = %+v", data.Usage.Totals)
	}
}

func TestNvidiaPaidCardDoesNotShowDisabledFreeCeiling(t *testing.T) {
	cards := nvidiaQuotaCards(json.RawMessage(`{"status":"registered","stay_within_free":false,"free_request_limit":10,"requests_per_minute":2,"rate_used":1,"accounts":[{"name":"nvidia-nim#1","health":"healthy"}]}`))
	if len(cards) != 1 || len(cards[0].Windows) != 1 {
		t.Fatalf("disabled free ceiling shown as active: %+v", cards)
	}
	if cards[0].Windows[0].Mode != "rolling" {
		t.Fatal("rolling RPM was presented as a periodically resetting credit bucket")
	}
}

func TestNvidiaPageHasNoInventedMonthlyCredit(t *testing.T) {
	cards := nvidiaQuotaCards(json.RawMessage(`{"status":"registered","stay_within_free":false,"accounts":[{"name":"nvidia-nim#1","health":"healthy"}]}`))
	if len(cards) != 1 || cards[0].Plan != "Paid use allowed (free guard off)" {
		t.Fatalf("cards = %+v", cards)
	}
	for _, w := range cards[0].Windows {
		if w.Known || w.ID == "monthly" {
			t.Fatalf("invented balance: %+v", w)
		}
	}
}
