package synthetic

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	defaultThresholdPercent  = 95
	defaultQuotaRefresh      = 2 * time.Minute
	minQuotaRefresh          = 30 * time.Second
	defaultFailureCooldown   = 10 * time.Minute
	defaultInfiniteCooldown  = 30 * time.Second
	defaultInFlightTTL       = 2 * time.Hour
	defaultPaidProvider      = "synthetic"
	defaultInfiniteProvider  = "synthetic-infinite"
	quotaPath                = "/v2/quotas"
	maxConfiguredConcurrency = 1024
)

// moduleConfig is the validated providers.synthetic block.
type moduleConfig struct {
	Enabled bool
	// ThresholdPercent is the share of the weekly credit limit (0-100) at or
	// above which the paid credential stops receiving new requests. Synthetic
	// regenerates 2% of the weekly limit every 202 minutes and there is no
	// lockout, so the default keeps the bar just under full instead of
	// letting requests hit the limit (and, if extra usage is enabled on the
	// account, spill into usage-based billing).
	ThresholdPercent int
	// QuotaRefresh is how often GET /v2/quotas is polled per account. The
	// endpoint does not count against the subscription. 0 disables polling;
	// the module then relies on the x-synthetic-quotas response header.
	QuotaRefresh time.Duration
	// QuotaURL overrides the quota endpoint. Empty derives it from each
	// provider's base-url origin plus /v2/quotas.
	QuotaURL string
	// FailureCooldown applies to a paid 429/402 that carries no
	// regeneration time.
	FailureCooldown time.Duration
	// InfiniteCooldown applies to an infinite-mode 429 (a busy signal, not
	// a quota: infinite mode never counts against the plan).
	InfiniteCooldown time.Duration
	// InFlightTTL expires an in-flight slot whose usage record never
	// arrived, so a lost record cannot wedge a concurrency-capped lane.
	InFlightTTL time.Duration
	// MaxConcurrent caps simultaneous requests per paid credential (0 = no
	// local cap). InfiniteMaxConcurrent does the same for infinite mode.
	MaxConcurrent         int
	InfiniteMaxConcurrent int
	PaidProviders         []string
	InfiniteProviders     []string
}

type rawModuleConfig struct {
	Enabled               *bool    `json:"enabled"`
	ThresholdPercent      *int     `json:"threshold-percent"`
	QuotaRefreshInterval  *string  `json:"quota-refresh-interval"`
	QuotaURL              string   `json:"quota-url"`
	FailureCooldown       *string  `json:"failure-cooldown"`
	InfiniteCooldown      *string  `json:"infinite-cooldown"`
	InFlightTTL           *string  `json:"in-flight-ttl"`
	MaxConcurrent         *int     `json:"max-concurrent"`
	InfiniteMaxConcurrent *int     `json:"infinite-max-concurrent"`
	PaidProviders         []string `json:"paid-providers"`
	InfiniteProviders     []string `json:"infinite-providers"`
}

func defaultConfig() moduleConfig {
	return moduleConfig{
		Enabled:           true,
		ThresholdPercent:  defaultThresholdPercent,
		QuotaRefresh:      defaultQuotaRefresh,
		FailureCooldown:   defaultFailureCooldown,
		InfiniteCooldown:  defaultInfiniteCooldown,
		InFlightTTL:       defaultInFlightTTL,
		PaidProviders:     []string{defaultPaidProvider},
		InfiniteProviders: []string{defaultInfiniteProvider},
	}
}

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := defaultConfig()
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return cfg, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var input rawModuleConfig
	if err := decoder.Decode(&input); err != nil {
		return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: decode provider config: %w", err)
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.ThresholdPercent != nil {
		cfg.ThresholdPercent = *input.ThresholdPercent
	}
	if cfg.ThresholdPercent < 1 || cfg.ThresholdPercent > 100 {
		return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: threshold-percent must be between 1 and 100")
	}
	var err error
	if cfg.QuotaRefresh, err = durationField("quota-refresh-interval", input.QuotaRefreshInterval, cfg.QuotaRefresh, true); err != nil {
		return moduleConfig{}, err
	}
	if cfg.QuotaRefresh > 0 && cfg.QuotaRefresh < minQuotaRefresh {
		return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: quota-refresh-interval must be 0 (off) or at least %s", minQuotaRefresh)
	}
	if cfg.FailureCooldown, err = durationField("failure-cooldown", input.FailureCooldown, cfg.FailureCooldown, false); err != nil {
		return moduleConfig{}, err
	}
	if cfg.InfiniteCooldown, err = durationField("infinite-cooldown", input.InfiniteCooldown, cfg.InfiniteCooldown, false); err != nil {
		return moduleConfig{}, err
	}
	if cfg.InFlightTTL, err = durationField("in-flight-ttl", input.InFlightTTL, cfg.InFlightTTL, false); err != nil {
		return moduleConfig{}, err
	}
	if input.QuotaURL != "" {
		parsed, errParse := url.Parse(strings.TrimSpace(input.QuotaURL))
		if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: quota-url must be an absolute http(s) URL")
		}
		cfg.QuotaURL = parsed.String()
	}
	if input.MaxConcurrent != nil {
		cfg.MaxConcurrent = *input.MaxConcurrent
	}
	if input.InfiniteMaxConcurrent != nil {
		cfg.InfiniteMaxConcurrent = *input.InfiniteMaxConcurrent
	}
	for name, value := range map[string]int{"max-concurrent": cfg.MaxConcurrent, "infinite-max-concurrent": cfg.InfiniteMaxConcurrent} {
		if value < 0 || value > maxConfiguredConcurrency {
			return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: %s must be between 0 (no cap) and %d", name, maxConfiguredConcurrency)
		}
	}
	if input.PaidProviders != nil {
		cfg.PaidProviders = normalizeNames(input.PaidProviders)
	}
	if input.InfiniteProviders != nil {
		cfg.InfiniteProviders = normalizeNames(input.InfiniteProviders)
	}
	if len(cfg.PaidProviders) == 0 && len(cfg.InfiniteProviders) == 0 {
		return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: at least one paid or infinite provider name is required")
	}
	for _, paid := range cfg.PaidProviders {
		for _, infinite := range cfg.InfiniteProviders {
			if paid == infinite {
				return moduleConfig{}, fmt.Errorf("synthetic_invalid_config: provider %q cannot be both paid and infinite", paid)
			}
		}
	}
	return cfg, nil
}

func durationField(name string, raw *string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	if raw == nil {
		return fallback, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "0" && allowZero {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("synthetic_invalid_config: %s must be a Go duration such as 2m or 30s", name)
	}
	if parsed < 0 || (parsed == 0 && !allowZero) {
		return 0, fmt.Errorf("synthetic_invalid_config: %s must be positive", name)
	}
	return parsed, nil
}

func normalizeNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// quotaURLFor returns the quota endpoint for a provider base URL: the
// configured override, else the base URL's origin plus /v2/quotas
// (https://api.synthetic.new/openai/v1 -> https://api.synthetic.new/v2/quotas).
func quotaURLFor(cfg moduleConfig, baseURL string) string {
	if cfg.QuotaURL != "" {
		return cfg.QuotaURL
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + quotaPath
}
