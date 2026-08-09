package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestSchedulerPickNoGroupDefers(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatalf("expected Handled=false when no group, got %+v", resp)
	}
}

func TestSchedulerPickFiltersByPlanType(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{Metadata: map[string]any{
			"group": "team",
		}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-c-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "codex-b-team" {
		t.Fatalf("expected team-only pick, got %+v", resp)
	}
}

func TestSchedulerPickPriorityTiebreaksByID(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-z-team", Provider: "codex", Priority: 5, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-a-team", Provider: "codex", Priority: 5, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-m-team", Provider: "codex", Priority: 9, Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	// Higher priority wins.
	if resp.AuthID != "codex-m-team" {
		t.Fatalf("expected highest priority, got %q", resp.AuthID)
	}

	// Equal priority → lowest ID.
	req2, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-z-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-a-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw2, _ := app.HandleMethod(MethodSchedulerPick, req2)
	var resp2 SchedulerPickResponse
	if err := unmarshalOK(raw2, &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.AuthID != "codex-a-team" {
		t.Fatalf("expected lowest ID tiebreak, got %q", resp2.AuthID)
	}
}

// Isolation guarantee: when a tier group has no matching candidate, we must NOT
// fall back to a different tier. The plugin must return a structured scheduler
// error because an empty AuthID is invalid and would make the host fall back.
func TestSchedulerPickNoTierMatchRefusesFallback(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "auth_not_found" || envelope.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected auth_not_found scheduler error, got %+v", envelope)
	}
}

// "supported"/"unknown" group matches only untiered candidates: a key pinned to
// a real tier never lands on an untiered file, and an untiered key never stings
// onto a tiered file.
func TestSchedulerPickSupportedMatchesUntieredOnly(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "supported"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-no-claim", Provider: "codex", Attributes: map[string]string{}},
			{ID: "codex-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "codex-no-claim" {
		t.Fatalf("expected untiered pick, got %+v", resp)
	}
}

// Custom classify groups are matched with the classify: prefix so they never
// collide with built-in plan_type values like "free".
func TestSchedulerPickMatchesClassifyPrefix(t *testing.T) {
	app := NewApp()
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
classify_rules:
  - name: vip-files
    field: filename
    pattern: "vip"
    group: vip
    enabled: true
keys: []
`)
	reqCfg, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml, SchemaVersion: SchemaVersion})
	if _, err := app.HandleMethod(MethodPluginReconfigure, reqCfg); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "classify:vip"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "free-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "vip-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "vip-user.json" {
		t.Fatalf("expected vip-user via classify:vip, got %+v", resp)
	}

	// Bare "vip" (no prefix) must NOT match — isolation from unprefixed names.
	req2, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "vip"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "vip-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
		},
	})
	raw2, _ := app.HandleMethod(MethodSchedulerPick, req2)
	var envelope Envelope
	if err := json.Unmarshal(raw2, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "auth_not_found" {
		t.Fatalf("bare vip must return auth_not_found, got %+v", envelope)
	}
}

func TestCandidateClassifyCacheTracksAttributeChanges(t *testing.T) {
	app := NewApp()
	free := app.candidateGroups(SchedulerAuthCandidate{ID: "same.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}})
	team := app.candidateGroups(SchedulerAuthCandidate{ID: "same.json", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}})
	if len(free) != 1 || free[0] != "free" || len(team) != 1 || team[0] != "team" {
		t.Fatalf("cached groups did not track attributes: free=%v team=%v", free, team)
	}
}

func TestCandidateClassifyCacheIsBounded(t *testing.T) {
	app := NewApp()
	for index := 0; index < classifyCacheCapacity+25; index++ {
		app.candidateGroups(SchedulerAuthCandidate{ID: fmt.Sprintf("auth-%d", index), Provider: "codex"})
	}
	app.classifyMu.RLock()
	size := len(app.classifyCache)
	app.classifyMu.RUnlock()
	if size > classifyCacheCapacity {
		t.Fatalf("classify cache size = %d, capacity = %d", size, classifyCacheCapacity)
	}
}

// antigravity uses a "tier" attribute rather than plan_type; same filter logic.
func TestSchedulerPickMatchesAntigravityTier(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "antigravity",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "free-tier"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "ag-paid", Provider: "antigravity", Attributes: map[string]string{"tier": "paid-tier"}},
			{ID: "ag-free", Provider: "antigravity", Attributes: map[string]string{"tier": "free-tier"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "ag-free" {
		t.Fatalf("expected antigravity free-tier pick, got %+v", resp)
	}
}

func unmarshalOK(raw []byte, v any) error {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Result, v)
}
