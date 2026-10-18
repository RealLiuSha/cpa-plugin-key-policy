package policy

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// usdTotals builds the expected management view of one bucket from
// nanodollar amounts computed by hand from the spec formula
// charge = round(Σ(tokens × USD-per-million) × 1e3 × multiplier).
func usdTotals(calls, unpriced, input, read, write, output int64, charged, readCost, writeCost Nano) UsageTotals {
	return UsageTotals{
		TotalUSD: charged.USD(), CallCount: calls, UnpricedCalls: unpriced,
		InputTokens: input, OutputTokens: output,
		CacheReadTokens: read, CacheCostUSD: readCost.USD(),
		CacheWriteTokens: write, CacheWriteUSD: writeCost.USD(),
	}
}

// The UsageDetail shapes mirror what CPA's parsers deliver for each
// protocol: OpenAI/Codex set CachedTokens = CacheReadTokens from
// prompt_tokens_details.cached_tokens; Claude sets CachedTokens to the
// creation count when nothing was read; Gemini reports thoughts separately
// and cachedContentTokenCount inside promptTokenCount.
func TestTokenBillingGoldenCases(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	write375, write25, zero := 3.75, 2.5, 0.0
	prices := []ModelPrice{
		{Name: "gpt-5.6", BillingMode: "tokens", InputPricePerMillion: 1.25, OutputPricePerMillion: 10, CacheReadPricePerMillion: 0.125},
		{Name: "gpt-5.6-w", BillingMode: "tokens", InputPricePerMillion: 1.25, OutputPricePerMillion: 10, CacheReadPricePerMillion: 0.125, CacheWritePricePerMillion: &write25},
		{Name: "claude-opus", Provider: "claude", BillingMode: "tokens", InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.3, CacheWritePricePerMillion: &write375},
		{Name: "claude-nowrite", Provider: "claude", BillingMode: "tokens", InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.3},
		{Name: "claude-zerowrite", Provider: "claude", BillingMode: "tokens", InputPricePerMillion: 3, OutputPricePerMillion: 15, CacheReadPricePerMillion: 0.3, CacheWritePricePerMillion: &zero},
		{Name: "gemini-2.5-pro", Provider: "gemini", BillingMode: "tokens", InputPricePerMillion: 1, OutputPricePerMillion: 10},
		{Name: "multiplied", BillingMode: "tokens", BillingMultiplier: 1.5, InputPricePerMillion: 1, OutputPricePerMillion: 2},
		{Name: "zero-row", BillingMode: "tokens"},
		{Name: "tiny", BillingMode: "tokens", CacheReadPricePerMillion: 0.0375, CacheWritePricePerMillion: ptr(0.0375)},
	}
	cases := []struct {
		name  string
		event UsageEvent
		want  UsageTotals
	}{
		{
			name:  "openai responses with cached prompt subset",
			event: UsageEvent{Provider: "codex", Model: "gpt-5.6", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 50, CachedTokens: 200, CacheReadTokens: 200, TotalTokens: 1100}},
			// 800×1.25 + 200×0.125 + 100×10 = 2025 µUSD; reasoning is inside output (total = in + out)
			want: usdTotals(1, 0, 800, 200, 0, 100, 2_025_000, 25_000, 0),
		},
		{
			name:  "codex total shows reasoning outside output",
			event: UsageEvent{Provider: "codex", Model: "gpt-5.6", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 50, TotalTokens: 1150}},
			// 1000×1.25 + 150×10 = 2750 µUSD
			want: usdTotals(1, 0, 1000, 0, 0, 150, 2_750_000, 0, 0),
		},
		{
			name:  "openai-compatible executor is subset even for a claude-named provider",
			event: UsageEvent{Provider: "openai-compatible-claude-proxy", ExecutorType: "openaicompatexecutor", Model: "gpt-5.6", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 200, CacheReadTokens: 200, CacheCreationTokens: 400, TotalTokens: 1100}},
			// no cache write price: creation stays inside the 800 billed input tokens
			want: usdTotals(1, 0, 800, 200, 0, 100, 2_025_000, 25_000, 0),
		},
		{
			name:  "subset with cache write price carves writes out of the remaining input",
			event: UsageEvent{Provider: "openai", Model: "gpt-5.6-w", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 200, CacheReadTokens: 200, CacheCreationTokens: 300, TotalTokens: 1100}},
			// 500×1.25 + 200×0.125 + 300×2.5 + 100×10 = 2400 µUSD
			want: usdTotals(1, 0, 500, 200, 300, 100, 2_400_000, 25_000, 750_000),
		},
		{
			name:  "claude cache write only is not a cache read",
			event: UsageEvent{Provider: "claude", ExecutorType: "claudeexecutor", Model: "claude-opus", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 400, CacheCreationTokens: 400, TotalTokens: 1500}},
			// 1000×3 + 400×3.75 + 100×15 = 6000 µUSD
			want: usdTotals(1, 0, 1000, 0, 400, 100, 6_000_000, 0, 1_500_000),
		},
		{
			name:  "claude cache read and write are independent of input",
			event: UsageEvent{Provider: "claude", ExecutorType: "claudeexecutor", Model: "claude-opus", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 2000, CacheReadTokens: 2000, CacheCreationTokens: 400, TotalTokens: 3500}},
			// 1000×3 + 2000×0.3 + 400×3.75 + 100×15 = 6600 µUSD
			want: usdTotals(1, 0, 1000, 2000, 400, 100, 6_600_000, 600_000, 1_500_000),
		},
		{
			name:  "claude without cache write price bills creation as input",
			event: UsageEvent{Provider: "claude", ExecutorType: "claudeexecutor", Model: "claude-nowrite", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 100, CacheReadTokens: 100, CacheCreationTokens: 400, TotalTokens: 1600}},
			// 1400×3 + 100×0.3 + 100×15 = 5730 µUSD
			want: usdTotals(1, 0, 1400, 100, 0, 100, 5_730_000, 30_000, 0),
		},
		{
			name:  "claude with explicit zero cache write price counts free writes",
			event: UsageEvent{Provider: "claude", Model: "claude-zerowrite", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 400, CacheCreationTokens: 400, TotalTokens: 1500}},
			// 1000×3 + 400×0 + 100×15 = 4500 µUSD
			want: usdTotals(1, 0, 1000, 0, 400, 100, 4_500_000, 0, 0),
		},
		{
			name:  "gemini thoughts are output and cached content is read at the input price",
			event: UsageEvent{Provider: "gemini", Model: "gemini-2.5-pro", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 50, CachedTokens: 200, CacheReadTokens: 200, TotalTokens: 1150}},
			// 800×1 + 200×1 (read price 0 → input price) + 150×10 = 2500 µUSD
			want: usdTotals(1, 0, 800, 200, 0, 150, 2_500_000, 200_000, 0),
		},
		{
			name:  "gemini reasoning-only turn is billable output",
			event: UsageEvent{Provider: "gemini", ExecutorType: "vertex", Model: "gemini-2.5-pro", Detail: UsageDetail{ReasoningTokens: 50, TotalTokens: 50}},
			want:  usdTotals(1, 0, 0, 0, 0, 50, 500_000, 0, 0),
		},
		{
			name:  "billing multiplier scales the token charge",
			event: UsageEvent{Provider: "codex", Model: "multiplied", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}},
			// (1000×1 + 100×2) × 1.5 = 1800 µUSD
			want: usdTotals(1, 0, 1000, 0, 0, 100, 1_800_000, 0, 0),
		},
		{
			name:  "model without a price row is free but counted",
			event: UsageEvent{Provider: "codex", Model: "gpt-6-terra", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, CachedTokens: 200, CacheReadTokens: 200, TotalTokens: 1100}},
			want:  usdTotals(1, 1, 800, 200, 0, 100, 0, 0, 0),
		},
		{
			name:  "price row with every price at zero is unpriced",
			event: UsageEvent{Provider: "codex", Model: "zero-row", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}},
			want:  usdTotals(1, 1, 1000, 0, 0, 100, 0, 0, 0),
		},
		{
			name:  "a provider-qualified row does not price another provider's calls",
			event: UsageEvent{Provider: "anthropic", Model: "claude-opus", Detail: UsageDetail{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}},
			want:  usdTotals(1, 1, 1000, 0, 0, 100, 0, 0, 0),
		},
		{
			name:  "negative counts are clamped to zero",
			event: UsageEvent{Provider: "codex", Model: "gpt-5.6", Detail: UsageDetail{InputTokens: 1000, OutputTokens: -100, CacheReadTokens: -5}},
			want:  usdTotals(1, 0, 1000, 0, 0, 0, 1_250_000, 0, 0),
		},
		{
			name:  "the per-record ceiling itself is accepted",
			event: UsageEvent{Provider: "codex", Model: "gpt-6-terra", Detail: UsageDetail{InputTokens: 1 << 31}},
			want:  usdTotals(1, 1, 1<<31, 0, 0, 0, 0, 0, 0),
		},
		{
			name:  "each class rounds on its own and the charge is their sum",
			event: UsageEvent{Provider: "claude", Model: "tiny", Detail: UsageDetail{CacheReadTokens: 1, CacheCreationTokens: 1}},
			// 1×0.0375 = 37.5 nano per class → 38 + 38; the shown cache amounts add up to the charge
			want: usdTotals(1, 0, 0, 1, 1, 0, 76, 38, 38),
		},
		{
			name:  "a model without a price row and without tokens is still a counted call",
			event: UsageEvent{Provider: "openai", Model: "sora-3"},
			want:  usdTotals(1, 1, 0, 0, 0, 0, 0, 0, 0),
		},
	}
	keys := make([]KeyConfig, 0, len(cases))
	for i := range cases {
		keys = append(keys, testKey(t, fmt.Sprintf("case-%02d", i), fmt.Sprintf("cpa_case_%02d", i)))
	}
	store := newStore(t, clock, keys, prices)
	for i, tc := range cases {
		event := tc.event
		event.APIKey = keys[i].ID
		if got := store.RecordUsage(event); got != UsageRecorded {
			t.Errorf("%s: result %q", tc.name, got)
			continue
		}
		_, history, _ := store.UsageHistoryFor(keys[i].ID, 1)
		if got := history[0].ByModel[event.Model]; got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
		if history[0].UsageTotals != tc.want {
			t.Errorf("%s: day total %+v differs from its only model", tc.name, history[0].UsageTotals)
		}
		_, rows, _ := store.ModelUsageFor(keys[i].ID)
		if len(rows) != 1 || rows[0].Daily.UsageTotals != tc.want || rows[0].Weekly.UsageTotals != tc.want || rows[0].Monthly.UsageTotals != tc.want {
			t.Errorf("%s: cycle buckets %+v differ from the day bucket", tc.name, rows)
		}
	}
	if health := store.Health(); health.ClampedTokens != 1 || health.UnpricedCalls != 5 || health.InvalidUsage != 0 || health.IgnoredEmptyUsage != 0 {
		t.Fatalf("health = %+v", health)
	}
}

func TestRecordUsageResultsAndHealth(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "team", "cpa_team")
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newStoreAt(t, clock, statePath, []KeyConfig{key}, []ModelPrice{tokenPrice("gpt-5.6", "", 1, 2)})
	key = keyByID(t, store, "team")
	key.Enabled = false
	if err := store.UpsertKey(key); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		event UsageEvent
		want  UsageResult
	}{
		{"disabled key still bills", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000}), UsageRecorded},
		{"plaintext key lookup", UsageEvent{Provider: "openai", Model: "gpt-5.6", APIKey: "cpa_team", Detail: UsageDetail{OutputTokens: 500_000}}, UsageRecorded},
		{"failed attempt", UsageEvent{Model: "gpt-5.6", APIKey: "team", Failed: true, Detail: UsageDetail{InputTokens: 10}}, UsageSkippedFailed},
		{"unknown key", tokenEvent("nobody", "openai", "gpt-5.6", UsageDetail{InputTokens: 10}), UsageIgnoredUnknownKey},
		{"no model name at all", UsageEvent{APIKey: "team", Detail: UsageDetail{InputTokens: 10}}, UsageIgnoredNoModel},
		{"requested model fallback matches prices without case", UsageEvent{RequestedModel: "GPT-5.6", APIKey: "team", Detail: UsageDetail{InputTokens: 1_000_000}}, UsageRecorded},
		{"negative clamped", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000, OutputTokens: -5}), UsageRecorded},
		{"oversized rejected", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: maxTokensPerRecord + 1}), UsageInvalid},
		{"oversized total is clamped, not rejected", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 1, TotalTokens: maxTokensPerRecord + 1}), UsageRecorded},
		{"empty after sanitizing", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{TotalTokens: 5, CachedTokens: 5, InputTokens: -1}), UsageIgnoredEmpty},
		{"reasoning the total does not show is inside output, so nothing is billable", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{ReasoningTokens: 50}), UsageIgnoredEmpty},
		{"cache reads beyond the input are not billable", tokenEvent("team", "openai", "gpt-5.6", UsageDetail{CacheReadTokens: 500, CachedTokens: 500, TotalTokens: 500}), UsageIgnoredEmpty},
		{"unpriced model keyed without case", tokenEvent("team", "openai", "GPT-6-Terra", UsageDetail{InputTokens: 100}), UsageRecorded},
		{"unpriced model without tokens is counted", tokenEvent("team", "openai", "gpt-6-terra", UsageDetail{}), UsageRecorded},
	}
	for _, tc := range cases {
		if got := store.RecordUsage(tc.event); got != tc.want {
			t.Errorf("%s: result %q, want %q", tc.name, got, tc.want)
		}
	}
	summary := store.UsageSummaryFor(keyByID(t, store, "team"))
	// 1 + 1 + 1 (fallback) + 1 (clamped) + 0.000001 (clamped total) = $4.000001; unpriced adds $0
	if !nearly(summary.DailyUSD, 4.000001) || summary.Status != "disabled" {
		t.Fatalf("summary = %+v", summary)
	}
	health := store.Health()
	want := HealthSnapshot{SkippedFailed: 1, IgnoredUnknownKey: 1, IgnoredNoModel: 1, IgnoredEmptyUsage: 3, ClampedTokens: 3, InvalidUsage: 1, UnpricedCalls: 2}
	if health != want {
		t.Fatalf("health = %+v, want %+v", health, want)
	}
	// Buckets are keyed by the lowercased id, so the differently cased
	// records share one row each.
	_, rows, _ := store.ModelUsageFor("team")
	if len(rows) != 2 || rows[0].Name != "gpt-5.6" || !rows[0].Priced || rows[0].Daily.CallCount != 5 || rows[1].Name != "gpt-6-terra" || rows[1].Priced || rows[1].Daily.UnpricedCalls != 2 {
		t.Fatalf("model rows = %+v", rows)
	}
	unpriced := store.UnpricedModels()
	if len(unpriced) != 1 || unpriced[0] != (UnpricedModel{Name: "gpt-6-terra", Calls: 2, LastSeen: "2026-10-10"}) {
		t.Fatalf("unpriced = %+v", unpriced)
	}
	// A provider-qualified row may still miss calls from other providers, so
	// the model stays listed and the list says a qualified row exists; an
	// unqualified row catches every call and removes it from the list.
	if err := store.UpsertModel(tokenPrice("gpt-6-terra", "openai", 0, 0)); err != nil {
		t.Fatal(err)
	}
	if got := store.UnpricedModels(); len(got) != 1 || !got[0].HasQualifiedRows {
		t.Fatalf("unpriced after adding a qualified row = %+v", got)
	}
	if err := store.UpsertModel(tokenPrice("gpt-6-terra", "", 0, 0)); err != nil {
		t.Fatal(err)
	}
	if got := store.UnpricedModels(); len(got) != 0 {
		t.Fatalf("unpriced after adding an unqualified row = %+v", got)
	}
	if err := store.Configure(Config{Enabled: false, StateFile: statePath, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	if got := store.RecordUsage(tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000})); got != UsageIgnoredDisabled {
		t.Fatalf("disabled plugin result = %q", got)
	}
	// Reconfiguring flushed the ledger; the decline counters must not move.
	if got := store.Health(); got.LastFlushAt == "" || (func() HealthSnapshot { got.LastFlushAt = ""; return got })() != want {
		t.Fatalf("disabled plugin touched health counters: %+v", got)
	}
}

func TestPerCallBillsOnlyAdmittedGenerations(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "img", "cpa_img")
	key.DailyLimitUSD = 2
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{perCallPrice("grok-imagine-video", "xai", 1)})
	headers := bearer("cpa_img")
	if decision := store.Authenticate("POST", "/v1/videos", headers, nil); !decision.Allowed || !decision.Deferred {
		t.Fatalf("generation auth = %+v", decision)
	}
	if denial := store.AdmitIntercepted(headers, "/v1/videos", "trace-1"); denial != nil {
		t.Fatalf("generation admission = %+v", denial)
	}
	// A reload while the generation runs must not forget the admitted trace.
	if err := store.Configure(Config{Enabled: true, StateFile: store.StatePath(), UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	// The generation's usage record carries whatever tokens the upstream
	// reported; per-call billing records the call and the fixed charge only.
	generation := UsageEvent{TraceID: "trace-1", Provider: "xai", Model: "grok-imagine-video", APIKey: "img", Detail: UsageDetail{InputTokens: 77, OutputTokens: 5}}
	if got := store.RecordUsage(generation); got != UsageRecorded {
		t.Fatalf("generation record = %q", got)
	}
	if got := store.RecordUsage(generation); got != UsageUnmeteredPerCall {
		t.Fatalf("second record with the same trace = %q, want unmetered", got)
	}
	_, history, _ := store.UsageHistoryFor("img", 1)
	if got := history[0].ByModel["grok-imagine-video"]; got != usdTotals(1, 0, 0, 0, 0, 0, 1_000_000_000, 0, 0) {
		t.Fatalf("per-call bucket = %+v", got)
	}
	// Result polling runs through the same executor and reports usage with
	// the default video model, but its trace was never admitted as a generation.
	polling := UsageEvent{TraceID: "trace-poll", Provider: "xai", Model: "grok-imagine-video", APIKey: "img"}
	if denial := store.AdmitIntercepted(headers, "/v1/videos/:request_id", "trace-poll"); denial != nil {
		t.Fatalf("polling admission = %+v", denial)
	}
	if got := store.RecordUsage(polling); got != UsageUnmeteredPerCall {
		t.Fatalf("polling record = %q, want unmetered", got)
	}
	if denial := store.AdmitIntercepted(headers, "/v1/videos", "trace-2"); denial != nil {
		t.Fatalf("second generation admission = %+v", denial)
	}
	clock.Advance(mediaTraceTTL + time.Second)
	if got := store.RecordUsage(UsageEvent{TraceID: "trace-2", Provider: "xai", Model: "grok-imagine-video", APIKey: "img"}); got != UsageUnmeteredPerCall {
		t.Fatalf("expired trace record = %q, want unmetered", got)
	}
	if got := store.RecordUsage(UsageEvent{Provider: "xai", Model: "grok-imagine-video", APIKey: "img", Detail: UsageDetail{InputTokens: 5}}); got != UsageUnmeteredPerCall {
		t.Fatalf("record without trace id = %q", got)
	}
	if denial := store.AdmitIntercepted(headers, "/v1/videos", "trace-failed"); denial != nil {
		t.Fatalf("third generation admission = %+v", denial)
	}
	if got := store.RecordUsage(UsageEvent{TraceID: "trace-failed", Provider: "xai", Model: "grok-imagine-video", APIKey: "img", Failed: true}); got != UsageSkippedFailed {
		t.Fatalf("failed generation = %q", got)
	}
	summary := store.UsageSummaryFor(keyByID(t, store, "img"))
	if summary.DailyUSD != 1 || store.Health().UnmeteredPerCall != 4 || store.Health().SkippedFailed != 1 {
		t.Fatalf("summary = %+v health = %+v", summary, store.Health())
	}
	if decision := store.Authenticate("POST", "/v1/videos", nil, map[string][]string{"key": {"cpa_img"}}); decision.Allowed || decision.Reason != "media_generation_requires_header_key" {
		t.Fatalf("query-key generation = %+v, want rejected", decision)
	}
	if decision := store.Authenticate("POST", "/v1/images/generations", nil, map[string][]string{"api_key": {"cpa_img"}}); decision.Allowed || decision.Reason != "media_generation_requires_header_key" {
		t.Fatalf("query-key image generation = %+v, want rejected", decision)
	}
	// A header key on a media route CPA does not intercept is admitted here.
	if decision := store.Authenticate("POST", "/openai/v1/images/generations", headers, nil); !decision.Allowed || decision.Deferred {
		t.Fatalf("header-key non-intercepted media route = %+v, want admitted at auth", decision)
	}
}

// The trace set is a bounded correlation aid: it forgets the oldest
// generation first, and forgetting only ever makes a call free.
func TestMediaTraceSetEvictsOldestBeyondCapacity(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "img", "cpa_img")}, []ModelPrice{perCallPrice("grok-imagine-image", "xai", 0.07)})
	headers := bearer("cpa_img")
	for i := 0; i <= mediaTraceCapacity; i++ {
		if denial := store.AdmitIntercepted(headers, "/v1/images/generations", fmt.Sprintf("trace-%d", i)); denial != nil {
			t.Fatal(denial)
		}
	}
	event := func(trace string) UsageEvent {
		return UsageEvent{TraceID: trace, Provider: "xai", Model: "grok-imagine-image", APIKey: "img"}
	}
	if got := store.RecordUsage(event("trace-0")); got != UsageUnmeteredPerCall {
		t.Fatalf("oldest trace beyond capacity = %q, want forgotten", got)
	}
	if got := store.RecordUsage(event("trace-1")); got != UsageRecorded {
		t.Fatalf("second oldest trace = %q, want still remembered", got)
	}
	if got := store.RecordUsage(event(fmt.Sprintf("trace-%d", mediaTraceCapacity))); got != UsageRecorded {
		t.Fatalf("newest trace = %q", got)
	}
	if summary := store.UsageSummaryFor(keyByID(t, store, "img")); !nearly(summary.DailyUSD, 0.14) {
		t.Fatalf("summary = %+v", summary)
	}
}
