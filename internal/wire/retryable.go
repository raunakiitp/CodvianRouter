// Shared retry taxonomy (research-log entry 183, SparkRouter).
//
// One function decides, for a bare HTTP status, whether the failure
// is a property of *that target* — retryable on another target —
// rather than a property of the request (which fails identically
// everywhere and must stop the chain). The wire error mappers
// (Anthropic, Gemini, Cohere, Bedrock) and the OpenAI-compatible
// client in internal/provider all consult it, so the four wires
// cannot drift apart on the same status again.
//
// Retryable: 408 (timeout), 429 (rate limit — the client maps it to
// RateLimitError first, so this is defensive here), 500/502/503/504,
// 529 (Anthropic/Cloudflare overload), 402 (payment required —
// billing/quota is a property of that target's key/account, so
// another target may serve) and 404 (model not found on that
// target). 405/501 are retryable ONLY on /v1/responses,
// /v1/embeddings, /v1/rerank and /v1/images (the target lacks that
// endpoint; another target may offer it) — on chat completions they
// are a client error and stop the chain.
//
// Not retryable: 400, 401, 403, 422 and everything else. 400/403/422
// stop the chain (research-log entries 3/26); 401 stays
// non-retryable — a bad key fails identically on every target of
// that provider, and retrying it only burns attempts. The one
// exception to "400 never retries" is entry 151: a 400 whose body
// signals model-not-found (see IsModelNotFoundSignal) IS retryable
// — the model is missing at that target, not the request malformed.
// That check lives in the mappers and in internal/provider, not in
// the bare status table, because it needs the body.
package wire

import (
	"net/http"
	"strings"
)

// modelNotFoundPhrases are the (lowercased) substrings that mark an
// upstream error's type/message as "this model does not exist at
// this target". They cover the shapes the providers actually emit:
//
//   - OpenAI: "The model `gpt-x` does not exist"
//   - Anthropic: type "not_found_error", "model: claude-x not found"
//   - Gemini: "models/gemini-x is not found" (404 NOT_FOUND)
//   - Bedrock: "The provided model identifier is invalid."
//
// The generic rule is "model" + a not-found phrase. "unknown model"
// is matched as a phrase, with one exclusion: "unknown model
// parameter" is a malformed-request error about an unknown
// parameter, not a missing model, and must NOT fall back.
var modelNotFoundPhrases = []string{
	"model_not_found",
	"model not found",
	"model_not_exist",
	"model does not exist",
	"model doesn't exist",
	"model identifier is invalid",
	"specified model",
	"unknown model",
	"no such model",
	"not found",
	"not_found",
	"does not exist",
	"doesn't exist",
}

// IsModelNotFoundSignal reports whether text (an upstream error
// body, message, or error type + message) signals that the model
// does not exist at that target. It is the entry-151 distinction:
// a 400 carrying this signal falls back, a 400 without it stops.
// This is the canonical phrase list: internal/provider reuses it
// (provider.IsModelNotFoundSignal) instead of duplicating it.
func IsModelNotFoundSignal(text string) bool {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "model") {
		return false
	}
	if strings.Contains(lower, "unknown model parameter") {
		// A malformed-request error about an unknown parameter,
		// not a missing model — but only that phrase is
		// excluded; any other not-found phrase still counts.
		lower = strings.ReplaceAll(lower, "unknown model parameter", "")
		if !strings.Contains(lower, "model") {
			return false
		}
	}
	for _, phrase := range modelNotFoundPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// contentRefusalPhrases are the (lowercased) substrings that mark an
// upstream error's type/message as "this target refused the content
// on policy/safety grounds" (CHECKLIST 161). They cover the shapes
// the providers actually emit:
//
//   - OpenAI: 400 code "content_filter" / "moderation", "content
//     filtered due to policy"
//   - Anthropic: 400 "content policy" violations
//   - Gemini: 400 "blocked ... safety", promptFeedback blockReason
//     SAFETY / OTHER (surfaced as a finish_reason, not an error)
//   - Bedrock: 400 guardrail_intervened / "guardrail" blocks
//   - Portkey / LiteLLM gateways: the same upstream text, passed
//     through with the gateway's own envelope
//
// The phrases are deliberately multi-word where the single word
// would be ambiguous ("safety" alone appears in benign text, so
// only "safety filter", "safety reasons", "blocked by safety" and
// the like match). A match says the *content* was refused —
// nothing about the provider being down — so the taxonomy routes
// it to the refusal chain, never to the breaker.
var contentRefusalPhrases = []string{
	"content_filter",
	"content filter",
	"content_filtered",
	"content policy",
	"content_policy",
	"content-policy",
	"content violation",
	"content_violation",
	"content blocked",
	"content_blocked",
	"content refused",
	"content safety",
	"policy violation",
	"policy_violation",
	"violates our content policy",
	"violates content policy",
	"violates the content policy",
	"violates policy",
	"blocked by safety",
	"blocked for safety",
	"safety filter",
	"safety reasons",
	"safety settings",
	"safety violation",
	"harmful content",
	"inappropriate content",
	"prohibited content",
	"disallowed content",
	"not allowed content",
	"guardrail intervened",
	"guardrail_intervened",
	"guardrail blocked",
	"guardrail violation",
	"moderation",
	"content moderation",
	"refused due to content",
	"refusal: content",
	"finish_reason content_filter",
}

// IsContentRefusalSignal reports whether text (an upstream error
// body, message, or error type + message) signals that the target
// refused the content on policy/safety grounds. This is the
// canonical phrase list: internal/provider reuses it
// (provider.IsContentRefusalSignal) instead of duplicating it.
func IsContentRefusalSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range contentRefusalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// IsQuotaExhaustedSignal reports whether text (an upstream error body,
// message, or error type + message) signals that the account is out of
// quota / credits — the reference router failureClassification.ts:26 QUOTA_MESSAGE
// (`quota|insufficient balance|credits exhausted|balance is \$0`), plus
// the "insufficient_quota" / "out of credits" / "no credits" markers
// the provider package already treats as quota (see isQuotaBody in
// internal/provider/client.go). A 429 carrying this signal is
// quota_exhausted: the account cannot serve, so the router fails over
// and never retries the same target (M4.4). A plain 429 without it is
// an ordinary rate limit.
func IsQuotaExhaustedSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range []string{
		"quota",
		"insufficient balance",
		"credits exhausted",
		"balance is $0",
		"insufficient_quota",
		"out of credits",
		"no credits",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// concurrencyLimitPhrases are the (lowercased) substrings that mark an
// upstream 429 as a concurrency-limit rejection rather than a rate
// limit — the reference router concurrencyLimit429.ts: the provider is refusing
// because too many requests are in flight at once, not because the
// quota window is full. Such a 429 fails over now, cools the target
// for 1 s, never climbs the backoff ladder, and never counts toward
// the breaker (M4.4).
var concurrencyLimitPhrases = []string{
	"concurrency limit",
	"concurrency_limit",
	"too many concurrent",
	"concurrent request",
	"concurrent connection",
	"concurrency exceeded",
	"max concurrency",
	"too many simultaneous",
}

// IsConcurrencyLimitSignal reports whether text signals a
// concurrency-limit 429 (see concurrencyLimitPhrases). "concurrent"
// alone is deliberately NOT a match: it appears in benign text
// ("concurrent users"), so only the multi-word limit shapes match.
func IsConcurrencyLimitSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range concurrencyLimitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// RetryableStatus reports whether a bare HTTP status is retryable
// on another target, for the chat-completions surface (where 405/501
// are client errors). Endpoints that a target may simply not offer
// use RetryableStatusForEndpoint instead.
func RetryableStatus(status int) bool {
	return RetryableStatusForEndpoint(status, "")
}

// RetryableStatusForEndpoint is RetryableStatus plus the
// endpoint-scoped rule: 405/501 are retryable only when endpoint
// names /v1/responses, /v1/embeddings, /v1/rerank or /v1/images.
// endpoint may be "" (chat completions or an unknown surface),
// "responses", "embeddings", "rerank", "images", or a full path
// such as "/v1/responses".
func RetryableStatusForEndpoint(status int, endpoint string) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529,                            // overload (Anthropic/Cloudflare)
		http.StatusPaymentRequired,     // 402 — that target's billing/quota
		http.StatusNotFound:            // 404 — that target lacks the model
		return true
	case http.StatusMethodNotAllowed, // 405
		http.StatusNotImplemented: // 501
		ep := strings.TrimPrefix(strings.TrimSpace(endpoint), "/")
		ep = strings.TrimPrefix(ep, "v1/")
		return ep == "responses" || ep == "embeddings" || ep == "rerank" || ep == "images"
	}
	return false
}
