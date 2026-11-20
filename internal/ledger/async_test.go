package ledger

import (
	"context"
	"testing"
	"time"
)

func TestAsyncWriterRecordsRows(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	w := NewAsyncWriter(l, prices, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()
	for i := 0; i < 300; i++ {
		w.Record(Usage{
			GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
			PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10,
		})
	}
	w.Flush()
	events, err := l.RecentEvents(ctx, 500, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 300 {
		t.Fatalf("rows = %d, want 300", len(events))
	}
	for _, e := range events {
		if e.TotalTokens != 150 || e.CostUSD != 0.20 {
			t.Fatalf("row = total %d cost %v, want 150 / 0.20", e.TotalTokens, e.CostUSD)
		}
	}
	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spent != 300*0.20 {
		t.Fatalf("spent = %v, want %v", spent, 300*0.20)
	}
	if st := w.Stats(); st.CommittedRows != 300 || st.FailedRows != 0 {
		t.Fatalf("stats = %+v, want 300 committed, 0 failed", st)
	}
}

// TestAsyncWriterAggregatesBudgetUpdates pins the 2026-11-18
// commitBatch change: one batch's spend is applied with one UPDATE
// per gateway key, not one per row. Two keys with different costs
// in a single batch must each land exactly their own sum in the
// budgets table — no mixing, no dropped key — and every row counts
// as committed.
func TestAsyncWriterAggregatesBudgetUpdates(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	if err := l.SetBudget(ctx, "key-1", 100, "test"); err != nil {
		t.Fatalf("SetBudget key-1: %v", err)
	}
	if err := l.SetBudget(ctx, "key-2", 100, "test"); err != nil {
		t.Fatalf("SetBudget key-2: %v", err)
	}
	w := NewAsyncWriter(l, prices, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()
	// key-1: 10 rows x $0.20 = $2.00; key-2: 5 rows x $0.05 = $0.25.
	for i := 0; i < 10; i++ {
		w.Record(Usage{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
			PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10})
	}
	for i := 0; i < 5; i++ {
		w.Record(Usage{GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
			PromptTokens: 50, CompletionTokens: 0, SplitKnown: true, OK: true, LatencyMS: 10})
	}
	w.Flush()
	if st := w.Stats(); st.CommittedRows != 15 || st.FailedRows != 0 {
		t.Fatalf("stats = %+v, want 15 committed, 0 failed", st)
	}
	b1, err := l.GetBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("GetBudget key-1: %v", err)
	}
	// Epsilon: float64 addition is not associative, so summing
	// ten 0.20 costs in Go (1.9999999999999998) differs in the
	// last bit from adding them one at a time in SQLite.
	if d := b1.SpentUSD - 2.00; d > 1e-9 || d < -1e-9 {
		t.Fatalf("key-1 spent = %v, want 2.00", b1.SpentUSD)
	}
	b2, err := l.GetBudget(ctx, "key-2")
	if err != nil {
		t.Fatalf("GetBudget key-2: %v", err)
	}
	if d := b2.SpentUSD - 0.25; d > 1e-9 || d < -1e-9 {
		t.Fatalf("key-2 spent = %v, want 0.25", b2.SpentUSD)
	}
}

// TestAsyncWriterMultiRowChunkBoundaries pins the 2026-11-18
// commitBatch change (CHECKLIST 287): one multi-row INSERT per
// 32-row chunk instead of one INSERT per row. A 70-row batch spans
// three chunks (32 + 32 + 6) and two gateway keys, so a boundary
// bug would drop or misprice rows at the chunk edges. Every row
// must land with its exact cost, each key's budget must hold
// exactly its own sum, and the stats must count all 70 rows
// committed with none failed.
func TestAsyncWriterMultiRowChunkBoundaries(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	if err := l.SetBudget(ctx, "key-1", 1e9, "test"); err != nil {
		t.Fatalf("SetBudget key-1: %v", err)
	}
	if err := l.SetBudget(ctx, "key-2", 1e9, "test"); err != nil {
		t.Fatalf("SetBudget key-2: %v", err)
	}
	w := NewAsyncWriter(l, prices, AsyncWriterConfig{BatchSize: 256, FlushInterval: time.Hour})
	defer w.Close()
	// 70 rows: key-1 rows 0-39 (40 x $0.20 = $8.00), key-2 rows
	// 40-69 (30 x $0.05 = $1.50). The key switch at row 40 sits
	// inside the second chunk, so a chunk-edge bug cannot hide
	// behind a key boundary.
	for i := 0; i < 70; i++ {
		key, prompt, completion := "key-1", 100, 50
		if i >= 40 {
			key, prompt, completion = "key-2", 50, 0
		}
		w.Record(Usage{GatewayKeyID: key, Provider: "p1", Model: "model-a",
			PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: true, LatencyMS: 10})
	}
	w.Flush()
	if st := w.Stats(); st.CommittedRows != 70 || st.FailedRows != 0 {
		t.Fatalf("stats = %+v, want 70 committed, 0 failed", st)
	}
	events, err := l.RecentEvents(ctx, 100, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 70 {
		t.Fatalf("rows = %d, want 70", len(events))
	}
	b1, err := l.GetBudget(ctx, "key-1")
	if err != nil {
		t.Fatalf("GetBudget key-1: %v", err)
	}
	if d := b1.SpentUSD - 8.00; d > 1e-9 || d < -1e-9 {
		t.Fatalf("key-1 spent = %v, want 8.00", b1.SpentUSD)
	}
	b2, err := l.GetBudget(ctx, "key-2")
	if err != nil {
		t.Fatalf("GetBudget key-2: %v", err)
	}
	if d := b2.SpentUSD - 1.50; d > 1e-9 || d < -1e-9 {
		t.Fatalf("key-2 spent = %v, want 1.50", b2.SpentUSD)
	}
}

func TestAsyncWriterSharedRows(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{FlushInterval: time.Hour})
	defer w.Close()
	w.RecordShared("p1", "model-a", true, 42)
	w.Flush()
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("rows = %d, want 1", len(events))
	}
	if !events[0].Shared || events[0].TotalTokens != 0 || events[0].CostUSD != 0 {
		t.Fatalf("shared row = %+v, want shared with zero tokens/cost", events[0])
	}
}

func TestAsyncWriterSyncFallbackOnFullQueue(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	// Queue of 2, a batch that never fills and a flush interval of
	// an hour: the writer cannot drain, so rows past the queue must
	// fall back to synchronous writes instead of being dropped.
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{QueueCap: 2, BatchSize: 1 << 20, FlushInterval: time.Hour})
	defer w.Close()
	const n = 50
	for i := 0; i < n; i++ {
		w.Record(Usage{Provider: "p1", Model: "m", OK: true, LatencyMS: 1})
	}
	w.Flush()
	events, err := l.RecentEvents(ctx, 100, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != n {
		t.Fatalf("rows = %d, want %d (no row may be dropped)", len(events), n)
	}
	if st := w.Stats(); st.SyncFallbacks == 0 {
		t.Fatalf("SyncFallbacks = 0, want > 0 (queue of 2, 50 rows)")
	}
}

func TestAsyncWriterCloseFlushes(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{FlushInterval: time.Hour})
	for i := 0; i < 25; i++ {
		w.Record(Usage{Provider: "p1", Model: "m", OK: true})
	}
	w.Close()
	events, err := l.RecentEvents(ctx, 100, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 25 {
		t.Fatalf("rows after Close = %d, want 25", len(events))
	}
	// Second Close is a no-op, not a panic or hang.
	w.Close()
}

func TestAsyncWriterNilLedgerIsNoop(t *testing.T) {
	w := NewAsyncWriter(nil, nil, AsyncWriterConfig{})
	w.Record(Usage{Provider: "p1", Model: "m"})
	w.RecordShared("p1", "m", true, 1)
	w.Flush()
	w.Close()
	if st := w.Stats(); st.CommittedRows != 0 {
		t.Fatalf("nil-ledger stats = %+v", st)
	}
}

// TestAsyncWriterSetPrices (CHECKLIST 179): a periodic models.dev
// re-sync replaces the writer's price map, and rows committed after
// the swap are charged at the new price — rows before it keep the
// old price. The swap replaces the map reference, so a row is
// always priced with one whole map.
func TestAsyncWriterSetPrices(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	oldPrices := Prices{{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	w := NewAsyncWriter(l, oldPrices, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()
	usage := Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10,
	}
	w.Record(usage)
	w.Flush()
	w.SetPrices(Prices{{Model: "model-a"}: {PromptPerToken: 0.01, CompletionPerToken: 0.02}})
	w.Record(usage)
	w.Flush()
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	costs := map[float64]bool{}
	for _, e := range events {
		costs[e.CostUSD] = true
	}
	if !costs[0.20] || !costs[2.00] {
		t.Errorf("costs = %v, want one row at 0.20 (old price) and one at 2.00 (new price)", costs)
	}
}
