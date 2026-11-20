package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestBlockB7DashboardPart1Gate validates the end-to-end integration
// of Block B7 (Area 8a: Dashboard Part 1 — Home, Providers, Models, Pricing).
func TestBlockB7DashboardPart1Gate(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig(tmpDir)

	_, rawAdminKey := issueScopedKey(t, cfg, "dash-admin", []string{"admin", "chat"})

	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithVersion("0.1.0"),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "prov-openai", Name: "OpenAI", Enabled: true, WireFormat: "openai"},
			{ID: "prov-anthropic", Name: "Anthropic", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "gpt-4o", ProviderID: "prov-openai", OwnedBy: "OpenAI"},
			{ID: "claude-3-5-sonnet", ProviderID: "prov-anthropic", OwnedBy: "Anthropic"},
		}}),
		WithModelRefresher(&fakeRefresher{results: []provider.ModelRefreshResult{
			{ProviderID: "prov-openai", Status: provider.ModelRefreshOK, Models: []string{"gpt-4o"}},
		}}),
	)

	// ---------------------------------------------------------
	// 1. Single Binary HTML Delivery & View Sections
	// ---------------------------------------------------------
	t.Run("DashboardHTMLViewsRender", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()

		// Verify Overview / Home view section
		if !strings.Contains(body, `id="view-overview"`) {
			t.Errorf("GET /dashboard missing id=\"view-overview\"")
		}
		if !strings.Contains(body, `class="card kpi"`) {
			t.Errorf("GET /dashboard missing KPI cards")
		}

		// Verify Providers view section
		if !strings.Contains(body, `id="view-providers"`) {
			t.Errorf("GET /dashboard missing id=\"view-providers\"")
		}
		if !strings.Contains(body, `id="catalogCard"`) {
			t.Errorf("GET /dashboard missing id=\"catalogCard\"")
		}

		// Verify Models view section & tabs
		if !strings.Contains(body, `id="view-models"`) {
			t.Errorf("GET /dashboard missing id=\"view-models\"")
		}
		if !strings.Contains(body, `id="modelsTabModels"`) || !strings.Contains(body, `id="modelsTabCombos"`) {
			t.Errorf("GET /dashboard missing Models / Combos tab controls")
		}

		// Verify Name Rule: Zero benchmark brand mentions in rendered HTML
		prohibited := []string{"omni" + "route"}
		for _, p := range prohibited {
			if strings.Contains(strings.ToLower(body), p) {
				t.Fatalf("CRITICAL: Prohibited brand name %q found in dashboard HTML", p)
			}
		}
	})

	// ---------------------------------------------------------
	// 2. Live Dashboard Feeds (Part 1 Surfaces)
	// ---------------------------------------------------------
	t.Run("DashboardPart1LiveFeeds", func(t *testing.T) {
		// 2.1 Overview feed (GET /dashboard/overview)
		rec := doReq(t, srv, http.MethodGet, "/dashboard/overview", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/overview code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var overview map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
			t.Fatalf("decode /dashboard/overview: %v", err)
		}

		// 2.2 Providers feed (GET /dashboard/providers)
		rec = doReq(t, srv, http.MethodGet, "/dashboard/providers", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/providers code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var provs map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &provs); err != nil {
			t.Fatalf("decode /dashboard/providers: %v", err)
		}
		provList, ok := provs["providers"].([]any)
		if !ok || len(provList) != 2 {
			t.Errorf("providers count = %d, want 2", len(provList))
		}

		// 2.3 Models feed (GET /dashboard/models)
		rec = doReq(t, srv, http.MethodGet, "/dashboard/models", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/models code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var models map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
			t.Fatalf("decode /dashboard/models: %v", err)
		}
		modelList, ok := models["models"].([]any)
		if !ok || len(modelList) != 2 {
			t.Errorf("models count = %d, want 2", len(modelList))
		}

		// 2.4 Prices feed (GET /dashboard/prices)
		rec = doReq(t, srv, http.MethodGet, "/dashboard/prices", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/prices code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var prices map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &prices); err != nil {
			t.Fatalf("decode /dashboard/prices: %v", err)
		}
	})

	// ---------------------------------------------------------
	// 3. Action Wiring & Honest Empty States
	// ---------------------------------------------------------
	t.Run("DashboardActionEndpointsWired", func(t *testing.T) {
		// 3.1 Refresh models action
		rec := doReq(t, srv, http.MethodPost, "/dashboard/models/refresh", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /dashboard/models/refresh code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 3.2 Catalog audit feed
		rec = doReq(t, srv, http.MethodGet, "/dashboard/catalog-audit", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/catalog-audit code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 3.3 Models health endpoint
		rec = doReq(t, srv, http.MethodGet, "/v1/models/health", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/models/health code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
