package zai

import (
	"os"
	"path/filepath"
	"testing"
)

// auth-dir decides where CPA credentials are read from and where the plugin
// keeps its secure state, so "~" expansion and the default must land under the
// real home directory rather than a literal "~" directory under the cwd.
func TestResolveAuthDirExpandsHomeAndDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":                  filepath.Join(home, ".cli-proxy-api"),
		"   ":               filepath.Join(home, ".cli-proxy-api"),
		"~":                 home,
		"~/":                home,
		"~/auth":            filepath.Join(home, "auth"),
		" ~/auth/../creds ": filepath.Join(home, "creds"),
		"/srv/cpa/auth/":    "/srv/cpa/auth",
		"relative/auth":     filepath.Join(cwd, "relative", "auth"),
		"~other/auth":       filepath.Join(cwd, "~other", "auth"),
	}
	for raw, want := range cases {
		got, err := resolveAuthDir(raw)
		if err != nil || got != want {
			t.Fatalf("resolveAuthDir(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestResolveCPAConfigPathRequiresValueAndAbsolutizes(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "  ", "."} {
		if got, err := resolveCPAConfigPath(raw); err == nil {
			t.Fatalf("resolveCPAConfigPath(%q) = %q, want error", raw, got)
		}
	}
	if got, err := resolveCPAConfigPath(" conf/config.yaml "); err != nil || got != filepath.Join(cwd, "conf", "config.yaml") {
		t.Fatalf("relative path = %q, %v", got, err)
	}
	if got, err := resolveCPAConfigPath("/etc/cpa/../cpa/config.yaml"); err != nil || got != "/etc/cpa/config.yaml" {
		t.Fatalf("absolute path = %q, %v", got, err)
	}
}
