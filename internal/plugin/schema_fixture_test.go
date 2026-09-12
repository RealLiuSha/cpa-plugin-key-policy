package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	policyPersist "cpa-key-policy/internal/policy/persist"
)

func TestManagementSchemaV3FixtureLoadsWithoutInventingCacheWrite(t *testing.T) {
	fixtureDir := filepath.Join("..", "policy", "testdata", "schema-v3")
	stateRaw, err := os.ReadFile(filepath.Join(fixtureDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	usageRaw, err := os.ReadFile(filepath.Join(fixtureDir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	if err := os.WriteFile(statePath, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPersist.UsagePath(statePath), usageRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.Store().SetClock(func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) })
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + filepath.ToSlash(statePath) + "\"\nusage_timezone: Asia/Shanghai\n"), SchemaVersion: SchemaVersion})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}
	models := app.store.ModelsSnapshot()
	if len(models) != 1 || models[0].CacheWritePricePerMillion != nil {
		t.Fatalf("v3 models invented cache-write: %+v", models)
	}
}

func TestManagementMigratesV4ToV5RuntimeResponses(t *testing.T) {
	fixtureDir := filepath.Join("..", "policy", "testdata", "schema-v4")
	stateRaw, err := os.ReadFile(filepath.Join(fixtureDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	usageRaw, err := os.ReadFile(filepath.Join(fixtureDir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	if err := os.WriteFile(statePath, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPersist.UsagePath(statePath), usageRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.Store().SetClock(func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) })
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + filepath.ToSlash(statePath) + "\"\nusage_timezone: Asia/Shanghai\n"), SchemaVersion: SchemaVersion})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}

	keysResponse := managementCall(t, app, "GET", "/v0/management/plugins/cpa-key-policy/keys", nil)
	modelsResponse := managementCall(t, app, "GET", "/v0/management/plugins/cpa-key-policy/models", nil)
	usageRequest, _ := json.Marshal(ManagementRequest{
		Method: "GET",
		Path:   "/v0/management/plugins/cpa-key-policy/keys/usage",
		Query:  map[string][]string{"id": {"team-a"}},
	})
	usageResponse := managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, usageRequest))
	if keysResponse.StatusCode != 200 || modelsResponse.StatusCode != 200 || usageResponse.StatusCode != 200 {
		t.Fatalf("schema responses status: keys=%d models=%d usage=%d", keysResponse.StatusCode, modelsResponse.StatusCode, usageResponse.StatusCode)
	}

	var keysEnvelope, modelsEnvelope map[string]any
	var keyUsage any
	if err := json.Unmarshal(keysResponse.Body, &keysEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(modelsResponse.Body, &modelsEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(usageResponse.Body, &keyUsage); err != nil {
		t.Fatal(err)
	}
	actual := map[string]any{
		"keys":      keysEnvelope["keys"],
		"models":    modelsEnvelope["models"],
		"key_usage": keyUsage,
	}
	wantRaw, err := os.ReadFile(filepath.Join("..", "policy", "testdata", "schema-v5", "management.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want) {
		actualRaw, _ := json.MarshalIndent(actual, "", "  ")
		t.Fatalf("management v5 fixture drifted; actual response composite:\n%s", actualRaw)
	}
}
