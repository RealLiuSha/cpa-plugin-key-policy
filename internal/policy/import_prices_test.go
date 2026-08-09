package policy

import (
	"path/filepath"
	"testing"
	"time"
)

func fptr(value float64) *float64 { return &value }

func configureImportStore(t *testing.T, models []ModelDefinition, keys []KeyConfig) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(time.Now)
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: models, Keys: keys,
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return store
}

func TestModelsSnapshotWithRefs(t *testing.T) {
	store := configureImportStore(t,
		[]ModelDefinition{tokenTestModel("shared", "openai", "gpt-4o", 1, 2), tokenTestModel("orphan", "openai", "gpt-mini", 1, 2)},
		[]KeyConfig{{ID: "k1", Models: modelRefs("shared")}, {ID: "k2", Models: modelRefs("shared")}},
	)
	list := store.ModelsSnapshotWithRefs()
	if len(list) != 2 {
		t.Fatalf("models = %+v", list)
	}
	byName := map[string]ModelWithRefs{}
	for _, model := range list {
		byName[model.Name] = model
	}
	if byName["shared"].RefCount != 2 || len(byName["shared"].RefKeys) != 2 {
		t.Fatalf("shared refs = %+v", byName["shared"])
	}
	if byName["orphan"].RefCount != 0 || byName["orphan"].RefKeys == nil {
		t.Fatalf("orphan refs = %+v", byName["orphan"])
	}
}

func TestImportModelPricesDryRunNoPersist(t *testing.T) {
	store := configureImportStore(t,
		[]ModelDefinition{tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)},
		[]KeyConfig{{ID: "k1", Models: modelRefs("gpt-4o")}},
	)
	result, err := store.ImportModelPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25), CacheWritePricePer1M: fptr(99),
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0].NewInputPricePerMillion != 5 {
		t.Fatalf("dry-run result = %+v", result)
	}
	model := store.ModelsSnapshot()[0]
	if model.InputPricePerMillion != 1 || model.OutputPricePerMillion != 2 {
		t.Fatalf("dry-run mutated model: %+v", model)
	}
}

func TestImportModelPricesDryRunRejectsInvalidPrices(t *testing.T) {
	store := configureImportStore(t,
		[]ModelDefinition{tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)},
		nil,
	)
	if _, err := store.ImportModelPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(-1),
	}}, true); err == nil {
		t.Fatal("dry-run accepted a price that apply would reject")
	}
	if got := store.ModelsSnapshot()[0].InputPricePerMillion; got != 1 {
		t.Fatalf("invalid dry-run mutated model price to %v", got)
	}
}

func TestImportModelPricesApplyAffectsAllKeysAndPersists(t *testing.T) {
	store := configureImportStore(t,
		[]ModelDefinition{tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)},
		[]KeyConfig{{ID: "k1", Models: modelRefs("gpt-4o")}, {ID: "k2", Models: modelRefs("gpt-4o")}},
	)
	result, err := store.ImportModelPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || len(result.AffectedKeys) != 2 {
		t.Fatalf("result = %+v", result)
	}
	model := store.ModelsSnapshot()[0]
	if model.InputPricePerMillion != 5 || model.OutputPricePerMillion != 30 || model.CacheReadPricePerMillion != 1.25 {
		t.Fatalf("applied model = %+v", model)
	}
	reloaded := NewStore()
	if err := reloaded.Configure(Config{Enabled: true, StateFile: store.StatePath()}); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ModelsSnapshot()[0]; got.InputPricePerMillion != 5 || got.OutputPricePerMillion != 30 {
		t.Fatalf("persisted model = %+v", got)
	}
}

func TestImportModelPricesMatchingConflictAndNoMatch(t *testing.T) {
	perCall := perCallTestModel("paid-call", "openai", "call-model", 0.5)
	multi := tokenTestModel("multi", "openai", "model-a", 1, 2)
	multi.Targets = append(multi.Targets, ModelTarget{Provider: "openai", TargetModel: "model-b"})
	store := configureImportStore(t, []ModelDefinition{
		tokenTestModel("fast", "openai", "gpt-4o", 1, 2),
		tokenTestModel("special-name", "openai", "totally-other", 1, 2),
		multi,
		perCall,
	}, nil)
	result, err := store.ImportModelPrices([]PriceImportMatch{
		{Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30)},
		{Model: "special-name", PromptPricePer1M: fptr(2), CompletionPricePer1M: fptr(4)},
		{Model: "model-a", PromptPricePer1M: fptr(1), CompletionPricePer1M: fptr(2)},
		{Model: "model-b", PromptPricePer1M: fptr(9), CompletionPricePer1M: fptr(9)},
		{Model: "call-model", PromptPricePer1M: fptr(3), CompletionPricePer1M: fptr(6)},
		{Model: "no-such-model", PromptPricePer1M: fptr(1), CompletionPricePer1M: fptr(1)},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	applied := map[string]PriceImportApplied{}
	for _, model := range result.Applied {
		applied[model.Model] = model
	}
	if _, ok := applied["fast"]; !ok {
		t.Fatalf("missing target match: %+v", result)
	}
	if _, ok := applied["special-name"]; !ok {
		t.Fatalf("missing name fallback: %+v", result)
	}
	if applied["paid-call"].Note == "" {
		t.Fatalf("per-call note missing: %+v", result.Applied)
	}
	conflict, noMatch := false, false
	for _, skipped := range result.Skipped {
		conflict = conflict || (skipped.Model == "multi" && skipped.Reason == "target_price_conflict")
		noMatch = noMatch || (skipped.MatchModel == "no-such-model" && skipped.Reason == "no_match")
	}
	if !conflict || !noMatch {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

func TestImportModelPricesRejectsPartialMultiTargetMatch(t *testing.T) {
	multi := tokenTestModel("multi", "openai", "model-a", 1, 2)
	multi.Targets = append(multi.Targets, ModelTarget{Provider: "openai", TargetModel: "model-b"})
	store := configureImportStore(t, []ModelDefinition{multi}, nil)

	result, err := store.ImportModelPrices([]PriceImportMatch{{
		Model: "model-a", PromptPricePer1M: fptr(9), CompletionPricePer1M: fptr(10),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "target_price_conflict" {
		t.Fatalf("partial multi-target result = %+v", result)
	}
	got := store.ModelsSnapshot()[0]
	if got.InputPricePerMillion != 1 || got.OutputPricePerMillion != 2 {
		t.Fatalf("partial match changed shared prices: %+v", got)
	}
}

func TestImportModelPricesProtectsFreeModels(t *testing.T) {
	store := configureImportStore(t, []ModelDefinition{freeTestModel("free", "openai", "free")}, nil)
	result, err := store.ImportModelPrices([]PriceImportMatch{{Model: "free", PromptPricePer1M: fptr(1)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != "free_model" {
		t.Fatalf("result = %+v", result)
	}
	if !store.ModelsSnapshot()[0].Free {
		t.Fatal("price import changed free state")
	}
}

func TestImportModelPricesKeepsMissingFields(t *testing.T) {
	model := tokenTestModel("m", "openai", "m", 1, 2)
	model.CacheReadPricePerMillion = 9.9
	store := configureImportStore(t, []ModelDefinition{model}, nil)
	result, err := store.ImportModelPrices([]PriceImportMatch{{Model: "m", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	updated := store.ModelsSnapshot()[0]
	if updated.CacheReadPricePerMillion != 9.9 || result.Applied[0].NewCacheReadPerMillion != 9.9 {
		t.Fatalf("missing cache price was overwritten: model=%+v result=%+v", updated, result)
	}
}
