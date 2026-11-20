// Package compress — ApplyOptions (G22, PARITY.md Area 10 rows
// :848/:865/:866/:867/:868/:869).
//
// Apply (compress.go) keeps its original signature and behaviour
// with zero options. ApplyWithOptions is the same pass with a
// caller-supplied config surface for the pieces Apply cannot
// derive from the model name alone:
//
//   - Cache-aware inputs (row :868): IsCachingProvider /
//     HasCacheControl feed ResolveCacheAwareStrategy (mode
//     downgrade) and ResolvePreserveSystemPrompt (freeze). The
//     provider for IsCachingProvider is the resolved target
//     provider (strategy.SupportsPromptCacheMarker at the call
//     site); Apply must not guess it from the model name —
//     bare model names ("gpt-4o", "claude-sonnet-...") carry no
//     provider prefix, and a wrong guess would either leave a
//     cacheable prefix unprotected or freeze prompts that have
//     no cache to protect. The router supplies IsCachingProvider
//     since G22/B10 (Engine.anyCachingProvider — true when any
//     configured provider client honours a marker; the stamp is
//     shared across the fallback chain, so "any" is the
//     conservative choice). HasCacheControl is still unsupplied
//     in the engine — the canonical message drops native markers
//     at parse — and the process-wide PrefixFreeze observation
//     is wired since :939: the engine holds the observer hooks
//     (SetPrefixFreezeHooks, installed by main.go, which owns
//     the *degrade.PrefixFreeze), maybeCompress observes once
//     per request and passes FrozenPrefix here. The observer
//     instance itself is still installed by main.go (follow-up);
//     until then the hooks are nil and behaviour is unchanged.
//   - PreserveSystemPromptMode (row :869): the per-request field
//     preserve_system_prompt_mode wins when set; otherwise this
//     option; otherwise the "always" default.
//   - FreezeSystemPrompt (row :868): an explicit freeze that
//     wins over the mode resolution — this is the seam a
//     PrefixFreeze owner uses (FreezeSystemPrompt =
//     freeze.IsFrozen(msgs)). FrozenPrefix is the same seam as
//     a callback: when set, a frozen prefix freezes the system
//     prompt even when the mode resolution would allow
//     truncation, but only when there is a cache to protect
//     (IsCachingProvider or HasCacheControl).
//   - ToolResultMaxChars (row :848): the opt-in Lite tool-result
//     cap; the per-request field wins when > 0.
//   - AutoTrigger / UseBudgetDial (rows :866/:867): opt-in
//     mode selection when the request names no explicit mode.
//     Default is explicit-only — a bare lossy request still runs
//     exactly the extractive pair — because silently upgrading a
//     lossy request to a rewriting engine would change the
//     documented lossy contract. Precedence: explicit mode >
//     AutoTrigger > BudgetDial > bare extractive.
package compress

import (
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ApplyOptions tunes one ApplyWithOptions call. The zero value
// is exactly Apply's behaviour.
type ApplyOptions struct {
	// IsCachingProvider reports whether the resolved target
	// provider caches a prompt prefix (see the package doc:
	// supplied by the caller, never guessed from the model
	// name). True downgrades aggressive/ultra to standard and
	// freezes the system prompt under mode "whenNoCache".
	IsCachingProvider bool
	// HasCacheControl reports whether the request carries a
	// cache_control marker. True freezes the system prompt
	// under mode "whenNoCache".
	HasCacheControl bool
	// PreserveSystemPromptMode is "always" (default), "whenNoCache"
	// or "never" (see SystemPromptMode). "" means "always".
	// The per-request field preserve_system_prompt_mode wins
	// when set.
	PreserveSystemPromptMode string
	// FreezeSystemPrompt freezes the system prompt regardless
	// of the mode resolution (the PrefixFreeze seam).
	FreezeSystemPrompt bool
	// FrozenPrefix, when non-nil, reports whether this
	// request's system prompt is a frozen (recurring,
	// cacheable) prefix — the PrefixFreeze observer's
	// IsFrozen, supplied by whoever owns the observer (see
	// degrade.PrefixFreeze.FreezeChecker). A frozen prefix
	// freezes the system prompt even when the mode
	// resolution would allow truncation, but only when
	// there is a cache to protect (IsCachingProvider or
	// HasCacheControl): for a non-caching provider there is
	// no cache to break, so the freeze must not fire there.
	FrozenPrefix func(msgs []provider.Message) bool
	// ToolResultMaxChars is the opt-in Lite tool-result cap in
	// runes. 0 means no cap. The per-request field
	// tool_result_max_chars wins when > 0.
	ToolResultMaxChars int
	// RTKLevel is the RTK tool-output filter level for this
	// pass (row B4-25): minimal, standard or aggressive. "" means
	// standard. The per-request field rtk_level wins when set
	// (see effectiveRTKLevel); an unparseable level also means
	// standard — an unknown level must never fail a request.
	RTKLevel RTKLevel
	// AutoTrigger selects the mode from prompt size (and tool
	// share) when the request names no explicit mode.
	AutoTrigger bool
	// AutoTriggerConfig tunes the auto-trigger. Zero values
	// take DefaultAutoTriggerConfig.
	AutoTriggerConfig AutoTriggerConfig
	// UseBudgetDial selects the cheapest mode whose estimated
	// output fits the lossy budget when the request names no
	// explicit mode.
	UseBudgetDial bool
}

// ApplyWithOptions is Apply with caller-supplied options (see
// ApplyOptions). It is the live call site for the cache-aware
// wiring, the Lite tool-result cap, and the opt-in auto-trigger
// / budget-dial mode selection.
func ApplyWithOptions(model string, msgs []provider.Message, req *provider.CompressionRequest, losslessModels []string, prior *provider.CompressionResult, opts ApplyOptions) ([]provider.Message, *provider.CompressionResult) {
	return applyWithOptions(model, msgs, req, losslessModels, prior, opts)
}

// effectivePreserveMode resolves the preserve mode: the
// per-request field wins when set, else the option, else "" (the
// "always" default in ResolvePreserveSystemPrompt).
func effectivePreserveMode(req *provider.CompressionRequest, opts ApplyOptions) string {
	if req != nil && req.PreserveSystemPromptMode != "" {
		return req.PreserveSystemPromptMode
	}
	return opts.PreserveSystemPromptMode
}

// effectiveRTKLevel resolves the RTK filter level for one Apply
// pass: the per-request field wins when set, then the caller
// option, then standard. An unparseable level means standard —
// an unknown level must never fail a request (the HTTP layer
// 400s an unknown rtk_level body value; this is the defensive
// fallback for direct callers).
func effectiveRTKLevel(req *provider.CompressionRequest, opts ApplyOptions) RTKLevel {
	if req != nil && req.RTKLevel != "" {
		if level, ok := ParseRTKLevel(req.RTKLevel); ok && level != "" {
			return level
		}
		return RTKLevelStandard
	}
	if opts.RTKLevel != "" {
		if level, ok := ParseRTKLevel(string(opts.RTKLevel)); ok && level != "" {
			return level
		}
		return RTKLevelStandard
	}
	return RTKLevelStandard
}

// effectiveToolCap resolves the Lite tool-result cap in runes:
// the per-request field wins when > 0, else the option. 0 means
// no cap. Out-of-range values are clamped into the usable range
// [256, 1,000,000] (toolcap.go) instead of falling back to a
// default cap — a fallback would invent a cap the client did
// not ask for.
func effectiveToolCap(req *provider.CompressionRequest, opts ApplyOptions) int {
	cap := opts.ToolResultMaxChars
	if req != nil && req.ToolResultMaxChars > 0 {
		cap = req.ToolResultMaxChars
	}
	if cap <= 0 {
		return 0
	}
	if cap < toolCapMin {
		return toolCapMin
	}
	if cap > toolCapMax {
		return toolCapMax
	}
	return cap
}

// resolveFreeze computes whether this request's system prompt
// must not be truncated: the explicit FreezeSystemPrompt seam
// wins, else a frozen prefix (when there is a cache to
// protect), else the preserve-mode resolution (default
// "always").
func resolveFreeze(msgs []provider.Message, req *provider.CompressionRequest, opts ApplyOptions) bool {
	if opts.FreezeSystemPrompt {
		return true
	}
	if opts.FrozenPrefix != nil && (opts.IsCachingProvider || opts.HasCacheControl) && opts.FrozenPrefix(msgs) {
		return true
	}
	return ResolvePreserveSystemPrompt(
		SystemPromptMode(effectivePreserveMode(req, opts)),
		opts.IsCachingProvider,
		opts.HasCacheControl,
	)
}
