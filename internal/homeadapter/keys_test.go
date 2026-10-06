package homeadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestZeroLimitMeansNoLimitInHome(t *testing.T) {
	zero, weekly := Limit(0), Limit(200)
	data, err := json.Marshal(UserWrite{Limit1d: &zero, Limit7d: &weekly})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"limit_1d_credits":null,"limit_7d_credits":200}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestWeeklyResetDayFollowsLocalCalendar(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	// 2026-10-04T16:00Z is Monday 00:00 in Shanghai but still Sunday in UTC.
	resetsAt := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	if got := weekday(resetsAt.In(shanghai)); got != 1 {
		t.Fatalf("got %d, want 1 (Monday)", got)
	}
	if got := weekday(time.Date(2026, 10, 4, 12, 0, 0, 0, shanghai)); got != 7 {
		t.Fatalf("got %d, want 7 (Sunday)", got)
	}
}

func TestSummaryReportsTheExhaustedWindow(t *testing.T) {
	limit := 200.0
	svc := NewService(nil, "kp_", time.UTC, time.Minute)
	e := &entry{user: User{CreditsUnlimited: true}, windows: []PeriodWindow{
		{ID: "1d", Used: 50},
		{ID: "7d", Limit: &limit, Used: 200},
	}}
	got := svc.summary(e)
	if got.Status != "limited" || got.BlockedReason != "weekly_exceeded" || got.WeeklyLimitUSD != 200 || got.DailyLimitUSD != 0 {
		t.Fatalf("unexpected summary: %+v", got)
	}
	e.windows[1].Used = 170
	if got := svc.summary(e); got.Status != "warning" {
		t.Fatalf("status %q, want warning at 85%% of the limit", got.Status)
	}
}

func TestPlaintextIsMatchedByPluginHash(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "ak_store.json")
	if err := os.WriteFile(store, []byte(`{"users":{"alice":{"ak":"sk-test-alice","org":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(lines, []byte("sk-test-bob\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := plaintextByHash([]string{store, lines})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"sk-test-alice", "sk-test-bob"} {
		sum := sha256.Sum256([]byte(value))
		if got := index[hex.EncodeToString(sum[:])]; got != value {
			t.Fatalf("hash of %q maps to %q", value, got)
		}
	}
	if len(index) != 2 {
		t.Fatalf("indexed %d values, want 2", len(index))
	}
}
