package ledger

import (
	"context"
	"testing"
	"time"
)

// TestPairQuotaSpendWindows checks that PairQuota sums priced spend
// per (provider, model) over the 5-hour and 7-day windows, that rows
// outside a window, rows for other pairs and shared rows never
// count, and that utilisation is -1 (no per-pair limit exists) while
// ResetAt is the oldest in-window priced spend's roll-off.
func TestPairQuotaSpendWindows(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()

	// In the 5h window, pair ("p", "m"): $2.00 + $0.50.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "p", "m", 2.00, 1, 1, 1, 0, 10)
	seedUsageRow(t, l, now.Add(-4*time.Hour), "gk1", "p", "m", 0.50, 1, 1, 1, 0, 10)
	// 6h ago: outside 5h, inside 7d: $1.00.
	seedUsageRow(t, l, now.Add(-6*time.Hour), "gk1", "p", "m", 1.00, 1, 1, 1, 0, 10)
	// 3 days ago: inside 7d only: $3.00.
	seedUsageRow(t, l, now.Add(-72*time.Hour), "gk1", "p", "m", 3.00, 1, 1, 1, 0, 10)
	// 8 days ago: outside both windows: $9.00, must never count.
	seedUsageRow(t, l, now.Add(-192*time.Hour), "gk1", "p", "m", 9.00, 1, 1, 1, 0, 10)
	// Other model, same provider, in window: $5.00, must not count.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "p", "other", 5.00, 1, 1, 1, 0, 10)
	// Other provider, same model, in window: $7.00, must not count.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "q", "m", 7.00, 1, 1, 1, 0, 10)
	// Shared row for the pair, in window: $4.00 — the leader's row
	// is the billed one, so a shared row must not count.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "p", "m", 4.00, 1, 1, 1, 1, 10)
	// Failed attempt, in window, cost $0: contributes nothing.
	seedUsageRow(t, l, now.Add(-30*time.Minute), "gk1", "p", "m", 0, 0, 1, 1, 0, 10)

	pq, err := l.PairQuota(ctx, "p", "m", now)
	if err != nil {
		t.Fatalf("PairQuota: %v", err)
	}
	if pq.Spent5h != 2.50 {
		t.Errorf("Spent5h = %.4f, want 2.50", pq.Spent5h)
	}
	if pq.Spent7d != 6.50 {
		t.Errorf("Spent7d = %.4f, want 6.50 (2.50 + 1.00 + 3.00)", pq.Spent7d)
	}
	if pq.Util5h != UnknownUtilisation {
		t.Errorf("Util5h = %v, want %v (no per-pair limit exists)", pq.Util5h, UnknownUtilisation)
	}
	if pq.Util7d != UnknownUtilisation {
		t.Errorf("Util7d = %v, want %v (no per-pair limit exists)", pq.Util7d, UnknownUtilisation)
	}
	// ResetAt: the oldest in-window priced spend (4h ago) ages
	// out 5h after it was recorded, i.e. ~1h from now.
	want := now.Add(-4 * time.Hour).Add(RollingWindow)
	if pq.ResetAt.IsZero() {
		t.Fatal("ResetAt is zero, want the 4h-old spend's roll-off")
	}
	if d := pq.ResetAt.Sub(want); d > time.Second || d < -time.Second {
		t.Errorf("ResetAt = %v, want ~%v", pq.ResetAt, want)
	}
}

// TestPairQuotaNoSpend checks the honest empty state: no rows at
// all means zero spend, unknown utilisation and a zero ResetAt.
func TestPairQuotaNoSpend(t *testing.T) {
	l := openKeyBudgetLedger(t)
	pq, err := l.PairQuota(context.Background(), "p", "m", time.Now())
	if err != nil {
		t.Fatalf("PairQuota: %v", err)
	}
	if pq.Spent5h != 0 || pq.Spent7d != 0 {
		t.Errorf("spend = %.4f/%.4f, want 0/0", pq.Spent5h, pq.Spent7d)
	}
	if pq.Util5h != UnknownUtilisation || pq.Util7d != UnknownUtilisation {
		t.Errorf("utils = %v/%v, want %v/%v", pq.Util5h, pq.Util7d, UnknownUtilisation, UnknownUtilisation)
	}
	if !pq.ResetAt.IsZero() {
		t.Errorf("ResetAt = %v, want zero (no priced spend to roll off)", pq.ResetAt)
	}
}

// TestPairQuotaNoPricedSpend checks that cost-$0 rows in the window
// (unpriced or failed attempts) leave ResetAt zero: there is no
// priced spend to age out.
func TestPairQuotaNoPricedSpend(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	// Unpriced row (price_known=0) and a failed row, both cost $0.
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "p", "m", 0, 1, 1, 0, 0, 10)
	seedUsageRow(t, l, now.Add(-2*time.Hour), "gk1", "p", "m", 0, 0, 1, 1, 0, 10)
	pq, err := l.PairQuota(ctx, "p", "m", now)
	if err != nil {
		t.Fatalf("PairQuota: %v", err)
	}
	if pq.Spent5h != 0 {
		t.Errorf("Spent5h = %.4f, want 0", pq.Spent5h)
	}
	if !pq.ResetAt.IsZero() {
		t.Errorf("ResetAt = %v, want zero (no priced spend in window)", pq.ResetAt)
	}
}

// TestSpendForProviderModel checks the single-window spend lookup
// directly, including the window boundary.
func TestSpendForProviderModel(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	seedUsageRow(t, l, now.Add(-1*time.Hour), "gk1", "p", "m", 2.00, 1, 1, 1, 0, 10)
	seedUsageRow(t, l, now.Add(-6*time.Hour), "gk1", "p", "m", 1.00, 1, 1, 1, 0, 10)
	got, err := l.SpendForProviderModel(ctx, "p", "m", now.Add(-RollingWindow))
	if err != nil {
		t.Fatalf("SpendForProviderModel: %v", err)
	}
	if got != 2.00 {
		t.Errorf("spend in 5h = %.4f, want 2.00 (the 6h-old $1.00 aged out)", got)
	}
	got, err = l.SpendForProviderModel(ctx, "p", "m", now.Add(-WeeklyWindow))
	if err != nil {
		t.Fatalf("SpendForProviderModel: %v", err)
	}
	if got != 3.00 {
		t.Errorf("spend in 7d = %.4f, want 3.00", got)
	}
	got, err = l.SpendForProviderModel(ctx, "p", "nope", now.Add(-WeeklyWindow))
	if err != nil {
		t.Fatalf("SpendForProviderModel: %v", err)
	}
	if got != 0 {
		t.Errorf("spend for unknown model = %.4f, want 0", got)
	}
}

// TestUtilisation checks the spend/limit math: a real limit gives a
// clamped ratio, and any missing/non-positive limit is unknown (-1),
// never an invented denominator.
func TestUtilisation(t *testing.T) {
	cases := []struct {
		spent, limit float64
		want         float64
	}{
		{2.50, 10, 0.25},
		{0, 10, 0},
		{12, 10, 1},   // over the cap clamps to 1
		{10, 10, 1},   // exactly at the cap
		{2.50, 0, -1}, // no limit: unknown, never divide by zero
		{2.50, -5, -1},
		{0, 0, -1},
	}
	for _, c := range cases {
		if got := Utilisation(c.spent, c.limit); got != c.want {
			t.Errorf("Utilisation(%.2f, %.2f) = %v, want %v", c.spent, c.limit, got, c.want)
		}
	}
}
