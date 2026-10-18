package policy

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/audit"
)

func TestKeyMutationsAuditAndDuplicateSecrets(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, nil, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	alice := testKey(t, "alice", "cpa_alice")
	alice.DailyLimitUSD = 1
	if err := store.UpsertKey(alice); err != nil {
		t.Fatal(err)
	}
	bob := testKey(t, "bob", "cpa_alice")
	if err := store.UpsertKey(bob); !errors.Is(err, ErrDuplicateKeyHash) {
		t.Fatalf("duplicate secret on create = %v", err)
	}
	bob = testKey(t, "bob", "cpa_bob")
	if err := store.UpsertKey(bob); err != nil {
		t.Fatal(err)
	}
	bob = keyByID(t, store, "bob")
	bob.KeyHash = strings.ToUpper(mustHash(t, "cpa_alice"))
	if err := store.UpsertKey(bob); !errors.Is(err, ErrDuplicateKeyHash) {
		t.Fatalf("duplicate secret on update = %v", err)
	}
	alice = keyByID(t, store, "alice")
	alice.Name, alice.RPM, alice.DailyLimitUSD, alice.WeeklyLimitUSD = "Alice", 5, 2, 7
	if err := store.UpsertKey(alice); err != nil {
		t.Fatalf("updating a key with its own secret: %v", err)
	}
	if _, _, err := store.RotateKey("alice"); err != nil {
		t.Fatal(err)
	}
	if store.FindByAPIKey("cpa_alice") != nil {
		t.Fatal("rotated secret still resolves")
	}
	store.RecordUsage(tokenEvent("alice", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	if err := store.DeleteKey("alice"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteKey("alice"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("second delete = %v", err)
	}
	stats := store.Stats(1)
	if len(stats.Keys) != 1 || stats.Keys[0].ID != "alice" || !stats.Keys[0].Deleted || stats.Keys[0].ChargedUSD != 1 {
		t.Fatalf("deleted key history = %+v", stats.Keys)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if len(reloaded.Keys()) != 1 || reloaded.Stats(1).Totals.Calls != 1 {
		t.Fatalf("deleted key history lost on reload: keys=%d stats=%+v", len(reloaded.Keys()), reloaded.Stats(1))
	}
	events, err := store.AuditEvents("alice", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]audit.Event{}
	for _, event := range events {
		actions[event.Action] = event
	}
	if _, ok := actions["create_key"]; !ok {
		t.Fatalf("missing create_key: %+v", events)
	}
	update := actions["update_key"].Changes
	// Events are read back from JSON, so every number is a float64.
	if update["name"] != (audit.Change{From: "alice", To: "Alice"}) || update["rpm"] != (audit.Change{From: float64(0), To: float64(5)}) || update["daily_limit_usd"] != (audit.Change{From: float64(1), To: float64(2)}) || len(update) != 4 {
		t.Fatalf("update changes = %+v", update)
	}
	if actions["delete_key"].Changes["daily_usd"].From != float64(1) {
		t.Fatalf("delete changes = %+v", actions["delete_key"].Changes)
	}
	if _, ok := actions["rotate_key"]; !ok {
		t.Fatal("missing rotate_key")
	}
	for _, event := range events {
		if strings.Contains(strings.ToLower(eventText(event)), "sha256:") || strings.Contains(eventText(event), "cpa_") {
			t.Fatalf("audit leaked a secret: %+v", event)
		}
	}
}

func eventText(event audit.Event) string {
	var parts []string
	for field, change := range event.Changes {
		parts = append(parts, field, strings.TrimSpace(strings.Join([]string{toString(change.From), toString(change.To)}, " ")))
	}
	return strings.Join(parts, " ")
}

func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func TestPriceRowsLookupAndImports(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, nil)
	if err := store.UpsertModel(tokenPrice("gpt-5.6", "", 1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertModel(tokenPrice("GPT-5.6", "openai-compatible-Proxy", 10, 20)); err != nil {
		t.Fatal(err)
	}
	bad := tokenPrice("too-much", "", 10001, 0)
	if err := store.UpsertModel(bad); err == nil {
		t.Fatal("price above the cap accepted")
	}
	bad = tokenPrice("too-much", "", 1, 0)
	bad.BillingMultiplier = 51
	if err := store.UpsertModel(bad); err == nil {
		t.Fatal("multiplier above the cap accepted")
	}
	store.RecordUsage(tokenEvent("k", "openai-compatible-proxy", "gpt-5.6", UsageDetail{InputTokens: 1_000_000}))
	store.RecordUsage(tokenEvent("k", "codex", "gpt-5.6", UsageDetail{InputTokens: 1_000_000}))
	if summary := store.UsageSummaryFor(keyByID(t, store, "k")); !nearly(summary.DailyUSD, 11) {
		t.Fatalf("provider-qualified lookup = %+v, want 10 + 1", summary)
	}
	// Buckets are keyed by the lowercased upstream id whichever row priced the
	// call, so differently spelled rows do not split one model's usage.
	if _, models, _ := store.ModelUsageFor("k"); len(models) != 1 || models[0].Name != "gpt-5.6" || models[0].Daily.CallCount != 2 || !nearly(models[0].Daily.TotalUSD, 11) {
		t.Fatalf("model rows = %+v, want one gpt-5.6 row with both calls", models)
	}
	rows := store.PriceRows()
	if len(rows) != 2 || rows[0].Provider != "" || rows[1].Provider != "proxy" || !rows[1].Priced {
		t.Fatalf("rows = %+v", rows)
	}
	imported, err := store.ImportModels([]ModelImportItem{{Name: "gpt-6-terra", Provider: "openai"}, {Name: "gpt-5.6"}, {Name: "gpt-6-terra", Provider: "openai"}})
	if err != nil || len(imported.Created) != 1 || len(imported.Skipped) != 2 || imported.Skipped[0].Reason != "exists" || imported.Skipped[1].Reason != "duplicate" {
		t.Fatalf("import = %+v err=%v", imported, err)
	}
	result, err := store.ImportModelPrices([]PriceImportMatch{{Model: "gpt-5.6", PromptPricePer1M: ptr(3.0)}, {Model: "nope", PromptPricePer1M: ptr(1.0)}}, false)
	if err != nil || len(result.Applied) != 2 || len(result.Skipped) != 1 {
		t.Fatalf("price import = %+v err=%v", result, err)
	}
	if err := store.UpsertModel(tokenPrice("gpt-5.6", "proxy", 3, 25)); err != nil {
		t.Fatal(err)
	}
	// Every price audit event carries the row's provider unchanged, so the
	// subject of a change is the exact (name, provider) row.
	events, err := store.AuditEvents("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string][]any{}
	for _, event := range events {
		providers[event.Action] = append(providers[event.Action], event.Changes["provider"])
	}
	if len(providers["create_model"]) != 2 || providers["create_model"][0] != (audit.Change{From: "proxy", To: "proxy"}) || providers["create_model"][1] != (audit.Change{From: "", To: ""}) ||
		len(providers["update_model"]) != 1 || providers["update_model"][0] != (audit.Change{From: "proxy", To: "proxy"}) ||
		len(providers["import_create_model"]) != 1 || providers["import_create_model"][0] != (audit.Change{From: "openai", To: "openai"}) ||
		len(providers["import_update_prices"]) != 2 {
		t.Fatalf("provider in price audit events = %+v", providers)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	rows = reloaded.PriceRows()
	if len(rows) != 3 || rows[0].InputPricePerMillion != 3 || rows[1].InputPricePerMillion != 3 || rows[2].Name != "gpt-6-terra" || rows[2].Priced {
		t.Fatalf("persisted rows = %+v", rows)
	}
	if err := reloaded.DeleteModel("gpt-5.6", "proxy"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteModel("gpt-5.6", "proxy"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("second delete = %v, want ErrUnknownModel", err)
	}
	if rows = reloaded.PriceRows(); len(rows) != 2 {
		t.Fatalf("rows after delete = %+v", rows)
	}
	// The delete audit names the row as stored, like create and update do.
	events, err = reloaded.AuditEvents("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	deleted := map[string]audit.Change{}
	for _, event := range events {
		if event.Action == "delete_model" {
			deleted = event.Changes
		}
	}
	if deleted["model"] != (audit.Change{From: "GPT-5.6", To: ""}) || deleted["provider"] != (audit.Change{From: "proxy", To: ""}) {
		t.Fatalf("delete_model changes = %+v, want the stored spelling", deleted)
	}
}

func ptr(value float64) *float64 { return &value }

func TestStatsAggregatesDaysModelsAndKeys(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 9, 9, 0, 0, 0, loc))
	keys := []KeyConfig{testKey(t, "k1", "cpa_k1"), testKey(t, "k2", "cpa_k2")}
	keys[0].Name = "Alice"
	store := newStore(t, clock, keys, []ModelPrice{tokenPrice("gpt-5.6", "", 1, 2)})
	store.RecordUsage(tokenEvent("k1", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_000_000, CacheReadTokens: 200_000}))
	clock.Set(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store.RecordUsage(tokenEvent("k2", "openai", "gpt-5.6", UsageDetail{OutputTokens: 1_500_000}))
	store.RecordUsage(tokenEvent("k2", "openai", "unknown-model", UsageDetail{InputTokens: 3}))

	stats := store.Stats(3)
	if stats.Timezone != "Asia/Shanghai" || stats.From != "2026-10-08" || stats.To != "2026-10-10" || !nearly(stats.Totals.ChargedUSD, 4) || stats.Totals.Calls != 3 || stats.Totals.UnpricedCalls != 1 || stats.Totals.CacheReadTokens != 200_000 {
		t.Fatalf("totals = %+v", stats)
	}
	if len(stats.Days) != 3 || stats.Days[0] != (StatsDay{Date: "2026-10-08"}) || stats.Days[1].Date != "2026-10-09" || !nearly(stats.Days[1].ChargedUSD, 1) || stats.Days[2].Calls != 2 {
		t.Fatalf("days = %+v", stats.Days)
	}
	if len(stats.Models) != 2 || stats.Models[0].Name != "gpt-5.6" || !stats.Models[0].Priced || stats.Models[1].Name != "unknown-model" || stats.Models[1].Priced {
		t.Fatalf("models = %+v", stats.Models)
	}
	if len(stats.Keys) != 2 || stats.Keys[0].ID != "k2" || stats.Keys[0].Name != "k2" || stats.Keys[1].Name != "Alice" || stats.KeysTotal != 2 || stats.KeysTruncated {
		t.Fatalf("keys = %+v", stats.Keys)
	}
	if len(stats.UnpricedModels) != 1 || stats.UnpricedModels[0] != (UnpricedModel{Name: "unknown-model", Calls: 1, LastSeen: "2026-10-10"}) {
		t.Fatalf("unpriced = %+v", stats.UnpricedModels)
	}
	if stats = store.Stats(1); stats.From != "2026-10-10" || stats.Totals.Calls != 2 || len(stats.Days) != 1 {
		t.Fatalf("one-day stats = %+v", stats)
	}
	// Out-of-range and oversized day counts fall back to the default and the cap.
	if stats = store.Stats(0); len(stats.Days) != 30 {
		t.Fatalf("default range = %d days", len(stats.Days))
	}
	if stats = store.Stats(365); len(stats.Days) != UsageRetentionDays || stats.From != "2026-07-13" {
		t.Fatalf("capped range = %d days from %s", len(stats.Days), stats.From)
	}
}

// Deleting a key forgets its periods and RPM window but not its days; a key
// recreated under the same id starts fresh periods anchored at its new
// creation and inherits the retained days, so stats stop calling it deleted.
func TestDeletedKeyHistoryOutlivesTheKeyAndRejoinsARecreatedOne(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "k", "cpa_k")
	key.RPM, key.CreatedAt = 1, time.Date(2026, 10, 1, 9, 0, 0, 0, loc)
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	headers := bearer("cpa_k")
	admitRequest(store, "POST", "/v1/chat/completions", headers, nil)
	store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 2_000_000}))
	if err := store.DeleteKey("k"); err != nil {
		t.Fatal(err)
	}
	if store.FindByAPIKey("cpa_k") != nil || store.Status()["key_count"] != 0 || len(store.Status()["rpm_usage"].(map[string]int)) != 0 {
		t.Fatalf("deleted key still resolves: %+v", store.Status())
	}
	if got := store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 1})); got != UsageIgnoredUnknownKey {
		t.Fatalf("usage for a deleted key = %q", got)
	}
	if _, _, ok := store.ModelUsageFor("k"); ok {
		t.Fatal("deleted key still has a usage page")
	}
	if stats := store.Stats(1); len(stats.Keys) != 1 || !stats.Keys[0].Deleted || stats.Keys[0].Name != "" || stats.Keys[0].Calls != 1 {
		t.Fatalf("stats after delete = %+v", stats.Keys)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	again := testKey(t, "k", "cpa_k_again")
	again.RPM = 1
	if err := store.UpsertKey(again); err != nil {
		t.Fatal(err)
	}
	summary := store.UsageSummaryFor(keyByID(t, store, "k"))
	if summary.DailyUSD != 0 || summary.WeeklyUSD != 0 || summary.MonthlyUSD != 0 || summary.Cycles[1].ResetKind != "initial" || !summary.Cycles[1].StartedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("recreated key inherited periods: %+v", summary)
	}
	if admission := admitRequest(store, "POST", "/v1/chat/completions", bearer("cpa_k_again"), nil); !admission.Allowed {
		t.Fatalf("recreated key inherited the RPM window: %+v", admission)
	}
	if stats := store.Stats(2); len(stats.Keys) != 1 || stats.Keys[0].Deleted || stats.Keys[0].Name != "k" || stats.Keys[0].Calls != 1 {
		t.Fatalf("stats after recreate = %+v", stats.Keys)
	}
	if _, history, _ := store.UsageHistoryFor("k", 2); history[0].CallCount != 1 || history[1].CallCount != 0 {
		t.Fatalf("history after recreate = %+v", history)
	}
	reloaded := reopenStore(t, clock, store.StatePath())
	if got := reloaded.UsageSummaryFor(keyByID(t, reloaded, "k")); got.Cycles[1].ResetKind != "initial" || !got.Cycles[1].StartedAt.Equal(clock.Now()) {
		t.Fatalf("recreated periods not persisted: %+v", got)
	}
}

// The delete audit reports the periods current at deletion, not the ones
// last touched: an expired daily period reads as $0 even when nothing
// advanced it since the last record.
func TestDeleteKeyAuditsCurrentPeriods(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 3_000_000}))
	clock.Advance(5 * 24 * time.Hour)
	if err := store.DeleteKey("k"); err != nil {
		t.Fatal(err)
	}
	events, err := store.AuditEvents("k", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	changes := events[0].Changes
	if events[0].Action != "delete_key" || changes["daily_usd"].From != float64(0) || changes["weekly_usd"].From != float64(3) || changes["monthly_usd"].From != float64(3) {
		t.Fatalf("delete_key changes = %+v", changes)
	}
}

// Persisted state stays authoritative on reconfigure: a key that is no
// longer in the state file must stop authenticating.
func TestReconfigureDoesNotResurrectKeysMissingFromState(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "on-disk", "cpa_on_disk")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	if err := store.UpsertKey(testKey(t, "revoked", "cpa_revoked")); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(store.StatePath(), state.DatasetID, state.Keys[:1], state.Models); err != nil {
		t.Fatal(err)
	}
	// Seeds are ignored once a state file exists.
	if err := store.Configure(Config{Enabled: true, StateFile: store.StatePath(), UsageTimezone: "Asia/Shanghai", Keys: []KeyConfig{testKey(t, "seed", "cpa_seed")}}); err != nil {
		t.Fatal(err)
	}
	if keys := store.Keys(); len(keys) != 1 || keys[0].ID != "on-disk" || store.FindByAPIKey("cpa_revoked") != nil || store.FindByAPIKey("cpa_seed") != nil {
		t.Fatalf("keys after reconfigure = %+v", keys)
	}
}

// Ids are matched exactly: with "Team" and "team" both registered, a
// differently cased id names neither of them.
func TestKeyIDsMatchExactly(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "Team", "cpa_upper"), testKey(t, "team", "cpa_lower")}, nil)
	if _, _, err := store.RotateKey("TEAM"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("rotate with another case = %v, want ErrUnknownKey", err)
	}
	if _, _, ok := store.ModelUsageFor("TEAM"); ok {
		t.Fatal("usage page resolved an id with another case")
	}
	if got := store.RecordUsage(tokenEvent("TEAM", "codex", "fast", UsageDetail{InputTokens: 1})); got != UsageIgnoredUnknownKey {
		t.Fatalf("usage for an id with another case = %q", got)
	}
	if _, stored, err := store.RotateKey("team"); err != nil || stored.ID != "team" {
		t.Fatalf("rotate exact id = %+v err=%v", stored, err)
	}
}

func TestKeyHelpers(t *testing.T) {
	hash := mustHash(t, "cpa_secret")
	if !strings.HasPrefix(hash, HashPrefix) || !MatchHash("cpa_secret", hash) || MatchHash("wrong", hash) || MatchHash("cpa_secret", strings.TrimPrefix(hash, HashPrefix)) {
		t.Fatalf("hash = %q", hash)
	}
	if _, err := HashKey("  "); err == nil {
		t.Fatal("empty key hashed")
	}
	plain, err := GenerateKey()
	if err != nil || !strings.HasPrefix(plain, "cpa_") || len(plain) < 40 {
		t.Fatalf("generated key = %q err=%v", plain, err)
	}
	if got := PreviewKey("cpa_abcdefghijklmnopqrstuvwxyz"); got != "cpa_abc...vwxyz" {
		t.Fatalf("preview = %q", got)
	}
	if got := PreviewKey("short"); got != "short" {
		t.Fatalf("short preview = %q", got)
	}
	for name, tc := range map[string]struct {
		headers http.Header
		query   map[string][]string
		want    string
	}{
		"bearer":                   {http.Header{"Authorization": {"Bearer cpa_1"}}, nil, "cpa_1"},
		"bearer wins over x-api":   {http.Header{"Authorization": {"Bearer cpa_1"}, "X-Api-Key": {"cpa_2"}}, nil, "cpa_1"},
		"non-bearer authorization": {http.Header{"Authorization": {"Basic abc"}, "X-Api-Key": {"cpa_2"}}, nil, "cpa_2"},
		"goog header":              {http.Header{"X-Goog-Api-Key": {"cpa_3"}}, nil, "cpa_3"},
		"query fallback":           {nil, map[string][]string{"key": {"cpa_4"}}, "cpa_4"},
		"api_key query first":      {nil, map[string][]string{"api_key": {" cpa_5 "}, "key": {"cpa_6"}}, "cpa_5"},
		"header beats query":       {http.Header{"X-Api-Key": {"cpa_7"}}, map[string][]string{"key": {"cpa_8"}}, "cpa_7"},
		"nothing":                  {http.Header{"Authorization": {"Bearer"}}, map[string][]string{"key": {""}}, ""},
	} {
		if got := ExtractAPIKey(tc.headers, tc.query); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	for path, want := range map[string]bool{"/v1/models": true, "/openai/v1/models/": true, "/v1beta/models": true, "/v1beta/models/gemini-2.5-pro:generateContent": false, "/v1/models/gpt": false, "/v1/chat/completions": false} {
		if got := IsModelsEndpoint(path); got != want {
			t.Errorf("IsModelsEndpoint(%q) = %v", path, got)
		}
	}
}

// Where admission happens depends on whether CPA will run the interceptor
// before upstream and whether the key is in a header it can read.
func TestAdmissionStageFollowsRouteAndKeyLocation(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "k", "cpa_k")
	key.RPM = 1
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	headers := bearer("cpa_k")
	query := map[string][]string{"key": {"cpa_k"}}
	for name, tc := range map[string]struct {
		method, path string
		headers      http.Header
		query        map[string][]string
		deferred     bool
	}{
		"chat with header key":           {"POST", "/v1/chat/completions", headers, nil, true},
		"claude messages":                {"POST", "/v1/messages", headers, nil, true},
		"codex responses":                {"POST", "/backend-api/codex/responses", headers, nil, true},
		"gemini generate":                {"POST", "/v1beta/models/gemini-2.5-pro:generateContent", headers, nil, true},
		"video retrieval":                {"GET", "/v1/videos/vid_1", headers, nil, true},
		"gemini generate with query key": {"POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil, query, false},
		"realtime is not intercepted":    {"POST", "/v1/realtime/calls", headers, nil, false},
		"unknown route":                  {"POST", "/v1/embeddings", headers, nil, false},
	} {
		store.ResetRPM("k")
		decision := store.Authenticate(tc.method, tc.path, tc.headers, tc.query)
		if !decision.Allowed || decision.Deferred != tc.deferred || decision.Principal != "k" {
			t.Errorf("%s: %+v, want allowed with deferred=%v", name, decision, tc.deferred)
		}
		// Admission at authentication already counted the request.
		if again := store.Authenticate(tc.method, tc.path, tc.headers, tc.query); !tc.deferred && (again.Allowed || again.Reason != "rpm_exceeded") {
			t.Errorf("%s: second request %+v, want rpm_exceeded at authentication", name, again)
		} else if tc.deferred && !again.Allowed {
			t.Errorf("%s: authentication counted a deferred request: %+v", name, again)
		}
	}
	// A WebSocket handshake checks quota only; the per-turn RPM is left to the
	// interceptor, so repeated handshakes are not rate limited here.
	store.ResetRPM("k")
	for i := 0; i < 3; i++ {
		if decision := store.Authenticate("GET", "/v1/responses", headers, nil); !decision.Allowed || decision.Deferred {
			t.Fatalf("handshake %d = %+v", i, decision)
		}
	}
	if usage := store.Status()["rpm_usage"].(map[string]int); usage["k"] != 0 {
		t.Fatalf("handshakes counted RPM: %+v", usage)
	}
}
