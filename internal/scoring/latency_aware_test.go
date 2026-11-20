package scoring

import (
	"math"
	"testing"
)

// G10 Area 12 row 9: latency-aware strategy weights + p2c latency term.
//
// Hand-computed expectations use the documented normalisations:
// latencyComponent = 1/(1+ms/1000), costComponent = 1/(1+$/10).

func TestLatencyAwareModePicksLowerLatencyAtEqualCost(t *testing.T) {
	fast := Candidate{ProviderID: "fast", Model: "m"}
	slow := Candidate{ProviderID: "slow", Model: "m"}
	signals := map[Candidate]Signals{
		fast: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		slow: {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
	}
	ranked := Rank([]Candidate{slow, fast}, signals, ModeLatencyAware)
	if ranked[0] != fast {
		t.Fatalf("latency-aware first = %v, want fast (100ms beats 500ms at equal cost)", ranked[0])
	}
	if got := Modes[ModeLatencyAware]; got.Latency+got.Success+got.Cost+got.Quality+got.Quota != 1 {
		t.Fatalf("ModeLatencyAware weights sum = %v, want 1", got)
	}
}

func TestLatencyAwareNoiseGapLosesToLargeCostGap(t *testing.T) {
	// The documented "noise" threshold is not a magic constant: it
	// falls out of the ModeLatencyAware weights. A 100ms vs 105ms
	// latency gap moves the latency component by only
	// 1/1.100 - 1/1.105 = 0.0041, weighted 0.70 -> 0.0029. A
	// $100/1M vs $1/1M cost gap moves the cost component by
	// 1/1.1 - 1/11 = 0.818, weighted 0.05 -> 0.0409 — 14x larger.
	// So the slow-but-cheap candidate must win: the latency gap is
	// noise, the cost gap is real. (Least-latency, by contrast,
	// would still pick the fast one — see strategy tests.)
	fastExp := Candidate{ProviderID: "fast-exp", Model: "m"}
	slowCheap := Candidate{ProviderID: "slow-cheap", Model: "m"}
	signals := map[Candidate]Signals{
		fastExp:   {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 100, QualityScore: 0.7, QuotaHint: 100},
		slowCheap: {LatencyMs: 105, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
	}
	ranked := Rank([]Candidate{fastExp, slowCheap}, signals, ModeLatencyAware)
	if ranked[0] != slowCheap {
		t.Fatalf("latency-aware first = %v, want slow-cheap (5ms gap is noise vs $99 cost gap)", ranked[0])
	}
}

func TestP2CLatencyTerm(t *testing.T) {
	w := Modes[ModeLatencyAware]
	fast := Signals{LatencyMs: 100}
	slow := Signals{LatencyMs: 500}
	// Hand-computed: 0.70 * (1/1.1 - 1/1.5) = 0.70 * 0.2424 = 0.1697.
	want := 0.70 * (1.0/1.1 - 1.0/1.5)
	if got := P2CLatencyTerm(fast, slow, w); math.Abs(got-want) > 1e-9 {
		t.Errorf("P2CLatencyTerm(100ms, 500ms) = %v, want %v", got, want)
	}
	// Symmetric: argument order does not matter.
	if got := P2CLatencyTerm(slow, fast, w); math.Abs(got-want) > 1e-9 {
		t.Errorf("P2CLatencyTerm(500ms, 100ms) = %v, want %v", got, want)
	}
	// Equal latencies: no term.
	if got := P2CLatencyTerm(fast, fast, w); got != 0 {
		t.Errorf("P2CLatencyTerm(equal) = %v, want 0", got)
	}
	// No samples on either side: no term — unknown latency never
	// wins a draw on latency alone.
	if got := P2CLatencyTerm(Signals{}, Signals{}, w); got != 0 {
		t.Errorf("P2CLatencyTerm(unknown, unknown) = %v, want 0", got)
	}
	// Zero latency weight: no term even with a real gap.
	if got := P2CLatencyTerm(fast, slow, Weights{Latency: 0, Success: 1}); got != 0 {
		t.Errorf("P2CLatencyTerm with zero latency weight = %v, want 0", got)
	}
}

func TestPickP2CChoosesLowerLatency(t *testing.T) {
	fast := Candidate{ProviderID: "fast", Model: "m"}
	slow := Candidate{ProviderID: "slow", Model: "m"}
	signals := map[Candidate]Signals{
		fast: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		slow: {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
	}
	if got := PickP2C(slow, fast, signals, ModeLatencyAware); got != fast {
		t.Errorf("PickP2C = %v, want fast", got)
	}
	if got := PickP2C(fast, slow, signals, ModeLatencyAware); got != fast {
		t.Errorf("PickP2C (swapped args) = %v, want fast", got)
	}
}

func TestPickP2CFallsBackToScoreOnLatencyTie(t *testing.T) {
	// Same latency, different cost: the latency term is 0, so the
	// full weighted score decides — the cheaper candidate wins.
	a := Candidate{ProviderID: "a", Model: "m"}
	b := Candidate{ProviderID: "b", Model: "m"}
	signals := map[Candidate]Signals{
		a: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 100, QualityScore: 0.7, QuotaHint: 100},
		b: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
	}
	if got := PickP2C(a, b, signals, ModeLatencyAware); got != b {
		t.Errorf("PickP2C on latency tie = %v, want b (cheaper wins on score)", got)
	}
}

func TestRankP2CSeededPicksLowerLatencyFirst(t *testing.T) {
	fast := Candidate{ProviderID: "fast", Model: "m"}
	slow := Candidate{ProviderID: "slow", Model: "m"}
	mid := Candidate{ProviderID: "mid", Model: "m"}
	signals := map[Candidate]Signals{
		fast: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		mid:  {LatencyMs: 300, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		slow: {LatencyMs: 900, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
	}
	cands := []Candidate{slow, mid, fast}
	// The p2c tournament can only emit the fastest candidate once
	// it has been drawn against another candidate; with three
	// candidates the fastest is not guaranteed first for every
	// seed. What IS guaranteed: the ranking is a permutation, it
	// is deterministic for a fixed seed, and the slowest candidate
	// can never win a draw against a faster one — so over seeds
	// the fastest candidate leads more often than the slowest.
	// Assert the deterministic + permutation properties here, and
	// the head-to-head property via PickP2C above.
	first := RankP2CSeeded(cands, signals, ModeLatencyAware, 7)
	second := RankP2CSeeded(cands, signals, ModeLatencyAware, 7)
	if len(first) != 3 {
		t.Fatalf("RankP2C returned %d candidates, want 3", len(first))
	}
	seen := map[Candidate]bool{}
	for _, c := range first {
		if seen[c] {
			t.Fatalf("RankP2C duplicated %v", c)
		}
		seen[c] = true
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("RankP2CSeeded not deterministic: %v vs %v", first, second)
		}
	}
	// Two candidates only: the draw is forced (both are drawn), so
	// the lower-latency one must lead for every seed.
	for _, seed := range []int64{1, 2, 3, 42} {
		ranked := RankP2CSeeded([]Candidate{slow, fast}, signals, ModeLatencyAware, seed)
		if ranked[0] != fast {
			t.Errorf("seed %d: RankP2C first = %v, want fast", seed, ranked[0])
		}
	}
}

// TestLatencyAwareToggleOnOff is the toggle-check for the latency-aware
// feature (G10 Area 12 row 9, Block 2 Batch 1): the SAME two candidates,
// identical in every signal except latency, must order strictly by
// latency with latency-awareness ON — and score exactly EQUAL with it
// OFF. If the implementation ignored latency (e.g. ModeLatencyAware
// carried a zero latency weight), the ON assertions fail; if latency
// leaked into every weight set, the OFF assertions fail. Either
// half failing means the toggle is not real.
func TestLatencyAwareToggleOnOff(t *testing.T) {
	fast := Candidate{ProviderID: "fast", Model: "m"}
	slow := Candidate{ProviderID: "slow", Model: "m"}
	signals := map[Candidate]Signals{
		fast: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		slow: {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
	}

	// ON: latency-aware (latency weight 0.70). The slow provider
	// scores strictly below the fast one.
	on := NewScorer(ModeLatencyAware)
	fastScore, slowScore := on.Score(fast, signals[fast]), on.Score(slow, signals[slow])
	if fastScore <= slowScore {
		t.Fatalf("latency-aware ON: Score(fast) = %v, Score(slow) = %v, want fast strictly higher", fastScore, slowScore)
	}
	if got := Rank([]Candidate{slow, fast}, signals, ModeLatencyAware); got[0] != fast {
		t.Fatalf("latency-aware ON: Rank first = %v, want fast", got[0])
	}
	// The p2c term is non-zero ON, so a two-choice draw also picks fast.
	if got := P2CLatencyTerm(signals[fast], signals[slow], Modes[ModeLatencyAware]); got <= 0 {
		t.Fatalf("latency-aware ON: P2CLatencyTerm = %v, want > 0", got)
	}

	// OFF: zero latency weight, every other weight kept. The two
	// candidates now score exactly equal (identical non-latency
	// signals), so the stable rank keeps input order — latency
	// moves nothing at all.
	offWeights := Weights{Latency: 0, Success: 0.25, Cost: 0.25, Quality: 0.25, Quota: 0.25}
	off := NewScorerWithWeights(offWeights)
	if off.Score(fast, signals[fast]) != off.Score(slow, signals[slow]) {
		t.Fatalf("latency-aware OFF: Score(fast) = %v, Score(slow) = %v, want exactly equal",
			off.Score(fast, signals[fast]), off.Score(slow, signals[slow]))
	}
	if got := RankWithWeights([]Candidate{slow, fast}, signals, offWeights); got[0] != slow {
		t.Fatalf("latency-aware OFF: Rank first = %v, want slow (input order kept, scores equal)", got[0])
	}
	if got := P2CLatencyTerm(signals[fast], signals[slow], offWeights); got != 0 {
		t.Fatalf("latency-aware OFF: P2CLatencyTerm = %v, want 0", got)
	}
}
