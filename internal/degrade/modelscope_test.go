package degrade

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Model scoping (CHECKLIST 187) — the bug this slice fixes, found
// live by the 163-164 smoke: the semantic vector is text-only, so
// a cached claude-sonnet response was served for a gpt-4o request
// in 0ms. Every cache path (exact / semantic / stale / stream /
// chunk-stream) must serve a cached response only to a request
// naming the same model.

const (
	scopeClaude = "claude-sonnet-4-5"
	scopeGPT    = "gpt-4o"
	scopeText   = "Describe the monsoon season in Mumbai."
)

func scopeReq(model, text string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: text}},
	}
}

// TestSemanticCacheNeverServesCrossModel is the exact 163-164
// repro through the engine: a claude-sonnet request is answered
// and cached; an identical-text gpt-4o request must go to the
// provider (chatCalls moves, content is the gpt-4o answer), never
// be served the claude response from the semantic cache.
func TestSemanticCacheNeverServesCrossModel(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Sunny.", Model: scopeClaude,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText)); err != nil {
		t.Fatalf("claude Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("claude Chat calls = %d, want 1", f.chatCalls)
	}

	// Same messages, different model: the exact key already
	// differs, so only the semantic layer could wrongly hit.
	f.chatResp = &provider.ChatResponse{
		Content: "GPT sunny.", Model: scopeGPT,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}
	resp, err := se.Chat(ctx, scopeReq(scopeGPT, scopeText))
	if err != nil {
		t.Fatalf("gpt-4o Chat: %v", err)
	}
	if resp == nil || resp.Content != "GPT sunny." {
		t.Fatalf("gpt-4o Chat returned %+v, want GPT sunny.", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("gpt-4o Chat calls = %d, want 2 (provider must be called)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a cross-model request, want no hit", kind)
	}
	if h, _ := m.get("semantic"); h != 0 {
		t.Fatalf("semantic cache hits = %d, want 0", h)
	}
}

// TestSemanticCacheSameModelStillHits proves the fix did not kill
// the layer: the same model, same text, is still served from the
// semantic cache without calling the provider.
func TestSemanticCacheSameModelStillHits(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Sunny.", Model: scopeClaude,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText)); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	// Different history, same last user text: exact misses,
	// semantic must hit for the same model.
	req := provider.ChatRequest{
		Model: scopeClaude,
		Messages: []provider.Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: scopeText},
		},
	}
	resp, err := se.Chat(ctx, req)
	if err != nil {
		t.Fatalf("same-model Chat: %v", err)
	}
	if resp == nil || resp.Content != "Sunny." {
		t.Fatalf("same-model Chat returned %+v, want Sunny.", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("same-model Chat calls = %d, want 1 (no provider call on a hit)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
	if h, _ := m.get("semantic"); h != 1 {
		t.Fatalf("semantic cache hits = %d, want 1", h)
	}
}

// TestStaleServeNeverServesCrossModel: with every provider down,
// a gpt-4o request must NOT be served the stale claude-sonnet
// entry — the stale path is the exact cache under
// ExactKey(req.Model, messages), so it is scoped by construction,
// and this test pins it. ServeCache is OFF here on purpose: with
// it on, a same-model repeat would be a proactive exact hit and
// never reach the stale fallback, so the stale path itself could
// not be exercised.
func TestStaleServeNeverServesCrossModel(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Sunny.", Model: scopeClaude,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	p := DefaultPolicy()
	p.Enabled = true
	se, _, _, _ := wrapSemanticTest(f, p)
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText)); err != nil {
		t.Fatalf("claude Chat: %v", err)
	}
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	// Same messages, other model, providers down: the error must
	// surface unchanged — never a stale answer from another model.
	if _, err := se.Chat(ctx, scopeReq(scopeGPT, scopeText)); err == nil {
		t.Fatal("cross-model Chat with providers down returned no error, want errUpstreamDown")
	} else if err != errUpstreamDown {
		t.Fatalf("cross-model Chat error = %v, want errUpstreamDown", err)
	}
	if se.LastServedStale() {
		t.Fatal("cross-model request marked stale, want false")
	}
	// The same-model request IS served stale.
	resp, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText))
	if err != nil {
		t.Fatalf("same-model stale Chat: %v", err)
	}
	if resp == nil || resp.Content != "Sunny." {
		t.Fatalf("same-model stale Chat returned %+v, want Sunny.", resp)
	}
	if !se.LastServedStale() {
		t.Fatal("same-model stale Chat not marked stale, want true")
	}
}

// TestSemanticCacheStreamNeverServesCrossModel: the streaming path
// (ChatStream) has its own proactive lookup — it must miss across
// models and stream the provider's answer instead.
func TestSemanticCacheStreamNeverServesCrossModel(t *testing.T) {
	f := &fakeEngine{
		chatResp:     &provider.ChatResponse{Content: "Sunny.", Model: scopeClaude},
		streamResp:   &provider.ChatResponse{Content: "Sunny.", Model: scopeClaude},
		streamDeltas: []string{"Sunny."},
	}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText)); err != nil {
		t.Fatalf("claude Chat: %v", err)
	}
	f.streamResp = &provider.ChatResponse{Content: "GPT sunny.", Model: scopeGPT}
	f.streamDeltas = []string{"GPT sunny."}
	var deltas []string
	resp, err := se.ChatStream(ctx, scopeReq(scopeGPT, scopeText), func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("gpt-4o ChatStream: %v", err)
	}
	if resp == nil || resp.Content != "GPT sunny." {
		t.Fatalf("gpt-4o ChatStream returned %+v, want GPT sunny.", resp)
	}
	if len(deltas) != 1 || deltas[0] != "GPT sunny." {
		t.Fatalf("gpt-4o ChatStream deltas = %v, want [GPT sunny.]", deltas)
	}
	if f.streamCalls != 1 {
		t.Fatalf("gpt-4o ChatStream calls = %d, want 1 (provider must stream)", f.streamCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a cross-model stream, want no hit", kind)
	}
}

// TestSemanticCacheChunkStreamNeverServesCrossModel: the chunk
// streaming path (ChatStreamChunks) shares the same lookup —
// cross-model must miss and the provider's chunks must flow.
func TestSemanticCacheChunkStreamNeverServesCrossModel(t *testing.T) {
	f := &fakeEngine{
		chatResp:     &provider.ChatResponse{Content: "Sunny.", Model: scopeClaude},
		streamResp:   &provider.ChatResponse{Content: "Sunny.", Model: scopeClaude},
		streamDeltas: []string{"Sunny."},
	}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeClaude, scopeText)); err != nil {
		t.Fatalf("claude Chat: %v", err)
	}
	f.streamResp = &provider.ChatResponse{Content: "GPT sunny.", Model: scopeGPT}
	f.streamDeltas = []string{"GPT sunny."}
	var chunks []provider.StreamChunk
	resp, err := se.ChatStreamChunks(ctx, scopeReq(scopeGPT, scopeText), func(c provider.StreamChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("gpt-4o ChatStreamChunks: %v", err)
	}
	if resp == nil || resp.Content != "GPT sunny." {
		t.Fatalf("gpt-4o ChatStreamChunks returned %+v, want GPT sunny.", resp)
	}
	if len(chunks) != 1 || chunks[0].Content != "GPT sunny." {
		t.Fatalf("gpt-4o ChatStreamChunks chunks = %+v, want [GPT sunny.]", chunks)
	}
	if f.streamCalls != 1 {
		t.Fatalf("gpt-4o ChatStreamChunks calls = %d, want 1 (provider must stream)", f.streamCalls)
	}
}

// TestSemanticCacheScopeIsRequestedModelNotUpstreamAlias pins the
// scope rule: the scope is the model the REQUEST named, not the
// model id the response carries. A request for gpt-4o answered by
// upstream gpt-4o-2024-05-13 is stored under gpt-4o: a later
// gpt-4o request hits it, a later gpt-4o-2024-05-13 request does
// not (it names a different model than the request that stored
// the entry — fail-closed, even though the upstream id matches).
func TestSemanticCacheScopeIsRequestedModelNotUpstreamAlias(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Alias answer.", Model: "gpt-4o-2024-05-13",
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, scopeReq(scopeGPT, scopeText)); err != nil {
		t.Fatalf("gpt-4o Chat: %v", err)
	}
	// Same model, different history (exact misses, semantic text
	// identical): hits, because the scope is the requested model.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	paraphraseReq := provider.ChatRequest{
		Model: scopeGPT,
		Messages: []provider.Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: scopeText},
		},
	}
	resp, err := se.Chat(ctx, paraphraseReq)
	if err != nil {
		t.Fatalf("gpt-4o paraphrase Chat: %v", err)
	}
	if resp == nil || resp.Content != "Alias answer." {
		t.Fatalf("gpt-4o paraphrase Chat returned %+v, want Alias answer.", resp)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("gpt-4o paraphrase LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
	// The upstream id as a REQUEST model: misses — the entry is
	// scoped to the model the storing request named, not to the
	// model the response carried.
	f.chatResp = &provider.ChatResponse{
		Content: "Upstream answer.", Model: "gpt-4o-2024-05-13",
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}
	f.chatErr = nil
	aliasReq := paraphraseReq
	aliasReq.Model = "gpt-4o-2024-05-13"
	resp, err = se.Chat(ctx, aliasReq)
	if err != nil {
		t.Fatalf("alias-model Chat: %v", err)
	}
	if resp == nil || resp.Content != "Upstream answer." {
		t.Fatalf("alias-model Chat returned %+v, want Upstream answer.", resp)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("alias-model LastCacheHit = (%q, true), want no hit", kind)
	}
}
