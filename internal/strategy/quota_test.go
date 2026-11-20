package strategy

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// Fixed "now" for every quota test, so reset/expiry arithmetic is
// exact and the tests never depend on the wall clock.
var quotaNow = time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC)

func quotaNames(in []QuotaSnapshot) []string {
	names := make([]string, len(in))
	for i, s := range in {
		names[i] = s.Provider
	}
	return names
}

func expectQuotaOrder(t *testing.T, got []QuotaSnapshot, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d targets %v, want %d %v", len(got), quotaNames(got), len(want), want)
	}
	for i, w := range want {
		if got[i].Provider != w {
			t.Errorf("position %d: got %q, want %q (full order %v)", i, got[i].Provider, w, quotaNames(got))
		}
	}
}

func expectApprox(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", what, got, want, tol)
	}
}

// --- quota-weighted ----------------------------------------------------

// Weights: a score 0.9 load 0 → 0.9; b score 0.9 load 1 → 0.45;
// c score 0.3 load 0 → 0.3; d score 0 → 0. Cumulative: a 0.9,
// b 1.35, c 1.65. Every boundary below is hand-computed from
// score/(1+load).
func TestQuotaWeightedWeight(t *testing.T) {
	cases := []struct {
		s    QuotaSnapshot
		want float64
	}{
		{QuotaSnapshot{Score: 0.9, Load: 0}, 0.9},
		{QuotaSnapshot{Score: 0.9, Load: 1}, 0.45},
		{QuotaSnapshot{Score: 0.3, Load: 0}, 0.3},
		{QuotaSnapshot{Score: 0, Load: 0}, 0},
		{QuotaSnapshot{Score: -1, Load: 0}, 0},     // negative score never wins
		{QuotaSnapshot{Score: 0.5, Load: -2}, 0.5}, // negative load counts as 0
		{QuotaSnapshot{Score: math.NaN(), Load: 0}, 0},
		{QuotaSnapshot{Score: 0.5, Load: math.NaN()}, 0.5},
	}
	for _, c := range cases {
		if got := QuotaWeightedWeight(c.s); got != c.want {
			t.Errorf("QuotaWeightedWeight(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// Half-open draw: the first cumulative weight STRICTLY greater
// than r wins; zero weights are skipped.
func TestPickWeightedIndex(t *testing.T) {
	weights := []float64{0.9, 0.45, 0.3, 0}
	cases := []struct {
		r    float64
		want int
	}{
		{0, 0},     // r == 0 lands on the first positive slot
		{0.899, 0}, // still inside a's interval
		{0.9, 1},   // exactly at a's edge → b (strictly greater)
		{1.349, 1}, // inside b's interval
		{1.35, 2},  // exactly at b's edge → c
		{1.649, 2}, // inside c's interval
		{2.0, 2},   // past the sum: last positive slot (rounding guard)
		{-1, 0},    // below zero: first positive slot
	}
	for _, c := range cases {
		if got := PickWeightedIndex(weights, c.r); got != c.want {
			t.Errorf("PickWeightedIndex(%v, %v) = %d, want %d", weights, c.r, got, c.want)
		}
	}
	if got := PickWeightedIndex([]float64{0, 0, -1}, 0); got != -1 {
		t.Errorf("PickWeightedIndex(all non-positive) = %d, want -1", got)
	}
	if got := PickWeightedIndex(nil, 0); got != -1 {
		t.Errorf("PickWeightedIndex(nil) = %d, want -1", got)
	}
}

// One positive weight among zeroes: that target MUST win the draw
// no matter the seed, and the rest follow by descending score.
// Input order is deliberately not the expected order, so this
// fails if the function ever returns its input unchanged.
func TestQuotaWeightedDrawSinglePositiveWeight(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "zero-a", Score: 0, Load: 0},
		{Provider: "winner", Score: 0.8, Load: 0},
		{Provider: "zero-b", Score: 0, Load: 0},
	}
	for seed := int64(0); seed < 10; seed++ {
		got := OrderQuotaWeighted(in, rand.New(rand.NewSource(seed)))
		expectQuotaOrder(t, got, "winner", "zero-a", "zero-b")
	}
	// Input is not mutated.
	expectQuotaOrder(t, in, "zero-a", "winner", "zero-b")
}

// With several positive weights the winner is always a
// positive-weight target, the result is a permutation of the
// input, and the same seed reproduces the same order.
func TestQuotaWeightedDrawProperties(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "a", Score: 0.9, Load: 0}, // weight 0.9
		{Provider: "b", Score: 0.9, Load: 1}, // weight 0.45
		{Provider: "c", Score: 0.3, Load: 0}, // weight 0.3
		{Provider: "d", Score: 0, Load: 0},   // weight 0: never wins
	}
	first := OrderQuotaWeighted(in, rand.New(rand.NewSource(7)))
	second := OrderQuotaWeighted(in, rand.New(rand.NewSource(7)))
	expectQuotaOrder(t, first, quotaNames(second)...)
	if first[0].Provider == "d" {
		t.Errorf("zero-weight target %q won the draw", first[0].Provider)
	}
	// Non-winners follow by descending score.
	for i := 1; i < len(first)-1; i++ {
		if first[i].Score < first[i+1].Score {
			t.Errorf("non-winners not score-ordered: %v", quotaNames(first))
		}
	}
	// Nil rnd is deterministic (fixed seed 1), not a crash.
	nilOne := OrderQuotaWeighted(in, nil)
	nilTwo := OrderQuotaWeighted(in, nil)
	expectQuotaOrder(t, nilOne, quotaNames(nilTwo)...)
	if len(OrderQuotaWeighted(nil, rand.New(rand.NewSource(1)))) != 0 {
		t.Errorf("OrderQuotaWeighted(nil) should be empty")
	}
}

// --- headroom ----------------------------------------------------------

// headroom = 1 − max(util5h, util7d):
// a: 1−max(.2,.8) = .2; b: 1−max(.1,.1) = .9; c: 1−max(.9,0) = .1.
// Input order c,a,b is not the expected order b,a,c.
func TestHeadroomOrder(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "c", Util5h: 0.9, Util7d: 0},
		{Provider: "a", Util5h: 0.2, Util7d: 0.8},
		{Provider: "b", Util5h: 0.1, Util7d: 0.1},
	}
	expectQuotaOrder(t, OrderHeadroom(in), "b", "a", "c")
	expectQuotaOrder(t, in, "c", "a", "b") // input not mutated
}

func TestHeadroomValues(t *testing.T) {
	cases := []struct {
		s    QuotaSnapshot
		want float64
	}{
		{QuotaSnapshot{Util5h: 0.2, Util7d: 0.8}, 0.2}, // worst window drives it
		{QuotaSnapshot{Util5h: 0, Util7d: 0}, 1},       // unknown → full headroom (fail-open)
		{QuotaSnapshot{Util5h: math.NaN(), Util7d: 0.5}, 0.5},
		{QuotaSnapshot{Util5h: 2, Util7d: -1}, 0}, // clamped to [0,1]
		{QuotaSnapshot{Util5h: math.Inf(1), Util7d: 0}, 0},
	}
	for _, c := range cases {
		expectApprox(t, Headroom(c.s), c.want, 1e-9, "Headroom")
	}
}

// Missing utilisation is fail-open (full headroom) and therefore
// ranks first; equal headroom keeps input order (stable sort).
func TestHeadroomMissingDataRanksFirstStable(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "used", Util5h: 0.5, Util7d: 0.5},
		{Provider: "unknown-a"},
		{Provider: "unknown-b", Util5h: math.NaN()},
	}
	expectQuotaOrder(t, OrderHeadroom(in), "unknown-a", "unknown-b", "used")
}

// --- expiry-first ------------------------------------------------------

// score = usable / hours-until-expiry:
// a: .8/2h = .4; b: .8/8h = .1; c: .5/1h = .5; d: 0 → 0 (spent).
// Input order b,d,a,c is not the expected order c,a,b,d.
func TestExpiryFirstOrder(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "b", QuotaRemaining: 0.8, ExpiresAt: quotaNow.Add(8 * time.Hour)},
		{Provider: "d", QuotaRemaining: 0, ExpiresAt: quotaNow.Add(30 * time.Minute)},
		{Provider: "a", QuotaRemaining: 0.8, ExpiresAt: quotaNow.Add(2 * time.Hour)},
		{Provider: "c", QuotaRemaining: 0.5, ExpiresAt: quotaNow.Add(1 * time.Hour)},
	}
	expectQuotaOrder(t, OrderExpiryFirst(in, quotaNow), "c", "a", "b", "d")
	expectQuotaOrder(t, in, "b", "d", "a", "c") // input not mutated
}

func TestExpiryFirstScore(t *testing.T) {
	cases := []struct {
		name string
		s    QuotaSnapshot
		want float64
	}{
		{"soon expiry", QuotaSnapshot{QuotaRemaining: 0.5, ExpiresAt: quotaNow.Add(time.Hour)}, 0.5},
		{"later expiry", QuotaSnapshot{QuotaRemaining: 0.8, ExpiresAt: quotaNow.Add(8 * time.Hour)}, 0.1},
		// No ExpiresAt: the ResetAt deadline is used instead.
		{"reset fallback", QuotaSnapshot{QuotaRemaining: 0.6, ResetAt: quotaNow.Add(3 * time.Hour)}, 0.2},
		// No deadline at all: score is the usable fraction alone.
		{"no deadline", QuotaSnapshot{QuotaRemaining: 0.9}, 0.9},
		// Spent, and at/below the 1% floor: never preferred.
		{"spent", QuotaSnapshot{QuotaRemaining: 0, ExpiresAt: quotaNow.Add(time.Hour)}, 0},
		{"floor", QuotaSnapshot{QuotaRemaining: 0.01, ExpiresAt: quotaNow.Add(time.Hour)}, 0},
		// A reset seconds away divides by the 0.25h minimum, not ~0.
		{"min hours", QuotaSnapshot{QuotaRemaining: 0.5, ExpiresAt: quotaNow.Add(time.Minute)}, 2.0},
		// No telemetry at all: nothing is known to be expiring.
		{"no data", QuotaSnapshot{}, 0},
	}
	for _, c := range cases {
		expectApprox(t, ExpiryFirstScore(c.s, quotaNow), c.want, 1e-9, "ExpiryFirstScore "+c.name)
	}
}

// Everything scored zero keeps the input (priority) order — no
// rotation, no re-ranking of spent/unknown targets.
func TestExpiryFirstAllZeroKeepsInputOrder(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "x", QuotaRemaining: 0, ExpiresAt: quotaNow.Add(time.Hour)},
		{Provider: "y"},
		{Provider: "z", QuotaRemaining: 0.005, ExpiresAt: quotaNow.Add(time.Hour)},
	}
	expectQuotaOrder(t, OrderExpiryFirst(in, quotaNow), "x", "y", "z")
}

// --- reset-aware -------------------------------------------------------
//
// Hand-computed with session 0.35 / weekly 0.65, session weights
// (0.45 remaining, 0.55 pressure), weekly weights (0.25, 0.75),
// ResetAt = now+5h (session urgency 0, weekly urgency 1−5/168):
// a (unused): session 0.45, weekly 0.25 → 0.32.
// b (90% used): session 0.045, weekly 0.679911 → 0.457692.
// c (50% used, no reset → urgency 0.5): session 0.3625, weekly
// 0.3125 → 0.33.
// So the nearly-empty account about to reset ranks FIRST: this is
// the reference router's recovery signal (scoreResetAwareQuota favours an
// account that will be useful again soon), not a bug.
func TestResetAwareScoreValues(t *testing.T) {
	a := QuotaSnapshot{Util5h: 0, Util7d: 0, ResetAt: quotaNow.Add(5 * time.Hour)}
	b := QuotaSnapshot{Util5h: 0.9, Util7d: 0.9, ResetAt: quotaNow.Add(5 * time.Hour)}
	c := QuotaSnapshot{Util5h: 0.5, Util7d: 0.5}
	expectApprox(t, ResetAwareScore(a, quotaNow), 0.32, 1e-9, "unused score")
	expectApprox(t, ResetAwareScore(b, quotaNow), 0.457692, 1e-6, "nearly-empty score")
	expectApprox(t, ResetAwareScore(c, quotaNow), 0.33, 1e-9, "half-used no-reset score")
	// No telemetry at all: the documented no-quota floor, not 0 or 1.
	expectApprox(t, ResetAwareScore(QuotaSnapshot{}, quotaNow), QuotaNoDataFloor, 0, "no-data floor")
}

func TestResetAwareOrder(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "a", Util5h: 0, Util7d: 0, ResetAt: quotaNow.Add(5 * time.Hour)},
		{Provider: "b", Util5h: 0.9, Util7d: 0.9, ResetAt: quotaNow.Add(5 * time.Hour)},
		{Provider: "c", Util5h: 0.5, Util7d: 0.5},
	}
	expectQuotaOrder(t, OrderResetAware(in, quotaNow), "b", "c", "a")
	expectQuotaOrder(t, in, "a", "b", "c") // input not mutated
}

// Exhaustion guard: 5% session remaining halves the blended score
// (0.170375 × max(0.05, 0.05/0.10) = 0.0851875), so a nearly-spent
// window is never ranked as healthy.
func TestResetAwareExhaustionGuard(t *testing.T) {
	s := QuotaSnapshot{Util5h: 0.95, Util7d: 0, ResetAt: quotaNow.Add(5 * time.Hour)}
	expectApprox(t, ResetAwareScore(s, quotaNow), 0.0851875, 1e-9, "guarded score")
	// At exactly 10% remaining the guard does not fire: blended
	// score is 0.35×0.045 + 0.65×0.25 = 0.17825.
	atEdge := QuotaSnapshot{Util5h: 0.9, Util7d: 0, ResetAt: quotaNow.Add(5 * time.Hour)}
	expectApprox(t, ResetAwareScore(atEdge, quotaNow), 0.17825, 1e-9, "guard edge score")
}

// --- reset-window ------------------------------------------------------

// Ascending time until reset; no known reset sorts last in input
// order. Input order c,a,b is not the expected order b,a,c.
func TestResetWindowOrder(t *testing.T) {
	in := []QuotaSnapshot{
		{Provider: "c"}, // no reset known → last
		{Provider: "a", ResetAt: quotaNow.Add(2 * time.Hour)},
		{Provider: "b", ResetAt: quotaNow.Add(30 * time.Minute)},
	}
	expectQuotaOrder(t, OrderResetWindow(in, quotaNow), "b", "a", "c")
	expectQuotaOrder(t, in, "c", "a", "b") // input not mutated
}

func TestResetWindowElapsedAndTies(t *testing.T) {
	// An already-elapsed reset collapses to 0 remaining and goes
	// first; equal resets keep input order.
	in := []QuotaSnapshot{
		{Provider: "later", ResetAt: quotaNow.Add(time.Hour)},
		{Provider: "same-b", ResetAt: quotaNow.Add(30 * time.Minute)},
		{Provider: "elapsed", ResetAt: quotaNow.Add(-time.Hour)},
		{Provider: "same-a", ResetAt: quotaNow.Add(30 * time.Minute)},
	}
	expectQuotaOrder(t, OrderResetWindow(in, quotaNow), "elapsed", "same-b", "same-a", "later")
}

// Unknown resets all tie at +Inf and keep input order.
func TestResetWindowAllUnknownKeepsInputOrder(t *testing.T) {
	in := []QuotaSnapshot{{Provider: "x"}, {Provider: "y"}, {Provider: "z"}}
	expectQuotaOrder(t, OrderResetWindow(in, quotaNow), "x", "y", "z")
}
