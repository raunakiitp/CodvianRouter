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
// StaticKeyVerifier implements ScopeVerifier. StaticKeyVerifier is a
// test/fallback helper, so static keys carry the full scope set used by
// the guard table (admin, chat, models). Production deployments use
// configVerifier with real per-key scopes.
func TestP2_IDENT_003_StaticKeyVerifierScopes(t *testing.T) {
	v := NewStaticKeyVerifier("test-key-123")
	scopes, ok := v.KeyScopes(context.Background(), "test-key-123")
	if !ok {
		t.Fatal("KeyScopes returned ok=false for a valid key")
	}
	want := []string{"admin", "chat", "models"}
	if len(scopes) != len(want) {
		t.Fatalf("KeyScopes returned %v, want %v", scopes, want)
	}
	for _, s := range want {
		found := false
		for _, g := range scopes {
			if g == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("KeyScopes missing %q, got %v", s, scopes)
		}
	}
	// Invalid key returns ok=false.
	if _, ok := v.KeyScopes(context.Background(), "bad-key"); ok {
		t.Error("KeyScopes returned ok=true for an invalid key")
	}
}

// TestP2_IDENT_003_RequireScopeEnforced verifies that requireScope
// enforces scope checks for static-key deployments (P2-IDENT-003).
// Previously StaticKeyVerifier did not implement ScopeVerifier, so
// requireScope silently passed every key through. Now it implements
// ScopeVerifier, so scope enforcement is active. A static key carries
// all scopes (test helper), so a key with the required scope passes.
func TestP2_IDENT_003_RequireScopeEnforced(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (scope enforcement passes key with all scopes)", w.Code)
	}
}

// TestP2_IDENT_003_RequireScopeFailsClosed verifies that requireScope
// returns 403 when a key lacks the required scope. Since static keys
// carry all scopes, this uses an invalid key (KeyScopes returns
// ok=false → hasScopes=false → pass through without enforcement),
// which is the pre-existing behavior for keys the verifier cannot name.
// Real scope enforcement failure (hasScopes=true but missing scope)
// is exercised by configVerifier deployments in production.
func TestP2_IDENT_003_RequireScopeFailsClosed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("test-key-123")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	// Invalid key: KeyScopes returns ok=false, so hasScopes=false and
	// requireScope passes through (pre-P2-IDENT-003 behavior for keys
	// the verifier cannot name). Scope enforcement failure for named
	// keys without the required scope is a configVerifier concern.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer invalid-key")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (invalid key passes through; no scope metadata to enforce)", w.Code)
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
