package openrouterfree

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
	// routes maps a requested route to every upstream id it could select
	// (ModelKey-folded). A shared alias is free only if all of them are.
	routes map[string][]string
}

// hostView is what the module needs from the host's openrouter-free entries.
type hostView struct {
	lanes   map[string]lane
	models  []providermodule.OpenAICompatibilityModel // first entry; what a sync reconciles
	apiKey  string                                    // first bound key, used only for GET /key
	present bool
}

// All keys share one budget: OpenRouter's free-model limits are per account.
func discover(host providermodule.HostConfig) hostView {
	view := hostView{lanes: map[string]lane{}}
	counts := map[string]int{}
	ordinal := 0
	for _, compat := range host.OpenAICompatibility {
		if compat.Disabled || !strings.EqualFold(strings.TrimSpace(compat.Name), providerID) {
			continue
		}
		if !view.present {
			view.present = true
			view.models = append([]providermodule.OpenAICompatibilityModel(nil), compat.Models...)
		}
		routes := map[string][]string{}
		for _, model := range compat.Models {
			upstream := providermodule.ModelKey(strings.TrimSpace(model.Name))
			for _, route := range []string{model.Name, model.Alias} {
				if key := providermodule.RouteKey(providerID, route); key != "" {
					routes[key] = append(routes[key], upstream)
				}
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
			// CLIProxyAPI v7.2.154 internal/watcher/synthesizer/helpers.go
			// uses this digest as a routing label, not a password verifier.
			// Keep the ID in memory and on the internal scheduler boundary;
			// status, logs and durable allowance state must never contain it.
			h := sha256.New()
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
			if bound && isFreeEndpoint(compat.BaseURL) && view.apiKey == "" {
				view.apiKey = key
			}
			view.lanes[id] = lane{name: fmt.Sprintf("openrouter-free#%d", ordinal), bound: bound, freeEndpoint: isFreeEndpoint(compat.BaseURL), routes: routes}
		}
	}
	return view
}

func isFreeEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "openrouter.ai") && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.EscapedPath() == "/api/v1" || u.EscapedPath() == "/api/v1/")
}
