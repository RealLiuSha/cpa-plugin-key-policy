package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendRotateAndQueryNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := New(path, 180, 3)
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 12; index++ {
		keyID := "a"
		if index%2 == 0 {
			keyID = "b"
		}
		if err := log.Append(Event{TS: base.Add(time.Duration(index) * time.Minute), Actor: "management-api", Action: "update_key", KeyID: keyID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("rotation exceeded configured backup count: %v", err)
	}
	events, err := log.Query("a", "", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("filtered events = %d, want 4", len(events))
	}
	for index := 1; index < len(events); index++ {
		if events[index].TS.After(events[index-1].TS) {
			t.Fatalf("events not newest first: %+v", events)
		}
	}
}

// The limit counts matching events, so a rare action is found behind a burst
// of others, across rotated files.
func TestQueryFiltersByActionBeforeTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := New(path, 400, 3)
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	actions := []string{"adjust_balance", "update_key", "update_key", "update_key", "update_key", "update_key", "update_key", "update_key"}
	for index, action := range actions {
		if err := log.Append(Event{TS: base.Add(time.Duration(index) * time.Minute), Actor: "management-api", Action: action, KeyID: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation backup missing: %v", err)
	}
	events, err := log.Query("", "adjust_balance", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "adjust_balance" || !events[0].TS.Equal(base) {
		t.Fatalf("action filter = %+v, want the one adjust_balance event", events)
	}
	if events, _ := log.Query("b", "adjust_balance", 2); len(events) != 0 {
		t.Fatalf("key and action filters combine: %+v", events)
	}
}

func TestAppendFailureIsReturned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(path, 1<<62, 1).Append(Event{Action: "create_key"}); err == nil {
		t.Fatal("append to directory unexpectedly succeeded")
	}
}

// Query reads newest first and stops once limit lines matched, keeping each
// file's newest matches; an older file is not opened before then.
func TestQueryStopsAtTheFilesThatHoldTheNewestEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := New(path, 200, 3)
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 7; index++ {
		if err := log.Append(Event{TS: base.Add(time.Duration(index) * time.Minute), Action: "update_key", KeyID: fmt.Sprintf("k%d", index)}); err != nil {
			t.Fatal(err)
		}
	}
	// Two events a file: current {k6}, .1 {k4 k5}, .2 {k2 k3}, .3 {k0 k1}.
	// Reading .3 now fails, so a query that reaches it reports an error.
	if err := os.Remove(path + ".3"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".3", 0o700); err != nil {
		t.Fatal(err)
	}
	keys := func(events []Event) string {
		ids := make([]string, 0, len(events))
		for _, event := range events {
			ids = append(ids, event.KeyID)
		}
		return strings.Join(ids, ",")
	}
	for limit, want := range map[int]string{1: "k6", 2: "k6,k5", 5: "k6,k5,k4,k3,k2"} {
		if events, err := log.Query("", "", limit); err != nil || keys(events) != want {
			t.Fatalf("limit %d = %s, %v; want %s", limit, keys(events), err, want)
		}
	}
	if events, err := log.Query("k3", "update_key", 10); err == nil {
		t.Fatalf("a filtered query with too few matches did not read the oldest file: %s", keys(events))
	}
}

// A tail cut short by a failed write is ended before the next event, so it
// stays one malformed line that hides no other event, and a line over a
// scanner's 1 MiB cap is still read.
func TestQuerySkipsMalformedLinesAndReadsLongOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := New(path, 1<<62, 1)
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	appendEvent := func(minute int, keyID string, changes map[string]Change) {
		t.Helper()
		if err := log.Append(Event{TS: base.Add(time.Duration(minute) * time.Minute), Action: "update_key", KeyID: keyID, Changes: changes}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, "a<b", nil)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"ts":"2026-08-08T00:01:00Z","act`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	appendEvent(2, "a<b", nil) // starts a line of its own after the torn one
	appendEvent(3, "long", map[string]Change{"name": {From: strings.Repeat("x", 2<<20), To: "y"}})
	appendEvent(4, "a<b", nil)
	events, err := log.Query("", "", 10)
	if err != nil || len(events) != 4 || events[0].KeyID != "a<b" || events[1].KeyID != "long" || events[1].Changes["name"].To != "y" || !events[2].TS.Equal(base.Add(2*time.Minute)) || !events[3].TS.Equal(base) {
		t.Fatalf("unfiltered = %d events, %v", len(events), err)
	}
	// The torn line is among the newest four but takes none of their places.
	if events, err := log.Query("", "", 4); err != nil || len(events) != 4 || !events[3].TS.Equal(base) {
		t.Fatalf("newest four = %d events, %v", len(events), err)
	}
	// The filter matches the escaped key id Append wrote.
	if events, err := log.Query("a<b", "update_key", 10); err != nil || len(events) != 3 || !events[0].TS.Equal(base.Add(4*time.Minute)) || !events[1].TS.Equal(base.Add(2*time.Minute)) || !events[2].TS.Equal(base) {
		t.Fatalf("filtered = %+v, %v", events, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Count(string(raw), "\n") != 5 || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("file has %d lines, %v; want the torn line ended and one line per event", strings.Count(string(raw), "\n"), err)
	}
}
