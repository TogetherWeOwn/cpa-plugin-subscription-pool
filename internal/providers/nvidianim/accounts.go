package nvidianim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

type lane struct {
	name         string
	bound        bool
	freeEndpoint bool
	models       map[string]bool
}

// All keys share one conservative allowance. NVIDIA account-level limits must
// not be multiplied by adding another key, duplicate provider entry or alias.
func discover(host providermodule.HostConfig, cfg moduleConfig) map[string]lane {
	out := map[string]lane{}
	counts := map[string]int{}
	ordinal := 0
	for _, compat := range host.OpenAICompatibility {
		if compat.Disabled || !strings.EqualFold(strings.TrimSpace(compat.Name), providerID) {
			continue
		}
		// A shared alias is safe only when every upstream it could select is
		// verified free. Never let entry order turn an ambiguous paid route
		// into a free one (including an alias colliding with a model name,
		// differing only in case, which the host pools, or adding this
		// provider's own prefix, which the host may strip).
		routes := map[string]bool{}
		for _, model := range compat.Models {
			for _, route := range []string{model.Name, model.Alias} {
				key := providermodule.RouteKey(providerID, route)
				if key == "" {
					continue
				}
				free, seen := routes[key]
				routes[key] = cfg.FreeModels[model.Name] && (!seen || free)
			}
		}
		models := map[string]bool{}
		for route, free := range routes {
			if free {
				models[route] = true
			}
		}
		entries := compat.APIKeyEntries
		if len(entries) == 0 {
			entries = []providermodule.OpenAICompatibilityAPIKey{{}}
		}
		for _, entry := range entries {
			kind := "openai-compatibility:" + providerID
			// Mirror the host's StableIDGenerator, including duplicates.
			parts := []string{entry.APIKey, compat.BaseURL, entry.ProxyURL}
			if len(compat.APIKeyEntries) == 0 {
				parts = []string{compat.BaseURL}
			}
			h := sha256.New() // codeql[go/weak-sensitive-data-hashing] Upstream non-security ID contract.
			h.Write([]byte(kind))
			for _, p := range parts {
				h.Write([]byte{0})
				h.Write([]byte(strings.TrimSpace(p)))
			}
			id := kind + ":" + hex.EncodeToString(h.Sum(nil))[:12]
			n := counts[id]
			counts[id]++
			if n > 0 {
				id += fmt.Sprintf("-%d", n)
			}
			ordinal++
			key := strings.TrimSpace(entry.APIKey)
			bound := key != "" && !strings.ContainsAny(key, "<>${} \r\n\t") && !strings.Contains(strings.ToLower(key), "placeholder")
			out[id] = lane{name: fmt.Sprintf("nvidia-nim#%d", ordinal), bound: bound, freeEndpoint: isFreeEndpoint(compat.BaseURL), models: models}
		}
	}
	return out
}

func isFreeEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "integrate.api.nvidia.com") && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.EscapedPath() == "/v1" || u.EscapedPath() == "/v1/")
}
