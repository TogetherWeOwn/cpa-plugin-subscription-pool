package openrouterfree

import (
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

func TestDiscoveredIDsMatchPinnedHostRoutingContract(t *testing.T) {
	// Literal vectors for CLIProxyAPI v7.2.154's
	// internal/watcher/synthesizer/helpers.go StableIDGenerator.Next and
	// config.go synthesizeOpenAICompat. These are routing IDs, not password
	// verifiers. Changing the digest or separators silently unbinds real
	// scheduler candidates, so expectations must not call discover itself.
	for _, tc := range []struct {
		name      string
		entries   []providermodule.OpenAICompatibilityAPIKey
		want      []string
		wantBound bool
	}{
		{
			name: "trimmed key and duplicate suffix",
			entries: []providermodule.OpenAICompatibilityAPIKey{
				{APIKey: " test-only-openrouter-key "},
				{APIKey: "test-only-openrouter-key"},
			},
			want: []string{
				"openai-compatibility:openrouter-free:d19a57c28f00",
				"openai-compatibility:openrouter-free:d19a57c28f00-1",
			},
			wantBound: true,
		},
		{
			name: "proxy is part of host identity",
			entries: []providermodule.OpenAICompatibilityAPIKey{
				{APIKey: "test-only-openrouter-key", ProxyURL: " http://proxy.invalid:8080 "},
			},
			want:      []string{"openai-compatibility:openrouter-free:7616a01399e2"},
			wantBound: true,
		},
		{
			name: "keyless fallback cannot admit",
			want: []string{"openai-compatibility:openrouter-free:2b93fe6554b9"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := entry(idA)
			e.Name, e.BaseURL, e.APIKeyEntries = " OpenRouter-Free ", " https://openrouter.ai/api/v1 ", tc.entries
			disabled := e
			disabled.Disabled = true
			view := discover(providermodule.HostConfig{OpenAICompatibility: []providermodule.OpenAICompatibility{disabled, e}})
			if len(view.lanes) != len(tc.want) {
				t.Fatalf("discovered %d lanes, want %d", len(view.lanes), len(tc.want))
			}
			for _, id := range tc.want {
				lane, ok := view.lanes[id]
				if !ok || lane.bound != tc.wantBound {
					t.Fatalf("host routing ID %s not bound as expected", id)
				}
			}
		})
	}
}
