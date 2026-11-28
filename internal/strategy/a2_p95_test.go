// A2-6: the SLA-aware signal is the TRUE histogram p95 (not the p50 proxy),
// and the 16-factor input prefers it too.
package strategy

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// TestSLAAwareUsesTrueP95OverP50 proves SLA ordering scores on the histogram
// p95, not the p50 EWMA: prov-a has a LOW p50 (50 ms) but a HIGH p95 (3000 ms,
// over the 2000 ms target); prov-b has p50 1200 ms / p95 1500 ms (compliant)
// but costs more. With hard constraints the compliant candidate must lead. If
// the hook used the p50 proxy instead of the p95 histogram signal, prov-a
// would be compliant (50 ms) and cheaper, so it would lead instead — this
// test FAILS when the p95 branch is removed.
func TestSLAAwareUsesTrueP95OverP50(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-a":
			return scoring.Signals{LatencyMs: 50, LatencyP95Ms: 3000, SuccessRate: 1, Attempts: 30, CostUSDPer1M: 1}
		case "prov-b":
			return scoring.Signals{LatencyMs: 1200, LatencyP95Ms: 1500, SuccessRate: 1, Attempts: 30, CostUSDPer1M: 5}
		}
		return scoring.Signals{CostUSDPer1M: -1}
	}
	chain := []router.Attempt{
		{ProviderID: "prov-a", Model: "m"},
		{ProviderID: "prov-b", Model: "m"},
	}
	hook := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)(
		"sla-aware", router.StrategyHookContext{SLAHardConstraints: true})
	got := hook(chain)
	if len(got) != 2 {
		t.Fatalf("hook returned %d attempts, want 2", len(got))
	}
	if got[0].ProviderID != "prov-b" {
		t.Fatalf("sla-aware first = %q, want prov-b (p95 1500ms compliant); prov-a first means the p50 proxy was used, not the p95 histogram signal", got[0].ProviderID)
	}
}

// TestSLAAwareP50FallbackBelowHistogramSamples: when the signal carries only
// the p50 proxy (p95 unavailable, <20 samples), SLA falls back to it rather
// than treating latency as unknown.
func TestSLAAwareP50FallbackBelowHistogramSamples(t *testing.T) {
	signals := func(providerID, model string) scoring.Signals {
		switch providerID {
		case "prov-a":
			return scoring.Signals{LatencyMs: 5000, SuccessRate: 1, Attempts: 5, CostUSDPer1M: 1} // p95 unset (-1)
		case "prov-b":
			return scoring.Signals{LatencyMs: 1000, SuccessRate: 1, Attempts: 5, CostUSDPer1M: 5}
		}
		return scoring.Signals{CostUSDPer1M: -1}
	}
	chain := []router.Attempt{
		{ProviderID: "prov-a", Model: "m"},
		{ProviderID: "prov-b", Model: "m"},
	}
	hook := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)(
		"sla-aware", router.StrategyHookContext{SLAHardConstraints: true})
	if got := hook(chain); got[0].ProviderID != "prov-b" {
		t.Fatalf("sla-aware (p50 fallback) first = %q, want prov-b (5000ms > 2000ms target)", got[0].ProviderID)
	}
}

// TestSixteenInputsPrefersTrueP95: the 16-factor input uses the true p95 when
// present, and the p50 proxy only as the bounded fallback.
func TestSixteenInputsPrefersTrueP95(t *testing.T) {
	withP95 := SixteenInputsFromSignals(scoring.Signals{LatencyMs: 50, LatencyP95Ms: 3000})
	if withP95.LatencyP95Ms != 3000 {
		t.Fatalf("SixteenInputs.LatencyP95Ms = %v, want the true p95 3000 (not the p50 50)", withP95.LatencyP95Ms)
	}
	p50Only := SixteenInputsFromSignals(scoring.Signals{LatencyMs: 50})
	if p50Only.LatencyP95Ms != 50 {
		t.Fatalf("SixteenInputs.LatencyP95Ms = %v, want the p50 fallback 50 when p95 is unavailable", p50Only.LatencyP95Ms)
	}
}
