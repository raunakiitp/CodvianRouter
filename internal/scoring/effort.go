// Effort-move preference (CHECKLIST 255, research-log entries
// 230–231, Magpie v0.1.4405): a routing-group entry is (model,
// effort), not model alone — the same model at different reasoning
// efforts is a distinct cost/latency/quality point. The scorer may
// therefore move effort before switching model: when the current
// (model, effort) must be left, changing the effort on the same
// model is preferred over switching to a different model.
//
// Why prefer the effort move: switching models pays a cost the raw
// score does not capture — the prompt cache is rebuilt, the
// serving path is cold, and the answer's character changes. An
// effort move on the same model keeps the cache shape and the
// model, and only trades reasoning depth. So a same-model
// different-effort candidate wins when its score is at least as
// good as the best model switch, within a small tolerance
// (EffortMoveTolerance): the tolerance is the price of staying on
// the model, and it is deliberately small — an effort move that is
// clearly worse on the measured signals still loses, so the
// preference can never override real evidence by much.
//
// BestEffortMove is the primitive for callers that know the current
// (model, effort): the auto-router and the strategy hook rank
// candidates, but only a caller that knows what is serving now can
// ask "should I move effort, or switch models". The hook itself
// cannot apply this preference — it ranks a chain with no current
// — so the preference lives here as a pure function, not in Rank.
package scoring

// EffortMoveTolerance is how far below the best model-switch score
// a same-model effort move may sit and still win: 0.05 on the
// unitless score scale. It is a policy constant, not a measured
// property — the documented price of staying on the model (cache
// shape + warm path + same answer character) rather than switching.
// A move more than this worse than the best switch loses.
const EffortMoveTolerance = 0.05

// BestEffortMove returns the best same-model, different-effort
// candidate to move to from current, and whether moving effort
// beats switching model.
//
//   - No candidate shares current's model at a different effort:
//     (Candidate{}, false) — there is no effort move to make.
//   - The best effort move E wins when its score is at least the
//     best alternative's score minus EffortMoveTolerance, where
//     the best alternative is the better of the best
//     different-model candidate and staying on current itself:
//     the move must be at least as good (within tolerance) as
//     both switching models and standing still. A move that is
//     clearly worse than either loses, so the preference never
//     overrides real evidence by more than the tolerance.
//   - Ties between effort moves keep input order (stable rank).
//
// Candidates and signals are keyed by the full candidate
// (provider, model, effort), so each effort level is scored by
// its own signals. A candidate with no signals entry scores by
// quota hint only (see Scorer.Score). The result is always one of
// the input candidates, never invented.
func BestEffortMove(current Candidate, candidates []Candidate, signals map[Candidate]Signals, mode Mode) (Candidate, bool) {
	scorer := NewScorer(mode)
	scoreOf := func(c Candidate) float64 {
		s, _ := signals[c]
		return scorer.Score(c, s)
	}
	var bestEffort Candidate
	bestEffortScore := 0.0
	foundEffort := false
	bestOtherScore := scoreOf(current)
	foundOther := false
	for _, c := range candidates {
		if c.Model == current.Model && c.Effort != current.Effort {
			if s := scoreOf(c); !foundEffort || s > bestEffortScore {
				bestEffort, bestEffortScore, foundEffort = c, s, true
			}
			continue
		}
		if c.Model != current.Model {
			foundOther = true
			if s := scoreOf(c); s > bestOtherScore {
				bestOtherScore = s
			}
		}
	}
	if !foundEffort {
		return Candidate{}, false
	}
	// No different-model candidate exists: the choice is move vs
	// stay, and the move wins only when it actually beats staying
	// (the tolerance does not apply — there is no switch to prefer
	// over, so a sideways move is not a preference, it is churn).
	if !foundOther {
		if bestEffortScore > scoreOf(current) {
			return bestEffort, true
		}
		return Candidate{}, false
	}
	if bestEffortScore >= bestOtherScore-EffortMoveTolerance {
		return bestEffort, true
	}
	return Candidate{}, false
}
