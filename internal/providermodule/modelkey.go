package providermodule

import (
	"strings"
	"unicode"
)

// RouteKey folds a requested or configured route the way the host matches
// aliases and upstream names (strings.EqualFold), then drops only the owning
// provider's own prefix. Another provider's prefix stays part of the route:
// on this provider "other/x" is a distinct alias that may name a costlier
// upstream than "x".
func RouteKey(provider, raw string) string {
	key := ModelKey(strings.TrimSpace(raw))
	if prefix := ModelKey(provider + "/"); strings.HasPrefix(key, prefix) {
		key = key[len(prefix):]
	}
	return key
}

// ModelKey canonicalizes a model route so two strings share a key exactly
// when strings.EqualFold treats them as equal, which is how the host resolves
// aliases and upstream names. strings.ToLower is not equivalent: it leaves
// fold-equal runes such as U+017F and "s" distinct. Guards must key every
// route through this so a case variant cannot select an unpriced upstream.
func ModelKey(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < low {
				low = f
			}
		}
		out = append(out, low)
	}
	return string(out)
}
