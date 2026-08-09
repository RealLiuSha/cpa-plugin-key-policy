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
			Enabled bool              `yaml:"enabled"`
			Dir     string            `yaml:"dir"`
			Configs map[string]Config `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	config, exists := document.Plugins.Configs["cpa-key-policy"]
	if !exists {
		t.Fatal("cpa-key-policy example config is missing")
	}
	if err := normalizeConfig(&config); err != nil {
		t.Fatalf("invalid current example: %v", err)
	}
	if len(config.Models) != 2 || len(config.Models[0].Targets) != 2 || len(config.Keys) != 1 || len(config.Keys[0].Models) != 2 {
		t.Fatalf("example does not cover multi-target models and key references: %+v", config)
	}
	if !config.Models[1].Free {
		t.Fatal("example must include an explicit free model")
	}
}
