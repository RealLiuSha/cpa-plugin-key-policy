package policy

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// A hot replacement: the old store bills until CPA switches to the newer one
// and a straggler reaches it afterwards; every record is billed exactly once
// and an admitted media generation is still billed per call by the newer
// store.
func TestHandoffBillsEveryRecordOnceAcrossInstances(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	prices := []ModelPrice{tokenPrice("fast", "", 1, 0), perCallPrice("grok-imagine-video", "xai", 1)}
	old := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, prices)
	oneDollar := tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1_000_000})
	bill := func(s *Store) {
		t.Helper()
		if got := s.RecordUsage(oneDollar); got != UsageRecorded {
			t.Fatalf("record = %q", got)
		}
	}

	bill(old)
	if denial := old.AdmitIntercepted(bearer("cpa_k"), "/v1/videos", "trace-1"); denial != nil {
		t.Fatalf("admission = %+v", denial)
	}
	if err := old.Quiesce(); err != nil {
		t.Fatal(err)
	}
	bill(old) // before the switch: journaled

	newer := reopenStore(t, clock, old.StatePath())
	if replayed, err := newer.AdoptHandoff(nil); err != nil || replayed != 2 {
		t.Fatalf("adopt = %d, %v; want the trace and one record", replayed, err)
	}
	generation := UsageEvent{TraceID: "trace-1", Provider: "xai", Model: "grok-imagine-video", APIKey: "k"}
	if got := newer.RecordUsage(generation); got != UsageRecorded {
		t.Fatalf("generation admitted by the old store = %q", got)
	}
	bill(old) // a straggler after the switch
	bill(newer)

	want := 5.0 // three records before the switch or straggling, one after, one generation
	deadline := time.Now().Add(5 * time.Second)
	for newer.UsageSummaryFor(keyByID(t, newer, "k")).DailyUSD < want && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := newer.UsageSummaryFor(keyByID(t, newer, "k")).DailyUSD; !nearly(got, want) {
		t.Fatalf("newer store bills $%v, want $%v", got, want)
	}
	if _, err := os.Stat(handoffPath(newer.StatePath())); !os.IsNotExist(err) {
		t.Fatalf("journal still pending after replay: %v", err)
	}
	if err := old.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := newer.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if got := reopenStore(t, clock, newer.StatePath()).UsageSummaryFor(keyByID(t, newer, "k")).DailyUSD; !nearly(got, want) {
		t.Fatalf("after restart $%v, want $%v", got, want)
	}
}

// Neither the next hot replacement nor a restart, each soon after taking
// over, replays a journal the newer store already billed.
func TestAdoptedJournalIsBilledOnce(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	old := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	event := tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1_000_000})
	old.RecordUsage(event)
	if err := old.Quiesce(); err != nil {
		t.Fatal(err)
	}
	old.RecordUsage(event) // journaled
	if err := old.Shutdown(); err != nil {
		t.Fatal(err)
	}

	second := reopenStore(t, clock, old.StatePath())
	if replayed, err := second.AdoptHandoff(nil); err != nil || replayed != 1 {
		t.Fatalf("second adopt = %d, %v", replayed, err)
	}
	if err := second.Quiesce(); err != nil { // the next replacement, within a second
		t.Fatal(err)
	}
	if err := second.Shutdown(); err != nil {
		t.Fatal(err)
	}

	third := reopenStore(t, clock, old.StatePath())
	if replayed, err := third.AdoptHandoff(nil); err != nil || replayed != 0 {
		t.Fatalf("third adopt = %d, %v; want nothing left to replay", replayed, err)
	}
	if err := third.Shutdown(); err != nil { // a restart right after
		t.Fatal(err)
	}
	restarted := reopenStore(t, clock, old.StatePath())
	if replayed, err := restarted.AdoptHandoff(nil); err != nil || replayed != 0 {
		t.Fatalf("adopt after restart = %d, %v", replayed, err)
	}
	if got := restarted.UsageSummaryFor(keyByID(t, restarted, "k")).DailyUSD; !nearly(got, 2) {
		t.Fatalf("billed $%v, want $2", got)
	}
}

// A record billed before midnight and replayed after it stays on its own day
// and out of the daily period that began at midnight; the weekly period,
// which had begun, counts it.
func TestReplayedRecordKeepsItsDay(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 18, 23, 59, 59, 0, loc))
	old := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	if err := old.Quiesce(); err != nil {
		t.Fatal(err)
	}
	old.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1_000_000})) // journaled
	old.Stop()
	clock.Set(time.Date(2026, 10, 19, 0, 0, 1, 0, loc))
	newer := reopenStore(t, clock, old.StatePath())
	if replayed, err := newer.AdoptHandoff(nil); err != nil || replayed != 1 {
		t.Fatalf("adopt = %d, %v", replayed, err)
	}
	_, days, _ := newer.UsageHistoryFor("k", 2)
	summary := newer.UsageSummaryFor(keyByID(t, newer, "k"))
	if len(days) != 2 || days[0].Date != "2026-10-18" || days[0].TotalUSD != 1 || days[1].TotalUSD != 0 || summary.DailyUSD != 0 || summary.WeeklyUSD != 1 {
		t.Fatalf("days %+v, daily period $%v, weekly period $%v; want $1 on 2026-10-18, $0 and $1", days, summary.DailyUSD, summary.WeeklyUSD)
	}
	if err := newer.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

// CPA rolls a failed replacement back with a reconfigure: the old store
// keeps everything it billed meanwhile, once, and drops its journal.
func TestResumeAfterQuiesceKeepsBillingOnce(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	event := tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1_000_000})
	store.RecordUsage(event)
	if err := store.Quiesce(); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(event)
	store.Resume()
	if _, err := os.Stat(handoffPath(store.StatePath())); !os.IsNotExist(err) {
		t.Fatalf("journal kept after resume: %v", err)
	}
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if got := reopenStore(t, clock, store.StatePath()).UsageSummaryFor(keyByID(t, store, "k")).DailyUSD; !nearly(got, 2) {
		t.Fatalf("billed $%v after rollback, want $2", got)
	}
}

// Quiesce journals every media generation still awaiting its usage record
// with one write: authentication and billing wait meanwhile, and a write
// with an fsync per generation took seconds for a thousand of them.
func TestQuiesceJournalsAwaitedGenerationsAtOnce(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	old := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{perCallPrice("grok-imagine-video", "xai", 1)})
	const generations = 1000
	for i := range generations {
		if denial := old.AdmitIntercepted(bearer("cpa_k"), "/v1/videos", fmt.Sprintf("trace-%d", i)); denial != nil {
			t.Fatalf("admission %d = %+v", i, denial)
		}
	}
	started := time.Now()
	if err := old.Quiesce(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("quiesce took %v for %d generations", took, generations)
	}
	newer := reopenStore(t, clock, old.StatePath())
	if replayed, err := newer.AdoptHandoff(nil); err != nil || replayed != generations {
		t.Fatalf("adopt = %d, %v; want %d generations", replayed, err, generations)
	}
	generation := UsageEvent{TraceID: "trace-500", Provider: "xai", Model: "grok-imagine-video", APIKey: "k"}
	newer.RecordUsage(generation)
	newer.RecordUsage(generation)
	if got := newer.UsageSummaryFor(keyByID(t, newer, "k")).DailyUSD; !nearly(got, 1) {
		t.Fatalf("billed $%v, want $1 for one generation", got)
	}
	_ = old.Shutdown()
	_ = newer.Shutdown()
}

// A store shut down without a quiesce and loaded again (CPA disabled, then
// enabled the plugin) still bills the media generations it admitted: it
// handed them to no one. After a quiesce it takes them from the journal of
// the store that took over, so one that store billed is not billed again.
func TestReloadKeepsOnlyGenerationsItDidNotJournal(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{perCallPrice("grok-imagine-video", "xai", 1)})
	cfg := Config{Enabled: true, StateFile: store.StatePath(), UsageTimezone: "Asia/Shanghai"}
	admit := func(trace string) {
		t.Helper()
		if denial := store.AdmitIntercepted(bearer("cpa_k"), "/v1/videos", trace); denial != nil {
			t.Fatalf("admission = %+v", denial)
		}
	}
	generation := func(trace string) UsageEvent {
		return UsageEvent{TraceID: trace, Provider: "xai", Model: "grok-imagine-video", APIKey: "k"}
	}

	admit("before-disable")
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(cfg, false); err != nil {
		t.Fatal(err)
	}
	if got := store.RecordUsage(generation("before-disable")); got != UsageRecorded {
		t.Fatalf("generation admitted before the disable = %q", got)
	}

	admit("before-quiesce")
	if err := store.Quiesce(); err != nil {
		t.Fatal(err)
	}
	newer := reopenStore(t, clock, store.StatePath())
	if _, err := newer.AdoptHandoff(nil); err != nil {
		t.Fatal(err)
	}
	if got := newer.RecordUsage(generation("before-quiesce")); got != UsageRecorded {
		t.Fatalf("generation on the newer store = %q", got)
	}
	if err := newer.Quiesce(); err != nil { // the switch back
		t.Fatal(err)
	}
	if err := store.Reload(cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptHandoff(nil); err != nil {
		t.Fatal(err)
	}
	if got := store.RecordUsage(generation("before-quiesce")); got != UsageUnmeteredPerCall {
		t.Fatalf("generation the newer store billed = %q, want it not billed again", got)
	}
	if got := store.UsageSummaryFor(keyByID(t, store, "k")).DailyUSD; !nearly(got, 2) {
		t.Fatalf("billed $%v, want $2", got)
	}
	_ = newer.Shutdown()
	_ = store.Shutdown()
}

// Replayed journals stay a week for an investigation and are removed when a
// later instance takes over; a fresh one stays.
func TestAdoptRemovesReplayedJournalsAfterAWeek(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, nil, nil)
	journal := handoffPath(store.StatePath())
	old, fresh, other := journal+".applied-20261001T000000Z", journal+".applied-20261009T000000Z", journal+".bak"
	for _, path := range []string{old, fresh, other} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	eightDays := time.Now().Add(-8 * 24 * time.Hour)
	for _, path := range []string{old, other} {
		if err := os.Chtimes(path, eightDays, eightDays); err != nil {
			t.Fatal(err)
		}
	}
	if replayed, err := store.AdoptHandoff(nil); err != nil || replayed != 0 {
		t.Fatalf("adopt = %d, %v", replayed, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("week-old replayed journal kept: %v", err)
	}
	for _, path := range []string{fresh, other} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s removed: %v", path, err)
		}
	}
	_ = store.Shutdown()
}
