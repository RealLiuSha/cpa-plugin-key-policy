package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	indexA = "0123456789abcdef"
	indexB = "fedcba9876543210"
	indexC = "00000000000000c0"
)

// testClock is a settable time source safe to share with goroutines.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func fixedNow(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return loc
}

func newTestRegistry(t *testing.T, opts Options) *Registry {
	t.Helper()
	r, err := NewRegistry(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ptr[T any](value T) *T { return &value }

func nearly(left, right float64) bool { return math.Abs(left-right) <= 1e-9 }

func TestObserveSummarizesLast24hAndToday(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 10, 19, 15, 30, 0, 0, loc)
	r := newTestRegistry(t, Options{MetaPath: filepath.Join(t.TempDir(), "channels.json"), Location: loc, Now: fixedNow(now)})
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("Acme")}); err != nil {
		t.Fatal(err)
	}
	ago := func(minutes int) time.Time { return now.Add(-time.Duration(minutes) * time.Minute) }
	success := func(at time.Time, latency, ttft time.Duration) Observation {
		return Observation{AuthIndex: indexA, AuthID: "codex:apikey:1", Provider: "codex", BaseURL: "https://up.example/v1", Model: "gpt-5", At: at, Latency: latency, TTFT: ttft, InputTokens: 100, OutputTokens: 10, ListCostNano: 1_000_000}
	}
	failure := func(at time.Time, status int) Observation {
		return Observation{AuthIndex: indexA, Model: "gpt-5", Failed: true, StatusCode: status, At: at, Latency: 30 * time.Millisecond, InputTokens: 5, ErrorSnippet: "upstream said no"}
	}
	for _, o := range []Observation{
		success(ago(50), 120*time.Millisecond, 0),
		success(ago(40), 180*time.Millisecond, 60*time.Millisecond),
		success(ago(30), 900*time.Millisecond, 400*time.Millisecond),
		failure(ago(20), 429),
		failure(ago(10), 0),
		// A client cancellation, which CPA records as failed with 499.
		failure(ago(5), 499),
		// 2026-10-18 19:30 local time: within the last 24 hours, not today.
		{AuthIndex: indexA, At: now.Add(-20 * time.Hour), InputTokens: 7},
		// Older than the 7-day window.
		{AuthIndex: indexA, At: now.Add(-8 * 24 * time.Hour), InputTokens: 1000},
		// Not the auth_index form CPA emits.
		{AuthIndex: strings.ToUpper(indexB)}, {AuthIndex: "0123"}, {AuthIndex: ""}, {AuthIndex: "0123456789abcdeg"},
	} {
		r.Observe(o)
	}

	views := r.List()
	if len(views) != 1 || views[0].AuthIndex != indexA {
		t.Fatalf("views = %+v, want only %s", views, indexA)
	}
	view := views[0]
	if view.AuthID != "codex:apikey:1" || view.Provider != "codex" || view.BaseURL != "https://up.example/v1" || view.Meta.Vendor != "Acme" {
		t.Fatalf("identity = %q %q %q meta %+v", view.AuthID, view.Provider, view.BaseURL, view.Meta)
	}
	last24h, today := view.Last24h, view.Today
	if last24h.Attempts != 6 || last24h.Failures != 2 || !nearly(last24h.SuccessRate, 4.0/6) || last24h.InputTokens != 317 {
		t.Fatalf("last 24h = %+v", last24h)
	}
	if today.Attempts != 5 || today.Failures != 2 || !nearly(today.SuccessRate, 3.0/5) || today.InputTokens != 310 || today.OutputTokens != 30 {
		t.Fatalf("today = %+v", today)
	}
	if !reflect.DeepEqual(today.StatusCodes, map[string]int{"429": 1, "0": 1}) {
		t.Fatalf("status codes = %v", today.StatusCodes)
	}
	// Successes of 120, 180 and 900 ms put the median in the (150, 200] ms
	// bucket; counting the 30 ms failures would move it below 150.
	if today.LatencyP50Ms <= 150 || today.LatencyP50Ms > 200 || today.LatencyP95Ms <= 750 || today.LatencyP95Ms > 1000 {
		t.Fatalf("latency p50 %d p95 %d", today.LatencyP50Ms, today.LatencyP95Ms)
	}
	if today.TTFTP95Ms <= 300 || today.TTFTP95Ms > 500 {
		t.Fatalf("ttft p95 %d", today.TTFTP95Ms)
	}
	if !nearly(today.ListCostUSD, 0.003) || !today.LastAt.Equal(ago(10)) {
		t.Fatalf("list cost %v last %v", today.ListCostUSD, today.LastAt)
	}
	detail, _ := r.Detail(indexA)
	if errs := detail.RecentErrors; len(errs) != 2 || errs[0].StatusCode != 0 || errs[1].StatusCode != 429 {
		t.Fatalf("recent errors = %+v", errs)
	}
}

func TestAdditionalModelRecordIsNotASecondAttempt(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := newTestRegistry(t, Options{Now: fixedNow(now)})
	started := func(n int) time.Time { return time.Date(2026, 10, 19, 19, 59, n, 123456789, mustShanghai(t)) }
	success := func(index, trace string, at time.Time, model string, latency time.Duration) Observation {
		return Observation{AuthIndex: index, TraceID: trace, Model: model, RequestedAt: at, Latency: latency, InputTokens: 10, ListCostNano: 1_000_000}
	}
	// CPA's image tool record shares the executor call's trace and
	// RequestedAt, even with another attempt of the channel in between and
	// after the journal.
	extra, err := json.Marshal(success(indexA, "t1", started(1), "gpt-image-2", 20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var replayed Observation
	if err := json.Unmarshal(extra, &replayed); err != nil {
		t.Fatal(err)
	}
	for _, o := range []Observation{
		success(indexA, "t1", started(1), "gpt-5", 100*time.Millisecond),
		success(indexA, "t2", started(2), "gpt-5", 100*time.Millisecond),
		replayed,
		// A retry on the same channel starts a new executor call.
		success(indexB, "t3", started(1), "gpt-5", 0), success(indexB, "t3", started(2), "gpt-5", 0),
		// Concurrent requests can start within the clock's resolution.
		success(indexB, "t4", started(3), "gpt-5", 0), success(indexB, "t5", started(3), "gpt-5", 0),
		// Without RequestedAt or a trace every record is an attempt.
		success(indexB, "t6", time.Time{}, "gpt-5", 0), success(indexB, "", started(4), "gpt-5", 0), success(indexB, "", started(4), "gpt-5", 0),
		// Only a successful attempt is recognized again.
		{AuthIndex: indexC, TraceID: "t7", Failed: true, StatusCode: 502, RequestedAt: started(1)}, success(indexC, "t7", started(1), "gpt-5", 0),
	} {
		r.Observe(o)
	}
	for index, want := range map[string]Summary{
		indexA: {Attempts: 2, InputTokens: 30, ListCostUSD: 0.003},
		indexB: {Attempts: 7, InputTokens: 70, ListCostUSD: 0.007},
		indexC: {Attempts: 2, Failures: 1, InputTokens: 10, ListCostUSD: 0.001},
	} {
		got, _ := r.Detail(index)
		if day := got.Today; day.Attempts != want.Attempts || day.Failures != want.Failures || day.InputTokens != want.InputTokens || !nearly(day.ListCostUSD, want.ListCostUSD) {
			t.Errorf("%s: today = %+v, want %+v", index, day, want)
		}
	}
	// The 20-minute latency of the image record is no sample.
	if got, _ := r.Detail(indexA); got.Today.LatencyP95Ms > 100 {
		t.Fatalf("latency p95 %d", got.Today.LatencyP95Ms)
	}
}

// Each model a channel serves is summarized on its own, under the model the
// client asked for without a reasoning suffix: success, cache share of the
// prompt, reasoning per successful attempt, pace of visible output after the
// first token of streamed answers and failures by status. A non-stream answer's TTFT is its
// whole latency, so it sets no pace; the image tool's second record of an
// attempt keeps the chat model's name but not its share, and a probe without a
// downstream key counts only for the channel. The breakdown survives a flush
// and reload.
func TestModelsAreSummarizedApartAndReloaded(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "stats.json")
	opts := Options{StatsPath: path, Now: fixedNow(now)}
	r := newTestRegistry(t, opts)
	sol := func(trace, requested string, stream bool, ttft, latency time.Duration, tokens Tokens) Observation {
		return Observation{AuthIndex: indexA, Provider: "codex", Model: "gpt-6.1-sol-2026", RequestedModel: requested, At: now, Stream: stream,
			TTFT: ttft, Latency: latency, Tokens: tokens, TraceID: trace, RequestedAt: now}
	}
	r.Observe(sol("t1", "GPT-6.1-sol(high)", true, time.Second, 3*time.Second, Tokens{Prompt: 1000, Cached: 800, Generated: 400, Reasoning: 300}))
	r.Observe(sol("t2", "gpt-6.1-sol", true, 3*time.Second, 5*time.Second, Tokens{Prompt: 1000, Cached: 0, Generated: 200, Reasoning: 100}))
	r.Observe(sol("t3", "gpt-6.1-sol", false, 9*time.Second, 9*time.Second+time.Millisecond, Tokens{Prompt: 1000, Cached: 200, Generated: 900, Reasoning: 200}))
	// The image tool's record of the first attempt: same trace and start.
	r.Observe(Observation{AuthIndex: indexA, Model: "gpt-image-2", RequestedModel: "GPT-6.1-sol(high)", TraceID: "t1", RequestedAt: now, At: now,
		Tokens: Tokens{Prompt: 5000, Generated: 5000}})
	r.Observe(Observation{AuthIndex: indexA, Model: "gpt-6.1-sol", At: now, Failed: true, StatusCode: 429})
	// No requested model: the upstream one names it.
	r.Observe(Observation{AuthIndex: indexA, Model: "gpt-6-astra", At: now, Tokens: Tokens{Prompt: 10, Generated: 5}})
	// An inspection question counts for the channel, not for the model.
	r.Observe(Observation{AuthIndex: indexA, Model: "gpt-6-astra", At: now, Probe: true, Tokens: Tokens{Prompt: 14000, Generated: 50}})

	check := func(r *Registry) {
		t.Helper()
		view := r.List()[0]
		models := view.Models24h
		if view.Last24h.Attempts != 6 || len(models) != 2 || models[0].Model != "gpt-6.1-sol" || models[1].Model != "gpt-6-astra" {
			t.Fatalf("models = %+v", models)
		}
		sol := models[0]
		if sol.Attempts != 4 || sol.Failures != 1 || !nearly(sol.SuccessRate, 3.0/4) || sol.StatusCodes["429"] != 1 || sol.PromptTokens != 3000 ||
			!nearly(sol.CacheHitRate, 1000.0/3000) || !nearly(sol.ReasoningPerAttempt, 200) || !nearly(sol.TokensPerSecond, 50) || sol.TTFTAvgMs != 2000 {
			t.Fatalf("gpt-6.1-sol = %+v", sol)
		}
		if astra := models[1]; astra.Attempts != 1 || astra.TokensPerSecond != 0 || astra.TTFTAvgMs != 0 {
			t.Fatalf("gpt-6-astra = %+v", astra)
		}
	}
	check(r)
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	check(newTestRegistry(t, opts))
}

func TestTodayFollowsTheConfiguredLocation(t *testing.T) {
	shanghai := mustShanghai(t)
	now := time.Date(2026, 10, 19, 0, 30, 0, 0, shanghai)
	for _, tc := range []struct {
		name  string
		loc   *time.Location
		today int64
	}{
		// Local midnight was 30 minutes ago, so 23:30 was yesterday.
		{"Asia/Shanghai", shanghai, 1},
		// Both attempts fall on 2026-10-18 UTC.
		{"UTC", time.UTC, 2},
	} {
		r := newTestRegistry(t, Options{Location: tc.loc, Now: fixedNow(now)})
		r.Observe(Observation{AuthIndex: indexA, At: time.Date(2026, 10, 18, 23, 30, 0, 0, shanghai)})
		r.Observe(Observation{AuthIndex: indexA, At: time.Date(2026, 10, 19, 0, 10, 0, 0, shanghai)})
		if view := r.List()[0]; view.Today.Attempts != tc.today || view.Last24h.Attempts != 2 {
			t.Fatalf("%s: today %d last 24h %d, want %d and 2", tc.name, view.Today.Attempts, view.Last24h.Attempts, tc.today)
		}
	}
}

func TestRecentErrorsAreScrubbedAndNewestFirst(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := newTestRegistry(t, Options{Now: fixedNow(now)})
	cases := []struct{ in, want string }{
		{"Authorization: Bearer abc.def-123 rejected", "Authorization: Bearer [redacted] rejected"},
		{"missing bearer token", "missing bearer token"},
		{"Incorrect API key provided: sk-proj-********abcd.", "Incorrect API key provided: [redacted]."},
		{`{"key":"sk-test1234"}`, `{"key":"[redacted]"}`},
		{"trace 0123456789abcdef0123456789abcdef01 done", "trace [redacted] done"},
		{"blob aGVsbG8gd29ybGQgaGVsbG8gd29ybGQgaGVsbG8=+/x end", "blob [redacted] end"},
		{"line one\nline two\r\n\tthree", "line one line two three"},
		{strings.Repeat("错", 250), strings.Repeat("错", 200)},
	}
	for i, tc := range cases {
		r.Observe(Observation{AuthIndex: indexA, Failed: true, StatusCode: 500 + i, Model: "m", At: now.Add(time.Duration(i-len(cases)) * time.Minute), ErrorSnippet: tc.in})
	}
	detail, ok := r.Detail(indexA)
	if !ok || len(detail.RecentErrors) != len(cases) {
		t.Fatalf("detail ok=%v errors=%d", ok, len(detail.RecentErrors))
	}
	for i, tc := range cases {
		if got := detail.RecentErrors[len(cases)-1-i]; got.Snippet != tc.want || got.StatusCode != 500+i || got.Model != "m" {
			t.Errorf("case %d: %+v, want snippet %q", i, got, tc.want)
		}
	}
	for i := 0; i < 25; i++ {
		r.Observe(Observation{AuthIndex: indexA, Failed: true, StatusCode: 600 + i, At: now})
	}
	if detail, _ = r.Detail(indexA); len(detail.RecentErrors) != 20 || detail.RecentErrors[0].StatusCode != 624 || detail.RecentErrors[19].StatusCode != 605 {
		t.Fatalf("ring = %d entries, newest %d oldest %d", len(detail.RecentErrors), detail.RecentErrors[0].StatusCode, detail.RecentErrors[19].StatusCode)
	}
}

func TestUpdateMetaValidatesPersistsAndRespectsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "channels.json")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	writable := true
	opts := Options{MetaPath: path, Now: fixedNow(now), CanWrite: func() bool { return writable }}
	r := newTestRegistry(t, opts)
	for name, patch := range map[string]MetaPatch{
		"short auth_index":   {AuthIndex: "abc"},
		"non-hex auth_index": {AuthIndex: "0123456789abcdeg"},
		"long display name":  {AuthIndex: indexA, DisplayName: ptr(strings.Repeat("名", 201))},
	} {
		if _, err := r.UpdateMeta(patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected patches wrote the file: %v", err)
	}

	meta, err := r.UpdateMeta(MetaPatch{AuthIndex: " 0123456789ABCDEF ", DisplayName: ptr("  主力  "), Vendor: ptr("Acme"), Note: ptr(strings.Repeat("备", 200))})
	if err != nil {
		t.Fatal(err)
	}
	if meta.AuthIndex != indexA || meta.DisplayName != "主力" || !meta.UpdatedAt.Equal(now) {
		t.Fatalf("normalized meta = %+v", meta)
	}
	if meta, err = r.UpdateMeta(MetaPatch{AuthIndex: indexA, Note: ptr("")}); err != nil || meta.Vendor != "Acme" || meta.Note != "" {
		t.Fatalf("partial patch = %+v, %v", meta, err)
	}
	for index, patch := range map[string]MetaPatch{indexB: {Vendor: ptr("beta")}, indexC: {DisplayName: ptr("no vendor")}} {
		patch.AuthIndex = index
		if _, err := r.UpdateMeta(patch); err != nil {
			t.Fatal(err)
		}
	}

	var file struct {
		Version  int    `json:"version"`
		Channels []Meta `json:"channels"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &file) != nil || file.Version != 1 || len(file.Channels) != 3 {
		t.Fatalf("file = %s (%v)", raw, err)
	}
	var order []string
	for _, view := range newTestRegistry(t, opts).List() {
		order = append(order, view.AuthIndex+"/"+view.Meta.Vendor)
	}
	if want := []string{indexA + "/Acme", indexB + "/beta", indexC + "/"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("reloaded order = %v, want %v", order, want)
	}

	writable = false
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("Other")}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only update err = %v", err)
	}
	writable = true
	if detail, _ := r.Detail(indexA); detail.Meta.Vendor != "Acme" {
		t.Fatalf("read-only update changed memory: %+v", detail.Meta)
	}
	// Clearing every field removes the entry.
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexC, DisplayName: ptr("")}); err != nil {
		t.Fatal(err)
	}
	if _, listed := r.Detail(indexC); listed {
		t.Fatal("a channel without metadata or traffic is still listed")
	}

	// A failed write leaves memory unchanged: the file's directory is
	// replaced by a regular file after the first write.
	blockedDir := filepath.Join(dir, "blocked")
	blockedPath := filepath.Join(blockedDir, "channels.json")
	blocked := newTestRegistry(t, Options{MetaPath: blockedPath})
	if _, err := blocked.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("Acme")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blockedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blockedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("Other")}); !errors.Is(err, ErrPersist) {
		t.Fatalf("blocked write err = %v", err)
	}
	if detail, _ := blocked.Detail(indexA); detail.Meta.Vendor != "Acme" {
		t.Fatalf("failed write changed memory: %+v", detail.Meta)
	}
}

func TestFlushStatsReloadKeepsSevenDays(t *testing.T) {
	loc := mustShanghai(t)
	clock := &testClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, loc)}
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	opts := Options{StatsPath: path, Location: loc, Now: clock.Now}
	r := newTestRegistry(t, opts)
	start := clock.Now()
	r.Observe(Observation{AuthIndex: indexA, Provider: "codex", Failed: true, StatusCode: 502, At: start.Add(-150 * time.Hour), ErrorSnippet: "bad gateway"})
	r.Observe(Observation{AuthIndex: indexA, Provider: "codex", At: start.Add(-time.Hour), Latency: 900 * time.Millisecond, ListCostNano: 2_000_000})
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	// Unchanged statistics are not written again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged flush wrote the snapshot: %v", err)
	}
	r.Observe(Observation{AuthIndex: indexA, At: start.Add(-time.Hour), Latency: 950 * time.Millisecond})
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}

	detail, ok := newTestRegistry(t, opts).Detail(indexA)
	week := detail.Last7d
	if !ok || week.Attempts != 3 || week.Failures != 1 || week.StatusCodes["502"] != 1 || detail.Provider != "codex" || len(detail.RecentErrors) != 1 || detail.RecentErrors[0].Snippet != "bad gateway" {
		t.Fatalf("reloaded detail ok=%v week=%+v errors=%+v provider=%q", ok, week, detail.RecentErrors, detail.Provider)
	}
	if week.LatencyP50Ms <= 750 || week.LatencyP50Ms > 1000 || len(detail.Hours) != hoursKept {
		t.Fatalf("latency p50 %d, %d hours", week.LatencyP50Ms, len(detail.Hours))
	}
	if hour := detail.Hours[hoursKept-2]; hour.Attempts != 2 || !nearly(hour.ListCostUSD, 0.002) || !hour.Hour.Equal(start.Add(-time.Hour)) || hour.LatencyP50Ms <= 750 {
		t.Fatalf("previous hour = %+v", hour)
	}

	// Two days later the 150-hour-old failure is outside the window.
	clock.Advance(48 * time.Hour)
	detail, ok = newTestRegistry(t, opts).Detail(indexA)
	if !ok || detail.Last7d.Attempts != 2 || detail.Last7d.Failures != 0 || len(detail.RecentErrors) != 0 || detail.Last24h.Attempts != 0 {
		t.Fatalf("aged detail ok=%v week=%+v errors=%d", ok, detail.Last7d, len(detail.RecentErrors))
	}

	readOnlyPath := filepath.Join(dir, "read-only.json")
	readOnly := newTestRegistry(t, Options{StatsPath: readOnlyPath, Now: clock.Now, CanWrite: func() bool { return false }})
	readOnly.Observe(Observation{AuthIndex: indexA})
	if err := readOnly.FlushStats(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readOnlyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only flush wrote: %v", err)
	}

	for name, content := range map[string]string{"truncated": `{"version":1,`, "unknown version": `{"version":2,"channels":[]}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewRegistry(opts); err == nil {
			t.Errorf("%s snapshot loaded without error", name)
		}
	}
}

func TestFlushStatsFreesIdleChannelsWithoutMetadata(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	r := newTestRegistry(t, Options{MetaPath: filepath.Join(dir, "channels.json"), StatsPath: filepath.Join(dir, "stats.json"), Now: clock.Now})
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexB, Vendor: ptr("Acme")}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{indexA, indexB} {
		r.Observe(Observation{AuthIndex: index, AuthID: "codex:apikey:" + index})
	}
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	// Nothing changed since the last write, so this flush writes nothing;
	// the idle channel is freed all the same.
	clock.Advance(8 * 24 * time.Hour)
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	if r.lookup(indexA) != nil {
		t.Fatal("an idle channel without metadata is still held")
	}
	if views := r.List(); len(views) != 1 || views[0].AuthIndex != indexB || views[0].AuthID != "codex:apikey:"+indexB {
		t.Fatalf("views = %+v, want %s with its auth_id", views, indexB)
	}
	r.Observe(Observation{AuthIndex: indexA})
	if detail, ok := r.Detail(indexA); !ok || detail.Last24h.Attempts != 1 {
		t.Fatalf("a freed channel seen again: ok=%v %+v", ok, detail.Last24h)
	}
}

// A channel given metadata while a flush frees idle channels is either kept
// or given its metadata after the flush, never freed once it has metadata.
func TestFlushStatsDoesNotFreeAChannelGivenMetadataMeanwhile(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	var r *Registry
	var armed, fired atomic.Bool
	updated := make(chan struct{})
	var updateErr error
	updatedMeanwhile := false
	now := func() time.Time {
		// The eviction reads the clock while it holds handoffMu exclusively,
		// after its metadata check: give the channel metadata right there.
		if armed.Load() {
			if r.handoffMu.TryRLock() {
				r.handoffMu.RUnlock()
			} else if fired.CompareAndSwap(false, true) {
				go func() {
					_, updateErr = r.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("Acme")})
					close(updated)
				}()
				select {
				case <-updated:
					updatedMeanwhile = true
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
		return clock.Now()
	}
	r = newTestRegistry(t, Options{MetaPath: filepath.Join(dir, "channels.json"), StatsPath: filepath.Join(dir, "stats.json"), Now: now})
	r.Observe(Observation{AuthIndex: indexA, AuthID: "codex:apikey:" + indexA})
	clock.Advance(8 * 24 * time.Hour)
	armed.Store(true)
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}
	if !fired.Load() {
		t.Fatal("the flush freed no channel")
	}
	<-updated
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	if updatedMeanwhile && r.lookup(indexA) == nil {
		t.Fatal("the flush freed a channel that had been given metadata")
	}
}

// Files of 0.21.0 and earlier may hold cost_ratio and balance: they load, an
// entry that held nothing else is dropped, and the next write leaves both out.
func TestLegacyCostAndBalanceFieldsAreIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.json")
	legacy := `{"version":1,"channels":[` +
		`{"auth_index":"` + indexA + `","cost_ratio":0.5,"balance":{"kind":"sub2api","threshold_usd":5}},` +
		`{"auth_index":"` + indexB + `","vendor":"beta","cost_ratio":2}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRegistry(t, Options{MetaPath: path})
	views := r.List()
	if len(views) != 1 || views[0].AuthIndex != indexB || views[0].Meta.Vendor != "beta" {
		t.Fatalf("views = %+v, want only %s with its vendor", views, indexB)
	}
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexB, Note: ptr("kept")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "cost_ratio") || strings.Contains(string(raw), "balance") || strings.Contains(string(raw), indexA) {
		t.Fatalf("rewritten file = %s (%v)", raw, err)
	}
}

// Delete forgets metadata and statistics at once: a reload from the files
// does not bring the channel back, while other channels are untouched.
func TestDeleteForgetsMetadataAndStatistics(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	writable := true
	opts := Options{MetaPath: filepath.Join(dir, "channels.json"), StatsPath: filepath.Join(dir, "stats.json"), Now: fixedNow(now), CanWrite: func() bool { return writable }}
	r := newTestRegistry(t, opts)
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexA, Vendor: ptr("gone")}); err != nil {
		t.Fatal(err)
	}
	r.Observe(Observation{AuthIndex: indexA, At: now.Add(-time.Hour)})
	r.Observe(Observation{AuthIndex: indexB, At: now.Add(-time.Hour)})
	if err := r.FlushStats(); err != nil {
		t.Fatal(err)
	}

	writable = false
	if _, err := r.Delete(indexA); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only delete err = %v", err)
	}
	writable = true
	if _, err := r.Delete("abc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed auth_index err = %v", err)
	}
	if found, err := r.Delete(strings.ToUpper(indexA)); err != nil || !found {
		t.Fatalf("delete = %v, %v", found, err)
	}
	if found, err := r.Delete(indexA); err != nil || found {
		t.Fatalf("second delete = %v, %v", found, err)
	}
	for label, registry := range map[string]*Registry{"memory": r, "reloaded": newTestRegistry(t, opts)} {
		views := registry.List()
		if len(views) != 1 || views[0].AuthIndex != indexB {
			t.Fatalf("%s views = %+v, want only %s", label, views, indexB)
		}
	}
}

func TestReadCPAConfigDerivesCPAAuthIndexes(t *testing.T) {
	type credential struct{ kind, prefix, baseURL, apiKey, id, provider, group, proxyURL string }
	for _, tc := range []struct {
		name string
		yaml string
		want []credential
	}{
		{
			name: "v8 layout",
			yaml: `config-version: 8
proxy-url: http://replaced.example:3128
requests:
  proxy-url: " http://global.example:3128 "
access:
  api-keys: [client-key]
api-keys:
  codex:
    - name: inferhub
      base-url: https://inferhub.net/v1
      prefix: hub
      proxy-url: socks5://proxy.example:1080
      headers: {X-Team: " red ", " Empty": ""}
      keys:
        - api-key: " codex-key-1 "
        - api-key: codex-key-2
          weight: 2
          prefix: /own/
          proxy-url: ""
          headers: {B: b, A: a}
    - name: no-base-url
      keys: [{api-key: orphan-key}]
  claude:
    - base-url: https://claude.example/
      keys: [{api-key: claude-key, prefix: "a/b"}]
  meta:
    - keys: [{api-key: meta-key}, {api-key: "dca:device"}]
  openai-compatibility:
    - name: Relay
      base-url: https://relay.example/v1
      keys: [{api-key: compat-key, proxy-url: http://p.example}, {api-key: ""}]
    - name: off
      disabled: true
      base-url: https://off.example
      keys: [{api-key: off-key}]
codex-api-key:
  - api-key: replaced-by-v8
    base-url: https://legacy.example
gemini-api-key:
  - api-key: gemini-key
    headers: {Z: z}
xai-api-key:
  - api-key: xai-key-without-base-url
`,
			want: []credential{
				{"codex", "codex-api-key", "https://inferhub.net/v1", "codex-key-1", "codex:apikey:775a472e19ec", "codex", "inferhub", "socks5://proxy.example:1080"},
				{"codex", "codex-api-key", "https://inferhub.net/v1", "codex-key-2", "codex:apikey:b5126a3106a2", "codex", "inferhub", "http://global.example:3128"},
				{"claude", "claude-api-key", "https://claude.example/", "claude-key", "claude:apikey:7dd596fd62f8", "claude", "", "http://global.example:3128"},
				{"meta", "meta-api-key", "https://api.meta.ai/v1", "meta-key", "meta:apikey:70d03cae28ed", "meta", "", "http://global.example:3128"},
				{"openai-compatibility", "openai-compatibility", "https://relay.example/v1", "compat-key", "openai-compatibility:relay:24356aaab180", "openai-compatible-relay", "Relay", "http://p.example"},
				{"gemini", "gemini-api-key", "", "gemini-key", "gemini:apikey:fdc89e775d50", "gemini", "", "http://global.example:3128"},
			},
		},
		{
			name: "older layout",
			yaml: `api-keys: [client-key]
proxy-url: socks5h://legacy-proxy.example:1080
codex-api-key:
  - &codex
    api-key: legacy-1
    base-url: https://legacy.example/v1
    prefix: team
  - <<: *codex
    api-key: legacy-2
claude-api-key:
  - api-key: claude-1
openai-compatibility:
  - name: relay
    base-url: " https://relay.example "
    api-key-entries:
      - api-key: compat-1
xai-api-key: not-a-list-secret
`,
			want: []credential{
				{"codex", "codex-api-key", "https://legacy.example/v1", "legacy-1", "codex:apikey:4dc376d49e61", "codex", "", "socks5h://legacy-proxy.example:1080"},
				{"codex", "codex-api-key", "https://legacy.example/v1", "legacy-2", "codex:apikey:d509d49d536f", "codex", "", "socks5h://legacy-proxy.example:1080"},
				{"claude", "claude-api-key", "", "claude-1", "claude:apikey:4117c8ba105b", "claude", "", "socks5h://legacy-proxy.example:1080"},
				{"openai-compatibility", "openai-compatibility", "https://relay.example", "compat-1", "openai-compatibility:relay:a32126534ab1", "openai-compatible-relay", "relay", "socks5h://legacy-proxy.example:1080"},
			},
		},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadCPAConfig(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := make(map[string]Credential, len(tc.want))
		for _, c := range tc.want {
			sum := sha256.Sum256([]byte(c.prefix + ":" + c.baseURL + "+" + c.apiKey))
			index := hex.EncodeToString(sum[:8])
			want[index] = Credential{AuthIndex: index, ID: c.id, Provider: c.provider, Kind: c.kind, Group: c.group, BaseURL: c.baseURL, APIKey: c.apiKey, ProxyURL: c.proxyURL}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %+v\nwant %+v", tc.name, got, want)
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("codex-api-key: [{api-key: secret-key-123, base-url: https://x.example"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCPAConfig(path); err == nil || strings.Contains(err.Error(), "secret-key-123") {
		t.Fatalf("broken YAML err = %v", err)
	}
	if _, err := ReadCPAConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("a missing config read without error")
	}
}

// CPA's panel disables a config API key by adding "*" to its
// excluded-models; in a v8 group a key's own list, other than null, replaces
// the group's. openai-compatibility has no such list.
func TestReadCPAConfigMarksKeysCPADisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`api-keys:
  codex:
    - name: off
      base-url: https://codex.example/v1
      excluded-models: ["*"]
      keys:
        - api-key: inherits-star
        - api-key: own-empty-list
          excluded-models: []
        - api-key: null-inherits
          excluded-models: null
        - api-key: some-models
          excluded-models: ["gpt-4*"]
  openai-compatibility:
    - name: relay
      base-url: https://relay.example/v1
      excluded-models: ["*"]
      keys: [{api-key: compat-key}]
claude-api-key:
  - api-key: legacy-star
    excluded-models: [" * "]
  - api-key: legacy-on
`), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := ReadCPAConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		got[credential.APIKey] = credential.Disabled
	}
	want := map[string]bool{"inherits-star": true, "own-empty-list": false, "null-inherits": true, "some-models": false, "compat-key": false, "legacy-star": true, "legacy-on": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled by key = %v, want %v", got, want)
	}
}

func TestObserveIsSafeForConcurrentUse(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	r := newTestRegistry(t, Options{StatsPath: filepath.Join(t.TempDir(), "stats.json"), Now: clock.Now})
	// indexB is idle when the flushes start, so they may free it while
	// observations land on it; none of them may be lost.
	r.Observe(Observation{AuthIndex: indexB})
	clock.Advance(8 * 24 * time.Hour)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			r.Observe(Observation{AuthIndex: indexB})
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Observe(Observation{AuthIndex: indexA, Failed: i%10 == 0, StatusCode: 500 + g, Latency: time.Duration(i) * time.Millisecond, ErrorSnippet: "Bearer secret-token-123"})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.List()
			r.Detail(indexA)
			if err := r.FlushStats(); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	views := r.List()
	if len(views) != 2 || views[0].Last24h.Attempts != 4000 || views[0].Last24h.Failures != 400 || views[1].AuthIndex != indexB || views[1].Last24h.Attempts != 500 {
		t.Fatalf("views = %+v", views)
	}
}

func TestReadCPAConfigListsTheModelsEachKeyServes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := `force-model-prefix: true
api-keys:
  codex:
    - name: main
      base-url: https://up.example
      models:
        - name: gpt-6-sol
      keys:
        - api-key: sk-group-models
        - api-key: sk-prefixed
          prefix: /zeu/
          excluded-models: ["gpt-6-sol?"]
        - api-key: sk-own-models
          excluded-models: ["*-luna"]
          models:
            - name: gpt-6-sol
              alias: alphasearch-nb-gpt-6-sol
            - name: gpt-6-luna
            - name: gpt-6-astra
    - name: plain
      base-url: https://plain.example
      keys:
        - api-key: sk-no-models
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCPAConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	models := map[string][]string{}
	for _, credential := range got {
		models[credential.APIKey] = credential.Models
	}
	// Under force-model-prefix a prefixed key serves only the prefixed name;
	// "?" in a pattern is no wildcard.
	if !slices.Equal(models["sk-group-models"], []string{"gpt-6-sol"}) || !slices.Equal(models["sk-prefixed"], []string{"zeu/gpt-6-sol"}) ||
		!slices.Equal(models["sk-own-models"], []string{"alphasearch-nb-gpt-6-sol", "gpt-6-astra"}) ||
		models["sk-no-models"] != nil {
		t.Fatalf("models = %v", models)
	}
}
