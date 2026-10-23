package zai

import (
	"context"
	"encoding/json"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

// zaiModule adapts the existing pluginRuntime to the coordinator-facing
// providermodule.Module boundary. It owns no state of its own beyond the
// wrapped runtime: every method delegates straight through to the runtime's
// existing, behaviorally-unchanged implementation.
type zaiModule struct {
	runtime *pluginRuntime
}

// NewModule constructs a zai provider module wrapping a fresh runtime.
func NewModule() *zaiModule {
	return &zaiModule{runtime: &pluginRuntime{}}
}

func (m *zaiModule) ID() string { return "zai" }

func (m *zaiModule) Recognize(candidate pluginapi.SchedulerAuthCandidate) bool {
	if recognizedZAICandidate(candidate) {
		return true
	}
	// A managed account's candidate does not always carry the attributes
	// recognizedZAICandidate inspects (e.g. bare {ID: authID} candidates, as
	// the host constructs for already-known auth records) -- the
	// pre-coordinator pick() treated snapshot membership as recognition in
	// its own right, and this module boundary must not narrow that.
	snapshot, err := m.runtime.current()
	if err != nil || snapshot == nil {
		return false
	}
	_, managed := snapshot.byAuthID[strings.TrimSpace(candidate.ID)]
	return managed
}

func (m *zaiModule) Reconfigure(_ context.Context, host providermodule.HostConfig, providerConfig json.RawMessage) error {
	cfg, err := parsePluginConfig(providerConfig)
	if err != nil {
		return m.runtime.recordError(err)
	}
	cpa := cpaConfigProjection{
		AuthDir:             host.AuthDir,
		ClaudeKeys:          toSDKClaudeKeys(host.ClaudeKeys),
		OpenAICompatibility: toSDKOpenAICompatibility(host.OpenAICompatibility),
	}
	providerKeys := cpaProviderKeys(cpa)
	return m.runtime.reconfigureWithAccounts(cfg, host.AuthDir, cpa, providerKeys)
}

func (m *zaiModule) OwnedAuthIDs(_ context.Context) []string {
	snapshot, err := m.runtime.current()
	if err != nil || snapshot == nil {
		return nil
	}
	authIDs := make([]string, 0, len(snapshot.byAuthID))
	for authID := range snapshot.byAuthID {
		authIDs = append(authIDs, authID)
	}
	return authIDs
}

func (m *zaiModule) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	return m.runtime.pick(req)
}

func (m *zaiModule) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	return m.runtime.handleUsage(record)
}

// UsageAccount names the Z.ai account a usage record belongs to, for the
// coordinator's usage ledger.
func (m *zaiModule) UsageAccount(record pluginapi.UsageRecord) string {
	snapshot, err := m.runtime.current()
	if err != nil || snapshot == nil {
		return ""
	}
	identity := snapshot.byAuthID[strings.TrimSpace(record.AuthID)]
	if identity == "" {
		return ""
	}
	return snapshot.byIdentity[identity].Name
}

func (m *zaiModule) Status(_ context.Context) (json.RawMessage, error) {
	status := "registered"
	if m.runtime.validationStatus() != "" {
		status = "reconfigure_rejected"
	}
	return json.Marshal(m.runtime.managementStatus(status))
}

func (m *zaiModule) Close(_ context.Context) error {
	return m.runtime.shutdown()
}

// ManagementRoutes and HandleManagement implement providermodule.ManagementCapable.
func (m *zaiModule) ManagementRoutes(context.Context) providermodule.ManagementRoutes {
	registration := managementRegistration()
	routes := make([]providermodule.ManagementRoute, 0, len(registration.Routes))
	for _, route := range registration.Routes {
		routes = append(routes, providermodule.ManagementRoute{Method: route.Method, Path: route.Path, Description: route.Description})
	}
	return providermodule.ManagementRoutes{Routes: routes}
}

func (m *zaiModule) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return m.runtime.handleManagement(ctx, req)
}

func toSDKClaudeKeys(keys []providermodule.ClaudeKey) []sdkconfig.ClaudeKey {
	out := make([]sdkconfig.ClaudeKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, sdkconfig.ClaudeKey{
			APIKey:   key.APIKey,
			BaseURL:  key.BaseURL,
			ProxyURL: key.ProxyURL,
			Prefix:   key.Prefix,
			Headers:  key.Headers,
		})
	}
	return out
}

func toSDKOpenAICompatibility(compats []providermodule.OpenAICompatibility) []sdkconfig.OpenAICompatibility {
	out := make([]sdkconfig.OpenAICompatibility, 0, len(compats))
	for _, compat := range compats {
		entries := make([]sdkconfig.OpenAICompatibilityAPIKey, 0, len(compat.APIKeyEntries))
		for _, entry := range compat.APIKeyEntries {
			entries = append(entries, sdkconfig.OpenAICompatibilityAPIKey{
				APIKey:   entry.APIKey,
				ProxyURL: entry.ProxyURL,
			})
		}
		out = append(out, sdkconfig.OpenAICompatibility{
			Name:          compat.Name,
			BaseURL:       compat.BaseURL,
			Disabled:      compat.Disabled,
			Headers:       compat.Headers,
			APIKeyEntries: entries,
		})
	}
	return out
}
