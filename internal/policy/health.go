package policy

import (
	"sync"
	"sync/atomic"
	"time"
)

// Health counts what the billing entry declined and how persistence is
// doing. Counters live in memory only and start at zero on every start.
type Health struct {
	mu             sync.Mutex
	lastFlushAt    time.Time
	lastFlushError string

	flushFailures     atomic.Int64
	skippedFailed     atomic.Int64
	ignoredUnknownKey atomic.Int64
	ignoredNoModel    atomic.Int64
	ignoredEmptyUsage atomic.Int64
	clampedTokens     atomic.Int64
	invalidUsage      atomic.Int64
	unmeteredPerCall  atomic.Int64
	unpricedCalls     atomic.Int64
}

// HealthSnapshot is the "health" object of GET /status.
type HealthSnapshot struct {
	LastFlushAt       string `json:"last_flush_at"`
	LastFlushError    string `json:"last_flush_error"`
	FlushFailures     int64  `json:"flush_failures"`
	SkippedFailed     int64  `json:"skipped_failed"`
	IgnoredUnknownKey int64  `json:"ignored_unknown_key"`
	IgnoredNoModel    int64  `json:"ignored_no_model"`
	IgnoredEmptyUsage int64  `json:"ignored_empty_usage"`
	ClampedTokens     int64  `json:"clamped_tokens"`
	InvalidUsage      int64  `json:"invalid_usage"`
	UnmeteredPerCall  int64  `json:"unmetered_per_call"`
	UnpricedCalls     int64  `json:"unpriced_calls"`
}

func (h *Health) flushSucceeded(at time.Time) {
	h.mu.Lock()
	h.lastFlushAt = at
	h.lastFlushError = ""
	h.mu.Unlock()
}

func (h *Health) flushFailed(err error) {
	h.flushFailures.Add(1)
	h.mu.Lock()
	h.lastFlushError = err.Error()
	h.mu.Unlock()
}

func (h *Health) Snapshot() HealthSnapshot {
	h.mu.Lock()
	lastFlushAt, lastFlushError := h.lastFlushAt, h.lastFlushError
	h.mu.Unlock()
	snapshot := HealthSnapshot{
		LastFlushError:    lastFlushError,
		FlushFailures:     h.flushFailures.Load(),
		SkippedFailed:     h.skippedFailed.Load(),
		IgnoredUnknownKey: h.ignoredUnknownKey.Load(),
		IgnoredNoModel:    h.ignoredNoModel.Load(),
		IgnoredEmptyUsage: h.ignoredEmptyUsage.Load(),
		ClampedTokens:     h.clampedTokens.Load(),
		InvalidUsage:      h.invalidUsage.Load(),
		UnmeteredPerCall:  h.unmeteredPerCall.Load(),
		UnpricedCalls:     h.unpricedCalls.Load(),
	}
	if !lastFlushAt.IsZero() {
		snapshot.LastFlushAt = lastFlushAt.UTC().Format(time.RFC3339)
	}
	return snapshot
}
