package policy

import (
	"fmt"
	"strings"
)

// Before data format 6 a model could list several upstream targets with a
// dispatch policy, pin targets to credential groups, carry an explicit free
// flag, and the state held credential classification rules. Those settings are
// gone; these shapes keep older state files and unchanged CPA config.yaml
// seeds readable, and projectCompatModels reports what each read drops.

type compatModel struct {
	BillingMultiplier         float64        `yaml:"billing_multiplier,omitempty" json:"billing_multiplier"`
	Name                      string         `yaml:"name" json:"name"`
	Provider                  string         `yaml:"provider,omitempty" json:"provider,omitempty"`
	TargetModel               string         `yaml:"target_model,omitempty" json:"target_model,omitempty"`
	Targets                   []compatTarget `yaml:"targets,omitempty" json:"targets,omitempty"`
	Dispatch                  string         `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`
	BillingMode               string         `yaml:"billing_mode,omitempty" json:"billing_mode,omitempty"`
	Free                      bool           `yaml:"free,omitempty" json:"free"`
	InputPricePerMillion      float64        `yaml:"input_price_per_million,omitempty" json:"input_price_per_million,omitempty"`
	OutputPricePerMillion     float64        `yaml:"output_price_per_million,omitempty" json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion  float64        `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion *float64       `yaml:"cache_write_price_per_million,omitempty" json:"cache_write_price_per_million,omitempty"`
	PerCallUSD                float64        `yaml:"per_call_usd,omitempty" json:"per_call_usd,omitempty"`
}

type compatTarget struct {
	Provider    string `yaml:"provider" json:"provider"`
	TargetModel string `yaml:"target_model" json:"target_model"`
	Group       string `yaml:"group,omitempty" json:"group,omitempty"`
}

type compatRule struct {
	Name    string `yaml:"name" json:"name"`
	Field   string `yaml:"field" json:"field"`
	Pattern string `yaml:"pattern" json:"pattern"`
	Group   string `yaml:"group" json:"group"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// usesRemovedSettings reports whether the input still spells out settings that
// format 6 no longer has.
func (m compatModel) usesRemovedSettings() bool {
	return len(m.Targets) > 0 || strings.TrimSpace(m.Dispatch) != "" || m.Free
}

// projectCompatModels keeps the first upstream target of each model, which is
// the primary under priority dispatch and a stable choice under round-robin.
// Free models already carry zero prices, so dropping the flag bills the same.
func projectCompatModels(models []compatModel, rules []compatRule) ([]ModelDefinition, []string, error) {
	projected := make([]ModelDefinition, 0, len(models))
	var notes []string
	for _, input := range models {
		model := ModelDefinition{
			Name: input.Name, Provider: input.Provider, TargetModel: input.TargetModel,
			BillingMode: input.BillingMode, BillingMultiplier: input.BillingMultiplier,
			InputPricePerMillion: input.InputPricePerMillion, OutputPricePerMillion: input.OutputPricePerMillion,
			CacheReadPricePerMillion: input.CacheReadPricePerMillion, CacheWritePricePerMillion: cloneFloat64(input.CacheWritePricePerMillion),
			PerCallUSD: input.PerCallUSD,
		}
		if len(input.Targets) > 0 {
			if strings.TrimSpace(input.Provider) != "" || strings.TrimSpace(input.TargetModel) != "" {
				return nil, nil, fmt.Errorf("model %q: set provider/target_model or the legacy targets list, not both", input.Name)
			}
			kept := input.Targets[0]
			model.Provider, model.TargetModel = kept.Provider, kept.TargetModel
			if len(input.Targets) > 1 {
				dropped := make([]string, 0, len(input.Targets)-1)
				for _, target := range input.Targets[1:] {
					dropped = append(dropped, targetLabel(target))
				}
				notes = append(notes, fmt.Sprintf("model %q keeps upstream %s; removed %s", input.Name, targetLabel(kept), strings.Join(dropped, ", ")))
			}
			for _, target := range input.Targets {
				if strings.TrimSpace(target.Group) != "" {
					notes = append(notes, fmt.Sprintf("model %q: removed credential group %q from %s", input.Name, strings.TrimSpace(target.Group), targetLabel(target)))
				}
			}
		}
		if input.Free {
			notes = append(notes, fmt.Sprintf("model %q: removed the free flag; its zero prices still bill $0", input.Name))
		}
		projected = append(projected, model)
	}
	if len(rules) > 0 {
		names := make([]string, 0, len(rules))
		for _, rule := range rules {
			names = append(names, rule.Name)
		}
		notes = append(notes, fmt.Sprintf("removed %d credential classification rule(s): %s", len(rules), strings.Join(names, ", ")))
	}
	return projected, notes, nil
}

func targetLabel(target compatTarget) string {
	return strings.ToLower(strings.TrimSpace(target.Provider)) + "/" + strings.TrimSpace(target.TargetModel)
}
