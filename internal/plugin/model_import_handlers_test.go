package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/pricingmetadata"
)

func TestManagementImportCreatesZeroPricedModels(t *testing.T) {
	app, _ := configureTestApp(t)
	response := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import", []byte(`{"items":[{"provider":"openai","target_model":"gpt-4o"},{"provider":"codex","target_model":"gpt-5-codex","name":"fast"}]}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("import status=%d body=%s", response.StatusCode, response.Body)
	}
	var result policy.ModelImportResult
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "gpt-4o" || len(result.Skipped) != 1 || result.Skipped[0].Reason != "exists" {
		t.Fatalf("import result = %+v", result)
	}
	for _, model := range app.store.ModelsSnapshot() {
		if model.Name == "gpt-4o" && (model.Provider != "openai" || model.InputPricePerMillion != 0) {
			t.Fatalf("imported model = %+v", model)
		}
	}
	invalid := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import", []byte(`{"items":[{"target_model":"m"}]}`))
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("import without provider status=%d body=%s", invalid.StatusCode, invalid.Body)
	}
}

func TestManagementPricingPreviewUsesModelsDev(t *testing.T) {
	app := NewApp()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + statePath + "\"\n"), SchemaVersion: SchemaVersion})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-4o":{"id":"gpt-4o","cost":{"input":2.5,"output":10,"cache_read":1.25,"cache_write":3.75}}}}}`))
	}))
	t.Cleanup(server.Close)
	app.SetPricingClient(pricingmetadata.NewClientForTest(&http.Client{Timeout: 2 * time.Second}, server.URL))
	response := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/pricing-preview", []byte(`{"models":["cpa/gpt-4o"]}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", response.StatusCode, response.Body)
	}
	var preview pricingmetadata.Preview
	if err := json.Unmarshal(response.Body, &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Matches) != 1 || preview.Matches[0].SourceProviderID != "openai" || preview.Matches[0].CacheWritePricePer1M == nil || *preview.Matches[0].CacheWritePricePer1M != 3.75 {
		t.Fatalf("preview = %+v", preview)
	}
}
