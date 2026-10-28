package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestShippedRuntimeIncludesAllFiveProviderModules(t *testing.T) {
	resp := runtimeState.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/subscription-pool/status"})
	var s struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(resp.Body, &s); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"zai", "opencode-go", "synthetic", "cloudflare-workers-ai", "nvidia-nim"} {
		if len(s.Providers[id]) == 0 {
			t.Fatalf("default runtime does not host %s", id)
		}
	}
}
