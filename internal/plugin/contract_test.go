package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The field lists below are the management API contract (spec §2 and §12).
var (
	keyFields         = []string{"id", "name", "enabled", "key_preview", "rpm", "daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd", "quota_mode", "usage", "created_at", "updated_at"}
	usageFields       = []string{"quota_mode", "cycles", "status", "daily_usd", "weekly_usd", "monthly_usd", "daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd", "next_accounting_boundary_at", "soft_limit_hit", "timezone"}
	cycleFields       = []string{"window", "started_at", "resets_at", "reset_kind", "used_usd", "limit_usd", "reset_after_manual_at"}
	windowCounters    = []string{"call_count", "input_tokens", "output_tokens", "cache_read_tokens", "cache_cost_usd", "cache_write_tokens", "cache_write_usd", "unpriced_calls"}
	priceRowFields    = []string{"name", "provider", "billing_mode", "billing_multiplier", "input_price_per_million", "output_price_per_million", "cache_read_price_per_million", "per_call_usd", "priced"}
	statsTotalsFields = []string{"charged_usd", "calls", "unpriced_calls", "input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens"}
	healthFields      = []string{"last_flush_at", "last_flush_error", "flush_failures", "skipped_failed", "ignored_unknown_key", "ignored_no_model", "ignored_empty_usage", "clamped_tokens", "invalid_usage", "unmetered_per_call", "unpriced_calls"}

	// The 萝卜 console routes.
	overviewFields       = []string{"today", "keys", "channels", "modules", "warnings", "recent_changes"}
	settingsFields       = []string{"version", "plugin_id", "display_name", "modules", "module_errors", "hooks", "config_warnings", "files", "ui", "ownership", "health", "timezone"}
	searchStateFields    = []string{"enabled", "declared", "settings", "status"}
	channelViewFields    = []string{"auth_index", "meta", "last_24h", "today", "models_24h", "rules"}
	channelViewOptional  = []string{"auth_id", "provider", "base_url"}
	channelSummaryFields = []string{"attempts", "failures", "success_rate", "latency_p50_ms", "latency_p95_ms", "ttft_p50_ms", "ttft_p95_ms", "input_tokens", "output_tokens", "list_cost_usd"}
)

func TestConsoleResponseShapes(t *testing.T) {
	app := newTestApp(t, appOptions{})
	recordUsage(t, app, UsageHandleRequest{AuthIndex: "0123456789abcdef", AuthID: "codex:apikey:1", Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	assertShape(t, "GET /overview", managementOK(t, app, http.MethodGet, "/overview", nil, nil), overviewFields, nil)
	assertShape(t, "GET /settings", managementOK(t, app, http.MethodGet, "/settings", nil, nil), settingsFields, nil)
	assertShape(t, "GET /search", managementOK(t, app, http.MethodGet, "/search", nil, nil), searchStateFields, nil)
	assertShape(t, "GET /adapters", managementOK(t, app, http.MethodGet, "/adapters", nil, nil), []string{"rules", "types"}, nil)

	var channels struct {
		Channels []json.RawMessage `json:"channels"`
	}
	body := managementOK(t, app, http.MethodGet, "/channels", nil, nil)
	assertShape(t, "GET /channels", body, []string{"channels", "groups", "ids"}, nil)
	decode(t, body, &channels)
	if len(channels.Channels) != 1 {
		t.Fatalf("channels = %s", body)
	}
	assertShape(t, "channels[0]", channels.Channels[0], channelViewFields, channelViewOptional)
	var row struct {
		Last24h json.RawMessage `json:"last_24h"`
	}
	decode(t, channels.Channels[0], &row)
	assertShape(t, "channels[0].last_24h", row.Last24h, channelSummaryFields, []string{"status_codes", "last_at"})
	if strings.Contains(string(body), "0001-01-01") {
		t.Fatalf("zero times leak into GET /channels: %s", body)
	}
}

func assertKeyObject(t *testing.T, label string, raw json.RawMessage) {
	t.Helper()
	assertShape(t, label, raw, keyFields, nil)
	var key struct {
		Usage json.RawMessage `json:"usage"`
	}
	decode(t, raw, &key)
	// balance_usd and net_credited_usd describe a balance key only.
	assertShape(t, label+".usage", key.Usage, usageFields, []string{"blocked_reason", "balance_usd", "net_credited_usd"})
	var usage struct {
		Cycles []json.RawMessage `json:"cycles"`
		Status string            `json:"status"`
	}
	decode(t, key.Usage, &usage)
	if len(usage.Cycles) != 3 {
		t.Fatalf("%s: cycles = %s", label, key.Usage)
	}
	for i, cycle := range usage.Cycles {
		assertShape(t, label+".usage.cycles["+string(rune('0'+i))+"]", cycle, cycleFields, nil)
	}
	if usage.Status != "normal" && usage.Status != "warning" && usage.Status != "limited" && usage.Status != "disabled" {
		t.Fatalf("%s: status %q", label, usage.Status)
	}
}

func TestKeysResponseShape(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	var response struct {
		Keys []json.RawMessage `json:"keys"`
	}
	body := managementOK(t, app, http.MethodGet, "/keys", nil, nil)
	assertShape(t, "GET /keys", body, []string{"keys"}, nil)
	decode(t, body, &response)
	if len(response.Keys) != 1 {
		t.Fatalf("keys = %s", body)
	}
	assertKeyObject(t, "keys[0]", response.Keys[0])
	for _, removed := range []string{"models", "allow_models_endpoint", "limits_changed_at", "limited_models", "partial"} {
		if strings.Contains(string(body), `"`+removed+`"`) {
			t.Fatalf("GET /keys still carries %q: %s", removed, body)
		}
	}
}

func TestCreateKeyResponseShapeAndDuplicateSecretConflict(t *testing.T) {
	app := newTestApp(t, appOptions{})
	created := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"team-b","key":"cpa_team_b_secret"}`))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.StatusCode, created.Body)
	}
	assertShape(t, "POST /keys", created.Body, []string{"key", "plain_key", "generated"}, nil)
	var response struct {
		Key       json.RawMessage `json:"key"`
		PlainKey  string          `json:"plain_key"`
		Generated bool            `json:"generated"`
	}
	decode(t, created.Body, &response)
	assertKeyObject(t, "POST /keys key", response.Key)
	if response.PlainKey != "cpa_team_b_secret" || response.Generated {
		t.Fatalf("create body = %s", created.Body)
	}

	duplicate := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"team-c","key":"`+testKeyPlain+`"}`))
	if duplicate.StatusCode != http.StatusConflict || errorCode(t, duplicate) != "duplicate_key" {
		t.Fatalf("duplicate secret on create: status=%d body=%s", duplicate.StatusCode, duplicate.Body)
	}
	upper := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"team-d","key":"CPA_TEAM_B_SECRET"}`))
	if upper.StatusCode != http.StatusCreated {
		t.Fatalf("a different secret must not conflict: status=%d body=%s", upper.StatusCode, upper.Body)
	}
	patched := managementCall(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"team-b","key":"`+testKeyPlain+`"}`))
	if patched.StatusCode != http.StatusConflict || errorCode(t, patched) != "duplicate_key" {
		t.Fatalf("duplicate secret on patch: status=%d body=%s", patched.StatusCode, patched.Body)
	}
	if response := managementCall(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"team-b","rpm":-1}`)); response.StatusCode != http.StatusBadRequest || errorCode(t, response) != "invalid_policy" {
		t.Fatalf("negative rpm: status=%d body=%s", response.StatusCode, response.Body)
	}
}

func TestKeyUsageResponseShape(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "free-model", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	body := managementOK(t, app, http.MethodGet, "/keys/usage", url.Values{"id": {testKeyID}}, nil)
	assertShape(t, "GET /keys/usage", body, []string{"key_id", "key_name", "daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd", "models", "usage"}, nil)
	var response struct {
		KeyID  string            `json:"key_id"`
		Models []json.RawMessage `json:"models"`
		Usage  json.RawMessage   `json:"usage"`
	}
	decode(t, body, &response)
	assertShape(t, "GET /keys/usage usage", response.Usage, usageFields, []string{"blocked_reason", "balance_usd", "net_credited_usd"})
	if response.KeyID != testKeyID || len(response.Models) != 2 {
		t.Fatalf("usage response = %s", body)
	}
	var priced struct {
		Name              string          `json:"name"`
		BillingMode       string          `json:"billing_mode"`
		BillingMultiplier float64         `json:"billing_multiplier"`
		Priced            bool            `json:"priced"`
		Daily             json.RawMessage `json:"daily"`
	}
	for i, raw := range response.Models {
		assertShape(t, "models["+string(rune('0'+i))+"]", raw, []string{"name", "priced", "daily", "weekly", "monthly"}, []string{"billing_mode", "billing_multiplier"})
		if strings.Contains(string(raw), `"in_config"`) {
			t.Fatalf("model row still carries in_config: %s", raw)
		}
	}
	decode(t, response.Models[0], &priced)
	if priced.Name != "free-model" || priced.Priced || priced.BillingMode != "tokens" {
		t.Fatalf("models[0] = %s", response.Models[0])
	}
	decode(t, response.Models[1], &priced)
	if priced.Name != "gpt-5-codex" || !priced.Priced || priced.BillingMode != "tokens" || priced.BillingMultiplier != 1 {
		t.Fatalf("models[1] = %s", response.Models[1])
	}
	assertShape(t, "models[1].daily", priced.Daily, []string{"total_usd"}, append([]string{"window_start"}, windowCounters...))
	var daily struct {
		TotalUSD     float64 `json:"total_usd"`
		CallCount    int64   `json:"call_count"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		WindowStart  string  `json:"window_start"`
	}
	decode(t, priced.Daily, &daily)
	if !nearly(daily.TotalUSD, 0.30) || daily.CallCount != 1 || daily.InputTokens != 200_000 || daily.OutputTokens != 100_000 || !strings.HasPrefix(daily.WindowStart, "2026-10-20T00:00:00+08:00") {
		t.Fatalf("daily window = %s", priced.Daily)
	}
	for _, tc := range []struct {
		query      url.Values
		wantStatus int
	}{
		{nil, http.StatusBadRequest},
		{url.Values{"id": {"nope"}}, http.StatusNotFound},
	} {
		if response := managementCall(t, app, http.MethodGet, "/keys/usage", tc.query, nil); response.StatusCode != tc.wantStatus {
			t.Fatalf("GET /keys/usage %v status=%d, want %d", tc.query, response.StatusCode, tc.wantStatus)
		}
	}
}

func TestKeyHistoryResponseShapeAndBounds(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}})
	body := managementOK(t, app, http.MethodGet, "/keys/history", url.Values{"id": {testKeyID}}, nil)
	assertShape(t, "GET /keys/history", body, []string{"key_id", "timezone", "days"}, nil)
	var response struct {
		KeyID    string `json:"key_id"`
		Timezone string `json:"timezone"`
		Days     []struct {
			Date    string                     `json:"date"`
			ByModel map[string]json.RawMessage `json:"by_model"`
		} `json:"days"`
	}
	decode(t, body, &response)
	if response.KeyID != testKeyID || response.Timezone != "Asia/Shanghai" || len(response.Days) != 30 || response.Days[0].Date != "2026-09-21" || response.Days[29].Date != "2026-10-20" {
		t.Fatalf("history = %s", body)
	}
	var days struct {
		Days []json.RawMessage `json:"days"`
	}
	decode(t, body, &days)
	assertShape(t, "days[29]", days.Days[29], []string{"date", "total_usd", "by_model"}, windowCounters)
	assertShape(t, "days[0]", days.Days[0], []string{"date", "total_usd", "by_model"}, nil)
	assertShape(t, "days[29].by_model[gpt-5-codex]", response.Days[29].ByModel["gpt-5-codex"], []string{"total_usd"}, windowCounters)

	for days, wantStatus := range map[string]int{"1": http.StatusOK, "90": http.StatusOK, "0": http.StatusBadRequest, "91": http.StatusBadRequest, "x": http.StatusBadRequest} {
		response := managementCall(t, app, http.MethodGet, "/keys/history", url.Values{"id": {testKeyID}, "days": {days}}, nil)
		if response.StatusCode != wantStatus {
			t.Fatalf("days=%s status=%d, want %d", days, response.StatusCode, wantStatus)
		}
	}
}

func TestModelsResponseShape(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	recordUsage(t, app, UsageHandleRequest{Provider: "openai", Model: "gpt-6-terra", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	recordUsage(t, app, UsageHandleRequest{Provider: "openai", Model: "gpt-6-terra", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "free-model", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	body := managementOK(t, app, http.MethodGet, "/models", nil, nil)
	assertShape(t, "GET /models", body, []string{"models", "unpriced"}, nil)
	var response struct {
		Models   []json.RawMessage `json:"models"`
		Unpriced []json.RawMessage `json:"unpriced"`
	}
	decode(t, body, &response)
	if len(response.Models) != 3 || len(response.Unpriced) != 1 {
		t.Fatalf("models response = %s", body)
	}
	for _, raw := range response.Models {
		assertShape(t, "models[]", raw, priceRowFields, []string{"cache_write_price_per_million"})
		if strings.Contains(string(raw), "target_model") || strings.Contains(string(raw), "ref_") {
			t.Fatalf("price row carries routing fields: %s", raw)
		}
	}
	var rows []struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Priced   bool   `json:"priced"`
	}
	decode(t, body, &struct {
		Models *[]struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Priced   bool   `json:"priced"`
		} `json:"models"`
	}{Models: &rows})
	if rows[0].Name != "free-model" || rows[0].Priced || rows[1].Name != "gpt-5-codex" || rows[1].Provider != "codex" || !rows[1].Priced || rows[2].Name != "grok-imagine-image" || !rows[2].Priced {
		t.Fatalf("price rows = %+v", rows)
	}
	assertShape(t, "unpriced[0]", response.Unpriced[0], []string{"name", "calls", "last_seen"}, []string{"has_qualified_rows"})
	if string(response.Unpriced[0]) != `{"name":"gpt-6-terra","calls":2,"last_seen":"2026-10-20"}` {
		t.Fatalf("unpriced[0] = %s", response.Unpriced[0])
	}
	// A provider-qualified row keeps the model listed for the calls it did
	// not price and says so.
	managementOK(t, app, http.MethodPost, "/models", nil, []byte(`{"name":"gpt-6-terra","provider":"codex","input_price_per_million":1}`))
	decode(t, managementOK(t, app, http.MethodGet, "/models", nil, nil), &response)
	if len(response.Unpriced) != 1 || string(response.Unpriced[0]) != `{"name":"gpt-6-terra","calls":2,"last_seen":"2026-10-20","has_qualified_rows":true}` {
		t.Fatalf("unpriced with a qualified row = %s", response.Unpriced)
	}
}

func TestModelWriteRoutes(t *testing.T) {
	app := newTestApp(t, appOptions{})
	created := managementOK(t, app, http.MethodPost, "/models", nil, []byte(`{"name":" GPT-6-sol ","billing_mode":"tokens","input_price_per_million":1.25,"output_price_per_million":10,"cache_read_price_per_million":0.125,"per_call_usd":0}`))
	assertShape(t, "POST /models", created, []string{"model"}, nil)
	var response struct {
		Model json.RawMessage `json:"model"`
	}
	decode(t, created, &response)
	assertShape(t, "POST /models model", response.Model, priceRowFields, []string{"cache_write_price_per_million"})
	var row struct {
		Name              string  `json:"name"`
		Provider          string  `json:"provider"`
		BillingMultiplier float64 `json:"billing_multiplier"`
		Priced            bool    `json:"priced"`
	}
	decode(t, response.Model, &row)
	if row.Name != "GPT-6-sol" || row.Provider != "" || row.BillingMultiplier != 1 || !row.Priced {
		t.Fatalf("created row = %s", response.Model)
	}
	zero := managementOK(t, app, http.MethodPost, "/models", nil, []byte(`{"name":"gpt-6-terra","provider":"openai-compatible-Nvidia"}`))
	decode(t, zero, &response)
	decode(t, response.Model, &row)
	if row.Name != "gpt-6-terra" || row.Provider != "nvidia" || row.Priced {
		t.Fatalf("all-zero row = %s", response.Model)
	}
	for name, body := range map[string][]byte{
		"routing fields":     []byte(`{"name":"old","target_model":"gpt"}`),
		"multiplier too big": []byte(`{"name":"x","billing_multiplier":51}`),
		"price too big":      []byte(`{"name":"x","input_price_per_million":10001}`),
	} {
		if response := managementCall(t, app, http.MethodPost, "/models", nil, body); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, response.StatusCode, response.Body)
		}
	}

	imported := managementOK(t, app, http.MethodPost, "/models/import", nil, []byte(`{"items":[{"name":"gpt-image-2","provider":"openai"},{"name":"gpt-5-codex","provider":"codex"},{"name":"gpt-image-2","provider":"openai"}]}`))
	assertShape(t, "POST /models/import", imported, []string{"created", "skipped"}, nil)
	if string(imported) != `{"created":[{"name":"gpt-image-2"}],"skipped":[{"name":"gpt-5-codex","reason":"exists"},{"name":"gpt-image-2","reason":"duplicate"}]}` {
		t.Fatalf("import result = %s", imported)
	}
	for name, body := range map[string][]byte{
		"legacy item fields": []byte(`{"items":[{"provider":"openai","target_model":"m"}]}`),
		"missing name":       []byte(`{"items":[{"provider":"openai"}]}`),
	} {
		if response := managementCall(t, app, http.MethodPost, "/models/import", nil, body); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, response.StatusCode, response.Body)
		}
	}

	prices := managementOK(t, app, http.MethodPost, "/models/import-prices", nil, []byte(`{"dry_run":false,"matches":[{"model":"gpt-image-2","prompt_price_per_1m":5,"completion_price_per_1m":10},{"model":"nope","prompt_price_per_1m":1}]}`))
	assertShape(t, "POST /models/import-prices", prices, []string{"applied", "unchanged", "skipped"}, nil)
	var priceResult struct {
		Applied []struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		} `json:"applied"`
		Skipped []struct {
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	decode(t, prices, &priceResult)
	if len(priceResult.Applied) != 1 || priceResult.Applied[0].Model != "gpt-image-2" || priceResult.Applied[0].Provider != "openai" || len(priceResult.Skipped) != 1 || priceResult.Skipped[0].Reason != "no_match" {
		t.Fatalf("import-prices result = %s", prices)
	}
	if response := managementCall(t, app, http.MethodPost, "/models/import-prices", nil, []byte(`{"dry_run":true,"matches":[{"model":"gpt-image-2","prompt_price_per_1m":-1}]}`)); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid dry-run status=%d body=%s", response.StatusCode, response.Body)
	}

	managementOK(t, app, http.MethodDelete, "/models", nil, []byte(`{"name":"gpt-image-2","provider":"openai"}`))
	managementOK(t, app, http.MethodDelete, "/models", nil, []byte(`{"name":"gpt-6-sol"}`))
	if response := managementCall(t, app, http.MethodDelete, "/models", nil, []byte(`{"name":"gpt-6-sol"}`)); response.StatusCode != http.StatusNotFound || errorCode(t, response) != "not_found" {
		t.Fatalf("deleting a missing row status=%d body=%s", response.StatusCode, response.Body)
	}
	var remaining struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/models", nil, nil), &remaining)
	names := make([]string, 0, len(remaining.Models))
	for _, model := range remaining.Models {
		names = append(names, model.Name)
	}
	if strings.Join(names, ",") != "free-model,gpt-5-codex,gpt-6-terra,grok-imagine-image" {
		t.Fatalf("remaining rows = %v", names)
	}

	// A state file that cannot be written is a server error, not a rejected
	// input, for model and key writes alike.
	statePath := app.store.StatePath()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if response := managementCall(t, app, http.MethodPost, "/models", nil, []byte(`{"name":"gpt-7"}`)); response.StatusCode != http.StatusInternalServerError || errorCode(t, response) != "persist_failed" {
		t.Fatalf("model write without a writable state file: status=%d body=%s", response.StatusCode, response.Body)
	}
	if response := managementCall(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"priced","rpm":5}`)); response.StatusCode != http.StatusInternalServerError || errorCode(t, response) != "persist_failed" {
		t.Fatalf("key write without a writable state file: status=%d body=%s", response.StatusCode, response.Body)
	}
}

func TestStatsResponseShapeAndBounds(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}})
	recordUsage(t, app, UsageHandleRequest{Provider: "openai", Model: "gpt-6-terra", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	body := managementOK(t, app, http.MethodGet, "/stats", url.Values{"days": {"7"}}, nil)
	assertShape(t, "GET /stats", body, []string{"timezone", "from", "to", "totals", "days", "models", "keys", "keys_total", "keys_truncated", "unpriced_models"}, nil)
	var response struct {
		Timezone       string            `json:"timezone"`
		From           string            `json:"from"`
		To             string            `json:"to"`
		Totals         json.RawMessage   `json:"totals"`
		Days           []json.RawMessage `json:"days"`
		Models         []json.RawMessage `json:"models"`
		Keys           []json.RawMessage `json:"keys"`
		KeysTotal      int               `json:"keys_total"`
		KeysTruncated  bool              `json:"keys_truncated"`
		UnpricedModels []json.RawMessage `json:"unpriced_models"`
	}
	decode(t, body, &response)
	if response.Timezone != "Asia/Shanghai" || response.From != "2026-10-14" || response.To != "2026-10-20" || len(response.Days) != 7 || len(response.Models) != 2 || len(response.Keys) != 1 || response.KeysTotal != 1 || response.KeysTruncated || len(response.UnpricedModels) != 1 {
		t.Fatalf("stats = %s", body)
	}
	assertShape(t, "totals", response.Totals, statsTotalsFields, nil)
	if string(response.Totals) != `{"charged_usd":0.3,"calls":2,"unpriced_calls":1,"input_tokens":200010,"cache_read_tokens":0,"cache_write_tokens":0,"output_tokens":100000}` {
		t.Fatalf("totals = %s", response.Totals)
	}
	assertShape(t, "days[6]", response.Days[6], append([]string{"date"}, statsTotalsFields...), nil)
	assertShape(t, "days[0]", response.Days[0], append([]string{"date"}, statsTotalsFields...), nil)
	assertShape(t, "models[0]", response.Models[0], append([]string{"name", "priced"}, statsTotalsFields...), nil)
	assertShape(t, "keys[0]", response.Keys[0], append([]string{"id", "name", "deleted"}, statsTotalsFields...), nil)
	assertShape(t, "unpriced_models[0]", response.UnpricedModels[0], []string{"name", "calls", "last_seen"}, []string{"has_qualified_rows"})
	if !strings.HasPrefix(string(response.Models[0]), `{"name":"gpt-5-codex","priced":true,"charged_usd":0.3,`) || !strings.HasPrefix(string(response.Models[1]), `{"name":"gpt-6-terra","priced":false,`) {
		t.Fatalf("stats models = %s", body)
	}
	if !strings.HasPrefix(string(response.Keys[0]), `{"id":"priced","name":"Priced","deleted":false,"charged_usd":0.3,`) {
		t.Fatalf("stats keys = %s", response.Keys[0])
	}

	var defaults struct {
		From string `json:"from"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/stats", nil, nil), &defaults)
	if defaults.From != "2026-09-21" {
		t.Fatalf("default range starts %s, want 30 days", defaults.From)
	}
	for days, wantStatus := range map[string]int{"1": http.StatusOK, "90": http.StatusOK, "0": http.StatusBadRequest, "91": http.StatusBadRequest, "x": http.StatusBadRequest} {
		if response := managementCall(t, app, http.MethodGet, "/stats", url.Values{"days": {days}}, nil); response.StatusCode != wantStatus {
			t.Fatalf("days=%s status=%d, want %d", days, response.StatusCode, wantStatus)
		}
	}
}

func TestStatsExportCSV(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}})
	response := managementCall(t, app, http.MethodGet, "/stats/export", url.Values{"from": {"2026-10-14"}, "to": {"2026-10-20"}}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("export status=%d body=%s", response.StatusCode, response.Body)
	}
	if ct := response.Headers.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := response.Headers.Get("Content-Disposition"); cd != "attachment; filename=cpa-key-policy-usage-2026-10-14_2026-10-20.csv" {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	want := "date,key_id,key_name,deleted,model,calls,unpriced_calls,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,charged_usd\n2026-10-20,priced,Priced,false,gpt-5-codex,1,0,200000,0,0,100000,0.3\n"
	if string(response.Body) != want {
		t.Fatalf("csv = %q", response.Body)
	}
	for name, tc := range map[string]struct {
		query      url.Values
		wantStatus int
		wantCode   string
	}{
		"too many days": {url.Values{"from": {"2026-01-01"}, "to": {"2026-06-30"}}, http.StatusRequestEntityTooLarge, "range_too_large"},
		"reversed":      {url.Values{"from": {"2026-10-20"}, "to": {"2026-10-14"}}, http.StatusBadRequest, "invalid_range"},
		"malformed":     {url.Values{"from": {"2026-13-01"}, "to": {"2026-10-14"}}, http.StatusBadRequest, "invalid_range"},
		"missing":       {nil, http.StatusBadRequest, "invalid_range"},
	} {
		response := managementCall(t, app, http.MethodGet, "/stats/export", tc.query, nil)
		if response.StatusCode != tc.wantStatus || errorCode(t, response) != tc.wantCode {
			t.Fatalf("%s: status=%d body=%s, want %d %s", name, response.StatusCode, response.Body, tc.wantStatus, tc.wantCode)
		}
	}
}

func TestStatusResponseShape(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	body := managementOK(t, app, http.MethodGet, "/status", nil, nil)
	assertShape(t, "GET /status", body, []string{"enabled", "state_file", "dataset_id", "key_count", "model_count", "rpm_usage", "health"}, nil)
	var response struct {
		Enabled    bool            `json:"enabled"`
		StateFile  string          `json:"state_file"`
		DatasetID  string          `json:"dataset_id"`
		KeyCount   int             `json:"key_count"`
		ModelCount int             `json:"model_count"`
		RPMUsage   map[string]int  `json:"rpm_usage"`
		Health     json.RawMessage `json:"health"`
	}
	decode(t, body, &response)
	if !response.Enabled || response.StateFile != app.store.StatePath() || response.DatasetID == "" || response.KeyCount != 1 || response.ModelCount != 3 || response.RPMUsage == nil {
		t.Fatalf("status = %s", body)
	}
	assertShape(t, "health", response.Health, healthFields, nil)
	if strings.Contains(string(body), `"usage"`) {
		t.Fatalf("status still reports per-key usage: %s", body)
	}
}
