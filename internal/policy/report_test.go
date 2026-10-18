package policy

import (
	"sync"
	"testing"
	"time"
)

// The priced flag of a usage report follows the ledger: it is false while
// the shown window holds a call billed without a price, whatever the price
// table says now. The price table's own flag is the price row's.
func TestReportPricedFlagFollowsTheBilledCalls(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("gpt-5.6", "codex", 1, 2)})
	if _, err := store.ImportModels([]ModelImportItem{{Name: "gpt-6-terra", Provider: "openai"}}); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(tokenEvent("k", "openai", "gpt-6-terra", UsageDetail{InputTokens: 10}))
	// The qualified row prices codex calls only; the openai call is free.
	store.RecordUsage(tokenEvent("k", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000}))

	_, rows, ok := store.ModelUsageFor("k")
	if !ok || len(rows) != 2 || rows[0].Priced || rows[0].Daily.TotalUSD != 0 || rows[0].Daily.UnpricedCalls != 1 || rows[1].Priced || rows[1].BillingMode != "tokens" || rows[1].Daily.UnpricedCalls != 1 {
		t.Fatalf("model usage with unpriced calls = %+v, want priced=false on both", rows)
	}
	stats := store.Stats(1)
	if len(stats.Models) != 2 || stats.Models[0].Priced || stats.Models[1].Priced || stats.Totals.UnpricedCalls != 2 {
		t.Fatalf("stats with unpriced calls = %+v", stats.Models)
	}
	if len(stats.UnpricedModels) != 2 || stats.UnpricedModels[0] != (UnpricedModel{Name: "gpt-5.6", Calls: 1, LastSeen: "2026-10-10", HasQualifiedRows: true}) || !stats.UnpricedModels[1].HasQualifiedRows {
		t.Fatalf("unpriced list = %+v, want both models flagged as having qualified rows", stats.UnpricedModels)
	}
	price := tokenPrice("gpt-6-terra", "openai", 1, 2)
	price.BillingMultiplier = 2
	if err := store.UpsertModel(price); err != nil {
		t.Fatal(err)
	}
	// The row is priced now, but the window still holds the free call.
	_, rows, _ = store.ModelUsageFor("k")
	if rows[1].Priced || rows[1].BillingMultiplier != 2 || store.Stats(1).Models[1].Priced {
		t.Fatalf("after pricing the row = %+v", rows)
	}
	if row, _ := store.PriceRowFor("gpt-6-terra", "openai"); !row.Priced {
		t.Fatalf("price row = %+v, want its own flag true", row)
	}
	store.RecordUsage(tokenEvent("k", "codex", "gpt-5.6", UsageDetail{InputTokens: 1_000_000}))
	if stats := store.Stats(1); stats.Models[0].Name != "gpt-5.6" || stats.Models[0].Priced || stats.Models[0].Calls != 2 || stats.Models[0].UnpricedCalls != 1 {
		t.Fatalf("mixed window = %+v, want priced=false while one call was free", stats.Models[0])
	}
}

// Report readers take the ledger clock under its lock; SetClock may run at
// any time (CPA reconfigures while serving), so the race detector checks it.
func TestReportsReadClockUnderLock(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 10}))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			store.SetClock(clock.Now)
		}()
		go func() {
			defer wg.Done()
			store.Stats(7)
			store.UnpricedModels()
		}()
	}
	wg.Wait()
}
