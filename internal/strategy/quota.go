// Package strategy — quota-driven ordering strategies (M3).
//
// This file holds the five quota-driven strategies as pure functions
// over a QuotaSnapshot. They take no I/O, no clock of their own (the
// caller passes `now`), and no shared state, so they are fully
// deterministic and unit-testable. The coordinator wires them into
// the registry and supplies real quota data later; nothing here
// fetches quota, touches the network, or reads the database.
//
// Semantics studied from the the sector baseline reference clone
// (the local reference clone, read-only; semantics only, no
// code copied). Each function cites its source below.
//
//   - quota-weighted: weighted draw, weight = max(0, score)/(1+load)
//     (quotaStrategies.ts, getQuotaWeightedDrawWeight /
//     pickWeightedIndex / orderTargetsByQuotaWeighted).
//   - headroom: headroom = 1 − max(util5h, util7d), most free capacity
//     first (headroomRanking.ts:54, computeHeadroom / rankByHeadroom).
//   - expiry-first: score = usable / hours-until-expiry, most urgent
//     first (quotaScoring.ts, scoreExpiryFirstQuota; ordered by
//     quotaStrategies.ts, orderTargetsByExpiryFirst).
//   - reset-aware: per-window score = remainingWeight*remaining +
//     resetPressureWeight*(urgency*(1−remaining)), session 0.35 /
//     weekly 0.65, exhaustion guard at 10% session remaining
//     (quotaScoring.ts, scoreResetAwareQuota; ordered by
//     quotaStrategies.ts, orderTargetsByResetAwareQuota).
//   - reset-window: ascending milliseconds until the configured
//     window resets, soonest first
//     (quotaScoring.ts, getResetWindowRemainingMs; ordered by
//     quotaStrategies.ts, orderTargetsByResetWindow).
//
// Missing-data rules (honest, deterministic, never invented):
//
//   - Util5h / Util7d zero, NaN, or out of [0,1]: clamped to [0,1],
//     NaN treated as 0. That is the sector-typical own fail-open for
//     headroom (headroomRanking.ts: "any missing / non-finite
//     utilization is treated as 0 (full headroom)"). It is NOT a
//     claim of infinite quota: headroom only ranks free capacity,
//     it never overrides exhaustion — a snapshot whose
//     QuotaRemaining is 0 scores 0 in expiry-first and is floored
//     in reset-aware, and the coordinator's exhaustion filter (not
//     this file) is what keeps spent targets out of a chain.
//   - Util5h / Util7d of -1 means "unknown" (no per-pair limit
//     exists to divide the spend by): headroom still treats it
//     as 0 (full headroom — the sector-typical own fail-open), but
//     reset-aware falls back to the snapshot's QuotaRemaining
//     fraction when it has one (G19 Area 1, remainingFraction),
//     so a measured hint fraction ranks continuously instead
//     of reading as full. A snapshot whose QuotaRemaining is 0
//     with unknown utilisation reads as exhausted there.
//   - Zero ResetAt / zero ExpiresAt: no reset/expiry is known. No
//     reset time is ever invented. Reset-aware treats an unknown
//     reset as neutral urgency 0.5 (quotaScoring.ts,
//     getResetUrgency: no resetAt → 0.5); reset-window treats it
//     as +Inf remaining, sorting last in input order
//     (quotaScoring.ts, getResetWindowRemainingMs); expiry-first
//     falls back to the usable fraction alone when no deadline is
//     known (quotaScoring.ts, scoreExpiryFirstQuota: "when no
//     window reports a reset, it falls back to weighted leftover").
//   - A fully-zero snapshot (Score, Util5h, Util7d, QuotaRemaining
//     all 0 and both times unset) carries no quota telemetry at
//     all: reset-aware scores it at the no-quota floor 0.5
//     (quotaScoring.ts, RESET_AWARE_DEFAULTS.noQuotaFloor), never
//     as empty and never as full. Expiry-first scores it 0 —
//     nothing is known to be expiring — and it keeps input order.
//   - QuotaRemaining is a 0..1 remaining fraction (1 = untouched,
//     0 = spent). Values outside [0,1] are clamped; NaN is 0.
//   - Load is the in-flight request count for the target; negative
//     loads are clamped to 0. A negative Score never wins a draw:
//     its weight is clamped to 0.
//
// Tie-breaking: every ordering here is a stable sort — equal keys
// keep input (declared/priority) order. the sector baseline additionally
// rotates leading ties (rotateLeadingTies) using shared RNG state;
// this file deliberately does not: rotation is a coordinator
// concern, and a pure function must return the same order for the
// same input. If the coordinator wants rotation it can rotate the
// tied head itself.
//
// All orderings return a NEW slice; the input is never mutated.
package strategy

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

// QuotaSnapshot is the per-target quota view one ordering pass ranks.
//
//   - Provider / Model identify the target (for the coordinator's
//     logging; the orderings key on the numeric fields only).
//   - Score is the target's base score (the reset-aware quota score
//     when one is available, else any 0..1-ish quality score the
//     caller supplies). Only quota-weighted reads it.
//   - Util5h / Util7d are 0..1 utilisation of the 5-hour (session)
//     and 7-day (weekly) quota windows. 0 means "unused or unknown".
//   - QuotaRemaining is the 0..1 remaining quota fraction across
//     the tightest window (1 = untouched, 0 = spent).
//   - ResetAt is when the quota window next resets; ExpiresAt is
//     when the quota itself expires (is thrown away). Either may
//     be the zero time when unknown.
//   - Load is the target's current in-flight request count.
type QuotaSnapshot struct {
	Provider       string
	Model          string
	Score          float64
	Util5h         float64
	Util7d         float64
	QuotaRemaining float64
	ResetAt        time.Time
	ExpiresAt      time.Time
	Load           float64
}

// Quota ordering constants, mirroring the the sector baseline defaults cited
// per function. They are exported so the coordinator and tests can
// name them instead of re-deriving magic numbers.
const (
	// QuotaNoDataFloor is the reset-aware score for a snapshot with
	// no quota telemetry at all (quotaScoring.ts
	// RESET_AWARE_DEFAULTS.noQuotaFloor = 0.5).
	QuotaNoDataFloor = 0.5
	// QuotaExhaustionGuard is the session-remaining fraction below
	// which reset-aware scales the score down (quotaScoring.ts
	// RESET_AWARE_DEFAULTS.exhaustionGuardPercent = 10).
	QuotaExhaustionGuard = 0.10
	// QuotaExhaustedFloor is the remaining fraction at or below
	// which expiry-first scores a target 0 (quotaScoring.ts
	// EXPIRY_FIRST_DEFAULTS.exhaustedFloorPercent = 1).
	QuotaExhaustedFloor = 0.01
	// QuotaExpiryMinHours is the smallest deadline expiry-first
	// divides by, so a reset seconds away does not produce an
	// unbounded score (quotaScoring.ts
	// EXPIRY_FIRST_DEFAULTS.minHours = 0.25).
	QuotaExpiryMinHours = 0.25
	// QuotaResetAwareSessionWeight / QuotaResetAwareWeeklyWeight
	// are the session/weekly blend weights (quotaScoring.ts
	// RESET_AWARE_DEFAULTS sessionWeight 0.35 / weeklyWeight 0.65).
	QuotaResetAwareSessionWeight = 0.35
	QuotaResetAwareWeeklyWeight  = 0.65
)

// Per-window score weights from quotaScoring.ts
// (RESET_AWARE_*_WINDOW_MS and
// RESET_AWARE_*_REMAINING/RESET_PRESSURE_WEIGHT).
const (
	quotaSessionWindowMs            = 5 * 60 * 60 * 1000
	quotaWeeklyWindowMs             = 7 * 24 * 60 * 60 * 1000
	quotaSessionRemainingWeight     = 0.45
	quotaSessionResetPressureWeight = 0.55
	quotaWeeklyRemainingWeight      = 0.25
	quotaWeeklyResetPressureWeight  = 0.75
)

// clamp01 clamps v to [0,1]; NaN becomes 0 (unknown → neutral-low,
// never a crash, never an invented value).
func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampUtil clamps a utilisation signal to [0,1]; non-finite
// becomes 0 — the sector baseline headroomRanking.ts clampUtil: "non-finite
// → 0 (generous default)".
func clampUtil(v float64) float64 {
	if !math.IsInf(v, 0) && math.IsNaN(v) {
		return 0
	}
	return clamp01(v)
}

// Headroom returns 1 − max(util5h, util7d), clamped to [0,1]
// (headroomRanking.ts:54 computeHeadroom). Higher means more free
// capacity. Missing utilisation is 0, i.e. full headroom — the
// documented fail-open, not a claim of infinite quota.
func Headroom(s QuotaSnapshot) float64 {
	return 1 - math.Max(clampUtil(s.Util5h), clampUtil(s.Util7d))
}

// OrderHeadroom returns the snapshots ordered by descending
// headroom (most free capacity first). Ties keep input order
// (stable sort). The input slice is not mutated; nil/empty input
// returns an empty (non-nil) slice.
func OrderHeadroom(in []QuotaSnapshot) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return Headroom(out[i]) > Headroom(out[j])
	})
	return out
}

// QuotaWeightedWeight returns the weighted-draw weight for one
// snapshot: max(0, score)/(1+load) (quotaStrategies.ts,
// getQuotaWeightedDrawWeight, leftover branch: "Only pace entries
// carry capacity; leftover retains its exact historical divisor").
// A non-finite or negative score weighs 0 and can never win a draw;
// a negative load counts as 0.
func QuotaWeightedWeight(s QuotaSnapshot) float64 {
	score := s.Score
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
		score = 0
	}
	load := s.Load
	if math.IsNaN(load) || math.IsInf(load, 0) || load < 0 {
		load = 0
	}
	return score / (1 + load)
}

// PickWeightedIndex is the pure weighted draw over weights
// (quotaStrategies.ts, pickWeightedIndex): r in [0, sum(weights)).
// The first cumulative weight strictly greater than r wins
// (half-open); zero or negative weights are skipped, so r == 0
// cannot land on a zero-weight leading slot. It returns -1 when
// every weight is non-positive — the caller then falls back to a
// uniform pick, exactly as the sector baseline does (secureRandomInt).
func PickWeightedIndex(weights []float64, r float64) int {
	type slot struct {
		i int
		w float64
	}
	positive := make([]slot, 0, len(weights))
	sum := 0.0
	for i, w := range weights {
		if w > 0 && !math.IsNaN(w) && !math.IsInf(w, 0) {
			positive = append(positive, slot{i, w})
			sum += w
		}
	}
	if len(positive) == 0 || sum == 0 {
		return -1
	}
	acc := 0.0
	for _, p := range positive {
		acc += p.w
		if acc > r {
			return p.i
		}
	}
	// r >= sum through floating-point rounding: the last positive
	// slot wins rather than reporting no winner.
	return positive[len(positive)-1].i
}

// quotaWeightedByScore orders by descending Score, stable — the
// order the sector baseline gives the non-winning targets in a
// quota-weighted pass (orderTargetsByQuotaWeighted: unused targets
// sorted by score, then pool B, then credits-only).
func quotaWeightedByScore(in []QuotaSnapshot) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// OrderQuotaWeighted performs one weighted draw over in and returns
// the winner first, followed by the remaining targets ordered by
// descending Score (quotaStrategies.ts,
// orderTargetsByQuotaWeighted: winner first, unused sorted by
// score). The draw uses rnd; a nil rnd uses a fixed seed (1), so
// the result stays deterministic for tests — production callers
// must pass a real *rand.Rand. Targets with zero/negative weight
// can never win while any positive weight exists; when every
// weight is non-positive the winner is a uniform pick from rnd.
// The input slice is not mutated.
func OrderQuotaWeighted(in []QuotaSnapshot, rnd *rand.Rand) []QuotaSnapshot {
	if len(in) == 0 {
		return []QuotaSnapshot{}
	}
	if rnd == nil {
		rnd = rand.New(rand.NewSource(1))
	}
	weights := make([]float64, len(in))
	sum := 0.0
	for i, s := range in {
		weights[i] = QuotaWeightedWeight(s)
		sum += weights[i]
	}
	pick := -1
	if sum > 0 {
		pick = PickWeightedIndex(weights, rnd.Float64()*sum)
	}
	if pick < 0 {
		pick = rnd.Intn(len(in))
	}
	winner := in[pick]
	rest := make([]QuotaSnapshot, 0, len(in)-1)
	for i, s := range in {
		if i != pick {
			rest = append(rest, s)
		}
	}
	rest = quotaWeightedByScore(rest)
	return append([]QuotaSnapshot{winner}, rest...)
}

// resetUrgency is how pressing a reset at resetAt is, measured
// against now over a window of windowMs (quotaScoring.ts,
// getResetUrgency): unknown or unparseable reset → 0.5 (neutral);
// already-reset → 1; otherwise 1 − msUntilReset/windowMs clamped
// to [0,1], so a reset far beyond the window scores ~0 and one
// imminent scores ~1.
func resetUrgency(resetAt time.Time, windowMs int64, now time.Time) float64 {
	if resetAt.IsZero() {
		return 0.5
	}
	msUntil := resetAt.Sub(now).Milliseconds()
	if msUntil <= 0 {
		return 1
	}
	return clamp01(1 - float64(msUntil)/float64(windowMs))
}

// scoreQuotaWindow scores one quota window (quotaScoring.ts,
// scoreQuotaWindow): remainingWeight*remaining +
// resetPressureWeight*(urgency*(1−remaining)). A nearly-empty
// window about to reset scores high on pressure (it will be
// useful again soon); a full window scores high on remaining.
func scoreQuotaWindow(remaining float64, resetAt time.Time, windowMs int64, remainingWeight, resetPressureWeight float64, now time.Time) float64 {
	remaining = clamp01(remaining)
	urgency := resetUrgency(resetAt, windowMs, now)
	pressure := urgency * (1 - remaining)
	return remainingWeight*remaining + resetPressureWeight*pressure
}

// remainingFraction is the remaining quota fraction for one
// window (G19 Area 1, PARITY row "reset-aware"): the measured
// utilisation when known (util >= 0), else the snapshot's
// remaining fraction — the registry/probe/engine-learned quota
// hint as a fraction, continuous instead of the booleanised hint. Unknown
// utilisation with no fraction reads as full remaining (the
// documented fail-open); a known fraction of 0 reads as
// exhausted, so the exhaustion guard below scales the score
// down instead of ranking a spent window as healthy.
func remainingFraction(util, quotaRemaining float64) float64 {
	if util >= 0 {
		return 1 - clampUtil(util)
	}
	return clamp01(quotaRemaining)
}

// ResetAwareScore scores one snapshot for the reset-aware strategy
// (quotaScoring.ts, scoreResetAwareQuota): session score from the
// 5h utilisation and weekly score from the 7d utilisation,
// blended 0.35/0.65, with the exhaustion guard. When a window's
// utilisation is unknown (Util < 0 — no per-pair limit exists to
// divide the spend by), the remaining fraction falls back to the
// snapshot's QuotaRemaining (see remainingFraction): a measured
// hint fraction is continuous, so reset-aware can finally rank
// a half-spent provider below a full one instead of scoring
// every unknown window as full. The single ResetAt applies to
// both windows — the snapshot carries one reset instant, and no
// per-window reset is invented. A fully-zero snapshot (no
// telemetry at all) scores QuotaNoDataFloor (0.5).
func ResetAwareScore(s QuotaSnapshot, now time.Time) float64 {
	if s.Score == 0 && s.Util5h == 0 && s.Util7d == 0 && s.QuotaRemaining == 0 && s.ResetAt.IsZero() && s.ExpiresAt.IsZero() {
		return QuotaNoDataFloor
	}
	sessionRemaining := remainingFraction(s.Util5h, s.QuotaRemaining)
	weeklyRemaining := remainingFraction(s.Util7d, s.QuotaRemaining)
	sessionScore := scoreQuotaWindow(sessionRemaining, s.ResetAt, quotaSessionWindowMs, quotaSessionRemainingWeight, quotaSessionResetPressureWeight, now)
	weeklyScore := scoreQuotaWindow(weeklyRemaining, s.ResetAt, quotaWeeklyWindowMs, quotaWeeklyRemainingWeight, quotaWeeklyResetPressureWeight, now)
	score := QuotaResetAwareSessionWeight*sessionScore + QuotaResetAwareWeeklyWeight*weeklyScore
	// Exhaustion guard (quotaScoring.ts, scoreResetAwareQuota):
	// below 10% session remaining the score is scaled down by
	// max(0.05, sessionRemaining/guard), so a nearly-spent window
	// is never ranked as if it were healthy.
	if QuotaExhaustionGuard > 0 && sessionRemaining < QuotaExhaustionGuard {
		score *= math.Max(0.05, sessionRemaining/QuotaExhaustionGuard)
	}
	return score
}

// OrderResetAware returns the snapshots ordered by descending
// reset-aware score (quotaStrategies.ts,
// orderTargetsByResetAwareQuota: sort by score desc, ties by
// input order). The input slice is not mutated.
func OrderResetAware(in []QuotaSnapshot, now time.Time) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), in...)
	type scored struct {
		s     QuotaSnapshot
		score float64
	}
	sc := make([]scored, len(out))
	for i, s := range out {
		sc[i] = scored{s, ResetAwareScore(s, now)}
	}
	sort.SliceStable(sc, func(i, j int) bool {
		return sc[i].score > sc[j].score
	})
	for i, e := range sc {
		out[i] = e.s
	}
	return out
}

// resetWindowRemainingMs is the milliseconds until s resets, for
// the reset-window strategy (quotaScoring.ts,
// getResetWindowRemainingMs): already-elapsed resets collapse to
// 0, and an unknown reset is +Inf (sorts last). No reset time is
// ever invented.
func resetWindowRemainingMs(s QuotaSnapshot, now time.Time) float64 {
	if s.ResetAt.IsZero() {
		return math.Inf(1)
	}
	ms := s.ResetAt.Sub(now).Milliseconds()
	if ms <= 0 {
		return 0
	}
	return float64(ms)
}

// OrderResetWindow returns the snapshots ordered by ascending time
// until reset — the account whose quota resets soonest goes first
// (quotaStrategies.ts, orderTargetsByResetWindow: "Ascending: the
// account whose quota resets SOONEST goes first. Targets with no
// known reset (Infinity) fall to the back, ordered by combo
// priority"). Ties (including shared +Inf) keep input order. The
// input slice is not mutated.
func OrderResetWindow(in []QuotaSnapshot, now time.Time) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), in...)
	type scored struct {
		s   QuotaSnapshot
		rem float64
	}
	sc := make([]scored, len(out))
	for i, s := range out {
		sc[i] = scored{s, resetWindowRemainingMs(s, now)}
	}
	sort.SliceStable(sc, func(i, j int) bool {
		return sc[i].rem < sc[j].rem
	})
	for i, e := range sc {
		out[i] = e.s
	}
	return out
}

// ExpiryFirstScore scores one snapshot for the expiry-first
// strategy (quotaScoring.ts, scoreExpiryFirstQuota): how much quota
// must be spent PER HOUR to avoid losing it at its deadline.
// usable is the tightest-window remaining fraction; the deadline
// is ExpiresAt when known, else ResetAt (the sector-typical deadline IS
// the window reset), else unknown. Score = usable / max(minHours,
// hoursUntilDeadline); with no known deadline it falls back to
// usable alone ("weighted leftover"). A spent snapshot (usable at
// or below the 1% floor) scores 0 and is never preferred; a
// no-telemetry snapshot scores 0 — nothing is known to be
// expiring — and keeps input order.
func ExpiryFirstScore(s QuotaSnapshot, now time.Time) float64 {
	usable := clamp01(s.QuotaRemaining)
	if usable <= QuotaExhaustedFloor {
		return 0
	}
	deadline := s.ExpiresAt
	if deadline.IsZero() {
		deadline = s.ResetAt
	}
	if deadline.IsZero() {
		return usable
	}
	hours := math.Max(QuotaExpiryMinHours, deadline.Sub(now).Hours())
	return usable / hours
}

// OrderExpiryFirst returns the snapshots ordered by descending
// expiry-first score — the account whose quota is closest to being
// thrown away goes first (quotaStrategies.ts,
// orderTargetsByExpiryFirst). When every score is 0 (everything
// spent or no telemetry), the input order is returned unchanged,
// matching the sector baseline ("Everything scored zero — keep the expanded
// (priority) order, no rotation"). The input slice is not mutated.
func OrderExpiryFirst(in []QuotaSnapshot, now time.Time) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), in...)
	type scored struct {
		s     QuotaSnapshot
		score float64
	}
	sc := make([]scored, len(out))
	best := 0.0
	for i, s := range out {
		sc[i] = scored{s, ExpiryFirstScore(s, now)}
		if sc[i].score > best {
			best = sc[i].score
		}
	}
	if best <= 0 {
		return out
	}
	sort.SliceStable(sc, func(i, j int) bool {
		return sc[i].score > sc[j].score
	})
	for i, e := range sc {
		out[i] = e.s
	}
	return out
}
