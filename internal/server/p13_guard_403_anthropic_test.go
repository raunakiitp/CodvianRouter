package server

// P2-IDENT-001: guard-layer 403 on /v1/messages must use the Anthropic
// envelope, not the OpenAI envelope.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noScopeVerifier implements KeyIDVerifier and ScopeVerifier: it verifies
// the key but returns zero scopes, so any requireScope check fails.
type noScopeVerifier struct{ key string }

func (v *noScopeVerifier) VerifyKey(_ context.Context, key string) bool { return key == v.key }
func (v *noScopeVerifier) KeyID(_ context.Context, key string) (string, bool) {
	if key == v.key {
		return "static-key", true
	}
	return "", false
}
func (v *noScopeVerifier) KeyScopes(_ context.Context, key string) ([]string, bool) {
	if key == v.key {
		return nil, true
	}
	return nil, false
}

// TestP2_IDENT_001_Guard403UsesAnthropicEnvelope verifies that a
// scope-denied key hitting /v1/messages gets an Anthropic-shaped 403,
// while the same denial on /v1/chat/completions gets OpenAI-shaped 403.
func TestP2_IDENT_001_Guard403UsesAnthropicEnvelope(t *testing.T) {
	// Create a server with a verifier that provides no scopes for the key,
	// so any scope requirement will fail.
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = &noScopeVerifier{key: "test-key-123"}

	// The requireScope wrapper is applied to routes that need specific scopes.
	// We test it directly by creating a handler that requires "admin" scope
	// on both an Anthropic and an OpenAI path.

	// Anthropic path: /v1/messages
	anthroMux := http.NewServeMux()
	anthroMux.HandleFunc("POST /v1/messages",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	anthroMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Anthropic /v1/messages: status = %d, want 403, body: %s", rec.Code, rec.Body.String())
	}
	// Assert Anthropic envelope: top-level type must be "error"
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Anthropic /v1/messages: body is not JSON (%v): %s", err, rec.Body.String())
	}
	if env.Type != "error" {
		t.Fatalf("Anthropic /v1/messages: top-level type = %q, want \"error\" (Anthropic envelope): %s", env.Type, rec.Body.String())
	}

	// OpenAI path: /v1/chat/completions
	openAIMux := http.NewServeMux()
	openAIMux.HandleFunc("POST /v1/chat/completions",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	openAIMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("OpenAI /v1/chat/completions: status = %d, want 403, body: %s", rec.Code, rec.Body.String())
	}
	// Assert OpenAI envelope: top-level type must NOT be "error"
	var openEnv struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &openEnv); err != nil {
		t.Fatalf("OpenAI /v1/chat/completions: body is not JSON (%v): %s", err, rec.Body.String())
	}
	if openEnv.Type == "error" {
		t.Fatalf("OpenAI /v1/chat/completions: top-level type = %q, should NOT be \"error\" (OpenAI envelope): %s", openEnv.Type, rec.Body.String())
	}
}
