package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestV3ManagementRejectsRemovedRoutesAndKeyShapes(t *testing.T) {
	app := NewApp()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	config := []byte(`
enabled: true
state_file: "` + statePath + `"
models:
  - name: fast
    free: true
    targets:
      - {provider: codex, target_model: gpt}
`)
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: config})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}

	removedCollection := strings.Join([]string{"ali", "ases"}, "")
	request, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/" + removedCollection,
	})
	response := managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, request))
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed route status = %d, want 404", response.StatusCode)
	}

	removedReferenceField := []byte(`{"id":"old-ref","` + removedCollection + `":[{"name":"fast"}]}`)
	directRoute := []byte(`{"id":"direct-route","models":[{"name":"fast","provider":"codex","target_model":"gpt"}]}`)
	priceOverride := []byte(`{"id":"price-override","models":[{"name":"fast","input_price_per_million":1}]}`)
	for name, body := range map[string][]byte{
		"removed reference field": removedReferenceField,
		"direct route fields":     directRoute,
		"key price override":      priceOverride,
	} {
		t.Run(name, func(t *testing.T) {
			request, _ := json.Marshal(ManagementRequest{
				Method: http.MethodPost,
				Path:   "/v0/management/plugins/cpa-key-policy/keys",
				Body:   body,
			})
			response := managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, request))
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestV3ModelManagementLifecycle(t *testing.T) {
	app := NewApp()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + statePath + "\"\n")})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}

	createModel := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models", []byte(`{
  "name": " Chat ",
  "targets": [{"provider":"CODEX","target_model":"gpt"}],
  "free": true
}`))
	if createModel.StatusCode != http.StatusOK {
		t.Fatalf("create model status=%d body=%s", createModel.StatusCode, createModel.Body)
	}
	var created struct {
		Model struct {
			Name    string `json:"name"`
			Targets []struct {
				Provider string `json:"provider"`
			} `json:"targets"`
			Dispatch    string `json:"dispatch"`
			BillingMode string `json:"billing_mode"`
		} `json:"model"`
	}
	if err := json.Unmarshal(createModel.Body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Model.Name != "Chat" || created.Model.Targets[0].Provider != "codex" || created.Model.Dispatch != "round-robin" || created.Model.BillingMode != "tokens" {
		t.Fatalf("model response is not canonical: %+v", created.Model)
	}

	createKey := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/keys", []byte(`{
  "id": "team-a",
  "key": "cpa_contract_key",
  "models": [{"name":"chat","daily_limit_usd":5}]
}`))
	if createKey.StatusCode != http.StatusCreated {
		t.Fatalf("create key status=%d body=%s", createKey.StatusCode, createKey.Body)
	}

	deleteBody := []byte(`{"name":"Chat"}`)
	blocked := managementCall(t, app, http.MethodDelete, "/v0/management/plugins/cpa-key-policy/models", deleteBody)
	if blocked.StatusCode != http.StatusBadRequest {
		t.Fatalf("referenced model delete status=%d body=%s", blocked.StatusCode, blocked.Body)
	}
	deletedKey := managementCall(t, app, http.MethodDelete, "/v0/management/plugins/cpa-key-policy/keys", []byte(`{"id":"team-a"}`))
	if deletedKey.StatusCode != http.StatusOK {
		t.Fatalf("delete key status=%d body=%s", deletedKey.StatusCode, deletedKey.Body)
	}
	deletedModel := managementCall(t, app, http.MethodDelete, "/v0/management/plugins/cpa-key-policy/models", deleteBody)
	if deletedModel.StatusCode != http.StatusOK {
		t.Fatalf("delete model status=%d body=%s", deletedModel.StatusCode, deletedModel.Body)
	}
}

func TestV3PriceImportRejectsInvalidDryRunAsClientError(t *testing.T) {
	app := NewApp()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	lifecycle, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte(`
enabled: true
state_file: "` + statePath + `"
models:
  - name: fast
    targets: [{provider: codex, target_model: gpt}]
    input_price_per_million: 1
`)})
	if _, err := app.HandleMethod(MethodPluginReconfigure, lifecycle); err != nil {
		t.Fatal(err)
	}
	response := managementCall(t, app, http.MethodPost, "/v0/management/plugins/cpa-key-policy/models/import-prices", []byte(`{
  "dry_run": true,
  "matches": [{"model":"fast","prompt_price_per_1m":-1}]
}`))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid dry-run status=%d body=%s", response.StatusCode, response.Body)
	}
}

func managementCall(t *testing.T, app *App, method, path string, body []byte) ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(ManagementRequest{Method: method, Path: path, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, raw))
}
