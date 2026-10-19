package search

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"
)

const (
	modelPlaceholder = "{model}"

	defaultTargetModel          = "gpt-6.1-sol"
	defaultChannelName          = "default"
	defaultProbeIntervalSeconds = 60
	minProbeIntervalSeconds     = 10
	defaultProbeTimeoutSeconds  = 15
	// maxProbeSeconds bounds the interval, timeout and idle pause far above
	// any real setting and far below a time.Duration overflow.
	maxProbeSeconds = 30 * 24 * 60 * 60
	defaultProbeURL = "http://127.0.0.1:8317/v1/alpha/search"
	// CPA forwards the client User-Agent upstream, and Cloudflare in front of
	// some upstreams rejects generic HTTP client agents with 403.
	defaultProbeUserAgent = "codex_cli_rs/0.130.0 (alpha-search-model probe)"
)

// DefaultSettings matches alpha-search-model's defaults: target gpt-6.1-sol,
// keep gpt-*, one "{model}" channel, probing off (60 s interval, 15 s
// timeout, http://127.0.0.1:8317/v1/alpha/search, a Codex user agent).
func DefaultSettings() Settings {
	return Settings{
		TargetModel: defaultTargetModel,
		KeepModels:  []string{"gpt-*"},
		Channels:    []Channel{{Name: defaultChannelName, Model: modelPlaceholder}},
		Probe: Probe{
			IntervalSeconds: defaultProbeIntervalSeconds,
			TimeoutSeconds:  defaultProbeTimeoutSeconds,
			URL:             defaultProbeURL,
			UserAgent:       defaultProbeUserAgent,
		},
	}
}

// Normalize trims, lowercases the keep patterns, fills defaults, enforces
// the probe interval minimum (10 s) and validates the patterns, templates,
// probe URL and probe header values. Probe.Model stays empty when unset:
// probes then use TargetModel, so a stored copy never goes stale. Interval,
// timeout and idle pause are at most 30 days. Every error wraps
// ErrInvalidSettings. The result shares no slice with s.
func Normalize(s Settings) (Settings, error) {
	defaults := DefaultSettings()
	out := Settings{TargetModel: strings.TrimSpace(s.TargetModel)}
	if out.TargetModel == "" {
		out.TargetModel = defaults.TargetModel
	}

	for _, pattern := range s.KeepModels {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return Settings{}, invalid("keep_models pattern %q: %v", pattern, err)
		}
		out.KeepModels = append(out.KeepModels, pattern)
	}
	if len(out.KeepModels) == 0 {
		out.KeepModels = defaults.KeepModels
	}

	names := make(map[string]bool, len(s.Channels))
	for i, ch := range s.Channels {
		ch.Name = strings.TrimSpace(ch.Name)
		ch.Model = strings.TrimSpace(ch.Model)
		if ch.Name == "" || ch.Model == "" {
			return Settings{}, invalid("channels[%d] needs a name and a model template", i)
		}
		if names[ch.Name] {
			return Settings{}, invalid("channels[%d]: duplicate name %q", i, ch.Name)
		}
		names[ch.Name] = true
		out.Channels = append(out.Channels, ch)
	}
	if len(out.Channels) == 0 {
		out.Channels = defaults.Channels
	}

	probe, err := normalizeProbe(s.Probe, defaults.Probe)
	if err != nil {
		return Settings{}, err
	}
	out.Probe = probe
	return out, nil
}

func normalizeProbe(p, defaults Probe) (Probe, error) {
	p.APIKey = strings.TrimSpace(p.APIKey)
	if strings.ContainsFunc(p.APIKey, unicode.IsControl) {
		return Probe{}, invalid("probe.api_key contains control characters")
	}
	if p.IntervalSeconds == 0 {
		p.IntervalSeconds = defaults.IntervalSeconds
	}
	if p.IntervalSeconds < minProbeIntervalSeconds || p.IntervalSeconds > maxProbeSeconds {
		return Probe{}, invalid("probe.interval_seconds must be between %d and %d", minProbeIntervalSeconds, maxProbeSeconds)
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if p.TimeoutSeconds > maxProbeSeconds {
		return Probe{}, invalid("probe.timeout_seconds must be at most %d", maxProbeSeconds)
	}
	if p.IdleSeconds < 0 || p.IdleSeconds > maxProbeSeconds {
		return Probe{}, invalid("probe.idle_seconds must be between 0 and %d", maxProbeSeconds)
	}
	p.Model = strings.TrimSpace(p.Model)
	p.URL = strings.TrimSpace(p.URL)
	if p.URL == "" {
		p.URL = defaults.URL
	}
	if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Probe{}, invalid("probe.url must be an absolute http or https URL")
	}
	p.UserAgent = strings.TrimSpace(p.UserAgent)
	if p.UserAgent == "" {
		p.UserAgent = defaults.UserAgent
	}
	if strings.ContainsFunc(p.UserAgent, unicode.IsControl) {
		return Probe{}, invalid("probe.user_agent contains control characters")
	}
	return p, nil
}

// FromPluginConfig converts an alpha-search-model YAML block, as CPA's
// management API returns it in JSON (keys target-model, keep-models,
// channels[].name/model/force-unhealthy, probe-*), into normalized Settings.
// Host keys such as enabled, priority and store, and unknown keys, are
// ignored. Scalars keep YAML's leniency for strings (a numeric channel name
// such as 7788101 becomes "7788101"); a value of the wrong kind is an error
// wrapping ErrInvalidSettings. A block with none of those keys, such as the
// empty one CPA returns for a plugin it has no configuration for, is
// ErrNothingToImport. The result carries probe-api-key in Probe.APIKey: hand
// it to Update, never back to a client.
func FromPluginConfig(block map[string]any) (Settings, error) {
	var s Settings
	var err error
	present := false
	lookup := func(key string) any {
		value, ok := block[key]
		present = present || ok
		return value
	}
	for _, field := range []struct {
		key string
		dst *string
	}{
		{"target-model", &s.TargetModel},
		{"probe-api-key", &s.Probe.APIKey},
		{"probe-model", &s.Probe.Model},
		{"probe-url", &s.Probe.URL},
		{"probe-user-agent", &s.Probe.UserAgent},
	} {
		if *field.dst, err = configString(lookup(field.key), field.key); err != nil {
			return Settings{}, err
		}
	}
	for _, field := range []struct {
		key string
		dst *int
	}{
		{"probe-interval-seconds", &s.Probe.IntervalSeconds},
		{"probe-timeout-seconds", &s.Probe.TimeoutSeconds},
		{"probe-idle-seconds", &s.Probe.IdleSeconds},
	} {
		if *field.dst, err = configInt(lookup(field.key), field.key); err != nil {
			return Settings{}, err
		}
	}

	patterns, err := configList(lookup("keep-models"), "keep-models")
	if err != nil {
		return Settings{}, err
	}
	for i, value := range patterns {
		pattern, err := configString(value, fmt.Sprintf("keep-models[%d]", i))
		if err != nil {
			return Settings{}, err
		}
		s.KeepModels = append(s.KeepModels, pattern)
	}

	channels, err := configList(lookup("channels"), "channels")
	if err != nil {
		return Settings{}, err
	}
	for i, value := range channels {
		entry, ok := value.(map[string]any)
		if !ok {
			return Settings{}, invalid("channels[%d] must be a mapping", i)
		}
		var ch Channel
		label := fmt.Sprintf("channels[%d].", i)
		if ch.Name, err = configString(entry["name"], label+"name"); err != nil {
			return Settings{}, err
		}
		if ch.Model, err = configString(entry["model"], label+"model"); err != nil {
			return Settings{}, err
		}
		if ch.ForceUnhealthy, err = configBool(entry["force-unhealthy"], label+"force-unhealthy"); err != nil {
			return Settings{}, err
		}
		s.Channels = append(s.Channels, ch)
	}
	if !present {
		return Settings{}, ErrNothingToImport
	}
	return Normalize(s)
}

func configString(value any, key string) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	return "", invalid("%s must be a string", key)
}

func configInt(value any, key string) (int, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return v, nil
	case int64:
		if v >= math.MinInt32 && v <= math.MaxInt32 {
			return int(v), nil
		}
	case float64:
		if v == math.Trunc(v) && v >= math.MinInt32 && v <= math.MaxInt32 {
			return int(v), nil
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= math.MinInt32 && n <= math.MaxInt32 {
			return int(n), nil
		}
	}
	return 0, invalid("%s must be a whole number", key)
}

func configBool(value any, key string) (bool, error) {
	switch v := value.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	}
	return false, invalid("%s must be true or false", key)
}

func configList(value any, key string) ([]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	case []string:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, nil
	case []map[string]any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, nil
	}
	return nil, invalid("%s must be a list", key)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSettings, fmt.Sprintf(format, args...))
}

// probing reports whether channel health is tracked. With one channel, or
// without an access key for CPA, every channel counts as healthy.
func (s Settings) probing() bool {
	return len(s.Channels) > 1 && s.Probe.APIKey != ""
}

// probeModel is the model put through each channel template for probes.
func (s Settings) probeModel() string {
	if s.Probe.Model != "" {
		return s.Probe.Model
	}
	return s.TargetModel
}

func keepModel(patterns []string, model string) bool {
	lowerModel := strings.ToLower(model)
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, lowerModel); matched {
			return true
		}
	}
	return false
}
