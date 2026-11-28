// M1.4 — tenant-scoped cache key, temperature gating, truncated-response
// guard, and the X-Codvian-Cache header contract.
//
// The pre-M1 exact key was SHA-256 of {model, messages} only, so one
// tenant's cached answer could be served to another, a temperature-1.0
// answer could be served to a temperature-0 request, and a truncated
// (finish_reason=length) answer was stored as if complete. ScopedKey
// is the replacement, used on all live paths (internal/degrade,
// internal/server relay). The legacy ExactKey(model, messages) is
// retained only as the unscoped primitive — its only remaining
// callers are tests (internal/cache/cache_test.go); there are no
// live call sites left to migrate.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// KeyInput is everything that decides whether two chat requests may
// share one cached answer. Fields are deliberately generic (any) so
// this package stays free of the provider package: the server/degrade
// call sites map provider.ChatRequest onto KeyInput.
//
// DECISION (M6.12) — per-provider isolation is deliberately NOT
// implemented; there is intentionally no Provider field here, and
// the parity tracker Area 6 row "Per-provider isolation" is a DIVERGENCE
// from the sector baseline (default-on cacheByProvider: true), not a gap to
// close by adding a field. Evidence, all verified 2026-11-18:
//
//   - No decision record existed in internal log, internal plan or
//     git history before M6.12 (PLAN.md M6.12 itself records the
//     absence). The sharing rationale exists only in code comments
//     (internal/cache/cache.go, internal/degrade/degrade.go and
//     internal/server/relay.go), coined in Sprint 9 commit 5413096
//     (CHECKLIST 187, semantic-cache model-scoping fix). This
//     comment is the decision record.
//
//   - The provider is genuinely UNKNOWN at key-derivation time.
//     provider.ChatRequest (internal/provider/client.go) carries
//     no Provider field at all. The exact lookup runs BEFORE
//     routing picks a provider: degrade.StaleEngine.Chat calls
//     lookup() (which derives scopedKey) before e.engine.Chat(),
//     and Engine.Chat (internal/router/router.go) builds its
//     fallback chain inside, choosing the provider per attempt
//     with failover — so the serving provider may differ from
//     any pre-routing prediction, and can change mid-request.
//     The /v1/messages relay resolves only the FIRST enabled
//     provider for a model (Registry.RelayTargetForModel), not
//     the provider that will serve, and a cache hit there is
//     served without calling any provider at all. A Provider
//     field on KeyInput would therefore be unfillable at both
//     call sites (degrade.scopedKey, relay.scopedMessagesKey)
//     except with a guess — and a guessed provider in the key
//     would fragment hits and could store an answer under a
//     provider that never served it.
//
//   - Even at STORE time the provider is not recorded:
//     provider.ChatResponse carries no provider field and
//     CachedResponse records only Model, so plumbing the actual
//     serving provider into the key needs engine/trace changes
//     outside this package. The semantic layer records provider
//     nowhere either: slots carry model only
//     (LocalSemanticCache, SetTextForModel/GetTextForModel), so
//     scoping the exact key alone would leave the semantic
//     layer cross-provider — an inconsistent half-isolation.
//
//   - Mitigations already in place: any request that DOES pin a
//     provider never touches the cache — a non-empty routing
//     contract (Routing.Only/Order/Exclude) bypasses lookup and
//     store, and Responses continuations (EndpointResponses, or
//     PreviousResponseID set — PinProviderID accompanies them but
//     is not itself the gate) are excluded by cacheable().
//     Sharing therefore applies only to default-routed requests,
//     where this router treats providers as interchangeable
//     attempts for one model id (failover across providers for
//     the same model is the M4 design).
//
//   - Honest caveat: sharing is a trade-off, not a claim that
//     providers are identical — the same model id CAN be
//     different weights/behaviour across providers (gpt-4o via
//     OpenAI vs via OpenRouter). An operator who needs a
//     provider-pinned answer must use a routing contract, which
//     bypasses the cache by design. Revisit this decision only
//     by restructuring lookup to run AFTER provider selection
//     (or by recording the serving provider at store time) —
//     never by adding an unfilled Provider field to this struct.
//     TestM612KeyInputHasNoProviderField trips if anyone does.
//
//   - Tenant is the gateway key id (the caller identity). "" means
//     "no gateway key on the request" — it is still hashed, so an
//     anonymous request never collides with a named tenant.
//
//   - Temperature/TopP enter as 2-decimal buckets: 0.70 and 0.71 are
//     different answers, 0.704 and 0.700 are the same bucket.
//
//   - Tools/ToolChoice are in the key even though tool-using
//     requests are currently excluded from caching entirely
//     (degrade.cacheable): if that exclusion ever loosens, a
//     tool-less request can never hit a tool-carrying entry.
//
//   - ResponseFormat, MaxTokens, Stop, Seed and ReasoningEffort all
//     change the answer, so they are all in the key. Fields the
//     provider package does not (yet) carry — ResponseFormat, Seed —
//     are accepted here as any so callers can pass them through
//     when they exist.
type KeyInput struct {
	Tenant          string
	Model           string
	Messages        any
	Tools           any
	ToolChoice      any
	Temperature     float64
	TopP            float64
	MaxTokens       int
	Stop            any
	ResponseFormat  any
	Seed            any
	ReasoningEffort string
	// Compression is the compression opt-in in force for the
	// request (lossy budget, RTK level, ...). Two requests with
	// identical raw messages but different compression params
	// may be answered from different prompts, so they must
	// never share an entry. Nil is omitted, so keys derived
	// without a compression snapshot are byte-identical to
	// before this field existed.
	Compression any
}

// TemperatureBucket buckets a temperature to 2 decimal places
// ("0.70"). Two temperatures in the same bucket share a cache key;
// temperatures 0.01 apart do not.
func TemperatureBucket(temperature float64) string {
	return fmt.Sprintf("%.2f", math.Round(temperature*100)/100)
}

// ScopedKey derives the M1.4 cache key: SHA-256 (hex, 64 chars) of
// the JSON encoding of {tenant, model, messages, tools, tool_choice,
// temperature bucket, top_p bucket, max_tokens, stop,
// response_format, seed, reasoning_effort, compression}. Identical inputs always
// produce the same key; any difference in any field produces a
// different key (up to SHA-256 collision resistance). JSON object
// keys sort deterministically under encoding/json, so map-shaped
// Messages/Tools/ResponseFormat values key stably.
func ScopedKey(in KeyInput) (string, error) {
	payload := struct {
		Tenant          string `json:"tenant"`
		Model           string `json:"model"`
		Messages        any    `json:"messages"`
		Tools           any    `json:"tools,omitempty"`
		ToolChoice      any    `json:"tool_choice,omitempty"`
		Temperature     string `json:"temperature_bucket"`
		TopP            string `json:"top_p_bucket"`
		MaxTokens       int    `json:"max_tokens,omitempty"`
		Stop            any    `json:"stop,omitempty"`
		ResponseFormat  any    `json:"response_format,omitempty"`
		Seed            any    `json:"seed,omitempty"`
		ReasoningEffort string `json:"reasoning_effort,omitempty"`
		Compression     any    `json:"compression,omitempty"`
	}{
		Tenant:          in.Tenant,
		Model:           in.Model,
		Messages:        in.Messages,
		Tools:           in.Tools,
		ToolChoice:      in.ToolChoice,
		Temperature:     TemperatureBucket(in.Temperature),
		TopP:            TemperatureBucket(in.TopP),
		MaxTokens:       in.MaxTokens,
		Stop:            in.Stop,
		ResponseFormat:  in.ResponseFormat,
		Seed:            in.Seed,
		ReasoningEffort: in.ReasoningEffort,
		Compression:     in.Compression,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// CacheableTemperature reports whether a response produced at
// temperature may be cached. Temperature 0 (including unset, which
// decodes as 0) is always cacheable. Any temperature above 0 is
// cacheable only when allowNonZeroTemp is true — the per-key explicit
// allowance (the sector-typical rule is "temperature 0 only"; we keep the
// same default and let a gateway key opt in). A negative temperature
// is invalid input and is never cacheable.
func CacheableTemperature(temperature float64, allowNonZeroTemp bool) bool {
	if temperature == 0 {
		return true
	}
	if temperature < 0 {
		return false
	}
	return allowNonZeroTemp
}

// ShouldStore is the single store-time gate: it answers whether a
// response may be written into any cache layer. It refuses:
//
//   - finish_reason "length": the answer was cut off by max_tokens
//     and is incomplete — storing it would serve a truncated answer
//     as if it were the whole one;
//   - temperature > 0 without the per-key allowance (see
//     CacheableTemperature);
//   - tool-carrying responses: a cached entry stores content only,
//     so storing one would let a later lookup silently drop the
//     tool calls the caller asked for.
//
// "stop", "" (unknown) and any other finish reason pass this check.
func ShouldStore(finishReason string, temperature float64, allowNonZeroTemp bool, hasToolCalls bool) bool {
	if hasToolCalls {
		return false
	}
	if strings.EqualFold(finishReason, "length") {
		return false
	}
	return CacheableTemperature(temperature, allowNonZeroTemp)
}

// X-Codvian-Cache header values (M1.4 correctness). A hit names the
// layer that served it ("exact" or "semantic"); a miss is always
// "MISS" — never a layer name, never empty — so clients and the
// dashboard can tell a miss from a hit without parsing anything else.
// The response-header writer (M2.1, internal/server) owns setting
// this header; this function is the single source of truth for the
// value so the cache package and the server cannot drift apart.
const (
	// CacheHeaderMiss is the X-Codvian-Cache value on a miss.
	CacheHeaderMiss = "MISS"
	// CacheHeaderExact is the X-Codvian-Cache value on an exact-layer hit.
	CacheHeaderExact = "exact"
	// CacheHeaderSemantic is the X-Codvian-Cache value on a semantic-layer hit.
	CacheHeaderSemantic = "semantic"
)

// CacheHeader returns the X-Codvian-Cache value for a lookup: the
// layer name on a hit ("" layer falls back to "HIT"), "MISS" on a
// miss.
func CacheHeader(layer string, hit bool) string {
	if !hit {
		return CacheHeaderMiss
	}
	if layer == "" {
		return "HIT"
	}
	return layer
}
