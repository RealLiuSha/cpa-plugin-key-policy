package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	nativeSearching = "aaaaaaaaaaaaaaaa"
	nativeDisabled  = "bbbbbbbbbbbbbbbb"
)

// newNativeApp registers the plugin over a native search file in which
// gpt-6-sol is a catalog model, channel a searched an hour ago and channel b
// is disabled by the operator.
func newNativeApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC()
	file := fmt.Sprintf(`{"version":1,
 "settings":{"enabled":true,"require_all":false,"excluded_models":[],"disabled_channels":[%q],"interval_hours":12},
 "catalog":[{"slug":"gpt-6-sol","seen_at":%q}],
 "credentials":[{"auth_index":%q,"id":"codex:apikey:a","provider":"codex","name":"a","models":["gpt-6-sol"]},
  {"auth_index":%q,"id":"codex:apikey:b","provider":"codex","name":"b","models":["gpt-6-sol"]}],
 "channels":[{"auth_index":%q,"probes":[{"at":%q,"model":"gpt-6-sol","result":"search","latency_ms":900}]}]}`,
		nativeDisabled, now.Format(time.RFC3339Nano), nativeSearching, nativeDisabled, nativeSearching, now.Add(-time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, nativeFile), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	return newTestApp(t, appOptions{Dir: dir})
}

func interceptResponseBody(t *testing.T, app *App, payload map[string]any) []byte {
	t.Helper()
	var response ResponseInterceptResponse
	decode(t, handle(t, app, MethodResponseInterceptAfter, payload), &response)
	return response.Body
}

func attemptBody(t *testing.T, app *App, authIndex string, body []byte) []byte {
	t.Helper()
	var response RequestInterceptResponse
	decode(t, handle(t, app, MethodRequestInterceptAfter, map[string]any{
		"RequestID": "req-1", "TraceID": "a1b2c3d4", "SourceFormat": "openai-response", "ToFormat": "codex",
		"Model": "gpt-6-sol", "RequestedModel": "gpt-6-sol", "Stream": true, "Headers": http.Header{}, "Body": body,
		"Metadata": map[string]any{"request_path": "/v1/responses", "selected_auth_index": authIndex},
	}), &response)
	return response.Body
}

func TestNativeSearchRewritesTheCatalogAndStripsDisabledChannels(t *testing.T) {
	app := newNativeApp(t)
	if registration := reconfigure(t, app, ""); registration.Capabilities.ResponseInterceptor {
		t.Fatal("response_interceptor declared while search is off")
	}
	if registration := reconfigure(t, app, "search_enabled: true\n"); !registration.Capabilities.ResponseInterceptor || !registration.Capabilities.ModelRouter {
		t.Fatalf("capabilities with search on: %+v", registration.Capabilities)
	}

	catalog := []byte(`{"models":[{"slug":"gpt-6-sol","use_responses_lite":true,"visibility":"list"},{"slug":"gpt-6-astra","use_responses_lite":true,"visibility":"list"}]}`)
	// The host's member order: the catalog has no model and comes as a model list.
	rewritten := interceptResponseBody(t, app, map[string]any{"RequestID": "r1", "SourceFormat": "openai", "Model": "", "RequestedModel": "", "Stream": false,
		"RequestHeaders": http.Header{}, "ResponseHeaders": http.Header{}, "OriginalRequest": nil, "RequestBody": nil, "Body": catalog, "StatusCode": 200, "Metadata": nil})
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil || out.Models[0]["use_responses_lite"] != false || out.Models[1]["use_responses_lite"] != true {
		t.Fatalf("catalog = %s (%v)", rewritten, err)
	}
	if body := interceptResponseBody(t, app, map[string]any{"RequestID": "r2", "SourceFormat": "openai-image", "Model": "gpt-image-2", "RequestedModel": "gpt-image-2",
		"Body": catalog, "StatusCode": 200}); body != nil {
		t.Fatalf("a model response was rewritten: %s", body)
	}

	request := []byte(`{"model":"gpt-6-sol","input":[],"tools":[{"type":"custom","name":"exec"},{"type":"web_search","external_web_access":false}]}`)
	if body := attemptBody(t, app, nativeSearching, request); body != nil {
		t.Fatalf("a searching channel lost its tool: %s", body)
	}
	if body := attemptBody(t, app, nativeDisabled, request); body == nil || strings.Contains(string(body), "web_search") || !strings.Contains(string(body), `"exec"`) {
		t.Fatalf("disabled channel body = %s", body)
	}

	// The searching channel refuses the tool once: its sessions stop getting it.
	recordUsage(t, app, UsageHandleRequest{AuthIndex: nativeSearching, AuthID: "codex:apikey:a", Provider: "codex", Model: "gpt-6-sol",
		Failed: true, Failure: UsageFailure{StatusCode: http.StatusBadRequest, Body: `{"error":{"message":"Unsupported tool type: web_search"}}`}})
	if body := attemptBody(t, app, nativeSearching, request); body == nil || strings.Contains(string(body), "web_search") {
		t.Fatalf("refusing channel body = %s", body)
	}
	var state struct {
		Native struct {
			Declared bool `json:"declared"`
			Models   []struct {
				Model string `json:"model"`
				Mode  string `json:"mode"`
			} `json:"models"`
			Channels []struct {
				AuthIndex string `json:"auth_index"`
				State     string `json:"state"`
			} `json:"channels"`
		} `json:"native"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/search", nil, nil), &state)
	// The catalog brought gpt-6-astra in, which no channel serves; gpt-6-sol
	// lost its only searching channel.
	modes := map[string]string{}
	for _, model := range state.Native.Models {
		modes[model.Model] = model.Mode
	}
	if !state.Native.Declared || modes["gpt-6-sol"] != "fallback" || modes["gpt-6-astra"] != "fallback" || state.Native.Channels[0].State != "rejected" {
		t.Fatalf("native state = %+v", state.Native)
	}
}

func TestNativeSearchSettingsAreValidatedAndAudited(t *testing.T) {
	app := newNativeApp(t)
	if response := managementCall(t, app, http.MethodPut, "/search/native", nil, []byte(`{"settings":{"enabled":true,"interval_hours":500}}`)); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("interval 500 = %d %s", response.StatusCode, response.Body)
	}
	if response := managementCall(t, app, http.MethodPut, "/search/native", nil, []byte(`{"settings":{"enabled":true},"extra":1}`)); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", response.StatusCode, response.Body)
	}
	body := managementOK(t, app, http.MethodPut, "/search/native", nil, []byte(`{"settings":{"enabled":true,"require_all":true,"excluded_models":[" gpt-6-sol "],"disabled_channels":["BBBBBBBBBBBBBBBB"],"interval_hours":6}}`))
	var stored struct {
		Settings struct {
			RequireAll       bool     `json:"require_all"`
			ExcludedModels   []string `json:"excluded_models"`
			DisabledChannels []string `json:"disabled_channels"`
			IntervalHours    int      `json:"interval_hours"`
		} `json:"settings"`
		Models []struct {
			Mode string `json:"mode"`
		} `json:"models"`
	}
	decode(t, body, &stored)
	if !stored.Settings.RequireAll || stored.Settings.IntervalHours != 6 || len(stored.Settings.ExcludedModels) != 1 || stored.Settings.ExcludedModels[0] != "gpt-6-sol" ||
		len(stored.Settings.DisabledChannels) != 1 || stored.Settings.DisabledChannels[0] != nativeDisabled || stored.Models[0].Mode != "excluded" {
		t.Fatalf("stored = %s", body)
	}
	if audit := managementOK(t, app, http.MethodGet, "/audit", nil, nil); !strings.Contains(string(audit), "update_native_search") {
		t.Fatalf("audit = %s", audit)
	}
	// No round runs while the search module is off; outside CPA there are no
	// credentials to probe.
	if response := managementCall(t, app, http.MethodPost, "/search/native/probe", nil, []byte(`{}`)); response.StatusCode != http.StatusConflict || !strings.Contains(string(response.Body), "search_off") {
		t.Fatalf("probe with search off = %d %s", response.StatusCode, response.Body)
	}
	reconfigure(t, app, "search_enabled: true\n")
	if response := managementCall(t, app, http.MethodPost, "/search/native/probe", nil, []byte(`{}`)); response.StatusCode != http.StatusBadGateway {
		t.Fatalf("probe without CPA = %d %s", response.StatusCode, response.Body)
	}
}

// Switching the search module off while a native search probe waits on CPA to
// open its stream: the reconfigure, which CPA applies no configuration until,
// does not wait for the probe, and the cancelled probe records nothing.
func TestSearchOffDoesNotWaitForANativeProbe(t *testing.T) {
	app := newTestApp(t, appOptions{})
	host := openingHost{detectionHost: newDetectionHost(), opening: make(chan struct{}, 1), open: make(chan struct{})}
	close(host.release)
	app.SetHost(host)
	cpaConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cpaConfig, []byte("codex-api-key: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "enabled: true\nstate_file: " + app.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\ncpa_config_path: " + cpaConfig + "\n"
	handle(t, app, MethodPluginReconfigure, LifecycleRequest{ConfigYAML: []byte(config + "search_enabled: true\n"), SchemaVersion: SchemaVersion})
	interceptResponseBody(t, app, map[string]any{"SourceFormat": "openai", "Model": "", "StatusCode": 200,
		"Body": []byte(`{"models":[{"slug":"gpt-6-sol","use_responses_lite":true,"visibility":"list"}]}`)})
	if response := managementCall(t, app, http.MethodPost, "/search/native/probe", nil, []byte(`{}`)); response.StatusCode != http.StatusAccepted {
		t.Fatalf("probe = %d %s", response.StatusCode, response.Body)
	}
	if audit := managementOK(t, app, http.MethodGet, "/audit", nil, nil); !strings.Contains(string(audit), "probe_native_search") {
		t.Fatalf("audit = %s", audit)
	}
	select {
	case <-host.opening:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never opened its stream")
	}

	request := lifecycleRequest(t, config)
	reconfigured := make(chan error, 1)
	go func() {
		_, err := app.HandleMethod(MethodPluginReconfigure, request)
		reconfigured <- err
	}()
	select {
	case err := <-reconfigured:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(host.open)
		<-reconfigured
		t.Fatal("switching search off waited for a probe CPA is still opening a stream for")
	}
	close(host.open)
	set := app.modules.Load()
	deadline := time.Now().Add(5 * time.Second)
	for set.native.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	status := set.native.Status()
	if status.Round != nil || status.Running || len(status.Channels) != 1 || len(status.Channels[0].Probes) != 0 {
		t.Fatalf("after search off: %+v", status)
	}
}
