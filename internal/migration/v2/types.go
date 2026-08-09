package v2

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"cpa-key-policy/internal/policy"
)

type State struct {
	Version       int                   `json:"version"`
	Keys          []KeyConfig           `json:"keys"`
	Aliases       []AliasMapping        `json:"aliases"`
	ClassifyRules []policy.ClassifyRule `json:"classify_rules,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

type KeyConfig struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Enabled             bool          `json:"enabled"`
	KeyHash             string        `json:"key_hash"`
	KeyPreview          string        `json:"key_preview"`
	RPM                 int           `json:"rpm"`
	Models              []ModelRule   `json:"models,omitempty"`
	Aliases             []KeyAliasRef `json:"aliases,omitempty"`
	AllowModelsEndpoint bool          `json:"allow_models_endpoint,omitempty"`
	DailyLimitUSD       float64       `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      float64       `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD     float64       `json:"monthly_limit_usd,omitempty"`
	LimitsChangedAt     time.Time     `json:"limits_changed_at,omitempty"`
	CreatedAt           time.Time     `json:"created_at,omitempty"`
	UpdatedAt           time.Time     `json:"updated_at,omitempty"`
}

type ModelRule struct {
	Alias                    string  `json:"alias"`
	Provider                 string  `json:"provider"`
	TargetModel              string  `json:"target_model"`
	Group                    string  `json:"group,omitempty"`
	InputPricePerMillion     float64 `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    float64 `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion float64 `json:"cache_read_price_per_million,omitempty"`
	BillingMode              string  `json:"billing_mode,omitempty"`
	PerCallUSD               float64 `json:"per_call_usd,omitempty"`
	AliasDailyLimitUSD       float64 `json:"alias_daily_limit_usd,omitempty"`
}

type AliasMapping struct {
	Alias                    string        `json:"alias"`
	Targets                  []AliasTarget `json:"targets"`
	Dispatch                 string        `json:"dispatch,omitempty"`
	BillingMode              string        `json:"billing_mode,omitempty"`
	InputPricePerMillion     float64       `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    float64       `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion float64       `json:"cache_read_price_per_million,omitempty"`
	PerCallUSD               float64       `json:"per_call_usd,omitempty"`
}

type AliasTarget struct {
	Provider    string `json:"provider"`
	TargetModel string `json:"target_model"`
	Group       string `json:"group,omitempty"`
}

type KeyAliasRef struct {
	Alias                    string   `json:"alias"`
	DailyLimitUSD            float64  `json:"daily_limit_usd,omitempty"`
	InputPricePerMillion     *float64 `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    *float64 `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion *float64 `json:"cache_read_price_per_million,omitempty"`
	PerCallUSD               *float64 `json:"per_call_usd,omitempty"`
}

type UsageFile struct {
	Version   int                    `json:"version"`
	Usage     map[string]*UsageState `json:"usage"`
	UpdatedAt time.Time              `json:"updated_at,omitempty"`
}

type UsageState struct {
	Days    map[string]policy.UsageBucket            `json:"days"`
	ByAlias map[string]map[string]policy.UsageBucket `json:"by_alias,omitempty"`
}

func decodeState(raw []byte) (State, error) {
	var state State
	err := decodeLegacyJSON(raw, &state)
	return state, err
}

func decodeUsage(raw []byte) (UsageFile, error) {
	var usage UsageFile
	err := decodeLegacyJSON(raw, &usage)
	return usage, err
}

func decodeLegacyJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
