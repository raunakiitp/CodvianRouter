// B4-33 Ultra heuristic pruning (Wave 12, Stage 1).
//
// PruneHeuristic is the score-based token pruning engine for the
// ultra mode: it spends its cuts where they cost the least meaning.
// Three named pieces, in order:
//
//   - Code thinning: inside fenced code blocks of droppable
//     messages, comment-only lines (// in C-like fences, # in
//     fences tagged a #-comment language) and blank-line runs go.
//     Comments are the lowest-value bytes in a code dump, and the
//     code itself is never touched.
//   - Binary-search truncation: the longest plain-text messages are
//     cut head+tail, with the keep fraction found by binary search
//     on the measured token count — the largest fraction that still
//     removes the needed tokens, never a blind proportional cut.
//   - Score-based dropping: if that still does not fit, whole
//     droppable messages go oldest-first (age is the score:
//     the oldest turn is worth the least to the next reply).
//
// Pinned messages (system/developer, last user, last message) are
// never pruned — the same pinning applyLossy uses. A counting
// failure fails closed: the input is returned unchanged.
package compress

import (
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// pruneMinKeepFrac is the smallest fraction of a message the
// binary-search truncation ever keeps: a message is never cut to
// nothing — at least ~5% (and the truncateMiddle floor of 2 runes)
// survives, or the message is dropped whole instead.
const pruneMinKeepFrac = 0.05

// pruneProbes caps the binary-search probes per message: 12 probes
// resolve the keep fraction to well under a token of precision on
// any realistic message, and the loop always terminates.
const pruneProbes = 12

// PruneHeuristic prunes msgs toward budget and returns the pruned
// messages plus how many were dropped whole / had content cut.
// budget <= 0 means "no target": the input is returned unchanged.
func PruneHeuristic(model string, msgs []provider.Message, budget int) ([]provider.Message, int, int) {
	if budget <= 0 || len(msgs) == 0 {
		return msgs, 0, 0
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	pinned := pinnedIndices(msgs)
	counts, total, err := countEach(model, out)
	if err != nil {
		return msgs, 0, 0
	}

	// 1. Code thinning on droppable plain-text messages.
	thinned := 0
	for i, m := range out {
		if pinned[i] || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		if t := thinCodeBlocks(m.Content); t != m.Content {
			out[i].Content = t
			thinned++
		}
	}
	if thinned > 0 {
		counts, total, err = countEach(model, out)
		if err != nil {
			return msgs, 0, 0
		}
	}
	if total <= budget {
		return out, 0, thinned
	}

	// 2. Binary-search truncation, oldest first (age is the
	// score: the oldest turn is worth the least). Within one
	// turn the largest message goes first.
	order := pruneOrder(msgs, counts, pinned)
	truncated := thinned
	for _, i := range order {
		if total <= budget {
			break
		}
		if len(out[i].Parts) > 0 || out[i].Content == "" {
			continue
		}
		need := total - budget
		if counts[i] <= 10 {
			continue
		}
		maxRemove := counts[i] - 10
		if need > maxRemove {
			need = maxRemove
		}
		frac, removed := binarySearchKeepFrac(model, out[i], need)
		if removed <= 0 {
			continue
		}
		out[i].Content = truncateMiddle(out[i].Content, frac)
		truncated++
		c, err := tokencount.CountPrompt(model, []provider.Message{out[i]})
		if err != nil {
			return msgs, 0, 0
		}
		total -= counts[i] - c.Tokens
		counts[i] = c.Tokens
	}
	if total <= budget {
		return out, 0, truncated
	}

	// 3. Drop whole droppable messages, oldest first.
	dropped := 0
	droppedSet := make([]bool, len(out))
	for _, i := range order {
		if total <= budget {
			break
		}
		if pinned[i] {
			continue
		}
		total -= counts[i]
		counts[i] = 0
		droppedSet[i] = true
		dropped++
	}
	kept := make([]provider.Message, 0, len(out))
	for i, m := range out {
		if !droppedSet[i] {
			kept = append(kept, m)
		}
	}
	return kept, dropped, truncated
}

// pinnedIndices marks the messages applyLossy never drops:
// system/developer, the last user message, and the last message
// of any role. Shared pinning, one definition.
func pinnedIndices(msgs []provider.Message) []bool {
	pinned := make([]bool, len(msgs))
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			pinned[i] = true
		}
		if m.Role == "user" {
			lastUser = i
		}
	}
	if lastUser >= 0 {
		pinned[lastUser] = true
	}
	if len(msgs) > 0 {
		pinned[len(msgs)-1] = true
	}
	return pinned
}

// countEach counts every message individually and returns the
// per-message counts and their sum. A counting failure is an
// error (callers fail closed on it).
func countEach(model string, msgs []provider.Message) ([]int, int, error) {
	counts := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		c, err := tokencount.CountPrompt(model, []provider.Message{m})
		if err != nil {
			return nil, 0, err
		}
		counts[i] = c.Tokens
		total += c.Tokens
	}
	return counts, total, nil
}

// pruneOrder ranks droppable messages for pruning: oldest first
// (age = turns from the end; the oldest turn scores lowest),
// largest first within one turn.
func pruneOrder(msgs []provider.Message, counts []int, pinned []bool) []int {
	// age[i] = number of non-system messages after i.
	age := make([]int, len(msgs))
	after := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		age[i] = after
		if msgs[i].Role != "system" && msgs[i].Role != "developer" {
			after++
		}
	}
	var order []int
	for i := range msgs {
		if !pinned[i] {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if age[ia] != age[ib] {
			return age[ia] > age[ib]
		}
		return counts[ia] > counts[ib]
	})
	return order
}

// binarySearchKeepFrac finds the largest keep fraction in
// [pruneMinKeepFrac, 1) whose truncation of m removes at least
// need tokens, probing the measured token count. It returns the
// fraction and the tokens actually removed. When no fraction can
// remove the full need, it returns the fraction that removes the
// most (partial progress is still progress — the caller moves on
// to the next message). It returns (1, 0) only when no cut
// removes anything (marker accounting): the caller then skips the
// message instead of burning a truncation marker for nothing.
func binarySearchKeepFrac(model string, m provider.Message, need int) (float64, int) {
	orig, err := tokencount.CountPrompt(model, []provider.Message{m})
	if err != nil {
		return 1, 0
	}
	lo, hi := pruneMinKeepFrac, 1.0
	meetFrac, meetRemoved := -1.0, 0
	maxFrac, maxRemoved := 1.0, 0
	for p := 0; p < pruneProbes; p++ {
		mid := (lo + hi) / 2
		if mid >= 1 {
			break
		}
		cand := m
		cand.Content = truncateMiddle(m.Content, mid)
		c, err := tokencount.CountPrompt(model, []provider.Message{cand})
		if err != nil {
			return 1, 0
		}
		removed := orig.Tokens - c.Tokens
		if removed > maxRemoved {
			maxFrac, maxRemoved = mid, removed
		}
		if removed >= need {
			// Meets the need: try keeping more.
			meetFrac, meetRemoved = mid, removed
			lo = mid
		} else {
			hi = mid
		}
	}
	if meetFrac >= 0 {
		return meetFrac, meetRemoved
	}
	return maxFrac, maxRemoved
}

// hashCommentLanguages are the fence tags whose # lines are
// comments (Python, shell, Ruby, YAML, TOML, Perl, R). A # line
// inside any other fence is left alone — in a markdown fence it
// is a header, not a comment.
var hashCommentLanguages = map[string]bool{
	"python": true, "py": true, "sh": true, "bash": true,
	"shell": true, "zsh": true, "ruby": true, "rb": true,
	"yaml": true, "yml": true, "toml": true, "perl": true,
	"pl": true, "r": true,
}

// thinCodeBlocks strips comment-only lines and blank-line runs
// inside fenced code blocks, leaving every other byte untouched
// (including all text outside fences). A fence's language tag
// decides whether # lines are comments; // lines are comments in
// any fence.
func thinCodeBlocks(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	hashComment := false
	blankRun := 0
	changed := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			hashComment = false
			if inFence {
				tag := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
				// The tag is the first word (a fence may carry
				// extra info after the language).
				if i := strings.IndexAny(tag, " \t"); i >= 0 {
					tag = tag[:i]
				}
				hashComment = hashCommentLanguages[tag]
			}
			out = append(out, ln)
			blankRun = 0
			continue
		}
		if !inFence {
			out = append(out, ln)
			continue
		}
		if trimmed == "" {
			blankRun++
			if blankRun <= 1 {
				out = append(out, "")
			} else {
				changed = true
			}
			continue
		}
		blankRun = 0
		if strings.HasPrefix(trimmed, "//") || (hashComment && strings.HasPrefix(trimmed, "#")) {
			changed = true
			continue
		}
		out = append(out, ln)
	}
	if !changed {
		return s
	}
	return strings.Join(out, "\n")
}
