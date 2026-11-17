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
	"time"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// anthropicMock serves the Anthropic Messages API the way the real
// API does (POST /v1/messages, GET /v1/models) and records every
// request, so tests can prove the request shape and the auth headers.
type anthropicMock struct {
	t           *testing.T
	lastPath    string
	lastHeaders http.Header
	lastBody    []byte
	// streamBody, when set, is the SSE body returned for stream:true.
	streamBody string
	// status/body override for the next messages call (error cases).
	errStatus int
	errBody   string
}

func (m *anthropicMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		m.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5"},{"id":"claude-haiku-4-5"}]}`))
		case "/v1/messages":
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			var decoded struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &decoded)
			w.Header().Set("Content-Type", "application/json")
			if decoded.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(m.streamBody))
				return
			}
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Pune"}}],"model":"claude-sonnet-4-5","stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":8}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newAnthropicMock(t *testing.T) (*httptest.Server, *anthropicMock) {
	t.Helper()
	m := &anthropicMock{t: t, streamBody: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":12}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi \"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Pune\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":8}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
	return httptest.NewServer(m.handler()), m
}

func mustAnthropicClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, "sk-ant-test", WithAuthHeaders(map[string]string{
		"Authorization":     "Bearer sk-ant-test",
		"anthropic-version": "2026-10-21",
	}), WithWireFormat(WireFormatAnthropic))
	if err != nil {
		t.Fatalf("NewClient anthropic: %v", err)
	}
	return c
}

func TestAnthropicChatRequestShape(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	resp, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if mock.lastPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", mock.lastPath)
	}
	if got := mock.lastHeaders.Get("Authorization"); got != "Bearer sk-ant-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := mock.lastHeaders.Get("anthropic-version"); got != "2026-10-21" {
		t.Errorf("anthropic-version = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["model"] != "claude-sonnet-4-5" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096 (Anthropic requires it)", body["max_tokens"])
	}
	if body["system"] != "Be helpful." {
		t.Errorf("system = %v, want the system message extracted", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want exactly the user message", body["messages"])
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want hi there", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls (Anthropic tool_use)", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("ToolCalls = %+v, want one get_weather call", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCall arguments = %q", resp.ToolCalls[0].Function.Arguments)
	}
	if resp.Usage != (Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestAnthropicChatStreamChunks(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	var chunks []StreamChunk
	cs, ok := mustAnthropicClient(t, srv).(ChunkStreamer)
	if !ok {
		t.Fatal("anthropic client must implement ChunkStreamer")
	}
	resp, err := cs.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if mock.lastPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", mock.lastPath)
	}
	var body map[string]any
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	if resp.Content != "hi there" {
		t.Errorf("reassembled Content = %q, want hi there", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("reassembled ToolCalls = %+v", resp.ToolCalls)
	}
	// The chunks must carry the text deltas, the tool-call argument
	// fragments, and the finish reason — not just text.
	var text, args, finish string
	for _, c := range chunks {
		text += c.Content
		for _, tc := range c.ToolCalls {
			args += tc.Arguments
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	if text != "hi there" {
		t.Errorf("chunk text = %q", text)
	}
	if args != `{"city":"Pune"}` {
		t.Errorf("chunk tool-call args = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("chunk finish reason = %q", finish)
	}
}

func TestAnthropicChatStreamTextOnly(t *testing.T) {
	srv, _ := newAnthropicMock(t)
	defer srv.Close()

	var deltas []string
	resp, err := mustAnthropicClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if strings.Join(deltas, "") != "hi there" {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Content != "hi there" || resp.FinishReason != "tool_calls" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestAnthropicChatRateLimit(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	mock.errStatus = http.StatusTooManyRequests
	mock.errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`

	_, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
}

func TestAnthropicChatRateLimitRetryAfter(t *testing.T) {
	// Retry-After is honoured via the header, like the OpenAI client.
	h := http.Header{}
	h.Set("Retry-After", "7")
	err := mapAnthropicError(wire.MapError(http.StatusTooManyRequests, h, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)))
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
	if rlErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", rlErr.RetryAfter)
	}
}

func TestAnthropicChatOverloaded(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	mock.errStatus = 529
	mock.errBody = `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`

	_, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if !upErr.Retryable {
		t.Errorf("overloaded_error must be retryable (failover trigger)")
	}
	if upErr.StatusCode != 529 {
		t.Errorf("StatusCode = %d, want 529", upErr.StatusCode)
	}
}

func TestAnthropicChatBillingIsQuota(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	mock.errStatus = http.StatusBadRequest
	mock.errBody = `{"type":"error","error":{"type":"billing_error","message":"insufficient credits"}}`

	_, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var qErr *QuotaError
	if !errors.As(err, &qErr) {
		t.Fatalf("error is %T, want *QuotaError: %v", err, err)
	}
}

func TestAnthropicChatUnrepresentable(t *testing.T) {
	srv, _ := newAnthropicMock(t)
	defer srv.Close()

	// Temperature above Anthropic's 0-1 range cannot be represented.
	_, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Temperature: 1.5,
	})
	if err == nil {
		t.Fatal("temperature 1.5 must fail on the Anthropic wire")
	}
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if upErr.Retryable {
		t.Errorf("unrepresentable input must NOT be retryable on the same provider")
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	mock.streamBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n"

	_, err := mustAnthropicClient(t, srv).(ChunkStreamer).ChatStreamChunks(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(StreamChunk) error { return nil })
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
}

func TestAnthropicListModels(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	ids, err := mustAnthropicClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "claude-sonnet-4-5" {
		t.Errorf("models = %v", ids)
	}
	if mock.lastPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", mock.lastPath)
	}
}

func TestNewClientWireFallback(t *testing.T) {
	// An unimplemented wire format falls back to the OpenAI client
	// instead of breaking ClientFor for providers added before
	// native transports existed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "k", WithWireFormat("gemini"))
	if err != nil {
		t.Fatalf("NewClient gemini: %v", err)
	}
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
}

func TestMessageJSONRoundTrip(t *testing.T) {
	// Content as an array of parts survives a decode/encode round
	// trip, and tool calls ride along on assistant messages.
	raw := `{"role":"assistant","content":[{"type":"text","text":"hi"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Pune\"}"}}]}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m.Content != "hi" || len(m.Parts) != 1 {
		t.Errorf("decoded = %+v", m)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("ToolCalls = %+v", m.ToolCalls)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m2 Message
	if err := json.Unmarshal(out, &m2); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if m2.Content != "hi" || len(m2.ToolCalls) != 1 {
		t.Errorf("round trip = %+v", m2)
	}
	// Plain string content stays a string.
	var m3 Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &m3); err != nil {
		t.Fatalf("Unmarshal string content: %v", err)
	}
	if m3.Content != "hello" || len(m3.Parts) != 0 {
		t.Errorf("string content = %+v", m3)
	}
}

func TestChatRequestStopForms(t *testing.T) {
	var r ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"stop":"END"}`), &r); err != nil {
		t.Fatalf("Unmarshal stop string: %v", err)
	}
	if len(r.Stop) != 1 || r.Stop[0] != "END" {
		t.Errorf("Stop = %v, want [END]", r.Stop)
	}
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"stop":["A","B"]}`), &r); err != nil {
		t.Fatalf("Unmarshal stop array: %v", err)
	}
	if len(r.Stop) != 2 {
		t.Errorf("Stop = %v, want [A B]", r.Stop)
	}
}
