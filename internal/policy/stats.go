package policy

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	statsKeysLimit     = 200
	statsCSVMaxDays    = 92
	statsCSVMaxRows    = 200000
	statsCSVColumnsRow = "date,key_id,key_name,deleted,model,calls,unpriced_calls,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,charged_usd"
)

var (
	ErrInvalidDateRange = errors.New("invalid date range")
	// ErrRangeTooLarge means the export exceeds 92 days or 200000 rows.
	ErrRangeTooLarge = errors.New("date range too large")
)

type StatsTotals struct {
	ChargedUSD       float64 `json:"charged_usd"`
	Calls            int64   `json:"calls"`
	UnpricedCalls    int64   `json:"unpriced_calls"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
}

func (b Bucket) statsTotals() StatsTotals {
	return StatsTotals{
		ChargedUSD: b.ChargedNano.USD(), Calls: b.Calls, UnpricedCalls: b.UnpricedCalls,
		InputTokens: b.InputTokens, CacheReadTokens: b.CacheReadTokens, CacheWriteTokens: b.CacheWriteTokens, OutputTokens: b.OutputTokens,
	}
}

type StatsDay struct {
	Date string `json:"date"`
	StatsTotals
}

type StatsModel struct {
	Name string `json:"name"`
	// Priced is false when any call in the range was billed without a price
	// (see the note above UnpricedModel).
	Priced bool `json:"priced"`
	StatsTotals
}

type StatsKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	StatsTotals
}

type StatsReport struct {
	Timezone       string          `json:"timezone"`
	From           string          `json:"from"`
	To             string          `json:"to"`
	Totals         StatsTotals     `json:"totals"`
	Days           []StatsDay      `json:"days"`
	Models         []StatsModel    `json:"models"`
	Keys           []StatsKey      `json:"keys"`
	KeysTotal      int             `json:"keys_total"`
	KeysTruncated  bool            `json:"keys_truncated"`
	UnpricedModels []UnpricedModel `json:"unpriced_models"`
}

// Stats aggregates the last days (1..90, default 30) across all keys with
// per-day, per-model and per-key accumulators only. The aggregation runs
// outside the ledger lock (see visitRange).
func (s *Store) Stats(days int) StatsReport {
	days = clampDays(days)
	s.mu.RLock()
	names := make(map[string]string, len(s.keys))
	for id, key := range s.keys {
		names[id] = key.Name
	}
	prices := s.prices
	s.mu.RUnlock()
	_, ledger := s.runtimeComponents()
	from, to := ledger.reportRange(days)

	var totals Bucket
	byDay := make(map[string]*Bucket, days)
	byModel := make(map[string]*Bucket)
	byKey := make(map[string]*Bucket)
	accumulate := func(index map[string]*Bucket, name string, bucket Bucket) {
		entry := index[name]
		if entry == nil {
			entry = &Bucket{}
			index[name] = entry
		}
		entry.add(bucket)
	}
	ledger.visitRange(from, to, func(id, date, model string, bucket Bucket) {
		totals.add(bucket)
		accumulate(byDay, date, bucket)
		accumulate(byModel, model, bucket)
		accumulate(byKey, id, bucket)
	})

	report := StatsReport{Timezone: ledger.timezone, From: from, To: to, Totals: totals.statsTotals(), Days: make([]StatsDay, 0, days)}
	for offset := 0; offset < days; offset++ {
		date := shiftDate(from, offset)
		day := StatsDay{Date: date}
		if bucket := byDay[date]; bucket != nil {
			day.StatsTotals = bucket.statsTotals()
		}
		report.Days = append(report.Days, day)
	}
	report.Models = make([]StatsModel, 0, len(byModel))
	for name, bucket := range byModel {
		price, ok := prices.rowByName(name)
		report.Models = append(report.Models, StatsModel{Name: name, Priced: pricedCalls(bucket.Calls, bucket.UnpricedCalls, ok && price.Priced()), StatsTotals: bucket.statsTotals()})
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].ChargedUSD != report.Models[j].ChargedUSD {
			return report.Models[i].ChargedUSD > report.Models[j].ChargedUSD
		}
		return report.Models[i].Name < report.Models[j].Name
	})
	keys := make([]StatsKey, 0, len(byKey))
	for id, bucket := range byKey {
		name, exists := names[id]
		keys = append(keys, StatsKey{ID: id, Name: name, Deleted: !exists, StatsTotals: bucket.statsTotals()})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ChargedUSD != keys[j].ChargedUSD {
			return keys[i].ChargedUSD > keys[j].ChargedUSD
		}
		return keys[i].ID < keys[j].ID
	})
	report.KeysTotal = len(keys)
	if len(keys) > statsKeysLimit {
		keys = keys[:statsKeysLimit]
		report.KeysTruncated = true
	}
	report.Keys = keys
	report.UnpricedModels = s.unpricedModels(ledger, from, to)
	return report
}

// StatsCSVFilename names the export for the Content-Disposition header.
func StatsCSVFilename(from, to string) string {
	return "cpa-key-policy-usage-" + from + "_" + to + ".csv"
}

// StatsCSV exports every (day, key, model) bucket within [from, to] (natural
// days, inclusive, at most 92) as CSV, key by key in id order. More than
// 200000 rows is refused with ErrRangeTooLarge. Amounts are exact decimals.
func (s *Store) StatsCSV(from, to string) ([]byte, error) {
	fromDay, err := time.Parse(dateLayout, from)
	if err != nil {
		return nil, fmt.Errorf("%w: from %q", ErrInvalidDateRange, from)
	}
	toDay, err := time.Parse(dateLayout, to)
	if err != nil {
		return nil, fmt.Errorf("%w: to %q", ErrInvalidDateRange, to)
	}
	if toDay.Before(fromDay) {
		return nil, fmt.Errorf("%w: to is before from", ErrInvalidDateRange)
	}
	if toDay.Sub(fromDay) >= statsCSVMaxDays*24*time.Hour {
		return nil, fmt.Errorf("%w: at most %d days", ErrRangeTooLarge, statsCSVMaxDays)
	}
	s.mu.RLock()
	names := make(map[string]string, len(s.keys))
	for id, key := range s.keys {
		names[id] = key.Name
	}
	s.mu.RUnlock()
	_, ledger := s.runtimeComponents()

	rows := 0
	ledger.visitRange(from, to, func(string, string, string, Bucket) { rows++ })
	if rows > statsCSVMaxRows {
		return nil, fmt.Errorf("%w: %d rows exceed %d", ErrRangeTooLarge, rows, statsCSVMaxRows)
	}
	var buffer bytes.Buffer
	buffer.Grow(rows*96 + len(statsCSVColumnsRow))
	writer := csv.NewWriter(&buffer)
	_ = writer.Write(strings.Split(statsCSVColumnsRow, ","))
	for _, id := range ledger.keyIDs() {
		days := ledger.copyKeyDays(id, from, to)
		if len(days) == 0 {
			continue
		}
		name, exists := names[id]
		deleted := strconv.FormatBool(!exists)
		dates := make([]string, 0, len(days))
		for date := range days {
			dates = append(dates, date)
		}
		sort.Strings(dates)
		for _, date := range dates {
			models := make([]string, 0, len(days[date]))
			for model := range days[date] {
				models = append(models, model)
			}
			sort.Strings(models)
			for _, model := range models {
				bucket := days[date][model]
				_ = writer.Write([]string{
					date, csvText(id), csvText(name), deleted, csvText(model),
					strconv.FormatInt(bucket.Calls, 10), strconv.FormatInt(bucket.UnpricedCalls, 10),
					strconv.FormatInt(bucket.InputTokens, 10), strconv.FormatInt(bucket.CacheReadTokens, 10),
					strconv.FormatInt(bucket.CacheWriteTokens, 10), strconv.FormatInt(bucket.OutputTokens, 10),
					bucket.ChargedNano.FormatUSD(),
				})
			}
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// csvText neutralizes spreadsheet formula injection in free-text cells.
func csvText(value string) string {
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}
