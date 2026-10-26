package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { probeRetryDelay = 0 }

// fakeCPA answers probes per auth index the way CPA's pinned streams do.
type fakeCPA struct {
	mu          sync.Mutex
	credentials []Credential
	listErr     error
	answer      map[string]func(model string, attempt int) ([]byte, int, error)
	calls       map[string]int
	block       chan struct{}
}

// EditExcluded fails: these credentials are not in a config (configCPA is).
func (f *fakeCPA) EditExcluded([]Edit) ([]EditResult, error) {
	return nil, errors.New("fakeCPA has no config")
}

func (f *fakeCPA) Credentials() ([]Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.credentials), f.listErr
}

func (f *fakeCPA) Stream(ctx context.Context, c Credential, model string, body any, headers http.Header) ([]byte, int, error) {
	if f.block != nil {
		<-ctx.Done()
		return nil, 0, context.Cause(ctx)
	}
	f.mu.Lock()
	f.calls[c.AuthIndex]++
	attempt := f.calls[c.AuthIndex]
	answer := f.answer[c.AuthIndex]
	f.mu.Unlock()
	if raw, _ := json.Marshal(body); !strings.Contains(string(raw), `"type":"web_search"`) || headers.Get("Session-Id") == "" {
		return nil, 0, errors.New("probe without the web_search tool or a session")
	}
	return answer(model, attempt)
}

func stream(items ...string) []byte {
	var b strings.Builder
	for _, item := range items {
		fmt.Fprintf(&b, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":%q}}\n", item)
	}
	b.WriteString(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":900,"output_tokens":9}}}` + "\n")
	return []byte(b.String())
}

func searches(string, int) ([]byte, int, error) {
	return stream("web_search_call", "message"), 200, nil
}
func ignores(string, int) ([]byte, int, error) { return stream("message"), 200, nil }

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

const catalog = `{"models":[
 {"slug":"gpt-6-sol","use_responses_lite":true,"visibility":"list","tool_mode":"code_mode_only","description":"<b>"},
 {"slug":"gpt-6-astra","use_responses_lite":true,"visibility":"list"},
 {"slug":"codex-auto-review","use_responses_lite":true,"visibility":"hide"},
 {"slug":"claude-sonnet","use_responses_lite":false,"visibility":"list"}],"etag":"x"}`

func newManager(t *testing.T, cpa *fakeCPA, c *clock) *Manager {
	t.Helper()
	m, err := Open(Options{Path: filepath.Join(t.TempDir(), "native.json"), Upstream: cpa, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runTick ticks and waits for the round it started.
func runTick(m *Manager) {
	m.Tick()
	m.mu.Lock()
	round := m.round
	m.mu.Unlock()
	if round != nil {
		<-round.ended
	}
}

func modeOf(t *testing.T, m *Manager, model string) ModelDecision {
	t.Helper()
	for _, d := range m.Status().Models {
		if d.Model == model {
			return d
		}
	}
	t.Fatalf("model %s not in status %+v", model, m.Status().Models)
	return ModelDecision{}
}

func TestClassifyReadsTheHistory(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	probe := func(ago time.Duration, r Result) Probe { return Probe{At: now.Add(-ago), Result: r} }
	for _, tc := range []struct {
		name   string
		rec    *channelRecord
		expect State
	}{
		{"never probed", nil, StateUnknown},
		{"searched", &channelRecord{Probes: []Probe{probe(time.Hour, ResultSearch)}}, StateSupported},
		{"one failure after a search", &channelRecord{Probes: []Probe{probe(2*time.Hour, ResultSearch), probe(time.Hour, ResultError)}}, StateSupported},
		{"two failures in a row", &channelRecord{Probes: []Probe{probe(3*time.Hour, ResultSearch), probe(2*time.Hour, ResultError), probe(time.Hour, ResultError)}}, StateFailing},
		{"searches part of the time", &channelRecord{Probes: []Probe{probe(13*time.Hour, ResultNoSearch), probe(time.Hour, ResultSearch)}}, StateUnstable},
		{"never searches", &channelRecord{Probes: []Probe{probe(time.Hour, ResultNoSearch)}}, StateNoSearch},
		{"last search too old", &channelRecord{Probes: []Probe{probe(26*time.Hour, ResultSearch)}}, StateStale},
		{"refused the tool", &channelRecord{Probes: []Probe{probe(time.Hour, ResultSearch)}, Pairs: map[string]*pairRecord{"gpt-6-sol": {Rejection: &Rejection{At: now}}}}, StateRejected},
	} {
		if got := classify(tc.rec, false, 12, now); got != tc.expect {
			t.Errorf("%s: state %s, want %s", tc.name, got, tc.expect)
		}
	}
	if got := classify(&channelRecord{Probes: []Probe{probe(time.Hour, ResultSearch)}}, true, 12, now); got != StateDisabled {
		t.Errorf("disabled channel: state %s", got)
	}
}

func TestDecideNeedsFreshSupportAndNamesTheGaps(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	searched := &channelRecord{Probes: []Probe{{At: now.Add(-time.Hour), Result: ResultSearch}}}
	ignored := &channelRecord{Probes: []Probe{{At: now.Add(-time.Hour), Result: ResultNoSearch}}}
	credentials := []Credential{
		{AuthIndex: "a", Models: []string{"gpt-6-sol"}},
		{AuthIndex: "b", Models: []string{"gpt-6-sol", "gpt-6-astra"}},
		{AuthIndex: "c", Models: []string{"gpt-6-sol"}, Disabled: true},
	}
	settings := DefaultSettings()

	none := func(string, string) bool { return false }
	d := decide(settings, []string{"gpt-6-sol", "gpt-6-astra"}, credentials, map[string]*channelRecord{}, none, now)
	if d.native["gpt-6-sol"] || d.models[0].Reason != "no_usable" {
		t.Fatalf("no evidence yet: %+v", d.models[0])
	}

	records := map[string]*channelRecord{"a": searched, "b": ignored, "c": searched}
	d = decide(settings, []string{"gpt-6-sol", "gpt-6-astra"}, credentials, records, none, now)
	if !d.native["gpt-6-sol"] || d.native["gpt-6-astra"] {
		t.Fatalf("native = %v, want gpt-6-sol only", d.native)
	}
	if sol := d.models[0]; !slices.Equal(sol.Usable, []string{"a"}) || len(sol.Gaps) != 1 || sol.Gaps[0] != (Gap{"b", StateNoSearch}) {
		t.Fatalf("gpt-6-sol decision %+v (a CPA-disabled credential serves nothing)", sol)
	}
	if len(d.strip) != 0 {
		t.Fatalf("strip %v: a channel ignoring the tool keeps it", d.strip)
	}

	settings.RequireAll = true
	if d = decide(settings, []string{"gpt-6-sol"}, credentials, records, none, now); d.native["gpt-6-sol"] || d.models[0].Reason != "incomplete" {
		t.Fatalf("require all with a gap: %+v", d.models[0])
	}
	settings.DisabledChannels = []string{"b"}
	if d = decide(settings, []string{"gpt-6-sol"}, credentials, records, none, now); !d.native["gpt-6-sol"] || !d.strip["b"] {
		t.Fatalf("require all, gap disabled: native %v strip %v", d.native, d.strip)
	}
	settings.ExcludedModels = []string{"GPT-6-SOL"}
	if d = decide(settings, []string{"gpt-6-sol"}, credentials, records, none, now); d.native["gpt-6-sol"] || d.models[0].Mode != ModeExcluded {
		t.Fatalf("excluded: %+v", d.models[0])
	}
}

func TestRewriteCatalogTurnsLiteOffForNativeModelsOnly(t *testing.T) {
	candidates, rewritten, ok := rewriteCatalog([]byte(catalog), map[string]bool{"gpt-6-sol": true, "claude-sonnet": true})
	if !ok || !slices.Equal(candidates, []string{"gpt-6-sol", "gpt-6-astra"}) {
		t.Fatalf("candidates %v ok %v", candidates, ok)
	}
	var out struct {
		Models []map[string]any `json:"models"`
		ETag   string           `json:"etag"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil || out.ETag != "x" || len(out.Models) != 4 {
		t.Fatalf("rewritten %s: %v", rewritten, err)
	}
	lite := map[string]any{}
	for _, model := range out.Models {
		lite[model["slug"].(string)] = model["use_responses_lite"]
	}
	if lite["gpt-6-sol"] != false || lite["gpt-6-astra"] != true || lite["codex-auto-review"] != true || out.Models[0]["tool_mode"] != "code_mode_only" {
		t.Fatalf("rewritten models %v", out.Models)
	}
	if !strings.Contains(string(rewritten), `"<b>"`) {
		t.Fatalf("HTML escaped in %s", rewritten)
	}
	if _, rewritten, ok := rewriteCatalog([]byte(catalog), nil); !ok || rewritten != nil {
		t.Fatalf("nothing native: rewritten %s ok %v", rewritten, ok)
	}
	if _, _, ok := rewriteCatalog([]byte(`{"object":"list","data":[{"id":"gpt-6-sol"}]}`), map[string]bool{"gpt-6-sol": true}); ok {
		t.Fatal("an OpenAI model list is not a Codex catalog")
	}
}

func TestStripWebSearchKeepsTheRest(t *testing.T) {
	// The input replays an earlier search, for which CPA would declare the
	// tool again.
	body := `{"model":"gpt-6-sol","input":[{"type":"message","content":"a<b"},{"type":"web_search_call","id":"ws_1","status":"completed"},{"role":"assistant","type":"message","content":"web_search_call"}],"tools":[{"type":"custom","name":"exec"},{"type":"web_search","external_web_access":true}],"tool_choice":{"type":"web_search"}}`
	out, ok := StripWebSearch([]byte(body))
	if !ok {
		t.Fatal("nothing stripped")
	}
	var got struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice any              `json:"tool_choice"`
		Input      []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got.Tools) != 1 || got.Tools[0]["name"] != "exec" || got.ToolChoice != "auto" ||
		len(got.Input) != 2 || got.Input[0]["content"] != "a<b" || got.Input[1]["content"] != "web_search_call" {
		t.Fatalf("stripped %s: %v", out, err)
	}
	if _, ok := StripWebSearch([]byte(`{"tools":[{"type":"function","name":"f"}]}`)); ok {
		t.Fatal("a body without web_search changed")
	}
}

func TestRoundsSwitchModelsAndPersist(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	cpa := &fakeCPA{calls: map[string]int{}, credentials: []Credential{
		{AuthIndex: "a", ID: "codex:apikey:a", Provider: "codex", Name: "a", Models: []string{"gpt-6-sol", "gpt-6-astra"}},
		{AuthIndex: "b", ID: "codex:apikey:b", Provider: "codex", Name: "b", Models: []string{"gpt-6-sol"}},
		{AuthIndex: "o", ID: "codex-oauth", Provider: "codex", Name: "oauth"},
		{AuthIndex: "x", ID: "claude:apikey:x", Provider: "claude", Name: "x"},
	}, answer: map[string]func(string, int) ([]byte, int, error){
		// a fails once with a server error, then searches.
		"a": func(model string, attempt int) ([]byte, int, error) {
			if attempt == 1 {
				return nil, 503, errors.New("upstream overloaded")
			}
			return searches(model, attempt)
		},
		"b": ignores,
		// The OAuth credential has no route to gpt-6-sol, then searches.
		"o": func(model string, attempt int) ([]byte, int, error) {
			if model == "gpt-6-sol" {
				return nil, 500, fmt.Errorf("%w: auth_not_found", ErrNotServing)
			}
			return searches(model, attempt)
		},
	}}
	m := newManager(t, cpa, c)
	if m.RewriteCatalog([]byte(catalog)) != nil {
		t.Fatal("rewrote the catalog without evidence")
	}
	m.Start()
	runTick(m)
	if cpa.calls["a"] != 0 {
		t.Fatal("probed within the startup grace")
	}
	c.now = c.now.Add(startupGrace)
	runTick(m)

	// The OAuth credential lists no models: it is a gap for gpt-6-sol until a
	// probe tells whether it serves it.
	sol := modeOf(t, m, "gpt-6-sol")
	if sol.Mode != ModeNative || !slices.Equal(sol.Usable, []string{"a"}) || !slices.Equal(sol.Gaps, []Gap{{"b", StateNoSearch}, {"o", StateUnknown}}) {
		t.Fatalf("gpt-6-sol after the first round: %+v", sol)
	}
	if astra := modeOf(t, m, "gpt-6-astra"); astra.Mode != ModeNative || !slices.Equal(astra.Usable, []string{"a", "o"}) {
		t.Fatalf("gpt-6-astra: %+v (the OAuth credential serves only astra)", astra)
	}
	status := m.Status()
	if len(status.Channels) != 3 || status.LastFullRound.IsZero() || status.Round != nil {
		t.Fatalf("status channels %+v", status.Channels)
	}
	if probes := status.Channels[0].Probes; len(probes) != 1 || probes[0].Result != ResultSearch || probes[0].Searches != 1 || probes[0].InputTokens != 900 {
		t.Fatalf("channel a probes %+v (retried after the 503)", probes)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(m.RewriteCatalog([]byte(catalog)), &out); err != nil || out.Models[0]["use_responses_lite"] != false {
		t.Fatalf("catalog not rewritten: %v", out.Models)
	}

	// The operator disables a: no channel searches for gpt-6-sol any more.
	settings := m.Settings()
	settings.DisabledChannels = []string{"a"}
	if _, _, err := m.Update(settings); err != nil {
		t.Fatal(err)
	}
	if sol := modeOf(t, m, "gpt-6-sol"); sol.Mode != ModeFallback || sol.Reason != "no_usable" {
		t.Fatalf("gpt-6-sol with a disabled: %+v", sol)
	}
	if !m.Strips("a", "gpt-6-astra") || !m.Strips("a", "gpt-6-sol") || m.Strips("b", "gpt-6-astra") || m.Strips("a", "claude-sonnet") {
		t.Fatal("strip applies to a disabled channel's catalog candidates only")
	}

	reopened, err := Open(Options{Path: m.path, Upstream: cpa, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if astra := modeOf(t, reopened, "gpt-6-astra"); astra.Mode != ModeNative || !slices.Equal(reopened.Settings().DisabledChannels, []string{"a"}) {
		t.Fatalf("reopened: %+v settings %+v", astra, reopened.Settings())
	}

	// b did not search once: it is asked again soon, which settles it, and
	// then not before the next full round. That round waits for the
	// interval; it asks the OAuth credential about gpt-6-sol first, which
	// CPA has no route to.
	before := cpa.calls["b"]
	c.now = c.now.Add(time.Hour)
	runTick(m)
	runTick(m)
	if cpa.calls["b"] != before+1 {
		t.Fatalf("b probed %d more times before the interval, want the one recheck", cpa.calls["b"]-before)
	}
	c.now = c.now.Add(12 * time.Hour)
	runTick(m)
	if sol := modeOf(t, m, "gpt-6-sol"); !slices.Equal(sol.Gaps, []Gap{{"a", StateDisabled}, {"b", StateNoSearch}}) || cpa.calls["b"] != before+2 {
		t.Fatalf("gpt-6-sol after the second round: %+v", sol)
	}
}

func TestRefusalStripsUntilAProbeSearches(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	cpa := &fakeCPA{calls: map[string]int{}, credentials: []Credential{
		{AuthIndex: "a", ID: "codex:apikey:a", Provider: "codex", Name: "a", Models: []string{"gpt-6-sol"}},
	}, answer: map[string]func(string, int) ([]byte, int, error){"a": searches}}
	m := newManager(t, cpa, c)
	m.RewriteCatalog([]byte(catalog))
	m.Start()
	c.now = c.now.Add(startupGrace)
	runTick(m)

	m.ObserveFailure("a", "gpt-6-sol", 500, "web_search failed")
	m.ObserveFailure("a", "gpt-6-sol", 400, "context too long")
	m.ObserveFailure("a", "claude-sonnet", 400, "unsupported tool web_search")
	if m.Strips("a", "gpt-6-sol") {
		t.Fatal("a server error, another refusal or a model outside the catalog marked a refusal")
	}
	m.ObserveFailure("a", "gpt-6-sol", 400, `{"error":{"message":"Unsupported tool type: web_search"}}`)
	m.ObserveFailure("a", "gpt-6-sol", 400, "Unsupported tool type: web_search")
	if !m.Strips("a", "gpt-6-sol") {
		t.Fatal("refusal did not take the tool out")
	}
	ch := m.Status().Channels[0]
	if ch.State != StateRejected || ch.Rejection == nil || ch.Rejection.Count != 2 {
		t.Fatalf("channel after refusals: %+v", ch)
	}
	// One refusing channel left: the model stays native, its tool goes.
	if sol := modeOf(t, m, "gpt-6-sol"); sol.Mode != ModeFallback {
		t.Fatalf("gpt-6-sol with its only channel refusing: %+v", sol)
	}

	before := cpa.calls["a"]
	c.now = c.now.Add(rejectionRecheck)
	runTick(m)
	if cpa.calls["a"] == before || m.Strips("a", "gpt-6-sol") || modeOf(t, m, "gpt-6-sol").Mode != ModeNative {
		t.Fatalf("recheck did not clear the refusal: calls %d status %+v", cpa.calls["a"], m.Status().Channels[0])
	}
}

func TestStopEndsARoundWithoutRecordingIt(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	cpa := &fakeCPA{calls: map[string]int{}, block: make(chan struct{}), credentials: []Credential{
		{AuthIndex: "a", ID: "codex:apikey:a", Provider: "codex", Name: "a", Models: []string{"gpt-6-sol"}},
	}}
	m := newManager(t, cpa, c)
	m.RewriteCatalog([]byte(catalog))
	if err := m.ProbeNow(""); !errors.Is(err, ErrOff) {
		t.Fatalf("round before Start: %v", err)
	}
	m.Start()
	if err := m.ProbeNow(""); err != nil {
		t.Fatal(err)
	}
	if err := m.ProbeNow(""); !errors.Is(err, ErrBusy) {
		t.Fatalf("second round: %v", err)
	}
	m.Stop()
	status := m.Status()
	if status.Round != nil || len(status.Channels[0].Probes) != 0 || !status.LastFullRound.IsZero() {
		t.Fatalf("a stopped round left %+v", status)
	}
	if err := m.ProbeNow(""); !errors.Is(err, ErrOff) {
		t.Fatalf("round after Stop: %v", err)
	}
	m.Start()
	if err := m.ProbeNow("nope"); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("unknown channel: %v", err)
	}

	// Switching native search off ends the round under way the same way.
	if err := m.ProbeNow(""); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	round := m.round
	m.mu.Unlock()
	settings := m.Settings()
	settings.Enabled = false
	if _, _, err := m.Update(settings); err != nil {
		t.Fatal(err)
	}
	select {
	case <-round.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("switching native search off left the round running")
	}
	if status := m.Status(); status.Round != nil || len(status.Channels[0].Probes) != 0 {
		t.Fatalf("a round native search was switched off during left %+v", status)
	}
}
