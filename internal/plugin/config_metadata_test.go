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

// An unknown top-level key no longer fails registration, which would drop
// every capability; it is reported instead. The migration checker's strict
// parse still rejects it before an upgrade.
func TestRegisterWarnsAboutUnknownKeysAndCheckerRejectsThem(t *testing.T) {
	app := NewApp()
	defer app.Shutdown()

	config := []byte(`
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
store:
  id: cpa-key-policy
priority: 10
unexpected_routes: []
`)
	request, err := json.Marshal(LifecycleRequest{ConfigYAML: config, SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.HandleMethod(MethodPluginRegister, request); err != nil {
		t.Fatalf("register with an unknown key: %v", err)
	}
	if warnings := strings.Join(app.configWarnings(), "\n"); !strings.Contains(warnings, "unexpected_routes") {
		t.Fatalf("warnings = %q", warnings)
	}
	if _, err := ParseConfig(config); err == nil || !strings.Contains(err.Error(), "unexpected_routes") {
		t.Fatalf("strict parse error = %v", err)
	}
}
