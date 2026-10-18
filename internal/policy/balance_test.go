package policy

import (
	"errors"
	"os"
	"testing"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

func balanceKey(t *testing.T, id, plain string) KeyConfig {
	t.Helper()
	key := testKey(t, id, plain)
	key.QuotaMode = QuotaModeBalance
	return key
}

// A prepaid key is refused until credited, spends its balance call by call,
// may end slightly below zero with what was in flight, and is refused again
// once the balance is used up; period limits never apply to it. A credit is
// on disk before AdjustBalance returns.
func TestBalanceKeySpendsCreditAndIsRefusedWhenUsedUp(t *testing.T) {
	loc := mustShanghai(t)
	clock := newTestClock(time.Date(2026, 10, 17, 10, 0, 0, 0, loc))
	store := newStore(t, clock, []KeyConfig{balanceKey(t, "prepaid", "cpa_prepaid")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})
	headers := bearer("cpa_prepaid")
	if got := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); got.Allowed || got.Reason != "balance_exhausted" || !got.Denial.RetryAt.IsZero() {
		t.Fatalf("uncredited balance key admission = %+v", got)
	}

	result, err := store.AdjustBalance("prepaid", 1, "first top-up", "")
	if err != nil || !nearly(result.BeforeUSD, 0) || !nearly(result.AfterUSD, 1) || !nearly(result.NetCreditedUSD, 1) || result.Duplicate {
		t.Fatalf("credit = %+v err=%v", result, err)
	}
	// Reopening without a flush proves the credit was written synchronously.
	if summary := reopenStore(t, clock, store.StatePath()).UsageSummaryFor(keyByID(t, store, "prepaid")); summary.BalanceUSD == nil || !nearly(*summary.BalanceUSD, 1) {
		t.Fatalf("credit on disk = %+v", summary)
	}
	if got := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); !got.Allowed {
		t.Fatalf("credited key refused: %+v", got)
	}

	store.RecordUsage(tokenEvent("prepaid", "codex", "fast", UsageDetail{InputTokens: 850_000}))
	summary := store.UsageSummaryFor(keyByID(t, store, "prepaid"))
	if summary.QuotaMode != QuotaModeBalance || !nearly(*summary.BalanceUSD, 0.15) || !nearly(*summary.NetCreditedUSD, 1) || summary.Status != "warning" || !summary.SoftLimitHit || !nearly(summary.DailyUSD, 0.85) {
		t.Fatalf("summary after spending 85%% = %+v", summary)
	}
	// The request admitted at $0.15 costs $0.30: the balance ends below zero.
	store.RecordUsage(tokenEvent("prepaid", "codex", "fast", UsageDetail{InputTokens: 300_000}))
	summary = store.UsageSummaryFor(keyByID(t, store, "prepaid"))
	if !nearly(*summary.BalanceUSD, -0.15) || summary.Status != "limited" || summary.BlockedReason != "balance_exhausted" {
		t.Fatalf("summary after overdraft = %+v", summary)
	}
	if got := admitRequest(store, "POST", "/v1/chat/completions", headers, nil); got.Allowed || got.Reason != "balance_exhausted" {
		t.Fatalf("exhausted balance admission = %+v", got)
	}
	// Model lists stay readable, as for an exhausted period quota.
	if got := admitRequest(store, "GET", "/v1/models", headers, nil); !got.Allowed {
		t.Fatalf("model list refused for an exhausted balance: %+v", got)
	}

	if result, err := store.AdjustBalance("prepaid", 2, "", ""); err != nil || !nearly(result.BeforeUSD, -0.15) || !nearly(result.AfterUSD, 1.85) || !nearly(result.NetCreditedUSD, 3) {
		t.Fatalf("second credit = %+v err=%v", result, err)
	}
	if result, err := store.AdjustBalance("prepaid", -0.85, "refund", ""); err != nil || !nearly(result.AfterUSD, 1) || !nearly(result.NetCreditedUSD, 2.15) {
		t.Fatalf("deduction = %+v err=%v", result, err)
	}
	events, err := store.AuditEvents("prepaid", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var adjustments []float64
	for _, event := range events {
		if event.Action == "adjust_balance" {
			adjustments = append(adjustments, event.Changes["amount_usd"].To.(float64))
		}
	}
	if len(adjustments) != 3 {
		t.Fatalf("adjust_balance audit events = %+v", events)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenStore(t, clock, store.StatePath())
	if summary := reopened.UsageSummaryFor(keyByID(t, reopened, "prepaid")); !nearly(*summary.BalanceUSD, 1) || !nearly(*summary.NetCreditedUSD, 2.15) {
		t.Fatalf("balance after restart = %+v", summary)
	}
}

// A retried adjustment with the same request id is answered with its first
// result and credits once; the id with another amount is a conflict. A
// failed write leaves the balance and the audit log untouched.
func TestBalanceAdjustmentIsIdempotentAndRollsBackOnFailure(t *testing.T) {
	clock := newTestClock(time.Date(2026, 10, 17, 10, 0, 0, 0, mustShanghai(t)))
	store := newStore(t, clock, []KeyConfig{balanceKey(t, "prepaid", "cpa_prepaid")}, nil)
	first, err := store.AdjustBalance("prepaid", 5, "", "order-1")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.AdjustBalance("prepaid", 5, "", "order-1")
	if err != nil || !retry.Duplicate || !nearly(retry.AfterUSD, first.AfterUSD) || !nearly(retry.BeforeUSD, 0) {
		t.Fatalf("retry = %+v err=%v", retry, err)
	}
	if _, err := store.AdjustBalance("prepaid", 6, "", "order-1"); !errors.Is(err, ErrBalanceRequestConflict) {
		t.Fatalf("same request id, other amount = %v", err)
	}
	if summary := store.UsageSummaryFor(keyByID(t, store, "prepaid")); !nearly(*summary.BalanceUSD, 5) {
		t.Fatalf("balance after retry = %v", *summary.BalanceUSD)
	}

	livePath := persist.UsagePath(store.StatePath())
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(livePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdjustBalance("prepaid", 7, "", "order-2"); err == nil {
		t.Fatal("adjustment should fail when the live file cannot be replaced")
	}
	if summary := store.UsageSummaryFor(keyByID(t, store, "prepaid")); !nearly(*summary.BalanceUSD, 5) || !nearly(*summary.NetCreditedUSD, 5) {
		t.Fatalf("balance after failed write = %+v, want rollback", summary)
	}
	events, _ := store.AuditEvents("prepaid", "", 10)
	if count := len(events); count != 1 { // only the first credit: seeded keys are not audited
		t.Fatalf("audit after retry and failure = %+v", events)
	}
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	// The failed request id was not remembered: a retry once the disk works applies it.
	if result, err := store.AdjustBalance("prepaid", 7, "", "order-2"); err != nil || result.Duplicate || !nearly(result.AfterUSD, 12) {
		t.Fatalf("retry after the failure = %+v err=%v", result, err)
	}
}

// Period limits belong to cycle keys, balances to balance keys. Deleting a
// key drops its balance (named in the audit event); switching a balance key
// back to cycle mode keeps its balance without spending it.
func TestBalanceModeRulesAndDeletion(t *testing.T) {
	clock := newTestClock(time.Date(2026, 10, 17, 10, 0, 0, 0, mustShanghai(t)))
	store := newStore(t, clock, []KeyConfig{testKey(t, "periodic", "cpa_periodic"), balanceKey(t, "prepaid", "cpa_prepaid")}, []ModelPrice{tokenPrice("fast", "", 1, 0)})

	limited := balanceKey(t, "prepaid", "cpa_prepaid")
	limited.DailyLimitUSD = 5
	if err := store.UpsertKey(limited); !errors.Is(err, ErrBalanceKeyLimits) {
		t.Fatalf("balance key with a daily limit = %v", err)
	}
	if _, err := store.AdjustBalance("periodic", 5, "", ""); !errors.Is(err, ErrNotBalanceMode) {
		t.Fatalf("credit on a cycle key = %v", err)
	}
	for _, amount := range []float64{0, 1e-12, 2_000_000} {
		if _, err := store.AdjustBalance("prepaid", amount, "", ""); !errors.Is(err, ErrInvalidBalanceAdjustment) {
			t.Fatalf("amount %v = %v", amount, err)
		}
	}
	if _, err := store.AdjustBalance("nobody", 1, "", ""); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("credit on an unknown key = %v", err)
	}

	if _, err := store.AdjustBalance("prepaid", 3, "", ""); err != nil {
		t.Fatal(err)
	}
	cycle := keyByID(t, store, "prepaid")
	cycle.QuotaMode = QuotaModeCycle
	if err := store.UpsertKey(cycle); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(tokenEvent("prepaid", "codex", "fast", UsageDetail{InputTokens: 1_000_000}))
	if entry := store.usage.snapshot()["prepaid"]; entry.Balance == nil || entry.Balance.Nano != USDToNano(3) {
		t.Fatalf("cycle-mode usage touched the kept balance: %+v", entry.Balance)
	}
	// The kept balance stays visible but is not a limit for the cycle key.
	if summary := store.UsageSummaryFor(keyByID(t, store, "prepaid")); summary.QuotaMode != QuotaModeCycle || summary.BalanceUSD == nil || *summary.BalanceUSD != 3 || summary.Status != "normal" {
		t.Fatalf("cycle key summary = %+v", summary)
	}

	if err := store.DeleteKey("prepaid"); err != nil {
		t.Fatal(err)
	}
	events, _ := store.AuditEvents("prepaid", "", 10)
	if deleted := events[0]; deleted.Action != "delete_key" || deleted.Changes["balance_usd"].From != 3.0 {
		t.Fatalf("delete audit = %+v", deleted)
	}
	if err := store.UpsertKey(balanceKey(t, "prepaid", "cpa_prepaid_again")); err != nil {
		t.Fatal(err)
	}
	if summary := store.UsageSummaryFor(keyByID(t, store, "prepaid")); *summary.BalanceUSD != 0 || summary.BlockedReason != "balance_exhausted" {
		t.Fatalf("recreated key inherited a balance: %+v", summary)
	}
}
