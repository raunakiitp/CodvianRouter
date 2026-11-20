package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

func TestIsModelNotFoundSignal(t *testing.T) {
	yes := []string{
		`{"error":{"message":"The model ` + "`gpt-9`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
		`{"error":{"message":"model: claude-x not found","type":"not_found_error"}}`,
		`{"error":{"message":"models/gemini-x is not found","code":404,"status":"NOT_FOUND"}}`,
		`The provided model identifier is invalid.`,
		`The specified model does not exist`,
		`Unknown model "foo"`,
		`no such model: foo`,
		`MODEL_NOT_FOUND`,
	}
	for _, text := range yes {
		if !IsModelNotFoundSignal(text) {
			t.Errorf("IsModelNotFoundSignal(%q) = false, want true", text)
		}
	}
	no := []string{
		`{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`,
		`{"error":{"message":"messages must not be empty","type":"invalid_request_error"}}`,
		`unknown model parameter "foo" is not supported`,
		`invalid JSON in request body`,
		`content filtered by policy`,
		``,
		`the model served this request fine`,
	}
	for _, text := range no {
		if IsModelNotFoundSignal(text) {
			t.Errorf("IsModelNotFoundSignal(%q) = true, want false", text)
		}
	}
}

func TestClassifyError(t *testing.T) {
	malformed400 := &UpstreamError{StatusCode: 400, Body: `{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`}
	deadModel400 := &UpstreamError{StatusCode: 400, Body: `{"error":{"message":"The model ` + "`gpt-9`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`}
	deadModel404 := &UpstreamError{StatusCode: 404, Body: `{"error":{"message":"model: claude-x not found","type":"not_found_error"}}`}
	bare404 := &UpstreamError{StatusCode: 404, Body: `{"error":{"message":"boom"}}`}
	cases := []struct {
		name     string
		err      error
		endpoint EndpointKind
		decision FallbackDecision
		reason   string
		status   int
	}{
		// The entry-151 split: same status, opposite verdicts.
		{"400 malformed stops", malformed400, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 400},
		{"400 dead model falls back", deadModel400, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 400},
		{"404 dead model falls back", deadModel404, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 404},
		{"404 bare falls back as not_found", bare404, EndpointChatCompletions, DecisionFallback, ReasonNotFound, 404},
		// Entry-183 status rules.
		{"529 falls back overloaded", &UpstreamError{StatusCode: 529, Body: "overloaded"}, EndpointChatCompletions, DecisionFallback, ReasonOverloaded, 529},
		{"500 falls back", &UpstreamError{StatusCode: 500, Body: "boom"}, EndpointChatCompletions, DecisionFallback, ReasonUpstreamError, 500},
		{"502 falls back", &UpstreamError{StatusCode: 502, Body: "boom"}, EndpointChatCompletions, DecisionFallback, ReasonUpstreamError, 502},
		{"503 falls back", &UpstreamError{StatusCode: 503, Body: "boom"}, EndpointChatCompletions, DecisionFallback, ReasonUpstreamError, 503},
		{"504 falls back", &UpstreamError{StatusCode: 504, Body: "boom"}, EndpointChatCompletions, DecisionFallback, ReasonUpstreamError, 504},
		{"408 falls back timeout", &UpstreamError{StatusCode: 408, Body: "slow"}, EndpointChatCompletions, DecisionFallback, ReasonTimeout, 408},
		{"402 falls back quota", &UpstreamError{StatusCode: 402, Body: "pay up"}, EndpointChatCompletions, DecisionFallback, ReasonQuotaExhausted, 402},
		{"401 stops auth", &UpstreamError{StatusCode: 401, Body: "bad key"}, EndpointChatCompletions, DecisionStop, ReasonAuthFailed, 401},
		{"403 stops forbidden", &UpstreamError{StatusCode: 403, Body: "nope"}, EndpointChatCompletions, DecisionStop, ReasonForbidden, 403},
		{"422 stops invalid", &UpstreamError{StatusCode: 422, Body: "bad field"}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 422},
		{"409 stops", &UpstreamError{StatusCode: 409, Body: "conflict"}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 409},
		// 405/501: endpoint-scoped.
		{"405 chat stops", &UpstreamError{StatusCode: 405, Body: "no"}, EndpointChatCompletions, DecisionStop, ReasonEndpointUnsupported, 405},
		{"501 chat stops", &UpstreamError{StatusCode: 501, Body: "no"}, EndpointChatCompletions, DecisionStop, ReasonEndpointUnsupported, 501},
		{"405 responses falls back", &UpstreamError{StatusCode: 405, Body: "no"}, EndpointResponses, DecisionFallback, ReasonEndpointUnsupported, 405},
		{"501 embeddings falls back", &UpstreamError{StatusCode: 501, Body: "no"}, EndpointEmbeddings, DecisionFallback, ReasonEndpointUnsupported, 501},
		// Typed errors.
		{"rate limit falls back", &RateLimitError{}, EndpointChatCompletions, DecisionFallback, ReasonRateLimited, 429},
		{"quota 402 falls back", &QuotaError{StatusCode: 402, Message: "out of credits"}, EndpointChatCompletions, DecisionFallback, ReasonQuotaExhausted, 402},
		{"quota 400 falls back", &QuotaError{StatusCode: 400, Message: "billing"}, EndpointChatCompletions, DecisionFallback, ReasonQuotaExhausted, 400},
		{"malformed envelope falls back", &UpstreamError{StatusCode: 0, Body: "upstream returned no choices"}, EndpointChatCompletions, DecisionFallback, ReasonInvalidResponse, 0},
		// Caller / transport.
		{"cancelled stops", context.Canceled, EndpointChatCompletions, DecisionStop, ReasonCancelled, 0},
		{"deadline falls back timeout", context.DeadlineExceeded, EndpointChatCompletions, DecisionFallback, ReasonTimeout, 0},
		{"untyped transport falls back", errors.New("connection refused"), EndpointChatCompletions, DecisionFallback, ReasonTransportError, 0},
		// CHECKLIST 189: a limiter shed is per-target capacity,
		// not a property of the request — it falls back to the
		// next target (classifying it Stop broke the chain: the
		// first shed returned ErrLimited while a healthy
		// provider stood idle). Only an all-targets shed fails
		// the request, with ErrLimited as the last error.
		{"limiter shed falls back", adaptive.ErrLimited, EndpointChatCompletions, DecisionFallback, ReasonConcurrencyLimited, 0},
		{"wrapped limiter shed falls back", &wrappedErr{adaptive.ErrLimited}, EndpointChatCompletions, DecisionFallback, ReasonConcurrencyLimited, 0},
		{"nil stops", nil, EndpointChatCompletions, DecisionStop, "none", 0},
		// Wire errors (raw, as mid-stream failures surface them).
		{"anthropic 404 falls back", &wire.AnthropicError{StatusCode: 404, Type: "not_found_error", Message: "model: x not found"}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 404},
		{"anthropic 400 malformed stops", &wire.AnthropicError{StatusCode: 400, Type: "invalid_request_error", Message: "temperature must be <= 1"}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 400},
		{"anthropic 400 dead model falls back", &wire.AnthropicError{StatusCode: 400, Type: "invalid_request_error", Message: "The model `x` does not exist"}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 400},
		{"gemini 404 falls back", &wire.GeminiError{StatusCode: 404, Status: "NOT_FOUND", Message: "models/x is not found"}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 404},
		{"cohere 404 falls back", &wire.CohereError{StatusCode: 404, Message: "model not found"}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 404},
		{"bedrock 400 bad model id falls back", &wire.BedrockError{StatusCode: 400, Type: "ValidationException", Message: "The provided model identifier is invalid."}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 400},
		{"bedrock 400 other stops", &wire.BedrockError{StatusCode: 400, Type: "ValidationException", Message: "1 validation error detected"}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 400},
		{"anthropic stream overloaded falls back", &wire.AnthropicError{StatusCode: 529, Type: "overloaded_error", Message: "overloaded"}, EndpointChatCompletions, DecisionFallback, ReasonOverloaded, 529},
		{"anthropic stream api_error falls back", &wire.AnthropicError{StatusCode: 0, Type: "api_error", Message: "boom", Retryable: true}, EndpointChatCompletions, DecisionFallback, ReasonUpstreamError, 0},
		{"anthropic stream invalid stops", &wire.AnthropicError{StatusCode: 0, Type: "invalid_request_error", Message: "bad input", Retryable: false}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 0},
		{"unrepresentable stops", &wire.UnrepresentableError{Reason: "temperature > 1"}, EndpointChatCompletions, DecisionStop, ReasonUnrepresentable, 0},
		// Wrapping must not hide the verdict.
		{"wrapped dead-model 404 falls back", &wrappedErr{&UpstreamError{StatusCode: 404, Body: "model not found"}}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 404},
		{"wrapped malformed 400 stops", &wrappedErr{malformed400}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err, tc.endpoint)
			if got.Decision != tc.decision || got.Reason != tc.reason || got.Status != tc.status {
				t.Errorf("ClassifyError(%v, %q) = %+v, want decision=%v reason=%q status=%d",
					tc.err, tc.endpoint, got, tc.decision, tc.reason, tc.status)
			}
		})
	}
}

// TestIsContentRefusalSignal pins the CHECKLIST 161 phrase list:
// the shapes providers actually emit for a content-policy /
// safety refusal must match, and nearby non-refusal text must
// not (a 400 about the request shape is request_invalid, not a
// refusal — the distinction is the whole point of the chain).
func TestIsContentRefusalSignal(t *testing.T) {
	yes := []string{
		`{"error":{"message":"content filtered due to policy","type":"invalid_request_error","code":"content_filter"}}`,
		`{"error":{"message":"This request violates our content policy","type":"invalid_request_error"}}`,
		`{"error":{"message":"blocked by safety filters","code":400,"status":"FAILED_PRECONDITION"}}`,
		`The prompt was blocked for safety reasons.`,
		`Content policy violation: disallowed content`,
		`guardrail_intervened: content blocked by guardrail`,
		`Request refused: harmful content detected by moderation`,
		`inappropriate content is not allowed`,
		`prohibited content in request`,
		`CONTENT_FILTER`,
	}
	for _, text := range yes {
		if !IsContentRefusalSignal(text) {
			t.Errorf("IsContentRefusalSignal(%q) = false, want true", text)
		}
	}
	no := []string{
		`{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`,
		`{"error":{"message":"messages must not be empty","type":"invalid_request_error"}}`,
		`{"error":{"message":"The model ` + "`gpt-9`" + ` does not exist","type":"invalid_request_error"}}`,
		`connection refused`,
		`the model served this request fine`,
		``,
		`safety is our top priority in this release`,
	}
	for _, text := range no {
		if IsContentRefusalSignal(text) {
			t.Errorf("IsContentRefusalSignal(%q) = true, want false", text)
		}
	}
}

// TestClassifyContentRefusal pins the CHECKLIST 161 taxonomy
// verdict: a 400/403/422 whose body signals a content refusal
// falls back WITH the refusal flag (onto the refusal chain),
// while the same statuses without the signal keep their old
// verdicts — a malformed 400 still stops, a plain 403 still
// stops. A typed ContentRefusalError classifies the same way.
func TestClassifyContentRefusal(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		endpoint EndpointKind
		decision FallbackDecision
		reason   string
		status   int
		refusal  bool
	}{
		{"400 content_filter falls back as refusal", &UpstreamError{StatusCode: 400, Body: `{"error":{"message":"content filtered due to policy","code":"content_filter"}}`}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"400 policy violation falls back as refusal", &UpstreamError{StatusCode: 400, Body: "This request violates our content policy"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"403 safety falls back as refusal", &UpstreamError{StatusCode: 403, Body: "blocked by safety filters"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 403, true},
		{"422 moderation falls back as refusal", &UpstreamError{StatusCode: 422, Body: "content moderation: disallowed content"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 422, true},
		{"400 malformed still stops, not a refusal", &UpstreamError{StatusCode: 400, Body: "temperature must be <= 1"}, EndpointChatCompletions, DecisionStop, ReasonRequestInvalid, 400, false},
		{"403 plain still stops, not a refusal", &UpstreamError{StatusCode: 403, Body: "nope"}, EndpointChatCompletions, DecisionStop, ReasonForbidden, 403, false},
		{"400 dead model still falls back, not a refusal", &UpstreamError{StatusCode: 400, Body: "The model `x` does not exist"}, EndpointChatCompletions, DecisionFallback, ReasonModelNotFound, 400, false},
		{"typed refusal", &ContentRefusalError{StatusCode: 400, Message: "finish_reason content_filter"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"typed refusal no status", &ContentRefusalError{Message: "blocked prompt"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 0, true},
		{"anthropic 400 content policy falls back as refusal", &wire.AnthropicError{StatusCode: 400, Type: "invalid_request_error", Message: "content policy violation"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"gemini 400 safety falls back as refusal", &wire.GeminiError{StatusCode: 400, Status: "FAILED_PRECONDITION", Message: "blocked by safety settings"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"bedrock guardrail falls back as refusal", &wire.BedrockError{StatusCode: 400, Type: "ValidationException", Message: "guardrail_intervened: content blocked"}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"mid-stream wire refusal, no status", &wire.AnthropicError{Type: "invalid_request_error", Message: "content filter triggered", Retryable: false}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 0, true},
		{"wrapped refusal keeps the flag", &wrappedErr{&UpstreamError{StatusCode: 400, Body: "content policy violation"}}, EndpointChatCompletions, DecisionFallback, ReasonContentRefused, 400, true},
		{"empty-stream content_filter is empty_stream in the taxonomy (the router flags it as a refusal via the finish reason)", &EmptyStreamError{FinishReason: "content_filter"}, EndpointChatCompletions, DecisionFallback, ReasonEmptyStream, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err, tc.endpoint)
			if got.Decision != tc.decision || got.Reason != tc.reason || got.Status != tc.status || got.ContentRefusal != tc.refusal {
				t.Errorf("ClassifyError(%v) = %+v, want decision=%v reason=%q status=%d refusal=%v",
					tc.err, got, tc.decision, tc.reason, tc.status, tc.refusal)
			}
		})
	}
}

// wrappedErr is a fmt-wrap stand-in: Unwrap only, no other behaviour.
type wrappedErr struct{ inner error }

func (e *wrappedErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedErr) Unwrap() error { return e.inner }

// TestClassifyErrorMatchesWireTable pins the classifier to the
// shared status table: for every status the wire table calls
// retryable (other than 400/404, which the body-signal layer
// owns), the classifier must agree — the two tables must never
// drift apart silently.
func TestClassifyErrorMatchesWireTable(t *testing.T) {
	for _, status := range []int{402, 408, 429, 500, 502, 503, 504, 529} {
		got := ClassifyError(&UpstreamError{StatusCode: status, Body: "boom"}, EndpointChatCompletions)
		if got.Decision != DecisionFallback {
			t.Errorf("ClassifyError(%d) = stop, want fallback (wire table says retryable)", status)
		}
		if !wire.RetryableStatusForEndpoint(status, "") {
			t.Errorf("wire table disagrees on %d (test bug, not classifier bug)", status)
		}
	}
	for _, status := range []int{401, 403, 405, 422, 409} {
		got := ClassifyError(&UpstreamError{StatusCode: status, Body: "boom"}, EndpointChatCompletions)
		if got.Decision != DecisionStop {
			t.Errorf("ClassifyError(%d) = fallback, want stop (wire table says non-retryable)", status)
		}
	}
	// 405/501 agree with the wire table on responses/embeddings too.
	for _, status := range []int{405, 501} {
		if got := ClassifyError(&UpstreamError{StatusCode: status}, EndpointResponses); got.Decision != DecisionFallback {
			t.Errorf("ClassifyError(%d, responses) = stop, want fallback", status)
		}
		if got := ClassifyError(&UpstreamError{StatusCode: status}, EndpointEmbeddings); got.Decision != DecisionFallback {
			t.Errorf("ClassifyError(%d, embeddings) = stop, want fallback", status)
		}
	}
}

// TestMapStatusErrorModelNotFound pins the OpenAI-compatible
// client's Retryable flag to the taxonomy: a 400 carrying a
// model-not-found body is retryable (it falls back), a plain 400
// is not, and a 404 always is.
func TestMapStatusErrorModelNotFound(t *testing.T) {
	h := http.Header{}
	dead400 := mapStatusError(400, h, []byte(`{"error":{"message":"The model `+"`gpt-9`"+` does not exist","type":"invalid_request_error","code":"model_not_found"}}`))
	var ue *UpstreamError
	if !errors.As(dead400, &ue) || !ue.Retryable {
		t.Errorf("mapStatusError(400 model-not-found) = %v, want retryable UpstreamError", dead400)
	}
	bad400 := mapStatusError(400, h, []byte(`{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`))
	if !errors.As(bad400, &ue) || ue.Retryable {
		t.Errorf("mapStatusError(400 malformed) = %v, want non-retryable UpstreamError", bad400)
	}
	nf404 := mapStatusError(404, h, []byte(`{"error":{"message":"model not found","type":"not_found_error"}}`))
	if !errors.As(nf404, &ue) || !ue.Retryable {
		t.Errorf("mapStatusError(404) = %v, want retryable UpstreamError", nf404)
	}
}

// TestClassifyEmptyStream pins the preread taxonomy (CHECKLIST 133,
// research-log entries 97 + 111): an empty stream fails over, and a
// client-synthesised status-200 error the client marked retryable
// ("SSE stream ended without [DONE]" with no content) fails over
// too — the status table alone would stop the chain on a bare 200,
// which would strand an empty pre-commit stream on a 502. A 200
// error the client marked non-retryable (malformed SSE) still stops.
func TestClassifyEmptyStream(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		decision FallbackDecision
		reason   string
	}{
		{"empty stream falls back", &EmptyStreamError{}, DecisionFallback, ReasonEmptyStream},
		{"empty stream with finish reason falls back", &EmptyStreamError{FinishReason: "stop"}, DecisionFallback, ReasonEmptyStream},
		{"wrapped empty stream falls back", &wrappedErr{&EmptyStreamError{FinishReason: "content_filter"}}, DecisionFallback, ReasonEmptyStream},
		{"truncated 200 stream falls back", &UpstreamError{StatusCode: 200, Retryable: true, Body: "SSE stream ended without [DONE]"}, DecisionFallback, ReasonInvalidResponse},
		{"malformed SSE 200 stops", &UpstreamError{StatusCode: 200, Retryable: false, Body: "malformed SSE line: garbage"}, DecisionStop, ReasonRequestInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err, EndpointChatCompletions)
			if got.Decision != tc.decision || got.Reason != tc.reason {
				t.Errorf("ClassifyError(%v) = %+v, want decision=%v reason=%q", tc.err, got, tc.decision, tc.reason)
			}
		})
	}
	if got := (&EmptyStreamError{FinishReason: "stop"}).Error(); got != "upstream stream ended with no content (finish_reason: stop)" {
		t.Errorf("EmptyStreamError.Error() = %q", got)
	}
	if got := (&EmptyStreamError{}).Error(); got != "upstream stream ended with no content" {
		t.Errorf("EmptyStreamError.Error() = %q", got)
	}
}
