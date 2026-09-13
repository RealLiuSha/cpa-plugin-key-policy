package policy

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return loc
}

func TestDailyBoundaryPreservesLongerCycles(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 7, 23, 30, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	key := KeyConfig{ID: "rebirth"}

	ledger.RecordCost(key.ID, "fast", 72.35, 0, 0, 0, 0, 0, 0, 1)
	now = time.Date(2026, 8, 8, 0, 30, 0, 0, loc)
	ledger.RecordCost(key.ID, "fast", 31.10, 0, 0, 0, 0, 0, 0, 1)

	summary := ledger.Summary(key.ID, quotaLimitsForKey(key))
	if summary.DailyUSD > summary.WeeklyUSD || summary.WeeklyUSD > summary.MonthlyUSD {
		t.Fatalf("window invariant violated: daily=%v weekly=%v monthly=%v", summary.DailyUSD, summary.WeeklyUSD, summary.MonthlyUSD)
	}
	if !nearly(summary.DailyUSD, 31.10) || !nearly(summary.WeeklyUSD, 103.45) {
		t.Fatalf("unexpected rebirth totals: %+v", summary)
	}
}

func TestBucketBoundaryIndependentOfFirstRecord(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 0, 30, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	ledger.RecordCost("early", "fast", 1, 0, 0, 0, 0, 0, 0, 1)

	now = time.Date(2026, 8, 8, 23, 30, 0, 0, loc)
	ledger.RecordCost("late", "fast", 1, 0, 0, 0, 0, 0, 0, 1)
	now = time.Date(2026, 8, 9, 0, 0, 0, 0, loc)

	for _, id := range []string{"early", "late"} {
		summary := ledger.Summary(id, quotaLimits{})
		if summary.DailyUSD != 0 {
			t.Fatalf("%s daily usage survived natural-day boundary: %+v", id, summary)
		}
		wantReset := time.Date(2026, 8, 10, 0, 0, 0, 0, loc)
		if !summary.NextAccountingBoundaryAt.Equal(wantReset) {
			t.Fatalf("%s boundary at %s, want %s", id, summary.NextAccountingBoundaryAt, wantReset)
		}
	}
}

func TestWeeklyResetsAtItsFixedBoundary(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	key := KeyConfig{ID: "rolling"}

	for day := 1; day <= 8; day++ {
		now = time.Date(2026, 8, day, 12, 0, 0, 0, loc)
		ledger.RecordCost(key.ID, "fast", 1, 0, 0, 0, 0, 0, 0, 1)
	}

	summary := ledger.Summary(key.ID, quotaLimitsForKey(key))
	if !nearly(summary.DailyUSD, 1) || !nearly(summary.WeeklyUSD, 1) || !nearly(summary.MonthlyUSD, 8) {
		t.Fatalf("fixed windows = %+v, want daily=1 weekly=1 monthly=8", summary)
	}
}

func TestRetentionEvictsBeyond35Days(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	for day := 0; day < 36; day++ {
		now = time.Date(2026, 6, 1, 12, 0, 0, 0, loc).AddDate(0, 0, day)
		ledger.RecordCost("retained", "fast", 1, 0, 0, 0, 0, 0, 0, 1)
	}

	state := ledger.snapshot()["retained"]
	if len(state.Days) != usageRetentionDays {
		t.Fatalf("retained buckets = %d, want %d", len(state.Days), usageRetentionDays)
	}
	if _, ok := state.Days["2026-06-01"]; ok {
		t.Fatal("36-day-old bucket was not evicted")
	}
	if got := len(state.ByModel["fast"]); got != usageRetentionDays {
		t.Fatalf("retained model buckets = %d, want %d", got, usageRetentionDays)
	}
}

func TestModelWindowsMatchKeyWindows(t *testing.T) {
	loc := mustShanghai(t)
	start := time.Date(2026, 7, 1, 12, 0, 0, 0, loc)
	now := start
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	key := KeyConfig{ID: "model-windows", Models: modelRefs("fast")}
	models := []ModelDefinition{freeTestModel("fast", "codex", "fast")}
	for day := 0; day < 31; day++ {
		now = start.AddDate(0, 0, day)
		ledger.RecordCost(key.ID, "fast", 1, 0.25, 2, 0, 0, 3, 4, 1)
	}
	summary := ledger.Summary(key.ID, quotaLimitsForKey(key))
	rows := ledger.ModelUsage(key.ID, models)
	if len(rows) != 1 {
		t.Fatalf("model rows = %+v", rows)
	}
	row := rows[0]
	if !nearly(row.Daily.TotalUSD, summary.DailyUSD) ||
		!nearly(row.Weekly.TotalUSD, summary.WeeklyUSD) ||
		!nearly(row.Monthly.TotalUSD, summary.MonthlyUSD) {
		t.Fatalf("model windows = %+v, key summary = %+v", row, summary)
	}
	if !nearly(row.Daily.TotalUSD, 1) || !nearly(row.Weekly.TotalUSD, 3) || !nearly(row.Monthly.TotalUSD, 1) {
		t.Fatalf("model fixed-cycle totals = %+v", row)
	}
	if row.Monthly.CallCount != 1 || row.Monthly.CacheReadTokens != 2 || !nearly(row.Monthly.CacheCostUSD, 0.25) || row.Monthly.InputTokens != 3 || row.Monthly.OutputTokens != 4 {
		t.Fatalf("model monthly counters = %+v", row.Monthly)
	}
}

func TestQuotaCheckHardLimitReasonsAndUnlimitedDimensions(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	key := KeyConfig{ID: "limited"}
	for day := 0; day < 3; day++ {
		now = time.Date(2026, 8, 6+day, 12, 0, 0, 0, loc)
		ledger.RecordCost(key.ID, "fast", 1, 0, 0, 0, 0, 0, 0, 1)
	}
	for _, test := range []struct {
		name   string
		key    KeyConfig
		reason string
	}{
		{name: "daily", key: KeyConfig{ID: key.ID, DailyLimitUSD: 1}, reason: "daily_exceeded"},
		{name: "weekly", key: KeyConfig{ID: key.ID, WeeklyLimitUSD: 3}, reason: "weekly_exceeded"},
		{name: "monthly", key: KeyConfig{ID: key.ID, MonthlyLimitUSD: 3}, reason: "monthly_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if reason, _ := ledger.OverLimit(test.key.ID, "", quotaLimitsForKey(test.key)); reason != test.reason {
				t.Fatalf("reason = %q, want %q", reason, test.reason)
			}
		})
	}
	if reason, _ := ledger.OverLimit(key.ID, "", quotaLimitsForKey(key)); reason != "" {
		t.Fatalf("zero limits blocked with %q", reason)
	}
}

func TestSoftLimitIsWarningOnly(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	ledger.RecordCost("warning", "fast", 8, 0, 0, 0, 0, 0, 0, 1)
	for _, test := range []struct {
		name string
		key  KeyConfig
	}{
		{name: "daily", key: KeyConfig{ID: "warning", DailyLimitUSD: 10}},
		{name: "weekly", key: KeyConfig{ID: "warning", WeeklyLimitUSD: 10}},
		{name: "monthly", key: KeyConfig{ID: "warning", MonthlyLimitUSD: 10}},
		{name: "model", key: KeyConfig{ID: "warning", Models: []KeyModelRef{{Name: "fast", DailyLimitUSD: 10}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if summary := ledger.Summary(test.key.ID, quotaLimitsForKey(test.key)); !summary.SoftLimitHit {
				t.Fatalf("80%% usage did not set warning: %+v", summary)
			}
			if reason, _ := ledger.OverLimit(test.key.ID, "", quotaLimitsForKey(test.key)); reason != "" {
				t.Fatalf("soft limit blocked request with %q", reason)
			}
		})
	}
	if summary := ledger.Summary("warning", quotaLimits{DailyUSD: 10.01}); summary.SoftLimitHit {
		t.Fatalf("usage below 80%% set warning: %+v", summary)
	}
}

func TestChangedLimitAppliesToAlreadyAccumulatedUsage(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, "Asia/Shanghai")
	key := KeyConfig{ID: "changed", DailyLimitUSD: 10}
	ledger.RecordCost(key.ID, "fast", 9, 0, 0, 0, 0, 0, 0, 1)
	if reason, _ := ledger.OverLimit(key.ID, "", quotaLimitsForKey(key)); reason != "" {
		t.Fatalf("original limit blocked with %q", reason)
	}
	key.DailyLimitUSD = 8
	if reason, summary := ledger.OverLimit(key.ID, "", quotaLimitsForKey(key)); reason != "daily_exceeded" || !nearly(summary.DailyUSD, 9) {
		t.Fatalf("changed limit did not apply immediately: %q %+v", reason, summary)
	}
}

func TestModelDailyLimitIsIsolatedAcrossModelsAndTargets(t *testing.T) {
	dir := t.TempDir()
	hash, err := HashKey("cpa_model_limit")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, mustShanghai(t))
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(dir, "state.json"),
		Models: []ModelDefinition{
			{Name: "fast", Targets: []ModelTarget{{Provider: "codex", TargetModel: "m1"}, {Provider: "openai", TargetModel: "m2"}}, BillingMode: "tokens", InputPricePerMillion: 1},
			tokenTestModel("slow", "codex", "m3", 1, 0),
		},
		Keys: []KeyConfig{{ID: "limited", Enabled: true, KeyHash: hash, Models: []KeyModelRef{{Name: "fast", DailyLimitUSD: 1}, {Name: "slow"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage("limited", "fast", "m1", false, UsageDetail{InputTokens: 1_000_000})
	headers := map[string][]string{"Authorization": {"Bearer cpa_model_limit"}}
	blocked := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if blocked.Allowed || blocked.Reason != "model_daily_exceeded" {
		t.Fatalf("fast model decision = %+v", blocked)
	}
	allowed := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"slow"}`))
	if !allowed.Allowed {
		t.Fatalf("slow model was affected: %+v", allowed)
	}
	key := store.Keys()[0]
	for _, ref := range key.Models {
		if ref.Name == "fast" && ref.DailyLimitUSD != 1 {
			t.Fatalf("model ref lost daily limit: %+v", ref)
		}
	}
	_, models, ok := store.ModelUsageFor(key.ID)
	if !ok || len(models) != 2 {
		t.Fatalf("model usage rows = %+v", models)
	}
	for _, entry := range models {
		if entry.Name == "fast" && (!nearly(entry.Daily.TotalUSD, 1) || !nearly(entry.Weekly.TotalUSD, 1) || !nearly(entry.Monthly.TotalUSD, 1)) {
			t.Fatalf("model window totals diverged: %+v", entry)
		}
	}
}

func TestLimitsChangedAtOnlyMovesWhenLimitChanges(t *testing.T) {
	store := NewStore()
	path := filepath.Join(t.TempDir(), "state.json")
	hash, err := HashKey("cpa_limits")
	if err != nil {
		t.Fatal(err)
	}
	key := KeyConfig{ID: "limits", Enabled: true, KeyHash: hash, DailyLimitUSD: 1, Models: modelRefs("fast")}
	if err := store.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{key}, Models: []ModelDefinition{freeTestModel("fast", "codex", "m")}}); err != nil {
		t.Fatal(err)
	}
	key = store.Keys()[0]
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	first := store.Keys()[0].LimitsChangedAt
	if first.IsZero() {
		t.Fatal("initial nonzero limit did not record change time")
	}
	unchanged := store.Keys()[0]
	unchanged.Name = "renamed"
	if err := store.UpsertKey(unchanged, true); err != nil {
		t.Fatal(err)
	}
	if got := store.Keys()[0].LimitsChangedAt; !got.Equal(first) {
		t.Fatalf("non-limit edit moved limits_changed_at: %s -> %s", first, got)
	}
	changed := store.Keys()[0]
	time.Sleep(time.Millisecond)
	changed.MonthlyLimitUSD = 10
	if err := store.UpsertKey(changed, true); err != nil {
		t.Fatal(err)
	}
	second := store.Keys()[0].LimitsChangedAt
	if !second.After(first) {
		t.Fatalf("limit change timestamp = %s, want after %s", second, first)
	}
	changed = store.Keys()[0]
	if len(changed.Models) != 1 {
		t.Fatalf("models = %+v", changed.Models)
	}
	time.Sleep(time.Millisecond)
	changed.Models[0].DailyLimitUSD = 2
	if err := store.UpsertKey(changed, true); err != nil {
		t.Fatal(err)
	}
	if got := store.Keys()[0].LimitsChangedAt; !got.After(second) {
		t.Fatalf("model limit change timestamp = %s, want after %s", got, second)
	}
}

func TestInvalidUsageTimezoneFallsBackToUTCWithWarning(t *testing.T) {
	if got := DefaultConfig().UsageTimezone; got != "Asia/Shanghai" {
		t.Fatalf("default usage timezone = %q", got)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	cfg, err := DecodeConfig([]byte("usage_timezone: Not/AZone\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsageTimezone != "UTC" || cfg.usageLocation != time.UTC {
		t.Fatalf("fallback config = %q %v", cfg.UsageTimezone, cfg.usageLocation)
	}
	if !strings.Contains(logs.String(), "falling back to UTC") {
		t.Fatalf("missing fallback warning: %q", logs.String())
	}
}

func TestSplitUsageFileAndDirtyFlush(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	hash, err := HashKey("cpa_split")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: statePath, Keys: []KeyConfig{{ID: "split", Enabled: true, KeyHash: hash, Models: modelRefs("fast")}}, Models: []ModelDefinition{perCallTestModel("fast", "codex", "m", 1)}}); err != nil {
		t.Fatal(err)
	}
	stateBeforeFlush, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	store.RecordUsage("split", "fast", "m", false, UsageDetail{})
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateRaw, []byte(`"usage"`)) {
		t.Fatalf("state file still embeds usage: %s", stateRaw)
	}
	if !bytes.Equal(stateBeforeFlush, stateRaw) {
		t.Fatal("usage flush rewrote the key state file")
	}
	usagePath := filepath.Join(dir, "cpa-key-policy-usage.json")
	firstInfo, err := os.Stat(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatalf("clean flush rewrote usage file: %s -> %s", firstInfo.ModTime(), secondInfo.ModTime())
	}
	usageRawBefore, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	key := store.Keys()[0]
	key.Name = "config-only"
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	usageRawAfter, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(usageRawBefore, usageRawAfter) {
		t.Fatal("key update rewrote usage file")
	}
}

func TestTmpCleanupRemovesOnlyOldMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, "cleanup-dataset", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, ".state.json.tmp-old")
	recentPath := filepath.Join(dir, ".state.json.tmp-recent")
	otherPath := filepath.Join(dir, ".other.tmp-old")
	for _, path := range []string{oldPath, recentPath, otherPath} {
		if err := os.WriteFile(path, []byte("tmp"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(oldPath, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old temp still exists: %v", err)
	}
	for _, path := range []string{recentPath, otherPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("non-stale/non-matching temp removed: %s: %v", path, err)
		}
	}
}

func TestConfigureCleansStaleStateAndUsageTemps(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, "cleanup-dataset", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := SaveUsage(filepath.Join(dir, "cpa-key-policy-usage.json"), "cleanup-dataset", map[string]*UsageState{}); err != nil {
		t.Fatal(err)
	}
	oldStateTemp := filepath.Join(dir, ".state.json.tmp-stale")
	oldUsageTemp := filepath.Join(dir, ".cpa-key-policy-usage.json.tmp-stale")
	recentUsageTemp := filepath.Join(dir, ".cpa-key-policy-usage.json.tmp-recent")
	for _, path := range []string{oldStateTemp, oldUsageTemp, recentUsageTemp} {
		if err := os.WriteFile(path, []byte("tmp"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{oldStateTemp, oldUsageTemp} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldStateTemp, oldUsageTemp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale startup temp still exists %s: %v", path, err)
		}
	}
	if _, err := os.Stat(recentUsageTemp); err != nil {
		t.Fatalf("recent usage temp was removed: %v", err)
	}
}

func TestQuotaCheckSkipsLedgerWorkWhenNoLimitsApply(t *testing.T) {
	loc := mustShanghai(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, loc, loc.String())
	if reason, _ := ledger.OverLimit("unlimited", "fast", quotaLimits{}); reason != "" {
		t.Fatalf("unlimited key rejected with %q", reason)
	}
	if ledger.lastEvictedDate != "" {
		t.Fatalf("unlimited quota check scanned the ledger on %q", ledger.lastEvictedDate)
	}
}
