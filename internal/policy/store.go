package policy

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-policy/internal/policy/audit"
	policyPersist "cpa-key-policy/internal/policy/persist"
)

type Store struct {
	mu         sync.RWMutex
	updateMu   sync.Mutex
	persistMu  sync.Mutex
	enabled    bool
	statePath  string
	keys       map[string]*KeyConfig
	keysByHash map[string]*KeyConfig
	limiter    *RateLimiter
	usage      *usageLedger
	auditLog   *audit.Log
	// flusher periodically persists the ledger to the independent usage file.
	flusher *usageFlusher
	// aliases is the global alias mapping table from config.yaml. Used to
	// resolve KeyAliasRef → ModelRule for routing and billing.
	aliases map[string]*AliasMapping
	// classifyRules are user-defined credential classification rules.
	classifyRules []ClassifyRule
	// rrCounters tracks round-robin position per alias name (global, shared
	// across all keys). Reset on Configure/UpsertKey when aliases change.
	rrCounters map[string]int
	// pendingPicks remembers the multi-target selection made at Authenticate
	// so Route (and the group stamped into scheduler metadata) use the same
	// target for the same request. Without this, round-robin would advance
	// twice (auth + route) and the scheduler could filter by the wrong group.
	// Keyed by lower(keyID)+"\0"+lower(alias); FIFO queue per key.
	pendingPicks map[string][]pendingPick
	// precharges pairs access-time image/video charges with later usage.handle
	// notifications when a host starts reporting those endpoints.
	precharges map[string][]time.Time
	// onClassifyRulesChanged is called when classify rules change, so the
	// plugin can clear its classify cache. Set by the plugin App.
	onClassifyRulesChanged func()
}

// ErrInvalidUsageResetWindow reports an unsupported administrative usage
// reset window.
var ErrInvalidUsageResetWindow = errors.New("invalid usage reset window")

// pendingPick is one Authenticate-time target selection waiting for Route.
type pendingPick struct {
	rule ModelRule
	at   time.Time
}

// pendingPickTTL drops orphaned selections when the host never called Route
// (e.g. rejected after auth). Long enough for normal request setup, short
// enough not to pin stale groups across unrelated traffic.
const pendingPickTTL = 30 * time.Second

// pendingPickMaxQueue caps how many unconsumed picks we keep per (key,alias).
const pendingPickMaxQueue = 32

type AuthDecision struct {
	Known       bool
	Allowed     bool
	KeyID       string
	Principal   string
	Requested   string
	Rule        ModelRule
	Reason      string
	ModelList   bool
	RateLimited bool
	CostLimited bool
	// PreCharged reports that this request was billed at access time because
	// it targets an image/video endpoint whose per_call alias CPA cannot bill
	// via usage.handle (the XAI executor skips UsageReporter on those paths).
	// The charge is unconditional (no failure refund), so this is a deliberate
	// trade-off documented in the UI.
	PreCharged bool
}

func NewStore() *Store {
	return &Store{
		enabled:      DefaultConfig().Enabled,
		keys:         make(map[string]*KeyConfig),
		keysByHash:   make(map[string]*KeyConfig),
		limiter:      NewRateLimiter(),
		usage:        newUsageLedger(time.Now),
		rrCounters:   make(map[string]int),
		pendingPicks: make(map[string][]pendingPick),
		precharges:   make(map[string][]time.Time),
	}
}

// SetClock injects a clock for testing (limiter + usage windows).
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	s.limiter = NewRateLimiterWithClock(now)
	s.usage = newUsageLedger(now)
	s.mu.Unlock()
}

func (s *Store) Configure(cfg Config) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	statePath, err := ResolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}

	// Bug 2 fix: flush any in-memory changes to the *old* state path BEFORE
	// loading the (possibly different) new state file. Without this, keys/usage
	// changed via the management API in the last <=15s window (or any abnormal
	// path that skipped persist) would be lost when LoadState reads a stale disk
	// snapshot. StopUsageFlusher stops the background loop and flushes once.
	s.StopUsageFlusher()

	keys := cfg.Keys
	var loadedUsage map[string]*UsageState
	firstBoot := false
	stateNeedsRewrite := false
	clockNow := time.Now
	s.mu.RLock()
	if s.usage != nil {
		clockNow = s.usage.now
	}
	s.mu.RUnlock()
	usagePath := policyPersist.UsagePath(statePath)
	if err := policyPersist.CleanupStaleTemps(usagePath, clockNow()); err != nil {
		return fmt.Errorf("cleanup usage temp files: %w", err)
	}
	if state, errLoad := LoadStateAt(statePath, clockNow(), cfg.usageLocation); errLoad == nil {
		keys = state.Keys
		loadedUsage = state.Usage
		stateNeedsRewrite = state.Version < usageFileVersion || state.usageMigrated
		if persistedUsage, errUsage := LoadUsage(usagePath); errUsage == nil {
			loadedUsage = persistedUsage
		} else if !errors.Is(errUsage, os.ErrNotExist) {
			return fmt.Errorf("load usage: %w", errUsage)
		}
		// If config.yaml has no global alias table, fall back to the one
		// persisted in state (so state-only reloads resolve key alias refs).
		stateAliases := cfg.Aliases
		if len(stateAliases) == 0 && len(state.Aliases) > 0 {
			stateAliases = state.Aliases
		}
		stateRules := cfg.ClassifyRules
		if len(stateRules) == 0 && len(state.ClassifyRules) > 0 {
			stateRules = state.ClassifyRules
		}
		// Validate state keys against the global alias table. normalizeConfig
		// also auto-migrates any state keys still using per-key Models.
		merged := Config{Enabled: cfg.Enabled, StateFile: cfg.StateFile, UsageTimezone: cfg.UsageTimezone, Keys: keys, Aliases: stateAliases, ClassifyRules: stateRules}
		if errNorm := normalizeConfig(&merged); errNorm != nil {
			return fmt.Errorf("load state: %w", errNorm)
		}
		keys = merged.Keys
		// Propagate the resolved alias table back to cfg for downstream use.
		cfg.Aliases = merged.Aliases
		cfg.ClassifyRules = merged.ClassifyRules
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		return fmt.Errorf("load state: %w", errLoad)
	} else {
		firstBoot = true
	}

	next := make(map[string]*KeyConfig, len(keys))
	now := time.Now().UTC()
	// Build the global alias lookup from the config (post-migration).
	aliasLookup := make(map[string]*AliasMapping, len(cfg.Aliases))
	for i := range cfg.Aliases {
		aliasLookup[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
	}

	for i := range keys {
		item := keys[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		if item.LimitsChangedAt.IsZero() && (item.DailyLimitUSD > 0 || item.WeeklyLimitUSD > 0 || item.MonthlyLimitUSD > 0 || hasAliasLimit(item.Aliases)) {
			item.LimitsChangedAt = item.CreatedAt
		}
		// If the key has Aliases refs, populate Models from the global table
		// so all downstream code (routing, billing, usage) works
		// unchanged. For round-robin aliases with multiple targets, we expand
		// to one ModelRule per target (the scheduler picks based on group).
		if len(item.Aliases) > 0 {
			item.Models = resolveAliasRefsToModels(item.Aliases, aliasLookup)
		}
		next[item.ID] = &item
	}

	s.mu.Lock()
	// Stop any prior flusher before rebuilding keys/state path. (StopUsageFlusher
	// above already handled the flush-then-stop for the old path; this guards
	// against a flusher that started after this point in a re-entrant call.)
	if s.flusher != nil {
		s.flusher.stop()
		s.flusher = nil
	}
	// The persisted state is authoritative during reconfigure. updateMu
	// serializes this replacement with management mutations, so a revoked or
	// deleted key cannot be resurrected from an older in-memory snapshot.
	s.enabled = cfg.Enabled
	s.statePath = statePath
	// Store the global alias table and classify rules for routing/billing.
	s.aliases = make(map[string]*AliasMapping, len(cfg.Aliases))
	for i := range cfg.Aliases {
		s.aliases[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
	}
	s.classifyRules = cfg.ClassifyRules
	s.auditLog = audit.New(policyPersist.AuditPath(statePath), audit.DefaultMaxBytes, audit.DefaultBackups)
	s.keys = next
	s.rebuildKeysByHashLocked()
	s.rrCounters = make(map[string]int)
	s.pendingPicks = make(map[string][]pendingPick)
	s.precharges = make(map[string][]time.Time)
	if s.limiter == nil {
		s.limiter = NewRateLimiter()
	}
	// Re-load usage into the (clock-bound) ledger for restart recovery. The
	// clock is preserved when set via SetClock; otherwise default time.Now.
	clockNow = s.usage.now
	s.usage = newUsageLedgerWithLocation(clockNow, cfg.usageLocation, cfg.UsageTimezone)
	s.usage.loadFromState(loadedUsage)

	// First boot seeds the config file. Loading a v1 state writes the independent
	// usage file before rewriting state, so a crash cannot discard legacy usage.
	var baseKeys []KeyConfig
	var baseUsage map[string]*UsageState
	var baseAliases []AliasMapping
	var baseRules []ClassifyRule
	if firstBoot || stateNeedsRewrite {
		baseKeys = s.keysSnapshotLocked()
		baseUsage = s.usageSnapshotLocked()
		baseAliases = s.aliasesSnapshotLocked()
		baseRules = s.classifyRulesSnapshotLocked()
	}
	s.mu.Unlock()
	if stateNeedsRewrite && len(baseUsage) > 0 {
		if errSave := SaveUsage(policyPersist.UsagePath(statePath), baseUsage); errSave != nil {
			return fmt.Errorf("migrate usage: %w", errSave)
		}
	}
	if firstBoot || stateNeedsRewrite {
		if errSave := s.saveState(statePath, baseKeys, baseAliases, baseRules); errSave != nil {
			return fmt.Errorf("seed state: %w", errSave)
		}
	}
	return nil
}

func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Store) runtimeComponents() (*RateLimiter, *usageLedger) {
	s.mu.RLock()
	limiter := s.limiter
	usage := s.usage
	s.mu.RUnlock()
	return limiter, usage
}

func (s *Store) StatePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statePath
}

func (s *Store) recordAudit(event audit.Event) {
	s.mu.RLock()
	logWriter := s.auditLog
	s.mu.RUnlock()
	if logWriter == nil {
		return
	}
	if err := logWriter.Append(event); err != nil {
		log.Printf("cpa-key-policy: audit write failed action=%q key_id=%q: %v", event.Action, event.KeyID, err)
	}
}

func (s *Store) AuditEvents(keyID string, limit int) ([]audit.Event, error) {
	s.mu.RLock()
	logWriter := s.auditLog
	s.mu.RUnlock()
	if logWriter == nil {
		return []audit.Event{}, nil
	}
	return logWriter.Query(strings.TrimSpace(keyID), limit)
}

// FindByAPIKey resolves a downstream plain key to policy (copy). Returns nil when unknown.
func (s *Store) FindByAPIKey(raw string) *KeyConfig {
	return s.findBySecret(raw)
}

func (s *Store) findBySecret(raw string) *KeyConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	hash, err := HashKey(raw)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := s.keysByHash[strings.ToLower(strings.TrimSpace(hash))]
	if key == nil {
		return nil
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy
}

func (s *Store) findBySecretWhenEnabled(raw string) (*KeyConfig, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		s.mu.RLock()
		enabled := s.enabled
		s.mu.RUnlock()
		return nil, enabled
	}
	hash, err := HashKey(raw)
	if err != nil {
		return nil, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return nil, false
	}
	key := s.keysByHash[strings.ToLower(strings.TrimSpace(hash))]
	if key == nil {
		return nil, true
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy, true
}

// findByID resolves a key config by its ID. The host's usage.handle call does
// NOT carry the client's plaintext key — CPA stores the plugin auth result's
// Principal (which THIS plugin sets to key.ID at store.go Authenticate) into
// the request context as "userApiKey", then forwards that as the UsageRecord's
// APIKey field. So the value we receive in usage.handle is key.ID, not the
// secret. Matching must therefore be ID-based, not hash-based.
func (s *Store) findByID(id string) *KeyConfig {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := s.keys[id]
	if key == nil {
		for candidateID, candidate := range s.keys {
			if strings.EqualFold(candidateID, id) {
				key = candidate
				break
			}
		}
	}
	if key == nil {
		return nil
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy
}

func (s *Store) rebuildKeysByHashLocked() {
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	byHash := make(map[string]*KeyConfig, len(ids))
	for _, id := range ids {
		key := s.keys[id]
		if key == nil {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(key.KeyHash))
		if hash == "" {
			continue
		}
		if _, exists := byHash[hash]; !exists {
			byHash[hash] = key
		}
	}
	s.keysByHash = byHash
}

func (k *KeyConfig) ModelForAlias(alias string) (ModelRule, bool) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ModelRule{}, false
	}
	for _, rule := range k.Models {
		if strings.EqualFold(rule.Alias, alias) {
			return rule, true
		}
	}
	return ModelRule{}, false
}

// resolveAliasRefsToModels expands a key's Alias refs into concrete ModelRule
// entries using the global alias table. For round-robin aliases with multiple
// targets, each target becomes a separate ModelRule (same alias, different
// provider/model/group) — the routing layer selects one at request time. Per-key
// price overrides on KeyAliasRef take precedence over the global alias pricing.
func resolveAliasRefsToModels(refs []KeyAliasRef, aliases map[string]*AliasMapping) []ModelRule {
	var out []ModelRule
	for _, ref := range refs {
		a, ok := aliases[strings.ToLower(ref.Alias)]
		if !ok {
			continue
		}
		for _, t := range a.Targets {
			rule := ModelRule{
				Alias:              a.Alias,
				Provider:           t.Provider,
				TargetModel:        t.TargetModel,
				Group:              t.Group,
				BillingMode:        a.BillingMode,
				AliasDailyLimitUSD: ref.DailyLimitUSD,
			}
			// Apply per-key price overrides (nil = use global default).
			if ref.InputPricePerMillion != nil {
				rule.InputPricePerMillion = *ref.InputPricePerMillion
			} else {
				rule.InputPricePerMillion = a.InputPricePerMillion
			}
			if ref.OutputPricePerMillion != nil {
				rule.OutputPricePerMillion = *ref.OutputPricePerMillion
			} else {
				rule.OutputPricePerMillion = a.OutputPricePerMillion
			}
			if ref.CacheReadPricePerMillion != nil {
				rule.CacheReadPricePerMillion = *ref.CacheReadPricePerMillion
			} else {
				rule.CacheReadPricePerMillion = a.CacheReadPricePerMillion
			}
			if ref.PerCallUSD != nil {
				rule.PerCallUSD = *ref.PerCallUSD
			} else {
				rule.PerCallUSD = a.PerCallUSD
			}
			out = append(out, rule)
		}
	}
	return out
}

// usageSnapshotLocked returns a deep copy of the usage ledger. Caller must
// hold s.mu (write or read) so Configure snapshots the selected state and its
// migrated usage before publishing the rebuilt store.
func (s *Store) usageSnapshotLocked() map[string]*UsageState {
	if s.usage == nil {
		return nil
	}
	return s.usage.snapshot()
}

// FlushUsage persists only a dirty usage snapshot. The revision handshake
// prevents a record that arrives during the write from being marked clean.
func (s *Store) FlushUsage() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	ledger := s.usage
	path := s.statePath
	s.mu.RUnlock()
	if path == "" || ledger == nil {
		return nil
	}
	usage, revision, dirty := ledger.snapshotForFlush()
	if !dirty {
		return nil
	}
	if err := SaveUsage(policyPersist.UsagePath(path), usage); err != nil {
		return err
	}
	ledger.markFlushed(revision)
	return nil
}

func (s *Store) saveState(path string, keys []KeyConfig, aliases []AliasMapping, rules []ClassifyRule) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return SaveState(path, keys, aliases, rules)
}

// StartUsageFlusher launches a goroutine that periodically persists the usage
// ledger to the independent usage file. Idempotent. Returns a stop function;
// the plugin host should call it (or FlushUsage) at reconfigure/shutdown.
func (s *Store) StartUsageFlusher() func() {
	s.mu.Lock()
	if s.flusher != nil {
		stop := s.flusher.stop
		s.mu.Unlock()
		return stop
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	f := &usageFlusher{stop: stop, stopCh: stopCh, doneCh: doneCh, store: s}
	s.flusher = f
	s.mu.Unlock()
	go f.loop()
	return stop
}

// StopUsageFlusher stops the background flusher and flushes once more.
func (s *Store) StopUsageFlusher() {
	s.mu.Lock()
	f := s.flusher
	s.flusher = nil
	s.mu.Unlock()
	if f != nil {
		f.stop()
		<-f.doneCh
	}
	_ = s.FlushUsage()
}

type usageFlusher struct {
	stop   func()
	stopCh chan struct{}
	doneCh chan struct{}
	store  *Store
}

func (f *usageFlusher) loop() {
	defer close(f.doneCh)
	t := time.NewTicker(usageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-t.C:
			_ = f.store.FlushUsage()
		}
	}
}
