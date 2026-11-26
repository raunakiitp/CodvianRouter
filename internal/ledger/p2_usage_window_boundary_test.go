package ledger

// Boundary-semantics regression test for the usage-window defect the
// consolidated verification campaign surfaced
// (TestUsageBudgetForecastProjected flake: "requests_1h = 0" while
// spend > 0). Summary's optional until bound is EXCLUSIVE
// (ts < until); SpendFor has NO upper bound. A usage row recorded in
// the same millisecond as the caller's `now` therefore counts toward
// spend but is dropped from a Summary window bounded at `now` — the
// forecast handler must query requests with the SAME window as
// SpendFor (zero until = unbounded). This test pins all three
// behaviors at the exact boundary millisecond so the windows can
// never silently diverge again.

import (
	"context"
	"testing"
	"time"
)

func TestUsageWindowBoundaryMs(t *testing.T) {
	l := openTestLedger(t)
	ctx := context.Background()
	if err := l.SetKeyBudget(ctx, "key-boundary", 100.00, UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	// A usage row stamped at exactly `now` ms — the boundary case
	// Windows clock granularity makes routine under load.
	now := time.Now()
	nowMs := now.UnixMilli()
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO usage_events (ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, ok)
		 VALUES (?, 'key-boundary', 'p', 'm', 100, 100, 200, 0.50, 1)`,
		nowMs)
	if err != nil {
		t.Fatalf("seed boundary row: %v", err)
	}

	since := now.Add(-time.Hour)

	// SpendFor: no upper bound — the boundary row counts.
	spend, err := l.SpendFor(ctx, "key-boundary", since)
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spend < 0.499 || spend > 0.501 {
		t.Fatalf("SpendFor at boundary = %v, want ~0.50", spend)
	}

	// Summary bounded at `now` (the display form): the exclusive
	// until DROPS the boundary row — pinned so display callers
	// keep their documented window semantics.
	bounded, err := l.Summary(ctx, since, now)
	if err != nil {
		t.Fatalf("Summary bounded: %v", err)
	}
	if bounded.Requests != 0 {
		t.Fatalf("Summary(ts < until) requests = %d, want 0 at the boundary (exclusive until)", bounded.Requests)
	}

	// Summary with zero until (the forecast form): SAME window as
	// SpendFor — the boundary row MUST count, or requests and
	// spend disagree at exactly this millisecond.
	unbounded, err := l.Summary(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("Summary unbounded: %v", err)
	}
	if unbounded.Requests != 1 {
		t.Fatalf("Summary(zero until) requests = %d, want 1 — the forecast's request window must match SpendFor's", unbounded.Requests)
	}
}
