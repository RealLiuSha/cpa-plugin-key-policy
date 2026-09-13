package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// UsageCycle owns quota consumption independently of retained daily history.
// ByModel is authoritative; the key total is derived so both views agree.
type UsageCycle struct {
	StartedAt time.Time              `json:"started_at"`
	ResetsAt  time.Time              `json:"resets_at"`
	ResetKind string                 `json:"reset_kind"`
	ByModel   map[string]UsageBucket `json:"by_model"`
}

type UsageCycles struct {
	Timezone string     `json:"timezone"`
	Daily    UsageCycle `json:"daily"`
	Weekly   UsageCycle `json:"weekly"`
	Monthly  UsageCycle `json:"monthly"`
}

type QuotaCycleSummary struct {
	Window             UsageResetWindow `json:"window"`
	StartedAt          time.Time        `json:"started_at"`
	ResetsAt           time.Time        `json:"resets_at"`
	ResetKind          string           `json:"reset_kind"`
	UsedUSD            float64          `json:"used_usd"`
	LimitUSD           float64          `json:"limit_usd"`
	ResetAfterManualAt time.Time        `json:"reset_after_manual_at"`
}

// Optional for existing API clients; the management UI sends the exact period
// and proposed deadline it displayed so a stale confirmation cannot reset anew.
type UsageResetExpectation struct {
	StartedAt          time.Time `json:"started_at"`
	ResetAfterManualAt time.Time `json:"reset_after_manual_at"`
}

var quotaWindows = [...]UsageResetWindow{UsageResetDaily, UsageResetWeekly, UsageResetMonthly}

func (window UsageResetWindow) days() int {
	switch window {
	case UsageResetWeekly:
		return 7
	case UsageResetMonthly:
		return 30
	default:
		return 1
	}
}

func (cycles *UsageCycles) cycle(window UsageResetWindow) *UsageCycle {
	switch window {
	case UsageResetWeekly:
		return &cycles.Weekly
	case UsageResetMonthly:
		return &cycles.Monthly
	default:
		return &cycles.Daily
	}
}

func cloneUsageCycles(cycles *UsageCycles) *UsageCycles {
	if cycles == nil {
		return nil
	}
	copy := *cycles
	for _, window := range quotaWindows {
		source, target := cycles.cycle(window), copy.cycle(window)
		target.ByModel = make(map[string]UsageBucket, len(source.ByModel))
		for name, bucket := range source.ByModel {
			target.ByModel[name] = bucket
		}
	}
	return &copy
}

func cycleTotal(cycle *UsageCycle) UsageBucket {
	names := make([]string, 0, len(cycle.ByModel))
	for name := range cycle.ByModel {
		names = append(names, name)
	}
	sort.Strings(names)
	var total UsageBucket
	for _, name := range names {
		total = addUsageBucket(total, cycle.ByModel[name])
	}
	return total
}

func cycleModelBucket(cycle *UsageCycle, model string) UsageBucket {
	names := make([]string, 0, len(cycle.ByModel))
	for name := range cycle.ByModel {
		if strings.EqualFold(name, model) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var total UsageBucket
	for _, name := range names {
		total = addUsageBucket(total, cycle.ByModel[name])
	}
	return total
}

func (l *usageLedger) newCycles(state *UsageState, now time.Time, kind string) *UsageCycles {
	cycles := &UsageCycles{Timezone: l.timezone}
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		*cycle = UsageCycle{
			StartedAt: now, ResetsAt: l.startOfDay(now).AddDate(0, 0, window.days()),
			ResetKind: kind, ByModel: make(map[string]UsageBucket),
		}
		if window == UsageResetDaily {
			cycle.StartedAt = l.startOfDay(now)
		}
		if kind == "migration" {
			for name, days := range state.ByModel {
				cycle.ByModel[name] = sumBuckets(days, l.dateKeyOffset(now, 1-window.days()), l.dateKey(now))
			}
		}
	}
	return cycles
}

// Called under the ledger lock by reads, writes and snapshots. Advancing from
// the old deadline preserves the calendar even after downtime or idle periods.
func (l *usageLedger) advanceCyclesLocked(state *UsageState, now time.Time) bool {
	if state.Cycles == nil {
		state.Cycles = l.newCycles(state, now, "initial")
		return true
	}
	changed := false
	for _, window := range quotaWindows {
		cycle := state.Cycles.cycle(window)
		if now.Before(cycle.ResetsAt) {
			continue
		}
		start := cycle.ResetsAt.In(l.location)
		// Calendar days, rather than 24-hour durations, keep DST boundaries valid.
		for !now.Before(start.AddDate(0, 0, window.days())) {
			start = start.AddDate(0, 0, window.days())
		}
		*cycle = UsageCycle{StartedAt: start, ResetsAt: start.AddDate(0, 0, window.days()), ResetKind: "automatic", ByModel: make(map[string]UsageBucket)}
		changed = true
	}
	return changed
}

func (l *usageLedger) currentEntryLocked(id string, now time.Time) *UsageState {
	state := l.entryLocked(id)
	if l.advanceCyclesLocked(state, now) {
		l.markDirtyLocked()
	}
	return state
}

func (l *usageLedger) initializeCycles(keys []KeyConfig, migrate bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, key := range keys {
		state := l.entryLocked(key.ID)
		if !migrate && state.Cycles == nil && !key.CreatedAt.IsZero() {
			state.Cycles = l.newCycles(state, key.CreatedAt, "initial")
			l.markDirtyLocked()
		}
	}
	for _, state := range l.entries {
		if state.Cycles == nil && migrate {
			state.Cycles = l.newCycles(state, now, "migration")
			l.markDirtyLocked()
		} else if l.advanceCyclesLocked(state, now) {
			l.markDirtyLocked()
		}
	}
}

func validateUsageCycles(cycles *UsageCycles) error {
	if cycles == nil {
		return nil // Only v3/v4 readers permit an absent cycle state.
	}
	location, err := time.LoadLocation(cycles.Timezone)
	if err != nil || cycles.Timezone == "" {
		return fmt.Errorf("invalid quota timezone %q", cycles.Timezone)
	}
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		if cycle.StartedAt.IsZero() || !cycle.ResetsAt.After(cycle.StartedAt) {
			return fmt.Errorf("invalid %s quota interval", window)
		}
		start := cycle.StartedAt.In(location)
		deadline := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location).AddDate(0, 0, window.days())
		if !deadline.Equal(cycle.ResetsAt) {
			return fmt.Errorf("invalid %s quota reset date", window)
		}
		switch cycle.ResetKind {
		case "initial", "migration", "manual", "automatic":
		default:
			return fmt.Errorf("invalid %s quota reset kind %q", window, cycle.ResetKind)
		}
		if cycle.ByModel == nil {
			return fmt.Errorf("missing %s quota model counters", window)
		}
		for name, bucket := range cycle.ByModel {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("empty %s quota model", window)
			}
			if err := validateUsageBucket(bucket); err != nil {
				return fmt.Errorf("%s quota model %q: %w", window, name, err)
			}
		}
	}
	return nil
}

func validateUsageBucket(bucket UsageBucket) error {
	for _, amount := range []float64{bucket.TotalUSD, bucket.CacheCostUSD, bucket.CacheWriteUSD} {
		if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
			return fmt.Errorf("usage costs must be finite and nonnegative")
		}
	}
	if bucket.InputTokens < 0 || bucket.OutputTokens < 0 || bucket.CacheReadTokens < 0 || bucket.CacheWriteTokens < 0 || bucket.CallCount < 0 {
		return fmt.Errorf("usage counters must be nonnegative")
	}
	return nil
}
