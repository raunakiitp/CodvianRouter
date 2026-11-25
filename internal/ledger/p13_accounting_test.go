// Package ledger tests the pre-P2 accounting fixes:
//
// NEW-M-02: the async writer's usage rows and the per-key budget
// delta must be ONE accounting fact. A budget UPDATE that fails
// must not leave the usage rows committed without the matching
// spend update (that is how a key's cap silently drifts below its
// real spend and the key can overspend).
//
// 7D: the drift population must be the ACTUAL provider-reported
// usage. A row whose recorded prompt count is only the conservative
// relay estimate echoed back compares the estimator with itself
// (always 0% drift) and dilutes the real population toward
// "perfect". Those rows are counted separately, never averaged in.
package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestAsyncBudgetFailureRollsBackUsage proves the NEW-M-02
// invariant: when the per-key budget UPDATE cannot be applied, the
// usage rows in that batch must NOT land. The failure is injected
// by dropping the budgets table's row target - the UPDATE then
// affects no row and the writer must treat it as failed rather than
// committing usage alone.
//
// The observable contract is simpler and stronger than the
// mechanism: after a failed batch, the committed usage rows and the
// committed budget delta must agree. Either both landed or neither
// did.
func TestAsyncBudgetFailureRollsBackUsage(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}

	// First: a key WITH a budget row - the healthy path.
	if _, err := l.db.ExecContext(ctx,
		`INSERT INTO budgets (key_id, cap_usd, spent_usd) VALUES (?, 100, 0)`, "key-1"); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	w := NewAsyncWriter(l, prices, AsyncWriterConfig{QueueCap: 64, FlushInterval: 10 * time.Millisecond})
	w.Record(Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w.Close()

	rows := countUsageRows(t, l)
	if rows == 0 {
		t.Fatal("no usage rows committed; the healthy path must commit usage")
	}
	spent := budgetSpent(t, l, "key-1")
	if spent <= 0 {
		t.Fatalf("budget spent = %v; want > 0 (usage and spend must move together)", spent)
	}

	// NEW-M-02: an SQL ERROR during the budget UPDATE must roll
	// back the WHOLE batch (usage rows AND spend delta). We inject
	// the error by dropping the budgets table - the prepared UPDATE
	// statement then fails with "no such table".
	// The observable contract: after a failed batch, the committed
	// usage rows and the committed budget delta must agree. Either
	// both landed or neither did.
	if _, err := l.db.ExecContext(ctx, `DROP TABLE budgets`); err != nil {
		t.Fatalf("drop budgets: %v", err)
	}

	before := countUsageRows(t, l)
	w2 := NewAsyncWriter(l, prices, AsyncWriterConfig{QueueCap: 64, FlushInterval: 10 * time.Millisecond})
	w2.Record(Usage{
		GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w2.Close()

	after := countUsageRows(t, l)
	if after != before {
		t.Errorf("usage rows = %d, want %d: a failed budget update (SQL error) must roll the usage rows back too (NEW-M-02)", after, before)
	}
	if st := w2.Stats(); st.FailedRows == 0 {
		t.Error("FailedRows = 0; a failed budget update must be surfaced, not swallowed")
	}
}

// TestDriftExcludesEstimateBackedRows proves the 7D invariant: the
// drift figures are computed over actual provider-reported usage
// only, and estimate-backed rows are reported as a separate count
// instead of diluting the population with a fake 0% error.
func TestDriftExcludesEstimateBackedRows(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Three ACTUAL rows: provider reported 100 prompt tokens,
	// local estimate was 80, 90, 110 (real errors: -20%, -10%, +10%).
	// Only the row where local_estimate == prompt_tokens is NOT actual.
	for _, est := range []int{80, 90, 110} {
		if _, err := l.db.ExecContext(ctx, `
			INSERT INTO usage_events
			  (ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens, total_tokens,
			   local_estimate_prompt_tokens, tokens_split_known, shared, ok)
			VALUES (?, 'key-1', 'openai', 'model-a', 100, 10, 110, ?, 1, 0, 1)`,
			time.Now().UnixMilli(), est); err != nil {
			t.Fatalf("seed actual usage: %v", err)
		}
	}
	// Three ESTIMATE-BACKED rows: recorded prompt count EQUALS
	// the local estimate, so they carry no provider measurement.
	for range 3 {
		if _, err := l.db.ExecContext(ctx, `
			INSERT INTO usage_events
			  (ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens, total_tokens,
			   local_estimate_prompt_tokens, tokens_split_known, shared, ok)
			VALUES (?, 'key-1', 'openai', 'model-a', 90, 10, 100, 90, 1, 0, 1)`,
			time.Now().UnixMilli()); err != nil {
			t.Fatalf("seed estimate-backed usage: %v", err)
		}
	}

	rows, err := l.DriftSummary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("DriftSummary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	d := rows[0]
	// We inserted 3 actual rows (local_estimate 80, 90, 110 vs prompt 100)
	// and 3 estimate-backed (local_estimate=prompt=90). So:
	if d.Compared != 3 {
		t.Errorf("Compared = %d, want 3 (the three actual provider-reported rows)", d.Compared)
	}
	if d.EstimateBacked != 3 {
		t.Errorf("EstimateBacked = %d, want 3 (the three estimate-backed rows counted separately)", d.EstimateBacked)
	}
	// MeanErrPct over actual rows: (-20% + -10% + +10%) / 3 = -20/3 = -6.666...
	if d.MeanErrPct < -6.67 || d.MeanErrPct > -6.66 {
		t.Errorf("MeanErrPct = %.4f, want -6.66... (drift over the actual rows only)", d.MeanErrPct)
	}
}

// countUsageRows counts the committed usage rows.
func countUsageRows(t *testing.T, l *Ledger) int {
	t.Helper()
	var n int
	if err := l.rdb.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	return n
}

// budgetSpent reads one key's committed budget delta.
func budgetSpent(t *testing.T, l *Ledger, keyID string) float64 {
	t.Helper()
	var spent float64
	err := l.rdb.QueryRow(`SELECT spent_usd FROM budgets WHERE key_id = ?`, keyID).Scan(&spent)
	if err != nil {
		return 0
	}
	return spent
}

// unused import guard: filepath is used by openTestLedger in the
// package's other test files, but this file references it too so
// the import stays meaningful if the helper moves.
var _ = filepath.Join
