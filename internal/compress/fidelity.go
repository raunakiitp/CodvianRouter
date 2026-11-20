// Fidelity gate for the lossy compression path (M7.2).
//
// Protection guarantee — what CheckLossy proves before a lossy
// result may be sent:
//
//   - Code fences: every fenced block in the original either
//     survives byte-identical in the compressed output or is
//     dropped whole with its message. A fence is never cut
//     mid-block, never left unclosed by the cut, and never
//     spliced into a block that did not exist in the original
//     (head+tail joins across a cut can fabricate one).
//   - JSON: every complete JSON object/array in the original
//     either survives byte-identical (parseable) or is dropped
//     whole. A JSON value is never cut mid-string or
//     mid-structure, and no spliced JSON value (head of one
//     value joined to the tail of another) is ever served.
//   - URLs: every URL in the original either survives intact or
//     is dropped whole. A URL is never cut mid-string, and no
//     spliced URL is ever served.
//   - Identifiers and structure: message order is preserved
//     (compressed is a subsequence of the original), roles,
//     names, tool-call ids and tool message ids are unchanged,
//     part-carrying (multimodal) messages are kept byte-identical
//     or dropped whole — never partially rewritten — and the
//     last message keeps its role and identifiers.
//
// The gate is conservative by design: on any doubt it FAILS.
// Apply treats a failure as "send the original messages
// unchanged" (Applied=false, SavedTokens=0, Steps records
// StepFidelityGateRefused). A refusal is a lost saving, never a
// corrupted prompt.
//
// What is NOT guaranteed:
//
//   - Prose meaning. Lossy compression is allowed to delete
//     prose — whole middle turns, and the middle of long
//     messages. The gate does not check that the surviving prose
//     still says the same thing; that is the lossy contract the
//     client opted into, and it is disclosed (LossyApplied,
//     response header "lossless+lossy").
//   - Fragments shorter than the detection floor. A surviving
//     fragment of a dropped unit shorter than ~8 characters
//     (fewer for very short JSON values) cannot be told apart
//     from ordinary prose, so it is not checked; a cut that
//     leaves only such a stub behind is not caught. Likewise a
//     unit longer than 4 KiB is checked on its lines and its
//     head/tail plus sparse windows, not on every window, so an
//     interior fragment of a very long unit that touches neither
//     end could in principle slip through — in practice the
//     lossy engine cuts a message once, at its middle, so a cut
//     unit always leaves a head or tail fragment, which is
//     checked.
//   - JSON or URLs the extractor cannot see. JSON is found by
//     parsing (a value must parse as complete JSON to count);
//     a JSON-shaped value that does not parse in the original
//     (single-quoted, trailing comma) is prose as far as the
//     gate is concerned, and is still protected by the fence
//     check when it sits inside a fence. URLs are found by
//     pattern (http/https up to whitespace, a quote, or an
//     unbalanced closing delimiter — a closer ends the URL only
//     when no opener of the same kind precedes it inside the
//     URL, so balanced parens/brackets/braces stay part of it);
//     a URL split across lines in the original is not one URL
//     to the gate.
//   - Anything about the lossless steps. The gate runs on the
//     lossy transform only (lossless output in, lossy output
//     out); the lossless equivalence check (VerifyLossless) is
//     a separate gate that runs before it.
package compress

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// CheckLossy verifies that compressed is a structure-preserving
// lossy form of original: whole messages may be gone, plain-text
// messages may be shorter, but no code fence, JSON value, URL,
// identifier or part boundary may be damaged. It returns ok=false
// plus human-readable reasons on the first sign of damage (all
// failing checks are reported, not just the first). It never
// mutates its inputs and performs no token counting.
func CheckLossy(original, compressed []provider.Message) (bool, []string) {
	var reasons []string
	fail := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	if len(original) == 0 && len(compressed) == 0 {
		return true, nil
	}
	if len(original) == 0 {
		fail("lossy output has %d messages but the original had none: fabricated messages", len(compressed))
		return false, reasons
	}
	if len(compressed) == 0 {
		fail("lossy compression dropped every message, including the pinned last message")
		return false, reasons
	}

	// (d) Structure first: order, roles, identifiers, parts.
	checkStructure(original, compressed, &reasons)

	// Text-level units, extracted per message (a fence, JSON
	// value or URL never spans two messages).
	origText := joinMessageTexts(original)
	compText := joinMessageTexts(compressed)

	origFences, origUnclosed := extractAllFences(original)
	compFences, compUnclosed := extractAllFences(compressed)
	origJSON := uniqueStrings(extractJSONValues(origText))
	compJSON := uniqueStrings(extractJSONValues(compText))
	origURLs := uniqueStrings(extractURLs(origText))
	compURLs := uniqueStrings(extractURLs(compText))

	// Surviving units are blanked out of the compressed text
	// (longest first, with equal-length spaces so nothing joins
	// across a blanked unit); whatever remains is the residual,
	// and any fragment of a dropped unit found in it proves the
	// unit survived in part.
	surviving := survivingUnits(origFences, compFences)
	surviving = append(surviving, survivingUnits(origUnclosed, compUnclosed)...)
	surviving = append(surviving, survivingUnits(origJSON, compJSON)...)
	surviving = append(surviving, survivingUnits(origURLs, compURLs)...)
	residual := blankOut(compText, surviving)

	// (a) Code fences.
	compFenceSet := toSet(compFences)
	for _, b := range compFences {
		if !toSet(origFences)[b] {
			fail("code fence in compressed output matches no original fence block (truncated or spliced fence): %s", excerpt(b, 80))
		}
	}
	compUnclosedSet := toSet(compUnclosed)
	for _, u := range compUnclosed {
		if !toSet(origUnclosed)[u] {
			fail("unclosed code fence in compressed output matches no original unclosed fence: %s", excerpt(u, 80))
		}
	}
	for _, b := range origFences {
		if compFenceSet[b] {
			continue
		}
		if frag := firstFragmentIn(fenceFragments(b), residual); frag != "" {
			fail("code fence truncated mid-block (fragment %q survives): %s", frag, excerpt(b, 80))
		}
	}
	for _, u := range origUnclosed {
		if compUnclosedSet[u] {
			continue
		}
		if frag := firstFragmentIn(fenceFragments(u), residual); frag != "" {
			fail("unclosed code fence damaged (fragment %q survives): %s", frag, excerpt(u, 80))
		}
	}

	// (b) JSON values.
	compJSONSet := toSet(compJSON)
	for _, v := range compJSON {
		if !toSet(origJSON)[v] {
			fail("JSON value in compressed output is not an original value (spliced JSON): %s", excerpt(v, 80))
		}
	}
	for _, v := range origJSON {
		if compJSONSet[v] {
			continue
		}
		if frag := firstFragmentIn(jsonFragments(v), residual); frag != "" {
			fail("JSON value cut mid-structure (fragment %q survives): %s", frag, excerpt(v, 80))
		}
	}

	// (c) URLs.
	compURLSet := toSet(compURLs)
	for _, u := range compURLs {
		if !toSet(origURLs)[u] {
			fail("URL in compressed output is not an original URL (spliced URL): %s", excerpt(u, 80))
		}
	}
	for _, u := range origURLs {
		if compURLSet[u] {
			continue
		}
		if frag := firstFragmentIn(urlFragments(u), residual); frag != "" {
			fail("URL cut mid-string (fragment %q survives): %s", frag, excerpt(u, 80))
		}
	}

	return len(reasons) == 0, reasons
}

// messageText returns the checkable text of one message: Content
// when set (it is the concatenation of the text parts when Parts
// is set), otherwise the text parts joined. Using one text per
// message matters: a fence split across two parts must be read
// from the concatenation, not from either part alone.
func messageText(m provider.Message) string {
	if m.Content != "" {
		return m.Content
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// joinMessageTexts joins per-message texts with a separator that
// cannot occur in a unit, so no fence, JSON value or URL can
// match across a message boundary.
func joinMessageTexts(msgs []provider.Message) string {
	texts := make([]string, len(msgs))
	for i, m := range msgs {
		texts[i] = messageText(m)
	}
	return strings.Join(texts, "\n\x00\n")
}

// structuralKey identifies a message's structure: role, name,
// tool ids and part skeleton (types + image URLs). Content is
// deliberately excluded — a plain-text message may legitimately
// be shorter after lossy truncation.
func structuralKey(m provider.Message) string {
	var sb strings.Builder
	sb.WriteString(m.Role)
	sb.WriteByte(0)
	sb.WriteString(m.Name)
	sb.WriteByte(0)
	sb.WriteString(m.ToolCallID)
	sb.WriteByte(0)
	for _, tc := range m.ToolCalls {
		sb.WriteString(tc.ID)
		sb.WriteByte(1)
	}
	sb.WriteByte(0)
	for _, p := range m.Parts {
		sb.WriteString(p.Type)
		sb.WriteByte(1)
		if p.ImageURL != nil {
			sb.WriteString(p.ImageURL.URL)
		}
		sb.WriteByte(2)
	}
	return sb.String()
}

// checkStructure enforces (d): compressed must be a subsequence
// of original under structuralKey (order preserved, nothing
// reordered, fabricated or re-identified), part-carrying messages
// must match byte-identically, and the last message must keep its
// role and identifiers.
func checkStructure(original, compressed []provider.Message, reasons *[]string) {
	j := 0
	for ci, cm := range compressed {
		matched := -1
		for k := j; k < len(original); k++ {
			if structuralKey(original[k]) != structuralKey(cm) {
				continue
			}
			if len(original[k].Parts) > 0 || len(cm.Parts) > 0 {
				if !reflect.DeepEqual(original[k].Parts, cm.Parts) || original[k].Content != cm.Content {
					continue
				}
			}
			matched = k
			break
		}
		if matched < 0 {
			*reasons = append(*reasons, fmt.Sprintf("compressed message %d (role %q, tool_call_id %q) has no matching original message: role, tool-call id or part structure changed", ci, cm.Role, cm.ToolCallID))
			return
		}
		j = matched + 1
	}
	if structuralKey(compressed[len(compressed)-1]) != structuralKey(original[len(original)-1]) {
		*reasons = append(*reasons, "last message changed role or identifiers (role, tool-call id or part structure differ from the original last message)")
	}
}

// extractAllFences extracts fence blocks and unclosed fence tails
// from every message and returns the union.
func extractAllFences(msgs []provider.Message) (blocks []string, unclosed []string) {
	for _, m := range msgs {
		b, u, has := extractFences(messageText(m))
		blocks = append(blocks, b...)
		if has {
			unclosed = append(unclosed, u)
		}
	}
	return uniqueStrings(blocks), uniqueStrings(unclosed)
}

// extractFences returns the closed ``` / ~~~ blocks in s and, if
// the text ends inside a fence, the unclosed tail (opener to end
// of text). A block is the fence lines plus everything between
// them, joined with "\n".
func extractFences(s string) (blocks []string, unclosed string, hasUnclosed bool) {
	lines := strings.Split(s, "\n")
	inFence := false
	var fenceChar byte
	fenceRun := 0
	start := 0
	for i, ln := range lines {
		c, run, rest := fenceLine(ln)
		if !inFence {
			if run >= 3 && !strings.ContainsRune(rest, rune(c)) {
				inFence, fenceChar, fenceRun, start = true, c, run, i
			}
			continue
		}
		if c == fenceChar && run >= fenceRun && strings.TrimSpace(rest) == "" {
			blocks = append(blocks, strings.Join(lines[start:i+1], "\n"))
			inFence = false
		}
	}
	if inFence {
		return blocks, strings.Join(lines[start:], "\n"), true
	}
	return blocks, "", false
}

// fenceLine parses a line as a fence marker: the fence char
// ('`' or '~'), the length of its leading run, and the rest of
// the line after the run (info string on an opener, empty on a
// closer). A line that is not a fence marker returns run 0.
func fenceLine(line string) (char byte, run int, rest string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return 0, 0, ""
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, ""
	}
	run = 0
	for run < len(trimmed) && trimmed[run] == c {
		run++
	}
	return c, run, trimmed[run:]
}

// extractJSONValues returns every complete JSON object/array in s
// (top-level and nested: a successfully parsed value contributes
// itself plus the values nested inside it). A '{' or '[' that
// does not start a complete, parseable value is skipped — prose
// braces and code that merely looks JSON-shaped do not count.
func extractJSONValues(s string) []string {
	var vals []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '{' && c != '[' {
			continue
		}
		var raw json.RawMessage
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		if len(raw) < 2 {
			continue
		}
		vals = append(vals, string(raw))
		// Nested values inside this one count too.
		vals = append(vals, extractJSONValues(string(raw[1:len(raw)-1]))...)
		i += len(raw) - 1
	}
	return vals
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// extractURLs returns the URLs in s. A trailing run of sentence
// punctuation (".", ",", ";", ":", "!", "?") is not part of the
// URL. A closing delimiter (")", "]", "}") is part of the URL
// only when it closes an opener of the same kind earlier in the
// URL (balanced-delimiter rule); a closer with no unmatched
// opener inside the URL ends it — e.g. the ")" closing "(see
// url)" or the ")" of a markdown link "[text](url)" — so a URL
// inside "(see url)" or "[text](url)" is read without it, while
// a Wikipedia-style paren URL keeps its balanced parens.
func extractURLs(s string) []string {
	var out []string
	for _, u := range urlPattern.FindAllString(s, -1) {
		u = trimUnbalancedClosers(u)
		u = strings.TrimRight(u, ".,;:!?")
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// trimUnbalancedClosers cuts u just before the first closing
// delimiter (")", "]", "}") that has no unmatched opener of the
// same kind earlier in u. Openers and closers of different kinds
// do not match each other, and an unmatched opener at the end of
// u is left in place (the URL simply ends there).
func trimUnbalancedClosers(u string) string {
	var paren, bracket, brace int
	for i := 0; i < len(u); i++ {
		switch u[i] {
		case '(':
			paren++
		case ')':
			if paren == 0 {
				return u[:i]
			}
			paren--
		case '[':
			bracket++
		case ']':
			if bracket == 0 {
				return u[:i]
			}
			bracket--
		case '{':
			brace++
		case '}':
			if brace == 0 {
				return u[:i]
			}
			brace--
		}
	}
	return u
}

// survivingUnits returns the units present in both lists (exact
// string equality): the units that survived the lossy step whole.
func survivingUnits(orig, comp []string) []string {
	compSet := toSet(comp)
	var out []string
	for _, u := range uniqueStrings(orig) {
		if compSet[u] {
			out = append(out, u)
		}
	}
	return out
}

// blankOut replaces every occurrence of each unit in s with
// spaces of the same length, longest unit first (so a unit nested
// inside a longer one is blanked as part of the longer one, not
// left as a fragment). What is left is the residual: text the
// lossy step kept that is NOT part of a surviving unit.
func blankOut(s string, units []string) string {
	sorted := append([]string(nil), units...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, u := range sorted {
		if u == "" {
			continue
		}
		s = strings.ReplaceAll(s, u, strings.Repeat(" ", len(u)))
	}
	return s
}

// firstFragmentIn returns the first fragment found in residual,
// or "" when none survives. Fragments are checked longest first
// so the reported fragment is the most informative one.
func firstFragmentIn(frags []string, residual string) string {
	sorted := append([]string(nil), frags...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, f := range sorted {
		if f != "" && strings.Contains(residual, f) {
			return f
		}
	}
	return ""
}

// fenceFragments are the substrings whose survival proves a fence
// block was cut, not dropped: its content lines, its head and
// tail, and — for a block whose whole content is short — the
// content itself.
func fenceFragments(block string) []string {
	lines := strings.Split(block, "\n")
	inner := lines
	if len(lines) >= 2 {
		inner = lines[1 : len(lines)-1]
		// An unclosed tail has no closing fence line.
		if !isFenceCloseLine(lines[len(lines)-1]) {
			inner = lines[1:]
		}
	}
	var frags []string
	var content strings.Builder
	for _, ln := range inner {
		t := strings.TrimSpace(ln)
		if len(t) >= 8 {
			frags = append(frags, t)
		}
		content.WriteString(ln)
		content.WriteByte('\n')
	}
	if len(block) >= 16 {
		frags = append(frags, block[:16], block[len(block)-16:])
	}
	if trimmed := strings.TrimSpace(content.String()); trimmed != "" && len(trimmed) < 16 {
		frags = append(frags, trimmed)
	}
	return frags
}

func isFenceCloseLine(line string) bool {
	c, run, rest := fenceLine(line)
	return run >= 3 && strings.TrimSpace(rest) == "" && c != 0
}

// jsonFragments are the substrings whose survival proves a JSON
// value was cut: its long lines, its head and tail, sliding
// windows over its body (every 8-char window for short values,
// every 12-char window at stride 4 for longer ones), and — for
// a very short value — its opening characters.
func jsonFragments(v string) []string {
	var frags []string
	for _, ln := range strings.Split(v, "\n") {
		if len(strings.TrimSpace(ln)) >= 12 {
			frags = append(frags, strings.TrimSpace(ln))
		}
	}
	if len(v) >= 20 {
		frags = append(frags, v[:20], v[len(v)-20:])
	}
	if len(v) < 16 && len(v) >= 5 {
		frags = append(frags, v[:5])
	}
	frags = append(frags, valueWindows(v)...)
	return frags
}

// urlFragments are the substrings whose survival proves a URL
// was cut: sliding windows over its body (every 8-char window
// for short URLs, every 12-char window at stride 4 for longer
// ones) plus its head and tail.
func urlFragments(u string) []string {
	var frags []string
	if len(u) >= 16 {
		frags = append(frags, u[:16], u[len(u)-16:])
	}
	frags = append(frags, valueWindows(u)...)
	return frags
}

// valueWindows returns sliding windows over s: window 8 at
// stride 1 for values up to 64 chars (catches any surviving
// fragment of 8+ chars), window 12 at stride 4 for longer values
// (catches any surviving fragment of 16+ chars, plus the head
// and tail windows, which the callers also add explicitly). For
// very long values the stride grows so the window count stays
// bounded; a cut by the lossy engine always leaves a head or
// tail fragment, which is checked separately.
func valueWindows(s string) []string {
	size, stride := 8, 1
	if len(s) > 64 {
		size, stride = 12, 4
	}
	if len(s) > 4096 {
		stride = len(s) / 1024
		if stride < 4 {
			stride = 4
		}
	}
	if len(s) < size {
		return nil
	}
	var out []string
	for i := 0; i+size <= len(s); i += stride {
		out = append(out, s[i:i+size])
	}
	last := s[len(s)-size:]
	if len(out) == 0 || out[len(out)-1] != last {
		out = append(out, last)
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}

// excerpt shortens s for a reason string, keeping the start and
// the end (the parts that identify which unit failed).
func excerpt(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	if n < 10 {
		return s[:n]
	}
	half := (n - 3) / 2
	return s[:half] + "..." + s[len(s)-(n-3-half):]
}
