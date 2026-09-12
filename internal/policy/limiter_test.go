package policy

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiterWithClock(func() time.Time { return now })
	if !limiter.Allow("team-a", 2) {
		t.Fatal("first request denied")
	}
	if !limiter.Allow("team-a", 2) {
		t.Fatal("second request denied")
	}
	if limiter.Allow("team-a", 2) {
		t.Fatal("third request allowed")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("team-a", 2) {
		t.Fatal("request after window denied")
	}
}

func TestRateLimiterRetryAfterRoundsUpRemainingWindow(t *testing.T) {
	now := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiterWithClock(func() time.Time { return now })
	if allowed, _ := limiter.AllowWithRetryAfter("team-a", 1); !allowed {
		t.Fatal("first request denied")
	}
	now = now.Add(1500 * time.Millisecond)
	allowed, retryAfter := limiter.AllowWithRetryAfter("team-a", 1)
	if allowed {
		t.Fatal("second request allowed")
	}
	if retryAfter != 59 {
		t.Fatalf("retry after = %d, want 59", retryAfter)
	}
	now = now.Add(-time.Hour)
	if allowed, retryAfter := limiter.AllowWithRetryAfter("team-a", 1); !allowed || retryAfter != 0 {
		t.Fatalf("clock rollback should rebuild the window, allowed=%v retry=%d", allowed, retryAfter)
	}
}
