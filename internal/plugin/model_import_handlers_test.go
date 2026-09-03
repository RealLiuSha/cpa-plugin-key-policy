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

func TestManagementImportModelsDryRunThenApply(t *testing.T) {
	app, _ := configureTestApp(t)
	body := []byte(`{"dry_run":true,"items":[{"name":"gpt-4o","targets":[{"provider":"openai","target_model":"gpt-4o"}],"input_price_per_million":5,"output_price_per_million":15}]}`)
	preview := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import", body)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("dry-run status=%d body=%s", preview.StatusCode, preview.Body)
	}
	var result policy.ModelImportResult
	if err := json.Unmarshal(preview.Body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("dry-run created = %+v", result)
	}
	for _, model := range app.store.ModelsSnapshot() {
		if model.Name == "gpt-4o" {
			t.Fatal("dry-run published gpt-4o")
		}
	}
	apply := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import", []byte(`{"dry_run":false,"items":[{"name":"gpt-4o","targets":[{"provider":"openai","target_model":"gpt-4o"}],"input_price_per_million":5,"output_price_per_million":15}]}`))
	if apply.StatusCode != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", apply.StatusCode, apply.Body)
	}
	found := false
	for _, model := range app.store.ModelsSnapshot() {
		if model.Name == "gpt-4o" {
			found = true
		}
	}
	if !found {
		t.Fatalf("apply did not persist gpt-4o: %+v", app.store.ModelsSnapshot())
	}
}

func TestManagementImportModelsRejectsMissingPriceOnApply(t *testing.T) {
	app, _ := configureTestApp(t)
	response := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import", []byte(`{"items":[{"name":"paid","targets":[{"provider":"openai","target_model":"m"}],"missing_price":true}]}`))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing price apply status=%d body=%s", response.StatusCode, response.Body)
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
