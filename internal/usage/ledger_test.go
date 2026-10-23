package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var base = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func record(at time.Time, model string, out int64, failed int) pluginapi.UsageRecord {
	r := pluginapi.UsageRecord{
		Alias:       model,
		Model:       "upstream/" + model,
		RequestedAt: at,
		Latency:     12 * time.Second,
		TTFT:        2 * time.Second,
		Detail:      pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: out, CachedTokens: 400},
	}
	if failed != 0 {
		r.Failed = true
		r.Failure = pluginapi.UsageFailure{StatusCode: failed}
	}
	return r
}

func TestFromRecordCountsTokensLatencyAndSpeed(t *testing.T) {
	c := FromRecord(record(base, "m", 100, 0))
	if c.Requests != 1 || c.Failures != 0 || c.InputTokens != 1000 || c.OutputTokens != 100 || c.CachedTokens != 400 || c.TotalTokens != 1100 {
		t.Fatalf("counters = %+v", c)
	}
	if c.LatencyMillis != 12000 || c.TTFTMillis != 2000 || c.GenTokens != 100 || c.GenMillis != 10000 {
		t.Fatalf("timing = %+v", c)
	}
	for status, want := range map[int]Counters{429: {Status429: 1}, 503: {Status5xx: 1}, 400: {Status4xx: 1}} {
		got := FromRecord(record(base, "m", 0, status))
		if got.Failures != 1 || got.Status429 != want.Status429 || got.Status5xx != want.Status5xx || got.Status4xx != want.Status4xx {
			t.Fatalf("status %d counters = %+v", status, got)
		}
	}
	if short := FromRecord(record(base, "m", 5, 0)); short.GenTokens != 0 {
		t.Fatalf("a 5-token response must not count toward output speed: %+v", short)
	}
}

func TestQueryBucketsByRangeAndKey(t *testing.T) {
	l := New()
	now := base.Add(59 * time.Minute)
	l.Record("synthetic", "synthetic#1", record(base.Add(1*time.Minute), "synthetic/glm-5.3", 100, 0), now)
	l.Record("synthetic", "synthetic#1", record(base.Add(2*time.Minute), "synthetic/glm-5.3", 100, 429), now)
	l.Record("synthetic", "synthetic-infinite#1", record(base.Add(31*time.Minute), "synthetic/deepseek-v4.1-flash", 300, 0), now)
	l.Record("zai", "zai-pro-1", record(base.Add(-3*time.Hour), "glm-5.3", 50, 0), now)

	report := l.Query(RangeByID("1h"), now)
	if report.Range != "1h" || report.StepSeconds != 300 || len(report.Buckets) != 12 {
		t.Fatalf("1h report shape = %s %d %d", report.Range, report.StepSeconds, len(report.Buckets))
	}
	if !report.Buckets[0].Equal(base) || !report.To.Equal(base.Add(time.Hour)) {
		t.Fatalf("1h buckets start %v end %v", report.Buckets[0], report.To)
	}
	if len(report.Totals) != 2 {
		t.Fatalf("1h totals = %+v (the 3h-old Z.ai record is out of range)", report.Totals)
	}
	paid := report.Totals[0]
	if paid.Account != "synthetic#1" || paid.Model != "synthetic/glm-5.3" || paid.Requests != 2 || paid.Failures != 1 || paid.Status429 != 1 {
		t.Fatalf("paid total = %+v", paid)
	}
	var infinite Series
	for _, s := range report.Series {
		if s.Account == "synthetic-infinite#1" {
			infinite = s
		}
	}
	if infinite.Requests[6] != 1 || infinite.OutputTokens[6] != 300 {
		t.Fatalf("infinite series = %+v, want the request in bucket 6 (12:30)", infinite)
	}

	day := l.Query(RangeByID("24h"), now)
	if len(day.Totals) != 3 || day.StepSeconds != 1800 || len(day.Buckets) != 48 {
		t.Fatalf("24h report = %d totals, step %d, %d buckets", len(day.Totals), day.StepSeconds, len(day.Buckets))
	}
	week := l.Query(RangeByID("7d"), now)
	if len(week.Totals) != 3 || week.StepSeconds != 6*3600 || len(week.Buckets) != 28 {
		t.Fatalf("7d report = %d totals, step %d, %d buckets", len(week.Totals), week.StepSeconds, len(week.Buckets))
	}
	if RangeByID("bogus").ID != DefaultRange {
		t.Fatal("unknown range must fall back to the default")
	}
}

func TestRecordIgnoresFutureAndExpiredRecords(t *testing.T) {
	l := New()
	l.Record("zai", "a", record(base.Add(time.Hour), "m", 1, 0), base)
	l.Record("zai", "a", record(base.Add(-9*24*time.Hour), "m", 1, 0), base)
	if got := l.Query(RangeByID("7d"), base); len(got.Totals) != 0 || got.Records != 0 {
		t.Fatalf("report = %+v", got)
	}
}

func TestRowCardinalityIsBounded(t *testing.T) {
	l := New()
	for i := 0; i < maxRowsPerBucket+50; i++ {
		l.Record("opencode-go", "go-1", record(base, "model-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), 1, 0), base)
	}
	report := l.Query(RangeByID("1h"), base)
	if len(report.Totals) > maxRowsPerBucket+1 {
		t.Fatalf("rows = %d, want at most %d", len(report.Totals), maxRowsPerBucket+1)
	}
	var overflow int64
	for _, total := range report.Totals {
		if total.Model == OverflowLabel {
			overflow = total.Requests
		}
	}
	if overflow != 50 {
		t.Fatalf("overflow requests = %d, want 50", overflow)
	}
}

func TestStateRoundTrip(t *testing.T) {
	l := New()
	now := base.Add(10 * time.Minute)
	l.Record("synthetic", "synthetic#1", record(base, "synthetic/kimi-k3", 40, 0), now)
	if !l.Dirty() {
		t.Fatal("ledger not dirty after Record")
	}
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	if l.Dirty() {
		t.Fatal("MarshalState must clear dirty")
	}
	restored := New()
	if err := restored.UnmarshalState(raw, now); err != nil {
		t.Fatal(err)
	}
	got := restored.Query(RangeByID("1h"), now)
	if len(got.Totals) != 1 || got.Totals[0].OutputTokens != 40 || got.Records != 1 || got.Since == nil {
		t.Fatalf("restored report = %+v", got)
	}
	if err := restored.UnmarshalState([]byte(`{"version":9}`), now); err == nil {
		t.Fatal("unknown state version accepted")
	}
}
