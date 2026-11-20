package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// The per-request key-budget memo (CHECKLIST 279-280): a chat
// request hits the key-budget guard twice — pre-body admission
// and the post-decode check — and the 273-274 CPU profile put
// those two GetKeyBudget reads at 11.54% cum. With one memo per
// request the second read is a map lookup, not a second SQLite
// query. These tests prove the memo by mutating the database
// behind its back: a memoised read must NOT see the change, a
// fresh context must, and a write through the same context must
// invalidate the memo entry.
func TestKeyBudgetMemoSharesOneRead(t *testing.T) {
	l := openKeyBudgetLedger(t)

	// Miss memoised: the first read finds no row; a row set
	// through a DIFFERENT context (no memo to invalidate) must
	// stay invisible to this memo.
	memoCtx := WithKeyBudgetMemo(context.Background())
	if _, err := l.GetKeyBudget(memoCtx, "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("memo first read = %v, want sql.ErrNoRows", err)
	}
	if err := l.SetKeyBudget(context.Background(), "memo-key", 20, UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	if _, err := l.GetKeyBudget(memoCtx, "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("memo second read = %v, want sql.ErrNoRows (memoised miss)", err)
	}
	// A fresh context sees the row.
	b, err := l.GetKeyBudget(context.Background(), "memo-key")
	if err != nil || b.LimitUSD != 20 {
		t.Fatalf("fresh read = %+v, err %v, want limit=20", b, err)
	}

	// Row memoised: a fresh memo reads the row once, then a row
	// deleted behind its back (raw SQL, bypassing
	// DeleteKeyBudget's invalidation) stays visible to it.
	memoCtx2 := WithKeyBudgetMemo(context.Background())
	b, err = l.GetKeyBudget(memoCtx2, "memo-key")
	if err != nil || b.LimitUSD != 20 {
		t.Fatalf("memo2 first read = %+v, err %v, want limit=20", b, err)
	}
	if _, err := l.db.ExecContext(context.Background(), `DELETE FROM key_budgets WHERE key_id = ?`, "memo-key"); err != nil {
		t.Fatalf("raw delete: %v", err)
	}
	b, err = l.GetKeyBudget(memoCtx2, "memo-key")
	if err != nil || b.LimitUSD != 20 {
		t.Fatalf("memo2 read after raw delete = %+v, err %v, want memoised limit=20", b, err)
	}
	if _, err := l.GetKeyBudget(context.Background(), "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fresh read after raw delete = %v, want sql.ErrNoRows", err)
	}

	// A write through the memo context invalidates it: set then
	// read on the same context sees the new row; delete then read
	// sees it gone.
	if err := l.SetKeyBudget(memoCtx2, "memo-key", 5, UnpricedCountAsZero); err != nil {
		t.Fatalf("SetKeyBudget (memo ctx): %v", err)
	}
	b, err = l.GetKeyBudget(memoCtx2, "memo-key")
	if err != nil || b.LimitUSD != 5 || b.UnpricedPolicy != UnpricedCountAsZero {
		t.Fatalf("memo read after SetKeyBudget = %+v, err %v, want limit=5 count_as_zero", b, err)
	}
	if err := l.DeleteKeyBudget(memoCtx2, "memo-key"); err != nil {
		t.Fatalf("DeleteKeyBudget (memo ctx): %v", err)
	}
	if _, err := l.GetKeyBudget(memoCtx2, "memo-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("memo read after DeleteKeyBudget = %v, want sql.ErrNoRows", err)
	}
}

// CheckKeyBudget must not re-read the key_budgets row after the
// memo has it: with a cap-0 budget (spent 0 >= cap 0, so every
// request is over the cap) the guard must still refuse on the
// same memo context after the row is deleted behind its back,
// and must allow on a fresh context.
func TestCheckKeyBudgetUsesMemo(t *testing.T) {
	l := openKeyBudgetLedger(t)
	now := time.Now()
	key := "memo-check"
	priced := func(string) bool { return true }
	memoCtx := WithKeyBudgetMemo(context.Background())
	if err := l.SetKeyBudget(context.Background(), key, 0, UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	if err := l.CheckKeyBudget(memoCtx, key, []string{"m"}, priced, now); !IsBudgetExceeded(err) {
		t.Fatalf("CheckKeyBudget (memo, cap 0) = %v, want BudgetExceededError", err)
	}
	// Delete the row behind the memo's back: the second check on
	// the same request context must still refuse (memoised row),
	// a fresh context must allow (no row = unlimited).
	if _, err := l.db.ExecContext(context.Background(), `DELETE FROM key_budgets WHERE key_id = ?`, key); err != nil {
		t.Fatalf("raw delete: %v", err)
	}
	if err := l.CheckKeyBudget(memoCtx, key, []string{"m"}, priced, now); !IsBudgetExceeded(err) {
		t.Fatalf("CheckKeyBudget (memo, row deleted) = %v, want BudgetExceededError from the memo", err)
	}
	if err := l.CheckKeyBudget(context.Background(), key, []string{"m"}, priced, now); err != nil {
		t.Fatalf("CheckKeyBudget (fresh ctx, row deleted) = %v, want nil (unlimited)", err)
	}
}

// The memoised read is a map lookup, not a SQLite query: it must
// be orders of magnitude cheaper than the database read it
// replaces (BenchmarkGetKeyBudgetMiss in budget_hotpath_test.go
// is the uncached reference).
func BenchmarkGetKeyBudgetMemoHit(b *testing.B) {
	l, err := Open(b.TempDir() + "/ledger.db")
	if err != nil {
		b.Fatalf("open test ledger: %v", err)
	}
	b.Cleanup(func() { l.Close() })
	ctx := WithKeyBudgetMemo(context.Background())
	if err := l.SetKeyBudget(context.Background(), "memo-bench", 20, UnpricedFailClosed); err != nil {
		b.Fatalf("SetKeyBudget: %v", err)
	}
	if _, err := l.GetKeyBudget(ctx, "memo-bench"); err != nil {
		b.Fatalf("warm memo: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kb, err := l.GetKeyBudget(ctx, "memo-bench")
		if err != nil || kb.LimitUSD != 20 {
			b.Fatalf("GetKeyBudget #%d = %+v, err %v, want limit=20", i, kb, err)
		}
	}
}
