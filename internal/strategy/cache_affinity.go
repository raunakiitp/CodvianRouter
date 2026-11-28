// Prompt-cache affinity routing (M6.5): cache-affinity ORDERING for the
// "cache-optimized" strategy.
//
// This file builds the ranking; wiringHook's StrategyCacheOptimized
// case wires it into the hook (see the "Wiring" note on
// OrderByCacheAffinity). Registration already exists —
// "cache-optimized" is in strategy.All (strategy.go const + All
// list + Describe), config validation (config.go global/chain
// strategy error lists + allowlist) and the strategy tree
// (strategytree/tree.go allowlist). M6.5-wiring: the case used to
// pass Warmth 0 for every target into OrderByCacheOptimized
// (affinity.go), so the strategy was the identity ordering; it
// now feeds OrderByCacheAffinity with measured warmth from the
// ledger accessor and the request's cacheability gate.
//
// What this ordering ranks on — signals that REALLY exist in the tree:
//
//  1. Measured per-target cache warmth (Warmth, 0..1), when the caller
//     has it (WarmthKnown). This is the same input OrderByCacheOptimized
//     takes. It DOMINATES the ranking: a genuinely warm target wins
//     over a merely cache-capable one.
//  2. Prompt-cache marker support (SupportsPromptCacheMarker): whether
//     cache.Prepare() will actually annotate this request for the
//     target's provider — anthropic / claude / openrouter only
//     (the promptCacheProviders map in internal/cache/cache.go,
//     used by Prepare). Among targets with equal (or unknown) warmth, a marker-supporting
//     target leads, because only there does the cacheable prefix get
//     explicitly marked for reuse. The set here is a deliberate copy
//     of that map, not an import of internal/cache (this ordering
//     stays pure and dependency-free); if Prepare() gains a provider,
//     this set must be updated in lockstep.
//  3. Fallback score (Score): the caller's existing default ranking
//     for the target — e.g. the 5-factor balanced score wiring builds
//     in toOrderingTargets. When warmth and marker support tie, the
//     order falls back to that score, so cache-affinity never
//     discards the default ordering; it only re-ranks on top of it.
//     A Score of 0 for every target (unknown) leaves declared order.
//
// What does NOT exist — and is NOT invented here:
//
//   - No per-(provider, model) cache-warmth signal feeds the
//     strategy hook today. scoring.Signals (scoring/scoring.go:60)
//     has no cache field; wiring.toOrderingTargets never fills
//     one; the degrade layer's LastCacheHit is the single
//     most-recent own-cache hit, global, not per-target;
//     metrics.IncCacheHit counts by layer only. The ledger DOES
//     record provider-reported cached tokens per attempt (ledger
//     CachedTokens, per-provider summaries in
//     ledger/cache_tokens_test.go), and M6.5-wiring added the
//     read-only accessor CachedTokenFraction (ledger/ledger.go)
//     that exposes a per-pair cached-token fraction — and the
//     production install that closes over it has LANDED
//     (cmd/codvian-router/main.go:3084 builds a 1-min
//     CachedTokenFraction snapshot, warmthSnap; :3097 wires
//     HookFactoryWithCacheAffinity as StrategyHookForRequest):
//     WarmthKnown is true for a target when the ledger holds
//     measured warmth for it, so the ordering ranks on (1)
//     then (2) + (3); only when no target has measured warmth
//     does the wiring's no-measured-warmth guard return
//     declared order instead. Verified live by V-B207
//     (2026-11-20 ~11:28 IST, toggle-proven: warmth disabled
//     makes the M65 reorder tests FAIL).
//   - OpenAI and Gemini are NOT marker-supporting, and that is NOT
//     a claim they cannot cache: OpenAI runs automatic prefix caching
//     server-side (Prepare is a no-op there by design) and Gemini
//     uses a separate cachedContents API. Marker support ranks only
//     the benefit this router can explicitly request (the
//     cache_control marker); automatic/server-side caching needs no
//     ordering help and is not penalised beyond losing the tiebreak.
//
// the sector baseline semantics studied (read-only reference
// the local reference clone; no code copied):
// applyStrategyOrdering.ts orderByCacheAffinity ranks by a
// cache-affinity score for the current prompt prefix, and
// strategyRegistry.ts:182 marks "cache-optimized" with
// cacheAffinityOwnsOrdering: true — the affinity owns the order.
// This ordering is that score's local form: warmth, then marker
// capability, then the default score.
//
// The ordering is pure and deterministic: no I/O, no network, no
// clock, no RNG, no global mutable state (the marker set is an
// immutable package-level map). Same inputs always give the same
// output; ties keep declared order (stable sort). The input slice
// is never mutated.
package strategy

import (
	"math"
	"sort"
	"strings"
)

// MinCacheablePromptTokens is the smallest cacheable-prefix size
// (tokens) for which affinity ordering applies. Anthropic prompt
// caching requires a minimum prefix of 1024 tokens (2048 for some
// models) and OpenAI automatic prefix caching likewise starts at
// 1024 tokens for most models, so 1024 is the shared floor below
// which no provider can reuse the prefix and re-ranking on cache
// affinity would be pure noise. A request shorter than this is
// left in declared order.
const MinCacheablePromptTokens = 1024

// promptCacheMarkerProviders mirrors internal/cache/cache.go
// promptCacheProviders: the providers (router provider name,
// lower-cased, whitespace stripped) that cache.Prepare actually
// annotates in production — the canonical marker set. Keep this
// map in lockstep with the cache map: every provider cache.Prepare
// annotates must win the marker tiebreak here, and nothing else.
// Do not read this map as "the marker will be sent" — that is the
// live-carried set (native Anthropic hosts via the Anthropic
// client, M6.9c); openrouter's OpenAI-wire markers are applied by
// internal/provider/client.go and are not part of this set.
var promptCacheMarkerProviders = map[string]bool{
	"anthropic": true,
	"claude":    true,
}

// normalizeCacheProvider lower-cases a provider name and strips
// ASCII whitespace, mirroring cache.normalizeProvider.
func normalizeCacheProvider(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// SupportsPromptCacheMarker reports whether cache.Prepare will
// actually annotate this request for the provider — i.e. whether
// the provider is in promptCacheMarkerProviders, kept in lockstep
// with internal/cache/cache.go's promptCacheProviders. It does NOT
// mean the marker will actually be sent: the live-carried set is
// narrower (native Anthropic hosts only, M6.9c). Matching is
// case-insensitive and ignores surrounding/embedded ASCII
// whitespace (plus '\r', which cache.normalizeProvider does not
// strip — a provider name containing '\r' is pathological either
// way).

func SupportsPromptCacheMarker(provider string) bool {
	return promptCacheMarkerProviders[normalizeCacheProvider(provider)]
}

// CacheAffinityTarget is one chain target for cache-affinity
// ordering: identity (embedded AffinityTarget, as in CacheTarget),
// plus the two signals this ordering can rank on and the fallback.
//
//   - Warmth is the measured per-target cache warmth for the
//     current prompt prefix, 0..1. It is meaningful ONLY when
//     WarmthKnown is true; a Warmth of 0 with WarmthKnown false
//     means "no measurement", not "measured cold".
//   - Score is the caller's existing default ranking for the
//     target (higher is better), e.g. the 5-factor balanced score
//     from toOrderingTargets. It is the tiebreak only; 0 means
//     unknown and leaves declared order among otherwise-tied
//     targets. NaN is treated as 0 (unknown), never as a rank.
type CacheAffinityTarget struct {
	AffinityTarget
	Warmth      float64
	WarmthKnown bool
	Score       float64
}

// CacheAffinityRequest describes the request being ordered, for the
// cacheability gate (IsCacheableRequest). Only the fields the gate
// needs are carried — the ordering itself never sees the prompt.
//
//   - PromptTokens is the size in tokens of the cacheable prefix
//     (the stable leading prompt that a cache marker would cover).
//   - HasTools: the request carries tool definitions. Tool calls
//     make the reusable prefix unstable across turns (tool
//     definitions and tool results vary per call), so the slice
//     treats tool requests as non-cacheable for affinity purposes.
//   - Temperature / TemperatureSet: the sampling temperature, when
//     the caller set one. Temperature does not technically disable
//     provider prompt caching, but this slice's gate is
//     conservative: affinity re-ranking is claimed only for
//     deterministic (temperature 0 / unset) requests, where the
//     cached prefix is replayed verbatim; sampled requests keep
//     declared order. A future wiring may relax this gate — the
//     ordering itself does not depend on it.
//   - ContractPinned: the client supplied a routing contract that
//     pins the try order (routing.sort / routing.order). The
//     contract wins over every strategy (precedence: contract >
//     alias > chain > global, see strategy.go header), so affinity
//     never re-ranks a pinned request.
type CacheAffinityRequest struct {
	PromptTokens   int
	HasTools       bool
	Temperature    float64
	TemperatureSet bool
	ContractPinned bool
}

// IsCacheableRequest reports whether affinity ordering may re-rank
// this request at all: a long enough stable prompt, no tools, a
// deterministic temperature, and no client-pinned try order.
// Anything else returns the declared order unchanged.
func IsCacheableRequest(req CacheAffinityRequest) bool {
	if req.ContractPinned || req.HasTools {
		return false
	}
	if req.TemperatureSet && req.Temperature > 0 {
		return false
	}
	if req.PromptTokens < MinCacheablePromptTokens {
		return false
	}
	return true
}

// effectiveWarmth is the warmth value used for ranking: the clamped
// measurement when known, 0 (coldest / unmeasured) otherwise.
// Out-of-range measurements are clamped to 0..1, never rejected —
// a bad meter must not break the ordering.
func effectiveWarmth(t CacheAffinityTarget) float64 {
	if !t.WarmthKnown {
		return 0
	}
	switch {
	case math.IsNaN(t.Warmth) || t.Warmth < 0:
		return 0
	case t.Warmth > 1:
		return 1
	default:
		return t.Warmth
	}
}

// effectiveScore is the fallback score used for ranking: NaN
// reads as 0 (unknown).
func effectiveScore(t CacheAffinityTarget) float64 {
	if math.IsNaN(t.Score) {
		return 0
	}
	return t.Score
}

// cacheAffinityRank is the ordering key, most significant first:
// measured warmth, then marker support, then fallback score.
// Comparing ranks lexicographically (in the sort) is what makes
// measured warmth dominate marker capability, and capability
// dominate the fallback — no weighting can let a cold
// marker-supporting target outrank a measured-warm one, and no
// fallback score can outrank a marker tiebreak.
func cacheAffinityRank(t CacheAffinityTarget) (warmth float64, marker bool, score float64) {
	return effectiveWarmth(t), SupportsPromptCacheMarker(t.Provider), effectiveScore(t)
}

// OrderByCacheAffinity orders targets for a cacheable request:
// highest measured warmth first, then marker-supporting targets,
// then highest fallback score; ties keep declared order (stable
// sort). For a non-cacheable request (see IsCacheableRequest) it
// returns the input order unchanged. The input slice is never
// mutated; the result is a new slice.
//
// Wiring (M6.5-wiring, live): the StrategyCacheOptimized case in
// wiringHook (wiring.go) builds []CacheAffinityTarget from the
// chain — Warmth/WarmthKnown from the factory's warmth func
// (measured per-pair cached-token fraction from the ledger
// accessor; WarmthKnown false when the pair has no data), Score
// from the balanced scorer — and calls this function with the
// request's CacheAffinityRequest (prompt tokens, tools,
// temperature, contract-pinned flag, all threaded from the engine
// through the per-request hook context), then maps the result
// back with affinityKeys/reorderAttempts. The wiring adds one
// guard on top of this function: when no target in the chain has
// any measured warmth above 0 it returns the chain unchanged,
// so a request with no cache signal at all keeps declared order
// instead of re-ranking on marker support / fallback score alone.
// The wiring must NOT set WarmthKnown true from any guessed
// value.
func OrderByCacheAffinity(targets []CacheAffinityTarget, req CacheAffinityRequest) []CacheAffinityTarget {
	out := append([]CacheAffinityTarget(nil), targets...)
	if len(out) <= 1 || !IsCacheableRequest(req) {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		wi, mi, si := cacheAffinityRank(out[i])
		wj, mj, sj := cacheAffinityRank(out[j])
		if wi != wj {
			return wi > wj
		}
		if mi != mj {
			return mi // true (marker-supporting) sorts first
		}
		return si > sj
	})
	return out
}
