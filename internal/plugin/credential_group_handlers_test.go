package plugin

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"cpa-key-policy/internal/policy"
)

// TestClassifyPreview verifies the classify-preview endpoint evaluates rules
// against credential descriptors and returns correct group mappings.
func TestClassifyPreview(t *testing.T) {
	app := NewApp()
	cfg := policy.Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		ClassifyRules: []policy.ClassifyRule{
			{Name: "team-rule", Field: "plan_type", Pattern: "^team$", Group: "team", Enabled: true},
			{Name: "free-rule", Field: "tier", Pattern: "^free$", Group: "free", Enabled: true},
		},
	}
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	// Build a classify-preview request with 3 descriptors.
	reqBody, _ := json.Marshal(map[string]any{
		"descriptors": []map[string]any{
			{"id": "codex-team-001", "provider": "codex", "attributes": map[string]string{"plan_type": "team"}},
			{"id": "codex-free-001", "provider": "codex", "attributes": map[string]string{"plan_type": "free"}},
			{"id": "unknown-001", "provider": "codex", "attributes": map[string]string{}},
		},
	})

	resp := app.classifyPreview(reqBody)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result struct {
		Groups      map[string][]string `json:"groups"`
		GroupCounts map[string]int      `json:"group_counts"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatal(err)
	}

	// "team" group should have 1 file (codex-team-001).
	if len(result.Groups["team"]) != 1 || result.Groups["team"][0] != "codex-team-001" {
		t.Fatalf("team group mismatch: %+v", result.Groups["team"])
	}
	// "supported" group should have 1 file (unknown-001 with no attributes).
	if len(result.Groups["supported"]) != 1 {
		t.Fatalf("supported group should have 1 file, got %+v", result.Groups["supported"])
	}
	// "free" group: plan_type=free doesn't match ^team$ (team-rule), but
	// free-rule checks tier, not plan_type. So codex-free-001 should fall to
	// built-in: plan_type=free → "free" group.
	if len(result.Groups["free"]) != 1 || result.Groups["free"][0] != "codex-free-001" {
		t.Fatalf("free group mismatch: %+v", result.Groups["free"])
	}
}

// TestClassifyPreviewCustomRules verifies that custom rules with a custom
// field name work correctly.
func TestClassifyPreviewCustomField(t *testing.T) {
	app := NewApp()
	cfg := policy.Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		ClassifyRules: []policy.ClassifyRule{
			{Name: "email-rule", Field: "email", Pattern: "@company\\.com$", Group: "company", Enabled: true},
		},
	}
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"descriptors": []map[string]any{
			{"id": "user1", "provider": "codex", "attributes": map[string]string{"email": "user@company.com"}},
			{"id": "user2", "provider": "codex", "attributes": map[string]string{"email": "user@other.com"}},
		},
	})

	resp := app.classifyPreview(reqBody)
	var result struct {
		Groups map[string][]string `json:"groups"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatal(err)
	}

	// "company" group should have user1.
	if len(result.Groups["company"]) != 1 || result.Groups["company"][0] != "user1" {
		t.Fatalf("company group mismatch: %+v", result.Groups["company"])
	}
	// user2 should fall to "supported" (no matching custom rule, no plan_type/tier).
	if len(result.Groups["supported"]) != 1 || result.Groups["supported"][0] != "user2" {
		t.Fatalf("supported group mismatch: %+v", result.Groups["supported"])
	}
}

// TestClassifyPreviewMultiGroup verifies that a credential matching multiple
// rules appears in multiple groups (multi-group semantics).
func TestClassifyPreviewMultiGroup(t *testing.T) {
	app := NewApp()
	cfg := policy.Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		ClassifyRules: []policy.ClassifyRule{
			{Name: "by-plan", Field: "plan_type", Pattern: "^team$", Group: "team", Enabled: true},
			{Name: "by-filename", Field: "filename", Pattern: "^codex-", Group: "codex-files", Enabled: true},
		},
	}
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"descriptors": []map[string]any{
			{"id": "codex-team-001", "provider": "codex", "attributes": map[string]string{"plan_type": "team"}},
		},
	})

	resp := app.classifyPreview(reqBody)
	var result struct {
		Groups map[string][]string `json:"groups"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatal(err)
	}

	// The credential should appear in BOTH "team" and "codex-files" groups.
	if len(result.Groups["team"]) != 1 || result.Groups["team"][0] != "codex-team-001" {
		t.Fatalf("team group mismatch: %+v", result.Groups["team"])
	}
	if len(result.Groups["codex-files"]) != 1 || result.Groups["codex-files"][0] != "codex-team-001" {
		t.Fatalf("codex-files group mismatch: %+v", result.Groups["codex-files"])
	}
}

// TestSchedulerCustomClassifyRule verifies that the scheduler's candidateGroups
// respects custom classify rules (multi-group, override built-in).
func TestSchedulerCustomClassifyRule(t *testing.T) {
	app := NewApp()
	cfg := policy.Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		ClassifyRules: []policy.ClassifyRule{
			{Name: "override-team", Field: "plan_type", Pattern: "^team$", Group: "custom-team", Enabled: true},
		},
	}
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	// A candidate with plan_type=team should be in "classify:custom-team"
	// (custom rule group names are prefixed so they never collide with built-in
	// plan_type values) AND NOT in bare "team" (custom match skips built-in).
	cand := SchedulerAuthCandidate{
		ID:         "codex-team-001",
		Provider:   "codex",
		Attributes: map[string]string{"plan_type": "team"},
	}
	groups := app.candidateGroups(cand)

	found := map[string]bool{}
	for _, g := range groups {
		found[g] = true
	}
	if !found["classify:custom-team"] {
		t.Fatalf("expected 'classify:custom-team' group, got %v", groups)
	}
	if found["custom-team"] {
		t.Fatalf("bare custom-team must not appear (needs classify: prefix), got %v", groups)
	}
	if found["team"] {
		t.Fatalf("built-in 'team' should not appear when custom rule matched, got %v", groups)
	}
}
