package ledger

import (
	"context"
	"math"
	"testing"
	"time"
)

// B12-LEDGER: Summary.ByKey must split per (gateway_key_id,
// provider) — one key routing to two providers must yield two
// rows with per-provider totals, never one merged row.
func TestSummaryByKeySplitsByProvider(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
		{Model: "model-b"}: {PromptPerToken: 0.01, CompletionPerToken: 0.02},
	}
	// key-1 -> p1/model-a: 100*0.001 + 50*0.002 = 0.20
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage p1: %v", err)
	}
	// key-1 -> p2/model-b: 10*0.01 + 10*0.02 = 0.30
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p2", Model: "model-b",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage p2: %v", err)
	}
	// key-2 -> p1/model-a: separate key, must stay separate.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 7, CompletionTokens: 3, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage key-2: %v", err)
	}

	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	// Three (key, provider) pairs -> three rows. Before the fix
	// the key roll-up grouped by gateway_key_id only, so key-1
	// merged into one row (2 rows total, cost 0.50 on key-1).
	if len(sum.ByKey) != 3 {
		t.Fatalf("len(ByKey) = %d, want 3 (one row per key,provider): %+v", len(sum.ByKey), sum.ByKey)
	}
	byPair := map[string]KeyUsage{}
	for _, k := range sum.ByKey {
		byPair[k.GatewayKeyID+"/"+k.Provider] = k
	}
	p1 := byPair["key-1/p1"]
	if p1.Requests != 1 || p1.PromptTokens != 100 || math.Abs(p1.CostUSD-0.20) > 1e-9 {
		t.Fatalf("key-1/p1 = %+v, want requests=1 prompt=100 cost=0.20", p1)
	}
	p2 := byPair["key-1/p2"]
	if p2.Requests != 1 || p2.PromptTokens != 10 || math.Abs(p2.CostUSD-0.30) > 1e-9 {
		t.Fatalf("key-1/p2 = %+v, want requests=1 prompt=10 cost=0.30", p2)
	}
	k2 := byPair["key-2/p1"]
	if k2.Requests != 1 || k2.PromptTokens != 7 {
		t.Fatalf("key-2/p1 = %+v, want requests=1 prompt=7", k2)
	}
}
