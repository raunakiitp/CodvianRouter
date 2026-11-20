// M6.7c: the 50-turn semantic bypass cap on the relay path
// (lookupMessagesCache + storeMessagesCache).
//
// A conversation longer than provider.SemanticMaxTurns (50, raw
// message count including system — see
// provider.SemanticTurnsExceeded) must never get a semantic hit
// and must never store a semantic entry, on either path. The exact
// layer is unaffected: a long conversation still stores and hits
// exactly. The 50-turn controls prove the cap is the boundary,
// not a blanket disable.
package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m67cRelayLongReq builds a plain n-message conversation
// alternating user/assistant turns, with distinct text per turn
// so no two windows collide. (Same shape as the engine's
// m67cLongReq in internal/degrade.)
func m67cRelayLongReq(n int) provider.ChatRequest {
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

func TestM67cRelayLookupBypassesPast50Turns(t *testing.T) {
	ctx := context.Background()
	seed := func(sem *cache.LocalSemanticCache, req provider.ChatRequest) {
		sem.SetTextForModelScoped(ctx, "test-model", provider.SemanticWindowText(req.Messages), "", 0, cache.SemanticEntry{
			Response: cache.CachedResponse{Content: "seeded answer", Model: "test-model"},
		}, time.Hour)
	}

	t.Run("51 turns miss despite a seeded entry", func(t *testing.T) {
		sem := cache.NewLocalSemanticCache()
		srv := &Server{semanticCache: sem}
		req := m67cRelayLongReq(51)
		seed(sem, req)
		if _, _, found := srv.lookupMessagesCache(ctx, req); found {
			t.Fatal("51-turn relay lookup hit a seeded semantic entry; want a miss (the lookup must bypass past 50 turns)")
		}
	})

	t.Run("50 turns still hit (control)", func(t *testing.T) {
		sem := cache.NewLocalSemanticCache()
		srv := &Server{semanticCache: sem}
		req := m67cRelayLongReq(50)
		seed(sem, req)
		hit, layer, found := srv.lookupMessagesCache(ctx, req)
		if !found || layer != "semantic" || hit.Content != "seeded answer" {
			t.Fatalf("50-turn relay lookup = %+v, %q, %v; want the seeded answer via semantic", hit, layer, found)
		}
	})
}

func TestM67cRelayStoreBypassesPast50Turns(t *testing.T) {
	ctx := context.Background()

	t.Run("51 turns store nothing semantically, exact still stored", func(t *testing.T) {
		exact := cache.NewExactCache(100)
		sem := cache.NewLocalSemanticCache()
		srv := &Server{exactCache: exact, semanticCache: sem}
		req := m67cRelayLongReq(51)
		srv.storeMessagesCache(ctx, req, []byte(anthropicJSONReply))
		if _, found := sem.GetTextForModel(ctx, "test-model", provider.SemanticWindowText(req.Messages)); found {
			t.Fatal("51-turn conversation stored a semantic entry via the relay; want none (the store path must bypass too)")
		}
		// Exact layer unaffected: the relay still stored the
		// entry under the scoped exact key.
		key, err := scopedMessagesKey(req)
		if err != nil {
			t.Fatalf("scopedMessagesKey: %v", err)
		}
		hit, found := exact.Get(ctx, key)
		if !found {
			t.Fatal("51-turn conversation stored no exact entry; want one (the cap gates the semantic layer only)")
		}
		if hit.Content != "Hello from Claude" {
			t.Fatalf("exact content = %q, want %q", hit.Content, "Hello from Claude")
		}
	})

	t.Run("50 turns still store semantically (control)", func(t *testing.T) {
		exact := cache.NewExactCache(100)
		sem := cache.NewLocalSemanticCache()
		srv := &Server{exactCache: exact, semanticCache: sem}
		req := m67cRelayLongReq(50)
		srv.storeMessagesCache(ctx, req, []byte(anthropicJSONReply))
		hit, found := sem.GetTextForModel(ctx, "test-model", provider.SemanticWindowText(req.Messages))
		if !found {
			t.Fatal("50-turn conversation stored no semantic entry via the relay; want one")
		}
		if hit.Response.Content != "Hello from Claude" {
			t.Fatalf("stored semantic content = %q, want %q", hit.Response.Content, "Hello from Claude")
		}
	})
}
