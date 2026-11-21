// Permanent regression cases for the eval harness (P2-T12).
//
// These are the substring-and-latency smoke cases the harness is built
// to run — see the package doc for what a "pass" does and does NOT
// mean. They are permanent because they pin the harness's own wiring:
// each category the Phase 2 directive names (protocol compat, nested
// model IDs, reasoning-only, usage accounting, empty prompt, failover,
// strategy routing, observability) has at least one case here, driven
// by a scripted fake engine in the accompanying test, so a change to
// the harness's Engine contract or Run loop breaks a named regression
// case instead of silently passing.
//
// The deeper behavioral guarantees behind each category (e.g. that a
// real router really does isolate caches, or that usage is really
// recorded into the ledger) live in the package integration suites:
// internal/router, internal/cache, internal/ledger, internal/strategy,
// internal/server. This list deliberately does not duplicate them; it
// exists so the eval harness is exercised end-to-end on every build.
package eval

import (
	"context"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// PermanentRegressionCases returns the permanent case set. Every case
// has a stable, unique ID so a regression is traceable to a name, not
// just a failing substring. The set is deterministic and ordered.
func PermanentRegressionCases() []Case {
	return []Case{
		{
			ID:             "regression/protocol-compat",
			Prompt:         "Return a standard chat completion.",
			Model:          "gpt-4o",
			ExpectContains: []string{"role", "content"},
		},
		{
			ID:             "regression/nested-model-id",
			Prompt:         "Call the nested model 'org/team/gpt-5-mini'.",
			Model:          "org/team/gpt-5-mini",
			ExpectContains: []string{"org/team/gpt-5-mini"},
		},
		{
			ID:             "regression/reasoning-only",
			Prompt:         "Think step by step and answer.",
			Model:          "deepseek-reasoner",
			ExpectContains: []string{"reasoning"},
		},
		{
			ID:             "regression/usage-accounting",
			Prompt:         "Report token usage.",
			Model:          "gpt-4o",
			ExpectContains: []string{"prompt_tokens", "total_tokens"},
		},
		{
			ID:             "regression/empty-prompt",
			Prompt:         "",
			Model:          "gpt-4o",
			ExpectContains: nil, // any successful response passes
		},
		{
			ID:             "regression/failover",
			Prompt:         "Primary provider is down; fail over.",
			Model:          "gpt-4o",
			ExpectContains: []string{"fallback"},
		},
		{
			ID:             "regression/strategy-routing",
			Prompt:         "Route by strategy.",
			Model:          "gpt-4o",
			ExpectContains: []string{"strategy"},
		},
		{
			ID:             "regression/observability",
			Prompt:         "Include a request id.",
			Model:          "gpt-4o",
			ExpectContains: []string{"request_id"},
		},
	}
}

// scriptedEngine returns a fake Engine whose Chat returns, for each
// permanent case ID, a response containing the case's expected
// substrings (and the case ID itself), so the harness Run loop is
// exercised end-to-end without any network. It proves the cases are
// wired and reachable, not that a model would produce them.
type scriptedEngine struct{}

func (scriptedEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	id := "unknown"
	// The harness does not pass the Case ID to the engine, so the fake
	// keys off the prompt to return the right markers; the test asserts
	// each case separately via the harness Report.
	markers := map[string]string{
		"Return a standard chat completion.":           `{"role":"assistant","content":"ok","request_id":"r-1","prompt_tokens":9,"total_tokens":18}`,
		"Call the nested model 'org/team/gpt-5-mini'.": "using org/team/gpt-5-mini",
		"Think step by step and answer.":               "reasoning: considered options",
		"Report token usage.":                          "prompt_tokens=9 completion_tokens=9 total_tokens=18",
		"":                                             "empty prompt accepted",
		"Primary provider is down; fail over.":         "fallback: served by secondary",
		"Route by strategy.":                           "strategy: balanced",
		"Include a request id.":                        "request_id=r-1",
	}
	body := ""
	if len(req.Messages) > 0 {
		body = markers[req.Messages[0].Content]
	}
	if body == "" {
		body = "response for " + id
	}
	return &provider.ChatResponse{Content: body}, nil
}
