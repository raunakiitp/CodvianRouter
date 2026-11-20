package feedback

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// scores builds a score map where the named tier is the argmax
// (0.9) and the rest sit below (0.3), unless overridden.
func scores(winner string, overrides map[string]float64) map[string]float64 {
	m := map[string]float64{"coding": 0.3, "chat": 0.3, "reasoning": 0.3, "quick-qa": 0.3}
	m[winner] = 0.9
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

func TestEvaluateFeedbackWins(t *testing.T) {
	// Train: 10 discards on reasoning (nudge -0.05), 10 accepts
	// on quick-qa (nudge +0.05). Test: decisions served on
	// reasoning with quick-qa a hair behind (0.87 vs 0.90):
	// feedback flips them to quick-qa, avoiding the discards.
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	train := map[string][]TimedSignal{}
	for i := 0; i < 10; i++ {
		train["reasoning"] = append(train["reasoning"], TimedSignal{Points: -1})
		train["quick-qa"] = append(train["quick-qa"], TimedSignal{Points: 1})
	}
	test := []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.87}), Signal: SignalDiscard, Points: -1},
		{DecisionID: "d2", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.87}), Signal: SignalDiscard, Points: -1},
		{DecisionID: "d3", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.87}), Signal: SignalRegenerate, Points: -1},
		// Not a flip: quick-qa is far behind, the nudge cannot move it.
		{DecisionID: "d4", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.3}), Signal: SignalAccept, Points: 1},
	}
	res := evaluateScored(train, test, cutoff)
	if res.Verdict != EvalFeedbackWins {
		t.Fatalf("verdict = %q, want %q (note: %s)", res.Verdict, EvalFeedbackWins, res.Note)
	}
	if res.Flips != 3 || res.FlipsAwayFromBad != 3 || res.FlipsAwayFromGood != 0 {
		t.Errorf("flips = %d (bad %d, good %d), want 3 (3, 0)", res.Flips, res.FlipsAwayFromBad, res.FlipsAwayFromGood)
	}
	if res.AvoidedPerFlip != 1 {
		t.Errorf("avoided per flip = %v, want 1", res.AvoidedPerFlip)
	}
	if res.Evaluated != 4 || res.Excluded != 0 {
		t.Errorf("evaluated/excluded = %d/%d, want 4/0", res.Evaluated, res.Excluded)
	}
	if res.TrainNudges["reasoning"] != -0.05 || res.TrainNudges["quick-qa"] != 0.05 {
		t.Errorf("train nudges = %v, want reasoning -0.05 quick-qa +0.05", res.TrainNudges)
	}
}

func TestEvaluateBaselineWins(t *testing.T) {
	// Train says reasoning is bad (-0.05) and quick-qa good
	// (+0.05), but the test decisions served on reasoning were
	// ACCEPTED: feedback would flip away from good outcomes.
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	train := map[string][]TimedSignal{}
	for i := 0; i < 10; i++ {
		train["reasoning"] = append(train["reasoning"], TimedSignal{Points: -1})
		train["quick-qa"] = append(train["quick-qa"], TimedSignal{Points: 1})
	}
	test := []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.87}), Signal: SignalAccept, Points: 1},
		{DecisionID: "d2", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.87}), Signal: SignalAccept, Points: 1},
	}
	res := evaluateScored(train, test, cutoff)
	if res.Verdict != EvalBaselineWins {
		t.Fatalf("verdict = %q, want %q (note: %s)", res.Verdict, EvalBaselineWins, res.Note)
	}
	if res.FlipsAwayFromGood != 2 || res.AvoidedPerFlip != -1 {
		t.Errorf("flips away from good = %d, avoided/flip = %v, want 2 / -1", res.FlipsAwayFromGood, res.AvoidedPerFlip)
	}
}

func TestEvaluateNoEvidenceAndNoFlip(t *testing.T) {
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	// No test decisions at all.
	if res := evaluateScored(map[string][]TimedSignal{}, nil, cutoff); res.Verdict != EvalNoEvidence {
		t.Errorf("empty test: verdict = %q, want no-evidence", res.Verdict)
	}
	// Test decisions exist but none have scores (pre-A/B rows).
	res := evaluateScored(map[string][]TimedSignal{}, []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Signal: SignalAccept, Points: 1},
	}, cutoff)
	if res.Verdict != EvalNoEvidence || res.Excluded != 1 {
		t.Errorf("unscored: verdict = %q excluded = %d, want no-evidence / 1", res.Verdict, res.Excluded)
	}
	// Policies agree: the nudge cannot move a clear winner.
	train := map[string][]TimedSignal{}
	for i := 0; i < 10; i++ {
		train["reasoning"] = append(train["reasoning"], TimedSignal{Points: -1})
	}
	res = evaluateScored(train, []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Scores: scores("reasoning", nil), Signal: SignalDiscard, Points: -1},
	}, cutoff)
	if res.Verdict != EvalNoFlip || res.Flips != 0 {
		t.Errorf("agree: verdict = %q flips = %d, want no-flip / 0", res.Verdict, res.Flips)
	}
}

func TestEvaluateExcludesFallbackDecisions(t *testing.T) {
	// Served tier is NOT the raw argmax (threshold fallback):
	// neither policy describes it, so it is excluded, not counted
	// as a flip either way.
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	train := map[string][]TimedSignal{}
	for i := 0; i < 10; i++ {
		train["quick-qa"] = append(train["quick-qa"], TimedSignal{Points: 1})
	}
	res := evaluateScored(train, []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Scores: scores("quick-qa", nil), Signal: SignalDiscard, Points: -1},
	}, cutoff)
	if res.Verdict != EvalNoEvidence || res.Excluded != 1 || res.Evaluated != 0 {
		t.Errorf("fallback: verdict = %q excluded = %d evaluated = %d, want no-evidence / 1 / 0", res.Verdict, res.Excluded, res.Evaluated)
	}
}

func TestEvaluateBelowFloorIsNoFlip(t *testing.T) {
	// Only 3 train signals: below the 10-sample floor, the nudge
	// is 0 and no decision can flip.
	cutoff := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	train := map[string][]TimedSignal{}
	for i := 0; i < 3; i++ {
		train["reasoning"] = append(train["reasoning"], TimedSignal{Points: -1})
	}
	res := evaluateScored(train, []ScoredDecision{
		{DecisionID: "d1", Tier: "reasoning", Scores: scores("reasoning", map[string]float64{"quick-qa": 0.89}), Signal: SignalDiscard, Points: -1},
	}, cutoff)
	if res.Verdict != EvalNoFlip {
		t.Errorf("below floor: verdict = %q, want no-flip (nudge must be 0)", res.Verdict)
	}
}

// TestEvaluateStoreRoundTrip runs the store-level evaluation on a
// real SQLite file: train signals before the cutoff, test
// decisions after it, scores round-tripped through the column.
func TestEvaluateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	// Train: 12 discards on reasoning, 12 accepts on quick-qa
	// (12, not 10: a day of decay at the cutoff leaves 10.9
	// effective samples, still above the 10-sample floor).
	for i := 0; i < 12; i++ {
		id := "train-r-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "reasoning", Model: "m", TierScores: scores("reasoning", nil)}); err != nil {
			t.Fatalf("register train reasoning %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalDiscard); err != nil {
			t.Fatalf("signal train reasoning %s: %v", id, err)
		}
		id = "train-q-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "quick-qa", Model: "m", TierScores: scores("quick-qa", nil)}); err != nil {
			t.Fatalf("register train quick-qa %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalAccept); err != nil {
			t.Fatalf("signal train quick-qa %s: %v", id, err)
		}
	}
	// Test: 2 discards on reasoning, quick-qa a hair behind.
	s.now = func() time.Time { return base.Add(48 * time.Hour) }
	for i := 0; i < 2; i++ {
		id := "test-r-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "reasoning", Model: "m", TierScores: scores("reasoning", map[string]float64{"quick-qa": 0.87})}); err != nil {
			t.Fatalf("register test %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalDiscard); err != nil {
			t.Fatalf("signal test %s: %v", id, err)
		}
	}
	res, err := s.EvaluateAt(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	if res.Verdict != EvalFeedbackWins || res.Flips != 2 || res.TrainSignals != 24 || res.TestDecisions != 2 {
		t.Errorf("result = %+v, want feedback-wins with 2 flips, 24 train, 2 test", res)
	}
	// Default 70/30 split on the same store: cutoff lands inside
	// the train block (24 of 26 signals), test = the 2 discards.
	res2, err := s.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res2.Verdict != EvalFeedbackWins || res2.Flips != 2 {
		t.Errorf("default split result = %+v, want feedback-wins with 2 flips", res2)
	}
}

// TestTierScoresMigration opens a store file built with the OLD
// schema (no tier_scores_json column) and proves the migration
// adds the column, old rows stay readable, and new registrations
// carry scores.
func TestTierScoresMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE feedback_decisions (
		decision_id TEXT PRIMARY KEY, ts INTEGER NOT NULL, tier TEXT NOT NULL,
		model TEXT NOT NULL, decision_latency_us INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE feedback_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		decision_id TEXT NOT NULL UNIQUE REFERENCES feedback_decisions(decision_id), signal TEXT NOT NULL);`)
	if err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO feedback_decisions (decision_id, ts, tier, model) VALUES ('old-1', 1000, 'reasoning', 'm')`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated store: %v", err)
	}
	defer s.Close()
	// Old row is still there (and unscored).
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM feedback_decisions WHERE decision_id = 'old-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old row after migration: count = %d, err = %v", count, err)
	}
	// New registration carries scores.
	if err := s.RegisterDecision(ctx, DecisionRef{ID: "new-1", Tier: "quick-qa", Model: "m", TierScores: scores("quick-qa", nil)}); err != nil {
		t.Fatalf("register with scores: %v", err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT tier_scores_json FROM feedback_decisions WHERE decision_id = 'new-1'`).Scan(&raw); err != nil {
		t.Fatalf("read scores: %v", err)
	}
	got := parseTierScores(raw)
	if got["quick-qa"] != 0.9 || len(got) != 4 {
		t.Errorf("stored scores = %v, want the 4-tier map with quick-qa 0.9", got)
	}
}

// TestRegisterDecisionScoresValidation rejects non-finite and
// out-of-range scores and unknown tier keys instead of storing
// them.
func TestRegisterDecisionScoresValidation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, sc := range []map[string]float64{
		{"reasoning": 2}, {"reasoning": -2}, {"reasoning": 0.5, "nope": 0.5},
	} {
		if err := s.RegisterDecision(ctx, DecisionRef{ID: "x", Tier: "reasoning", Model: "m", TierScores: sc}); err == nil {
			t.Errorf("RegisterDecision(%v) = nil, want an error", sc)
		}
	}
}

// TestEvaluateSameMillisecondOrder proves the (ts, id) tiebreak
// in the eval queries (CHECKLIST 263–264, the 251–252 fix
// applied to eval.go): every signal in a block is recorded at
// the same frozen millisecond, so ORDER BY ts alone would leave
// their order unspecified. The test pins the order the tiebreak
// guarantees three ways: the rows evalRowsQuery returns come
// back in insertion (id) order — visible as the alternating
// reasoning/quick-qa tier sequence of the train block — and
// EvaluateAt / Evaluate return byte-identical results across
// 25 runs each. SQLite's tie order without the tiebreak is
// unspecified (in practice often insertion order, so this test
// pins the contract rather than reproducing a flake — the
// observable flake was TrainingPairs, fixed in 251–252; here
// the order is observable at the evalRowCap boundary, where
// which same-millisecond test decisions get capped depends on
// row order).
func TestEvaluateSameMillisecondOrder(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	// Train block: 12 discards on reasoning + 12 accepts on
	// quick-qa, recorded interleaved (reasoning, quick-qa, ...)
	// all at the same millisecond.
	for i := 0; i < 12; i++ {
		id := "train-r-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "reasoning", Model: "m", TierScores: scores("reasoning", nil)}); err != nil {
			t.Fatalf("register train reasoning %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalDiscard); err != nil {
			t.Fatalf("signal train reasoning %s: %v", id, err)
		}
		id = "train-q-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "quick-qa", Model: "m", TierScores: scores("quick-qa", nil)}); err != nil {
			t.Fatalf("register train quick-qa %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalAccept); err != nil {
			t.Fatalf("signal train quick-qa %s: %v", id, err)
		}
	}
	// Test block: 2 discards on reasoning, all at one later
	// millisecond shared by both rows.
	s.now = func() time.Time { return base.Add(48 * time.Hour) }
	for i := 0; i < 2; i++ {
		id := "test-r-" + string(rune('a'+i))
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "reasoning", Model: "m", TierScores: scores("reasoning", map[string]float64{"quick-qa": 0.87})}); err != nil {
			t.Fatalf("register test %s: %v", id, err)
		}
		if _, _, err := s.RecordSignal(ctx, id, SignalDiscard); err != nil {
			t.Fatalf("signal test %s: %v", id, err)
		}
	}
	// The production query returns rows in insertion (id)
	// order inside each same-millisecond group: the train
	// block alternates reasoning/discard, quick-qa/accept,
	// then the two reasoning/discard test rows.
	rows, err := s.db.QueryContext(ctx, evalRowsQuery)
	if err != nil {
		t.Fatalf("evalRowsQuery: %v", err)
	}
	var got []string
	for rows.Next() {
		var tier, scoresJSON, signal string
		var tsMs int64
		if err := rows.Scan(&tier, &scoresJSON, &signal, &tsMs); err != nil {
			rows.Close()
			t.Fatalf("scan eval row: %v", err)
		}
		got = append(got, tier+"/"+signal)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("eval rows: %v", err)
	}
	var want []string
	for i := 0; i < 12; i++ {
		want = append(want, "reasoning/discard", "quick-qa/accept")
	}
	want = append(want, "reasoning/discard", "reasoning/discard")
	if len(got) != len(want) {
		t.Fatalf("eval rows = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("eval row %d = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
	// The evaluations themselves are identical on every run.
	first, err := s.EvaluateAt(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	if first.Verdict != EvalFeedbackWins || first.Flips != 2 || first.TrainSignals != 24 || first.TestDecisions != 2 {
		t.Fatalf("EvaluateAt = %+v, want feedback-wins with 2 flips, 24 train, 2 test", first)
	}
	for i := 0; i < 25; i++ {
		res, err := s.EvaluateAt(ctx, base.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("EvaluateAt run %d: %v", i, err)
		}
		if res.Verdict != first.Verdict || res.Flips != first.Flips || res.AvoidedPoints != first.AvoidedPoints || res.AvoidedPerFlip != first.AvoidedPerFlip || res.TrainSignals != first.TrainSignals || res.TestDecisions != first.TestDecisions {
			t.Fatalf("EvaluateAt run %d = %+v, want %+v", i, res, first)
		}
	}
	firstDefault, err := s.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if firstDefault.Verdict != EvalFeedbackWins || firstDefault.Flips != 2 {
		t.Fatalf("Evaluate = %+v, want feedback-wins with 2 flips", firstDefault)
	}
	for i := 0; i < 25; i++ {
		res, err := s.Evaluate(ctx)
		if err != nil {
			t.Fatalf("Evaluate run %d: %v", i, err)
		}
		if res.Verdict != firstDefault.Verdict || res.Flips != firstDefault.Flips || res.AvoidedPoints != firstDefault.AvoidedPoints || res.AvoidedPerFlip != firstDefault.AvoidedPerFlip {
			t.Fatalf("Evaluate run %d = %+v, want %+v", i, res, firstDefault)
		}
	}
}
