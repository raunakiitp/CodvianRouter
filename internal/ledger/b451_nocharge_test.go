// B4-51 own-cache tokens_saved accounting (parity tracker:1115): a
// NoCharge ledger row — a proactive own-cache hit, a request that
// made no upstream call at all — must cost $0 by construction, and
// that $0 must be known (priced), not unknown. normalizeUsage
// forces it: it zeroes any hand-computed or reported cost and sets
// priceKnown. These tests exercise the real Ledger (Open +
// RecordUsage + RecentEvents) with prices configured for the model,
// so the $0 is proven forced — the same usage without NoCharge
// prices above $0.
package ledger

import (
	"context"
	"testing"
)

func b451ptrf(v float64) *float64 { return &v }

// b451Prices prices gpt-4o at $0.00001/prompt token and
// $0.00002/completion token, so 100 prompt + 50 completion tokens
// price at $0.002 when charged normally.
var b451Prices = Prices{
	PriceKey{Provider: "openai", Model: "gpt-4o"}: ModelPrice{PromptPerToken: 0.00001, CompletionPerToken: 0.00002},
}

// TestB451NoChargeRowCostsZero: the same usage prices above $0 as a
// normal row, but $0 as a NoCharge row — and the stored row says so
// (cost 0, saving kept verbatim, split known, no cost delta).
func TestB451NoChargeRowCostsZero(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Control: the same usage as a normal (non-cache) row prices
	// above $0 — proving the model is priced, so the NoCharge $0
	// below is forced, not "no price configured".
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		CacheTokensSaved: 0, SplitKnown: true, OK: true, LatencyMS: 100,
	}, b451Prices)
	if err != nil {
		t.Fatalf("RecordUsage (control): %v", err)
	}
	if cost <= 0 {
		t.Fatalf("control cost = %v, want > 0 (the model is priced)", cost)
	}

	// The NoCharge row: identical usage, plus the saving and the
	// flag — cost must be exactly $0.
	cost, err = l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		CacheTokensSaved: 150, SplitKnown: true, OK: true, LatencyMS: 90,
		NoCharge: true,
	}, b451Prices)
	if err != nil {
		t.Fatalf("RecordUsage (NoCharge): %v", err)
	}
	if cost != 0 {
		t.Fatalf("NoCharge cost = %v, want 0 (no upstream call happened)", cost)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	// Newest first: the NoCharge row, then the control row.
	noCharge, control := events[0], events[1]
	if noCharge.CostUSD != 0 {
		t.Errorf("stored NoCharge cost = %v, want 0", noCharge.CostUSD)
	}
	if noCharge.CacheTokensSaved != 150 {
		t.Errorf("stored NoCharge CacheTokensSaved = %d, want 150", noCharge.CacheTokensSaved)
	}
	if !noCharge.SplitKnown {
		t.Error("stored NoCharge SplitKnown = false, want true")
	}
	if noCharge.CostDeltaUSD != 0 {
		t.Errorf("stored NoCharge CostDeltaUSD = %v, want 0", noCharge.CostDeltaUSD)
	}
	if noCharge.ReportedCostUSD != nil {
		t.Errorf("stored NoCharge ReportedCostUSD = %v, want nil (nothing reported)", *noCharge.ReportedCostUSD)
	}
	if control.CostUSD <= 0 {
		t.Errorf("stored control cost = %v, want > 0", control.CostUSD)
	}
	if control.CacheTokensSaved != 0 {
		t.Errorf("stored control CacheTokensSaved = %d, want 0 (not a cache hit)", control.CacheTokensSaved)
	}
}

// TestB451NoChargeIgnoresReportedCost: a reported cost on a NoCharge
// row is contradictory — no upstream call happened, so nothing could
// have reported a cost — and normalizeUsage ignores it: the stored
// row costs $0 with no reported cost and no delta.
func TestB451NoChargeIgnoresReportedCost(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		CacheTokensSaved: 150, SplitKnown: true, OK: true, LatencyMS: 90,
		NoCharge: true, Cost: b451ptrf(0.5),
	}, b451Prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0 {
		t.Fatalf("NoCharge cost = %v, want 0 (a reported cost on a no-call row is ignored)", cost)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e.CostUSD != 0 {
		t.Errorf("stored cost = %v, want 0", e.CostUSD)
	}
	if e.ReportedCostUSD != nil {
		t.Errorf("stored ReportedCostUSD = %v, want nil (reported cost ignored on a NoCharge row)", *e.ReportedCostUSD)
	}
	if e.CostDeltaUSD != 0 {
		t.Errorf("stored CostDeltaUSD = %v, want 0", e.CostDeltaUSD)
	}
	if e.CacheTokensSaved != 150 {
		t.Errorf("stored CacheTokensSaved = %d, want 150", e.CacheTokensSaved)
	}
}
