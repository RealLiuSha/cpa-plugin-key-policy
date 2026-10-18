// Package legacyv6 converts a data format 6 dataset (written by 0.11.x
// releases) into a new format 7 dataset. It runs offline, before the upgrade,
// and is linked only into cmd/cpa-key-policy-check; the plugin itself reads
// format 7 only. Delete the package once no instance runs format 6.
package legacyv6

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

// Version is the data format 0.11.x releases wrote.
const Version = 6

// The structures below are the exact version 6 file layout, decoded
// strictly, so a file that was not written by a 0.11.x release is rejected
// rather than half-read.

type legacyState struct {
	Version   int           `json:"version"`
	DatasetID string        `json:"dataset_id"`
	Keys      []legacyKey   `json:"keys"`
	Models    []legacyModel `json:"models"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type legacyKey struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Enabled         bool             `json:"enabled"`
	KeyHash         string           `json:"key_hash"`
	KeyPreview      string           `json:"key_preview"`
	RPM             int              `json:"rpm"`
	Models          []legacyKeyModel `json:"models"`
	AllowModels     bool             `json:"allow_models_endpoint,omitempty"`
	DailyLimitUSD   float64          `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD  float64          `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD float64          `json:"monthly_limit_usd,omitempty"`
	LimitTimestamp  time.Time        `json:"limits_changed_at,omitempty"`
	CreatedAt       time.Time        `json:"created_at,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at,omitempty"`
}

type legacyKeyModel struct {
	Name          string  `json:"name"`
	DailyLimitUSD float64 `json:"daily_limit_usd,omitempty"`
}

type legacyModel struct {
	Name                      string   `json:"name"`
	Provider                  string   `json:"provider"`
	TargetModel               string   `json:"target_model"`
	BillingMode               string   `json:"billing_mode,omitempty"`
	BillingMultiplier         float64  `json:"billing_multiplier"`
	InputPricePerMillion      float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion     float64  `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion  float64  `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion *float64 `json:"cache_write_price_per_million,omitempty"`
	PerCallUSD                float64  `json:"per_call_usd,omitempty"`
}

type legacyUsageFile struct {
	Version   int                        `json:"version"`
	DatasetID string                     `json:"dataset_id"`
	Usage     map[string]*legacyUsageKey `json:"usage"`
	UpdatedAt time.Time                  `json:"updated_at"`
}

type legacyUsageKey struct {
	Days    map[string]legacyBucket            `json:"days"`
	ByModel map[string]map[string]legacyBucket `json:"by_model,omitempty"`
	Cycles  *legacyCycles                      `json:"cycles,omitempty"`
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
	TotalUSD         float64 `json:"total_usd,omitempty"`
	CallCount        int64   `json:"call_count,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheCostUSD     float64 `json:"cache_cost_usd,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CacheWriteUSD    float64 `json:"cache_write_usd,omitempty"`
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
}

func (b legacyBucket) bucket() policy.Bucket {
	return policy.Bucket{
		Calls: b.CallCount, InputTokens: b.InputTokens, CacheReadTokens: b.CacheReadTokens,
		CacheWriteTokens: b.CacheWriteTokens, OutputTokens: b.OutputTokens,
		ChargedNano: policy.USDToNano(b.TotalUSD), CacheReadCostNano: policy.USDToNano(b.CacheCostUSD), CacheWriteCostNano: policy.USDToNano(b.CacheWriteUSD),
	}
}

func (b legacyBucket) add(src legacyBucket) legacyBucket {
	b.TotalUSD += src.TotalUSD
	b.CallCount += src.CallCount
	b.CacheReadTokens += src.CacheReadTokens
	b.CacheCostUSD += src.CacheCostUSD
	b.CacheWriteTokens += src.CacheWriteTokens
	b.CacheWriteUSD += src.CacheWriteUSD
	b.InputTokens += src.InputTokens
	b.OutputTokens += src.OutputTokens
	return b
}

func (b legacyBucket) equals(other legacyBucket) bool {
	nearly := func(left, right float64) bool { return math.Abs(left-right) <= 1e-9 }
	return nearly(b.TotalUSD, other.TotalUSD) && b.CallCount == other.CallCount && b.CacheReadTokens == other.CacheReadTokens &&
		nearly(b.CacheCostUSD, other.CacheCostUSD) && b.CacheWriteTokens == other.CacheWriteTokens && nearly(b.CacheWriteUSD, other.CacheWriteUSD) &&
		b.InputTokens == other.InputTokens && b.OutputTokens == other.OutputTokens
}

// Convert reads the version 6 state at statePath and its paired usage file,
// and writes the version 7 dataset at outStatePath, whose files must not
// exist yet. The source files are only read. Nothing is written when the
// source does not convert cleanly.
func Convert(statePath, outStatePath, timezone string, now time.Time) (*Report, error) {
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	usageRaw, err := os.ReadFile(persist.UsagePath(statePath))
	if err != nil {
		return nil, fmt.Errorf("read usage paired with state: %w", err)
	}
	state, err := decodeState(stateRaw)
	if err != nil {
		return nil, err
	}
	usage, err := decodeUsage(usageRaw)
	if err != nil {
		return nil, err
	}
	if usage.DatasetID != state.DatasetID {
		return nil, fmt.Errorf("state/usage dataset_id mismatch: state=%q usage=%q", state.DatasetID, usage.DatasetID)
	}
	report := &Report{}
	rows := convertModels(state.Models, report)
	keys := convertKeys(state.Keys, state.Models, len(rows), report)
	converted, err := convertUsage(usage.Usage, timezone, report)
	if err != nil {
		return nil, fmt.Errorf("convert usage version %d: %w", Version, err)
	}
	if err := policy.WriteDataset(outStatePath, state.DatasetID, timezone, now, keys, rows, converted); err != nil {
		return nil, fmt.Errorf("write version 7 dataset: %w", err)
	}
	return report, nil
}

func decodeState(raw []byte) (*legacyState, error) {
	var disk legacyState
	if err := decodeStrict(raw, &disk, "state"); err != nil {
		return nil, err
	}
	disk.DatasetID = strings.TrimSpace(disk.DatasetID)
	return &disk, nil
}

func decodeUsage(raw []byte) (*legacyUsageFile, error) {
	var disk legacyUsageFile
	if err := decodeStrict(raw, &disk, "usage"); err != nil {
		return nil, err
	}
	disk.DatasetID = strings.TrimSpace(disk.DatasetID)
	if disk.Usage == nil {
		disk.Usage = make(map[string]*legacyUsageKey)
	}
	for id, usage := range disk.Usage {
		if usage == nil {
			return nil, fmt.Errorf("usage version %d key %q is null", Version, id)
		}
		if usage.Cycles == nil {
			return nil, fmt.Errorf("usage version %d key %q is missing quota cycles", Version, id)
		}
	}
	return &disk, nil
}

// decodeStrict checks the version and dataset header first, so a file in
// another format is named as such, then decodes the whole document
// rejecting unknown fields and trailing values.
func decodeStrict(raw []byte, target any, kind string) error {
	var header struct {
		Version   int    `json:"version"`
		DatasetID string `json:"dataset_id"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fmt.Errorf("decode %s header: %w", kind, err)
	}
	if header.Version != Version {
		return fmt.Errorf("%s version %d is not data format %d", kind, header.Version, Version)
	}
	if strings.TrimSpace(header.DatasetID) == "" {
		return fmt.Errorf("%s dataset_id is required", kind)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s version %d: %w", kind, Version, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode %s version %d: multiple JSON values", kind, Version)
	}
	return nil
}

// Report lists what the conversion dropped or chose.
type Report struct {
	IgnoredModelGrants       int
	KeysWithModelLists       int
	PartialScopeKeys         []string
	KeysWithoutModels        []string
	RemovedModelDailyLimits  int
	KeysWithoutModelsListing int
	RenamedModels            []string
	DroppedDuplicateRows     []string
	DayTotalMismatches       []string
	SharedSecrets            []string
}

const keysListed = 50

// Notes renders the report as one line per dropped setting or choice.
func (r Report) Notes() []string {
	notes := []string{}
	if r.KeysWithModelLists > 0 {
		note := fmt.Sprintf("ignored %d per-key model authorization(s) on %d key(s); every key can now use every model", r.IgnoredModelGrants, r.KeysWithModelLists)
		if len(r.PartialScopeKeys) > 0 {
			note += fmt.Sprintf("; %d key(s) were limited to a subset of the price rows: %s", len(r.PartialScopeKeys), listKeys(r.PartialScopeKeys))
		}
		notes = append(notes, note)
	}
	if len(r.KeysWithoutModels) > 0 {
		notes = append(notes, fmt.Sprintf("%d key(s) had no model authorization and could call no model; they now use every model: %s", len(r.KeysWithoutModels), listKeys(r.KeysWithoutModels)))
	}
	if r.RemovedModelDailyLimits > 0 {
		notes = append(notes, fmt.Sprintf("removed %d per-model daily limit(s); only key-level limits remain", r.RemovedModelDailyLimits))
	}
	if r.KeysWithoutModelsListing > 0 {
		notes = append(notes, fmt.Sprintf("%d key(s) had the model list endpoint disabled; every enabled key can now list models", r.KeysWithoutModelsListing))
	}
	for _, renamed := range r.RenamedModels {
		notes = append(notes, "price row keyed by upstream model id: "+renamed)
	}
	for _, dropped := range r.DroppedDuplicateRows {
		notes = append(notes, "dropped duplicate price row: "+dropped)
	}
	for _, mismatch := range r.DayTotalMismatches {
		notes = append(notes, "day total differed from its models and was recomputed from them: "+mismatch)
	}
	for _, shared := range r.SharedSecrets {
		notes = append(notes, "keys share one secret; only the first by id authenticates, rotate the other: "+shared)
	}
	return notes
}

func listKeys(ids []string) string {
	if len(ids) <= keysListed {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:keysListed], ", ") + ", …"
}

// rowKey groups price rows the way the plugin keys them: the upstream model
// id without case, and the provider without case or the OpenAI
// compatibility prefix. policy.WriteDataset rejects any pair it still finds
// duplicated.
func rowKey(model, provider string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible-")
}

// convertKeys drops the per-key model lists, the model list switch and the
// limit timestamp, and reports what each key loses. A key whose list covered
// fewer price rows than exist is named, since it gains access.
func convertKeys(keys []legacyKey, models []legacyModel, rowCount int, report *Report) []policy.KeyConfig {
	rowOfName := make(map[string]string, len(models))
	for _, model := range models {
		rowOfName[strings.ToLower(strings.TrimSpace(model.Name))] = rowKey(model.TargetModel, model.Provider)
	}
	converted := make([]policy.KeyConfig, 0, len(keys))
	secrets := make(map[string]string, len(keys))
	for _, key := range keys {
		if hash := strings.ToLower(strings.TrimSpace(key.KeyHash)); hash != "" {
			if first, exists := secrets[hash]; exists && first < key.ID {
				report.SharedSecrets = append(report.SharedSecrets, first+", "+key.ID)
			} else if exists {
				report.SharedSecrets = append(report.SharedSecrets, key.ID+", "+first)
				secrets[hash] = key.ID
			} else {
				secrets[hash] = key.ID
			}
		}
		converted = append(converted, policy.KeyConfig{
			ID: key.ID, Name: key.Name, Enabled: key.Enabled, KeyHash: key.KeyHash, KeyPreview: key.KeyPreview, RPM: key.RPM,
			DailyLimitUSD: key.DailyLimitUSD, WeeklyLimitUSD: key.WeeklyLimitUSD, MonthlyLimitUSD: key.MonthlyLimitUSD,
			CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt,
		})
		if !key.AllowModels {
			report.KeysWithoutModelsListing++
		}
		if len(key.Models) == 0 {
			if rowCount > 0 {
				report.KeysWithoutModels = append(report.KeysWithoutModels, key.ID)
			}
			continue
		}
		report.KeysWithModelLists++
		report.IgnoredModelGrants += len(key.Models)
		covered := make(map[string]struct{}, len(key.Models))
		for _, ref := range key.Models {
			if ref.DailyLimitUSD > 0 {
				report.RemovedModelDailyLimits++
			}
			if row, ok := rowOfName[strings.ToLower(strings.TrimSpace(ref.Name))]; ok {
				covered[row] = struct{}{}
			}
		}
		if len(covered) < rowCount {
			report.PartialScopeKeys = append(report.PartialScopeKeys, key.ID)
		}
	}
	return converted
}

// convertModels keys each price row by its upstream model id. When several
// public names pointed at the same upstream model, the row whose public name
// already was the upstream id wins, otherwise the first name in lexical
// order; the others are reported.
func convertModels(models []legacyModel, report *Report) []policy.ModelPrice {
	byTarget := make(map[string][]legacyModel)
	var order []string
	for _, model := range models {
		key := rowKey(model.TargetModel, model.Provider)
		if _, seen := byTarget[key]; !seen {
			order = append(order, key)
		}
		byTarget[key] = append(byTarget[key], model)
	}
	rows := make([]policy.ModelPrice, 0, len(order))
	for _, key := range order {
		candidates := byTarget[key]
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			leftExact, rightExact := strings.EqualFold(left.Name, left.TargetModel), strings.EqualFold(right.Name, right.TargetModel)
			if leftExact != rightExact {
				return leftExact
			}
			return left.Name < right.Name
		})
		kept := candidates[0]
		if !strings.EqualFold(kept.Name, kept.TargetModel) {
			report.RenamedModels = append(report.RenamedModels, fmt.Sprintf("%q is now %q (%s)", kept.Name, kept.TargetModel, kept.Provider))
		}
		for _, dropped := range candidates[1:] {
			report.DroppedDuplicateRows = append(report.DroppedDuplicateRows, fmt.Sprintf("%q also pointed at %s/%s, kept %q", dropped.Name, dropped.Provider, dropped.TargetModel, kept.Name))
		}
		var cacheWrite *float64
		if kept.CacheWritePricePerMillion != nil {
			price := *kept.CacheWritePricePerMillion
			cacheWrite = &price
		}
		rows = append(rows, policy.ModelPrice{
			Name: kept.TargetModel, Provider: kept.Provider, BillingMode: kept.BillingMode, BillingMultiplier: kept.BillingMultiplier,
			InputPricePerMillion: kept.InputPricePerMillion, OutputPricePerMillion: kept.OutputPricePerMillion,
			CacheReadPricePerMillion: kept.CacheReadPricePerMillion, CacheWritePricePerMillion: cacheWrite,
			PerCallUSD: kept.PerCallUSD,
		})
	}
	return rows
}

// convertUsage turns by_model[model][date] into days[date][model] with
// nanodollar amounts, verifies the redundant day totals before dropping
// them, and converts each quota cycle as it is.
func convertUsage(usage map[string]*legacyUsageKey, timezone string, report *Report) (map[string]*policy.KeyUsage, error) {
	keys := make(map[string]*policy.KeyUsage, len(usage))
	ids := make([]string, 0, len(usage))
	for id := range usage {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		legacy := usage[id]
		if legacy.Cycles.Timezone != timezone {
			return nil, fmt.Errorf("key %q uses quota timezone %q; keep usage_timezone consistent", id, legacy.Cycles.Timezone)
		}
		converted := &policy.KeyUsage{Days: make(map[string]map[string]policy.Bucket)}
		sums := make(map[string]legacyBucket)
		for model, days := range legacy.ByModel {
			for date, bucket := range days {
				models := converted.Days[date]
				if models == nil {
					models = make(map[string]policy.Bucket)
					converted.Days[date] = models
				}
				models[model] = bucket.bucket()
				sums[date] = sums[date].add(bucket)
			}
		}
		dates := make([]string, 0, len(legacy.Days))
		for date := range legacy.Days {
			dates = append(dates, date)
		}
		sort.Strings(dates)
		for _, date := range dates {
			if !legacy.Days[date].equals(sums[date]) {
				report.DayTotalMismatches = append(report.DayTotalMismatches, id+" "+date)
			}
		}
		converted.Cycles = &policy.Cycles{
			Daily: convertCycle(legacy.Cycles.Daily), Weekly: convertCycle(legacy.Cycles.Weekly), Monthly: convertCycle(legacy.Cycles.Monthly),
		}
		keys[id] = converted
	}
	return keys, nil
}

func convertCycle(source legacyCycle) policy.Cycle {
	cycle := policy.Cycle{StartedAt: source.StartedAt, ResetsAt: source.ResetsAt, ResetKind: source.ResetKind, ByModel: make(map[string]policy.Bucket, len(source.ByModel))}
	for model, bucket := range source.ByModel {
		cycle.ByModel[model] = bucket.bucket()
	}
	return cycle
}
