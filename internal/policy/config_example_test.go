package policy

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigExampleContainsValidCurrentPluginConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Plugins struct {
			Configs map[string]map[string]any `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	section, exists := document.Plugins.Configs["cpa-key-policy"]
	if !exists {
		t.Fatal("cpa-key-policy example config is missing")
	}
	delete(section, "priority") // host-owned, stripped at the plugin boundary
	pluginYAML, err := yaml.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	config, err := DecodeConfig(pluginYAML)
	if err != nil {
		t.Fatalf("invalid current example: %v", err)
	}
	if len(config.Models) != 2 || config.Models[1].BillingMode != "per_call" || len(config.Keys) != 1 || len(config.Keys[0].Models) != 2 {
		t.Fatalf("example does not cover token and per-call models with key references: %+v", config)
	}
}
