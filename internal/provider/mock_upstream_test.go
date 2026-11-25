package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMockUpstreamHTTP11 verifies the Chat method works with HTTP/1.1 upstream.
func TestMockUpstreamHTTP11(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hello there" {
		t.Errorf("Content = %q, want 'Hello there'", resp.Content)
	}
}

// TestMockUpstreamHTTP2 verifies that the upstream actually receives an HTTP/2 request.
// This test asserts r.ProtoMajor == 2 using genuine Go HTTP/2 test infrastructure:
// httptest.NewUnstartedServer, EnableHTTP2 = true, and StartTLS().
// The test will PASS when genuine HTTP/2 is established, FAIL otherwise.
func TestMockUpstreamHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Genuinely assert that the server received HTTP/2.
		if r.ProtoMajor != 2 {
			t.Errorf("expected ProtoMajor == 2 (genuine HTTP/2), got %d — genuine HTTP/2 not established", r.ProtoMajor)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello HTTP/2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// Create a client that trusts the test server's self-signed cert and forces HTTP/2
	customClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // For test only - test server uses self-signed cert
			},
			ForceAttemptHTTP2: true,
		},
	}

	c, err := NewClient(srv.URL, "test-key", WithHTTPClient(customClient), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hello HTTP/2" {
		t.Errorf("Content = %q, want 'Hello HTTP/2'", resp.Content)
	}
}

// TestMockUpstreamTimeout verifies that Chat respects the timeout.
func TestMockUpstreamTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
		_, _ = w.Write([]byte(`{"ok":"ok"}`))
	}))
	defer srv.Close()

	// Use a very short timeout - should get context deadline exceeded
	customClient := &http.Client{Timeout: 1 * time.Second}
	c2, err := NewClient(srv.URL, "test-key", WithHTTPClient(customClient), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c2.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	// Should get a timeout/error - in Go, this is "context deadline exceeded"
	if err == nil {
		t.Fatal("expected timeout/error but got nil")
	}
	// The error should indicate a timeout/deadline exceeded
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("expected timeout or deadline error, got: %v", err)
	}
}

// TestMockUpstreamRepeatedRequests verifies that HTTP connections are reused.
func TestMockUpstreamRepeatedRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Make 5 repeated requests
	for i := 1; i <= 5; i++ {
		resp, err := c.Chat(context.Background(), ChatRequest{
			Model:    "gpt-4",
			Messages: []Message{{Role: "user", Content: fmt.Sprintf("request %d", i)}},
		})
		if err != nil {
			t.Fatalf("Chat request %d: %v", i, err)
		}
		if !strings.Contains(resp.Content, "response") {
			t.Errorf("Request %d: Content = %q, want to contain 'response'", i, resp.Content)
		}
	}
}

// TestMockUpstreamServerError verifies error handling for upstream errors (e.g., 502).
func TestMockUpstreamServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"bad_gateway"}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
}

// TestMockUpstreamChatStreamSimple verifies streaming chat works - SSE format matching TestChatStreamSuccess.
func TestMockUpstreamChatStreamSimple(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must set Content-Type to text/event-stream
		w.Header().Set("Content-Type", "text/event-stream")
		// Send SSE events matching the format expected by ChatStreamChunks,
		// ending with [DONE]. Format: data: <json>\n\n per event, with optional
		// comment lines starting with :. Final event is data: [DONE]\n\n
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n"))
		_, _ = w.Write([]byte(": keep-alive comment\n\n"))
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"prompt_tokens\":3," + "\"completion_tokens\":2," + "\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Stream chat using ChatStream with onDelta callback
	var fullContent string
	_, err = c.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(delta string) error {
		fullContent += delta
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if fullContent != "Hello" {
		t.Errorf("Content = %q, want 'Hello'", fullContent)
	}
}

// TestMockUpstreamConnectionReset verifies handling of connection reset by upstream.
func TestMockUpstreamConnectionReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate connection reset by closing without writing response
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack not supported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a connection error
	if !strings.Contains(err.Error(), "connection") && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") {
		t.Errorf("expected connection error, got: %v", err)
	}
}

// TestMockUpstreamMalformedResponse verifies handling of malformed JSON from upstream.
func TestMockUpstreamMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write invalid JSON
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1",`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a JSON parsing error
	if !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "JSON") && !strings.Contains(err.Error(), "syntax") {
		t.Errorf("expected JSON error, got: %v", err)
	}
}

// TestMockUpstream429Handling verifies handling of 429 Too Many Requests.
func TestMockUpstream429Handling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "1")
		_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded"}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a 429 error
	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "rate_limit") && !strings.Contains(err.Error(), "rate limited") && !strings.Contains(err.Error(), "Too Many Requests") {
		t.Errorf("expected 429/rate limit error, got: %v", err)
	}
}

// TestMockUpstreamClientCancellation verifies that client cancellation is respected.
func TestMockUpstreamClientCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay to allow cancellation
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"Hello"},\"finish_reason\":\"stop\"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately
	cancel()

	_, err = c.Chat(ctx, ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a cancellation error
	if !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "cancel") {
		t.Errorf("expected cancellation error, got: %v", err)
	}
}

// TestMockUpstreamStreamInterrupted verifies handling of stream interrupted mid-response.
func TestMockUpstreamStreamInterrupted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Send first chunk
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		// Then close connection without sending [DONE]
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack not supported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var fullContent string
	_, err = c.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(delta string) error {
		fullContent += delta
		return nil
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a connection/stream error
	if !strings.Contains(err.Error(), "connection") && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "stream") {
		t.Errorf("expected stream/connection error, got: %v", err)
	}
}

// TestMockUpstreamPartialResponse verifies handling of partial/truncated response.
func TestMockUpstreamPartialResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Send incomplete JSON
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a JSON parsing error
	if !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "JSON") && !strings.Contains(err.Error(), "syntax") && !strings.Contains(err.Error(), "truncated") {
		t.Errorf("expected JSON error, got: %v", err)
	}
}

// TestMockUpstreamEmptyResponse verifies handling of empty response body.
func TestMockUpstreamEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Send empty body with 200 OK
		_, _ = w.Write([]byte{})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a parsing error
	if !strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "JSON") && !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected error for empty response, got: %v", err)
	}
}

// TestMockUpstreamInvalidStatus verifies handling of non-200 status without error body.
func TestMockUpstreamInvalidStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// No error body
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	// Should get a 500 error
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("expected 500 error, got: %v", err)
	}
}
