package cloudflare

import (
	"context"
	"encoding/json"
	"strings"
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

func TestCaseVariantAliasCannotBorrowACheaperReservation(t *testing.T) {
	cheap := providermodule.OpenAICompatibilityModel{Name: "@cf/qwen/qwen3-30b-a3b-fp8", Alias: providerID + "/step"}
	m, _, h := configured(t, `{}`)
	h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{cheap}
	reconfigure(t, m, h, `{}`)
	m.pollOnce(context.Background(), true)
	mustAdmit(t, m, pickModel(m, 0, strings.ToUpper(providerID)+"/STEP")) // the host matches case-insensitively
	// The host pools every fold-equal alias, so the costlier upstream must
	// bound each spelling regardless of configuration order.
	for _, variant := range []string{"STEP", "Step", "ſtep"} {
		costly := providermodule.OpenAICompatibilityModel{Name: "@cf/openai/gpt-oss-120b", Alias: providerID + "/" + variant}
		for _, models := range [][]providermodule.OpenAICompatibilityModel{{cheap, costly}, {costly, cheap}} {
			m, _, h := configured(t, `{}`)
			h.OpenAICompatibility[0].Models = models
			reconfigure(t, m, h, `{}`)
			m.pollOnce(context.Background(), true)
			for _, model := range []string{providerID + "/step", providerID + "/" + variant, strings.ToUpper(providerID) + "/step", "step"} {
				mustRefuse(t, m, pickModel(m, 0, model), "included_exhausted")
			}
		}
	}
}

func TestAnotherProvidersPrefixIsPartOfTheRoute(t *testing.T) {
	other := providerID + "-other"
	m, _, h := configured(t, `{}`)
	h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{
		{Name: "@cf/qwen/qwen3-30b-a3b-fp8", Alias: providerID + "/qwen3-30b-a3b-fp8"},
		{Name: "@cf/openai/gpt-oss-120b", Alias: other + "/qwen3-30b-a3b-fp8"},
	}
	reconfigure(t, m, h, `{"providers":["`+providerID+`","`+other+`"]}`)
	m.pollOnce(context.Background(), true)
	mustRefuse(t, m, pickModel(m, 0, other+"/qwen3-30b-a3b-fp8"), "included_exhausted")
	m.mu.Lock()
	debited := m.budgetLocked(testAccount, m.now()).Committed
	m.mu.Unlock()
	if debited != 0 {
		t.Fatalf("refused request debited %v neurons", debited)
	}
	mustAdmit(t, m, pickModel(m, 0, providerID+"/qwen3-30b-a3b-fp8"))
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
