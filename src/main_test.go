package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

// writeSrcCPAConfigFixture writes a host CLIProxyAPI config in the
// coordinator's own nested shape, mirroring
// internal/coordinator/zai_integration_test.go's writeCoordinatorCPAConfigFixture,
// so ABI-layer tests here can drive a real coordinator+zai module without
// touching any of zai's now-unexported internals.
func writeSrcCPAConfigFixture(t *testing.T, path, authDir, key string) {
	t.Helper()
	raw := "auth-dir: " + authDir + "\n" +
		"plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    " + pluginID + ":\n" +
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

func TestPluginRegistration(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Name != pluginID {
		t.Fatalf("plugin name = %q, want %q", registration.Metadata.Name, pluginID)
	}
	// The pinned host rejects plugins that advertise no capability
	// (internal/pluginhost host.go validPlugin). Every advertised
	// capability must also have a handler in pluginCall.
	if registration.Capabilities != (capabilities{Scheduler: true, SchedulerAcrossPriorities: true, UsagePlugin: true, ManagementAPI: true}) {
		t.Fatalf("capabilities = %#v, want scheduler (across priorities), usage_plugin, and management_api", registration.Capabilities)
	}
}

func TestDefaultRuntimeHostsBothProviderModules(t *testing.T) {
	previous := runtimeState
	defer func() { runtimeState = previous }()

	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, configPath, authDir, "test-only-two-provider-key")

	c := coordinator.New(zai.NewModule(), opencodego.NewModule())
	rawConfig := []byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n  opencode-go:\n    accounts:\n      - name: go-a\n        auth-ids: [go-a-1]\n")
	if err := c.Reconfigure(rawConfig); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	if got := c.OwnedAuthIDs("opencode-go"); len(got) != 1 || got[0] != "go-a-1" {
		t.Fatalf("OpenCode Go owned auth IDs = %#v", got)
	}
}

func TestSchedulerPickDeclinesUnmanagedTraffic(t *testing.T) {
	previous := runtimeState
	defer func() { runtimeState = previous }()

	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, configPath, authDir, "test-only-scheduler-key")

	c := coordinator.New(zai.NewModule())
	rawConfig := []byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n")
	if err := c.Reconfigure(rawConfig); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	runtimeState = c

	request, err := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other-auth", Provider: "gemini"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := schedulerPick(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var pick pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(envelope.Result, &pick); err != nil {
		t.Fatal(err)
	}
	if pick.Handled || pick.AuthID != "" || pick.DelegateBuiltin != "" {
		t.Fatalf("unmanaged traffic must be declined: %#v", pick)
	}
}

// TestSchedulerPickRateLimitedEnvelopeKeepsHTTP429 pins the wire shape CPA
// turns into the client status: an envelope error with http_status set becomes
// that status, and one without becomes a 500. A request-rate throttle on every
// managed account must therefore reach the host as a retryable 429, not as the
// zai_no_capacity 500 the model router quarantines the whole lane on.
func TestSchedulerPickRateLimitedEnvelopeKeepsHTTP429(t *testing.T) {
	previous := runtimeState
	defer func() { runtimeState = previous }()

	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, configPath, authDir, "test-only-scheduler-key")

	c := coordinator.New(zai.NewModule())
	rawConfig := []byte("cpa-config-path: " + configPath + "\nproviders:\n  zai:\n    default-plan: pro\n")
	if err := c.Reconfigure(rawConfig); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	runtimeState = c

	authIDs := c.OwnedAuthIDs("zai")
	if len(authIDs) == 0 {
		t.Fatal("zai module reported no owned auth ids")
	}
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		if err := c.HandleUsage(pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`}}); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: authID, Attributes: map[string]string{"base_url": "https://api.z.ai/api/anthropic"}})
	}
	request, err := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedulerPick(request); err == nil {
		t.Fatal("schedulerPick() succeeded with every managed account throttled")
	} else {
		var wire envelope
		if err := json.Unmarshal(errorEnvelopeFor(err), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.OK || wire.Error == nil || wire.Error.Code != "zai_rate_limited" || !wire.Error.Retryable || wire.Error.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("envelope = %#v, want retryable zai_rate_limited with http_status 429", wire.Error)
		}
	}
}

// managementRoutesWire mirrors the JSON shape of the coordinator's
// (unexported) managementRoutesBody, since only its JSON tags, not the type
// itself, are part of the ABI contract this package relies on.
type managementRoutesWire struct {
	Routes []struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description,omitempty"`
	} `json:"routes"`
	Resources []struct {
		Path        string `json:"path"`
		Menu        string `json:"menu"`
		Description string `json:"description,omitempty"`
	} `json:"resources,omitempty"`
}

func TestManagementRegistrationRoutes(t *testing.T) {
	raw, err := okEnvelope(runtimeState.ManagementRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var routes managementRoutesWire
	if err := json.Unmarshal(envelope.Result, &routes); err != nil {
		t.Fatal(err)
	}
	// The coordinator's own aggregated status route, plus the zai module's 4
	// declared routes (status/refresh/unblock/account-config) = 5 total.
	if len(routes.Routes) != 5 {
		t.Fatalf("routes = %#v, want 5 routes (coordinator status + zai's 4)", routes.Routes)
	}
	wantStatusPath := "/v0/management/plugins/" + pluginID + "/status"
	if route := routes.Routes[0]; route.Method != "GET" || route.Path != wantStatusPath {
		t.Fatalf("route = %#v, want GET %s", route, wantStatusPath)
	}
}

func TestManagementHandleStatus(t *testing.T) {
	// The host forwards the full request path (internal/pluginhost
	// management.go ServeManagementHTTP passes r.URL.Path verbatim).
	statusPath := "/v0/management/plugins/" + pluginID + "/status"
	request, err := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   statusPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := managementHandle(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	// The host decodes the result as pluginapi.ManagementResponse, whose
	// fields carry no JSON tags and whose Body is base64-encoded bytes.
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var status struct {
		Plugin  string `json:"plugin"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(resp.Body, &status); err != nil {
		t.Fatal(err)
	}
	if status.Plugin != pluginID || status.Status != "registered" || status.Version != pluginVersion {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagementHandleUnknownPath(t *testing.T) {
	request, err := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/other"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := managementHandle(request)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(wrapped.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestUsageHandleAcknowledges(t *testing.T) {
	record, err := json.Marshal(pluginapi.UsageRecord{Provider: "zai", Model: "glm-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := usageHandle(record)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("usageHandle() envelope = %#v, want ok", envelope)
	}
}

func TestEnvelopes(t *testing.T) {
	raw, err := okEnvelope(map[string]string{"status": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	var response envelope
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Result) == 0 || response.Error != nil {
		t.Fatalf("unexpected success envelope: %#v", response)
	}

	if err := json.Unmarshal(errorEnvelope("test", "failure"), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "test" {
		t.Fatalf("unexpected error envelope: %#v", response)
	}
}
