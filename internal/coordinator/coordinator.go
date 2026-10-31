// Package coordinator implements the subscription-pool CLIProxyAPI plugin's
// CGo-facing lifecycle: it owns the scheduler slot, management routes,
// secure storage placement, and dispatch across every provider module it
// hosts. Each providermodule.Module owns its own accounts, quota, pollers,
// health, cursors, settings, and status.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/usage"
)

// PluginID is this plugin's stable, host-facing identifier.
const PluginID = "subscription-pool"

const requiredPluginPriority = 1000

// moduleEntry pairs a provider module with its stable ID, in registration
// order, so iteration order (and therefore registry-conflict messages) is
// deterministic.
type moduleEntry struct {
	id     string
	module providermodule.Module
}

// Coordinator is the CGo ABI-facing type driving every provider module
// behind the subscription-pool plugin. It replaces the pre-Slice-C
// pluginRuntime as the single object src/'s ABI glue calls into.
type Coordinator struct {
	modules []moduleEntry

	mu      sync.RWMutex
	lastErr error
	stopped bool
	// accepted is set once a configuration has been fully applied. Until
	// then no module knows which credentials it owns, so rejected mode
	// fails every pick closed.
	accepted bool
	// hostOwned holds the auth IDs the latest readable host config gives
	// the HostCredentialOwner modules. Rejected mode blocks them as well,
	// so credentials added to the CPA config alongside a rejected plugin
	// config stay guarded.
	hostOwned map[string]struct{}

	registry registry

	// reconfigureMu serializes lifecycle requests end to end, so an older
	// reconfigure can never clear the rejection a newer one recorded, and
	// staging, committing and rolling back modules never interleave.
	// applied records the config each module last accepted in a fully
	// successful reconfigure, so a commit that fails part-way can restore
	// the modules it already changed.
	reconfigureMu sync.Mutex
	applied       map[string]appliedConfig
	// commitMu makes picks wait while modules are being committed or
	// rolled back, so no pick ever sees modules on mixed configs.
	commitMu sync.RWMutex

	reconfigures sync.WaitGroup
	// usage counts handleUsage calls in progress; Shutdown waits for them
	// before its final ledger flush, so no accepted record misses it.
	usage sync.WaitGroup

	// ledger counts every usage record a hosted module accepts, for the
	// usage page; ledgerStore persists it under the plugin's auth dir.
	ledger      *usage.Ledger
	ledgerStore ledgerPersistence

	now func() time.Time
}

// New constructs a Coordinator hosting the given provider modules. Module
// IDs must be unique; New panics on a duplicate, since that is a
// programming error caught at construction, never a runtime input.
func New(modules ...providermodule.Module) *Coordinator {
	c := &Coordinator{now: func() time.Time { return time.Now().UTC() }, ledger: usage.New()}
	seen := make(map[string]struct{}, len(modules))
	for _, module := range modules {
		id := module.ID()
		if _, exists := seen[id]; exists {
			panic(fmt.Sprintf("coordinator: duplicate provider module id %q", id))
		}
		seen[id] = struct{}{}
		c.modules = append(c.modules, moduleEntry{id: id, module: module})
	}
	return c
}

// appliedConfig is one module's share of an accepted reconfigure.
type appliedConfig struct {
	host           providermodule.HostConfig
	providerConfig json.RawMessage
}

// stagedModule is one module's share of the reconfigure being applied.
type stagedModule struct {
	entry          moduleEntry
	providerConfig json.RawMessage
}

// configRejectedError is the hard scheduler error pick returns while the
// most recent configuration is rejected. It carries a stable wire code so
// the ABI glue reports it as such instead of a generic plugin error.
type configRejectedError struct{ reason string }

func (e *configRejectedError) Error() string {
	return "coordinator_config_rejected: " + e.message()
}

func (e *configRejectedError) message() string {
	return "plugin configuration was rejected; managed credentials stay blocked until a valid configuration is applied: " + e.reason
}

func (e *configRejectedError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: "coordinator_config_rejected", Message: e.message()}
}

func (c *Coordinator) runtimeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

// reconfigure applies rawConfig (the full "config_yaml" lifecycle payload,
// i.e. the plugin's own providers.* subtree) to every hosted module and
// rebuilds the dispatch registry from their post-reconfigure ownership.
func (c *Coordinator) reconfigure(rawConfig []byte) error {
	c.reconfigureMu.Lock()
	defer c.reconfigureMu.Unlock()

	cfg, err := parseCoordinatorConfig(rawConfig)
	if err != nil {
		return c.recordError(err)
	}
	configPath, err := resolveCPAConfigPath(cfg.CPAConfigPath)
	if err != nil {
		return c.recordError(err)
	}
	cpa, err := loadHostCPAConfig(configPath)
	if err != nil {
		return c.recordError(err)
	}
	c.recordHostOwned(providermodule.HostConfig{
		CPAConfigPath:       configPath,
		ClaudeKeys:          cpa.ClaudeKeys,
		OpenAICompatibility: cpa.OpenAICompatibility,
	})
	if err = validateSchedulerDeployment(cpa.Plugins, cfg.CoexistingPlugins); err != nil {
		return c.recordError(err)
	}
	rootAuthDir, err := resolveAuthDir(cpa.AuthDir)
	if err != nil {
		return c.recordError(err)
	}
	pluginAuthDir := filepath.Join(rootAuthDir, PluginID)
	if err := securestore.EnsureEmptyProviderDir(pluginAuthDir); err != nil {
		return c.recordError(err)
	}
	c.attachLedgerStore(pluginAuthDir)

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	c.reconfigures.Add(1)
	c.mu.Unlock()
	defer c.reconfigures.Done()

	host := providermodule.HostConfig{
		CPAConfigPath:       configPath,
		AuthDir:             pluginAuthDir,
		ClaudeKeys:          cpa.ClaudeKeys,
		OpenAICompatibility: cpa.OpenAICompatibility,
	}

	// Stage: resolve and validate every module's config, and reserve its
	// storage, before any module commits. A config one module rejects
	// therefore leaves every module on its previous config.
	staged := make([]stagedModule, 0, len(c.modules))
	for _, entry := range c.modules {
		providerConfig, err := providerConfigRaw(cfg, entry.id)
		if err != nil {
			return c.recordError(err)
		}
		if validator, ok := entry.module.(providermodule.ConfigValidator); ok {
			if err := validator.ValidateConfig(host, providerConfig); err != nil {
				return c.recordError(fmt.Errorf("provider %s: %w", entry.id, err))
			}
		}
		if err := securestore.EnsureEmptyProviderDir(providerStubDir(pluginAuthDir, entry.id)); err != nil {
			return c.recordError(err)
		}
		staged = append(staged, stagedModule{entry: entry, providerConfig: providerConfig})
	}

	// Commit. A module can still fail here (storage, shutdown); the modules
	// already committed are then restored to their last accepted config.
	// Picks wait until the outcome, rejection included, is published.
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	for i, next := range staged {
		if err := next.entry.module.Reconfigure(context.Background(), host, next.providerConfig); err != nil {
			return c.recordError(errors.Join(fmt.Errorf("provider %s: %w", next.entry.id, err), c.rollback(staged[:i])))
		}
	}

	owned := make(map[string][]string, len(c.modules))
	for _, entry := range c.modules {
		owned[entry.id] = entry.module.OwnedAuthIDs(context.Background())
	}
	byAuth, err := buildRegistry(owned)
	if err != nil {
		return c.recordError(errors.Join(err, c.rollback(staged)))
	}
	c.registry.swap(byAuth)

	applied := make(map[string]appliedConfig, len(staged))
	for _, next := range staged {
		applied[next.entry.id] = appliedConfig{host: host, providerConfig: next.providerConfig}
	}
	c.applied = applied

	if err := writeManifest(pluginAuthDir, c.modules); err != nil {
		// Manifest is diagnostic-only; never authoritative for dispatch.
		_ = err
	}

	c.mu.Lock()
	if !c.stopped {
		c.lastErr = nil
	}
	c.accepted = true
	c.mu.Unlock()
	return nil
}

// rollback restores each committed module to the config it last accepted.
// It is best effort: a module with no accepted config, or one that refuses
// its old config, keeps whatever it has, and the coordinator's rejected
// mode keeps pick failing closed either way. The returned error names each
// module that refused, for status. Callers hold reconfigureMu and commitMu.
func (c *Coordinator) rollback(committed []stagedModule) error {
	var failed []error
	for _, next := range committed {
		previous, ok := c.applied[next.entry.id]
		if !ok {
			continue
		}
		if err := next.entry.module.Reconfigure(context.Background(), previous.host, previous.providerConfig); err != nil {
			failed = append(failed, fmt.Errorf("rollback provider %s: %w", next.entry.id, err))
		}
	}
	return errors.Join(failed...)
}

// recordHostOwned replaces hostOwned with the auth IDs host gives the
// HostCredentialOwner modules.
func (c *Coordinator) recordHostOwned(host providermodule.HostConfig) {
	owned := make(map[string]struct{})
	for _, entry := range c.modules {
		if owner, ok := entry.module.(providermodule.HostCredentialOwner); ok {
			for _, id := range owner.HostAuthIDs(host) {
				owned[strings.TrimSpace(id)] = struct{}{}
			}
		}
	}
	c.mu.Lock()
	c.hostOwned = owned
	c.mu.Unlock()
}

func (c *Coordinator) recordError(err error) error {
	if err == nil {
		return nil
	}
	c.mu.Lock()
	if !c.stopped {
		c.lastErr = err
	}
	c.mu.Unlock()
	return err
}

func (c *Coordinator) validationStatus() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastErr == nil {
		return ""
	}
	return c.lastErr.Error()
}

// pick implements the 6 dispatch invariants documented in
// internal/providermodule.Module: every module's Recognize is consulted
// before any Pick is called, zero recognizing modules yields Handled:false,
// more than one recognizing module fails closed, and a recognized module's
// own Pick result (including its unmanaged/no-capacity errors) is
// propagated verbatim.
func (c *Coordinator) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	c.commitMu.RLock()
	defer c.commitMu.RUnlock()
	c.mu.RLock()
	stopped, rejected, accepted, hostOwned := c.stopped, c.lastErr, c.accepted, c.hostOwned
	c.mu.RUnlock()
	if stopped {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("plugin is shutting down")
	}
	if rejected != nil {
		if !accepted {
			// No configuration was ever accepted: modules recognize
			// nothing, so no candidate can be shown to be unguarded.
			return pluginapi.SchedulerPickResponse{}, &configRejectedError{reason: rejected.Error()}
		}
		return c.pickRejected(req, rejected, hostOwned)
	}

	var recognizing []moduleEntry
	for _, entry := range c.modules {
		for _, candidate := range req.Candidates {
			if entry.module.Recognize(candidate) {
				recognizing = append(recognizing, entry)
				break
			}
		}
	}
	switch len(recognizing) {
	case 0:
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	case 1:
		return recognizing[0].module.Pick(context.Background(), req)
	default:
		ids := make([]string, 0, len(recognizing))
		for _, entry := range recognizing {
			ids = append(ids, entry.id)
		}
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("coordinator_mixed_provider_pick: candidates recognized by multiple providers: %s", strings.Join(ids, ", "))
	}
}

// pickRejected answers picks while the most recent configuration is
// rejected after an earlier one was accepted. The plugin stays registered as the host's scheduler, and every
// candidate a hosted module recognizes, that the last accepted registry
// owned, or that the host config gives a HostCredentialOwner module, gets a
// hard error. Only requests with none of those keep Handled:false, so
// unrelated providers stay untouched.
func (c *Coordinator) pickRejected(req pluginapi.SchedulerPickRequest, rejected error, hostOwned map[string]struct{}) (pluginapi.SchedulerPickResponse, error) {
	for _, candidate := range req.Candidates {
		id := strings.TrimSpace(candidate.ID)
		if _, owned := hostOwned[id]; owned || c.registry.providerFor(id) != "" {
			return pluginapi.SchedulerPickResponse{}, &configRejectedError{reason: rejected.Error()}
		}
		for _, entry := range c.modules {
			if entry.module.Recognize(candidate) {
				return pluginapi.SchedulerPickResponse{}, &configRejectedError{reason: rejected.Error()}
			}
		}
	}
	return pluginapi.SchedulerPickResponse{Handled: false}, nil
}

// handleUsage dispatches strictly by registry lookup of record.AuthID,
// never by record.Provider — invariant 4. An auth ID the registry does not
// map to any hosted module is a silent no-op, matching the pre-coordinator
// behavior of ignoring usage records belonging to other CPA providers.
func (c *Coordinator) handleUsage(record pluginapi.UsageRecord) error {
	// Register under the same lock Shutdown uses to set stopped, so no
	// handler can start between Shutdown's drain and its final flush.
	c.mu.RLock()
	if c.stopped {
		c.mu.RUnlock()
		return nil
	}
	c.usage.Add(1)
	c.mu.RUnlock()
	defer c.usage.Done()

	providerID := c.registry.providerFor(strings.TrimSpace(record.AuthID))
	if providerID == "" {
		providerID = c.usageClaimant(record)
	}
	if providerID == "" {
		return nil
	}
	for _, entry := range c.modules {
		if entry.id == providerID {
			err := entry.module.HandleUsage(context.Background(), record)
			c.recordUsage(entry, record)
			return err
		}
	}
	return nil
}

// usageClaimant returns the ID of the single module that claims record via
// providermodule.UsageRecognizer, or "" when none or more than one does.
// Ambiguity is a silent no-op, matching the registry's behavior for usage
// that belongs to no hosted module.
func (c *Coordinator) usageClaimant(record pluginapi.UsageRecord) string {
	claimant := ""
	for _, entry := range c.modules {
		recognizer, ok := entry.module.(providermodule.UsageRecognizer)
		if !ok || !recognizer.RecognizeUsage(record) {
			continue
		}
		if claimant != "" {
			return ""
		}
		claimant = entry.id
	}
	return claimant
}

// managementStatusBody is the redacted JSON served at the coordinator's
// status route, aggregating every module's own Status() payload.
type managementStatusBody struct {
	Plugin          string                     `json:"plugin"`
	Status          string                     `json:"status"`
	Version         string                     `json:"version"`
	GeneratedAt     time.Time                  `json:"generated_at"`
	ValidationError string                     `json:"validation_error,omitempty"`
	Providers       map[string]json.RawMessage `json:"providers,omitempty"`
}

// PluginVersion is stamped at build time via -ldflags by src/'s build; it is
// the single source of truth src/'s own pluginRegistration() also reports to
// the host, so the registered plugin metadata and the management status body
// never disagree on version.
var PluginVersion = "0.0.0-dev"

func (c *Coordinator) managementStatus() managementStatusBody {
	status := "registered"
	validationErr := c.validationStatus()
	if validationErr != "" {
		status = "reconfigure_rejected"
	}
	providers := make(map[string]json.RawMessage, len(c.modules))
	for _, entry := range c.modules {
		raw, err := entry.module.Status(context.Background())
		if err != nil {
			continue
		}
		providers[entry.id] = raw
	}
	return managementStatusBody{
		Plugin:          PluginID,
		Status:          status,
		Version:         PluginVersion,
		GeneratedAt:     c.runtimeNow(),
		ValidationError: validationErr,
		Providers:       providers,
	}
}

// Reconfigure is the exported entry point src/'s CGo ABI glue calls on
// plugin register/reconfigure.
func (c *Coordinator) Reconfigure(rawConfig []byte) error { return c.reconfigure(rawConfig) }

// Pick is the exported entry point src/'s CGo ABI glue calls for a scheduler
// pick request.
func (c *Coordinator) Pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	return c.pick(req)
}

// HandleUsage is the exported entry point src/'s CGo ABI glue calls for a
// usage-plugin record.
func (c *Coordinator) HandleUsage(record pluginapi.UsageRecord) error { return c.handleUsage(record) }

// Shutdown is the exported entry point src/'s CGo ABI glue calls on plugin
// shutdown.
func (c *Coordinator) Shutdown() error { return c.shutdown() }

// RecordError is the exported entry point src/'s CGo ABI glue calls to
// record a lifecycle-request decode failure. It is ordered with reconfigures
// like any other lifecycle request.
func (c *Coordinator) RecordError(err error) error {
	c.reconfigureMu.Lock()
	defer c.reconfigureMu.Unlock()
	return c.recordError(err)
}

// ValidationStatus is the exported entry point src/'s CGo ABI glue calls to
// report the plugin's own reconfigure-rejection status.
func (c *Coordinator) ValidationStatus() string { return c.validationStatus() }

// OwnedAuthIDs returns the auth IDs currently owned by the named provider
// module, or nil if no hosted module has that ID. It exists for ABI-layer
// tests that need a concrete managed auth ID without reaching into any
// module's own private account types.
func (c *Coordinator) OwnedAuthIDs(providerID string) []string {
	for _, entry := range c.modules {
		if entry.id == providerID {
			return entry.module.OwnedAuthIDs(context.Background())
		}
	}
	return nil
}

func (c *Coordinator) shutdown() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	c.reconfigures.Wait()
	c.usage.Wait()
	c.flushLedger()

	var firstErr error
	for _, entry := range c.modules {
		if err := entry.module.Close(context.Background()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
