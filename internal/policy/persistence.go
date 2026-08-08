package policy

import (
	"bytes"
	"encoding/json"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

const (
	usageFileVersion = 2
	legacyWeekWindow = 7 * 24 * time.Hour
)

type persistedState struct {
	Version       int             `json:"version"`
	Keys          []KeyConfig     `json:"keys"`
	Usage         json.RawMessage `json:"usage,omitempty"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Aliases       []AliasMapping  `json:"aliases,omitempty"`
	ClassifyRules []ClassifyRule  `json:"classify_rules,omitempty"`
}

type persistedUsage struct {
	Version   int                    `json:"version"`
	Usage     map[string]*UsageState `json:"usage"`
	UpdatedAt time.Time              `json:"updated_at,omitempty"`
}

type legacyUsageState struct {
	Daily   UsageWindow                `json:"daily"`
	Weekly  UsageWindow                `json:"weekly"`
	ByAlias map[string]json.RawMessage `json:"by_alias,omitempty"`
}

// UsageMigrated reports whether LoadState converted a v1 aggregate ledger.
func (s *State) UsageMigrated() bool {
	return s != nil && s.usageMigrated
}

// PreMigrationUsageTotals returns the aggregate values stored in legacy
// windows before v1-to-v2 conversion. V2 entries are absent from this map.
func (s *State) PreMigrationUsageTotals() map[string]UsageMigrationTotals {
	if s == nil {
		return map[string]UsageMigrationTotals{}
	}
	result := make(map[string]UsageMigrationTotals, len(s.preMigrationTotals))
	for id, totals := range s.preMigrationTotals {
		result[id] = totals
	}
	return result
}

func ResolveStatePath(path string) (string, error) {
	return persist.ResolvePath(path, DefaultConfig().StateFile)
}

func LoadState(path string) (*State, error) {
	location, err := time.LoadLocation(DefaultConfig().UsageTimezone)
	if err != nil {
		location = time.UTC
	}
	return LoadStateAt(path, time.Now(), location)
}

func LoadStateAt(path string, now time.Time, location *time.Location) (*State, error) {
	if err := persist.CleanupStaleTemps(path, now); err != nil {
		return nil, err
	}
	raw, err := persist.Read(path)
	if err != nil {
		return nil, err
	}
	var disk persistedState
	if err := json.Unmarshal(raw, &disk); err != nil {
		return nil, err
	}
	usage, before, migrated, err := decodeUsageMap(disk.Usage, now, location)
	if err != nil {
		return nil, err
	}
	version := disk.Version
	if version == 0 {
		version = 1
	}
	return &State{
		Version: version, Keys: disk.Keys, Usage: usage, UpdatedAt: disk.UpdatedAt,
		Aliases: disk.Aliases, ClassifyRules: disk.ClassifyRules,
		usageMigrated: migrated, preMigrationTotals: before,
	}, nil
}

func LoadUsage(path string) (map[string]*UsageState, error) {
	raw, err := persist.Read(path)
	if err != nil {
		return nil, err
	}
	var disk persistedUsage
	if err := json.Unmarshal(raw, &disk); err != nil {
		return nil, err
	}
	if disk.Usage == nil {
		disk.Usage = make(map[string]*UsageState)
	}
	return disk.Usage, nil
}

func stripDerived(keys []KeyConfig) []KeyConfig {
	clean := make([]KeyConfig, len(keys))
	for i := range keys {
		clean[i] = keys[i]
		clean[i].Models = nil
	}
	return clean
}

// SaveState persists policy configuration only. Usage has a separate atomic
// file so high-frequency accounting flushes never rewrite keys.
func SaveState(path string, keys []KeyConfig, aliases []AliasMapping, rules []ClassifyRule) error {
	state := persistedState{
		Version: usageFileVersion, Keys: stripDerived(keys), UpdatedAt: time.Now().UTC(),
		Aliases: aliases, ClassifyRules: rules,
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

func SaveUsage(path string, usage map[string]*UsageState) error {
	disk := persistedUsage{Version: usageFileVersion, Usage: usage}
	raw, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

func decodeUsageMap(raw json.RawMessage, now time.Time, location *time.Location) (map[string]*UsageState, map[string]UsageMigrationTotals, bool, error) {
	result := make(map[string]*UsageState)
	before := make(map[string]UsageMigrationTotals)
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return result, before, false, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil, false, err
	}
	migrated := false
	for id, entry := range entries {
		var shape map[string]json.RawMessage
		if err := json.Unmarshal(entry, &shape); err != nil {
			return nil, nil, false, err
		}
		if _, isV2 := shape["days"]; isV2 {
			var state UsageState
			if err := json.Unmarshal(entry, &state); err != nil {
				return nil, nil, false, err
			}
			if state.Days == nil {
				state.Days = make(map[string]UsageBucket)
			}
			if state.ByAlias == nil {
				state.ByAlias = make(map[string]map[string]UsageBucket)
			}
			result[id] = &state
			continue
		}
		var legacy legacyUsageState
		if err := json.Unmarshal(entry, &legacy); err != nil {
			return nil, nil, false, err
		}
		before[id] = summarizeEffectiveLegacyUsage(legacy, now, location)
		result[id] = migrateLegacyUsageState(legacy, now, location)
		migrated = true
	}
	return result, before, migrated, nil
}

func migrateLegacyUsageState(legacy legacyUsageState, now time.Time, location *time.Location) *UsageState {
	if location == nil {
		location = time.UTC
	}
	state := &UsageState{Days: make(map[string]UsageBucket), ByAlias: make(map[string]map[string]UsageBucket)}
	migrateLegacyWindows(state.Days, legacy.Daily, legacy.Weekly, now, location)
	for alias, raw := range legacy.ByAlias {
		var windows struct {
			Daily  UsageWindow `json:"daily"`
			Weekly UsageWindow `json:"weekly"`
		}
		var shape map[string]json.RawMessage
		if json.Unmarshal(raw, &shape) != nil {
			continue
		}
		_, hasDaily := shape["daily"]
		_, hasWeekly := shape["weekly"]
		if hasDaily || hasWeekly {
			if json.Unmarshal(raw, &windows) != nil {
				continue
			}
		} else {
			if json.Unmarshal(raw, &windows.Daily) != nil {
				continue
			}
			windows.Weekly = windows.Daily
		}
		days := make(map[string]UsageBucket)
		migrateLegacyWindows(days, windows.Daily, windows.Weekly, now, location)
		if len(days) > 0 {
			state.ByAlias[alias] = days
		}
	}
	return state
}

func legacyDailyBelongsToToday(daily UsageWindow, now time.Time, location *time.Location) bool {
	return !daily.WindowStart.IsZero() && daily.WindowStart.In(location).Format(dateLayout) == now.In(location).Format(dateLayout)
}

func legacyWeeklyIsActive(weekly UsageWindow, now time.Time) bool {
	return !weekly.WindowStart.IsZero() && now.Sub(weekly.WindowStart) < legacyWeekWindow
}

func summarizeEffectiveLegacyUsage(legacy legacyUsageState, now time.Time, location *time.Location) UsageMigrationTotals {
	result := UsageMigrationTotals{}
	if legacyDailyBelongsToToday(legacy.Daily, now, location) {
		result.DailyUSD = roundedUSD(legacy.Daily.TotalUSD)
	}
	if legacyWeeklyIsActive(legacy.Weekly, now) {
		result.WeeklyUSD = roundedUSD(legacy.Weekly.TotalUSD)
	}
	// The v1 format never stored a trailing-30-day aggregate. nil is serialized
	// as JSON null so operators cannot mistake the old weekly value for a month.
	result.MonthlyUSD = nil
	return result
}

func migrateLegacyWindows(days map[string]UsageBucket, daily, weekly UsageWindow, now time.Time, location *time.Location) {
	localNow := now.In(location)
	today := localNow.Format(dateLayout)
	dailyBucket := usageBucketFromWindow(daily)
	dailyIncluded := legacyDailyBelongsToToday(daily, now, location)
	if dailyIncluded {
		days[today] = addUsageBucket(days[today], dailyBucket)
	}
	weeklyBucket := UsageBucket{}
	if legacyWeeklyIsActive(weekly, now) {
		weeklyBucket = usageBucketFromWindow(weekly)
	}
	residual := weeklyBucket
	if dailyIncluded {
		residual = subtractUsageBucket(weeklyBucket, dailyBucket)
	}
	if residual == (UsageBucket{}) {
		return
	}
	date := weekly.WindowStart.In(location).Format(dateLayout)
	if weekly.WindowStart.IsZero() || date > today {
		date = today
	}
	weeklyOldest := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -6).Format(dateLayout)
	if date < weeklyOldest {
		date = weeklyOldest
	}
	days[date] = addUsageBucket(days[date], residual)
}

func usageBucketFromWindow(window UsageWindow) UsageBucket {
	return UsageBucket{
		TotalUSD: window.TotalUSD, CallCount: window.CallCount,
		CacheReadTokens: window.CacheReadTokens, CacheCostUSD: window.CacheCostUSD,
		InputTokens: window.InputTokens, OutputTokens: window.OutputTokens,
	}
}

func subtractUsageBucket(total, part UsageBucket) UsageBucket {
	return UsageBucket{
		TotalUSD:        max(total.TotalUSD-part.TotalUSD, 0),
		CallCount:       max(total.CallCount-part.CallCount, 0),
		CacheReadTokens: max(total.CacheReadTokens-part.CacheReadTokens, 0),
		CacheCostUSD:    max(total.CacheCostUSD-part.CacheCostUSD, 0),
		InputTokens:     max(total.InputTokens-part.InputTokens, 0),
		OutputTokens:    max(total.OutputTokens-part.OutputTokens, 0),
	}
}
