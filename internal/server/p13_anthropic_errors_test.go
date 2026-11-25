// AUD-REL-001 / AUD-API-001 regression tests.
//
// REL-001: an upstream 429 or quota rejection used to collapse into the
// generic 502 with Retry-After dropped, so a client could not back off.
// API-001: /v1/messages answered non-streaming errors with the OpenAI
// shape, which an Anthropic SDK cannot parse.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// p13MessagesEngineServer serves /v1/messages through the ENGINE path
// (guardrails blocking forces the canonical form), with the engine
// answering chatErr — so the upstream error type reaches the handler.
func p13MessagesEngineServer(t *testing.T, chatErr error) *Server {
	t.Helper()
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	eng := &fakeEngine{chatErr: chatErr}
	srv := newMessagesServer(t, eng, mock, WithGuardrails(guardrails.NewStore()))
	srv.guardrails.SetConfig(guardrails.Config{BlockPII: true})
	return srv
}

const p13AnthropicBody = `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Hi","cache_control":{"type":"ephemeral"}}]}]}`

var p13AnthropicErrorTypes = map[string]bool{
	"invalid_request_error": true, "authentication_error": true, "permission_error": true,
	"not_found_error": true, "rate_limit_error": true, "api_error": true, "overloaded_error": true,
}

// p13DecodeAnthropicError asserts the Anthropic envelope and returns the
// inner error object.
func p13DecodeAnthropicError(t *testing.T, status int, body string) map[string]any {
	t.Helper()
	var env struct {
		Type  string         `json:"type"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("status %d: body is not JSON (%v): %s", status, err, body)
	}
	if env.Type != "error" {
		t.Fatalf("status %d: top-level type = %q, want \"error\" (Anthropic envelope): %s", status, env.Type, body)
	}
	if env.Error == nil {
		t.Fatalf("status %d: no error object: %s", status, body)
	}
	et, _ := env.Error["type"].(string)
	if !p13AnthropicErrorTypes[et] {
		t.Fatalf("status %d: error.type = %q, not an Anthropic error type: %s", status, et, body)
	}
	if _, ok := env.Error["code"]; ok {
		t.Errorf("status %d: OpenAI-shaped error.code present: %s", status, body)
	}
	if msg, _ := env.Error["message"].(string); msg == "" {
		t.Errorf("status %d: empty error message: %s", status, body)
	}
	return env.Error
}

// TestAUD_API_001ValidationErrorsUseAnthropicEnvelope.
func TestAUD_API_001ValidationErrorsUseAnthropicEnvelope(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing model", `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"invalid json", `{not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, srv, http.MethodPost, "/v1/messages", tc.body, "test-key-123")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
		})
	}
}

// TestAUD_API_001RoutingHeaderErrorUsesAnthropicEnvelope covers the
// routing-contract guard, which answers 400 before any provider call.
func TestAUD_API_001RoutingHeaderErrorUsesAnthropicEnvelope(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(p13AnthropicBody))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Codvian-Routing", "cost")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
}

// TestAUD_API_001AuthFailureUsesAnthropicEnvelope: the auth choke point
// must speak Anthropic on the Anthropic surface.
func TestAUD_API_001AuthFailureUsesAnthropicEnvelope(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status %d, want 401", rec.Code)
	}
	p13DecodeAnthropicError(t, rec.Code, rec.Body.String())

	rec = doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "wrong-key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status %d, want 401", rec.Code)
	}
	p13DecodeAnthropicError(t, rec.Code, rec.Body.String())

	// The OpenAI surface keeps its own (unchanged) shape.
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"m"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chat missing key: status %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("chat 401 took the Anthropic envelope: %s", rec.Body.String())
	}
}

// TestAUD_REL_001UpstreamRateLimitMapsTo429WithRetryAfter.
func TestAUD_REL_001UpstreamRateLimitMapsTo429WithRetryAfter(t *testing.T) {
	srv := p13MessagesEngineServer(t, &provider.RateLimitError{
		RetryAfter: 30 * time.Second, RetryAfterPresent: true, Body: "slow down",
	})
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (was the generic 502 before the fix): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want \"30\" (the upstream hint used to be dropped)", got)
	}
	inner := p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
	if inner["type"] != "rate_limit_error" {
		t.Errorf("error.type = %v, want rate_limit_error", inner["type"])
	}
}

// TestAUD_REL_001RateLimitWithoutHintSendsNoRetryAfter: an absent hint
// must not invent one.
func TestAUD_REL_001RateLimitWithoutHintSendsNoRetryAfter(t *testing.T) {
	srv := p13MessagesEngineServer(t, &provider.RateLimitError{Body: "slow down"})
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want absent when the upstream sent no hint", got)
	}
}

// TestAUD_REL_001PresentButZeroHintClampsToOne: present-but-zero is
// distinguishable from absent, and must never yield "retry after 0".
func TestAUD_REL_001PresentButZeroHintClampsToOne(t *testing.T) {
	srv := p13MessagesEngineServer(t, &provider.RateLimitError{RetryAfter: 0, RetryAfterPresent: true})
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want \"1\" (floor)", got)
	}
}

// TestAUD_REL_001QuotaKeepsUpstreamStatus covers upstream quota
// exhaustion: the upstream's own status is the truthful answer.
func TestAUD_REL_001QuotaKeepsUpstreamStatus(t *testing.T) {
	for _, tc := range []struct {
		upstream int
		want     int
	}{{http.StatusPaymentRequired, http.StatusPaymentRequired}, {http.StatusTooManyRequests, http.StatusTooManyRequests}, {0, http.StatusPaymentRequired}, {999, http.StatusPaymentRequired}} {
		srv := p13MessagesEngineServer(t, &provider.QuotaError{StatusCode: tc.upstream, Body: "quota"})
		rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
		if rec.Code != tc.want {
			t.Errorf("upstream quota status %d: got %d, want %d", tc.upstream, rec.Code, tc.want)
		}
		p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
	}
}

// TestAUD_REL_001UpstreamStatusNeverRelaysSuccess: a malformed upstream
// answer must not become a 200.
func TestAUD_REL_001UpstreamStatusNeverRelaysSuccess(t *testing.T) {
	srv := p13MessagesEngineServer(t, &provider.UpstreamError{StatusCode: 200, Body: "truncated"})
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a malformed upstream 200 is a failure)", rec.Code)
	}
	p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
}

// TestAUD_REL_001InternalLimitsStayDistinctFromUpstream429: our own
// per-key window refusal must not read as an upstream rate limit.
func TestAUD_REL_001InternalLimitsStayDistinctFromUpstream429(t *testing.T) {
	srv := p13MessagesEngineServer(t, &router.TokenLimitBreach{NextReset: time.Now().Add(45 * time.Second)})
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	p13DecodeAnthropicError(t, rec.Code, rec.Body.String())
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("internal window refusal must name its reset")
	}
	if strings.Contains(rec.Body.String(), "upstream provider rate limited") {
		t.Errorf("internal refusal described as an upstream rate limit: %s", rec.Body.String())
	}
}

// TestAUD_REL_001NoRetryAmplification: the mapping must not retry a
// rate-limited request — exactly one upstream attempt happens.
func TestAUD_REL_001NoRetryAmplification(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	calls := 0
	counting := &p13CountingEngine{inner: &fakeEngine{chatErr: &provider.RateLimitError{RetryAfter: time.Second, RetryAfterPresent: true}}, calls: &calls}
	srv := newMessagesServer(t, counting, mock, WithGuardrails(guardrails.NewStore()))
	srv.guardrails.SetConfig(guardrails.Config{BlockPII: true})

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13AnthropicBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("engine attempts = %d, want exactly 1 (no rate-limit amplification)", calls)
	}
}

type p13CountingEngine struct {
	inner Engine
	calls *int
}

func (c *p13CountingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	*c.calls++
	return c.inner.Chat(ctx, req)
}

func (c *p13CountingEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	*c.calls++
	return c.inner.ChatStream(ctx, req, onDelta)
}
