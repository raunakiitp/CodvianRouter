// M3 Phase B conformance suite: every named strategy resolves
// through HookFactory, returns a permutation of the chain (never
// drops or duplicates an attempt), and — where its ordering is
// determined by live signals alone — produces the expected order
// on a fixed fixture. A test here fails if a strategy is removed
// from the registry, unregistered from config, or returns a
// non-permutation. Randomised strategies (random, strict-random,
// p2c, weighted draw) are asserted as permutations only; their
// exact orders are covered by the builders' unit tests.
package strategy

import (
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// conformanceChain is the fixed fixture: three attempts with
// distinct live signals. a: slow-ish, reliable, mid price, quota,
// 5 attempts, light load, reset in 1h, 20% utilised. b: fastest,
// most reliable, priciest, NO quota, 1 attempt, heavy load, reset
// in 5m, 80% utilised. c: slowest, least reliable, cheapest,
// quota, 9 attempts, idle, reset in 2h, 50% utilised.
func conformanceChain() []router.Attempt {
	return []router.Attempt{
		{ProviderID: "prov-a", Model: "model-x"},
		{ProviderID: "prov-b", Model: "model-x"},
		{ProviderID: "prov-c", Model: "model-x"},
	}
}

func conformanceSignals(providerID, model string) scoring.Signals {
	now := time.Now()
	switch providerID {
	case "prov-a":
		return scoring.Signals{LatencyMs: 100, SuccessRate: 0.90, CostUSDPer1M: 10, QualityScore: 0.9, QuotaHint: 5, Attempts: 5, Load: 0.1, ResetAt: now.Add(time.Hour), Util5h: 0.2, Util7d: 0.2}
	case "prov-b":
		return scoring.Signals{LatencyMs: 50, SuccessRate: 0.95, CostUSDPer1M: 30, QualityScore: 0.95, QuotaHint: 0, Attempts: 1, Load: 0.9, ResetAt: now.Add(5 * time.Minute), Util5h: 0.8, Util7d: 0.8}
	case "prov-c":
		return scoring.Signals{LatencyMs: 200, SuccessRate: 0.80, CostUSDPer1M: 1, QualityScore: 0.8, QuotaHint: 10, Attempts: 9, Load: 0, ResetAt: now.Add(2 * time.Hour), Util5h: 0.5, Util7d: 0.5}
	}
	return scoring.Signals{}
}

func conformanceOrder(t *testing.T, ranked []router.Attempt) string {
	t.Helper()
	if len(ranked) != 3 {
		t.Fatalf("ranked %d attempts, want 3 (a permutation)", len(ranked))
	}
	seen := map[string]bool{}
	out := ""
	for _, att := range ranked {
		if seen[att.ProviderID] {
			t.Fatalf("duplicate attempt %q: not a permutation", att.ProviderID)
		}
		seen[att.ProviderID] = true
		out += att.ProviderID[len("prov-"):]
	}
	if len(seen) != 3 {
		t.Fatalf("ranked order %q is not a permutation of the fixture", out)
	}
	return out
}

func TestConformanceEveryStrategyIsAPermutation(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	for _, st := range All {
		hook := factory(string(st))
		if hook == nil {
			t.Fatalf("strategy %q: HookFactory returned nil", st)
		}
		ranked := hook(conformanceChain())
		conformanceOrder(t, ranked)
	}
}

func TestConformanceUnknownStrategyIsPriority(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	ranked := factory("no-such-strategy")(conformanceChain())
	if got := conformanceOrder(t, ranked); got != "abc" {
		t.Errorf("unknown strategy order = %q, want declared order abc", got)
	}
}

func TestConformanceNilSignalsIsIdentity(t *testing.T) {
	factory := HookFactory(nil, scoring.Weights{})
	for _, st := range All {
		if got := conformanceOrder(t, factory(string(st))(conformanceChain())); got != "abc" {
			t.Errorf("strategy %q with nil signals = %q, want declared order abc", st, got)
		}
	}
}

func TestConformanceRoundRobinCycles(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	hook := factory("round-robin")
	// Rotation state is process-wide and per chain (Phase C), so
	// the absolute starting order depends on earlier calls in
	// this test binary — what the conformance suite pins is the
	// cycle itself: each call is a left rotation of the previous
	// one, and the fourth call returns to the first order. The
	// exact rotation from a fresh state is covered by the
	// builder's TestOrderingRoundRobinCyclesEvenly.
	rotateLeft := func(s string) string {
		if len(s) < 2 {
			return s
		}
		return s[1:] + s[:1]
	}
	orders := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		orders = append(orders, conformanceOrder(t, hook(conformanceChain())))
	}
	for i := 1; i < 4; i++ {
		if orders[i] != rotateLeft(orders[i-1]) {
			t.Errorf("round-robin call %d = %q, want left rotation of %q", i+1, orders[i], orders[i-1])
		}
	}
	if orders[3] != orders[0] {
		t.Errorf("round-robin after a full cycle = %q, want %q", orders[3], orders[0])
	}
}

func TestConformanceFillFirstDemotesNoQuota(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// b has QuotaHint 0 (no quota); a and c have quota, in
	// declared order. Fill-first drains a, then c, then b.
	if got := conformanceOrder(t, factory("fill-first")(conformanceChain())); got != "acb" {
		t.Errorf("fill-first order = %q, want acb", got)
	}
}

func TestConformanceCostOptimizedCheapestFirst(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Prices: c $1, a $10, b $30 — pure cheapest-first.
	if got := conformanceOrder(t, factory("cost-optimized")(conformanceChain())); got != "cab" {
		t.Errorf("cost-optimized order = %q, want cab", got)
	}
}

func TestConformanceModePacksDiffer(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// ship-fast is latency-heavy: b (50 ms) must lead.
	if got := conformanceOrder(t, factory("ship-fast")(conformanceChain())); got[0] != 'b' {
		t.Errorf("ship-fast order = %q, want b first", got)
	}
	// cost-saver is cost-heavy: c ($1) must lead.
	if got := conformanceOrder(t, factory("cost-saver")(conformanceChain())); got[0] != 'c' {
		t.Errorf("cost-saver order = %q, want c first", got)
	}
}

func TestConformanceCustomWeights(t *testing.T) {
	costOnly := scoring.Weights{Cost: 1}
	factory := HookFactory(conformanceSignals, costOnly)
	if got := conformanceOrder(t, factory("custom-weights")(conformanceChain())); got[0] != 'c' {
		t.Errorf("custom-weights(cost-only) order = %q, want c first", got)
	}
	latencyOnly := scoring.Weights{Latency: 1}
	factory = HookFactory(conformanceSignals, latencyOnly)
	if got := conformanceOrder(t, factory("custom-weights")(conformanceChain())); got[0] != 'b' {
		t.Errorf("custom-weights(latency-only) order = %q, want b first", got)
	}
}

func TestConformanceWeightedDrawIsAPermutation(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Uniform draw (all weights 1): the leader varies, but the
	// result is always a permutation with the drawn leader first.
	for i := 0; i < 20; i++ {
		conformanceOrder(t, factory("weighted")(conformanceChain()))
	}
}

func TestConformanceDataLimitedStrategiesAreIdentityToday(t *testing.T) {
	// context-optimized (no windows) and context-relay (no engine
	// integration) return the declared order until their data lands
	// (Phase D). The test pins the honest behaviour so a future
	// data-plumbing change must update it deliberately. least-used
	// and lkgp left this list in Phase C: attempt counts and LKGP
	// records are now live inputs, covered by their own conformance
	// tests below. cache-optimized left this list in M6.5-wiring:
	// it now reorders on measured warmth when the per-request
	// factory carries a cacheable request (see
	// m6_5_wiring_test.go); under the context-free HookFactory it
	// still returns declared order, because the gate has no prompt
	// to measure and no target has measured warmth — pinned by
	// TestM65CacheOptimizedContextFreeFactoryIsIdentity, not here.
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	for _, name := range []string{"context-optimized", "context-relay"} {
		if got := conformanceOrder(t, factory(name)(conformanceChain())); got != "abc" {
			t.Errorf("strategy %q = %q today, want declared order abc (no data yet)", name, got)
		}
	}
}

func TestConformanceEmptyChain(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	for _, st := range All {
		if got := factory(string(st))(nil); len(got) != 0 {
			t.Errorf("strategy %q on empty chain returned %d attempts, want 0", st, len(got))
		}
	}
}

// M3 Phase C conformance: the quota/affinity strategies now read
// live inputs (attempt counts, load, reset times, utilisation,
// chain weights, LKGP records), so their orders are asserted
// exactly on the fixture — not just as permutations.

func TestConformanceLeastUsedFewestFirst(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Attempts: b 1, a 5, c 9 — fewest first.
	if got := conformanceOrder(t, factory("least-used")(conformanceChain())); got != "bac" {
		t.Errorf("least-used order = %q, want bac", got)
	}
}

func TestConformanceResetWindowSoonestFirst(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Resets: b +5m, a +1h, c +2h — soonest first.
	if got := conformanceOrder(t, factory("reset-window")(conformanceChain())); got != "bac" {
		t.Errorf("reset-window order = %q, want bac", got)
	}
}

func TestConformanceResetAwareOrdersByScore(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Reset-aware scores on the fixture (remaining from utils,
	// urgency from resets): b ~0.61 (nearly spent but resetting
	// in 5m), c ~0.46, a ~0.38 — most urgent first.
	if got := conformanceOrder(t, factory("reset-aware")(conformanceChain())); got != "bca" {
		t.Errorf("reset-aware order = %q, want bca", got)
	}
}

func TestConformanceExpiryFirstMostUrgentFirst(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// Usable fraction (quota boolean) over hours until the reset
	// deadline: a 1/1h = 1.0, c 1/2h = 0.5, b spent = 0.
	if got := conformanceOrder(t, factory("expiry-first")(conformanceChain())); got != "acb" {
		t.Errorf("expiry-first order = %q, want acb", got)
	}
}

func TestConformanceQuotaWeightedIsAPermutation(t *testing.T) {
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	// The draw is random (score/(1+load)); the result is always
	// a permutation with the drawn winner first.
	for i := 0; i < 20; i++ {
		conformanceOrder(t, factory("quota-weighted")(conformanceChain()))
	}
}

func TestConformanceWeightedChainWeights(t *testing.T) {
	// Only prov-a carries a positive chain weight for model-x,
	// so the weighted draw must pick it every time; the rest
	// follow in declared order.
	SetChainWeights(map[string]map[string]float64{
		"model-x": {"prov-a/model-x": 1, "prov-b/model-x": 0, "prov-c/model-x": 0},
	})
	t.Cleanup(func() { SetChainWeights(nil) })
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	for i := 0; i < 20; i++ {
		if got := conformanceOrder(t, factory("weighted")(conformanceChain())); got != "abc" {
			t.Fatalf("weighted with chain weights = %q, want abc (a always drawn first)", got)
		}
	}
}

func TestConformanceLKGPOrdersByLastGood(t *testing.T) {
	// Record prov-b as the last-known-good provider for model-x:
	// the lkgp hook must put it first, the rest in declared order.
	// This test runs after TestConformanceDataLimitedStrategies-
	// AreIdentityToday (file order), which pins the empty-store
	// behaviour before any record lands in the shared registry.
	DefaultLKGPRegistry.RecordSuccessFor("model-x", "prov-b", "model-x", "")
	factory := HookFactory(conformanceSignals, scoring.Weights{})
	if got := conformanceOrder(t, factory("lkgp")(conformanceChain())); got != "bac" {
		t.Errorf("lkgp order = %q, want bac", got)
	}
}

// TestSixteenInputsFromSignalsLiveFactors (G19 Area 1): every new
// live signal lands in the 16-factor inputs — measured quota
// fraction, positively-known CLOSED breaker, key-pool size,
// registry context window, known-$0 free tier, real reset time.
func TestSixteenInputsFromSignalsLiveFactors(t *testing.T) {
	now := time.Now()
	s := scoring.Signals{
		LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 0, QualityScore: 0.8,
		QuotaHint: 40, QuotaRemainingPct: 40, Attempts: 3,
		BreakerClosed: true, ConnectionCount: 4, ContextWindow: 8000,
		ResetAt: now.Add(2 * time.Hour),
	}
	in := SixteenInputsFromSignals(s)
	if in.QuotaRemainingPct != 40 {
		t.Errorf("QuotaRemainingPct = %v, want 40 (measured fraction)", in.QuotaRemainingPct)
	}
	if in.Breaker != scoring.BreakerClosed {
		t.Errorf("Breaker = %v, want closed (positively known)", in.Breaker)
	}
	if in.ConnectionCount != 4 {
		t.Errorf("ConnectionCount = %d, want 4 (live key-pool size)", in.ConnectionCount)
	}
	if in.ModelContextTokens != 8000 {
		t.Errorf("ModelContextTokens = %v, want 8000", in.ModelContextTokens)
	}
	if in.ModelTier != scoring.ModelTierFree {
		t.Errorf("ModelTier = %v, want free (known $0 price)", in.ModelTier)
	}
	if in.ResetInSeconds <= 0 || in.ResetInSeconds > 2*3600 {
		t.Errorf("ResetInSeconds = %v, want (0, 7200]", in.ResetInSeconds)
	}
	if in.LatencyP95Ms != 120 || in.CostPer1M != 0 || in.SuccessRate != 0.9 || in.QualityScore != 0.8 {
		t.Errorf("carried signals = latency %v cost %v success %v quality %v, want 120 0 0.9 0.8",
			in.LatencyP95Ms, in.CostPer1M, in.SuccessRate, in.QualityScore)
	}
	// Contradictory OPEN + CLOSED reads OPEN (fail-closed).
	s.BreakerOpen = true
	if got := SixteenInputsFromSignals(s); got.Breaker != scoring.BreakerOpen {
		t.Errorf("OPEN+CLOSED Breaker = %v, want open", got.Breaker)
	}
}

// TestSixteenInputsFromSignalsUnknownStaysNeutral (G19 Area 1): a
// signal with no measurements degrades to neutral, never to a
// fake value — except QuotaHint 0, which the existing convention
// reads as exhausted (the scalar hint is authoritative there).
func TestSixteenInputsFromSignalsUnknownStaysNeutral(t *testing.T) {
	in := SixteenInputsFromSignals(scoring.Signals{QuotaHint: 5, CostUSDPer1M: -1, Util5h: -1, Util7d: -1})
	if in.QuotaRemainingPct != -1 {
		t.Errorf("QuotaRemainingPct = %v, want -1 (unknown fraction)", in.QuotaRemainingPct)
	}
	if in.Breaker != scoring.BreakerUnknown {
		t.Errorf("Breaker = %v, want unknown", in.Breaker)
	}
	if in.ConnectionCount != 0 {
		t.Errorf("ConnectionCount = %d, want 0 (unknown pool size)", in.ConnectionCount)
	}
	if in.ModelContextTokens != 0 || in.CachedPromptFraction != -1 || in.ResetInSeconds != -1 {
		t.Errorf("unknown inputs = window %v cache %v reset %v, want 0 -1 -1",
			in.ModelContextTokens, in.CachedPromptFraction, in.ResetInSeconds)
	}
	if got := scoring.FactorQuota(in); got != 0.5 {
		t.Errorf("FactorQuota(unknown) = %v, want neutral 0.5", got)
	}
	// Zero Signals (QuotaHint 0) reads as exhausted, per the
	// long-standing scalar-hint convention — not as unknown.
	if got := SixteenInputsFromSignals(scoring.Signals{}); got.QuotaRemainingPct != 0 {
		t.Errorf("zero Signals QuotaRemainingPct = %v, want 0 (exhausted)", got.QuotaRemainingPct)
	}
}

// TestSixteenInputsCtxPromptAndWarmth (G19 Area 1): the
// per-request context feeds contextAffinity (prompt tokens vs
// the model's window) and the warmth func feeds cacheAffinity;
// both stay neutral when the input is absent.
func TestSixteenInputsCtxPromptAndWarmth(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		return scoring.Signals{QuotaHint: 10, CostUSDPer1M: 5, ContextWindow: 8000}
	}
	att := router.Attempt{ProviderID: "p", Model: "m"}
	ctx := router.StrategyHookContext{PromptTokens: 4000}
	warmth := func(provider, model string) (float64, bool) { return 0.75, true }
	in := sixteenInputsCtx(att, signals, 1000, 10, ctx, warmth)
	if got := scoring.FactorContextAffinity(in); got != 0.5 {
		t.Errorf("FactorContextAffinity = %v, want 0.5 (1 - 4000/8000)", got)
	}
	if got := scoring.FactorCacheAffinity(in); got != 0.75 {
		t.Errorf("FactorCacheAffinity = %v, want 0.75 (measured warmth)", got)
	}
	// No context, no warmth: both neutral.
	plain := sixteenInputsCtx(att, signals, 1000, 10, router.StrategyHookContext{}, nil)
	if got := scoring.FactorContextAffinity(plain); got != 0.5 {
		t.Errorf("FactorContextAffinity(no ctx) = %v, want neutral 0.5", got)
	}
	if got := scoring.FactorCacheAffinity(plain); got != 0.5 {
		t.Errorf("FactorCacheAffinity(no warmth) = %v, want neutral 0.5", got)
	}
}

// TestSixteenHookWarmthReordersModePack (G19 Area 1, RT-4): the
// live mode-pack path ranks by measured warmth — the warmth func
// flows through HookFactoryWithCacheAffinity into sixteenHookCtx,
// and the pack's nonzero cacheAffinity weight makes the warm
// candidate lead. With a nil warmth func the factor stays neutral
// and the declared order stands.
func TestSixteenHookWarmthReordersModePack(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		return scoring.Signals{QuotaHint: 10, CostUSDPer1M: 5}
	}
	chain := []router.Attempt{
		{ProviderID: "cold", Model: "m"},
		{ProviderID: "warm", Model: "m"},
	}
	warmth := func(provider, model string) (float64, bool) {
		if provider == "warm" {
			return 0.9, true
		}
		return 0.0, true
	}
	factory := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, warmth)
	ranked := factory("ship-fast", router.StrategyHookContext{})(chain)
	if ranked[0].ProviderID != "warm" {
		t.Fatalf("ship-fast with warmth = %q first, want warm (measured 0.9 vs 0.0)", ranked[0].ProviderID)
	}
	// No warmth source: neutral, declared order stands.
	plain := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)
	if got := plain("ship-fast", router.StrategyHookContext{})(chain); got[0].ProviderID != "cold" {
		t.Fatalf("ship-fast without warmth = %q first, want cold (declared order)", got[0].ProviderID)
	}
}

// TestContextOptimizedOrdersByLiveWindow (G19 Area 1): with real
// windows in the signals the largest window leads; with no window
// anywhere the declared order stands.
func TestContextOptimizedOrdersByLiveWindow(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-a":
			return scoring.Signals{ContextWindow: 8000}
		case "prov-b":
			return scoring.Signals{ContextWindow: 128000}
		default:
			return scoring.Signals{ContextWindow: 32000}
		}
	}
	factory := HookFactory(signals, scoring.Weights{})
	ranked := factory("context-optimized")(conformanceChain())
	if got := conformanceOrder(t, ranked); got != "bca" {
		t.Errorf("context-optimized order = %q, want bca (128k, 32k, 8k)", got)
	}
	// No windows anywhere: identity.
	blank := HookFactory(func(_, _ string) scoring.Signals { return scoring.Signals{} }, scoring.Weights{})
	if got := conformanceOrder(t, blank("context-optimized")(conformanceChain())); got != "abc" {
		t.Errorf("context-optimized (no windows) = %q, want declared abc", got)
	}
}

// TestSixteenHookContextAffinityDecides (G19 Area 1): under the
// ship-fast pack the contextAffinity factor (weight 0.0095) is the
// only difference between two otherwise-identical candidates, so
// the model whose window fits the prompt leads.
func TestSixteenHookContextAffinityDecides(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		base := scoring.Signals{LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 10, QualityScore: 0.9, QuotaHint: 10}
		if providerID == "prov-a" {
			base.ContextWindow = 8000
		} else {
			base.ContextWindow = 1000
		}
		return base
	}
	chain := []router.Attempt{
		{ProviderID: "prov-b", Model: "model-x"},
		{ProviderID: "prov-a", Model: "model-x"},
	}
	ctx := router.StrategyHookContext{PromptTokens: 4000}
	factory := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)
	ranked := factory("ship-fast", ctx)(chain)
	if len(ranked) != 2 || ranked[0].ProviderID != "prov-a" {
		t.Errorf("ship-fast order = %v, want prov-a first (window fits the 4000-token prompt)", ranked)
	}
}

// TestToQuotaSnapshotsRealFraction (G19 Area 1): a measured
// remaining fraction flows into the snapshot as a continuous
// value (expiry-first's usable), not the booleanised hint.
func TestToQuotaSnapshotsRealFraction(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		if providerID == "prov-a" {
			return scoring.Signals{QuotaHint: 40, QuotaRemainingPct: 40}
		}
		return scoring.Signals{QuotaHint: 90, QuotaRemainingPct: 90}
	}
	snaps := toQuotaSnapshots(conformanceChain()[:2], signals, balancedScorer())
	if snaps[0].QuotaRemaining != 0.4 || snaps[1].QuotaRemaining != 0.9 {
		t.Errorf("QuotaRemaining = %v, %v, want 0.4, 0.9", snaps[0].QuotaRemaining, snaps[1].QuotaRemaining)
	}
	// Without a measured fraction the old booleanisation stands.
	plain := toQuotaSnapshots(conformanceChain()[:2],
		func(_, _ string) scoring.Signals { return scoring.Signals{QuotaHint: 5} }, balancedScorer())
	if plain[0].QuotaRemaining != 1 {
		t.Errorf("QuotaRemaining(no fraction) = %v, want 1 (hint > 0)", plain[0].QuotaRemaining)
	}
	// The fraction changes expiry-first ordering: 40% usable
	// scores below 90% usable at the same deadline.
	now := time.Now()
	a := QuotaSnapshot{Provider: "prov-a", Model: "m", QuotaRemaining: 0.4, ResetAt: now.Add(time.Hour)}
	b := QuotaSnapshot{Provider: "prov-b", Model: "m", QuotaRemaining: 0.9, ResetAt: now.Add(time.Hour)}
	if got := ExpiryFirstScore(a, now); got >= ExpiryFirstScore(b, now) {
		t.Errorf("ExpiryFirstScore(40%%) = %v, want < ExpiryFirstScore(90%%) = %v", got, ExpiryFirstScore(b, now))
	}
}
