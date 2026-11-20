// M6.11-fix — relay proof that the two BLOCKED M6.11 findings
// are closed on the /v1/messages path too, through the real
// relay functions (lookupMessagesCache / storeMessagesCache):
// the relay shares the one semantic store with the engine, so
// it had the same tenant-blind, temperature-blind holes by
// construction.
//
// The relay's temperature rule is unchanged: a temp>0 request
// never consults the relay cache (CacheableTemperature with
// allowance false), and a temp>0 response is never stored from
// the relay (ShouldStore with allowance false). The property
// proven here is the one the relay could violate: a temp-0
// relay lookup being served an entry another tenant stored, or
// an entry the engine stored at temp 0.7.
package server

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m611FixRelayReq builds a canonical relay request for one
// (tenant, prompt, temperature) combination.
func m611FixRelayReq(tenant, text string, temp float64) provider.ChatRequest {
	return provider.ChatRequest{
		Model:       "test-model",
		Messages:    []provider.Message{{Role: "user", Content: text}},
		CallerID:    tenant,
		Temperature: temp,
	}
}

// m611FixRelayBody is a minimal Anthropic response body carrying
// content, in the shape storeMessagesCache converts.
func m611FixRelayBody(content string) []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"` + content + `"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":7}}`)
}

// TestM611FixRelayCrossTenantParaphrase proves BLOCKED-1 on the
// relay: an entry stored by the relay for tenant A (through the
// real storeMessagesCache) is never served to tenant B's
// paraphrase lookup, while tenant A's own paraphrase still hits
// semantically. The reverse direction is covered too: an entry
// the engine stored scoped for tenant A (direct scoped store,
// the store the engine writes through) is invisible to the
// relay's tenant-B lookup.
func TestM611FixRelayCrossTenantParaphrase(t *testing.T) {
	ctx := context.Background()
	sem := cache.NewLocalSemanticCache()
	srv := &Server{semanticCache: sem}
	// Relay stores tenant A's Paris answer (temp 0).
	srv.storeMessagesCache(ctx, m611FixRelayReq("tenant-a", "What is the capital of France?", 0),
		m611FixRelayBody("Paris is the capital of France."))
	// Control: tenant A's own paraphrase hits semantically.
	if hit, layer, found := srv.lookupMessagesCache(ctx, m611FixRelayReq("tenant-a", "What is the capital of France today?", 0)); !found || layer != "semantic" || hit.Content != "Paris is the capital of France." {
		t.Fatalf("same-tenant relay paraphrase = (%+v, %q, %v), want the Paris answer via semantic", hit, layer, found)
	}
	// Property: tenant B's paraphrase misses.
	if hit, layer, found := srv.lookupMessagesCache(ctx, m611FixRelayReq("tenant-b", "What is the capital of France today?", 0)); found {
		t.Fatalf("cross-tenant relay paraphrase = (%+v, %q, %v), want a miss: tenant B must never be served tenant A's answer", hit, layer, found)
	}
	// And the identical text misses for B too (the provenance
	// gate would have caught this one; the paraphrase above is
	// the case only the scoped store closes).
	if _, _, found := srv.lookupMessagesCache(ctx, m611FixRelayReq("tenant-b", "What is the capital of France?", 0)); found {
		t.Fatal("cross-tenant relay identical-text lookup hit; want a miss")
	}
	// Reverse direction: engine-stored scoped entry, relay lookup.
	sem2 := cache.NewLocalSemanticCache()
	sem2.SetTextForModelScoped(ctx, "test-model", "What is the capital of France?", "tenant-a", 0,
		cache.SemanticEntry{Response: cache.CachedResponse{Content: "Paris is the capital of France.", Model: "test-model"}}, time.Hour)
	srv2 := &Server{semanticCache: sem2}
	if _, _, found := srv2.lookupMessagesCache(ctx, m611FixRelayReq("tenant-b", "What is the capital of France today?", 0)); found {
		t.Fatal("relay lookup served an engine-stored entry to another tenant's paraphrase; want a miss")
	}
	if hit, layer, found := srv2.lookupMessagesCache(ctx, m611FixRelayReq("tenant-a", "What is the capital of France today?", 0)); !found || layer != "semantic" || hit.Content != "Paris is the capital of France." {
		t.Fatalf("relay lookup of own-tenant engine entry = (%+v, %q, %v), want the Paris answer via semantic", hit, layer, found)
	}
}

// TestM611FixRelayCrossTemp proves BLOCKED-2 on the relay: a
// temp-0 relay lookup is never served an answer the engine
// stored at temp 0.7 — identical text or paraphrase — while a
// temp-0 entry still hits its own temp-0 lookup. The relay
// itself never stores temp>0 (ShouldStore allowance false), so
// the hot entry is seeded the way the engine would store it.
func TestM611FixRelayCrossTemp(t *testing.T) {
	ctx := context.Background()
	sem := cache.NewLocalSemanticCache()
	sem.SetTextForModelScoped(ctx, "test-model", "What is the capital of France?", "tenant-a", 0.7,
		cache.SemanticEntry{Response: cache.CachedResponse{Content: "Paris is the capital of France.", Model: "test-model"}}, time.Hour)
	srv := &Server{semanticCache: sem}
	for _, text := range []string{"What is the capital of France?", "What is the capital of France today?"} {
		if hit, layer, found := srv.lookupMessagesCache(ctx, m611FixRelayReq("tenant-a", text, 0)); found {
			t.Fatalf("temp-0 relay lookup text %q served the temp-0.7 entry (%+v via %q); want a miss", text, hit, layer)
		}
	}
	// Control: a temp-0 entry still hits its own temp-0 lookup.
	sem2 := cache.NewLocalSemanticCache()
	sem2.SetTextForModelScoped(ctx, "test-model", "What is the capital of France?", "tenant-a", 0,
		cache.SemanticEntry{Response: cache.CachedResponse{Content: "Paris is the capital of France.", Model: "test-model"}}, time.Hour)
	srv2 := &Server{semanticCache: sem2}
	if hit, layer, found := srv2.lookupMessagesCache(ctx, m611FixRelayReq("tenant-a", "What is the capital of France today?", 0)); !found || layer != "semantic" || hit.Content != "Paris is the capital of France." {
		t.Fatalf("temp-0 relay paraphrase = (%+v, %q, %v), want the Paris answer via semantic", hit, layer, found)
	}
}

// TestM611FixRelayAnonymousTenant proves BLOCKED-1 on the relay
// for the anonymous tenant in both directions: "" is a tenant
// like any other on this path too.
func TestM611FixRelayAnonymousTenant(t *testing.T) {
	ctx := context.Background()
	for _, pair := range [][2]string{{"", "gk-named"}, {"gk-named", ""}} {
		sem := cache.NewLocalSemanticCache()
		srv := &Server{semanticCache: sem}
		srv.storeMessagesCache(ctx, m611FixRelayReq(pair[0], "What is the capital of France?", 0),
			m611FixRelayBody("Paris is the capital of France."))
		if hit, layer, found := srv.lookupMessagesCache(ctx, m611FixRelayReq(pair[1], "What is the capital of France today?", 0)); found {
			t.Fatalf("tenants %q/%q: cross-tenant relay paraphrase = (%+v, %q, %v), want a miss", pair[0], pair[1], hit, layer, found)
		}
	}
}
