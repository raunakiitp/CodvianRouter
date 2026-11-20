package autorouter

import (
	"strings"
	"testing"
)

// seedOutcomes records n outcomes for (task, model): ok ones with
// latency okMs, failures with latency failMs and reason "upstream_error".
func seedOutcomes(r *Router, task, model string, okN, failN int, okMs, failMs int64) {
	for i := 0; i < okN; i++ {
		r.RecordOutcome(Outcome{Task: task, Model: model, LatencyMs: okMs, OK: true})
	}
	for i := 0; i < failN; i++ {
		r.RecordOutcome(Outcome{Task: task, Model: model, LatencyMs: failMs, OK: false, Reason: "upstream_error"})
	}
}

func TestOutcomeRecorderStats(t *testing.T) {
	r := New(Config{DefaultModel: "m"})
	// Empty task/model outcomes are ignored: nothing to learn about.
	r.RecordOutcome(Outcome{Task: "", Model: "m", OK: true})
	r.RecordOutcome(Outcome{Task: "coding", Model: "", OK: true})
	if got := r.OutcomeStats(); len(got) != 0 {
		t.Fatalf("empty outcomes produced stats: %+v", got)
	}
	seedOutcomes(r, "coding", "model-a", 8, 2, 100, 400)
	seedOutcomes(r, "coding", "model-b", 10, 0, 50, 0)
	stats := r.OutcomeStats()
	if len(stats) != 2 {
		t.Fatalf("stats rows = %d, want 2", len(stats))
	}
	a := stats[0]
	if a.Task != "coding" || a.Model != "model-a" || a.Requests != 10 || a.Successes != 8 || a.Failures != 2 {
		t.Fatalf("model-a stats = %+v", a)
	}
	if a.SuccessRate != 0.8 {
		t.Fatalf("model-a success rate = %v, want 0.8", a.SuccessRate)
	}
	// Mean latency is over ALL requests, successes and failures alike.
	if a.MeanLatencyMs != 160 {
		t.Fatalf("model-a mean latency = %v, want 160", a.MeanLatencyMs)
	}
	if a.FailureReasons["upstream_error"] != 2 {
		t.Fatalf("model-a failure reasons = %v, want upstream_error:2", a.FailureReasons)
	}
	b := stats[1]
	if b.Model != "model-b" || b.SuccessRate != 1.0 || b.MeanLatencyMs != 50 {
		t.Fatalf("model-b stats = %+v", b)
	}
}

func TestComputeSuggestionsIntentRemapSuccess(t *testing.T) {
	r := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentFast: {Model: "model-a", Effort: EffortLow}},
		TierModels:   map[Tier][]string{TierCoding: {"model-a", "model-b"}},
		DefaultModel: "fallback",
	})
	// Incumbent model-a fails 40% on intent/fast; model-b is perfect
	// on the same task. Both sides are above the 10-sample floor.
	seedOutcomes(r, "intent/fast", "model-a", 6, 4, 200, 200)
	seedOutcomes(r, "intent/fast", "model-b", 12, 0, 100, 0)
	suggs := r.Suggestions()
	if len(suggs) != 1 {
		t.Fatalf("suggestions = %+v, want exactly 1 intent_remap", suggs)
	}
	s := suggs[0]
	if s.Kind != SuggestionIntentRemap || s.Task != "intent/fast" || s.CurrentModel != "model-a" || s.SuggestedModel != "model-b" {
		t.Fatalf("suggestion = %+v", s)
	}
	if s.Effort != EffortLow {
		t.Fatalf("suggestion effort = %q, want low (unchanged)", s.Effort)
	}
	if s.Evidence.Current.Requests != 10 || s.Evidence.Challenger.Requests != 12 {
		t.Fatalf("evidence counts = %+v, want real counts 10 and 12", s.Evidence)
	}
	if s.Evidence.Current.SuccessRate != 0.6 || s.Evidence.Challenger.SuccessRate != 1.0 {
		t.Fatalf("evidence rates = %v / %v, want 0.6 / 1.0", s.Evidence.Current.SuccessRate, s.Evidence.Challenger.SuccessRate)
	}
	if !strings.Contains(s.Reason, "model-b") || !strings.Contains(s.Reason, "model-a") {
		t.Fatalf("reason %q must name both models", s.Reason)
	}
}

func TestComputeSuggestionsLatencyRule(t *testing.T) {
	r := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentBalanced: {Model: "model-slow"}},
		DefaultModel: "fallback",
	})
	// Same success rate, but the challenger is 3x faster: the
	// latency rule fires. (Both sides above the sample floor.)
	seedOutcomes(r, "intent/balanced", "model-slow", 10, 0, 900, 0)
	seedOutcomes(r, "intent/balanced", "model-quick", 10, 0, 300, 0)
	suggs := r.Suggestions()
	if len(suggs) != 1 || suggs[0].Kind != SuggestionIntentRemap || suggs[0].SuggestedModel != "model-quick" {
		t.Fatalf("latency-rule suggestions = %+v, want 1 remap to model-quick", suggs)
	}
	// A faster challenger that is LESS reliable does NOT win on latency.
	r2 := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentBalanced: {Model: "model-slow"}},
		DefaultModel: "fallback",
	})
	seedOutcomes(r2, "intent/balanced", "model-slow", 10, 0, 900, 0)
	seedOutcomes(r2, "intent/balanced", "model-quick", 8, 2, 300, 300)
	if got := r2.Suggestions(); len(got) != 0 {
		t.Fatalf("less-reliable faster challenger must not suggest: %+v", got)
	}
}

func TestComputeSuggestionsNeedsEvidence(t *testing.T) {
	r := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentFast: {Model: "model-a"}},
		TierModels:   map[Tier][]string{TierCoding: {"model-a", "model-b"}},
		DefaultModel: "fallback",
	})
	// Challenger below the sample floor: no suggestion, however
	// good its (thin) record looks.
	seedOutcomes(r, "intent/fast", "model-a", 6, 4, 200, 200)
	seedOutcomes(r, "intent/fast", "model-b", 9, 0, 100, 0)
	// Incumbent below the floor too for the demote rule.
	seedOutcomes(r, "coding", "model-a", 5, 4, 100, 100)
	if got := r.Suggestions(); len(got) != 0 {
		t.Fatalf("below-floor evidence must suggest nothing, got %+v", got)
	}
	// No stats at all: no suggestions, never invented ones.
	empty := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentFast: {Model: "model-a"}},
		DefaultModel: "fallback",
	})
	if got := empty.Suggestions(); len(got) != 0 {
		t.Fatalf("no stats must suggest nothing, got %+v", got)
	}
}

func TestComputeSuggestionsTierDemote(t *testing.T) {
	r := New(Config{
		TierModels:   map[Tier][]string{TierCoding: {"model-a", "model-b"}},
		DefaultModel: "fallback",
	})
	// model-a fails 60% of its coding requests (floor 70%);
	// model-b is fine. Only the demote fires.
	seedOutcomes(r, "coding", "model-a", 4, 6, 100, 100)
	seedOutcomes(r, "coding", "model-b", 10, 0, 100, 0)
	suggs := r.Suggestions()
	if len(suggs) != 1 || suggs[0].Kind != SuggestionTierDemote {
		t.Fatalf("suggestions = %+v, want exactly 1 tier_demote", suggs)
	}
	s := suggs[0]
	if s.Task != "coding" || s.CurrentModel != "model-a" || s.SuggestedModel != "" {
		t.Fatalf("demote suggestion = %+v", s)
	}
	if s.Evidence.Challenger != nil {
		t.Fatalf("demote evidence must have no challenger, got %+v", s.Evidence.Challenger)
	}
	if s.Evidence.Current.Requests != 10 || s.Evidence.Current.SuccessRate != 0.4 {
		t.Fatalf("demote evidence = %+v, want 10 requests at 0.4", s.Evidence.Current)
	}
}

func TestApplyAndRevertIntentRemap(t *testing.T) {
	r := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentFast: {Model: "model-a", Effort: EffortLow}},
		DefaultModel: "fallback",
	})
	seedOutcomes(r, "intent/fast", "model-a", 6, 4, 200, 200)
	seedOutcomes(r, "intent/fast", "model-b", 12, 0, 100, 0)
	suggs := r.Suggestions()
	if len(suggs) != 1 {
		t.Fatalf("need 1 suggestion, got %+v", suggs)
	}
	change, err := r.ApplySuggestion(suggs[0].ID)
	if err != nil {
		t.Fatalf("ApplySuggestion: %v", err)
	}
	if change.Kind != SuggestionIntentRemap || change.Previous != "model=model-a effort=low" || change.New != "model=model-b effort=low" {
		t.Fatalf("change = %+v", change)
	}
	// Approve applies: the tier now routes to model-b.
	if m, _ := r.ResolveIntent(IntentFast); m.Model != "model-b" {
		t.Fatalf("after approve, fast maps to %+v, want model-b", m)
	}
	if model, _, ok := r.Resolve("intent/fast", ""); !ok || model != "model-b" {
		t.Fatalf("after approve, Resolve(intent/fast) = (%q, %v)", model, ok)
	}
	// The suggestion is gone from pending (the mapping changed).
	if got := r.Suggestions(); len(got) != 0 {
		t.Fatalf("after approve, pending = %+v, want none", got)
	}
	// Applying the same id again is an error, never a double apply.
	if _, err := r.ApplySuggestion(suggs[0].ID); err == nil {
		t.Fatal("re-applying a spent suggestion must error")
	}
	// The log records the change.
	log := r.SuggestionLog()
	if len(log) != 1 || log[0].ID != change.ID || log[0].Reverted {
		t.Fatalf("log = %+v", log)
	}
	// Revert restores the previous mapping.
	rev, err := r.RevertChange(change.ID)
	if err != nil {
		t.Fatalf("RevertChange: %v", err)
	}
	if !rev.Reverted {
		t.Fatalf("reverted change = %+v", rev)
	}
	if m, _ := r.ResolveIntent(IntentFast); m.Model != "model-a" || m.Effort != EffortLow {
		t.Fatalf("after revert, fast maps to %+v, want model-a/low", m)
	}
	// The suggestion is pending again (the evidence still says so).
	if got := r.Suggestions(); len(got) != 1 {
		t.Fatalf("after revert, pending = %+v, want the suggestion back", got)
	}
	// A second revert is an error, never a double revert.
	if _, err := r.RevertChange(change.ID); err == nil {
		t.Fatal("re-reverting must error")
	}
	// Unknown ids error honestly.
	if _, err := r.ApplySuggestion("intent_remap:intent/fast:nope:nope"); err == nil {
		t.Fatal("applying an unknown suggestion must error")
	}
	if _, err := r.RevertChange("chg-999"); err == nil {
		t.Fatal("reverting an unknown change must error")
	}
}

func TestApplyAndRevertTierDemote(t *testing.T) {
	r := New(Config{
		TierModels:   map[Tier][]string{TierCoding: {"model-a", "model-b"}},
		DefaultModel: "fallback",
	})
	seedOutcomes(r, "coding", "model-a", 4, 6, 100, 100)
	seedOutcomes(r, "coding", "model-b", 10, 0, 100, 0)
	suggs := r.Suggestions()
	if len(suggs) != 1 || suggs[0].Kind != SuggestionTierDemote {
		t.Fatalf("need 1 demote, got %+v", suggs)
	}
	change, err := r.ApplySuggestion(suggs[0].ID)
	if err != nil {
		t.Fatalf("ApplySuggestion: %v", err)
	}
	if got := r.ModelsForTier(TierCoding); len(got) != 1 || got[0] != "model-b" {
		t.Fatalf("after demote, coding candidates = %v, want [model-b]", got)
	}
	if _, err := r.RevertChange(change.ID); err != nil {
		t.Fatalf("RevertChange: %v", err)
	}
	if got := r.ModelsForTier(TierCoding); len(got) != 2 || got[1] != "model-a" {
		t.Fatalf("after revert, coding candidates = %v, want model-a back", got)
	}
}
