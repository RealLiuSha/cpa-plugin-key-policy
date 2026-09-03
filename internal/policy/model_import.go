package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"cpa-key-policy/internal/policy/audit"
)

var ErrInvalidModelImport = errors.New("invalid model import")

type ModelImportItem struct {
	Name          string        `json:"name"`
	Targets       []ModelTarget `json:"targets"`
	Dispatch      string        `json:"dispatch,omitempty"`
	Free          bool          `json:"free"`
	Overwrite     bool          `json:"overwrite"`
	Input         float64       `json:"input_price_per_million"`
	Output        float64       `json:"output_price_per_million"`
	CacheRead     float64       `json:"cache_read_price_per_million"`
	CacheWrite    *float64      `json:"cache_write_price_per_million,omitempty"`
	PerCallUSD    float64       `json:"per_call_usd,omitempty"`
	BillingMode   string        `json:"billing_mode,omitempty"`
	MissingPrice  bool          `json:"missing_price,omitempty"`
	PriceConflict bool          `json:"price_conflict,omitempty"`
}

type ModelImportRow struct {
	Name          string   `json:"name"`
	Action        string   `json:"action"`
	Reason        string   `json:"reason,omitempty"`
	AffectedKeys  []string `json:"affected_keys,omitempty"`
	Duplicate     bool     `json:"duplicate,omitempty"`
	MissingPrice  bool     `json:"missing_price,omitempty"`
	PriceConflict bool     `json:"price_conflict,omitempty"`
}

type ModelImportResult struct {
	Created      []ModelImportRow `json:"created"`
	Updated      []ModelImportRow `json:"updated"`
	Skipped      []ModelImportRow `json:"skipped"`
	Conflicts    []ModelImportRow `json:"conflicts"`
	MissingPrice []ModelImportRow `json:"missing_price"`
	AffectedKeys []string         `json:"affected_keys"`
}

func (s *Store) ImportModels(items []ModelImportItem, dryRun bool) (ModelImportResult, error) {
	result := ModelImportResult{
		Created: []ModelImportRow{}, Updated: []ModelImportRow{}, Skipped: []ModelImportRow{},
		Conflicts: []ModelImportRow{}, MissingPrice: []ModelImportRow{}, AffectedKeys: []string{},
	}
	if len(items) == 0 {
		return result, nil
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

	existing := make(map[string]int, len(models))
	for i, model := range models {
		existing[strings.ToLower(model.Name)] = i
	}
	batchNames := make(map[string]int)
	affected := make(map[string]struct{})
	blocking := false
	nextModels := append([]ModelDefinition(nil), models...)

	for _, item := range items {
		row, model, err := normalizeImportItem(item)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrInvalidModelImport, err)
		}
		nameKey := strings.ToLower(model.Name)
		if seen := batchNames[nameKey]; seen > 0 {
			row.Action = "conflict"
			row.Reason = "duplicate_batch_name"
			row.Duplicate = true
			result.Conflicts = append(result.Conflicts, row)
			blocking = true
			continue
		}
		batchNames[nameKey] = 1
		if item.MissingPrice && !item.Free {
			row.Action = "missing_price"
			row.Reason = "missing_price"
			row.MissingPrice = true
			result.MissingPrice = append(result.MissingPrice, row)
			blocking = true
			continue
		}
		if item.PriceConflict && !item.Free {
			row.Action = "conflict"
			row.Reason = "target_price_conflict"
			row.PriceConflict = true
			result.Conflicts = append(result.Conflicts, row)
			blocking = true
			continue
		}
		if index, ok := existing[nameKey]; ok {
			row.AffectedKeys = append([]string(nil), refs[nameKey]...)
			for _, keyID := range row.AffectedKeys {
				affected[keyID] = struct{}{}
			}
			if !item.Overwrite {
				row.Action = "skip"
				row.Reason = "exists"
				result.Skipped = append(result.Skipped, row)
				continue
			}
			nextModels[index] = model
			row.Action = "update"
			result.Updated = append(result.Updated, row)
			continue
		}
		nextModels = append(nextModels, model)
		existing[nameKey] = len(nextModels) - 1
		row.Action = "create"
		result.Created = append(result.Created, row)
	}

	for keyID := range affected {
		result.AffectedKeys = append(result.AffectedKeys, keyID)
	}
	sort.Strings(result.AffectedKeys)
	if blocking {
		if dryRun {
			return result, nil
		}
		return result, fmt.Errorf("%w: batch contains missing prices or conflicts", ErrInvalidModelImport)
	}
	cfg := Config{Enabled: true, Keys: keys, Models: nextModels, ClassifyRules: rules}
	if err := normalizeConfig(&cfg); err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidModelImport, err)
	}
	if dryRun {
		return result, nil
	}
	if len(result.Created) == 0 && len(result.Updated) == 0 {
		return result, nil
	}
	if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
		return result, err
	}
	s.publishModels(cfg.Models, true)
	for _, created := range result.Created {
		s.recordAudit(audit.Event{Action: "import_create_model", Changes: map[string]audit.Change{"model": {From: "", To: created.Name}}})
	}
	for _, updated := range result.Updated {
		s.recordAudit(audit.Event{Action: "import_overwrite_model", Changes: map[string]audit.Change{
			"model":         {From: updated.Name, To: updated.Name},
			"affected_keys": {From: updated.AffectedKeys, To: updated.AffectedKeys},
		}})
	}
	return result, nil
}

func normalizeImportItem(item ModelImportItem) (ModelImportRow, ModelDefinition, error) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return ModelImportRow{}, ModelDefinition{}, errors.New("model name is required")
	}
	if len(item.Targets) != 1 {
		return ModelImportRow{Name: name, Action: "conflict", Reason: "multiple_targets"}, ModelDefinition{}, fmt.Errorf("model %q must bind exactly one target", name)
	}
	model := ModelDefinition{
		Name: name, Targets: item.Targets, Dispatch: item.Dispatch, BillingMode: item.BillingMode, Free: item.Free,
		InputPricePerMillion: item.Input, OutputPricePerMillion: item.Output,
		CacheReadPricePerMillion: item.CacheRead, CacheWritePricePerMillion: item.CacheWrite, PerCallUSD: item.PerCallUSD,
	}
	return ModelImportRow{Name: name, AffectedKeys: []string{}}, model, nil
}
