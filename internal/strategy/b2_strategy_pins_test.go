// B2 gap tests (T-B2-STRAT): the two missing strategy tests that
// block flips. Both exercise the real ranking code — no mocks of
// the ranking logic — and both pin behaviour the existing suite
// leaves unproven:
//
//   - TestBalancedHookOrdersBySixteenOnlyFactor pins the default
//     16-factor wiring: factory("balanced") must order a chain by
//     a 16-only factor (reset-window affinity). It FAILS if the
//     balanced hook is the 5-factor HookWithWeights instead of
//     the 16-factor default (unlocks PARITY :510/:511/:512).
//   - TestPoolRankingLKGPStickinessLeads runs the real pool
//     ranking (RankModelsByBestPair16) with a real LKGP pair and
//     asserts the last-good model leads (unlocks PARITY :527).
package strategy

import (
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// TestBalancedHookOrdersBySixteenOnlyFactor: factory("balanced") on
// a chain where ONLY a 16-only factor discriminates. Both
// candidates carry identical signals on every 5-factor input
// (latency, success, cost, quality, quota) and differ only on
// ResetAt — the reset-window affinity factor, which the 5-factor
// scorer never reads. The sooner-resetting candidate must lead
// under the 16-factor default; the 5-factor control must keep
// declared order, proving the chain isolates a 16-only factor.
func TestBalancedHookOrdersBySixteenOnlyFactor(t *testing.T) {
	now := time.Now()
	signals := func(providerID, model string) scoring.Signals {
		s := scoring.Signals{
			LatencyMs:    100,
			SuccessRate:  0.9,
			CostUSDPer1M: 1.0,
			QualityScore: 0.8,
			QuotaHint:    50,
			Attempts:     10,
		}
		switch providerID {
		case "prov-a":
			// Resets in 29 days: reset-window factor ~0.03.
			s.ResetAt = now.Add(29 * 24 * time.Hour)
		case "prov-b":
			// Resets in 1 minute: reset-window factor ~1.0.
			s.ResetAt = now.Add(time.Minute)
		}
		return s
	}
	chain := []router.Attempt{
		{ProviderID: "prov-a", Model: "model-a"},
		{ProviderID: "prov-b", Model: "model-b"},
	}

	// Control: the 5-factor balanced hook cannot see the reset
	// window, so with all five inputs equal it must keep the
	// declared order. If this ever reorders, the chain no longer
	// isolates a 16-only factor and the test below proves nothing.
	five := scoring.HookWithWeights(signals, scoring.Modes[scoring.ModeBalanced])(chain)
	if five[0].ProviderID != "prov-a" || five[1].ProviderID != "prov-b" {
		t.Fatalf("5-factor control: got [%s %s], want declared [prov-a prov-b] — the chain must discriminate ONLY via a 16-only factor",
			five[0].ProviderID, five[1].ProviderID)
	}

	// The factory("balanced") hook is the 16-factor default
	// (wiringHook StrategyBalanced -> scoring.Hook16Default): the
	// sooner-resetting candidate must lead. If the wiring is
	// reverted to the 5-factor HookWithWeights, this returns
	// declared order and the test FAILS — that is the pin.
	hook := HookFactory(signals, scoring.Weights{})("balanced")
	got := hook(chain)
	if len(got) != 2 || got[0].ProviderID != "prov-b" || got[1].ProviderID != "prov-a" {
		t.Fatalf("balanced hook: got [%s %s], want [prov-b prov-a] — the 16-factor default must order by reset-window affinity",
			got[0].ProviderID, got[1].ProviderID)
	}
}

// TestPoolRankingLKGPStickinessLeads: the real pool ranking with a
// real LKGP pair. Two models, identical signals on every factor,
// offered by different providers. With no LKGP record the ranking
// ties and keeps declared order; after recording a success for
// model-b on prov-b, the last-good model must lead — the session
// affinity of the 16-factor pool ranker (RankModelsByBestPair16)
// hoisting the LKGP pair, exactly as the production rankModels
// closure layers it (cmd/codvian-router/main.go: the record from
// LatestHealthyRecord sets SessionProvider/SessionModel).
func TestPoolRankingLKGPStickinessLeads(t *testing.T) {
	signalsFn := func(providerID, model string) scoring.Signals {
		return scoring.Signals{
			LatencyMs:    100,
			SuccessRate:  0.9,
			CostUSDPer1M: 1.0,
			QualityScore: 0.8,
			QuotaHint:    50,
			Attempts:     10,
		}
	}
	offers := func(model string) []scoring.Candidate {
		switch model {
		case "model-a":
			return []scoring.Candidate{{ProviderID: "prov-a", Model: "model-a"}}
		case "model-b":
			return []scoring.Candidate{{ProviderID: "prov-b", Model: "model-b"}}
		}
		return nil
	}
	models := []string{"model-a", "model-b"}
	// inputsFor mirrors the production rankModels closure: the
	// signal-derived 16-factor inputs plus the LKGP layering —
	// the most recent still-healthy last-known-good record sets
	// the session pair for the session-affinity factor.
	inputsFor := func(reg *LKGPRegistry) func(scoring.Candidate) scoring.SixteenInputs {
		return func(c scoring.Candidate) scoring.SixteenInputs {
			in := SixteenInputsFromSignals(signalsFn(c.ProviderID, c.Model))
			if rec, ok := reg.LatestHealthyRecord(signalsFn); ok {
				in.SessionProvider = rec.Provider
				in.SessionModel = rec.Model
			}
			return in
		}
	}

	// Control: no LKGP record — every factor ties, declared order.
	empty := NewLKGPRegistry()
	got := scoring.RankModelsByBestPair16(models, offers, inputsFor(empty), scoring.DefaultWeights16)
	if len(got) != 2 || got[0] != "model-a" || got[1] != "model-b" {
		t.Fatalf("no LKGP record: got %v, want [model-a model-b]", got)
	}

	// Record a success for model-b on prov-b, then the real pool
	// ranking must lead with the last-good model.
	reg := NewLKGPRegistry()
	reg.RecordSuccessFor("model-b", "prov-b", "model-b", "")
	got = scoring.RankModelsByBestPair16(models, offers, inputsFor(reg), scoring.DefaultWeights16)
	if len(got) != 2 || got[0] != "model-b" || got[1] != "model-a" {
		t.Fatalf("LKGP for prov-b/model-b: got %v, want [model-b model-a] — the last-good model must lead the pool ranking", got)
	}
}
