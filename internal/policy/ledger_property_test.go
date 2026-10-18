package policy

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// propertyEvent is one billed call as the test remembers it: the oracle
// recomputes every bucket and period total from this list alone.
type propertyEvent struct {
	at      time.Time
	key     string
	model   string
	in, out int64
	charged Nano
}

// Ledger invariants under random traffic, clock jumps, manual resets and
// flushes: every retained day bucket equals the sum of its events, every
// period holds exactly the events since its start, nothing goes negative,
// and a restart reproduces memory. Prices are $1/M input and $2/M output,
// so a charge is (in + 2·out) × 1000 nanodollars exactly.
func TestLedgerInvariantsUnderRandomTraffic(t *testing.T) {
	loc := mustShanghai(t)
	for _, seed := range []int64{1, 7, 2026} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			clock := newTestClock(time.Date(2026, 9, 1, 9, 0, 0, 0, loc))
			keys := []KeyConfig{testKey(t, "p1", "cpa_p1"), testKey(t, "p2", "cpa_p2")}
			store := newStore(t, clock, keys, []ModelPrice{tokenPrice("fast", "", 1, 2)})
			var events []propertyEvent
			resets, flushes := 0, 0
			for step := 0; step < 2000; step++ {
				switch roll := rng.Float64(); {
				case roll < 0.015:
					clock.Advance(time.Duration(1+rng.Intn(2)) * 24 * time.Hour)
				case roll < 0.2:
					clock.Advance(time.Duration(rng.Intn(6)) * time.Hour)
				default:
					clock.Advance(time.Duration(rng.Intn(600)) * time.Second)
				}
				if rng.Float64() < 0.01 {
					if _, err := store.ResetUsageWindow(keys[rng.Intn(2)].ID, quotaWindows[rng.Intn(3)], nil); err != nil {
						t.Fatal(err)
					}
					resets++
				}
				if rng.Float64() < 0.02 {
					if err := store.FlushUsage(); err != nil {
						t.Fatal(err)
					}
					flushes++
				}
				model := "fast"
				if rng.Intn(5) == 0 {
					model = "free"
				}
				event := propertyEvent{at: clock.Now(), key: keys[rng.Intn(2)].ID, model: model, in: int64(rng.Intn(5000)), out: int64(rng.Intn(3000))}
				if event.in == 0 && event.out == 0 {
					event.in = 1
				}
				if model == "fast" {
					event.charged = Nano((event.in + 2*event.out) * 1000)
				}
				if got := store.RecordUsage(tokenEvent(event.key, "codex", model, UsageDetail{InputTokens: event.in, OutputTokens: event.out})); got != UsageRecorded {
					t.Fatalf("step %d: %q", step, got)
				}
				events = append(events, event)
			}
			if days := int(clock.Now().Sub(events[0].at).Hours() / 24); days < 14 || resets == 0 || flushes == 0 {
				t.Fatalf("run covered %d days with %d resets and %d flushes; the invariants need day boundaries, resets and flushes", days, resets, flushes)
			}
			checkLedgerAgainstEvents(t, store, keys, events, clock.Now())
			if err := store.Shutdown(); err != nil {
				t.Fatal(err)
			}
			reloaded := reopenStore(t, clock, store.StatePath())
			checkLedgerAgainstEvents(t, reloaded, keys, events, clock.Now())
			if !reflect.DeepEqual(bucketSums(reloaded.usage.snapshot()), bucketSums(store.usage.snapshot())) {
				t.Fatal("reloaded buckets differ from memory")
			}
			for _, key := range keys {
				before, after := store.UsageSummaryFor(key), reloaded.UsageSummaryFor(key)
				for i := range before.Cycles {
					if !before.Cycles[i].StartedAt.Equal(after.Cycles[i].StartedAt) || !before.Cycles[i].ResetsAt.Equal(after.Cycles[i].ResetsAt) || before.Cycles[i].ResetKind != after.Cycles[i].ResetKind || before.Cycles[i].UsedUSD != after.Cycles[i].UsedUSD {
						t.Fatalf("%s %s cycle differs after restart: %+v vs %+v", key.ID, before.Cycles[i].Window, before.Cycles[i], after.Cycles[i])
					}
				}
			}
		})
	}
}

func checkLedgerAgainstEvents(t *testing.T, store *Store, keys []KeyConfig, events []propertyEvent, now time.Time) {
	t.Helper()
	dateOf := func(at time.Time) string { return at.In(now.Location()).Format(dateLayout) }
	oldest := dateOf(now.AddDate(0, 0, -(UsageRetentionDays - 1)))
	var grand Bucket
	for _, key := range keys {
		// Retained days: one bucket per (date, model) from the events alone.
		want := make(map[string]map[string]Bucket)
		for _, event := range events {
			if event.key != key.ID || dateOf(event.at) < oldest {
				continue
			}
			date := dateOf(event.at)
			if want[date] == nil {
				want[date] = make(map[string]Bucket)
			}
			bucket := want[date][event.model]
			bucket.add(propertyBucket(event))
			want[date][event.model] = bucket
			grand.add(propertyBucket(event))
		}
		_, history, ok := store.UsageHistoryFor(key.ID, UsageRetentionDays)
		if !ok || len(history) != UsageRetentionDays || history[0].Date != oldest {
			t.Fatalf("%s history window = %d days from %s", key.ID, len(history), history[0].Date)
		}
		for _, day := range history {
			var total Bucket
			for model, bucket := range want[day.Date] {
				if got := day.ByModel[model]; got != bucket.totals() {
					t.Fatalf("%s %s %s: got %+v want %+v", key.ID, day.Date, model, got, bucket.totals())
				}
				total.add(bucket)
			}
			if len(day.ByModel) != len(want[day.Date]) || day.UsageTotals != total.totals() {
				t.Fatalf("%s %s: day %+v does not match its models %+v", key.ID, day.Date, day.UsageTotals, want[day.Date])
			}
		}
		// Periods: exactly the events since each period started.
		summary := store.UsageSummaryFor(key)
		_, rows, _ := store.ModelUsageFor(key.ID)
		for _, cycle := range summary.Cycles {
			if !cycle.StartedAt.Before(now) && !cycle.StartedAt.Equal(now) || !cycle.ResetsAt.After(now) {
				t.Fatalf("%s %s period %s..%s does not contain now %s", key.ID, cycle.Window, cycle.StartedAt, cycle.ResetsAt, now)
			}
			byModel := make(map[string]Bucket)
			var used Nano
			for _, event := range events {
				if event.key == key.ID && !event.at.Before(cycle.StartedAt) {
					used += event.charged
					bucket := byModel[event.model]
					bucket.add(propertyBucket(event))
					byModel[event.model] = bucket
				}
			}
			if cycle.UsedUSD != used.USD() {
				t.Fatalf("%s %s used %v, want %v from %d events since %s", key.ID, cycle.Window, cycle.UsedUSD, used.USD(), len(byModel), cycle.StartedAt)
			}
			for _, row := range rows {
				var window UsageWindow
				switch cycle.Window {
				case UsageResetDaily:
					window = row.Daily
				case UsageResetWeekly:
					window = row.Weekly
				default:
					window = row.Monthly
				}
				wantTotals := byModel[row.Name].totals()
				if window.UsageTotals != wantTotals || (wantTotals.CallCount > 0 && !window.WindowStart.Equal(cycle.StartedAt)) {
					t.Fatalf("%s %s %s: got %+v want %+v", key.ID, cycle.Window, row.Name, window, wantTotals)
				}
			}
		}
	}
	stats := store.Stats(UsageRetentionDays)
	if stats.Totals != grand.statsTotals() {
		t.Fatalf("stats totals %+v, want %+v", stats.Totals, grand.statsTotals())
	}
	var byDay, byModel, byKey Bucket
	for _, day := range stats.Days {
		byDay.add(statsBucket(day.StatsTotals))
	}
	for _, model := range stats.Models {
		byModel.add(statsBucket(model.StatsTotals))
	}
	for _, key := range stats.Keys {
		byKey.add(statsBucket(key.StatsTotals))
	}
	if byDay != grand || byModel != grand || byKey != grand {
		t.Fatalf("stats breakdowns disagree: days %+v models %+v keys %+v want %+v", byDay, byModel, byKey, grand)
	}
}

func propertyBucket(event propertyEvent) Bucket {
	bucket := Bucket{Calls: 1, InputTokens: event.in, OutputTokens: event.out, ChargedNano: event.charged}
	if event.model == "free" {
		bucket.UnpricedCalls = 1
	}
	return bucket
}

// statsBucket inverts statsTotals for exact nanodollar sums.
func statsBucket(totals StatsTotals) Bucket {
	return Bucket{
		Calls: totals.Calls, UnpricedCalls: totals.UnpricedCalls, InputTokens: totals.InputTokens,
		CacheReadTokens: totals.CacheReadTokens, CacheWriteTokens: totals.CacheWriteTokens, OutputTokens: totals.OutputTokens,
		ChargedNano: USDToNano(totals.ChargedUSD),
	}
}
