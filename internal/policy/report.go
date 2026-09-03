package policy

import (
	"sort"
	"strings"
)

func (s *Store) Status() map[string]any {
	s.mu.RLock()
	enabled := s.enabled
	statePath := s.statePath
	datasetID := s.datasetID
	keys := s.keysSnapshotLocked()
	modelCount := len(s.models)
	limiter := s.limiter
	usage := s.usage
	s.mu.RUnlock()
	rpmUsage := map[string]int{}
	if limiter != nil {
		rpmUsage = limiter.Snapshot()
	}
	return map[string]any{
		"enabled": enabled, "state_file": statePath, "dataset_id": datasetID,
		"key_count": len(keys), "model_count": modelCount,
		"rpm_usage": rpmUsage, "usage": usageSummaryForKeys(usage, keys),
	}
}

func usageSummaryForKeys(usage *usageLedger, keys []KeyConfig) map[string]UsageSummary {
	if usage == nil {
		return map[string]UsageSummary{}
	}
	result := make(map[string]UsageSummary, len(keys))
	for _, key := range keys {
		result[key.ID] = usage.Summary(key.ID, quotaLimitsForKey(key))
	}
	return result
}

func quotaLimitsForKey(key KeyConfig) quotaLimits {
	limits := quotaLimits{
		DailyUSD: key.DailyLimitUSD, WeeklyUSD: key.WeeklyLimitUSD,
		MonthlyUSD: key.MonthlyLimitUSD, LimitsChangedAt: key.LimitsChangedAt,
	}
	for _, ref := range key.Models {
		if ref.DailyLimitUSD > 0 {
			limits.Models = append(limits.Models, modelQuotaLimit{Name: ref.Name, DailyUSD: ref.DailyLimitUSD})
		}
	}
	return limits
}

func (s *Store) Keys() []KeyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keysSnapshotLocked()
}

func (s *Store) keysSnapshotLocked() []KeyConfig {
	keys := make([]KeyConfig, 0, len(s.keys))
	for _, key := range s.keys {
		copy := *key
		copy.Models = append([]KeyModelRef(nil), key.Models...)
		keys = append(keys, copy)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

func (s *Store) modelsSnapshotLocked() []ModelDefinition {
	models := make([]ModelDefinition, 0, len(s.models))
	for _, model := range s.models {
		copy := *model
		copy.Targets = append([]ModelTarget(nil), model.Targets...)
		copy.CacheWritePricePerMillion = cloneFloat64(model.CacheWritePricePerMillion)
		models = append(models, copy)
	}
	sort.Slice(models, func(i, j int) bool { return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name) })
	return models
}

func (s *Store) ModelsSnapshot() []ModelDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modelsSnapshotLocked()
}

type ModelWithRefs struct {
	ModelDefinition
	RefCount int      `json:"ref_count"`
	RefKeys  []string `json:"ref_keys"`
}

func (s *Store) modelRefIndexLocked() map[string][]string {
	index := make(map[string][]string)
	for _, key := range s.keys {
		if key == nil {
			continue
		}
		for _, ref := range key.Models {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if name != "" {
				index[name] = append(index[name], key.ID)
			}
		}
	}
	for name := range index {
		sort.Strings(index[name])
	}
	return index
}

func (s *Store) ModelsSnapshotWithRefs() []ModelWithRefs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := s.modelsSnapshotLocked()
	refs := s.modelRefIndexLocked()
	result := make([]ModelWithRefs, 0, len(models))
	for _, model := range models {
		keys := refs[strings.ToLower(model.Name)]
		if keys == nil {
			keys = []string{}
		}
		result = append(result, ModelWithRefs{ModelDefinition: model, RefCount: len(keys), RefKeys: keys})
	}
	return result
}

func (s *Store) ModelRefKeys(name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := s.modelRefIndexLocked()[strings.ToLower(strings.TrimSpace(name))]
	if keys == nil {
		return []string{}
	}
	return append([]string(nil), keys...)
}

func (s *Store) classifyRulesSnapshotLocked() []ClassifyRule {
	return append([]ClassifyRule(nil), s.classifyRules...)
}

func (s *Store) ClassifyRulesSnapshot() []ClassifyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.classifyRulesSnapshotLocked()
}

func (s *Store) UsageSummaryFor(key KeyConfig) UsageSummary {
	_, usage := s.runtimeComponents()
	if usage == nil {
		return UsageSummary{
			DailyLimitUSD: key.DailyLimitUSD, WeeklyLimitUSD: key.WeeklyLimitUSD,
			MonthlyLimitUSD: key.MonthlyLimitUSD, LimitsChangedAt: key.LimitsChangedAt,
		}
	}
	return usage.Summary(key.ID, quotaLimitsForKey(key))
}

func (s *Store) ResetUsage(id string) {
	_, usage := s.runtimeComponents()
	if usage != nil {
		usage.resetUsage(id)
	}
}

func (s *Store) ModelUsageFor(keyID string) (KeyConfig, []ModelUsageEntry, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	s.mu.RLock()
	definitions := make([]ModelDefinition, 0, len(key.Models))
	for _, ref := range key.Models {
		if model := s.models[strings.ToLower(ref.Name)]; model != nil {
			copy := *model
			copy.Targets = append([]ModelTarget(nil), model.Targets...)
			definitions = append(definitions, copy)
		}
	}
	s.mu.RUnlock()
	_, usage := s.runtimeComponents()
	if usage == nil {
		rows := make([]ModelUsageEntry, 0, len(definitions))
		for _, model := range definitions {
			rows = append(rows, ModelUsageEntry{
				Name: model.Name, BillingMode: model.BillingMode, Free: model.Free,
				PerCallUSD: model.PerCallUSD, InConfig: true,
			})
		}
		return *key, rows, true
	}
	return *key, usage.ModelUsage(key.ID, definitions), true
}

func (s *Store) UsageHistoryFor(keyID string, days int) (KeyConfig, []UsageHistoryDay, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	_, usage := s.runtimeComponents()
	if usage == nil {
		return *key, []UsageHistoryDay{}, true
	}
	return *key, usage.History(key.ID, days), true
}
