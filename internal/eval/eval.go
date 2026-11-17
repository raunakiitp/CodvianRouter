// Package eval is a small, honest evaluation harness for Codvian Router.
//
// It runs a list of Cases against an Engine and reports, per case,
// whether the response contained every expected substring within the
// latency budget. That is ALL it checks.
//
// Scoring honesty: a "pass" means only that every string in
// Case.ExpectContains appeared in the response (case-insensitively) and,
// when Case.MaxLatencyMs > 0, that the call finished within that budget.
// The harness does NOT judge answer quality, correctness, relevance,
// factual accuracy, tone, or completeness beyond those substrings. A
// response can pass while being wrong, misleading, or mostly nonsense,
// as long as it happens to contain the expected strings; conversely a
// correct answer phrased differently fails. Treat PassRate as a
// substring-and-latency smoke signal, never as a quality score.
//
// The harness never panics because an engine misbehaves: engine errors,
// nil responses, and engine panics are recorded as case failures
// (Result.Error), and per-case timeouts are recorded the same way.
package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// DefaultTimeout is the per-case timeout used when Harness.Timeout is 0.
const DefaultTimeout = 30 * time.Second

// Case is one evaluation case: a prompt sent to a model, plus the
// (substring-only) expectations the response must satisfy.
type Case struct {
	// ID identifies the case in reports. It should be unique within a run.
	ID string
	// Prompt is the user message sent to the engine. It may be empty;
	// see SmokeCases for how the empty-prompt edge case is treated.
	Prompt string
	// Model is the model name passed through to the engine.
	Model string
	// ExpectContains lists substrings that must ALL appear in the
	// response, compared case-insensitively. An empty list means "any
	// successful response passes" (subject to the latency check).
	ExpectContains []string
	// MaxLatencyMs is the latency budget in milliseconds. 0 means no
	// latency check is applied (the per-case timeout still applies).
	MaxLatencyMs int
}

// Result is the outcome of running one Case.
type Result struct {
	// CaseID is the ID of the case this result belongs to.
	CaseID string
	// OK is true only when the engine returned a response (no error, no
	// timeout) containing every expected substring within the latency
	// budget. See the package doc: OK is a substring-and-latency signal
	// only, not a judgement of answer quality.
	OK bool
	// LatencyMs is the wall-clock time of the engine call in milliseconds.
	LatencyMs int64
	// Missing lists the expected substrings that did not appear in the
	// response (case-insensitive comparison). Empty on a pass, and empty
	// when the case failed before a response was available (see Error).
	Missing []string
	// Response is the engine's raw response content, verbatim. Empty when
	// the engine errored, timed out, or returned no response.
	Response string
	// Error records why a case failed before or during scoring: an engine
	// error, a timeout, a nil response, or a latency-budget breach. Empty
	// on a pass; on a missing-substring failure it is empty and Missing
	// explains the failure instead.
	Error string
}

// Report summarises one Harness.Run call over a list of cases.
type Report struct {
	// Total is the number of cases run.
	Total int
	// Passed is the number of cases with Result.OK == true.
	Passed int
	// Failed is Total - Passed.
	Failed int
	// Results holds one Result per case, in the same order as the input.
	Results []Result
	// StartedAt is when Run began; FinishedAt is when it returned.
	StartedAt  time.Time
	FinishedAt time.Time
	// PassRate is Passed / Total as a fraction in [0, 1]. It is 0 when
	// Total is 0 (an empty run), not NaN. It measures substring-and-
	// latency passes only — see the package doc.
	PassRate float64
}

// Engine is the minimal interface the harness needs from a chat backend:
// one non-streaming chat completion. It is defined locally over provider
// types (not internal/router) so the harness can be driven by any engine
// — a real provider.Client, the router, or a fake in tests — without
// importing the router package.
type Engine interface {
	Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)
}

// Harness runs Cases against an Engine, one at a time, in order.
type Harness struct {
	// Engine is the backend under evaluation. A nil Engine makes every
	// case fail with Error "eval: harness has no engine"; Run never panics.
	Engine Engine
	// Timeout is the per-case timeout. 0 means DefaultTimeout (30s).
	// A negative value is treated the same as 0.
	Timeout time.Duration
}

// timeout returns the effective per-case timeout.
func (h *Harness) timeout() time.Duration {
	if h == nil || h.Timeout <= 0 {
		return DefaultTimeout
	}
	return h.Timeout
}

// engine returns the harness engine, or nil when there is no harness.
func (h *Harness) engine() Engine {
	if h == nil {
		return nil
	}
	return h.Engine
}

// Run runs cases sequentially against the harness engine and returns a
// Report. It never panics on engine errors: an engine error, a nil
// response, an engine panic, or a per-case timeout is recorded as a
// failed Result with Error set. Cases are run in input order and Results
// preserve that order. An empty case list yields a 0/0 report with
// PassRate 0 and an empty (non-nil) Results slice.
func (h *Harness) Run(ctx context.Context, cases []Case) Report {
	started := time.Now()
	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		results = append(results, h.runOne(ctx, c))
	}
	passed := 0
	for _, r := range results {
		if r.OK {
			passed++
		}
	}
	total := len(results)
	passRate := 0.0
	if total > 0 {
		passRate = float64(passed) / float64(total)
	}
	return Report{
		Total:      total,
		Passed:     passed,
		Failed:     total - passed,
		Results:    results,
		StartedAt:  started,
		FinishedAt: time.Now(),
		PassRate:   passRate,
	}
}

// runOne runs a single case with a per-case timeout and scores it.
func (h *Harness) runOne(ctx context.Context, c Case) (res Result) {
	res = Result{CaseID: c.ID, Missing: []string{}}
	// An engine panic must not take the harness (or the caller) down:
	// record it as a case failure, like any other engine misbehaviour.
	defer func() {
		if r := recover(); r != nil {
			res.OK = false
			res.Response = ""
			res.Missing = []string{}
			res.Error = fmt.Sprintf("eval: engine panicked: %v", r)
		}
	}()

	engine := h.engine()
	if engine == nil {
		res.Error = "eval: harness has no engine"
		return res
	}

	// Per-case timeout, derived from the caller's context so a cancelled
	// parent context also aborts the case promptly.
	caseCtx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()

	start := time.Now()
	resp, err := engine.Chat(caseCtx, provider.ChatRequest{
		Model:    c.Model,
		Messages: []provider.Message{{Role: "user", Content: c.Prompt}},
	})
	res.LatencyMs = time.Since(start).Milliseconds()

	if err != nil {
		// Distinguish a timeout from any other engine error: a timeout
		// means the engine did not answer in time, which is a different
		// failure mode from an immediate upstream error.
		if caseCtx.Err() == context.DeadlineExceeded {
			res.Error = fmt.Sprintf("eval: case timed out after %s: %v", h.timeout(), err)
		} else {
			res.Error = err.Error()
		}
		return res
	}
	if resp == nil {
		res.Error = "eval: engine returned a nil response with a nil error"
		return res
	}
	// An engine that ignores its context may return a successful response
	// after the deadline; that is still a timeout failure, not a pass.
	if caseCtx.Err() == context.DeadlineExceeded {
		res.Error = fmt.Sprintf("eval: case timed out after %s", h.timeout())
		return res
	}
	res.Response = resp.Content

	// Substring check only (case-insensitive). This is deliberately not
	// a quality judgement — see the package doc.
	lower := strings.ToLower(resp.Content)
	for _, want := range c.ExpectContains {
		if want == "" {
			continue
		}
		if !strings.Contains(lower, strings.ToLower(want)) {
			res.Missing = append(res.Missing, want)
		}
	}
	if len(res.Missing) > 0 {
		return res
	}

	// Latency budget, when the case sets one (0 = no check).
	if c.MaxLatencyMs > 0 && res.LatencyMs > int64(c.MaxLatencyMs) {
		res.Error = fmt.Sprintf("eval: latency %dms exceeded budget %dms", res.LatencyMs, c.MaxLatencyMs)
		return res
	}

	res.OK = true
	return res
}

// SmokeCases returns a small built-in case set for the given model: a
// greeting, a capital question, and an empty-prompt edge case. The cases
// are deterministic (fixed prompts and expected substrings); whether they
// pass depends on the engine under test.
//
// Empty-prompt behaviour, defined: an empty prompt is sent to the engine
// exactly like any other prompt. If the engine rejects it with an error
// (as most real upstreams do — provider.Client.Chat rejects a request
// with no messages, and an empty message is likewise invalid upstream),
// that is recorded as a case failure with Result.Error set, NOT a harness
// panic or crash. A smoke run against a strict engine therefore normally
// shows this case failing; that failure is the harness proving it handles
// a bad input cleanly, not a defect in the harness. The case has no
// ExpectContains entries, so if an engine does answer an empty prompt,
// any successful response passes it.
func SmokeCases(model string) []Case {
	return []Case{
		{
			ID:             "smoke-greeting",
			Prompt:         "Hello, say hello back.",
			Model:          model,
			ExpectContains: []string{"hello"},
			MaxLatencyMs:   30000,
		},
		{
			ID:             "smoke-capital",
			Prompt:         "What is the capital of France?",
			Model:          model,
			ExpectContains: []string{"Paris"},
			MaxLatencyMs:   30000,
		},
		{
			ID:           "smoke-empty-prompt",
			Prompt:       "",
			Model:        model,
			MaxLatencyMs: 5000,
		},
	}
}
