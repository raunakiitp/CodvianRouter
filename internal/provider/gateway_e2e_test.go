package provider_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// TestGatewayE2E verifies an end-to-end flow: router starts with a mock
// upstream, a gateway key is bootstrapped, authentication works, and
// /v1/chat/completions returns a valid completion response. The upstream
// mock receives the request and the gateway returns the correct completion
// response.
func TestGatewayE2E(t *testing.T) {
	// Start an upstream mock server that the provider client will call.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The provider client sends Authorization: Bearer <apiKey>
		// Our gateway key is "test-gateway-key-e2e", which the server
		// validates and then forwards as the provider's apiKey.
		if r.Header.Get("Authorization") != "Bearer test-gateway-key-e2e" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello from upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstreamSrv.Close()

	// Create a provider client that points to the upstream mock.
	// The client's baseURL is the upstream mock URL.
	upstreamClient, err := provider.NewClient(upstreamSrv.URL, "test-gateway-key-e2e")
	if err != nil {
		t.Fatalf("NewClient for upstream: %v", err)
	}

	// Create a minimal engine that uses the upstream client.
	// We use the router's Engine with a single provider client.
	clients := map[string]provider.Client{
		router.ClientKey("mock-provider", ""): upstreamClient,
	}
	eng := router.NewEngine(clients, nil, router.Config{})

	// Start the real router server via httptest.NewServer.
	realSrv := server.New(eng,
		server.WithStaticKeys("test-gateway-key-e2e"),
		server.WithLogger(log.New(io.Discard, "", 0)),
	)
	testSrv := httptest.NewServer(realSrv.Handler())
	defer testSrv.Close()

	// Bootstrap a gateway key via the admin API (POST /admin/bootstrap).
	// If a key already exists, we get 409 which we ignore.
	bootstrapResp, err := http.Post(
		testSrv.URL+"/admin/bootstrap",
		"application/json",
		strings.NewReader(`{"key": "test-gateway-key-e2e"}`),
	)
	if err != nil {
		t.Fatalf("bootstrap POST: %v", err)
	}
	if bootstrapResp.StatusCode != http.StatusOK && bootstrapResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(bootstrapResp.Body)
		t.Fatalf("bootstrap returned %d: %s", bootstrapResp.StatusCode, string(body))
	}
	_ = bootstrapResp

	// Send an authenticated /v1/chat/completions request through the
	// router (testSrv) to the upstream mock.
	req, err := http.NewRequest(http.MethodPost, testSrv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}`))
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
