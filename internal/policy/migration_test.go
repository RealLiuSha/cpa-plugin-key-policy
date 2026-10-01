package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

func TestMigrationResumesAfterUsageWasPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stateRaw := mustReadSchemaFixture(t, "testdata/schema-v4/state.json")
	usageRaw := mustReadSchemaFixture(t, "testdata/schema-v4/usage.json")
	if err := os.WriteFile(path, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persist.UsagePath(path), usageRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := LoadUsage(persist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, mustShanghai(t))
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, mustShanghai(t), "Asia/Shanghai")
	ledger.loadFromState(usage.Usage)
	ledger.initializeCycles(state.Keys, true)
	opening := ledger.snapshot()
	if err := backupBeforeMigration(path, state.DatasetID); err != nil {
		t.Fatal(err)
	}
	// Reproduce a process stopping after the first durable write, before state
	// is replaced. Restart must complete this exact partial migration.
	if err := SaveUsage(persist.UsagePath(path), state.DatasetID, opening); err != nil {
		t.Fatal(err)
	}
	persisted, err := LoadUsage(persist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Version != currentUsageFileVersion {
		t.Fatal("cycle ledger was not persisted before state")
	}
	now = now.Add(time.Hour)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	recovered, err := LoadUsage(persist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Usage, recovered.Usage) {
		t.Fatal("retry changed cycle anchors or reapplied opening balances")
	}
	backupRaw, err := os.ReadFile(MigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var backup migrationBackup
	if err := json.Unmarshal(backupRaw, &backup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup.State, stateRaw) || !bytes.Equal(backup.Usage, usageRaw) {
		t.Fatal("original recovery pair was changed")
	}
}

func TestMigrationFailurePreservesOriginalPair(t *testing.T) {
	for _, kind := range []string{"unwritable", "corrupt_state", "wrong_usage_dataset"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			stateRaw := mustReadSchemaFixture(t, "testdata/schema-v3/state.json")
			usageRaw := mustReadSchemaFixture(t, "testdata/schema-v3/usage.json")
			if err := os.WriteFile(path, stateRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(persist.UsagePath(path), usageRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "unwritable" {
				if err := os.Mkdir(MigrationBackupPath(path), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				state, err := decodeState(stateRaw)
				if err != nil {
					t.Fatal(err)
				}
				backup := migrationBackup{DatasetID: state.DatasetID, State: stateRaw, Usage: usageRaw}
				if kind == "corrupt_state" {
					backup.State = []byte("broken")
				} else {
					backup.Usage = bytes.ReplaceAll(usageRaw, []byte(state.DatasetID), []byte("another-dataset"))
				}
				raw, err := json.Marshal(backup)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(MigrationBackupPath(path), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := NewStore().Configure(Config{Enabled: true, StateFile: path}); err == nil {
				t.Fatal("migration continued without a recoverable backup")
			}
			for name, want := range map[string][]byte{path: stateRaw, persist.UsagePath(path): usageRaw} {
				got, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("failed migration changed source pair")
				}
			}
		})
	}
}

func TestMigrationAfterRollbackBacksUpCurrentData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stateRaw := mustReadSchemaFixture(t, "testdata/schema-v4/state.json")
	usageRaw := mustReadSchemaFixture(t, "testdata/schema-v4/usage.json")
	if err := os.WriteFile(path, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persist.UsagePath(path), usageRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := decodeState(stateRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := backupBeforeMigration(path, state.DatasetID); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(MigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	// A legacy instance used after rollback can have a later snapshot of the same dataset.
	var changed persistedUsage
	if err := json.Unmarshal(usageRaw, &changed); err != nil {
		t.Fatal(err)
	}
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Hour)
	current, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persist.UsagePath(path), current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backupBeforeMigration(path, state.DatasetID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(MigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var backup migrationBackup
	if err := json.Unmarshal(raw, &backup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup.Usage, current) {
		t.Fatal("re-upgrade retained stale recovery data")
	}
	archives, err := filepath.Glob(MigrationBackupPath(path) + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("backup archives: %v", archives)
	}
	archived, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, archived) {
		t.Fatal("previous recovery point was overwritten")
	}
}

func TestCycleScheduleSurvivesManualResetAndDowntime(t *testing.T) {
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, mustShanghai(t))
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, mustShanghai(t), "Asia/Shanghai")
	ledger.RecordCost("key", "model", 5, 0, 0, 0, 0, 100, 0, 1)
	now = now.AddDate(0, 0, 3)
	result, err := ledger.resetWindowAndPersist("key", UsageResetWeekly, nil, func(map[string]*UsageState) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.AfterWeeklyUSD != 0 || result.AfterMonthlyUSD != 5 || result.NextAccountingBoundaryAt.Day() != 22 {
		t.Fatalf("manual reset: %+v", result)
	}
	if len(ledger.History("key", 4)) != 4 || ledger.History("key", 4)[0].TotalUSD != 5 {
		t.Fatal("manual reset removed history")
	}
	now = time.Date(2026, 10, 8, 12, 0, 0, 0, mustShanghai(t))
	summary := ledger.Summary("key", quotaLimits{WeeklyUSD: 10, MonthlyUSD: 20})
	if summary.Cycles[1].ResetsAt.Day() != 13 || summary.Cycles[2].ResetsAt.Day() != 12 || summary.MonthlyUSD != 5 {
		t.Fatalf("downtime shifted independent schedules: %+v", summary)
	}
}

// Format 6 drops multi-target routing, credential groups and rules, and the
// free flag. Converting a v5 dataset must keep every key, price, multiplier
// and quota cycle, keep the original pair as a backup, and record the drops.
func TestV5DatasetConvertsToCurrentFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stateRaw := mustReadSchemaFixture(t, "testdata/schema-v5/state.json")
	usageRaw := mustReadSchemaFixture(t, "testdata/schema-v5/usage.json")
	if err := os.WriteFile(path, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persist.UsagePath(path), usageRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := LoadUsage(persist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, mustShanghai(t))
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{Enabled: true, StateFile: path, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := LoadUsage(persist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentStateFileVersion || usage.Version != currentUsageFileVersion || len(state.RemovedSettings) != 0 {
		t.Fatalf("converted versions = %d/%d, removed=%v", state.Version, usage.Version, state.RemovedSettings)
	}
	models := map[string]ModelDefinition{}
	for _, model := range state.Models {
		models[model.Name] = model
	}
	chat, community := models["Chat"], models["Community"]
	if chat.Provider != "codex" || chat.TargetModel != "gpt-5.6" || chat.BillingMultiplier != 1.2 || chat.InputPricePerMillion != 1 || chat.CacheWritePricePerMillion == nil || *chat.CacheWritePricePerMillion != 0.3 {
		t.Fatalf("Chat = %+v", chat)
	}
	if community.Provider != "codex" || community.InputPricePerMillion != 0 || community.OutputPricePerMillion != 0 {
		t.Fatalf("Community = %+v", community)
	}
	key := state.Keys[0]
	if key.KeyHash != "sha256:"+strings.Repeat("a", 64) || key.DailyLimitUSD != 10 || len(key.Models) != 2 || key.Models[0].DailyLimitUSD != 5 {
		t.Fatalf("key = %+v", key)
	}
	if !reflect.DeepEqual(before.Usage, usage.Usage) {
		t.Fatal("conversion changed usage history or quota cycles")
	}
	backupRaw, err := os.ReadFile(MigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var backup migrationBackup
	if err := json.Unmarshal(backupRaw, &backup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup.State, stateRaw) || !bytes.Equal(backup.Usage, usageRaw) {
		t.Fatal("backup does not hold the original v5 pair")
	}
	events, err := store.AuditEvents("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "migrate_state" {
		t.Fatalf("audit = %+v, want one migrate_state event", events)
	}
	removed, _ := json.Marshal(events[0].Changes["removed"].From)
	for _, want := range []string{"keeps upstream codex/gpt-5.6; removed openai/gpt-5.6", `credential group \"team\"`, `\"Community\": removed the free flag`, "1 credential classification rule(s): team"} {
		if !strings.Contains(string(removed), want) {
			t.Errorf("removed settings %s lack %q", removed, want)
		}
	}
	reloaded := NewStore()
	reloaded.SetClock(func() time.Time { return now })
	if err := reloaded.Configure(Config{Enabled: true, StateFile: path, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := reloaded.AuditEvents("", 10); len(again) != 1 {
		t.Fatalf("restart repeated the conversion report: %+v", again)
	}
}
