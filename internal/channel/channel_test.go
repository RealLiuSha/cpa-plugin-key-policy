package channel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: indexA, CostRatio: ptr(0.5)}); err != nil {
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
	if view.AuthID != "codex:apikey:1" || view.Provider != "codex" || view.BaseURL != "https://up.example/v1" || view.Meta.CostRatio != 0.5 {
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
	if !nearly(today.ListCostUSD, 0.003) || !nearly(today.UpstreamCostUSD, 0.0015) || !today.LastAt.Equal(ago(10)) {
		t.Fatalf("cost list %v upstream %v last %v", today.ListCostUSD, today.UpstreamCostUSD, today.LastAt)
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
		"ratio above 10":     {AuthIndex: indexA, CostRatio: ptr(10.5)},
		"negative ratio":     {AuthIndex: indexA, CostRatio: ptr(-0.1)},
		"NaN ratio":          {AuthIndex: indexA, CostRatio: ptr(math.NaN())},
		"negative threshold": {AuthIndex: indexA, Balance: &BalanceSource{Kind: BalanceSub2API, ThresholdUSD: -1}},
		"unknown kind":       {AuthIndex: indexA, Balance: &BalanceSource{Kind: "stripe"}},
		"long display name":  {AuthIndex: indexA, DisplayName: ptr(strings.Repeat("名", 201))},
	} {
		if _, err := r.UpdateMeta(patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected patches wrote the file: %v", err)
	}

	meta, err := r.UpdateMeta(MetaPatch{AuthIndex: " 0123456789ABCDEF ", DisplayName: ptr("  主力  "), Vendor: ptr("Acme"), Note: ptr(strings.Repeat("备", 200)), CostRatio: ptr(10.0), Balance: &BalanceSource{Kind: " Sub2API ", ThresholdUSD: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if meta.AuthIndex != indexA || meta.DisplayName != "主力" || meta.CostRatio != 10 || meta.Balance != (BalanceSource{Kind: BalanceSub2API, ThresholdUSD: 5}) || !meta.UpdatedAt.Equal(now) {
		t.Fatalf("normalized meta = %+v", meta)
	}
	if meta, err = r.UpdateMeta(MetaPatch{AuthIndex: indexA, Note: ptr("")}); err != nil || meta.Vendor != "Acme" || meta.Note != "" || meta.CostRatio != 10 {
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

func TestRefreshBalanceReadsBothSourcesWithoutLeakingTheKey(t *testing.T) {
	const key = "relay-key-7"
	const missing, unpriced, unlimited, tokens = "00000000000000d0", "00000000000000e0", "00000000000000f0", "00000000000000a0"
	loc := mustShanghai(t)
	var calls []string
	failSub2API := false
	httpDo := func(_ context.Context, method, url string, headers map[string]string) (int, []byte, error) {
		calls = append(calls, url)
		if method != http.MethodGet || headers["Authorization"] != "Bearer "+key {
			return 0, nil, fmt.Errorf("unexpected request %s %s", method, url)
		}
		switch url {
		case "https://sub.example/v1/usage":
			if failSub2API {
				return 401, []byte(`{"error":{"type":"authentication_error","message":"Invalid API key relay-key-7"}}`), nil
			}
			return 200, []byte(`{"mode":"unrestricted","isValid":true,"remaining":4.5,"balance":4.5,"unit":"USD"}`), nil
		case "https://bill.example/v1/dashboard/billing/subscription":
			return 200, []byte(`{"object":"billing_subscription","has_payment_method":true,"hard_limit_usd":12.5}`), nil
		case "https://bill.example/v1/dashboard/billing/usage?start_date=2026-10-01&end_date=2026-10-19":
			return 200, []byte(`{"object":"list","total_usage":250}`), nil
		case "https://err.example/v1/dashboard/billing/subscription":
			return 200, []byte(`{"error":{"message":"token relay-key-7 has expired","type":"new_api_error"}}`), nil
		case "https://unlimited.example/v1/dashboard/billing/subscription":
			// new-api and one-api answer this for a token with unlimited quota.
			return 200, []byte(`{"object":"billing_subscription","has_payment_method":true,"hard_limit_usd":100000000}`), nil
		case "https://unlimited.example/v1/dashboard/billing/usage?start_date=2026-10-01&end_date=2026-10-19":
			return 200, []byte(`{"object":"list","total_usage":20}`), nil
		case "https://tokens.example/v1/dashboard/billing/subscription":
			// new-api showing raw quota: a quota above 1e8 is not unlimited.
			return 200, []byte(`{"object":"billing_subscription","has_payment_method":true,"hard_limit_usd":160000000}`), nil
		case "https://tokens.example/v1/dashboard/billing/usage?start_date=2026-10-01&end_date=2026-10-19":
			return 200, []byte(`{"object":"list","total_usage":1000000000}`), nil
		}
		return 404, []byte("not found"), nil
	}
	credentials := map[string]Credential{
		indexA:    {BaseURL: "https://sub.example/v1/", APIKey: key},
		indexB:    {BaseURL: "https://bill.example", APIKey: " " + key + " "},
		indexC:    {BaseURL: "https://err.example/v1", APIKey: key},
		unlimited: {BaseURL: "https://unlimited.example/v1", APIKey: key},
		tokens:    {BaseURL: "https://tokens.example/v1", APIKey: key},
	}
	r := newTestRegistry(t, Options{
		MetaPath: filepath.Join(t.TempDir(), "channels.json"),
		Location: loc,
		Now:      fixedNow(time.Date(2026, 10, 19, 9, 0, 0, 0, loc)),
		HTTPDo:   httpDo,
		Credentials: func(index string) (Credential, bool) {
			credential, ok := credentials[index]
			return credential, ok
		},
	})
	for index, kind := range map[string]string{indexA: BalanceSub2API, indexB: BalanceOpenAIBilling, indexC: BalanceOpenAIBilling, unlimited: BalanceOpenAIBilling, tokens: BalanceOpenAIBilling, missing: BalanceSub2API} {
		if _, err := r.UpdateMeta(MetaPatch{AuthIndex: index, Balance: &BalanceSource{Kind: kind, ThresholdUSD: 5}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, index string
		remaining   float64
		low         bool
		// unit is "" for openai_billing: the site's display unit is unknown.
		unit    string
		failure string
	}{
		{"sub2api", indexA, 4.5, true, "USD", ""},
		{"openai billing", indexB, 10, false, "", ""},
		{"openai billing in raw quota", tokens, 150000000, false, "", ""},
		{"error object in a 200 answer", indexC, 0, false, "", "GET /v1/dashboard/billing/subscription: HTTP 200: upstream error: token [redacted] has expired"},
		{"token with unlimited quota", unlimited, 0, false, "", "GET /v1/dashboard/billing/subscription: HTTP 200: the key has unlimited quota on this site, so the account balance cannot be read with it"},
		{"credential missing from CPA's config", missing, 0, false, "", "credential not found in CPA's config"},
	} {
		state, err := r.RefreshBalance(context.Background(), tc.index)
		if err != nil || state.Error != tc.failure || state.Remaining != tc.remaining || state.Low != tc.low || state.Unit != tc.unit || state.At.IsZero() {
			t.Errorf("%s: state %+v err %v", tc.name, state, err)
		}
	}

	// A failed query names the status, keeps the last reading and never
	// shows the key, even when the upstream echoes it.
	failSub2API = true
	state, err := r.RefreshBalance(context.Background(), indexA)
	if err != nil || !strings.Contains(state.Error, "GET /v1/usage: HTTP 401") || strings.Contains(state.Error, key) || state.Remaining != 4.5 || !state.Low {
		t.Fatalf("failed refresh = %+v, %v", state, err)
	}
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: unpriced, Vendor: ptr("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RefreshBalance(context.Background(), unpriced); !errors.Is(err, ErrNoBalanceSource) {
		t.Fatalf("refresh without a source err = %v", err)
	}
	if _, err := r.RefreshBalance(context.Background(), "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("refresh of a malformed index err = %v", err)
	}

	calls = nil
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if r.RefreshBalances(canceled); len(calls) != 0 {
		t.Fatalf("canceled refresh queried %v", calls)
	}
	failSub2API = false
	// One request for sub2api, two for each billing site, one each for the
	// error object and the unlimited token; the missing credential needs none.
	if r.RefreshBalances(context.Background()); len(calls) != 7 {
		t.Fatalf("refresh queried %v", calls)
	}
	for _, view := range r.List() {
		if view.AuthIndex == indexA && (view.Balance == nil || view.Balance.Error != "" || view.Balance.Remaining != 4.5) {
			t.Fatalf("listed balance = %+v", view.Balance)
		}
	}
}

func TestRefreshBalanceUsesTheCredentialsProxy(t *testing.T) {
	const usage = `{"mode":"unrestricted","isValid":true,"remaining":3,"unit":"USD"}`
	var mu sync.Mutex
	var proxied, direct []string
	record := func(into *[]string, answer string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			mu.Lock()
			*into = append(*into, request.URL.String())
			mu.Unlock()
			_, _ = w.Write([]byte(answer))
		})
	}
	proxy := httptest.NewServer(record(&proxied, usage))
	defer proxy.Close()
	target := httptest.NewServer(record(&direct, usage))
	defer target.Close()
	const invalid, unsupported = "00000000000000d0", "00000000000000e0"
	credentials := map[string]Credential{
		// The host does not resolve: only the proxy can answer for it.
		indexA:      {BaseURL: "http://sub.proxied.example/v1", APIKey: "k", ProxyURL: proxy.URL},
		indexB:      {BaseURL: target.URL, APIKey: "k", ProxyURL: "DIRECT"},
		indexC:      {BaseURL: target.URL, APIKey: "k", ProxyURL: "none"},
		invalid:     {BaseURL: target.URL, APIKey: "k", ProxyURL: "http://user:secret-pass@[bad"},
		unsupported: {BaseURL: target.URL, APIKey: "k", ProxyURL: "ftp://user:secret-pass@p.example"},
	}
	r := newTestRegistry(t, Options{
		MetaPath: filepath.Join(t.TempDir(), "channels.json"),
		Credentials: func(index string) (Credential, bool) {
			credential, ok := credentials[index]
			return credential, ok
		},
	})
	for index := range credentials {
		if _, err := r.UpdateMeta(MetaPatch{AuthIndex: index, Balance: &BalanceSource{Kind: BalanceSub2API}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range []string{indexA, indexB, indexC} {
		if state, err := r.RefreshBalance(context.Background(), index); err != nil || state.Error != "" || state.Remaining != 3 {
			t.Fatalf("%s: state %+v err %v", index, state, err)
		}
	}
	if !reflect.DeepEqual(proxied, []string{"http://sub.proxied.example/v1/usage"}) || !reflect.DeepEqual(direct, []string{"/v1/usage", "/v1/usage"}) {
		t.Fatalf("through the proxy %v, direct %v", proxied, direct)
	}
	for _, index := range []string{invalid, unsupported} {
		state, err := r.RefreshBalance(context.Background(), index)
		if err != nil || !strings.Contains(state.Error, "proxy-url CPA's config gives this credential") || strings.Contains(state.Error, "secret-pass") {
			t.Errorf("%s: state %+v err %v", index, state, err)
		}
	}
	if len(proxied) != 1 || len(direct) != 2 {
		t.Fatalf("a rejected proxy-url sent a request: proxied %v, direct %v", proxied, direct)
	}

	// As in CPA, the hop to an https proxy offers only HTTP/1.1: a proxy that
	// picks h2 cannot take the HTTP/1.1 CONNECT that follows. The client
	// rejects the test certificate afterwards; the offer is recorded first.
	var offered [][]string
	tlsProxy := httptest.NewUnstartedServer(http.NotFoundHandler())
	tlsProxy.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		offered = append(offered, hello.SupportedProtos)
		mu.Unlock()
		return nil, nil
	}}
	tlsProxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsProxy.StartTLS()
	defer tlsProxy.Close()
	const viaHTTPSProxy = "00000000000000f0"
	credentials[viaHTTPSProxy] = Credential{BaseURL: "https://sub.proxied.example/v1", APIKey: "k", ProxyURL: tlsProxy.URL}
	if _, err := r.UpdateMeta(MetaPatch{AuthIndex: viaHTTPSProxy, Balance: &BalanceSource{Kind: BalanceSub2API}}); err != nil {
		t.Fatal(err)
	}
	if state, err := r.RefreshBalance(context.Background(), viaHTTPSProxy); err != nil || state.Error == "" {
		t.Fatalf("through the untrusted https proxy: state %+v err %v", state, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(offered, [][]string{{"http/1.1"}}) {
		t.Fatalf("protocols offered to the https proxy = %q, want only http/1.1", offered)
	}
}

// The expected IDs and providers are what CPA v8.0.13's config synthesizer
// gives these configs; the global proxy-url is not part of an ID.
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
