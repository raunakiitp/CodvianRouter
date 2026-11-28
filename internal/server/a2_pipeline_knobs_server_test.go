// A2-7 server-level proof: the pipeline knobs are reachable from a
// real HTTP request through the real engine — the config fields are
// wired into router.Config (see cmd/codvian-router/main.go) and a
// "pipeline" request honours them. This test pins skip_under (a short
// prompt is served by a single provider, not the full multi-stage
// pipeline) end to end.
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

// pipeStaticClient returns a fixed content for every chat.
type pipeStaticClient struct{ content string }

func (c *pipeStaticClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *pipeStaticClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *pipeStaticClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *pipeStaticClient) TestKey(ctx context.Context) error                { return nil }

// pipelineServerEngine builds a real engine whose chain is [a, b] with
// PipelineSkipTokensUnder set high, so a short "pipeline" request is
// served by a single provider (a) instead of running both stages.
func pipelineServerEngine(t *testing.T) *router.Engine {
	t.Helper()
	cfg := router.Config{
		MaxAttempts:             5,
		HedgeDelay:              -1,
		BackoffBase:             time.Millisecond,
		BackoffMax:              5 * time.Millisecond,
		BreakerThreshold:        5,
		BreakerOpenFor:          time.Minute,
		RetryAfterCap:           30 * time.Second,
		PipelineSkipTokensUnder: 1000,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}
		},
	}
	return router.NewEngine(map[string]provider.Client{
		"a": &pipeStaticClient{content: "a-single"},
		"b": &pipeStaticClient{content: "b-stage"},
	}, nil, cfg)
}

// TestServerPipelineSkipTokensUnderReachable is the
// production-reachability proof for A2-7 skip_under: a real HTTP chat
// request with strategy=pipeline and a short prompt is served by the
// single provider (a), not the last pipeline stage (b). FAILS-WHEN-OFF:
// with the knob unwired the request would run both stages and serve "b".
func TestServerPipelineSkipTokensUnderReachable(t *testing.T) {
	srv := newTestServer(t, pipelineServerEngine(t))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"strategy":"pipeline"}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := pipeChatContent(rec.Body.Bytes()); got != "a-single" {
		t.Fatalf("served content = %q, want the single provider %q (short prompt skipped the pipeline)", got, "a-single")
	}
}

// pipeChatContent extracts the first choice's message content.
func pipeChatContent(body []byte) string {
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
