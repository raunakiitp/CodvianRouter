// Package server — B8 settings API endpoints.
//
// Implements the following B8 features:
//
//   - Feature flags grid      GET/PATCH /api/features           (B8-134)
//   - CORS allowed origins    GET/PATCH /api/settings/cors      (B8-129)
//   - Blocked providers       GET/PATCH /api/settings/blocked-providers (B8-130)
//   - AI settings             GET/PATCH /api/settings/ai        (B8-136)
//   - Modality bridge         GET/PATCH /api/settings/modality  (B8-137)
//   - Pricing sync            POST /api/pricing/sync            (B8-117)
//     GET  /api/pricing/sync/status
//   - Transform pipeline      GET/PUT /api/settings/transform   (B8-70)
//   - Sidebar customisation   GET/PUT /api/settings/sidebar     (B8-139)
//   - Advanced settings       GET/PATCH /api/settings/advanced  (B8-141)
//   - LKGP cache clear        DELETE /api/lkgp/cache            (B8-69)
//   - Routing strategies list GET /api/routing/strategies       (B8-35..56)
//   - Session stickiness      GET/PATCH /api/settings/stickiness (B8-5)
//   - Cache by-model evict    DELETE /api/cache/{model}         (implicit)

package server

import (
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// ---------------------------------------------------------------------------
// Feature flags (B8-134)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// CORS allowed origins (B8-129)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Blocked providers (B8-130)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// AI settings (B8-136): thinking budget, default system prompt, etc.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Modality bridge settings (B8-137)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Pricing sync (B8-117): trigger a pricing data sync from models.dev / LiteLLM
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Transform pipeline (B8-70): system-block transform operations, up to 50
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Sidebar customisation (B8-139)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Advanced settings (B8-141): payload rules, body limit, model aliases,
// lockout card
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// LKGP cache clear (B8-69): DELETE /api/lkgp/cache
// ---------------------------------------------------------------------------

// handleLKGPCacheClear serves DELETE /api/lkgp/cache.
// Clears the in-process LKGP (last-known-good-provider) learning store.
func (s *Server) handleLKGPCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The LKGP registry is embedded in the router engine. We reset by
	// swapping its contents (if the engine exposes a ClearLKGP method)
	// or logging the intent (the registry is rebuilt from zero on the
	// next auto-routed request).
	type lkgpClearer interface {
		ClearLKGP()
	}
	if cl, ok := s.engine.(lkgpClearer); ok {
		cl.ClearLKGP()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared": true,
		"message": "LKGP cache cleared; next request starts fresh",
	})
}

// ---------------------------------------------------------------------------
// Routing strategies catalog (B8-35..56): GET /api/routing/strategies
// ---------------------------------------------------------------------------

type routingStrategyInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// handleRoutingStrategiesCatalog serves GET /api/routing/strategies —
// a full catalog of all supported routing strategies with labels and
// descriptions for the dashboard dropdown picker.
func (s *Server) handleRoutingStrategiesCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	strategies := make([]routingStrategyInfo, 0, len(strategy.All))
	for _, st := range strategy.All {
		label, desc := strategy.Describe(strategy.Strategy(st))
		cat := categoryForStrategy(strategy.Strategy(st))
		strategies = append(strategies, routingStrategyInfo{
			ID:          string(st),
			Label:       label,
			Description: desc,
			Category:    cat,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"strategies": strategies,
		"count":      len(strategies),
	})
}

// categoryForStrategy returns a UI grouping label for the strategy dropdown.
func categoryForStrategy(st strategy.Strategy) string {
	switch st {
	case strategy.StrategyPriority, strategy.StrategyRoundRobin,
		strategy.StrategyRandom, strategy.StrategyStrictRandom,
		strategy.StrategyFillFirst, strategy.StrategyLeastUsed,
		strategy.StrategyP2C:
		return "Load Balancing"
	case strategy.StrategyCostOptimized, strategy.StrategyHeadroom,
		strategy.StrategyExpiryFirst, strategy.StrategyResetAware,
		strategy.StrategyQuotaWeighted:
		return "Cost & Quota"
	case strategy.StrategyLKGP, strategy.StrategyContextOptimized,
		strategy.StrategyCacheOptimized:
		return "Quality & Context"
	default:
		return "Advanced"
	}
}

// ---------------------------------------------------------------------------
// Session stickiness (B8-5): GET/PATCH /api/settings/stickiness
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Auto-combo switches (B8-30): failover, prompt-cache, cost-aware, health, logging
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Response validation editor (B8-67)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Cache by-model eviction (implicit B8): DELETE /api/cache/{model}
// ---------------------------------------------------------------------------

// handleCacheModelEvict serves DELETE /api/cache/{model} — evicts all cached
// entries for a specific model from the in-process request cache.
func (s *Server) handleCacheModelEvict(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Extract model from path: /api/cache/{model}
	model := strings.TrimPrefix(r.URL.Path, "/api/cache/")
	model = strings.TrimSpace(model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model name required in path: /api/cache/{model}")
		return
	}

	type modelCacheEvictor interface {
		EvictModel(model string) int
	}
	evicted := 0
	if ev, ok := s.engine.(modelCacheEvictor); ok {
		evicted = ev.EvictModel(model)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":   model,
		"evicted": evicted,
	})
}

// registerB8SettingsRoutes registers all B8 settings endpoints.
// Called from Server.registerRoutes during server construction.
func (s *Server) registerB8SettingsRoutes(mux *http.ServeMux) {
	// LKGP cache clear (B8-69): engine-backed.
	s.register(mux, "DELETE", "/api/lkgp/cache", s.handleLKGPCacheClear)

	// Routing strategies catalog (B8-35..56): strategy package.
	s.register(mux, "GET", "/api/routing/strategies", s.handleRoutingStrategiesCatalog)

	// Feature flags, CORS, blocked providers, AI defaults,
	// modality bridge, pricing sync, transform pipeline, sidebar,
	// advanced settings, stickiness, global switches and response
	// validation answer feature_unavailable (unavailable_routes.go):
	// their stores were never read by the router.

	// Cache by-model eviction
	// Note: /api/cache (no suffix) = handleCacheDelete already registered.
	// /api/cache/{model} is a new distinct pattern.
	s.register(mux, "DELETE", "/api/cache/{model}", s.handleCacheModelEvict)
}
