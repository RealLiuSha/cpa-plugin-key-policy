package policy

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	usageRetentionDays = 35
	usageFlushInterval = 15 * time.Second
	softLimitRatio     = 0.8
	dateLayout         = "2006-01-02"
)

type UsageResetWindow string

const (
	UsageResetDaily   UsageResetWindow = "daily"
	UsageResetWeekly  UsageResetWindow = "weekly"
	UsageResetMonthly UsageResetWindow = "monthly"
)

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

// usageLedger owns quota cycles and retained daily consumption history.
// Resets only touch cycles; accounting history survives manual and automatic resets.
type usageLedger struct {
	mu              sync.Mutex
	now             func() time.Time
	location        *time.Location
	timezone        string
	entries         map[string]*UsageState
	dirty           bool
	revision        uint64
	lastEvictedDate string
}

func newUsageLedger(now func() time.Time) *usageLedger {
	location, err := time.LoadLocation(DefaultConfig().UsageTimezone)
	if err != nil {
		location = time.UTC
	}
	return newUsageLedgerWithLocation(now, location, location.String())
}

func newUsageLedgerWithLocation(now func() time.Time, location *time.Location, timezone string) *usageLedger {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = time.UTC
	}
	if strings.TrimSpace(timezone) == "" {
		timezone = location.String()
	}
	return &usageLedger{
		now:      now,
		location: location,
		timezone: timezone,
		entries:  make(map[string]*UsageState),
	}
}

func (l *usageLedger) loadFromState(usage map[string]*UsageState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make(map[string]*UsageState, len(usage))
	for id, state := range usage {
		if state == nil {
			continue
		}
		l.entries[id] = cloneUsageState(state)
	}
	// A replacement snapshot has not been checked even when the accounting date
	// matches the previous one, so force one retention pass after every load.
	l.lastEvictedDate = ""
	if l.evictExpiredLocked(l.now()) {
		l.markDirtyLocked()
	} else {
		l.dirty = false
	}
}

func cloneUsageState(state *UsageState) *UsageState {
	if state == nil {
		return nil
	}
	clone := &UsageState{
		Days:    make(map[string]UsageBucket, len(state.Days)),
		ByModel: make(map[string]map[string]UsageBucket, len(state.ByModel)),
		Cycles:  cloneUsageCycles(state.Cycles),
	}
	for date, bucket := range state.Days {
		clone.Days[date] = bucket
	}
	for model, days := range state.ByModel {
		modelDays := make(map[string]UsageBucket, len(days))
		for date, bucket := range days {
			modelDays[date] = bucket
		}
		clone.ByModel[model] = modelDays
	}
	return clone
}

func (l *usageLedger) entryLocked(id string) *UsageState {
	state := l.entries[id]
	if state == nil {
		state = &UsageState{}
		l.entries[id] = state
	}
	if state.Days == nil {
		state.Days = make(map[string]UsageBucket)
	}
	if state.ByModel == nil {
		state.ByModel = make(map[string]map[string]UsageBucket)
	}
	return state
}

func (l *usageLedger) markDirtyLocked() {
	l.dirty = true
	l.revision++
}

func (l *usageLedger) dateKey(at time.Time) string {
	return at.In(l.location).Format(dateLayout)
}

func (l *usageLedger) startOfDay(at time.Time) time.Time {
	local := at.In(l.location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, l.location)
}

func (l *usageLedger) dateKeyOffset(at time.Time, days int) string {
	return l.startOfDay(at).AddDate(0, 0, days).Format(dateLayout)
}

func addUsageBucket(dst UsageBucket, src UsageBucket) UsageBucket {
	dst.TotalUSD += src.TotalUSD
	dst.CallCount += src.CallCount
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCostUSD += src.CacheCostUSD
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.CacheWriteUSD += src.CacheWriteUSD
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	return dst
}

func sumBuckets(days map[string]UsageBucket, fromKey, toKey string) UsageBucket {
	var total UsageBucket
	dates := make([]string, 0, len(days))
	for date := range days {
		if date >= fromKey && date <= toKey {
			dates = append(dates, date)
		}
	}
	// Stable date order keeps floating-point totals deterministic across reads.
	sort.Strings(dates)
	for _, date := range dates {
		total = addUsageBucket(total, days[date])
	}
	return total
}

func (l *usageLedger) evictExpiredLocked(now time.Time) bool {
	currentDate := l.dateKey(now)
	if currentDate == l.lastEvictedDate {
		return false
	}
	l.lastEvictedDate = currentDate
	oldest := l.dateKeyOffset(now, -(usageRetentionDays - 1))
	changed := false
	for _, state := range l.entries {
		if state == nil {
			continue
		}
		for date := range state.Days {
			if date < oldest {
				delete(state.Days, date)
				changed = true
			}
		}
		for model, days := range state.ByModel {
			for date := range days {
				if date < oldest {
					delete(days, date)
					changed = true
				}
			}
			if len(days) == 0 {
				delete(state.ByModel, model)
			}
		}

	}
	return changed
}

func (l *usageLedger) RecordCost(id, model string, amount, cacheCost float64, cacheReadTokens int64, cacheWriteCost float64, cacheWriteTokens, inputTokens, outputTokens int64, callCount int64) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(model) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.currentEntryLocked(id, now)
	date := l.dateKey(now)
	delta := UsageBucket{
		TotalUSD:         amount,
		CallCount:        callCount,
		CacheReadTokens:  cacheReadTokens,
		CacheCostUSD:     cacheCost,
		CacheWriteTokens: cacheWriteTokens,
		CacheWriteUSD:    cacheWriteCost,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
	}
	for _, window := range quotaWindows {
		cycle := state.Cycles.cycle(window)
		cycle.ByModel[model] = addUsageBucket(cycle.ByModel[model], delta)
	}
	state.Days[date] = addUsageBucket(state.Days[date], delta)
	modelDays := state.ByModel[model]
	if modelDays == nil {
		modelDays = make(map[string]UsageBucket)
		state.ByModel[model] = modelDays
	}
	modelDays[date] = addUsageBucket(modelDays[date], delta)
	l.evictExpiredLocked(now)
	l.markDirtyLocked()
}

type UsageSummary struct {
	Cycles                   []QuotaCycleSummary `json:"cycles"`
	Status                   string              `json:"status"`
	BlockedReason            string              `json:"blocked_reason,omitempty"`
	LimitedModels            []string            `json:"limited_models"`
	DailyUSD                 float64             `json:"daily_usd"`
	WeeklyUSD                float64             `json:"weekly_usd"`
	MonthlyUSD               float64             `json:"monthly_usd"`
	DailyLimitUSD            float64             `json:"daily_limit_usd"`
	WeeklyLimitUSD           float64             `json:"weekly_limit_usd"`
	MonthlyLimitUSD          float64             `json:"monthly_limit_usd"`
	NextAccountingBoundaryAt time.Time           `json:"next_accounting_boundary_at"`
	DailyCacheCostUSD        float64             `json:"daily_cache_cost_usd,omitempty"`
	WeeklyCacheCostUSD       float64             `json:"weekly_cache_cost_usd,omitempty"`
	MonthlyCacheCostUSD      float64             `json:"monthly_cache_cost_usd,omitempty"`
	DailyCacheReadTokens     int64               `json:"daily_cache_read_tokens,omitempty"`
	WeeklyCacheReadTokens    int64               `json:"weekly_cache_read_tokens,omitempty"`
	MonthlyCacheReadTokens   int64               `json:"monthly_cache_read_tokens,omitempty"`
	DailyCacheWriteUSD       float64             `json:"daily_cache_write_usd,omitempty"`
	WeeklyCacheWriteUSD      float64             `json:"weekly_cache_write_usd,omitempty"`
	MonthlyCacheWriteUSD     float64             `json:"monthly_cache_write_usd,omitempty"`
	DailyCacheWriteTokens    int64               `json:"daily_cache_write_tokens,omitempty"`
	WeeklyCacheWriteTokens   int64               `json:"weekly_cache_write_tokens,omitempty"`
	MonthlyCacheWriteTokens  int64               `json:"monthly_cache_write_tokens,omitempty"`
	DailyInputTokens         int64               `json:"daily_input_tokens,omitempty"`
	WeeklyInputTokens        int64               `json:"weekly_input_tokens,omitempty"`
	MonthlyInputTokens       int64               `json:"monthly_input_tokens,omitempty"`
	DailyCallCount           int64               `json:"daily_call_count,omitempty"`
	WeeklyCallCount          int64               `json:"weekly_call_count,omitempty"`
	MonthlyCallCount         int64               `json:"monthly_call_count,omitempty"`
	SoftLimitHit             bool                `json:"soft_limit_hit"`
	Timezone                 string              `json:"timezone"`
	LimitsChangedAt          time.Time           `json:"limits_changed_at,omitempty"`
}

type modelQuotaLimit struct {
	Name     string
	DailyUSD float64
}

// quotaLimits is the complete policy input needed by the accounting ledger.
// Keeping this smaller than the full key policy prevents quota logic from
// depending on routing, secrets, RPM, or management metadata.
type quotaLimits struct {
	DailyUSD        float64
	WeeklyUSD       float64
	MonthlyUSD      float64
	Models          []modelQuotaLimit
	LimitsChangedAt time.Time
}

func (l *usageLedger) windowBucketsLocked(state *UsageState, now time.Time) (UsageBucket, UsageBucket, UsageBucket) {
	if state == nil {
		return UsageBucket{}, UsageBucket{}, UsageBucket{}
	}
	if l.advanceCyclesLocked(state, now) {
		l.markDirtyLocked()
	}
	return cycleTotal(&state.Cycles.Daily), cycleTotal(&state.Cycles.Weekly), cycleTotal(&state.Cycles.Monthly)
}

func limitWarning(used, limit float64) bool {
	return limit > 0 && used >= limit*softLimitRatio
}

func (l *usageLedger) summaryLocked(keyID string, limits quotaLimits, now time.Time) UsageSummary {
	state := l.currentEntryLocked(keyID, now)
	daily, weekly, monthly := l.windowBucketsLocked(state, now)
	summary := UsageSummary{
		DailyUSD:                 daily.TotalUSD,
		WeeklyUSD:                weekly.TotalUSD,
		MonthlyUSD:               monthly.TotalUSD,
		DailyLimitUSD:            limits.DailyUSD,
		WeeklyLimitUSD:           limits.WeeklyUSD,
		MonthlyLimitUSD:          limits.MonthlyUSD,
		NextAccountingBoundaryAt: l.startOfDay(now).AddDate(0, 0, 1),
		DailyCacheCostUSD:        daily.CacheCostUSD,
		WeeklyCacheCostUSD:       weekly.CacheCostUSD,
		MonthlyCacheCostUSD:      monthly.CacheCostUSD,
		DailyCacheReadTokens:     daily.CacheReadTokens,
		WeeklyCacheReadTokens:    weekly.CacheReadTokens,
		MonthlyCacheReadTokens:   monthly.CacheReadTokens,
		DailyCacheWriteUSD:       daily.CacheWriteUSD,
		WeeklyCacheWriteUSD:      weekly.CacheWriteUSD,
		MonthlyCacheWriteUSD:     monthly.CacheWriteUSD,
		DailyCacheWriteTokens:    daily.CacheWriteTokens,
		WeeklyCacheWriteTokens:   weekly.CacheWriteTokens,
		MonthlyCacheWriteTokens:  monthly.CacheWriteTokens,
		DailyInputTokens:         daily.InputTokens,
		WeeklyInputTokens:        weekly.InputTokens,
		MonthlyInputTokens:       monthly.InputTokens,
		DailyCallCount:           daily.CallCount,
		WeeklyCallCount:          weekly.CallCount,
		MonthlyCallCount:         monthly.CallCount,
		Timezone:                 l.timezone,
		LimitsChangedAt:          limits.LimitsChangedAt,
	}
	summary.Status = "normal"
	summary.LimitedModels = []string{}
	limitsByWindow := []float64{limits.DailyUSD, limits.WeeklyUSD, limits.MonthlyUSD}
	summary.NextAccountingBoundaryAt = state.Cycles.Daily.ResetsAt
	for i, window := range quotaWindows {
		cycle := state.Cycles.cycle(window)
		used, limit := cycleTotal(cycle).TotalUSD, limitsByWindow[i]
		summary.Cycles = append(summary.Cycles, QuotaCycleSummary{
			Window: window, StartedAt: cycle.StartedAt, ResetsAt: cycle.ResetsAt, ResetKind: cycle.ResetKind,
			UsedUSD: used, LimitUSD: limit, ResetAfterManualAt: l.startOfDay(now).AddDate(0, 0, window.days()),
		})
		if limitWarning(used, limit) {
			summary.SoftLimitHit = true
		}
		if limit > 0 && used >= limit && summary.BlockedReason == "" {
			summary.BlockedReason = string(window) + "_exceeded"
		}
	}
	for _, model := range limits.Models {
		used := cycleModelBucket(&state.Cycles.Daily, model.Name).TotalUSD
		if limitWarning(used, model.DailyUSD) {
			summary.SoftLimitHit = true
		}
		if model.DailyUSD > 0 && used >= model.DailyUSD {
			summary.LimitedModels = append(summary.LimitedModels, model.Name)
		}
	}
	sort.Strings(summary.LimitedModels)
	switch {
	case summary.BlockedReason != "":
		summary.Status = "limited"
	case len(summary.LimitedModels) > 0:
		summary.Status = "partial"
	case summary.SoftLimitHit:
		summary.Status = "warning"
	}

	return summary
}

func (l *usageLedger) Summary(keyID string, limits quotaLimits) UsageSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.evictExpiredLocked(now) {
		l.markDirtyLocked()
	}
	return l.summaryLocked(keyID, limits, now)
}

func modelHardLimit(limits quotaLimits, requested string) float64 {
	for _, model := range limits.Models {
		if strings.EqualFold(model.Name, requested) {
			return model.DailyUSD
		}
	}
	return 0
}

func (l *usageLedger) OverLimit(keyID, requestedModel string, limits quotaLimits) (string, UsageSummary) {
	modelLimit := modelHardLimit(limits, requestedModel)
	if limits.DailyUSD <= 0 && limits.WeeklyUSD <= 0 && limits.MonthlyUSD <= 0 && modelLimit <= 0 {
		return "", UsageSummary{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.evictExpiredLocked(now) {
		l.markDirtyLocked()
	}
	summary := l.summaryLocked(keyID, limits, now)
	if summary.BlockedReason != "" {
		return summary.BlockedReason, summary
	}
	if modelLimit > 0 && cycleModelBucket(&l.entries[keyID].Cycles.Daily, requestedModel).TotalUSD >= modelLimit {
		return "model_daily_exceeded", summary
	}
	return "", UsageSummary{}
}

func (l *usageLedger) removeKeyUsage(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.entries[id]; ok {
		delete(l.entries, id)
		l.markDirtyLocked()
	}
}

// resetWindowLocked starts a new quota period without changing history or other periods.
func (l *usageLedger) resetWindowLocked(id string, window UsageResetWindow, now time.Time) UsageResetResult {
	state := l.currentEntryLocked(id, now)
	dailyBefore, weeklyBefore, monthlyBefore := l.windowBucketsLocked(state, now)
	cycle := state.Cycles.cycle(window)
	*cycle = UsageCycle{StartedAt: now, ResetsAt: l.startOfDay(now).AddDate(0, 0, window.days()), ResetKind: "manual", ByModel: make(map[string]UsageBucket)}
	l.markDirtyLocked()
	dailyAfter, weeklyAfter, monthlyAfter := l.windowBucketsLocked(state, now)
	return UsageResetResult{
		KeyID: id, Window: window,
		BeforeDailyUSD: dailyBefore.TotalUSD, BeforeWeeklyUSD: weeklyBefore.TotalUSD, BeforeMonthlyUSD: monthlyBefore.TotalUSD,
		AfterDailyUSD: dailyAfter.TotalUSD, AfterWeeklyUSD: weeklyAfter.TotalUSD, AfterMonthlyUSD: monthlyAfter.TotalUSD,
		NextAccountingBoundaryAt: cycle.ResetsAt,
	}
}

// resetWindowAndPersist owns the reset transaction so callers never manipulate
// ledger internals. The persist callback runs while l.mu is held, establishing
// a single accounting cut; on failure the exact prior ledger is restored.
func (l *usageLedger) resetWindowAndPersist(id string, window UsageResetWindow, expected *UsageResetExpectation, persist func(map[string]*UsageState) error) (UsageResetResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if expected != nil {
		cycle := l.currentEntryLocked(id, now).Cycles.cycle(window)
		if !cycle.StartedAt.Equal(expected.StartedAt) || !l.startOfDay(now).AddDate(0, 0, window.days()).Equal(expected.ResetAfterManualAt) {
			return UsageResetResult{}, ErrUsageResetChanged
		}
	}
	previous, hadPrevious := l.entries[id]
	previous = cloneUsageState(previous)
	previousDirty, previousRevision := l.dirty, l.revision
	result := l.resetWindowLocked(id, window, now)
	if err := persist(l.snapshotLocked()); err != nil {
		if hadPrevious {
			l.entries[id] = previous
		} else {
			delete(l.entries, id)
		}
		l.dirty, l.revision = previousDirty, previousRevision
		return UsageResetResult{}, err
	}
	l.dirty = false
	return result, nil
}

type ModelUsageEntry struct {
	Name        string      `json:"name"`
	BillingMode string      `json:"billing_mode,omitempty"`
	Free        bool        `json:"free"`
	PerCallUSD  float64     `json:"per_call_usd,omitempty"`
	InConfig    bool        `json:"in_config"`
	Daily       UsageWindow `json:"daily"`
	Weekly      UsageWindow `json:"weekly"`
	Monthly     UsageWindow `json:"monthly"`
}

func usageWindowFromBucket(bucket UsageBucket, start time.Time) UsageWindow {
	return UsageWindow{
		TotalUSD:         bucket.TotalUSD,
		WindowStart:      start,
		CacheReadTokens:  bucket.CacheReadTokens,
		CacheCostUSD:     bucket.CacheCostUSD,
		CacheWriteTokens: bucket.CacheWriteTokens,
		CacheWriteUSD:    bucket.CacheWriteUSD,
		InputTokens:      bucket.InputTokens,
		OutputTokens:     bucket.OutputTokens,
		CallCount:        bucket.CallCount,
	}
}

func (l *usageLedger) ModelUsage(keyID string, models []ModelDefinition) []ModelUsageEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.evictExpiredLocked(now) {
		l.markDirtyLocked()
	}
	byModel := make(map[string]ModelUsageEntry)
	canonical := make(map[string]string)
	for _, model := range models {
		lower := strings.ToLower(model.Name)
		canonical[lower] = model.Name
		byModel[model.Name] = ModelUsageEntry{
			Name: model.Name, BillingMode: model.BillingMode, Free: model.Free,
			PerCallUSD: model.PerCallUSD, InConfig: true,
		}
	}
	state := l.currentEntryLocked(keyID, now)
	for _, window := range quotaWindows {
		cycle := state.Cycles.cycle(window)
		names := make([]string, 0, len(cycle.ByModel))
		for name := range cycle.ByModel {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, model := range names {
			bucket := cycle.ByModel[model]
			display := model
			if configured, ok := canonical[strings.ToLower(model)]; ok {
				display = configured
			}
			entry, ok := byModel[display]
			if !ok {
				entry = ModelUsageEntry{Name: display}
			}
			usage := usageWindowFromBucket(bucket, cycle.StartedAt)
			switch window {
			case UsageResetDaily:
				entry.Daily = addUsageWindow(entry.Daily, usage)
			case UsageResetWeekly:
				entry.Weekly = addUsageWindow(entry.Weekly, usage)
			case UsageResetMonthly:
				entry.Monthly = addUsageWindow(entry.Monthly, usage)
			}
			byModel[display] = entry
		}
	}

	result := make([]ModelUsageEntry, 0, len(byModel))
	for _, entry := range byModel {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func addUsageWindow(dst, src UsageWindow) UsageWindow {
	dst.TotalUSD += src.TotalUSD
	dst.CallCount += src.CallCount
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCostUSD += src.CacheCostUSD
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.CacheWriteUSD += src.CacheWriteUSD
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	if !src.WindowStart.IsZero() && (dst.WindowStart.IsZero() || src.WindowStart.Before(dst.WindowStart)) {
		dst.WindowStart = src.WindowStart
	}
	return dst
}

type UsageHistoryDay struct {
	Date string `json:"date"`
	UsageBucket
	ByModel map[string]UsageBucket `json:"by_model,omitempty"`
}

func (l *usageLedger) History(keyID string, count int) []UsageHistoryDay {
	if count < 1 {
		count = 30
	}
	if count > usageRetentionDays {
		count = usageRetentionDays
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.evictExpiredLocked(now) {
		l.markDirtyLocked()
	}
	state := l.entries[keyID]
	result := make([]UsageHistoryDay, 0, count)
	for offset := -(count - 1); offset <= 0; offset++ {
		date := l.dateKeyOffset(now, offset)
		day := UsageHistoryDay{Date: date, ByModel: make(map[string]UsageBucket)}
		if state != nil {
			day.UsageBucket = state.Days[date]
			for model, days := range state.ByModel {
				if bucket, ok := days[date]; ok {
					day.ByModel[model] = bucket
				}
			}
		}
		result = append(result, day)
	}
	return result
}

func (l *usageLedger) snapshot() map[string]*UsageState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.evictExpiredLocked(l.now()) {
		l.markDirtyLocked()
	}
	return l.snapshotLocked()
}

func (l *usageLedger) snapshotLocked() map[string]*UsageState {
	result := make(map[string]*UsageState, len(l.entries))
	for id, state := range l.entries {
		result[id] = cloneUsageState(state)
	}
	return result
}

func (l *usageLedger) snapshotForFlush() (map[string]*UsageState, uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.evictExpiredLocked(l.now()) {
		l.markDirtyLocked()
	}
	for _, state := range l.entries {
		if l.advanceCyclesLocked(state, l.now()) {
			l.markDirtyLocked()
		}
	}
	if !l.dirty {
		return nil, l.revision, false
	}
	return l.snapshotLocked(), l.revision, true
}

func (l *usageLedger) markFlushed(revision uint64) {
	l.mu.Lock()
	if l.revision == revision {
		l.dirty = false
	}
	l.mu.Unlock()
}
