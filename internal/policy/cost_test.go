package policy

import (
	"path/filepath"
	"testing"
	"time"
)

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestComputeCacheCostSubsetProvider: OpenAI-style — cache hits are a SUBSET of
// InputTokens. 1M input total, 200K of which are cached. Input $3/M, output
// $15/M, cache-read $0.30/M. Expected: (1M-200K)*3 + 200K*0.30 + 0.5M*15
// = 800K*3/1M + 200K*0.3/1M + 500K*15/1M = 2.4 + 0.06 + 7.5 = 9.96.
// Verify the cached subset is NOT double-billed at the input price.
func TestComputeCacheCostSubsetProvider(t *testing.T) {
	detail := UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000,
		CachedTokens: 200_000, // subset of input
	}
	got := ComputeCacheCost("openai", 3, 15, 0.30, nil, true, detail)
	if !nearly(got, 9.96) {
		t.Fatalf("subset cache cost = %v, want 9.96", got)
	}
}

// TestComputeCacheCostAdditiveProvider: Anthropic — cache reads are OUTSIDE
// InputTokens. 800K regular input + 200K cache-read, 500K output. Cache-creation
// tokens (writes) billed at input price. Input $3/M, output $15/M, cache-read
// $0.30/M, plus 100K cache-creation. Expected: 800K*3 + 200K*0.30 + 100K*3 +
// 500K*15 = (900K*3 + 200K*0.3 + 500K*15)/1M = 2.7 + 0.06 + 7.5 = 10.26.
func TestComputeCacheCostAdditiveProvider(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	got := ComputeCacheCost("claude", 3, 15, 0.30, nil, true, detail)
	if !nearly(got, 10.26) {
		t.Fatalf("additive cache cost = %v, want 10.26", got)
	}
}

// TestComputeCacheCostNoCachePriceFallsBackToInput: with cacheReadPerMillion=0,
// cache hits fall back to the input price. Subset provider: input already
// includes cache, so total == plain input+output cost (no double count).
// 1M input (incl 200K cached) + 500K output @ $3/$15 → 3 + 7.5 = 10.5.
func TestComputeCacheCostNoCachePriceFallsBackToInput(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	got := ComputeCacheCost("openai", 3, 15, 0, nil, true, detail)
	if !nearly(got, 10.5) {
		t.Fatalf("fallback cost = %v, want 10.5", got)
	}
	// Additive provider: cache reads are outside input, so the fallback total
	// must add them at the input price (matching pre-cache-pricing behavior).
	// 800K input + 200K cacheRead + 500K output @ $3/$15 → 1M*3 + 0.5M*15 = 10.5.
	detail2 := UsageDetail{InputTokens: 800_000, OutputTokens: 500_000, CacheReadTokens: 200_000}
	got2 := ComputeCacheCost("claude", 3, 15, 0, nil, true, detail2)
	if !nearly(got2, 10.5) {
		t.Fatalf("additive fallback cost = %v, want 10.5", got2)
	}
}

// TestComputeCacheCostUnpricedZero: unknown model (priced=false) → 0 even with
// tokens and cache configured.
func TestComputeCacheCostUnpricedZero(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 1_000_000, CachedTokens: 500_000}
	if c := ComputeCacheCost("openai", 3, 15, 0.3, nil, false, detail); c != 0 {
		t.Fatalf("unpriced cost = %v, want 0", c)
	}
}

// TestComputeCacheCostBreakdown verifies the cache-spend / cache-hit breakdown
// returned alongside the total, used by the ledger for hit-rate + cache spend.
// Subset provider, explicit cache price: 1M input (incl 200K cached) + 500K
// output @ $3/$15/$0.30. Total 9.96 (as above). cacheCost = 200K×0.30/1M = 0.06.
// cacheReadTokens = 200K. nonCache input billed at input price = 800K.
func TestComputeCacheCostBreakdown(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	got := ComputeCacheCostBreakdown("openai", 3, 15, 0.30, nil, true, detail)
	if !nearly(got.TotalCost, 9.96) {
		t.Fatalf("total = %v, want 9.96", got.TotalCost)
	}
	if !nearly(got.CacheReadCost, 0.06) {
		t.Fatalf("cacheCost = %v, want 0.06", got.CacheReadCost)
	}
	if got.CacheReadTokens != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000", got.CacheReadTokens)
	}
}

// TestComputeCacheCostBreakdownNoCachePrice: when no cache price is configured,
// cache hits fold into the input-price line. cacheCost must be 0 (not separably
// priced) even though cacheRead is still reported for hit-rate accounting.
func TestComputeCacheCostBreakdownNoCachePrice(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	got := ComputeCacheCostBreakdown("openai", 3, 15, 0, nil, true, detail)
	if !nearly(got.TotalCost, 10.5) {
		t.Fatalf("total = %v, want 10.5", got.TotalCost)
	}
	if got.CacheReadCost != 0 {
		t.Fatalf("cacheCost = %v, want 0 (no separable cache spend without a cache price)", got.CacheReadCost)
	}
	if got.CacheReadTokens != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000 (still reported for hit-rate)", got.CacheReadTokens)
	}
}

// TestComputeCacheCostBreakdownAdditive: Claude — cache reads outside input.
// 800K input + 200K cacheRead + 100K cacheCreation + 500K output @ $3/$15/$0.30.
// Total 10.26 (as above). cacheCost = 200K×0.30/1M = 0.06. cacheRead = 200K.
// cacheCreation tokens are NOT counted as cacheRead (they are writes).
func TestComputeCacheCostBreakdownAdditive(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	got := ComputeCacheCostBreakdown("claude", 3, 15, 0.30, nil, true, detail)
	if !nearly(got.TotalCost, 10.26) {
		t.Fatalf("total = %v, want 10.26", got.TotalCost)
	}
	if !nearly(got.CacheReadCost, 0.06) {
		t.Fatalf("cacheCost = %v, want 0.06", got.CacheReadCost)
	}
	if got.CacheReadTokens != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000 (creation excluded)", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 0 || got.CacheWriteCost != 0 {
		t.Fatalf("unconfigured cache-write leaked into separable stats: %+v", got)
	}
}

func TestComputeCacheCostExplicitCacheWrite(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	got := ComputeCacheCostBreakdown("claude", 3, 15, 0.30, fptr(3.75), true, detail)
	if !nearly(got.TotalCost, 10.335) {
		t.Fatalf("total = %v, want 10.335", got.TotalCost)
	}
	if !nearly(got.CacheWriteCost, 0.375) || got.CacheWriteTokens != 100_000 {
		t.Fatalf("cache-write = %+v", got)
	}
}

func TestComputeCacheCostUnconfiguredCacheWriteFallsBackToInput(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	got := ComputeCacheCost("claude", 3, 15, 0.30, nil, true, detail)
	if !nearly(got, 10.26) {
		t.Fatalf("fallback cost=%v, want 10.26", got)
	}
}

func TestComputeCacheCostExplicitZeroCacheWriteIsFree(t *testing.T) {
	detail := UsageDetail{CacheCreationTokens: 100_000}
	got := ComputeCacheCostBreakdown("claude", 3, 15, 0.30, fptr(0), true, detail)
	if got.TotalCost != 0 || got.CacheWriteCost != 0 || got.CacheWriteTokens != 100_000 || got.InputTokens != 0 {
		t.Fatalf("explicit free cache-write breakdown = %+v", got)
	}
}

// the policy layer: RecordUsage bills from already-parsed token counts (as
// delivered by usage.handle), with no response body to parse. Previously only
// response-body billing required a parseable body — unreachable
// for streams. 1M input × $1/M = $1.00 == daily limit → next auth blocked.
func TestRecordUsageBillsFromParsedTokens(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	model := tokenTestModel("fast", "codex", "gpt-5-codex", 1, 1)
	model.BillingMultiplier = 1.1
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{model},
		Keys: []KeyConfig{{
			ID: "streamy", Enabled: true, DailyLimitUSD: 1.00,
			KeyHash: hashForUsageTest(t, "cpa_stream"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_stream"}}

	// No body involved — usage came pre-parsed from the host's usage.handle.
	cost := store.RecordUsage("cpa_stream", "fast", "gpt-5-codex", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0, TotalTokens: 1_000_000,
	})
	if !nearly(cost, 1.1) {
		t.Fatalf("cost = %v, want 1.1", cost)
	}
	_, history, _ := store.UsageHistoryFor("streamy", 1)
	if len(history) != 1 || history[0].InputTokens != 1_000_000 || !nearly(history[0].TotalUSD, 1.1) {
		t.Fatalf("multiplier changed tokens or missed history: %+v", history)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("streaming usage should be billed & block: %+v", d)
	}
}

// TestRecordUsageUnknownKeyZeroCost: usage for a key not in our config bills
// nothing (the host fires usage.handle for all keys, including non-managed ones).
func TestRecordUsageUnknownKeyZeroCost(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 1)},
		Keys: []KeyConfig{{
			ID: "k", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_known"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cost := store.RecordUsage("cpa_unknown", "fast", "gpt-5-codex", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if cost != 0 {
		t.Fatalf("unknown key should cost 0, got %v", cost)
	}
}

// TestRecordUsageMatchesByID verifies the real host wire value: CPA forwards
// our auth Principal (key.ID) as the UsageRecord.APIKey, not the plaintext
// secret. RecordUsage must resolve the key by ID.
func TestRecordUsageMatchesByID(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 1)},
		Keys: []KeyConfig{{
			ID: "team-x", Enabled: true, DailyLimitUSD: 0.50,
			KeyHash: hashForUsageTest(t, "cpa_secret_xyz"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_secret_xyz"}}

	// Host sends key.ID ("team-x"), NOT the secret. Must still bill.
	cost := store.RecordUsage("team-x", "fast", "gpt-5-codex", false, UsageDetail{
		InputTokens: 500_000, OutputTokens: 0, TotalTokens: 500_000,
	})
	if !nearly(cost, 0.50) {
		t.Fatalf("cost = %v, want 0.50", cost)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("ID-matched usage should bill & block: %+v", d)
	}
}
