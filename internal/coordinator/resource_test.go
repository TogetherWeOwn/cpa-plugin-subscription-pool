package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeStatusModule is a fakeModule that returns a fixed Status() payload and
// names accounts for the usage ledger, so tests can drive the usage page
// with realistic per-provider JSON without any real provider logic.
type fakeStatusModule struct {
	fakeModule
	status   json.RawMessage
	accounts map[string]string
}

func (m *fakeStatusModule) Status(context.Context) (json.RawMessage, error) {
	return m.status, nil
}

func (m *fakeStatusModule) UsageAccount(record pluginapi.UsageRecord) string {
	return m.accounts[record.AuthID]
}

var pageNow = time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)

const (
	hiddenIdentity        = "test-only-private-account-pseudonym"
	hiddenPaidLaneRef     = "openai-compatibility:synthetic:35470c92a63b"
	hiddenInfiniteLaneRef = "openai-compatibility:synthetic-infinite:c5e9e38ce3e1"
	hiddenAccount         = "acct-0123456789"
	hostileName           = "go-1</script><script>alert(1)</script>"
)

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pageCoordinator hosts zai, opencode-go and synthetic fakes whose status
// payloads carry the private fields each real module emits.
func pageCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	fiveHourReset := pageNow.Add(2 * time.Hour)
	weeklyReset := pageNow.Add(4 * 24 * time.Hour)
	zaiStatus := mustJSON(t, map[string]any{"accounts": []map[string]any{{
		"identity":              hiddenIdentity,
		"cooldown":              map[string]any{"active": true, "until": fiveHourReset, "reason": "retry_after", "source": "zai_runtime_health_v1"},
		"name":                  "zai-pro-1",
		"key_suffix":            "redacted",
		"plan":                  "pro",
		"five_hour_utilization": 0.42,
		"weekly_utilization":    0.10,
		"five_hour_resets_at":   fiveHourReset,
		"weekly_resets_at":      weeklyReset,
		"quota_source":          "quota_api",
		"health":                "healthy",
	}}})
	opencodeStatus := mustJSON(t, map[string]any{"status": "registered", "accounts": []map[string]any{{
		"name": hostileName,
		"windows": map[string]any{"monthly": map[string]any{
			"known": true, "utilization": 0.75, "resets_at": pageNow.Add(8 * 24 * time.Hour), "source": "dashboard usage poll", "authoritative": true,
		}},
	}}})
	regen := pageNow.Add(40 * time.Minute)
	syntheticStatus := mustJSON(t, map[string]any{
		"provider": "synthetic", "status": "registered", "threshold_percent": 95,
		"accounts": []map[string]any{
			{
				"name": "synthetic#1", "provider": "synthetic", "kind": "paid", "auth_id": hiddenPaidLaneRef, "account": hiddenAccount,
				"credential_bound": true, "health": "healthy", "next_regen_percent": 2, "in_flight": 1, "peak_in_flight": 7, "max_concurrent": 0,
				"windows": map[string]any{"weekly": map[string]any{"known": true, "utilization": 0.0442, "resets_at": regen, "source": "quota endpoint (v2)", "authoritative": true}},
				"stats":   map[string]any{"requests": 20, "failures": 1, "output_tokens": 9000, "tps_samples": 20, "median_output_tps": 131.2, "mean_ttft_ms": 900},
			},
			{
				"name": "synthetic-infinite#1", "provider": "synthetic-infinite", "kind": "infinite", "auth_id": hiddenInfiniteLaneRef, "account": hiddenAccount,
				"credential_bound": true, "health": "healthy", "in_flight": 0, "peak_in_flight": 10, "max_concurrent": 0,
				"windows": map[string]any{"weekly": map[string]any{"known": true, "utilization": 0, "source": "infinite mode (free, not metered)"}},
				"stats":   map[string]any{"requests": 45, "output_tokens": 30000, "tps_samples": 40, "median_output_tps": 5.1, "mean_ttft_ms": 13100},
			},
		},
		"routing":          map[string]any{"paid_picks": 20, "infinite_picks": 45, "fallbacks_to_infinite": 1, "rejections": 0, "infinite_non_stream_picks": 0},
		"observation_gaps": []string{"resets_at is the next 2% regeneration tick"},
	})
	zai := &fakeStatusModule{fakeModule: fakeModule{id: "zai"}, status: zaiStatus}
	opencodeGo := &fakeStatusModule{fakeModule: fakeModule{id: "opencode-go"}, status: opencodeStatus}
	syn := &fakeStatusModule{
		fakeModule: fakeModule{id: "synthetic"}, status: syntheticStatus,
		accounts: map[string]string{hiddenPaidLaneRef: "synthetic#1", hiddenInfiniteLaneRef: "synthetic-infinite#1"},
	}
	c := New(zai, opencodeGo, syn)
	c.now = func() time.Time { return pageNow }
	c.registry.swap(map[string]registryEntry{hiddenPaidLaneRef: {providerID: "synthetic"}, hiddenInfiniteLaneRef: {providerID: "synthetic"}})
	return c
}

func usageRecord(authID, alias string, at time.Time, out int64, status int) pluginapi.UsageRecord {
	record := pluginapi.UsageRecord{
		AuthID: authID, Provider: "openai-compatible-synthetic", Alias: alias, Model: "hf:upstream", APIKey: "client-key-must-not-leak",
		RequestedAt: at, Latency: 20 * time.Second, TTFT: 10 * time.Second,
		Detail: pluginapi.UsageDetail{InputTokens: 2000, OutputTokens: out},
	}
	if status != 0 {
		record.Failed = true
		record.Failure = pluginapi.UsageFailure{StatusCode: status, Body: "Too many concurrent requests"}
	}
	return record
}

func get(c *Coordinator, path string, query url.Values) pluginapi.ManagementResponse {
	return c.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: path, Query: query})
}

func assertRedacted(t *testing.T, body string) {
	t.Helper()
	for _, forbidden := range []string{hiddenIdentity, hiddenPaidLaneRef, hiddenInfiniteLaneRef, "openai-compatibility:", hiddenAccount, "retry_after", "zai_runtime_health_v1", "client-key-must-not-leak", "Bearer", "dashboard-api-key", "key_suffix"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("usage output contains forbidden %q", forbidden)
		}
	}
}

func TestUsagePageServesNoncedScriptAndEscapedInitialData(t *testing.T) {
	c := pageCoordinator(t)
	resp := get(c, resourcePluginBasePath+resourceStatusPath, nil)
	if resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != resourceContentType {
		t.Fatalf("page = %d %q", resp.StatusCode, resp.Headers.Get("Content-Type"))
	}
	body := string(resp.Body)
	csp := resp.Headers.Get("Content-Security-Policy")
	match := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]{20,})'`).FindStringSubmatch(csp)
	if match == nil {
		t.Fatalf("CSP %q has no script nonce", csp)
	}
	if !strings.Contains(body, `<script nonce="`+match[1]+`">`) {
		t.Fatal("inline script does not carry the CSP nonce")
	}
	for _, directive := range []string{"default-src 'none'", "style-src 'unsafe-inline'", "connect-src 'self'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP %q missing %q", csp, directive)
		}
	}
	for _, unsafe := range []string{"unsafe-eval", "frame-ancestors", "script-src 'unsafe-inline'"} {
		if strings.Contains(csp, unsafe) {
			t.Fatalf("CSP %q contains %q", csp, unsafe)
		}
	}
	if strings.Contains(body, usagePageNonceMarker) || strings.Contains(body, usagePageInitialMarker) {
		t.Fatal("template markers left in the page")
	}
	for _, header := range [][2]string{{"Referrer-Policy", "no-referrer"}, {"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-store"}} {
		if got := resp.Headers.Get(header[0]); got != header[1] {
			t.Fatalf("%s = %q", header[0], got)
		}
	}
	// A hostile account name cannot close the data element or inject a script.
	if strings.Contains(body, "</script><script>alert(1)") {
		t.Fatal("initial data was not HTML-escaped")
	}
	start := strings.Index(body, `<script type="application/json" id="sp-data">`)
	end := strings.Index(body[start:], "</script>")
	if start < 0 || end < 0 {
		t.Fatal("initial data element missing")
	}
	var initial usagePageData
	if err := json.Unmarshal([]byte(body[start+len(`<script type="application/json" id="sp-data">`):start+end]), &initial); err != nil {
		t.Fatalf("initial data is not JSON: %v", err)
	}
	if len(initial.Quota) != 4 || initial.Quota[1].Account != hostileName {
		t.Fatalf("initial quota = %+v", initial.Quota)
	}
	assertRedacted(t, body)

	again := get(c, resourceStatusPath, nil)
	if again.Headers.Get("Content-Security-Policy") == csp {
		t.Fatal("nonce must change on every response")
	}
}

func TestUsageDataReportsQuotaWindowsAndSyntheticPanel(t *testing.T) {
	c := pageCoordinator(t)
	resp := get(c, resourcePluginBasePath+resourceUsageDataPath, nil)
	if resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != managementContentType || resp.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("usage.json = %d %v", resp.StatusCode, resp.Headers)
	}
	assertRedacted(t, string(resp.Body))
	var data usagePageData
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		t.Fatal(err)
	}
	if data.Plugin != PluginID || data.Usage.Range != "24h" || strings.Join(data.Ranges, ",") != "1h,6h,24h,7d" {
		t.Fatalf("header = %s %s %v", data.Plugin, data.Usage.Range, data.Ranges)
	}
	if len(data.Providers) != 3 || data.Providers[0].Label != "Z.ai" || data.Providers[1].Label != "OpenCode Go" || data.Providers[2].Label != "synthetic.new" || data.Providers[2].Accounts != 2 {
		t.Fatalf("providers = %+v", data.Providers)
	}
	zai := data.Quota[0]
	if zai.Plan != "pro" || len(zai.Windows) != 2 || *zai.Windows[0].RemainingPercent != 58 || !zai.Windows[0].Authoritative || zai.Windows[0].WindowSeconds != 5*3600 {
		t.Fatalf("zai card = %+v", zai)
	}
	if !zai.Windows[0].ResetsAt.Equal(pageNow.Add(2*time.Hour)) || !zai.Windows[0].StartsAt.Equal(pageNow.Add(-3*time.Hour)) {
		t.Fatalf("zai 5-hour window = %v .. %v", zai.Windows[0].StartsAt, zai.Windows[0].ResetsAt)
	}
	goCard := data.Quota[1]
	if len(goCard.Windows) != 3 || goCard.Windows[0].Known || *goCard.Windows[2].UsedPercent != 75 {
		t.Fatalf("opencode-go card = %+v", goCard)
	}
	paid, infinite := data.Quota[2], data.Quota[3]
	weekly := paid.Windows[0]
	if paid.Kind != "paid" || weekly.Mode != "regen" || *weekly.RemainingPercent != 95.58 || *weekly.RegenPercent != 2 || weekly.ThresholdPercent != 95 {
		t.Fatalf("synthetic paid window = %+v", weekly)
	}
	// 4.42% used refills in three 2% ticks: 40 min, then 2 x 202 min later.
	if !weekly.ResetsAt.Equal(pageNow.Add(40*time.Minute)) || !weekly.FullAt.Equal(pageNow.Add(40*time.Minute+2*202*time.Minute)) {
		t.Fatalf("regen next %v full %v", weekly.ResetsAt, weekly.FullAt)
	}
	if infinite.Kind != "infinite" || infinite.Windows[0].Mode != "free" {
		t.Fatalf("infinite card = %+v", infinite)
	}
	panel := data.Synthetic
	if panel == nil || panel.ThresholdPercent != 95 || panel.RegenIntervalMinutes != 202 || len(panel.Lanes) != 2 || panel.Lanes[1].PeakInFlight != 10 || *panel.Lanes[1].MedianTPS != 5.1 {
		t.Fatalf("synthetic panel = %+v", panel)
	}
	if len(panel.Schedule) != 3 || panel.Schedule[2].RemainingPercent != 100 || panel.Schedule[2].AddsPercent != 0.42 {
		t.Fatalf("regen schedule = %+v", panel.Schedule)
	}
	if panel.Routing.Fallbacks != 1 || panel.Routing.InfinitePicks != 45 {
		t.Fatalf("routing = %+v", panel.Routing)
	}
}

func TestUsageDataCountsDispatchedUsagePerAccountAndModel(t *testing.T) {
	c := pageCoordinator(t)
	_ = c.handleUsage(usageRecord(hiddenPaidLaneRef, "synthetic/deepseek-v4.1-flash", pageNow.Add(-10*time.Minute), 400, 0))
	_ = c.handleUsage(usageRecord(hiddenPaidLaneRef, "synthetic/deepseek-v4.1-flash", pageNow.Add(-9*time.Minute), 0, http.StatusTooManyRequests))
	_ = c.handleUsage(usageRecord(hiddenInfiniteLaneRef, "synthetic/deepseek-v4.1-flash", pageNow.Add(-3*time.Hour), 600, 0))
	_ = c.handleUsage(usageRecord("not-ours", "other", pageNow, 1, 0))

	for _, path := range []string{resourcePluginBasePath + resourceUsageDataPath, managementUsagePath} {
		resp := get(c, path, url.Values{"range": {"1h"}})
		assertRedacted(t, string(resp.Body))
		var data usagePageData
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			t.Fatal(err)
		}
		if data.Usage.Range != "1h" || len(data.Usage.Buckets) != 12 || len(data.Usage.Totals) != 1 {
			t.Fatalf("%s 1h usage = %+v", path, data.Usage)
		}
		total := data.Usage.Totals[0]
		if total.Provider != "synthetic" || total.Account != "synthetic#1" || total.Model != "synthetic/deepseek-v4.1-flash" ||
			total.Requests != 2 || total.Failures != 1 || total.Status429 != 1 || total.OutputTokens != 400 || total.InputTokens != 4000 {
			t.Fatalf("%s 1h total = %+v", path, total)
		}
	}
	week := get(c, resourceUsageDataPath, url.Values{"range": {"7d"}})
	var data usagePageData
	if err := json.Unmarshal(week.Body, &data); err != nil {
		t.Fatal(err)
	}
	if data.Usage.Range != "7d" || len(data.Usage.Totals) != 2 || data.Usage.Records != 3 {
		t.Fatalf("7d usage = %+v", data.Usage)
	}
	bogus := get(c, resourceUsageDataPath, url.Values{"range": {"<script>"}})
	if err := json.Unmarshal(bogus.Body, &data); err != nil || data.Usage.Range != "24h" {
		t.Fatalf("unknown range = %q, %v", data.Usage.Range, err)
	}
}

func TestUsageRoutesAreRegistered(t *testing.T) {
	routes := New(&fakeModule{id: "alpha"}).managementRoutes()
	var usageRoute bool
	for _, route := range routes.Routes {
		if route.Method == http.MethodGet && route.Path == managementUsagePath {
			usageRoute = true
		}
	}
	if !usageRoute {
		t.Fatalf("routes = %+v, want GET %s", routes.Routes, managementUsagePath)
	}
	// The host HTML-escapes plugin menu labels in its plugin list, so a label
	// with &, <, >, ' or " would show its entity text in the sidebar.
	if strings.ContainsAny(usagePageMenu, "&<>'\"") {
		t.Fatalf("menu label %q contains characters the host escapes", usagePageMenu)
	}
	if len(routes.Resources) != 2 || routes.Resources[0].Path != resourceStatusPath || routes.Resources[0].Menu != usagePageMenu ||
		routes.Resources[1].Path != resourceUsageDataPath || routes.Resources[1].Menu != "" {
		t.Fatalf("resources = %+v", routes.Resources)
	}
}

func TestResourceStatusResponseHandlesLookalikeResourcePath(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	for _, path := range []string{"/attacker" + resourcePluginBasePath + resourceStatusPath, "/attacker" + resourcePluginBasePath + resourceUsageDataPath} {
		if resp := get(c, path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("lookalike resource path %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestUsagePageRendersWithNoAccounts(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	resp := get(c, resourceStatusPath, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), "No accounts are configured yet.") {
		t.Fatalf("empty page = %d", resp.StatusCode)
	}
	var data usagePageData
	if err := json.Unmarshal(get(c, resourceUsageDataPath, nil).Body, &data); err != nil || len(data.Quota) != 0 || data.Synthetic != nil {
		t.Fatalf("empty data = %+v, %v", data, err)
	}
}

func TestUsageLedgerPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c := pageCoordinator(t)
	c.attachLedgerStore(dir)
	_ = c.handleUsage(usageRecord(hiddenPaidLaneRef, "synthetic/glm-5.3", pageNow.Add(-time.Minute), 100, 0))
	c.flushLedger()
	saved := filepath.Join(dir, ledgerSubdir, ledgerStateName)
	raw, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("ledger not saved: %v", err)
	}
	if info, _ := os.Stat(saved); info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, want 0600", info.Mode().Perm())
	}
	assertRedacted(t, string(raw))

	restarted := pageCoordinator(t)
	restarted.attachLedgerStore(dir)
	var data usagePageData
	if err := json.Unmarshal(get(restarted, resourceUsageDataPath, url.Values{"range": {"1h"}}).Body, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Usage.Totals) != 1 || data.Usage.Totals[0].OutputTokens != 100 || !data.Ledger.Persisted {
		t.Fatalf("restored usage = %+v, ledger %+v", data.Usage.Totals, data.Ledger)
	}
}

func TestUsageAfterShutdownStartsNoBackgroundSave(t *testing.T) {
	dir := t.TempDir()
	c := pageCoordinator(t)
	c.attachLedgerStore(dir)
	if err := c.Shutdown(); err != nil {
		t.Fatal(err)
	}
	_ = c.handleUsage(usageRecord(hiddenPaidLaneRef, "synthetic/glm-5.3", pageNow, 10, 0))
	c.ledgerStore.wg.Wait()
	if _, err := os.Stat(filepath.Join(dir, ledgerSubdir, ledgerStateName)); !os.IsNotExist(err) {
		t.Fatalf("a usage record after shutdown wrote the ledger (stat err = %v)", err)
	}
}
