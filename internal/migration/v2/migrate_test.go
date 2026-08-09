package v2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
)

func fixedMigrationTime() time.Time {
	return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
}

func copyFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	usagePath := filepath.Join(dir, "usage.json")
	for source, target := range map[string]string{
		filepath.Join("testdata", "state-v2.json"): statePath,
		filepath.Join("testdata", "usage-v2.json"): usagePath,
	} {
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte("{\"action\":\"create_alias\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditPath+".1", []byte("{\"action\":\"update_alias\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return statePath, usagePath, auditPath
}

func migrationOptions(statePath, usagePath, auditPath, backupDir string, dryRun bool) Options {
	return Options{
		StatePath: statePath, UsagePath: usagePath, AuditPath: auditPath, BackupDir: backupDir,
		Timezone: "UTC", FreeModels: []string{"Free"}, DryRun: dryRun,
	}
}

func TestDryRunIsDeterministicAndHasNoFileSideEffects(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	stateBefore, _ := os.ReadFile(statePath)
	usageBefore, _ := os.ReadFile(usagePath)
	var first, second bytes.Buffer
	for _, output := range []*bytes.Buffer{&first, &second} {
		if err := Run(migrationOptions(statePath, usagePath, auditPath, backupDir, true), output, fixedMigrationTime); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("dry-run reports differ:\n%s\n%s", first.Bytes(), second.Bytes())
	}
	if !bytes.Contains(first.Bytes(), []byte(`"status": "ready"`)) {
		t.Fatalf("report = %s", first.Bytes())
	}
	var report Report
	if err := json.Unmarshal(first.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.Before.DailyUSD != 1.5 || report.Summary.Before.WeeklyUSD != 2.5 || report.Summary.Before.MonthlyUSD == nil || *report.Summary.Before.MonthlyUSD != 2.5 {
		t.Fatalf("before summary = %+v", report.Summary.Before)
	}
	if !usageTotalsEqual(report.Summary.Before, report.Summary.After) {
		t.Fatalf("summary changed: %+v", report.Summary)
	}
	if _, err := os.Stat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created backup directory: %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestUsageReportSummaryUsesDeterministicKeyOrder(t *testing.T) {
	monthlyA, monthlyB, monthlyC := 175.208475, 0.00000000000003, 937.739043
	totals := map[string]policy.UsageMigrationTotals{
		"z": {DailyUSD: 175.208475, WeeklyUSD: 937.739043, MonthlyUSD: &monthlyA},
		"a": {DailyUSD: 0.00000000000003, WeeklyUSD: 0.00000000000003, MonthlyUSD: &monthlyB},
		"m": {DailyUSD: 937.739043, WeeklyUSD: 937.739043, MonthlyUSD: &monthlyC},
	}
	want := sumUsageTotals(totals)
	for index := 0; index < 100; index++ {
		if got := sumUsageTotals(totals); !usageTotalsEqual(got, want) {
			t.Fatalf("iteration %d summary changed: got=%+v want=%+v", index, got, want)
		}
	}
}

func usageTotalsEqual(left, right policy.UsageMigrationTotals) bool {
	return left.DailyUSD == right.DailyUSD && left.WeeklyUSD == right.WeeklyUSD &&
		left.MonthlyUSD != nil && right.MonthlyUSD != nil && *left.MonthlyUSD == *right.MonthlyUSD
}

func TestMigrationCreatesRollbackPackageAndV3IsIdempotent(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	stateBefore, _ := os.ReadFile(statePath)
	usageBefore, _ := os.ReadFile(usagePath)
	var output bytes.Buffer
	if err := Run(migrationOptions(statePath, usagePath, auditPath, backupDir, false), &output, fixedMigrationTime); err != nil {
		t.Fatal(err)
	}
	state, err := policy.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := policy.LoadUsage(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 3 || state.DatasetID == "" || state.DatasetID != usage.DatasetID {
		t.Fatalf("state=%+v usage=%+v", state, usage)
	}
	if len(state.Keys) != 2 || len(state.Models) != 3 || len(state.Keys[0].Models) != 2 {
		t.Fatalf("migrated state = %+v", state)
	}
	if got := usage.Usage["team-a"].ByModel["Chat"]["2026-08-08"]; got.TotalUSD != 0.5 {
		t.Fatalf("canonical model bucket = %+v", got)
	}
	if _, err := os.Stat(auditPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active audit still exists: %v", err)
	}
	if _, err := os.Stat(auditPath + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotated audit still exists: %v", err)
	}
	assertFileBytes(t, filepath.Join(backupDir, filepath.Base(statePath)), stateBefore)
	assertFileBytes(t, filepath.Join(backupDir, filepath.Base(usagePath)), usageBefore)
	checksums, err := os.ReadFile(filepath.Join(backupDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), sha256String(stateBefore)) || !strings.Contains(string(checksums), sha256String(usageBefore)) {
		t.Fatalf("SHA256SUMS = %s", checksums)
	}
	stateHash := sha256String(mustRead(t, statePath))
	usageHash := sha256String(mustRead(t, usagePath))
	var rerun bytes.Buffer
	secondBackup := filepath.Join(filepath.Dir(statePath), "unused-second-backup")
	if err := Run(migrationOptions(statePath, usagePath, auditPath, secondBackup, false), &rerun, fixedMigrationTime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rerun.Bytes(), []byte(`"status": "already_migrated"`)) {
		t.Fatalf("rerun report = %s", rerun.Bytes())
	}
	if stateHash != sha256String(mustRead(t, statePath)) || usageHash != sha256String(mustRead(t, usagePath)) {
		t.Fatal("v3 rerun changed migrated files")
	}
	if _, err := os.Stat(secondBackup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v3 rerun created backup: %v", err)
	}
}

func TestConversionRejectsUnsafeInputs(t *testing.T) {
	stateRaw := mustRead(t, filepath.Join("testdata", "state-v2.json"))
	usageRaw := mustRead(t, filepath.Join("testdata", "usage-v2.json"))
	location := time.UTC
	for name, mutate := range map[string]func(map[string]any, map[string]any){
		"unknown state field": func(state map[string]any, _ map[string]any) { state["unexpected"] = true },
		"version mismatch":    func(_ map[string]any, usage map[string]any) { usage["version"] = float64(3) },
		"dangling ref": func(state map[string]any, _ map[string]any) {
			keys := state["keys"].([]any)
			keys[0].(map[string]any)["aliases"] = []any{map[string]any{"alias": "missing"}}
		},
		"inconsistent buckets": func(_ map[string]any, usage map[string]any) {
			entries := usage["usage"].(map[string]any)
			team := entries["team-a"].(map[string]any)
			days := team["days"].(map[string]any)
			days["2026-08-08"].(map[string]any)["total_usd"] = float64(99)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var state, usage map[string]any
			if err := json.Unmarshal(stateRaw, &state); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(usageRaw, &usage); err != nil {
				t.Fatal(err)
			}
			mutate(state, usage)
			stateMutated, _ := json.Marshal(state)
			usageMutated, _ := json.Marshal(usage)
			if _, err := Convert(stateMutated, usageMutated, []string{"Free"}, fixedMigrationTime(), location, "UTC", "/backup"); err == nil {
				t.Fatal("unsafe input was accepted")
			}
		})
	}
	if _, err := Convert(stateRaw, usageRaw, nil, fixedMigrationTime(), location, "UTC", "/backup"); err == nil || !strings.Contains(err.Error(), "-free-model") {
		t.Fatalf("zero-price decision error = %v", err)
	}
	if _, err := Convert(stateRaw, usageRaw, []string{"Free", "missing"}, fixedMigrationTime(), location, "UTC", "/backup"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unused free-model decision error = %v", err)
	}
	if _, err := Convert(stateRaw, usageRaw, []string{"Free", "Chat"}, fixedMigrationTime(), location, "UTC", "/backup"); err == nil || !strings.Contains(err.Error(), "every v2 price field is zero") {
		t.Fatalf("priced free-model decision error = %v", err)
	}
}

func TestSecondReplaceFailureRestoresOriginalFiles(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	renameCalls := 0
	operations := defaultFileOperations
	operations.rename = func(from, to string) error {
		renameCalls++
		if renameCalls == 2 {
			return errors.New("injected second rename failure")
		}
		return os.Rename(from, to)
	}
	err := run(migrationOptions(statePath, usagePath, auditPath, backupDir, false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil || !strings.Contains(err.Error(), "replace usage") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestPostReplaceValidationFailureRestoresOriginalFiles(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	operations := defaultFileOperations
	operations.validatePair = func(string, string, string) error {
		return errors.New("injected pair validation failure")
	}
	err := run(migrationOptions(statePath, usagePath, auditPath, filepath.Join(filepath.Dir(statePath), "rollback"), false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil || !strings.Contains(err.Error(), "validate replaced state/usage pair") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestBackupDirectoryMustBeEmptyBeforeWrites(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "existing"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(migrationOptions(statePath, usagePath, auditPath, backupDir, false), ioDiscard{}, fixedMigrationTime); err == nil {
		t.Fatal("non-empty backup directory was accepted")
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

type ioDiscard struct{}

func (ioDiscard) Write(raw []byte) (int, error) { return len(raw), nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected report write failure")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := mustRead(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s changed\ngot: %s\nwant: %s", path, got, want)
	}
}

func sha256String(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestStagedFilesAreRereadBeforeReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := writeValidatedTemp(path, []byte(`{"version":3,"dataset_id":"x","keys":[],"models":[],"updated_at":"bad"}`), true, "x"); err == nil {
		t.Fatal("invalid staged state passed reread validation")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("formal path was created: %v", err)
	}
}

func TestFirstReplaceFailureLeavesFormalFilesUntouched(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	operations := defaultFileOperations
	operations.rename = func(string, string) error { return errors.New("injected first rename failure") }
	err := run(migrationOptions(statePath, usagePath, auditPath, filepath.Join(filepath.Dir(statePath), "rollback"), false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil || !strings.Contains(err.Error(), "replace state") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestReportWriteFailureLeavesFormalFilesUntouched(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	err := Run(migrationOptions(statePath, usagePath, auditPath, backupDir, false), failingWriter{}, fixedMigrationTime)
	if err == nil || !strings.Contains(err.Error(), "write migration report") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
	if _, err := os.Stat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("report failure created backup directory: %v", err)
	}
}

func TestBackupFailureLeavesFormalFilesUntouched(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	operations := defaultFileOperations
	operations.writeBackup = func(string, map[string][]byte) error {
		return errors.New("injected backup failure")
	}
	err := run(migrationOptions(statePath, usagePath, auditPath, filepath.Join(filepath.Dir(statePath), "rollback"), false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil || !strings.Contains(err.Error(), "injected backup failure") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestStagedWriteFailureLeavesFormalFilesUntouched(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore := mustRead(t, statePath), mustRead(t, usagePath)
	operations := defaultFileOperations
	operations.writeValidatedTemp = func(string, []byte, bool, string) (string, error) {
		return "", errors.New("injected staged write failure")
	}
	err := run(migrationOptions(statePath, usagePath, auditPath, filepath.Join(filepath.Dir(statePath), "rollback"), false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil || !strings.Contains(err.Error(), "injected staged write failure") {
		t.Fatalf("error = %v", err)
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
}

func TestBackupFilesAreOwnerOnly(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	backupDir := filepath.Join(filepath.Dir(statePath), "rollback")
	if err := Run(migrationOptions(statePath, usagePath, auditPath, backupDir, false), ioDiscard{}, fixedMigrationTime); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("backup %s mode = %o, want 600", entry.Name(), info.Mode().Perm())
		}
	}
}

func TestAuditRemovalFailureRestoresDataset(t *testing.T) {
	statePath, usagePath, auditPath := copyFixture(t)
	stateBefore, usageBefore, auditBefore := mustRead(t, statePath), mustRead(t, usagePath), mustRead(t, auditPath)
	operations := defaultFileOperations
	operations.remove = func(path string) error {
		if path == auditPath {
			return errors.New("injected audit removal failure")
		}
		return os.Remove(path)
	}
	err := run(migrationOptions(statePath, usagePath, auditPath, filepath.Join(filepath.Dir(statePath), "rollback"), false), ioDiscard{}, fixedMigrationTime, operations)
	if err == nil {
		t.Fatal("audit removal failure was ignored")
	}
	assertFileBytes(t, statePath, stateBefore)
	assertFileBytes(t, usagePath, usageBefore)
	assertFileBytes(t, auditPath, auditBefore)
}
