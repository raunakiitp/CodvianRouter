// SLA-aware strategy hook tests (M3 Area 1, PARITY row 394) +
// nadir chain-hook identity (row 396).
package strategy

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

func TestSLAAwareHookOrdersCompliantFirst(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	ranked := factory("sla-aware")(conformanceChain())
	// a: p95 100ms / 10% errors / $10 — compliant on latency,
	//      violates the 5% error cap (score 0.5 on errors).
	// b: p95 50ms / 5% errors / $30 — compliant everywhere,
	//      priciest (cost score 0).
	// c: p95 200ms / 20% errors / $1 — violates the error cap
	//      hardest, cheapest. Scores: b 0.875, a 0.767, c 0.709.
	if got := conformanceOrder(t, ranked); got != "bac" {
		t.Errorf("sla-aware order = %q, want bac", got)
	}
}

func TestSLAAwareAlias(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	if got := conformanceOrder(t, factory("sla")(conformanceChain())); got != "bac" {
		t.Errorf("sla alias order = %q, want bac (same as sla-aware)", got)
	}
}

func TestSLAAwareHookHardConstraints(t *testing.T) {
	// x violates the error cap (6% errors) but is free and fast,
	// so it wins on soft score; y meets every SLO but is the
	// priciest, so it wins only when hard constraints sort by
	// violation first.
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-x":
			return scoring.Signals{LatencyMs: 100, SuccessRate: 0.94, CostUSDPer1M: 0, Attempts: 10}
		case "prov-y":
			return scoring.Signals{LatencyMs: 1500, SuccessRate: 0.96, CostUSDPer1M: 30, Attempts: 10}
		}
		return scoring.Signals{}
	}
	chain := []router.Attempt{
		{ProviderID: "prov-x", Model: "model-x"},
		{ProviderID: "prov-y", Model: "model-x"},
	}
	soft := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)("sla-aware", router.StrategyHookContext{})
	if got := soft(chain); got[0].ProviderID != "prov-x" {
		t.Errorf("soft sla-aware first = %q, want prov-x (higher soft score)", got[0].ProviderID)
	}
	hard := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)("sla-aware", router.StrategyHookContext{SLAHardConstraints: true})
	if got := hard(chain); got[0].ProviderID != "prov-y" {
		t.Errorf("hard sla-aware first = %q, want prov-y (zero violations)", got[0].ProviderID)
	}
}

func TestSLAAwareHookUnknownSignalsKeepDeclaredOrder(t *testing.T) {
	// No measurements anywhere: every candidate scores neutral
	// 0.5, so the stable sort keeps declared order — unknown is
	// never ranked above (or below) anything on no evidence.
	factory := HookFactory(func(providerID, model string) scoring.Signals {
		return scoring.Signals{CostUSDPer1M: -1}
	}, scoring.Weights{})
	if got := conformanceOrder(t, factory("sla-aware")(conformanceChain())); got != "abc" {
		t.Errorf("sla-aware with no signals = %q, want declared order abc", got)
	}
}

func TestSLAAwareHookFiltersBreakerOpenCandidates(t *testing.T) {
	// the reference router's rule: OPEN-breaker candidates are removed
	// before scoring; only if every candidate is OPEN is the
	// full pool scored. prov-a would win on every signal
	// (fastest, cleanest, cheapest) but its breaker is OPEN,
	// so it must sort last — never first — while the closed
	// candidates keep their scored order. Toggle-verified:
	// without the filter prov-a ranks first and this fails.
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-a":
			return scoring.Signals{LatencyMs: 50, SuccessRate: 1, CostUSDPer1M: 1, Attempts: 10, BreakerOpen: true}
		case "prov-b":
			return scoring.Signals{LatencyMs: 100, SuccessRate: 0.95, CostUSDPer1M: 10, Attempts: 10}
		case "prov-c":
			return scoring.Signals{LatencyMs: 200, SuccessRate: 0.9, CostUSDPer1M: 5, Attempts: 10}
		}
		return scoring.Signals{}
	}
	chain := []router.Attempt{
		{ProviderID: "prov-a", Model: "model-x"},
		{ProviderID: "prov-b", Model: "model-x"},
		{ProviderID: "prov-c", Model: "model-x"},
	}
	hook := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)("sla-aware", router.StrategyHookContext{})
	ranked := hook(chain)
	if got := ranked[0].ProviderID; got != "prov-b" {
		t.Errorf("sla-aware with prov-a OPEN: first = %q, want prov-b", got)
	}
	if got := ranked[2].ProviderID; got != "prov-a" {
		t.Errorf("sla-aware with prov-a OPEN: last = %q, want prov-a (OPEN sorts last, never dropped)", got)
	}
	// All OPEN: the full pool is scored (the reference router's
	// fallback), so the best candidate still serves first.
	allOpen := func(providerID, model string) scoring.Signals {
		s := signals(providerID, model)
		s.BreakerOpen = true
		return s
	}
	hook = HookFactoryWithCacheAffinity(allOpen, scoring.Weights{}, nil)("sla-aware", router.StrategyHookContext{})
	ranked = hook(chain)
	if got := ranked[0].ProviderID; got != "prov-a" {
		t.Errorf("sla-aware with all OPEN: first = %q, want prov-a (full pool scored)", got)
	}
}

func TestSLAAwareHookZeroToleranceHonoured(t *testing.T) {
	// prov-x has 4% errors but is fast and free, so under the
	// default 5% cap with hard constraints it beats the clean
	// but slow and pricey prov-y (both zero violations; x wins
	// on score). Under an explicitly configured 0 error rate
	// (zero-tolerance) x violates and y wins on violations.
	// A 0 rate WITHOUT the Set flag still means "default"
	// (context-free callers), so x must still win there.
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-x":
			return scoring.Signals{LatencyMs: 100, SuccessRate: 0.96, CostUSDPer1M: 0, Attempts: 10}
		case "prov-y":
			return scoring.Signals{LatencyMs: 1500, SuccessRate: 1, CostUSDPer1M: 30, Attempts: 10}
		}
		return scoring.Signals{}
	}
	chain := []router.Attempt{
		{ProviderID: "prov-x", Model: "model-x"},
		{ProviderID: "prov-y", Model: "model-x"},
	}
	factory := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)
	def := factory("sla-aware", router.StrategyHookContext{SLAHardConstraints: true})(chain)
	if got := def[0].ProviderID; got != "prov-x" {
		t.Errorf("default 5%% cap, hard constraints: first = %q, want prov-x", got)
	}
	zero := factory("sla-aware", router.StrategyHookContext{SLAMaxErrorRate: 0, SLAMaxErrorRateSet: true, SLAHardConstraints: true})(chain)
	if got := zero[0].ProviderID; got != "prov-y" {
		t.Errorf("zero-tolerance (explicit 0), hard constraints: first = %q, want prov-y", got)
	}
	unset := factory("sla-aware", router.StrategyHookContext{SLAMaxErrorRate: 0, SLAHardConstraints: true})(chain)
	if got := unset[0].ProviderID; got != "prov-x" {
		t.Errorf("unset 0 rate (no Set flag), hard constraints: first = %q, want prov-x (default 5%% cap)", got)
	}
}

func TestDescribeSLAAwareDisclosesP50StandIn(t *testing.T) {
	// The review corrected the design's premise: Describe
	// did NOT disclose the p50 stand-in. It must now say
	// the latency is the measured p50 EWMA, not a true p95,
	// and that stability is neutral (no stddev signal).
	_, rule := Describe(StrategySLAAware)
	for _, want := range []string{"p50", "not a true p95", "neutral"} {
		if !strings.Contains(rule, want) {
			t.Errorf("Describe(sla-aware) = %q, want it to disclose %q", rule, want)
		}
	}
}

func TestNadirHookIsIdentity(t *testing.T) {
	// Nadir is a model-selection strategy, not an ordering: the
	// chain hook must leave the declared (priority) order
	// untouched and make no decision.
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	if got := conformanceOrder(t, factory("nadir")(conformanceChain())); got != "abc" {
		t.Errorf("nadir hook order = %q, want declared order abc", got)
	}
	if st, ok := Parse("nadir"); !ok || st != StrategyNadir {
		t.Errorf("Parse(nadir) = %q, %v, want nadir, true", st, ok)
	}
}
