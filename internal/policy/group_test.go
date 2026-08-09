package policy

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestModelTargetGroupRoundTrip(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
models:
  - name: fast
    free: true
    targets:
      - provider: Codex
        target_model: gpt-5-codex
        group: TEAM
  - name: any
    free: true
    targets:
      - provider: codex
        target_model: gpt-5-codex
keys:
  - id: k
    enabled: true
    key_hash: x
    models:
      - name: fast
      - name: any
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Keys) != 1 || len(cfg.Keys[0].Models) != 2 || len(cfg.Models) != 2 {
		t.Fatalf("unexpected decode: %+v", cfg)
	}
	if cfg.Models[0].Targets[0].Group != "team" || cfg.Models[1].Targets[0].Group != "" {
		t.Fatalf("group normalization failed: %+v", cfg.Models)
	}
	raw, err := json.Marshal(cfg.Models)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []ModelDefinition
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0].Targets[0].Group != "team" {
		t.Fatalf("JSON round trip lost group: %+v", decoded)
	}
}

func TestAuthenticateSurfacesGroup(t *testing.T) {
	store := NewStore()
	hash, _ := HashKey("k1")
	model := freeTestModel("fast", "codex", "gpt-5-codex")
	model.Targets[0].Group = "team"
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("fast")}},
	}); err != nil {
		t.Fatal(err)
	}
	decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer k1"}}, nil, []byte(`{"model":"fast"}`))
	if !decision.Allowed || decision.Route.Group != "team" {
		t.Fatalf("expected group on route, got %+v", decision)
	}
}

func TestAuthenticateAndRouteShareMultiTargetGroup(t *testing.T) {
	store := NewStore()
	plain := "multi-group-key"
	hash, _ := HashKey(plain)
	model := ModelDefinition{
		Name: "mixed", Dispatch: "round-robin", BillingMode: "tokens", Free: true,
		Targets: []ModelTarget{
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "free"},
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "team"},
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "plus"},
		},
	}
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("mixed")}},
	}); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		decision := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"mixed"}`))
		route, keyID, ok := store.Route(headers, nil, "mixed")
		if !decision.Allowed || !ok || keyID != "k" {
			t.Fatalf("request %d: auth=%+v route=%+v ok=%v key=%q", i, decision, route, ok, keyID)
		}
		if route.Group != decision.Route.Group || route.Provider != decision.Route.Provider || route.TargetModel != decision.Route.TargetModel {
			t.Fatalf("request %d: auth route %+v != routed target %+v", i, decision.Route, route)
		}
		seen[route.Group]++
	}
	for _, group := range []string{"free", "team", "plus"} {
		if seen[group] == 0 {
			t.Fatalf("round robin missed %q: %v", group, seen)
		}
	}
}

func TestAuthenticateAndRoutePriorityKeepsFirstGroup(t *testing.T) {
	store := NewStore()
	plain := "prio-group-key"
	hash, _ := HashKey(plain)
	model := ModelDefinition{
		Name: "prio", Dispatch: "priority", BillingMode: "tokens", Free: true,
		Targets: []ModelTarget{
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "team"},
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "free"},
		},
	}
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("prio")}},
	}); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	for i := 0; i < 3; i++ {
		decision := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"prio"}`))
		route, _, ok := store.Route(headers, nil, "prio")
		if !decision.Allowed || decision.Route.Group != "team" || !ok || route.Group != "team" {
			t.Fatalf("iteration %d: decision=%+v route=%+v", i, decision, route)
		}
	}
}

func TestRouteWithoutAuthenticateStillResolves(t *testing.T) {
	store := NewStore()
	plain := "route-only-key"
	hash, _ := HashKey(plain)
	model := ModelDefinition{
		Name: "solo", Dispatch: "round-robin", BillingMode: "tokens", Free: true,
		Targets: []ModelTarget{
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "free"},
			{Provider: "codex", TargetModel: "gpt-5.4-mini", Group: "team"},
		},
	}
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Models: []ModelDefinition{model},
		Keys: []KeyConfig{{ID: "k", Enabled: true, KeyHash: hash, Models: modelRefs("solo")}},
	}); err != nil {
		t.Fatal(err)
	}
	route, _, ok := store.Route(http.Header{"Authorization": {"Bearer " + plain}}, nil, "solo")
	if !ok || route.Group == "" {
		t.Fatalf("expected route-only multi-target resolve, got ok=%v route=%+v", ok, route)
	}
}
