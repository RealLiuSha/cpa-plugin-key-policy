package policy

import (
	"fmt"
	"math"
	"strings"
)

// ModelDefinition is one public model: the name clients send, the CPA
// capability it routes to, and the prices charged for it. All-zero prices are
// valid and bill $0, so a freshly imported model is usable before pricing.
type ModelDefinition struct {
	Name                      string   `json:"name"`
	Provider                  string   `json:"provider"`
	TargetModel               string   `json:"target_model"`
	BillingMode               string   `json:"billing_mode,omitempty"`
	BillingMultiplier         float64  `json:"billing_multiplier"`
	InputPricePerMillion      float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion     float64  `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion  float64  `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion *float64 `json:"cache_write_price_per_million,omitempty"`
	PerCallUSD                float64  `json:"per_call_usd,omitempty"`
}

type KeyModelRef struct {
	Name          string  `yaml:"name" json:"name"`
	DailyLimitUSD float64 `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
}

func normalizeModelDefinitions(models []ModelDefinition) (map[string]*ModelDefinition, error) {
	index := make(map[string]*ModelDefinition, len(models))
	for i := range models {
		model := &models[i]
		model.Name = strings.TrimSpace(model.Name)
		if model.Name == "" {
			return nil, fmt.Errorf("model entry %d: name is required", i)
		}
		lowerName := strings.ToLower(model.Name)
		if _, duplicate := index[lowerName]; duplicate {
			return nil, fmt.Errorf("duplicate model name %q", model.Name)
		}
		model.Provider = strings.ToLower(strings.TrimSpace(model.Provider))
		model.TargetModel = strings.TrimSpace(model.TargetModel)
		if model.Provider == "" || model.TargetModel == "" {
			return nil, fmt.Errorf("model %q: provider and target_model are required", model.Name)
		}
		if model.BillingMultiplier == 0 {
			model.BillingMultiplier = 1
		}
		if model.BillingMultiplier < 1 || math.IsNaN(model.BillingMultiplier) || math.IsInf(model.BillingMultiplier, 0) {
			return nil, fmt.Errorf("model %q billing_multiplier must be finite and at least 1", model.Name)
		}
		switch strings.ToLower(strings.TrimSpace(model.BillingMode)) {
		case "", "tokens":
			model.BillingMode = "tokens"
		case "per_call":
			model.BillingMode = "per_call"
		default:
			return nil, fmt.Errorf("model %q billing_mode %q must be \"tokens\" or \"per_call\"", model.Name, model.BillingMode)
		}
		if model.InputPricePerMillion < 0 || model.OutputPricePerMillion < 0 || model.CacheReadPricePerMillion < 0 || optionalPriceNegative(model.CacheWritePricePerMillion) || model.PerCallUSD < 0 {
			return nil, fmt.Errorf("model %q prices cannot be negative", model.Name)
		}
		index[lowerName] = model
	}
	return index, nil
}

func optionalPriceValue(price *float64) float64 {
	if price == nil {
		return 0
	}
	return *price
}

func optionalPriceNegative(price *float64) bool {
	return price != nil && *price < 0
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneModel(model ModelDefinition) ModelDefinition {
	model.CacheWritePricePerMillion = cloneFloat64(model.CacheWritePricePerMillion)
	return model
}
