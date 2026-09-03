package policy

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newClockedStore(t *testing.T, now time.Time) (*Store, time.Time) {
	t.Helper()
	tm := now
	store := NewStore()
	store.SetClock(func() time.Time { return tm })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 2)},
		Keys: []KeyConfig{
			{
				ID: "team-a", Enabled: true,
				KeyHash:        hashForUsageTest(t, "cpa_usage"),
				KeyPreview:     "cpa_us..._age",
				Models:         modelRefs("fast"),
				DailyLimitUSD:  1.00,
				WeeklyLimitUSD: 5.00,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, tm
}

func TestUsageLedgerLoadAndSnapshotDeepCopyModelMap(t *testing.T) {
	ledger := newUsageLedger(time.Now)
	date := ledger.dateKey(time.Now())
	sourceState := &UsageState{
		Days: map[string]UsageBucket{date: {TotalUSD: 1}},
		ByModel: map[string]map[string]UsageBucket{
			"fast": {date: {TotalUSD: 1}},
		},
	}
	ledger.loadFromState(map[string]*UsageState{"team-a": sourceState})

	// Mutating the state object supplied by the persistence layer must not
	// change the live ledger.
	sourceState.Days[date] = UsageBucket{TotalUSD: 9}
	sourceState.ByModel["fast"] = map[string]UsageBucket{date: {TotalUSD: 9}}
	first := ledger.snapshot()["team-a"]
	if !nearly(first.Days[date].TotalUSD, 1) || !nearly(first.ByModel["fast"][date].TotalUSD, 1) {
		t.Fatalf("loaded ledger shares source state: %+v", first)
	}

	// A caller mutating a persistence/reporting snapshot must likewise leave
	// the live ledger untouched.
	first.Days[date] = UsageBucket{TotalUSD: 7}
	first.ByModel["fast"] = map[string]UsageBucket{date: {TotalUSD: 7}}
	second := ledger.snapshot()["team-a"]
	if !nearly(second.Days[date].TotalUSD, 1) || !nearly(second.ByModel["fast"][date].TotalUSD, 1) {
		t.Fatalf("ledger shares returned snapshot: %+v", second)
	}
}

func hashForUsageTest(t *testing.T, key string) string {
	t.Helper()
	h, err := HashKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUsageRecordAndOverLimitDaily(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store, tm := newClockedStore(t, now)
	headers := map[string][]string{"Authorization": {"Bearer cpa_usage"}}

	// 500K prompt × $1/M = $0.50 → under the $1 daily limit.
	_ = store.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 500_000})
	d := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("first request should be allowed: %+v", d)
	}
	// Another $0.50 → total $1.00, equals the daily limit. Billing is
	// post-hoc: this request itself was allowed (the prior Authenticate
	// passed), but the NEXT request now sees daily_usd >= limit and is
	// rejected (Authenticate is a pre-request gate on accumulated usage).
	_ = store.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 500_000})
	d = store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("at-limit request should be rejected on the next Authenticate: %+v", d)
	}
	// Crossing UTC midnight resets the daily window.
	tm = tm.Add(14 * time.Hour) // next day
	store.SetClock(func() time.Time { return tm })
	d = store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("after midnight should be allowed again: %+v", d)
	}
}

func TestUsageUnlimitedKeyNeverBlocked(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 10, 10)},
		Keys: []KeyConfig{{
			ID: "free", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_free"),
			Models:  modelRefs("fast"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_free"}}
	for i := 0; i < 50; i++ {
		_ = store.RecordUsage("free", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 1_000_000, OutputTokens: 1_000_000})
		d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
		if !d.Allowed {
			t.Fatalf("unlimited key blocked at iter %d: %+v", i, d)
		}
	}
}

func TestUsageUnpricedModelRejected(t *testing.T) {
	store := NewStore()
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models: []ModelDefinition{{
			Name: "fast", Targets: []ModelTarget{{Provider: "codex", TargetModel: "gpt-5-codex"}}, BillingMode: "tokens",
		}},
		Keys: []KeyConfig{{
			ID: "cheap", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_cheap"),
			Models:  modelRefs("fast"),
		}},
	})
	if err == nil {
		t.Fatal("unpriced model was accepted")
	}
}

func TestUsageHandleBillsStreamingAndSkipsMissingReports(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 1)},
		Keys: []KeyConfig{{
			ID: "streamy", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_stream"),
			Models:  modelRefs("fast"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_stream"}}

	// usage.handle delivers the parsed final frame for streaming requests.
	_ = store.RecordUsage("streamy", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 1_000_000})
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	// 1M tokens × $1/M = $1.00 >= $0.01 limit → rejected.
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("streaming with usage frame should be billed & blocked: %+v", d)
	}

	// A streaming body the host passes WITHOUT any usage frame is not billed.
	store2 := NewStore()
	store2.SetClock(func() time.Time { return now })
	if err := store2.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state2.json"),
		Models: []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 1)},
		Keys: []KeyConfig{{
			ID: "streamy2", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_stream2"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr2 := map[string][]string{"Authorization": {"Bearer cpa_stream2"}}
	d = store2.Authenticate("POST", "/v1/chat/completions", hdr2, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("streaming without usage frame should not be billed: %+v", d)
	}
}

func TestUsageSummaryReflectsUsage(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store, _ := newClockedStore(t, now)
	_ = store.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 200_000, OutputTokens: 100_000})
	keys := store.Keys()
	var key KeyConfig
	for _, k := range keys {
		if k.ID == "team-a" {
			key = k
		}
	}
	s := store.UsageSummaryFor(key)
	// 200K×$1/M + 100K×$2/M = $0.20 + $0.20 = $0.40
	if !nearly(s.DailyUSD, 0.40) || !nearly(s.WeeklyUSD, 0.40) {
		t.Fatalf("summary = %+v, want 0.40/0.40", s)
	}
	if s.DailyLimitUSD != 1.0 || s.WeeklyLimitUSD != 5.0 {
		t.Fatalf("limits = %+v", s)
	}
	if s.NextAccountingBoundaryAt.IsZero() {
		t.Fatal("next_accounting_boundary_at should be set")
	}
}

func TestUsagePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	mk := func(clock func() time.Time) *Store {
		s := NewStore()
		s.SetClock(clock)
		if err := s.Configure(Config{
			Enabled: true, StateFile: path,
			Models: []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5-codex", 1, 0)},
			Keys: []KeyConfig{{
				ID: "team-a", Enabled: true, DailyLimitUSD: 1.0,
				KeyHash: hashForUsageTest(t, "cpa_usage"),
				Models:  modelRefs("fast"),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	s1 := mk(func() time.Time { return now })
	hdr := map[string][]string{"Authorization": {"Bearer cpa_usage"}}
	_ = s1.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 800_000})
	if err := s1.FlushUsage(); err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh store loads from the same state file.
	s2 := mk(func() time.Time { return now })
	keys := s2.Keys()
	var key KeyConfig
	for _, k := range keys {
		if k.ID == "team-a" {
			key = k
		}
	}
	s := s2.UsageSummaryFor(key)
	if !nearly(s.DailyUSD, 0.80) {
		t.Fatalf("usage after restart = %+v, want 0.80", s)
	}
	// Over-limit is enforced post-restart (0.80 < 1.0, allowed; then bill to >1).
	d := s2.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("should be allowed at 0.80/1.0: %+v", d)
	}
}

func TestResetUsageWindowDailyAndWeekly(t *testing.T) {
	now := time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5", 1, 1)},
		Keys: []KeyConfig{{
			ID: "resettable", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_resettable"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 300_000,
	})
	dailyResult, err := store.ResetUsageWindow("resettable", UsageResetDaily)
	if err != nil {
		t.Fatal(err)
	}
	if !nearly(dailyResult.BeforeDailyUSD, 0.30) || !nearly(dailyResult.BeforeWeeklyUSD, 0.30) {
		t.Fatalf("daily reset before = %+v, want 0.30/0.30", dailyResult)
	}
	summary := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0) || !nearly(summary.WeeklyUSD, 0) {
		t.Fatalf("after daily reset = %+v, want daily 0 / weekly 0", summary)
	}
	_, models, ok := store.ModelUsageFor("resettable")
	if !ok || len(models) != 1 || !nearly(models[0].Daily.TotalUSD, 0) || !nearly(models[0].Weekly.TotalUSD, 0) {
		t.Fatalf("model after daily reset = %+v, want daily 0 / weekly 0", models)
	}

	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 200_000,
	})
	weeklyResult, err := store.ResetUsageWindow("resettable", UsageResetWeekly)
	if err != nil {
		t.Fatal(err)
	}
	if !nearly(weeklyResult.BeforeDailyUSD, 0.20) || !nearly(weeklyResult.BeforeWeeklyUSD, 0.20) {
		t.Fatalf("weekly reset before = %+v, want 0.20/0.20", weeklyResult)
	}
	summary = store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0) || !nearly(summary.WeeklyUSD, 0) {
		t.Fatalf("after weekly reset = %+v, want daily 0 / weekly 0", summary)
	}
	_, models, _ = store.ModelUsageFor("resettable")
	if len(models) != 1 || !nearly(models[0].Daily.TotalUSD, 0) || !nearly(models[0].Weekly.TotalUSD, 0) {
		t.Fatalf("model after weekly reset = %+v, want daily 0 / weekly 0", models)
	}

	reloaded := NewStore()
	reloaded.SetClock(func() time.Time { return now })
	if err := reloaded.Configure(Config{Enabled: true, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}
	reloadedSummary := reloaded.UsageSummaryFor(reloaded.Keys()[0])
	if !nearly(reloadedSummary.DailyUSD, 0) || !nearly(reloadedSummary.WeeklyUSD, 0) {
		t.Fatalf("persisted reset after reload = %+v, want 0/0", reloadedSummary)
	}
}

func TestResetUsageWindowMonthlyClearsThirtyDays(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled: true, StateFile: statePath,
		Models: []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5", 1, 0)},
		Keys: []KeyConfig{{
			ID: "monthly-reset", Enabled: true, KeyHash: hashForUsageTest(t, "cpa_monthly_reset"),
			Models: modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	now = now.AddDate(0, 0, -20)
	_ = store.RecordUsage("monthly-reset", "fast", "gpt-5", false, UsageDetail{InputTokens: 2_000_000})
	now = now.AddDate(0, 0, 20)
	_ = store.RecordUsage("monthly-reset", "fast", "gpt-5", false, UsageDetail{InputTokens: 1_000_000})

	result, err := store.ResetUsageWindow("monthly-reset", UsageResetMonthly)
	if err != nil {
		t.Fatal(err)
	}
	if !nearly(result.BeforeDailyUSD, 1) || !nearly(result.BeforeWeeklyUSD, 1) || !nearly(result.BeforeMonthlyUSD, 3) ||
		!nearly(result.AfterDailyUSD, 0) || !nearly(result.AfterWeeklyUSD, 0) || !nearly(result.AfterMonthlyUSD, 0) {
		t.Fatalf("monthly reset result = %+v", result)
	}
	summary := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0) || !nearly(summary.WeeklyUSD, 0) || !nearly(summary.MonthlyUSD, 0) {
		t.Fatalf("monthly reset summary = %+v", summary)
	}
	_, models, ok := store.ModelUsageFor("monthly-reset")
	if !ok || len(models) != 1 || !nearly(models[0].Monthly.TotalUSD, 0) {
		t.Fatalf("monthly reset models = %+v", models)
	}
}

func TestResetUsageWindowRejectsInvalidInput(t *testing.T) {
	store, _ := newClockedStore(t, time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC))
	if _, err := store.ResetUsageWindow("missing", UsageResetDaily); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error = %v, want ErrUnknownKey", err)
	}
	if _, err := store.ResetUsageWindow("team-a", UsageResetWindow("year")); err == nil {
		t.Fatal("invalid reset window should fail")
	}
}

func TestResetUsageWindowRollsBackWhenPersistenceFails(t *testing.T) {
	now := time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "gpt-5", 1, 1)},
		Keys: []KeyConfig{{
			ID: "resettable", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_resettable"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 300_000,
	})

	corruptState := []byte(`{"version":`)
	if err := os.WriteFile(statePath, corruptState, 0o600); err != nil {
		t.Fatal(err)
	}
	usagePath := filepath.Join(filepath.Dir(statePath), "cpa-key-policy-usage.json")
	if err := os.Remove(usagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(usagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetUsageWindow("resettable", UsageResetWeekly); err == nil {
		t.Fatal("reset should fail when the usage file cannot be replaced")
	}

	summary := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0.30) || !nearly(summary.WeeklyUSD, 0.30) {
		t.Fatalf("usage after failed reset = %+v, want in-memory rollback to 0.30/0.30", summary)
	}
	_, models, ok := store.ModelUsageFor("resettable")
	if !ok || len(models) != 1 || !nearly(models[0].Daily.TotalUSD, 0.30) || !nearly(models[0].Weekly.TotalUSD, 0.30) {
		t.Fatalf("model usage after failed reset = %+v, want rollback to 0.30/0.30", models)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != string(corruptState) {
		t.Fatalf("failed reset changed state file: %q", persisted)
	}
}

// newCacheStore builds a store with one key whose model has an explicit
// cache-read price, for cache-stat accounting tests.
func newCacheStore(t *testing.T, now time.Time, provider string) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models: []ModelDefinition{{
			Name: "fast", Targets: []ModelTarget{{Provider: provider, TargetModel: "m"}}, BillingMode: "tokens",
			InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.30,
		}},
		Keys: []KeyConfig{{
			ID: "cache-key", Enabled: true,
			KeyHash:    hashForUsageTest(t, "cpa_cache"),
			KeyPreview: "cpa_ca...che",
			Models:     modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestCacheStatsAccumulatedAndHitRate: a subset-provider record (1M input incl
// 200K cached + 500K output @ $3/$15/$0.30) → total $9.96, cacheCost $0.06,
// cacheRead 200K, nonCacheInput 800K. Hit-rate = 200K/(200K+800K) = 20%.
func TestCacheStatsAccumulatedAndHitRate(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "openai")
	cost := store.RecordUsage("cache-key", "FAST", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	if !nearly(cost, 9.96) {
		t.Fatalf("cost = %v, want 9.96", cost)
	}
	key := store.Keys()[0]
	s := store.UsageSummaryFor(key)
	if !nearly(s.DailyCacheCostUSD, 0.06) {
		t.Fatalf("daily cache cost = %v, want 0.06", s.DailyCacheCostUSD)
	}
	if s.DailyCacheReadTokens != 200_000 {
		t.Fatalf("daily cache read tokens = %d, want 200000", s.DailyCacheReadTokens)
	}
	if s.DailyInputTokens != 800_000 {
		t.Fatalf("daily non-cache input tokens = %d, want 800000", s.DailyInputTokens)
	}
	// Hit-rate = cacheRead / (cacheRead + input) = 200K / 1M = 0.2.
	if got := float64(s.DailyCacheReadTokens) / float64(s.DailyCacheReadTokens+s.DailyInputTokens); !nearly(got, 0.2) {
		t.Fatalf("hit-rate = %v, want 0.2", got)
	}
	// Weekly window mirrors daily for a same-day single record.
	if !nearly(s.WeeklyCacheCostUSD, 0.06) || s.WeeklyCacheReadTokens != 200_000 {
		t.Fatalf("weekly cache stats = %+v, want 0.06/200000", s)
	}
	_, rows, ok := store.ModelUsageFor("cache-key")
	if !ok || len(rows) != 1 || rows[0].Name != "fast" {
		t.Fatalf("cache-aware mixed-case model rows = %+v, want one canonical fast row", rows)
	}
}

// TestCacheStatsResetAtMidnight: cache counters reset when the daily window
// rolls across UTC midnight. SetClock rebuilds the in-memory ledger (matching
// how the existing daily-limit test models a clock jump), so we re-record after
// advancing the clock and assert the daily cache stats reflect ONLY the new
// day's record. (Cross-day weekly accumulation is covered by the persist test,
// which survives the ledger rebuild via the state file.)
func TestCacheStatsResetAtMidnight(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "openai")
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	store.SetClock(func() time.Time { return now.Add(14 * time.Hour) }) // next UTC day
	// A new day-2 record: 1M input (200K cached), no output.
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0, CachedTokens: 200_000,
	})

	s := store.UsageSummaryFor(store.Keys()[0])
	// Daily = day-2 only: 200K cacheRead, 0.06 cacheCost, 800K nonCache input.
	if s.DailyCacheReadTokens != 200_000 || !nearly(s.DailyCacheCostUSD, 0.06) || s.DailyInputTokens != 800_000 || s.DailyCacheWriteTokens != 0 {
		t.Fatalf("daily cache stats after midnight = %+v, want 200000/0.06/800000", s)
	}
	// Day-1's 500K output must NOT leak into the new daily window.
	if !nearly(s.DailyUSD, 2.46) { // 800K*3 + 200K*0.30 = 2.4 + 0.06
		t.Fatalf("daily total after midnight = %v, want 2.46 (day-2 only)", s.DailyUSD)
	}
}

// TestCacheStatsAdditiveExcludesCreation: for an additive provider (Claude),
// cache-creation (write) tokens must NOT be counted in cacheRead — only reads.
// 800K input + 200K cacheRead + 100K cacheCreation + 500K output @ $3/$15/$0.30.
func TestCacheStatsAdditiveExcludesCreation(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "claude")
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	})
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCacheReadTokens != 200_000 {
		t.Fatalf("daily cacheRead = %d, want 200000 (creation excluded)", s.DailyCacheReadTokens)
	}
	// nonCache input = input + creation = 900K (additive bills creation at input price).
	if s.DailyInputTokens != 900_000 {
		t.Fatalf("daily non-cache input = %d, want 900000", s.DailyInputTokens)
	}
	if s.DailyCacheWriteTokens != 0 {
		t.Fatalf("unconfigured cache-write invented tokens: %d", s.DailyCacheWriteTokens)
	}
}

func TestCacheWriteStatsAccumulatedIndependently(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models: []ModelDefinition{{
			Name: "fast", Targets: []ModelTarget{{Provider: "claude", TargetModel: "m"}}, BillingMode: "tokens",
			InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.30, CacheWritePricePerMillion: fptr(3.75),
		}},
		Keys: []KeyConfig{{
			ID: "cache-key", Enabled: true,
			KeyHash:    hashForUsageTest(t, "cpa_cache"),
			KeyPreview: "cpa_ca...che",
			Models:     modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cost := store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 800_000, OutputTokens: 500_000, CacheReadTokens: 200_000, CacheCreationTokens: 100_000,
	})
	if !nearly(cost, 10.335) {
		t.Fatalf("cost = %v, want 10.335", cost)
	}
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCacheWriteTokens != 100_000 || !nearly(s.DailyCacheWriteUSD, 0.375) {
		t.Fatalf("cache-write stats = %+v", s)
	}
	if s.DailyInputTokens != 800_000 {
		t.Fatalf("input tokens = %d, want 800000 after peeling writes", s.DailyInputTokens)
	}
}

func TestCacheOnlyUsageIsBilledAndRecorded(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models: []ModelDefinition{{
			Name: "fast", Targets: []ModelTarget{{Provider: "claude", TargetModel: "m"}}, BillingMode: "tokens",
			InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.30, CacheWritePricePerMillion: fptr(3.75),
		}},
		Keys: []KeyConfig{{ID: "cache-key", Enabled: true, KeyHash: hashForUsageTest(t, "cpa_cache"), Models: modelRefs("fast")}},
	}); err != nil {
		t.Fatal(err)
	}

	writeCost := store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{CacheCreationTokens: 100_000})
	readCost := store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{CacheReadTokens: 100_000})
	if !nearly(writeCost, 0.375) || !nearly(readCost, 0.03) {
		t.Fatalf("cache-only costs = write %v read %v", writeCost, readCost)
	}
	summary := store.UsageSummaryFor(store.Keys()[0])
	if summary.DailyCacheWriteTokens != 100_000 || summary.DailyCacheReadTokens != 100_000 || summary.DailyInputTokens != 0 {
		t.Fatalf("cache-only usage summary = %+v", summary)
	}
}

// TestCacheStatsPersistAcrossRestart: cache counters survive a state-file
// reload, so the UI keeps showing cache spend/hit-rate after a plugin restart.
func TestCacheStatsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	mk := func() *Store {
		s := NewStore()
		s.SetClock(func() time.Time { return now })
		if err := s.Configure(Config{
			Enabled: true, StateFile: path,
			Models: []ModelDefinition{{
				Name: "fast", Targets: []ModelTarget{{Provider: "openai", TargetModel: "m"}}, BillingMode: "tokens",
				InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.30,
			}},
			Keys: []KeyConfig{{
				ID: "cache-key", Enabled: true,
				KeyHash: hashForUsageTest(t, "cpa_cache"),
				Models:  modelRefs("fast"),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := mk()
	_ = s1.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	if err := s1.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	s2 := mk()
	got := s2.UsageSummaryFor(s2.Keys()[0])
	if !nearly(got.DailyCacheCostUSD, 0.06) || got.DailyCacheReadTokens != 200_000 || got.DailyInputTokens != 800_000 {
		t.Fatalf("cache stats after restart = %+v, want 0.06/200000/800000", got)
	}
}

// newPerCallStore builds a store with one key whose model is billed per-call at
// perCallUSD, under a daily dollar limit, for per-call billing tests.
func newPerCallStore(t *testing.T, perCallUSD, dailyLimit float64) *Store {
	t.Helper()
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	model := perCallTestModel("fast", "codex", "gpt-5-codex", perCallUSD)
	if perCallUSD == 0 {
		model.Free = true
	}
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{model},
		Keys: []KeyConfig{{
			ID: "percall", Enabled: true, DailyLimitUSD: dailyLimit,
			KeyHash: hashForUsageTest(t, "cpa_percall"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestPerCallBillsFixedUSD: each successful request charges PerCallUSD
// regardless of token counts; token prices are ignored. Two $0.50 calls hit
// the $1.00 daily limit, so the next Authenticate is rejected.
func TestPerCallBillsFixedUSD(t *testing.T) {
	store := newPerCallStore(t, 0.50, 1.00)
	hdr := map[string][]string{"Authorization": {"Bearer cpa_percall"}}

	// A "successful" usage record with huge token counts — per_call must charge
	// the fixed $0.50, NOT the (dormant) token price.
	cost := store.RecordUsage("percall", "FAST", "gpt-5-codex", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if !nearly(cost, 0.50) {
		t.Fatalf("per_call cost = %v, want 0.50", cost)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("first call should be allowed: %+v", d)
	}
	// Second $0.50 → total $1.00 == limit. Next Authenticate rejected.
	_ = store.RecordUsage("percall", "FaSt", "gpt-5-codex", false, UsageDetail{})
	d = store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("after two per_call charges, next should be daily_exceeded: %+v", d)
	}
	// CallCount reflects two successful calls.
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 2 {
		t.Fatalf("daily call count = %d, want 2", s.DailyCallCount)
	}
	_, rows, ok := store.ModelUsageFor("percall")
	if !ok || len(rows) != 1 || rows[0].Name != "fast" || rows[0].Daily.CallCount != 2 {
		t.Fatalf("per-call mixed-case model rows = %+v, want one canonical fast row with 2 calls", rows)
	}
}

// TestTokenModeFreeModelStillCounts: a token-mode model whose configured
// input/output/cache prices are all 0 (priced=true but free) must still
// record token + call counters. The usage handler previously gated ledger
// recording on `cost > 0`, dropping free-but-priced
// requests entirely so their usage volume / hit-rate was invisible.
func TestTokenModeFreeModelStillCounts(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	hash, err := HashKey("cpa_free2")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{freeTestModel("fast", "anthropic", "m")}, Keys: []KeyConfig{{
		ID: "free", Enabled: true, KeyHash: hash,
		Models: modelRefs("fast"),
	}}}); err != nil {
		t.Fatal(err)
	}
	// usage.handle path (RecordUsage): non-zero tokens, all prices 0.
	cost := store.RecordUsage("free", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 200_000,
	})
	if cost != 0 {
		t.Fatalf("cost = %v, want 0 (free model)", cost)
	}
	sum := store.UsageSummaryFor(store.Keys()[0])
	if sum.DailyCallCount != 1 {
		t.Fatalf("DailyCallCount = %d, want 1 (free call must still count)", sum.DailyCallCount)
	}
	if sum.DailyInputTokens != 1_000_000 {
		t.Fatalf("DailyInputTokens = %d, want 1000000 (Anthropic input excludes cache reads)", sum.DailyInputTokens)
	}
	if sum.DailyCacheReadTokens != 200_000 {
		t.Fatalf("DailyCacheReadTokens = %d, want 200000 (cache tokens must count even when free)", sum.DailyCacheReadTokens)
	}
}

// TestAllowModelsEndpointPerKey: a key with AllowModelsEndpoint=true may reach
// GET /v1/models (allowed); a key with it false (default) is 401. We cannot
// filter the list contents per key (CPA limitation), only hide/show it.
func TestAllowModelsEndpointPerKey(t *testing.T) {
	// Distinct secrets per key so Authenticate resolves the intended config
	// (sharing one hash makes map iteration order non-deterministic).
	store := NewStore()
	hashA, _ := HashKey("cpa_a")
	hashB, _ := HashKey("cpa_b")
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state2.json"), Models: []ModelDefinition{freeTestModel("fast", "codex", "gpt-5-codex")}, Keys: []KeyConfig{
		{ID: "hidden", Enabled: true, KeyHash: hashA, Models: modelRefs("fast")},
		{ID: "open", Enabled: true, KeyHash: hashB, Models: modelRefs("fast"), AllowModelsEndpoint: true},
	}}); err != nil {
		t.Fatal(err)
	}
	dHidden := store.Authenticate("GET", "/v1/models", http.Header{"Authorization": {"Bearer cpa_a"}}, nil, nil)
	if dHidden.Allowed || dHidden.Reason != "models_endpoint_disabled" {
		t.Fatalf("hidden key (secret a) = %+v, want models_endpoint_disabled", dHidden)
	}
	dOpen := store.Authenticate("GET", "/v1/models", http.Header{"Authorization": {"Bearer cpa_b"}}, nil, nil)
	if !dOpen.Allowed || dOpen.Reason != "models_endpoint_allowed" {
		t.Fatalf("open key (secret b) = %+v, want Allowed + models_endpoint_allowed", dOpen)
	}
}

// TestPerCallFailedNotBilled: a failed request charges nothing and does not
// increment CallCount (per-call only applies to HTTP-200 outcomes).
func TestPerCallFailedNotBilled(t *testing.T) {
	store := newPerCallStore(t, 0.50, 1.00)
	cost := store.RecordUsage("percall", "fast", "gpt-5-codex", true, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if cost != 0 {
		t.Fatalf("failed per_call cost = %v, want 0", cost)
	}
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 0 || !nearly(s.DailyUSD, 0) {
		t.Fatalf("failed call should not count: %+v", s)
	}
}

// TestPerCallZeroStillCounts: PerCallUSD=0 is allowed (free calls). Cost is 0
// but CallCount still increments, and the key never exceeds a dollar limit.
func TestPerCallZeroStillCounts(t *testing.T) {
	store := newPerCallStore(t, 0, 0.01)
	for i := 0; i < 5; i++ {
		cost := store.RecordUsage("percall", "fast", "gpt-5-codex", false, UsageDetail{})
		if cost != 0 {
			t.Fatalf("free per_call cost = %v, want 0", cost)
		}
	}
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 5 {
		t.Fatalf("daily call count = %d, want 5", s.DailyCallCount)
	}
	// No dollar spend → never blocked even with a tiny limit.
	hdr := map[string][]string{"Authorization": {"Bearer cpa_percall"}}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("free per_call should never exceed dollar limit: %+v", d)
	}
}

// TestModelUsageBreakdown verifies model windows, unused configured rows, and
// historical residuals after a model is removed from a key.
func TestModelUsageBreakdown(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled: true, StateFile: statePath,
		Models: []ModelDefinition{
			tokenTestModel("fast", "codex", "gpt-5-codex", 1, 2),
			tokenTestModel("slow", "codex", "o4-mini", 1, 1),
		},
		Keys: []KeyConfig{{
			ID: "team-a", Enabled: true, KeyHash: hashForUsageTest(t, "cpa_usage"), Models: modelRefs("fast", "slow"),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Bill fast: 200K input + 100K output @ $1/$2 = $0.20 + $0.20 = $0.40.
	_ = store.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{
		InputTokens: 200_000, OutputTokens: 100_000,
	})
	// Bill slow so it has history, then remove it from the key's config below.
	_ = store.RecordUsage("team-a", "slow", "o4-mini", false, UsageDetail{
		InputTokens: 50_000, OutputTokens: 0,
	})
	teamA := store.Keys()[0]
	teamA.Models = modelRefs("fast")
	if err := store.UpsertKey(teamA, true); err != nil {
		t.Fatal(err)
	}

	_, rows, ok := store.ModelUsageFor("team-a")
	if !ok {
		t.Fatal("key not found")
	}
	byModel := map[string]ModelUsageEntry{}
	for _, row := range rows {
		byModel[row.Name] = row
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2 (fast+slow residual)", len(rows))
	}
	// fast: configured, billed $0.40 daily & weekly, 1 call, 200K input, 100K output.
	f := byModel["fast"]
	if !f.InConfig || !nearly(f.Daily.TotalUSD, 0.40) || !nearly(f.Weekly.TotalUSD, 0.40) {
		t.Fatalf("fast row = %+v, want in_config=true $0.40/$0.40", f)
	}
	if f.Daily.CallCount != 1 || f.Daily.InputTokens != 200_000 || f.Daily.OutputTokens != 100_000 {
		t.Fatalf("fast daily counters = %+v, want 1/200000/100000", f.Daily)
	}
	// slow: removed from config but has historical usage → InConfig=false, residual data.
	s := byModel["slow"]
	if s.InConfig {
		t.Fatalf("slow should be in_config=false after removal: %+v", s)
	}
	if !nearly(s.Daily.TotalUSD, 0.05) || s.Daily.InputTokens != 50_000 || s.Daily.CallCount != 1 {
		t.Fatalf("slow residual daily = %+v, want $0.05 / 50000 / 1 call", s.Daily)
	}
	if rows[0].Name != "fast" || rows[1].Name != "slow" {
		t.Fatalf("rows not sorted by model: %+v", rows)
	}
}

func TestModelUsageUnknownKey(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "s.json")}); err != nil {
		t.Fatal(err)
	}
	_, _, ok := store.ModelUsageFor("nope")
	if ok {
		t.Fatal("unknown key should return ok=false")
	}
}

// TestRecordUsageCanonicalModelCaseVariants ensures case variants land in one
// bucket using the configured spelling.
func TestRecordUsageCanonicalModelCaseVariants(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Models:    []ModelDefinition{tokenTestModel("gpt-5.6-sol", "codex", "gpt-5.6", 1, 2)},
		Keys: []KeyConfig{{
			ID: "team-sol", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_sol"),
			Models:  modelRefs("gpt-5.6-sol"),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Three case variants, same token shape each: 100K in / 50K out → $0.10+$0.10=$0.20
	variants := []string{"gpt-5.6-sol", "Gpt-5.6-sol", "GPT-5.6-SOL"}
	for _, v := range variants {
		cost := store.RecordUsage("team-sol", v, "gpt-5.6", false, UsageDetail{
			InputTokens: 100_000, OutputTokens: 50_000,
		})
		if !nearly(cost, 0.20) {
			t.Fatalf("RecordUsage(%q) cost = %v, want 0.20", v, cost)
		}
	}

	_, rows, ok := store.ModelUsageFor("team-sol")
	if !ok {
		t.Fatal("key not found")
	}
	if len(rows) != 1 {
		t.Fatalf("row count = %d, want 1 canonical bucket; rows=%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Name != "gpt-5.6-sol" || !row.InConfig {
		t.Fatalf("row = %+v, want name=gpt-5.6-sol in_config=true", row)
	}
	if !nearly(row.Daily.TotalUSD, 0.60) || row.Daily.CallCount != 3 {
		t.Fatalf("daily = %+v, want $0.60 / 3 calls", row.Daily)
	}
	if row.Daily.InputTokens != 300_000 || row.Daily.OutputTokens != 150_000 {
		t.Fatalf("daily tokens = %+v, want 300000/150000", row.Daily)
	}
	// Key-level totals must match (no double-count at key vs model).
	s := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(s.DailyUSD, 0.60) || s.DailyCallCount != 3 {
		t.Fatalf("key daily = %+v, want $0.60 / 3", s)
	}

	// Unknown model: zero cost, no forged empty ByModel row.
	unknownCost := store.RecordUsage("team-sol", "totally-unknown-model", "x", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0,
	})
	if unknownCost != 0 {
		t.Fatalf("unknown model cost = %v, want 0", unknownCost)
	}
	_, rows2, _ := store.ModelUsageFor("team-sol")
	if len(rows2) != 1 {
		t.Fatalf("unknown model created extra rows: %+v", rows2)
	}
	if rows2[0].Name != "gpt-5.6-sol" {
		t.Fatalf("unexpected residual after unknown: %+v", rows2)
	}
}

// TestModelUsageCaseCanonicalReadOnlyMerge: historical mixed-case ByModel
// buckets merge on read into the config spelling; state file bytes and ledger
// entries are not mutated by the read.
func TestModelUsageCaseCanonicalReadOnlyMerge(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")

	// Seed mixed-case historical buckets and one removed residual model.
	datasetID := "mixed-case-dataset"
	model := tokenTestModel("gpt-5.6-sol", "codex", "gpt-5.6", 1, 2)
	key := KeyConfig{ID: "team-sol", Enabled: true, KeyHash: hashForUsageTest(t, "cpa_sol"), Models: modelRefs("gpt-5.6-sol")}
	if err := SaveState(statePath, datasetID, []KeyConfig{key}, []ModelDefinition{model}, nil); err != nil {
		t.Fatal(err)
	}
	date := "2026-06-29"
	usageState := &UsageState{
		Days: map[string]UsageBucket{date: {TotalUSD: 0.65, CallCount: 4, CacheReadTokens: 60_000, CacheCostUSD: 0.06, InputTokens: 350_000, OutputTokens: 150_000}},
		ByModel: map[string]map[string]UsageBucket{
			"gpt-5.6-sol": {date: {TotalUSD: 0.20, CallCount: 1, CacheReadTokens: 10_000, CacheCostUSD: 0.01, InputTokens: 100_000, OutputTokens: 50_000}},
			"Gpt-5.6-sol": {date: {TotalUSD: 0.30, CallCount: 1, CacheReadTokens: 20_000, CacheCostUSD: 0.02, InputTokens: 150_000, OutputTokens: 75_000}},
			"GPT-5.6-SOL": {date: {TotalUSD: 0.10, CallCount: 1, CacheReadTokens: 30_000, CacheCostUSD: 0.03, InputTokens: 50_000, OutputTokens: 25_000}},
			"old-model":   {date: {TotalUSD: 0.05, CallCount: 1, InputTokens: 50_000}},
		},
	}
	usagePath := filepath.Join(filepath.Dir(statePath), "cpa-key-policy-usage.json")
	if err := SaveUsage(usagePath, datasetID, map[string]*UsageState{"team-sol": usageState}); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{Enabled: true, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the complete ledger before read (must be unchanged after).
	_, usageBefore := store.runtimeComponents()
	var beforeLedgerBytes []byte
	if usageBefore != nil {
		beforeLedgerBytes, err = json.Marshal(usageBefore.snapshot())
		if err != nil {
			t.Fatal(err)
		}
	}

	_, rows, ok := store.ModelUsageFor("team-sol")
	if !ok {
		t.Fatal("key not found")
	}
	byModel := map[string]ModelUsageEntry{}
	for _, row := range rows {
		byModel[row.Name] = row
	}
	// Configured canonical + residual old-model only (3 case variants merged).
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2; rows=%+v", len(rows), rows)
	}
	canon := byModel["gpt-5.6-sol"]
	if !canon.InConfig {
		t.Fatalf("canonical must be in_config=true: %+v", canon)
	}
	if !nearly(canon.Daily.TotalUSD, 0.60) || canon.Daily.CallCount != 3 {
		t.Fatalf("canonical daily = %+v, want $0.60 / 3", canon.Daily)
	}
	if canon.Daily.InputTokens != 300_000 || canon.Daily.OutputTokens != 150_000 {
		t.Fatalf("canonical tokens = %+v, want 300000/150000", canon.Daily)
	}
	if canon.Daily.CacheReadTokens != 60_000 || !nearly(canon.Daily.CacheCostUSD, 0.06) {
		t.Fatalf("canonical cache counters = %+v, want 60000/$0.06", canon.Daily)
	}
	if !nearly(canon.Weekly.TotalUSD, 0.60) || canon.Weekly.CallCount != 3 {
		t.Fatalf("canonical weekly = %+v, want $0.60 / 3", canon.Weekly)
	}
	if canon.Weekly.CacheReadTokens != 60_000 || !nearly(canon.Weekly.CacheCostUSD, 0.06) {
		t.Fatalf("canonical weekly cache counters = %+v, want 60000/$0.06", canon.Weekly)
	}
	residual := byModel["old-model"]
	if residual.InConfig || !nearly(residual.Daily.TotalUSD, 0.05) {
		t.Fatalf("residual = %+v, want in_config=false $0.05", residual)
	}

	// Key totals include every model bucket, including the residual.
	s := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(s.DailyUSD, 0.65) || s.DailyCallCount != 4 {
		t.Fatalf("key summary daily = %+v, want $0.65 / 4", s)
	}

	// The complete ledger still holds all historical spellings and values.
	_, usageAfter := store.runtimeComponents()
	var afterLedgerBytes []byte
	if usageAfter != nil {
		afterLedgerBytes, err = json.Marshal(usageAfter.snapshot())
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(beforeLedgerBytes) != string(afterLedgerBytes) {
		t.Fatal("ledger snapshot changed by ModelUsage read")
	}
	// Usage file bytes are untouched by the read (no flush/save side effect).
	afterBytes, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatal("usage file bytes changed by ModelUsage read")
	}
}

func TestAddUsageWindowUsesEarliestNonZeroStart(t *testing.T) {
	earlier := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(24 * time.Hour)

	merged := addUsageWindow(
		UsageWindow{TotalUSD: 0.20, WindowStart: later},
		UsageWindow{TotalUSD: 0.30, WindowStart: earlier},
	)
	if !merged.WindowStart.Equal(earlier) {
		t.Fatalf("merged window start = %s, want earliest %s", merged.WindowStart, earlier)
	}
	if !nearly(merged.TotalUSD, 0.50) {
		t.Fatalf("merged total = %v, want 0.50", merged.TotalUSD)
	}

	reversed := addUsageWindow(
		UsageWindow{TotalUSD: 0.30, WindowStart: earlier},
		UsageWindow{TotalUSD: 0.20, WindowStart: later},
	)
	if !reversed.WindowStart.Equal(earlier) || !nearly(reversed.TotalUSD, 0.50) {
		t.Fatalf("reverse-order merge = %+v, want earliest start and total 0.50", reversed)
	}
}

// increment CallCount (the counter is mode-agnostic for successful requests).
func TestCallCountIncrementedTokenMode(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{tokenTestModel("fast", "codex", "m", 1, 1)},
		Keys: []KeyConfig{{
			ID: "tok", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_tok"),
			Models:  modelRefs("fast"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.RecordUsage("tok", "fast", "m", false, UsageDetail{InputTokens: 100_000, OutputTokens: 0})
	_ = store.RecordUsage("tok", "fast", "m", false, UsageDetail{InputTokens: 0, OutputTokens: 100_000})
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 2 {
		t.Fatalf("token-mode daily call count = %d, want 2", s.DailyCallCount)
	}
}
