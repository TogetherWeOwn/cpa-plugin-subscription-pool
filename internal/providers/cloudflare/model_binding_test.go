package cloudflare

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

func TestIncludedReservationUsesConfiguredUpstreamNotAliasSpelling(t *testing.T) {
	for _, upstream := range []string{"@cf/openai/gpt-oss-120b", "@cf/zai-org/glm-5.3", "@cf/unknown/unpriced"} {
		t.Run(upstream, func(t *testing.T) {
			m, _, h := configured(t, `{}`)
			h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{{Name: upstream, Alias: providerID + "/qwen3-30b-a3b-fp8"}}
			if err := m.Reconfigure(context.Background(), h, nil); err != nil {
				t.Fatal(err)
			}
			m.pollOnce(context.Background(), true)
			if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
				t.Fatal("cheap alias disguised a costly or unbounded upstream")
			}
		})
	}
}

func TestStatusUsesConfiguredModelsForRemainingHeadroom(t *testing.T) {
	m, _, h := configured(t, `{}`)
	h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{{Name: "@cf/zai-org/glm-4.7-flash", Alias: "glm-4.7-flash"}}
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	r := pickRequest(m, 0)
	r.Model = "glm-4.7-flash"
	if _, err := m.Pick(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	raw, err := m.Status(context.Background())
	var status StatusView
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("invalid status")
	}
	if status.Accounts[0].Health != "exhausted" {
		t.Fatal("quota page advertised capacity that cannot fit any configured model")
	}
}

func TestIncludedRequiresModelMappingAndAllowsVerifiedCustomAlias(t *testing.T) {
	m, _, h := configured(t, `{}`)
	h.OpenAICompatibility[0].Models = nil
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("unconfigured model alias admitted")
	}
	h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{{Name: "@cf/qwen/qwen3-30b-a3b-fp8", Alias: providerID + "/verified-custom"}}
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	r := pickRequest(m, 0)
	r.Model = providerID + "/verified-custom"
	if _, err := m.Pick(context.Background(), r); err != nil {
		t.Fatalf("verified upstream with custom alias refused: %v", err)
	}
}
