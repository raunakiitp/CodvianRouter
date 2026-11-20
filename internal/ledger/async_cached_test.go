package ledger

import (
	"context"
	"testing"
	"time"
)

// TestAsyncWriterCarriesCachedTokens is the regression test for the
// bug the 131-132 live smoke caught: commitBatch called
// normalizeUsage (which computes cachedTokens) but its INSERT never
// listed the cached_tokens column, so every row the production
// async path wrote lost its provider-reported cached tokens — the
// synchronous RecordUsage path kept them, so unit tests using it
// passed while the live binary recorded 0. The batch path must carry
// the same column as the sync path.
func TestAsyncWriterCarriesCachedTokens(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()
	w.Record(Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010,
		CachedTokens: 800, SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w.Record(Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 200, CompletionTokens: 5, TotalTokens: 205,
		CachedTokens: 0, SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w.Flush()
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	if events[1].CachedTokens != 800 || events[1].PromptTokens != 1000 {
		t.Fatalf("batch row = %+v, want cached 800 prompt 1000", events[1])
	}
	if events[0].CachedTokens != 0 {
		t.Fatalf("batch row = %+v, want cached 0", events[0])
	}
	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CachedTokens != 800 || sum.RequestsWithCachedTokens != 1 {
		t.Fatalf("summary cached = %d with-cached = %d, want 800 / 1", sum.CachedTokens, sum.RequestsWithCachedTokens)
	}
}
