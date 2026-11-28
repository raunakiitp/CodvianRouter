// A2-7: pipeline knobs surfaced + honoured.
//
// task_detection modes, skip_pipeline_for_tokens_under, and
// pipeline_fallback "single-provider" are surfaced on router.Config and
// honoured by the engine's "pipeline" strategy path. Each genuinely
// implemented knob has a test here; max_reflection_loops is documented
// as NOT-APPLICABLE (the pipeline primitive has no reflection stage) in
// the PipelineMaxReflectionLoops comment and the PR body.
//
// FAILS-WHEN-OFF: each test fails when its knob is turned off (the
// pipeline then runs both stages, or fails), so removing the knob
// wiring is caught.
package router

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// pipelineEngine builds an engine whose chain for any model is
// [a, b], so a "pipeline" request runs stage-1 on a and stage-2 on b.
// cfgMut lets a test set the pipeline knobs before the engine is built.
func pipelineEngine(t *testing.T, a, b provider.Client, cfgMut func(*Config)) *Engine {
	t.Helper()
	cfg := testConfig()
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}
	}
	return NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
}

// TestPipelineSkipTokensUnderServesSingle: with PipelineSkipTokensUnder
// above the short prompt's estimate, the multi-stage pipeline is skipped
// and a single provider (a, the first chain member) serves — b is never
// dialled. FAILS-WHEN-OFF: with the threshold 0, both stages run and the
// served output is b's (the last stage), not a's.
func TestPipelineSkipTokensUnderServesSingle(t *testing.T) {
	a := okClient("a-answer")
	b := okClient("b-answer")
	eng := pipelineEngine(t, a, b, func(c *Config) { c.PipelineSkipTokensUnder = 1000 })
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "pipeline")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(pipeline, skip_under) = %v, want nil", err)
	}
	if resp.Content != "a-answer" {
		t.Fatalf("served content = %q, want the single provider %q (short prompt skipped the pipeline, not the last stage %q)", resp.Content, "a-answer", "b-answer")
	}
	if b.chatCalls() != 0 {
		t.Fatalf("provider b calls = %d, want 0 (the pipeline was skipped, b never dialled)", b.chatCalls())
	}
}

// TestPipelineTaskDetectionAutoSkipsSimple: with task_detection "auto",
// a simple (non-code) prompt is served by a single provider — b never
// dialled. FAILS-WHEN-OFF: with detection "off" (default), both stages
// run and the served output is b's.
func TestPipelineTaskDetectionAutoSkipsSimple(t *testing.T) {
	a := okClient("a-simple")
	b := okClient("b-simple")
	eng := pipelineEngine(t, a, b, func(c *Config) { c.PipelineTaskDetection = "auto" })
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "pipeline")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(pipeline, auto simple) = %v, want nil", err)
	}
	if resp.Content != "a-simple" {
		t.Fatalf("served content = %q, want the single provider %q (simple task skipped the pipeline)", resp.Content, "a-simple")
	}
	if b.chatCalls() != 0 {
		t.Fatalf("provider b calls = %d, want 0 (simple task: b never dialled)", b.chatCalls())
	}
}

// TestPipelineTaskDetectionAutoKeepsComplex: with task_detection "auto",
// a complex (code) prompt DOES run the multi-stage pipeline — the served
// output is the last stage's (b). FAILS-WHEN-OFF: if detection also
// skipped complex tasks, b would never be dialled.
func TestPipelineTaskDetectionAutoKeepsComplex(t *testing.T) {
	a := okClient("a-stage")
	b := okClient("b-final")
	eng := pipelineEngine(t, a, b, func(c *Config) { c.PipelineTaskDetection = "auto" })
	req := comboReq()
	req.Messages = []provider.Message{{Role: RoleUser, Content: "implement a function that sorts a list in place, then refactor it"}}
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), req, "pipeline")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(pipeline, auto complex) = %v, want nil", err)
	}
	if resp.Content != "b-final" {
		t.Fatalf("served content = %q, want the last pipeline stage %q (a complex task runs the full pipeline)", resp.Content, "b-final")
	}
	if b.chatCalls() != 1 {
		t.Fatalf("provider b calls = %d, want 1 (the complex task ran stage-2 on b)", b.chatCalls())
	}
}

// TestPipelineFallbackSingleProvider: with pipeline_fallback
// "single-provider", a pipeline that fails (stage-2 on b errors) falls
// back to serving the single provider (a) — the request succeeds with
// a's output. FAILS-WHEN-OFF: with pipeline_fallback "" the pipeline
// error propagates and the request fails.
func TestPipelineFallbackSingleProvider(t *testing.T) {
	a := okClient("a-fallback")
	b := errClient(errors.New("b is down"))
	eng := pipelineEngine(t, a, b, func(c *Config) { c.PipelineFallback = "single-provider" })
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "pipeline")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(pipeline, single-provider fallback) = %v, want nil", err)
	}
	if resp.Content != "a-fallback" {
		t.Fatalf("served content = %q, want the single-provider fallback %q", resp.Content, "a-fallback")
	}
	if a.chatCalls() < 1 {
		t.Fatalf("provider a calls = %d, want >= 1 (a served the fallback)", a.chatCalls())
	}
}
