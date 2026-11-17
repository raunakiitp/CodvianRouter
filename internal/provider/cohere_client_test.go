package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cohereMock serves the Cohere Chat API v2 the way the real API does
// (GET /v1/models, POST /v2/chat with "stream": true/false) and
// records every request, so tests can prove the request shape and the
// auth headers.
type cohereMock struct {
	t           *testing.T
	lastPath    string
	lastHeaders http.Header
	lastBody    []byte
	// streamBody, when set, is the SSE body returned for streaming.
	streamBody string
	// status/body override for the next chat call (error cases).
	errStatus int
	errBody   string
}

func (m *cohereMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		m.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch {
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"command-a-03-2025"},{"name":"command-r-plus-08-2024"}]}`))
		case r.URL.Path == "/v2/chat":
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			var decoded struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &decoded)
			if decoded.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(m.streamBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chat-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]},"usage":{"input_tokens":12,"output_tokens":8}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newCohereMock(t *testing.T) (*httptest.Server, *cohereMock) {
	t.Helper()
	m := &cohereMock{t: t, streamBody: "data: {\"type\":\"message-start\",\"id\":\"chat-1\",\"delta\":{\"message\":{\"role\":\"assistant\"}}}\n\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"type\":\"text\",\"text\":\"hi \"}}}}\n\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"type\":\"text\",\"text\":\"there\"}}}}\n\ndata: {\"type\":\"message-end\",\"delta\":{\"finish_reason\":\"COMPLETE\",\"usage\":{\"input_tokens\":12,\"output_tokens\":8}}}\n\n"}
	return httptest.NewServer(m.handler()), m
}

func mustCohereClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, "co-key-test", WithAuthHeaders(map[string]string{
		"Authorization": "Bearer co-key-test",
	}), WithWireFormat(WireFormatCohere))
	if err != nil {
		t.Fatalf("NewClient cohere: %v", err)
	}
	return c
}

func TestCohereClientChat(t *testing.T) {
	srv, mock := newCohereMock(t)
	defer srv.Close()
	c := mustCohereClient(t, srv)

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model: "command-a-03-2025",
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi there" || resp.FinishReason != "stop" {
		t.Errorf("Content/FinishReason = %q/%q", resp.Content, resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.TotalTokens != 20 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if mock.lastPath != "/v2/chat" {
		t.Errorf("path = %q, want /v2/chat", mock.lastPath)
	}
	if got := mock.lastHeaders.Get("Authorization"); got != "Bearer co-key-test" {
		t.Errorf("Authorization = %q, want Bearer co-key-test", got)
	}
	// The request body is Cohere v2, not OpenAI wire: system content
	// is a plain string, user content is typed blocks; no
	// /v1/chat/completions shape.
	var decoded struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(mock.lastBody, &decoded); err != nil {
		t.Fatalf("request body decode: %v", err)
	}
	if decoded.Model != "command-a-03-2025" || len(decoded.Messages) != 2 || decoded.Messages[0].Role != "system" {
		t.Errorf("request body = %s", mock.lastBody)
	}
	var sys string
	if err := json.Unmarshal(decoded.Messages[0].Content, &sys); err != nil || sys != "be brief" {
		t.Errorf("system content = %s (err %v)", decoded.Messages[0].Content, err)
	}
	var userBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(decoded.Messages[1].Content, &userBlocks); err != nil {
		t.Fatalf("user content decode: %v", err)
	}
	if len(userBlocks) != 1 || userBlocks[0].Text != "hi" {
		t.Errorf("user content = %+v", userBlocks)
	}
}

func TestCohereClientChatTools(t *testing.T) {
	srv, mock := newCohereMock(t)
	defer srv.Close()
	// Override the mock's chat reply with a tool-call response by
	// replacing the handler via a second server is overkill; instead
	// point the mock at the tool-call body through errStatus == 0
	// and a custom streamBody is not used for non-streaming. Use a
	// dedicated server here.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.lastPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		mock.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-2","finish_reason":"TOOL_CALL","message":{"role":"assistant","content":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Pune\"}"}}]},"usage":{"input_tokens":20,"output_tokens":5}}`))
	}))
	defer srv2.Close()
	c := mustCohereClient(t, srv2)

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "command-a-03-2025",
		Messages: []Message{{Role: "user", Content: "weather?"}},
		Tools: []ToolDefinition{{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ToolChoice: json.RawMessage(`"required"`),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("resp = %+v", resp)
	}
	// The tool_choice went out as Cohere's REQUIRED enum.
	if !strings.Contains(string(mock.lastBody), `"tool_choice":"REQUIRED"`) {
		t.Errorf("request body = %s, want tool_choice REQUIRED", mock.lastBody)
	}
}

func TestCohereClientChatStream(t *testing.T) {
	srv, mock := newCohereMock(t)
	defer srv.Close()
	c := mustCohereClient(t, srv)
	streamer, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatalf("cohere client does not implement ChunkStreamer")
	}

	var chunks []StreamChunk
	resp, err := streamer.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "command-a-03-2025", Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "hi there" || resp.FinishReason != "stop" {
		t.Errorf("Content/FinishReason = %q/%q", resp.Content, resp.FinishReason)
	}
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want >= 3 (2 deltas + finish)", len(chunks))
	}
	if chunks[0].Content != "hi " || chunks[1].Content != "there" {
		t.Errorf("chunks = %+v", chunks)
	}
	if chunks[len(chunks)-1].FinishReason != "stop" {
		t.Errorf("final chunk = %+v", chunks[len(chunks)-1])
	}
	if !strings.Contains(string(mock.lastBody), `"stream":true`) {
		t.Errorf("request body = %s, want stream:true", mock.lastBody)
	}
}

func TestCohereClientErrors(t *testing.T) {
	srv, mock := newCohereMock(t)
	defer srv.Close()
	c := mustCohereClient(t, srv)

	// 429 -> *RateLimitError (failover trigger).
	mock.errStatus = http.StatusTooManyRequests
	mock.errBody = `{"message":"Too many requests"}`
	_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Errorf("429 err = %T %v, want *RateLimitError", err, err)
	}

	// 402 -> *QuotaError.
	mock.errStatus = http.StatusPaymentRequired
	mock.errBody = `{"message":"Payment required"}`
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Errorf("402 err = %T %v, want *QuotaError", err, err)
	}

	// 500 -> *UpstreamError, retryable.
	mock.errStatus = http.StatusInternalServerError
	mock.errBody = `{"message":"boom"}`
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var ue *UpstreamError
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Errorf("500 err = %T %v, want retryable *UpstreamError", err, err)
	}

	// Unrepresentable input (top_p 1.0) fails before any HTTP call.
	mock.errStatus = 0
	_, err = c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, TopP: 1.0})
	if !errors.As(err, &ue) || ue.Retryable {
		t.Errorf("top_p err = %T %v, want non-retryable *UpstreamError", err, err)
	}
}

func TestCohereClientListModels(t *testing.T) {
	srv, mock := newCohereMock(t)
	defer srv.Close()
	c := mustCohereClient(t, srv)

	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "command-a-03-2025" || ids[1] != "command-r-plus-08-2024" {
		t.Errorf("ids = %v", ids)
	}
	if mock.lastPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", mock.lastPath)
	}
	if err := c.TestKey(context.Background()); err != nil {
		t.Errorf("TestKey: %v", err)
	}
}

func TestCohereClientIsChunkStreamer(t *testing.T) {
	srv, _ := newCohereMock(t)
	defer srv.Close()
	c := mustCohereClient(t, srv)
	if _, ok := c.(ChunkStreamer); !ok {
		t.Errorf("cohere client does not implement ChunkStreamer")
	}
}
