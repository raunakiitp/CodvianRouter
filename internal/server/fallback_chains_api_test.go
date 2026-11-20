// G11 Area 2 row 7: fallback-chains CRUD API — handler round-trip
// tests. The handlers are called directly (no server.go
// registration needed); the store is a real *router.Engine.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

func TestG11FallbackChainsAPIRoundTrip(t *testing.T) {
	eng := router.NewEngine(nil, nil, router.Config{})
	srv := newTestServer(t, eng)

	call := func(method, target, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		} else {
			req = httptest.NewRequest(method, target, nil)
		}
		rec := httptest.NewRecorder()
		switch {
		case method == http.MethodDelete:
			srv.handleFallbackChain(rec, req)
		default:
			srv.handleFallbackChains(rec, req)
		}
		return rec
	}

	// GET on an empty store lists nothing.
	rec := call(http.MethodGet, "/api/fallback/chains", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list empty = %d, want 200", rec.Code)
	}
	var list fallbackChainsResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 0 {
		t.Fatalf("GET list empty = %+v, %v, want 0 chains", list, err)
	}

	// POST create → 201.
	rec = call(http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "b", "priority": 1}, {"provider": "a", "priority": 0}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created fallbackChainView
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil || created.Model != "gpt-4o" || len(created.Entries) != 2 {
		t.Fatalf("POST create body = %+v, %v, want model gpt-4o with 2 entries", created, err)
	}
	if !created.Entries[0].Enabled || !created.Entries[1].Enabled {
		t.Fatalf("POST create entries = %+v, want enabled defaulting to true", created.Entries)
	}

	// GET list → the chain; GET ?model= → the chain.
	rec = call(http.MethodGet, "/api/fallback/chains", "")
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 1 {
		t.Fatalf("GET list = %+v, %v, want 1 chain", list, err)
	}
	rec = call(http.MethodGet, "/api/fallback/chains?model=gpt-4o", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET one = %d, want 200", rec.Code)
	}

	// POST update (same model) → 200, and the engine store
	// reflects the replacement, priority-sorted.
	rec = call(http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "c", "priority": 0}, {"provider": "a", "priority": 5, "enabled": false}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	atts := eng.FallbackChainFor("gpt-4o")
	if len(atts) != 1 || atts[0].ProviderID != "c" {
		t.Fatalf("engine FallbackChainFor after update = %+v, want [c] (disabled a skipped)", atts)
	}

	// DELETE → 200; GET list is empty again; DELETE again → 404.
	rec = call(http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", rec.Code)
	}
	rec = call(http.MethodGet, "/api/fallback/chains", "")
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 0 {
		t.Fatalf("GET list after delete = %+v, %v, want 0 chains", list, err)
	}
	rec = call(http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", rec.Code)
	}

	// Validation: empty model and empty entries are 400s.
	rec = call(http.MethodPost, "/api/fallback/chains", `{"model": "", "entries": [{"provider": "a"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST empty model = %d, want 400", rec.Code)
	}
	rec = call(http.MethodPost, "/api/fallback/chains", `{"model": "m", "entries": []}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST empty entries = %d, want 400", rec.Code)
	}
}

func TestG11FallbackChainsAPIWithoutStore(t *testing.T) {
	// An engine without the store answers 503, honestly,
	// instead of a fake empty list.
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/api/fallback/chains", nil)
	rec := httptest.NewRecorder()
	srv.handleFallbackChains(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET without store = %d, want 503", rec.Code)
	}
}

// TestG11FallbackChainsAPIThroughMux is the toggle test for the
// same CRUD round-trip, driven through the live server mux
// (srv.ServeHTTP) with an admin gateway key — the "runtime CRUD
// API verified" the PARITY row was missing. The direct-handler
// test above passes even if the routes are never registered;
// this one answers 404 when the three s.register lines in
// server.go are removed (verified by hand: comment them out and
// this test fails on every step), and 401 without a key.
func TestG11FallbackChainsAPIThroughMux(t *testing.T) {
	eng := router.NewEngine(nil, nil, router.Config{})
	cfg := &config.Config{}
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, eng, WithConfig(cfg))

	// No key → 401 on every method (TierManagement, admin scope).
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/fallback/chains", ""},
		{http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "a"}]}`},
		{http.MethodDelete, "/api/fallback/chains?model=gpt-4o", ""},
	} {
		if rec := doReq(t, srv, tc.method, tc.path, tc.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	// GET on an empty store lists nothing.
	rec := doReq(t, srv, http.MethodGet, "/api/fallback/chains", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list empty = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var list fallbackChainsResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 0 {
		t.Fatalf("GET list empty = %+v, %v, want 0 chains", list, err)
	}

	// POST create → 201.
	rec = doReq(t, srv, http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "b", "priority": 1}, {"provider": "a", "priority": 0}]}`, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created fallbackChainView
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil || created.Model != "gpt-4o" || len(created.Entries) != 2 {
		t.Fatalf("POST create body = %+v, %v, want model gpt-4o with 2 entries", created, err)
	}
	if !created.Entries[0].Enabled || !created.Entries[1].Enabled {
		t.Fatalf("POST create entries = %+v, want enabled defaulting to true", created.Entries)
	}

	// GET list → the chain; GET ?model= → the chain; unknown → 404.
	rec = doReq(t, srv, http.MethodGet, "/api/fallback/chains", "", raw)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 1 {
		t.Fatalf("GET list = %+v, %v, want 1 chain", list, err)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/fallback/chains?model=gpt-4o", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET one = %d, want 200", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/fallback/chains?model=missing", "", raw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing = %d, want 404", rec.Code)
	}

	// POST update (same model) → 200, and the engine store
	// reflects the replacement, priority-sorted.
	rec = doReq(t, srv, http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "c", "priority": 0}, {"provider": "a", "priority": 5, "enabled": false}]}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	atts := eng.FallbackChainFor("gpt-4o")
	if len(atts) != 1 || atts[0].ProviderID != "c" {
		t.Fatalf("engine FallbackChainFor after update = %+v, want [c] (disabled a skipped)", atts)
	}

	// DELETE → 200; GET list is empty again; DELETE again → 404.
	rec = doReq(t, srv, http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/api/fallback/chains", "", raw)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list.Chains) != 0 {
		t.Fatalf("GET list after delete = %+v, %v, want 0 chains", list, err)
	}
	rec = doReq(t, srv, http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "", raw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", rec.Code)
	}

	// Validation: empty model and empty entries are 400s.
	rec = doReq(t, srv, http.MethodPost, "/api/fallback/chains", `{"model": "", "entries": [{"provider": "a"}]}`, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST empty model = %d, want 400", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/api/fallback/chains", `{"model": "m", "entries": []}`, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST empty entries = %d, want 400", rec.Code)
	}
}
