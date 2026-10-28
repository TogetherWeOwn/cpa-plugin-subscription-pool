package providermodule

import (
	"strings"
	"testing"
)

func TestModelKeyMatchesEqualFold(t *testing.T) {
	words := []string{"route", "ROUTE", "Route", "ſtep", "step", "STEP", "Kimi", "kimi", "KIMI", "straße", "STRASSE", "\xff", "\xfe", "a/b", "A/B", ""}
	for _, a := range words {
		for _, b := range words {
			if got, want := ModelKey(a) == ModelKey(b), strings.EqualFold(a, b); got != want {
				t.Fatalf("ModelKey(%q)==ModelKey(%q) = %v, EqualFold = %v", a, b, got, want)
			}
		}
	}
}

func TestRouteKeyDropsOnlyTheOwnProviderPrefix(t *testing.T) {
	for raw, want := range map[string]string{
		"x":                       ModelKey("x"),
		" NVIDIA-NIM/X ":          ModelKey("x"),
		"nvidia-nim/a/b":          ModelKey("a/b"),
		"other/x":                 ModelKey("other/x"),
		"nvidia-nimx":             ModelKey("nvidia-nimx"),
		"nvidia-nim/":             "",
		"nvidia-nim/nvidia-nim/x": ModelKey("nvidia-nim/x"),
	} {
		if got := RouteKey("nvidia-nim", raw); got != want {
			t.Fatalf("RouteKey(%q) = %q, want %q", raw, got, want)
		}
	}
}
