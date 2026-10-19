package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

func TestDailyBoundaryPreservesLongerCycles(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 8, 7, 23, 30, 0, 0, loc))
	key := testKey(t, "rebirth", "cpa_rebirth")
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})

	store.RecordUsage(tokenEvent("rebirth", "codex", "fast", UsageDetail{InputTokens: 72_350_000}))
	clock.Set(time.Date(2026, 8, 8, 0, 30, 0, 0, loc))
	store.RecordUsage(tokenEvent("rebirth", "codex", "fast", UsageDetail{InputTokens: 31_100_000}))

	summary := store.UsageSummaryFor(key)
	if !nearly(summary.DailyUSD, 31.10) || !nearly(summary.WeeklyUSD, 103.45) || !nearly(summary.MonthlyUSD, 103.45) {
		t.Fatalf("totals = %+v", summary)
	}
	wantBoundary := time.Date(2026, 8, 9, 0, 0, 0, 0, loc)
	if !summary.NextAccountingBoundaryAt.Equal(wantBoundary) {
		t.Fatalf("next boundary = %s, want %s", summary.NextAccountingBoundaryAt, wantBoundary)
	}
}

func TestWeeklyAndMonthlyCyclesAreAnchoredAtCreation(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 8, 1, 12, 0, 0, 0, loc))
	key := testKey(t, "rolling", "cpa_rolling")
	key.CreatedAt = time.Date(2026, 8, 1, 12, 0, 0, 0, loc)
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	for day := 1; day <= 8; day++ {
		clock.Set(time.Date(2026, 8, day, 12, 0, 0, 0, loc))
		store.RecordUsage(tokenEvent("rolling", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	}
	summary := store.UsageSummaryFor(key)
	if !nearly(summary.DailyUSD, 1) || !nearly(summary.WeeklyUSD, 1) || !nearly(summary.MonthlyUSD, 8) {
		t.Fatalf("fixed windows = %+v, want daily=1 weekly=1 monthly=8", summary)
	}
	if summary.Cycles[1].ResetsAt.Day() != 15 || summary.Cycles[1].ResetKind != "automatic" || summary.Cycles[2].ResetsAt.Day() != 31 {
		t.Fatalf("cycles = %+v", summary.Cycles)
	}
}

func TestCycleScheduleSurvivesManualResetAndDowntime(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 9, 12, 14, 0, 0, 0, loc))
	key := testKey(t, "key", "cpa_key")
	key.WeeklyLimitUSD, key.MonthlyLimitUSD = 10, 20
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("model", "", 1, 0)})
	store.RecordUsage(tokenEvent("key", "codex", "model", UsageDetail{InputTokens: 5_000_000}))
	clock.Advance(3 * 24 * time.Hour)
	result, err := store.ResetUsageWindow("key", UsageResetWeekly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.AfterWeeklyUSD != 0 || result.AfterMonthlyUSD != 5 || result.BeforeWeeklyUSD != 5 || result.NextAccountingBoundaryAt.Day() != 22 {
		t.Fatalf("manual reset: %+v", result)
	}
	_, history, _ := store.UsageHistoryFor("key", 4)
	if len(history) != 4 || history[0].TotalUSD != 5 || history[0].ByModel["model"].CallCount != 1 {
		t.Fatalf("manual reset removed history: %+v", history)
	}
	clock.Set(time.Date(2026, 10, 8, 12, 0, 0, 0, loc))
	summary := store.UsageSummaryFor(keyByID(t, store, "key"))
	if summary.Cycles[1].ResetsAt.Day() != 13 || summary.Cycles[2].ResetsAt.Day() != 12 || summary.MonthlyUSD != 5 || summary.Cycles[2].ResetKind != "initial" {
		t.Fatalf("downtime shifted independent schedules: %+v", summary)
	}
}

func TestRetentionKeepsNinetyDays(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 6, 1, 12, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "retained", "cpa_retained")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	for day := 0; day <= UsageRetentionDays; day++ {
		clock.Set(time.Date(2026, 6, 1, 12, 0, 0, 0, loc).AddDate(0, 0, day))
		store.RecordUsage(tokenEvent("retained", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	}
	_, history, _ := store.UsageHistoryFor("retained", 90)
	if len(history) != UsageRetentionDays || history[0].Date != "2026-06-02" || history[0].CallCount != 1 {
		t.Fatalf("history = %d days starting %s", len(history), history[0].Date)
	}
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(dataset.Keys["retained"].Days); got != UsageRetentionDays || dataset.Through != "2026-08-29" {
		t.Fatalf("persisted days = %d through %s", got, dataset.Through)
	}
}

// Only key registration opens periods. Reads of an unknown or deleted id
// answer with throwaway periods, a record for a deleted key reaches its day
// history only, and the live file never regains the deleted key's periods.
func TestReadsDoNotRegisterPeriodsAndDeletedKeysKeepOnlyDays(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "a", "cpa_a"), testKey(t, "b", "cpa_b")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	store.RecordUsage(tokenEvent("b", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	snapshot := store.Keys()
	if err := store.DeleteKey("b"); err != nil {
		t.Fatal(err)
	}
	limits := quotaLimits{DailyUSD: 1}
	for _, key := range snapshot {
		key.DailyLimitUSD = 1
		store.UsageSummaryFor(key)
	}
	store.usage.modelUsage("b")
	store.usage.quotaDenial("b", limits)
	store.usage.summary("nobody", limits)
	store.usage.modelUsage("nobody")
	if _, err := store.ResetUsageWindow("b", UsageResetDaily, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("reset of a deleted key = %v", err)
	}
	store.usage.record("ghost", "fast", Bucket{Calls: 1}, false)
	entries := store.usage.snapshot()
	if b := entries["b"]; b == nil || b.Cycles != nil || b.Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("deleted key after reads = %+v", b)
	}
	if ghost := entries["ghost"]; ghost == nil || ghost.Cycles != nil || ghost.Days["2026-10-10"]["fast"].Calls != 1 {
		t.Fatalf("record for an unregistered id = %+v", ghost)
	}
	if _, exists := entries["nobody"]; exists {
		t.Fatal("reading an unknown id created a ledger entry")
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	dataset, err := LoadUsage(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Keys["b"].Cycles != nil || dataset.Keys["ghost"].Cycles != nil || dataset.Keys["a"].Cycles == nil {
		t.Fatalf("live file periods: b=%v ghost=%v a=%v", dataset.Keys["b"].Cycles != nil, dataset.Keys["ghost"].Cycles != nil, dataset.Keys["a"].Cycles != nil)
	}
}

func TestDeleteKeyUnderConcurrentReadsAndRecordsLeavesNoPeriods(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "b", "cpa_b")
	key.DailyLimitUSD = 100
	store := newStore(t, clock, []KeyConfig{testKey(t, "a", "cpa_a"), key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	snapshot := keyByID(t, store, "b")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				store.UsageSummaryFor(snapshot)
				store.RecordUsage(tokenEvent("b", "codex", "fast", UsageDetail{InputTokens: 10}))
				admitRequest(store, "POST", "/v1/chat/completions", bearer("cpa_b"), nil)
				store.ModelUsageFor("b")
			}
		}()
	}
	if err := store.DeleteKey("b"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if b := store.usage.snapshot()["b"]; b != nil && b.Cycles != nil {
		t.Fatalf("deleted key regained periods: %+v", b.Cycles)
	}
}

// Reports scan the ledger outside its lock: a record made while a scan visits
// buckets neither waits for the scan nor changes what the scan reports, and
// the closed day the scan shares is never written.
func TestVisitRangeRunsOutsideTheLedgerLock(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "a", "cpa_a")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	clock.Advance(24 * time.Hour)
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(tokenEvent("a", "codex", "fast", UsageDetail{InputTokens: 2_000_000}))
	ledger := store.usage
	ledger.mu.Lock()
	closedThrough := ledger.closedThrough
	ledger.mu.Unlock()
	from, to := ledger.reportRange(2)
	visited := make(map[string]Bucket)
	ledger.visitRange(from, to, func(_, date, _ string, bucket Bucket) {
		if !ledger.mu.TryLock() {
			t.Fatal("visit runs while the ledger lock is held")
		}
		ledger.mu.Unlock()
		ledger.record("a", "fast", Bucket{Calls: 1}, false)
		visited[date] = bucket
	})
	if len(visited) != 2 || closedThrough != "2026-10-10" || visited["2026-10-10"].ChargedNano != USDToNano(1) || visited["2026-10-11"].Calls != 1 || visited["2026-10-11"].ChargedNano != USDToNano(2) {
		t.Fatalf("visited = %+v closed through %s", visited, closedThrough)
	}
	if got := store.usage.snapshot()["a"].Days["2026-10-11"]["fast"].Calls; got != 3 {
		t.Fatalf("records made during the scan: today's calls = %d, want 3", got)
	}
}

func TestResetUsageWindowRollsBackWhenPersistenceFails(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 8, 2, 9, 30, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "resettable", "cpa_resettable")}, []ModelPrice{tokenPrice("fast", "", 1, 1)})
	store.RecordUsage(tokenEvent("resettable", "codex", "fast", UsageDetail{InputTokens: 300_000}))

	livePath := persist.UsagePath(store.StatePath())
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(livePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetUsageWindow("resettable", UsageResetWeekly, nil); err == nil {
		t.Fatal("reset should fail when the live file cannot be replaced")
	}
	summary := store.UsageSummaryFor(keyByID(t, store, "resettable"))
	if !nearly(summary.DailyUSD, 0.30) || !nearly(summary.WeeklyUSD, 0.30) || summary.Cycles[1].ResetKind != "initial" {
		t.Fatalf("usage after failed reset = %+v, want in-memory rollback", summary)
	}
}

func TestResetUsageWindowRejectsStaleExpectation(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 8, 2, 9, 30, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 1)})
	summary := store.UsageSummaryFor(keyByID(t, store, "k"))
	expected := &UsageResetExpectation{StartedAt: summary.Cycles[0].StartedAt, ResetAfterManualAt: summary.Cycles[0].ResetAfterManualAt}
	clock.Advance(24 * time.Hour)
	if _, err := store.ResetUsageWindow("k", UsageResetDaily, expected); !errors.Is(err, ErrUsageResetChanged) {
		t.Fatalf("stale expectation error = %v, want ErrUsageResetChanged", err)
	}
	if _, err := store.ResetUsageWindow("missing", UsageResetDaily, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error = %v", err)
	}
	if _, err := store.ResetUsageWindow("k", UsageResetWindow("year"), nil); !errors.Is(err, ErrInvalidUsageResetWindow) {
		t.Fatalf("invalid window error = %v", err)
	}
	summary = store.UsageSummaryFor(keyByID(t, store, "k"))
	fresh := &UsageResetExpectation{StartedAt: summary.Cycles[0].StartedAt, ResetAfterManualAt: summary.Cycles[0].ResetAfterManualAt}
	result, err := store.ResetUsageWindow("k", UsageResetDaily, fresh)
	if err != nil || result.NextAccountingBoundaryAt.Day() != 4 {
		t.Fatalf("fresh expectation: result=%+v err=%v", result, err)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if got := reloaded.UsageSummaryFor(keyByID(t, reloaded, "k")); got.Cycles[0].ResetKind != "manual" {
		t.Fatalf("manual reset was not persisted: %+v", got.Cycles[0])
	}
}

// Period boundaries come from calendar dates. Where local midnight always
// exists they are the same instants, written with the same offsets, as the
// earlier midnight-plus-AddDate arithmetic reproduced here.
func TestPeriodBoundariesUnchangedWhereMidnightAlwaysExists(t *testing.T) {
	for _, loc := range []*time.Location{mustShanghai(t), time.UTC} {
		oldStartOfDay := func(at time.Time) time.Time {
			local := at.In(loc)
			return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		}
		oldNewCycles := func(at time.Time) *Cycles {
			cycles := &Cycles{}
			for _, window := range quotaWindows {
				*cycles.cycle(window) = Cycle{StartedAt: at, ResetsAt: oldStartOfDay(at).AddDate(0, 0, window.days()), ResetKind: "initial", ByModel: make(map[string]Bucket)}
			}
			cycles.Daily.StartedAt = oldStartOfDay(at)
			return cycles
		}
		oldAdvance := func(cycles *Cycles, now time.Time) {
			for _, window := range quotaWindows {
				cycle := cycles.cycle(window)
				if now.Before(cycle.ResetsAt) {
					continue
				}
				start := cycle.ResetsAt.In(loc)
				for !now.Before(start.AddDate(0, 0, window.days())) {
					start = start.AddDate(0, 0, window.days())
				}
				*cycle = Cycle{StartedAt: start, ResetsAt: start.AddDate(0, 0, window.days()), ResetKind: "automatic", ByModel: make(map[string]Bucket)}
			}
		}
		// The live file decodes times with fixed offsets; advance what it holds.
		advanced := func(cycles *Cycles, now time.Time, advance func(*Cycles, time.Time)) []byte {
			raw, _ := json.Marshal(cycles)
			var decoded Cycles
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			advance(&decoded, now)
			if err := validateCycles(&decoded, loc); err != nil {
				t.Fatalf("%s: cycles advanced to %s: %v", loc, now, err)
			}
			raw, _ = json.Marshal(&decoded)
			return raw
		}
		ledger := newUsageLedger(time.Now, loc, loc.String())
		newAdvance := func(cycles *Cycles, now time.Time) { ledger.advanceCyclesLocked(cycles, now) }
		begin := time.Date(2025, 1, 1, 0, 0, 0, 0, loc)
		for step, at := 0, begin; at.Before(begin.AddDate(2, 0, 0)); step, at = step+1, at.Add(time.Hour) {
			for _, days := range []int{-(UsageRetentionDays - 1), -1, 0, 1, 7, 30} {
				want := oldStartOfDay(at).AddDate(0, 0, days)
				if got := dayStart(at, days, loc); got.Format(time.RFC3339Nano) != want.Format(time.RFC3339Nano) || got.Location() != want.Location() {
					t.Fatalf("%s: dayStart(%s, %d) = %s, want %s", loc, at, days, got, want)
				}
				if got := ledger.dateKeyOffset(at, days); got != want.Format(dateLayout) {
					t.Fatalf("%s: dateKeyOffset(%s, %d) = %s, want %s", loc, at, days, got, want.Format(dateLayout))
				}
			}
			for _, window := range quotaWindows {
				if got, want := ledger.manualResetDeadline(at, window), oldStartOfDay(at).AddDate(0, 0, window.days()); got.Format(time.RFC3339Nano) != want.Format(time.RFC3339Nano) {
					t.Fatalf("%s: %s manual deadline at %s = %s, want %s", loc, window, at, got, want)
				}
			}
			if step%23 != 0 { // periods are opened at least once a day, at every hour in turn
				continue
			}
			cycles, old := ledger.newCycles(at, "initial"), oldNewCycles(at)
			gotRaw, _ := json.Marshal(cycles)
			if wantRaw, _ := json.Marshal(old); !bytes.Equal(gotRaw, wantRaw) {
				t.Fatalf("%s: newCycles(%s) = %s, want %s", loc, at, gotRaw, wantRaw)
			}
			for _, now := range []time.Time{old.Daily.ResetsAt, at.AddDate(0, 0, 45).Add(5 * time.Hour)} {
				if got, want := advanced(cycles, now, newAdvance), advanced(old, now, oldAdvance); !bytes.Equal(got, want) {
					t.Fatalf("%s: cycles opened %s advanced to %s = %s, want %s", loc, at, now, got, want)
				}
			}
		}
	}
}

// Where DST starts at local midnight, the transition day starts at 01:00 and
// every other period boundary stays on 00:00, so each restart reloads the
// live file the plugin wrote.
func TestMidnightDSTZonesKeepPeriodsOnDayStarts(t *testing.T) {
	for zone, transition := range map[string]string{"America/Santiago": "2025-09-07", "Africa/Cairo": "2025-04-25"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		day, _ := time.Parse(dateLayout, transition)
		clock := newTestClock(time.Date(day.Year(), day.Month(), day.Day()-2, 12, 0, 0, 0, loc))
		key := testKey(t, "k", "cpa_k")
		key.DailyLimitUSD = 10
		statePath := filepath.Join(t.TempDir(), "state.json")
		store := NewStore()
		store.SetClock(clock.Now)
		if err := store.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: zone, Keys: []KeyConfig{key}, Models: []ModelPrice{tokenPrice("fast", "", 1, 0)}}); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
			if err := store.FlushUsage(); err != nil {
				t.Fatal(err)
			}
			reopened := NewStore()
			reopened.SetClock(clock.Now)
			if err := reopened.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: zone}); err != nil {
				t.Fatalf("%s: restart at %s: %v", zone, clock.Now().In(loc), err)
			}
			summary := reopened.UsageSummaryFor(keyByID(t, reopened, "k"))
			reopened.Stop()
			if !nearly(summary.DailyUSD, 1) {
				t.Fatalf("%s: daily usage after restart at %s = %v", zone, clock.Now().In(loc), summary.DailyUSD)
			}
			for _, boundary := range []time.Time{summary.Cycles[0].StartedAt, summary.Cycles[0].ResetsAt, summary.Cycles[1].ResetsAt, summary.Cycles[2].ResetsAt} {
				local, wantHour := boundary.In(loc), 0
				if local.Format(dateLayout) == transition {
					wantHour = 1
				}
				if local.Hour() != wantHour || local.Minute() != 0 || local.Second() != 0 {
					t.Fatalf("%s: boundary %s at %s, want %02d:00", zone, local, clock.Now().In(loc), wantHour)
				}
			}
			clock.Advance(24 * time.Hour)
		}
		if got := newUsageLedger(time.Now, loc, zone).dateKeyOffset(time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc), -1); got != day.AddDate(0, 0, -1).Format(dateLayout) {
			t.Fatalf("%s: the day before %s = %s", zone, transition, got)
		}
		store.Stop()
	}
}
