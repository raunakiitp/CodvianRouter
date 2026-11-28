// Poison guards on every cache store path (Stream D, semantic-cache
// area).
//
// A response that the upstream marked as refused, truncated, or
// tool-call-carrying, or that carries no content at all, is not an
// answer: storing it would replay that non-answer to every later
// request that keys or embeds close to this one, and the cache would
// keep serving it until the entry's TTL. cache.ShouldStore already
// refuses tool-call-carrying and finish_reason="length" responses;
// StoreGuard.Poison is the stricter gate the store sites now run, and
// it adds the refusal finish reasons (a content-policy refusal is a
// property of the provider/model/region, not of the prompt, so it
// must never be cached as if it were the answer), the other truncation
// spellings a passthrough wire may pass through un-normalised, and
// empty-content responses (a provider hiccup must not become a
// permanent blank answer).
//
// The guard runs at the store sites (degrade.StaleEngine.store and
// server.storeMessagesCache), which are the only writers of the exact
// and semantic layers, so no path can smuggle a poisoned entry in.
package cache

import "strings"

// StorePoison names why an upstream response must never enter a cache
// layer. The zero value means "safe to store".
type StorePoison string

const (
	// StorePoisonNone means the response may be cached.
	StorePoisonNone StorePoison = ""
	// StorePoisonRefusal: the upstream refused the content (content
	// policy / safety filter), reported through the finish reason.
	StorePoisonRefusal StorePoison = "refusal"
	// StorePoisonTruncated: the response was cut off (max_tokens and
	// friends), so it is incomplete.
	StorePoisonTruncated StorePoison = "truncated"
	// StorePoisonToolCalls: the response carries tool calls, which the
	// cache layers cannot replay faithfully.
	StorePoisonToolCalls StorePoison = "tool_calls"
	// StorePoisonEmpty: no content and no tool calls - nothing to
	// serve, and caching a blank would replay the hiccup.
	StorePoisonEmpty StorePoison = "empty"
	// StorePoisonTemperature: temperature > 0 without the per-key
	// allowance; the answer is not deterministic.
	StorePoisonTemperature StorePoison = "temperature"
)

// String returns the reason label (used as a metrics label value).
func (p StorePoison) String() string { return string(p) }

// StoreGuard is everything a store site knows about one upstream
// response when it decides whether to cache it.
type StoreGuard struct {
	// FinishReason is the upstream finish reason, as the wire mapping
	// left it (empty when the provider reported none).
	FinishReason string
	// Content is the response's text content.
	Content string
	// Temperature is the request's temperature.
	Temperature float64
	// AllowNonZeroTemp is the per-key allowance for temperature > 0.
	AllowNonZeroTemp bool
	// HasToolCalls reports whether the response carries tool calls.
	HasToolCalls bool
}

// Poison returns the reason this response must not be cached, or
// StorePoisonNone when it is safe to store. The checks run in a fixed
// order so the reported reason names the real cause: a refused or
// truncated response usually also has empty content, and the refusal
// / truncation is the more useful label.
func (g StoreGuard) Poison() StorePoison {
	if g.HasToolCalls {
		return StorePoisonToolCalls
	}
	if IsRefusalFinish(g.FinishReason) {
		return StorePoisonRefusal
	}
	if IsTruncatedFinish(g.FinishReason) {
		return StorePoisonTruncated
	}
	if strings.TrimSpace(g.Content) == "" {
		return StorePoisonEmpty
	}
	if !CacheableTemperature(g.Temperature, g.AllowNonZeroTemp) {
		return StorePoisonTemperature
	}
	return StorePoisonNone
}

// normalizeFinish lower-cases and trims a finish reason and strips the
// punctuation some wires add, so "Content_Filter", "content-filter"
// and "content_filter" all compare equal.
func normalizeFinish(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	r = strings.ReplaceAll(r, "-", "_")
	r = strings.ReplaceAll(r, " ", "_")
	return r
}

// IsRefusalFinish reports whether a finish reason marks a content-policy
// / safety refusal. The set covers the spellings the shipped wire
// mappers can emit verbatim on a passthrough (OpenAI-compatible)
// surface - "content_filter" (OpenAI/Gemini), "refusal" (Anthropic's
// stop_reason, kept when a passthrough echoes it) - plus the
// guardrail/safety labels the native wires report before mapping.
// A refusal is a property of the provider/model/region that tripped
// the filter, not of the prompt, so it must never be cached as the
// answer to anything.
func IsRefusalFinish(reason string) bool {
	switch normalizeFinish(reason) {
	case "content_filter", "content_filtered", "refusal", "input_refusal",
		"output_refusal", "guardrail_intervened", "safety", "recitation",
		"blocked", "moderation":
		return true
	}
	return false
}

// IsTruncatedFinish reports whether a finish reason means the response
// was cut off before it was complete. "length" is the normalised form
// the wire mappers produce; the other spellings are what a passthrough
// upstream may send un-normalised, and ShouldStore's pinned matrix
// only refuses "length", so the store-site guard refuses all of them.
func IsTruncatedFinish(reason string) bool {
	switch normalizeFinish(reason) {
	case "length", "max_tokens", "max_output_tokens", "truncated",
		"model_context_window_exceeded", "context_length_exceeded",
		"token_limit_reached", "max_completion_tokens":
		return true
	}
	return false
}
