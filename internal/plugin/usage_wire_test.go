package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cpa-key-policy/internal/policy"
)

func keyUsageModels(t *testing.T, app *App) []policy.ModelUsageEntry {
	t.Helper()
	var response struct {
		Models []policy.ModelUsageEntry `json:"models"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys/usage", url.Values{"id": {testKeyID}}, nil), &response)
	return response.Models
}

func health(t *testing.T, app *App) policy.HealthSnapshot {
	t.Helper()
	var response struct {
		Health policy.HealthSnapshot `json:"health"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/status", nil, nil), &response)
	return response.Health
}

// usage.handle receives pluginapi.UsageRecord as CPA marshals it: Go field
// names, durations as integer nanoseconds and many fields this plugin never
// reads. The record bills 200K input + 100K output at $1/M each.
func TestUsageHandleBillsHostWireRecord(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	record := hostUsageRecord(t)
	var fields map[string]json.RawMessage
	decode(t, record, &fields)
	if string(fields["Latency"]) != "1234567890" || string(fields["Generate"]) != "true" || string(fields["APIKey"]) != `"priced"` || string(fields["Model"]) != `"gpt-5-codex"` {
		t.Fatalf("wire fixture drifted: %s", record)
	}
	handle(t, app, MethodUsageHandle, record)

	models := keyUsageModels(t, app)
	if len(models) != 1 || models[0].Name != "gpt-5-codex" || !models[0].Priced || models[0].BillingMode != "tokens" {
		t.Fatalf("billed models = %+v", models)
	}
	daily := models[0].Daily
	if !nearly(daily.TotalUSD, 0.30) || daily.CallCount != 1 || daily.InputTokens != 200_000 || daily.OutputTokens != 100_000 || daily.UnpricedCalls != 0 {
		t.Fatalf("daily window = %+v", daily)
	}
	var stats policy.StatsReport
	decode(t, managementOK(t, app, http.MethodGet, "/stats", url.Values{"days": {"7"}}, nil), &stats)
	if !nearly(stats.Totals.ChargedUSD, 0.30) || stats.Totals.Calls != 1 || stats.Totals.InputTokens != 200_000 || stats.Totals.OutputTokens != 100_000 || len(stats.Keys) != 1 || stats.Keys[0].ID != testKeyID || stats.Keys[0].Deleted {
		t.Fatalf("stats = %+v", stats)
	}
	if h := health(t, app); h.IgnoredUnknownKey != 0 || h.InvalidUsage != 0 || h.IgnoredEmptyUsage != 0 {
		t.Fatalf("health after a valid record = %+v", h)
	}
}

// One request as CPA drives it: authentication defers admission, the
// interceptor admits, usage.handle bills, and the reads reflect it. A key
// disabled while its request is in flight is still billed.
func TestRequestPathEndToEnd(t *testing.T) {
	app := newTestApp(t, appOptions{RPM: 5, DailyLimitUSD: 1, Clock: testClock(t)})
	headers := bearer(testKeyPlain)
	if response := authenticate(t, app, http.MethodPost, "/v1/responses", headers, nil); !response.Authenticated || response.Principal != testKeyID {
		t.Fatalf("authenticate = %+v", response)
	}
	if response := interceptBefore(t, app, "a1b2c3d4", "openai", headers, "/v1/responses"); response.Terminate {
		t.Fatalf("admitted request terminated: %+v", response)
	}
	handle(t, app, MethodUsageHandle, hostUsageRecord(t))

	managementOK(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"priced","enabled":false}`))
	if response := authenticate(t, app, http.MethodPost, "/v1/responses", headers, nil); response.Authenticated {
		t.Fatalf("disabled key authenticated: %+v", response)
	}
	handle(t, app, MethodUsageHandle, hostUsageRecord(t))

	models := keyUsageModels(t, app)
	if len(models) != 1 || models[0].Daily.CallCount != 2 || !nearly(models[0].Daily.TotalUSD, 0.60) || !nearly(models[0].Weekly.TotalUSD, 0.60) || !nearly(models[0].Monthly.TotalUSD, 0.60) {
		t.Fatalf("usage after two records = %+v", models)
	}
	var keys struct {
		Keys []publicKey `json:"keys"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys", nil, nil), &keys)
	if keys.Keys[0].Usage.Status != "disabled" || !nearly(keys.Keys[0].Usage.DailyUSD, 0.60) || keys.Keys[0].Usage.DailyLimitUSD != 1 {
		t.Fatalf("key summary = %+v", keys.Keys[0].Usage)
	}
	var history struct {
		Days []policy.UsageHistoryDay `json:"days"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys/history", url.Values{"id": {testKeyID}, "days": {"1"}}, nil), &history)
	if len(history.Days) != 1 || history.Days[0].Date != "2026-10-20" || !nearly(history.Days[0].TotalUSD, 0.60) || history.Days[0].CallCount != 2 || !nearly(history.Days[0].ByModel["gpt-5-codex"].TotalUSD, 0.60) {
		t.Fatalf("history = %+v", history.Days)
	}
	var stats policy.StatsReport
	decode(t, managementOK(t, app, http.MethodGet, "/stats", url.Values{"days": {"1"}}, nil), &stats)
	if stats.From != "2026-10-20" || !nearly(stats.Totals.ChargedUSD, 0.60) || stats.Totals.Calls != 2 || len(stats.Days) != 1 || !nearly(stats.Days[0].ChargedUSD, 0.60) || stats.Models[0].Name != "gpt-5-codex" || stats.Keys[0].Name != "Priced" {
		t.Fatalf("stats = %+v", stats)
	}
	export := managementCall(t, app, http.MethodGet, "/stats/export", url.Values{"from": {"2026-10-20"}, "to": {"2026-10-20"}}, nil)
	if export.StatusCode != http.StatusOK || !strings.HasSuffix(string(export.Body), "\n2026-10-20,priced,Priced,false,gpt-5-codex,2,0,400000,0,0,200000,0.6\n") {
		t.Fatalf("export = %d %s", export.StatusCode, export.Body)
	}
}

// A per-call model is billed only for the usage record whose TraceID the
// interceptor admitted on a generation route; result polling, replays and
// records without a trace id stay unmetered and are counted in health.
func TestPerCallBillingFollowsAdmittedTraceID(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	headers := bearer(testKeyPlain)
	if response := authenticate(t, app, http.MethodPost, "/v1/images/generations", headers, nil); !response.Authenticated {
		t.Fatalf("generation request denied: %+v", response)
	}
	if response := interceptBefore(t, app, "trace-img-1", "openai", headers, "/v1/images/generations"); response.Terminate {
		t.Fatalf("generation request terminated: %+v", response)
	}
	generation := UsageHandleRequest{TraceID: "trace-img-1", Provider: "xai", ExecutorType: "xaiexecutor", Model: "grok-imagine-image", APIKey: testKeyID}
	recordUsage(t, app, generation)
	recordUsage(t, app, generation)
	poll := generation
	poll.TraceID = "trace-img-2"
	recordUsage(t, app, poll)
	legacyHost := generation
	legacyHost.TraceID = ""
	recordUsage(t, app, legacyHost)

	models := keyUsageModels(t, app)
	if len(models) != 1 || models[0].Name != "grok-imagine-image" || models[0].BillingMode != "per_call" || !models[0].Priced || models[0].Daily.CallCount != 1 || !nearly(models[0].Daily.TotalUSD, 0.07) {
		t.Fatalf("per-call usage = %+v", models)
	}
	if h := health(t, app); h.UnmeteredPerCall != 3 {
		t.Fatalf("health = %+v, want 3 unmetered per-call records", h)
	}
	// Polling a result is admitted without a quota check and billed nothing.
	if response := interceptBefore(t, app, "trace-poll", "openai", headers, "/v1/videos/:request_id"); response.Terminate {
		t.Fatalf("result polling terminated: %+v", response)
	}
}
