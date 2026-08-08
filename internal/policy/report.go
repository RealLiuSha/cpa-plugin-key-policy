package policy

import (
	"sort"
	"strings"
)

func (s *Store) Status() map[string]any {
	s.mu.RLock()
	enabled := s.enabled
	statePath := s.statePath
	keys := s.keysSnapshotLocked()
	limiter := s.limiter
	usage := s.usage
	s.mu.RUnlock()
	rpmUsage := map[string]int{}
	if limiter != nil {
		rpmUsage = limiter.Snapshot()
	}
	out := map[string]any{
		"enabled":    enabled,
		"state_file": statePath,
		"key_count":  len(keys),
		"rpm_usage":  rpmUsage,
		"usage":      usageSummaryForKeys(usage, keys),
	}
	return out
}

func usageSummaryForKeys(usage *usageLedger, keys []KeyConfig) map[string]UsageSummary {
	if usage == nil {
		return map[string]UsageSummary{}
	}
	out := make(map[string]UsageSummary, len(keys))
	for _, key := range keys {
		out[key.ID] = usage.Summary(key.ID, quotaLimitsForKey(key))
	}
	return out
}

func quotaLimitsForKey(key KeyConfig) quotaLimits {
	limits := quotaLimits{
		DailyUSD: key.DailyLimitUSD, WeeklyUSD: key.WeeklyLimitUSD,
		MonthlyUSD: key.MonthlyLimitUSD, LimitsChangedAt: key.LimitsChangedAt,
	}
	seen := make(map[string]struct{})
	for _, rule := range key.Models {
		canonical := strings.ToLower(strings.TrimSpace(rule.Alias))
		if canonical == "" || rule.AliasDailyLimitUSD <= 0 {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		limits.Aliases = append(limits.Aliases, aliasQuotaLimit{Alias: rule.Alias, DailyUSD: rule.AliasDailyLimitUSD})
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
		copy.Models = append([]ModelRule(nil), key.Models...)
		copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
		keys = append(keys, copy)
	}
	// Bug 5 fix: stable order by ID so list APIs and frontend rendering are
	// deterministic (Go map iteration is randomized). Doesn't affect which key
	// a button targets (bound by id), but prevents rows from jumping around.
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// aliasesSnapshotLocked returns a copy of the global alias table.
// Caller must hold s.mu.
func (s *Store) aliasesSnapshotLocked() []AliasMapping {
	out := make([]AliasMapping, 0, len(s.aliases))
	for _, a := range s.aliases {
		copy := *a
		copy.Targets = append([]AliasTarget(nil), a.Targets...)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// AliasesSnapshot returns a copy of the global alias table (thread-safe).
func (s *Store) AliasesSnapshot() []AliasMapping {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.aliasesSnapshotLocked()
}

// AliasWithRefs is an AliasMapping enriched with runtime key-reference stats.
// RefCount / RefKeys are never persisted; they are computed from the live key
// table for GET /aliases and import-prices responses.
// AliasMapping is embedded so new alias fields surface in the JSON response
// without a second hand-written copy (anonymous embed flattens in encoding/json).
type AliasWithRefs struct {
	AliasMapping
	RefCount int      `json:"ref_count"`
	RefKeys  []string `json:"ref_keys"`
}

// aliasRefIndexLocked builds lower(alias) → key IDs that reference it.
// Caller must hold s.mu (read or write).
func (s *Store) aliasRefIndexLocked() map[string][]string {
	idx := make(map[string][]string)
	for _, key := range s.keys {
		if key == nil {
			continue
		}
		seen := make(map[string]struct{})
		for _, ref := range key.Aliases {
			al := strings.ToLower(strings.TrimSpace(ref.Alias))
			if al == "" {
				continue
			}
			if _, dup := seen[al]; dup {
				continue
			}
			seen[al] = struct{}{}
			idx[al] = append(idx[al], key.ID)
		}
	}
	return idx
}

// AliasesSnapshotWithRefs returns the global alias table with per-alias
// reference counts derived from the live key table. ref_keys is always a
// non-nil slice (empty when unreferenced).
func (s *Store) AliasesSnapshotWithRefs() []AliasWithRefs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	aliases := s.aliasesSnapshotLocked()
	refs := s.aliasRefIndexLocked()
	out := make([]AliasWithRefs, 0, len(aliases))
	for _, a := range aliases {
		keys := refs[strings.ToLower(a.Alias)]
		if keys == nil {
			keys = []string{}
		}
		out = append(out, AliasWithRefs{
			AliasMapping: a,
			RefCount:     len(keys),
			RefKeys:      keys,
		})
	}
	return out
}

// AliasRefKeys returns the key IDs that reference the named alias (case-
// insensitive). The returned slice is never nil.
func (s *Store) AliasRefKeys(aliasName string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	refs := s.aliasRefIndexLocked()
	keys := refs[strings.ToLower(strings.TrimSpace(aliasName))]
	if keys == nil {
		return []string{}
	}
	return append([]string(nil), keys...)
}

// classifyRulesSnapshotLocked returns a copy of the classify rules.
// Caller must hold s.mu.
func (s *Store) classifyRulesSnapshotLocked() []ClassifyRule {
	return append([]ClassifyRule(nil), s.classifyRules...)
}

// ClassifyRulesSnapshot returns a copy of the classify rules (thread-safe).
func (s *Store) ClassifyRulesSnapshot() []ClassifyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.classifyRulesSnapshotLocked()
}

// UsageSummaryFor returns the current daily/7-day/30-day usage and limits for a key
// (for the keys-list management API).
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

// ResetUsage clears in-memory usage for a key (manual quota unlock).
func (s *Store) ResetUsage(id string) {
	_, usage := s.runtimeComponents()
	if usage != nil {
		usage.resetUsage(id)
	}
}

// AliasUsageFor returns a per-alias usage breakdown for the key with the given
// id, for the key detail management API. Returns the key config, the alias
// rows, and whether the key was found. Configured-but-unused aliases appear
// with zero values; ledger residuals for aliases no longer in the key's config
// appear with InConfig=false. Rows are sorted by alias.
func (s *Store) AliasUsageFor(keyID string) (KeyConfig, []AliasUsageEntry, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	_, usage := s.runtimeComponents()
	if usage == nil {
		rows := make([]AliasUsageEntry, 0, len(key.Models))
		for _, r := range key.Models {
			rows = append(rows, AliasUsageEntry{
				Alias:       r.Alias,
				Provider:    r.Provider,
				TargetModel: r.TargetModel,
				BillingMode: r.BillingMode,
				PerCallUSD:  r.PerCallUSD,
				InConfig:    true,
			})
		}
		return *key, rows, true
	}
	return *key, usage.AliasUsage(key.ID, key.Models), true
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
