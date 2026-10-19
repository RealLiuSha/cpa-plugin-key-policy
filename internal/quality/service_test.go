package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is CPA's side of the callbacks: an auth manager list (files, by
// default empty: it leaves out the config's API keys), and model streams that
// answer at once, or hold until closed when hold is set. events replaces the
// answer's event stream; a gate holds execute_stream until it is closed.
type fakeHost struct {
	answer string
	hold   bool
	files  string
	events string
	gate   chan struct{}

	mu      sync.Mutex
	streams map[string]chan struct{}
	next    int
	closed  int
	peak    int
	gets    int
	pinned  []string
}

// panicky panics after the fake answered method, as a broken host bridge
// would.
type panicky struct {
	*fakeHost
	method string
}

func (p panicky) Call(method string, payload any) (json.RawMessage, error) {
	raw, err := p.fakeHost.Call(method, payload)
	if method == p.method {
		panic("simulated host bridge panic")
	}
	return raw, err
}

func (h *fakeHost) openStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

func (h *fakeHost) requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pinned)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

// configKey is the one API key of CPA's config these tests know.
func configKey() ([]Credential, error) {
	return []Credential{{ID: "codex:apikey:1", AuthIndex: "0123456789abcdef", Provider: "codex", Source: SourceProvider,
		Name: PreviewKey("sk-secret-0123456789"), Group: "relay"}}, nil
}

// open opens a service whose pending history writes end with the test.
func open(t *testing.T, opts Options) *Service {
	if opts.ConfigCredentials == nil {
		opts.ConfigCredentials = configKey
	}
	s := Open(opts)
	t.Cleanup(s.Stop)
	return s
}

func (h *fakeHost) Call(method string, payload any) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams == nil {
		h.streams = map[string]chan struct{}{}
	}
	switch method {
	case "host.auth.list":
		if h.files != "" {
			return json.RawMessage(h.files), nil
		}
		return json.RawMessage(`{"files":[]}`), nil
	case "host.auth.get":
		h.gets++
		return json.RawMessage(`{"json":{"plan_type":"plus"}}`), nil
	case "host.model.execute_stream":
		h.pinned = append(h.pinned, payload.(map[string]any)["auth_id"].(string))
		if h.gate != nil {
			h.mu.Unlock()
			<-h.gate
			h.mu.Lock()
		}
		h.next++
		id := fmt.Sprint(h.next)
		h.streams[id] = make(chan struct{})
		h.peak = max(h.peak, len(h.streams))
		return json.RawMessage(`{"stream_id":"` + id + `"}`), nil
	case "host.model.stream_read":
		id := payload.(map[string]any)["stream_id"].(string)
		closed, ok := h.streams[id]
		if h.hold {
			if ok {
				h.mu.Unlock()
				<-closed
				h.mu.Lock()
			}
			return json.RawMessage(`{"error":"stream closed","done":true}`), nil
		}
		text, _ := json.Marshal(h.answer)
		event := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` +
			string(text) + `}]}],"usage":{"input_tokens":14000,"output_tokens":5}}}` + "\n\n"
		if h.events != "" {
			event = h.events
		}
		payload, _ := json.Marshal(map[string]any{"payload": []byte(event), "done": true})
		return payload, nil
	case "host.model.stream_close":
		id := payload.(map[string]any)["stream_id"].(string)
		if ch, ok := h.streams[id]; ok {
			close(ch)
			delete(h.streams, id)
			h.closed++
		}
		return json.RawMessage(`{}`), nil
	}
	return nil, fmt.Errorf("unexpected host call %s", method)
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if s.Running() == 0 {
			return
		}
	}
	t.Fatal("tests still running")
}

// A candy run on a config API key, which the auth manager list leaves out, is
// pinned to the key's ID; the answer is judged, kept and written once for the
// whole run. The list carries no answer; the record, named by its time, does.
func TestCandyRunIsJudgedAndSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.json")
	host := &fakeHost{answer: "最少取出 21 个"}
	s := open(t, Options{Path: path, Host: host})
	state, err := s.State()
	if err != nil || len(state.Auths) != 1 || state.Auths[0].AuthIndex != "0123456789abcdef" || strings.Contains(state.Auths[0].Name, "secret") {
		t.Fatalf("state = %+v, %v", state, err)
	}
	if summary, err := s.Run(RunRequest{Test: TestCandy, All: true, Model: "gpt-6-sol", Effort: "low", Runs: 2}); err != nil || summary.Started != 1 {
		t.Fatalf("run = %+v, %v", summary, err)
	}
	waitIdle(t, s)
	if strings.Join(host.pinned, ",") != "codex:apikey:1,codex:apikey:1" {
		t.Fatalf("requests pinned to %v", host.pinned)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history written before the flush delay: %v", err)
	}
	s.flushHistory()
	reopened := open(t, Options{Path: path, Host: &fakeHost{}})
	state, _ = reopened.State()
	results := state.Auths[0].Results
	if len(results) != 2 || !results[0].OK || !results[1].OK || results[1].InputTokens != 14000 || results[0].Answer != "" {
		t.Fatalf("saved results = %+v", results)
	}
	summary, _ := json.Marshal(results[0])
	var named struct {
		Time string `json:"time"`
	}
	_ = json.Unmarshal(summary, &named)
	if record, err := reopened.Record(TestCandy, "codex:apikey:1", named.Time); err != nil || record.(CandyResult).Answer != host.answer {
		t.Fatalf("record %s = %+v, %v", named.Time, record, err)
	}
}

// Stop cuts a test that waits on the upstream short: it closes the stream,
// waits for the run to end and keeps the cancelled record.
func TestStopCancelsARunningTest(t *testing.T) {
	host := &fakeHost{hold: true}
	s := open(t, Options{Path: filepath.Join(t.TempDir(), "quality.json"), Host: host})
	if _, err := s.Run(RunRequest{Test: TestPelican, All: true, Model: "gpt-6-sol"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	state, _ := s.State()
	if pelicans := state.Auths[0].Pelicans; len(pelicans) != 1 || pelicans[0].Error != cancelledMessage || host.closed != 1 || s.Running() != 0 {
		t.Fatalf("after Stop: pelicans %+v, streams closed %d, running %d", pelicans, host.closed, s.Running())
	}
	if _, err := os.Stat(s.path); err != nil {
		t.Fatalf("history not written at Stop: %v", err)
	}
	// A run that reaches the service after Stop is refused instead of
	// starting a test nothing waits for.
	if _, err := s.Run(RunRequest{Test: TestPelican, All: true, Model: "gpt-6-sol"}); !errors.Is(err, ErrUnavailable) || s.Running() != 0 {
		t.Fatalf("run after Stop: err %v, running %d", err, s.Running())
	}
}

// An inspection's verdict counts only its own records: a wrong answer marks
// the credential degraded. Its candy records keep their answers, as
// candy-eval's inspection script reads them.
func TestInspectionVerdict(t *testing.T) {
	s := open(t, Options{Path: filepath.Join(t.TempDir(), "quality.json"), Host: &fakeHost{answer: "最少取出 19 个"}})
	if _, err := s.Inspection(TestCandy); err == nil {
		t.Fatal("verdict before any inspection")
	}
	// A config API key is matched by its group.
	req := InspectionRequest{RunRequest: RunRequest{Test: TestCandy, Model: "gpt-6-sol", Runs: 1}, Include: []string{"rel*"}}
	if summary, err := s.Inspect(req); err != nil || summary.Started != 1 {
		t.Fatalf("inspect = %+v, %v", summary, err)
	}
	waitIdle(t, s)
	view, err := s.Inspection(TestCandy)
	if err != nil || view.Running || len(view.Credentials) != 1 || view.Credentials[0].Degraded == nil || !*view.Credentials[0].Degraded {
		t.Fatalf("inspection = %+v, %v", view, err)
	}
	if results := view.Credentials[0].Results.([]CandyResult); results[0].Answer != "最少取出 19 个" {
		t.Fatalf("inspected records = %+v", results)
	}
}

// HandOff does not wait on a request CPA is still opening. The service keeps
// saving afterwards, as it serves on when CPA rolls the replacement back.
func TestHandOffIsBoundedAndSavingResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.json")
	host := &fakeHost{answer: "21", gate: make(chan struct{})}
	s := open(t, Options{Path: path, Host: host})
	// Stop at cleanup waits for the gated request.
	release := sync.OnceFunc(func() { close(host.gate) })
	t.Cleanup(release)
	if _, err := s.Run(RunRequest{Test: TestPelican, All: true, Model: "gpt-6-sol"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the request", func() bool { return host.requests() == 1 })
	if still := s.HandOff(50 * time.Millisecond); still != 1 {
		t.Fatalf("HandOff left %d tests running, want 1", still)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history not written at HandOff: %v", err)
	}
	release()
	waitIdle(t, s)
	if _, err := s.Run(RunRequest{Test: TestCandy, All: true, Model: "gpt-6-sol", Runs: 1}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	s.flushHistory()
	saved, _, err := loadHistory(path)
	if err != nil || len(saved.Results["codex:apikey:1"]) != 1 || len(saved.Pelicans["codex:apikey:1"]) != 1 {
		t.Fatalf("saved after HandOff = %+v, %v", saved, err)
	}
}

// Candy and pelican requests share global slots: a run on many credentials
// queues the rest, and a cancel ends the queued ones at once.
func TestAskRequestsShareGlobalSlots(t *testing.T) {
	keys := make([]Credential, 50)
	for i := range keys {
		keys[i] = Credential{ID: fmt.Sprintf("codex:apikey:%d", i), Provider: "codex", Source: SourceProvider, Name: fmt.Sprint(i)}
	}
	host := &fakeHost{hold: true}
	s := open(t, Options{Host: host, ConfigCredentials: func() ([]Credential, error) { return keys, nil }})
	if summary, err := s.Run(RunRequest{Test: TestCandy, All: true, Model: "gpt-6-sol", Runs: 1}); err != nil || summary.Started != len(keys) {
		t.Fatalf("run = %+v, %v", summary, err)
	}
	waitFor(t, "the slots to fill", func() bool { return host.openStreams() >= askGlobalConcurrency })
	if _, err := s.Cancel(TestCandy, nil, true); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	state, _ := s.State()
	for _, auth := range state.Auths {
		for _, r := range auth.Results {
			if r.Error != cancelledMessage {
				t.Fatalf("%s: %+v", auth.ID, r)
			}
		}
	}
	if host.peak != askGlobalConcurrency {
		t.Fatalf("%d requests at once, want %d", host.peak, askGlobalConcurrency)
	}
}

// A model stuck repeating itself is cut off: the response arrived, so the
// probe does not ask again.
func TestOversizedStreamIsCutOffOnce(t *testing.T) {
	host := &fakeHost{events: `data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"` +
		strings.Repeat("21 ", ModelStreamMaxBytes/3) + `"}]}}` + "\n"}
	c := Credential{ID: "codex:apikey:1", Provider: "codex"}
	_, attempts, err := executeProbe(context.Background(), host, c, "gpt-6-sol", map[string]any{}, nil, make(chan struct{}, 1))
	var answerErr *AnswerError
	if !errors.As(err, &answerErr) || attempts != 1 || host.next != 1 || host.closed != 1 {
		t.Fatalf("attempts %d, streams %d opened %d closed, err %v", attempts, host.next, host.closed, err)
	}
}

// A panic in the host bridge fails the request, not CPA: on the run's own
// goroutine and on the one that closes a cancelled stream.
func TestHostPanicFailsOnlyTheRequest(t *testing.T) {
	s := open(t, Options{Host: panicky{&fakeHost{answer: "21"}, "host.model.stream_read"}})
	if _, err := s.Run(RunRequest{Test: TestCandy, All: true, Model: "gpt-6-sol", Runs: 1}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if state, _ := s.State(); len(state.Auths[0].Results) != 1 || !strings.HasPrefix(state.Auths[0].Results[0].Error, "模型请求异常：") {
		t.Fatalf("candy records = %+v", state.Auths[0].Results)
	}
	host := &fakeHost{hold: true}
	s = open(t, Options{Host: panicky{host, "host.model.stream_close"}})
	if _, err := s.Run(RunRequest{Test: TestPelican, All: true, Model: "gpt-6-sol"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stream to open", func() bool { return host.openStreams() == 1 })
	if _, err := s.Cancel(TestPelican, nil, true); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if state, _ := s.State(); len(state.Auths[0].Pelicans) != 1 || state.Auths[0].Pelicans[0].Error != cancelledMessage {
		t.Fatalf("pelican records = %+v", state.Auths[0].Pelicans)
	}
}

// The subscription of a Codex auth file is read from it again only once the
// file changed.
func TestPlanTypeIsReadOnceItsFileChanges(t *testing.T) {
	list := func(modTime string) string {
		return `{"files":[{"id":"codex-a.json","auth_index":"a1","name":"a@example.com","provider":"codex","source":"file","path":"/auths/codex-a.json","modtime":"` + modTime + `"}]}`
	}
	host := &fakeHost{files: list("2026-10-19T00:00:00Z")}
	s := open(t, Options{Host: host})
	for _, want := range []int{1, 1} {
		if state, err := s.State(); err != nil || state.Auths[0].PlanType != "plus" || host.gets != want {
			t.Fatalf("state = %+v, %v, %d reads", state.Auths, err, host.gets)
		}
	}
	host.mu.Lock()
	host.files = list("2026-10-19T00:05:00Z")
	host.mu.Unlock()
	if state, _ := s.State(); state.Auths[0].PlanType != "plus" || host.gets != 2 {
		t.Fatalf("after the file changed: %+v, %d reads", state.Auths, host.gets)
	}
}

// candy-eval's file that does not load is not imported and saving goes on. A
// history file of its own that does not load pauses saving, which clearing
// all records resumes, setting that file aside.
func TestUnreadableHistory(t *testing.T) {
	dir := t.TempDir()
	path, legacy := filepath.Join(dir, "quality.json"), filepath.Join(dir, "candy-eval.json")
	if err := os.WriteFile(legacy, []byte(`{"results":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, Options{Path: path, Legacy: legacy, Host: &fakeHost{answer: "21"}})
	if _, err := s.Run(RunRequest{Test: TestCandy, All: true, Model: "gpt-6-sol", Runs: 1}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	s.flushHistory()
	if state, _ := s.State(); state.StorageError != "" {
		t.Fatalf("storage error = %q", state.StorageError)
	}
	if saved, _, err := loadHistory(path); err != nil || len(saved.Results["codex:apikey:1"]) != 1 {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	if err := os.WriteFile(path, []byte(`{"results":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, Options{Path: path, Host: &fakeHost{}})
	if state, _ := s.State(); !strings.Contains(state.StorageError, path) || !strings.Contains(state.StorageError, "clear all records") {
		t.Fatalf("storage error = %q", state.StorageError)
	}
	if err := s.Clear("all"); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(path + ".corrupt-*")
	if state, _ := s.State(); state.StorageError != "" || len(aside) != 1 {
		t.Fatalf("storage error %q, set aside %v", state.StorageError, aside)
	}
	if _, found, err := loadHistory(path); !found || err != nil {
		t.Fatalf("history after clearing: found %v, %v", found, err)
	}
}

// Records expire after historyRetention, and a credential left with none is
// forgotten.
func TestOldRecordsExpire(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old, recent := now.AddDate(0, 0, -100), now.AddDate(0, 0, -1)
	h := emptyHistory()
	h.Results["deleted"] = []CandyResult{{Time: old}}
	h.Results["codex:apikey:1"] = []CandyResult{{Time: old}, {Time: recent}, {Time: now}}
	h.Pelicans["deleted"] = []PelicanResult{{ID: "1", Time: old}}
	data, _ := json.Marshal(h)
	path := filepath.Join(t.TempDir(), "quality.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	open(t, Options{Path: path, Host: &fakeHost{}, Now: func() time.Time { return now }}).Stop()
	saved, _, err := loadHistory(path)
	if _, kept := saved.Results["deleted"]; err != nil || kept || len(saved.Pelicans) != 0 || len(saved.Results["codex:apikey:1"]) != 2 {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
}
