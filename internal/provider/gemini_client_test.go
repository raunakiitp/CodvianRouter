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

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// geminiMock serves the Gemini API the way the real API does
// (GET /models, POST /models/{model}:generateContent and
// :streamGenerateContent?alt=sse) and records every request, so
// tests can prove the request shape and the auth headers.
type geminiMock struct {
	t           *testing.T
	lastPath    string
	lastQuery   string
	lastHeaders http.Header
	lastBody    []byte
	// streamBody, when set, is the SSE body returned for streaming.
	streamBody string
	// status/body override for the next generate call (error cases).
	errStatus int
	errBody   string
}

func (m *geminiMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		m.lastQuery = r.URL.RawQuery
		m.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch {
		case r.URL.Path == "/models" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-1.5-pro"}]}`))
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"},{"functionCall":{"name":"get_weather","args":{"city":"Pune"}}}]},"finishReason":"TOOL_CALLS"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20},"modelVersion":"gemini-2.0-flash-001"}`))
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(m.streamBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newGeminiMock(t *testing.T) (*httptest.Server, *geminiMock) {
	t.Helper()
	m := &geminiMock{t: t, streamBody: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi \"}]},\"finishReason\":\"STOP\"}]}\n\ndata: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"there\"},{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Pune\"}}}]},\"finishReason\":\"TOOL_CALLS\"}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":8,\"totalTokenCount\":20}}\n\n"}
	return httptest.NewServer(m.handler()), m
}

func mustGeminiClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, "gem-key-test", WithAuthHeaders(map[string]string{
		"x-goog-api-key": "gem-key-test",
	}), WithWireFormat(WireFormatGemini))
	if err != nil {
		t.Fatalf("NewClient gemini: %v", err)
	}
	return c
}

func TestGeminiChatRequestShape(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()

	resp, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "gemini-2.0-flash",
		Messages: []Message{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if mock.lastPath != "/models/gemini-2.0-flash:generateContent" {
		t.Errorf("path = %q, want /models/gemini-2.0-flash:generateContent", mock.lastPath)
	}
	if got := mock.lastHeaders.Get("x-goog-api-key"); got != "gem-key-test" {
		t.Errorf("x-goog-api-key = %q", got)
	}
	if _, ok := mock.lastHeaders["Authorization"]; ok {
		t.Errorf("Gemini must not send an Authorization header")
	}
	var body map[string]any
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	sys, _ := body["systemInstruction"].(map[string]any)
	parts, _ := sys["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("systemInstruction = %v, want one part", body["systemInstruction"])
	}
	if parts[0].(map[string]any)["text"] != "Be helpful." {
		t.Errorf("systemInstruction text = %v", parts[0])
	}
	contents, _ := body["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want exactly the user content", body["contents"])
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want hi there", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls (Gemini TOOL_CALLS)", resp.FinishReason)
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

func TestGeminiChatModelPrefixStripped(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()

	_, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "models/gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if mock.lastPath != "/models/gemini-2.0-flash:generateContent" {
		t.Errorf("path = %q, want the models/ prefix stripped", mock.lastPath)
	}
}

func TestGeminiChatStreamChunks(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()

	var chunks []StreamChunk
	cs, ok := mustGeminiClient(t, srv).(ChunkStreamer)
	if !ok {
		t.Fatal("gemini client must implement ChunkStreamer")
	}
	resp, err := cs.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if mock.lastPath != "/models/gemini-2.0-flash:streamGenerateContent" {
		t.Errorf("path = %q, want :streamGenerateContent", mock.lastPath)
	}
	if mock.lastQuery != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", mock.lastQuery)
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

func TestGeminiChatStreamTextOnly(t *testing.T) {
	srv, _ := newGeminiMock(t)
	defer srv.Close()

	var deltas []string
	resp, err := mustGeminiClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}},
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

func TestGeminiChatRateLimit(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	mock.errStatus = http.StatusTooManyRequests
	mock.errBody = `{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`

	_, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
}

func TestGeminiChatUnavailableIsRetryable(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	mock.errStatus = http.StatusServiceUnavailable
	mock.errBody = `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`

	_, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if !upErr.Retryable {
		t.Errorf("UNAVAILABLE must be retryable (failover trigger)")
	}
	if upErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", upErr.StatusCode)
	}
}

func TestGeminiChatQuotaIsQuota(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	mock.errStatus = http.StatusForbidden
	mock.errBody = `{"error":{"code":403,"message":"quota exceeded for this project","status":"PERMISSION_DENIED"}}`

	_, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var qErr *QuotaError
	if !errors.As(err, &qErr) {
		t.Fatalf("error is %T, want *QuotaError: %v", err, err)
	}
}

func TestGeminiChatUnrepresentable(t *testing.T) {
	srv, _ := newGeminiMock(t)
	defer srv.Close()

	// An http(s) image URL cannot be represented on the Gemini wire.
	partsJSON, err := json.Marshal([]ContentPart{
		{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/pic.png"}},
	})
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	var msg Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":`+string(partsJSON)+`}`), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	_, err = mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{msg},
	})
	if err == nil {
		t.Fatal("an http(s) image URL must fail on the Gemini wire")
	}
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if upErr.Retryable {
		t.Errorf("unrepresentable input must NOT be retryable on the same provider")
	}
}

func TestGeminiStreamErrorEvent(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	mock.streamBody = "data: {\"error\":{\"code\":429,\"message\":\"slow down\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n"

	_, err := mustGeminiClient(t, srv).(ChunkStreamer).ChatStreamChunks(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(StreamChunk) error { return nil })
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
}

func TestGeminiListModels(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()

	ids, err := mustGeminiClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "gemini-2.0-flash" || ids[1] != "gemini-1.5-pro" {
		t.Errorf("models = %v, want the models/ prefix stripped", ids)
	}
	if mock.lastPath != "/models" {
		t.Errorf("path = %q, want /models", mock.lastPath)
	}
}
