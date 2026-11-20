// Package sla scores routing candidates against latency /
// error-rate / cost service-level objectives — the local,
// measurement-only form of the reference router's sla-aware strategy
// (open-sse/services/autoCombo/routerStrategy.ts SLAStrategy,
// studied, not copied).
//
// Every input is a measured value or an explicit unknown; nothing
// here invents a signal. The two callers feed it from different
// sources — internal/strategy from live per-(provider, model)
// signals, internal/autorouter from the enriched catalog — and
// both get the same ranking for the same inputs.
//
// Deliberate divergences from the reference router's SLAStrategy, each
// because the reference treats an unknown as a perfect score:
//   - Unknown latency / error rate / cost scores NEUTRAL (0.5),
//     not perfect (1.0): a candidate with no measurement has not
//     proven it meets the SLO, and ranking it first would be
//     routing on no evidence. (the reference router's
//     scoreAtOrBelowThreshold gives a 0 p95 latency a perfect
//     score.)
//   - Stability is neutral (0.5) everywhere: no latency-stddev
//     signal exists in either caller, so the 5% stability weight
//     cannot move a ranking.
//   - Health is the observed serving health (1 when the candidate
//     has served successfully, 0 when every observed attempt
//     failed, neutral when unobserved); the reference reads a
//     circuit-breaker state the hook inputs do not carry.
package sla

import "math"

// Policy is one SLA: the latency / error-rate / cost objectives
// a candidate is scored against. Zero values mean "default" —
// see DefaultPolicy.
type Policy struct {
	// TargetP95Ms is the p95 latency objective in milliseconds.
	// Default 2000 (the reference router DEFAULT_SLA_TARGET_P95_MS).
	TargetP95Ms float64
	// MaxErrorRate is the tolerated error fraction in [0,1].
	// Default 0.05 (the reference router DEFAULT_SLA_MAX_ERROR_RATE).
	MaxErrorRate float64
	// MaxCostPer1MTokens caps the blended cost in USD per 1M
	// tokens. 0 means "no cap": cost then scores relative to
	// the pool maximum instead of against a threshold.
	MaxCostPer1MTokens float64
	// HardConstraints sorts by violation count first (fewest
	// SLO breaches wins, score breaks ties) instead of by
	// score alone.
	HardConstraints bool
}

// DefaultPolicy is the SLA policy when the operator configures
// none: the reference router's defaults (2000 ms p95, 5% errors, no cost
// cap, soft constraints).
func DefaultPolicy() Policy {
	return Policy{TargetP95Ms: 2000, MaxErrorRate: 0.05}
}

// Resolve fills zero policy fields with the defaults: a target
// <= 0 means "default target", a negative error rate means
// "default rate", a negative cost cap means "no cap". Note the
// asymmetry is deliberate: MaxErrorRate 0 is a real
// zero-tolerance policy, not "unset" — a caller that wants the
// default rate passes DefaultPolicy or a negative value.
func (p Policy) Resolve() Policy {
	if p.TargetP95Ms <= 0 {
		p.TargetP95Ms = 2000
	}
	if p.MaxErrorRate < 0 {
		p.MaxErrorRate = 0.05
	}
	if p.MaxCostPer1MTokens < 0 {
		p.MaxCostPer1MTokens = 0
	}
	return p
}

// Candidate is one routing candidate's measured SLO signals.
// Every Has* flag distinguishes "measured" from "unknown": an
// unknown signal scores neutral and contributes no violation —
// never a guessed value.
type Candidate struct {
	// LatencyMs is the p95 latency in milliseconds, valid when
	// HasLatency.
	LatencyMs  float64
	HasLatency bool
	// ErrorRate is the measured error fraction in [0,1], valid
	// when HasErrorRate.
	ErrorRate    float64
	HasErrorRate bool
	// CostPer1MTokens is the blended cost in USD per 1M tokens,
	// valid when HasCost.
	CostPer1MTokens float64
	HasCost         bool
	// Health is the observed serving health in [0,1] (1 =
	// served successfully, 0 = every observed attempt
	// failed), valid when HasHealth.
	Health    float64
	HasHealth bool
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// scoreAtOrBelowThreshold is the reference router's threshold score: 1 when
// the value is at or below the threshold, falling as it exceeds
// it. A zero threshold is an intentional zero-tolerance policy
// (only an exact 0 scores 1).
func scoreAtOrBelowThreshold(value, threshold float64) float64 {
	if threshold <= 0 {
		if value == 0 {
			return 1
		}
		return 0
	}
	if value < 0.000001 {
		value = 0.000001
	}
	return clamp01(threshold / value)
}

func inverseNormalized(value, maxValue float64) float64 {
	if !math.IsInf(value, 0) && !math.IsNaN(value) && value >= 0 {
		if !math.IsInf(maxValue, 0) && !math.IsNaN(maxValue) && maxValue > 0 {
			return clamp01(1 - value/maxValue)
		}
		return 1
	}
	return 0
}

// Score ranks one candidate under policy. maxCost is the pool's
// maximum known cost (for the relative cost score when no cap is
// set); <= 0 means "no known cost anywhere". It returns the
// violation score (0 = meets every SLO; higher = further from
// the objectives) and the weighted score (higher = better, the
// 0.35 latency / 0.35 error / 0.15 health / 0.10 cost / 0.05
// stability mix of the reference). Unknown signals score neutral
// (0.5) and add no violation.
func Score(c Candidate, policy Policy, maxCost float64) (violation, score float64) {
	p := policy.Resolve()
	var latencyScore, errorScore, healthScore, costScore float64
	const stabilityScore = 0.5 // no stddev signal anywhere (see package doc)

	if c.HasLatency {
		latencyScore = scoreAtOrBelowThreshold(c.LatencyMs, p.TargetP95Ms)
		if c.LatencyMs > p.TargetP95Ms {
			violation += (c.LatencyMs - p.TargetP95Ms) / p.TargetP95Ms
		}
	} else {
		latencyScore = 0.5
	}
	if c.HasErrorRate {
		errorScore = scoreAtOrBelowThreshold(c.ErrorRate, p.MaxErrorRate)
		if c.ErrorRate > p.MaxErrorRate {
			if p.MaxErrorRate > 0 {
				violation += (c.ErrorRate - p.MaxErrorRate) / p.MaxErrorRate
			} else {
				violation += c.ErrorRate
			}
		}
	} else {
		errorScore = 0.5
	}
	if c.HasHealth {
		healthScore = clamp01(c.Health)
		if c.Health <= 0 {
			violation += 1
		}
	} else {
		healthScore = 0.5
	}
	if p.MaxCostPer1MTokens > 0 {
		if c.HasCost {
			costScore = scoreAtOrBelowThreshold(c.CostPer1MTokens, p.MaxCostPer1MTokens)
			if c.CostPer1MTokens > p.MaxCostPer1MTokens {
				violation += (c.CostPer1MTokens - p.MaxCostPer1MTokens) / p.MaxCostPer1MTokens
			}
		} else {
			costScore = 0.5
		}
	} else if c.HasCost {
		costScore = inverseNormalized(c.CostPer1MTokens, maxCost)
	} else {
		costScore = 0.5
	}
	score = latencyScore*0.35 + errorScore*0.35 + healthScore*0.15 + costScore*0.10 + stabilityScore*0.05
	return violation, score
}

// Better reports whether a beats b under policy: with hard
// constraints the fewer violations win (score breaks ties);
// otherwise the higher score wins.
func Better(aViolation, aScore, bViolation, bScore float64, hard bool) bool {
	if hard {
		if aViolation != bViolation {
			return aViolation < bViolation
		}
	}
	return aScore > bScore
}
