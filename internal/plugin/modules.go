package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-policy/internal/adapt"
	"cpa-key-policy/internal/channel"
	"cpa-key-policy/internal/quality"
	"cpa-key-policy/internal/search"
	"cpa-key-policy/internal/search/native"
)

// Module files live next to the key store's state file. They are separate
// from the v7 dataset, so an older release that knows nothing about them
// still reads its own files after a rollback.
const (
	adaptersFile     = "cpa-key-policy-adapters.json"
	searchFile       = "cpa-key-policy-search.json"
	nativeFile       = "cpa-key-policy-search-native.json"
	channelsFile     = "cpa-key-policy-channels.json"
	channelStatsFile = "cpa-key-policy-channel-stats.json"
	qualityFile      = "cpa-key-policy-quality.json"
	inspectionFile   = "cpa-key-policy-inspection.json"

	// Channel statistics are also written exactly at close and quiesce, so a
	// crash loses at most this much of them, never billing.
	statsFlushInterval = 5 * time.Minute
	// The inspection schedule is checked this often, so a round starts at
	// most this late.
	inspectionTickInterval = time.Minute
	cpaConfigCacheTTL      = time.Minute
)

// qualityHandOffLimit bounds how long quiesce waits for detection tests CPA
// is still opening a stream for; it holds every management write meanwhile.
// A variable only so that tests can shorten it.
var qualityHandOffLimit = 15 * time.Second

// moduleSet is what one data directory holds besides the key store: the
// adapter rules, the alpha search router, native search, the channel
// registry and the detection tests. It is opened once per directory and
// swapped atomically, so the request path reads it without locks. A module
// whose file cannot be opened stays nil and its error is shown on the
// settings page; the rest of the plugin works.
type moduleSet struct {
	dir      string
	adapters *adapt.Manager
	search   *search.Router
	native   *native.Manager
	channels *channel.Registry
	quality  *quality.Service
	errors   map[string]string
	// detached is set when a register replaced the set without waiting for
	// its detection tests: what they still record is never saved.
	detached atomic.Bool
}

// openModules opens the modules of dir unless they already are.
func (a *App) openModules(dir string) *moduleSet {
	if current := a.modules.Load(); current != nil && current.dir == dir {
		return current
	}
	set := &moduleSet{dir: dir, errors: map[string]string{}}
	var err error
	if set.adapters, err = adapt.NewManager(filepath.Join(dir, adaptersFile), a.canWrite, time.Now); err != nil {
		set.adapters = nil
		set.errors["adapters"] = err.Error()
	}
	if set.search, err = search.NewRouter(filepath.Join(dir, searchFile), a.canWrite, a.logf); err != nil {
		set.search = nil
		set.errors["search"] = err.Error()
	}
	if set.channels, err = channel.NewRegistry(channel.Options{
		MetaPath:  filepath.Join(dir, channelsFile),
		StatsPath: filepath.Join(dir, channelStatsFile),
		Location:  a.store.Location(),
		CanWrite:  a.canWrite,
		Now:       time.Now,
		Logf:      a.logf,
	}); err != nil {
		set.channels = nil
		set.errors["channels"] = err.Error()
	}
	// A history file it cannot read leaves detection working without saving;
	// the detection page shows why.
	set.quality = quality.Open(quality.Options{
		Path:              filepath.Join(dir, qualityFile),
		Legacy:            filepath.Join(dir, qualityLegacyFile),
		InspectionPath:    filepath.Join(dir, inspectionFile),
		Location:          a.store.Location(),
		Host:              qualityHost{a},
		ConfigCredentials: a.cpaCredentials.qualityCredentials,
		CanWrite:          func() bool { return !set.detached.Load() && a.canWrite() },
		Logf:              a.logf,
	})
	if set.native, err = native.Open(native.Options{
		Path:     filepath.Join(dir, nativeFile),
		CanWrite: func() bool { return !set.detached.Load() && a.canWrite() },
		Logf:     a.logf,
		Upstream: nativeUpstream{app: a, quality: set.quality},
	}); err != nil {
		set.native = nil
		set.errors["native_search"] = err.Error()
	}
	for name, message := range set.errors {
		a.logf("warn", fmt.Sprintf("cpa-key-policy: %s module unavailable: %s", name, message))
	}
	if previous := a.modules.Swap(set); previous != nil {
		previous.close()
	}
	return set
}

// channelHandoff names channel observations in the handoff journal.
const channelHandoff = "channel"

// handOff prepares the modules for a hot replacement: the search probes and
// the detection tests stop, and the channel statistics are written and from
// the same instant passed to journal as well. A detection or native search
// request CPA is still opening cannot be cut short, so both are cancelled
// first and together get qualityHandOffLimit to end; one that ends later
// closes its own stream and saves nothing.
func (set *moduleSet) handOff(journal func(channel.Observation), logf func(level, message string)) error {
	if set.search != nil {
		set.search.Stop()
	}
	deadline := time.Now().Add(qualityHandOffLimit)
	if set.native != nil {
		set.native.HandOff(0)
	}
	if set.quality != nil {
		if left := set.quality.HandOff(qualityHandOffLimit); left > 0 {
			logf("warn", fmt.Sprintf("cpa-key-policy: %d detection tests still wait on CPA for a first response; handing off without them", left))
		}
	}
	if set.native != nil {
		if left := set.native.HandOff(max(time.Until(deadline), 0)); left > 0 {
			logf("warn", "cpa-key-policy: a native search probe still waits on CPA for a first response; handing off without it")
		}
	}
	if set.channels != nil {
		return set.channels.HandOff(journal)
	}
	return nil
}

// journalObservation hands one channel observation to the newer instance.
func (a *App) journalObservation(o channel.Observation) {
	if payload, err := json.Marshal(o); err == nil {
		a.store.JournalModule(channelHandoff, payload)
	}
}

// replayHandoff applies the module entries of a journal another instance
// left.
func (a *App) replayHandoff(module string, payload json.RawMessage) {
	set := a.modules.Load()
	if module != channelHandoff || set == nil || set.channels == nil {
		return
	}
	var o channel.Observation
	if err := json.Unmarshal(payload, &o); err == nil {
		set.channels.Observe(o)
	}
}

// close stops what a replaced module set still runs and saves its
// statistics. The detection tests and the native search round end before it
// returns. At shutdown either can wait for the upstream's next chunk, so the
// statistics are written before them too, and once more for the attempts of
// those just cancelled.
func (set *moduleSet) close() {
	if set.search != nil {
		set.search.Stop()
	}
	if set.channels != nil {
		_ = set.channels.FlushStats()
	}
	set.stopProbes()
	if set.channels != nil {
		_ = set.channels.FlushStats()
	}
}

// stopProbes cancels the native search round and the detection tests, then
// waits for both: CPA may free the host callbacks right after a shutdown.
func (set *moduleSet) stopProbes() {
	if set.native != nil {
		set.native.HandOff(0)
	}
	if set.quality != nil {
		set.quality.Stop()
	}
	if set.native != nil {
		set.native.Stop()
	}
}

// detach stops a module set a register replaces after this instance gave the
// data directory up, writing nothing. Unlike close it does not wait for the
// detection tests: one CPA is still opening a stream for cannot be cut short,
// and CPA applies no configuration until the register returns. The host
// callbacks stay open, since this instance serves on, so such a test closes
// its own stream once CPA answers; what it records is never saved, as the
// reopened modules own the files.
func (set *moduleSet) detach(logf func(level, message string)) {
	if set.search != nil {
		set.search.Stop()
	}
	if set.native != nil {
		set.detached.Store(true)
		set.native.HandOff(0)
	}
	if set.quality != nil {
		set.detached.Store(true)
		if left := set.quality.HandOff(0); left > 0 {
			logf("warn", fmt.Sprintf("cpa-key-policy: %d detection tests of the replaced modules are still ending; their records are not saved", left))
		}
	}
}

// canWrite gates every module file write: only the instance holding the data
// directory writes, and after quiesce a newer instance owns the files.
func (a *App) canWrite() bool {
	return !a.quiesced.Load() && a.ownsData.Load()
}

// background runs the periodic module work: statistics flushes and the
// inspection schedule. One loop exists per App; whoever starts it stops it.
type background struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *App) startBackground() {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.loop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &background{cancel: cancel, done: make(chan struct{})}
	a.loop = loop
	go func() {
		defer close(loop.done)
		a.runBackground(ctx)
	}()
}

func (a *App) stopBackground() {
	a.backgroundMu.Lock()
	loop := a.loop
	a.loop = nil
	a.backgroundMu.Unlock()
	if loop != nil {
		loop.cancel()
		<-loop.done
	}
}

func (a *App) runBackground(ctx context.Context) {
	stats := time.NewTicker(statsFlushInterval)
	defer stats.Stop()
	inspection := time.NewTicker(inspectionTickInterval)
	defer inspection.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-inspection.C:
			a.safely("inspection schedule", func() {
				if set := a.modules.Load(); set != nil && set.quality != nil {
					set.quality.Tick()
				}
			})
			a.safely("native search schedule", func() {
				if set := a.modules.Load(); set != nil && set.native != nil {
					set.native.Tick()
				}
			})
		case <-stats.C:
			a.safely("channel statistics flush", func() {
				if set := a.modules.Load(); set != nil && set.channels != nil {
					if err := set.channels.FlushStats(); err != nil {
						a.logf("warn", "cpa-key-policy: channel statistics flush failed: "+err.Error())
					}
				}
			})
		}
	}
}

// safely runs one piece of background work. An unrecovered panic in a
// plugin goroutine would end the whole CPA process.
func (a *App) safely(what string, work func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			a.logf("error", fmt.Sprintf("cpa-key-policy: %s panicked: %v", what, recovered))
		}
	}()
	work()
}

// syncSearch starts or stops the alpha search probes and the native search
// rounds to match the settings. A native search round is cancelled, not
// waited for, as in detach: its probe may wait on CPA to open a stream, CPA
// applies no configuration until the register returns, and this instance
// serves on.
func (a *App) syncSearch(set *moduleSet, settings pluginSettings) {
	if set == nil {
		return
	}
	on := settings.SearchEnabled && !a.quiesced.Load()
	if set.search != nil {
		if on {
			set.search.Start()
		} else {
			set.search.Stop()
		}
	}
	if set.native != nil {
		if on {
			set.native.Start()
		} else {
			set.native.HandOff(0)
		}
	}
}

// logf writes one line to CPA's log through host.log, so it lands in
// main.log next to the host's own lines; outside a host, or when the host
// refuses the line, it goes to stderr. CPA refuses a callback with an error
// envelope and a zero status (host_callbacks_unix.go), as it does for every
// callback once it has closed the instance, which it does before shutdown.
func (a *App) logf(level, message string) {
	if a.host == nil {
		log.Print(message)
		return
	}
	logged := false
	if request, err := json.Marshal(map[string]string{"level": level, "message": message}); err == nil {
		if raw, err := a.host.Call("host.log", request); err == nil {
			var answer struct {
				OK bool `json:"ok"`
			}
			logged = json.Unmarshal(raw, &answer) == nil && answer.OK
		}
	}
	if !logged {
		log.Print(message)
	}
}

// cpaConfigCache reads the API keys of CPA's config.yaml for detection tests,
// adapter rule tries and group names, at most once a minute. The upstream keys it holds stay
// in this process.
type cpaConfigCache struct {
	mu       sync.Mutex
	path     func() string
	logf     func(level, message string)
	readAt   time.Time
	readPath string
	byIndex  map[string]channel.Credential
	err      error
}

func (c *cpaConfigCache) lookup(authIndex string) (channel.Credential, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	credential, ok := c.byIndex[authIndex]
	return credential, ok
}

// groups names each config API key's group in CPA's config.yaml by
// auth_index, and gives its credential ID: CPA's own key lists flatten the
// groups and drop their names, and look a credential's models up by ID.
func (c *cpaConfigCache) groups() (groups, ids map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	groups, ids = make(map[string]string, len(c.byIndex)), make(map[string]string, len(c.byIndex))
	for index, credential := range c.byIndex {
		if credential.Group != "" {
			groups[index] = credential.Group
		}
		ids[index] = credential.ID
	}
	return groups, ids
}

// qualityCredentials lists the config's API keys for the detection tests,
// each shown as a masked preview.
func (c *cpaConfigCache) qualityCredentials() ([]quality.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	out := make([]quality.Credential, 0, len(c.byIndex))
	for _, credential := range c.byIndex {
		out = append(out, quality.Credential{
			ID: credential.ID, AuthIndex: credential.AuthIndex, Provider: credential.Provider, Source: quality.SourceProvider,
			Name: quality.PreviewKey(credential.APIKey), Group: credential.Group, BaseURL: credential.BaseURL,
			// A key CPA routes nothing to (excluded-models "*") is not tested
			// by "all" or an inspection.
			Disabled: credential.Disabled,
		})
	}
	return out, c.err
}

// models gives the models CPA's config lists for each API key by auth_index;
// a key listing none is absent.
func (c *cpaConfigCache) models() map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	out := make(map[string][]string, len(c.byIndex))
	for index, credential := range c.byIndex {
		if credential.Models != nil {
			out[index] = slices.Clone(credential.Models)
		}
	}
	return out
}

// nativeUpstream is CPA as native search probes it: the detection module's
// credentials, with the models CPA's config lists for each API key, and
// model streams pinned to one of them.
type nativeUpstream struct {
	app     *App
	quality *quality.Service
}

// Credentials fails when CPA's config cannot be read: without its API keys
// the list is incomplete, and native search would drop their evidence.
func (u nativeUpstream) Credentials() ([]native.Credential, error) {
	list, configErr, err := u.quality.Credentials()
	if err != nil {
		return nil, err
	}
	if configErr != "" {
		return nil, fmt.Errorf("read CPA config: %s", configErr)
	}
	models := u.app.cpaCredentials.models()
	out := make([]native.Credential, 0, len(list))
	for _, c := range list {
		credential := native.Credential{AuthIndex: c.AuthIndex, ID: c.ID, Provider: c.Provider, Name: c.Name, Group: c.Group,
			Disabled: c.Disabled, Unavailable: c.Unavailable}
		if c.Source == quality.SourceProvider {
			credential.Models = models[c.AuthIndex]
		}
		out = append(out, credential)
	}
	return out, nil
}

func (u nativeUpstream) Stream(ctx context.Context, c native.Credential, model string, body any, headers http.Header) ([]byte, int, error) {
	events, status, err := quality.StreamEvents(ctx, qualityHost{u.app}, quality.Credential{ID: c.ID, AuthIndex: c.AuthIndex, Provider: c.Provider}, model, body, headers)
	if err != nil && quality.NotServing(err) {
		return nil, status, fmt.Errorf("%w: %v", native.ErrNotServing, err)
	}
	return events, status, err
}

func (c *cpaConfigCache) refreshLocked() {
	path := c.path()
	if !c.readAt.IsZero() && path == c.readPath && time.Since(c.readAt) <= cpaConfigCacheTTL {
		return
	}
	byIndex, err := channel.ReadCPAConfig(path)
	if err != nil && (c.err == nil || c.err.Error() != err.Error()) && c.logf != nil {
		c.logf("warn", "cpa-key-policy: cannot read CPA config for balance queries and detection: "+err.Error())
	}
	c.byIndex, c.err = byIndex, err
	c.readAt, c.readPath = time.Now(), path
}
