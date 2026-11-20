// M6.2b — shutdown sync + savings prices (cmd wiring).
//
// Toggle-verification: these tests FAIL against the pre-M6.2b
// code (restored from backup) — savingsPricesFor and
// syncCachesOnShutdown do not exist there, so the file does not
// compile; and the pruning assertions fail if the shutdown sync
// is a no-op (evicted rows linger on disk until a Sync rewrites
// the table from live memory).
package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/config"

	_ "modernc.org/sqlite"
)

// TestM62bSavingsPricesFor proves the savings price lookup is
// the model-level price converted to per-token USD, and $0
// whenever the price is unknown — never an invented price.
func TestM62bSavingsPricesFor(t *testing.T) {
	book := config.NewPriceBook()
	book.Entries["gpt-4o"] = config.ModelPrice{
		InputUSDPer1M:  fptr(2.50),
		OutputUSDPer1M: fptr(10.00),
	}
	book.Entries["free-model"] = config.ModelPrice{} // priced, but no rates at all
	fn := savingsPricesFor(book)
	pp, cp := fn("gpt-4o")
	if pp != 2.50/1e6 || cp != 10.00/1e6 {
		t.Fatalf("savingsPricesFor(gpt-4o) = (%v, %v), want (%v, %v)", pp, cp, 2.50/1e6, 10.00/1e6)
	}
	if pp, cp := fn("free-model"); pp != 0 || cp != 0 {
		t.Fatalf("savingsPricesFor(free-model) = (%v, %v), want (0, 0) (no rates configured)", pp, cp)
	}
	if pp, cp := fn("unknown-model"); pp != 0 || cp != 0 {
		t.Fatalf("savingsPricesFor(unknown-model) = (%v, %v), want (0, 0) (unknown price is $0, never invented)", pp, cp)
	}
	if pp, cp := savingsPricesFor(nil)("gpt-4o"); pp != 0 || cp != 0 {
		t.Fatalf("savingsPricesFor(nil) = (%v, %v), want (0, 0)", pp, cp)
	}
}

// TestM62bStackSyncCachesOnShutdown proves the graceful-shutdown
// sync: entries stored during the run are on disk already
// (write-through), and — the part only a Sync does — rows for
// entries evicted from memory are pruned, so a restart never
// reloads them. Without the sync the files still hold the
// evicted rows (8 exact / 3 semantic); with it they hold only
// the live entries (5 / 2).
func TestM62bStackSyncCachesOnShutdown(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 5)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	psc, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{MaxEntries: 2})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	for i := 0; i < 8; i++ {
		pc.Set(ctx, string(rune('a'+i)), cache.CachedResponse{Content: "v", Model: "m"}, time.Hour)
	}
	texts := []string{
		"alpha bravo charlie delta echo foxtrot",
		"golf hotel india juliet kilo lima mike",
		"november oscar papa quebec romeo sierra",
	}
	for _, text := range texts {
		if err := psc.SetTextForModel(ctx, "m", text, cache.SemanticEntry{Response: cache.CachedResponse{Content: "v", Model: "m"}}, time.Hour); err != nil {
			t.Fatalf("SetTextForModel: %v", err)
		}
	}
	// Precondition: the evicted rows are still on disk — a Set
	// writes its row immediately, eviction removes only memory.
	if n := m62bMainRowCount(t, filepath.Join(dir, "exact.db"), "cache_entries"); n != 8 {
		t.Fatalf("exact disk rows before shutdown sync = %d, want 8 (evicted rows linger)", n)
	}
	if n := m62bMainRowCount(t, filepath.Join(dir, "sem.db"), "semantic_entries"); n != 3 {
		t.Fatalf("semantic disk rows before shutdown sync = %d, want 3 (evicted row lingers)", n)
	}

	st := &stack{persistExact: pc, persistSemantic: psc}
	st.syncCachesOnShutdown(ctx)

	if n := m62bMainRowCount(t, filepath.Join(dir, "exact.db"), "cache_entries"); n != 5 {
		t.Fatalf("exact disk rows after shutdown sync = %d, want 5 (evicted rows pruned)", n)
	}
	if n := m62bMainRowCount(t, filepath.Join(dir, "sem.db"), "semantic_entries"); n != 2 {
		t.Fatalf("semantic disk rows after shutdown sync = %d, want 2 (evicted row pruned)", n)
	}
	// A restart loads only the live entries.
	pc2, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 5)
	if err != nil {
		t.Fatalf("reopen exact: %v", err)
	}
	defer pc2.Close()
	if pc2.Size() != 5 {
		t.Fatalf("reopened exact Size = %d, want 5", pc2.Size())
	}
	psc2, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{MaxEntries: 2})
	if err != nil {
		t.Fatalf("reopen semantic: %v", err)
	}
	defer psc2.Close()
	if psc2.Size() != 2 {
		t.Fatalf("reopened semantic Size = %d, want 2", psc2.Size())
	}
	if _, ok := psc2.GetTextForModel(ctx, "m", texts[0]); ok {
		t.Fatal("reopened GetTextForModel(texts[0]) hit; evicted entry resurrected")
	}
}

// TestM62bStackSyncCachesOnShutdownNilStack proves the shutdown
// sync is a no-op (not a panic) for a stack with no persistent
// caches — the memory-only serve mode.
func TestM62bStackSyncCachesOnShutdownNilStack(t *testing.T) {
	(&stack{}).syncCachesOnShutdown(context.Background())
	(*stack)(nil).syncCachesOnShutdown(context.Background())
}

// m62bMainRowCount counts rows in table in the SQLite file at path.
func m62bMainRowCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("COUNT(*) FROM %s: %v", table, err)
	}
	return n
}
