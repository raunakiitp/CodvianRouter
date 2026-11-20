// M6.7b threshold method on the persistent semantic adapter.
//
// PersistentSemanticCache.AsStore did not implement
// GetTextForModelWithThreshold, so serve mode narrowed through
// the M6.4 post-filter fallback in degrade.semanticLookup (same
// hit/miss outcome, but the floor enforced in the wrapper rather
// than in the cache). The adapter now delegates to the
// persistent cache's own threshold lookup, so serve mode takes
// the in-cache path. Similarities come from the real EmbedText
// path (Cosine over the hashed bag-of-words vectors), never
// invented: each test first asserts the measured similarity lands
// in the band it needs, reusing the M6.7 narrow/below-floor
// pairs (m67NarrowStore/m67NarrowQuery score in (0.85, 0.90);
// m67BelowFloorStore/m67BelowFloorQuery score in (0.80, 0.85)).
package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// m67bThresholdStore is the per-call-threshold extension of the
// semantic store (same method set as degrade's
// semanticThresholdStore, which the degrade wrapper
// type-asserts): a store offering it takes the request's
// effective floor into the lookup itself.
type m67bThresholdStore interface {
	GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool)
}

func m67bOpenPersistent(t *testing.T) (*PersistentSemanticCache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantic.db")
	p, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, path
}

// TestM67bPersistentStoreThresholdNarrows: a 0.90-threshold
// lookup misses an entry the 0.85 default hits — through the
// persistent wrapper itself, through its AsStore adapter, and
// through a reopened adapter after a restart (the entry and the
// narrowing both survive on disk).
func TestM67bPersistentStoreThresholdNarrows(t *testing.T) {
	sim := m67Sim(t, m67NarrowStore, m67NarrowQuery, 0.85, 0.90)
	t.Logf("narrow pair similarity = %.4f", sim)
	ctx := context.Background()
	p, path := m67bOpenPersistent(t)
	if err := p.SetTextForModel(ctx, m67Model, m67NarrowStore, SemanticEntry{
		Response: CachedResponse{Content: "seeded", Model: m67Model},
	}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}

	// The wrapper itself offers the threshold method, with the
	// same semantics as LocalSemanticCache (floor at 0.85, 0 =
	// configured).
	if hit, found := p.GetTextForModel(ctx, m67Model, m67NarrowQuery); !found {
		t.Fatal("persistent default GetTextForModel missed; want a hit (similarity above 0.85)")
	} else if hit.Similarity != sim {
		t.Fatalf("persistent default hit similarity = %.4f, want %.4f (the real embedding score)", hit.Similarity, sim)
	}
	if _, found := p.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.90); found {
		t.Fatal("persistent WithThreshold(0.90) hit; want a miss (similarity below 0.90)")
	}
	if _, found := p.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.85); !found {
		t.Fatal("persistent WithThreshold(0.85) missed; want a hit (similarity at the floor)")
	}
	if _, found := p.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0); !found {
		t.Fatal("persistent WithThreshold(0) missed; want a hit (0 means no override)")
	}
	if _, found := p.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.50); !found {
		t.Fatal("persistent WithThreshold(0.50) missed; want a hit (0.50 behaves as the 0.85 floor)")
	}

	// The AsStore adapter must offer the method too: this is what
	// flips serve mode from the post-filter fallback to the
	// in-cache path.
	store := p.AsStore()
	thr, ok := interface{}(store).(m67bThresholdStore)
	if !ok {
		t.Fatal("AsStore adapter does not implement GetTextForModelWithThreshold; serve mode still falls back to the post-filter")
	}
	if _, found := thr.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.90); found {
		t.Fatal("adapter WithThreshold(0.90) hit; want a miss (similarity below 0.90)")
	}
	if hit, found := thr.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.85); !found || hit.Response.Content != "seeded" {
		t.Fatalf("adapter WithThreshold(0.85) = %+v, %v; want the seeded entry", hit, found)
	}

	// Reopen: the entry survives on disk and the reopened
	// adapter narrows the same way.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen OpenPersistentSemanticCache: %v", err)
	}
	defer p2.Close()
	thr2, ok := interface{}(p2.AsStore()).(m67bThresholdStore)
	if !ok {
		t.Fatal("reopened AsStore adapter does not implement GetTextForModelWithThreshold")
	}
	if _, found := thr2.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.90); found {
		t.Fatal("reopened adapter WithThreshold(0.90) hit; want a miss")
	}
	if hit, found := thr2.GetTextForModelWithThreshold(ctx, m67Model, m67NarrowQuery, 0.85); !found || hit.Response.Content != "seeded" {
		t.Fatalf("reopened adapter WithThreshold(0.85) = %+v, %v; want the seeded entry", hit, found)
	}
}

// TestM67bPersistentStoreThresholdNeverBelowFloor: a 0.50 (or
// 0.80) override behaves as the 0.85 floor on the persistent
// adapter too — an entry scoring ~0.84 still misses, where a
// true 0.50 floor would have hit it.
func TestM67bPersistentStoreThresholdNeverBelowFloor(t *testing.T) {
	sim := m67Sim(t, m67BelowFloorStore, m67BelowFloorQuery, 0.80, 0.85)
	t.Logf("below-floor pair similarity = %.4f", sim)
	ctx := context.Background()
	p, _ := m67bOpenPersistent(t)
	if err := p.SetTextForModel(ctx, m67Model, m67BelowFloorStore, SemanticEntry{
		Response: CachedResponse{Content: "seeded", Model: m67Model},
	}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if _, found := p.GetTextForModel(ctx, m67Model, m67BelowFloorQuery); found {
		t.Fatal("persistent default GetTextForModel hit; want a miss (similarity below 0.85)")
	}
	for _, thr := range []float32{0.50, 0.80, 0.84} {
		if _, found := p.GetTextForModelWithThreshold(ctx, m67Model, m67BelowFloorQuery, thr); found {
			t.Fatalf("persistent WithThreshold(%.2f) hit; want a miss — the override must behave as the 0.85 floor", thr)
		}
	}
	adapter, ok := interface{}(p.AsStore()).(m67bThresholdStore)
	if !ok {
		t.Fatal("AsStore adapter does not implement GetTextForModelWithThreshold")
	}
	if _, found := adapter.GetTextForModelWithThreshold(ctx, m67Model, m67BelowFloorQuery, 0.50); found {
		t.Fatal("adapter WithThreshold(0.50) hit; want a miss — the override must behave as the 0.85 floor")
	}
}
