package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxBytes = int64(10 << 20)
	DefaultBackups  = 3
)

type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

type Event struct {
	TS      time.Time         `json:"ts"`
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	KeyID   string            `json:"key_id,omitempty"`
	Changes map[string]Change `json:"changes,omitempty"`
}

type Log struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
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

	l.mu.Lock()
	defer l.mu.Unlock()
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
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
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

func (l *Log) Query(keyID string, limit int) ([]Event, error) {
	if l == nil {
		return []Event{}, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	paths := []string{l.path}
	for index := 1; index <= l.backups; index++ {
		paths = append(paths, fmt.Sprintf("%s.%d", l.path, index))
	}
	events := make([]Event, 0, limit)
	for _, path := range paths {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("decode audit event in %s: %w", path, err)
			}
			if keyID == "" || event.KeyID == keyID {
				events = append(events, event)
			}
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.After(events[j].TS) })
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}
