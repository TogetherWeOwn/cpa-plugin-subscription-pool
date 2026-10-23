// Package usage keeps the subscription-pool plugin's own request and token
// history for the usage page: every usage record a hosted module accepts is
// counted per provider, account and model in fixed time buckets.
//
// Two tiers bound memory and disk: 5-minute buckets for the last 48 hours
// and 1-hour buckets for the last 8 days. Each bucket holds at most
// maxRowsPerBucket ordinary rows; anything past that is folded into one
// reserved overflow row per provider, so a flood of distinct model names
// cannot grow the ledger without limit. The ledger stores no credential,
// auth ID, client key or request content: only provider IDs, account
// display names, model names, counters and durations.
package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// FineStep and FineRetention describe the short-range tier.
	FineStep      = 5 * time.Minute
	FineRetention = 48 * time.Hour
	// CoarseStep and CoarseRetention describe the long-range tier.
	CoarseStep      = time.Hour
	CoarseRetention = 8 * 24 * time.Hour

	maxRowsPerBucket = 100
	maxLabel         = 120
	// maxStateBytes bounds the encoded ledger well under securestore's
	// 8 MiB file limit; MarshalState drops the oldest buckets to fit.
	maxStateBytes = 6 << 20
	// OverflowLabel names the row that absorbs keys past maxRowsPerBucket.
	OverflowLabel = "(other)"

	// minGeneration and minGenerationTokens skip responses too short to
	// give a meaningful output speed (for example one buffered chunk).
	minGeneration       = 100 * time.Millisecond
	minGenerationTokens = 20

	stateVersion = 2
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

func overflowKey(provider string) Key {
	return Key{Provider: provider, Account: OverflowLabel, Model: OverflowLabel}
}

func isOverflow(key Key) bool {
	return key.Account == OverflowLabel && key.Model == OverflowLabel
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
		// Overflow rows are reserved: ordinaryRows does not count them, so
		// restoring a full bucket (whose overflow row sorts first) keeps
		// every key it tracked. An overflow key maps to itself here.
		if ordinaryRows(rows) >= maxRowsPerBucket {
			key = overflowKey(key.Provider)
			row = rows[key]
		}
		if row == nil {
			row = &Counters{}
			rows[key] = row
		}
	}
	row.Add(delta)
}

func ordinaryRows(rows map[Key]*Counters) int {
	count := len(rows)
	if count < maxRowsPerBucket {
		return count
	}
	for key := range rows {
		if isOverflow(key) {
			count--
		}
	}
	return count
}

// oldest is the start of the oldest bucket the tier retains at now.
func (t *tier) oldest(now time.Time) time.Time {
	return now.Add(-t.retention).Truncate(t.step)
}

func (t *tier) prune(now time.Time) {
	oldest := t.oldest(now).Unix()
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
	// generation counts changes, so a save can clear dirty only when no
	// record arrived between its marshal and its successful write.
	generation uint64
	// fineFrom and coarseFrom are set when a restored state had dropped
	// that tier's older buckets to fit the store: the tier's history is
	// complete only from that instant. Zero means complete.
	fineFrom   time.Time
	coarseFrom time.Time
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
	l.generation++
	l.pruneLocked(now)
}

// pruneLocked drops buckets past each tier's retention, and a coverage
// marker once every retained bucket is newer than it.
func (l *Ledger) pruneLocked(now time.Time) {
	l.fine.prune(now)
	l.coarse.prune(now)
	if !l.fineFrom.IsZero() && !l.fineFrom.After(l.fine.oldest(now)) {
		l.fineFrom = time.Time{}
	}
	if !l.coarseFrom.IsZero() && !l.coarseFrom.After(l.coarse.oldest(now)) {
		l.coarseFrom = time.Time{}
	}
}

func clean(value, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	if len(value) > maxLabel {
		// Cut on a rune boundary: a split multibyte rune would be invalid
		// UTF-8 in memory but U+FFFD after a JSON round trip, splitting one
		// label into two rows across a restart.
		cut := 0
		for index, r := range value {
			if index+utf8.RuneLen(r) > maxLabel {
				break
			}
			cut = index + utf8.RuneLen(r)
		}
		value = value[:cut]
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "\uFFFD")
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
	// CompleteFrom is set when this range starts before the point from
	// which the ledger's history is complete: an earlier save had to drop
	// older buckets to stay under the store limit, so buckets before
	// CompleteFrom undercount.
	CompleteFrom *time.Time `json:"complete_from,omitempty"`
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
	source, completeFrom := &l.coarse, l.coarseFrom
	if rng.Duration <= FineRetention && step%FineStep == 0 {
		source, completeFrom = &l.fine, l.fineFrom
	}
	if completeFrom.After(first) {
		value := completeFrom
		report.CompleteFrom = &value
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

// persisted is the on-disk shape (version 2). Labels are stored once in
// Keys; each row is [key index, counters...] in values() order with
// trailing zeros trimmed, which keeps a full ledger to a few MiB. FineFrom
// and CoarseFrom (Unix seconds) mark where each tier's history becomes
// complete when older buckets had to be dropped.
type persisted struct {
	Version    int             `json:"version"`
	Since      *time.Time      `json:"since,omitempty"`
	Records    int64           `json:"records"`
	FineFrom   int64           `json:"fine_from,omitempty"`
	CoarseFrom int64           `json:"coarse_from,omitempty"`
	Keys       [][3]string     `json:"keys"`
	Fine       []persistBucket `json:"fine"`
	Coarse     []persistBucket `json:"coarse"`
}

type persistBucket struct {
	Start int64     `json:"t"`
	Rows  [][]int64 `json:"r"`
}

func (c Counters) values() []int64 {
	out := []int64{c.Requests, c.Failures, c.Status429, c.Status4xx, c.Status5xx, c.InputTokens, c.OutputTokens,
		c.ReasoningTokens, c.CachedTokens, c.TotalTokens, c.LatencyMillis, c.LatencyCount, c.TTFTMillis, c.TTFTCount,
		c.GenTokens, c.GenMillis}
	for len(out) > 0 && out[len(out)-1] == 0 {
		out = out[:len(out)-1]
	}
	return out
}

func countersFrom(values []int64) Counters {
	get := func(i int) int64 {
		if i < len(values) && values[i] > 0 {
			return values[i]
		}
		return 0
	}
	return Counters{Requests: get(0), Failures: get(1), Status429: get(2), Status4xx: get(3), Status5xx: get(4),
		InputTokens: get(5), OutputTokens: get(6), ReasoningTokens: get(7), CachedTokens: get(8), TotalTokens: get(9),
		LatencyMillis: get(10), LatencyCount: get(11), TTFTMillis: get(12), TTFTCount: get(13), GenTokens: get(14), GenMillis: get(15)}
}

// Dirty reports whether the ledger changed since the last MarkSaved.
func (l *Ledger) Dirty() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dirty
}

// MarshalState serializes the ledger without changing its dirty state.
func (l *Ledger) MarshalState(now time.Time) ([]byte, error) {
	raw, _, err := l.MarshalStateGeneration(now)
	return raw, err
}

// MarshalStateGeneration serializes the ledger and returns the generation it
// captured; pass it to MarkSaved once the bytes are durably written.
//
// The encoded state stays under maxStateBytes. Every byte is counted as
// encoding/json writes it (escaped labels, exact digits), and buckets of
// both tiers are kept newest first, so trimming only ever drops the oldest
// history: the 1h, 6h and 24h views keep their newest fine buckets before
// any older coarse bucket. Where a tier lost buckets, the state records the
// instant its history is complete from, and Query reports it.
func (l *Ledger) MarshalStateGeneration(now time.Time) ([]byte, uint64, error) {
	return l.marshalWithin(now, maxStateBytes)
}

// marshalWithin is MarshalStateGeneration with the byte limit as a
// parameter, so tests can check the accounting at any size.
func (l *Ledger) marshalWithin(now time.Time, limit int) ([]byte, uint64, error) {
	l.mu.Lock()
	l.pruneLocked(now)
	state := persisted{Version: stateVersion, Records: l.records, Keys: [][3]string{}, Fine: []persistBucket{}, Coarse: []persistBucket{}}
	if !l.since.IsZero() {
		since := l.since
		state.Since = &since
	}
	// The header with empty arrays and the widest possible markers: each
	// kept key, bucket and row then adds its own bytes plus one comma.
	widest := state
	widest.FineFrom, widest.CoarseFrom = math.MaxInt64, math.MaxInt64
	header, err := json.Marshal(widest)
	if err != nil {
		l.mu.Unlock()
		return nil, 0, err
	}
	budget := limit - len(header)

	type candidate struct {
		fine  bool
		start int64
	}
	order := make([]candidate, 0, len(l.fine.buckets)+len(l.coarse.buckets))
	for start := range l.fine.buckets {
		order = append(order, candidate{fine: true, start: start})
	}
	for start := range l.coarse.buckets {
		order = append(order, candidate{start: start})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].start != order[j].start {
			return order[i].start > order[j].start
		}
		return order[i].fine && !order[j].fine
	})

	index := make(map[Key]int64)
	var digits []byte
	numberLen := func(value int64) int {
		digits = strconv.AppendInt(digits[:0], value, 10)
		return len(digits)
	}
	kept := 0
	for ; kept < len(order); kept++ {
		item := order[kept]
		rows := l.coarse.buckets[item.start]
		if item.fine {
			rows = l.fine.buckets[item.start]
		}
		bucket := persistBucket{Start: item.start, Rows: make([][]int64, 0, len(rows))}
		// The bucket with its separator, plus the commas between its rows.
		cost := len(`{"t":,"r":[]},`) + numberLen(item.start)
		if len(rows) > 1 {
			cost += len(rows) - 1
		}
		var added [][3]string
		for _, key := range sortedKeys(rows) {
			id, ok := index[key]
			if !ok {
				id = int64(len(state.Keys) + len(added))
				label := [3]string{key.Provider, key.Account, key.Model}
				encoded, errKey := json.Marshal(label)
				if errKey != nil {
					l.mu.Unlock()
					return nil, 0, errKey
				}
				added = append(added, label)
				cost += len(encoded) + 1
			}
			row := append([]int64{id}, rows[key].values()...)
			bucket.Rows = append(bucket.Rows, row)
			cost += len("[]") + len(row) - 1
			for _, value := range row {
				cost += numberLen(value)
			}
		}
		if cost > budget {
			break
		}
		budget -= cost
		for _, label := range added {
			index[Key{Provider: label[0], Account: label[1], Model: label[2]}] = int64(len(state.Keys))
			state.Keys = append(state.Keys, label)
		}
		if item.fine {
			state.Fine = append(state.Fine, bucket)
		} else {
			state.Coarse = append(state.Coarse, bucket)
		}
	}
	// Everything newer than a tier's newest dropped bucket was kept.
	fineFrom, coarseFrom := l.fineFrom, l.coarseFrom
	var fineCut, coarseCut bool
	for _, item := range order[kept:] {
		switch {
		case item.fine && !fineCut:
			fineCut = true
			fineFrom = later(fineFrom, time.Unix(item.start, 0).Add(FineStep))
		case !item.fine && !coarseCut:
			coarseCut = true
			coarseFrom = later(coarseFrom, time.Unix(item.start, 0).Add(CoarseStep))
		}
	}
	if !fineFrom.IsZero() {
		state.FineFrom = fineFrom.Unix()
	}
	if !coarseFrom.IsZero() {
		state.CoarseFrom = coarseFrom.Unix()
	}
	generation := l.generation
	l.mu.Unlock()
	raw, err := json.Marshal(state)
	if err == nil && len(raw) > limit {
		err = fmt.Errorf("usage ledger state: encoded %d bytes, over the %d-byte budget", len(raw), limit)
	}
	return raw, generation, err
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b.UTC()
	}
	return a
}

// MarkSaved clears the dirty flag if nothing changed since the marshal that
// returned generation.
func (l *Ledger) MarkSaved(generation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.generation == generation {
		l.dirty = false
	}
}

func sortedKeys(rows map[Key]*Counters) []Key {
	keys := make([]Key, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		return a.Model < b.Model
	})
	return keys
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
	keys := make([]Key, len(state.Keys))
	for i, key := range state.Keys {
		keys[i] = Key{Provider: clean(key[0], "unknown"), Account: clean(key[1], "unattributed"), Model: clean(key[2], "unknown")}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, pair := range []struct {
		t       *tier
		buckets []persistBucket
	}{{&l.fine, state.Fine}, {&l.coarse, state.Coarse}} {
		for _, bucket := range pair.buckets {
			at := time.Unix(bucket.Start, 0).UTC()
			for _, row := range bucket.Rows {
				if len(row) == 0 || row[0] < 0 || row[0] >= int64(len(keys)) {
					continue
				}
				pair.t.add(at, keys[row[0]], countersFrom(row[1:]))
			}
		}
	}
	for _, marker := range []struct {
		unix   int64
		target *time.Time
	}{{state.FineFrom, &l.fineFrom}, {state.CoarseFrom, &l.coarseFrom}} {
		if marker.unix <= 0 {
			continue
		}
		from := time.Unix(marker.unix, 0).UTC()
		if from.After(now) {
			from = now.UTC()
		}
		*marker.target = later(*marker.target, from)
	}
	l.pruneLocked(now)
	if state.Since != nil && (l.since.IsZero() || state.Since.Before(l.since)) {
		l.since = state.Since.UTC()
	}
	l.records += state.Records
	l.generation++
	return nil
}
