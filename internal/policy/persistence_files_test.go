package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// writeJSON writes a file in the spec §3.5 shape built from plain maps, so
// the tests exercise the documented file contract rather than the writers.
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func bucketJSON(calls, input int64, chargedNano int64) map[string]any {
	return map[string]any{"calls": calls, "input_tokens": input, "charged_nano": chargedNano}
}

func cyclesJSON(loc *time.Location, day, weekStart, monthStart time.Time, kind string, byModel map[string]any) map[string]any {
	cycle := func(start time.Time, days int) map[string]any {
		return map[string]any{"started_at": start.Format(time.RFC3339), "resets_at": time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, days).Format(time.RFC3339), "reset_kind": kind, "by_model": byModel}
	}
	return map[string]any{"daily": cycle(day, 1), "weekly": cycle(weekStart, 7), "monthly": cycle(monthStart, 30)}
}

// A handwritten v7 dataset: the history reaches 2026-10-10, the live file
// was last written before that history write (history_through "") and
// still carries a stale copy of 2026-10-10 plus the open day 2026-10-11.
func writeSplitDataset(t *testing.T, dir string, loc *time.Location) (statePath string) {
	t.Helper()
	statePath = filepath.Join(dir, "state.json")
	key := testKey(t, "a", "cpa_a")
	key.CreatedAt, key.UpdatedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	writeJSON(t, statePath, map[string]any{"version": 7, "dataset_id": "ds-1", "keys": []KeyConfig{key}, "models": []ModelPrice{tokenPrice("fast", "", 1, 0)}, "updated_at": "2026-10-11T00:00:00Z"})
	writeJSON(t, persist.HistoryPath(statePath), map[string]any{
		"version": 7, "dataset_id": "ds-1", "updated_at": "2026-10-11T00:00:01Z", "through": "2026-10-10",
		"days": map[string]any{
			"a":    map[string]any{"2026-10-09": map[string]any{"fast": bucketJSON(1, 1000, 1_000)}, "2026-10-10": map[string]any{"fast": bucketJSON(2, 2000, 2_000)}},
			"gone": map[string]any{"2026-10-10": map[string]any{"fast": bucketJSON(5, 5000, 5_000)}},
		},
	})
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, loc)
	writeJSON(t, persist.UsagePath(statePath), map[string]any{
		"version": 7, "dataset_id": "ds-1", "updated_at": "2026-10-11T00:00:00Z", "timezone": "Asia/Shanghai", "history_through": "",
		"keys": map[string]any{"a": map[string]any{
			"cycles": cyclesJSON(loc, day, time.Date(2026, 10, 8, 0, 0, 0, 0, loc), time.Date(2026, 10, 1, 0, 0, 0, 0, loc), "automatic", map[string]any{"fast": bucketJSON(1, 500, 500)}),
			"days":   map[string]any{"2026-10-10": map[string]any{"fast": bucketJSON(9, 9000, 9_000)}, "2026-10-11": map[string]any{"fast": bucketJSON(1, 500, 500)}},
		}},
	})
	return statePath
}

func TestLoadMergesHistoryAndLiveByWatermark(t *testing.T) {
	loc := mustShanghai(t)
	statePath := writeSplitDataset(t, t.TempDir(), loc)
	dataset, err := LoadUsage(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Through != "2026-10-10" || dataset.Timezone != "Asia/Shanghai" || dataset.DatasetID != "ds-1" {
		t.Fatalf("dataset header = through %q tz %q id %q", dataset.Through, dataset.Timezone, dataset.DatasetID)
	}
	a := dataset.Keys["a"]
	if a.Days["2026-10-10"]["fast"].Calls != 2 || a.Days["2026-10-09"]["fast"].Calls != 1 || a.Days["2026-10-11"]["fast"].Calls != 1 || len(a.Days) != 3 {
		t.Fatalf("history must own 2026-10-10 and live only 2026-10-11: %+v", a.Days)
	}
	if a.Cycles == nil || a.Cycles.Daily.ByModel["fast"].InputTokens != 500 || dataset.Keys["gone"].Cycles != nil || dataset.Keys["gone"].Days["2026-10-10"]["fast"].Calls != 5 {
		t.Fatalf("cycles = %+v gone = %+v", a.Cycles, dataset.Keys["gone"])
	}
	clock := newTestClock(time.Date(2026, 10, 11, 9, 0, 0, 0, loc))
	store := reopenStore(t, clock, statePath)
	stats := store.Stats(3)
	if stats.Totals.Calls != 9 || len(stats.Keys) != 2 || stats.Keys[0].ID != "gone" || !stats.Keys[0].Deleted || stats.Keys[1].Deleted {
		t.Fatalf("stats = %+v", stats)
	}
	if summary := store.UsageSummaryFor(keyByID(t, store, "a")); summary.DailyUSD != 0.0000005 {
		t.Fatalf("summary = %+v", summary)
	}
	// Nothing closed and nothing changed: a clean flush leaves both files
	// alone, and the stale live day stays shadowed by the history watermark.
	liveInfo, _ := os.Stat(persist.UsagePath(statePath))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(persist.UsagePath(statePath)); !again.ModTime().Equal(liveInfo.ModTime()) {
		t.Fatal("a clean flush rewrote the live file")
	}
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 1000}))
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	var live liveFile
	liveRaw, _ := os.ReadFile(persist.UsagePath(statePath))
	if err := json.Unmarshal(liveRaw, &live); err != nil {
		t.Fatal(err)
	}
	if live.HistoryThrough != "2026-10-10" || len(live.Keys["a"].Days) != 1 || live.Keys["a"].Days["2026-10-11"]["fast"].Calls != 2 || live.Keys["gone"].Cycles != nil {
		t.Fatalf("live after a dirty flush = %+v", live)
	}
	if reloaded, err := LoadUsage(statePath); err != nil || reloaded.Through != "2026-10-10" || len(reloaded.Keys["a"].Days) != 3 || reloaded.Keys["a"].Days["2026-10-10"]["fast"].Calls != 2 {
		t.Fatalf("reload after flush = %+v err=%v", reloaded, err)
	}
}

func TestLoadRejectsInconsistentPairs(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 11, 9, 0, 0, 0, loc))
	configure := func(statePath string) error {
		s := NewStore()
		s.SetClock(clock.Now)
		return s.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: "Asia/Shanghai"})
	}
	for name, tc := range map[string]struct {
		mutate func(statePath string)
		want   string
	}{
		"live watermark beyond history": {func(statePath string) {
			raw, _ := os.ReadFile(persist.UsagePath(statePath))
			_ = os.WriteFile(persist.UsagePath(statePath), []byte(strings.Replace(string(raw), `"history_through":""`, `"history_through":"2026-10-11"`, 1)), 0o600)
		}, "only reaches 2026-10-10"},
		"history day after its watermark": {func(statePath string) {
			raw, _ := os.ReadFile(persist.HistoryPath(statePath))
			_ = os.WriteFile(persist.HistoryPath(statePath), []byte(strings.Replace(string(raw), `"2026-10-09"`, `"2026-10-12"`, 1)), 0o600)
		}, "after through"},
		"history with the previous version": {func(statePath string) {
			raw, _ := os.ReadFile(persist.HistoryPath(statePath))
			_ = os.WriteFile(persist.HistoryPath(statePath), []byte(strings.Replace(string(raw), `"version":7`, `"version":6`, 1)), 0o600)
		}, "version 6"},
		"state with another dataset": {func(statePath string) {
			raw, _ := os.ReadFile(statePath)
			_ = os.WriteFile(statePath, []byte(strings.Replace(string(raw), `"dataset_id":"ds-1"`, `"dataset_id":"ds-2"`, 1)), 0o600)
		}, "dataset_id mismatch"},
		"live file missing": {func(statePath string) { _ = os.Remove(persist.UsagePath(statePath)) }, "read live usage"},
		"live with invalid timezone": {func(statePath string) {
			raw, _ := os.ReadFile(persist.UsagePath(statePath))
			_ = os.WriteFile(persist.UsagePath(statePath), []byte(strings.Replace(string(raw), `"timezone":"Asia/Shanghai"`, `"timezone":"Mars/Olympus"`, 1)), 0o600)
		}, "timezone"},
		"live cycle with a foreign reset date": {func(statePath string) {
			raw, _ := os.ReadFile(persist.UsagePath(statePath))
			_ = os.WriteFile(persist.UsagePath(statePath), []byte(strings.Replace(string(raw), `"resets_at":"2026-10-12T00:00:00+08:00"`, `"resets_at":"2026-10-12T01:00:00+08:00"`, 1)), 0o600)
		}, "reset date"},
		"state without its ledger": {func(statePath string) {
			_ = os.Remove(statePath)
		}, "without its paired state file"},
		"history watermark one day ahead of the clock": {func(statePath string) {
			raw, _ := os.ReadFile(persist.HistoryPath(statePath))
			_ = os.WriteFile(persist.HistoryPath(statePath), []byte(strings.Replace(string(raw), `"through":"2026-10-10"`, `"through":"2026-10-12"`, 1)), 0o600)
		}, "after today (2026-10-11)"},
	} {
		t.Run(name, func(t *testing.T) {
			statePath := writeSplitDataset(t, t.TempDir(), loc)
			tc.mutate(statePath)
			if err := configure(statePath); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A watermark on today itself loads (a restart after the clock rolled back
// onto the day the history was written). Today is closed then, so a record
// lands on the first open day, and reports must reach that day or the
// record stays invisible until the clock catches up.
func TestWatermarkOnTodayKeepsNewRecordsVisible(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 11, 9, 0, 0, 0, loc))
	statePath := writeSplitDataset(t, t.TempDir(), loc)
	raw, _ := os.ReadFile(persist.HistoryPath(statePath))
	_ = os.WriteFile(persist.HistoryPath(statePath), []byte(strings.Replace(string(raw), `"through":"2026-10-10"`, `"through":"2026-10-11"`, 1)), 0o600)
	store := reopenStore(t, clock, statePath)
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 1000}))
	store.RecordUsage(tokenEvent("a", "codex", "unpriced", UsageDetail{InputTokens: 1}))
	_, history, _ := store.UsageHistoryFor("a", 2)
	if len(history) != 2 || history[0].Date != "2026-10-11" || history[0].CallCount != 0 || history[1].Date != "2026-10-12" || history[1].CallCount != 2 {
		t.Fatalf("history = %+v, want the records on the first open day", history)
	}
	stats := store.Stats(1)
	if stats.From != "2026-10-12" || stats.To != "2026-10-12" || stats.Totals.Calls != 2 || len(stats.Days) != 1 || stats.Days[0].Calls != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if unpriced := store.UnpricedModels(); len(unpriced) != 1 || unpriced[0].LastSeen != "2026-10-12" {
		t.Fatalf("unpriced = %+v", unpriced)
	}
}

func TestLiveWriteFailureIsReportedAndRetried(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "a", "cpa_a")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	livePath := persist.UsagePath(store.StatePath())
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(livePath, 0o700); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	// The first failure and the recovery reach the plugin's log once each,
	// not every interval, and whichever flush sees them: a flusher started
	// again after a rolled-back replacement does not report the failure
	// again, and a recovery seen by the quiesce or shutdown cut is reported.
	var logged []string
	store.SetLogf(func(level, message string) { logged = append(logged, level+" "+message) })
	for i := 1; i <= 3; i++ {
		flusher := &usageFlusher{store: store}
		flusher.flush()
		if health := store.Health(); health.FlushFailures != int64(i) || !strings.Contains(health.LastFlushError, "live usage") || health.LastFlushAt != "" {
			t.Fatalf("health after failure %d = %+v", i, health)
		}
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "error cpa-key-policy: usage ledger cannot be saved") || !strings.Contains(logged[0], "live usage") {
		t.Fatalf("logged while failing = %q", logged)
	}
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := store.stopUsageFlusher(); err != nil {
		t.Fatal(err)
	}
	if health := store.Health(); health.FlushFailures != 3 || health.LastFlushError != "" || health.LastFlushAt == "" {
		t.Fatalf("health after recovery = %+v", health)
	}
	(&usageFlusher{store: store}).flush()
	if len(logged) != 2 || logged[1] != "info cpa-key-policy: usage ledger saved again" {
		t.Fatalf("logged after recovery = %q", logged)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if summary := reloaded.UsageSummaryFor(keyByID(t, reloaded, "a")); summary.DailyUSD != 1 {
		t.Fatalf("usage lost across the failed flushes: %+v", summary)
	}
	info, err := os.Stat(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(livePath); !again.ModTime().Equal(info.ModTime()) {
		t.Fatal("a clean flush rewrote the live file")
	}
}

// Interrupted atomic writes leave temp files next to the targets; only the
// stale ones for our own files are removed at startup.
func TestConfigureRemovesOnlyStaleOwnTempFiles(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, nil, nil)
	dir := filepath.Dir(store.StatePath())
	stale := []string{filepath.Join(dir, ".state.json.tmp-a"), filepath.Join(dir, ".cpa-key-policy-usage.json.tmp-b"), filepath.Join(dir, ".cpa-key-policy-history.json.tmp-c")}
	kept := []string{filepath.Join(dir, ".cpa-key-policy-usage.json.tmp-recent"), filepath.Join(dir, ".other.json.tmp-d")}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range append(append([]string{}, stale...), kept...) {
		if err := os.WriteFile(path, []byte("tmp"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range append(stale, kept[1]) {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Temp files are swept when the dataset is loaded, which a restart does.
	reopened := NewStore()
	reopened.SetClock(clock.Now)
	if err := reopened.Configure(Config{Enabled: true, StateFile: store.StatePath(), UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range stale {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale temp survived: %s", path)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated or recent temp removed: %s", path)
		}
	}
}

// A loaded dataset keeps its usage timezone: another one is refused without
// reloading the files, which would hold authentication and billing up on
// every config apply only to fail the same way.
func TestConfigureRefusesAnotherTimezoneWithoutReloading(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, nil)
	statePath := store.StatePath()
	store.lifecycleMu.RLock() // what a request under way holds
	done := make(chan error, 1)
	go func() {
		done <- store.Configure(Config{Enabled: false, StateFile: statePath, UsageTimezone: "UTC"})
	}()
	var err error
	select {
	case err = <-done:
		store.lifecycleMu.RUnlock()
	case <-time.After(5 * time.Second):
		store.lifecycleMu.RUnlock()
		t.Fatalf("a rejected timezone waited for requests under way: %v", <-done)
	}
	if err == nil || !strings.Contains(err.Error(), `usage ledger uses quota timezone "Asia/Shanghai"`) {
		t.Fatalf("configure with another timezone = %v", err)
	}
	if store.StatePath() != statePath || !store.Enabled() || store.Timezone() != "Asia/Shanghai" {
		t.Fatalf("rejected configuration changed the store: path=%q enabled=%v zone=%q", store.StatePath(), store.Enabled(), store.Timezone())
	}
	_ = store.Shutdown()
}
