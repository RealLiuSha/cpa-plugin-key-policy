package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/policy/audit"
	policyPersist "cpa-key-policy/internal/policy/persist"
)

func (s *Store) updateAliasesLocked(aliases []AliasMapping) {
	s.aliases = make(map[string]*AliasMapping, len(aliases))
	for i := range aliases {
		s.aliases[strings.ToLower(aliases[i].Alias)] = &aliases[i]
	}
}

func (s *Store) UpsertKey(input KeyConfig, persist bool) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	// Build a config that includes the store's current global alias table
	// and classify rules, so normalizeConfig can validate the key's alias
	// references and migrate any per-key Models into existing or new aliases.
	s.mu.RLock()
	existingAliases := s.aliasesSnapshotLocked()
	existingRules := s.classifyRulesSnapshotLocked()
	s.mu.RUnlock()
	cfg := Config{Enabled: true, StateFile: s.StatePath(), Keys: []KeyConfig{input}, Aliases: existingAliases, ClassifyRules: existingRules}
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	key := cfg.Keys[0]
	now := time.Now().UTC()
	s.mu.Lock()
	old := s.keys[key.ID]
	var previous *KeyConfig
	if old != nil {
		copy := *old
		copy.Aliases = append([]KeyAliasRef(nil), old.Aliases...)
		previous = &copy
	}
	if old != nil && !old.CreatedAt.IsZero() {
		key.CreatedAt = old.CreatedAt
	} else if key.CreatedAt.IsZero() {
		key.CreatedAt = now
	}
	if old == nil {
		if key.DailyLimitUSD > 0 || key.WeeklyLimitUSD > 0 || key.MonthlyLimitUSD > 0 || hasAliasLimit(key.Aliases) {
			key.LimitsChangedAt = now
		}
	} else if !limitsEqual(*old, key) {
		key.LimitsChangedAt = now
	} else {
		key.LimitsChangedAt = old.LimitsChangedAt
	}
	key.UpdatedAt = now
	// Populate Models from the key's Alias refs + global table for downstream use.
	if len(key.Aliases) > 0 {
		aliasLookup := make(map[string]*AliasMapping, len(cfg.Aliases))
		for i := range cfg.Aliases {
			aliasLookup[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
		}
		key.Models = resolveAliasRefsToModels(key.Aliases, aliasLookup)
	}
	s.keys[key.ID] = &key
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(key.ID)
	// Update the store's global alias table if migration added new aliases.
	s.updateAliasesLocked(cfg.Aliases)
	keys := s.keysSnapshotLocked()
	path := s.statePath
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	s.mu.Unlock()
	if persist {
		if err := s.saveState(path, keys, aliases, rules); err != nil {
			return err
		}
		action := "create_key"
		if previous != nil {
			action = "update_key"
		}
		s.recordAudit(audit.Event{Action: action, KeyID: key.ID, Changes: keyLimitChanges(previous, key)})
	}
	return nil
}

func keyLimitChanges(previous *KeyConfig, current KeyConfig) map[string]audit.Change {
	changes := make(map[string]audit.Change)
	before := KeyConfig{}
	if previous != nil {
		before = *previous
	}
	for field, values := range map[string][2]float64{
		"daily_limit_usd":   {before.DailyLimitUSD, current.DailyLimitUSD},
		"weekly_limit_usd":  {before.WeeklyLimitUSD, current.WeeklyLimitUSD},
		"monthly_limit_usd": {before.MonthlyLimitUSD, current.MonthlyLimitUSD},
	} {
		if values[0] != values[1] {
			changes[field] = audit.Change{From: values[0], To: values[1]}
		}
	}
	beforeAliases := make(map[string]float64, len(before.Aliases))
	for _, ref := range before.Aliases {
		beforeAliases[strings.ToLower(ref.Alias)] = ref.DailyLimitUSD
	}
	afterAliases := make(map[string]float64, len(current.Aliases))
	for _, ref := range current.Aliases {
		afterAliases[strings.ToLower(ref.Alias)] = ref.DailyLimitUSD
	}
	beforeRaw, _ := json.Marshal(beforeAliases)
	afterRaw, _ := json.Marshal(afterAliases)
	if string(beforeRaw) != string(afterRaw) {
		changes["alias_daily_limits"] = audit.Change{From: beforeAliases, To: afterAliases}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

func hasAliasLimit(refs []KeyAliasRef) bool {
	for _, ref := range refs {
		if ref.DailyLimitUSD > 0 {
			return true
		}
	}
	return false
}

func limitsEqual(a, b KeyConfig) bool {
	if a.DailyLimitUSD != b.DailyLimitUSD || a.WeeklyLimitUSD != b.WeeklyLimitUSD || a.MonthlyLimitUSD != b.MonthlyLimitUSD {
		return false
	}
	aLimits := make(map[string]float64, len(a.Aliases))
	for _, ref := range a.Aliases {
		aLimits[strings.ToLower(ref.Alias)] = ref.DailyLimitUSD
	}
	if len(aLimits) != len(b.Aliases) {
		return false
	}
	for _, ref := range b.Aliases {
		if value, ok := aLimits[strings.ToLower(ref.Alias)]; !ok || value != ref.DailyLimitUSD {
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
	s.mu.Lock()
	if _, ok := s.keys[id]; !ok {
		s.mu.Unlock()
		return ErrUnknownKey
	}
	delete(s.keys, id)
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(id)
	keys := s.keysSnapshotLocked()
	path := s.statePath
	limiter := s.limiter
	usageLedger := s.usage
	s.mu.Unlock()
	if limiter != nil {
		limiter.Reset(id)
	}
	if usageLedger != nil {
		usageLedger.resetUsage(id)
	}
	if err := s.saveState(path, keys, s.AliasesSnapshot(), s.ClassifyRulesSnapshot()); err != nil {
		return err
	}
	s.recordAudit(audit.Event{Action: "delete_key", KeyID: id})
	return nil
}

func (s *Store) RotateKey(id string) (string, KeyConfig, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
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
	s.mu.Lock()
	key := s.keys[id]
	if key == nil {
		s.mu.Unlock()
		return "", KeyConfig{}, ErrUnknownKey
	}
	key.KeyHash = hash
	key.KeyPreview = PreviewKey(plain)
	key.UpdatedAt = time.Now().UTC()
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(id)
	keys := s.keysSnapshotLocked()
	path := s.statePath
	limiter := s.limiter
	s.mu.Unlock()
	if limiter != nil {
		limiter.Reset(id)
	}
	if err := s.saveState(path, keys, s.AliasesSnapshot(), s.ClassifyRulesSnapshot()); err != nil {
		return "", KeyConfig{}, err
	}
	s.recordAudit(audit.Event{Action: "rotate_key", KeyID: id})
	return plain, copy, nil
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

// ResetUsageWindow clears one key's selected rolling window and persists the
// change before returning. Wider resets also clear every nested shorter window.
func (s *Store) ResetUsageWindow(id string, window UsageResetWindow) (UsageResetResult, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return UsageResetResult{}, errors.New("id is required")
	}
	if window != UsageResetDaily && window != UsageResetWeekly && window != UsageResetMonthly {
		return UsageResetResult{}, fmt.Errorf("%w: %q", ErrInvalidUsageResetWindow, window)
	}

	s.mu.RLock()
	_, exists := s.keys[id]
	usage := s.usage
	path := s.statePath
	s.mu.RUnlock()
	if !exists {
		return UsageResetResult{}, ErrUnknownKey
	}
	if usage == nil {
		return UsageResetResult{KeyID: id, Window: window}, nil
	}

	// The flusher takes persistMu before snapshotting the ledger, so take the
	// same lock order here. The ledger owns its reset, snapshot, and rollback;
	// the Store supplies only the file persistence boundary.
	s.persistMu.Lock()
	result, err := usage.resetWindowAndPersist(id, window, func(snapshot map[string]*UsageState) error {
		return SaveUsage(policyPersist.UsagePath(path), snapshot)
	})
	s.persistMu.Unlock()
	if err != nil {
		return UsageResetResult{}, fmt.Errorf("persist usage reset: %w", err)
	}
	s.recordAudit(audit.Event{Action: "reset_usage", KeyID: id, Changes: map[string]audit.Change{"window": {From: "", To: string(window)}}})
	return result, nil
}

// --- Global alias mapping table management ---

// UpsertAlias adds or replaces an alias in the global table. Validates the
// alias (non-empty name, at least one target, valid dispatch/billing mode).
// Persists the full state to disk.
func (s *Store) UpsertAlias(alias AliasMapping) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	// Build a temp config to validate the single alias.
	existing := s.AliasesSnapshot()
	// Replace or append.
	found := false
	for i, a := range existing {
		if strings.EqualFold(a.Alias, alias.Alias) {
			existing[i] = alias
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, alias)
	}
	tmp := Config{Enabled: true, StateFile: s.StatePath(), Aliases: existing}
	if err := normalizeConfig(&tmp); err != nil {
		return err
	}
	s.mu.Lock()
	s.updateAliasesLocked(tmp.Aliases)
	// Re-resolve all keys' Models from the updated alias table.
	s.resolveAllModelsLocked()
	keys := s.keysSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if err := s.saveState(path, keys, s.AliasesSnapshot(), s.ClassifyRulesSnapshot()); err != nil {
		return err
	}
	action := "create_alias"
	if found {
		action = "update_alias"
	}
	s.recordAudit(audit.Event{Action: action, Changes: map[string]audit.Change{"alias": {From: alias.Alias, To: alias.Alias}}})
	return nil
}

// DeleteAlias removes an alias from the global table. Returns an error if any
// key still references it (must remove references first).
func (s *Store) DeleteAlias(aliasName string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	aliasName = strings.TrimSpace(aliasName)
	if aliasName == "" {
		return errors.New("alias name is required")
	}
	// Check for key references.
	s.mu.RLock()
	refCount := 0
	for _, key := range s.keys {
		for _, ref := range key.Aliases {
			if strings.EqualFold(ref.Alias, aliasName) {
				refCount++
			}
		}
	}
	s.mu.RUnlock()
	if refCount > 0 {
		return errors.New("alias is referenced by " + itoa(refCount) + " key(s); remove references first")
	}
	existing := s.AliasesSnapshot()
	filtered := existing[:0]
	for _, a := range existing {
		if !strings.EqualFold(a.Alias, aliasName) {
			filtered = append(filtered, a)
		}
	}
	s.mu.Lock()
	s.updateAliasesLocked(filtered)
	keys := s.keysSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if err := s.saveState(path, keys, s.AliasesSnapshot(), s.ClassifyRulesSnapshot()); err != nil {
		return err
	}
	s.recordAudit(audit.Event{Action: "delete_alias", Changes: map[string]audit.Change{"alias": {From: aliasName, To: ""}}})
	return nil
}

// --- Batch price import (Models.dev-style matches) ---

// PriceImportMatch is one row from a Models.dev-style models.json export.
// Units are already USD per million tokens (same as AliasMapping). cache_write
// is accepted for wire compatibility but intentionally ignored.
// Pointer price fields: nil means "field absent — keep existing value on apply".
// A present 0 is a deliberate zero (unlike a missing key).
type PriceImportMatch struct {
	Model                string   `json:"model"`
	PromptPricePer1M     *float64 `json:"prompt_price_per_1m"`
	CompletionPricePer1M *float64 `json:"completion_price_per_1m"`
	CacheReadPricePer1M  *float64 `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M *float64 `json:"cache_write_price_per_1m"`
}

// PriceImportApplied is one alias whose token prices were (or would be) updated.
type PriceImportApplied struct {
	Alias                    string  `json:"alias"`
	OldInputPricePerMillion  float64 `json:"old_input_price_per_million"`
	OldOutputPricePerMillion float64 `json:"old_output_price_per_million"`
	OldCacheReadPerMillion   float64 `json:"old_cache_read_price_per_million"`
	NewInputPricePerMillion  float64 `json:"new_input_price_per_million"`
	NewOutputPricePerMillion float64 `json:"new_output_price_per_million"`
	NewCacheReadPerMillion   float64 `json:"new_cache_read_price_per_million"`
	// Note carries non-fatal context, e.g. per_call billing leaves token prices dormant.
	Note string `json:"note,omitempty"`
}

// PriceImportUnchanged is an alias that matched but already had the same prices.
type PriceImportUnchanged struct {
	Alias string `json:"alias"`
}

// PriceImportSkipped is a match or alias that could not be applied.
type PriceImportSkipped struct {
	// Model is the import match model name when the skip is match-centric.
	Model string `json:"model,omitempty"`
	// Alias is set when a known alias was considered but rejected (e.g. conflict).
	Alias  string `json:"alias,omitempty"`
	Reason string `json:"reason"`
}

// PriceImportResult is the full response of ImportAliasPrices.
type PriceImportResult struct {
	Applied      []PriceImportApplied   `json:"applied"`
	Unchanged    []PriceImportUnchanged `json:"unchanged"`
	Skipped      []PriceImportSkipped   `json:"skipped"`
	AffectedKeys []string               `json:"affected_keys"`
}

// importPricePatch is a sparse token-price update: nil field = leave unchanged.
type importPricePatch struct {
	in, out, cache *float64
}

func (p importPricePatch) empty() bool {
	return p.in == nil && p.out == nil && p.cache == nil
}

func importPatchesEqual(a, b importPricePatch) bool {
	return ptrFloatEqual(a.in, b.in) && ptrFloatEqual(a.out, b.out) && ptrFloatEqual(a.cache, b.cache)
}

func ptrFloatEqual(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// importPriceTriple is the fully resolved three token prices after merge.
type importPriceTriple struct {
	in, out, cache float64
}

func importPricesEqual(a, b importPriceTriple) bool {
	return a.in == b.in && a.out == b.out && a.cache == b.cache
}

func mergeImportPatch(old importPriceTriple, p importPricePatch) importPriceTriple {
	r := old
	if p.in != nil {
		r.in = *p.in
	}
	if p.out != nil {
		r.out = *p.out
	}
	if p.cache != nil {
		r.cache = *p.cache
	}
	return r
}

func matchToPatch(m PriceImportMatch) importPricePatch {
	// cache_write intentionally ignored.
	return importPricePatch{
		in:    m.PromptPricePer1M,
		out:   m.CompletionPricePer1M,
		cache: m.CacheReadPricePer1M,
	}
}

// ImportAliasPrices batch-updates token prices on existing global aliases from
// Models.dev-style matches. Matching: target_model exact (case-insensitive)
// first; if no target hits, alias name fallback. Multi-target aliases that hit
// different prices are skipped with target_price_conflict. cache_write is
// ignored. Only existing aliases are updated — nothing is created.
// Omitted price fields in a match keep the alias's current value (not zeroed).
//
// dry_run=true computes the same classification without mutating or persisting.
// dry_run=false applies all updates under one lock, one resolveAllModelsLocked,
// and one saveState (never loops UpsertAlias).
func (s *Store) ImportAliasPrices(matches []PriceImportMatch, dryRun bool) (PriceImportResult, error) {
	result := PriceImportResult{
		Applied:      []PriceImportApplied{},
		Unchanged:    []PriceImportUnchanged{},
		Skipped:      []PriceImportSkipped{},
		AffectedKeys: []string{},
	}
	// Index matches by lower(model). Last wins on duplicate model names.
	byModel := make(map[string]importPricePatch, len(matches))
	matchOrder := make([]string, 0, len(matches))
	seenModel := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		name := strings.TrimSpace(m.Model)
		if name == "" {
			continue
		}
		p := matchToPatch(m)
		if p.empty() {
			continue
		}
		lk := strings.ToLower(name)
		byModel[lk] = p
		if _, ok := seenModel[lk]; !ok {
			seenModel[lk] = struct{}{}
			matchOrder = append(matchOrder, lk)
		}
	}

	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	s.mu.Lock()
	aliases := s.aliasesSnapshotLocked()
	refIdx := s.aliasRefIndexLocked()

	// Track which match models hit at least one alias (for no_match reporting).
	matchedModels := make(map[string]struct{})
	// pending updates: alias lower name → new prices + applied record
	type pendingUpdate struct {
		idx  int
		newP importPriceTriple
		rec  PriceImportApplied
	}
	var pending []pendingUpdate
	affectedSet := make(map[string]struct{})

	for i := range aliases {
		a := &aliases[i]
		// Collect patches hit via target_model.
		var hitPatches []importPricePatch
		var hitModels []string
		for _, t := range a.Targets {
			lk := strings.ToLower(strings.TrimSpace(t.TargetModel))
			if p, ok := byModel[lk]; ok {
				hitPatches = append(hitPatches, p)
				hitModels = append(hitModels, lk)
			}
		}
		var chosen *importPricePatch
		if len(hitPatches) == 0 {
			// Alias-name fallback.
			if p, ok := byModel[strings.ToLower(a.Alias)]; ok {
				cp := p
				chosen = &cp
				matchedModels[strings.ToLower(a.Alias)] = struct{}{}
			}
		} else {
			// All hit targets must agree on one sparse patch.
			first := hitPatches[0]
			conflict := false
			for _, p := range hitPatches[1:] {
				if !importPatchesEqual(p, first) {
					conflict = true
					break
				}
			}
			for _, m := range hitModels {
				matchedModels[m] = struct{}{}
			}
			if conflict {
				result.Skipped = append(result.Skipped, PriceImportSkipped{
					Alias:  a.Alias,
					Reason: "target_price_conflict",
				})
				continue
			}
			// If some targets missed while others hit, still apply the agreed
			// price from the hits only when every target hit. Partial hit → skip.
			if len(hitPatches) != len(a.Targets) {
				result.Skipped = append(result.Skipped, PriceImportSkipped{
					Alias:  a.Alias,
					Reason: "partial_target_match",
				})
				continue
			}
			cp := first
			chosen = &cp
		}
		if chosen == nil {
			continue // alias not related to this import batch
		}
		old := importPriceTriple{a.InputPricePerMillion, a.OutputPricePerMillion, a.CacheReadPricePerMillion}
		merged := mergeImportPatch(old, *chosen)
		if importPricesEqual(old, merged) {
			result.Unchanged = append(result.Unchanged, PriceImportUnchanged{Alias: a.Alias})
			continue
		}
		rec := PriceImportApplied{
			Alias:                    a.Alias,
			OldInputPricePerMillion:  old.in,
			OldOutputPricePerMillion: old.out,
			OldCacheReadPerMillion:   old.cache,
			NewInputPricePerMillion:  merged.in,
			NewOutputPricePerMillion: merged.out,
			NewCacheReadPerMillion:   merged.cache,
		}
		if strings.EqualFold(strings.TrimSpace(a.BillingMode), "per_call") {
			rec.Note = "billing_mode is per_call; imported token prices are stored but not billed until mode switches to tokens"
		}
		pending = append(pending, pendingUpdate{idx: i, newP: merged, rec: rec})
		for _, kid := range refIdx[strings.ToLower(a.Alias)] {
			affectedSet[kid] = struct{}{}
		}
	}

	// Matches that never hit any alias → no_match.
	for _, m := range matchOrder {
		if _, ok := matchedModels[m]; !ok {
			result.Skipped = append(result.Skipped, PriceImportSkipped{
				Model:  m,
				Reason: "no_match",
			})
		}
	}

	for _, p := range pending {
		result.Applied = append(result.Applied, p.rec)
	}
	for kid := range affectedSet {
		result.AffectedKeys = append(result.AffectedKeys, kid)
	}
	sort.Strings(result.AffectedKeys)

	if dryRun || len(pending) == 0 {
		s.mu.Unlock()
		return result, nil
	}

	// Apply all price updates in memory, re-resolve keys once, save once.
	for _, p := range pending {
		aliases[p.idx].InputPricePerMillion = p.newP.in
		aliases[p.idx].OutputPricePerMillion = p.newP.out
		aliases[p.idx].CacheReadPricePerMillion = p.newP.cache
		// billing_mode intentionally untouched (including per_call).
	}
	s.updateAliasesLocked(aliases)
	s.resolveAllModelsLocked()
	keys := s.keysSnapshotLocked()
	path := s.statePath
	rules := s.classifyRulesSnapshotLocked()
	s.mu.Unlock()
	if err := s.saveState(path, keys, s.AliasesSnapshot(), rules); err != nil {
		return result, err
	}
	for _, applied := range result.Applied {
		s.recordAudit(audit.Event{Action: "update_alias", Changes: map[string]audit.Change{
			"alias":                        {From: applied.Alias, To: applied.Alias},
			"input_price_per_million":      {From: applied.OldInputPricePerMillion, To: applied.NewInputPricePerMillion},
			"output_price_per_million":     {From: applied.OldOutputPricePerMillion, To: applied.NewOutputPricePerMillion},
			"cache_read_price_per_million": {From: applied.OldCacheReadPerMillion, To: applied.NewCacheReadPerMillion},
		}})
	}
	return result, nil
}

// --- Classification rule management ---

// UpsertClassifyRule adds or replaces a classification rule. Validates the
// regex pattern. Persists the full state to disk.
func (s *Store) UpsertClassifyRule(rule ClassifyRule) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	existing := s.ClassifyRulesSnapshot()
	found := false
	for i, r := range existing {
		if strings.EqualFold(r.Name, rule.Name) {
			existing[i] = rule
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, rule)
	}
	tmp := Config{Enabled: true, StateFile: s.StatePath(), ClassifyRules: existing}
	if err := normalizeConfig(&tmp); err != nil {
		return err
	}
	s.mu.Lock()
	s.classifyRules = tmp.ClassifyRules
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	if err := s.saveState(path, keys, aliases, rules); err != nil {
		return err
	}
	action := "create_classify_rule"
	if found {
		action = "update_classify_rule"
	}
	s.recordAudit(audit.Event{Action: action, Changes: map[string]audit.Change{"rule": {From: rule.Name, To: rule.Name}}})
	return nil
}

// DeleteClassifyRule removes a classification rule by name.
func (s *Store) DeleteClassifyRule(name string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("rule name is required")
	}
	existing := s.ClassifyRulesSnapshot()
	filtered := existing[:0]
	for _, r := range existing {
		if !strings.EqualFold(r.Name, name) {
			filtered = append(filtered, r)
		}
	}
	s.mu.Lock()
	s.classifyRules = filtered
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	if err := s.saveState(path, keys, aliases, rules); err != nil {
		return err
	}
	s.recordAudit(audit.Event{Action: "delete_classify_rule", Changes: map[string]audit.Change{"rule": {From: name, To: ""}}})
	return nil
}

// ReorderClassifyRules reorders the classification rules to match the given
// name order. Rules not in the list keep their relative order at the end.
func (s *Store) ReorderClassifyRules(names []string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	existing := s.ClassifyRulesSnapshot()
	byName := make(map[string]ClassifyRule)
	for _, r := range existing {
		byName[strings.ToLower(r.Name)] = r
	}
	var reordered []ClassifyRule
	used := make(map[string]bool)
	for _, name := range names {
		if r, ok := byName[strings.ToLower(name)]; ok {
			reordered = append(reordered, r)
			used[strings.ToLower(name)] = true
		}
	}
	for _, r := range existing {
		if !used[strings.ToLower(r.Name)] {
			reordered = append(reordered, r)
		}
	}
	s.mu.Lock()
	s.classifyRules = reordered
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	if err := s.saveState(path, keys, aliases, rules); err != nil {
		return err
	}
	s.recordAudit(audit.Event{Action: "reorder_classify_rules"})
	return nil
}

// resolveAllModelsLocked re-populates every key's Models from its Aliases
// refs + the global alias table. Caller must hold s.mu.
func (s *Store) resolveAllModelsLocked() {
	for _, key := range s.keys {
		if len(key.Aliases) > 0 {
			key.Models = resolveAliasRefsToModels(key.Aliases, s.aliases)
		}
	}
}

// itoa is a minimal int→string to avoid importing strconv in this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SetOnClassifyRulesChanged registers a callback fired when classify rules
// change, so the plugin can clear its classify cache.
func (s *Store) SetOnClassifyRulesChanged(fn func()) {
	s.mu.Lock()
	s.onClassifyRulesChanged = fn
	s.mu.Unlock()
}

// callClassifyRulesChanged invokes a callback captured while holding s.mu.
// It must be called after releasing s.mu because callbacks may re-enter Store.
func callClassifyRulesChanged(fn func()) {
	if fn != nil {
		fn()
	}
}
