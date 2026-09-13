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
	if rpm <= 0 {
		return true
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
		return false
	}
	bucket.count++
	l.buckets[id] = bucket
	return true
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
