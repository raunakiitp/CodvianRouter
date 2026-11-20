package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestChatCompletionsResponseSanitizationNonStreaming proves B3-21 (row 784):
// non-streaming response sanitization via applyResponseGuardrails.
// With WithResponseSanitization(true) and default redact mode, a provider
// reply containing PII has the PII redacted in-place, and the response carries
// X-Codvian-Guardrail: redacted.
// When toggled off (WithResponseSanitization(false)), the reply flows flagged
// but unredacted.
// With ResponseModeBlock, the reply is refused with 403 policy_refusal.
func TestChatCompletionsResponseSanitizationNonStreaming(t *testing.T) {
	email := "secret-contact@example.com"

	// 1. Redact mode (default when responseSanitization is on)
	engRedact := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Contact the user at " + email,
		Model:   "test-model",
	}}
	srvRedact := newTestServer(t, engRedact, WithResponseSanitization(true))
	rec := doReq(t, srvRedact, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Guardrail"); got != "redacted" {
		t.Fatalf("X-Codvian-Guardrail = %q, want redacted", got)
	}
	if strings.Contains(rec.Body.String(), email) {
		t.Fatalf("response leaked raw email: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED:") {
		t.Fatalf("response missing redaction placeholder: %s", rec.Body.String())
	}

	// 2. Toggle check: responseSanitization OFF (default) delivers flagged but unredacted
	engOff := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Contact the user at " + email,
		Model:   "test-model",
	}}
	srvOff := newTestServer(t, engOff, WithResponseSanitization(false))
	recOff := doReq(t, srvOff, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if recOff.Code != http.StatusOK {
		t.Fatalf("off status = %d, want 200", recOff.Code)
	}
	if got := recOff.Header().Get("X-Codvian-Guardrail"); got != "flagged" {
		t.Fatalf("off X-Codvian-Guardrail = %q, want flagged", got)
	}
	if !strings.Contains(recOff.Body.String(), email) {
		t.Fatalf("off response should have preserved email: %s", recOff.Body.String())
	}

	// 3. Block mode: WithResponseGuardMode(ResponseModeBlock) withholds response with 403
	engBlock := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Contact the user at " + email,
		Model:   "test-model",
	}}
	srvBlock := newTestServer(t, engBlock, WithResponseSanitization(true), WithResponseGuardMode(guardrails.ResponseModeBlock))
	recBlock := doReq(t, srvBlock, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if recBlock.Code != http.StatusForbidden {
		t.Fatalf("block status = %d, want 403 (body: %s)", recBlock.Code, recBlock.Body.String())
	}
	if got := recBlock.Header().Get("X-Codvian-Guardrail"); got != "blocked" {
		t.Fatalf("block X-Codvian-Guardrail = %q, want blocked", got)
	}
	if strings.Contains(recBlock.Body.String(), email) {
		t.Fatalf("block response echoes email: %s", recBlock.Body.String())
	}
}

// TestChatCompletionsResponseSanitizationStreaming proves B3-22 (row 785):
// streaming response sanitization via ResponseStreamRedactor in handleChatStream.
// In redact mode, chunks carrying PII are sanitized before being sent over SSE.
// In block mode, the stream is cut at the finding.
// With sanitization off (toggle check), the stream carries verbatim PII.
func TestChatCompletionsResponseSanitizationStreaming(t *testing.T) {
	email := "stream-leak@example.com"
	deltas := []string{"Hello, ", "your login is ", email, "\nPlease keep it safe."}
	streamBody := `{"model":"test-model","messages":[{"role":"user","content":"tell me my login"}],"stream":true}`

	// 1. Redact mode: PII in delta is redacted
	engRedact := &fakeEngine{deltas: deltas}
	srvRedact := newTestServer(t, engRedact, WithResponseSanitization(true))
	recRedact := doReq(t, srvRedact, http.MethodPost, "/v1/chat/completions", streamBody, "test-key-123")
	if recRedact.Code != http.StatusOK {
		t.Fatalf("redact stream status = %d, want 200 (body: %s)", recRedact.Code, recRedact.Body.String())
	}
	bodyRedact := recRedact.Body.String()
	if strings.Contains(bodyRedact, email) {
		t.Fatalf("redact stream leaked raw email: %s", bodyRedact)
	}
	if !strings.Contains(bodyRedact, "[REDACTED:") {
		t.Fatalf("redact stream missing redaction placeholder: %s", bodyRedact)
	}

	// 2. Toggle check: sanitization off delivers verbatim stream
	engOff := &fakeEngine{deltas: deltas}
	srvOff := newTestServer(t, engOff, WithResponseSanitization(false))
	recOff := doReq(t, srvOff, http.MethodPost, "/v1/chat/completions", streamBody, "test-key-123")
	if recOff.Code != http.StatusOK {
		t.Fatalf("off stream status = %d, want 200", recOff.Code)
	}
	bodyOff := recOff.Body.String()
	if !strings.Contains(bodyOff, email) {
		t.Fatalf("off stream should contain raw email: %s", bodyOff)
	}

	// 3. Block mode: stream is cut at the finding and refusal returned (403 or policy_refusal)
	engBlock := &fakeEngine{deltas: deltas}
	srvBlock := newTestServer(t, engBlock, WithResponseSanitization(true), WithResponseGuardMode(guardrails.ResponseModeBlock))
	recBlock := doReq(t, srvBlock, http.MethodPost, "/v1/chat/completions", streamBody, "test-key-123")
	bodyBlock := recBlock.Body.String()
	if strings.Contains(bodyBlock, email) {
		t.Fatalf("block stream leaked raw email: %s", bodyBlock)
	}
	if recBlock.Code != http.StatusForbidden && !strings.Contains(bodyBlock, "policy_refusal") {
		t.Fatalf("block stream neither 403 nor policy_refusal: code=%d body=%s", recBlock.Code, bodyBlock)
	}
}

// TestMessagesRelayResponseSanitizationStreaming proves B3-26 (row 798):
// route coverage of response checks extending to verbatim relay streams.
// When WithResponseSanitization(true) is enabled, Anthropic SSE relay stream
// running through handleMessagesStream passes chunks through ResponseStreamRedactor.
func TestMessagesRelayResponseSanitizationStreaming(t *testing.T) {
	email := "relay-leak@example.com"
	rawSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Contact " + email + "\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	reqBody := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`

	// 1. Sanitization ON: relay stream redacts PII
	mockOn := &anthropicMock{streamBody: rawSSE}
	srvOn := newMessagesServer(t, &fakeEngine{}, mockOn, WithResponseSanitization(true))
	recOn := doReq(t, srvOn, http.MethodPost, "/v1/messages", reqBody, "test-key-123")
	if recOn.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recOn.Code, recOn.Body.String())
	}
	gotOn := recOn.Body.String()
	if strings.Contains(gotOn, email) {
		t.Fatalf("sanitization ON: relay stream leaked email: %s", gotOn)
	}
	if !strings.Contains(gotOn, "[REDACTED:") {
		t.Fatalf("sanitization ON: relay stream missing redacted token: %s", gotOn)
	}

	// 2. Toggle check: Sanitization OFF: verbatim passthrough
	mockOff := &anthropicMock{streamBody: rawSSE}
	srvOff := newMessagesServer(t, &fakeEngine{}, mockOff, WithResponseSanitization(false))
	recOff := doReq(t, srvOff, http.MethodPost, "/v1/messages", reqBody, "test-key-123")
	if recOff.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recOff.Code, recOff.Body.String())
	}
	gotOff := recOff.Body.String()
	if !strings.Contains(gotOff, email) {
		t.Fatalf("sanitization OFF: relay stream should contain raw email: %s", gotOff)
	}
}
