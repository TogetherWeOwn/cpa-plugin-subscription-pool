package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/securestore"
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

func TestLabelTruncationKeepsValidUTF8AcrossPersistence(t *testing.T) {
	long := strings.Repeat("a", 119) + "é" + "zzzz"
	for _, label := range []string{long, strings.Repeat("é", 70), strings.Repeat("€", 50)} {
		got := clean(label, "x")
		if !utf8.ValidString(got) || len(got) > maxLabel {
			t.Fatalf("clean(%q) = %q (valid=%v, %d bytes)", label, got, utf8.ValidString(got), len(got))
		}
	}
	l := New()
	now := base.Add(time.Minute)
	l.Record("synthetic", long, record(base, long, 30, 0), now)
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.UnmarshalState(raw, now); err != nil {
		t.Fatal(err)
	}
	restored.Record("synthetic", long, record(base, long, 30, 0), now)
	if got := restored.Query(RangeByID("1h"), now).Totals; len(got) != 1 || got[0].Requests != 2 {
		t.Fatalf("after restart the same long label split into %d rows: %+v", len(got), got)
	}
}

func TestPersistedStateStaysUnderTheStoreLimitAtWorstCase(t *testing.T) {
	l := New()
	now := base
	big := pluginapi.UsageRecord{Latency: time.Hour, TTFT: time.Minute, Detail: pluginapi.UsageDetail{InputTokens: 1 << 40, OutputTokens: 1 << 40, ReasoningTokens: 1 << 40, CachedTokens: 1 << 40, TotalTokens: 1 << 41}, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}}
	// Every fine and coarse bucket full to the row cap with long distinct labels.
	for minutes := 0; minutes < int(CoarseRetention/time.Minute); minutes += 5 {
		at := now.Add(-time.Duration(minutes) * time.Minute)
		for row := 0; row < maxRowsPerBucket; row++ {
			big.RequestedAt = at
			big.Alias = strings.Repeat("m", 90) + string(rune('A'+row%26)) + time.Duration(row).String()
			l.Record("opencode-go", strings.Repeat("a", 100)+time.Duration(row%7).String(), big, now)
		}
	}
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= maxStateBytes || maxStateBytes >= 8<<20 {
		t.Fatalf("state = %d bytes, budget %d, store limit %d", len(raw), maxStateBytes, 8<<20)
	}
	restored := New()
	if err := restored.UnmarshalState(raw, now); err != nil {
		t.Fatal(err)
	}
	// The newest fine buckets and the coarse totals survive the budget.
	if hour := restored.Query(RangeByID("1h"), now); len(hour.Totals) == 0 {
		t.Fatal("newest fine buckets were dropped")
	}
	week := restored.Query(RangeByID("7d"), now)
	if len(week.Totals) == 0 {
		t.Fatal("coarse totals were dropped")
	}
}

func TestMarshalStateKeepsDirtyUntilMarkedSaved(t *testing.T) {
	l := New()
	l.Record("zai", "a", record(base, "m", 1, 0), base)
	_, generation, err := l.MarshalStateGeneration(base)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Dirty() {
		t.Fatal("marshal alone must not clear dirty: the write may still fail")
	}
	l.Record("zai", "a", record(base, "m", 1, 0), base)
	l.MarkSaved(generation)
	if !l.Dirty() {
		t.Fatal("a record that arrived during the write must keep the ledger dirty")
	}
	_, generation, _ = l.MarshalStateGeneration(base)
	l.MarkSaved(generation)
	if l.Dirty() {
		t.Fatal("MarkSaved with the current generation must clear dirty")
	}
}

// snapshot copies every bucket of both tiers, so persistence tests can
// compare the restored ledger with the original exactly.
func snapshot(l *Ledger) map[string]map[int64]map[Key]Counters {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]map[int64]map[Key]Counters{}
	for name, t := range map[string]*tier{"fine": &l.fine, "coarse": &l.coarse} {
		buckets := map[int64]map[Key]Counters{}
		for start, rows := range t.buckets {
			copied := map[Key]Counters{}
			for key, row := range rows {
				copied[key] = *row
			}
			buckets[start] = copied
		}
		out[name] = buckets
	}
	return out
}

func restore(t *testing.T, raw []byte, now time.Time) *Ledger {
	t.Helper()
	restored := New()
	if err := restored.UnmarshalState(raw, now); err != nil {
		t.Fatal(err)
	}
	return restored
}

// distinctCounters sets every Counters field to a distinct nonzero value
// (offset+1, offset+2, ...), so a swapped or dropped column cannot cancel out.
func distinctCounters(offset int64) Counters {
	var c Counters
	v := reflect.ValueOf(&c).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).SetInt(offset + int64(i) + 1)
	}
	return c
}

func TestCompactCountersRoundTripEveryFieldExactly(t *testing.T) {
	full := distinctCounters(100)
	if got := countersFrom(full.values()); got != full {
		t.Fatalf("countersFrom(values()) = %+v, want %+v", got, full)
	}
	// Each field alone: the trailing-zero trim must keep its column.
	fields := reflect.TypeOf(Counters{}).NumField()
	for i := 0; i < fields; i++ {
		var only Counters
		reflect.ValueOf(&only).Elem().Field(i).SetInt(7)
		if got := countersFrom(only.values()); got != only {
			t.Fatalf("field %s alone round-trips as %+v", reflect.TypeOf(Counters{}).Field(i).Name, got)
		}
	}
	if len((Counters{}).values()) != 0 {
		t.Fatal("an all-zero row must encode as an empty value list")
	}
}

func TestStateRoundTripRestoresBothTiersExactly(t *testing.T) {
	l := New()
	now := base.Add(30 * time.Minute)
	// Recorded rows: success, 429, other 4xx and 5xx, in two buckets.
	for i, status := range []int{0, 429, 404, 503} {
		l.Record("synthetic", "synthetic#1", record(base.Add(time.Duration(i)*6*time.Minute), "glm-5.3", int64(30+i), status), now)
	}
	l.Record("zai", "zai-pro-1", record(base.Add(-3*time.Hour), "glm-5.3", 70, 0), now)
	// Injected rows with every field distinct, different per tier, plus
	// trailing-zero shapes.
	l.mu.Lock()
	l.fine.add(base.Add(20*time.Minute), Key{"opencode-go", "go-1", "kimi-k3"}, distinctCounters(1000))
	l.coarse.add(base.Add(20*time.Minute), Key{"opencode-go", "go-1", "kimi-k3"}, distinctCounters(2000))
	l.fine.add(base.Add(25*time.Minute), Key{"opencode-go", "go-1", "only-requests"}, Counters{Requests: 3})
	l.coarse.add(base.Add(-2*24*time.Hour), Key{"opencode-go", "go-1", "trailing"}, Counters{Requests: 2, InputTokens: 5, GenTokens: 9})
	l.mu.Unlock()

	before := snapshot(l)
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	restored := restore(t, raw, now)
	if after := snapshot(restored); !reflect.DeepEqual(before, after) {
		t.Fatalf("restored tiers differ:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, rng := range Ranges {
		want, got := l.Query(rng, now), restored.Query(rng, now)
		if !reflect.DeepEqual(want.Totals, got.Totals) || !reflect.DeepEqual(want.Series, got.Series) || want.Records != got.Records || !want.Since.Equal(*got.Since) {
			t.Fatalf("%s report differs after restore:\nbefore %+v\nafter  %+v", rng.ID, want, got)
		}
	}
}

func TestFullBucketWithOverflowRestoresExactly(t *testing.T) {
	l := New()
	now := base.Add(time.Minute)
	for i := 0; i <= maxRowsPerBucket; i++ {
		l.Record("opencode-go", "go-1", record(base, fmt.Sprintf("model-%03d", i), int64(10+i), 0), now)
	}
	before := l.Query(RangeByID("1h"), now)
	if len(before.Totals) != maxRowsPerBucket+1 {
		t.Fatalf("rows before save = %d, want %d models + overflow", len(before.Totals), maxRowsPerBucket)
	}
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	after := restore(t, raw, now).Query(RangeByID("1h"), now)
	if !reflect.DeepEqual(before.Totals, after.Totals) {
		t.Fatalf("restore moved a tracked model into %s: %d rows before, %d after", OverflowLabel, len(before.Totals), len(after.Totals))
	}
}

// hourlyDistinctLabels records models models per hour over the whole coarse
// retention, each model name unique to its hour, as a flood of new model
// IDs would.
func hourlyDistinctLabels(l *Ledger, now time.Time, models int, label func(hour, model int) string) {
	for hour := 0; hour < int(CoarseRetention/time.Hour); hour++ {
		at := now.Add(-time.Duration(hour) * time.Hour)
		for model := 0; model < models; model++ {
			l.Record("synthetic", "synthetic#1", pluginapi.UsageRecord{
				Alias: label(hour, model), RequestedAt: at, Latency: 20 * time.Second, TTFT: time.Second,
				Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 100},
			}, now)
		}
	}
}

func TestRestoreKeepsRecentHistoryWhenEveryHourHasNewModels(t *testing.T) {
	l := New()
	now := base
	hourlyDistinctLabels(l, now, maxRowsPerBucket, func(hour, model int) string {
		return fmt.Sprintf("%s-%03d-%03d", strings.Repeat("m", 105), hour, model) // 113 bytes
	})
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxStateBytes {
		t.Fatalf("state = %d bytes, budget %d", len(raw), maxStateBytes)
	}
	restored := restore(t, raw, now)
	for _, rng := range Ranges {
		want, got := l.Query(rng, now), restored.Query(rng, now)
		if len(want.Totals) == 0 || !reflect.DeepEqual(want.Totals, got.Totals) || got.CompleteFrom != nil {
			t.Fatalf("%s after restore: %d rows (want %d), complete_from %v; the state fits, so nothing may be dropped", rng.ID, len(got.Totals), len(want.Totals), got.CompleteFrom)
		}
	}
}

func TestEscapedLabelsStayUnderTheStoreLimitAndKeepTheNewestHistory(t *testing.T) {
	l := New()
	now := base
	// '<', '>', '&', U+2028 and control bytes each encode to six JSON bytes.
	hourlyDistinctLabels(l, now, maxRowsPerBucket, func(hour, model int) string {
		return strings.Repeat("<", 100) + "&> \x01" + fmt.Sprintf("%03d%03d", hour, model)
	})
	raw, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxStateBytes {
		t.Fatalf("state = %d bytes, over the %d-byte budget (store limit %d)", len(raw), maxStateBytes, 8<<20)
	}
	// Through the real store: the write must succeed and read back.
	store, err := securestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.WriteJSON("ledger.state", json.RawMessage(raw)); err != nil {
		t.Fatalf("securestore rejected the state: %v", err)
	}
	var back json.RawMessage
	if found, err := store.ReadJSONIfExists("ledger.state", &back); err != nil || !found {
		t.Fatalf("read back = %v, %v", found, err)
	}
	restored := restore(t, back, now)
	// The newest history (every fine bucket) survives exactly.
	for _, id := range []string{"1h", "6h", "24h"} {
		want, got := l.Query(RangeByID(id), now), restored.Query(RangeByID(id), now)
		if len(want.Totals) == 0 || !reflect.DeepEqual(want.Totals, got.Totals) || got.CompleteFrom != nil {
			t.Fatalf("%s after restore: %d rows (want %d), complete_from %v", id, len(got.Totals), len(want.Totals), got.CompleteFrom)
		}
	}
	// The 7-day view lost its oldest hours and says so.
	want, got := l.Query(RangeByID("7d"), now), restored.Query(RangeByID("7d"), now)
	if got.CompleteFrom == nil || !got.CompleteFrom.After(got.From) || !got.CompleteFrom.Before(now.Add(-48*time.Hour)) {
		t.Fatalf("7d complete_from = %v, want a time between %v and 48h ago", got.CompleteFrom, got.From)
	}
	step := time.Duration(got.StepSeconds) * time.Second
	for i, start := range got.Buckets {
		if start.Before(*got.CompleteFrom) {
			continue
		}
		var wantSum, gotSum int64
		for _, s := range want.Series {
			wantSum += s.Requests[i]
		}
		for _, s := range got.Series {
			gotSum += s.Requests[i]
		}
		if wantSum != gotSum {
			t.Fatalf("7d bucket %v (step %v) after complete_from: %d requests restored, want %d", start, step, gotSum, wantSum)
		}
	}
}

func TestCoverageMarkerSurvivesTheNextSaveAndExpiresWithRetention(t *testing.T) {
	now := base
	cut := now.Add(-100 * time.Hour)
	raw := fmt.Sprintf(`{"version":2,"records":1,"coarse_from":%d,"keys":[["zai","a","m"]],"fine":[],"coarse":[{"t":%d,"r":[[0,1]]}]}`, cut.Unix(), now.Add(-time.Hour).Unix())
	l := restore(t, []byte(raw), now)
	if got := l.Query(RangeByID("7d"), now).CompleteFrom; got == nil || !got.Equal(cut) {
		t.Fatalf("7d complete_from = %v, want %v", got, cut)
	}
	if got := l.Query(RangeByID("24h"), now).CompleteFrom; got != nil {
		t.Fatalf("24h complete_from = %v; the fine tier lost nothing", got)
	}
	// A save that drops nothing new still carries the earlier loss forward.
	saved, err := l.MarshalState(now)
	if err != nil {
		t.Fatal(err)
	}
	if got := restore(t, saved, now).Query(RangeByID("7d"), now).CompleteFrom; got == nil || !got.Equal(cut) {
		t.Fatalf("complete_from after a second save = %v, want %v", got, cut)
	}
	// Once the coarse window starts after the cut, the history is complete.
	later := cut.Add(CoarseRetention + time.Hour)
	l.Record("zai", "a", record(later, "m", 1, 0), later)
	if got := l.Query(RangeByID("7d"), later).CompleteFrom; got != nil {
		t.Fatalf("complete_from = %v after the retention passed the cut", got)
	}
	if saved, _ := l.MarshalState(later); strings.Contains(string(saved), "coarse_from") {
		t.Fatal("an expired coverage marker was saved again")
	}
}

// TestByteAccountingIsExactAtEveryLimit sweeps the byte limit across a
// ledger with escaped, multibyte and control-character labels: every
// result must fit (an under-count fails here), keep a newest-first prefix
// of both tiers, and leave no room for the next bucket (an over-count
// fails the tightness check).
func TestByteAccountingIsExactAtEveryLimit(t *testing.T) {
	l := New()
	now := base
	labels := []string{"plain", "<&>\"\\", "é€😀", "ctl\x01\x1f", strings.Repeat("<", 40)}
	for hour := 0; hour < 60; hour++ {
		at := now.Add(-time.Duration(hour) * 30 * time.Minute)
		for i, label := range labels {
			l.Record("synthetic", fmt.Sprintf("lane#%d", i%2+1), record(at, fmt.Sprintf("%s-%d", label, hour%7), int64(10+hour*i), []int{0, 429, 503}[i%3]), now)
		}
	}
	full, _, err := l.marshalWithin(now, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var all persisted
	if err := json.Unmarshal(full, &all); err != nil {
		t.Fatal(err)
	}
	// The accounting reserves room for the two coverage markers' widest
	// values and over-counts one comma per non-empty array; nothing else.
	slack := 3 + 2*len(`,"coarse_from":9223372036854775807`)
	previous := 0
	for limit := 200; limit <= len(full)+slack+97; limit += 97 {
		raw, _, err := l.marshalWithin(now, limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(raw) > limit {
			t.Fatalf("limit %d: encoded %d bytes", limit, len(raw))
		}
		var got persisted
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		kept := len(got.Fine) + len(got.Coarse)
		if kept < previous {
			t.Fatalf("limit %d keeps %d buckets, fewer than a smaller limit (%d)", limit, kept, previous)
		}
		previous = kept
		// Tightness: re-encoding with the next newest dropped bucket added
		// must exceed the limit, less the fixed reserve for the two
		// coverage markers' widest values and one comma per array.
		if kept < len(all.Fine)+len(all.Coarse) {
			if grown := encodeWithNextBucket(t, l, now, got); len(grown) <= limit-slack {
				t.Fatalf("limit %d: kept %d bytes but %d bytes with the next bucket would fit", limit, len(raw), len(grown))
			}
		}
	}
	if previous != len(all.Fine)+len(all.Coarse) {
		t.Fatalf("the largest limit kept %d of %d buckets", previous, len(all.Fine)+len(all.Coarse))
	}
	if raw, _, err := l.marshalWithin(now, len(full)+slack); err != nil || !bytes.Equal(raw, full) {
		t.Fatalf("a limit of the full size plus the reserve must keep everything: %d bytes, %v", len(raw), err)
	}
}

// encodeWithNextBucket appends the newest bucket state left out (by the
// same newest-first order MarshalState uses) and re-encodes it.
func encodeWithNextBucket(t *testing.T, l *Ledger, now time.Time, state persisted) []byte {
	t.Helper()
	keptFine, keptCoarse := map[int64]bool{}, map[int64]bool{}
	for _, b := range state.Fine {
		keptFine[b.Start] = true
	}
	for _, b := range state.Coarse {
		keptCoarse[b.Start] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	best, bestFine, found := int64(0), false, false
	for start := range l.fine.buckets {
		if !keptFine[start] && (!found || start > best || (start == best && !bestFine)) {
			best, bestFine, found = start, true, true
		}
	}
	for start := range l.coarse.buckets {
		if !keptCoarse[start] && (!found || start > best) {
			best, bestFine, found = start, false, true
		}
	}
	rows := l.coarse.buckets[best]
	if bestFine {
		rows = l.fine.buckets[best]
	}
	index := map[[3]string]int64{}
	for i, key := range state.Keys {
		index[key] = int64(i)
	}
	bucket := persistBucket{Start: best}
	for _, key := range sortedKeys(rows) {
		label := [3]string{key.Provider, key.Account, key.Model}
		id, ok := index[label]
		if !ok {
			id = int64(len(state.Keys))
			index[label] = id
			state.Keys = append(state.Keys, label)
		}
		bucket.Rows = append(bucket.Rows, append([]int64{id}, rows[key].values()...))
	}
	if bestFine {
		state.Fine = append(state.Fine, bucket)
	} else {
		state.Coarse = append(state.Coarse, bucket)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
