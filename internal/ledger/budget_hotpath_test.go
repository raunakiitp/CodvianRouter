package ledger

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

// The hot-path budget reads (CHECKLIST 249-250): GetBudget and
// GetKeyBudget run on prepared statements now, and
// CheckBudgetOrError / CheckBudgetForEstimate read the budget row
// once instead of twice. Behaviour must be identical to the
// two-read version on every path: no row, a row below/above the
// 80% warn line, and an exhausted fail-closed cap.
func TestCheckBudgetOrErrorSingleReadBehaviour(t *testing.T) {
	ctx := context.Background()

	// No row: unlimited, +Inf remaining, no warning, no error —
	// the per-request bench path (fresh gateway key, no budget).
	l := openTestLedger(t)
	remaining, warn, err := l.CheckBudgetOrError(ctx, "no-budget-key")
	if err != nil || warn || !math.IsInf(remaining, 1) {
		t.Fatalf("no-row CheckBudgetOrError = (%v, warn=%v, err=%v), want (+Inf, false, nil)", remaining, warn, err)
	}

	// Row present, spent below 80%: no warning.
	if err := l.SetBudget(ctx, "key-1", 100, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	remaining, warn, err = l.CheckBudgetOrError(ctx, "key-1")
	if err != nil || warn || remaining != 100 {
		t.Fatalf("fresh budget CheckBudgetOrError = (%v, warn=%v, err=%v), want (100, false, nil)", remaining, warn, err)
	}

	// Spend 85 of 100 via RecordUsage: warn fires at >= 80%.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 85, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{{Model: "m"}: {PromptPerToken: 1.0}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	remaining, warn, err = l.CheckBudgetOrError(ctx, "key-1")
	if err != nil || !warn || remaining != 15 {
		t.Fatalf("85%% spent CheckBudgetOrError = (%v, warn=%v, err=%v), want (15, true, nil)", remaining, warn, err)
	}

	// Exhausted, fail-closed: the typed 402 error with the same
	// numbers the old two-read version produced.
	l.FailClosed = true
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 15, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{{Model: "m"}: {PromptPerToken: 1.0}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	remaining, warn, err = l.CheckBudgetOrError(ctx, "key-1")
	if err == nil {
		t.Fatalf("exhausted fail-closed CheckBudgetOrError = (%v, warn=%v, nil), want BudgetExceededError", remaining, warn)
	}
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("exhausted fail-closed err = %T %v, want *BudgetExceededError", err, err)
	}
	if be.CapUSD != 100 || be.SpentUSD != 100 || be.RemainingUSD != 0 {
		t.Fatalf("BudgetExceededError = %+v, want cap=100 spent=100 remaining=0", be)
	}
}

// Repeated reads through the prepared statements must agree with
// each other and with the direct query, for present and missing
// rows, on both budget stores.
func TestPreparedBudgetReadsAgree(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Missing rows, many times (the bench path: no budget set).
	for i := 0; i < 50; i++ {
		if _, err := l.GetBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetBudget(missing) #%d = %v, want sql.ErrNoRows", i, err)
		}
		if _, err := l.GetKeyBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetKeyBudget(missing) #%d = %v, want sql.ErrNoRows", i, err)
		}
	}
	// Present rows, many times.
	if err := l.SetBudget(ctx, "key-1", 10, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if err := l.SetKeyBudget(ctx, "key-1", 20, UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	for i := 0; i < 50; i++ {
		b, err := l.GetBudget(ctx, "key-1")
		if err != nil || b.CapUSD != 10 || b.Period != "monthly" {
			t.Fatalf("GetBudget #%d = %+v, err %v, want cap=10 period=monthly", i, b, err)
		}
		kb, err := l.GetKeyBudget(ctx, "key-1")
		if err != nil || kb.LimitUSD != 20 || kb.UnpricedPolicy != UnpricedFailClosed {
			t.Fatalf("GetKeyBudget #%d = %+v, err %v, want limit=20 fail_closed", i, kb, err)
		}
	}
	// A deleted row goes back to ErrNoRows on the prepared path.
	if err := l.DeleteKeyBudget(ctx, "key-1"); err != nil {
		t.Fatalf("DeleteKeyBudget: %v", err)
	}
	if _, err := l.GetKeyBudget(ctx, "key-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetKeyBudget after delete = %v, want sql.ErrNoRows", err)
	}
}

// CheckBudgetForEstimate keeps its single-read behaviour too: an
// estimate that fits is allowed with the 80% warn line, an
// estimate that blows a fail-closed cap is refused.
func TestCheckBudgetForEstimateSingleReadBehaviour(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	l.FailClosed = true
	if err := l.SetBudget(ctx, "key-1", 10, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	remaining, warn, err := l.CheckBudgetForEstimate(ctx, "key-1", 1.0)
	if err != nil || warn || remaining != 10 {
		t.Fatalf("fitting estimate = (%v, warn=%v, err=%v), want (10, false, nil)", remaining, warn, err)
	}
	remaining, warn, err = l.CheckBudgetForEstimate(ctx, "key-1", 9.0)
	if err != nil || !warn || remaining != 10 {
		t.Fatalf("near-cap estimate = (%v, warn=%v, err=%v), want (10, true, nil)", remaining, warn, err)
	}
	_, _, err = l.CheckBudgetForEstimate(ctx, "key-1", 11.0)
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("over-cap estimate err = %v, want *BudgetExceededError", err)
	}
}

func openBenchLedger(b *testing.B) *Ledger {
	b.Helper()
	l, err := Open(filepath.Join(b.TempDir(), "ledger.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { l.Close() })
	return l
}

// BenchmarkGetBudgetMissDirect is the pre-fix path for comparison:
// the same SELECT issued through l.db (re-prepared on every call).
// It must measure slower than BenchmarkGetBudgetMiss by about the
// _sqlite3Prepare share, or the prepared statement is not earning
// its keep and the claim in the report must say so.
func BenchmarkGetBudgetMissDirect(b *testing.B) {
	l := openBenchLedger(b)
	ctx := context.Background()
	var bd Budget
	for i := 0; i < b.N; i++ {
		err := l.db.QueryRowContext(ctx, `
			SELECT key_id, cap_usd, spent_usd, period FROM budgets
			WHERE key_id = ?`, "missing").Scan(&bd.KeyID, &bd.CapUSD, &bd.SpentUSD, &bd.Period)
		if !errors.Is(err, sql.ErrNoRows) {
			b.Fatalf("direct GetBudget(missing): %v", err)
		}
	}
}

// BenchmarkGetBudgetMiss is the per-request bench path: a key with
// no budget row. Before the fix (profile, 2026-11-18) a GetBudget
// call cost ~36 µs (prepare + execute + scan); the prepared
// statement must measure well under that.
func BenchmarkGetBudgetMiss(b *testing.B) {
	l := openBenchLedger(b)
	ctx := context.Background()
	if _, err := l.GetBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		b.Fatalf("GetBudget(missing): %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.GetBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			b.Fatalf("GetBudget(missing): %v", err)
		}
	}
}

// BenchmarkGetKeyBudgetMiss is the same for the rolling key-budget
// guard, also called once per request.
func BenchmarkGetKeyBudgetMiss(b *testing.B) {
	l := openBenchLedger(b)
	ctx := context.Background()
	if _, err := l.GetKeyBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		b.Fatalf("GetKeyBudget(missing): %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.GetKeyBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			b.Fatalf("GetKeyBudget(missing): %v", err)
		}
	}
}

// BenchmarkCheckBudgetOrErrorMiss is the full pre-send guard on
// the bench path (one prepared GetBudget read, no row).
func BenchmarkCheckBudgetOrErrorMiss(b *testing.B) {
	l := openBenchLedger(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := l.CheckBudgetOrError(ctx, "missing"); err != nil {
			b.Fatalf("CheckBudgetOrError(missing): %v", err)
		}
	}
}
