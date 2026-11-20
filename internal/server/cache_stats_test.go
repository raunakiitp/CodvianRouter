package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M6.8: GET /api/cache/stats handler tests.
//
// The handler is invoked directly here (srv.handleCacheStats) —
// route registration (guard-table row + s.register call) landed at
// merge; the direct-invocation tests below still exercise the
// handler in isolation. Every number asserted below is read
// back from the same live sources the handler reads (the seeded
// caches, the recorder, the real PersistentCache), never from a
// hand-written expectation of the handler's own output.

// cacheStatsFakeEngine is a minimal Engine that also reports
// savings via a live *cache.PersistentCache, so the handler's
// savings block can be exercised exactly like production would
// once the coordinator wires it. It deliberately does NOT
// implement CoalescingStats: the two optional blocks are tested
// by separate fakes, because a fake that implements
// SavingsTotals() with a nil store would report a zero savings
// block — indistinguishable from a real store with zero recorded
// hits — and that is exactly the fake-data trap the handler
// must not fall into.
type cacheStatsFakeEngine struct {
	fakeEngine
	savings *cache.PersistentCache
}

func (e *cacheStatsFakeEngine) SavingsTotals() cache.SavingsTotals {
	return e.savings.SavingsTotals()
}

func (e *cacheStatsFakeEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return e.fakeEngine.Chat(ctx, req)
}

func (e *cacheStatsFakeEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.fakeEngine.ChatStream(ctx, req, onDelta)
}

// cacheStatsCoalescingEngine is a minimal Engine that reports
// single-flight counters only (no savings source).
type cacheStatsCoalescingEngine struct {
	fakeEngine
	coalescing coalescing.Stats
}

func (e *cacheStatsCoalescingEngine) CoalescingStats() coalescing.Stats { return e.coalescing }

func (e *cacheStatsCoalescingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return e.fakeEngine.Chat(ctx, req)
}

func (e *cacheStatsCoalescingEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.fakeEngine.ChatStream(ctx, req, onDelta)
}

// cacheStatsStack seeds live caches + a live recorder and returns
// the server plus a getter for the handler's JSON. Seeded values:
// 2 exact entries, 1 semantic entry, recorder 3 exact hits / 1
// exact miss / 2 semantic hits / 0 semantic misses.
func cacheStatsStack(t *testing.T, eng Engine) (*Server, *metrics.Recorder, func(t *testing.T) (int, cacheStatsResponse)) {
	t.Helper()
	ctx := context.Background()
	exact := cache.NewExactCache(500)
	exact.Set(ctx, "k1", cache.CachedResponse{Content: "one", Model: "m", Usage: cache.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, time.Hour)
	exact.Set(ctx, "k2", cache.CachedResponse{Content: "two", Model: "m", Usage: cache.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}}, time.Hour)
	semantic := cache.NewLocalSemanticCache()
	semantic.SetTextForModel(ctx, "m", "hello world this is a test", cache.SemanticEntry{Response: cache.CachedResponse{Content: "sem", Model: "m", Usage: cache.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}}, time.Hour)
	rec := metrics.NewRecorder()
	rec.IncCacheHit("exact")
	rec.IncCacheHit("exact")
	rec.IncCacheHit("exact")
	rec.IncCacheMiss("exact")
	rec.IncCacheHit("semantic")
	rec.IncCacheHit("semantic")
	srv := newTestServer(t, eng, WithCaches(exact, semantic), WithMetrics(rec))
	get := func(t *testing.T) (int, cacheStatsResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/cache/stats", nil)
		w := httptest.NewRecorder()
		srv.handleCacheStats(w, req)
		var out cacheStatsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode cache stats: %v (body %s)", err, w.Body.String())
		}
		return w.Code, out
	}
	return srv, rec, get
}

func TestCacheStatsMatchesLiveSources(t *testing.T) {
	eng := &cacheStatsCoalescingEngine{coalescing: coalescing.Stats{Requests: 5, Flights: 2, Shared: 3, UpstreamAvoided: 3, QuotaSaved: 3}}
	_, rec, get := cacheStatsStack(t, eng)
	code, out := get(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Exact layer: 2 seeded entries; hits/misses from the recorder.
	if out.Exact == nil {
		t.Fatal("exact layer absent, want live exact stats")
	}
	if out.Exact.Entries != 2 {
		t.Errorf("exact.entries = %d, want 2", out.Exact.Entries)
	}
	if out.Exact.Bytes <= 0 {
		t.Errorf("exact.bytes = %d, want > 0 (seeded entries account bytes)", out.Exact.Bytes)
	}
	if out.Exact.Hits != 3 || out.Exact.Misses != 1 {
		t.Errorf("exact hits/misses = %d/%d, want 3/1", out.Exact.Hits, out.Exact.Misses)
	}
	if out.Exact.HitRatio == nil || *out.Exact.HitRatio != 0.75 {
		t.Errorf("exact.hit_ratio = %v, want 0.75", out.Exact.HitRatio)
	}
	// Semantic layer: 1 seeded entry, 2 hits, 0 misses.
	if out.Semantic == nil {
		t.Fatal("semantic layer absent, want live semantic stats")
	}
	if out.Semantic.Entries != 1 {
		t.Errorf("semantic.entries = %d, want 1", out.Semantic.Entries)
	}
	if out.Semantic.Hits != 2 || out.Semantic.Misses != 0 {
		t.Errorf("semantic hits/misses = %d/%d, want 2/0", out.Semantic.Hits, out.Semantic.Misses)
	}
	if out.Semantic.HitRatio == nil || *out.Semantic.HitRatio != 1 {
		t.Errorf("semantic.hit_ratio = %v, want 1", out.Semantic.HitRatio)
	}
	// Totals are the live sum of the layers.
	if out.Totals == nil {
		t.Fatal("totals absent")
	}
	if out.Totals.Entries != 3 || out.Totals.Hits != 5 || out.Totals.Misses != 1 {
		t.Errorf("totals = %+v, want entries 3 hits 5 misses 1", out.Totals)
	}
	if out.Totals.Bytes != out.Exact.Bytes+out.Semantic.Bytes {
		t.Errorf("totals.bytes = %d, want exact+semantic = %d", out.Totals.Bytes, out.Exact.Bytes+out.Semantic.Bytes)
	}
	// Coalescing block comes from the engine when it reports it.
	if out.Coalescing == nil || out.Coalescing.Shared != 3 || out.Coalescing.Flights != 2 {
		t.Errorf("coalescing = %+v, want shared 3 flights 2", out.Coalescing)
	}
	// No savings source on this engine: savings omitted, note present.
	if out.Savings != nil {
		t.Errorf("savings = %+v, want omitted (no savings recorder wired)", out.Savings)
	}
	if out.SavingsNote == "" {
		t.Error("savings_note empty, want an honest unavailable note")
	}
	// Cross-check against the recorder snapshot directly.
	snap := rec.Snapshot()
	if snap.CacheHitsByLayer["exact"] != 3 || snap.CacheMissesByLayer["semantic"] != 0 {
		t.Errorf("recorder snapshot disagrees: %+v", snap)
	}
}

func TestCacheStatsReflectsFreshHit(t *testing.T) {
	// Toggle-verify: the handler re-reads live state on every GET,
	// so a hit recorded between two GETs must move the number. A
	// handler returning a startup snapshot would fail this test.
	_, rec, get := cacheStatsStack(t, &fakeEngine{})
	_, before := get(t)
	if before.Exact == nil {
		t.Fatal("exact layer absent")
	}
	rec.IncCacheHit("exact")
	_, after := get(t)
	if after.Exact.Hits != before.Exact.Hits+1 {
		t.Errorf("exact.hits after fresh hit = %d, want %d", after.Exact.Hits, before.Exact.Hits+1)
	}
	if after.Exact.HitRatio == nil || *after.Exact.HitRatio != 0.8 {
		t.Errorf("exact.hit_ratio after fresh hit = %v, want 0.8 (4 hits / 5 lookups)", after.Exact.HitRatio)
	}
}

func TestCacheStatsSavingsFromPersistentCache(t *testing.T) {
	ctx := context.Background()
	pc, err := cache.OpenPersistentCache(filepath.Join(t.TempDir(), "cache.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer pc.Close()
	resp := cache.CachedResponse{Content: "x", Model: "m", Usage: cache.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}
	if _, err := pc.RecordHit(ctx, "key1", "k1", resp, 0.001, 0.002); err != nil {
		t.Fatalf("RecordHit 1: %v", err)
	}
	if _, err := pc.RecordHit(ctx, "key1", "k2", resp, 0.001, 0.002); err != nil {
		t.Fatalf("RecordHit 2: %v", err)
	}
	want := pc.SavingsTotals() // hits 2, tokens 300, cost $0.40 (per hit: 100*0.001 + 50*0.002 = $0.20)
	_, _, get := cacheStatsStack(t, &cacheStatsFakeEngine{savings: pc})
	_, out := get(t)
	if out.Savings == nil {
		t.Fatal("savings absent, want the live PersistentCache totals")
	}
	// Assert literals, not just equality with the same live source
	// (a plumbing check alone would pass even if RecordHit priced
	// wrong — the values below are hand-computed from the RecordHit
	// calls above; cost compared with an epsilon, it is a float sum).
	if want.Hits != 2 || want.TokensSaved != 300 || math.Abs(want.CostSavedUSD-0.40) > 1e-9 {
		t.Fatalf("live SavingsTotals = %+v, want {hits 2, tokens 300, cost $0.40}", want)
	}
	if out.Savings.Hits != want.Hits || out.Savings.TokensSaved != want.TokensSaved || out.Savings.CostSavedUSD != want.CostSavedUSD {
		t.Errorf("savings = %+v, want %+v (live SavingsTotals)", out.Savings, want)
	}
	if out.SavingsNote != "" {
		t.Errorf("savings_note = %q, want empty when savings are present", out.SavingsNote)
	}
}

// TestCacheStatsSavingsGenuineZero (coordinator merge addition, M6.8
// review nit 2): a real PersistentCache with ZERO recorded hits must
// still report a savings block (all zeros, note empty) — "no source"
// and "zero savings" are different states and the handler must not
// confuse them.
func TestCacheStatsSavingsGenuineZero(t *testing.T) {
	pc, err := cache.OpenPersistentCache(filepath.Join(t.TempDir(), "cache.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer pc.Close()
	_, _, get := cacheStatsStack(t, &cacheStatsFakeEngine{savings: pc})
	_, out := get(t)
	if out.Savings == nil {
		t.Fatal("savings absent for a real (empty) PersistentCache, want a zero block")
	}
	if out.Savings.Hits != 0 || out.Savings.TokensSaved != 0 || out.Savings.CostSavedUSD != 0 {
		t.Errorf("savings = %+v, want all zeros for zero recorded hits", out.Savings)
	}
	if out.SavingsNote != "" {
		t.Errorf("savings_note = %q, want empty when a savings source exists", out.SavingsNote)
	}
}

func TestCacheStatsMemoryOnlyMode(t *testing.T) {
	// No persistence, no savings recorder on the engine: the
	// savings block must be honestly absent — never zero-filled.
	_, _, get := cacheStatsStack(t, &fakeEngine{})
	_, out := get(t)
	if out.Savings != nil {
		t.Errorf("savings = %+v, want omitted in memory-only mode", out.Savings)
	}
	if out.SavingsNote == "" {
		t.Error("savings_note empty, want the unavailable note in memory-only mode")
	}
	// Layers still report live: entries exist, hit_ratio present
	// once lookups happened, and totals sum the layers.
	if out.Exact == nil || out.Exact.Entries != 2 {
		t.Errorf("exact = %+v, want 2 live entries", out.Exact)
	}
	if out.Totals == nil || out.Totals.Entries != 3 {
		t.Errorf("totals = %+v, want 3 entries", out.Totals)
	}
}

func TestCacheStatsNoCachesNoRecorder(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/api/cache/stats", nil)
	w := httptest.NewRecorder()
	srv.handleCacheStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var out cacheStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Nothing wired: no layer, no totals, no fake zeros — only the
	// honest savings note.
	if out.Exact != nil || out.Semantic != nil || out.Totals != nil || out.Savings != nil {
		t.Errorf("response = %+v, want all blocks absent when nothing is wired", out)
	}
	if out.SavingsNote == "" {
		t.Error("savings_note empty, want the unavailable note")
	}
}

func TestCacheStatsMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodPost, "/api/cache/stats", nil)
	w := httptest.NewRecorder()
	srv.handleCacheStats(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestCacheStatsHitRatioOmittedWithNoLookups(t *testing.T) {
	// A recorder with no hits/misses: hit_ratio must be omitted,
	// not 0.0 — zero lookups is not "every lookup missed".
	srv := newTestServer(t, &fakeEngine{}, WithCaches(cache.NewExactCache(500), cache.NewLocalSemanticCache()), WithMetrics(metrics.NewRecorder()))
	req := httptest.NewRequest(http.MethodGet, "/api/cache/stats", nil)
	w := httptest.NewRecorder()
	srv.handleCacheStats(w, req)
	var out cacheStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Exact == nil || out.Exact.HitRatio != nil {
		t.Errorf("exact.hit_ratio = %v, want omitted with no lookups", out.Exact.HitRatio)
	}
	if out.Totals == nil || out.Totals.HitRatio != nil {
		t.Errorf("totals.hit_ratio = %v, want omitted with no lookups", out.Totals.HitRatio)
	}
}
