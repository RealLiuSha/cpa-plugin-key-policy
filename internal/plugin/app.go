package plugin

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-policy/internal/adapt"
	"cpa-key-policy/internal/channel"
	"cpa-key-policy/internal/plugin/web"
	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
	"cpa-key-policy/internal/pricingmetadata"
	"cpa-key-policy/internal/search"
	"cpa-key-policy/internal/search/native"
)

type App struct {
	store   *policy.Store
	pricing *pricingmetadata.Client
	// host calls back into CPA (host.log); nil outside CPA, as in tests.
	host HostCaller

	// lifecycleMu serializes register, reconfigure and quiesce.
	lifecycleMu sync.Mutex
	// writeMu orders management writes against a change of ownership. A
	// write holds it shared and checks quiesced under it; quiesce and a move
	// to another data directory hold it exclusively, so a write lands while
	// this instance owns the files or is refused. Lock order: lifecycleMu,
	// writeMu, channelPatchMu, then the channel registry's and the store's
	// locks.
	writeMu sync.RWMutex
	// channelPatchMu serializes PATCH /channels, the only channel metadata
	// write: its audit line compares the metadata read before the update with
	// what the update stored, and no other patch may land in between.
	channelPatchMu sync.Mutex
	settings       atomic.Pointer[pluginSettings]
	// warnings belong to the configuration in effect; rejected says why the
	// latest one was not applied and is cleared once one is.
	warnings atomic.Pointer[[]string]
	rejected atomic.Pointer[string]
	// declaredRouter and declaredNative are what the last registration told
	// CPA about model_router and response_interceptor; the settings can ask
	// for a change that is not yet live.
	declaredRouter atomic.Bool
	declaredNative atomic.Bool
	// hostSchema is the lifecycle version the host offered.
	hostSchema atomic.Uint32
	// quiesced is set between plugin.quiesce and the next configuration: a
	// newer instance owns the data files, so nothing here writes them.
	quiesced atomic.Bool
	// readOnly is set while quiesced when a rollback could not take the data
	// directory back: writes are refused until a later config apply can.
	readOnly atomic.Bool
	// owner is the data directory's lock. Two instances of this plugin live
	// in one CPA process during a hot replacement; only the holder writes.
	owner     *persist.Lock
	ownerPath string
	// ownsData mirrors owner != nil for module goroutines.
	ownsData atomic.Bool

	modules        atomic.Pointer[moduleSet]
	cpaCredentials cpaConfigCache
	backgroundMu   sync.Mutex
	loop           *background
	timings        hookTimings
	ui             web.Server

	// usageDecodeLogged is when a usage record that did not decode was last
	// logged (Unix nanoseconds).
	usageDecodeLogged atomic.Int64
}

// HostCaller performs one host callback with a JSON request and returns the
// host's JSON envelope.
type HostCaller interface {
	Call(method string, request []byte) ([]byte, error)
}

func NewApp() *App {
	app := &App{store: policy.NewStore(), pricing: pricingmetadata.NewClient()}
	app.cpaCredentials.path = func() string { return app.pluginSettings().CPAConfigPath }
	app.cpaCredentials.logf = app.logf
	app.store.SetLogf(app.logf)
	return app
}

// SetHost connects the App to CPA's host callbacks.
func (a *App) SetHost(host HostCaller) {
	a.host = host
}

// pluginSettings returns the plugin's own settings from the last valid
// configuration.
func (a *App) pluginSettings() pluginSettings {
	if settings := a.settings.Load(); settings != nil {
		return *settings
	}
	return pluginSettings{CPAConfigPath: defaultCPAConfigPath}
}

func (a *App) SetPricingClient(client *pricingmetadata.Client) {
	if a == nil || client == nil {
		return
	}
	a.pricing = client
}

// HandleMethod answers one host call. request may point into memory the
// host owns and frees when the call returns: nothing may keep it, or any
// slice of it, past the return.
func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	started := time.Now()
	defer func() { a.timings.record(method, time.Since(started)) }()
	return safePluginCall(method, func() ([]byte, error) {
		return a.handleMethod(method, request)
	})
}

func (a *App) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		if err := a.configure(request, method == MethodPluginRegister); err != nil {
			return nil, err
		}
		return OKEnvelope(a.registration())
	case MethodPluginQuiesce:
		// CPA only logs a failed quiesce; the data directory this instance
		// kept is what stops the replacement.
		if err := a.quiesce(); err != nil {
			return ErrorEnvelope("handoff_failed", err.Error(), http.StatusInternalServerError), nil
		}
		return OKEnvelope(struct{}{})
	case MethodFrontendAuthIdentifier:
		return OKEnvelope(IdentifierResponse{Identifier: PluginID})
	case MethodFrontendAuthAuthenticate:
		return a.authenticate(request)
	case MethodRequestInterceptBefore:
		return a.interceptRequest(request)
	case MethodRequestInterceptAfter:
		return a.adaptAttempt(request)
	case MethodModelRoute:
		return a.routeModel(request)
	case MethodResponseInterceptAfter:
		return a.interceptResponse(request)
	case MethodUsageHandle:
		return a.handleUsage(request)
	case MethodManagementRegister:
		return OKEnvelope(a.managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

// safePluginCall answers a panic in call with an error that names the method
// and the value. CPA turns a failed authentication into a 401 without a log
// line and logs a failed usage.handle only at debug level, so the panic is
// logged here as well.
func safePluginCall(method string, call func() ([]byte, error)) (response []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = fmt.Errorf("plugin panic in %s: %v", method, recovered)
			panicLogs.report(method, recovered)
		}
	}()
	return call()
}

// panicLogs writes the stack of a panic at most once a minute per method,
// and one line otherwise: a panic on the request path repeats on every
// request. It logs to stderr, not through a host callback.
var panicLogs = panicLog{last: map[string]time.Time{}}

type panicLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (p *panicLog) report(method string, recovered any) {
	now := time.Now()
	p.mu.Lock()
	withStack := now.Sub(p.last[method]) >= time.Minute
	if withStack {
		p.last[method] = now
	}
	p.mu.Unlock()
	if withStack {
		log.Printf("cpa-key-policy: %s panicked: %v\n%s", method, recovered, debug.Stack())
		return
	}
	log.Printf("cpa-key-policy: %s panicked: %v", method, recovered)
}

// configure applies a register or reconfigure. CPA sends reconfigure on
// every config apply and drops the plugin from its active set when it fails,
// taking auth, billing and routing down until the next apply. So once a
// dataset is loaded, a configuration that cannot be applied keeps the last
// valid one and is reported on the settings page instead; only a first
// registration that cannot load its data fails, closed.
//
// A register can also reach an App that loaded its dataset before: CPA maps
// a library whose bytes already ran in the process to the image still
// resident (a rollback by store.version, or a re-enable after another build
// ran). When this App gave the data directory up meanwhile, another instance
// may have written the files, so it reloads them and replays the journal
// like a first registration, and fails, closed, when they do not load.
func (a *App) configure(raw []byte, register bool) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	var req LifecycleRequest
	if len(raw) > 0 {
		// Lenient like the YAML block: a newer host may add fields along with
		// the higher schema_version accepted below, and refusing them would
		// drop every capability, leaving CPA without this access provider.
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	if req.SchemaVersion < MinHostSchemaVersion {
		return fmt.Errorf("unsupported lifecycle schema_version %d; require >= %d", req.SchemaVersion, MinHostSchemaVersion)
	}
	a.hostSchema.Store(req.SchemaVersion)
	cfg, settings, warnings, err := parseConfigDocument(req.ConfigYAML, false)
	var claimed *persist.Lock
	var claimedPath string
	if err == nil {
		claimed, claimedPath, err = a.claimDataDirectory(cfg.StateFile)
	}
	if claimed != nil {
		// The dataset and the modules move to another directory: no
		// management write lands in either one meanwhile.
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
	}
	firstLoad := !a.store.Loaded()
	stale := register && !firstLoad && a.owner == nil
	if err == nil {
		if stale {
			err = a.store.Reload(cfg, a.quiesced.Load())
		} else {
			err = a.store.Configure(cfg)
		}
	}
	if err == nil && stale {
		// The modules reopen from the files too. The old ones write nothing:
		// this instance does not own the directory yet.
		if previous := a.modules.Swap(nil); previous != nil {
			previous.detach(a.logf)
		}
		firstLoad = true
	}
	if err != nil {
		// The directory that failed to load is free again; the one this
		// instance serves from stays locked.
		_ = claimed.Release()
		if !a.store.Loaded() || stale {
			return err
		}
		rejected := "configuration not applied, the last valid one stays in effect: " + err.Error()
		if a.owner == nil && errors.Is(err, persist.ErrLocked) {
			rejected = "cannot take the data directory back after a rolled-back replacement; management writes are refused until the lock is free and the next config apply: " + err.Error()
		}
		a.rejected.Store(&rejected)
		a.logf("warn", "cpa-key-policy: "+rejected)
		a.resume(a.pluginSettings())
		return nil
	}
	// The directory just loaded is this instance's from here on. The one it
	// leaves stays locked until its modules are closed, which writes their
	// statistics there.
	left := a.adoptDataLock(claimed, claimedPath)
	dir := filepath.Dir(a.store.StatePath())
	if firstLoad {
		// The modules open before the journal replays, so the channel
		// observations it carries find their registry.
		a.openModules(dir)
		if replayed, err := a.store.AdoptHandoff(a.replayHandoff); err != nil {
			warnings = append(warnings, "handoff journal not replayed: "+err.Error())
		} else if replayed > 0 {
			a.logf("info", fmt.Sprintf("cpa-key-policy: took over %d journaled entries from the previous instance", replayed))
		}
	}
	a.settings.Store(&settings)
	a.warnings.Store(&warnings)
	a.rejected.Store(nil)
	a.ui.SetDataDir(dir)
	a.openModules(dir)
	_ = left.Release()
	a.resume(settings)
	return nil
}

// claimDataDirectory locks the data directory the configuration points at
// and returns that lock and its path, or a nil lock when this instance holds
// it already. Ownership changes only when the caller adopts the lock, once
// the dataset there has loaded. A newer instance gets the directory only
// after the older one released it at quiesce; without it a first
// registration fails, closed, and CPA keeps the older instance.
func (a *App) claimDataDirectory(stateFile string) (*persist.Lock, string, error) {
	statePath, err := policy.ResolveStatePath(stateFile)
	if err != nil {
		return nil, "", err
	}
	path := persist.LockPath(statePath)
	if a.owner != nil && a.ownerPath == path {
		return nil, "", nil
	}
	lock, err := persist.TryLock(path)
	if err != nil {
		return nil, "", err
	}
	return lock, path, nil
}

// adoptDataLock makes a claimed lock this instance's ownership and returns
// the lock it held before, for the caller to release. A nil lock changes
// nothing.
func (a *App) adoptDataLock(lock *persist.Lock, path string) *persist.Lock {
	if lock == nil {
		return nil
	}
	previous := a.owner
	a.owner, a.ownerPath = lock, path
	a.ownsData.Store(true)
	return previous
}

// resume restarts what quiesce stopped; it is also the normal start. A
// reconfigure after quiesce means CPA rolled the replacement back and this
// instance serves again.
func (a *App) resume(settings pluginSettings) {
	if a.owner == nil {
		// Rolled back after quiesce released the lock: take it again.
		lock, path, err := a.claimDataDirectory(a.store.StatePath())
		if err != nil {
			a.readOnly.Store(true)
			a.logf("error", "cpa-key-policy: cannot take the data directory back, staying read-only: "+err.Error())
			return
		}
		a.adoptDataLock(lock, path)
	}
	a.readOnly.Store(false)
	a.quiesced.Store(false)
	a.store.Resume()
	if set := a.modules.Load(); set != nil {
		if set.channels != nil {
			set.channels.Resume()
		}
		// A shutdown stopped the detection tests when CPA discarded a load
		// that reached this same App (a hot replacement by the same bytes)
		// before rolling back to it.
		if set.quality != nil {
			set.quality.Resume()
		}
	}
	a.startBackground()
	a.syncSearch(a.modules.Load(), settings)
}

// quiesce prepares for a hot replacement: CPA loads a newer instance next,
// which reads the same files. Everything is written once more and nothing
// here writes again until a configuration arrives (a rollback); what still
// arrives goes to the handoff journal.
//
// When the accounting cut, the journal or the channel statistics cannot be
// written, the newer instance would miss what this one billed or counted.
// This instance then keeps the data directory and serves on unchanged: the
// newer one cannot register, and CPA 911f49c and later roll the replacement
// back to this one. CPA v8.0.13 unloads the failed library instead and
// hangs, which is why hot replacement is not used there.
func (a *App) quiesce() error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	// Management writes under way finish first and later ones are refused,
	// so none lands after the newer instance read the files.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.owner == nil {
		// Handed off already, or read-only since a rollback could not take
		// the directory back: the files are not this instance's to write.
		return nil
	}
	a.stopBackground()
	// The journal opens first, so the channel statistics hand over from the
	// instant they are written; both writes need the files still owned.
	err := a.store.Quiesce()
	if set := a.modules.Load(); err == nil && set != nil {
		if err = set.handOff(a.journalObservation, a.logf); err != nil {
			err = fmt.Errorf("hand off channel statistics: %w", err)
		}
	}
	if err != nil {
		a.logf("error", "cpa-key-policy: cannot hand off for a hot replacement, keeping the data directory so CPA keeps this instance: "+err.Error())
		a.resume(a.pluginSettings())
		return err
	}
	a.quiesced.Store(true)
	if a.owner != nil {
		_ = a.owner.Release()
		a.owner = nil
		a.ownsData.Store(false)
	}
	a.logf("info", "cpa-key-policy: quiesced for a hot replacement")
	return nil
}

// Shutdown writes the final accounting cut. The host calls it on plugin
// unload and at process exit. A quiesced instance wrote its cut already and
// no longer owns the files.
//
// CPA refuses every callback of the plugin, stream_close included, before it
// calls shutdown, so a detection test or native search probe still reading a
// model stream ends only when the upstream sends again, and CPA gives the
// whole shutdown 30 seconds. The order follows from that: the accounting cut
// first, which also applies the tail of a journal being followed; then the
// modules, whose channel statistics then include that tail and are written
// before the tests and probes are waited for; the data directory lock last,
// so every write still owns the files. They are always waited for: CPA
// v8.0.13 unloads the library right after shutdown returns.
func (a *App) Shutdown() {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.stopBackground()
	set := a.modules.Load()
	if a.quiesced.Load() || a.owner == nil {
		// The files are not this instance's: nothing is written.
		a.store.Stop()
		if set != nil {
			set.stopProbes()
		}
		return
	}
	if err := a.store.Shutdown(); err != nil {
		// Not through host.log: CPA has closed the callback instance by now
		// and refuses the line.
		log.Printf("cpa-key-policy: final usage flush failed: %v", err)
	}
	if set != nil {
		set.close()
	}
	_ = a.owner.Release()
	a.owner = nil
	a.ownsData.Store(false)
}

func (a *App) registration() Registration {
	set := a.modules.Load()
	searchOn := a.pluginSettings().SearchEnabled && set != nil
	router, interceptor := searchOn && set.search != nil, searchOn && set.native != nil
	a.declaredRouter.Store(router)
	a.declaredNative.Store(interceptor)
	return Registration{
		SchemaVersion: min(SchemaVersion, max(a.hostSchema.Load(), MinHostSchemaVersion)),
		Metadata: Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "cpa-key-policy",
			GitHubRepository: "https://github.com/RealLiuSha/cpa-plugin-key-policy",
			ConfigFields: []ConfigField{
				{Name: "enabled", Type: "boolean", Description: "Enable or disable this plugin without unloading it."},
				{Name: "state_file", Type: "string", Description: "JSON state file used for key policy changes made through the Management API."},
				{Name: "usage_timezone", Type: "string", Description: "IANA timezone used for natural-day usage buckets. Defaults to Asia/Shanghai."},
				{Name: "keys", Type: "array", Description: "First-boot downstream key seeds. State is authoritative after initialization."},
				{Name: "models", Type: "array", Description: "First-boot price rows: upstream model id (name), optional provider qualifier, billing mode and prices."},
				{Name: "search_enabled", Type: "boolean", Description: "Codex web search: native search through each channel's web_search tool, with alpha search as the fallback (registers a model router and a response interceptor; settings are on the plugin's search page)."},
				{Name: "cpa_config_path", Type: "string", Description: "CPA config.yaml, read only for the upstream keys of detection tests and adapter rule tries and for group names. Defaults to config.yaml."},
			},
		},
		Capabilities: Capabilities{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: false,
			ModelRouter:                   router,
			ResponseInterceptor:           interceptor,
			RequestInterceptor:            true,
			UsagePlugin:                   true,
			ManagementAPI:                 true,
		},
	}
}

func (a *App) authenticate(raw []byte) ([]byte, error) {
	var req FrontendAuthRequest
	if _, err := decodeSkippingBody(raw, &req); err != nil {
		return nil, err
	}
	decision := a.store.Authenticate(req.Method, req.Path, req.Headers, req.Query)
	if !decision.Known || !decision.Allowed {
		return OKEnvelope(FrontendAuthResponse{Authenticated: false})
	}
	return OKEnvelope(FrontendAuthResponse{
		Authenticated: true,
		Principal:     decision.Principal,
		Metadata:      map[string]string{"provider": PluginID, "key_id": decision.KeyID},
	})
}

// interceptRequest admits a request whose RPM and quota checks authentication
// deferred, terminating it with 429 when the key is over a limit.
func (a *App) interceptRequest(raw []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if _, err := decodeSkippingBody(raw, &req); err != nil {
		return nil, err
	}
	denial := a.store.AdmitIntercepted(req.Headers, string(req.Metadata.RequestPath), req.TraceID)
	if denial == nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	return OKEnvelope(rejectionResponse(req.SourceFormat, denial, time.Now()))
}

// adaptAttempt prepares one upstream attempt for the selected credential:
// native search takes the web_search tool out on a channel the operator
// disabled or that refuses it, then the channel's adapter rules run. The body
// is decoded only when either applies; any problem leaves the attempt as it
// is.
func (a *App) adaptAttempt(raw []byte) ([]byte, error) {
	set := a.modules.Load()
	if set == nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	var req AttemptInterceptRequest
	value, err := decodeSkippingBody(raw, &req)
	if err != nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	authIndex, requestPath := string(req.Metadata.SelectedAuthIndex), string(req.Metadata.RequestPath)
	strip := set.native != nil && set.native.Strips(authIndex, req.RequestedModel)
	var book *adapt.Book
	if set.adapters != nil {
		book = set.adapters.Book()
	}
	rules := book != nil && book.Match(authIndex, requestPath)
	if !strip && !rules {
		return OKEnvelope(RequestInterceptResponse{})
	}
	body, err := decodeBodyBytes(value)
	if err != nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	changed := false
	if strip {
		if stripped, ok := native.StripWebSearch(body); ok {
			body, changed = stripped, true
		}
	}
	if rules {
		if rewritten, applied, err := book.Apply(authIndex, requestPath, body); err == nil && rewritten != nil {
			set.adapters.RecordHits(applied)
			body, changed = rewritten, true
		}
	}
	if !changed {
		return OKEnvelope(RequestInterceptResponse{})
	}
	return OKEnvelope(RequestInterceptResponse{Body: body})
}

// interceptResponse rewrites the Codex model catalog for native search. The
// host sends every successful non-streaming response and model list here; the
// catalog is a model list, the only kind without a model, so the members are
// read in the host's order (SourceFormat, Model and RequestedModel come before
// the bodies) and the call ends at the first one that rules it out.
func (a *App) interceptResponse(raw []byte) ([]byte, error) {
	set := a.modules.Load()
	if set == nil || set.native == nil {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	catalog := true
	var value []byte
	err := scanMembers(raw, func(key, member []byte) bool {
		switch string(key) {
		case "SourceFormat":
			catalog = string(member) == `"openai"`
		case "Model", "RequestedModel":
			catalog = string(member) == `""` || string(member) == "null"
		case "StatusCode":
			catalog = string(member) == "200"
		case "Body":
			value = member
		}
		return catalog
	})
	if err != nil || !catalog || value == nil {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	body, err := decodeBodyBytes(value)
	if err != nil || body == nil {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	return OKEnvelope(ResponseInterceptResponse{Body: set.native.RewriteCatalog(body)})
}

// routeModel answers model.route: alpha search requests only.
func (a *App) routeModel(raw []byte) ([]byte, error) {
	set := a.modules.Load()
	if set == nil || set.search == nil || !a.pluginSettings().SearchEnabled {
		return OKEnvelope(search.Decision{})
	}
	var req ModelRouteRequest
	if _, err := decodeSkippingBody(raw, &req); err != nil {
		return OKEnvelope(search.Decision{})
	}
	return OKEnvelope(set.search.Route(search.RouteRequest{SourceFormat: req.SourceFormat, RequestedModel: req.RequestedModel, Headers: req.Headers}))
}

func (a *App) handleUsage(raw []byte) ([]byte, error) {
	var req UsageHandleRequest
	// A record that cannot be read must never break the request path: it
	// bills nothing. A field whose type changed is left zero and the record
	// still counts, unless billing reads that field.
	if err := json.Unmarshal(raw, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		billable := errors.As(err, &typeErr)
		if billable {
			// encoding/json reports only the first field whose type changed
			// and leaves later ones zero, so the fields billing reads are
			// decoded once more on their own: the fields of this struct
			// shadow the embedded ones billing does not read.
			var billing struct {
				UsageHandleRequest
				BaseURL, AuthID, AuthIndex, RequestedAt, Latency, TTFT, Stream, Failure json.RawMessage
			}
			if billingErr := json.Unmarshal(raw, &billing); billingErr != nil {
				billable, err = false, billingErr
			}
		}
		a.reportUsageDecode(err, billable)
		if !billable {
			return OKEnvelope(UsageHandleResponse{})
		}
	}
	requestedAt, _ := time.Parse(time.RFC3339Nano, req.RequestedAt)
	event := policy.UsageEvent{
		TraceID:        req.TraceID,
		Provider:       req.Provider,
		ExecutorType:   req.ExecutorType,
		Model:          req.Model,
		RequestedModel: req.Alias,
		APIKey:         req.APIKey,
		Failed:         req.Failed,
		Detail: policy.UsageDetail{
			InputTokens:         req.Detail.InputTokens,
			OutputTokens:        req.Detail.OutputTokens,
			ReasoningTokens:     req.Detail.ReasoningTokens,
			CachedTokens:        req.Detail.CachedTokens,
			CacheReadTokens:     req.Detail.CacheReadTokens,
			CacheCreationTokens: req.Detail.CacheCreationTokens,
			TotalTokens:         req.Detail.TotalTokens,
		},
	}
	// Channel statistics see every attempt, also those billing ignores:
	// they describe the upstream, not the downstream key.
	if set := a.modules.Load(); set != nil && set.channels != nil {
		listCost, _ := a.store.ListCost(event)
		prompt, cached, generated, reasoning := policy.ShapeTokens(req.Provider, req.ExecutorType, event.Detail)
		set.channels.Observe(channel.Observation{
			AuthIndex: req.AuthIndex, AuthID: req.AuthID, Provider: req.Provider, BaseURL: req.BaseURL, Model: req.Model,
			RequestedModel: event.RequestedModel, Failed: req.Failed, StatusCode: req.Failure.StatusCode,
			Latency: time.Duration(req.Latency), TTFT: time.Duration(req.TTFT), Stream: req.Stream, At: time.Now(), RequestedAt: requestedAt, TraceID: req.TraceID,
			InputTokens: req.Detail.InputTokens, OutputTokens: req.Detail.OutputTokens,
			CacheReadTokens: req.Detail.CacheReadTokens, CacheWriteTokens: req.Detail.CacheCreationTokens,
			Tokens: channel.Tokens{Prompt: prompt, Cached: cached, Generated: generated, Reasoning: reasoning},
			// Host-callback requests (detection, inspection) carry no key.
			Probe:        strings.TrimSpace(req.APIKey) == "",
			ListCostNano: int64(listCost), ErrorSnippet: req.Failure.Body,
		})
	}
	if set := a.modules.Load(); req.Failed && set != nil && set.native != nil {
		set.native.ObserveFailure(req.AuthIndex, cmp.Or(req.Alias, req.Model), req.Failure.StatusCode, req.Failure.Body)
	}
	a.store.RecordUsage(event)
	return OKEnvelope(UsageHandleResponse{})
}

// reportUsageDecode logs a usage record that did not decode cleanly, at most
// once a minute: CPA logs a failed usage.handle only at debug level, and a
// wire type that changed fails every record. The error names the field and
// the types, never the record.
func (a *App) reportUsageDecode(err error, billed bool) {
	now := time.Now().UnixNano()
	last := a.usageDecodeLogged.Load()
	if now-last < int64(time.Minute) || !a.usageDecodeLogged.CompareAndSwap(last, now) {
		return
	}
	outcome := "not billed"
	if billed {
		outcome = "billed without that field"
	}
	a.logf("warn", fmt.Sprintf("cpa-key-policy: usage record does not decode (%s): %v", outcome, err))
}

func (a *App) managementRegistration() ManagementRegistrationResponse {
	base := "/plugins/" + PluginID
	return ManagementRegistrationResponse{
		Routes: append([]ManagementRoute{
			{Method: http.MethodGet, Path: base + "/keys", Description: "List downstream CPA key policies."},
			{Method: http.MethodPost, Path: base + "/keys", Description: "Create a downstream CPA key policy."},
			{Method: http.MethodPatch, Path: base + "/keys", Description: "Update a downstream CPA key policy by id."},
			{Method: http.MethodDelete, Path: base + "/keys", Description: "Delete a downstream CPA key policy by id."},
			{Method: http.MethodPost, Path: base + "/keys/rotate", Description: "Rotate one downstream CPA key by id."},
			{Method: http.MethodPost, Path: base + "/keys/reset-rpm", Description: "Reset one downstream CPA key RPM counter by id."},
			{Method: http.MethodPost, Path: base + "/keys/reset-usage", Description: "Reset one downstream CPA key daily, weekly, or monthly usage window by id."},
			{Method: http.MethodPost, Path: base + "/keys/balance", Description: "Add to or deduct from a balance-mode key's prepaid balance (audited, request_id makes retries idempotent)."},
			{Method: http.MethodGet, Path: base + "/keys/usage", Description: "Per-model usage breakdown for one downstream CPA key by id."},
			{Method: http.MethodGet, Path: base + "/keys/history", Description: "Natural-day usage history for one downstream CPA key (days 1..90)."},
			{Method: http.MethodGet, Path: base + "/audit", Description: "Read append-only management audit events."},
			{Method: http.MethodGet, Path: base + "/status", Description: "Show cpa-key-policy runtime status and ledger health."},
			{Method: http.MethodGet, Path: base + "/stats", Description: "Usage totals across all keys for the last days (1..90)."},
			{Method: http.MethodGet, Path: base + "/stats/export", Description: "Export per-day, per-key, per-model usage as CSV for a date range."},
			{Method: http.MethodGet, Path: base + "/models", Description: "List price rows and the models seen without a price."},
			{Method: http.MethodPost, Path: base + "/models", Description: "Create or update a price row by upstream model id and optional provider."},
			{Method: http.MethodDelete, Path: base + "/models", Description: "Delete a price row by upstream model id and optional provider."},
			{Method: http.MethodPost, Path: base + "/models/import-prices", Description: "Batch-import token prices for existing price rows (dry_run supported)."},
			{Method: http.MethodPost, Path: base + "/models/pricing-preview", Description: "Preview Models.dev prices for selected CPA models without writing state."},
			{Method: http.MethodPost, Path: base + "/models/import", Description: "Create $0 price rows for upstream models; existing rows are skipped."},
			{Method: http.MethodGet, Path: base + "/overview", Description: "Today's numbers and what needs attention across keys and channels."},
			{Method: http.MethodGet, Path: base + "/channels", Description: "Per-channel statistics and metadata by CPA auth_index."},
			{Method: http.MethodGet, Path: base + "/channels/detail", Description: "One channel's 7-day history and recent errors."},
			{Method: http.MethodPatch, Path: base + "/channels", Description: "Update one channel's name, vendor or note."},
			{Method: http.MethodDelete, Path: base + "/channels", Description: "Forget a channel CPA's config no longer has: its statistics, metadata and adapter rules."},
			{Method: http.MethodGet, Path: base + "/adapters", Description: "List per-channel request rewrite rules."},
			{Method: http.MethodPost, Path: base + "/adapters", Description: "Create or update a request rewrite rule."},
			{Method: http.MethodDelete, Path: base + "/adapters", Description: "Delete a request rewrite rule by id."},
			{Method: http.MethodPost, Path: base + "/adapters/preview", Description: "Show what a rule does to a sample body."},
			{Method: http.MethodPost, Path: base + "/adapters/try", Description: "Send one real request with a rule applied, pinned to the rule's channel."},
			{Method: http.MethodGet, Path: base + "/search", Description: "Alpha search routing settings and live channel state."},
			{Method: http.MethodPut, Path: base + "/search", Description: "Replace the alpha search routing settings."},
			{Method: http.MethodPost, Path: base + "/search/import", Description: "Import settings from an alpha-search-model plugin block."},
			{Method: http.MethodPut, Path: base + "/search/native", Description: "Replace the native search settings."},
			{Method: http.MethodPost, Path: base + "/search/native/probe", Description: "Probe every channel serving a Codex catalog model for native search now, or one by auth_index."},
			{Method: http.MethodGet, Path: base + "/settings", Description: "Module state, hook timings, data files and warnings."},
		}, qualityRoutes(base)...),
		Resources: []ResourceRoute{
			// TRADEOFF: host menu is static Chinese only, revisit when CPA supports locale maps
			{Path: web.IndexPath, Menu: PluginName, Description: "萝卜: keys, channels, prices, statistics, alpha search, detection and settings."},
		},
	}
}

func (a *App) handleManagement(raw []byte) ([]byte, error) {
	var req ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Plugin resource GETs (unauthenticated browser UI) are dispatched through
	// the same management.handle method by CPA's ServeResourceHTTP.
	resourcePrefix := "/v0/resource/plugins/" + PluginID
	if req.Method == http.MethodGet && strings.HasPrefix(path, resourcePrefix) {
		status, headers, body := a.ui.Serve(strings.TrimPrefix(path, resourcePrefix), req.Headers)
		return OKEnvelope(ManagementResponse{StatusCode: status, Headers: headers, Body: body})
	}

	// After quiesce a newer instance owns every data file; writes wait for
	// it. A write holds writeMu shared and checks quiesced under it, so it
	// lands before quiesce writes the accounting cut or is refused.
	base := "/v0/management/plugins/" + PluginID
	if req.Method != http.MethodGet {
		if !networkBoundCalls[strings.TrimPrefix(path, base)] {
			a.writeMu.RLock()
			defer a.writeMu.RUnlock()
		}
		if a.quiesced.Load() {
			return OKEnvelope(a.writeRefused())
		}
	}

	switch {
	case req.Method == http.MethodGet && path == base+"/keys":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"keys": a.publicKeys(a.store.Keys())}))
	case req.Method == http.MethodPost && path == base+"/keys":
		return OKEnvelope(a.createKey(req.Body))
	case req.Method == http.MethodPatch && path == base+"/keys":
		return OKEnvelope(a.patchKey(req.Body))
	case req.Method == http.MethodDelete && path == base+"/keys":
		return OKEnvelope(a.deleteKey(strings.TrimSpace(req.Query.Get("id"))))
	case req.Method == http.MethodPost && path == base+"/keys/rotate":
		id, err := idFromBody(req.Body)
		if err != nil {
			return OKEnvelope(jsonError(http.StatusBadRequest, "invalid_json", err.Error()))
		}
		return OKEnvelope(a.rotateKey(id))
	case req.Method == http.MethodPost && path == base+"/keys/reset-rpm":
		id, err := idFromBody(req.Body)
		if err != nil {
			return OKEnvelope(jsonError(http.StatusBadRequest, "invalid_json", err.Error()))
		}
		return OKEnvelope(a.resetRPM(id))
	case req.Method == http.MethodPost && path == base+"/keys/reset-usage":
		return OKEnvelope(a.resetUsage(req.Body))
	case req.Method == http.MethodPost && path == base+"/keys/balance":
		return OKEnvelope(a.adjustBalance(req.Body))
	case req.Method == http.MethodGet && path == base+"/keys/usage":
		return OKEnvelope(a.keyUsage(strings.TrimSpace(req.Query.Get("id"))))
	case req.Method == http.MethodGet && path == base+"/keys/history":
		return OKEnvelope(a.keyHistory(strings.TrimSpace(req.Query.Get("id")), intQuery(req.Query, "days", 30)))
	case req.Method == http.MethodGet && path == base+"/audit":
		return OKEnvelope(a.auditEvents(strings.TrimSpace(req.Query.Get("key_id")), strings.TrimSpace(req.Query.Get("action")), intQuery(req.Query, "limit", 100)))
	case req.Method == http.MethodGet && path == base+"/status":
		return OKEnvelope(jsonResponse(http.StatusOK, a.store.Status()))
	case req.Method == http.MethodGet && path == base+"/stats":
		return OKEnvelope(a.stats(intQuery(req.Query, "days", 30)))
	case req.Method == http.MethodGet && path == base+"/stats/export":
		return OKEnvelope(a.statsExport(strings.TrimSpace(req.Query.Get("from")), strings.TrimSpace(req.Query.Get("to"))))
	case req.Method == http.MethodGet && path == base+"/models":
		return OKEnvelope(a.listModels())
	case req.Method == http.MethodPost && path == base+"/models":
		return OKEnvelope(a.upsertModel(req.Body))
	case req.Method == http.MethodDelete && path == base+"/models":
		return OKEnvelope(a.deleteModel(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/import-prices":
		return OKEnvelope(a.importModelPrices(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/pricing-preview":
		return OKEnvelope(a.previewModelPrices(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/import":
		return OKEnvelope(a.importModels(req.Body))
	case req.Method == http.MethodGet && path == base+"/overview":
		return OKEnvelope(a.overview())
	case req.Method == http.MethodGet && path == base+"/channels":
		return OKEnvelope(a.listChannels())
	case req.Method == http.MethodGet && path == base+"/channels/detail":
		return OKEnvelope(a.channelDetail(req.Query.Get("auth_index")))
	case req.Method == http.MethodPatch && path == base+"/channels":
		return OKEnvelope(a.patchChannel(req.Body))
	case req.Method == http.MethodDelete && path == base+"/channels":
		return OKEnvelope(a.deleteChannel(req.Query.Get("auth_index")))
	case req.Method == http.MethodGet && path == base+"/adapters":
		return OKEnvelope(a.listAdapters())
	case req.Method == http.MethodPost && path == base+"/adapters":
		return OKEnvelope(a.upsertAdapter(req.Body))
	case req.Method == http.MethodDelete && path == base+"/adapters":
		return OKEnvelope(a.deleteAdapter(req.Query.Get("id")))
	case req.Method == http.MethodPost && path == base+"/adapters/preview":
		return OKEnvelope(a.previewAdapter(req.Body))
	case req.Method == http.MethodPost && path == base+"/adapters/try":
		return OKEnvelope(a.tryAdapter(req.Body, req.HostCallbackID))
	case req.Method == http.MethodGet && path == base+"/search":
		return OKEnvelope(a.searchState())
	case req.Method == http.MethodPut && path == base+"/search":
		return OKEnvelope(a.updateSearch(req.Body))
	case req.Method == http.MethodPost && path == base+"/search/import":
		return OKEnvelope(a.importSearch(req.Body))
	case req.Method == http.MethodPut && path == base+"/search/native":
		return OKEnvelope(a.updateNativeSearch(req.Body))
	case req.Method == http.MethodPost && path == base+"/search/native/probe":
		return OKEnvelope(a.probeNativeSearch(req.Body))
	case req.Method == http.MethodGet && path == base+"/settings":
		return OKEnvelope(a.settingsPage())
	case path == base+"/quality" || strings.HasPrefix(path, base+"/quality/"):
		return OKEnvelope(a.handleQuality(req, strings.TrimPrefix(path, base+"/quality")))
	default:
		return OKEnvelope(jsonError(http.StatusNotFound, "not_found", "unknown management route"))
	}
}

// networkBoundCalls wait on the network and write no data file: an adapter
// test request through host.model.execute (its audit line is written under
// writeMu before the call) and a models.dev price preview. They do not hold
// writeMu, which would hold quiesce up for as long as they wait.
var networkBoundCalls = map[string]bool{
	"/adapters/try":           true,
	"/models/pricing-preview": true,
}

// upgrading answers a write that reached a quiesced instance.
func upgrading() ManagementResponse {
	return jsonError(http.StatusServiceUnavailable, "upgrading", "a newer plugin instance is taking over; retry in a moment")
}

// writeRefused answers a write while quiesced. After a rollback that could
// not take the data directory back, a retry does not help until the lock is
// free and CPA applies its config again; the code stays upgrading, which
// every caller already treats as "not now".
func (a *App) writeRefused() ManagementResponse {
	if a.readOnly.Load() {
		return jsonError(http.StatusServiceUnavailable, "upgrading", "this plugin instance is read-only: it could not take the data directory back after a rolled-back replacement; see the settings page")
	}
	return upgrading()
}

// keyWriteRequest is the POST/PATCH /keys body. Unknown fields are rejected,
// except the two settings earlier releases had: loboo-mono and
// reeka-ai-install still send models and allow_models_endpoint, so both are
// accepted and ignored.
type keyWriteRequest struct {
	ID                  string          `json:"id"`
	Name                *string         `json:"name,omitempty"`
	Enabled             *bool           `json:"enabled,omitempty"`
	Key                 string          `json:"key,omitempty"`
	RPM                 *int            `json:"rpm,omitempty"`
	DailyLimitUSD       *float64        `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      *float64        `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD     *float64        `json:"monthly_limit_usd,omitempty"`
	QuotaMode           *string         `json:"quota_mode,omitempty"`
	Models              json.RawMessage `json:"models,omitempty"`
	AllowModelsEndpoint json.RawMessage `json:"allow_models_endpoint,omitempty"`
}

type publicKey struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Enabled         bool                `json:"enabled"`
	KeyPreview      string              `json:"key_preview"`
	RPM             int                 `json:"rpm"`
	DailyLimitUSD   float64             `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64             `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64             `json:"monthly_limit_usd"`
	QuotaMode       string              `json:"quota_mode"`
	Usage           policy.UsageSummary `json:"usage"`
	CreatedAt       string              `json:"created_at,omitempty"`
	UpdatedAt       string              `json:"updated_at,omitempty"`
}

func (a *App) createKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	plain := strings.TrimSpace(req.Key)
	generated := false
	var err error
	if plain == "" {
		plain, err = policy.GenerateKey()
		if err != nil {
			return jsonError(http.StatusInternalServerError, "key_generation_failed", err.Error())
		}
		generated = true
	}
	hash, err := policy.HashKey(plain)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rpm := 0
	if req.RPM != nil {
		rpm = *req.RPM
	}
	name := req.ID
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		name = strings.TrimSpace(*req.Name)
	}
	item := policy.KeyConfig{
		ID:              req.ID,
		Name:            name,
		Enabled:         enabled,
		KeyHash:         hash,
		KeyPreview:      policy.PreviewKey(plain),
		RPM:             rpm,
		DailyLimitUSD:   applyFloat64(req.DailyLimitUSD, 0),
		WeeklyLimitUSD:  applyFloat64(req.WeeklyLimitUSD, 0),
		MonthlyLimitUSD: applyFloat64(req.MonthlyLimitUSD, 0),
	}
	if req.QuotaMode != nil {
		item.QuotaMode = *req.QuotaMode
	}
	if err := a.store.UpsertKey(item); err != nil {
		return keyWriteError(err)
	}
	if stored := a.keyByID(item.ID); stored != nil {
		item = *stored
	}
	return jsonResponse(http.StatusCreated, map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": generated,
	})
}

func (a *App) patchKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	current := a.keyByID(id)
	if current == nil {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	if req.Name != nil {
		current.Name = strings.TrimSpace(*req.Name)
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.RPM != nil {
		current.RPM = *req.RPM
	}
	if req.DailyLimitUSD != nil {
		current.DailyLimitUSD = *req.DailyLimitUSD
	}
	if req.WeeklyLimitUSD != nil {
		current.WeeklyLimitUSD = *req.WeeklyLimitUSD
	}
	if req.MonthlyLimitUSD != nil {
		current.MonthlyLimitUSD = *req.MonthlyLimitUSD
	}
	if req.QuotaMode != nil {
		switching := !strings.EqualFold(strings.TrimSpace(*req.QuotaMode), current.Mode())
		current.QuotaMode = *req.QuotaMode
		// A balance key has no period limits: switching to it clears those the
		// request does not set, so a caller only has to name the mode.
		if switching && current.BalanceMode() {
			current.DailyLimitUSD = applyFloat64(req.DailyLimitUSD, 0)
			current.WeeklyLimitUSD = applyFloat64(req.WeeklyLimitUSD, 0)
			current.MonthlyLimitUSD = applyFloat64(req.MonthlyLimitUSD, 0)
		}
	}
	if strings.TrimSpace(req.Key) != "" {
		hash, err := policy.HashKey(req.Key)
		if err != nil {
			return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
		}
		current.KeyHash = hash
		current.KeyPreview = policy.PreviewKey(req.Key)
	}
	if err := a.store.UpsertKey(*current); err != nil {
		return keyWriteError(err)
	}
	if stored := a.keyByID(current.ID); stored != nil {
		current = stored
	}
	return jsonResponse(http.StatusOK, map[string]any{"key": a.publicKeyFromConfig(*current)})
}

func (a *App) keyByID(id string) *policy.KeyConfig {
	for _, key := range a.store.Keys() {
		if key.ID == id {
			copy := key
			return &copy
		}
	}
	return nil
}

func (a *App) deleteKey(id string) ManagementResponse {
	if err := a.store.DeleteKey(id); err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{"deleted": true, "id": strings.TrimSpace(id)})
}

func (a *App) rotateKey(id string) ManagementResponse {
	plain, item, err := a.store.RotateKey(id)
	if err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": true,
	})
}

func (a *App) resetRPM(id string) ManagementResponse {
	if err := a.store.ResetRPM(id); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"reset": true, "id": strings.TrimSpace(id)})
}

func (a *App) resetUsage(body []byte) ManagementResponse {
	var req struct {
		ID       string                        `json:"id"`
		Window   policy.UsageResetWindow       `json:"window"`
		Expected *policy.UsageResetExpectation `json:"expected,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	result, err := a.store.ResetUsageWindow(req.ID, req.Window, req.Expected)
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrUnknownKey):
			return jsonError(http.StatusNotFound, "not_found", "key not found")
		case errors.Is(err, policy.ErrInvalidUsageResetWindow):
			return jsonError(http.StatusBadRequest, "invalid_window", "window must be daily, weekly, or monthly")
		case errors.Is(err, policy.ErrInvalidUsageResetExpectation):
			return jsonError(http.StatusBadRequest, "invalid_expectation", err.Error())
		case errors.Is(err, policy.ErrUsageResetChanged):
			return jsonError(http.StatusConflict, "quota_changed", "Quota period changed; refresh and confirm the new reset date")
		default:
			return jsonError(http.StatusInternalServerError, "reset_failed", err.Error())
		}
	}
	log.Printf("cpa-key-policy: usage reset key_id=%q window=%s before_daily_usd=%.6f before_weekly_usd=%.6f before_monthly_usd=%.6f", result.KeyID, result.Window, result.BeforeDailyUSD, result.BeforeWeeklyUSD, result.BeforeMonthlyUSD)
	return jsonResponse(http.StatusOK, result)
}

func (a *App) adjustBalance(body []byte) ManagementResponse {
	var req struct {
		ID        string   `json:"id"`
		AmountUSD *float64 `json:"amount_usd"`
		Note      string   `json:"note,omitempty"`
		RequestID string   `json:"request_id,omitempty"`
	}
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	if strings.TrimSpace(req.ID) == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	if req.AmountUSD == nil {
		return jsonError(http.StatusBadRequest, "invalid_amount", "amount_usd is required")
	}
	result, err := a.store.AdjustBalance(req.ID, *req.AmountUSD, req.Note, req.RequestID)
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrUnknownKey):
			return jsonError(http.StatusNotFound, "not_found", "key not found")
		case errors.Is(err, policy.ErrNotBalanceMode):
			return jsonError(http.StatusConflict, "not_balance_mode", "Switch the key to balance mode before adjusting its balance")
		case errors.Is(err, policy.ErrBalanceRequestConflict):
			return jsonError(http.StatusConflict, "request_id_conflict", err.Error())
		case errors.Is(err, policy.ErrInvalidBalanceAdjustment):
			return jsonError(http.StatusBadRequest, "invalid_amount", err.Error())
		default:
			return jsonError(http.StatusInternalServerError, "persist_failed", err.Error())
		}
	}
	return jsonResponse(http.StatusOK, result)
}

// keyUsage returns the per-model usage breakdown for one downstream key.
func (a *App) keyUsage(id string) ManagementResponse {
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	key, models, ok := a.store.ModelUsageFor(id)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key_id":            key.ID,
		"key_name":          key.Name,
		"daily_limit_usd":   key.DailyLimitUSD,
		"weekly_limit_usd":  key.WeeklyLimitUSD,
		"monthly_limit_usd": key.MonthlyLimitUSD,
		"models":            models,
		"usage":             a.store.UsageSummaryFor(key),
	})
}

func (a *App) keyHistory(id string, days int) ManagementResponse {
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	if days < 1 || days > policy.UsageRetentionDays {
		return jsonError(http.StatusBadRequest, "invalid_days", fmt.Sprintf("days must be between 1 and %d", policy.UsageRetentionDays))
	}
	key, history, ok := a.store.UsageHistoryFor(id, days)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key_id":   key.ID,
		"timezone": a.store.Timezone(),
		"days":     history,
	})
}

func (a *App) auditEvents(keyID, action string, limit int) ManagementResponse {
	if limit < 1 || limit > 1000 {
		return jsonError(http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000")
	}
	events, err := a.store.AuditEvents(keyID, action, limit)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "audit_read_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"events": events})
}

func intQuery(query map[string][]string, name string, fallback int) int {
	values := query[name]
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return -1
	}
	return value
}

// keyWriteError maps a rejected key write: a secret already in use is a
// conflict, a failed state write a server error, anything else a validation
// failure.
func keyWriteError(err error) ManagementResponse {
	switch {
	case errors.Is(err, policy.ErrDuplicateKeyHash):
		return jsonError(http.StatusConflict, "duplicate_key", err.Error())
	case errors.Is(err, policy.ErrStatePersist):
		return persistError(err)
	default:
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
}

func storeError(err error) ManagementResponse {
	switch {
	case errors.Is(err, policy.ErrUnknownKey):
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	case errors.Is(err, policy.ErrDuplicateKeyHash):
		return jsonError(http.StatusConflict, "duplicate_key", err.Error())
	case errors.Is(err, policy.ErrStatePersist):
		return persistError(err)
	default:
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
}

// persistError answers a management write whose state file could not be
// written. It is logged here because the host only sees the response.
func persistError(err error) ManagementResponse {
	log.Printf("cpa-key-policy: state write failed: %v", err)
	return jsonError(http.StatusInternalServerError, "persist_failed", err.Error())
}

func idFromBody(body []byte) (string, error) {
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeStrictBody(body, &payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.ID), nil
}

func (a *App) publicKeys(keys []policy.KeyConfig) []publicKey {
	out := make([]publicKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, a.publicKeyFromConfig(key))
	}
	return out
}

func (a *App) publicKeyFromConfig(key policy.KeyConfig) publicKey {
	out := publicKey{
		ID:              key.ID,
		Name:            key.Name,
		Enabled:         key.Enabled,
		KeyPreview:      key.KeyPreview,
		RPM:             key.RPM,
		DailyLimitUSD:   key.DailyLimitUSD,
		WeeklyLimitUSD:  key.WeeklyLimitUSD,
		MonthlyLimitUSD: key.MonthlyLimitUSD,
		QuotaMode:       key.Mode(),
		Usage:           a.store.UsageSummaryFor(key),
	}
	if !key.CreatedAt.IsZero() {
		out.CreatedAt = key.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if !key.UpdatedAt.IsZero() {
		out.UpdatedAt = key.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

func applyFloat64(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func jsonResponse(status int, payload any) ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "json_error", err.Error())
	}
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func jsonError(status int, code, message string) ManagementResponse {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    strings.TrimSpace(code),
			"message": strings.TrimSpace(message),
		},
	})
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func decodeStrictBody(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func (a *App) Store() *policy.Store {
	if a == nil {
		return nil
	}
	return a.store
}

func DebugEnvelope(raw []byte) string {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Sprintf("invalid envelope: %v", err)
	}
	if env.Error != nil {
		return env.Error.Message
	}
	return string(env.Result)
}
