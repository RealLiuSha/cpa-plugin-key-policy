package policy

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateModelsToAliases verifies that per-key Models are promoted to the
// global alias table and keys get alias references on decode.
func TestMigrateModelsToAliases(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: k1
    enabled: true
    key_hash: x
    models:
      - alias: fast
        provider: openai
        target_model: gpt-4o
      - alias: slow
        provider: anthropic
        target_model: claude-3
  - id: k2
    enabled: true
    key_hash: y
    models:
      - alias: fast
        provider: openai
        target_model: gpt-4o
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Two global aliases: "fast" (deduped from k1+k2) and "slow".
	if len(cfg.Aliases) != 2 {
		t.Fatalf("expected 2 global aliases, got %d: %+v", len(cfg.Aliases), cfg.Aliases)
	}
	// k1 should reference both aliases, k2 should reference only "fast".
	if len(cfg.Keys[0].Aliases) != 2 {
		t.Fatalf("k1 should have 2 alias refs, got %d", len(cfg.Keys[0].Aliases))
	}
	if len(cfg.Keys[1].Aliases) != 1 {
		t.Fatalf("k2 should have 1 alias ref, got %d", len(cfg.Keys[1].Aliases))
	}
	if cfg.Keys[1].Aliases[0].Alias != "fast" {
		t.Fatalf("k2 should reference 'fast', got %q", cfg.Keys[1].Aliases[0].Alias)
	}
	// Models should be cleared after migration.
	if len(cfg.Keys[0].Models) != 0 || len(cfg.Keys[1].Models) != 0 {
		t.Fatalf("Models should be cleared after migration")
	}
}

// TestMigrateModelsDedup verifies that the same alias+provider+target_model
// across keys is deduped into one global alias entry.
func TestMigrateModelsDedup(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: k1
    enabled: true
    key_hash: x
    models:
      - alias: shared
        provider: openai
        target_model: gpt-4o
        input_price_per_million: 5
  - id: k2
    enabled: true
    key_hash: y
    models:
      - alias: shared
        provider: openai
        target_model: gpt-4o
        input_price_per_million: 10
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// "shared" should appear once (deduped by alias+provider+target_model).
	if len(cfg.Aliases) != 1 {
		t.Fatalf("expected 1 global alias, got %d", len(cfg.Aliases))
	}
	// Both keys reference the same alias.
	if len(cfg.Keys[0].Aliases) != 1 || len(cfg.Keys[1].Aliases) != 1 {
		t.Fatalf("both keys should have 1 alias ref")
	}
}

// TestAliasValidation verifies strong validation of the global alias table.
func TestAliasValidation(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		errSub string
	}{
		{
			name: "duplicate alias name",
			yaml: `
enabled: true
state_file: "s.json"
aliases:
  - alias: dup
    targets: [{provider: openai, target_model: gpt-4o}]
  - alias: dup
    targets: [{provider: anthropic, target_model: claude}]
keys: []
`,
			errSub: "duplicate alias name",
		},
		{
			name: "alias without targets",
			yaml: `
enabled: true
state_file: "s.json"
aliases:
  - alias: empty
    targets: []
keys: []
`,
			errSub: "at least one target",
		},
		{
			name: "invalid dispatch",
			yaml: `
enabled: true
state_file: "s.json"
aliases:
  - alias: bad
    targets: [{provider: openai, target_model: gpt-4o}]
    dispatch: random
keys: []
`,
			errSub: "dispatch",
		},
		{
			name: "key references unknown alias",
			yaml: `
enabled: true
state_file: "s.json"
aliases:
  - alias: known
    targets: [{provider: openai, target_model: gpt-4o}]
keys:
  - id: k
    enabled: true
    key_hash: x
    aliases:
      - alias: unknown
`,
			errSub: "unknown alias",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeConfig([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errSub)
			}
		})
	}
}

// TestClassifyRuleValidation verifies that classify rules validate regex and
// require non-empty fields.
func TestClassifyRuleValidation(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
classify_rules:
  - name: bad-regex
    field: plan_type
    pattern: "[invalid"
    group: team
    enabled: true
keys: []
`)
	_, err := DecodeConfig(yaml)
	if err == nil {
		t.Fatalf("expected regex compilation error, got nil")
	}
}

// TestRoundRobinDispatch verifies that round-robin dispatch rotates through
// targets across multiple Route calls.
func TestRoundRobinDispatch(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	// Build a config with a multi-target alias using round-robin dispatch.
	cfg := Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Aliases: []AliasMapping{
			{
				Alias:    "multi",
				Dispatch: "round-robin",
				Targets: []AliasTarget{
					{Provider: "openai", TargetModel: "gpt-4o"},
					{Provider: "anthropic", TargetModel: "claude-3"},
					{Provider: "google", TargetModel: "gemini-pro"},
				},
				BillingMode: "tokens",
			},
		},
		Keys: []KeyConfig{
			{
				ID: "k", Enabled: true, KeyHash: "x",
				Aliases: []KeyAliasRef{{Alias: "multi"}},
			},
		},
	}
	if err := store.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer testkey"}}
	// We need a real key hash for authentication to work. For routing test,
	// call Route directly which only needs findBySecret to find the key.
	// Since we set KeyHash="x", we need to pass the matching key. But Route
	// calls findBySecret which hashes the input. Let's use the key ID path
	// instead — actually Route uses ExtractAPIKey + findBySecret.
	// Instead, test resolveRuleForAlias directly.
	key := store.findByID("k")
	if key == nil {
		t.Fatalf("key not found")
	}
	// Call resolveRuleForAlias 6 times and collect providers.
	seen := []string{}
	for i := 0; i < 6; i++ {
		rule, ok := store.resolveRuleForAlias(key, "multi")
		if !ok {
			t.Fatalf("route %d: expected match, got false", i)
		}
		seen = append(seen, rule.Provider)
	}
	// Should cycle through all 3 providers twice: [openai, anthropic, google, openai, anthropic, google]
	// (order depends on rrCounters start, but should hit all 3 in rotation)
	providerSet := map[string]bool{}
	for _, p := range seen {
		providerSet[p] = true
	}
	if len(providerSet) != 3 {
		t.Fatalf("round-robin should hit all 3 providers, got %d: %v", len(providerSet), seen)
	}
	// Verify it's actually rotating (not always the same one).
	if seen[0] == seen[1] {
		t.Fatalf("round-robin should rotate, but first two are same: %v", seen[:2])
	}
	_ = hdr // unused, routing tested via resolveRuleForAlias directly
}

// TestPriorityDispatch verifies that priority dispatch always returns the first target.
func TestPriorityDispatch(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	cfg := Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Aliases: []AliasMapping{
			{
				Alias:    "prio",
				Dispatch: "priority",
				Targets: []AliasTarget{
					{Provider: "openai", TargetModel: "gpt-4o"},
					{Provider: "anthropic", TargetModel: "claude-3"},
				},
				BillingMode: "tokens",
			},
		},
		Keys: []KeyConfig{
			{
				ID: "k", Enabled: true, KeyHash: "x",
				Aliases: []KeyAliasRef{{Alias: "prio"}},
			},
		},
	}
	if err := store.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}
	key := store.findByID("k")
	// Call 5 times — should always return "openai" (first target).
	for i := 0; i < 5; i++ {
		rule, ok := store.resolveRuleForAlias(key, "prio")
		if !ok {
			t.Fatalf("route %d: expected match", i)
		}
		if rule.Provider != "openai" {
			t.Fatalf("priority should always return first target 'openai', got %q on call %d", rule.Provider, i)
		}
	}
}

// TestUpsertAlias verifies adding and updating aliases via the Store API.
func TestUpsertAlias(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
	}); err != nil {
		t.Fatal(err)
	}
	// Add a new alias.
	if err := store.UpsertAlias(AliasMapping{
		Alias:    "test-alias",
		Targets:  []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}},
		Dispatch: "round-robin",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	aliases := store.AliasesSnapshot()
	if len(aliases) != 1 || aliases[0].Alias != "test-alias" {
		t.Fatalf("expected 1 alias 'test-alias', got %+v", aliases)
	}
	// Update the alias (add a target).
	if err := store.UpsertAlias(AliasMapping{
		Alias:    "test-alias",
		Targets:  []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}, {Provider: "anthropic", TargetModel: "claude"}},
		Dispatch: "priority",
	}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	aliases = store.AliasesSnapshot()
	if len(aliases) != 1 || len(aliases[0].Targets) != 2 || aliases[0].Dispatch != "priority" {
		t.Fatalf("update failed: %+v", aliases)
	}
}

// TestDeleteAliasReferencedBlocked verifies that deleting an alias referenced
// by a key returns an error.
func TestDeleteAliasReferencedBlocked(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	cfg := Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Aliases: []AliasMapping{
			{Alias: "ref", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}}, Dispatch: "round-robin"},
		},
		Keys: []KeyConfig{
			{ID: "k", Enabled: true, KeyHash: "x", Aliases: []KeyAliasRef{{Alias: "ref"}}},
		},
	}
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	err := store.DeleteAlias("ref")
	if err == nil {
		t.Fatalf("expected error deleting referenced alias, got nil")
	}
}

// TestClassifyRuleCRUD verifies creating, updating, and deleting classify rules.
func TestClassifyRuleCRUD(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
	}); err != nil {
		t.Fatal(err)
	}
	// Create a rule.
	rule := ClassifyRule{
		Name: "team-rule", Field: "plan_type", Pattern: "^team$", Group: "team", Enabled: true,
	}
	if err := store.UpsertClassifyRule(rule); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	rules := store.ClassifyRulesSnapshot()
	if len(rules) != 1 || rules[0].Name != "team-rule" {
		t.Fatalf("expected 1 rule, got %+v", rules)
	}
	// Update the rule.
	rule.Pattern = "^team_plus$"
	if err := store.UpsertClassifyRule(rule); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	rules = store.ClassifyRulesSnapshot()
	if rules[0].Pattern != "^team_plus$" {
		t.Fatalf("update failed: %+v", rules[0])
	}
	// Delete the rule.
	if err := store.DeleteClassifyRule("team-rule"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	rules = store.ClassifyRulesSnapshot()
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestClassifyRuleChangeCallbackCanReenterStore(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
	}); err != nil {
		t.Fatal(err)
	}

	callbackDone := make(chan struct{}, 3)
	store.SetOnClassifyRulesChanged(func() {
		_ = store.ClassifyRulesSnapshot()
		callbackDone <- struct{}{}
	})

	rule := ClassifyRule{
		Name: "reentrant", Field: "filename", Pattern: ".*", Group: "g", Enabled: true,
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "upsert", run: func() error { return store.UpsertClassifyRule(rule) }},
		{name: "reorder", run: func() error { return store.ReorderClassifyRules([]string{rule.Name}) }},
		{name: "delete", run: func() error { return store.DeleteClassifyRule(rule.Name) }},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			errCh := make(chan error, 1)
			go func() { errCh <- operation.run() }()
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("classify rule update deadlocked while callback re-entered Store")
			}
			select {
			case <-callbackDone:
			case <-time.After(2 * time.Second):
				t.Fatal("classify rule callback was not invoked")
			}
		})
	}
}

// TestReorderClassifyRules verifies that rules can be reordered.
func TestReorderClassifyRules(t *testing.T) {
	store := NewStore()
	store.SetClock(func() time.Time { return time.Now() })
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
	}); err != nil {
		t.Fatal(err)
	}
	// Create 3 rules.
	for _, name := range []string{"r1", "r2", "r3"} {
		if err := store.UpsertClassifyRule(ClassifyRule{
			Name: name, Field: "filename", Pattern: ".*", Group: "g", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Reorder to r3, r1, r2.
	if err := store.ReorderClassifyRules([]string{"r3", "r1", "r2"}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	rules := store.ClassifyRulesSnapshot()
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
	if rules[0].Name != "r3" || rules[1].Name != "r1" || rules[2].Name != "r2" {
		t.Fatalf("reorder failed, expected r3,r1,r2, got %s,%s,%s",
			rules[0].Name, rules[1].Name, rules[2].Name)
	}
}

// TestAliasStatePersistence verifies that the global alias table is persisted
// to state and survives a restart (state-only reload).
func TestAliasStatePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	mk := func() *Store {
		s := NewStore()
		s.SetClock(func() time.Time { return time.Now() })
		if err := s.Configure(Config{
			Enabled: true, StateFile: path,
			Aliases: []AliasMapping{
				{Alias: "persist-test", Targets: []AliasTarget{{Provider: "openai", TargetModel: "gpt-4o"}}, Dispatch: "round-robin"},
			},
			Keys: []KeyConfig{
				{ID: "k", Enabled: true, KeyHash: "x", Aliases: []KeyAliasRef{{Alias: "persist-test"}}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := mk()
	// Flush to persist state.
	if err := s1.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	// Restart: a new store with NO Aliases in config — should load from state.
	s2 := NewStore()
	s2.SetClock(func() time.Time { return time.Now() })
	if err := s2.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	aliases := s2.AliasesSnapshot()
	if len(aliases) != 1 || aliases[0].Alias != "persist-test" {
		t.Fatalf("alias not persisted/restored: %+v", aliases)
	}
	// Key should still reference it and Models should be populated.
	key := s2.findByID("k")
	if key == nil {
		t.Fatalf("key not found after restart")
	}
	if len(key.Aliases) != 1 || key.Aliases[0].Alias != "persist-test" {
		t.Fatalf("key alias ref not restored: %+v", key.Aliases)
	}
	if len(key.Models) != 1 || key.Models[0].Provider != "openai" {
		t.Fatalf("key Models not resolved from alias: %+v", key.Models)
	}
}

// TestMultiTargetAliasNoDuplicateModelError reproduces the bug where a key
// referencing a multi-target global alias had its Models expanded to multiple
// ModelRules sharing the same alias name (one per target), then validation
// rejected it as "duplicate model alias". With the fix, duplicate alias names
// in key.Models are legitimate (per multi-target aliases) and the migration
// produces a single deduped KeyAliasRef.
func TestMultiTargetAliasNoDuplicateModelError(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
aliases:
  - alias: tri
    targets:
      - {provider: openai, target_model: gpt-4o}
      - {provider: anthropic, target_model: claude-3}
      - {provider: google, target_model: gemini-pro}
    dispatch: round-robin
    billing_mode: tokens
keys:
  - id: k
    enabled: true
    key_hash: x
    # The frontend submits Models (one per selected target), all sharing the
    # alias name, since picker emits per-target ModelRules. This must NOT error.
    models:
      - {alias: tri, provider: openai, target_model: gpt-4o}
      - {alias: tri, provider: anthropic, target_model: claude-3}
      - {alias: tri, provider: google, target_model: gemini-pro}
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode should accept multi-target alias submit, got: %v", err)
	}
	// Migration should produce ONE deduped alias ref pointing at "tri".
	if len(cfg.Keys[0].Aliases) != 1 || cfg.Keys[0].Aliases[0].Alias != "tri" {
		t.Fatalf("expected single ref {tri}, got %+v", cfg.Keys[0].Aliases)
	}
	// Models should be cleared after migration.
	if len(cfg.Keys[0].Models) != 0 {
		t.Fatalf("Models should be cleared, got %d", len(cfg.Keys[0].Models))
	}
}

// TestReloadStateWithDuplicateAliasRefSelfHeals reproduces the state-file
// corruption produced by the pre-fix migration: a key with Aliases=[{test},
// {test}] (duplicate refs created by the buggy migration). The fixed
// normalization silently dedups the refs instead of erroring.
func TestReloadStateWithDuplicateAliasRefSelfHeals(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
aliases:
  - alias: test
    targets:
      - {provider: openai, target_model: gpt-4o}
      - {provider: anthropic, target_model: claude-3}
    dispatch: round-robin
    billing_mode: tokens
keys:
  - id: k
    enabled: true
    key_hash: x
    aliases:
      - alias: test
      - alias: test
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("duplicate alias ref should self-heal, got: %v", err)
	}
	// The deduped Aliases should be a single {test} entry.
	if len(cfg.Keys[0].Aliases) != 1 || cfg.Keys[0].Aliases[0].Alias != "test" {
		t.Fatalf("expected deduped single ref, got %+v", cfg.Keys[0].Aliases)
	}
}

// TestReloadStateWithDiskModelsDuplicatesNoError reproduces the user's bug
// "key gemma has duplicate model alias test": the on-disk state had the
// derived Models persisted with duplicate alias entries (one per target of a
// multi-target alias). The removed duplicate-model-alias check must accept
// these Models + a populated Aliases slice without error.
func TestReloadStateWithDiskModelsDuplicatesNoError(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
aliases:
  - alias: test
    targets:
      - {provider: openai, target_model: gpt-4o}
      - {provider: anthropic, target_model: claude-3}
    dispatch: round-robin
    billing_mode: tokens
keys:
  - id: gemma
    enabled: true
    key_hash: x
    aliases:
      - alias: test
    # Simulated pre-fix persisted derived Models (one ModelRule per target,
    # both with alias "test").
    models:
      - {alias: test, provider: openai, target_model: gpt-4o}
      - {alias: test, provider: anthropic, target_model: claude-3}
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("disk Models with duplicate alias names should be accepted, got: %v", err)
	}
	// Migration reconciles gemma's Aliases with the disk Models: the disk
	// Models both have alias "test" which already has a ref, so the ref is
	// preserved (deduped). Models is cleared (canonical source = Aliases).
	if len(cfg.Keys[0].Models) != 0 {
		t.Fatalf("migration should clear Models, got %d", len(cfg.Keys[0].Models))
	}
	if len(cfg.Keys[0].Aliases) != 1 || cfg.Keys[0].Aliases[0].Alias != "test" {
		t.Fatalf("expected single deduped ref {test}, got %+v", cfg.Keys[0].Aliases)
	}
}

// TestSaveStateStripsDerivedModels verifies that SaveState does NOT persist the
// derived Models field (so reload never re-triggers validation problems). The
// Configure path repopulates Models in memory from Aliases + global table.
func TestSaveStateStripsDerivedModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	keys := []KeyConfig{{
		ID: "k", Enabled: true, KeyHash: "x",
		Aliases: []KeyAliasRef{{Alias: "test", InputPricePerMillion: ptrF(1.0)}},
		Models: []ModelRule{
			{Alias: "test", Provider: "openai", TargetModel: "gpt-4o"},
			{Alias: "test", Provider: "anthropic", TargetModel: "claude-3"},
		},
	}}
	if err := SaveState(path, keys, []AliasMapping{{
		Alias: "test", Targets: []AliasTarget{
			{Provider: "openai", TargetModel: "gpt-4o"},
			{Provider: "anthropic", TargetModel: "claude-3"},
		},
		Dispatch: "round-robin", BillingMode: "tokens",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(loaded.Keys))
	}
	if len(loaded.Keys[0].Models) != 0 {
		t.Fatalf("SaveState must strip Models, got %+v", loaded.Keys[0].Models)
	}
	if len(loaded.Keys[0].Aliases) != 1 {
		t.Fatalf("SaveState must keep Aliases, got %+v", loaded.Keys[0].Aliases)
	}
}

func ptrF(v float64) *float64 { return &v }

// TestEditExistingKeyAddsAliasRef reproduces the user's bug: editing a key
// that already has alias refs and adding a new alias via the frontend (which
// submits the full Models list). The old migration skipped keys that already
// had Aliases, so the new alias never made it into the key's Aliases and was
// silently dropped on save. The fixed migration reconciles Aliases with the
// submitted Models: adds new refs, drops removed refs, preserves surviving
// price overrides.
func TestEditExistingKeyAddsAliasRef(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
aliases:
  - alias: existing
    targets: [{provider: openai, target_model: gpt-4o}]
    dispatch: round-robin
    billing_mode: tokens
  - alias: new-alias
    targets: [{provider: anthropic, target_model: claude-3}]
    dispatch: round-robin
    billing_mode: tokens
keys:
  - id: k
    enabled: true
    key_hash: x
    aliases:
      - alias: existing
    # Frontend submits the FULL models list (existing + new). Migration must
    # add a ref for new-alias without dropping existing.
    models:
      - {alias: existing, provider: openai, target_model: gpt-4o}
      - {alias: new-alias, provider: anthropic, target_model: claude-3}
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Keys[0].Aliases) != 2 {
		t.Fatalf("expected 2 alias refs after edit, got %d: %+v", len(cfg.Keys[0].Aliases), cfg.Keys[0].Aliases)
	}
	// Verify both refs present.
	names := map[string]bool{}
	for _, ref := range cfg.Keys[0].Aliases {
		names[ref.Alias] = true
	}
	if !names["existing"] || !names["new-alias"] {
		t.Fatalf("expected refs for 'existing' and 'new-alias', got %+v", cfg.Keys[0].Aliases)
	}
	// Models cleared after migration.
	if len(cfg.Keys[0].Models) != 0 {
		t.Fatalf("Models should be cleared, got %d", len(cfg.Keys[0].Models))
	}
}

// TestEditExistingKeyRemovesAliasRef verifies that removing a model (by
// clicking the chip ×) drops the corresponding alias ref on save.
func TestEditExistingKeyRemovesAliasRef(t *testing.T) {
	yaml := []byte(`
enabled: true
state_file: "s.json"
aliases:
  - alias: keep
    targets: [{provider: openai, target_model: gpt-4o}]
    dispatch: round-robin
    billing_mode: tokens
  - alias: drop
    targets: [{provider: anthropic, target_model: claude-3}]
    dispatch: round-robin
    billing_mode: tokens
keys:
  - id: k
    enabled: true
    key_hash: x
    aliases:
      - alias: keep
      - alias: drop
    # Frontend submits only the 'keep' model (user removed 'drop' chip).
    models:
      - {alias: keep, provider: openai, target_model: gpt-4o}
`)
	cfg, err := DecodeConfig(yaml)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Keys[0].Aliases) != 1 || cfg.Keys[0].Aliases[0].Alias != "keep" {
		t.Fatalf("expected single ref {keep}, got %+v", cfg.Keys[0].Aliases)
	}
}
