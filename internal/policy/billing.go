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
	target := model.Targets[0]
	for _, candidate := range model.Targets {
		if strings.EqualFold(candidate.TargetModel, targetModel) {
			target = candidate
			break
		}
	}
	route := resolveModelRoute(model, target)
	_, usageLedger := s.runtimeComponents()

	if route.BillingMode == "per_call" {
		if failed {
			return 0
		}
		if consumePrecharge && s.consumePrecharge(key.ID, publicModel) {
			return 0
		}
		cost := route.PerCallUSD
		if usageLedger != nil {
			usageLedger.RecordCost(key.ID, publicModel, cost, 0, 0, 0, 0, 0, 0, 1)
		}
		return cost
	}

	if detail.InputTokens <= 0 && detail.OutputTokens <= 0 && detail.CachedTokens <= 0 && detail.CacheReadTokens <= 0 && detail.CacheCreationTokens <= 0 {
		return 0
	}
	breakdown := ComputeCacheCostBreakdown(
		route.Provider,
		route.InputPricePerMillion,
		route.OutputPricePerMillion,
		route.CacheReadPricePerMillion,
		route.CacheWritePricePerMillion,
		true,
		detail,
	)
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

func (s *Store) consumePrecharge(keyID, model string) bool {
	key := prechargeKey(keyID, model)
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
	// TRADEOFF: FIFO key-model matching can pair concurrent requests imprecisely, revisit when usage.handle exposes a stable request id
	queue = queue[1:]
	if len(queue) == 0 {
		delete(s.precharges, key)
	} else {
		s.precharges[key] = queue
	}
	return true
}
