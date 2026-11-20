// Ordering strategies for M3 (strategies & combos): pure functions over
// a target list, plus the two stateful orderers (round-robin rotation
// and the strict-random deck). Nothing here is wired into the registry
// yet — the coordinator wires these into the strategy registry later.
// This file intentionally touches no other file.
//
// OmniRoute semantics (read-only reference,
// ~/workspace/omniroute-reference; studied, not copied):
//   - random:        fisherYatesShuffle — applyStrategyOrdering.ts "random"
//   - strict-random: shuffleDeck.ts getNextFromDeck — deck + mutex, each
//     item exactly once per cycle, last-of-cycle !=
//     first-of-next; applyStrategyOrdering.ts also shuffles
//     the fallback remainder (#3959)
//   - fill-first:    applyStrategyOrdering.ts "fill-first" preserves
//     priority order; the drain is enforced by retrying the
//     first target until it fails. OrderFillFirst expresses
//     that drain as a pure ordering: declared order among
//     targets that still have quota, quota-exhausted targets
//     demoted behind them.
//   - least-used:    targetSorters.ts:107 sortTargetsByUsage — usage ascending
//   - p2c:           targetSorters.ts:136 orderTargetsByPowerOfTwoChoices —
//     draw 2 distinct indices, better score first, rest in
//     input order; getP2CTargetScore: successRate/100 +
//     1/log10(avgLatencyMs+10) − breaker penalty, OPEN
//     breaker scores -Inf
//   - weighted:      targetSorters.ts:28 selectWeightedTarget — total-weight
//     draw (r = rand*total, walk subtracting each weight,
//     first r <= 0 wins; total <= 0 -> uniform pick) and
//     orderTargetsForWeightedFallback (selected first, rest
//     weight-desc unless preserveExistingOrder)
//
// Weighted FIX (PARITY.md Area 1 row "weighted", WRONG-NAME,
// internal/strategy/strategy.go:180): the old "weighted" strategy is
// not a per-target weight at all — WeightsFor re-weights the 5 scoring
// factors (latency/success/cost/quality/quota) with operator weights.
// OmniRoute's "weighted" is a per-target weight draw over the targets
// themselves. WeightedDraw below implements OmniRoute's draw; it does
// not replace WeightsFor (that stays for the scoring-factor weights),
// it supplies the missing per-target-weight behaviour.
package strategy

import (
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// OrderingTarget is the minimal input to the ordering strategies: one chain
// entry (one provider serving one model). Fields are used as follows:
//
//   - Provider, Model: identity; Key() is "provider/model".
//   - Score:  P2C composite, higher is better (the caller folds
//     success rate and latency into it, e.g. successRate/100 +
//     1/log10(avgLatencyMs+10) per OmniRoute's getP2CTargetScore).
//     Pass math.Inf(-1) for a target whose breaker is OPEN —
//     OmniRoute scores it -Inf so it can never win a P2C draw.
//     The p2c draw adds the G10 latency term to this score
//     (see OrderP2C), so LatencyMs below can still move the
//     draw even when two Scores tie.
//   - LatencyMs: measured latency in milliseconds for the p2c
//     latency term; <= 0 means no sample (neutral, equivalent
//     to 1000 ms). Only OrderP2C reads it.
//   - Attempts: completed attempts / usage count; least-used orders
//     ascending on it.
//   - QuotaRemaining: remaining quota units; fill-first demotes
//     targets at <= 0 (exhausted) behind targets that still have
//     quota. Pass +Inf (or any value > 0) when quota is unknown.
//   - Weight: per-target weight for WeightedDraw; <= 0 means
//     unweighted (never drawn while any positive weight exists).
type OrderingTarget struct {
	Provider       string
	Model          string
	Score          float64
	LatencyMs      float64
	Attempts       int
	QuotaRemaining float64
	Weight         float64
}

// Key identifies a target: "provider/model".
func (t OrderingTarget) Key() string { return t.Provider + "/" + t.Model }

// rng returns rnd, or a fresh time-seeded source when rnd is nil so
// callers that do not need reproducibility never share a *rand.Rand
// (a *rand.Rand is not safe for concurrent use).
func rng(rnd *rand.Rand) *rand.Rand {
	if rnd != nil {
		return rnd
	}
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

// copyTargets returns a shallow copy; every orderer returns a new
// slice and never mutates its input.
func copyTargets(targets []OrderingTarget) []OrderingTarget {
	out := make([]OrderingTarget, len(targets))
	copy(out, targets)
	return out
}

// ShuffleTargets returns a Fisher-Yates shuffle of targets (new
// slice; input untouched). OmniRoute's fisherYatesShuffle
// (shuffleDeck.ts): for i from n-1 down to 1, swap i with a uniform
// j in [0, i].
func ShuffleTargets(targets []OrderingTarget, rnd *rand.Rand) []OrderingTarget {
	out := copyTargets(targets)
	r := rng(rnd)
	for i := len(out) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// OrderLeastUsed orders by attempt/usage count ascending (fewest
// attempts first). Ties keep declared order (stable sort), matching
// OmniRoute's sortTargetsByUsage (Array.sort is stable; equal usage
// keeps input order).
func OrderLeastUsed(targets []OrderingTarget) []OrderingTarget {
	out := copyTargets(targets)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Attempts < out[j].Attempts })
	return out
}

// OrderFillFirst is the quota-drain ordering: targets that still
// have quota (QuotaRemaining > 0) keep declared order at the front;
// quota-exhausted targets (QuotaRemaining <= 0) are demoted behind
// them, still in declared order. Trying the list in this order
// drains the first live target's quota before moving to the next —
// OmniRoute's fill-first keeps priority order and drains by retrying
// the first target; this adds the exhausted-demotion so a drained
// target stops leading the chain.
func OrderFillFirst(targets []OrderingTarget) []OrderingTarget {
	out := copyTargets(targets)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].QuotaRemaining > 0 && out[j].QuotaRemaining <= 0
	})
	return out
}

// OrderP2C implements power-of-two-choices (targetSorters.ts:136):
// draw two distinct indices uniformly at random, put the
// better-Score one first, and keep every other target in input
// order behind it. The draw is exactly OmniRoute's: first =
// Intn(n); second = Intn(n-1), bumped by one when it would collide
// with first. A tie keeps the first-drawn target (strict >).
//
// G12: the comparison is Score plus the G10 p2c latency term
// (scoring.P2CLatencyTerm under ModeLatencyAware weights, latency
// 0.70): the faster of the two gains +term, the slower loses it,
// so the second-drawn target wins when
// (Score[second] - Score[first]) + signedTerm > 0. Between
// otherwise-equal candidates the lower-latency one wins; a large
// Score gap still beats a noise-level latency gap (the term is
// 0.0029 for 100-vs-105ms, while a $100-vs-$1 cost gap moves the
// balanced Score by 0.16). The sign comes from
// scoring.LatencyComponent — the same normalisation the term uses
// — so sign and magnitude can never disagree. A zero term (equal
// or unknown latencies) leaves the comparison exactly as before,
// so score-only orderings are unchanged.
func OrderP2C(targets []OrderingTarget, rnd *rand.Rand) []OrderingTarget {
	if len(targets) <= 1 {
		return copyTargets(targets)
	}
	r := rng(rnd)
	n := len(targets)
	first := r.Intn(n)
	second := r.Intn(n - 1)
	if second >= first {
		second++
	}
	winner := first
	term := scoring.P2CLatencyTerm(
		scoring.Signals{LatencyMs: targets[first].LatencyMs},
		scoring.Signals{LatencyMs: targets[second].LatencyMs},
		scoring.Modes[scoring.ModeLatencyAware])
	signedTerm := term
	if scoring.LatencyComponent(targets[second].LatencyMs) < scoring.LatencyComponent(targets[first].LatencyMs) {
		signedTerm = -term
	}
	if targets[second].Score-targets[first].Score+signedTerm > 0 {
		winner = second
	}
	out := make([]OrderingTarget, 0, n)
	out = append(out, targets[winner])
	for i, t := range targets {
		if i != winner {
			out = append(out, t)
		}
	}
	return out
}

// WeightedDraw implements OmniRoute's selectWeightedTarget
// (targetSorters.ts:28): a single per-target weight draw. Total
// weight W = sum of positive weights; draw r uniform in [0, W) and
// walk the targets subtracting each weight, returning the first
// target where r <= 0. When the total weight is <= 0 (no positive
// weight anywhere) it falls back to a uniform pick, exactly as
// OmniRoute does. Negative weights are clamped to 0 (weights are
// non-negative by definition). It reports false only for an empty
// list. The last target is the unreachable-in-practice fallback
// (OmniRoute's targets.at(-1)) if floating point ever walks past
// every target.
func WeightedDraw(targets []OrderingTarget, rnd *rand.Rand) (OrderingTarget, bool) {
	if len(targets) == 0 {
		return OrderingTarget{}, false
	}
	r := rng(rnd)
	total := 0.0
	for _, t := range targets {
		if t.Weight > 0 {
			total += t.Weight
		}
	}
	if total <= 0 {
		return targets[r.Intn(len(targets))], true
	}
	draw := r.Float64() * total
	for _, t := range targets {
		if t.Weight > 0 {
			draw -= t.Weight
		}
		if draw <= 0 {
			return t, true
		}
	}
	return targets[len(targets)-1], true
}

// OrderWeightedFallback implements
// orderTargetsForWeightedFallback (targetSorters.ts): the selected
// target (by Key) leads, and the rest follow sorted by weight
// descending — unless preserveExistingOrder is set, in which case
// the rest keep declared order. A selected target not in the list
// yields just the rest.
func OrderWeightedFallback(targets []OrderingTarget, selected OrderingTarget, preserveExistingOrder bool) []OrderingTarget {
	rest := make([]OrderingTarget, 0, len(targets))
	var sel *OrderingTarget
	for _, t := range targets {
		if t.Key() == selected.Key() && sel == nil {
			c := t
			sel = &c
			continue
		}
		rest = append(rest, t)
	}
	if !preserveExistingOrder {
		sort.SliceStable(rest, func(i, j int) bool { return rest[i].Weight > rest[j].Weight })
	}
	if sel == nil {
		return rest
	}
	return append([]OrderingTarget{*sel}, rest...)
}

// RoundRobin is a stateful per-chain rotation: each Order call
// starts at the next position (counter % len(targets)) and advances
// the counter by one, so every target leads exactly once per cycle.
// It is safe for concurrent use (mutex). It stores no target list —
// the counter is taken modulo the current list length on every call
// — so the list may grow, shrink, or reorder between calls without
// a panic or stale index; rotation simply continues over whatever
// list is current. This matches OmniRoute's rrState.ts counter
// (startIndex = counter % targets.length), minus the sticky-target
// affinity, which is a separate concern and is not implemented here.
type RoundRobin struct {
	mu      sync.Mutex
	counter uint64
}

// Order returns the targets rotated so the next target in the cycle
// leads; the rotation advances by one per call.
func (rr *RoundRobin) Order(targets []OrderingTarget) []OrderingTarget {
	if len(targets) == 0 {
		return nil
	}
	rr.mu.Lock()
	start := int(rr.counter % uint64(len(targets)))
	rr.counter++
	rr.mu.Unlock()
	out := make([]OrderingTarget, 0, len(targets))
	out = append(out, targets[start:]...)
	out = append(out, targets[:start]...)
	return out
}

// Next returns the single next target in the cycle and advances it.
func (rr *RoundRobin) Next(targets []OrderingTarget) (OrderingTarget, bool) {
	if len(targets) == 0 {
		return OrderingTarget{}, false
	}
	ordered := rr.Order(targets)
	return ordered[0], true
}

// Reset restarts the cycle from the first target.
func (rr *RoundRobin) Reset() {
	rr.mu.Lock()
	rr.counter = 0
	rr.mu.Unlock()
}

// StrictRandomDeck is the strict-random orderer: a shuffled deck of
// the current target set, dealt one leader per call, with no repeat
// until the deck is exhausted, then reshuffled. Thread-safe (mutex).
// Semantics follow shuffleDeck.ts getNextFromDeck:
//   - the deck is keyed on the sorted target set, so a changed target
//     list rebuilds the deck instead of serving stale entries;
//   - the first card of a new cycle is never the last card of the
//     previous cycle (swapped with a random later card);
//   - the fallback remainder behind the dealt leader is also
//     Fisher-Yates shuffled (applyStrategyOrdering.ts #3959), so a
//     failing leader does not always fall through to the same peer.
type StrictRandomDeck struct {
	mu      sync.Mutex
	order   []int // shuffled indices into the current target list
	index   int   // next card to deal
	idsKey  string
	lastKey string // Key() of the last card of the previous cycle
}

// Order returns [dealt leader, ...shuffled rest] and advances the
// deck by one card. The dealt leader is the next un-dealt card; when
// the deck is exhausted (or the target set changed) it reshuffles.
func (d *StrictRandomDeck) Order(targets []OrderingTarget, rnd *rand.Rand) []OrderingTarget {
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == 1 {
		return copyTargets(targets)
	}
	r := rng(rnd)

	// Sorted-join key of the current set, as in shuffleDeck.ts.
	keys := make([]string, len(targets))
	for i, t := range targets {
		keys[i] = t.Key()
	}
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	idsKey := strings.Join(sorted, ",")

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idsKey != idsKey || d.index >= len(d.order) || len(d.order) != len(targets) {
		// (Re)shuffle a fresh deck of indices.
		d.order = make([]int, len(targets))
		for i := range d.order {
			d.order[i] = i
		}
		for i := len(d.order) - 1; i > 0; i-- {
			j := r.Intn(i + 1)
			d.order[i], d.order[j] = d.order[j], d.order[i]
		}
		// Never repeat the last card of the previous cycle first.
		if d.lastKey != "" && len(d.order) > 1 && targets[d.order[0]].Key() == d.lastKey {
			swap := 1 + r.Intn(len(d.order)-1)
			d.order[0], d.order[swap] = d.order[swap], d.order[0]
		}
		d.idsKey = idsKey
		d.index = 0
	}
	leader := d.order[d.index]
	d.index++
	d.lastKey = targets[leader].Key()

	rest := make([]OrderingTarget, 0, len(targets)-1)
	for i, t := range targets {
		if i != leader {
			rest = append(rest, t)
		}
	}
	for i := len(rest) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		rest[i], rest[j] = rest[j], rest[i]
	}
	return append([]OrderingTarget{targets[leader]}, rest...)
}

// Reset discards the current deck; the next Order reshuffles.
func (d *StrictRandomDeck) Reset() {
	d.mu.Lock()
	d.order = nil
	d.index = 0
	d.idsKey = ""
	d.lastKey = ""
	d.mu.Unlock()
}
