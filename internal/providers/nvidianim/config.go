// Package nvidianim protects NVIDIA-hosted NIM developer API allowances.
// NVIDIA publishes free prototyping access, not a universal monthly credit
// grant or RPM. Account/model limits must be verified in the API Catalog.
package nvidianim

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const providerID = "nvidia-nim"

// moduleConfig deliberately has no invented credit balance or monthly reset.
// FreeRequestLimit is an optional, durable, LOCAL request ceiling, not a
// conversion between NVIDIA credits, tokens or requests. Zero means no local
// lifetime ceiling; the account's verified free access and RPM are still required.
type moduleConfig struct {
	Enabled           bool
	StayWithinFree    bool
	FreeTierConfirmed bool
	RequestsPerMinute int
	FreeRequestLimit  int64
	TrialExpiresAt    time.Time
	FreeModels        map[string]bool
}

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := moduleConfig{Enabled: true, StayWithinFree: true, FreeModels: map[string]bool{}}
	if strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	var input struct {
		Enabled           *bool    `json:"enabled"`
		StayWithinFree    *bool    `json:"stayWithinFree"`
		StayWithinFreeAlt *bool    `json:"stay-within-free"`
		FreeTierConfirmed bool     `json:"free-tier-confirmed"`
		RequestsPerMinute int      `json:"requests-per-minute"`
		FreeRequestLimit  int64    `json:"free-request-limit"`
		TrialExpiresAt    string   `json:"trial-expires-at"`
		FreeModels        []string `json:"free-models"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: invalid provider settings")
	}
	if d.Decode(new(any)) != io.EOF {
		return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: expected one settings object")
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.StayWithinFree != nil && input.StayWithinFreeAlt != nil {
		return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: use only one stayWithinFree spelling")
	}
	if input.StayWithinFree != nil {
		cfg.StayWithinFree = *input.StayWithinFree
	}
	if input.StayWithinFreeAlt != nil {
		cfg.StayWithinFree = *input.StayWithinFreeAlt
	}
	if input.RequestsPerMinute < 0 || input.RequestsPerMinute > 100000 || input.FreeRequestLimit < 0 {
		return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: request limits must be nonnegative; RPM at most 100000")
	}
	cfg.FreeTierConfirmed, cfg.RequestsPerMinute, cfg.FreeRequestLimit = input.FreeTierConfirmed, input.RequestsPerMinute, input.FreeRequestLimit
	if input.TrialExpiresAt != "" {
		var err error
		cfg.TrialExpiresAt, err = time.Parse(time.RFC3339, input.TrialExpiresAt)
		if err != nil {
			return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: trial-expires-at must be RFC3339")
		}
	}
	for _, name := range input.FreeModels {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, "\r\n\t ") {
			return moduleConfig{}, fmt.Errorf("nvidia_nim_invalid_config: free-models must contain exact model IDs")
		}
		cfg.FreeModels[name] = true
	}
	return cfg, nil
}
