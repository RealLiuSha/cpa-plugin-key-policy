package policy

import (
	"strings"
	"time"
)

const (
	prechargeTTL      = 2 * time.Minute
	prechargeMaxQueue = 1024
)

func (s *Store) RecordUsage(apiKeyOrID, requestedModel, targetModel string, failed bool, detail UsageDetail) float64 {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return s.recordUsage(apiKeyOrID, requestedModel, targetModel, failed, detail, true)
}

func (s *Store) recordUsage(apiKeyOrID, requestedModel, targetModel string, failed bool, detail UsageDetail, consumePrecharge bool) float64 {
	if !s.Enabled() {
		return 0
	}
	key := s.findByID(apiKeyOrID)
	if key == nil || !key.Enabled {
		key = s.findBySecret(apiKeyOrID)
	}
	if key == nil || !key.Enabled {
		return 0
	}
	publicModel := strings.TrimSpace(requestedModel)
	if publicModel == "" {
		publicModel = strings.TrimSpace(targetModel)
	}
	model, ok := s.modelForKey(key, publicModel)
	if !ok {
		return 0
	}
	publicModel = model.Name
	_, usageLedger := s.runtimeComponents()

	if model.BillingMode == "per_call" {
		var prepaidAt time.Time
		prepaid := false
		if consumePrecharge {
			prepaidAt, prepaid = s.consumePrecharge(key.ID, publicModel)
		}
		if failed {
			// A failed generation delivered nothing, so its precharge is returned.
			if prepaid && usageLedger != nil {
				usageLedger.ReturnCost(key.ID, publicModel, model.PerCallUSD, prepaidAt)
			}
			return 0
		}
		if prepaid {
			return 0
		}
		cost := model.PerCallUSD
		if usageLedger != nil {
			usageLedger.RecordCost(key.ID, publicModel, cost, 0, 0, 0, 0, 0, 0, 1)
		}
		return cost
	}

	if detail.InputTokens <= 0 && detail.OutputTokens <= 0 && detail.CachedTokens <= 0 && detail.CacheReadTokens <= 0 && detail.CacheCreationTokens <= 0 {
		return 0
	}
	breakdown := ComputeCacheCostBreakdown(
		model.Provider,
		model.InputPricePerMillion,
		model.OutputPricePerMillion,
		model.CacheReadPricePerMillion,
		model.CacheWritePricePerMillion,
		true,
		detail,
	)
	breakdown.TotalCost *= model.BillingMultiplier
	breakdown.CacheReadCost *= model.BillingMultiplier
	breakdown.CacheWriteCost *= model.BillingMultiplier
	if usageLedger != nil {
		usageLedger.RecordCost(key.ID, publicModel, breakdown.TotalCost, breakdown.CacheReadCost, breakdown.CacheReadTokens, breakdown.CacheWriteCost, breakdown.CacheWriteTokens, breakdown.InputTokens, detail.OutputTokens, 1)
	}
	return breakdown.TotalCost
}

func prechargeKey(keyID, model string) string {
	return strings.ToLower(strings.TrimSpace(keyID)) + "\x00" + strings.ToLower(strings.TrimSpace(model))
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

func (s *Store) rememberPrecharge(keyID, model string) {
	key := prechargeKey(keyID, model)
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
	// TRADEOFF: Retain at most 1024 unmatched precharges per key and model, revisit if legitimate two-minute concurrency approaches this ceiling.
	if len(queue) > prechargeMaxQueue {
		queue = queue[len(queue)-prechargeMaxQueue:]
	}
	s.precharges[key] = queue
	s.mu.Unlock()
}

// consumePrecharge takes the oldest live precharge for the key and model and
// reports when it was charged.
func (s *Store) consumePrecharge(keyID, model string) (time.Time, bool) {
	key := prechargeKey(keyID, model)
	if key == "\x00" {
		return time.Time{}, false
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
		return time.Time{}, false
	}
	// TRADEOFF: FIFO key-model matching can pair concurrent requests imprecisely, revisit when usage.handle exposes a stable request id
	chargedAt := queue[0]
	queue = queue[1:]
	if len(queue) == 0 {
		delete(s.precharges, key)
	} else {
		s.precharges[key] = queue
	}
	return chargedAt, true
}
