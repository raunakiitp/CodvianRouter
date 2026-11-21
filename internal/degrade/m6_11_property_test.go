// M6.11 — cache correctness property tests, engine level.
//
// PLAN.md M6.11 (exit gate, correctness half): no cross-tenant
// serves, no temperature leaks, no truncated serves — proven
// through the REAL StaleEngine (Chat / ChatStream /
// ChatStreamChunks / ChatStreamFrom) with the existing
// fake-engine harnesses, never by re-proving store() in isolation
// (M6.3 already covers store() directly). Every cached entry here
// comes from a real engine store call; every similarity comes
// from the real EmbedText path via wrapSemanticTest.
//
// Toggle evidence: every property has an inversion control in the
// same test — the same setup with the distinguishing input removed
// (same tenant, same temperature, complete finish) DOES hit and
// DOES serve without a provider call. A test that only ever misses
// proves nothing; the controls prove these tests can tell hit
// from miss.
//
// CLOSED GAPS (2026-11-21, Phase 2/T9 audit): both findings below were
// surfaced by this slice and have since been CLOSED by the M6.11 scoped
// store — LocalSemanticCache records tenant + temperature bucket on
// every entry made through degrade's Scoped APIs, and semanticLookup/
// semanticStore use them, so a cross-tenant paraphrase misses inside
// the layer and a cross-temperature hit never serves. The tests that
// used to demonstrate the leaks now pin the closed behaviour; their
// comments below are kept as history:
//   - TestM611CrossTenantSemanticParaphraseGap: a cross-tenant
//     PARAPHRASE used to be served the other tenant's semantic answer
//     when only exact-text matches were gated (semanticBlocked). The
//     scoped store closed it; the exact-text provenance gate remains
//     as defence in depth for stores without scoped methods.
//   - TestM611TempSemanticCrossTempWithAllowance: with
//     AllowNonZeroTemp on, a temp-0 request used to be served an
//     answer stored from a temp>0 turn. Temperature-bucket scoping
//     (tempBucket on every scoped entry) closed it; the test pins the
//     partition either way. TestM611TempSeparationWithAllowance proves
//     the EXACT layer alone partitions correctly too.
package degrade

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// m611Policy enables proactive cache serving and stale fallback,
// so a stored entry would be observable on the second call either
// way — a miss under this policy proves nothing was stored or
// nothing was served.
func m611Policy() Policy {
	return Policy{Enabled: true, AllowStale: true, ServeCache: true}
}

// m611Req builds a request for one (tenant, model, prompt,
// temperature) combination.
func m611Req(tenant, model, text string, temp float64) provider.ChatRequest {
	return provider.ChatRequest{
		CallerID:    tenant,
		Model:       model,
		Messages:    []provider.Message{{Role: "user", Content: text}},
		Temperature: temp,
	}
}

// m611Resp builds a provider response with the given content and
// finish reason.
func m611Resp(content, finish string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Content:      content,
		FinishReason: finish,
		Usage:        provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Model:        "test-model",
	}
}

// m611ScopedKey is the engine's scoped exact key for req.
func m611ScopedKey(t *testing.T, se *StaleEngine, req provider.ChatRequest) string {
	t.Helper()
	k, err := se.scopedKey(req)
	if err != nil {
		t.Fatalf("scopedKey: %v", err)
	}
	return k
}

// m611FailProvider makes f fail every call with the upstream-down
// error, so any served answer must have come from cache.
func m611FailProvider(f *fakeEngine) {
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = nil
}

// TestM611NoCrossTenantExactChat proves property 1 at the exact
// layer through Chat: an entry stored under tenant A is never
// served to tenant B — not proactively and not as a stale
// fallback — while tenant A keeps hitting it. Parameterised over
// tenant pairs (including anonymous "" vs named).
func TestM611NoCrossTenantExactChat(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-b"},
		{"gk-1", "gk-2"},
		{"", "gk-named"},
	}
	ctx := context.Background()
	for _, pair := range pairs {
		f := &fakeEngine{chatResp: m611Resp("4", "stop")}
		se, exact, _, _ := wrapSemanticTest(f, m611Policy())
		aReq := m611Req(pair[0], "test-model", "What is 2+2?", 0)
		bReq := m611Req(pair[1], "test-model", "What is 2+2?", 0)

		if _, err := se.Chat(ctx, aReq); err != nil {
			t.Fatalf("tenants %q/%q: tenant A store Chat: %v", pair[0], pair[1], err)
		}
		if f.chatCalls != 1 {
			t.Fatalf("tenants %q/%q: store Chat calls = %d, want 1", pair[0], pair[1], f.chatCalls)
		}
		// Control: same tenant hits its own entry without a provider call.
		if resp, err := se.Chat(ctx, aReq); err != nil || resp == nil || resp.Content != "4" {
			t.Fatalf("tenants %q/%q: same-tenant Chat = (%+v, %v), want content 4 from cache", pair[0], pair[1], resp, err)
		}
		if f.chatCalls != 1 {
			t.Fatalf("tenants %q/%q: same-tenant Chat called provider %d times, want 1", pair[0], pair[1], f.chatCalls)
		}
		// The entry is under A's key only, at the layer itself.
		if _, ok := exact.Get(ctx, m611ScopedKey(t, se, aReq)); !ok {
			t.Fatalf("tenants %q/%q: exact cache misses tenant A's own key", pair[0], pair[1])
		}
		if _, ok := exact.Get(ctx, m611ScopedKey(t, se, bReq)); ok {
			t.Fatalf("tenants %q/%q: exact cache holds an entry under tenant B's key", pair[0], pair[1])
		}
		// Property: tenant B, providers down, must fail — neither
		// a proactive hit nor a stale serve may hand it A's answer.
		m611FailProvider(f)
		if resp, err := se.Chat(ctx, bReq); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("tenants %q/%q: tenant B Chat = (%+v, %v), want the upstream error", pair[0], pair[1], resp, err)
		}
		if f.chatCalls != 2 {
			t.Fatalf("tenants %q/%q: tenant B Chat calls = %d, want 2 (B must reach the provider)", pair[0], pair[1], f.chatCalls)
		}
		// And tenant A still hits afterwards: B's miss evicted nothing.
		if resp, err := se.Chat(ctx, aReq); err != nil || resp == nil || resp.Content != "4" {
			t.Fatalf("tenants %q/%q: tenant A Chat after B = (%+v, %v), want content 4 from cache", pair[0], pair[1], resp, err)
		}
		if f.chatCalls != 2 {
			t.Fatalf("tenants %q/%q: tenant A re-hit called provider %d times, want 2", pair[0], pair[1], f.chatCalls)
		}
	}
}

// TestM611NoCrossTenantExactChatStream proves property 1 on the
// streaming path: tenant B's stream with providers down errors
// with zero deltas (no stale replay of A's answer), while tenant
// A's stream replays the stored answer as a single delta without
// calling the provider.
func TestM611NoCrossTenantExactChatStream(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{streamResp: m611Resp("4", "stop"), streamDeltas: []string{"4"}}
	se, _, _, _ := wrapSemanticTest(f, m611Policy())
	aReq := m611Req("tenant-a", "test-model", "What is 2+2?", 0)
	bReq := m611Req("tenant-b", "test-model", "What is 2+2?", 0)

	if _, err := se.ChatStream(ctx, aReq, func(string) error { return nil }); err != nil {
		t.Fatalf("tenant A store ChatStream: %v", err)
	}
	// Control: same tenant replays from cache, provider untouched.
	var got []string
	if _, err := se.ChatStream(ctx, aReq, func(d string) error { got = append(got, d); return nil }); err != nil {
		t.Fatalf("same-tenant ChatStream: %v", err)
	}
	if f.streamCalls != 1 {
		t.Fatalf("same-tenant ChatStream calls = %d, want 1", f.streamCalls)
	}
	if len(got) != 1 || got[0] != "4" {
		t.Fatalf("same-tenant ChatStream deltas = %q, want [4] replayed from cache", got)
	}
	// Property: tenant B errors, delivers nothing.
	m611FailProvider(f)
	var bGot []string
	if _, err := se.ChatStream(ctx, bReq, func(d string) error { bGot = append(bGot, d); return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B ChatStream err = %v, want the upstream error", err)
	}
	if len(bGot) != 0 {
		t.Fatalf("tenant B ChatStream delivered deltas %q: tenant A's answer must never replay to B", bGot)
	}
	if f.streamCalls != 2 {
		t.Fatalf("tenant B ChatStream calls = %d, want 2 (B must reach the provider)", f.streamCalls)
	}
}

// TestM611NoCrossTenantSemanticExactText proves property 1 at the
// semantic layer for identical text: tenant B asking tenant A's
// exact prompt (similarity 1.0) is NOT served A's answer — the
// wrapper's semanticBlocked gate skips the lookup — while the
// layer still holds A's entry (the block is at serve time, not a
// missing store). The same-tenant paraphrase control proves the
// semantic layer itself works in this harness.
func TestM611NoCrossTenantSemanticExactText(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, sem, _ := wrapSemanticTest(f, m611Policy())
	aReq := m611Req("tenant-a", "test-model", "What is the capital of France?", 0)
	bReq := m611Req("tenant-b", "test-model", "What is the capital of France?", 0)

	if _, err := se.Chat(ctx, aReq); err != nil {
		t.Fatalf("tenant A store Chat: %v", err)
	}
	// Control: same-tenant paraphrase hits semantically, no provider call.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France today?", 0))
	if err != nil || resp == nil || resp.Content != "Paris is the capital of France." {
		t.Fatalf("same-tenant paraphrase Chat = (%+v, %v), want the semantic hit", resp, err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("same-tenant paraphrase LastCacheHit kind = %q, want semantic", kind)
	}
	// The layer still holds A's entry: the block below is a serve
	// gate, not an absent store.
	if _, ok := sem.GetTextForModel(ctx, "test-model", "What is the capital of France?"); !ok {
		t.Fatal("semantic layer lost tenant A's entry (want it held; the wrapper blocks serving, not storing)")
	}
	// Property: tenant B, identical text, providers down, must fail.
	if resp, err := se.Chat(ctx, bReq); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B identical-text Chat = (%+v, %v), want the upstream error", resp, err)
	}
}

// TestM611CrossTenantSemanticParaphraseGap is the probe for property 1:
// a cross-tenant PARAPHRASE — similar but not identical text — must
// never be served another tenant's semantic answer. It originally
// demonstrated the leak the wrapper-level provenance gate could not
// close (semanticBlocked gates exact-text matches only); the M6.11
// scoped store (tenant recorded per entry, enforced by semanticLookup)
// closed it, and this test now PINS the closed behaviour: tenant B
// fails with providers down. The exact-text gate stays as defence in
// depth for stores without scoped methods.
func TestM611CrossTenantSemanticParaphraseGap(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, _, _ := wrapSemanticTest(f, m611Policy())
	if _, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France?", 0)); err != nil {
		t.Fatalf("tenant A store Chat: %v", err)
	}
	// Tenant B paraphrases A's prompt. Property 1 says B must
	// never be served A's answer; providers are down, so a miss
	// must surface as the upstream error.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, m611Req("tenant-b", "test-model", "What is the capital of France today?", 0))
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B paraphrase Chat = (%+v, %v), want the upstream error: cross-tenant paraphrase served tenant A's semantic answer (known gap, see file header)", resp, err)
	}
}

// m611ThinkingResp is a thinking tool-call response under callID,
// mirroring m66ThinkingResp with a variable tool-call id.
func m611ThinkingResp(callID string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Content: "calling calc",
		ToolCalls: []provider.ToolCall{
			{ID: callID, Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
		},
		Reasoning: &reasoning.State{
			Blocks:     []reasoning.Block{{Kind: reasoning.KindThought, Text: "thinking about math"}},
			SourceWire: reasoning.WireOpenAI,
		},
		Model: "deepseek-chat",
	}
}

// m611FollowUpReq is the client replaying the thinking turn's
// transcript WITHOUT reasoning, under tenant.
func m611FollowUpReq(tenant, callID string) provider.ChatRequest {
	return provider.ChatRequest{
		CallerID: tenant,
		Model:    "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "calling calc", ToolCalls: []provider.ToolCall{
				{ID: callID, Type: "function", Function: provider.ToolCallFunction{Name: "calc", Arguments: "{}"}},
			}},
			{Role: "tool", ToolCallID: callID, Content: "4"},
			{Role: "user", Content: "and the result is?"},
		},
	}
}

// TestM611NoCrossTenantReasoningReplay proves property 1 for
// reasoning replay: reasoning captured under tenant A (keyed by
// tool-call id + conversation digest, both tenant-scoped) is never
// injected into tenant B's replay of the same transcript —
// including the reused-id case (clients reuse call_0 across
// tenants). The same-tenant control proves injection works in
// this harness.
func TestM611NoCrossTenantReasoningReplay(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-b"},
		{"gk-1", "gk-2"},
		{"", "gk-named"},
	}
	callIDs := []string{"call_0", "call_1", "call_42"}
	ctx := context.Background()
	for _, pair := range pairs {
		for _, callID := range callIDs {
			f := &m66CapturingEngine{}
			se, _ := wrapTest(f, DefaultPolicy())
			rc := cache.NewReasoningCache()
			se.SetReasoningCache(rc)

			f.chatResp = m611ThinkingResp(callID)
			turn1 := m611Req(pair[0], "deepseek-chat", "what is 2+2?", 0)
			if _, err := se.Chat(ctx, turn1); err != nil {
				t.Fatalf("tenants %q/%q call %q: turn 1 Chat: %v", pair[0], pair[1], callID, err)
			}
			if st := se.ReasoningCacheStats(); st.Entries == 0 {
				t.Fatalf("tenants %q/%q call %q: reasoning cache empty after a thinking response", pair[0], pair[1], callID)
			}
			// Control: same tenant's replay gets the reasoning back.
			f.chatResp = &provider.ChatResponse{Content: "the result is 4", Model: "deepseek-chat"}
			if _, err := se.Chat(ctx, m611FollowUpReq(pair[0], callID)); err != nil {
				t.Fatalf("tenants %q/%q call %q: same-tenant follow-up Chat: %v", pair[0], pair[1], callID, err)
			}
			if got := f.receivedChat().Messages[1].Reasoning; got == nil || got.ReasoningText() != "thinking about math" {
				t.Fatalf("tenants %q/%q call %q: same-tenant replay received reasoning %v, want 'thinking about math'", pair[0], pair[1], callID, got)
			}
			// Property: the other tenant's identical replay gets nothing injected.
			if _, err := se.Chat(ctx, m611FollowUpReq(pair[1], callID)); err != nil {
				t.Fatalf("tenants %q/%q call %q: other-tenant follow-up Chat: %v", pair[0], pair[1], callID, err)
			}
			if got := f.receivedChat().Messages[1].Reasoning; got != nil && !got.Empty() {
				t.Fatalf("tenants %q/%q call %q: other tenant's replay received tenant A's reasoning %q", pair[0], pair[1], callID, got.ReasoningText())
			}
		}
	}
}

// TestM611TempNeverStoredWithoutAllowance proves property 2 under
// the default rule (AllowNonZeroTemp=false): a temp>0 response is
// never stored — on the Chat path or the ChatStream path — so a
// later identical request with providers down fails instead of
// being served the hot answer, and a temp-0 request can never
// receive it either (nothing was stored to receive). Parameterised
// over temperatures including the 0.0001 boundary the key
// bucketing cannot see.
func TestM611TempNeverStoredWithoutAllowance(t *testing.T) {
	temps := []float64{0.0001, 0.1, 0.7, 1.0, 2.0}
	ctx := context.Background()
	for _, temp := range temps {
		f := &fakeEngine{chatResp: m611Resp("hot-answer", "stop")}
		se, exact, sem, _ := wrapSemanticTest(f, m611Policy())
		req := m611Req("tenant-a", "test-model", "What is 2+2?", temp)
		resp, err := se.Chat(ctx, req)
		if err != nil || resp == nil || resp.Content != "hot-answer" {
			t.Fatalf("temp %v: first Chat = (%+v, %v), want the hot answer served once", temp, resp, err)
		}
		// Nothing stored in either layer.
		if _, ok := exact.Get(ctx, m611ScopedKey(t, se, req)); ok {
			t.Fatalf("temp %v: exact cache holds a temp>0 entry", temp)
		}
		if _, ok := sem.GetTextForModel(ctx, "test-model", "What is 2+2?"); ok {
			t.Fatalf("temp %v: semantic cache holds a temp>0 entry", temp)
		}
		// Second identical request, providers down: must fail.
		m611FailProvider(f)
		if _, err := se.Chat(ctx, req); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("temp %v: second Chat err = %v, want the upstream error (hot answer must not be stored)", temp, err)
		}
		// And a temp-0 request for the same prompt also fails:
		// there is no hot entry for it to be served.
		if _, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0)); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("temp %v: temp-0 Chat err = %v, want the upstream error", temp, err)
		}
	}
	// Streaming path, temp 0.7: assembled stream stored nowhere.
	f := &fakeEngine{streamResp: m611Resp("hot-answer", "stop"), streamDeltas: []string{"hot-answer"}}
	se, exact, sem, _ := wrapSemanticTest(f, m611Policy())
	req := m611Req("tenant-a", "test-model", "What is 2+2?", 0.7)
	if _, err := se.ChatStream(ctx, req, func(string) error { return nil }); err != nil {
		t.Fatalf("temp 0.7 stream: first ChatStream: %v", err)
	}
	if _, ok := exact.Get(ctx, m611ScopedKey(t, se, req)); ok {
		t.Fatal("temp 0.7 stream: exact cache holds a temp>0 streamed entry")
	}
	if _, ok := sem.GetTextForModel(ctx, "test-model", "What is 2+2?"); ok {
		t.Fatal("temp 0.7 stream: semantic cache holds a temp>0 streamed entry")
	}
	m611FailProvider(f)
	if _, err := se.ChatStream(ctx, req, func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("temp 0.7 stream: second ChatStream err = %v, want the upstream error", err)
	}
	// Control: temp 0 DOES store, and the second Chat is an exact
	// hit that never calls the provider.
	f0 := &fakeEngine{chatResp: m611Resp("cold-answer", "stop")}
	se0, _, _, _ := wrapSemanticTest(f0, m611Policy())
	if _, err := se0.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0)); err != nil {
		t.Fatalf("temp 0 control: first Chat: %v", err)
	}
	m611FailProvider(f0)
	resp, err := se0.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0))
	if err != nil || resp == nil || resp.Content != "cold-answer" {
		t.Fatalf("temp 0 control: second Chat = (%+v, %v), want the stored cold answer", resp, err)
	}
	if f0.chatCalls != 1 {
		t.Fatalf("temp 0 control: chatCalls = %d, want 1 (exact hit)", f0.chatCalls)
	}
}

// TestM611TempZeroNeverServedHotEntry proves the other direction
// of property 2 under the default rule: a temp>0 request is never
// served a temp-0 cached answer — the lookup gate refuses to
// consult the cache at all — and a temp-0 request is never served
// a temp>0 answer (nothing hot is stored, see
// TestM611TempNeverStoredWithoutAllowance). The control proves a
// temp-0 request DOES hit its own temp-0 entry in this harness.
func TestM611TempZeroNeverServedHotEntry(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: m611Resp("cold-answer", "stop")}
	se, _, _, _ := wrapSemanticTest(f, m611Policy())
	cold := m611Req("tenant-a", "test-model", "What is 2+2?", 0)
	hot := m611Req("tenant-a", "test-model", "What is 2+2?", 0.7)
	if _, err := se.Chat(ctx, cold); err != nil {
		t.Fatalf("store temp-0 Chat: %v", err)
	}
	// Control: temp-0 hits its own entry without a provider call.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if resp, err := se.Chat(ctx, cold); err != nil || resp == nil || resp.Content != "cold-answer" {
		t.Fatalf("temp-0 control Chat = (%+v, %v), want the stored cold answer", resp, err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("temp-0 control chatCalls = %d, want 1", f.chatCalls)
	}
	// Property: the temp>0 request for the same prompt must fail,
	// not be served the cold answer.
	if resp, err := se.Chat(ctx, hot); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("temp>0 Chat = (%+v, %v), want the upstream error (hot request must not be served a cold answer)", resp, err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("temp>0 Chat calls = %d, want 2 (hot request must reach the provider)", f.chatCalls)
	}
}

// TestM611TempSeparationWithAllowance proves the exact layer's
// temperature partitioning when the per-key allowance IS on:
// a temp-0.7 answer is stored and served to temp-0.7 requests,
// but a temp-0 request is never served it (different temperature
// bucket, different key) and vice versa. It runs on the
// EXACT-ONLY harness (wrapTest, no semantic layer) on purpose:
// the semantic layer has no temperature dimension, so a
// semantic-enabled engine would serve the hot answer to a temp-0
// request regardless of the exact key — that behaviour is the
// separate probe TestM611TempSemanticCrossTempWithAllowance, not
// this test. It also pins the bucket semantics: 0.704 shares
// with 0.700, 0.70 does not share with 0.71.
func TestM611TempSeparationWithAllowance(t *testing.T) {
	ctx := context.Background()
	pol := m611Policy()
	pol.AllowNonZeroTemp = true
	f := &fakeEngine{chatResp: m611Resp("hot-answer", "stop")}
	se, exact := wrapTest(f, pol)
	hot := m611Req("tenant-a", "test-model", "What is 2+2?", 0.7)
	cold := m611Req("tenant-a", "test-model", "What is 2+2?", 0)
	if _, err := se.Chat(ctx, hot); err != nil {
		t.Fatalf("store temp-0.7 Chat: %v", err)
	}
	// Layer probe: the hot entry is under the hot key only.
	if _, ok := exact.Get(ctx, m611ScopedKey(t, se, hot)); !ok {
		t.Fatal("exact cache misses the temp-0.7 key after storing it")
	}
	if _, ok := exact.Get(ctx, m611ScopedKey(t, se, cold)); ok {
		t.Fatal("exact cache holds the hot answer under the temp-0 key")
	}
	// Control: same temp hits its own entry without a provider call.
	if resp, err := se.Chat(ctx, hot); err != nil || resp == nil || resp.Content != "hot-answer" {
		t.Fatalf("same-temp Chat = (%+v, %v), want the stored hot answer", resp, err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("same-temp chatCalls = %d, want 1", f.chatCalls)
	}
	// Property: temp-0 is NOT served the hot answer — it goes
	// upstream and gets its own (cold) answer.
	f.chatResp = m611Resp("cold-answer", "stop")
	f.chatErr = nil
	if resp, err := se.Chat(ctx, cold); err != nil || resp == nil || resp.Content != "cold-answer" {
		t.Fatalf("temp-0 Chat = (%+v, %v), want the fresh cold answer, not the stored hot one", resp, err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("temp-0 Chat calls = %d, want 2 (cold request must reach the provider)", f.chatCalls)
	}
	// And the reverse: the hot request is served its OWN stored
	// hot answer — not the cold answer, and without a provider
	// call. (A same-temperature repeat is a legitimate exact hit;
	// the property is only that it never receives the other
	// temperature's answer.)
	if resp, err := se.Chat(ctx, hot); err != nil || resp == nil || resp.Content != "hot-answer" {
		t.Fatalf("temp-0.7 Chat after cold store = (%+v, %v), want its own stored hot answer, never the cold one", resp, err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("temp-0.7 Chat after cold store calls = %d, want 2 (own-temperature exact hit)", f.chatCalls)
	}
	// Bucket semantics: 0.704 shares 0.700's entry, 0.71 does not
	// share 0.70's.
	f2 := &fakeEngine{chatResp: m611Resp("bucket-answer", "stop")}
	se2, _ := wrapTest(f2, pol)
	if _, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0.700)); err != nil {
		t.Fatalf("store temp-0.700 Chat: %v", err)
	}
	if _, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0.704)); err != nil {
		t.Fatalf("temp-0.704 Chat: %v", err)
	}
	if f2.chatCalls != 1 {
		t.Fatalf("0.704 after 0.700 chatCalls = %d, want 1 (same bucket shares)", f2.chatCalls)
	}
	f2.chatResp = m611Resp("other-bucket", "stop")
	if resp, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0.71)); err != nil || resp == nil || resp.Content != "other-bucket" {
		t.Fatalf("temp-0.71 Chat = (%+v, %v), want a fresh answer (different bucket)", resp, err)
	}
	if f2.chatCalls != 2 {
		t.Fatalf("0.71 after 0.700 chatCalls = %d, want 2 (different bucket must reach the provider)", f2.chatCalls)
	}
}

// TestM611TempSemanticCrossTempWithAllowance is the KNOWN-GAP
// probe for property 2 under the allowance (see the file header):
// the exact layer partitions by temperature bucket, but the
// semantic layer has no temperature dimension, so a temp-0
// request is served an answer stored from a temp>0 turn — for
// the IDENTICAL prompt as well as for a paraphrase. Each case
// uses t.Errorf (not Fatalf) so one run records both
// manifestations of the single root cause. This test expects the
// property (temp-0 requests must fail with providers down) and
// FAILS against current production code. Do not weaken it: the
// coordinator decides whether the allowance accepts
// cross-temperature semantic sharing or the semantic layer needs
// temperature scoping.
func TestM611TempSemanticCrossTempWithAllowance(t *testing.T) {
	ctx := context.Background()
	pol := m611Policy()
	pol.AllowNonZeroTemp = true
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, _, _ := wrapSemanticTest(f, pol)
	hot := m611Req("tenant-a", "test-model", "What is the capital of France?", 0.7)
	if _, err := se.Chat(ctx, hot); err != nil {
		t.Fatalf("store temp-0.7 Chat: %v", err)
	}
	// Temp-0 requests for the hot turn, providers down: property
	// 2 says both must fail, not be served the hot answer.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if resp, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France?", 0)); !errors.Is(err, errUpstreamDown) {
		t.Errorf("identical-text temp-0 Chat = (%+v, %v), want the upstream error: temp-0 request served a temp>0 semantic answer (known gap, see file header)", resp, err)
	}
	if resp, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France today?", 0)); !errors.Is(err, errUpstreamDown) {
		t.Errorf("paraphrase temp-0 Chat = (%+v, %v), want the upstream error: temp-0 request served a temp>0 semantic answer (known gap, see file header)", resp, err)
	}
}

// TestM611TruncatedNeverStoredEnginePaths proves property 3
// through the real engine on every store path: a response with
// finish_reason "length" (in any case spelling — the gate is
// case-insensitive) is served once but stored nowhere, on Chat,
// ChatStream, ChatStreamChunks, and the ChatStreamFrom fallback —
// so a second identical request with providers down fails instead
// of serving the truncated answer. The "stop" control proves the
// same harness stores a complete response.
func TestM611TruncatedNeverStoredEnginePaths(t *testing.T) {
	finishes := []string{"length", "LENGTH", "Length"}
	ctx := context.Background()
	assertNothingStored := func(t *testing.T, se *StaleEngine, exact *cache.ExactCache, sem *cache.LocalSemanticCache) {
		t.Helper()
		if _, ok := exact.Get(ctx, m611ScopedKey(t, se, testReq())); ok {
			t.Fatal("exact cache holds an entry for the truncated response")
		}
		if _, ok := sem.GetTextForModel(ctx, "test-model", "What is 2+2?"); ok {
			t.Fatal("semantic cache holds an entry for the truncated response")
		}
	}
	for _, finish := range finishes {
		// Chat path.
		f := &fakeEngine{chatResp: m611Resp("partial", finish)}
		se, exact, sem, _ := wrapSemanticTest(f, m611Policy())
		resp, err := se.Chat(ctx, testReq())
		if err != nil || resp == nil || resp.Content != "partial" {
			t.Fatalf("finish %q Chat: first = (%+v, %v), want the truncated content served once", finish, resp, err)
		}
		assertNothingStored(t, se, exact, sem)
		m611FailProvider(f)
		if _, err := se.Chat(ctx, testReq()); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("finish %q Chat: second err = %v, want the upstream error", finish, err)
		}

		// ChatStream path.
		fs := &fakeEngine{streamResp: m611Resp("partial", finish), streamDeltas: []string{"partial"}}
		ses, exacts, sems, _ := wrapSemanticTest(fs, m611Policy())
		resp, err = ses.ChatStream(ctx, testReq(), func(string) error { return nil })
		if err != nil || resp == nil || resp.Content != "partial" || resp.FinishReason != finish {
			t.Fatalf("finish %q ChatStream: first = (%+v, %v), want the truncated response", finish, resp, err)
		}
		assertNothingStored(t, ses, exacts, sems)
		m611FailProvider(fs)
		if _, err := ses.ChatStream(ctx, testReq(), func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("finish %q ChatStream: second err = %v, want the upstream error", finish, err)
		}

		// ChatStreamChunks path (finish arrives on the terminal chunk).
		fc := &chunkFakeEngine{
			chunks: []provider.StreamChunk{
				{Content: "partial"},
				{FinishReason: finish},
			},
			chunkResp: m611Resp("partial", finish),
		}
		sec, exactc, semc, _ := wrapSemanticTest(fc, m611Policy())
		resp, err = sec.ChatStreamChunks(ctx, testReq(), func(provider.StreamChunk) error { return nil })
		if err != nil || resp == nil || resp.Content != "partial" || resp.FinishReason != finish {
			t.Fatalf("finish %q ChatStreamChunks: first = (%+v, %v), want the truncated response", finish, resp, err)
		}
		assertNothingStored(t, sec, exactc, semc)
		fc.chunkResp = nil
		fc.chunkErr = errUpstreamDown
		fc.chunks = nil
		if _, err := sec.ChatStreamChunks(ctx, testReq(), func(provider.StreamChunk) error { return nil }); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("finish %q ChatStreamChunks: second err = %v, want the upstream error", finish, err)
		}

		// ChatStreamFrom fallback path (engine has no resumer).
		ff := &fakeEngine{streamResp: m611Resp("partial", finish), streamDeltas: []string{"partial"}}
		sef, exactf, semf, _ := wrapSemanticTest(ff, m611Policy())
		resp, err = sef.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil })
		if err != nil || resp == nil || resp.FinishReason != finish {
			t.Fatalf("finish %q ChatStreamFrom: first = (%+v, %v), want the truncated response", finish, resp, err)
		}
		assertNothingStored(t, sef, exactf, semf)
		m611FailProvider(ff)
		if _, err := sef.ChatStreamFrom(ctx, testReq(), 0, func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("finish %q ChatStreamFrom: second err = %v, want the upstream error", finish, err)
		}
	}
	// Control: a complete ("stop") response IS stored on the Chat
	// path, and the second Chat is an exact hit.
	fc := &fakeEngine{chatResp: m611Resp("4", "stop")}
	sec, exactc, _, _ := wrapSemanticTest(fc, m611Policy())
	if _, err := sec.Chat(ctx, testReq()); err != nil {
		t.Fatalf("control first Chat: %v", err)
	}
	if _, ok := exactc.Get(ctx, m611ScopedKey(t, sec, testReq())); !ok {
		t.Fatal("control: complete response was not stored in the exact cache")
	}
	m611FailProvider(fc)
	if resp, err := sec.Chat(ctx, testReq()); err != nil || resp == nil || resp.Content != "4" {
		t.Fatalf("control second Chat = (%+v, %v), want the stored complete answer", resp, err)
	}
}

// TestM611NoCrossTenantSemanticOnStream (Phase 2/T9) closes the last
// uncovered surface of property 1: the STREAM path. The wrapper's
// lookup() is shared by Chat and ChatStream, but that sharing was
// proven for the exact layer (TestM611NoCrossTenantExactChatStream)
// and only implied for semantic. This pins it: tenant A stores via a
// streaming request; tenant A's own paraphrase stream hits semantically
// (control); tenant B's identical-text AND paraphrase streams must both
// go upstream (providers down), and a cross-temperature paraphrase by
// tenant A itself must miss too.
func TestM611NoCrossTenantSemanticOnStream(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{streamResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	pol := m611Policy()
	pol.AllowNonZeroTemp = true // temperature scoping needs nonzero entries storable
	se, _, _, _ := wrapSemanticTest(f, pol)

	if _, err := se.ChatStream(ctx, m611Req("tenant-a", "test-model", "What is the capital of France?", 0), func(string) error { return nil }); err != nil {
		t.Fatalf("tenant A store ChatStream: %v", err)
	}
	// Control: same-tenant paraphrase on the STREAM path hits semantically.
	f.streamResp = nil
	f.streamErr = errUpstreamDown
	resp, err := se.ChatStream(ctx, m611Req("tenant-a", "test-model", "What is the capital of France today?", 0), func(string) error { return nil })
	if err != nil || resp == nil || resp.Content != "Paris is the capital of France." {
		t.Fatalf("same-tenant paraphrase ChatStream = (%+v, %v), want the semantic hit", resp, err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("same-tenant paraphrase LastCacheHit kind = %q, want semantic", kind)
	}
	// Property: tenant B, identical text, stream must go upstream.
	if _, err := se.ChatStream(ctx, m611Req("tenant-b", "test-model", "What is the capital of France?", 0), func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B identical-text ChatStream err = %v, want the upstream error (cross-tenant serve)", err)
	}
	// Property: tenant B, paraphrase, stream must also miss.
	if _, err := se.ChatStream(ctx, m611Req("tenant-b", "test-model", "What is the capital of France today?", 0), func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B paraphrase ChatStream err = %v, want the upstream error (cross-tenant paraphrase serve)", err)
	}

	// Temperature partition on the stream path: a temp-0.7 entry is not
	// served to a temp-0.9 request (same tenant, paraphrase).
	g := &fakeEngine{streamResp: &provider.ChatResponse{
		Content: "hot answer",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	seg, _, _, _ := wrapSemanticTest(g, pol)
	if _, err := seg.ChatStream(ctx, m611Req("tenant-c", "test-model", "What is the capital of France?", 0.7), func(string) error { return nil }); err != nil {
		t.Fatalf("tenant C store ChatStream @0.7: %v", err)
	}
	g.streamResp = nil
	g.streamErr = errUpstreamDown
	if _, err := seg.ChatStream(ctx, m611Req("tenant-c", "test-model", "What is the capital of France today?", 0.9), func(string) error { return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("cross-temperature paraphrase ChatStream err = %v, want the upstream error (temperature leak)", err)
	}
}
