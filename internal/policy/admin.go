package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cpa-key-policy/internal/policy/audit"
	policyPersist "cpa-key-policy/internal/policy/persist"
)

func (s *Store) UpsertKey(input KeyConfig, persist bool) error {
	previous, stored, err := s.applyKeyMutation(input, persist)
	if err != nil {
		return err
	}
	if persist {
		action := "create_key"
		if previous != nil {
			action = "update_key"
		}
		s.recordAudit(audit.Event{Action: action, KeyID: stored.ID, Changes: keyLimitChanges(previous, stored)})
	}
	return nil
}

// applyKeyMutation owns the normalized, persist-before-publish key update.
// Callers add the audit event that describes their management operation.
func (s *Store) applyKeyMutation(input KeyConfig, persist bool) (*KeyConfig, KeyConfig, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()

	now := s.billingNow().UTC()
	var previous *KeyConfig
	found := false
	for i := range keys {
		if keys[i].ID != input.ID {
			continue
		}
		copy := keys[i]
		previous = &copy
		if !keys[i].CreatedAt.IsZero() {
			input.CreatedAt = keys[i].CreatedAt
		}
		keys[i] = input
		found = true
		break
	}
	if !found {
		keys = append(keys, input)
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = now
	}
	if previous == nil {
		if input.DailyLimitUSD > 0 || input.WeeklyLimitUSD > 0 || input.MonthlyLimitUSD > 0 || hasModelLimit(input.Models) {
			input.LimitsChangedAt = now
		}
	} else if !limitsEqual(*previous, input) {
		input.LimitsChangedAt = now
	} else {
		input.LimitsChangedAt = previous.LimitsChangedAt
	}
	input.UpdatedAt = now
	if found {
		for i := range keys {
			if keys[i].ID == input.ID {
				keys[i] = input
				break
			}
		}
	} else {
		keys[len(keys)-1] = input
	}
	cfg := Config{Enabled: true, Keys: keys, Models: models, ClassifyRules: rules}
	if err := normalizeConfig(&cfg); err != nil {
		return nil, KeyConfig{}, err
	}
	for i := range cfg.Keys {
		if cfg.Keys[i].ID == input.ID {
			input = cfg.Keys[i]
			break
		}
	}
	if persist {
		if err := s.saveState(path, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
			return nil, KeyConfig{}, err
		}
	}
	s.mu.Lock()
	copy := input
	copy.Models = append([]KeyModelRef(nil), input.Models...)
	s.keys[input.ID] = &copy
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(input.ID)
	s.mu.Unlock()
	_, ledger := s.runtimeComponents()
	if ledger != nil {
		ledger.initializeCycles([]KeyConfig{input}, false)
	}
	return previous, input, nil
}

func keyLimitChanges(previous *KeyConfig, current KeyConfig) map[string]audit.Change {
	changes := make(map[string]audit.Change)
	before := KeyConfig{}
	if previous != nil {
		before = *previous
	}
	for field, values := range map[string][2]float64{
		"daily_limit_usd": {before.DailyLimitUSD, current.DailyLimitUSD}, "weekly_limit_usd": {before.WeeklyLimitUSD, current.WeeklyLimitUSD},
		"monthly_limit_usd": {before.MonthlyLimitUSD, current.MonthlyLimitUSD},
	} {
		if values[0] != values[1] {
			changes[field] = audit.Change{From: values[0], To: values[1]}
		}
	}
	beforeModels := make(map[string]float64, len(before.Models))
	for _, ref := range before.Models {
		beforeModels[strings.ToLower(ref.Name)] = ref.DailyLimitUSD
	}
	afterModels := make(map[string]float64, len(current.Models))
	for _, ref := range current.Models {
		afterModels[strings.ToLower(ref.Name)] = ref.DailyLimitUSD
	}
	beforeRaw, _ := json.Marshal(beforeModels)
	afterRaw, _ := json.Marshal(afterModels)
	if string(beforeRaw) != string(afterRaw) {
		changes["model_daily_limits"] = audit.Change{From: beforeModels, To: afterModels}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

func hasModelLimit(refs []KeyModelRef) bool {
	for _, ref := range refs {
		if ref.DailyLimitUSD > 0 {
			return true
		}
	}
	return false
}

func limitsEqual(left, right KeyConfig) bool {
	if left.DailyLimitUSD != right.DailyLimitUSD || left.WeeklyLimitUSD != right.WeeklyLimitUSD || left.MonthlyLimitUSD != right.MonthlyLimitUSD {
		return false
	}
	limits := make(map[string]float64, len(left.Models))
	for _, ref := range left.Models {
		limits[strings.ToLower(ref.Name)] = ref.DailyLimitUSD
	}
	if len(limits) != len(right.Models) {
		return false
	}
	for _, ref := range right.Models {
		if value, exists := limits[strings.ToLower(ref.Name)]; !exists || value != ref.DailyLimitUSD {
			return false
		}
	}
	return true
}

func (s *Store) DeleteKey(id string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.modelsSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path, datasetID := s.statePath, s.datasetID
	_, exists := s.keys[id]
	limiter, usageLedger := s.limiter, s.usage
	s.mu.RUnlock()
	if !exists {
		return ErrUnknownKey
	}
	filtered := keys[:0]
	for _, key := range keys {
		if key.ID != id {
			filtered = append(filtered, key)
		}
	}
	if err := s.saveState(path, datasetID, filtered, models, rules); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.keys, id)
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(id)
	s.mu.Unlock()
	if limiter != nil {
		limiter.Reset(id)
	}
	if usageLedger != nil {
		usageLedger.removeKeyUsage(id)
	}
	s.recordAudit(audit.Event{Action: "delete_key", KeyID: id})
	return nil
}

func (s *Store) RotateKey(id string) (string, KeyConfig, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", KeyConfig{}, errors.New("id is required")
	}
	plain, err := GenerateKey()
	if err != nil {
		return "", KeyConfig{}, err
	}
	hash, err := HashKey(plain)
	if err != nil {
		return "", KeyConfig{}, err
	}
	key := s.findByID(id)
	if key == nil {
		return "", KeyConfig{}, ErrUnknownKey
	}
	key.KeyHash = hash
	key.KeyPreview = PreviewKey(plain)
	_, stored, err := s.applyKeyMutation(*key, true)
	if err != nil {
		return "", KeyConfig{}, err
	}
	s.recordAudit(audit.Event{Action: "rotate_key", KeyID: id})
	return plain, stored, nil
}

func (s *Store) ResetRPM(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is required")
	}
	limiter, _ := s.runtimeComponents()
	if limiter != nil {
		limiter.Reset(id)
	}
	return nil
}

func (s *Store) ResetUsageWindow(id string, window UsageResetWindow, expected *UsageResetExpectation) (UsageResetResult, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return UsageResetResult{}, errors.New("id is required")
	}
	if window != UsageResetDaily && window != UsageResetWeekly && window != UsageResetMonthly {
		return UsageResetResult{}, fmt.Errorf("%w: %q", ErrInvalidUsageResetWindow, window)
	}
	if expected != nil && (expected.StartedAt.IsZero() || expected.ResetAfterManualAt.IsZero()) {
		return UsageResetResult{}, ErrInvalidUsageResetExpectation
	}
	s.mu.RLock()
	_, exists := s.keys[id]
	usage, path, datasetID := s.usage, s.statePath, s.datasetID
	s.mu.RUnlock()
	if !exists {
		return UsageResetResult{}, ErrUnknownKey
	}
	if usage == nil {
		return UsageResetResult{KeyID: id, Window: window}, nil
	}
	s.persistMu.Lock()
	result, err := usage.resetWindowAndPersist(id, window, expected, func(snapshot map[string]*UsageState) error {
		return SaveUsage(policyPersist.UsagePath(path), datasetID, snapshot)
	})
	s.persistMu.Unlock()
	if err != nil {
		return UsageResetResult{}, fmt.Errorf("persist usage reset: %w", err)
	}
	s.recordAudit(audit.Event{Action: "reset_usage", KeyID: id, Changes: map[string]audit.Change{
		"window":        {From: "", To: string(window)},
		"daily_usd":     {From: result.BeforeDailyUSD, To: result.AfterDailyUSD},
		"weekly_usd":    {From: result.BeforeWeeklyUSD, To: result.AfterWeeklyUSD},
		"monthly_usd":   {From: result.BeforeMonthlyUSD, To: result.AfterMonthlyUSD},
		"next_reset_at": {To: result.NextAccountingBoundaryAt},
	}})
	return result, nil
}
