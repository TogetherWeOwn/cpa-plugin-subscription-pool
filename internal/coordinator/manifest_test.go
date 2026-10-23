package coordinator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

func TestWriteManifestRecordsEveryHostedProvider(t *testing.T) {
	pluginAuthDir := t.TempDir()
	modules := []moduleEntry{
		{id: "zai", module: &fakeModule{id: "zai"}},
		{id: "opencode-go", module: &fakeModule{id: "opencode-go"}},
	}

	if err := writeManifest(pluginAuthDir, modules); err != nil {
		t.Fatalf("writeManifest() error = %v", err)
	}

	store, err := securestore.New(pluginAuthDir)
	if err != nil {
		t.Fatalf("securestore.New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	var got manifest
	if err := store.ReadJSON("manifest.json", &got); err != nil {
		t.Fatalf("ReadJSON(manifest.json) error = %v", err)
	}
	if got.Version != manifestVersion {
		t.Fatalf("Version = %d, want %d", got.Version, manifestVersion)
	}
	if got.CoordinatorID != PluginID {
		t.Fatalf("CoordinatorID = %q, want %q", got.CoordinatorID, PluginID)
	}
	if _, ok := got.Providers["zai"]; !ok {
		t.Fatal("manifest missing provider \"zai\"")
	}
	if _, ok := got.Providers["opencode-go"]; !ok {
		t.Fatal("manifest missing provider \"opencode-go\"")
	}
}

func TestWriteManifestIsIdempotentAcrossReconfigures(t *testing.T) {
	pluginAuthDir := t.TempDir()
	modules := []moduleEntry{{id: "zai", module: &fakeModule{id: "zai"}}}

	if err := writeManifest(pluginAuthDir, modules); err != nil {
		t.Fatalf("first writeManifest() error = %v", err)
	}
	if err := writeManifest(pluginAuthDir, modules); err != nil {
		t.Fatalf("second writeManifest() error = %v", err)
	}
}

// providerStubDir must be created empty and left unpopulated for a provider
// (opencode-go) that has no module code of its own yet — "create but don't
// populate" per plan §7.
func TestProviderStubDirIsCreatedEmpty(t *testing.T) {
	pluginAuthDir := t.TempDir()
	stubDir := providerStubDir(pluginAuthDir, "opencode-go")

	if got, want := stubDir, filepath.Join(pluginAuthDir, "providers", "opencode-go"); got != want {
		t.Fatalf("providerStubDir() = %q, want %q", got, want)
	}

	if err := securestore.EnsureEmptyProviderDir(stubDir); err != nil {
		t.Fatalf("EnsureEmptyProviderDir() error = %v", err)
	}

	info, err := os.Stat(stubDir)
	if err != nil {
		t.Fatalf("stat stub dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("stub dir is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("stub dir perm = %o, want 0700", info.Mode().Perm())
	}

	entries, err := os.ReadDir(stubDir)
	if err != nil {
		t.Fatalf("read stub dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("stub dir has %d entries, want 0 (unpopulated)", len(entries))
	}
}

func TestProviderStubDirEnsureIsIdempotent(t *testing.T) {
	pluginAuthDir := t.TempDir()
	stubDir := providerStubDir(pluginAuthDir, "opencode-go")

	if err := securestore.EnsureEmptyProviderDir(stubDir); err != nil {
		t.Fatalf("first EnsureEmptyProviderDir() error = %v", err)
	}
	if err := securestore.EnsureEmptyProviderDir(stubDir); err != nil {
		t.Fatalf("second EnsureEmptyProviderDir() error = %v", err)
	}
}
