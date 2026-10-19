package quality

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// historyLimit is how many records of each test a credential keeps.
const historyLimit = 20

// historyRetention is how long a record is kept, the ledger's 90 days: the
// records of a credential removed from CPA expire with it.
const historyRetention = 90 * 24 * time.Hour

// historyFlushDelay gathers the records tests add into one write: every write
// is the whole file.
const historyFlushDelay = 5 * time.Second

// history is every credential's records of each test, oldest first. Its JSON
// form is candy-eval's state file, so that file imports as is.
type history struct {
	Results      map[string][]CandyResult       `json:"results"`
	Fingerprints map[string][]FingerprintResult `json:"fingerprints"`
	ModelTraces  map[string][]TraceResult       `json:"modeltraces,omitempty"`
	Pelicans     map[string][]PelicanResult     `json:"pelicans,omitempty"`
}

func emptyHistory() history {
	return history{
		Results:      map[string][]CandyResult{},
		Fingerprints: map[string][]FingerprintResult{},
		ModelTraces:  map[string][]TraceResult{},
		Pelicans:     map[string][]PelicanResult{},
	}
}

// loadHistory reads one history file; found is false when it does not exist.
func loadHistory(path string) (h history, found bool, err error) {
	h = emptyHistory()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, true, err
	}
	var decoded *history
	if err := json.Unmarshal(data, &decoded); err != nil {
		return h, true, err
	}
	if decoded == nil {
		return h, true, errors.New("the file holds no records")
	}
	if decoded.Results != nil {
		h.Results = decoded.Results
	}
	if decoded.Fingerprints != nil {
		h.Fingerprints = decoded.Fingerprints
	}
	if decoded.ModelTraces != nil {
		h.ModelTraces = decoded.ModelTraces
	}
	if decoded.Pelicans != nil {
		h.Pelicans = decoded.Pelicans
	}
	return h, true, nil
}

func appendRecent[T any](records []T, r T) []T {
	records = append(records, r)
	return records[max(0, len(records)-historyLimit):]
}

type timedRecord interface{ recorded() time.Time }

func (r CandyResult) recorded() time.Time       { return r.Time }
func (r FingerprintResult) recorded() time.Time { return r.Time }
func (r TraceResult) recorded() time.Time       { return r.Time }
func (r PelicanResult) recorded() time.Time     { return r.Time }

// keepRecent keeps each credential's last historyLimit records made since
// cutoff, and forgets a credential left with none.
func keepRecent[T timedRecord](histories map[string][]T, cutoff time.Time) {
	for id, records := range histories {
		records = records[max(0, len(records)-historyLimit):]
		expired := 0
		for expired < len(records) && records[expired].recorded().Before(cutoff) {
			expired++
		}
		if expired == len(records) {
			delete(histories, id)
			continue
		}
		histories[id] = records[expired:]
	}
}

// pruneLocked drops the records the history no longer keeps. The caller
// holds s.mu.
func (s *Service) pruneLocked() {
	cutoff := s.now().Add(-historyRetention)
	keepRecent(s.hist.Results, cutoff)
	keepRecent(s.hist.Fingerprints, cutoff)
	keepRecent(s.hist.ModelTraces, cutoff)
	keepRecent(s.hist.Pelicans, cutoff)
}

// saveLocked writes every history as one snapshot. Nothing is written while
// another instance owns the data files, or after the file failed to load,
// which keeps that file for inspection. The caller holds s.mu.
func (s *Service) saveLocked() error {
	s.pruneLocked()
	err := s.writeLocked()
	s.storageError = ""
	if err != nil {
		s.storageError = "detection history not saved: " + err.Error()
		return err
	}
	s.dirty = false
	return nil
}

func (s *Service) writeLocked() error {
	if s.loadErr != nil {
		return s.loadErr
	}
	if s.path == "" || !s.canWrite() {
		return nil
	}
	// Pelican pages keep <, > and & literal, as adapt.Manager.save does.
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s.hist); err != nil {
		return err
	}
	if err := persist.AtomicWrite(s.path, data.Bytes()); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

// markDirtyLocked schedules a write of the records a test just added. The
// caller holds s.mu.
func (s *Service) markDirtyLocked() {
	s.dirty = true
	if s.flushArmed || s.stopped {
		return
	}
	s.flushArmed = true
	if s.flushTimer == nil {
		s.flushTimer = time.AfterFunc(historyFlushDelay, s.flushHistory)
	} else {
		s.flushTimer.Reset(historyFlushDelay)
	}
}

// flushHistory is the write markDirtyLocked schedules.
func (s *Service) flushHistory() {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logf("error", fmt.Sprintf("cpa-key-policy: detection history write panicked: %v", recovered))
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Stop or HandOff wrote everything while this waited for the lock.
	if !s.flushArmed {
		return
	}
	s.flushArmed = false
	if s.dirty {
		_ = s.saveLocked()
	}
}

// flushNowLocked writes the history at once and drops a scheduled write. The
// caller holds s.mu.
func (s *Service) flushNowLocked() {
	if s.flushTimer != nil {
		s.flushTimer.Stop()
	}
	s.flushArmed = false
	_ = s.saveLocked()
}

// openHistory loads the service's history and cleans up temp files a crash
// left. A file that cannot be read keeps the service working without saving;
// candy-eval's file, which is only read, is then not imported.
func (s *Service) openHistory(legacy string) {
	h, found, err := loadHistory(s.path)
	if err != nil {
		s.loadErr = fmt.Errorf("cannot read %s, saving is paused to protect it (clear all records to set it aside and resume saving): %w", s.path, err)
		s.storageError = s.loadErr.Error()
		s.logf("warn", "cpa-key-policy: detection history: "+s.loadErr.Error())
		return
	}
	imported := false
	if !found && legacy != "" {
		if old, ok, err := loadHistory(legacy); err != nil {
			s.logf("warn", fmt.Sprintf("cpa-key-policy: detection history: cannot import %s, starting empty: %v", legacy, err))
		} else {
			h, imported = old, ok
		}
	}
	s.hist = h
	s.pruneLocked()
	if s.path != "" && s.canWrite() {
		if err := persist.CleanupStaleTemps(s.path, time.Now()); err != nil {
			s.logf("warn", "cpa-key-policy: detection history: "+err.Error())
		}
	}
	if imported {
		s.logf("info", "cpa-key-policy: imported the detection history of cpa-codex-candy-eval from "+legacy)
		s.dirty = true
		_ = s.saveLocked()
	}
}

// setAsideUnreadableLocked renames the history file that failed to load, so
// that saving resumes; the file stays for inspection. The caller holds s.mu.
func (s *Service) setAsideUnreadableLocked() error {
	aside := s.path + ".corrupt-" + s.now().UTC().Format("20060102T150405Z")
	if err := os.Rename(s.path, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.logf("warn", "cpa-key-policy: detection history: moved the unreadable "+s.path+" to "+aside)
	s.loadErr = nil
	return nil
}
