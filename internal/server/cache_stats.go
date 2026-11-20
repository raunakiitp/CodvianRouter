// Cache analytics (M6.8 — GET /api/cache/stats).
//
// OmniRoute parity, Area 6 "Cache analytics" (PARTIAL): OmniRoute has
// a cache_metrics table, getCacheStats(), GET /api/cache/stats and
// /dashboard/cache. This file is the backend stats endpoint only —
// the dashboard page is M10's job and is NOT built here.
//
// Every number in the response is read live at request time from a
// real source; a source that does not exist is ABSENT from the
// response, never zero-filled:
//
//   - exact / semantic entries + bytes: ExactCache.Stats() /
//     LocalSemanticCache.Stats() on the live caches the server was
//     wired with (WithCaches — the same instances the engine wraps,
//     so a key stored by either dialect is counted here).
//   - exact / semantic hits + misses: metrics.Recorder.Snapshot()
//     CacheHitsByLayer / CacheMissesByLayer (the recorder the
//     degrade wrapper reports into via SetCacheMetrics, and the
//     relay reports into directly on a cache hit). A read API
//     DOES exist — Snapshot() — so no new counters were invented.
//     Hit/miss counts are since process start, like /metrics.
//   - savings {hits, tokens_saved, cost_saved_usd}:
//     PersistentCache.SavingsTotals(), read through the serving
//     engine only when it implements cacheSavingsReporter. Since
//     M6.2b the production wiring does implement it when
//     persistence is on: main.go calls SetSavingsRecorder /
//     SetSavingsPrices and wraps the engine with degrade.WithSavings
//     (which exposes SavingsTotals only when a real PersistentCache
//     is wired — a bare StaleEngine deliberately does NOT implement
//     the reporter, so a memory-only deployment emits no zero
//     savings block). When no savings source is wired the savings
//     block is omitted and savings_note says so — never a
//     zero-filled fake. A memory-only deployment is the same case:
//     no savings store exists.
//   - coalescing: the engine's CoalescingStats() when it implements
//     the reporter interface (same structural pattern as
//     handleCoalescing). In production StaleEngine always
//     implements it and returns zero Stats when the wrapped engine
//     has none, so the block is always present there, possibly
//     all-zero — an honest empty state, not a fake.
//
// Route registration and auth live in guard.go (TierManagement,
// AuthExempt: true — counts only, same class as /api/providers
// and /dashboard/coalescing) with the s.register call in
// server.go New(); both landed at merge (see the note at the
// bottom of this file).
package server

import (
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
)

// cacheLayerStats is one cache layer's live counters. HitRatio is
// nil (omitted) until the layer has seen at least one lookup — a
// 0.0 with zero lookups would falsely claim every lookup missed.
type cacheLayerStats struct {
	Entries  int      `json:"entries"`
	Bytes    int      `json:"bytes"`
	Hits     uint64   `json:"hits"`
	Misses   uint64   `json:"misses"`
	HitRatio *float64 `json:"hit_ratio,omitempty"`
}

// cacheSavingsStats is the savings block: the aggregated savings of
// every recorded own-cache hit, from PersistentCache.SavingsTotals().
// Present only when a savings source is wired; see the file header.
type cacheSavingsStats struct {
	Hits         int     `json:"hits"`
	TokensSaved  int     `json:"tokens_saved"`
	CostSavedUSD float64 `json:"cost_saved_usd"`
}

// cacheStatsResponse is the GET /api/cache/stats payload. Layers,
// totals, savings and coalescing are pointers with omitempty: a
// source that is not wired is absent, never a zero-filled fake.
type cacheStatsResponse struct {
	Exact       *cacheLayerStats   `json:"exact,omitempty"`
	Semantic    *cacheLayerStats   `json:"semantic,omitempty"`
	Totals      *cacheLayerStats   `json:"totals,omitempty"`
	Savings     *cacheSavingsStats `json:"savings,omitempty"`
	SavingsNote string             `json:"savings_note,omitempty"`
	Coalescing  *coalescing.Stats  `json:"coalescing,omitempty"`
}

// cacheSavingsReporter is optionally implemented by engines that can
// report aggregated cache savings (PersistentCache satisfies it;
// a bare StaleEngine does not — only the WithSavings decorator
// exposes it, and only when a real PersistentCache is wired; see
// the file header). Structural so this package does not import
// internal/degrade.
type cacheSavingsReporter interface {
	SavingsTotals() cache.SavingsTotals
}

func cacheHitRatio(hits, misses uint64) *float64 {
	total := hits + misses
	if total == 0 {
		return nil
	}
	r := float64(hits) / float64(total)
	return &r
}

func (s *Server) cacheLayerStats(entries, bytes int, hits, misses uint64) *cacheLayerStats {
	return &cacheLayerStats{
		Entries: entries, Bytes: bytes,
		Hits: hits, Misses: misses,
		HitRatio: cacheHitRatio(hits, misses),
	}
}

// handleCacheStats serves GET /api/cache/stats (M6.8): the live
// per-layer cache counters, totals, coalescing counters and — when a
// savings source is wired — aggregated savings. Read-only counts
// only: no keys, no message content, no per-request data.
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var snapHits, snapMisses map[string]uint64
	if s.recorder != nil {
		snap := s.recorder.Snapshot()
		snapHits, snapMisses = snap.CacheHitsByLayer, snap.CacheMissesByLayer
	}
	out := cacheStatsResponse{}
	if s.exactCache != nil {
		entries, bytes := s.exactCache.Stats()
		out.Exact = s.cacheLayerStats(entries, bytes, snapHits["exact"], snapMisses["exact"])
	}
	if s.semanticCache != nil {
		entries, bytes := s.semanticCache.Stats()
		out.Semantic = s.cacheLayerStats(entries, bytes, snapHits["semantic"], snapMisses["semantic"])
	}
	if out.Exact != nil || out.Semantic != nil {
		tot := &cacheLayerStats{}
		for _, layer := range []*cacheLayerStats{out.Exact, out.Semantic} {
			if layer == nil {
				continue
			}
			tot.Entries += layer.Entries
			tot.Bytes += layer.Bytes
			tot.Hits += layer.Hits
			tot.Misses += layer.Misses
		}
		tot.HitRatio = cacheHitRatio(tot.Hits, tot.Misses)
		out.Totals = tot
	}
	if sr, ok := s.engine.(cacheSavingsReporter); ok && sr != nil {
		tot := sr.SavingsTotals()
		out.Savings = &cacheSavingsStats{
			Hits: tot.Hits, TokensSaved: tot.TokensSaved, CostSavedUSD: tot.CostSavedUSD,
		}
	} else {
		// No savings source is wired (memory-only deployment, or
		// persistence on without a savings recorder). Say so; do
		// not emit a zero savings block that would read as
		// "nothing saved".
		out.SavingsNote = "savings unavailable: no savings recorder is wired to the serving engine (persistence disabled or savings recording not configured)"
	}
	if cr, ok := s.engine.(coalescingReporter); ok && cr != nil {
		stats := cr.CoalescingStats()
		out.Coalescing = &stats
	}
	writeJSON(w, http.StatusOK, out)
}

// Coordinator wiring (landed at merge — guard.go row after the
// "/api/usage/om-usage" row, s.register call in server.go New()
// after the "/api/usage/om-usage" register line; kept here so a
// future reader can see the registration without hunting):
//
// guard.go, Tier 3 MANAGEMENT section, after the
// "/api/usage/om-usage" row:
//   {Method: "GET", Path: "/api/cache/stats", Tier: TierManagement, Description: "Live cache analytics — per-layer entries/bytes/hits/misses/hit-ratio for the exact + semantic caches, totals, single-flight coalescing counters, and aggregated savings (tokens + USD) when a savings recorder is wired; savings omitted with a note when not. Counts only, no keys, no message content.", AuthExempt: true},
//
// server.go, in New() after the "/api/usage/om-usage" register line:
//   s.register(mux, "GET", "/api/cache/stats", s.handleCacheStats)
