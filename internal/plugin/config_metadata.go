package plugin

import (
	"bytes"
	"errors"
	"io"

	"cpa-key-policy/internal/policy"

	"gopkg.in/yaml.v3"
)

// ParseConfig reads the plugin section of CPA's config.yaml the way
// registration does: host catalog fields are dropped, everything else is
// decoded strictly. The migration checker uses it to vet a configuration
// before an upgrade switches to this release.
func ParseConfig(raw []byte) (policy.Config, error) {
	configYAML, err := stripHostConfigMetadata(raw)
	if err != nil {
		return policy.Config{}, err
	}
	return policy.ParseConfig(configYAML)
}

// stripHostConfigMetadata keeps CPA-owned plugin catalog fields outside the
// policy configuration schema, which remains strict about unknown fields.
func stripHostConfigMetadata(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("configuration must contain one YAML document")
		}
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return raw, nil
	}

	mapping := document.Content[0]
	filtered := make([]*yaml.Node, 0, len(mapping.Content))
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Kind == yaml.ScalarNode && (key.Value == "store" || key.Value == "priority") {
			continue
		}
		filtered = append(filtered, key, value)
	}
	mapping.Content = filtered

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
