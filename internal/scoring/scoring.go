// Package scoring ranks provider/model candidates for the Codvian Router
// fallback chain by cost, latency, quality and reliability signals.
//
// Honest limitations — read before trusting a ranking:
//
//   - Quality scores are only as good as the signals fed in. There is
//     NO quality signal in Phase 1: every candidate's QualityScore is 0
//     by default, which this package treats as "unknown" (neutral 0.5
//     minus a small penalty), never as measured quality. Until a real
//     eval source exists (internal/eval, a later phase), ModeQuality
//     rankings are driven by the other signals, not by measured quality.
//     This package does not claim, measure or imply any quality ranking
//     of real providers.
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
	"sort"
)

// Candidate is one provider/model pair that could serve a request.
type Candidate struct {
	// ProviderID identifies the provider (router.Attempt.ProviderID).
	ProviderID string
	// Model is the model ID on that provider.
	Model string
}

// Signals are the measurements a candidate is scored on. All fields are
// caller-supplied; zero values mean "unknown", not "measured zero".
// In particular QualityScore 0 means no quality signal exists yet —
// see the package docs — and LatencyMs <= 0 means no latency sample.
type Signals struct {
	// LatencyMs is the recent p50 latency in milliseconds. <= 0 means
	// no latency sample is available.
	LatencyMs float64
	// SuccessRate is the recent fraction of successful attempts, 0..1.
	// 0 means unknown-or-always-failing; callers should only pass
	// measured rates.
	SuccessRate float64
	// CostUSDPer1M is the blended price in USD per 1M tokens.
	CostUSDPer1M float64
	// QualityScore is a 0..1 quality rating, or 0 when unknown.
	// By convention 0 always means unknown in Phase 2: a genuinely
	// measured score of exactly 0 cannot be distinguished from
	// "no signal", and is treated the same way (neutral, penalised).
	QualityScore float64
	// QuotaHint is the remaining-quota hint (same scale as
	// router.Config.QuotaHint: higher = more headroom, 0 = none).
	QuotaHint int
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
	// known-good candidate on quality alone — but see the package docs:
	// with no quality signal anywhere yet, this mode currently ranks
	// mostly on the other signals.
	ModeQuality Mode = "quality"
	// ModeReliable is success-rate and quota heavy: prefer providers
	// that succeed and still have headroom.
	ModeReliable Mode = "reliable"
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

// costComponent normalises cost to [0, 1], higher = cheaper.
// A negative cost is invalid and clamped to 0 (free).
func costComponent(costUSDPer1M float64) float64 {
	if costUSDPer1M < 0 || math.IsNaN(costUSDPer1M) {
		return 1.0
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

// Rank orders candidates best-first by score under the given mode and
// signals, stably: equal scores keep their input order. It never drops
// or duplicates a candidate — the result is a permutation of the input
// (same length, same elements). Candidates with no entry in signals
// are scored by quota hint only (see Scorer.Score); a candidate with
// no signals entry and no quota hint scores 0 and ranks last.
func Rank(candidates []Candidate, signals map[string]Signals, mode Mode) []Candidate {
	scorer := NewScorer(mode)
	type scored struct {
		c     Candidate
		score float64
	}
	items := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		s, _ := signals[c.ProviderID]
		items = append(items, scored{c: c, score: scorer.Score(c, s)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	out := make([]Candidate, 0, len(items))
	for _, it := range items {
		out = append(out, it.c)
	}
	return out
}
