package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// The per-request global-budget memo (CHECKLIST 299-300): the
// 279-280 profile put CheckBudgetOrError/GetBudget at 8.20% cum,
// the biggest named router cost. Like the key-budget memo
// (279-280) this memo shares one GetBudget read per request per
// key — a second read on the same request context is a map
// lookup, not a second SQLite query. These tests prove the memo
// by mutating the database behind its back: a memoised read must
// NOT see the change, a fresh context must, a write through the
// same context must invalidate the memo entry, and a real
// database error must never be cached.
//
// Scope note (see budget_memo.go): on today's hot paths the
// global-budget guard reads once per request, so the dedup below
// is the mechanism's proof, not a claim that the chat path reads
// twice — the profile in
// docs/cpu-profile-2026-11-18-globalbudget.md records what the
// memo actually saved on the measured path.
func TestBudgetMemoSharesOneRead(t *testing.T) {
	l := openTestLedger(t)

	// Miss memoised: the first read finds no row; a row set
	// through a DIFFERENT context (no memo to invalidate) must
	// stay invisible to this memo.
	memoCtx := WithBudgetMemo(context.Background())
	if _, err := l.GetBudget(memoCtx, "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("memo first read = %v, want sql.ErrNoRows", err)
	}
	if err := l.SetBudget(context.Background(), "memo-key", 20, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, err := l.GetBudget(memoCtx, "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("memo second read = %v, want sql.ErrNoRows (memoised miss)", err)
	}
	// A fresh context sees the row.
	b, err := l.GetBudget(context.Background(), "memo-key")
	if err != nil || b.CapUSD != 20 || b.Period != "monthly" {
		t.Fatalf("fresh read = %+v, err %v, want cap=20 period=monthly", b, err)
	}

	// Row memoised: a fresh memo reads the row once, then a row
	// deleted behind its back (raw SQL, bypassing any
	// invalidation) stays visible to it.
	memoCtx2 := WithBudgetMemo(context.Background())
	b, err = l.GetBudget(memoCtx2, "memo-key")
	if err != nil || b.CapUSD != 20 {
		t.Fatalf("memo2 first read = %+v, err %v, want cap=20", b, err)
	}
	if _, err := l.db.ExecContext(context.Background(), `DELETE FROM budgets WHERE key_id = ?`, "memo-key"); err != nil {
		t.Fatalf("raw delete: %v", err)
	}
	b, err = l.GetBudget(memoCtx2, "memo-key")
	if err != nil || b.CapUSD != 20 {
		t.Fatalf("memo2 read after raw delete = %+v, err %v, want memoised cap=20", b, err)
	}
	if _, err := l.GetBudget(context.Background(), "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fresh read after raw delete = %v, want sql.ErrNoRows", err)
	}

	// A write through the memo context invalidates it: set then
	// read on the same context sees the new row; reset then read
	// sees spent zeroed.
	if err := l.SetBudget(memoCtx2, "memo-key", 5, "weekly"); err != nil {
		t.Fatalf("SetBudget (memo ctx): %v", err)
	}
	b, err = l.GetBudget(memoCtx2, "memo-key")
	if err != nil || b.CapUSD != 5 || b.Period != "weekly" {
		t.Fatalf("memo read after SetBudget = %+v, err %v, want cap=5 period=weekly", b, err)
	}
	// Spend through the same context: the memoised row carries
	// SpentUSD, so RecordUsage must invalidate it — the next
	// read on this request sees the moved spend, not the stale
	// pre-spend row.
	if _, err := l.RecordUsage(memoCtx2, Usage{
		GatewayKeyID: "memo-key", Model: "m",
		PromptTokens: 2, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{{Model: "m"}: {PromptPerToken: 1.0}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	b, err = l.GetBudget(memoCtx2, "memo-key")
	if err != nil || b.SpentUSD != 2 {
		t.Fatalf("memo read after RecordUsage = %+v, err %v, want spent=2", b, err)
	}
	if err := l.ResetSpent(memoCtx2, "memo-key"); err != nil {
		t.Fatalf("ResetSpent (memo ctx): %v", err)
	}
	b, err = l.GetBudget(memoCtx2, "memo-key")
	if err != nil || b.SpentUSD != 0 {
		t.Fatalf("memo read after ResetSpent = %+v, err %v, want spent=0", b, err)
	}
}

// CheckBudgetOrError must not re-read the budgets row after the
// memo has it: with a cap-0 budget (spent 0 >= cap 0, so every
// request is over the cap) the guard must still refuse on the
// same memo context after the row is deleted behind its back,
// and must allow on a fresh context.
func TestCheckBudgetOrErrorUsesMemo(t *testing.T) {
	l := openTestLedger(t)
	l.FailClosed = true
	key := "memo-check"
	memoCtx := WithBudgetMemo(context.Background())
	if err := l.SetBudget(context.Background(), key, 0, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, _, err := l.CheckBudgetOrError(memoCtx, key); !IsBudgetExceeded(err) {
		t.Fatalf("CheckBudgetOrError (memo, cap 0) = %v, want BudgetExceededError", err)
	}
	// Delete the row behind the memo's back: the second check on
	// the same request context must still refuse (memoised row),
	// a fresh context must allow (no row = unlimited).
	if _, err := l.db.ExecContext(context.Background(), `DELETE FROM budgets WHERE key_id = ?`, key); err != nil {
		t.Fatalf("raw delete: %v", err)
	}
	if _, _, err := l.CheckBudgetOrError(memoCtx, key); !IsBudgetExceeded(err) {
		t.Fatalf("CheckBudgetOrError (memo, row deleted) = %v, want BudgetExceededError from the memo", err)
	}
	if _, _, err := l.CheckBudgetOrError(context.Background(), key); err != nil {
		t.Fatalf("CheckBudgetOrError (fresh ctx, row deleted) = %v, want nil (unlimited)", err)
	}
}

// A real database error is never memoised: drop the budgets
// table behind the memo's back (no read has run yet on this
// ledger, so no prepared statement is cached), read once to hit
// the error, recreate the table with a row, and read again on
// the SAME memo context — it must see the row, proving the
// failed read left nothing in the memo.
func TestBudgetMemoDoesNotCacheErrors(t *testing.T) {
	l := openTestLedger(t)
	memoCtx := WithBudgetMemo(context.Background())
	if _, err := l.db.ExecContext(context.Background(), `DROP TABLE budgets`); err != nil {
		t.Fatalf("drop budgets: %v", err)
	}
	if _, err := l.GetBudget(memoCtx, "memo-key"); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBudget (no table) = %v, want a real database error, not nil/ErrNoRows", err)
	}
	if _, err := l.db.ExecContext(context.Background(), `CREATE TABLE budgets (
		key_id   TEXT PRIMARY KEY,
		cap_usd  REAL NOT NULL,
		spent_usd REAL NOT NULL DEFAULT 0,
		period   TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("recreate budgets: %v", err)
	}
	if err := l.SetBudget(context.Background(), "memo-key", 7, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	b, err := l.GetBudget(memoCtx, "memo-key")
	if err != nil || b.CapUSD != 7 {
		t.Fatalf("memo read after error + recreate = %+v, err %v, want cap=7 (error not cached)", b, err)
	}
}

// The memoised read is a map lookup, not a SQLite query: it must
// be orders of magnitude cheaper than the database read it
// replaces (BenchmarkGetBudgetMiss in budget_hotpath_test.go is
// the uncached reference).
func BenchmarkGetBudgetMemoHit(b *testing.B) {
	l := openBenchLedger(b)
	ctx := WithBudgetMemo(context.Background())
	if err := l.SetBudget(context.Background(), "memo-bench", 20, "monthly"); err != nil {
		b.Fatalf("SetBudget: %v", err)
	}
	if _, err := l.GetBudget(ctx, "memo-bench"); err != nil {
		b.Fatalf("warm memo: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bd, err := l.GetBudget(ctx, "memo-bench")
		if err != nil || bd.CapUSD != 20 {
			b.Fatalf("GetBudget #%d = %+v, err %v, want cap=20", i, bd, err)
		}
	}
}

// Same, for the bench path's memoised miss (no budget row).
func BenchmarkGetBudgetMemoMissHit(b *testing.B) {
	l := openBenchLedger(b)
	ctx := WithBudgetMemo(context.Background())
	if _, err := l.GetBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		b.Fatalf("warm memo: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.GetBudget(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			b.Fatalf("GetBudget #%d = %v, want sql.ErrNoRows", i, err)
		}
	}
}
