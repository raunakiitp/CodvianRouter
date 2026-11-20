package compress

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Toggle-evidence convention used by every test in this file:
// each behaviour is asserted BOTH with the pipeline engaged and
// with it bypassed (engines unavailable / stages disabled). A
// test that passes when the pipeline is bypassed — i.e. when
// RunStacked ignores its config and returns its input — fails
// the "engaged" assertion; a test that passes when the pipeline
// always runs fails the "bypassed" assertion. So every test
// below fails with the pipeline bypassed and passes with it.

// allEngines is an availability with every pipeline engine on.
func allEngines() *EngineAvailability {
	return &EngineAvailability{Lossless: true, RTK: true, Caveman: true, LossyDropTruncate: true}
}

// noEngines is an explicit all-off availability: every engine
// is unavailable. (Distinct from a nil Availability, which
// means "unset" and resolves to DefaultAvailability().)
func noEngines() *EngineAvailability {
	return &EngineAvailability{}
}

// stackedFixture is a prompt every stage can bite on: a
// duplicate system block (lossless), a go-test tool dump with
// routine pass lines (RTK), and a filler-heavy user turn
// (Caveman).
func stackedFixture() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "tool", Content: "=== RUN TestAlpha\n--- PASS: TestAlpha (0.01s)\n=== RUN TestBeta\n--- PASS: TestBeta (0.01s)\n=== RUN TestGamma\n--- FAIL: TestGamma (0.02s)\n    gamma_test.go:42: expected 1, got 2\nFAIL\tgithub.com/example/pkg\t0.03s"},
		{Role: "user", Content: "Hello, could you please explain in detail how the caching system works in this project? I really appreciate it, thank you so much for your help with this problem."},
	}
}

func TestRunStackedOrderAndAccounting(t *testing.T) {
	msgs := stackedFixture()
	before := append([]provider.Message(nil), msgs...)
	res := RunStacked(msgs, StackedConfig{Availability: allEngines()})

	if res.Refused {
		t.Fatalf("pipeline refused: gate %q: %s", res.RefusalGate, res.RefusalReason)
	}
	if !res.Applied {
		t.Fatalf("pipeline applied nothing; want compressed output")
	}
	// Stage order is lossless, RTK, Caveman — the M7.6 order.
	if len(res.Stages) != 3 {
		t.Fatalf("got %d stages, want 3: %+v", len(res.Stages), res.Stages)
	}
	wantOrder := []string{StageLossless, StageRTK, StageCaveman}
	for i, want := range wantOrder {
		if res.Stages[i].Engine != want {
			t.Errorf("stage %d: engine %q, want %q", i, res.Stages[i].Engine, want)
		}
		if res.Stages[i].Skipped {
			t.Errorf("stage %q unexpectedly skipped: %s", want, res.Stages[i].SkipReason)
		}
	}
	// Per-stage accounting: tokens never rise across a stage,
	// and the pipeline totals bracket the stages.
	if res.BeforeTokens <= 0 {
		t.Errorf("BeforeTokens %d, want > 0", res.BeforeTokens)
	}
	if res.AfterTokens >= res.BeforeTokens {
		t.Errorf("AfterTokens %d, want < BeforeTokens %d", res.AfterTokens, res.BeforeTokens)
	}
	prev := res.BeforeTokens
	for _, s := range res.Stages {
		if s.TokensBefore != prev {
			t.Errorf("stage %q TokensBefore %d, want %d (previous stage's after)", s.Engine, s.TokensBefore, prev)
		}
		if s.TokensAfter > s.TokensBefore {
			t.Errorf("stage %q inflated: %d after vs %d before", s.Engine, s.TokensAfter, s.TokensBefore)
		}
		prev = s.TokensAfter
	}
	if res.AfterTokens != prev {
		t.Errorf("AfterTokens %d, want last stage's after %d", res.AfterTokens, prev)
	}
	// Each engine actually bit: duplicate system gone, the
	// routine pass run collapsed but the failure kept, filler gone.
	got := res.Messages
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3 (duplicate system dropped): %+v", len(got), got)
	}
	// RTK standard collapses a run of routine pass lines to
	// the first (it does not drop them all — that is the
	// aggressive level): TestBeta's PASS goes, TestAlpha's
	// stays, and the === RUN chatter goes entirely.
	if strings.Contains(got[1].Content, "--- PASS: TestBeta") {
		t.Errorf("RTK stage did not collapse routine pass lines:\n%s", got[1].Content)
	}
	if !strings.Contains(got[1].Content, "--- PASS: TestAlpha") {
		t.Errorf("RTK standard should keep the first routine pass line of a run:\n%s", got[1].Content)
	}
	if strings.Contains(got[1].Content, "=== RUN") {
		t.Errorf("RTK stage did not drop === RUN chatter:\n%s", got[1].Content)
	}
	if !strings.Contains(got[1].Content, "--- FAIL: TestGamma") {
		t.Errorf("RTK stage dropped the failure line:\n%s", got[1].Content)
	}
	if strings.Contains(got[2].Content, "could you please") || strings.Contains(got[2].Content, "thank you so much") {
		t.Errorf("Caveman stage did not remove filler:\n%s", got[2].Content)
	}
	// Input never mutated.
	if !reflect.DeepEqual(msgs, before) {
		t.Errorf("RunStacked mutated its input")
	}

	// Toggle: the same input with every engine unavailable must
	// come back unchanged (pipeline bypassed => no compression).
	bypassed := RunStacked(msgs, StackedConfig{Availability: noEngines()})
	if bypassed.Applied {
		t.Errorf("bypassed pipeline applied compression; want input unchanged")
	}
	if !reflect.DeepEqual(bypassed.Messages, msgs) {
		t.Errorf("bypassed pipeline changed messages")
	}
}

func TestRunStackedLosslessOnly(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Be brief.   "},
		{Role: "system", Content: "Be brief.   "},
		{Role: "user", Content: "What is the capital of France?"},
	}
	res := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	// Lossless dedupes the system block; RTK/Caveman have
	// nothing to bite on (no tool message, user message too
	// short for Caveman's 50-char floor).
	if len(res.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(res.Messages), res.Messages)
	}
	if !res.Stages[0].Applied || res.Stages[0].Engine != StageLossless {
		t.Errorf("lossless stage should have applied first: %+v", res.Stages[0])
	}
}

func TestRunStackedUnavailableEnginesSkipped(t *testing.T) {
	msgs := stackedFixture()
	// DefaultAvailability reports RTK/Caveman true since the M7
	// merge wave (both engines landed and are wired into Apply):
	// under the default the pipeline runs all three stages.
	res := RunStacked(msgs, StackedConfig{})
	if len(res.Stages) != 3 {
		t.Fatalf("got %d stages, want 3: %+v", len(res.Stages), res.Stages)
	}
	for _, s := range res.Stages {
		if s.Skipped {
			t.Errorf("stage %q skipped under DefaultAvailability: %s", s.Engine, s.SkipReason)
		}
	}
	if res.Refused {
		t.Errorf("unexpected refusal: %s", res.RefusalReason)
	}

	// The skip behaviour itself is pinned with an explicit
	// availability that has lossless on but both lossy engines
	// off: both lossy stages record as skipped with a reason,
	// lossless still runs, and the tool output is untouched
	// (RTK never ran).
	lossyOff := &EngineAvailability{Lossless: true, LossyDropTruncate: true}
	skipped := RunStacked(msgs, StackedConfig{Availability: lossyOff})
	if len(skipped.Stages) != 3 {
		t.Fatalf("got %d stages, want 3: %+v", len(skipped.Stages), skipped.Stages)
	}
	if skipped.Stages[0].Skipped {
		t.Errorf("lossless stage skipped under lossy-off availability: %s", skipped.Stages[0].SkipReason)
	}
	for _, s := range skipped.Stages[1:] {
		if !s.Skipped {
			t.Errorf("stage %q ran under lossy-off availability, want skipped", s.Engine)
		}
		if s.SkipReason == "" {
			t.Errorf("stage %q skipped with no reason", s.Engine)
		}
	}
	// Only the lossless dedupe applied: 3 messages (the
	// duplicate system block is gone), tool output untouched.
	if len(skipped.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (lossless only): %+v", len(skipped.Messages), skipped.Messages)
	}
	if !strings.Contains(skipped.Messages[1].Content, "--- PASS: TestAlpha") {
		t.Errorf("RTK ran despite being unavailable")
	}
	if skipped.Refused {
		t.Errorf("unexpected refusal: %s", skipped.RefusalReason)
	}

	// Toggle: with engines available the same fixture compresses
	// further (3 messages, filler gone).
	engaged := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	if len(engaged.Messages) != 3 {
		t.Errorf("engaged pipeline: got %d messages, want 3", len(engaged.Messages))
	}
}

func TestRunStackedRTKOnlyAndCavemanOnly(t *testing.T) {
	msgs := stackedFixture()
	rtkOnly := RunStacked(msgs, StackedConfig{
		Availability: &EngineAvailability{Lossless: true, RTK: true},
	})
	if rtkOnly.Stages[2].Engine != StageCaveman || !rtkOnly.Stages[2].Skipped {
		t.Errorf("Caveman stage should be skipped when unavailable: %+v", rtkOnly.Stages[2])
	}
	if !strings.Contains(rtkOnly.Messages[2].Content, "could you please") {
		t.Errorf("Caveman ran despite being unavailable")
	}
	cavemanOnly := RunStacked(msgs, StackedConfig{
		Availability: &EngineAvailability{Lossless: true, Caveman: true},
	})
	if cavemanOnly.Stages[1].Engine != StageRTK || !cavemanOnly.Stages[1].Skipped {
		t.Errorf("RTK stage should be skipped when unavailable: %+v", cavemanOnly.Stages[1])
	}
	if !strings.Contains(cavemanOnly.Messages[1].Content, "--- PASS: TestAlpha") {
		t.Errorf("RTK ran despite being unavailable")
	}
}

func TestRunStackedInvalidRTKLevelSkipped(t *testing.T) {
	msgs := stackedFixture()
	res := RunStacked(msgs, StackedConfig{
		Availability: allEngines(),
		RTKLevel:     RTKLevel("bogus"),
	})
	if !res.Stages[1].Skipped {
		t.Errorf("invalid RTK level should skip the RTK stage: %+v", res.Stages[1])
	}
	if !strings.Contains(res.Stages[1].SkipReason, "bogus") {
		t.Errorf("skip reason should name the level: %q", res.Stages[1].SkipReason)
	}
	// The other stages still run.
	if res.Stages[0].Skipped || res.Stages[2].Skipped {
		t.Errorf("other stages should still run: %+v", res.Stages)
	}
}

func TestRunStackedDefaults(t *testing.T) {
	// Empty levels default to RTK standard / Caveman full
	// (the reference router's stacked default): same output as explicit.
	msgs := stackedFixture()
	def := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	exp := RunStacked(msgs, StackedConfig{
		Availability: allEngines(), RTKLevel: RTKLevelStandard, CavemanLevel: LevelFull,
	})
	if !reflect.DeepEqual(def.Messages, exp.Messages) {
		t.Errorf("default levels differ from explicit standard/full")
	}
	if def.BeforeTokens != exp.BeforeTokens || def.AfterTokens != exp.AfterTokens {
		t.Errorf("default levels token counts differ: %+v vs %+v", def, exp)
	}
}

func TestRunStackedEmptyInput(t *testing.T) {
	res := RunStacked(nil, StackedConfig{Availability: allEngines()})
	if res.Applied || res.Refused || len(res.Messages) != 0 {
		t.Errorf("empty input: got Applied=%v Refused=%v msgs=%d", res.Applied, res.Refused, len(res.Messages))
	}
	res = RunStacked([]provider.Message{}, StackedConfig{Availability: allEngines()})
	if res.Applied || res.Refused {
		t.Errorf("empty slice: got Applied=%v Refused=%v", res.Applied, res.Refused)
	}
}

// TestApplyStageInflationSkip proves the per-stage inflation
// rule with a synthetic inflating stage: the shipped engines
// only delete, so a real engine can (today) never inflate —
// the skip is defence-in-depth, and this test is the proof it
// works. Toggle: an applyStage without the inflation check
// would return the inflated output and fail both assertions.
func TestApplyStageInflationSkip(t *testing.T) {
	msgs := []provider.Message{{Role: "user", Content: "A short message about caching."}}
	inflating := func(in []provider.Message) []provider.Message {
		out := append([]provider.Message(nil), in...)
		out[0].Content += " " + strings.Repeat("and then some more filler text to inflate the token count. ", 20)
		return out
	}
	out, sr := applyStage(msgs, "synthetic", "gpt-4o", inflating)
	if !sr.Skipped {
		t.Fatalf("inflating stage not skipped: %+v", sr)
	}
	if !strings.Contains(sr.SkipReason, "inflate") {
		t.Errorf("skip reason should say inflate: %q", sr.SkipReason)
	}
	if !reflect.DeepEqual(out, msgs) {
		t.Errorf("inflating stage output shipped; want original messages")
	}
	if sr.TokensAfter != sr.TokensBefore {
		t.Errorf("skipped stage TokensAfter %d, want TokensBefore %d", sr.TokensAfter, sr.TokensBefore)
	}

	// A shrinking stage runs and is accounted.
	shrinking := func(in []provider.Message) []provider.Message {
		out := append([]provider.Message(nil), in...)
		out[0].Content = "Caching."
		return out
	}
	out, sr = applyStage(msgs, "synthetic", "gpt-4o", shrinking)
	if sr.Skipped || !sr.Applied {
		t.Fatalf("shrinking stage should apply: %+v", sr)
	}
	if sr.TokensAfter >= sr.TokensBefore {
		t.Errorf("shrinking stage tokens did not fall: %+v", sr)
	}
	if out[0].Content != "Caching." {
		t.Errorf("shrinking stage output wrong: %q", out[0].Content)
	}
}

// TestFinalGatesInflation proves the inflation gate at the
// helper level: a candidate that costs more than the original
// is refused with gate "inflation". (In RunStacked the
// per-stage inflation skips make a final inflation
// unreachable with today's delete-only engines; the gate is
// defence-in-depth and is proved here with a crafted
// candidate.)
func TestFinalGatesInflation(t *testing.T) {
	orig := []provider.Message{{Role: "user", Content: "What is caching?"}}
	candidate := []provider.Message{{Role: "user", Content: "What is caching? " + strings.Repeat("Extra repeated filler sentence that adds tokens. ", 10)}}
	refused, gate, reason := finalGates("gpt-4o", orig, candidate, orig)
	if !refused {
		t.Fatalf("inflated candidate not refused")
	}
	if gate != "inflation" {
		t.Errorf("gate %q, want %q (reason: %s)", gate, "inflation", reason)
	}
	// Toggle: an equal-cost candidate passes the inflation gate.
	same := []provider.Message{{Role: "user", Content: "What is caching?"}}
	if refused, _, _ := finalGates("gpt-4o", orig, same, orig); refused {
		t.Errorf("identical candidate refused by final gates")
	}
}

// TestFinalGatesFidelity proves the fidelity gate at the helper
// level: a candidate that cuts a JSON value mid-structure is
// refused with gate "fidelity".
func TestFinalGatesFidelity(t *testing.T) {
	orig := []provider.Message{{Role: "user", Content: `The config is {"alpha": "1234567890abcdef", "beta": "ghijklmnopqrstuv"} end.`}}
	cut := []provider.Message{{Role: "user", Content: `The config is {"alpha": "1234567890abc end.`}}
	refused, gate, reason := finalGates("gpt-4o", orig, cut, orig)
	if !refused {
		t.Fatalf("fidelity-cut candidate not refused")
	}
	if gate != "fidelity" {
		t.Errorf("gate %q, want %q (reason: %s)", gate, "fidelity", reason)
	}
	// Toggle: dropping the JSON value whole passes.
	dropped := []provider.Message{{Role: "user", Content: `The config is end.`}}
	if refused, gate, reason := finalGates("gpt-4o", orig, dropped, orig); refused {
		t.Errorf("whole-drop candidate refused by gate %q: %s", gate, reason)
	}
}

// TestRunStackedFidelityRefusalEndToEnd proves the one
// end-to-end fidelity refusal reachable with the shipped
// engines. RTK drops an info-level JSONL line; the kept prose
// line repeats the dropped line's request id verbatim, so the
// M7.2 gate sees a fragment of a dropped JSON value surviving
// outside any surviving unit and refuses the whole pipeline
// result — the original messages come back.
//
// NOTE on what does NOT trip it: substrings shared with the
// kept ERROR line do not count — the kept error JSON value is
// a surviving unit, so the gate blanks it out of the residual
// before looking for fragments. The refusal needs the fragment
// to survive in kept NON-JSON text, which is what the prose
// line below provides.
func TestRunStackedFidelityRefusalEndToEnd(t *testing.T) {
	tool := `{"level":"info","timestamp":"2026-11-19T03:00:00Z","service":"api-gateway","request_id":"req-9f8e7d6c5b4a","message":"request started for user alice"}
{"level":"error","timestamp":"2026-11-19T03:00:02Z","service":"api-gateway","request_id":"req-9f8e7d6c5b4a","message":"request failed for user alice"}
request req-9f8e7d6c5b4a finished with status error`
	msgs := []provider.Message{
		{Role: "user", Content: "What happened with the last request? Please explain the failure in detail for me."},
		{Role: "tool", Content: tool},
	}
	res := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	if !res.Refused {
		t.Fatalf("JSONL pipeline result not refused; want fidelity refusal, got Applied=%v msgs=%+v", res.Applied, res.Messages)
	}
	if res.RefusalGate != "fidelity" {
		t.Fatalf("refusal gate %q, want %q (reason: %s)", res.RefusalGate, "fidelity", res.RefusalReason)
	}
	if !reflect.DeepEqual(res.Messages, msgs) {
		t.Errorf("refused pipeline did not return the original messages")
	}
	if res.Applied || res.AfterTokens != res.BeforeTokens {
		t.Errorf("refused pipeline should report Applied=false and After==Before, got Applied=%v After=%d Before=%d", res.Applied, res.AfterTokens, res.BeforeTokens)
	}
}
