package strategy

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// threeProviders differ on every axis, so each strategy's weights
// produce a distinct expected winner (hand-computed from the
// documented normalisations in internal/scoring):
//
//	cheap:  800ms, 90% success, $1/1M,  quota 100
//	fast:   100ms, 90% success, $30/1M, quota 100
//	steady: 500ms, 99% success, $5/1M,  quota 100
//
// cheapest -> cheap ($1 beats $5 beats $30 despite slower latency)
// fastest  -> fast (100ms beats 500ms beats 800ms)
// most-reliable -> steady (99% success beats 90%)
// least-latency -> fast (100ms is the lowest latency, full stop)
var strategySignals = map[string]scoring.Signals{
	"cheap":  {LatencyMs: 800, SuccessRate: 0.90, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
	"fast":   {LatencyMs: 100, SuccessRate: 0.90, CostUSDPer1M: 30, QualityScore: 0.7, QuotaHint: 100},
	"steady": {LatencyMs: 500, SuccessRate: 0.99, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
}

func strategyChain() []router.Attempt {
	return []router.Attempt{
		{ProviderID: "cheap", Model: "m"},
		{ProviderID: "fast", Model: "m"},
		{ProviderID: "steady", Model: "m"},
	}
}

func strategySignalsFn(id, model string) scoring.Signals { return strategySignals[id] }

func TestParse(t *testing.T) {
	cases := map[string]Strategy{
		"cheapest": "cheapest", "Cheapest": "cheapest", " cheap ": "cheapest",
		"fastest": "fastest", "fast": "fastest",
		"most-reliable": "most-reliable", "reliable": "most-reliable",
		"least-latency": "least-latency", "latency": "least-latency",
		"balanced": "balanced", "weighted": "weighted",
		"custom-weights": "custom-weights", "custom": "custom-weights",
		"code": "code", "priority": "priority", "Priority": "priority",
		"round-robin": "round-robin", "eco": "cost-optimized",
	}
	for in, want := range cases {
		got, ok := Parse(in)
		if !ok || got != want {
			t.Errorf("Parse(%q) = %q, %v (want %q, true)", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "  ", "smartest", "auto"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) unexpectedly valid", bad)
		}
	}
}

func TestEachStrategyPicksExpectedProvider(t *testing.T) {
	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	cases := map[string]string{
		"cheapest":      "cheap",
		"fastest":       "fast",
		"most-reliable": "steady",
		"least-latency": "fast",
		// priority keeps declared order: cheap is first in the
		// chain, so it stays first even though it is not the
		// fastest/cheapest winner under scoring.
		"priority": "cheap",
	}
	for name, wantFirst := range cases {
		hook := factory(name)
		ranked := hook(strategyChain())
		if len(ranked) != 3 {
			t.Fatalf("%s: ranked %d attempts (want 3)", name, len(ranked))
		}
		if ranked[0].ProviderID != wantFirst {
			t.Errorf("%s: first = %q (want %q); order = %v", name, ranked[0].ProviderID, wantFirst, ranked)
		}
	}
}

func TestLeastLatencyIgnoresCost(t *testing.T) {
	// Even when the slowest provider is free and the fastest is
	// the most expensive, least-latency still picks the fastest:
	// cost never moves its order.
	signals := map[string]scoring.Signals{
		"slow-free": {LatencyMs: 2000, SuccessRate: 0.99, CostUSDPer1M: 0, QualityScore: 0.9, QuotaHint: 100},
		"fast-exp":  {LatencyMs: 50, SuccessRate: 0.99, CostUSDPer1M: 100, QualityScore: 0.9, QuotaHint: 100},
	}
	factory := HookFactory(func(id, model string) scoring.Signals { return signals[id] }, scoring.Weights{})
	ranked := factory("least-latency")([]router.Attempt{
		{ProviderID: "slow-free", Model: "m"},
		{ProviderID: "fast-exp", Model: "m"},
	})
	if ranked[0].ProviderID != "fast-exp" {
		t.Errorf("least-latency first = %q (want fast-exp)", ranked[0].ProviderID)
	}
	// ... while cheapest picks the free one on the same signals.
	ranked = factory("cheapest")([]router.Attempt{
		{ProviderID: "slow-free", Model: "m"},
		{ProviderID: "fast-exp", Model: "m"},
	})
	if ranked[0].ProviderID != "slow-free" {
		t.Errorf("cheapest first = %q (want slow-free)", ranked[0].ProviderID)
	}
}

func TestCustomWeightsUsesCustomWeights(t *testing.T) {
	// Custom weights: cost only. The cheapest provider wins even
	// though it is the slowest and least reliable.
	custom := scoring.Weights{Cost: 1}
	factory := HookFactory(strategySignalsFn, custom)
	ranked := factory("custom-weights")(strategyChain())
	if ranked[0].ProviderID != "cheap" {
		t.Errorf("custom-weights(cost-only) first = %q (want cheap)", ranked[0].ProviderID)
	}
	// Custom weights: latency only. The fastest wins.
	factory = HookFactory(strategySignalsFn, scoring.Weights{Latency: 1})
	ranked = factory("custom-weights")(strategyChain())
	if ranked[0].ProviderID != "fast" {
		t.Errorf("custom-weights(latency-only) first = %q (want fast)", ranked[0].ProviderID)
	}
	// Invalid custom weights (zero, negative) fall back to
	// balanced instead of producing a broken ranking.
	for _, bad := range []scoring.Weights{{}, {Cost: -1}, {Latency: 1, Cost: -1}} {
		factory = HookFactory(strategySignalsFn, bad)
		ranked = factory("custom-weights")(strategyChain())
		if len(ranked) != 3 {
			t.Errorf("custom-weights(%v): ranked %d (want 3, a permutation)", bad, len(ranked))
		}
	}
}

func TestUnknownStrategyFallsBackToPriority(t *testing.T) {
	// M1.11: an unknown strategy name falls back to priority
	// (declared order, no re-ranking), not to balanced.
	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	in := strategyChain()
	got := factory("no-such-strategy")(in)
	if len(got) != len(in) {
		t.Fatalf("unknown-strategy ranked %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("unknown strategy order %v != declared order %v (want priority fallback)", got, in)
		}
	}
}

func TestHookNeverDropsOrDuplicates(t *testing.T) {
	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	for _, name := range All {
		ranked := factory(string(name))(strategyChain())
		if len(ranked) != 3 {
			t.Errorf("%s: ranked %d (want 3)", name, len(ranked))
		}
		seen := map[string]bool{}
		for _, att := range ranked {
			if seen[att.ProviderID] {
				t.Errorf("%s: duplicate %q", name, att.ProviderID)
			}
			seen[att.ProviderID] = true
		}
	}
}

func TestHookFactoryNilSignalsIsIdentity(t *testing.T) {
	factory := HookFactory(nil, scoring.Weights{})
	in := strategyChain()
	out := factory("cheapest")(in)
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("nil signals: hook reordered the chain (%v -> %v)", in, out)
		}
	}
}

func TestAutoAxis(t *testing.T) {
	cases := map[string]Strategy{
		"auto:cheapest":       StrategyCheapest,
		"auto:fastest":        StrategyFastest,
		"auto:most-reliable":  StrategyMostReliable,
		"auto:reliable":       StrategyMostReliable,
		"auto:least-latency":  StrategyLeastLatency,
		"auto:balanced":       StrategyBalanced,
		"auto:weighted":       StrategyWeighted,
		"auto:custom-weights": StrategyCustomWeights,
		"auto:code":           StrategyCode,
		"AUTO:Cheapest":       StrategyCheapest,
	}
	for model, want := range cases {
		got, ok := AutoAxis(model)
		if !ok || got != want {
			t.Errorf("AutoAxis(%q) = %q, %v (want %q, true)", model, got, ok, want)
		}
	}
	for _, notAlias := range []string{"auto", "auto/fast", "gpt-4o", "auto:", ""} {
		if _, ok := AutoAxis(notAlias); ok {
			t.Errorf("AutoAxis(%q) unexpectedly parsed as an alias", notAlias)
		}
	}
}

func TestAxisVirtualModel(t *testing.T) {
	cases := map[Strategy]string{
		StrategyCheapest:     "auto/cheap",
		StrategyFastest:      "auto/fast",
		StrategyLeastLatency: "auto/fast",
		StrategyMostReliable: "auto/reasoning",
		StrategyBalanced:     "auto",
		StrategyWeighted:     "auto",
		StrategyCode:         "auto/code",
	}
	for axis, want := range cases {
		if got := AxisVirtualModel(axis); got != want {
			t.Errorf("AxisVirtualModel(%q) = %q (want %q)", axis, got, want)
		}
	}
}

func TestWeightsFor(t *testing.T) {
	// Every strategy's weights sum to 1.
	for _, st := range All {
		w := WeightsFor(st, scoring.Weights{})
		sum := w.Latency + w.Success + w.Cost + w.Quality + w.Quota
		if sum < 0.999 || sum > 1.001 {
			t.Errorf("WeightsFor(%q) sums to %v (want 1)", st, sum)
		}
	}
	// Custom weighted weights are normalised.
	w := WeightsFor(StrategyCustomWeights, scoring.Weights{Latency: 2, Cost: 2})
	if w.Latency != 0.5 || w.Cost != 0.5 {
		t.Errorf("custom-weights normalisation = %+v (want latency .5, cost .5)", w)
	}
}

func TestDescribeCoversAll(t *testing.T) {
	for _, st := range All {
		name, rule := Describe(st)
		if name == "" || rule == "" {
			t.Errorf("Describe(%q) returned empty name/rule", st)
		}
	}
}

func TestCheapestSameModelDifferentProvider(t *testing.T) {
	// The cheapest strategy must rank on the (provider, model)
	// pair price: the same model at a cheap provider beats the
	// same model at an expensive provider, and an unknown pair is
	// never treated as free (a genuinely free pair still wins).
	signals := func(id, model string) scoring.Signals {
		base := scoring.Signals{LatencyMs: 100, SuccessRate: 0.9, QualityScore: 0.7, QuotaHint: 100}
		switch id {
		case "cheap-provider":
			base.CostUSDPer1M = 1
		case "expensive-provider":
			base.CostUSDPer1M = 60
		case "unknown-provider":
			base.CostUSDPer1M = -1 // unknown: neutral, never $0
		case "free-provider":
			base.CostUSDPer1M = 0 // genuinely free
		}
		return base
	}
	factory := HookFactory(signals, scoring.Weights{})
	chain := []router.Attempt{
		{ProviderID: "expensive-provider", Model: "shared-model"},
		{ProviderID: "unknown-provider", Model: "shared-model"},
		{ProviderID: "cheap-provider", Model: "shared-model"},
	}
	ranked := factory("cheapest")(chain)
	if len(ranked) != 3 {
		t.Fatalf("cheapest ranked %d attempts, want 3", len(ranked))
	}
	if ranked[0].ProviderID != "cheap-provider" {
		t.Errorf("cheapest first = %q (want cheap-provider: $1 pair beats $60 pair)", ranked[0].ProviderID)
	}
	if ranked[1].ProviderID != "unknown-provider" || ranked[2].ProviderID != "expensive-provider" {
		t.Errorf("cheapest order = %v (want cheap, unknown, expensive: unknown is neutral, never cheapest)", ranked)
	}
	// Free still beats unknown: unknown is not $0.
	free := factory("cheapest")([]router.Attempt{
		{ProviderID: "unknown-provider", Model: "shared-model"},
		{ProviderID: "free-provider", Model: "shared-model"},
	})
	if free[0].ProviderID != "free-provider" {
		t.Errorf("cheapest free-vs-unknown first = %q (want free-provider: unknown must never rank as free)", free[0].ProviderID)
	}
}

// M1.11 acceptance: priority is the default, so it leads the
// declared order; code stays declared alongside it.
func TestAllStartsWithPriorityAndContainsCode(t *testing.T) {
	if len(All) == 0 || All[0] != StrategyPriority {
		t.Fatalf("All[0] = %v; want priority first (the default)", All)
	}
	found := map[Strategy]bool{}
	for _, st := range All {
		found[st] = true
	}
	for _, want := range []Strategy{StrategyPriority, StrategyBalanced, StrategyCheapest, StrategyFastest, StrategyMostReliable, StrategyLeastLatency, StrategyWeighted, StrategyCode} {
		if !found[want] {
			t.Errorf("All is missing %q", want)
		}
	}
}

// differentiatingSignals makes provider "a" slow and "b" fast at
// equal cost/success, so any scoring strategy reorders [a, b] to
// [b, a] while priority (declared order) must keep [a, b].
func differentiatingSignals(providerID, model string) scoring.Signals {
	if providerID == "a" {
		return scoring.Signals{LatencyMs: 5000, SuccessRate: 1, CostUSDPer1M: 1, QualityScore: 1}
	}
	return scoring.Signals{LatencyMs: 10, SuccessRate: 1, CostUSDPer1M: 1, QualityScore: 1}
}

func attemptOrder(chain []router.Attempt) []string {
	out := make([]string, len(chain))
	for i, att := range chain {
		out[i] = att.ProviderID
	}
	return out
}

// M1.11 acceptance: the default strategy routes in declared order
// even when live signals would re-rank under balanced.
func TestPriorityHookPreservesDeclaredOrder(t *testing.T) {
	factory := HookFactory(differentiatingSignals, scoring.Weights{})
	chain := []router.Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	if got := attemptOrder(factory("priority")(chain)); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("priority hook ordered %v; want [a b] (declared order, no re-ranking)", got)
	}
	if got := attemptOrder(factory("balanced")(chain)); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("balanced hook ordered %v; want [b a] (control: signals do re-rank under balanced)", got)
	}
}

// M1.10 acceptance: "code" is a real declared strategy whose
// chain-order hook is balanced on purpose (cost-per-solved-task
// ranking happens at model-resolution time, not here).
func TestCodeHookIsBalanced(t *testing.T) {
	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	a := factory("code")(strategyChain())
	b := factory("balanced")(strategyChain())
	if len(a) != len(b) {
		t.Fatalf("code ranked %d, balanced %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("code order %v != balanced order %v", a, b)
		}
	}
	if got := WeightsFor(StrategyCode, scoring.Weights{}); got != scoring.Modes[scoring.ModeBalanced] {
		t.Fatalf("WeightsFor(code) = %+v; want balanced weights", got)
	}
	if name, _ := Describe(StrategyCode); name == "" {
		t.Fatal("Describe(code) returned an empty name")
	}
}

// M1.11 acceptance: priority is describable and its auto alias
// resolves to the plain auto model.
func TestDescribePriorityAndAutoAxis(t *testing.T) {
	if name, rule := Describe(StrategyPriority); name == "" || rule == "" {
		t.Fatalf("Describe(priority) = %q, %q; want a name and a rule", name, rule)
	}
	if st, ok := AutoAxis("auto:priority"); !ok || st != StrategyPriority {
		t.Fatalf("AutoAxis(auto:priority) = %q, %v; want priority, true", st, ok)
	}
	if got := AxisVirtualModel(StrategyPriority); got != "auto" {
		t.Fatalf("AxisVirtualModel(priority) = %q; want auto", got)
	}
}
