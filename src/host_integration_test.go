//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginhost"
	"gopkg.in/yaml.v3"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/abiclient"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

const fixtureKey = "test-only-host-integration-key"

// resourceStatusPath and managementStatusPath mirror the coordinator's own
// aggregated status route and the zai module's declared (relative) resource
// path -- see internal/coordinator/management.go and
// internal/providers/zai/management.go. The coordinator does no path
// rewriting on module-declared routes in this slice; only the resource
// prefix (the registered plugin ID) changed with the rename.
const (
	managementStatusPath  = "/v0/management/plugins/" + pluginID + "/status"
	resourceStatusPath    = "/status"
	resourceUsageDataPath = "/usage.json"
	usagePageMenu         = "Usage & Quota"
)

// ownedZaiAuthIDs computes the exact auth IDs the zai module will assign for
// the account(s) described by cpaConfigPath, by driving a throwaway
// in-process coordinator+zai module through the identical Reconfigure inputs
// the compiled plugin under test also receives. Account discovery is a pure
// function of the CPA config content, so this yields byte-identical auth IDs
// to whatever the separately-compiled .so computes, without this package
// (package main) needing to call any of zai's now-unexported internals.
func ownedZaiAuthIDs(t *testing.T, cpaConfigPath string) []string {
	t.Helper()
	c := coordinator.New(zai.NewModule())
	rawConfig := []byte("cpa-config-path: " + cpaConfigPath + "\nproviders:\n  zai:\n    default-plan: pro\n")
	if err := c.Reconfigure(rawConfig); err != nil {
		t.Fatalf("reference coordinator Reconfigure() error = %v", err)
	}
	ids := c.OwnedAuthIDs("zai")
	if len(ids) == 0 {
		t.Fatal("zai module reported no owned auth ids")
	}
	return ids
}

// TestManagementRouteEndToEnd pins the full management dispatch contract the
// review flagged: the route declared by management.register must resolve to
// the same routing key the host dispatches, and management.handle must answer
// with pluginapi.ManagementResponse — not a bare status object — or the host
// decodes an empty HTTP response.
//
// The SDK host does not expose ServeManagementHTTP, so this test loads the
// compiled c-shared library through internal/abiclient (the host loader's C
// ABI) and replays the host's wire behavior byte-for-byte: the declared route
// normalizes to the coordinator's own aggregated status path,
// ServeManagementHTTP forwards that exact path in ManagementRequest, and the
// response is decoded as pluginapi.ManagementResponse.
func TestManagementRouteEndToEnd(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}
	client, err := abiclient.Open(binary)
	if err != nil {
		t.Fatalf("open plugin: %v", err)
	}
	defer client.Close()

	// Host side of management.register: rpcManagementRegistrationResponse
	// decodes routes with untagged Method/Path fields.
	registerRaw, err := client.Call(pluginabi.MethodManagementRegister, []byte("{}"))
	if err != nil {
		t.Fatalf("management.register: %v", err)
	}
	registered := decodeEnvelopeResult[struct {
		Routes    []pluginapi.ManagementRoute `json:"routes"`
		Resources []pluginapi.ResourceRoute   `json:"resources"`
	}](t, registerRaw, pluginabi.MethodManagementRegister)
	// The coordinator's own aggregated status and usage routes, plus the zai
	// module's 4 declared routes (status/refresh/unblock/account-config) = 6.
	if len(registered.Routes) != 6 {
		t.Fatalf("management.register routes = %#v, want six (coordinator status + usage + zai's 4)", registered.Routes)
	}
	if len(registered.Resources) != 2 || registered.Resources[0].Path != resourceStatusPath || registered.Resources[0].Menu != usagePageMenu || registered.Resources[1].Path != resourceUsageDataPath || registered.Resources[1].Menu != "" {
		t.Fatalf("management.register resources = %#v, want the usage page menu and its data route", registered.Resources)
	}
	route := registered.Routes[0]
	if !strings.EqualFold(route.Method, http.MethodGet) || route.Path != managementStatusPath {
		t.Fatalf("status route = %#v, want GET %s", route, managementStatusPath)
	}

	// normalizeManagementRoute (identical in v7.2.150 and v7.2.151): a leading
	// /v0/management/ prefix is trimmed, then re-prefixed, yielding the
	// canonical key the host registers and dispatches.
	dispatchPath := normalizeManagementRouteForTest(t, route.Path)

	// Host side of management.handle: rpcManagementRequest embeds
	// ManagementRequest, whose Path is r.URL.Path — the full dispatch path.
	requestBody, err := json.Marshal(map[string]any{
		"method":           http.MethodGet,
		"path":             dispatchPath,
		"host_callback_id": "test-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	handleRaw, err := client.Call(pluginabi.MethodManagementHandle, requestBody)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	resp := decodeEnvelopeResult[pluginapi.ManagementResponse](t, handleRaw, pluginabi.MethodManagementHandle)

	// ServeManagementHTTP maps a zero status to 200 and writes Body verbatim.
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		t.Fatalf("management status = %d, want 200", status)
	}
	var body struct {
		Plugin  string `json:"plugin"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("management body is not the status JSON: %v (raw %q)", err, resp.Body)
	}
	if body.Plugin != pluginID || body.Status != "registered" {
		t.Fatalf("management body = %#v", body)
	}
}

func TestResourceRouteEndToEndAllowsCrossOriginManagementCenterFrame(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}
	client, err := abiclient.Open(binary)
	if err != nil {
		t.Fatalf("open plugin: %v", err)
	}
	defer client.Close()

	registerRaw, err := client.Call(pluginabi.MethodManagementRegister, []byte("{}"))
	if err != nil {
		t.Fatalf("management.register: %v", err)
	}
	registered := decodeEnvelopeResult[struct {
		Resources []pluginapi.ResourceRoute `json:"resources"`
	}](t, registerRaw, pluginabi.MethodManagementRegister)
	if len(registered.Resources) != 2 {
		t.Fatalf("management.register resources = %#v, want the page and its data route", registered.Resources)
	}
	resource := registered.Resources[0]
	if resource.Path != resourceStatusPath || resource.Menu != usagePageMenu {
		t.Fatalf("resource route = %#v", resource)
	}

	// The host prefixes a module's own relative resource path by the
	// *registered* plugin ID (the coordinator's), not the owning module's own
	// internal ID -- see internal/coordinator/management.go
	// resourcePluginBasePath.
	requestBody, err := json.Marshal(map[string]any{
		"method": http.MethodGet,
		"path":   "/v0/resource/plugins/" + pluginID + resource.Path,
		"headers": http.Header{
			"Origin": []string{"https://management.example.test"},
		},
		"host_callback_id": "test-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	handleRaw, err := client.Call(pluginabi.MethodManagementHandle, requestBody)
	if err != nil {
		t.Fatalf("management.handle resource: %v", err)
	}
	response := decodeEnvelopeResult[pluginapi.ManagementResponse](t, handleRaw, pluginabi.MethodManagementHandle)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "Quota Management") || !strings.Contains(string(response.Body), `id="sp-data"`) {
		t.Fatalf("resource response = %d %q", response.StatusCode, response.Body)
	}
	if csp := response.Headers.Get("Content-Security-Policy"); strings.Contains(csp, "frame-ancestors") {
		t.Fatalf("resource CSP %q blocks a cross-origin Management Center iframe", csp)
	}
}

// normalizeManagementRouteForTest reproduces the host's route normalization
// (internal/pluginhost management.go): trim, force a leading slash, strip a
// redundant /v0/management prefix (keeping the leading slash), strip trailing
// slashes, then re-prefix.
func normalizeManagementRouteForTest(t *testing.T, declared string) string {
	t.Helper()
	const basePath = "/v0/management"
	path := strings.TrimSpace(declared)
	if path == "" {
		t.Fatal("declared route path is empty")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(path, basePath+"/") {
		path = strings.TrimPrefix(path, basePath)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		t.Fatal("declared route path normalizes to the management base path")
	}
	full := basePath + path
	if !strings.HasPrefix(full, basePath+"/") {
		t.Fatalf("normalized path %q is not under %s", full, basePath)
	}
	return full
}

// decodeEnvelopeResult unwraps a plugin envelope the way the host's
// callPlugin does (internal/pluginhost rpc_client.go): a false envelope is
// an error; a true envelope decodes Result into the target type.
func decodeEnvelopeResult[T any](t *testing.T, raw []byte, method string) T {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", method, err)
	}
	if !envelope.OK {
		t.Fatalf("plugin call %s failed: %+v", method, envelope.Error)
	}
	var out T
	if err := json.Unmarshal(envelope.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", method, err)
	}
	return out
}

// TestHostRegistersPlugin loads the compiled plugin through the pinned
// CLIProxyAPI plugin host (the same loader the server uses) and asserts
// the plugin survives registration: the host rejects plugins whose
// metadata is incomplete or that advertise no capability.
func TestHostRegistersPlugin(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}

	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins", "linux", "amd64")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(pluginDir, pluginID+"-v0.0.0-test.so")
	if err := copyFile(versioned, binary); err != nil {
		t.Fatal(err)
	}

	authDir := filepath.Join(root, "auth")
	cpaConfigPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, cpaConfigPath, authDir, fixtureKey)
	enabled := true
	configNode := pluginConfigNode(t, cpaConfigPath)
	host := pluginhost.New()
	host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
		Enabled: true,
		Dir:     filepath.Join(root, "plugins"),
		AuthDir: authDir,
		Configs: map[string]pluginhost.PluginInstanceConfig{
			pluginID: {Enabled: &enabled, Priority: 1000, Raw: configNode},
		},
	})
	defer host.ShutdownAll()

	registered := host.RegisteredPlugins()
	found := false
	for _, plugin := range registered {
		if plugin.ID != pluginID {
			continue
		}
		found = true
		if plugin.Metadata.Name != pluginID {
			t.Fatalf("registered name = %q, want %q", plugin.Metadata.Name, pluginID)
		}
		if strings.TrimSpace(plugin.Metadata.Version) == "" {
			t.Fatal("registered version is empty")
		}
	}
	if !found {
		t.Fatalf("plugin %s absent from host registration snapshot; plugins = %#v", pluginID, registered)
	}

	// Healthy managed traffic explicitly delegates to the native round-robin
	// scheduler; returning unhandled would permit an accidental fallback path.
	authIDs := ownedZaiAuthIDs(t, cpaConfigPath)
	resp, handled, errPick := host.PickAuth(context.Background(), pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: authIDs[0]}}})
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v", errPick)
	}
	if !handled || !resp.Handled || resp.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("PickAuth() = %#v, handled %v, want explicit round-robin delegation", resp, handled)
	}

	if !host.HasScheduler() {
		t.Fatal("HasScheduler() = false, want the advertised scheduler capability to register")
	}
}

// buildTestPlugin compiles the plugin package into a c-shared library the
// same way the Makefile release build does. The module root is the
// repository root; the src package is imported by path from there.
func buildTestPlugin(t *testing.T) (string, error) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("go toolchain required: %w", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	repoRoot := filepath.Dir(dir)
	out := filepath.Join(t.TempDir(), pluginID+".so")
	cmd := exec.Command(goTool, "build", "-buildvcs=false", "-buildmode=c-shared",
		"-ldflags", "-X main.pluginVersion=0.0.0-test", "-o", out, "./src")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, output)
	}
	if _, err := os.Stat(out); err != nil {
		return "", err
	}
	return out, nil
}

func TestHostPropagatesAllImpairedSchedulerError(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}
	client, err := abiclient.Open(binary)
	if err != nil {
		t.Fatalf("open plugin: %v", err)
	}
	defer client.Close()

	root := t.TempDir()
	cpaConfigPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, cpaConfigPath, filepath.Join(root, "auth"), fixtureKey)
	registerRequest, err := json.Marshal(map[string]any{"config_yaml": []byte("cpa-config-path: " + cpaConfigPath + "\nproviders:\n  zai:\n    default-plan: pro\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Call(pluginabi.MethodPluginRegister, registerRequest); err != nil {
		t.Fatal(err)
	}
	authIDs := ownedZaiAuthIDs(t, cpaConfigPath)
	usage, err := json.Marshal(pluginapi.UsageRecord{AuthID: authIDs[0], Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Call(pluginabi.MethodUsageHandle, usage); err != nil {
		t.Fatal(err)
	}
	pick, err := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: authIDs[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, callErr := client.Call(pluginabi.MethodSchedulerPick, pick)
	if callErr == nil {
		t.Fatal("all-impaired scheduler call succeeded")
	}
	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Code != "zai_no_capacity" || result.Error.Retryable {
		t.Fatalf("scheduler error envelope = %#v", result.Error)
	}
}

func TestInvalidReconfigureWithdrawsHostRegistration(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}

	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins", "linux", "amd64")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(pluginDir, pluginID+"-v0.0.0-test.so")
	if err := copyFile(versioned, binary); err != nil {
		t.Fatal(err)
	}

	authDir := filepath.Join(root, "auth")
	cpaConfigPath := filepath.Join(root, "config.yaml")
	writeSrcCPAConfigFixture(t, cpaConfigPath, authDir, fixtureKey)
	enabled := true
	configNode := pluginConfigNode(t, cpaConfigPath)
	host := pluginhost.New()
	host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
		Enabled: true,
		Dir:     filepath.Join(root, "plugins"),
		AuthDir: authDir,
		Configs: map[string]pluginhost.PluginInstanceConfig{
			pluginID: {Enabled: &enabled, Priority: 1000, Raw: configNode},
		},
	})
	defer host.ShutdownAll()
	if !host.HasScheduler() {
		t.Fatal("initial exclusive configuration did not register scheduler")
	}

	raw, err := os.ReadFile(cpaConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "      priority: 1000\n", "      priority: 1000\n    competitor:\n      enabled: true\n      priority: 2000\n", 1))
	if err = os.WriteFile(cpaConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
		Enabled: true,
		Dir:     filepath.Join(root, "plugins"),
		AuthDir: authDir,
		Configs: map[string]pluginhost.PluginInstanceConfig{
			pluginID: {Enabled: &enabled, Priority: 1000, Raw: configNode},
		},
	})

	if host.HasScheduler() {
		t.Fatal("invalid live reconfigure retained scheduler capability")
	}
	if plugins := host.RegisteredPlugins(); len(plugins) != 0 {
		t.Fatalf("registered plugins = %#v, want none after rejected reconfigure", plugins)
	}
}

// pluginConfigNode builds the plugin's own config_yaml node: only
// cpa-config-path and providers.*. The real host always re-injects its own
// enabled/priority scalars into this node before the plugin sees it
// (pluginhost's normalizedConfigNode/ensureMappingScalar), so omitting them
// here exercises the same shape the coordinator must also accept when they
// are present.
func pluginConfigNode(t *testing.T, cpaConfigPath string) yaml.Node {
	t.Helper()
	var configNode yaml.Node
	if err := yaml.Unmarshal([]byte("cpa-config-path: "+cpaConfigPath+"\nproviders:\n  zai:\n    default-plan: pro\n"), &configNode); err != nil {
		t.Fatal(err)
	}
	return *configNode.Content[0]
}

func copyFile(dst, src string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o755)
}
