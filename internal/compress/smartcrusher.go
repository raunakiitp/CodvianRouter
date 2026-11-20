// B4-27 — SmartCrusher (parity tracker:1081): Headroom tabular JSON
// → columnar, LOSSLESS, stackPriority 15.
//
// A JSON array of objects with a uniform key set is rewritten in
// columnar form: one header line naming the columns, one line per
// row, values as compact JSON literals. No data is dropped — the
// transform is exactly reversible in principle (same keys, same
// values, only the layout changes) — so this engine is lossless
// and may run without the lossy opt-in.
//
// Safety: a table is skipped (left byte-identical) when any
// rendered value contains the '|' separator or a newline (the
// columnar form could not represent it unambiguously), when the
// table has fewer than 2 rows, or when the keys are not uniform.
// Messages with tool calls / parts are never rewritten, and a
// message that would come out empty reverts to the original.
package compress

import (
	"strings"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// CrushTablesToColumnar rewrites every tabular JSON value in the
// messages (user/assistant/system prose) into columnar form.
func CrushTablesToColumnar(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if len(m.Parts) > 0 || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			continue
		}
		if m.Content == "" {
			continue
		}
		if next, changed := crushText(m.Content); changed {
			if strings.TrimSpace(next) == "" || utf8.RuneCountInString(next) < 10 {
				continue
			}
			out[i].Content = next
		}
	}
	return out
}

// crushText rewrites the tables in one text, reporting whether
// anything changed.
func crushText(text string) (string, bool) {
	tables := findJSONTables(text)
	if len(tables) == 0 {
		return text, false
	}
	var b strings.Builder
	pos := 0
	changed := false
	for _, t := range tables {
		rendered, ok := renderColumnar(t)
		if !ok {
			continue
		}
		b.WriteString(text[pos:t.start])
		b.WriteString(rendered)
		pos = t.end
		changed = true
	}
	if !changed {
		return text, false
	}
	b.WriteString(text[pos:])
	return b.String(), true
}

// renderColumnar renders one table in columnar form, or reports
// false when the table cannot be represented unambiguously.
func renderColumnar(t jsonTable) (string, bool) {
	var b strings.Builder
	b.WriteString("[smartcrusher:columns ")
	b.WriteString(strings.Join(t.keys, "|"))
	b.WriteString("]\n")
	for _, row := range t.rows {
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte('|')
			}
			v := renderValue(row[k])
			if strings.ContainsAny(v, "|\n\r") {
				return "", false
			}
			b.WriteString(v)
		}
		b.WriteByte('\n')
	}
	b.WriteString("[/smartcrusher]")
	return b.String(), true
}
