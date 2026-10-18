package policy

import (
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageRetentionDays is how many natural days of per-model consumption each
// key keeps, today included. Quota cycles do not depend on it.
const UsageRetentionDays = 90

const (
	usageFlushInterval = 15 * time.Second
	softLimitRatio     = 0.8
	dateLayout         = "2006-01-02"
)

// KeyUsage is everything the ledger holds for one key id. Cycles is nil for
// a deleted key, whose days stay until they age out of retention; Balance is
// nil until a balance key's first adjustment or deduction, and goes with the
// periods when the key is deleted.
type KeyUsage struct {
	Cycles  *Cycles
	Balance *Balance
	Days    map[string]map[string]Bucket // date → model → bucket
}

func (u *KeyUsage) clone() *KeyUsage {
	if u == nil {
		return nil
	}
	return &KeyUsage{Cycles: u.Cycles.clone(), Balance: u.Balance.clone(), Days: cloneDays(u.Days, func(string) bool { return true })}
}

func cloneDays(days map[string]map[string]Bucket, keep func(date string) bool) map[string]map[string]Bucket {
	result := make(map[string]map[string]Bucket, len(days))
	for date, models := range days {
		if !keep(date) {
			continue
		}
		result[date] = maps.Clone(models)
	}
	return result
}

// usageLedger owns quota cycles and retained daily consumption. Resets only
// touch cycles; daily history survives manual and automatic resets. All
// mutation happens under mu; readers copy what they need out of the lock.
//
// Periods are opened only by openCycles and syncCycles (key registration);
// reads describe a key without periods with throwaway values, and a record
// for such a key only reaches the day's history. This keeps deleted keys
// from reappearing in the live file with periods a recreated key would
// inherit.
//
// Days at or before closedThrough are closed: no bucket in them is written
// again (a record dated there moves to the first open day), so a history
// write may share their model maps while it runs outside the lock. The live
// part of a flush is copied under the lock.
type usageLedger struct {
	mu       sync.Mutex
	now      func() time.Time
	location *time.Location
	timezone string
	entries  map[string]*KeyUsage
	dirty    bool
	revision uint64
	// historyThrough is the last date the history file holds. Days after it
	// live only in the live file until the next history write.
	historyThrough string
	// closedThrough is the watermark of the latest planned history write,
	// successful or not. It never moves backwards and is never below
	// historyThrough.
	closedThrough string
	// historyStale means retention evicted days the history file still holds,
	// so it must be rewritten although no day closed.
	historyStale    bool
	lastEvictedDate string
}

func newUsageLedger(now func() time.Time, location *time.Location, timezone string) *usageLedger {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = time.UTC
	}
	if strings.TrimSpace(timezone) == "" {
		timezone = location.String()
	}
	return &usageLedger{now: now, location: location, timezone: timezone, entries: make(map[string]*KeyUsage)}
}

func (l *usageLedger) setClock(now func() time.Time) {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}

// load replaces the ledger with a loaded dataset and runs one retention pass,
// since the snapshot may be older than the current accounting day.
func (l *usageLedger) load(keys map[string]*KeyUsage, historyThrough string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make(map[string]*KeyUsage, len(keys))
	for id, usage := range keys {
		if usage != nil {
			l.entries[id] = usage.clone()
		}
	}
	l.historyThrough, l.closedThrough = historyThrough, historyThrough
	l.historyStale = false
	l.lastEvictedDate = ""
	l.dirty = false
	l.evictExpiredLocked(l.now())
}

func (l *usageLedger) markDirtyLocked() {
	l.dirty = true
	l.revision++
}

func (l *usageLedger) dateKey(at time.Time) string {
	return at.In(l.location).Format(dateLayout)
}

func (l *usageLedger) dateKeyOffset(at time.Time, days int) string {
	return l.startOfDay(at).AddDate(0, 0, days).Format(dateLayout)
}

// shiftDate moves a yyyy-mm-dd date key by days; an empty key stays empty.
func shiftDate(date string, days int) string {
	day, err := time.Parse(dateLayout, date)
	if err != nil {
		return date
	}
	return day.AddDate(0, 0, days).Format(dateLayout)
}

func (l *usageLedger) evictExpiredLocked(now time.Time) bool {
	currentDate := l.dateKey(now)
	if currentDate == l.lastEvictedDate {
		return false
	}
	l.lastEvictedDate = currentDate
	oldest := l.dateKeyOffset(now, -(UsageRetentionDays - 1))
	changed := false
	for id, usage := range l.entries {
		for date := range usage.Days {
			if date < oldest {
				delete(usage.Days, date)
				changed = true
				if date <= l.historyThrough {
					l.historyStale = true
				}
			}
		}
		if usage.Cycles == nil && len(usage.Days) == 0 {
			delete(l.entries, id)
			changed = true
		}
	}
	if changed {
		l.markDirtyLocked()
	}
	return changed
}

// record adds one billed call to today's bucket and, when the key still has
// periods, to each of them; deduct also takes the charge from the key's
// balance.
func (l *usageLedger) record(id, model string, delta Bucket, deduct bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	usage := l.entries[id]
	if usage == nil {
		usage = &KeyUsage{Days: make(map[string]map[string]Bucket)}
		l.entries[id] = usage
	}
	if usage.Cycles != nil {
		l.advanceCyclesLocked(usage.Cycles, now)
		for _, window := range quotaWindows {
			cycle := usage.Cycles.cycle(window)
			bucket := cycle.ByModel[model]
			bucket.add(delta)
			cycle.ByModel[model] = bucket
		}
		if deduct {
			if usage.Balance == nil {
				usage.Balance = &Balance{}
			}
			usage.Balance.Nano -= delta.ChargedNano
		}
	}
	date := l.openDateLocked(now)
	models := usage.Days[date]
	if models == nil {
		models = make(map[string]Bucket)
		usage.Days[date] = models
	}
	bucket := models[model]
	bucket.add(delta)
	models[model] = bucket
	l.evictExpiredLocked(now)
	l.markDirtyLocked()
}

// openDateLocked is the day a record made now belongs to. After a clock
// rollback onto a closed day it is the first open day: closed buckets stay
// immutable and the record still reaches the live file.
func (l *usageLedger) openDateLocked(now time.Time) string {
	if date := l.dateKey(now); date > l.closedThrough {
		return date
	}
	return shiftDate(l.closedThrough, 1)
}

// reportEndLocked is the last day reports cover: today, or the first open
// day when the closed watermark has reached today (a restart after the
// clock rolled back onto the day a history was written). Records made then
// land on that day (openDateLocked) and must not wait for the clock.
func (l *usageLedger) reportEndLocked(now time.Time) string {
	return max(l.dateKey(now), shiftDate(l.closedThrough, 1))
}

// reportRange is the count-day window, inclusive, ending at reportEndLocked.
func (l *usageLedger) reportRange(count int) (from, to string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evictExpiredLocked(now)
	to = l.reportEndLocked(now)
	return shiftDate(to, -(count - 1)), to
}

// openCyclesLocked opens the key's periods when it has none, anchored at the
// key's creation so the first weekly and monthly deadlines follow it, and
// rolls expired ones forward.
func (l *usageLedger) openCyclesLocked(key KeyConfig, now time.Time) {
	usage := l.entries[key.ID]
	if usage == nil {
		usage = &KeyUsage{Days: make(map[string]map[string]Bucket)}
		l.entries[key.ID] = usage
	}
	if usage.Cycles == nil {
		at := key.CreatedAt
		if at.IsZero() {
			at = now
		}
		usage.Cycles = l.newCycles(at, "initial")
		l.markDirtyLocked()
	}
	if l.advanceCyclesLocked(usage.Cycles, now) {
		l.markDirtyLocked()
	}
}

// openCycles registers one key's periods (creation or update).
func (l *usageLedger) openCycles(key KeyConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.openCyclesLocked(key, l.now())
}

// syncCycles registers the periods of every key the state file holds and
// forgets those of any other id: a crash between DeleteKey's state write and
// the next flush leaves them in the live file, and a key recreated under
// that id must start its own periods.
func (l *usageLedger) syncCycles(keys []KeyConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	known := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		known[key.ID] = struct{}{}
		l.openCyclesLocked(key, now)
	}
	for id, usage := range l.entries {
		if _, ok := known[id]; ok || (usage.Cycles == nil && usage.Balance == nil) {
			continue
		}
		usage.Cycles, usage.Balance = nil, nil
		if len(usage.Days) == 0 {
			delete(l.entries, id)
		}
		l.markDirtyLocked()
	}
}

// forgetKey drops a deleted key's periods and balance and reports what they
// held. Its daily history stays until retention drops it.
func (l *usageLedger) forgetKey(id string) (daily, weekly, monthly, balance Nano) {
	l.mu.Lock()
	defer l.mu.Unlock()
	usage := l.entries[id]
	if usage == nil || (usage.Cycles == nil && usage.Balance == nil) {
		return 0, 0, 0, 0
	}
	if usage.Cycles != nil {
		l.advanceCyclesLocked(usage.Cycles, l.now())
		daily, weekly, monthly = usage.Cycles.Daily.used(), usage.Cycles.Weekly.used(), usage.Cycles.Monthly.used()
	}
	if usage.Balance != nil {
		balance = usage.Balance.Nano
	}
	usage.Cycles, usage.Balance = nil, nil
	if len(usage.Days) == 0 {
		delete(l.entries, id)
	}
	l.markDirtyLocked()
	return daily, weekly, monthly, balance
}

// currentCyclesLocked returns the key's periods rolled forward to now. A key
// without periods is described by fresh values that are not stored.
func (l *usageLedger) currentCyclesLocked(id string, now time.Time) *Cycles {
	if usage := l.entries[id]; usage != nil && usage.Cycles != nil {
		if l.advanceCyclesLocked(usage.Cycles, now) {
			l.markDirtyLocked()
		}
		return usage.Cycles
	}
	return l.newCycles(now, "initial")
}

type UsageSummary struct {
	// QuotaMode is cycle or balance. BalanceUSD and NetCreditedUSD describe
	// the prepaid money: always for a balance key, and for a cycle key that
	// kept an unspent balance from an earlier balance period.
	QuotaMode                string              `json:"quota_mode"`
	BalanceUSD               *float64            `json:"balance_usd,omitempty"`
	NetCreditedUSD           *float64            `json:"net_credited_usd,omitempty"`
	Cycles                   []QuotaCycleSummary `json:"cycles"`
	Status                   string              `json:"status"`
	BlockedReason            string              `json:"blocked_reason,omitempty"`
	DailyUSD                 float64             `json:"daily_usd"`
	WeeklyUSD                float64             `json:"weekly_usd"`
	MonthlyUSD               float64             `json:"monthly_usd"`
	DailyLimitUSD            float64             `json:"daily_limit_usd"`
	WeeklyLimitUSD           float64             `json:"weekly_limit_usd"`
	MonthlyLimitUSD          float64             `json:"monthly_limit_usd"`
	NextAccountingBoundaryAt time.Time           `json:"next_accounting_boundary_at"`
	SoftLimitHit             bool                `json:"soft_limit_hit"`
	Timezone                 string              `json:"timezone"`
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

// quotaLimits is the complete policy input the ledger needs, so quota logic
// never depends on secrets, RPM or management metadata. A balance key has no
// period limits; its balance is its only limit.
type quotaLimits struct {
	DailyUSD, WeeklyUSD, MonthlyUSD float64
	Balance                         bool
}

func quotaLimitsForKey(key KeyConfig) quotaLimits {
	if key.BalanceMode() {
		return quotaLimits{Balance: true}
	}
	return quotaLimits{DailyUSD: key.DailyLimitUSD, WeeklyUSD: key.WeeklyLimitUSD, MonthlyUSD: key.MonthlyLimitUSD}
}

func (limits quotaLimits) forWindow(window UsageResetWindow) float64 {
	switch window {
	case UsageResetWeekly:
		return limits.WeeklyUSD
	case UsageResetMonthly:
		return limits.MonthlyUSD
	default:
		return limits.DailyUSD
	}
}

func (limits quotaLimits) any() bool {
	return limits.Balance || limits.DailyUSD > 0 || limits.WeeklyUSD > 0 || limits.MonthlyUSD > 0
}

func (l *usageLedger) summaryLocked(id string, limits quotaLimits, now time.Time) UsageSummary {
	cycles := l.currentCyclesLocked(id, now)
	summary := UsageSummary{
		QuotaMode:     QuotaModeCycle,
		Status:        "normal",
		DailyLimitUSD: limits.DailyUSD, WeeklyLimitUSD: limits.WeeklyUSD, MonthlyLimitUSD: limits.MonthlyUSD,
		NextAccountingBoundaryAt: cycles.Daily.ResetsAt,
		Timezone:                 l.timezone,
	}
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		used, limitUSD := cycle.used(), limits.forWindow(window)
		limit := USDToNano(limitUSD)
		summary.Cycles = append(summary.Cycles, QuotaCycleSummary{
			Window: window, StartedAt: cycle.StartedAt, ResetsAt: cycle.ResetsAt, ResetKind: cycle.ResetKind,
			UsedUSD: used.USD(), LimitUSD: limitUSD, ResetAfterManualAt: l.manualResetDeadline(now, window),
		})
		switch window {
		case UsageResetDaily:
			summary.DailyUSD = used.USD()
		case UsageResetWeekly:
			summary.WeeklyUSD = used.USD()
		case UsageResetMonthly:
			summary.MonthlyUSD = used.USD()
		}
		if limit > 0 && float64(used) >= float64(limit)*softLimitRatio {
			summary.SoftLimitHit = true
		}
		if limit > 0 && used >= limit && summary.BlockedReason == "" {
			summary.BlockedReason = string(window) + "_exceeded"
		}
	}
	// A cycle key that was a balance key keeps its balance unspent; it is
	// reported so switching back shows what it will spend.
	var balance Balance
	usage := l.entries[id]
	if usage != nil && usage.Balance != nil {
		balance = *usage.Balance
	}
	if limits.Balance || (usage != nil && usage.Balance != nil) {
		left, credited := balance.Nano.USD(), balance.NetCreditedNano.USD()
		summary.BalanceUSD, summary.NetCreditedUSD = &left, &credited
	}
	if limits.Balance {
		summary.QuotaMode = QuotaModeBalance
		switch {
		case balance.Nano <= 0:
			summary.BlockedReason = "balance_exhausted"
		case balance.NetCreditedNano > 0 && float64(balance.NetCreditedNano-balance.Nano) >= float64(balance.NetCreditedNano)*softLimitRatio:
			summary.SoftLimitHit = true
		}
	}
	switch {
	case summary.BlockedReason != "":
		summary.Status = "limited"
	case summary.SoftLimitHit:
		summary.Status = "warning"
	}
	return summary
}

func (l *usageLedger) summary(id string, limits quotaLimits) UsageSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evictExpiredLocked(now)
	return l.summaryLocked(id, limits, now)
}

// quotaDenial describes an exhausted quota. When several periods are
// exhausted at once, the one that resets last decides when requests can
// resume, so the denial names that period.
func (l *usageLedger) quotaDenial(id string, limits quotaLimits) *Denial {
	if limits.Balance {
		return l.balanceDenial(id)
	}
	if !limits.any() {
		return nil
	}
	summary := l.summary(id, limits)
	if summary.BlockedReason == "" {
		return nil
	}
	denial := &Denial{Reason: summary.BlockedReason, Timezone: summary.Timezone}
	for _, cycle := range summary.Cycles {
		if cycle.LimitUSD > 0 && USDToNano(cycle.UsedUSD) >= USDToNano(cycle.LimitUSD) && !cycle.ResetsAt.Before(denial.RetryAt) {
			denial.Reason, denial.Limit, denial.RetryAt = string(cycle.Window)+"_exceeded", cycle.LimitUSD, cycle.ResetsAt
		}
	}
	return denial
}

type UsageResetResult struct {
	KeyID                    string           `json:"id"`
	Window                   UsageResetWindow `json:"window"`
	BeforeDailyUSD           float64          `json:"before_daily_usd"`
	BeforeWeeklyUSD          float64          `json:"before_weekly_usd"`
	BeforeMonthlyUSD         float64          `json:"before_monthly_usd"`
	AfterDailyUSD            float64          `json:"after_daily_usd"`
	AfterWeeklyUSD           float64          `json:"after_weekly_usd"`
	AfterMonthlyUSD          float64          `json:"after_monthly_usd"`
	NextAccountingBoundaryAt time.Time        `json:"next_accounting_boundary_at"`
}

// Optional for existing API clients; the management UI sends the exact period
// and proposed deadline it displayed so a stale confirmation cannot reset anew.
type UsageResetExpectation struct {
	StartedAt          time.Time `json:"started_at"`
	ResetAfterManualAt time.Time `json:"reset_after_manual_at"`
}

// resetWindowAndPersist owns the reset transaction. The persist callback runs
// while mu is held so the file holds exactly one accounting cut; on failure
// the prior ledger is restored.
//
// TRADEOFF: persisting under the ledger lock blocks billing for the write.
// The live file is small (about 2 MB for 1000 keys, 10–25 ms), so this is
// kept. Ceiling: key count or period detail growing the live file past 10 MB.
// Revisit when a reset takes longer than 50 ms.
func (l *usageLedger) resetWindowAndPersist(id string, window UsageResetWindow, expected *UsageResetExpectation, persist func(through string, keys map[string]liveKey) error) (UsageResetResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	usage := l.entries[id]
	if usage == nil || usage.Cycles == nil {
		return UsageResetResult{}, ErrUnknownKey
	}
	cycles := usage.Cycles
	if l.advanceCyclesLocked(cycles, now) {
		l.markDirtyLocked()
	}
	if expected != nil {
		cycle := cycles.cycle(window)
		if !cycle.StartedAt.Equal(expected.StartedAt) || !l.manualResetDeadline(now, window).Equal(expected.ResetAfterManualAt) {
			return UsageResetResult{}, ErrUsageResetChanged
		}
	}
	previous := cycles.clone()
	previousDirty, previousRevision := l.dirty, l.revision
	result := UsageResetResult{
		KeyID: id, Window: window,
		BeforeDailyUSD: cycles.Daily.used().USD(), BeforeWeeklyUSD: cycles.Weekly.used().USD(), BeforeMonthlyUSD: cycles.Monthly.used().USD(),
	}
	cycle := cycles.cycle(window)
	*cycle = Cycle{StartedAt: now, ResetsAt: l.manualResetDeadline(now, window), ResetKind: "manual", ByModel: make(map[string]Bucket)}
	l.markDirtyLocked()
	result.AfterDailyUSD, result.AfterWeeklyUSD, result.AfterMonthlyUSD = cycles.Daily.used().USD(), cycles.Weekly.used().USD(), cycles.Monthly.used().USD()
	result.NextAccountingBoundaryAt = cycle.ResetsAt
	if err := persist(l.historyThrough, l.liveSnapshotLocked(l.historyThrough)); err != nil {
		usage.Cycles = previous
		l.dirty, l.revision = previousDirty, previousRevision
		return UsageResetResult{}, err
	}
	l.dirty = false
	return result, nil
}

// modelWindows is one model's consumption in the key's three current periods.
type modelWindows struct {
	Name                   string
	Daily, Weekly, Monthly UsageWindow
}

func (l *usageLedger) modelUsage(id string) []modelWindows {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cycles := l.currentCyclesLocked(id, now)
	byModel := make(map[string]*modelWindows)
	for _, window := range quotaWindows {
		cycle := cycles.cycle(window)
		for model, bucket := range cycle.ByModel {
			entry := byModel[model]
			if entry == nil {
				entry = &modelWindows{Name: model}
				byModel[model] = entry
			}
			usage := UsageWindow{UsageTotals: bucket.totals(), WindowStart: cycle.StartedAt}
			switch window {
			case UsageResetDaily:
				entry.Daily = usage
			case UsageResetWeekly:
				entry.Weekly = usage
			case UsageResetMonthly:
				entry.Monthly = usage
			}
		}
	}
	result := make([]modelWindows, 0, len(byModel))
	for _, entry := range byModel {
		result = append(result, *entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type UsageHistoryDay struct {
	Date string `json:"date"`
	UsageTotals
	ByModel map[string]UsageTotals `json:"by_model"`
}

// history returns the last count days, oldest first, with missing days as
// zero rows. The day total is computed from its models.
func (l *usageLedger) history(id string, count int) []UsageHistoryDay {
	count = clampDays(count)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evictExpiredLocked(now)
	usage := l.entries[id]
	end := l.reportEndLocked(now)
	result := make([]UsageHistoryDay, 0, count)
	for offset := -(count - 1); offset <= 0; offset++ {
		date := shiftDate(end, offset)
		day := UsageHistoryDay{Date: date, ByModel: make(map[string]UsageTotals)}
		if usage != nil {
			var total Bucket
			for model, bucket := range usage.Days[date] {
				day.ByModel[model] = bucket.totals()
				total.add(bucket)
			}
			day.UsageTotals = total.totals()
		}
		result = append(result, day)
	}
	return result
}

func clampDays(count int) int {
	if count < 1 {
		return 30
	}
	if count > UsageRetentionDays {
		return UsageRetentionDays
	}
	return count
}

// visitRange calls visit for every bucket dated within [from, to] while the
// lock is held; visit must not block or take other locks.
func (l *usageLedger) visitRange(from, to string, visit func(id, date, model string, bucket Bucket)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictExpiredLocked(l.now())
	for id, usage := range l.entries {
		for date, models := range usage.Days {
			if date < from || date > to {
				continue
			}
			for model, bucket := range models {
				visit(id, date, model, bucket)
			}
		}
	}
}

func (l *usageLedger) keyIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.entries))
	for id := range l.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// copyKeyDays returns one key's buckets within [from, to].
func (l *usageLedger) copyKeyDays(id, from, to string) map[string]map[string]Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	usage := l.entries[id]
	if usage == nil {
		return nil
	}
	return cloneDays(usage.Days, func(date string) bool { return date >= from && date <= to })
}

// flushPlan is one persistence cut taken under the ledger lock.
type flushPlan struct {
	revision uint64
	// through is the watermark the live file carries when no history write
	// happens or it fails; historyThrough is the watermark after a successful
	// history write ("" when none is planned).
	through, historyThrough string
	history                 map[string]map[string]map[string]Bucket
	live                    map[string]liveKey
}

// planFlush decides what the next flush must write. A history write is due
// when a day has closed since the last one, or the file's content went stale;
// it is planned even when the ledger is otherwise clean, so a failed history
// write is retried on the next tick.
func (l *usageLedger) planFlush() (flushPlan, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evictExpiredLocked(now)
	for _, usage := range l.entries {
		if usage.Cycles != nil && l.advanceCyclesLocked(usage.Cycles, now) {
			l.markDirtyLocked()
		}
	}
	yesterday := l.dateKeyOffset(now, -1)
	needHistory := l.historyStale || (yesterday > l.historyThrough && l.hasDaysInRangeLocked(l.historyThrough, yesterday))
	if !l.dirty && !needHistory {
		return flushPlan{}, false
	}
	plan := flushPlan{revision: l.revision, through: l.historyThrough, live: l.liveSnapshotLocked(l.historyThrough)}
	if needHistory {
		// The watermark never moves backwards: after a clock rollback the
		// history keeps the days it already holds.
		plan.historyThrough = max(yesterday, l.historyThrough)
		l.closedThrough = max(l.closedThrough, plan.historyThrough)
		plan.history = l.historySnapshotLocked(plan.historyThrough)
		l.historyStale = false
	}
	return plan, true
}

func (l *usageLedger) hasDaysInRangeLocked(after, through string) bool {
	for _, usage := range l.entries {
		for date, models := range usage.Days {
			if date > after && date <= through && len(models) > 0 {
				return true
			}
		}
	}
	return false
}

// liveSnapshotLocked deep-copies the periods and the days after through.
func (l *usageLedger) liveSnapshotLocked(through string) map[string]liveKey {
	keys := make(map[string]liveKey, len(l.entries))
	for id, usage := range l.entries {
		var days map[string]map[string]Bucket
		for date, models := range usage.Days {
			if date > through && len(models) > 0 {
				if days == nil {
					days = make(map[string]map[string]Bucket, 2)
				}
				days[date] = maps.Clone(models)
			}
		}
		keys[id] = liveKey{Cycles: usage.Cycles.clone(), Balance: usage.Balance.clone(), Days: days}
	}
	return keys
}

// historySnapshotLocked copies each key's date table and shares the closed
// days' model maps, which are never written again (see usageLedger).
func (l *usageLedger) historySnapshotLocked(through string) map[string]map[string]map[string]Bucket {
	keys := make(map[string]map[string]map[string]Bucket, len(l.entries))
	for id, usage := range l.entries {
		var days map[string]map[string]Bucket
		for date, models := range usage.Days {
			if date <= through && len(models) > 0 {
				if days == nil {
					days = make(map[string]map[string]Bucket, len(usage.Days))
				}
				days[date] = models
			}
		}
		if days != nil {
			keys[id] = days
		}
	}
	return keys
}

// markFlushed records what the files now hold once the writes finished,
// successfully or not. The ledger stays dirty when the live write failed
// or records arrived after the plan was taken; a failed history write is
// planned again on the next flush.
func (l *usageLedger) markFlushed(plan flushPlan, historyWritten, liveWritten bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case historyWritten:
		l.historyThrough = plan.historyThrough
	case plan.history != nil:
		l.historyStale = true
	}
	if liveWritten && l.revision == plan.revision {
		l.dirty = false
	}
}

// snapshot copies the whole ledger for tests and tools.
func (l *usageLedger) snapshot() map[string]*KeyUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make(map[string]*KeyUsage, len(l.entries))
	for id, usage := range l.entries {
		result[id] = usage.clone()
	}
	return result
}
