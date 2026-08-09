package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/policy"
)

type Counts struct {
	Keys             int `json:"keys"`
	Models           int `json:"models"`
	ModelReferences  int `json:"model_references"`
	DateBuckets      int `json:"date_buckets"`
	ModelDateBuckets int `json:"model_date_buckets"`
}

type Report struct {
	Status           string                                 `json:"status"`
	StateSHA256      string                                 `json:"state_sha256"`
	UsageSHA256      string                                 `json:"usage_sha256"`
	DatasetID        string                                 `json:"dataset_id,omitempty"`
	PlannedBackupDir string                                 `json:"planned_backup_dir,omitempty"`
	Counts           Counts                                 `json:"counts"`
	Before           map[string]policy.UsageMigrationTotals `json:"before"`
	After            map[string]policy.UsageMigrationTotals `json:"after"`
	Summary          UsageReportSummary                     `json:"summary"`
}

type UsageReportSummary struct {
	Before policy.UsageMigrationTotals `json:"before"`
	After  policy.UsageMigrationTotals `json:"after"`
}

type Conversion struct {
	Keys      []policy.KeyConfig
	Models    []policy.ModelDefinition
	Rules     []policy.ClassifyRule
	Usage     map[string]*policy.UsageState
	DatasetID string
	Report    Report
	AlreadyV3 bool
}

func Convert(stateRaw, usageRaw []byte, freeModels []string, now time.Time, location *time.Location, timezone, backupDir string) (*Conversion, error) {
	stateVersion, err := documentVersion(stateRaw)
	if err != nil {
		return nil, fmt.Errorf("read state version: %w", err)
	}
	usageVersion, err := documentVersion(usageRaw)
	if err != nil {
		return nil, fmt.Errorf("read usage version: %w", err)
	}
	if stateVersion != usageVersion {
		return nil, fmt.Errorf("state/usage version mismatch: state=%d usage=%d", stateVersion, usageVersion)
	}
	stateSHA := sha256Hex(stateRaw)
	usageSHA := sha256Hex(usageRaw)
	if stateVersion == 3 {
		return convertAlreadyV3(stateRaw, usageRaw, stateSHA, usageSHA, now, location, timezone, backupDir)
	}
	if stateVersion != 2 {
		if stateVersion <= 1 {
			return nil, fmt.Errorf("v%d input is not supported; migrate it to v2 with the previous plugin tool first", stateVersion)
		}
		return nil, fmt.Errorf("version %d is newer than this migrator supports", stateVersion)
	}
	legacyState, err := decodeState(stateRaw)
	if err != nil {
		return nil, fmt.Errorf("decode v2 state: %w", err)
	}
	legacyUsage, err := decodeUsage(usageRaw)
	if err != nil {
		return nil, fmt.Errorf("decode v2 usage: %w", err)
	}
	if legacyState.Version != 2 || legacyUsage.Version != 2 {
		return nil, errors.New("both state and usage must declare version 2")
	}
	if legacyUsage.Usage == nil {
		legacyUsage.Usage = make(map[string]*UsageState)
	}
	freeSet := make(map[string]struct{}, len(freeModels))
	for _, name := range freeModels {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			freeSet[name] = struct{}{}
		}
	}
	models, modelIndex, err := convertModels(legacyState.Aliases, freeSet)
	if err != nil {
		return nil, err
	}
	for name := range freeSet {
		if _, exists := modelIndex[name]; !exists {
			return nil, fmt.Errorf("-free-model %q does not match any v2 model", name)
		}
	}
	keys, refCount, err := convertKeys(legacyState.Keys, modelIndex)
	if err != nil {
		return nil, err
	}
	usage, dateBuckets, modelDateBuckets, err := convertUsage(legacyUsage.Usage, modelIndex)
	if err != nil {
		return nil, err
	}
	if location == nil {
		location = time.UTC
	}
	before := summarizeLegacyUsageStates(legacyUsage.Usage, now, location, timezone)
	after := policy.SummarizeUsageStates(usage, now, location, timezone)
	if err := validateWindowTotals(before, after); err != nil {
		return nil, err
	}
	summary := UsageReportSummary{Before: sumUsageTotals(before), After: sumUsageTotals(after)}
	datasetHash := sha256.New()
	_, _ = datasetHash.Write(stateRaw)
	_, _ = datasetHash.Write([]byte{0})
	_, _ = datasetHash.Write(usageRaw)
	datasetID := hex.EncodeToString(datasetHash.Sum(nil)[:16])
	if _, err := policy.MarshalState(datasetID, keys, models, legacyState.ClassifyRules, now.UTC()); err != nil {
		return nil, fmt.Errorf("validate converted state: %w", err)
	}
	if _, err := policy.MarshalUsage(datasetID, usage, now.UTC()); err != nil {
		return nil, fmt.Errorf("validate converted usage: %w", err)
	}
	return &Conversion{
		Keys: keys, Models: models, Rules: legacyState.ClassifyRules, Usage: usage, DatasetID: datasetID,
		Report: Report{
			Status: "ready", StateSHA256: stateSHA, UsageSHA256: usageSHA, DatasetID: datasetID,
			PlannedBackupDir: backupDir,
			Counts:           Counts{Keys: len(keys), Models: len(models), ModelReferences: refCount, DateBuckets: dateBuckets, ModelDateBuckets: modelDateBuckets},
			Before:           before, After: after, Summary: summary,
		},
	}, nil
}

func summarizeLegacyUsageStates(states map[string]*UsageState, now time.Time, location *time.Location, timezone string) map[string]policy.UsageMigrationTotals {
	legacyDays := make(map[string]*policy.UsageState, len(states))
	for keyID, state := range states {
		if state == nil {
			continue
		}
		days := make(map[string]policy.UsageBucket, len(state.Days))
		for date, bucket := range state.Days {
			days[date] = bucket
		}
		legacyDays[keyID] = &policy.UsageState{Days: days}
	}
	return policy.SummarizeUsageStates(legacyDays, now, location, timezone)
}

func convertAlreadyV3(stateRaw, usageRaw []byte, stateSHA, usageSHA string, now time.Time, location *time.Location, timezone, backupDir string) (*Conversion, error) {
	var state struct {
		Version       int                      `json:"version"`
		DatasetID     string                   `json:"dataset_id"`
		Keys          []policy.KeyConfig       `json:"keys"`
		Models        []policy.ModelDefinition `json:"models"`
		ClassifyRules []policy.ClassifyRule    `json:"classify_rules,omitempty"`
		UpdatedAt     time.Time                `json:"updated_at"`
	}
	var usage struct {
		Version   int                           `json:"version"`
		DatasetID string                        `json:"dataset_id"`
		Usage     map[string]*policy.UsageState `json:"usage"`
		UpdatedAt time.Time                     `json:"updated_at"`
	}
	if err := decodeStrictJSON(stateRaw, &state); err != nil {
		return nil, err
	}
	if err := decodeStrictJSON(usageRaw, &usage); err != nil {
		return nil, err
	}
	if strings.TrimSpace(state.DatasetID) == "" || state.DatasetID != usage.DatasetID {
		return nil, errors.New("v3 state/usage dataset_id mismatch")
	}
	if err := policy.ValidateUsageStates(usage.Usage); err != nil {
		return nil, err
	}
	if _, err := policy.MarshalState(state.DatasetID, state.Keys, state.Models, state.ClassifyRules, state.UpdatedAt); err != nil {
		return nil, fmt.Errorf("validate v3 state: %w", err)
	}
	if _, err := policy.MarshalUsage(usage.DatasetID, usage.Usage, usage.UpdatedAt); err != nil {
		return nil, fmt.Errorf("validate v3 usage: %w", err)
	}
	if location == nil {
		location = time.UTC
	}
	totals := policy.SummarizeUsageStates(usage.Usage, now, location, timezone)
	summary := UsageReportSummary{Before: sumUsageTotals(totals), After: sumUsageTotals(totals)}
	refs := 0
	for _, key := range state.Keys {
		refs += len(key.Models)
	}
	dateBuckets, modelDateBuckets := countBuckets(usage.Usage)
	return &Conversion{
		DatasetID: state.DatasetID, AlreadyV3: true,
		Report: Report{
			Status: "already_migrated", StateSHA256: stateSHA, UsageSHA256: usageSHA,
			DatasetID: state.DatasetID, PlannedBackupDir: backupDir,
			Counts: Counts{Keys: len(state.Keys), Models: len(state.Models), ModelReferences: refs, DateBuckets: dateBuckets, ModelDateBuckets: modelDateBuckets},
			Before: totals, After: totals, Summary: summary,
		},
	}, nil
}

func decodeStrictJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON document must contain exactly one value")
	}
	return nil
}

func documentVersion(raw []byte) (int, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, err
	}
	if header.Version == 0 {
		return 1, nil
	}
	return header.Version, nil
}

func convertModels(aliases []AliasMapping, freeSet map[string]struct{}) ([]policy.ModelDefinition, map[string]policy.ModelDefinition, error) {
	models := make([]policy.ModelDefinition, 0, len(aliases))
	index := make(map[string]policy.ModelDefinition, len(aliases))
	for _, legacy := range aliases {
		name := strings.TrimSpace(legacy.Alias)
		canonical := strings.ToLower(name)
		if name == "" {
			return nil, nil, errors.New("v2 alias name is required")
		}
		if _, duplicate := index[canonical]; duplicate {
			return nil, nil, fmt.Errorf("conflicting v2 model name %q", name)
		}
		_, free := freeSet[canonical]
		billingMode := strings.ToLower(strings.TrimSpace(legacy.BillingMode))
		if billingMode == "" {
			billingMode = "tokens"
		}
		if free && (legacy.InputPricePerMillion != 0 || legacy.OutputPricePerMillion != 0 || legacy.CacheReadPricePerMillion != 0 || legacy.PerCallUSD != 0) {
			return nil, nil, fmt.Errorf("-free-model %q is only valid when every v2 price field is zero", name)
		}
		if !free && ((billingMode == "per_call" && legacy.PerCallUSD == 0) || (billingMode == "tokens" && legacy.InputPricePerMillion == 0 && legacy.OutputPricePerMillion == 0 && legacy.CacheReadPricePerMillion == 0)) {
			return nil, nil, fmt.Errorf("v2 model %q has zero active prices; pass -free-model %q only after confirming it is intentionally free", name, name)
		}
		model := policy.ModelDefinition{
			Name: name, Dispatch: legacy.Dispatch, BillingMode: billingMode, Free: free,
			InputPricePerMillion: legacy.InputPricePerMillion, OutputPricePerMillion: legacy.OutputPricePerMillion,
			CacheReadPricePerMillion: legacy.CacheReadPricePerMillion, PerCallUSD: legacy.PerCallUSD,
		}
		if free {
			model.InputPricePerMillion = 0
			model.OutputPricePerMillion = 0
			model.CacheReadPricePerMillion = 0
			model.PerCallUSD = 0
		}
		for _, target := range legacy.Targets {
			model.Targets = append(model.Targets, policy.ModelTarget{Provider: target.Provider, TargetModel: target.TargetModel, Group: target.Group})
		}
		models = append(models, model)
		index[canonical] = model
	}
	sort.Slice(models, func(i, j int) bool { return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name) })
	return models, index, nil
}

func convertKeys(legacyKeys []KeyConfig, modelIndex map[string]policy.ModelDefinition) ([]policy.KeyConfig, int, error) {
	keys := make([]policy.KeyConfig, 0, len(legacyKeys))
	refCount := 0
	for _, legacy := range legacyKeys {
		if len(legacy.Models) > 0 {
			return nil, 0, fmt.Errorf("key %q contains persisted derived models; normalize it with v2 before migration", legacy.ID)
		}
		key := policy.KeyConfig{
			ID: legacy.ID, Name: legacy.Name, Enabled: legacy.Enabled, KeyHash: legacy.KeyHash, KeyPreview: legacy.KeyPreview,
			RPM: legacy.RPM, AllowModelsEndpoint: legacy.AllowModelsEndpoint,
			DailyLimitUSD: legacy.DailyLimitUSD, WeeklyLimitUSD: legacy.WeeklyLimitUSD, MonthlyLimitUSD: legacy.MonthlyLimitUSD,
			LimitsChangedAt: legacy.LimitsChangedAt, CreatedAt: legacy.CreatedAt, UpdatedAt: legacy.UpdatedAt,
		}
		seen := make(map[string]struct{}, len(legacy.Aliases))
		for _, ref := range legacy.Aliases {
			canonical := strings.ToLower(strings.TrimSpace(ref.Alias))
			model, exists := modelIndex[canonical]
			if !exists {
				return nil, 0, fmt.Errorf("key %q references unknown v2 model %q", legacy.ID, ref.Alias)
			}
			if _, duplicate := seen[canonical]; duplicate {
				return nil, 0, fmt.Errorf("key %q contains duplicate model reference %q", legacy.ID, ref.Alias)
			}
			seen[canonical] = struct{}{}
			if err := validateOverrides(ref, model); err != nil {
				return nil, 0, fmt.Errorf("key %q model %q: %w", legacy.ID, ref.Alias, err)
			}
			key.Models = append(key.Models, policy.KeyModelRef{Name: model.Name, DailyLimitUSD: ref.DailyLimitUSD})
			refCount++
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, refCount, nil
}

func validateOverrides(ref KeyAliasRef, model policy.ModelDefinition) error {
	checks := []struct {
		name string
		got  *float64
		want float64
	}{
		{"input_price_per_million", ref.InputPricePerMillion, model.InputPricePerMillion},
		{"output_price_per_million", ref.OutputPricePerMillion, model.OutputPricePerMillion},
		{"cache_read_price_per_million", ref.CacheReadPricePerMillion, model.CacheReadPricePerMillion},
		{"per_call_usd", ref.PerCallUSD, model.PerCallUSD},
	}
	for _, check := range checks {
		if check.got != nil && *check.got != check.want {
			return fmt.Errorf("price override %s=%v differs from global value %v and cannot be represented in v3", check.name, *check.got, check.want)
		}
	}
	return nil
}

func convertUsage(legacy map[string]*UsageState, modelIndex map[string]policy.ModelDefinition) (map[string]*policy.UsageState, int, int, error) {
	result := make(map[string]*policy.UsageState, len(legacy))
	for keyID, state := range legacy {
		if state == nil {
			return nil, 0, 0, fmt.Errorf("key %q has null usage state", keyID)
		}
		converted := &policy.UsageState{Days: make(map[string]policy.UsageBucket, len(state.Days)), ByModel: make(map[string]map[string]policy.UsageBucket)}
		for date, bucket := range state.Days {
			if err := validateBucket(bucket); err != nil {
				return nil, 0, 0, fmt.Errorf("key %q date %q: %w", keyID, date, err)
			}
			converted.Days[date] = bucket
		}
		for legacyName, days := range state.ByAlias {
			model, exists := modelIndex[strings.ToLower(strings.TrimSpace(legacyName))]
			if !exists {
				return nil, 0, 0, fmt.Errorf("key %q usage references unknown v2 model %q", keyID, legacyName)
			}
			modelDays := converted.ByModel[model.Name]
			if modelDays == nil {
				modelDays = make(map[string]policy.UsageBucket)
				converted.ByModel[model.Name] = modelDays
			}
			for date, bucket := range days {
				if err := validateBucket(bucket); err != nil {
					return nil, 0, 0, fmt.Errorf("key %q model %q date %q: %w", keyID, legacyName, date, err)
				}
				modelDays[date] = addBucket(modelDays[date], bucket)
			}
		}
		result[keyID] = converted
	}
	if err := policy.ValidateUsageStates(result); err != nil {
		return nil, 0, 0, fmt.Errorf("v2 days/by_alias reconciliation failed: %w", err)
	}
	dateBuckets, modelDateBuckets := countBuckets(result)
	return result, dateBuckets, modelDateBuckets, nil
}

func validateBucket(bucket policy.UsageBucket) error {
	if bucket.TotalUSD < 0 || bucket.CacheCostUSD < 0 || bucket.CallCount < 0 || bucket.CacheReadTokens < 0 || bucket.InputTokens < 0 || bucket.OutputTokens < 0 {
		return errors.New("usage counters cannot be negative")
	}
	return nil
}

func addBucket(left, right policy.UsageBucket) policy.UsageBucket {
	left.TotalUSD += right.TotalUSD
	left.CallCount += right.CallCount
	left.CacheReadTokens += right.CacheReadTokens
	left.CacheCostUSD += right.CacheCostUSD
	left.InputTokens += right.InputTokens
	left.OutputTokens += right.OutputTokens
	return left
}

func countBuckets(states map[string]*policy.UsageState) (int, int) {
	dateBuckets, modelDateBuckets := 0, 0
	for _, state := range states {
		if state == nil {
			continue
		}
		dateBuckets += len(state.Days)
		for _, days := range state.ByModel {
			modelDateBuckets += len(days)
		}
	}
	return dateBuckets, modelDateBuckets
}

func validateWindowTotals(before, after map[string]policy.UsageMigrationTotals) error {
	if len(before) != len(after) {
		return errors.New("usage total key count changed during migration")
	}
	for keyID, left := range before {
		right, exists := after[keyID]
		if !exists || left.DailyUSD != right.DailyUSD || left.WeeklyUSD != right.WeeklyUSD || !floatPointerEqual(left.MonthlyUSD, right.MonthlyUSD) {
			return fmt.Errorf("key %q usage totals changed during migration", keyID)
		}
		if left.DailyUSD > left.WeeklyUSD || (left.MonthlyUSD != nil && left.WeeklyUSD > *left.MonthlyUSD) {
			return fmt.Errorf("key %q violates daily <= 7-day <= 30-day", keyID)
		}
	}
	return nil
}

func sumUsageTotals(totals map[string]policy.UsageMigrationTotals) policy.UsageMigrationTotals {
	monthly := 0.0
	result := policy.UsageMigrationTotals{MonthlyUSD: &monthly}
	keyIDs := make([]string, 0, len(totals))
	for keyID := range totals {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	for _, keyID := range keyIDs {
		total := totals[keyID]
		result.DailyUSD += total.DailyUSD
		result.WeeklyUSD += total.WeeklyUSD
		if total.MonthlyUSD != nil {
			monthly += *total.MonthlyUSD
		}
	}
	return result
}

func floatPointerEqual(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return math.Abs(*left-*right) <= 1e-9
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
