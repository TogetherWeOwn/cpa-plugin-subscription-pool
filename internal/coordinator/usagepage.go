package coordinator

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/usage"
)

// The usage page is one self-contained HTML document (styles and script
// inline, no external assets) served on the plugin's resource route. It
// reads its data from usage.json on the same origin and also receives the
// first payload inline, so it renders without a round trip.
//
//go:embed ui/usage.html
var usagePageTemplate string

const (
	resourceUsageDataPath   = "/usage.json"
	managementUsagePath     = "/v0/management/plugins/" + PluginID + "/usage"
	usagePageMenu           = "Usage and Quota"
	usagePageInitialMarker  = "__SP_INITIAL_DATA__"
	usagePageNonceMarker    = "__SP_NONCE__"
	syntheticRegenInterval  = 202 * time.Minute
	syntheticDefaultRegenPc = 2.0
)

var providerLabels = map[string]string{
	"zai":         "Z.ai",
	"opencode-go": "OpenCode Go",
	"synthetic":   "synthetic.new",
}

func providerLabel(id string) string {
	if label := providerLabels[id]; label != "" {
		return label
	}
	return id
}

// usagePageData is the redacted payload of the usage page. It is served on
// an unauthenticated resource route, so it carries provider IDs, account
// display names, model names, quota percentages, reset times, counters and
// closed-vocabulary codes only: never a key, key fragment, fingerprint, auth
// ID, request content, or free text (upstream error bodies, health reasons,
// validation errors). Free text stays on the authenticated status route.
type usagePageData struct {
	Plugin           string          `json:"plugin"`
	Version          string          `json:"version"`
	Status           string          `json:"status"`
	ValidationFailed bool            `json:"validation_failed,omitempty"`
	GeneratedAt      time.Time       `json:"generated_at"`
	Ranges           []string        `json:"ranges"`
	Providers        []usageProvider `json:"providers"`
	Quota            []quotaCard     `json:"quota"`
	Usage            usage.Report    `json:"usage"`
	Synthetic        *syntheticPanel `json:"synthetic,omitempty"`
	Ledger           ledgerInfo      `json:"ledger"`
}

type usageProvider struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Accounts int    `json:"accounts"`
}

type quotaCard struct {
	Provider      string        `json:"provider"`
	ProviderLabel string        `json:"provider_label"`
	Account       string        `json:"account"`
	Kind          string        `json:"kind,omitempty"`
	Plan          string        `json:"plan,omitempty"`
	Health        string        `json:"health"`
	Reason        string        `json:"reason,omitempty"`
	Windows       []quotaWindow `json:"windows"`
	Metrics       []quotaMetric `json:"metrics,omitempty"`
}

// quotaWindow is one quota bar. Used/remaining are percentages (0-100).
// Mode "reset" windows restart at ResetsAt; mode "regen" windows (the
// synthetic.new weekly credit) refill RegenPercent every RegenSeconds from
// ResetsAt (the next tick) until FullAt; mode "free" is not metered.
type quotaWindow struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	Mode             string     `json:"mode"`
	Known            bool       `json:"known"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	StartsAt         *time.Time `json:"starts_at,omitempty"`
	WindowSeconds    int64      `json:"window_seconds,omitempty"`
	RegenPercent     *float64   `json:"regen_percent,omitempty"`
	RegenSeconds     int64      `json:"regen_seconds,omitempty"`
	FullAt           *time.Time `json:"full_at,omitempty"`
	ThresholdPercent int        `json:"threshold_percent,omitempty"`
	Source           string     `json:"source,omitempty"`
	Authoritative    bool       `json:"authoritative,omitempty"`
	Stale            bool       `json:"stale,omitempty"`
	Exhausted        bool       `json:"exhausted,omitempty"`
}

type quotaMetric struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

type syntheticPanel struct {
	Status               string             `json:"status"`
	ThresholdPercent     int                `json:"threshold_percent"`
	RegenIntervalMinutes int                `json:"regen_interval_minutes"`
	Lanes                []syntheticLane    `json:"lanes"`
	Routing              syntheticRouting   `json:"routing"`
	Schedule             []syntheticRegenAt `json:"regen_schedule,omitempty"`
}

type syntheticLane struct {
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Health         string     `json:"health"`
	Reason         string     `json:"reason,omitempty"`
	Learned        bool       `json:"learned,omitempty"`
	InFlight       int        `json:"in_flight"`
	PeakInFlight   int        `json:"peak_in_flight"`
	PeakInFlightAt *time.Time `json:"peak_in_flight_at,omitempty"`
	MaxConcurrent  int        `json:"max_concurrent"`
	Requests       int64      `json:"requests"`
	Failures       int64      `json:"failures"`
	OutputTokens   int64      `json:"output_tokens"`
	TPSSamples     int        `json:"tps_samples"`
	MedianTPS      *float64   `json:"median_output_tps,omitempty"`
	MeanTPS        *float64   `json:"mean_output_tps,omitempty"`
	MeanTTFTMillis *float64   `json:"mean_ttft_ms,omitempty"`
	LastStatus     int        `json:"last_status,omitempty"`
}

// syntheticRoutingStatus is the module's routing block as the authenticated
// status reports it (with the free-text fallback reason).
type syntheticRoutingStatus struct {
	PaidPicks          int64      `json:"paid_picks"`
	InfinitePicks      int64      `json:"infinite_picks"`
	Fallbacks          int64      `json:"fallbacks_to_infinite"`
	Rejections         int64      `json:"rejections"`
	InfiniteNonStream  int64      `json:"infinite_non_stream_picks"`
	LastFallbackReason string     `json:"last_fallback_reason"`
	LastFallbackAt     *time.Time `json:"last_fallback_at"`
}

// syntheticRouting is the page's copy: the fallback reason becomes a code.
type syntheticRouting struct {
	PaidPicks          int64      `json:"paid_picks"`
	InfinitePicks      int64      `json:"infinite_picks"`
	Fallbacks          int64      `json:"fallbacks_to_infinite"`
	Rejections         int64      `json:"rejections"`
	InfiniteNonStream  int64      `json:"infinite_non_stream_picks"`
	LastFallbackReason string     `json:"last_fallback_reason,omitempty"`
	LastFallbackAt     *time.Time `json:"last_fallback_at,omitempty"`
}

// reasonCode maps a module's free-text health or fallback reason (which can
// embed an upstream error body) to a closed vocabulary for the page.
func reasonCode(text string) string {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return ""
	case strings.HasPrefix(text, "exhausted"):
		return "quota_exhausted"
	case strings.HasPrefix(text, "weekly credits"):
		return "at_threshold"
	case strings.Contains(text, "concurrency cap"):
		return "concurrency_cap"
	case strings.HasPrefix(text, "busy until"):
		return "resting"
	case strings.HasPrefix(text, "credential not bound"):
		return "unbound_credential"
	case strings.HasPrefix(text, "no API key yet"):
		return "placeholder_key"
	default:
		return "other"
	}
}

// sourceCode maps a quota window's source text to a closed vocabulary.
func sourceCode(raw string, authoritative bool) string {
	raw = strings.TrimSpace(raw)
	switch {
	case authoritative, raw == "quota_api", raw == "authoritative":
		return "authoritative"
	case raw == "estimate":
		return "estimate"
	case strings.HasPrefix(raw, "quota poll"):
		return "quota_poll"
	case strings.HasPrefix(raw, "response header"):
		return "response_header"
	case strings.Contains(raw, "proxy-observed"), raw == "429-derived":
		return "429_derived"
	case strings.Contains(raw, "free"):
		return "free"
	case raw == "", raw == "unknown":
		return "unknown"
	default:
		return "observed"
	}
}

// syntheticRegenAt is one projected regeneration tick of the paid weekly
// credit: At, the percent it adds, and the projected remaining afterwards.
type syntheticRegenAt struct {
	At               time.Time `json:"at"`
	AddsPercent      float64   `json:"adds_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
}

type ledgerInfo struct {
	Persisted     bool   `json:"persisted"`
	PersistError  string `json:"persist_error,omitempty"`
	FineStepSec   int64  `json:"fine_step_seconds"`
	FineKeepSec   int64  `json:"fine_retention_seconds"`
	CoarseStepSec int64  `json:"coarse_step_seconds"`
	CoarseKeepSec int64  `json:"coarse_retention_seconds"`
}

// buildUsagePageData assembles the page payload for rangeID.
func (c *Coordinator) buildUsagePageData(rangeID string) usagePageData {
	status := c.managementStatus()
	now := status.GeneratedAt
	rng := usage.RangeByID(rangeID)
	data := usagePageData{
		Plugin:           PluginID,
		Version:          status.Version,
		Status:           status.Status,
		ValidationFailed: status.ValidationError != "",
		GeneratedAt:      now,
		Providers:        []usageProvider{},
		Quota:            []quotaCard{},
		Usage:            c.ledger.Query(rng, now),
	}
	for _, item := range usage.Ranges {
		data.Ranges = append(data.Ranges, item.ID)
	}
	for _, entry := range c.modules {
		raw := status.Providers[entry.id]
		var cards []quotaCard
		switch entry.id {
		case "zai":
			cards = zaiQuotaCards(raw)
		case "opencode-go":
			cards = opencodeQuotaCards(raw)
		case "synthetic":
			var panel *syntheticPanel
			cards, panel = syntheticQuotaCards(raw, now)
			data.Synthetic = panel
		}
		data.Providers = append(data.Providers, usageProvider{ID: entry.id, Label: providerLabel(entry.id), Status: moduleStatusOf(raw), Accounts: len(cards)})
		data.Quota = append(data.Quota, cards...)
	}
	persisted, persistErr := c.ledgerPersistenceStatus()
	data.Ledger = ledgerInfo{
		Persisted: persisted, PersistError: persistErr,
		FineStepSec: int64(usage.FineStep / time.Second), FineKeepSec: int64(usage.FineRetention / time.Second),
		CoarseStepSec: int64(usage.CoarseStep / time.Second), CoarseKeepSec: int64(usage.CoarseRetention / time.Second),
	}
	return data
}

func moduleStatusOf(raw json.RawMessage) string {
	var head struct {
		Status string `json:"status"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &head) != nil || head.Status == "" {
		return "unknown"
	}
	return head.Status
}

func percentPtr(value float64) *float64 {
	value = math.Round(clampPercent(value)*100) / 100
	return &value
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func resetWindow(id, label string, duration time.Duration, utilization float64, resetsAt *time.Time, source string, authoritative, stale, exhausted bool) quotaWindow {
	used := utilization * 100
	if exhausted && used < 100 {
		used = 100
	}
	window := quotaWindow{
		ID: id, Label: label, Mode: "reset", Known: true,
		UsedPercent: percentPtr(used), RemainingPercent: percentPtr(100 - used),
		WindowSeconds: int64(duration / time.Second), Source: source,
		Authoritative: authoritative, Stale: stale, Exhausted: exhausted,
	}
	if resetsAt != nil && !resetsAt.IsZero() {
		window.ResetsAt = timePtr(*resetsAt)
		window.StartsAt = timePtr(resetsAt.Add(-duration))
	}
	return window
}

func zaiQuotaCards(raw json.RawMessage) []quotaCard {
	var status struct {
		Accounts []struct {
			zaiResourceAccount
			Plan string `json:"plan"`
		} `json:"accounts"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &status) != nil {
		return nil
	}
	cards := make([]quotaCard, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		health := a.Health
		if health == "" {
			health = "unknown"
		}
		authoritative := a.QuotaSource == "quota_api"
		source := sourceCode(a.QuotaSource, authoritative)
		cards = append(cards, quotaCard{
			Provider: "zai", ProviderLabel: providerLabel("zai"), Account: a.Name, Plan: a.Plan, Health: health,
			Windows: []quotaWindow{
				resetWindow("five_hour", "5-hour", 5*time.Hour, a.FiveHourUtilization, a.FiveHourResetsAt, source, authoritative, a.QuotaStale, false),
				resetWindow("weekly", "Weekly", 7*24*time.Hour, a.WeeklyUtilization, a.WeeklyResetsAt, source, authoritative, a.QuotaStale, false),
			},
		})
	}
	return cards
}

var opencodeWindows = []struct {
	id, label string
	duration  time.Duration
}{
	{"five_hour", "5-hour", 5 * time.Hour},
	{"weekly", "Weekly", 7 * 24 * time.Hour},
	{"monthly", "Monthly", 30 * 24 * time.Hour},
}

func opencodeQuotaCards(raw json.RawMessage) []quotaCard {
	var status opencodeResourceStatus
	if len(raw) == 0 || json.Unmarshal(raw, &status) != nil {
		return nil
	}
	cards := make([]quotaCard, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		card := quotaCard{Provider: "opencode-go", ProviderLabel: providerLabel("opencode-go"), Account: a.Name, Health: "healthy"}
		if a.Disabled {
			card.Health = "disabled"
		}
		for _, spec := range opencodeWindows {
			w, ok := a.Windows[spec.id]
			if !ok || !w.Known {
				card.Windows = append(card.Windows, quotaWindow{ID: spec.id, Label: spec.label, Mode: "reset", Known: false, WindowSeconds: int64(spec.duration / time.Second)})
				continue
			}
			utilization := 0.0
			if w.Utilization != nil {
				utilization = *w.Utilization
			}
			if w.Exhausted && !a.Disabled {
				card.Health = "exhausted"
			}
			card.Windows = append(card.Windows, resetWindow(spec.id, spec.label, spec.duration, utilization, w.ResetAt, sourceCode(w.Source, w.Authoritative), w.Authoritative, false, w.Exhausted))
		}
		cards = append(cards, card)
	}
	return cards
}

type syntheticStatusView struct {
	Status           string `json:"status"`
	ThresholdPercent int    `json:"threshold_percent"`
	Accounts         []struct {
		Name             string                            `json:"name"`
		Provider         string                            `json:"provider"`
		Kind             string                            `json:"kind"`
		Learned          bool                              `json:"learned"`
		Health           string                            `json:"health"`
		HealthReason     string                            `json:"health_reason"`
		Windows          map[string]opencodeResourceWindow `json:"windows"`
		NextRegenPercent *float64                          `json:"next_regen_percent"`
		InFlight         int                               `json:"in_flight"`
		PeakInFlight     int                               `json:"peak_in_flight"`
		PeakInFlightAt   *time.Time                        `json:"peak_in_flight_at"`
		MaxConcurrent    int                               `json:"max_concurrent"`
		Stats            struct {
			Requests        int64    `json:"requests"`
			Failures        int64    `json:"failures"`
			OutputTokens    int64    `json:"output_tokens"`
			TPSSamples      int      `json:"tps_samples"`
			MeanOutputTPS   *float64 `json:"mean_output_tps"`
			MedianOutputTPS *float64 `json:"median_output_tps"`
			MeanTTFTMillis  *float64 `json:"mean_ttft_ms"`
			LastStatus      int      `json:"last_status"`
		} `json:"stats"`
	} `json:"accounts"`
	Routing syntheticRoutingStatus `json:"routing"`
}

// syntheticQuotaCards turns the synthetic module's status into quota cards
// (one per lane) and the synthetic.new panel: the regen schedule of the paid
// weekly credit, infinite-mode speed, and concurrency.
func syntheticQuotaCards(raw json.RawMessage, now time.Time) ([]quotaCard, *syntheticPanel) {
	var status syntheticStatusView
	if len(raw) == 0 || json.Unmarshal(raw, &status) != nil {
		return nil, nil
	}
	panel := &syntheticPanel{
		Status: status.Status, ThresholdPercent: status.ThresholdPercent,
		RegenIntervalMinutes: int(syntheticRegenInterval / time.Minute),
		Lanes:                []syntheticLane{},
		Routing: syntheticRouting{
			PaidPicks: status.Routing.PaidPicks, InfinitePicks: status.Routing.InfinitePicks, Fallbacks: status.Routing.Fallbacks,
			Rejections: status.Routing.Rejections, InfiniteNonStream: status.Routing.InfiniteNonStream,
			LastFallbackReason: reasonCode(status.Routing.LastFallbackReason), LastFallbackAt: status.Routing.LastFallbackAt,
		},
	}
	cards := make([]quotaCard, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		lane := syntheticLane{
			Name: a.Name, Kind: a.Kind, Health: a.Health, Reason: reasonCode(a.HealthReason), Learned: a.Learned,
			InFlight: a.InFlight, PeakInFlight: a.PeakInFlight, PeakInFlightAt: a.PeakInFlightAt, MaxConcurrent: a.MaxConcurrent,
			Requests: a.Stats.Requests, Failures: a.Stats.Failures, OutputTokens: a.Stats.OutputTokens,
			TPSSamples: a.Stats.TPSSamples, MedianTPS: a.Stats.MedianOutputTPS, MeanTPS: a.Stats.MeanOutputTPS,
			MeanTTFTMillis: a.Stats.MeanTTFTMillis, LastStatus: a.Stats.LastStatus,
		}
		panel.Lanes = append(panel.Lanes, lane)

		card := quotaCard{Provider: "synthetic", ProviderLabel: providerLabel("synthetic"), Account: a.Name, Kind: a.Kind, Health: a.Health, Reason: reasonCode(a.HealthReason)}
		if a.Kind == "infinite" {
			card.Plan = "Infinite mode (free)"
			card.Windows = []quotaWindow{{ID: "infinite", Label: "Infinite mode", Mode: "free", Known: true, UsedPercent: percentPtr(0), RemainingPercent: percentPtr(100), Source: "free"}}
		} else {
			card.Plan = "Weekly credit"
			card.Windows = []quotaWindow{syntheticWeeklyWindow(a.Windows["weekly"], a.NextRegenPercent, status.ThresholdPercent, now)}
			if panel.Schedule == nil {
				panel.Schedule = regenSchedule(card.Windows[0], 8)
			}
		}
		card.Metrics = append(card.Metrics, quotaMetric{Key: "in_flight", Label: "In flight", Value: float64(a.InFlight)})
		if a.MaxConcurrent > 0 {
			card.Metrics = append(card.Metrics, quotaMetric{Key: "max_concurrent", Label: "Cap", Value: float64(a.MaxConcurrent)})
		}
		if a.Stats.MedianOutputTPS != nil {
			card.Metrics = append(card.Metrics, quotaMetric{Key: "median_output_tps", Label: "Speed", Value: *a.Stats.MedianOutputTPS, Unit: "tok/s"})
		}
		if a.Stats.MeanTTFTMillis != nil {
			card.Metrics = append(card.Metrics, quotaMetric{Key: "mean_ttft_s", Label: "TTFT", Value: math.Round(*a.Stats.MeanTTFTMillis/100) / 10, Unit: "s"})
		}
		cards = append(cards, card)
	}
	return cards, panel
}

func syntheticWeeklyWindow(w opencodeResourceWindow, regenPercent *float64, threshold int, now time.Time) quotaWindow {
	window := quotaWindow{ID: "weekly", Label: "Weekly credit", Mode: "regen", Known: false, RegenSeconds: int64(syntheticRegenInterval / time.Second), ThresholdPercent: threshold, Source: "unknown"}
	if !w.Known || w.Utilization == nil {
		return window
	}
	used := *w.Utilization * 100
	if w.Exhausted && used < 100 {
		used = 100
	}
	regen := syntheticDefaultRegenPc
	if regenPercent != nil && *regenPercent > 0 {
		regen = *regenPercent
	}
	window.Known = true
	window.UsedPercent = percentPtr(used)
	window.RemainingPercent = percentPtr(100 - used)
	window.RegenPercent = &regen
	window.Source = sourceCode(w.Source, w.Authoritative)
	window.Authoritative = w.Authoritative
	window.Exhausted = w.Exhausted
	if w.ResetAt != nil && !w.ResetAt.IsZero() {
		next := w.ResetAt.UTC()
		for next.Before(now) {
			next = next.Add(syntheticRegenInterval)
		}
		window.ResetsAt = timePtr(next)
		ticks := int(math.Ceil(used/regen - 1e-9))
		if ticks < 1 {
			window.FullAt = timePtr(next)
		} else {
			window.FullAt = timePtr(next.Add(time.Duration(ticks-1) * syntheticRegenInterval))
		}
		window.StartsAt = timePtr(next.Add(-syntheticRegenInterval))
		window.WindowSeconds = int64(syntheticRegenInterval / time.Second)
	}
	return window
}

// regenSchedule projects the next ticks of a regen window (no new spend).
func regenSchedule(w quotaWindow, limit int) []syntheticRegenAt {
	if !w.Known || w.ResetsAt == nil || w.RegenPercent == nil || w.RemainingPercent == nil {
		return nil
	}
	remaining := *w.RemainingPercent
	out := []syntheticRegenAt{}
	at := *w.ResetsAt
	for i := 0; i < limit && remaining < 100; i++ {
		add := math.Min(*w.RegenPercent, 100-remaining)
		remaining = math.Round((remaining+add)*100) / 100
		out = append(out, syntheticRegenAt{At: at, AddsPercent: math.Round(add*100) / 100, RemainingPercent: remaining})
		at = at.Add(syntheticRegenInterval)
	}
	return out
}

// --- handlers ---

func isResourceUsageDataPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	return path == resourceUsageDataPath || path == resourcePluginBasePath+resourceUsageDataPath
}

func requestRange(req pluginapi.ManagementRequest) string {
	if req.Query == nil {
		return usage.DefaultRange
	}
	return usage.RangeByID(strings.TrimSpace(req.Query.Get("range"))).ID
}

// usageDataResponse answers usage.json (resource route, unauthenticated, so
// redacted data only) and the authenticated management usage route.
func (c *Coordinator) usageDataResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, err := json.Marshal(c.buildUsagePageData(requestRange(req)))
	if err != nil {
		return managementError(http.StatusInternalServerError, "encode_failed", "could not encode usage data")
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           []string{managementContentType},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: body,
	}
}

func newNonce() string {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// usagePageResponse renders the usage page. The inline script runs under a
// per-response CSP nonce; the initial payload is embedded as JSON (not
// executed), and json.Marshal escapes <, > and & so it cannot close the
// script element.
func (c *Coordinator) usagePageResponse(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	data, err := json.Marshal(c.buildUsagePageData(requestRange(req)))
	if err != nil {
		return managementError(http.StatusInternalServerError, "encode_failed", "could not encode usage data")
	}
	nonce := newNonce()
	if nonce == "" {
		return managementError(http.StatusInternalServerError, "nonce_failed", "could not create a script nonce")
	}
	// Nonce first, on the template only, so account or model names that
	// happen to contain a marker can never be rewritten.
	page := strings.ReplaceAll(usagePageTemplate, usagePageNonceMarker, nonce)
	page = strings.Replace(page, usagePageInitialMarker, string(data), 1)
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            []string{resourceContentType},
			"Content-Security-Policy": []string{"default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'"},
			"Referrer-Policy":         []string{"no-referrer"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Cache-Control":           []string{"no-store"},
		},
		Body: []byte(page),
	}
}
