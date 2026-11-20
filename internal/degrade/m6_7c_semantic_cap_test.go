// M6.7c: the 50-turn semantic bypass cap on the engine path
// (StaleEngine lookup + store).
//
// A conversation longer than provider.SemanticMaxTurns (50, raw
// message count including system — see
// provider.SemanticTurnsExceeded) must never get a semantic hit
// and must never store a semantic entry, on either path. The exact
// layer is unaffected: a long conversation still hits exactly.
// The 50-turn controls prove the cap is the boundary, not a
// blanket disable: an identical-shape 50-turn conversation still
// hits and stores semantically.
package degrade

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m67cLongReq builds a plain n-message conversation alternating
// user/assistant turns, with distinct text per turn so no two
// windows collide.
func m67cLongReq(n int) provider.ChatRequest {
	msgs := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			msgs = append(msgs, provider.Message{Role: "user", Content: fmt.Sprintf("Turn %d question?", i)})
		} else {
			msgs = append(msgs, provider.Message{Role: "assistant", Content: fmt.Sprintf("Turn %d answer.", i)})
		}
	}
	return provider.ChatRequest{Model: "test-model", Messages: msgs}
}

func TestM67cEngineLookupBypassesPast50Turns(t *testing.T) {
	ctx := context.Background()
	seed := func(sem *cache.LocalSemanticCache, req provider.ChatRequest) {
		sem.SetTextForModelScoped(ctx, "test-model", provider.SemanticWindowText(req.Messages), "", 0, cache.SemanticEntry{
			Response: cache.CachedResponse{Content: "seeded answer", Model: "test-model"},
		}, time.Hour)
	}

	t.Run("51 turns miss despite a seeded entry", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, sem, _ := wrapSemanticTest(f, serveCachePolicy())
		req := m67cLongReq(51)
		seed(sem, req)
		resp, err := se.Chat(ctx, req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Content != "4" {
			t.Fatalf("content = %q, want 4 (provider answer, not the seeded semantic entry)", resp.Content)
		}
		if f.chatCalls != 1 {
			t.Fatalf("chatCalls = %d, want 1: the 51-turn lookup must bypass the semantic cache and call the provider", f.chatCalls)
		}
		if _, _, ok := se.LastCacheHit(); ok {
			t.Fatal("51-turn lookup must not report a cache hit")
		}
	})

	t.Run("50 turns still hit (control)", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, sem, _ := wrapSemanticTest(f, serveCachePolicy())
		req := m67cLongReq(50)
		seed(sem, req)
		resp, err := se.Chat(ctx, req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Content != "seeded answer" {
			t.Fatalf("content = %q, want the seeded answer (semantic hit)", resp.Content)
		}
		if f.chatCalls != 0 {
			t.Fatalf("chatCalls = %d, want 0: the 50-turn lookup must hit semantically without calling the provider", f.chatCalls)
		}
		if kind, sim, ok := se.LastCacheHit(); !ok || kind != "semantic" || sim != 1.0 {
			t.Fatalf("LastCacheHit = %q,%.4f,%v, want semantic,1.0,true", kind, sim, ok)
		}
	})
}

func TestM67cEngineStoreBypassesPast50Turns(t *testing.T) {
	ctx := context.Background()

	t.Run("51 turns store nothing semantically", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, sem, _ := wrapSemanticTest(f, serveCachePolicy())
		req := m67cLongReq(51)
		if _, err := se.Chat(ctx, req); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if f.chatCalls != 1 {
			t.Fatalf("chatCalls = %d, want 1", f.chatCalls)
		}
		if _, found := sem.GetTextForModel(ctx, "test-model", provider.SemanticWindowText(req.Messages)); found {
			t.Fatal("51-turn conversation stored a semantic entry; want none (the store path must bypass too)")
		}
		// Exact layer unaffected: the same conversation again
		// hits exactly, without calling the provider.
		resp, err := se.Chat(ctx, req)
		if err != nil {
			t.Fatalf("second Chat: %v", err)
		}
		if resp.Content != "4" {
			t.Fatalf("second Chat content = %q, want 4 (exact hit)", resp.Content)
		}
		if f.chatCalls != 1 {
			t.Fatalf("second Chat chatCalls = %d, want 1: the exact layer must still serve a 51-turn conversation", f.chatCalls)
		}
		if kind, _, ok := se.LastCacheHit(); !ok || kind != "exact" {
			t.Fatalf("second Chat LastCacheHit = %q,%v, want exact,true (semantic must not have served it)", kind, ok)
		}
	})

	t.Run("50 turns still store semantically (control)", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, sem, _ := wrapSemanticTest(f, serveCachePolicy())
		req := m67cLongReq(50)
		if _, err := se.Chat(ctx, req); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		hit, found := sem.GetTextForModel(ctx, "test-model", provider.SemanticWindowText(req.Messages))
		if !found {
			t.Fatal("50-turn conversation stored no semantic entry; want one")
		}
		if hit.Response.Content != "4" {
			t.Fatalf("stored semantic content = %q, want 4", hit.Response.Content)
		}
	})
}
