package degrade

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestContractRequestSkipsCache is the permanent gate for CHECKLIST
// 157's cache rule: a request carrying a per-request routing
// contract must never be served from — or stored into — the
// exact/semantic cache. A cached entry holds content only, with no
// record of which provider produced it, so serving one could hand
// a client an answer from a provider its own only/exclude/order/
// max_price/data_collection forbade, and storing one could serve
// it to a later request whose contract forbids that provider.
func TestContractRequestSkipsCache(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{Content: "plain-answer"}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	// First plain request: provider called, answer stored.
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("chatCalls = %d, want 1", f.chatCalls)
	}

	// The provider now answers differently. A contracted request
	// for the same prompt must go to the provider (not the cache)
	// and get the fresh answer.
	f.chatResp = &provider.ChatResponse{Content: "contract-answer"}
	contracted := testReq()
	contracted.Routing = &provider.RequestRouting{Only: []string{"mid"}}
	resp, err := se.Chat(ctx, contracted)
	if err != nil {
		t.Fatalf("contract Chat: %v", err)
	}
	if resp == nil || resp.Content != "contract-answer" {
		t.Fatalf("contract Chat returned %+v, want contract-answer (live, not cached)", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("chatCalls = %d, want 2: the contracted request must call the provider", f.chatCalls)
	}
	if h, _ := m.get("exact"); h != 0 {
		t.Fatalf("exact cache hits = %d, want 0 for the contracted request", h)
	}
	if kind, _, hit := se.LastCacheHit(); hit {
		t.Fatalf("LastCacheHit = (%q, true): a contracted request must never record a cache hit", kind)
	}

	// The contracted answer must not have been stored: a plain
	// repeat still gets the original cached answer.
	resp, err = se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("plain repeat Chat: %v", err)
	}
	if resp == nil || resp.Content != "plain-answer" {
		t.Fatalf("plain repeat returned %+v, want plain-answer from the cache (the contract answer must never have been stored)", resp)
	}
	if f.chatCalls != 2 {
		t.Fatalf("chatCalls = %d, want 2: the plain repeat must be a cache hit", f.chatCalls)
	}
	if h, _ := m.get("exact"); h != 1 {
		t.Fatalf("exact cache hits = %d, want 1 (the plain repeat)", h)
	}
}

// TestContractRequestSkipsStaleFallback pins the same rule for the
// degradation path: when every provider fails, a contracted request
// must fail honestly instead of being served a stale entry from a
// provider its contract may forbid.
func TestContractRequestSkipsStaleFallback(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{Content: "plain-answer"}}
	// Degradation on, proactive cache serving OFF: the repeat
	// goes to the provider, fails, and the stale entry is the
	// only way it can be answered.
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _, _, _ := wrapSemanticTest(f, policy)
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	// Providers now fail. A plain repeat is served stale; a
	// contracted repeat must fail instead.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("plain repeat Chat: %v (want the stale answer)", err)
	}
	if resp == nil || resp.Content != "plain-answer" || !se.LastServedStale() {
		t.Fatalf("plain repeat = %+v stale=%v, want the stale plain-answer", resp, se.LastServedStale())
	}
	contracted := testReq()
	contracted.Routing = &provider.RequestRouting{Exclude: []string{"mid"}}
	if _, err := se.Chat(ctx, contracted); err == nil {
		t.Fatalf("contract Chat with all providers down: want an error, got a stale answer")
	}
}
