package zai

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var sessionHeaderNames = []string{
	"X-Session-ID",
	"Session-Id",
	"Session_id",
	"X-Client-Request-Id",
}

type routingState struct {
	cursors map[string]uint64
}

func (state *routingState) initialize() {
	if state.cursors == nil {
		state.cursors = make(map[string]uint64)
	}
}

func (state *routingState) next(scope string, candidates []pluginapi.SchedulerAuthCandidate) string {
	const maxRoutingScopes = 1024
	if _, exists := state.cursors[scope]; !exists && len(state.cursors) >= maxRoutingScopes {
		clear(state.cursors)
	}
	cursor := state.cursors[scope]
	selected := candidates[int(cursor%uint64(len(candidates)))].ID
	state.cursors[scope] = cursor + 1
	return selected
}

func (state *routingState) sticky(scope, sessionID string, candidates []pluginapi.SchedulerAuthCandidate) string {
	selected := candidates[0].ID
	highest := stableHash(scope + "\x00" + selected + "\x00" + sessionID)
	for _, candidate := range candidates[1:] {
		score := stableHash(scope + "\x00" + candidate.ID + "\x00" + sessionID)
		if score > highest {
			selected = candidate.ID
			highest = score
		}
	}
	return selected
}

func validateSchedulerDeployment(plugins cpaPluginsProjection) error {
	if !plugins.Enabled {
		return fmt.Errorf("plugins must be enabled")
	}
	item, exists := plugins.Configs[pluginID]
	if !exists || item.Enabled == nil || !*item.Enabled {
		return fmt.Errorf("%s must be enabled", pluginID)
	}
	if item.Priority != requiredPluginPriority {
		return fmt.Errorf("%s priority must be %d", pluginID, requiredPluginPriority)
	}
	for id, configured := range plugins.Configs {
		if id == pluginID || configured.Enabled == nil || !*configured.Enabled {
			continue
		}
		return fmt.Errorf("%s must be the sole enabled scheduler plugin; disable %s", pluginID, id)
	}
	return nil
}

func (r *pluginRuntime) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	now := r.runtimeNow()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_not_configured", "Z.ai scheduler is not configured")
	}

	managedCount := 0
	impairedCount := 0
	for _, candidate := range req.Candidates {
		identity := r.snapshot.byAuthID[strings.TrimSpace(candidate.ID)]
		if identity == "" {
			if recognizedZAICandidate(candidate) {
				return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_unmanaged_candidate", "recognized Z.ai credential is absent from the validated account snapshot")
			}
			continue
		}
		managedCount++
		if r.refreshCapacityLocked(identity, now).assess(r.snapshot.byIdentity[identity], now).Status != healthHealthy {
			impairedCount++
		}
	}
	if managedCount == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	// Built-in round-robin would choose among foreign candidates too, so a
	// mixed set takes the explicit path below.
	if impairedCount == 0 && managedCount == len(req.Candidates) {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}, nil
	}

	healthy := make([]pluginapi.SchedulerAuthCandidate, 0, managedCount-impairedCount)
	var throttledUntil time.Time
	for _, candidate := range req.Candidates {
		identity := r.snapshot.byAuthID[strings.TrimSpace(candidate.ID)]
		if identity == "" {
			continue
		}
		switch health := r.snapshot.Health[identity].assess(r.snapshot.byIdentity[identity], now); health.Status {
		case healthHealthy:
			healthy = append(healthy, candidate)
		case healthThrottled:
			if throttledUntil.IsZero() || health.ResetAt.Before(throttledUntil) {
				throttledUntil = health.ResetAt
			}
		}
	}
	if len(healthy) == 0 && !throttledUntil.IsZero() {
		return pluginapi.SchedulerPickResponse{}, newRateLimitedError(throttledUntil.Sub(now))
	}
	if len(healthy) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_no_capacity", "no healthy managed Z.ai capacity remains")
	}

	scope := schedulerScope(req)
	if sessionID := schedulerSessionID(req.Options.Headers); sessionID != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: r.snapshot.Routing.sticky(scope, sessionID, healthy)}, nil
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: r.snapshot.Routing.next(scope, healthy)}, nil
}

func recognizedZAICandidate(candidate pluginapi.SchedulerAuthCandidate) bool {
	baseURL := candidate.Attributes["base_url"]
	if _, recognized := recognizedBaseURL(baseURL, zaiAnthropicBaseURL); recognized {
		return true
	}
	if _, recognized := recognizedBaseURL(baseURL, zaiOpenAIBaseURL); recognized {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(candidate.Attributes["compat_name"]), zaiCompatName) ||
		isZAICompatibilityProvider(candidate.Provider) ||
		isZAICompatibilityProvider(candidate.Attributes["provider_key"])
}

func isZAICompatibilityProvider(raw string) bool {
	provider := strings.ToLower(strings.TrimSpace(raw))
	return provider == zaiCompatName || provider == "openai-compatible-"+zaiCompatName
}

func schedulerScope(req pluginapi.SchedulerPickRequest) string {
	providers := append([]string{strings.ToLower(strings.TrimSpace(req.Provider))}, req.Providers...)
	for i := range providers {
		providers[i] = strings.ToLower(strings.TrimSpace(providers[i]))
	}
	return strings.Join(providers, ",") + "\x00" + strings.ToLower(strings.TrimSpace(req.Model))
}

func schedulerSessionID(headers map[string][]string) string {
	for _, name := range sessionHeaderNames {
		for key, values := range headers {
			if !strings.EqualFold(strings.TrimSpace(key), name) {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func stableHash(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return hash.Sum64()
}
