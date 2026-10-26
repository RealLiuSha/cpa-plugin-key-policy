package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"cpa-key-policy/internal/policy"

	"gopkg.in/yaml.v3"
)

// pluginSettings are the keys of the plugin's YAML block that belong to the
// plugin rather than to the key store.
type pluginSettings struct {
	// SearchEnabled declares model_router and answers alpha search routing.
	SearchEnabled bool
	// CPAConfigPath is CPA's config.yaml, read for the upstream keys of
	// detection tests and adapter rule tries and edited by native search's
	// automatic exclusion; relative paths resolve against CPA's working
	// directory.
	CPAConfigPath string
}

const defaultCPAConfigPath = "config.yaml"

// policyConfigKeys are the top-level keys the key store decodes.
var policyConfigKeys = map[string]bool{"enabled": true, "state_file": true, "usage_timezone": true, "keys": true, "models": true}

// ParseConfig reads the plugin section of CPA's config.yaml strictly: host
// catalog fields are dropped and any other unknown key is an error. The
// migration checker uses it to vet a configuration before an upgrade
// switches to this release, so a typo is caught before CPA restarts.
func ParseConfig(raw []byte) (policy.Config, error) {
	cfg, _, _, err := parseConfigDocument(raw, true)
	return cfg, err
}

// parseConfigDocument splits the plugin block into the key store's
// configuration and the plugin's own settings. Lenient parsing (strict
// false) is what registration uses: an unknown top-level key becomes a
// warning instead of a failed registration, which would drop every
// capability of the plugin until the next config apply.
func parseConfigDocument(raw []byte, strict bool) (policy.Config, pluginSettings, []string, error) {
	settings := pluginSettings{CPAConfigPath: defaultCPAConfigPath}
	if len(bytes.TrimSpace(raw)) == 0 {
		cfg, err := policy.ParseConfig(raw)
		return cfg, settings, nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return policy.Config{}, settings, nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return policy.Config{}, settings, nil, errors.New("configuration must contain one YAML document")
		}
		return policy.Config{}, settings, nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		cfg, err := policy.ParseConfig(raw)
		return cfg, settings, nil, err
	}

	var warnings []string
	mapping := document.Content[0]
	kept := make([]*yaml.Node, 0, len(mapping.Content))
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		name := key.Value
		switch {
		case key.Kind != yaml.ScalarNode:
			return policy.Config{}, settings, nil, errors.New("configuration keys must be strings")
		case name == "store" || name == "priority":
			// CPA's own catalog fields, injected into every plugin block.
		case name == "search_enabled":
			if err := value.Decode(&settings.SearchEnabled); err != nil {
				return policy.Config{}, settings, nil, fmt.Errorf("search_enabled: %w", err)
			}
		case name == "cpa_config_path":
			var path string
			if err := value.Decode(&path); err != nil {
				return policy.Config{}, settings, nil, fmt.Errorf("cpa_config_path: %w", err)
			}
			if path = strings.TrimSpace(path); path != "" {
				settings.CPAConfigPath = path
			}
		case policyConfigKeys[name] || strict:
			// In strict mode an unknown key reaches the key store's decoder,
			// whose error names every unknown field at once.
			kept = append(kept, key, value)
		default:
			warnings = append(warnings, fmt.Sprintf("unknown configuration key %q ignored", name))
		}
	}
	sort.Strings(warnings)
	mapping.Content = kept

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return policy.Config{}, settings, nil, err
	}
	if err := encoder.Close(); err != nil {
		return policy.Config{}, settings, nil, err
	}
	cfg, err := policy.ParseConfig(output.Bytes())
	return cfg, settings, warnings, err
}
