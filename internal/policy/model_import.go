package policy

import (
	"errors"
	"fmt"
	"strings"

	"cpa-key-policy/internal/policy/audit"
)

var ErrInvalidModelImport = errors.New("invalid model import")

// ModelImportItem asks for one public model bound to a CPA capability. The
// public name defaults to the upstream model id.
type ModelImportItem struct {
	Name        string `json:"name,omitempty"`
	Provider    string `json:"provider"`
	TargetModel string `json:"target_model"`
}

type ModelImportRow struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

type ModelImportResult struct {
	Created []ModelImportRow `json:"created"`
	// Skipped names already exist or repeat within the batch ("exists",
	// "duplicate"). Import never overwrites a model.
	Skipped []ModelImportRow `json:"skipped"`
}

// ImportModels creates the requested models at $0 in one state write; prices
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
	models := s.modelsSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()

	taken := make(map[string]bool, len(models)+len(items))
	for _, model := range models {
		taken[strings.ToLower(model.Name)] = true
	}
	batch := make(map[string]bool, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = strings.TrimSpace(item.TargetModel)
		}
		if name == "" {
			return result, fmt.Errorf("%w: target_model is required", ErrInvalidModelImport)
		}
		switch lower := strings.ToLower(name); {
		case batch[lower]:
			result.Skipped = append(result.Skipped, ModelImportRow{Name: name, Reason: "duplicate"})
			continue
		case taken[lower]:
			batch[lower] = true
			result.Skipped = append(result.Skipped, ModelImportRow{Name: name, Reason: "exists"})
			continue
		default:
			batch[lower] = true
		}
		models = append(models, ModelDefinition{Name: name, Provider: item.Provider, TargetModel: item.TargetModel})
		result.Created = append(result.Created, ModelImportRow{Name: name})
	}
	if len(result.Created) == 0 {
		return result, nil
	}
	cfg := Config{Enabled: true, Keys: keys, Models: models}
	if err := normalizeConfig(&cfg); err != nil {
		return ModelImportResult{Created: []ModelImportRow{}, Skipped: []ModelImportRow{}}, fmt.Errorf("%w: %v", ErrInvalidModelImport, err)
	}
	if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models); err != nil {
		return ModelImportResult{Created: []ModelImportRow{}, Skipped: []ModelImportRow{}}, err
	}
	s.publishModels(cfg.Models)
	for _, created := range result.Created {
		s.recordAudit(audit.Event{Action: "import_create_model", Changes: map[string]audit.Change{"model": {From: "", To: created.Name}}})
	}
	return result, nil
}
