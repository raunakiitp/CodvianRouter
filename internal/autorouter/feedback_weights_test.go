package autorouter

import (
	"strings"
	"testing"
)

// stubWeights is a WeightsProvider with fixed per-tier weights for
// tests: it proves the nudge moves routing without a database.
type stubWeights map[Tier]TierWeight

func (s stubWeights) LearnedWeights() map[Tier]TierWeight { return s }

func TestDecisionIDsUnique(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		dec := r.Classify("auto", "Write a Python function to sort a list", 0)
		if dec.ID == "" {
			t.Fatal("decision has no ID")
		}
		if !strings.HasPrefix(dec.ID, "dec-") {
			t.Errorf("decision ID %q lacks the dec- prefix", dec.ID)
		}
		if seen[dec.ID] {
			t.Fatalf("duplicate decision ID %q", dec.ID)
		}
		seen[dec.ID] = true
	}
	// Two routers built in sequence mint different ID spaces (the
	// boot-nanos prefix): an id from one process can never collide
	// with an id from another.
	r2 := New(Config{DefaultModel: "model-x"})
	if r2.Classify("auto", "hello", 0).ID == r.Classify("auto", "hello", 0).ID {
		t.Error("two routers minted the same decision ID")
	}
}

func TestNoProviderMeansNoNudge(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	dec := r.Classify("auto", "Write a Python function to sort a list", 0)
	if len(dec.LearnedNudge) != 0 || len(dec.LearnedSamples) != 0 {
		t.Errorf("no provider: LearnedNudge=%v LearnedSamples=%v, want both empty", dec.LearnedNudge, dec.LearnedSamples)
	}
	if strings.Contains(dec.Reason, "nudge") {
		t.Errorf("no provider: reason %q mentions a nudge", dec.Reason)
	}
}

// TestLearnedNudgeFlipsBorderlineDecision is the prototype's core
// claim: a bounded positive nudge can move a borderline decision
// across the threshold, and the decision reports the nudge and
// the sample count behind it.
func TestLearnedNudgeFlipsBorderlineDecision(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	// "Tell me a funny joke about cats" scores ~0.83 on chat (a
	// real mid-range match, not an exact utterance): a threshold
	// just above it makes the raw decision fall back, and a +0.05
	// nudge on chat must push it back over.
	prompt := "Tell me a funny joke about cats"
	raw := r.Classify("auto", prompt, 0)
	if raw.Tier != TierChat {
		t.Fatalf("setup: raw tier = %s, want chat", raw.Tier)
	}
	threshold := raw.Confidence + 0.02
	if threshold > 1 {
		t.Fatalf("setup: raw confidence %.3f too close to 1 to place a threshold above it", raw.Confidence)
	}
	fb := r.Classify("auto", prompt, threshold)
	if !fb.Fallback {
		t.Fatalf("raw decision at threshold %.3f: fallback=%v, want fallback (test setup)", threshold, fb.Fallback)
	}
	winner := Tier("chat")
	// The raw winner for this prompt is chat; nudge it, not the
	// fallback tier, so the flip is the learned preference.
	if fb.Tier != TierReasoning {
		t.Fatalf("setup: fallback tier = %s, want reasoning", fb.Tier)
	}
	r.SetWeightsProvider(stubWeights{
		winner: {Weight: 0.05, Samples: 12, Accepts: 12},
	})
	nudged := r.Classify("auto", prompt, threshold)
	if nudged.Fallback {
		t.Errorf("nudged decision still falls back: reason %q", nudged.Reason)
	}
	if nudged.Tier != winner {
		t.Errorf("nudged tier = %s, want %s", nudged.Tier, winner)
	}
	if got := nudged.LearnedNudge[string(winner)]; got != 0.05 {
		t.Errorf("LearnedNudge[chat] = %v, want 0.05", got)
	}
	if got := nudged.LearnedSamples[string(winner)]; got != 12 {
		t.Errorf("LearnedSamples[chat] = %v, want 12", got)
	}
	if !strings.Contains(nudged.Reason, "learned nudge +0.050") {
		t.Errorf("reason %q does not name the applied nudge", nudged.Reason)
	}
}

// TestLearnedNudgeCannotPromoteFarBelowTier: a -0.05/+0.05 nudge
// must not overturn a large raw gap — a tier scoring far below the
// winner stays losing even with the maximum nudge.
func TestLearnedNudgeCannotPromoteFarBelowTier(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	prompt := "Write a Python function to sort a list"
	raw := r.Classify("auto", prompt, 0)
	if raw.Tier != TierCoding {
		t.Fatalf("setup: raw tier = %s, want coding", raw.Tier)
	}
	// Give chat the maximum positive nudge and coding the maximum
	// negative one; the raw gap (coding ~0.8 vs chat ~0.2) dwarfs
	// the 0.10 total swing.
	r.SetWeightsProvider(stubWeights{
		TierChat:   {Weight: 0.05, Samples: 50, Accepts: 50},
		TierCoding: {Weight: -0.05, Samples: 50, Discards: 50},
	})
	nudged := r.Classify("auto", prompt, 0)
	if nudged.Tier != TierCoding {
		t.Errorf("nudged tier = %s, want coding (nudge must not overturn a large raw gap)", nudged.Tier)
	}
}

// TestSetWeightsProviderNilDisablesLearning: un-wiring the provider
// returns routing to exactly the raw decision.
func TestSetWeightsProviderNilDisablesLearning(t *testing.T) {
	r := New(Config{DefaultModel: "model-x"})
	r.SetWeightsProvider(stubWeights{TierChat: {Weight: 0.05, Samples: 10, Accepts: 10}})
	r.SetWeightsProvider(nil)
	dec := r.Classify("auto", "Tell me a funny joke about cats", 0)
	if len(dec.LearnedNudge) != 0 {
		t.Errorf("after un-wiring: LearnedNudge=%v, want empty", dec.LearnedNudge)
	}
}
