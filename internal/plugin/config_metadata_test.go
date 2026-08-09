package plugin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterAcceptsHostConfigMetadata(t *testing.T) {
	app := NewApp()
	defer app.Shutdown()

	request, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
store:
  id: cpa-key-policy
  name: CPA Key Policy
  install:
    type: local
priority: 10
`), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.HandleMethod(MethodPluginRegister, request); err != nil {
		t.Fatalf("register with host metadata: %v", err)
	}
}

func TestRegisterStillRejectsUnknownPolicyFields(t *testing.T) {
	app := NewApp()
	defer app.Shutdown()

	request, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte(`
store:
  id: cpa-key-policy
priority: 10
unexpected_routes: []
`), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}

	_, err = app.HandleMethod(MethodPluginRegister, request)
	if err == nil || !strings.Contains(err.Error(), "unexpected_routes") {
		t.Fatalf("unknown policy field error = %v", err)
	}
}
