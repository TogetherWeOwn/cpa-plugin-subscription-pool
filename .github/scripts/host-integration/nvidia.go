package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const nvSmokeKey = "test-only-nvidia-integration-key"
const nvAlias = "nvidia-nim/glm-5.3-flash"

// Only the isolated TLS stub receives these dummy credentials. A real key is
// neither required nor accepted by this smoke: production trial inference is
// not a substitute for verifying NVIDIA's account/model terms.
func verifyNvidiaHTTP(client *http.Client, baseURL, configPath, originalConfig string, restart func() error) error {
	if err := inferenceRoundTrip(client, baseURL, "/v1/chat/completions", nvAlias, "nvidia smoke ok", false); err != nil {
		return fmt.Errorf("NVIDIA nonstream: %w", err)
	}
	stream := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":16,"messages":[{"role":"user","content":"fixture only"}],"tools":[{"type":"function","function":{"name":"fixture","parameters":{"type":"object","properties":{}}}}]}`, nvAlias)
	response, err := requestJSON(client, http.MethodPost, baseURL+"/v1/chat/completions", clientKey, strings.NewReader(stream))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "nvidia smoke ok") || !strings.Contains(string(response.Body), "[DONE]") {
		return fmt.Errorf("NVIDIA streaming/tools status=%d body=%q", response.StatusCode, response.Body)
	}
	blocked := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"must not reach fixture"}],"max_tokens":16}`, nvAlias)
	response, err = requestJSON(client, http.MethodPost, baseURL+"/v1/chat/completions", clientKey, strings.NewReader(blocked))
	if err != nil {
		return err
	}
	if response.StatusCode != 429 || !strings.Contains(string(response.Body), "nvidia_nim_no_capacity") {
		return fmt.Errorf("NVIDIA local ceiling status=%d, want guarded 429", response.StatusCode)
	}
	response, err = requestJSON(client, http.MethodGet, baseURL+cfStatusPath, "", nil)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(response.Body), `"free_exhausted"`) {
		return errors.New("NVIDIA local ceiling not reflected on public quota page")
	}
	if strings.Contains(string(response.Body), nvSmokeKey) {
		return errors.New("NVIDIA public quota page contains credential")
	}

	// A missing real key leaves the HOST lane disabled, not just the module.
	// That must remove the model even though native scheduling still exists.
	disabled := strings.Replace(originalConfig, "  - name: nvidia-nim\n", "  - name: nvidia-nim\n    disabled: true\n", 1)
	if disabled == originalConfig {
		return errors.New("NVIDIA disabled-host fixture missing")
	}
	if err := replaceSmokeConfig(configPath, disabled); err != nil {
		return err
	}
	if err := restart(); err != nil {
		return err
	}
	response, err = requestJSON(client, http.MethodGet, baseURL+"/v1/models", clientKey, nil)
	if err != nil || response.StatusCode != 200 {
		return errors.New("NVIDIA disabled-host models check unavailable")
	}
	if strings.Contains(string(response.Body), nvAlias) {
		return errors.New("disabled NVIDIA lane still publishes its model")
	}
	response, err = requestJSON(client, http.MethodPost, baseURL+"/v1/chat/completions", clientKey, strings.NewReader(blocked))
	if err != nil {
		return err
	}
	if response.StatusCode == 200 {
		return errors.New("disabled NVIDIA lane admitted inference")
	}
	if err := replaceSmokeConfig(configPath, originalConfig); err != nil {
		return err
	}
	if err := restart(); err != nil {
		return err
	}
	// Restart and re-enable must not replenish the lifetime local ceiling.
	response, err = requestJSON(client, http.MethodPost, baseURL+"/v1/chat/completions", clientKey, strings.NewReader(blocked))
	if err != nil || response.StatusCode != 429 || !strings.Contains(string(response.Body), "nvidia_nim_no_capacity") {
		return errors.New("NVIDIA restart replenished local allowance")
	}
	fmt.Println("NVIDIA local-fixture smoke: nonstream + streaming/tools admitted, third refused, disabled host removes model, restart preserves ceiling; no real NVIDIA traffic")
	return nil
}
