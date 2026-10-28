package coordinator

import (
	"path/filepath"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
)

const manifestVersion = 1

// manifest is a diagnostic-only record of which providers this coordinator
// has registered. It is never read back for dispatch decisions — the
// in-memory registry, rebuilt from every live module's OwnedAuthIDs() on
// each reconfigure, stays the sole source of truth for that.
type manifest struct {
	Version       int                      `json:"version"`
	CoordinatorID string                   `json:"coordinator_id"`
	Providers     map[string]manifestEntry `json:"providers"`
}

type manifestEntry struct {
	RegisteredAt time.Time `json:"registered_at"`
}

// writeManifest records every currently hosted module under pluginAuthDir.
// Write failures are soft by design (the caller ignores this function's
// error): manifest.json is diagnostic-only, so a failure here must never
// fail Reconfigure.
func writeManifest(pluginAuthDir string, modules []moduleEntry) error {
	store, err := securestore.New(pluginAuthDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	providers := make(map[string]manifestEntry, len(modules))
	for _, entry := range modules {
		providers[entry.id] = manifestEntry{RegisteredAt: now}
	}
	m := manifest{Version: manifestVersion, CoordinatorID: PluginID, Providers: providers}
	return store.WriteJSON("manifest.json", m)
}

// providerStubDir is the empty, reserved storage directory a provider gets
// before it has any state of its own to persist.
func providerStubDir(pluginAuthDir, providerID string) string {
	return filepath.Join(pluginAuthDir, "providers", providerID)
}
