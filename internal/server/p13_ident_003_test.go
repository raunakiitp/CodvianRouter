package server

// P2-IDENT-003: StaticKeyVerifier now implements ScopeVerifier, so
// scope enforcement fails closed instead of silently passing.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestP2_IDENT_003_StaticKeyVerifierScopes verifies that
// StaticKeyVerifier implements ScopeVerifier and returns empty scopes.
func TestP2_IDENT_003_StaticKeyVerifierScopes(t *testing.T) {
	v := NewStaticKeyVerifier("test-key-123")
	scopes, ok := v.KeyScopes(context.Background(), "test-key-123")
	if !ok {
		t.Fatal("KeyScopes returned ok=false for a valid key")
	}
	if scopes != nil {
		t.Errorf("KeyScopes returned %v, want nil (empty scope set)", scopes)
	}
	// Invalid key returns ok=false.
	if _, ok := v.KeyScopes(context.Background(), "bad-key"); ok {
		t.Error("KeyScopes returned ok=true for an invalid key")
	}
}

// TestP2_IDENT_003_RequireScopeFailsClosed verifies that requireScope
// returns 403 for a static key with no scopes (previously it passed
// through silently because StaticKeyVerifier did not implement
// ScopeVerifier).
func TestP2_IDENT_003_RequireScopeFailsClosed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("test-key-123")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (scope enforcement fails closed)", w.Code)
	}
}

// TestP2_IDENT_003_GuardStatusReportsScopeEnforcement verifies that
// GET /dashboard/guard reports scope_enforcement: true when a verifier
// is configured.
func TestP2_IDENT_003_GuardStatusReportsScopeEnforcement(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("test-key-123")

	// Get the guard status directly via the handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard/guard", srv.handleGuard)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/guard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !contains(body, `"scope_enforcement":true`) {
		t.Errorf("body does not contain scope_enforcement=true: %s", body)
	}
}
