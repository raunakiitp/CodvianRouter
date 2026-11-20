// Hard-budget post-pass (M7.7, PLAN.md): the final guarantee that a
// prompt fits a token budget, run AFTER Apply's lossless+lossy pass
// (the coordinator wires it into Apply at merge — this file is the
// post-pass only, it does not call itself from Apply).
//
// the reference router's hardBudget.ts ("compress to ≤ N cl100k tokens", study
// only, never copied) distributes the aggregate budget
// proportionally per message, drops the lowest-saliency sentences
// first, never drops preserve-anchored units (digits, URLs, errors,
// code fences, paths, key=value lines), and — the honest part —
// measures the result and WARNS when preserved content makes the
// target unreachable instead of claiming a fit that didn't happen.
//
// Our post-pass reuses the existing extractive steps instead of
// reimplementing them: each round calls applyLossy (drop droppable
// oldest-first, then truncate the longest plain-text message
// head+tail with the truncation marker), then RE-MEASURES the whole
// prompt with tokencount.CountPrompt and keeps the round only when
// the re-measured total strictly decreased. That re-measure is the
// whole point of a post-pass: applyLossy tracks a sum of
// per-message counts, while the budget is a whole-prompt count,
// and the truncation marker (9 BPE tokens — see
// TestHardBudgetMarkerCostsNineTokens) is only visible in the
// re-measure. A round that does not strictly improve — including a
// round where the marker costs more than the cut saved — is rolled
// back whole, so the post-pass can never return a prompt MORE
// expensive than the one it was given. Termination is by
// no-progress (every accepted round strictly decreases an integer
// token total) plus a hard round cap as a backstop.
//
// The squeeze: applyLossy sizes each truncation to the remaining
// overage, but the marker adds 9 tokens back plus ~2 tokens of
// fragment overhead (mid-word head/tail splits), so a cut nets
// overage-11 — and when the remaining overage is itself ≲11, a
// cut nets ~0 and the loop stalls a few tokens over budget. The
// post-pass therefore runs in two phases. Phase 1 targets the
// true budget. If (and only if) phase 1 stalls while still over
// budget, phase 2 ("squeeze") re-runs the same rounds targeting
// budget-18 (two markers): each cut then removes overage+18
// tokens of text, nets ~overage, and the fit lands. Fit is
// ALWAYS judged against the true budget — the squeeze only
// changes what the steps aim at. The 18 is a target offset, not
// a per-round cost budget: a round may truncate several fresh
// messages (one marker each), and applyLossy self-corrects via
// its internal recounts to the squeeze target, so the offset
// only has to absorb the whole-prompt-recount vs
// per-message-sum gap (a few tokens from BPE merges across
// message boundaries). If that gap ever exceeded 18, the result
// is an honest non-fit, never a false fit.
// The squeeze's price is bounded over-trim for truncation-driven
// fits: it can cut up to two markers (18 tokens) more text than
// the true budget strictly needs, and only in the stall case
// where the alternative is missing the budget. (When a DROP does
// the fitting, the overshoot is the dropped message's size, as
// with any lossy drop.) It cannot gut a pinned message: a message
// already at its truncation core is nearly untruncatable (the
// marker's own runes dominate the kept fraction, so a re-cut
// nets ~0 and the candidate retires) — the honest non-fit still
// ends with the cores intact and the exact overage reported.
//
// Honest non-fit: when even the pinned messages alone (system /
// developer, last user, last message — applyLossy's pinned set, and
// with FreezeSystemPrompt the system prompt additionally exempt
// from truncation) do not fit, the result says Fitted=false with
// Overage = AfterTokens - budget, the exact number of tokens over.
// It never claims a fit that didn't happen, and it never truncates
// a pinned message to nothing: applyLossy keeps a ~10-token core.
//
// FreezeSystemPrompt is the cache-aware seam (see
// internal/degrade/cache_aware.go): when the system prompt is a
// frozen cacheable prefix, it is never truncated by the post-pass.
// The mechanism is token-neutral marking, not a second trimming
// implementation: for one round, plain-text system/developer
// messages are passed to applyLossy as part-carrying messages
// (Parts = one text part holding the same content), which
// applyLossy already skips for truncation ("part-carrying
// messages are dropped whole or kept, never partially rewritten")
// while still never dropping them (they are pinned). The mark is
// token-neutral because tokencount counts a message's text parts
// when Parts is set, and the synthetic part holds exactly the
// original content — so the round's budget accounting is the same
// as unmarked. After the round the original structs are restored
// by position (system/developer messages are never dropped, so
// their subsequence pairs up exactly). A frozen prefix is
// therefore never rewritten even when that makes the budget
// unreachable (then Fitted=false, honestly).
//
// The post-pass does NOT re-run the lossless steps: it runs on
// Apply's output, which is already lossless-normalised. Called
// directly on raw messages it still works, but a duplicate system
// block the lossless step would have removed stays — that is the
// caller's (Apply's) job, not the post-pass's.
package compress

import (
	"reflect"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Steps the hard-budget post-pass reports, distinct from Apply's
// lossy step names so the coordinator can merge them without
// double-counting a drop/truncate that both passes performed.
const (
	StepHardBudgetDropped   = "hard_budget_dropped_messages"
	StepHardBudgetTruncated = "hard_budget_truncated_messages"
)

// maxHardBudgetRounds bounds the post-pass loop. It is a backstop
// only: the no-progress break ends the loop first in every case
// (an accepted round strictly decreases the integer token total),
// so reaching the cap would mean a counting inconsistency, and
// even then the loop ends and reports the best state honestly.
const maxHardBudgetRounds = 64

// hardBudgetMarkerTokens is the truncation marker's cost in BPE
// tokens (see TestHardBudgetMarkerCostsNineTokens — if the
// tokenizer ever changes, that test fails and this constant gets
// updated with it). The squeeze phase (below) budgets two markers
// under the true budget.
const hardBudgetMarkerTokens = 9

// squeezeAllowance is how far under the true budget the squeeze
// phase aims. One marker (9 tokens) is not enough: a truncation
// costs the 9-token marker PLUS ~2 tokens of fragment overhead —
// the head/tail cut lands mid-word, so "word" becomes "wo"+"ord"
// (2 tokens where 1 was). A cut sized to overage+9 therefore
// nets ~0 (measured: a squeeze-9 round on a 2-token overage
// returns the prompt unchanged). Aiming two markers under makes
// each squeeze cut net ~overage, so the fit lands.
const squeezeAllowance = 2 * hardBudgetMarkerTokens

// HardBudgetOptions tunes one HardBudget call.
type HardBudgetOptions struct {
	// FreezeSystemPrompt never truncates a system/developer
	// message: a round that truncated one has it restored
	// before the re-measure. System/developer messages are
	// already never DROPPED (applyLossy's pinned set); this
	// adds never-TRUNCATED, which is what a frozen cacheable
	// prefix needs — rewriting the prefix breaks the
	// provider-side prompt cache. Set from
	// degrade.PrefixFreeze.IsFrozen / ResolvePreserveSystemPrompt
	// by the coordinator at merge; default false keeps today's
	// lossy semantics (system may be truncated, never dropped).
	FreezeSystemPrompt bool
}

// HardBudgetResult is one post-pass outcome. All token counts are
// whole-prompt local tiktoken estimates (internal/tokencount), the
// same estimate class as Apply — never provider-reported numbers.
type HardBudgetResult struct {
	// Messages is the post-pass output. On a no-op (already in
	// budget) it is the input slice unchanged; on a no-progress
	// non-fit it is the best (cheapest) state seen, which may be
	// the input itself.
	Messages []provider.Message
	// Fitted is true only when AfterTokens <= budget, measured
	// by a whole-prompt recount — never by arithmetic.
	Fitted bool
	// Steps names the extractive steps the post-pass actually
	// kept, in application order (dropped, truncated).
	Steps []string
	// BeforeTokens / AfterTokens are the whole-prompt counts
	// before / after the post-pass (0 when counting failed).
	BeforeTokens int
	AfterTokens  int
	// Overage is AfterTokens - budget when !Fitted (the exact
	// number of tokens over — the honest non-fit report); 0
	// when Fitted; -1 when counting failed and no overage is
	// known (CountError is true then).
	Overage int
	// Iterations is how many applyLossy rounds ran (0 on a
	// no-op or a counting failure). Always <=
	// maxHardBudgetRounds — the termination proof in one number.
	Iterations int
	// CountError is true when the prompt could not be counted:
	// the input is returned unchanged, Fitted=false,
	// Overage=-1. A counting failure never fails a request and
	// never claims a fit.
	CountError bool
}

// HardBudget runs the hard-budget post-pass with default options
// (no system-prompt freeze). See HardBudgetWithOptions.
func HardBudget(model string, msgs []provider.Message, budget int) HardBudgetResult {
	return HardBudgetWithOptions(model, msgs, budget, HardBudgetOptions{})
}

// HardBudgetWithOptions repeatedly applies the existing extractive
// steps (applyLossy: drop droppable, truncate longest,
// marker-aware) until the whole-prompt token count is <= budget or
// no round can make progress, and reports honestly whether the
// budget was met. It never mutates the caller's messages: rounds
// work on applyLossy's fresh slices, and a no-op returns the input
// unchanged.
func HardBudgetWithOptions(model string, msgs []provider.Message, budget int, opts HardBudgetOptions) HardBudgetResult {
	before, err := tokencount.CountPrompt(model, msgs)
	if err != nil {
		return HardBudgetResult{Messages: msgs, Fitted: false, Overage: -1, CountError: true}
	}
	total := before.Tokens
	if total <= budget {
		return HardBudgetResult{Messages: msgs, Fitted: true, BeforeTokens: total, AfterTokens: total}
	}

	best := msgs
	bestTotal := total
	cur := msgs
	var steps []string
	iterations := 0
	// effectiveBudget is the budget the extractive rounds aim
	// at. Phase 1 aims at the true budget; the squeeze phase
	// aims 18 tokens lower — two markers (see the package comment). Fit is
	// always judged against the true budget below.
	effectiveBudget := budget
	squeezing := false
	for iterations < maxHardBudgetRounds {
		iterations++
		// One extractive round on the current state. With a
		// frozen system prompt the round runs on the marked
		// form (see freezeSystemPrompt) so applyLossy can
		// only spend its cuts on non-system messages; the
		// originals are restored before the re-measure.
		roundIn := cur
		if opts.FreezeSystemPrompt {
			roundIn = freezeSystemPrompt(cur)
		}
		round, dropped, truncated := applyLossy(model, roundIn, effectiveBudget)
		if dropped == 0 && truncated == 0 {
			// Nothing extractive left to do: only pinned
			// messages remain, all part-carrying, or every
			// survivor is already at its truncation core.
			// If phase 1 stalled while still over budget,
			// the squeeze gets one chance at the lower aim
			// (its cuts are sized overage+9, so they net
			// ~overage instead of ~0); otherwise stop.
			if !squeezing && bestTotal > budget {
				squeezing = true
				effectiveBudget = budget - squeezeAllowance
				continue
			}
			break
		}
		if opts.FreezeSystemPrompt {
			round = thawSystemPrompt(cur, round)
		}
		rec, err := tokencount.CountPrompt(model, round)
		if err != nil {
			break
		}
		if rec.Tokens >= bestTotal {
			// No progress — or regression. The usual cause
			// is marker accounting: the overage was smaller
			// than the 9-token truncation marker, so cutting
			// text out cost more than it saved. Keep the
			// best state seen; if phase 1 stalled this way,
			// the squeeze retries at the lower aim, else stop:
			// re-running the same round on the same input
			// would repeat it.
			if !squeezing && bestTotal > budget {
				squeezing = true
				effectiveBudget = budget - squeezeAllowance
				continue
			}
			break
		}
		if dropped > 0 && !contains(steps, StepHardBudgetDropped) {
			steps = append(steps, StepHardBudgetDropped)
		}
		if truncated > 0 && anyTruncationSurvived(cur, round) && !contains(steps, StepHardBudgetTruncated) {
			steps = append(steps, StepHardBudgetTruncated)
		}
		best, bestTotal, cur = round, rec.Tokens, round
		if bestTotal <= budget {
			return HardBudgetResult{
				Messages: best, Fitted: true, Steps: steps,
				BeforeTokens: total, AfterTokens: bestTotal,
				Iterations: iterations,
			}
		}
	}
	return HardBudgetResult{
		Messages: best, Fitted: false, Steps: steps,
		BeforeTokens: total, AfterTokens: bestTotal,
		Overage: bestTotal - budget, Iterations: iterations,
	}
}

// freezeSystemPrompt marks plain-text system/developer messages
// as part-carrying for one round, so applyLossy skips them for
// truncation (it never drops them either way — they are pinned).
// It returns copies; the input messages are never mutated. A
// message that already carries parts needs no mark — applyLossy
// already skips it — and is returned as-is.
func freezeSystemPrompt(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if (m.Role == "system" || m.Role == "developer") && len(m.Parts) == 0 && m.Content != "" {
			out[i].Parts = []provider.ContentPart{{Type: "text", Text: m.Content}}
		}
	}
	return out
}

// thawSystemPrompt restores the original system/developer structs
// into a round's output, undoing freezeSystemPrompt's mark.
// System/developer messages are never dropped by applyLossy, so
// their subsequence is identical in length and order in both
// slices and pairs up by index — no alignment guessing. It
// returns a copy; neither input slice is mutated. If the
// subsequences ever fail to pair up (defensive only — applyLossy
// never drops these roles), the overlapping prefix is still
// restored by index and the synthetic freeze mark is stripped
// from any unpaired system/developer message, so no mark can
// leak into the result — never restore the wrong message
// wholesale.
func thawSystemPrompt(before, after []provider.Message) []provider.Message {
	var beforeIdx, afterIdx []int
	for i, m := range before {
		if m.Role == "system" || m.Role == "developer" {
			beforeIdx = append(beforeIdx, i)
		}
	}
	for i, m := range after {
		if m.Role == "system" || m.Role == "developer" {
			afterIdx = append(afterIdx, i)
		}
	}
	if len(beforeIdx) != len(afterIdx) {
		// Defensive only: pair up the overlapping prefix by
		// index, and strip a synthetic freeze mark (one text
		// part equal to the content) from any unpaired
		// system/developer message, so the mark cannot leak.
		out := append([]provider.Message(nil), after...)
		n := len(beforeIdx)
		if len(afterIdx) < n {
			n = len(afterIdx)
		}
		for k := 0; k < n; k++ {
			out[afterIdx[k]] = before[beforeIdx[k]]
		}
		for _, i := range afterIdx[n:] {
			if len(out[i].Parts) == 1 && out[i].Parts[0].Type == "text" && out[i].Parts[0].Text == out[i].Content {
				out[i].Parts = nil
			}
		}
		return out
	}
	out := append([]provider.Message(nil), after...)
	for k := range beforeIdx {
		out[afterIdx[k]] = before[beforeIdx[k]]
	}
	return out
}

// anyTruncationSurvived reports whether any message in `after`
// carries content no message in `before` has — i.e. a truncation
// actually survived into the round. Truncation is the only
// content-modifier applyLossy performs (drops remove whole
// messages, which this check ignores), so an unmatched message
// means a truncation survived. With a frozen system prompt the
// system messages can never be truncated in the first place, so
// a surviving truncation is always a non-system one.
func anyTruncationSurvived(before, after []provider.Message) bool {
	for _, a := range after {
		matched := false
		for _, b := range before {
			if reflect.DeepEqual(a, b) {
				matched = true
				break
			}
		}
		if !matched {
			return true
		}
	}
	return false
}
