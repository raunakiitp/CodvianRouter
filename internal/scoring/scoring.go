// Package scoring ranks provider/model candidates for the Codvian Router
// fallback chain by cost, latency, quality and reliability signals.
//
// Honest limitations — read before trusting a ranking:
//
//   - Quality is a MEASURED signal, defined exactly as follows: the
//     error-free completion rate over real attempts — successes /
//     attempts, counted only from attempts that actually ran
//     (hedge losers and limiter-shed calls are never counted).
//     The caller supplies it as Signals.QualityScore; 0 always
//     means "no attempts yet" (unknown, neutral-minus-penalty),
//     never a measured zero. Quality is NOT a benchmark score,
//     NOT an eval judgement, NOT a claimed capability rating and
//     NOT a vendor number: it says nothing about how good an
//     answer is, only about how often attempts through that
//     provider completed without error. A provider with no
//     attempts yet has unknown quality, however good its models
//     may be claimed to be.
//   - Latency, success rate and cost are likewise only as fresh and
//     honest as the Signals the caller supplies. Stale or invented
//     signals produce stale or invented rankings; the scorer never
//     verifies them.
//   - Scores are relative, unitless weighted sums for ordering only.
//     A score of 0.8 does not mean "80% good" in any absolute sense.
//
// The scorer is pure and deterministic: the same candidates, signals
// and mode always produce the same order. It makes no network calls and
// holds no state, so it is safe to call from the router's OrderHook.
package scoring

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// Candidate is one provider/model pair that could serve a request.
type Candidate struct {
	// ProviderID identifies the provider (router.Attempt.ProviderID).
	ProviderID string
	// Model is the model ID on that provider.
	Model string
	// Effort is the reasoning effort this candidate runs at
	// (CHECKLIST 255): "" means no explicit effort, otherwise one
	// of the provider.ReasoningEffort* levels. A routing-group
	// entry is (model, effort), so the same model at different
	// efforts is a different candidate — signals are keyed by the
	// full candidate (provider AND model AND effort), so each
	// effort level ranks by its own measured signals, never by a
	// blended or model-only price. The zero value ("") keeps
	// existing callers working: a candidate with no effort is
	// the pair at no explicit effort.
	Effort string
}

// Signals are the measurements a candidate is scored on. All fields are
// caller-supplied; zero values mean "unknown", not "measured zero".
// In particular QualityScore 0 means no quality signal exists yet —
// see the package docs — and LatencyMs <= 0 means no latency sample.
type Signals struct {
	// LatencyMs is the recent p50 latency in milliseconds. <= 0 means
	// no latency sample is available.
	LatencyMs float64
	// LatencyP95Ms is the true p95 latency in milliseconds from the
	// engine's latency histogram (B9). <= 0 means unavailable
	// (insufficient samples). When available, SLA-aware routing
	// uses this instead of the p50 proxy.
	LatencyP95Ms float64
	// SuccessRate is the recent fraction of successful attempts, 0..1.
	// 0 means unknown-or-always-failing; callers should only pass
	// measured rates.
	SuccessRate float64
	// CostUSDPer1M is the blended price in USD per 1M tokens.
	// Negative means "no price known" (neutral 0.5 on the cost
	// component — never the cheapest); 0 means genuinely free.
	CostUSDPer1M float64
	// QualityScore is the MEASURED quality signal for the candidate:
	// the error-free completion rate over real attempts
	// (successes / attempts, real attempts only — see the package
	// docs), in [0, 1]. 0 means unknown (no attempts yet): it is
	// treated as neutral-minus-penalty, never as measured quality.
	// A genuinely measured rate of exactly 0 (every attempt
	// failed) cannot be distinguished from "no signal" and is
	// treated the same way.
	QualityScore float64
	// QuotaHint is the remaining-quota hint (same scale as
	// router.Config.QuotaHint: higher = more headroom, 0 = none).
	QuotaHint int
	// Util5h is the measured utilisation of this pair's quota
	// over the rolling 5-hour window: priced spend in the window
	// divided by the pair's limit, in [0, 1]. -1 means unknown:
	// the system holds no per-(provider, model) limit to divide
	// by, so no utilisation can be computed — never an invented
	// limit. Callers that fill this field must set it explicitly
	// to -1 when unknown: the zero value reads as "0% utilised",
	// which is only true when the spend was actually measured
	// against a known limit. (M3 Phase C quota plumbing.)
	Util5h float64
	// Util7d is the same utilisation over the rolling 7-day
	// window. -1 means unknown, for the same reason as Util5h:
	// no per-pair limit exists. (M3 Phase C quota plumbing.)
	Util7d float64
	// ResetAt is when this pair's current quota window next
	// frees capacity: the oldest priced spend in the rolling
	// 5-hour window ages out at this time. Zero means unknown:
	// no priced spend sits in the window, so there is nothing
	// to roll off. Never a calendar reset — the window rolls.
	// (M3 Phase C quota plumbing.)
	ResetAt time.Time
	// ExpiresAt is when this pair's quota grant expires. Zero
	// means unknown: no quota-expiry data has been learned for
	// this pair yet — the engine's learned hint (signalsFn)
	// supplies the upstream window-reset instant once a real
	// response has taught it, and until then there is no expiry
	// to report. (M3 Phase C quota plumbing.)
	ExpiresAt time.Time
	// Load is this provider's current load from the engine's
	// adaptive concurrency limiter: in-flight requests divided
	// by the live limit, in [0, 1]. 0 means unknown (no limiter
	// state for this provider) or idle — both score the same
	// (no load penalty), so the ambiguity is harmless. (M3
	// Phase C quota plumbing.)
	Load float64
	// Attempts is the measured attempt count for this provider
	// from the engine's live attempt record — the same source
	// SuccessRate comes from. -1 means unknown: the engine has
	// no recorded attempts for this provider yet. Per provider,
	// not per model: the engine's attempt record is keyed by
	// provider, exactly like SuccessRate. (M3 Phase C quota
	// plumbing.)
	Attempts int
	// BreakerOpen is true when this provider's circuit breaker
	// is positively known OPEN (M3 Area 1, PARITY row 394):
	// the sla-aware hook filters such candidates before
	// scoring, and the 16-factor scorer's health factor reads
	// it. False means "not open OR unknown" — the hook must
	// treat it as "no breaker information", i.e. score
	// normally, never as proof of health (an OPEN filter
	// only ever removes candidates positively known OPEN).
	BreakerOpen bool
	// BreakerDegraded is true when this provider's circuit
	// breaker is positively known DEGRADED (RT-2 follow-up):
	// consecutive failures at/above the profile's degrade
	// threshold but below the open threshold — the provider
	// is still serving, but slowing/failing (see
	// Engine.BreakerDegradedForProvider, the read of
	// breakerStateOf's DEGRADED state). The LKGP hook treats
	// a degraded pin like an open one: it is never hoisted.
	// OPEN and DEGRADED are mutually exclusive states, so a
	// provider that is OPEN reports false here — OPEN is
	// already excluded by the BreakerOpen check, and the
	// hook must not double-count it. HALF_OPEN also reports
	// false (cooling down, probe allowed — not degrading).
	// False means "not degraded OR unknown" and must never
	// be read as proof of health.
	BreakerDegraded bool
	// BreakerClosed is true only when this provider's breaker
	// is positively known CLOSED (G19 Area 1: the engine
	// reports CLOSED for every provider with a registered
	// client whose breaker is not open — see
	// Engine.BreakerClosedForProvider). False means "not
	// closed OR unknown" and must never be read as "open":
	// the 16-factor health factor scores it neutral, and
	// only BreakerOpen ever demotes. A candidate with both
	// BreakerOpen and BreakerClosed true is contradictory
	// input; BreakerOpen wins (fail-closed).
	BreakerClosed bool
	// QuotaRemainingPct is the remaining quota as a
	// percentage, 0..100, from the same measured hint the
	// scalar QuotaHint booleanises (G19 Area 1): the
	// registry/probe quota hint, or the engine's learned
	// hint, as a fraction, when one was actually measured. Negative means unknown — no hint
	// was measured — and callers must set it explicitly to
	// -1 when unknown: the zero value reads as "0%
	// remaining" (exhausted) only together with
	// QuotaHint <= 0 (see strategy.sixteenInputs), never on
	// its own. A positive value is a real measured fraction,
	// never a default: QuotaHintFunc's 100-for-unknown is
	// NOT copied here.
	QuotaRemainingPct float64
	// ConnectionCount is the number of accounts/connections
	// in this candidate's provider pool (the provider's key
	// pool size), for the 16-factor connectionDensity factor.
	// -1 means unknown; 0 is never written (an unknown pool
	// size must not score as a known single connection).
	// Callers must set it explicitly to -1 when unknown.
	ConnectionCount int
	// ContextWindow is the model's context window in tokens,
	// when the registry knows it (operator-set or models.dev
	// synced). 0 means unknown — never a guessed window.
	// The 16-factor contextAffinity factor and the
	// context-optimized hook read it.
	ContextWindow int
}

// Scorer ranks one candidate against one set of signals. Higher scores
// are better; scores are only comparable within the same mode and the
// same signals snapshot.
type Scorer interface {
	Score(c Candidate, s Signals) float64
}

// Mode selects a scoring preset (a fixed set of weights).
type Mode string

// Scoring modes. Each mode is a preset of documented weights that sum
// to 1; see Modes for the numbers.
const (
	// ModeBalanced weighs latency, reliability, cost and quality
	// roughly equally, with a small quota term.
	ModeBalanced Mode = "balanced"
	// ModeFast is latency-heavy: pick the fastest provider that is
	// still reasonably reliable.
	ModeFast Mode = "fast"
	// ModeCheap is cost-heavy: pick the cheapest provider that is
	// still reasonably reliable.
	ModeCheap Mode = "cheap"
	// ModeQuality is quality-heavy. Unknown quality (QualityScore 0)
	// is treated as neutral-minus-penalty, so it can never outrank a
	// known-good candidate on quality alone. Quality here is the
	// measured error-free completion rate from real attempts (see
	// the package docs), never a benchmark or claimed score — so
	// this mode ranks providers by how often their real attempts
	// complete without error, not by how good their answers are
	// claimed to be.
	ModeQuality Mode = "quality"
	// ModeReliable is success-rate and quota heavy: prefer providers
	// that succeed and still have headroom.
	ModeReliable Mode = "reliable"
	// ModeLatencyAware is latency-dominant with a reliability guard:
	// prefer the lowest-latency candidate among viable ones, with
	// cost/quality/quota kept as small tiebreakers only. It is the
	// weight set behind strategy "latency-aware" (G10 Area 12 row 9).
	// Our own weights — NOT the reference router's 16-factor latencyInv
	// (0.1143): latency 0.70, success 0.15, cost 0.05, quality
	// 0.05, quota 0.05. It differs from ModeFast (latency 0.55,
	// still a general-purpose fast mode) and from least-latency
	// (0.80/0.20, no cost/quality/quota at all): latency-aware
	// lets a large cost gap beat a noise-level latency gap (see
	// TestLatencyAwareNoiseGapLosesToLargeCostGap), while
	// least-latency never lets cost move the order.
	ModeLatencyAware Mode = "latency-aware"
)

// Weights are the five component weights of a mode. They must sum to 1.
// Fields are in the same order as the score components.
type Weights struct {
	// Latency weight (lower latency scores higher).
	Latency float64
	// Success weight (higher success rate scores higher).
	Success float64
	// Cost weight (lower cost scores higher).
	Cost float64
	// Quality weight (higher measured quality scores higher).
	Quality float64
	// Quota weight (more remaining quota scores higher).
	Quota float64
}

// Modes maps every Mode to its preset weights. Every entry sums to 1.
var Modes = map[Mode]Weights{
	ModeBalanced: {Latency: 0.25, Success: 0.25, Cost: 0.20, Quality: 0.20, Quota: 0.10},
	ModeFast:     {Latency: 0.55, Success: 0.20, Cost: 0.05, Quality: 0.10, Quota: 0.10},
	ModeCheap:    {Latency: 0.10, Success: 0.15, Cost: 0.55, Quality: 0.10, Quota: 0.10},
	ModeQuality:  {Latency: 0.10, Success: 0.20, Cost: 0.05, Quality: 0.55, Quota: 0.10},
	ModeReliable: {Latency: 0.10, Success: 0.45, Cost: 0.05, Quality: 0.10, Quota: 0.30},
	// Latency-aware (G10 Area 12 row 9): latency-dominant, our own
	// weights — see ModeLatencyAware.
	ModeLatencyAware: {Latency: 0.70, Success: 0.15, Cost: 0.05, Quality: 0.05, Quota: 0.05},
}

// weightedScorer is the Scorer implementation: a weighted sum of the
// five normalised components, each in [0, 1].
type weightedScorer struct {
	w Weights
}

// NewScorer returns a Scorer for the given mode. An unknown mode falls
// back to ModeBalanced; it never panics and never returns nil.
func NewScorer(mode Mode) Scorer {
	w, ok := Modes[mode]
	if !ok {
		w = Modes[ModeBalanced]
	}
	return weightedScorer{w: w}
}

// NewScorerWithWeights returns a Scorer for an explicit weight set
// (the "weighted" strategy: the operator supplies the weights).
// Weights are normalised to sum to 1; a zero or negative total, or
// any negative/NaN component, falls back to ModeBalanced — a bad
// custom weight set can never produce a negative or unbounded score.
func NewScorerWithWeights(w Weights) Scorer {
	if nw, ok := normalizeWeights(w); ok {
		return weightedScorer{w: nw}
	}
	return weightedScorer{w: Modes[ModeBalanced]}
}

// normalizeWeights scales w to sum to 1. It reports false when the
// weights cannot be used as given (any negative, NaN or infinite
// component, or a total <= 0).
func normalizeWeights(w Weights) (Weights, bool) {
	parts := []float64{w.Latency, w.Success, w.Cost, w.Quality, w.Quota}
	total := 0.0
	for _, p := range parts {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			return Weights{}, false
		}
		total += p
	}
	if total <= 0 {
		return Weights{}, false
	}
	return Weights{
		Latency: w.Latency / total,
		Success: w.Success / total,
		Cost:    w.Cost / total,
		Quality: w.Quality / total,
		Quota:   w.Quota / total,
	}, true
}

// UnknownQualityScore is the effective quality component for a
// candidate with no quality signal (QualityScore 0): neutral 0.5 minus
// a small penalty, so unknown quality never beats known-good quality
// and never beats known-bad quality by much either — it sits just
// below neutral.
const UnknownQualityScore = 0.45

// unknownQualityPenalty is the penalty subtracted from neutral 0.5
// for an unknown quality score.
const unknownQualityPenalty = 0.05

// latencyRefMs is the reference latency for the latency component:
// 1/(1+LatencyMs/latencyRefMs). 1000ms scores 0.5, 100ms ~0.91,
// 5000ms ~0.17. The value is a normalisation constant, not a measured
// property of any provider.
const latencyRefMs = 1000.0

// costRefUSD is the reference cost for the cost component:
// 1/(1+CostUSDPer1M/costRefUSD). $10/1M scores 0.5, $1/1M ~0.91,
// $100/1M ~0.09. The value is a normalisation constant, not a measured
// property of any provider.
const costRefUSD = 10.0

// quotaRef is the quota-hint value that scores a full 1.0; hints are
// clamped to [0, quotaRef] before dividing.
const quotaRef = 100.0

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// latencyComponent normalises latency to [0, 1], higher = faster.
// LatencyMs <= 0 means no sample: neutral 0.5, never the best.
func latencyComponent(latencyMs float64) float64 {
	if latencyMs <= 0 || math.IsNaN(latencyMs) || math.IsInf(latencyMs, 0) {
		return 0.5
	}
	return 1.0 / (1.0 + latencyMs/latencyRefMs)
}

// LatencyComponent exposes latencyComponent for the p2c draw in
// internal/strategy (G12 Area 12 row 9): the sign of the pairwise
// P2CLatencyTerm comes from comparing the two candidates'
// components, and the comparison must use the same normalisation
// the term itself uses — a local copy would drift.
func LatencyComponent(latencyMs float64) float64 {
	return latencyComponent(latencyMs)
}

// costComponent normalises cost to [0, 1], higher = cheaper.
// A negative cost means "unknown" (no price configured): neutral
// 0.5, never the cheapest — an unpriced provider must not outrank a
// known-cheap one on cost alone. Cost 0 (a genuinely free model)
// scores 1.0. Positive infinity scores 0.
func costComponent(costUSDPer1M float64) float64 {
	if math.IsNaN(costUSDPer1M) {
		return 0.5
	}
	if costUSDPer1M < 0 {
		return 0.5
	}
	if math.IsInf(costUSDPer1M, 1) {
		return 0
	}
	return 1.0 / (1.0 + costUSDPer1M/costRefUSD)
}

// qualityComponent normalises quality to [0, 1], higher = better.
// QualityScore 0 means unknown: neutral 0.5 minus the small penalty
// (UnknownQualityScore), so unknown quality can never outrank
// known-good quality on this component.
func qualityComponent(qualityScore float64) float64 {
	if qualityScore == 0 {
		return UnknownQualityScore
	}
	return clamp01(qualityScore)
}

// quotaComponent normalises a quota hint to [0, 1], higher = more
// headroom. Hints above quotaRef saturate at 1; negative hints are 0.
func quotaComponent(quotaHint int) float64 {
	if quotaHint <= 0 {
		return 0
	}
	return clamp01(float64(quotaHint) / quotaRef)
}

// isEmpty reports whether s carries no measured signal at all: every
// measured field is its zero value. QuotaHint is deliberately excluded:
// a candidate that has only a quota hint is still "no signals" and is
// scored by quota hint only (see Score), so unknown latency/cost are
// never mistaken for "instant and free".
func (s Signals) isEmpty() bool {
	return s.LatencyMs == 0 && s.SuccessRate == 0 && s.CostUSDPer1M == 0 &&
		s.QualityScore == 0
}

// Score returns the weighted score for candidate c under signals s.
// Higher is better. The Candidate itself carries no scored fields —
// everything scored comes from s — but it is kept in the signature so
// future scorers can use provider/model identity.
//
// A candidate with no measured signals (latency, success, cost and
// quality all zero — including the zero Signals value) is scored by
// quota hint only: the weighted quota component alone, with every other
// component contributing 0. With no hint either that is 0, so an
// entirely unknown candidate always ranks last among candidates with
// any real signal, but is never dropped.
func (sc weightedScorer) Score(c Candidate, s Signals) float64 {
	if s.isEmpty() {
		return sc.w.Quota * quotaComponent(s.QuotaHint)
	}
	return sc.w.Latency*latencyComponent(s.LatencyMs) +
		sc.w.Success*clamp01(s.SuccessRate) +
		sc.w.Cost*costComponent(s.CostUSDPer1M) +
		sc.w.Quality*qualityComponent(s.QualityScore) +
		sc.w.Quota*quotaComponent(s.QuotaHint)
}

// RankWithWeights orders candidates best-first by score under an
// explicit weight set, stably. It never drops or duplicates a
// candidate — the result is a permutation of the input. Signals are
// keyed by the full candidate (provider AND model AND effort): the
// same model at two providers can cost differently, so a
// provider-only key would rank both by one price, and the same
// model at two efforts can cost and answer differently, so a
// (provider, model) key would rank both efforts by one signal. A
// candidate with no entry scores by quota hint only (see
// Scorer.Score).
func RankWithWeights(candidates []Candidate, signals map[Candidate]Signals, w Weights) []Candidate {
	scorer := NewScorerWithWeights(w)
	type scored struct {
		c     Candidate
		score float64
	}
	items := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		s, _ := signals[c]
		items = append(items, scored{c: c, score: scorer.Score(c, s)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]Candidate, 0, len(items))
	for _, it := range items {
		out = append(out, it.c)
	}
	return out
}

// Rank orders candidates best-first by score under the given mode and
// signals, stably: equal scores keep their input order. It never drops
// or duplicates a candidate — the result is a permutation of the input
// (same length, same elements). Signals are keyed by the full
// candidate (provider AND model AND effort), so the same model at two
// providers ranks by each pair's own price, and the same model at two
// efforts ranks by each effort's own signals. Candidates with no entry in signals
// are scored by quota hint only (see Scorer.Score); a candidate with
// no signals entry and no quota hint scores 0 and ranks last.
func Rank(candidates []Candidate, signals map[Candidate]Signals, mode Mode) []Candidate {
	scorer := NewScorer(mode)
	type scored struct {
		c     Candidate
		score float64
	}
	items := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		s, _ := signals[c]
		items = append(items, scored{c: c, score: scorer.Score(c, s)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]Candidate, 0, len(items))
	for _, it := range items {
		out = append(out, it.c)
	}
	return out
}

// --- Power-of-two-choices latency term (G10 Area 12 row 9) -----------
//
// The p2c latency term makes latency an explicit, weighted component
// of a two-choice draw instead of a tiebreak-only afterthought: draw
// two candidates, compare their weighted latency components, and the
// lower-latency one wins the draw. The term is the weighted latency
// advantage of the faster candidate:
//
//	w.Latency * (latencyComponent(faster) - latencyComponent(slower))
//
// in [0, w.Latency]. It is 0 when the latencies are equal or when
// neither candidate has a latency sample (unknown latency never
// wins a draw on latency alone — the draw then falls back to the
// full weighted score). It is deliberately NOT the reference router's
// getP2CTargetScore (successRate/100 + 1/log10(avgLatencyMs+10)):
// ours reuses the same 1/(1+ms/1000) normalisation the rest of the
// scorer uses, so the term is directly comparable with the latency
// component of Score instead of living on a second scale.

// P2CLatencyTerm returns the weighted latency advantage of the
// faster of a and b under weights w: w.Latency times the difference
// of their latency components. 0 when the latencies tie or neither
// has a sample. Always >= 0.
func P2CLatencyTerm(a, b Signals, w Weights) float64 {
	if w.Latency <= 0 {
		return 0
	}
	ca, cb := latencyComponent(a.LatencyMs), latencyComponent(b.LatencyMs)
	if ca == cb {
		return 0
	}
	if ca > cb {
		return w.Latency * (ca - cb)
	}
	return w.Latency * (cb - ca)
}

// PickP2C picks the winner of a two-choice draw between a and b: the
// lower-latency candidate wins when the weighted latency term is
// non-zero; on a latency tie (including both unknown) the higher
// full weighted Score wins; on an exact score tie a wins. It never
// drops a candidate — the loser is simply not returned.
func PickP2C(a, b Candidate, signals map[Candidate]Signals, mode Mode) Candidate {
	w, ok := Modes[mode]
	if !ok {
		w = Modes[ModeBalanced]
	}
	sa, sb := signals[a], signals[b]
	if term := P2CLatencyTerm(sa, sb, w); term > 0 {
		if latencyComponent(sa.LatencyMs) > latencyComponent(sb.LatencyMs) {
			return a
		}
		return b
	}
	scorer := NewScorer(mode)
	if scorer.Score(b, sb) > scorer.Score(a, sa) {
		return b
	}
	return a
}

// RankP2C orders candidates best-first by repeated power-of-two-
// choices draws under mode: while candidates remain, draw two
// distinct ones uniformly at random, let PickP2C choose the winner,
// emit it, and repeat with the rest. The result is a permutation of
// the input (never drops or duplicates). A nil rnd gets a fresh
// time-seeded source; pass a seeded *rand.Rand (see
// RankP2CSeeded) for a reproducible order in tests.
func RankP2C(candidates []Candidate, signals map[Candidate]Signals, mode Mode, rnd *rand.Rand) []Candidate {
	if len(candidates) <= 1 {
		return append([]Candidate(nil), candidates...)
	}
	r := rnd
	if r == nil {
		r = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	rest := append([]Candidate(nil), candidates...)
	out := make([]Candidate, 0, len(rest))
	for len(rest) > 1 {
		i := r.Intn(len(rest))
		j := r.Intn(len(rest) - 1)
		if j >= i {
			j++
		}
		winner := PickP2C(rest[i], rest[j], signals, mode)
		out = append(out, winner)
		kept := rest[:0]
		dropped := false
		for _, c := range rest {
			if !dropped && c == winner {
				dropped = true
				continue
			}
			kept = append(kept, c)
		}
		rest = kept
	}
	return append(out, rest...)
}

// RankP2CSeeded is RankP2C with a deterministic seed: the same
// candidates, signals, mode and seed always produce the same order.
func RankP2CSeeded(candidates []Candidate, signals map[Candidate]Signals, mode Mode, seed int64) []Candidate {
	return RankP2C(candidates, signals, mode, rand.New(rand.NewSource(seed)))
}

// best-scoring (provider, model) pair offering each model. offers
// reports the pairs that can serve a model (every enabled provider
// offering it; empty means no provider is known to offer it, and
// the model is then scored on its own signals — the model-level
// price fallback, with no provider's latency/success). signals
// reports the live signals for one pair (its own price, that
// provider's latency/success/quality). Each model inherits its
// best pair's score, so a model that is cheap at one provider and
// expensive at another ranks by its cheap pair — the pair the
// router would actually serve it through — never by a blended or
// model-only price. The result is a permutation of the input
// (same models, same count); ties keep input order. This is what
// the auto-router's tier ordering uses (wired in cmd as the
// RankModels ranker), so the cheapest strategy and the tier
// ordering agree on the pair price.
func RankModelsByBestPair(models []string, offers func(model string) []Candidate, signals func(Candidate) Signals, mode Mode) []string {
	scorer := NewScorer(mode)
	type scored struct {
		model string
		score float64
	}
	items := make([]scored, 0, len(models))
	for _, m := range models {
		pairs := offers(m)
		if len(pairs) == 0 {
			// No provider known to offer this model: score
			// the bare model (model-level fallback price,
			// no provider signals) instead of dropping it.
			pairs = []Candidate{{Model: m}}
		}
		best := math.Inf(-1)
		for _, c := range pairs {
			if c.Model == "" {
				c.Model = m
			}
			if s := scorer.Score(c, signals(c)); s > best {
				best = s
			}
		}
		items = append(items, scored{model: m, score: best})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.model)
	}
	return out
}

// RankModelsByBestPair16 is RankModelsByBestPair under the
// 16-factor scorer (G19 Area 1, PARITY rows "auto 16-factor
// scoring" / "Router strategy rules" / "Router strategy score"):
// each model inherits its best pair's Score16 under weight set w,
// so the auto pool is ranked by all 16 factors — quota, health,
// cost, latency, taskFit, stability, tiers, specificity,
// context/cache/session affinity, reset-window affinity,
// connection density, quality, reliability — instead of the
// 5-factor balanced score. inputs reports the 16-factor inputs
// for one pair; the caller leaves MaxLatencyMs / MaxCostPer1M /
// MaxStdDevMs unset (<= 0) and this function fills them from the
// pool maxima over every pair of every model, exactly like
// the reference router's computePoolMaxima: the pool-relative factors are
// meaningless against any other maxima. A pair's inputs are read
// once per model (the maxima pass) and reused for scoring. The
// result is a permutation of the input; ties keep input order.
func RankModelsByBestPair16(models []string, offers func(model string) []Candidate, inputs func(Candidate) SixteenInputs, w Weights16) []string {
	type scored struct {
		model string
		score float64
	}
	// First pass: collect every pair's inputs and the pool maxima.
	type pairInputs struct {
		c  Candidate
		in SixteenInputs
	}
	perModel := make([][]pairInputs, len(models))
	maxLatency, maxCost, maxStdDev := -1.0, -1.0, -1.0
	for mi, m := range models {
		pairs := offers(m)
		if len(pairs) == 0 {
			pairs = []Candidate{{Model: m}}
		}
		for _, c := range pairs {
			if c.Model == "" {
				c.Model = m
			}
			in := inputs(c)
			if in.LatencyP95Ms > maxLatency {
				maxLatency = in.LatencyP95Ms
			}
			if in.CostPer1M > maxCost {
				maxCost = in.CostPer1M
			}
			if in.LatencyStdDevMs > maxStdDev {
				maxStdDev = in.LatencyStdDevMs
			}
			perModel[mi] = append(perModel[mi], pairInputs{c: c, in: in})
		}
	}
	items := make([]scored, 0, len(models))
	for mi, m := range models {
		best := math.Inf(-1)
		for _, pi := range perModel[mi] {
			in := pi.in
			in.MaxLatencyMs = maxLatency
			in.MaxCostPer1M = maxCost
			in.MaxStdDevMs = maxStdDev
			if s := Score16(pi.c, in, w); s > best {
				best = s
			}
		}
		items = append(items, scored{model: m, score: best})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.model)
	}
	return out
}

// ---------------------------------------------------------------------------
// 16-factor default scoring (Stage 1, B2-05/B2-10/B2-11/B2-12/B2-16:
// the M3-gate unlock — Area 1 weighted 79.4% -> >= 80%).
//
// The 16 factor functions, DefaultWeights16, the mode packs and
// Score16/Rank16 already live in scoring16.go, and the mode-pack
// chain hooks (strategy.sixteenHookCtx) and the plain-auto tier
// ranking (main.go rankModels -> RankModelsByBestPair16) already
// use them. What was missing was the DEFAULT scorer: the default
// ("balanced") chain hook still ranked with the 5-factor
// HookWithWeights, so 11 of the 16 factors never voted on the
// default path. This section is that default scorer: the
// signal->inputs converter plus the 16-factor chain hook under
// the reference router's DEFAULT_WEIGHTS. Wiring it in as the default hook
// is one case in strategy.wiringHook (StrategyBalanced ->
// Hook16Default) — a follow-up in the strategy lane, NOT done
// here, because wiring.go belongs to another Stage 1 builder.
// Until that case lands, the default chain still scores 5-factor;
// this hook is the complete, ready-to-wire replacement, and the
// test phase verifies it directly (Hook16Default orders a
// discriminating chain by 16 factors; HookWithWeights does not).
// ---------------------------------------------------------------------------

// Inputs16FromSignals builds the signal-derived 16-factor inputs
// for one candidate from live scoring signals. Every factor with
// a live source is filled from real data; every factor without
// one sits at its documented neutral value (see scoring16.go).
// Pool maxima are left unset (<= 0): the caller fills them from
// the pool it ranks against (Hook16 does).
//
//   - Quota: the measured remaining fraction when the signal
//     carries one (QuotaRemainingPct > 0 — a real probe/registry
//     measurement); otherwise the booleanised scalar QuotaHint
//     (<= 0 means exhausted, factor 0; > 0 means headroom
//     remains but the fraction is unknown, factor neutral).
//     A QuotaRemainingPct of exactly 0 with QuotaHint > 0 is
//     contradictory input and reads as unknown, never as
//     exhausted: the scalar hint is the older, authoritative
//     source and it says headroom remains.
//   - Latency: the measured p50 EWMA, fed as the latency value
//     (pool-relative, so the ranking reflects measured latency;
//     true p95 arrives with the latency-histogram plumbing).
//   - Cost / success / quality: straight from the signals.
//   - Health: OPEN when positively open, CLOSED when positively
//     closed, unknown otherwise — never claimed CLOSED on no
//     evidence. A contradictory OPEN + CLOSED input reads OPEN
//     (fail-closed).
//   - Reset: the real ledger roll-off time when known.
//   - Connection density: the live key-pool size when known.
//   - Context window: the registry/model window when known.
//   - Still neutral (no live source anywhere): taskFit tags,
//     tierPriority (no account-tier source), tierAffinity /
//     specificityMatch (the model's own tier IS now classified
//     from its real price — see below — but the routing hint
//     that names a recommended tier / specificity score does
//     not exist anywhere, so the factors stay neutral without
//     one), stability (no latency-stddev source), sessionAffinity
//     (no session store on this hook — the LKGP session pair is
//     layered by the caller that owns the session, e.g. the
//     plain-auto ranker in main.go), cacheAffinity (needs the
//     per-request warmth func, which a bare chain hook does not
//     have — the mode-pack hook layers it).
//
// This is the scoring-package home of the mapping that
// strategy.SixteenInputsFromSignals also implements: the two must
// stay in step (same neutrals, same rules) until the strategy
// package adopts this one — deleting the duplicate is a
// follow-up, not part of this slice, because strategy/wiring.go
// belongs to another Stage 1 builder.
func Inputs16FromSignals(s Signals) SixteenInputs {
	in := SixteenInputs{
		QuotaRemainingPct:    -1, // unknown fraction -> neutral 0.5
		LatencyP95Ms:         -1, // p50 signal fed below; -1 when none
		MaxLatencyMs:         -1,
		CostPer1M:            -1, // unknown price -> neutral 0.5
		MaxCostPer1M:         -1,
		SuccessRate:          -1, // no observation -> neutral 0.5
		QualityScore:         0,  // unknown -> package neutral 0.45
		LatencyStdDevMs:      -1,
		MaxStdDevMs:          -1,
		Breaker:              BreakerUnknown,
		Tier:                 AccountTierUnknown,
		ModelTier:            ModelTierUnknown,
		SpecificityScore:     -1,
		ContextTokens:        -1,
		ModelContextTokens:   0,
		CachedPromptFraction: -1,
		ResetInSeconds:       -1,
		ConnectionCount:      0, // unknown pool size -> neutral 0.5
	}
	if s.QuotaRemainingPct > 0 {
		// Real measured fraction (0..100).
		in.QuotaRemainingPct = s.QuotaRemainingPct
	} else if s.QuotaHint <= 0 {
		in.QuotaRemainingPct = 0 // no headroom: exhausted, factor 0
	}
	if s.LatencyMs > 0 {
		// Pool-relative latency factor: the p50 signal is fed as
		// the latency value (see file header); true p95 is Phase C.
		in.LatencyP95Ms = s.LatencyMs
	}
	if s.CostUSDPer1M >= 0 {
		in.CostPer1M = s.CostUSDPer1M
		// Economic tier from the real price: ClassifyPriceTier
		// on the blended price (no input leg reaches this
		// scorer) — free at $0, cheap at <= $1/M, premium
		// above — so tierAffinity/specificityMatch vote on
		// measured prices instead of staying neutral. The
		// factors still need a routing hint (recommended tier /
		// specificity score) to vote: without one they stay
		// neutral, as documented in scoring16.go.
		in.ModelTier = ClassifyPriceTier(0, false, s.CostUSDPer1M)
	}
	if s.Attempts > 0 {
		// Measured rate, even 0: every attempt failed is a
		// real 0, not unknown. (The old `SuccessRate > 0`
		// test left a measured-0 success rate at neutral 0.5,
		// ranking a provider that fails everything as if it
		// had never been tried.)
		in.SuccessRate = s.SuccessRate
	}
	// Breaker state: a positively OPEN breaker reads as OPEN
	// for the health factor; a positively CLOSED one reads as
	// CLOSED; false on both means "not open OR unknown" and
	// stays Unknown — never claimed CLOSED, which would score
	// perfect health on no evidence. A contradictory OPEN +
	// CLOSED input reads OPEN (fail-closed).
	if s.BreakerOpen {
		in.Breaker = BreakerOpen
	} else if s.BreakerClosed {
		in.Breaker = BreakerClosed
	}
	if !s.ResetAt.IsZero() {
		// Real reset time (ledger roll-off): the reset factor
		// is no longer neutral. A past reset yields a negative
		// value, which the scorer treats as neutral (see
		// scoring16.go resetFactor).
		in.ResetInSeconds = time.Until(s.ResetAt).Seconds()
	}
	if s.ConnectionCount > 0 {
		in.ConnectionCount = s.ConnectionCount
	}
	if s.ContextWindow > 0 {
		in.ModelContextTokens = float64(s.ContextWindow)
	}
	in.QualityScore = s.QualityScore
	return in
}

// Hook16 is the 16-factor chain hook: the HookWithWeights
// analogue that ranks a fallback chain by Score16 under the
// given 16-factor weights instead of the 5-factor weighted
// score. It is the hook the default ("balanced") strategy must
// use for all 16 factors to vote on the default path (see the
// section header: wiring is a strategy-lane follow-up).
//
// Contract, mirroring HookWithWeights:
//
//   - If signals is nil, the returned hook is the identity: the
//     chain passes through untouched and quota order stands.
//   - Otherwise each attempt is looked up once (signals funcs
//     must be cheap and non-blocking — the hook runs on the
//     request path), pool maxima are computed from the chain's
//     own signals (costInv/latencyInv are pool-relative, exactly
//     like the reference router's computePoolMaxima), each attempt scores
//     by Score16, and the ranked order is returned. The hook
//     never drops or duplicates an attempt: the output is a
//     permutation of the input; ties keep declared order
//     (stable sort).
//   - Attempts whose provider/model has no signals score from
//     neutral inputs (unknown quality 0.45, unknown quota /
//     latency / cost neutral 0.5) and rank on what is known.
//   - No per-request context flows into this hook, so the two
//     context factors stay neutral: contextAffinity (no prompt
//     token count) and cacheAffinity (no warmth func), and
//     sessionAffinity (no session pair — the caller that owns
//     the session layers it, as the plain-auto ranker does).
//     Stability is likewise neutral: Signals carries no latency
//     std-dev. Those five neutrals are documented in
//     Inputs16FromSignals, never silently perfect.
func Hook16(signals SignalsFunc, w Weights16) func(chain []router.Attempt) []router.Attempt {
	if signals == nil {
		return func(chain []router.Attempt) []router.Attempt { return chain }
	}
	return func(chain []router.Attempt) []router.Attempt {
		if len(chain) == 0 {
			return chain
		}
		// One lookup per attempt; pool maxima from the chain's
		// own signals (pool-relative factors need them).
		sigs := make([]Signals, len(chain))
		var maxLatency, maxCost float64 = -1, -1
		for i, att := range chain {
			sigs[i] = signals(att.ProviderID, att.Model)
			if sigs[i].LatencyMs > 0 && sigs[i].LatencyMs > maxLatency {
				maxLatency = sigs[i].LatencyMs
			}
			if sigs[i].CostUSDPer1M >= 0 && sigs[i].CostUSDPer1M > maxCost {
				maxCost = sigs[i].CostUSDPer1M
			}
		}
		type scored struct {
			att   router.Attempt
			score float64
		}
		items := make([]scored, 0, len(chain))
		for i, att := range chain {
			in := Inputs16FromSignals(sigs[i])
			in.MaxLatencyMs = maxLatency
			in.MaxCostPer1M = maxCost
			// MaxStdDevMs stays -1: no latency-stddev source,
			// so stability stays neutral (see
			// Inputs16FromSignals).
			items = append(items, scored{
				att:   att,
				score: Score16(Candidate{ProviderID: att.ProviderID, Model: att.Model, Effort: att.Effort}, in, w),
			})
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
		out := make([]router.Attempt, 0, len(items))
		for _, it := range items {
			out = append(out, it.att)
		}
		return out
	}
}

// Hook16Default is Hook16 under the reference router's DEFAULT_WEIGHTS
// (DefaultWeights16): the default 16-factor chain hook. This is
// the scorer the "balanced" default strategy must use —
// B2-10 (DEFAULT_WEIGHTS), B2-11 (default strategy rules) and
// B2-12 (default strategy score) all resolve to this hook. The
// one-case wiring (StrategyBalanced -> Hook16Default in
// strategy.wiringHook) is a strategy-lane follow-up; see the
// section header.
func Hook16Default(signals SignalsFunc) func(chain []router.Attempt) []router.Attempt {
	return Hook16(signals, DefaultWeights16)
}
