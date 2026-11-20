package cache

// Inverted-index tests (CHECKLIST 149-150, benchmark slice).
//
// The inverted index is an optimisation, not a new similarity
// function: Get must return exactly what the old full scan
// returned — same best entry, same similarity (up to float
// summation order, ~1e-16), same threshold behaviour — for every
// cache state, including after evictions (stale postings) and
// rebuilds. TestSemanticGetMatchesBruteForce is the property test
// that pins that: it scores every live entry with the reference
// dense cosine and compares.
import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// bruteForceBest is the reference implementation Get must agree
// with: the best live, unexpired entry by dense cosine, with the
// cache's threshold applied. It reads the cache's own slots, so it
// shares the entry set (and the expiry clock) with the code under
// test — what it pins is the scoring/selection, not the storage.
func bruteForceBest(c *LocalSemanticCache, query []float32) (SemanticEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	bestSlot := -1
	var best float32
	for slot, e := range c.slots {
		if e == nil || now.After(e.expires) || e.norm == 0 {
			continue
		}
		if sim := cosine(query, e.vec); sim > best {
			best, bestSlot = sim, slot
		}
	}
	if bestSlot < 0 || best < c.threshold {
		return SemanticEntry{}, false
	}
	hit := c.slots[bestSlot].entry
	hit.Similarity = best
	return hit, true
}

func TestSemanticGetMatchesBruteForce(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(20261118))
	words := []string{"explain", "how", "python", "windows", "install", "step", "guide", "cache", "router", "token", "model", "fast", "cheap", "stream", "retry", "quota", "limit", "docker", "linux", "memory", "query", "search", "index", "vector", "score"}
	text := func() string {
		n := 3 + rng.Intn(8)
		s := ""
		for i := 0; i < n; i++ {
			if i > 0 {
				s += " "
			}
			s += words[rng.Intn(len(words))]
		}
		return s
	}
	check := func(c *LocalSemanticCache, query string) {
		t.Helper()
		q := EmbedText(query)
		got, gotOK := c.Get(ctx, q)
		want, wantOK := bruteForceBest(c, q)
		if gotOK != wantOK {
			t.Fatalf("Get(%q) ok=%v, brute force ok=%v (size %d)", query, gotOK, wantOK, c.Size())
		}
		if !gotOK {
			return
		}
		if got.Response.Content != want.Response.Content {
			t.Fatalf("Get(%q) content=%q, brute force %q", query, got.Response.Content, want.Response.Content)
		}
		if d := got.Similarity - want.Similarity; d > 1e-5 || d < -1e-5 {
			t.Fatalf("Get(%q) similarity=%v, brute force %v", query, got.Similarity, want.Similarity)
		}
	}
	// Small cache with heavy churn: every store past 20 entries
	// evicts, so postings go stale and the bulk rebuild fires
	// repeatedly; Get must still agree with brute force throughout.
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntries: 20, TTL: time.Hour})
	for i := 0; i < 300; i++ {
		tx := text()
		c.SetText(ctx, tx, SemanticEntry{Response: CachedResponse{Content: fmt.Sprintf("resp-%d", i)}}, time.Hour)
		if i%7 == 0 {
			check(c, text())
		}
	}
	// Same queries must agree after the churn, including queries
	// for texts stored long ago (evicted) and recent ones.
	for i := 0; i < 50; i++ {
		check(c, text())
	}
	if c.Size() > 20 {
		t.Fatalf("Size = %d, want <= 20", c.Size())
	}
	// The index must stay bounded: postings never run away with
	// churn (stale postings are reclaimed by rebuild).
	c.mu.Lock()
	posted, live := c.postedTotal, c.livePosts
	c.mu.Unlock()
	if posted > 2*live+512 {
		t.Fatalf("postedTotal=%d livePosts=%d: stale postings unbounded", posted, live)
	}
}

func TestSemanticSetExactDedup(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	vec := EmbedText("explain how to install python on windows step by step")
	c.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "first"}})
	c.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "second"}})
	if c.Size() != 1 {
		t.Fatalf("Size after identical re-store = %d, want 1", c.Size())
	}
	got, ok := c.Get(ctx, vec)
	if !ok || got.Response.Content != "second" {
		t.Fatalf("Get after identical re-store = (%+v, %v), want second", got, ok)
	}
	// A merely near-identical vector (cosine >= 0.9999, not
	// identical) now takes its own slot: the old near-duplicate
	// scan is gone, and the LRU bounds the duplication. This is
	// the documented behaviour change of the 2026-11-18 index
	// rewrite — the cache stays correct, just less deduplicating.
	// (One extra token moves a short text's cosine to ~0.97, far
	// below 0.9999, so the near vector here is built by nudging
	// one component of the base vector by 0.01% — the smallest
	// realistic near-duplicate at this threshold.)
	near := make([]float32, len(vec))
	copy(near, vec)
	for i, v := range near {
		if v != 0 {
			near[i] = v * 1.0001
			break
		}
	}
	if sim := cosine(vec, near); sim < 0.9999 {
		t.Fatalf("near vector cosine = %v, want >= 0.9999", sim)
	}
	c2 := NewLocalSemanticCache()
	c2.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "first"}})
	c2.Set(ctx, near, SemanticEntry{Response: CachedResponse{Content: "near"}})
	if c2.Size() != 2 {
		t.Fatalf("Size after near-identical store = %d, want 2", c2.Size())
	}
}

func TestSemanticWideVectorFallback(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	// A 512-dim raw vector: the index cannot cover it, so Get
	// must fall back to the exact full scan — same result as the
	// reference cosine, no panic, no silent miss.
	wide := make([]float32, 512)
	for i := range wide {
		if i%7 == 0 {
			wide[i] = 0.5
		}
	}
	// Normalise (L2) so cosine is meaningful.
	var sum float64
	for _, v := range wide {
		sum += float64(v) * float64(v)
	}
	if sum > 0 {
		inv := 1.0 / math.Sqrt(sum)
		for i := range wide {
			wide[i] = float32(float64(wide[i]) * inv)
		}
	}
	c.Set(ctx, wide, SemanticEntry{Response: CachedResponse{Content: "wide"}})
	// A 256-dim query must still find the wide entry only through
	// the legacy path (wideCount > 0 forces it).
	got, ok := c.Get(ctx, wide)
	if !ok || got.Response.Content != "wide" {
		t.Fatalf("Get(wide) = (%+v, %v), want wide hit", got, ok)
	}
	// And a 256-dim entry stored alongside still resolves.
	c.SetText(ctx, "explain how to install python", SemanticEntry{Response: CachedResponse{Content: "narrow"}}, time.Hour)
	got, ok = c.GetText(ctx, "explain how to install python")
	if !ok || got.Response.Content != "narrow" {
		t.Fatalf("GetText(narrow) with a wide entry live = (%+v, %v), want narrow hit", got, ok)
	}
}

// BenchmarkSemanticGet500 / BenchmarkSemanticSet500 pin the hot-path
// cost of the semantic cache at a full 500 entries (CHECKLIST 149):
// the numbers to beat are the pre-index ones measured 2026-11-18
// (Get ~120 µs/op, Set ~76 µs/op on this VM's microbenchmarks).
func BenchmarkSemanticGet500(b *testing.B) {
	c := NewLocalSemanticCache()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		text := fmt.Sprintf("Explain topic number %d with some distinct words alpha%d beta%d gamma%d in one sentence.", i, i, i, i)
		c.SetText(ctx, text, SemanticEntry{Response: CachedResponse{Content: "x"}}, time.Hour)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.GetText(ctx, "Explain photosynthesis in desert cacti in one sentence.")
	}
}

func BenchmarkSemanticSet500(b *testing.B) {
	c := NewLocalSemanticCache()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		text := fmt.Sprintf("Explain topic number %d with some distinct words alpha%d beta%d gamma%d in one sentence.", i, i, i, i)
		c.SetText(ctx, text, SemanticEntry{Response: CachedResponse{Content: "x"}}, time.Hour)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SetText(ctx, fmt.Sprintf("Explain fresh topic %d with new words delta%d epsilon%d in one sentence.", i, i, i), SemanticEntry{Response: CachedResponse{Content: "x"}}, time.Hour)
	}
}

// BenchmarkSemanticGetScoped500 pins the hot-path cost of
// getScoped alone (CHECKLIST 263–264, follow-up to the 249–250
// profile: getScoped 1.14 s cum, 3.1% of router CPU): the query
// embedding is precomputed, so the number is the lookup itself
// (index walk + best-slot scan), not the text embedding that
// GetText adds on top. A <= 0 dot skip in the best-slot scan
// was tried here and REVERTED: before/after minima overlap
// (shared-vocab 12,180 vs 12,155 ns/op; disjoint 2,039 vs
// 2,365 ns/op — all within run-to-run noise on this VM), so
// no win is claimed. The divisions are not on the critical
// path: the max-scan loop is bound by the per-slot checks and
// loads, and the divide latency hides behind out-of-order
// execution (no dependency chain — the result feeds a compare
// that rarely updates best).
func BenchmarkSemanticGetScoped500(b *testing.B) {
	c := NewLocalSemanticCache()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		text := fmt.Sprintf("Explain topic number %d with some distinct words alpha%d beta%d gamma%d in one sentence.", i, i, i, i)
		c.SetText(ctx, text, SemanticEntry{Response: CachedResponse{Content: "x"}}, time.Hour)
	}
	query := EmbedText("Explain photosynthesis in desert cacti in one sentence.")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, query)
	}
}

// BenchmarkSemanticGetScoped500Disjoint pins getScoped when the
// query shares no embedding dimension with any stored entry (the
// pure best-slot-scan case: every accumulated dot is exactly 0,
// so no slot can win and the scan is all overhead). It is the
// discriminating case for best-slot-scan costs — the
// shared-vocabulary bench above hides them behind posting
// overlap. A <= 0 dot skip was tried and reverted on this bench
// too (no measurable win — see BenchmarkSemanticGetScoped500).
func BenchmarkSemanticGetScoped500Disjoint(b *testing.B) {
	c := NewLocalSemanticCache()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		vec := make([]float32, SemanticVectorDims)
		for d := 0; d < 10; d++ {
			vec[(i*7+d*13)%128] = 0.3
		}
		c.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "x"}})
	}
	query := make([]float32, SemanticVectorDims)
	for d := 128; d < 138; d++ {
		query[d] = 0.3
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, query)
	}
}
