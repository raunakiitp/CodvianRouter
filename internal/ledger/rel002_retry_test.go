package ledger

// AUD-REL-002: bounded retry for async batch commits whose
// transaction provably rolled back with nothing applied. The retry
// cannot double-record (nothing landed on a rolled-back attempt);
// failure modes where partial application cannot be ruled out (a
// failed COMMIT) are never retried. RetryAttempts and FailedRows
// keep the accounting honest either way.

import (
	"context"
	"testing"
	"time"
)

// TestAsyncCommitRetriesRolledBackBatch proves the bounded retry:
// against a ledger whose DB connection is closed, BeginTx fails
// every attempt — a retry-safe failure mode — so commitBatch
// attempts exactly asyncCommitMaxAttempts times, counts
// asyncCommitMaxAttempts-1 retry attempts, and finally counts the
// whole batch as failed rows (the rows never landed). The writer
// does not wedge and the counters tell the truth.
func TestAsyncCommitRetriesRolledBackBatch(t *testing.T) {
	l := openTestLedger(t)
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()

	// Kill the DB so every BeginTx fails: the retry-safest failure
	// mode (the transaction never even starts).
	if err := l.db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	batch := []Usage{
		{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a", PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true},
		{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a", PromptTokens: 20, CompletionTokens: 5, SplitKnown: true, OK: true},
	}
	w.commitBatch(batch)

	st := w.Stats()
	if st.RetryAttempts != asyncCommitMaxAttempts-1 {
		t.Fatalf("RetryAttempts = %d, want %d (bounded retry budget)", st.RetryAttempts, asyncCommitMaxAttempts-1)
	}
	if st.FailedRows != uint64(len(batch)) {
		t.Fatalf("FailedRows = %d, want %d (rows never landed after every retry rolled back)", st.FailedRows, len(batch))
	}
	if st.CommittedRows != 0 {
		t.Fatalf("CommittedRows = %d, want 0", st.CommittedRows)
	}
	if st.Batches != 0 {
		t.Fatalf("Batches = %d, want 0", st.Batches)
	}
}

// TestAsyncCommitHealthyBatchNoRetry proves the normal path pays no
// retry tax: a healthy commit records RetryAttempts = 0, counts the
// batch and its rows, and loses nothing.
func TestAsyncCommitHealthyBatchNoRetry(t *testing.T) {
	l := openTestLedger(t)
	w := NewAsyncWriter(l, nil, AsyncWriterConfig{BatchSize: 64, FlushInterval: time.Hour})
	defer w.Close()

	batch := []Usage{
		{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a", PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true},
	}
	w.commitBatch(batch)

	st := w.Stats()
	if st.RetryAttempts != 0 {
		t.Fatalf("RetryAttempts = %d, want 0 on a healthy commit", st.RetryAttempts)
	}
	if st.FailedRows != 0 {
		t.Fatalf("FailedRows = %d, want 0", st.FailedRows)
	}
	if st.CommittedRows != 1 || st.Batches != 1 {
		t.Fatalf("CommittedRows/Batches = %d/%d, want 1/1", st.CommittedRows, st.Batches)
	}
	// The row actually landed.
	events, err := l.RecentEvents(context.Background(), 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("rows in ledger = %d, want 1", len(events))
	}
}
