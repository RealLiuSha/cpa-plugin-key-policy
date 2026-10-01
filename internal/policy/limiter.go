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

// Allow counts one request in the key's one-minute window, which starts at the
// first request after the previous window ended. A denied request reports
// when the current window ends.
func (l *RateLimiter) Allow(id string, rpm int) (bool, time.Time) {
	if rpm <= 0 {
		return true, time.Time{}
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
		return false, bucket.windowStart.Add(time.Minute)
	}
	bucket.count++
	l.buckets[id] = bucket
	return true, time.Time{}
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
