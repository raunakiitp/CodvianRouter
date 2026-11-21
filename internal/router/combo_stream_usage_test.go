package router

// Phase 2 carry-over closure (addendum §7.2): combo strategies run
// NON-streaming over their panel and replay the winner verbatim as a
// Done event. That design means streaming usage accounting must come
// from the panel's own ledger rows (noteSuccess / noteFailure /
// recordCancelledAttempt), never a second row from the replayed drain.
// This file proves both halves: the winner's ledger row carries the
// provider-reported split, and the replayed Done response carries it
// to the caller with NO duplicate row.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

type comboUsageRow struct {
	providerID, model string
	ok                bool
	usage             provider.Usage
}

type comboUsageLedger struct {
	mu        sync.Mutex
	rows      []comboUsageRow
	cancelled []string // providerIDs of hedged/cancelled attempts (CancelledLedger)
}

func (l *comboUsageLedger) RecordAttempt(providerID, model string, ok bool, latencyMs int64, u provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, comboUsageRow{providerID, model, ok, u})
}

// RecordCancelledAttempt mirrors ledger.CancelledLedger: hedged fan-out
// losers are recorded as cancellations (usage unknown), not as rows.
func (l *comboUsageLedger) RecordCancelledAttempt(keyID, providerID, clientKeyID, model string, latencyMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelled = append(l.cancelled, providerID)
}

func (l *comboUsageLedger) snapshot() ([]comboUsageRow, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]comboUsageRow(nil), l.rows...), append([]string(nil), l.cancelled...)
}

func comboUsageClient(content string, u provider.Usage) *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: content, Usage: u, Model: "m"}, nil
	}}
}

func TestComboStreamUsageRecordedOnceAndReported(t *testing.T) {
	loser := comboUsageClient("short", provider.Usage{PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18, CachedTokens: 4})
	winner := comboUsageClient("this is a much longer answer that the heuristic judge must pick",
		provider.Usage{PromptTokens: 20, CompletionTokens: 9, TotalTokens: 29, CachedTokens: 10, ReasoningTokens: 3})
	led := &comboUsageLedger{}
	eng := NewEngine(map[string]provider.Client{"a": loser, "b": winner}, led, testConfig())

	ch, err := eng.ChatStreamWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatStreamWithChainStrategy(fusion) = %v, want nil", err)
	}
	var sb strings.Builder
	var doneResp *provider.ChatResponse
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		sb.WriteString(ev.Delta)
		if ev.Done {
			doneResp = ev.Resp
		}
	}
	if doneResp == nil {
		t.Fatal("stream ended without a Done event")
	}
	// The caller sees the winner's provider-reported usage on the Done resp.
	if doneResp.Usage.PromptTokens != 20 || doneResp.Usage.CompletionTokens != 9 || doneResp.Usage.TotalTokens != 29 {
		t.Fatalf("done usage = %+v, want the winner's reported 20/9/29", doneResp.Usage)
	}
	if doneResp.Usage.CachedTokens != 10 || doneResp.Usage.ReasoningTokens != 3 {
		t.Errorf("done details = cached %d / reasoning %d, want 10 / 3", doneResp.Usage.CachedTokens, doneResp.Usage.ReasoningTokens)
	}

	// Exactly ONE success ledger row (the winner, reported usage) plus the
	// fan-out loser recorded as a cancellation. No duplicate success row
	// from the replayed drain.
	rows, cancelled := led.snapshot()
	if len(rows) != 1 || !rows[0].ok {
		t.Fatalf("success ledger rows = %+v, want exactly one (the winner)", rows)
	}
	winRow := &rows[0]
	if winRow.providerID != "b" || winRow.model != "m" {
		t.Errorf("winner row = %s/%s, want b/m", winRow.providerID, winRow.model)
	}
	if winRow.usage.PromptTokens != 20 || winRow.usage.CompletionTokens != 9 || winRow.usage.TotalTokens != 29 {
		t.Errorf("winner ledger usage = %+v, want the reported 20/9/29", winRow.usage)
	}
	if winRow.usage.LocalEstimate {
		t.Error("winner ledger row marked LocalEstimate despite an upstream report")
	}
	if len(cancelled) != 1 || cancelled[0] != "a" {
		t.Errorf("cancelled attempts = %v, want exactly [a] (the hedged fan-out loser)", cancelled)
	}
}
