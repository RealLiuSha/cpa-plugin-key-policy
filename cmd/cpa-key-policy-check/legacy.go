package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

// legacyVersion is the data format 0.11.x releases wrote.
const legacyVersion = 6

// The structures below read the format 6 files independently of the
// plugin's converter, so the check compares the conversion with the source
// rather than with itself (internal/legacyv6). Only the fields the comparison needs are read.

type legacyPair struct {
	state *legacyState
	usage *legacyUsage
}

type legacyState struct {
	Version   int           `json:"version"`
	DatasetID string        `json:"dataset_id"`
	Keys      []legacyKey   `json:"keys"`
	Models    []legacyModel `json:"models"`
}

type legacyKey struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Enabled         bool             `json:"enabled"`
	KeyHash         string           `json:"key_hash"`
	KeyPreview      string           `json:"key_preview"`
	RPM             int              `json:"rpm"`
	Models          []legacyKeyModel `json:"models"`
	AllowModels     bool             `json:"allow_models_endpoint"`
	DailyLimitUSD   float64          `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64          `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64          `json:"monthly_limit_usd"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

type legacyKeyModel struct {
	Name          string  `json:"name"`
	DailyLimitUSD float64 `json:"daily_limit_usd"`
}

type legacyModel struct {
	Name                      string   `json:"name"`
	Provider                  string   `json:"provider"`
	TargetModel               string   `json:"target_model"`
	BillingMode               string   `json:"billing_mode"`
	BillingMultiplier         float64  `json:"billing_multiplier"`
	InputPricePerMillion      float64  `json:"input_price_per_million"`
	OutputPricePerMillion     float64  `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64  `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion *float64 `json:"cache_write_price_per_million"`
	PerCallUSD                float64  `json:"per_call_usd"`
}

type legacyUsage struct {
	Version   int                        `json:"version"`
	DatasetID string                     `json:"dataset_id"`
	Usage     map[string]*legacyKeyUsage `json:"usage"`
}

type legacyKeyUsage struct {
	Days    map[string]legacyBucket            `json:"days"`
	ByModel map[string]map[string]legacyBucket `json:"by_model"`
	Cycles  *legacyCycles                      `json:"cycles"`
}

type legacyCycles struct {
	Timezone string      `json:"timezone"`
	Daily    legacyCycle `json:"daily"`
	Weekly   legacyCycle `json:"weekly"`
	Monthly  legacyCycle `json:"monthly"`
}

type legacyCycle struct {
	StartedAt time.Time               `json:"started_at"`
	ResetsAt  time.Time               `json:"resets_at"`
	ResetKind string                  `json:"reset_kind"`
	ByModel   map[string]legacyBucket `json:"by_model"`
}

type legacyBucket struct {
	TotalUSD         float64 `json:"total_usd"`
	CallCount        int64   `json:"call_count"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheCostUSD     float64 `json:"cache_cost_usd"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheWriteUSD    float64 `json:"cache_write_usd"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
}

// bucket is the format 7 bucket the source bucket must become: counters
// unchanged, dollars rounded to nanodollars, no unpriced calls.
func (b legacyBucket) bucket() policy.Bucket {
	return policy.Bucket{
		Calls: b.CallCount, InputTokens: b.InputTokens, CacheReadTokens: b.CacheReadTokens,
		CacheWriteTokens: b.CacheWriteTokens, OutputTokens: b.OutputTokens,
		ChargedNano: policy.USDToNano(b.TotalUSD), CacheReadCostNano: policy.USDToNano(b.CacheCostUSD), CacheWriteCostNano: policy.USDToNano(b.CacheWriteUSD),
	}
}

func readLegacyPair(statePath string) (*legacyPair, error) {
	pair := &legacyPair{}
	if err := readJSON(statePath, &pair.state); err != nil {
		return nil, err
	}
	if pair.state.Version != legacyVersion {
		return nil, fmt.Errorf("state version %d is not %d", pair.state.Version, legacyVersion)
	}
	usagePath := persist.UsagePath(statePath)
	version, err := fileVersion(usagePath)
	if err != nil {
		return nil, err
	}
	if version != legacyVersion {
		return nil, fmt.Errorf("usage version %d is not %d; a format 6 state needs its format 6 usage file", version, legacyVersion)
	}
	if err := readJSON(usagePath, &pair.usage); err != nil {
		return nil, err
	}
	if pair.usage.DatasetID != pair.state.DatasetID {
		return nil, fmt.Errorf("source state/usage dataset_id mismatch: %q vs %q", pair.state.DatasetID, pair.usage.DatasetID)
	}
	return pair, nil
}

func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// verifyKeys checks that every key kept its identity, secret hash and
// policy fields; only the model list, the model list switch and the limit
// timestamp may disappear.
func (p *legacyPair) verifyKeys(state *policy.State) error {
	if state.DatasetID != p.state.DatasetID {
		return fmt.Errorf("dataset_id changed from %q to %q", p.state.DatasetID, state.DatasetID)
	}
	if len(state.Keys) != len(p.state.Keys) {
		return fmt.Errorf("key count changed from %d to %d", len(p.state.Keys), len(state.Keys))
	}
	byID := make(map[string]policy.KeyConfig, len(state.Keys))
	for _, key := range state.Keys {
		byID[key.ID] = key
	}
	for _, old := range p.state.Keys {
		want := policy.KeyConfig{
			ID: strings.TrimSpace(old.ID), Name: strings.TrimSpace(old.Name), Enabled: old.Enabled,
			KeyHash: strings.TrimSpace(old.KeyHash), KeyPreview: strings.TrimSpace(old.KeyPreview), RPM: old.RPM,
			DailyLimitUSD: old.DailyLimitUSD, WeeklyLimitUSD: old.WeeklyLimitUSD, MonthlyLimitUSD: old.MonthlyLimitUSD,
		}
		if want.Name == "" {
			want.Name = want.ID
		}
		got, ok := byID[want.ID]
		if !ok {
			return fmt.Errorf("key %q disappeared", want.ID)
		}
		if got.Name != want.Name || got.Enabled != want.Enabled || got.KeyHash != want.KeyHash || got.KeyPreview != want.KeyPreview || got.RPM != want.RPM ||
			got.DailyLimitUSD != want.DailyLimitUSD || got.WeeklyLimitUSD != want.WeeklyLimitUSD || got.MonthlyLimitUSD != want.MonthlyLimitUSD ||
			!got.CreatedAt.Equal(old.CreatedAt) || !got.UpdatedAt.Equal(old.UpdatedAt) {
			return fmt.Errorf("key %q identity or policy changed", want.ID)
		}
	}
	return nil
}

type priceRowKey struct{ name, provider string }

func normalizeProvider(provider string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible-")
}

func rowKey(name, provider string) priceRowKey {
	return priceRowKey{strings.ToLower(strings.TrimSpace(name)), normalizeProvider(provider)}
}

// expectedRows projects the format 6 models the way the conversion must:
// one row per (provider, upstream model id), keyed by the upstream id. When
// several public names pointed at one upstream model, the name equal to the
// upstream id wins, otherwise the first name in lexical order.
func (p *legacyPair) expectedRows() (map[priceRowKey]policy.ModelPrice, []string) {
	groups := make(map[priceRowKey][]legacyModel)
	var order []priceRowKey
	for _, model := range p.state.Models {
		key := rowKey(model.TargetModel, model.Provider)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], model)
	}
	rows := make(map[priceRowKey]policy.ModelPrice, len(order))
	var dropped []string
	for _, key := range order {
		candidates := groups[key]
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			leftExact, rightExact := strings.EqualFold(left.Name, left.TargetModel), strings.EqualFold(right.Name, right.TargetModel)
			if leftExact != rightExact {
				return leftExact
			}
			return left.Name < right.Name
		})
		kept := candidates[0]
		for _, other := range candidates[1:] {
			dropped = append(dropped, fmt.Sprintf("%s -> %s/%s (kept %s)", other.Name, other.Provider, other.TargetModel, kept.Name))
		}
		mode := strings.ToLower(strings.TrimSpace(kept.BillingMode))
		if mode == "" {
			mode = "tokens"
		}
		multiplier := kept.BillingMultiplier
		if multiplier == 0 {
			multiplier = 1
		}
		var cacheWrite *float64
		if kept.CacheWritePricePerMillion != nil {
			value := *kept.CacheWritePricePerMillion
			cacheWrite = &value
		}
		rows[key] = policy.ModelPrice{
			Name: strings.TrimSpace(kept.TargetModel), Provider: normalizeProvider(kept.Provider), BillingMode: mode, BillingMultiplier: multiplier,
			InputPricePerMillion: kept.InputPricePerMillion, OutputPricePerMillion: kept.OutputPricePerMillion,
			CacheReadPricePerMillion: kept.CacheReadPricePerMillion, CacheWritePricePerMillion: cacheWrite, PerCallUSD: kept.PerCallUSD,
		}
	}
	return rows, dropped
}

func (p *legacyPair) verifyPrices(state *policy.State) error {
	expected, _ := p.expectedRows()
	if len(state.Models) != len(expected) {
		return fmt.Errorf("price row count is %d, want %d rows projected from %d models", len(state.Models), len(expected), len(p.state.Models))
	}
	for _, got := range state.Models {
		want, ok := expected[rowKey(got.Name, got.Provider)]
		if !ok {
			return fmt.Errorf("price row %q/%q has no source model", got.Provider, got.Name)
		}
		if got.Name != want.Name || got.Provider != want.Provider || got.BillingMode != want.BillingMode || got.BillingMultiplier != want.BillingMultiplier ||
			got.InputPricePerMillion != want.InputPricePerMillion || got.OutputPricePerMillion != want.OutputPricePerMillion ||
			got.CacheReadPricePerMillion != want.CacheReadPricePerMillion || got.PerCallUSD != want.PerCallUSD ||
			(got.CacheWritePricePerMillion == nil) != (want.CacheWritePricePerMillion == nil) ||
			(got.CacheWritePricePerMillion != nil && *got.CacheWritePricePerMillion != *want.CacheWritePricePerMillion) {
			return fmt.Errorf("price row %q/%q differs from its source model", got.Provider, got.Name)
		}
	}
	return nil
}

// verifyHistory checks every (key, model, day) bucket inside retention
// against the source and that nothing else appeared. It returns how many
// (key, day) pairs were kept and how many expired.
func (p *legacyPair) verifyHistory(converted *policy.UsageDataset, oldest string) (int, int, error) {
	preserved := make(map[[2]string]struct{})
	expired := make(map[[2]string]struct{})
	for id, usage := range p.usage.Usage {
		if usage == nil {
			continue
		}
		target := converted.Keys[id]
		for model, days := range usage.ByModel {
			for date, bucket := range days {
				pair := [2]string{id, date}
				if date < oldest {
					expired[pair] = struct{}{}
					if target != nil {
						if _, kept := target.Days[date][model]; kept {
							return 0, 0, fmt.Errorf("key %q kept %s %q, which is outside the %d-day retention", id, date, model, policy.UsageRetentionDays)
						}
					}
					continue
				}
				preserved[pair] = struct{}{}
				if target == nil {
					return 0, 0, fmt.Errorf("key %q usage disappeared", id)
				}
				got, ok := target.Days[date][model]
				if !ok {
					return 0, 0, fmt.Errorf("key %q lost %s %q", id, date, model)
				}
				if got != bucket.bucket() {
					return 0, 0, fmt.Errorf("key %q %s %q changed: %+v, want %+v", id, date, model, got, bucket.bucket())
				}
			}
		}
	}
	for id, usage := range converted.Keys {
		source := p.usage.Usage[id]
		if source == nil && len(usage.Days) > 0 {
			return 0, 0, fmt.Errorf("key %q has history without a source", id)
		}
		for date, models := range usage.Days {
			for model := range models {
				if _, ok := source.ByModel[model][date]; !ok || date < oldest {
					return 0, 0, fmt.Errorf("key %q gained %s %q", id, date, model)
				}
			}
		}
	}
	return len(preserved), len(expired), nil
}

// verifyCycles checks that each quota period kept its interval, reset kind
// and per-model consumption.
func (p *legacyPair) verifyCycles(converted *policy.UsageDataset) error {
	for id, usage := range p.usage.Usage {
		if usage == nil || usage.Cycles == nil {
			continue
		}
		target := converted.Keys[id]
		if target == nil || target.Cycles == nil {
			return fmt.Errorf("key %q lost its quota periods", id)
		}
		for _, window := range []struct {
			name string
			old  legacyCycle
			got  policy.Cycle
		}{{"daily", usage.Cycles.Daily, target.Cycles.Daily}, {"weekly", usage.Cycles.Weekly, target.Cycles.Weekly}, {"monthly", usage.Cycles.Monthly, target.Cycles.Monthly}} {
			if !window.got.StartedAt.Equal(window.old.StartedAt) || !window.got.ResetsAt.Equal(window.old.ResetsAt) || window.got.ResetKind != window.old.ResetKind {
				return fmt.Errorf("key %q %s period interval or kind changed", id, window.name)
			}
			if len(window.got.ByModel) != len(window.old.ByModel) {
				return fmt.Errorf("key %q %s period model set changed", id, window.name)
			}
			for model, bucket := range window.old.ByModel {
				if got, ok := window.got.ByModel[model]; !ok || got != bucket.bucket() {
					return fmt.Errorf("key %q %s period consumption of %q changed", id, window.name, model)
				}
			}
		}
	}
	return nil
}

// renamedModels lists the source models whose public name differed from
// the upstream model id; their price rows are now keyed by the upstream id.
func (p *legacyPair) renamedModels() []string {
	renamed := []string{}
	for _, model := range p.state.Models {
		if !strings.EqualFold(model.Name, model.TargetModel) {
			renamed = append(renamed, model.Name+" -> "+model.Provider+"/"+model.TargetModel)
		}
	}
	return renamed
}

// summary counts the settings the conversion drops, from the source itself.
func (p *legacyPair) summary() *conversionSummary {
	rows, dropped := p.expectedRows()
	rowOfName := make(map[string]priceRowKey, len(p.state.Models))
	for _, model := range p.state.Models {
		rowOfName[strings.ToLower(strings.TrimSpace(model.Name))] = rowKey(model.TargetModel, model.Provider)
	}
	summary := &conversionSummary{PartialScopeKeys: []string{}, DroppedDuplicateRows: dropped}
	if summary.DroppedDuplicateRows == nil {
		summary.DroppedDuplicateRows = []string{}
	}
	for _, key := range p.state.Keys {
		if !key.AllowModels {
			summary.KeysWithoutModelsListing++
		}
		if len(key.Models) == 0 {
			continue
		}
		summary.KeysWithModelLists++
		summary.IgnoredModelGrants += len(key.Models)
		covered := make(map[priceRowKey]struct{}, len(key.Models))
		for _, ref := range key.Models {
			if ref.DailyLimitUSD > 0 {
				summary.RemovedModelDailyLimits++
			}
			if row, ok := rowOfName[strings.ToLower(strings.TrimSpace(ref.Name))]; ok {
				covered[row] = struct{}{}
			}
		}
		if len(covered) < len(rows) {
			summary.PartialScopeKeyCount++
			if len(summary.PartialScopeKeys) < partialScopeKeysListed {
				summary.PartialScopeKeys = append(summary.PartialScopeKeys, key.ID)
			}
		}
	}
	return summary
}
