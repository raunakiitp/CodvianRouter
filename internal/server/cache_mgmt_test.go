package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/cache"
)

// M8.15: DELETE /api/cache handler tests.
//
// The handler is invoked directly here (srv.handleCacheDelete)
// — route registration (guard-table row + s.register call)
// landed at merge. Counts asserted below are read back from the
// same live caches the handler flushes, never from a
// hand-written expectation of the handler's own output.

// m815SeedCaches builds an exact cache with 2 entries and a
// semantic cache with 1 entry, and returns both stores.
func m815SeedCaches(t *testing.T) (*cache.ExactCache, *cache.LocalSemanticCache) {
	t.Helper()
	ctx := context.Background()
	exact := cache.NewExactCache(500)
	exact.Set(ctx, "k1", cache.CachedResponse{Content: "one", Model: "mock-model", Usage: cache.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, time.Hour)
	exact.Set(ctx, "k2", cache.CachedResponse{Content: "two", Model: "mock-model", Usage: cache.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}}, time.Hour)
	semantic := cache.NewLocalSemanticCache()
	semantic.SetTextForModel(ctx, "mock-model", "hello world this is a test",
		cache.SemanticEntry{Response: cache.CachedResponse{Content: "sem", Model: "mock-model", Usage: cache.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}}, time.Hour)
	if n, _ := exact.Stats(); n != 2 {
		t.Fatalf("seeded exact entries = %d (want 2)", n)
	}
	if n, _ := semantic.Stats(); n != 1 {
		t.Fatalf("seeded semantic entries = %d (want 1)", n)
	}
	return exact, semantic
}

// m815CallCacheDelete invokes the cache-delete handler directly.
func m815CallCacheDelete(t *testing.T, srv *Server, method string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/cache", nil)
	rec := httptest.NewRecorder()
	srv.handleCacheDelete(rec, req)
	return rec
}

func m815DecodeCacheDelete(t *testing.T, rec *httptest.ResponseRecorder) cacheDeleteResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (want 200)", rec.Code, rec.Body.String())
	}
	var resp cacheDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return resp
}

func TestM815CacheDeleteFixture(t *testing.T) {
	exact, semantic := m815SeedCaches(t)
	srv := newTestServer(t, &fakeEngine{}, WithCaches(exact, semantic))
	f := apitest.LoadFixture(t, "m8.15", "cache-delete-200")
	apitest.Run(t, http.HandlerFunc(srv.handleCacheDelete), f)
	// The stores are really empty afterwards — the fixture's
	// counts were real, not echoed.
	if n, _ := exact.Stats(); n != 0 {
		t.Errorf("exact entries after DELETE = %d (want 0)", n)
	}
	if n, _ := semantic.Stats(); n != 0 {
		t.Errorf("semantic entries after DELETE = %d (want 0)", n)
	}
}

func TestM815CacheDeleteTwice(t *testing.T) {
	exact, semantic := m815SeedCaches(t)
	srv := newTestServer(t, &fakeEngine{}, WithCaches(exact, semantic))
	first := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if first.TotalEvicted != 3 || first.Evicted["exact"] != 2 || first.Evicted["semantic"] != 1 {
		t.Fatalf("first DELETE = %+v (want exact 2, semantic 1, total 3)", first)
	}
	if !first.Flushed["exact"] || !first.Flushed["semantic"] {
		t.Fatalf("first DELETE flushed = %v (want exact+semantic true)", first.Flushed)
	}
	second := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if second.TotalEvicted != 0 {
		t.Fatalf("second DELETE total_evicted = %d (want 0: empty layers evict nothing)", second.TotalEvicted)
	}
	if second.EntriesBefore["exact"] != 0 || second.EntriesBefore["semantic"] != 0 {
		t.Errorf("second DELETE entries_before = %v (want exact+semantic 0)", second.EntriesBefore)
	}
}

func TestM815CacheDeleteMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := m815CallCacheDelete(t, srv, http.MethodGet)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d (want 405)", rec.Code)
	}
}

// m815ReasoningFlushEngine is a fake Engine that offers the
// reasoningCacheFlusher contract: it reports 3 reasoning entries
// flushed. It proves the handler clears the reasoning layer when
// the engine offers the flush — no shipped engine does yet.
type m815ReasoningFlushEngine struct {
	fakeEngine
	flushCalls int
}

func (e *m815ReasoningFlushEngine) FlushReasoningCache() int {
	e.flushCalls++
	return 3
}

// m815FlushStoreEngine is a fake Engine implementing
// cache.FlushStore (the production degrade-wrapper path):
// DELETE must route the exact flush through it, not through
// the exactCacheFlushStore adapter. It reports 7 evicted and
// touches no store, so the seeded exact cache must still hold
// its 2 entries afterwards — proving the engine path was taken.
type m815FlushStoreEngine struct {
	fakeEngine
	flushAllCalls int
}

func (e *m815FlushStoreEngine) FlushAll(ctx context.Context) int {
	e.flushAllCalls++
	return 7
}

func (e *m815FlushStoreEngine) FlushTenant(ctx context.Context, tenant string) int {
	return 0
}

func TestM815CacheDeleteExactFlushStoreEngine(t *testing.T) {
	exact, semantic := m815SeedCaches(t)
	eng := &m815FlushStoreEngine{}
	srv := newTestServer(t, eng, WithCaches(exact, semantic))
	resp := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if resp.Evicted["exact"] != 7 || !resp.Flushed["exact"] {
		t.Fatalf("exact evicted=%d flushed=%v (want 7, true: engine FlushStore path)", resp.Evicted["exact"], resp.Flushed["exact"])
	}
	if eng.flushAllCalls != 1 {
		t.Errorf("engine FlushAll calls = %d (want 1)", eng.flushAllCalls)
	}
	if _, noted := resp.Notes["exact"]; noted {
		t.Errorf("exact note present despite a successful engine flush: %v", resp.Notes)
	}
	// The engine fake flushed nothing itself, so the seeded
	// exact cache is untouched — the 7 came from the engine,
	// not the adapter (which would have reported 2).
	if n, _ := exact.Stats(); n != 2 {
		t.Errorf("exact entries after DELETE = %d (want 2: engine path, adapter not used)", n)
	}
	if resp.Evicted["semantic"] != 1 || resp.TotalEvicted != 8 {
		t.Errorf("semantic=%d total=%d (want 1, 7+1+0=8)", resp.Evicted["semantic"], resp.TotalEvicted)
	}
}

// m815NoFlushSemanticStore is a SemanticStore offering no
// FlushAll (the persistent-adapter wiring): DELETE must report
// the semantic layer honestly, without leaking internal repo
// paths or type names into the operator-facing note.
type m815NoFlushSemanticStore struct{}

func (m815NoFlushSemanticStore) GetTextForModel(ctx context.Context, model, text string) (cache.SemanticEntry, bool) {
	return cache.SemanticEntry{}, false
}

func (m815NoFlushSemanticStore) SetTextForModel(ctx context.Context, model, text string, entry cache.SemanticEntry, ttl time.Duration) {
}

func (m815NoFlushSemanticStore) Stats() (entries, bytes int) { return 1, 0 }

func TestM815CacheDeleteNotesAreGeneric(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithCaches(nil, m815NoFlushSemanticStore{}))
	resp := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if resp.Flushed["semantic"] || resp.Evicted["semantic"] != 0 {
		t.Fatalf("semantic flushed=%v evicted=%d (want false, 0)", resp.Flushed["semantic"], resp.Evicted["semantic"])
	}
	if resp.EntriesBefore["semantic"] != 1 {
		t.Errorf("semantic entries_before = %d (want 1)", resp.EntriesBefore["semantic"])
	}
	for layer, note := range resp.Notes {
		if strings.Contains(note, "internal/") || strings.Contains(note, ".go") || strings.Contains(note, "cache.") {
			t.Errorf("%s note leaks internals: %q", layer, note)
		}
	}
	if !strings.Contains(resp.Notes["semantic"], "does not expose FlushAll") {
		t.Errorf("semantic note = %q (want the generic 'does not expose FlushAll' wording)", resp.Notes["semantic"])
	}
	if !strings.Contains(resp.Notes["reasoning"], "does not expose a reasoning flush") {
		t.Errorf("reasoning note = %q (want the generic 'does not expose a reasoning flush' wording)", resp.Notes["reasoning"])
	}
}

func TestM815CacheDeleteReasoningFlusher(t *testing.T) {
	exact, semantic := m815SeedCaches(t)
	eng := &m815ReasoningFlushEngine{}
	srv := newTestServer(t, eng, WithCaches(exact, semantic))
	resp := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if resp.Evicted["reasoning"] != 3 || !resp.Flushed["reasoning"] {
		t.Fatalf("reasoning evicted=%d flushed=%v (want 3, true)", resp.Evicted["reasoning"], resp.Flushed["reasoning"])
	}
	if _, noted := resp.Notes["reasoning"]; noted {
		t.Errorf("reasoning note present despite a successful flush: %v", resp.Notes)
	}
	if eng.flushCalls != 1 {
		t.Errorf("FlushReasoningCache calls = %d (want 1)", eng.flushCalls)
	}
	if resp.TotalEvicted != 6 {
		t.Errorf("total_evicted = %d (want 2+1+3=6)", resp.TotalEvicted)
	}
}

func TestM815CacheDeleteReasoningUnwired(t *testing.T) {
	// The plain fake engine offers no reasoning flush: the
	// response must say so (flushed:false + note) instead of
	// claiming a reasoning eviction.
	srv := newTestServer(t, &fakeEngine{})
	resp := m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	if resp.Evicted["reasoning"] != 0 || resp.Flushed["reasoning"] {
		t.Fatalf("reasoning evicted=%d flushed=%v (want 0, false)", resp.Evicted["reasoning"], resp.Flushed["reasoning"])
	}
	if !strings.Contains(resp.Notes["reasoning"], "does not expose") {
		t.Errorf("reasoning note = %q (must explain the engine exposes no reasoning flush)", resp.Notes["reasoning"])
	}
}

func TestM815CacheDeleteLeavesPricingIntact(t *testing.T) {
	// DELETE /api/cache must not delete the ledger or any other
	// state: the price table is identical before and after.
	exact, semantic := m815SeedCaches(t)
	srv, _, _ := m815PricingServer(t, nil, WithCaches(exact, semantic))
	before := m815GetPricing(t, srv, "/api/pricing")
	m815DecodeCacheDelete(t, m815CallCacheDelete(t, srv, http.MethodDelete))
	after := m815GetPricing(t, srv, "/api/pricing")
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("pricing changed across DELETE /api/cache:\nbefore %s\nafter  %s", beforeJSON, afterJSON)
	}
	// And the ledger still charges the same rate.
	if got := srv.getLedgerPrices().Cost("mock-upstream", "mock-model", 1_000_000, 0); got != 2.5 {
		t.Errorf("ledger cost after DELETE = %v (want 2.5)", got)
	}
}
