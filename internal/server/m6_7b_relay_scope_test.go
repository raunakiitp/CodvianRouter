// M6.7b semantic scope parity for the relay + threshold method on
// the persistent semantic adapter.
//
// Before M6.7b the relay keyed semantic entries on the last user
// message only (lastUserText) while the engine keyed them on the
// last 3 turns (degrade.semanticText, SemanticScopeTurns = 3), so
// cross-dialect semantic sharing was broken: an engine-stored
// entry was invisible to the relay and vice versa, and the
// relay/engine provenance keys disagreed. The relay now uses the
// same 3-turn window (relaySemanticText), duplicated from
// degrade.semanticText because internal/server cannot import
// internal/degrade (import cycle: degrade imports server).
//
// These tests pin the relay to the engine's window using the SAME
// literal window strings the engine's own scope test pins
// (TestM67SemanticTextWindow in internal/degrade), and prove
// sharing through the real store/lookup functions —
// lookupMessagesCache / storeMessagesCache on the relay side and
// GetTextForModel(GetTextForModelWithThreshold) on the cache,
// which is the lookup the engine performs — not by comparing
// strings only. Similarities come from the real EmbedText/Cosine
// path, never invented: each test first asserts the measured
// similarity lands in the band it needs.
package server

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func m67bMsg(role, text string) provider.Message {
	return provider.Message{Role: role, Content: text}
}

// m67bStoreReq is the 3-turn conversation the sharing tests
// seed: the assistant turn is the Paris answer. (Same messages
// as the engine's m67ScopeStoreReq in internal/degrade.)
func m67bStoreReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67bMsg("user", "What is the capital of France?"),
			m67bMsg("assistant", "The capital of France is Paris, a large historic city on the river Seine famous for museums gardens and cafes."),
			m67bMsg("user", "Tell me more about its history."),
		},
	}
}

// m67bStoreWindow is the engine's window text for m67bStoreReq:
// the literal string degrade.semanticText produces for these
// messages (pinned by TestM67SemanticTextWindow's shape).
const m67bStoreWindow = "user: What is the capital of France?\nassistant: The capital of France is Paris, a large historic city on the river Seine famous for museums gardens and cafes.\nuser: Tell me more about its history."

// m67bLookupReq is the same conversation with the assistant turn
// swapped to the Berlin answer. The last user message is
// identical, so the old last-user-message-only scope would match
// at similarity 1.0; the 3-turn window scores ~0.82, below the
// 0.85 floor, so the new scope must miss.
func m67bLookupReq() provider.ChatRequest {
	req := m67bStoreReq()
	req.Messages[1] = m67bMsg("assistant", "The capital of Germany is Berlin, a large historic city on the river Spree famous for museums parks and nightlife.")
	return req
}

// m67bLookupWindow is the engine's window text for m67bLookupReq.
const m67bLookupWindow = "user: What is the capital of France?\nassistant: The capital of Germany is Berlin, a large historic city on the river Spree famous for museums parks and nightlife.\nuser: Tell me more about its history."

// m67bWindowSim asserts the real embedding similarity of the two
// relay window texts lands in (lo, hi) and returns it, so the
// test's premise is checked against the actual embedding instead
// of assumed.
func m67bWindowSim(t *testing.T, store, lookup provider.ChatRequest, lo, hi float32) float32 {
	t.Helper()
	sim := cache.Cosine(cache.EmbedText(relaySemanticText(store)), cache.EmbedText(relaySemanticText(lookup)))
	if sim <= lo || sim >= hi {
		t.Fatalf("relay window similarity = %.4f, want in (%.2f, %.2f): store window %q lookup window %q", sim, lo, hi, relaySemanticText(store), relaySemanticText(lookup))
	}
	return sim
}

// TestM67bRelaySemanticWindow: the relay's window text equals the
// engine's window text for the same messages — the exact window
// shape (last 3 non-system turns, "role: text" lines in order,
// older history dropped, empty-text messages contribute no line,
// system and developer messages excluded even inside the window),
// pinned here with the same literals the engine's scope test
// uses, so a drift in either direction breaks loudly.
func TestM67bRelaySemanticWindow(t *testing.T) {
	// The scope pair: relay text must equal the engine's literal
	// window text for both conversations.
	if got := relaySemanticText(m67bStoreReq()); got != m67bStoreWindow {
		t.Fatalf("relaySemanticText(store) = %q, want %q (the engine's window text)", got, m67bStoreWindow)
	}
	if got := relaySemanticText(m67bLookupReq()); got != m67bLookupWindow {
		t.Fatalf("relaySemanticText(lookup) = %q, want %q (the engine's window text)", got, m67bLookupWindow)
	}
	// Last 3 turns only; empty-text messages contribute no line.
	req := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67bMsg("system", "You are helpful."),
			m67bMsg("user", "Old question?"),
			m67bMsg("assistant", "Old answer."),
			m67bMsg("user", "New question?"),
			m67bMsg("assistant", ""),
		},
	}
	if want := "assistant: Old answer.\nuser: New question?"; relaySemanticText(req) != want {
		t.Fatalf("relaySemanticText = %q, want %q (last 3 turns only, empty text skipped)", relaySemanticText(req), want)
	}
	// A single user message keeps the "user: " prefix.
	if got := relaySemanticText(provider.ChatRequest{Model: "test-model", Messages: []provider.Message{m67bMsg("user", "What is 2+2?")}}); got != "user: What is 2+2?" {
		t.Fatalf("relaySemanticText(single) = %q, want %q", got, "user: What is 2+2?")
	}
	// A system message inside the window is excluded: the window
	// is the same with or without it, so a different system
	// prompt cannot break a same-question match.
	sysReq := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67bMsg("system", "Be brief."),
			m67bMsg("user", "What is 2+2?"),
		},
	}
	if got := relaySemanticText(sysReq); got != "user: What is 2+2?" {
		t.Fatalf("relaySemanticText(system+user) = %q, want %q (system excluded)", got, "user: What is 2+2?")
	}
	devReq := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67bMsg("developer", "Answer concisely."),
			m67bMsg("user", "What is 2+2?"),
		},
	}
	if got := relaySemanticText(devReq); got != "user: What is 2+2?" {
		t.Fatalf("relaySemanticText(developer+user) = %q, want %q (developer excluded)", got, "user: What is 2+2?")
	}
	// A missing role reads as "user", as in the engine's window.
	noRole := provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Content: "What is 2+2?"}},
	}
	if got := relaySemanticText(noRole); got != "user: What is 2+2?" {
		t.Fatalf("relaySemanticText(no role) = %q, want %q (missing role reads as user)", got, "user: What is 2+2?")
	}
	// No text anywhere: "".
	empty := provider.ChatRequest{Model: "test-model", Messages: []provider.Message{m67bMsg("assistant", "")}}
	if got := relaySemanticText(empty); got != "" {
		t.Fatalf("relaySemanticText(empty) = %q, want empty", got)
	}
	// Text parts (array content) are joined with a space, same
	// as the engine's messageText.
	parts := provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Parts: []provider.ContentPart{{Type: "text", Text: "Hello"}, {Type: "text", Text: "there"}}}},
	}
	if got := relaySemanticText(parts); got != "user: Hello there" {
		t.Fatalf("relaySemanticText(parts) = %q, want %q", got, "user: Hello there")
	}
}

// TestM67bRelayEngineSemanticSharing: cross-dialect sharing
// through the real store/lookup functions, both directions.
//
//   - Engine -> relay: an entry stored under the engine's window
//     text (via the cache's SetTextForModelScoped, the store the engine
//     writes through) is found by the relay's lookupMessagesCache
//     for the same conversation, and still found when the lookup
//     carries an extra older turn the store lacked (the window is
//     identical — older history cannot break the match).
//   - Relay -> engine: an entry stored by the relay's
//     storeMessagesCache is found by the cache lookups the engine
//     performs (GetTextForModel and the threshold-aware
//     GetTextForModelWithThreshold) under the engine's window text.
func TestM67bRelayEngineSemanticSharing(t *testing.T) {
	ctx := context.Background()
	sem := cache.NewLocalSemanticCache()

	t.Run("engine store found by relay lookup", func(t *testing.T) {
		sem.SetTextForModelScoped(ctx, "test-model", m67bStoreWindow, "", 0, cache.SemanticEntry{
			Response: cache.CachedResponse{Content: "Paris answer", Model: "test-model"},
		}, time.Hour)
		srv := &Server{semanticCache: sem}
		hit, layer, found := srv.lookupMessagesCache(ctx, m67bStoreReq())
		if !found {
			t.Fatal("relay lookup missed an entry stored under the engine's window text; want a semantic hit")
		}
		if layer != "semantic" {
			t.Fatalf("relay lookup layer = %q, want semantic", layer)
		}
		if hit.Content != "Paris answer" {
			t.Fatalf("relay lookup content = %q, want %q", hit.Content, "Paris answer")
		}
		// Extra older history on the lookup side must not break
		// the match: the last 3 turns are identical.
		lookup := m67bStoreReq()
		lookup.Messages = append([]provider.Message{m67bMsg("user", "Something else entirely?")}, lookup.Messages...)
		if got, want := relaySemanticText(lookup), m67bStoreWindow; got != want {
			t.Fatalf("window with extra older turn = %q, want %q (identical window)", got, want)
		}
		if hit, layer, found := srv.lookupMessagesCache(ctx, lookup); !found || layer != "semantic" || hit.Content != "Paris answer" {
			t.Fatalf("relay lookup with extra older turn = %+v, %q, %v; want the Paris answer via semantic", hit, layer, found)
		}
	})

	t.Run("relay store found by engine lookup", func(t *testing.T) {
		sem2 := cache.NewLocalSemanticCache()
		srv := &Server{semanticCache: sem2}
		// storeMessagesCache stores the Anthropic reply body
		// ("Hello from Claude") under the relay's window text.
		srv.storeMessagesCache(ctx, m67bStoreReq(), []byte(anthropicJSONReply))
		// The engine looks up by its window text through the
		// cache's own lookup functions.
		hit, found := sem2.GetTextForModel(ctx, "test-model", m67bStoreWindow)
		if !found {
			t.Fatal("engine GetTextForModel missed an entry stored by the relay; want a hit under the engine's window text")
		}
		if hit.Response.Content != "Hello from Claude" {
			t.Fatalf("engine lookup content = %q, want %q", hit.Response.Content, "Hello from Claude")
		}
		if hit, found := sem2.GetTextForModelWithThreshold(ctx, "test-model", m67bStoreWindow, 0); !found || hit.Response.Content != "Hello from Claude" {
			t.Fatalf("engine WithThreshold(0) lookup = %+v, %v; want the relay-stored entry", hit, found)
		}
	})
}

// TestM67bRelayScopeRegression: the old last-user-only key NO
// LONGER matches when earlier turns differ. The store and lookup
// conversations share the identical last user message ("Tell me
// more about its history."), so the old scope would have matched
// at similarity 1.0; the 3-turn windows score ~0.82, below the
// 0.85 floor, so the relay lookup must miss — and the identical
// conversation must still hit (control: the store itself works).
func TestM67bRelayScopeRegression(t *testing.T) {
	sim := m67bWindowSim(t, m67bStoreReq(), m67bLookupReq(), 0.80, 0.85)
	t.Logf("differing-assistant window similarity = %.4f", sim)
	// The old key really would have matched: the last user
	// message is byte-identical in both conversations.
	storeMsgs, lookupMsgs := m67bStoreReq().Messages, m67bLookupReq().Messages
	if storeMsgs[len(storeMsgs)-1].Content != lookupMsgs[len(lookupMsgs)-1].Content {
		t.Fatal("test premise broken: last user messages differ; the old scope would not have matched either")
	}
	if got, want := relaySemanticText(m67bStoreReq()), relaySemanticText(m67bLookupReq()); got == want {
		t.Fatalf("relay window texts are identical %q; want them to differ (the assistant turn differs)", got)
	}

	ctx := context.Background()
	sem := cache.NewLocalSemanticCache()
	srv := &Server{semanticCache: sem}
	srv.storeMessagesCache(ctx, m67bStoreReq(), []byte(anthropicJSONReply))

	if _, _, found := srv.lookupMessagesCache(ctx, m67bLookupReq()); found {
		t.Fatal("relay lookup hit for the differing-assistant conversation; want a miss (the earlier turn is in the window now)")
	}
	if hit, layer, found := srv.lookupMessagesCache(ctx, m67bStoreReq()); !found || layer != "semantic" || hit.Content != "Hello from Claude" {
		t.Fatalf("relay lookup for the identical conversation = %+v, %q, %v; want the stored entry via semantic (control)", hit, layer, found)
	}
}
