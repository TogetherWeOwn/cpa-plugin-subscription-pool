package openrouterfree

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var start = time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)

const (
	idA = "google/gemma-4-31b-it:free"
	idB = "nvidia/nemotron-3.5-lightning:free"
	idC = "poolside/laguna-s-2.1:free"
)

func catalogJSON(free ...string) string {
	items := []string{
		`{"id":"anthropic/paid-model","pricing":{"prompt":"0.000003","completion":"0.000015"}}`,
		`{"id":"openrouter/free","pricing":{"prompt":"0","completion":"0"}}`,
		`{"id":"vendor/priced-request:free","pricing":{"prompt":"0","completion":"0","request":"0.01"}}`,
		`{"id":"vendor/image-only:free","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["image"]}}`,
	}
	for _, id := range free {
		items = append(items, `{"id":"`+id+`","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}`)
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

type call struct {
	method, url, auth, body string
}

// fakeHTTP serves OpenRouter and the host management API deterministically.
type fakeHTTP struct {
	mu       sync.Mutex
	catalog  string
	catCode  int
	freeTier bool
	keyCode  int
	keyBody  string
	patchOK  int
	calls    []call
	patched  chan struct{}
}

func newFake(free ...string) *fakeHTTP {
	return &fakeHTTP{catalog: catalogJSON(free...), catCode: 200, keyCode: 200, patchOK: 200, patched: make(chan struct{}, 8)}
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{req.Method, req.URL.String(), req.Header.Get("Authorization"), body})
	code, out := 404, ""
	switch {
	case req.URL.String() == catalogURL:
		code, out = f.catCode, f.catalog
	case req.URL.String() == keyURL:
		code, out = f.keyCode, f.keyBody
		if out == "" {
			code, out = f.keyCode, `{"data":{"is_free_tier":`+map[bool]string{true: "true", false: "false"}[f.freeTier]+`,"free_model_daily_requests":{"limit":`+map[bool]string{true: "50", false: "1000"}[f.freeTier]+`}}}`
		}
	case req.Method == http.MethodPatch && strings.HasSuffix(req.URL.Path, "/v0/management/openai-compatibility"):
		code, out = f.patchOK, `{"status":"ok"}`
	}
	f.mu.Unlock()
	if req.Method == http.MethodPatch {
		defer func() {
			select {
			case f.patched <- struct{}{}:
			default:
			}
		}()
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(out))}, nil
}

func (f *fakeHTTP) patches() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == http.MethodPatch {
			out = append(out, c)
		}
	}
	return out
}

func entry(models ...string) providermodule.OpenAICompatibility {
	var ms []providermodule.OpenAICompatibilityModel
	for _, id := range models {
		ms = append(ms, providermodule.OpenAICompatibilityModel{Name: id, Alias: aliasPrefix + id})
	}
	return providermodule.OpenAICompatibility{Name: providerID, BaseURL: "https://openrouter.ai/api/v1", APIKeyEntries: []providermodule.OpenAICompatibilityAPIKey{{APIKey: "test-only-openrouter-key"}}, Models: ms}
}

func hostWith(t *testing.T, f *fakeHTTP, e providermodule.OpenAICompatibility) providermodule.HostConfig {
	t.Helper()
	return providermodule.HostConfig{AuthDir: t.TempDir(), OpenAICompatibility: []providermodule.OpenAICompatibility{e}, HTTPClient: f}
}

func setup(t *testing.T, host providermodule.HostConfig, cfg string) *Module {
	t.Helper()
	m := NewModule()
	m.noLoop = true
	m.logf = func(string, ...any) {}
	m.now = func() time.Time { return start }
	if err := m.Reconfigure(context.Background(), host, json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func req(m *Module, model string) pluginapi.SchedulerPickRequest {
	ids := m.OwnedAuthIDs(context.Background())
	return pluginapi.SchedulerPickRequest{Model: model, Stream: true, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: ids[0], Provider: "openai-compatible-openrouter-free", Attributes: map[string]string{"compat_name": providerID}}}}
}

func assertPick(t *testing.T, m *Module, r pluginapi.SchedulerPickRequest, want bool) {
	t.Helper()
	p, e := m.Pick(context.Background(), r)
	if want && (e != nil || !p.Handled || p.AuthID == "") {
		t.Fatalf("expected admission, got %+v %v", p, e)
	}
	if !want && e == nil {
		t.Fatalf("expected fail-closed, got %+v", p)
	}
}

func syncNow(t *testing.T, m *Module) {
	t.Helper()
	m.syncOnce(context.Background())
}

func status(t *testing.T, m *Module) StatusView {
	t.Helper()
	raw, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var s StatusView
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseCatalogKeepsOnlyZeroPricedFreeTextModels(t *testing.T) {
	got, err := parseCatalog([]byte(catalogJSON(idB, idA, idA)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != idA+","+idB {
		t.Fatalf("got %v, want sorted de-duplicated %s,%s", got, idA, idB)
	}
	for _, bad := range []string{`{}`, `{"data":[]}`, `not json`} {
		if _, err := parseCatalog([]byte(bad)); err == nil {
			t.Fatalf("catalog %q accepted", bad)
		}
	}
}

func TestNothingIsAdmittedBeforeTheFirstVerifiedCatalog(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{}`)
	assertPick(t, m, req(m, aliasPrefix+idA), false)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
}

func TestOnlyVerifiedFreeUpstreamsAreAdmitted(t *testing.T) {
	f := newFake(idA)
	paidAlias := providermodule.OpenAICompatibilityModel{Name: "anthropic/paid-model", Alias: aliasPrefix + idA + "-sneaky"}
	e := entry(idA)
	e.Models = append(e.Models, paidAlias, providermodule.OpenAICompatibilityModel{Name: "vendor/not-suffixed", Alias: "vendor/not-suffixed"})
	m := setup(t, hostWith(t, f, e), `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	assertPick(t, m, req(m, strings.ToUpper(aliasPrefix+idA)), true) // host matches case-insensitively
	assertPick(t, m, req(m, aliasPrefix+idA+"-sneaky"), false)       // alias of a paid upstream
	assertPick(t, m, req(m, "vendor/not-suffixed"), false)
	assertPick(t, m, req(m, "anthropic/paid-model"), false)
	assertPick(t, m, req(m, ""), false)
}

func TestAmbiguousAliasMappingToAPaidModelIsRefused(t *testing.T) {
	f := newFake(idA)
	e := entry(idA)
	e.Models = append(e.Models, providermodule.OpenAICompatibilityModel{Name: "anthropic/paid-model", Alias: aliasPrefix + idA})
	m := setup(t, hostWith(t, f, e), `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), false)
}

func TestWrongEndpointAndPlaceholderKeyAreRefused(t *testing.T) {
	for _, kind := range []string{"endpoint", "placeholder"} {
		f := newFake(idA)
		e := entry(idA)
		if kind == "endpoint" {
			e.BaseURL = "https://paid.example/api/v1"
		} else {
			e.APIKeyEntries[0].APIKey = "<OPENROUTER PLACEHOLDER>"
		}
		m := setup(t, hostWith(t, f, e), `{}`)
		syncNow(t, m)
		assertPick(t, m, req(m, aliasPrefix+idA), false)
	}
}

func TestRemovedModelStopsBeingAdmittedAtTheNextSyncEvenBeforeHostCatchesUp(t *testing.T) {
	f := newFake(idA, idB)
	m := setup(t, hostWith(t, f, entry(idA, idB)), `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idB), true)
	f.mu.Lock()
	f.catalog = catalogJSON(idA)
	f.mu.Unlock()
	m.now = func() time.Time { return start.Add(6 * time.Hour) }
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idB), false)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
}

func TestChangesAreRecordedLoggedAndExposedInStatus(t *testing.T) {
	f := newFake(idA, idB)
	m := setup(t, hostWith(t, f, entry(idA, idB)), `{}`)
	var logged []string
	m.logf = func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(strings.ReplaceAll(format, "%s", "")+argString(args)))
	}
	syncNow(t, m)
	f.mu.Lock()
	f.catalog = catalogJSON(idA, idC)
	f.mu.Unlock()
	m.now = func() time.Time { return start.Add(6 * time.Hour) }
	syncNow(t, m)
	s := status(t, m)
	if strings.Join(s.Models, ",") != idA+","+idC || s.ModelCount != 2 {
		t.Fatalf("models = %v", s.Models)
	}
	var added, removed []string
	for _, c := range s.RecentChanges {
		if c.Action == "added" {
			added = append(added, c.Model)
		} else {
			removed = append(removed, c.Model)
		}
	}
	if strings.Join(added, ",") != idA+","+idB+","+idC || strings.Join(removed, ",") != idB {
		t.Fatalf("added=%v removed=%v", added, removed)
	}
	if got := strings.Join(logged, "\n"); !strings.Contains(got, "free model removed: "+idB) || !strings.Contains(got, "free model added: "+idC) {
		t.Fatalf("log lines: %q", got)
	}
}

func argString(args []any) string {
	var out []string
	for _, a := range args {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func TestCatalogOutageKeepsThePreviousListUntilItGoesStale(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{"catalog-max-age":"12h"}`)
	syncNow(t, m)
	f.mu.Lock()
	f.catCode = 503
	f.mu.Unlock()
	m.now = func() time.Time { return start.Add(6 * time.Hour) }
	syncNow(t, m)
	if s := status(t, m); s.LastSyncError != "catalog_http_503" || s.ModelCount != 1 || s.HostSync != reconcileCatalogFailed {
		t.Fatalf("status after outage: %+v", s)
	}
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	m.now = func() time.Time { return start.Add(13 * time.Hour) }
	assertPick(t, m, req(m, aliasPrefix+idA), false)
	if s := status(t, m); s.CatalogFresh {
		t.Fatal("stale catalog reported fresh")
	}
}

func TestDailyLimitFollowsTheKeyTierAndNeverExceedsAnOperatorCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		freeTier bool
		cfg      string
		want     int64
		source   string
	}{
		{"free tier", true, `{}`, 50, "openrouter_key"},
		{"purchased credits", false, `{}`, 1000, "openrouter_key"},
		{"operator cap lowers", false, `{"daily-request-limit":7}`, 7, "operator_cap"},
		{"operator cap cannot raise", true, `{"daily-request-limit":5000}`, 50, "openrouter_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(idA)
			f.freeTier = tc.freeTier
			m := setup(t, hostWith(t, f, entry(idA)), tc.cfg)
			syncNow(t, m)
			if s := status(t, m); s.DailyLimit != tc.want || s.DailyLimitSource != tc.source {
				t.Fatalf("limit = %d (%s), want %d (%s)", s.DailyLimit, s.DailyLimitSource, tc.want, tc.source)
			}
		})
	}
}

func TestUnknownTierAssumesTheLowestLimit(t *testing.T) {
	f := newFake(idA)
	f.keyCode = 500
	m := setup(t, hostWith(t, f, entry(idA)), `{}`)
	syncNow(t, m)
	if s := status(t, m); s.DailyLimit != 50 || s.DailyLimitSource != "assumed_lowest" {
		t.Fatalf("status: %+v", s)
	}
}

func TestDailyLimitIsEnforcedAndResetsAtUTCMidnight(t *testing.T) {
	f := newFake(idA)
	f.freeTier = true
	m := setup(t, hostWith(t, f, entry(idA)), `{"daily-request-limit":3,"requests-per-minute":20}`)
	syncNow(t, m)
	r := req(m, aliasPrefix+idA)
	for i := range 3 {
		m.now = func() time.Time { return start.Add(time.Duration(i) * 2 * time.Minute) }
		assertPick(t, m, r, true)
	}
	m.now = func() time.Time { return start.Add(10 * time.Minute) }
	assertPick(t, m, r, false)
	m.now = func() time.Time { return time.Date(2026, 10, 26, 0, 0, 1, 0, time.UTC) }
	f.mu.Lock()
	f.catalog = catalogJSON(idA)
	f.mu.Unlock()
	syncNow(t, m) // keep the catalog fresh across the day boundary
	assertPick(t, m, r, true)
	if s := status(t, m); s.UsedToday != 1 {
		t.Fatalf("used today = %d", s.UsedToday)
	}
}

func TestRateLimitIsAtomicAcrossConcurrentPicks(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{"requests-per-minute":4}`)
	syncNow(t, m)
	r := req(m, aliasPrefix+idA)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, e := m.Pick(context.Background(), r); e == nil && p.Handled {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 4 {
		t.Fatalf("admitted %d, want exactly 4", ok)
	}
	m.now = func() time.Time { return start.Add(time.Minute) }
	assertPick(t, m, r, true)
}

func TestStayWithinFreeOffSkipsOnlyTheLocalLimits(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{"stayWithinFree":false,"requests-per-minute":1,"daily-request-limit":1}`)
	syncNow(t, m)
	r := req(m, aliasPrefix+idA)
	assertPick(t, m, r, true)
	assertPick(t, m, r, true)
	assertPick(t, m, req(m, "anthropic/paid-model"), false)
}

func TestDailyAllowanceRefusalRestsUntilMidnightAndOtherFailuresBackOff(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{}`)
	syncNow(t, m)
	r := req(m, aliasPrefix+idA)
	id := r.Candidates[0].ID
	rec := pluginapi.UsageRecord{AuthID: id, Failed: true}
	rec.Failure.StatusCode, rec.Failure.Body = 429, `{"error":{"message":"Rate limit exceeded: free-models-per-day"}}`
	if err := m.HandleUsage(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	s := status(t, m)
	if s.CooldownUntil == nil || !s.CooldownUntil.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cooldown = %v", s.CooldownUntil)
	}
	assertPick(t, m, r, false)

	m2 := setup(t, hostWith(t, f, entry(idA)), `{}`)
	syncNow(t, m2)
	rec.AuthID = m2.OwnedAuthIDs(context.Background())[0]
	rec.Failure.StatusCode, rec.Failure.Body = 402, ""
	if err := m2.HandleUsage(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if s := status(t, m2); s.CooldownUntil == nil || s.CooldownUntil.Sub(start) < time.Hour {
		t.Fatalf("402 cooldown = %v", s.CooldownUntil)
	}
}

func TestVerifiedListAndCountersSurviveARestart(t *testing.T) {
	f := newFake(idA)
	host := hostWith(t, f, entry(idA))
	m := setup(t, host, `{}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := setup(t, host, `{}`)
	if s := status(t, again); s.ModelCount != 1 || s.UsedToday != 1 {
		t.Fatalf("after restart: %+v", s)
	}
	assertPick(t, again, req(again, aliasPrefix+idA), true)
}

func TestReconcilePatchesOnlyModelsThroughTheManagementAPI(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(idB, idA)
	host := hostWith(t, f, entry("vendor/stale:free"))
	m := setup(t, host, `{"management-key-file":"`+keyFile+`","management-url":"http://127.0.0.1:9999/"}`)
	syncNow(t, m)
	patches := f.patches()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(patches))
	}
	p := patches[0]
	if p.url != "http://127.0.0.1:9999/v0/management/openai-compatibility" || p.auth != "Bearer test-only-management-key" {
		t.Fatalf("patch target/auth: %s %q", p.url, p.auth)
	}
	var body struct {
		Name  string                         `json:"name"`
		Value map[string][]map[string]string `json:"value"`
	}
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(p.body), &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(p.body), &body); err != nil {
		t.Fatal(err)
	}
	if len(raw["value"].(map[string]any)) != 1 || body.Name != providerID {
		t.Fatalf("patch must carry only models: %s", p.body)
	}
	got := body.Value["models"]
	if len(got) != 2 || got[0]["name"] != idA || got[0]["alias"] != aliasPrefix+idA || got[1]["name"] != idB {
		t.Fatalf("models = %v", got)
	}
	if strings.Contains(p.body, "key") {
		t.Fatalf("patch leaked credential material: %s", p.body)
	}
	if s := status(t, m); s.HostSync != reconcileSynced || s.HostSyncedAt == nil {
		t.Fatalf("status: %+v", s)
	}
	// The host reloads with the reconciled list: the next pass must not patch again.
	host.OpenAICompatibility = []providermodule.OpenAICompatibility{entry(idA, idB)}
	if err := m.Reconfigure(context.Background(), host, json.RawMessage(`{"management-key-file":"`+keyFile+`"}`)); err != nil {
		t.Fatal(err)
	}
	syncNow(t, m)
	if len(f.patches()) != 1 {
		t.Fatalf("redundant patch: %d", len(f.patches()))
	}
	if s := status(t, m); s.HostSync != reconcileInSync {
		t.Fatalf("host sync = %s", s.HostSync)
	}
}

func TestReconcileReportsMissingPrerequisitesWithoutPatching(t *testing.T) {
	for _, tc := range []struct {
		name, cfg, want string
	}{
		{"no key file configured", `{}`, reconcileNoKey},
		{"key file missing", `{"management-key-file":"/nonexistent/mgmt.key"}`, reconcileNoKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(idA)
			m := setup(t, hostWith(t, f, entry()), tc.cfg)
			syncNow(t, m)
			if s := status(t, m); s.HostSync != tc.want || s.ModelCount != 1 {
				t.Fatalf("status: %+v", s)
			}
			if len(f.patches()) != 0 {
				t.Fatal("patched without a management key")
			}
		})
	}
}

func TestManagementFailureIsReportedAndRetriedNextPass(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(idA)
	f.patchOK = 401
	m := setup(t, hostWith(t, f, entry()), `{"management-key-file":"`+keyFile+`"}`)
	syncNow(t, m)
	if s := status(t, m); s.HostSync != "management_http_401" {
		t.Fatalf("host sync = %s", s.HostSync)
	}
	f.mu.Lock()
	f.patchOK = 200
	f.mu.Unlock()
	syncNow(t, m)
	if s := status(t, m); s.HostSync != reconcileSynced || len(f.patches()) != 2 {
		t.Fatalf("host sync = %s, patches = %d", s.HostSync, len(f.patches()))
	}
}

func TestInactiveWithoutAnEnabledHostEntryAndNoSyncTraffic(t *testing.T) {
	f := newFake(idA)
	e := entry(idA)
	e.Disabled = true
	m := setup(t, hostWith(t, f, e), `{}`)
	if ids := m.OwnedAuthIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("owned = %v", ids)
	}
	syncNow(t, m)
	if len(f.calls) != 0 {
		t.Fatalf("inactive module called out: %v", f.calls)
	}
	if s := status(t, m); s.Status != "inactive" {
		t.Fatalf("status = %s", s.Status)
	}
}

func TestSyncLoopRunsOnStartAndStopsOnClose(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(idA)
	m := NewModule()
	m.logf = func(string, ...any) {}
	if err := m.Reconfigure(context.Background(), hostWith(t, f, entry()), json.RawMessage(`{"management-key-file":"`+keyFile+`"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.patched:
	case <-time.After(10 * time.Second):
		t.Fatal("no sync pass after start")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{
		`{"requests-per-minute":21}`, `{"requests-per-minute":0}`, `{"daily-request-limit":-1}`,
		`{"sync-interval":"1m"}`, `{"sync-interval":"nope"}`, `{"sync-interval":"6h","catalog-max-age":"1h"}`,
		`{"management-url":"https://evil.example"}`, `{"management-url":"http://10.0.0.5:8317"}`,
		`{"stayWithinFree":true,"stay-within-free":true}`, `{"unknown":1}`, `{} {}`,
	} {
		if _, err := parseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("config %s accepted", bad)
		}
	}
	cfg, err := parseConfig(nil)
	if err != nil || cfg.RequestsPerMinute != 20 || cfg.SyncInterval != 6*time.Hour || !cfg.StayWithinFree || !cfg.Enabled {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	if _, err := parseConfig(json.RawMessage(`{"management-url":"http://localhost:8317","sync-interval":"30m"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogWithNoFreeModelsKeepsThePreviousListAndNeverWipesTheHost(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{"management-key-file":"`+keyFile+`"}`)
	syncNow(t, m)
	f.mu.Lock()
	f.catalog = catalogJSON() // parses, but nothing survives the free filter
	f.mu.Unlock()
	m.now = func() time.Time { return start.Add(6 * time.Hour) }
	syncNow(t, m)
	if s := status(t, m); s.LastSyncError != "catalog_no_free_models" || s.ModelCount != 1 || s.HostSync != reconcileCatalogFailed {
		t.Fatalf("status: %+v", s)
	}
	if len(f.patches()) != 0 {
		t.Fatal("host list was patched from an empty free list")
	}
	assertPick(t, m, req(m, aliasPrefix+idA), true)
}

func TestStaleCatalogRefusesEvenWithTheLocalLimitsOff(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{"stayWithinFree":false}`)
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	m.now = func() time.Time { return start.Add(25 * time.Hour) }
	assertPick(t, m, req(m, aliasPrefix+idA), false)
}

func TestKeyDailyLimitUsesReportedCeiling(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int64
	}{
		{"paid tier below threshold", `{"data":{"is_free_tier":false,"free_model_daily_requests":{"limit":50}}}`, 50},
		{"reported higher limit", `{"data":{"is_free_tier":false,"free_model_daily_requests":{"limit":1000}}}`, 1000},
		{"missing ceiling", `{"data":{"is_free_tier":false}}`, 50},
		{"invalid ceiling", `{"data":{"is_free_tier":false,"free_model_daily_requests":{"limit":-1}}}`, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(idA)
			f.keyBody = tc.body
			got, err := fetchDailyLimit(context.Background(), f, "test-key")
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestReconcileAcceptsNoContent(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(idA)
	f.patchOK = http.StatusNoContent
	m := setup(t, hostWith(t, f, entry()), `{"management-key-file":"`+keyFile+`"}`)
	syncNow(t, m)
	if s := status(t, m); s.HostSync != reconcileSynced || s.HostSyncedAt == nil {
		t.Fatalf("status: %+v", s)
	}
}

func TestUnavailableKeyLimitFallsBackAfterPreviouslyHigherLimit(t *testing.T) {
	f := newFake(idA)
	m := setup(t, hostWith(t, f, entry(idA)), `{}`)
	syncNow(t, m)
	if s := status(t, m); s.DailyLimit != 1000 {
		t.Fatalf("initial limit = %d, want 1000", s.DailyLimit)
	}
	f.mu.Lock()
	f.keyCode = http.StatusInternalServerError
	f.mu.Unlock()
	syncNow(t, m)
	if s := status(t, m); s.DailyLimit != 50 || s.DailyLimitSource != "assumed_lowest" {
		t.Fatalf("status after key endpoint failure: %+v", s)
	}
}
