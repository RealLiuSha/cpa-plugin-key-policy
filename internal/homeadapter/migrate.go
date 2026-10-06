package homeadapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// pluginKey is a key as cpa-key-policy stored it in its v6 state file.
type pluginKey struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Enabled         bool       `json:"enabled"`
	KeyHash         string     `json:"key_hash"`
	RPM             int        `json:"rpm"`
	Models          []ModelRef `json:"models"`
	DailyLimitUSD   float64    `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64    `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64    `json:"monthly_limit_usd"`
	CreatedAt       time.Time  `json:"created_at"`
}

type MigrateOptions struct {
	StatePath      string
	UsagePath      string
	PlaintextPaths []string
	Apply          bool
}

// MigrateReport counts what a migration did or would do.
type MigrateReport struct {
	Keys             int      `json:"keys"`
	Create           int      `json:"create"`
	Update           int      `json:"update"`
	MissingPlaintext []string `json:"missing_plaintext"`
	Unsupported      []string `json:"unsupported"`
	Failed           []string `json:"failed"`
}

// Migrate replays cpa-key-policy keys into Home through Upsert, the same path
// POST /keys uses, so a migrated key cannot differ from an API-created one.
// Each key keeps the weekday its weekly cycle resets on. The plugin stored
// only key hashes, so values come from the plaintext files of the issuer.
func Migrate(ctx context.Context, svc *Service, opts MigrateOptions, progress io.Writer) (MigrateReport, error) {
	var state struct {
		Keys []pluginKey `json:"keys"`
	}
	if err := readJSON(opts.StatePath, &state); err != nil {
		return MigrateReport{}, err
	}
	resetDays, err := weeklyResetDays(opts.UsagePath, svc.location)
	if err != nil {
		return MigrateReport{}, err
	}
	plaintext, err := plaintextByHash(opts.PlaintextPaths)
	if err != nil {
		return MigrateReport{}, err
	}
	existing, err := svc.home.ListUsers(ctx)
	if err != nil {
		return MigrateReport{}, err
	}
	known := make(map[string]bool, len(existing))
	for _, u := range existing {
		known[u.Username] = true
	}

	report := MigrateReport{Keys: len(state.Keys), MissingPlaintext: []string{}, Unsupported: []string{}, Failed: []string{}}
	sort.Slice(state.Keys, func(i, j int) bool { return state.Keys[i].ID < state.Keys[j].ID })
	for _, key := range state.Keys {
		value, ok := plaintext[strings.ToLower(strings.TrimPrefix(key.KeyHash, "sha256:"))]
		if !ok {
			report.MissingPlaintext = append(report.MissingPlaintext, key.ID)
			continue
		}
		models := key.Models
		enabled, name := key.Enabled, key.Name
		write := KeyWrite{
			ID: key.ID, Name: &name, Enabled: &enabled, Key: value, RPM: &key.RPM, Models: &models,
			DailyLimitUSD: &key.DailyLimitUSD, WeeklyLimitUSD: &key.WeeklyLimitUSD, MonthlyLimitUSD: &key.MonthlyLimitUSD,
		}
		if err := write.validate(); err != nil {
			report.Unsupported = append(report.Unsupported, fmt.Sprintf("%s: %v", key.ID, err))
			continue
		}
		if known[svc.username(key.ID)] {
			report.Update++
		} else {
			report.Create++
		}
		if !opts.Apply {
			continue
		}
		day := resetDays[key.ID]
		if day == 0 && !key.CreatedAt.IsZero() {
			day = weekday(key.CreatedAt.In(svc.location))
		}
		if _, err := svc.Upsert(ctx, write, day); err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", key.ID, err))
			continue
		}
		if progress != nil {
			fmt.Fprintf(progress, "migrated %s\n", key.ID)
		}
	}
	return report, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// weeklyResetDays reads the weekday each key's current weekly cycle resets on.
func weeklyResetDays(path string, location *time.Location) (map[string]int, error) {
	out := make(map[string]int)
	if path == "" {
		return out, nil
	}
	var usage struct {
		Usage map[string]struct {
			Cycles struct {
				Weekly struct {
					ResetsAt time.Time `json:"resets_at"`
				} `json:"weekly"`
			} `json:"cycles"`
		} `json:"usage"`
	}
	if err := readJSON(path, &usage); err != nil {
		return nil, err
	}
	for id, entry := range usage.Usage {
		if resetsAt := entry.Cycles.Weekly.ResetsAt; !resetsAt.IsZero() {
			out[id] = weekday(resetsAt.In(location))
		}
	}
	return out, nil
}

// plaintextByHash indexes candidate key values by their SHA-256. A file may be
// JSON (every string under an "ak", "key", "api_key" or "plain_key" field
// counts) or plain text with one key per line.
func plaintextByHash(paths []string) (map[string]string, error) {
	out := make(map[string]string)
	add := func(value string) {
		if value = strings.TrimSpace(value); value != "" && !strings.ContainsAny(value, " \t") {
			sum := sha256.Sum256([]byte(value))
			out[hex.EncodeToString(sum[:])] = value
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var doc any
		if json.Unmarshal(data, &doc) == nil {
			collectKeyValues(doc, add)
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			add(scanner.Text())
		}
	}
	return out, nil
}

func collectKeyValues(node any, add func(string)) {
	switch value := node.(type) {
	case map[string]any:
		for field, child := range value {
			switch strings.ToLower(field) {
			case "ak", "key", "api_key", "plain_key":
				if text, ok := child.(string); ok {
					add(text)
					continue
				}
			}
			collectKeyValues(child, add)
		}
	case []any:
		for _, child := range value {
			collectKeyValues(child, add)
		}
	}
}
