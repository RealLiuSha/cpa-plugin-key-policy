package policy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsUnknownField(t *testing.T) {
	if _, err := DecodeConfig([]byte("unexpected_routes: []")); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func TestModelDefinitionValidation(t *testing.T) {
	valid := freeTestModel("Fast", "Codex", "gpt-5")
	cases := []struct {
		name   string
		models []ModelDefinition
		want   string
	}{
		{"empty name", []ModelDefinition{{Provider: "codex", TargetModel: "gpt"}}, "name is required"},
		{"duplicate name", []ModelDefinition{valid, freeTestModel("fast", "codex", "other")}, "duplicate model name"},
		{"no upstream", []ModelDefinition{{Name: "x"}}, "provider and target_model are required"},
		{"bad billing mode", []ModelDefinition{{Name: "x", Provider: "codex", TargetModel: "gpt", BillingMode: "monthly"}}, "billing_mode"},
		{"negative price", []ModelDefinition{{Name: "x", Provider: "codex", TargetModel: "gpt", InputPricePerMillion: -1}}, "negative"},
		{"multiplier below one", []ModelDefinition{{Name: "x", Provider: "codex", TargetModel: "gpt", BillingMultiplier: 0.5}}, "billing_multiplier"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Models: test.models}
			if err := normalizeConfig(&cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
	unpriced := []ModelDefinition{valid, {Name: "image", Provider: "xai", TargetModel: "grok-imagine", BillingMode: "per_call"}}
	cfg := Config{Models: unpriced, Keys: []KeyConfig{{ID: "k", Models: []KeyModelRef{{Name: " fast "}}}}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatalf("zero prices must be accepted: %v", err)
	}
	if cfg.Models[0].Provider != "codex" || cfg.Models[0].BillingMultiplier != 1 || cfg.Keys[0].Models[0].Name != "Fast" {
		t.Fatalf("normalization failed: %+v", cfg)
	}
}

// An unchanged CPA config.yaml written for older releases must keep
// registering; removed settings are ignored and seeds keep the first target.
func TestLegacyConfigSeedsStillParse(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
models:
  - name: chat
    dispatch: priority
    free: true
    targets:
      - {provider: Codex, target_model: gpt-a, group: team}
      - {provider: openai, target_model: gpt-b}
classify_rules:
  - {name: team, field: filename, pattern: team, group: team, enabled: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].Provider != "Codex" || cfg.Models[0].TargetModel != "gpt-a" {
		t.Fatalf("seed projection = %+v", cfg.Models)
	}
	if _, err := ParseConfig([]byte("models:\n  - {name: x, provider: codex, target_model: a, targets: [{provider: codex, target_model: b}]}\n")); err == nil {
		t.Fatal("a model with both upstream forms must be rejected")
	}
}

func TestKeyModelReferenceValidation(t *testing.T) {
	model := freeTestModel("Fast", "codex", "gpt")
	for name, key := range map[string]KeyConfig{
		"unknown":   {ID: "k", Models: modelRefs("missing")},
		"duplicate": {ID: "k", Models: []KeyModelRef{{Name: "fast"}, {Name: "FAST"}}},
		"negative":  {ID: "k", Models: []KeyModelRef{{Name: "fast", DailyLimitUSD: -1}}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Models: []ModelDefinition{model}, Keys: []KeyConfig{key}}
			if err := normalizeConfig(&cfg); err == nil {
				t.Fatal("expected invalid reference")
			}
		})
	}
}

func TestRouteResolvesTheModelUpstream(t *testing.T) {
	plain := "route-key"
	hash, _ := HashKey(plain)
	store := NewStore()
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models: []ModelDefinition{freeTestModel("chat", "codex", "gpt-a"), freeTestModel("other", "codex", "gpt-b")},
		Keys:   []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("chat")}},
	}); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	if model, keyID, ok := store.Route(headers, nil, "CHAT"); !ok || keyID != "k" || model.Provider != "codex" || model.TargetModel != "gpt-a" {
		t.Fatalf("route = %+v %q %v", model, keyID, ok)
	}
	if _, _, ok := store.Route(headers, nil, "other"); ok {
		t.Fatal("a model the key does not reference must not route")
	}
}

func TestUpsertAndDeleteModel(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json")}); err != nil {
		t.Fatal(err)
	}
	model := freeTestModel("Fast", "codex", "gpt")
	if err := store.UpsertModel(model); err != nil {
		t.Fatal(err)
	}
	if got := store.ModelsSnapshot(); len(got) != 1 || got[0].Name != "Fast" {
		t.Fatalf("models = %+v", got)
	}
	key := KeyConfig{ID: "k", Enabled: true, KeyHash: "sha256:x", Models: modelRefs("fast")}
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	updated := freeTestModel(" fast ", "openai", "gpt-next")
	if err := store.UpsertModel(updated); err != nil {
		t.Fatal(err)
	}
	if got := store.ModelsSnapshot(); len(got) != 1 || got[0].Name != "Fast" || got[0].TargetModel != "gpt-next" {
		t.Fatalf("case-insensitive update changed model identity: %+v", got)
	}
	if got := store.Keys()[0].Models[0].Name; got != "Fast" {
		t.Fatalf("model update changed key reference identity to %q", got)
	}
	if err := store.DeleteModel("FAST"); err == nil || !strings.Contains(err.Error(), "referenced by 1 key") {
		t.Fatalf("delete referenced model error = %v", err)
	}
	key.Models = []KeyModelRef{}
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteModel("fast"); err != nil {
		t.Fatal(err)
	}
}

func TestModelMutationSaveFailureDoesNotPublishRuntimeIndex(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Models:    []ModelDefinition{freeTestModel("fast", "codex", "gpt")},
	}); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.statePath = filepath.Join(blockedParent, "state.json")
	store.mu.Unlock()

	if err := store.UpsertModel(freeTestModel("fast", "openai", "gpt-next")); err == nil {
		t.Fatal("model update unexpectedly succeeded with an invalid persistence path")
	}
	models := store.ModelsSnapshot()
	if len(models) != 1 || models[0].Provider != "codex" || models[0].TargetModel != "gpt" {
		t.Fatalf("failed persistence published a partial runtime model: %+v", models)
	}
}

func TestModelStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	model := tokenTestModel("Fast", "codex", "gpt", 1, 2)
	if err := store.Configure(Config{
		Enabled: true, StateFile: path, Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Models: []KeyModelRef{{Name: "fast", DailyLimitUSD: 3}}}},
	}); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentStateFileVersion || state.DatasetID == "" || len(state.Models) != 1 || state.Keys[0].Models[0].Name != "Fast" {
		t.Fatalf("persisted state = %+v", state)
	}
	reloaded := NewStore()
	if err := reloaded.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ModelsSnapshotWithRefs(); len(got) != 1 || got[0].RefCount != 1 {
		t.Fatalf("reloaded models = %+v", got)
	}
}

func TestEditExistingKeyAddsAndRemovesModelRef(t *testing.T) {
	store := NewStore()
	models := []ModelDefinition{freeTestModel("a", "codex", "a"), freeTestModel("b", "codex", "b")}
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: models,
		Keys: []KeyConfig{{ID: "k", Models: modelRefs("a")}},
	}); err != nil {
		t.Fatal(err)
	}
	key := store.Keys()[0]
	key.Models = modelRefs("a", "b")
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	if got := store.Keys()[0].Models; len(got) != 2 {
		t.Fatalf("after add = %+v", got)
	}
	key = store.Keys()[0]
	key.Models = modelRefs("b")
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	if got := store.Keys()[0].Models; len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("after remove = %+v", got)
	}
}
