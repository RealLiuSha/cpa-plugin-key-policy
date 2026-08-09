package policy

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestUpsertKeyReferencesMultiTargetModelWithoutPersistingRoutes(t *testing.T) {
	plain := "multi-target-key"
	hash, _ := HashKey(plain)
	model := ModelDefinition{
		Name: "test", Dispatch: "round-robin", BillingMode: "tokens", Free: true,
		Targets: []ModelTarget{
			{Provider: "nvidia", TargetModel: "z-ai/glm-5.2"},
			{Provider: "opencode", TargetModel: "glm-5.2"},
		},
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKey(KeyConfig{ID: "mt-key", Enabled: true, KeyHash: hash, Models: modelRefs("test")}, true); err != nil {
		t.Fatal(err)
	}
	key := store.findByID("mt-key")
	if key == nil || len(key.Models) != 1 || key.Models[0].Name != "test" {
		t.Fatalf("key = %+v", key)
	}
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	first, _, ok := store.Route(headers, nil, "test")
	if !ok {
		t.Fatal("first route not handled")
	}
	second, _, ok := store.Route(headers, nil, "test")
	if !ok || first.TargetModel == second.TargetModel {
		t.Fatalf("round robin routes = %+v then %+v", first, second)
	}
	state, err := LoadState(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Keys[0].Models) != 1 || len(state.Models[0].Targets) != 2 {
		t.Fatalf("persisted state = %+v", state)
	}
}
