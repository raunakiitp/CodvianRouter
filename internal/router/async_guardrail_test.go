package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestAsyncGuardrailCutStream proves the engine side of async
// guardrail mode (CHECKLIST 185): when the server cancels a
// stream's context with an AsyncBlockError cause (the async
// verdict said BLOCK), the engine
//
//   - ends the stream without failing over (a policy cut is not a
//     provider failure — the second target is never dialled),
//   - records the trace attempt as outcome/reason
//     async_guardrail_blocked (never "transport_error"),
//   - records the ledger row as failed with the local prompt
//     estimate only (LocalEstimate, completion 0 — billed for
//     what was actually served before the cut, never an invented
//     split),
//   - touches no health signal: no attempt stats, no breaker, no
//     cooldown — a second request still routes to the same
//     provider.
//
// The returned error is the block error when the terminal event
// wins the race, or context.Canceled when the drain's own
// cancellation wins it — the server decides the refusal frame from
// its own BlockVerdict, not from this error, so either is correct
// here and the test accepts both; the trace and the ledger row are
// the deterministic record.
func TestAsyncGuardrailCutStream(t *testing.T) {
	// slowClient emits one chunk, then blocks until its context
	// ends or release closes. The first request is cut by the
	// async verdict; the second is released to prove the provider
	// is still healthy afterwards.
	delivered := make(chan struct{})
	release := make(chan struct{})
	var deliverOnce sync.Once
	slow := &prereadChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: "Hello "}); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &provider.ChatResponse{Content: "Hello world"}, nil
		}
	}}
	b := streamClient([]string{"bee"}, nil)
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"a": slow, "b": b}, led, testConfig())

	// The server side of the cut: a BLOCK verdict cancels the
	// stream context with the block as the cause, as soon as the
	// first chunk has been delivered to the client (the test's
	// onChunk signals it — waiting on the client's emission
	// instead would cancel before the drain forwards the chunk).
	streamCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		<-delivered
		cancel(&guardrails.AsyncBlockError{Guardrail: "toxicity", Kind: "toxicity"})
	}()
	var got strings.Builder
	_, err := eng.ChatStreamChunks(streamCtx, testReq(), func(chunk provider.StreamChunk) error {
		got.WriteString(chunk.Content)
		deliverOnce.Do(func() { close(delivered) })
		return nil
	})
	if err == nil {
		t.Fatalf("cut stream must return an error, got nil (delivered %q)", got.String())
	}
	var blk *guardrails.AsyncBlockError
	if !errors.As(err, &blk) && !errors.Is(err, context.Canceled) {
		t.Fatalf("cut stream error = %v, want the async block error or context.Canceled", err)
	}
	if got.String() != "Hello " {
		t.Fatalf("delivered = %q, want only the chunk sent before the cut", got.String())
	}
	if b.streamCalls() != 0 {
		t.Fatalf("b stream calls = %d, want 0 (no failover after a policy cut)", b.streamCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	}
	att := trace.Attempts[0]
	if att.Outcome != guardrails.AsyncBlockOutcome || att.Reason != guardrails.AsyncBlockReason {
		t.Fatalf("trace attempt outcome=%q reason=%q, want %q/%q", att.Outcome, att.Reason, guardrails.AsyncBlockOutcome, guardrails.AsyncBlockReason)
	}
	led.mu.Lock()
	rows := append([]ledgerCall(nil), led.calls...)
	led.mu.Unlock()
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 (the cut attempt)", len(rows))
	}
	row := rows[0]
	if row.ok {
		t.Fatalf("ledger row ok=true, want false (the stream was cut, not served)")
	}
	if row.usage.PromptTokens <= 0 || !row.usage.LocalEstimate {
		t.Fatalf("ledger row usage = %+v, want the local prompt estimate (billed for the prompt served)", row.usage)
	}
	if row.usage.CompletionTokens != 0 {
		t.Fatalf("ledger row completion tokens = %d, want 0 (unknown, never invented)", row.usage.CompletionTokens)
	}
	// No health signal: the cut recorded no attempt stats, so the
	// provider's success rate is untouched.
	eng.mu.Lock()
	stats := eng.attemptStats["a"]
	eng.mu.Unlock()
	if stats != nil {
		t.Fatalf("attempt stats for a = %+v, want none (a policy cut says nothing about provider health)", stats)
	}
	// A second request still routes to the same provider: no
	// breaker trip, no cooldown skip.
	close(release)
	_, err = eng.ChatStreamChunks(context.Background(), testReq(), func(chunk provider.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("second stream after the cut: %v", err)
	}
	if b.streamCalls() != 0 {
		t.Fatalf("b stream calls = %d after the second request, want 0 (a was not skipped)", b.streamCalls())
	}
}

// TestAsyncGuardrailCutBeforeCommit proves a BLOCK verdict that
// lands before the first byte still cuts the stream (the client
// gets the 403 refusal, not a partial body): the provider emits
// nothing, the server cancels with the block cause, and the engine
// records the same async_guardrail_blocked attempt + prompt-estimate
// ledger row.
func TestAsyncGuardrailCutBeforeCommit(t *testing.T) {
	started := make(chan struct{})
	var startOnce sync.Once
	slow := &prereadChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		startOnce.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	b := streamClient([]string{"bee"}, nil)
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"a": slow, "b": b}, led, testConfig())

	streamCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		<-started
		cancel(&guardrails.AsyncBlockError{Guardrail: "toxicity", Kind: "toxicity"})
	}()
	var got strings.Builder
	_, err := eng.ChatStreamChunks(streamCtx, testReq(), func(chunk provider.StreamChunk) error {
		got.WriteString(chunk.Content)
		return nil
	})
	if err == nil {
		t.Fatalf("pre-commit cut must return an error, got nil")
	}
	if got.String() != "" {
		t.Fatalf("delivered = %q, want nothing (the cut landed before the first byte)", got.String())
	}
	if b.streamCalls() != 0 {
		t.Fatalf("b stream calls = %d, want 0 (no failover after a policy cut)", b.streamCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != guardrails.AsyncBlockOutcome {
		t.Fatalf("trace = %+v, want exactly 1 async_guardrail_blocked attempt", trace)
	}
	led.mu.Lock()
	rows := append([]ledgerCall(nil), led.calls...)
	led.mu.Unlock()
	if len(rows) != 1 || rows[0].ok || rows[0].usage.PromptTokens <= 0 || !rows[0].usage.LocalEstimate {
		t.Fatalf("ledger rows = %+v, want 1 failed row with the local prompt estimate", rows)
	}
}

// TestAsyncGuardrailLateCancelKeepsSuccess proves the guard on the
// cut: a cancel whose cause is an async block, arriving AFTER the
// stream already completed, must not rewrite history — the
// attempt stays a success. (The server records a too-late BLOCK
// as blocked_after_delivery; the engine must not contradict it.)
func TestAsyncGuardrailLateCancelKeepsSuccess(t *testing.T) {
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"a": streamClient([]string{"done"}, nil)}, led, testConfig())
	streamCtx, cancel := context.WithCancelCause(context.Background())
	_, err := eng.ChatStreamChunks(streamCtx, testReq(), func(chunk provider.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("complete stream: %v", err)
	}
	cancel(&guardrails.AsyncBlockError{Guardrail: "toxicity", Kind: "toxicity"})
	// Give the engine no chance to re-record: the row was already
	// written when the stream completed. Poll briefly only to let
	// any stray goroutine settle, then assert the single row.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace = %+v, want exactly 1 success attempt (the late cancel must not rewrite it)", trace)
	}
	led.mu.Lock()
	rows := append([]ledgerCall(nil), led.calls...)
	led.mu.Unlock()
	if len(rows) != 1 || !rows[0].ok {
		t.Fatalf("ledger rows = %+v, want 1 successful row", rows)
	}
}
