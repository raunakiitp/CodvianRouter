// Package scoring — 16-factor auto scoring (M3, builder S3).
//
// This file implements the reference router's 16-factor auto-combo scoring
// (open-sse/services/autoCombo/scoring.ts DEFAULT_WEIGHTS and
// modePacks.ts MODE_PACKS) as a standalone, pure Go scorer. It is NOT
// yet wired into the router: the coordinator wires Score16/Rank16 into
// the scorer later. Nothing in this file touches the network, disk or
// any global state.
//
// Relationship to the existing 5-factor scorer (scoring.go):
//   - The 5 carried-over factors (quota, health-as-breaker, cost,
//     latency, quality, reliability-as-success) are re-implemented
//     here in the reference router's POOL-RELATIVE form (1 - value/poolMax),
//     which differs deliberately from scoring.go's absolute forms
//     (latencyComponent 1/(1+ms/1000), costComponent 1/(1+$/10)).
//     Only quality reuses scoring.go's qualityComponent verbatim, so
//     the package keeps one unknown-quality convention (0.45).
//   - The 11 factors scoring.go lacks — taskFit, tierPriority,
//     tierAffinity, specificityMatch, stability, health,
//     contextAffinity, cacheAffinity, sessionAffinity,
//     connectionDensity, resetWindowAffinity — are implemented here
//     from real candidate/target inputs. Each factor is a pure
//     function returning a value in [0, 1]; 0 is worst, 1 is best.
//
// Missing-input rule (task requirement, applied to all 11 new
// factors and to the pool-relative ones): an input that is absent or
// unusable degrades to neutral 0.5, never to a fake signal. Two
// documented divergences from the reference router's defaults, both forced by
// that rule:
//   - the reference router defaults cacheAffinity to 0 and sessionAvailability
//     to 1 when absent; here both are 0.5 when unknown.
//   - the reference router defaults reliability (unobserved failure rate) to 1;
//     here an unknown success rate is 0.5.
//   - the reference router defaults connectionDensity to 0 for a single
//     connection; here a KNOWN single connection still scores 0
//     (same as the reference router) and only an UNKNOWN pool size scores 0.5.
//   - tierPriority keeps the reference router's own unknown-tier default
//     (standard base 0.33 -> 0.264), because that IS the reference router's
//     defined behaviour for an absent tier, not a missing signal.
//
// Mode-pack weight tables are copied VERBATIM from modePacks.ts,
// including their rounding: every pack sums to 0.9999, not 1.0.
// Score16 therefore normalises any weight set before applying it
// (the same pattern as scoring.go's normalizeWeights), so the
// EFFECTIVE weights always sum to 1.0. ModePack returns the verbatim
// table; NormalizeWeights16 returns the effective one. The 0.9999
// sum is asserted in scoring16_test.go (TestSixteenWeightsSumToOne)
// so the gap stays visible instead of being silently patched.
//
// Semantics that could NOT be copied 1:1 from the reference (the
// reference computes them from data we do not have in this package):
//   - taskFit: the reference router looks a model x task-type pair up in a
//     fitness table (taskFitness.ts: user overrides, Arena ELO,
//     models.dev tiers, static table, wildcard boosts; unknown pairs
//     fall back to 0.5). That table now exists in this repo as the
//     modelsync Intelligence store (user_override > arena_elo >
//     models_dev_tier); the caller looks the model up there and
//     hands the score to FactorTaskFit via TaskFitScore/TaskFitKnown,
//     which votes as-is. With no live score the factor falls back
//     to the Jaccard similarity of the request's task tags against
//     the model's capability tags — a real computation from real
//     inputs, with the same 0.5 neutral point for "no tags at all".
//   - contextAffinity: the reference router's is a candidate-supplied score for
//     "staying on the current session's provider/account/model path"
//     (a stickiness signal). This package has no such supplied score,
//     so FactorContextAffinity is computed from real context-window
//     headroom instead: 1 - contextTokens/modelContextTokens (see
//     its doc). Session stickiness lives in FactorSessionAffinity.
//   - sessionAffinity: the reference router's sessionAvailability is likewise a
//     candidate-supplied score. FactorSessionAffinity computes it
//     from the session's current provider/model vs the candidate
//     (same pair 1.0, same provider 0.7, other provider 0.3).
//   - cacheAffinity: the reference router's is a candidate-supplied score for
//     the account selected by the stable prompt-cache key.
//     FactorCacheAffinity computes it from the fraction of the
//     prompt already cached on this candidate (0..1).
//   - resetWindowAffinity: the reference router's is a candidate-supplied score
//     ("sooner selected reset windows score higher").
//     FactorResetWindowAffinity computes it from seconds until the
//     candidate's quota resets: 1 - resetSecs/30d.
package scoring

import (
	"math"
	"sort"
)

// BreakerState is the circuit-breaker state of a candidate's provider
// (the reference router ProviderCandidate.circuitBreakerState).
type BreakerState string

// Breaker states. BreakerUnknown ("") means no breaker state was
// supplied; FactorHealth scores it neutral 0.5.
const (
	BreakerClosed   BreakerState = "closed"
	BreakerHalfOpen BreakerState = "half_open"
	BreakerOpen     BreakerState = "open"
	BreakerUnknown  BreakerState = ""
)

// AccountTier is a provider account's subscription tier
// (the reference router ProviderCandidate.accountTier).
type AccountTier string

// Account tiers. AccountTierUnknown ("") means no tier was supplied;
// FactorTierPriority scores it with the reference router's unknown-tier base
// (standard, 0.33) rather than inventing a tier.
const (
	AccountTierUltra    AccountTier = "ultra"
	AccountTierPro      AccountTier = "pro"
	AccountTierStandard AccountTier = "standard"
	AccountTierFree     AccountTier = "free"
	AccountTierUnknown  AccountTier = ""
)

// ModelTier is the price tier a model is classified into
// (the reference router classifyTier: "free" | "cheap" | "premium").
// Used by tierAffinity and specificityMatch.
type ModelTier string

// Model tiers. ModelTierUnknown ("") means the model has not been
// classified; tierAffinity and specificityMatch score it neutral 0.5.
const (
	ModelTierFree    ModelTier = "free"
	ModelTierCheap   ModelTier = "cheap"
	ModelTierPremium ModelTier = "premium"
	ModelTierUnknown ModelTier = ""
)

// resetWindowRefSecs is the reference window for reset-related
// factors: 30 days in seconds (2,592,000). It is the same constant
// the reference router's calculateTierScore uses for its reset bonus.
const resetWindowRefSecs = 2592000.0

// SixteenInputs is the minimal input set the 16 factors need. Every
// field is caller-supplied and real: latencies, costs, quota and
// variance come from live telemetry; tags, tiers, context size,
// cache warmth, session and reset time come from the request and the
// candidate's account data. Sentinels for "unknown" are documented
// per field; an unknown input degrades its factor to neutral 0.5
// (see the factor docs), never to a fabricated value.
//
// Pool maxima (MaxLatencyMs, MaxCostPer1M, MaxStdDevMs) are pool-wide
// values shared by every candidate in one scoring pass — the reference router's
// computePoolMaxima. Score one candidate at a time only with the
// maxima of the pool it is being ranked against; scoring candidates
// from different pools against different maxima is meaningless.
type SixteenInputs struct {
	// QuotaRemainingPct is the remaining quota as a percentage,
	// 0..100 (the reference router ProviderCandidate.quotaRemaining).
	// Negative means unknown -> neutral 0.5. 0 is real: quota
	// exhausted, factor 0.
	QuotaRemainingPct float64
	// LatencyP95Ms is the candidate's p95 latency in ms. <= 0 means
	// no latency sample -> neutral 0.5.
	LatencyP95Ms float64
	// MaxLatencyMs is the largest p95 latency in the pool. <= 0
	// means the pool maximum is unknown -> neutral 0.5.
	MaxLatencyMs float64
	// CostPer1M is the candidate's price in USD per 1M tokens.
	// Negative means unknown price -> neutral 0.5 (never cheapest).
	// 0 means genuinely free -> factor 1.
	CostPer1M float64
	// MaxCostPer1M is the largest price in the pool. <= 0 means the
	// pool maximum is unknown -> neutral 0.5.
	MaxCostPer1M float64
	// SuccessRate is the observed fraction of successful attempts,
	// 0..1. Negative means no observation -> neutral 0.5. 0 is a
	// real measured rate (every attempt failed) -> factor 0.
	SuccessRate float64
	// QualityScore is the measured error-free completion rate,
	// 0..1 (same signal as Signals.QualityScore). 0 means unknown
	// -> the package's UnknownQualityScore (0.45), via
	// qualityComponent.
	QualityScore float64
	// LatencyStdDevMs is the standard deviation of the candidate's
	// latency in ms. Negative means unknown -> neutral 0.5.
	LatencyStdDevMs float64
	// MaxStdDevMs is the largest latency std-dev in the pool.
	// <= 0 means the pool maximum is unknown -> neutral 0.5.
	MaxStdDevMs float64
	// Breaker is the candidate provider's circuit-breaker state.
	// BreakerUnknown means no state supplied -> neutral 0.5.
	Breaker BreakerState
	// TaskTags are the request's task tags (e.g. ["code",
	// "review"]); ModelTags are the model's capability tags.
	// Empty on either side (or both) means no tag evidence ->
	// neutral 0.5.
	TaskTags  []string
	ModelTags []string
	// TaskFitScore is a live task-fit score for this candidate's
	// model, already looked up by the caller in the Intelligence
	// store (user_override > arena_elo > models_dev_tier) for the
	// request's task category; TaskFitKnown says the lookup hit.
	// Only then does the score vote. A miss leaves both unset and
	// FactorTaskFit falls back to the tag Jaccard below, and with
	// no tags at all to neutral 0.5. The store is the only source:
	// a score is never invented here, and a 0 is a real measured
	// 0 (an override), never an unknown — hence the separate
	// Known flag instead of a negative-score sentinel, so the
	// zero SixteenInputs value still reads neutral.
	TaskFitScore float64
	TaskFitKnown bool
	// Tier is the candidate account's subscription tier.
	// AccountTierUnknown falls back to the reference router's unknown-tier
	// base (standard). QuotaResetIntervalSecs is the account's
	// quota reset interval in seconds; <= 0 means no reset
	// information (no bonus, never a penalty).
	Tier                   AccountTier
	QuotaResetIntervalSecs float64
	// ModelTier is the candidate model's price tier;
	// RecommendedMinTier is the minimum tier the routing hint
	// recommends for this request. Either unknown -> neutral 0.5.
	ModelTier          ModelTier
	RecommendedMinTier ModelTier
	// SpecificityScore is the request's specificity score, 0..100
	// (the reference router RoutingHint.specificity.score). Negative means
	// unknown -> neutral 0.5.
	SpecificityScore float64
	// ContextTokens is the request's context size in tokens;
	// ModelContextTokens is the model's context window in tokens.
	// Either <= 0 (unknown/negative) -> neutral 0.5.
	ContextTokens      float64
	ModelContextTokens float64
	// CachedPromptFraction is the fraction of this request's
	// prompt already cached on this candidate, 0..1. Negative
	// means unknown -> neutral 0.5.
	CachedPromptFraction float64
	// SessionProvider/SessionModel identify the provider/model
	// the current session is already on. Both empty means there
	// is no session to stay on -> neutral 0.5.
	SessionProvider string
	SessionModel    string
	// ResetInSeconds is the seconds until this candidate's quota
	// resets. Negative means unknown -> neutral 0.5. 0 means
	// resetting now -> factor 1.
	ResetInSeconds float64
	// ConnectionCount is the number of connections/accounts in
	// this candidate's provider pool. <= 0 means unknown ->
	// neutral 0.5. A known 1 scores 0 (same as the reference router).
	ConnectionCount int
}

// Factors16 is the 16 computed factors for one candidate, each in
// [0, 1], 1 = best. Field order matches Weights16 exactly so a
// weight can never silently vote for a missing factor (enforced by
// TestSixteenFactorsCoverAllSixteenWeights).
type Factors16 struct {
	Quota               float64
	Health              float64
	CostInv             float64
	LatencyInv          float64
	TaskFit             float64
	Stability           float64
	TierPriority        float64
	TierAffinity        float64
	SpecificityMatch    float64
	ContextAffinity     float64
	CacheAffinity       float64
	SessionAffinity     float64
	ResetWindowAffinity float64
	ConnectionDensity   float64
	Quality             float64
	Reliability         float64
}

// Weights16 is a 16-factor weight set. Field order matches
// Factors16 exactly. Weights must be non-negative; Score16
// normalises them to sum to 1 (see NormalizeWeights16).
type Weights16 struct {
	Quota               float64
	Health              float64
	CostInv             float64
	LatencyInv          float64
	TaskFit             float64
	Stability           float64
	TierPriority        float64
	TierAffinity        float64
	SpecificityMatch    float64
	ContextAffinity     float64
	CacheAffinity       float64
	SessionAffinity     float64
	ResetWindowAffinity float64
	ConnectionDensity   float64
	Quality             float64
	Reliability         float64
}

// Sum returns the plain sum of the weights.
func (w Weights16) Sum() float64 {
	return w.Quota + w.Health + w.CostInv + w.LatencyInv +
		w.TaskFit + w.Stability + w.TierPriority + w.TierAffinity +
		w.SpecificityMatch + w.ContextAffinity + w.CacheAffinity +
		w.SessionAffinity + w.ResetWindowAffinity + w.ConnectionDensity +
		w.Quality + w.Reliability
}

// NormalizeWeights16 scales w to sum to 1. It reports false when the
// weights cannot be used as given (any negative, NaN or infinite
// component, or a total <= 0); Score16 then falls back to
// DefaultWeights16. Same contract as scoring.go's normalizeWeights.
func NormalizeWeights16(w Weights16) (Weights16, bool) {
	parts := []float64{
		w.Quota, w.Health, w.CostInv, w.LatencyInv, w.TaskFit,
		w.Stability, w.TierPriority, w.TierAffinity, w.SpecificityMatch,
		w.ContextAffinity, w.CacheAffinity, w.SessionAffinity,
		w.ResetWindowAffinity, w.ConnectionDensity, w.Quality, w.Reliability,
	}
	total := 0.0
	for _, p := range parts {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			return Weights16{}, false
		}
		total += p
	}
	if total <= 0 {
		return Weights16{}, false
	}
	scale := 1.0 / total
	return Weights16{
		Quota: w.Quota * scale, Health: w.Health * scale,
		CostInv: w.CostInv * scale, LatencyInv: w.LatencyInv * scale,
		TaskFit: w.TaskFit * scale, Stability: w.Stability * scale,
		TierPriority: w.TierPriority * scale, TierAffinity: w.TierAffinity * scale,
		SpecificityMatch:    w.SpecificityMatch * scale,
		ContextAffinity:     w.ContextAffinity * scale,
		CacheAffinity:       w.CacheAffinity * scale,
		SessionAffinity:     w.SessionAffinity * scale,
		ResetWindowAffinity: w.ResetWindowAffinity * scale,
		ConnectionDensity:   w.ConnectionDensity * scale,
		Quality:             w.Quality * scale, Reliability: w.Reliability * scale,
	}, true
}

// Price-tier thresholds (G19 Area 1, PARITY rows "auto/best-*" /
// "auto/pro-*"): the reference router's classifyTier (tierResolver.ts, with
// tierConfig.ts DEFAULT_TIER_CONFIG) classifies from the INPUT
// leg: free when input cost <= freeThreshold ($0/1M), cheap when
// input cost <= cheapThreshold ($1.00/1M), premium above that
// (provider/model overrides aside — this router has no override
// table; an operator override is a config decision, not a
// classification). ClassifyPriceTier applies those thresholds to
// real prices: the input leg when known (faithful), the blended
// price as a documented proxy when only it is known (blended is
// 60% input + 40% output, so it can only over-classify toward
// premium, never toward free — a cheap model is never sold as
// free). A negative price (unknown) classifies Unknown, never a
// guessed tier.
const (
	// FreeTierMaxInputUSDPer1M is the reference router's freeThreshold: $0/1M.
	FreeTierMaxInputUSDPer1M = 0.0
	// CheapTierMaxInputUSDPer1M is the reference router's cheapThreshold: $1.00/1M.
	CheapTierMaxInputUSDPer1M = 1.0
)

// ClassifyPriceTier classifies a (provider, model) pair into an
// economic tier from real prices. inputUSDPer1M is the input leg
// in USD per 1M tokens when hasInput; blendedUSDPer1M is the
// blended price, used only when no input leg is known. Negative
// (unknown) prices classify ModelTierUnknown.
func ClassifyPriceTier(inputUSDPer1M float64, hasInput bool, blendedUSDPer1M float64) ModelTier {
	price := blendedUSDPer1M
	if hasInput {
		price = inputUSDPer1M
	}
	if price < 0 || math.IsNaN(price) {
		return ModelTierUnknown
	}
	if price <= FreeTierMaxInputUSDPer1M {
		return ModelTierFree
	}
	if price <= CheapTierMaxInputUSDPer1M {
		return ModelTierCheap
	}
	return ModelTierPremium
}

// DefaultWeights16 is the reference router's DEFAULT_WEIGHTS (scoring.ts:62)
// with ONE deliberate divergence (G19 Area 1, gap-track RT-4):
// the reference router weights cacheAffinity, resetWindowAffinity and
// reliability 0, so those factors never vote — but this router
// now feeds all three from LIVE signals (measured cache warmth
// from the ledger, reset time from the ledger roll-off, observed
// success rate), so zeroing them throws away real data. The
// three vote at the sibling factors' weights (cacheAffinity and
// resetWindowAffinity 0.0476, like context/session affinity;
// reliability 0.03, like quality) and the other thirteen are
// rescaled proportionally (×0.8748) so the set still sums to
// exactly 1.0. The verbatim the reference router table is NOT preserved
// here — the divergence is the point — and the mode packs
// (ModePacks) carry the SAME divergence, so a pack's published
// weights are the reference tables rescaled ×0.9048 with the two
// live factors added — still checkable against modePacks.ts by
// un-scaling (see ModePacks).
var DefaultWeights16 = Weights16{
	Quota: 0.12500892, Health: 0.1404054, CostInv: 0.12500892, LatencyInv: 0.09998964,
	TaskFit: 0.06665976, Stability: 0.04164048, TierPriority: 0.04164048,
	TierAffinity: 0.04164048, SpecificityMatch: 0.04164048, ContextAffinity: 0.04164048,
	CacheAffinity: 0.0476, SessionAffinity: 0.04164048, ResetWindowAffinity: 0.0476,
	ConnectionDensity: 0.04164048, Quality: 0.026244, Reliability: 0.03,
}

// ModePacks is the reference router's MODE_PACKS (modePacks.ts), keyed by
// pack name, with ONE deliberate divergence (G19 Area 1,
// gap-track RT-4 — the same divergence as DefaultWeights16):
// the reference ships cacheAffinity and resetWindowAffinity 0 in
// every pack, so those factors never vote under any pack — but
// this router feeds both from LIVE signals (measured cache warmth
// from the ledger, reset time from the ledger roll-off), so
// zeroing them throws away real data. Each pack therefore gives
// both factors the sibling-affinity weight on the pack's 0.9999
// base — 0.0476 × 0.9999 = 0.04759524, so after the normalisation
// Score16 applies they vote at exactly 0.0476, like
// DefaultWeights16 — and rescales every other factor
// proportionally (×0.9048) so the pack still sums to 0.9999.
// Reliability was already nonzero in the reference (0.03, 0.04 in
// reliability-first) and is rescaled with the rest, preserving
// its relative standing. The reference tables are NOT preserved
// verbatim here — the divergence is the point — but every value
// below is checkable against modePacks.ts by un-scaling
// (reference = value / 0.9048 for the rescaled factors).
// tierAffinity and specificityMatch stay 0 in every pack: those
// factors only vote when a manifest routing hint is in play, per
// the reference comments ("tierAffinity/specificityMatch stay at
// 0 (manifest-routing-only weights)") — no such hint exists
// anywhere in this router yet, so the factors stay neutral
// without one.
//
// Exact per-pack tables after the divergence (headline weights
// first, remainder as rescaled — the reference ships COMPLETE
// tables, so no remainder had to be distributed proportionally;
// there is nothing left to distribute):
//
//	ship-fast:        quota .10251384, health .24131016,
//	                  costInv .02497248, latencyInv .27578304,
//	                  taskFit .08613696, stability 0,
//	                  tierPriority .03402048, contextAffinity .0085956,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .018096,
//	                  reliability .027144
//	cost-saver:       quota .10251384, health .1637688,
//	                  costInv .30075552, latencyInv .04306848,
//	                  taskFit .08613696, stability .04306848,
//	                  tierPriority .03402048,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .018096,
//	                  reliability .027144
//	quality-first:    quota .06804096, health .15508272,
//	                  costInv .02497248, latencyInv .04306848,
//	                  taskFit .31885152, stability .12929592,
//	                  tierPriority .02497248,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .027144,
//	                  reliability .027144
//	offline-friendly: quota .30075552, health .24131016,
//	                  costInv .06804096, latencyInv .04306848,
//	                  taskFit 0, stability .08613696,
//	                  tierPriority .03402048,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .018096,
//	                  reliability .027144
//	reliability-first: quota .10251384, health .31885152,
//	                  costInv .01637688, latencyInv .04306848,
//	                  taskFit .08613696, stability .1723644,
//	                  tierPriority .02497248,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .018096,
//	                  reliability .036192
//	chaos-mode:       quota .03402048, health .36192,
//	                  costInv .0126672, latencyInv .01682928,
//	                  taskFit .1723644, stability .15508272,
//	                  tierPriority .0036192,
//	                  contextAffinity .01682928,
//	                  sessionAffinity .04306848,
//	                  connectionDensity .04306848, quality .018096,
//	                  reliability .027144
//	(all packs also: cacheAffinity .04759524,
//	resetWindowAffinity .04759524)
var ModePacks = map[string]Weights16{
	"ship-fast": {
		Quota: 0.10251384, Health: 0.24131016, CostInv: 0.02497248, LatencyInv: 0.27578304,
		TaskFit: 0.08613696, Stability: 0, TierPriority: 0.03402048,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0.0085956,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.018096, Reliability: 0.027144,
	},
	"cost-saver": {
		Quota: 0.10251384, Health: 0.1637688, CostInv: 0.30075552, LatencyInv: 0.04306848,
		TaskFit: 0.08613696, Stability: 0.04306848, TierPriority: 0.03402048,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.018096, Reliability: 0.027144,
	},
	"quality-first": {
		Quota: 0.06804096, Health: 0.15508272, CostInv: 0.02497248, LatencyInv: 0.04306848,
		TaskFit: 0.31885152, Stability: 0.12929592, TierPriority: 0.02497248,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.027144, Reliability: 0.027144,
	},
	"offline-friendly": {
		Quota: 0.30075552, Health: 0.24131016, CostInv: 0.06804096, LatencyInv: 0.04306848,
		TaskFit: 0, Stability: 0.08613696, TierPriority: 0.03402048,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.018096, Reliability: 0.027144,
	},
	"reliability-first": {
		Quota: 0.10251384, Health: 0.31885152, CostInv: 0.01637688, LatencyInv: 0.04306848,
		TaskFit: 0.08613696, Stability: 0.1723644, TierPriority: 0.02497248,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.018096, Reliability: 0.036192,
	},
	"chaos-mode": {
		Quota: 0.03402048, Health: 0.36192, CostInv: 0.0126672, LatencyInv: 0.01682928,
		TaskFit: 0.1723644, Stability: 0.15508272, TierPriority: 0.0036192,
		TierAffinity: 0, SpecificityMatch: 0, ContextAffinity: 0.01682928,
		CacheAffinity: 0.04759524, SessionAffinity: 0.04306848, ResetWindowAffinity: 0.04759524,
		ConnectionDensity: 0.04306848, Quality: 0.018096, Reliability: 0.027144,
	},
}

// ModePackNames lists the mode packs in a stable order.
var ModePackNames = []string{
	"ship-fast", "cost-saver", "quality-first",
	"offline-friendly", "reliability-first", "chaos-mode",
}

// ModePack returns the weight table for a pack name, or
// DefaultWeights16 for an unknown name. The table sums to
// 0.9999 (reference rounding); Score16 normalises it before use.
// The table is the reference table rescaled for the live
// cache/reset factors (see ModePacks), not the verbatim
// modePacks.ts table.
func ModePack(name string) Weights16 {
	if w, ok := ModePacks[name]; ok {
		return w
	}
	return DefaultWeights16
}

// --- individual factors ------------------------------------------------------
// Each Factor* function is pure: same inputs, same output. 0 is worst,
// 1 is best, 0.5 is neutral (missing input). Every factor clamps its
// result to [0, 1] so one bad telemetry value cannot distort a score.

// FactorQuota scores remaining quota: quotaRemaining/100.
// 100% -> 1, 0% (exhausted) -> 0, unknown (negative) -> 0.5.
// (the reference router: clamp01(quotaRemaining/100).)
func FactorQuota(in SixteenInputs) float64 {
	if in.QuotaRemainingPct < 0 || math.IsNaN(in.QuotaRemainingPct) {
		return 0.5
	}
	return clamp01(in.QuotaRemainingPct / 100)
}

// FactorHealth scores provider health from the circuit-breaker
// state: CLOSED -> 1, HALF_OPEN -> 0.5, OPEN -> 0, unknown -> 0.5.
// (the reference router calculateFactors: health.)
func FactorHealth(in SixteenInputs) float64 {
	switch in.Breaker {
	case BreakerClosed:
		return 1.0
	case BreakerHalfOpen:
		return 0.5
	case BreakerOpen:
		return 0.0
	default:
		return 0.5
	}
}

// FactorCostInv scores cheapness pool-relatively: 1 - cost/maxCost.
// Free (0) -> 1, most expensive in the pool -> 0, unknown price
// (negative) or unknown pool max (<= 0) -> 0.5, never cheapest.
// (the reference router: clamp01(1 - costPer1MTokens/maxCost). Deliberately
// pool-relative, unlike scoring.go's absolute costComponent.)
func FactorCostInv(in SixteenInputs) float64 {
	if in.CostPer1M < 0 || math.IsNaN(in.CostPer1M) || in.MaxCostPer1M <= 0 {
		return 0.5
	}
	return clamp01(1 - in.CostPer1M/in.MaxCostPer1M)
}

// FactorLatencyInv scores speed pool-relatively: 1 - p95/maxP95.
// Fastest in the pool approaches 1, slowest -> 0, no sample
// (<= 0) or unknown pool max (<= 0) -> 0.5.
// (the reference router: clamp01(1 - p95LatencyMs/maxLatency). Deliberately
// pool-relative, unlike scoring.go's absolute latencyComponent.)
func FactorLatencyInv(in SixteenInputs) float64 {
	if in.LatencyP95Ms <= 0 || math.IsNaN(in.LatencyP95Ms) || in.MaxLatencyMs <= 0 {
		return 0.5
	}
	return clamp01(1 - in.LatencyP95Ms/in.MaxLatencyMs)
}

// FactorTaskFit scores how well the model fits the request's task:
// a live Intelligence-store lookup first (TaskFitScore, set by the
// caller from modelsync.Intelligence.TaskFit — user_override >
// arena_elo > models_dev_tier), then the Jaccard similarity
// |task ∩ model| / |task ∪ model| of the tag sets. Identical tags
// -> 1, disjoint tags -> 0, partial overlap in between, no tags on
// either side -> neutral 0.5.
// (the reference router's taskFit is a fitness-table lookup by model x
// task-type with a 0.5 fallback for unknown pairs; the live lookup
// is that table's local equivalent, and the tag overlap is the
// fallback computation from real inputs. Same neutral point, same
// [0, 1] range.)
func FactorTaskFit(in SixteenInputs) float64 {
	// Live intelligence first: the caller looked this model up in
	// the Intelligence store for the request's task category. A
	// hit votes as-is — a 0 is a real measured 0 (an override),
	// never an unknown. A miss (Known false, or a score outside
	// [0, 1] / NaN, which the store never stores) falls through
	// to the tag computation below, never to an invented score.
	if in.TaskFitKnown && !math.IsNaN(in.TaskFitScore) && in.TaskFitScore >= 0 && in.TaskFitScore <= 1 {
		return in.TaskFitScore
	}
	if len(in.TaskTags) == 0 || len(in.ModelTags) == 0 {
		return 0.5
	}
	modelSet := make(map[string]struct{}, len(in.ModelTags))
	for _, t := range in.ModelTags {
		modelSet[t] = struct{}{}
	}
	inter := 0
	seenTask := make(map[string]struct{}, len(in.TaskTags))
	for _, t := range in.TaskTags {
		if _, dup := seenTask[t]; dup {
			continue
		}
		seenTask[t] = struct{}{}
		if _, ok := modelSet[t]; ok {
			inter++
		}
	}
	union := len(seenTask) + len(modelSet) - inter
	if union <= 0 {
		return 0.5
	}
	return float64(inter) / float64(union)
}

// FactorStability scores latency steadiness pool-relatively:
// 1 - stdDev/maxStdDev. Zero variance -> 1, most variable in the
// pool -> 0, unknown variance (negative) or unknown pool max
// (<= 0) -> 0.5.
// (the reference router: clamp01(1 - latencyStdDev/maxStdDev).)
func FactorStability(in SixteenInputs) float64 {
	if in.LatencyStdDevMs < 0 || math.IsNaN(in.LatencyStdDevMs) || in.MaxStdDevMs <= 0 {
		return 0.5
	}
	return clamp01(1 - in.LatencyStdDevMs/in.MaxStdDevMs)
}

// FactorTierPriority scores the account tier, with a bonus for a
// short quota reset interval. Tier bases (the reference router
// calculateTierScore): ultra 1.0, pro 0.67, standard 0.33, free 0.0,
// unknown tier 0.33. Score = min(1, base*0.8 + resetBonus*0.2)
// where resetBonus = max(0, 1 - resetIntervalSecs/2592000) when a
// positive interval is known, else 0. A free tier with no reset
// info scores 0 — that is a real measured tier, not a missing input.
func FactorTierPriority(in SixteenInputs) float64 {
	var base float64
	switch in.Tier {
	case AccountTierUltra:
		base = 1.0
	case AccountTierPro:
		base = 0.67
	case AccountTierStandard:
		base = 0.33
	case AccountTierFree:
		base = 0.0
	default:
		base = 0.33
	}
	resetBonus := 0.0
	if in.QuotaResetIntervalSecs > 0 {
		resetBonus = math.Max(0, 1-in.QuotaResetIntervalSecs/resetWindowRefSecs)
	}
	return math.Min(1, base*0.8+resetBonus*0.2)
}

// FactorTierAffinity scores how well the model's price tier matches
// the minimum tier the routing hint recommends (the reference router
// calculateTierAffinity): same tier -> 1.0, one tier apart -> 0.7,
// two tiers apart -> 0.3, no hint or unclassified model -> 0.5.
// Tier order is free < cheap < premium.
func FactorTierAffinity(in SixteenInputs) float64 {
	if in.ModelTier == ModelTierUnknown || in.RecommendedMinTier == ModelTierUnknown {
		return 0.5
	}
	order := map[ModelTier]int{
		ModelTierFree: 0, ModelTierCheap: 1, ModelTierPremium: 2,
	}
	a, okA := order[in.ModelTier]
	b, okB := order[in.RecommendedMinTier]
	if !okA || !okB {
		return 0.5
	}
	switch diff := a - b; {
	case diff == 0:
		return 1.0
	case diff == 1 || diff == -1:
		return 0.7
	default:
		return 0.3
	}
}

// FactorSpecificityMatch scores whether the model's tier suits the
// request's specificity (the reference router calculateSpecificityMatch):
// free tier wants specificity <= 15 (0.9, else 0.2); cheap wants
// 15 < specificity <= 50 (0.9, else 0.4); premium wants
// specificity > 50 (0.9, else 0.3). Unknown tier or unknown
// specificity (negative) -> 0.5.
func FactorSpecificityMatch(in SixteenInputs) float64 {
	if in.SpecificityScore < 0 || math.IsNaN(in.SpecificityScore) {
		return 0.5
	}
	switch in.ModelTier {
	case ModelTierFree:
		if in.SpecificityScore <= 15 {
			return 0.9
		}
		return 0.2
	case ModelTierCheap:
		if in.SpecificityScore > 15 && in.SpecificityScore <= 50 {
			return 0.9
		}
		return 0.4
	case ModelTierPremium:
		if in.SpecificityScore > 50 {
			return 0.9
		}
		return 0.3
	default:
		return 0.5
	}
}

// FactorContextAffinity scores context-window headroom:
// 1 - contextTokens/modelContextTokens. An empty context fits
// anywhere (1); a context filling the window scores 0; a context
// larger than the window scores 0 (the model cannot serve it —
// filtering that out is contextfit.go's job, this only ranks).
// Unknown context or window (<= 0) -> 0.5.
// (the reference router's contextAffinity is a supplied stickiness score, not
// a window computation — see the file header. Headroom is the
// computation this package can honestly make from real inputs.)
func FactorContextAffinity(in SixteenInputs) float64 {
	if in.ContextTokens < 0 || math.IsNaN(in.ContextTokens) ||
		in.ModelContextTokens <= 0 || math.IsNaN(in.ModelContextTokens) {
		return 0.5
	}
	return clamp01(1 - in.ContextTokens/in.ModelContextTokens)
}

// FactorCacheAffinity scores prompt-cache warmth: the fraction of
// this request's prompt already cached on this candidate. Fully
// cached -> 1, nothing cached -> 0, unknown (negative) -> 0.5.
// (the reference router's cacheAffinity is a supplied score for the account
// selected by the stable prompt-cache key; the cached fraction is
// the computation this package can honestly make from real inputs.)
func FactorCacheAffinity(in SixteenInputs) float64 {
	if in.CachedPromptFraction < 0 || math.IsNaN(in.CachedPromptFraction) {
		return 0.5
	}
	return clamp01(in.CachedPromptFraction)
}

// FactorSessionAffinity scores staying on the session's current
// path: same provider AND model -> 1.0, same provider but a
// different model -> 0.7, a different provider -> 0.3, no session
// at all -> 0.5.
// (the reference router's sessionAvailability is a supplied score; this is
// the computation from the real session identity. The Candidate
// carries the identity — that is why this factor, unlike the
// others, takes c as well as in.)
func FactorSessionAffinity(c Candidate, in SixteenInputs) float64 {
	if in.SessionProvider == "" && in.SessionModel == "" {
		return 0.5
	}
	if in.SessionProvider == c.ProviderID && in.SessionModel == c.Model {
		return 1.0
	}
	if in.SessionProvider == c.ProviderID {
		return 0.7
	}
	return 0.3
}

// FactorResetWindowAffinity scores how soon the candidate's quota
// resets: 1 - resetInSeconds/2592000 (30 days). Resetting now -> 1,
// resetting in 15 days -> 0.5, in 30+ days -> 0, unknown
// (negative) -> 0.5.
// (the reference router's resetWindowAffinity is a supplied score — "sooner
// selected reset windows score higher". This is that rule computed
// from the real reset time.)
func FactorResetWindowAffinity(in SixteenInputs) float64 {
	if in.ResetInSeconds < 0 || math.IsNaN(in.ResetInSeconds) {
		return 0.5
	}
	return clamp01(1 - in.ResetInSeconds/resetWindowRefSecs)
}

// FactorConnectionDensity scores how many connections/accounts the
// provider pool offers: (count-1)/10 clamped. 1 connection -> 0,
// 6 -> 0.5, 11+ -> 1 (saturates). Unknown pool size (<= 0) -> 0.5.
// (the reference router: clamp01((connectionPoolSize ?? 1) - 1) / 10 — a known
// single connection scores 0 there too; only the unknown case
// differs, per the missing-input rule.)
func FactorConnectionDensity(in SixteenInputs) float64 {
	if in.ConnectionCount <= 0 {
		return 0.5
	}
	return clamp01(float64(in.ConnectionCount-1) / 10)
}

// FactorQuality reuses scoring.go's qualityComponent: the measured
// error-free completion rate, 0..1. QualityScore 0 means unknown ->
// UnknownQualityScore (0.45, neutral-minus-penalty), the package's
// existing convention — documented here rather than silently
// changed to 0.5.
func FactorQuality(in SixteenInputs) float64 {
	return qualityComponent(in.QualityScore)
}

// FactorReliability scores the observed success share: the
// SuccessRate itself, 0..1. 1 -> 1, 0 (every attempt failed) -> 0,
// unknown (negative) -> 0.5.
// (the reference router's reliability is 1 - failureRate with unobserved
// candidates reading 1; here an unobserved candidate reads neutral
// 0.5, per the missing-input rule — "has not failed anything" is
// not evidence it will succeed.)
func FactorReliability(in SixteenInputs) float64 {
	if in.SuccessRate < 0 || math.IsNaN(in.SuccessRate) {
		return 0.5
	}
	return clamp01(in.SuccessRate)
}

// --- combined scoring --------------------------------------------------------

// ComputeFactors16 computes all 16 factors for candidate c under inputs in.
func ComputeFactors16(c Candidate, in SixteenInputs) Factors16 {
	return Factors16{
		Quota:               FactorQuota(in),
		Health:              FactorHealth(in),
		CostInv:             FactorCostInv(in),
		LatencyInv:          FactorLatencyInv(in),
		TaskFit:             FactorTaskFit(in),
		Stability:           FactorStability(in),
		TierPriority:        FactorTierPriority(in),
		TierAffinity:        FactorTierAffinity(in),
		SpecificityMatch:    FactorSpecificityMatch(in),
		ContextAffinity:     FactorContextAffinity(in),
		CacheAffinity:       FactorCacheAffinity(in),
		SessionAffinity:     FactorSessionAffinity(c, in),
		ResetWindowAffinity: FactorResetWindowAffinity(in),
		ConnectionDensity:   FactorConnectionDensity(in),
		Quality:             FactorQuality(in),
		Reliability:         FactorReliability(in),
	}
}

// Score16 returns the combined 16-factor weighted score for candidate
// c under inputs in and weight set w. Higher is better; the result is
// clamped to [0, 1]. Weights are normalised to sum to 1 before they
// are applied (so the verbatim mode-pack tables, which sum to 0.9999,
// vote at their intended proportions); an unusable weight set falls
// back to DefaultWeights16, mirroring NewScorerWithWeights.
func Score16(c Candidate, in SixteenInputs, w Weights16) float64 {
	nw, ok := NormalizeWeights16(w)
	if !ok {
		nw = DefaultWeights16
	}
	f := ComputeFactors16(c, in)
	return clamp01(
		nw.Quota*f.Quota +
			nw.Health*f.Health +
			nw.CostInv*f.CostInv +
			nw.LatencyInv*f.LatencyInv +
			nw.TaskFit*f.TaskFit +
			nw.Stability*f.Stability +
			nw.TierPriority*f.TierPriority +
			nw.TierAffinity*f.TierAffinity +
			nw.SpecificityMatch*f.SpecificityMatch +
			nw.ContextAffinity*f.ContextAffinity +
			nw.CacheAffinity*f.CacheAffinity +
			nw.SessionAffinity*f.SessionAffinity +
			nw.ResetWindowAffinity*f.ResetWindowAffinity +
			nw.ConnectionDensity*f.ConnectionDensity +
			nw.Quality*f.Quality +
			nw.Reliability*f.Reliability,
	)
}

// Rank16 orders candidates best-first by Score16 under weight set w,
// stably: equal scores keep their input order. It never drops or
// duplicates a candidate — the result is a permutation of the input.
// Inputs are keyed by the full candidate (provider AND model AND
// effort, as in Rank); a candidate with no entry scores from the
// zero SixteenInputs value, i.e. neutral 0.5 on every factor whose
// input is missing (quality 0.45, tierPriority 0.264).
func Rank16(candidates []Candidate, inputs map[Candidate]SixteenInputs, w Weights16) []Candidate {
	type scored struct {
		c     Candidate
		score float64
	}
	items := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		in, _ := inputs[c]
		items = append(items, scored{c: c, score: Score16(c, in, w)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]Candidate, 0, len(items))
	for _, it := range items {
		out = append(out, it.c)
	}
	return out
}
