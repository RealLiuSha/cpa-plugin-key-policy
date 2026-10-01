// cpa-key-policy-check rehearses migration on a private copy of a state/usage
// pair. It never configures a Store against the supplied source directory.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

func main() {
	statePath := flag.String("state", "", "source state file (read only)")
	zone := flag.String("timezone", "Asia/Shanghai", "accounting timezone")
	at := flag.String("at", "", "optional RFC3339 migration instant")
	flag.Parse()
	if err := run(*statePath, *zone, *at); err != nil {
		fmt.Fprintln(os.Stderr, "migration check failed:", err)
		os.Exit(1)
	}
}

func run(source, zone, at string) error {
	if source == "" {
		return fmt.Errorf("--state is required")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return err
	}
	now := time.Now()
	if at != "" {
		now, err = time.Parse(time.RFC3339, at)
		if err != nil {
			return err
		}
	}
	directory, err := os.MkdirTemp("", "cpa-key-policy-check-")
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "state.json")
	defer func() {
		for _, owned := range []string{path, persist.UsagePath(path), policy.MigrationBackupPath(path)} {
			_ = os.Remove(owned)
		}
		_ = os.Remove(directory)
	}()
	for _, pair := range [][2]string{{source, path}, {persist.UsagePath(source), persist.UsagePath(path)}} {
		raw, err := os.ReadFile(pair[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(pair[1], raw, 0o600); err != nil {
			return err
		}
	}
	before, err := policy.LoadState(path)
	if err != nil {
		return err
	}
	oldUsage, err := policy.LoadUsage(persist.UsagePath(path))
	if err != nil {
		return err
	}
	store := policy.NewStore()
	store.SetClock(func() time.Time { return now })
	cfg := policy.Config{Enabled: true, StateFile: path, UsageTimezone: zone}
	if err := store.Configure(cfg); err != nil {
		return err
	}
	if err := store.FlushUsage(); err != nil {
		return err
	}
	after, err := policy.LoadState(path)
	if err != nil {
		return err
	}
	newUsage, err := policy.LoadUsage(persist.UsagePath(path))
	if err != nil {
		return err
	}
	// LoadState already projects older formats, so the source must read back
	// exactly as the migrated models; what the projection drops is reported.
	if before.DatasetID != after.DatasetID || !reflect.DeepEqual(before.Models, after.Models) {
		return fmt.Errorf("dataset identity or models changed")
	}
	keys := make(map[string]policy.KeyConfig)
	for _, key := range store.Keys() {
		keys[key.ID] = key
	}
	if len(keys) != len(before.Keys) {
		return fmt.Errorf("key count changed")
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	oldest := day.AddDate(0, 0, -34).Format(time.DateOnly)
	today := day.Format(time.DateOnly)
	preserved := 0
	for id, state := range oldUsage.Usage {
		next := newUsage.Usage[id]
		if next == nil {
			return fmt.Errorf("usage entry disappeared")
		}
		for date, bucket := range state.Days {
			if date < oldest {
				continue
			}
			if !reflect.DeepEqual(bucket, next.Days[date]) {
				return fmt.Errorf("retained consumption history changed")
			}
			for name, modelDays := range state.ByModel {
				if !reflect.DeepEqual(modelDays[date], next.ByModel[name][date]) {
					return fmt.Errorf("model history changed")
				}
			}
			preserved++
		}
	}
	for _, old := range before.Keys {
		key := keys[old.ID]
		// Missing legacy timestamps may be initialized; identity and all policy
		// fields must survive exactly after the old reader's normalization.
		old.CreatedAt, old.UpdatedAt, old.LimitsChangedAt = key.CreatedAt, key.UpdatedAt, key.LimitsChangedAt
		if !reflect.DeepEqual(old, key) {
			return fmt.Errorf("key identity or policy changed")
		}
		if oldUsage.Version >= 5 {
			continue
		}
		summary := store.UsageSummaryFor(key)
		for _, cycle := range summary.Cycles {
			days := 1
			if cycle.Window == policy.UsageResetWeekly {
				days = 7
			}
			if cycle.Window == policy.UsageResetMonthly {
				days = 30
			}
			from := day.AddDate(0, 0, 1-days).Format(time.DateOnly)
			want := 0.0
			if usage := oldUsage.Usage[key.ID]; usage != nil {
				for date, bucket := range usage.Days {
					if date >= from && date <= today {
						want += bucket.TotalUSD
					}
				}
			}
			if math.Abs(cycle.UsedUSD-want) > 1e-8 || !cycle.ResetsAt.Equal(day.AddDate(0, 0, days)) {
				return fmt.Errorf("%s opening consumption or deadline changed", cycle.Window)
			}
		}
	}
	if err := store.Configure(cfg); err != nil {
		return err
	}
	if err := store.FlushUsage(); err != nil {
		return err
	}
	reloaded, err := policy.LoadUsage(persist.UsagePath(path))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(newUsage.Usage, reloaded.Usage) {
		return fmt.Errorf("restarting reapplied migration or changed quota state")
	}
	digest, err := policyDigest(after)
	if err != nil {
		return err
	}
	removed := before.RemovedSettings
	if removed == nil {
		removed = []string{}
	}
	// Public names that differ from the upstream id rely on the response
	// interceptor's model-name rewrite.
	renamed := []string{}
	for _, model := range after.Models {
		if model.Name != model.TargetModel {
			renamed = append(renamed, model.Name+" -> "+model.Provider+"/"+model.TargetModel)
		}
	}
	report := map[string]any{
		"result": "passed", "source_state_version": before.Version, "source_usage_version": oldUsage.Version,
		"target_version": after.Version, "keys": len(keys), "models": len(after.Models), "preserved_history_days": preserved,
		"opening_balances_preserved": true, "restart_idempotent": true, "source_read_only": true, "checked_at": now,
		"policy_sha256": digest, "removed_settings": removed, "renamed_models": renamed,
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

// The publisher compares normalized policy before and after activation. Keep
// normalization in the policy package; timestamps and serialization order are
// not policy and may legitimately change during a legacy migration.
func policyDigest(state *policy.State) (string, error) {
	keys := append([]policy.KeyConfig(nil), state.Keys...)
	for i := range keys {
		keys[i].CreatedAt = time.Time{}
		keys[i].UpdatedAt = time.Time{}
		keys[i].LimitsChangedAt = time.Time{}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	models := append([]policy.ModelDefinition(nil), state.Models...)
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	raw, err := json.Marshal(struct {
		DatasetID string                   `json:"dataset_id"`
		Keys      []policy.KeyConfig       `json:"keys"`
		Models    []policy.ModelDefinition `json:"models"`
	}{state.DatasetID, keys, models})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
