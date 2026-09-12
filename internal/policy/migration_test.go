package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if persisted.Version != 5 {
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
