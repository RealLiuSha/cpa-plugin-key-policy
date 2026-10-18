package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
)

const (
	managementBase = "/v0/management/plugins/" + PluginID
	testKeyID      = "priced"
	testKeyPlain   = "cpa_priced"
)

// testClock is a fixed instant that differs from the wall clock, so a date
// that leaks from time.Now() shows up in assertions.
func testClock(t *testing.T) func() time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, loc)
	return func() time.Time { return now }
}

type appOptions struct {
	RPM           int
	DailyLimitUSD float64
	// Clock replaces the store clock before registration; nil keeps time.Now.
	Clock func() time.Time
}

// newTestApp registers the plugin with one key ("priced", secret cpa_priced)
// and three price rows: gpt-5-codex (tokens, $1/M in and out, provider
// codex), grok-imagine-image (per_call $0.07, provider xai) and free-model,
// an all-zero row.
func newTestApp(t *testing.T, opts appOptions) *App {
	t.Helper()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	config := fmt.Sprintf(`
enabled: true
state_file: %q
usage_timezone: Asia/Shanghai
models:
  - name: gpt-5-codex
    provider: codex
    billing_mode: tokens
    input_price_per_million: 1
    output_price_per_million: 1
  - name: grok-imagine-image
    provider: xai
    billing_mode: per_call
    per_call_usd: 0.07
  - name: free-model
keys:
  - id: %s
    name: Priced
    enabled: true
    key_hash: %q
    key_preview: "cpa_pr...ced"
    rpm: %d
    daily_limit_usd: %g
`, statePath, testKeyID, hashForTest(t, testKeyPlain), opts.RPM, opts.DailyLimitUSD)
	app := NewApp()
	if opts.Clock != nil {
		app.Store().SetClock(opts.Clock)
	}
	register(t, app, config)
	t.Cleanup(app.Shutdown)
	return app
}

func register(t *testing.T, app *App, configYAML string) {
	t.Helper()
	raw, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte(configYAML), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.HandleMethod(MethodPluginRegister, raw); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func hashForTest(t *testing.T, key string) string {
	t.Helper()
	hash, err := policy.HashKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func bearer(plain string) http.Header {
	return http.Header{"Authorization": {"Bearer " + plain}}
}

// handle sends one host->plugin call and returns the envelope result,
// failing on a transport error or an error envelope.
func handle(t *testing.T, app *App, method string, payload any) json.RawMessage {
	t.Helper()
	raw, ok := payload.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	response, err := app.HandleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var env Envelope
	if err := json.Unmarshal(response, &env); err != nil {
		t.Fatalf("%s envelope: %v", method, err)
	}
	if !env.OK {
		t.Fatalf("%s returned an error envelope: %+v", method, env.Error)
	}
	return env.Result
}

func decode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, &target); err != nil {
		t.Fatalf("decode %T: %v\n%s", target, err, raw)
	}
}

func authenticate(t *testing.T, app *App, method, path string, headers http.Header, query url.Values) FrontendAuthResponse {
	t.Helper()
	// The host sends the whole pluginapi.FrontendAuthRequest, body included.
	var response FrontendAuthResponse
	decode(t, handle(t, app, MethodFrontendAuthAuthenticate, map[string]any{
		"Method": method, "Path": path, "Headers": headers, "Query": query, "Body": []byte(`{"model":"gpt-5-codex"}`),
	}), &response)
	return response
}

// interceptBefore sends request.intercept_before in CPA's wire shape (Go
// field names, base64 body, host_callback_id) and decodes the answer.
func interceptBefore(t *testing.T, app *App, traceID, sourceFormat string, headers http.Header, requestPath string) RequestInterceptResponse {
	t.Helper()
	var response RequestInterceptResponse
	decode(t, handle(t, app, MethodRequestInterceptBefore, map[string]any{
		"RequestID": "req-" + traceID, "TraceID": traceID, "SourceFormat": sourceFormat, "ToFormat": "",
		"Model": "gpt-5-codex", "RequestedModel": "gpt-5-codex", "Stream": false,
		"Headers": headers, "Body": []byte(`{"model":"gpt-5-codex"}`),
		"Metadata":         map[string]any{"request_path": requestPath, "requested_model": "gpt-5-codex"},
		"host_callback_id": "cb-1",
	}), &response)
	return response
}

func recordUsage(t *testing.T, app *App, record UsageHandleRequest) {
	t.Helper()
	handle(t, app, MethodUsageHandle, record)
}

// hostUsageRecord is a pluginapi.UsageRecord exactly as CPA v8.0.25 marshals
// it for usage.handle: Go field names, durations as int64 nanoseconds.
func hostUsageRecord(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "usage-record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func managementCall(t *testing.T, app *App, method, path string, query url.Values, body []byte) ManagementResponse {
	t.Helper()
	var response ManagementResponse
	decode(t, handle(t, app, MethodManagementHandle, ManagementRequest{Method: method, Path: managementBase + path, Query: query, Body: body}), &response)
	return response
}

func managementGet(t *testing.T, app *App, path string, query url.Values) ManagementResponse {
	t.Helper()
	return managementCall(t, app, http.MethodGet, path, query, nil)
}

func managementOK(t *testing.T, app *App, method, path string, query url.Values, body []byte) []byte {
	t.Helper()
	response := managementCall(t, app, method, path, query, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s %s status=%d body=%s", method, path, response.StatusCode, response.Body)
	}
	return response.Body
}

func errorCode(t *testing.T, response ManagementResponse) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, response.Body, &body)
	return body.Error.Code
}

func objectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var object map[string]json.RawMessage
	decode(t, raw, &object)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// assertShape checks that every required key is present and no key outside
// required ∪ optional appears.
func assertShape(t *testing.T, label string, raw json.RawMessage, required, optional []string) {
	t.Helper()
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	present := make(map[string]bool)
	for _, key := range objectKeys(t, raw) {
		present[key] = true
		if !allowed[key] {
			t.Errorf("%s: unexpected field %q in %s", label, key, raw)
		}
	}
	for _, key := range required {
		if !present[key] {
			t.Errorf("%s: missing field %q in %s", label, key, raw)
		}
	}
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
