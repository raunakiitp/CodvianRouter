package strategy

// B2-542 selector tests (S1-B2-542-SELECTOR, 2026-11-20).
//
// "cascade" must be a selectable strategy name: strategy.Parse
// (the dashboard/MCP surface) accepts it, it sits in All (the
// picker order), Describe names it, and the hook factory returns
// the identity for it — the cascade ordering lives in the engine
// (internal/router/cascade.go), so a hook call must never
// re-rank it under balanced weights (the Nadir precedent).

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

func TestB2CascadeParse(t *testing.T) {
	st, ok := Parse("cascade")
	if !ok || st != StrategyCascade {
		t.Fatalf("Parse(cascade) = %q, %v (want cascade, true)", st, ok)
	}
	st, ok = Parse("Cascade")
	if !ok || st != StrategyCascade {
		t.Fatalf("Parse(Cascade) = %q, %v (want cascade, true)", st, ok)
	}
	if !IsValid("cascade") {
		t.Fatalf("IsValid(cascade) = false, want true")
	}
	found := false
	for _, s := range All {
		if s == StrategyCascade {
			found = true
		}
	}
	if !found {
		t.Fatalf("All does not contain cascade (picker cannot list it)")
	}
	name, rule := Describe(StrategyCascade)
	if name == "" || rule == "" {
		t.Fatalf("Describe(cascade) = %q, %q (want a name and a rule)", name, rule)
	}
	if name == "Balanced" {
		t.Fatalf("Describe(cascade) fell through to the default (Balanced)")
	}
}

// TestB2CascadeHookIsIdentity pins the wiringHook case: a direct
// hook call with "cascade" keeps the declared order. The control
// proves the signals would re-rank under balanced — steady
// dominates on every axis (fastest, cheapest, most reliable) —
// so identity is the cascade case doing its job, not the signals
// being ties.
func TestB2CascadeHookIsIdentity(t *testing.T) {
	sig := func(id, model string) scoring.Signals {
		switch id {
		case "steady":
			return scoring.Signals{LatencyMs: 10, SuccessRate: 1, CostUSDPer1M: 0, QualityScore: 1}
		case "fast":
			return scoring.Signals{LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 10, QualityScore: 0.8}
		default: // cheap
			return scoring.Signals{LatencyMs: 5000, SuccessRate: 0.5, CostUSDPer1M: 100, QualityScore: 0.1}
		}
	}
	factory := HookFactory(sig, scoring.Weights{})
	control := factory("balanced")(strategyChain())
	if len(control) != 3 || control[0].ProviderID != "steady" {
		t.Fatalf("balanced control first = %q (want steady — the signals must re-rank, or this test proves nothing)", providerIDOf(control))
	}
	ranked := factory("cascade")(strategyChain())
	if len(ranked) != 3 || ranked[0].ProviderID != "cheap" || ranked[1].ProviderID != "fast" || ranked[2].ProviderID != "steady" {
		t.Fatalf("cascade hook order = %v, want declared [cheap fast steady] (identity, never a re-rank)", providerIDsOf(ranked))
	}
}

func providerIDOf(chain []router.Attempt) string {
	if len(chain) == 0 {
		return ""
	}
	return chain[0].ProviderID
}

func providerIDsOf(chain []router.Attempt) []string {
	out := make([]string, len(chain))
	for i, att := range chain {
		out[i] = att.ProviderID
	}
	return out
}
