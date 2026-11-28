// Combo execution primitives: fusion / pipeline / jev (M3 Phase C).
//
// Pure logic over caller-supplied executors. This file makes NO network
// calls, reads NO config, and touches NO engine state: the coordinator
// wires these primitives into the engine afterwards by supplying a
// PanelCaller / PipelineCaller that performs one real attempt (and, for
// fusion/jev, a Judge / JevScorer backed by a real model call).
//
// Semantics are taken from the the reference router reference clone (study only,
// no code copied — see the per-section citations):
//
//   - fusion:  open-sse/services/fusion.ts — parallel panel + judge
//     synthesis. the reference router defaults there are minPanel 2, straggler
//     grace 8 s, hard timeout 90 s, maxPanel 40. This primitive
//     differs deliberately in two places: the default max panel is 5
//     (the auto/chaos panel size in parity tracker Area 1, and the task
//     brief), and collection joins ALL panel members instead of
//     the reference router's quorum-grace early return — the engine can layer
//     quorum-grace on top later; the primitive guarantees every
//     fan-out goroutine is joined.
//   - pipeline: open-sse/services/autoCombo/pipelineRouter.ts +
//     src/domain/pipeline.ts — multi-stage execution where each
//     stage's output is threaded forward as context for the next
//     stage (plan_context / execution_response / reflection_response
//     in the reference). A stage failure marks fallback and stops
//     the reference pipeline; here it aborts with an error unless
//     the stage is marked Optional.
//   - jev: open-sse/services/combo/jevStrategy.ts — pure judge over
//     a caller-supplied choice answer. Floors and cap are verbatim:
//     probability floor 0.15, confidence floor 0.65, max 4 admitted
//     (jevStrategy.ts:11-13). Low confidence or an unavailable /
//     out-of-set choice falls open to the full user order.
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// PanelCaller executes one attempt and returns its response text.
// It MUST respect ctx (return promptly when ctx is done); the panel
// primitives pass a ctx carrying the configured per-call timeout, and
// they join every goroutine they spawn, so a caller that ignores ctx
// is the only way a fan-out can outlive its deadline.
type PanelCaller func(ctx context.Context, att Attempt) (string, error)

// PanelResult is one panel member's outcome, index-aligned with the
// targets it was fanned out over. Exactly one of Text / Err is set:
// a nil error with empty/whitespace text is normalised to an error
// ("empty response") by the fan-out, so Text is never an empty
// success — an empty reply is a failure, not a candidate.
type PanelResult struct {
	Attempt Attempt
	Text    string
	Err     error
}

// Label identifies a target the way errors name it: "provider/model".
func (r PanelResult) Label() string { return attemptLabel(r.Attempt) }

func attemptLabel(att Attempt) string {
	if att.ProviderID == "" {
		return att.Model
	}
	return att.ProviderID + "/" + att.Model
}

// PanelConfig bounds a parallel fan-out. Zero values take the defaults.
type PanelConfig struct {
	// MaxPanel caps the fan-out size. <= 0 means DefaultPanelMax.
	// the reference router's fusion default is 40 (fusion.ts FUSION_DEFAULTS);
	// the default here is 5 (auto/chaos panel size, parity tracker Area 1).
	MaxPanel int
	// Timeout is the per-call deadline applied to the ctx handed to
	// the caller. 0 means no extra deadline beyond the caller's ctx.
	// the reference router's panel hard timeout is 90 s (fusion.ts).
	Timeout time.Duration
	// MinPanel is the smallest panel that still runs a judge: when the
	// fan-out has fewer than MinPanel targets, the first successful
	// member is served directly (no judge). <= 0 means DefaultMinPanel
	// (2) — the documented fusion default (fusion.ts). A value
	// of 1 also serves a lone member directly, exactly like the
	// single-target rule.
	MinPanel int
	// StragglerGrace, when > 0, bounds how long the fan-out waits for
	// slow members AFTER the first successful result: once at least
	// MinPanel successes are in and the grace has elapsed since the
	// first result, the remaining members are abandoned (their ctx is
	// cancelled and they are recorded as errors). 0 means join ALL
	// members (this primitive's default — see the file header).
	StragglerGrace time.Duration
}

// DefaultPanelMax is the default fan-out cap (see PanelConfig).
const DefaultPanelMax = 5

// DefaultFusionMaxPanel is the fusion strategy's panel cap (fusion.ts
// FUSION_DEFAULTS maxPanel, the A2-3 configured default).
const DefaultFusionMaxPanel = 40

// DefaultMinPanel is the default smallest judged panel (fusion.ts).
const DefaultMinPanel = 2

// DefaultPanelTimeout is the default per-call deadline for a fan-out
// member, matching the reference router's 90 s panel hard timeout (fusion.ts).
const DefaultPanelTimeout = 90 * time.Second

func (c PanelConfig) maxPanel() int {
	if c.MaxPanel <= 0 {
		return DefaultPanelMax
	}
	return c.MaxPanel
}

func (c PanelConfig) minPanel() int {
	if c.MinPanel <= 0 {
		return DefaultMinPanel
	}
	return c.MinPanel
}

func (c PanelConfig) stragglerGrace() time.Duration {
	if c.StragglerGrace <= 0 {
		return 0
	}
	return c.StragglerGrace
}

func (c PanelConfig) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultPanelTimeout
	}
	return c.Timeout
}

// fanOut runs call for every target in parallel and joins every
// goroutine before returning (sync.WaitGroup; no goroutine is left
// behind on any path). Results are index-aligned with targets. A
// caller that returns ("", nil) is recorded as an empty-response
// failure. ctx may already carry a deadline; cfg.Timeout, when set,
// tightens it for the fan-out only.
func fanOut(ctx context.Context, targets []Attempt, call PanelCaller, cfg PanelConfig) []PanelResult {
	timeout := cfg.timeout()
	callCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	results := make([]PanelResult, len(targets))
	var wg sync.WaitGroup
	for i, att := range targets {
		wg.Add(1)
		go func(i int, att Attempt) {
			defer wg.Done()
			text, err := call(callCtx, att)
			if err == nil && strings.TrimSpace(text) == "" {
				err = errors.New("empty response")
			}
			results[i] = PanelResult{Attempt: att, Text: text, Err: err}
		}(i, att)
	}
	wg.Wait()
	return results
}

// fanOutFailures renders every failed result as "label: err" lines
// for the all-failed error; successes are omitted.
func fanOutFailures(results []PanelResult) string {
	var parts []string
	for _, r := range results {
		if r.Err != nil {
			parts = append(parts, r.Label()+": "+r.Err.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// errStragglerAbandoned marks a panel member the straggler-grace window
// left behind: it was still running when the quorum + grace elapsed, so
// its ctx was cancelled and it never reached the judge. Recorded as an
// error (never a faked success).
var errStragglerAbandoned = errors.New("straggler grace elapsed before this panel member answered")

// fanOutWithGrace is fanOut with a straggler-grace window: it returns as
// soon as at least minSuccess results have arrived AND the grace window
// has elapsed since the FIRST result, cancelling the members still
// running (recorded with errStragglerAbandoned). It never returns before
// the first result (or the panel hard timeout / ctx). A grace <= 0 means
// join all, exactly like fanOut. Results are index-aligned with targets.
func fanOutWithGrace(ctx context.Context, targets []Attempt, call PanelCaller, cfg PanelConfig, minSuccess int) []PanelResult {
	timeout := cfg.timeout()
	callCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		callCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	results := make([]PanelResult, len(targets))
	got := make([]bool, len(targets))
	var mu sync.Mutex
	// abandoned is set (under mu) BEFORE cancel() so a member waking up from
	// the cancelled call records errStragglerAbandoned instead of the raw
	// context error — the abandon decision is made before cancellation, so
	// the goroutine's view is deterministic (no race between cancel() and
	// the parent's abandon-marking loop).
	abandoned := false
	done := make(chan int, len(targets))
	var wg sync.WaitGroup
	for i, att := range targets {
		wg.Add(1)
		go func(i int, att Attempt) {
			defer wg.Done()
			text, err := call(callCtx, att)
			if err == nil && strings.TrimSpace(text) == "" {
				err = errors.New("empty response")
			}
			if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				mu.Lock()
				if abandoned && !got[i] {
					err = errStragglerAbandoned
				}
				mu.Unlock()
			}
			mu.Lock()
			results[i] = PanelResult{Attempt: att, Text: text, Err: err}
			got[i] = true
			mu.Unlock()
			done <- i
		}(i, att)
	}
	grace := cfg.stragglerGrace()
	if grace <= 0 {
		// Join all: drain every member.
		for received := 0; received < len(targets); received++ {
			<-done
		}
		mu.Lock()
		defer mu.Unlock()
		return results
	}
	received, successes := 0, 0
	var graceC <-chan time.Time
	var timer *time.Timer
collect:
	for received < len(targets) {
		select {
		case i := <-done:
			received++
			mu.Lock()
			ok := results[i].Err == nil
			mu.Unlock()
			if ok {
				successes++
			}
			if graceC == nil && successes >= 1 {
				timer = time.NewTimer(grace)
				graceC = timer.C
			}
			if successes >= minSuccess {
				select {
				case <-graceC:
					break collect
				case <-callCtx.Done():
					break collect
				case j := <-done:
					received++
					mu.Lock()
					ok := results[j].Err == nil
					mu.Unlock()
					if ok {
						successes++
					}
				}
			}
		case <-callCtx.Done():
			break collect
		}
	}
	if timer != nil {
		timer.Stop()
	}
	if ctx.Err() == nil {
		// The parent context is still alive, so this return is the grace or
		// hard-timeout path: the unfinished members are abandoned by choice.
		// (On a caller cancellation the raw context error is the honest one.)
		mu.Lock()
		abandoned = true
		mu.Unlock()
	}
	cancel()
	// Wait for every writer to exit before reading results: returning while
	// a goroutine may still write results[i] would be a data race on the
	// returned slice. cancel() bounds the wait — callers honour callCtx.
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for i := range results {
		if !got[i] {
			results[i] = PanelResult{Attempt: targets[i], Err: errStragglerAbandoned}
		}
	}
	return results
}

// ---------------------------------------------------------------------------
// Fusion
// ---------------------------------------------------------------------------

// Judge picks the winning candidate index. prompt is the original user
// prompt; candidates are the SUCCESSFUL panel results only (failures
// never reach the judge). The returned index addresses candidates,
// not the original target list. A Judge must be deterministic for a
// fixed input, and must never fabricate a winner: return an error
// rather than an index when no candidate can be judged.
type Judge interface {
	Score(ctx context.Context, prompt string, candidates []PanelResult) (winner int, err error)
}

// JudgeFunc adapts a plain function to the Judge interface.
type JudgeFunc func(ctx context.Context, prompt string, candidates []PanelResult) (int, error)

// Score calls the adapted function.
func (f JudgeFunc) Score(ctx context.Context, prompt string, candidates []PanelResult) (int, error) {
	return f(ctx, prompt, candidates)
}

// HeuristicJudge is the deterministic built-in judge used when no
// Judge is supplied. It is a HEURISTIC, not an LLM judge, and its
// picks must never be presented as judged quality:
//
//   - empty/whitespace answers are disqualified;
//   - among the rest, the winner is the longest answer up to a sane
//     cap, with a heavy penalty per refusal signal — so a refusal
//     never beats a real answer, however long the refusal is;
//   - ties break toward the earliest panel member (stable, fixed).
//
// "Longest sane" means length stops counting past
// heuristicSaneCap: beyond that, extra verbosity buys nothing.
// Refusal is detected by fixed text signals (see
// heuristicRefusalSignals); this is surface text matching, not the
// provider error taxonomy — a model can refuse in words the list
// does not know, and a non-refusal can contain one of the phrases.
type HeuristicJudge struct{}

// heuristicSaneCap is the length (in bytes) past which a longer
// answer scores no better. It bounds the "longest wins" rule so a
// runaway verbose answer cannot dominate by sheer size.
const heuristicSaneCap = 20000

// heuristicRefusalPenalty is subtracted per distinct refusal signal
// found. It exceeds heuristicSaneCap, so any refusal loses to any
// non-refusal regardless of length.
const heuristicRefusalPenalty = 100000

// heuristicRefusalSignals are lowercase substrings that mark an
// answer as (probably) a refusal. Matched case-insensitively
// against the answer text; each distinct signal counts once.
var heuristicRefusalSignals = []string{
	"i'm sorry",
	"i am sorry",
	"i cannot",
	"i can't",
	"i'm unable",
	"i am unable",
	"i don't have the ability",
	"as an ai",
	"content_filter",
	"i am not able to",
	"i'm not able to",
}

// heuristicScore scores one non-empty answer: capped length minus
// refusal penalties. Higher wins.
func heuristicScore(text string) int {
	lower := strings.ToLower(text)
	score := len(text)
	if score > heuristicSaneCap {
		score = heuristicSaneCap
	}
	for _, sig := range heuristicRefusalSignals {
		if strings.Contains(lower, sig) {
			score -= heuristicRefusalPenalty
		}
	}
	return score
}

// Score implements Judge for HeuristicJudge.
func (HeuristicJudge) Score(ctx context.Context, prompt string, candidates []PanelResult) (int, error) {
	if len(candidates) == 0 {
		return 0, errors.New("heuristic judge: no candidates to score")
	}
	best := -1
	bestScore := 0
	for i, c := range candidates {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if strings.TrimSpace(c.Text) == "" {
			continue // disqualified: empty answers never win
		}
		if s := heuristicScore(c.Text); best < 0 || s > bestScore {
			best, bestScore = i, s
		}
	}
	if best < 0 {
		return 0, errors.New("heuristic judge: all candidates are empty")
	}
	return best, nil
}

// FusionConfig tunes RunFusion. Zero values take the defaults
// (PanelConfig defaults; Judge nil means the built-in heuristic).
type FusionConfig struct {
	PanelConfig
	// Judge selects the winner. Nil uses HeuristicJudge — a
	// deterministic heuristic, NOT an LLM judge. Supply a Judge
	// backed by a real model call for judged quality.
	Judge Judge
}

// FusionResult is the outcome of RunFusion.
type FusionResult struct {
	// Winner is the judged winning panel result.
	Winner PanelResult
	// WinnerIndex is the winner's index into the original targets.
	WinnerIndex int
	// Candidates are all panel outcomes, index-aligned with targets
	// (failures carry Err, successes carry Text).
	Candidates []PanelResult
	// JudgeName is "heuristic" when the built-in heuristic judge
	// picked the winner, "custom" when a caller-supplied Judge did.
	JudgeName string
}

// RunFusion fans one prompt out to targets in parallel (bounded by
// cfg.MaxPanel, default 5) and picks a winner with cfg.Judge (or the
// built-in heuristic when nil).
//
//   - no targets → error; more targets than the cap → error naming
//     the count and the cap, rejected BEFORE any fan-out (cf.
//     fusion.ts issue #1905: oversized panels are rejected up front);
//   - a single target answers directly with no judge (fusion.ts:
//     "a single-model fusion has nothing to fuse");
//   - when every target fails (or returns empty), the error names
//     each failure as "provider/model: err";
//   - a judge error propagates; the winner is never silently
//     substituted by another judge.
//
// The judge sees only successful results. Response text is never
// fabricated: the winner's Text is a panel member's verbatim reply.
func RunFusion(ctx context.Context, prompt string, targets []Attempt, call PanelCaller, cfg FusionConfig) (FusionResult, error) {
	if call == nil {
		return FusionResult{}, errors.New("fusion: nil caller")
	}
	if len(targets) == 0 {
		return FusionResult{}, errors.New("fusion: no panel targets")
	}
	maxPanel := cfg.maxPanel()
	if len(targets) > maxPanel {
		return FusionResult{}, fmt.Errorf("fusion: panel too large (%d targets, max %d)", len(targets), maxPanel)
	}
	if err := ctx.Err(); err != nil {
		return FusionResult{}, err
	}

	var results []PanelResult
	if cfg.stragglerGrace() > 0 {
		results = fanOutWithGrace(ctx, targets, call, cfg.PanelConfig, cfg.minPanel())
	} else {
		results = fanOut(ctx, targets, call, cfg.PanelConfig)
	}

	// A panel smaller than minPanel (including a single target) has
	// nothing to judge: serve the FIRST successful member directly
	// (fusion.ts: "a single-model fusion has nothing to fuse"). A lone
	// SURVIVOR after a larger fan-out is also served directly — the
	// caller (the engine) checks that before synthesising.
	if len(targets) < cfg.minPanel() {
		first := -1
		for i, r := range results {
			if r.Err == nil {
				first = i
				break
			}
		}
		if first < 0 {
			return FusionResult{}, fmt.Errorf("fusion: all %d panel targets failed: %s", len(targets), fanOutFailures(results))
		}
		return FusionResult{Winner: results[first], WinnerIndex: first, Candidates: results, JudgeName: "direct"}, nil
	}

	var successes []PanelResult
	var successIdx []int
	for i, r := range results {
		if r.Err == nil {
			successes = append(successes, r)
			successIdx = append(successIdx, i)
		}
	}
	if len(successes) == 0 {
		return FusionResult{}, fmt.Errorf("fusion: all %d panel targets failed: %s", len(targets), fanOutFailures(results))
	}
	// A lone survivor has nothing to judge: serve it directly, no
	// judge call (fusion.ts single-model rule, at panel scale).
	if len(successes) == 1 {
		return FusionResult{Winner: successes[0], WinnerIndex: successIdx[0], Candidates: results, JudgeName: "direct"}, nil
	}

	judge := cfg.Judge
	judgeName := "custom"
	if judge == nil {
		judge = HeuristicJudge{}
		judgeName = "heuristic"
	}
	winner, err := judge.Score(ctx, prompt, successes)
	if err != nil {
		return FusionResult{}, fmt.Errorf("fusion: judge failed: %w", err)
	}
	if winner < 0 || winner >= len(successes) {
		return FusionResult{}, fmt.Errorf("fusion: judge returned out-of-range winner %d for %d candidates", winner, len(successes))
	}
	return FusionResult{
		Winner:      successes[winner],
		WinnerIndex: successIdx[winner],
		Candidates:  results,
		JudgeName:   judgeName,
	}, nil
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// PipelineCaller executes one pipeline stage: att is the stage's
// target, input is the text the stage must answer — the original
// prompt for the first stage, and the original prompt plus the
// previous stage's output for later stages (see RunPipeline).
// It MUST respect ctx.
type PipelineCaller func(ctx context.Context, att Attempt, input string) (string, error)

// PipelineStage is one step of a multi-stage route.
type PipelineStage struct {
	// Name identifies the stage in errors and results ("plan",
	// "execute", "reflect", "fix" in the reference templates).
	Name string
	// Target is the attempt that executes this stage.
	Target Attempt
	// Optional stages may fail without aborting the pipeline: the
	// failure is recorded as Skipped and the next stage receives
	// the last good output instead. A non-optional failure aborts.
	Optional bool
}

// PipelineStageResult is one executed stage's outcome, in execution
// order. A skipped (optional, failed) stage carries Skipped=true
// and its Err; the aborting stage of a failed pipeline carries
// Err with Skipped=false.
type PipelineStageResult struct {
	Stage   PipelineStage
	Output  string
	Skipped bool
	Err     error
}

// PipelineResult is the outcome of RunPipeline. Text is the last
// non-skipped stage's output ("" when no stage produced output).
// On a non-optional stage failure RunPipeline returns the partial
// result (stages executed so far, Text = last good output) ALONG
// WITH the error, so the caller can see how far it got.
type PipelineResult struct {
	Stages []PipelineStageResult
	Text   string
}

// pipelineStageInput builds the input for stage i: the original
// prompt, plus — for every stage after the first — the previous
// stage's output, so the stage works from both (cf. domain/
// pipeline.ts, which renders each stage from original_request plus
// the threaded context: plan_context / execution_response /
// reflection_response).
func pipelineStageInput(prompt, prevStageName, prevOutput string, first bool) string {
	if first {
		return prompt
	}
	return prompt + "\n\nPrevious stage " + fmt.Sprintf("%q", prevStageName) + " output:\n" + prevOutput
}

// RunPipeline executes stages in order through call. Stage N+1
// receives the original prompt plus stage N's output as (part of)
// its input. A stage that errors — or returns empty — fails: if the
// stage is Optional it is recorded as skipped and the pipeline
// continues with the last good output; otherwise the pipeline
// aborts immediately with an error naming the stage and the target
// that failed, and later stages never run. ctx cancellation aborts
// between stages and is propagated as the caller's error.
func RunPipeline(ctx context.Context, prompt string, stages []PipelineStage, call PipelineCaller) (PipelineResult, error) {
	if call == nil {
		return PipelineResult{}, errors.New("pipeline: nil caller")
	}
	if len(stages) == 0 {
		return PipelineResult{}, errors.New("pipeline: no stages")
	}
	res := PipelineResult{}
	var lastOutput string
	for i, stage := range stages {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("pipeline: stage %q aborted: %w", stage.Name, err)
		}
		input := pipelineStageInput(prompt, res.lastStageName(), lastOutput, i == 0)
		output, err := call(ctx, stage.Target, input)
		if err == nil && strings.TrimSpace(output) == "" {
			err = errors.New("empty response")
		}
		if err != nil {
			res.Stages = append(res.Stages, PipelineStageResult{Stage: stage, Err: err, Skipped: stage.Optional})
			if stage.Optional {
				continue // keep lastOutput: the next stage sees the last good output
			}
			return res, fmt.Errorf("pipeline: stage %q (%s) failed: %w", stage.Name, attemptLabel(stage.Target), err)
		}
		res.Stages = append(res.Stages, PipelineStageResult{Stage: stage, Output: output})
		lastOutput = output
		res.Text = output
	}
	return res, nil
}

func (r PipelineResult) lastStageName() string {
	if len(r.Stages) == 0 {
		return ""
	}
	return r.Stages[len(r.Stages)-1].Stage.Name
}

// ---------------------------------------------------------------------------
// JEV — judge with admitted / held sets
// ---------------------------------------------------------------------------

// JEV floors and cap, verbatim from jevStrategy.ts:11-13
// (JEV_PROBABILITY_FLOOR, JEV_CONFIDENCE_FLOOR, JEV_MAX_ADMITTED).
const (
	JevProbabilityFloor = 0.15
	JevConfidenceFloor  = 0.65
	JevMaxAdmitted      = 4
)

// JevHoldReason explains why a candidate was held (or why the
// judge fell open, in JevResult.FallbackReason).
type JevHoldReason string

const (
	// JevHoldBelowFloor: the candidate's probability is below
	// JevProbabilityFloor.
	JevHoldBelowFloor JevHoldReason = "below_probability_floor"
	// JevHoldDominated: the candidate cleared the floor but the
	// admitted set was already full (JevMaxAdmitted).
	JevHoldDominated JevHoldReason = "dominated"
	// JevHoldLowConfidence: the choice answer's confidence is below
	// JevConfidenceFloor, so the judge fell open.
	JevHoldLowConfidence JevHoldReason = "low_confidence"
	// JevHoldUnavailable: no choice answer was available (or the
	// choice named an id that was never offered), so the judge fell
	// open.
	JevHoldUnavailable JevHoldReason = "jev_unavailable"
)

// JevCandidate is one entry of the user's ordered preference list.
// ID is an opaque, request-local key (it must not expose model or
// account identifiers to the judge); UserRank is its 0-based rank
// in the stored list.
type JevCandidate struct {
	ID       string
	UserRank int
	Label    string // optional human-readable label; not used by the judge
}

// JevChoiceAnswer is the caller-supplied System One choice: the
// winning candidate id, per-candidate probabilities, and overall
// confidence. The panel code NEVER invents these numbers — they
// come from the caller (a JevScorer, ultimately a model call).
// A nil *JevChoiceAnswer means the System One call was unavailable.
type JevChoiceAnswer struct {
	Choice        string
	Probabilities map[string]float64
	Confidence    float64
}

// JevVerdictRow is one candidate's verdict.
type JevVerdictRow struct {
	ID       string
	UserRank int
	// Verdict is "admitted" or "held".
	Verdict string
	// Reason is set for held rows (below_probability_floor /
	// dominated) and for fall-open admitted rows (low_confidence /
	// jev_unavailable). Empty for normally admitted rows.
	Reason JevHoldReason
	// Probability is the candidate's probability from the choice
	// answer; HasProbability is false when the judge fell open
	// (probability unknown / null in the reference).
	Probability    float64
	HasProbability bool
}

// JevResult is the outcome of JudgeJev.
type JevResult struct {
	// Admitted: winner first, then other admitted candidates in
	// userRank order. At most JevMaxAdmitted rows.
	Admitted []JevVerdictRow
	// Held: candidates that must not be attempted, in userRank order.
	Held []JevVerdictRow
	// FellOpen is true when the judge could not trust its answer
	// and admitted every candidate in user order instead.
	FellOpen bool
	// FallbackReason is JevHoldLowConfidence or JevHoldUnavailable
	// when FellOpen, "" otherwise.
	FallbackReason JevHoldReason
}

// jevProbability returns the candidate's probability from the
// answer, or 0 when absent / non-finite (jevStrategy.ts
// probabilityFor).
func jevProbability(answer *JevChoiceAnswer, id string) float64 {
	if answer == nil {
		return 0
	}
	p, ok := answer.Probabilities[id]
	if !ok || p != p || p > 1e308 || p < -1e308 { // NaN / ±Inf → 0
		return 0
	}
	return p
}

// jevFallOpen admits every candidate in user order, tagging each
// row with the fallback reason and no probability
// (jevStrategy.ts fallOpen).
func jevFallOpen(candidates []JevCandidate, reason JevHoldReason) JevResult {
	sorted := append([]JevCandidate(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].UserRank < sorted[j].UserRank })
	admitted := make([]JevVerdictRow, 0, len(sorted))
	for _, c := range sorted {
		admitted = append(admitted, JevVerdictRow{
			ID: c.ID, UserRank: c.UserRank, Verdict: "admitted", Reason: reason,
		})
	}
	return JevResult{Admitted: admitted, Held: []JevVerdictRow{}, FellOpen: true, FallbackReason: reason}
}

// JudgeJev is the pure JEV judge (jevStrategy.ts judgeJev, ported
// as bookkeeping only — no scoring happens here). Blocked targets
// must be filtered out by the caller before invoking this; they
// never appear in the result.
//
//   - no candidates → empty verdict, no fall-open;
//   - one candidate → admitted as-is (the System One call is skipped);
//   - nil answer → fall open, jev_unavailable;
//   - confidence below JevConfidenceFloor → fall open, low_confidence;
//   - choice naming an id outside the candidate set → fall open,
//     jev_unavailable;
//   - otherwise the winner is admitted first; each other candidate
//     in userRank order is admitted when its probability is >=
//     JevProbabilityFloor and the admitted set is not yet full
//     (JevMaxAdmitted), else held as below_probability_floor or
//     dominated respectively.
func JudgeJev(candidates []JevCandidate, answer *JevChoiceAnswer) JevResult {
	if len(candidates) == 0 {
		return JevResult{Admitted: []JevVerdictRow{}, Held: []JevVerdictRow{}}
	}
	if len(candidates) == 1 {
		only := candidates[0]
		row := JevVerdictRow{ID: only.ID, UserRank: only.UserRank, Verdict: "admitted"}
		if answer != nil {
			row.Probability = jevProbability(answer, only.ID)
			row.HasProbability = true
		}
		return JevResult{Admitted: []JevVerdictRow{row}, Held: []JevVerdictRow{}}
	}
	if answer == nil {
		return jevFallOpen(candidates, JevHoldUnavailable)
	}
	if !(answer.Confidence >= JevConfidenceFloor) {
		return jevFallOpen(candidates, JevHoldLowConfidence)
	}
	winnerIdx := -1
	for i, c := range candidates {
		if c.ID == answer.Choice {
			winnerIdx = i
			break
		}
	}
	if winnerIdx < 0 {
		return jevFallOpen(candidates, JevHoldUnavailable)
	}
	winner := candidates[winnerIdx]

	sorted := append([]JevCandidate(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].UserRank < sorted[j].UserRank })

	admitted := []JevVerdictRow{{
		ID: winner.ID, UserRank: winner.UserRank, Verdict: "admitted",
		Probability: jevProbability(answer, winner.ID), HasProbability: true,
	}}
	held := []JevVerdictRow{}
	for _, c := range sorted {
		if c.ID == winner.ID {
			continue
		}
		p := jevProbability(answer, c.ID)
		if p < JevProbabilityFloor {
			held = append(held, JevVerdictRow{
				ID: c.ID, UserRank: c.UserRank, Verdict: "held",
				Reason: JevHoldBelowFloor, Probability: p, HasProbability: true,
			})
			continue
		}
		if len(admitted) >= JevMaxAdmitted {
			held = append(held, JevVerdictRow{
				ID: c.ID, UserRank: c.UserRank, Verdict: "held",
				Reason: JevHoldDominated, Probability: p, HasProbability: true,
			})
			continue
		}
		admitted = append(admitted, JevVerdictRow{
			ID: c.ID, UserRank: c.UserRank, Verdict: "admitted",
			Probability: p, HasProbability: true,
		})
	}
	return JevResult{Admitted: admitted, Held: held}
}

// JevScorer produces the System One choice answer for a set of
// executed candidates. It is caller-supplied — the panel code does
// not score anything itself. Returning (nil, nil) signals that the
// scoring call was unavailable, which falls open per JudgeJev;
// returning a non-nil error propagates as a hard failure.
type JevScorer interface {
	ScoreJev(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error)
}

// JevScorerFunc adapts a plain function to the JevScorer interface.
type JevScorerFunc func(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error)

// ScoreJev calls the adapted function.
func (f JevScorerFunc) ScoreJev(ctx context.Context, prompt string, candidates []PanelResult) (*JevChoiceAnswer, error) {
	return f(ctx, prompt, candidates)
}

// JevPanelResult is the outcome of RunJevPanel.
type JevPanelResult struct {
	// Results are all executed candidates, index-aligned with
	// targets (failures carry Err). Only successful results were
	// offered to the scorer.
	Results []PanelResult
	// Verdict is the admit/hold bookkeeping over the successful
	// candidates. Verdict row IDs are PanelResult labels
	// ("provider/model"); map them back via Results.
	Verdict JevResult
}

// RunJevPanel executes targets in parallel (bounded like fusion),
// scores the successful results with the caller-supplied scorer,
// and returns the JEV admit/hold verdict over them. Failed targets
// never reach the scorer and never appear in the verdict. A nil
// scorer answer falls open (jev_unavailable); a scorer error
// propagates. When every target fails, the error names each
// failure. Scores are never invented here.
func RunJevPanel(ctx context.Context, prompt string, targets []Attempt, call PanelCaller, scorer JevScorer, cfg PanelConfig) (JevPanelResult, error) {
	if call == nil {
		return JevPanelResult{}, errors.New("jev: nil caller")
	}
	if scorer == nil {
		return JevPanelResult{}, errors.New("jev: nil scorer")
	}
	if len(targets) == 0 {
		return JevPanelResult{}, errors.New("jev: no candidates")
	}
	if len(targets) > cfg.maxPanel() {
		return JevPanelResult{}, fmt.Errorf("jev: candidate set too large (%d targets, max %d)", len(targets), cfg.maxPanel())
	}
	if err := ctx.Err(); err != nil {
		return JevPanelResult{}, err
	}

	results := fanOut(ctx, targets, call, cfg)
	var successes []PanelResult
	for _, r := range results {
		if r.Err == nil {
			successes = append(successes, r)
		}
	}
	if len(successes) == 0 {
		return JevPanelResult{}, fmt.Errorf("jev: all %d candidates failed: %s", len(targets), fanOutFailures(results))
	}

	answer, err := scorer.ScoreJev(ctx, prompt, successes)
	if err != nil {
		return JevPanelResult{}, fmt.Errorf("jev: scorer failed: %w", err)
	}
	candidates := make([]JevCandidate, len(successes))
	for i, r := range successes {
		candidates[i] = JevCandidate{ID: r.Label(), UserRank: i, Label: r.Label()}
	}
	return JevPanelResult{Results: results, Verdict: JudgeJev(candidates, answer)}, nil
}
