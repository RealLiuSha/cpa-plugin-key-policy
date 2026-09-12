package policy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

const (
	currentStateFileVersion = 5
	currentUsageFileVersion = 5
	minReadableFileVersion  = 3
	maxReadableFileVersion  = 5
)

type persistedState struct {
	Version       int               `json:"version"`
	DatasetID     string            `json:"dataset_id"`
	Keys          []KeyConfig       `json:"keys"`
	Models        []ModelDefinition `json:"models"`
	ClassifyRules []ClassifyRule    `json:"classify_rules,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type persistedUsage struct {
	Version   int                    `json:"version"`
	DatasetID string                 `json:"dataset_id"`
	Usage     map[string]*UsageState `json:"usage"`
	UpdatedAt time.Time              `json:"updated_at"`
}

type UsageFile struct {
	Version   int
	DatasetID string
	Usage     map[string]*UsageState
	UpdatedAt time.Time
}

func ResolveStatePath(path string) (string, error) {
	return persist.ResolvePath(path, DefaultConfig().StateFile)
}

func NewDatasetID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate dataset id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func LoadState(path string) (*State, error) {
	if err := persist.CleanupStaleTemps(path, time.Now()); err != nil {
		return nil, err
	}
	raw, err := persist.Read(path)
	if err != nil {
		return nil, err
	}
	return decodeState(raw)
}

func decodeState(raw []byte) (*State, error) {
	var header struct {
		Version   int    `json:"version"`
		DatasetID string `json:"dataset_id"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("decode state header: %w", err)
	}
	if header.Version < minReadableFileVersion || header.Version > maxReadableFileVersion {
		return nil, schemaVersionError("state", header.Version)
	}
	if strings.TrimSpace(header.DatasetID) == "" {
		return nil, errors.New("state dataset_id is required")
	}
	var disk persistedState
	if err := decodeJSONStrict(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode current state: %w", err)
	}
	cfg := Config{Enabled: true, Keys: disk.Keys, Models: disk.Models, ClassifyRules: disk.ClassifyRules}
	if err := normalizeConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate current state: %w", err)
	}
	return &State{
		Version:       disk.Version,
		DatasetID:     strings.TrimSpace(disk.DatasetID),
		Keys:          cfg.Keys,
		Models:        cfg.Models,
		ClassifyRules: cfg.ClassifyRules,
		UpdatedAt:     disk.UpdatedAt,
	}, nil
}

func LoadUsage(path string) (*UsageFile, error) {
	raw, err := persist.Read(path)
	if err != nil {
		return nil, err
	}
	return decodeUsage(raw)
}

func decodeUsage(raw []byte) (*UsageFile, error) {
	var header struct {
		Version   int    `json:"version"`
		DatasetID string `json:"dataset_id"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("decode usage header: %w", err)
	}
	if header.Version < minReadableFileVersion || header.Version > maxReadableFileVersion {
		return nil, schemaVersionError("usage", header.Version)
	}
	if strings.TrimSpace(header.DatasetID) == "" {
		return nil, errors.New("usage dataset_id is required")
	}
	var disk persistedUsage
	if err := decodeJSONStrict(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode current usage: %w", err)
	}
	if disk.Usage == nil {
		disk.Usage = make(map[string]*UsageState)
	}
	if err := ValidateUsageStates(disk.Usage); err != nil {
		return nil, fmt.Errorf("validate current usage: %w", err)
	}
	if disk.Version >= 5 {
		for id, state := range disk.Usage {
			if state.Cycles == nil {
				return nil, fmt.Errorf("v5 key %q is missing quota cycles", id)
			}
		}
	}
	return &UsageFile{
		Version:   disk.Version,
		DatasetID: strings.TrimSpace(disk.DatasetID),
		Usage:     disk.Usage,
		UpdatedAt: disk.UpdatedAt,
	}, nil
}

func schemaVersionError(kind string, version int) error {
	return fmt.Errorf("unsupported %s version %d; require version %d through %d", kind, version, minReadableFileVersion, maxReadableFileVersion)
}

func decodeJSONStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func SaveState(path, datasetID string, keys []KeyConfig, models []ModelDefinition, rules []ClassifyRule) error {
	raw, err := MarshalState(datasetID, keys, models, rules, time.Now().UTC())
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

func MarshalState(datasetID string, keys []KeyConfig, models []ModelDefinition, rules []ClassifyRule, updatedAt time.Time) ([]byte, error) {
	datasetID = strings.TrimSpace(datasetID)
	if datasetID == "" {
		return nil, errors.New("state dataset_id is required")
	}
	cfg := Config{Enabled: true, Keys: append([]KeyConfig(nil), keys...), Models: append([]ModelDefinition(nil), models...), ClassifyRules: append([]ClassifyRule(nil), rules...)}
	if err := normalizeConfig(&cfg); err != nil {
		return nil, err
	}
	state := persistedState{
		Version: currentStateFileVersion, DatasetID: datasetID, Keys: cfg.Keys, Models: cfg.Models,
		ClassifyRules: cfg.ClassifyRules, UpdatedAt: updatedAt.UTC(),
	}
	return json.MarshalIndent(state, "", "  ")
}

func SaveUsage(path, datasetID string, usage map[string]*UsageState) error {
	raw, err := MarshalUsage(datasetID, usage, time.Now().UTC())
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

func MarshalUsage(datasetID string, usage map[string]*UsageState, updatedAt time.Time) ([]byte, error) {
	datasetID = strings.TrimSpace(datasetID)
	if datasetID == "" {
		return nil, errors.New("usage dataset_id is required")
	}
	if err := ValidateUsageStates(usage); err != nil {
		return nil, err
	}
	for id, state := range usage {
		if state.Cycles == nil {
			return nil, fmt.Errorf("cannot save v5 key %q without quota cycles", id)
		}
	}
	disk := persistedUsage{
		Version: currentUsageFileVersion, DatasetID: datasetID, Usage: usage, UpdatedAt: updatedAt.UTC(),
	}
	return json.MarshalIndent(disk, "", "  ")
}

func ValidateUsageStates(states map[string]*UsageState) error {
	keyIDs := make([]string, 0, len(states))
	for keyID := range states {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	for _, keyID := range keyIDs {
		state := states[keyID]
		if state == nil {
			return fmt.Errorf("key %q has null usage state", keyID)
		}
		if err := validateUsageCycles(state.Cycles); err != nil {
			return fmt.Errorf("key %q: %w", keyID, err)
		}
		if state.Days == nil {
			state.Days = make(map[string]UsageBucket)
		}
		if state.ByModel == nil {
			state.ByModel = make(map[string]map[string]UsageBucket)
		}
		modelNames := make([]string, 0, len(state.ByModel))
		for name := range state.ByModel {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("key %q has empty by_model name", keyID)
			}
			modelNames = append(modelNames, name)
		}
		sort.Strings(modelNames)
		allDates := make(map[string]struct{}, len(state.Days))
		for date := range state.Days {
			allDates[date] = struct{}{}
		}
		for _, name := range modelNames {
			for date := range state.ByModel[name] {
				allDates[date] = struct{}{}
			}
		}
		for date := range allDates {
			var sum UsageBucket
			for _, name := range modelNames {
				sum = addUsageBucket(sum, state.ByModel[name][date])
			}
			if !usageBucketsEqual(state.Days[date], sum) {
				return fmt.Errorf("key %q date %q days bucket does not equal by_model sum", keyID, date)
			}
		}
	}
	return nil
}

func usageBucketsEqual(left, right UsageBucket) bool {
	return nearlyEqual(left.TotalUSD, right.TotalUSD) &&
		left.CallCount == right.CallCount &&
		left.CacheReadTokens == right.CacheReadTokens &&
		nearlyEqual(left.CacheCostUSD, right.CacheCostUSD) &&
		left.CacheWriteTokens == right.CacheWriteTokens &&
		nearlyEqual(left.CacheWriteUSD, right.CacheWriteUSD) &&
		left.InputTokens == right.InputTokens &&
		left.OutputTokens == right.OutputTokens
}

func nearlyEqual(left, right float64) bool {
	return math.Abs(left-right) <= 1e-9
}
