package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
)

func TestRunIsRepeatableAndReportsBeforeAfterTotals(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "policy", "testdata", "legacy-usage-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	usagePath := filepath.Join(dir, "usage.json")
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, location)
	now := func() time.Time { return at }

	dryArgs := []string{"-state", statePath, "-out", usagePath, "-timezone", "Asia/Shanghai", "-dry-run"}
	var firstDry, secondDry bytes.Buffer
	if err := run(dryArgs, &firstDry, &bytes.Buffer{}, now); err != nil {
		t.Fatal(err)
	}
	if err := run(dryArgs, &secondDry, &bytes.Buffer{}, now); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstDry.Bytes(), secondDry.Bytes()) {
		t.Fatalf("dry-run output changed:\n%s\n%s", firstDry.Bytes(), secondDry.Bytes())
	}
	if _, err := os.Stat(usagePath); !os.IsNotExist(err) {
		t.Fatalf("dry-run created output: %v", err)
	}
	var report struct {
		Migrated     bool                                   `json:"migrated"`
		BeforeTotals map[string]policy.UsageMigrationTotals `json:"before_totals"`
		AfterTotals  map[string]policy.UsageMigrationTotals `json:"after_totals"`
	}
	if err := json.Unmarshal(firstDry.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	before, after := report.BeforeTotals["rebirth"], report.AfterTotals["rebirth"]
	if !report.Migrated || before.DailyUSD != 31.1 || before.WeeklyUSD != 103.45 || before.MonthlyUSD != nil ||
		after.DailyUSD != before.DailyUSD || after.WeeklyUSD != before.WeeklyUSD || after.MonthlyUSD == nil || *after.MonthlyUSD != 103.45 {
		t.Fatalf("migration report = %+v", report)
	}

	writeArgs := []string{"-state", statePath, "-out", usagePath, "-timezone", "Asia/Shanghai"}
	var firstWrite, secondWrite bytes.Buffer
	if err := run(writeArgs, &firstWrite, &bytes.Buffer{}, now); err != nil {
		t.Fatal(err)
	}
	firstUsage, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(writeArgs, &secondWrite, &bytes.Buffer{}, now); err != nil {
		t.Fatal(err)
	}
	secondUsage, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstWrite.Bytes(), secondWrite.Bytes()) || !bytes.Equal(firstUsage, secondUsage) {
		t.Fatal("repeated migration changed its report or usage output")
	}

	// After the plugin rewrites state as v2, usage exists only in the separate
	// file. A migration rerun must load that file instead of overwriting it with
	// an empty ledger.
	if err := policy.SaveState(statePath, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var v2Report bytes.Buffer
	if err := run(writeArgs, &v2Report, &bytes.Buffer{}, now); err != nil {
		t.Fatal(err)
	}
	v2Usage, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondUsage, v2Usage) {
		t.Fatal("v2 state rerun erased or changed the independent usage file")
	}
	var v2 struct {
		Migrated     bool                                   `json:"migrated"`
		BeforeTotals map[string]policy.UsageMigrationTotals `json:"before_totals"`
		AfterTotals  map[string]policy.UsageMigrationTotals `json:"after_totals"`
	}
	if err := json.Unmarshal(v2Report.Bytes(), &v2); err != nil {
		t.Fatal(err)
	}
	v2Before, v2After := v2.BeforeTotals["rebirth"], v2.AfterTotals["rebirth"]
	if v2.Migrated || v2Before.DailyUSD != v2After.DailyUSD || v2Before.WeeklyUSD != v2After.WeeklyUSD ||
		v2Before.MonthlyUSD == nil || v2After.MonthlyUSD == nil || *v2Before.MonthlyUSD != *v2After.MonthlyUSD || v2After.WeeklyUSD != 103.45 {
		t.Fatalf("v2 rerun report = %+v", v2)
	}
}

func TestRunRejectsMissingState(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(nil, &bytes.Buffer{}, &stderr, time.Now); err == nil || err.Error() != "-state is required" {
		t.Fatalf("missing state error = %v", err)
	}
}

func TestRunRefusesToOverwriteState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	original := []byte(`{"version":2,"keys":[]}`)
	if err := os.WriteFile(statePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-state", statePath, "-out", statePath}, &bytes.Buffer{}, &bytes.Buffer{}, time.Now)
	if err == nil || err.Error() != "output usage path must differ from state path" {
		t.Fatalf("same-path error = %v", err)
	}
	after, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("state file changed: %s", after)
	}
}
