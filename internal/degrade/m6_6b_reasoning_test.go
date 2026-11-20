package degrade

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// M6.6b — shadowing fix + chunk-stream injection + serve wiring.
//
// SHADOWING (the MUST-FIX): degrade runs outside the router, so
// its inject fires first and the router's injectReasoning then
// skips any message that already carries reasoning. M6.6 injected
// a TEXT-ONLY KindThought block, so on an Anthropic/Bedrock target
// the router's full signed state was pre-empted by unsigned text
// and the hop failed (reasoning.Check → UnrepresentableError).
// M6.6b stores and injects the FULL captured state, so the skip
// is exact: the signed state is already there. Toggle evidence:
// with injectReasoning reverted to the text-only shape, the
// Anthropic/Bedrock tests below fail (Check returns
// UnrepresentableError); with the injectReasoning call removed
// from ChatStreamChunks / ChatStreamFromChunks, the chunk tests
// fail (the inner engine receives no reasoning); with the call
// removed from ChatStreamFrom, the text-resume test below fails
// the same way (critic 2026-11-20: 17/17 passed with it off).

// m66bChunkEngine is m66CapturingEngine plus the chunk interfaces
// and the text StreamResumer, recording the exact request each
// call received.
type m66bChunkEngine struct {
	*m66CapturingEngine
	chunkResp         *provider.ChatResponse
	chunkErr          error
	fromChunksResp    *provider.ChatResponse
	fromChunksErr     error
	fromResp          *provider.ChatResponse
	fromErr           error
	lastChunkReq      provider.ChatRequest
	lastFromChunksReq provider.ChatRequest
	lastFromReq       provider.ChatRequest
	chunkCalls        int
	fromChunksCalls   int
	fromCalls         int
}

func (f *m66bChunkEngine) ChatStreamChunks(_ context.Context, req provider.ChatRequest, _ func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastChunkReq = req
	f.chunkCalls++
	return f.chunkResp, f.chunkErr
}

func (f *m66bChunkEngine) ChatStreamFromChunks(_ context.Context, req provider.ChatRequest, _ int, _ func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFromChunksReq = req
	f.fromChunksCalls++
	return f.fromChunksResp, f.fromChunksErr
}

// ChatStreamFrom is the text-only StreamResumer: a client
// reconnect asking to resume from an offset. It records the
// request so the text-resume test can assert injection happened
// before the delegation, exactly like the chunk methods above.
func (f *m66bChunkEngine) ChatStreamFrom(_ context.Context, req provider.ChatRequest, _ int, _ func(string) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFromReq = req
	f.fromCalls++
	return f.fromResp, f.fromErr
}

func (f *m66bChunkEngine) receivedChunk() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastChunkReq
}

func (f *m66bChunkEngine) receivedFrom() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFromReq
}

func (f *m66bChunkEngine) receivedFromChunks() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFromChunksReq
}

func m66bAnthropicThinkingResp() *provider.ChatResponse {
	return &provider.ChatResponse{
		Content: "calling calc",
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
		},
		Reasoning: &reasoning.State{
			Blocks: []reasoning.Block{{
				Kind:      reasoning.KindThinking,
				Text:      "anthropic thought",
				Signature: "sig-abc-123",
			}},
			SourceWire:  reasoning.WireAnthropic,
			SourceModel: "claude-sonnet-4-5",
		},
		Model: "claude-sonnet-4-5",
	}
}

func m66bFollowUpReq(model string) provider.ChatRequest {
	req := m66FollowUpReq("tenant-a")
	req.Model = model
	return req
}

func TestM66bSignedAnthropicReasoningSurvivesDegradeInject(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// Turn 1: an Anthropic thinking response with a signature.
	f.chatResp = m66bAnthropicThinkingResp()
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "claude-sonnet-4-5",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("reasoning cache empty after a signed thinking response (want stored)")
	}

	// Turn 2: the client replays the transcript WITHOUT reasoning.
	// The inner engine must receive the SIGNED block — not the
	// old text-only KindThought shape, which Check rejects for
	// Anthropic targets.
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "claude-sonnet-4-5"}
	if _, err := se.Chat(ctx, m66bFollowUpReq("claude-sonnet-4-5")); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	got := received.Messages[1].Reasoning
	if got == nil {
		t.Fatalf("engine received no reasoning (want the signed Anthropic state)")
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindThinking {
		t.Fatalf("engine received blocks %v, want one KindThinking block (not a text-only KindThought)", got.Blocks)
	}
	if got.Blocks[0].Text != "anthropic thought" || got.Blocks[0].Signature != "sig-abc-123" {
		t.Fatalf("engine received block %+v, want text 'anthropic thought' with signature 'sig-abc-123'", got.Blocks[0])
	}
	if got.SourceWire != reasoning.WireAnthropic {
		t.Fatalf("SourceWire = %q, want %q (genuine provenance, not re-stamped)", got.SourceWire, reasoning.WireAnthropic)
	}
	// The wire verdict is the point of the fix: the injected
	// state must be representable on Anthropic, exactly as the
	// router's own store would have made it.
	if status, _, err := reasoning.Check(got, reasoning.WireAnthropic, "claude-sonnet-4-5"); err != nil || status != reasoning.StatusPreserved {
		t.Fatalf("reasoning.Check(anthropic) = (%q, %v), want (preserved, nil) — the signed state was lost to a text-only inject", status, err)
	}
}

func TestM66bSignedBedrockReasoningSurvivesDegradeInject(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	data := json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"bedrock thought","signature":"bedrock-sig-9"}}}`)
	f.chatResp = &provider.ChatResponse{
		Content: "calling calc",
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
		},
		Reasoning: &reasoning.State{
			Blocks:      []reasoning.Block{{Kind: reasoning.KindBedrock, Data: data}},
			SourceWire:  reasoning.WireBedrock,
			SourceModel: "anthropic.claude-sonnet-4-5",
		},
		Model: "anthropic.claude-sonnet-4-5",
	}
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "anthropic.claude-sonnet-4-5",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "anthropic.claude-sonnet-4-5"}
	if _, err := se.Chat(ctx, m66bFollowUpReq("anthropic.claude-sonnet-4-5")); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	got := received.Messages[1].Reasoning
	if got == nil || len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindBedrock {
		t.Fatalf("engine received reasoning %v, want one KindBedrock block verbatim", got)
	}
	if string(got.Blocks[0].Data) != string(data) {
		t.Fatalf("engine received Bedrock Data %s, want %s (verbatim, not text-extracted)", got.Blocks[0].Data, data)
	}
	if status, _, err := reasoning.Check(got, reasoning.WireBedrock, "anthropic.claude-sonnet-4-5"); err != nil || status != reasoning.StatusPreserved {
		t.Fatalf("reasoning.Check(bedrock) = (%q, %v), want (preserved, nil)", status, err)
	}
}

func TestM66bSignedReasoningConversationKeyPath(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// Turn 1: signed thinking, no tool calls — stored under the
	// conversation digest, not a tool-call key.
	f.chatResp = &provider.ChatResponse{
		Content: "hi there",
		Reasoning: &reasoning.State{
			Blocks:     []reasoning.Block{{Kind: reasoning.KindThinking, Text: "greeting thought", Signature: "sig-greet"}},
			SourceWire: reasoning.WireAnthropic,
		},
		Model: "claude-sonnet-4-5",
	}
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "claude-sonnet-4-5",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	f.chatResp = &provider.ChatResponse{Content: "and then?", Model: "claude-sonnet-4-5"}
	turn2 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "claude-sonnet-4-5",
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
	got := received.Messages[1].Reasoning
	if got == nil || len(got.Blocks) != 1 || got.Blocks[0].Signature != "sig-greet" {
		t.Fatalf("conversation-key injection lost the signature: %v", got)
	}
}

func TestM66bRedactedOnlyStateStoredAndInjected(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// A redacted-only block has no text at all, but it replays
	// verbatim on Anthropic — the old text=="" store gate would
	// have dropped it.
	f.chatResp = &provider.ChatResponse{
		Content: "calling calc",
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
		},
		Reasoning: &reasoning.State{
			Blocks:     []reasoning.Block{{Kind: reasoning.KindRedactedThinking, Data: json.RawMessage(`"encrypted-blob"`)}},
			SourceWire: reasoning.WireAnthropic,
		},
		Model: "claude-sonnet-4-5",
	}
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "claude-sonnet-4-5",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("redacted-only reasoning not stored (want stored: it replays verbatim on Anthropic)")
	}
	f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "claude-sonnet-4-5"}
	if _, err := se.Chat(ctx, m66bFollowUpReq("claude-sonnet-4-5")); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	received := f.receivedChat()
	got := received.Messages[1].Reasoning
	if got == nil || len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindRedactedThinking || string(got.Blocks[0].Data) != `"encrypted-blob"` {
		t.Fatalf("redacted block not injected verbatim: %v", got)
	}
	if status, _, err := reasoning.Check(got, reasoning.WireAnthropic, "claude-sonnet-4-5"); err != nil || status != reasoning.StatusPreserved {
		t.Fatalf("reasoning.Check(anthropic) on redacted state = (%q, %v), want (preserved, nil)", status, err)
	}
}

func TestM66bVacuousEmptyThoughtBlockNotStored(t *testing.T) {
	ctx := context.Background()
	f := &m66CapturingEngine{}
	se, _ := wrapTest(f, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	// An empty-text thought block with no signature and no Data
	// is vacuous: storing it would hand an Anthropic/Bedrock
	// target an unsigned block for no reason.
	f.chatResp = &provider.ChatResponse{
		Content:   "calling calc",
		ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}}},
		Reasoning: &reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: ""}}},
		Model:     "deepseek-chat",
	}
	req := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries != 0 {
		t.Fatalf("vacuous empty-text thought block stored (%d entries), want refused", st.Entries)
	}
}

// m66bSeedReasoning stores one thinking tool-call turn via Chat
// and returns the wrapper around a fresh chunk engine, so the
// chunk tests below start from a populated reasoning cache.
func m66bSeedReasoning(t *testing.T, ctx context.Context) (*StaleEngine, *m66bChunkEngine) {
	t.Helper()
	inner := &m66bChunkEngine{m66CapturingEngine: &m66CapturingEngine{}}
	se, _ := wrapTest(inner, DefaultPolicy())
	rc := cache.NewReasoningCache()
	se.SetReasoningCache(rc)

	inner.chatResp = m66ThinkingResp()
	turn1 := provider.ChatRequest{
		CallerID: "tenant-a",
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "what is 2+2?"}},
	}
	if _, err := se.Chat(ctx, turn1); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	if st := se.ReasoningCacheStats(); st.Entries == 0 {
		t.Fatalf("seed left the reasoning cache empty")
	}
	return se, inner
}

func TestM66bChunkStreamInjectsBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	se, inner := m66bSeedReasoning(t, ctx)
	inner.chunkResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}

	// The server's live streaming path is ChatStreamChunks: the
	// inner chunk engine must receive the restored reasoning.
	if _, err := se.ChatStreamChunks(ctx, m66FollowUpReq("tenant-a"), func(provider.StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	received := inner.receivedChunk()
	if len(received.Messages) != 4 {
		t.Fatalf("chunk engine received %d messages, want 4", len(received.Messages))
	}
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("chunk engine received reasoning %v, want restored text 'thinking about math'", got)
	}
	if inner.chunkCalls != 1 {
		t.Fatalf("chunkCalls = %d, want 1 (the chunk path, not the text bridge)", inner.chunkCalls)
	}
}

func TestM66bChunkStreamFromChunksInjectsBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	se, inner := m66bSeedReasoning(t, ctx)
	inner.fromChunksResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}

	// Delegated resume path (wrapped engine implements the chunk
	// resumer): injection happens before the delegation.
	if _, err := se.ChatStreamFromChunks(ctx, m66FollowUpReq("tenant-a"), 0, func(provider.StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamFromChunks: %v", err)
	}
	received := inner.receivedFromChunks()
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("chunk resumer received reasoning %v, want restored text 'thinking about math'", got)
	}
	if inner.fromChunksCalls != 1 {
		t.Fatalf("fromChunksCalls = %d, want 1", inner.fromChunksCalls)
	}
}

func TestM66bTextStreamFromInjectsBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	se, inner := m66bSeedReasoning(t, ctx)
	inner.fromResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}

	// Text resume path (a client reconnect asking to resume from
	// an offset): the wrapped engine implements server.StreamResumer
	// and the cached reasoning must be restored onto the request
	// before the delegation — the same guarantee the chunk tests
	// pin for ChatStreamChunks / ChatStreamFromChunks. Until this
	// test existed, the injectReasoning call in ChatStreamFrom
	// (degrade.go, 4c209cb) was unpinned: 17/17 tests passed with
	// it removed (V-B232 Toggle B).
	if _, err := se.ChatStreamFrom(ctx, m66FollowUpReq("tenant-a"), 0, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	received := inner.receivedFrom()
	if len(received.Messages) != 4 {
		t.Fatalf("text resumer received %d messages, want 4", len(received.Messages))
	}
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("text resumer received reasoning %v, want restored text 'thinking about math'", got)
	}
	if inner.fromCalls != 1 {
		t.Fatalf("fromCalls = %d, want 1 (the text resume path, not a fallback)", inner.fromCalls)
	}
}

func TestM66bChunkStreamFromChunksFallbackInjectsBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	// A text-only inner engine (no chunk methods): the wrapper
	// replays via ChatStreamChunks' bridge, and injection must
	// still have happened before that replay.
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
		t.Fatalf("seed Chat: %v", err)
	}
	f.streamResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
	if _, err := se.ChatStreamFromChunks(ctx, m66FollowUpReq("tenant-a"), 0, func(provider.StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamFromChunks fallback: %v", err)
	}
	// The fallback reaches the inner engine through ChatStream.
	f.mu.Lock()
	received := f.lastStreamReq
	f.mu.Unlock()
	got := received.Messages[1].Reasoning
	if got == nil || got.ReasoningText() != "thinking about math" {
		t.Fatalf("fallback stream received reasoning %v, want restored text 'thinking about math'", got)
	}
}
