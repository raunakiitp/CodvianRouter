package provider

// Prompt-cache affinity: the router-generated prompt_cache_key for
// OpenAI's own API (research-log entry 184, SparkRouter).
//
// OpenAI's prompt_cache_key steers which cache node serves a request,
// so repeat prompts with the same shared prefix land on the warm node.
// The router adds one automatically, per attempt, only when ALL of
// these hold:
//
//   - the target is OpenAI's own API (host api.openai.com) speaking
//     the OpenAI protocol — other OpenAI-compatible endpoints never
//     get a generated key (some reject unknown fields);
//   - the client did NOT supply a prompt_cache_key — a client key,
//     including an explicit JSON null, is never replaced;
//   - the caller is identified (a gateway-key ID is known);
//   - the request has a shared prefix: leading system/developer
//     messages, or tools. Requests with no instructions and no tools
//     get no key.
//
// The key is "codvian-" + a keyed HMAC of the caller (gateway-key ID
// + endpoint) and the request's shared prefix. User messages are NOT
// part of it, so every conversation of one agent gets the same key
// and different agents/callers do not.
//
// The key only influences OpenAI's cache placement. It is generated
// per attempt at send time, never used for routing, and never stored:
// it lives only in the wire body of the upstream request — never in
// the ledger, history, route trace, or any dashboard feed (the
// ChatRequest fields that carry the caller identity and the client's
// own key are json:"-" for exactly this reason).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
)

// CacheKeyEndpointChatCompletions is the endpoint label mixed into the auto
// cache key for /v1/chat/completions. A future /v1/responses path
// would use "responses" so the two endpoints never share a key.
const CacheKeyEndpointChatCompletions = "chat/completions"

// cacheKeyPrefix is the literal prefix of every router-generated key.
const cacheKeyPrefix = "codvian-"

// IsOpenAIAPI reports whether baseURL is OpenAI's own API. The
// comparison is on the URL host only, so api.openai.com:443,
// https://api.openai.com/v1 and a trailing slash all match, while
// lookalikes (api.openai.com.evil.com, notapi.openai.com) do not.
func IsOpenAIAPI(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.openai.com")
}

// sharedPrefix is the canonical form of a request's shared prefix:
// the leading consecutive system/developer messages, plus the tool
// definitions. Encoding a struct (fixed field order, no maps) makes
// the bytes deterministic, and re-marshalling the decoded messages
// normalises JSON key ordering — two requests with the same prefix
// in different key order produce the same bytes.
type sharedPrefix struct {
	Messages []Message        `json:"messages,omitempty"`
	Tools    []ToolDefinition `json:"tools,omitempty"`
}

// sharedPrefixJSON returns the canonical bytes of req's shared prefix
// and whether there is one at all: false when there are no leading
// system/developer messages AND no tools.
func sharedPrefixJSON(req ChatRequest) ([]byte, bool) {
	var prefix sharedPrefix
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "developer" {
			break
		}
		prefix.Messages = append(prefix.Messages, m)
	}
	prefix.Tools = req.Tools
	if len(prefix.Messages) == 0 && len(prefix.Tools) == 0 {
		return nil, false
	}
	b, err := json.Marshal(prefix)
	if err != nil {
		return nil, false
	}
	return b, true
}

// AutoPromptCacheKey derives the router-generated prompt_cache_key
// for one request. callerID is the gateway-key ID the request
// arrived under, endpoint the API endpoint label
// (CacheKeyEndpointChatCompletions). It returns false when there is no
// caller to key on or no shared prefix to anchor on — the caller
// then sends no key at all, rather than a key that would mix
// callers or anchor on nothing.
func AutoPromptCacheKey(callerID, endpoint string, req ChatRequest) (string, bool) {
	if callerID == "" || endpoint == "" {
		return "", false
	}
	prefix, ok := sharedPrefixJSON(req)
	if !ok {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(callerID))
	_, _ = mac.Write([]byte(endpoint))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write(prefix)
	return cacheKeyPrefix + hex.EncodeToString(mac.Sum(nil)), true
}
