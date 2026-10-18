package policy

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// twoDayStore records on two days without flushing and returns the store
// after the first day was flushed (so the live file holds day one only).
func twoDayStore(t *testing.T, clock *testClock) *Store {
	t.Helper()
	store := newStore(t, clock, []KeyConfig{testKey(t, "a", "cpa_a"), testKey(t, "b", "cpa_b")}, []ModelPrice{tokenPrice("fast", "", 1, 2)})
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	store.RecordUsage(tokenEvent("b", "codex", "fast", UsageDetail{OutputTokens: 1_000_000}))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestFlushClosesDaysIntoHistoryBeforeLive(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	statePath := store.StatePath()
	if _, err := os.Stat(persist.HistoryPath(statePath)); !os.IsNotExist(err) {
		t.Fatalf("history written before any day closed: %v", err)
	}
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 500_000}))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	var live liveFile
	liveRaw, _ := os.ReadFile(persist.UsagePath(statePath))
	if err := json.Unmarshal(liveRaw, &live); err != nil {
		t.Fatal(err)
	}
	var history historyFile
	historyRaw, _ := os.ReadFile(persist.HistoryPath(statePath))
	if err := json.Unmarshal(historyRaw, &history); err != nil {
		t.Fatal(err)
	}
	if live.HistoryThrough != "2026-10-10" || history.Through != "2026-10-10" || live.Timezone != "Asia/Shanghai" {
		t.Fatalf("watermarks: live=%q history=%q tz=%q", live.HistoryThrough, history.Through, live.Timezone)
	}
	if _, stale := live.Keys["a"].Days["2026-10-10"]; stale || live.Keys["a"].Days["2026-10-11"]["fast"].InputTokens != 500_000 {
		t.Fatalf("live days = %+v", live.Keys["a"].Days)
	}
	if history.Days["a"]["2026-10-10"]["fast"].ChargedNano != 1_000_000_000 || history.Days["b"]["2026-10-10"]["fast"].OutputTokens != 1_000_000 {
		t.Fatalf("history days = %+v", history.Days)
	}
	liveInfo, _ := os.Stat(persist.UsagePath(statePath))
	historyInfo, _ := os.Stat(persist.HistoryPath(statePath))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(persist.UsagePath(statePath)); !again.ModTime().Equal(liveInfo.ModTime()) {
		t.Fatal("a clean flush rewrote the live file")
	}
	if again, _ := os.Stat(persist.HistoryPath(statePath)); !again.ModTime().Equal(historyInfo.ModTime()) {
		t.Fatal("a clean flush rewrote the history file")
	}
	reloaded := reopenStore(t, clock, statePath)
	if !reflect.DeepEqual(bucketSums(reloaded.usage.snapshot()), bucketSums(store.usage.snapshot())) {
		t.Fatal("reloaded buckets differ from memory")
	}
}

// Three crash shapes around a day boundary: the history write succeeded
// and the live write failed; both succeeded; neither ran. After restart the
// buckets must equal memory except for the last unflushed window, with no
// double counting.
func TestCrashConsistencyAroundDayBoundary(t *testing.T) {
	loc := mustShanghai(t)
	for _, scenario := range []string{"history_only", "both", "neither"} {
		t.Run(scenario, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
			store := twoDayStore(t, clock)
			statePath := store.StatePath()
			clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
			store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 500_000}))
			before := bucketSums(store.usage.snapshot())
			lostWindow := map[[3]string]bool{}
			switch scenario {
			case "history_only":
				livePath := persist.UsagePath(statePath)
				liveRaw, _ := os.ReadFile(livePath)
				if err := os.Remove(livePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(livePath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := store.FlushUsage(); err == nil || store.Health().FlushFailures != 1 || store.Health().LastFlushError == "" {
					t.Fatalf("live failure not reported: err=%v health=%+v", err, store.Health())
				}
				if err := os.Remove(livePath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(livePath, liveRaw, 0o600); err != nil {
					t.Fatal(err)
				}
				lostWindow[[3]string{"a", "2026-10-11", "fast"}] = true
			case "both":
				if err := store.FlushUsage(); err != nil {
					t.Fatal(err)
				}
			case "neither":
				lostWindow[[3]string{"a", "2026-10-11", "fast"}] = true
			}
			reloaded := reopenStore(t, clock, statePath)
			after := bucketSums(reloaded.usage.snapshot())
			for key, want := range before {
				got, ok := after[key]
				if lostWindow[key] {
					if ok {
						t.Fatalf("%v: unflushed window reappeared: %+v", key, got)
					}
					continue
				}
				if !ok || got != want {
					t.Fatalf("%v: got %+v want %+v", key, got, want)
				}
			}
			if len(after) > len(before) {
				t.Fatalf("restart invented buckets: %+v", after)
			}
			if summary := reloaded.UsageSummaryFor(keyByID(t, reloaded, "b")); !nearly(summary.WeeklyUSD, 2) || !nearly(summary.DailyUSD, 0) {
				t.Fatalf("cycles after restart = %+v", summary)
			}
		})
	}
}

// A crash between DeleteKey's state write and its live write leaves the
// deleted key's periods in the live file. The restart forgets them, so a key
// recreated under the id starts its own periods at its creation.
func TestDeletedKeyPeriodsLeftInLiveFileAreForgottenOnRestart(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	statePath := store.StatePath()
	liveBeforeDelete, _ := os.ReadFile(persist.UsagePath(statePath))
	if err := store.DeleteKey("b"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persist.UsagePath(statePath), liveBeforeDelete, 0o600); err != nil {
		t.Fatal(err)
	}
	if dataset, err := LoadUsage(statePath); err != nil || dataset.Keys["b"].Cycles == nil {
		t.Fatalf("precondition: the live file should still hold b's periods (err=%v)", err)
	}
	clock.Advance(24 * time.Hour)
	reloaded := reopenStore(t, clock, statePath)
	if err := reloaded.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Keys["b"] == nil || dataset.Keys["b"].Cycles != nil || dataset.Keys["b"].Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("restart kept the deleted key's periods: %+v", dataset.Keys["b"])
	}
	again := testKey(t, "b", "cpa_b_again")
	if err := reloaded.UpsertKey(again); err != nil {
		t.Fatal(err)
	}
	summary := reloaded.UsageSummaryFor(keyByID(t, reloaded, "b"))
	if summary.DailyUSD != 0 || summary.WeeklyUSD != 0 || summary.MonthlyUSD != 0 || summary.Cycles[1].ResetKind != "initial" || !summary.Cycles[1].StartedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("recreated key inherited periods: %+v", summary)
	}
}

// DeleteKey writes the live file itself: a key recreated under the id before
// the next periodic flush, followed by a crash, must not find the old
// periods on restart. When that write fails the key is still deleted; the
// failure is counted and the flusher retries.
func TestDeleteKeyPersistsClearedPeriodsBeforeRecreation(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	statePath := store.StatePath()
	if err := store.DeleteKey("b"); err != nil {
		t.Fatal(err)
	}
	if dataset, err := LoadUsage(statePath); err != nil || dataset.Keys["b"].Cycles != nil || dataset.Keys["b"].Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("live file after delete = %+v err=%v, want b without periods", dataset.Keys["b"], err)
	}
	clock.Advance(time.Hour)
	if err := store.UpsertKey(testKey(t, "b", "cpa_b_again")); err != nil {
		t.Fatal(err)
	}
	// Crash before the next flush: only the files written so far survive.
	reloaded := reopenStore(t, clock, statePath)
	summary := reloaded.UsageSummaryFor(keyByID(t, reloaded, "b"))
	if summary.WeeklyUSD != 0 || summary.MonthlyUSD != 0 || summary.Cycles[1].ResetKind != "initial" || !summary.Cycles[1].StartedAt.Equal(clock.Now()) {
		t.Fatalf("recreated key inherited the deleted periods: %+v", summary)
	}

	livePath := persist.UsagePath(statePath)
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(livePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteKey("a"); err != nil {
		t.Fatalf("delete with a failing live write = %v, want the key deleted anyway", err)
	}
	if reloaded.FindByAPIKey("cpa_a") != nil || len(reloaded.Keys()) != 1 {
		t.Fatalf("key survived its delete: %+v", reloaded.Keys())
	}
	if events, _ := reloaded.AuditEvents("a", "", 1); len(events) != 1 || events[0].Action != "delete_key" {
		t.Fatalf("delete audit = %+v", events)
	}
	if health := reloaded.Health(); health.FlushFailures != 1 || !strings.Contains(health.LastFlushError, "live usage") {
		t.Fatalf("health = %+v, want the failed live write counted", health)
	}
}

// A record dated on a closed day (clock rollback) lands on the first open
// day, so closed buckets never change and the record reaches the live file
// through any live write, here the one a manual reset makes under the lock.
func TestLateRecordLandsOnFirstOpenDay(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	historyInfo, _ := os.Stat(persist.HistoryPath(store.StatePath()))
	clock.Set(time.Date(2026, 10, 10, 23, 59, 0, 0, loc))
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 123}))
	clock.Set(time.Date(2026, 10, 11, 0, 6, 0, 0, loc))
	if _, err := store.ResetUsageWindow("b", UsageResetDaily, nil); err != nil {
		t.Fatal(err)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	_, history, _ := reloaded.UsageHistoryFor("a", 2)
	if history[0].Date != "2026-10-10" || history[0].CallCount != 1 || history[0].InputTokens != 1_000_000 || history[1].Date != "2026-10-11" || history[1].CallCount != 1 || history[1].InputTokens != 123 {
		t.Fatalf("late record after restart = %+v", history)
	}
	if again, _ := os.Stat(persist.HistoryPath(store.StatePath())); !again.ModTime().Equal(historyInfo.ModTime()) {
		t.Fatal("the late record rewrote the closed history")
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(persist.HistoryPath(store.StatePath())); !again.ModTime().Equal(historyInfo.ModTime()) {
		t.Fatal("the next flush rewrote the closed history")
	}
}

// Retention alone makes the history file stale: when an old day expires on a
// day without any new closed data, the next flush still rewrites the file.
func TestRetentionEvictionRewritesHistoryWithoutNewClosedDays(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	// 2026-10-10 falls out of the 90-day window; nothing was recorded since.
	clock.Set(time.Date(2027, 1, 8, 9, 0, 0, 0, loc))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Through != "2027-01-07" || len(dataset.Keys["a"].Days) != 0 || len(dataset.Keys["b"].Days) != 0 {
		t.Fatalf("history after retention = through %s keys %+v", dataset.Through, dataset.Keys)
	}
}

func TestShutdownWritesHistoryThenLive(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	store.StartUsageFlusher()
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	store.RecordUsage(tokenEvent("b", "codex", "fast", UsageDetail{OutputTokens: 100}))
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Through != "2026-10-10" || dataset.Keys["b"].Days["2026-10-11"]["fast"].OutputTokens != 100 || dataset.Keys["a"].Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("dataset after shutdown = through %s keys %+v", dataset.Through, dataset.Keys)
	}
	if health := store.Health(); health.LastFlushAt == "" || health.LastFlushError != "" {
		t.Fatalf("health = %+v", health)
	}
}

func TestHistoryWriteFailureKeepsClosedDaysInLiveAndRetries(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	historyPath := persist.HistoryPath(store.StatePath())
	if err := os.Mkdir(historyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	if err := store.FlushUsage(); err == nil || !strings.Contains(err.Error(), "usage history") {
		t.Fatalf("history failure = %v", err)
	}
	if err := os.Remove(historyPath); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Through != "" || dataset.Keys["a"].Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("closed day missing from live after history failure: %+v", dataset)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatalf("retry after the history path was freed: %v", err)
	}
	if dataset, err = LoadUsage(store.StatePath()); err != nil || dataset.Through != "2026-10-10" {
		t.Fatalf("history not written on retry: through=%q err=%v", dataset.Through, err)
	}
	if health := store.Health(); health.FlushFailures != 1 || health.LastFlushError != "" || health.LastFlushAt == "" {
		t.Fatalf("health = %+v", health)
	}
}

func TestLoadRejectsStructuralDamage(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := twoDayStore(t, clock)
	statePath := store.StatePath()
	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, loc))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	livePath, historyPath := persist.UsagePath(statePath), persist.HistoryPath(statePath)
	liveRaw, _ := os.ReadFile(livePath)
	historyRaw, _ := os.ReadFile(historyPath)
	configure := func(timezone string) error {
		s := NewStore()
		s.SetClock(clock.Now)
		return s.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: timezone})
	}
	for name, tc := range map[string]struct {
		prepare func()
		want    string
	}{
		"missing history": {func() { _ = os.Remove(historyPath) }, "history"},
		"history dataset mismatch": {func() {
			_ = os.WriteFile(historyPath, []byte(strings.Replace(string(historyRaw), `"dataset_id":"`, `"dataset_id":"x`, 1)), 0o600)
		}, "dataset_id mismatch"},
		"negative bucket": {func() {
			_ = os.WriteFile(historyPath, []byte(strings.Replace(string(historyRaw), `"input_tokens":1000000`, `"input_tokens":-1`, 1)), 0o600)
		}, "nonnegative"},
		"unknown field": {func() {
			_ = os.WriteFile(livePath, []byte(strings.Replace(string(liveRaw), `"version":7,`, `"version":7,"extra":1,`, 1)), 0o600)
		}, "unknown field"},
		"old version": {func() {
			_ = os.WriteFile(livePath, []byte(strings.Replace(string(liveRaw), `"version":7,`, `"version":5,`, 1)), 0o600)
		}, "convert the dataset offline"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				_ = os.WriteFile(livePath, liveRaw, 0o600)
				_ = os.WriteFile(historyPath, historyRaw, 0o600)
			}()
			tc.prepare()
			if err := configure("Asia/Shanghai"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := configure("UTC"); err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("timezone mismatch error = %v", err)
	}
	if err := configure("Asia/Shanghai"); err != nil {
		t.Fatalf("intact dataset rejected: %v", err)
	}
}

// Billing keeps running while a flush shares closed days with the writer;
// the race detector checks the hand-off.
func TestRecordingDuringFlushAcrossMidnight(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 23, 59, 59, 0, loc))
	store := twoDayStore(t, clock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		headers := bearer("cpa_a")
		for i := 0; i < 2000; i++ {
			store.Authenticate("POST", "/v1/chat/completions", headers, nil)
			store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 10}))
			if i == 500 {
				clock.Set(time.Date(2026, 10, 11, 0, 0, 1, 0, loc))
			}
			if i == 1200 {
				clock.Set(time.Date(2026, 10, 10, 23, 59, 58, 0, loc)) // clock rollback onto a closed day
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if err := store.FlushUsage(); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if !reflect.DeepEqual(bucketSums(reloaded.usage.snapshot()), bucketSums(store.usage.snapshot())) {
		t.Fatal("reloaded buckets differ from memory after concurrent flushes")
	}
	var calls int64
	for key, bucket := range bucketSums(reloaded.usage.snapshot()) {
		if key[0] == "a" {
			calls += bucket.Calls
		}
	}
	if calls != 2001 {
		t.Fatalf("calls across both days = %d, want 2001", calls)
	}
}
