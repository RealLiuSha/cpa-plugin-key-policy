package policy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A hot replacement hands the ledger from the quiesced instance to the newer
// one CPA loads next. CPA keeps calling the old instance until it switches
// its snapshot, and a few calls already under way reach it afterwards. So
// the old instance writes its accounting cut at quiesce and from then on
// appends what it still bills, the media generations it admitted and what
// other modules hand over (channel observations), to a journal next to the
// state file. The new instance loads the files, replays
// the journal, writes a cut and renames the journal right away, so neither a
// restart nor the next hot replacement replays it again, and keeps following
// the renamed file until it stops growing.
//
// TRADEOFF: a crash of the new instance between writing that cut and the
// rename replays the journal again on the next start, and a line the old
// instance appends after the new one stopped following is not billed. The
// windows are one rename and one in-flight usage record; review if hot
// replacement becomes the routine upgrade path under constant load.

const (
	handoffFile       = "cpa-key-policy-handoff.jsonl"
	handoffPoll       = time.Second
	handoffQuietAfter = 30 * time.Second
	handoffFollowMax  = 2 * time.Minute
	// A replayed journal is renamed with this infix and a UTC time, and kept
	// for handoffAppliedKeep.
	handoffAppliedInfix = ".applied-"
	handoffAppliedKeep  = 7 * 24 * time.Hour
)

func handoffPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), handoffFile)
}

// handoffEntry is one journal line: a billed record, an admitted media
// generation whose usage record has not arrived yet, or an entry another
// module owns, which the store passes on without reading it.
type handoffEntry struct {
	Type    string          `json:"type"`
	Key     string          `json:"key,omitempty"`
	Model   string          `json:"model,omitempty"`
	Bucket  *Bucket         `json:"bucket,omitempty"`
	Deduct  bool            `json:"deduct,omitempty"`
	Trace   string          `json:"trace,omitempty"`
	Module  string          `json:"module,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      time.Time       `json:"at"`
}

const (
	handoffUsage  = "usage"
	handoffTrace  = "trace"
	handoffModule = "module"
)

// HandoffReplay receives the entries other modules journaled, in order.
type HandoffReplay func(module string, payload json.RawMessage)

// handoffWriter appends journal lines in batches, each batch with one write
// and one fsync. The reader drops an incomplete last line, so a line is
// either whole or absent.
type handoffWriter struct {
	mu   sync.Mutex
	file *os.File
}

func openHandoffWriter(path string) (*handoffWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &handoffWriter{file: file}, nil
}

func (w *handoffWriter) append(entries ...handoffEntry) {
	var batch []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		batch = append(append(batch, line...), '\n')
	}
	if len(batch) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return
	}
	_, err := w.file.Write(batch)
	if err == nil {
		err = w.file.Sync()
	}
	if err != nil {
		log.Printf("cpa-key-policy: handoff journal write failed: %v", err)
	}
}

func (w *handoffWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

// journalUsage records one billed usage while handing off. Its time is the
// ledger clock's, read right after the booking, which the newer instance
// books the record at.
func (s *Store) journalUsage(keyID, model string, bucket Bucket, deduct bool) {
	if w := s.handoff.Load(); w != nil {
		w.append(handoffEntry{Type: handoffUsage, Key: keyID, Model: model, Bucket: &bucket, Deduct: deduct, At: s.clock()()})
	}
}

// journalTrace records one admitted media generation while handing off.
func (s *Store) journalTrace(traceID string, at time.Time) {
	if w := s.handoff.Load(); w != nil {
		w.append(handoffEntry{Type: handoffTrace, Trace: traceID, At: at})
	}
}

// JournalModule records an entry of another module while handing off; the
// newer instance passes it to the replay function given to AdoptHandoff.
// Outside a handoff it does nothing.
func (s *Store) JournalModule(module string, payload json.RawMessage) {
	if w := s.handoff.Load(); w != nil {
		w.append(handoffEntry{Type: handoffModule, Module: module, Payload: payload, At: time.Now()})
	}
}

// Quiesce writes the accounting cut, stops the periodic flusher and starts
// the handoff journal with the media generations still awaiting their usage
// record. CPA sends it just before loading a newer instance that reads the
// same files, which this instance must not overwrite afterwards. Resume
// undoes it when the replacement is rolled back.
func (s *Store) Quiesce() error {
	s.stopFollowing()
	// Billing waits while the cut is written and the journal starts, so no
	// record falls between the two.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	flushErr := s.stopUsageFlusher()
	s.mu.RLock()
	statePath, traces := s.statePath, s.mediaTraces
	s.mu.RUnlock()
	if statePath == "" {
		return flushErr
	}
	writer, err := openHandoffWriter(handoffPath(statePath))
	if err != nil {
		return errors.Join(flushErr, fmt.Errorf("open handoff journal: %w", err))
	}
	// One fsync for all of them: billing waits on lifecycleMu meanwhile, and
	// up to an hour of media generations may still await their record.
	live := traces.live()
	pending := make([]handoffEntry, 0, len(live))
	for _, trace := range live {
		pending = append(pending, handoffEntry{Type: handoffTrace, Trace: trace.id, At: trace.at})
	}
	writer.append(pending...)
	s.handoff.Store(writer)
	return flushErr
}

// Resume ends a handoff that did not happen: CPA rolled the replacement back
// and this instance, whose memory holds everything it billed meanwhile,
// writes the files again. It is also the normal start and does nothing more
// then.
func (s *Store) Resume() {
	if writer := s.handoff.Swap(nil); writer != nil {
		writer.close()
		s.mu.RLock()
		statePath := s.statePath
		s.mu.RUnlock()
		if err := os.Remove(handoffPath(statePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("cpa-key-policy: remove handoff journal: %v", err)
		}
	}
	s.StartUsageFlusher()
}

// AdoptHandoff replays the journal a quiesced instance left, closes it and
// follows it until it stops growing; replay receives the other modules'
// entries. The plugin calls it once the dataset is loaded, the data
// directory is its own and the modules are open, before the periodic
// flusher starts. It reports how many entries it replayed.
func (s *Store) AdoptHandoff(replay HandoffReplay) (int, error) {
	s.mu.RLock()
	statePath := s.statePath
	s.mu.RUnlock()
	path := handoffPath(statePath)
	removeOldAppliedHandoffs(path, time.Now())
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open handoff journal: %w", err)
	}
	reader := &handoffReader{file: file}
	entries, err := reader.next()
	if err != nil {
		_ = file.Close()
		return 0, err
	}
	s.applyHandoff(entries, replay)
	closed := s.closeHandoff(path)
	follower := &handoffFollower{stop: make(chan struct{}), done: make(chan struct{})}
	s.followMu.Lock()
	s.follower = follower
	s.followMu.Unlock()
	go s.followHandoff(follower, reader, path, closed, replay)
	return len(entries), nil
}

// closeHandoff writes a cut that contains every entry applied so far, then
// renames the journal; the open file keeps receiving the old instance's late
// lines. It reports false when the cut failed and the journal stays.
func (s *Store) closeHandoff(path string) bool {
	if err := s.FlushUsage(); err != nil {
		log.Printf("cpa-key-policy: flush after handoff replay failed, retrying: %v", err)
		return false
	}
	applied := path + handoffAppliedInfix + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, applied); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("cpa-key-policy: rename handoff journal: %v", err)
	}
	return true
}

// removeOldAppliedHandoffs deletes replayed journals older than
// handoffAppliedKeep, by wall-clock modification time like the temp file
// sweep. They were billed long before and only serve an investigation, so a
// failure is logged and never stops a registration.
func removeOldAppliedHandoffs(path string, now time.Time) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+handoffAppliedInfix
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("cpa-key-policy: list replayed handoff journals: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < handoffAppliedKeep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("cpa-key-policy: remove replayed handoff journal: %v", err)
		}
	}
}

func (s *Store) applyHandoff(entries []handoffEntry, replay HandoffReplay) {
	if len(entries) == 0 {
		return
	}
	_, ledger := s.runtimeComponents()
	s.mu.RLock()
	traces := s.mediaTraces
	s.mu.RUnlock()
	for _, entry := range entries {
		switch {
		case entry.Type == handoffUsage && entry.Bucket != nil && entry.Key != "" && entry.Model != "":
			ledger.recordJournaled(entry.Key, entry.Model, *entry.Bucket, entry.Deduct, entry.At)
		case entry.Type == handoffTrace && entry.Trace != "":
			traces.rememberAt(entry.Trace, entry.At)
		case entry.Type == handoffModule && entry.Module != "" && replay != nil:
			replay(entry.Module, entry.Payload)
		}
	}
}

// recordJournaled is record for a journaled entry, booked as of at, when the
// quiesced instance billed it: on that day, or the first open day when that
// one is closed, and only in the periods that had started by then. So a
// record billed before midnight and replayed after it, or replayed at a
// later start from a journal a crash left, stays on its own day and does not
// count against periods that began after it. The balance is charged as when
// it was billed. A zero at, or one ahead of the clock, is now.
func (l *usageLedger) recordJournaled(id, model string, delta Bucket, deduct bool, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	usage := l.entries[id]
	if usage == nil {
		usage = &KeyUsage{Days: make(map[string]map[string]Bucket)}
		l.entries[id] = usage
	}
	if usage.Cycles != nil {
		l.advanceCyclesLocked(usage.Cycles, now)
		for _, window := range quotaWindows {
			cycle := usage.Cycles.cycle(window)
			if at.Before(cycle.StartedAt) {
				continue
			}
			bucket := cycle.ByModel[model]
			bucket.add(delta)
			cycle.ByModel[model] = bucket
		}
		if deduct {
			if usage.Balance == nil {
				usage.Balance = &Balance{}
			}
			usage.Balance.Nano -= delta.ChargedNano
		}
	}
	date := l.openDateLocked(at)
	models := usage.Days[date]
	if models == nil {
		models = make(map[string]Bucket)
		usage.Days[date] = models
	}
	bucket := models[model]
	bucket.add(delta)
	models[model] = bucket
	l.evictExpiredLocked(now)
	l.markDirtyLocked()
}

type handoffFollower struct {
	stop chan struct{}
	done chan struct{}
}

// followHandoff applies what the quiesced instance still appends and writes
// a cut after each batch; it retries closing the journal when AdoptHandoff
// could not. Stopped by a quiesce or shutdown of this store, it applies what
// is left and closes the journal first, so the cut that follows contains
// those entries and no later instance replays them.
func (s *Store) followHandoff(follower *handoffFollower, reader *handoffReader, path string, closed bool, replay HandoffReplay) {
	defer close(follower.done)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("cpa-key-policy: handoff follow panicked: %v", recovered)
		}
	}()
	defer func() { _ = reader.file.Close() }()
	started, lastGrowth := time.Now(), time.Now()
	ticker := time.NewTicker(handoffPoll)
	defer ticker.Stop()
	for {
		stopping := false
		select {
		case <-follower.stop:
			stopping = true
		case <-ticker.C:
		}
		entries, err := reader.next()
		if err != nil {
			log.Printf("cpa-key-policy: handoff journal read failed: %v", err)
			return
		}
		s.applyHandoff(entries, replay)
		if len(entries) > 0 {
			lastGrowth = time.Now()
		}
		switch {
		case !closed:
			closed = s.closeHandoff(path)
		case len(entries) > 0 && !stopping:
			if err := s.FlushUsage(); err != nil {
				log.Printf("cpa-key-policy: flush after handoff replay failed: %v", err)
			}
		}
		quiet := time.Since(lastGrowth) >= handoffQuietAfter || time.Since(started) >= handoffFollowMax
		if stopping || (closed && quiet) {
			return
		}
	}
}

func (s *Store) stopFollowing() {
	s.followMu.Lock()
	follower := s.follower
	s.follower = nil
	s.followMu.Unlock()
	if follower != nil {
		close(follower.stop)
		<-follower.done
	}
}

// handoffReader reads whole lines appended since the last call.
type handoffReader struct {
	file    *os.File
	partial []byte
}

func (r *handoffReader) next() ([]handoffEntry, error) {
	raw, err := io.ReadAll(r.file)
	if err != nil {
		return nil, err
	}
	raw = append(r.partial, raw...)
	cut := bytes.LastIndexByte(raw, '\n')
	if cut < 0 {
		r.partial = raw
		return nil, nil
	}
	r.partial = append([]byte(nil), raw[cut+1:]...)
	var entries []handoffEntry
	scanner := bufio.NewScanner(bytes.NewReader(raw[:cut+1]))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var entry handoffEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			log.Printf("cpa-key-policy: skipping malformed handoff line: %v", err)
			continue
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}
