// Combo strategy engine wiring (Phase D, D3): fusion / pipeline / jev /
// chaos-mode executed through the real engine request path.
//
// Tests drive the public interface (ChatWithChainStrategy /
// ChatStreamWithChainStrategy) with fake provider.Clients (the
// router_test.go harness). Every test asserts something ONLY the
// combo path produces — winner identity picked by the judge, stage-2
// input threaded from stage 1, JEV admit/hold outcomes on the trace,
// parallel fan-out overlap, stream replay of the combo result — so
// each test FAILS when the interception block in router.go is
// removed (verified by toggling it off; see the D3 report).
package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// traceOutcomes returns provider -> outcome for every trace attempt.
func traceOutcomes(tr *routetrace.RouteTrace) map[string]string {
	out := make(map[string]string)
	if tr == nil {
		return out
	}
	for _, a := range tr.Attempts {
		out[a.Provider] = a.Outcome
	}
	return out
}

func comboReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

func comboChain(providers ...string) []Attempt {
	chain := make([]Attempt, len(providers))
	for i, p := range providers {
		chain[i] = Attempt{ProviderID: p, Model: "m"}
	}
	return chain
}

// TestComboFusionPicksJudgeWinner: strategy "fusion" with two fake
// providers returns the heuristic-judge winner (longest answer),
// and the trace names both attempts (winner "success", loser
// "hedged_cancelled"). Without the combo interception the
// sequential path returns the FIRST success ("short") instead.
func TestComboFusionPicksJudgeWinner(t *testing.T) {
	a := okClient("short")
	b := okClient("this is a much longer answer that the heuristic judge must pick")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "this is a much longer answer that the heuristic judge must pick" {
		t.Fatalf("fusion winner = %q, want the long answer from b", resp.Content)
	}
	outcomes := traceOutcomes(trace)
	if outcomes["b"] != "success" {
		t.Fatalf("trace outcomes = %v, want b=success", outcomes)
	}
	if outcomes["a"] != "hedged_cancelled" {
		t.Fatalf("trace outcomes = %v, want a=hedged_cancelled (fan-out loser)", outcomes)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 {
		t.Fatalf("chat calls a=%d b=%d, want 1 each (both panel members dialled)", a.chatCalls(), b.chatCalls())
	}
}

// TestComboReplayExemptSkipsFanOut: a replay-exempt request
// (WarmUp) with strategy "fusion" must NEVER fan out — exactly
// one attempt, no fallback. a fails, b would succeed, but b must
// never be dialled and the request errors. Without the
// `exempt == ""` guard the combo path would fan out to both and
// return b's answer with no error, so this test fails if the
// guard is removed.
func TestComboReplayExemptSkipsFanOut(t *testing.T) {
	a := errClient(errors.New("a down"))
	b := okClient("b answer")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	req := comboReq()
	req.WarmUp = true
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), req, "fusion")
	if err == nil {
		t.Fatalf("ChatWithChainStrategy(fusion, warm-up) = %+v, want error: exactly one attempt, no fan-out", resp)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("chat calls a=%d, want 1 (the single allowed attempt)", a.chatCalls())
	}
	if b.chatCalls() != 0 {
		t.Fatalf("chat calls b=%d, want 0 (replay-exempt requests never fan out)", b.chatCalls())
	}
	outcomes := traceOutcomes(trace)
	if outcomes["a"] != "error" {
		t.Fatalf("trace outcomes = %v, want a=error", outcomes)
	}
	if _, ok := outcomes["b"]; ok {
		t.Fatalf("trace outcomes = %v, want no b row (b was never attempted)", outcomes)
	}
}

// TestComboPipelineSkippedLastStageServesLastGood: when the last
// stage is optional and fails (skipped), the served result is the
// last GOOD stage, never the failed one — matching
// PipelineResult.Text (last non-skipped stage's output). Direct
// unit test of recordPipelineStages: the engine path only ever
// builds non-optional stages today, so this pins the contract
// for the day an optional stage exists.
func TestComboPipelineSkippedLastStageServesLastGood(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	rec := newComboCallRecorder()
	a := Attempt{ProviderID: "a", Model: "m"}
	b := Attempt{ProviderID: "b", Model: "m"}
	rec.record(attemptResult{att: a, resp: &provider.ChatResponse{Content: "good"}, called: true})
	rec.record(attemptResult{att: b, err: errors.New("b down"), called: true})
	stages := []PipelineStageResult{
		{Stage: PipelineStage{Name: "stage-1", Target: a}, Output: "good"},
		{Stage: PipelineStage{Name: "stage-2", Target: b, Optional: true}, Err: errors.New("b down"), Skipped: true},
	}
	trace := &routetrace.RouteTrace{}
	lastRes, ok := eng.recordPipelineStages(trace, stages, rec, new(int), "", "key", comboReq())
	if !ok {
		t.Fatalf("recordPipelineStages ok=false, want the last good stage (a)")
	}
	if lastRes.att.ProviderID != "a" || lastRes.resp == nil || lastRes.resp.Content != "good" {
		t.Fatalf("last stage result = %+v, want provider a with content \"good\"", lastRes)
	}
}

// TestComboFusionAllFail: when every panel member fails, fusion
// fails honestly with an error naming the failures, and the trace
// records both attempts as errors.
func TestComboFusionAllFail(t *testing.T) {
	a := errClient(errors.New("a down"))
	b := errClient(errors.New("b down"))
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err == nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %+v, want error when all panel members fail", resp)
	}
	if !strings.Contains(err.Error(), "a down") || !strings.Contains(err.Error(), "b down") {
		t.Fatalf("fusion error = %q, want it to name both failures", err.Error())
	}
	outcomes := traceOutcomes(trace)
	if outcomes["a"] != "error" || outcomes["b"] != "error" {
		t.Fatalf("trace outcomes = %v, want a=error b=error", outcomes)
	}
}

// TestComboPipelineFeedsStageOutput: strategy "pipeline" feeds
// stage 1's output into stage 2's input. Without the combo
// interception the sequential path sends the ORIGINAL prompt ("hi")
// to b, so b's assertion fails.
func TestComboPipelineFeedsStageOutput(t *testing.T) {
	a := okClient("PLAN-OUTPUT")
	var bGot string
	var mu sync.Mutex
	b := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		var sb strings.Builder
		for _, m := range req.Messages {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		mu.Lock()
		bGot = sb.String()
		mu.Unlock()
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "pipeline")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(pipeline) = %v, want nil", err)
	}
	if resp.Content != "done" {
		t.Fatalf("pipeline result = %q, want last stage output %q", resp.Content, "done")
	}
	mu.Lock()
	got := bGot
	mu.Unlock()
	if !strings.Contains(got, "PLAN-OUTPUT") {
		t.Fatalf("stage 2 input = %q, want it to contain stage 1 output %q", got, "PLAN-OUTPUT")
	}
	outcomes := traceOutcomes(trace)
	if outcomes["a"] != "success" || outcomes["b"] != "success" {
		t.Fatalf("trace outcomes = %v, want a=success b=success", outcomes)
	}
}

// TestComboPipelineStageFailureAborts: a failing first stage aborts
// the pipeline — the second stage is never dialled — matching
// RunPipeline semantics.
func TestComboPipelineStageFailureAborts(t *testing.T) {
	a := errClient(errors.New("stage one exploded"))
	b := okClient("never")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "pipeline")
	if err == nil {
		t.Fatalf("ChatWithChainStrategy(pipeline) = %+v, want error on stage failure", resp)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("stage 2 chat calls = %d, want 0 (aborted before stage 2)", b.chatCalls())
	}
}

// TestComboJevAdmitsAndHolds: strategy "jev" with three candidates
// — a medium answer first, a long answer second, a refusal third.
// The heuristic JEV scorer admits the long answer as winner; the
// refusal (probability 0, below the 0.15 floor) is HELD on the
// trace, and the admitted non-winner is a hedged loser. Without the
// combo interception the sequential path returns the FIRST success
// (the medium answer) instead of the judged winner.
func TestComboJevAdmitsAndHolds(t *testing.T) {
	medium := okClient("a medium length answer here")
	long := okClient("this is a much longer answer that must win the jev verdict outright")
	refusal := okClient("I'm sorry, I cannot help with that request at all")
	eng := NewEngine(map[string]provider.Client{"m1": medium, "m2": long, "m3": refusal}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("m1", "m2", "m3"), comboReq(), "jev")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(jev) = %v, want nil", err)
	}
	if resp.Content != "this is a much longer answer that must win the jev verdict outright" {
		t.Fatalf("jev winner = %q, want the long answer from m2", resp.Content)
	}
	outcomes := traceOutcomes(trace)
	if outcomes["m2"] != "success" {
		t.Fatalf("trace outcomes = %v, want m2=success (admitted winner)", outcomes)
	}
	if outcomes["m1"] != "hedged_cancelled" {
		t.Fatalf("trace outcomes = %v, want m1=hedged_cancelled (admitted non-winner)", outcomes)
	}
	if outcomes["m3"] != "held_jev" {
		t.Fatalf("trace outcomes = %v, want m3=held_jev (below the 0.15 probability floor)", outcomes)
	}
}

// TestComboChaosModeFansOutParallel: strategy "chaos-mode" fans out
// over the chain in parallel — the second member starts before the
// first finishes — and returns a winner even when one member fails.
// Without the combo interception the sequential path runs the
// members one after another (no overlap) and hedges nothing.
func TestComboChaosModeFansOutParallel(t *testing.T) {
	var mu sync.Mutex
	var starts, ends []time.Time
	recording := func(content string) *fakeClient {
		return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			start := time.Now()
			mu.Lock()
			starts = append(starts, start)
			mu.Unlock()
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			mu.Lock()
			ends = append(ends, time.Now())
			mu.Unlock()
			return &provider.ChatResponse{Content: content}, nil
		}}
	}
	a := recording("chaos-a")
	b := recording("chaos-b-longer-answer-so-it-wins-the-judge")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	start := time.Now()
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "chaos-mode")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(chaos-mode) = %v, want nil", err)
	}
	if resp.Content != "chaos-b-longer-answer-so-it-wins-the-judge" {
		t.Fatalf("chaos-mode winner = %q, want b's answer", resp.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("starts=%d ends=%d, want 2 panel calls each", len(starts), len(ends))
	}
	// Parallel fan-out: the second member starts before the first
	// finishes. Sequential execution would start the second only
	// after the first ends (~150 ms later).
	firstEnd := ends[0]
	if !starts[1].Before(firstEnd) {
		t.Fatalf("second panel member started at %v, first ended at %v: no overlap, fan-out was sequential", starts[1], firstEnd)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("chaos-mode took %v, want < 300ms for two parallel 150ms calls", elapsed)
	}
	outcomes := traceOutcomes(trace)
	if outcomes["b"] != "success" || outcomes["a"] != "hedged_cancelled" {
		t.Fatalf("trace outcomes = %v, want b=success a=hedged_cancelled", outcomes)
	}
}

// TestComboChaosModeToleratesMemberFailure: one failing panel member
// does not fail a chaos-mode request when another member succeeds.
func TestComboChaosModeToleratesMemberFailure(t *testing.T) {
	a := errClient(errors.New("chaos member down"))
	b := okClient("chaos survivor")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "chaos-mode")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(chaos-mode) = %v, want nil", err)
	}
	if resp.Content != "chaos survivor" {
		t.Fatalf("chaos-mode winner = %q, want b's answer", resp.Content)
	}
	outcomes := traceOutcomes(trace)
	if outcomes["a"] != "error" || outcomes["b"] != "success" {
		t.Fatalf("trace outcomes = %v, want a=error b=success", outcomes)
	}
}

// TestComboNonComboStrategySequential: a non-combo strategy
// ("priority") still takes the sequential path — the combo
// interception does not fire. First target fails, second serves;
// no hedged_cancelled entry appears (nothing fanned out).
func TestComboNonComboStrategySequential(t *testing.T) {
	a := errClient(errors.New("boom"))
	b := okClient("from-b")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "priority")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(priority) = %v, want nil", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("priority result = %q, want from-b", resp.Content)
	}
	outcomes := traceOutcomes(trace)
	if outcomes["a"] != "error" || outcomes["b"] != "success" {
		t.Fatalf("trace outcomes = %v, want a=error b=success", outcomes)
	}
	for _, at := range trace.Attempts {
		if at.Outcome == "hedged_cancelled" || at.Outcome == "held_jev" {
			t.Fatalf("trace has combo outcome %q for provider %q on a sequential request", at.Outcome, at.Provider)
		}
	}
}

// TestComboStreamFusionReplay: a "fusion" stream runs the combo
// non-streaming and replays the winner verbatim as stream events:
// the joined deltas equal the winner text, and the Done event
// carries the full response. Without the combo interception the
// sequential stream would emit the FIRST provider's text ("short").
func TestComboStreamFusionReplay(t *testing.T) {
	a := okClient("short")
	b := okClient("this is a much longer answer that the heuristic judge must pick")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatStreamWithChainStrategy(fusion) = %v, want nil", err)
	}
	var sb strings.Builder
	var doneResp *provider.ChatResponse
	sawDone := false
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		sb.WriteString(ev.Delta)
		if ev.Done {
			sawDone = true
			doneResp = ev.Resp
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a Done event")
	}
	if got := sb.String(); got != "this is a much longer answer that the heuristic judge must pick" {
		t.Fatalf("replayed deltas = %q, want the fusion winner verbatim", got)
	}
	if doneResp == nil || doneResp.Content != "this is a much longer answer that the heuristic judge must pick" {
		t.Fatalf("Done Resp = %+v, want the fusion winner response", doneResp)
	}
	// The trace must say the result came from the combo path: both
	// panel members recorded, winner success, loser hedged.
	tr := eng.LastTrace()
	if tr == nil {
		t.Fatal("no LastTrace published for the combo stream")
	}
	outcomes := traceOutcomes(tr)
	if outcomes["b"] != "success" || outcomes["a"] != "hedged_cancelled" {
		t.Fatalf("stream trace outcomes = %v, want b=success a=hedged_cancelled", outcomes)
	}
}

// TestComboStreamNonComboStillStreams: a non-combo stream still
// takes the live sequential streaming path (no replay).
func TestComboStreamNonComboStillStreams(t *testing.T) {
	a := streamClient([]string{"hel", "lo"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	ch, err := eng.ChatStreamWithChainStrategy(context.Background(), comboChain("a"), comboReq(), "priority")
	if err != nil {
		t.Fatalf("ChatStreamWithChainStrategy(priority) = %v, want nil", err)
	}
	var sb strings.Builder
	sawDone := false
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		sb.WriteString(ev.Delta)
		if ev.Done {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a Done event")
	}
	if got := sb.String(); got != "hello" {
		t.Fatalf("streamed deltas = %q, want %q", got, "hello")
	}
}

// TestLLMJudgeScoresViaRealCall: the LLM judge calls the engine's
// own callChat once on its judge attempt and parses the winner from
// the reply ("WINNER: 2" -> index 1). The fake records the judge
// request, proving the judge prompt (with both candidates) was
// actually sent to the judge provider.
func TestLLMJudgeScoresViaRealCall(t *testing.T) {
	var judgeReq provider.ChatRequest
	judge := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		judgeReq = req
		return &provider.ChatResponse{Content: "WINNER: 2"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"j": judge}, nil, testConfig())
	j := &llmJudge{engine: eng, judge: Attempt{ProviderID: "j", Model: "m"}}
	candidates := []PanelResult{
		{Attempt: Attempt{ProviderID: "a", Model: "m"}, Text: "first candidate"},
		{Attempt: Attempt{ProviderID: "b", Model: "m"}, Text: "second candidate"},
	}
	winner, err := j.Score(context.Background(), "which is better?", candidates)
	if err != nil {
		t.Fatalf("llmJudge.Score = %v, want nil", err)
	}
	if winner != 1 {
		t.Fatalf("llmJudge winner = %d, want 1 (WINNER: 2)", winner)
	}
	if judge.chatCalls() != 1 {
		t.Fatalf("judge chat calls = %d, want exactly 1", judge.chatCalls())
	}
	var joined string
	for _, m := range judgeReq.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "first candidate") || !strings.Contains(joined, "second candidate") {
		t.Fatalf("judge request = %q, want both candidates in the judge prompt", joined)
	}
}

// TestLLMJudgeUnparseableFallsBack: a judge reply that names no
// winner is an error (never an invented judgement); the
// fallbackJudge wrapper then uses the heuristic and reports the
// fallback via onFallback.
func TestLLMJudgeUnparseableFallsBack(t *testing.T) {
	judge := okClient("these are both fine, hard to say")
	eng := NewEngine(map[string]provider.Client{"j": judge}, nil, testConfig())
	j := &llmJudge{engine: eng, judge: Attempt{ProviderID: "j", Model: "m"}}
	candidates := []PanelResult{
		{Attempt: Attempt{ProviderID: "a", Model: "m"}, Text: "short"},
		{Attempt: Attempt{ProviderID: "b", Model: "m"}, Text: "a much longer answer that must win on fallback"},
	}
	if _, err := j.Score(context.Background(), "pick one", candidates); err == nil {
		t.Fatal("llmJudge.Score with unparseable reply = nil error, want an error (never invent a judgement)")
	}
	fellBack := false
	fb := fallbackJudge{primary: j, onFallback: func(err error) { fellBack = true }}
	winner, err := fb.Score(context.Background(), "pick one", candidates)
	if err != nil {
		t.Fatalf("fallbackJudge.Score = %v, want nil", err)
	}
	if winner != 1 {
		t.Fatalf("fallbackJudge winner = %d, want 1 (heuristic longest)", winner)
	}
	if !fellBack {
		t.Fatal("fallbackJudge did not report the fallback via onFallback")
	}
}

// TestHeuristicJevScorerFallsOpen: when the heuristic cannot
// distinguish the candidates (all refusals, scores clamped to 0)
// the scorer reports unavailable (nil answer), so JudgeJev falls
// open instead of inventing probabilities.
func TestHeuristicJevScorerFallsOpen(t *testing.T) {
	s := heuristicJevScorer{}
	candidates := []PanelResult{
		{Attempt: Attempt{ProviderID: "a", Model: "m"}, Text: "I'm sorry, I cannot help"},
		{Attempt: Attempt{ProviderID: "b", Model: "m"}, Text: "I am unable to do that"},
	}
	ans, err := s.ScoreJev(context.Background(), "hi", candidates)
	if err != nil {
		t.Fatalf("heuristicJevScorer.ScoreJev = %v, want nil", err)
	}
	if ans != nil {
		t.Fatalf("heuristicJevScorer answer = %+v, want nil (unavailable -> fall open)", ans)
	}
}

// ---------------------------------------------------------------------------
// RT-3: the fan-out panel must not bypass the strict per-request
// budget gate. Prices reuse the budget-test stub: "hello world"
// is 9 local tokens, so each "cheap" member estimates $0.00009
// and a 5-member panel sums to $0.00045.
// ---------------------------------------------------------------------------

// comboBudgetPanel builds 5 fake "cheap" providers and the
// 5-member chain over them, returning both so tests can count
// calls per member.
func comboBudgetPanel() (map[string]provider.Client, []*fakeClient, []Attempt) {
	ids := []string{"p0", "p1", "p2", "p3", "p4"}
	clients := make(map[string]provider.Client, len(ids))
	fakes := make([]*fakeClient, len(ids))
	chain := make([]Attempt, len(ids))
	for i, id := range ids {
		fakes[i] = okClient("answer from " + id)
		clients[id] = fakes[i]
		chain[i] = Attempt{ProviderID: id, Model: "cheap"}
	}
	return clients, fakes, chain
}

// TestComboFusionStrictBudgetRefusesOversizePanel (RT-3): a
// strict $0.0002 budget fits ONE $0.00009 attempt (so the
// server's one-attempt pre-send check passes) but not the
// 5-member panel ($0.00045). The fusion request must be refused
// with a RequestBudgetExceededError BEFORE any provider is
// dialled — zero calls on all five members, every member traced
// as skipped_budget. Before the fix the panel fanned out and
// spent up to 5x the budget.
func TestComboFusionStrictBudgetRefusesOversizePanel(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	clients, fakes, chain := comboBudgetPanel()
	eng := NewEngine(clients, nil, cfg)
	_, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, budgetTestReq(0.0002, true), "fusion")
	if err == nil || !IsRequestBudgetExceeded(err) {
		t.Fatalf("err = %v, want a RequestBudgetExceededError", err)
	}
	var be *RequestBudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("err is not a RequestBudgetExceededError: %v", err)
	}
	// Cumulative: p0 $0.00009, p1 $0.00018, p2 would push the sum
	// to $0.00027 > $0.0002 — p2 is the blocking member.
	if be.BudgetUSD != 0.0002 || be.Provider != "p2" || be.NextEstimateUSD != 0.00009 {
		t.Errorf("error = %+v, want budget 0.0002 / provider p2 / estimate 0.00009", be)
	}
	for i, f := range fakes {
		if f.chatCalls() != 0 {
			t.Errorf("provider p%d calls = %d, want 0 (refused before fan-out)", i, f.chatCalls())
		}
	}
	if trace == nil || len(trace.Attempts) != 5 {
		t.Fatalf("trace attempts = %d, want 5 (one skipped_budget row per panel member)", len(trace.Attempts))
	}
	for i, a := range trace.Attempts {
		if a.Outcome != "skipped_budget" {
			t.Errorf("trace[%d].Outcome = %q, want skipped_budget", i, a.Outcome)
		}
	}
}

// TestComboJevStrictBudgetRefusesOversizePanel (RT-3): the same
// panel-sum refusal on the jev fan-out path — the gate covers
// every fan-out strategy, not just fusion.
func TestComboJevStrictBudgetRefusesOversizePanel(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	clients, fakes, chain := comboBudgetPanel()
	eng := NewEngine(clients, nil, cfg)
	_, _, err := eng.ChatWithChainStrategy(context.Background(), chain, budgetTestReq(0.0002, true), "jev")
	if err == nil || !IsRequestBudgetExceeded(err) {
		t.Fatalf("err = %v, want a RequestBudgetExceededError", err)
	}
	for i, f := range fakes {
		if f.chatCalls() != 0 {
			t.Errorf("provider p%d calls = %d, want 0 (refused before fan-out)", i, f.chatCalls())
		}
	}
}

// TestComboFusionBudgetFitsPanelProceeds (RT-3): the control — a
// strict $0.0005 budget fits the panel's $0.00045 sum, so fusion
// runs, every member is dialled exactly once, and a winner is
// served.
func TestComboFusionBudgetFitsPanelProceeds(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	clients, fakes, chain := comboBudgetPanel()
	eng := NewEngine(clients, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), chain, budgetTestReq(0.0005, true), "fusion")
	if err != nil {
		t.Fatalf("err = %v, want nil (panel fits the budget)", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatalf("resp = %+v, want a served winner", resp)
	}
	for i, f := range fakes {
		if f.chatCalls() != 1 {
			t.Errorf("provider p%d calls = %d, want 1 (panel member dialled once)", i, f.chatCalls())
		}
	}
}

// TestComboFusionNonStrictBudgetProceeds (RT-3): non-strict mode
// never blocks — the same oversize panel runs and the overage is
// the server's to record post-hoc, exactly like the sequential
// path.
func TestComboFusionNonStrictBudgetProceeds(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	clients, fakes, chain := comboBudgetPanel()
	eng := NewEngine(clients, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), chain, budgetTestReq(0.0002, false), "fusion")
	if err != nil {
		t.Fatalf("err = %v, want nil (non-strict never blocks)", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatalf("resp = %+v, want a served winner", resp)
	}
	for i, f := range fakes {
		if f.chatCalls() != 1 {
			t.Errorf("provider p%d calls = %d, want 1 (panel member dialled once)", i, f.chatCalls())
		}
	}
}
