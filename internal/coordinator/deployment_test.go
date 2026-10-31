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

// Provider-only plugins (no Scheduler capability) at the default priority must
// coexist: the host consults only the first Scheduler in priority-desc/ID-asc
// order, so they can never displace this plugin. Regression for a deployment
// where enabling an unrelated provider plugin made reconfigure fail with
// "must be the sole enabled scheduler plugin" and dropped every pool account.
func TestValidateSchedulerDeploymentAllowsNonOutrankingPlugins(t *testing.T) {
	plugins := pluginsProjection{Enabled: true, Configs: map[string]pluginProjection{
		PluginID:                enabledPlugin(requiredPluginPriority),
		"synthetic":             enabledPlugin(0),
		"nvidia-nim":            enabledPlugin(0),
		"cloudflare-workers-ai": enabledPlugin(0),
		"aaa-low":               enabledPlugin(requiredPluginPriority - 1),
		"zzz-tie":               enabledPlugin(requiredPluginPriority),
		"disabled-high":         disabledPlugin(requiredPluginPriority + 50),
		"unset-enabled":         {Priority: requiredPluginPriority + 50},
	}}
	if err := validateSchedulerDeployment(plugins); err != nil {
		t.Fatalf("validateSchedulerDeployment() = %v, want provider plugins that cannot outrank %s accepted", err, PluginID)
	}
}

func TestValidateSchedulerDeploymentRejectsPluginsConsultedFirst(t *testing.T) {
	for name, tc := range map[string]struct {
		id       string
		priority int
	}{
		"higher priority":      {"zzz-high", requiredPluginPriority + 1},
		"tie with smaller ID":  {"aaa-tie", requiredPluginPriority},
		"much higher priority": {"synthetic", 5000},
	} {
		t.Run(name, func(t *testing.T) {
			plugins := pluginsProjection{Enabled: true, Configs: map[string]pluginProjection{
				PluginID: enabledPlugin(requiredPluginPriority),
				tc.id:    enabledPlugin(tc.priority),
			}}
			err := validateSchedulerDeployment(plugins)
			if err == nil || !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("validateSchedulerDeployment() = %v, want rejection naming %s", err, tc.id)
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
			if err := validateSchedulerDeployment(plugins); err == nil {
				t.Fatal("validateSchedulerDeployment() = nil, want error")
			}
		})
	}
}
