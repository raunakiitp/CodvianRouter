package compress

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M7 merge-wave tests: named modes wired into Apply (M7.4 RTK,
// M7.5 Caveman, M7.6 modes, M7.7 hard-budget post-pass, M7.8
// stacked pipeline). A mode selects lossy engines; the lossy
// opt-in (lossy_max_prompt_tokens > 0) authorises them; a bare
// lossy request stays extractive-only, and a mode without the
// opt-in runs the lossless steps only.

func modeReq(t *testing.T, mode string, budget int) *provider.CompressionRequest {
	t.Helper()
	return &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget, Mode: mode}
}

func hasStep(res *provider.CompressionResult, step string) bool {
	for _, s := range res.Steps {
		if s == step {
			return true
		}
	}
	return false
}

// TestApplyModeStandardRunsCaveman: mode=standard condenses prose
// (Caveman full) under the lossy opt-in; the budget is generous
// so the extractive pair is a no-op and the saving is Caveman's.
func TestApplyModeStandardRunsCaveman(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello, could you please explain in detail how the caching system works in this project? I really appreciate it, thank you so much for your help with this problem."},
	}
	out, res := Apply("gpt-4o", msgs, modeReq(t, "standard", 10000), nil, nil)
	if res.Mode != "standard" {
		t.Fatalf("res.Mode = %q, want standard", res.Mode)
	}
	if !hasStep(res, StepCaveman) {
		t.Fatalf("standard mode must record %q, steps=%v", StepCaveman, res.Steps)
	}
	if !res.Applied || !res.LossyApplied {
		t.Fatalf("standard mode must apply lossy, got %+v", res)
	}
	if strings.Contains(out[1].Content, "could you please") {
		t.Errorf("Caveman must remove the filler phrase, got %q", out[1].Content)
	}
	if res.SavedTokens <= 0 {
		t.Errorf("standard mode must save tokens, got %+v", res)
	}
}

// TestApplyModeRTKRunsFilter: mode=rtk strips routine tool-output
// lines and keeps failures. RTK keeps the FIRST pass line as a
// representative and drops the rest: the === RUN lines and the
// second pass line go, the failure (and its detail) stays.
func TestApplyModeRTKRunsFilter(t *testing.T) {
	msgs := stackedFixture()
	out, res := Apply("gpt-4o", msgs, modeReq(t, "rtk", 10000), nil, nil)
	if res.Mode != "rtk" {
		t.Fatalf("res.Mode = %q, want rtk", res.Mode)
	}
	if !hasStep(res, StepRTK) {
		t.Fatalf("rtk mode must record %q, steps=%v", StepRTK, res.Steps)
	}
	tool := findRole(out, "tool")
	if tool == nil {
		t.Fatalf("no tool message in output: %+v", out)
	}
	if strings.Contains(tool.Content, "--- PASS: TestBeta") || strings.Contains(tool.Content, "=== RUN") {
		t.Errorf("RTK must strip routine lines, got %q", tool.Content)
	}
	if !strings.Contains(tool.Content, "--- FAIL: TestGamma") || !strings.Contains(tool.Content, "gamma_test.go:42") {
		t.Errorf("RTK must keep failures and their detail, got %q", tool.Content)
	}
}

func findRole(msgs []provider.Message, role string) *provider.Message {
	for i := range msgs {
		if msgs[i].Role == role {
			return &msgs[i]
		}
	}
	return nil
}

// TestApplyModeStackedRunsPipeline: mode=stacked runs the RTK →
// Caveman core (via RunStacked) after the lossless steps.
func TestApplyModeStackedRunsPipeline(t *testing.T) {
	msgs := stackedFixture()
	out, res := Apply("gpt-4o", msgs, modeReq(t, "stacked", 10000), nil, nil)
	if res.Mode != "stacked" {
		t.Fatalf("res.Mode = %q, want stacked", res.Mode)
	}
	if !hasStep(res, StepRTK) || !hasStep(res, StepCaveman) {
		t.Fatalf("stacked mode must record both %q and %q, steps=%v", StepRTK, StepCaveman, res.Steps)
	}
	if len(out) != 3 {
		t.Fatalf("stacked must dedupe the system block: got %d messages", len(out))
	}
	tool := findRole(out, "tool")
	if tool == nil {
		t.Fatalf("no tool message in output: %+v", out)
	}
	if strings.Contains(tool.Content, "--- PASS: TestBeta") || strings.Contains(tool.Content, "=== RUN") {
		t.Errorf("stacked RTK stage must strip routine lines, got %q", tool.Content)
	}
	if !strings.Contains(tool.Content, "--- FAIL: TestGamma") {
		t.Errorf("stacked RTK stage must keep failures, got %q", tool.Content)
	}
	if strings.Contains(out[2].Content, "could you please") {
		t.Errorf("stacked Caveman stage must remove the filler phrase")
	}
}

// TestApplyModeAggressiveRunsAvailableSubset: aggressive's
// defining engines (message aging, summariser) do not exist, so
// the mode runs its available subset — the extractive pair — and
// never claims an RTK/Caveman step.
func TestApplyModeAggressiveRunsAvailableSubset(t *testing.T) {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 9)
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: prose},
	}
	out, res := Apply("gpt-4o", msgs, modeReq(t, "aggressive", 100), nil, nil)
	if res.Mode != "aggressive" {
		t.Fatalf("res.Mode = %q, want aggressive", res.Mode)
	}
	if hasStep(res, StepRTK) || hasStep(res, StepCaveman) {
		t.Fatalf("aggressive must not claim engine steps it cannot run, steps=%v", res.Steps)
	}
	if !res.Applied || res.AfterTokens > 100 {
		t.Fatalf("aggressive subset must fit the budget, got %+v", res)
	}
	_ = out
}

// TestApplyBareLossyRunsNoModeEngines: a lossy request with no
// mode runs exactly the extractive pair (+ hard-budget post-pass),
// never RTK/Caveman — the documented lossy contract is
// extractive trimming, and prose rewriting needs a named mode.
func TestApplyBareLossyRunsNoModeEngines(t *testing.T) {
	msgs := stackedFixture()
	_, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: 100}, nil, nil)
	if res.Mode != "" {
		t.Fatalf("bare lossy must report no mode, got %q", res.Mode)
	}
	if hasStep(res, StepRTK) || hasStep(res, StepCaveman) {
		t.Fatalf("bare lossy must not run mode engines, steps=%v", res.Steps)
	}
}

// TestApplyModeWithoutLossyOptInRunsLosslessOnly: a named mode
// without lossy_max_prompt_tokens selects nothing — a mode can
// never switch a request to lossy by itself.
func TestApplyModeWithoutLossyOptInRunsLosslessOnly(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello, could you please explain in detail how the caching system works in this project? I really appreciate it, thank you so much for your help with this problem."},
	}
	_, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true), Mode: "standard"}, nil, nil)
	if hasStep(res, StepCaveman) {
		t.Fatalf("mode without the lossy opt-in must not run Caveman, steps=%v", res.Steps)
	}
	if res.LossyApplied {
		t.Fatalf("mode without the lossy opt-in must not report lossy, got %+v", res)
	}
}

// TestApplyModeStackedFidelityRefusal: the fidelity gate still
// refuses on the stacked path — a lossy result that cuts a paren
// URL mid-string returns the original, with the fidelity step as
// the only step. Geometry copied from
// TestInflationGuardSkippedOnFidelityRefusal.
func TestApplyModeStackedFidelityRefusal(t *testing.T) {
	content := fidelityProse(12) + "See " + fidelityParenURL + " for details.\n" + fidelityProse(9)
	msgs := fidelitySystemUser(content)
	total := countTokens(t, msgs)
	out, res := Apply(inflationTestModel, msgs, modeReq(t, "stacked", total-2), nil, nil)
	if !reflect.DeepEqual(msgs, out) {
		t.Fatalf("stacked fidelity refusal must return the original messages")
	}
	if len(res.Steps) != 1 || res.Steps[0] != StepFidelityGateRefused {
		t.Fatalf("stacked fidelity refusal must record exactly [%q], got %v res=%+v", StepFidelityGateRefused, res.Steps, res)
	}
}

// TestApplyModeUnknownDefensive: an unparseable mode (a direct
// engine caller bypassing the HTTP 400) degrades to no-mode, never
// fails the request.
func TestApplyModeUnknownDefensive(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello world, this is a short prompt."},
	}
	_, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: 10000, Mode: "medium"}, nil, nil)
	if res.Mode != "" {
		t.Fatalf("unknown mode must degrade to no-mode, got %q", res.Mode)
	}
}
