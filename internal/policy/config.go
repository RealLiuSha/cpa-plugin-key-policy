package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled       bool
	StateFile     string
	UsageTimezone string
	Keys          []KeyConfig
	Models        []ModelDefinition
	usageLocation *time.Location
}

// configDocument is the YAML accepted at plugin registration. Model seeds and
// classify_rules still accept the settings removed in data format 6, so a CPA
// config.yaml written for older releases keeps registering.
type configDocument struct {
	Enabled       bool          `yaml:"enabled"`
	StateFile     string        `yaml:"state_file"`
	UsageTimezone string        `yaml:"usage_timezone,omitempty"`
	Keys          []KeyConfig   `yaml:"keys"`
	Models        []compatModel `yaml:"models"`
	ClassifyRules []compatRule  `yaml:"classify_rules,omitempty"`
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
	Cycles  *UsageCycles                      `json:"cycles,omitempty"`
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
	Version   int
	DatasetID string
	Keys      []KeyConfig
	Models    []ModelDefinition
	UpdatedAt time.Time
	// RemovedSettings describes what reading a pre-format-6 file dropped.
	RemovedSettings []string
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
	document := configDocument{Enabled: cfg.Enabled, StateFile: cfg.StateFile, UsageTimezone: cfg.UsageTimezone}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("configuration must contain one YAML document")
		}
		return Config{}, err
	}
	models, _, err := projectCompatModels(document.Models, document.ClassifyRules)
	if err != nil {
		return Config{}, err
	}
	if len(document.ClassifyRules) > 0 || usesRemovedModelSettings(document.Models) {
		// CPA registers and reconfigures a plugin several times while starting.
		removedSettingsWarning.Do(func() {
			log.Printf("cpa-key-policy: config.yaml still sets classify_rules or model targets/dispatch/free, which this release no longer supports; seeds keep only the first target and the rest is ignored")
		})
	}
	cfg.Enabled, cfg.StateFile, cfg.UsageTimezone = document.Enabled, document.StateFile, document.UsageTimezone
	cfg.Keys, cfg.Models = document.Keys, models
	if strings.TrimSpace(cfg.StateFile) == "" {
		cfg.StateFile = DefaultConfig().StateFile
	}
	return cfg, nil
}

var removedSettingsWarning sync.Once

func usesRemovedModelSettings(models []compatModel) bool {
	for _, model := range models {
		if model.usesRemovedSettings() {
			return true
		}
	}
	return false
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

	return nil
}
