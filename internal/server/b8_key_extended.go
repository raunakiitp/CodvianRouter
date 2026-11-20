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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
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

type keyBudgetWindowRequest struct {
	DailyUSD   *float64 `json:"daily_usd,omitempty"`
	WeeklyUSD  *float64 `json:"weekly_usd,omitempty"`
	MonthlyUSD *float64 `json:"monthly_usd,omitempty"`
}

// handleKeyBudgetWindow serves PATCH /admin/keys/{id}/budget — sets the
// daily, weekly and/or monthly spend limits on a single key.
// These are stored as TokenLimit entries with the appropriate ResetInterval.
func (s *Server) handleKeyBudgetWindow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/budget")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	var req keyBudgetWindowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid budget request: "+err.Error())
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID != id {
			continue
		}
		k := &s.cfg.GatewayKeys[i]
		// Merge the window limits into the in-memory budget store.
		setWindowLimit(k, id, "daily", req.DailyUSD)
		setWindowLimit(k, id, "weekly", req.WeeklyUSD)
		setWindowLimit(k, id, "monthly", req.MonthlyUSD)
		s.persistConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          id,
			"daily_usd":   getWindowLimit(id, "daily"),
			"weekly_usd":  getWindowLimit(id, "weekly"),
			"monthly_usd": getWindowLimit(id, "monthly"),
		})
		return
	}
	writeError(w, http.StatusNotFound, "key not found: "+id)
}

// keyWindowBudgets holds in-memory per-key USD window budgets.
// TokenLimit.Limit is token-count only; USD window budgets live here.
type keyWindowBudgets struct {
	mu      sync.RWMutex
	budgets map[string]map[string]float64 // keyID → interval → USD
}

var globalKeyWindowBudgets = &keyWindowBudgets{budgets: map[string]map[string]float64{}}

// setWindowLimit stores a USD window budget for a key in the in-memory store.
// intervalName is "daily", "weekly" or "monthly". If limit is nil or ≤0, the
// entry is removed (no limit on that window).
func setWindowLimit(_ *config.GatewayKey, keyID, intervalName string, limit *float64) {
	globalKeyWindowBudgets.mu.Lock()
	defer globalKeyWindowBudgets.mu.Unlock()
	if globalKeyWindowBudgets.budgets[keyID] == nil {
		globalKeyWindowBudgets.budgets[keyID] = map[string]float64{}
	}
	if limit == nil || *limit <= 0 {
		delete(globalKeyWindowBudgets.budgets[keyID], intervalName)
		return
	}
	globalKeyWindowBudgets.budgets[keyID][intervalName] = *limit
}

// getWindowLimit retrieves the stored USD window budget for a key (0 = no limit).
func getWindowLimit(keyID, intervalName string) float64 {
	globalKeyWindowBudgets.mu.RLock()
	defer globalKeyWindowBudgets.mu.RUnlock()
	if m, ok := globalKeyWindowBudgets.budgets[keyID]; ok {
		return m[intervalName]
	}
	return 0
}

// ---------------------------------------------------------------------------
// Budget warn threshold (B8-100)
// ---------------------------------------------------------------------------

type budgetWarnStore struct {
	mu         sync.RWMutex
	thresholds map[string]float64 // keyID → warn %
}

var globalBudgetWarn = &budgetWarnStore{
	thresholds: map[string]float64{},
}

// handleKeyBudgetWarn serves PATCH /admin/keys/{id}/budget-warn.
func (s *Server) handleKeyBudgetWarn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/budget-warn")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	var body struct {
		WarnAtPct float64 `json:"warn_at_pct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid budget-warn request: "+err.Error())
		return
	}
	if body.WarnAtPct < 0 || body.WarnAtPct > 100 {
		writeError(w, http.StatusBadRequest, "warn_at_pct must be 0–100")
		return
	}
	globalBudgetWarn.mu.Lock()
	globalBudgetWarn.thresholds[id] = body.WarnAtPct
	globalBudgetWarn.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          id,
		"warn_at_pct": body.WarnAtPct,
	})
}

// ---------------------------------------------------------------------------
// Budget reset interval (B8-101)
// ---------------------------------------------------------------------------

type budgetResetStore struct {
	mu     sync.RWMutex
	resets map[string]budgetResetConfig
}

type budgetResetConfig struct {
	Interval     string     `json:"interval"`
	ResetTimeUTC string     `json:"reset_time_utc"`
	NextReset    *time.Time `json:"next_reset,omitempty"`
}

var globalBudgetReset = &budgetResetStore{
	resets: map[string]budgetResetConfig{},
}

// handleKeyBudgetReset serves PATCH /admin/keys/{id}/budget-reset.
func (s *Server) handleKeyBudgetReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/budget-reset")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	var cfg budgetResetConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid budget-reset request: "+err.Error())
		return
	}
	globalBudgetReset.mu.Lock()
	globalBudgetReset.resets[id] = cfg
	globalBudgetReset.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"config": cfg,
	})
}

// ---------------------------------------------------------------------------
// Budget templates (B8-102)
// ---------------------------------------------------------------------------

type budgetTemplate struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	DailyUSD   float64  `json:"daily_usd,omitempty"`
	MonthlyUSD float64  `json:"monthly_usd,omitempty"`
	WarnAtPct  float64  `json:"warn_at_pct,omitempty"`
	Interval   string   `json:"interval,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

type budgetTemplatesStore struct {
	mu        sync.RWMutex
	templates []budgetTemplate
}

var globalBudgetTemplates = &budgetTemplatesStore{
	templates: []budgetTemplate{},
}

// handleBudgetTemplates serves GET/POST /admin/budget-templates.
func (s *Server) handleBudgetTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalBudgetTemplates.mu.RLock()
		tpls := append([]budgetTemplate(nil), globalBudgetTemplates.templates...)
		globalBudgetTemplates.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"templates": tpls, "count": len(tpls)})

	case http.MethodPost:
		var tpl budgetTemplate
		if err := json.NewDecoder(r.Body).Decode(&tpl); err != nil {
			writeError(w, http.StatusBadRequest, "invalid budget template: "+err.Error())
			return
		}
		if tpl.ID == "" {
			tpl.ID = newShortID()
		}
		globalBudgetTemplates.mu.Lock()
		globalBudgetTemplates.templates = append(globalBudgetTemplates.templates, tpl)
		globalBudgetTemplates.mu.Unlock()
		writeJSON(w, http.StatusCreated, tpl)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleBudgetTemplateApply serves POST /admin/budget-templates/{id}/apply.
func (s *Server) handleBudgetTemplateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/budget-templates/", "/apply")
	if id == "" {
		writeError(w, http.StatusBadRequest, "template id required")
		return
	}
	var body struct {
		KeyIDs []string `json:"key_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid apply request: "+err.Error())
		return
	}
	globalBudgetTemplates.mu.RLock()
	var tpl *budgetTemplate
	for i, t := range globalBudgetTemplates.templates {
		if t.ID == id {
			tpl = &globalBudgetTemplates.templates[i]
			break
		}
	}
	globalBudgetTemplates.mu.RUnlock()
	if tpl == nil {
		writeError(w, http.StatusNotFound, "template not found: "+id)
		return
	}
	applied := 0
	if s.cfg != nil {
		s.keyMu.Lock()
		for i := range s.cfg.GatewayKeys {
			for _, kid := range body.KeyIDs {
				if s.cfg.GatewayKeys[i].ID == kid {
					if tpl.DailyUSD > 0 {
						dv := tpl.DailyUSD
						setWindowLimit(&s.cfg.GatewayKeys[i], kid, "daily", &dv)
					}
					if tpl.MonthlyUSD > 0 {
						mv := tpl.MonthlyUSD
						setWindowLimit(&s.cfg.GatewayKeys[i], kid, "monthly", &mv)
					}
					applied++
				}
			}
		}
		if applied > 0 {
			s.persistConfig()
		}
		s.keyMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"template_id": id,
		"applied_to":  applied,
	})
}

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

type exclusiveLeasesStore struct {
	mu     sync.RWMutex
	leases map[string]bool // keyID → has lease
}

var globalExclusiveLeases = &exclusiveLeasesStore{
	leases: map[string]bool{},
}

// handleKeyExclusiveLease serves POST /admin/keys/{id}/lease (set/clear).
func (s *Server) handleKeyExclusiveLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := keyIDFromPath(r.URL.Path, "/admin/keys/", "/lease")
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}
	var body struct {
		Exclusive bool `json:"exclusive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid lease request: "+err.Error())
		return
	}
	globalExclusiveLeases.mu.Lock()
	globalExclusiveLeases.leases[id] = body.Exclusive
	globalExclusiveLeases.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        id,
		"exclusive": body.Exclusive,
	})
}

// ---------------------------------------------------------------------------
// Management access tokens (B8-99)
// ---------------------------------------------------------------------------

type managementToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Access    string     `json:"access"` // "read", "write", "admin"
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Revoked   bool       `json:"revoked,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	// RawToken is only returned once at creation.
	RawToken string `json:"token,omitempty"`
}

type managementTokensStore struct {
	mu     sync.RWMutex
	tokens []managementToken
}

var globalManagementTokens = &managementTokensStore{
	tokens: []managementToken{},
}

// handleManagementTokens serves GET /admin/management-tokens and
// POST /admin/management-tokens.
func (s *Server) handleManagementTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalManagementTokens.mu.RLock()
		// Never return raw token in list.
		out := make([]managementToken, len(globalManagementTokens.tokens))
		for i, t := range globalManagementTokens.tokens {
			out[i] = t
			out[i].RawToken = ""
		}
		globalManagementTokens.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "count": len(out)})

	case http.MethodPost:
		var req struct {
			Name      string     `json:"name"`
			Access    string     `json:"access"`
			ExpiresAt *time.Time `json:"expires_at,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid token request: "+err.Error())
			return
		}
		if req.Access == "" {
			req.Access = "read"
		}
		raw, err := generateSecureToken(32)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to generate token")
			return
		}
		tok := managementToken{
			ID:        newShortID(),
			Name:      req.Name,
			Access:    req.Access,
			ExpiresAt: req.ExpiresAt,
			CreatedAt: time.Now(),
			RawToken:  "mgt_" + raw,
		}
		globalManagementTokens.mu.Lock()
		globalManagementTokens.tokens = append(globalManagementTokens.tokens, tok)
		globalManagementTokens.mu.Unlock()
		// Return with raw token — shown exactly once.
		writeJSON(w, http.StatusCreated, tok)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleManagementTokenByID serves GET/DELETE /admin/management-tokens/{id}.
func (s *Server) handleManagementTokenByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/management-tokens/")
	switch r.Method {
	case http.MethodGet:
		globalManagementTokens.mu.RLock()
		defer globalManagementTokens.mu.RUnlock()
		for _, t := range globalManagementTokens.tokens {
			if t.ID == id {
				t.RawToken = ""
				writeJSON(w, http.StatusOK, t)
				return
			}
		}
		writeError(w, http.StatusNotFound, "token not found: "+id)

	case http.MethodDelete:
		globalManagementTokens.mu.Lock()
		defer globalManagementTokens.mu.Unlock()
		for i, t := range globalManagementTokens.tokens {
			if t.ID == id {
				globalManagementTokens.tokens[i].Revoked = true
				writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": true})
				return
			}
		}
		writeError(w, http.StatusNotFound, "token not found: "+id)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Key pools / fair-share quotas (B8-97)
// ---------------------------------------------------------------------------

type keyPool struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyIDs    []string `json:"key_ids"`
	ShareMode string   `json:"share_mode"` // "equal", "weighted"
	TotalUSD  float64  `json:"total_usd,omitempty"`
}

type keyPoolsStore struct {
	mu    sync.RWMutex
	pools []keyPool
}

var globalKeyPools = &keyPoolsStore{
	pools: []keyPool{},
}

// handleKeyPools serves GET/POST /admin/key-pools.
func (s *Server) handleKeyPools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalKeyPools.mu.RLock()
		pools := append([]keyPool(nil), globalKeyPools.pools...)
		globalKeyPools.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"pools": pools, "count": len(pools)})

	case http.MethodPost:
		var pool keyPool
		if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
			writeError(w, http.StatusBadRequest, "invalid key pool: "+err.Error())
			return
		}
		if pool.ID == "" {
			pool.ID = newShortID()
		}
		if pool.ShareMode == "" {
			pool.ShareMode = "equal"
		}
		globalKeyPools.mu.Lock()
		globalKeyPools.pools = append(globalKeyPools.pools, pool)
		globalKeyPools.mu.Unlock()
		writeJSON(w, http.StatusCreated, pool)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleKeyPoolByID serves GET/PATCH/DELETE /admin/key-pools/{id}.
func (s *Server) handleKeyPoolByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/key-pools/")
	switch r.Method {
	case http.MethodGet:
		globalKeyPools.mu.RLock()
		defer globalKeyPools.mu.RUnlock()
		for _, p := range globalKeyPools.pools {
			if p.ID == id {
				writeJSON(w, http.StatusOK, p)
				return
			}
		}
		writeError(w, http.StatusNotFound, "key pool not found: "+id)

	case http.MethodPatch:
		var patch keyPool
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid key pool patch: "+err.Error())
			return
		}
		globalKeyPools.mu.Lock()
		defer globalKeyPools.mu.Unlock()
		for i := range globalKeyPools.pools {
			if globalKeyPools.pools[i].ID == id {
				if patch.Name != "" {
					globalKeyPools.pools[i].Name = patch.Name
				}
				if patch.KeyIDs != nil {
					globalKeyPools.pools[i].KeyIDs = patch.KeyIDs
				}
				if patch.ShareMode != "" {
					globalKeyPools.pools[i].ShareMode = patch.ShareMode
				}
				if patch.TotalUSD > 0 {
					globalKeyPools.pools[i].TotalUSD = patch.TotalUSD
				}
				writeJSON(w, http.StatusOK, globalKeyPools.pools[i])
				return
			}
		}
		writeError(w, http.StatusNotFound, "key pool not found: "+id)

	case http.MethodDelete:
		globalKeyPools.mu.Lock()
		defer globalKeyPools.mu.Unlock()
		for i, p := range globalKeyPools.pools {
			if p.ID == id {
				globalKeyPools.pools = append(globalKeyPools.pools[:i], globalKeyPools.pools[i+1:]...)
				writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
				return
			}
		}
		writeError(w, http.StatusNotFound, "key pool not found: "+id)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

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

type ipFilterSettings struct {
	GlobalAllowlist []string `json:"global_allowlist,omitempty"`
	GlobalBlocklist []string `json:"global_blocklist,omitempty"`
	Enabled         bool     `json:"enabled"`
}

type ipFilterStore struct {
	mu  sync.RWMutex
	cfg ipFilterSettings
}

var globalIPFilter = &ipFilterStore{
	cfg: ipFilterSettings{Enabled: false},
}

// handleIPFilter serves GET/PATCH /api/settings/ip-filter.
func (s *Server) handleIPFilter(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalIPFilter.mu.RLock()
		cfg := globalIPFilter.cfg
		globalIPFilter.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalIPFilter.mu.Lock()
		cur := globalIPFilter.cfg
		globalIPFilter.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid ip filter settings: "+err.Error())
			return
		}
		globalIPFilter.mu.Lock()
		globalIPFilter.cfg = cur
		globalIPFilter.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

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

	// Per-key budget window limits (B8-11, 12, 13)
	s.register(mux, "PATCH", "/admin/keys/{id}/budget", s.handleKeyBudgetWindow)

	// Budget warn threshold (B8-100)
	s.register(mux, "PATCH", "/admin/keys/{id}/budget-warn", s.handleKeyBudgetWarn)

	// Budget reset interval (B8-101)
	s.register(mux, "PATCH", "/admin/keys/{id}/budget-reset", s.handleKeyBudgetReset)

	// Budget templates (B8-102)
	s.register(mux, "GET", "/admin/budget-templates", s.handleBudgetTemplates)
	s.register(mux, "POST", "/admin/budget-templates", s.handleBudgetTemplates)
	s.register(mux, "POST", "/admin/budget-templates/{id}/apply", s.handleBudgetTemplateApply)

	// Device tracking (B8-87)
	s.register(mux, "GET", "/api/keys/{id}/devices", s.handleKeyDevices)

	// Exclusive lease badge (B8-85)
	s.register(mux, "POST", "/admin/keys/{id}/lease", s.handleKeyExclusiveLease)

	// Management access tokens (B8-99)
	s.register(mux, "GET", "/admin/management-tokens", s.handleManagementTokens)
	s.register(mux, "POST", "/admin/management-tokens", s.handleManagementTokens)
	s.register(mux, "GET", "/admin/management-tokens/{id}", s.handleManagementTokenByID)
	s.register(mux, "DELETE", "/admin/management-tokens/{id}", s.handleManagementTokenByID)

	// Key pools (B8-97)
	s.register(mux, "GET", "/admin/key-pools", s.handleKeyPools)
	s.register(mux, "POST", "/admin/key-pools", s.handleKeyPools)
	s.register(mux, "GET", "/admin/key-pools/{id}", s.handleKeyPoolByID)
	s.register(mux, "PATCH", "/admin/key-pools/{id}", s.handleKeyPoolByID)
	s.register(mux, "DELETE", "/admin/key-pools/{id}", s.handleKeyPoolByID)

	// Per-key dashboard pages (B8-94, B8-95)
	s.register(mux, "GET", "/dashboard/keys/{id}/routing", s.handleKeyRoutingPage)
	s.register(mux, "GET", "/dashboard/keys/{id}/access", s.handleKeyAccessPage)

	// IP filter (B8-131)
	s.register(mux, "GET", "/api/settings/ip-filter", s.handleIPFilter)
	s.register(mux, "PATCH", "/api/settings/ip-filter", s.handleIPFilter)
}
