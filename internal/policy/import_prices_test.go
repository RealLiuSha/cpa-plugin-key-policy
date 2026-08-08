package policy

import (
	"path/filepath"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

func configureImportStore(t *testing.T, aliases []AliasMapping, keys []KeyConfig) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	cfg := Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Aliases:   aliases,
		Keys:      keys,
	}
	if err := store.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return store
}

func TestAliasesSnapshotWithRefs(t *testing.T) {
	store := configureImportStore(t,
		[]AliasMapping{
			{Alias: "shared", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}}, Dispatch: "round-robin", BillingMode: "tokens", InputPricePerMillion: 1},
			{Alias: "orphan", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-mini"}}, Dispatch: "round-robin"},
		},
		[]KeyConfig{
			{ID: "k1", Enabled: true, KeyHash: "h1", Aliases: []KeyAliasRef{{Alias: "shared"}}},
			{ID: "k2", Enabled: true, KeyHash: "h2", Aliases: []KeyAliasRef{{Alias: "shared"}}},
		},
	)
	list := store.AliasesSnapshotWithRefs()
	if len(list) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(list))
	}
	var shared, orphan *AliasWithRefs
	for i := range list {
		switch list[i].Alias {
		case "shared":
			shared = &list[i]
		case "orphan":
			orphan = &list[i]
		}
	}
	if shared == nil || orphan == nil {
		t.Fatalf("missing aliases: %+v", list)
	}
	if shared.RefCount != 2 {
		t.Fatalf("shared ref_count want 2, got %d", shared.RefCount)
	}
	if len(shared.RefKeys) != 2 {
		t.Fatalf("shared ref_keys want 2, got %v", shared.RefKeys)
	}
	got := map[string]bool{}
	for _, k := range shared.RefKeys {
		got[k] = true
	}
	if !got["k1"] || !got["k2"] {
		t.Fatalf("shared ref_keys missing k1/k2: %v", shared.RefKeys)
	}
	if orphan.RefCount != 0 {
		t.Fatalf("orphan ref_count want 0, got %d", orphan.RefCount)
	}
	if orphan.RefKeys == nil {
		t.Fatalf("orphan ref_keys must be non-nil empty slice")
	}
	if len(orphan.RefKeys) != 0 {
		t.Fatalf("orphan ref_keys want empty, got %v", orphan.RefKeys)
	}
}

func TestImportAliasPricesDryRunNoPersist(t *testing.T) {
	store := configureImportStore(t,
		[]AliasMapping{
			{
				Alias: "gpt-4o", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}},
				Dispatch: "round-robin", BillingMode: "tokens",
				InputPricePerMillion: 1, OutputPricePerMillion: 2, CacheReadPricePerMillion: 0.5,
			},
		},
		[]KeyConfig{
			{ID: "k1", Enabled: true, KeyHash: "h1", Aliases: []KeyAliasRef{{Alias: "gpt-4o"}}},
		},
	)
	res, err := store.ImportAliasPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25),
		CacheWritePricePer1M: fptr(99), // must be ignored
	}}, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("applied want 1, got %+v", res.Applied)
	}
	if res.Applied[0].NewInputPricePerMillion != 5 || res.Applied[0].NewOutputPricePerMillion != 30 {
		t.Fatalf("applied prices wrong: %+v", res.Applied[0])
	}
	// Dry-run must not mutate store or key resolved models.
	aliases := store.AliasesSnapshot()
	if aliases[0].InputPricePerMillion != 1 || aliases[0].OutputPricePerMillion != 2 {
		t.Fatalf("dry_run mutated alias prices: %+v", aliases[0])
	}
	key := store.findByID("k1")
	if key == nil || len(key.Models) == 0 {
		t.Fatalf("key models missing")
	}
	if key.Models[0].InputPricePerMillion != 1 {
		t.Fatalf("dry_run mutated key model price: %+v", key.Models[0])
	}
}

func TestImportAliasPricesApplyAffectsAllKeys(t *testing.T) {
	store := configureImportStore(t,
		[]AliasMapping{
			{
				Alias: "gpt-4o", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}},
				Dispatch: "round-robin", BillingMode: "tokens",
				InputPricePerMillion: 1, OutputPricePerMillion: 2, CacheReadPricePerMillion: 0.5,
			},
		},
		[]KeyConfig{
			{ID: "k1", Enabled: true, KeyHash: "h1", Aliases: []KeyAliasRef{{Alias: "gpt-4o"}}},
			{ID: "k2", Enabled: true, KeyHash: "h2", Aliases: []KeyAliasRef{{Alias: "gpt-4o"}}},
		},
	)
	res, err := store.ImportAliasPrices([]PriceImportMatch{{
		Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25),
	}}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("applied want 1, got %+v", res.Applied)
	}
	if len(res.AffectedKeys) != 2 {
		t.Fatalf("affected_keys want 2, got %v", res.AffectedKeys)
	}
	for _, id := range []string{"k1", "k2"} {
		key := store.findByID(id)
		if key == nil || len(key.Models) == 0 {
			t.Fatalf("%s models missing", id)
		}
		m := key.Models[0]
		if m.InputPricePerMillion != 5 || m.OutputPricePerMillion != 30 || m.CacheReadPricePerMillion != 1.25 {
			t.Fatalf("%s resolved prices want 5/30/1.25, got %v/%v/%v", id,
				m.InputPricePerMillion, m.OutputPricePerMillion, m.CacheReadPricePerMillion)
		}
	}
	// Reload from disk to prove persist.
	store2 := NewStore()
	store2.SetClock(func() time.Time { return time.Now() })
	if err := store2.Configure(Config{Enabled: true, StateFile: store.StatePath()}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := store2.AliasesSnapshot()
	if len(a) != 1 || a[0].InputPricePerMillion != 5 || a[0].OutputPricePerMillion != 30 {
		t.Fatalf("persisted prices wrong: %+v", a)
	}
}

func TestImportAliasPricesMatchRules(t *testing.T) {
	store := configureImportStore(t,
		[]AliasMapping{
			// target_model match (alias renamed)
			{Alias: "fast", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}}, Dispatch: "round-robin", BillingMode: "tokens"},
			// alias-name fallback only
			{Alias: "special-name", Targets: []AliasTarget{{Provider: "openai", TargetModel: "totally-other"}}, Dispatch: "round-robin", BillingMode: "tokens"},
			// multi-target conflict
			{Alias: "multi", Targets: []AliasTarget{
				{Provider: "openai", TargetModel: "model-a"},
				{Provider: "openai", TargetModel: "model-b"},
			}, Dispatch: "round-robin", BillingMode: "tokens"},
			// per_call keeps mode
			{Alias: "paid-call", Targets: []AliasTarget{{Provider: "openai", TargetModel: "call-model"}}, Dispatch: "round-robin", BillingMode: "per_call", PerCallUSD: 0.5},
		},
		nil,
	)
	res, err := store.ImportAliasPrices([]PriceImportMatch{
		{Model: "gpt-4o", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1)},
		{Model: "special-name", PromptPricePer1M: fptr(2), CompletionPricePer1M: fptr(4), CacheReadPricePer1M: fptr(0)},
		{Model: "model-a", PromptPricePer1M: fptr(1), CompletionPricePer1M: fptr(2), CacheReadPricePer1M: fptr(0)},
		{Model: "model-b", PromptPricePer1M: fptr(9), CompletionPricePer1M: fptr(9), CacheReadPricePer1M: fptr(0)},
		{Model: "call-model", PromptPricePer1M: fptr(3), CompletionPricePer1M: fptr(6), CacheReadPricePer1M: fptr(0.5)},
		{Model: "no-such-model", PromptPricePer1M: fptr(1), CompletionPricePer1M: fptr(1), CacheReadPricePer1M: fptr(0)},
	}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	applied := map[string]PriceImportApplied{}
	for _, a := range res.Applied {
		applied[a.Alias] = a
	}
	if _, ok := applied["fast"]; !ok {
		t.Fatalf("expected target_model match for fast: %+v", res)
	}
	if _, ok := applied["special-name"]; !ok {
		t.Fatalf("expected alias-name fallback for special-name: %+v", res)
	}
	if a, ok := applied["paid-call"]; !ok || a.Note == "" {
		t.Fatalf("per_call should apply token prices with note: %+v", res.Applied)
	}

	// multi conflict
	foundConflict := false
	foundNoMatch := false
	for _, s := range res.Skipped {
		if s.Alias == "multi" && s.Reason == "target_price_conflict" {
			foundConflict = true
		}
		if s.Model == "no-such-model" && s.Reason == "no_match" {
			foundNoMatch = true
		}
	}
	if !foundConflict {
		t.Fatalf("expected target_price_conflict for multi: %+v", res.Skipped)
	}
	if !foundNoMatch {
		t.Fatalf("expected no_match for no-such-model: %+v", res.Skipped)
	}

	aliases := store.AliasesSnapshot()
	byName := map[string]AliasMapping{}
	for _, a := range aliases {
		byName[a.Alias] = a
	}
	if byName["fast"].InputPricePerMillion != 5 {
		t.Fatalf("fast price not applied: %+v", byName["fast"])
	}
	if byName["special-name"].InputPricePerMillion != 2 {
		t.Fatalf("special-name price not applied: %+v", byName["special-name"])
	}
	if byName["multi"].InputPricePerMillion != 0 {
		t.Fatalf("multi should remain unchanged on conflict: %+v", byName["multi"])
	}
	if byName["paid-call"].BillingMode != "per_call" || byName["paid-call"].PerCallUSD != 0.5 {
		t.Fatalf("per_call mode must be preserved: %+v", byName["paid-call"])
	}
	if byName["paid-call"].InputPricePerMillion != 3 {
		t.Fatalf("per_call token prices should still update: %+v", byName["paid-call"])
	}
}

func TestImportAliasPricesUnchanged(t *testing.T) {
	store := configureImportStore(t,
		[]AliasMapping{{
			Alias: "m", Targets: []AliasTarget{{Provider: "openai", TargetModel: "m"}},
			Dispatch: "round-robin", BillingMode: "tokens",
			InputPricePerMillion: 5, OutputPricePerMillion: 30, CacheReadPricePerMillion: 1.25,
		}},
		nil,
	)
	res, err := store.ImportAliasPrices([]PriceImportMatch{{
		Model: "m", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30), CacheReadPricePer1M: fptr(1.25),
	}}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Applied) != 0 || len(res.Unchanged) != 1 {
		t.Fatalf("want unchanged only, got applied=%+v unchanged=%+v", res.Applied, res.Unchanged)
	}
}

func TestImportAliasPricesOmitsMissingFields(t *testing.T) {
	// Missing cache_read_price_per_1m must NOT zero the existing cache price.
	store := configureImportStore(t,
		[]AliasMapping{{
			Alias: "m", Targets: []AliasTarget{{Provider: "openai", TargetModel: "m"}},
			Dispatch: "round-robin", BillingMode: "tokens",
			InputPricePerMillion: 1, OutputPricePerMillion: 2, CacheReadPricePerMillion: 9.9,
		}},
		nil,
	)
	res, err := store.ImportAliasPrices([]PriceImportMatch{{
		Model: "m", PromptPricePer1M: fptr(5), CompletionPricePer1M: fptr(30),
		// CacheReadPricePer1M deliberately nil
	}}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("want 1 applied, got %+v", res)
	}
	a := store.AliasesSnapshot()[0]
	if a.InputPricePerMillion != 5 || a.OutputPricePerMillion != 30 {
		t.Fatalf("prompt/completion not applied: %+v", a)
	}
	if a.CacheReadPricePerMillion != 9.9 {
		t.Fatalf("missing cache_read must keep 9.9, got %v", a.CacheReadPricePerMillion)
	}
	if res.Applied[0].NewCacheReadPerMillion != 9.9 {
		t.Fatalf("applied record cache want 9.9, got %+v", res.Applied[0])
	}
}

func TestMigrateExistingAliasKeepsPrice(t *testing.T) {
	// Existing alias price is authoritative; migrate merges target only.
	store := configureImportStore(t,
		[]AliasMapping{{
			Alias: "gpt-4o", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}},
			Dispatch: "round-robin", BillingMode: "tokens",
			InputPricePerMillion: 10, OutputPricePerMillion: 20, CacheReadPricePerMillion: 1,
		}},
		[]KeyConfig{{ID: "k1", Enabled: true, KeyHash: "h1", Aliases: []KeyAliasRef{{Alias: "gpt-4o"}}}},
	)
	// Submit models with different prices — should not change global alias price.
	err := store.UpsertKey(KeyConfig{
		ID:      "k1",
		Enabled: true,
		KeyHash: "h1",
		Models: []ModelRule{{
			Alias: "gpt-4o", Provider: "openai", TargetModel: "gpt-4o",
			InputPricePerMillion: 99, OutputPricePerMillion: 99,
		}},
	}, true)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	aliases := store.AliasesSnapshot()
	if aliases[0].InputPricePerMillion != 10 || aliases[0].OutputPricePerMillion != 20 {
		t.Fatalf("existing alias price must be kept: %+v", aliases[0])
	}
}

func TestMigrateNewAliasZeroPrice(t *testing.T) {
	store := configureImportStore(t, nil, nil)
	err := store.UpsertKey(KeyConfig{
		ID:      "k1",
		Enabled: true,
		KeyHash: "h1",
		// No price fields → 0 = unpriced
		Models: []ModelRule{{
			Alias: "new-model", Provider: "openai", TargetModel: "new-model",
		}},
	}, true)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	aliases := store.AliasesSnapshot()
	if len(aliases) != 1 {
		t.Fatalf("want 1 alias, got %+v", aliases)
	}
	a := aliases[0]
	if a.InputPricePerMillion != 0 || a.OutputPricePerMillion != 0 || a.CacheReadPricePerMillion != 0 {
		t.Fatalf("new alias should be 0-priced: %+v", a)
	}
}
