package degrade

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Lossy-compression × semantic-cache interaction (CHECKLIST
// 193-194) — the bug the 183-184 smoke found: the gateway's
// semantic cache is keyed on the last user message's text, so a
// response produced from a LOSSY-trimmed context could be served
// to a later request with the same last user message but a
// different (full) context — a trimmed-context answer presented
// as a full-context answer.
//
// Fix (exclusion, not fingerprint scoping): a lossy-compressed
// request's response NEVER enters, and is NEVER served from, the
// semantic layer. A fingerprint would have to cover the whole
// (compressed) prompt — at which point a semantic hit needs exact
// fingerprint equality and the layer degenerates into a second
// exact-match cache for lossy requests, with a new keying scheme
// and new failure modes, for no benefit: lossy requests already
// trade answer fidelity for tokens, and the exact layer still
// serves identical lossy prompts. Exclusion is fail-closed and
// auditable: one predicate (lossyCompressed), two call sites
// (lookup, store). Lossless compression keeps sharing — the
// lossless steps are exactly the canonical normal form
// (compress.VerifyLossless), so the meaning is unchanged.

const (
	lossyLastUser = "What is 2+2?"
	lossyModel    = "gate-model"
)

// lossyHistory is a long conversation whose last user message is
// lossyLastUser: the middle turns are what lossy trimming drops.
func lossyHistory() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Tell me a long story about the first expedition to the moon, in great detail."},
		{Role: "assistant", Content: "Once upon a time, a long time ago, people dreamed of the moon..."},
		{Role: "user", Content: "Continue the story with the landing itself, every detail you know."},
		{Role: "assistant", Content: "The landing was a careful descent through the thin lunar sky..."},
		{Role: "user", Content: lossyLastUser},
	}
}

// lossyTrimmed is lossyHistory after lossy trimming: the middle
// turns are dropped, the system message and the last user message
// are pinned and kept. This is what the server handler hands the
// wrapper on the production path (handleChatCompletions applies
// compression before the engine call), so the wrapper sees the
// trimmed messages plus the stamp — never the original history.
func lossyTrimmed() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: lossyLastUser},
	}
}

// lossyReqA is request A of the repro: the long history, answered
// from a lossy-trimmed context. variant "stamp" simulates the
// server path (the handler compressed before the wrapper saw the
// request: trimmed messages + a stamp saying lossy ran);
// variant "optin" simulates a direct engine caller (compression
// happens inside the engine, after this wrapper's lookup, so the
// wrapper sees the full history and only the opt-in). Both must
// be excluded from the semantic layer.
//
// NOTE on the "optin" variant and the exact layer: the wrapper
// keys the exact cache on the messages IT saw. On the server
// path those are the trimmed messages, so a full-context request
// can never collide. A direct-engine caller hands the wrapper
// the uncompressed history, so its exact entry is keyed on the
// full history — that path never wraps an engine in production
// (only cmd/codvian-router serve mode builds a StaleEngine), so
// no production request can take it; the opt-in signal is
// defence-in-depth for the semantic layer only, and the opt-in
// test below asserts semantic invariants only, with B's messages
// differing from A's so the exact layer cannot interfere.
func lossyReqA(variant string) provider.ChatRequest {
	switch variant {
	case "stamp":
		return provider.ChatRequest{
			Model: lossyModel, Messages: lossyTrimmed(),
			Compressed: &provider.CompressionResult{Applied: true, Lossless: true, LossyApplied: true, LossyMaxTokens: 40},
		}
	case "optin":
		return provider.ChatRequest{
			Model: lossyModel, Messages: lossyHistory(),
			Compression: &provider.CompressionRequest{LossyMaxPromptTokens: 40},
		}
	default:
		panic("unknown variant " + variant)
	}
}

// lossyReqB is request B of the repro: the SAME long history, the
// SAME last user message, the SAME model — but a full context,
// no compression at all. It must get a fresh provider call, never
// A's trimmed-context answer.
func lossyReqB() provider.ChatRequest {
	return provider.ChatRequest{Model: lossyModel, Messages: lossyHistory()}
}

// TestSemanticCacheNeverServesLossyResponse is the exact 183-184
// repro through the wrapper, on the production (server) path: a
// lossy request is answered from its trimmed context; a later
// full-context request with the same last user message must go to
// the provider and get the full-context answer — never the
// trimmed one.
func TestSemanticCacheNeverServesLossyResponse(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "TRIMMED-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, sem, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	resp, err := se.Chat(ctx, lossyReqA("stamp"))
	if err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	if resp == nil || resp.Content != "TRIMMED-ANSWER" {
		t.Fatalf("lossy Chat returned %+v, want TRIMMED-ANSWER", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("lossy Chat calls = %d, want 1", f.chatCalls)
	}
	if sem.Size() != 0 {
		t.Fatalf("semantic cache size = %d after a lossy store, want 0 (never stored)", sem.Size())
	}

	// Same history, same last user message, full context: the
	// exact key differs (different messages — trimmed vs full),
	// so only the semantic layer could wrongly hit — and it must
	// not.
	f.chatResp = &provider.ChatResponse{
		Content: "FULL-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
	}
	resp, err = se.Chat(ctx, lossyReqB())
	if err != nil {
		t.Fatalf("full-context Chat: %v", err)
	}
	if resp == nil || resp.Content != "FULL-ANSWER" {
		t.Fatalf("full-context Chat returned %+v, want FULL-ANSWER (never the trimmed answer)", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("full-context Chat calls = %d, want 2 (provider must be called)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a full-context request, want no hit", kind)
	}
	if h, _ := m.get("semantic"); h != 0 {
		t.Fatalf("semantic cache hits = %d, want 0", h)
	}
}

// TestSemanticCacheNeverStoresLossyOptInResponse pins the second
// lossy signal: a request carrying only the lossy opt-in (no
// applied stamp — the direct-engine-caller shape) is excluded
// from the semantic layer too, on both the store side (nothing
// is stored) and the lookup side (a later request with the same
// last user message gets no semantic hit). B's messages differ
// from A's by one system line, so the exact layer cannot
// interfere with the assertion — this test is about the semantic
// layer only (see the lossyReqA note on the exact layer).
func TestSemanticCacheNeverStoresLossyOptInResponse(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "TRIMMED-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, sem, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqA("optin")); err != nil {
		t.Fatalf("lossy-optin Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("lossy-optin Chat calls = %d, want 1", f.chatCalls)
	}
	if sem.Size() != 0 {
		t.Fatalf("semantic cache size = %d after a lossy-optin store, want 0 (never stored)", sem.Size())
	}

	// Same last user message, different history (extra system
	// line, so the exact key differs too): must go to the
	// provider — the semantic layer has nothing to serve, because
	// the lossy response was never stored.
	f.chatResp = &provider.ChatResponse{
		Content: "FULL-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
	}
	reqB := provider.ChatRequest{
		Model:    lossyModel,
		Messages: append([]provider.Message{{Role: "system", Content: "Be thorough."}}, lossyHistory()...),
	}
	resp, err := se.Chat(ctx, reqB)
	if err != nil {
		t.Fatalf("full-context Chat: %v", err)
	}
	if resp == nil || resp.Content != "FULL-ANSWER" {
		t.Fatalf("full-context Chat returned %+v, want FULL-ANSWER", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("full-context Chat calls = %d, want 2 (provider must be called)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a full-context request, want no hit", kind)
	}
	if h, _ := m.get("semantic"); h != 0 {
		t.Fatalf("semantic cache hits = %d, want 0", h)
	}
}

// TestSemanticCacheLosslessSharingStillWorks proves the exclusion
// did not kill the layer for the case that IS safe: lossless
// compression changes only exact-duplicate system blocks and
// redundant whitespace (the canonical normal form), so a
// losslessly-compressed request's answer is the one the
// uncompressed prompt would have produced — and a later
// full-context request with the same last user message is still
// served it semantically.
func TestSemanticCacheLosslessSharingStillWorks(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10},
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	// Request A: duplicate system block + redundant whitespace,
	// lossless opt-in, stamp says lossless applied, lossy NOT
	// applied — the response is stored semantically.
	lossless := true
	reqA := provider.ChatRequest{
		Model: lossyModel,
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful.   \n\n\nBe concise."},
			{Role: "system", Content: "You are helpful.   \n\n\nBe concise."},
			{Role: "user", Content: lossyLastUser},
		},
		Compression: &provider.CompressionRequest{Lossless: &lossless},
		Compressed:  &provider.CompressionResult{Applied: true, Lossless: true, Verified: true},
	}
	if _, err := se.Chat(ctx, reqA); err != nil {
		t.Fatalf("lossless Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("lossless Chat calls = %d, want 1", f.chatCalls)
	}

	// Request B: the same prompt already in normal form (single
	// system block, clean whitespace), no compression asked —
	// different messages (exact misses), same last user text and
	// model: the semantic layer must hit.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	reqB := provider.ChatRequest{
		Model: lossyModel,
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful.\n\nBe concise."},
			{Role: "user", Content: lossyLastUser},
		},
	}
	resp, err := se.Chat(ctx, reqB)
	if err != nil {
		t.Fatalf("full-context Chat: %v", err)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("full-context Chat returned %+v, want 4 (lossless semantic hit)", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("full-context Chat calls = %d, want 1 (no provider call on a lossless semantic hit)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
	if h, _ := m.get("semantic"); h != 1 {
		t.Fatalf("semantic cache hits = %d, want 1", h)
	}
}

// TestExactCacheUnaffectedByLossy pins the other half of the
// contract: the exact-match cache is keyed on the messages as
// sent, so lossy exclusion from the semantic layer changes
// nothing there — an identical lossy repeat still hits exactly,
// and a full-context request with different messages still
// misses exactly.
func TestExactCacheUnaffectedByLossy(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "TRIMMED-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqA("stamp")); err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	// Identical lossy repeat (same messages, same lossy stamp):
	// exact hit — the provider is not called again, and the hit
	// is labelled exact, not semantic.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, lossyReqA("stamp"))
	if err != nil {
		t.Fatalf("lossy repeat Chat: %v", err)
	}
	if resp == nil || resp.Content != "TRIMMED-ANSWER" {
		t.Fatalf("lossy repeat Chat returned %+v, want TRIMMED-ANSWER (exact hit)", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("lossy repeat Chat calls = %d, want 1 (exact hit, no provider call)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "exact" {
		t.Fatalf("lossy repeat LastCacheHit = (%q, %v), want (exact, true)", kind, ok)
	}
	if h, _ := m.get("exact"); h != 1 {
		t.Fatalf("exact cache hits = %d, want 1", h)
	}
	if h, _ := m.get("semantic"); h != 0 {
		t.Fatalf("semantic cache hits = %d, want 0", h)
	}

	// Full-context request with different messages: exact misses
	// (different key) and semantic misses (nothing stored) — the
	// provider is called.
	f.chatResp = &provider.ChatResponse{
		Content: "FULL-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
	}
	f.chatErr = nil
	resp, err = se.Chat(ctx, lossyReqB())
	if err != nil {
		t.Fatalf("full-context Chat: %v", err)
	}
	if resp == nil || resp.Content != "FULL-ANSWER" {
		t.Fatalf("full-context Chat returned %+v, want FULL-ANSWER", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("full-context Chat calls = %d, want 2", f.chatCalls)
	}
}

// TestStaleServeUnaffectedByLossy pins the stale fallback: it is
// the exact cache, so a lossy request is still served stale on
// provider failure when its own exact entry exists, and a
// full-context request is never served the lossy entry stale.
func TestStaleServeUnaffectedByLossy(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "TRIMMED-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	p := DefaultPolicy()
	p.Enabled = true
	se, _, _, _ := wrapSemanticTest(f, p)
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqA("stamp")); err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	// Same lossy request, providers down: served stale from its
	// own exact entry.
	resp, err := se.Chat(ctx, lossyReqA("stamp"))
	if err != nil {
		t.Fatalf("lossy stale Chat: %v", err)
	}
	if resp == nil || resp.Content != "TRIMMED-ANSWER" {
		t.Fatalf("lossy stale Chat returned %+v, want TRIMMED-ANSWER", resp)
	}
	if !se.LastServedStale() {
		t.Fatal("lossy stale Chat not marked stale, want true")
	}
	// Full-context request, providers down: no stale serve — the
	// error surfaces unchanged, never the trimmed answer.
	if _, err := se.Chat(ctx, lossyReqB()); err == nil {
		t.Fatal("full-context Chat with providers down returned no error, want errUpstreamDown")
	} else if err != errUpstreamDown {
		t.Fatalf("full-context Chat error = %v, want errUpstreamDown", err)
	}
	if se.LastServedStale() {
		t.Fatal("full-context request marked stale, want false")
	}
}

// TestSemanticCacheStreamNeverServesLossyResponse: the streaming
// path (ChatStream) has its own proactive lookup — a lossy
// response stored via Chat must not be streamed to a later
// full-context request either.
func TestSemanticCacheStreamNeverServesLossyResponse(t *testing.T) {
	f := &fakeEngine{
		chatResp:     &provider.ChatResponse{Content: "TRIMMED-ANSWER", Model: lossyModel},
		streamResp:   &provider.ChatResponse{Content: "FULL-ANSWER", Model: lossyModel},
		streamDeltas: []string{"FULL-ANSWER"},
	}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqA("stamp")); err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	var deltas []string
	resp, err := se.ChatStream(ctx, lossyReqB(), func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("full-context ChatStream: %v", err)
	}
	if resp == nil || resp.Content != "FULL-ANSWER" {
		t.Fatalf("full-context ChatStream returned %+v, want FULL-ANSWER", resp)
	}
	if len(deltas) != 1 || deltas[0] != "FULL-ANSWER" {
		t.Fatalf("full-context ChatStream deltas = %v, want [FULL-ANSWER]", deltas)
	}
	if f.streamCalls != 1 {
		t.Fatalf("full-context ChatStream calls = %d, want 1 (provider must stream)", f.streamCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a full-context stream, want no hit", kind)
	}
}

// TestSemanticCacheChunkStreamNeverServesLossyResponse: the chunk
// streaming path (ChatStreamChunks) shares the same lookup —
// same rule.
func TestSemanticCacheChunkStreamNeverServesLossyResponse(t *testing.T) {
	f := &fakeEngine{
		chatResp:     &provider.ChatResponse{Content: "TRIMMED-ANSWER", Model: lossyModel},
		streamResp:   &provider.ChatResponse{Content: "FULL-ANSWER", Model: lossyModel},
		streamDeltas: []string{"FULL-ANSWER"},
	}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqA("stamp")); err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	var chunks []provider.StreamChunk
	resp, err := se.ChatStreamChunks(ctx, lossyReqB(), func(c provider.StreamChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("full-context ChatStreamChunks: %v", err)
	}
	if resp == nil || resp.Content != "FULL-ANSWER" {
		t.Fatalf("full-context ChatStreamChunks returned %+v, want FULL-ANSWER", resp)
	}
	if len(chunks) != 1 || chunks[0].Content != "FULL-ANSWER" {
		t.Fatalf("full-context ChatStreamChunks chunks = %+v, want [FULL-ANSWER]", chunks)
	}
	if f.streamCalls != 1 {
		t.Fatalf("full-context ChatStreamChunks calls = %d, want 1 (provider must stream)", f.streamCalls)
	}
}

// TestLossyCompressedPredicate pins the two-signal rule directly:
// stamp alone, opt-in alone, both, neither — plus the negative
// cases (lossless-only stamp/opt-in must NOT exclude).
func TestLossyCompressedPredicate(t *testing.T) {
	lossless := true
	cases := []struct {
		name string
		req  provider.ChatRequest
		want bool
	}{
		{"neither", provider.ChatRequest{}, false},
		{"stamp lossy applied", provider.ChatRequest{Compressed: &provider.CompressionResult{Applied: true, LossyApplied: true}}, true},
		{"stamp lossless only", provider.ChatRequest{Compressed: &provider.CompressionResult{Applied: true, Lossless: true, Verified: true}}, false},
		{"stamp lossy requested but not applied", provider.ChatRequest{Compressed: &provider.CompressionResult{LossyRequested: true}}, false},
		{"opt-in lossy", provider.ChatRequest{Compression: &provider.CompressionRequest{LossyMaxPromptTokens: 40}}, true},
		{"opt-in lossless only", provider.ChatRequest{Compression: &provider.CompressionRequest{Lossless: &lossless}}, false},
		{"opt-in lossy zero budget", provider.ChatRequest{Compression: &provider.CompressionRequest{LossyMaxPromptTokens: 0}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lossyCompressed(tc.req); got != tc.want {
				t.Fatalf("lossyCompressed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSemanticCacheLossyLookupSkippedForLossyRequester pins the
// lookup side independently of the store side: a FULL-context
// response is stored semantically first, and a later LOSSY
// request with the same last user message must still go to the
// provider — symmetric exclusion, so no direction of the mix-up
// is possible. (Serving a full-context answer to a lossy request
// would be harmless in isolation, but the symmetric rule keeps
// the invariant auditable: the semantic layer never sees a lossy
// request at all.)
func TestSemanticCacheLossyLookupSkippedForLossyRequester(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "FULL-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42},
	}}
	se, _, sem, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, lossyReqB()); err != nil {
		t.Fatalf("full-context Chat: %v", err)
	}
	if sem.Size() != 1 {
		t.Fatalf("semantic cache size = %d after a full-context store, want 1", sem.Size())
	}
	// Same history, but now asking for lossy trimming: must go to
	// the provider, even though a full-context entry exists.
	f.chatResp = &provider.ChatResponse{
		Content: "TRIMMED-ANSWER", Model: lossyModel,
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}
	resp, err := se.Chat(ctx, lossyReqA("stamp"))
	if err != nil {
		t.Fatalf("lossy Chat: %v", err)
	}
	if resp == nil || resp.Content != "TRIMMED-ANSWER" {
		t.Fatalf("lossy Chat returned %+v, want TRIMMED-ANSWER (fresh provider call)", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("lossy Chat calls = %d, want 2 (provider must be called)", f.chatCalls)
	}
	if kind, _, ok := se.LastCacheHit(); ok {
		t.Fatalf("LastCacheHit = (%q, true) after a lossy request, want no hit", kind)
	}
	if h, _ := m.get("semantic"); h != 0 {
		t.Fatalf("semantic cache hits = %d, want 0", h)
	}
	// And the lossy response did not replace the full-context
	// entry: the semantic cache still holds exactly the one
	// full-context entry.
	if sem.Size() != 1 {
		t.Fatalf("semantic cache size = %d after a lossy request, want 1 (unchanged)", sem.Size())
	}
}
