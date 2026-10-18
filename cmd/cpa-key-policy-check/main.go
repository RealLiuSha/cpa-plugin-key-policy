// cpa-key-policy-check prepares and verifies cpa-key-policy data without
// connecting to CPA. The source files are only read.
//
//	--state S --out DIR  convert a data format 6 pair into a new format 7
//	                     dataset in DIR (empty or missing) and verify it
//	--state S            rehearse that conversion in a private directory, or
//	                     check that a format 7 dataset loads and survives a
//	                     restart unchanged
//	--config FILE        check that this release accepts the cpa-key-policy
//	                     section of a CPA config.yaml
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/legacyv6"
	"cpa-key-policy/internal/plugin"
	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"

	"gopkg.in/yaml.v3"
)

const targetVersion = 7

func main() {
	statePath := flag.String("state", "", "source state file (read only)")
	out := flag.String("out", "", "empty or missing directory that receives the converted format 7 dataset")
	configPath := flag.String("config", "", "CPA config.yaml whose cpa-key-policy section is checked")
	zone := flag.String("timezone", "Asia/Shanghai", "accounting timezone")
	at := flag.String("at", "", "optional RFC3339 conversion instant")
	flag.Parse()
	var report any
	var err error
	switch {
	case *configPath != "" && (*statePath != "" || *out != ""):
		err = errors.New("--config is checked on its own")
	case *configPath != "":
		report, err = checkConfig(*configPath)
	default:
		report, err = run(*statePath, *out, *zone, *at)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migration check failed:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "migration check failed:", err)
		os.Exit(1)
	}
}

// checkReport is what publish.py reads: result, target_version, keys,
// models and policy_sha256 drive its pre/post-start comparison, output names
// the converted dataset.
type checkReport struct {
	Result               string `json:"result"`
	SourceStateVersion   int    `json:"source_state_version"`
	SourceUsageVersion   int    `json:"source_usage_version"`
	TargetVersion        int    `json:"target_version"`
	Keys                 int    `json:"keys"`
	Models               int    `json:"models"`
	PreservedHistoryDays int    `json:"preserved_history_days"`
	ExpiredHistoryDays   int    `json:"expired_history_days"`
	// QuotaCyclesPreserved is only reported when a dataset was converted.
	QuotaCyclesPreserved bool               `json:"quota_cycles_preserved,omitempty"`
	RestartIdempotent    bool               `json:"restart_idempotent"`
	SourceReadOnly       bool               `json:"source_read_only"`
	CheckedAt            time.Time          `json:"checked_at"`
	PolicySHA256         string             `json:"policy_sha256"`
	RemovedSettings      []string           `json:"removed_settings"`
	RenamedModels        []string           `json:"renamed_models"`
	Conversion           *conversionSummary `json:"conversion,omitempty"`
	Output               string             `json:"output,omitempty"`
}

// conversionSummary is computed from the format 6 input itself, independent
// of the converter's own report.
type conversionSummary struct {
	KeysWithModelLists       int      `json:"keys_with_model_lists"`
	IgnoredModelGrants       int      `json:"ignored_model_grants"`
	PartialScopeKeyCount     int      `json:"partial_scope_key_count"`
	PartialScopeKeys         []string `json:"partial_scope_keys"`
	RemovedModelDailyLimits  int      `json:"removed_model_daily_limits"`
	KeysWithoutModelsListing int      `json:"keys_without_models_listing"`
	DroppedDuplicateRows     []string `json:"dropped_duplicate_rows"`
}

const partialScopeKeysListed = 50

func run(source, out, zone, at string) (*checkReport, error) {
	if source == "" {
		return nil, errors.New("--state is required")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if at != "" {
		if now, err = time.Parse(time.RFC3339, at); err != nil {
			return nil, err
		}
	}
	stateVersion, err := fileVersion(source)
	if err != nil {
		return nil, err
	}
	usageVersion, err := fileVersion(persist.UsagePath(source))
	if err != nil {
		return nil, err
	}
	// The load and restart checks run on a private copy, so a converted
	// output stays exactly as the converter wrote it.
	directory, err := os.MkdirTemp("", "cpa-key-policy-check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, filepath.Base(source))
	report := &checkReport{
		Result: "passed", SourceStateVersion: stateVersion, SourceUsageVersion: usageVersion,
		SourceReadOnly: true, CheckedAt: now, RemovedSettings: []string{}, RenamedModels: []string{},
	}
	switch stateVersion {
	case legacyVersion:
		target := path
		if out != "" {
			if err := prepareOutput(out); err != nil {
				return nil, err
			}
			target = filepath.Join(out, filepath.Base(source))
		}
		if err := convert(source, target, zone, now, loc, report); err != nil {
			return nil, err
		}
		if out != "" {
			report.Output = out
			if err := copyDataset(target, path); err != nil {
				return nil, err
			}
		}
	case targetVersion:
		if out != "" {
			return nil, fmt.Errorf("%s is already data format %d; nothing to convert", source, targetVersion)
		}
		if err := copyDataset(source, path); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("state version %d is neither %d nor %d", stateVersion, legacyVersion, targetVersion)
	}
	if err := verifyLoad(path, zone, now, report); err != nil {
		return nil, err
	}
	return report, nil
}

// convert writes the format 7 dataset and compares it, before any Store
// touches it, with the independently read format 6 source.
func convert(source, target, zone string, now time.Time, loc *time.Location, report *checkReport) error {
	legacy, err := readLegacyPair(source)
	if err != nil {
		return err
	}
	converted, err := legacyv6.Convert(source, target, zone, now)
	if err != nil {
		return err
	}
	state, err := policy.LoadState(target)
	if err != nil {
		return err
	}
	usage, err := policy.LoadUsage(target)
	if err != nil {
		return err
	}
	if state.DatasetID != usage.DatasetID || state.DatasetID != legacy.state.DatasetID {
		return fmt.Errorf("converted dataset_id %q/%q, source %q", state.DatasetID, usage.DatasetID, legacy.state.DatasetID)
	}
	if err := legacy.verifyKeys(state); err != nil {
		return err
	}
	if err := legacy.verifyPrices(state); err != nil {
		return err
	}
	local := now.In(loc)
	oldest := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1-policy.UsageRetentionDays).Format(time.DateOnly)
	if report.PreservedHistoryDays, report.ExpiredHistoryDays, err = legacy.verifyHistory(usage, oldest); err != nil {
		return err
	}
	if err := legacy.verifyCycles(usage); err != nil {
		return err
	}
	report.QuotaCyclesPreserved = true
	report.RenamedModels = legacy.renamedModels()
	report.Conversion = legacy.summary()
	report.RemovedSettings = converted.Notes()
	return nil
}

// prepareOutput accepts a missing directory (created private) or an empty one.
func prepareOutput(out string) error {
	entries, err := os.ReadDir(out)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.Mkdir(out, 0o700)
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("output directory %s is not empty", out)
	}
	return nil
}

// verifyLoad starts the plugin's Store on the dataset at path, then restarts
// it: the second start must load what the first wrote and change nothing.
func verifyLoad(path, zone string, now time.Time, report *checkReport) error {
	cfg := policy.Config{Enabled: true, StateFile: path, UsageTimezone: zone}
	store := policy.NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(cfg); err != nil {
		return err
	}
	state, err := policy.LoadState(path)
	if err != nil {
		return err
	}
	if state.Version != targetVersion {
		return fmt.Errorf("dataset has version %d, want %d", state.Version, targetVersion)
	}
	digest, err := policyDigest(state)
	if err != nil {
		return err
	}
	report.TargetVersion, report.Keys, report.Models, report.PolicySHA256 = state.Version, len(state.Keys), len(state.Models), digest
	if err := store.Shutdown(); err != nil {
		return fmt.Errorf("first flush: %w", err)
	}
	first, err := policy.LoadUsage(path)
	if err != nil {
		return err
	}
	reopened := policy.NewStore()
	reopened.SetClock(func() time.Time { return now })
	if err := reopened.Configure(cfg); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	if err := reopened.Shutdown(); err != nil {
		return fmt.Errorf("restart flush: %w", err)
	}
	second, err := policy.LoadUsage(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(bucketSums(first), bucketSums(second)) || first.Through != second.Through || !sameCycleSets(first, second) {
		return errors.New("restarting changed the ledger")
	}
	restarted, err := policy.LoadState(path)
	if err != nil {
		return err
	}
	if again, err := policyDigest(restarted); err != nil || again != digest {
		return fmt.Errorf("restarting changed keys or prices (digest %s → %s, %v)", digest, again, err)
	}
	report.RestartIdempotent = true
	return nil
}

type configReport struct {
	Result        string `json:"result"`
	Enabled       bool   `json:"enabled"`
	StateFile     string `json:"state_file"`
	UsageTimezone string `json:"usage_timezone"`
}

// checkConfig decodes the cpa-key-policy section of a CPA config.yaml the
// way plugin registration does. A section this release rejects would make
// the plugin fail to register after the switch.
func checkConfig(path string) (*configReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	section, ok := document.Plugins.Configs[plugin.PluginID]
	if !ok {
		return nil, fmt.Errorf("%s has no plugins.configs.%s section", path, plugin.PluginID)
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	if err := encoder.Encode(&section); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	cfg, err := plugin.ParseConfig(encoded.Bytes())
	if err != nil {
		return nil, fmt.Errorf("plugins.configs.%s: %w", plugin.PluginID, err)
	}
	return &configReport{Result: "passed", Enabled: cfg.Enabled, StateFile: cfg.StateFile, UsageTimezone: cfg.UsageTimezone}, nil
}

// copyDataset copies the state file and its paired ledger files into the
// private directory. The live file is copied before the history file: a
// running plugin writes history first, so this order never yields a live
// file that references a newer history than the copy holds.
func copyDataset(source, target string) error {
	pairs := [][2]string{
		{source, target},
		{persist.UsagePath(source), persist.UsagePath(target)},
	}
	if _, err := os.Stat(persist.HistoryPath(source)); err == nil {
		pairs = append(pairs, [2]string{persist.HistoryPath(source), persist.HistoryPath(target)})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, pair := range pairs {
		raw, err := os.ReadFile(pair[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(pair[1], raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func fileVersion(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return header.Version, nil
}

func bucketSums(dataset *policy.UsageDataset) map[[3]string]policy.Bucket {
	sums := make(map[[3]string]policy.Bucket)
	for id, usage := range dataset.Keys {
		for date, models := range usage.Days {
			for model, bucket := range models {
				sums[[3]string{id, date, model}] = bucket
			}
		}
	}
	return sums
}

func sameCycleSets(left, right *policy.UsageDataset) bool {
	if len(left.Keys) != len(right.Keys) {
		return false
	}
	for id, usage := range left.Keys {
		other := right.Keys[id]
		if other == nil || !sameCycles(usage.Cycles, other.Cycles) {
			return false
		}
	}
	return true
}

func sameCycles(left, right *policy.Cycles) bool {
	if left == nil || right == nil {
		return left == right
	}
	for _, pair := range [][2]policy.Cycle{{left.Daily, right.Daily}, {left.Weekly, right.Weekly}, {left.Monthly, right.Monthly}} {
		if !pair[0].StartedAt.Equal(pair[1].StartedAt) || !pair[0].ResetsAt.Equal(pair[1].ResetsAt) || pair[0].ResetKind != pair[1].ResetKind || !reflect.DeepEqual(pair[0].ByModel, pair[1].ByModel) {
			return false
		}
	}
	return true
}

// The publisher compares normalized policy before and after activation.
// Timestamps and serialization order are not policy and may change.
func policyDigest(state *policy.State) (string, error) {
	keys := append([]policy.KeyConfig(nil), state.Keys...)
	for i := range keys {
		keys[i].CreatedAt = time.Time{}
		keys[i].UpdatedAt = time.Time{}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	models := append([]policy.ModelPrice(nil), state.Models...)
	sort.Slice(models, func(i, j int) bool {
		left, right := strings.ToLower(models[i].Name), strings.ToLower(models[j].Name)
		if left != right {
			return left < right
		}
		return models[i].Provider < models[j].Provider
	})
	raw, err := json.Marshal(struct {
		DatasetID string              `json:"dataset_id"`
		Keys      []policy.KeyConfig  `json:"keys"`
		Models    []policy.ModelPrice `json:"models"`
	}{state.DatasetID, keys, models})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
