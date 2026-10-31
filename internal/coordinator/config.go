package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"gopkg.in/yaml.v3"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const defaultCPAConfigPath = "config.yaml"

// coordinatorConfig is the coordinator's own YAML shape: everything under
// plugins.configs.subscription-pool. The host always re-injects its own
// enabled/priority scalars into this raw node before handing it to the
// plugin (pluginhost's normalizedConfigNode/ensureMappingScalar, present in
// both v7.2.150 and v7.2.157) rather than stripping them, so KnownFields(true)
// must declare and ignore them here or every real reconfigure call fails.
type coordinatorConfig struct {
	CPAConfigPath string `yaml:"cpa-config-path"`
	// CoexistingPlugins lists plugin IDs the operator confirms register NO
	// Scheduler capability (provider-only plugins). Only these may be enabled
	// alongside this plugin; see validateSchedulerDeployment.
	CoexistingPlugins []string             `yaml:"coexisting-plugins"`
	Providers         map[string]yaml.Node `yaml:"providers"`
	Enabled           *bool                `yaml:"enabled"`
	Priority          *int                 `yaml:"priority"`
}

func parseCoordinatorConfig(raw []byte) (coordinatorConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg coordinatorConfig
	if err := decoder.Decode(&cfg); err != nil {
		return coordinatorConfig{}, fmt.Errorf("decode coordinator config: %w", err)
	}
	if strings.TrimSpace(cfg.CPAConfigPath) == "" {
		cfg.CPAConfigPath = defaultCPAConfigPath
	}
	return cfg, nil
}

// providerConfigRaw re-marshals the providers.<id> YAML node back into JSON
// so a module's Reconfigure can decode it with its own, independent shape.
// A provider absent from the config re-marshals its zero yaml.Node, which
// re-marshals as JSON null; modules must treat that the same as "{}".
func providerConfigRaw(cfg coordinatorConfig, providerID string) ([]byte, error) {
	node, ok := cfg.Providers[providerID]
	if !ok {
		return []byte("{}"), nil
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode providers.%s: %w", providerID, err)
	}
	if value == nil {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("re-encode providers.%s: %w", providerID, err)
	}
	return raw, nil
}

func resolveCPAConfigPath(pluginConfigPath string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(pluginConfigPath))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("cpa-config-path is required")
	}
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	absolute, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("resolve cpa-config-path: %w", err)
	}
	return absolute, nil
}

func resolveAuthDir(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = "~/.cli-proxy-api"
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve auth-dir: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	absolute, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", fmt.Errorf("resolve auth-dir: %w", err)
	}
	return absolute, nil
}

// hostCPAConfig is the coordinator's own projection of the fields it (not
// any individual module) needs from the host CLIProxyAPI config: the raw
// plugins block for the scheduler-slot-exclusivity check, plus the account
// discovery inputs every module's HostConfig gets a copy of.
type hostCPAConfig struct {
	AuthDir             string
	ClaudeKeys          []providermodule.ClaudeKey
	OpenAICompatibility []providermodule.OpenAICompatibility
	Plugins             pluginsProjection
}

type pluginsProjection struct {
	Enabled bool
	Configs map[string]pluginProjection
}

type pluginProjection struct {
	Enabled  *bool
	Priority int
}

func loadHostCPAConfig(path string) (hostCPAConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return hostCPAConfig{}, fmt.Errorf("read CPA config: %w", err)
	}
	cfg, err := sdkconfig.ParseConfigBytes(data)
	if err != nil {
		return hostCPAConfig{}, fmt.Errorf("load CPA config: invalid configuration")
	}
	plugins := pluginsProjection{Enabled: cfg.Plugins.Enabled, Configs: make(map[string]pluginProjection, len(cfg.Plugins.Configs))}
	for id, item := range cfg.Plugins.Configs {
		plugins.Configs[id] = pluginProjection{Enabled: item.Enabled, Priority: item.Priority}
	}
	claudeKeys := make([]providermodule.ClaudeKey, 0, len(cfg.ClaudeKey))
	for _, key := range cfg.ClaudeKey {
		claudeKeys = append(claudeKeys, providermodule.ClaudeKey{
			APIKey:   key.APIKey,
			BaseURL:  key.BaseURL,
			ProxyURL: key.ProxyURL,
			Prefix:   key.Prefix,
			Headers:  key.Headers,
		})
	}
	compats := make([]providermodule.OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for _, compat := range cfg.OpenAICompatibility {
		entries := make([]providermodule.OpenAICompatibilityAPIKey, 0, len(compat.APIKeyEntries))
		for _, entry := range compat.APIKeyEntries {
			entries = append(entries, providermodule.OpenAICompatibilityAPIKey{
				APIKey:   entry.APIKey,
				ProxyURL: entry.ProxyURL,
			})
		}
		models := make([]providermodule.OpenAICompatibilityModel, 0, len(compat.Models))
		for _, model := range compat.Models {
			models = append(models, providermodule.OpenAICompatibilityModel{Name: model.Name, Alias: model.Alias})
		}
		compats = append(compats, providermodule.OpenAICompatibility{
			Name:          compat.Name,
			BaseURL:       compat.BaseURL,
			Disabled:      compat.Disabled,
			Headers:       compat.Headers,
			APIKeyEntries: entries,
			Models:        models,
		})
	}
	return hostCPAConfig{
		AuthDir:             cfg.AuthDir,
		ClaudeKeys:          claudeKeys,
		OpenAICompatibility: compats,
		Plugins:             plugins,
	}, nil
}

// validateSchedulerDeployment enforces the host-level rule that this plugin
// must be the sole enabled scheduler: CLIProxyAPI's scheduler slot runs only
// the first active plugin that registers a Scheduler, ordered by priority
// desc, then plugin ID asc, and silently ignores the rest (host
// internal/pluginhost/snapshot.go sortRecords, scheduler.go schedulerRecord),
// so a second scheduler would blend or lose quota domains without any error.
//
// Capabilities are not visible from configuration, so another enabled plugin
// is accepted only when the operator explicitly confirms it registers no
// Scheduler (listed in coexisting-plugins) AND it cannot sort ahead of this
// plugin. Every unlisted enabled plugin still fails closed.
func validateSchedulerDeployment(plugins pluginsProjection, coexisting []string) error {
	if !plugins.Enabled {
		return fmt.Errorf("plugins must be enabled")
	}
	item, exists := plugins.Configs[PluginID]
	if !exists || item.Enabled == nil || !*item.Enabled {
		return fmt.Errorf("%s must be enabled", PluginID)
	}
	if item.Priority != requiredPluginPriority {
		return fmt.Errorf("%s priority must be %d", PluginID, requiredPluginPriority)
	}
	confirmed := make(map[string]bool, len(coexisting))
	for _, id := range coexisting {
		confirmed[strings.TrimSpace(id)] = true
	}
	ids := make([]string, 0, len(plugins.Configs))
	for id := range plugins.Configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		configured := plugins.Configs[id]
		if id == PluginID || configured.Enabled == nil || !*configured.Enabled {
			continue
		}
		if !confirmed[id] {
			return fmt.Errorf("%s must be the sole enabled scheduler plugin; disable %s, or list it in coexisting-plugins if it registers no Scheduler", PluginID, id)
		}
		if outranksInHostOrder(id, configured.Priority, PluginID, item.Priority) {
			return fmt.Errorf("%s must be the first plugin in host order; coexisting plugin %s (priority %d) would sort ahead of it: give it a priority below %d", PluginID, id, configured.Priority, requiredPluginPriority)
		}
	}
	return nil
}

// outranksInHostOrder mirrors the host's record ordering: higher priority
// first, ties broken by ascending plugin ID.
func outranksInHostOrder(id string, priority int, selfID string, selfPriority int) bool {
	if priority != selfPriority {
		return priority > selfPriority
	}
	return id < selfID
}
