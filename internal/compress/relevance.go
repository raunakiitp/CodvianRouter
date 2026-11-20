// B4-29 — Relevance (parity tracker:1083): extractive sentence
// scoring, LOSSY, stackPriority 18, preserve-regex.
//
// Sentences are scored by term frequency across the message:
// a word that recurs in several sentences marks the sentences
// containing it as more central (classic extractive scoring).
// The first and last sentences are always kept (position bias),
// sentences matching the preserve regex are always kept, and
// the top half of the rest by score is kept; the rest is dropped.
// This engine drops content, so it is lossy and runs only under
// the lossy opt-in (enforced by the caller — see
// ApplyWithEngines).
//
// Safety: fenced code blocks are never touched; system messages
// (instructions) are never touched; messages with fewer than 4
// sentences are untouched; messages with tool calls / parts are
// never rewritten; a message that would come out empty reverts
// to the original.
package compress

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// sentenceSplitter finds sentences: a run of non-terminator
// characters ending in . ! or ? (with trailing quotes/parens),
// or a terminator-less fragment (headers, list items).
var sentenceSplitter = regexp.MustCompile(`[^.!?\n]+[.!?]+["')\]]*|[^.!?\n]+`)

// wordSplitter finds words for scoring (letters and digits).
var wordSplitter = regexp.MustCompile(`[\p{L}\p{N}]+`)

// relevanceStopwords are ignored when scoring — they carry no
// topical signal.
var relevanceStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "to": true,
	"of": true, "in": true, "on": true, "for": true, "with": true,
	"and": true, "or": true, "but": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true,
	"as": true, "at": true, "by": true, "from": true, "not": true,
	"no": true, "do": true, "does": true, "did": true, "have": true,
	"has": true, "had": true, "will": true, "would": true, "can": true,
	"could": true, "should": true, "i": true, "you": true, "he": true,
	"she": true, "we": true, "they": true, "my": true, "your": true,
	"our": true, "their": true, "me": true, "him": true, "her": true,
	"us": true, "them": true, "so": true, "if": true, "then": true,
	"than": true, "when": true, "where": true, "which": true,
	"who": true, "whom": true, "what": true, "how": true, "why": true,
	"all": true, "any": true, "some": true, "more": true, "most": true,
	"other": true, "into": true, "over": true, "under": true, "up": true,
	"down": true, "out": true, "about": true, "between": true,
	"through": true, "after": true, "before": true, "during": true,
	"per": true, "via": true, "am": true,
}

// ScoreSentences applies extractive sentence scoring to the
// user/assistant messages, keeping the most central sentences.
// preserve is the preserve-regex: matching sentences are always
// kept. Nil means no preserve filter.
func ScoreSentences(msgs []provider.Message, preserve *regexp.Regexp) []provider.Message {
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
		if next, changed := scoreText(m.Content, preserve); changed {
			if strings.TrimSpace(next) == "" || utf8.RuneCountInString(next) < 10 {
				continue
			}
			out[i].Content = next
		}
	}
	return out
}

// scoreText scores the sentences in one text (fenced blocks pass
// through untouched), reporting whether anything was dropped.
func scoreText(text string, preserve *regexp.Regexp) (string, bool) {
	segs := splitFenced(text)
	var b strings.Builder
	changed := false
	for _, s := range segs {
		if s.fenced {
			b.WriteString(s.text)
			continue
		}
		next, c := scoreSegment(s.text, preserve)
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

// fencedSegment is one stretch of text, flagged when it is inside
// a ``` fence.
type fencedSegment struct {
	text   string
	fenced bool
}

// splitFenced splits text at ``` fence boundaries. The fence
// markers stay attached to the fenced segment.
func splitFenced(text string) []fencedSegment {
	var out []fencedSegment
	rest := text
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			break
		}
		lineEnd := strings.Index(rest[open:], "\n")
		if lineEnd < 0 {
			break
		}
		bodyStart := open + lineEnd + 1
		closeIdx := strings.Index(rest[bodyStart:], "\n```")
		if closeIdx < 0 {
			break
		}
		closeEnd := bodyStart + closeIdx + len("\n```")
		if open > 0 {
			out = append(out, fencedSegment{text: rest[:open]})
		}
		out = append(out, fencedSegment{text: rest[open:closeEnd], fenced: true})
		rest = rest[closeEnd:]
	}
	if rest != "" {
		out = append(out, fencedSegment{text: rest})
	}
	return out
}

// scoreSegment drops the lowest-scoring sentences of one
// non-fenced segment, reporting whether anything was dropped.
func scoreSegment(text string, preserve *regexp.Regexp) (string, bool) {
	raw := sentenceSplitter.FindAllString(text, -1)
	// Drop empty fragments (whitespace-only) but remember that
	// the text may be nothing but separators.
	sentences := make([]string, 0, len(raw))
	for _, s := range raw {
		if strings.TrimSpace(s) == "" {
			continue
		}
		sentences = append(sentences, s)
	}
	if len(sentences) < 4 {
		return text, false
	}
	// Document frequency: in how many sentences each word occurs.
	df := map[string]int{}
	wordsPer := make([][]string, len(sentences))
	for i, s := range sentences {
		seen := map[string]bool{}
		for _, w := range wordSplitter.FindAllString(strings.ToLower(s), -1) {
			if relevanceStopwords[w] {
				continue
			}
			if !seen[w] {
				seen[w] = true
				df[w]++
			}
			wordsPer[i] = append(wordsPer[i], w)
		}
	}
	scores := make([]float64, len(sentences))
	for i := range sentences {
		sum := 0
		for _, w := range wordsPer[i] {
			sum += df[w]
		}
		if len(wordsPer[i]) > 0 {
			scores[i] = float64(sum) / math.Sqrt(float64(len(wordsPer[i])))
		}
	}
	// Keep first + last, preserve matches, and the top half of
	// the rest by score (ties keep the earlier sentence).
	keep := map[int]bool{0: true, len(sentences) - 1: true}
	for i, s := range sentences {
		if preserve != nil && preserve.MatchString(s) {
			keep[i] = true
		}
	}
	mid := make([]int, 0, len(sentences))
	for i := range sentences {
		if !keep[i] {
			mid = append(mid, i)
		}
	}
	keepCount := (len(mid) + 1) / 2 // ceil of half the rest
	sort.Slice(mid, func(a, b int) bool {
		if scores[mid[a]] != scores[mid[b]] {
			return scores[mid[a]] > scores[mid[b]]
		}
		return mid[a] < mid[b]
	})
	for _, i := range mid[:keepCount] {
		keep[i] = true
	}
	if len(keep) == len(sentences) {
		return text, false
	}
	kept := make([]string, 0, len(keep))
	for i, s := range sentences {
		if keep[i] {
			kept = append(kept, strings.TrimSpace(s))
		}
	}
	return strings.Join(kept, " "), true
}
