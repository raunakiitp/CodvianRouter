// B4-28 — Ionizer (parity tracker:1082): tabular row sampling,
// LOSSY, stackPriority 13.
//
// A JSON table with more than ionizerMaxRows rows is sampled: the
// first 10 rows and the last 2 are kept, the middle is dropped,
// and a marker line records exactly how many rows were kept out
// of how many — the sampling is visible, never silent. The output
// stays valid JSON (same shape, fewer rows), so downstream
// parsers keep working. This engine drops content, so it is lossy
// and runs only under the lossy opt-in (enforced by the caller —
// see ApplyWithEngines).
//
// Safety: tables with <= ionizerMaxRows rows are untouched; a
// sampled table is emitted with keys in sorted order so the
// output is deterministic; messages with tool calls / parts are
// never rewritten.
package compress

import (
	"strings"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ionizerMaxRows is the largest table Ionizer leaves untouched.
// Above it, ionizerKeepHead + ionizerKeepTail rows are kept.
const ionizerMaxRows = 20

const ionizerKeepHead = 10
const ionizerKeepTail = 2

// SampleTableRows samples the rows of every oversized tabular
// JSON value in the messages.
func SampleTableRows(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if len(m.Parts) > 0 || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			continue
		}
		if m.Content == "" {
			continue
		}
		if next, changed := sampleText(m.Content); changed {
			if strings.TrimSpace(next) == "" || utf8.RuneCountInString(next) < 10 {
				continue
			}
			out[i].Content = next
		}
	}
	return out
}

// sampleText samples the oversized tables in one text, reporting
// whether anything changed.
func sampleText(text string) (string, bool) {
	tables := findJSONTables(text)
	if len(tables) == 0 {
		return text, false
	}
	var b strings.Builder
	pos := 0
	changed := false
	for _, t := range tables {
		if len(t.rows) <= ionizerMaxRows {
			continue
		}
		b.WriteString(text[pos:t.start])
		b.WriteString(renderSampled(t))
		pos = t.end
		changed = true
	}
	if !changed {
		return text, false
	}
	b.WriteString(text[pos:])
	return b.String(), true
}

// renderSampled renders a table with only its head + tail rows,
// plus a marker line stating the kept/total counts.
func renderSampled(t jsonTable) string {
	head := t.rows[:ionizerKeepHead]
	tail := t.rows[len(t.rows)-ionizerKeepTail:]
	var b strings.Builder
	b.WriteByte('[')
	first := true
	for _, row := range append(append([]map[string]any{}, head...), tail...) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(renderRow(row, t.keys))
	}
	b.WriteString("]\n[ionizer: kept ")
	b.WriteString(itoa(ionizerKeepHead + ionizerKeepTail))
	b.WriteString(" of ")
	b.WriteString(itoa(len(t.rows)))
	b.WriteString(" rows]")
	return b.String()
}
