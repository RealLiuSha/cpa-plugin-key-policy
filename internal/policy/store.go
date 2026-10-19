package policy

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	// handoff is set from Quiesce until Resume: billed usage and admitted
	// media generations are then also journaled for the newer instance.
	handoff  atomic.Pointer[handoffWriter]
	followMu sync.Mutex
	follower *handoffFollower

	// logf, when set, receives the ledger turning unsaved and saved again,
	// so they reach CPA's log; stdlib log only reaches stderr.
	logf func(level, message string)
	// unsaved is set from a failed usage flush to the next successful one,
	// whichever path flushes; persistMu guards it.
	unsaved bool
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

// SetLogf sends the lines that report the usage ledger turning unsaved and
// saved again to logf. It is called before the first configuration.
func (s *Store) SetLogf(logf func(level, message string)) {
	s.logf = logf
}

// report writes one line through logf, or to stderr without it.
func (s *Store) report(level, message string) {
	if s.logf != nil {
		s.logf(level, message)
		return
	}
	log.Print(message)
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

func (s *Store) Configure(cfg Config) error {
	return s.configure(cfg, false, false)
}

// Reload replaces what memory holds with the dataset in the files, which
// another instance may have written since this store gave them up. Memory is
// not written out first, and the journal this store kept since its quiesce
// is closed once the files have loaded; the caller replays the journal in
// the directory next (AdoptHandoff). journaled says this store gave the files
// up by a quiesce, which journaled the media generations it had admitted.
func (s *Store) Reload(cfg Config, journaled bool) error {
	return s.configure(cfg, true, journaled)
}

func (s *Store) configure(cfg Config, reload, journaled bool) (err error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	statePath, err := ResolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}
	// CPA re-sends the configuration on every config apply, panel saves that
	// never touch this block included. With the same dataset already in
	// memory only the enabled switch can change, so nothing is reloaded and
	// the request path is never held up. Edits made to the files while the
	// plugin runs are not picked up; they need a restart. Another timezone
	// for the same files is refused right here: the files carry the zone in
	// memory, so loading them again could only fail.
	if loaded, current := s.datasetLoaded(statePath); loaded && !reload {
		if zone, _ := resolveUsageZone(cfg.UsageTimezone); zone != current {
			return fmt.Errorf("usage ledger uses quota timezone %q; keep usage_timezone consistent", current)
		}
		s.mu.Lock()
		s.enabled = cfg.Enabled
		s.mu.Unlock()
		return nil
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.RLock()
	wasFlushing := s.flusher != nil
	s.mu.RUnlock()
	defer func() {
		if err != nil && wasFlushing && !reload {
			s.StartUsageFlusher()
		}
	}()
	s.stopFollowing()
	if reload {
		// Memory is older than the files: it is replaced, never written.
		s.stopFlusher()
	} else if err := s.stopUsageFlusher(); err != nil {
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

	if reload {
		// Billing waits on lifecycleMu, so nothing is journaled after this.
		if writer := s.handoff.Swap(nil); writer != nil {
			writer.close()
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
	s.prices = prices
	s.auditLog = audit.New(persist.AuditPath(statePath), audit.DefaultMaxBytes, audit.DefaultBackups)
	s.rebuildKeysByHashLocked()
	s.usage = nextUsage
	// mediaTraces outlives reconfiguration: a media generation admitted before
	// a reload still has to be billed when its usage record arrives. After a
	// quiesce a reload takes the generations still awaiting a record from the
	// journal instead: the instance that adopted this store's journal may have
	// billed them. A store shut down without a quiesce handed its own to no one.
	if reload && journaled {
		s.mediaTraces = newMediaTraceSet(clockNow)
	}
	s.mu.Unlock()
	return nil
}

// datasetLoaded reports whether the dataset at statePath is in memory, and
// the usage timezone its ledger counts in.
func (s *Store) datasetLoaded(statePath string) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.datasetID != "" && s.statePath == statePath, s.usage.timezone
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

// RecordChange audits a management change outside the key store, such as a
// channel or adapter rule; target names what changed.
func (s *Store) RecordChange(action, target string, changes map[string]audit.Change) {
	s.recordAudit(audit.Event{Actor: "management", Action: action, Target: target, Changes: changes})
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
		s.noteFlushLocked(err)
		return err
	}
	if historyErr != nil {
		health.flushFailed(historyErr)
		s.noteFlushLocked(historyErr)
		return historyErr
	}
	health.flushSucceeded(time.Now())
	s.noteFlushLocked(nil)
	return nil
}

// noteFlushLocked reports the ledger turning unsaved and saved again, once
// each way, whichever flush sees it: the periodic one, a quiesce, a shutdown
// or a key deletion. A failed flush keeps the ledger dirty, so only a real
// write reports it saved again. persistMu is held.
func (s *Store) noteFlushLocked(err error) {
	switch {
	case err != nil && !s.unsaved:
		s.unsaved = true
		s.report("error", fmt.Sprintf("cpa-key-policy: usage ledger cannot be saved, billing stays in memory until a flush succeeds (retried every %s): %v", usageFlushInterval, err))
	case err == nil && s.unsaved:
		s.unsaved = false
		s.report("info", "cpa-key-policy: usage ledger saved again")
	}
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
// history first when days have closed, so nothing stays only in memory. A
// store that is handing off wrote its cut at Quiesce and no longer owns the
// files, so it writes nothing.
func (s *Store) Shutdown() error {
	s.stopFollowing()
	if writer := s.handoff.Swap(nil); writer != nil {
		writer.close()
		return nil
	}
	return s.stopUsageFlusher()
}

// Stop ends the journal follower, the handoff journal and the periodic
// flusher without writing anything: the plugin calls it at shutdown when the
// data directory is not its own.
//
// The closed journal writer stays in place. On CPA HEAD a hot replacement by
// the same bytes reaches this same store: when CPA discards that load, it
// shuts the store down here and then rolls back with a reconfigure, whose
// Resume removes the journal only through the writer. Memory holds every
// entry of that journal, so a journal left behind would bill them again at
// the next start or replacement. A closed writer appends nothing.
func (s *Store) Stop() {
	s.stopFollowing()
	if writer := s.handoff.Load(); writer != nil {
		writer.close()
	}
	s.stopFlusher()
}

// Loaded reports whether a dataset has been loaded.
func (s *Store) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.datasetID != ""
}

func (s *Store) stopUsageFlusher() error {
	s.stopFlusher()
	return s.FlushUsage()
}

// stopFlusher ends the periodic flusher and waits for a flush under way.
func (s *Store) stopFlusher() {
	s.mu.Lock()
	f := s.flusher
	s.flusher = nil
	s.mu.Unlock()
	if f != nil {
		f.stop()
		<-f.doneCh
	}
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
			f.flush()
		}
	}
}

// flush runs one periodic flush. A panic here would end the whole CPA
// process, since the plugin shares it, so it is counted as a failed flush
// and retried next interval. Every failure goes to stderr; FlushUsage sends
// the first one and the recovery to CPA's log.
func (f *usageFlusher) flush() {
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("usage flush panicked: %v", recovered)
			f.store.health.flushFailed(err)
			log.Printf("cpa-key-policy: %v", err)
		}
	}()
	if err := f.store.FlushUsage(); err != nil {
		log.Printf("cpa-key-policy: usage flush failed, retrying next interval: %v", err)
	}
}
