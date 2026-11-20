// B4-51 own-cache tokens_saved accounting (parity tracker:1115): a
// proactive own-cache hit must record one ledger row carrying the
// avoided call's measured usage and the tokens saved — prompt+
// completion for an exact hit, prompt only for a semantic hit —
// marked NoCharge ($0 by construction, no upstream call happened).
// These tests exercise the real serving path (StaleEngine.Chat) and
// the real recordCacheHitLedger, with a capturing ledgerRecorder.
package degrade

import (
	"context"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// b451FakeLedgerRecorder is a ledgerRecorder stand-in: it keeps
// every Record call so the test can assert on the row the hit
// recorded (or that no row was recorded).
type b451FakeLedgerRecorder struct {
	mu   sync.Mutex
	rows []ledger.Usage
}

func (r *b451FakeLedgerRecorder) Record(u ledger.Usage) {
	r.mu.Lock()
	r.rows = append(r.rows, u)
	r.mu.Unlock()
}

func (r *b451FakeLedgerRecorder) snapshot() []ledger.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ledger.Usage(nil), r.rows...)
}

// TestB451ExactHitRecordsNoChargeLedgerRow: an exact proactive hit
// records exactly one ledger row with the avoided call's full split
// (prompt+completion) and CacheTokensSaved = prompt+completion,
// marked NoCharge. The first (miss) Chat records nothing.
func TestB451ExactHitRecordsNoChargeLedgerRow(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	rec := &b451FakeLedgerRecorder{}
	se.SetLedgerRecorder(rec)
	ctx := context.Background()
	alice := testReq()
	alice.CallerID = "gk-alice"
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("ledger rows after miss = %d, want 0 (a miss records no hit row)", len(rec.snapshot()))
	}
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "exact" {
		t.Fatalf("LastCacheHit = (%q, %v), want (exact, true)", kind, ok)
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1 (one proactive exact hit)", len(rows))
	}
	row := rows[0]
	if row.GatewayKeyID != "gk-alice" {
		t.Errorf("GatewayKeyID = %q, want gk-alice", row.GatewayKeyID)
	}
	if row.Provider != "cache" {
		t.Errorf("Provider = %q, want cache", row.Provider)
	}
	if row.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", row.Model)
	}
	if row.PromptTokens != 5 || row.CompletionTokens != 1 || row.TotalTokens != 6 {
		t.Errorf("tokens = %d/%d/%d, want 5/1/6 (the avoided call's measured usage)", row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
	if row.CacheTokensSaved != 6 {
		t.Errorf("CacheTokensSaved = %d, want 6 (5 prompt + 1 completion)", row.CacheTokensSaved)
	}
	if !row.SplitKnown {
		t.Error("SplitKnown = false, want true (the avoided call's split is known)")
	}
	if !row.OK {
		t.Error("OK = false, want true (the hit was served successfully)")
	}
	if !row.NoCharge {
		t.Error("NoCharge = false, want true (no upstream call happened)")
	}
}

// TestB451SemanticHitRecordsPromptOnlyNoChargeLedgerRow: a semantic
// proactive hit records exactly one ledger row with the avoided
// call's full split but CacheTokensSaved = prompt only — the
// paraphrase's own completion would have differed — marked NoCharge.
func TestB451SemanticHitRecordsPromptOnlyNoChargeLedgerRow(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	rec := &b451FakeLedgerRecorder{}
	se.SetLedgerRecorder(rec)
	ctx := context.Background()

	first := capitalReq("What is the capital of France?")
	first.CallerID = "gk-alice"
	if _, err := se.Chat(ctx, first); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("ledger rows after miss = %d, want 0", len(rec.snapshot()))
	}

	// Paraphrase with one extra word: same words, same order.
	// Must hit semantically, without calling the provider.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	second := capitalReq("What is the capital of France today?")
	second.CallerID = "gk-alice"
	if _, err := se.Chat(ctx, second); err != nil {
		t.Fatalf("paraphrase Chat: %v", err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
	if f.chatCalls != 1 {
		t.Fatalf("semantic hit called provider %d times, want 1 (no provider call on a hit)", f.chatCalls)
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1 (one proactive semantic hit)", len(rows))
	}
	row := rows[0]
	if row.GatewayKeyID != "gk-alice" {
		t.Errorf("GatewayKeyID = %q, want gk-alice", row.GatewayKeyID)
	}
	if row.Provider != "cache" {
		t.Errorf("Provider = %q, want cache", row.Provider)
	}
	if row.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", row.Model)
	}
	if row.PromptTokens != 8 || row.CompletionTokens != 7 || row.TotalTokens != 15 {
		t.Errorf("tokens = %d/%d/%d, want 8/7/15 (the avoided call's measured usage)", row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
	if row.CacheTokensSaved != 8 {
		t.Errorf("CacheTokensSaved = %d, want 8 (prompt only — the paraphrase's own completion would have differed)", row.CacheTokensSaved)
	}
	if !row.SplitKnown {
		t.Error("SplitKnown = false, want true")
	}
	if !row.OK {
		t.Error("OK = false, want true")
	}
	if !row.NoCharge {
		t.Error("NoCharge = false, want true (no upstream call happened)")
	}
}

// TestB451SetLedgerRecorderNilDoesNotPanic: with no recorder wired
// (nil), a served exact hit must not panic — recording is best
// effort and disabled by a nil recorder. The same holds when a
// wired recorder is replaced by nil mid-life: earlier rows stay,
// later hits record nothing, nothing panics.
func TestB451SetLedgerRecorderNilDoesNotPanic(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	se.SetLedgerRecorder(nil)
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat (nil recorder): %v", err)
	}
	// Second Chat is an exact hit; with a nil recorder it must
	// still serve without panicking.
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("exact-hit Chat (nil recorder): %v", err)
	}

	rec := &b451FakeLedgerRecorder{}
	se.SetLedgerRecorder(rec)
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("exact-hit Chat (recorder wired): %v", err)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rec.snapshot()))
	}
	se.SetLedgerRecorder(nil)
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("exact-hit Chat (recorder unwired): %v", err)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("ledger rows after unwire = %d, want 1 (no further rows)", len(rec.snapshot()))
	}
}
