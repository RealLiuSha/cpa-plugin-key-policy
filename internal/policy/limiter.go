package policy

import (
	"sync"
	"time"
)

type rateBucket struct {
	windowStart time.Time
	count       int
}

type RateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]rateBucket
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		now:     time.Now,
		buckets: make(map[string]rateBucket),
	}
}

func NewRateLimiterWithClock(now func() time.Time) *RateLimiter {
	limiter := NewRateLimiter()
	if now != nil {
		limiter.now = now
	}
	return limiter
}

func (l *RateLimiter) Allow(id string, rpm int) bool {
	allowed, _ := l.AllowWithRetryAfter(id, rpm)
	return allowed
}

func (l *RateLimiter) AllowWithRetryAfter(id string, rpm int) (bool, int) {
	if rpm <= 0 {
		return true, 0
	}
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.buckets[id]
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= time.Minute || now.Before(bucket.windowStart) {
		bucket = rateBucket{windowStart: now, count: 0}
	}
	if bucket.count >= rpm {
		l.buckets[id] = bucket
		return false, retryAfterSeconds(bucket.windowStart, now)
	}
	bucket.count++
	l.buckets[id] = bucket
	return true, 0
}

func retryAfterSeconds(windowStart, now time.Time) int {
	remaining := time.Minute - now.Sub(windowStart)
	if remaining <= 0 {
		return 1
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (l *RateLimiter) Reset(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, id)
}

func (l *RateLimiter) Snapshot() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.buckets))
	for key, bucket := range l.buckets {
		out[key] = bucket.count
	}
	return out
}
