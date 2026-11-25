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
)

func mustClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
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

// TestChatTargetPropertyErrorsRetryable: the shared taxonomy
// (research-log entry 183) — 404 (that target lacks the model) and
// 529 (overload) are retryable on another target, while 405 stays
// non-retryable on chat completions (it is only retryable on
// /v1/responses and /v1/embeddings, which this client never calls).
func TestChatTargetPropertyErrorsRetryable(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{404, true},
		{529, true},
		{405, false},
		{501, false},
		{400, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		_, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
			Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
		})
		srv.Close()
		var upErr *UpstreamError
		if !errors.As(err, &upErr) {
			t.Fatalf("status %d: error is %T, want *UpstreamError: %v", tc.status, err, err)
		}
		if upErr.StatusCode != tc.status || upErr.Retryable != tc.retryable {
			t.Errorf("status %d: UpstreamError = %+v, want retryable=%v", tc.status, upErr, tc.retryable)
		}
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

	c, err := NewClient(srv.URL+"/v1", "k", WithAllowPrivateDestinations(true))
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
	if got, present := parseRetryAfter(h); got != 120*time.Second || !present {
		t.Errorf("seconds: got %v, present %v", got, present)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if got, present := parseRetryAfter(h); got <= 0 || got > 31*time.Second || !present {
		t.Errorf("HTTP date: got %v, present %v", got, present)
	}
	h.Set("Retry-After", "nonsense")
	if got, present := parseRetryAfter(h); got != 0 || present {
		t.Errorf("garbage: got %v, present %v", got, present)
	}
	// Presence matters (CHECKLIST 167-168): an explicit "0" is a
	// poisoned hint the router floors to 10s, while an absent
	// header means "no hint" and keeps exponential backoff — the
	// two must never collapse into the same 0.
	h.Set("Retry-After", "0")
	if got, present := parseRetryAfter(h); got != 0 || !present {
		t.Errorf("explicit 0: got %v, present %v, want 0, true", got, present)
	}
	h.Del("Retry-After")
	if got, present := parseRetryAfter(h); got != 0 || present {
		t.Errorf("absent: got %v, present %v, want 0, false", got, present)
	}
}

// TestMapStatusErrorRedactsKeyInBody (slice 5, entries 150/152)
// proves an upstream error body that quotes a provider key is
// redacted at construction, before the error can reach any log
// line, trace, or error string. The fixture key is synthetic.
func TestMapStatusError429BodyRetryAfter(t *testing.T) {
	// M4.4: a 429 with no Retry-After header falls back to the hint
	// in the error body (Gemini RetryInfo here); the header always
	// wins when present.
	body := []byte(`{"error":{"code":429,"message":"Resource exhausted","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`)
	err := mapStatusError(http.StatusTooManyRequests, nil, body)
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("error is %T, want *RateLimitError", err)
	}
	if !rle.RetryAfterPresent || rle.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v present = %v, want 30s present", rle.RetryAfter, rle.RetryAfterPresent)
	}
	h := http.Header{}
	h.Set("Retry-After", "3")
	err = mapStatusError(http.StatusTooManyRequests, h, body)
	rle, ok = err.(*RateLimitError)
	if !ok || rle.RetryAfter != 3*time.Second {
		t.Fatalf("header must win: RetryAfter = %v, want 3s", rle.RetryAfter)
	}
	// "retry in Xs" free text is a fallback too.
	err = mapStatusError(http.StatusTooManyRequests, nil, []byte(`{"error":{"message":"slow down, retry in 45s"}}`))
	rle, ok = err.(*RateLimitError)
	if !ok || !rle.RetryAfterPresent || rle.RetryAfter != 45*time.Second {
		t.Fatalf("free-text hint: RetryAfter = %v present = %v, want 45s present", rle.RetryAfter, rle.RetryAfterPresent)
	}
	// No hint anywhere: absent, never a guessed wait.
	err = mapStatusError(http.StatusTooManyRequests, nil, []byte(`{"error":{"message":"slow down"}}`))
	rle, ok = err.(*RateLimitError)
	if !ok || rle.RetryAfterPresent {
		t.Fatalf("no hint: RetryAfterPresent = %v, want false", rle.RetryAfterPresent)
	}
}

func TestMapStatusErrorRedactsKeyInBody(t *testing.T) {
	key := "sk-proj-TESTKEY0123456789abcdefghij"
	body := []byte(`{"error":{"message":"bad key ` + key + `","type":"invalid_request_error"}}`)
	err := mapStatusError(http.StatusBadRequest, nil, body)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("upstream error leaks the key: %q", err.Error())
	}
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("error is %T, want *UpstreamError", err)
	}
	if strings.Contains(upErr.Body, key) {
		t.Fatalf("UpstreamError.Body leaks the key: %q", upErr.Body)
	}
	// A labelled key in a quota body is redacted the same way.
	qbody := []byte(`{"error":{"message":"insufficient_quota for api_key=` + key + `"}}`)
	qerr := mapStatusError(http.StatusPaymentRequired, nil, qbody)
	if strings.Contains(qerr.Error(), key) {
		t.Fatalf("quota error leaks the key: %q", qerr.Error())
	}
}

// TestChatReasoningEffortForwarded (CHECKLIST 255-256): the
// client-requested reasoning effort reaches the OpenAI wire body
// verbatim — the effort a group entry or the client asked for is
// what the upstream is asked to run at.
func TestChatReasoningEffortForwarded(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	if _, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:           "gpt-4o-mini",
		Messages:        []Message{{Role: "user", Content: "Hi"}},
		ReasoningEffort: "low",
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("wire body is not JSON: %v", err)
	}
	if wire["reasoning_effort"] != "low" {
		t.Errorf("wire reasoning_effort = %v, want low", wire["reasoning_effort"])
	}
}

// TestChatNoReasoningEffortOmitted: no explicit effort means no
// reasoning_effort field on the wire at all — the upstream's
// default applies, and no empty value is ever sent.
func TestChatNoReasoningEffortOmitted(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	if _, err := mustClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("wire body contains reasoning_effort with no effort requested: %s", body)
	}
}

// TestValidReasoningEffort: the level set is exactly the six
// levels the group syntax and the API accept — anything else is
// rejected at validation, never silently normalised.
func TestValidReasoningEffort(t *testing.T) {
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if !ValidReasoningEffort(level) {
			t.Errorf("ValidReasoningEffort(%q) = false, want true", level)
		}
		if NormalizeReasoningEffort("  "+strings.ToUpper(level)+" ") != level {
			t.Errorf("NormalizeReasoningEffort(%q) != %q", level, level)
		}
	}
	for _, bad := range []string{"", "bogus", "med", "low-high"} {
		if ValidReasoningEffort(bad) {
			t.Errorf("ValidReasoningEffort(%q) = true, want false", bad)
		}
	}
	// Whitespace and case are trimmed/normalised, not rejected.
	if !ValidReasoningEffort("high ") || NormalizeReasoningEffort("high ") != "high" {
		t.Errorf("ValidReasoningEffort(\"high \") / Normalize failed")
	}
	if NormalizeReasoningEffort("LOW") != "low" {
		t.Errorf("NormalizeReasoningEffort(LOW) = %q, want low", NormalizeReasoningEffort("LOW"))
	}
}
