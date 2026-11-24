package evalharness

// Fidelity metrics for the compression benchmark (P1.4).
//
// What these numbers are: deterministic, reproducible PROXIES computed
// by this package over a fixed corpus. They measure structural
// survival of prompt units (instruction text, tool calls, tool
// results, parseable JSON, sentences carrying concrete facts, prose
// words). They are NOT semantic proofs — a high score does not prove
// the compressed prompt still asks the same thing, and a low score on
// lossy is expected by contract (lossy deletes middle turns on
// purpose). The honesty rule is: report the number, name its
// definition, never present a proxy as meaning preservation.
//
// Definitions (all comparisons are on normalised text — whitespace
// runs collapsed to one space, trimmed — so lossless whitespace
// stripping and system-deduplication do not read as loss):
//
//   - InstructionPreservation: fraction of system/developer messages
//     whose normalised content appears verbatim in the compressed
//     transcript. Duplicate system blocks that lossless dedupe merges
//     still count as preserved (the text is present once).
//   - ToolCallPreservation: fraction of assistant tool calls whose
//     name AND argument text both survive verbatim.
//   - ToolResultPreservation: fraction of tool-result messages
//     (keyed by tool_call_id) whose content survives verbatim.
//   - JSONPreserved: fraction of parseable JSON values in the
//     original that survive byte-equivalently after normalisation.
//     Separately, JSONSplices counts original values that were cut —
//     a long prefix or suffix fragment of a dropped value reappearing
//     in the output. Splices are corruption, not loss.
//   - FactPreservation: fraction of sentences carrying a concrete
//     marker (a digit, a date-like run, a URL, or a quoted literal)
//     whose normalised text survives. Plain opinion sentences do not
//     count as facts here.
//   - ProsePreservation: set-based word containment — fraction of the
//     distinct lowercased word types in user/assistant prose that
//     still appear in the compressed transcript. Set semantics so a
//     lossless dedupe cannot read as prose loss.
//
// Every ratio is 0 when its denominator is 0 (no units of that kind),
// so an empty category reads as "nothing to preserve", not as a
// perfect score silently.

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// FidelityUnits records the raw counts behind every ratio, so a
// reported 1.00 can be audited ("12/12 instructions kept") instead of
// trusted.
type FidelityUnits struct {
	Instructions     int `json:"instructions"`
	InstructionsKept int `json:"instructions_kept"`
	ToolCalls        int `json:"tool_calls"`
	ToolCallsKept    int `json:"tool_calls_kept"`
	ToolResults      int `json:"tool_results"`
	ToolResultsKept  int `json:"tool_results_kept"`
	JSONValues       int `json:"json_values"`
	JSONValuesKept   int `json:"json_values_kept"`
	JSONSplices      int `json:"json_splices"`
	Facts            int `json:"facts"`
	FactsKept        int `json:"facts_kept"`
	ProseWordTypes   int `json:"prose_word_types"`
	ProseWordsKept   int `json:"prose_words_kept"`
}

// Fidelity is the measured outcome for one (case, engine) pair.
type Fidelity struct {
	InstructionPreservation float64       `json:"instruction_preservation"`
	ToolCallPreservation    float64       `json:"tool_call_preservation"`
	ToolResultPreservation  float64       `json:"tool_result_preservation"`
	JSONPreserved           float64       `json:"json_preserved"`
	FactPreservation        float64       `json:"fact_preservation"`
	ProsePreservation       float64       `json:"prose_preservation"`
	Units                   FidelityUnits `json:"units"`
}

// MeasureFidelity computes every metric between the original prompt
// and the compressed output. It never fails: a metric it cannot
// compute is 0 with a zero denominator, and Check decides whether that
// is acceptable.
//
// The comparison baseline depends on the engine, because "preserved"
// means different things:
//
//   - lossless: the contract is the canonical normal form
//     (compress.Normalise), so metrics compare Normalise(original)
//     against the output. Canonicalisation itself — whitespace
//     stripping, system dedupe, keeping the last tool result per
//     call id — is not loss and must not be scored as loss.
//   - lossy: the contract is the raw original; anything present in
//     the original and absent from the output was dropped by the
//     client-opted-in trim.
func MeasureFidelity(engine Engine, original, compressed []provider.Message) Fidelity {
	baseline := original
	if engine == EngineLossless {
		baseline = compress.Normalise(original)
	}
	outText := transcript(compressed)

	f := Fidelity{}
	f.Units.Instructions, f.Units.InstructionsKept = keptUnits(outText, instructionUnits(baseline))
	f.Units.ToolCalls, f.Units.ToolCallsKept = keptUnits(outText, toolCallUnits(baseline))
	f.Units.ToolResults, f.Units.ToolResultsKept = keptUnits(outText, toolResultUnits(baseline))

	jsonVals := jsonUnits(baseline)
	f.Units.JSONValues = len(jsonVals)
	for _, v := range jsonVals {
		if strings.Contains(outText, v) {
			f.Units.JSONValuesKept++
			continue
		}
		if jsonFragmentReappears(outText, v) {
			f.Units.JSONSplices++
		}
	}

	f.Units.Facts, f.Units.FactsKept = keptUnits(outText, factUnits(baseline))

	origWords := proseWordSet(baseline)
	outSet := wordSet([]string{outText})
	kept := 0
	for w := range origWords {
		if outSet[w] {
			kept++
		}
	}
	f.Units.ProseWordTypes = len(origWords)
	f.Units.ProseWordsKept = kept

	f.InstructionPreservation = ratio(f.Units.InstructionsKept, f.Units.Instructions)
	f.ToolCallPreservation = ratio(f.Units.ToolCallsKept, f.Units.ToolCalls)
	f.ToolResultPreservation = ratio(f.Units.ToolResultsKept, f.Units.ToolResults)
	f.JSONPreserved = ratio(f.Units.JSONValuesKept, f.Units.JSONValues)
	f.FactPreservation = ratio(f.Units.FactsKept, f.Units.Facts)
	f.ProsePreservation = ratio(f.Units.ProseWordsKept, f.Units.ProseWordTypes)
	return f
}

func ratio(kept, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(kept) / float64(total)
}

// transcript flattens messages into the text a containment check runs
// against: content, then tool-call name and arguments, then tool
// result ids. Normalised.
func transcript(msgs []provider.Message) string {
	parts := make([]string, 0, len(msgs)*3)
	for _, m := range msgs {
		if m.Content != "" {
			parts = append(parts, m.Content)
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, tc.Function.Name+"\x1f"+tc.Function.Arguments)
			if tc.ID != "" {
				parts = append(parts, tc.ID)
			}
		}
		if m.ToolCallID != "" {
			parts = append(parts, m.ToolCallID+"\x1f"+m.Content)
		}
	}
	return normalise(strings.Join(parts, "\n"))
}

func normalise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func instructionUnits(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if n := normalise(m.Content); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

func toolCallUnits(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			out = append(out, normalise(tc.Function.Name+"\x1f"+tc.Function.Arguments))
		}
	}
	return out
}

func toolResultUnits(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "tool" || m.ToolCallID != "" {
			out = append(out, normalise(m.ToolCallID+"\x1f"+m.Content))
		}
	}
	return out
}

func factUnits(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, sent := range splitSentences(m.Content) {
			n := normalise(sent)
			if n == "" {
				continue
			}
			if hasConcreteMarker(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

func splitSentences(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\n' })
}

func hasConcreteMarker(s string) bool {
	hasDigit := false
	for _, r := range s {
		if unicode.IsDigit(r) {
			hasDigit = true
			break
		}
	}
	if hasDigit {
		return true
	}
	if strings.Contains(s, "http://") || strings.Contains(s, "https://") {
		return true
	}
	if strings.Contains(s, "\"") || strings.Contains(s, "'") {
		return true
	}
	return false
}

func keptUnits(outText string, units []string) (total, kept int) {
	total = len(units)
	for _, u := range units {
		if u != "" && strings.Contains(outText, u) {
			kept++
		}
	}
	return total, kept
}

// jsonUnits extracts every parseable JSON object/array in the
// original messages, normalised. The scanner is deterministic and
// string-aware (braces inside strings do not open a value).
func jsonUnits(msgs []provider.Message) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range msgs {
		for _, v := range scanJSON(m.Content) {
			n := normalise(v)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
		for _, tc := range m.ToolCalls {
			for _, v := range scanJSON(tc.Function.Arguments) {
				n := normalise(v)
				if n == "" || seen[n] {
					continue
				}
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// scanJSON returns the balanced, parseable top-level JSON values in s.
func scanJSON(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '{' && c != '[' {
			i++
			continue
		}
		end := matchBalanced(s, i)
		if end < 0 {
			i++
			continue
		}
		cand := s[i:end]
		if json.Valid([]byte(cand)) {
			out = append(out, cand)
		}
		i = end
	}
	return out
}

// matchBalanced returns the index just past the value opened at start,
// or -1 if it never closes. String-aware; escapes honoured.
func matchBalanced(s string, start int) int {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// jsonFragmentReappears reports whether a dropped JSON value left a
// long prefix or suffix fragment behind — the signature of a mid-value
// cut (or a head+tail splice). The floor keeps ordinary prose from
// tripping the check.
func jsonFragmentReappears(outText, value string) bool {
	const floor = 24
	if len(value) < floor*2 {
		return false
	}
	prefix := value[:floor]
	suffix := value[len(value)-floor:]
	return strings.Contains(outText, prefix) || strings.Contains(outText, suffix)
}

// proseWordSet returns the distinct lowercased word types in
// user/assistant content. Set semantics: duplicates collapse, so a
// lossless dedupe cannot read as prose loss.
func proseWordSet(msgs []provider.Message) map[string]bool {
	set := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		for _, w := range words(m.Content) {
			set[w] = true
		}
	}
	return set
}

func wordSet(texts []string) map[string]bool {
	set := map[string]bool{}
	for _, t := range texts {
		for _, w := range words(t) {
			set[w] = true
		}
	}
	return set
}

// words splits on non-alphanumerics and lowercases. Deterministic.
func words(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(f))
	}
	sort.Strings(out)
	return out
}
