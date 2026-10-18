package policy

import (
	"fmt"
	"time"
)

// Bucket is the aggregate for one key, model and natural day (in history) or
// one key, model and quota cycle. Every counter only grows; the billing
// entry sanitizes records so negative values never reach a bucket.
type Bucket struct {
	Calls         int64 `json:"calls,omitempty"`
	UnpricedCalls int64 `json:"unpriced_calls,omitempty"`
	// InputTokens are the non-cached input tokens billed at the input price;
	// OutputTokens include reasoning tokens that were billed as output.
	InputTokens      int64 `json:"input_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	ChargedNano      Nano  `json:"charged_nano,omitempty"`
	// The cache amounts are the multiplied amounts actually charged for cache
	// reads and writes; they are part of ChargedNano and shown separately.
	CacheReadCostNano  Nano `json:"cache_read_cost_nano,omitempty"`
	CacheWriteCostNano Nano `json:"cache_write_cost_nano,omitempty"`
}

func (b *Bucket) add(src Bucket) {
	b.Calls += src.Calls
	b.UnpricedCalls += src.UnpricedCalls
	b.InputTokens += src.InputTokens
	b.CacheReadTokens += src.CacheReadTokens
	b.CacheWriteTokens += src.CacheWriteTokens
	b.OutputTokens += src.OutputTokens
	b.ChargedNano += src.ChargedNano
	b.CacheReadCostNano += src.CacheReadCostNano
	b.CacheWriteCostNano += src.CacheWriteCostNano
}

func (b Bucket) validate() error {
	if b.Calls < 0 || b.UnpricedCalls < 0 || b.InputTokens < 0 || b.CacheReadTokens < 0 || b.CacheWriteTokens < 0 || b.OutputTokens < 0 ||
		b.ChargedNano < 0 || b.CacheReadCostNano < 0 || b.CacheWriteCostNano < 0 {
		return fmt.Errorf("usage counters must be nonnegative")
	}
	return nil
}

// UsageTotals is a Bucket as the management API reports it: dollars as
// float64, zero counters omitted, total_usd always present.
type UsageTotals struct {
	TotalUSD         float64 `json:"total_usd"`
	CallCount        int64   `json:"call_count,omitempty"`
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheCostUSD     float64 `json:"cache_cost_usd,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CacheWriteUSD    float64 `json:"cache_write_usd,omitempty"`
	UnpricedCalls    int64   `json:"unpriced_calls,omitempty"`
}

func (b Bucket) totals() UsageTotals {
	return UsageTotals{
		TotalUSD:         b.ChargedNano.USD(),
		CallCount:        b.Calls,
		InputTokens:      b.InputTokens,
		OutputTokens:     b.OutputTokens,
		CacheReadTokens:  b.CacheReadTokens,
		CacheCostUSD:     b.CacheReadCostNano.USD(),
		CacheWriteTokens: b.CacheWriteTokens,
		CacheWriteUSD:    b.CacheWriteCostNano.USD(),
		UnpricedCalls:    b.UnpricedCalls,
	}
}

// UsageWindow is UsageTotals over one quota cycle, with the cycle start.
type UsageWindow struct {
	UsageTotals
	WindowStart time.Time `json:"window_start,omitzero"`
}
