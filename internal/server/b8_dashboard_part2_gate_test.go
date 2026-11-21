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

// TestBlockB8DashboardPart2Gate checks the Dashboard Part 2 routes
// that are backed by real router state. Phase 0 (2026-11-21): the
// B8 routes that only stored values in a process-local map nothing
// read (budget windows, warn/reset, templates, leases, management
// tokens, key pools, IP filter, feature flags, CORS, AI defaults,
// stickiness, switches, transform, sidebar, advanced, response
// validation, pricing sync, combo capabilities/tuning, account
// fallback, the never-written log feeds and log destinations) now
// answer 501 feature_unavailable; that contract is checked route by
// route in TestUnavailableRoutesHonest, and asserted here for a
// sample so this gate cannot regress to accepting them.
func TestBlockB8DashboardPart2Gate(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig(tmpDir)

	_, rawAdminKey := issueScopedKey(t, cfg, "b8-admin-key", []string{"admin", "models"})
	k2, _ := issueScopedKey(t, cfg, "b8-test-worker", []string{"chat"})

	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithVersion("0.1.0"),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "prov-openai", Name: "OpenAI", Enabled: true, WireFormat: "openai"},
		}}),
	)

	t.Run("KeyManagement", func(t *testing.T) {
		rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+k2.ID+"/disable", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("disable: status %d, body: %s", rec.Code, rec.Body.String())
		}
		disabled := false
		for _, k := range cfg.GatewayKeys {
			if k.ID == k2.ID {
				disabled = k.Disabled
			}
		}
		if !disabled {
			t.Fatal("disable answered 200 but the key is not disabled in config")
		}
		rec = doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+k2.ID+"/enable", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("enable: status %d, body: %s", rec.Code, rec.Body.String())
		}
		for _, p := range []string{"/api/keys/" + k2.ID + "/devices", "/dashboard/keys/" + k2.ID + "/routing", "/dashboard/keys/" + k2.ID + "/access"} {
			if rec := doLocalReq(t, srv, http.MethodGet, p, "", rawAdminKey); rec.Code != http.StatusOK {
				t.Errorf("GET %s: status %d", p, rec.Code)
			}
		}
		// Never-enforced controls refuse instead of pretending.
		for _, c := range []struct{ m, p, b string }{
			{http.MethodPatch, "/admin/keys/" + k2.ID + "/budget", `{"daily_usd": 15.5}`},
			{http.MethodPost, "/admin/management-tokens", `{"name":"x"}`},
			{http.MethodPatch, "/api/settings/ip-filter", `{"enabled":true}`},
		} {
			if rec := doLocalReq(t, srv, c.m, c.p, c.b, rawAdminKey); rec.Code != http.StatusNotImplemented {
				t.Errorf("%s %s: status %d, want 501 feature_unavailable", c.m, c.p, rec.Code)
			}
		}
	})

	t.Run("AnalyticsSurfaces", func(t *testing.T) {
		for _, ep := range []string{
			"/api/analytics/cost-explorer",
			"/api/analytics/spend-projection",
			"/api/analytics/budget-table",
			"/api/analytics/cost-by-key",
			"/api/analytics/weekly-pattern",
			"/api/analytics/heatmap",
			"/api/analytics/utilization",
			"/api/analytics/search",
			"/api/analytics/free-rankings",
			"/api/analytics/scoring-inspector",
			"/api/analytics/candidates",
		} {
			if rec := doLocalReq(t, srv, http.MethodGet, ep, "", rawAdminKey); rec.Code != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200: %s", ep, rec.Code, rec.Body.String())
			}
		}
		if rec := doLocalReq(t, srv, http.MethodGet, "/api/analytics/cost-by-account", "", rawAdminKey); rec.Code != http.StatusNotImplemented {
			t.Errorf("cost-by-account: status %d, want 501", rec.Code)
		}
	})

	t.Run("Logs", func(t *testing.T) {
		if rec := doLocalReq(t, srv, http.MethodGet, "/api/logs/export", "", rawAdminKey); rec.Code != http.StatusOK {
			t.Errorf("GET /api/logs/export: status %d", rec.Code)
		}
		if rec := doLocalReq(t, srv, http.MethodGet, "/api/logs/proxy", "", rawAdminKey); rec.Code != http.StatusNotImplemented {
			t.Errorf("GET /api/logs/proxy: status %d, want 501", rec.Code)
		}
	})

	t.Run("SettingsAndControls", func(t *testing.T) {
		if rec := doLocalReq(t, srv, http.MethodDelete, "/api/lkgp/cache", "", rawAdminKey); rec.Code != http.StatusOK {
			t.Errorf("DELETE /api/lkgp/cache status = %d", rec.Code)
		}
		if rec := doLocalReq(t, srv, http.MethodGet, "/api/routing/strategies", "", rawAdminKey); rec.Code != http.StatusOK {
			t.Errorf("GET /api/routing/strategies status = %d", rec.Code)
		}
		if rec := doLocalReq(t, srv, http.MethodDelete, "/api/cache/gpt-4o", "", rawAdminKey); rec.Code != http.StatusOK {
			t.Errorf("DELETE /api/cache/{model} status = %d", rec.Code)
		}
		if rec := doLocalReq(t, srv, http.MethodPatch, "/api/features", `{"flags":{"x":true}}`, rawAdminKey); rec.Code != http.StatusNotImplemented {
			t.Errorf("PATCH /api/features: status %d, want 501", rec.Code)
		}
	})

	t.Run("CombosExtended", func(t *testing.T) {
		rec := doLocalReq(t, srv, http.MethodGet, "/api/settings/scoring-modes", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/settings/scoring-modes status = %d", rec.Code)
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
		// An unknown combo is 404, not an empty "active" cascade.
		if rec := doLocalReq(t, srv, http.MethodGet, "/api/combos/no-such-combo/studio-cascade", "", rawAdminKey); rec.Code != http.StatusNotFound {
			t.Errorf("unknown combo studio-cascade: status %d, want 404", rec.Code)
		}
	})

	t.Run("BrandLeakVerification", func(t *testing.T) {
		prohibited := []string{"omni" + "route"}
		for _, ep := range []string{
			"/api/routing/strategies",
			"/api/settings/scoring-modes",
			"/api/analytics/scoring-inspector",
		} {
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
