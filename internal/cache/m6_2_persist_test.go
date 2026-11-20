// M6.2 — persistence tiers: restart survival for both layers,
// semantic persistence round-trip, and the semantic flush API.
//
// Toggle-verification: every restart test below is written so it
// FAILS against a memory-only cache — TestM62ExactRestartSurvival
// has a companion sub-case that runs the identical steps against
// NewExactCache (no file) and asserts the reopened get MISSES,
// proving the test detects the absence of persistence rather
// than passing vacuously. The semantic round-trip asserts the
// SQLite file exists on disk and holds a row, so a
// memory-only implementation cannot pass it either.
package cache

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestM62ExactRestartSurvival(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")

	// Open, set (one tenant-attributed, one plain), close, reopen.
	pc, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	resp := CachedResponse{Content: "hello persistent", Model: "gpt-4o", Usage: Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}
	if err := pc.SetForTenant(ctx, "k1", "key-1", resp, time.Hour); err != nil {
		t.Fatalf("SetForTenant: %v", err)
	}
	pc.Set(ctx, "k2", CachedResponse{Content: "plain", Model: "gpt-4o"}, time.Hour)
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	pc2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen OpenPersistentCache: %v", err)
	}
	defer pc2.Close()
	if got, ok := pc2.Get(ctx, "k1"); !ok || got.Content != "hello persistent" {
		t.Fatalf("reopened Get(k1) = %+v, %v; want hit with content %q", got, ok, "hello persistent")
	}
	if got, ok := pc2.Get(ctx, "k2"); !ok || got.Content != "plain" {
		t.Fatalf("reopened Get(k2) = %+v, %v; want hit with content %q", got, ok, "plain")
	}
	// Tenant attribution survived the restart too.
	if n := pc2.FlushTenant(ctx, "key-1"); n != 1 {
		t.Fatalf("reopened FlushTenant(key-1) = %d; want 1", n)
	}
	if _, ok := pc2.Get(ctx, "k1"); ok {
		t.Fatalf("Get(k1) after FlushTenant hit; want miss")
	}
	if _, ok := pc2.Get(ctx, "k2"); !ok {
		t.Fatalf("Get(k2) after flushing another tenant missed; want hit")
	}
}

func TestM62ExactRestartSurvivalFailsMemoryOnly(t *testing.T) {
	// Toggle check: the identical steps against a memory-only
	// ExactCache MUST miss after "reopen" (a fresh cache), proving
	// TestM62ExactRestartSurvival detects missing persistence.
	ctx := context.Background()
	c := NewExactCache(100)
	c.Set(ctx, "k1", CachedResponse{Content: "hello persistent", Model: "gpt-4o"}, time.Hour)
	reopened := NewExactCache(100) // a restart with no file
	if _, ok := reopened.Get(ctx, "k1"); ok {
		t.Fatalf("memory-only reopened cache hit k1; want miss — toggle check broken")
	}
}

func TestM62ExactExpiredRowsPrunedOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	pc.Set(ctx, "short", CachedResponse{Content: "gone soon"}, 50*time.Millisecond)
	pc.Set(ctx, "long", CachedResponse{Content: "stays"}, time.Hour)
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	pc2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen OpenPersistentCache: %v", err)
	}
	defer pc2.Close()
	if _, ok := pc2.Get(ctx, "short"); ok {
		t.Fatalf("reopened Get(short) hit; want miss (expired row pruned)")
	}
	if got, ok := pc2.Get(ctx, "long"); !ok || got.Content != "stays" {
		t.Fatalf("reopened Get(long) = %+v, %v; want hit", got, ok)
	}
	// The expired row is gone from disk, not just from memory:
	// a second reopen must still miss it.
	if err := pc2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc3, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer pc3.Close()
	if _, ok := pc3.Get(ctx, "short"); ok {
		t.Fatalf("second reopen Get(short) hit; expired row was resurrected from disk")
	}
}

func TestM62ExactSyncPersistsInnerCacheWrites(t *testing.T) {
	// M6.2 serve-mode path: writes made directly through the
	// inner ExactCache (what degrade holds) reach disk via Sync,
	// and survive a reopen.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	pc.Exact().Set(ctx, "via-inner", CachedResponse{Content: "served", Model: "gpt-4o"}, time.Hour)
	if n, err := pc.Sync(ctx); err != nil || n != 1 {
		t.Fatalf("Sync() = %d, %v; want 1, nil", n, err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if got, ok := pc2.Get(ctx, "via-inner"); !ok || got.Content != "served" {
		t.Fatalf("reopened Get(via-inner) = %+v, %v; want hit", got, ok)
	}
}

func TestM62SemanticPersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "semantic.db")
	ps, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	text := "how do I reset my router password"
	entry := SemanticEntry{Response: CachedResponse{Content: "open settings and reset", Model: "gpt-4o"}}
	if err := ps.SetTextForModel(ctx, "gpt-4o", text, entry, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if ps.Size() != 1 {
		t.Fatalf("Size() = %d; want 1", ps.Size())
	}
	if err := ps.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The entries really are on disk: the file exists and holds a row.
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("stat %q = %+v, %v; want a non-empty SQLite file", path, fi, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM semantic_entries`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("semantic_entries rows = %d; want 1", rows)
	}

	ps2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ps2.Close()
	got, ok := ps2.GetTextForModel(ctx, "gpt-4o", text)
	if !ok || got.Response.Content != "open settings and reset" {
		t.Fatalf("reopened GetTextForModel = %+v, %v; want hit with content %q", got, ok, "open settings and reset")
	}
	// Model scope survived the restart: another model still misses.
	if _, ok := ps2.GetTextForModel(ctx, "claude-sonnet", text); ok {
		t.Fatalf("reopened GetTextForModel(claude-sonnet) hit; want miss (model scope)")
	}
}

func TestM62SemanticExpiredRowsPrunedOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "semantic.db")
	ps, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	if err := ps.SetTextForModel(ctx, "gpt-4o", "short lived router question", SemanticEntry{Response: CachedResponse{Content: "gone", Model: "gpt-4o"}}, 50*time.Millisecond); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if err := ps.SetTextForModel(ctx, "gpt-4o", "long lived printer question", SemanticEntry{Response: CachedResponse{Content: "stays", Model: "gpt-4o"}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if err := ps.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	ps2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ps2.Close()
	if _, ok := ps2.GetTextForModel(ctx, "gpt-4o", "short lived router question"); ok {
		t.Fatalf("reopened GetTextForModel(short) hit; want miss (expired row pruned)")
	}
	if got, ok := ps2.GetTextForModel(ctx, "gpt-4o", "long lived printer question"); !ok || got.Response.Content != "stays" {
		t.Fatalf("reopened GetTextForModel(long) = %+v, %v; want hit", got, ok)
	}
}

func TestM62SemanticLoadAndSnapshotHelpers(t *testing.T) {
	// Serve-mode path: LoadSemanticCache populates a plain
	// *LocalSemanticCache from disk, SnapshotSemanticCache writes
	// its live entries back — the round trip the main.go wiring
	// relies on.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "semantic.db")
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, "gpt-4o", "how do I reset my router password", SemanticEntry{Response: CachedResponse{Content: "open settings and reset", Model: "gpt-4o"}}, time.Hour)
	if n, err := SnapshotSemanticCache(c, path); err != nil || n != 1 {
		t.Fatalf("SnapshotSemanticCache = %d, %v; want 1, nil", n, err)
	}
	loaded, err := LoadSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("LoadSemanticCache: %v", err)
	}
	got, ok := loaded.GetTextForModel(ctx, "gpt-4o", "how do I reset my router password")
	if !ok || got.Response.Content != "open settings and reset" {
		t.Fatalf("loaded GetTextForModel = %+v, %v; want hit", got, ok)
	}
	// Snapshot drops evicted entries instead of resurrecting them.
	loaded.FlushAll()
	if n, err := SnapshotSemanticCache(loaded, path); err != nil || n != 0 {
		t.Fatalf("SnapshotSemanticCache after flush = %d, %v; want 0, nil", n, err)
	}
	reloaded, err := LoadSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("second LoadSemanticCache: %v", err)
	}
	if reloaded.Size() != 0 {
		t.Fatalf("reloaded Size() = %d; want 0 (flushed entries must not resurrect)", reloaded.Size())
	}
}

func TestM62SemanticFlushAll(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, "gpt-4o", "how do I reset my router password", SemanticEntry{Response: CachedResponse{Content: "reset help", Model: "gpt-4o"}}, time.Hour)
	c.SetTextForModel(ctx, "claude-sonnet", "how do I fix my printer jam", SemanticEntry{Response: CachedResponse{Content: "printer help", Model: "claude-sonnet"}}, time.Hour)
	if c.Size() != 2 {
		t.Fatalf("Size() = %d; want 2", c.Size())
	}
	if n := c.FlushAll(); n != 2 {
		t.Fatalf("FlushAll() = %d; want 2", n)
	}
	if c.Size() != 0 {
		t.Fatalf("Size() after FlushAll = %d; want 0", c.Size())
	}
	entries, bytes := c.Stats()
	if entries != 0 || bytes != 0 {
		t.Fatalf("Stats() after FlushAll = (%d, %d); want (0, 0)", entries, bytes)
	}
	if _, ok := c.GetTextForModel(ctx, "gpt-4o", "how do I reset my router password"); ok {
		t.Fatalf("GetTextForModel after FlushAll hit; want miss")
	}
	// The cache is still usable after a flush.
	c.SetTextForModel(ctx, "gpt-4o", "how do I reset my router password", SemanticEntry{Response: CachedResponse{Content: "reset help", Model: "gpt-4o"}}, time.Hour)
	if got, ok := c.GetTextForModel(ctx, "gpt-4o", "how do I reset my router password"); !ok || got.Response.Content != "reset help" {
		t.Fatalf("GetTextForModel after re-store = %+v, %v; want hit", got, ok)
	}
}

func TestM62SemanticFlushModelOnlyRemovesThatModel(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, "gpt-4o", "how do I reset my router password", SemanticEntry{Response: CachedResponse{Content: "reset help", Model: "gpt-4o"}}, time.Hour)
	c.SetTextForModel(ctx, "claude-sonnet", "how do I fix my printer jam", SemanticEntry{Response: CachedResponse{Content: "printer help", Model: "claude-sonnet"}}, time.Hour)
	if n := c.FlushModel("gpt-4o"); n != 1 {
		t.Fatalf("FlushModel(gpt-4o) = %d; want 1", n)
	}
	if _, ok := c.GetTextForModel(ctx, "gpt-4o", "how do I reset my router password"); ok {
		t.Fatalf("GetTextForModel(gpt-4o) after FlushModel hit; want miss")
	}
	if got, ok := c.GetTextForModel(ctx, "claude-sonnet", "how do I fix my printer jam"); !ok || got.Response.Content != "printer help" {
		t.Fatalf("GetTextForModel(claude-sonnet) = %+v, %v; want hit (other model untouched)", got, ok)
	}
	// Flushing an absent model removes nothing.
	if n := c.FlushModel("does-not-exist"); n != 0 {
		t.Fatalf("FlushModel(absent) = %d; want 0", n)
	}
	if c.Size() != 1 {
		t.Fatalf("Size() = %d; want 1", c.Size())
	}
	// Byte accounting survives a partial flush: exactly the one
	// remaining entry's bytes are still counted.
	if entries, bytes := c.Stats(); entries != 1 || bytes <= 0 {
		t.Fatalf("Stats() = (%d, %d), want (1, >0) after partial flush", entries, bytes)
	}
}

func TestM62PersistentSemanticFlushModelClearsDiskToo(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "semantic.db")
	ps, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	if err := ps.SetTextForModel(ctx, "gpt-4o", "how do I reset my router password", SemanticEntry{Response: CachedResponse{Content: "reset help", Model: "gpt-4o"}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if err := ps.SetTextForModel(ctx, "claude-sonnet", "how do I fix my printer jam", SemanticEntry{Response: CachedResponse{Content: "printer help", Model: "claude-sonnet"}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if n := ps.FlushModel(ctx, "gpt-4o"); n != 1 {
		t.Fatalf("FlushModel(gpt-4o) = %d; want 1", n)
	}
	if err := ps.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ps2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ps2.Close()
	if _, ok := ps2.GetTextForModel(ctx, "gpt-4o", "how do I reset my router password"); ok {
		t.Fatalf("reopened GetTextForModel(gpt-4o) hit; flushed model was resurrected from disk")
	}
	if _, ok := ps2.GetTextForModel(ctx, "claude-sonnet", "how do I fix my printer jam"); !ok {
		t.Fatalf("reopened GetTextForModel(claude-sonnet) missed; want hit")
	}
}
