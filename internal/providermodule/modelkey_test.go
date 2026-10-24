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
