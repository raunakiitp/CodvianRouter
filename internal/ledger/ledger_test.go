package ledger

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRecordUsageAndSpendFor(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		"model-a": {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}

	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 120,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// 100*0.001 + 50*0.002 = 0.20
	if cost != 0.20 {
		t.Fatalf("cost = %v, want 0.20", cost)
	}

	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: false, LatencyMS: 5,
	}, prices); err != nil {
		t.Fatalf("RecordUsage (failed call): %v", err)
	}

	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	// 0.20 + (10*0.001 + 10*0.002 = 0.03) = 0.23. Failed calls still
	// cost tokens and are still recorded.
	if spent != 0.23 {
		t.Fatalf("spent = %v, want 0.23", spent)
	}

	// Another key is isolated.
	spent, err = l.SpendFor(ctx, "key-2", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor key-2: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spent key-2 = %v, want 0", spent)
	}

	// since filters old events.
	spent, err = l.SpendFor(ctx, "key-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SpendFor future since: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spent since future = %v, want 0", spent)
	}
}

func TestRecordUsageUnknownModelCostsZero(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	// No price for "mystery-model": cost must be 0, recorded as 0 —
	// never an invented price.
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "mystery-model",
		PromptTokens: 100000, CompletionTokens: 100000, SplitKnown: true, OK: true,
	}, Prices{"other-model": {PromptPerToken: 1, CompletionPerToken: 1}})
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0 {
		t.Fatalf("unknown model cost = %v, want 0", cost)
	}
	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spent = %v, want 0", spent)
	}
}

func TestCheckBudgetNoRowIsUnlimited(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	allowed, remaining, err := l.CheckBudget(ctx, "no-budget-key")
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if !allowed {
		t.Fatalf("allowed = false, want true for key with no budget")
	}
	if !math.IsInf(remaining, 1) {
		t.Fatalf("remaining = %v, want +Inf", remaining)
	}
}

func TestCheckBudgetSoftCap(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	if err := l.SetBudget(ctx, "key-1", 1.00, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	// FailClosed off (default): exhausted budget still allows, but
	// remaining goes <= 0.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 10, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.10}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	allowed, remaining, err := l.CheckBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if !allowed {
		t.Fatalf("soft cap: allowed = false, want true")
	}
	if remaining != 0 {
		t.Fatalf("remaining = %v, want 0 (spent 1.00 of 1.00)", remaining)
	}
}

func TestCheckBudgetFailClosed(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	l.FailClosed = true
	if err := l.SetBudget(ctx, "key-1", 0.50, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 5, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.10}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	allowed, remaining, err := l.CheckBudgetOrError(ctx, "key-1")
	_ = allowed // CheckBudgetOrError reports via err, not allowed
	if err == nil {
		t.Fatalf("CheckBudgetOrError: want BudgetExceededError, got nil (remaining=%v)", remaining)
	}
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("err type = %T, want *BudgetExceededError", err)
	}
	if !IsBudgetExceeded(err) {
		t.Fatalf("IsBudgetExceeded = false, want true")
	}
	if be.KeyID != "key-1" || be.CapUSD != 0.50 || be.SpentUSD != 0.50 {
		t.Fatalf("BudgetExceededError = %+v, want key-1 cap 0.50 spent 0.50", be)
	}
}

func TestCheckBudgetFailClosedCapZero(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	l.FailClosed = true
	// Cap of 0: any spend would exceed it, so nothing is allowed —
	// even before any usage is recorded.
	if err := l.SetBudget(ctx, "key-1", 0, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	allowed, _, err := l.CheckBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if allowed {
		t.Fatalf("cap 0 + FailClosed: allowed = true, want false")
	}
	_, _, err = l.CheckBudgetOrError(ctx, "key-1")
	if !IsBudgetExceeded(err) {
		t.Fatalf("CheckBudgetOrError: want BudgetExceededError, got %v", err)
	}
}

func TestSoftWarningAtEightyPercent(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	if err := l.SetBudget(ctx, "key-1", 1.00, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	// 0.79 spent: no warning yet.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 79, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.01}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	_, warn, err := l.CheckBudgetOrError(ctx, "key-1")
	if err != nil {
		t.Fatalf("CheckBudgetOrError: %v", err)
	}
	if warn {
		t.Fatalf("warn = true at 79%% of cap, want false")
	}
	// +0.01 -> 0.80 spent: warning fires.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 1, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.01}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	_, warn, err = l.CheckBudgetOrError(ctx, "key-1")
	if err != nil {
		t.Fatalf("CheckBudgetOrError: %v", err)
	}
	if !warn {
		t.Fatalf("warn = false at 80%% of cap, want true")
	}
}

func TestSetBudgetPreservesSpent(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	if err := l.SetBudget(ctx, "key-1", 1.00, "monthly"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 10, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.10}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Raising the cap must not wipe the spend.
	if err := l.SetBudget(ctx, "key-1", 5.00, "monthly"); err != nil {
		t.Fatalf("SetBudget raise: %v", err)
	}
	b, err := l.GetBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if b.CapUSD != 5.00 || b.SpentUSD != 1.00 {
		t.Fatalf("budget = %+v, want cap 5.00 spent 1.00", b)
	}
	if err := l.ResetSpent(ctx, "key-1"); err != nil {
		t.Fatalf("ResetSpent: %v", err)
	}
	b, err = l.GetBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("GetBudget after reset: %v", err)
	}
	if b.SpentUSD != 0 {
		t.Fatalf("spent after reset = %v, want 0", b.SpentUSD)
	}
}

func TestGetBudgetMissingKey(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	_, err := l.GetBudget(ctx, "nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBudget missing key: err = %v, want sql.ErrNoRows", err)
	}
}

// readUsageRow reads one stored usage_events row back for assertions.
func readUsageRow(t *testing.T, l *Ledger, keyID string) (prompt, completion, total, splitKnown int, cost float64) {
	t.Helper()
	err := l.db.QueryRowContext(context.Background(), `
		SELECT prompt_tokens, completion_tokens, total_tokens,
		       tokens_split_known, cost_usd
		FROM usage_events WHERE gateway_key_id = ?`, keyID,
	).Scan(&prompt, &completion, &total, &splitKnown, &cost)
	if err != nil {
		t.Fatalf("read usage row: %v", err)
	}
	return prompt, completion, total, splitKnown, cost
}

func TestRecordUsageSplitKnownCostDifferentPrices(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	// Prompt and completion prices deliberately differ, so a cost
	// computed from a collapsed total would be wrong.
	prices := Prices{"model-a": {PromptPerToken: 0.001, CompletionPerToken: 0.01}}
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// 100*0.001 + 50*0.01 = 0.60
	if cost != 0.60 {
		t.Fatalf("cost = %v, want 0.60", cost)
	}
	prompt, completion, total, splitKnown, storedCost := readUsageRow(t, l, "key-1")
	if prompt != 100 || completion != 50 || total != 150 || splitKnown != 1 || storedCost != 0.60 {
		t.Fatalf("stored row = prompt %d completion %d total %d split_known %d cost %v, want 100/50/150/1/0.60",
			prompt, completion, total, splitKnown, storedCost)
	}

	// An explicitly supplied TotalTokens is stored as given.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage (explicit total): %v", err)
	}
	if _, _, total, _, _ := readUsageRow(t, l, "key-2"); total != 15 {
		t.Fatalf("explicit total = %d, want 15", total)
	}
}

func TestRecordUsageUnknownSplitCostsZero(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{"model-a": {PromptPerToken: 0.001, CompletionPerToken: 0.01}}
	// Even with prices configured and split-looking numbers present,
	// an unknown split must never be priced: the split fields are
	// ignored, the total is preserved, and cost is 0.
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 400, TotalTokens: 500,
		SplitKnown: false, OK: true,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0 {
		t.Fatalf("unknown-split cost = %v, want 0", cost)
	}
	prompt, completion, total, splitKnown, storedCost := readUsageRow(t, l, "key-1")
	if prompt != 0 || completion != 0 || total != 500 || splitKnown != 0 || storedCost != 0 {
		t.Fatalf("stored row = prompt %d completion %d total %d split_known %d cost %v, want 0/0/500/0/0",
			prompt, completion, total, splitKnown, storedCost)
	}
	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spent = %v, want 0 (unknown split is never billed)", spent)
	}
}

// usageColumns returns the column names of usage_events in l's database.
func usageColumns(t *testing.T, l *Ledger) map[string]bool {
	t.Helper()
	rows, err := l.db.Query(`PRAGMA table_info(usage_events)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read table_info: %v", err)
	}
	return cols
}

func TestOpenMigratesOldDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-ledger.db")
	// Build a database with the pre-migration schema (no total_tokens /
	// tokens_split_known columns), exactly as an older binary left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	oldSchema := `CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  gateway_key_id    TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE budgets (
  key_id   TEXT PRIMARY KEY,
  cap_usd  REAL NOT NULL,
  spent_usd REAL NOT NULL DEFAULT 0,
  period   TEXT NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	cols := usageColumns(t, l)
	if !cols["total_tokens"] || !cols["tokens_split_known"] {
		l.Close()
		t.Fatalf("migrated columns = %v, want total_tokens + tokens_split_known", cols)
	}
	// RecordUsage works against the migrated database, both
	// split-known and unknown-split.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true,
	}, Prices{"m": {PromptPerToken: 0.01, CompletionPerToken: 0.02}}); err != nil {
		l.Close()
		t.Fatalf("RecordUsage after migration: %v", err)
	}
	if _, _, total, splitKnown, _ := readUsageRow(t, l, "key-1"); total != 15 || splitKnown != 1 {
		l.Close()
		t.Fatalf("migrated row total = %d split_known = %d, want 15/1", total, splitKnown)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}

	// Opening again must be a no-op (idempotent migration).
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open migrated db: %v", err)
	}
	defer l2.Close()
	cols = usageColumns(t, l2)
	if !cols["total_tokens"] || !cols["tokens_split_known"] {
		t.Fatalf("columns after re-open = %v", cols)
	}
}

// TestRecordShared: a shared (coalesced) row is marked shared, carries
// zero tokens and zero cost even when usage is passed in, and never
// moves spend — the leader's row is the only one billed.
func TestRecordShared(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{"model-a": {PromptPerToken: 0.001, CompletionPerToken: 0.002}}

	// The leader's real attempt.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 120,
	}, prices); err != nil {
		t.Fatalf("RecordUsage (leader): %v", err)
	}
	// Two waiters served from the leader's call.
	for i := 0; i < 2; i++ {
		cost, err := l.RecordShared(ctx, "p1", "model-a", true, 90)
		if err != nil {
			t.Fatalf("RecordShared: %v", err)
		}
		if cost != 0 {
			t.Fatalf("RecordShared cost = %v, want 0", cost)
		}
	}
	// Spend is the leader's cost only: 100*0.001 + 50*0.002 = 0.20.
	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spent != 0.20 {
		t.Fatalf("spent = %v, want 0.20 (shared rows must not add spend)", spent)
	}
	// Rows: 1 leader (shared=0) + 2 waiters (shared=1, zero tokens/cost).
	rows, err := l.db.QueryContext(ctx, `
		SELECT shared, prompt_tokens, completion_tokens, total_tokens,
		       tokens_split_known, cost_usd, ok, latency_ms
		FROM usage_events ORDER BY id`)
	if err != nil {
		t.Fatalf("query usage_events: %v", err)
	}
	defer rows.Close()
	type row struct {
		shared, prompt, completion, total, splitKnown int
		cost                                          float64
		ok                                            int
		latency                                       int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.shared, &r.prompt, &r.completion, &r.total, &r.splitKnown, &r.cost, &r.ok, &r.latency); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	if got[0].shared != 0 || got[0].total != 150 || got[0].cost != 0.20 {
		t.Fatalf("leader row = %+v, want shared=0 total=150 cost=0.20", got[0])
	}
	for i, r := range got[1:] {
		if r.shared != 1 || r.prompt != 0 || r.completion != 0 || r.total != 0 || r.splitKnown != 0 || r.cost != 0 || r.ok != 1 || r.latency != 90 {
			t.Fatalf("shared row %d = %+v, want shared=1 zero tokens/cost ok=1 latency=90", i, r)
		}
	}
}

// TestRecordUsageSharedFlagForcedZero: even RecordUsage with Shared
// set and a real split records zero tokens and zero cost.
func TestRecordUsageSharedFlagForcedZero(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{"model-a": {PromptPerToken: 0.01, CompletionPerToken: 0.02}}
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, Shared: true,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0 {
		t.Fatalf("cost = %v, want 0 for a shared row", cost)
	}
	prompt, completion, total, splitKnown, rowCost := readUsageRow(t, l, "key-1")
	if prompt != 0 || completion != 0 || total != 0 || splitKnown != 0 || rowCost != 0 {
		t.Fatalf("row = %d/%d/%d split=%d cost=%v, want all zero", prompt, completion, total, splitKnown, rowCost)
	}
}
