// Combo strategy engine wiring (Phase D, D3): fusion / pipeline / jev /
// chaos-mode.
//
// The panel primitives in panel.go (RunFusion, RunPipeline,
// RunJevPanel) are pure logic over caller-supplied executors. This
// file is the engine wiring: it adapts the engine's real leaf
// executor (callChat) to PanelCaller / PipelineCaller, runs the
// primitive over the ordered chain, and records every attempt on
// the trace with the engine's own bookkeeping (noteSuccess /
// noteFailure / noteHedgedLoser), so a combo request leaves exactly
// the same ledger / breaker / cooldown / trace footprint as the
// sequential path — only the execution shape differs.
//
// Judge policy (verified, no judge model is configured anywhere:
// Config has no judge field and no judge plumbing exists): the
// default judge is HeuristicJudge, the deterministic heuristic from
// panel.go. llmJudge below is a real Judge backed by one callChat
// on an explicit judge attempt, implemented and tested for the day
// a judge model IS plumbed — it is not the default, and no judge
// model is invented here. When an LLM judge is used through
// fallbackJudge, a judge-call failure falls back to HeuristicJudge
// and the fallback is reported via onFallback (the caller records
// it on the trace); the request never fails because the judge did.
//
// chaos-mode is fusion over the panel chain (parallel fan-out +
// heuristic judge). Fault injection — deliberately failing a
// provider so the chaos-mode path can exercise failover — lives
// in faultinject.go (B2-13): an operator/test-armed fault
// intercepts the attempt in callChat before any provider is
// dialled. It is OFF by default and can only be armed through an
// explicit ArmFault call, never from a user request; breaking
// live user requests is not a routing feature, and no fault is
// ever armed unless someone arms it.
//
// Replay-exempt requests (stateful: embeddings, Responses
// continuations, warm-ups) never fan out: the interception points
// in router.go skip the combo path when ReplayExemption(req) is
// set, so a stateful request is still attempted exactly once.
package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// comboStrategies is the set of strategy names executed by the
// combo engine path instead of the sequential attempt loop. The
// names arrive as the EFFECTIVE strategy (see effectiveStrategy):
// the per-request override, chain override, or global strategy,
// verbatim — "fusion" / "pipeline" / "jev" have no ordering hook
// in internal/strategy (wiringHook returns nil for them), so there
// is no double-application for those three. "chaos-mode" DOES have
// an ordering hook (the 16-factor "chaos-mode" mode pack in
// wiring.go) which runs during orderAttempts and only reorders the
// panel; the combo interception below owns execution.
var comboStrategies = map[string]bool{
	"fusion":     true,
	"pipeline":   true,
	"jev":        true,
	"chaos-mode": true,
}

// isComboStrategy reports whether strategy is executed by the combo
// engine path (runComboStrategy) rather than the sequential loop.
func isComboStrategy(strategy string) bool {
	return comboStrategies[strategy]
}

// comboPromptText extracts the prompt text the panel primitives
// judge / thread: the last user message, falling back to all
// non-empty message contents joined (never an invented prompt).
func comboPromptText(req provider.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			return req.Messages[i].Content
		}
	}
	var parts []string
	for _, m := range req.Messages {
		if strings.TrimSpace(m.Content) != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// comboCallRecorder collects the full attemptResult of every real
// provider call the panel/pipeline adapters make, keyed by Attempt
// (FIFO per key, so duplicate attempts resolve in call order).
// The primitives only return text, but the trace/ledger
// bookkeeping needs latency, usage and the response — the recorder
// is how runComboStrategy recovers them.
type comboCallRecorder struct {
	mu        sync.Mutex
	byAttempt map[Attempt][]attemptResult
}

func newComboCallRecorder() *comboCallRecorder {
	return &comboCallRecorder{byAttempt: make(map[Attempt][]attemptResult)}
}

func (r *comboCallRecorder) record(res attemptResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAttempt[res.att] = append(r.byAttempt[res.att], res)
}

// take returns the earliest unclaimed result for att, or false when
// the adapters never recorded one (a wiring bug — the caller must
// treat it as an error, never invent a result).
func (r *comboCallRecorder) take(att Attempt) (attemptResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.byAttempt[att]
	if len(q) == 0 {
		return attemptResult{}, false
	}
	res := q[0]
	r.byAttempt[att] = q[1:]
	return res, true
}

// comboPanelCaller adapts the engine's real leaf executor to the
// panel PanelCaller: one real callChat per panel target, with the
// real client, the real ctx deadline, and the original request.
// It returns the response text ("" on error — the primitives treat
// an empty success as a failure, so nothing is fabricated).
func (e *Engine) comboPanelCaller(req provider.ChatRequest, rec *comboCallRecorder) PanelCaller {
	return func(ctx context.Context, att Attempt) (string, error) {
		res := e.callChat(ctx, att, req)
		rec.record(res)
		if res.err != nil {
			return "", res.err
		}
		if res.resp == nil {
			return "", errors.New("router: combo panel attempt returned no response")
		}
		return res.resp.Content, nil
	}
}

// comboPipelineCaller adapts the engine's real leaf executor to the
// pipeline PipelineCaller: each stage dials its target with a
// request carrying the stage input (original prompt for stage 1,
// prompt + previous output for later stages) as the single user
// message. The compression stamp is cleared: it describes the
// original prompt, not the threaded stage input.
func (e *Engine) comboPipelineCaller(req provider.ChatRequest, rec *comboCallRecorder) PipelineCaller {
	return func(ctx context.Context, att Attempt, input string) (string, error) {
		stageReq := req
		stageReq.Messages = []provider.Message{{Role: RoleUser, Content: input}}
		stageReq.Compressed = nil
		res := e.callChat(ctx, att, stageReq)
		rec.record(res)
		if res.err != nil {
			return "", res.err
		}
		if res.resp == nil {
			return "", errors.New("router: combo pipeline stage returned no response")
		}
		return res.resp.Content, nil
	}
}

// runComboStrategy executes one combo-strategy request over the
// already-ordered, context-fit chain and returns the winning
// response. trace is the request's trace (attempts are recorded on
// it as they resolve); contract / tree / treeRoot / treeOrdered /
// mode are the request's routing context, carried so the decision
// record matches the sequential path's. The caller owns trace
// publication (setLastTrace on the chat path, the deferred
// publishTrace on the stream path).
//
// The panel is capped at Config.MaxAttempts: a chain
// longer than the attempt budget is trimmed to the budget and the
// dropped members are traced as skipped_max_attempts, exactly as
// the sequential loop stops after MaxAttempts attempts. A
// zero/negative budget attempts nothing (ErrNoAvailableAttempt),
// exactly like the sequential loop.
func (e *Engine) runComboStrategy(ctx context.Context, ordered []Attempt, req provider.ChatRequest, trace *routetrace.RouteTrace, strategy string, contract *routetrace.RoutingContract, tree *strategytree.Node, treeRoot string, treeOrdered bool, mode string) (*provider.ChatResponse, error) {
	if len(ordered) == 0 {
		return nil, ErrNoAvailableAttempt
	}
	maxAttempts := e.cfg.MaxAttempts
	if maxAttempts <= 0 {
		// Aligned with the sequential loop (router.go: the loop
		// condition `attemptsUsed < maxAttempts` attempts
		// nothing and the request ends ErrNoAvailableAttempt):
		// a zero/negative budget means no attempts, not a
		// silent default to 5.
		return nil, ErrNoAvailableAttempt
	}
	if len(ordered) > maxAttempts {
		for _, att := range ordered[maxAttempts:] {
			addTraceAttempt(trace, att, 0, "skipped_max_attempts", nil)
		}
		ordered = ordered[:maxAttempts]
	}
	// RT-3: the fan-out panel bypasses the sequential per-attempt
	// budget gate — comboPanelCaller dials callChat directly, so
	// budgetGate never runs for a panel member — and the server
	// pre-send check prices only ONE attempt at model level. Price
	// the whole panel BEFORE fan-out (fusion / chaos-mode / jev
	// only): in strict mode the summed prompt estimates must fit
	// the budget, or the request is refused with zero provider
	// calls. Pipeline is sequential (one stage at a time), so it
	// does not fan out and is not covered here.
	if strategy == "fusion" || strategy == "chaos-mode" || strategy == "jev" {
		if err := e.comboPanelBudgetCheck(req, ordered, trace); err != nil {
			return nil, err
		}
	}
	prompt := comboPromptText(req)
	rec := newComboCallRecorder()
	consecutive429 := 0

	switch strategy {
	case "fusion", "chaos-mode":
		// chaos-mode IS fusion over the panel chain: parallel
		// fan-out + judge. No fault injection (see the file
		// header).
		//
		// Judge policy (A2-3, A2-3b): fusion ALWAYS runs the real
		// LLM judge (llmJudge) — the operator-configured judge
		// model (Config.JudgeModel) when set, else the FIRST PANEL
		// MEMBER (the fusion default: no configuration needed). Once
		// the judge picked a winner over MORE THAN ONE successful
		// panel answer, fusion synthesises ONE grounded answer from
		// the panel (see synthesizeFusion). A judge failure falls
		// back to the deterministic HeuristicJudge, and then the
		// judged winner is served VERBATIM (no synthesis). A single
		// survivor / single target is served directly (RunFusion's
		// minPanel rule), so a one-member panel never pays for a
		// judge call.
		fusionCfg := FusionConfig{PanelConfig: e.comboPanelConfig(), Judge: HeuristicJudge{}}
		usedLLM := false
		var judgeAtt Attempt
		if strategy == "fusion" {
			if ja, ok := e.comboJudgeAttempt(req, ordered); ok {
				judgeAtt = ja
				usedLLM = true
				fusionCfg.Judge = fallbackJudge{
					primary:    &llmJudge{engine: e, judge: ja, base: req},
					onFallback: func(error) { usedLLM = false },
				}
			}
		}
		res, err := RunFusion(ctx, prompt, ordered, e.comboPanelCaller(req, rec), fusionCfg)
		if err != nil {
			// RunFusion returns an empty FusionResult on
			// error (no Candidates to walk), so record from
			// the ordered chain via the recorder instead.
			e.recordOrderedAttempts(trace, ordered, rec, &consecutive429, mode, req.CallerID, req.Model)
			return nil, err
		}
		successes := 0
		for _, c := range res.Candidates {
			if c.Err == nil {
				successes++
			}
		}
		resp, ferr := e.finishCombo(trace, res.Candidates, res.WinnerIndex, strategy, rec, &consecutive429, mode, req, contract, tree, treeRoot, treeOrdered)
		if ferr != nil {
			return nil, ferr
		}
		// Synthesise only when the LLM judge actually judged, there
		// is more than one answer to fuse, and the winner is a plain
		// text turn (a tool-calling winner is served verbatim — the
		// synthesis is text-only and must not drop a tool call). A
		// synthesis failure leaves the judged winner's verbatim text.
		if usedLLM && successes > 1 && len(resp.ToolCalls) == 0 {
			if synth, serr := e.synthesizeFusion(ctx, prompt, res.Candidates, judgeAtt, req); serr == nil && strings.TrimSpace(synth) != "" {
				resp.Content = synth
			}
		}
		return resp, nil
	case "pipeline":
		// A2-7 pipeline knobs, surfaced and honoured here:
		// PipelineSkipTokensUnder skips the multi-stage pipeline for a
		// short prompt, PipelineTaskDetection "auto" skips it for a
		// simple (non-code) task, and PipelineFallback "single-provider"
		// serves a single provider when the pipeline fails.
		// PipelineMaxReflectionLoops > 0 appends that many real
		// reflection stages after the main stages: each reflection
		// stage re-examines the original prompt plus the previous
		// stage's output and produces a revised output (threaded
		// forward by pipelineStageInput), targeting the last main
		// stage's provider. The stages are Optional, so a reflection
		// failure is recorded as skipped and never aborts the
		// pipeline; 0 (the default) appends nothing, so reflection is
		// observably OFF — no extra stage, no extra provider call,
		// and the final Text is the last main stage's output.
		if e.pipelineWantsSingle(prompt) {
			return e.pipelineServeSingle(ctx, ordered, req, rec, trace, &consecutive429, mode, contract, tree, treeRoot, treeOrdered)
		}
		stages := make([]PipelineStage, len(ordered))
		for i, att := range ordered {
			stages[i] = PipelineStage{Name: fmt.Sprintf("stage-%d", i+1), Target: att}
		}
		if loops := e.cfg.PipelineMaxReflectionLoops; loops > 0 && len(ordered) > 0 {
			reflTarget := ordered[len(ordered)-1]
			for k := 1; k <= loops; k++ {
				stages = append(stages, PipelineStage{
					Name:     fmt.Sprintf("reflection-%d", k),
					Target:   reflTarget,
					Optional: true,
				})
			}
		}
		pres, err := RunPipeline(ctx, prompt, stages, e.comboPipelineCaller(req, rec))
		if err != nil {
			// A2-7: pipeline_fallback "single-provider" — on a pipeline
			// failure, record the stages that ran, then serve a single
			// provider (ordered[0]) as a fresh call instead of failing
			// the request. Any other PipelineFallback value (or "") keeps
			// the fail-on-error behaviour.
			if e.cfg.PipelineFallback == "single-provider" {
				e.recordPipelineStages(trace, pres.Stages, rec, &consecutive429, mode, req.CallerID, req)
				return e.pipelineServeSingle(ctx, ordered, req, rec, trace, &consecutive429, mode, contract, tree, treeRoot, treeOrdered)
			}
			e.recordPipelineStages(trace, pres.Stages, rec, &consecutive429, mode, req.CallerID, req)
			return nil, err
		}
		lastRes, ok := e.recordPipelineStages(trace, pres.Stages, rec, &consecutive429, mode, req.CallerID, req)
		if !ok {
			return nil, errors.New("router: combo pipeline lost the last stage result")
		}
		e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, lastRes.att.Model, lastRes.att.ProviderID)
		noteTreeServed(trace, tree, lastRes.att.ProviderID, lastRes.att.Model, treeRoot)
		return e.finishResponse(lastRes), nil
	case "jev":
		scorer := JevScorer(heuristicJevScorer{})
		// A2-4: when the operator turned the JEV LLM judge ON
		// (config routing.jev_judge_enabled) and a judge model
		// resolves (config routing.jev_judge_model), run the real
		// judge call (llmJevScorer) — the configured JEV judge
		// model. It must produce choice+probabilities. A judge
		// failure (or an off flag / unresolvable judge model) falls
		// back to heuristicJevScorer via fallbackJevScorer, so the
		// preference order is unchanged whenever the flag is off or
		// the judge call fails. Never calls any vendor decision API.
		if e.cfg.ComboJevLLMJudge {
			if ja, ok := e.jevJudgeAttempt(req, ordered); ok {
				scorer = fallbackJevScorer{
					primary:    &llmJevScorer{engine: e, judge: ja, base: req},
					onFallback: func(error) {},
				}
			}
		}
		pres, err := RunJevPanel(ctx, prompt, ordered, e.comboPanelCaller(req, rec), scorer, PanelConfig{})
		if err != nil {
			// As in the fusion branch: RunJevPanel returns
			// an empty result on error, so record from the
			// ordered chain via the recorder.
			e.recordOrderedAttempts(trace, ordered, rec, &consecutive429, mode, req.CallerID, req.Model)
			return nil, err
		}
		return e.finishJev(trace, pres, rec, &consecutive429, mode, req, contract, tree, treeRoot, treeOrdered)
	default:
		return nil, fmt.Errorf("router: runComboStrategy called for non-combo strategy %q", strategy)
	}
}

// comboPanelBudgetCheck prices every member of a fan-out panel
// (fusion / chaos-mode / jev) BEFORE any of them is dialled, and
// refuses in strict mode when the summed prompt estimates exceed
// the request's budget (RT-3). It reuses budgetGate per member
// with the running total as spentUSD, so the check is exactly
// "sum of the panel's estimates vs the budget": budgetGate's own
// rules apply unchanged — no budget, no PriceFunc, or a $0
// estimate never blocks, and non-strict mode never blocks (the
// server records the overage post-hoc). On refusal every panel
// member is traced as skipped_budget (none was dialled) and the
// returned error is the typed RequestBudgetExceededError naming
// the first member whose estimate pushed the sum over — the
// sequential path's refusal, at panel scale.
func (e *Engine) comboPanelBudgetCheck(req provider.ChatRequest, ordered []Attempt, trace *routetrace.RouteTrace) error {
	if req.BudgetUSD == nil || !req.BudgetStrict {
		return nil
	}
	var total float64
	for _, att := range ordered {
		blocked, est := e.budgetGate(req, att, total)
		if blocked {
			be := e.budgetExceededError(req, att, total, est)
			for _, m := range ordered {
				addTraceAttempt(trace, m, 0, "skipped_budget", be)
			}
			return be
		}
		total += est
	}
	return nil
}

// recordOrderedAttempts records every attempt in ordered on the
// trace via the recorder: failures via noteFailure, successes (a
// primitive errored after the panel answered, e.g. a judge
// failure) as hedged losers. Used when a primitive returns an
// error with no candidate list to walk.
func (e *Engine) recordOrderedAttempts(trace *routetrace.RouteTrace, ordered []Attempt, rec *comboCallRecorder, consecutive429 *int, mode, keyID, chainKey string) {
	for _, att := range ordered {
		res, ok := rec.take(att)
		if !ok {
			continue
		}
		if res.err != nil {
			e.noteFailure(trace, res, consecutive429, mode, keyID, chainKey)
		} else {
			e.noteHedgedLoser(trace, res, keyID)
		}
	}
}

// finishCombo records a fusion/chaos panel on the trace — winner as
// success, every other successful member as a hedged loser, every
// failure via noteFailure — then records the decision and returns
// the winner's response verbatim. winnerIndex is RunFusion's
// WinnerIndex (index into candidates).
func (e *Engine) finishCombo(trace *routetrace.RouteTrace, candidates []PanelResult, winnerIndex int, strategy string, rec *comboCallRecorder, consecutive429 *int, mode string, req provider.ChatRequest, contract *routetrace.RoutingContract, tree *strategytree.Node, treeRoot string, treeOrdered bool) (*provider.ChatResponse, error) {
	if winnerIndex < 0 || winnerIndex >= len(candidates) {
		return nil, fmt.Errorf("router: combo winner index %d out of range for %d candidates", winnerIndex, len(candidates))
	}
	var winnerRes attemptResult
	foundWinner := false
	for i, c := range candidates {
		res, ok := rec.take(c.Attempt)
		if !ok {
			return nil, fmt.Errorf("router: combo lost the attempt result for %s", c.Label())
		}
		switch {
		case c.Err != nil:
			e.noteFailure(trace, res, consecutive429, mode, req.CallerID, req.Model)
		case i == winnerIndex:
			winnerRes = res
			foundWinner = true
			e.noteSuccess(trace, res, req)
		default:
			e.noteHedgedLoser(trace, res, req.CallerID)
		}
	}
	if !foundWinner {
		// The winner slot errored (or was never recorded): the
		// switch above routes an errored winner to noteFailure,
		// so winnerRes stays unset. Serving it would return an
		// empty response with no error — fail honestly instead.
		// Unreachable via RunFusion's contract (WinnerIndex
		// always indexes a success), but the guard is cheap.
		return nil, fmt.Errorf("router: combo winner %d was not a successful candidate", winnerIndex)
	}
	e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, winnerRes.att.Model, winnerRes.att.ProviderID)
	noteTreeServed(trace, tree, winnerRes.att.ProviderID, winnerRes.att.Model, treeRoot)
	return e.finishResponse(winnerRes), nil
}

// recordPipelineStages records every executed pipeline stage on the
// trace in execution order: successes via noteSuccess, failures
// (including skipped optional stages) via noteFailure. Successes
// are recorded against the ORIGINAL request, not the threaded stage
// input: the trace/ledger/breaker half is identical either way, and
// the original request is what the reasoning capture must key the
// final turn on (the stage inputs are internal threading, not the
// user transcript). Two honest caveats follow from that choice:
// the local token estimate on a stage's ledger row is estimated
// from the original prompt, not the longer threaded stage input
// (provider-reported numbers are never touched — see
// reconcileUsage); and an intermediate stage's reasoning state is
// stored under a key its response text names, so only the last
// stage's state is ever restored by a later turn.
//
// It returns the last successful stage's attemptResult (and
// whether one was recorded): the caller needs it for the decision
// record and the response, and the recorder is consumed by this
// walk, so the result cannot be taken again afterwards. A skipped
// (optional, failed) stage never becomes the served result — the
// last good stage does, matching PipelineResult.Text (the last
// non-skipped stage's output). When no stage succeeded there is
// nothing to serve, so ok is false and the caller errors.
func (e *Engine) recordPipelineStages(trace *routetrace.RouteTrace, stages []PipelineStageResult, rec *comboCallRecorder, consecutive429 *int, mode, keyID string, req provider.ChatRequest) (attemptResult, bool) {
	var lastRes attemptResult
	lastOK := false
	for _, s := range stages {
		res, ok := rec.take(s.Stage.Target)
		if !ok {
			continue
		}
		if s.Err == nil {
			lastRes, lastOK = res, true
		}
		if s.Err != nil {
			e.noteFailure(trace, res, consecutive429, mode, keyID, req.Model)
		} else {
			e.noteSuccess(trace, res, req)
		}
	}
	return lastRes, lastOK
}

// pipelineTokenEstimate estimates the token count of the pipeline
// prompt for the skip_under knob: ~4 chars per token, the same budget
// heuristic relayEstimateTokens uses (relay.go). A budget guard, never
// a billing count.
func pipelineTokenEstimate(prompt string) int {
	return len(prompt) / 4
}

// detectTaskComplex reports whether a pipeline prompt is a "complex"
// task (A2-7 task_detection "auto"): a code fence or a code/planning
// marker. A prompt with none of these is a simple task, which a single
// provider can answer — the multi-stage pipeline adds nothing. This is
// a deliberately small, honest heuristic, never a model judgement.
func detectTaskComplex(prompt string) bool {
	p := strings.ToLower(strings.TrimSpace(prompt))
	if p == "" {
		return false
	}
	for _, m := range []string{
		"```", "func ", "function", "implement", "refactor",
		"debug", "write a program", "write code", "architecture",
		"plan ",
	} {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// pipelineWantsSingle reports whether the pipeline should be skipped
// in favour of a single provider (A2-7): when the prompt is short
// enough (PipelineSkipTokensUnder) or — under task_detection "auto" —
// is a simple non-code task. The multi-stage pipeline is for complex,
// long work; serving a single provider for the simple/short case is the
// documented cost/quality win.
func (e *Engine) pipelineWantsSingle(prompt string) bool {
	if e.cfg.PipelineSkipTokensUnder > 0 && pipelineTokenEstimate(prompt) < e.cfg.PipelineSkipTokensUnder {
		return true
	}
	if e.cfg.PipelineTaskDetection == "auto" && !detectTaskComplex(prompt) {
		return true
	}
	return false
}

// pipelineServeSingle serves the pipeline request by a single provider
// (ordered[0]) instead of running the multi-stage pipeline (A2-7): a
// short/simple prompt, or a single-provider fallback after a pipeline
// failure. It dials ordered[0] once, records it as the served success,
// traces the remaining members as skipped_pipeline (they never ran),
// records the decision as "pipeline", and returns the response. If the
// single provider fails, the failure is recorded and the error returned.
func (e *Engine) pipelineServeSingle(ctx context.Context, ordered []Attempt, req provider.ChatRequest, rec *comboCallRecorder, trace *routetrace.RouteTrace, consecutive429 *int, mode string, contract *routetrace.RoutingContract, tree *strategytree.Node, treeRoot string, treeOrdered bool) (*provider.ChatResponse, error) {
	if len(ordered) == 0 {
		return nil, ErrNoAvailableAttempt
	}
	att := ordered[0]
	res := e.callChat(ctx, att, req)
	rec.record(res)
	if res.err != nil {
		e.noteFailure(trace, res, consecutive429, mode, req.CallerID, req.Model)
		return nil, res.err
	}
	if res.resp == nil {
		return nil, errors.New("router: combo pipeline single provider returned no response")
	}
	e.noteSuccess(trace, res, req)
	for _, a := range ordered[1:] {
		addTraceAttempt(trace, a, 0, "skipped_pipeline", nil)
	}
	e.recordDecision(trace, contract, treeOrdered, "pipeline", req.Model, res.att.Model, res.att.ProviderID)
	noteTreeServed(trace, tree, res.att.ProviderID, res.att.Model, treeRoot)
	return e.finishResponse(res), nil
}

// finishJev records a JEV panel on the trace and returns the
// admitted winner's response.
//
// RunJevPanel returns a verdict, not a winner: Admitted[0] is the
// winner (the scorer's choice, or the first candidate in user
// order when the judge fell open). Held candidates were executed
// by the fan-out but must not serve: they are traced as held_jev
// (a distinct outcome so the verdict is visible on the trace),
// admitted non-winners as hedged losers, failures via noteFailure.
// There is NO hold-error in the primitive's contract — a hold is a
// verdict row, never a fabricated answer and never an error — so
// a hold is preserved as a trace outcome, not converted into a
// fake response.
func (e *Engine) finishJev(trace *routetrace.RouteTrace, pres JevPanelResult, rec *comboCallRecorder, consecutive429 *int, mode string, req provider.ChatRequest, contract *routetrace.RoutingContract, tree *strategytree.Node, treeRoot string, treeOrdered bool) (*provider.ChatResponse, error) {
	if len(pres.Verdict.Admitted) == 0 {
		return nil, errors.New("router: jev verdict admitted nothing")
	}
	winnerLabel := pres.Verdict.Admitted[0].ID
	held := make(map[string]bool, len(pres.Verdict.Held))
	for _, h := range pres.Verdict.Held {
		held[h.ID] = true
	}
	var winnerRes attemptResult
	foundWinner := false
	for _, r := range pres.Results {
		res, ok := rec.take(r.Attempt)
		if !ok {
			return nil, fmt.Errorf("router: combo lost the attempt result for %s", r.Label())
		}
		switch {
		case r.Err != nil:
			e.noteFailure(trace, res, consecutive429, mode, req.CallerID, req.Model)
		case r.Label() == winnerLabel:
			winnerRes = res
			foundWinner = true
			e.noteSuccess(trace, res, req)
		case held[r.Label()]:
			// Held by the JEV verdict: executed, but must not
			// serve. Trace outcome held_jev (not success, not
			// a hedged race loss); ledger/breaker handling
			// mirrors noteHedgedLoser — the attempt did not
			// serve this request.
			addTraceAttemptReasoning(trace, res.att, res.latencyMs, "held_jev", nil, res.reasoning)
			if e.ledger != nil && res.called {
				e.recordAttempt(req.CallerID, res.att, false, res.latencyMs, provider.Usage{})
			}
			e.breakerNeutral(res.att.ProviderID)
		default:
			e.noteHedgedLoser(trace, res, req.CallerID)
		}
	}
	if !foundWinner {
		return nil, errors.New("router: jev verdict winner was not among the executed candidates")
	}
	e.recordDecision(trace, contract, treeOrdered, "jev", req.Model, winnerRes.att.Model, winnerRes.att.ProviderID)
	noteTreeServed(trace, tree, winnerRes.att.ProviderID, winnerRes.att.Model, treeRoot)
	return e.finishResponse(winnerRes), nil
}

// replayComboResult emits a combo result verbatim as stream events:
// one Delta carrying the full content, then a Done event carrying
// the full response (including tool calls and finish reason). This
// is a REPLAY, not a live stream — the panel already ran
// non-streaming — and the trace already records the combo path
// (see runComboStrategy), so the Done event's Resp is the honest
// source for the drain, not a claim about a live relay.
func (e *Engine) replayComboResult(ctx context.Context, out chan<- StreamEvent, resp *provider.ChatResponse) {
	if resp == nil {
		e.sendTerminal(out, errors.New("router: combo produced no response to replay"))
		return
	}
	if resp.Content != "" {
		select {
		case out <- StreamEvent{Delta: resp.Content}:
		case <-ctx.Done():
			e.sendTerminal(out, ctx.Err())
			return
		}
	}
	done := StreamEvent{Done: true, FinishReason: resp.FinishReason, Resp: resp}
	for i, tc := range resp.ToolCalls {
		done.ToolCalls = append(done.ToolCalls, provider.ChunkToolCall{
			Index:     i,
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	select {
	case out <- done:
	case <-ctx.Done():
		e.sendTerminal(out, ctx.Err())
	}
}

// ---------------------------------------------------------------------------
// Judges
// ---------------------------------------------------------------------------

// llmJudge is a Judge backed by one real engine attempt: it sends
// the original prompt plus the numbered candidates to the judge
// attempt's provider/model and parses "WINNER: N" (1-based) from
// the reply. It calls e.callChat directly — a leaf, never the
// combo interception — so it cannot recurse. An unparseable reply,
// an out-of-range winner, or a failed judge call is an ERROR, never
// an invented judgement: the caller (fallbackJudge) falls back to
// HeuristicJudge on exactly that signal.
//
// No judge model is configured (see the file header), so the
// engine does not use llmJudge by default; it exists, tested, for
// the day a judge model is plumbed.
type llmJudge struct {
	engine *Engine
	judge  Attempt
	// base is the request the judge prompt is built from (model,
	// temperature and friends carry over; messages are replaced
	// by the judge prompt, and the compression stamp is cleared
	// — it describes the original prompt, not the judge prompt).
	base provider.ChatRequest
}

// Score implements Judge for llmJudge.
func (j *llmJudge) Score(ctx context.Context, prompt string, candidates []PanelResult) (int, error) {
	if j == nil || j.engine == nil {
		return 0, errors.New("llm judge: no engine")
	}
	if len(candidates) == 0 {
		return 0, errors.New("llm judge: no candidates to score")
	}
	var sb strings.Builder
	sb.WriteString("Original request:\n")
	sb.WriteString(prompt)
	sb.WriteString("\n\nCandidates:\n")
	for i, c := range candidates {
		fmt.Fprintf(&sb, "\nCandidate %d (from %s):\n%s\n", i+1, c.Label(), c.Text)
	}
	sb.WriteString("\nReply with exactly one line: WINNER: <number> (1-based) naming the best candidate.")
	judgeReq := j.base
	judgeReq.Messages = []provider.Message{{Role: RoleUser, Content: sb.String()}}
	judgeReq.Compressed = nil
	res := j.engine.callChat(ctx, j.judge, judgeReq)
	if res.err != nil {
		return 0, fmt.Errorf("llm judge: judge call failed: %w", res.err)
	}
	if res.resp == nil {
		return 0, errors.New("llm judge: judge call returned no response")
	}
	winner, err := parseJudgeWinner(res.resp.Content, len(candidates))
	if err != nil {
		return 0, fmt.Errorf("llm judge: %w", err)
	}
	return winner, nil
}

// parseJudgeWinner parses "WINNER: N" (1-based, case-insensitive)
// from a judge reply and returns the 0-based index. Anything else
// is an error — a winner is never guessed from free text.
func parseJudgeWinner(reply string, nCandidates int) (int, error) {
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 7 || !strings.EqualFold(line[:7], "winner:") {
			continue
		}
		num := strings.TrimSpace(line[7:])
		// Take the leading integer token ("2", "2." or "2)").
		num = strings.TrimRight(num, ".)")
		fields := strings.Fields(num)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if v < 1 || v > nCandidates {
			return 0, fmt.Errorf("judge returned out-of-range winner %d for %d candidates", v, nCandidates)
		}
		return v - 1, nil
	}
	return 0, errors.New("judge reply named no winner (expected \"WINNER: N\")")
}

// fallbackJudge tries a primary Judge and falls back to
// HeuristicJudge when the primary fails. onFallback, when set, is
// called with the primary's error so the caller can record the
// fallback (on the trace) — the request never fails because the
// judge did, and the fallback is never silent.
type fallbackJudge struct {
	primary    Judge
	onFallback func(err error)
}

// Score implements Judge for fallbackJudge.
func (j fallbackJudge) Score(ctx context.Context, prompt string, candidates []PanelResult) (int, error) {
	winner, err := j.primary.Score(ctx, prompt, candidates)
	if err == nil {
		return winner, nil
	}
	if j.onFallback != nil {
		j.onFallback(err)
	}
	return HeuristicJudge{}.Score(ctx, prompt, candidates)
}

// comboPanelConfig builds the panel config for the combo strategies
// (fusion / jev / chaos-mode) from the operator's Config knobs (A2-3):
// maxPanel (default 40, fusion.ts FUSION_DEFAULTS), panelHardTimeoutMs
// (default 90 s), minPanel (default 2) and stragglerGraceMs (default 0
// = join all). Zero/negative values keep the documented defaults.
func (e *Engine) comboPanelConfig() PanelConfig {
	c := PanelConfig{MaxPanel: DefaultFusionMaxPanel}
	if e.cfg.ComboMaxPanel > 0 {
		c.MaxPanel = e.cfg.ComboMaxPanel
	}
	if e.cfg.ComboMinPanel > 0 {
		c.MinPanel = e.cfg.ComboMinPanel
	}
	if e.cfg.ComboPanelHardTimeoutMs > 0 {
		c.Timeout = time.Duration(e.cfg.ComboPanelHardTimeoutMs) * time.Millisecond
	}
	if e.cfg.ComboStragglerGraceMs > 0 {
		c.StragglerGrace = time.Duration(e.cfg.ComboStragglerGraceMs) * time.Millisecond
	}
	return c
}

// comboJudgeAttempt resolves the LLM judge's target for a fusion panel
// (A2-3): the attempt serving the configured judge model
// (Config.JudgeModel) when it is resolvable — an attempt already on the
// panel, else the first attempt of that model's own chain — otherwise
// the first panel member (ordered[0]), the fusion default. It reports
// false only when there is no panel at all.
func (e *Engine) comboJudgeAttempt(req provider.ChatRequest, ordered []Attempt) (Attempt, bool) {
	if len(ordered) == 0 {
		return Attempt{}, false
	}
	if e.cfg.JudgeModel != nil {
		if jm := strings.TrimSpace(e.cfg.JudgeModel(req.Model)); jm != "" {
			for _, att := range ordered {
				if att.Model == jm {
					return att, true
				}
			}
			if chain := e.chainFor(jm); len(chain) > 0 {
				return chain[0], true
			}
		}
	}
	return ordered[0], true
}

// synthesizeFusion asks the judge model for ONE grounded answer built
// from the panel's successful answers (A2-3): the consensus, the
// contradictions, partial coverage, unique insights and blind spots,
// with the panel members anonymised as "Source N" (never their
// provider/model, and never the winner's own label). It makes exactly
// one callChat — never a fan-out — and returns the judge's reply or an
// error; the caller falls back to the judged winner's verbatim text on
// error (a failed synthesis never fails the request).
func (e *Engine) synthesizeFusion(ctx context.Context, prompt string, candidates []PanelResult, judge Attempt, base provider.ChatRequest) (string, error) {
	var sb strings.Builder
	sb.WriteString("You are the synthesis judge for a panel of independent answers to the same user request. ")
	sb.WriteString("Write ONE grounded final answer for the user.\n\nRules:\n")
	sb.WriteString("- Use only the panel's content; never invent facts, sources or citations.\n")
	sb.WriteString("- Lead with what the sources agree on (consensus).\n")
	sb.WriteString("- Where sources contradict each other, prefer the better-supported statement and say so briefly.\n")
	sb.WriteString("- Keep points only some sources raised (partial coverage) and any unique insight worth keeping.\n")
	sb.WriteString("- Note blind spots: what none of the sources addressed.\n")
	sb.WriteString("- Do not name the sources in the final answer; write it as one answer.\n\n")
	sb.WriteString("Original request:\n")
	sb.WriteString(prompt)
	sb.WriteString("\n\nPanel answers (anonymised):\n")
	n := 0
	for _, c := range candidates {
		if c.Err != nil || strings.TrimSpace(c.Text) == "" {
			continue
		}
		n++
		fmt.Fprintf(&sb, "\nSource %d:\n%s\n", n, c.Text)
	}
	if n == 0 {
		return "", errors.New("fusion synthesis: no successful panel answers to synthesise")
	}
	sb.WriteString("\nWrite the final answer now, and nothing else.")
	req := base
	req.Messages = []provider.Message{{Role: RoleUser, Content: sb.String()}}
	req.Compressed = nil
	res := e.callChat(ctx, judge, req)
	if res.err != nil {
		return "", fmt.Errorf("fusion synthesis: judge call failed: %w", res.err)
	}
	if res.resp == nil || strings.TrimSpace(res.resp.Content) == "" {
		return "", errors.New("fusion synthesis: judge returned no answer")
	}
	return res.resp.Content, nil
}

// heuristicJevScorer is the JevScorer for the engine JEV path. It
// derives probabilities from the same deterministic heuristic the
// HeuristicJudge uses (capped length minus refusal penalties,
// clamped at zero, normalised to sum 1); the choice is the argmax
// and the confidence is the winner's normalised share. These are
// HEURISTIC-derived numbers, not a model's judgement — they are
// labelled as such wherever the verdict is surfaced (the trace
// outcome held_jev / hedged_cancelled names the verdict, never a
// model score). When the heuristic cannot distinguish the
// candidates at all (every score clamps to zero) it reports
// unavailable (nil, nil) so JudgeJev falls open instead of
// inventing probabilities.
type heuristicJevScorer struct{}

// ScoreJev implements JevScorer for heuristicJevScorer.
func (heuristicJevScorer) ScoreJev(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error) {
	if len(candidates) == 0 {
		return nil, errors.New("jev scorer: no candidates to score")
	}
	scores := make([]float64, len(candidates))
	var total float64
	for i, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := heuristicScore(c.Text)
		if s < 0 {
			s = 0
		}
		scores[i] = float64(s)
		total += scores[i]
	}
	if total <= 0 {
		return nil, nil // indistinguishable: unavailable, JudgeJev falls open
	}
	probs := make(map[string]float64, len(candidates))
	best := 0
	for i, c := range candidates {
		probs[c.Label()] = scores[i] / total
		if scores[i] > scores[best] {
			best = i
		}
	}
	return &JevChoiceAnswer{
		Choice:        candidates[best].Label(),
		Probabilities: probs,
		Confidence:    probs[candidates[best].Label()],
	}, nil
}

// llmJevScorer is a JevScorer backed by one real engine attempt: it
// sends the original prompt plus the numbered candidates to the
// judge attempt's provider/model and parses a judge answer
// (choice + confidence + per-candidate probabilities) from the
// reply. It calls e.callChat directly — a leaf, never the combo
// interception — so it cannot recurse. An unparseable reply, an
// out-of-range winner, probabilities that do not sum sensibly, or a
// failed judge call is an ERROR, never an invented judgement: the
// caller (fallbackJevScorer) falls back to heuristicJevScorer on
// exactly that signal. It is gated behind Config.ComboJevLLMJudge
// (A2-4) and never calls any vendor decision API.
type llmJevScorer struct {
	engine *Engine
	judge  Attempt
	// base is the request the judge prompt is built from (model,
	// temperature and friends carry over; messages are replaced by
	// the judge prompt, and the compression stamp is cleared — it
	// describes the original prompt, not the judge prompt).
	base provider.ChatRequest
}

// ScoreJev implements JevScorer for llmJevScorer.
func (s *llmJevScorer) ScoreJev(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error) {
	if s == nil || s.engine == nil {
		return nil, errors.New("llm jev scorer: no engine")
	}
	if len(candidates) == 0 {
		return nil, errors.New("llm jev scorer: no candidates to score")
	}
	var sb strings.Builder
	sb.WriteString("Original request:\n")
	sb.WriteString(prompt)
	sb.WriteString("\n\nCandidates:\n")
	for i, c := range candidates {
		fmt.Fprintf(&sb, "\nCandidate %d (from %s):\n%s\n", i+1, c.Label(), c.Text)
	}
	sb.WriteString("\nReply with exactly one line: WINNER: <number> CONFIDENCE: <0-1> PROBS: <p1> <p2> ... <pN> naming the best candidate (1-based), your confidence in that choice, and a probability for every candidate (in 1-based order) summing to 1.")
	req := s.base
	req.Messages = []provider.Message{{Role: RoleUser, Content: sb.String()}}
	req.Compressed = nil
	res := s.engine.callChat(ctx, s.judge, req)
	if res.err != nil {
		return nil, fmt.Errorf("llm jev scorer: judge call failed: %w", res.err)
	}
	if res.resp == nil {
		return nil, errors.New("llm jev scorer: judge call returned no response")
	}
	return parseJevJudge(res.resp.Content, candidates)
}

var (
	jevWinnerRe     = regexp.MustCompile(`(?i)\bWINNER:\s*(\d+)`)
	jevConfidenceRe = regexp.MustCompile(`(?i)\bCONFIDENCE:\s*([0-9]*\.?[0-9]+)`)
	jevProbsRe      = regexp.MustCompile(`(?i)\bPROBS:\s*([0-9.,\s]+)`)
)

// parseJevJudge parses the judge answer from a judge reply: the
// 1-based winner index, an overall confidence, and a probability for
// every candidate in 1-based order (whitespace or comma separated,
// summing to 1). Anything malformed is an error — a judgement is
// never guessed from free text. The returned answer's Choice is the
// winner's candidate Label and its Probabilities map label -> p.
func parseJevJudge(reply string, candidates []PanelResult) (*JevChoiceAnswer, error) {
	if len(candidates) == 0 {
		return nil, errors.New("jev judge: no candidates to parse")
	}
	wm := jevWinnerRe.FindStringSubmatch(reply)
	if wm == nil {
		return nil, errors.New("jev judge reply named no winner (expected \"WINNER: <number>\")")
	}
	winner, err := strconv.Atoi(wm[1])
	if err != nil || winner < 1 || winner > len(candidates) {
		return nil, fmt.Errorf("jev judge returned out-of-range winner %q for %d candidates", wm[1], len(candidates))
	}
	cm := jevConfidenceRe.FindStringSubmatch(reply)
	if cm == nil {
		return nil, errors.New("jev judge reply named no confidence (expected \"CONFIDENCE: <0-1>\")")
	}
	conf, err := strconv.ParseFloat(cm[1], 64)
	if err != nil || conf < 0 || conf > 1 {
		return nil, fmt.Errorf("jev judge returned out-of-range confidence %q", cm[1])
	}
	pm := jevProbsRe.FindStringSubmatch(reply)
	if pm == nil {
		return nil, errors.New("jev judge reply named no probabilities (expected \"PROBS: <p1> <p2> ... <pN>\")")
	}
	parts := strings.FieldsFunc(pm[1], func(r rune) bool { return r == ',' || r == ' ' || r == '	' })
	if len(parts) != len(candidates) {
		return nil, fmt.Errorf("jev judge returned %d probabilities for %d candidates", len(parts), len(candidates))
	}
	probs := make([]float64, len(parts))
	var total float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || v > 1 {
			return nil, fmt.Errorf("jev judge returned out-of-range probability %q", p)
		}
		probs[i] = v
		total += v
	}
	if total <= 0 {
		return nil, errors.New("jev judge probabilities sum to zero")
	}
	answer := &JevChoiceAnswer{
		Choice:        candidates[winner-1].Label(),
		Confidence:    conf,
		Probabilities: make(map[string]float64, len(candidates)),
	}
	for i, c := range candidates {
		answer.Probabilities[c.Label()] = probs[i]
	}
	return answer, nil
}

// fallbackJevScorer tries a primary JevScorer and falls back to
// heuristicJevScorer when the primary fails. onFallback, when set, is
// called with the primary's error so the caller can record the
// fallback — the request never fails because the judge did, and the
// fallback is never silent. When the flag is off, runComboStrategy
// never constructs a fallbackJevScorer at all (it keeps the plain
// heuristicJevScorer), so the preference order is unchanged.
type fallbackJevScorer struct {
	primary    JevScorer
	onFallback func(err error)
}

// ScoreJev implements JevScorer for fallbackJevScorer.
func (s fallbackJevScorer) ScoreJev(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error) {
	answer, err := s.primary.ScoreJev(ctx, prompt, candidates)
	if err == nil {
		return answer, nil
	}
	if s.onFallback != nil {
		s.onFallback(err)
	}
	return heuristicJevScorer{}.ScoreJev(ctx, prompt, candidates)
}

// jevJudgeAttempt resolves the JEV judge's target (A2-4): the
// attempt serving the configured JEV judge model (Config.
// JevJudgeModel) when it is resolvable — an attempt already on the
// panel, else the first attempt of that model's own chain. Unlike
// comboJudgeAttempt (fusion), it reports false when NO judge model
// is configured or the named model has no serving chain: with the
// JEV LLM judge on but no resolvable model there is nothing to
// judge with, so the caller keeps the heuristic scorer and never
// dials a panel member as a judge.
func (e *Engine) jevJudgeAttempt(req provider.ChatRequest, ordered []Attempt) (Attempt, bool) {
	if len(ordered) == 0 {
		return Attempt{}, false
	}
	if e.cfg.JevJudgeModel == nil {
		return Attempt{}, false
	}
	jm := strings.TrimSpace(e.cfg.JevJudgeModel(req.Model))
	if jm == "" {
		return Attempt{}, false
	}
	for _, att := range ordered {
		if att.Model == jm {
			return att, true
		}
	}
	if chain := e.chainFor(jm); len(chain) > 0 {
		return chain[0], true
	}
	return Attempt{}, false
}
