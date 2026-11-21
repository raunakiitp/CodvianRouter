package eval

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// T12: the permanent regression case set is wired end-to-end through the
// harness against the scripted fake engine — every case passes, so a
// change to the Engine contract, Case fields or the Run loop breaks a
// named regression case instead of silently passing. The fake returns
// the case's expected markers verbatim (no network), proving the harness
// wiring, not a model's answer quality (see the package doc).
func TestPermanentRegressionCasesRunThroughHarness(t *testing.T) {
	cases := PermanentRegressionCases()
	if len(cases) < 8 {
		t.Fatalf("permanent case set shrank to %d; expected >= 8", len(cases))
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ID] {
			t.Fatalf("duplicate permanent case ID %q", c.ID)
		}
		seen[c.ID] = true
		if c.ID == "" {
			t.Fatal("a permanent case has an empty ID")
		}
	}

	h := &Harness{Engine: scriptedEngine{}, Timeout: 0}
	rep := h.Run(context.Background(), cases)
	if rep.Total != len(cases) {
		t.Fatalf("report total = %d, want %d", rep.Total, len(cases))
	}
	for _, r := range rep.Results {
		if !r.OK {
			t.Errorf("case %q failed: error=%q missing=%v response=%q", r.CaseID, r.Error, r.Missing, r.Response)
		}
	}
	if rep.Failed != 0 || rep.Passed != len(cases) {
		t.Fatalf("report = %d/%d passed, want all pass", rep.Passed, rep.Total)
	}
	if rep.PassRate != 1.0 {
		t.Fatalf("pass rate = %v, want 1.0", rep.PassRate)
	}
}

// The fake engine must actually be used (non-nil, reachable), or the
// whole test is vacuous. This guards against someone gutting the
// scriptedEngine body later.
func TestPermanentRegressionScriptedEngineServesMarkers(t *testing.T) {
	eng := scriptedEngine{}
	resp, err := eng.Chat(context.Background(), chatRequestWith("Think step by step and answer."))
	if err != nil || resp == nil || resp.Content != "reasoning: considered options" {
		t.Fatalf("scripted engine did not serve the reasoning marker: resp=%+v err=%v", resp, err)
	}
	// Empty-prompt case must not panic the fake.
	if _, err := eng.Chat(context.Background(), chatRequestWith("")); err != nil {
		t.Fatalf("scripted engine errored on empty prompt: %v", err)
	}
}

func chatRequestWith(content string) provider.ChatRequest {
	return provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: content}}}
}
