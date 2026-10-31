package coordinator

import (
	"strings"
	"testing"
)

func enabledPlugin(priority int) pluginProjection {
	on := true
	return pluginProjection{Enabled: &on, Priority: priority}
}

func disabledPlugin(priority int) pluginProjection {
	off := false
	return pluginProjection{Enabled: &off, Priority: priority}
}

// Provider-only plugins (no Scheduler capability) that the operator lists in
// coexisting-plugins must coexist when they cannot sort ahead of this plugin.
// Regression for a deployment where enabling an unrelated provider plugin made
// reconfigure fail with "must be the sole enabled scheduler plugin" and
// dropped every pool account.
func TestValidateSchedulerDeploymentAllowsConfirmedNonOutrankingPlugins(t *testing.T) {
	plugins := pluginsProjection{Enabled: true, Configs: map[string]pluginProjection{
		PluginID:                enabledPlugin(requiredPluginPriority),
		"synthetic":             enabledPlugin(0),
		"nvidia-nim":            enabledPlugin(0),
		"cloudflare-workers-ai": enabledPlugin(requiredPluginPriority - 1),
		"zzz-tie":               enabledPlugin(requiredPluginPriority),
		"disabled-high":         disabledPlugin(requiredPluginPriority + 50),
		"unset-enabled":         {Priority: requiredPluginPriority + 50},
	}}
	coexisting := []string{"synthetic", "nvidia-nim", " cloudflare-workers-ai ", "zzz-tie"}
	if err := validateSchedulerDeployment(plugins, coexisting); err != nil {
		t.Fatalf("validateSchedulerDeployment() = %v, want confirmed provider plugins accepted", err)
	}
}

// An unlisted enabled plugin may register a Scheduler the host would silently
// ignore (lower priority) or consult first (higher priority): both fail closed.
func TestValidateSchedulerDeploymentRejectsUnconfirmedPlugins(t *testing.T) {
	for name, priority := range map[string]int{"lower competitor": 1, "default priority": 0, "higher competitor": 2000, "equal priority": requiredPluginPriority} {
		t.Run(name, func(t *testing.T) {
			plugins := pluginsProjection{Enabled: true, Configs: map[string]pluginProjection{
				PluginID:     enabledPlugin(requiredPluginPriority),
				"competitor": enabledPlugin(priority),
			}}
			err := validateSchedulerDeployment(plugins, []string{"something-else"})
			if err == nil || !strings.Contains(err.Error(), "sole enabled scheduler") || !strings.Contains(err.Error(), "competitor") {
				t.Fatalf("validateSchedulerDeployment() = %v, want fail-closed rejection naming competitor", err)
			}
		})
	}
}

// Even a confirmed plugin must not sort ahead of this plugin in host order.
func TestValidateSchedulerDeploymentRejectsConfirmedPluginsThatOutrank(t *testing.T) {
	for name, tc := range map[string]struct {
		id       string
		priority int
	}{
		"higher priority":     {"zzz-high", requiredPluginPriority + 1},
		"tie with smaller ID": {"aaa-tie", requiredPluginPriority},
	} {
		t.Run(name, func(t *testing.T) {
			plugins := pluginsProjection{Enabled: true, Configs: map[string]pluginProjection{
				PluginID: enabledPlugin(requiredPluginPriority),
				tc.id:    enabledPlugin(tc.priority),
			}}
			err := validateSchedulerDeployment(plugins, []string{tc.id})
			if err == nil || !strings.Contains(err.Error(), "sort ahead") {
				t.Fatalf("validateSchedulerDeployment() = %v, want ordering rejection for %s", err, tc.id)
			}
		})
	}
}

func TestValidateSchedulerDeploymentKeepsOwnRequirements(t *testing.T) {
	for name, plugins := range map[string]pluginsProjection{
		"plugins disabled": {Enabled: false, Configs: map[string]pluginProjection{PluginID: enabledPlugin(requiredPluginPriority)}},
		"self missing":     {Enabled: true, Configs: map[string]pluginProjection{}},
		"self disabled":    {Enabled: true, Configs: map[string]pluginProjection{PluginID: disabledPlugin(requiredPluginPriority)}},
		"wrong priority":   {Enabled: true, Configs: map[string]pluginProjection{PluginID: enabledPlugin(10)}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSchedulerDeployment(plugins, nil); err == nil {
				t.Fatal("validateSchedulerDeployment() = nil, want error")
			}
		})
	}
}

func TestParseCoordinatorConfigAcceptsCoexistingPlugins(t *testing.T) {
	cfg, err := parseCoordinatorConfig([]byte("cpa-config-path: /tmp/config.yaml\ncoexisting-plugins: [nvidia-nim, cloudflare-workers-ai]\nenabled: true\npriority: 1000\n"))
	if err != nil || strings.Join(cfg.CoexistingPlugins, ",") != "nvidia-nim,cloudflare-workers-ai" {
		t.Fatalf("parseCoordinatorConfig() = %+v, %v", cfg.CoexistingPlugins, err)
	}
}
