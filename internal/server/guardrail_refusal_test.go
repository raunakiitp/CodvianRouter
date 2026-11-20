package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestGuardrailRefusalShape proves a guardrail block is a policy
// refusal, not a generic client error and not an upstream failure
// (CHECKLIST 147, research-log entry 179): HTTP 403 with the
// Codvian policy-refusal code -32001, type "policy_refusal" and a
// machine-readable reason naming the kind — never the upstream
// -32603, never a bare 400.
func TestGuardrailRefusalShape(t *testing.T) {
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}}}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":false,"block_secrets":true,"block_injection":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle POST status = %d", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "my key is sk-proj-abcdefghij1234567890", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    *int   `json:"code"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("block body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code == nil || *body.Error.Code != guardrails.PolicyRefusalCode {
		t.Fatalf("error.code = %v, want %d (body %s)", body.Error.Code, guardrails.PolicyRefusalCode, rec.Body.String())
	}
	if body.Error.Type != "policy_refusal" {
		t.Fatalf("error.type = %q, want policy_refusal", body.Error.Type)
	}
	if body.Error.Reason != guardrails.KindSecret {
		t.Fatalf("error.reason = %q, want %q", body.Error.Reason, guardrails.KindSecret)
	}
	if body.Error.Message == "" || !strings.Contains(body.Error.Message, guardrails.KindSecret) {
		t.Fatalf("error.message = %q, want it to name the kind", body.Error.Message)
	}
	if strings.Contains(rec.Body.String(), "sk-proj-") {
		t.Fatal("refusal echoes the secret back")
	}
	if eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", eng.chatCalls.Load())
	}
}

// TestResponseGuardrailBlocksNonStreaming proves the response
// guardrail (CHECKLIST 147): a provider reply carrying PII is
// withheld with 403 / -32001 when BlockPII is on, and its content
// never reaches the client — while the same reply flows with the
// toggles off. Tool-call arguments the model produced are scanned
// too.
func TestResponseGuardrailBlocksNonStreaming(t *testing.T) {
	email := "leaked@example.com"
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{
		Content: "reach me at " + email, Model: "test-model",
	}}}
	srv := newTestServer(t, eng)
	// Toggles off: the PII reply flows, flagged but delivered.
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), email) {
		t.Fatalf("toggles-off reply: status=%d body=%s, want 200 carrying the reply", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Guardrail"); got != "flagged" {
		t.Fatalf("X-Codvian-Guardrail = %q, want flagged", got)
	}
	// BlockPII on: the same reply is refused, content withheld.
	rec = doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":true,"block_secrets":false,"block_injection":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle POST status = %d", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked response status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), email) {
		t.Fatal("refusal echoes the provider's PII reply back")
	}
	var body struct {
		Error struct {
			Code   *int   `json:"code"`
			Reason string `json:"reason"`
			Type   string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal body is not JSON: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != guardrails.PolicyRefusalCode {
		t.Fatalf("error.code = %v, want %d", body.Error.Code, guardrails.PolicyRefusalCode)
	}
	if body.Error.Reason != guardrails.KindPII || body.Error.Type != "policy_refusal" {
		t.Fatalf("error = %+v, want reason pii / type policy_refusal", body.Error)
	}
	if got := rec.Header().Get("X-Codvian-Guardrail"); got != "blocked" {
		t.Fatalf("X-Codvian-Guardrail = %q, want blocked", got)
	}
	// A secret inside tool-call arguments the model produced is
	// scanned the same way.
	key := "sk-proj-abcdefghij1234567890"
	eng2 := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{
		Content: "calling tool", Model: "test-model",
		ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: wire.ToolCallFunction{Name: "lookup", Arguments: `{"key":"` + key + `"}`},
		}},
	}}}
	srv2 := newTestServer(t, eng2)
	_ = doReq(t, srv2, http.MethodPost, "/dashboard/guardrails", `{"block_pii":false,"block_secrets":true,"block_injection":false}`, "test-key-123")
	rec = doReq(t, srv2, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tool-args response status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatal("refusal echoes the tool-call secret back")
	}
}

// TestResponseGuardrailStreamingLimitation pins the honest limit:
// a streaming response is delivered incrementally and cannot be
// recalled, so the response guardrail does not (and cannot) block
// it — the request-side check is the only guard on a stream. If
// this test ever fails with a 403, the stream path changed; update
// the CHECKLIST limitation note with it.
func TestResponseGuardrailStreamingLimitation(t *testing.T) {
	eng := &fakeEngine{
		chatResp: provider.ChatResponse{Content: "reach me at leaked@example.com", Model: "test-model"},
		deltas:   []string{"reach me at leaked@example.com"},
	}
	srv := newTestServer(t, eng)
	_ = doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":true,"block_secrets":false,"block_injection":false}`, "")
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", true), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming PII reply: status=%d, want 200 (delivered — streams cannot be recalled)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "leaked@example.com") {
		t.Fatalf("streaming reply did not deliver the content:\n%s", rec.Body.String())
	}
}

// TestUpstreamFailureCodeDistinct proves the other half of the
// contract (CHECKLIST 147): an identical upstream failure answers
// with its own distinct shape — HTTP 502 with code -32603 for an
// exhausted chain, the upstream's own 4xx with no JSON-RPC code
// for a terminal rejection — and never -32001. A guardrail block
// of the same request shape answers 403 / -32001.
func TestUpstreamFailureCodeDistinct(t *testing.T) {
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) (code *int, typ, reason string) {
		t.Helper()
		var body struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    *int   `json:"code"`
				Reason  string `json:"reason"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
		}
		return body.Error.Code, body.Error.Type, body.Error.Reason
	}
	// Exhausted chain (502): code -32603, type server_error.
	eng := &fakeEngine{chatErr: &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	code, typ, reason := decode(t, rec)
	if code == nil || *code != guardrails.UpstreamFailureCode {
		t.Fatalf("upstream error.code = %v, want %d", code, guardrails.UpstreamFailureCode)
	}
	if typ != "server_error" || reason != "" {
		t.Fatalf("upstream error type/reason = %q/%q, want server_error/\"\"", typ, reason)
	}
	// Terminal upstream rejection (400): the upstream's own status,
	// no JSON-RPC code at all.
	eng = &fakeEngine{chatErr: &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: "bad request"}}
	srv = newTestServer(t, eng)
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("terminal status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	code, typ, _ = decode(t, rec)
	if code != nil {
		t.Fatalf("terminal error.code = %d, want absent (the status is its code)", *code)
	}
	if typ != "upstream_error" {
		t.Fatalf("terminal error.type = %q, want upstream_error", typ)
	}
	// Same request shape, guardrail block instead: 403 / -32001.
	eng2 := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}}}
	srv = newTestServer(t, eng2)
	_ = doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":false,"block_secrets":true,"block_injection":false}`, "test-key-123")
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "my key is sk-proj-abcdefghij1234567890", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("refusal status = %d, want 403", rec.Code)
	}
	code, typ, reason = decode(t, rec)
	if code == nil || *code != guardrails.PolicyRefusalCode || typ != "policy_refusal" || reason != guardrails.KindSecret {
		t.Fatalf("refusal error = code %v type %q reason %q, want -32001/policy_refusal/secret", code, typ, reason)
	}
}

// TestGuardrailMetricsPhaseAction proves the metrics carry the
// phase/action labels (CHECKLIST 147): request + response
// evaluations are counted separately, blocked vs allowed, and
// upstream failures have their own counter — never mixed into the
// guardrail series.
func TestGuardrailMetricsPhaseAction(t *testing.T) {
	recorder := metrics.NewRecorder()
	eng := &fakeEngine{
		chatResp: provider.ChatResponse{Content: "reach me at leaked@example.com", Model: "test-model"},
		chatErr:  &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"},
	}
	srv := newTestServer(t, eng, WithMetrics(recorder))
	_ = doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":true,"block_secrets":false,"block_injection":false}`, "test-key-123")
	// Upstream failure: the engine fails before any response exists.
	_ = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	// Response refusal: make the engine succeed with PII content.
	eng.chatErr = nil
	_ = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	// Allowed request: clean prompt, clean (changed) response.
	eng.chatResp = provider.ChatResponse{Content: "ok", Model: "test-model"}
	_ = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	rec := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`codvian_router_guardrail_evaluations_total{phase="request",action="allowed"} 3`,
		`codvian_router_guardrail_evaluations_total{phase="response",action="blocked"} 1`,
		`codvian_router_guardrail_evaluations_total{phase="response",action="allowed"} 1`,
		`codvian_router_upstream_failures_total 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack series %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `codvian_router_guardrail_evaluations_total{phase="response",action="blocked"} 2`) {
		t.Fatal("response blocked counted twice")
	}
}

// TestGuardrailsFeedRefusalCodes proves the dashboard feed carries
// the refusal-code contract and the live upstream-failure count
// next to the per-phase evaluation breakdown (CHECKLIST 148).
func TestGuardrailsFeedRefusalCodes(t *testing.T) {
	recorder := metrics.NewRecorder()
	eng := &countingEngine{fakeEngine: &fakeEngine{
		chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"},
		chatErr:  &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"},
	}}
	srv := newTestServer(t, eng, WithMetrics(recorder))
	_ = doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":false,"block_secrets":true,"block_injection":false}`, "test-key-123")
	_ = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "my key is sk-proj-abcdefghij1234567890", false), "test-key-123")
	_ = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed status = %d", rec.Code)
	}
	var feed struct {
		guardrails.Stats
		PolicyRefusalCode   int    `json:"policy_refusal_code"`
		UpstreamFailureCode int    `json:"upstream_failure_code"`
		UpstreamFailures    uint64 `json:"upstream_failures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if feed.PolicyRefusalCode != guardrails.PolicyRefusalCode {
		t.Fatalf("policy_refusal_code = %d, want %d", feed.PolicyRefusalCode, guardrails.PolicyRefusalCode)
	}
	if feed.UpstreamFailureCode != guardrails.UpstreamFailureCode {
		t.Fatalf("upstream_failure_code = %d, want %d", feed.UpstreamFailureCode, guardrails.UpstreamFailureCode)
	}
	if feed.UpstreamFailures != 1 {
		t.Fatalf("upstream_failures = %d, want 1 (one exhausted-chain 502)", feed.UpstreamFailures)
	}
	if feed.Blocked != 1 {
		t.Fatalf("blocked = %d, want 1 (one policy refusal)", feed.Blocked)
	}
	evals := map[string]uint64{}
	for _, e := range feed.Evaluations {
		evals[e.Phase+"/"+e.Action] = e.Count
	}
	if evals["request/blocked"] != 1 || evals["request/allowed"] != 1 {
		t.Fatalf("evaluations = %+v, want request/blocked 1 and request/allowed 1", evals)
	}
}
