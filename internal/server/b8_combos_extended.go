package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// B8 combo extensions that are backed by real router state:
// GET /api/settings/scoring-modes (B8-77) and GET
// /api/combos/{id}/studio-cascade (B8-83). The capability, tuning
// and account-fallback settings routes are listed in
// unavailable_routes.go: they stored values nothing ever read.

// weights16Map names each factor of a scoring.Weights16 by the
// field name the scorer uses.
func weights16Map(w scoring.Weights16) map[string]float64 {
	return map[string]float64{
		"quota": w.Quota, "health": w.Health, "cost_inv": w.CostInv, "latency_inv": w.LatencyInv,
		"task_fit": w.TaskFit, "stability": w.Stability, "tier_priority": w.TierPriority,
		"tier_affinity": w.TierAffinity, "specificity_match": w.SpecificityMatch,
		"context_affinity": w.ContextAffinity, "cache_affinity": w.CacheAffinity,
		"session_affinity": w.SessionAffinity, "reset_window_affinity": w.ResetWindowAffinity,
		"connection_density": w.ConnectionDensity, "quality": w.Quality, "reliability": w.Reliability,
	}
}

// handleScoringModes serves GET /api/settings/scoring-modes: the
// 16 factors with the default weights the scorer actually uses
// (scoring.DefaultWeights16) and every mode pack from
// scoring.ModePacks. The previous version served a hand-written
// table whose factors and weights did not match the scorer.
func (s *Server) handleScoringModes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	def := weights16Map(scoring.DefaultWeights16)
	names := make([]string, 0, len(def))
	for n := range def {
		names = append(names, n)
	}
	sort.Strings(names)
	factors := make([]map[string]any, 0, len(names))
	for _, n := range names {
		factors = append(factors, map[string]any{"name": n, "default_weight": def[n]})
	}
	packIDs := make([]string, 0, len(scoring.ModePacks))
	for id := range scoring.ModePacks {
		packIDs = append(packIDs, id)
	}
	sort.Strings(packIDs)
	packs := make([]map[string]any, 0, len(packIDs))
	for _, id := range packIDs {
		packs = append(packs, map[string]any{"id": id, "weights": weights16Map(scoring.ModePacks[id])})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"factors":    factors,
		"mode_packs": packs,
		"source":     "internal/scoring (DefaultWeights16, ModePacks)",
	})
}

// handleComboStudioCascade serves GET /api/combos/{id}/studio-cascade:
// the combo's configured chain in order. It reports configuration
// only; live health is not read here, so no status or health field
// is claimed (the previous version marked every step active and
// healthy). Unknown combo ids answer 404.
func (s *Server) handleComboStudioCascade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/combos/")
	id = strings.TrimSuffix(id, "/studio-cascade")
	if id == "" {
		writeError(w, http.StatusBadRequest, "combo id required")
		return
	}
	steps := []map[string]any{}
	store := s.combosStore()
	store.mu.RLock()
	rec, ok := store.records[id]
	if ok {
		for i, st := range rec.chain {
			steps = append(steps, map[string]any{
				"step":     i + 1,
				"provider": st.provider,
				"model":    st.model,
				"effort":   st.effort,
			})
		}
	}
	store.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "combo not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combo_id": id,
		"steps":    steps,
		"count":    len(steps),
	})
}

// registerB8CombosRoutes registers the real B8 combo routes.
func (s *Server) registerB8CombosRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/settings/scoring-modes", s.handleScoringModes)
	s.register(mux, "GET", "/api/combos/{id}/studio-cascade", s.handleComboStudioCascade)
}
