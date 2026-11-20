// M6.4 per-request cache controls (bypass headers + per-request
// cache controls) through the serving path (StaleEngine): a bypass
// request must skip lookup AND store AND the stale fallback; a
// no-store request skips store but may still hit; cache-type
// selects layers; a per-request threshold narrows the semantic
// floor; a cache-key override is tenant-scoped.
package degrade

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func m64ReqWith(cc *provider.CacheControls) provider.ChatRequest {
	req := testReq()
	req.CacheControls = cc
	return req
}

func m64BypassReq() provider.ChatRequest {
	return m64ReqWith(&provider.CacheControls{Bypass: true})
}

// TestM64BypassSkipsLookupAndStore: a seeded entry is served to a
// normal request, but a bypass request for the same prompt goes to
// the provider (lookup skipped), its answer is NOT stored (a later
// normal request still gets the seeded answer, not the bypass
// answer), and a bypass request is NOT served stale when the
// provider fails.
func TestM64BypassSkipsLookupAndStore(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("seed chatCalls = %d, want 1", f.chatCalls)
	}
	// Bypass request: same prompt, different answer. Lookup must
	// be skipped (provider called) and the answer must not be
	// stored.
	f.chatResp = &provider.ChatResponse{
		Content: "bypass-answer",
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Model:   "test-model",
	}
	resp, err := se.Chat(ctx, m64BypassReq())
	if err != nil {
		t.Fatalf("bypass Chat: %v", err)
	}
	if resp.Content != "bypass-answer" {
		t.Fatalf("bypass Chat content = %q, want bypass-answer (lookup must be skipped)", resp.Content)
	}
	if f.chatCalls != 2 {
		t.Fatalf("bypass chatCalls = %d, want 2: the bypass request must reach the provider", f.chatCalls)
	}
	// A normal request must still hit the SEEDED answer, not the
	// bypass answer: the bypass response was never stored.
	resp, err = se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("normal Chat after bypass: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("normal Chat after bypass content = %q, want 4: the bypass answer must not have been stored", resp.Content)
	}
	if f.chatCalls != 2 {
		t.Fatalf("normal Chat after bypass chatCalls = %d, want 2 (exact hit)", f.chatCalls)
	}
	// Bypass + failing provider: no stale serve, the upstream
	// error comes back.
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	if _, err := se.Chat(ctx, m64BypassReq()); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("bypass Chat with failing provider: err = %v, want the upstream error (never a stale serve)", err)
	}
}

// TestM64TypeNoneBehavesAsBypass: cache-type none is bypass under
// another name — lookup skipped, store skipped.
func TestM64TypeNoneBehavesAsBypass(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	none := m64ReqWith(&provider.CacheControls{Type: "none"})
	if _, err := se.Chat(ctx, none); err != nil {
		t.Fatalf("type=none Chat: %v", err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("type=none chatCalls = %d, want 2: must reach the provider", f.chatCalls)
	}
	// Nothing stored by the type=none request either: with the
	// provider failing, a normal request still gets the SEEDED
	// entry from the proactive lookup (proving the seed
	// survived) — and a type=none request under the same failing
	// provider gets the upstream error, never the cached entry.
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	calls := f.chatCalls
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("normal Chat after type=none with failing provider: %v (seed must still serve from cache)", err)
	}
	if resp.Content != "4" {
		t.Fatalf("cached content = %q, want 4", resp.Content)
	}
	if f.chatCalls != calls {
		t.Fatalf("normal Chat after type=none chatCalls = %d, want %d (cache hit, no provider call)", f.chatCalls, calls)
	}
	if _, err := se.Chat(ctx, none); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("type=none Chat with failing provider: err = %v, want the upstream error (never stale)", err)
	}
}

// TestM64NoStoreSkipsStoreButAllowsLookup: a no-store request for
// a fresh prompt stores nothing (a later normal request with a
// failing provider fails), but a no-store request for a SEEDED
// prompt is still served from the cache.
func TestM64NoStoreSkipsStoreButAllowsLookup(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	noStore := m64ReqWith(&provider.CacheControls{NoStore: true})

	if _, err := se.Chat(ctx, noStore); err != nil {
		t.Fatalf("no-store Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("no-store chatCalls = %d, want 1", f.chatCalls)
	}
	// Nothing was stored: with the provider failing, a normal
	// request for the same prompt must fail, not serve stale.
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	if _, err := se.Chat(ctx, testReq()); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("normal Chat after no-store with failing provider: err = %v, want the upstream error (nothing stored)", err)
	}
	// Seed normally, then a no-store request IS served from the
	// cache (lookup allowed).
	f.chatErr = nil
	f.chatResp = freshResp()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	calls := f.chatCalls
	resp, err := se.Chat(ctx, noStore)
	if err != nil {
		t.Fatalf("no-store Chat after seed: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("no-store Chat after seed content = %q, want 4 (lookup allowed)", resp.Content)
	}
	if f.chatCalls != calls {
		t.Fatalf("no-store Chat after seed chatCalls = %d, want %d (cache hit, no provider call)", f.chatCalls, calls)
	}
}

// TestM64TypeSemanticSkipsExact: with type=semantic, an identical
// prompt that would hit the exact layer is served from the
// semantic layer instead (LastCacheHit kind "semantic") — proof
// the exact layer was not consulted.
func TestM64TypeSemanticSkipsExact(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	sem := m64ReqWith(&provider.CacheControls{Type: "semantic"})
	resp, err := se.Chat(ctx, sem)
	if err != nil {
		t.Fatalf("type=semantic Chat: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("type=semantic content = %q, want 4", resp.Content)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = %q,%v, want semantic,true: the exact layer must have been skipped", kind, ok)
	}
	if f.chatCalls != 1 {
		t.Fatalf("type=semantic chatCalls = %d, want 1 (semantic hit, no provider call)", f.chatCalls)
	}
}

// TestM64TypeSemanticStalePathSkipsExact (coordinator merge
// addition, M6.4 review FAIL fix): layer selection must hold on
// the FAILURE path too. The policy here has ServeCache off (like
// TestChatFailureWithCacheHitReturnsStale), so the proactive
// lookup never runs and both requests reach staleFor: a
// type=semantic request whose provider fails must get the original
// error, not the surviving stale exact entry, while default
// controls serve it stale. (The proactive type=semantic behaviour
// is already proven by TestM64TypeSemanticSkipsExact.)
func TestM64TypeSemanticStalePathSkipsExact(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _, _, _ := wrapSemanticTest(f, policy)
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	// Now every provider fails.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	semReq := m64ReqWith(&provider.CacheControls{Type: "semantic"})
	_, err := se.Chat(ctx, semReq)
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("type=semantic Chat on failure = %v, want the original error (no stale exact serve)", err)
	}
	if se.LastServedStale() {
		t.Fatal("type=semantic failure served a stale exact entry — layer selection must hold on the failure path")
	}
	// Control: default controls DO serve the stale exact entry.
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("control Chat on failure: %v, want a stale serve", err)
	}
	if resp == nil || resp.Content != "4" || !se.LastServedStale() {
		t.Fatalf("control stale serve = %+v, servedStale=%v, want content 4 + stale", resp, se.LastServedStale())
	}
}

// m64Paraphrase is a near-identical prompt to testReq's text: it
// shares every token but one, so its bag-of-words similarity to
// the seeded text is above the 0.85 semantic floor (verified by
// TestM64ParaphraseHitsSemantically below) but below 1.0.
func m64Paraphrase() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "What is 2+2 please?"}},
	}
}

// TestM64ParaphraseHitsSemantically pins the fixture the next two
// tests rely on: the paraphrase DOES hit the semantic layer with
// default controls.
func TestM64ParaphraseHitsSemantically(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	resp, err := se.Chat(ctx, m64Paraphrase())
	if err != nil {
		t.Fatalf("paraphrase Chat: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("paraphrase content = %q, want 4", resp.Content)
	}
	kind, sim, ok := se.LastCacheHit()
	if !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = %q,%v, want semantic,true (fixture: paraphrase must hit semantically)", kind, ok)
	}
	if sim <= 0.85 || sim >= 1.0 {
		t.Fatalf("paraphrase similarity = %v, want in (0.85, 1.0) (fixture)", sim)
	}
}

// TestM64TypeExactSkipsSemantic: with type=exact, the paraphrase
// that hits semantically by default must miss (provider called).
func TestM64TypeExactSkipsSemantic(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	exact := m64Paraphrase()
	exact.CacheControls = &provider.CacheControls{Type: "exact"}
	if _, err := se.Chat(ctx, exact); err != nil {
		t.Fatalf("type=exact paraphrase Chat: %v", err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("type=exact paraphrase chatCalls = %d, want 2: the semantic layer must have been skipped", f.chatCalls)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("type=exact paraphrase must not report a cache hit")
	}
}

// TestM64ThresholdOverrideChangesBoundary: the paraphrase hits
// semantically by default; a per-request floor of 1.0 turns the
// same request into a miss, and a floor of 0.5 keeps the hit.
// Each sub-case uses a fresh engine: a miss would call the
// provider, whose answer would then be stored under the
// paraphrase's exact key and satisfy the next sub-case exactly
// instead of semantically.
func TestM64ThresholdOverrideChangesBoundary(t *testing.T) {
	run := func(t *testing.T, thr float32, wantMiss bool) {
		t.Helper()
		f := &fakeEngine{chatResp: freshResp()}
		se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
		ctx := context.Background()
		if _, err := se.Chat(ctx, testReq()); err != nil {
			t.Fatalf("seed Chat: %v", err)
		}
		para := m64Paraphrase()
		para.CacheControls = &provider.CacheControls{Threshold: &thr}
		resp, err := se.Chat(ctx, para)
		if err != nil {
			t.Fatalf("threshold=%v paraphrase Chat: %v", thr, err)
		}
		if resp.Content != "4" {
			t.Fatalf("threshold=%v content = %q, want 4", thr, resp.Content)
		}
		if wantMiss {
			if f.chatCalls != 2 {
				t.Fatalf("threshold=%v chatCalls = %d, want 2: the floor must turn the hit into a miss", thr, f.chatCalls)
			}
			if _, _, ok := se.LastCacheHit(); ok {
				t.Fatalf("threshold=%v must not report a cache hit", thr)
			}
			return
		}
		if f.chatCalls != 1 {
			t.Fatalf("threshold=%v chatCalls = %d, want 1: the floor must keep the hit", thr, f.chatCalls)
		}
		if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
			t.Fatalf("threshold=%v LastCacheHit = %q,%v, want semantic,true", thr, kind, ok)
		}
	}
	t.Run("floor 1.0 misses", func(t *testing.T) { run(t, 1.0, true) })
	t.Run("floor 0.5 hits", func(t *testing.T) { run(t, 0.5, false) })
	// An out-of-range floor is treated as unset (backstop for
	// directly-constructed controls): the hit stands.
	t.Run("floor 2.0 ignored", func(t *testing.T) { run(t, 2.0, false) })
}

// TestM64KeyOverrideIsTenantScoped: two tenants sharing one
// override value never share an entry; the same tenant reuses
// the override bucket across different prompts; and an override
// request never hits (or pollutes) the message-derived key.
func TestM64KeyOverrideIsTenantScoped(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	alice := testReq()
	alice.CallerID = "gk-alice"
	alice.CacheControls = &provider.CacheControls{KeyOverride: "shared-bucket"}
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("alice override Chat: %v", err)
	}
	// Bob, same override, same prompt: must reach the provider.
	bob := testReq()
	bob.CallerID = "gk-bob"
	bob.CacheControls = &provider.CacheControls{KeyOverride: "shared-bucket"}
	if _, err := se.Chat(ctx, bob); err != nil {
		t.Fatalf("bob override Chat: %v", err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("bob override chatCalls = %d, want 2: no cross-tenant hit on a shared override", f.chatCalls)
	}
	// Alice, same override, DIFFERENT prompt: the override bucket
	// is the key, so this hits without a provider call.
	aliceOther := provider.ChatRequest{
		Model:         "test-model",
		Messages:      []provider.Message{{Role: "user", Content: "A completely different question?"}},
		CallerID:      "gk-alice",
		CacheControls: &provider.CacheControls{KeyOverride: "shared-bucket"},
	}
	resp, err := se.Chat(ctx, aliceOther)
	if err != nil {
		t.Fatalf("alice override other-prompt Chat: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("alice override other-prompt content = %q, want 4 (override bucket hit)", resp.Content)
	}
	if f.chatCalls != 2 {
		t.Fatalf("alice override other-prompt chatCalls = %d, want 2 (override bucket hit)", f.chatCalls)
	}
	// Alice WITHOUT the override, same prompt as the seed: the
	// override entry lives under a different key, so this misses
	// (and, with the provider now answering, proves the override
	// never polluted the message-derived key).
	plain := testReq()
	plain.CallerID = "gk-alice"
	if _, err := se.Chat(ctx, plain); err != nil {
		t.Fatalf("alice plain Chat: %v", err)
	}
	if f.chatCalls != 3 {
		t.Fatalf("alice plain chatCalls = %d, want 3: the override entry must not satisfy the derived key", f.chatCalls)
	}
}
