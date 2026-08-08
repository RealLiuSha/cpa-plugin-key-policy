package policy

import (
	"strings"
	"time"
)

const (
	prechargeTTL      = 2 * time.Minute
	prechargeMaxQueue = 1024
)

// RecordUsage bills a finalized usage record delivered by the host via the
// usage.handle plugin call. CPA parses the token counts itself (including the
// final usage frame of a streaming response) before invoking us, so we receive
// ready-made Input/Output token counts rather than a body to parse. This is
// the billing entry point that covers streaming responses — the host never
// invokes response.intercept_after on the streaming path. Best-effort: unknown
// keys or aliases cost nothing.
//
// failed reports whether the upstream request failed (non-2xx). Per-call
// billing only charges on success (failed=false); token billing is implicitly
// zero on failure (no tokens reported). Failed requests never increment
// CallCount.
//
// key resolution: the host's UsageRecord.APIKey is NOT the client's plaintext
// secret — CPA stores our auth result's Principal (set to key.ID) into the
// request context as "userApiKey" and forwards that. So we match by key.ID
// first, then fall back to a plaintext-secret match for forward compatibility
// (in case a future CPA build forwards the raw secret).
//
// alias resolution: prefer the client-requested Alias (what the caller put in
// the request body's "model" field); fall back to the resolved upstream Model.
func (s *Store) RecordUsage(apiKeyOrID, alias, model string, failed bool, detail UsageDetail) float64 {
	return s.recordUsage(apiKeyOrID, alias, model, failed, detail, true)
}

func (s *Store) recordUsage(apiKeyOrID, alias, model string, failed bool, detail UsageDetail, consumePrecharge bool) float64 {
	if !s.Enabled() {
		return 0
	}
	// Match by ID first (the documented wire value), then by plaintext secret.
	key := s.findByID(apiKeyOrID)
	if key == nil || !key.Enabled {
		key = s.findBySecret(apiKeyOrID)
	}
	if key == nil || !key.Enabled {
		return 0
	}
	_, usageLedger := s.runtimeComponents()
	// Resolve the alias to price against. Prefer the client-requested alias
	// (matches what the user configured prices for); fall back to the upstream
	// model id, which equals the alias for this plugin (alias == target_model).
	resolved := strings.TrimSpace(alias)
	if resolved == "" {
		resolved = strings.TrimSpace(model)
	}
	if resolved == "" {
		return 0
	}
	// Canonicalize to the configured rule.Alias spelling so pure case variants
	// share one ByAlias bucket. Unknown aliases keep existing zero-cost /
	// reject-at-auth semantics and must not create forged empty ledger rows.
	rule, ok := key.ModelForAlias(resolved)
	if !ok {
		return 0
	}
	resolved = rule.Alias

	// Per-call billing: a fixed USD charge per SUCCESSFUL request, independent
	// of token counts. Failed requests are not charged and don't count. A
	// PerCallUSD of 0 is allowed (free calls); CallCount still increments so the
	// UI can report call volume. The token-price fields on the rule are dormant
	// under this mode.
	if strings.EqualFold(rule.BillingMode, "per_call") {
		if failed {
			return 0
		}
		if consumePrecharge && s.consumePrecharge(key.ID, resolved) {
			return 0
		}
		cost := rule.PerCallUSD
		if cost < 0 {
			cost = 0
		}
		if usageLedger != nil {
			// callCount=1 regardless of cost (even free calls count toward volume).
			usageLedger.RecordCost(key.ID, resolved, cost, 0, 0, 0, 0, 1)
		}
		return cost
	}

	usageFound := detail.InputTokens > 0 || detail.OutputTokens > 0
	if !usageFound {
		return 0
	}
	// Cache-aware billing: the usage.handle detail carries cache-read / cached
	// token counts. We price cache-hit input tokens at the alias's cache-read
	// price (falling back to the input price when none is configured), with
	// provider-specific semantics for whether cache hits sit inside or outside
	// InputTokens. The owning rule's provider selects the semantics.
	provider := rule.Provider
	inputPerMillion, outputPerMillion, cacheReadPerMillion, priced := key.PriceForAlias(resolved)
	cost, cacheCost, cacheReadTokens := ComputeCacheCostBreakdown(provider, inputPerMillion, outputPerMillion, cacheReadPerMillion, priced, detail)
	// Non-cache input tokens billed at the input price — the denominator partner
	// for hit-rate = cacheRead / (cacheRead + input). Must mirror the biller's
	// internal split so the reported rate matches the actual pricing.
	var nonCacheInput int64
	if priced && (detail.InputTokens > 0 || detail.OutputTokens > 0) {
		if isCacheAdditiveProvider(provider) {
			nonCacheInput = detail.InputTokens + detail.CacheCreationTokens
		} else {
			cr := detail.CacheReadTokens
			if cr == 0 {
				cr = detail.CachedTokens
			}
			if cr > detail.InputTokens {
				cr = detail.InputTokens
			}
			nonCacheInput = detail.InputTokens - cr
		}
	}
	if priced && usageFound && usageLedger != nil {
		// Record even when cost == 0 (priced-but-free alias: all token prices 0).
		// Token (input/output/cache) + call counters must advance so the UI
		// reports usage volume and hit-rate; USD stays 0. Previously `cost > 0`
		// dropped free-but-priced requests entirely, hiding their volume.
		// callCount=1: this was a successful, token-billed request.
		usageLedger.RecordCost(key.ID, resolved, cost, cacheCost, cacheReadTokens, nonCacheInput, int64(detail.OutputTokens), 1)
	}
	return cost
}

func prechargeKey(keyID, alias string) string {
	return strings.ToLower(strings.TrimSpace(keyID)) + "\x00" + strings.ToLower(strings.TrimSpace(alias))
}

func (s *Store) billingNow() time.Time {
	s.mu.RLock()
	ledger := s.usage
	s.mu.RUnlock()
	if ledger != nil {
		return ledger.now()
	}
	return time.Now()
}

func (s *Store) rememberPrecharge(keyID, alias string) {
	key := prechargeKey(keyID, alias)
	if key == "\x00" {
		return
	}
	now := s.billingNow()
	s.mu.Lock()
	queue := s.precharges[key]
	cutoff := now.Add(-prechargeTTL)
	first := 0
	for first < len(queue) && queue[first].Before(cutoff) {
		first++
	}
	queue = append(queue[first:], now)
	// TRADEOFF: Retain at most 1024 unmatched precharges per key and alias, revisit if legitimate two-minute concurrency approaches this ceiling.
	if len(queue) > prechargeMaxQueue {
		queue = queue[len(queue)-prechargeMaxQueue:]
	}
	s.precharges[key] = queue
	s.mu.Unlock()
}

func (s *Store) consumePrecharge(keyID, alias string) bool {
	key := prechargeKey(keyID, alias)
	if key == "\x00" {
		return false
	}
	now := s.billingNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.precharges[key]
	cutoff := now.Add(-prechargeTTL)
	first := 0
	for first < len(queue) && queue[first].Before(cutoff) {
		first++
	}
	queue = queue[first:]
	if len(queue) == 0 {
		delete(s.precharges, key)
		return false
	}
	// TRADEOFF: FIFO key-alias matching can pair concurrent requests imprecisely, revisit when usage.handle exposes a stable request id
	queue = queue[1:]
	if len(queue) == 0 {
		delete(s.precharges, key)
	} else {
		s.precharges[key] = queue
	}
	return true
}
