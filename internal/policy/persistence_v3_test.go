package policy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	policyPersist "cpa-key-policy/internal/policy/persist"
)

func TestFirstBootCreatesPairedCurrentDataset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := LoadUsage(policyPersist.UsagePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentStateFileVersion || usage.Version != currentUsageFileVersion || state.DatasetID == "" || state.DatasetID != usage.DatasetID {
		t.Fatalf("state=%+v usage=%+v", state, usage)
	}
}

func TestFirstBootPersistsGeneratedKeyTimestampsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	config := Config{
		Enabled:   true,
		StateFile: path,
		Models:    []ModelDefinition{freeTestModel("fast", "codex", "gpt")},
		Keys:      []KeyConfig{{ID: "team-a", Enabled: true, Models: modelRefs("fast")}},
	}
	first := NewStore()
	if err := first.Configure(config); err != nil {
		t.Fatal(err)
	}
	created := first.Keys()[0]
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("generated timestamps were not published: %+v", created)
	}

	second := NewStore()
	if err := second.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	reloaded := second.Keys()[0]
	if !reloaded.CreatedAt.Equal(created.CreatedAt) || !reloaded.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("timestamps changed across restart: first=%+v reloaded=%+v", created, reloaded)
	}
}

func TestRuntimeRejectsUnsupportedVersions(t *testing.T) {
	for name, version := range map[string]int{"below-current": 2, "future": 6} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			statePath := filepath.Join(directory, "state.json")
			usagePath := filepath.Join(directory, "usage.json")
			if err := os.WriteFile(statePath, []byte(`{"version":`+strconv.Itoa(version)+`,"keys":[],"updated_at":"2026-08-08T00:00:00Z"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(usagePath, []byte(`{"version":`+strconv.Itoa(version)+`,"usage":{},"updated_at":"2026-08-08T00:00:00Z"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			_, stateErr := LoadState(statePath)
			if stateErr == nil {
				t.Fatal("unsupported state version was accepted")
			}
			_, usageErr := LoadUsage(usagePath)
			if usageErr == nil {
				t.Fatal("unsupported usage version was accepted")
			}
			if !strings.Contains(stateErr.Error(), "require version 3 through 5") || !strings.Contains(usageErr.Error(), "require version 3 through 5") {
				t.Fatalf("version errors: state=%v usage=%v", stateErr, usageErr)
			}
		})
	}
}

func TestConfigureRejectsDatasetMismatchAndMissingPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, "state-dataset", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	usagePath := policyPersist.UsagePath(path)
	if err := SaveUsage(usagePath, "usage-dataset", map[string]*UsageState{}); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err == nil || !strings.Contains(err.Error(), "dataset_id mismatch") {
		t.Fatalf("mismatch error = %v", err)
	}
	if err := os.Remove(usagePath); err != nil {
		t.Fatal(err)
	}
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err == nil || !strings.Contains(err.Error(), "paired with state") {
		t.Fatalf("missing pair error = %v", err)
	}
	if err := os.WriteFile(usagePath, []byte(`{"version":2,"usage":{},"updated_at":"2026-08-08T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err == nil || !strings.Contains(err.Error(), "require version 3 through 5") {
		t.Fatalf("mixed-version pair error = %v", err)
	}
}

func TestUsageSaveWritesRealUpdatedAtAndPreservesInvariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	usage := map[string]*UsageState{
		"k": {
			Days:    map[string]UsageBucket{"2026-08-08": {TotalUSD: 1, CallCount: 1}},
			ByModel: map[string]map[string]UsageBucket{"fast": {"2026-08-08": {TotalUSD: 1, CallCount: 1}}},
		},
	}
	before := time.Now().UTC()
	usage["k"].Cycles = newUsageLedger(nil).newCycles(usage["k"], before, "initial")
	if err := SaveUsage(path, "dataset", usage); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpdatedAt.Before(before) || loaded.UpdatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("updated_at = %s, before=%s", loaded.UpdatedAt, before)
	}
	usage["k"].Days["2026-08-08"] = UsageBucket{TotalUSD: 2, CallCount: 1}
	if err := SaveUsage(path, "dataset", usage); err == nil || !strings.Contains(err.Error(), "by_model sum") {
		t.Fatalf("invariant error = %v", err)
	}
}

func TestFailedUsageFlushKeepsLedgerDirtyAndAdvancesUpdatedAtOnlyAfterSuccess(t *testing.T) {
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	store := NewStore()
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Models:    []ModelDefinition{perCallTestModel("image", "xai", "grok-image", 1)},
		Keys:      []KeyConfig{{ID: "team-a", Enabled: true, Models: modelRefs("image")}},
	}); err != nil {
		t.Fatal(err)
	}
	usagePath := policyPersist.UsagePath(statePath)
	beforeRaw, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := LoadUsage(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	store.RecordUsage("team-a", "image", "grok-image", false, UsageDetail{})

	blockedParent := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.statePath = filepath.Join(blockedParent, "state.json")
	store.mu.Unlock()
	if err := store.FlushUsage(); err == nil {
		t.Fatal("usage flush unexpectedly succeeded through a non-directory path")
	}
	afterFailureRaw, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterFailureRaw) != string(beforeRaw) {
		t.Fatal("failed flush changed the formal usage file")
	}

	store.mu.Lock()
	store.statePath = statePath
	store.mu.Unlock()
	time.Sleep(time.Millisecond)
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	after, err := LoadUsage(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("updated_at did not advance after the successful retry: before=%s after=%s", before.UpdatedAt, after.UpdatedAt)
	}
	if got := after.Usage["team-a"].Days; len(got) != 1 {
		t.Fatalf("failed flush was incorrectly marked clean; persisted days=%+v", got)
	}
}

func TestStateJSONRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := []byte(`{"version":3,"dataset_id":"dataset","keys":[],"models":[],"unexpected_routes":[],"updated_at":"2026-08-08T00:00:00Z"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}
