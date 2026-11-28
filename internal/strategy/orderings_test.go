// Acceptance tests for the M3 ordering strategies in orderings.go.
//
// Rule for every test here: it must FAIL if the strategy function is
// removed or returns the input order unchanged. All randomness is
// seeded (math/rand with a fixed seed) so the tests are reproducible.
//
// the sector baseline semantics under test (read-only reference,
// the local reference clone):
//   - random:        fisherYatesShuffle, applyStrategyOrdering.ts "random"
//   - strict-random: shuffleDeck.ts getNextFromDeck — deck + mutex, no
//     repeat until exhausted, last-of-cycle != first-of-next;
//     applyStrategyOrdering.ts also shuffles the fallback rest
//   - fill-first:    applyStrategyOrdering.ts "fill-first" preserves priority
//     order (the drain happens by retrying the first target);
//     our pure-function form additionally demotes
//     quota-exhausted targets behind targets that still have
//     quota, which is the drain expressed as an ordering
//   - least-used:    targetSorters.ts:107 sortTargetsByUsage — usage ascending
//   - p2c:           targetSorters.ts:136 orderTargetsByPowerOfTwoChoices —
//     draw 2 distinct indices, better score first, rest in
//     input order
//   - weighted:      targetSorters.ts:28 selectWeightedTarget — total-weight
//     draw (r = rand*total, walk subtracting each weight,
//     first r <= 0 wins; total <= 0 -> uniform pick) and
//     orderTargetsForWeightedFallback (selected first, rest
//     weight-desc unless preserveExistingOrder)
package strategy

import (
	"math"
	"math/rand"
	"sync"
	"testing"
)

func orderingTargets() []OrderingTarget {
	return []OrderingTarget{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
		{Provider: "c", Model: "m3"},
		{Provider: "d", Model: "m4"},
	}
}

func orderingKeys(ts []OrderingTarget) []string {
	keys := make([]string, len(ts))
	for i, t := range ts {
		keys[i] = t.Key()
	}
	return keys
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOrderingRandomChangesOrder: a seeded Fisher-Yates shuffle must
// actually reorder (fails on identity) while preserving the set,
// leaving the input untouched, and reproducing the same output for
// the same seed.
func TestOrderingRandomChangesOrder(t *testing.T) {
	in := orderingTargets()
	before := orderingKeys(in)

	got := ShuffleTargets(in, rand.New(rand.NewSource(7)))
	if sameKeys(orderingKeys(got), before) {
		t.Fatalf("ShuffleTargets returned input order unchanged: %v", orderingKeys(got))
	}
	if len(got) != len(in) {
		t.Fatalf("ShuffleTargets changed length: got %d, want %d", len(got), len(in))
	}
	seen := map[string]int{}
	for _, k := range orderingKeys(got) {
		seen[k]++
	}
	for _, k := range before {
		if seen[k] != 1 {
			t.Fatalf("ShuffleTargets lost/duplicated target %q: %v", k, orderingKeys(got))
		}
	}
	if !sameKeys(orderingKeys(in), before) {
		t.Fatalf("ShuffleTargets mutated its input: %v", orderingKeys(in))
	}
	again := ShuffleTargets(orderingTargets(), rand.New(rand.NewSource(7)))
	if !sameKeys(orderingKeys(got), orderingKeys(again)) {
		t.Fatalf("same seed gave different shuffles: %v vs %v", orderingKeys(got), orderingKeys(again))
	}
}

// TestOrderingRandomEdgeCases: empty and singleton inputs pass through.
func TestOrderingRandomEdgeCases(t *testing.T) {
	if got := ShuffleTargets(nil, rand.New(rand.NewSource(1))); len(got) != 0 {
		t.Fatalf("ShuffleTargets(nil) = %v, want empty", got)
	}
	one := []OrderingTarget{{Provider: "a", Model: "m1"}}
	if got := ShuffleTargets(one, rand.New(rand.NewSource(1))); len(got) != 1 || got[0].Key() != "a/m1" {
		t.Fatalf("ShuffleTargets(singleton) = %v, want [a/m1]", orderingKeys(got))
	}
}

// TestOrderingRoundRobinCyclesEvenly: successive Orders rotate the
// start position by one, so every target leads exactly once per cycle.
func TestOrderingRoundRobinCyclesEvenly(t *testing.T) {
	rr := &RoundRobin{}
	in := orderingTargets()
	firsts := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		got := rr.Order(in)
		if len(got) != len(in) {
			t.Fatalf("call %d: length %d, want %d", i, len(got), len(in))
		}
		firsts = append(firsts, got[0].Key())
		// Full rotation shape: the order must be the input rotated.
		want := append(append([]OrderingTarget{}, in[i%len(in):]...), in[:i%len(in)]...)
		if !sameKeys(orderingKeys(got), orderingKeys(want)) {
			t.Fatalf("call %d: got %v, want rotation %v", i, orderingKeys(got), orderingKeys(want))
		}
	}
	wantFirsts := []string{"a/m1", "b/m2", "c/m3", "d/m4", "a/m1", "b/m2", "c/m3", "d/m4"}
	if !sameKeys(firsts, wantFirsts) {
		t.Fatalf("round-robin firsts = %v, want %v (fails if rotation is identity)", firsts, wantFirsts)
	}
}

// TestOrderingRoundRobinSurvivesListChange: the target list may grow
// or shrink between calls; rotation must stay valid, never panic,
// and still cycle through whatever list is current.
func TestOrderingRoundRobinSurvivesListChange(t *testing.T) {
	rr := &RoundRobin{}
	four := orderingTargets()
	rr.Order(four)
	rr.Order(four)

	two := orderingTargets()[:2]
	for i := 0; i < 4; i++ {
		got := rr.Order(two)
		if len(got) != 2 || (got[0].Key() != "a/m1" && got[0].Key() != "b/m2") {
			t.Fatalf("shrunk list call %d: got %v, want a 2-target rotation", i, orderingKeys(got))
		}
	}

	five := append(orderingTargets(), OrderingTarget{Provider: "e", Model: "m5"})
	seenFirst := map[string]bool{}
	for i := 0; i < 5; i++ {
		seenFirst[rr.Order(five)[0].Key()] = true
	}
	if len(seenFirst) != 5 {
		t.Fatalf("grown list: only %d distinct leaders in a full cycle, want 5: %v", len(seenFirst), seenFirst)
	}
}

// TestOrderingRoundRobinConcurrent: concurrent Orders are race-safe
// and every result is a valid permutation. Run with -race to prove it.
func TestOrderingRoundRobinConcurrent(t *testing.T) {
	rr := &RoundRobin{}
	in := orderingTargets()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got := rr.Order(in)
				if len(got) != len(in) {
					t.Errorf("concurrent Order length %d, want %d", len(got), len(in))
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestOrderingStrictRandomNoRepeatUntilExhausted: one full cycle of
// deck draws must visit every target exactly once as leader, and the
// last leader of a cycle must not repeat as the first leader of the
// next cycle (shuffleDeck.ts boundary rule).
func TestOrderingStrictRandomNoRepeatUntilExhausted(t *testing.T) {
	deck := &StrictRandomDeck{}
	in := orderingTargets()
	rnd := rand.New(rand.NewSource(42))

	firsts := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		got := deck.Order(in, rnd)
		if len(got) != len(in) {
			t.Fatalf("call %d: length %d, want %d", i, len(got), len(in))
		}
		firsts = append(firsts, got[0].Key())
		// Each returned list must be a full permutation.
		seen := map[string]int{}
		for _, k := range orderingKeys(got) {
			seen[k]++
		}
		for _, k := range orderingKeys(in) {
			if seen[k] != 1 {
				t.Fatalf("call %d: not a permutation: %v", i, orderingKeys(got))
			}
		}
	}
	seen := map[string]int{}
	for _, k := range firsts {
		seen[k]++
	}
	for _, k := range orderingKeys(in) {
		if seen[k] != 1 {
			t.Fatalf("one deck cycle visited leaders %v, want each target exactly once", firsts)
		}
	}
	nextFirst := deck.Order(in, rnd)[0].Key()
	if nextFirst == firsts[len(firsts)-1] {
		t.Fatalf("first leader of new cycle %q repeats last leader of previous cycle", nextFirst)
	}
}

// TestOrderingStrictRandomResetsOnListChange: when the target set
// changes mid-cycle the deck rebuilds instead of serving stale keys.
func TestOrderingStrictRandomResetsOnListChange(t *testing.T) {
	deck := &StrictRandomDeck{}
	in := orderingTargets()
	rnd := rand.New(rand.NewSource(9))
	deck.Order(in, rnd)
	deck.Order(in, rnd)

	changed := []OrderingTarget{{Provider: "x", Model: "m9"}, {Provider: "y", Model: "m8"}}
	seen := map[string]int{}
	for i := 0; i < 2; i++ {
		got := deck.Order(changed, rnd)
		if len(got) != 2 {
			t.Fatalf("changed list call %d: got %v", i, orderingKeys(got))
		}
		seen[got[0].Key()]++
		for _, k := range orderingKeys(got) {
			if k != "x/m9" && k != "y/m8" {
				t.Fatalf("changed list served stale target %q: %v", k, orderingKeys(got))
			}
		}
	}
	if seen["x/m9"] != 1 || seen["y/m8"] != 1 {
		t.Fatalf("changed list cycle leaders = %v, want each of x/m9, y/m8 exactly once", seen)
	}
}

// TestOrderingFillFirstDemotesExhausted: the declared order stands
// among targets that still have quota; a quota-exhausted target drops
// behind them (the drain: first target first until its quota is gone).
func TestOrderingFillFirstDemotesExhausted(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", QuotaRemaining: 0},
		{Provider: "b", Model: "m2", QuotaRemaining: 10},
		{Provider: "c", Model: "m3", QuotaRemaining: 5},
	}
	got := OrderFillFirst(in)
	want := []string{"b/m2", "c/m3", "a/m1"}
	if !sameKeys(orderingKeys(got), want) {
		t.Fatalf("OrderFillFirst = %v, want %v (fails if exhausted target stays first)", orderingKeys(got), want)
	}
	if !sameKeys(orderingKeys(in), []string{"a/m1", "b/m2", "c/m3"}) {
		t.Fatalf("OrderFillFirst mutated its input")
	}

	// All fresh: declared order is the drain order, unchanged.
	fresh := []OrderingTarget{
		{Provider: "a", Model: "m1", QuotaRemaining: 3},
		{Provider: "b", Model: "m2", QuotaRemaining: 1},
	}
	if got := OrderFillFirst(fresh); !sameKeys(orderingKeys(got), []string{"a/m1", "b/m2"}) {
		t.Fatalf("OrderFillFirst(all fresh) = %v, want declared order", orderingKeys(got))
	}
	// All exhausted: declared order stands (stable, nothing to drain).
	dead := []OrderingTarget{
		{Provider: "a", Model: "m1", QuotaRemaining: 0},
		{Provider: "b", Model: "m2", QuotaRemaining: -2},
	}
	if got := OrderFillFirst(dead); !sameKeys(orderingKeys(got), []string{"a/m1", "b/m2"}) {
		t.Fatalf("OrderFillFirst(all exhausted) = %v, want declared order", orderingKeys(got))
	}
}

// TestOrderingLeastUsedAscending: fewest attempts first; ties keep
// declared order (stable sort).
func TestOrderingLeastUsedAscending(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Attempts: 5},
		{Provider: "b", Model: "m2", Attempts: 1},
		{Provider: "c", Model: "m3", Attempts: 3},
		{Provider: "d", Model: "m4", Attempts: 1},
	}
	got := OrderLeastUsed(in)
	want := []string{"b/m2", "d/m4", "c/m3", "a/m1"}
	if !sameKeys(orderingKeys(got), want) {
		t.Fatalf("OrderLeastUsed = %v, want %v (fails if input order is returned)", orderingKeys(got), want)
	}
	if in[0].Attempts != 5 || in[1].Attempts != 1 {
		t.Fatalf("OrderLeastUsed mutated its input")
	}
}

// TestOrderingP2CBetterScoreFirst: with exactly two targets both are
// always drawn, so the higher-Score target must lead on every seed —
// an identity return fails whenever the better target is listed second.
func TestOrderingP2CBetterScoreFirst(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "low", Model: "m1", Score: 0.1},
		{Provider: "high", Model: "m2", Score: 0.9},
	}
	for _, seed := range []int64{1, 2, 3, 7, 42} {
		got := OrderP2C(in, rand.New(rand.NewSource(seed)))
		if len(got) != 2 || got[0].Key() != "high/m2" || got[1].Key() != "low/m1" {
			t.Fatalf("seed %d: OrderP2C = %v, want [high/m2 low/m1]", seed, orderingKeys(got))
		}
	}
	// -Inf score models an OPEN breaker (targetSorters.ts getP2CTargetScore):
	// it must never beat a finite score.
	broken := []OrderingTarget{
		{Provider: "open", Model: "m1", Score: math.Inf(-1)},
		{Provider: "ok", Model: "m2", Score: 0.2},
	}
	got := OrderP2C(broken, rand.New(rand.NewSource(1)))
	if got[0].Key() != "ok/m2" {
		t.Fatalf("OrderP2C with breaker-open target = %v, want ok/m2 first", orderingKeys(got))
	}
	// Singleton / empty pass through.
	if got := OrderP2C(nil, rand.New(rand.NewSource(1))); len(got) != 0 {
		t.Fatalf("OrderP2C(nil) = %v, want empty", got)
	}
}

// TestOrderingP2CPreservesRestOrder: the loser of the draw and every
// undrawn target keep their input relative order behind the winner.
func TestOrderingP2CPreservesRestOrder(t *testing.T) {
	in := orderingTargets()
	for i := range in {
		in[i].Score = float64(10 - i) // a best, d worst; scores strictly decreasing
	}
	got := OrderP2C(in, rand.New(rand.NewSource(5)))
	if len(got) != len(in) {
		t.Fatalf("OrderP2C length %d, want %d", len(got), len(in))
	}
	// Winner is one of the two drawn; the rest must appear in input order.
	rest := got[1:]
	prev := -1
	for _, rt := range rest {
		idx := -1
		for j, ot := range in {
			if ot.Key() == rt.Key() {
				idx = j
			}
		}
		if idx < 0 || idx <= prev {
			t.Fatalf("OrderP2C rest %v is not in input order", orderingKeys(rest))
		}
		prev = idx
	}
}

// TestWeightedDrawMatchesReferenceSemantics: the per-target weight
// draw of targetSorters.ts:28 selectWeightedTarget — NOT the old
// "weighted" behaviour in strategy.go (WeightsFor re-weighting 5
// scoring factors, which has no per-target weight or draw at all).
func TestWeightedDrawMatchesReferenceSemantics(t *testing.T) {
	// Weight concentrated on one target: that target always wins.
	onlyFirst := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 1},
		{Provider: "b", Model: "m2", Weight: 0},
		{Provider: "c", Model: "m3", Weight: 0},
	}
	for _, seed := range []int64{1, 2, 3} {
		got, ok := WeightedDraw(onlyFirst, rand.New(rand.NewSource(seed)))
		if !ok || got.Key() != "a/m1" {
			t.Fatalf("seed %d: WeightedDraw(weight 1,0,0) = %v, want a/m1", seed, got.Key())
		}
	}
	onlyLast := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 0},
		{Provider: "b", Model: "m2", Weight: 0},
		{Provider: "c", Model: "m3", Weight: 1},
	}
	got, ok := WeightedDraw(onlyLast, rand.New(rand.NewSource(1)))
	if !ok || got.Key() != "c/m3" {
		t.Fatalf("WeightedDraw(weight 0,0,1) = %v, want c/m3", got.Key())
	}

	// Total weight <= 0: uniform pick among all targets (the sector baseline
	// fallback branch) — result must be a member, never an error.
	zero := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 0},
		{Provider: "b", Model: "m2", Weight: 0},
	}
	got, ok = WeightedDraw(zero, rand.New(rand.NewSource(1)))
	if !ok || (got.Key() != "a/m1" && got.Key() != "b/m2") {
		t.Fatalf("WeightedDraw(all zero weight) = %v, %v, want a member", got.Key(), ok)
	}

	// Empty list: no draw.
	if _, ok := WeightedDraw(nil, rand.New(rand.NewSource(1))); ok {
		t.Fatalf("WeightedDraw(nil) returned ok=true, want false")
	}
}

// TestWeightedDrawProportions: a 3:1 weight split must draw the heavy
// target about 75% of the time over many seeded draws.
func TestWeightedDrawProportions(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 3},
		{Provider: "b", Model: "m2", Weight: 1},
	}
	rnd := rand.New(rand.NewSource(1234))
	first := 0
	const draws = 2000
	for i := 0; i < draws; i++ {
		got, ok := WeightedDraw(in, rnd)
		if !ok {
			t.Fatalf("draw %d: ok=false", i)
		}
		if got.Key() == "a/m1" {
			first++
		}
	}
	share := float64(first) / draws
	if share < 0.65 || share > 0.85 {
		t.Fatalf("WeightedDraw 3:1 share = %.3f, want ~0.75 in [0.65, 0.85]", share)
	}
}

// TestOrderingWeightedFallback: selected target first, rest by weight
// descending — targetSorters.ts orderTargetsForWeightedFallback.
func TestOrderingWeightedFallback(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 1},
		{Provider: "b", Model: "m2", Weight: 5},
		{Provider: "c", Model: "m3", Weight: 3},
	}
	got := OrderWeightedFallback(in, OrderingTarget{Provider: "a", Model: "m1"}, false)
	want := []string{"a/m1", "b/m2", "c/m3"}
	if !sameKeys(orderingKeys(got), want) {
		t.Fatalf("OrderWeightedFallback = %v, want %v", orderingKeys(got), want)
	}
	// preserveExistingOrder keeps the rest in declared order.
	got = OrderWeightedFallback(in, OrderingTarget{Provider: "a", Model: "m1"}, true)
	if !sameKeys(orderingKeys(got), []string{"a/m1", "b/m2", "c/m3"}) {
		t.Fatalf("OrderWeightedFallback(preserve) = %v, want declared rest order", orderingKeys(got))
	}
	// Unknown selected target: rest only, weight-desc.
	got = OrderWeightedFallback(in, OrderingTarget{Provider: "z", Model: "m9"}, false)
	if !sameKeys(orderingKeys(got), []string{"b/m2", "c/m3", "a/m1"}) {
		t.Fatalf("OrderWeightedFallback(unknown selected) = %v, want weight-desc rest", orderingKeys(got))
	}
}

// TestOrderingHelpersDoNotMutateInput: every pure orderer returns a
// new slice and leaves the caller's list alone.
func TestOrderingHelpersDoNotMutateInput(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Score: 0.1, Attempts: 5, QuotaRemaining: 0, Weight: 1},
		{Provider: "b", Model: "m2", Score: 0.9, Attempts: 1, QuotaRemaining: 9, Weight: 5},
		{Provider: "c", Model: "m3", Score: 0.5, Attempts: 3, QuotaRemaining: 2, Weight: 3},
	}
	snapshot := append([]OrderingTarget{}, in...)
	rnd := rand.New(rand.NewSource(3))
	ShuffleTargets(in, rnd)
	OrderLeastUsed(in)
	OrderFillFirst(in)
	OrderP2C(in, rnd)
	OrderWeightedFallback(in, in[0], false)
	if len(in) != len(snapshot) {
		t.Fatalf("input length changed")
	}
	for i := range in {
		if in[i] != snapshot[i] {
			t.Fatalf("input mutated at index %d: %+v vs %+v", i, in[i], snapshot[i])
		}
	}
}
