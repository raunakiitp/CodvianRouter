// B2-08 (PARITY.md:508): quota-share ENGINE path. The DRR core is
// already pinned (TestQuotaShareDRRMatchesReferenceSequence); what
// was missing is the engine around it: at-cap gating, the P2C
// top-two swap, the strategy-tree intercept, and the
// Config.QuotaShareFor wiring from the operator share table.
// In-flight counts are driven through the engine's own adaptive
// limiter (TryAcquire/Release, keyed by provider) — no mocks of the
// routing logic.
package router

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// b2Acquire fills provider p's limiter to its cap (DefaultInitialLimit
// 20) and returns a release func. Every acquire must succeed: a
// failed acquire means the test setup is wrong, not the code.
func b2Acquire(t *testing.T, eng *Engine, p string) func() {
	t.Helper()
	for i := 0; i < int(adaptive.DefaultInitialLimit); i++ {
		if !eng.limiter.TryAcquire(p) {
			t.Fatalf("acquire %d for %q failed — limiter cap is not %v", i+1, p, adaptive.DefaultInitialLimit)
		}
	}
	return func() {
		for i := 0; i < int(adaptive.DefaultInitialLimit); i++ {
			eng.limiter.Release(p)
		}
	}
}

// b2InFlight returns the limiter's in-flight count and cap for p, or
// fails when p has no limiter state at all.
func b2InFlight(t *testing.T, eng *Engine, p string) (int, float64) {
	t.Helper()
	for _, st := range eng.ConcurrencyStats() {
		if st.Provider == p {
			return st.InFlight, st.Limit
		}
	}
	t.Fatalf("no limiter state for %q (acquires did not register?)", p)
	return 0, 0
}

// TestB2QuotaShareAtCapGating: a provider at its adaptive cap is
// deprioritised to the tail; when every member is at cap the pool
// fails open and the DRR order stands.
func TestB2QuotaShareAtCapGating(t *testing.T) {
	// Control: no load — DRR winner is the first member.
	eng := NewEngine(nil, nil, testConfig())
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "a" {
		t.Fatalf("unloaded orderQuotaShare = %v, want [a b]", got)
	}

	// "a" at cap: "b" serves first, "a" demoted to the tail.
	eng = NewEngine(nil, nil, testConfig())
	release := b2Acquire(t, eng, "a")
	defer release()
	if n, lim := b2InFlight(t, eng, "a"); n < int(lim) {
		t.Fatalf("a in-flight %d < cap %v — not at cap, test setup broken", n, lim)
	}
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("at-cap orderQuotaShare = %v, want [b a] (capped member last)", got)
	}

	// Both at cap: fail-open — the whole pool is eligible again and
	// the DRR order (fresh engine, first round) is [a b].
	eng = NewEngine(nil, nil, testConfig())
	releaseA := b2Acquire(t, eng, "a")
	defer releaseA()
	releaseB := b2Acquire(t, eng, "b")
	defer releaseB()
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "a" {
		t.Fatalf("all-at-cap orderQuotaShare = %v, want [a b] (fail-open, DRR order)", got)
	}
}

// TestB2QuotaShareP2CSwapsToLessLoaded: between the top two DRR
// candidates the one with fewer in-flight requests wins.
func TestB2QuotaShareP2CSwapsToLessLoaded(t *testing.T) {
	// DRR winner is "a" (equal shares, input order); "a" busier
	// than "b" → P2C swaps, "b" first.
	eng := NewEngine(nil, nil, testConfig())
	if !eng.limiter.TryAcquire("a") {
		t.Fatal("setup acquire for a failed")
	}
	defer eng.limiter.Release("a")
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "b" {
		t.Fatalf("P2C orderQuotaShare = %v, want [b a] (a has 1 in-flight, b has 0)", got)
	}

	// Reverse: "b" busier — no swap, DRR winner "a" stays first.
	eng = NewEngine(nil, nil, testConfig())
	if !eng.limiter.TryAcquire("b") {
		t.Fatal("setup acquire for b failed")
	}
	defer eng.limiter.Release("b")
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "a" {
		t.Fatalf("P2C orderQuotaShare = %v, want [a b] (b has 1 in-flight, a has 0)", got)
	}

	// No load at all: DRR winner "a" first, no swap.
	eng = NewEngine(nil, nil, testConfig())
	if got := providerIDs(eng.orderQuotaShare(qsChain("a", "b"))); len(got) != 2 || got[0] != "a" {
		t.Fatalf("unloaded orderQuotaShare = %v, want [a b]", got)
	}
}

// TestB2QuotaShareTreeIntercept: a strategy-tree node naming
// "quota-share" orders its children's winners through the engine
// DRR — six sequential orderings rotate A,B,A,A,B,A for 2:1 shares.
// Without the intercept the winners would rank by input order and
// "a" would win every round.
func TestB2QuotaShareTreeIntercept(t *testing.T) {
	cfg := testConfig()
	cfg.QuotaShareFor = func(providerID, model string) float64 {
		if providerID == "a" {
			return 2
		}
		return 1
	}
	eng := NewEngine(nil, nil, cfg)
	tree := &strategytree.Node{Strategy: "quota-share", Children: []*strategytree.Node{
		{Provider: "a", Model: "m"},
		{Provider: "b", Model: "m"},
	}}
	want := []string{"a", "b", "a", "a", "b", "a"}
	for i, w := range want {
		ordered, _, err := eng.orderAttemptsByTree(qsChain("a", "b"), tree, "", "hard")
		if err != nil {
			t.Fatalf("round %d: orderAttemptsByTree = %v", i+1, err)
		}
		if len(ordered) != 2 || ordered[0].ProviderID != w {
			t.Fatalf("round %d: tree first pick = %v, want %q (DRR rotation through the tree intercept)", i+1, providerIDs(ordered), w)
		}
	}
}

// TestB2QuotaShareRankAttemptsIntercept pins the same intercept at
// the rankAttempts wrapper the tree path calls.
func TestB2QuotaShareRankAttemptsIntercept(t *testing.T) {
	cfg := testConfig()
	cfg.QuotaShareFor = func(providerID, model string) float64 {
		if providerID == "a" {
			return 2
		}
		return 1
	}
	eng := NewEngine(nil, nil, cfg)
	want := []string{"a", "b", "a", "a", "b", "a"}
	for i, w := range want {
		got := eng.rankAttempts("quota-share", qsChain("a", "b"))
		if len(got) != 2 || got[0].ProviderID != w {
			t.Fatalf("round %d: rankAttempts(quota-share) = %v, want first pick %q", i+1, providerIDs(got), w)
		}
	}
	// A non-quota-share strategy does NOT run the DRR: input order.
	if got := providerIDs(eng.rankAttempts("priority", qsChain("a", "b"))); len(got) != 2 || got[0] != "a" {
		t.Fatalf("rankAttempts(priority) = %v, want [a b] (no DRR outside quota-share)", got)
	}
}

// TestB2QuotaShareConfigSharesAreModelSpecific: the engine passes
// (provider, model) — not just the provider — to Config.QuotaShareFor,
// the contract main.go's quota_shares table relies on. Shares a/m1=3,
// a/m2=1 (missing entries default to 1): the m1 pool rotates 3:1
// (A,A,B,A) while the m2 pool rotates 1:1 (A,B,A,B). A provider-only
// lookup would rotate both pools 3:1.
func TestB2QuotaShareConfigSharesAreModelSpecific(t *testing.T) {
	shares := map[string]map[string]float64{"a": {"m1": 3}}
	cfg := testConfig()
	cfg.QuotaShareFor = func(providerID, model string) float64 {
		if byModel, ok := shares[providerID]; ok {
			if s, ok := byModel[model]; ok {
				return s
			}
		}
		return 1
	}
	eng := NewEngine(nil, nil, cfg)
	chainM1 := []Attempt{{ProviderID: "a", Model: "m1"}, {ProviderID: "b", Model: "m1"}}
	chainM2 := []Attempt{{ProviderID: "a", Model: "m2"}, {ProviderID: "b", Model: "m2"}}
	wantM1 := []string{"a", "a", "b", "a"}
	wantM2 := []string{"a", "b", "a", "b"}
	for i := range wantM1 {
		if got := providerIDs(eng.orderQuotaShare(chainM1)); got[0] != wantM1[i] {
			t.Fatalf("m1 pool round %d: first pick = %v, want %q (3:1 shares)", i+1, got, wantM1[i])
		}
		if got := providerIDs(eng.orderQuotaShare(chainM2)); got[0] != wantM2[i] {
			t.Fatalf("m2 pool round %d: first pick = %v, want %q (1:1 shares — model must reach the share lookup)", i+1, got, wantM2[i])
		}
	}
}
