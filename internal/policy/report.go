package policy

import "sort"

func (s *Store) Status() map[string]any {
	s.mu.RLock()
	enabled, statePath, datasetID := s.enabled, s.statePath, s.datasetID
	keyCount, modelCount := len(s.keys), len(s.prices.rows)
	limiter, health := s.limiter, s.health
	s.mu.RUnlock()
	return map[string]any{
		"enabled": enabled, "state_file": statePath, "dataset_id": datasetID,
		"key_count": keyCount, "model_count": modelCount,
		"rpm_usage": limiter.Snapshot(), "health": health.Snapshot(),
	}
}

func (s *Store) Health() HealthSnapshot {
	s.mu.RLock()
	health := s.health
	s.mu.RUnlock()
	return health.Snapshot()
}

func (s *Store) Timezone() string {
	_, ledger := s.runtimeComponents()
	return ledger.timezone
}

// PriceRows lists the price table with each row's priced flag.
func (s *Store) PriceRows() []PriceRow {
	s.mu.RLock()
	rows := s.prices.snapshot()
	s.mu.RUnlock()
	result := make([]PriceRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, PriceRow{ModelPrice: row, Priced: row.Priced()})
	}
	return result
}

// PriceRowFor returns the row stored for (name, provider), the way the
// management API lists it. The provider qualifier is matched as UpsertModel
// stored it, so a caller need not know the normalization.
func (s *Store) PriceRowFor(name, provider string) (PriceRow, bool) {
	s.mu.RLock()
	prices := s.prices
	s.mu.RUnlock()
	row, ok := prices.row(name, provider)
	if !ok {
		return PriceRow{}, false
	}
	return PriceRow{ModelPrice: row, Priced: row.Priced()}, true
}

// Two notions of "priced" exist and each has one owner. PriceRow.Priced is
// the price table's: this row charges something. The priced flag on a usage
// report row (ModelUsageEntry, StatsModel) and the unpriced lists are the
// ledger's: whether the calls shown were actually charged, which a
// provider-qualified row may not have done for calls from other providers.

// UnpricedModel is a model with calls that were billed without a price in
// the reported window.
type UnpricedModel struct {
	Name     string `json:"name"`
	Calls    int64  `json:"calls"`
	LastSeen string `json:"last_seen"`
	// HasQualifiedRows means price rows exist for the id, but only with a
	// provider qualifier, so calls from other providers missed them. Buckets
	// carry no provider, so which provider missed is not known.
	HasQualifiedRows bool `json:"has_qualified_rows,omitempty"`
}

// UnpricedModels lists the models with unpriced calls in the retention
// window, most calls first. A model with an unqualified row is left out even
// though an all-zero row keeps producing unpriced calls: that row is on the
// Models page with its own unpriced badge, and this list exists to surface
// models that have no row to badge.
func (s *Store) UnpricedModels() []UnpricedModel {
	_, ledger := s.runtimeComponents()
	from, to := ledger.reportRange(UsageRetentionDays)
	return s.unpricedModels(ledger, from, to)
}

func (s *Store) unpricedModels(ledger *usageLedger, from, to string) []UnpricedModel {
	seen := make(map[string]*UnpricedModel)
	ledger.visitRange(from, to, func(_, date, model string, bucket Bucket) {
		if bucket.UnpricedCalls == 0 {
			return
		}
		entry := seen[model]
		if entry == nil {
			entry = &UnpricedModel{Name: model}
			seen[model] = entry
		}
		entry.Calls += bucket.UnpricedCalls
		if date > entry.LastSeen {
			entry.LastSeen = date
		}
	})
	s.mu.RLock()
	prices := s.prices
	s.mu.RUnlock()
	result := make([]UnpricedModel, 0, len(seen))
	for _, entry := range seen {
		if _, unqualified := prices.row(entry.Name, ""); unqualified {
			continue
		}
		entry.HasQualifiedRows = prices.hasName(entry.Name)
		result = append(result, *entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Calls != result[j].Calls {
			return result[i].Calls > result[j].Calls
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (s *Store) UsageSummaryFor(key KeyConfig) UsageSummary {
	_, ledger := s.runtimeComponents()
	summary := ledger.summary(key.ID, quotaLimitsForKey(key))
	if !key.Enabled {
		summary.Status = "disabled"
	}
	return summary
}

// ModelUsageEntry is one model's consumption in a key's current periods,
// with its price row's billing settings when one exists.
type ModelUsageEntry struct {
	Name              string  `json:"name"`
	BillingMode       string  `json:"billing_mode,omitempty"`
	BillingMultiplier float64 `json:"billing_multiplier,omitempty"`
	// Priced is false when any call in the three periods was billed without
	// a price (see the note above UnpricedModel).
	Priced  bool        `json:"priced"`
	Daily   UsageWindow `json:"daily"`
	Weekly  UsageWindow `json:"weekly"`
	Monthly UsageWindow `json:"monthly"`
}

// ModelUsageFor lists the models that appear in the key's current periods,
// sorted by name. Buckets do not record the provider, so each model's
// billing settings come from the row rowByName picks.
func (s *Store) ModelUsageFor(keyID string) (KeyConfig, []ModelUsageEntry, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	_, ledger := s.runtimeComponents()
	windows := ledger.modelUsage(key.ID)
	s.mu.RLock()
	prices := s.prices
	s.mu.RUnlock()
	rows := make([]ModelUsageEntry, 0, len(windows))
	for _, window := range windows {
		entry := ModelUsageEntry{Name: window.Name, Daily: window.Daily, Weekly: window.Weekly, Monthly: window.Monthly}
		price, ok := prices.rowByName(window.Name)
		if ok {
			entry.BillingMode, entry.BillingMultiplier = price.BillingMode, price.BillingMultiplier
		}
		calls := window.Daily.CallCount + window.Weekly.CallCount + window.Monthly.CallCount
		unpriced := window.Daily.UnpricedCalls + window.Weekly.UnpricedCalls + window.Monthly.UnpricedCalls
		entry.Priced = pricedCalls(calls, unpriced, ok && price.Priced())
		rows = append(rows, entry)
	}
	return *key, rows, true
}

// pricedCalls is the ledger's priced flag for a set of calls: every call was
// charged. Without calls the flag falls back to the price table's.
func pricedCalls(calls, unpriced int64, rowPriced bool) bool {
	if calls == 0 {
		return rowPriced
	}
	return unpriced == 0
}

// UsageHistoryFor returns the key's last days (1..90, default 30) with a
// computed daily total and the per-model buckets; missing days are zero.
func (s *Store) UsageHistoryFor(keyID string, days int) (KeyConfig, []UsageHistoryDay, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	_, ledger := s.runtimeComponents()
	return *key, ledger.history(key.ID, days), true
}
