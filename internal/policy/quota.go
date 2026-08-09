package policy

import (
	"math"
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

// usageLedger owns the in-memory accounting time series. All persisted
// counters are natural-day buckets; daily, trailing-7-day and trailing-30-day
// windows are derived through the same sum path.
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
	// Stable date order makes floating-point totals deterministic. The migration
	// CLI relies on byte-identical reports when the same snapshot is run twice.
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
	for id, state := range l.entries {
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
		if len(state.Days) == 0 && len(state.ByModel) == 0 {
			delete(l.entries, id)
		}
	}
	return changed
}

func (l *usageLedger) RecordCost(id, model string, amount, cacheCost float64, cacheReadTokens, inputTokens, outputTokens int64, callCount int64) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(model) == "" {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.entryLocked(id)
	date := l.dateKey(now)
	delta := UsageBucket{
		TotalUSD:        amount,
		CallCount:       callCount,
		CacheReadTokens: cacheReadTokens,
		CacheCostUSD:    cacheCost,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
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
	DailyUSD                 float64   `json:"daily_usd"`
	WeeklyUSD                float64   `json:"weekly_usd"`
	MonthlyUSD               float64   `json:"monthly_usd"`
	DailyLimitUSD            float64   `json:"daily_limit_usd"`
	WeeklyLimitUSD           float64   `json:"weekly_limit_usd"`
	MonthlyLimitUSD          float64   `json:"monthly_limit_usd"`
	NextAccountingBoundaryAt time.Time `json:"next_accounting_boundary_at"`
	DailyCacheCostUSD        float64   `json:"daily_cache_cost_usd,omitempty"`
	WeeklyCacheCostUSD       float64   `json:"weekly_cache_cost_usd,omitempty"`
	MonthlyCacheCostUSD      float64   `json:"monthly_cache_cost_usd,omitempty"`
	DailyCacheReadTokens     int64     `json:"daily_cache_read_tokens,omitempty"`
	WeeklyCacheReadTokens    int64     `json:"weekly_cache_read_tokens,omitempty"`
	MonthlyCacheReadTokens   int64     `json:"monthly_cache_read_tokens,omitempty"`
	DailyInputTokens         int64     `json:"daily_input_tokens,omitempty"`
	WeeklyInputTokens        int64     `json:"weekly_input_tokens,omitempty"`
	MonthlyInputTokens       int64     `json:"monthly_input_tokens,omitempty"`
	DailyCallCount           int64     `json:"daily_call_count,omitempty"`
	WeeklyCallCount          int64     `json:"weekly_call_count,omitempty"`
	MonthlyCallCount         int64     `json:"monthly_call_count,omitempty"`
	SoftLimitHit             bool      `json:"soft_limit_hit"`
	Timezone                 string    `json:"timezone"`
	LimitsChangedAt          time.Time `json:"limits_changed_at,omitempty"`
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
	today := l.dateKey(now)
	return sumBuckets(state.Days, today, today),
		sumBuckets(state.Days, l.dateKeyOffset(now, -6), today),
		sumBuckets(state.Days, l.dateKeyOffset(now, -29), today)
}

func limitWarning(used, limit float64) bool {
	return limit > 0 && used >= limit*softLimitRatio
}

func (l *usageLedger) summaryLocked(keyID string, limits quotaLimits, now time.Time) UsageSummary {
	daily, weekly, monthly := l.windowBucketsLocked(l.entries[keyID], now)
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
		DailyInputTokens:         daily.InputTokens,
		WeeklyInputTokens:        weekly.InputTokens,
		MonthlyInputTokens:       monthly.InputTokens,
		DailyCallCount:           daily.CallCount,
		WeeklyCallCount:          weekly.CallCount,
		MonthlyCallCount:         monthly.CallCount,
		Timezone:                 l.timezone,
		LimitsChangedAt:          limits.LimitsChangedAt,
	}
	summary.SoftLimitHit = limitWarning(summary.DailyUSD, limits.DailyUSD) ||
		limitWarning(summary.WeeklyUSD, limits.WeeklyUSD) ||
		limitWarning(summary.MonthlyUSD, limits.MonthlyUSD)
	if state := l.entries[keyID]; state != nil {
		for _, model := range limits.Models {
			if limitWarning(modelBucketForDate(state, model.Name, l.dateKey(now)).TotalUSD, model.DailyUSD) {
				summary.SoftLimitHit = true
				break
			}
		}
	}
	return summary
}

func (l *usageLedger) Summary(keyID string, limits quotaLimits) UsageSummary {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
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
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.evictExpiredLocked(now) {
		l.markDirtyLocked()
	}
	summary := l.summaryLocked(keyID, limits, now)
	if limits.DailyUSD > 0 && summary.DailyUSD >= limits.DailyUSD {
		return "daily_exceeded", summary
	}
	if limits.WeeklyUSD > 0 && summary.WeeklyUSD >= limits.WeeklyUSD {
		return "weekly_exceeded", summary
	}
	if limits.MonthlyUSD > 0 && summary.MonthlyUSD >= limits.MonthlyUSD {
		return "monthly_exceeded", summary
	}
	if modelLimit > 0 {
		if state := l.entries[keyID]; state != nil && modelBucketForDate(state, requestedModel, l.dateKey(now)).TotalUSD >= modelLimit {
			return "model_daily_exceeded", summary
		}
	}
	return "", UsageSummary{}
}

func modelBucketForDate(state *UsageState, model, date string) UsageBucket {
	var total UsageBucket
	for name, days := range state.ByModel {
		if strings.EqualFold(name, model) {
			total = addUsageBucket(total, sumBuckets(days, date, date))
		}
	}
	return total
}

func (l *usageLedger) resetUsage(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.entries[id]; ok {
		delete(l.entries, id)
		l.markDirtyLocked()
	}
}

func deleteBucketRange(days map[string]UsageBucket, fromKey, toKey string) {
	for date := range days {
		if date >= fromKey && date <= toKey {
			delete(days, date)
		}
	}
}

// resetWindowLocked applies an in-memory reset. Caller must hold l.mu.
func (l *usageLedger) resetWindowLocked(id string, window UsageResetWindow, now time.Time) UsageResetResult {
	state := l.entryLocked(id)
	dailyBefore, weeklyBefore, monthlyBefore := l.windowBucketsLocked(state, now)
	today := l.dateKey(now)
	from := today
	if window == UsageResetWeekly {
		from = l.dateKeyOffset(now, -6)
	} else if window == UsageResetMonthly {
		from = l.dateKeyOffset(now, -29)
	}
	deleteBucketRange(state.Days, from, today)
	for model, days := range state.ByModel {
		deleteBucketRange(days, from, today)
		if len(days) == 0 {
			delete(state.ByModel, model)
		}
	}
	l.markDirtyLocked()
	dailyAfter, weeklyAfter, monthlyAfter := l.windowBucketsLocked(state, now)
	nextMidnight := l.startOfDay(now).AddDate(0, 0, 1)
	return UsageResetResult{
		KeyID:                    id,
		Window:                   window,
		BeforeDailyUSD:           dailyBefore.TotalUSD,
		BeforeWeeklyUSD:          weeklyBefore.TotalUSD,
		BeforeMonthlyUSD:         monthlyBefore.TotalUSD,
		AfterDailyUSD:            dailyAfter.TotalUSD,
		AfterWeeklyUSD:           weeklyAfter.TotalUSD,
		AfterMonthlyUSD:          monthlyAfter.TotalUSD,
		NextAccountingBoundaryAt: nextMidnight,
	}
}

// resetWindowAndPersist owns the reset transaction so callers never manipulate
// ledger internals. The persist callback runs while l.mu is held, establishing
// a single accounting cut; on failure the exact prior ledger is restored.
func (l *usageLedger) resetWindowAndPersist(id string, window UsageResetWindow, persist func(map[string]*UsageState) error) (UsageResetResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous, hadPrevious := l.entries[id]
	previous = cloneUsageState(previous)
	previousDirty, previousRevision := l.dirty, l.revision
	result := l.resetWindowLocked(id, window, l.now())
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
		TotalUSD:        bucket.TotalUSD,
		WindowStart:     start,
		CacheReadTokens: bucket.CacheReadTokens,
		CacheCostUSD:    bucket.CacheCostUSD,
		InputTokens:     bucket.InputTokens,
		OutputTokens:    bucket.OutputTokens,
		CallCount:       bucket.CallCount,
	}
}

func (l *usageLedger) ModelUsage(keyID string, models []ModelDefinition) []ModelUsageEntry {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
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
	if state := l.entries[keyID]; state != nil {
		for model, days := range state.ByModel {
			display := model
			if configured, ok := canonical[strings.ToLower(model)]; ok {
				display = configured
			}
			entry, ok := byModel[display]
			if !ok {
				entry = ModelUsageEntry{Name: display}
			}
			today := l.dateKey(now)
			entry.Daily = addUsageWindow(entry.Daily, usageWindowFromBucket(sumBuckets(days, today, today), l.startOfDay(now)))
			entry.Weekly = addUsageWindow(entry.Weekly, usageWindowFromBucket(sumBuckets(days, l.dateKeyOffset(now, -6), today), l.startOfDay(now).AddDate(0, 0, -6)))
			entry.Monthly = addUsageWindow(entry.Monthly, usageWindowFromBucket(sumBuckets(days, l.dateKeyOffset(now, -29), today), l.startOfDay(now).AddDate(0, 0, -29)))
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

type UsageMigrationTotals struct {
	DailyUSD   float64  `json:"daily_usd"`
	WeeklyUSD  float64  `json:"weekly_usd"`
	MonthlyUSD *float64 `json:"monthly_usd"`
}

// SummarizeUsageStates computes deterministic window totals for an offline
// migration report without mutating the supplied states.
func SummarizeUsageStates(states map[string]*UsageState, now time.Time, location *time.Location, timezone string) map[string]UsageMigrationTotals {
	ledger := newUsageLedgerWithLocation(func() time.Time { return now }, location, timezone)
	ledger.loadFromState(states)
	result := make(map[string]UsageMigrationTotals, len(states))
	for id := range states {
		summary := ledger.Summary(id, quotaLimits{})
		monthly := roundedUSD(summary.MonthlyUSD)
		result[id] = UsageMigrationTotals{DailyUSD: roundedUSD(summary.DailyUSD), WeeklyUSD: roundedUSD(summary.WeeklyUSD), MonthlyUSD: &monthly}
	}
	return result
}

func roundedUSD(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func (l *usageLedger) History(keyID string, count int) []UsageHistoryDay {
	if count < 1 {
		count = 30
	}
	if count > usageRetentionDays {
		count = usageRetentionDays
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
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
