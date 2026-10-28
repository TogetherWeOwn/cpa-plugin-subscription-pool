// Package cloudflare implements account-wide Workers AI daily neuron guards.
// Pricing: https://developers.cloudflare.com/workers-ai/platform/pricing/
package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	providerID              = "cloudflare-workers-ai"
	includedDailyNeurons    = 10000.0
	pricePerThousandNeurons = 0.011
)

type moduleConfig struct {
	Enabled            bool
	StayWithinIncluded bool
	SafetyMargin       float64
	Refresh            time.Duration
	MaxAge             time.Duration
	Providers          []string
}

type rawConfig struct {
	Enabled            *bool           `json:"enabled"`
	StayWithinIncluded json.RawMessage `json:"stayWithinIncluded"`
	SafetyMargin       *float64        `json:"safety-margin-neurons"`
	Refresh            *string         `json:"usage-refresh-interval"`
	MaxAge             *string         `json:"usage-max-age"`
	Providers          []string        `json:"providers"`
}

var providerNameRE = regexp.MustCompile(`^cloudflare-workers-ai(?:-[a-z0-9]+)*$`)
var accountRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := moduleConfig{Enabled: true, StayWithinIncluded: true, SafetyMargin: 500, Refresh: 30 * time.Second, MaxAge: 2 * time.Minute, Providers: []string{providerID}}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in rawConfig
	if err := dec.Decode(&in); err != nil {
		return cfg, fmt.Errorf("cloudflare_invalid_config: invalid provider config")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("cloudflare_invalid_config: trailing JSON")
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if len(in.StayWithinIncluded) > 0 {
		if string(in.StayWithinIncluded) == "null" || json.Unmarshal(in.StayWithinIncluded, &cfg.StayWithinIncluded) != nil {
			return cfg, fmt.Errorf("cloudflare_invalid_config: stayWithinIncluded must be boolean")
		}
	}
	if in.SafetyMargin != nil {
		cfg.SafetyMargin = *in.SafetyMargin
	}
	if cfg.SafetyMargin < 0 || cfg.SafetyMargin >= includedDailyNeurons {
		return cfg, fmt.Errorf("cloudflare_invalid_config: invalid safety margin")
	}
	for _, v := range []struct {
		raw *string
		dst *time.Duration
	}{{in.Refresh, &cfg.Refresh}, {in.MaxAge, &cfg.MaxAge}} {
		if v.raw == nil {
			continue
		}
		d, err := time.ParseDuration(*v.raw)
		if err != nil {
			return cfg, fmt.Errorf("cloudflare_invalid_config: invalid usage interval")
		}
		*v.dst = d
	}
	if cfg.Refresh < 15*time.Second || cfg.Refresh > 5*time.Minute || cfg.MaxAge < cfg.Refresh || cfg.MaxAge > 10*time.Minute {
		return cfg, fmt.Errorf("cloudflare_invalid_config: usage refresh must be 15s..5m and max age refresh..10m")
	}
	if in.Providers != nil {
		cfg.Providers = in.Providers
	}
	if len(cfg.Providers) == 0 {
		return cfg, fmt.Errorf("cloudflare_invalid_config: providers cannot be empty")
	}
	seen := map[string]bool{}
	for _, name := range cfg.Providers {
		if !providerNameRE.MatchString(name) || seen[name] {
			return cfg, fmt.Errorf("cloudflare_invalid_config: invalid or duplicate provider name")
		}
		seen[name] = true
	}
	return cfg, nil
}

func accountID(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme != "https" || u.Host != "api.cloudflare.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", fmt.Errorf("cloudflare_invalid_config: expected official Workers AI HTTPS base URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "client" || parts[1] != "v4" || parts[2] != "accounts" || !accountRE.MatchString(parts[3]) || parts[4] != "ai" || parts[5] != "v1" {
		return "", fmt.Errorf("cloudflare_invalid_config: expected account-scoped Workers AI base URL")
	}
	if u.Path != "/client/v4/accounts/"+parts[3]+"/ai/v1" && u.Path != "/client/v4/accounts/"+parts[3]+"/ai/v1/" {
		return "", fmt.Errorf("cloudflare_invalid_config: invalid Workers AI path")
	}
	return parts[3], nil
}

func utcDay(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

type monthQuote struct {
	Days               int     `json:"days_in_calendar_month"`
	IncludedNeurons    float64 `json:"included_neurons_across_daily_windows"`
	IncludedValueUSD   float64 `json:"included_value_usd"`
	WorkersPaidBaseUSD float64 `json:"workers_paid_base_usd"`
	PricePerThousand   float64 `json:"usd_per_thousand_neurons"`
	Rollover           bool    `json:"daily_allocation_rolls_over"`
}

// This is a calendar projection, NOT a spendable monthly bank. Each day's
// first 10,000 neurons is free; never subtract a monthly pool from usage.
func calendarQuote(now time.Time) monthQuote {
	now = now.UTC()
	days := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return monthQuote{Days: days, IncludedNeurons: float64(days) * includedDailyNeurons, IncludedValueUSD: float64(days) * 0.11, WorkersPaidBaseUSD: 5, PricePerThousand: pricePerThousandNeurons}
}
