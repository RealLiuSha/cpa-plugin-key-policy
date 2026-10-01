package policy

import (
	"os"
	"testing"
)

func TestImportModelsCreatesZeroPricedModelsAndSkipsExisting(t *testing.T) {
	existing := tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)
	existing.BillingMultiplier = 1.2
	store := configureImportStore(t, []ModelDefinition{existing}, []KeyConfig{{ID: "k1", Models: modelRefs("gpt-4o")}})
	result, err := store.ImportModels([]ModelImportItem{
		{Provider: "openai", TargetModel: "gpt-4o"},
		{Provider: "xai", TargetModel: "grok-4.7"},
		{Name: "image", Provider: "xai", TargetModel: "grok-imagine-image"},
		{Name: "IMAGE", Provider: "xai", TargetModel: "grok-imagine-image-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 2 || result.Created[0].Name != "grok-4.7" || result.Created[1].Name != "image" {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Skipped) != 2 || result.Skipped[0].Reason != "exists" || result.Skipped[1].Reason != "duplicate" {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
	byName := map[string]ModelDefinition{}
	for _, model := range store.ModelsSnapshot() {
		byName[model.Name] = model
	}
	if got := byName["gpt-4o"]; got.InputPricePerMillion != 1 || got.BillingMultiplier != 1.2 {
		t.Fatalf("import changed an existing model: %+v", got)
	}
	if got := byName["grok-4.7"]; got.Provider != "xai" || got.InputPricePerMillion != 0 || got.BillingMultiplier != 1 || got.BillingMode != "tokens" {
		t.Fatalf("imported model = %+v, want a $0 token model", got)
	}
	reloaded := NewStore()
	if err := reloaded.Configure(Config{Enabled: true, StateFile: store.StatePath()}); err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.ModelsSnapshot()); got != 3 {
		t.Fatalf("persisted models = %d, want 3", got)
	}
}

func TestImportModelsRejectsInvalidBatchWithoutWriting(t *testing.T) {
	store := configureImportStore(t, nil, nil)
	before, _ := os.ReadFile(store.StatePath())
	if _, err := store.ImportModels([]ModelImportItem{
		{Provider: "openai", TargetModel: "ok"},
		{Name: "no-provider", TargetModel: "m"},
	}); err == nil {
		t.Fatal("an item without provider was accepted")
	}
	after, _ := os.ReadFile(store.StatePath())
	if string(before) != string(after) || len(store.ModelsSnapshot()) != 0 {
		t.Fatalf("rejected batch changed state: %+v", store.ModelsSnapshot())
	}
}
