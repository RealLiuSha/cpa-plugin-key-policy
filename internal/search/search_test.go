package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testProbeKey = "probe-key-0123456789"

// newTestRouter returns a router on a fresh settings file, updated to s and
// stopped when the test ends.
func newTestRouter(t *testing.T, s Settings) *Router {
	t.Helper()
	r, err := NewRouter(filepath.Join(t.TempDir(), "cpa-key-policy-search.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	if _, err := r.Update(s, false); err != nil {
		t.Fatal(err)
	}
	return r
}

// channelSettings is the production layout: a primary serving model names
// as they are and a fallback whose credential registers only prefixed names.
func channelSettings(probeURL string) Settings {
	return Settings{
		TargetModel: "gpt-6-astra",
		Channels: []Channel{
			{Name: "primary", Model: "{model}"},
			{Name: "fallback", Model: "alphasearch-nb-{model}"},
		},
		Probe: Probe{APIKey: testProbeKey, URL: probeURL},
	}
}

func TestRoute(t *testing.T) {
	routed := func(target string) Decision {
		return Decision{Handled: true, TargetKind: "provider", Target: "codex", TargetModel: target}
	}
	custom := Settings{TargetModel: " gpt-6-sol ", KeepModels: []string{"gpt-*", " GLM-* "}}
	forcePrimary := channelSettings("")
	forcePrimary.Channels[0].ForceUnhealthy = true
	forceAll := channelSettings("")
	forceAll.Channels[0].ForceUnhealthy, forceAll.Channels[1].ForceUnhealthy = true, true
	tests := []struct {
		name      string
		settings  Settings
		unhealthy []int  // channels failing their probe
		format    string // empty means SourceFormat
		model     string
		headers   http.Header
		want      Decision
	}{
		{name: "claude search uses the default target", model: "claude-sonnet-5.5", want: routed("gpt-6.1-sol")},
		{name: "gemini search", model: "gemini-3.8", want: routed("gpt-6.1-sol")},
		{name: "grok search", model: "grok-4.6", want: routed("gpt-6.1-sol")},
		{name: "gpt search keeps its model", model: "gpt-6-luna"},
		{name: "keep match is case-insensitive", model: "GPT-6-Sol"},
		{name: "empty model is left alone", model: " "},
		{name: "claude chat is not routed", format: "openai-response", model: "claude-sonnet-5.5"},
		{name: "claude messages are not routed", format: "claude", model: "claude-sonnet-5.5"},
		{name: "custom keep list", settings: custom, model: "glm-5.3"},
		{name: "custom target outside the keep list", settings: custom, model: "claude-opus-5.5", want: routed("gpt-6-sol")},
		{name: "model equal to the target is left alone", settings: Settings{TargetModel: "claude-x"}, model: "Claude-X"},
		{name: "blank keep list falls back to gpt-*", settings: Settings{KeepModels: []string{" "}}, model: "gpt-6-astra"},
		{name: "primary keeps a gpt model", settings: channelSettings(""), model: "gpt-6-sol"},
		{name: "primary maps claude to the target", settings: channelSettings(""), model: "claude-sonnet-5.5", want: routed("gpt-6-astra")},
		{name: "forced primary sends gpt to the fallback", settings: forcePrimary, model: "gpt-6-sol", want: routed("alphasearch-nb-gpt-6-sol")},
		{name: "forced primary sends grok to the fallback target", settings: forcePrimary, model: "grok-4.6", want: routed("alphasearch-nb-gpt-6-astra")},
		{name: "probe-failed primary sends gpt to the fallback", settings: channelSettings(""), unhealthy: []int{0}, model: "gpt-6-sol", want: routed("alphasearch-nb-gpt-6-sol")},
		{name: "probe requests keep their model", settings: forcePrimary, model: "alphasearch-nb-gpt-6-astra", headers: http.Header{"x-alpha-search-model-probe": {"1"}}},
		{name: "all channels forced fall back to the first", settings: forceAll, model: "gemini-3.8", want: routed("gpt-6-astra")},
		{name: "all channels failing fall back to the first", settings: channelSettings(""), unhealthy: []int{0, 1}, model: "gemini-3.8", want: routed("gpt-6-astra")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRouter(t, tt.settings)
			for _, i := range tt.unhealthy {
				r.state.Load().channels[i].unhealthy.Store(true)
			}
			format := tt.format
			if format == "" {
				format = SourceFormat
			}
			got := r.Route(RouteRequest{SourceFormat: format, RequestedModel: tt.model, Headers: tt.headers})
			if got.Handled && !strings.Contains(got.Reason, tt.want.TargetModel) {
				t.Errorf("reason %q does not name the target", got.Reason)
			}
			got.Reason = ""
			if got != tt.want {
				t.Fatalf("Route(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
		})
	}
}

func TestFromPluginConfig(t *testing.T) {
	decode := func(raw string) map[string]any {
		t.Helper()
		var block map[string]any
		if err := json.Unmarshal([]byte(raw), &block); err != nil {
			t.Fatal(err)
		}
		return block
	}
	got, err := FromPluginConfig(decode(`{
		"enabled": true, "priority": 3, "store": {"path": "x"}, "unknown-panel-key": 1,
		"target-model": " gpt-6-astra ", "keep-models": ["GPT-*", "glm-*", ""],
		"channels": [
			{"name": 7788101, "model": "{model}"},
			{"name": "nbility", "model": "alphasearch-nb-{model}", "force-unhealthy": true, "note": "ignored"}
		],
		"probe-api-key": " probe-key ", "probe-interval-seconds": 30, "probe-timeout-seconds": 5,
		"probe-model": "gpt-6-sol", "probe-url": "http://127.0.0.1:18317/v1/alpha/search",
		"probe-user-agent": "codex_cli_rs/0.131.0", "probe-idle-seconds": 600
	}`))
	want := Settings{
		TargetModel: "gpt-6-astra",
		KeepModels:  []string{"gpt-*", "glm-*"},
		Channels: []Channel{
			{Name: "7788101", Model: "{model}"},
			{Name: "nbility", Model: "alphasearch-nb-{model}", ForceUnhealthy: true},
		},
		Probe: Probe{
			APIKey: "probe-key", IntervalSeconds: 30, TimeoutSeconds: 5, Model: "gpt-6-sol",
			URL: "http://127.0.0.1:18317/v1/alpha/search", UserAgent: "codex_cli_rs/0.131.0", IdleSeconds: 600,
		},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("FromPluginConfig = %+v, %v; want %+v", got, err, want)
	}
	if got, err := FromPluginConfig(decode(`{"enabled": false, "channels": [], "keep-models": []}`)); err != nil || !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("FromPluginConfig(empty settings) = %+v, %v; want the defaults", got, err)
	}
	// CPA answers {} for a plugin without a configuration block, and host
	// keys alone are not that plugin's settings.
	for _, block := range []map[string]any{nil, {}, decode(`{"enabled": true, "priority": 3, "store": {"path": "x"}}`)} {
		if _, err := FromPluginConfig(block); !errors.Is(err, ErrNothingToImport) {
			t.Fatalf("FromPluginConfig(%v) error = %v, want ErrNothingToImport", block, err)
		}
	}
	for _, raw := range []string{
		`{"channels": [{"name": "a", "model": "{model}"}, {"name": "a", "model": "x-{model}"}]}`,
		`{"channels": [{"name": "a"}]}`,
		`{"channels": ["primary"]}`,
		`{"channels": [{"name": "a", "model": "{model}", "force-unhealthy": "yes"}]}`,
		`{"keep-models": ["gpt-["]}`,
		`{"keep-models": "gpt-*"}`,
		`{"probe-interval-seconds": 5}`,
		`{"probe-interval-seconds": "60"}`,
		`{"probe-timeout-seconds": 1.5}`,
		`{"probe-idle-seconds": -1}`,
		`{"probe-url": "127.0.0.1:8317/v1/alpha/search"}`,
	} {
		if _, err := FromPluginConfig(decode(raw)); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("FromPluginConfig(%s) error = %v, want ErrInvalidSettings", raw, err)
		}
	}
}

func TestUpdateKeepsProbeKeyHealthAndCounters(t *testing.T) {
	r := newTestRouter(t, channelSettings(""))
	r.state.Load().channels[0].unhealthy.Store(true)
	search := RouteRequest{SourceFormat: SourceFormat, RequestedModel: "gpt-6-sol"}
	r.Route(search)
	r.Route(search)
	r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "gpt-6-sol", Headers: http.Header{ProbeHeader: {"1"}}})
	r.Route(RouteRequest{SourceFormat: "openai-response", RequestedModel: "gpt-6-sol"})
	status := r.Status()
	if status.RoutedTotal != 2 || status.Channels[0].Routed != 0 || status.Channels[1].Routed != 2 ||
		status.Channels[1].LastRoutedAt.IsZero() || status.LastSearchAt.IsZero() || !status.ProbeKeyStored {
		t.Fatalf("status after two searches = %+v", status)
	}

	// An empty key keeps the stored one; unchanged channels keep their state.
	next := r.Settings()
	if next.Probe.APIKey != "" {
		t.Fatal("Settings returned the probe key")
	}
	next.Channels[1].ForceUnhealthy = true
	next.Channels = append(next.Channels, Channel{Name: "spare", Model: "spare-{model}"})
	saved, err := r.Update(next, false)
	if err != nil {
		t.Fatal(err)
	}
	status = r.Status()
	if !status.ProbeKeyStored || status.Channels[0].Healthy || status.Channels[1].Routed != 2 || status.Channels[2].Routed != 0 {
		t.Fatalf("status after update = %+v", status)
	}
	for _, v := range []any{saved, r.Settings(), status} {
		if raw, _ := json.Marshal(v); strings.Contains(string(raw), testProbeKey) || strings.Contains(string(raw), "0001-01-01") {
			t.Fatalf("probe key or zero time encoded in %s", raw)
		}
	}
	if reopened, err := NewRouter(r.path, nil, nil); err != nil || !reopened.Status().ProbeKeyStored {
		t.Fatalf("reloaded router lost the probe key: %v", err)
	}

	// A new template starts that channel afresh.
	next = r.Settings()
	next.Channels[1].Model = "alphasearch-x-{model}"
	if _, err := r.Update(next, false); err != nil {
		t.Fatal(err)
	}
	if got := r.Status().Channels[1].Routed; got != 0 {
		t.Fatalf("changed channel kept %d routed searches", got)
	}

	// Clearing the key turns probing off, and with it the probe health.
	if _, err := r.Update(r.Settings(), true); err != nil {
		t.Fatal(err)
	}
	if status := r.Status(); status.ProbeKeyStored || !status.Channels[0].Healthy || status.RoutedTotal != 2 {
		t.Fatalf("status after clearing the key = %+v", status)
	}
	if raw, err := os.ReadFile(r.path); err != nil || strings.Contains(string(raw), testProbeKey) {
		t.Fatalf("settings file after clearing the key: %s, %v", raw, err)
	}
}

func TestSettingsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "cpa-key-policy-search.json")
	var readOnly atomic.Bool
	r, err := NewRouter(path, func() bool { return !readOnly.Load() }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	if got := r.Settings(); !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("settings without a file = %+v, want the defaults", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewRouter wrote the settings file: %v", err)
	}

	saved, err := r.Update(channelSettings("https://cpa.example/v1/alpha/search"), false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version  int             `json:"version"`
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(raw, &file); err != nil || file.Version != 1 || len(file.Settings) == 0 {
		t.Fatalf("settings file %s: %v", raw, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode: %v, %v", info, err)
	}
	reopened, err := NewRouter(path, nil, nil)
	if err != nil || !reflect.DeepEqual(reopened.Settings(), saved) || !reopened.Status().ProbeKeyStored {
		t.Fatalf("reloaded settings = %+v, %v; want %+v with the key", reopened.Settings(), err, saved)
	}

	// Refused or failed updates leave memory, and a refused one the file, alone.
	changed := saved
	changed.TargetModel = "gpt-6-sol"
	readOnly.Store(true)
	if _, err := r.Update(changed, false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only update error = %v, want ErrReadOnly", err)
	}
	readOnly.Store(false)
	if after, _ := os.ReadFile(path); string(after) != string(raw) {
		t.Fatal("read-only update changed the file")
	}
	tooFast := changed
	tooFast.Probe.IntervalSeconds = 5
	if _, err := r.Update(tooFast, false); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("invalid update error = %v, want ErrInvalidSettings", err)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(path), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Update(changed, false); err == nil || errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("update into a missing directory error = %v, want a write error", err)
	}
	if got := r.Settings(); !reflect.DeepEqual(got, saved) {
		t.Fatalf("settings after refused updates = %+v, want %+v", got, saved)
	}

	for _, content := range []string{
		`{`,
		`{"version": 2, "settings": {}}`,
		`{"version": 1, "settings": {"channels": [{"name": "a"}]}}`,
	} {
		other := filepath.Join(dir, "other.json")
		if err := os.WriteFile(other, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewRouter(other, nil, nil); err == nil {
			t.Errorf("NewRouter accepted %s", content)
		}
	}
}

// htmlPage makes fakeCPA answer 200 with an HTML page.
const htmlPage = -1

// fakeCPA stands in for CPA's /v1/alpha/search during probes. status maps a
// probe model to its answer; 401 echoes the request's key, and unknown
// models get 503.
type fakeCPA struct {
	t      *testing.T
	url    string
	hits   chan string
	mu     sync.Mutex
	status map[string]int
	seen   map[string]int
}

func newFakeCPA(t *testing.T, status map[string]int) *fakeCPA {
	f := &fakeCPA{t: t, hits: make(chan string, 64), status: status, seen: map[string]int{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.url = server.URL + "/v1/alpha/search"
	return f
}

func (f *fakeCPA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string         `json:"model"`
		Commands map[string]any `json:"commands"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode probe body: %v", err)
	}
	if r.Header.Get("Authorization") != "Bearer "+testProbeKey || r.Header.Get(ProbeHeader) != "1" ||
		r.Header.Get("User-Agent") != defaultProbeUserAgent || body.Commands["response_length"] != "short" {
		f.t.Errorf("probe request headers %v commands %v", r.Header, body.Commands)
	}
	f.mu.Lock()
	status, ok := f.status[body.Model]
	f.seen[body.Model]++
	f.mu.Unlock()
	select {
	case f.hits <- body.Model:
	default:
	}
	switch {
	case !ok:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"auth_not_found: no auth available"}`))
	case status == http.StatusOK:
		_, _ = w.Write([]byte(`{"output":"2026-10-19","results":[]}`))
	case status == htmlPage:
		_, _ = w.Write([]byte(`<html>home</html>`))
	case status == http.StatusUnauthorized:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + `"}`))
	default:
		w.WriteHeader(status)
	}
}

func (f *fakeCPA) set(model string, status int) {
	f.mu.Lock()
	f.status[model] = status
	f.mu.Unlock()
}

func (f *fakeCPA) count(model string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[model]
}

// expectProbe waits for the next probe and checks its model.
func (f *fakeCPA) expectProbe(t *testing.T, model string) {
	t.Helper()
	select {
	case got := <-f.hits:
		if got != model {
			t.Fatalf("probe for %q, want %q", got, model)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no probe for %q", model)
	}
}

func TestProbesSwitchChannels(t *testing.T) {
	cpa := newFakeCPA(t, map[string]int{"gpt-6-astra": http.StatusOK, "alphasearch-nb-gpt-6-astra": http.StatusOK})
	r := newTestRouter(t, channelSettings(cpa.url))
	round := func() { r.probeRound(context.Background(), r.state.Load()) }
	search := func() string {
		return r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "gpt-6-luna"}).TargetModel
	}

	round()
	if got := search(); got != "" {
		t.Fatalf("healthy primary routed gpt-6-luna to %q", got)
	}
	if n := cpa.count("alphasearch-nb-gpt-6-astra"); n != 0 {
		t.Fatalf("fallback probed %d times while the primary was healthy", n)
	}
	for _, failure := range []int{http.StatusForbidden, htmlPage, http.StatusUnauthorized} {
		cpa.set("gpt-6-astra", failure)
		round()
		if got := search(); got != "alphasearch-nb-gpt-6-luna" {
			t.Fatalf("primary failing with %d routed to %q, want the fallback", failure, got)
		}
		primary := r.Status().Channels[0]
		if primary.Healthy || primary.LastProbeAt.IsZero() || primary.LastProbeErr == "" || strings.Contains(primary.LastProbeErr, testProbeKey) {
			t.Fatalf("primary failing with %d: %+v", failure, primary)
		}
	}
	cpa.set("alphasearch-nb-gpt-6-astra", http.StatusBadGateway)
	round()
	if got := search(); got != "" {
		t.Fatalf("all channels failing routed to %q, want the primary", got)
	}
	cpa.set("gpt-6-astra", http.StatusOK)
	round()
	if got, primary := search(), r.Status().Channels[0]; got != "" || !primary.Healthy || primary.LastProbeErr != "" {
		t.Fatalf("recovered primary: routed to %q, status %+v", got, primary)
	}
}

func TestProbeKeepsHealthWhenCPAIsUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + listener.Addr().String() + "/v1/alpha/search"
	_ = listener.Close()
	r := newTestRouter(t, channelSettings(closedURL))
	st := r.state.Load()
	st.channels[0].unhealthy.Store(true)
	r.probeRound(context.Background(), st)
	status := r.Status()
	if status.Channels[0].Healthy || !status.Channels[1].Healthy || !strings.Contains(status.Channels[1].LastProbeErr, "CPA unreachable") {
		t.Fatalf("dial errors changed channel health: %+v", status.Channels)
	}
}

func TestProbeLoopLifecycle(t *testing.T) {
	cpa := newFakeCPA(t, map[string]int{"gpt-6-astra": http.StatusOK, "gpt-6-sol": http.StatusOK})
	r := newTestRouter(t, channelSettings(cpa.url))
	if r.Status().ProbeRunning {
		t.Fatal("probes ran before Start")
	}

	r.Start()
	cpa.expectProbe(t, "gpt-6-astra")
	first := r.loop.Load()
	r.Start()
	if r.loop.Load() != first || !r.Status().ProbeRunning {
		t.Fatal("second Start replaced or lost the running loop")
	}

	next := r.Settings()
	next.Probe.Model = "gpt-6-sol"
	if _, err := r.Update(next, false); err != nil {
		t.Fatal(err)
	}
	cpa.expectProbe(t, "gpt-6-sol")
	if first.running() {
		t.Fatal("Update left the old loop running")
	}

	current := r.loop.Load()
	r.Stop()
	r.Stop()
	if current.running() || r.Status().ProbeRunning {
		t.Fatal("Stop left the loop running")
	}

	// After Start, clearing the key stops probing and a new key resumes it.
	r.Start()
	cpa.expectProbe(t, "gpt-6-sol")
	if _, err := r.Update(r.Settings(), true); err != nil {
		t.Fatal(err)
	}
	if r.Status().ProbeRunning {
		t.Fatal("probes kept running without a key")
	}
	withKey := r.Settings()
	withKey.Probe.APIKey = testProbeKey
	if _, err := r.Update(withKey, false); err != nil {
		t.Fatal(err)
	}
	cpa.expectProbe(t, "gpt-6-sol")
}

func TestStopAbortsProbeInFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The server notices a client hang-up only once the body is read.
		_, _ = io.Copy(io.Discard, req.Body)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-req.Context().Done()
	}))
	t.Cleanup(server.Close)
	r := newTestRouter(t, channelSettings(server.URL))
	r.Start()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no probe arrived")
	}
	begin := time.Now()
	r.Stop()
	if waited := time.Since(begin); waited > 5*time.Second {
		t.Fatalf("Stop waited %v for the probe timeout", waited)
	}
	if primary := r.Status().Channels[0]; !primary.Healthy || primary.LastProbeErr != "" {
		t.Fatalf("an aborted probe changed the primary: %+v", primary)
	}
}

type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) { panic("transport exploded") }

func TestProbeGoroutineSurvivesPanics(t *testing.T) {
	logs := make(chan string, 16)
	r, err := NewRouter(filepath.Join(t.TempDir(), "search.json"), nil, func(level, message string) {
		select {
		case logs <- level + " " + message:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	if _, err := r.Update(channelSettings(""), false); err != nil {
		t.Fatal(err)
	}
	r.transport = panicTransport{}
	r.Start()
	for line := ""; !strings.Contains(line, "warn alpha search: probe round aborted by a panic: transport exploded"); {
		select {
		case line = <-logs:
		case <-time.After(5 * time.Second):
			t.Fatal("no log line for the panicking round")
		}
	}
	if !r.Status().ProbeRunning {
		t.Fatal("the probe loop ended with the panicking round")
	}

	// A panicking log sink costs only its log lines.
	cpa := newFakeCPA(t, map[string]int{"gpt-6-astra": http.StatusOK})
	quiet, err := NewRouter(filepath.Join(t.TempDir(), "search.json"), nil, func(string, string) { panic("log sink exploded") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(quiet.Stop)
	if _, err := quiet.Update(channelSettings(cpa.url), false); err != nil {
		t.Fatal(err)
	}
	quiet.Start()
	cpa.expectProbe(t, "gpt-6-astra")
	quiet.Stop()
}

func TestReadsDoNotWaitForWrites(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	r, err := NewRouter(filepath.Join(t.TempDir(), "search.json"), func() bool {
		if block.Load() {
			entered <- struct{}{}
			<-release
		}
		return true
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	block.Store(true)
	updated := make(chan error, 1)
	go func() {
		_, err := r.Update(channelSettings(""), false)
		updated <- err
	}()
	<-entered
	read := make(chan Decision, 1)
	go func() {
		r.Status()
		r.Settings()
		read <- r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "claude-sonnet-5.5"})
	}()
	var got Decision
	select {
	case got = <-read:
	case <-time.After(5 * time.Second):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if got.TargetModel != "gpt-6.1-sol" {
		t.Fatalf("Route during an Update = %+v, want an answer from the previous settings without waiting", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	cpa := newFakeCPA(t, map[string]int{"gpt-6-astra": http.StatusOK, "gpt-6-sol": http.StatusOK})
	r := newTestRouter(t, channelSettings(cpa.url))
	settings := [2]Settings{channelSettings(cpa.url), channelSettings(cpa.url)}
	settings[1].Probe.Model = "gpt-6-sol"
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "claude-sonnet-5.5"})
				r.Status()
				r.Settings()
			}
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 20 {
			if _, err := r.Update(settings[i%2], false); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			r.Start()
			r.Stop()
			r.Start()
		}
	}()
	wg.Wait()
	if got := r.Status().RoutedTotal; got != 800 {
		t.Fatalf("RoutedTotal = %d, want 800", got)
	}
}

func TestIdleProbesPause(t *testing.T) {
	r := newTestRouter(t, channelSettings(""))
	now := time.Now()
	if r.idle(600, now) {
		t.Fatal("a new router is idle")
	}
	r.createdAt = now.Add(-2 * time.Hour)
	r.lastSearch.Store(now.Add(-time.Hour).UnixNano())
	if !r.idle(600, now) {
		t.Fatal("probes not idle after an hour without searches")
	}
	r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "gpt-6-sol", Headers: http.Header{ProbeHeader: {"1"}}})
	r.Route(RouteRequest{SourceFormat: "openai-response", RequestedModel: "gpt-6-sol"})
	if !r.idle(600, now) {
		t.Fatal("probe or chat requests counted as search traffic")
	}
	r.Route(RouteRequest{SourceFormat: SourceFormat, RequestedModel: "gpt-6-sol"})
	if r.idle(600, time.Now()) {
		t.Fatal("probes still idle after a client search")
	}
	if r.idle(0, now.Add(24*time.Hour)) {
		t.Fatal("idle_seconds 0 paused probes")
	}
}
