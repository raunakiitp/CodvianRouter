// Package compress — cache-aware system-prompt preservation (G22,
// parity tracker Area 10 rows :868/:869).
//
// This file is the CANONICAL home of the two pure cache-aware
// resolvers. They lived in internal/degrade/cache_aware.go, but
// Apply is the real caller and compress cannot import degrade
// (degrade imports router, router imports compress — an import
// would close the cycle), so the resolvers moved here and degrade
// keeps type/function aliases for its existing callers and tests.
// There is exactly one implementation; do not reintroduce a copy
// in degrade.
//
// What the sector baseline does (study only, never copied —
// the earlier implementation,
// prefixFreeze.ts, types.ts:200-240, 428):
//
//   - getCacheAwareStrategy downgrades aggressive/ultra to
//     standard for a caching provider, sets skipSystemPrompt=true
//     and deterministicOnly=true. Since the sector baseline #3955 the
//     provider alone is sufficient — automatic-prefix-cache
//     providers (OpenAI, Codex, Azure) carry no cache_control
//     markers, yet rewriting the prefix still guarantees a cache
//     miss.
//   - preserveSystemPromptMode: "always" (default — never
//     compress the system prompt), "whenNoCache" (compress it
//     only when there is no cache to protect — i.e. preserve when
//     the provider caches or cache_control is present), "never"
//     (always compress it, even when it breaks a cache).
//
// What "preserve" means here: the system/developer prompt is
// never DROPPED (the drop pin in applyLossy is unconditional)
// and, when preserved, never TRUNCATED either — by applyLossy's
// head+tail cut or by the hard-budget post-pass. That is the
// stronger property the post-pass adds, and the one a frozen
// cacheable prefix needs: rewriting the prefix breaks the
// provider-side prompt cache. Mode "never" therefore still never
// drops the system prompt; it only permits truncation.
//
// The two bools (isCachingProvider, hasCacheControl) must be
// supplied by the caller: the provider list lives in
// strategy.SupportsPromptCacheMarker and provider.Message has no
// CacheControl field (see internal/degrade/cache_aware.go). A
// PrefixFreeze observation is supplied the same way, as the
// FreezeSystemPrompt flag on ApplyOptions — Apply never owns the
// observer; whoever owns it passes IsFrozen in.
package compress

// CacheAwareStrategy is the cache-aware compression decision for
// one request, mirroring the sector-typical getCacheAwareStrategy return
// shape.
type CacheAwareStrategy struct {
	// Strategy is the mode to actually run: aggressive/ultra
	// are downgraded to standard for a caching provider,
	// anything else passes through unchanged.
	Strategy string
	// SkipSystemPrompt, when true, means the system prompt
	// must not be compressed at all (it is the cacheable
	// prefix).
	SkipSystemPrompt bool
	// DeterministicOnly, when true, means only deterministic
	// compression steps may run (no sampling-dependent
	// behaviour that would rewrite the prefix differently
	// per request). Every engine in this repo is deterministic
	// by construction, so this is recorded, not enforced.
	DeterministicOnly bool
}

// ResolveCacheAwareStrategy downgrades the requested compression
// strategy for a caching provider: aggressive/ultra become
// standard, the system prompt is skipped, and only deterministic
// steps run. isCachingProvider must be supplied by the caller
// (strategy.SupportsPromptCacheMarker at the call site). A
// caching provider alone is sufficient — no cache_control marker
// is required (the sector baseline #3955). A non-caching provider returns
// the strategy unchanged with both guards off.
func ResolveCacheAwareStrategy(strategy string, isCachingProvider bool) CacheAwareStrategy {
	if !isCachingProvider {
		return CacheAwareStrategy{Strategy: strategy}
	}
	out := CacheAwareStrategy{
		Strategy:          strategy,
		SkipSystemPrompt:  true,
		DeterministicOnly: true,
	}
	if strategy == "aggressive" || strategy == "ultra" {
		out.Strategy = "standard"
	}
	return out
}

// SystemPromptMode is the operator's system-prompt preservation
// intent, mirroring the sector-typical PreserveSystemPromptMode.
type SystemPromptMode string

const (
	// SystemPromptPreserveAlways never compresses the system
	// prompt. This is the sector-typical default.
	SystemPromptPreserveAlways SystemPromptMode = "always"
	// SystemPromptPreserveWhenNoCache compresses the system
	// prompt only when there is no cache to protect.
	SystemPromptPreserveWhenNoCache SystemPromptMode = "whenNoCache"
	// SystemPromptPreserveNever always compresses the system
	// prompt, even when it breaks a prompt cache. (It still
	// never DROPS it — the drop pin in applyLossy is
	// unconditional; "never" permits truncation only.)
	SystemPromptPreserveNever SystemPromptMode = "never"
)

// ResolvePreserveSystemPrompt resolves a preservation mode to the
// effective boolean: should this request's system prompt be
// preserved (never truncated)? always → true; never → false;
// whenNoCache → true exactly when there IS a cache to protect
// (the provider caches, or cache_control is present);
// "" or an unknown mode → true (the "always" default). The two
// bools must be supplied by the caller.
func ResolvePreserveSystemPrompt(mode SystemPromptMode, isCachingProvider, hasCacheControl bool) bool {
	switch mode {
	case SystemPromptPreserveNever:
		return false
	case SystemPromptPreserveWhenNoCache:
		return isCachingProvider || hasCacheControl
	case SystemPromptPreserveAlways:
		return true
	default:
		// Absent/unknown → the "always" default.
		return true
	}
}
