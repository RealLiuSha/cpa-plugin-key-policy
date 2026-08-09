package policy

import (
	"errors"
	"strings"

	"cpa-key-policy/internal/policy/audit"
)

func (s *Store) UpsertClassifyRule(rule ClassifyRule) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	found := false
	for i := range rules {
		if strings.EqualFold(rules[i].Name, rule.Name) {
			rules[i] = rule
			found = true
			break
		}
	}
	if !found {
		rules = append(rules, rule)
	}
	cfg := Config{Enabled: true, Keys: keys, Models: models, ClassifyRules: rules}
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
		return err
	}
	s.mu.Lock()
	s.classifyRules = cfg.ClassifyRules
	onChanged := s.onClassifyRulesChanged
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	action := "create_classify_rule"
	if found {
		action = "update_classify_rule"
	}
	s.recordAudit(audit.Event{Action: action, Changes: map[string]audit.Change{"rule": {From: rule.Name, To: rule.Name}}})
	return nil
}

func (s *Store) DeleteClassifyRule(name string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("rule name is required")
	}
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	filtered := rules[:0]
	for _, rule := range rules {
		if !strings.EqualFold(rule.Name, name) {
			filtered = append(filtered, rule)
		}
	}
	if err := s.saveState(path, datasetID, keys, models, filtered); err != nil {
		return err
	}
	s.mu.Lock()
	s.classifyRules = filtered
	onChanged := s.onClassifyRulesChanged
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	s.recordAudit(audit.Event{Action: "delete_classify_rule", Changes: map[string]audit.Change{"rule": {From: name, To: ""}}})
	return nil
}

func (s *Store) ReorderClassifyRules(names []string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()
	byName := make(map[string]ClassifyRule, len(rules))
	for _, rule := range rules {
		byName[strings.ToLower(rule.Name)] = rule
	}
	reordered := make([]ClassifyRule, 0, len(rules))
	used := make(map[string]bool, len(names))
	for _, name := range names {
		canonical := strings.ToLower(name)
		if rule, exists := byName[canonical]; exists && !used[canonical] {
			reordered = append(reordered, rule)
			used[canonical] = true
		}
	}
	for _, rule := range rules {
		if !used[strings.ToLower(rule.Name)] {
			reordered = append(reordered, rule)
		}
	}
	if err := s.saveState(path, datasetID, keys, models, reordered); err != nil {
		return err
	}
	s.mu.Lock()
	s.classifyRules = reordered
	onChanged := s.onClassifyRulesChanged
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	s.recordAudit(audit.Event{Action: "reorder_classify_rules"})
	return nil
}

func (s *Store) SetOnClassifyRulesChanged(fn func()) {
	s.mu.Lock()
	s.onClassifyRulesChanged = fn
	s.mu.Unlock()
}

func callClassifyRulesChanged(fn func()) {
	if fn != nil {
		fn()
	}
}
