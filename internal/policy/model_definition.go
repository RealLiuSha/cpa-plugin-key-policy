package policy

import (
	"fmt"
	"strings"
)

type ModelDefinition struct {
	Name                     string        `yaml:"name" json:"name"`
	Targets                  []ModelTarget `yaml:"targets" json:"targets"`
	Dispatch                 string        `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`
	BillingMode              string        `yaml:"billing_mode,omitempty" json:"billing_mode,omitempty"`
	Free                     bool          `yaml:"free" json:"free"`
	InputPricePerMillion     float64       `yaml:"input_price_per_million,omitempty" json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    float64       `yaml:"output_price_per_million,omitempty" json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion float64       `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
	PerCallUSD               float64       `yaml:"per_call_usd,omitempty" json:"per_call_usd,omitempty"`
}

type ModelTarget struct {
	Provider    string `yaml:"provider" json:"provider"`
	TargetModel string `yaml:"target_model" json:"target_model"`
	Group       string `yaml:"group,omitempty" json:"group,omitempty"`
}

type KeyModelRef struct {
	Name          string  `yaml:"name" json:"name"`
	DailyLimitUSD float64 `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
}

type ResolvedModelRoute struct {
	PublicModel              string
	Provider                 string
	TargetModel              string
	Group                    string
	BillingMode              string
	Free                     bool
	InputPricePerMillion     float64
	OutputPricePerMillion    float64
	CacheReadPricePerMillion float64
	PerCallUSD               float64
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
		if len(model.Targets) == 0 {
			return nil, fmt.Errorf("model %q must have at least one target", model.Name)
		}
		targetSeen := make(map[string]struct{}, len(model.Targets))
		for j := range model.Targets {
			target := &model.Targets[j]
			target.Provider = strings.ToLower(strings.TrimSpace(target.Provider))
			target.TargetModel = strings.TrimSpace(target.TargetModel)
			target.Group = strings.ToLower(strings.TrimSpace(target.Group))
			if target.Provider == "" || target.TargetModel == "" {
				return nil, fmt.Errorf("model %q target %d: provider and target_model are required", model.Name, j)
			}
			targetKey := strings.ToLower(target.Provider) + "\x00" + strings.ToLower(target.TargetModel) + "\x00" + strings.ToLower(target.Group)
			if _, duplicate := targetSeen[targetKey]; duplicate {
				return nil, fmt.Errorf("model %q has duplicate target provider=%q target_model=%q group=%q", model.Name, target.Provider, target.TargetModel, target.Group)
			}
			targetSeen[targetKey] = struct{}{}
		}
		switch strings.ToLower(strings.TrimSpace(model.Dispatch)) {
		case "", "round-robin":
			model.Dispatch = "round-robin"
		case "priority":
			model.Dispatch = "priority"
		default:
			return nil, fmt.Errorf("model %q dispatch %q must be \"round-robin\" or \"priority\"", model.Name, model.Dispatch)
		}
		switch strings.ToLower(strings.TrimSpace(model.BillingMode)) {
		case "", "tokens":
			model.BillingMode = "tokens"
		case "per_call":
			model.BillingMode = "per_call"
		default:
			return nil, fmt.Errorf("model %q billing_mode %q must be \"tokens\" or \"per_call\"", model.Name, model.BillingMode)
		}
		if model.InputPricePerMillion < 0 || model.OutputPricePerMillion < 0 || model.CacheReadPricePerMillion < 0 || model.PerCallUSD < 0 {
			return nil, fmt.Errorf("model %q prices cannot be negative", model.Name)
		}
		if model.Free {
			if model.InputPricePerMillion != 0 || model.OutputPricePerMillion != 0 || model.CacheReadPricePerMillion != 0 || model.PerCallUSD != 0 {
				return nil, fmt.Errorf("model %q is free and all price fields must be zero", model.Name)
			}
		} else if model.BillingMode == "per_call" {
			if model.PerCallUSD <= 0 {
				return nil, fmt.Errorf("model %q per_call_usd must be positive unless free is true", model.Name)
			}
		} else if model.InputPricePerMillion <= 0 && model.OutputPricePerMillion <= 0 && model.CacheReadPricePerMillion <= 0 {
			return nil, fmt.Errorf("model %q must have a positive token price unless free is true", model.Name)
		}
		index[lowerName] = model
	}
	return index, nil
}

func resolveModelRoute(model ModelDefinition, target ModelTarget) ResolvedModelRoute {
	return ResolvedModelRoute{
		PublicModel:              model.Name,
		Provider:                 target.Provider,
		TargetModel:              target.TargetModel,
		Group:                    target.Group,
		BillingMode:              model.BillingMode,
		Free:                     model.Free,
		InputPricePerMillion:     model.InputPricePerMillion,
		OutputPricePerMillion:    model.OutputPricePerMillion,
		CacheReadPricePerMillion: model.CacheReadPricePerMillion,
		PerCallUSD:               model.PerCallUSD,
	}
}
