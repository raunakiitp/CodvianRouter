package server

// oauth_redirect_validation_test.go — P2-SEC-03: validate redirect_uri on error path

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/oauth"
)

// TestOAuthAuthorizeErrorValidatesRedirectURI ensures that oauthAuthorizeError
// refuses to redirect to a redirect_uri that is not registered for the client.
func TestOAuthAuthorizeErrorValidatesRedirectURI(t *testing.T) {
	iss, err := oauth.NewIssuer("")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	client, _, err := iss.RegisterClient("test-client", []string{"https://valid.example.com/callback"}, []string{"chat", "models"}, false)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	srv := newTestServer(t, &fakeEngine{}, WithStaticKeys("test-key-123"))
	srv.oauth = iss
	testVerifier := &testScopeVerifier{key: "test-key-123", scopes: []string{"admin"}}
	srv.verifier = testVerifier

	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize",
		srv.requireAuthUnlessRelaxed(
			srv.requireScope(srv.handleOAuthAuthorize, []string{"admin"}),
		))

	// Test 1: valid redirect_uri should redirect
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?response_type=code&client_id="+client.ID+"&redirect_uri=https%3A%2F%2Fvalid.example.com%2Fcallback&state=xyz", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("valid redirect_uri: status = %d, want 302, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") == "" {
		t.Error("valid redirect_uri: no Location header")
	}

	// Test 2: unregistered redirect_uri should NOT redirect (should return 400)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?response_type=code&client_id="+client.ID+"&redirect_uri=https%3A%2F%2Fevil.com%2Fsteal&state=xyz", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unregistered redirect_uri: status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Error("unregistered redirect_uri: should not have Location header")
	}

	// Test 3: missing client_id should return 400 (not redirect)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?response_type=code&redirect_uri=https%3A%2F%2Fvalid.example.com%2Fcallback&state=xyz", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing client_id: status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}

	// Test 4: unknown client_id should return 400 (not redirect)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?response_type=code&client_id=unknown&redirect_uri=https%3A%2F%2Fvalid.example.com%2Fcallback&state=xyz", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown client_id: status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// testScopeVerifier is a test verifier that provides a fixed set of scopes for a key.
type testScopeVerifier struct {
	key    string
	scopes []string
}

func (v *testScopeVerifier) VerifyKey(_ context.Context, key string) bool {
	return key == v.key
}

func (v *testScopeVerifier) KeyScopes(_ context.Context, key string) ([]string, bool) {
	if key == v.key {
		return v.scopes, true
	}
	return nil, false
}

func (v *testScopeVerifier) KeyID(_ context.Context, key string) (string, bool) {
	if key == v.key {
		return "test-key", true
	}
	return "", false
}
