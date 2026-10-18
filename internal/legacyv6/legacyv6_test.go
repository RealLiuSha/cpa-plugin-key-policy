package legacyv6

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// installFixture copies the 0.11.x fixture into a private directory, applying
// edits to the usage file; it returns the state path.
func installFixture(t *testing.T, edit func(usage string) string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"state.json", "cpa-key-policy-usage.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		if name != "state.json" && edit != nil {
			if content = edit(content); content == string(raw) {
				t.Fatal("fixture edit matched nothing")
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "state.json")
}

// The fixture is a 0.11.x dataset: two keys (one with the model list
// disabled and a per-model limit), five models including two public names
// for one upstream id and a per-call row, periods of every reset kind, and
// history both inside and outside the 90-day retention window. The
// conversion runs at 2026-10-12, so days before 2026-07-15 are dropped.
func TestConvertWritesTheFormat7Dataset(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, loc)
	source := installFixture(t, nil)
	sourceState, _ := os.ReadFile(source)
	sourceUsage, _ := os.ReadFile(persist.UsagePath(source))
	target := filepath.Join(t.TempDir(), "state.json")

	report, err := Convert(source, target, "Asia/Shanghai", now)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(source); string(raw) != string(sourceState) {
		t.Fatal("conversion changed the source state")
	}
	if raw, _ := os.ReadFile(persist.UsagePath(source)); string(raw) != string(sourceUsage) {
		t.Fatal("conversion changed the source usage")
	}

	state, err := policy.LoadState(target)
	if err != nil {
		t.Fatal(err)
	}
	if state.DatasetID != "f2c9c9e7f82278cd5a822c8996827db9" || len(state.Keys) != 2 {
		t.Fatalf("state = %+v", state)
	}
	teamA, teamB := state.Keys[0], state.Keys[1]
	if teamA.ID != "team-a" || teamA.Name != "Team A" || !teamA.Enabled || teamA.KeyHash != "sha256:650b7e4673773b8f8d0330016af9183c21244a13593feb507fc7c669c0d9e9d1" ||
		teamA.KeyPreview != "cpa_v6_...alpha" || teamA.RPM != 60 || teamA.DailyLimitUSD != 10 || teamA.WeeklyLimitUSD != 50 || teamA.MonthlyLimitUSD != 100 ||
		!teamA.CreatedAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || teamB.ID != "team-b" || teamB.RPM != 0 || teamB.DailyLimitUSD != 0 {
		t.Fatalf("converted keys = %+v", state.Keys)
	}
	rows := map[string]policy.ModelPrice{}
	for _, row := range state.Models {
		rows[row.Name+"@"+row.Provider] = row
	}
	chat := rows["gpt-5.6@codex"]
	if len(rows) != 4 || chat.BillingMultiplier != 1.2 || chat.CacheWritePricePerMillion == nil || *chat.CacheWritePricePerMillion != 0.3 || chat.CacheReadPricePerMillion != 0.1 ||
		rows["gpt-5.6@openai"].InputPricePerMillion != 1.25 || rows["grok-imagine-image@xai"].PerCallUSD != 0.07 || rows["grok-imagine-image@xai"].BillingMode != "per_call" || rows["community-model@codex"].Priced() {
		t.Fatalf("price rows = %+v", rows)
	}

	dataset, err := policy.LoadUsage(target)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Through != "2026-10-11" || dataset.Timezone != "Asia/Shanghai" {
		t.Fatalf("history through = %q tz = %q", dataset.Through, dataset.Timezone)
	}
	usageA := dataset.Keys["team-a"]
	if _, dropped := usageA.Days["2026-07-01"]; dropped || len(usageA.Days) != 4 {
		t.Fatalf("days outside retention survived: %v", usageA.Days)
	}
	if got := usageA.Days["2026-07-20"]["Chat"]; got != (policy.Bucket{Calls: 2, InputTokens: 2000, ChargedNano: 2_000_000}) {
		t.Fatalf("old retained day = %+v", got)
	}
	// $0.0015300000000000001 → 1_530_000 nanodollars; cache amounts convert alike.
	if got := usageA.Days["2026-10-08"]["Chat"]; got != (policy.Bucket{Calls: 1, InputTokens: 850, OutputTokens: 200, CacheReadTokens: 100, CacheWriteTokens: 50, ChargedNano: 1_530_000, CacheReadCostNano: 12_000, CacheWriteCostNano: 18_000}) {
		t.Fatalf("converted bucket = %+v", got)
	}
	if got := usageA.Days["2026-10-10"]["grok-imagine-image"]; got != (policy.Bucket{Calls: 1, ChargedNano: 70_000_000}) {
		t.Fatalf("per-call bucket = %+v", got)
	}
	cycles := dataset.Keys["team-b"].Cycles
	if cycles.Daily.ResetKind != "automatic" || cycles.Weekly.ResetKind != "manual" || cycles.Monthly.ResetKind != "initial" ||
		!cycles.Weekly.StartedAt.Equal(time.Date(2026, 10, 10, 9, 30, 0, 0, loc)) || !cycles.Weekly.ResetsAt.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, loc)) ||
		!cycles.Monthly.StartedAt.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) || !cycles.Monthly.ResetsAt.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, loc)) ||
		cycles.Monthly.ByModel["gpt-5.6"].ChargedNano != 6_500_000 || cycles.Weekly.ByModel["Fast"].ChargedNano != 6_000 || len(cycles.Monthly.ByModel) != 4 {
		t.Fatalf("cycles = %+v", cycles)
	}

	notes := strings.Join(report.Notes(), "\n")
	for _, want := range []string{
		"ignored 7 per-key model authorization(s) on 2 key(s)", "1 key(s) were limited to a subset of the price rows: team-a",
		"removed 2 per-model daily limit(s)", "1 key(s) had the model list endpoint disabled",
		`"Chat" is now "gpt-5.6" (codex)`, `"Community" is now "community-model" (codex)`, `"Fast" also pointed at codex/gpt-5.6, kept "Chat"`,
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %s lack %q", notes, want)
		}
	}
	if strings.Contains(notes, "day total differed") || strings.Contains(notes, "share one secret") {
		t.Fatalf("notes report problems the fixture does not have: %s", notes)
	}

	// The plugin starts on the converted files as they are.
	store := policy.NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(policy.Config{Enabled: true, StateFile: target, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	// team-a 0.002+0.00153+0.00504+0.00072+0.07, team-b 0.0065+0.07+0.000006; 2026-07-01 is gone.
	if stats := store.Stats(90); stats.Totals.Calls != 10 || stats.Days[0].Date != "2026-07-15" || math.Abs(stats.Totals.ChargedUSD-0.155796) > 1e-9 || stats.Totals.InputTokens != 8460 {
		t.Fatalf("stats after conversion = %+v", stats.Totals)
	}
}

// A source that would not load once converted, or one in another format, is
// refused before any output file is written.
func TestConvertRefusesBeforeWriting(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, shanghai(t))
	for name, tc := range map[string]struct {
		edit     func(string) string
		timezone string
		want     string
	}{
		"negative bucket": {func(usage string) string {
			return strings.Replace(usage, `"2026-10-10":{"total_usd":0.00072`, `"2026-10-10":{"total_usd":-0.00072`, 1)
		}, "Asia/Shanghai", `day 2026-10-10 model "Chat": usage counters must be nonnegative`},
		"foreign timezone":      {nil, "UTC", `uses quota timezone "Asia/Shanghai"`},
		"already format 7":      {func(usage string) string { return strings.Replace(usage, `"version":6`, `"version":7`, 1) }, "Asia/Shanghai", "usage version 7 is not data format 6"},
		"unknown field in pair": {func(usage string) string { return strings.Replace(usage, `"version":6,`, `"version":6,"extra":1,`, 1) }, "Asia/Shanghai", "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			source := installFixture(t, tc.edit)
			target := filepath.Join(t.TempDir(), "state.json")
			if _, err := Convert(source, target, tc.timezone, now); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			entries, _ := os.ReadDir(filepath.Dir(target))
			if len(entries) != 0 {
				t.Fatalf("refused conversion wrote %v", entries)
			}
		})
	}
}

// Everything the conversion drops or chooses is named: keys that gain
// access, recomputed day totals and secrets two keys share.
func TestConvertReportsWhatChanges(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, shanghai(t))
	source := installFixture(t, func(usage string) string {
		// team-b's 2026-10-09 day total ($0.07) no longer equals its models.
		return strings.Replace(usage, `"2026-10-09":{"total_usd":0.07,"call_count":2`, `"2026-10-09":{"total_usd":0.08,"call_count":2`, 1)
	})
	raw, _ := os.ReadFile(source)
	// team-b takes team-a's secret, spelled in upper case, and loses its model list.
	const teamAHash, teamBHash = "sha256:650b7e4673773b8f8d0330016af9183c21244a13593feb507fc7c669c0d9e9d1", "sha256:1b400e9adc6ad4d2ef2240c6d4b8c67a27ad7e8f0a8aee748720d24edc4a3d0b"
	edited := strings.Replace(string(raw), teamBHash, strings.ToUpper(teamAHash), 1)
	if edited == string(raw) {
		t.Fatal("fixture hash not found")
	}
	if err := os.WriteFile(source, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "state.json")
	report, err := Convert(source, target, "Asia/Shanghai", now)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(report.Notes(), "\n")
	for _, want := range []string{
		"day total differed from its models and was recomputed from them: team-b 2026-10-09",
		"keys share one secret; only the first by id authenticates, rotate the other: team-a, team-b",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %s lack %q", notes, want)
		}
	}
	dataset, err := policy.LoadUsage(target)
	if err != nil {
		t.Fatal(err)
	}
	// The by_model buckets are authoritative; the redundant day total is gone.
	if got := dataset.Keys["team-b"].Days["2026-10-09"]; got["Community"].Calls != 1 || got["grok-imagine-image"].ChargedNano != 70_000_000 || len(got) != 2 {
		t.Fatalf("converted day = %+v", got)
	}
}
