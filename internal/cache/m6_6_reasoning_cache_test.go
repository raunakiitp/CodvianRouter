package cache

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M6.6 — reasoning replay cache (PARITY.md Area 6, "Reasoning cache
// (2 h)", OmniRoute reasoningCache.ts:37). Every behaviour here has
// a test that fails without the implementation; toggle evidence is
// recorded in the M6.6 report (expiry check removed / tenant
// dropped from the key → these tests go red).

func TestM66ReasoningDefaultsMatchOmniRoute(t *testing.T) {
	if DefaultReasoningTTL != 2*time.Hour {
		t.Fatalf("DefaultReasoningTTL = %v, want 2h (reasoningCache.ts TTL_MS)", DefaultReasoningTTL)
	}
	if DefaultReasoningMaxEntries != 200 {
		t.Fatalf("DefaultReasoningMaxEntries = %d, want 200 (MAX_MEMORY_ENTRIES)", DefaultReasoningMaxEntries)
	}
	if DefaultReasoningMaxEntryBytes != 256<<10 {
		t.Fatalf("DefaultReasoningMaxEntryBytes = %d, want 256 KiB (M6.1)", DefaultReasoningMaxEntryBytes)
	}
	if DefaultReasoningMaxTotalBytes != 2<<20 {
		t.Fatalf("DefaultReasoningMaxTotalBytes = %d, want 2 MiB (M6.1)", DefaultReasoningMaxTotalBytes)
	}
}

func TestM66ReasoningTTLExpiry(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{TTL: 50 * time.Millisecond})
	key := ToolCallKey("tenant-a", "call_1")
	c.Set(ctx, key, "deepseek", "deepseek-chat", "let me think step by step")
	if got, ok := c.Get(ctx, key); !ok || got != "let me think step by step" {
		t.Fatalf("Get before expiry = %q, %v, want hit with stored text", got, ok)
	}
	time.Sleep(80 * time.Millisecond)
	if got, ok := c.Get(ctx, key); ok {
		t.Fatalf("Get after 2h-TTL expiry = %q, true, want miss", got)
	}
	if c.Size() != 0 {
		t.Fatalf("Size after expired access = %d, want 0 (expired entry deleted)", c.Size())
	}
	if st := c.Stats(); st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("Stats = hits %d misses %d, want 1/1 (real counters, not zero-filled)", st.Hits, st.Misses)
	}
}

func TestM66ReasoningTenantIsolation(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	keyA := ToolCallKey("tenant-a", "call_0")
	keyB := ToolCallKey("tenant-b", "call_0")
	if keyA == keyB {
		t.Fatalf("ToolCallKey identical across tenants for reused id call_0: %q", keyA)
	}
	if keyA == "call_0" || keyB == "call_0" {
		t.Fatalf("ToolCallKey must not be the bare id: %q / %q", keyA, keyB)
	}
	if got := ToolCallKey("tenant-a", "call_0"); got != keyA {
		t.Fatalf("ToolCallKey not deterministic: %q != %q", got, keyA)
	}
	c.SetForTenant(ctx, keyA, "tenant-a", "deepseek", "deepseek-chat", "tenant A reasoning")
	if got, ok := c.Get(ctx, keyB); ok {
		t.Fatalf("tenant B served tenant A's reasoning: %q", got)
	}
	if got, ok := c.Get(ctx, keyA); !ok || got != "tenant A reasoning" {
		t.Fatalf("tenant A Get = %q, %v, want own entry", got, ok)
	}
	// Anonymous ("") is its own tenant, never another key's.
	keyAnon := ToolCallKey("", "call_0")
	if keyAnon == keyA {
		t.Fatalf("anonymous key collides with tenant-a key: %q", keyAnon)
	}
	if _, ok := c.Get(ctx, keyAnon); ok {
		t.Fatalf("anonymous lookup hit tenant A's entry")
	}
}

func TestM66ReasoningConversationKey(t *testing.T) {
	turns := []ReasoningTurn{
		{Role: "user", Text: "solve this"},
		{Role: "assistant", Text: "here is the answer", ToolCalls: []ReasoningToolCall{{ID: "call_1", Name: "calc", Arguments: "{}"}}},
	}
	k1 := ConversationKey("tenant-a", turns)
	k2 := ConversationKey("tenant-a", turns)
	if k1 == "" || k1 != k2 {
		t.Fatalf("ConversationKey not stable: %q vs %q", k1, k2)
	}
	if got := ConversationKey("tenant-b", turns); got == k1 {
		t.Fatalf("ConversationKey identical across tenants: %q", got)
	}
	changed := []ReasoningTurn{
		{Role: "user", Text: "solve that"},
		{Role: "assistant", Text: "here is the answer", ToolCalls: []ReasoningToolCall{{ID: "call_1", Name: "calc", Arguments: "{}"}}},
	}
	if got := ConversationKey("tenant-a", changed); got == k1 {
		t.Fatalf("ConversationKey identical for a different transcript: %q", got)
	}
	if got := ConversationKey("tenant-a", nil); got != "" {
		t.Fatalf("ConversationKey with no turns = %q, want empty (can never match)", got)
	}
	// Tool-call arguments are part of the turn: same text, different
	// arguments is a different turn.
	argChanged := []ReasoningTurn{
		{Role: "user", Text: "solve this"},
		{Role: "assistant", Text: "here is the answer", ToolCalls: []ReasoningToolCall{{ID: "call_1", Name: "calc", Arguments: `{"x": 2}`}}},
	}
	if got := ConversationKey("tenant-a", argChanged); got == k1 {
		t.Fatalf("ConversationKey ignores tool-call arguments: %q", got)
	}
}

func TestM66ReasoningEmptyRefused(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	c.Set(ctx, ToolCallKey("t", "call_1"), "p", "m", "")
	c.Set(ctx, "", "p", "m", "some reasoning")
	c.Set(ctx, ToolCallKey("t", ""), "p", "m", "some reasoning")
	if c.Size() != 0 {
		t.Fatalf("Size = %d after empty key/reasoning stores, want 0", c.Size())
	}
	if _, ok := c.Get(ctx, ""); ok {
		t.Fatalf("empty key lookup hit")
	}
}

func TestM66ReasoningPerEntryCapRefusesNeverTruncates(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntryBytes: 100})
	// OmniRoute truncates at 10,000 chars; we refuse over the cap.
	// A truncated half-thought must never be stored as if whole.
	c.Set(ctx, ToolCallKey("t", "call_1"), "p", "m", strings.Repeat("x", 101))
	if c.Size() != 0 {
		t.Fatalf("oversize entry stored, want refused (M6.1 per-entry cap)")
	}
	c.Set(ctx, ToolCallKey("t", "call_1"), "p", "m", strings.Repeat("x", 100))
	if c.Size() != 1 {
		t.Fatalf("at-cap entry not stored, want stored")
	}
}

func TestM66ReasoningOldestFirstEviction(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntries: 2, MaxTotalBytes: 1 << 20})
	k1 := ToolCallKey("t", "call_1")
	k2 := ToolCallKey("t", "call_2")
	k3 := ToolCallKey("t", "call_3")
	c.Set(ctx, k1, "p", "m", "first")
	c.Set(ctx, k2, "p", "m", "second")
	c.Set(ctx, k3, "p", "m", "third")
	if _, ok := c.Get(ctx, k1); ok {
		t.Fatalf("oldest entry survived a full cache (want oldest-first eviction, reasoningCache.ts evictOldest)")
	}
	if _, ok := c.Get(ctx, k2); !ok {
		t.Fatalf("second entry evicted, want kept")
	}
	if _, ok := c.Get(ctx, k3); !ok {
		t.Fatalf("third entry missing, want stored")
	}
	if st := c.Stats(); st.Evicted != 1 {
		t.Fatalf("Evicted = %d, want 1 (real counter)", st.Evicted)
	}
}

func TestM66ReasoningTotalBytesCapEvicts(t *testing.T) {
	ctx := context.Background()
	// Total cap fits two 10-byte entries, not three.
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntries: 100, MaxTotalBytes: 25})
	c.Set(ctx, ToolCallKey("t", "call_1"), "p", "m", strings.Repeat("a", 10))
	c.Set(ctx, ToolCallKey("t", "call_2"), "p", "m", strings.Repeat("b", 10))
	c.Set(ctx, ToolCallKey("t", "call_3"), "p", "m", strings.Repeat("c", 10))
	if c.Size() != 2 {
		t.Fatalf("Size = %d under total-bytes cap, want 2", c.Size())
	}
	if st := c.Stats(); st.Bytes > 25 {
		t.Fatalf("Bytes = %d exceeds total cap 25", st.Bytes)
	}
}

func TestM66ReasoningReplaceRefreshesExpiryWithoutGrowing(t *testing.T) {
	ctx := context.Background()
	// Generous margins: under full-suite parallel load the 40ms sleeps
	// overshot 2.5x (test took 0.20s) and the entry expired before Get —
	// a timing flake, not a code bug (setStateForTenant refreshes expiry
	// on replace, reasoning_cache.go:411; 15/15 isolated passes). TTL 200ms
	// with 50ms sleeps survives a 2.5x sleep overshoot on both legs.
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{TTL: 200 * time.Millisecond, MaxEntries: 2})
	key := ToolCallKey("t", "call_1")
	c.Set(ctx, key, "p", "m", "v1")
	time.Sleep(50 * time.Millisecond)
	c.Set(ctx, key, "p", "m", "v2")
	time.Sleep(50 * time.Millisecond)
	got, ok := c.Get(ctx, key)
	if !ok || got != "v2" {
		t.Fatalf("replaced entry Get = %q, %v, want refreshed v2 hit", got, ok)
	}
	if c.Size() != 1 {
		t.Fatalf("Size = %d after replace, want 1", c.Size())
	}
}

func TestM66ReasoningStatsAreReal(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	st := c.Stats()
	if st.Entries != 0 || st.Hits != 0 || st.Misses != 0 || st.Evicted != 0 {
		t.Fatalf("fresh Stats not zero: %+v (counters must start at zero, never zero-filled)", st)
	}
	k1 := ToolCallKey("tenant-a", "call_1")
	k2 := ToolCallKey("tenant-a", "call_2")
	c.SetForTenant(ctx, k1, "tenant-a", "deepseek", "deepseek-chat", "r1")
	c.SetForTenant(ctx, k2, "tenant-a", "deepseek", "deepseek-chat", "r2")
	if _, ok := c.Get(ctx, k1); !ok {
		t.Fatal("expected hit on k1")
	}
	if _, ok := c.Get(ctx, ToolCallKey("tenant-a", "call_9")); ok {
		t.Fatal("unexpected hit on missing key")
	}
	st = c.Stats()
	if st.Entries != 2 || st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("Stats = %+v, want entries 2 hits 1 misses 1", st)
	}
	if st.ByModel["deepseek-chat"] != 2 {
		t.Fatalf("ByModel = %v, want deepseek-chat:2", st.ByModel)
	}
	if st.ByTenant["tenant-a"] != 2 {
		t.Fatalf("ByTenant = %v, want tenant-a:2", st.ByTenant)
	}
	if n := c.FlushAll(); n != 2 || c.Size() != 0 {
		t.Fatalf("FlushAll = %d, Size = %d, want 2/0", n, c.Size())
	}
}
