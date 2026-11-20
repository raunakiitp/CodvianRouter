// B2 (M10): per-key daily / weekly / monthly USD limits — ALL
// THREE enforced at once, each against its own UTC calendar
// period's spend from the ledger (daily [today 00:00, tomorrow
// 00:00), weekly [Monday 00:00, next Monday 00:00), monthly [1st
// 00:00, next 1st 00:00) at the default reset time). ResetInterval
// only picks the status "active" window; it never exempts a set
// cap. Enforcement runs through CheckKeyBudget and ReserveSpend
// (the same gate), refuses with a 402 naming the breached window,
// and fails closed on unpriced usage in a set window's period.
package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestKeyBudgetB2AllWindowsEnforced: every set window gates, not
// just the ResetInterval-selected one. Each sub-case spends over
// exactly one window while the others are clear.
func TestKeyBudgetB2AllWindowsEnforced(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	// Wednesday 2026-11-18 12:00 UTC (week Mon Nov 16–Sun Nov 22).
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	var be *BudgetExceededError

	// Weekly over, daily clear: $6 yesterday (same week, other
	// day) against daily $10 / weekly $5, interval daily.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-week", BudgetResetDaily, fptr(10), fptr(5), fptr(100))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-25*time.Hour), "k-b2-week", "p", "m", 6.00, 1, 1, 1, 0, 10)
	err := l.CheckKeyBudget(ctx, "k-b2-week", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "weekly" || be.CapUSD != 5 || be.SpentUSD != 6 {
		t.Fatalf("weekly over-cap = %v, want BudgetExceededError weekly $5/$6", err)
	}

	// Monthly over, daily + weekly clear: $6 on Nov 9 (last
	// week, same month) against daily $10 / weekly $100 /
	// monthly $5, interval daily.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-month", BudgetResetDaily, fptr(10), fptr(100), fptr(5))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC), "k-b2-month", "p", "m", 6.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-b2-month", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "monthly" || be.CapUSD != 5 || be.SpentUSD != 6 {
		t.Fatalf("monthly over-cap = %v, want BudgetExceededError monthly $5/$6", err)
	}

	// Daily over while the interval is MONTHLY: $6 today against
	// daily $5 / weekly $100 / monthly $100 — the daily cap gates
	// even though the active window is monthly.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-daily", BudgetResetMonthly, fptr(5), fptr(100), fptr(100))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-b2-daily", "p", "m", 6.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-b2-daily", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "daily" || be.CapUSD != 5 || be.SpentUSD != 6 {
		t.Fatalf("daily over-cap (interval monthly) = %v, want BudgetExceededError daily $5/$6", err)
	}

	// Under every cap: $4 today against daily $10 / weekly $50 /
	// monthly $100 passes, and the status reports the spend in
	// all three windows.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-ok", BudgetResetDaily, fptr(10), fptr(50), fptr(100))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-b2-ok", "p", "m", 4.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-b2-ok", []string{"m"}, priced, now); err != nil {
		t.Fatalf("under-cap check = %v, want nil", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-b2-ok", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Daily.SpentUSD != 4 || st.Weekly.SpentUSD != 4 || st.Monthly.SpentUSD != 4 {
		t.Errorf("status spend = daily $%.2f weekly $%.2f monthly $%.2f, want $4 in all three", st.Daily.SpentUSD, st.Weekly.SpentUSD, st.Monthly.SpentUSD)
	}
	if st.AtLimit {
		t.Error("status at limit = true, want false under every cap")
	}
}

// TestKeyBudgetB2ZeroLimitRejected: a daily, weekly, or monthly
// limit of exactly 0 is rejected (like a negative one) — nil
// means unset; 0 would refuse every request while looking like a
// configured budget. Positive limits still store fine.
func TestKeyBudgetB2ZeroLimitRejected(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		kb   KeyBudget
	}{
		{"daily zero", windowedKey("k-b2-z", BudgetResetDaily, fptr(0), nil, nil)},
		{"weekly zero", windowedKey("k-b2-z", BudgetResetDaily, nil, fptr(0), nil)},
		{"monthly zero", windowedKey("k-b2-z", BudgetResetDaily, nil, nil, fptr(0))},
		{"daily negative", windowedKey("k-b2-z", BudgetResetDaily, fptr(-5), nil, nil)},
		{"weekly negative", windowedKey("k-b2-z", BudgetResetDaily, nil, fptr(-5), nil)},
		{"monthly negative", windowedKey("k-b2-z", BudgetResetDaily, nil, nil, fptr(-5))},
	} {
		if err := l.SetKeyBudgetConfig(ctx, tc.kb); err == nil {
			t.Errorf("%s: SetKeyBudgetConfig accepted it, want an error", tc.name)
		}
	}
	if _, err := l.GetKeyBudget(ctx, "k-b2-z"); err == nil {
		t.Error("rejected zero/negative limits left a budget row behind")
	}
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-z", BudgetResetDaily, fptr(1), fptr(2), fptr(3))); err != nil {
		t.Errorf("positive D/W/M limits rejected: %v", err)
	}
}

// TestKeyBudgetB2PerKeyIsolation: one key's cap never blocks
// another key — the windows are summed per gateway_key_id from
// usage_events, never across keys.
func TestKeyBudgetB2PerKeyIsolation(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	var be *BudgetExceededError

	// Key A: $6 this month against a $5 monthly cap — refused.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-a", BudgetResetDaily, nil, nil, fptr(5))); err != nil {
		t.Fatalf("set A: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-b2-a", "p", "m", 6.00, 1, 1, 1, 0, 10)
	// Key B: same $5 monthly cap, only $4 spent — passes.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-b", BudgetResetDaily, nil, nil, fptr(5))); err != nil {
		t.Fatalf("set B: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-b2-b", "p", "m", 4.00, 1, 1, 1, 0, 10)
	// Key C: no budget row at all — passes.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-b2-c", "p", "m", 100.00, 1, 1, 1, 0, 10)

	err := l.CheckKeyBudget(ctx, "k-b2-a", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "monthly" {
		t.Fatalf("key A = %v, want BudgetExceededError monthly", err)
	}
	if err := l.CheckKeyBudget(ctx, "k-b2-b", []string{"m"}, priced, now); err != nil {
		t.Fatalf("key B (under its own cap) = %v, want nil — A's spend must not count against B", err)
	}
	if err := l.CheckKeyBudget(ctx, "k-b2-c", []string{"m"}, priced, now); err != nil {
		t.Fatalf("key C (no budget) = %v, want nil", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-b2-b", now)
	if err != nil {
		t.Fatalf("status B: %v", err)
	}
	if st.Monthly.SpentUSD != 4 {
		t.Errorf("key B monthly spent = $%.2f, want $4 (only B's rows)", st.Monthly.SpentUSD)
	}
}

// TestKeyBudgetB2ReserveSpendEnforcesWindows: the reservation
// path runs the same all-windows gate — an estimate that would
// push a non-active window over its cap is refused, and one
// that fits reserves.
func TestKeyBudgetB2ReserveSpendEnforcesWindows(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	var be *BudgetExceededError

	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-b2-res", BudgetResetDaily, fptr(10), fptr(5), fptr(100))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-25*time.Hour), "k-b2-res", "p", "m", 4.50, 1, 1, 1, 0, 10)
	// $4.50 weekly + $1.00 estimate = $5.50 over the $5 weekly
	// cap (daily is clear at $0) — refused, window weekly.
	if _, err := l.ReserveSpend(ctx, "k-b2-res", []string{"m"}, priced, 1.00, now); !errors.As(err, &be) || be.Window != "weekly" {
		t.Fatalf("reserve over weekly cap = %v, want BudgetExceededError weekly", err)
	}
	// $4.50 + $0.25 = $4.75 fits — reserves.
	id, err := l.ReserveSpend(ctx, "k-b2-res", []string{"m"}, priced, 0.25, now)
	if err != nil {
		t.Fatalf("reserve inside the cap = %v, want an id", err)
	}
	if id == "" {
		t.Fatal("reserve inside the cap returned an empty id")
	}
	if err := l.SettleReservation(ctx, id); err != nil {
		t.Fatalf("settle: %v", err)
	}
}
