// Package openrouterfree keeps the host's `openrouter-free` OpenAI-compatible
// provider on exactly OpenRouter's current free models, and never admits a
// request for anything else. OpenRouter publishes the free list (`:free` ids
// with zero prices) in its public catalog and the daily free-request limit
// (50, or normally 1000 after at least $10 in credit purchases) on GET /api/v1/key.
package openrouterfree

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	providerID = "openrouter-free"
	// aliasPrefix namespaces host aliases so a request for one of these
	// models can only ever resolve to this provider.
	aliasPrefix = providerID + "/"

	// OpenRouter documents 20 requests/minute for free models.
	maxRequestsPerMinute = 20
	// Lowest daily free-model limit (accounts without purchased credits);
	// used whenever GET /key has not told us better.
	lowestDailyLimit = 50

	defaultSyncInterval  = 6 * time.Hour
	minSyncInterval      = 10 * time.Minute
	defaultCatalogMaxAge = 24 * time.Hour
	defaultManagementURL = "http://127.0.0.1:8317"
)

// moduleConfig is providers.openrouter-free in the plugin's config block.
type moduleConfig struct {
	Enabled           bool
	StayWithinFree    bool
	RequestsPerMinute int
	// DailyRequestLimit only ever lowers the limit OpenRouter reports.
	DailyRequestLimit int64
	SyncInterval      time.Duration
	CatalogMaxAge     time.Duration
	ManagementURL     string
	ManagementKeyFile string
}

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := moduleConfig{
		Enabled: true, StayWithinFree: true, RequestsPerMinute: maxRequestsPerMinute,
		SyncInterval: defaultSyncInterval, CatalogMaxAge: defaultCatalogMaxAge, ManagementURL: defaultManagementURL,
	}
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		raw = json.RawMessage(`{}`)
	}
	var input struct {
		Enabled           *bool  `json:"enabled"`
		StayWithinFree    *bool  `json:"stayWithinFree"`
		StayWithinFreeAlt *bool  `json:"stay-within-free"`
		RequestsPerMinute *int   `json:"requests-per-minute"`
		DailyRequestLimit int64  `json:"daily-request-limit"`
		SyncInterval      string `json:"sync-interval"`
		CatalogMaxAge     string `json:"catalog-max-age"`
		ManagementURL     string `json:"management-url"`
		ManagementKeyFile string `json:"management-key-file"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: invalid provider settings")
	}
	if d.Decode(new(any)) != io.EOF {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: expected one settings object")
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.StayWithinFree != nil && input.StayWithinFreeAlt != nil {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: use only one stayWithinFree spelling")
	}
	if input.StayWithinFree != nil {
		cfg.StayWithinFree = *input.StayWithinFree
	}
	if input.StayWithinFreeAlt != nil {
		cfg.StayWithinFree = *input.StayWithinFreeAlt
	}
	if input.RequestsPerMinute != nil {
		cfg.RequestsPerMinute = *input.RequestsPerMinute
	}
	if cfg.RequestsPerMinute < 1 || cfg.RequestsPerMinute > maxRequestsPerMinute {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: requests-per-minute must be 1 to %d", maxRequestsPerMinute)
	}
	if input.DailyRequestLimit < 0 {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: daily-request-limit must be nonnegative")
	}
	cfg.DailyRequestLimit = input.DailyRequestLimit
	var err error
	if cfg.SyncInterval, err = parseDuration(input.SyncInterval, cfg.SyncInterval); err != nil || cfg.SyncInterval < minSyncInterval {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: sync-interval must be a duration of at least %s", minSyncInterval)
	}
	if cfg.CatalogMaxAge, err = parseDuration(input.CatalogMaxAge, cfg.CatalogMaxAge); err != nil || cfg.CatalogMaxAge < cfg.SyncInterval {
		return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: catalog-max-age must be a duration of at least sync-interval")
	}
	if v := strings.TrimSpace(input.ManagementURL); v != "" {
		if !loopbackURL(v) {
			return moduleConfig{}, fmt.Errorf("openrouter_free_invalid_config: management-url must be a loopback http(s) URL")
		}
		cfg.ManagementURL = strings.TrimRight(v, "/")
	}
	cfg.ManagementKeyFile = strings.TrimSpace(input.ManagementKeyFile)
	return cfg, nil
}

func parseDuration(raw string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	return time.ParseDuration(strings.TrimSpace(raw))
}

// loopbackURL keeps the management key from ever leaving the host.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
