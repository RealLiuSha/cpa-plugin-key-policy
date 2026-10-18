package policy

import (
	"errors"
	"fmt"
	"strings"

	"cpa-key-policy/internal/policy/audit"
)

var (
	ErrInvalidModelImport      = errors.New("invalid model import")
	ErrInvalidModelPriceImport = errors.New("invalid model price import")
	ErrUnknownModel            = errors.New("model not found")
)

// UpsertModel creates or replaces the price row for (name, provider).
func (s *Store) UpsertModel(input ModelPrice) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	rows := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	input.Name = strings.TrimSpace(input.Name)
	input.Provider = normalizeProvider(input.Provider)
	key := priceKey(input.Name, input.Provider)
	var previous *ModelPrice
	index := -1
	for i := range rows {
		if priceKey(rows[i].Name, rows[i].Provider) == key {
			stored := rows[i].clone()
			previous = &stored
			input.Name = rows[i].Name
			index = i
			break
		}
	}
	if index >= 0 {
		rows[index] = input
	} else {
		rows = append(rows, input)
		index = len(rows) - 1
	}
	table, err := normalizeModelPrices(rows)
	if err != nil {
		return err
	}
	if err := s.saveState(path, datasetID, keys, rows); err != nil {
		return err
	}
	s.publishPrices(table)
	action := "create_model"
	if previous != nil {
		action = "update_model"
	}
	s.recordAudit(audit.Event{Action: action, Changes: priceChanges(previous, rows[index])})
	return nil
}

// priceChanges records what an upsert changed. "model" always names the
// subject and "provider" always carries the row's qualifier unchanged, since
// (name, provider) is the row's identity; an update lists only the fields
// whose values differ.
func priceChanges(before *ModelPrice, after ModelPrice) map[string]audit.Change {
	changes := map[string]audit.Change{"model": {From: after.Name, To: after.Name}, "provider": {From: after.Provider, To: after.Provider}}
	prior := ModelPrice{}
	if before == nil {
		changes["model"] = audit.Change{From: "", To: after.Name}
	} else {
		prior = *before
	}
	record := func(field string, from, to any) {
		if from != to {
			changes[field] = audit.Change{From: from, To: to}
		}
	}
	record("billing_mode", prior.BillingMode, after.BillingMode)
	record("billing_multiplier", prior.BillingMultiplier, after.BillingMultiplier)
	record("input_price_per_million", prior.InputPricePerMillion, after.InputPricePerMillion)
	record("output_price_per_million", prior.OutputPricePerMillion, after.OutputPricePerMillion)
	record("cache_read_price_per_million", prior.CacheReadPricePerMillion, after.CacheReadPricePerMillion)
	if !pricePointersEqual(prior.CacheWritePricePerMillion, after.CacheWritePricePerMillion) {
		changes["cache_write_price_per_million"] = audit.Change{From: prior.CacheWritePricePerMillion, To: after.CacheWritePricePerMillion}
	}
	record("per_call_usd", prior.PerCallUSD, after.PerCallUSD)
	return changes
}

// DeleteModel removes the price row for (name, provider). History billed
// under the model stays; later calls are unpriced.
func (s *Store) DeleteModel(name, provider string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("model name is required")
	}
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	rows := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	key := priceKey(name, provider)
	filtered := rows[:0]
	var stored *ModelPrice
	for _, row := range rows {
		if priceKey(row.Name, row.Provider) == key {
			match := row
			stored = &match
			continue
		}
		filtered = append(filtered, row)
	}
	if stored == nil {
		return fmt.Errorf("%w: %q (provider %q)", ErrUnknownModel, name, normalizeProvider(provider))
	}
	table, err := normalizeModelPrices(filtered)
	if err != nil {
		return err
	}
	if err := s.saveState(path, datasetID, keys, filtered); err != nil {
		return err
	}
	s.publishPrices(table)
	s.recordAudit(audit.Event{Action: "delete_model", Changes: map[string]audit.Change{
		"model":    {From: stored.Name, To: ""},
		"provider": {From: stored.Provider, To: ""},
	}})
	return nil
}

// ModelImportItem asks for one unpriced row keyed by the upstream model id.
type ModelImportItem struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type ModelImportRow struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

type ModelImportResult struct {
	Created []ModelImportRow `json:"created"`
	// Skipped names already exist or repeat within the batch ("exists",
	// "duplicate"). Import never overwrites a row.
	Skipped []ModelImportRow `json:"skipped"`
}

// ImportModels creates the requested rows at $0 in one state write; prices
// are synced afterwards. Invalid items reject the whole batch.
func (s *Store) ImportModels(items []ModelImportItem) (ModelImportResult, error) {
	result := ModelImportResult{Created: []ModelImportRow{}, Skipped: []ModelImportRow{}}
	if len(items) == 0 {
		return result, nil
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	rows := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()

	taken := make(map[string]bool, len(rows)+len(items))
	for _, row := range rows {
		taken[priceKey(row.Name, row.Provider)] = true
	}
	batch := make(map[string]bool, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return result, fmt.Errorf("%w: name is required", ErrInvalidModelImport)
		}
		switch key := priceKey(name, item.Provider); {
		case batch[key]:
			result.Skipped = append(result.Skipped, ModelImportRow{Name: name, Reason: "duplicate"})
			continue
		case taken[key]:
			batch[key] = true
			result.Skipped = append(result.Skipped, ModelImportRow{Name: name, Reason: "exists"})
			continue
		default:
			batch[key] = true
		}
		rows = append(rows, ModelPrice{Name: name, Provider: item.Provider})
		result.Created = append(result.Created, ModelImportRow{Name: name})
	}
	if len(result.Created) == 0 {
		return result, nil
	}
	table, err := normalizeModelPrices(rows)
	if err != nil {
		return ModelImportResult{Created: []ModelImportRow{}, Skipped: []ModelImportRow{}}, fmt.Errorf("%w: %v", ErrInvalidModelImport, err)
	}
	if err := s.saveState(path, datasetID, keys, rows); err != nil {
		return ModelImportResult{Created: []ModelImportRow{}, Skipped: []ModelImportRow{}}, err
	}
	s.publishPrices(table)
	// The created rows are the tail of rows, normalized in place above.
	for _, row := range rows[len(rows)-len(result.Created):] {
		s.recordAudit(audit.Event{Action: "import_create_model", Changes: map[string]audit.Change{
			"model":    {From: "", To: row.Name},
			"provider": {From: row.Provider, To: row.Provider},
		}})
	}
	return result, nil
}

type PriceImportMatch struct {
	Model                string   `json:"model"`
	PromptPricePer1M     *float64 `json:"prompt_price_per_1m"`
	CompletionPricePer1M *float64 `json:"completion_price_per_1m"`
	CacheReadPricePer1M  *float64 `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M *float64 `json:"cache_write_price_per_1m"`
}

type PriceImportApplied struct {
	Model                    string   `json:"model"`
	Provider                 string   `json:"provider"`
	OldInputPricePerMillion  float64  `json:"old_input_price_per_million"`
	OldOutputPricePerMillion float64  `json:"old_output_price_per_million"`
	OldCacheReadPerMillion   float64  `json:"old_cache_read_price_per_million"`
	OldCacheWritePerMillion  *float64 `json:"old_cache_write_price_per_million,omitempty"`
	NewInputPricePerMillion  float64  `json:"new_input_price_per_million"`
	NewOutputPricePerMillion float64  `json:"new_output_price_per_million"`
	NewCacheReadPerMillion   float64  `json:"new_cache_read_price_per_million"`
	NewCacheWritePerMillion  *float64 `json:"new_cache_write_price_per_million,omitempty"`
	Note                     string   `json:"note,omitempty"`
}

type PriceImportUnchanged struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}
type PriceImportSkipped struct {
	MatchModel string `json:"match_model,omitempty"`
	Model      string `json:"model,omitempty"`
	Reason     string `json:"reason"`
}
type PriceImportResult struct {
	Applied   []PriceImportApplied   `json:"applied"`
	Unchanged []PriceImportUnchanged `json:"unchanged"`
	Skipped   []PriceImportSkipped   `json:"skipped"`
}

type importPricePatch struct{ input, output, cache, cacheWrite *float64 }

func (patch importPricePatch) empty() bool {
	return patch.input == nil && patch.output == nil && patch.cache == nil && patch.cacheWrite == nil
}

// ImportModelPrices applies token prices from a pricing source to every row
// whose model id matches, regardless of provider qualifier.
func (s *Store) ImportModelPrices(matches []PriceImportMatch, dryRun bool) (PriceImportResult, error) {
	result := PriceImportResult{Applied: []PriceImportApplied{}, Unchanged: []PriceImportUnchanged{}, Skipped: []PriceImportSkipped{}}
	patches := make(map[string]importPricePatch, len(matches))
	matchOrder := make([]string, 0, len(matches))
	for _, match := range matches {
		name := strings.ToLower(strings.TrimSpace(match.Model))
		patch := importPricePatch{input: match.PromptPricePer1M, output: match.CompletionPricePer1M, cache: match.CacheReadPricePer1M, cacheWrite: match.CacheWritePricePer1M}
		if name == "" || patch.empty() {
			continue
		}
		if _, exists := patches[name]; !exists {
			matchOrder = append(matchOrder, name)
		}
		patches[name] = patch
	}

	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	rows := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	matched := make(map[string]struct{})
	changed := false
	for i := range rows {
		row := &rows[i]
		name := strings.ToLower(row.Name)
		patch, exists := patches[name]
		if !exists {
			continue
		}
		matched[name] = struct{}{}
		oldInput, oldOutput, oldCache, oldWrite := row.InputPricePerMillion, row.OutputPricePerMillion, row.CacheReadPricePerMillion, row.CacheWritePricePerMillion
		if patch.input != nil {
			row.InputPricePerMillion = *patch.input
		}
		if patch.output != nil {
			row.OutputPricePerMillion = *patch.output
		}
		if patch.cache != nil {
			row.CacheReadPricePerMillion = *patch.cache
		}
		if patch.cacheWrite != nil {
			row.CacheWritePricePerMillion = cloneFloat64(patch.cacheWrite)
		}
		if oldInput == row.InputPricePerMillion && oldOutput == row.OutputPricePerMillion && oldCache == row.CacheReadPricePerMillion && pricePointersEqual(oldWrite, row.CacheWritePricePerMillion) {
			result.Unchanged = append(result.Unchanged, PriceImportUnchanged{Model: row.Name, Provider: row.Provider})
			continue
		}
		record := PriceImportApplied{
			Model: row.Name, Provider: row.Provider, OldInputPricePerMillion: oldInput, OldOutputPricePerMillion: oldOutput, OldCacheReadPerMillion: oldCache, OldCacheWritePerMillion: oldWrite,
			NewInputPricePerMillion: row.InputPricePerMillion, NewOutputPricePerMillion: row.OutputPricePerMillion, NewCacheReadPerMillion: row.CacheReadPricePerMillion, NewCacheWritePerMillion: row.CacheWritePricePerMillion,
		}
		if row.BillingMode == "per_call" {
			record.Note = "billing_mode is per_call; token prices remain dormant"
		}
		result.Applied = append(result.Applied, record)
		changed = true
	}
	for _, name := range matchOrder {
		if _, exists := matched[name]; !exists {
			result.Skipped = append(result.Skipped, PriceImportSkipped{MatchModel: name, Reason: "no_match"})
		}
	}
	if !changed {
		return result, nil
	}
	table, err := normalizeModelPrices(rows)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidModelPriceImport, err)
	}
	if dryRun {
		return result, nil
	}
	if err := s.saveState(path, datasetID, keys, rows); err != nil {
		return result, err
	}
	s.publishPrices(table)
	for _, applied := range result.Applied {
		s.recordAudit(audit.Event{Action: "import_update_prices", Changes: map[string]audit.Change{
			"model":                         {From: applied.Model, To: applied.Model},
			"provider":                      {From: applied.Provider, To: applied.Provider},
			"input_price_per_million":       {From: applied.OldInputPricePerMillion, To: applied.NewInputPricePerMillion},
			"output_price_per_million":      {From: applied.OldOutputPricePerMillion, To: applied.NewOutputPricePerMillion},
			"cache_read_price_per_million":  {From: applied.OldCacheReadPerMillion, To: applied.NewCacheReadPerMillion},
			"cache_write_price_per_million": {From: applied.OldCacheWritePerMillion, To: applied.NewCacheWritePerMillion},
		}})
	}
	return result, nil
}

func (s *Store) publishPrices(table *priceTable) {
	s.mu.Lock()
	s.prices = table
	s.mu.Unlock()
}
