// M6.7 semantic threshold override: a per-call threshold
// narrows (raises) the similarity floor through
// GetTextForModelWithThreshold / GetForModelWithThreshold, and
// the effective floor never drops below DefaultSemanticThreshold
// (0.85). Similarities below come from the real EmbedText path
// (Cosine over the hashed bag-of-words vectors), never invented:
// each test first asserts the measured similarity lands in the
// band it needs, so a change to the embedding breaks loudly
// instead of silently passing.
package cache

import (
	"context"
	"testing"
	"time"
)

const m67Model = "test-model"

// m67NarrowPair is a store/query pair scoring in (0.85, 0.90):
// the default floor hits it, a 0.90 floor misses it.
const (
	m67NarrowStore = "what is the capital of France and how many people live there"
	m67NarrowQuery = "what is the capital of France and how many people live in Paris"
)

// m67BelowFloorPair is a store/query pair scoring in (0.80, 0.85):
// even a 0.50 override must miss it, because the override
// behaves as the 0.85 floor.
const (
	m67BelowFloorStore = "the quick brown fox jumps over the lazy dog"
	m67BelowFloorQuery = "the quick red fox jumps over the lazy dog"
)

func m67Seed(t *testing.T, c *LocalSemanticCache, text string) {
	t.Helper()
	c.SetTextForModel(context.Background(), m67Model, text, SemanticEntry{
		Response: CachedResponse{Content: "seeded", Model: m67Model},
	}, time.Hour)
}

// m67Sim asserts the real embedding similarity of store/query
// lands in (lo, hi) and returns it, so the test's premise is
// checked against the actual embedding instead of assumed.
func m67Sim(t *testing.T, store, query string, lo, hi float32) float32 {
	t.Helper()
	sim := Cosine(EmbedText(store), EmbedText(query))
	if sim <= lo || sim >= hi {
		t.Fatalf("similarity(%q, %q) = %.4f, want in (%.2f, %.2f): the embedding drifted, re-pick the pair", store, query, sim, lo, hi)
	}
	return sim
}

// TestM67ThresholdOverrideNarrows: a 0.90-threshold request does
// NOT match an entry scoring ~0.88, while the default 0.85 DOES
// match it — the override narrows, through the per-call
// parameter, not a post-filter.
func TestM67ThresholdOverrideNarrows(t *testing.T) {
	sim := m67Sim(t, m67NarrowStore, m67NarrowQuery, 0.85, 0.90)
	t.Logf("narrow pair similarity = %.4f", sim)
	ctx := context.Background()
	c := NewLocalSemanticCache()
	m67Seed(t, c, m67NarrowStore)

	if hit, found := c.GetTextForModel(ctx, m67Model, m67NarrowQuery); !found {
		t.Fatal("default GetTextForModel missed; want a hit (similarity above 0.85)")
	} else if hit.Similarity != sim {
		t.Fatalf("default hit similarity = %.4f, want %.4f (the real embedding score)", hit.Similarity, sim)
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.90); found {
		t.Fatal("WithThreshold(0.90) hit; want a miss (similarity below 0.90)")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.85); !found {
		t.Fatal("WithThreshold(0.85) missed; want a hit (similarity at the floor)")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0); !found {
		t.Fatal("WithThreshold(0) missed; want a hit (0 means no override)")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, -1); !found {
		t.Fatal("WithThreshold(-1) missed; want a hit (negative means no override)")
	}
	// Exact match at a 1.0 floor still hits; the narrowing pair
	// at a 1.0 floor misses.
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowStore, 1.0); !found {
		t.Fatal("WithThreshold(1.0) on identical text missed; want a hit")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 1.0); found {
		t.Fatal("WithThreshold(1.0) on the narrowing pair hit; want a miss")
	}
}

// TestM67ThresholdNeverBelowFloor: a 0.50 (or 0.80) override
// behaves as the 0.85 floor — an entry scoring ~0.84 still
// misses, where a true 0.50 floor would have hit it.
func TestM67ThresholdNeverBelowFloor(t *testing.T) {
	sim := m67Sim(t, m67BelowFloorStore, m67BelowFloorQuery, 0.80, 0.85)
	t.Logf("below-floor pair similarity = %.4f", sim)
	ctx := context.Background()
	c := NewLocalSemanticCache()
	m67Seed(t, c, m67BelowFloorStore)

	if _, found := c.GetTextForModel(ctx, m67Model, m67BelowFloorQuery); found {
		t.Fatal("default GetTextForModel hit; want a miss (similarity below 0.85)")
	}
	for _, thr := range []float32{0.50, 0.80, 0.84} {
		if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67BelowFloorQuery, thr); found {
			t.Fatalf("WithThreshold(%.2f) hit; want a miss — the override must behave as the 0.85 floor", thr)
		}
	}
	// And the floor does not swallow a genuinely close entry:
	// the narrowing pair still hits under a 0.50 override.
	m67Seed(t, c, m67NarrowStore)
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.50); !found {
		t.Fatal("WithThreshold(0.50) missed the narrowing pair; want a hit (0.50 behaves as 0.85, similarity above it)")
	}
}

// TestM67ThresholdAboveOneClamps: a directly-constructed
// threshold above 1 clamps to 1 (exact matches only) instead
// of matching nothing or panicking.
func TestM67ThresholdAboveOneClamps(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	m67Seed(t, c, m67NarrowStore)
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowStore, 1.5); !found {
		t.Fatal("WithThreshold(1.5) on identical text missed; want a hit (clamped to 1.0)")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 1.5); found {
		t.Fatal("WithThreshold(1.5) on the narrowing pair hit; want a miss (clamped to 1.0)")
	}
}

// TestM67ThresholdEmbeddingVariant: the same floor rule through
// the raw-embedding entry point (GetForModelWithThreshold),
// which is what non-text callers use.
func TestM67ThresholdEmbeddingVariant(t *testing.T) {
	m67Sim(t, m67NarrowStore, m67NarrowQuery, 0.85, 0.90)
	ctx := context.Background()
	c := NewLocalSemanticCache()
	m67Seed(t, c, m67NarrowStore)
	q := EmbedText(m67NarrowQuery)
	if _, found := c.GetForModelWithThreshold(ctx, m67Model, q, 0.90); found {
		t.Fatal("GetForModelWithThreshold(0.90) hit; want a miss")
	}
	if _, found := c.GetForModelWithThreshold(ctx, m67Model, q, 0.50); !found {
		t.Fatal("GetForModelWithThreshold(0.50) missed; want a hit (behaves as 0.85)")
	}
}

// TestM67ConfiguredThresholdIsAlsoFloored: a cache constructed
// with a below-floor configured threshold (0.70) still enforces
// 0.85 on the per-call path — the constructor's acceptance of a
// low value must not leak through WithThreshold.
func TestM67ConfiguredThresholdIsAlsoFloored(t *testing.T) {
	m67Sim(t, m67BelowFloorStore, m67BelowFloorQuery, 0.80, 0.85)
	ctx := context.Background()
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{Threshold: 0.70})
	m67Seed(t, c, m67BelowFloorStore)
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67BelowFloorQuery, 0.50); found {
		t.Fatal("WithThreshold(0.50) on a 0.70-configured cache hit; want a miss (floor is 0.85 regardless)")
	}
	if _, found := c.GetTextForModelWithThreshold(ctx, m67Model, m67BelowFloorQuery, 0); found {
		t.Fatal("WithThreshold(0) on a 0.70-configured cache hit; want a miss (floor is 0.85 regardless)")
	}
}

// TestM67WideVectorThreshold: the legacy full-scan path (vectors
// longer than SemanticVectorDims) applies the same per-call
// floor, so raw-embedding callers get the same rule.
func TestM67WideVectorThreshold(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	vec := make([]float32, SemanticVectorDims+16)
	for i := range vec {
		vec[i] = 1
	}
	c.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "wide", Model: m67Model}})
	// An identical wide vector scores 1.0: hits at any floor up
	// to 1.0, and a floor above the identical score is impossible
	// (clamped to 1.0), so the discriminating check is the
	// below-floor override: a 0.50 request on a 1.0-similarity
	// entry hits, and the entry is served by the legacy path.
	if hit, found := c.GetForModelWithThreshold(ctx, m67Model, vec, 0.50); !found || hit.Response.Content != "wide" {
		t.Fatalf("wide-vector WithThreshold(0.50) = %+v, %v; want the wide entry", hit, found)
	}
	if _, found := c.GetForModelWithThreshold(ctx, m67Model, vec, 1.0); !found {
		t.Fatal("wide-vector WithThreshold(1.0) on identical vector missed; want a hit")
	}
}

// TestM67ConstructorFloorsSubFloorThreshold (M6.7 review N1): a
// cache constructed with Threshold 0.70 must floor to 0.85 at
// construction, so even the legacy Get path (no WithThreshold)
// can never serve below the floor.
func TestM67ConstructorFloorsSubFloorThreshold(t *testing.T) {
	m67Sim(t, m67BelowFloorStore, m67BelowFloorQuery, 0.80, 0.85)
	ctx := context.Background()
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{Threshold: 0.70})
	m67Seed(t, c, m67BelowFloorStore)
	if _, found := c.GetTextForModel(ctx, m67Model, m67BelowFloorQuery); found {
		t.Fatal("legacy Get on a 0.70-configured cache hit a 0.8421-similarity entry; want a miss (constructor floors to 0.85)")
	}
}
