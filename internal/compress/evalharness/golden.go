package evalharness

// goldenInputTokens pins the exact BPE input-token count of every
// corpus case, measured by a real harness run (go test
// ./internal/compress/evalharness/ -run TestPrintMeasuredSavingsTable
// -v -count=1, Go 1.27.4, model gpt-4o). Values are filled in from
// that run's output — never hand-estimated. See
// TestGoldenInputTokens.
var goldenInputTokens = map[string]int{
	"go-code-fenced":         80,
	"python-code-fenced":     69,
	"json-nested":            123,
	"json-tabular":           150,
	"prose-short":            12,
	"prose-long":             184,
	"tool-output-json":       78,
	"tool-output-ansi":       135,
	"system-prompt-heavy":    108,
	"mixed-code-tool-prose":  87,
	"noop-clean-prose":       7,
	"whitespace-heavy-prose": 48,
	"multi-turn-history":     119,
}
