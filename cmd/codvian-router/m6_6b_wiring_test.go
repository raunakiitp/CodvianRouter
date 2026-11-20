package main

import (
	"context"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// M6.6b — serve-mode wiring for the reasoning replay cache.
//
// openStack has no test harness (it builds the vault, ledger,
// engine, guardrails and plugins for a real config), so the
// wiring is tested at the seam openStack itself calls:
// wireReasoningCache must leave a StaleEngine with a LIVE
// reasoning cache — one that actually captures and injects —
// not a nil or a dead one. The remaining link (openStack calls
// wireReasoningCache next to SetCacheMetrics) is verified by
// code inspection + build; the call site is a single line in
// openStack, next to the SetCacheMetrics wiring. Toggle
// evidence: with the SetReasoningCache line in wireReasoningCache
// removed, TestM66bWireReasoningCacheWiresLiveCache fails (the
// stats stay zero and the follow-up carries no reasoning).

// m66bCmdFakeEngine records the exact request each call received.
type m66bCmdFakeEngine struct {
	mu          sync.Mutex
	chatResp    *provider.ChatResponse
	lastChatReq provider.ChatRequest
	chatCalls   int
}

func (f *m66bCmdFakeEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastChatReq = req
	f.chatCalls++
	return f.chatResp, nil
}

func (f *m66bCmdFakeEngine) ChatStream(_ context.Context, req provider.ChatRequest, _ func(string) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastChatReq = req
	return f.chatResp, nil
}

func (f *m66bCmdFakeEngine) receivedChat() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastChatReq
}

func TestM66bWireReasoningCacheWiresLiveCache(t *testing.T) {
	ctx := context.Background()
	f := &m66bCmdFakeEngine{}
	se := degrade.Wrap(f, cache.NewExactCache(10), degrade.DefaultPolicy()).(*degrade.StaleEngine)

	// The helper openStack calls must wire a live cache.
	wireReasoningCache(se)
	wireReasoningCache(nil) // nil wrapper is a no-op, never a panic

	// Turn 1: a thinking tool-call response must be captured.
	f.chatResp = &provider.ChatResponse{
		Content: "calling calc",
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
		},
		Reasoning: &reasoning.State{
			Blocks:     []reasoning.Block{{Kind: reasoning.KindThought, Text: "thinking about math"}},
			SourceWire: reasoning.WireOpenAI,
		},
		Model: "deepseek-chat",
	}
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("reasoning cache empty after wiring + a thinking response (want a live cache, not a no-op wire)")
	}

	// Turn 2: the follow-up must carry the restored reasoning.
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
	followUp := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "calling calc", ToolCalls: []provider.ToolCall{
				{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "4"},
			{Role: "user", Content: "and the result is?"},
		},
	}
	if _, err := se.Chat(ctx, followUp); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("wired engine received reasoning %v, want restored text 'thinking about math'", got)
	}
}

func TestM66bWireReasoningCacheDefaults(t *testing.T) {
	// The wired cache must be the M6.6 defaults: 2 h TTL, 200
	// entries, M6.1 caps — i.e. exactly cache.NewReasoningCache().
	// There is no exported accessor for the TTL/caps, so this
	// pins the constructor the helper uses: the defaults the
	// package documents.
	if cache.DefaultReasoningTTL.Hours() != 2 || cache.DefaultReasoningMaxEntries != 200 {
		t.Fatalf("reasoning defaults = (%v, %d), want (2h, 200)", cache.DefaultReasoningTTL, cache.DefaultReasoningMaxEntries)
	}
	if cache.DefaultReasoningMaxEntryBytes != cache.DefaultMaxEntryBytes || cache.DefaultReasoningMaxTotalBytes != cache.DefaultMaxTotalBytes {
		t.Fatalf("reasoning caps = (%d, %d), want M6.1 caps (%d, %d)",
			cache.DefaultReasoningMaxEntryBytes, cache.DefaultReasoningMaxTotalBytes,
			cache.DefaultMaxEntryBytes, cache.DefaultMaxTotalBytes)
	}
}
