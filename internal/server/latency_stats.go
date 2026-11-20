// Model latency stats (G10 Area 12 row 9 — GET
// /api/telemetry/model-latency-stats).
//
// The handler reads per-(provider, model) p50/p95/p99 from the live
// observability Collector: every percentile comes from samples the
// request path actually recorded, never from an estimate. A key
// with no recorded samples never appears, so an empty list is the
// honest "no latency data yet" state. Optional ?provider= and
// ?model= query params filter by exact match.
//
// Route registration: guard row in guard.go (Tier 3 MANAGEMENT
// section, after the "/api/cache/stats" row) + s.register call in
// server.go New(), after the "/api/cache/stats" register line —
// following the GET /api/cache/stats pattern in this package (see
// internal/server/cache_stats.go:178-181).
package server

import (
	"net/http"
)

// modelLatencyStat is one (provider, model) row of the
// /api/telemetry/model-latency-stats payload. It mirrors
// observability.ModelLatencyStat so the handler can filter without
// re-typing the collector's rows.
type modelLatencyStat struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	Samples  uint64  `json:"samples"`
}

// modelLatencyStatsResponse is the GET
// /api/telemetry/model-latency-stats payload. Stats is never nil:
// with no recorded samples it is an empty list, not a null.
type modelLatencyStatsResponse struct {
	Stats []modelLatencyStat `json:"model_latency_stats"`
	Count int                `json:"count"`
}

// handleModelLatencyStats serves GET /api/telemetry/model-latency-stats:
// per-(provider, model) p50/p95/p99 from the observability
// Collector's recorded samples. A nil collector (telemetry off)
// answers 503, mirroring observability.Handler — telemetry off is
// never reported as empty data.
func (s *Server) handleModelLatencyStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.obsCollector == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not enabled")
		return
	}
	providerFilter := r.URL.Query().Get("provider")
	modelFilter := r.URL.Query().Get("model")
	stats := make([]modelLatencyStat, 0)
	for _, st := range s.obsCollector.LatencyStats() {
		if providerFilter != "" && st.Provider != providerFilter {
			continue
		}
		if modelFilter != "" && st.Model != modelFilter {
			continue
		}
		stats = append(stats, modelLatencyStat(st))
	}
	writeJSON(w, http.StatusOK, modelLatencyStatsResponse{Stats: stats, Count: len(stats)})
}
