package server_test

// Regression test for the Anthropic native-wire integration: in serve
// mode the engine is wrapped by degrade.Wrap, and the wrapper used
// to lack ChatStreamChunks — so the server silently fell back to
// text-only streaming and tool-call deltas never reached the client.
// This file is package server_test (external) so it can import both
// server and degrade without an import cycle.

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// extChunkEngine is a minimal chunk-aware engine for this test.
type extChunkEngine struct {
	resp   provider.ChatResponse
	chunks []provider.StreamChunk
}

func (f *extChunkEngine) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	resp := f.resp
	return &resp, nil
}

func (f *extChunkEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	for _, c := range f.chunks {
		if c.Content != "" {
			if err := onDelta(c.Content); err != nil {
				return nil, err
			}
		}
	}
	resp := f.resp
	return &resp, nil
}

func (f *extChunkEngine) ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	return f.ChatStream(ctx, req, onDelta)
}

func (f *extChunkEngine) ChatStreamChunks(_ context.Context, _ provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	for _, c := range f.chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	resp := f.resp
	return &resp, nil
}

func (f *extChunkEngine) ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	return f.ChatStreamChunks(ctx, req, onChunk)
}

func TestChatCompletionsStreamingChunkAwareThroughStaleWrapper(t *testing.T) {
	toolCalls := []provider.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: wire.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`},
	}}
	eng := degrade.Wrap(&extChunkEngine{
		resp: provider.ChatResponse{
			Content:      "hi ",
			FinishReason: "tool_calls",
			ToolCalls:    toolCalls,
		},
		chunks: []provider.StreamChunk{
			{Content: "hi "},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather"}}},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, Arguments: `{"city":"Pune"}`}}},
			{FinishReason: "tool_calls"},
		},
	}, cache.NewExactCache(10), degrade.DefaultPolicy())
	srv := server.New(eng, server.WithStaticKeys("test-key-123"), server.WithLogger(log.New(io.Discard, "", 0)))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls":[{"index":0,"id":"call_1","name":"get_weather"}]`) {
		t.Errorf("SSE body has no tool-call head delta:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("SSE body has no tool_calls finish_reason:\n%s", body)
	}
}
