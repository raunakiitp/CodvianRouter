package ledger

// BenchmarkCommitBatch pins the cost of the async writer's batch
// commit: one transaction per batch, one INSERT per row. In the
// modernc.org/sqlite driver every Exec re-parses its SQL
// (prepareV2 inside exec, statement finalized after each call),
// so the per-row INSERT re-parses the 22-column statement once
// per row — the cpu2 profile (2026-11-18) showed
// insert.ExecContext at 2.09 s of commitBatch's 2.62 s cum.
// A multi-row INSERT must move this number; if it does not, the
// change is within noise and must not be claimed as a win.
import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func makeBenchBatch(n int) []Usage {
	batch := make([]Usage, n)
	for i := range batch {
		batch[i] = Usage{
			GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
			PromptTokens: 100, CompletionTokens: 50, SplitKnown: true,
			OK: true, LatencyMS: 10,
		}
	}
	return batch
}

func BenchmarkCommitBatch256(b *testing.B) {
	l, err := Open(filepath.Join(b.TempDir(), "ledger.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { l.Close() })
	prices := Prices{{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	if err := l.SetBudget(context.Background(), "key-1", 1e9, "test"); err != nil {
		b.Fatalf("SetBudget: %v", err)
	}
	w := NewAsyncWriter(l, prices, AsyncWriterConfig{BatchSize: 256, FlushInterval: time.Hour})
	defer w.Close()
	batch := makeBenchBatch(256)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.commitBatch(batch)
	}
	b.ReportMetric(float64(len(batch)), "rows/batch")
}
