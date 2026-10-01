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
	shared := tokenTestModel("shared", "openai", "gpt-4o", 1, 2)
	shared.CacheWritePricePerMillion = fptr(3)
	store := configureImportStore(t,
		[]ModelDefinition{shared, tokenTestModel("orphan", "openai", "gpt-mini", 1, 2)},
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
	sharedSnapshot := byName["shared"]
	if sharedSnapshot.CacheWritePricePerMillion == nil {
		t.Fatalf("shared cache-write price missing: %+v", sharedSnapshot)
	}
	*sharedSnapshot.CacheWritePricePerMillion = 99
	for _, got := range store.ModelsSnapshotWithRefs() {
		if got.Name == "shared" && (got.CacheWritePricePerMillion == nil || *got.CacheWritePricePerMillion != 3) {
			t.Fatalf("snapshot mutation leaked into store: %+v", got)
		}
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
	priced := tokenTestModel("gpt-4o", "openai", "gpt-4o", 1, 2)
	priced.BillingMultiplier = 1.2
	store := configureImportStore(t,
		[]ModelDefinition{priced},
		[]KeyConfig{{ID: "k1", Models: modelRefs("gpt-4o")}, {ID: "k2", Models: modelRefs("gpt-4o")}},
	)
	result, err := store.ImportModelPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25), CacheWritePricePer1M: fptr(3.75),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || len(result.AffectedKeys) != 2 {
		t.Fatalf("result = %+v", result)
	}
	model := store.ModelsSnapshot()[0]
	if model.BillingMultiplier != 1.2 {
		t.Fatal("price import overwrote billing multiplier")
	}
	if model.InputPricePerMillion != 5 || model.OutputPricePerMillion != 30 || model.CacheReadPricePerMillion != 1.25 || model.CacheWritePricePerMillion == nil || *model.CacheWritePricePerMillion != 3.75 {
		t.Fatalf("applied model = %+v", model)
	}
	reloaded := NewStore()
	if err := reloaded.Configure(Config{Enabled: true, StateFile: store.StatePath()}); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ModelsSnapshot()[0]; got.InputPricePerMillion != 5 || got.OutputPricePerMillion != 30 || got.CacheWritePricePerMillion == nil || *got.CacheWritePricePerMillion != 3.75 {
		t.Fatalf("persisted model = %+v", got)
	}
	events, err := store.AuditEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Action == "import_update_prices" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing import_update_prices audit: %+v", events)
	}
}

func TestImportModelPricesMatchingAndNoMatch(t *testing.T) {
	store := configureImportStore(t, []ModelDefinition{
		tokenTestModel("fast", "openai", "gpt-4o", 1, 2),
		tokenTestModel("special-name", "openai", "totally-other", 1, 2),
		perCallTestModel("paid-call", "openai", "call-model", 0.5),
	}, nil)
	result, err := store.ImportModelPrices([]PriceImportMatch{
		{Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30)},
		{Model: "special-name", PromptPricePer1M: fptr(2), CompletionPricePer1M: fptr(4)},
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
		t.Fatalf("missing upstream match: %+v", result)
	}
	if _, ok := applied["special-name"]; !ok {
		t.Fatalf("missing name fallback: %+v", result)
	}
	if applied["paid-call"].Note == "" {
		t.Fatalf("per-call note missing: %+v", result.Applied)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].MatchModel != "no-such-model" || result.Skipped[0].Reason != "no_match" {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

// Imported models start at $0; syncing prices must be able to price them.
func TestImportModelPricesPricesZeroPricedModels(t *testing.T) {
	store := configureImportStore(t, []ModelDefinition{freeTestModel("fresh", "openai", "fresh")}, nil)
	result, err := store.ImportModelPrices([]PriceImportMatch{{Model: "fresh", PromptPricePer1M: fptr(1)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || store.ModelsSnapshot()[0].InputPricePerMillion != 1 {
		t.Fatalf("result = %+v", result)
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
