package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDryRunParsesRequiredFlagsAndRepeatedFreeModels(t *testing.T) {
	directory := t.TempDir()
	statePath := copyMigrationFixture(t, filepath.Join("..", "..", "internal", "migration", "v2", "testdata", "state-v2.json"), filepath.Join(directory, "state.json"))
	usagePath := copyMigrationFixture(t, filepath.Join("..", "..", "internal", "migration", "v2", "testdata", "usage-v2.json"), filepath.Join(directory, "usage.json"))
	backupPath := filepath.Join(directory, "rollback")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-state", statePath,
		"-usage", usagePath,
		"-backup-dir", backupPath,
		"-timezone", "UTC",
		"-free-model", "Free",
		"-dry-run",
	}, &stdout, &stderr, func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("run: %v, stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "ready"`) || !strings.Contains(stdout.String(), `"summary"`) {
		t.Fatalf("report = %s", stdout.String())
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run created backup directory: %v", err)
	}
}

func TestRunRejectsPositionalArguments(t *testing.T) {
	err := run([]string{"unexpected"}, &bytes.Buffer{}, &bytes.Buffer{}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("error = %v", err)
	}
}

func copyMigrationFixture(t *testing.T, source, destination string) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return destination
}
