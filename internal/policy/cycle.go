package policy

import (
	"fmt"
	"strings"
	"time"
)

type UsageResetWindow string

const (
	UsageResetDaily   UsageResetWindow = "daily"
	UsageResetWeekly  UsageResetWindow = "weekly"
	UsageResetMonthly UsageResetWindow = "monthly"
)

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

func (window UsageResetWindow) valid() bool {
	return window == UsageResetDaily || window == UsageResetWeekly || window == UsageResetMonthly
}

// Cycle is one quota period. ByModel is the only stored consumption; the
// period total is derived from it.
type Cycle struct {
	StartedAt time.Time         `json:"started_at"`
	ResetsAt  time.Time         `json:"resets_at"`
	ResetKind string            `json:"reset_kind"`
	ByModel   map[string]Bucket `json:"by_model"`
}

type Cycles struct {
	Daily   Cycle `json:"daily"`
	Weekly  Cycle `json:"weekly"`
	Monthly Cycle `json:"monthly"`
}

func (cycles *Cycles) cycle(window UsageResetWindow) *Cycle {
	switch window {
	case UsageResetWeekly:
		return &cycles.Weekly
	case UsageResetMonthly:
		return &cycles.Monthly
	default:
		return &cycles.Daily
	}
}

func (cycles *Cycles) clone() *Cycles {
	if cycles == nil {
		return nil
	}
	copy := *cycles
	for _, window := range quotaWindows {
		source, target := cycles.cycle(window), copy.cycle(window)
		target.ByModel = make(map[string]Bucket, len(source.ByModel))
		for name, bucket := range source.ByModel {
			target.ByModel[name] = bucket
		}
	}
	return &copy
}

func (cycle *Cycle) used() Nano {
	var used Nano
	for _, bucket := range cycle.ByModel {
		used += bucket.ChargedNano
	}
	return used
}

func (l *usageLedger) startOfDay(at time.Time) time.Time {
	return dayStart(at, 0, l.location)
}

// dayStart is the first instant of the calendar day `days` after at's local
// day. Where DST starts at local midnight (Santiago, Cairo) 00:00 does not
// exist and time.Date may resolve it to 23:00 of the day before; the day
// then starts when that zone ends. Boundaries are always derived from a
// calendar date this way, since AddDate on such a 23:00 would carry the
// hour into every later period.
func dayStart(at time.Time, days int, loc *time.Location) time.Time {
	local := at.In(loc)
	year, month, day := time.Date(local.Year(), local.Month(), local.Day()+days, 0, 0, 0, 0, time.UTC).Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if start.Day() != day {
		_, start = start.ZoneBounds()
	}
	return start
}

// newCycles opens the three periods at the key's creation (or migration)
// instant: the daily period starts at that day's 00:00, the longer periods
// at the instant itself; every period ends when the day N days after that
// day starts.
func (l *usageLedger) newCycles(at time.Time, kind string) *Cycles {
	cycles := &Cycles{}
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		*cycle = Cycle{
			StartedAt: at, ResetsAt: dayStart(at, window.days(), l.location),
			ResetKind: kind, ByModel: make(map[string]Bucket),
		}
		if window == UsageResetDaily {
			cycle.StartedAt = l.startOfDay(at)
		}
	}
	return cycles
}

// advanceCyclesLocked rolls expired periods forward from their own deadline,
// so the calendar survives downtime and idle periods. Calendar days rather
// than 24-hour durations keep DST boundaries valid.
func (l *usageLedger) advanceCyclesLocked(cycles *Cycles, now time.Time) bool {
	changed := false
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		if now.Before(cycle.ResetsAt) {
			continue
		}
		start := cycle.ResetsAt.In(l.location)
		next := dayStart(start, window.days(), l.location)
		for !now.Before(next) {
			start, next = next, dayStart(next, window.days(), l.location)
		}
		*cycle = Cycle{StartedAt: start, ResetsAt: next, ResetKind: "automatic", ByModel: make(map[string]Bucket)}
		changed = true
	}
	return changed
}

func (l *usageLedger) manualResetDeadline(now time.Time, window UsageResetWindow) time.Time {
	return dayStart(now, window.days(), l.location)
}

func validateCycles(cycles *Cycles, location *time.Location) error {
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		if cycle.StartedAt.IsZero() || !cycle.ResetsAt.After(cycle.StartedAt) {
			return fmt.Errorf("invalid %s quota interval", window)
		}
		if !dayStart(cycle.StartedAt, window.days(), location).Equal(cycle.ResetsAt) {
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
			if err := bucket.validate(); err != nil {
				return fmt.Errorf("%s quota model %q: %w", window, name, err)
			}
		}
	}
	return nil
}
