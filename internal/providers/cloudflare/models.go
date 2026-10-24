package cloudflare

import (
	"math"
	"strings"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

// Published rates and context limits, retrieved 2026-10-24. Neurons are per
// million tokens, not USD. Cached input is deliberately charged as uncached
// for reservations. No independent quality bar is changed by this catalog.
// https://developers.cloudflare.com/workers-ai/platform/pricing/
// https://developers.cloudflare.com/workers-ai/models/<model>/
// Both prompt and completion are bounded by the FULL published context here;
// the scheduler ABI does not expose either requested length. This deliberately
// over-reserves. Multimodal models are PAYG-only: token rates do not bound
// arbitrary image processing costs. Unknown models fail closed in included mode.
type modelPrice struct {
	Upstream      string
	Alias         string
	Context       int64
	InputNeurons  float64
	OutputNeurons float64
	TextOnly      bool
}

var modelCatalog = []modelPrice{
	{"@cf/qwen/qwen3-30b-a3b-fp8", "qwen3-30b-a3b-fp8", 32768, 4625, 30475, true},
	{"@cf/zai-org/glm-4.7-flash", "glm-4.7-flash", 131072, 5500, 36400, true},
	{"@cf/openai/gpt-oss-120b", "gpt-oss-120b", 128000, 31818, 68182, true},
	{"@cf/zai-org/glm-5.3", "glm-5.3", 1048576, 127273, 400000, false},
	{"@cf/zai-org/glm-5.3-flash", "glm-5.3-flash", 1048576, 13636, 45455, false},
	{"@cf/deepseek-ai/deepseek-v4.1-flash-1017", "deepseek-v4.1-flash-1017", 1048576, 27273, 109091, false},
	{"@cf/moonshotai/kimi-k2.7-code", "kimi-k2.7-code", 262144, 86364, 363636, false},
	{"@cf/qwen/qwen3.8-27b", "qwen3.8-27b", 262144, 40909, 290909, false},
}

func reservationFor(model string) (float64, bool) {
	// Candidates have already been restricted to this provider. Hosts can pass
	// an upstream ID, a prefixed alias, or the alias after prefix resolution.
	for _, m := range modelCatalog {
		if model != m.Upstream && model != providerID+"/"+m.Alias && model != m.Alias {
			continue
		}
		if !m.TextOnly {
			return 0, false
		}
		return math.Ceil(float64(m.Context) * (m.InputNeurons + m.OutputNeurons) / 1e6), true
	}
	return 0, false
}

// routeKey folds a requested or configured route the way the host matches
// aliases and upstream names (strings.EqualFold), then drops only the owning
// provider's own prefix. Another provider's prefix stays part of the route:
// on this provider "other/x" is a distinct alias that may name a costlier
// upstream than "x".
func routeKey(provider, raw string) string {
	key := providermodule.ModelKey(strings.TrimSpace(raw))
	if prefix := providermodule.ModelKey(provider + "/"); strings.HasPrefix(key, prefix) {
		key = key[len(prefix):]
	}
	return key
}

// Bind reservations to the host's actual upstream, not a cheap-looking alias.
// A zero entry is unbounded. Conflicting aliases keep the highest reservation,
// or remain unbounded if any possible upstream is unknown/multimodal. Keys are
// routeKey values, so case variants the host pools together share one entry.
func configuredReservations(provider string, models []providermodule.OpenAICompatibilityModel) map[string]float64 {
	out := map[string]float64{}
	for _, configured := range models {
		upstream := strings.TrimSpace(configured.Name)
		reserve := 0.0
		for _, known := range modelCatalog {
			if upstream == known.Upstream && known.TextOnly {
				reserve, _ = reservationFor(upstream)
				break
			}
		}
		for _, raw := range []string{upstream, configured.Alias} {
			key := routeKey(provider, raw)
			if key == "" {
				continue
			}
			if previous, exists := out[key]; exists {
				if previous == 0 || reserve == 0 {
					out[key] = 0
				} else {
					out[key] = math.Max(previous, reserve)
				}
			} else {
				out[key] = reserve
			}
		}
	}
	return out
}

func canonicalProvider(raw string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "openai-compatible-")
}
