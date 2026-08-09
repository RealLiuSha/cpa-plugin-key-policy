package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestBuildCatalogManagementRoute(t *testing.T) {
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

	body, _ := json.Marshal(map[string]any{
		"credentials": []map[string]any{
			{
				"id":         "vip-user.json",
				"provider":   "codex",
				"attributes": map[string]string{"plan_type": "free"},
				"models":     []string{"gpt-5.4-mini", "gpt-5.5"},
			},
			{
				"id":         "plain-free.json",
				"provider":   "codex",
				"attributes": map[string]string{"plan_type": "free"},
				"models":     []string{"gpt-5.4-mini"},
			},
			{
				"id":       "claude-1.json",
				"provider": "claude",
				"models":   []string{"claude-sonnet"},
			},
		},
	})
	mgmtReq, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/catalog",
		Body:   body,
	})
	raw, err := app.HandleMethod(MethodManagementHandle, mgmtReq)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(resp.Body))
	}
	var out struct {
		Entries []struct {
			Provider string   `json:"provider"`
			Group    string   `json:"group"`
			Models   []string `json:"models"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	byKey := map[string][]string{}
	for _, e := range out.Entries {
		byKey[e.Provider+"|"+e.Group] = e.Models
	}
	if _, ok := byKey["codex|classify:vip"]; !ok {
		t.Fatalf("missing classify:vip entry: %+v", out.Entries)
	}
	if _, ok := byKey["codex|free"]; !ok {
		t.Fatalf("missing built-in free entry: %+v", out.Entries)
	}
	if models, ok := byKey["claude|"]; !ok || len(models) != 1 {
		t.Fatalf("expected flat claude entry, got %+v", out.Entries)
	}
}

func TestBuildCatalogRegisteredInManagement(t *testing.T) {
	app := NewApp()
	reg := app.managementRegistration()
	found := false
	for _, r := range reg.Routes {
		if r.Path == "/plugins/cpa-key-policy/catalog" && r.Method == http.MethodPost {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("catalog route not registered: %+v", reg.Routes)
	}
}
