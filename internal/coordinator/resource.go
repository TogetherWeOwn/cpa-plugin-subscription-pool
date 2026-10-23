package coordinator

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// isResourceStatusPath reports whether path is this plugin's single
// unauthenticated resource route, either as the module-relative path a
// ManagementCapable module would see ("/status") or as the host-facing path
// under this plugin's own resourcePluginBasePath.
func isResourceStatusPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	return path == resourceStatusPath || path == resourcePluginBasePath+resourceStatusPath
}

// resourceStatusResponse renders the Subscription Quota dashboard entirely
// server-side from the coordinator's own managementStatus() aggregation.
// This route is dispatched without authentication (the host's resource-route
// convention), so it must never embed the management key or any other
// credential — only the redacted per-account fields each module already
// exposes through Status().
func (c *Coordinator) resourceStatusResponse() pluginapi.ManagementResponse {
	body := renderResourceStatusHTML(c.managementStatus(), c.runtimeNow())
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            []string{resourceContentType},
			"Content-Security-Policy": []string{"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"},
			"Referrer-Policy":         []string{"no-referrer"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Cache-Control":           []string{"no-store"},
		},
		Body: []byte(body),
	}
}

type dashboardWindow struct {
	Label   string
	Known   bool
	Percent float64
	ResetAt *time.Time
	Source  string
}

type dashboardAccount struct {
	Provider string
	Name     string
	Health   string
	Windows  []dashboardWindow
}

type zaiResourceAccount struct {
	Name                string     `json:"name"`
	FiveHourUtilization float64    `json:"five_hour_utilization"`
	WeeklyUtilization   float64    `json:"weekly_utilization"`
	FiveHourResetsAt    *time.Time `json:"five_hour_resets_at"`
	WeeklyResetsAt      *time.Time `json:"weekly_resets_at"`
	QuotaSource         string     `json:"quota_source"`
	QuotaStale          bool       `json:"quota_stale"`
	Health              string     `json:"health"`
}

type zaiResourceStatus struct {
	Accounts []zaiResourceAccount `json:"accounts"`
}

func zaiDashboardAccounts(raw json.RawMessage) []dashboardAccount {
	if len(raw) == 0 {
		return nil
	}
	var status zaiResourceStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil
	}
	accounts := make([]dashboardAccount, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		health := a.Health
		if health == "" {
			health = "unknown"
		}
		source := zaiSourceLabel(a.QuotaSource, a.QuotaStale)
		accounts = append(accounts, dashboardAccount{
			Provider: "Z.ai",
			Name:     a.Name,
			Health:   health,
			Windows: []dashboardWindow{
				{Label: "5h", Known: true, Percent: a.FiveHourUtilization * 100, ResetAt: a.FiveHourResetsAt, Source: source},
				{Label: "Weekly", Known: true, Percent: a.WeeklyUtilization * 100, ResetAt: a.WeeklyResetsAt, Source: source},
			},
		})
	}
	return accounts
}

func zaiSourceLabel(source string, stale bool) string {
	label := source
	if label == "" {
		label = "unknown"
	}
	if stale {
		label += " (stale)"
	}
	return label
}

type opencodeResourceWindow struct {
	Known         bool       `json:"known"`
	Utilization   *float64   `json:"utilization"`
	Exhausted     bool       `json:"exhausted"`
	ResetAt       *time.Time `json:"resets_at"`
	Source        string     `json:"source"`
	Authoritative bool       `json:"authoritative"`
}

type opencodeResourceAccount struct {
	Name     string                            `json:"name"`
	Disabled bool                              `json:"disabled"`
	Windows  map[string]opencodeResourceWindow `json:"windows"`
}

type opencodeResourceStatus struct {
	Accounts []opencodeResourceAccount `json:"accounts"`
}

func opencodeDashboardAccounts(raw json.RawMessage) []dashboardAccount {
	if len(raw) == 0 {
		return nil
	}
	var status opencodeResourceStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil
	}
	accounts := make([]dashboardAccount, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		health := "healthy"
		if a.Disabled {
			health = "disabled"
		} else {
			for _, kind := range []string{"five_hour", "weekly", "monthly"} {
				if w, ok := a.Windows[kind]; ok && w.Known && w.Exhausted {
					health = "exhausted"
					break
				}
			}
		}
		windows := make([]dashboardWindow, 0, 3)
		for _, kind := range []string{"five_hour", "weekly", "monthly"} {
			label := opencodeWindowLabel(kind)
			w, ok := a.Windows[kind]
			if !ok || !w.Known {
				windows = append(windows, dashboardWindow{Label: label, Known: false, Source: "unknown"})
				continue
			}
			percent := 0.0
			if w.Utilization != nil {
				percent = *w.Utilization * 100
			}
			if w.Exhausted && percent < 100 {
				percent = 100
			}
			windows = append(windows, dashboardWindow{Label: label, Known: true, Percent: percent, ResetAt: w.ResetAt, Source: opencodeSourceLabel(w)})
		}
		accounts = append(accounts, dashboardAccount{Provider: "OpenCode Go", Name: a.Name, Health: health, Windows: windows})
	}
	return accounts
}

type syntheticResourceAccount struct {
	Name    string                            `json:"name"`
	Kind    string                            `json:"kind"`
	Health  string                            `json:"health"`
	Windows map[string]opencodeResourceWindow `json:"windows"`
}

type syntheticResourceStatus struct {
	Accounts []syntheticResourceAccount `json:"accounts"`
}

// syntheticDashboardAccounts renders the synthetic module's lanes. Its
// weekly window uses the same shape as OpenCode Go's windows; resets_at is
// the next 2% regeneration tick rather than a full reset.
func syntheticDashboardAccounts(raw json.RawMessage) []dashboardAccount {
	if len(raw) == 0 {
		return nil
	}
	var status syntheticResourceStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil
	}
	accounts := make([]dashboardAccount, 0, len(status.Accounts))
	for _, a := range status.Accounts {
		w, ok := a.Windows["weekly"]
		window := dashboardWindow{Label: "Weekly credits (next 2% regen)", Known: false, Source: "unknown"}
		if ok && w.Known {
			percent := 0.0
			if w.Utilization != nil {
				percent = *w.Utilization * 100
			}
			if w.Exhausted && percent < 100 {
				percent = 100
			}
			window = dashboardWindow{Label: window.Label, Known: true, Percent: percent, ResetAt: w.ResetAt, Source: opencodeSourceLabel(w)}
		}
		if a.Kind == "infinite" {
			window.Label = "Infinite mode (free, not metered)"
		}
		health := a.Health
		if health == "unbound" {
			health = "disabled"
		}
		accounts = append(accounts, dashboardAccount{Provider: "synthetic.new " + a.Kind, Name: a.Name, Health: health, Windows: []dashboardWindow{window}})
	}
	return accounts
}

func opencodeWindowLabel(kind string) string {
	switch kind {
	case "five_hour":
		return "5h"
	case "weekly":
		return "Weekly"
	case "monthly":
		return "Monthly"
	default:
		return kind
	}
}

func opencodeSourceLabel(w opencodeResourceWindow) string {
	switch {
	case w.Authoritative:
		return "authoritative"
	case strings.HasPrefix(w.Source, "proxy-observed"):
		return "429-derived"
	case w.Source == "":
		return "unknown"
	default:
		return w.Source
	}
}

func renderResourceStatusHTML(status managementStatusBody, now time.Time) string {
	var b strings.Builder
	b.WriteString(resourceStatusHead)
	b.WriteString(`<main><h1>Subscription Quota</h1><p class="meta">Generated `)
	b.WriteString(html.EscapeString(status.GeneratedAt.UTC().Format("2006-01-02 15:04:05 UTC")))
	b.WriteString(`</p>`)

	accounts := append(zaiDashboardAccounts(status.Providers["zai"]), opencodeDashboardAccounts(status.Providers["opencode-go"])...)
	accounts = append(accounts, syntheticDashboardAccounts(status.Providers["synthetic"])...)
	if len(accounts) == 0 {
		b.WriteString(`<p class="empty">No accounts are configured yet.</p>`)
	} else {
		b.WriteString(`<div class="grid">`)
		for _, account := range accounts {
			renderAccountCard(&b, account, now)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</main></body></html>`)
	return b.String()
}

func renderAccountCard(b *strings.Builder, account dashboardAccount, now time.Time) {
	b.WriteString(`<section class="card"><header><h2>`)
	b.WriteString(html.EscapeString(account.Name))
	b.WriteString(`</h2><span class="provider">`)
	b.WriteString(html.EscapeString(account.Provider))
	b.WriteString(`</span><span class="health `)
	b.WriteString(healthClass(account.Health))
	b.WriteString(`">`)
	b.WriteString(html.EscapeString(account.Health))
	b.WriteString(`</span></header>`)
	for _, window := range account.Windows {
		renderWindowRow(b, window, now)
	}
	b.WriteString(`</section>`)
}

func renderWindowRow(b *strings.Builder, window dashboardWindow, now time.Time) {
	b.WriteString(`<div class="window"><div class="window-label">`)
	b.WriteString(html.EscapeString(window.Label))
	b.WriteString(`</div>`)
	if !window.Known {
		b.WriteString(`<div class="unknown">unknown</div></div>`)
		return
	}
	percent := clampPercent(window.Percent)
	b.WriteString(`<div class="bar"><div class="fill" style="width:`)
	fmt.Fprintf(b, "%.1f", percent)
	b.WriteString(`%"></div></div><div class="stats"><span class="percent">`)
	fmt.Fprintf(b, "%.0f%%", percent)
	b.WriteString(`</span><span class="source">`)
	b.WriteString(html.EscapeString(window.Source))
	b.WriteString(`</span></div>`)
	absolute, countdown := formatResetAt(window.ResetAt, now)
	b.WriteString(`<div class="reset">resets `)
	b.WriteString(html.EscapeString(absolute))
	b.WriteString(` (`)
	b.WriteString(html.EscapeString(countdown))
	b.WriteString(`)</div></div>`)
}

func clampPercent(percent float64) float64 {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func healthClass(health string) string {
	switch health {
	case "healthy", "throttled", "exhausted", "suspended", "disabled":
		return health
	default:
		return "unknown"
	}
}

func formatResetAt(resetAt *time.Time, now time.Time) (absolute, countdown string) {
	if resetAt == nil || resetAt.IsZero() {
		return "unknown", "unknown"
	}
	absolute = resetAt.UTC().Format("2006-01-02 15:04:05 UTC")
	remaining := resetAt.Sub(now)
	if remaining <= 0 {
		return absolute, "now"
	}
	return absolute, formatDuration(remaining)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

const resourceStatusHead = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Subscription Quota</title>
<style>
:root{color-scheme:light dark;font:14px/1.45 system-ui,sans-serif;background:#111;color:#eee}
body{margin:0;padding:24px}
main{max-width:1000px;margin:auto}
h1{font-size:24px;margin:0}
.meta{color:#aaa;margin:4px 0 20px}
.empty{padding:14px 16px;border:1px solid #555;border-radius:10px;background:#222}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:16px}
.card{border:1px solid #444;border-radius:10px;padding:16px;background:#1a1a1a}
.card header{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:10px}
.card h2{font-size:16px;margin:0}
.provider{color:#aaa;font-size:12px}
.health{margin-left:auto;font-size:11px;padding:2px 8px;border-radius:999px;text-transform:uppercase}
.health.healthy{background:#173d1e;color:#7be495}
.health.throttled{background:#3d3a17;color:#e8dc7b}
.health.exhausted{background:#4a1c1c;color:#ff8a8a}
.health.suspended{background:#4a3a1c;color:#ffd27a}
.health.disabled{background:#333;color:#999}
.health.unknown{background:#333;color:#999}
.window{margin-top:10px}
.window-label{font-size:12px;color:#aaa;margin-bottom:4px}
.bar{height:8px;border-radius:4px;background:#333;overflow:hidden}
.fill{height:100%;background:#58a6ff}
.stats{display:flex;justify-content:space-between;font-size:12px;color:#ccc;margin-top:4px}
.reset{font-size:11px;color:#888;margin-top:2px}
.unknown{font-size:12px;color:#888}
@media(prefers-color-scheme:light){:root{background:#f7f7f7;color:#222}.meta{color:#666}.empty{background:white;border-color:#ddd}.card{background:white;border-color:#ddd}}
</style>
</head>
<body>
`
