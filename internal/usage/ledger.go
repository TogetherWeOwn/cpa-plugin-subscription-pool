// Package usage keeps the subscription-pool plugin's own request and token
// history for the usage page: every usage record a hosted module accepts is
// counted per provider, account and model in fixed time buckets.
//
// Two tiers bound memory and disk: 5-minute buckets for the last 48 hours
// and 1-hour buckets for the last 8 days. Each bucket holds at most
// maxRowsPerBucket rows; anything past that is folded into one overflow row
// per provider, so a flood of distinct model names cannot grow the ledger
// without limit. The ledger stores no credential, auth ID, client key or
// request content: only provider IDs, account display names, model names,
// counters and durations.
package usage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// FineStep and FineRetention describe the short-range tier.
	FineStep      = 5 * time.Minute
	FineRetention = 48 * time.Hour
	// CoarseStep and CoarseRetention describe the long-range tier.
	CoarseStep      = time.Hour
	CoarseRetention = 8 * 24 * time.Hour

	maxRowsPerBucket = 200
	maxLabel         = 120
	// OverflowLabel names the row that absorbs keys past maxRowsPerBucket.
	OverflowLabel = "(other)"

	// minGeneration and minGenerationTokens skip responses too short to
	// give a meaningful output speed (for example one buffered chunk).
	minGeneration       = 100 * time.Millisecond
	minGenerationTokens = 20

	stateVersion = 1
)

// Key identifies one ledger row.
type Key struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Model    string `json:"model"`
}

// Counters are additive per-row totals.
type Counters struct {
	Requests        int64 `json:"requests"`
	Failures        int64 `json:"failures"`
	Status429       int64 `json:"status_429"`
	Status4xx       int64 `json:"status_4xx"`
	Status5xx       int64 `json:"status_5xx"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	LatencyMillis   int64 `json:"latency_ms_sum"`
	LatencyCount    int64 `json:"latency_count"`
	TTFTMillis      int64 `json:"ttft_ms_sum"`
	TTFTCount       int64 `json:"ttft_count"`
	GenTokens       int64 `json:"gen_tokens"`
	GenMillis       int64 `json:"gen_ms_sum"`
}

// Add accumulates other into c.
func (c *Counters) Add(other Counters) {
	c.Requests += other.Requests
	c.Failures += other.Failures
	c.Status429 += other.Status429
	c.Status4xx += other.Status4xx
	c.Status5xx += other.Status5xx
	c.InputTokens += other.InputTokens
	c.OutputTokens += other.OutputTokens
	c.ReasoningTokens += other.ReasoningTokens
	c.CachedTokens += other.CachedTokens
	c.TotalTokens += other.TotalTokens
	c.LatencyMillis += other.LatencyMillis
	c.LatencyCount += other.LatencyCount
	c.TTFTMillis += other.TTFTMillis
	c.TTFTCount += other.TTFTCount
	c.GenTokens += other.GenTokens
	c.GenMillis += other.GenMillis
}

// FromRecord converts one usage record into counters.
func FromRecord(record pluginapi.UsageRecord) Counters {
	c := Counters{Requests: 1}
	if record.Failed {
		c.Failures = 1
		switch status := record.Failure.StatusCode; {
		case status == 429:
			c.Status429 = 1
		case status >= 500:
			c.Status5xx = 1
		case status >= 400:
			c.Status4xx = 1
		}
	}
	detail := record.Detail
	c.InputTokens = nonNegative(detail.InputTokens)
	c.OutputTokens = nonNegative(detail.OutputTokens)
	c.ReasoningTokens = nonNegative(detail.ReasoningTokens)
	c.CachedTokens = nonNegative(detail.CachedTokens)
	if c.CachedTokens == 0 {
		c.CachedTokens = nonNegative(detail.CacheReadTokens)
	}
	c.TotalTokens = nonNegative(detail.TotalTokens)
	if c.TotalTokens == 0 {
		c.TotalTokens = c.InputTokens + c.OutputTokens
	}
	if record.Latency > 0 {
		c.LatencyMillis = record.Latency.Milliseconds()
		c.LatencyCount = 1
	}
	if record.TTFT > 0 {
		c.TTFTMillis = record.TTFT.Milliseconds()
		c.TTFTCount = 1
	}
	if !record.Failed && c.OutputTokens >= minGenerationTokens {
		if generation := record.Latency - record.TTFT; generation >= minGeneration {
			c.GenTokens = c.OutputTokens
			c.GenMillis = generation.Milliseconds()
		}
	}
	return c
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

type tier struct {
	step      time.Duration
	retention time.Duration
	buckets   map[int64]map[Key]*Counters
}

func newTier(step, retention time.Duration) tier {
	return tier{step: step, retention: retention, buckets: make(map[int64]map[Key]*Counters)}
}

func (t *tier) add(at time.Time, key Key, delta Counters) {
	start := at.Truncate(t.step).Unix()
	rows := t.buckets[start]
	if rows == nil {
		rows = make(map[Key]*Counters)
		t.buckets[start] = rows
	}
	row := rows[key]
	if row == nil {
		if len(rows) >= maxRowsPerBucket {
			key = Key{Provider: key.Provider, Account: OverflowLabel, Model: OverflowLabel}
			row = rows[key]
		}
		if row == nil {
			row = &Counters{}
			rows[key] = row
		}
	}
	row.Add(delta)
}

func (t *tier) prune(now time.Time) {
	oldest := now.Add(-t.retention).Truncate(t.step).Unix()
	for start := range t.buckets {
		if start < oldest {
			delete(t.buckets, start)
		}
	}
}

// Ledger is safe for concurrent use.
type Ledger struct {
	mu      sync.Mutex
	fine    tier
	coarse  tier
	since   time.Time
	records int64
	dirty   bool
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{fine: newTier(FineStep, FineRetention), coarse: newTier(CoarseStep, CoarseRetention)}
}

// Record counts one usage record against provider/account/model at the
// record's request time (now when the record carries none). Records older
// than the coarse retention or more than one fine step in the future are
// ignored.
func (l *Ledger) Record(provider, account string, record pluginapi.UsageRecord, now time.Time) {
	at := record.RequestedAt.UTC()
	if at.IsZero() {
		at = now.UTC()
	}
	if at.After(now.Add(FineStep)) || at.Before(now.Add(-CoarseRetention)) {
		return
	}
	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	key := Key{Provider: clean(provider, "unknown"), Account: clean(account, "unattributed"), Model: clean(model, "unknown")}
	delta := FromRecord(record)

	l.mu.Lock()
	defer l.mu.Unlock()
	if !at.Before(now.Add(-FineRetention)) {
		l.fine.add(at, key, delta)
	}
	l.coarse.add(at, key, delta)
	if l.since.IsZero() || at.Before(l.since) {
		l.since = at
	}
	l.records++
	l.dirty = true
	l.fine.prune(now)
	l.coarse.prune(now)
}

func clean(value, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	if len(value) > maxLabel {
		value = value[:maxLabel]
	}
	return value
}

// Range is one selectable time range of the usage page.
type Range struct {
	ID       string
	Duration time.Duration
	Step     time.Duration
}

// Ranges lists the usage page's time ranges, shortest first.
var Ranges = []Range{
	{ID: "1h", Duration: time.Hour, Step: 5 * time.Minute},
	{ID: "6h", Duration: 6 * time.Hour, Step: 15 * time.Minute},
	{ID: "24h", Duration: 24 * time.Hour, Step: 30 * time.Minute},
	{ID: "7d", Duration: 7 * 24 * time.Hour, Step: 6 * time.Hour},
}

// DefaultRange is used when the client asks for none or an unknown one.
const DefaultRange = "24h"

// RangeByID returns the named range, or the default one.
func RangeByID(id string) Range {
	for _, item := range Ranges {
		if item.ID == id {
			return item
		}
	}
	for _, item := range Ranges {
		if item.ID == DefaultRange {
			return item
		}
	}
	return Ranges[0]
}

// Series is one row's per-bucket values; index i matches Report.Buckets[i].
type Series struct {
	Key
	Requests     []int64 `json:"requests"`
	Failures     []int64 `json:"failures"`
	InputTokens  []int64 `json:"input_tokens"`
	OutputTokens []int64 `json:"output_tokens"`
}

// Total is one row's sum over the whole range.
type Total struct {
	Key
	Counters
}

// Report is the ledger's view of one range.
type Report struct {
	Range       string      `json:"range"`
	StepSeconds int64       `json:"step_seconds"`
	From        time.Time   `json:"from"`
	To          time.Time   `json:"to"`
	Buckets     []time.Time `json:"buckets"`
	Series      []Series    `json:"series"`
	Totals      []Total     `json:"totals"`
	Since       *time.Time  `json:"since,omitempty"`
	Records     int64       `json:"records_since_start"`
}

// Query aggregates the ledger over rng ending at now. The returned buckets
// are rng.Step wide and aligned to rng.Step; the last one contains now.
func (l *Ledger) Query(rng Range, now time.Time) Report {
	now = now.UTC()
	step := rng.Step
	last := now.Truncate(step)
	count := int(rng.Duration / step)
	if count < 1 {
		count = 1
	}
	first := last.Add(-time.Duration(count-1) * step)
	report := Report{
		Range:       rng.ID,
		StepSeconds: int64(step / time.Second),
		From:        first,
		To:          last.Add(step),
		Buckets:     make([]time.Time, count),
		Series:      []Series{},
		Totals:      []Total{},
	}
	for i := range report.Buckets {
		report.Buckets[i] = first.Add(time.Duration(i) * step)
	}

	l.mu.Lock()
	source := &l.coarse
	if rng.Duration <= FineRetention && step%FineStep == 0 {
		source = &l.fine
	}
	series := make(map[Key]*Series)
	totals := make(map[Key]*Counters)
	for start, rows := range source.buckets {
		at := time.Unix(start, 0).UTC()
		if at.Before(first) || !at.Before(report.To) {
			continue
		}
		index := int(at.Sub(first) / step)
		if index < 0 || index >= count {
			continue
		}
		for key, row := range rows {
			s := series[key]
			if s == nil {
				s = &Series{Key: key, Requests: make([]int64, count), Failures: make([]int64, count), InputTokens: make([]int64, count), OutputTokens: make([]int64, count)}
				series[key] = s
				totals[key] = &Counters{}
			}
			s.Requests[index] += row.Requests
			s.Failures[index] += row.Failures
			s.InputTokens[index] += row.InputTokens
			s.OutputTokens[index] += row.OutputTokens
			totals[key].Add(*row)
		}
	}
	if !l.since.IsZero() {
		since := l.since
		report.Since = &since
	}
	report.Records = l.records
	l.mu.Unlock()

	keys := make([]Key, 0, len(series))
	for key := range series {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := totals[keys[i]].Requests, totals[keys[j]].Requests
		if left != right {
			return left > right
		}
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		if keys[i].Account != keys[j].Account {
			return keys[i].Account < keys[j].Account
		}
		return keys[i].Model < keys[j].Model
	})
	for _, key := range keys {
		report.Series = append(report.Series, *series[key])
		report.Totals = append(report.Totals, Total{Key: key, Counters: *totals[key]})
	}
	return report
}

// persisted is the on-disk shape. Rows are stored as arrays to keep the
// file small: [provider, account, model, counters].
type persisted struct {
	Version int             `json:"version"`
	Since   *time.Time      `json:"since,omitempty"`
	Records int64           `json:"records"`
	Fine    []persistBucket `json:"fine"`
	Coarse  []persistBucket `json:"coarse"`
}

type persistBucket struct {
	Start int64        `json:"t"`
	Rows  []persistRow `json:"rows"`
}

type persistRow struct {
	Key
	Counters
}

// Dirty reports whether the ledger changed since the last MarshalState.
func (l *Ledger) Dirty() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dirty
}

// MarshalState serializes the ledger and clears the dirty flag.
func (l *Ledger) MarshalState(now time.Time) ([]byte, error) {
	l.mu.Lock()
	l.fine.prune(now)
	l.coarse.prune(now)
	state := persisted{Version: stateVersion, Records: l.records, Fine: exportTier(l.fine), Coarse: exportTier(l.coarse)}
	if !l.since.IsZero() {
		since := l.since
		state.Since = &since
	}
	l.dirty = false
	l.mu.Unlock()
	return json.Marshal(state)
}

func exportTier(t tier) []persistBucket {
	out := make([]persistBucket, 0, len(t.buckets))
	for start, rows := range t.buckets {
		bucket := persistBucket{Start: start, Rows: make([]persistRow, 0, len(rows))}
		for key, row := range rows {
			bucket.Rows = append(bucket.Rows, persistRow{Key: key, Counters: *row})
		}
		sort.Slice(bucket.Rows, func(i, j int) bool {
			a, b := bucket.Rows[i].Key, bucket.Rows[j].Key
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			if a.Account != b.Account {
				return a.Account < b.Account
			}
			return a.Model < b.Model
		})
		out = append(out, bucket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// UnmarshalState merges a previously saved ledger into l (counters add up,
// so loading twice double-counts: load once, before any Record). Buckets
// outside the retention windows are dropped.
func (l *Ledger) UnmarshalState(raw []byte, now time.Time) error {
	var state persisted
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("usage ledger state: %w", err)
	}
	if state.Version != stateVersion {
		return fmt.Errorf("usage ledger state: unsupported version %d", state.Version)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	importTier(&l.fine, state.Fine)
	importTier(&l.coarse, state.Coarse)
	l.fine.prune(now)
	l.coarse.prune(now)
	if state.Since != nil && (l.since.IsZero() || state.Since.Before(l.since)) {
		l.since = state.Since.UTC()
	}
	l.records += state.Records
	return nil
}

func importTier(t *tier, buckets []persistBucket) {
	for _, bucket := range buckets {
		at := time.Unix(bucket.Start, 0).UTC()
		for _, row := range bucket.Rows {
			key := Key{Provider: clean(row.Provider, "unknown"), Account: clean(row.Account, "unattributed"), Model: clean(row.Model, "unknown")}
			t.add(at, key, row.Counters)
		}
	}
}
