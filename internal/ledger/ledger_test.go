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
		PromptTokens: 100, CompletionTokens: 50, OK: true, LatencyMS: 120,
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
		PromptTokens: 10, CompletionTokens: 10, OK: false, LatencyMS: 5,
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
		PromptTokens: 100000, CompletionTokens: 100000, OK: true,
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
		PromptTokens: 10, CompletionTokens: 0, OK: true,
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
		PromptTokens: 5, CompletionTokens: 0, OK: true,
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
		PromptTokens: 79, CompletionTokens: 0, OK: true,
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
		PromptTokens: 1, CompletionTokens: 0, OK: true,
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
		PromptTokens: 10, CompletionTokens: 0, OK: true,
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
