package plugin

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
)

func TestNewAppDoesNotWriteStateBeforeConfigure(t *testing.T) {
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	app := NewApp()
	if app == nil || app.store == nil {
		t.Fatal("NewApp returned an incomplete app")
	}
	if _, err := os.Stat(policy.DefaultConfig().StateFile); !os.IsNotExist(err) {
		t.Fatalf("NewApp wrote default state before lifecycle configure: %v", err)
	}
}

func TestRegistrationDeclaresRequestPathCapabilitiesOnly(t *testing.T) {
	app := newTestApp(t, appOptions{})
	var registration struct {
		SchemaVersion uint32                     `json:"schema_version"`
		Metadata      Metadata                   `json:"metadata"`
		Capabilities  map[string]json.RawMessage `json:"capabilities"`
	}
	raw, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + filepath.ToSlash(app.store.StatePath()) + "\"\n"), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	decode(t, handle(t, app, MethodPluginReconfigure, raw), &registration)
	if registration.SchemaVersion != SchemaVersion || registration.Metadata.Version != Version || registration.Metadata.GitHubRepository != "https://github.com/RealLiuSha/cpa-plugin-key-policy" {
		t.Fatalf("registration metadata = %+v", registration.Metadata)
	}
	for _, capability := range []string{"frontend_auth_provider", "request_interceptor", "usage_plugin", "management_api"} {
		if string(registration.Capabilities[capability]) != "true" {
			t.Fatalf("capability %s = %s", capability, registration.Capabilities[capability])
		}
	}
	// model_router is declared only while search_enabled is set.
	for _, removed := range []string{"model_router", "response_interceptor"} {
		if _, declared := registration.Capabilities[removed]; declared {
			t.Fatalf("capability %s must not be declared: %v", removed, registration.Capabilities)
		}
	}
	var fields []string
	for _, field := range registration.Metadata.ConfigFields {
		fields = append(fields, field.Name)
		if strings.Contains(field.Description, "target_model") {
			t.Fatalf("config field %s still describes routing: %q", field.Name, field.Description)
		}
	}
	if strings.Join(fields, ",") != "enabled,state_file,usage_timezone,keys,models,search_enabled,cpa_config_path" {
		t.Fatalf("config fields = %v", fields)
	}
}

func TestRemovedHostMethodsAreUnknown(t *testing.T) {
	app := newTestApp(t, appOptions{})
	raw, err := app.HandleMethod("response.intercept_after", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	decode(t, raw, &env)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("response.intercept_after envelope = %+v", env)
	}
	// model.route leaves every request to CPA while search is off.
	if route := handle(t, app, MethodModelRoute, map[string]any{"SourceFormat": "codex-alpha-search", "RequestedModel": "claude-x"}); string(route) != `{"Handled":false}` {
		t.Fatalf("model.route with search off = %s", route)
	}
	if raw := handle(t, app, MethodRequestInterceptAfter, map[string]any{"Headers": bearer(testKeyPlain)}); string(raw) != "{}" {
		t.Fatalf("intercept_after must pass requests through unchanged: %s", raw)
	}
}

func TestLifecycleAcceptsNewerHostSchema(t *testing.T) {
	// The host sends the highest lifecycle version it supports and honors
	// whatever the plugin reports back at plugin.register.
	for _, hostSchema := range []uint32{MinHostSchemaVersion, SchemaVersion + 1, SchemaVersion + 2} {
		app := NewApp()
		statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
		raw, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: \"" + statePath + "\"\n"), SchemaVersion: hostSchema})
		response, err := app.HandleMethod(MethodPluginRegister, raw)
		if err != nil {
			t.Fatalf("host schema_version %d was rejected: %v", hostSchema, err)
		}
		// The plugin answers the lower version, so an older host keeps its own
		// contract and a newer one gets raw management responses.
		var env struct {
			Result Registration `json:"result"`
		}
		decode(t, response, &env)
		if want := min(hostSchema, SchemaVersion); env.Result.SchemaVersion != want {
			t.Fatalf("host schema %d answered with %d, want %d", hostSchema, env.Result.SchemaVersion, want)
		}
		app.Shutdown()
	}
}

func TestLifecycleRejectsOlderHostSchemaAndUnknownFields(t *testing.T) {
	app := NewApp()
	statePath := filepath.ToSlash(filepath.Join(t.TempDir(), "state.json"))
	older, _ := json.Marshal(map[string]any{"schema_version": MinHostSchemaVersion - 1, "config_yaml": []byte("enabled: true\nstate_file: \"" + statePath + "\"\n")})
	if _, err := app.HandleMethod(MethodPluginRegister, older); err == nil {
		t.Fatalf("host schema_version %d was accepted", MinHostSchemaVersion-1)
	}
	unknown, _ := json.Marshal(map[string]any{"schema_version": SchemaVersion, "config_yaml": []byte("enabled: true\nstate_file: \"" + statePath + "\"\n"), "unexpected": true})
	if _, err := app.HandleMethod(MethodPluginRegister, unknown); err == nil {
		t.Fatal("unknown lifecycle field was accepted")
	}
}

// A config.yaml written for earlier releases keeps registering: the removed
// key and model settings decode and are ignored, and a model seed names its
// price row by target_model.
// Settings of earlier releases are not part of the configuration any more:
// registration refuses them by name, while CPA's own catalog fields pass.
func TestConfigRejectsRemovedSettingsAndKeepsHostFields(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
enabled: true
state_file: "/data/state.json"
priority: 10
store:
  id: cpa-key-policy
  version: "0.12.0"
models:
  - name: grok-4.7
    provider: xai
    input_price_per_million: 2
`))
	if err != nil || cfg.StateFile != "/data/state.json" || len(cfg.Models) != 1 || cfg.Models[0].Name != "grok-4.7" {
		t.Fatalf("current configuration = %+v err=%v", cfg, err)
	}
	_, err = ParseConfig([]byte(`
enabled: true
classify_rules:
  - match: foo
models:
  - name: fast
    target_model: gpt-5-codex
keys:
  - id: legacy
    allow_models_endpoint: false
    models:
      - name: fast
`))
	if err == nil {
		t.Fatal("removed settings were accepted")
	}
	for _, field := range []string{"classify_rules", "target_model", "allow_models_endpoint", "field models"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name %s", err, field)
		}
	}
}

func TestAuthenticateIdentifiesKeyAndDefersAdmission(t *testing.T) {
	app := newTestApp(t, appOptions{})
	response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer(testKeyPlain), nil)
	if !response.Authenticated || response.Principal != testKeyID {
		t.Fatalf("auth response = %+v", response)
	}
	if len(response.Metadata) != 2 || response.Metadata["provider"] != PluginID || response.Metadata["key_id"] != testKeyID {
		t.Fatalf("auth metadata = %v, want provider and key_id only", response.Metadata)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer("cpa_unknown"), nil); response.Authenticated {
		t.Fatalf("unknown key authenticated: %+v", response)
	}
	if response := authenticate(t, app, http.MethodGet, "/v1/models", nil, url.Values{"key": {testKeyPlain}}); !response.Authenticated {
		t.Fatalf("model list with a query key denied: %+v", response)
	}
	if raw, err := json.Marshal(FrontendAuthResponse{Authenticated: false}); err != nil || string(raw) != `{"Authenticated":false}` {
		t.Fatalf("denied response wire shape = %s (%v)", raw, err)
	}
}

func TestAuthenticateRejectsQueryOnlyKeyOnMediaGeneration(t *testing.T) {
	app := newTestApp(t, appOptions{})
	query := url.Values{"key": {testKeyPlain}}
	if response := authenticate(t, app, http.MethodPost, "/v1/images/generations", nil, query); response.Authenticated {
		t.Fatalf("query-only key on a generation route must be refused: %+v", response)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/images/generations", bearer(testKeyPlain), nil); !response.Authenticated {
		t.Fatalf("header key on a generation route denied: %+v", response)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/embeddings", nil, query); !response.Authenticated {
		t.Fatalf("query key on a non-media route denied: %+v", response)
	}
}

func TestInterceptorEnforcesRPMWith429(t *testing.T) {
	app := newTestApp(t, appOptions{RPM: 60})
	headers := bearer(testKeyPlain)
	for i := 0; i < 61; i++ {
		if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", headers, nil); !response.Authenticated {
			t.Fatalf("authentication %d = %+v; RPM belongs to the interceptor", i, response)
		}
		response := interceptBefore(t, app, "trace-"+strconv.Itoa(i), "openai", headers, "/v1/chat/completions")
		if i < 60 {
			if response.Terminate {
				t.Fatalf("request %d within the limit was terminated: %+v", i, response)
			}
			continue
		}
		if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request over the limit = %+v, want 429", response)
		}
		if seconds, err := strconv.Atoi(response.ResponseHeaders.Get("Retry-After")); err != nil || seconds < 1 || seconds > 60 {
			t.Fatalf("Retry-After = %q", response.ResponseHeaders.Get("Retry-After"))
		}
		var body struct {
			Error struct{ Message, Type, Code string } `json:"error"`
		}
		decode(t, response.ResponseBody, &body)
		if body.Error.Code != "rate_limit_exceeded" || body.Error.Type != "rate_limit_error" || !strings.Contains(body.Error.Message, "limit is 60 RPM") {
			t.Fatalf("429 body = %s", response.ResponseBody)
		}
		if strings.Contains(string(response.ResponseBody), testKeyPlain) {
			t.Fatal("rejection leaked the plaintext key")
		}
	}
	managementOK(t, app, http.MethodPost, "/keys/reset-rpm", nil, []byte(`{"id":"priced"}`))
	if response := interceptBefore(t, app, "trace-after-reset", "openai", headers, "/v1/chat/completions"); response.Terminate {
		t.Fatalf("request after RPM reset was terminated: %+v", response)
	}
}

// 1M input tokens at $1/M meet the $1 daily limit: authentication still
// passes and the interceptor answers the next request with 429 in the
// caller's protocol, while the model list stays readable.
func TestUsageHandleBillsAndQuotaTerminatesWith429(t *testing.T) {
	app := newTestApp(t, appOptions{DailyLimitUSD: 1})
	headers := bearer(testKeyPlain)
	recordUsage(t, app, UsageHandleRequest{
		Provider: "codex", ExecutorType: "codexexecutor", Model: "gpt-5-codex", Alias: "fast", APIKey: testKeyID,
		Detail: UsageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", headers, nil); !response.Authenticated {
		t.Fatalf("quota must not turn into an authentication failure: %+v", response)
	}
	openAI := interceptBefore(t, app, "trace-openai", "openai", headers, "/v1/chat/completions")
	var openAIBody struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	decode(t, openAI.ResponseBody, &openAIBody)
	if !openAI.Terminate || openAI.StatusCode != http.StatusTooManyRequests || openAIBody.Error.Type != "insufficient_quota" || openAIBody.Error.Code != "insufficient_quota" || !strings.Contains(openAIBody.Error.Message, "Daily quota of $1.00") {
		t.Fatalf("OpenAI rejection = %+v body=%s", openAI, openAI.ResponseBody)
	}
	if openAI.ResponseHeaders.Get("Retry-After") == "" || openAI.ResponseHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("rejection headers = %v", openAI.ResponseHeaders)
	}
	responses := interceptBefore(t, app, "trace-responses", "openai-response", headers, "/v1/responses")
	var responsesBody struct {
		Error struct {
			Message, Type, Code string
			ResetsAt            int64 `json:"resets_at"`
		} `json:"error"`
	}
	decode(t, responses.ResponseBody, &responsesBody)
	if !responses.Terminate || responses.StatusCode != http.StatusTooManyRequests || responsesBody.Error.Type != "usage_limit_reached" || responsesBody.Error.Code != "insufficient_quota" ||
		responsesBody.Error.ResetsAt <= time.Now().Unix() || !strings.Contains(responsesBody.Error.Message, "Daily quota of $1.00") {
		t.Fatalf("Responses rejection = %+v body=%s", responses, responses.ResponseBody)
	}
	claude := interceptBefore(t, app, "trace-claude", "claude", headers, "/v1/messages")
	var claudeBody struct {
		Type  string                         `json:"type"`
		Error struct{ Type, Message string } `json:"error"`
	}
	decode(t, claude.ResponseBody, &claudeBody)
	if !claude.Terminate || claudeBody.Type != "error" || claudeBody.Error.Type != "rate_limit_error" || !strings.Contains(claudeBody.Error.Message, "resets at") {
		t.Fatalf("Claude rejection body = %s", claude.ResponseBody)
	}
	gemini := interceptBefore(t, app, "trace-gemini", "gemini", headers, "/v1beta/models/gemini-pro:generateContent")
	var geminiBody struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	decode(t, gemini.ResponseBody, &geminiBody)
	if !gemini.Terminate || geminiBody.Error.Code != http.StatusTooManyRequests || geminiBody.Error.Status != "RESOURCE_EXHAUSTED" {
		t.Fatalf("Gemini rejection body = %s", gemini.ResponseBody)
	}
	if response := authenticate(t, app, http.MethodGet, "/v1/models", headers, nil); !response.Authenticated {
		t.Fatalf("model list must stay readable when the quota is used up: %+v", response)
	}
	var keys struct {
		Keys []struct {
			Usage policy.UsageSummary `json:"usage"`
		} `json:"keys"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys", nil, nil), &keys)
	if len(keys.Keys) != 1 || keys.Keys[0].Usage.Status != "limited" || keys.Keys[0].Usage.BlockedReason != "daily_exceeded" || !nearly(keys.Keys[0].Usage.DailyUSD, 1) {
		t.Fatalf("key usage = %+v", keys.Keys)
	}
}

// The host's second model field names what the client asked for; it is the
// billed model only when the upstream model id is missing.
func TestUsageHandleFallsBackToRequestedModelName(t *testing.T) {
	app := newTestApp(t, appOptions{})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Alias: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 300_000}})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", Alias: "fast", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000}})
	var usage struct {
		Models []policy.ModelUsageEntry `json:"models"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys/usage", url.Values{"id": {testKeyID}}, nil), &usage)
	if len(usage.Models) != 1 || usage.Models[0].Name != "gpt-5-codex" || usage.Models[0].Daily.CallCount != 2 || !nearly(usage.Models[0].Daily.TotalUSD, 0.5) {
		t.Fatalf("usage models = %+v", usage.Models)
	}
}

func TestUsageHandleIgnoresFailedUnknownAndMalformedRecords(t *testing.T) {
	app := newTestApp(t, appOptions{})
	recordUsage(t, app, UsageHandleRequest{Model: "gpt-5-codex", APIKey: testKeyID, Failed: true, Detail: UsageDetail{InputTokens: 1_000_000}})
	recordUsage(t, app, UsageHandleRequest{Model: "gpt-5-codex", APIKey: "cpa_some_other_key", Detail: UsageDetail{InputTokens: 1_000_000}})
	if raw := handle(t, app, MethodUsageHandle, []byte(`{"Model":`)); string(raw) != "{}" {
		t.Fatalf("malformed usage record answer = %s", raw)
	}
	var status struct {
		Health policy.HealthSnapshot `json:"health"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/status", nil, nil), &status)
	if status.Health.SkippedFailed != 1 || status.Health.IgnoredUnknownKey != 1 {
		t.Fatalf("health = %+v", status.Health)
	}
	var usage struct {
		Models []policy.ModelUsageEntry `json:"models"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys/usage", url.Values{"id": {testKeyID}}, nil), &usage)
	if len(usage.Models) != 0 {
		t.Fatalf("ignored records were billed: %+v", usage.Models)
	}
}

func TestUsageHandleTransportFieldNames(t *testing.T) {
	if ABIVersion != 1 || SchemaVersion != 6 {
		t.Fatalf("ABI/schema versions changed: %d/%d", ABIVersion, SchemaVersion)
	}
	raw, err := json.Marshal(UsageHandleRequest{TraceID: "a1b2c3d4", Provider: "codex", ExecutorType: "codexexecutor", Model: "upstream-model", Alias: "public-model", APIKey: "key-id", Detail: UsageDetail{InputTokens: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	decode(t, raw, &payload)
	requestedModelField := string([]byte{65, 108, 105, 97, 115})
	// Billing reads the first eight; channel statistics read the attempt's
	// credential, timing and failure fields, which handleUsage lists again
	// when it decodes the billing fields alone.
	fields := []string{"TraceID", "Provider", "ExecutorType", "Model", requestedModelField, "APIKey", "Failed", "Detail",
		"BaseURL", "AuthID", "AuthIndex", "RequestedAt", "Latency", "TTFT", "Failure"}
	for _, field := range fields {
		if _, exists := payload[field]; !exists {
			t.Fatalf("transport snapshot missing %q: %s", field, raw)
		}
	}
	if len(payload) != len(fields) || string(payload[requestedModelField]) != `"public-model"` {
		t.Fatalf("transport snapshot = %s", raw)
	}
}

func TestManagementKeyLifecycle(t *testing.T) {
	app := newTestApp(t, appOptions{})
	created := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"team-b","name":"Team B","rpm":10}`))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.StatusCode, created.Body)
	}
	var createdBody struct {
		Key       publicKey `json:"key"`
		PlainKey  string    `json:"plain_key"`
		Generated bool      `json:"generated"`
	}
	decode(t, created.Body, &createdBody)
	if createdBody.Key.ID != "team-b" || createdBody.Key.Name != "Team B" || createdBody.Key.RPM != 10 || !createdBody.Key.Enabled || !createdBody.Generated || !strings.HasPrefix(createdBody.PlainKey, "cpa_") {
		t.Fatalf("create body = %s", created.Body)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer(createdBody.PlainKey), nil); !response.Authenticated || response.Principal != "team-b" {
		t.Fatalf("created key does not authenticate: %+v", response)
	}

	var rotated struct {
		PlainKey string `json:"plain_key"`
	}
	decode(t, managementOK(t, app, http.MethodPost, "/keys/rotate", nil, []byte(`{"id":"team-b"}`)), &rotated)
	if rotated.PlainKey == "" || rotated.PlainKey == createdBody.PlainKey {
		t.Fatalf("rotate returned %q", rotated.PlainKey)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer(createdBody.PlainKey), nil); response.Authenticated {
		t.Fatal("rotated-out secret still authenticates")
	}

	var patched struct {
		Key publicKey `json:"key"`
	}
	decode(t, managementOK(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"team-b","daily_limit_usd":1.5,"weekly_limit_usd":10,"monthly_limit_usd":30,"enabled":false}`)), &patched)
	if patched.Key.DailyLimitUSD != 1.5 || patched.Key.WeeklyLimitUSD != 10 || patched.Key.MonthlyLimitUSD != 30 || patched.Key.Enabled || patched.Key.Usage.Status != "disabled" || patched.Key.Usage.MonthlyLimitUSD != 30 {
		t.Fatalf("patched key = %+v", patched.Key)
	}
	if response := managementCall(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"missing","rpm":1}`)); response.StatusCode != http.StatusNotFound {
		t.Fatalf("patch unknown key status=%d", response.StatusCode)
	}

	var audit struct {
		Events []struct {
			Action  string                     `json:"action"`
			Changes map[string]json.RawMessage `json:"changes"`
		} `json:"events"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/audit", url.Values{"key_id": {"team-b"}, "limit": {"10"}}, nil), &audit)
	if len(audit.Events) < 3 || audit.Events[0].Action != "update_key" || audit.Events[0].Changes["monthly_limit_usd"] == nil {
		t.Fatalf("audit events = %+v", audit.Events)
	}

	if response := managementCall(t, app, http.MethodDelete, "/keys", url.Values{"key_id": {"team-b"}}, nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without id status=%d body=%s", response.StatusCode, response.Body)
	}
	managementOK(t, app, http.MethodDelete, "/keys", url.Values{"id": {"team-b"}}, nil)
	if response := managementCall(t, app, http.MethodDelete, "/keys", url.Values{"id": {"team-b"}}, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status=%d", response.StatusCode)
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer(rotated.PlainKey), nil); response.Authenticated {
		t.Fatal("deleted key still authenticates")
	}
}

func TestManagementResetUsage(t *testing.T) {
	app := newTestApp(t, appOptions{})
	usage := UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}}
	recordUsage(t, app, usage)
	var result policy.UsageResetResult
	decode(t, managementOK(t, app, http.MethodPost, "/keys/reset-usage", nil, []byte(`{"id":"priced","window":"daily"}`)), &result)
	if result.KeyID != testKeyID || result.Window != policy.UsageResetDaily || !nearly(result.BeforeDailyUSD, 0.30) || !nearly(result.AfterDailyUSD, 0) || !nearly(result.AfterWeeklyUSD, 0.30) {
		t.Fatalf("daily reset result = %+v", result)
	}
	recordUsage(t, app, usage)
	decode(t, managementOK(t, app, http.MethodPost, "/keys/reset-usage", nil, []byte(`{"id":"priced","window":"monthly"}`)), &result)
	if result.Window != policy.UsageResetMonthly || !nearly(result.BeforeMonthlyUSD, 0.60) || !nearly(result.AfterMonthlyUSD, 0) {
		t.Fatalf("monthly reset result = %+v", result)
	}
	stale, _ := json.Marshal(map[string]any{"id": "priced", "window": "daily", "expected": policy.UsageResetExpectation{StartedAt: result.NextAccountingBoundaryAt, ResetAfterManualAt: result.NextAccountingBoundaryAt}})
	for _, tc := range []struct {
		name       string
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{"malformed json", []byte(`{"id":`), http.StatusBadRequest, "invalid_json"},
		{"missing id", []byte(`{"window":"daily"}`), http.StatusBadRequest, "missing_id"},
		{"invalid window", []byte(`{"id":"priced","window":"year"}`), http.StatusBadRequest, "invalid_window"},
		{"unknown key", []byte(`{"id":"missing","window":"daily"}`), http.StatusNotFound, "not_found"},
		{"stale expectation", stale, http.StatusConflict, "quota_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := managementCall(t, app, http.MethodPost, "/keys/reset-usage", nil, tc.body)
			if response.StatusCode != tc.wantStatus || errorCode(t, response) != tc.wantCode {
				t.Fatalf("status=%d body=%s, want %d %s", response.StatusCode, response.Body, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestManagementServesResourceUI(t *testing.T) {
	app := NewApp()
	registration := app.managementRegistration()
	found := false
	for _, resource := range registration.Resources {
		if resource.Path == "/index.html" {
			found = true
			if resource.Menu != PluginName || PluginName != "萝卜" {
				t.Fatalf("Menu = %q, want 萝卜", resource.Menu)
			}
		}
	}
	if !found {
		t.Fatalf("resources do not include /index.html: %+v", registration.Resources)
	}
	routes := make(map[string]bool, len(registration.Routes))
	for _, route := range registration.Routes {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{"GET /plugins/cpa-key-policy/stats", "GET /plugins/cpa-key-policy/stats/export", "POST /plugins/cpa-key-policy/models/import"} {
		if !routes[want] {
			t.Fatalf("management routes lack %q: %v", want, routes)
		}
	}
	var response ManagementResponse
	decode(t, handle(t, app, MethodManagementHandle, ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/cpa-key-policy/index.html"}), &response)
	if response.StatusCode != http.StatusOK || response.Headers.Get("Content-Type") != "text/html; charset=utf-8" || len(response.Body) == 0 {
		t.Fatalf("resource response = %d %v %d bytes", response.StatusCode, response.Headers, len(response.Body))
	}
	decode(t, handle(t, app, MethodManagementHandle, ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/cpa-key-policy/assets/app.js"}), &response)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resource status = %d, want 404", response.StatusCode)
	}
}

// A panic answers an error naming the method and the value, which CPA
// shows for interceptors, and is logged with its stack: CPA logs nothing for
// a failed authentication.
// A panic answers with an error naming the method and the value; its stack
// is logged at most once a minute per method, a single line otherwise.
func TestSafePluginCallRecoversPanic(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	panicLogs.mu.Lock()
	delete(panicLogs.last, "test.panic") // an earlier run in this process
	panicLogs.mu.Unlock()
	for _, withStack := range []bool{true, false} {
		logged.Reset()
		response, err := safePluginCall("test.panic", func() ([]byte, error) {
			panic("boom")
		})
		if err == nil || response != nil || !strings.Contains(err.Error(), "test.panic") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("safePluginCall response=%q error=%v", response, err)
		}
		if !strings.Contains(logged.String(), "test.panic panicked: boom") || strings.Contains(logged.String(), "goroutine") != withStack {
			t.Fatalf("panic log (stack expected: %v) = %q", withStack, logged.String())
		}
	}
}

// A line CPA refuses, as it refuses every callback once it has closed the
// instance before shutdown, goes to stderr rather than nowhere: the refusal
// is an error envelope with a zero status.
func TestLogLineTheHostRefusesGoesToStderr(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	app := NewApp()
	host := newDetectionHost()
	app.SetHost(host)
	app.logf("info", "taken by the host")
	host.closed.Store(true)
	app.logf("error", "refused by the host")
	if got := logged.String(); strings.Contains(got, "taken by the host") || !strings.Contains(got, "refused by the host") {
		t.Fatalf("stderr = %q", got)
	}
}

// OKEnvelope writes in one pass the bytes the two-pass encoding wrote.
func TestOKEnvelopeKeepsTheEnvelopeBytes(t *testing.T) {
	for _, value := range []any{
		struct{}{}, nil, RequestInterceptResponse{}, RequestInterceptResponse{Body: []byte("<&>\u2028")},
		jsonResponse(http.StatusOK, map[string]any{"note": "<b>\u2029", "n": 1.5e300}), json.RawMessage(`{ "a" : [1, 2] }`), "plain", []string(nil),
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(Envelope{OK: true, Result: raw})
		if got, err := OKEnvelope(value); err != nil || !bytes.Equal(got, want) {
			t.Errorf("OKEnvelope(%#v) = %s, %v; want %s", value, got, err, want)
		}
	}
}

// A key switched to balance mode loses its period limits, is refused with
// 429 insufficient_quota (no reset time) until credited and once its balance
// is used up, and every adjustment is audited; request_id makes a retried
// credit idempotent.
func TestBalanceModeThroughManagementAndInterceptor(t *testing.T) {
	app := newTestApp(t, appOptions{DailyLimitUSD: 2})
	var patched struct {
		Key publicKey `json:"key"`
	}
	decode(t, managementOK(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"priced","quota_mode":"balance"}`)), &patched)
	if patched.Key.QuotaMode != "balance" || patched.Key.DailyLimitUSD != 0 || patched.Key.Usage.BlockedReason != "balance_exhausted" || patched.Key.Usage.BalanceUSD == nil || *patched.Key.Usage.BalanceUSD != 0 {
		t.Fatalf("switched key = %+v", patched.Key)
	}
	for body, code := range map[string]string{
		`{"id":"priced","daily_limit_usd":5}`:                            "invalid_policy",
		`{"id":"priced","quota_mode":"prepaid"}`:                         "invalid_policy",
		`{"id":"cycle-key","quota_mode":"balance","weekly_limit_usd":1}`: "invalid_policy",
	} {
		method := http.MethodPatch
		if strings.Contains(body, "cycle-key") {
			method = http.MethodPost
		}
		if response := managementCall(t, app, method, "/keys", nil, []byte(body)); response.StatusCode != http.StatusBadRequest || errorCode(t, response) != code {
			t.Fatalf("%s %s = %d %s", method, body, response.StatusCode, response.Body)
		}
	}

	headers := bearer(testKeyPlain)
	refused := interceptBefore(t, app, "trace-uncredited", "openai-response", headers, "/v1/responses")
	var refusedBody struct {
		Error struct {
			Message, Type, Code string
			ResetsAt            *int64 `json:"resets_at"`
		} `json:"error"`
	}
	decode(t, refused.ResponseBody, &refusedBody)
	if !refused.Terminate || refused.StatusCode != http.StatusTooManyRequests || refusedBody.Error.Type != "insufficient_quota" || refusedBody.Error.Code != "insufficient_quota" ||
		refusedBody.Error.ResetsAt != nil || refused.ResponseHeaders.Get("Retry-After") != "" || !strings.Contains(refusedBody.Error.Message, "prepaid balance") {
		t.Fatalf("uncredited balance rejection = %+v body=%s", refused, refused.ResponseBody)
	}

	var credit policy.BalanceAdjustment
	decode(t, managementOK(t, app, http.MethodPost, "/keys/balance", nil, []byte(`{"id":"priced","amount_usd":1.5,"note":"top-up","request_id":"order-7"}`)), &credit)
	if credit.KeyID != "priced" || !nearly(credit.AfterUSD, 1.5) || credit.Duplicate {
		t.Fatalf("credit = %+v", credit)
	}
	decode(t, managementOK(t, app, http.MethodPost, "/keys/balance", nil, []byte(`{"id":"priced","amount_usd":1.5,"request_id":"order-7"}`)), &credit)
	if !credit.Duplicate || !nearly(credit.AfterUSD, 1.5) {
		t.Fatalf("retried credit = %+v", credit)
	}
	if response := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"cycle-key","key":"cpa_cycle_key_secret"}`)); response.StatusCode != http.StatusCreated {
		t.Fatalf("create cycle key = %d %s", response.StatusCode, response.Body)
	}
	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"id":"priced"}`, http.StatusBadRequest, "invalid_amount"},
		{`{"id":"priced","amount_usd":0}`, http.StatusBadRequest, "invalid_amount"},
		{`{"id":"priced","amount_usd":1,"extra":true}`, http.StatusBadRequest, "invalid_json"},
		{`{"id":"nobody","amount_usd":1}`, http.StatusNotFound, "not_found"},
		{`{"id":"cycle-key","amount_usd":1}`, http.StatusConflict, "not_balance_mode"},
		{`{"id":"priced","amount_usd":2,"request_id":"order-7"}`, http.StatusConflict, "request_id_conflict"},
	} {
		if response := managementCall(t, app, http.MethodPost, "/keys/balance", nil, []byte(tc.body)); response.StatusCode != tc.status || errorCode(t, response) != tc.code {
			t.Fatalf("POST /keys/balance %s = %d %s", tc.body, response.StatusCode, response.Body)
		}
	}

	if response := interceptBefore(t, app, "trace-credited", "openai", headers, "/v1/chat/completions"); response.Terminate {
		t.Fatalf("credited key refused: %s", response.ResponseBody)
	}
	recordUsage(t, app, UsageHandleRequest{
		Provider: "codex", ExecutorType: "codexexecutor", Model: "gpt-5-codex", APIKey: testKeyID,
		Detail: UsageDetail{InputTokens: 1_500_000, TotalTokens: 1_500_000},
	})
	var usage struct {
		Usage policy.UsageSummary `json:"usage"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys/usage", url.Values{"id": {"priced"}}, nil), &usage)
	if usage.Usage.BalanceUSD == nil || !nearly(*usage.Usage.BalanceUSD, 0) || usage.Usage.Status != "limited" || !nearly(*usage.Usage.NetCreditedUSD, 1.5) {
		t.Fatalf("usage after spending the balance = %+v", usage.Usage)
	}
	if response := interceptBefore(t, app, "trace-spent", "claude", headers, "/v1/messages"); !response.Terminate || !strings.Contains(string(response.ResponseBody), "prepaid balance") {
		t.Fatalf("spent balance rejection = %+v body=%s", response, response.ResponseBody)
	}

	var audit struct {
		Events []struct {
			Action  string                     `json:"action"`
			Changes map[string]json.RawMessage `json:"changes"`
		} `json:"events"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/audit", url.Values{"key_id": {"priced"}, "action": {"adjust_balance"}}, nil), &audit)
	if len(audit.Events) != 1 || audit.Events[0].Action != "adjust_balance" {
		t.Fatalf("adjust_balance events = %+v", audit.Events)
	}
	if changes := audit.Events[0].Changes; string(changes["note"]) == "" || string(changes["balance_usd"]) == "" {
		t.Fatalf("adjust_balance changes = %v", changes)
	}
}
