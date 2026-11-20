// A/B evaluation of feedback routing (item 115).
//
// The calibrated heuristic (CalibratedNudge) is deliberately small
// and unverified — docs/feedback-calibration.md says it "needs real
// traffic and an A/B-style comparison before any stronger claim".
// This file is that comparison, run offline on the recorded local
// traffic in feedback.db:
//
//   - Train: signals recorded up to a cutoff are frozen into the
//     per-tier nudges the heuristic would have applied (decay
//     measured at the cutoff, so the policy is exactly the one the
//     live router would have had then).
//   - Test: signaled decisions recorded AFTER the cutoff, each with
//     the raw per-tier scores stored at serve time
//     (tier_scores_json — metadata only, no prompts). For each:
//     the baseline policy picks argmax(raw scores); the feedback
//     policy picks argmax(raw scores + train nudges). Decisions
//     whose served tier is not the raw argmax are EXCLUDED — the
//     router served a threshold fallback or a restricted candidate
//     set there, so neither policy's plain argmax describes what
//     happened.
//   - Outcome: the recorded signal's points for the SERVED tier
//     (+1 accept, -0.5 edit, -1 discard/regenerate). When the
//     feedback policy would have picked a different tier, the
//     served outcome is the outcome AVOIDED: avoiding a bad outcome
//     (negative points) scores positive, avoiding a good outcome
//     scores negative.
//
// Verdict: feedback wins only when the avoided-outcome points per
// flip are positive on enough flipped decisions; otherwise the
// baseline wins and learning stays OFF. A tie keeps the baseline
// (do no harm). No test signals, or no scored decisions, is an
// honest no-evidence result — never a win for either side.
//
// HONEST LIMIT — READ THIS. The evaluation can only observe the
// outcome of the tier actually served. It cannot measure what the
// alternative tier would have done for the same prompt: a flip
// away from a bad outcome is evidence the heuristic avoids tiers
// users rated badly, not proof the destination tier would have
// done better. The numbers below are evidence, not proof, and the
// verdict never claims more than "on this recorded traffic".
// Decisions recorded before tier scores existed (empty
// tier_scores_json) are excluded, never guessed.
package feedback

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
)

// EvalVerdict values for EvalResult.Verdict.
const (
	// EvalNoEvidence: no signaled test decisions to judge on.
	EvalNoEvidence = "no-evidence"
	// EvalNoFlip: both policies agree on every evaluable decision.
	EvalNoFlip = "no-flip"
	// EvalFeedbackWins: the feedback policy avoided more bad
	// outcomes than good ones per flip.
	EvalFeedbackWins = "feedback"
	// EvalBaselineWins: the feedback policy avoided more good
	// outcomes than bad ones per flip.
	EvalBaselineWins = "baseline"
	// EvalTie: flips are exactly outcome-neutral — keep the
	// baseline (do no harm).
	EvalTie = "tie"
)

// evalRowCap bounds how many signaled decisions one evaluation
// scans. Local feedback stores are small; the cap exists so a
// pathological store cannot make the dashboard or the CLI hang.
// When the cap bites, the result says so.
const evalRowCap = 100000

// ScoredDecision is one signaled test decision: the tier actually
// served, its stored raw scores, and the recorded outcome.
type ScoredDecision struct {
	DecisionID string
	Tier       string // tier actually served
	Scores     map[string]float64
	Signal     Signal
	Points     float64 // SignalPoints[Signal]
	SignalTS   time.Time
}

// EvalResult is one A/B comparison outcome.
type EvalResult struct {
	Cutoff            time.Time          `json:"cutoff"`
	TrainSignals      int                `json:"train_signals"`
	TestDecisions     int                `json:"test_decisions"`
	Evaluated         int                `json:"evaluated"`
	Excluded          int                `json:"excluded"`
	Flips             int                `json:"flips"`
	FlipsAwayFromBad  int                `json:"flips_away_from_bad"`
	FlipsAwayFromGood int                `json:"flips_away_from_good"`
	AvoidedPoints     float64            `json:"avoided_points"`
	AvoidedPerFlip    float64            `json:"avoided_per_flip"`
	TrainNudges       map[string]float64 `json:"train_nudges"`
	Verdict           string             `json:"verdict"`
	Note              string             `json:"note"`
	Capped            bool               `json:"capped"`
}

// argmaxTier returns the tier with the highest effective score
// (raw score + nudge, clamped to [0, 1] exactly as
// autorouter.Classify's effective() does). Ties break toward the
// earlier tier in AllTiers order (strict > comparison), matching
// Classify's best-tier loop.
func argmaxTier(scores map[string]float64, nudges map[string]float64) string {
	best := ""
	var bestScore float64 = -1
	for _, tier := range autorouter.AllTiers {
		s := scores[string(tier)] + nudges[string(tier)]
		if s < 0 {
			s = 0
		}
		if s > 1 {
			s = 1
		}
		if s > bestScore {
			bestScore = s
			best = string(tier)
		}
	}
	return best
}

// evaluateScored is the pure A/B core (no I/O): train nudges from
// pre-cutoff signals, test on scored post-cutoff decisions.
func evaluateScored(train map[string][]TimedSignal, test []ScoredDecision, cutoff time.Time) EvalResult {
	res := EvalResult{Cutoff: cutoff, TrainNudges: map[string]float64{}}
	nudges := map[string]float64{}
	for _, tier := range autorouter.AllTiers {
		nudge, _, _ := CalibratedNudge(train[string(tier)])
		nudges[string(tier)] = nudge
		res.TrainNudges[string(tier)] = math.Round(nudge*1000) / 1000
	}
	for _, tier := range autorouter.AllTiers {
		res.TrainSignals += len(train[string(tier)])
	}
	res.TestDecisions = len(test)
	for _, d := range test {
		if len(d.Scores) == 0 {
			res.Excluded++
			continue
		}
		// The served tier must be the plain argmax, or the
		// decision was a threshold fallback / restricted
		// candidate set and neither policy describes it.
		if argmaxTier(d.Scores, nil) != d.Tier {
			res.Excluded++
			continue
		}
		res.Evaluated++
		feedbackTier := argmaxTier(d.Scores, nudges)
		if feedbackTier == d.Tier {
			continue
		}
		res.Flips++
		// The served outcome is the outcome avoided.
		if d.Points < 0 {
			res.FlipsAwayFromBad++
		} else if d.Points > 0 {
			res.FlipsAwayFromGood++
		}
		res.AvoidedPoints += -d.Points
	}
	if res.Flips > 0 {
		res.AvoidedPerFlip = math.Round(res.AvoidedPoints/float64(res.Flips)*1000) / 1000
	}
	switch {
	case res.TestDecisions == 0:
		res.Verdict = EvalNoEvidence
		res.Note = "no signals recorded after the cutoff — nothing to judge the policies on"
	case res.Evaluated == 0:
		res.Verdict = EvalNoEvidence
		res.Note = "no evaluable test decisions — none have recorded tier scores (scores are stored for decisions served since the A/B slice; older decisions are excluded, never guessed)"
	case res.Flips == 0:
		res.Verdict = EvalNoFlip
		res.Note = "both policies agree on every evaluable decision — learning would change nothing on this traffic"
	case res.AvoidedPerFlip > 0:
		res.Verdict = EvalFeedbackWins
		res.Note = fmt.Sprintf("feedback avoids %.3f outcome points per flipped decision (%d flips: %d away from bad outcomes, %d away from good ones) — on this recorded traffic the heuristic helps", res.AvoidedPerFlip, res.Flips, res.FlipsAwayFromBad, res.FlipsAwayFromGood)
	case res.AvoidedPerFlip < 0:
		res.Verdict = EvalBaselineWins
		res.Note = fmt.Sprintf("feedback avoids %.3f outcome points per flipped decision (%d flips: %d away from bad outcomes, %d away from good ones) — on this recorded traffic the heuristic hurts; keep learning off", res.AvoidedPerFlip, res.Flips, res.FlipsAwayFromBad, res.FlipsAwayFromGood)
	default:
		res.Verdict = EvalTie
		res.Note = fmt.Sprintf("flips are exactly outcome-neutral (%d flips) — no evidence either way; keep the baseline", res.Flips)
	}
	return res
}

// evalRowsQuery loads every signaled decision for the A/B
// comparison, and evalSignalTimesQuery loads every signal
// timestamp for the default 70/30 cutoff. Both order by (ts,
// id): signals recorded in the same millisecond break the tie
// by insertion id (CHECKLIST 263–264 — the same fix 251–252
// applied to TrainingPairs), so two runs over the same store
// always see the same row order. ORDER BY ts alone leaves
// same-millisecond rows in unspecified SQLite order: the
// verdict sums are order-independent, but the row order is
// observable at the evalRowCap boundary (which test decisions
// get capped), so the order must be total, not just by
// timestamp. (For the times query the tiebreak is belt and
// braces — only ts values are returned and Evaluate sorts
// them in Go — but the retrieval order is the same total
// order either way.)
const evalRowsQuery = `
	SELECT d.tier, d.tier_scores_json, e.signal, e.ts
	FROM feedback_events e
	JOIN feedback_decisions d ON d.decision_id = e.decision_id
	ORDER BY e.ts ASC, e.id ASC`

const evalSignalTimesQuery = `SELECT ts FROM feedback_events ORDER BY ts ASC, id ASC`

// EvaluateAt runs the A/B comparison with an explicit train/test
// cutoff: signals at or before the cutoff train the nudges,
// signaled decisions after it are the test set.
func (s *Store) EvaluateAt(ctx context.Context, cutoff time.Time) (EvalResult, error) {
	rows, err := s.db.QueryContext(ctx, evalRowsQuery)
	if err != nil {
		return EvalResult{}, fmt.Errorf("feedback: load evaluation rows: %w", err)
	}
	defer rows.Close()
	train := map[string][]TimedSignal{}
	for _, t := range autorouter.AllTiers {
		train[string(t)] = nil
	}
	var test []ScoredDecision
	capped := false
	for rows.Next() {
		var tier, scoresJSON, signal string
		var tsMs int64
		if err := rows.Scan(&tier, &scoresJSON, &signal, &tsMs); err != nil {
			return EvalResult{}, fmt.Errorf("feedback: load evaluation rows: %w", err)
		}
		sig := Signal(signal)
		points, ok := SignalPoints[sig]
		if !ok {
			continue
		}
		ts := time.UnixMilli(tsMs).UTC()
		if !ts.After(cutoff) {
			// Train: decay measured at the cutoff — the policy
			// as it stood when the test period began.
			age := cutoff.Sub(ts)
			if age < 0 {
				age = 0
			}
			train[tier] = append(train[tier], TimedSignal{Points: points, Age: age})
			continue
		}
		if len(test) >= evalRowCap {
			capped = true
			continue
		}
		test = append(test, ScoredDecision{
			Tier:     tier,
			Scores:   parseTierScores(scoresJSON),
			Signal:   sig,
			Points:   points,
			SignalTS: ts,
		})
	}
	if err := rows.Err(); err != nil {
		return EvalResult{}, fmt.Errorf("feedback: load evaluation rows: %w", err)
	}
	res := evaluateScored(train, test, cutoff)
	res.Capped = capped
	if capped {
		res.Note += fmt.Sprintf(" (test set capped at %d decisions)", evalRowCap)
	}
	return res, nil
}

// Evaluate runs the A/B comparison with the default cutoff: the
// timestamp that splits the recorded signals 70/30 by count
// (train/test). With fewer than 2 signals there is nothing to
// split — the cutoff is now and the result is no-evidence.
func (s *Store) Evaluate(ctx context.Context) (EvalResult, error) {
	var times []int64
	rows, err := s.db.QueryContext(ctx, evalSignalTimesQuery)
	if err != nil {
		return EvalResult{}, fmt.Errorf("feedback: load signal times: %w", err)
	}
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			rows.Close()
			return EvalResult{}, fmt.Errorf("feedback: load signal times: %w", err)
		}
		times = append(times, ts)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EvalResult{}, fmt.Errorf("feedback: load signal times: %w", err)
	}
	if len(times) < 2 {
		return s.EvaluateAt(ctx, s.now())
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	cutoff := time.UnixMilli(times[len(times)*7/10]).UTC()
	return s.EvaluateAt(ctx, cutoff)
}
