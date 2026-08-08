package audit

import (
	"os"
	"path/filepath"
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
	events, err := log.Query("a", 4)
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

func TestAppendFailureIsReturned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(path, 1024, 1).Append(Event{Action: "create_key"}); err == nil {
		t.Fatal("append to directory unexpectedly succeeded")
	}
}
