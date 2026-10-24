package nvidianim

import (
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

func TestAmbiguousAliasCannotRouteToANonfreeModel(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		h := hostConfig(t)
		a := providermodule.OpenAICompatibilityModel{Name: "moonshotai/kimi-k3", Alias: "shared"}
		b := providermodule.OpenAICompatibilityModel{Name: "paid/model", Alias: "shared"}
		h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{a, b}
		if reverse {
			h.OpenAICompatibility[0].Models = []providermodule.OpenAICompatibilityModel{b, a}
		}
		m := setup(t, h, freeConfig)
		r := req(m)
		r.Model = "shared"
		assertPick(t, m, r, false)
		r.Model = "moonshotai/kimi-k3"
		assertPick(t, m, r, true)
	}
}

func TestFreeEndpointIsAnExactHTTPSOriginAndPath(t *testing.T) {
	for _, u := range []string{"http://integrate.api.nvidia.com/v1", "https://integrate.api.nvidia.com.evil.invalid/v1", "https://integrate.api.nvidia.com:8443/v1", "https://user@integrate.api.nvidia.com/v1", "https://integrate.api.nvidia.com/v1?paid=true", "https://integrate.api.nvidia.com/v1#fragment", "https://integrate.api.nvidia.com/v1/../paid", "https://integrate.api.nvidia.com/%76%31"} {
		if isFreeEndpoint(u) {
			t.Fatalf("unsafe endpoint accepted: %q", u)
		}
	}
	for _, u := range []string{"https://integrate.api.nvidia.com/v1", "https://integrate.api.nvidia.com/v1/", "https://integrate.api.nvidia.com:443/v1"} {
		if !isFreeEndpoint(u) {
			t.Fatalf("official endpoint refused: %q", u)
		}
	}
}
