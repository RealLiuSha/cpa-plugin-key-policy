package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

type managementFixtureKey struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Enabled             bool          `json:"enabled"`
	KeyPreview          string        `json:"key_preview"`
	RPM                 int           `json:"rpm"`
	Models              []KeyModelRef `json:"models"`
	DailyLimitUSD       float64       `json:"daily_limit_usd"`
	WeeklyLimitUSD      float64       `json:"weekly_limit_usd"`
	MonthlyLimitUSD     float64       `json:"monthly_limit_usd"`
	AllowModelsEndpoint bool          `json:"allow_models_endpoint,omitempty"`
	Usage               UsageSummary  `json:"usage"`
	CreatedAt           string        `json:"created_at,omitempty"`
	UpdatedAt           string        `json:"updated_at,omitempty"`
}

type managementFixtureKeyUsage struct {
	KeyID           string            `json:"key_id"`
	KeyName         string            `json:"key_name"`
	DailyLimitUSD   float64           `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64           `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64           `json:"monthly_limit_usd"`
	Models          []ModelUsageEntry `json:"models"`
}

func TestSchemaV3Fixtures(t *testing.T) {
	directory := filepath.Join("testdata", "schema-v3")
	state, err := LoadState(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	usage, err := LoadUsage(filepath.Join(directory, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 3 || usage.Version != 3 || state.DatasetID != usage.DatasetID {
		t.Fatalf("state/usage pair = version %d/%d dataset %q/%q", state.Version, usage.Version, state.DatasetID, usage.DatasetID)
	}
	if len(state.Models) != 1 || len(state.Models[0].Targets) != 2 || len(state.Keys) != 1 || len(state.Keys[0].Models) != 1 {
		t.Fatalf("state fixture lost model-domain structure: %+v", state)
	}

	managementRaw := mustReadSchemaFixture(t, filepath.Join(directory, "management.json"))
	var management struct {
		Keys     []managementFixtureKey    `json:"keys"`
		Models   []ModelWithRefs           `json:"models"`
		KeyUsage managementFixtureKeyUsage `json:"key_usage"`
	}
	if err := decodeJSONStrict(managementRaw, &management); err != nil {
		t.Fatal(err)
	}
	if len(management.Keys) != 1 || len(management.Models) != 1 || len(management.KeyUsage.Models) != 1 {
		t.Fatalf("management fixture = %+v", management)
	}
	if management.Models[0].RefCount != 1 || management.KeyUsage.Models[0].Name != "Chat" {
		t.Fatalf("management model references = %+v usage=%+v", management.Models[0], management.KeyUsage.Models)
	}

	assertJSONFields(t, mustReadSchemaFixture(t, filepath.Join(directory, "state.json")), []string{"classify_rules", "dataset_id", "keys", "models", "updated_at", "version"})
	assertJSONFields(t, mustReadSchemaFixture(t, filepath.Join(directory, "usage.json")), []string{"dataset_id", "updated_at", "usage", "version"})
	assertJSONFields(t, managementRaw, []string{"key_usage", "keys", "models"})
}

func mustReadSchemaFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertJSONFields(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(document))
	for field := range document {
		got = append(got, field)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level fields = %v, want %v", got, want)
	}
}
