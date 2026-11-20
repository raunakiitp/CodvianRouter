package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// M5.13: upstream rate-limit headers surface on the response (and
// on the 429 error) so the router can learn from them.
func TestChatResponseCarriesRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit-requests", "5000")
		w.Header().Set("x-ratelimit-remaining-requests", "4990")
		w.Header().Set("x-ratelimit-limit-tokens", "200000")
		w.Header().Set("x-ratelimit-remaining-tokens", "150000")
		w.Header().Set("x-ratelimit-reset-tokens", "90")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	resp, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "gpt-4o-mini", Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.RateLimit == nil {
		t.Fatal("RateLimit is nil — the response headers were not parsed onto the response")
	}
	sig := resp.RateLimit
	if !sig.HasRequests || sig.RequestsLimit != 5000 || sig.RequestsRemaining != 4990 {
		t.Errorf("requests = %+v, want 5000/4990", sig)
	}
	if !sig.HasTokens || sig.TokensLimit != 200000 || sig.TokensRemaining != 150000 {
		t.Errorf("tokens = %+v, want 200000/150000", sig)
	}
	if sig.TokensResetAt == nil || sig.TokensResetAt.Before(time.Now().Add(80*time.Second)) {
		t.Errorf("tokens reset = %v, want ~now+90s", sig.TokensResetAt)
	}
}

func TestChatResponseWithoutRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	resp, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "gpt-4o-mini", Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.RateLimit == nil {
		t.Fatal("RateLimit is nil — the parser must always attach a (possibly empty) signal")
	}
	if !resp.RateLimit.Empty() {
		t.Errorf("RateLimit = %+v, want empty when the upstream sends no headers", resp.RateLimit)
	}
}

func TestChatRateLimitErrorCarriesSignals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	}))
	defer srv.Close()

	_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("error is %T, want *RateLimitError: %v", err, err)
	}
	if rlErr.Signals == nil || !rlErr.Signals.HasRequests || rlErr.Signals.RequestsRemaining != 0 {
		t.Errorf("429 signals = %+v, want requests 100/0", rlErr.Signals)
	}
}
