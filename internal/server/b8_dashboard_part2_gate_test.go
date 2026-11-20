package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// doLocalReq sends an authenticated request with RemoteAddr set to loopback (127.0.0.1).
func doLocalReq(t *testing.T, srv *Server, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "127.0.0.1:54321"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestBlockB8DashboardPart2Gate tests all Dashboard Part 2 endpoints and functionality.
func TestBlockB8DashboardPart2Gate(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig(tmpDir)

	k, rawAdminKey := issueScopedKey(t, cfg, "b8-admin-key", []string{"admin", "models"})
	k2, _ := issueScopedKey(t, cfg, "b8-test-worker", []string{"chat"})

	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithVersion("0.1.0"),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "prov-openai", Name: "OpenAI", Enabled: true, WireFormat: "openai"},
		}}),
	)

	// -----------------------------------------------------------------------
	// 1. Key Management Extensions (B8-10..13, 85, 87, 94, 95, 97, 99..102, 131)
	// -----------------------------------------------------------------------
	t.Run("KeyManagementExtended", func(t *testing.T) {
		// 1.1 Single-key disable, enable, revoke
		rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+k2.ID+"/disable", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /admin/keys/{id}/disable: status %d, body: %s", rec.Code, rec.Body.String())
		}
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+k2.ID+"/enable", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /admin/keys/{id}/enable: status %d, body: %s", rec.Code, rec.Body.String())
		}

		// 1.2 Budget window limits
		budgetReq := `{"daily_usd": 15.5, "weekly_usd": 75.0, "monthly_usd": 250.0}`
		rec = doLocalReq(t, srv, http.MethodPatch, "/admin/keys/"+k.ID+"/budget", budgetReq, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /admin/keys/{id}/budget: status %d, body: %s", rec.Code, rec.Body.String())
		}
		var bRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &bRes)
		if bRes["daily_usd"] != 15.5 {
			t.Errorf("expected daily_usd 15.5, got %v", bRes["daily_usd"])
		}

		// 1.3 Budget warn and reset interval
		rec = doLocalReq(t, srv, http.MethodPatch, "/admin/keys/"+k.ID+"/budget-warn", `{"threshold_pct": 80}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /admin/keys/{id}/budget-warn: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/admin/keys/"+k.ID+"/budget-reset", `{"reset_interval": "weekly"}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /admin/keys/{id}/budget-reset: status %d", rec.Code)
		}

		// 1.4 Budget templates
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/budget-templates", `{"name": "Standard Dev", "daily_usd": 10.0, "monthly_usd": 200.0}`, rawAdminKey)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("POST /admin/budget-templates: status %d, body: %s", rec.Code, rec.Body.String())
		}
		var tplRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &tplRes)
		tplID, _ := tplRes["id"].(string)

		rec = doLocalReq(t, srv, http.MethodGet, "/admin/budget-templates", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/budget-templates: status %d", rec.Code)
		}

		rec = doLocalReq(t, srv, http.MethodPost, "/admin/budget-templates/"+tplID+"/apply", `{"key_ids": ["`+k.ID+`"]}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /admin/budget-templates/{id}/apply: status %d", rec.Code)
		}

		// 1.5 Device tracking per key
		rec = doLocalReq(t, srv, http.MethodGet, "/api/keys/"+k.ID+"/devices", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/keys/{id}/devices: status %d", rec.Code)
		}

		// 1.6 Exclusive lease badge
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+k.ID+"/lease", `{"lease_holder": "node-1", "duration_seconds": 3600}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /admin/keys/{id}/lease: status %d", rec.Code)
		}

		// 1.7 Management access tokens
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/management-tokens", `{"name": "CI-Runner", "role": "admin"}`, rawAdminKey)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("POST /admin/management-tokens: status %d", rec.Code)
		}
		var tokRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &tokRes)
		tokID, _ := tokRes["id"].(string)

		rec = doLocalReq(t, srv, http.MethodGet, "/admin/management-tokens", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/management-tokens: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodGet, "/admin/management-tokens/"+tokID, "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/management-tokens/{id}: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodDelete, "/admin/management-tokens/"+tokID, "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE /admin/management-tokens/{id}: status %d", rec.Code)
		}

		// 1.8 Key pools
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/key-pools", `{"name": "Research Pool", "quota_pct": 50}`, rawAdminKey)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("POST /admin/key-pools: status %d", rec.Code)
		}
		var poolRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &poolRes)
		poolID, _ := poolRes["id"].(string)

		rec = doLocalReq(t, srv, http.MethodGet, "/admin/key-pools", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/key-pools: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/admin/key-pools/"+poolID, `{"quota_pct": 60}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /admin/key-pools/{id}: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodDelete, "/admin/key-pools/"+poolID, "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE /admin/key-pools/{id}: status %d", rec.Code)
		}

		// 1.9 Per-key routing & access pages
		rec = doLocalReq(t, srv, http.MethodGet, "/dashboard/keys/"+k.ID+"/routing", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/keys/{id}/routing: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodGet, "/dashboard/keys/"+k.ID+"/access", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/keys/{id}/access: status %d", rec.Code)
		}

		// 1.10 IP filter
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/ip-filter", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/settings/ip-filter: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/ip-filter", `{"enabled": true, "allowlist": ["10.0.0.0/8"]}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /api/settings/ip-filter: status %d", rec.Code)
		}
	})

	// -----------------------------------------------------------------------
	// 2. Analytics Surfaces (B8-18, 19, 78, 82, 103, 105, 108, 113..115, 118, 121)
	// -----------------------------------------------------------------------
	t.Run("AnalyticsSurfaces", func(t *testing.T) {
		endpoints := []string{
			"/api/analytics/cost-explorer",
			"/api/analytics/spend-projection",
			"/api/analytics/budget-table",
			"/api/analytics/cost-by-key",
			"/api/analytics/cost-by-account",
			"/api/analytics/weekly-pattern",
			"/api/analytics/heatmap",
			"/api/analytics/utilization",
			"/api/analytics/search",
			"/api/analytics/free-rankings",
			"/api/analytics/scoring-inspector",
			"/api/analytics/candidates",
		}
		for _, ep := range endpoints {
			rec := doLocalReq(t, srv, http.MethodGet, ep, "", rawAdminKey)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200: %s", ep, rec.Code, rec.Body.String())
			}
		}
	})

	// -----------------------------------------------------------------------
	// 3. Logs & Diagnostics (B8-22..28, 126, 127)
	// -----------------------------------------------------------------------
	t.Run("LogsAndDiagnostics", func(t *testing.T) {
		logEndpoints := []string{
			"/api/logs/proxy",
			"/api/logs/console",
			"/api/logs/timeline/req-sample",
			"/api/logs/activity",
			"/api/logs/conversations",
			"/api/logs/compression",
			"/api/logs/audit",
			"/api/logs/export",
			"/api/logs/destinations",
		}
		for _, ep := range logEndpoints {
			rec := doLocalReq(t, srv, http.MethodGet, ep, "", rawAdminKey)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200: %s", ep, rec.Code, rec.Body.String())
			}
		}

		// Log destinations CRUD
		rec := doLocalReq(t, srv, http.MethodPost, "/api/logs/destinations", `{"name": "S3 Cold Storage", "type": "s3", "endpoint": "https://s3.example.com"}`, rawAdminKey)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/logs/destinations: status %d, body: %s", rec.Code, rec.Body.String())
		}
		var destRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &destRes)
		destID, _ := destRes["id"].(string)

		rec = doLocalReq(t, srv, http.MethodGet, "/api/logs/destinations/"+destID, "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/logs/destinations/{id}: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/logs/destinations/"+destID, `{"endpoint": "https://s3-v2.example.com"}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /api/logs/destinations/{id}: status %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodDelete, "/api/logs/destinations/"+destID, "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE /api/logs/destinations/{id}: status %d", rec.Code)
		}
	})

	// -----------------------------------------------------------------------
	// 4. Settings & Operational Controls (B8-5, 30, 35..56, 67, 69, 70, 117, 129, 130, 134, 136, 137, 139, 141)
	// -----------------------------------------------------------------------
	t.Run("SettingsAndControls", func(t *testing.T) {
		// Feature flags
		rec := doLocalReq(t, srv, http.MethodGet, "/api/features", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/features status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/features", `{"flags": {"dark_mode": true}}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/features status = %d", rec.Code)
		}

		// CORS settings
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/cors", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/cors status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/cors", `{"allowed_origins": ["https://app.codvian.com"]}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/cors status = %d", rec.Code)
		}

		// Blocked providers
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/blocked-providers", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/blocked-providers status = %d", rec.Code)
		}

		// AI Settings
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/ai", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/ai status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/ai", `{"default_thinking_budget": 2048}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/ai status = %d", rec.Code)
		}

		// Modality bridge
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/modality", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/modality status = %d", rec.Code)
		}

		// Pricing sync
		rec = doLocalReq(t, srv, http.MethodGet, "/api/pricing/sync/status", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/pricing/sync/status status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPost, "/api/pricing/sync", "", rawAdminKey)
		if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
			t.Errorf("POST /api/pricing/sync status = %d", rec.Code)
		}

		// Transform pipeline
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/transform", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/transform status = %d", rec.Code)
		}

		// Sidebar settings
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/sidebar", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/sidebar status = %d", rec.Code)
		}

		// Advanced settings
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/advanced", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/advanced status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/advanced", `{"max_body_bytes": 10485760}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/advanced status = %d", rec.Code)
		}

		// LKGP cache clear
		rec = doLocalReq(t, srv, http.MethodDelete, "/api/lkgp/cache", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("DELETE /api/lkgp/cache status = %d", rec.Code)
		}

		// Routing strategies catalog
		rec = doLocalReq(t, srv, http.MethodGet, "/api/routing/strategies", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/routing/strategies status = %d", rec.Code)
		}

		// Session stickiness
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/stickiness", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/stickiness status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/stickiness", `{"enabled": true, "ttl_seconds": 3600}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/stickiness status = %d", rec.Code)
		}

		// Global switches
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/switches", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/switches status = %d", rec.Code)
		}

		// Response validation
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/response-validation", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/response-validation status = %d", rec.Code)
		}

		// Model cache evict
		rec = doLocalReq(t, srv, http.MethodDelete, "/api/cache/gpt-4o", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("DELETE /api/cache/{model} status = %d", rec.Code)
		}
	})

	// -----------------------------------------------------------------------
	// 5. Combos Extended (B8-3, 58, 61..66, 77, 79, 83)
	// -----------------------------------------------------------------------
	t.Run("CombosExtended", func(t *testing.T) {
		// Capability-specific combo configuration
		rec := doLocalReq(t, srv, http.MethodGet, "/api/combos/default-combo/capabilities", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/combos/{id}/capabilities status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPut, "/api/combos/default-combo/capabilities", `{"capabilities": [{"capability": "chat", "model": "gpt-4o", "enabled": true}]}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PUT /api/combos/{id}/capabilities status = %d", rec.Code)
		}

		// Combo tuning settings
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/combo-tuning", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/combo-tuning status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/combo-tuning", `{"max_concurrency": 100, "max_nesting_depth": 8}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/combo-tuning status = %d", rec.Code)
		}

		// 16-factor scoring modes & mode packs
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/scoring-modes", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/scoring-modes status = %d", rec.Code)
		}
		var modesRes map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &modesRes)
		factors, _ := modesRes["factors"].([]any)
		if len(factors) != 16 {
			t.Errorf("expected 16 scoring factors, got %d", len(factors))
		}
		modePacks, _ := modesRes["mode_packs"].([]any)
		if len(modePacks) != 6 {
			t.Errorf("expected 6 mode packs, got %d", len(modePacks))
		}

		// Account fallback strategies
		rec = doLocalReq(t, srv, http.MethodGet, "/api/settings/account-fallback", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/settings/account-fallback status = %d", rec.Code)
		}
		rec = doLocalReq(t, srv, http.MethodPatch, "/api/settings/account-fallback", `{"strategy": "cheapest_reserve"}`, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("PATCH /api/settings/account-fallback status = %d", rec.Code)
		}

		// Studio cascade
		rec = doLocalReq(t, srv, http.MethodGet, "/api/combos/default-combo/studio-cascade", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/combos/{id}/studio-cascade status = %d", rec.Code)
		}
	})

	// -----------------------------------------------------------------------
	// 6. Name Rule: Zero benchmark brand leaks
	// -----------------------------------------------------------------------
	t.Run("BrandLeakVerification", func(t *testing.T) {
		prohibited := []string{"omni" + "route"}
		// Sample multiple endpoints to guarantee zero leaks in serialised responses
		testEndpoints := []string{
			"/api/features",
			"/api/routing/strategies",
			"/api/settings/scoring-modes",
			"/api/analytics/scoring-inspector",
		}
		for _, ep := range testEndpoints {
			rec := doLocalReq(t, srv, http.MethodGet, ep, "", rawAdminKey)
			body := strings.ToLower(rec.Body.String())
			for _, p := range prohibited {
				if strings.Contains(body, p) {
					t.Fatalf("CRITICAL: Prohibited brand name %q found in %s response", p, ep)
				}
			}
		}
	})
}
