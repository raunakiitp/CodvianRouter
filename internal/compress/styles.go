// B4-49 — Output styles (parity tracker:1104): Terse prose, Less
// code, Ponytail, "I have ADHD", Terse CJK. Five output styles
// from the sector-typical outputStyles catalog — mutually exclusive
// alternatives: a request picks exactly one style, never the
// whole set. Each is a registry engine (stack priorities 19-23,
// after Relevance 18) and runs only when selected explicitly via
// ApplyWithEngines under the lossy opt-in — every style drops or
// rewrites content, so all five are lossy and the plain Apply
// path never runs any of them.
//
// Safety (same as the other engines): fenced code blocks are
// never rewritten by the prose styles; system messages
// (instructions) are never rewritten; messages with tool calls /
// parts are never rewritten; a message that would come out empty
// or under 10 runes reverts to the original.
package compress

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// styleMessages applies a per-text transform to the user/assistant
// prose messages, with the usual safety guards, reporting the
// messages with the transform applied.
func styleMessages(msgs []provider.Message, fn func(string) (string, bool)) []provider.Message {
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if len(m.Parts) > 0 || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			continue
		}
		if m.Content == "" {
			continue
		}
		if next, changed := fn(m.Content); changed {
			if strings.TrimSpace(next) == "" || utf8.RuneCountInString(next) < 10 {
				continue
			}
			out[i].Content = next
		}
	}
	return out
}

// styleText applies proseFn to the non-fenced segments of text
// and fencedFn to the fenced segments (a nil function passes its
// segments through byte-identical), reporting whether anything
// changed.
func styleText(text string, proseFn, fencedFn func(string) (string, bool)) (string, bool) {
	segs := splitFenced(text)
	var b strings.Builder
	changed := false
	for _, s := range segs {
		fn := proseFn
		if s.fenced {
			fn = fencedFn
		}
		if fn == nil {
			b.WriteString(s.text)
			continue
		}
		next, c := fn(s.text)
		if c {
			changed = true
		}
		b.WriteString(next)
	}
	if !changed {
		return text, false
	}
	return b.String(), true
}

// spaceRun collapses runs of whitespace to a single space.
var spaceRun = regexp.MustCompile(`\s+`)

// terseFiller matches filler words and phrases that carry no
// proposition: intensifiers and throat-clearing. "quite" and
// "rather" are deliberately absent — "quite a few" and "rather
// than" carry meaning, and dropping them would break the
// sentence.
var terseFiller = regexp.MustCompile(`(?i)\b(?:very|really|just|actually|basically|simply|indeed|certainly|definitely|in fact|of course|needless to say|as a matter of fact|in other words|it is important to note that)\b`)

// cleanTerseSpacing removes the filler matched by terseFiller and
// repairs the spacing and punctuation the removal leaves behind.
func cleanTerseSpacing(s string) string {
	s = terseFiller.ReplaceAllString(s, "")
	s = spaceRun.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, " ,", ",")
	s = strings.ReplaceAll(s, " .", ".")
	s = strings.ReplaceAll(s, " ;", ";")
	s = strings.ReplaceAll(s, " :", ":")
	s = strings.ReplaceAll(s, ",,", ",")
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, ",;: ")
	return strings.TrimSpace(s)
}

// StyleTerse drops filler words from prose, leaving minimal-word
// text. Fenced code blocks pass through untouched.
func StyleTerse(msgs []provider.Message) []provider.Message {
	return styleMessages(msgs, func(text string) (string, bool) {
		return styleText(text, terseSegment, nil)
	})
}

// terseSegment drops filler from one non-fenced segment.
func terseSegment(text string) (string, bool) {
	next := cleanTerseSpacing(text)
	if next == text {
		return text, false
	}
	return next, true
}

// StyleLessCode drops blank lines and full-line comments from
// fenced code blocks, leaving fewer code tokens. Prose passes
// through untouched.
func StyleLessCode(msgs []provider.Message) []provider.Message {
	return styleMessages(msgs, func(text string) (string, bool) {
		return styleText(text, nil, lessCodeSegment)
	})
}

// lessCodeSegment drops blank lines and full-line comments from
// one fenced code block. The fence markers stay byte-identical;
// a block with no room for code between the fences is untouched.
func lessCodeSegment(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		return text, false
	}
	out := make([]string, 0, len(lines))
	changed := false
	for i, ln := range lines {
		if i == 0 || i == len(lines)-1 {
			out = append(out, ln)
			continue
		}
		trimmed := strings.TrimRight(ln, " \t\r")
		if trimmed != ln {
			changed = true
		}
		if strings.TrimSpace(trimmed) == "" {
			changed = true
			continue
		}
		lead := strings.TrimLeft(trimmed, " \t")
		if strings.HasPrefix(lead, "//") || strings.HasPrefix(lead, "#") || strings.HasPrefix(lead, "--") {
			changed = true
			continue
		}
		out = append(out, trimmed)
	}
	if !changed {
		return text, false
	}
	return strings.Join(out, "\n"), true
}

// StylePonytail keeps the first and last sentences of each prose
// stretch and drops the middle — a short summary tail. Fenced
// code blocks pass through untouched.
func StylePonytail(msgs []provider.Message) []provider.Message {
	return styleMessages(msgs, func(text string) (string, bool) {
		return styleText(text, ponytailSegment, nil)
	})
}

// ponytailSegment keeps the first and last sentences of one
// non-fenced segment. Fewer than three sentences means there is
// no middle to drop.
func ponytailSegment(text string) (string, bool) {
	sentences := splitSentences(text)
	if len(sentences) < 3 {
		return text, false
	}
	next := sentences[0] + " " + sentences[len(sentences)-1]
	if next == strings.TrimSpace(text) {
		return text, false
	}
	return next, true
}

// StyleADHD renders prose as a very short scannable form: at most
// the first three sentences, each on its own bullet line, with
// filler dropped. Fenced code blocks pass through untouched.
func StyleADHD(msgs []provider.Message) []provider.Message {
	return styleMessages(msgs, func(text string) (string, bool) {
		return styleText(text, adhdSegment, nil)
	})
}

// listItemPrefix matches a numbered list item ("1. " / "1) ")
// so it is not given a second bullet.
var listItemPrefix = regexp.MustCompile(`^\d+[.)]\s`)

// adhdSegment keeps at most three sentences of one non-fenced
// segment, one per bullet line. Sentences that already start a
// bullet or a numbered item keep their marker.
func adhdSegment(text string) (string, bool) {
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return text, false
	}
	keep := sentences
	if len(keep) > 3 {
		keep = keep[:3]
	}
	var b strings.Builder
	for i, s := range keep {
		s = cleanTerseSpacing(s)
		if !strings.HasPrefix(s, "- ") && !strings.HasPrefix(s, "* ") && !listItemPrefix.MatchString(s) {
			s = "- " + s
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	next := b.String()
	if next == strings.TrimSpace(text) {
		return text, false
	}
	return next, true
}

// splitSentences splits text into its sentences (headers and list
// items count as fragments), dropping whitespace-only pieces.
func splitSentences(text string) []string {
	raw := sentenceSplitter.FindAllString(text, -1)
	sentences := make([]string, 0, len(raw))
	for _, s := range raw {
		if strings.TrimSpace(s) == "" {
			continue
		}
		sentences = append(sentences, strings.TrimSpace(s))
	}
	return sentences
}

// cjkFillers are filler phrases dropped by the Terse CJK style.
// Each is a whole word or phrase: 其实 (actually), 实际上 (in
// fact), 非常 (very), 真的 (really), 只是 (just), 基本上
// (basically), 简单来说 (simply put), 当然 (of course), 说实话 /
// 老实说 / 坦白说 (to be honest), 众所周知 (as everyone knows).
var cjkFillers = []string{
	"其实", "实际上", "非常", "真的", "只是", "基本上",
	"简单来说", "当然", "说实话", "老实说", "坦白说", "众所周知",
}

// isCJK reports whether r is a CJK ideograph, hiragana,
// katakana or hangul rune.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x3040 && r <= 0x30FF,
		r >= 0xAC00 && r <= 0xD7AF:
		return true
	}
	return false
}

// hasCJK reports whether s contains any CJK rune.
func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// StyleTerseCJK drops CJK filler phrases (and Latin filler in
// mixed text), leaving terse form for CJK text. Text with no CJK
// runes is untouched — the plain Terse style covers that.
// Fenced code blocks pass through untouched.
func StyleTerseCJK(msgs []provider.Message) []provider.Message {
	return styleMessages(msgs, func(text string) (string, bool) {
		return styleText(text, terseCJKSegment, nil)
	})
}

// terseCJKSegment drops CJK filler from one non-fenced segment.
func terseCJKSegment(text string) (string, bool) {
	if !hasCJK(text) {
		return text, false
	}
	next := text
	for _, f := range cjkFillers {
		next = strings.ReplaceAll(next, f, "")
	}
	next = cleanTerseSpacing(next)
	if next == text {
		return text, false
	}
	return next, true
}
