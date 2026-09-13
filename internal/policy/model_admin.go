package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"cpa-key-policy/internal/policy/audit"
)

var ErrInvalidModelPriceImport = errors.New("invalid model price import")

func (s *Store) UpsertModel(input ModelDefinition) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	input.Name = strings.TrimSpace(input.Name)
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	found := false
	previousMultiplier := 1.0
	for i := range models {
		if strings.EqualFold(models[i].Name, input.Name) {
			previousMultiplier = models[i].BillingMultiplier
			input.Name = models[i].Name
			models[i] = input
			found = true
			break
		}
	}
	if !found {
		models = append(models, input)
	}
	cfg := Config{Enabled: true, Keys: keys, Models: models, ClassifyRules: rules}
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	canonicalName := strings.TrimSpace(input.Name)
	for _, model := range cfg.Models {
		if strings.EqualFold(model.Name, canonicalName) {
			canonicalName = model.Name
			break
		}
	}
	if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
		return err
	}
	s.publishModels(cfg.Models, true)
	action := "create_model"
	if found {
		action = "update_model"
	}
	changes := map[string]audit.Change{"model": {From: canonicalName, To: canonicalName}}
	for _, model := range cfg.Models {
		if model.Name == canonicalName && model.BillingMultiplier != previousMultiplier {
			changes["billing_multiplier"] = audit.Change{From: previousMultiplier, To: model.BillingMultiplier}
		}
	}
	s.recordAudit(audit.Event{Action: action, Changes: changes})
	return nil
}

func (s *Store) DeleteModel(name string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("model name is required")
	}
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	refs := s.modelRefIndexLocked()[strings.ToLower(name)]
	s.mu.RUnlock()
	if len(refs) > 0 {
		return fmt.Errorf("model is referenced by %d key(s); remove references first", len(refs))
	}
	filtered := models[:0]
	found := false
	for _, model := range models {
		if strings.EqualFold(model.Name, name) {
			found = true
			continue
		}
		filtered = append(filtered, model)
	}
	if !found {
		return errors.New("model not found")
	}
	if err := s.saveState(path, datasetID, keys, filtered, rules); err != nil {
		return err
	}
	s.publishModels(filtered, false)
	s.recordAudit(audit.Event{Action: "delete_model", Changes: map[string]audit.Change{"model": {From: name, To: ""}}})
	return nil
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
	Model string `json:"model"`
}
type PriceImportSkipped struct {
	MatchModel string `json:"match_model,omitempty"`
	Model      string `json:"model,omitempty"`
	Reason     string `json:"reason"`
}
type PriceImportResult struct {
	Applied      []PriceImportApplied   `json:"applied"`
	Unchanged    []PriceImportUnchanged `json:"unchanged"`
	Skipped      []PriceImportSkipped   `json:"skipped"`
	AffectedKeys []string               `json:"affected_keys"`
}

type importPricePatch struct{ input, output, cache, cacheWrite *float64 }

func (patch importPricePatch) empty() bool {
	return patch.input == nil && patch.output == nil && patch.cache == nil && patch.cacheWrite == nil
}
func pricePointersEqual(left, right *float64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
func importPatchesEqual(left, right importPricePatch) bool {
	return pricePointersEqual(left.input, right.input) && pricePointersEqual(left.output, right.output) && pricePointersEqual(left.cache, right.cache) && pricePointersEqual(left.cacheWrite, right.cacheWrite)
}

func (s *Store) ImportModelPrices(matches []PriceImportMatch, dryRun bool) (PriceImportResult, error) {
	result := PriceImportResult{Applied: []PriceImportApplied{}, Unchanged: []PriceImportUnchanged{}, Skipped: []PriceImportSkipped{}, AffectedKeys: []string{}}
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
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	refs := s.modelRefIndexLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	matched := make(map[string]struct{})
	affected := make(map[string]struct{})
	changed := false
	for i := range models {
		model := &models[i]
		var hits []importPricePatch
		var hitNames []string
		for _, target := range model.Targets {
			name := strings.ToLower(target.TargetModel)
			if patch, exists := patches[name]; exists {
				hits = append(hits, patch)
				hitNames = append(hitNames, name)
			}
		}
		targetMatched := len(hits) > 0
		if !targetMatched {
			name := strings.ToLower(model.Name)
			if patch, exists := patches[name]; exists {
				hits = []importPricePatch{patch}
				hitNames = []string{name}
			}
		}
		if len(hits) == 0 {
			continue
		}
		for _, name := range hitNames {
			matched[name] = struct{}{}
		}
		if model.Free {
			result.Skipped = append(result.Skipped, PriceImportSkipped{Model: model.Name, Reason: "free_model"})
			continue
		}
		conflict := false
		for _, patch := range hits[1:] {
			if !importPatchesEqual(hits[0], patch) {
				conflict = true
				break
			}
		}
		if conflict || (targetMatched && len(hits) != len(model.Targets)) {
			result.Skipped = append(result.Skipped, PriceImportSkipped{Model: model.Name, Reason: "target_price_conflict"})
			continue
		}
		oldInput, oldOutput, oldCache, oldWrite := model.InputPricePerMillion, model.OutputPricePerMillion, model.CacheReadPricePerMillion, model.CacheWritePricePerMillion
		if hits[0].input != nil {
			model.InputPricePerMillion = *hits[0].input
		}
		if hits[0].output != nil {
			model.OutputPricePerMillion = *hits[0].output
		}
		if hits[0].cache != nil {
			model.CacheReadPricePerMillion = *hits[0].cache
		}
		if hits[0].cacheWrite != nil {
			model.CacheWritePricePerMillion = cloneFloat64(hits[0].cacheWrite)
		}
		if oldInput == model.InputPricePerMillion && oldOutput == model.OutputPricePerMillion && oldCache == model.CacheReadPricePerMillion && pricePointersEqual(oldWrite, model.CacheWritePricePerMillion) {
			result.Unchanged = append(result.Unchanged, PriceImportUnchanged{Model: model.Name})
			continue
		}
		record := PriceImportApplied{
			Model: model.Name, OldInputPricePerMillion: oldInput, OldOutputPricePerMillion: oldOutput, OldCacheReadPerMillion: oldCache, OldCacheWritePerMillion: oldWrite,
			NewInputPricePerMillion: model.InputPricePerMillion, NewOutputPricePerMillion: model.OutputPricePerMillion, NewCacheReadPerMillion: model.CacheReadPricePerMillion, NewCacheWritePerMillion: model.CacheWritePricePerMillion,
		}
		if model.BillingMode == "per_call" {
			record.Note = "billing_mode is per_call; token prices remain dormant"
		}
		result.Applied = append(result.Applied, record)
		for _, keyID := range refs[strings.ToLower(model.Name)] {
			affected[keyID] = struct{}{}
		}
		changed = true
	}
	for _, name := range matchOrder {
		if _, exists := matched[name]; !exists {
			result.Skipped = append(result.Skipped, PriceImportSkipped{MatchModel: name, Reason: "no_match"})
		}
	}
	for keyID := range affected {
		result.AffectedKeys = append(result.AffectedKeys, keyID)
	}
	sort.Strings(result.AffectedKeys)
	if !changed {
		return result, nil
	}
	cfg := Config{Enabled: true, Keys: keys, Models: models, ClassifyRules: rules}
	if err := normalizeConfig(&cfg); err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidModelPriceImport, err)
	}
	if dryRun {
		return result, nil
	}
	if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
		return result, err
	}
	s.publishModels(cfg.Models, false)
	for _, applied := range result.Applied {
		s.recordAudit(audit.Event{Action: "import_update_prices", Changes: map[string]audit.Change{
			"model":                         {From: applied.Model, To: applied.Model},
			"input_price_per_million":       {From: applied.OldInputPricePerMillion, To: applied.NewInputPricePerMillion},
			"output_price_per_million":      {From: applied.OldOutputPricePerMillion, To: applied.NewOutputPricePerMillion},
			"cache_read_price_per_million":  {From: applied.OldCacheReadPerMillion, To: applied.NewCacheReadPerMillion},
			"cache_write_price_per_million": {From: applied.OldCacheWritePerMillion, To: applied.NewCacheWritePerMillion},
		}})
	}
	return result, nil
}

func (s *Store) publishModels(models []ModelDefinition, resetPicks bool) {
	index := make(map[string]*ModelDefinition, len(models))
	for i := range models {
		copy := models[i]
		copy.Targets = append([]ModelTarget(nil), models[i].Targets...)
		copy.CacheWritePricePerMillion = cloneFloat64(models[i].CacheWritePricePerMillion)
		index[strings.ToLower(copy.Name)] = &copy
	}
	s.mu.Lock()
	s.models = index
	s.rrCounters = make(map[string]int)
	if resetPicks {
		s.pendingPicks = make(map[string][]pendingPick)
	}
	s.mu.Unlock()
}
