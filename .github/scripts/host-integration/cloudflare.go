package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cfSmokeAccount = "0123456789abcdef0123456789abcdef"
const cfSmokeKey = "test-only-cloudflare-integration-key"
const cfAlias = "cloudflare-workers-ai/qwen3-30b-a3b-fp8"
const cfStatusPath = "/v0/resource/plugins/subscription-pool/usage.json"

// Runs ONLY against the API-key-only throwaway host and local TLS fixture.
// The container must map api.cloudflare.com to loopback with networking off.
func verifyCloudflareHTTP(client *http.Client, baseURL, root, configPath, originalConfig string, restart func() error) error {
	if err := waitCloudflareMode(client, baseURL, true); err != nil {
		return err
	}
	// Full-context reservation is 1,151 neurons, so eight fit below 9,500.
	for i := 0; i < 8; i++ {
		if err := inferenceRoundTrip(client, baseURL, "/v1/chat/completions", cfAlias, "cloudflare smoke ok", false); err != nil {
			return fmt.Errorf("Cloudflare bounded pick %d: %w", i+1, err)
		}
	}
	if err := cloudflareRefused(client, baseURL); err != nil {
		return err
	}
	response, err := requestJSON(client, http.MethodGet, baseURL+cfStatusPath, "", nil)
	if err != nil || response.StatusCode != 200 {
		return fmt.Errorf("Cloudflare usage JSON unavailable: %v", err)
	}
	if !strings.Contains(string(response.Body), `"included_exhausted"`) {
		return errors.New("Cloudflare boundary not reflected on quota page")
	}
	for _, secret := range []string{cfSmokeAccount, cfSmokeKey, "openai-compatibility:"} {
		if strings.Contains(string(response.Body), secret) {
			return errors.New("Cloudflare public quota data contains private identifiers")
		}
	}
	if out := os.Getenv("SP_CLOUDFLARE_PAGE_OUT"); out != "" {
		page, err := requestJSON(client, http.MethodGet, baseURL+"/v0/resource/plugins/subscription-pool/status", "", nil)
		if err != nil || page.StatusCode != 200 {
			return errors.New("Cloudflare page render failed")
		}
		if err := os.MkdirAll(out, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "quota.html"), page.Body, 0600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "usage.json"), response.Body, 0600); err != nil {
			return err
		}
	}
	// Replace the FULL isolated test config; never touch production management.
	payg := strings.Replace(originalConfig, "stayWithinIncluded: true", "stayWithinIncluded: false", 1)
	if payg == originalConfig {
		return errors.New("Cloudflare toggle fixture absent")
	}
	if err := replaceSmokeConfig(configPath, payg); err != nil {
		return err
	}
	if err := restart(); err != nil {
		return err
	}
	if err := waitCloudflareMode(client, baseURL, false); err != nil {
		return err
	}
	if err := inferenceRoundTrip(client, baseURL, "/v1/chat/completions", cfAlias, "cloudflare smoke ok", false); err != nil {
		return fmt.Errorf("Cloudflare PAYG toggle: %w", err)
	}
	if err := replaceSmokeConfig(configPath, originalConfig); err != nil {
		return err
	}
	if err := restart(); err != nil {
		return err
	}
	if err := waitCloudflareMode(client, baseURL, true); err != nil {
		return err
	}
	if err := cloudflareRefused(client, baseURL); err != nil {
		return err
	}
	fmt.Println("Cloudflare local-fixture smoke: 8 bounded requests admitted, ninth refused, PAYG admitted, re-enabled guard refused; daily quota page redacted")
	return nil
}

func replaceSmokeConfig(path, config string) error {
	if err := os.WriteFile(path+".tmp", []byte(config), 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func waitCloudflareMode(client *http.Client, baseURL string, included bool) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := requestJSON(client, http.MethodGet, baseURL+statusPath, managementKey, nil)
		if err == nil && response.StatusCode == 200 {
			var status struct {
				Providers map[string]struct {
					Stay     bool `json:"stayWithinIncluded"`
					Accounts []struct {
						Health string `json:"health"`
					} `json:"accounts"`
				} `json:"providers"`
			}
			if json.Unmarshal(response.Body, &status) == nil {
				if p, ok := status.Providers["cloudflare-workers-ai"]; ok && p.Stay == included && len(p.Accounts) == 1 && p.Accounts[0].Health != "unknown" {
					return nil
				}
				// Re-enabling after PAYG intentionally has health unknown until next UTC
				// day. Config mode, not a green quota, confirms the reload completed.
				if p, ok := status.Providers["cloudflare-workers-ai"]; ok && p.Stay == included && len(p.Accounts) == 1 && strings.Contains(string(response.Body), "recent_payg_usage") {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Cloudflare mode/analytics fixture did not become ready")
}

func cloudflareRefused(client *http.Client, baseURL string) error {
	raw := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"must not reach fixture"}],"max_tokens":16}`, cfAlias)
	response, err := requestJSON(client, http.MethodPost, baseURL+"/v1/chat/completions", clientKey, strings.NewReader(raw))
	if err != nil {
		return err
	}
	if response.StatusCode != 429 || !strings.Contains(string(response.Body), "cloudflare_no_capacity") {
		return fmt.Errorf("Cloudflare fence response=%d (want guarded 429)", response.StatusCode)
	}
	return nil
}
