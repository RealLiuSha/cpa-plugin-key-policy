package policy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestV3ConfigRejectsRemovedRouteFields(t *testing.T) {
	for name, raw := range map[string]string{
		"top-level route table": `model_routes: []`,
		"key embedded route":    "models:\n  - name: fast\n    free: true\n    targets:\n      - provider: codex\n        target_model: gpt\nkeys:\n  - id: k\n    routes:\n      - name: fast",
		"direct model rule":     "models:\n  - name: fast\n    free: true\n    targets:\n      - provider: codex\n        target_model: gpt\nkeys:\n  - id: k\n    models:\n      - name: fast\n        provider: codex",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(raw)); err == nil {
				t.Fatal("expected legacy field to be rejected")
			}
		})
	}
}

func TestModelDefinitionValidation(t *testing.T) {
	valid := freeTestModel("Fast", "Codex", "gpt-5")
	cases := []struct {
		name   string
		models []ModelDefinition
		want   string
	}{
		{"empty name", []ModelDefinition{{Targets: valid.Targets, Free: true}}, "name is required"},
		{"duplicate name", []ModelDefinition{valid, freeTestModel("fast", "codex", "other")}, "duplicate model name"},
		{"no target", []ModelDefinition{{Name: "x", Free: true}}, "at least one target"},
		{"duplicate target", []ModelDefinition{{Name: "x", Free: true, Targets: []ModelTarget{{Provider: "codex", TargetModel: "gpt"}, {Provider: "CODEX", TargetModel: "GPT"}}}}, "duplicate target"},
		{"bad dispatch", []ModelDefinition{{Name: "x", Free: true, Dispatch: "random", Targets: valid.Targets}}, "dispatch"},
		{"negative price", []ModelDefinition{{Name: "x", Targets: valid.Targets, InputPricePerMillion: -1}}, "negative"},
		{"unpriced tokens", []ModelDefinition{{Name: "x", Targets: valid.Targets}}, "positive token price"},
		{"unpriced call", []ModelDefinition{{Name: "x", Targets: valid.Targets, BillingMode: "per_call"}}, "per_call_usd"},
		{"free with price", []ModelDefinition{{Name: "x", Free: true, Targets: valid.Targets, InputPricePerMillion: 1}}, "all price fields must be zero"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Models: test.models}
			if err := normalizeConfig(&cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
	cfg := Config{Models: []ModelDefinition{valid}, Keys: []KeyConfig{{ID: "k", Models: []KeyModelRef{{Name: " fast "}}}}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Models[0].Targets[0].Provider != "codex" || cfg.Keys[0].Models[0].Name != "Fast" {
		t.Fatalf("normalization failed: %+v", cfg)
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

func TestClassifyRuleValidation(t *testing.T) {
	cfg := Config{ClassifyRules: []ClassifyRule{{Name: "team", Field: "filename", Pattern: "[", Group: "team", Enabled: true}}}
	if err := normalizeConfig(&cfg); err == nil || !strings.Contains(err.Error(), "invalid regex") {
		t.Fatalf("error = %v", err)
	}
	cfg = Config{ClassifyRules: []ClassifyRule{
		{Name: "team", Field: "filename", Pattern: ".*", Group: "team"},
		{Name: "TEAM", Field: "provider", Pattern: ".*", Group: "other"},
	}}
	if err := normalizeConfig(&cfg); err == nil || !strings.Contains(err.Error(), "duplicate classify rule") {
		t.Fatalf("error = %v", err)
	}
}

func TestRoundRobinDispatch(t *testing.T) {
	store, plain := configuredDispatchStore(t, "round-robin")
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	got := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		route, _, ok := store.Route(headers, nil, "chat")
		if !ok {
			t.Fatal("route not handled")
		}
		got = append(got, route.TargetModel)
	}
	want := []string{"gpt-a", "gpt-b", "gpt-a", "gpt-b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence = %v, want %v", got, want)
		}
	}
}

func TestPriorityDispatch(t *testing.T) {
	store, plain := configuredDispatchStore(t, "priority")
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	for i := 0; i < 3; i++ {
		route, _, ok := store.Route(headers, nil, "chat")
		if !ok || route.TargetModel != "gpt-a" {
			t.Fatalf("route %d = %+v, ok=%v", i, route, ok)
		}
	}
}

func TestResponseModelLookupDoesNotAdvanceRoundRobin(t *testing.T) {
	store, plain := configuredDispatchStore(t, "round-robin")
	headers := http.Header{"Authorization": {"Bearer " + plain}}

	firstDecision := store.Authenticate(http.MethodPost, "/v1/chat/completions", headers, nil, []byte(`{"model":"chat"}`))
	if !firstDecision.Allowed {
		t.Fatalf("first authentication = %+v", firstDecision)
	}
	firstRoute, _, ok := store.Route(headers, nil, "chat")
	if !ok || firstRoute.TargetModel != "gpt-a" {
		t.Fatalf("first route = %+v, ok=%v", firstRoute, ok)
	}
	if publicModel, ok := store.ResponseModel(headers, nil, "chat"); !ok || publicModel != "chat" {
		t.Fatalf("response model = %q, ok=%v", publicModel, ok)
	}

	secondDecision := store.Authenticate(http.MethodPost, "/v1/chat/completions", headers, nil, []byte(`{"model":"chat"}`))
	if !secondDecision.Allowed {
		t.Fatalf("second authentication = %+v", secondDecision)
	}
	secondRoute, _, ok := store.Route(headers, nil, "chat")
	if !ok || secondRoute.TargetModel != "gpt-b" {
		t.Fatalf("response lookup advanced round-robin: second route = %+v, ok=%v", secondRoute, ok)
	}
}

func configuredDispatchStore(t *testing.T, dispatch string) (*Store, string) {
	t.Helper()
	plain := "dispatch-key"
	hash, _ := HashKey(plain)
	model := ModelDefinition{
		Name: "chat", Dispatch: dispatch, BillingMode: "tokens", Free: true,
		Targets: []ModelTarget{{Provider: "codex", TargetModel: "gpt-a"}, {Provider: "codex", TargetModel: "gpt-b"}},
	}
	store := NewStore()
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("chat")}},
	}); err != nil {
		t.Fatal(err)
	}
	return store, plain
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
	if got := store.ModelsSnapshot(); len(got) != 1 || got[0].Name != "Fast" || got[0].Targets[0].TargetModel != "gpt-next" {
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
	if len(models) != 1 || models[0].Targets[0].Provider != "codex" || models[0].Targets[0].TargetModel != "gpt" {
		t.Fatalf("failed persistence published a partial runtime model: %+v", models)
	}
}

func TestClassifyRuleCRUDAndReorder(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json")}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []ClassifyRule{
		{Name: "one", Field: "filename", Pattern: "one", Group: "one", Enabled: true},
		{Name: "two", Field: "filename", Pattern: "two", Group: "two", Enabled: true},
		{Name: "three", Field: "filename", Pattern: "three", Group: "three", Enabled: true},
	} {
		if err := store.UpsertClassifyRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReorderClassifyRules([]string{"three", "one"}); err != nil {
		t.Fatal(err)
	}
	rules := store.ClassifyRulesSnapshot()
	if rules[0].Name != "three" || rules[1].Name != "one" || rules[2].Name != "two" {
		t.Fatalf("reordered rules = %+v", rules)
	}
	if err := store.DeleteClassifyRule("one"); err != nil {
		t.Fatal(err)
	}
	if got := store.ClassifyRulesSnapshot(); len(got) != 2 {
		t.Fatalf("rules after delete = %+v", got)
	}
}

func TestClassifyRuleChangeCallbackCanReenterStore(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json")}); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	store.SetOnClassifyRulesChanged(func() {
		_ = store.ClassifyRulesSnapshot()
		called <- struct{}{}
	})
	if err := store.UpsertClassifyRule(ClassifyRule{Name: "x", Field: "filename", Pattern: ".*", Group: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("callback deadlocked while re-entering store")
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
	if state.Version != 3 || state.DatasetID == "" || len(state.Models) != 1 || state.Keys[0].Models[0].Name != "Fast" {
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
