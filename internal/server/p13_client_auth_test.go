// P1.3 — client-protocol compatibility: the gateway credential may be
// presented in any of the three forms real clients send, all carrying
// the same key. Bearer stays canonical; x-api-key serves Anthropic-style
// clients; x-goog-api-key serves the Gemini SDKs. A request with none of
// them is still 401 with the standard error envelope.
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// p13IssueRaw issues one gateway key against the setup's config and
// returns its raw secret.
func p13IssueRaw(t *testing.T, s *keyBudgetSetup) string {
	t.Helper()
	_, raw := issueScopedKey(t, s.cfg, "p13-client", []string{"chat", "admin"})
	return raw
}

// p13Probe probes GET /v1/models with a key presented via headers,
// returning the status code.
func p13Probe(t *testing.T, s *keyBudgetSetup, headers map[string]string) int {
	t.Helper()
	srv := s.server(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code
}

func TestP13GatewayKeyBearerOnly(t *testing.T) {
	s := newKeyBudgetSetup(t)
	if code := p13Probe(t, s, map[string]string{"Authorization": "Bearer definitely-not-a-valid-key"}); code != http.StatusUnauthorized {
		t.Fatalf("a bogus Bearer key must be 401, got %d", code)
	}
}

func TestP13GatewayKeyNoCredentialIs401(t *testing.T) {
	s := newKeyBudgetSetup(t)
	if code := p13Probe(t, s, nil); code != http.StatusUnauthorized {
		t.Fatalf("no credential must be 401, got %d", code)
	}
}

func TestP13GatewayKeyBasicSchemeIsNotACredential(t *testing.T) {
	s := newKeyBudgetSetup(t)
	if code := p13Probe(t, s, map[string]string{"Authorization": "Basic abcdef"}); code != http.StatusUnauthorized {
		t.Fatalf("a Basic Authorization header must not authenticate, got %d", code)
	}
}

func TestP13GatewayKeyXApiKeyAuthenticates(t *testing.T) {
	s := newKeyBudgetSetup(t)
	raw := p13IssueRaw(t, s)
	srv := s.server(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", raw)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key auth = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestP13GatewayKeyXGoogApiKeyAuthenticates(t *testing.T) {
	s := newKeyBudgetSetup(t)
	raw := p13IssueRaw(t, s)
	srv := s.server(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Goog-Api-Key", raw)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-goog-api-key auth = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestP13GatewayKeyBearerAuthenticates(t *testing.T) {
	s := newKeyBudgetSetup(t)
	raw := p13IssueRaw(t, s)
	srv := s.server(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer auth = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
