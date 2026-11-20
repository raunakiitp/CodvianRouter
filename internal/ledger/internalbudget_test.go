package ledger

import (
	"context"
	"testing"
	"time"
)

// M5.11: internal scoped budgets — scope × period × limit type,
// allow/warn/deny, most-restrictive wins.
//
// 2026-11-18 is a Wednesday; all windows are UTC calendar
// periods (midnight reset).
func TestInternalBudgetEvaluate(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// Seed $4 of spend today (integer dollars: 4/5 is exactly
	// 0.8 in binary, so the 80% warning threshold is exact) +
	// 2 requests.
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 4.0, 1, 1, 1, 0, 2000)
	seedUsageRow(t, l, now.Add(-30*time.Minute), "k2", "anthropic", "claude-x", 0.0, 1, 1, 1, 0, 500)

	set := func(b InternalBudget) {
		t.Helper()
		if err := l.SetInternalBudget(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// Global currency $5: used $4 → 80% → warn at 0.8.
	set(InternalBudget{ID: "g1", Scope: InternalBudgetScopeGlobal, Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 5, WarningThreshold: 0.8, Enabled: true})
	ev, err := l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now)
	if err != nil || ev == nil || ev.Decision != InternalBudgetWarn || ev.Budget.ID != "g1" {
		t.Fatalf("global $5 at $4: ev=%+v err=%v, want warn/g1", ev, err)
	}
	if ev.Used != 4 || ev.Remaining != 1 {
		t.Errorf("used/remaining = %v/%v, want 4/1", ev.Used, ev.Remaining)
	}
	// Disabled budgets never apply.
	set(InternalBudget{ID: "g0", Scope: InternalBudgetScopeGlobal, Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 1, Enabled: false})
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now)
	if ev == nil || ev.Decision != InternalBudgetWarn {
		t.Fatalf("disabled $0.001 must not deny: ev=%+v", ev)
	}
	// Provider scope: anthropic has $0 spend (the row is
	// unpriced) → allow; openai $4 vs $3 → deny.
	set(InternalBudget{ID: "p1", Scope: InternalBudgetScopeProvider, ProviderID: "openai", Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 3, Enabled: true})
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now)
	if ev == nil || ev.Decision != InternalBudgetDeny || ev.Budget.ID != "p1" {
		t.Fatalf("openai $3 at $4: ev=%+v, want deny/p1", ev)
	}
	ev, _ = l.EvaluateInternalBudgets(ctx, "anthropic", "claude-x", nil, now)
	if ev == nil || ev.Decision != InternalBudgetWarn {
		t.Fatalf("anthropic: ev=%+v, want warn (global still applies)", ev)
	}
	// Model scope, requests: gpt-4o has 1 request today vs
	// limit 1 → deny; the global warn loses to the deny.
	set(InternalBudget{ID: "m1", Scope: InternalBudgetScopeModel, ModelID: "gpt-4o", Period: "daily", LimitType: InternalBudgetLimitRequests, LimitValue: 1, Enabled: true})
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now)
	if ev == nil || ev.Decision != InternalBudgetDeny || ev.Budget.ID != "m1" {
		t.Fatalf("gpt-4o 1/1 requests: ev=%+v, want deny/m1 (smallest remaining wins)", ev)
	}
	// Tokens, weekly window: 2500 tokens this week vs 3000 →
	// allow (83% < 0.8? No: 2500/3000 = 83% >= 80% → warn).
	set(InternalBudget{ID: "t1", Scope: InternalBudgetScopeGlobal, Period: "weekly", LimitType: InternalBudgetLimitTokens, LimitValue: 3000, Enabled: true})
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now)
	if ev == nil || ev.Decision != InternalBudgetDeny {
		t.Fatalf("weekly tokens must not hide the deny: ev=%+v", ev)
	}
	// A request for a different model with no model budget and
	// no provider budget sees the global warn + weekly warn —
	// the smallest remaining wins (global: 0.01 remaining).
	ev, _ = l.EvaluateInternalBudgets(ctx, "anthropic", "other-model", nil, now)
	if ev == nil || ev.Decision != InternalBudgetWarn || ev.Budget.ID != "g1" {
		t.Fatalf("other model: ev=%+v, want warn/g1", ev)
	}
	// Pool scope: a pool with k1 + k2 as members, $4 total
	// vs $5 → warn; a pool the caller is not in does not
	// apply.
	pool := testPool()
	pool.ID = "pool1"
	pool.Allocations = []PoolAllocation{{KeyID: "k1", Weight: 50}, {KeyID: "k2", Weight: 50}}
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	set(InternalBudget{ID: "pool1-b", Scope: InternalBudgetScopePool, PoolID: "pool1", Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 5, Enabled: true})
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", []string{"pool1"}, now)
	if ev == nil || ev.Decision != InternalBudgetDeny {
		t.Fatalf("pool member: ev=%+v, want deny (provider p1 still applies)", ev)
	}
	ev, _ = l.EvaluateInternalBudgets(ctx, "anthropic", "other-model", []string{"pool9"}, now)
	if ev == nil || ev.Budget.ID != "g1" {
		t.Fatalf("non-member pool: ev=%+v, want warn/g1 (pool budget must not apply)", ev)
	}
	// Last week's spend does not count in any current window
	// (daily, weekly and monthly are all empty a week back).
	ev, _ = l.EvaluateInternalBudgets(ctx, "openai", "gpt-4o", nil, now.AddDate(0, 0, -7))
	if ev == nil || ev.Decision != InternalBudgetAllow {
		t.Fatalf("last week: ev=%+v, want allow (no spend in any window)", ev)
	}
}

func TestInternalBudgetProviderCheck(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 4.0, 1, 1, 1, 0, 2000)
	if err := l.SetInternalBudget(ctx, InternalBudget{ID: "p1", Scope: InternalBudgetScopeProvider, ProviderID: "openai", Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 3, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetInternalBudget(ctx, InternalBudget{ID: "g1", Scope: InternalBudgetScopeGlobal, Period: "daily", LimitType: InternalBudgetLimitCurrency, LimitValue: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The provider check sees only provider-scoped budgets: the
	// global $0.01 deny must NOT surface here (the server gate
	// owns global scope).
	ev, err := l.CheckInternalBudgetForProvider(ctx, "openai", now)
	if err != nil || ev == nil || ev.Decision != InternalBudgetDeny || ev.Budget.ID != "p1" {
		t.Fatalf("provider check openai: ev=%+v err=%v, want deny/p1", ev, err)
	}
	ev, err = l.CheckInternalBudgetForProvider(ctx, "anthropic", now)
	if err != nil || ev != nil {
		t.Fatalf("provider check anthropic: ev=%+v err=%v, want nil", ev, err)
	}
}

func TestInternalBudgetCRUD(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	b := InternalBudget{ID: "b1", Scope: InternalBudgetScopeGlobal, Period: "monthly", LimitType: InternalBudgetLimitTokens, LimitValue: 100000, WarningThreshold: 0, Enabled: true}
	if err := l.SetInternalBudget(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := l.GetInternalBudget(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	// Zero threshold is stored as the default 0.8.
	if got.WarningThreshold != 0.8 {
		t.Errorf("threshold = %v, want 0.8 (zero stored as default)", got.WarningThreshold)
	}
	if got.Period != "monthly" || got.LimitValue != 100000 {
		t.Errorf("round-trip = %+v, want monthly/100000", got)
	}
	list, err := l.ListInternalBudgets(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d rows err %v, want 1", len(list), err)
	}
	if err := l.DeleteInternalBudget(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	list, _ = l.ListInternalBudgets(ctx)
	if len(list) != 0 {
		t.Errorf("list after delete = %d, want 0", len(list))
	}
}
