package plugin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

const testChannel = "0123456789abcdef"

// interceptAfter sends request.intercept_after the way CPA does after it
// picked a credential.
func interceptAfter(t *testing.T, app *App, authIndex, requestPath string, body []byte) RequestInterceptResponse {
	t.Helper()
	var response RequestInterceptResponse
	decode(t, handle(t, app, MethodRequestInterceptAfter, map[string]any{
		"RequestID": "req-1", "TraceID": "a1b2c3d4", "SourceFormat": "openai-image", "ToFormat": "openai-image",
		"Model": "gpt-image-2", "RequestedModel": "gpt-image-2", "Stream": false, "Headers": http.Header{}, "Body": body,
		"Metadata":         map[string]any{"request_path": requestPath, "selected_auth_index": authIndex, "selected_auth_id": "codex:apikey:1"},
		"host_callback_id": "cb-1",
	}), &response)
	return response
}

func reconfigure(t *testing.T, app *App, extra string) Registration {
	t.Helper()
	config := "enabled: true\nstate_file: " + app.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n" + extra
	var registration Registration
	decode(t, handle(t, app, MethodPluginReconfigure, LifecycleRequest{ConfigYAML: []byte(config), SchemaVersion: SchemaVersion}), &registration)
	return registration
}

func TestAdapterRewritesOnlyItsChannel(t *testing.T) {
	app := newTestApp(t, appOptions{})
	var created struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"rule"`
	}
	decode(t, managementOK(t, app, http.MethodPost, "/adapters", nil, []byte(`{"channel":"`+testChannel+`","type":"edits_image_field","enabled":true}`)), &created)
	if created.Rule.ID == "" {
		t.Fatal("rule was not assigned an id")
	}

	edit := []byte(`{"model":"gpt-image-2","prompt":"cat","images":[{"image_url":"data:image/png;base64,AAAA"}]}`)
	rewritten := interceptAfter(t, app, testChannel, "/v1/images/edits", edit)
	var got map[string]any
	decode(t, rewritten.Body, &got)
	want := map[string]any{"model": "gpt-image-2", "prompt": "cat", "image": "data:image/png;base64,AAAA"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewritten body = %s", rewritten.Body)
	}
	for _, other := range []struct{ channel, path string }{{"fedcba9876543210", "/v1/images/edits"}, {testChannel, "/v1/responses"}} {
		if response := interceptAfter(t, app, other.channel, other.path, edit); len(response.Body) != 0 || response.Terminate {
			t.Fatalf("%s %s was rewritten: %+v", other.channel, other.path, response)
		}
	}

	var listed struct {
		Rules []struct {
			ID   string `json:"id"`
			Hits struct {
				Count int64 `json:"count"`
			} `json:"hits"`
		} `json:"rules"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/adapters", nil, nil), &listed)
	if len(listed.Rules) != 1 || listed.Rules[0].Hits.Count != 1 {
		t.Fatalf("rules = %+v", listed.Rules)
	}
}

func TestUsageFeedsChannelStatistics(t *testing.T) {
	app := newTestApp(t, appOptions{})
	recordUsage(t, app, UsageHandleRequest{AuthIndex: testChannel, AuthID: "codex:apikey:1", Provider: "codex", Model: "gpt-5-codex",
		APIKey: testKeyID, Latency: int64(2 * time.Second), Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100}})
	// Failed attempts and unknown downstream keys still describe the upstream.
	recordUsage(t, app, UsageHandleRequest{AuthIndex: testChannel, Provider: "codex", Model: "gpt-5-codex", APIKey: "someone-else",
		Failed: true, Failure: UsageFailure{StatusCode: 502, Body: "bad gateway, Authorization: Bearer sk-live-0123456789abcdef0123456789abcdef"}})

	var listed struct {
		Channels []struct {
			AuthIndex string `json:"auth_index"`
			Last24h   struct {
				Attempts    int64   `json:"attempts"`
				Failures    int64   `json:"failures"`
				ListCostUSD float64 `json:"list_cost_usd"`
			} `json:"last_24h"`
		} `json:"channels"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/channels", nil, nil), &listed)
	if len(listed.Channels) != 1 || listed.Channels[0].AuthIndex != testChannel {
		t.Fatalf("channels = %+v", listed.Channels)
	}
	day := listed.Channels[0].Last24h
	if day.Attempts != 2 || day.Failures != 1 || !nearly(day.ListCostUSD, 0.0011) {
		t.Fatalf("last 24h = %+v", day)
	}
	detail := managementOK(t, app, http.MethodGet, "/channels/detail", url.Values{"auth_index": {testChannel}}, nil)
	if !strings.Contains(string(detail), `"status_code":502`) || strings.Contains(string(detail), "sk-live") {
		t.Fatalf("detail = %s", detail)
	}

	// CPA's key lists drop the group names of its config; the list names
	// them by auth_index, without the keys.
	cpaConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cpaConfig, []byte("config-version: 8\napi-keys:\n  codex:\n    - name: \"codex-6\"\n      base-url: \"https://relay.example/v1\"\n      keys:\n        - api-key: \"sk-relay-1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconfigure(t, app, "cpa_config_path: "+cpaConfig+"\n")
	sum := sha256.Sum256([]byte("codex-api-key:https://relay.example/v1+sk-relay-1"))
	var named struct {
		Groups map[string]string `json:"groups"`
	}
	listing := managementOK(t, app, http.MethodGet, "/channels", nil, nil)
	decode(t, listing, &named)
	if named.Groups[hex.EncodeToString(sum[:8])] != "codex-6" || strings.Contains(string(listing), "sk-relay") {
		t.Fatalf("groups = %v", named.Groups)
	}
}

// A channel CPA's config no longer has can be deleted with its adapter
// rules; one still configured is refused, since traffic would bring it back.
func TestDeleteChannelForgetsWhatCPANoLongerHas(t *testing.T) {
	app := newTestApp(t, appOptions{})
	cpaConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cpaConfig, []byte("config-version: 8\napi-keys:\n  codex:\n    - name: \"codex-6\"\n      base-url: \"https://relay.example/v1\"\n      keys:\n        - api-key: \"sk-relay-1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconfigure(t, app, "cpa_config_path: "+cpaConfig+"\n")
	sum := sha256.Sum256([]byte("codex-api-key:https://relay.example/v1+sk-relay-1"))
	configured := hex.EncodeToString(sum[:8])
	for _, index := range []string{testChannel, configured} {
		recordUsage(t, app, UsageHandleRequest{AuthIndex: index, Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 10}})
	}
	managementOK(t, app, http.MethodPost, "/adapters", nil, []byte(`{"channel":"`+testChannel+`","type":"edits_image_field","enabled":true}`))

	if response := managementCall(t, app, http.MethodDelete, "/channels", url.Values{"auth_index": {configured}}, nil); response.StatusCode != http.StatusConflict || errorCode(t, response) != "channel_in_cpa" {
		t.Fatalf("delete of a configured channel = %d %s", response.StatusCode, response.Body)
	}
	var deleted struct {
		Deleted     string `json:"deleted"`
		RulesDelete int    `json:"adapter_rules_deleted"`
	}
	decode(t, managementOK(t, app, http.MethodDelete, "/channels", url.Values{"auth_index": {strings.ToUpper(testChannel)}}, nil), &deleted)
	if deleted.Deleted != testChannel || deleted.RulesDelete != 1 {
		t.Fatalf("deleted = %+v", deleted)
	}
	var listed struct {
		Channels []struct {
			AuthIndex string `json:"auth_index"`
		} `json:"channels"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/channels", nil, nil), &listed)
	if len(listed.Channels) != 1 || listed.Channels[0].AuthIndex != configured {
		t.Fatalf("channels after delete = %+v", listed.Channels)
	}
	if rules := managementOK(t, app, http.MethodGet, "/adapters", nil, nil); strings.Contains(string(rules), testChannel) {
		t.Fatalf("rules after delete = %s", rules)
	}
	if response := managementCall(t, app, http.MethodDelete, "/channels", url.Values{"auth_index": {testChannel}}, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d %s", response.StatusCode, response.Body)
	}
	if audit := managementOK(t, app, http.MethodGet, "/audit", url.Values{"action": {"delete_channel"}}, nil); !strings.Contains(string(audit), testChannel) {
		t.Fatalf("audit = %s", audit)
	}
}

func TestSearchRoutingFollowsTheSwitch(t *testing.T) {
	app := newTestApp(t, appOptions{})
	if registration := reconfigure(t, app, ""); registration.Capabilities.ModelRouter {
		t.Fatal("model_router declared while search is off")
	}
	managementOK(t, app, http.MethodPut, "/search", nil, []byte(`{"settings":{"target_model":"gpt-6-astra","keep_models":["gpt-*"],
		"channels":[{"name":"primary","model":"{model}"},{"name":"fallback","model":"alphasearch-nb-{model}"}]}}`))
	// alpha-search-model without a configuration block of its own: CPA
	// answers {} and the settings above stay.
	if response := managementCall(t, app, http.MethodPost, "/search/import", nil, []byte(`{"config":{"enabled":true}}`)); response.StatusCode != http.StatusConflict || errorCode(t, response) != "nothing_to_import" {
		t.Fatalf("import of an empty block = %d %s", response.StatusCode, response.Body)
	}
	route := func(sourceFormat, model string) map[string]any {
		var decision map[string]any
		decode(t, handle(t, app, MethodModelRoute, map[string]any{
			"Plugin": map[string]any{}, "PluginID": PluginID, "SourceFormat": sourceFormat, "RequestedModel": model,
			"Stream": false, "Headers": http.Header{}, "Body": []byte(`{"model":"` + model + `"}`), "Metadata": map[string]any{},
		}), &decision)
		return decision
	}
	if decision := route("codex-alpha-search", "claude-sonnet-5.5"); decision["Handled"] == true {
		t.Fatalf("routed while search is off: %v", decision)
	}

	if registration := reconfigure(t, app, "search_enabled: true\n"); !registration.Capabilities.ModelRouter {
		t.Fatal("model_router not declared after search_enabled")
	}
	if decision := route("codex-alpha-search", "claude-sonnet-5.5"); decision["Handled"] != true || decision["TargetModel"] != "gpt-6-astra" {
		t.Fatalf("alpha search decision = %v", decision)
	}
	if decision := route("openai-response", "claude-sonnet-5.5"); decision["Handled"] == true {
		t.Fatalf("chat request routed: %v", decision)
	}
	var state struct {
		Enabled  bool `json:"enabled"`
		Declared bool `json:"declared"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/search", nil, nil), &state)
	if !state.Enabled || !state.Declared {
		t.Fatalf("search state = %+v", state)
	}
}

func TestReconfigureKeepsTheLastValidConfiguration(t *testing.T) {
	app := newTestApp(t, appOptions{})
	if body := managementOK(t, app, http.MethodGet, "/settings", nil, nil); !bytes.Contains(body, []byte(`"config_warnings":[]`)) {
		t.Fatalf("settings without warnings = %s", body)
	}
	reconfigure(t, app, "bogus_setting: 1\n")
	// A block the key store cannot apply keeps the loaded dataset serving.
	raw, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte("state_file: [1, 2]\n"), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	// CPA sends the same block again on every reload of its config file.
	for range 2 {
		if _, err := app.HandleMethod(MethodPluginReconfigure, raw); err != nil {
			t.Fatalf("reconfigure with a broken block: %v", err)
		}
	}
	if response := authenticate(t, app, http.MethodPost, "/v1/responses", bearer(testKeyPlain), nil); !response.Authenticated {
		t.Fatal("key stopped authenticating after a rejected configuration")
	}
	var settings struct {
		Warnings []string `json:"config_warnings"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/settings", nil, nil), &settings)
	joined := strings.Join(settings.Warnings, "\n")
	if !strings.Contains(joined, "bogus_setting") || strings.Count(joined, "last valid one") != 1 {
		t.Fatalf("warnings = %q", settings.Warnings)
	}
	reconfigure(t, app, "")
	if body := managementOK(t, app, http.MethodGet, "/settings", nil, nil); !bytes.Contains(body, []byte(`"config_warnings":[]`)) {
		t.Fatalf("settings after a valid configuration = %s", body)
	}

	// A first registration that cannot load still fails, closed.
	fresh := NewApp()
	defer fresh.Shutdown()
	if _, err := fresh.HandleMethod(MethodPluginRegister, raw); err == nil {
		t.Fatal("first registration with a broken block succeeded")
	}
}

// Billing that cannot be saved stays only in memory: the landing page says so
// until a write succeeds again.
func TestOverviewWarnsWhileTheLedgerIsNotSaved(t *testing.T) {
	app := newTestApp(t, appOptions{})
	billOneAttempt(t, app)
	dir := filepath.Dir(app.Store().StatePath())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := app.Store().FlushUsage(); err == nil {
		t.Skip("the data directory stays writable (running as root?)")
	}
	warnings := func() string {
		var overview struct {
			Warnings []string `json:"warnings"`
		}
		decode(t, managementOK(t, app, http.MethodGet, "/overview", nil, nil), &overview)
		return strings.Join(overview.Warnings, "\n")
	}
	if got := warnings(); !strings.Contains(got, "usage ledger not saved (1 failed writes, last success ") || !strings.Contains(got, "write live usage") {
		t.Fatalf("overview warnings while unsaved = %q", got)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := app.Store().FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if got := warnings(); got != "" {
		t.Fatalf("overview warnings once saved = %q", got)
	}
}

// Concurrent edits of one channel: each audit line records the vendor its
// patch replaced, so the lines chain from the first to the last.
func TestConcurrentChannelPatchesAuditWhatTheyReplaced(t *testing.T) {
	app := newTestApp(t, appOptions{})
	const patches = 12
	errs := make(chan error, patches)
	for i := range patches {
		request, err := json.Marshal(ManagementRequest{Method: http.MethodPatch, Path: managementBase + "/channels",
			Body: []byte(`{"auth_index":"` + testChannel + `","vendor":"v` + strconv.Itoa(i) + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, err := app.HandleMethod(MethodManagementHandle, request)
			errs <- err
		}()
	}
	for range patches {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var audit struct {
		Events []struct {
			Changes map[string]struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"changes"`
		} `json:"events"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/audit", url.Values{"action": {"update_channel"}}, nil), &audit)
	previous := ""
	for i := len(audit.Events) - 1; i >= 0; i-- { // oldest first
		vendor, ok := audit.Events[i].Changes["vendor"]
		if !ok || vendor.From != previous {
			t.Fatalf("audit line %d = %+v after vendor %q", len(audit.Events)-i, audit.Events[i].Changes, previous)
		}
		previous = vendor.To
	}
	if len(audit.Events) != patches {
		t.Fatalf("%d audit lines for %d patches", len(audit.Events), patches)
	}
}

func TestQuiesceStopsModuleWritesUntilReconfigured(t *testing.T) {
	app := newTestApp(t, appOptions{})
	patch := []byte(`{"auth_index":"` + testChannel + `","display_name":"Primary"}`)
	handle(t, app, MethodPluginQuiesce, map[string]any{})
	if response := managementCall(t, app, http.MethodPatch, "/channels", nil, patch); response.StatusCode != http.StatusServiceUnavailable || errorCode(t, response) != "upgrading" {
		t.Fatalf("write while quiesced = %d %s", response.StatusCode, response.Body)
	}
	// CPA rolls a failed replacement back with a reconfigure.
	reconfigure(t, app, "")
	managementOK(t, app, http.MethodPatch, "/channels", nil, patch)
}

// A hot replacement hands the channel statistics over with the ledger: the
// newer instance counts and bills every attempt once, also one that reached
// the quiesced instance before CPA switched and a straggler after.
func TestHotReplacementCountsEachAttemptOnce(t *testing.T) {
	old := newTestApp(t, appOptions{})
	attempt := func(app *App) {
		t.Helper()
		recordUsage(t, app, UsageHandleRequest{AuthIndex: testChannel, Provider: "codex", Model: "gpt-5-codex",
			APIKey: testKeyID, Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100}})
	}
	attempt(old) // in the statistics written at quiesce
	handle(t, old, MethodPluginQuiesce, map[string]any{})
	attempt(old) // before CPA switches: journaled

	newer := NewApp()
	t.Cleanup(newer.Shutdown)
	register(t, newer, "enabled: true\nstate_file: "+old.Store().StatePath()+"\nusage_timezone: Asia/Shanghai\n")
	attempt(old) // a straggler, read from the journal the newer instance follows
	attempt(newer)

	counted := func() (int64, float64) {
		var channels struct {
			Channels []struct {
				Last24h struct {
					Attempts int64 `json:"attempts"`
				} `json:"last_24h"`
			} `json:"channels"`
		}
		var keys struct {
			Keys []struct {
				Usage struct {
					DailyUSD float64 `json:"daily_usd"`
				} `json:"usage"`
			} `json:"keys"`
		}
		decode(t, managementOK(t, newer, http.MethodGet, "/channels", nil, nil), &channels)
		decode(t, managementOK(t, newer, http.MethodGet, "/keys", nil, nil), &keys)
		if len(channels.Channels) != 1 || len(keys.Keys) != 1 {
			return 0, 0
		}
		return channels.Channels[0].Last24h.Attempts, keys.Keys[0].Usage.DailyUSD
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if attempts, _ := counted(); attempts >= 4 {
			break
		}
	}
	if attempts, billed := counted(); attempts != 4 || !nearly(billed, 4*0.0011) {
		t.Fatalf("newer instance counts %d attempts and bills $%v, want 4 and $%v", attempts, billed, 4*0.0011)
	}
}

func lifecycleRequest(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte(configYAML), SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A quiesce that cannot hand off keeps the data directory: the newer
// instance cannot register, so CPA rolls the replacement back, and this
// instance never stops serving. Once the journal can be written, the hand-off
// goes through and the newer instance bills what this one billed.
func TestFailedHandoffKeepsTheDataDirectory(t *testing.T) {
	old := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + old.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	journal := filepath.Join(filepath.Dir(old.Store().StatePath()), "cpa-key-policy-handoff.jsonl")
	if err := os.Mkdir(journal, 0o700); err != nil { // the journal cannot be opened
		t.Fatal(err)
	}
	raw, err := old.HandleMethod(MethodPluginQuiesce, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	decode(t, raw, &env)
	if env.OK || env.Error == nil || env.Error.Code != "handoff_failed" {
		t.Fatalf("quiesce without a journal = %s", raw)
	}
	managementOK(t, old, http.MethodPatch, "/channels", nil, []byte(`{"auth_index":"`+testChannel+`","display_name":"Primary"}`))
	fresh := NewApp()
	defer fresh.Shutdown()
	if _, err := fresh.HandleMethod(MethodPluginRegister, lifecycleRequest(t, config)); !errors.Is(err, persist.ErrLocked) {
		t.Fatalf("newer instance registration = %v, want the data directory locked", err)
	}
	reconfigure(t, old, "") // CPA's rollback
	recordUsage(t, old, UsageHandleRequest{AuthIndex: testChannel, Provider: "codex", Model: "gpt-5-codex",
		APIKey: testKeyID, Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100}})

	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	handle(t, old, MethodPluginQuiesce, map[string]any{})
	newer := NewApp()
	t.Cleanup(newer.Shutdown)
	register(t, newer, config)
	var keys struct {
		Keys []struct {
			Usage struct {
				DailyUSD float64 `json:"daily_usd"`
			} `json:"usage"`
		} `json:"keys"`
	}
	var channels struct {
		Channels []struct {
			Meta struct {
				DisplayName string `json:"display_name"`
			} `json:"meta"`
		} `json:"channels"`
	}
	decode(t, managementOK(t, newer, http.MethodGet, "/keys", nil, nil), &keys)
	decode(t, managementOK(t, newer, http.MethodGet, "/channels", nil, nil), &channels)
	if len(keys.Keys) != 1 || !nearly(keys.Keys[0].Usage.DailyUSD, 0.0011) || len(channels.Channels) != 1 || channels.Channels[0].Meta.DisplayName != "Primary" {
		t.Fatalf("newer instance has keys %+v and channels %+v", keys.Keys, channels.Channels)
	}
	// Having handed off, the older instance has nothing to hand off again.
	handle(t, old, MethodPluginQuiesce, map[string]any{})
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second quiesce wrote into the newer instance's directory: %v", err)
	}
}

// A management write under way when CPA quiesces the plugin finishes before
// the newer instance can take the data directory; a later write is refused.
func TestQuiesceWaitsForWritesUnderWay(t *testing.T) {
	old := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + old.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	old.writeMu.RLock() // what handleManagement holds through a write
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = old.HandleMethod(MethodPluginQuiesce, []byte(`{}`))
	}()
	select {
	case <-done:
		t.Fatal("quiesce finished while a management write was under way")
	case <-time.After(200 * time.Millisecond):
	}
	early := NewApp()
	defer early.Shutdown()
	if _, err := early.HandleMethod(MethodPluginRegister, lifecycleRequest(t, config)); !errors.Is(err, persist.ErrLocked) {
		t.Fatalf("newer instance registration during a write = %v, want the data directory locked", err)
	}
	old.writeMu.RUnlock()
	<-done

	newer := NewApp()
	t.Cleanup(newer.Shutdown)
	register(t, newer, config)
	if response := managementCall(t, old, http.MethodPost, "/keys", nil, []byte(`{"id":"late"}`)); response.StatusCode != http.StatusServiceUnavailable || errorCode(t, response) != "upgrading" {
		t.Fatalf("write after quiesce = %d %s", response.StatusCode, response.Body)
	}
}

// Pointing state_file at another directory moves the lock only once the
// dataset there has loaded: a directory that fails to load stays free, and
// the one in use stays locked and written.
func TestStateFileMoveKeepsTheLockUntilLoaded(t *testing.T) {
	app := newTestApp(t, appOptions{})
	current := app.Store().StatePath()
	locked := func(statePath string) bool {
		t.Helper()
		lock, err := persist.TryLock(persist.LockPath(statePath))
		if err != nil {
			return true
		}
		_ = lock.Release()
		return false
	}
	moveTo := func(statePath string) {
		t.Helper()
		handle(t, app, MethodPluginReconfigure, lifecycleRequest(t, "enabled: true\nstate_file: "+statePath+"\nusage_timezone: Asia/Shanghai\n"))
	}

	broken := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(persist.UsagePath(broken), []byte("{}"), 0o600); err != nil { // usage without its state file
		t.Fatal(err)
	}
	moveTo(broken)
	if !locked(current) || locked(broken) {
		t.Fatalf("after a failed move: current locked=%v, failed directory locked=%v", locked(current), locked(broken))
	}
	if response := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(`{"id":"after-failed-move"}`)); response.StatusCode != http.StatusCreated {
		t.Fatalf("key write after a failed move = %d %s", response.StatusCode, response.Body)
	}
	state, err := policy.LoadState(current)
	if err != nil || !slices.ContainsFunc(state.Keys, func(key policy.KeyConfig) bool { return key.ID == "after-failed-move" }) {
		t.Fatalf("state file in use = %+v, %v", state, err)
	}

	moved := filepath.Join(t.TempDir(), "state.json")
	moveTo(moved)
	if locked(current) || !locked(moved) {
		t.Fatalf("after a move: previous locked=%v, new locked=%v", locked(current), locked(moved))
	}
}

func TestIndexPageRevalidates(t *testing.T) {
	app := newTestApp(t, appOptions{})
	get := func(headers http.Header) ManagementResponse {
		var response ManagementResponse
		decode(t, handle(t, app, MethodManagementHandle, ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + PluginID + "/index.html", Headers: headers}), &response)
		return response
	}
	first := get(nil)
	etag := first.Headers.Get("Etag")
	if first.StatusCode != http.StatusOK || etag == "" || len(first.Body) == 0 {
		t.Fatalf("first load = %d etag=%q", first.StatusCode, etag)
	}
	if again := get(http.Header{"If-None-Match": {etag}}); again.StatusCode != http.StatusNotModified || len(again.Body) != 0 {
		t.Fatalf("revalidation = %d with %d bytes", again.StatusCode, len(again.Body))
	}
	// The compressed page survives the management envelope: CPA writes the
	// body and headers it is given.
	zipped := get(http.Header{"Accept-Encoding": {"gzip, deflate, br"}})
	if zipped.StatusCode != http.StatusOK || zipped.Headers.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip load = %d %v", zipped.StatusCode, zipped.Headers)
	}
	reader, err := gzip.NewReader(bytes.NewReader(zipped.Body))
	if err != nil {
		t.Fatal(err)
	}
	if page, err := io.ReadAll(reader); err != nil || !bytes.Equal(page, first.Body) {
		t.Fatalf("gzip body does not unpack to the page: %v", err)
	}
	if again := get(http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {"W/" + zipped.Headers.Get("Etag")}}); again.StatusCode != http.StatusNotModified || len(again.Body) != 0 {
		t.Fatalf("gzip revalidation = %d with %d bytes", again.StatusCode, len(again.Body))
	}
}

// fakeHost answers host.model.execute and keeps the last request.
type fakeHost struct {
	method  string
	request map[string]any
}

func (h *fakeHost) Call(method string, request []byte) ([]byte, error) {
	if method != "host.model.execute" {
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	h.method = method
	_ = json.Unmarshal(request, &h.request)
	return []byte(`{"ok":true,"result":{"status_code":200,"body":"eyJkYXRhIjpbXX0="}}`), nil
}

func TestTryAdapterSendsTheRewrittenBodyPinned(t *testing.T) {
	app := newTestApp(t, appOptions{})
	host := &fakeHost{}
	app.SetHost(host)
	var created struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"rule"`
	}
	decode(t, managementOK(t, app, http.MethodPost, "/adapters", nil, []byte(`{"channel":"`+testChannel+`","type":"edits_image_field","enabled":true}`)), &created)
	try := []byte(`{"rule_id":"` + created.Rule.ID + `","body":{"model":"gpt-image-2","prompt":"cat","images":[{"image_url":"data:x"}]}}`)
	if response := managementCall(t, app, http.MethodPost, "/adapters/try", nil, try); response.StatusCode != http.StatusConflict || errorCode(t, response) != "channel_not_seen" {
		t.Fatalf("try before any traffic = %d %s", response.StatusCode, response.Body)
	}
	recordUsage(t, app, UsageHandleRequest{AuthIndex: testChannel, AuthID: "codex:apikey:pinned", Provider: "codex", Model: "gpt-image-2", APIKey: testKeyID})

	var result struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	// CPA sends the management call's host callback id; passed on, it lets a
	// dropped browser connection cancel the upstream call.
	var response ManagementResponse
	decode(t, handle(t, app, MethodManagementHandle, ManagementRequest{Method: http.MethodPost, Path: managementBase + "/adapters/try", Body: try, HostCallbackID: "cb-mgmt"}), &response)
	decode(t, response.Body, &result)
	if response.StatusCode != http.StatusOK || result.StatusCode != http.StatusOK || result.Body != `{"data":[]}` {
		t.Fatalf("try result = %d %+v", response.StatusCode, result)
	}
	if host.request["auth_id"] != "codex:apikey:pinned" || host.request["path"] != "/v1/images/edits" || host.request["entry_protocol"] != "openai-image" || host.request["host_callback_id"] != "cb-mgmt" {
		t.Fatalf("host request = %v", host.request)
	}
	sent, _ := base64.StdEncoding.DecodeString(host.request["body"].(string))
	if !strings.Contains(string(sent), `"image":"data:x"`) || strings.Contains(string(sent), `"images"`) {
		t.Fatalf("sent body = %s", sent)
	}

	// A rule on Codex CLI's direct route is sent as a Responses request.
	decode(t, managementOK(t, app, http.MethodPost, "/adapters", nil, []byte(`{"channel":"`+testChannel+`","type":"set_field","paths":["/backend-api/codex/responses"],"field":"store","value":false,"enabled":true}`)), &created)
	managementOK(t, app, http.MethodPost, "/adapters/try", nil, []byte(`{"rule_id":"`+created.Rule.ID+`","body":{"model":"gpt-5-codex","input":"hi"}}`))
	if host.request["path"] != "/backend-api/codex/responses" || host.request["entry_protocol"] != "openai-response" {
		t.Fatalf("direct route host request = %v", host.request)
	}

	// An API key of CPA's config.yaml is pinned before any traffic by the ID
	// CPA derives for it (CPA v8.0.13's ID for legacy-1, as in channel_test.go).
	cpaConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cpaConfig, []byte("api-keys: [client-key]\nproxy-url: socks5h://legacy-proxy.example:1080\ncodex-api-key:\n  - &codex\n    api-key: legacy-1\n    base-url: https://legacy.example/v1\n    prefix: team\n  - <<: *codex\n    api-key: legacy-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconfigure(t, app, "cpa_config_path: "+cpaConfig+"\n")
	sum := sha256.Sum256([]byte("codex-api-key:https://legacy.example/v1+legacy-1"))
	decode(t, managementOK(t, app, http.MethodPost, "/adapters", nil, []byte(`{"channel":"`+hex.EncodeToString(sum[:8])+`","type":"edits_image_field","enabled":true}`)), &created)
	managementOK(t, app, http.MethodPost, "/adapters/try", nil, []byte(`{"rule_id":"`+created.Rule.ID+`","body":{"model":"gpt-image-2","prompt":"cat","images":[{"image_url":"data:x"}]}}`))
	if host.request["auth_id"] != "codex:apikey:4dc376d49e61" {
		t.Fatalf("config key host request = %v", host.request)
	}
}

// billOneAttempt records one $0.0011 attempt of the test key on testChannel.
func billOneAttempt(t *testing.T, app *App) {
	t.Helper()
	recordUsage(t, app, UsageHandleRequest{AuthIndex: testChannel, Provider: "codex", Model: "gpt-5-codex",
		APIKey: testKeyID, Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100}})
}

// servedTotals is what app serves: the test key's daily usage, the key ids
// and the attempts counted on all channels in the last 24 hours.
func servedTotals(t *testing.T, app *App) (float64, []string, int64) {
	t.Helper()
	var keys struct {
		Keys []struct {
			ID    string `json:"id"`
			Usage struct {
				DailyUSD float64 `json:"daily_usd"`
			} `json:"usage"`
		} `json:"keys"`
	}
	var channels struct {
		Channels []struct {
			Last24h struct {
				Attempts int64 `json:"attempts"`
			} `json:"last_24h"`
		} `json:"channels"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/keys", nil, nil), &keys)
	decode(t, managementOK(t, app, http.MethodGet, "/channels", nil, nil), &channels)
	daily, ids, attempts := 0.0, []string{}, int64(0)
	for _, key := range keys.Keys {
		ids = append(ids, key.ID)
		if key.ID == testKeyID {
			daily = key.Usage.DailyUSD
		}
	}
	for _, c := range channels.Channels {
		attempts += c.Last24h.Attempts
	}
	return daily, ids, attempts
}

// CPA HEAD maps a library whose bytes already ran in the process to the
// resident image, so a hot switch back to an earlier build registers the
// App that served before. It serves and writes what the newer build left in
// the files and its journal, not what it held in memory.
func TestResidentRuntimeRegisteredAgainServesTheFiles(t *testing.T) {
	a := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + a.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	billOneAttempt(t, a)
	var revoked, made struct {
		PlainKey string `json:"plain_key"`
	}
	decode(t, managementCall(t, a, http.MethodPost, "/keys", nil, []byte(`{"id":"revoked"}`)).Body, &revoked)
	handle(t, a, MethodPluginQuiesce, map[string]any{})

	b := NewApp()
	t.Cleanup(b.Shutdown)
	register(t, b, config)
	managementOK(t, b, http.MethodDelete, "/keys", url.Values{"id": {"revoked"}}, nil)
	billOneAttempt(t, b)
	decode(t, managementCall(t, b, http.MethodPost, "/keys", nil, []byte(`{"id":"made-on-b"}`)).Body, &made)
	handle(t, b, MethodPluginQuiesce, map[string]any{})
	billOneAttempt(t, b) // a straggler on b, journaled

	register(t, a, config) // the switch back to a's bytes
	if response := authenticate(t, a, http.MethodPost, "/v1/responses", bearer(made.PlainKey), nil); !response.Authenticated {
		t.Error("a key created on the newer build does not authenticate")
	}
	if response := authenticate(t, a, http.MethodPost, "/v1/responses", bearer(revoked.PlainKey), nil); response.Authenticated {
		t.Error("a key deleted on the newer build authenticates again")
	}
	if daily, _, attempts := servedTotals(t, a); !nearly(daily, 3*0.0011) || attempts != 3 {
		t.Errorf("served $%v and %d attempts, want $%v and 3", daily, attempts, 3*0.0011)
	}
	billOneAttempt(t, a)
	a.Shutdown()

	fresh := NewApp()
	t.Cleanup(fresh.Shutdown)
	register(t, fresh, config)
	if daily, ids, attempts := servedTotals(t, fresh); !nearly(daily, 4*0.0011) || attempts != 4 || !slices.Equal(ids, []string{"made-on-b", "priced"}) {
		t.Fatalf("files hold keys %v, $%v and %d attempts; want [made-on-b priced], $%v and 4", ids, daily, attempts, 4*0.0011)
	}
}

// The same bytes as the running build: CPA quiesces it and registers the
// same App again. Every record is billed and counted once.
func TestRuntimeReplacedByItselfBillsOnce(t *testing.T) {
	a := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + a.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	billOneAttempt(t, a)
	handle(t, a, MethodPluginQuiesce, map[string]any{})
	billOneAttempt(t, a) // journaled
	register(t, a, config)
	billOneAttempt(t, a)
	a.Shutdown()
	fresh := NewApp()
	t.Cleanup(fresh.Shutdown)
	register(t, fresh, config)
	if daily, _, attempts := servedTotals(t, fresh); !nearly(daily, 3*0.0011) || attempts != 3 {
		t.Fatalf("billed $%v and counted %d attempts, want $%v and 3", daily, attempts, 3*0.0011)
	}
}

// rollBackSelfReplacement plays a hot replacement by the same bytes that CPA
// HEAD discards: the quiesced App is registered again (with a usage_timezone
// the files reject when fail is set), shut down as the discarded load, then
// reconfigured as the instance CPA rolls back to. One record is billed before
// the quiesce and one after it.
func rollBackSelfReplacement(t *testing.T, a *App, fail bool) {
	t.Helper()
	config := "enabled: true\nstate_file: " + a.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	billOneAttempt(t, a)
	handle(t, a, MethodPluginQuiesce, map[string]any{})
	billOneAttempt(t, a) // in memory and in the journal
	if fail {
		if _, err := a.HandleMethod(MethodPluginRegister, lifecycleRequest(t, strings.Replace(config, "Asia/Shanghai", "UTC", 1))); err == nil {
			t.Fatal("a register with another timezone succeeded")
		}
	} else {
		register(t, a, config)
	}
	a.Shutdown()          // the discarded load
	reconfigure(t, a, "") // the rollback
}

// After the rollback the instance serves on: detection runs, the journal of
// the handoff that did not happen is gone, and every record is billed once.
func TestDiscardedSelfReplacementServesOnAndBillsOnce(t *testing.T) {
	for _, fail := range []bool{true, false} {
		a := newTestApp(t, appOptions{})
		host := newDetectionHost()
		close(host.release)
		a.SetHost(host)
		config := "enabled: true\nstate_file: " + a.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
		rollBackSelfReplacement(t, a, fail)
		if _, err := os.Stat(filepath.Join(filepath.Dir(a.Store().StatePath()), "cpa-key-policy-handoff.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("register fails=%v: the journal is still there after the rollback (%v)", fail, err)
		}
		if response := managementCall(t, a, http.MethodPost, "/quality/run", nil, []byte(candyRun)); response.StatusCode != http.StatusOK {
			t.Errorf("register fails=%v: detection after the rollback = %d %s", fail, response.StatusCode, response.Body)
		}
		billOneAttempt(t, a)
		a.Shutdown()
		fresh := NewApp()
		register(t, fresh, config)
		if daily, _, attempts := servedTotals(t, fresh); !nearly(daily, 3*0.0011) || attempts != 3 {
			t.Errorf("register fails=%v: billed $%v and counted %d attempts after a restart, want $%v and 3", fail, daily, attempts, 3*0.0011)
		}
		fresh.Shutdown()
	}
}

// The next ordinary replacement after such a rollback hands over only what
// came after its own quiesce.
func TestRolledBackJournalIsNotReplayedByTheNextReplacement(t *testing.T) {
	a := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + a.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	rollBackSelfReplacement(t, a, true)
	billOneAttempt(t, a)
	handle(t, a, MethodPluginQuiesce, map[string]any{})
	b := NewApp()
	t.Cleanup(b.Shutdown)
	register(t, b, config)
	if daily, _, attempts := servedTotals(t, b); !nearly(daily, 3*0.0011) || attempts != 3 {
		t.Fatalf("the newer build serves $%v and %d attempts, want $%v and 3", daily, attempts, 3*0.0011)
	}
}

// Disabled, then another build ran and closed a day, then the first build is
// enabled again with a configuration the files reject: the registration
// fails, and an instance that does not own the files writes nothing, then or
// at shutdown.
func TestStaleRegisterThatCannotLoadWritesNothing(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Pointer[time.Time]
	day := func(d int) { at := time.Date(2026, 10, d, 10, 0, 0, 0, loc); now.Store(&at) }
	clock := func() time.Time { return *now.Load() }
	day(20)
	a := newTestApp(t, appOptions{Clock: clock})
	statePath := a.Store().StatePath()
	billOneAttempt(t, a)
	a.Shutdown() // disabled
	day(21)
	b := NewApp()
	b.Store().SetClock(clock)
	register(t, b, "enabled: true\nstate_file: "+statePath+"\nusage_timezone: Asia/Shanghai\n")
	billOneAttempt(t, b)
	day(22)
	b.Shutdown()
	history, live := persist.HistoryPath(statePath), persist.UsagePath(statePath)
	historyBefore, _ := os.ReadFile(history)
	liveBefore, _ := os.ReadFile(live)

	day(23)
	if _, err := a.HandleMethod(MethodPluginRegister, lifecycleRequest(t, "enabled: true\nstate_file: "+statePath+"\nusage_timezone: UTC\n")); err == nil {
		t.Error("a registration whose files do not load succeeded")
	}
	a.Shutdown()
	historyAfter, _ := os.ReadFile(history)
	liveAfter, _ := os.ReadFile(live)
	if !bytes.Equal(historyBefore, historyAfter) || !bytes.Equal(liveBefore, liveAfter) {
		t.Fatal("an instance that does not own the data directory wrote the usage files")
	}
}

// detectionHost answers the detection callbacks for one credential on
// testChannel. A model stream read waits for release; once closed, every
// callback is refused the way CPA does after it closed the plugin's
// callback instance, which it does before calling shutdown: an error
// envelope with a zero status, so the host table call returns no error.
type detectionHost struct {
	closed  atomic.Bool
	reading chan struct{}
	release chan struct{}
}

func newDetectionHost() *detectionHost {
	return &detectionHost{reading: make(chan struct{}, 1), release: make(chan struct{})}
}

func (h *detectionHost) Call(method string, request []byte) ([]byte, error) {
	if h.closed.Load() {
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"host plugin callback instance is closed"}}`), nil
	}
	switch method {
	case "host.auth.list":
		return []byte(`{"ok":true,"result":{"files":[{"id":"codex-a.json","auth_index":"` + testChannel + `","name":"a","provider":"codex","source":"file","path":"/auths/a.json","plan_type":"plus"}]}}`), nil
	case "host.model.execute_stream":
		return []byte(`{"ok":true,"result":{"stream_id":"1"}}`), nil
	case "host.model.stream_read":
		select {
		case h.reading <- struct{}{}:
		default:
		}
		<-h.release
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"stream gone"}}`), errors.New("stream gone")
	default:
		return []byte(`{"ok":true,"result":{}}`), nil
	}
}

const candyRun = `{"test":"candy","auth_ids":["codex-a.json"],"model":"gpt-5","runs":1}`

// At shutdown a detection test can wait for the upstream's next chunk, since
// CPA no longer lets the plugin close the stream: the final accounting cut
// and the channel statistics are written before the test is waited for.
func TestShutdownWritesBeforeWaitingForDetection(t *testing.T) {
	app := newTestApp(t, appOptions{})
	host := newDetectionHost()
	app.SetHost(host)
	managementOK(t, app, http.MethodPost, "/quality/run", nil, []byte(candyRun))
	select {
	case <-host.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the detection test never read its stream")
	}
	dir := filepath.Dir(app.Store().StatePath())
	files := []string{persist.UsagePath(app.Store().StatePath()), filepath.Join(dir, channelStatsFile)}
	read := func() [][]byte {
		out := [][]byte{}
		for _, path := range files {
			raw, _ := os.ReadFile(path)
			out = append(out, raw)
		}
		return out
	}
	before := read()
	billOneAttempt(t, app)
	host.closed.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); app.Shutdown() }()
	written := func() bool {
		now := read()
		return !bytes.Equal(before[0], now[0]) && !bytes.Equal(before[1], now[1])
	}
	deadline := time.Now().Add(5 * time.Second)
	for !written() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ok := written()
	close(host.release)
	<-done
	if !ok {
		t.Fatal("usage or channel statistics waited for a detection stream CPA no longer lets the plugin close")
	}
}

// A newer instance shut down while it still follows the journal applies the
// remaining lines first; the channel statistics it writes include them.
func TestShutdownKeepsTheJournalTailInChannelStats(t *testing.T) {
	old := newTestApp(t, appOptions{})
	config := "enabled: true\nstate_file: " + old.Store().StatePath() + "\nusage_timezone: Asia/Shanghai\n"
	handle(t, old, MethodPluginQuiesce, map[string]any{})
	newer := NewApp()
	register(t, newer, config)
	billOneAttempt(t, old) // a straggler, read by the follower's last pass
	newer.Shutdown()
	fresh := NewApp()
	t.Cleanup(fresh.Shutdown)
	register(t, fresh, config)
	if daily, _, attempts := servedTotals(t, fresh); !nearly(daily, 0.0011) || attempts != 1 {
		t.Fatalf("after restart: billed $%v and counted %d attempts, want $0.0011 and 1", daily, attempts)
	}
}

// Detection runs and inspection rounds spend the credentials' quota, and a
// clear destroys records: they are audited, as is a schedule change.
func TestDetectionRunsAndClearsAreAudited(t *testing.T) {
	app := newTestApp(t, appOptions{})
	host := newDetectionHost()
	close(host.release) // every stream ends at once
	app.SetHost(host)
	managementOK(t, app, http.MethodPost, "/quality/run", nil, []byte(candyRun))
	managementOK(t, app, http.MethodPut, "/quality/inspection", nil, []byte(`{"times":["08:00"],"runs":1,"groups":[{"model":"gpt-5","channels":["0123456789abcdef"]}]}`))
	managementOK(t, app, http.MethodPost, "/quality/inspection", nil, nil)
	managementOK(t, app, http.MethodDelete, "/quality/results", url.Values{"test": {"all"}}, nil)
	for action, target := range map[string]string{"run_quality": "candy", "update_inspection": "inspection", "start_inspection": "inspection", "clear_quality": "all"} {
		var audit struct {
			Events []struct {
				Target  string                     `json:"target"`
				Changes map[string]json.RawMessage `json:"changes"`
			} `json:"events"`
		}
		decode(t, managementOK(t, app, http.MethodGet, "/audit", url.Values{"action": {action}}, nil), &audit)
		if len(audit.Events) != 1 || audit.Events[0].Target != target {
			t.Errorf("%s audit = %+v", action, audit.Events)
		}
	}
}

// A rollback that cannot take the data directory back, held by another
// process, leaves the instance read-only: writes say so, as does the
// settings page, until a later config apply gets the lock.
func TestRollbackWithoutTheDirectoryIsReadOnly(t *testing.T) {
	app := newTestApp(t, appOptions{})
	handle(t, app, MethodPluginQuiesce, map[string]any{})
	lock, err := persist.TryLock(persist.LockPath(app.Store().StatePath()))
	if err != nil {
		t.Fatal(err)
	}
	reconfigure(t, app, "") // CPA's rollback
	patch := []byte(`{"id":"priced","rpm":3}`)
	warnings := func() string {
		var settings struct {
			Warnings []string `json:"config_warnings"`
		}
		decode(t, managementOK(t, app, http.MethodGet, "/settings", nil, nil), &settings)
		return strings.Join(settings.Warnings, "\n")
	}
	if response := managementCall(t, app, http.MethodPatch, "/keys", nil, patch); response.StatusCode != http.StatusServiceUnavailable || errorCode(t, response) != "upgrading" || !strings.Contains(string(response.Body), "read-only") {
		t.Fatalf("write while read-only = %d %s", response.StatusCode, response.Body)
	}
	if got := warnings(); !strings.Contains(got, "cannot take the data directory back") {
		t.Fatalf("warnings = %q", got)
	}
	_ = lock.Release()
	reconfigure(t, app, "")
	managementOK(t, app, http.MethodPatch, "/keys", nil, patch)
	if got := warnings(); got != "" {
		t.Fatalf("warnings after the lock came back = %q", got)
	}
}

// openingHost is a detectionHost on which CPA is still opening the model
// stream: execute_stream answers only once open is closed, whatever the test
// cancelled meanwhile.
type openingHost struct {
	*detectionHost
	opening chan struct{}
	open    chan struct{}
}

func (h openingHost) Call(method string, request []byte) ([]byte, error) {
	if method == "host.model.execute_stream" {
		select {
		case h.opening <- struct{}{}:
		default:
		}
		<-h.open
	}
	return h.detectionHost.Call(method, request)
}

// A hot replacement by the same bytes registers the quiesced App again while
// a detection test it could not cut short still waits on CPA: the register,
// which CPA applies no configuration until, does not wait for that test, and
// what the test records later is never saved over the reopened history.
func TestStaleRegisterDoesNotWaitForDetection(t *testing.T) {
	limit := qualityHandOffLimit
	qualityHandOffLimit = 50 * time.Millisecond
	t.Cleanup(func() { qualityHandOffLimit = limit })
	app := newTestApp(t, appOptions{})
	host := openingHost{detectionHost: newDetectionHost(), opening: make(chan struct{}, 1), open: make(chan struct{})}
	close(host.release)
	app.SetHost(host)
	managementOK(t, app, http.MethodPost, "/quality/run", nil, []byte(candyRun))
	select {
	case <-host.opening:
	case <-time.After(5 * time.Second):
		t.Fatal("the detection test never opened its stream")
	}
	handle(t, app, MethodPluginQuiesce, map[string]any{})
	replaced := app.modules.Load()
	history := filepath.Join(filepath.Dir(app.Store().StatePath()), qualityFile)
	before, _ := os.ReadFile(history)

	request := lifecycleRequest(t, "enabled: true\nstate_file: "+app.Store().StatePath()+"\nusage_timezone: Asia/Shanghai\n")
	registered := make(chan error, 1)
	go func() {
		_, err := app.HandleMethod(MethodPluginRegister, request)
		registered <- err
	}()
	select {
	case err := <-registered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(host.open)
		<-registered
		t.Fatal("the register waited for a detection test CPA is still opening a stream for")
	}
	close(host.open)
	deadline := time.Now().Add(5 * time.Second)
	for replaced.quality.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if replaced.quality.Running() > 0 {
		t.Fatal("the detection test never ended")
	}
	replaced.quality.HandOff(0) // writes what it recorded, if it may
	if after, _ := os.ReadFile(history); !bytes.Equal(before, after) {
		t.Fatalf("the replaced modules' detection test wrote the history:\n%s", after)
	}
}

// A config key CPA routes nothing to (excluded-models "*") is listed as
// disabled and left out of a run on every credential.
func TestDetectionSkipsKeysCPADisabled(t *testing.T) {
	app := newTestApp(t, appOptions{})
	app.SetHost(&noAuthFilesHost{})
	cpaConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cpaConfig, []byte("codex-api-key:\n  - api-key: live-1\n    base-url: https://relay.example/v1\n  - api-key: off-1\n    base-url: https://relay.example/v1\n    excluded-models: [\"*\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconfigure(t, app, "cpa_config_path: "+cpaConfig+"\n")
	var state struct {
		Auths []struct {
			Name     string `json:"name"`
			Disabled bool   `json:"disabled"`
		} `json:"auths"`
	}
	decode(t, managementOK(t, app, http.MethodGet, "/quality", nil, nil), &state)
	disabled := map[bool]int{}
	for _, auth := range state.Auths {
		disabled[auth.Disabled]++
	}
	if len(state.Auths) != 2 || disabled[true] != 1 {
		t.Fatalf("credentials = %+v", state.Auths)
	}
	var run struct {
		Started int `json:"started"`
	}
	decode(t, managementOK(t, app, http.MethodPost, "/quality/run", nil, []byte(`{"test":"candy","all":true,"model":"gpt-5","runs":1}`)), &run)
	if run.Started != 1 {
		t.Fatalf("started = %d, want only the enabled key", run.Started)
	}
}

// noAuthFilesHost is fakeHost on a CPA without auth files.
type noAuthFilesHost struct{ fakeHost }

func (h *noAuthFilesHost) Call(method string, request []byte) ([]byte, error) {
	if method == "host.auth.list" {
		return []byte(`{"ok":true,"result":{"files":[]}}`), nil
	}
	return h.fakeHost.Call(method, request)
}
