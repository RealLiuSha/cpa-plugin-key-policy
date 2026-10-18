package policy

import (
	"strings"
	"sync"
	"time"
)

// UsageEvent is one finished upstream call as the host reports it after the
// response completed, with the token counts already parsed.
type UsageEvent struct {
	// TraceID identifies the inbound HTTP request; it links a media
	// generation's usage record to the request the interceptor admitted.
	TraceID      string
	Provider     string
	ExecutorType string
	// Model is the upstream model id; RequestedModel is the name the client
	// sent and is only used when Model is empty.
	Model          string
	RequestedModel string
	// APIKey is the key id or the plaintext key, whichever the host has.
	APIKey string
	Failed bool
	Detail UsageDetail
}

// UsageDetail is the token breakdown delivered by the host, already parsed
// from the upstream response (including the final usage frame of a stream).
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// UsageResult says what RecordUsage did with an event. Every value other
// than UsageRecorded is also counted in Health.
type UsageResult string

const (
	UsageRecorded          UsageResult = "recorded"
	UsageIgnoredDisabled   UsageResult = "ignored_disabled"
	UsageSkippedFailed     UsageResult = "skipped_failed"
	UsageIgnoredUnknownKey UsageResult = "ignored_unknown_key"
	UsageIgnoredNoModel    UsageResult = "ignored_no_model"
	UsageInvalid           UsageResult = "invalid_usage"
	UsageIgnoredEmpty      UsageResult = "ignored_empty_usage"
	UsageUnmeteredPerCall  UsageResult = "unmetered_per_call"
)

// RecordUsage bills one event. The key need not still be enabled: the
// request was authorized when it started, and disabling the key midway
// must not make its last call free. Failed attempts, including streams the
// client cancelled, are never billed.
func (s *Store) RecordUsage(event UsageEvent) UsageResult {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	s.mu.RLock()
	enabled, prices, ledger, traces, health := s.enabled, s.prices, s.usage, s.mediaTraces, s.health
	s.mu.RUnlock()
	if !enabled {
		return UsageIgnoredDisabled
	}
	if event.Failed {
		health.skippedFailed.Add(1)
		return UsageSkippedFailed
	}
	key := s.findByID(event.APIKey)
	if key == nil {
		key = s.findBySecret(event.APIKey)
	}
	if key == nil {
		health.ignoredUnknownKey.Add(1)
		return UsageIgnoredUnknownKey
	}
	model := strings.TrimSpace(event.Model)
	if model == "" {
		model = strings.TrimSpace(event.RequestedModel)
	}
	if model == "" {
		health.ignoredNoModel.Add(1)
		return UsageIgnoredNoModel
	}
	detail, clamped, invalid := sanitizeDetail(event.Detail)
	if invalid {
		health.invalidUsage.Add(1)
		return UsageInvalid
	}
	if clamped {
		health.clampedTokens.Add(1)
	}
	price, found := prices.lookup(event.Provider, model)
	priced := found && price.Priced()
	// The ledger key is the lowercased upstream id, never a price row's
	// spelling: rows match without case and may be provider-qualified, so the
	// row that priced a call must not split one model into several buckets.
	model = strings.ToLower(model)
	if !found {
		price = ModelPrice{BillingMode: "tokens", BillingMultiplier: 1}
	}
	var bucket Bucket
	if price.BillingMode == "per_call" {
		// Only the usage record of the generation request itself is billed;
		// result polling, an expired trace or a host without trace ids
		// produce records that cannot be told apart and are left unmetered.
		if event.TraceID == "" || !traces.take(event.TraceID) {
			health.unmeteredPerCall.Add(1)
			return UsageUnmeteredPerCall
		}
		bucket = Bucket{Calls: 1, ChargedNano: USDToNano(price.PerCallUSD)}
	} else {
		bucket = billTokens(price, tokenSemanticsFor(event.Provider, event.ExecutorType), detail)
		// A priced model that reports nothing billable has nothing to charge.
		// An unpriced one is still counted: a new media model whose records
		// carry no tokens must show up in the unpriced list to get a price.
		if priced && bucket.InputTokens == 0 && bucket.CacheReadTokens == 0 && bucket.CacheWriteTokens == 0 && bucket.OutputTokens == 0 {
			health.ignoredEmptyUsage.Add(1)
			return UsageIgnoredEmpty
		}
	}
	if !priced {
		bucket.UnpricedCalls = 1
		health.unpricedCalls.Add(1)
	}
	ledger.record(key.ID, model, bucket, key.BalanceMode())
	return UsageRecorded
}

// sanitizeDetail is the only boundary for upstream token counts: negatives
// become zero, and a billed count above the per-record ceiling rejects the
// record. TotalTokens and CachedTokens are not billed, so an absurd value
// there is dropped like a negative one instead of making the call free.
// After it, billing relies on every billed count being within
// [0, maxTokensPerRecord].
func sanitizeDetail(detail UsageDetail) (UsageDetail, bool, bool) {
	clamped, invalid := false, false
	for _, field := range []*int64{&detail.InputTokens, &detail.OutputTokens, &detail.ReasoningTokens, &detail.CacheReadTokens, &detail.CacheCreationTokens} {
		switch {
		case *field < 0:
			*field = 0
			clamped = true
		case *field > maxTokensPerRecord:
			invalid = true
		}
	}
	for _, field := range []*int64{&detail.TotalTokens, &detail.CachedTokens} {
		if *field < 0 || *field > maxTokensPerRecord {
			*field = 0
			clamped = true
		}
	}
	return detail, clamped, invalid
}

// tokenSemantics says how a provider reports cache and reasoning tokens. It
// mirrors CPA's tokenAccountingSemanticsFor so bills agree with its own
// accounting.
type tokenSemantics uint8

const (
	// subset: cache reads are inside InputTokens, reasoning inside OutputTokens.
	semanticsSubset tokenSemantics = iota
	// independent: cache reads and writes are reported outside InputTokens.
	semanticsIndependent
	// separateReasoning: cache reads inside InputTokens, reasoning outside OutputTokens.
	semanticsSeparateReasoning
)

func tokenSemanticsFor(provider, executorType string) tokenSemantics {
	provider = strings.ToLower(strings.TrimSpace(provider))
	executorType = strings.ToLower(strings.TrimSpace(executorType))
	value := strings.TrimSpace(provider + " " + executorType)
	if executorType == "openaicompatexecutor" || provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") {
		return semanticsSubset
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return semanticsIndependent
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return semanticsSeparateReasoning
		}
	}
	return semanticsSubset
}

// billTokens splits a sanitized record into the billed token classes and
// prices them. Cache reads come only from CacheReadTokens: CPA sets
// CachedTokens to the creation count for Claude when reads are zero.
func billTokens(price ModelPrice, semantics tokenSemantics, detail UsageDetail) Bucket {
	hasWritePrice := price.CacheWritePricePerMillion != nil
	cacheRead, output := detail.CacheReadTokens, detail.OutputTokens
	var input, cacheWrite int64
	switch semantics {
	case semanticsIndependent:
		input = detail.InputTokens
		if hasWritePrice {
			cacheWrite = detail.CacheCreationTokens
		} else {
			input += detail.CacheCreationTokens
		}
	default:
		cacheRead = min(cacheRead, detail.InputTokens)
		input = detail.InputTokens - cacheRead
		if hasWritePrice {
			cacheWrite = min(detail.CacheCreationTokens, input)
			input -= cacheWrite
		}
		if semantics == semanticsSeparateReasoning {
			output += detail.ReasoningTokens
		} else if detail.ReasoningTokens > 0 && detail.TotalTokens-detail.InputTokens-detail.OutputTokens == detail.ReasoningTokens {
			// The upstream total shows reasoning was reported outside output.
			output += detail.ReasoningTokens
		}
	}
	inputPrice := price.InputPricePerMillion
	readPrice := price.CacheReadPricePerMillion
	if readPrice == 0 {
		readPrice = inputPrice
	}
	writePrice := inputPrice
	if hasWritePrice {
		writePrice = *price.CacheWritePricePerMillion
	}
	multiplier := price.BillingMultiplier
	// Each class is rounded on its own and the charge is their sum, so the
	// cache amounts shown separately never exceed the charge they are part of.
	inputCost, outputCost := tokenCharge(float64(input)*inputPrice, multiplier), tokenCharge(float64(output)*price.OutputPricePerMillion, multiplier)
	readCost, writeCost := tokenCharge(float64(cacheRead)*readPrice, multiplier), tokenCharge(float64(cacheWrite)*writePrice, multiplier)
	return Bucket{
		Calls: 1, InputTokens: input, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite, OutputTokens: output,
		ChargedNano: inputCost + readCost + writeCost + outputCost, CacheReadCostNano: readCost, CacheWriteCostNano: writeCost,
	}
}

const (
	mediaTraceCapacity = 4096
	mediaTraceTTL      = time.Hour
)

// mediaTraceSet remembers the trace ids of admitted media generation
// requests until their usage record arrives. It only correlates records
// with requests; losing an entry makes one call free, never double-billed.
type mediaTraceSet struct {
	mu    sync.Mutex
	now   func() time.Time
	order []mediaTrace // oldest first; entries taken early stay until trimmed
	index map[string]time.Time
}

type mediaTrace struct {
	id string
	at time.Time
}

func newMediaTraceSet(now func() time.Time) *mediaTraceSet {
	return &mediaTraceSet{now: now, index: make(map[string]time.Time)}
}

func (m *mediaTraceSet) setClock(now func() time.Time) {
	m.mu.Lock()
	m.now = now
	m.mu.Unlock()
}

func (m *mediaTraceSet) remember(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.trimLocked(now)
	if _, exists := m.index[id]; exists {
		return
	}
	for len(m.index) >= mediaTraceCapacity && len(m.order) > 0 {
		oldest := m.order[0]
		m.order = m.order[1:]
		if at, live := m.index[oldest.id]; live && at.Equal(oldest.at) {
			delete(m.index, oldest.id)
		}
	}
	m.index[id] = now
	m.order = append(m.order, mediaTrace{id: id, at: now})
}

// take consumes the trace id and reports whether it was known and fresh.
func (m *mediaTraceSet) take(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trimLocked(m.now())
	if _, exists := m.index[id]; !exists {
		return false
	}
	delete(m.index, id)
	return true
}

func (m *mediaTraceSet) trimLocked(now time.Time) {
	cutoff := now.Add(-mediaTraceTTL)
	first := 0
	for first < len(m.order) && m.order[first].at.Before(cutoff) {
		expired := m.order[first]
		if at, live := m.index[expired.id]; live && at.Equal(expired.at) {
			delete(m.index, expired.id)
		}
		first++
	}
	m.order = m.order[first:]
}
