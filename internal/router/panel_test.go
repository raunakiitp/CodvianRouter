// Combo execution primitives: fusion / pipeline / jev (M3 Phase C, builder C4).
//
// Tests FIRST: every test below fails (does not compile) until panel.go
// exists, and fails at runtime if the logic it pins is removed. All
// callers here are fakes — no network, no real provider, no fabricated
// response text: every output string is a literal written in the test.
package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func panelAtt(provider, model string) Attempt {
	return Attempt{ProviderID: provider, Model: model}
}

func fakePanelCaller(out map[string]string, fail map[string]error) PanelCaller {
	return func(ctx context.Context, att Attempt) (string, error) {
		if err, ok := fail[att.Model]; ok {
			return "", err
		}
		if text, ok := out[att.Model]; ok {
			return text, nil
		}
		return "", errors.New("fake caller: no fixture for model " + att.Model)
	}
}

// --- fusion: built-in heuristic judge -------------------------------------

func TestFusionHeuristicPicksLongestNonRefusal(t *testing.T) {
	// Fixed fixture: a short answer, a long good answer, and a longer
	// refusal. The heuristic must pick the long GOOD answer — longest
	// sane wins only among non-refusals.
	targets := []Attempt{
		panelAtt("prov-a", "short"),
		panelAtt("prov-b", "long-good"),
		panelAtt("prov-c", "long-refusal"),
	}
	call := fakePanelCaller(map[string]string{
		"short":        "Yes.",
		"long-good":    strings.Repeat("The answer is 42 because the question asks for the meaning of life. ", 10),
		"long-refusal": strings.Repeat("I'm sorry, I cannot help with that request. ", 20),
	}, nil)
	res, err := RunFusion(context.Background(), "What is the answer?", targets, call, FusionConfig{})
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if res.Winner.Attempt.Model != "long-good" {
		t.Errorf("winner = %q, want %q (candidates: %v)", res.Winner.Attempt.Model, "long-good", winnerModels(res))
	}
	if res.JudgeName != "heuristic" {
		t.Errorf("JudgeName = %q, want %q", res.JudgeName, "heuristic")
	}
	if len(res.Candidates) != 3 {
		t.Errorf("len(Candidates) = %d, want 3", len(res.Candidates))
	}
}

func winnerModels(res FusionResult) []string {
	out := make([]string, len(res.Candidates))
	for i, c := range res.Candidates {
		out[i] = c.Attempt.Model
	}
	return out
}

func TestFusionHeuristicEmptyDisqualified(t *testing.T) {
	// An empty answer must never win, even when it is the only
	// "longest" candidate by position; the non-empty one wins.
	targets := []Attempt{panelAtt("prov-a", "empty"), panelAtt("prov-b", "real")}
	call := fakePanelCaller(map[string]string{"empty": "   ", "real": "Here is the answer."}, nil)
	res, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{})
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if res.Winner.Attempt.Model != "real" {
		t.Errorf("winner = %q, want %q", res.Winner.Attempt.Model, "real")
	}
}

func TestFusionAllTargetsFailNamesFailures(t *testing.T) {
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := fakePanelCaller(nil, map[string]error{
		"m1": errors.New("boom-a"),
		"m2": errors.New("boom-b"),
	})
	_, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{})
	if err == nil {
		t.Fatal("RunFusion with all targets failing: got nil error, want error naming the failures")
	}
	for _, want := range []string{"prov-a/m1", "prov-b/m2", "boom-a", "boom-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

func TestFusionAllEmptyIsError(t *testing.T) {
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := fakePanelCaller(map[string]string{"m1": "", "m2": "  "}, nil)
	_, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{})
	if err == nil {
		t.Fatal("RunFusion with all empty responses: got nil error, want error")
	}
}

func TestFusionSingleTargetDirect(t *testing.T) {
	// the sector baseline fusion.ts: a single-model panel answers directly, no judge.
	targets := []Attempt{panelAtt("prov-a", "only")}
	call := fakePanelCaller(map[string]string{"only": "direct answer"}, nil)
	res, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{})
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if res.Winner.Text != "direct answer" || res.Winner.Attempt.Model != "only" {
		t.Errorf("winner = %+v, want model only / direct answer", res.Winner)
	}
}

func TestFusionTooManyTargetsRejected(t *testing.T) {
	targets := make([]Attempt, 6)
	for i := range targets {
		targets[i] = panelAtt("prov", "m")
	}
	_, err := RunFusion(context.Background(), "prompt", targets, fakePanelCaller(nil, nil), FusionConfig{})
	if err == nil {
		t.Fatal("RunFusion with 6 targets over default max-panel 5: got nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "6") || !strings.Contains(err.Error(), "5") {
		t.Errorf("error %q should name panel size 6 and max 5", err.Error())
	}
}

func TestFusionCustomJudgeUsed(t *testing.T) {
	// A caller-supplied Judge decides; the heuristic must not override it.
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := fakePanelCaller(map[string]string{
		"m1": strings.Repeat("long first answer ", 50),
		"m2": "short",
	}, nil)
	res, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{
		Judge: JudgeFunc(func(ctx context.Context, prompt string, c []PanelResult) (int, error) {
			return 1, nil // always pick the second candidate
		}),
	})
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if res.Winner.Attempt.Model != "m2" {
		t.Errorf("winner = %q, want %q (custom judge picks index 1)", res.Winner.Attempt.Model, "m2")
	}
	if res.JudgeName != "custom" {
		t.Errorf("JudgeName = %q, want %q", res.JudgeName, "custom")
	}
}

func TestFusionJudgeErrorPropagates(t *testing.T) {
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := fakePanelCaller(map[string]string{"m1": "a", "m2": "b"}, nil)
	_, err := RunFusion(context.Background(), "prompt", targets, call, FusionConfig{
		Judge: JudgeFunc(func(ctx context.Context, prompt string, c []PanelResult) (int, error) {
			return 0, errors.New("judge exploded")
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "judge exploded") {
		t.Errorf("RunFusion with failing judge: err = %v, want it to propagate 'judge exploded'", err)
	}
}

func TestFusionContextTimeoutRespected(t *testing.T) {
	// Conforming caller: blocks until ctx is done, then returns ctx's error.
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := func(ctx context.Context, att Attempt) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RunFusion(ctx, "prompt", targets, call, FusionConfig{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("RunFusion with expired ctx: got nil error, want error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("RunFusion took %v after ctx timeout; fan-out did not respect the deadline", elapsed)
	}
}

func TestFusionNoTargetsError(t *testing.T) {
	_, err := RunFusion(context.Background(), "prompt", nil, fakePanelCaller(nil, nil), FusionConfig{})
	if err == nil {
		t.Fatal("RunFusion with no targets: got nil error, want error")
	}
}

// --- pipeline ---------------------------------------------------------------

func TestPipelinePassesOutputsInOrder(t *testing.T) {
	// Stage 2's input must contain stage 1's output; stage 3's input
	// must contain stage 2's output; the final text is stage 3's output.
	var seen []string
	call := func(ctx context.Context, att Attempt, input string) (string, error) {
		seen = append(seen, input)
		switch att.Model {
		case "plan":
			return "PLAN: do X then Y", nil
		case "exec":
			if !strings.Contains(input, "PLAN: do X then Y") {
				t.Errorf("exec stage input %q does not contain plan output", input)
			}
			return "EXEC: did X then Y", nil
		case "fix":
			if !strings.Contains(input, "EXEC: did X then Y") {
				t.Errorf("fix stage input %q does not contain exec output", input)
			}
			return "FIX: polished result", nil
		}
		return "", errors.New("unexpected stage " + att.Model)
	}
	stages := []PipelineStage{
		{Name: "plan", Target: panelAtt("prov-a", "plan")},
		{Name: "execute", Target: panelAtt("prov-b", "exec")},
		{Name: "fix", Target: panelAtt("prov-c", "fix")},
	}
	res, err := RunPipeline(context.Background(), "original prompt", stages, call)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.Text != "FIX: polished result" {
		t.Errorf("final text = %q, want %q", res.Text, "FIX: polished result")
	}
	if len(res.Stages) != 3 {
		t.Fatalf("len(Stages) = %d, want 3", len(res.Stages))
	}
	if !strings.Contains(seen[0], "original prompt") {
		t.Errorf("first stage input %q does not contain the original prompt", seen[0])
	}
}

func TestPipelineStageFailureAborts(t *testing.T) {
	called := map[string]bool{}
	call := func(ctx context.Context, att Attempt, input string) (string, error) {
		called[att.Model] = true
		if att.Model == "exec" {
			return "", errors.New("upstream 503")
		}
		return "ok-" + att.Model, nil
	}
	stages := []PipelineStage{
		{Name: "plan", Target: panelAtt("prov-a", "plan")},
		{Name: "execute", Target: panelAtt("prov-b", "exec")},
		{Name: "fix", Target: panelAtt("prov-c", "fix")},
	}
	res, err := RunPipeline(context.Background(), "prompt", stages, call)
	if err == nil {
		t.Fatal("RunPipeline with failing middle stage: got nil error, want abort error")
	}
	for _, want := range []string{"execute", "prov-b/exec", "upstream 503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
	if called["fix"] {
		t.Error("fix stage ran after a non-optional stage failure; pipeline must abort")
	}
	if len(res.Stages) != 2 {
		t.Errorf("len(Stages) = %d, want 2 (plan + failed execute)", len(res.Stages))
	}
	if res.Stages[1].Err == nil {
		t.Error("failed stage result carries nil Err")
	}
}

func TestPipelineOptionalStageSkips(t *testing.T) {
	// An optional stage that fails is skipped; the next stage receives
	// the last GOOD output (the plan's), not the failed stage's.
	var fixInput string
	call := func(ctx context.Context, att Attempt, input string) (string, error) {
		switch att.Model {
		case "plan":
			return "PLAN-OUT", nil
		case "reflect":
			return "", errors.New("reflect crashed")
		case "fix":
			fixInput = input
			return "FIX-OUT", nil
		}
		return "", errors.New("unexpected " + att.Model)
	}
	stages := []PipelineStage{
		{Name: "plan", Target: panelAtt("prov-a", "plan")},
		{Name: "reflect", Target: panelAtt("prov-b", "reflect"), Optional: true},
		{Name: "fix", Target: panelAtt("prov-c", "fix")},
	}
	res, err := RunPipeline(context.Background(), "prompt", stages, call)
	if err != nil {
		t.Fatalf("RunPipeline with failing OPTIONAL stage: %v (must skip, not abort)", err)
	}
	if res.Text != "FIX-OUT" {
		t.Errorf("final text = %q, want %q", res.Text, "FIX-OUT")
	}
	if len(res.Stages) != 3 || !res.Stages[1].Skipped || res.Stages[1].Err == nil {
		t.Errorf("stages = %+v, want reflect marked skipped with its error", res.Stages)
	}
	if !strings.Contains(fixInput, "PLAN-OUT") {
		t.Errorf("fix stage input %q does not contain the plan output (skipped stage's output must not flow forward)", fixInput)
	}
}

func TestPipelineEmptyStagesError(t *testing.T) {
	_, err := RunPipeline(context.Background(), "prompt", nil, func(ctx context.Context, att Attempt, input string) (string, error) {
		return "x", nil
	})
	if err == nil {
		t.Fatal("RunPipeline with no stages: got nil error, want error")
	}
}

func TestPipelineContextCancelledAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunPipeline(ctx, "prompt", []PipelineStage{
		{Name: "plan", Target: panelAtt("prov-a", "plan")},
	}, func(ctx context.Context, att Attempt, input string) (string, error) {
		return "", ctx.Err()
	})
	if err == nil {
		t.Fatal("RunPipeline with cancelled ctx: got nil error, want error")
	}
}

// --- jev: pure admit/hold bookkeeping --------------------------------------

func jevFixture() ([]JevCandidate, *JevChoiceAnswer) {
	cands := []JevCandidate{
		{ID: "a", UserRank: 0},
		{ID: "b", UserRank: 1},
		{ID: "c", UserRank: 2},
		{ID: "d", UserRank: 3},
		{ID: "e", UserRank: 4},
	}
	// Hand-computed: winner b (0.40); a 0.30 and c 0.20 clear the 0.15
	// floor; d 0.10 and e 0.05 do not. Confidence 0.80 clears 0.65.
	ans := &JevChoiceAnswer{
		Choice:        "b",
		Confidence:    0.80,
		Probabilities: map[string]float64{"a": 0.30, "b": 0.40, "c": 0.20, "d": 0.10, "e": 0.05},
	}
	return cands, ans
}

func jevIDs(rows []JevVerdictRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func TestJevAdmitHoldHandComputed(t *testing.T) {
	cands, ans := jevFixture()
	res := JudgeJev(cands, ans)
	if res.FellOpen {
		t.Error("JudgeJev fell open on a confident, in-set answer; want a real verdict")
	}
	if got, want := strings.Join(jevIDs(res.Admitted), ","), "b,a,c"; got != want {
		t.Errorf("admitted = %q, want %q (winner first, then user-rank order)", got, want)
	}
	if got, want := strings.Join(jevIDs(res.Held), ","), "d,e"; got != want {
		t.Errorf("held = %q, want %q", got, want)
	}
	for _, row := range res.Held {
		if row.Reason != JevHoldBelowFloor {
			t.Errorf("held %q reason = %q, want %q", row.ID, row.Reason, JevHoldBelowFloor)
		}
	}
}

func TestJevMaxAdmittedDominated(t *testing.T) {
	// Six candidates, all above the floor, choice c, confidence 0.9:
	// winner c first, then a, b, d fill the max-4 admitted set; e and f
	// are held as "dominated".
	cands := []JevCandidate{
		{ID: "a", UserRank: 0}, {ID: "b", UserRank: 1}, {ID: "c", UserRank: 2},
		{ID: "d", UserRank: 3}, {ID: "e", UserRank: 4}, {ID: "f", UserRank: 5},
	}
	ans := &JevChoiceAnswer{Choice: "c", Confidence: 0.90,
		Probabilities: map[string]float64{"a": 0.5, "b": 0.5, "c": 0.5, "d": 0.5, "e": 0.5, "f": 0.5}}
	res := JudgeJev(cands, ans)
	if got, want := strings.Join(jevIDs(res.Admitted), ","), "c,a,b,d"; got != want {
		t.Errorf("admitted = %q, want %q (max 4)", got, want)
	}
	if got, want := strings.Join(jevIDs(res.Held), ","), "e,f"; got != want {
		t.Errorf("held = %q, want %q", got, want)
	}
	for _, row := range res.Held {
		if row.Reason != JevHoldDominated {
			t.Errorf("held %q reason = %q, want %q", row.ID, row.Reason, JevHoldDominated)
		}
	}
}

func TestJevLowConfidenceFallsOpen(t *testing.T) {
	cands, ans := jevFixture()
	ans.Confidence = 0.50 // below the 0.65 floor
	res := JudgeJev(cands, ans)
	if !res.FellOpen || res.FallbackReason != JevHoldLowConfidence {
		t.Errorf("low-confidence verdict = fellOpen=%v reason=%q, want fellOpen=true reason=%q",
			res.FellOpen, res.FallbackReason, JevHoldLowConfidence)
	}
	if got, want := strings.Join(jevIDs(res.Admitted), ","), "a,b,c,d,e"; got != want {
		t.Errorf("fall-open admitted = %q, want full user order %q", got, want)
	}
	if len(res.Held) != 0 {
		t.Errorf("fall-open held = %v, want none", jevIDs(res.Held))
	}
}

func TestJevUnavailableFallsOpen(t *testing.T) {
	cands, _ := jevFixture()
	res := JudgeJev(cands, nil) // no judge answer at all
	if !res.FellOpen || res.FallbackReason != JevHoldUnavailable {
		t.Errorf("nil-answer verdict = fellOpen=%v reason=%q, want fellOpen=true reason=%q",
			res.FellOpen, res.FallbackReason, JevHoldUnavailable)
	}
	if got, want := strings.Join(jevIDs(res.Admitted), ","), "a,b,c,d,e"; got != want {
		t.Errorf("fall-open admitted = %q, want %q", got, want)
	}
}

func TestJevChoiceNotInSetFallsOpen(t *testing.T) {
	cands, ans := jevFixture()
	ans.Choice = "zzz" // judge picked an id that was never offered
	res := JudgeJev(cands, ans)
	if !res.FellOpen || res.FallbackReason != JevHoldUnavailable {
		t.Errorf("unknown-choice verdict = fellOpen=%v reason=%q, want fellOpen=true reason=%q",
			res.FellOpen, res.FallbackReason, JevHoldUnavailable)
	}
}

func TestJevSingleCandidateAdmitted(t *testing.T) {
	res := JudgeJev([]JevCandidate{{ID: "only", UserRank: 0}}, &JevChoiceAnswer{
		Choice: "only", Confidence: 0.1, Probabilities: map[string]float64{"only": 0.01},
	})
	if res.FellOpen || len(res.Admitted) != 1 || res.Admitted[0].ID != "only" {
		t.Errorf("single-candidate verdict = %+v, want only admitted, no fall-open", res)
	}
}

func TestJevFloorBoundaryAdmitted(t *testing.T) {
	// Exactly at the 0.15 floor admits (>= floor, per jevStrategy.ts).
	cands := []JevCandidate{{ID: "a", UserRank: 0}, {ID: "b", UserRank: 1}}
	ans := &JevChoiceAnswer{Choice: "a", Confidence: 0.65,
		Probabilities: map[string]float64{"a": 0.85, "b": 0.15}}
	res := JudgeJev(cands, ans)
	if got, want := strings.Join(jevIDs(res.Admitted), ","), "a,b"; got != want {
		t.Errorf("boundary admitted = %q, want %q", got, want)
	}
}

// --- jev: fan-out + caller-supplied scorer ---------------------------------

func TestJevPanelAdmitsViaScorer(t *testing.T) {
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2"), panelAtt("prov-c", "m3")}
	call := fakePanelCaller(map[string]string{"m1": "one", "m2": "two", "m3": "three"}, nil)
	scorer := JevScorerFunc(func(ctx context.Context, prompt string, c []PanelResult) (*JevChoiceAnswer, error) {
		// Caller-supplied scores: the panel code must not invent these.
		return &JevChoiceAnswer{
			Choice: "prov-b/m2", Confidence: 0.9,
			Probabilities: map[string]float64{"prov-a/m1": 0.2, "prov-b/m2": 0.6, "prov-c/m3": 0.1},
		}, nil
	})
	res, err := RunJevPanel(context.Background(), "prompt", targets, call, scorer, PanelConfig{})
	if err != nil {
		t.Fatalf("RunJevPanel: %v", err)
	}
	if got, want := strings.Join(jevIDs(res.Verdict.Admitted), ","), "prov-b/m2,prov-a/m1"; got != want {
		t.Errorf("admitted = %q, want %q", got, want)
	}
	if got, want := strings.Join(jevIDs(res.Verdict.Held), ","), "prov-c/m3"; got != want {
		t.Errorf("held = %q, want %q", got, want)
	}
	if len(res.Results) != 3 {
		t.Errorf("len(Results) = %d, want 3 (all executed candidates)", len(res.Results))
	}
}

func TestJevPanelScorerErrorPropagates(t *testing.T) {
	targets := []Attempt{panelAtt("prov-a", "m1"), panelAtt("prov-b", "m2")}
	call := fakePanelCaller(map[string]string{"m1": "one", "m2": "two"}, nil)
	scorer := JevScorerFunc(func(ctx context.Context, prompt string, c []PanelResult) (*JevChoiceAnswer, error) {
		return nil, errors.New("scorer down")
	})
	_, err := RunJevPanel(context.Background(), "prompt", targets, call, scorer, PanelConfig{})
	if err == nil || !strings.Contains(err.Error(), "scorer down") {
		t.Errorf("RunJevPanel with failing scorer: err = %v, want 'scorer down' propagated", err)
	}
}
