// Package server — B8 key management extended API.
//
// Implements:
//   - Single-key disable/enable   POST /admin/keys/{id}/disable   (B8-10)
//   - Single-key revoke           POST /admin/keys/{id}/revoke    (B8-10)
//   - Per-key daily/weekly/monthly budget limits  PATCH /admin/keys/{id}/budget (B8-11,12,13)
//   - Device tracking per key     GET /api/keys/{id}/devices      (B8-87)
//   - Exclusive lease badge       POST /admin/keys/{id}/lease     (B8-85)
//   - JSON-stream default badge   PATCH /admin/keys/{id}  (field json_stream_default) (B8-86)
//   - Management access tokens    GET/POST/DELETE /admin/management-tokens (B8-99)
//   - Key pools (fair-share)      GET/POST/PATCH/DELETE /admin/key-pools (B8-97)
//   - Budget warn threshold       PATCH /admin/keys/{id}/budget-warn (B8-100)
//   - Budget reset interval       PATCH /admin/keys/{id}/budget-reset (B8-101)
//   - Budget templates            GET/POST /admin/budget-templates (B8-102)
//   - Per-key routing page        GET /dashboard/keys/{id}/routing (B8-94)
//   - Per-key access page         GET /dashboard/keys/{id}/access  (B8-95)

package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Single-key disable / enable (B8-10)
// ---------------------------------------------------------------------------

// handleKeyDisable serves POST /admin/keys/{id}/disable — sets Disabled=true
// on a single key. The key is refused on the next request without restart.
func (s *Server) handleKeyDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/disable")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == id {
			s.cfg.GatewayKeys[i].Disabled = true
			if s.configPersist != nil {
				_ = s.configPersist()
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":       id,
				"disabled": true,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "key not found: "+id)
}

// handleKeyEnable serves POST /admin/keys/{id}/enable — clears Disabled flag.
func (s *Server) handleKeyEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/enable")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == id {
			s.cfg.GatewayKeys[i].Disabled = false
			s.persistConfig()
			writeJSON(w, http.StatusOK, map[string]any{
				"id":       id,
				"disabled": false,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "key not found: "+id)
}

// handleKeyRevoke serves POST /admin/keys/{id}/revoke — permanently deletes
// the key (alias for DELETE /admin/keys/{id} as a named action).
func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/revoke")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	for i, k := range s.cfg.GatewayKeys {
		if k.ID == id {
			s.cfg.GatewayKeys = append(s.cfg.GatewayKeys[:i], s.cfg.GatewayKeys[i+1:]...)
			s.persistConfig()
			writeJSON(w, http.StatusOK, map[string]any{
				"id":     id,
				"status": "revoked",
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "key not found: "+id)
}

// ---------------------------------------------------------------------------
// Per-key budget window limits (B8-11, B8-12, B8-13): daily/weekly/monthly
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Budget warn threshold (B8-100)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Budget reset interval (B8-101)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Budget templates (B8-102)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Device tracking per key (B8-87)
// ---------------------------------------------------------------------------

// handleKeyDevices serves GET /api/keys/{id}/devices — returns device tracking
// info for a single key (distinct IPs/UAs seen in the last 30 min).
func (s *Server) handleKeyDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	id = strings.TrimSuffix(id, "/devices")
	id = strings.TrimSpace(id)
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	devices := s.keyDevicesFor(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"key_id":  id,
		"devices": devices,
		"count":   len(devices),
	})
}

// ---------------------------------------------------------------------------
// Exclusive lease badge (B8-85)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Management access tokens (B8-99)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Key pools / fair-share quotas (B8-97)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Per-key routing page (B8-94): GET /dashboard/keys/{id}/routing
// ---------------------------------------------------------------------------

// handleKeyRoutingPage serves GET /dashboard/keys/{id}/routing —
// returns the effective routing strategy and overrides for a key.
func (s *Server) handleKeyRoutingPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/dashboard/keys/")
	id = strings.TrimSuffix(id, "/routing")
	globalStrategy := ""
	if s.cfg != nil && s.cfg.Routing != nil {
		globalStrategy = s.cfg.Routing.GlobalStrategy
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key_id":          id,
		"global_strategy": globalStrategy,
		"overrides":       map[string]any{},
		"note":            "per-key strategy overrides not yet configured",
	})
}

// ---------------------------------------------------------------------------
// Per-key access page (B8-95): GET /dashboard/keys/{id}/access
// ---------------------------------------------------------------------------

// handleKeyAccessPage serves GET /dashboard/keys/{id}/access —
// returns the access policy (models, endpoints, connections, combos) for a key.
func (s *Server) handleKeyAccessPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/dashboard/keys/")
	id = strings.TrimSuffix(id, "/access")
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	for _, k := range s.cfg.GatewayKeys {
		if k.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{
				"key_id":            id,
				"model_access_mode": k.ModelAccessMode,
				"allowed_models":    k.AllowedModels,
				"blocked_models":    k.BlockedModels,
				"allowed_endpoints": k.AllowedEndpoints,
				"ip_allowlist":      k.IPAllowlist,
				"ip_blocklist":      k.IPBlocklist,
				"key_groups":        k.KeyGroups,
				"forced_model":      k.ForcedModel,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "key not found: "+id)
}

// ---------------------------------------------------------------------------
// IP filter settings (B8-131)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// keyIDFromPath extracts a key ID from a URL path that has the form
// prefix + id + suffix (e.g. "/admin/keys/{id}/disable").
func keyIDFromPath(path, prefix, suffix string) string {
	s := strings.TrimPrefix(path, prefix)
	s = strings.TrimSuffix(s, suffix)
	return strings.TrimSpace(s)
}

// newShortID generates a short random hex ID.
func newShortID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// generateSecureToken returns a hex-encoded random token of n bytes.
func generateSecureToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// persistConfig calls s.configPersist if set (no-op otherwise).
func (s *Server) persistConfig() {
	if s.configPersist != nil {
		_ = s.configPersist()
	}
}

// registerB8KeyRoutes registers all B8 key management endpoints.
func (s *Server) registerB8KeyRoutes(mux *http.ServeMux) {
	// Single-key disable/enable/revoke (B8-10)
	s.register(mux, "POST", "/admin/keys/{id}/disable", s.handleKeyDisable)
	s.register(mux, "POST", "/admin/keys/{id}/enable", s.handleKeyEnable)
	s.register(mux, "POST", "/admin/keys/{id}/revoke", s.handleKeyRevoke)

	// Device tracking (B8-87)
	s.register(mux, "GET", "/api/keys/{id}/devices", s.handleKeyDevices)

	// Per-key dashboard pages (B8-94, B8-95)
	s.register(mux, "GET", "/dashboard/keys/{id}/routing", s.handleKeyRoutingPage)
	s.register(mux, "GET", "/dashboard/keys/{id}/access", s.handleKeyAccessPage)

	// Budget windows/warn/reset/templates, leases, management
	// tokens, key pools and the global IP filter answer
	// feature_unavailable: see unavailable_routes.go.
}
