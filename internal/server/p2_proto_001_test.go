package server

// P2-PROTO-001: Guard-layer 401/403 on /v1beta/... (Gemini surface)
// returns the Gemini error envelope {error:{code,message,status}},
// not the OpenAI envelope.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestP2_PROTO_001_Guard401UsesGeminiEnvelope verifies that a 401 on
// /v1beta/models returns the Gemini error envelope, while the same
// denial on /v1/chat/completions returns the OpenAI envelope.
func TestP2_PROTO_001_Guard401UsesGeminiEnvelope(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = &noScopeVerifier{key: "test-key-123"}

	// Gemini path: /v1beta/models
	geminiMux := http.NewServeMux()
	geminiMux.HandleFunc("GET /v1beta/models",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	geminiMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Gemini /v1beta/models: status = %d, want 403, body: %s", rec.Code, rec.Body.String())
	}
	// Assert Gemini envelope: top-level has "error" object with "code", "message", "status"
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Gemini /v1beta/models: body is not JSON (%v): %s", err, rec.Body.String())
	}
	if env.Error.Code != http.StatusForbidden {
		t.Errorf("Gemini error.code = %d, want %d", env.Error.Code, http.StatusForbidden)
	}
	if env.Error.Status == "" {
		t.Error("Gemini error.status is empty")
	}
	if env.Error.Message == "" {
		t.Error("Gemini error.message is empty")
	}

	// OpenAI path: /v1/chat/completions
	openAIMux := http.NewServeMux()
	openAIMux.HandleFunc("POST /v1/chat/completions",
		srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		}, []string{"admin"}))

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	openAIMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("OpenAI /v1/chat/completions: status = %d, want 403, body: %s", rec.Code, rec.Body.String())
	}
	// Assert OpenAI envelope: top-level has "error" object with "message" and "type" but NOT "status"
	var openEnv struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &openEnv); err != nil {
		t.Fatalf("OpenAI /v1/chat/completions: body is not JSON (%v): %s", err, rec.Body.String())
	}
	if openEnv.Error.Status != "" {
		t.Errorf("OpenAI error.status = %q, should be empty (not Gemini envelope)", openEnv.Error.Status)
	}
}
