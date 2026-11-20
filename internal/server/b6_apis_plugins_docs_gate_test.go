package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// b6FakeEngine implements Engine and embedRerankEngine.
type b6FakeEngine struct {
	fakeEngine
}

func (e *b6FakeEngine) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return &provider.EmbeddingResponse{
		Object: "list",
		Data: []provider.Embedding{
			{Object: "embedding", Index: 0, Embedding: json.RawMessage(`[0.1, 0.2, 0.3]`)},
		},
		Model: req.Model,
		Usage: provider.Usage{PromptTokens: 5, TotalTokens: 5},
	}, nil
}

func (e *b6FakeEngine) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	return &provider.RerankResponse{
		Model: req.Model,
		Results: []provider.RerankResult{
			{Index: 0, RelevanceScore: 0.95},
		},
	}, nil
}

func doB6Req(srv *Server, method, path, body, key string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "127.0.0.1:20128"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestBlockB6APIsPluginsAndDocsGate validates the end-to-end integration
// of Block B6 (Area 14a-14d: APIs, Plugins, and Documentation).
func TestBlockB6APIsPluginsAndDocsGate(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig(tmpDir)

	_, rawAdminKey := issueScopedKey(t, cfg, "admin-key", []string{"admin", "chat"})

	led, err := ledger.Open(filepath.Join(tmpDir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()

	eng := &b6FakeEngine{}
	srv := newTestServer(t, eng,
		WithConfig(cfg),
		WithLedger(led),
		WithVersion("0.1.0"),
		WithListenAddr("127.0.0.1:20128"),
	)

	// ---------------------------------------------------------
	// 1. Area 14a: Public /v1 API Endpoints
	// ---------------------------------------------------------
	t.Run("Area14a_PublicV1APIs", func(t *testing.T) {
		// 1.1 GET /v1/models
		rec := doB6Req(srv, http.MethodGet, "/v1/models", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/models code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var modelsResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &modelsResp); err != nil {
			t.Fatalf("decode /v1/models: %v", err)
		}
		if _, ok := modelsResp["data"].([]any); !ok {
			t.Errorf("/v1/models missing 'data' array")
		}

		// 1.2 POST /v1/chat/completions
		chatBody := `{"model":"mock-model","messages":[{"role":"user","content":"Hello B6"}]}`
		rec = doB6Req(srv, http.MethodPost, "/v1/chat/completions", chatBody, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/chat/completions code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.3 POST /v1/completions (legacy text completions)
		compBody := `{"model":"mock-model","prompt":"Hello B6 legacy prompt"}`
		rec = doB6Req(srv, http.MethodPost, "/v1/completions", compBody, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/completions code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.4 POST /v1/embeddings
		embedBody := `{"model":"mock-model","input":"text for embedding"}`
		rec = doB6Req(srv, http.MethodPost, "/v1/embeddings", embedBody, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/embeddings code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.5 POST /v1/rerank
		rerankBody := `{"model":"mock-model","query":"search query","documents":["doc1","doc2"]}`
		rec = doB6Req(srv, http.MethodPost, "/v1/rerank", rerankBody, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/rerank code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.6 GET /v1/me/status
		rec = doB6Req(srv, http.MethodGet, "/v1/me/status", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/me/status code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.7 GET /v1/me/sessions
		rec = doB6Req(srv, http.MethodGet, "/v1/me/sessions", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/me/sessions code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.8 Responses API: Create, List, Get, Delete
		respCreateBody := `{"model":"mock-model","input":"Responses API test"}`
		rec = doB6Req(srv, http.MethodPost, "/v1/responses", respCreateBody, rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/responses code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var createdResp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &createdResp)
		respID, _ := createdResp["id"].(string)

		rec = doB6Req(srv, http.MethodGet, "/v1/responses", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/responses code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		if respID != "" {
			rec = doB6Req(srv, http.MethodGet, "/v1/responses/"+respID, "", rawAdminKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/responses/%s code = %d, want 200", respID, rec.Code)
			}

			rec = doB6Req(srv, http.MethodDelete, "/v1/responses/"+respID, "", rawAdminKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("DELETE /v1/responses/%s code = %d, want 200", respID, rec.Code)
			}
		}

		// 1.9 Files API: List
		rec = doB6Req(srv, http.MethodGet, "/v1/files", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/files code = %d, want 200: %s", rec.Code, rec.Body.String())
		}

		// 1.10 Batches API: List
		rec = doB6Req(srv, http.MethodGet, "/v1/batches", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/batches code = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	// ---------------------------------------------------------
	// 2. Area 14b: Management / Admin APIs
	// ---------------------------------------------------------
	t.Run("Area14b_ManagementAndAdminAPIs", func(t *testing.T) {
		// 2.1 GET /api/catalog and GET /api/providers
		rec := doB6Req(srv, http.MethodGet, "/api/catalog", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/catalog code = %d, want 200", rec.Code)
		}

		rec = doB6Req(srv, http.MethodGet, "/api/providers", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/providers code = %d, want 200", rec.Code)
		}

		// 2.2 Combos: GET /v1/combos
		rec = doB6Req(srv, http.MethodGet, "/v1/combos", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/combos code = %d, want 200", rec.Code)
		}

		// 2.3 Cache Stats & Eviction: GET /api/cache/stats and DELETE /api/cache
		rec = doB6Req(srv, http.MethodGet, "/api/cache/stats", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/cache/stats code = %d, want 200", rec.Code)
		}

		rec = doB6Req(srv, http.MethodDelete, "/api/cache", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE /api/cache code = %d, want 200", rec.Code)
		}

		// 2.4 Resilience: GET /api/resilience
		rec = doB6Req(srv, http.MethodGet, "/api/resilience", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/resilience code = %d, want 200", rec.Code)
		}

		// 2.5 Webhooks: GET /api/webhooks
		rec = doB6Req(srv, http.MethodGet, "/api/webhooks", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/webhooks code = %d, want 200", rec.Code)
		}

		// 2.6 Teams: GET /api/teams
		rec = doB6Req(srv, http.MethodGet, "/api/teams", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/teams code = %d, want 200", rec.Code)
		}

		// 2.7 Quota Pools: GET /api/quota-pools
		rec = doB6Req(srv, http.MethodGet, "/api/quota-pools", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/quota-pools code = %d, want 200", rec.Code)
		}

		// 2.8 Analytics: GET /api/analytics/summary
		rec = doB6Req(srv, http.MethodGet, "/api/analytics/summary", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/analytics/summary code = %d, want 200", rec.Code)
		}

		// 2.9 Admin Status: GET /admin/status
		rec = doB6Req(srv, http.MethodGet, "/admin/status", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/status code = %d, want 200", rec.Code)
		}
	})

	// ---------------------------------------------------------
	// 3. Area 14c: In-Process Plugin Lifecycle & Safe Execution
	// ---------------------------------------------------------
	t.Run("Area14c_PluginsSafeArchitecture", func(t *testing.T) {
		// 3.1 GET /dashboard/plugins returns InProcessOnly: true
		rec := doB6Req(srv, http.MethodGet, "/dashboard/plugins", "", rawAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/plugins code = %d, want 200", rec.Code)
		}
		var feed map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
			t.Fatalf("decode /dashboard/plugins: %v", err)
		}
		if feed["in_process_only"] != true {
			t.Errorf("in_process_only = %v, want true", feed["in_process_only"])
		}

		// 3.2 Safety Enforcements: Dynamic/Arbitrary code loading rejected with 501 Not Implemented
		// per Section D.4 Rule #5 (deliberate security design divergence).
		for _, endpoint := range []string{"/admin/plugin/load", "/admin/service/install", "/admin/update"} {
			rec = doB6Req(srv, http.MethodPost, endpoint, "{}", rawAdminKey)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("POST %s code = %d, want 501 (honest safety refusal against dynamic code loading)", endpoint, rec.Code)
			}
		}
	})

	// ---------------------------------------------------------
	// 4. Area 14d: Docs & OpenAPI Spec
	// ---------------------------------------------------------
	t.Run("Area14d_OpenAPISpecAndDocs", func(t *testing.T) {
		// 4.1 GET /openapi.json returns valid OpenAPI 3.0+ schema
		rec := doB6Req(srv, http.MethodGet, "/openapi.json", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /openapi.json code = %d, want 200", rec.Code)
		}

		var spec map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("decode /openapi.json: %v", err)
		}

		openapiVersion, _ := spec["openapi"].(string)
		if !strings.HasPrefix(openapiVersion, "3.") {
			t.Errorf("openapi version = %q, want 3.x", openapiVersion)
		}

		paths, ok := spec["paths"].(map[string]any)
		if !ok || len(paths) < 40 {
			t.Fatalf("openapi paths count = %d, want >= 40", len(paths))
		}
	})
}
