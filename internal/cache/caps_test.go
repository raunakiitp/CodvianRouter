package cache

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M6.1 — cache caps: per-entry size cap (256 KiB default) + total
// memory cap (2 MiB default), on both layers. Entry size is exact:
// exact = response content bytes; semantic = vector bytes (4 per
// float32) + response content bytes.

func TestExactCachePerEntrySizeCap(t *testing.T) {
	c := NewExactCacheWithCaps(100, 1024, 1<<20)
	ctx := context.Background()
	// 2 KiB of content exceeds the 1 KiB per-entry cap: refused.
	c.Set(ctx, "big", CachedResponse{Content: strings.Repeat("x", 2048)}, time.Hour)
	if _, ok := c.Get(ctx, "big"); ok {
		t.Error("oversize entry must not be stored")
	}
	// Exactly at the cap stores.
	c.Set(ctx, "edge", CachedResponse{Content: strings.Repeat("x", 1024)}, time.Hour)
	if _, ok := c.Get(ctx, "edge"); !ok {
		t.Error("entry exactly at the per-entry cap must be stored")
	}
	// Small entry stores.
	c.Set(ctx, "small", CachedResponse{Content: "hi"}, time.Hour)
	if _, ok := c.Get(ctx, "small"); !ok {
		t.Error("small entry must be stored")
	}
	if entries, _ := c.Stats(); entries != 2 {
		t.Errorf("entries = %d, want 2 (big refused, edge + small stored)", entries)
	}
}

func TestExactCacheTotalBytesCap(t *testing.T) {
	c := NewExactCacheWithCaps(100, 1<<20, 2048)
	ctx := context.Background()
	c.Set(ctx, "k1", CachedResponse{Content: strings.Repeat("a", 1024)}, time.Hour)
	c.Set(ctx, "k2", CachedResponse{Content: strings.Repeat("b", 1024)}, time.Hour)
	// Cache is exactly at the 2048-byte cap; a third 1 KiB entry
	// must evict the least recently used (k1).
	c.Set(ctx, "k3", CachedResponse{Content: strings.Repeat("c", 1024)}, time.Hour)
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Error("k1 must be evicted when the total cap is exceeded")
	}
	if _, ok := c.Get(ctx, "k2"); !ok {
		t.Error("k2 must survive (eviction is LRU, k1 was oldest)")
	}
	if _, ok := c.Get(ctx, "k3"); !ok {
		t.Error("k3 must be stored")
	}
	if entries, bytes := c.Stats(); entries != 2 || bytes != 2048 {
		t.Errorf("Stats() = (%d, %d), want (2, 2048)", entries, bytes)
	}
}

func TestExactCacheReplaceFreesBytes(t *testing.T) {
	c := NewExactCacheWithCaps(100, 1<<20, 2048)
	ctx := context.Background()
	c.Set(ctx, "k1", CachedResponse{Content: strings.Repeat("a", 1024)}, time.Hour)
	c.Set(ctx, "k2", CachedResponse{Content: strings.Repeat("b", 1024)}, time.Hour)
	// Replace k1 (1024 B) with a 100 B response: 924 B freed.
	c.Set(ctx, "k1", CachedResponse{Content: strings.Repeat("a", 100)}, time.Hour)
	if entries, bytes := c.Stats(); entries != 2 || bytes != 1124 {
		t.Fatalf("Stats() after replace = (%d, %d), want (2, 1124)", entries, bytes)
	}
	// A new 1 KiB entry now needs 1024 B but only 924 B are free:
	// the LRU entry (k2, untouched since its store) is evicted.
	c.Set(ctx, "k3", CachedResponse{Content: strings.Repeat("c", 1024)}, time.Hour)
	if _, ok := c.Get(ctx, "k2"); ok {
		t.Error("k2 must be evicted to make room for k3")
	}
	if _, ok := c.Get(ctx, "k1"); !ok {
		t.Error("k1 must survive (most recently used by the replace)")
	}
	if _, ok := c.Get(ctx, "k3"); !ok {
		t.Error("k3 must be stored")
	}
}

func TestExactCacheOversizeReplaceDoesNotDelete(t *testing.T) {
	c := NewExactCacheWithCaps(100, 1024, 1<<20)
	ctx := context.Background()
	c.Set(ctx, "k1", CachedResponse{Content: "original"}, time.Hour)
	// An oversize replacement is refused — and must NOT delete the
	// entry it would have replaced.
	c.Set(ctx, "k1", CachedResponse{Content: strings.Repeat("x", 2048)}, time.Hour)
	resp, ok := c.Get(ctx, "k1")
	if !ok || resp.Content != "original" {
		t.Errorf("Get(k1) = (%q, %v), want (original, true)", resp.Content, ok)
	}
}

func TestExactCacheStatsAndFlush(t *testing.T) {
	c := NewExactCacheWithCaps(100, 1<<20, 1<<20)
	ctx := context.Background()
	c.Set(ctx, "k1", CachedResponse{Content: strings.Repeat("a", 100)}, time.Hour)
	c.Set(ctx, "k2", CachedResponse{Content: strings.Repeat("b", 200)}, time.Hour)
	if entries, bytes := c.Stats(); entries != 2 || bytes != 300 {
		t.Errorf("Stats() = (%d, %d), want (2, 300)", entries, bytes)
	}
	if n := c.FlushAll(); n != 2 {
		t.Errorf("FlushAll() = %d, want 2", n)
	}
	if entries, bytes := c.Stats(); entries != 0 || bytes != 0 {
		t.Errorf("Stats() after flush = (%d, %d), want (0, 0)", entries, bytes)
	}
}

func TestSemanticCachePerEntrySizeCap(t *testing.T) {
	// The 256-dim vector alone is 1024 B, so the per-entry cap
	// must exceed that for anything to store: 2048 B here.
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntryBytes: 2048, MaxTotalBytes: 1 << 20})
	ctx := context.Background()
	// 2 KiB of content + 1 KiB vector = 3072 B > 2048: refused.
	c.SetText(ctx, "alpha bravo", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("x", 2048)}}, time.Hour)
	if _, ok := c.GetText(ctx, "alpha bravo"); ok {
		t.Error("oversize semantic entry must not be stored")
	}
	// 100 B of content + 1 KiB vector = 1124 B <= 2048: stored.
	c.SetText(ctx, "charlie delta", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("y", 100)}}, time.Hour)
	if _, ok := c.GetText(ctx, "charlie delta"); !ok {
		t.Error("small semantic entry must be stored")
	}
	if entries, _ := c.Stats(); entries != 1 {
		t.Errorf("entries = %d, want 1", entries)
	}
}

func TestSemanticCacheTotalBytesCap(t *testing.T) {
	// Two ~1124 B entries (1 KiB vector + 100 B content) fit in
	// 2500 B; a third must evict the least recently used.
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntryBytes: 2048, MaxTotalBytes: 2500})
	ctx := context.Background()
	c.SetText(ctx, "alpha bravo", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("a", 100)}}, time.Hour)
	c.SetText(ctx, "charlie delta", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("b", 100)}}, time.Hour)
	c.SetText(ctx, "echo foxtrot", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("c", 100)}}, time.Hour)
	if _, ok := c.GetText(ctx, "alpha bravo"); ok {
		t.Error("alpha bravo must be evicted when the total cap is exceeded")
	}
	if _, ok := c.GetText(ctx, "charlie delta"); !ok {
		t.Error("charlie delta must survive")
	}
	if _, ok := c.GetText(ctx, "echo foxtrot"); !ok {
		t.Error("echo foxtrot must be stored")
	}
	if entries, bytes := c.Stats(); entries != 2 || bytes > 2500 {
		t.Errorf("Stats() = (%d, %d), want (2, <= 2500)", entries, bytes)
	}
}

func TestSemanticCacheReplaceKeepsBytesHonest(t *testing.T) {
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntryBytes: 4096, MaxTotalBytes: 1 << 20})
	ctx := context.Background()
	c.SetText(ctx, "alpha bravo", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("a", 100)}}, time.Hour)
	// Same text = same vector = dedup replace: size stays 1, bytes
	// follow the NEW response (1024 vector + 200 content = 1224).
	c.SetText(ctx, "alpha bravo", SemanticEntry{Response: CachedResponse{Content: strings.Repeat("b", 200)}}, time.Hour)
	entries, bytes := c.Stats()
	if entries != 1 || bytes != 1224 {
		t.Errorf("Stats() after dedup replace = (%d, %d), want (1, 1224)", entries, bytes)
	}
	if got, ok := c.GetText(ctx, "alpha bravo"); !ok || got.Response.Content != strings.Repeat("b", 200) {
		t.Errorf("GetText after replace: ok=%v, want the new 200 B response", ok)
	}
}
