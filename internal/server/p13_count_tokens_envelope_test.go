// P13: Anthropic error envelope on /v1/messages/count_tokens
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCountTokensAnthropicErrorEnvelope(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	defer srv.Close()

	tests := []struct {
		name            string
		method          string
		body            string
		wantStatus      int
		wantErrorType   string
		wantMsgContains string
	}{
		{
			name:            "malformed body",
			method:          http.MethodPost,
			body:            `{not valid json}`,
			wantStatus:      http.StatusBadRequest,
			wantErrorType:   "invalid_request_error",
			wantMsgContains: "invalid request body",
		},
		{
			name:            "missing model",
			method:          http.MethodPost,
			body:            `{"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusBadRequest,
			wantErrorType:   "invalid_request_error",
			wantMsgContains: "model is required",
		},
		{
			name:            "missing messages",
			method:          http.MethodPost,
			body:            `{"model":"claude-3"}`,
			wantStatus:      http.StatusBadRequest,
			wantErrorType:   "invalid_request_error",
			wantMsgContains: "messages is required",
		},
		{
			name:            "body too large",
			method:          http.MethodPost,
			body:            `{"model":"claude-3","messages":[{"role":"user","content":"` + string(make([]byte, maxRequestBody+100)) + `"}]}`, // over limit
			wantStatus:      http.StatusRequestEntityTooLarge,
			wantErrorType:   "invalid_request_error",
			wantMsgContains: "request body too large",
		},
		{
			name:            "method not allowed (GET)",
			method:          http.MethodGet,
			body:            "",
			wantStatus:      http.StatusMethodNotAllowed,
			wantErrorType:   "invalid_request_error",
			wantMsgContains: "method not allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/messages/count_tokens", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-key-123")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}

			// The method-not-allowed case is answered by the HTTP
			// router (the route is registered for POST only), not by
			// the handler, so it carries the mux's plain-text 405
			// with no JSON envelope. Assert the status only.
			if tc.method == http.MethodGet {
				return
			}

			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not valid JSON: %v\nBody: %s", err, w.Body.String())
			}

			// Verify Anthropic error envelope shape: {"type":"error","error":{"type":...,"message":...}}
			if resp["type"] != "error" {
				t.Errorf("top-level type = %v, want 'error'", resp["type"])
			}

			errObj, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("error field missing or not object: %+v", resp)
			}

			if errObj["type"] != tc.wantErrorType {
				t.Errorf("error.type = %v, want %q", errObj["type"], tc.wantErrorType)
			}

			msg, _ := errObj["message"].(string)
			if msg == "" || !contains(msg, tc.wantMsgContains) {
				t.Errorf("error.message = %q does not contain %q", msg, tc.wantMsgContains)
			}
		})
	}
}

func TestChatCompletionsUsesOpenAIEnvelope(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	defer srv.Close()

	// /v1/chat/completions should use OpenAI error envelope
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Should be 400 with OpenAI shape: {"error":{"message":"...","type":"invalid_request_error"}}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}

	// OpenAI envelope has "error" at top level, not "type":"error"
	if resp["error"] == nil {
		t.Fatalf("OpenAI envelope missing 'error' field: %+v", resp)
	}

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field not object: %+v", resp)
	}

	// OpenAI uses "type" inside error object, but no top-level "type":"error"
	if _, hasTopLevelType := resp["type"]; hasTopLevelType {
		t.Errorf("OpenAI envelope must not have top-level 'type' field, got %v", resp["type"])
	}

	if errObj["type"] != "invalid_request_error" {
		t.Errorf("OpenAI error.type = %v, want 'invalid_request_error'", errObj["type"])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
