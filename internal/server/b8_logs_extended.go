// Package server — B8 extended logs API.
//
// Implements:
//   - Proxy logs tab         GET /api/logs/proxy           (B8-22)
//   - Console output view    GET /api/logs/console         (B8-23)
//   - Activity feed          GET /api/logs/activity        (B8-25)
//   - Conversations view     GET /api/logs/conversations   (B8-26)
//   - Compression log        GET /api/logs/compression     (B8-27)
//   - Audit log (B8 ring)    GET /api/logs/audit           (B8-28)
//     (distinct from GET /dashboard/audit which uses audit.go)
//   - Log export (JSON)      GET /api/logs/export?format=json (B8-126)
//   - Log export destinations GET/POST /api/logs/destinations (B8-127)
//   - Visual request timeline GET /api/logs/timeline/{id}  (B8-24)

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory management-action audit ring buffer (B8-28)
// — This is distinct from the dashboard/audit endpoint in audit.go which reads
//   route-guard denials and MCP events. This ring stores management actions
//   (key created/disabled/revoked, settings changed, etc.) written by the
//   B8 handlers in this file and b8_key_extended.go.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// In-memory console output ring buffer (B8-23)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Proxy logs (B8-22): upstream/downstream proxy request log
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Activity feed (B8-25)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Conversations view (B8-26)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Compression log (B8-27)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Visual request timeline (B8-24)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Log export (JSON format) (B8-126)
// ---------------------------------------------------------------------------

// handleLogExportJSON serves GET /api/logs/export — exports request logs as JSON.
func (s *Server) handleLogExportJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	format := r.URL.Query().Get("format")
	if format != "json" && format != "" {
		writeError(w, http.StatusBadRequest, "this endpoint only serves format=json")
		return
	}
	limit := parseIntQuery(r, "limit", 1000, 50000)
	rows := []map[string]any{}
	if s.ledger != nil {
		ctx := r.Context()
		if events, err := s.ledger.RecentEvents(ctx, limit, ""); err == nil {
			for _, e := range events {
				rows = append(rows, map[string]any{
					"id":                e.ID,
					"timestamp":         e.TS,
					"model":             e.Model,
					"provider":          e.Provider,
					"prompt_tokens":     e.PromptTokens,
					"completion_tokens": e.CompletionTokens,
					"cost_usd":          e.CostUSD,
					"duration_ms":       e.LatencyMS,
					"ok":                e.OK,
				})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="codvian-logs.json"`)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"exported_at": time.Now(),
		"count":       len(rows),
		"entries":     rows,
	})
}

// ---------------------------------------------------------------------------
// Log export destinations (B8-127)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseIntQuery returns a query parameter as an int clamped to [1, max].
func parseIntQuery(r *http.Request, key string, defaultVal, max int) int {
	if s := r.URL.Query().Get(key); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 && n <= max {
			return n
		}
	}
	return defaultVal
}

// registerB8LogsRoutes registers all B8 log endpoints.
func (s *Server) registerB8LogsRoutes(mux *http.ServeMux) {
	// Log export JSON (B8-126): ledger-backed.
	s.register(mux, "GET", "/api/logs/export", s.handleLogExportJSON)

	// The proxy/console/timeline/activity/conversations/
	// compression/audit feeds and log destinations answer
	// feature_unavailable (unavailable_routes.go): nothing ever
	// wrote to their in-memory stores.
}
