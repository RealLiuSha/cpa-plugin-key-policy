package policy

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

func TestStatsListsAtMostTwoHundredKeysByCharge(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	keys := make([]KeyConfig, 0, statsKeysLimit+1)
	for i := 0; i <= statsKeysLimit; i++ {
		keys = append(keys, testKey(t, fmt.Sprintf("k%03d", i), fmt.Sprintf("cpa_k%03d", i)))
	}
	store := newStore(t, clock, keys, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	for i, key := range keys {
		store.RecordUsage(tokenEvent(key.ID, "codex", "fast", UsageDetail{InputTokens: int64(i+1) * 1000}))
	}
	stats := store.Stats(1)
	if !stats.KeysTruncated || stats.KeysTotal != statsKeysLimit+1 || len(stats.Keys) != statsKeysLimit || stats.Keys[0].ID != "k200" || stats.Keys[statsKeysLimit-1].ID != "k001" {
		t.Fatalf("truncation: total=%d truncated=%v len=%d first=%s last=%s", stats.KeysTotal, stats.KeysTruncated, len(stats.Keys), stats.Keys[0].ID, stats.Keys[len(stats.Keys)-1].ID)
	}
	// The totals still cover every key, truncated or not.
	if stats.Totals.Calls != statsKeysLimit+1 || !nearly(stats.Totals.ChargedUSD, 0.001*float64((statsKeysLimit+1)*(statsKeysLimit+2)/2)) {
		t.Fatalf("totals = %+v", stats.Totals)
	}
	if err := store.DeleteKey("k000"); err != nil {
		t.Fatal(err)
	}
	if stats = store.Stats(1); stats.KeysTotal != statsKeysLimit+1 || stats.Keys[statsKeysLimit-1].ID != "k001" {
		t.Fatalf("a deleted key keeps its place in the ranking: %+v", stats.Keys[statsKeysLimit-1])
	}
}

// A dataset with 90 retained days × 2223 models for one key: 200070 buckets,
// just over the export row cap, written in the spec §3.5 file shape.
func writeWideDataset(t *testing.T, dir string, loc *time.Location) string {
	t.Helper()
	statePath := filepath.Join(dir, "state.json")
	key := testKey(t, "k", "cpa_k")
	key.CreatedAt, key.UpdatedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeJSON(t, statePath, map[string]any{"version": 7, "dataset_id": "wide", "keys": []KeyConfig{key}, "models": []ModelPrice{}, "updated_at": "2026-10-15T00:00:00Z"})
	const models = 2223
	day := func() map[string]any {
		buckets := make(map[string]any, models)
		for m := 0; m < models; m++ {
			buckets[fmt.Sprintf("m%04d", m)] = bucketJSON(1, 10, 1_234)
		}
		return buckets
	}
	first := time.Date(2026, 7, 18, 0, 0, 0, 0, loc)
	historyDays := make(map[string]any, 89)
	for offset := 0; offset < 89; offset++ {
		historyDays[first.AddDate(0, 0, offset).Format(dateLayout)] = day()
	}
	writeJSON(t, persist.HistoryPath(statePath), map[string]any{"version": 7, "dataset_id": "wide", "updated_at": "2026-10-15T00:00:00Z", "through": "2026-10-14", "days": map[string]any{"k": historyDays}})
	today := time.Date(2026, 10, 15, 0, 0, 0, 0, loc)
	writeJSON(t, persist.UsagePath(statePath), map[string]any{
		"version": 7, "dataset_id": "wide", "updated_at": "2026-10-15T00:00:00Z", "timezone": "Asia/Shanghai", "history_through": "2026-10-14",
		"keys": map[string]any{"k": map[string]any{
			"cycles": cyclesJSON(loc, today, time.Date(2026, 10, 9, 0, 0, 0, 0, loc), time.Date(2026, 9, 30, 0, 0, 0, 0, loc), "automatic", map[string]any{}),
			"days":   map[string]any{"2026-10-15": day()},
		}},
	})
	return statePath
}

func TestStatsCSVRefusesOversizedExports(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 15, 9, 0, 0, 0, loc))
	store := reopenStore(t, clock, writeWideDataset(t, t.TempDir(), loc))
	if stats := store.Stats(90); stats.Totals.Calls != 200070 || stats.Totals.UnpricedCalls != 0 || len(stats.Models) != 2223 {
		t.Fatalf("stats = calls %d models %d", stats.Totals.Calls, len(stats.Models))
	}
	if _, err := store.StatsCSV("2026-07-18", "2026-10-15"); !errors.Is(err, ErrRangeTooLarge) || !strings.Contains(err.Error(), "200070 rows") {
		t.Fatalf("90 days × 2223 models = %v, want ErrRangeTooLarge by row count", err)
	}
	// 89 days × 2223 = 197847 rows fit.
	raw, err := store.StatsCSV("2026-07-19", "2026-10-15")
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(raw, []byte("\n")); lines != 197848 {
		t.Fatalf("csv lines = %d", lines)
	}
	if !bytes.HasPrefix(raw, []byte(statsCSVColumnsRow+"\n2026-07-19,k,k,false,m0000,1,0,10,0,0,0,0.000001234\n")) {
		t.Fatalf("csv head = %q", raw[:160])
	}
	for name, tc := range map[string]struct{ from, to string }{
		"ninety-three days": {"2026-07-15", "2026-10-15"},
		"reversed":          {"2026-10-15", "2026-10-14"},
		"malformed from":    {"2026-7-15", "2026-10-15"},
		"malformed to":      {"2026-07-15", "today"},
	} {
		_, err := store.StatsCSV(tc.from, tc.to)
		if name == "ninety-three days" && !errors.Is(err, ErrRangeTooLarge) {
			t.Fatalf("%s: %v", name, err)
		}
		if name != "ninety-three days" && !errors.Is(err, ErrInvalidDateRange) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Exactly 92 days is the inclusive limit; an empty range is a header only.
	if raw, err := store.StatsCSV("2026-01-01", "2026-04-02"); err != nil || string(raw) != statsCSVColumnsRow+"\n" {
		t.Fatalf("92-day empty range = %q err=%v", raw, err)
	}
	if got := StatsCSVFilename("2026-07-19", "2026-10-15"); got != "cpa-key-policy-usage-2026-07-19_2026-10-15.csv" {
		t.Fatalf("filename = %q", got)
	}
}

func TestStatsCSVRowsAndAmountFormatting(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 9, 9, 0, 0, 0, loc))
	keys := []KeyConfig{testKey(t, "=evil", "cpa_evil"), testKey(t, "k2", "cpa_k2"), testKey(t, "-dash", "cpa_dash")}
	keys[0].Name, keys[2].Name = "+Alice", "@Bob"
	expensive := tokenPrice("expensive", "", 10000, 10000)
	store := newStore(t, clock, keys, []ModelPrice{tokenPrice("gpt-5.6", "", 1, 2), expensive})
	store.RecordUsage(tokenEvent("=evil", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000, CacheReadTokens: 200_000}))
	store.RecordUsage(tokenEvent("-dash", "openai", "expensive", UsageDetail{InputTokens: 1 << 31}))
	clock.Set(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store.RecordUsage(tokenEvent("k2", "openai", "gpt-5.6", UsageDetail{OutputTokens: 1_500_000}))
	store.RecordUsage(tokenEvent("k2", "openai", "=SUM(A1)", UsageDetail{InputTokens: 3}))
	store.RecordUsage(tokenEvent("k2", "openai", "gpt-5.6", UsageDetail{InputTokens: 1}))
	if err := store.DeleteKey("-dash"); err != nil {
		t.Fatal(err)
	}
	raw, err := store.StatsCSV("2026-10-09", "2026-10-10")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		statsCSVColumnsRow,
		// 2^31 tokens × $10000/M = $21474836.48: exact, no exponent
		"2026-10-09,'-dash,,true,expensive,1,0,2147483648,0,0,0,21474836.48",
		"2026-10-09,'=evil,'+Alice,false,gpt-5.6,1,0,800000,200000,0,0,1",
		// unpriced models are keyed in lower case
		"2026-10-10,k2,k2,false,'=sum(a1),1,1,3,0,0,0,0",
		// 1 token × $1/M = 1000 nanodollars on top of $3
		"2026-10-10,k2,k2,false,gpt-5.6,2,0,1,0,0,1500000,3.000001",
	}, "\n") + "\n"
	if string(raw) != want {
		t.Fatalf("csv =\n%s\nwant\n%s", raw, want)
	}
	for nano, text := range map[Nano]string{0: "0", 1: "0.000000001", 1234: "0.000001234", 1_500_000_000: "1.5", 21_474_836_480_000: "21474.83648", 9_223_372_036_854_775_807: "9223372036.854775807"} {
		if got := nano.FormatUSD(); got != text {
			t.Errorf("FormatUSD(%d) = %q, want %q", nano, got, text)
		}
	}
}
