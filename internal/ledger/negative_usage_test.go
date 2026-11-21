package ledger

// P1-09 security review: a malformed or hostile upstream usage
// report can carry NEGATIVE parent token counts. Subsets were
// already clamped; the parents must be too — a negative prompt or
// completion would price as negative spend and corrupt budgets
// and analytics. Valid reports are unchanged.

import (
	"context"
	"testing"
)

func TestRecordUsageNegativeParentsClamped(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Negative prompt/completion/total with a known split: all
	// clamp to 0; the derived total is 0; cost is $0, never
	// negative spend.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: -100, CompletionTokens: -5, TotalTokens: -105,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Negative total only: parents stay, total derives from them.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: -1,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 2: %v", err)
	}
	// A valid report is untouched.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 3: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	// Newest first.
	valid, negTotal, negAll := events[0], events[1], events[2]
	if negAll.PromptTokens != 0 || negAll.CompletionTokens != 0 || negAll.TotalTokens != 0 {
		t.Errorf("negative parents stored as %+v, want all 0", negAll)
	}
	if negAll.CostUSD < 0 {
		t.Errorf("negative-parent cost = %v, want never negative", negAll.CostUSD)
	}
	if negTotal.PromptTokens != 100 || negTotal.CompletionTokens != 10 || negTotal.TotalTokens != 110 {
		t.Errorf("negative total row = %+v, want parents kept + total derived 110", negTotal)
	}
	if valid.PromptTokens != 100 || valid.CompletionTokens != 10 || valid.TotalTokens != 110 {
		t.Errorf("valid report changed: %+v", valid)
	}
}
