package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestChatSuccess(t *testing.T) {
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

	resp, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hello there" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.Usage != (Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestChatValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for invalid requests")
	}))
	defer srv.Close()
	c := mustClient(t, srv)
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Error("empty model should fail")
	}
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m"}); err == nil {
		t.Error("no messages should fail")
	}
}

func TestChatRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
	if rlErr.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter = %v, want 5s", rlErr.RetryAfter)
	}
}

func TestChatRateLimitNoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
	if rlErr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 without header", rlErr.RetryAfter)
	}
}

func TestChatServerErrorRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if upErr.StatusCode != 500 || !upErr.Retryable {
		t.Errorf("UpstreamError = %+v, want 500 retryable", upErr)
	}
}

func TestChatQuotaError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"402", 402, `{"error":{"message":"insufficient_quota","type":"billing"}}`},
		{"403 quota body", 403, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
				Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
			})
			var qErr *QuotaError
			if !errors.As(err, &qErr) {
				t.Fatalf("error is %T, want *QuotaError: %v", err, err)
			}
			if qErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d", qErr.StatusCode)
			}
		})
	}
}

func TestChatUnauthorizedNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth"}}`))
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if upErr.StatusCode != 401 || upErr.Retryable {
		t.Errorf("UpstreamError = %+v, want 401 non-retryable", upErr)
	}
	var qErr *QuotaError
	if errors.As(err, &qErr) {
		t.Error("401 auth error must not be a QuotaError")
	}
}

func TestChatStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n"))
		_, _ = w.Write([]byte(": keep-alive comment\n\n"))
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var deltas []string
	resp, err := mustClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "Hello" {
		t.Errorf("Content = %q, want Hello", resp.Content)
	}
	if len(deltas) != 2 || deltas[0] != "Hel" || deltas[1] != "lo" {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestChatStreamMalformedSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {this is not json\n\n"))
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected error for malformed SSE data")
	}
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError: %v", err, err)
	}
	if !strings.Contains(upErr.Body, "malformed SSE data") {
		t.Errorf("Body = %q", upErr.Body)
	}
}

func TestChatStreamTruncatedNoDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Closes immediately with no data and no [DONE].
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected error for stream ending without [DONE] or content")
	}
}

func TestChatStreamServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(string) error { return nil })
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) || rlErr.RetryAfter != 2*time.Second {
		t.Fatalf("error = %v, want RateLimitError 2s", err)
	}
}

func TestChatStreamOnDeltaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	sentinel := errors.New("downstream aborted")
	_, err := mustClient(t, srv).ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	}, func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model"},{"id":"model-b","object":"model"}]}`))
	}))
	defer srv.Close()

	ids, err := mustClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "model-a" || ids[1] != "model-b" {
		t.Errorf("ids = %v", ids)
	}
}

func TestListModelsBaseURLWithV1(t *testing.T) {
	// Base URL already ending in /v1 must not become /v1/v1/models.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s, want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL+"/v1", "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
}

func TestTestKey(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer okSrv.Close()
	if err := mustClient(t, okSrv).TestKey(context.Background()); err != nil {
		t.Errorf("TestKey on 200: %v", err)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer badSrv.Close()
	if err := mustClient(t, badSrv).TestKey(context.Background()); err == nil {
		t.Error("TestKey on 401 should fail")
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "120")
	if got := parseRetryAfter(h); got != 120*time.Second {
		t.Errorf("seconds: got %v", got)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if got := parseRetryAfter(h); got <= 0 || got > 31*time.Second {
		t.Errorf("HTTP date: got %v", got)
	}
	h.Set("Retry-After", "nonsense")
	if got := parseRetryAfter(h); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
}
