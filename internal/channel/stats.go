package channel

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

const (
	// hoursKept is the statistics window: 7 days of hourly buckets.
	hoursKept         = 7 * 24
	maxStatusCodes    = 8
	recentErrorsKept  = 20
	errorSnippetLimit = 200
	statsFileVersion  = 1
	nanoPerUSD        = 1e9
	// recentStartsKept is how many attempt starts a channel remembers to
	// recognize a second record of an attempt (see Observe); a few more than
	// one tolerate other records of the channel arriving in between.
	recentStartsKept = 8
	// clientClosedRequest is the status CPA records for a downstream cancel.
	clientClosedRequest = 499
	// maxModelsPerHour bounds the models one channel hour breaks down by;
	// attempts of further models count only for the channel.
	maxModelsPerHour = 16
)

// histogramBoundsMs are the inclusive upper bounds of the latency and TTFT
// histogram buckets, roughly log-scale from 50 ms to 20 minutes; one more
// bucket takes everything slower.
var histogramBoundsMs = [...]int64{
	50, 100, 150, 200, 300, 500, 750,
	1_000, 1_500, 2_000, 3_000, 5_000, 7_500,
	10_000, 15_000, 20_000, 30_000, 45_000,
	60_000, 90_000, 120_000, 180_000, 300_000, 600_000, 1_200_000,
}

const histogramBuckets = len(histogramBoundsMs) + 1

type histogram [histogramBuckets]uint32

func (h *histogram) add(d time.Duration) {
	ms, i := d.Milliseconds(), 0
	for i < len(histogramBoundsMs) && ms > histogramBoundsMs[i] {
		i++
	}
	h[i]++
}

type statusCount struct {
	code  int32
	count int64
}

// statusCodes counts failures by upstream status in a fixed table.
type statusCodes [maxStatusCodes]statusCount

// hourStats is one hour of one channel. Hours are unix hours (unix seconds
// / 3600) and buckets therefore UTC-aligned.
type hourStats struct {
	// hour is the unix hour the slot holds; a slot whose hour is outside
	// the window is empty.
	hour         int64
	attempts     int64
	failures     int64
	codes        statusCodes
	latency      histogram
	ttft         histogram
	inputTokens  int64
	outputTokens int64
	listCostNano int64
	// lastAt is the latest attempt in unix nanoseconds.
	lastAt int64
	// models breaks the hour down by the model clients asked for.
	models []modelHour
}

// modelHour is one model's share of a channel hour. streamed counts the
// successful streamed attempts that reported a first-token time, ttftMs sums
// those times; paceTokens and paceMs are the visible output tokens (reasoning
// left out) and the time after the first token, over those that took at
// least a millisecond more. CPA marks the first token at the first event with
// content, and Codex streams no reasoning, so its reasoning time falls before
// the first token.
type modelHour struct {
	model      string
	attempts   int64
	failures   int64
	codes      statusCodes
	prompt     int64
	cached     int64
	generated  int64
	reasoning  int64
	streamed   int64
	ttftMs     int64
	paceTokens int64
	paceMs     int64
}

// model returns the hour's entry for key, adding it while the hour has
// room; nil for no key or a full hour. key must not be a view of a borrowed
// buffer once added, so it is cloned.
func (b *hourStats) model(key string) *modelHour {
	if key == "" {
		return nil
	}
	for i := range b.models {
		if b.models[i].model == key {
			return &b.models[i]
		}
	}
	if len(b.models) == maxModelsPerHour {
		return nil
	}
	b.models = append(b.models, modelHour{model: strings.Clone(key)})
	return &b.models[len(b.models)-1]
}

// modelKey is the model an observation is counted under: the requested model
// (else the upstream one) without a reasoning suffix such as "(high)", in
// lower case, as CPA's model lists name it.
func modelKey(o *Observation) string {
	name := strings.TrimSpace(o.RequestedModel)
	if name == "" {
		name = strings.TrimSpace(o.Model)
	}
	if strings.HasSuffix(name, ")") {
		if i := strings.LastIndexByte(name, '('); i > 0 {
			name = strings.TrimSpace(name[:i])
		}
	}
	return strings.ToLower(name)
}

// count counts one failure under code. A full table admits a new code only
// in place of one seen once, so it keeps the frequent codes without ever
// overstating a count.
func (t *statusCodes) count(code int32) {
	free, single := -1, -1
	for i := range t {
		entry := &t[i]
		switch {
		case entry.count == 0:
			if free < 0 {
				free = i
			}
		case entry.code == code:
			entry.count++
			return
		case entry.count == 1 && single < 0:
			single = i
		}
	}
	if free < 0 {
		free = single
	}
	if free >= 0 {
		t[free] = statusCount{code: code, count: 1}
	}
}

// fileMap is the table as the statistics file writes it; nil when empty.
func (t *statusCodes) fileMap() map[string]int64 {
	var out map[string]int64
	for _, entry := range t {
		if entry.count > 0 {
			if out == nil {
				out = make(map[string]int64, maxStatusCodes)
			}
			out[strconv.Itoa(int(entry.code))] = entry.count
		}
	}
	return out
}

// parseStatusCodes reads a table the statistics file wrote.
func parseStatusCodes(codes map[string]int64) (statusCodes, error) {
	var t statusCodes
	if len(codes) > maxStatusCodes {
		return t, fmt.Errorf("more than %d status codes", maxStatusCodes)
	}
	slot := 0
	for key, count := range codes {
		code, err := strconv.ParseInt(key, 10, 32)
		if err != nil || code < 0 || count <= 0 {
			return t, fmt.Errorf("an invalid status code count %q: %d", key, count)
		}
		t[slot] = statusCount{code: int32(code), count: count}
		slot++
	}
	return t, nil
}

// channelStats holds one channel's last 7 days in a ring indexed by hour,
// so recording never scans or allocates and old hours fall out as their
// slots are reused.
type channelStats struct {
	mu       sync.Mutex
	authID   string
	provider string
	baseURL  string
	hours    [hoursKept]hourStats
	// errors is a ring of the latest failed attempts; errorsNext is where
	// the next one goes.
	errors     [recentErrorsKept]ErrorEntry
	errorsNext int
	errorsLen  int
	// starts is a ring of the executor calls (attemptKey) of the latest
	// counted successful attempts; startsNext is where the next one goes.
	starts     [recentStartsKept]uint64
	startsNext int
}

func slotIndex(hour int64) int {
	return int((hour%hoursKept + hoursKept) % hoursKept)
}

func (c *channelStats) slot(hour int64) *hourStats {
	b := &c.hours[slotIndex(hour)]
	if b.hour != hour {
		*b = hourStats{hour: hour}
	}
	return b
}

func (c *channelStats) addError(entry ErrorEntry) {
	c.errors[c.errorsNext] = entry
	c.errorsNext = (c.errorsNext + 1) % recentErrorsKept
	if c.errorsLen < recentErrorsKept {
		c.errorsLen++
	}
}

// countedStart reports whether a successful attempt of the executor call key
// was counted already, and remembers key otherwise; 0 means unknown.
func (c *channelStats) countedStart(key uint64) bool {
	if key == 0 {
		return false
	}
	for _, seen := range c.starts {
		if seen == key {
			return true
		}
	}
	c.starts[c.startsNext] = key
	c.startsNext = (c.startsNext + 1) % recentStartsKept
	return false
}

var attemptSeed = maphash.MakeSeed()

// attemptKey names one executor call: the downstream request's trace and the
// time CPA started the call. RequestedAt alone is not enough, as concurrent
// requests on a channel can start within the clock's resolution. 0 when
// either is missing.
func attemptKey(o Observation) uint64 {
	if o.TraceID == "" || o.RequestedAt.IsZero() {
		return 0
	}
	return max(maphash.String(attemptSeed, o.TraceID)^uint64(o.RequestedAt.UnixNano())*0x9e3779b97f4a7c15, 1)
}

// eachError visits the stored errors newest first.
func (c *channelStats) eachError(visit func(ErrorEntry)) {
	for i := 1; i <= c.errorsLen; i++ {
		visit(c.errors[(c.errorsNext-i+recentErrorsKept)%recentErrorsKept])
	}
}

// active reports whether the channel had an attempt within the hours.
func (c *channelStats) active(w span) bool {
	for h := w.from; h <= w.to; h++ {
		if c.hours[slotIndex(h)].hour == h {
			return true
		}
	}
	return false
}

func (c *channelStats) collect(w span, into *aggregate) {
	for h := w.from; h <= w.to; h++ {
		if b := &c.hours[slotIndex(h)]; b.hour == h {
			into.add(b)
		}
	}
}

// collectModels sums the window by model, most attempts first. An hour from
// a file can hold a model without attempts; it is left out.
func (c *channelStats) collectModels(w span) []ModelSummary {
	var sums []modelHour
	codes := map[string]map[int32]int64{}
	for h := w.from; h <= w.to; h++ {
		b := &c.hours[slotIndex(h)]
		if b.hour != h {
			continue
		}
		for _, m := range b.models {
			i := slices.IndexFunc(sums, func(sum modelHour) bool { return sum.model == m.model })
			if i < 0 {
				sums = append(sums, modelHour{model: m.model})
				i = len(sums) - 1
			}
			sum := &sums[i]
			sum.attempts += m.attempts
			sum.failures += m.failures
			sum.prompt += m.prompt
			sum.cached += m.cached
			sum.generated += m.generated
			sum.reasoning += m.reasoning
			sum.streamed += m.streamed
			sum.ttftMs += m.ttftMs
			sum.paceTokens += m.paceTokens
			sum.paceMs += m.paceMs
			for _, entry := range m.codes {
				if entry.count > 0 {
					if codes[m.model] == nil {
						codes[m.model] = map[int32]int64{}
					}
					codes[m.model][entry.code] += entry.count
				}
			}
		}
	}
	out := make([]ModelSummary, 0, len(sums))
	for i := range sums {
		if sums[i].attempts > 0 {
			out = append(out, sums[i].summary(codes[sums[i].model]))
		}
	}
	slices.SortFunc(out, func(a, b ModelSummary) int {
		if a.Attempts != b.Attempts {
			return cmp.Compare(b.Attempts, a.Attempts)
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out
}

func (m *modelHour) summary(codes map[int32]int64) ModelSummary {
	s := ModelSummary{Model: m.model, Attempts: m.attempts, Failures: m.failures, PromptTokens: m.prompt,
		CachedTokens: m.cached, GeneratedTokens: m.generated, ReasoningTokens: m.reasoning}
	if m.attempts > 0 {
		s.SuccessRate = float64(m.attempts-m.failures) / float64(m.attempts)
	}
	if successes := m.attempts - m.failures; successes > 0 {
		s.ReasoningPerAttempt = float64(m.reasoning) / float64(successes)
	}
	if m.prompt > 0 {
		s.CacheHitRate = float64(min(m.cached, m.prompt)) / float64(m.prompt)
	}
	if m.paceMs > 0 {
		s.TokensPerSecond = float64(m.paceTokens) * 1000 / float64(m.paceMs)
	}
	if m.streamed > 0 {
		s.TTFTAvgMs = m.ttftMs / m.streamed
	}
	if len(codes) > 0 {
		s.StatusCodes = make(map[string]int, len(codes))
		for code, count := range codes {
			s.StatusCodes[strconv.Itoa(int(code))] = int(count)
		}
	}
	return s
}

// remember keeps the latest non-empty identity fields. They are cloned
// because the caller's strings may be views of a borrowed request buffer.
func (c *channelStats) remember(o *Observation) {
	if o.AuthID != "" && o.AuthID != c.authID {
		c.authID = strings.Clone(o.AuthID)
	}
	if o.Provider != "" && o.Provider != c.provider {
		c.provider = strings.Clone(o.Provider)
	}
	if o.BaseURL != "" && o.BaseURL != c.baseURL {
		c.baseURL = strings.Clone(o.BaseURL)
	}
}

func unixHour(t time.Time) int64 {
	s := t.Unix()
	h := s / 3600
	if s%3600 < 0 {
		h--
	}
	return h
}

func hourStart(hour int64, loc *time.Location) time.Time {
	return time.Unix(hour*3600, 0).In(loc)
}

// span is an inclusive range of unix hours.
type span struct{ from, to int64 }

// spans are the windows the views report, relative to one instant.
type spans struct {
	// last24h is the current hour and the 23 before it.
	last24h span
	// today starts with the hour that contains local midnight, so it is
	// exact for whole-hour zone offsets and includes up to 59 minutes of the
	// previous day otherwise.
	today span
	// last7d is the whole statistics window.
	last7d span
}

func (r *Registry) windows(now time.Time) spans {
	current := unixHour(now)
	local := now.In(r.loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, r.loc)
	// Where DST starts at 00:00, local midnight does not exist and Go lands
	// on the previous day; the day starts at its first instant instead.
	for midnight.In(r.loc).Day() != local.Day() {
		midnight = midnight.Add(time.Hour)
	}
	return spans{
		last24h: span{current - 23, current},
		today:   span{unixHour(midnight), current},
		last7d:  span{current - hoursKept + 1, current},
	}
}

// Observe adds one usage record. Records whose auth_index is not 16
// lowercase hex characters, records older than the 7-day window and client
// cancellations are ignored; a missing or future time counts as now. A
// successful record of a recently counted executor call of the channel (same
// trace and RequestedAt) adds only its tokens and cost.
func (r *Registry) Observe(o Observation) {
	if !isAuthIndex(o.AuthIndex) {
		return
	}
	// CPA records a downstream cancel (context.Canceled) as failed with 499;
	// it says nothing about the upstream, so it is neither an attempt nor a
	// failure.
	if o.Failed && o.StatusCode == clientClosedRequest {
		return
	}
	started := attemptKey(o)
	r.handoffMu.RLock()
	defer r.handoffMu.RUnlock()
	now := r.now()
	at := o.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	hour := unixHour(at)
	if hour <= unixHour(now)-hoursKept {
		return
	}
	var failure ErrorEntry
	if o.Failed {
		failure = ErrorEntry{At: at, StatusCode: o.StatusCode, Model: strings.Clone(o.Model), Snippet: scrubText(o.ErrorSnippet, errorSnippetLimit)}
	}
	key := modelKey(&o)
	if o.Probe {
		key = ""
	}
	c := r.channelFor(o.AuthIndex)
	c.mu.Lock()
	b := c.slot(hour)
	// The model's share; the second record of an attempt stays out of it
	// (see below).
	var m *modelHour
	switch {
	case o.Failed:
		b.attempts++
		b.failures++
		code := int32(0)
		if o.StatusCode > 0 && o.StatusCode <= math.MaxInt32 {
			code = int32(o.StatusCode)
		}
		b.codes.count(code)
		c.addError(failure)
		if m = b.model(key); m != nil {
			m.attempts++
			m.failures++
			m.codes.count(code)
		}
	case c.countedStart(started):
		// CPA's Codex image tool publishes a second successful record for
		// the same executor call (PublishAdditionalModel): same trace and
		// RequestedAt, another model. Each executor call stamps its own
		// RequestedAt, so a retry is counted as an attempt again. The record
		// keeps the chat model's requested name, so its tokens stay out of
		// that model's share.
	default:
		b.attempts++
		if o.Latency > 0 {
			b.latency.add(o.Latency)
		}
		if o.TTFT > 0 {
			b.ttft.add(o.TTFT)
		}
		if m = b.model(key); m != nil {
			m.attempts++
			if o.Stream && o.TTFT > 0 {
				m.streamed++
				m.ttftMs += o.TTFT.Milliseconds()
				if pace, visible := o.Latency-o.TTFT, o.Tokens.Generated-o.Tokens.Reasoning; pace >= time.Millisecond && visible > 0 {
					m.paceTokens += visible
					m.paceMs += pace.Milliseconds()
				}
			}
		}
	}
	b.inputTokens += max(o.InputTokens, 0)
	b.outputTokens += max(o.OutputTokens, 0)
	b.listCostNano += max(o.ListCostNano, 0)
	if m != nil {
		m.prompt += max(o.Tokens.Prompt, 0)
		m.cached += max(o.Tokens.Cached, 0)
		m.generated += max(o.Tokens.Generated, 0)
		m.reasoning += max(o.Tokens.Reasoning, 0)
	}
	if nano := at.UnixNano(); nano > b.lastAt {
		b.lastAt = nano
	}
	c.remember(&o)
	c.mu.Unlock()
	r.generation.Add(1)
	if r.handoff != nil {
		o.At, o.ErrorSnippet = at, failure.Snippet
		r.handoff(o)
	}
}

// HandOff writes the statistics and, from the same instant on, also passes
// every observation to journal: a newer instance that loads the file and
// replays the journal counts each attempt once. The plugin calls it at
// quiesce, while it still owns the files; Resume stops the journaling when
// CPA rolls the replacement back.
func (r *Registry) HandOff(journal func(Observation)) error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.handoffMu.Lock()
	generation := r.generation.Load()
	snapshot, _ := r.statsSnapshot(r.now())
	r.handoff = journal
	r.handoffMu.Unlock()
	return r.writeStatsLocked(snapshot, generation)
}

// Resume stops passing observations on; this instance's memory holds them
// all.
func (r *Registry) Resume() {
	r.handoffMu.Lock()
	r.handoff = nil
	r.handoffMu.Unlock()
}

func (r *Registry) channelFor(index string) *channelStats {
	r.mu.RLock()
	c := r.channels[index]
	r.mu.RUnlock()
	if c != nil {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c = r.channels[index]; c == nil {
		c = &channelStats{}
		r.channels[strings.Clone(index)] = c
	}
	return c
}

func (r *Registry) lookup(index string) *channelStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.channels[index]
}

// aggregate sums hours for a Summary.
type aggregate struct {
	attempts     int64
	failures     int64
	codes        map[int32]int64
	latency      [histogramBuckets]uint64
	ttft         [histogramBuckets]uint64
	inputTokens  int64
	outputTokens int64
	listCostNano int64
	lastAt       int64
}

func (a *aggregate) add(b *hourStats) {
	a.attempts += b.attempts
	a.failures += b.failures
	for _, entry := range b.codes {
		if entry.count == 0 {
			continue
		}
		if a.codes == nil {
			a.codes = make(map[int32]int64, maxStatusCodes)
		}
		a.codes[entry.code] += entry.count
	}
	for i := range b.latency {
		a.latency[i] += uint64(b.latency[i])
		a.ttft[i] += uint64(b.ttft[i])
	}
	a.inputTokens += b.inputTokens
	a.outputTokens += b.outputTokens
	a.listCostNano += b.listCostNano
	a.lastAt = max(a.lastAt, b.lastAt)
}

func (a *aggregate) summary(loc *time.Location) Summary {
	s := Summary{
		Attempts:     a.attempts,
		Failures:     a.failures,
		LatencyP50Ms: quantileMs(&a.latency, 0.5),
		LatencyP95Ms: quantileMs(&a.latency, 0.95),
		TTFTP50Ms:    quantileMs(&a.ttft, 0.5),
		TTFTP95Ms:    quantileMs(&a.ttft, 0.95),
		InputTokens:  a.inputTokens,
		OutputTokens: a.outputTokens,
		ListCostUSD:  float64(a.listCostNano) / nanoPerUSD,
	}
	if a.attempts > 0 {
		s.SuccessRate = float64(a.attempts-a.failures) / float64(a.attempts)
	}
	if len(a.codes) > 0 {
		s.StatusCodes = make(map[string]int, len(a.codes))
		for code, count := range a.codes {
			s.StatusCodes[strconv.Itoa(int(code))] = int(count)
		}
	}
	if a.lastAt > 0 {
		s.LastAt = time.Unix(0, a.lastAt).In(loc)
	}
	return s
}

// quantileMs estimates the q-quantile by linear interpolation inside the
// bucket where the cumulative count reaches it; the slowest bucket reports
// its lower bound, 20 minutes. It is 0 without samples.
func quantileMs(h *[histogramBuckets]uint64, q float64) int64 {
	var total uint64
	for _, n := range h {
		total += n
	}
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	var below uint64
	for i, n := range h {
		if n == 0 || float64(below+n) < rank {
			below += n
			continue
		}
		if i == len(histogramBoundsMs) {
			break
		}
		lower := 0.0
		if i > 0 {
			lower = float64(histogramBoundsMs[i-1])
		}
		upper := float64(histogramBoundsMs[i])
		return int64(math.Round(lower + (upper-lower)*(rank-float64(below))/float64(n)))
	}
	return histogramBoundsMs[len(histogramBoundsMs)-1]
}

// List returns every channel that has metadata or was seen in the last 7
// days, ordered by vendor (case-insensitive, channels without one last)
// then auth_index.
func (r *Registry) List() []View {
	now := r.now()
	w := r.windows(now)
	metas := r.metaSnapshot()
	r.mu.RLock()
	channels := make(map[string]*channelStats, len(r.channels))
	for index, c := range r.channels {
		channels[index] = c
	}
	r.mu.RUnlock()

	views := make([]View, 0, max(len(channels), len(metas)))
	for index, c := range channels {
		meta, hasMeta := metas[index]
		c.mu.Lock()
		active := c.active(w.last7d)
		var view View
		if active || hasMeta {
			view = r.channelView(index, c, meta, w)
		}
		c.mu.Unlock()
		if active || hasMeta {
			views = append(views, view)
		}
	}
	for index, meta := range metas {
		if _, seen := channels[index]; !seen {
			views = append(views, r.channelView(index, nil, meta, w))
		}
	}
	sort.Slice(views, func(i, j int) bool {
		left, right := views[i].Meta.Vendor, views[j].Meta.Vendor
		if (left == "") != (right == "") {
			return right == ""
		}
		if left, right = strings.ToLower(left), strings.ToLower(right); left != right {
			return left < right
		}
		return views[i].AuthIndex < views[j].AuthIndex
	})
	return views
}

// channelView builds the list fields; the caller holds c.mu when c is set.
func (r *Registry) channelView(index string, c *channelStats, meta Meta, w spans) View {
	meta.AuthIndex = index
	view := View{AuthIndex: index, Meta: meta}
	var last24h, today aggregate
	view.Models24h = []ModelSummary{}
	if c != nil {
		view.AuthID, view.Provider, view.BaseURL = c.authID, c.provider, c.baseURL
		c.collect(w.last24h, &last24h)
		c.collect(w.today, &today)
		view.Models24h = c.collectModels(w.last24h)
	}
	view.Last24h = last24h.summary(r.loc)
	view.Today = today.summary(r.loc)
	return view
}

// Detail returns one channel's view with its last 7 days: the 168 hourly
// points oldest first and the recent errors newest first. It reports false
// for a channel List would not show.
func (r *Registry) Detail(authIndex string) (Detail, bool) {
	index, err := normalizeAuthIndex(authIndex)
	if err != nil {
		return Detail{}, false
	}
	now := r.now()
	w := r.windows(now)
	meta, hasMeta := r.metaFor(index)
	detail := Detail{Models7d: []ModelSummary{}, Hours: make([]Point, 0, hoursKept), RecentErrors: []ErrorEntry{}}
	var week aggregate
	active := false
	if c := r.lookup(index); c != nil {
		c.mu.Lock()
		if active = c.active(w.last7d); active || hasMeta {
			detail.View = r.channelView(index, c, meta, w)
			c.collect(w.last7d, &week)
			detail.Models7d = c.collectModels(w.last7d)
			for h := w.last7d.from; h <= w.last7d.to; h++ {
				detail.Hours = append(detail.Hours, hourPoint(&c.hours[slotIndex(h)], h, r.loc))
			}
			since := hourStart(w.last7d.from, time.UTC)
			c.eachError(func(entry ErrorEntry) {
				if !entry.At.Before(since) {
					entry.At = entry.At.In(r.loc)
					detail.RecentErrors = append(detail.RecentErrors, entry)
				}
			})
		}
		c.mu.Unlock()
	}
	if !active && !hasMeta {
		return Detail{}, false
	}
	if len(detail.Hours) == 0 {
		detail.View = r.channelView(index, nil, meta, w)
		for h := w.last7d.from; h <= w.last7d.to; h++ {
			detail.Hours = append(detail.Hours, Point{Hour: hourStart(h, r.loc)})
		}
	}
	detail.Last7d = week.summary(r.loc)
	return detail, true
}

func hourPoint(b *hourStats, hour int64, loc *time.Location) Point {
	point := Point{Hour: hourStart(hour, loc)}
	if b.hour != hour {
		return point
	}
	var latency [histogramBuckets]uint64
	for i, n := range b.latency {
		latency[i] = uint64(n)
	}
	point.Attempts, point.Failures = b.attempts, b.failures
	point.LatencyP50Ms = quantileMs(&latency, 0.5)
	point.ListCostUSD = float64(b.listCostNano) / nanoPerUSD
	return point
}

// FlushStats writes the statistics snapshot when it changed since the last
// successful flush; the plugin calls it from its background loop and on
// shutdown. While CanWrite reports false it writes nothing and returns nil,
// keeping the changes for a later flush. Either way it then frees the
// statistics of channels idle for the whole window, which the snapshot
// leaves out, unless they have metadata: their auth_id, provider and base
// URL stay known.
func (r *Registry) FlushStats() error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	generation := r.generation.Load()
	snapshot, idle := r.statsSnapshot(r.now())
	err := r.writeStatsLocked(snapshot, generation)
	// Idleness moves with the clock, not with the generation.
	r.evictIdle(idle)
	return err
}

// evictIdle drops the statistics of the given channels that have no
// metadata and are still idle. Holding metaWriteMu, no UpdateMeta gives one
// of them metadata between the check and the delete; holding handoffMu
// exclusively, no Observe is between channelFor and its update, so none
// lands on a dropped entry. The caller holds flushMu.
func (r *Registry) evictIdle(indexes []string) {
	if len(indexes) == 0 {
		return
	}
	r.metaWriteMu.Lock()
	defer r.metaWriteMu.Unlock()
	metas := r.metaSnapshot()
	indexes = slices.DeleteFunc(indexes, func(index string) bool {
		_, hasMeta := metas[index]
		return hasMeta
	})
	if len(indexes) == 0 {
		return
	}
	r.handoffMu.Lock()
	defer r.handoffMu.Unlock()
	// The window is taken again: the hour may have turned since the
	// snapshot, and an attempt observed since then is within it.
	week := r.windows(r.now()).last7d
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, index := range indexes {
		c := r.channels[index]
		if c == nil {
			continue
		}
		c.mu.Lock()
		idle := !c.active(week)
		c.mu.Unlock()
		if idle {
			delete(r.channels, index)
		}
	}
}

// Delete forgets a channel: its metadata and its statistics, which are
// written at once so a restart does not bring them back. A channel CPA still
// sends traffic to reappears with its next attempt. It reports whether there
// was anything to forget. While CanWrite reports false it returns
// ErrReadOnly; a failed metadata write wraps ErrPersist and changes nothing.
// A failed statistics write is logged and retried by the next flush.
func (r *Registry) Delete(authIndex string) (bool, error) {
	index, err := normalizeAuthIndex(authIndex)
	if err != nil {
		return false, err
	}
	if !r.canWrite() {
		return false, ErrReadOnly
	}
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.metaWriteMu.Lock()
	_, hadMeta := r.metaFor(index)
	if hadMeta {
		r.metaMu.RLock()
		metas := maps.Clone(r.metas)
		r.metaMu.RUnlock()
		delete(metas, index)
		if err := r.writeMeta(metas); err != nil {
			r.metaWriteMu.Unlock()
			return false, err
		}
		r.metaMu.Lock()
		r.metas = metas
		r.metaMu.Unlock()
	}
	// Holding handoffMu exclusively, no Observe is between channelFor and its
	// update, so none lands on the dropped entry.
	r.handoffMu.Lock()
	r.mu.Lock()
	_, hadStats := r.channels[index]
	delete(r.channels, index)
	r.mu.Unlock()
	r.handoffMu.Unlock()
	r.metaWriteMu.Unlock()
	if hadStats {
		generation := r.generation.Add(1)
		snapshot, _ := r.statsSnapshot(r.now())
		if err := r.writeStatsLocked(snapshot, generation); err != nil {
			r.logf("warn", "cpa-key-policy: channel statistics not saved after deleting "+index+": "+err.Error())
		}
	}
	return hadMeta || hadStats, nil
}

// writeStatsLocked writes snapshot, taken at generation, unless the file
// already holds that generation. The caller holds flushMu.
func (r *Registry) writeStatsLocked(snapshot statsFile, generation uint64) error {
	if generation == r.flushed || r.statsPath == "" || !r.canWrite() {
		return nil
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode channel statistics: %w", err)
	}
	if err := persist.AtomicWrite(r.statsPath, raw); err != nil {
		return fmt.Errorf("write channel statistics %s: %w", r.statsPath, err)
	}
	r.flushed = generation
	return nil
}

// statsFile is the statistics snapshot. Histogram counts follow
// HistogramBoundsMs: one count per bound, then the count above the last.
type statsFile struct {
	Version           int                `json:"version"`
	UpdatedAt         time.Time          `json:"updated_at"`
	HistogramBoundsMs []int64            `json:"histogram_bounds_ms"`
	Channels          []statsFileChannel `json:"channels"`
}

type statsFileChannel struct {
	AuthIndex string          `json:"auth_index"`
	AuthID    string          `json:"auth_id,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	BaseURL   string          `json:"base_url,omitempty"`
	Hours     []statsFileHour `json:"hours,omitempty"`
	// Errors are oldest first.
	Errors []ErrorEntry `json:"errors,omitempty"`
}

type statsFileHour struct {
	Hour         time.Time        `json:"hour"`
	Attempts     int64            `json:"attempts"`
	Failures     int64            `json:"failures,omitempty"`
	StatusCodes  map[string]int64 `json:"status_codes,omitempty"`
	Latency      []uint32         `json:"latency_histogram,omitempty"`
	TTFT         []uint32         `json:"ttft_histogram,omitempty"`
	InputTokens  int64            `json:"input_tokens,omitempty"`
	OutputTokens int64            `json:"output_tokens,omitempty"`
	ListCostNano int64            `json:"list_cost_nano,omitempty"`
	LastAt       time.Time        `json:"last_at"`
	// Models is the hour by model; files of 0.24.0 and earlier have none.
	Models []statsFileModel `json:"models,omitempty"`
}

type statsFileModel struct {
	Model           string           `json:"model"`
	Attempts        int64            `json:"attempts,omitempty"`
	Failures        int64            `json:"failures,omitempty"`
	StatusCodes     map[string]int64 `json:"status_codes,omitempty"`
	PromptTokens    int64            `json:"prompt_tokens,omitempty"`
	CachedTokens    int64            `json:"cached_tokens,omitempty"`
	GeneratedTokens int64            `json:"generated_tokens,omitempty"`
	ReasoningTokens int64            `json:"reasoning_tokens,omitempty"`
	Streamed        int64            `json:"streamed,omitempty"`
	TTFTMs          int64            `json:"ttft_ms,omitempty"`
	PaceTokens      int64            `json:"pace_tokens,omitempty"`
	PaceMs          int64            `json:"pace_ms,omitempty"`
}

// statsSnapshot also returns the channels it leaves out: those without an
// hour or an error in the window.
func (r *Registry) statsSnapshot(now time.Time) (statsFile, []string) {
	week := r.windows(now).last7d
	since := hourStart(week.from, time.UTC)
	r.mu.RLock()
	indexes := make([]string, 0, len(r.channels))
	for index := range r.channels {
		indexes = append(indexes, index)
	}
	channels := make([]*channelStats, len(indexes))
	sort.Strings(indexes)
	for i, index := range indexes {
		channels[i] = r.channels[index]
	}
	r.mu.RUnlock()

	file := statsFile{Version: statsFileVersion, UpdatedAt: now.UTC(), HistogramBoundsMs: histogramBoundsMs[:], Channels: []statsFileChannel{}}
	var idle []string
	for i, c := range channels {
		c.mu.Lock()
		entry := statsFileChannel{AuthIndex: indexes[i], AuthID: c.authID, Provider: c.provider, BaseURL: c.baseURL}
		for h := week.from; h <= week.to; h++ {
			if b := &c.hours[slotIndex(h)]; b.hour == h {
				entry.Hours = append(entry.Hours, fileHour(b))
			}
		}
		c.eachError(func(e ErrorEntry) {
			if !e.At.Before(since) {
				e.At = e.At.UTC()
				entry.Errors = append(entry.Errors, e)
			}
		})
		c.mu.Unlock()
		slices.Reverse(entry.Errors)
		if len(entry.Hours) > 0 || len(entry.Errors) > 0 {
			file.Channels = append(file.Channels, entry)
		} else {
			idle = append(idle, indexes[i])
		}
	}
	return file, idle
}

func fileHour(b *hourStats) statsFileHour {
	out := statsFileHour{
		Hour:         hourStart(b.hour, time.UTC),
		Attempts:     b.attempts,
		Failures:     b.failures,
		InputTokens:  b.inputTokens,
		OutputTokens: b.outputTokens,
		ListCostNano: b.listCostNano,
	}
	if b.lastAt > 0 {
		out.LastAt = time.Unix(0, b.lastAt).UTC()
	}
	out.StatusCodes = b.codes.fileMap()
	for i := range b.models {
		m := &b.models[i]
		out.Models = append(out.Models, statsFileModel{
			Model: m.model, Attempts: m.attempts, Failures: m.failures, StatusCodes: m.codes.fileMap(),
			PromptTokens: m.prompt, CachedTokens: m.cached, GeneratedTokens: m.generated, ReasoningTokens: m.reasoning,
			Streamed: m.streamed, TTFTMs: m.ttftMs, PaceTokens: m.paceTokens, PaceMs: m.paceMs,
		})
	}
	if b.latency != (histogram{}) {
		out.Latency = slices.Clone(b.latency[:])
	}
	if b.ttft != (histogram{}) {
		out.TTFT = slices.Clone(b.ttft[:])
	}
	return out
}

// loadStats reads the snapshot written by FlushStats, dropping hours and
// errors outside the 7-day window that ends at now.
func loadStats(path string, now time.Time) (map[string]*channelStats, error) {
	channels := make(map[string]*channelStats)
	if path == "" {
		return channels, nil
	}
	raw, err := persist.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return channels, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read channel statistics %s: %w", path, err)
	}
	var file statsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse channel statistics %s: %w", path, err)
	}
	if file.Version != statsFileVersion {
		return nil, fmt.Errorf("channel statistics %s: unsupported version %d", path, file.Version)
	}
	if !slices.Equal(file.HistogramBoundsMs, histogramBoundsMs[:]) {
		return nil, fmt.Errorf("channel statistics %s: histogram bounds differ from this release", path)
	}
	week := span{unixHour(now) - hoursKept + 1, unixHour(now)}
	since := hourStart(week.from, time.UTC)
	for _, entry := range file.Channels {
		if !isAuthIndex(entry.AuthIndex) {
			return nil, fmt.Errorf("channel statistics %s: invalid auth_index %q", path, entry.AuthIndex)
		}
		if _, duplicate := channels[entry.AuthIndex]; duplicate {
			return nil, fmt.Errorf("channel statistics %s: duplicate auth_index %s", path, entry.AuthIndex)
		}
		c := &channelStats{authID: entry.AuthID, provider: entry.Provider, baseURL: entry.BaseURL}
		kept := false
		for _, fh := range entry.Hours {
			b, err := fh.stats()
			if err != nil {
				return nil, fmt.Errorf("channel statistics %s: channel %s: %w", path, entry.AuthIndex, err)
			}
			if b.hour < week.from || b.hour > week.to {
				continue
			}
			slot := &c.hours[slotIndex(b.hour)]
			if slot.hour == b.hour {
				return nil, fmt.Errorf("channel statistics %s: channel %s: duplicate hour %s", path, entry.AuthIndex, fh.Hour.Format(time.RFC3339))
			}
			*slot, kept = b, true
		}
		for _, e := range entry.Errors {
			if !e.At.Before(since) && !e.At.After(now) {
				c.addError(e)
			}
		}
		if kept || c.errorsLen > 0 {
			channels[entry.AuthIndex] = c
		}
	}
	return channels, nil
}

func (fh statsFileHour) stats() (hourStats, error) {
	if fh.Hour.IsZero() || fh.Hour.Unix()%3600 != 0 || fh.Hour.Nanosecond() != 0 {
		return hourStats{}, fmt.Errorf("hour %s does not start an hour", fh.Hour.Format(time.RFC3339Nano))
	}
	if fh.Attempts < 0 || fh.Failures < 0 || fh.Failures > fh.Attempts || fh.InputTokens < 0 || fh.OutputTokens < 0 || fh.ListCostNano < 0 {
		return hourStats{}, fmt.Errorf("hour %s has negative or inconsistent counts", fh.Hour.Format(time.RFC3339))
	}
	codes, err := parseStatusCodes(fh.StatusCodes)
	if err != nil {
		return hourStats{}, fmt.Errorf("hour %s has %w", fh.Hour.Format(time.RFC3339), err)
	}
	if len(fh.Models) > maxModelsPerHour {
		return hourStats{}, fmt.Errorf("hour %s has more than %d models", fh.Hour.Format(time.RFC3339), maxModelsPerHour)
	}
	b := hourStats{
		codes:        codes,
		hour:         unixHour(fh.Hour),
		attempts:     fh.Attempts,
		failures:     fh.Failures,
		inputTokens:  fh.InputTokens,
		outputTokens: fh.OutputTokens,
		listCostNano: fh.ListCostNano,
	}
	if !fh.LastAt.IsZero() {
		b.lastAt = fh.LastAt.UnixNano()
	}
	for _, fm := range fh.Models {
		m, err := fm.stats()
		if err != nil {
			return hourStats{}, fmt.Errorf("hour %s: %w", fh.Hour.Format(time.RFC3339), err)
		}
		if slices.ContainsFunc(b.models, func(other modelHour) bool { return other.model == m.model }) {
			return hourStats{}, fmt.Errorf("hour %s has model %q twice", fh.Hour.Format(time.RFC3339), m.model)
		}
		b.models = append(b.models, m)
	}
	for _, h := range []struct {
		counts []uint32
		into   *histogram
	}{{fh.Latency, &b.latency}, {fh.TTFT, &b.ttft}} {
		switch len(h.counts) {
		case 0:
		case histogramBuckets:
			copy(h.into[:], h.counts)
		default:
			return hourStats{}, fmt.Errorf("hour %s has a histogram of %d buckets, want %d", fh.Hour.Format(time.RFC3339), len(h.counts), histogramBuckets)
		}
	}
	return b, nil
}

func (fm statsFileModel) stats() (modelHour, error) {
	if fm.Model == "" || fm.Model != strings.ToLower(strings.TrimSpace(fm.Model)) {
		return modelHour{}, fmt.Errorf("model %q is not a model key", fm.Model)
	}
	for _, n := range []int64{fm.Attempts, fm.Failures, fm.PromptTokens, fm.CachedTokens, fm.GeneratedTokens, fm.ReasoningTokens, fm.Streamed, fm.TTFTMs, fm.PaceTokens, fm.PaceMs} {
		if n < 0 {
			return modelHour{}, fmt.Errorf("model %q has a negative count", fm.Model)
		}
	}
	if fm.Failures > fm.Attempts || fm.Streamed > fm.Attempts-fm.Failures {
		return modelHour{}, fmt.Errorf("model %q has inconsistent counts", fm.Model)
	}
	codes, err := parseStatusCodes(fm.StatusCodes)
	if err != nil {
		return modelHour{}, fmt.Errorf("model %q has %w", fm.Model, err)
	}
	return modelHour{model: fm.Model, attempts: fm.Attempts, failures: fm.Failures, codes: codes, prompt: fm.PromptTokens,
		cached: fm.CachedTokens, generated: fm.GeneratedTokens, reasoning: fm.ReasoningTokens, streamed: fm.Streamed,
		ttftMs: fm.TTFTMs, paceTokens: fm.PaceTokens, paceMs: fm.PaceMs}, nil
}
