// Package compress — Lite tool-result truncation cap (G22,
// parity tracker Area 10 row :848).
//
// What the sector baseline does (study only, never copied —
// the earlier implementation, compressToolResults /
// truncateToolText):
//
//   - Tool-result strings over a cap (default 2000 chars, usable
//     range 256–1,000,000) are truncated. A multi-line result
//     keeps its tail as well as its head — a head-only cut deletes
//     the lines an agent is about to edit, so the model rebuilds
//     them from memory and the edit is rejected as stale. The
//     head is cut on a word boundary (look back up to 80 chars,
//     else forward, else the hard cut); the tail starts on a line
//     boundary inside its budget. Content with 10 lines or fewer
//     keeps the head-only cut. The marker is "\n...[truncated]".
//   - Results of the CURRENT turn — tool messages after the last
//     assistant message — are never cut: cutting them makes the
//     model re-read the same file forever, since every re-read is
//     cut again.
//
// What exists here, and how it differs (deliberate divergences):
//
//   - Opt-in only, no default cap. the sector baseline caps whenever Lite
//     runs; here the cap applies only when the client sets
//     tool_result_max_chars on the request (or the caller passes
//     ToolResultMaxChars on ApplyOptions). 0 / unset means no
//     cap, and an unset cap changes nothing — the Lite steps
//     keep their previous behaviour exactly.
//   - The cap is in RUNES, not JS string units: the same text
//     measures the same on both sides of the boundary for
//     non-ASCII content.
//   - Out-of-range caps are clamped into the sector-typical usable
//     range [256, 1,000,000] (effectiveToolCap in
//     apply_options.go) instead of falling back to a default
//     cap: falling back would invent a cap the client did not
//     ask for.
//   - Only role:"tool" messages are capped. the sector baseline also caps
//     tool_result blocks inside role:"user" messages (Anthropic
//     wire shape); this router's canonical messages carry tool
//     results as role:"tool", so there is no second shape to
//     cover. The Responses-API trailing-output exemption has no
//     canonical-message equivalent here either, so only the
//     last-assistant rule applies.
//   - A part-carrying tool message is capped per text part
//     (each text part independently capped, Content rebuilt as
//     their concatenation — the same rebuild stripWhitespaceAll
//     performs), because parts are the message's text. A cap
//     therefore bounds every part, not the joined total.
package compress

import (
	"math"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// SmartFilterText is the MCP-accessibility text filter for tool
// results (B4-38): ANSI escape sequences go (a screen reader
// cannot read them), runs of identical consecutive lines collapse
// to one (repeated log lines are noise to a listener), and
// redundant whitespace is canonicalised. It is idempotent and
// never touches code fences' bytes beyond the whitespace rules
// stripRedundantWhitespace already applies. Exported so the MCP
// lane can filter tool results at the source; the Lite cap path
// below applies it before capping.
func SmartFilterText(s string) string {
	if s == "" {
		return s
	}
	out := stripANSI(s)
	if c, dup := collapseDuplicateLines(out); dup {
		out = c
	}
	return stripRedundantWhitespace(out)
}

// toolCapHintMarker names the omitted count in a truncation hint:
// the model can see HOW MUCH was cut and re-fetch it, instead of
// guessing from a bare "[truncated]".
func toolCapHintMarker(omitted int) string {
	return "\n...[truncated: " + intString(omitted) + " chars omitted]"
}

// toolCapMarker is appended to a truncated tool result, marking
// the cut so the model can see text was removed. Mirrors
// the sector-typical TRUNCATION_MARKER. It is the fallback marker for
// caps too small to fit a hint; the head-only cut uses the
// count-carrying hint above.
const toolCapMarker = "\n...[truncated]"

// toolCapElisionReserve is room for the middle elision marker
// ("... [N lines elided] ..."), whatever its line count.
// Mirrors the sector-typical ELISION_RESERVE.
const toolCapElisionReserve = 32

// toolCapTailShare is the share of the truncation budget the
// tail keeps on multi-line content. Mirrors the sector-typical
// TAIL_SHARE.
const toolCapTailShare = 0.3

// toolCapMinLinesForTail is the line count above which a
// truncated result keeps a tail. Mirrors the sector-typical
// MIN_LINES_FOR_TAIL.
const toolCapMinLinesForTail = 10

// toolCapLookback is how far the word-boundary search looks back
// (then forward) from a hard cut. Mirrors the sector-typical
// TOOL_TRUNCATION_LOOKBACK.
const toolCapLookback = 80

// toolCapBounds are the sector-typical usable cap range
// (isUsableLiteMaxToolLength): below the floor or above the
// ceiling the value is clamped, never silently replaced by a
// default.
const (
	toolCapMin = 256
	toolCapMax = 1000000
)

// truncateToolResultsToCap truncates over-cap tool-role messages
// to maxChars runes, except tool messages of the current turn
// (at/after the last assistant message — see the package doc).
// It returns the result and whether anything changed. The input
// slice is never mutated. maxChars <= 0 means "no cap" and
// returns the input unchanged.
func truncateToolResultsToCap(msgs []provider.Message, maxChars int) ([]provider.Message, bool) {
	if maxChars <= 0 || len(msgs) == 0 {
		return msgs, false
	}
	// Current-turn start: the message after the last assistant
	// message (or len(msgs) when there is none — every tool
	// message is then an older turn's and may be capped).
	currentTurnStart := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			currentTurnStart = i + 1
			break
		}
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	changed := false
	for i, m := range out {
		if m.Role != "tool" || i >= currentTurnStart {
			continue
		}
		capped, did := capToolMessage(m, maxChars)
		if did {
			out[i] = capped
			changed = true
		}
	}
	return out, changed
}

// capToolMessage caps one tool message's text to maxChars runes.
// The text is accessibility-filtered first (SmartFilterText:
// ANSI strip, duplicate-line collapse, whitespace canonicalise),
// so a capped result is readable as well as bounded. Part-carrying
// messages are capped per text part; Content-only messages are
// capped as a whole. The message is copied, never mutated in place.
func capToolMessage(m provider.Message, maxChars int) (provider.Message, bool) {
	if len(m.Parts) > 0 {
		parts := make([]provider.ContentPart, len(m.Parts))
		copy(parts, m.Parts)
		var sb strings.Builder
		changed := false
		for i, p := range parts {
			if p.Type == "text" {
				filtered := SmartFilterText(p.Text)
				if filtered != p.Text {
					parts[i].Text = filtered
					changed = true
				}
				if capped := truncateToolText(parts[i].Text, maxChars); capped != parts[i].Text {
					parts[i].Text = capped
					changed = true
				}
				sb.WriteString(parts[i].Text)
			}
		}
		if !changed {
			return m, false
		}
		m.Parts = parts
		m.Content = sb.String()
		return m, true
	}
	orig := m.Content
	filtered := SmartFilterText(orig)
	if capped := truncateToolText(filtered, maxChars); capped != filtered {
		m.Content = capped
		return m, true
	}
	if filtered != orig {
		m.Content = filtered
		return m, true
	}
	return m, false
}

// truncateToolText truncates text to at most maxChars runes,
// mirroring the sector-typical truncateToolText: a multi-line result
// over 10 lines keeps head + tail with an elision marker; a
// short or single-line result keeps a head-only cut on a word
// boundary. Text already within the cap is returned unchanged.
// The result is always <= maxChars runes when the marker fits;
// when maxChars is smaller than the marker itself the marker
// alone is returned (a cap that small is clamped to 256 by
// effectiveToolCap before it reaches here).
func truncateToolText(text string, maxChars int) string {
	r := []rune(text)
	if len(r) <= maxChars {
		return text
	}
	marker := []rune(toolCapMarker)
	if maxChars <= len(marker) {
		return string(marker[:maxChars])
	}
	// Multi-line content keeps its tail as well as its head.
	if budget := maxChars - len(marker) - toolCapElisionReserve; budget > 0 && countLines(r) > toolCapMinLinesForTail {
		headCut := backOffToWordBoundary(r, int(math.Ceil(float64(budget)*(1-toolCapTailShare))))
		// The head must not end in whitespace: the marker
		// starts with "\n", so a trailing space would be
		// stripped by the lossless whitespace step on a
		// re-normalise and break the cap's idempotence (the
		// equivalence check would refuse the capped output).
		headCut = trimTrailingSpaceRunes(r, headCut)
		if tailStart := findTailStart(r, budget-headCut); tailStart > headCut {
			// Lines elided: the sector baseline counts the split length
			// (newlines + 1) of the elided span.
			elided := strings.Count(string(r[headCut:tailStart]), "\n") + 1
			var sb strings.Builder
			sb.WriteString(string(r[:headCut]))
			sb.WriteString("\n... [")
			sb.WriteString(intString(elided))
			sb.WriteString(" lines elided] ...\n")
			sb.WriteString(string(r[tailStart:]))
			sb.WriteString(toolCapMarker)
			if out := []rune(sb.String()); len(out) <= maxChars {
				return sb.String()
			}
			// The elision marker itself pushed the result over
			// the cap (a huge line count): fall through to the
			// head-only cut rather than ship an over-cap result.
		}
	}
	// Head-only cut with a truncation hint (B4-38): the marker
	// names how many chars were omitted, so the model can
	// re-fetch them instead of guessing from a bare
	// "[truncated]". The hint length depends on the omitted
	// count, which depends on the cut — resolve with one re-cut,
	// then trim until the result fits the cap (the digit count
	// can shift by one between passes).
	cut := backOffToWordBoundary(r, maxChars-len(marker))
	cut = trimTrailingSpaceRunes(r, cut)
	hint := toolCapHintMarker(len(r) - cut)
	if len([]rune(hint)) > len(marker) {
		cut = backOffToWordBoundary(r, maxChars-len([]rune(hint)))
		cut = trimTrailingSpaceRunes(r, cut)
		hint = toolCapHintMarker(len(r) - cut)
	}
	out := string(r[:cut]) + hint
	for len([]rune(out)) > maxChars && cut > 0 {
		cut--
		cut = trimTrailingSpaceRunes(r, cut)
		hint = toolCapHintMarker(len(r) - cut)
		out = string(r[:cut]) + hint
	}
	return out
}

// trimTrailingSpaceRunes backs cut off trailing spaces/tabs so
// a truncated head never ends in whitespace (the marker starts
// with "\n", and the lossless whitespace step would strip such
// a space on a re-normalise, breaking idempotence).
func trimTrailingSpaceRunes(r []rune, cut int) int {
	for cut > 0 && (r[cut-1] == ' ' || r[cut-1] == '\t') {
		cut--
	}
	return cut
}

// countLines reports how many lines r holds (newline count + 1
// for non-empty text), without splitting the payload.
func countLines(r []rune) int {
	if len(r) == 0 {
		return 0
	}
	n := 1
	for _, c := range r {
		if c == '\n' {
			n++
		}
	}
	return n
}

// findTailStart returns the start of the first line that begins
// inside the last tailBudget runes of r — the tail never exceeds
// its budget — or -1 when no complete line fits.
func findTailStart(r []rune, tailBudget int) int {
	if tailBudget <= 0 || len(r) == 0 {
		return -1
	}
	start := len(r) - tailBudget
	if start < 0 {
		start = 0
	}
	for i := start; i < len(r); i++ {
		if r[i] == '\n' && i+1 < len(r) {
			return i + 1
		}
	}
	return -1
}

// backOffToWordBoundary adjusts a hard cut index to the nearest
// whitespace within a small lookback/lookahead window, so a
// truncated tool result never garbles the word it lands in the
// middle of. It prefers backing off to the end of the previous
// word (keeps the result at or under the limit); if no
// whitespace precedes the cut within the window, it looks
// forward to complete the current word instead; it falls back
// to the hard cut when neither direction finds a boundary.
func backOffToWordBoundary(r []rune, cutIndex int) int {
	if cutIndex < 0 {
		return 0
	}
	if cutIndex > len(r) {
		return len(r)
	}
	if cutIndex == 0 || cutIndex == len(r) {
		return cutIndex
	}
	isWord := func(c rune) bool { return c != ' ' && c != '\t' && c != '\n' && c != '\r' }
	if !isWord(r[cutIndex-1]) || !isWord(r[cutIndex]) {
		return cutIndex
	}
	start := cutIndex - toolCapLookback
	if start < 0 {
		start = 0
	}
	for i := cutIndex; i > start; i-- {
		if !isWord(r[i-1]) {
			return i - 1
		}
	}
	end := cutIndex + toolCapLookback
	if end > len(r) {
		end = len(r)
	}
	for i := cutIndex; i < end; i++ {
		if !isWord(r[i]) {
			return i
		}
	}
	return cutIndex
}

// intString formats a small non-negative integer without
// importing strconv into this file's scope for one call.
func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
