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
//   - Additive providers (Anthropic/Claude): cache-read and cache-creation
//     tokens are reported OUTSIDE InputTokens. Reads bill at cache-read, writes
//     at cache-write, and remaining input at the input price.
//   - Subset providers (OpenAI/Gemini/Codex/...): cache-hit tokens are already
//     INSIDE InputTokens, so we split them out: (InputTokens - cacheHits) at the
//     input price + cacheHits at the cache-read price. Explicit cache-write
//     prices also peel cache-creation tokens out of the remaining input.
//
// A zero cache-read price means unconfigured and falls back to the regular
// input price. Cache-write uses presence semantics: nil falls back to input,
// while an explicit zero is a configured free cache-write price. priced=false
// or no usable tokens → 0.
//
// cacheReadTokensOut reports the number of cache-hit input tokens billed at the
// cache price for THIS record (for the ledger's hit-rate / cache-cost tracking).
// It is the same CacheRead value used inside the cost formula (after clamping
// for subset providers); 0 when the record had no cache hits or was unpriced.
type CacheCostBreakdown struct {
	TotalCost        float64
	CacheReadCost    float64
	CacheReadTokens  int64
	CacheWriteCost   float64
	CacheWriteTokens int64
	InputTokens      int64
}

func ComputeCacheCost(provider string, inputPerMillion, outputPerMillion, cacheReadPerMillion float64, cacheWritePerMillion *float64, priced bool, detail UsageDetail) float64 {
	return ComputeCacheCostBreakdown(provider, inputPerMillion, outputPerMillion, cacheReadPerMillion, cacheWritePerMillion, priced, detail).TotalCost
}

func ComputeCacheCostBreakdown(provider string, inputPerMillion, outputPerMillion, cacheReadPerMillion float64, cacheWritePerMillion *float64, priced bool, detail UsageDetail) CacheCostBreakdown {
	if !priced {
		return CacheCostBreakdown{}
	}
	input := detail.InputTokens
	output := detail.OutputTokens
	if input <= 0 && output <= 0 && detail.CachedTokens <= 0 && detail.CacheReadTokens <= 0 && detail.CacheCreationTokens <= 0 {
		return CacheCostBreakdown{}
	}
	cacheRead := detail.CacheReadTokens
	if cacheRead == 0 {
		// OpenAI/Gemini/Codex report cache hits as CachedTokens (a subset of
		// input) without a separate CacheRead field; Claude sets CachedTokens =
		// CacheReadTokens. Either way CachedTokens is the cache-hit count.
		cacheRead = detail.CachedTokens
	}
	cacheReadPrice := cacheReadPerMillion
	if cacheReadPrice == 0 {
		cacheReadPrice = inputPerMillion
	}
	cacheWritePrice := optionalPriceValue(cacheWritePerMillion)
	if cacheWritePerMillion == nil {
		cacheWritePrice = inputPerMillion
	}

	var inputTokensToBill int64
	var cacheWriteTokens int64
	if isCacheAdditiveProvider(provider) {
		inputTokensToBill = input
		if cacheWritePerMillion == nil {
			inputTokensToBill += detail.CacheCreationTokens
		} else {
			cacheWriteTokens = detail.CacheCreationTokens
		}
	} else {
		if cacheRead > input {
			cacheRead = input
		}
		inputTokensToBill = input - cacheRead
		if cacheWritePerMillion != nil {
			cacheWriteTokens = detail.CacheCreationTokens
			if cacheWriteTokens > inputTokensToBill {
				cacheWriteTokens = inputTokensToBill
			}
			inputTokensToBill -= cacheWriteTokens
		}
	}

	breakdown := CacheCostBreakdown{
		TotalCost: float64(inputTokensToBill)/1_000_000*inputPerMillion +
			float64(cacheRead)/1_000_000*cacheReadPrice +
			float64(cacheWriteTokens)/1_000_000*cacheWritePrice +
			float64(output)/1_000_000*outputPerMillion,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWriteTokens,
		InputTokens:      inputTokensToBill,
	}
	if cacheReadPerMillion != 0 && cacheRead > 0 {
		breakdown.CacheReadCost = float64(cacheRead) / 1_000_000 * cacheReadPrice
	}
	if cacheWritePerMillion != nil && cacheWriteTokens > 0 {
		breakdown.CacheWriteCost = float64(cacheWriteTokens) / 1_000_000 * cacheWritePrice
	}
	return breakdown
}
