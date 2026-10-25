package native

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

const (
	defaultIntervalHours = 12
	maxIntervalHours     = 168
	maxListEntries       = 200
	maxNameLength        = 200
)

// ErrInvalidSettings wraps every validation error of Normalize.
var ErrInvalidSettings = errors.New("invalid native search settings")

// Settings is what the operator controls. Native search is on by default;
// RequireAll switches a model only when every channel serving it supports
// native search (or is disabled), instead of when one does.
type Settings struct {
	Enabled          bool     `json:"enabled"`
	RequireAll       bool     `json:"require_all"`
	ExcludedModels   []string `json:"excluded_models"`
	DisabledChannels []string `json:"disabled_channels"`
	IntervalHours    int      `json:"interval_hours"`
}

// DefaultSettings: on, a model switches when one channel supports native
// search, a full probe round every 12 hours.
func DefaultSettings() Settings {
	return Settings{Enabled: true, ExcludedModels: []string{}, DisabledChannels: []string{}, IntervalHours: defaultIntervalHours}
}

// Normalize trims and deduplicates the lists, lowercases the channels (CPA's
// auth_index is lowercase hex) and checks the interval (0 means the default,
// otherwise 1 to 168 hours). The result shares no slice with s.
func Normalize(s Settings) (Settings, error) {
	out := Settings{Enabled: s.Enabled, RequireAll: s.RequireAll, IntervalHours: s.IntervalHours}
	if out.IntervalHours == 0 {
		out.IntervalHours = defaultIntervalHours
	}
	if out.IntervalHours < 1 || out.IntervalHours > maxIntervalHours {
		return Settings{}, fmt.Errorf("%w: interval_hours must be between 1 and %d", ErrInvalidSettings, maxIntervalHours)
	}
	var err error
	if out.ExcludedModels, err = cleanList(s.ExcludedModels, "excluded_models"); err != nil {
		return Settings{}, err
	}
	if out.DisabledChannels, err = cleanList(s.DisabledChannels, "disabled_channels"); err != nil {
		return Settings{}, err
	}
	for i, index := range out.DisabledChannels {
		out.DisabledChannels[i] = strings.ToLower(index)
	}
	return out, nil
}

func cleanList(values []string, field string) ([]string, error) {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || slices.ContainsFunc(out, func(v string) bool { return strings.EqualFold(v, value) }) {
			continue
		}
		if len(value) > maxNameLength || strings.ContainsFunc(value, unicode.IsControl) {
			return nil, fmt.Errorf("%w: %s holds an invalid entry", ErrInvalidSettings, field)
		}
		out = append(out, value)
	}
	if len(out) > maxListEntries {
		return nil, fmt.Errorf("%w: %s holds more than %d entries", ErrInvalidSettings, field, maxListEntries)
	}
	return out, nil
}

func (s Settings) clone() Settings {
	s.ExcludedModels = slices.Clone(s.ExcludedModels)
	s.DisabledChannels = slices.Clone(s.DisabledChannels)
	return s
}
