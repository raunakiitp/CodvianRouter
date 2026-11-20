package strategy

import (
	"math/rand"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// G12 Area 12 row 9: the G10 p2c latency term wired into the live
// p2c path — OrderP2C (called by the "p2c" hook case in wiring.go)
// now adds scoring.P2CLatencyTerm to the two-choice comparison.
// Hand-computed expectations use the documented normalisations:
// latencyComponent = 1/(1+ms/1000), costComponent = 1/(1+$/10),
// balanced Score weights 0.25/0.25/0.20/0.20/0.10, and the term
// under ModeLatencyAware (latency weight 0.70).

// TestOrderingP2CLatencyBreaksScoreTie: with equal Scores the
// lower-latency candidate must win the draw — the term, not the
// Score, decides. With exactly two targets both are always drawn,
// so the result is seed-independent; both input orders are tried.
func TestOrderingP2CLatencyBreaksScoreTie(t *testing.T) {
	// Term: 0.70 * (1/1.1 - 1/1.9) = 0.70 * 0.3828 = 0.2679.
	in := []OrderingTarget{
		{Provider: "slow", Model: "m1", Score: 0.5, LatencyMs: 900},
		{Provider: "fast", Model: "m2", Score: 0.5, LatencyMs: 100},
	}
	rev := []OrderingTarget{in[1], in[0]}
	for _, seed := range []int64{1, 2, 3, 7, 42} {
		for _, list := range [][]OrderingTarget{in, rev} {
			got := OrderP2C(list, rand.New(rand.NewSource(seed)))
			if len(got) != 2 || got[0].Key() != "fast/m2" || got[1].Key() != "slow/m1" {
				t.Fatalf("seed %d, input %v: OrderP2C = %v, want [fast/m2 slow/m1]", seed, orderingKeys(list), orderingKeys(got))
			}
		}
	}
}

// TestP2CHookPrefersLowerLatencyOverModerateScoreGap: the live
// "p2c" hook (factory -> toOrderingTargets -> OrderP2C) flips an
// order the old score-only draw got the other way. Balanced
// Scores: fast (100ms/$30) 0.7423, slow (900ms/$1) 0.7784 —
// slow leads on Score by 0.0361. The latency term is
// 0.70 * (1/1.1 - 1/1.9) = 0.2679, 7x the Score gap, so fast
// must lead. Under the old score-only OrderP2C this test fails
// (slow first) — it is the proof the term is live in production.
func TestP2CHookPrefersLowerLatencyOverModerateScoreGap(t *testing.T) {
	signals := map[string]scoring.Signals{
		"fast": {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 30, QualityScore: 0.7, QuotaHint: 100},
		"slow": {LatencyMs: 900, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
	}
	chain := []router.Attempt{{ProviderID: "slow", Model: "m"}, {ProviderID: "fast", Model: "m"}}
	factory := HookFactory(func(id, model string) scoring.Signals { return signals[id] }, scoring.Weights{})
	ranked := factory("p2c")(chain)
	if len(ranked) != 2 {
		t.Fatalf("p2c ranked %d attempts, want 2", len(ranked))
	}
	if ranked[0].ProviderID != "fast" {
		t.Errorf("p2c first = %q, want fast (latency term 0.2679 beats Score gap 0.0361); order = %v", ranked[0].ProviderID, ranked)
	}
}

// TestP2CHookNoiseGapLosesToLargeCostGap: same numbers as G10's
// scoring TestLatencyAwareNoiseGapLosesToLargeCostGap, driven
// through the live "p2c" hook. A 100ms vs 105ms gap is worth
// 0.70 * (1/1.100 - 1/1.105) = 0.0029 weighted, while the
// $100/1M vs $1/1M cost gap moves the balanced Score by
// 0.20 * (1/1.1 - 1/11) = +0.1636 cost, −0.0010 latency, net +0.1626 — ~56x larger — so the
// slow-but-cheap target must lead. (This also passes under the
// old score-only draw; it guards the noise rule, it does not by
// itself prove the term is wired —
// TestP2CHookPrefersLowerLatencyOverModerateScoreGap does that.)
func TestP2CHookNoiseGapLosesToLargeCostGap(t *testing.T) {
	signals := map[string]scoring.Signals{
		"slow-cheap": {LatencyMs: 105, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
		"fast-exp":   {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 100, QualityScore: 0.7, QuotaHint: 100},
	}
	chain := []router.Attempt{{ProviderID: "slow-cheap", Model: "m"}, {ProviderID: "fast-exp", Model: "m"}}
	factory := HookFactory(func(id, model string) scoring.Signals { return signals[id] }, scoring.Weights{})
	ranked := factory("p2c")(chain)
	if len(ranked) != 2 {
		t.Fatalf("p2c ranked %d attempts, want 2", len(ranked))
	}
	if ranked[0].ProviderID != "slow-cheap" {
		t.Errorf("p2c first = %q, want slow-cheap (5ms gap is noise vs $99 cost gap); order = %v", ranked[0].ProviderID, ranked)
	}
}
