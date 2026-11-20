// B2-14 pinning tests (T-B2-524-TEST): FactorTaskFit consumption
// contract for the live-intelligence scoring hook.
//
// The ranking closure in cmd/codvian-router/main.go sets
// TaskFitScore/TaskFitKnown from modelsync.Intelligence.TaskFit and
// only on a hit — a miss leaves Known false. FactorTaskFit must
// therefore vote the live score as-is when known, and fall through
// to the tag-Jaccard computation (0.5 with no tags) when unknown.
// A known score is a real measured value, never an unknown — even
// a 0 from an override must win over the tags.
package scoring

import (
	"math"
	"testing"
)

// TestB2_524_FactorTaskFitKnownWins pins that a known live score
// votes as-is, even against disjoint tags.
func TestB2_524_FactorTaskFitKnownWins(t *testing.T) {
	if got := FactorTaskFit(SixteenInputs{
		TaskFitKnown: true, TaskFitScore: 0.77,
		TaskTags: []string{"coding"}, ModelTags: []string{"review"},
	}); got != 0.77 {
		t.Fatalf("known 0.77 with disjoint tags = %v, want 0.77", got)
	}
	// A known 0 (an override) is a real measured 0, not an unknown.
	if got := FactorTaskFit(SixteenInputs{
		TaskFitKnown: true, TaskFitScore: 0,
		TaskTags: []string{"coding"}, ModelTags: []string{"coding"},
	}); got != 0 {
		t.Fatalf("known 0 with identical tags = %v, want 0", got)
	}
}

// TestB2_524_FactorTaskFitUnknownFallsBack pins the fallback: with
// no live signal the factor is the tag Jaccard, 0.5 with no tags.
func TestB2_524_FactorTaskFitUnknownFallsBack(t *testing.T) {
	// |{a,b} ∩ {b,c}| / |{a,b,c}| = 1/3.
	if got := FactorTaskFit(SixteenInputs{
		TaskTags: []string{"a", "b"}, ModelTags: []string{"b", "c"},
	}); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Fatalf("unknown, tags a,b vs b,c = %v, want 1/3", got)
	}
	if got := FactorTaskFit(SixteenInputs{
		TaskTags: []string{"coding"}, ModelTags: []string{"coding"},
	}); got != 1 {
		t.Fatalf("unknown, identical tags = %v, want 1", got)
	}
	if got := FactorTaskFit(SixteenInputs{
		TaskTags: []string{"coding"}, ModelTags: []string{"review"},
	}); got != 0 {
		t.Fatalf("unknown, disjoint tags = %v, want 0", got)
	}
	if got := FactorTaskFit(SixteenInputs{}); got != 0.5 {
		t.Fatalf("unknown, no tags = %v, want 0.5 neutral", got)
	}
}

// TestB2_524_FactorTaskFitKnownInvalidFallsThrough pins that a
// known-but-unusable score (NaN, out of range) never votes — it
// falls through to the Jaccard computation like an unknown.
func TestB2_524_FactorTaskFitKnownInvalidFallsThrough(t *testing.T) {
	if got := FactorTaskFit(SixteenInputs{
		TaskFitKnown: true, TaskFitScore: math.NaN(),
		TaskTags: []string{"coding"}, ModelTags: []string{"review"},
	}); got != 0 {
		t.Fatalf("known NaN with disjoint tags = %v, want 0 (Jaccard)", got)
	}
	if got := FactorTaskFit(SixteenInputs{
		TaskFitKnown: true, TaskFitScore: 1.5,
		TaskTags: []string{"coding"}, ModelTags: []string{"coding"},
	}); got != 1 {
		t.Fatalf("known 1.5 with identical tags = %v, want 1 (Jaccard)", got)
	}
}
