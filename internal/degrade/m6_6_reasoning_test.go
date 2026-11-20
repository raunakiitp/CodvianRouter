package degrade

import (
	"context"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// M6.6 — degrade wiring for the reasoning replay cache (parity tracker
// Area 6, "Reasoning cache (2 h)"). The toggle evidence is
// recorded in the M6.6 report: with the injectReasoning call in
// Chat removed, TestM66ReasoningWiringHitInjectsBeforeUpstream
// fails (the engine receives no reasoning); with the
// storeReasoning call in store() removed, the same test fails at
// the store step (no entry exists to inject).

// m66CapturingEngine records the exact request each call received,
// so tests can assert reasoning was injected before upstream.
type m66CapturingEngine struct {
	mu            sync.Mutex
	chatResp      *provider.ChatResponse
	chatErr       error
	streamResp    *provider.ChatResponse
	streamErr     error
	lastChatReq   provider.ChatRequest
	lastStreamReq provider.ChatRequest
	chatCalls     int
}

func (f *m66CapturingEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastChatReq = req
	f.chatCalls++
	return f.chatResp, f.chatErr
}

func (f *m66CapturingEngine) ChatStream(_ context.Context, req provider.ChatRequest, _ func(string) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.lastStreamReq = req
	resp, err := f.streamResp, f.streamErr
	f.mu.Unlock()
	return resp, err
}

func (f *m66CapturingEngine) receivedChat() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastChatReq
}

func m66ThinkingResp() *provider.ChatResponse {
	return &provider.ChatResponse{
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
}

func m66FollowUpReq(tenant string) provider.ChatRequest {
	return provider.ChatRequest{
		CallerID: tenant,
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
}

func TestM66ReasoningWiringHitInjectsBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// Turn 1: a thinking response carrying reasoning + a tool call.
	f.chatResp = m66ThinkingResp()
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("reasoning cache empty after a thinking tool-call response (want stored)")
	}

	// Turn 2: the client replays the transcript WITHOUT reasoning.
	// The engine must still be called (injection is not a cache
	// hit), but the assistant message it receives must carry the
	// restored reasoning.
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
	followUp := m66FollowUpReq("tenant-a")
	origMsgs := followUp.Messages
	if _, err := se.Chat(ctx, followUp); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	if len(received.Messages) != 4 {
		t.Fatalf("engine received %d messages, want 4", len(received.Messages))
	}
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("engine received reasoning %v, want restored text 'thinking about math'", got)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit true after a reasoning injection: injection serves no cached response")
	}
	// Copy-on-write: the caller's own slice must be untouched.
	if origMsgs[1].Reasoning != nil {
		t.Fatalf("caller's request slice mutated by injection (want copy-on-write)")
	}
}

func TestM66ReasoningWiringTenantIsolation(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	f.chatResp = m66ThinkingResp()
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	// Same tool-call id, different tenant: no injection.
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
	if _, err := se.Chat(ctx, m66FollowUpReq("tenant-b")); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	if received.Messages[1].Reasoning != nil {
		t.Fatalf("tenant B's request got tenant A's reasoning injected: %v", received.Messages[1].Reasoning)
	}
}

func TestM66ReasoningWiringTemperatureGate(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy()) // AllowNonZeroTemp=false
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// temp 1.0: hot reasoning is never stored (M1.4/M6.3).
	f.chatResp = m66ThinkingResp()
	hot := provider.ChatRequest{
		CallerID:    "tenant-a",
		Model:       "deepseek-chat",
		Temperature: 1.0,
		Messages:    []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, hot); err != nil {
		t.Fatalf("hot Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries != 0 {
		t.Fatalf("temp=1.0 reasoning stored (%d entries), want refused", st.Entries)
	}
	// temp 0: stored.
	f.chatResp = m66ThinkingResp()
	cold := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, cold); err != nil {
		t.Fatalf("cold Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("temp=0 reasoning not stored, want stored")
	}
}

func TestM66ReasoningWiringTruncatedNotStored(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	resp := m66ThinkingResp()
	resp.FinishReason = "length"
	f.chatResp = resp
	req := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries != 0 {
		t.Fatalf("truncated (finish_reason=length) reasoning stored (%d entries), want refused (M6.3)", st.Entries)
	}
}

func TestM66ReasoningWiringToolCallResponsesAreStored(t *testing.T) {
	// The ShouldStore tool-call exclusion does NOT apply to
	// reasoning replay: the cache stores reasoning text, not
	// content, so it cannot drop tool calls — and tool-call
	// turns are exactly the turns replay exists for. A response
	// WITH tool calls and reasoning MUST be stored.
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	f.chatResp = m66ThinkingResp() // has ToolCalls AND Reasoning
	req := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("tool-call reasoning response not stored, want stored (replay exists for tool turns)")
	}
}

func TestM66ReasoningWiringConversationKeyPath(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// Turn 1: plain assistant response carrying reasoning, no
	// tool calls — stored under the conversation digest.
	f.chatResp = &provider.ChatResponse{
		Content:   "hi there",
		Reasoning: &reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "greeting thought"}}},
		Model:     "deepseek-chat",
	}
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	// Turn 2: client replays user + assistant WITHOUT reasoning.
	f.chatResp = &provider.ChatResponse{Content: "and then?", Model: "deepseek-chat"}
	turn2 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
			{Role: "user", Content: "go on"},
		},
	}
	if _, err := se.Chat(ctx, turn2); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	if received.Messages[1].Reasoning == nil || received.Messages[1].Reasoning.ReasoningText() != "greeting thought" {
		t.Fatalf("conversation-key injection missing: assistant reasoning = %v", received.Messages[1].Reasoning)
	}
}

func TestM66ReasoningWiringStreamStores(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	f.streamResp = m66ThinkingResp()
	req := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.ChatStream(ctx, req, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("reasoning not stored after a successful stream, want stored (store() covers all paths)")
	}
}

func TestM66ReasoningWiringAlreadyHasReasoningUntouched(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// Seed the cache with one entry.
	f.chatResp = m66ThinkingResp()
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	// Turn 2 carries its OWN reasoning: injection must not
	// overwrite it with the cached text.
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
	followUp := m66FollowUpReq("tenant-a")
	followUp.Messages[1].Reasoning = &reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "caller's own reasoning"}},
	}
	if _, err := se.Chat(ctx, followUp); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	if got := received.Messages[1].Reasoning; got == nil || got.ReasoningText() != "caller's own reasoning" {
		t.Fatalf("own reasoning overwritten: %v", got)
	}
}

func TestM66ReasoningWiringNoCacheIsPassthrough(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	// No SetReasoningCache call: the wrapper must behave exactly
	// as before, and stats must be honestly zero.
	f.chatResp = m66ThinkingResp()
	req := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, req); err != nil {
		t.Fatalf("Chat without reasoning cache: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries != 0 || st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("ReasoningCacheStats without a cache = %+v, want zero (no cache, no stats)", st)
	}
	if f.chatCalls != 1 {
		t.Fatalf("chatCalls = %d, want 1", f.chatCalls)
	}
}
