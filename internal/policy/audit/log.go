package audit

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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxBytes = int64(10 << 20)
	DefaultBackups  = 3
)

// spareLines widens each file's window past the events still needed, so a
// few malformed lines among the newest, such as a tail cut short by a failed
// write (Append ends it before writing the next event), do not take the
// place of valid events. Without a filter the lines are kept unchecked:
// validating every line of a full file would make the query about ten times
// slower.
const spareLines = 8

type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

type Event struct {
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	KeyID  string    `json:"key_id,omitempty"`
	// Target names what a change outside the key store applied to, such as
	// a channel's auth_index or an adapter rule id.
	Target  string            `json:"target,omitempty"`
	Changes map[string]Change `json:"changes,omitempty"`
}

type Log struct {
	mu              sync.Mutex
	path            string
	maxBytes        int64
	backups         int
	warnedMalformed bool
}

func New(path string, maxBytes int64, backups int) *Log {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if backups < 1 {
		backups = DefaultBackups
	}
	return &Log{path: path, maxBytes: maxBytes, backups: backups}
}

func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *Log) Append(event Event) error {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return errors.New("audit log is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Stamped under the lock, so the order of the files and their lines is
	// the TS order Query relies on; only tests pass a TS of their own.
	if event.TS.IsZero() {
		event.TS = time.Now().UTC()
	}
	if strings.TrimSpace(event.Actor) == "" {
		event.Actor = "management-api"
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(l.path); err == nil && info.Size()+int64(len(raw)) > l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	// A write cut short by ENOSPC or a crash leaves a line without its
	// newline; end it, or this event is glued onto it and lost with it.
	if info, err := file.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := file.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			raw = append([]byte{'\n'}, raw...)
		}
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (l *Log) rotateLocked() error {
	for index := l.backups; index >= 1; index-- {
		from := l.path
		if index > 1 {
			from = fmt.Sprintf("%s.%d", l.path, index-1)
		}
		to := fmt.Sprintf("%s.%d", l.path, index)
		if index == l.backups {
			if err := os.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Query returns the newest events, at most limit, of one key and one action
// when they are set. Files are read newest first and an older one only while
// fewer than limit events matched: Append stamps TS under the lock, so a
// rotated file holds no newer event. Lines that do not decode are skipped;
// only more than spareLines of them among one file's newest lines can leave
// out a valid event of that file.
func (l *Log) Query(keyID, action string, limit int) ([]Event, error) {
	if l == nil {
		return []Event{}, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var needles [][]byte
	if keyID != "" {
		needles = append(needles, fieldNeedle("key_id", keyID))
	}
	if action != "" {
		needles = append(needles, fieldNeedle("action", action))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	paths := []string{l.path}
	for index := 1; index <= l.backups; index++ {
		paths = append(paths, fmt.Sprintf("%s.%d", l.path, index))
	}
	events := make([]Event, 0, limit+spareLines)
	for _, path := range paths {
		if len(events) >= limit {
			break
		}
		lines, err := l.newestMatchesLocked(path, keyID, action, needles, limit-len(events)+spareLines)
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				l.warnMalformedLocked(path, err)
				continue
			}
			events = append(events, event)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.After(events[j].TS) })
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// newestMatchesLocked returns the last limit lines of one file that match
// the filters, oldest first. Without a filter no line is decoded; with one, a
// line is decoded only when it contains every needle.
func (l *Log) newestMatchesLocked(path, keyID, action string, needles [][]byte, limit int) ([][]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ring := make([][]byte, 0, limit)
	next := 0
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		// ReadBytes has no line cap and returns a fresh slice the ring keeps.
		line, readErr := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 && l.matchesLocked(path, line, keyID, action, needles) {
			if len(ring) < limit {
				ring = append(ring, line)
			} else {
				ring[next] = line
				next = (next + 1) % limit
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return slices.Concat(ring[next:], ring[:next]), nil
}

func (l *Log) matchesLocked(path string, line []byte, keyID, action string, needles [][]byte) bool {
	if len(needles) == 0 {
		return true
	}
	for _, needle := range needles {
		if !bytes.Contains(line, needle) {
			return false
		}
	}
	var head struct {
		KeyID  string `json:"key_id"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		l.warnMalformedLocked(path, err)
		return false
	}
	return (keyID == "" || head.KeyID == keyID) && (action == "" || head.Action == action)
}

// fieldNeedle is a top-level string field as Append's json.Marshal writes
// it, so a line without it cannot match the filter on that field.
func fieldNeedle(name, value string) []byte {
	raw, _ := json.Marshal(value)
	return append([]byte(`"`+name+`":`), raw...)
}

// warnMalformedLocked logs the first malformed line once per log: the
// overview queries on every visit and would repeat it until it rotates out.
func (l *Log) warnMalformedLocked(path string, err error) {
	if l.warnedMalformed {
		return
	}
	l.warnedMalformed = true
	log.Printf("cpa-key-policy: skipping malformed audit lines, first in %s: %v", path, err)
}
