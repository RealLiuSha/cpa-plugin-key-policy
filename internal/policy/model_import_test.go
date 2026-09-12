package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importItem(name, provider, target string, overwrite bool, input, output float64) ModelImportItem {
	return ModelImportItem{
		Name: name, Overwrite: overwrite, Input: input, Output: output,
		Targets: []ModelTarget{{Provider: provider, TargetModel: target}},
	}
}

func TestImportModelsDryRunCreatesNoState(t *testing.T) {
	store := configureImportStore(t, nil, nil)
	before, _ := os.ReadFile(store.StatePath())
	result, err := store.ImportModels([]ModelImportItem{importItem("gpt-4o", "openai", "gpt-4o", false, 5, 15)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "gpt-4o" {
		t.Fatalf("dry-run created = %+v", result)
	}
	after, _ := os.ReadFile(store.StatePath())
	if string(before) != string(after) {
		t.Fatal("dry-run wrote state")
	}
	if len(store.ModelsSnapshot()) != 0 {
		t.Fatalf("dry-run published models: %+v", store.ModelsSnapshot())
	}
}

func TestImportModelsDefaultSkipAndExplicitOverwrite(t *testing.T) {
	store := configureImportStore(t,
		[]ModelDefinition{tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)},
		[]KeyConfig{{ID: "k1", Models: modelRefs("gpt-4o")}},
	)
	skipped, err := store.ImportModels([]ModelImportItem{importItem("gpt-4o", "openai", "gpt-4o", false, 5, 15)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped.Skipped) != 1 || skipped.Skipped[0].Reason != "exists" || len(skipped.Skipped[0].AffectedKeys) != 1 {
		t.Fatalf("skip result = %+v", skipped)
	}
	if store.ModelsSnapshot()[0].InputPricePerMillion != 1 {
		t.Fatal("skip mutated existing model")
	}
	updated, err := store.ImportModels([]ModelImportItem{importItem("gpt-4o", "openai", "gpt-4o", true, 5, 15)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Updated) != 1 || updated.Updated[0].AffectedKeys[0] != "k1" {
		t.Fatalf("overwrite result = %+v", updated)
	}
	if store.ModelsSnapshot()[0].InputPricePerMillion != 5 {
		t.Fatalf("overwrite did not persist: %+v", store.ModelsSnapshot()[0])
	}
}

func TestImportModelsRejectsMissingPriceAndConflictsWithoutPublishing(t *testing.T) {
	store := configureImportStore(t, nil, nil)
	_, err := store.ImportModels([]ModelImportItem{
		{Name: "paid", Targets: []ModelTarget{{Provider: "openai", TargetModel: "m"}}, MissingPrice: true},
		importItem("ok", "openai", "ok", false, 1, 2),
	}, false)
	if err == nil {
		t.Fatal("missing price was accepted")
	}
	if len(store.ModelsSnapshot()) != 0 {
		t.Fatalf("partial publish after missing price: %+v", store.ModelsSnapshot())
	}
	preview, err := store.ImportModels([]ModelImportItem{
		importItem("dup", "openai", "a", false, 1, 2),
		importItem("dup", "codex", "b", false, 1, 2),
	}, true)
	if err != nil {
		t.Fatalf("dry-run should report conflicts without failing: %v", err)
	}
	if len(preview.Conflicts) == 0 {
		t.Fatalf("dry-run conflicts = %+v", preview)
	}
	if _, err := store.ImportModels([]ModelImportItem{
		importItem("dup", "openai", "a", false, 1, 2),
		importItem("dup", "codex", "b", false, 1, 2),
	}, false); err == nil {
		t.Fatal("duplicate batch names were applied")
	}
}

func TestImportModelsApplyIsAtomicAndAudited(t *testing.T) {
	store := configureImportStore(t, nil, nil)
	result, err := store.ImportModels([]ModelImportItem{
		importItem("fast", "codex", "gpt", false, 1, 2),
		importItem("slow", "openai", "gpt-mini", false, 3, 4),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 2 {
		t.Fatalf("created = %+v", result)
	}
	if len(store.ModelsSnapshot()) != 2 {
		t.Fatalf("published = %+v", store.ModelsSnapshot())
	}
	events, err := store.AuditEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range events {
		if event.Action == "import_create_model" {
			found++
		}
		raw := filepath.Join(filepath.Dir(store.StatePath()), "unused")
		_ = raw
		if strings.Contains(event.Action, "cpa_") {
			t.Fatalf("audit leaked secret in action: %+v", event)
		}
	}
	if found != 2 {
		t.Fatalf("create audit events = %d, want 2 from %+v", found, events)
	}
}
