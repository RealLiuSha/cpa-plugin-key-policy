package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	StateFile     string            `yaml:"state_file" json:"state_file"`
	UsageTimezone string            `yaml:"usage_timezone,omitempty" json:"usage_timezone,omitempty"`
	Keys          []KeyConfig       `yaml:"keys" json:"keys"`
	Models        []ModelDefinition `yaml:"models" json:"models"`
	ClassifyRules []ClassifyRule    `yaml:"classify_rules,omitempty" json:"classify_rules,omitempty"`
	usageLocation *time.Location
}

type KeyConfig struct {
	ID                  string        `yaml:"id" json:"id"`
	Name                string        `yaml:"name" json:"name"`
	Enabled             bool          `yaml:"enabled" json:"enabled"`
	KeyHash             string        `yaml:"key_hash" json:"key_hash"`
	KeyPreview          string        `yaml:"key_preview" json:"key_preview"`
	RPM                 int           `yaml:"rpm" json:"rpm"`
	Models              []KeyModelRef `yaml:"models" json:"models"`
	AllowModelsEndpoint bool          `yaml:"allow_models_endpoint,omitempty" json:"allow_models_endpoint,omitempty"`
	DailyLimitUSD       float64       `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      float64       `yaml:"weekly_limit_usd,omitempty" json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD     float64       `yaml:"monthly_limit_usd,omitempty" json:"monthly_limit_usd,omitempty"`
	LimitsChangedAt     time.Time     `yaml:"limits_changed_at,omitempty" json:"limits_changed_at,omitempty"`
	CreatedAt           time.Time     `yaml:"created_at,omitempty" json:"created_at,omitempty"`
	UpdatedAt           time.Time     `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

type ClassifyRule struct {
	Name     string `yaml:"name" json:"name"`
	Field    string `yaml:"field" json:"field"`
	Pattern  string `yaml:"pattern" json:"pattern"`
	Group    string `yaml:"group" json:"group"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	compiled *regexp.Regexp
}

func (r *ClassifyRule) Compiled() *regexp.Regexp {
	return r.compiled
}

type UsageBucket struct {
	TotalUSD         float64 `json:"total_usd,omitempty"`
	CallCount        int64   `json:"call_count,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheCostUSD     float64 `json:"cache_cost_usd,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CacheWriteUSD    float64 `json:"cache_write_usd,omitempty"`
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
}

type UsageState struct {
	Days    map[string]UsageBucket            `json:"days"`
	ByModel map[string]map[string]UsageBucket `json:"by_model,omitempty"`
}

type UsageWindow struct {
	TotalUSD         float64   `json:"total_usd"`
	WindowStart      time.Time `json:"window_start,omitempty"`
	CacheReadTokens  int64     `json:"cache_read_tokens,omitempty"`
	CacheCostUSD     float64   `json:"cache_cost_usd,omitempty"`
	CacheWriteTokens int64     `json:"cache_write_tokens,omitempty"`
	CacheWriteUSD    float64   `json:"cache_write_usd,omitempty"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	CallCount        int64     `json:"call_count,omitempty"`
}

type State struct {
	Version       int               `json:"version"`
	DatasetID     string            `json:"dataset_id"`
	Keys          []KeyConfig       `json:"keys"`
	Models        []ModelDefinition `json:"models"`
	ClassifyRules []ClassifyRule    `json:"classify_rules,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		StateFile:     "cpa-key-policy-state.json",
		UsageTimezone: "Asia/Shanghai",
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return Config{}, err
	}
	if err := normalizeConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ParseConfig strictly decodes the YAML shape without validating managed
// domain data. Store.Configure selects either first-boot seeds or persisted
// state before validating that effective domain.
func ParseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("configuration must contain one YAML document")
		}
		return Config{}, err
	}
	if strings.TrimSpace(cfg.StateFile) == "" {
		cfg.StateFile = DefaultConfig().StateFile
	}
	return cfg, nil
}

func normalizeConfig(cfg *Config) error {
	zone := strings.TrimSpace(cfg.UsageTimezone)
	if zone == "" {
		zone = DefaultConfig().UsageTimezone
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		log.Printf("cpa-key-policy: invalid usage_timezone %q; falling back to UTC: %v", zone, err)
		zone = "UTC"
		location = time.UTC
	}
	cfg.UsageTimezone = zone
	cfg.usageLocation = location

	modelIndex, err := normalizeModelDefinitions(cfg.Models)
	if err != nil {
		return err
	}

	keySeen := make(map[string]struct{}, len(cfg.Keys))
	for i := range cfg.Keys {
		key := &cfg.Keys[i]
		key.ID = strings.TrimSpace(key.ID)
		key.Name = strings.TrimSpace(key.Name)
		key.KeyHash = strings.TrimSpace(key.KeyHash)
		key.KeyPreview = strings.TrimSpace(key.KeyPreview)
		if key.ID == "" {
			return errors.New("key id is required")
		}
		if _, exists := keySeen[key.ID]; exists {
			return fmt.Errorf("duplicate key id %q", key.ID)
		}
		keySeen[key.ID] = struct{}{}
		if key.Name == "" {
			key.Name = key.ID
		}
		if key.RPM < 0 {
			return fmt.Errorf("key %q rpm cannot be negative", key.ID)
		}
		if key.DailyLimitUSD < 0 || key.WeeklyLimitUSD < 0 || key.MonthlyLimitUSD < 0 {
			return fmt.Errorf("key %q limits cannot be negative", key.ID)
		}
		refSeen := make(map[string]struct{}, len(key.Models))
		for j := range key.Models {
			ref := &key.Models[j]
			ref.Name = strings.TrimSpace(ref.Name)
			if ref.Name == "" {
				return fmt.Errorf("key %q model ref %d: name is required", key.ID, j)
			}
			canonical, exists := modelIndex[strings.ToLower(ref.Name)]
			if !exists {
				return fmt.Errorf("key %q references unknown model %q", key.ID, ref.Name)
			}
			if _, duplicate := refSeen[strings.ToLower(ref.Name)]; duplicate {
				return fmt.Errorf("key %q has duplicate model reference %q", key.ID, ref.Name)
			}
			refSeen[strings.ToLower(ref.Name)] = struct{}{}
			ref.Name = canonical.Name
			if ref.DailyLimitUSD < 0 {
				return fmt.Errorf("key %q model %q daily_limit_usd cannot be negative", key.ID, ref.Name)
			}
		}
	}

	ruleSeen := make(map[string]struct{}, len(cfg.ClassifyRules))
	for i := range cfg.ClassifyRules {
		rule := &cfg.ClassifyRules[i]
		rule.Name = strings.TrimSpace(rule.Name)
		rule.Field = strings.TrimSpace(rule.Field)
		rule.Pattern = strings.TrimSpace(rule.Pattern)
		rule.Group = strings.TrimSpace(rule.Group)
		if rule.Name == "" || rule.Field == "" || rule.Pattern == "" || rule.Group == "" {
			return fmt.Errorf("classify rule %d requires name, field, pattern, and group", i)
		}
		if _, exists := ruleSeen[strings.ToLower(rule.Name)]; exists {
			return fmt.Errorf("duplicate classify rule name %q", rule.Name)
		}
		ruleSeen[strings.ToLower(rule.Name)] = struct{}{}
		compiled, compileErr := regexp.Compile(rule.Pattern)
		if compileErr != nil {
			return fmt.Errorf("classify rule %q: invalid regex %q: %w", rule.Name, rule.Pattern, compileErr)
		}
		rule.compiled = compiled
	}
	return nil
}
