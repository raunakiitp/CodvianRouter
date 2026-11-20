// Shared tabular-JSON detection for the SmartCrusher (B4-27) and
// Ionizer (B4-28) engines.
//
// A "table" is a JSON array of >= 2 objects with an identical key
// set. Detection never fails a request: anything unparseable,
// non-uniform, or too small is simply not a table and is left
// untouched. Detection is deterministic: keys are sorted, so the
// column order never depends on map iteration order.
package compress

import (
	"encoding/json"
	"sort"
	"strings"
)

// jsonTable is one detected tabular JSON value plus the span it
// occupies in the source text (including any surrounding fence,
// so a replacement drops the fence markers with it).
type jsonTable struct {
	start, end int
	keys       []string
	rows       []map[string]any
}

// findJSONTables scans text for tabular JSON values: whole fenced
// blocks whose content parses as a uniform array of objects, and
// bare arrays found by decoding one JSON value at each '['.
func findJSONTables(text string) []jsonTable {
	var out []jsonTable
	// Track every fenced region (whether or not it parses as a
	// table): the bare-array pass must not reach inside a fence
	// and replace an inner array while leaving the fence behind.
	var fences [][2]int
	// First pass: fenced regions (``` ... ```). A fenced region
	// whose whole content is one table is reported as a single
	// table spanning the fences.
	rest := text
	off := 0
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			break
		}
		// The fence opening line ends at the next newline.
		lineEnd := strings.Index(rest[open:], "\n")
		if lineEnd < 0 {
			break
		}
		bodyStart := open + lineEnd + 1
		closeIdx := strings.Index(rest[bodyStart:], "\n```")
		if closeIdx < 0 {
			break
		}
		bodyEnd := bodyStart + closeIdx
		fenceEnd := off + bodyEnd + len("\n```")
		fences = append(fences, [2]int{off + open, fenceEnd})
		if t, ok := parseJSONTable(strings.TrimSpace(rest[bodyStart:bodyEnd])); ok {
			t.start = off + open
			t.end = fenceEnd
			out = append(out, t)
		}
		rest = rest[fenceEnd-off:]
		off = fenceEnd
	}
	// Second pass: bare arrays outside fenced regions. Re-scan
	// the original text but skip spans inside a fence or already
	// claimed by a fenced table.
	for i := 0; i < len(text); i++ {
		if text[i] != '[' {
			continue
		}
		insideFence := false
		for _, f := range fences {
			if i >= f[0] && i < f[1] {
				insideFence = true
				break
			}
		}
		if insideFence {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var v any
		if err := dec.Decode(&v); err != nil {
			continue
		}
		end := i + int(dec.InputOffset())
		rows, ok := uniformRows(v)
		if !ok {
			continue
		}
		out = append(out, jsonTable{start: i, end: end, keys: sortedKeys(rows), rows: rows})
		i = end - 1
	}
	// Report in source order.
	sort.Slice(out, func(a, b int) bool { return out[a].start < out[b].start })
	return out
}

// parseJSONTable decodes s as a JSON value and reports it as a
// table when it is an array of >= 2 objects with a uniform key set.
func parseJSONTable(s string) (jsonTable, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return jsonTable{}, false
	}
	rows, ok := uniformRows(v)
	if !ok {
		return jsonTable{}, false
	}
	return jsonTable{keys: sortedKeys(rows), rows: rows}, true
}

// uniformRows reports v as rows when it is an array of >= 2 JSON
// objects sharing exactly the same key set.
func uniformRows(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) < 2 {
		return nil, false
	}
	rows := make([]map[string]any, 0, len(arr))
	var keys map[string]bool
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, false
		}
		if keys == nil {
			keys = map[string]bool{}
			for k := range m {
				keys[k] = true
			}
		} else {
			if len(m) != len(keys) {
				return nil, false
			}
			for k := range m {
				if !keys[k] {
					return nil, false
				}
			}
		}
		rows = append(rows, m)
	}
	return rows, true
}

// sortedKeys returns the shared key set of rows in sorted order.
func sortedKeys(rows []map[string]any) []string {
	keys := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// renderRow renders one row as compact JSON with keys in sorted
// order (encoding/json on a map would randomise the order).
func renderRow(row map[string]any, keys []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, _ := json.Marshal(row[k])
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// renderValue renders one cell as compact JSON.
func renderValue(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
