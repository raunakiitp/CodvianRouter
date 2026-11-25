package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// AUD-PERF-002 regression: the ledger keeps one WRITER connection but
// runs reads on a separate pool, so a budget check must NOT queue
// behind the writer. This holds a write transaction open on the writer
// handle — exactly what the async batch writer does for the duration
// of a batch commit — and asserts a GetBudget read still returns
// promptly. On the pre-fix code (a single connection for reads and
// writes) GetBudget checks out a connection, finds the only one held
// by the open transaction, and blocks until it is released; the read
// below would time out.
func TestBudgetReadNotBlockedByOpenWrite(t *testing.T) {
	ctx := context.Background()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if err := l.SetBudget(ctx, "key-1", 100, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}

	// Hold the single writer connection in an open transaction, as a
	// batch commit does. A *sql.Tx owns its connection for its whole
	// lifetime, so with one shared connection every read queues behind
	// this.
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE budgets SET spent_usd = spent_usd WHERE key_id = ?`, "key-1"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold write: %v", err)
	}

	type result struct {
		spent float64
		err   error
	}
	done := make(chan result, 1)
	go func() {
		b, err := l.GetBudget(ctx, "key-1")
		done <- result{b.SpentUSD, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			_ = tx.Rollback()
			t.Fatalf("GetBudget while a write txn is open: %v", r.err)
		}
		// Returned while the writer transaction is still open: reads
		// run on their own connections, off the writer.
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		// Let the (now-unblocked) read finish so no goroutine leaks
		// into the next test.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("GetBudget blocked behind an open write transaction: reads still share the single writer connection (AUD-PERF-002)")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// AUD-PERF-002 companion: the check path (CheckBudgetOrError) on the
// no-budget miss path is a pure read and must also return while the
// writer connection is held. This is the benchmark hot path
// (BenchmarkCheckBudgetOrErrorMiss).
func TestCheckBudgetOrErrorNotBlockedByOpenWrite(t *testing.T) {
	ctx := context.Background()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE budgets SET spent_usd = spent_usd`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := l.CheckBudgetOrError(ctx, "no-such-key")
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("CheckBudgetOrError while a write txn is open: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("CheckBudgetOrError blocked behind an open write transaction (AUD-PERF-002)")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
