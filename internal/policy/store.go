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
	// Reconfiguration must not replace a ledger while authentication or billing
	// still holds the previous runtime. Normal requests share this read lock.
	lifecycleMu            sync.RWMutex
	mu                     sync.RWMutex
	updateMu               sync.Mutex
	persistMu              sync.Mutex
	enabled                bool
	statePath              string
	datasetID              string
	keys                   map[string]*KeyConfig
	keysByHash             map[string]*KeyConfig
	models                 map[string]*ModelDefinition
	classifyRules          []ClassifyRule
	limiter                *RateLimiter
	usage                  *usageLedger
	auditLog               *audit.Log
	flusher                *usageFlusher
	rrCounters             map[string]int
	pendingPicks           map[string][]pendingPick
	precharges             map[string][]time.Time
	onClassifyRulesChanged func()
}

var ErrInvalidUsageResetWindow = errors.New("invalid usage reset window")
var ErrUsageResetChanged = errors.New("quota period or reset date changed")
var ErrInvalidUsageResetExpectation = errors.New("expected quota period and reset date are required")

type pendingPick struct {
	route ResolvedModelRoute
	at    time.Time
}

const pendingPickTTL = 30 * time.Second
const pendingPickMaxQueue = 32

type AuthDecision struct {
	Known       bool
	Allowed     bool
	KeyID       string
	Principal   string
	Requested   string
	Route       ResolvedModelRoute
	Reason      string
	ModelList   bool
	RateLimited bool
	CostLimited bool
	PreCharged  bool
}

func NewStore() *Store {
	return &Store{
		enabled:      DefaultConfig().Enabled,
		keys:         make(map[string]*KeyConfig),
		keysByHash:   make(map[string]*KeyConfig),
		models:       make(map[string]*ModelDefinition),
		limiter:      NewRateLimiter(),
		usage:        newUsageLedger(time.Now),
		rrCounters:   make(map[string]int),
		pendingPicks: make(map[string][]pendingPick),
		precharges:   make(map[string][]time.Time),
	}
}

func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	s.limiter = NewRateLimiterWithClock(now)
	s.usage = newUsageLedger(now)
	s.mu.Unlock()
}

func (s *Store) Configure(cfg Config) (err error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.RLock()
	wasFlushing := s.flusher != nil
	s.mu.RUnlock()
	defer func() {
		if err != nil && wasFlushing {
			s.StartUsageFlusher()
		}
	}()
	statePath, err := ResolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}

	if err := s.stopUsageFlusher(); err != nil {
		return fmt.Errorf("flush usage before reconfigure: %w", err)
	}
	s.mu.RLock()
	clockNow := time.Now
	if s.usage != nil {
		clockNow = s.usage.now
	}
	s.mu.RUnlock()
	usagePath := policyPersist.UsagePath(statePath)
	if err := policyPersist.CleanupStaleTemps(statePath, clockNow()); err != nil {
		return fmt.Errorf("cleanup state temp files: %w", err)
	}
	if err := policyPersist.CleanupStaleTemps(usagePath, clockNow()); err != nil {
		return fmt.Errorf("cleanup usage temp files: %w", err)
	}

	keys := cfg.Keys
	models := cfg.Models
	rules := cfg.ClassifyRules
	usage := make(map[string]*UsageState)
	datasetID := ""
	firstBoot := false
	stateVersion, usageVersion := currentStateFileVersion, currentUsageFileVersion
	state, stateErr := LoadState(statePath)
	switch {
	case stateErr == nil:
		usageFile, usageErr := LoadUsage(usagePath)
		if usageErr != nil {
			return fmt.Errorf("load usage paired with state: %w", usageErr)
		}
		if state.DatasetID != usageFile.DatasetID {
			return fmt.Errorf("state/usage dataset_id mismatch: state=%q usage=%q", state.DatasetID, usageFile.DatasetID)
		}
		stateVersion, usageVersion = state.Version, usageFile.Version
		keys = state.Keys
		models = state.Models
		rules = state.ClassifyRules
		usage = usageFile.Usage
		datasetID = state.DatasetID
	case errors.Is(stateErr, os.ErrNotExist):
		if _, usageErr := os.Stat(usagePath); usageErr == nil {
			return errors.New("usage file exists without its paired state file")
		} else if !errors.Is(usageErr, os.ErrNotExist) {
			return fmt.Errorf("inspect usage file: %w", usageErr)
		}
		datasetID, err = NewDatasetID()
		if err != nil {
			return err
		}
		firstBoot = true
	default:
		return fmt.Errorf("load state: %w", stateErr)
	}

	effective := Config{
		Enabled: cfg.Enabled, StateFile: cfg.StateFile, UsageTimezone: cfg.UsageTimezone,
		Keys: keys, Models: models, ClassifyRules: rules,
	}
	if err := normalizeConfig(&effective); err != nil {
		if firstBoot {
			return fmt.Errorf("validate initial configuration: %w", err)
		}
		return fmt.Errorf("validate state: %w", err)
	}
	cfg = effective
	keys, models, rules = cfg.Keys, cfg.Models, cfg.ClassifyRules

	now := clockNow().UTC()
	nextKeys := make(map[string]*KeyConfig, len(keys))
	for i := range keys {
		item := keys[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		if item.LimitsChangedAt.IsZero() && (item.DailyLimitUSD > 0 || item.WeeklyLimitUSD > 0 || item.MonthlyLimitUSD > 0 || hasModelLimit(item.Models)) {
			item.LimitsChangedAt = item.CreatedAt
		}
		keys[i] = item
		copy := item
		copy.Models = append([]KeyModelRef(nil), item.Models...)
		nextKeys[item.ID] = &copy
	}
	nextModels := make(map[string]*ModelDefinition, len(models))
	for i := range models {
		copy := models[i]
		copy.Targets = append([]ModelTarget(nil), models[i].Targets...)
		copy.CacheWritePricePerMillion = cloneFloat64(models[i].CacheWritePricePerMillion)
		nextModels[strings.ToLower(copy.Name)] = &copy
	}

	nextUsage := newUsageLedgerWithLocation(clockNow, cfg.usageLocation, cfg.UsageTimezone)
	for id, entry := range usage {
		if entry.Cycles != nil && entry.Cycles.Timezone != cfg.UsageTimezone {
			return fmt.Errorf("key %q uses quota timezone %q; keep usage_timezone consistent", id, entry.Cycles.Timezone)
		}
	}
	nextUsage.loadFromState(usage)
	nextUsage.initializeCycles(keys, usageVersion < 5)
	usage = nextUsage.snapshot()
	cfg.Keys = keys
	if !firstBoot {
		if err := migrateStorage(statePath, datasetID, stateVersion, usageVersion, cfg, usage); err != nil {
			return err
		}
	}
	if firstBoot {
		if err := SaveUsage(usagePath, datasetID, usage); err != nil {
			return fmt.Errorf("seed usage: %w", err)
		}
		if err := SaveState(statePath, datasetID, keys, models, rules); err != nil {
			_ = os.Remove(usagePath)
			return fmt.Errorf("seed state: %w", err)
		}
	}

	s.mu.Lock()
	if s.flusher != nil {
		s.flusher.stop()
		s.flusher = nil
	}
	s.enabled = cfg.Enabled
	s.statePath = statePath
	s.datasetID = datasetID
	s.keys = nextKeys
	s.models = nextModels
	s.classifyRules = rules
	s.auditLog = audit.New(policyPersist.AuditPath(statePath), audit.DefaultMaxBytes, audit.DefaultBackups)
	s.rebuildKeysByHashLocked()
	s.rrCounters = make(map[string]int)
	s.pendingPicks = make(map[string][]pendingPick)
	s.precharges = make(map[string][]time.Time)
	if s.limiter == nil {
		s.limiter = NewRateLimiter()
	}
	s.usage = nextUsage
	s.mu.Unlock()
	return nil
}

func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Store) runtimeComponents() (*RateLimiter, *usageLedger) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.limiter, s.usage
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
	return cloneKeyConfig(s.keysByHash[strings.ToLower(strings.TrimSpace(hash))])
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
	return cloneKeyConfig(s.keysByHash[strings.ToLower(strings.TrimSpace(hash))]), true
}

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
	return cloneKeyConfig(key)
}

func cloneKeyConfig(key *KeyConfig) *KeyConfig {
	if key == nil {
		return nil
	}
	copy := *key
	copy.Models = append([]KeyModelRef(nil), key.Models...)
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
		if hash != "" {
			if _, exists := byHash[hash]; !exists {
				byHash[hash] = key
			}
		}
	}
	s.keysByHash = byHash
}

func (s *Store) modelForKey(key *KeyConfig, requested string) (ModelDefinition, bool) {
	requested = strings.TrimSpace(requested)
	if key == nil || requested == "" {
		return ModelDefinition{}, false
	}
	allowed := false
	for _, ref := range key.Models {
		if strings.EqualFold(ref.Name, requested) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ModelDefinition{}, false
	}
	s.mu.RLock()
	model := s.models[strings.ToLower(requested)]
	if model == nil {
		s.mu.RUnlock()
		return ModelDefinition{}, false
	}
	copy := *model
	copy.Targets = append([]ModelTarget(nil), model.Targets...)
	s.mu.RUnlock()
	return copy, true
}

func (s *Store) usageSnapshotLocked() map[string]*UsageState {
	if s.usage == nil {
		return nil
	}
	return s.usage.snapshot()
}

func (s *Store) FlushUsage() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	ledger := s.usage
	path := s.statePath
	datasetID := s.datasetID
	s.mu.RUnlock()
	if path == "" || datasetID == "" || ledger == nil {
		return nil
	}
	usage, revision, dirty := ledger.snapshotForFlush()
	if !dirty {
		return nil
	}
	if err := SaveUsage(policyPersist.UsagePath(path), datasetID, usage); err != nil {
		return err
	}
	ledger.markFlushed(revision)
	return nil
}

func (s *Store) saveState(path, datasetID string, keys []KeyConfig, models []ModelDefinition, rules []ClassifyRule) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return SaveState(path, datasetID, keys, models, rules)
}

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

func (s *Store) StopUsageFlusher() {
	if err := s.stopUsageFlusher(); err != nil {
		log.Printf("cpa-key-policy: flush usage on stop: %v", err)
	}
}

func (s *Store) stopUsageFlusher() error {
	s.mu.Lock()
	f := s.flusher
	s.flusher = nil
	s.mu.Unlock()
	if f != nil {
		f.stop()
		<-f.doneCh
	}
	return s.FlushUsage()
}

type usageFlusher struct {
	stop   func()
	stopCh chan struct{}
	doneCh chan struct{}
	store  *Store
}

func (f *usageFlusher) loop() {
	defer close(f.doneCh)
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			_ = f.store.FlushUsage()
		}
	}
}
