// Cache management API (M8.15 — DELETE /api/cache).
//
// OmniRoute parity, Area 6 "Flush / invalidation" and Area 14 §14b
// "Cache": OmniRoute has clearCache / invalidateByModel /
// DELETE /api/cache; we had only POST /admin/cache/flush (exact
// layer, admin scope) plus GET /api/cache/stats. This file adds
// the DELETE that clears every cache layer the server can reach
// and reports the real evicted count per layer — counts read from
// the stores, never estimated.
//
// LAYERS, and what "cleared" means for each:
//   - exact: flushed through s.cacheFlushStore() — the serving
//     engine when it implements cache.FlushStore (the degrade
//     wrapper, whose FlushAll drops the exact entries and the
//     store-time/tenant indexes), else the relay's exact cache.
//     With a *PersistentCache the flush deletes the disk rows
//     immediately, so a restart cannot resurrect them.
//   - semantic: flushed through the SemanticStore interface when
//     the wired store offers a FlushAll (either spelling:
//     FlushAll(ctx) int on *PersistentSemanticCache, FlushAll()
//     int on *LocalSemanticCache). The persistent adapter the
//     server holds in persistent mode (PersistentSemanticCache.
//     AsStore()) does NOT offer FlushAll, so in that wiring the
//     semantic layer reports evicted:0, flushed:false and a note
//     saying so — never a fake count. Closing that gap needs one
//     method on the adapter in internal/cache/semantic_persist.go,
//     outside this slice's owned files; see the M8.15 report.
//   - reasoning: flushed through the engine when it implements
//     reasoningCacheFlusher (FlushReasoningCache() int). No
//     shipped engine implements it yet: the M6.6 reasoning cache
//     (cache.ReasoningCache) has FlushAll, but the server holds
//     no handle to it — main.go wires it onto the degrade
//     wrapper only. Until the wrapper exposes a flush, the
//     reasoning layer reports evicted:0, flushed:false and a
//     note, and entries_before (from ReasoningCacheStats when the
//     engine offers it, else the router's reasoning stats) shows
//     the entries that are still there. The router's own
//     reasoning.Store (internal/reasoning) has no flush method
//     at all and is not cleared by this endpoint either.
//
// The response carries evicted, entries_before and flushed per
// layer, so "0 because the layer was empty" is distinguishable
// from "0 because the layer could not be flushed": a layer with
// entries_before > 0, evicted 0 and flushed false was NOT
// cleared, and the note says why.
//
// This endpoint deletes cache entries ONLY. It never touches the
// ledger, the config, the price book, the responses store, or any
// other state — the handler makes no call outside the three
// flush paths above (see TestM815CacheDeleteLeavesPricingIntact,
// which GETs /api/pricing before and after a DELETE).
//
// Route registration and auth live in guard.go + server.go New()
// (coordinator wiring, NOT this file — this slice owns new files
// only). Proposed classification, see the M8.15 report:
//
//	DELETE /api/cache  Tier 2 ALWAYS_PROTECTED, Scopes ["admin"]
//	                    (destructive — evicts cached work; same
//	                    class as POST /admin/cache/flush)
//	s.register(mux, "DELETE", "/api/cache", s.handleCacheDelete)
package server

import (
	"context"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/cache"
)

// cacheDeleteResponse is the DELETE /api/cache payload. Evicted,
// EntriesBefore and Flushed are keyed by layer ("exact",
// "semantic", "reasoning"). EntriesBefore omits a layer when no
// count source exists for it; Evicted and Flushed always carry
// all three layers. Notes carries one entry per layer that could
// not be flushed, explaining why.
type cacheDeleteResponse struct {
	Evicted       map[string]int    `json:"evicted"`
	EntriesBefore map[string]int    `json:"entries_before"`
	Flushed       map[string]bool   `json:"flushed"`
	TotalEvicted  int               `json:"total_evicted"`
	Notes         map[string]string `json:"notes,omitempty"`
}

// reasoningCacheFlusher is optionally implemented by engines
// that can flush the M6.6 reasoning replay cache
// (cache.ReasoningCache). The contract for the degrade wrapper:
// add FlushReasoningCache() int delegating to the wired
// reasoning cache's FlushAll (0 when none is wired). No shipped
// engine implements it yet — see the file header.
type reasoningCacheFlusher interface {
	FlushReasoningCache() int
}

// reasoningCacheStatsReporter is optionally implemented by
// engines that report the M6.6 reasoning cache's live
// accounting (degrade.StaleEngine implements it).
type reasoningCacheStatsReporter interface {
	ReasoningCacheStats() cache.ReasoningStats
}

// handleCacheDelete serves DELETE /api/cache (M8.15): clear the
// exact, semantic and reasoning cache layers and report the real
// evicted count per layer.
func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	resp := cacheDeleteResponse{
		Evicted:       map[string]int{"exact": 0, "semantic": 0, "reasoning": 0},
		EntriesBefore: map[string]int{},
		Flushed:       map[string]bool{"exact": false, "semantic": false, "reasoning": false},
		Notes:         map[string]string{},
	}

	// Exact layer: the serving engine's flush store, else the
	// relay's exact cache. Flushed is true only when a real
	// store was flushed; with no engine flush store and no
	// exact cache wired there is nothing to flush.
	_, engineFlushes := s.engine.(cache.FlushStore)
	exactWired := engineFlushes || s.exactCache != nil
	if s.exactCache != nil {
		entries, _ := s.exactCache.Stats()
		resp.EntriesBefore["exact"] = entries
	}
	if exactWired {
		resp.Evicted["exact"] = s.cacheFlushStore().FlushAll(ctx)
		resp.Flushed["exact"] = true
	} else {
		resp.Notes["exact"] = "exact cache not flushed: no exact cache is wired"
	}

	// Semantic layer: flush through whichever FlushAll form the
	// wired store offers. The persistent adapter offers neither
	// (see the file header): report that honestly.
	if s.semanticCache != nil {
		entries, _ := s.semanticCache.Stats()
		resp.EntriesBefore["semantic"] = entries
		if n, ok := flushSemanticStore(ctx, s.semanticCache); ok {
			resp.Evicted["semantic"] = n
			resp.Flushed["semantic"] = true
		} else {
			resp.Notes["semantic"] = "semantic cache not flushed: the wired semantic store does not expose FlushAll"
		}
	} else {
		resp.Notes["semantic"] = "semantic cache not flushed: no semantic cache is wired"
	}

	// Reasoning layer: flush through the engine when it offers
	// the flusher contract. No shipped engine does yet (see the
	// file header): report the live count and that it was not
	// cleared, never a fake evicted number.
	if st, ok := reasoningEntriesBefore(s.engine); ok {
		resp.EntriesBefore["reasoning"] = st
	}
	if fl, ok := s.engine.(reasoningCacheFlusher); ok && fl != nil {
		resp.Evicted["reasoning"] = fl.FlushReasoningCache()
		resp.Flushed["reasoning"] = true
	} else {
		resp.Notes["reasoning"] = "reasoning cache not flushed: the serving engine does not expose a reasoning flush"
	}

	resp.TotalEvicted = resp.Evicted["exact"] + resp.Evicted["semantic"] + resp.Evicted["reasoning"]
	if len(resp.Notes) == 0 {
		resp.Notes = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// flushSemanticStore flushes a semantic store through whichever
// FlushAll form it offers, returning the real evicted count.
// ok=false means the store offers no flush at all.
func flushSemanticStore(ctx context.Context, store cache.SemanticStore) (n int, ok bool) {
	switch c := store.(type) {
	case interface{ FlushAll(context.Context) int }:
		return c.FlushAll(ctx), true
	case interface{ FlushAll() int }:
		return c.FlushAll(), true
	default:
		return 0, false
	}
}

// reasoningEntriesBefore reads the live reasoning-cache entry
// count from the engine when it offers one: the M6.6 cache's
// stats first, else the router replay store's stats (the
// reasoningReporter the dashboard reasoning feed already uses).
// ok=false means no count source is wired.
func reasoningEntriesBefore(engine Engine) (entries int, ok bool) {
	if engine == nil {
		return 0, false
	}
	if rep, is := engine.(reasoningCacheStatsReporter); is && rep != nil {
		return rep.ReasoningCacheStats().Entries, true
	}
	if rep, is := engine.(reasoningReporter); is && rep != nil {
		return rep.ReasoningStats().Entries, true
	}
	return 0, false
}
