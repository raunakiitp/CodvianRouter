package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// gatewayTestEngine adapts the real provider client to the gateway Engine
// interface so the request traverses the actual HTTP gateway before reaching
// the mock upstream.
type gatewayTestEngine struct {
	client *provider.Client
}

func (e gatewayTestEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return e.client.Chat(ctx, req)
}

func (e gatewayTestEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.client.ChatStream(ctx, req, onDelta)
}

// TestGatewayE2E verifies gateway authentication, request handling, provider
// forwarding, and the response path using a real provider client and mock
// upstream. The client request is sent to the gateway, never directly upstream.
func TestGatewayE2E(t *testing.T) {
	const gatewayKey = "test-gateway-key-e2e"
	const providerKey = "test-provider-key-e2e"

	upstreamRequests := make(chan *http.Request, 1)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected upstream route", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+providerKey {
			http.Error(w, "unexpected provider authorization", http.StatusUnauthorized)
			return
		}
		upstreamRequests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-e2e","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello from mock upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstreamSrv.Close()

	providerClient, err := provider.NewClient(upstreamSrv.URL, providerKey)
	if err != nil {
		t.Fatalf("provider.NewClient: %v", err)
	}
	engine := gatewayTestEngine{client: providerClient}
	gateway := server.New(engine, server.WithStaticKeys(gatewayKey))
	gatewaySrv := httptest.NewServer(gateway.Handler())
	defer gatewaySrv.Close()

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}`
	req, err := http.NewRequest(http.MethodPost, gatewaySrv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request creation: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway chat completion: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var responseBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&responseBody)
		t.Fatalf("gateway returned %d: %#v", resp.StatusCode, responseBody)
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if len(chatResp.Choices) != 1 || chatResp.Choices[0].Message.Content != "Hello from mock upstream" {
		t.Fatalf("unexpected gateway response: %#v", chatResp)
	}

	select {
	case upstreamReq := <-upstreamRequests:
		if upstreamReq.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", upstreamReq.URL.Path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not forward the request to the mock upstream")
	}
}
