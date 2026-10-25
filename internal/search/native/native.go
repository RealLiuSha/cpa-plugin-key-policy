// Package native decides which Codex models search natively: through the
// web_search tool of the upstream serving the conversation, instead of a
// separate POST /v1/alpha/search.
//
// Codex chooses per model from the catalog CPA serves. A Responses Lite model
// (use_responses_lite) is sent no hosted tool and calls /alpha/search, which
// few upstreams implement; any other model declares the hosted web_search tool
// with the conversation. The plugin turns Responses Lite off in the catalog for
// the models whose channels proved they run the tool, so searches follow CPA's
// routing of the conversation across every such channel. A model none of whose
// channels does stays on /alpha/search, the fallback.
//
// The tool runs on whichever channel serves the turn, so a channel that
// ignores it leaves its sessions without search; the request path can only
// take the tool out for a channel (disabled by the operator, or refusing it),
// never move the turn elsewhere. The status names those gaps per model.
//
// The evidence comes from probes pinned to each channel through CPA (a full
// round every IntervalHours, a new channel at once, a failing or refusing one
// again later) and from client requests an upstream refused for the tool.
// Codex reads the catalog every few minutes, so a change reaches it within
// that.
package native

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

const (
	fileVersion = 1
	// catalogRetention drops a catalog model Codex has not been shown for a
	// week, and catalogTouch is how often a model seen again is written.
	catalogRetention = 7 * 24 * time.Hour
	catalogTouch     = time.Hour
	// startupGrace keeps rounds back after Start: CPA may still be loading
	// its credentials, and a probe then would record their absence.
	startupGrace = 2 * time.Minute
	// A refusing channel is probed again after rejectionRecheck, then every
	// failingRecheck, as a failing one is; refusals within rejectionDebounce
	// of the last only count.
	rejectionRecheck  = 10 * time.Minute
	rejectionDebounce = time.Minute
	failingRecheck    = time.Hour
)

var (
	// ErrReadOnly reports a write refused because this instance no longer
	// owns the data directory.
	ErrReadOnly = errors.New("native search settings are read-only in this plugin instance")
	// ErrBusy reports a probe request while a round runs.
	ErrBusy = errors.New("a native search probe round is running")
	// ErrOff reports a probe request while rounds do not run: native search
	// or the search module is switched off.
	ErrOff = errors.New("native search probes run only while native search and the search module are on")
	// ErrNotServing is what Upstream.Stream wraps when CPA has no route to the
	// model on the pinned channel; CPA answers it without an upstream request.
	ErrNotServing = errors.New("the channel does not serve the model")
	// ErrUnknownChannel reports a probe request for a channel CPA does not
	// list, or one serving no catalog model.
	ErrUnknownChannel = errors.New("no such codex channel serving a catalog model")
)

// Credential is one codex credential CPA can pin a request to. Models are the
// names its config lists (nil: the provider's own models); Disabled says CPA
// routes nothing to it, Unavailable that it is cooling down.
type Credential struct {
	AuthIndex   string   `json:"auth_index"`
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	Name        string   `json:"name"`
	Group       string   `json:"group,omitempty"`
	Models      []string `json:"models,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	Unavailable bool     `json:"unavailable,omitempty"`
}

// Upstream is CPA as the probes see it.
type Upstream interface {
	// Credentials lists the credentials CPA can pin to; an error means the
	// list is unknown, not empty.
	Credentials() ([]Credential, error)
	// Stream sends a streaming Responses request pinned to c and returns the
	// complete event stream, or the upstream status and an error; it wraps
	// ErrNotServing when CPA has no route to model on c.
	Stream(ctx context.Context, c Credential, model string, body any, headers http.Header) ([]byte, int, error)
}

// Rejection is a client request that failed because the upstream refused
// the web_search tool. It holds until a probe after At searches.
type Rejection struct {
	At        time.Time `json:"at"`
	Count     int       `json:"count"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	Error     string    `json:"error"`
	RecheckAt time.Time `json:"recheck_at"`
}

// channelRecord is the evidence about one channel.
type channelRecord struct {
	AuthIndex string     `json:"auth_index"`
	Probes    []Probe    `json:"probes"`
	Rejection *Rejection `json:"rejection,omitempty"`
	// NotServing and Served list the catalog models CPA had no route to, or
	// did route a probe to, on a credential whose config lists none.
	NotServing []string `json:"not_serving,omitempty"`
	Served     []string `json:"served,omitempty"`
	// Rotation picks the model of the next probe among those served.
	Rotation int `json:"rotation,omitempty"`
}

type catalogModel struct {
	Slug   string    `json:"slug"`
	SeenAt time.Time `json:"seen_at"`
}

type fileData struct {
	Version       int             `json:"version"`
	Settings      Settings        `json:"settings"`
	Catalog       []catalogModel  `json:"catalog"`
	Credentials   []Credential    `json:"credentials"`
	Channels      []channelRecord `json:"channels"`
	LastFullRound time.Time       `json:"last_full_round,omitzero"`
}

// Round kinds.
const (
	RoundFull     = "full"
	RoundDiscover = "discover"
	RoundRecheck  = "recheck"
	RoundManual   = "manual"
)

type roundState struct {
	kind    string
	started time.Time
	total   int
	done    int
	cancel  context.CancelFunc
	// ended is closed after the round's last host call.
	ended chan struct{}
}

// job is one channel of a round: models in the order to try, the first one
// CPA routes being probed.
type job struct {
	credential Credential
	models     []string
}

// Options configure Open.
type Options struct {
	Path     string
	CanWrite func() bool
	Logf     func(level, message string)
	Upstream Upstream
	// Now replaces the clock; tests only.
	Now func() time.Time
}

// Manager owns the native search settings, the evidence and the probe
// rounds. The request path (RewriteCatalog, Strips, ObserveFailure) reads an
// immutable snapshot; ObserveFailure takes the lock only when it records a
// refusal.
type Manager struct {
	path     string
	canWrite func() bool
	logf     func(level, message string)
	upstream Upstream
	now      func() time.Time

	snapshot atomic.Pointer[decisions]

	// mu guards everything below; rounds take it only to record.
	mu          sync.Mutex
	settings    Settings
	catalog     map[string]time.Time
	credentials []Credential
	credErr     string
	records     map[string]*channelRecord
	lastFull    time.Time
	dirty       bool
	round       *roundState
	started     bool
	startedAt   time.Time
}

// Open loads the file at path; a missing one means DefaultSettings and no
// evidence. A corrupt file or another version is an error, and the plugin
// then shows the module as unavailable. Nothing is probed before Start.
func Open(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Path) == "" || opts.Upstream == nil {
		return nil, errors.New("native search needs a file path and CPA")
	}
	m := &Manager{path: opts.Path, canWrite: opts.CanWrite, logf: opts.Logf, upstream: opts.Upstream, now: opts.Now,
		catalog: map[string]time.Time{}, records: map[string]*channelRecord{}}
	if m.canWrite == nil {
		m.canWrite = func() bool { return true }
	}
	if m.logf == nil {
		m.logf = func(string, string) {}
	}
	if m.now == nil {
		m.now = time.Now
	}
	data, err := load(opts.Path)
	if err != nil {
		return nil, err
	}
	m.settings, m.credentials, m.lastFull = data.Settings, data.Credentials, data.LastFullRound
	for _, c := range data.Catalog {
		m.catalog[c.Slug] = c.SeenAt
	}
	for i := range data.Channels {
		rec := data.Channels[i]
		m.records[rec.AuthIndex] = &rec
	}
	if m.canWrite() {
		if err := persist.CleanupStaleTemps(opts.Path, m.now()); err != nil {
			m.log("warn", "native search: cannot remove stale temporary files: "+err.Error())
		}
	}
	m.mu.Lock()
	m.rebuildLocked()
	m.mu.Unlock()
	return m, nil
}

func load(path string) (fileData, error) {
	raw, err := persist.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileData{Version: fileVersion, Settings: DefaultSettings()}, nil
	}
	if err != nil {
		return fileData{}, fmt.Errorf("read native search file: %w", err)
	}
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fileData{}, fmt.Errorf("decode native search file %s: %w", path, err)
	}
	if data.Version != fileVersion {
		return fileData{}, fmt.Errorf("native search file %s: unsupported version %d; require version %d", path, data.Version, fileVersion)
	}
	if data.Settings, err = Normalize(data.Settings); err != nil {
		return fileData{}, fmt.Errorf("native search file %s: %w", path, err)
	}
	return data, nil
}

// saveLocked writes the file when this instance may. Probes older than the
// channel history are not kept, and channels a complete credential list no
// longer has are dropped.
func (m *Manager) saveLocked() error {
	if !m.canWrite() {
		return ErrReadOnly
	}
	data := fileData{Version: fileVersion, Settings: m.settings, Credentials: m.credentials, LastFullRound: m.lastFull,
		Catalog: []catalogModel{}, Channels: []channelRecord{}}
	now := m.now()
	for slug, seen := range m.catalog {
		if now.Sub(seen) > catalogRetention {
			delete(m.catalog, slug)
			continue
		}
		data.Catalog = append(data.Catalog, catalogModel{Slug: slug, SeenAt: seen})
	}
	slices.SortFunc(data.Catalog, func(a, b catalogModel) int { return strings.Compare(a.Slug, b.Slug) })
	for _, rec := range m.records {
		data.Channels = append(data.Channels, *rec)
	}
	slices.SortFunc(data.Channels, func(a, b channelRecord) int { return strings.Compare(a.AuthIndex, b.AuthIndex) })
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode native search file: %w", err)
	}
	if err := persist.AtomicWrite(m.path, raw); err != nil {
		return fmt.Errorf("save native search file: %w", err)
	}
	m.dirty = false
	return nil
}

// flushLocked saves pending evidence; a failure is logged and retried at the
// next tick.
func (m *Manager) flushLocked() {
	if !m.dirty || !m.canWrite() {
		return
	}
	if err := m.saveLocked(); err != nil {
		m.log("warn", "native search: "+err.Error())
	}
}

// candidatesLocked lists the catalog models seen within catalogRetention.
func (m *Manager) candidatesLocked(now time.Time) []string {
	out := make([]string, 0, len(m.catalog))
	for slug, seen := range m.catalog {
		if now.Sub(seen) <= catalogRetention {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return out
}

// rebuildLocked recomputes the snapshot and logs every model that changed
// how it searches.
func (m *Manager) rebuildLocked() {
	now := m.now()
	previous := m.snapshot.Load()
	next := decide(m.settings, m.candidatesLocked(now), m.credentials, m.records, now)
	m.snapshot.Store(next)
	if previous == nil {
		return
	}
	for _, model := range next.models {
		i := slices.IndexFunc(previous.models, func(d ModelDecision) bool { return d.Model == model.Model })
		if i >= 0 && previous.models[i].Mode != model.Mode {
			m.log("info", fmt.Sprintf("native search: %s now %s (was %s; %d supported channels)", model.Model, model.Mode, previous.models[i].Mode, len(model.Usable)))
		}
	}
}

// RewriteCatalog returns a Codex catalog body with Responses Lite turned off
// for the native models, or nil to keep the body as it is: not a catalog, or
// nothing to change. It also records the catalog's candidate models.
func (m *Manager) RewriteCatalog(body []byte) []byte {
	candidates, rewritten, ok := rewriteCatalog(body, m.snapshot.Load().native)
	if !ok {
		return nil
	}
	m.observeCandidates(candidates)
	return rewritten
}

func (m *Manager) observeCandidates(candidates []string) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	added := false
	for _, slug := range candidates {
		seen, known := m.catalog[slug]
		if known && now.Sub(seen) < catalogTouch {
			continue
		}
		m.catalog[slug], m.dirty = now.UTC(), true
		added = added || !known || now.Sub(seen) > catalogRetention
	}
	if added {
		m.rebuildLocked()
	}
}

// Strips reports whether the web_search tool of a request for model is to be
// taken out on the channel at authIndex: the model is a catalog candidate,
// which a client sends the tool for while it holds a catalog in which the
// model searched natively, and the operator disabled the channel or it
// refused the tool.
func (m *Manager) Strips(authIndex, model string) bool {
	d := m.snapshot.Load()
	return d.strip[authIndex] && d.candidates[model]
}

// StripWebSearch takes the hosted web_search tools out of a request body;
// ok is false when it has none.
func StripWebSearch(body []byte) ([]byte, bool) { return stripWebSearch(body) }

// ObserveFailure looks at one failed client request: an upstream that refused
// the web_search tool of a catalog candidate (400, 404 or 422 naming
// web_search) is marked as refusing, its sessions stop being sent the tool,
// and it is probed again after rejectionRecheck.
func (m *Manager) ObserveFailure(authIndex, model string, status int, message string) {
	if d := m.snapshot.Load(); authIndex == "" || !d.enabled || !d.candidates[model] {
		return
	}
	if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusUnprocessableEntity {
		return
	}
	if !strings.Contains(strings.ToLower(message), "web_search") {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(authIndex)
	count := 1
	if previous := rec.Rejection; previous != nil {
		count = previous.Count + 1
		if now.Sub(previous.At) < rejectionDebounce {
			previous.Count, m.dirty = count, true
			return
		}
	}
	rec.Rejection = &Rejection{At: now.UTC(), Count: count, Model: model, Status: status, Error: truncate(message), RecheckAt: now.Add(rejectionRecheck).UTC()}
	m.dirty = true
	m.rebuildLocked()
	m.log("warn", fmt.Sprintf("native search: channel %s refused the web_search tool for %s (HTTP %d); its sessions search no more until a probe passes", authIndex, model, status))
}

func (m *Manager) recordLocked(authIndex string) *channelRecord {
	rec := m.records[authIndex]
	if rec == nil {
		rec = &channelRecord{AuthIndex: authIndex}
		m.records[authIndex] = rec
	}
	return rec
}

// Settings returns the current settings.
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.clone()
}

// Update normalizes and stores new settings and returns the previous and
// stored ones. Equal settings are not written. Switching native search off
// cancels the round under way. A read-only instance returns ErrReadOnly,
// invalid settings an error wrapping ErrInvalidSettings; after any error the
// settings are unchanged.
func (m *Manager) Update(next Settings) (previous, stored Settings, err error) {
	if !m.canWrite() {
		return Settings{}, Settings{}, ErrReadOnly
	}
	normalized, err := Normalize(next)
	if err != nil {
		return Settings{}, Settings{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous = m.settings.clone()
	if reflect.DeepEqual(normalized, m.settings) {
		return previous, previous, nil
	}
	m.settings = normalized
	if err := m.saveLocked(); err != nil {
		m.settings = previous
		return Settings{}, Settings{}, err
	}
	if !normalized.Enabled && m.round != nil {
		m.round.cancel()
	}
	m.rebuildLocked()
	return previous, normalized.clone(), nil
}

// Start lets Tick run probe rounds; rounds begin startupGrace later. The
// plugin calls it when the search module is switched on and registered.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		m.started, m.startedAt = true, m.now()
	}
}

// Stop ends the round under way and waits for it: CPA may free the host
// callbacks right after the plugin shuts down. Evidence already recorded
// stays.
func (m *Manager) Stop() {
	m.HandOff(-1)
}

// HandOff stops rounds for a hot replacement and waits up to limit (forever
// when negative) for the one under way, whose probe may wait on CPA to open a
// stream; it returns how many rounds still end. What they record later is not
// saved once this instance gives the files up. Each round has its own ended
// channel, so a round a rolled-back replacement starts never races this wait.
func (m *Manager) HandOff(limit time.Duration) int {
	m.mu.Lock()
	m.started = false
	round := m.round
	if round != nil {
		round.cancel()
	}
	m.flushLocked()
	m.mu.Unlock()
	if round == nil {
		return 0
	}
	if limit < 0 {
		<-round.ended
		return 0
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-round.ended:
		return 0
	case <-timer.C:
	}
	// select picks at random when both are ready: the round ended as the
	// time ran out, or the limit is zero.
	select {
	case <-round.ended:
		return 0
	default:
		return 1
	}
}

// Tick runs the periodic work: it refreshes the credential list, starts the
// round that is due and writes pending evidence. The plugin calls it every
// minute. Before Start it only keeps the decisions current, without asking
// CPA for anything.
func (m *Manager) Tick() {
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	var credentials []Credential
	var listErr error
	if started {
		credentials, listErr = m.upstream.Credentials()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if started {
		m.applyCredentialsLocked(credentials, listErr)
	}
	m.rebuildLocked()
	now := m.now()
	if m.started && m.round == nil && m.settings.Enabled && m.credErr == "" && now.Sub(m.startedAt) >= startupGrace {
		if kind, jobs := m.dueLocked(now); len(jobs) > 0 {
			m.startRoundLocked(kind, jobs)
		}
	}
	m.flushLocked()
}

// applyCredentialsLocked keeps the codex credentials of a complete list and
// drops the evidence of channels it no longer has; after a failed list the
// last one stays.
func (m *Manager) applyCredentialsLocked(listed []Credential, listErr error) {
	if listErr != nil {
		m.credErr = listErr.Error()
		return
	}
	m.credErr = ""
	codex := []Credential{}
	for _, c := range listed {
		if strings.EqualFold(c.Provider, "codex") && c.AuthIndex != "" && c.ID != "" {
			codex = append(codex, c)
		}
	}
	slices.SortFunc(codex, func(a, b Credential) int {
		return cmp.Or(strings.Compare(a.Group, b.Group), strings.Compare(a.Name, b.Name), strings.Compare(a.AuthIndex, b.AuthIndex))
	})
	if reflect.DeepEqual(codex, m.credentials) {
		return
	}
	m.credentials, m.dirty = codex, true
	for index := range m.records {
		if !slices.ContainsFunc(codex, func(c Credential) bool { return c.AuthIndex == index }) {
			delete(m.records, index)
		}
	}
}

// servedLocked lists the catalog models channel c may serve, in probe
// order: for a credential whose config lists none, the models it was never
// probed with first, so CPA tells which it lacks (free) until one is served;
// then from the rotation on, so successive rounds cover every model.
func (m *Manager) servedLocked(c Credential, candidates []string) []string {
	rec := m.records[c.AuthIndex]
	unverified, served := []string{}, []string{}
	for _, model := range candidates {
		switch {
		case !serves(c, rec, model):
		case c.Models == nil && !servesKnown(c, rec, model):
			unverified = append(unverified, model)
		default:
			served = append(served, model)
		}
	}
	if rec != nil && len(served) > 1 {
		start := rec.Rotation % len(served)
		served = append(served[start:], served[:start]...)
	}
	return append(unverified, served...)
}

// dueLocked picks the round to start: a full round of every channel once the
// interval passed since the last; else the channels never probed, refusing
// past their recheck time, or failing for failingRecheck.
func (m *Manager) dueLocked(now time.Time) (string, []job) {
	candidates := m.candidatesLocked(now)
	if len(candidates) == 0 {
		return "", nil
	}
	full := m.lastFull.IsZero() || now.Sub(m.lastFull) >= time.Duration(m.settings.IntervalHours)*time.Hour
	kind, jobs := RoundFull, []job{}
	if !full {
		kind = ""
	}
	for _, c := range m.credentials {
		if c.Disabled || c.Unavailable {
			continue
		}
		rec := m.records[c.AuthIndex]
		models := m.servedLocked(c, candidates)
		if full && c.Models == nil && rec != nil {
			// Ask again, free when CPA still has no route, about the models it
			// had none to.
			models = append(slices.Clone(rec.NotServing), models...)
		}
		if len(models) == 0 {
			continue
		}
		if full {
			jobs = append(jobs, job{credential: c, models: models})
			continue
		}
		switch {
		case rec == nil || len(rec.Probes) == 0 && rec.Rejection == nil:
			kind = cmp.Or(kind, RoundDiscover)
		case rec.Rejection != nil && !now.Before(rec.Rejection.RecheckAt):
			kind = cmp.Or(kind, RoundRecheck)
			models = moveFirst(models, rec.Rejection.Model)
		case rec.Rejection == nil && classify(rec, false, m.settings.IntervalHours, now) == StateFailing && now.Sub(rec.Probes[len(rec.Probes)-1].At) >= failingRecheck:
			kind = cmp.Or(kind, RoundRecheck)
		default:
			continue
		}
		jobs = append(jobs, job{credential: c, models: models})
	}
	return kind, jobs
}

func moveFirst(models []string, model string) []string {
	if i := slices.Index(models, model); i > 0 {
		return append([]string{model}, append(slices.Clone(models[:i]), models[i+1:]...)...)
	}
	return models
}

// ProbeNow starts a round now: every channel serving a catalog model, or
// only the one at authIndex. It returns ErrBusy while a round runs and ErrOff
// while rounds do not, so none starts after Stop.
func (m *Manager) ProbeNow(authIndex string) error {
	credentials, listErr := m.upstream.Credentials()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started || !m.settings.Enabled {
		return ErrOff
	}
	m.applyCredentialsLocked(credentials, listErr)
	if listErr != nil {
		return fmt.Errorf("list CPA credentials: %w", listErr)
	}
	if m.round != nil {
		return ErrBusy
	}
	candidates := m.candidatesLocked(m.now())
	jobs := []job{}
	for _, c := range m.credentials {
		if c.Disabled || (authIndex != "" && c.AuthIndex != authIndex) {
			continue
		}
		if models := m.servedLocked(c, candidates); len(models) > 0 {
			jobs = append(jobs, job{credential: c, models: models})
		}
	}
	if len(jobs) == 0 {
		return ErrUnknownChannel
	}
	kind := RoundManual
	if authIndex == "" {
		kind = RoundFull
	}
	m.startRoundLocked(kind, jobs)
	return nil
}

func (m *Manager) startRoundLocked(kind string, jobs []job) {
	ctx, cancel := context.WithCancel(context.Background())
	round := &roundState{kind: kind, started: m.now(), total: len(jobs), cancel: cancel, ended: make(chan struct{})}
	m.round = round
	m.log("info", fmt.Sprintf("native search: %s probe round of %d channels started", kind, len(jobs)))
	go m.runRound(ctx, round, jobs)
}

// runRound probes the channels at most probeConcurrency at a time and records
// each as it ends. A full round that was not stopped moves the schedule on.
func (m *Manager) runRound(ctx context.Context, round *roundState, jobs []job) {
	defer close(round.ended)
	defer round.cancel()
	slots := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			m.runJob(ctx, round, j)
		}()
	}
	wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	stopped := ctx.Err() != nil
	if round.kind == RoundFull && !stopped {
		m.lastFull = round.started
		m.dirty = true
	}
	m.round = nil
	m.rebuildLocked()
	m.flushLocked()
	m.log("info", fmt.Sprintf("native search: %s probe round ended (%d of %d channels probed, stopped %t)", round.kind, round.done, round.total, stopped))
}

// runJob probes one channel with the first model CPA routes there. A
// credential whose config lists no models learns which it lacks; one whose
// config lists the model but that CPA has no route for records the mismatch
// as a failure.
func (m *Manager) runJob(ctx context.Context, round *roundState, j job) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.log("error", fmt.Sprintf("native search: probe of %s panicked: %v", j.credential.AuthIndex, recovered))
		}
	}()
	for _, model := range j.models {
		probe, outcome := probeChannel(ctx, m.upstream, j.credential, model, m.now)
		switch outcome {
		case outcomeCancelled:
			return
		case outcomeNotServing:
			if j.credential.Models == nil {
				m.markNotServing(j.credential.AuthIndex, model)
				continue
			}
			probe = Probe{At: m.now().UTC(), Model: model, Result: ResultError, Error: "CPA has no route to this model on this channel, though its config lists it"}
		}
		m.recordProbe(round, j.credential.AuthIndex, probe)
		return
	}
	m.mu.Lock()
	round.done++
	m.mu.Unlock()
}

func (m *Manager) markNotServing(authIndex, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(authIndex)
	if !slices.Contains(rec.NotServing, model) {
		rec.NotServing = append(rec.NotServing, model)
		m.dirty = true
	}
}

// recordProbe keeps a probe in the channel's history. One that searches
// after a refusal clears it; any other moves its recheck on.
func (m *Manager) recordProbe(round *roundState, authIndex string, probe Probe) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(authIndex)
	rec.NotServing = slices.DeleteFunc(rec.NotServing, func(model string) bool { return model == probe.Model })
	if !slices.Contains(rec.Served, probe.Model) {
		rec.Served = append(rec.Served, probe.Model)
	}
	rec.Probes = append(rec.Probes, probe)
	if len(rec.Probes) > historyLimit {
		rec.Probes = slices.Clone(rec.Probes[len(rec.Probes)-historyLimit:])
	}
	rec.Rotation++
	if r := rec.Rejection; r != nil {
		if probe.Result == ResultSearch && probe.At.After(r.At) {
			rec.Rejection = nil
		} else {
			r.RecheckAt = m.now().Add(failingRecheck).UTC()
		}
	}
	round.done++
	m.dirty = true
	m.rebuildLocked()
}

// Running reports whether a round is under way.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.round != nil
}

// log reports through logf and swallows a panic from it.
func (m *Manager) log(level, message string) {
	defer func() { _ = recover() }()
	m.logf(level, message)
}
