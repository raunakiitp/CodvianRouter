// M6.3 truncated-response guard audit tests.
//
// PARITY Area 6 ("Truncated-response guard") was MISSING on a stale note
// claiming store() stores any success. Since M1.4, store()
// (degrade.go:934) refuses finish_reason="length" via
// cache.ShouldStore (degrade.go:951) before writing EITHER layer.
// These tests prove the guard holds on every store path that funnels
// through store() — non-streaming chat, all four streaming entry
// points, and the semantic layer — by behaviour, not by re-testing
// ShouldStore itself:
//
//   - the first call returns the truncated content (serving still
//     works; only storage is refused), and
//   - the exact cache has no entry for the request afterwards, the
//     semantic cache has no entry for its text, and a second
//     identical request with a failing provider errors instead of
//     being served from cache (proactive hit or stale replay).
//
// Guard-removal sensitivity (verified by code inspection, not by
// mutating production code): the ONLY writer of either cache layer
// in this package is store() (degrade.go:972 exact, :984 semantic),
// and its ONLY gate is the ShouldStore call at :951 —
// lookup()/staleFor() read e.exact/e.semantic directly. So if the
// length check in ShouldStore were removed, store() would write the
// entry and every test below would fail at its exact.Get miss
// assertion and at its second-call error assertion. The "stop"
// control tests prove the harness can tell stored from not-stored:
// the same assertions pass for a complete response.
package degrade

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// truncatedResp is a response cut off by max_tokens: content is real
// but incomplete, and the upstream says so with finish_reason="length".
func truncatedResp() *provider.ChatResponse {
	return &provider.ChatResponse{
		Content:      "partial",
		FinishReason: "length",
		Usage:        provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Model:        "test-model",
	}
}

// completeResp is the same shape with a clean finish, for controls.
func completeResp() *provider.ChatResponse {
	return &provider.ChatResponse{
		Content:      "4",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Model:        "test-model",
	}
}

// m63Policy enables both proactive cache serving and stale fallback,
// so a stored entry would be observable on the second call either
// way — a miss under this policy proves nothing was stored.
func m63Policy() Policy {
	return Policy{Enabled: true, AllowStale: true, ServeCache: true}
}

// m63ExactKey is the scoped exact key for testReq().
func m63ExactKey(t *testing.T) string {
	t.Helper()
	key, err := cache.ScopedKey(cache.KeyInput{Model: testReq().Model, Messages: testReq().Messages})
	if err != nil {
		t.Fatalf("ScopedKey: %v", err)
	}
	return key
}

// m63AssertNothingStored fails unless BOTH layers are empty for the
// request: no exact entry under its scoped key, no semantic entry
// for its last user text.
func m63AssertNothingStored(t *testing.T, ctx context.Context, exact *cache.ExactCache, sem *cache.LocalSemanticCache) {
	t.Helper()
	if _, ok := exact.Get(ctx, m63ExactKey(t)); ok {
		t.Fatal("exact cache holds an entry for the truncated response (guard did not refuse the store)")
	}
	if _, ok := sem.GetTextForModel(ctx, "test-model", "What is 2+2?"); ok {
		t.Fatal("semantic cache holds an entry for the truncated response (guard did not refuse the store)")
	}
}

// TestM63TruncatedChatNotStoredExactAndSemantic (M6.3): a truncated
// non-streaming Chat is served once but stored nowhere — neither the
// exact layer nor the semantic layer — so a later identical request
// (and a paraphrase, which would hit semantically) both fail when
// the provider is down instead of serving the truncated answer.
// (Behavioural twin of TestW1TruncatedResponseNotStored, extended
// here with direct probes of both layers.)
func TestM63TruncatedChatNotStoredExactAndSemantic(t *testing.T) {
	f := &fakeEngine{chatResp: truncatedResp()}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if resp == nil || resp.Content != "partial" {
		t.Fatalf("first Chat returned %+v, want the truncated content served once", resp)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	// Provider now fails: nothing stored means no stale serve.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if _, err := se.Chat(ctx, testReq()); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second Chat: err = %v, want the upstream error (truncated answer must not be stored)", err)
	}
	// And no semantic serve either: a paraphrase must also fail.
	if _, err := se.Chat(ctx, capitalReq("What is 2+2 today?")); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("paraphrase Chat: err = %v, want the upstream error (truncated answer must not be in the semantic layer)", err)
	}
}

// TestM63TruncatedChatStreamNotStored (M6.3): a truncated assembled
// stream (degrade.go:1230 store call) is served once but stored
// nowhere — a second stream with a failing provider errors instead
// of replaying the truncated answer as stale.
func TestM63TruncatedChatStreamNotStored(t *testing.T) {
	f := &fakeEngine{streamResp: truncatedResp(), streamDeltas: []string{"partial"}}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	var got []string
	resp, err := se.ChatStream(ctx, testReq(), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("first ChatStream: %v", err)
	}
	if resp == nil || resp.Content != "partial" || resp.FinishReason != "length" {
		t.Fatalf("first ChatStream returned %+v deltas %q, want the truncated response", resp, got)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = nil
	if _, err := se.ChatStream(ctx, testReq(), func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStream: err = %v, want the upstream error (truncated stream must not be stored)", err)
	}
	if f.streamCalls != 2 {
		t.Fatalf("second ChatStream reached the provider %d times, want 2 (a stored entry would have short-circuited it)", f.streamCalls)
	}
}

// TestM63CompleteChatStreamStored (M6.3 control): a complete stream
// IS stored — the second stream is a proactive hit that never calls
// the provider. Proves the truncated-stream test above can tell
// stored from not-stored.
func TestM63CompleteChatStreamStored(t *testing.T) {
	f := &fakeEngine{streamResp: completeResp(), streamDeltas: []string{"4"}}
	se, exact, _, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	if _, err := se.ChatStream(ctx, testReq(), func(string) error { return nil }); err != nil {
		t.Fatalf("first ChatStream: %v", err)
	}
	if _, ok := exact.Get(ctx, m63ExactKey(t)); !ok {
		t.Fatal("complete stream was not stored in the exact cache")
	}
	var got []string
	if _, err := se.ChatStream(ctx, testReq(), func(d string) error {
		got = append(got, d)
		return nil
	}); err != nil {
		t.Fatalf("second ChatStream: %v", err)
	}
	if f.streamCalls != 1 {
		t.Fatalf("second ChatStream called the provider (streamCalls %d, want 1): complete stream was not served from cache", f.streamCalls)
	}
	if len(got) != 1 || got[0] != "4" {
		t.Fatalf("second ChatStream deltas = %q, want [4] replayed from cache", got)
	}
}

// TestM63TruncatedChatStreamChunksNotStored (M6.3): a truncated
// chunk stream (degrade.go:1415 store call, finish_reason arriving
// on the terminal chunk) is served once but stored nowhere.
func TestM63TruncatedChatStreamChunksNotStored(t *testing.T) {
	f := &chunkFakeEngine{
		chunks: []provider.StreamChunk{
			{Content: "partial"},
			{FinishReason: "length"},
		},
		chunkResp: truncatedResp(),
	}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	var got []provider.StreamChunk
	resp, err := se.ChatStreamChunks(ctx, testReq(), func(c provider.StreamChunk) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("first ChatStreamChunks: %v", err)
	}
	if resp == nil || resp.Content != "partial" || resp.FinishReason != "length" {
		t.Fatalf("first ChatStreamChunks returned %+v chunks %v, want the truncated response", resp, got)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	f.chunkResp = nil
	f.chunkErr = errUpstreamDown
	f.chunks = nil
	if _, err := se.ChatStreamChunks(ctx, testReq(), func(provider.StreamChunk) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStreamChunks: err = %v, want the upstream error (truncated chunk stream must not be stored)", err)
	}
}

// TestM63TruncatedChatStreamFromResumerNotStored (M6.3): a truncated
// resume delegated to the wrapped engine's resumer
// (degrade.go:1286 store call) is stored nowhere.
func TestM63TruncatedChatStreamFromResumerNotStored(t *testing.T) {
	inner := &resumingFakeEngine{resumeDeltas: []string{"partial"}}
	inner.streamResp = truncatedResp()
	se, exact, sem, _ := wrapSemanticTest(inner, m63Policy())
	ctx := context.Background()

	resp, err := se.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil })
	if err != nil {
		t.Fatalf("first ChatStreamFrom: %v", err)
	}
	if resp == nil || resp.FinishReason != "length" {
		t.Fatalf("first ChatStreamFrom returned %+v, want the truncated response", resp)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	inner.streamResp = nil
	inner.resumeErr = errUpstreamDown
	inner.resumeDeltas = nil
	if _, err := se.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStreamFrom: err = %v, want the upstream error (truncated resume must not be stored)", err)
	}
}

// TestM63TruncatedChatStreamFromFallbackNotStored (M6.3): a truncated
// resume against an engine with no resumer falls back to ChatStream
// (degrade.go:1319 store call) and is stored nowhere.
func TestM63TruncatedChatStreamFromFallbackNotStored(t *testing.T) {
	f := &fakeEngine{streamResp: truncatedResp(), streamDeltas: []string{"partial"}}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	resp, err := se.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil })
	if err != nil {
		t.Fatalf("first ChatStreamFrom: %v", err)
	}
	if resp == nil || resp.FinishReason != "length" {
		t.Fatalf("first ChatStreamFrom returned %+v, want the truncated response", resp)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = nil
	if _, err := se.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStreamFrom: err = %v, want the upstream error (truncated fallback resume must not be stored)", err)
	}
}

// TestM63TruncatedChatStreamFromChunksDelegatedNotStored (M6.3): a
// truncated chunk resume delegated to the wrapped engine's chunk
// resumer (degrade.go:1456 store call) is stored nowhere.
func TestM63TruncatedChatStreamFromChunksDelegatedNotStored(t *testing.T) {
	f := &chunkFakeEngine{
		chunks: []provider.StreamChunk{
			{Content: "partial"},
			{FinishReason: "length"},
		},
		chunkResp: truncatedResp(),
	}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	resp, err := se.ChatStreamFromChunks(ctx, testReq(), 0, func(provider.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("first ChatStreamFromChunks: %v", err)
	}
	if resp == nil || resp.FinishReason != "length" {
		t.Fatalf("first ChatStreamFromChunks returned %+v, want the truncated response", resp)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	f.chunkResp = nil
	f.chunkErr = errUpstreamDown
	f.chunks = nil
	if _, err := se.ChatStreamFromChunks(ctx, testReq(), 0, func(provider.StreamChunk) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStreamFromChunks: err = %v, want the upstream error (truncated chunk resume must not be stored)", err)
	}
}

// m63ChunkOnlyEngine streams chunks but implements no chunk resumer,
// so ChatStreamFromChunks must take the chatStreamChunksInner
// fallback (degrade.go:1498 store call).
type m63ChunkOnlyEngine struct {
	fakeEngine
	chunks     []provider.StreamChunk
	chunkResp  *provider.ChatResponse
	chunkErr   error
	chunkCalls int
}

func (f *m63ChunkOnlyEngine) ChatStreamChunks(_ context.Context, _ provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.chunkCalls++
	chunks := append([]provider.StreamChunk(nil), f.chunks...)
	resp, err := f.chunkResp, f.chunkErr
	f.mu.Unlock()
	for _, c := range chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	return resp, err
}

// TestM63TruncatedChatStreamFromChunksFallbackNotStored (M6.3): a
// truncated chunk resume against an engine with chunks but no chunk
// resumer falls back through chatStreamChunksInner
// (degrade.go:1498 store call) and is stored nowhere.
func TestM63TruncatedChatStreamFromChunksFallbackNotStored(t *testing.T) {
	f := &m63ChunkOnlyEngine{
		chunks: []provider.StreamChunk{
			{Content: "partial"},
			{FinishReason: "length"},
		},
		chunkResp: truncatedResp(),
	}
	se, exact, sem, _ := wrapSemanticTest(f, m63Policy())
	ctx := context.Background()

	resp, err := se.ChatStreamFromChunks(ctx, testReq(), 0, func(provider.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("first ChatStreamFromChunks: %v", err)
	}
	if resp == nil || resp.FinishReason != "length" {
		t.Fatalf("first ChatStreamFromChunks returned %+v, want the truncated response", resp)
	}
	m63AssertNothingStored(t, ctx, exact, sem)

	f.chunkResp = nil
	f.chunkErr = errUpstreamDown
	f.chunks = nil
	if _, err := se.ChatStreamFromChunks(ctx, testReq(), 0, func(provider.StreamChunk) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second ChatStreamFromChunks: err = %v, want the upstream error (truncated chunk fallback resume must not be stored)", err)
	}
}
