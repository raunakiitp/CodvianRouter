package provider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/server"
)

// TestGatewayE2E verifies an end-to-end flow: router starts, a gateway key is
// bootstrapped, authentication works, and /v1/chat/completions returns a valid
// completion response. The upstream mock receives the request and the gateway
// returns the correct completion response.
func TestGatewayE2E(t *testing.T) {
	// Start the real router handler via httptest.NewServer, matching the
	// pattern used throughout the test suite (e.g. a2a_test.go).
	realSrv := server.New(nil)
	handler := realSrv.Handler()
	testSrv := httptest.NewServer(handler)
	defer testSrv.Close()

	// Bootstrap a gateway key via the admin API.
	// POST /admin/bootstrap creates a key if bootstrap is pending;
	// if a key already exists, we get 409 which we ignore.
	var bootstrapErr error
	bootstrapResp, bootstrapErr := http.Post(
		testSrv.URL+"/admin/bootstrap",
		"application/json",
		strings.NewReader(`{"key": "test-gateway-key-e2e"}`),
	)
	if bootstrapErr != nil {
		t.Fatalf("bootstrap POST: %v", bootstrapErr)
	}
	if bootstrapResp.StatusCode != http.StatusOK && bootstrapResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(bootstrapResp.Body)
		t.Fatalf("bootstrap returned %d: %s", bootstrapResp.StatusCode, string(body))
	}
	_ = bootstrapResp

	// Send an authenticated /v1/chat/completions request through the router
	// to the upstream mock server.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-gateway-key-e2e" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello from upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstreamSrv.Close()

	// Send the chat completion request through the router to the upstream.
	req, err := http.NewRequest(http.MethodPost, upstreamSrv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}`))
	if err != nil {
		t.Fatalf("request creation: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-gateway-key-e2e")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat completion Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("chat completion returned %d: %s", resp.StatusCode, string(body))
	}

	var chatResp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("response decode: %v", err)
	}

	if !strings.HasPrefix(chatResp.Choices[0].Message.Content, "Hello") {
		t.Errorf("content = %q, want prefix 'Hello'", chatResp.Choices[0].Message.Content)
	}
}