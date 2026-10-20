package quality

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Errors the management routes tell apart; each wraps a detail.
var (
	ErrInvalid     = errors.New("invalid detection request")
	ErrUnavailable = errors.New("detection is paused while another plugin instance owns the data files")
	ErrHost        = errors.New("CPA credential list unavailable")
	ErrNotFound    = errors.New("record not found")
	ErrPersist     = errors.New("detection history not saved")
	ErrBusy        = errors.New("an inspection round is already under way")
)

// Test names.
const (
	TestCandy       = "candy"
	TestFingerprint = "fingerprint"
	TestModelTrace  = "modeltrace"
	TestPelican     = "pelican"
)

// Options configures a Service.
type Options struct {
	// Path is the history file next to the plugin state; Legacy is
	// candy-eval's state file, imported once while Path does not exist.
	Path   string
	Legacy string
	// InspectionPath is the inspection's settings and rounds; Location is
	// the timezone of its times of day, UTC when nil.
	InspectionPath string
	Location       *time.Location
	Host           Host
	// ConfigCredentials lists the API keys of CPA's config.yaml, which the
	// auth manager list leaves out, with masked names; nil lists none.
	ConfigCredentials func() ([]Credential, error)
	// CanWrite reports whether this instance owns the data files; tests
	// start and history is written only while it does.
	CanWrite func() bool
	Logf     func(level, message string)
	Now      func() time.Time
}

// Service runs the detection tests on CPA credentials and keeps their
// history. A credential runs one test at a time; different credentials run
// in parallel, fingerprint and ModelTrace requests within global limits.
type Service struct {
	host              Host
	configCredentials func() ([]Credential, error)
	path              string
	canWrite          func() bool
	logf              func(level, message string)
	now               func() time.Time

	mu           sync.Mutex
	hist         history
	loadErr      error
	storageError string
	candy        map[string]*candyProgress
	fingerprint  map[string]*fingerprintProgress
	trace        map[string]*traceProgress
	pelican      map[string]*pelicanProgress
	// The daily inspection: its file, the timezone of its times, what the
	// file holds or why it could not be read, the round under way and the
	// last error a scheduled round failed to start with.
	inspectionPath         string
	location               *time.Location
	inspection             inspectionFile
	inspectionErr          error
	inspectionStorageError string
	round                  *roundProgress
	tickError              string
	// plans caches the subscription of Codex auth files by auth_index.
	plans map[string]cachedPlan
	// dirty marks records not written yet, flushArmed a write flushTimer is
	// due to make; once stopped, no write is scheduled.
	dirty, flushArmed, stopped bool
	flushTimer                 *time.Timer
	// tasks counts run goroutines; Stop waits for them.
	tasks sync.WaitGroup

	fingerprintSlots chan struct{}
	traceSlots       chan struct{}
	askSlots         chan struct{}
}

type cachedPlan struct {
	modTime time.Time
	plan    string
}

type candyProgress struct {
	Done   int `json:"done"`
	Total  int `json:"total"`
	cancel context.CancelFunc
}

type fingerprintProgress struct {
	Model       string `json:"model"`
	Mode        string `json:"mode"`
	Done        int    `json:"done"`
	Total       int    `json:"total"`
	Valid       int    `json:"valid"`
	Errors      int    `json:"errors"`
	Phase       string `json:"phase"`
	Concurrency int    `json:"concurrency"`
	cancel      context.CancelFunc
}

type traceProgress struct {
	Model       string `json:"model"`
	Phase       string `json:"phase"`
	Done        int    `json:"done"`
	Concurrency int    `json:"concurrency"`
	cancel      context.CancelFunc
}

type pelicanProgress struct {
	Model  string `json:"model"`
	Phase  string `json:"phase"`
	cancel context.CancelFunc
}

// Open loads the history. Nil options default to time.Now, always writable
// and no logging.
func Open(opts Options) *Service {
	s := &Service{
		host: opts.Host, configCredentials: opts.ConfigCredentials, path: opts.Path, canWrite: opts.CanWrite, logf: opts.Logf, now: opts.Now,
		inspectionPath: opts.InspectionPath, location: opts.Location,
		hist: emptyHistory(), candy: map[string]*candyProgress{}, fingerprint: map[string]*fingerprintProgress{},
		trace: map[string]*traceProgress{}, pelican: map[string]*pelicanProgress{},
		fingerprintSlots: make(chan struct{}, FingerprintMaxConcurrency), traceSlots: make(chan struct{}, traceGlobalConcurrency),
		askSlots: make(chan struct{}, askGlobalConcurrency),
	}
	if s.canWrite == nil {
		s.canWrite = func() bool { return true }
	}
	if s.logf == nil {
		s.logf = func(string, string) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.location == nil {
		s.location = time.UTC
	}
	s.mu.Lock()
	s.openHistory(opts.Legacy)
	s.openInspection()
	s.mu.Unlock()
	return s
}

// traceGlobalConcurrency caps ModelTrace requests across credentials, as
// candy-eval does.
const traceGlobalConcurrency = 6

// askGlobalConcurrency caps the candy and pelican requests across
// credentials: each carries Codex's long context, and API keys of CPA's config
// may share one upstream account.
const askGlobalConcurrency = 16

// CredentialView is one credential with its running test and records.
type CredentialView struct {
	Credential
	Running            *candyProgress       `json:"running,omitempty"`
	Results            []CandyResult        `json:"results"`
	FingerprintRunning *fingerprintProgress `json:"fingerprint_running,omitempty"`
	Fingerprints       []FingerprintResult  `json:"fingerprints"`
	ModelTraceRunning  *traceProgress       `json:"modeltrace_running,omitempty"`
	ModelTraces        []TraceResult        `json:"modeltraces"`
	PelicanRunning     *pelicanProgress     `json:"pelican_running,omitempty"`
	Pelicans           []PelicanResult      `json:"pelicans"`
}

// StateView is what the detection page shows. ConfigError says why the API
// keys of CPA's config are missing.
type StateView struct {
	Auths        []CredentialView `json:"auths"`
	StorageError string           `json:"storage_error"`
	ConfigError  string           `json:"config_error,omitempty"`
	Config       map[string]any   `json:"config"`
}

// State lists the credentials with their records, which are summaries:
// Record returns one in full. Host calls stay outside the lock.
func (s *Service) State() (StateView, error) {
	credentials, configErr, err := s.credentials()
	if err != nil {
		return StateView{}, err
	}
	s.planTypes(credentials)
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]CredentialView, 0, len(credentials))
	for _, c := range credentials {
		views = append(views, CredentialView{
			Credential: c,
			Running:    copyProgress(s.candy[c.ID]), Results: summaries(s.hist.Results[c.ID]),
			FingerprintRunning: copyProgress(s.fingerprint[c.ID]), Fingerprints: summaries(s.hist.Fingerprints[c.ID]),
			ModelTraceRunning: copyProgress(s.trace[c.ID]), ModelTraces: summaries(s.hist.ModelTraces[c.ID]),
			PelicanRunning: copyProgress(s.pelican[c.ID]), Pelicans: summaries(s.hist.Pelicans[c.ID]),
		})
	}
	return StateView{Auths: views, StorageError: s.storageError, ConfigError: configErr, Config: configView()}, nil
}

// planTypes fills in the subscription of the Codex auth files the list does
// not label. host.auth.get clones CPA's whole credential list and reads the
// file, so a plan is looked up again only once its file changed; a failed
// lookup is not kept.
func (s *Service) planTypes(credentials []Credential) {
	s.mu.Lock()
	known := s.plans
	s.mu.Unlock()
	plans := make(map[string]cachedPlan, len(known))
	for i := range credentials {
		c := &credentials[i]
		if c.PlanType = strings.TrimSpace(c.PlanType); c.PlanType != "" || c.Source != SourceFile || c.Provider != "codex" || c.AuthIndex == "" {
			continue
		}
		cached, hit := known[c.AuthIndex]
		if !hit || c.modTime.IsZero() || !cached.modTime.Equal(c.modTime) {
			plan, ok := planType(s.host, c.AuthIndex)
			if !ok {
				continue
			}
			cached = cachedPlan{modTime: c.modTime, plan: plan}
		}
		c.PlanType = cached.plan
		if !c.modTime.IsZero() {
			plans[c.AuthIndex] = cached
		}
	}
	s.mu.Lock()
	s.plans = plans
	s.mu.Unlock()
}

// credentials lists what a test can be pinned to: the auth manager's
// credentials, then the API keys of CPA's config ordered by group and name.
// When the config cannot be read only the former are listed, and configErr
// says why.
func (s *Service) credentials() (list []Credential, configErr string, err error) {
	list, err = listCredentials(s.host, s.now())
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrHost, err)
	}
	if s.configCredentials == nil {
		return list, "", nil
	}
	configured, readErr := s.configCredentials()
	if readErr != nil {
		configErr = readErr.Error()
	}
	slices.SortFunc(configured, func(a, b Credential) int {
		return cmp.Or(strings.Compare(a.Group, b.Group), strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	for _, c := range configured {
		if !slices.ContainsFunc(list, func(listed Credential) bool { return listed.ID == c.ID }) {
			list = append(list, c)
		}
	}
	return list, configErr, nil
}

func configView() map[string]any {
	return map[string]any{
		TestCandy:       map[string]any{"max_runs": CandyMaxRuns, "prompt": CandyPrompt, "answer": "21", "max_parallel": askGlobalConcurrency},
		TestFingerprint: map[string]any{"modes": FingerprintModes, "default_concurrency": FingerprintDefaultConcurrency, "max_concurrency": FingerprintMaxConcurrency},
		TestModelTrace:  map[string]any{"requests": TraceTarget, "default_concurrency": TraceDefaultConcurrency, "max_concurrency": TraceMaxConcurrency, "bank_revision": TraceBankRevision},
		TestPelican:     map[string]any{"prompt": PelicanPrompt, "max_parallel": askGlobalConcurrency},
	}
}

func copyProgress[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func summaries[T interface{ Summary() T }](records []T) []T {
	out := make([]T, len(records))
	for i, r := range records {
		out[i] = r.Summary()
	}
	return out
}

// RunRequest starts one test on credentials. Runs applies to the candy test,
// Mode to fingerprints, Concurrency to fingerprints and ModelTrace, Effort to
// candy and pelican ("none" leaves it to CPA).
type RunRequest struct {
	Test        string   `json:"test"`
	AuthIDs     []string `json:"auth_ids"`
	All         bool     `json:"all"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	Runs        int      `json:"runs"`
	Mode        string   `json:"mode"`
	Concurrency int      `json:"concurrency"`
}

// RunSummary counts what a run request started.
type RunSummary struct {
	Started int `json:"started"`
	Busy    int `json:"busy,omitempty"`
}

// Run starts req.Test on every selected credential that is enabled, not
// cooling down and idle.
func (s *Service) Run(req RunRequest) (RunSummary, error) {
	start, err := s.prepare(&req)
	if err != nil {
		return RunSummary{}, err
	}
	if !s.canWrite() {
		return RunSummary{}, ErrUnavailable
	}
	credentials, err := s.selected(req.AuthIDs, req.All)
	if err != nil {
		return RunSummary{}, err
	}
	if len(credentials) == 0 {
		return RunSummary{}, fmt.Errorf("%w: no enabled credential to test", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return RunSummary{}, ErrUnavailable
	}
	summary := RunSummary{}
	for _, c := range credentials {
		if s.busyLocked(c.ID) {
			summary.Busy++
			continue
		}
		summary.Started++
		ctx, cancel := context.WithCancel(context.Background())
		s.tasks.Add(1)
		start(ctx, cancel, c)
	}
	return summary, nil
}

// prepare validates req and returns the function that starts it on one
// credential with s.mu held.
func (s *Service) prepare(req *RunRequest) (func(context.Context, context.CancelFunc, Credential), error) {
	req.Model, req.Effort = strings.TrimSpace(req.Model), strings.TrimSpace(req.Effort)
	switch req.Test {
	case TestCandy:
		if !ValidModelName(req.Model, true) {
			return nil, fmt.Errorf("%w: choose a model", ErrInvalid)
		}
		runs := min(max(req.Runs, 1), CandyMaxRuns)
		return func(ctx context.Context, cancel context.CancelFunc, c Credential) {
			p := &candyProgress{Total: runs, cancel: cancel}
			s.candy[c.ID] = p
			go s.runCandy(ctx, c, req.Model, req.Effort, runs, p)
		}, nil
	case TestFingerprint:
		if !ValidModelName(req.Model, false) {
			return nil, fmt.Errorf("%w: choose a model without a reasoning-effort suffix", ErrInvalid)
		}
		i := slices.IndexFunc(FingerprintModes, func(m FingerprintMode) bool { return m.ID == req.Mode })
		if i < 0 {
			return nil, fmt.Errorf("%w: choose the quick, standard or strict mode", ErrInvalid)
		}
		mode := FingerprintModes[i]
		concurrency := req.Concurrency
		if concurrency == 0 {
			concurrency = FingerprintDefaultConcurrency
		}
		if concurrency < 1 || concurrency > FingerprintMaxConcurrency {
			return nil, fmt.Errorf("%w: concurrency per credential must be 1-%d", ErrInvalid, FingerprintMaxConcurrency)
		}
		return func(ctx context.Context, cancel context.CancelFunc, c Credential) {
			p := &fingerprintProgress{Model: req.Model, Mode: mode.ID, Total: mode.Cells * mode.Samples, Phase: "collecting", Concurrency: concurrency, cancel: cancel}
			s.fingerprint[c.ID] = p
			go s.runFingerprint(ctx, c, req.Model, mode, p)
		}, nil
	case TestModelTrace:
		if !ValidModelName(req.Model, false) {
			return nil, fmt.Errorf("%w: choose a model without a reasoning-effort suffix", ErrInvalid)
		}
		concurrency := req.Concurrency
		if concurrency == 0 {
			concurrency = TraceDefaultConcurrency
		}
		if concurrency < 1 || concurrency > TraceMaxConcurrency {
			return nil, fmt.Errorf("%w: concurrency per credential must be 1-%d", ErrInvalid, TraceMaxConcurrency)
		}
		return func(ctx context.Context, cancel context.CancelFunc, c Credential) {
			p := &traceProgress{Model: req.Model, Phase: "collecting", Concurrency: concurrency, cancel: cancel}
			s.trace[c.ID] = p
			go s.runModelTrace(ctx, c, req.Model, concurrency, p)
		}, nil
	case TestPelican:
		if !ValidModelName(req.Model, true) {
			return nil, fmt.Errorf("%w: choose a model", ErrInvalid)
		}
		return func(ctx context.Context, cancel context.CancelFunc, c Credential) {
			p := &pelicanProgress{Model: req.Model, Phase: "collecting", cancel: cancel}
			s.pelican[c.ID] = p
			go s.runPelican(ctx, c, req.Model, req.Effort, p)
		}, nil
	default:
		return nil, fmt.Errorf("%w: unknown test %q", ErrInvalid, req.Test)
	}
}

// selected returns the enabled, available credentials among ids, or all of
// them.
func (s *Service) selected(ids []string, all bool) ([]Credential, error) {
	credentials, _, err := s.credentials()
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	out := credentials[:0]
	for _, c := range credentials {
		if !c.Disabled && !c.Unavailable && (all || wanted[c.ID]) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) busyLocked(id string) bool {
	return s.candy[id] != nil || s.fingerprint[id] != nil || s.trace[id] != nil || s.pelican[id] != nil
}

// Cancel stops test on the given credentials, or on all of them.
func (s *Service) Cancel(test string, ids []string, all bool) (int, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	cancel := func(id string, phase *string, stop context.CancelFunc) {
		if all || wanted[id] {
			if phase != nil {
				*phase = "cancelling"
			}
			stop()
			count++
		}
	}
	switch test {
	case TestCandy:
		for id, p := range s.candy {
			cancel(id, nil, p.cancel)
		}
	case TestFingerprint:
		for id, p := range s.fingerprint {
			cancel(id, &p.Phase, p.cancel)
		}
	case TestModelTrace:
		for id, p := range s.trace {
			cancel(id, &p.Phase, p.cancel)
		}
	case TestPelican:
		for id, p := range s.pelican {
			cancel(id, &p.Phase, p.cancel)
		}
	default:
		return 0, fmt.Errorf("%w: unknown test %q", ErrInvalid, test)
	}
	return count, nil
}

// Stop cancels every running test and returns once their goroutines have
// ended and the history is written; no write is scheduled after it. The
// plugin calls it when it closes the service: CPA may free the host callbacks
// right after, so nothing may be left reading a model stream.
func (s *Service) Stop() {
	s.mu.Lock()
	// Set before the cancel, under the lock Run starts tests with: a run
	// either started before (and is cancelled here) or is refused, so no
	// tasks.Add can race the Wait below.
	s.stopped = true
	s.cancelAllLocked()
	s.mu.Unlock()
	s.tasks.Wait()
	s.mu.Lock()
	s.flushNowLocked()
	s.mu.Unlock()
}

// Resume lets a stopped service start tests and schedule history writes
// again: CPA rolled back to the instance whose shutdown stopped it. Stop's
// Wait has returned, so the WaitGroup may be reused.
func (s *Service) Resume() {
	s.mu.Lock()
	s.stopped = false
	s.mu.Unlock()
}

// HandOff cancels every test like Stop but waits at most limit for them to
// end, and returns how many still run. A request CPA is still opening cannot
// be cut short (the stream is closable only once execute_stream answers), and
// a hot replacement must not wait on an upstream that does not answer. Tests
// that end later close their own streams; their records are not saved, the
// files belong to the newer instance by then. A rolled-back replacement keeps
// using the service, which saves as before.
func (s *Service) HandOff(limit time.Duration) int {
	s.mu.Lock()
	s.cancelAllLocked()
	s.mu.Unlock()
	// Polled rather than tasks.Wait in a goroutine: after a rolled-back
	// replacement a new run's tasks.Add would race that Wait.
	deadline := time.Now().Add(limit)
	for s.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushNowLocked()
	return s.runningLocked()
}

func (s *Service) cancelAllLocked() {
	for _, p := range s.candy {
		p.cancel()
	}
	for _, p := range s.fingerprint {
		p.Phase = "cancelling"
		p.cancel()
	}
	for _, p := range s.trace {
		p.Phase = "cancelling"
		p.cancel()
	}
	for _, p := range s.pelican {
		p.Phase = "cancelling"
		p.cancel()
	}
	if s.round != nil {
		s.round.cancel()
	}
}

// Running counts the credentials with a test in progress, and an inspection
// round under way.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningLocked()
}

func (s *Service) runningLocked() int {
	running := len(s.candy) + len(s.fingerprint) + len(s.trace) + len(s.pelican)
	if s.round != nil {
		running++
	}
	return running
}

// Record returns one record in full. A candy record has no ID: id is its
// time as its JSON writes it, RFC 3339 with nanoseconds.
func (s *Service) Record(test, authID, id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var record any
	switch test {
	case TestCandy:
		record = find(s.hist.Results[authID], func(r CandyResult) bool { return r.Time.Format(time.RFC3339Nano) == id })
	case TestFingerprint:
		record = find(s.hist.Fingerprints[authID], func(r FingerprintResult) bool { return r.ID == id })
	case TestModelTrace:
		record = find(s.hist.ModelTraces[authID], func(r TraceResult) bool { return r.ID == id })
	case TestPelican:
		record = find(s.hist.Pelicans[authID], func(r PelicanResult) bool { return r.ID == id })
	default:
		return nil, fmt.Errorf("%w: unknown test %q", ErrInvalid, test)
	}
	if record == nil {
		return nil, fmt.Errorf("%w: it may have been cleared", ErrNotFound)
	}
	return record, nil
}

func find[T any](records []T, match func(T) bool) any {
	if i := slices.IndexFunc(records, match); i >= 0 {
		return records[i]
	}
	return nil
}

// Clear deletes the records of one test, or of all of them. Clearing all of
// them also sets a history file that failed to load aside, so saving resumes.
func (s *Service) Clear(test string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.canWrite() {
		return ErrUnavailable
	}
	previous := s.hist
	next := s.hist
	switch test {
	case TestCandy:
		next.Results = map[string][]CandyResult{}
	case TestFingerprint:
		next.Fingerprints = map[string][]FingerprintResult{}
	case TestModelTrace:
		next.ModelTraces = map[string][]TraceResult{}
	case TestPelican:
		next.Pelicans = map[string][]PelicanResult{}
	case "all":
		next = emptyHistory()
	default:
		return fmt.Errorf("%w: unknown test %q", ErrInvalid, test)
	}
	if test == "all" && s.loadErr != nil {
		if err := s.setAsideUnreadableLocked(); err != nil {
			return fmt.Errorf("%w: cannot set the unreadable file aside: %v", ErrPersist, err)
		}
	}
	s.hist = next
	if err := s.saveLocked(); err != nil {
		s.hist = previous
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}
