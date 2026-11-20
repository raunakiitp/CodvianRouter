// M6.7 semantic scope + threshold plumbing through the serving
// path (StaleEngine): the semantic cache now matches on the last
// 3 turns (SemanticScopeTurns), not just the last user message,
// with the same window at store and lookup; and the effective
// threshold (max of the 0.85 floor, the configured
// Policy.SemanticThreshold, and the per-request header floor) is
// pushed into the cache. The paraphrase pair's similarity is
// measured through the real embedding path (see the log line):
// "What is 2+2?" vs "What is 2+2 please?" scores ~0.92 under
// the "user: " window prefix, so a 0.95 floor misses it and a
// 0.90 floor hits it.
package degrade

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func m67Msg(role, text string) provider.Message {
	return provider.Message{Role: role, Content: text}
}

// m67ScopeStoreReq is the 3-turn conversation the scope tests
// seed: the assistant turn is the Paris answer.
func m67ScopeStoreReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67Msg("user", "What is the capital of France?"),
			m67Msg("assistant", "The capital of France is Paris, a large historic city on the river Seine famous for museums gardens and cafes."),
			m67Msg("user", "Tell me more about its history."),
		},
	}
}

// m67ScopeLookupReq is the same conversation with the assistant
// turn swapped to the Berlin answer. The last user message is
// identical, so the old last-user-message-only scope would match
// at similarity 1.0; the 3-turn window scores ~0.82, below the
// 0.85 floor, so the new scope must miss. (Measured with the
// real EmbedText/Cosine over the window strings; the test
// asserts the measured value in the band it needs.)
func m67ScopeLookupReq() provider.ChatRequest {
	req := m67ScopeStoreReq()
	req.Messages[1] = m67Msg("assistant", "The capital of Germany is Berlin, a large historic city on the river Spree famous for museums parks and nightlife.")
	return req
}

func m67WindowSim(t *testing.T, store, lookup provider.ChatRequest, lo, hi float32) float32 {
	t.Helper()
	sim := cache.Cosine(cache.EmbedText(semanticText(store)), cache.EmbedText(semanticText(lookup)))
	if sim <= lo || sim >= hi {
		t.Fatalf("window similarity = %.4f, want in (%.2f, %.2f): store window %q lookup window %q", sim, lo, hi, semanticText(store), semanticText(lookup))
	}
	return sim
}

// TestM67SemanticTextWindow: the exact window shape — the last 3
// non-system messages, "role: text" lines in order, older history
// dropped; empty-text messages contribute no line; system and
// developer messages are excluded even inside the window (see
// SemanticScopeTurns).
func TestM67SemanticTextWindow(t *testing.T) {
	req := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67Msg("system", "You are helpful."),
			m67Msg("user", "Old question?"),
			m67Msg("assistant", "Old answer."),
			m67Msg("user", "New question?"),
			m67Msg("assistant", ""),
		},
	}
	want := "assistant: Old answer.\nuser: New question?"
	if got := semanticText(req); got != want {
		t.Fatalf("semanticText = %q, want %q (last 3 turns only, empty text skipped)", got, want)
	}
	// A single user message keeps the "user: " prefix.
	if got := semanticText(testReq()); got != "user: What is 2+2?" {
		t.Fatalf("semanticText(single) = %q, want %q", got, "user: What is 2+2?")
	}
	// A system message inside the window is excluded: the window
	// is the same with or without it, so a different system
	// prompt cannot break a same-question match.
	sysReq := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67Msg("system", "Be brief."),
			m67Msg("user", "What is 2+2?"),
		},
	}
	if got := semanticText(sysReq); got != "user: What is 2+2?" {
		t.Fatalf("semanticText(system+user) = %q, want %q (system excluded)", got, "user: What is 2+2?")
	}
	devReq := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			m67Msg("developer", "Answer concisely."),
			m67Msg("user", "What is 2+2?"),
		},
	}
	if got := semanticText(devReq); got != "user: What is 2+2?" {
		t.Fatalf("semanticText(developer+user) = %q, want %q (developer excluded)", got, "user: What is 2+2?")
	}
	// No text anywhere: "".
	empty := provider.ChatRequest{Model: "test-model", Messages: []provider.Message{m67Msg("assistant", "")}}
	if got := semanticText(empty); got != "" {
		t.Fatalf("semanticText(empty) = %q, want empty", got)
	}
	// Text parts (array content) are joined, same as before.
	parts := provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Parts: []provider.ContentPart{{Type: "text", Text: "Hello"}, {Type: "text", Text: "there"}}}},
	}
	if got := semanticText(parts); got != "user: Hello there" {
		t.Fatalf("semanticText(parts) = %q, want %q", got, "user: Hello there")
	}
}

// TestM67ScopeIncludesEarlierTurns: the same last user message
// with a different assistant turn in the window must MISS — the
// scope is the last 3 turns, not the last user message alone.
func TestM67ScopeIncludesEarlierTurns(t *testing.T) {
	sim := m67WindowSim(t, m67ScopeStoreReq(), m67ScopeLookupReq(), 0.80, 0.85)
	t.Logf("differing-assistant window similarity = %.4f", sim)
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, m67ScopeStoreReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("seed chatCalls = %d, want 1", f.chatCalls)
	}
	resp, err := se.Chat(ctx, m67ScopeLookupReq())
	if err != nil {
		t.Fatalf("lookup Chat: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("lookup content = %q, want 4 (provider answer, not a cache hit)", resp.Content)
	}
	if f.chatCalls != 2 {
		t.Fatalf("lookup chatCalls = %d, want 2: the differing assistant turn must break the semantic match", f.chatCalls)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("differing-assistant lookup must not report a cache hit")
	}
}

// TestM67ScopeIgnoresOlderHistory: the same last 3 turns match
// even when older history differs — both directions: the lookup
// carries an extra older turn the store lacked, and the store
// carries one the lookup lacks. (The exact layer cannot serve
// either way: the full message lists differ, so the exact keys
// differ — the hit must come from the semantic window.)
func TestM67ScopeIgnoresOlderHistory(t *testing.T) {
	older := m67Msg("user", "Something else entirely?")
	t.Run("lookup has extra older turn", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
		ctx := context.Background()
		if _, err := se.Chat(ctx, m67ScopeStoreReq()); err != nil {
			t.Fatalf("seed Chat: %v", err)
		}
		lookup := m67ScopeStoreReq()
		lookup.Messages = append([]provider.Message{older}, lookup.Messages...)
		if got, want := semanticText(lookup), semanticText(m67ScopeStoreReq()); got != want {
			t.Fatalf("window with extra older turn = %q, want %q (identical window)", got, want)
		}
		resp, err := se.Chat(ctx, lookup)
		if err != nil {
			t.Fatalf("lookup Chat: %v", err)
		}
		if resp.Content != "4" {
			t.Fatalf("lookup content = %q, want 4 (semantic hit)", resp.Content)
		}
		if f.chatCalls != 1 {
			t.Fatalf("lookup chatCalls = %d, want 1: identical last 3 turns must hit semantically", f.chatCalls)
		}
		if kind, sim, ok := se.LastCacheHit(); !ok || kind != "semantic" || sim != 1.0 {
			t.Fatalf("LastCacheHit = %q,%.4f,%v, want semantic,1.0,true", kind, sim, ok)
		}
	})
	t.Run("store has extra older turn", func(t *testing.T) {
		f := &fakeEngine{chatResp: freshResp()}
		se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
		ctx := context.Background()
		seed := m67ScopeStoreReq()
		seed.Messages = append([]provider.Message{older}, seed.Messages...)
		if _, err := se.Chat(ctx, seed); err != nil {
			t.Fatalf("seed Chat: %v", err)
		}
		resp, err := se.Chat(ctx, m67ScopeStoreReq())
		if err != nil {
			t.Fatalf("lookup Chat: %v", err)
		}
		if resp.Content != "4" {
			t.Fatalf("lookup content = %q, want 4 (semantic hit)", resp.Content)
		}
		if f.chatCalls != 1 {
			t.Fatalf("lookup chatCalls = %d, want 1: identical last 3 turns must hit semantically", f.chatCalls)
		}
		if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
			t.Fatalf("LastCacheHit kind = %q,%v, want semantic,true", kind, ok)
		}
	})
}

// TestM67ThresholdNarrowing: the paraphrase hits by default; a
// 0.95 per-request floor turns it into a miss, a 0.90 floor
// keeps the hit, and a 0.50 floor keeps the hit (it behaves as
// the 0.85 floor, and the paraphrase scores above it).
func TestM67ThresholdNarrowing(t *testing.T) {
	sim := cache.Cosine(cache.EmbedText(semanticText(testReq())), cache.EmbedText(semanticText(m64Paraphrase())))
	if sim <= 0.90 || sim >= 0.95 {
		t.Fatalf("paraphrase window similarity = %.4f, want in (0.90, 0.95)", sim)
	}
	t.Logf("paraphrase window similarity = %.4f", sim)
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
			t.Fatalf("threshold=%v Chat: %v", thr, err)
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
	t.Run("floor 0.95 misses", func(t *testing.T) { run(t, 0.95, true) })
	t.Run("floor 0.90 hits", func(t *testing.T) { run(t, 0.90, false) })
	t.Run("floor 0.50 hits (behaves as 0.85)", func(t *testing.T) { run(t, 0.50, false) })
}

// TestM67BelowFloorHeaderStillMisses: a 0.50 header on the
// differing-assistant lookup must STILL miss — the entry scores
// ~0.82, below the 0.85 floor, and the 0.50 override behaves as
// 0.85 rather than as 0.50 (a true 0.50 floor would hit it).
func TestM67BelowFloorHeaderStillMisses(t *testing.T) {
	m67WindowSim(t, m67ScopeStoreReq(), m67ScopeLookupReq(), 0.80, 0.85)
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, m67ScopeStoreReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	lookup := m67ScopeLookupReq()
	thr := float32(0.50)
	lookup.CacheControls = &provider.CacheControls{Threshold: &thr}
	if _, err := se.Chat(ctx, lookup); err != nil {
		t.Fatalf("lookup Chat: %v", err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("lookup chatCalls = %d, want 2: a 0.50 override must behave as the 0.85 floor and miss", f.chatCalls)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("0.50-override lookup must not report a cache hit")
	}
}

// TestM67PolicyThreshold: the configured floor (Policy.
// SemanticThreshold) applies without any header; a header can
// only raise it further, never lower it; out-of-range policy
// values are ignored (the floor stays 0.85).
func TestM67PolicyThreshold(t *testing.T) {
	run := func(t *testing.T, policyThr float32, headerThr *float32, wantMiss bool) {
		t.Helper()
		f := &fakeEngine{chatResp: freshResp()}
		policy := serveCachePolicy()
		policy.SemanticThreshold = policyThr
		se, _, _, _ := wrapSemanticTest(f, policy)
		ctx := context.Background()
		if _, err := se.Chat(ctx, testReq()); err != nil {
			t.Fatalf("seed Chat: %v", err)
		}
		para := m64Paraphrase()
		if headerThr != nil {
			para.CacheControls = &provider.CacheControls{Threshold: headerThr}
		}
		if _, err := se.Chat(ctx, para); err != nil {
			t.Fatalf("paraphrase Chat: %v", err)
		}
		if wantMiss && f.chatCalls != 2 {
			t.Fatalf("policy=%v header=%v chatCalls = %d, want 2 (miss)", policyThr, headerThr, f.chatCalls)
		}
		if !wantMiss && f.chatCalls != 1 {
			t.Fatalf("policy=%v header=%v chatCalls = %d, want 1 (hit)", policyThr, headerThr, f.chatCalls)
		}
	}
	thr90 := float32(0.90)
	t.Run("policy 0.95 misses", func(t *testing.T) { run(t, 0.95, nil, true) })
	t.Run("policy 0.90 hits", func(t *testing.T) { run(t, 0.90, nil, false) })
	t.Run("policy 0.50 behaves as 0.85, hits", func(t *testing.T) { run(t, 0.50, nil, false) })
	t.Run("policy 0.95 + header 0.90 still misses", func(t *testing.T) { run(t, 0.95, &thr90, true) })
	t.Run("policy 2.0 ignored, hits", func(t *testing.T) { run(t, 2.0, nil, false) })
	t.Run("policy 0 unset, hits", func(t *testing.T) { run(t, 0, nil, false) })
}

// TestM67EffectiveThreshold: the resolution table — max(0.85,
// policy, header), with out-of-range values ignored.
func TestM67EffectiveThreshold(t *testing.T) {
	thr := func(v float32) *float32 { return &v }
	cases := []struct {
		name   string
		policy float32
		header *float32
		want   float32
	}{
		{"defaults", 0, nil, 0.85},
		{"policy raises", 0.90, nil, 0.90},
		{"header raises", 0, thr(0.92), 0.92},
		{"higher of policy and header wins", 0.90, thr(0.95), 0.95},
		{"header cannot lower policy", 0.95, thr(0.90), 0.95},
		{"header below floor behaves as floor", 0, thr(0.50), 0.85},
		{"policy below floor ignored", 0.50, nil, 0.85},
		{"policy above 1 ignored", 2.0, nil, 0.85},
		{"header above 1 ignored", 0, thr(2.0), 0.85},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := &StaleEngine{policy: Policy{SemanticThreshold: tc.policy}}
			req := testReq()
			if tc.header != nil {
				req.CacheControls = &provider.CacheControls{Threshold: tc.header}
			}
			if got := se.effectiveSemanticThreshold(req); got != tc.want {
				t.Fatalf("effectiveSemanticThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// m67LegacyStore is a SemanticStore WITHOUT the per-call
// threshold method: it delegates to LocalSemanticCache's legacy
// lookup, so the degrade wrapper must serve it through the
// post-filter fallback (the shape the persistent adapter has
// today). It must narrow exactly like the threshold-aware path.
type m67LegacyStore struct{ inner *cache.LocalSemanticCache }

func (s m67LegacyStore) GetTextForModel(ctx context.Context, model, text string) (cache.SemanticEntry, bool) {
	return s.inner.GetTextForModel(ctx, model, text)
}
func (s m67LegacyStore) SetTextForModel(ctx context.Context, model, text string, entry cache.SemanticEntry, ttl time.Duration) {
	s.inner.SetTextForModel(ctx, model, text, entry, ttl)
}
func (s m67LegacyStore) Stats() (entries, bytes int) { return s.inner.Stats() }

// TestM67LegacyStoreFallbackNarrows: through a store without
// GetTextForModelWithThreshold, a 0.95 floor still misses the
// ~0.92 paraphrase and the default still hits it — the fallback
// post-filter enforces the same effective threshold.
func TestM67LegacyStoreFallbackNarrows(t *testing.T) {
	sim := cache.Cosine(cache.EmbedText(semanticText(testReq())), cache.EmbedText(semanticText(m64Paraphrase())))
	if sim <= 0.90 || sim >= 0.95 {
		t.Fatalf("paraphrase window similarity = %.4f, want in (0.90, 0.95)", sim)
	}
	run := func(t *testing.T, thr *float32, wantMiss bool) {
		t.Helper()
		f := &fakeEngine{chatResp: freshResp()}
		exact := cache.NewExactCache(100)
		sem := cache.NewLocalSemanticCache()
		eng := WrapWithSemantic(f, exact, m67LegacyStore{inner: sem}, serveCachePolicy())
		se, ok := eng.(*StaleEngine)
		if !ok {
			t.Fatal("WrapWithSemantic did not return *StaleEngine")
		}
		ctx := context.Background()
		if _, err := se.Chat(ctx, testReq()); err != nil {
			t.Fatalf("seed Chat: %v", err)
		}
		para := m64Paraphrase()
		if thr != nil {
			para.CacheControls = &provider.CacheControls{Threshold: thr}
		}
		if _, err := se.Chat(ctx, para); err != nil {
			t.Fatalf("paraphrase Chat: %v", err)
		}
		if wantMiss && f.chatCalls != 2 {
			t.Fatalf("legacy store chatCalls = %d, want 2 (miss)", f.chatCalls)
		}
		if !wantMiss && f.chatCalls != 1 {
			t.Fatalf("legacy store chatCalls = %d, want 1 (hit)", f.chatCalls)
		}
	}
	thr95 := float32(0.95)
	t.Run("default hits", func(t *testing.T) { run(t, nil, false) })
	t.Run("floor 0.95 misses", func(t *testing.T) { run(t, &thr95, true) })
	// The legacy store must NOT satisfy the threshold-aware
	// interface: if it ever does, this test's premise is gone
	// (the test would be exercising the new path, not the
	// fallback).
	if _, ok := interface{}(m67LegacyStore{}).(semanticThresholdStore); ok {
		t.Fatal("m67LegacyStore unexpectedly implements semanticThresholdStore; the fallback is untested")
	}
}
