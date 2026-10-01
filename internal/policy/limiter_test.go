package policy

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiterWithClock(func() time.Time { return now })
	allow := func() bool { allowed, _ := limiter.Allow("team-a", 2); return allowed }
	if !allow() || !allow() {
		t.Fatal("requests within the limit were denied")
	}
	now = now.Add(20 * time.Second)
	if allowed, retryAt := limiter.Allow("team-a", 2); allowed || !retryAt.Equal(now.Add(40*time.Second)) {
		t.Fatalf("third request = %v, retry at %v; want denied until the window ends", allowed, retryAt)
	}
	now = now.Add(40 * time.Second)
	if !allow() {
		t.Fatal("request after window denied")
	}
}

func TestRateLimiterClockRollbackStartsNewWindow(t *testing.T) {
	now := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiterWithClock(func() time.Time { return now })
	allow := func() bool { allowed, _ := limiter.Allow("team-a", 1); return allowed }
	if !allow() {
		t.Fatal("first request denied")
	}
	now = now.Add(1500 * time.Millisecond)
	if allow() {
		t.Fatal("second request allowed")
	}
	now = now.Add(-time.Hour)
	if !allow() {
		t.Fatal("clock rollback did not rebuild window")
	}
}
