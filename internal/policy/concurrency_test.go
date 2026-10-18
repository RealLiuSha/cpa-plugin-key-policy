package policy

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Everything the host can do at once: billing, admission, manual resets,
// flushes, management writes and every report, while the clock crosses
// midnight. The race detector checks the locking; the totals check that no
// call was lost or counted twice, and a restart must reproduce memory.
func TestConcurrentTrafficAdminAndReports(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 23, 59, 50, 0, loc))
	keys := []KeyConfig{testKey(t, "a", "cpa_a"), testKey(t, "b", "cpa_b"), testKey(t, "c", "cpa_c"), testKey(t, "img", "cpa_img")}
	keys[0].RPM, keys[1].DailyLimitUSD = 1000, 5
	store := newStore(t, clock, keys, []ModelPrice{tokenPrice("fast", "", 1, 2), perCallPrice("grok-imagine-image", "xai", 0.07)})
	store.StartUsageFlusher()
	var recorded, billedTraces atomic.Int64
	var wg sync.WaitGroup
	for _, key := range keys[:3] {
		wg.Add(1)
		go func(key KeyConfig) {
			defer wg.Done()
			headers := bearer("cpa_" + key.ID)
			for i := 0; i < 400; i++ {
				admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil)
				if admission.Allowed || admission.Reason == "daily_exceeded" {
					if got := store.RecordUsage(tokenEvent(key.ID, "codex", "fast", UsageDetail{InputTokens: 1000, OutputTokens: 100})); got == UsageRecorded {
						recorded.Add(1)
					}
				}
				if i%5 == 0 {
					store.RecordUsage(tokenEvent(key.ID, "codex", "unknown", UsageDetail{InputTokens: 1}))
					recorded.Add(1)
				}
			}
		}(key)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		headers := bearer("cpa_img")
		for i := 0; i < 300; i++ {
			trace := fmt.Sprintf("gen-%d", i)
			if denial := store.AdmitIntercepted(headers, "/v1/images/generations", trace); denial != nil {
				t.Errorf("image admission %d = %+v", i, denial)
				return
			}
			if got := store.RecordUsage(UsageEvent{TraceID: trace, Provider: "xai", Model: "grok-imagine-image", APIKey: "img"}); got == UsageRecorded {
				recorded.Add(1)
				billedTraces.Add(1)
			}
			store.RecordUsage(UsageEvent{TraceID: "poll-" + trace, Provider: "xai", Model: "grok-imagine-image", APIKey: "img"})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			if _, err := store.ResetUsageWindow(keys[i%3].ID, quotaWindows[i%3], nil); err != nil {
				t.Errorf("reset %d: %v", i, err)
			}
			if err := store.FlushUsage(); err != nil {
				t.Errorf("flush %d: %v", i, err)
			}
			if i == 10 {
				clock.Set(time.Date(2026, 10, 11, 0, 0, 1, 0, loc))
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			price := tokenPrice("fast", "", 1, 2)
			price.BillingMultiplier = 1 + float64(i%3)
			if err := store.UpsertModel(price); err != nil {
				t.Errorf("upsert model: %v", err)
			}
			if _, err := store.ImportModelPrices([]PriceImportMatch{{Model: "fast", PromptPricePer1M: ptr(1.0)}}, i%2 == 0); err != nil {
				t.Errorf("import prices: %v", err)
			}
			if key := store.FindByAPIKey("cpa_c"); key == nil {
				t.Errorf("key c vanished")
			} else {
				key.Name = fmt.Sprintf("c-%d", i)
				if err := store.UpsertKey(*key); err != nil {
					t.Errorf("upsert key: %v", err)
				}
			}
			store.SetClock(clock.Now)
		}
	}()
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				store.Stats(90)
				store.UnpricedModels()
				store.PriceRows()
				store.Status()
				for _, key := range store.Keys() {
					store.UsageSummaryFor(key)
					store.UsageHistoryFor(key.ID, 7)
					store.ModelUsageFor(key.ID)
				}
				if _, err := store.StatsCSV("2026-10-10", "2026-10-11"); err != nil {
					t.Errorf("csv: %v", err)
				}
				store.Authenticate("GET", "/v1/models", bearer("cpa_a"), nil)
			}
		}()
	}
	wg.Wait()
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if billedTraces.Load() != 300 {
		t.Fatalf("billed generations = %d, want every admitted trace exactly once", billedTraces.Load())
	}
	stats := store.Stats(2)
	if stats.Totals.Calls != recorded.Load() || stats.KeysTotal != 4 {
		t.Fatalf("stats calls = %d, recorded %d; keys %d", stats.Totals.Calls, recorded.Load(), stats.KeysTotal)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if !reflect.DeepEqual(bucketSums(reloaded.usage.snapshot()), bucketSums(store.usage.snapshot())) {
		t.Fatal("reloaded buckets differ from memory")
	}
	for _, key := range store.Keys() {
		before, after := store.UsageSummaryFor(key), reloaded.UsageSummaryFor(key)
		if before.DailyUSD != after.DailyUSD || before.WeeklyUSD != after.WeeklyUSD || before.MonthlyUSD != after.MonthlyUSD || !before.Cycles[1].StartedAt.Equal(after.Cycles[1].StartedAt) {
			t.Fatalf("%s summary differs after restart: %+v vs %+v", key.ID, before, after)
		}
	}
}
