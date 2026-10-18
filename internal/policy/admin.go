package policy

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"cpa-key-policy/internal/policy/audit"
	"cpa-key-policy/internal/policy/persist"
)

// UpsertKey creates or updates a key, persisting before publishing. A
// secret another key already uses is rejected with ErrDuplicateKeyHash.
func (s *Store) UpsertKey(input KeyConfig) error {
	previous, stored, err := s.applyKeyMutation(input)
	if err != nil {
		return err
	}
	action := "create_key"
	if previous != nil {
		action = "update_key"
	}
	s.recordAudit(audit.Event{Action: action, KeyID: stored.ID, Changes: keyChanges(previous, stored)})
	return nil
}

func (s *Store) applyKeyMutation(input KeyConfig) (*KeyConfig, KeyConfig, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	s.mu.RUnlock()

	now := s.clock()().UTC()
	input.ID = strings.TrimSpace(input.ID)
	var previous *KeyConfig
	index := -1
	for i := range keys {
		if keys[i].ID == input.ID {
			copy := keys[i]
			previous = &copy
			index = i
			break
		}
	}
	if previous != nil && !previous.CreatedAt.IsZero() {
		input.CreatedAt = previous.CreatedAt
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = now
	}
	input.UpdatedAt = now
	if index >= 0 {
		keys[index] = input
	} else {
		keys = append(keys, input)
		index = len(keys) - 1
	}
	if err := normalizeKeys(keys); err != nil {
		return nil, KeyConfig{}, err
	}
	if err := secretUnique(keys, keys[index]); err != nil {
		return nil, KeyConfig{}, err
	}
	input = keys[index]
	if err := s.saveState(path, datasetID, keys, models); err != nil {
		return nil, KeyConfig{}, err
	}
	// Periods exist before the key is published, so a usage record that
	// arrives right after publication is counted against its quota.
	s.mu.RLock()
	ledger := s.usage
	s.mu.RUnlock()
	ledger.openCycles(input)
	s.mu.Lock()
	copy := input
	s.keys[input.ID] = &copy
	s.rebuildKeysByHashLocked()
	s.mu.Unlock()
	return previous, input, nil
}

// keyChanges lists the managed fields an upsert changed. Secrets, hashes
// and previews are never recorded.
func keyChanges(previous *KeyConfig, current KeyConfig) map[string]audit.Change {
	before := KeyConfig{}
	if previous != nil {
		before = *previous
	}
	changes := make(map[string]audit.Change)
	record := func(field string, from, to any) {
		if from != to {
			changes[field] = audit.Change{From: from, To: to}
		}
	}
	record("name", before.Name, current.Name)
	record("enabled", before.Enabled, current.Enabled)
	record("rpm", before.RPM, current.RPM)
	record("daily_limit_usd", before.DailyLimitUSD, current.DailyLimitUSD)
	record("weekly_limit_usd", before.WeeklyLimitUSD, current.WeeklyLimitUSD)
	record("monthly_limit_usd", before.MonthlyLimitUSD, current.MonthlyLimitUSD)
	record("quota_mode", before.Mode(), current.Mode())
	if len(changes) == 0 {
		return nil
	}
	return changes
}

// DeleteKey removes the key, its quota periods, balance and RPM window. Its
// daily history stays until retention drops it and is reported as deleted.
func (s *Store) DeleteKey(id string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	s.mu.RLock()
	keys := s.keysSnapshotLocked()
	models := s.prices.snapshot()
	path, datasetID := s.statePath, s.datasetID
	_, exists := s.keys[id]
	limiter, ledger := s.limiter, s.usage
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
	if err := s.saveState(path, datasetID, filtered, models); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.keys, id)
	s.rebuildKeysByHashLocked()
	s.mu.Unlock()
	limiter.Reset(id)
	daily, weekly, monthly, balance := ledger.forgetKey(id)
	changes := map[string]audit.Change{
		"daily_usd":   {From: daily.USD(), To: nil},
		"weekly_usd":  {From: weekly.USD(), To: nil},
		"monthly_usd": {From: monthly.USD(), To: nil},
	}
	if balance != 0 {
		changes["balance_usd"] = audit.Change{From: balance.USD(), To: nil}
	}
	s.recordAudit(audit.Event{Action: "delete_key", KeyID: id, Changes: changes})
	// The live file follows now rather than at the next tick: a key recreated
	// under this id before then, followed by a crash, would find the cleared
	// periods on restart and inherit them. TRADEOFF: the state write is not
	// rolled back when this fails and the delete still reports success; the
	// key is gone either way, the failure is logged, counted in health and
	// retried by the flusher.
	if err := s.FlushUsage(); err != nil {
		log.Printf("cpa-key-policy: usage flush after deleting key %q failed, retrying next interval: %v", id, err)
	}
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
	_, stored, err := s.applyKeyMutation(*key)
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
	limiter.Reset(id)
	return nil
}

// ResetUsageWindow starts a new quota period now. With expected set, the
// reset only happens when the period and proposed deadline still match
// what the caller displayed (ErrUsageResetChanged otherwise).
func (s *Store) ResetUsageWindow(id string, window UsageResetWindow, expected *UsageResetExpectation) (UsageResetResult, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return UsageResetResult{}, errors.New("id is required")
	}
	if !window.valid() {
		return UsageResetResult{}, fmt.Errorf("%w: %q", ErrInvalidUsageResetWindow, window)
	}
	if expected != nil && (expected.StartedAt.IsZero() || expected.ResetAfterManualAt.IsZero()) {
		return UsageResetResult{}, ErrInvalidUsageResetExpectation
	}
	s.mu.RLock()
	_, exists := s.keys[id]
	ledger, path, datasetID := s.usage, s.statePath, s.datasetID
	s.mu.RUnlock()
	if !exists {
		return UsageResetResult{}, ErrUnknownKey
	}
	// The callback runs under the ledger lock, so the clock is read here.
	writtenAt := s.clock()()
	s.persistMu.Lock()
	result, err := ledger.resetWindowAndPersist(id, window, expected, func(through string, keys map[string]liveKey) error {
		return writeLive(persist.UsagePath(path), datasetID, ledger.timezone, through, keys, writtenAt)
	})
	s.persistMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrUsageResetChanged) {
			return UsageResetResult{}, err
		}
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
