// Cache-aware compression (M7.7, PLAN.md): the the sector baseline
// cachingAware.ts / prefixFreeze.ts behaviour, as pure logic.
// The resolvers now live in internal/compress/preserve.go
// (Apply is the real caller and compress cannot import this
// package); this file keeps aliases for them and owns the
// PrefixFreeze observer. Read this header before wiring it.
//
// What the sector baseline does (study only, never copied):
//
//   - cachingAware.ts detects a caching context from the request
//     body (hasCacheControl anywhere in system/tools/messages/
//     input/contents, provider inferred from the model prefix,
//     providerSupportsCaching, automatic-prefix-caching
//     providers) and getCacheAwareStrategy downgrades
//     aggressive/ultra to standard, sets skipSystemPrompt=true
//     and deterministicOnly=true for a caching provider. Since
//     the sector baseline #3955 the provider alone is sufficient —
//     automatic-prefix-cache providers (OpenAI, Codex, Azure)
//     carry no cache_control markers, yet rewriting the prefix
//     still guarantees a cache miss.
//   - prefixFreeze.ts OBSERVES which system prompts recur: the
//     SHA-256 (first 24 hex chars) of the system prompt is
//     counted per request, and once seen >= threshold (default
//     3) the prefix is treated as a stable cacheable prefix to
//     preserve. Default OFF, in-memory, bounded (5,000 entries,
//     oldest evicted first). A freeze only PRESERVES the prefix
//     from compression — it never corrupts a payload.
//   - types.ts preserveSystemPromptMode: "always" (default —
//     never compress the system prompt), "whenNoCache" (compress
//     it only when there is no cache to protect — i.e. preserve
//     when the provider caches or cache_control is present),
//     "never" (always compress it, even when it breaks a cache).
//
// What exists here, as a real seam:
//
//   - PrefixFreeze is the observer itself: Observe records one
//     sighting of a request's system prompt, IsFrozen reports
//     whether it has reached the threshold. It is a standalone
//     value — no existing degrade state is touched, and nothing
//     in the request path calls it yet.
//   - CacheAwareStrategy is the pure downgrade decision, and
//     ResolvePreserveSystemPrompt is the pure mode resolver.
//     Both take their inputs as explicit arguments.
//
// What is UNWIRED, and why it stays that way in this slice:
//
//   - No live path derives `isCachingProvider` for these
//     functions — until G22/B10: the engine now supplies it at
//     its compression call site (router.maybeCompress via
//     Engine.anyCachingProvider, reading the canonical
//     cache.SupportsPromptCacheMarker accessor). This package
//     still deliberately does not duplicate the table (the map
//     comment requires the two to stay in lockstep; a third
//     copy would drift).
//   - `hasCacheControl` cannot be derived from a request here
//     at all: provider.Message has no CacheControl field
//     (internal/provider/client.go:46-54; see also the M6.9
//     audit comment in internal/cache/cache.go and
//     internal/provider/anthropic_client.go:192-193 — the
//     Anthropic wire carries the marker, the canonical message
//     does not). A Message field + wire translation is a
//     provider slice first; until it lands, callers that know
//     the wire shape must supply the bool.
//   - The freeze decision IS consulted by a live path since
//     G22: compress.ApplyWithOptions resolves the preserve mode
//     (default "always") and the cache-aware strategy, and
//     passes FreezeSystemPrompt into both applyLossy's
//     truncation and HardBudgetWithOptions — the post-pass
//     honours it by marking the system prompt part-carrying for
//     the round (see internal/compress/postpass.go). The
//     PrefixFreeze seam is wired at the Apply layer since
//     Block 2 Batch 1: ApplyOptions.FrozenPrefix (supplied via
//     PrefixFreeze.FreezeChecker) freezes a frozen prefix even
//     when the mode would allow truncation, but only when a
//     cache exists to protect. What is still supplied by the
//     caller, not derived here: the hasCacheControl bool (the
//     engine cannot see native markers — the canonical message
//     drops them at parse), and the PrefixFreeze observation
//     itself — no live path owns a process-wide observer yet, so
//     observation (one Observe per request, when enabled) plus
//     passing FrozenPrefix = freeze.FreezeChecker() is
//     coordinator follow-up (server/router pass-through).
//   - QuantumLock (the sector-typical opt-in prefix stabilisation) is
//     NOT built here: no equivalent exists in this repo and it
//     is out of the M7.7 slice.
//
// The one deliberate divergence from the sector baseline: our lossy path
// already pins system/developer messages from being DROPPED
// (compress.applyLossy), so "preserve system prompt" here means
// the stronger property the post-pass adds — never TRUNCATED
// either. Mode "never" therefore still never drops the system
// prompt (that pin is unconditional in applyLossy); it only
// permits truncation.
package degrade

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// DefaultPrefixFreezeThreshold is how many times a system prompt
// must be observed before it is treated as a frozen stable
// prefix. Mirrors the sector-typical DEFAULT_PREFIX_FREEZE.threshold.
const DefaultPrefixFreezeThreshold = 3

// MaxPrefixFreezeEntries bounds the observer: the oldest
// observation is evicted first once the cap is reached. Mirrors
// the sector-typical MAX_PREFIX_ENTRIES.
const MaxPrefixFreezeEntries = 5000

// SystemPromptHash is the stable identity of a request's system
// prompt: SHA-256 (first 24 hex chars) of the system/developer
// message contents joined with "\n". the sector baseline hashes the same
// shape across OpenAI messages, Claude `system` and Gemini
// `systemInstruction`; ours hashes provider.Message roles only —
// the canonical form this router carries. "" when there is no
// system/developer text to freeze.
func SystemPromptHash(msgs []provider.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		if text := systemPromptText(m); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:24]
}

// systemPromptText is the freezable text of one message: its text
// parts when present, else its Content. Blank text contributes
// nothing (mirrors the sector-typical collectText skipping blank
// strings).
func systemPromptText(m provider.Message) string {
	if len(m.Parts) > 0 {
		var texts []string
		for _, p := range m.Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	if strings.TrimSpace(m.Content) == "" {
		return ""
	}
	return m.Content
}

// PrefixFreeze observes system prompts across requests and
// reports which ones have recurred often enough to be treated as
// stable cacheable prefixes. It is safe for concurrent use. A
// freeze only ever PRESERVES a prefix from compression; it never
// rewrites, drops, or otherwise changes a payload. Default OFF:
// a disabled observer records nothing and freezes nothing (zero
// cost when off, mirroring the sector baseline).
type PrefixFreeze struct {
	mu           sync.Mutex
	enabled      bool
	threshold    int
	observations map[string]int
	order        []string // first-seen order; order[0] is the oldest live hash
}

// NewPrefixFreeze returns a disabled-by-default observer.
// threshold <= 0 means DefaultPrefixFreezeThreshold.
func NewPrefixFreeze(enabled bool, threshold int) *PrefixFreeze {
	if threshold <= 0 {
		threshold = DefaultPrefixFreezeThreshold
	}
	return &PrefixFreeze{
		enabled:      enabled,
		threshold:    threshold,
		observations: make(map[string]int),
	}
}

// Enabled reports whether the observer records and freezes.
func (f *PrefixFreeze) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

// Observe records one sighting of the system prompt in msgs.
// No-op when disabled or when there is no system prompt.
func (f *PrefixFreeze) Observe(msgs []provider.Message) {
	f.ObserveHash(SystemPromptHash(msgs))
}

// ObserveHash records one sighting of a system-prompt hash.
// No-op when disabled or when hash is "".
func (f *PrefixFreeze) ObserveHash(hash string) {
	if hash == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled {
		return
	}
	if _, seen := f.observations[hash]; !seen {
		if len(f.observations) >= MaxPrefixFreezeEntries {
			oldest := f.order[0]
			f.order = f.order[1:]
			delete(f.observations, oldest)
		}
		f.order = append(f.order, hash)
	}
	f.observations[hash]++
}

// IsFrozen reports whether the system prompt in msgs has been
// observed at least threshold times. False when disabled, when
// there is no system prompt, and below the threshold.
func (f *PrefixFreeze) IsFrozen(msgs []provider.Message) bool {
	return f.IsFrozenHash(SystemPromptHash(msgs))
}

// IsFrozenHash reports whether hash has been observed at least
// threshold times. False when disabled or hash is "".
func (f *PrefixFreeze) IsFrozenHash(hash string) bool {
	if hash == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled && f.observations[hash] >= f.threshold
}

// Observations returns the current sighting count for hash (0
// when never seen). It counts even when disabled — a disabled
// observer just never accumulates.
func (f *PrefixFreeze) Observations(hash string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observations[hash]
}

// Entries returns how many distinct prefixes are tracked.
func (f *PrefixFreeze) Entries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.observations)
}

// Reset clears all observations (tests + operator reset).
func (f *PrefixFreeze) Reset() {
	f.mu.Lock()
	f.observations = make(map[string]int)
	f.order = nil
	f.mu.Unlock()
}

// FreezeChecker returns a predicate for
// compress.ApplyOptions.FrozenPrefix: it reports whether the
// system prompt in msgs is frozen (observed at least threshold
// times). The predicate is safe to pass to Apply even when the
// observer is disabled (it then always reports false), and a
// nil receiver yields a predicate that always reports false
// instead of panicking. Apply consults it only when there is a
// cache to protect, so wiring this in never changes behaviour
// for a non-caching provider.
func (f *PrefixFreeze) FreezeChecker() func([]provider.Message) bool {
	if f == nil {
		return func([]provider.Message) bool { return false }
	}
	return func(msgs []provider.Message) bool { return f.IsFrozen(msgs) }
}

// Cache-aware resolvers — aliases to the canonical
// implementations in internal/compress/preserve.go (moved there
// in G22 so Apply can call them without an import cycle;
// degrade imports router, router imports compress). Do not
// reintroduce a second implementation here.
type CacheAwareStrategy = compress.CacheAwareStrategy
type SystemPromptMode = compress.SystemPromptMode

const (
	SystemPromptPreserveAlways      = compress.SystemPromptPreserveAlways
	SystemPromptPreserveWhenNoCache = compress.SystemPromptPreserveWhenNoCache
	SystemPromptPreserveNever       = compress.SystemPromptPreserveNever
)

// ResolveCacheAwareStrategy is compress.ResolveCacheAwareStrategy.
func ResolveCacheAwareStrategy(strategy string, isCachingProvider bool) CacheAwareStrategy {
	return compress.ResolveCacheAwareStrategy(strategy, isCachingProvider)
}

// ResolvePreserveSystemPrompt is compress.ResolvePreserveSystemPrompt.
func ResolvePreserveSystemPrompt(mode SystemPromptMode, isCachingProvider, hasCacheControl bool) bool {
	return compress.ResolvePreserveSystemPrompt(mode, isCachingProvider, hasCacheControl)
}
