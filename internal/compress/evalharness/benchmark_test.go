package evalharness

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestPrintCompressionBenchmark is the P1.4 entry point: it runs both
// engines over the fixed corpus, applies the per-engine thresholds and
// prints the measured table plus the machine-readable JSON. The text
// table is the source for docs/compression-fidelity-benchmark.md —
// copy verbatim, never retype.
//
//	go test ./internal/compress/evalharness/ -run TestPrintCompressionBenchmark -v -count=1
func TestPrintCompressionBenchmark(t *testing.T) {
	report, err := RunBenchmark(Options{Model: DefaultModel, Cases: Corpus(), Repeats: 3})
	if err != nil {
		t.Fatalf("benchmark run failed: %v", err)
	}
	t.Logf("\n%s", RenderReportText(report))
	jsonBytes, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON render failed: %v", err)
	}
	t.Logf("\n%s", jsonBytes)
}

// TestPrintLongContextBenchmark runs the long-context corpus through
// the same benchmark so long-prompt behaviour is measured, not
// assumed.
func TestPrintLongContextBenchmark(t *testing.T) {
	cases := LongContextCorpus()
	if len(cases) == 0 {
		t.Fatal("long-context corpus is empty")
	}
	report, err := RunBenchmark(Options{Model: DefaultModel, Cases: cases, Repeats: 1})
	if err != nil {
		t.Fatalf("long-context benchmark failed: %v", err)
	}
	t.Logf("\n%s", RenderReportText(report))
	for _, c := range report.Cases {
		for _, e := range c.Engines {
			if e.InputTokens < 3000 {
				t.Fatalf("case %s engine %s: input %d tokens — long-context case is not long", c.Name, e.Engine, e.InputTokens)
			}
		}
		var lossy *EngineReport
		for i := range c.Engines {
			if c.Engines[i].Engine == EngineLossy {
				lossy = &c.Engines[i]
			}
		}
		if lossy == nil {
			t.Fatalf("case %s: no lossy row", c.Name)
		}
		if lossy.OutputTokens >= lossy.InputTokens {
			t.Fatalf("case %s: lossy output %d >= input %d — long prompt was not trimmed",
				c.Name, lossy.OutputTokens, lossy.InputTokens)
		}
	}
}

// TestBenchmarkStableJSONDeterministic: two runs produce byte-identical
// JSONStable output (latency excluded), and the latency-bearing JSON is
// also structurally stable apart from durations (same length fields,
// same pass/fail).
func TestBenchmarkStableJSONDeterministic(t *testing.T) {
	first, err := RunBenchmark(Options{Model: DefaultModel, Cases: Corpus(), Repeats: 1})
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	second, err := RunBenchmark(Options{Model: DefaultModel, Cases: Corpus(), Repeats: 1})
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	a, err := first.JSONStable()
	if err != nil {
		t.Fatalf("stable JSON failed: %v", err)
	}
	b, err := second.JSONStable()
	if err != nil {
		t.Fatalf("stable JSON failed: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("stable JSON differs between runs:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}
	if !strings.Contains(string(a), "\"totals\"") || !strings.Contains(string(a), "\"fidelity\"") {
		t.Fatalf("stable JSON is missing expected sections:\n%s", a)
	}
	txtA, txtB := RenderReportText(first), RenderReportText(second)
	if txtA != txtB {
		t.Fatalf("text report differs between runs:\n--- first ---\n%s\n--- second ---\n%s", txtA, txtB)
	}
}

// TestThresholdGateCatchesViolations: every threshold profile rejects
// a row that violates it. Tamper one metric at a time and require a
// non-empty failure naming the metric.
func TestThresholdGateCatchesViolations(t *testing.T) {
	base := EngineReport{
		Engine:           EngineLossless,
		InputTokens:      100,
		OutputTokens:     90,
		SavedTokens:      10,
		LosslessVerified: true,
		Fidelity: Fidelity{
			InstructionPreservation: 1.0,
			ToolCallPreservation:    1.0,
			ToolResultPreservation:  1.0,
			JSONPreserved:           1.0,
			FactPreservation:        1.0,
			ProsePreservation:       1.0,
			Units: FidelityUnits{
				Instructions: 1, InstructionsKept: 1,
				ToolCalls: 1, ToolCallsKept: 1,
				ToolResults: 1, ToolResultsKept: 1,
				JSONValues: 1, JSONValuesKept: 1,
				Facts: 1, FactsKept: 1,
				ProseWordTypes: 10, ProseWordsKept: 10,
			},
		},
	}
	if got := Check(EngineLossless, base, ThresholdsFor(EngineLossless)); len(got) != 0 {
		t.Fatalf("a fully-compliant lossless row failed: %v", got)
	}

	cases := []struct {
		name   string
		mutate func(*EngineReport)
		want   string
	}{
		{"instruction loss", func(r *EngineReport) { r.Fidelity.InstructionPreservation = 0.5 }, "instruction_preservation"},
		{"tool call loss", func(r *EngineReport) { r.Fidelity.ToolCallPreservation = 0.5 }, "tool_call_preservation"},
		{"json damage", func(r *EngineReport) { r.Fidelity.JSONPreserved = 0.5 }, "json_preserved"},
		{"prose loss", func(r *EngineReport) { r.Fidelity.ProsePreservation = 0.5 }, "prose_preservation"},
		{"negative savings", func(r *EngineReport) { r.SavedTokens = -1; r.OutputTokens = 101 }, "negative savings"},
	}
	for _, tc := range cases {
		row := base
		tc.mutate(&row)
		got := Check(EngineLossless, row, ThresholdsFor(EngineLossless))
		if len(got) == 0 {
			t.Fatalf("%s: threshold gate accepted a violating row", tc.name)
		}
		if !strings.Contains(strings.Join(got, "; "), tc.want) {
			t.Fatalf("%s: failure %q does not name the metric", tc.name, got)
		}
	}

	// Lossless verification flag is mandatory for the lossless profile.
	row := base
	row.LosslessVerified = false
	if got := Check(EngineLossless, row, ThresholdsFor(EngineLossless)); len(got) == 0 {
		t.Fatal("lossless gate accepted an unverified row")
	}

	// A JSON splice fails even when the ratio floor would be met.
	spliced := base
	spliced.Fidelity.Units.JSONSplices = 1
	if got := Check(EngineLossless, spliced, ThresholdsFor(EngineLossless)); len(got) == 0 {
		t.Fatal("gate accepted a row with a JSON splice")
	}

	// A row whose metrics have empty denominators (no instructions, no
	// tool calls, no JSON, no facts in the prompt) is vacuous, not a
	// violation: zero-denominator ratios stay visible as 0 but do not
	// fail the threshold.
	vacuous := base
	vacuous.Fidelity = Fidelity{}
	if got := Check(EngineLossless, vacuous, ThresholdsFor(EngineLossless)); len(got) != 0 {
		t.Fatalf("empty-denominator metrics failed the gate: %v", got)
	}

	// The lossy profile must NOT inherit the lossless floors: a lossy
	// row with dropped prose passes its own profile while failing the
	// lossless one.
	lossyRow := base
	lossyRow.Engine = EngineLossy
	lossyRow.LosslessVerified = false
	lossyRow.Fidelity.ProsePreservation = 0.4
	if got := Check(EngineLossy, lossyRow, ThresholdsFor(EngineLossy)); len(got) != 0 {
		t.Fatalf("lossy profile rejected a compliant lossy row: %v", got)
	}
	if got := Check(EngineLossless, lossyRow, ThresholdsFor(EngineLossless)); len(got) == 0 {
		t.Fatal("lossless profile accepted a lossy row with dropped prose")
	}
}

// TestFidelityMetricDefinitions pins what each metric actually
// detects, on small hand-built transcripts:
//
//   - an instruction message that survives is preserved; one that is
//     deleted drops InstructionPreservation below 1;
//   - a tool call whose arguments survive counts as kept;
//   - a JSON object cut mid-string leaves a fragment and is counted as
//     a splice, not as preserved;
//   - a sentence containing a number counts as a fact.
func TestFidelityMetricDefinitions(t *testing.T) {
	original := []provider.Message{
		{Role: "system", Content: "Always answer in French."},
		{Role: "user", Content: "Order 42 shipped on 2026-01-02. Ref https://example.com/a."},
		{Role: "assistant", Content: "Calling the tool.", ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_order", Arguments: `{"id": 42}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"status": "delivered"}`},
	}

	surviving := MeasureFidelity(EngineLossless, original, original)
	if surviving.Units.Instructions != 1 || surviving.Units.InstructionsKept != 1 {
		t.Fatalf("identity: instructions %d/%d, want 1/1", surviving.Units.InstructionsKept, surviving.Units.Instructions)
	}
	if surviving.Units.ToolCalls != 1 || surviving.Units.ToolCallsKept != 1 {
		t.Fatalf("identity: tool calls %d/%d, want 1/1", surviving.Units.ToolCallsKept, surviving.Units.ToolCalls)
	}
	if surviving.Units.ToolResults != 1 || surviving.Units.ToolResultsKept != 1 {
		t.Fatalf("identity: tool results %d/%d, want 1/1", surviving.Units.ToolResultsKept, surviving.Units.ToolResults)
	}
	if surviving.Units.JSONValues == 0 {
		t.Fatal("identity: no JSON values detected — scanner is broken")
	}
	if surviving.Units.JSONValuesKept != surviving.Units.JSONValues {
		t.Fatalf("identity: JSON kept %d of %d", surviving.Units.JSONValuesKept, surviving.Units.JSONValues)
	}
	if surviving.Units.Facts == 0 || surviving.Units.FactsKept != surviving.Units.Facts {
		t.Fatalf("identity: facts %d/%d", surviving.Units.FactsKept, surviving.Units.Facts)
	}
	if surviving.InstructionPreservation != 1 || surviving.ProsePreservation != 1 {
		t.Fatalf("identity: instruction %v prose %v, want 1/1",
			surviving.InstructionPreservation, surviving.ProsePreservation)
	}

	droppedInstruction := []provider.Message{original[1], original[2], original[3]}
	lost := MeasureFidelity(EngineLossy, original, droppedInstruction)
	if lost.InstructionPreservation != 0 {
		t.Fatalf("dropped system message: instruction preservation %v, want 0", lost.InstructionPreservation)
	}

	// A JSON value cut mid-string leaves a long prefix fragment behind:
	// counted as a splice (corruption), never as preserved.
	longValue := `{"note": "abcdefghijklmnopqrstuvwxyz0123456789"}`
	cutOriginal := []provider.Message{{Role: "user", Content: longValue}}
	cutOutput := []provider.Message{{Role: "user", Content: `{"note": "abcdefghijklmnopqrstuvwx`}}
	cut := MeasureFidelity(EngineLossy, cutOriginal, cutOutput)
	if cut.Units.JSONValues != 1 {
		t.Fatalf("scanner found %d JSON values in the original, want 1", cut.Units.JSONValues)
	}
	if cut.Units.JSONValuesKept != 0 {
		t.Fatalf("a cut JSON value was counted as preserved (%d)", cut.Units.JSONValuesKept)
	}
	if cut.Units.JSONSplices == 0 {
		t.Fatal("a cut JSON value left no splice fragment — detection is broken")
	}

	// A JSON value dropped with its whole message is loss, not a
	// splice: kept=0, splices=0.
	dropped := MeasureFidelity(EngineLossy, cutOriginal, []provider.Message{{Role: "user", Content: "nothing here"}})
	if dropped.Units.JSONValuesKept != 0 || dropped.Units.JSONSplices != 0 {
		t.Fatalf("dropped-whole JSON: kept=%d splices=%d, want 0/0",
			dropped.Units.JSONValuesKept, dropped.Units.JSONSplices)
	}
}
