package strategy

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// G10 Area 12 row 9: the "latency-aware" strategy — lowest latency
// among viable targets (see wiringHook). Uses the same
// three-provider shape as strategy_test.go, plus targeted cases.

func TestLatencyAwareParsesAndDescribes(t *testing.T) {
	if st, ok := Parse("latency-aware"); !ok || st != StrategyLatencyAware {
		t.Errorf("Parse(latency-aware) = %q, %v, want latency-aware, true", st, ok)
	}
	if !IsValid("latency-aware") {
		t.Error("IsValid(latency-aware) = false, want true")
	}
	name, rule := Describe(StrategyLatencyAware)
	if name == "" || rule == "" {
		t.Errorf("Describe(latency-aware) = %q, %q, want non-empty", name, rule)
	}
	if got := WeightsFor(StrategyLatencyAware, scoring.Weights{}); got != scoring.Modes[scoring.ModeLatencyAware] {
		t.Errorf("WeightsFor(latency-aware) = %+v, want ModeLatencyAware %+v", got, scoring.Modes[scoring.ModeLatencyAware])
	}
	if got := AxisVirtualModel(StrategyLatencyAware); got != "auto/fast" {
		t.Errorf("AxisVirtualModel(latency-aware) = %q, want auto/fast", got)
	}
}

func TestLatencyAwarePicksLowerLatencyAtEqualCost(t *testing.T) {
	// Same fixture shape as TestEachStrategyPicksExpectedProvider:
	// cheap 800ms/$1, fast 100ms/$30, steady 500ms/$5. At equal
	// cost the fast one must win; on the shared fixture the
	// latency-aware winner is fast (100ms) even though it is the
	// most expensive — latency dominates, and all three are viable
	// (no Attempts recorded, so unknown fails open).
	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	ranked := factory("latency-aware")(strategyChain())
	if len(ranked) != 3 {
		t.Fatalf("ranked %d attempts, want 3", len(ranked))
	}
	if ranked[0].ProviderID != "fast" {
		t.Errorf("latency-aware first = %q, want fast; order = %v", ranked[0].ProviderID, ranked)
	}
}

func TestLatencyAwareNoiseGapLosesToLargeCostGap(t *testing.T) {
	// Noise threshold, documented in scoring
	// TestLatencyAwareNoiseGapLosesToLargeCostGap: a 100ms vs 105ms
	// gap is worth 0.0029 weighted, a $100 vs $1 gap is worth
	// 0.0409 — so the slow-but-cheap target must lead, while
	// least-latency on the same signals still picks the fast one.
	signals := map[string]scoring.Signals{
		"slow-cheap": {LatencyMs: 105, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
		"fast-exp":   {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 100, QualityScore: 0.7, QuotaHint: 100},
	}
	chain := []router.Attempt{{ProviderID: "slow-cheap", Model: "m"}, {ProviderID: "fast-exp", Model: "m"}}
	factory := HookFactory(func(id, model string) scoring.Signals { return signals[id] }, scoring.Weights{})
	if got := factory("latency-aware")(chain)[0].ProviderID; got != "slow-cheap" {
		t.Errorf("latency-aware first = %q, want slow-cheap (5ms gap is noise vs $99 cost gap)", got)
	}
	if got := factory("least-latency")(chain)[0].ProviderID; got != "fast-exp" {
		t.Errorf("least-latency first = %q, want fast-exp (cost never moves it)", got)
	}
}

func TestLatencyAwareDemotesMostlyFailingFastTarget(t *testing.T) {
	// Viability rule: Attempts > 0 with SuccessRate < 0.5 means
	// "measured mostly-failing" — demoted behind every viable
	// target however fast it is. Attempts <= 0 (unknown) fails
	// open and stays viable.
	signals := map[string]scoring.Signals{
		"fast-dead": {LatencyMs: 50, SuccessRate: 0.1, CostUSDPer1M: 1, QualityScore: 0.1, QuotaHint: 100, Attempts: 10},
		"slow-ok":   {LatencyMs: 900, SuccessRate: 0.95, CostUSDPer1M: 1, QualityScore: 0.9, QuotaHint: 100, Attempts: 10},
		"unknown":   {LatencyMs: 60, SuccessRate: 0, CostUSDPer1M: 1, QualityScore: 0, QuotaHint: 100, Attempts: 0},
	}
	chain := []router.Attempt{
		{ProviderID: "fast-dead", Model: "m"},
		{ProviderID: "slow-ok", Model: "m"},
		{ProviderID: "unknown", Model: "m"},
	}
	factory := HookFactory(func(id, model string) scoring.Signals { return signals[id] }, scoring.Weights{})
	ranked := factory("latency-aware")(chain)
	if len(ranked) != 3 {
		t.Fatalf("ranked %d attempts, want 3", len(ranked))
	}
	// Unknown (60ms, viable) leads; slow-ok (viable) next;
	// fast-dead (non-viable) last despite being fastest.
	if ranked[0].ProviderID != "unknown" || ranked[1].ProviderID != "slow-ok" || ranked[2].ProviderID != "fast-dead" {
		t.Errorf("latency-aware order = %v, want [unknown slow-ok fast-dead]", ranked)
	}
}
