package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	maxPricePerMillion   = 10000
	maxPerCallUSD        = 10000
	maxBillingMultiplier = 50
	// Per-record token counts above this are rejected at the billing
	// boundary; together with the price caps it keeps charges inside int64.
	maxTokensPerRecord = 1 << 31
)

// ModelPrice prices one upstream model id. Provider is an optional
// qualifier; an empty provider matches any provider. Zero prices are legal
// and bill $0, so a freshly imported row is usable before it is priced.
type ModelPrice struct {
	Name                      string   `json:"name"`
	Provider                  string   `json:"provider"`
	BillingMode               string   `json:"billing_mode"`
	BillingMultiplier         float64  `json:"billing_multiplier"`
	InputPricePerMillion      float64  `json:"input_price_per_million"`
	OutputPricePerMillion     float64  `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64  `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion *float64 `json:"cache_write_price_per_million,omitempty"`
	PerCallUSD                float64  `json:"per_call_usd"`
}

// PriceRow is a ModelPrice as the management API lists it.
type PriceRow struct {
	ModelPrice
	Priced bool `json:"priced"`
}

// Priced reports whether the row charges anything. Unpriced rows still count
// tokens and calls, and each call is marked unpriced.
func (p ModelPrice) Priced() bool {
	if p.BillingMode == "per_call" {
		return p.PerCallUSD > 0
	}
	return p.InputPricePerMillion > 0 || p.OutputPricePerMillion > 0 || p.CacheReadPricePerMillion > 0 ||
		(p.CacheWritePricePerMillion != nil && *p.CacheWritePricePerMillion > 0)
}

func (p ModelPrice) clone() ModelPrice {
	p.CacheWritePricePerMillion = cloneFloat64(p.CacheWritePricePerMillion)
	return p
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// normalizeProvider makes CPA's "openai-compatible-<name>" provider keys
// comparable with the bare name an operator writes in a price row.
func normalizeProvider(provider string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible-")
}

func priceKey(name, provider string) string {
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" + normalizeProvider(provider)
}

// normalizeModelPrices validates and canonicalizes rows in place and returns
// the lookup table. The same (name, provider) pair may appear only once.
func normalizeModelPrices(rows []ModelPrice) (*priceTable, error) {
	table := &priceTable{index: make(map[string]int, len(rows))}
	for i := range rows {
		row := &rows[i]
		row.Name = strings.TrimSpace(row.Name)
		if row.Name == "" {
			return nil, fmt.Errorf("model entry %d: name is required", i)
		}
		row.Provider = normalizeProvider(row.Provider)
		key := priceKey(row.Name, row.Provider)
		if _, duplicate := table.index[key]; duplicate {
			return nil, fmt.Errorf("duplicate price row for model %q provider %q", row.Name, row.Provider)
		}
		if row.BillingMultiplier == 0 {
			row.BillingMultiplier = 1
		}
		if math.IsNaN(row.BillingMultiplier) || math.IsInf(row.BillingMultiplier, 0) || row.BillingMultiplier < 1 || row.BillingMultiplier > maxBillingMultiplier {
			return nil, fmt.Errorf("model %q billing_multiplier must be between 1 and %d", row.Name, maxBillingMultiplier)
		}
		switch strings.ToLower(strings.TrimSpace(row.BillingMode)) {
		case "", "tokens":
			row.BillingMode = "tokens"
		case "per_call":
			row.BillingMode = "per_call"
		default:
			return nil, fmt.Errorf("model %q billing_mode %q must be \"tokens\" or \"per_call\"", row.Name, row.BillingMode)
		}
		for _, price := range []float64{row.InputPricePerMillion, row.OutputPricePerMillion, row.CacheReadPricePerMillion} {
			if !validPrice(price, maxPricePerMillion) {
				return nil, fmt.Errorf("model %q token prices must be between 0 and %d USD per million", row.Name, maxPricePerMillion)
			}
		}
		if row.CacheWritePricePerMillion != nil && !validPrice(*row.CacheWritePricePerMillion, maxPricePerMillion) {
			return nil, fmt.Errorf("model %q cache_write_price_per_million must be between 0 and %d", row.Name, maxPricePerMillion)
		}
		if !validPrice(row.PerCallUSD, maxPerCallUSD) {
			return nil, fmt.Errorf("model %q per_call_usd must be between 0 and %d", row.Name, maxPerCallUSD)
		}
		table.index[key] = i
	}
	table.rows = rows
	return table, nil
}

func validPrice(price float64, max float64) bool {
	return !math.IsNaN(price) && !math.IsInf(price, 0) && price >= 0 && price <= max
}

// priceTable is the published, immutable lookup view of the price rows.
type priceTable struct {
	rows  []ModelPrice
	index map[string]int
}

// lookup prefers the provider-qualified row and falls back to the
// unqualified one. Missing rows mean the model is unpriced.
func (t *priceTable) lookup(provider, model string) (ModelPrice, bool) {
	if t == nil {
		return ModelPrice{}, false
	}
	if provider = normalizeProvider(provider); provider != "" {
		if i, ok := t.index[priceKey(model, provider)]; ok {
			return t.rows[i].clone(), true
		}
	}
	if i, ok := t.index[priceKey(model, "")]; ok {
		return t.rows[i].clone(), true
	}
	return ModelPrice{}, false
}

// row returns exactly the row stored for (name, provider), without the
// provider fallback lookup applies.
func (t *priceTable) row(name, provider string) (ModelPrice, bool) {
	if t == nil {
		return ModelPrice{}, false
	}
	if i, ok := t.index[priceKey(name, provider)]; ok {
		return t.rows[i].clone(), true
	}
	return ModelPrice{}, false
}

// hasName reports whether any row prices this model id under any provider.
func (t *priceTable) hasName(model string) bool {
	_, ok := t.rowByName(model)
	return ok
}

// rowByName describes a model id for reports whose buckets carry no
// provider: the unqualified row when there is one, otherwise the qualified
// row with the lowest provider name.
func (t *priceTable) rowByName(model string) (ModelPrice, bool) {
	if t == nil {
		return ModelPrice{}, false
	}
	if i, ok := t.index[priceKey(model, "")]; ok {
		return t.rows[i].clone(), true
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	best := -1
	for i := range t.rows {
		if strings.ToLower(t.rows[i].Name) == lower && (best < 0 || t.rows[i].Provider < t.rows[best].Provider) {
			best = i
		}
	}
	if best < 0 {
		return ModelPrice{}, false
	}
	return t.rows[best].clone(), true
}

func (t *priceTable) snapshot() []ModelPrice {
	if t == nil {
		return []ModelPrice{}
	}
	rows := make([]ModelPrice, 0, len(t.rows))
	for i := range t.rows {
		rows = append(rows, t.rows[i].clone())
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if left != right {
			return left < right
		}
		return rows[i].Provider < rows[j].Provider
	})
	return rows
}

func pricePointersEqual(left, right *float64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
