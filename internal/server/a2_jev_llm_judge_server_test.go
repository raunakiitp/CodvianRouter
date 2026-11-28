// A2-4 server-level proof: the JEV LLM judge flag is reachable from a
// real HTTP request through the real engine — not only an engine unit
// test. A chat request carrying strategy=jev on a real router.Engine
// (with the jev judge flag on) makes the judge call exactly once and
// serves the judge's choice; with the flag off the judge is never
// called and the heuristic winner is served.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// staticClient returns a fixed content for every chat.
type staticClient struct{ content string }

func (c *staticClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *staticClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content}, nil
}
func (c *staticClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *staticClient) TestKey(ctx context.Context) error                { return nil }

// srvJevJudge is a fake judge model that counts calls and answers the
// JEV System One prompt with a fixed choice+probabilities line.
type srvJevJudge struct {
	mu    sync.Mutex
	chats int
}

func (c *srvJevJudge) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.chats++
	c.mu.Unlock()
	// Judge picks candidate 1 (provider "a", the short answer) with
	// high confidence, so its choice is admitted and served.
	return &provider.ChatResponse{Content: "WINNER: 1 CONFIDENCE: 0.8 PROBS: 0.8 0.2"}, nil
}
func (c *srvJevJudge) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return c.Chat(ctx, req)
}
func (c *srvJevJudge) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *srvJevJudge) TestKey(ctx context.Context) error                { return nil }

func (c *srvJevJudge) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats
}

// jevServerEngine builds a real engine over a [a,b] panel plus a
// dedicated judge provider, with the JEV LLM judge flag on or off.
func jevServerEngine(t *testing.T, judge *srvJevJudge, enabled bool) *router.Engine {
	t.Helper()
	cfg := router.Config{
		MaxAttempts:             5,
		HedgeDelay:              -1,
		BackoffBase:             time.Millisecond,
		BackoffMax:              5 * time.Millisecond,
		BreakerThreshold:        5,
		BreakerOpenFor:          time.Minute,
		RetryAfterCap:           30 * time.Second,
		ComboJevLLMJudge:        enabled,
		JevJudgeModel:           func(model string) string { return "judge-model" },
		ComboPanelHardTimeoutMs: 5000,
		ChainFor: func(model string) []router.Attempt {
			if model == "judge-model" {
				return []router.Attempt{{ProviderID: "judge", Model: model}}
			}
			return []router.Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}
		},
	}
	return router.NewEngine(map[string]provider.Client{
		"a":     &staticClient{content: "short"},
		"b":     &staticClient{content: "a much longer panel answer that the heuristic judge would prefer over a"},
		"judge": judge,
	}, nil, cfg)
}

// TestServerJevLLMJudgeReachable is the production-reachability proof
// for A2-4: a real HTTP chat request with strategy=jev on a real
// engine routes through the JEV LLM judge when the flag is on — the
// judge is called exactly once and the served answer is the judge's
// choice ("short", the heuristic loser). FAILS-WHEN-OFF: with the
// flag wiring removed the judge is never called and the served
// answer is the heuristic winner.
func TestServerJevLLMJudgeReachable(t *testing.T) {
	judge := &srvJevJudge{}
	srv := newTestServer(t, jevServerEngine(t, judge, true))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"strategy":"jev"}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if judge.calls() != 1 {
		t.Fatalf("judge calls = %d, want 1 (the JEV System One call from a real HTTP request)", judge.calls())
	}
	if got := chatContent(rec.Body.Bytes()); got != "short" {
		t.Fatalf("served content = %q, want the judge's choice %q (not the heuristic winner)", got, "short")
	}
}

// TestServerJevLLMJudgeOffIsHeuristic: with the flag off, the same
// HTTP request never calls the judge and serves the heuristic winner
// ("b", the longer answer). FAILS-WHEN-ON: forcing the flag on would
// call the judge and serve "a".
func TestServerJevLLMJudgeOffIsHeuristic(t *testing.T) {
	judge := &srvJevJudge{}
	srv := newTestServer(t, jevServerEngine(t, judge, false))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"strategy":"jev"}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if judge.calls() != 0 {
		t.Fatalf("judge calls = %d, want 0 (flag off: no judge call)", judge.calls())
	}
	if got := chatContent(rec.Body.Bytes()); got != "a much longer panel answer that the heuristic judge would prefer over a" {
		t.Fatalf("served content = %q, want the heuristic winner verbatim (flag off)", got)
	}
}

// chatContent extracts the first choice's message content from a chat
// completions response body.
func chatContent(body []byte) string {
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
