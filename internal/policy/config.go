package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled       bool
	StateFile     string
	UsageTimezone string
	Keys          []KeyConfig
	Models        []ModelPrice
	usageLocation *time.Location
}

type KeyConfig struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Enabled         bool    `json:"enabled"`
	KeyHash         string  `json:"key_hash"`
	KeyPreview      string  `json:"key_preview"`
	RPM             int     `json:"rpm"`
	DailyLimitUSD   float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD  float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD float64 `json:"monthly_limit_usd,omitempty"`
	// QuotaMode is QuotaModeBalance for a prepaid key; empty means the
	// default period quotas, so state files without prepaid keys stay
	// readable by releases that predate the field.
	QuotaMode string    `json:"quota_mode,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Quota modes. A cycle key is limited by its daily, 7-day and 30-day
// quotas; a balance key spends a prepaid balance that only management
// adjustments add to, and has no period limits.
const (
	QuotaModeCycle   = "cycle"
	QuotaModeBalance = "balance"
)

// BalanceMode reports whether the key spends a prepaid balance.
func (k KeyConfig) BalanceMode() bool {
	return k.QuotaMode == QuotaModeBalance
}

// Mode is the key's quota mode as the management API names it.
func (k KeyConfig) Mode() string {
	if k.BalanceMode() {
		return QuotaModeBalance
	}
	return QuotaModeCycle
}

// ErrDuplicateKeyHash reports a secret that another key already uses. It
// applies to creation, update, rotation and first-boot seeds alike.
var ErrDuplicateKeyHash = errors.New("another key already uses this secret")

// configDocument is the YAML accepted at plugin registration; unknown fields
// are rejected.
type configDocument struct {
	Enabled       bool          `yaml:"enabled"`
	StateFile     string        `yaml:"state_file"`
	UsageTimezone string        `yaml:"usage_timezone,omitempty"`
	Keys          []configKey   `yaml:"keys"`
	Models        []configModel `yaml:"models"`
}

type configKey struct {
	ID              string    `yaml:"id"`
	Name            string    `yaml:"name"`
	Enabled         bool      `yaml:"enabled"`
	KeyHash         string    `yaml:"key_hash"`
	KeyPreview      string    `yaml:"key_preview"`
	RPM             int       `yaml:"rpm"`
	DailyLimitUSD   float64   `yaml:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD  float64   `yaml:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD float64   `yaml:"monthly_limit_usd,omitempty"`
	CreatedAt       time.Time `yaml:"created_at,omitempty"`
	UpdatedAt       time.Time `yaml:"updated_at,omitempty"`
}

// configModel seeds one price row keyed by the upstream model id.
type configModel struct {
	Name                      string   `yaml:"name"`
	Provider                  string   `yaml:"provider,omitempty"`
	BillingMode               string   `yaml:"billing_mode,omitempty"`
	BillingMultiplier         float64  `yaml:"billing_multiplier,omitempty"`
	InputPricePerMillion      float64  `yaml:"input_price_per_million,omitempty"`
	OutputPricePerMillion     float64  `yaml:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion  float64  `yaml:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion *float64 `yaml:"cache_write_price_per_million,omitempty"`
	PerCallUSD                float64  `yaml:"per_call_usd,omitempty"`
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
	cfg.Keys = make([]KeyConfig, 0, len(document.Keys))
	for _, key := range document.Keys {
		cfg.Keys = append(cfg.Keys, KeyConfig{
			ID: key.ID, Name: key.Name, Enabled: key.Enabled, KeyHash: key.KeyHash, KeyPreview: key.KeyPreview, RPM: key.RPM,
			DailyLimitUSD: key.DailyLimitUSD, WeeklyLimitUSD: key.WeeklyLimitUSD, MonthlyLimitUSD: key.MonthlyLimitUSD,
			CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt,
		})
	}
	cfg.Models = make([]ModelPrice, 0, len(document.Models))
	for _, model := range document.Models {
		cfg.Models = append(cfg.Models, ModelPrice{
			Name: model.Name, Provider: model.Provider, BillingMode: model.BillingMode, BillingMultiplier: model.BillingMultiplier,
			InputPricePerMillion: model.InputPricePerMillion, OutputPricePerMillion: model.OutputPricePerMillion,
			CacheReadPricePerMillion: model.CacheReadPricePerMillion, CacheWritePricePerMillion: cloneFloat64(model.CacheWritePricePerMillion),
			PerCallUSD: model.PerCallUSD,
		})
	}
	cfg.Enabled, cfg.StateFile, cfg.UsageTimezone = document.Enabled, document.StateFile, document.UsageTimezone
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
	if _, err := normalizeModelPrices(cfg.Models); err != nil {
		return err
	}
	if err := normalizeKeys(cfg.Keys); err != nil {
		return err
	}
	return uniqueSecrets(cfg.Keys)
}

// normalizeKeys trims and validates keys in place: ids unique and non-empty,
// limits and RPM nonnegative. Secret uniqueness is checked separately where
// keys enter (seeds and management writes), not when a dataset is loaded.
func normalizeKeys(keys []KeyConfig) error {
	seenID := make(map[string]struct{}, len(keys))
	for i := range keys {
		key := &keys[i]
		key.ID = strings.TrimSpace(key.ID)
		key.Name = strings.TrimSpace(key.Name)
		key.KeyHash = strings.TrimSpace(key.KeyHash)
		key.KeyPreview = strings.TrimSpace(key.KeyPreview)
		if key.ID == "" {
			return errors.New("key id is required")
		}
		if _, exists := seenID[key.ID]; exists {
			return fmt.Errorf("duplicate key id %q", key.ID)
		}
		seenID[key.ID] = struct{}{}
		if key.Name == "" {
			key.Name = key.ID
		}
		if key.RPM < 0 {
			return fmt.Errorf("key %q rpm cannot be negative", key.ID)
		}
		if key.DailyLimitUSD < 0 || key.WeeklyLimitUSD < 0 || key.MonthlyLimitUSD < 0 {
			return fmt.Errorf("key %q limits cannot be negative", key.ID)
		}
		switch mode := strings.ToLower(strings.TrimSpace(key.QuotaMode)); mode {
		case "", QuotaModeCycle:
			key.QuotaMode = ""
		case QuotaModeBalance:
			key.QuotaMode = mode
			if key.DailyLimitUSD > 0 || key.WeeklyLimitUSD > 0 || key.MonthlyLimitUSD > 0 {
				return fmt.Errorf("key %q: %w", key.ID, ErrBalanceKeyLimits)
			}
		default:
			return fmt.Errorf("key %q quota_mode %q must be %s or %s", key.ID, key.QuotaMode, QuotaModeCycle, QuotaModeBalance)
		}
	}
	return nil
}

// ErrBalanceKeyLimits rejects period limits on a prepaid key, whose only
// limit is its balance.
var ErrBalanceKeyLimits = errors.New("a balance key has no daily, 7-day or 30-day limits; set them to 0")

// uniqueSecrets rejects a secret shared by two seed keys (hash compared
// without case). Earlier releases allowed it, so loaded datasets are not
// checked; a management write checks only the key it changes.
func uniqueSecrets(keys []KeyConfig) error {
	for i := range keys {
		if err := secretUnique(keys, keys[i]); err != nil {
			return err
		}
	}
	return nil
}

func secretUnique(keys []KeyConfig, key KeyConfig) error {
	hash := strings.ToLower(strings.TrimSpace(key.KeyHash))
	if hash == "" {
		return nil
	}
	for _, other := range keys {
		if other.ID != key.ID && strings.ToLower(strings.TrimSpace(other.KeyHash)) == hash {
			return fmt.Errorf("key %q: %w (key %q)", key.ID, ErrDuplicateKeyHash, other.ID)
		}
	}
	return nil
}
