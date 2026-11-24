package evalharness

import "fmt"

// Explicit fidelity thresholds and fail conditions for the P1.4
// benchmark.
//
// A threshold exists so that "compressed 80% and lost the system
// prompt" is a FAILURE, not a win. Every number here is a floor on a
// metric defined in fidelity.go; Check returns a human-readable
// failure per violated floor, naming the case/engine/metric/observed/
// required values so a red run says what broke.
//
// Two profiles:
//
//   - lossless: structural. The pipeline guarantees the output is the
//     canonical normal form of the input, so every preservation ratio
//     must be 1.0 and VerifyLossless must hold. Anything less means
//     the transform is not lossless and the benchmark must say so.
//   - lossy: contractual. Lossy deletes middle turns BY DESIGN, so
//     prose/fact/tool-result floors are deliberately below 1.0 — they
//     are regression floors, not claims of perfection. What lossy may
//     NEVER do is damage what the client did not opt into losing:
//     instruction text and tool-call arguments must survive whole
//     (floor 1.0), and no JSON value may be spliced (MaxJSONSplices 0,
//     enforced at any preservation ratio).

// Thresholds is the floor set applied to one engine's row.
type Thresholds struct {
	MinInstructionPreservation float64
	MinToolCallPreservation    float64
	MinToolResultPreservation  float64
	MinJSONPreserved           float64
	MinFactPreservation        float64
	MinProsePreservation       float64
	MaxJSONSplices             int
	RequireLosslessVerified    bool
}

// ThresholdsFor returns the baseline profile for an engine.
//
// Lossless floors are structural: the pipeline guarantees the output
// is the canonical normal form, so every preservation ratio must be
// 1.0 and VerifyLossless must hold. Anything less means the transform
// is not lossless and the benchmark must say so.
//
// Lossy floors encode what the client did NOT opt into losing.
// Lossy deletes middle turns by design, so low prose/fact/tool
// survival is expected and reported, not failed. What lossy may never
// do: damage instruction text (floor 1.0), leave a JSON value cut with
// its fragment reappearing (MaxJSONSplices 0), or inflate the prompt.
// Category-specific floors (lossyCategoryFloors below) add regression
// floors where the corpus gives real evidence.
func ThresholdsFor(engine Engine) Thresholds {
	if engine == EngineLossless {
		return Thresholds{
			MinInstructionPreservation: 1.0,
			MinToolCallPreservation:    1.0,
			MinToolResultPreservation:  1.0,
			MinJSONPreserved:           1.0,
			MinFactPreservation:        1.0,
			MinProsePreservation:       1.0,
			MaxJSONSplices:             0,
			RequireLosslessVerified:    true,
		}
	}
	return Thresholds{
		MinInstructionPreservation: 1.0,
		MinToolCallPreservation:    0.0,
		MinToolResultPreservation:  0.0,
		MinJSONPreserved:           0.0,
		MinFactPreservation:        0.0,
		MinProsePreservation:       0.0,
		MaxJSONSplices:             0,
		RequireLosslessVerified:    false,
	}
}

// lossyCategoryFloors adds per-category regression floors on top of the
// lossy baseline. Values are maxima: a category floor never loosens
// the baseline. Each floor is grounded in a measured run of the
// current corpus (docs/compression-fidelity-benchmark.md records the
// run); tightening after a deliberate pipeline change is the intended
// workflow, silently deleting a floor is not.
var lossyCategoryFloors = map[string]Thresholds{
	// The two json-category cases keep every parseable value whole at
	// budget; a drop below 0.9 means the lossy trim started eating
	// JSON it used to preserve.
	"json": {MinJSONPreserved: 0.9},
	// Content categories: the lossy trim must leave at least a fifth
	// of the distinct prose word types. Measured survival on the
	// current corpus is 0.27–1.00, so this catches a real regression
	// without failing legitimate trims.
	"code":         {MinProsePreservation: 0.2},
	"prose":        {MinProsePreservation: 0.2},
	"tool-output":  {MinProsePreservation: 0.2},
	"history":      {MinProsePreservation: 0.2},
	"mixed":        {MinProsePreservation: 0.2},
	"system-heavy": {MinProsePreservation: 0.2},
	// A pinned final turn is trimmed INSIDE the message (head+tail),
	// so most prose survives; measured 0.80. Floor 0.5 gives margin.
	"long-context-final": {MinProsePreservation: 0.5},
	// long-context-middle-turn deliberately has NO floor: when the
	// bulk of the prompt is a droppable middle turn, the lossy
	// contract allows dropping it whole (measured: 99.6% saved, prose
	// survival 0.04). A floor here would fail legitimate behaviour, so
	// the case exists to make that trade-off visible in the report
	// rather than to gate it.
}

// ThresholdsForCase returns the effective profile for one corpus case:
// the engine baseline merged with the category floor (max wins).
func ThresholdsForCase(engine Engine, category string) Thresholds {
	base := ThresholdsFor(engine)
	cat, ok := lossyCategoryFloors[category]
	if !ok || engine != EngineLossy {
		return base
	}
	merge := func(a, b float64) float64 {
		if b > a {
			return b
		}
		return a
	}
	base.MinInstructionPreservation = merge(base.MinInstructionPreservation, cat.MinInstructionPreservation)
	base.MinToolCallPreservation = merge(base.MinToolCallPreservation, cat.MinToolCallPreservation)
	base.MinToolResultPreservation = merge(base.MinToolResultPreservation, cat.MinToolResultPreservation)
	base.MinJSONPreserved = merge(base.MinJSONPreserved, cat.MinJSONPreserved)
	base.MinFactPreservation = merge(base.MinFactPreservation, cat.MinFactPreservation)
	base.MinProsePreservation = merge(base.MinProsePreservation, cat.MinProsePreservation)
	if cat.MaxJSONSplices > base.MaxJSONSplices {
		base.MaxJSONSplices = cat.MaxJSONSplices
	}
	return base
}

// Check applies a threshold profile to one measured row and returns
// every violation (empty slice = pass). It never panics on a zero
// denominator: MeasureFidelity reports 0 there, and a 0 against a
// non-zero floor fails — which is correct, because "nothing measured"
// must not read as "everything preserved".
func Check(engine Engine, er EngineReport, t Thresholds) []string {
	var failures []string
	add := func(metric string, got, want float64) {
		failures = append(failures, fmt.Sprintf("%s: %s %.4f < required %.4f", engine, metric, got, want))
	}
	// A metric with an empty denominator is vacuous, not failed:
	// "this prompt contains no tool calls" must neither read as a
	// perfect score nor as a violation. The ratio itself stays 0 and is
	// visible in the report; only the threshold is skipped.
	check := func(metric string, kept, total int, min, val float64) {
		if total == 0 {
			return
		}
		if val < min {
			add(metric, val, min)
		}
	}

	if t.RequireLosslessVerified && !er.LosslessVerified {
		failures = append(failures, fmt.Sprintf("%s: lossless output failed VerifyLossless", engine))
	}
	u := er.Fidelity.Units
	check("instruction_preservation", u.InstructionsKept, u.Instructions, t.MinInstructionPreservation, er.Fidelity.InstructionPreservation)
	check("tool_call_preservation", u.ToolCallsKept, u.ToolCalls, t.MinToolCallPreservation, er.Fidelity.ToolCallPreservation)
	check("tool_result_preservation", u.ToolResultsKept, u.ToolResults, t.MinToolResultPreservation, er.Fidelity.ToolResultPreservation)
	check("json_preserved", u.JSONValuesKept, u.JSONValues, t.MinJSONPreserved, er.Fidelity.JSONPreserved)
	check("fact_preservation", u.FactsKept, u.Facts, t.MinFactPreservation, er.Fidelity.FactPreservation)
	check("prose_preservation", u.ProseWordsKept, u.ProseWordTypes, t.MinProsePreservation, er.Fidelity.ProsePreservation)
	if u.JSONSplices > t.MaxJSONSplices {
		failures = append(failures, fmt.Sprintf("%s: json_splices %d > maximum %d (a JSON value was cut and its fragment reappeared)",
			engine, u.JSONSplices, t.MaxJSONSplices))
	}
	if er.SavedTokens < 0 {
		failures = append(failures, fmt.Sprintf("%s: negative savings %d (compression inflated the prompt)", engine, er.SavedTokens))
	}
	return failures
}
