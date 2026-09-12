package policy

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-key-policy/internal/policy/audit"
)

func TestManagementMutationsProduceAuditEvents(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(dir, "state.json"), Models: []ModelDefinition{freeTestModel("fast", "codex", "m")}}); err != nil {
		t.Fatal(err)
	}
	hash, err := HashKey("cpa_audit")
	if err != nil {
		t.Fatal(err)
	}
	key := KeyConfig{ID: "audit-key", Enabled: true, KeyHash: hash, DailyLimitUSD: 1, WeeklyLimitUSD: 5, MonthlyLimitUSD: 10, Models: modelRefs("fast")}
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	key = store.Keys()[0]
	key.DailyLimitUSD = 2
	key.WeeklyLimitUSD = 6
	key.MonthlyLimitUSD = 20
	if err := store.UpsertKey(key, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RotateKey(key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetUsageWindow(key.ID, UsageResetDaily, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteKey(key.ID); err != nil {
		t.Fatal(err)
	}
	model := freeTestModel("audit-model", "codex", "m")
	if err := store.UpsertModel(model); err != nil {
		t.Fatal(err)
	}
	model.Dispatch = "priority"
	if err := store.UpsertModel(model); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteModel(model.Name); err != nil {
		t.Fatal(err)
	}
	rule := ClassifyRule{Name: "audit-rule", Field: "provider", Pattern: "codex", Group: "paid", Enabled: true}
	if err := store.UpsertClassifyRule(rule); err != nil {
		t.Fatal(err)
	}
	rule.Pattern = "openai|codex"
	if err := store.UpsertClassifyRule(rule); err != nil {
		t.Fatal(err)
	}
	if err := store.ReorderClassifyRules([]string{rule.Name}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteClassifyRule(rule.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportModels([]ModelImportItem{{
		Name: "imported", Targets: []ModelTarget{{Provider: "codex", TargetModel: "m"}}, Input: 1, Output: 2,
	}}, false); err != nil {
		t.Fatal(err)
	}
	events, err := store.AuditEvents("", 100)
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]int)
	for _, event := range events {
		actions[event.Action]++
		if event.Actor != "management-api" {
			t.Fatalf("actor = %q", event.Actor)
		}
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, []byte("cpa_audit")) {
			t.Fatalf("audit leaked plaintext key: %s", encoded)
		}
	}
	for _, action := range []string{"create_key", "update_key", "rotate_key", "reset_usage", "delete_key", "create_model", "update_model", "delete_model", "create_classify_rule", "update_classify_rule", "reorder_classify_rules", "delete_classify_rule", "import_create_model"} {
		if actions[action] == 0 {
			t.Errorf("missing audit action %q: %+v", action, actions)
		}
	}
	if actions["update_key"] != 1 || actions["rotate_key"] != 1 {
		t.Fatalf("key update and rotation must each emit exactly one semantic event: %+v", actions)
	}
	keyEvents, err := store.AuditEvents("audit-key", 100)
	if err != nil {
		t.Fatal(err)
	}
	limitChanges := make(map[string]audit.Change)
	for _, event := range keyEvents {
		if event.Action == "update_key" {
			for _, field := range []string{"daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd"} {
				if change, ok := event.Changes[field]; ok {
					limitChanges[field] = change
				}
			}
		}
	}
	for field, want := range map[string]audit.Change{
		"daily_limit_usd":   {From: float64(1), To: float64(2)},
		"weekly_limit_usd":  {From: float64(5), To: float64(6)},
		"monthly_limit_usd": {From: float64(10), To: float64(20)},
	} {
		if got, ok := limitChanges[field]; !ok || got != want {
			t.Errorf("%s audit change = %#v, want %#v", field, got, want)
		}
	}
}

func TestAuditWriteFailureDoesNotFailManagementOperation(t *testing.T) {
	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogWriter)
	dir := t.TempDir()
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(dir, "state.json"), Models: []ModelDefinition{freeTestModel("fast", "codex", "m")}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "cpa-key-policy-audit.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	hash, err := HashKey("cpa_audit_failure")
	if err != nil {
		t.Fatal(err)
	}
	err = store.UpsertKey(KeyConfig{ID: "still-created", Enabled: true, KeyHash: hash, Models: modelRefs("fast")}, true)
	if err != nil {
		t.Fatalf("audit failure escaped management operation: %v", err)
	}
	if store.findByID("still-created") == nil {
		t.Fatal("successful management operation was rolled back")
	}
	if !strings.Contains(logs.String(), "audit write failed") {
		t.Fatalf("audit failure was not logged: %q", logs.String())
	}
}
