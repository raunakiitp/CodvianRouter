// Package compress — inflation guard (M7.3).
//
// CheckNoInflation is the M7 exit-gate guard for compression: a
// compressed prompt must never cost MORE tokens than the input it
// came from. The coordinator wires it into Apply at merge time
// (this file is standalone; it does not touch Apply itself).
package compress

import (
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// CheckNoInflation reports whether compressing original into
// compressed INFLATED the prompt — i.e. the compressed form costs
// more tokens than the original — and returns the exact token
// counts on both sides.
//
// Call contract (for the coordinator wiring it into Apply):
//
//   - Call as CheckNoInflation(model, original, out) where model is
//     the model the prompt will be sent to and out is Apply's
//     output for original. The model matters: the BPE encoding is
//     chosen from it (o200k_base for gpt-4o-era models,
//     cl100k_base otherwise / as fallback — see below).
//   - What it counts: exact local BPE tokens of the prompt text
//     only, via tokencount.CountPrompt — message content + text
//     parts + tool-call arguments, joined with "\n" separators,
//     exactly the same count Apply reports as BeforeTokens /
//     AfterTokens. (Above 64 KiB CountPrompt uses chunked counting
//     with a documented 0.5% boundary-drift budget, measured 0.000%
//     on prose; both sides share the chunking, so the comparison
//     stands.) It does NOT count wire framing, tool schemas,
//     or per-message overhead the upstream adds, and it is a local
//     estimate, never a provider-reported number.
//   - What counts as inflation: after > before, STRICTLY greater.
//     Equal counts are NOT inflation: a compression that saves
//     nothing (zero-saving) passes the guard. Only a strictly
//     larger compressed prompt is inflated.
//   - Fail closed: if either side cannot be counted exactly, the
//     guard reports inflated=true — never serve a compression
//     whose cost you cannot prove. Two ways that happens:
//     (1) tokencount.CountPrompt returns an error (only a
//     vendored-BPE-load failure, i.e. a build/packaging defect);
//     (2) either side is Estimated — the input exceeded the 4 MiB
//     exact-count cap and was counted as chars/4 instead of BPE.
//     In both cases the returned counts are the measured values
//     where one exists and -1 where a side was never counted
//     (an errored side is -1; an estimated side reports its
//     chars/4 estimate, labelled here as untrusted — it is
//     returned for diagnostics only, never as proof of a saving).
//   - Encoding fallback is NOT fail-closed: an unknown /
//     non-OpenAI model name resolves to cl100k_base with
//     Fallback=true, but that is still an exact BPE count under
//     that encoding, and both sides are counted under the SAME
//     encoding, so the after-vs-before comparison stands. The
//     absolute numbers are rougher for such models (the provider
//     tokenizes differently); the comparison is what the guard
//     vouches for, not the absolute count.
//   - The guard is pure and side-effect free: it counts, it does
//     not rewrite, drop, or choose anything. The caller decides
//     what to do on inflated=true (M7.3 gate: the caller sends the
//     original; counting the violation is the caller's job — this
//     guard only reports inflated=true).
//
// Real inflation cases found by the probe
// (TestInflationProbeAgreement / TestInflationRealCases, model
// "gpt-4o", Apply path — lossless first, then lossy trim):
//
//   - 131-token prompt (system + one user message of 9 repeated
//     sentences), lossy budget 129 (over by 2) → 140 tokens (+9).
//   - Same 131-token prompt, budget 126 (over by 5) → 137 (+6).
//   - Same 131-token prompt, budget 121 (over by 10) → 132 (+1).
//   - 565-token prompt (same shape, 40 repeats), budget 562
//     (over by 3) → 571 tokens (+6).
//
// Mechanism (verified, not assumed): applyLossy's truncateMiddle
// inserts the marker "[…truncated by lossy compression…]", which
// costs 9 BPE tokens, while cutting only `over` tokens of text.
// When the overage is smaller than the marker cost, the net token
// count RISES — and applyLossy's no-progress guard then retires
// the message, so the inflated output ships as-is. Apply today
// clamps the reported SavedTokens to 0 but still returns the
// inflated messages with Applied=true, which is why this guard
// must run before Apply returns. No lossless-only, tiny, short,
// empty-content, tool-call-only, already-under-budget, or
// large-overage input inflated in the probe — inflation is a
// small-overage lossy-truncation phenomenon on the inputs probed.
//
// What the guard does NOT cover (stated plainly):
//
//   - Prompt tokens only. It says nothing about the downstream
//     cost of a degraded answer — a compression can pass this
//     guard (fewer prompt tokens) while still making the model
//     produce a longer, worse, or more expensive completion.
//     Semantic fidelity is M7.2's gate, not this one.
//   - Provider-reported usage. Both counts are local BPE
//     estimates; the provider's own tokenizer may disagree by a
//     token or two, so a 1-token margin either way is within
//     counting noise — the guard reports the exact local numbers
//     and lets the caller decide the margin policy.
//   - Whether the compression was a good idea. Equal counts pass
//     even when the transform destroyed meaning (that is the
//     fidelity gate's job), and a real saving passes even when
//     the lossy trim cut something important.
func CheckNoInflation(model string, original, compressed []provider.Message) (inflated bool, before, after int) {
	beforeCount, beforeErr := tokencount.CountPrompt(model, original)
	afterCount, afterErr := tokencount.CountPrompt(model, compressed)

	// -1 = never counted (counting error on that side). An
	// estimated side keeps its chars/4 number for diagnostics.
	before, after = -1, -1
	if beforeErr == nil {
		before = beforeCount.Tokens
	}
	if afterErr == nil {
		after = afterCount.Tokens
	}

	// Fail closed: an unprovable cost is treated as inflation.
	// Either a counting error, or an estimated (chars/4,
	// over the 4 MiB exact-count cap) count on either side.
	if beforeErr != nil || afterErr != nil || beforeCount.Estimated || afterCount.Estimated {
		return true, before, after
	}
	return afterCount.Tokens > beforeCount.Tokens, beforeCount.Tokens, afterCount.Tokens
}
