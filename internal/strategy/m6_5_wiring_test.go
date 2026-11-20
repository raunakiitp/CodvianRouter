// M6.5-wiring tests: the cache-optimized strategy is wired into
// the live routing path — the per-request hook factory
// (HookFactoryWithCacheAffinity) threads the request's
// cacheability gate and the ledger's measured per-pair warmth
// into wiringHook's StrategyCacheOptimized case, which orders
// with OrderByCacheAffinity and maps back with affinityKeys.
//
// Every test here FAILS without the wiring: with the old
// identity case (Warmth 0 for every target, no gate) the reorder
// tests return declared order "abc" and the gate tests pass
// vacuously — the toggle evidence is recorded in the M6.5-wiring
// report (revert the StrategyCacheOptimized case to the old
// identity body and TestM65CacheOptimizedReordersOnWarmth /
// TestM65CacheOptimizedWarmthBeatsMarkerSupport fail).
package strategy

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// m65Warmth builds a CacheWarmthFunc from a provider/model ->
// fraction map. A pair absent from the map has no measurement
// (ok=false); a pair present with 0 is measured cold.
func m65Warmth(m map[string]float64) CacheWarmthFunc {
	return func(provider, model string) (float64, bool) {
		f, ok := m[provider+"/"+model]
		return f, ok
	}
}

// m65CacheableCtx is a cacheable request context: a 2,000-token
// prompt, no tools, deterministic temperature.
func m65CacheableCtx() router.StrategyHookContext {
	return router.StrategyHookContext{PromptTokens: 2000}
}

func TestM65CacheOptimizedReordersOnWarmth(t *testing.T) {
	// Warmth differs across the fixture: c is warmest (0.9), a
	// lukewarm (0.1), b measured cold (0). The hook must reorder
	// warmest-first: cab, not declared abc.
	factory := HookFactoryWithCacheAffinity(conformanceSignals, scoring.Weights{}, m65Warmth(map[string]float64{
		"prov-a/model-x": 0.1,
		"prov-b/model-x": 0,
		"prov-c/model-x": 0.9,
	}))
	hook := factory("cache-optimized", m65CacheableCtx())
	if got := conformanceOrder(t, hook(conformanceChain())); got != "cab" {
		t.Errorf("cache-optimized with warmth c=0.9 a=0.1 b=0 = %q, want cab", got)
	}
}

func TestM65CacheOptimizedWarmthBeatsMarkerSupport(t *testing.T) {
	// Warmth DOMINATES marker support: anthropic is
	// marker-supporting but measured cold (0), openai is not
	// marker-supporting but measured warm (0.8) — the warm
	// target must lead. Scores are 0 for both (zero signals),
	// so only warmth can decide.
	chain := []router.Attempt{
		{ProviderID: "anthropic", Model: "model-x"},
		{ProviderID: "openai", Model: "model-x"},
	}
	factory := HookFactoryWithCacheAffinity(
		func(providerID, model string) scoring.Signals { return scoring.Signals{} },
		scoring.Weights{},
		m65Warmth(map[string]float64{
			"anthropic/model-x": 0,
			"openai/model-x":    0.8,
		}),
	)
	ranked := factory("cache-optimized", m65CacheableCtx())(chain)
	if len(ranked) != 2 {
		t.Fatalf("ranked %d attempts, want 2", len(ranked))
	}
	if ranked[0].ProviderID != "openai" {
		t.Errorf("first = %q, want openai (warmth 0.8 beats anthropic marker support at warmth 0)", ranked[0].ProviderID)
	}
	if ranked[1].ProviderID != "anthropic" {
		t.Errorf("second = %q, want anthropic", ranked[1].ProviderID)
	}
}

func TestM65CacheOptimizedGateBlocks(t *testing.T) {
	// The cacheability gate blocks wiring even when warmth is
	// strongly differentiated (c 0.9): every non-cacheable
	// request keeps declared order. A temperature of exactly 0
	// (explicitly set) is deterministic and passes the gate —
	// the positive control that the gate is about sampling, not
	// about the temperature field being present.
	warmth := m65Warmth(map[string]float64{
		"prov-a/model-x": 0.1,
		"prov-b/model-x": 0,
		"prov-c/model-x": 0.9,
	})
	factory := HookFactoryWithCacheAffinity(conformanceSignals, scoring.Weights{}, warmth)
	cases := []struct {
		name string
		ctx  router.StrategyHookContext
		want string
	}{
		{"short prompt", router.StrategyHookContext{PromptTokens: 512}, "abc"},
		{"at threshold minus one", router.StrategyHookContext{PromptTokens: MinCacheablePromptTokens - 1}, "abc"},
		{"tools present", router.StrategyHookContext{PromptTokens: 2000, HasTools: true}, "abc"},
		{"sampled temperature", router.StrategyHookContext{PromptTokens: 2000, Temperature: 0.7, TemperatureSet: true}, "abc"},
		{"contract pinned", router.StrategyHookContext{PromptTokens: 2000, ContractPinned: true}, "abc"},
		{"temperature zero passes", router.StrategyHookContext{PromptTokens: 2000, Temperature: 0, TemperatureSet: true}, "cab"},
		{"at threshold passes", router.StrategyHookContext{PromptTokens: MinCacheablePromptTokens}, "cab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conformanceOrder(t, factory("cache-optimized", tc.ctx)(conformanceChain())); got != tc.want {
				t.Errorf("cache-optimized (%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestM65CacheOptimizedNoWarmthIsIdentity(t *testing.T) {
	// No ledger data degrades to the old identity order — no
	// regression. Three ways to have no measured warmth: no
	// warmth source at all (nil func), a source that knows no
	// pair, and a source that measured every pair cold (0).
	// In all three the hook must return declared order, even for
	// a cacheable request: affinity has nothing to say, and the
	// wiring must not re-rank on marker support / balanced score
	// alone.
	factory := HookFactoryWithCacheAffinity(conformanceSignals, scoring.Weights{}, nil)
	if got := conformanceOrder(t, factory("cache-optimized", m65CacheableCtx())(conformanceChain())); got != "abc" {
		t.Errorf("cache-optimized with nil warmth = %q, want declared order abc", got)
	}
	factory = HookFactoryWithCacheAffinity(conformanceSignals, scoring.Weights{}, m65Warmth(map[string]float64{}))
	if got := conformanceOrder(t, factory("cache-optimized", m65CacheableCtx())(conformanceChain())); got != "abc" {
		t.Errorf("cache-optimized with unknown warmth = %q, want declared order abc", got)
	}
	factory = HookFactoryWithCacheAffinity(conformanceSignals, scoring.Weights{}, m65Warmth(map[string]float64{
		"prov-a/model-x": 0,
		"prov-b/model-x": 0,
		"prov-c/model-x": 0,
	}))
	if got := conformanceOrder(t, factory("cache-optimized", m65CacheableCtx())(conformanceChain())); got != "abc" {
		t.Errorf("cache-optimized with all-cold warmth = %q, want declared order abc", got)
	}
}

func TestM65CacheOptimizedContextFreeFactoryIsIdentity(t *testing.T) {
	// The context-free HookFactory carries no request context and
	// no warmth source, so cache-optimized still returns declared
	// order under it: the gate has no prompt to measure (0
	// tokens) and no target has measured warmth. This pins the
	// boundary — the wiring only activates through the
	// per-request factory — and keeps the old conformance
	// behaviour for context-free callers.
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	if got := conformanceOrder(t, factory("cache-optimized")(conformanceChain())); got != "abc" {
		t.Errorf("context-free cache-optimized = %q, want declared order abc", got)
	}
}

func TestM65CacheOptimizedNilSignalsIsIdentity(t *testing.T) {
	// Nil signals: identity for every strategy, matching
	// HookFactory's nil rule — even with warmth and a cacheable
	// request, a factory with no signal source must not reorder.
	factory := HookFactoryWithCacheAffinity(nil, scoring.Weights{}, m65Warmth(map[string]float64{
		"prov-c/model-x": 0.9,
	}))
	if got := conformanceOrder(t, factory("cache-optimized", m65CacheableCtx())(conformanceChain())); got != "abc" {
		t.Errorf("cache-optimized with nil signals = %q, want declared order abc", got)
	}
}
