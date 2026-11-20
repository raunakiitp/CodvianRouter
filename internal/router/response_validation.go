// 200-OK body validation predicate → failover (G11 Area 2, parity tracker
// Area 2 row "200-OK body validation predicate → failover").
//
// the reference router's evidence is the per-combo `responseValidation` predicate
// (comboConfig.ts responseValidationSchema): forbiddenSubstrings,
// requiredSubstrings, minContentLength, jsonPathPredicates. A 200 OK
// whose body fails the predicate fails over to the next target, on the
// same path as an HTTP error.
//
// Ours is our own implementation of the same rule, in the one place
// every non-streaming attempt passes through (callChat, router.go):
// the body is validated BEFORE the response is committed to the
// client, so a failing body fails over exactly like a failed attempt.
// Streaming keeps the preread holdback path (preread.go) — a stream
// is validated by its leading events, not by a buffered body.
//
// The built-in predicate (always on unless Disabled) catches the
// error payloads providers actually smuggle inside a 200:
//   - a top-level JSON "error" object/string (e.g. a provider that
//     answers HTTP 200 with {"error": {"message": ...}}), and
//   - a top-level JSON "choices": [] (an empty choices array).
//
// An empty body with no tool calls is already a failover via
// isEmptyCompletion (callChat); this predicate does not duplicate it.
//
// The operator-configured fields mirror the reference router's schema names
// (forbidden/required substrings, minimum content length). The
// jsonPathPredicates arm of the reference router's schema is deliberately NOT
// implemented: a JSON-path predicate language is a new mini-DSL
// with its own parsing and error surface, and the built-in
// error-payload + substring rules cover the failure modes a 200
// body actually exhibits. Recorded as a deliberate divergence in
// the G11 report.
//
// A failing body is reported as *provider.UpstreamError with
// StatusCode 0 and Retryable true — the taxonomy's existing verdict
// for "a malformed upstream envelope (no choices, unparseable body):
// retryable before the response is committed" (ClassifyError,
// internal/provider/taxonomy.go). That makes the failure
// non-terminal (the chain continues), retryable (soft-demote mode
// demotes the target), and breaker-counted under reason
// "invalid_response" (a provider serving error payloads as 200s is
// unhealthy — the same reason the taxonomy already uses for
// status-0 envelopes). No new error type, no new taxonomy rule.
package router

import (
	"encoding/json"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ResponseValidationConfig is the Go-level surface for the 200-OK
// body validation predicate (G11 Area 2). The zero value is the
// built-in error-payload predicate, enabled. Config-file wiring
// (routing.response_validation) is the coordinator's — see the G11
// report for the verbatim wiring.
type ResponseValidationConfig struct {
	// Disabled turns the whole predicate off: a 200 body is
	// accepted exactly as before (only the empty-completion
	// rule in callChat still applies).
	Disabled bool
	// ForbiddenSubstrings fails the body when any of them
	// appears in the response content (the reference router's
	// forbiddenSubstrings).
	ForbiddenSubstrings []string
	// RequiredSubstrings fails the body when any of them is
	// missing from the response content (the reference router's
	// requiredSubstrings).
	RequiredSubstrings []string
	// MinContentLength fails the body when the response content
	// is shorter than this (the reference router's minContentLength).
	// 0 disables the length check.
	MinContentLength int
}

// ValidateResponseBody reports whether resp's body passes the
// predicate. A nil response passes (nothing to validate); a
// response with tool calls passes the JSON error-payload check —
// the tool calls ARE content, so a body that carries them served
// something even when its text looks odd. The returned error is
// *provider.UpstreamError{StatusCode: 0, Retryable: true} (see the
// file header); nil means the body is acceptable.
func ValidateResponseBody(resp *provider.ChatResponse, cfg ResponseValidationConfig) error {
	if resp == nil || cfg.Disabled {
		return nil
	}
	content := resp.Content
	if reason, bad := invalidBodyReason(content, len(resp.ToolCalls) > 0, cfg); bad {
		return &provider.UpstreamError{
			StatusCode: 0,
			Retryable:  true,
			Body:       "router: 200 response failed body validation: " + reason,
		}
	}
	return nil
}

// invalidBodyReason runs the checks in order and names the first
// failure. hasToolCalls exempts the JSON error-payload check only;
// the operator-configured substring/length checks still apply to
// the text itself.
func invalidBodyReason(content string, hasToolCalls bool, cfg ResponseValidationConfig) (string, bool) {
	if !hasToolCalls && content != "" {
		if reason, bad := invalidJSONBodyReason(content); bad {
			return reason, true
		}
	}
	for _, sub := range cfg.ForbiddenSubstrings {
		if sub != "" && strings.Contains(content, sub) {
			return "response contains forbidden substring " + quoteForValidation(sub), true
		}
	}
	for _, sub := range cfg.RequiredSubstrings {
		if sub != "" && !strings.Contains(content, sub) {
			return "response is missing required substring " + quoteForValidation(sub), true
		}
	}
	if cfg.MinContentLength > 0 && len(content) < cfg.MinContentLength {
		return "response content is shorter than the configured minimum", true
	}
	return "", false
}

// invalidJSONBodyReason checks a JSON-looking body for the
// error-payload shapes: a top-level "error" member that is not
// null/empty, and a top-level "choices" member that is an empty
// array. Non-object bodies pass (a plain-text completion is never
// an error payload); an unparseable "{"-prefixed body also passes
// — garbage that is not even JSON is the wire's problem, not this
// predicate's, and failing over on it would mask wire bugs as
// provider failures.
func invalidJSONBodyReason(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", false
	}
	if raw, ok := obj["error"]; ok {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil && v != nil {
			if s, isStr := v.(string); !isStr || s != "" {
				return "body carries a top-level error payload", true
			}
		}
	}
	if raw, ok := obj["choices"]; ok {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) == 0 {
			return "body carries an empty choices array", true
		}
	}
	return "", false
}

// quoteForValidation quotes a substring for an error message
// without importing strconv for one call site.
func quoteForValidation(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
