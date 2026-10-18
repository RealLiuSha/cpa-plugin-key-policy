package policy

import (
	"net/http"
	"testing"
	"time"
)

func TestAuthenticateAdmitsRPMThenQuota(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "team", "cpa_team")
	key.RPM, key.DailyLimitUSD, key.WeeklyLimitUSD = 1, 1, 1.5
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("gpt-5.6", "", 1, 0)})
	headers := bearer("cpa_team")

	if decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer nope"}}, nil); decision.Known || decision.Allowed || decision.Reason != "unknown_key" {
		t.Fatalf("unknown key = %+v", decision)
	}
	if decision := store.Authenticate("GET", "/v1/models", headers, nil); !decision.Allowed || !decision.ModelList || decision.Deferred {
		t.Fatalf("model list = %+v, want allowed without admission", decision)
	}
	if admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); !admission.Allowed || !admission.Deferred {
		t.Fatalf("first request = %+v", admission)
	}
	admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil)
	if admission.Allowed || admission.Reason != "rpm_exceeded" || admission.Denial.Limit != 1 || !admission.Denial.RetryAt.Equal(clock.Now().Add(time.Minute).UTC()) {
		t.Fatalf("second request = %+v denial=%+v, want rpm_exceeded", admission, admission.Denial)
	}
	if decision := store.Authenticate("GET", "/v1/models", headers, nil); !decision.Allowed {
		t.Fatalf("model list during rpm block = %+v", decision)
	}
	if decision := store.Authenticate("GET", "/v1beta/models", headers, nil); !decision.Allowed || !decision.ModelList {
		t.Fatalf("gemini model list during rpm block = %+v", decision)
	}
	// Query-string keys are admitted during authentication, where CPA answers 401.
	query := map[string][]string{"key": {"cpa_team"}}
	if decision := store.Authenticate("POST", "/v1beta/models/gemini:generateContent", nil, query); decision.Allowed || decision.Deferred || decision.Reason != "rpm_exceeded" {
		t.Fatalf("query-key request = %+v, want rpm_exceeded in authentication", decision)
	}
	clock.Advance(time.Minute)
	store.RecordUsage(tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 1_200_000}))
	admission = admitRequest(store, "POST", "/v1/chat/completions", headers, nil)
	wantRetry := time.Date(2026, 10, 11, 0, 0, 0, 0, loc)
	if admission.Allowed || admission.Reason != "daily_exceeded" || !admission.Denial.RetryAt.Equal(wantRetry) || admission.Denial.Limit != 1 || admission.Denial.Timezone != "Asia/Shanghai" {
		t.Fatalf("over quota = %+v denial=%+v", admission, admission.Denial)
	}
	// Both daily and weekly are exhausted: the later reset decides the retry time.
	store.RecordUsage(tokenEvent("team", "openai", "gpt-5.6", UsageDetail{InputTokens: 300_000}))
	if decision := store.Authenticate("GET", "/v1/responses", headers, nil); decision.Allowed || decision.Reason != "weekly_exceeded" || !decision.Denial.RetryAt.After(wantRetry) {
		t.Fatalf("websocket handshake over quota = %+v denial=%+v", decision, decision.Denial)
	}
	if decision := store.Authenticate("GET", "/v1/models", headers, nil); !decision.Allowed {
		t.Fatalf("model list over quota = %+v", decision)
	}
	clock.Advance(time.Minute) // a fresh RPM window; retrieval still counts RPM
	if denial := store.AdmitIntercepted(headers, "/v1/videos/:request_id", "t"); denial != nil {
		t.Fatalf("video retrieval over quota = %+v, want admitted", denial)
	}
	summary := store.UsageSummaryFor(keyByID(t, store, "team"))
	if summary.Status != "limited" || summary.BlockedReason != "daily_exceeded" || !summary.SoftLimitHit {
		t.Fatalf("summary = %+v", summary)
	}
	clock.Set(wantRetry.Add(time.Minute))
	if admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); admission.Allowed || admission.Reason != "weekly_exceeded" {
		t.Fatalf("next day = %+v, want weekly still exhausted", admission)
	}
	key = keyByID(t, store, "team")
	key.Enabled = false
	if err := store.UpsertKey(key); err != nil {
		t.Fatal(err)
	}
	if decision := store.Authenticate("GET", "/v1/models", headers, nil); decision.Allowed || decision.Reason != "key_disabled" {
		t.Fatalf("disabled key = %+v", decision)
	}
}

func TestRPMIsAFixedWindowPerKey(t *testing.T) {
	loc := mustShanghai(t)
	start := time.Date(2026, 10, 10, 9, 0, 0, 0, loc)
	clock := newTestClock(start)
	alice, bob := testKey(t, "alice", "cpa_alice"), testKey(t, "bob", "cpa_bob")
	alice.RPM, bob.RPM = 2, 1
	store := newStore(t, clock, []KeyConfig{alice, bob}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	headers := bearer("cpa_alice")
	for i := 0; i < 2; i++ {
		if admission := admitRequest(store, "POST", "/v1/messages", headers, nil); !admission.Allowed {
			t.Fatalf("request %d = %+v", i+1, admission)
		}
	}
	clock.Advance(20 * time.Second)
	admission := admitRequest(store, "POST", "/v1/messages", headers, nil)
	if admission.Allowed || admission.Reason != "rpm_exceeded" || admission.Denial.Limit != 2 || !admission.Denial.RetryAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("third request = %+v denial=%+v, want denied until the window that opened at the first request ends", admission, admission.Denial)
	}
	if admission := admitRequest(store, "POST", "/v1/messages", bearer("cpa_bob"), nil); !admission.Allowed {
		t.Fatalf("another key shares the window: %+v", admission)
	}
	clock.Set(start.Add(59 * time.Second))
	if admission := admitRequest(store, "POST", "/v1/messages", headers, nil); admission.Allowed {
		t.Fatalf("request one second before the window ends = %+v", admission)
	}
	clock.Set(start.Add(time.Minute))
	if admission := admitRequest(store, "POST", "/v1/messages", headers, nil); !admission.Allowed {
		t.Fatalf("request when the window ends = %+v", admission)
	}
	if usage := store.Status()["rpm_usage"].(map[string]int); usage["alice"] != 1 || usage["bob"] != 1 {
		t.Fatalf("rpm_usage = %+v", usage)
	}
	if err := store.ResetRPM("alice"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if admission := admitRequest(store, "POST", "/v1/messages", headers, nil); !admission.Allowed {
			t.Fatalf("request %d after reset = %+v", i+1, admission)
		}
	}
	clock.Set(start.Add(-time.Hour))
	if admission := admitRequest(store, "POST", "/v1/messages", headers, nil); !admission.Allowed {
		t.Fatalf("clock rollback must open a new window: %+v", admission)
	}
	// Status reports open windows only.
	clock.Advance(time.Minute)
	if usage := store.Status()["rpm_usage"].(map[string]int); len(usage) != 0 {
		t.Fatalf("rpm_usage after every window ended = %+v", usage)
	}
}

func TestDenialNamesThePeriodThatResetsLast(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	key := testKey(t, "k", "cpa_k")
	key.DailyLimitUSD, key.WeeklyLimitUSD, key.MonthlyLimitUSD = 1, 2, 3
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	headers := bearer("cpa_k")
	store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 3_000_000}))
	summary := store.UsageSummaryFor(keyByID(t, store, "k"))
	want := map[UsageResetWindow][2]any{
		UsageResetMonthly: {float64(3), summary.Cycles[2].ResetsAt},
		UsageResetWeekly:  {float64(2), summary.Cycles[1].ResetsAt},
		UsageResetDaily:   {float64(1), summary.Cycles[0].ResetsAt},
	}
	for _, window := range []UsageResetWindow{UsageResetMonthly, UsageResetWeekly, UsageResetDaily} {
		admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil)
		if admission.Allowed || admission.Reason != string(window)+"_exceeded" || admission.Denial.Limit != want[window][0] || !admission.Denial.RetryAt.Equal(want[window][1].(time.Time)) {
			t.Fatalf("with %s still exhausted: %+v denial=%+v", window, admission, admission.Denial)
		}
		if _, err := store.ResetUsageWindow("k", window, nil); err != nil {
			t.Fatal(err)
		}
	}
	if admission := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); !admission.Allowed {
		t.Fatalf("after resetting every period = %+v", admission)
	}
	if summary = store.UsageSummaryFor(keyByID(t, store, "k")); summary.Status != "normal" || summary.BlockedReason != "" || summary.SoftLimitHit {
		t.Fatalf("summary after resets = %+v", summary)
	}
}

func TestDisabledPluginAuthenticatesNothing(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 10, 9, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{testKey(t, "k", "cpa_k")}, nil)
	if err := store.Configure(Config{Enabled: false, StateFile: store.StatePath(), UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	headers := bearer("cpa_k")
	if decision := store.Authenticate("GET", "/v1/models", headers, nil); decision.Known || decision.Allowed || decision.Reason != "plugin_disabled" {
		t.Fatalf("disabled plugin = %+v", decision)
	}
	if denial := store.AdmitIntercepted(headers, "/v1/chat/completions", "t"); denial != nil {
		t.Fatalf("disabled plugin interceptor = %+v", denial)
	}
	if store.Enabled() || store.Status()["enabled"] != false {
		t.Fatal("store reports enabled")
	}
}

// Periods roll forward from their own deadlines, so after months offline the
// schedule is the same one an always-on process would have reached.
func TestCyclesAdvanceFromTheirDeadlinesAfterLongDowntime(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 8, 1, 12, 0, 0, 0, loc))
	key := testKey(t, "k", "cpa_k")
	key.CreatedAt = clock.Now()
	store := newStore(t, clock, []KeyConfig{key}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	store.RecordUsage(tokenEvent("k", "codex", "fast", UsageDetail{InputTokens: 5_000_000}))
	if err := store.Shutdown(); err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Date(2026, 11, 20, 9, 0, 0, 0, loc))
	reloaded := reopenStore(t, clock, store.StatePath())
	summary := reloaded.UsageSummaryFor(keyByID(t, reloaded, "k"))
	if summary.DailyUSD != 0 || summary.WeeklyUSD != 0 || summary.MonthlyUSD != 0 {
		t.Fatalf("usage survived the downtime: %+v", summary)
	}
	wantStarts := []time.Time{
		time.Date(2026, 11, 20, 0, 0, 0, 0, loc), // daily
		time.Date(2026, 11, 14, 0, 0, 0, 0, loc), // 2026-08-08 + 14 weeks
		time.Date(2026, 10, 30, 0, 0, 0, 0, loc), // 2026-08-31 + 60 days
	}
	for i, cycle := range summary.Cycles {
		if !cycle.StartedAt.Equal(wantStarts[i]) || !cycle.ResetsAt.Equal(wantStarts[i].AddDate(0, 0, cycle.Window.days())) || cycle.ResetKind != "automatic" {
			t.Fatalf("%s cycle = %+v, want start %s", cycle.Window, cycle, wantStarts[i])
		}
	}
	if _, history, _ := reloaded.UsageHistoryFor("k", 90); history[0].Date != "2026-08-23" || history[0].CallCount != 0 {
		t.Fatalf("history after downtime = %+v", history[0])
	}
	if stats := reloaded.Stats(90); stats.Totals.Calls != 0 || len(stats.Keys) != 0 {
		t.Fatalf("stats after downtime = %+v", stats)
	}
}
