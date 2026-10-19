// Package search chooses the model, and so the credential, for Codex web
// search (POST /v1/alpha/search).
//
// CPA serves alpha search only from codex credentials that enable
// alpha-search and register the requested model. Before selecting one, it
// asks model routers for the selection model with SourceFormat
// "codex-alpha-search" and accepts only a provider route to codex. The
// router answers in two steps:
//
//  1. A model outside KeepModels (a Claude, Gemini or Grok session) is
//     replaced by TargetModel, since no codex credential registers it.
//  2. The model is put through the template of the first healthy channel,
//     for example "{model}" for the primary upstream and
//     "alphasearch-nb-{model}" for a fallback whose credential registers only
//     those names. CPA maps that name back to the upstream model before
//     forwarding the search.
//
// CPA sends a search once and never reports its outcome to plugins, so
// channel health comes only from ForceUnhealthy and from optional probes
// sent through CPA's own /v1/alpha/search. Every other request is left
// unhandled.
package search

import (
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

// SourceFormat is the model.route source format of alpha search requests.
const SourceFormat = "codex-alpha-search"

// ProbeHeader marks the router's own probes; requests carrying it are never
// rewritten, so a probe reaches exactly the model it names.
const ProbeHeader = "X-Alpha-Search-Model-Probe"

// settingsFileVersion is the format of the settings file,
// {"version":1,"settings":{...}}.
const settingsFileVersion = 1

var (
	// ErrReadOnly reports an Update refused because canWrite is false: this
	// plugin instance no longer owns the data directory.
	ErrReadOnly = errors.New("search settings are read-only in this plugin instance")
	// ErrInvalidSettings wraps every validation error of Normalize and
	// FromPluginConfig.
	ErrInvalidSettings = errors.New("invalid search settings")
	// ErrNothingToImport reports an alpha-search-model block without any of
	// that plugin's settings, as CPA returns it when the plugin has no
	// configuration of its own; importing it would only bring the defaults.
	ErrNothingToImport = errors.New("the alpha-search-model block has none of its settings")
)

// Channel is one upstream choice, tried in order.
type Channel struct {
	Name string `json:"name"`
	// Model is the selection model template; "{model}" is replaced by the
	// (possibly mapped) requested model.
	Model          string `json:"model"`
	ForceUnhealthy bool   `json:"force_unhealthy"`
}

// Probe configures active health probes. Probing is off while APIKey is
// empty or there is only one channel. APIKey is a CPA access key and is
// never returned by the management API.
type Probe struct {
	APIKey          string `json:"api_key,omitempty"`
	IntervalSeconds int    `json:"interval_seconds"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	// Model is put through each channel template for probes; empty means
	// TargetModel.
	Model     string `json:"model,omitempty"`
	URL       string `json:"url"`
	UserAgent string `json:"user_agent"`
	// IdleSeconds pauses probes after that long without a client search; 0
	// always probes.
	IdleSeconds int `json:"idle_seconds"`
}

// Settings is the router configuration the management UI edits.
type Settings struct {
	TargetModel string    `json:"target_model"`
	KeepModels  []string  `json:"keep_models"`
	Channels    []Channel `json:"channels"`
	Probe       Probe     `json:"probe"`
}

// RouteRequest is the part of the model.route request the router reads.
type RouteRequest struct {
	SourceFormat   string
	RequestedModel string
	Headers        http.Header
}

// Decision mirrors pluginapi.ModelRouteResponse; its Go field names are the
// wire names.
type Decision struct {
	Handled     bool
	TargetKind  string `json:",omitempty"`
	Target      string `json:",omitempty"`
	TargetModel string `json:",omitempty"`
	Reason      string `json:",omitempty"`
}

// ChannelStatus is one channel's live state for the management UI. Healthy
// means Route may pick the channel: it is neither forced nor failing its
// last probe. Routed counts the searches sent through the channel since the
// router was created, including those whose model needed no rewrite.
type ChannelStatus struct {
	Name         string    `json:"name"`
	Model        string    `json:"model"`
	Healthy      bool      `json:"healthy"`
	Forced       bool      `json:"forced"`
	Routed       int64     `json:"routed"`
	LastRoutedAt time.Time `json:"last_routed_at,omitzero"`
	LastProbeAt  time.Time `json:"last_probe_at,omitzero"`
	LastProbeErr string    `json:"last_probe_error,omitempty"`
}

// Status is the router's live state. RoutedTotal also counts searches
// routed through channels removed since.
type Status struct {
	Channels       []ChannelStatus `json:"channels"`
	RoutedTotal    int64           `json:"routed_total"`
	LastSearchAt   time.Time       `json:"last_search_at,omitzero"`
	ProbeRunning   bool            `json:"probe_running"`
	ProbeKeyStored bool            `json:"probe_key_stored"`
}

// Router owns the settings file, the live channel health, the counters and
// the probe loop. Route, Settings and Status are safe for concurrent use and
// never block on I/O or on a lock: they read an immutable snapshot and
// atomics.
type Router struct {
	path      string
	canWrite  func() bool
	logf      func(level, message string)
	createdAt time.Time
	// transport replaces the per-round probe transport; tests only.
	transport http.RoundTripper

	state       atomic.Pointer[routerState]
	loop        atomic.Pointer[probeLoop]
	lastSearch  atomic.Int64 // unix nanoseconds of the latest client search; 0 before the first
	routedTotal atomic.Int64

	// mu serializes Update, Start and Stop, including the file write and the
	// wait for the probe loop to exit. Neither the read paths nor the probe
	// loop take it, so waiting for the loop under mu cannot deadlock.
	mu sync.Mutex
	// started is true while Start was called more recently than Stop, so
	// Update can start or stop probing when the settings turn it on or off.
	started bool
}

// routerState is one immutable settings snapshot with the live state of
// its channels.
type routerState struct {
	settings Settings
	// channels parallels settings.Channels. Update hands a channelState on
	// to the next snapshot when the channel's name and template are
	// unchanged, so health and counters survive the swap.
	channels []*channelState
}

type channelState struct {
	unhealthy  atomic.Bool
	routed     atomic.Int64
	lastRouted atomic.Int64 // unix nanoseconds
	lastProbe  atomic.Pointer[probeResult]
}

type probeResult struct {
	at  time.Time
	err string
}

type settingsFile struct {
	Version  int      `json:"version"`
	Settings Settings `json:"settings"`
}

// NewRouter loads the settings stored at path (a missing file means
// DefaultSettings); a corrupt file, another version or invalid settings are
// an error. canWrite gates every save, and nil means always writable; when
// it allows, NewRouter also removes temporary files an interrupted save left
// behind. logf reports probe state changes ("info", "warn"), and nil
// discards them. Probes do not start until Start.
func NewRouter(path string, canWrite func() bool, logf func(level, message string)) (*Router, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("search settings path is required")
	}
	if canWrite == nil {
		canWrite = func() bool { return true }
	}
	if logf == nil {
		logf = func(string, string) {}
	}
	settings, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	r := &Router{path: path, canWrite: canWrite, logf: logf, createdAt: time.Now()}
	r.state.Store(newState(settings, nil))
	if canWrite() {
		if err := persist.CleanupStaleTemps(path, time.Now()); err != nil {
			r.log("warn", "alpha search: cannot remove stale temporary settings files: "+err.Error())
		}
	}
	return r, nil
}

func loadSettings(path string) (Settings, error) {
	raw, err := persist.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read search settings: %w", err)
	}
	var file settingsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return Settings{}, fmt.Errorf("decode search settings %s: %w", path, err)
	}
	if file.Version != settingsFileVersion {
		return Settings{}, fmt.Errorf("search settings %s: unsupported version %d; require version %d", path, file.Version, settingsFileVersion)
	}
	settings, err := Normalize(file.Settings)
	if err != nil {
		return Settings{}, fmt.Errorf("search settings %s: %w", path, err)
	}
	return settings, nil
}

// newState builds the snapshot for settings, taking over the live state of
// the previous snapshot's channels whose name and template are unchanged.
// Without probing nothing can clear a failed probe, so taken-over channels
// then lose their probe health.
func newState(settings Settings, previous *routerState) *routerState {
	st := &routerState{settings: settings, channels: make([]*channelState, len(settings.Channels))}
	for i, ch := range settings.Channels {
		var live *channelState
		if previous != nil {
			live = previous.channel(ch.Name, ch.Model)
		}
		switch {
		case live == nil:
			live = &channelState{}
		case !settings.probing():
			live.unhealthy.Store(false)
			live.lastProbe.Store(nil)
		}
		st.channels[i] = live
	}
	return st
}

func (st *routerState) channel(name, model string) *channelState {
	for i, ch := range st.settings.Channels {
		if ch.Name == name && ch.Model == model {
			return st.channels[i]
		}
	}
	return nil
}

// channelIndex returns the first channel that is neither forced unhealthy
// nor failing its probe, falling back to the first channel when none is
// usable.
func (st *routerState) channelIndex() int {
	for i, ch := range st.settings.Channels {
		if !ch.ForceUnhealthy && !st.channels[i].unhealthy.Load() {
			return i
		}
	}
	return 0
}

// Route answers one model.route call. It handles alpha search requests only,
// and only with a provider route to codex: CPA turns any other handled
// answer into an error for the client.
func (r *Router) Route(req RouteRequest) Decision {
	if req.SourceFormat != SourceFormat || isProbe(req.Headers) {
		return Decision{}
	}
	now := time.Now().UnixNano()
	r.lastSearch.Store(now)
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		return Decision{}
	}
	st := r.state.Load()
	mapped := model
	if !keepModel(st.settings.KeepModels, model) {
		mapped = st.settings.TargetModel
	}
	i := st.channelIndex()
	ch, live := st.settings.Channels[i], st.channels[i]
	live.routed.Add(1)
	live.lastRouted.Store(now)
	r.routedTotal.Add(1)
	selected := strings.ReplaceAll(ch.Model, modelPlaceholder, mapped)
	if strings.EqualFold(selected, model) {
		return Decision{}
	}
	return Decision{
		Handled:     true,
		TargetKind:  "provider",
		Target:      "codex",
		TargetModel: selected,
		Reason:      "alpha search model " + model + " routed to " + selected + " via channel " + ch.Name,
	}
}

func isProbe(headers http.Header) bool {
	for name, values := range headers {
		if strings.EqualFold(name, ProbeHeader) && len(values) > 0 {
			return true
		}
	}
	return false
}

// Settings returns the current settings with Probe.APIKey cleared.
func (r *Router) Settings() Settings {
	return publicSettings(r.state.Load().settings)
}

func publicSettings(s Settings) Settings {
	s.KeepModels = slices.Clone(s.KeepModels)
	s.Channels = slices.Clone(s.Channels)
	s.Probe.APIKey = ""
	return s
}

// Update normalizes and persists new settings and returns them with
// Probe.APIKey cleared. A non-empty Probe.APIKey replaces the stored key;
// an empty one keeps it unless clearProbeKey, which removes it. Health and
// counters of channels whose name and template are unchanged are kept,
// except that probe health is dropped when the new settings turn probing
// off, and a running probe loop is restarted with the new settings; Update also
// starts or stops probing after Start when the change turns it on or off.
// Settings equal to the current ones are neither written nor re-probed.
//
// A read-only instance returns ErrReadOnly, invalid settings an error
// wrapping ErrInvalidSettings; after any error, including a failed write,
// the router keeps its previous settings.
func (r *Router) Update(s Settings, clearProbeKey bool) (Settings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.canWrite() {
		return Settings{}, ErrReadOnly
	}
	next, err := Normalize(s)
	if err != nil {
		return Settings{}, err
	}
	current := r.state.Load()
	if next.Probe.APIKey == "" && !clearProbeKey {
		next.Probe.APIKey = current.settings.Probe.APIKey
	}
	if reflect.DeepEqual(next, current.settings) {
		return publicSettings(current.settings), nil
	}
	if err := r.save(next); err != nil {
		return Settings{}, err
	}
	r.stopLoop()
	r.state.Store(newState(next, current))
	if r.started {
		r.startLoop()
	}
	return publicSettings(next), nil
}

func (r *Router) save(settings Settings) error {
	raw, err := json.MarshalIndent(settingsFile{Version: settingsFileVersion, Settings: settings}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode search settings: %w", err)
	}
	if err := persist.AtomicWrite(r.path, raw); err != nil {
		return fmt.Errorf("save search settings: %w", err)
	}
	return nil
}

// Status reports the live state.
func (r *Router) Status() Status {
	st := r.state.Load()
	out := Status{
		Channels:       make([]ChannelStatus, len(st.settings.Channels)),
		RoutedTotal:    r.routedTotal.Load(),
		LastSearchAt:   unixTime(r.lastSearch.Load()),
		ProbeRunning:   r.loop.Load().running(),
		ProbeKeyStored: st.settings.Probe.APIKey != "",
	}
	for i, ch := range st.settings.Channels {
		live := st.channels[i]
		status := ChannelStatus{
			Name:         ch.Name,
			Model:        ch.Model,
			Healthy:      !ch.ForceUnhealthy && !live.unhealthy.Load(),
			Forced:       ch.ForceUnhealthy,
			Routed:       live.routed.Load(),
			LastRoutedAt: unixTime(live.lastRouted.Load()),
		}
		if probe := live.lastProbe.Load(); probe != nil {
			status.LastProbeAt, status.LastProbeErr = probe.at, probe.err
		}
		out.Channels[i] = status
	}
	return out
}

func unixTime(nanos int64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// Start runs the probe loop when probing is configured; it is a no-op when
// already running. The plugin calls it on register and reconfigure.
func (r *Router) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = true
	r.startLoop()
}

// Stop stops the probe loop and waits for it to exit; it is a no-op when no
// loop runs. The plugin calls it on quiesce and shutdown. Routing keeps
// working with the last known health.
func (r *Router) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = false
	r.stopLoop()
}

// log reports through logf and swallows a panic from it: no log line is
// worth ending the CPA process.
func (r *Router) log(level, message string) {
	defer func() { _ = recover() }()
	r.logf(level, message)
}
