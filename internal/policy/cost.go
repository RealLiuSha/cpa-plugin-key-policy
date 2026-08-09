package policy

import "strings"

// isCacheAdditiveProvider reports whether the provider reports cache-hit input
// tokens SEPARATELY from InputTokens (so InputTokens excludes cache reads and
// the two must be summed). Anthropic/Claude is the additive case: CPA parses
// input_tokens, cache_read_input_tokens, cache_creation_input_tokens as
// independent fields and sets TotalTokens = Input + Output + CacheRead +
// CacheCreation (usage_helpers.parseClaudeUsageNode). All other providers CPA
// supports (OpenAI, Gemini, Codex, ...) report cache hits as a SUBSET already
// counted inside InputTokens (prompt_tokens_details.cached_tokens,
// cachedContentTokenCount) — the subset must be split out and repriced rather
// than added.
func isCacheAdditiveProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic", "vertex-claude":
		return true
	default:
		return false
	}
}

// UsageDetail is the token breakdown delivered by the host's usage.handle call,
// already parsed from the upstream response (including the final usage frame of
// a stream). Only the fields we bill on are tracked here.
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// ComputeCacheCost is the cache-aware biller for the usage.handle path. It takes
// the full token detail (with cache breakdown) plus the public model's prices
// and routed provider, and prices cache-hit input tokens at the cache-read
// price instead of the regular input price. Provider semantics:
//
//   - Additive providers (Anthropic/Claude): cache-read tokens are reported
//     OUTSIDE InputTokens, so the cache-read subset is billed at the cache price
//     and InputTokens at the input price, summed. Cache-creation tokens are not
//     covered by the single cache-read price and are billed at the input price
//     (they are fresh prompt tokens that got written to the cache).
//   - Subset providers (OpenAI/Gemini/Codex/...): cache-hit tokens are already
//     INSIDE InputTokens, so we split them out: (InputTokens - cacheHits) at the
//     input price + cacheHits at the cache price, to avoid double-counting.
//
// When cacheReadPerMillion is 0 (not configured), cache hits fall back to the
// regular input price in both cases, preserving prior behavior. priced=false or
// no usable tokens → 0.
//
// cacheReadTokensOut reports the number of cache-hit input tokens billed at the
// cache price for THIS record (for the ledger's hit-rate / cache-cost tracking).
// It is the same CacheRead value used inside the cost formula (after clamping
// for subset providers); 0 when the record had no cache hits or was unpriced.
func ComputeCacheCost(provider string, inputPerMillion, outputPerMillion, cacheReadPerMillion float64, priced bool, detail UsageDetail) float64 {
	total, _, _ := ComputeCacheCostBreakdown(provider, inputPerMillion, outputPerMillion, cacheReadPerMillion, priced, detail)
	return total
}

// ComputeCacheCostBreakdown is the same biller as ComputeCacheCost but also
// returns the cache-hit breakdown used for reporting (cache spend + the cache
// count billed at the cache price). Callers that only need the total should
// call ComputeCacheCost; the ledger calls this to accumulate cache stats.
//
// Returns:
//   - totalCost: the full dollar bill (same as ComputeCacheCost).
//   - cacheCost: the dollar portion attributable to cache-hit input tokens
//     (cacheRead × cachePrice / 1M). When no cache is configured (cacheRead=0)
//     or the model is unpriced, cacheCost is 0 even if cache hits existed
//     (because they were folded into the input-price line, not separably priced).
//   - cacheReadTokens: the cache-hit count billed at the cache price (post-clamp).
func ComputeCacheCostBreakdown(provider string, inputPerMillion, outputPerMillion, cacheReadPerMillion float64, priced bool, detail UsageDetail) (totalCost, cacheCost float64, cacheReadTokens int64) {
	if !priced {
		return 0, 0, 0
	}
	input := detail.InputTokens
	output := detail.OutputTokens
	if input == 0 && output == 0 {
		return 0, 0, 0
	}
	cacheRead := detail.CacheReadTokens
	if cacheRead == 0 {
		// OpenAI/Gemini/Codex report cache hits as CachedTokens (a subset of
		// input) without a separate CacheRead field; Claude sets CachedTokens =
		// CacheReadTokens. Either way CachedTokens is the cache-hit count.
		cacheRead = detail.CachedTokens
	}
	cachePrice := cacheReadPerMillion
	if cachePrice == 0 {
		// No cache price configured: bill everything at the regular input price.
		// For subset providers input already includes cache hits, so this is
		// correct as-is (no double count). For additive providers, cache reads
		// are outside input, so we still add them at the input price to match the
		// pre-cache-pricing total (Input + Output + CacheRead + CacheCreation).
		cachePrice = inputPerMillion
	}

	var inputTokensToBill int64
	if isCacheAdditiveProvider(provider) {
		// Cache hits are NOT in input; bill input at input price, cache reads at
		// the cache price, and cache-creation tokens (writes) at the input price.
		inputTokensToBill = input + detail.CacheCreationTokens
	} else {
		// Cache hits ARE a subset of input; peel them off and reprice.
		if cacheRead > input {
			cacheRead = input // defensive: clamp to what's reported
		}
		inputTokensToBill = input - cacheRead
	}

	cacheReadTokens = cacheRead
	cost := float64(inputTokensToBill)/1_000_000*inputPerMillion +
		float64(cacheRead)/1_000_000*cachePrice +
		float64(output)/1_000_000*outputPerMillion
	// cacheCost is the cache-hit line only. Report it as separably priced only
	// when a cache price was explicitly configured (cacheReadPerMillion != 0);
	// otherwise cache hits were folded into the input-price bill and reporting
	// them as "cache spend" would overstate savings/mislead the dashboards.
	var cachePortion float64
	if cacheReadPerMillion != 0 && cacheRead > 0 {
		cachePortion = float64(cacheRead) / 1_000_000 * cachePrice
	}
	return cost, cachePortion, cacheReadTokens
}
