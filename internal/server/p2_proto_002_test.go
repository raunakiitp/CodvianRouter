package server

// P2-PROTO-002: Gemini native credential forms — accept ?key= query
// param scoped to /v1beta/, rejected elsewhere.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestP2_PROTO_002_QueryKeyScopedToGemini verifies that ?key= query
// credential is accepted on /v1beta/ paths but rejected elsewhere.
func TestP2_PROTO_002_QueryKeyScopedToGemini(t *testing.T) {
	// /v1beta/models with ?key= should be recognized as a credential
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models?key=test-key-123", nil)
	if got := requestKey(req); got != "test-key-123" {
		t.Errorf("Gemini ?key=: requestKey = %q, want %q", got, "test-key-123")
	}

	// /v1/chat/completions with ?key= should NOT be recognized
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=test-key-123", nil)
	if got := requestKey(req); got != "" {
		t.Errorf("OpenAI ?key=: requestKey = %q, want empty (query key rejected outside /v1beta/)", got)
	}

	// /v1beta/models without any credential should return empty
	req = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	if got := requestKey(req); got != "" {
		t.Errorf("Gemini no cred: requestKey = %q, want empty", got)
	}

	// Authorization header still works on /v1beta/
	req = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	if got := requestKey(req); got != "test-key-123" {
		t.Errorf("Gemini Bearer: requestKey = %q, want %q", got, "test-key-123")
	}
}
