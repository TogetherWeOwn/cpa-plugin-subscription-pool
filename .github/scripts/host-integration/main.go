// host-integration verifies the built plugin through a real CLIProxyAPI image binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/abiclient"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const (
	pluginID       = "subscription-pool"
	statusPath     = "/v0/management/plugins/subscription-pool/status"
	pluginListPath = "/v0/management/plugins"
	smokePlanKey   = "release-integration-plan-key-000001"
	managementKey  = "release-integration-management-key"
	clientKey      = "release-integration-client-key"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("host-integration", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	hostBinary := flags.String("host-binary", "", "CLIProxyAPI binary extracted from the tested image")
	plugin := flags.String("plugin", "", "built plugin shared library")
	image := flags.String("image", "host image", "human-readable image name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *hostBinary == "" || *plugin == "" {
		return errors.New("host-binary and plugin are required")
	}
	if err := verifyCapabilities(*plugin); err != nil {
		return err
	}
	return verifyHostHTTP(*hostBinary, *plugin, *image)
}

func verifyCapabilities(plugin string) error {
	client, err := abiclient.Open(plugin)
	if err != nil {
		return fmt.Errorf("load plugin registration: %w", err)
	}
	defer client.Close()
	root, err := os.MkdirTemp("", "zai-capability-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	cpaConfigPath := filepath.Join(root, "config.yaml")
	cpaConfig := fmt.Sprintf(`auth-dir: %q
claude-api-key:
  - api-key: %q
    base-url: https://api.z.ai/api/anthropic
    prefix: zai
    headers:
      X-CPA-Smoke-Lane: anthropic
openai-compatibility:
  - name: zai-coding-plan
    prefix: zai-openai
    base-url: https://api.z.ai/api/coding/paas/v4
    api-key-entries:
      - api-key: %q
plugins:
  enabled: true
  dir: plugins
  configs:
    subscription-pool:
      enabled: true
      priority: 1000
`, filepath.Join(root, "auth"), smokePlanKey, smokePlanKey)
	if err := os.WriteFile(cpaConfigPath, []byte(cpaConfig), 0o600); err != nil {
		return err
	}
	pluginConfig := fmt.Sprintf("cpa-config-path: %s\nproviders:\n  zai:\n    default-plan: pro\n    accounts:\n      - key-suffix: 000001\n        name: release-capability\n        plan: pro\n", cpaConfigPath)
	request, err := json.Marshal(map[string]any{"config_yaml": []byte(pluginConfig)})
	if err != nil {
		return err
	}
	raw, callErr := client.Call(pluginabi.MethodPluginRegister, request)
	if callErr != nil && len(raw) == 0 {
		return fmt.Errorf("plugin registration: %w", callErr)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode registration envelope: %w (raw %q, call error %v)", err, raw, callErr)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("registration returned no result (raw %q, call error %v)", raw, callErr)
	}
	var registration struct {
		Capabilities struct {
			Scheduler     bool `json:"scheduler"`
			UsagePlugin   bool `json:"usage_plugin"`
			ManagementAPI bool `json:"management_api"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		return fmt.Errorf("decode registration result: %w", err)
	}
	caps := registration.Capabilities
	if !envelope.OK || !caps.Scheduler || !caps.UsagePlugin || !caps.ManagementAPI {
		return fmt.Errorf("required capabilities not advertised: scheduler=%t usage_plugin=%t management_api=%t", caps.Scheduler, caps.UsagePlugin, caps.ManagementAPI)
	}
	return nil
}

func verifyHostHTTP(hostBinary, plugin, image string) error {
	root, err := os.MkdirTemp("", "zai-host-integration-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	stub, caPath, err := startUpstreamStub(root)
	if err != nil {
		return err
	}
	defer stub.server.Shutdown(context.Background())

	pluginDir := filepath.Join(root, "plugins", "linux", "amd64")
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return err
	}
	pluginDst := filepath.Join(pluginDir, pluginID+"-v0.2.0.so")
	if err := copyFile(pluginDst, plugin, 0o755); err != nil {
		return err
	}

	port, err := availablePort()
	if err != nil {
		return err
	}
	configPath := filepath.Join(root, "config.yaml")
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
auth-dir: %q
api-keys: [%q]
force-model-prefix: true
claude-api-key:
  - api-key: %q
    base-url: https://api.z.ai/api/anthropic
    prefix: zai
    headers:
      X-CPA-Smoke-Lane: anthropic
    models:
      - name: smoke-upstream-anthropic
        alias: smoke-model
openai-compatibility:
  - name: zai-coding-plan
    prefix: zai-openai
    base-url: https://api.z.ai/api/coding/paas/v4
    headers:
      X-CPA-Smoke-Lane: openai
    api-key-entries:
      - api-key: %q
    models:
      - name: smoke-upstream-openai
        alias: smoke-model
  - name: cloudflare-workers-ai
    base-url: https://api.cloudflare.com/client/v4/accounts/0123456789abcdef0123456789abcdef/ai/v1
    api-key-entries:
      - api-key: test-only-cloudflare-integration-key
    models:
      - name: "@cf/qwen/qwen3-30b-a3b-fp8"
        alias: cloudflare-workers-ai/qwen3-30b-a3b-fp8
        max-context-length: 32768
        thinking: {}
  - name: nvidia-nim
    base-url: https://integrate.api.nvidia.com/v1
    api-key-entries:
      - api-key: test-only-nvidia-integration-key
    models:
      - name: z-ai/glm-5.3-flash
        alias: nvidia-nim/glm-5.3-flash
plugins:
  enabled: true
  dir: %q
  configs:
    subscription-pool:
      enabled: true
      priority: 1000
      cpa-config-path: %q
      providers:
        nvidia-nim:
          stayWithinFree: true
          free-tier-confirmed: true
          requests-per-minute: 10
          free-request-limit: 2
          free-models: [z-ai/glm-5.3-flash]
        cloudflare-workers-ai:
          stayWithinIncluded: true
        zai:
          quota-refresh-interval: 1m
          authoritative-max-age: 3m
          default-plan: pro
          accounts:
            - key-suffix: 000001
              name: release-integration
              plan: pro
`, port, managementKey, authDir, clientKey, smokePlanKey, smokePlanKey, filepath.Join(root, "plugins"), configPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return err
	}

	logFile, err := os.OpenFile(filepath.Join(root, "host.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	var command *exec.Cmd
	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	restart := func() error {
		stopProcess(command)
		command = exec.Command(hostBinary, "-config", configPath)
		command.Dir = root
		command.Stdout = logFile
		command.Stderr = logFile
		command.Env = cleanProxyEnvironment(append(os.Environ(), "SSL_CERT_FILE="+caPath))
		if err := command.Start(); err != nil {
			return fmt.Errorf("start tested host binary: %w", err)
		}
		return waitForRegistered(client, baseURL)
	}
	defer func() { stopProcess(command) }()
	if err := restart(); err != nil {
		return withHostLog(root, err)
	}
	if err := verifyStatusAuthentication(client, baseURL); err != nil {
		return withHostLog(root, err)
	}
	if err := inferenceRoundTrip(client, baseURL, "/v1/messages", "zai/smoke-model", "anthropic smoke ok", true); err != nil {
		return withHostLog(root, err)
	}
	if err := inferenceRoundTrip(client, baseURL, "/v1/chat/completions", "zai-openai/smoke-model", "openai smoke ok", false); err != nil {
		return withHostLog(root, err)
	}
	if err := verifyNvidiaHTTP(client, baseURL, configPath, config, restart); err != nil {
		return withHostLog(root, err)
	}
	if err := verifyCloudflareHTTP(client, baseURL, root, configPath, config, restart); err != nil {
		return withHostLog(root, err)
	}
	if err := stub.assertRequests(); err != nil {
		return withHostLog(root, err)
	}
	if err := rejectSchedulerErrors(root); err != nil {
		return err
	}
	fmt.Printf("%s: registered with scheduler, usage_plugin, management_api; real scheduler picks served zai/* and zai-openai/* with HTTP 200\n", image)
	return nil
}

func waitForRegistered(client *http.Client, baseURL string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		registered, err := pluginRegistered(client, baseURL)
		if err == nil && registered {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("tested host image did not register and enable the plugin within 30 seconds")
}

func pluginRegistered(client *http.Client, baseURL string) (bool, error) {
	response, err := requestJSON(client, http.MethodGet, baseURL+pluginListPath, managementKey, nil)
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("plugin list status = %d", response.StatusCode)
	}
	var body struct {
		PluginsEnabled bool `json:"plugins_enabled"`
		Plugins        []struct {
			ID               string `json:"id"`
			Configured       bool   `json:"configured"`
			Registered       bool   `json:"registered"`
			Enabled          bool   `json:"enabled"`
			EffectiveEnabled bool   `json:"effective_enabled"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return false, err
	}
	for _, item := range body.Plugins {
		if item.ID == pluginID {
			return body.PluginsEnabled && item.Configured && item.Registered && item.Enabled && item.EffectiveEnabled, nil
		}
	}
	return false, nil
}

func verifyStatusAuthentication(client *http.Client, baseURL string) error {
	unauthorized, err := requestJSON(client, http.MethodGet, baseURL+statusPath, "", nil)
	if err != nil {
		return err
	}
	if unauthorized.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated status code = %d, want 401", unauthorized.StatusCode)
	}
	authorized, err := requestJSON(client, http.MethodGet, baseURL+statusPath, managementKey, nil)
	if err != nil {
		return err
	}
	if authorized.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticated status code = %d, want 200", authorized.StatusCode)
	}
	var body struct {
		Plugin    string                     `json:"plugin"`
		Status    string                     `json:"status"`
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(authorized.Body, &body); err != nil {
		return fmt.Errorf("decode authenticated status: %w", err)
	}
	var zaiStatus struct {
		Accounts []struct {
			KeySuffix string `json:"key_suffix"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body.Providers["zai"], &zaiStatus); err != nil {
		return fmt.Errorf("decode zai provider status: %w", err)
	}
	if body.Plugin != pluginID || body.Status != "registered" || len(zaiStatus.Accounts) != 1 || zaiStatus.Accounts[0].KeySuffix != "redacted" {
		return fmt.Errorf("authenticated status is not registered and redacted: plugin=%q status=%q accounts=%d suffix=%q", body.Plugin, body.Status, len(zaiStatus.Accounts), firstSuffix(zaiStatus.Accounts))
	}
	return nil
}

func inferenceRoundTrip(client *http.Client, baseURL, path, model, marker string, anthropic bool) error {
	requestBody := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": "compatibility smoke"}},
	}
	if anthropic {
		requestBody["max_tokens"] = 16
	} else {
		requestBody["max_tokens"] = 16
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if anthropic {
		request.Header.Set("X-Api-Key", clientKey)
		request.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+clientKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), marker) {
		return fmt.Errorf("%s inference status = %d, marker %q absent; body %q", model, response.StatusCode, marker, body)
	}
	return nil
}

type statusResponse struct {
	StatusCode int
	Body       []byte
}

func requestJSON(client *http.Client, method, url, key string, body io.Reader) (statusResponse, error) {
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return statusResponse{}, err
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return statusResponse{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return statusResponse{}, err
	}
	return statusResponse{StatusCode: response.StatusCode, Body: raw}, nil
}

func availablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve host integration port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func firstSuffix(accounts []struct {
	KeySuffix string `json:"key_suffix"`
}) string {
	if len(accounts) == 0 {
		return ""
	}
	return accounts[0].KeySuffix
}

func copyFile(dst, src string, mode os.FileMode) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, mode)
}

func stopProcess(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	// Let the host drain callbacks and save its usage ledger on a normal
	// fixture restart. Billing reservations are already durable before dispatch.
	_ = command.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-done
	}
}

func rejectSchedulerErrors(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "host.log"))
	if err != nil {
		return err
	}
	log := string(raw)
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "cloudflare_no_capacity: included_exhausted") || strings.Contains(line, "cloudflare_no_capacity: recent_payg_usage") || strings.Contains(line, "nvidia_nim_no_capacity: free_or_rate_boundary") {
			continue
		}
		for _, marker := range []string{"zai_unmanaged_candidate", "scheduler rejected auth pick"} {
			if strings.Contains(line, marker) {
				return fmt.Errorf("host log contains unexpected scheduler failure %q\nhost log:\n%s", marker, log)
			}
		}
	}
	return nil
}

func cleanProxyEnvironment(environment []string) []string {
	out := environment[:0]
	for _, item := range environment {
		key := strings.ToUpper(strings.SplitN(item, "=", 2)[0])
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		default:
			out = append(out, item)
		}
	}
	return append(out, "NO_PROXY=*", "no_proxy=*")
}

func withHostLog(root string, cause error) error {
	raw, err := os.ReadFile(filepath.Join(root, "host.log"))
	if err != nil {
		return cause
	}
	return fmt.Errorf("%w\nhost log:\n%s", cause, raw)
}
