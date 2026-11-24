package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

type mcpTestProviders struct {
	providers []provider.DashboardProvider
}

func (f mcpTestProviders) DashboardProviders() []provider.DashboardProvider {
	return f.providers
}

type mcpTestRegistry struct {
	models []Model
}

func (f mcpTestRegistry) Models(_ context.Context) ([]Model, error) {
	return f.models, nil
}

func TestHandleMCP(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{},
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder"},
			{ID: "p2", Name: "Chatter"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{{ID: "a"}, {ID: "b"}, {ID: "c"}}}),
		WithListenAddr("127.0.0.1:20128"),
	)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/mcp", nil)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/mcp status = %d, want 200", rec.Code)
	}
	var out mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Transport != "stdio" || out.Command != "codvian-router mcp" {
		t.Errorf("transport/command = %q/%q, want stdio/codvian-router mcp", out.Transport, out.Command)
	}
	if len(out.Tools) != 32 {
		t.Fatalf("tools = %d, want 32", len(out.Tools))
	}
	names := map[string]bool{}
	for _, tool := range out.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"router_status", "provider_list", "model_list"} {
		if !names[want] {
			t.Errorf("tool %q missing from feed", want)
		}
	}
	if out.Providers != 2 || out.Models != 3 {
		t.Errorf("providers/models = %d/%d, want 2/3 (live counts)", out.Providers, out.Models)
	}
	if out.Listen != "127.0.0.1:20128" || !out.Ready {
		t.Errorf("listen/ready = %q/%v, want 127.0.0.1:20128/true", out.Listen, out.Ready)
	}
}

func TestHandleMCPEmptyState(t *testing.T) {
	// No registry, no provider lister: honest zeros, not an error.
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/dashboard/mcp", nil)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/mcp status = %d, want 200", rec.Code)
	}
	var out mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Providers != 0 || out.Models != 0 || out.Ready {
		t.Errorf("empty state = providers %d models %d ready %v, want 0/0/false", out.Providers, out.Models, out.Ready)
	}
	if out.Listen != "unknown" {
		t.Errorf("listen = %q, want unknown when unset", out.Listen)
	}
	if len(out.Tools) != 32 {
		t.Errorf("tools = %d even with no registry, want 32 (the tool list is static)", len(out.Tools))
	}
}

func TestHandleMCPMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodPost, "/dashboard/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /dashboard/mcp status = %d, want 405", rec.Code)
	}
}
