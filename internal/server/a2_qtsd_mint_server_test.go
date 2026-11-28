// A2-5 server-level proof: qtSd/ virtual-model minting is reachable
// from a real HTTP request through the real engine's model-resolution
// path — not only an engine unit test. A chat request naming model
// "qtSd/pool/prov/m" is unwrapped to the routing group's provider/model
// set and routed as a quota-share pool (the DRR picks the high-share
// member), served at the concrete model "prov/m".
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// qtSdStaticClient returns a fixed content for every chat.
type qtSdStaticClient struct{ content string }

func (c *qtSdStaticClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *qtSdStaticClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *qtSdStaticClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *qtSdStaticClient) TestKey(ctx context.Context) error                { return nil }

// qtSdServerEngine builds a real engine whose "pool" group lists
// [b, a] serving the concrete model, with quota-share weights favouring
// a (share 10) over b (share 1): the DRR must pick a even though b is
// first in the group's member order.
func qtSdServerEngine(t *testing.T) *router.Engine {
	t.Helper()
	cfg := router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		QuotaShareFor: func(providerID, model string) float64 {
			if providerID == "a" {
				return 10
			}
			if providerID == "b" {
				return 1
			}
			return 1
		},
		GroupChain: func(group, model string) ([]router.Attempt, bool) {
			if group != "pool" || model != "prov/m" {
				return nil, false
			}
			return []router.Attempt{{ProviderID: "b", Model: model}, {ProviderID: "a", Model: model}}, true
		},
	}
	return router.NewEngine(map[string]provider.Client{
		"a": &qtSdStaticClient{content: "answer-from-a"},
		"b": &qtSdStaticClient{content: "answer-from-b"},
	}, nil, cfg)
}

// TestServerQtSdMintReachable is the production-reachability proof for
// A2-5: a real HTTP chat request naming model "qtSd/pool/prov/m" is
// minted to the quota-share pool and served at the concrete model
// "prov/m" by the DRR winner (provider "a", share 10, despite being
// listed second in the group). FAILS-WHEN-OFF: without the minting the
// served model is not "prov/m" and the DRR does not run.
func TestServerQtSdMintReachable(t *testing.T) {
	srv := newTestServer(t, qtSdServerEngine(t))
	body := `{"model":"qtSd/pool/prov/m","messages":[{"role":"user","content":"hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := qtSdChatContent(rec.Body.Bytes()); got != "answer-from-a" {
		t.Fatalf("served content = %q, want the DRR winner %q (a, share 10) at the concrete model", got, "answer-from-a")
	}
	// The response must carry the concrete model (not the qtSd alias).
	var chat struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chat); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if chat.Model != "prov/m" {
		t.Fatalf("response model = %q, want the unwrapped concrete model %q", chat.Model, "prov/m")
	}
}

// qtSdChatContent extracts the first choice's message content from a
// chat completions response body.
func qtSdChatContent(body []byte) string {
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return ""
	}
	if len(chat.Choices) == 0 {
		return ""
	}
	return chat.Choices[0].Message.Content
}
