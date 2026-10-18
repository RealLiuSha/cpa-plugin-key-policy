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
	"cpa-key-policy/internal/policy/persist"
)

type Store struct {
	// Reconfiguration must not replace a ledger while authentication or billing
	// still holds the previous runtime. Normal requests share this read lock.
	lifecycleMu sync.RWMutex
	mu          sync.RWMutex
	updateMu    sync.Mutex
	// persistMu serializes every ledger write. Lock order is persistMu →
	// ledger.mu → store.mu, never the reverse.
	persistMu   sync.Mutex
	enabled     bool
	statePath   string
	datasetID   string
	keys        map[string]*KeyConfig
	keysByHash  map[string]*KeyConfig
	prices      *priceTable
	limiter     *RateLimiter
	usage       *usageLedger
	auditLog    *audit.Log
	flusher     *usageFlusher
	mediaTraces *mediaTraceSet
	health      *Health
}

var (
	ErrUnknownKey                   = errors.New("unknown key")
	ErrInvalidUsageResetWindow      = errors.New("invalid usage reset window")
	ErrUsageResetChanged            = errors.New("quota period or reset date changed")
	ErrInvalidUsageResetExpectation = errors.New("expected quota period and reset date are required")
	// ErrStatePersist wraps every failure to write the state file from a
	// management write, so callers can tell it from a rejected input.
	ErrStatePersist = errors.New("persist state")
)

func NewStore() *Store {
	location, err := time.LoadLocation(DefaultConfig().UsageTimezone)
	if err != nil {
		location = time.UTC
	}
	return &Store{
		enabled:     DefaultConfig().Enabled,
		keys:        make(map[string]*KeyConfig),
		keysByHash:  make(map[string]*KeyConfig),
		prices:      &priceTable{index: make(map[string]int)},
		limiter:     NewRateLimiter(),
		usage:       newUsageLedger(time.Now, location, location.String()),
		mediaTraces: newMediaTraceSet(time.Now),
		health:      &Health{},
	}
}

// SetClock replaces the time source of every time-dependent component.
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.RLock()
	limiter, ledger, traces := s.limiter, s.usage, s.mediaTraces
	s.mu.RUnlock()
	limiter.setClock(now)
	ledger.setClock(now)
	traces.setClock(now)
}

func (s *Store) clock() func() time.Time {
	s.mu.RLock()
	ledger := s.usage
	s.mu.RUnlock()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.now
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
	clockNow := s.clock()
	livePath, historyPath := persist.UsagePath(statePath), persist.HistoryPath(statePath)
	// Temp files carry wall-clock modification times, so their age is not
	// measured with the ledger clock.
	for _, path := range []string{statePath, livePath, historyPath} {
		if err := persist.CleanupStaleTemps(path, time.Now()); err != nil {
			return fmt.Errorf("cleanup temp files: %w", err)
		}
	}

	effective := Config{Enabled: cfg.Enabled, StateFile: cfg.StateFile, UsageTimezone: cfg.UsageTimezone, Keys: append([]KeyConfig{}, cfg.Keys...), Models: append([]ModelPrice{}, cfg.Models...)}
	seedErr := normalizeConfig(&effective)
	zone, location := effective.UsageTimezone, effective.usageLocation
	now := clockNow()

	var keys []KeyConfig
	var models []ModelPrice
	datasetID := ""
	firstBoot := false
	dataset := &UsageDataset{Keys: make(map[string]*KeyUsage)}
	_, stateErr := os.Stat(statePath)
	switch {
	case stateErr == nil:
		state, err := LoadState(statePath)
		if err != nil {
			return fmt.Errorf("load state: %w", err)
		}
		if dataset, err = LoadUsage(statePath); err != nil {
			return fmt.Errorf("load usage paired with state: %w", err)
		}
		if state.DatasetID != dataset.DatasetID {
			return fmt.Errorf("state/usage dataset_id mismatch: state=%q usage=%q", state.DatasetID, dataset.DatasetID)
		}
		if dataset.Timezone != zone {
			return fmt.Errorf("usage ledger uses quota timezone %q; keep usage_timezone consistent", dataset.Timezone)
		}
		// A watermark ahead of the clock would close every day as it arrives;
		// one on today is a restart after a clock rollback and is served from
		// the first open day (usageLedger.reportEndLocked).
		if today := now.In(location).Format(dateLayout); dataset.Through > today {
			return fmt.Errorf("usage history through %s is after today (%s); restore the files or fix the clock", dataset.Through, today)
		}
		keys, models, datasetID = state.Keys, state.Models, state.DatasetID
	case errors.Is(stateErr, os.ErrNotExist):
		for _, path := range []string{livePath, historyPath} {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s exists without its paired state file", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect usage file: %w", err)
			}
		}
		if seedErr != nil {
			return fmt.Errorf("validate initial configuration: %w", seedErr)
		}
		if datasetID, err = NewDatasetID(); err != nil {
			return err
		}
		keys, models, firstBoot = effective.Keys, effective.Models, true
	default:
		return fmt.Errorf("load state: %w", stateErr)
	}

	nextKeys := make(map[string]*KeyConfig, len(keys))
	for i := range keys {
		item := keys[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now.UTC()
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		keys[i] = item
		copy := item
		nextKeys[item.ID] = &copy
	}
	prices, err := normalizeModelPrices(models)
	if err != nil {
		return fmt.Errorf("validate state: %w", err)
	}
	nextUsage := newUsageLedger(clockNow, location, zone)
	nextUsage.load(dataset.Keys, dataset.Through)
	nextUsage.syncCycles(keys)
	if firstBoot {
		nextUsage.mu.Lock()
		live := nextUsage.liveSnapshotLocked("")
		nextUsage.mu.Unlock()
		if err := writeLive(livePath, datasetID, zone, "", live, now); err != nil {
			return fmt.Errorf("seed usage: %w", err)
		}
		if err := SaveState(statePath, datasetID, keys, models); err != nil {
			_ = os.Remove(livePath)
			return fmt.Errorf("seed state: %w", err)
		}
		nextUsage.mu.Lock()
		nextUsage.dirty = false
		nextUsage.mu.Unlock()
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
	s.prices = prices
	s.auditLog = audit.New(persist.AuditPath(statePath), audit.DefaultMaxBytes, audit.DefaultBackups)
	s.rebuildKeysByHashLocked()
	s.usage = nextUsage
	// mediaTraces outlives reconfiguration: a media generation admitted before
	// a reload still has to be billed when its usage record arrives.
	s.mu.Unlock()
	return nil
}

func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Store) StatePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statePath
}

func (s *Store) runtimeComponents() (*RateLimiter, *usageLedger) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.limiter, s.usage
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

func (s *Store) AuditEvents(keyID, action string, limit int) ([]audit.Event, error) {
	s.mu.RLock()
	logWriter := s.auditLog
	s.mu.RUnlock()
	if logWriter == nil {
		return []audit.Event{}, nil
	}
	return logWriter.Query(strings.TrimSpace(keyID), strings.TrimSpace(action), limit)
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
	return cloneKeyConfig(s.keysByHash[strings.ToLower(hash)])
}

func (s *Store) findBySecretWhenEnabled(raw string) (*KeyConfig, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, s.Enabled()
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
	return cloneKeyConfig(s.keysByHash[strings.ToLower(hash)]), true
}

// findByID matches the id exactly: ids are unique by exact string, so a
// case-insensitive match could name another key.
func (s *Store) findByID(id string) *KeyConfig {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneKeyConfig(s.keys[id])
}

func cloneKeyConfig(key *KeyConfig) *KeyConfig {
	if key == nil {
		return nil
	}
	copy := *key
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
		if hash := strings.ToLower(key.KeyHash); hash != "" {
			if _, exists := byHash[hash]; !exists {
				byHash[hash] = key
			}
		}
	}
	s.keysByHash = byHash
}

func (s *Store) Keys() []KeyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keysSnapshotLocked()
}

func (s *Store) keysSnapshotLocked() []KeyConfig {
	keys := make([]KeyConfig, 0, len(s.keys))
	for _, key := range s.keys {
		keys = append(keys, *key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// FlushUsage writes one accounting cut: the history file first when a day
// has closed since the last history write, then the live file. A failed
// history write leaves the closed days in the live file and is retried on
// the next flush; every failure is logged, counted and reported by Status.
func (s *Store) FlushUsage() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	ledger, path, datasetID, health := s.usage, s.statePath, s.datasetID, s.health
	s.mu.RUnlock()
	if path == "" || datasetID == "" {
		return nil
	}
	plan, ok := ledger.planFlush()
	if !ok {
		return nil
	}
	through, historyWritten := plan.through, false
	var historyErr error
	if plan.history != nil {
		if err := writeHistory(persist.HistoryPath(path), datasetID, plan.historyThrough, plan.history, time.Now()); err != nil {
			historyErr = fmt.Errorf("write usage history: %w", err)
		} else {
			through, historyWritten = plan.historyThrough, true
		}
	}
	liveErr := writeLive(persist.UsagePath(path), datasetID, ledger.timezone, through, plan.live, time.Now())
	ledger.markFlushed(plan, historyWritten, liveErr == nil)
	if liveErr != nil {
		err := fmt.Errorf("write live usage: %w", liveErr)
		if historyErr != nil {
			err = fmt.Errorf("%w; %v", historyErr, err)
		}
		health.flushFailed(err)
		return err
	}
	if historyErr != nil {
		health.flushFailed(historyErr)
		return historyErr
	}
	health.flushSucceeded(time.Now())
	return nil
}

// saveState writes the state file for a management write. Callers validate
// the rows first, so a failure here is a persistence failure.
func (s *Store) saveState(path, datasetID string, keys []KeyConfig, models []ModelPrice) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if err := SaveState(path, datasetID, keys, models); err != nil {
		return fmt.Errorf("%w: %w", ErrStatePersist, err)
	}
	return nil
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

// Shutdown stops the periodic flusher and writes the final accounting cut,
// history first when days have closed, so nothing stays only in memory.
func (s *Store) Shutdown() error {
	return s.stopUsageFlusher()
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
			if err := f.store.FlushUsage(); err != nil {
				log.Printf("cpa-key-policy: usage flush failed, retrying next interval: %v", err)
			}
		}
	}
}
