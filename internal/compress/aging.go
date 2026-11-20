// B4-31 Progressive aging (Wave 12, Stage 1).
//
// AgeMessages is the progressive-aging step for the aggressive
// mode: the older a turn, the stronger its compression. Turns
// inside the recency window are untouched; outside it, each older
// turn keeps a shrinking head+tail fraction — the oldest turn
// keeps the least. This is extractive trimming only (the same
// head+tail cut applyLossy uses, via truncateMiddle): there is no
// summariser in-router by design, and this step never invents
// one. Pinned messages (system/developer, last user, last
// message) are never aged, and part-carrying messages are never
// partially rewritten.
package compress

import (
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// agingRecencyTurns is the recency window: the last N turns (a
// turn = one user/assistant/tool message, counting from the end)
// are never aged. A CHOSEN DEFAULT (an operator default, not a
// measurement): small enough that long sessions age visibly,
// large enough that the recent context — the part the next reply
// actually builds on — stays verbatim.
const agingRecencyTurns = 4

// agingMinKeepFrac is the smallest head+tail fraction any aged
// turn keeps: even the oldest turn keeps at least ~10% of its
// runes (plus truncateMiddle's 2-rune floor), so it is never
// aged to nothing — a turn that must go entirely is dropped by
// the extractive pair instead.
const agingMinKeepFrac = 0.10

// AgeMessages progressively compresses older turns and returns
// the aged messages plus how many changed. recencyTurns < 0 is
// treated as 0 (every turn ages). The input slice is never
// mutated.
func AgeMessages(msgs []provider.Message, recencyTurns int) ([]provider.Message, int) {
	if recencyTurns < 0 {
		recencyTurns = 0
	}
	if len(msgs) == 0 {
		return msgs, 0
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	pinned := pinnedIndices(msgs)
	// age[i] = turns after i (user/assistant/tool messages
	// only — system/developer messages are pinned and never
	// aged, so they do not advance the age count).
	age := make([]int, len(msgs))
	after := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		age[i] = after
		if msgs[i].Role != "system" && msgs[i].Role != "developer" {
			after++
		}
	}
	changed := 0
	for i, m := range out {
		if pinned[i] || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		steps := age[i] - recencyTurns + 1
		if steps < 1 {
			continue
		}
		// Step 1 (just outside the window) keeps half; each
		// older step halves again, floored at the minimum.
		frac := 0.5
		for s := 1; s < steps; s++ {
			frac /= 2
		}
		if frac < agingMinKeepFrac {
			frac = agingMinKeepFrac
		}
		aged := truncateMiddle(m.Content, frac)
		if aged != m.Content {
			out[i].Content = aged
			changed++
		}
	}
	return out, changed
}
