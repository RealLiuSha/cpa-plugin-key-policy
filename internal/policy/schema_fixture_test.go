package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"cpa-key-policy/internal/policy/persist"
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
	if state.Models[0].CacheWritePricePerMillion != nil {
		t.Fatalf("v3 missing cache-write must stay unconfigured, got %v", state.Models[0].CacheWritePricePerMillion)
	}
	day := usage.Usage["team-a"].Days["2026-08-08"]
	if day.CacheWriteTokens != 0 || day.CacheWriteUSD != 0 {
		t.Fatalf("v3 usage invented cache-write history: %+v", day)
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

func TestSchemaV4Fixtures(t *testing.T) {
	directory := filepath.Join("testdata", "schema-v4")
	state, err := LoadState(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	usage, err := LoadUsage(filepath.Join(directory, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 4 || usage.Version != 4 || state.DatasetID != usage.DatasetID {
		t.Fatalf("state/usage pair = version %d/%d dataset %q/%q", state.Version, usage.Version, state.DatasetID, usage.DatasetID)
	}
	if state.Models[0].CacheWritePricePerMillion == nil || *state.Models[0].CacheWritePricePerMillion != 0.3 {
		t.Fatalf("v4 cache-write price = %v, want 0.3", state.Models[0].CacheWritePricePerMillion)
	}
	if usage.Usage["team-a"].Days["2026-08-08"].CacheWriteTokens != 50 {
		t.Fatalf("v4 cache-write tokens = %+v", usage.Usage["team-a"].Days["2026-08-08"])
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
	if management.Models[0].CacheWritePricePerMillion == nil || *management.Models[0].CacheWritePricePerMillion != 0.3 {
		t.Fatalf("management v4 cache-write = %+v", management.Models[0])
	}
	assertJSONFields(t, mustReadSchemaFixture(t, filepath.Join(directory, "state.json")), []string{"classify_rules", "dataset_id", "keys", "models", "updated_at", "version"})
	assertJSONFields(t, mustReadSchemaFixture(t, filepath.Join(directory, "usage.json")), []string{"dataset_id", "updated_at", "usage", "version"})
	assertJSONFields(t, managementRaw, []string{"key_usage", "keys", "models"})
}

func TestV3StateMigratesToV5BeforeServing(t *testing.T) {
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	usagePath := persist.UsagePath(statePath)
	if err := os.WriteFile(statePath, mustReadSchemaFixture(t, filepath.Join("testdata", "schema-v3", "state.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath, mustReadSchemaFixture(t, filepath.Join("testdata", "schema-v3", "usage.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	loadedState, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if loadedState.Version != currentStateFileVersion || loadedState.Models[0].CacheWritePricePerMillion != nil {
		t.Fatalf("configure must migrate v3 before publishing: %+v", loadedState)
	}
	model := loadedState.Models[0]
	model.CacheWritePricePerMillion = fptr(0.3)
	if err := store.UpsertModel(model); err != nil {
		t.Fatal(err)
	}
	upgraded, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != currentStateFileVersion || upgraded.Models[0].CacheWritePricePerMillion == nil || *upgraded.Models[0].CacheWritePricePerMillion != 0.3 {
		t.Fatalf("upgraded state = %+v", upgraded)
	}
	usage, err := LoadUsage(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Version != currentUsageFileVersion {
		t.Fatalf("usage version = %d, want current after migration", usage.Version)
	}
	if usage.Usage["team-a"].Days["2026-08-08"].CacheWriteTokens != 0 {
		t.Fatal("v3 history invented cache-write tokens")
	}
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
