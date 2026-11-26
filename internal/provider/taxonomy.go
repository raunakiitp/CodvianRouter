// Package provider — error taxonomy (research-log entries 151 + 183).
//
// ClassifyError is the single place that decides whether an upstream
// failure means "try the next target" (the failure is a property of
// *that target*) or "stop the chain" (the failure is a property of
// the request and would fail identically everywhere).
//
// The bare HTTP-status table lives in internal/wire
// (RetryableStatusForEndpoint: 402/404/408/429/500/502/503/504/529
// retryable; 405/501 retryable only on /v1/responses and
// /v1/embeddings; 400/401/403/422 stop the chain) and is reused
// here, never duplicated. What this file adds on top of the status
// table is the body-signal layer entry 151 demands: a 400 (or 404)
// that means "model not found at this target" falls back, while a
// 400 that means "malformed request" stops — the distinction is
// model-availability vs request-validity, and it belongs here in
// the taxonomy, not in a raw status list.
//
// A 401 stops the chain (landed taxonomy choice, not SparkRouter's
// literal list): a bad key fails identically on every target of
// that provider, and retrying it only burns attempts.
package provider

import (
	"context"
	"errors"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// EndpointKind names the API surface a request was made to. The
// kind matters only for the 405/501 rule: a target that lacks
// /v1/responses or /v1/embeddings may still serve the same request
// on another target, while on chat completions 405/501 is a client
// error and stops the chain.
type EndpointKind string

const (
	// EndpointChatCompletions is POST /v1/chat/completions.
	EndpointChatCompletions EndpointKind = "chat_completions"
	// EndpointMessages is POST /v1/messages (Anthropic Messages
	// dialect, translated through the engine — CHECKLIST 211).
	// It is NOT replay-exempt: a translated messages request
	// fails over like a chat completion, and its 405/501 rule is
	// the chat rule (wireName returns "" for it, below). It
	// exists so the route trace can name the surface: without
	// it a messages trace would be indistinguishable from a
	// chat trace on the dashboard.
	EndpointMessages EndpointKind = "messages"
	// EndpointResponses is POST /v1/responses.
	EndpointResponses EndpointKind = "responses"
	// EndpointGeminiGenerateContent is POST /v1beta/models/{model}:generateContent
	// (Gemini native surface). It maps to the chat surface for routing
	// purposes but carries its own kind so Gemini spend rows are
	// attributable separately from chat completions in analytics.
	EndpointGeminiGenerateContent EndpointKind = "gemini_generate_content"
	// EndpointEmbeddings is POST /v1/embeddings.
	EndpointEmbeddings EndpointKind = "embeddings"
	// EndpointImages is POST /v1/images (generations/edits).
	EndpointImages EndpointKind = "images"
	// EndpointRerank is POST /v1/rerank.
	EndpointRerank EndpointKind = "rerank"
)

// wireName renders the kind the way wire.RetryableStatusForEndpoint
// expects it ("" for chat completions, "responses"/"embeddings"/
// "rerank"/"images" otherwise).
func (k EndpointKind) wireName() string {
	switch k {
	case EndpointResponses:
		return "responses"
	case EndpointEmbeddings:
		return "embeddings"
	case EndpointRerank:
		return "rerank"
	case EndpointImages:
		return "images"
	default:
		return ""
	}
}

// FallbackDecision is what the router should do after a failed
// attempt: try the next target, or stop the chain and report the
// failure.
type FallbackDecision int

const (
	// DecisionFallback means the failure is a property of that
	// target — the next target may serve the same request.
	DecisionFallback FallbackDecision = iota
	// DecisionStop means the failure is a property of the request
	// (or the caller) — retrying it elsewhere would fail the
	// same way, so the chain stops and the error is reported.
	DecisionStop
)

// Classification is the taxonomy verdict for one upstream failure.
// Reason is a stable machine-readable slug for the dashboard and
// the route trace (see the Reason constants below); Status is the
// upstream HTTP status when one is known, 0 otherwise.
// ContentRefusal is true when the failure is an upstream
// content-policy / safety refusal (ReasonContentRefused): the
// content was refused, not the request shape and not the
// provider's health. Callers must route it onto the refusal
// chain (never the same target, never a breaker/cooldown/health
// signal) — DecisionFallback alone does not say that.
type Classification struct {
	Decision       FallbackDecision
	Reason         string
	Status         int
	ContentRefusal bool
}

// ContentRefusalError is a typed upstream content-policy / safety
// refusal: the provider refused the content itself. Clients that
// can tell a refusal apart from an HTTP error (a
// finish_reason of "content_filter" with no content, a blocked
// prompt) return this instead of a generic UpstreamError, so the
// taxonomy never has to guess from a message string. StatusCode
// is the upstream HTTP status when one is known, 0 otherwise.
type ContentRefusalError struct {
	StatusCode int
	Message    string
}

func (e *ContentRefusalError) Error() string {
	if e == nil {
		return "upstream refused content on policy grounds"
	}
	if e.Message != "" {
		return "upstream refused content on policy grounds: " + e.Message
	}
	return "upstream refused content on policy grounds"
}

// Fallback-reason slugs. They are the vocabulary the dashboard's
// Live Routing view renders per attempt; keep them stable.
const (
	// ReasonRateLimited is a 429 (or a rate-limit error type).
	ReasonRateLimited = "rate_limited"
	// ReasonQuotaExhausted is a 402 / billing/quota error: that
	// target's key/account cannot pay, another target may.
	ReasonQuotaExhausted = "quota_exhausted"
	// ReasonModelNotFound is a 400/404 whose body signals that
	// the model does not exist at that target. It falls back.
	ReasonModelNotFound = "model_not_found"
	// ReasonNotFound is a 404 with no model signal in the body.
	// It still falls back (a 404 is a property of that target),
	// but the dashboard says so honestly instead of claiming a
	// dead model it cannot prove.
	ReasonNotFound = "not_found"
	// ReasonOverloaded is a 529 (Anthropic/Cloudflare overload).
	ReasonOverloaded = "overloaded"
	// ReasonUpstreamError is a 5xx (other than 529) or a
	// retryable mid-stream failure with no more specific cause.
	ReasonUpstreamError = "upstream_error"
	// ReasonEmptyStream is a stream that ended ([DONE] or close)
	// without ever emitting real content: the attempt served
	// nothing, so the chain moves on — pre-commit only, decided by
	// the preread buffer (research-log entries 97 + 111).
	ReasonEmptyStream = "empty_stream"
	// ReasonInvalidResponse is a malformed upstream envelope
	// (no choices, unparseable body): retryable before the
	// response is committed, per entry 183.
	ReasonInvalidResponse = "invalid_response"
	// ReasonTransportError is an untyped failure from a real
	// client call (connect/read/request failure): retryable by
	// definition.
	ReasonTransportError = "transport_error"
	// ReasonPanicRecovered is a panic recovered at a request/stream
	// boundary (CHECKLIST 165): a provider client, a chunk
	// converter or a hook panicked and the gateway recovered
	// instead of dying. The attempt failed over (or ended the
	// stream) like any other retryable failure; provider.ClassifyError
	// does not know this type (it cannot import router), so the
	// router sets this reason directly in addTraceAttemptPreread.
	ReasonPanicRecovered = "panic_recovered"
	// ReasonTimeout is a per-attempt timeout (408, or a client
	// timeout): the attempt failed, the request may still serve
	// elsewhere.
	ReasonTimeout = "timeout"
	// ReasonRequestInvalid is a 400 with no model-not-found
	// signal (malformed request, bad params, invalid fields),
	// a 422, or a non-retryable mid-stream failure: it stops
	// the chain.
	ReasonRequestInvalid = "request_invalid"
	// ReasonAuthFailed is a 401: the key fails identically on
	// every target of that provider, so the chain stops.
	ReasonAuthFailed = "auth_failed"
	// ReasonForbidden is a 403: the request is refused for this
	// caller, not something another target fixes.
	ReasonForbidden = "forbidden"
	// ReasonContentRefused is an upstream content-policy / safety
	// refusal (CHECKLIST 161): the target refused the content
	// itself, not the request shape and not its own health. It
	// falls back — but only onto the refusal chain (see
	// internal/router: never the same target again, never a
	// breaker/cooldown/health signal), and an exhausted refusal
	// chain ends in an honest 403 with the policy-refusal code
	// -32001, never a fake success and never a 502.
	ReasonContentRefused = "content_refused"
	// ReasonEndpointUnsupported is a 405/501: the target lacks
	// that endpoint. It falls back only on /v1/responses and
	// /v1/embeddings; on chat completions it stops the chain.
	ReasonEndpointUnsupported = "endpoint_unsupported"
	// ReasonUnrepresentable is a request the wire cannot
	// represent on that target (e.g. temperature > 1 on
	// Anthropic): it stops the chain.
	ReasonUnrepresentable = "unrepresentable"
	// ReasonReasoningUnrepresentable is a hop whose restored
	// reasoning state the target wire cannot represent (e.g.
	// Anthropic thinking without a signature, or any reasoning
	// state on Cohere): it falls back to a target that can
	// carry the reasoning, and only fails the request when no
	// target can (CHECKLIST 163).
	ReasonReasoningUnrepresentable = "reasoning_unrepresentable"
	// ReasonCancelled is a caller cancellation: never falls back.
	ReasonCancelled = "cancelled"
	// ReasonConcurrencyLimited is an adaptive-limiter shed: the
	// target was full, so the chain falls back to the next
	// target (DecisionFallback) — only when every target sheds
	// does the request fail with ErrLimited.
	ReasonConcurrencyLimited = "concurrency_limited"
)

// EmptyStreamError is returned when an upstream SSE stream ends
// without ever emitting real content (no text delta, no tool-call
// delta). FinishReason is the last finish reason the stream
// carried, "" when it sent none (e.g. a bare [DONE]).
type EmptyStreamError struct {
	FinishReason string
}

func (e *EmptyStreamError) Error() string {
	if e == nil {
		return "upstream stream ended with no content"
	}
	if e.FinishReason != "" {
		return "upstream stream ended with no content (finish_reason: " + e.FinishReason + ")"
	}
	return "upstream stream ended with no content"
}

// IsModelNotFoundSignal reports whether an error's type/status/message
// text signals that the model does not exist at that target. It is the
// entry-151 distinction: a 400 carrying this signal falls back, a 400
// without it stops. The canonical phrase list lives in
// internal/wire (IsModelNotFoundSignal) — wire cannot import provider,
// so the matcher lives there and this package reuses it rather than
// duplicating the list.
func IsModelNotFoundSignal(text string) bool {
	return wire.IsModelNotFoundSignal(text)
}

// IsContentRefusalSignal reports whether an error's type/status/
// message text signals that the target refused the content on
// policy/safety grounds (CHECKLIST 161). The canonical phrase
// list lives in internal/wire (IsContentRefusalSignal); this
// package reuses it rather than duplicating it.
func IsContentRefusalSignal(text string) bool {
	return wire.IsContentRefusalSignal(text)
}

// contentRefusalClassification is the taxonomy verdict for an
// upstream content-policy / safety refusal: fall back onto the
// refusal chain (ContentRefusal tells the router that), with the
// upstream status when one is known.
func contentRefusalClassification(status int) Classification {
	return Classification{Decision: DecisionFallback, Reason: ReasonContentRefused, Status: status, ContentRefusal: true}
}

// reasonForStatus names the reason slug for a bare HTTP status
// (used for the non-400/404 statuses, where the body signal does
// not change the verdict).
func reasonForStatus(status int) string {
	switch status {
	case 429:
		return ReasonRateLimited
	case 408:
		return ReasonTimeout
	case 529:
		return ReasonOverloaded
	case 402:
		return ReasonQuotaExhausted
	case 401:
		return ReasonAuthFailed
	case 403:
		return ReasonForbidden
	case 405, 501:
		return ReasonEndpointUnsupported
	case 0:
		return ReasonInvalidResponse
	}
	if status >= 500 {
		return ReasonUpstreamError
	}
	return ReasonRequestInvalid
}

// classifyStatus applies the shared status table
// (wire.RetryableStatusForEndpoint) plus the 400/404 body-signal
// layer, for one upstream HTTP status and the error text that came
// with it.
func classifyStatus(status int, text string, endpoint EndpointKind) Classification {
	// Content-policy refusal (CHECKLIST 161): a 400/403/422 whose
	// body signals the content was refused is NOT a malformed
	// request and NOT a forbidden caller — it is a refusal of
	// this content at this target, so it falls back onto the
	// refusal chain. This check comes before the 400 split and
	// the status table, because both would otherwise stop the
	// chain (400 → request_invalid, 403 → forbidden) and the
	// denial would never reach a target that accepts the
	// content. A 403 with no content signal still stops: the
	// caller is refused, not the content.
	if (status == 400 || status == 403 || status == 422) && IsContentRefusalSignal(text) {
		return contentRefusalClassification(status)
	}
	// 400 is the entry-151 split: model-not-found falls back,
	// anything else stops. The status table alone says 400 is
	// never retryable, so this check must come first.
	if status == 400 {
		if IsModelNotFoundSignal(text) {
			return Classification{Decision: DecisionFallback, Reason: ReasonModelNotFound, Status: status}
		}
		return Classification{Decision: DecisionStop, Reason: ReasonRequestInvalid, Status: status}
	}
	// 404 always falls back (a 404 is a property of that
	// target), but the reason says honestly whether the body
	// actually named a missing model.
	if status == 404 {
		if IsModelNotFoundSignal(text) {
			return Classification{Decision: DecisionFallback, Reason: ReasonModelNotFound, Status: status}
		}
		return Classification{Decision: DecisionFallback, Reason: ReasonNotFound, Status: status}
	}
	if wire.RetryableStatusForEndpoint(status, endpoint.wireName()) {
		return Classification{Decision: DecisionFallback, Reason: reasonForStatus(status), Status: status}
	}
	return Classification{Decision: DecisionStop, Reason: reasonForStatus(status), Status: status}
}

// wireErrorInfo extracts the status, the signal text (type +
// message) and the mapper's retryable flag from one of the four
// native-wire error types. ok is false when err is not a wire
// error (errors.As unwraps, so wrapped wire errors count).
func wireErrorInfo(err error) (status int, text string, retryable bool, ok bool) {
	var ae *wire.AnthropicError
	if errors.As(err, &ae) {
		return ae.StatusCode, ae.Type + " " + ae.Message, ae.Retryable, true
	}
	var ge *wire.GeminiError
	if errors.As(err, &ge) {
		return ge.StatusCode, ge.Status + " " + ge.Message, ge.Retryable, true
	}
	var ce *wire.CohereError
	if errors.As(err, &ce) {
		return ce.StatusCode, ce.Message, ce.Retryable, true
	}
	var be *wire.BedrockError
	if errors.As(err, &be) {
		return be.StatusCode, be.Type + " " + be.Message, be.Retryable, true
	}
	return 0, "", false, false
}

// ClassifyError decides, for one failed attempt, whether the router
// should try the next target (DecisionFallback) or stop the chain
// and report the failure (DecisionStop). endpoint is the API
// surface the request was made to; it matters only for the 405/501
// rule.
//
// Precedence: caller cancellation first (never fall back on it),
// then the typed errors (rate limit, quota, upstream, wire),
// then a per-attempt timeout, then untyped transport failures
// (fail open: an untyped error from a real client call is a
// connect/read failure, retryable by definition). A nil error
// classifies as Stop with reason "none" — there is nothing to fall
// back from.
func ClassifyError(err error, endpoint EndpointKind) Classification {
	if err == nil {
		return Classification{Decision: DecisionStop, Reason: "none", Status: 0}
	}
	if errors.Is(err, context.Canceled) {
		return Classification{Decision: DecisionStop, Reason: ReasonCancelled, Status: 0}
	}
	if errors.Is(err, adaptive.ErrLimited) {
		// A limiter shed is per-target capacity, not a property
		// of the request: the target is full, another target may
		// serve. It must fall back, never stop the chain —
		// classifying it Stop made ChatWithChainStrategy break
		// out of the attempt loop on the first shed (via
		// isTerminalErr) and return "adaptive concurrency limit
		// reached" while a healthy provider stood idle
		// (CHECKLIST 189). When every target sheds, the loop
		// still ends with lastErr == ErrLimited, so the
		// all-at-cap behaviour is unchanged.
		return Classification{Decision: DecisionFallback, Reason: ReasonConcurrencyLimited, Status: 0}
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return Classification{Decision: DecisionFallback, Reason: ReasonRateLimited, Status: 429}
	}
	var cre *ContentRefusalError
	if errors.As(err, &cre) {
		// Typed refusal (a finish_reason of "content_filter",
		// a blocked prompt): the content was refused, so it
		// falls back onto the refusal chain — never the same
		// target, never a health signal.
		return contentRefusalClassification(cre.StatusCode)
	}
	var qe *QuotaError
	if errors.As(err, &qe) {
		// Billing/quota is a property of that target's
		// key/account: another target may serve. (The soft
		// cooldown only demotes 402s; fallback is broader —
		// trying the next target once is not a demotion.)
		return Classification{Decision: DecisionFallback, Reason: ReasonQuotaExhausted, Status: qe.StatusCode}
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// StatusCode 0 is a malformed upstream envelope (no
		// choices, unparseable body): retryable before the
		// response is committed, per entry 183.
		if ue.StatusCode == 0 {
			return Classification{Decision: DecisionFallback, Reason: ReasonInvalidResponse, Status: 0}
		}
		// A client-synthesised status-200 error is not an HTTP
		// verdict at all: the client already decided whether it
		// is retryable (e.g. "SSE stream ended without [DONE]"
		// with no content is retryable), so honour that instead
		// of the status table, which only knows real HTTP
		// statuses. A 200 error the client marked non-retryable
		// (malformed SSE) still stops the chain.
		if ue.StatusCode == http.StatusOK && ue.Retryable {
			return Classification{Decision: DecisionFallback, Reason: ReasonInvalidResponse, Status: http.StatusOK}
		}
		return classifyStatus(ue.StatusCode, ue.Body, endpoint)
	}
	var ese *EmptyStreamError
	if errors.As(err, &ese) {
		// The preread buffer's verdict: the stream ended with
		// no content before commit, so the attempt served
		// nothing and the next target may serve the request.
		return Classification{Decision: DecisionFallback, Reason: ReasonEmptyStream, Status: 0}
	}
	if st, text, retryable, ok := wireErrorInfo(err); ok {
		if st != 0 {
			return classifyStatus(st, text, endpoint)
		}
		// Mid-stream wire error (the HTTP status was already
		// 200): no status to consult, so the message decides.
		// A content-policy signal refuses the content (onto
		// the refusal chain); a model signal falls back;
		// otherwise the mapper's own retryable flag decides.
		if IsContentRefusalSignal(text) {
			return contentRefusalClassification(0)
		}
		if IsModelNotFoundSignal(text) {
			return Classification{Decision: DecisionFallback, Reason: ReasonModelNotFound, Status: 0}
		}
		if retryable {
			return Classification{Decision: DecisionFallback, Reason: ReasonUpstreamError, Status: 0}
		}
		return Classification{Decision: DecisionStop, Reason: ReasonRequestInvalid, Status: 0}
	}
	var unrep *wire.UnrepresentableError
	if errors.As(err, &unrep) {
		return Classification{Decision: DecisionStop, Reason: ReasonUnrepresentable, Status: 0}
	}
	var runrep *reasoning.UnrepresentableError
	if errors.As(err, &runrep) {
		// Reasoning state the target wire cannot represent:
		// a property of that target, not of the request — the
		// next target may carry it, so fall back. When no
		// target can, the engine ends the request with an
		// honest 400 (see internal/server handleEngineError).
		return Classification{Decision: DecisionFallback, Reason: ReasonReasoningUnrepresentable, Status: 0}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A per-attempt timeout (e.g. the client's own
		// timeout on a hanging upstream). The engine's own
		// deadline budget is guarded by the caller's ctx check
		// at the top of the attempt loop, so a budget expiry
		// never reaches a second attempt through here.
		return Classification{Decision: DecisionFallback, Reason: ReasonTimeout, Status: 0}
	}
	// Untyped error from a real client call: a transport
	// failure (connect/read/request). Fail open — today's
	// router falls back on these, and a transport failure says
	// nothing about the request.
	return Classification{Decision: DecisionFallback, Reason: ReasonTransportError, Status: 0}
}
