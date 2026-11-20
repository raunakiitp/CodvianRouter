// M6.2b — persistence write-through follow-up: flush durability,
// the Sync-vs-flush race, evicted-row pruning, and the
// PersistentSemanticCache Sync.
//
// Toggle-verification: every test below FAILS against the
// pre-M6.2b code (restored from backup): Delete / Sync (semantic)
// / AsStore do not exist there, so the files do not compile;
// the race tests fail intermittently because the old Sync read
// its snapshot outside p.mu and the old flushes released p.mu
// between the memory flush and the disk delete.
package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// m62bRowCount returns how many rows table holds in the SQLite
// file at path, read straight from disk (not through any cache),
// so the test asserts on durable state, not memory.
func m62bRowCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("COUNT(*) FROM %s: %v", table, err)
	}
	return n
}

func m62bResp(content string) CachedResponse {
	return CachedResponse{Content: content, Model: "m", Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
}

// TestM62bExactWriteThroughNoSync proves an exact Set reaches
// disk without any Sync: close the cache, reopen, and the entry
// is there. (Regression guard: PersistentCache.Set was already
// write-through; the M6.2b change is that serve mode can now
// pass the wrapper itself — covered in degrade/server tests.)
func TestM62bExactWriteThroughNoSync(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	pc.Set(ctx, "k1", m62bResp("hello"), time.Hour)
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if got, ok := pc2.Get(ctx, "k1"); !ok || got.Content != "hello" {
		t.Fatalf("reopened Get(k1) = %+v, %v; want hit with content %q", got, ok, "hello")
	}
}

// TestM62bExactDeleteDurable proves Delete removes the disk row
// at once: a restart cannot reload a deleted entry.
func TestM62bExactDeleteDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	pc.Set(ctx, "k1", m62bResp("one"), time.Hour)
	pc.Set(ctx, "k2", m62bResp("two"), time.Hour)
	pc.Delete(ctx, "k1")
	if _, ok := pc.Get(ctx, "k1"); ok {
		t.Fatal("Get(k1) after Delete hit; want miss")
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if _, ok := pc2.Get(ctx, "k1"); ok {
		t.Fatal("reopened Get(k1) hit after Delete; the disk row survived the delete")
	}
	if got, ok := pc2.Get(ctx, "k2"); !ok || got.Content != "two" {
		t.Fatalf("reopened Get(k2) = %+v, %v; want hit (only k1 was deleted)", got, ok)
	}
}

// TestM62bExactFlushDurableAcrossRestart proves an admin flush is
// durable immediately: FlushAll / FlushTenant delete the disk
// rows at once, and a Sync running after the flush (the 1-minute
// ticker) does not resurrect them.
func TestM62bExactFlushDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	if err := pc.SetForTenant(ctx, "k1", "tenant-a", m62bResp("a"), time.Hour); err != nil {
		t.Fatalf("SetForTenant: %v", err)
	}
	if err := pc.SetForTenant(ctx, "k2", "tenant-b", m62bResp("b"), time.Hour); err != nil {
		t.Fatalf("SetForTenant: %v", err)
	}
	if n := pc.FlushTenant(ctx, "tenant-a"); n != 1 {
		t.Fatalf("FlushTenant(tenant-a) = %d, want 1", n)
	}
	// The ticker fires after the flush: it must not resurrect k1.
	if _, err := pc.Sync(ctx); err != nil {
		t.Fatalf("Sync after flush: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if _, ok := pc2.Get(ctx, "k1"); ok {
		t.Fatal("reopened Get(k1) hit after FlushTenant + Sync; the flushed entry resurrected")
	}
	if got, ok := pc2.Get(ctx, "k2"); !ok || got.Content != "b" {
		t.Fatalf("reopened Get(k2) = %+v, %v; want hit (other tenant untouched)", got, ok)
	}

	// FlushAll, then a Sync, then a restart: nothing comes back.
	if n := pc2.FlushAll(ctx); n != 1 {
		t.Fatalf("FlushAll = %d, want 1", n)
	}
	if _, err := pc2.Sync(ctx); err != nil {
		t.Fatalf("Sync after FlushAll: %v", err)
	}
	if err := pc2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc3, err := OpenPersistentCache(path, 500)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer pc3.Close()
	if pc3.Size() != 0 {
		t.Fatalf("reopened Size = %d, want 0 after FlushAll + Sync", pc3.Size())
	}
}

// TestM62bExactSyncFlushRace hammers Sync and FlushAll
// concurrently (with -race) and proves a flushed entry can never
// be resurrected by a snapshot taken before the flush. Seeds live
// in memory only (Exact().Set, no disk write), so every Sync
// snapshot is a full 500-row rewrite — the widest race window.
// Each round ends with a flush, so when the storm stops memory
// is deterministically empty and the disk must be too: a Sync
// that committed before a flush had its rows deleted by that
// flush, and a Sync that ran after the last flush snapshotted
// empty memory. Any row left on disk is a resurrection.
func TestM62bExactSyncFlushRace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 1000)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	seed := func() {
		for i := 0; i < 150; i++ {
			pc.Exact().Set(ctx, fmt.Sprintf("k%03d", i), CachedResponse{Content: strings.Repeat("v", 200), Model: "m"}, time.Hour)
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = pc.Sync(ctx)
			}
		}
	}()
	for i := 0; i < 30; i++ {
		seed()
		pc.FlushAll(ctx)
	}
	close(done)
	wg.Wait()
	if pc.Size() != 0 {
		t.Fatalf("Size after storm = %d, want 0 (last op was a flush)", pc.Size())
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := m62bRowCount(t, path, "cache_entries"); n != 0 {
		t.Fatalf("disk cache_entries rows = %d, want 0 (a flush lost the race to a snapshot)", n)
	}
	pc2, err := OpenPersistentCache(path, 1000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if pc2.Size() != 0 {
		t.Fatalf("reopened Size = %d, want 0 (flushed entries resurrected)", pc2.Size())
	}
}

// TestM62bExactSyncPrunesEvicted proves the other half of the
// Sync contract: a Set writes its row to disk immediately, but
// eviction removes only the memory slot, so evicted rows linger
// on disk until a Sync rewrites the table from live memory.
// Before the Sync the file holds 8 rows (the 3 evicted entries
// are still there); after it, 5 — and a restart loads only the
// 5 live entries, never the evicted ones.
func TestM62bExactSyncPrunesEvicted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exact.db")
	pc, err := OpenPersistentCache(path, 5)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	for i := 0; i < 8; i++ {
		pc.Set(ctx, fmt.Sprintf("k%d", i), m62bResp(fmt.Sprintf("v%d", i)), time.Hour)
	}
	if pc.Size() != 5 {
		t.Fatalf("Size = %d, want 5 (maxEntries)", pc.Size())
	}
	if n := m62bRowCount(t, path, "cache_entries"); n != 8 {
		t.Fatalf("disk rows before Sync = %d, want 8 (evicted rows linger until Sync)", n)
	}
	if n, err := pc.Sync(ctx); err != nil || n != 5 {
		t.Fatalf("Sync = %d, %v; want 5, nil", n, err)
	}
	if n := m62bRowCount(t, path, "cache_entries"); n != 5 {
		t.Fatalf("disk rows after Sync = %d, want 5 (evicted rows pruned)", n)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := OpenPersistentCache(path, 5)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if pc2.Size() != 5 {
		t.Fatalf("reopened Size = %d, want 5", pc2.Size())
	}
	for i := 0; i < 3; i++ {
		if _, ok := pc2.Get(ctx, fmt.Sprintf("k%d", i)); ok {
			t.Fatalf("reopened Get(k%d) hit; evicted entry resurrected", i)
		}
	}
	for i := 3; i < 8; i++ {
		if _, ok := pc2.Get(ctx, fmt.Sprintf("k%d", i)); !ok {
			t.Fatalf("reopened Get(k%d) miss; live entry lost", i)
		}
	}
}

// TestM62bSemanticWriteThroughNoSync proves a semantic Set
// reaches disk without any Sync: close, reopen, and the entry
// is there. (Regression guard: storeThrough was already
// write-through; the M6.2b change is that serve mode can now
// pass the wrapper itself — covered in degrade/server tests.)
func TestM62bSemanticWriteThroughNoSync(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sem.db")
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	if err := psc.SetTextForModel(ctx, "m", "what is the capital of france", SemanticEntry{Response: m62bResp("paris")}, time.Hour); err != nil {
		t.Fatalf("SetTextForModel: %v", err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	if got, ok := psc2.GetTextForModel(ctx, "m", "what is the capital of france"); !ok || got.Response.Content != "paris" {
		t.Fatalf("reopened GetTextForModel = %+v, %v; want hit with content %q", got, ok, "paris")
	}
}

// TestM62bSemanticFlushDurable proves FlushAll / FlushModel
// delete the disk rows at once: a restart cannot reload a
// flushed semantic entry, and other models are untouched.
func TestM62bSemanticFlushDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sem.db")
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	texts := map[string]string{
		"m-a": "alpha bravo charlie delta echo",
		"m-b": "foxtrot golf hotel india juliet",
	}
	for model, text := range texts {
		if err := psc.SetTextForModel(ctx, model, text, SemanticEntry{Response: m62bResp(model)}, time.Hour); err != nil {
			t.Fatalf("SetTextForModel(%s): %v", model, err)
		}
	}
	if n := psc.FlushModel(ctx, "m-a"); n != 1 {
		t.Fatalf("FlushModel(m-a) = %d, want 1", n)
	}
	// A Sync after the flush (the 1-minute ticker) must not resurrect it.
	if _, err := psc.Sync(ctx); err != nil {
		t.Fatalf("Sync after FlushModel: %v", err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	if _, ok := psc2.GetTextForModel(ctx, "m-a", texts["m-a"]); ok {
		t.Fatal("reopened GetTextForModel(m-a) hit after FlushModel + Sync; the flushed entry resurrected")
	}
	if got, ok := psc2.GetTextForModel(ctx, "m-b", texts["m-b"]); !ok || got.Response.Content != "m-b" {
		t.Fatalf("reopened GetTextForModel(m-b) = %+v, %v; want hit (other model untouched)", got, ok)
	}

	if n := psc2.FlushAll(ctx); n != 1 {
		t.Fatalf("FlushAll = %d, want 1", n)
	}
	if _, err := psc2.Sync(ctx); err != nil {
		t.Fatalf("Sync after FlushAll: %v", err)
	}
	if err := psc2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc3, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer psc3.Close()
	if psc3.Size() != 0 {
		t.Fatalf("reopened Size = %d, want 0 after FlushAll + Sync", psc3.Size())
	}
}

// TestM62bSemanticSyncPrunesEvicted proves Sync drops disk rows
// for entries evicted from memory: with MaxEntries 2, storing 3
// texts evicts the first from memory but its row stays on disk
// (3 rows) until a Sync rewrites the table from live slots (2
// rows) — and a restart then loads only the 2 live entries.
func TestM62bSemanticSyncPrunesEvicted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sem.db")
	cfg := LocalSemanticCacheConfig{MaxEntries: 2}
	psc, err := OpenPersistentSemanticCache(path, cfg)
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	texts := []string{
		"alpha bravo charlie delta echo foxtrot",
		"golf hotel india juliet kilo lima mike",
		"november oscar papa quebec romeo sierra",
	}
	for i, text := range texts {
		if err := psc.SetTextForModel(ctx, "m", text, SemanticEntry{Response: m62bResp(fmt.Sprintf("v%d", i))}, time.Hour); err != nil {
			t.Fatalf("SetTextForModel(%d): %v", i, err)
		}
	}
	if psc.Size() != 2 {
		t.Fatalf("Size = %d, want 2 (MaxEntries)", psc.Size())
	}
	if n := m62bRowCount(t, path, "semantic_entries"); n != 3 {
		t.Fatalf("disk rows before Sync = %d, want 3 (evicted row lingers until Sync)", n)
	}
	if n, err := psc.Sync(ctx); err != nil || n != 2 {
		t.Fatalf("Sync = %d, %v; want 2, nil", n, err)
	}
	if n := m62bRowCount(t, path, "semantic_entries"); n != 2 {
		t.Fatalf("disk rows after Sync = %d, want 2 (evicted row pruned)", n)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	if psc2.Size() != 2 {
		t.Fatalf("reopened Size = %d, want 2", psc2.Size())
	}
	if _, ok := psc2.GetTextForModel(ctx, "m", texts[0]); ok {
		t.Fatal("reopened GetTextForModel(texts[0]) hit; evicted entry resurrected")
	}
	for i := 1; i < 3; i++ {
		if got, ok := psc2.GetTextForModel(ctx, "m", texts[i]); !ok || got.Response.Content != fmt.Sprintf("v%d", i) {
			t.Fatalf("reopened GetTextForModel(texts[%d]) = %+v, %v; want hit", i, got, ok)
		}
	}
}

// TestM62bSemanticSyncFlushRace is the semantic twin of
// TestM62bExactSyncFlushRace: Sync and FlushAll run concurrently
// (with -race) and the disk table must end empty — a store or
// snapshot in flight when a flush lands cannot re-insert a row
// the flush just deleted. Each round ends with a flush, so when
// the storm stops memory is deterministically empty and the
// disk must be too.
func TestM62bSemanticSyncFlushRace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sem.db")
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{MaxEntries: 500})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	seed := func() {
		for i := 0; i < 100; i++ {
			text := fmt.Sprintf("seed entry number %d with distinct words %d", i, i*7)
			psc.Inner().SetTextForModel(ctx, "m", text, SemanticEntry{Response: m62bResp("seed")}, time.Hour)
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = psc.Sync(ctx)
			}
		}
	}()
	for i := 0; i < 15; i++ {
		seed()
		psc.FlushAll(ctx)
	}
	close(done)
	wg.Wait()
	if psc.Size() != 0 {
		t.Fatalf("Size after storm = %d, want 0 (last op was a flush)", psc.Size())
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := m62bRowCount(t, path, "semantic_entries"); n != 0 {
		t.Fatalf("disk semantic_entries rows = %d, want 0 (a flush lost the race to a store/snapshot)", n)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{MaxEntries: 500})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	if psc2.Size() != 0 {
		t.Fatalf("reopened Size = %d, want 0 (flushed entries resurrected)", psc2.Size())
	}
}

// TestM62bSemanticStoreAdapter proves the AsStore adapter
// satisfies SemanticStore and writes through: a Set through the
// adapter is on disk after a restart with no Sync.
func TestM62bSemanticStoreAdapter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sem.db")
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	var store SemanticStore = psc.AsStore()
	store.SetTextForModel(ctx, "m", "adapter write through check", SemanticEntry{Response: m62bResp("via-adapter")}, time.Hour)
	if entries, _ := store.Stats(); entries != 1 {
		t.Fatalf("adapter Stats entries = %d, want 1", entries)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	if got, ok := psc2.GetTextForModel(ctx, "m", "adapter write through check"); !ok || got.Response.Content != "via-adapter" {
		t.Fatalf("reopened GetTextForModel = %+v, %v; want hit with content %q", got, ok, "via-adapter")
	}
}
