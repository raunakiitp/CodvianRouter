package autorouter

import (
	"strings"
	"testing"
)

// stubLearned is a LearnedPredictor with fixed per-tier win
// probabilities for composition tests.
type stubLearned struct {
	probs  map[Tier]float32
	active bool
}

func (s stubLearned) LearnedWinProbs(map[string]float32) (map[Tier]float32, bool) {
	return s.probs, s.active
}

// TestLearnedInactiveLeavesRoutingUnchanged: the item-115
// no-evidence rule as a test — a predictor that reports
// inactive (off / learning / uncalibrated) must change NOTHING,
// even when it returns extreme probabilities. The decision must
// be byte-identical to the no-predictor decision except for the
// ID and the measured latency.
func TestLearnedInactiveLeavesRoutingUnchanged(t *testing.T) {
	prompts := []string{
		"Write a Python function to sort a list",
		"Tell me a funny joke about cats",
		"What is the boiling point of water at sea level",
		"Solve this step by step: a train travels 120 km in 2 hours, what is its speed",
		"blorp zibble wumpus",
	}
	for _, prompt := range prompts {
		plain := testRouter()
		base := plain.Classify("auto", prompt, 0)
		wired := testRouter()
		wired.SetLearnedPredictor(stubLearned{
			probs:  map[Tier]float32{TierCoding: 1, TierChat: 0, TierReasoning: 0, TierQuickQA: 0},
			active: false,
		})
		got := wired.Classify("auto", prompt, 0)
		if got.Tier != base.Tier || got.Confidence != base.Confidence || got.Fallback != base.Fallback || got.Reason != base.Reason {
			t.Errorf("prompt %q: inactive predictor changed routing: got %+v, want %+v", prompt, got, base)
		}
		if len(got.LearnedWinProb) != 0 || got.LearnedActive {
			t.Errorf("prompt %q: inactive predictor left LearnedWinProb=%v LearnedActive=%v, want empty/false", prompt, got.LearnedWinProb, got.LearnedActive)
		}
	}
}

// TestLearnedActiveFlipsOnlyBorderlineDecision mirrors the
// nudge flip test: a +0.05 learned contribution (win prob 1.0)
// on the raw winner must push a decision just below the
// threshold over it — and a threshold 0.06 above the raw score
// must still fall back, proving the contribution is bounded.
func TestLearnedActiveFlipsOnlyBorderlineDecision(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	prompt := "Tell me a funny joke about cats"
	raw := r.Classify("auto", prompt, 0)
	if raw.Tier != TierChat {
		t.Fatalf("setup: raw tier = %s, want chat", raw.Tier)
	}
	r.SetLearnedPredictor(stubLearned{
		probs:  map[Tier]float32{TierCoding: 0.5, TierChat: 1.0, TierReasoning: 0.5, TierQuickQA: 0.5},
		active: true,
	})
	// Threshold 0.02 above the raw score: the learned +0.05 on
	// chat must flip it back over.
	threshold := raw.Confidence + 0.02
	flipped := r.Classify("auto", prompt, threshold)
	if flipped.Fallback {
		t.Errorf("learned decision still falls back at threshold %.3f: reason %q", threshold, flipped.Reason)
	}
	if flipped.Tier != TierChat {
		t.Errorf("learned tier = %s, want chat", flipped.Tier)
	}
	if got := flipped.LearnedWinProb[string(TierChat)]; got != 1.0 {
		t.Errorf("LearnedWinProb[chat] = %v, want 1.0", got)
	}
	if !flipped.LearnedActive {
		t.Error("LearnedActive = false, want true")
	}
	if !strings.Contains(flipped.Reason, "learned win-prob 1.00") {
		t.Errorf("reason %q does not name the applied win probability", flipped.Reason)
	}
	// Threshold 0.06 above the raw score: even the maximum
	// learned contribution (+0.05) cannot reach it.
	far := r.Classify("auto", prompt, raw.Confidence+0.06)
	if !far.Fallback {
		t.Errorf("learned decision did not fall back at threshold %.3f (raw %.3f): the bound must hold", raw.Confidence+0.06, raw.Confidence)
	}
}

// TestLearnedCannotPromoteFarBelowTier: a tier scoring far below
// the winner stays losing even with win probability 1.0.
func TestLearnedCannotPromoteFarBelowTier(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	prompt := "Write a Python function to sort a list"
	raw := r.Classify("auto", prompt, 0)
	if raw.Tier != TierCoding {
		t.Fatalf("setup: raw tier = %s, want coding", raw.Tier)
	}
	r.SetLearnedPredictor(stubLearned{
		probs:  map[Tier]float32{TierCoding: 0.0, TierChat: 1.0, TierReasoning: 0.5, TierQuickQA: 0.5},
		active: true,
	})
	got := r.Classify("auto", prompt, 0)
	if got.Tier != TierCoding {
		t.Errorf("learned tier = %s, want coding (win-prob must not overturn a large raw gap)", got.Tier)
	}
}

// TestSetLearnedPredictorNilDisablesLearned: un-wiring the
// predictor returns routing to exactly the raw decision.
func TestSetLearnedPredictorNilDisablesLearned(t *testing.T) {
	r := testRouter()
	r.SetLearnedPredictor(stubLearned{
		probs:  map[Tier]float32{TierCoding: 1.0},
		active: true,
	})
	r.SetLearnedPredictor(nil)
	dec := r.Classify("auto", "Tell me a funny joke about cats", 0)
	if len(dec.LearnedWinProb) != 0 || dec.LearnedActive {
		t.Errorf("after un-wiring: LearnedWinProb=%v LearnedActive=%v, want empty/false", dec.LearnedWinProb, dec.LearnedActive)
	}
}
