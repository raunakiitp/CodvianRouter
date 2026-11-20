// Package compress implements lossless-first prompt/context
// compression (CHECKLIST 183): shrinking the prompt before the
// provider call saves input tokens, but a lossy summary must never
// silently change meaning. So the order is fixed — lossless steps
// first, lossy only on a separate explicit per-call opt-in — and
// every application is disclosed (response headers, route trace,
// ledger row), never silent.
//
// Lossless steps (each is a pure function on []provider.Message,
// and each is pinned by unit tests on byte-exact fixtures):
//
//   - dedupe repeated system/developer blocks: an exact-duplicate
//     system or developer message (same role, same content) after
//     the first is redundant — the model already has those
//     instructions. Later copies are dropped, the first kept.
//     User/assistant/tool messages are NEVER deduped: a repeated
//     user turn may be intentional.
//   - strip redundant whitespace: outside fenced code blocks,
//     trailing whitespace per line is removed, runs of blank lines
//     collapse to at most one, and leading/trailing blank lines
//     go. Inside ``` fences (and indented code) every byte is
//     kept: whitespace there can be the content.
//   - canonicalise repeated tool results: a "tool" message whose
//     tool_call_id already has a later result is a stale replay —
//     the later result supersedes it, so only the last result per
//     tool_call_id is kept.
//
// Equivalence check (VerifyLossless): the compressed form must be
// exactly the canonical normal form of the original —
// Normalise(original) == Normalise(compressed) == compressed.
// That is a structural guarantee, stated honestly: it proves no
// byte changed except through the three named steps, and that the
// transform is idempotent. It is NOT a semantic proof (no such
// proof exists for arbitrary prompts); the semantic claim rests on
// the step definitions themselves — exact duplicates only, and
// whitespace only outside code fences. Apply enforces the check:
// if it ever fails, the original messages are sent unchanged and
// the result says Applied=false — a failed check never ships a
// transformed prompt.
//
// Lossy step (extractive trimming, NOT an abstractive LLM summary
// — the router runs no summariser model, and claiming one would
// be fake): when the client sets lossy_max_prompt_tokens, and the
// prompt is still over that budget after the lossless steps, whole
// middle turns are dropped oldest-first (system/developer messages
// and the last user message are always kept; the last message of
// any role is kept), and if that still does not fit, the longest
// plain-text messages are truncated head+tail with an inline
// "[…truncated by lossy compression…]" marker. Part-carrying
// (multimodal) messages are dropped whole or kept, never partially
// rewritten. The result reports LossyApplied=true and the saved
// count; the response header says "lossy" so the client can see
// meaning may have changed. AfterTokens may still exceed the
// budget when even the pinned messages alone do not fit — the
// count reported is the real one, never forced to the budget.
//
// Named modes (M7.6, modes.go): a request may name a preset
// ("compression.mode": lite, standard, aggressive, ultra, rtk,
// stacked) that selects lossy engines — RTK tool-output filtering
// (M7.4), Caveman-lite prose condensation (M7.5), or the stacked
// RTK → Caveman pipeline (M7.8, RunStacked). A mode never enables
// lossy by itself: the engines run only under the same explicit
// lossy opt-in (lossy_max_prompt_tokens) as the extractive pair,
// and the hard-budget post-pass (M7.7) runs after the pair when
// the prompt is still over budget. Every lossy output — whichever
// engines produced it — passes the M7.2 fidelity gate and the M7.3
// inflation guard before it is sent.
//
// Token counts are local tiktoken estimates (internal/tokencount):
// BeforeTokens / AfterTokens / SavedTokens are estimates, the same
// estimate class as the pre-send budget gate, never
// provider-reported numbers. A counting failure never fails a
// request: Apply returns the original messages with Applied=false.
package compress

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
	_ "modernc.org/sqlite"
)

// Steps, named in CompressionResult.Steps in application order.
const (
	StepDedupeSystem   = "deduped_system_blocks"
	StepWhitespace     = "stripped_redundant_whitespace"
	StepToolResults    = "canonicalised_tool_results"
	StepLossyDropped   = "lossy_dropped_messages"
	StepLossyTruncated = "lossy_truncated_messages"
	// StepFidelityGateRefused records that the M7.2 fidelity gate
	// refused a lossy result: the lossy output cut a code fence,
	// JSON value, URL, identifier or part boundary, so the
	// ORIGINAL messages were sent instead (Applied=false,
	// SavedTokens=0). It is the only step a refused result
	// carries; see CheckLossy in fidelity.go.
	StepFidelityGateRefused = "fidelity_gate_refused"
	// StepInflationGuardRefused records that the M7.3 inflation
	// guard refused a compressed result: the final output cost
	// MORE tokens than the original (the lossy truncation marker
	// can cost more than the overage it removes), so the
	// ORIGINAL messages were sent instead (Applied=false,
	// SavedTokens=0). It is the only step a refused result
	// carries; see CheckNoInflation in inflation.go. It is
	// checked after the fidelity gate, so a result refused by
	// both carries only the fidelity step.
	StepInflationGuardRefused = "inflation_guard_refused"
	// StepRTK records that the M7.4 RTK tool-output filter ran
	// (command-output detection, ANSI/progress strip, failures +
	// tails kept) as part of a named mode.
	StepRTK = "rtk_tool_output_filter"
	// StepCaveman records that the M7.5 Caveman-lite prose
	// condensation ran (extractive + rule-based only, no model
	// calls) as part of a named mode.
	StepCaveman = "caveman_prose_condensation"
	// StepMessageAging records that the B4-31 progressive aging
	// step ran (older turns kept at a shrinking head+tail
	// fraction) as part of the aggressive mode.
	StepMessageAging = "aged_older_turns"
	// StepHeuristicPrune records that the B4-33 heuristic
	// pruning step ran (code thinning + binary-search truncation
	// + oldest-first dropping) as part of the ultra mode.
	StepHeuristicPrune = "heuristic_pruned_tokens"
	// StepToolResultCap records that the G22 Lite tool-result
	// cap ran (a pre-current-turn tool result over the opted-in
	// cap was truncated; see toolcap.go).
	StepToolResultCap = "truncated_tool_results_to_cap"
)

// truncationMarker is inserted inline where lossy truncation cut a
// message's middle out, so the model can see text was removed.
const truncationMarker = "[…truncated by lossy compression…]"

// Decide resolves whether compression applies to one request: the
// per-request opt-in wins; the operator's per-model list decides
// only when the request says nothing. Lossy is never decided here
// from config — only from the request's LossyMaxPromptTokens — so
// no operator setting can ever switch a request to lossy.
//
// Returns (lossless, lossyMaxTokens, source). Source is "request"
// when the request asked for lossless (or lossy), "model-config"
// when the operator's list matched, "none" otherwise. An explicit
// lossless:false opts OUT even for a listed model.
func Decide(req *provider.CompressionRequest, model string, losslessModels []string) (bool, int, string) {
	lossyMax := 0
	if req != nil {
		lossyMax = req.LossyMaxPromptTokens
		if req.Lossless != nil {
			if *req.Lossless {
				return true, lossyMax, "request"
			}
			// Explicit false: lossless is off. A lossy request
			// still implies the lossless steps first — lossy is
			// defined as "lossless, then trim".
			if lossyMax > 0 {
				return true, lossyMax, "request"
			}
			return false, 0, "none"
		}
	}
	if lossyMax > 0 {
		return true, lossyMax, "request"
	}
	// G22: an opted-in Lite tool-result cap (ToolResultMaxChars
	// > 0) is itself a lossless-class opt-in — the client asked
	// for a Lite transform, so the lossless pass runs to apply
	// it. An explicit lossless:false above still wins (it
	// returned first), so a client can always refuse.
	if req != nil && req.ToolResultMaxChars > 0 {
		return true, lossyMax, "request"
	}
	for _, m := range losslessModels {
		if m == model {
			return true, 0, "model-config"
		}
	}
	return false, 0, "none"
}

// Apply compresses msgs for model under the decided opt-in, and
// returns the (possibly unchanged) messages plus the stamped
// result. prior is the stamp a previous pass already made (the
// server handler compressing before the engine sees an "auto/*"
// model resolve): when it says a pass already applied, the before
// count is kept from it — the saving is measured against the
// ORIGINAL prompt, not the already-compressed one — and the steps
// are merged. A nil prior means "first pass". Apply never fails a
// request: on a counting failure the original messages are
// returned with Applied=false.
func Apply(model string, msgs []provider.Message, req *provider.CompressionRequest, losslessModels []string, prior *provider.CompressionResult) ([]provider.Message, *provider.CompressionResult) {
	return applyWithOptions(model, msgs, req, losslessModels, prior, ApplyOptions{})
}

// applyWithOptions is Apply with caller-supplied options (see
// ApplyOptions in apply_options.go). Apply delegates to it with
// zero options.
func applyWithOptions(model string, msgs []provider.Message, req *provider.CompressionRequest, losslessModels []string, prior *provider.CompressionResult, opts ApplyOptions) ([]provider.Message, *provider.CompressionResult) {
	lossless, lossyMax, source := Decide(req, model, losslessModels)
	res := &provider.CompressionResult{
		Model:          model,
		Source:         source,
		LossyRequested: lossyMax > 0,
		LossyMaxTokens: lossyMax,
	}
	if prior != nil && prior.Applied {
		res.BeforeTokens = prior.BeforeTokens
		res.Steps = append(res.Steps, prior.Steps...)
		res.Lossless = prior.Lossless
	}
	if !lossless {
		// Nothing to do — but still stamp the decision (with the
		// model it was made for) so a later pass for the same
		// model does not re-decide, and a later pass for a
		// different model knows this one said no.
		if prior != nil && prior.Applied {
			// A previous pass applied compression for another
			// model name; this model does not opt in to more,
			// but the applied work stands — keep its stamp,
			// only updating the model it now answers for.
			prior.Model = model
			return msgs, prior
		}
		return msgs, res
	}
	before, err := tokencount.CountPrompt(model, msgs)
	if err != nil {
		return msgs, res
	}
	if res.BeforeTokens == 0 {
		res.BeforeTokens = before.Tokens
	}
	// G22 cache-aware inputs (rows :868/:869): the effective
	// Lite tool-result cap and whether the system prompt must
	// not be truncated. The freeze defaults to true (preserve
	// mode "always", the reference router's default) — a caller opts out
	// per request ("never") or per cache state ("whenNoCache"
	// with no cache to protect), or freezes explicitly via the
	// PrefixFreeze seam (opts.FreezeSystemPrompt, or a frozen
	// prefix reported through opts.FrozenPrefix when a cache
	// exists to protect).
	toolCap := effectiveToolCap(req, opts)
	freeze := resolveFreeze(msgs, req, opts)

	// Lossless first, always.
	losslessOut, steps := applyLossless(msgs, toolCap)
	res.Lossless = true
	for _, s := range steps {
		if !contains(res.Steps, s) {
			res.Steps = append(res.Steps, s)
		}
	}
	// The equivalence check gates the send: the lossless output
	// must be exactly the normal form of the input (cap-aware:
	// the cap is part of the normal form when opted in).
	if !VerifyLosslessWithCap(msgs, losslessOut, toolCap) {
		// Never ship a transform the check cannot vouch for.
		return msgs, &provider.CompressionResult{
			Model: model, Source: source,
			LossyRequested: res.LossyRequested, LossyMaxTokens: lossyMax,
		}
	}
	res.Verified = true
	out := losslessOut
	// preLossy is the fidelity gate's reference input: the gate
	// runs on the lossy transform only, so lossless changes
	// must never trip it (see finalGates in pipeline.go).
	preLossy := out

	// M7.4–M7.8 mode engines (merge wave). A named mode selects
	// lossy engines; the lossy opt-in (lossyMax > 0) authorises
	// them. A mode without the opt-in selects nothing — a mode
	// can never switch a request to lossy by itself — and a bare
	// lossy request (no mode) runs exactly the extractive pair
	// below, never RTK/Caveman: the documented lossy contract
	// is extractive trimming, and prose rewriting needs the
	// client to name the mode. An explicit mode whose defining
	// engine does not exist in this build (aggressive/ultra)
	// runs its available subset, the extractive pair; Steps
	// names what actually ran and res.Mode names the mode that
	// selected it. The adaptive budget dial (PickCheapestFitting)
	// and AutoTrigger have a live call site since G22 — opt-in
	// only, via ApplyOptions (ApplyWithOptions) when the request
	// names no explicit mode; default selection is still
	// explicit-only (see below).
	mode := ModeOff
	if req != nil && req.Mode != "" {
		if m, err := ParseMode(req.Mode); err == nil {
			mode = m
		}
		// Unparseable mode: defensive no-mode. The HTTP surface
		// 400s unknown modes in validateChatRequest; a direct
		// engine caller gets the lossless-only path, never a
		// failed request.
	}
	// G22 opt-in mode selection (rows :866/:867): when the
	// request names no explicit mode, AutoTrigger (then the
	// budget dial) may pick one. Default is explicit-only — a
	// bare lossy request with neither option still runs exactly
	// the extractive pair below.
	if mode == ModeOff && lossyMax > 0 {
		if opts.AutoTrigger {
			toolTokens := 0
			for _, m := range msgs {
				if m.Role != "tool" {
					continue
				}
				if c, err := tokencount.CountPrompt(model, []provider.Message{m}); err == nil {
					toolTokens += c.Tokens
				}
			}
			if m, _ := AutoTrigger(AutoTriggerInput{PromptTokens: res.BeforeTokens, ToolTokens: toolTokens}, opts.AutoTriggerConfig); m != ModeOff {
				mode = m
			}
		} else if opts.UseBudgetDial {
			if d, err := PickCheapestFitting(res.BeforeTokens, lossyMax, DefaultAvailability()); err == nil {
				mode = d.SelectedMode
			}
		}
	}
	// G22 cache-aware downgrade (row :868): for a caching
	// provider, aggressive/ultra become standard so a rewriting
	// mode cannot break the cacheable prefix (the reference router #3955:
	// the provider alone is sufficient — no cache_control
	// marker is required). res.Mode below names the effective
	// mode that actually ran.
	if opts.IsCachingProvider && mode != ModeOff {
		if cas := ResolveCacheAwareStrategy(string(mode), true); cas.Strategy != string(mode) {
			if m, err := ParseMode(cas.Strategy); err == nil {
				mode = m
			}
		}
	}
	if mode != ModeOff {
		res.Mode = string(mode)
	}

	// RTK level (row B4-25): the per-request field wins, then
	// the caller option, then standard. Resolved once here so
	// the mode=rtk step and the stacked pipeline's RTK stage
	// filter at the same level.
	rtkLevel := effectiveRTKLevel(req, opts)

	lossyChanged := false
	if lossyMax > 0 && mode != ModeOff {
		if mode == ModeStacked {
			// RunStacked owns the RTK → Caveman core, with
			// its own per-stage accounting and final gates.
			// SkipLossless: Apply already ran the lossless
			// steps above. The availability is passed
			// explicitly (a real availability, not the nil
			// default) so the call does not depend on the
			// package default.
			avail := DefaultAvailability()
			cfg := DefaultStackedConfig()
			cfg.Model = model
			cfg.RTKLevel = rtkLevel
			cfg.SkipLossless = true
			cfg.Availability = &avail
			sr := RunStacked(out, cfg)
			switch {
			case sr.Refused && sr.RefusalGate == "counting":
				// RunStacked could not count its input;
				// Apply's own counts already succeeded above,
				// so the lossy pair below still runs on the
				// post-lossless output. No refusal is recorded
				// — nothing was refused, only a counting path
				// declined.
			case sr.Refused:
				step := StepFidelityGateRefused
				if sr.RefusalGate == "inflation" {
					step = StepInflationGuardRefused
				}
				return msgs, &provider.CompressionResult{
					Model: model, Source: source,
					LossyRequested: res.LossyRequested, LossyMaxTokens: lossyMax,
					BeforeTokens: res.BeforeTokens, AfterTokens: res.BeforeTokens,
					Mode:  res.Mode,
					Steps: []string{step},
				}
			case sr.Applied:
				for _, st := range sr.Stages {
					if st.Applied {
						res.Steps = append(res.Steps, stackedStepName(st.Engine))
					}
				}
				out = sr.Messages
				lossyChanged = true
			}
		} else if pipeline, err := ResolveMode(mode, lossyMax, DefaultAvailability()); err == nil {
			for _, step := range pipeline.Steps {
				if !step.Available {
					continue
				}
				switch step.Engine {
				case EngineRTK:
					filtered, _ := FilterRTKMessages(out, rtkLevel)
					if !reflect.DeepEqual(out, filtered) {
						res.Steps = append(res.Steps, StepRTK)
						lossyChanged = true
					}
					out = filtered
				case EngineCaveman:
					// Standard mode IS Caveman (the reference router
					// standard); the stacked default is
					// Caveman full, and standard takes the
					// same level.
					condensed := CondenseMessages(out, LevelFull)
					if !reflect.DeepEqual(out, condensed.Messages) {
						res.Steps = append(res.Steps, StepCaveman)
						lossyChanged = true
					}
					out = condensed.Messages
				case EngineMessageAging:
					// Aggressive's defining step (B4-31):
					// older turns keep a shrinking head+tail
					// fraction; the recency window stays
					// verbatim. No summariser is involved.
					aged, n := AgeMessages(out, agingRecencyTurns)
					if n > 0 {
						res.Steps = append(res.Steps, StepMessageAging)
						lossyChanged = true
					}
					out = aged
				case EngineHeuristicPrune:
					// Ultra's defining step (B4-33): score-based
					// pruning toward the lossy budget — code
					// thinning, binary-search truncation,
					// oldest-first dropping.
					pruned, dropped, truncated := PruneHeuristic(model, out, lossyMax)
					if dropped > 0 || truncated > 0 {
						res.Steps = append(res.Steps, StepHeuristicPrune)
						lossyChanged = true
					}
					out = pruned
				}
			}
		}
	}

	if lossyMax > 0 {
		// G22 freeze (rows :868/:869): when the system prompt
		// is preserved, the extractive round runs on the
		// freeze-marked form (freezeSystemPrompt, the same
		// mechanism the hard-budget post-pass uses), so
		// applyLossy can only spend its cuts on non-system
		// messages; the originals are restored before the
		// re-measure. System/developer messages are never
		// dropped either way (the pinned set); freeze adds
		// never-TRUNCATED.
		lossyIn := out
		if freeze {
			lossyIn = freezeSystemPrompt(out)
		}
		after, dropped, truncated := applyLossy(model, lossyIn, lossyMax)
		if freeze {
			after = thawSystemPrompt(out, after)
		}
		if dropped > 0 {
			res.Steps = append(res.Steps, StepLossyDropped)
		}
		if truncated > 0 {
			res.Steps = append(res.Steps, StepLossyTruncated)
		}
		if dropped > 0 || truncated > 0 {
			lossyChanged = true
		}
		out = after
		// M7.7 hard-budget post-pass: applyLossy is an attempt
		// — it can stop just over budget when the truncation
		// marker costs what the cut saved (a few-token
		// overage), or when the pinned set alone does not fit.
		// When still over, run HardBudget rounds until fit or
		// no progress; it reports honestly when the pinned set
		// alone does not fit (Fitted=false, Overage exact).
		// Same transform class as applyLossy (extractive
		// trimming), so it runs on the bare lossy path too.
		if over, err := tokencount.CountPrompt(model, out); err == nil && over.Tokens > lossyMax {
			// G22: the post-pass honours the freeze — a frozen
			// system prompt is never truncated, even when that
			// makes the budget unreachable (then Fitted=false,
			// honestly). The frozen path uses
			// HardBudgetWithOptions; the unfrozen path uses the
			// plain HardBudget entry point (default options) —
			// the two are the same post-pass, split only on
			// the freeze.
			var hb HardBudgetResult
			if freeze {
				hb = HardBudgetWithOptions(model, out, lossyMax, HardBudgetOptions{FreezeSystemPrompt: true})
			} else {
				hb = HardBudget(model, out, lossyMax)
			}
			if !hb.CountError && !reflect.DeepEqual(out, hb.Messages) {
				for _, s := range hb.Steps {
					if !contains(res.Steps, s) {
						res.Steps = append(res.Steps, s)
					}
				}
				out = hb.Messages
				lossyChanged = true
			}
		}
	}
	if lossyChanged {
		res.LossyApplied = true
		// M7.2 fidelity gate, on the FINAL lossy output: the
		// lossy output (RTK, Caveman, extractive pair,
		// hard-budget post-pass) must not have cut a code
		// fence, JSON value, URL, identifier or part boundary.
		// The reference is the post-lossless output —
		// lossless changes must never trip the gate. On any
		// doubt the gate fails and the ORIGINAL messages are
		// sent unchanged. The refusal is recorded as the only
		// step, with saved=0; the gate's reasons are available
		// from CheckLossy (fidelity.go).
		if ok, _ := CheckLossy(preLossy, out); !ok {
			return msgs, &provider.CompressionResult{
				Model: model, Source: source,
				LossyRequested: res.LossyRequested, LossyMaxTokens: lossyMax,
				BeforeTokens: res.BeforeTokens, AfterTokens: res.BeforeTokens,
				Mode:  res.Mode,
				Steps: []string{StepFidelityGateRefused},
			}
		}
	}

	// M7.3 inflation guard: the final output must not cost more
	// than the original — the lossy truncation marker (9 BPE
	// tokens) can cost more than the small overage it removes,
	// so a barely-over-budget prompt can come back MORE
	// expensive. Checked on the final `out` vs `msgs`, after
	// the fidelity gate (a fidelity refusal already returned
	// above, so a result refused by both carries only the
	// fidelity step). On inflation the ORIGINAL messages are
	// sent instead, with the inflation step as the only step.
	if !reflect.DeepEqual(msgs, out) {
		if inflated, _, _ := CheckNoInflation(model, msgs, out); inflated {
			return msgs, &provider.CompressionResult{
				Model: model, Source: source,
				LossyRequested: res.LossyRequested, LossyMaxTokens: lossyMax,
				BeforeTokens: res.BeforeTokens, AfterTokens: res.BeforeTokens,
				Mode:  res.Mode,
				Steps: []string{StepInflationGuardRefused},
			}
		}
	}

	afterCount, err := tokencount.CountPrompt(model, out)
	if err != nil {
		return msgs, res
	}
	res.AfterTokens = afterCount.Tokens
	res.SavedTokens = res.BeforeTokens - res.AfterTokens
	if res.SavedTokens < 0 {
		res.SavedTokens = 0
	}
	res.Applied = !reflect.DeepEqual(msgs, out)
	if !res.Applied {
		if prior != nil && prior.Applied {
			// A re-pass (server, then engine after an
			// "auto/*" resolve) that changes nothing more:
			// the first pass's saving stands — report the
			// prior stamp, not a zeroed re-pass.
			prior.Model = model
			return out, prior
		}
		// Compression ran but changed nothing (no duplicates,
		// no redundant whitespace, under the lossy budget):
		// report it as not applied — "compression applied, 0
		// saved" would be noise in the trace and ledger.
		res.Steps = nil
		res.LossyApplied = false
		res.AfterTokens = res.BeforeTokens
		res.SavedTokens = 0
	}
	return out, res
}

// stackedStepName maps a RunStacked stage engine to the step name
// Apply reports. Unknown engines are reported by their engine id
// rather than dropped — a step that ran must appear in Steps.
func stackedStepName(engine string) string {
	switch engine {
	case StageRTK:
		return StepRTK
	case StageCaveman:
		return StepCaveman
	default:
		return engine
	}
}

// applyLossless runs the lossless steps in order and returns
// the result plus which steps changed anything. toolCap is the
// opted-in Lite tool-result cap in runes (0 = no cap): the cap
// runs last, on the canonicalised survivors only.
func applyLossless(msgs []provider.Message, toolCap int) ([]provider.Message, []string) {
	var steps []string
	out := stripWhitespaceAll(msgs)
	if !reflect.DeepEqual(msgs, out) {
		steps = append(steps, StepWhitespace)
	}
	next := dedupeSystem(out)
	if !reflect.DeepEqual(out, next) {
		steps = append(steps, StepDedupeSystem)
	}
	out = next
	next = canonicaliseToolResults(out)
	if !reflect.DeepEqual(out, next) {
		steps = append(steps, StepToolResults)
	}
	out = next
	if toolCap > 0 {
		if capped, changed := truncateToolResultsToCap(out, toolCap); changed {
			steps = append(steps, StepToolResultCap)
			out = capped
		}
	}
	return out, steps
}

// Normalise is the pure lossless normal form: whitespace, then
// system dedupe, then tool-result canonicalisation. Apply's output
// for a lossless-only request is exactly this; VerifyLossless
// compares against it.
func Normalise(msgs []provider.Message) []provider.Message {
	return NormaliseWithCap(msgs, 0)
}

// NormaliseWithCap is Normalise with an opted-in Lite tool-result
// cap (0 = no cap). The cap is part of the normal form when
// opted in, so VerifyLosslessWithCap compares against it.
func NormaliseWithCap(msgs []provider.Message, toolCap int) []provider.Message {
	out, _ := applyLossless(msgs, toolCap)
	return out
}

// VerifyLossless reports whether compressed is exactly the
// canonical normal form of original: normalising the original
// gives the compressed form, and the compressed form is already
// normal (idempotent — a second pass changes nothing). A compressed
// form with a word changed, a non-duplicate message dropped, or
// whitespace altered inside a code fence fails.
func VerifyLossless(original, compressed []provider.Message) bool {
	return VerifyLosslessWithCap(original, compressed, 0)
}

// VerifyLosslessWithCap is VerifyLossless with an opted-in Lite
// tool-result cap: the compressed form must be exactly the
// cap-aware normal form of the original.
func VerifyLosslessWithCap(original, compressed []provider.Message, toolCap int) bool {
	norm := NormaliseWithCap(original, toolCap)
	if !reflect.DeepEqual(norm, compressed) {
		return false
	}
	return reflect.DeepEqual(NormaliseWithCap(compressed, toolCap), compressed)
}

// stripWhitespaceAll applies stripRedundantWhitespace to every
// message, preserving everything else about the message (role,
// tool calls, reasoning state) by shallow copy.
func stripWhitespaceAll(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = stripMessageWhitespace(m)
	}
	return out
}

// stripMessageWhitespace rewrites one message's text. With content
// parts, each text part is rewritten and Content rebuilt as their
// concatenation (matching Message.UnmarshalJSON); image parts are
// untouched.
func stripMessageWhitespace(m provider.Message) provider.Message {
	if len(m.Parts) > 0 {
		parts := make([]provider.ContentPart, len(m.Parts))
		copy(parts, m.Parts)
		var sb strings.Builder
		for i, p := range parts {
			if p.Type == "text" {
				parts[i].Text = stripRedundantWhitespace(p.Text)
				sb.WriteString(parts[i].Text)
			}
		}
		m.Parts = parts
		m.Content = sb.String()
		return m
	}
	m.Content = stripRedundantWhitespace(m.Content)
	return m
}

// stripRedundantWhitespace canonicalises whitespace outside fenced
// code blocks: trailing spaces/tabs per line go, runs of blank
// lines collapse to at most one, leading/trailing blank lines go.
// Inside ``` fences every byte is kept verbatim.
func stripRedundantWhitespace(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	blankRun := 0
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, ln)
			blankRun = 0
			continue
		}
		if inFence {
			out = append(out, ln)
			continue
		}
		if trimmed == "" {
			blankRun++
			if blankRun <= 1 {
				out = append(out, "")
			}
			continue
		}
		blankRun = 0
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	// Trim leading/trailing blank lines.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// dedupeSystem drops exact-duplicate system/developer messages
// after the first (same role, same content). Every other role is
// untouched.
func dedupeSystem(msgs []provider.Message) []provider.Message {
	seen := map[string]bool{}
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			key := m.Role + "\x00" + m.Content
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, m)
	}
	return out
}

// canonicaliseToolResults keeps only the LAST result per
// tool_call_id: an earlier result for the same call is a stale
// replay, superseded by the later one. Tool messages without a
// tool_call_id are kept as-is.
func canonicaliseToolResults(msgs []provider.Message) []provider.Message {
	last := map[string]int{}
	for i, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			last[m.ToolCallID] = i
		}
	}
	out := make([]provider.Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" && last[m.ToolCallID] != i {
			continue
		}
		out = append(out, m)
	}
	return out
}

// applyLossy extractively trims msgs toward maxTokens (local
// tiktoken estimate). It returns the trimmed messages and how many
// messages were dropped whole / truncated. Pinned (never dropped):
// system/developer messages, the last user message, and the last
// message of any role. Truncation applies to plain-text messages
// only; part-carrying messages are dropped whole or kept.
func applyLossy(model string, msgs []provider.Message, maxTokens int) ([]provider.Message, int, int) {
	counts := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		c, err := tokencount.CountPrompt(model, []provider.Message{m})
		if err != nil {
			return msgs, 0, 0
		}
		counts[i] = c.Tokens
		total += c.Tokens
	}
	if total <= maxTokens {
		return msgs, 0, 0
	}
	// Pinned indices: system/developer, last user, last message.
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
	// Drop whole droppable messages, oldest first, until in budget.
	keep := make([]bool, len(msgs))
	for i := range msgs {
		keep[i] = true
	}
	dropped := 0
	for i := range msgs {
		if total <= maxTokens {
			break
		}
		if pinned[i] || !keep[i] {
			continue
		}
		keep[i] = false
		total -= counts[i]
		dropped++
	}
	out := make([]provider.Message, 0, len(msgs))
	outCounts := make([]int, 0, len(msgs))
	for i, m := range msgs {
		if keep[i] {
			out = append(out, m)
			outCounts = append(outCounts, counts[i])
		}
	}
	if total <= maxTokens {
		return out, dropped, 0
	}
	// Still over: truncate the longest plain-text messages,
	// head+tail, longest first, until in budget or nothing left
	// to truncate. A message may be truncated more than once
	// (it stays the longest); the loop always terminates — see
	// the no-progress guard below.
	truncated := 0
	for total > maxTokens {
		// Longest remaining plain-text message.
		best, bestCount := -1, 0
		for i, m := range out {
			if len(m.Parts) > 0 || m.Content == "" {
				continue
			}
			if outCounts[i] > bestCount {
				best, bestCount = i, outCounts[i]
			}
		}
		if best < 0 {
			break
		}
		over := total - maxTokens
		// Tokens to remove from this message: the whole
		// overage, but never more than the message minus a
		// small kept core (a message is never truncated to
		// nothing — at least ~10 tokens of it survive, or the
		// truncation stops and the overage is reported
		// honestly).
		remove := over
		if keepTokens := bestCount - remove; keepTokens < 10 {
			remove = bestCount - 10
		}
		if remove <= 0 {
			break
		}
		frac := float64(bestCount-remove) / float64(bestCount)
		out[best].Content = truncateMiddle(out[best].Content, frac)
		truncated++
		c, err := tokencount.CountPrompt(model, []provider.Message{out[best]})
		if err != nil {
			return msgs, 0, 0
		}
		reduced := outCounts[best] - c.Tokens
		total -= reduced
		outCounts[best] = c.Tokens
		// No progress (rounding, or the marker costs what the
		// cut saved): never pick this message again, or the
		// loop would spin — every iteration either shrinks the
		// total or retires a candidate, so it always ends.
		if reduced <= 0 || c.Tokens <= 10 {
			outCounts[best] = 0
		}
	}
	return out, dropped, truncated
}

// truncateMiddle keeps the head and tail of s, cutting the middle
// out and marking the cut. frac is the fraction of runes to keep
// (0 < frac < 1); the head takes 60% of the kept runes, the tail
// 40% — the end of a message usually carries the question.
func truncateMiddle(s string, frac float64) string {
	r := []rune(s)
	if frac <= 0 || frac >= 1 || len(r) == 0 {
		return s
	}
	keep := int(float64(len(r)) * frac)
	if keep < 2 {
		keep = 2
	}
	head := keep * 6 / 10
	tail := keep - head
	return string(r[:head]) + "\n" + truncationMarker + "\n" + string(r[len(r)-tail:])
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- Engine registry (B4-21, Stage 1) ----
//
// the reference router registers 15 compression engines and orders them by
// stackPriority; a stacked pipeline runs them in that order and a
// caller selects engines per request. This build previously had
// only a fixed 3-step lossless pipeline plus the two mode engines
// (RTK, Caveman) reached through ResolveMode/RunStacked. The
// registry below is the per-engine selection layer: every engine
// is a descriptor (id, stack priority, lossless flag) plus a run
// function, and a caller selects engines by id — explicitly, or
// as the default stack (priority order).
//
// Stack priorities: session-dedup 3, CCR 4, codex 12, Ionizer
// 13, SmartCrusher 15 and Relevance 18 match the reference router's
// published stackPriority values for those engines.
// Lite 1, RTK 6 and Caveman 8 are ours — they preserve the
// effective order this build already had (lossless first, RTK
// before Caveman in the stacked pipeline). Styles 19-23 are ours
// too — the five B4-49 output styles run last, after the content
// engines, and are mutually exclusive alternatives (select
// exactly one).
//
// Engine ids for engines defined here. EngineLite, EngineRTK,
// EngineCaveman and EngineSessionDedup already exist in modes.go
// and are reused, not redeclared.
const (
	// EngineCCR is the contextual-compression-retrieval engine
	// (B4-24): retrieval markers backed by a 24 h SQLite store.
	EngineCCR = "ccr"
	// EngineCodex is the Codex Responses tool-output engine
	// (B4-26): ANSI strip, repeated-line collapse, head+tail
	// truncation of code-execution-style tool outputs.
	EngineCodex = "codex"
	// EngineIonizer is the tabular row-sampling engine (B4-28):
	// oversized JSON tables are sampled (head + tail rows kept).
	EngineIonizer = "ionizer"
	// EngineSmartCrusher is the tabular JSON → columnar engine
	// (B4-27): uniform JSON tables rewritten in columnar form,
	// losslessly.
	EngineSmartCrusher = "smartcrusher"
	// EngineRelevance is the extractive sentence-scoring engine
	// (B4-29): low-scoring sentences are dropped, first/last and
	// preserve-regex matches are always kept.
	EngineRelevance = "relevance"
	// EngineStyleTerse is the Terse prose output style (B4-49):
	// filler words dropped, minimal-word prose.
	EngineStyleTerse = "style_terse"
	// EngineStyleLessCode is the Less code output style (B4-49):
	// blank lines and full-line comments dropped from fenced
	// code.
	EngineStyleLessCode = "style_less_code"
	// EngineStylePonytail is the Ponytail output style (B4-49):
	// first + last sentence kept, the middle dropped — a short
	// summary tail.
	EngineStylePonytail = "style_ponytail"
	// EngineStyleADHD is the "I have ADHD" output style (B4-49):
	// at most three sentences, each on its own bullet line.
	EngineStyleADHD = "style_adhd"
	// EngineStyleTerseCJK is the Terse CJK output style (B4-49):
	// CJK + Latin filler dropped, terse form for CJK text.
	EngineStyleTerseCJK = "style_terse_cjk"
)

// StackPriority values for the registered engines. See the
// registry doc above for which are the reference router's and which are ours.
const (
	PriorityLite          = 1
	PrioritySessionDedup  = 3
	PriorityCCR           = 4
	PriorityRTK           = 6
	PriorityCaveman       = 8
	PriorityCodex         = 12
	PriorityIonizer       = 13
	PrioritySmartCrusher  = 15
	PriorityRelevance     = 18
	PriorityStyleTerse    = 19
	PriorityStyleLessCode = 20
	PriorityStylePonytail = 21
	PriorityStyleADHD     = 22
	PriorityStyleTerseCJK = 23
)

// StepSessionDedup records that the session-dedup engine replaced
// a repeated block with a [dedup:ref sha=…] marker.
const StepSessionDedup = "session_dedup_refs"

// StepCCR records that the CCR engine replaced retrieved content
// with a [ccr:ref sha=…] marker.
const StepCCR = "ccr_retrieval_markers"

// StepCodex records that the Codex Responses engine compressed a
// tool output.
const StepCodex = "codex_tool_output_compression"

// StepIonizer records that the Ionizer engine sampled the rows of
// an oversized JSON table.
const StepIonizer = "ionizer_row_sampling"

// StepSmartCrusher records that the SmartCrusher engine rewrote a
// JSON table in columnar form.
const StepSmartCrusher = "smartcrusher_tables_columnar"

// StepRelevance records that the Relevance engine dropped
// low-scoring sentences.
const StepRelevance = "relevance_sentence_scoring"

// StepStyleTerse records that the Terse style dropped filler
// words from prose.
const StepStyleTerse = "style_terse"

// StepStyleLessCode records that the Less code style dropped
// blank lines and full-line comments from fenced code.
const StepStyleLessCode = "style_less_code"

// StepStylePonytail records that the Ponytail style kept only
// the first and last sentences.
const StepStylePonytail = "style_ponytail"

// StepStyleADHD records that the "I have ADHD" style kept at
// most three sentences as bullet lines.
const StepStyleADHD = "style_adhd"

// StepStyleTerseCJK records that the Terse CJK style dropped
// CJK filler phrases.
const StepStyleTerseCJK = "style_terse_cjk"

// StyleEngines lists the five output styles (B4-49) in stack
// order. The styles are mutually exclusive alternatives: a
// request picks exactly one via ApplyWithEngines — selecting
// several stacks them, which is never what a caller wants. They
// are all lossy, so they run only under the lossy opt-in, and the
// plain Apply path never runs any of them.
var StyleEngines = []string{
	EngineStyleTerse,
	EngineStyleLessCode,
	EngineStylePonytail,
	EngineStyleADHD,
	EngineStyleTerseCJK,
}

// EngineContext carries the per-request inputs the registry
// engines need beyond the messages: the RTK level (empty means
// standard) and the Codex truncation budget in runes (<= 0 means
// lossless-only parts: ANSI strip + duplicate-line collapse, no
// truncation). The session-dedup (B4-23) and CCR (B4-24) features
// add the session id and the stores to this struct as they land.
// A nil store means its engine is skipped, never an error.
type EngineContext struct {
	// RTKLevel selects the RTK filter level for the registry
	// RTK runner (B4-25). "" means RTKLevelStandard. An
	// unparseable level also means standard — an unknown level
	// must never fail a request.
	RTKLevel RTKLevel
	// CodexMaxChars is the Codex engine's truncation budget in
	// runes (B4-26). <= 0 means no truncation.
	CodexMaxChars int
	// SessionID scopes the session-dedup store (B4-23). "" means
	// no session: the session-dedup engine is skipped.
	SessionID string
	// DedupStore is the cross-turn store for session-dedup
	// (B4-23). Nil means the engine is skipped.
	DedupStore *SessionDedupStore
	// CCRStore is the retrieval-marker store for CCR (B4-24).
	// Nil means the engine is skipped.
	CCRStore *CCRStore
	// PreserveRegex is the Relevance engine's preserve filter
	// (B4-29): sentences matching it are always kept. Nil means
	// no preserve filter.
	PreserveRegex *regexp.Regexp
}

// EngineDescriptor describes one registered engine: its id, its
// stack priority (lower runs first), and whether it is lossless
// (a lossless engine never changes meaning; a lossy one only
// runs under the lossy opt-in — enforced by the caller, see
// ApplyWithEngines).
type EngineDescriptor struct {
	ID            string
	StackPriority int
	Lossless      bool
	Description   string
}

// EngineRegistry maps engine ids to descriptors plus run
// functions. It is built once via DefaultRegistry; Register is
// for tests and future engines, not for request-time mutation.
type EngineRegistry struct {
	engines map[string]EngineDescriptor
	run     map[string]func([]provider.Message, EngineContext) []provider.Message
}

// Register adds (or replaces) one engine. A nil run function
// registers the descriptor but the engine never runs.
func (r *EngineRegistry) Register(d EngineDescriptor, run func([]provider.Message, EngineContext) []provider.Message) {
	if r.engines == nil {
		r.engines = map[string]EngineDescriptor{}
	}
	if r.run == nil {
		r.run = map[string]func([]provider.Message, EngineContext) []provider.Message{}
	}
	r.engines[d.ID] = d
	r.run[d.ID] = run
}

// Get returns the descriptor for id, or false when unregistered.
func (r *EngineRegistry) Get(id string) (EngineDescriptor, bool) {
	d, ok := r.engines[id]
	return d, ok
}

// Ordered returns all registered engines sorted by stack
// priority (lowest first). Ties break by id, so the order is
// deterministic.
func (r *EngineRegistry) Ordered() []EngineDescriptor {
	out := make([]EngineDescriptor, 0, len(r.engines))
	for _, d := range r.engines {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackPriority != out[j].StackPriority {
			return out[i].StackPriority < out[j].StackPriority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DefaultStack returns the ids of the default stacked pipeline:
// every registered engine, in priority order.
func (r *EngineRegistry) DefaultStack() []string {
	ordered := r.Ordered()
	ids := make([]string, len(ordered))
	for i, d := range ordered {
		ids[i] = d.ID
	}
	return ids
}

// Run runs the named engines in stack-priority order (not in
// list order — the list is a selection, the priority is the
// order). An empty ids list runs the default stack. Unknown ids
// and engines without a run function are skipped, never an
// error. It returns the messages and the ids that changed
// anything.
func (r *EngineRegistry) Run(msgs []provider.Message, ids []string, ctx EngineContext) ([]provider.Message, []string) {
	if len(ids) == 0 {
		ids = r.DefaultStack()
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := msgs
	var applied []string
	for _, d := range r.Ordered() {
		if !want[d.ID] {
			continue
		}
		run, ok := r.run[d.ID]
		if !ok || run == nil {
			continue
		}
		next := run(out, ctx)
		if !reflect.DeepEqual(out, next) {
			applied = append(applied, d.ID)
		}
		out = next
	}
	return out, applied
}

// DefaultRegistry returns the registry with every engine this
// build has: lite, session-dedup (B4-23), CCR (B4-24), RTK,
// Caveman, codex (B4-26), Ionizer (B4-28), SmartCrusher (B4-27),
// Relevance (B4-29) and the five output styles (B4-49). The
// styles are mutually exclusive alternatives — select exactly one
// via ApplyWithEngines, never the whole set.
func DefaultRegistry() *EngineRegistry {
	r := &EngineRegistry{}
	r.Register(EngineDescriptor{
		ID: EngineLite, StackPriority: PriorityLite, Lossless: true,
		Description: "lossless 3-step pipeline: whitespace strip + system dedupe + tool-result canonicalisation",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		out, _ := applyLossless(in, 0)
		return out
	})
	r.Register(EngineDescriptor{
		ID: EngineSessionDedup, StackPriority: PrioritySessionDedup, Lossless: true,
		Description: "session-dedup (B4-23): cross-turn repeated blocks replaced by [dedup:ref sha=…] markers",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return DeduplicateSession(in, ctx.DedupStore, ctx.SessionID)
	})
	r.Register(EngineDescriptor{
		ID: EngineCCR, StackPriority: PriorityCCR, Lossless: true,
		Description: "CCR (B4-24): retrieved content replaced by [ccr:ref sha=…] markers, 24 h SQLite store",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return ApplyCCR(in, ctx.CCRStore)
	})
	r.Register(EngineDescriptor{
		ID: EngineRTK, StackPriority: PriorityRTK, Lossless: false,
		Description: "RTK tool-output filter (M7.4); level from EngineContext, standard when unset",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		level := ctx.RTKLevel
		if level == "" {
			level = RTKLevelStandard
		}
		if _, ok := ParseRTKLevel(string(level)); !ok {
			level = RTKLevelStandard
		}
		out, _ := FilterRTKMessages(in, level)
		return out
	})
	r.Register(EngineDescriptor{
		ID: EngineCaveman, StackPriority: PriorityCaveman, Lossless: false,
		Description: "Caveman-lite prose condensation (M7.5), full level",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return CondenseMessages(in, LevelFull).Messages
	})
	r.Register(EngineDescriptor{
		ID: EngineCodex, StackPriority: PriorityCodex, Lossless: false,
		Description: "Codex Responses tool-output engine (B4-26): ANSI strip, repeated-line collapse, head+tail truncation",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		out, _ := CompressCodexResponses(in, ctx.CodexMaxChars)
		return out
	})
	r.Register(EngineDescriptor{
		ID: EngineIonizer, StackPriority: PriorityIonizer, Lossless: false,
		Description: "Ionizer tabular row sampling (B4-28): oversized JSON tables sampled to head + tail rows with a kept/total marker",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return SampleTableRows(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineSmartCrusher, StackPriority: PrioritySmartCrusher, Lossless: true,
		Description: "SmartCrusher tabular JSON → columnar (B4-27): uniform JSON tables rewritten in columnar form, losslessly",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return CrushTablesToColumnar(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineRelevance, StackPriority: PriorityRelevance, Lossless: false,
		Description: "Relevance extractive sentence scoring (B4-29): low-scoring sentences dropped, first/last and preserve-regex matches kept",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return ScoreSentences(in, ctx.PreserveRegex)
	})
	r.Register(EngineDescriptor{
		ID: EngineStyleTerse, StackPriority: PriorityStyleTerse, Lossless: false,
		Description: "Terse prose output style (B4-49): filler words dropped, minimal-word prose",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return StyleTerse(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineStyleLessCode, StackPriority: PriorityStyleLessCode, Lossless: false,
		Description: "Less code output style (B4-49): blank lines and full-line comments dropped from fenced code",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return StyleLessCode(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineStylePonytail, StackPriority: PriorityStylePonytail, Lossless: false,
		Description: "Ponytail output style (B4-49): first + last sentence kept, the middle dropped — a short summary tail",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return StylePonytail(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineStyleADHD, StackPriority: PriorityStyleADHD, Lossless: false,
		Description: "\"I have ADHD\" output style (B4-49): at most three sentences, each on its own bullet line",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return StyleADHD(in)
	})
	r.Register(EngineDescriptor{
		ID: EngineStyleTerseCJK, StackPriority: PriorityStyleTerseCJK, Lossless: false,
		Description: "Terse CJK output style (B4-49): CJK + Latin filler dropped, terse form for CJK text",
	}, func(in []provider.Message, ctx EngineContext) []provider.Message {
		return StyleTerseCJK(in)
	})
	return r
}

// engineStepName maps a registry engine id to the step name Apply
// reports for it. Unknown engines are reported by id rather than
// dropped — a step that ran must appear in Steps.
func engineStepName(engine string) string {
	switch engine {
	case EngineLite:
		return StepWhitespace // lite is the lossless 3-step pipeline; its steps are named by applyLossless
	case EngineSessionDedup:
		return StepSessionDedup
	case EngineCCR:
		return StepCCR
	case EngineRTK:
		return StepRTK
	case EngineCaveman:
		return StepCaveman
	case EngineCodex:
		return StepCodex
	case EngineIonizer:
		return StepIonizer
	case EngineSmartCrusher:
		return StepSmartCrusher
	case EngineRelevance:
		return StepRelevance
	case EngineStyleTerse:
		return StepStyleTerse
	case EngineStyleLessCode:
		return StepStyleLessCode
	case EngineStylePonytail:
		return StepStylePonytail
	case EngineStyleADHD:
		return StepStyleADHD
	case EngineStyleTerseCJK:
		return StepStyleTerseCJK
	default:
		return engine
	}
}

// ApplyWithEngines is Apply with an explicit engine selection:
// the named registry engines run first (in stack-priority order),
// then the result goes through exactly Apply's path (lossless
// steps, mode engines, lossy pair, hard-budget post-pass, fidelity
// and inflation gates, counting). Engines Apply already runs
// (lite, rtk, caveman) are NOT re-run in the pre-pass — the
// pre-pass runs only the engines Apply does not cover
// (session-dedup, ccr, codex), so nothing runs twice. The lossy
// engines in the selection run only when the request carries the
// lossy opt-in (lossyMax > 0), exactly like Apply's mode engines:
// without it the pre-pass runs only the lossless selected engines
// and Apply runs its lossless path. A nil/empty selection means
// plain Apply. Steps from the pre-pass are merged into the
// result's Steps when the result is Applied.
func ApplyWithEngines(model string, msgs []provider.Message, req *provider.CompressionRequest, losslessModels []string, prior *provider.CompressionResult, engines []string, ctx EngineContext) ([]provider.Message, *provider.CompressionResult) {
	reg := DefaultRegistry()
	var pre []string
	lossyMax := 0
	if req != nil {
		lossyMax = req.LossyMaxPromptTokens
	}
	for _, id := range engines {
		if id == EngineLite || id == EngineRTK || id == EngineCaveman {
			continue
		}
		if d, ok := reg.Get(id); ok && !d.Lossless && lossyMax <= 0 {
			continue
		}
		pre = append(pre, id)
	}
	out := msgs
	var applied []string
	if len(pre) > 0 {
		out, applied = reg.Run(out, pre, ctx)
	}
	out, res := applyWithOptions(model, out, req, losslessModels, prior, ApplyOptions{})
	if len(applied) > 0 {
		res.Applied = true
		for _, id := range applied {
			step := engineStepName(id)
			if !contains(res.Steps, step) {
				res.Steps = append(res.Steps, step)
			}
		}
		if res.BeforeTokens == 0 {
			if bc, err := tokencount.CountPrompt(model, msgs); err == nil {
				res.BeforeTokens = bc.Tokens
			}
		}
		if ac, err := tokencount.CountPrompt(model, out); err == nil {
			res.AfterTokens = ac.Tokens
		}
		res.SavedTokens = res.BeforeTokens - res.AfterTokens
		if res.SavedTokens < 0 {
			res.SavedTokens = 0
		}
	}
	return out, res
}

// ---- Session-dedup (B4-23, Stage 1) ----
//
// the reference router's session-dedup engine (stackPriority 3, lossless):
// content repeated across turns of one session is replaced by a
// [dedup:ref sha=…] marker pointing at the earlier occurrence —
// the model still sees the original earlier in the conversation,
// so nothing is lost, but the repeated bytes are not sent again.
// Content-addressed: the marker names the SHA-256 of the exact
// role + content, so a marker can only ever name the identical
// bytes.
//
// Contract (stated, not assumed): a marker is valid only while
// the original occurrence is still in the conversation the model
// sees. The store is per-session and the caller must pass the
// session's store; without a session id or a store the engine is
// skipped, never guessed — a global store would let one session's
// marker name content another session never saw. User messages
// are never deduped (a repeated user turn may be intentional —
// the same rule as dedupeSystem), part-carrying messages are
// left alone, and an already-marked message is left alone so the
// transform is idempotent. The live request-path wiring (the
// server passing the session id + store into ApplyWithEngines)
// is a server-lane follow-up; the engine, store and registry
// entry are live here.

// SessionDedupStore is the cross-turn content-addressed store
// for one router process: session id -> set of content SHAs seen
// in that session. In-memory only; a restart forgets it (a
// missed dedup costs tokens, never correctness — a marker is
// only ever emitted for content this process has seen).
type SessionDedupStore struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

// NewSessionDedupStore returns an empty store.
func NewSessionDedupStore() *SessionDedupStore {
	return &SessionDedupStore{seen: map[string]map[string]struct{}{}}
}

// Seen reports whether sha was already recorded for sessionID.
func (s *SessionDedupStore) Seen(sessionID, sha string) bool {
	if s == nil || sessionID == "" || sha == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.seen[sessionID]
	if !ok {
		return false
	}
	_, ok = m[sha]
	return ok
}

// Mark records sha for sessionID.
func (s *SessionDedupStore) Mark(sessionID, sha string) {
	if s == nil || sessionID == "" || sha == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.seen[sessionID]
	if !ok {
		m = map[string]struct{}{}
		s.seen[sessionID] = m
	}
	m[sha] = struct{}{}
}

// dedupContentSHA is the content address: SHA-256 hex of
// role + NUL + content.
func dedupContentSHA(role, content string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// DedupRefMarker renders the marker for a content SHA. Only the
// first 16 hex chars travel (64 bits — a collision needs a
// deliberate second preimage, and the marker names content the
// model already saw in this session).
func DedupRefMarker(sha string) string {
	if len(sha) > 16 {
		sha = sha[:16]
	}
	return "[dedup:ref sha=" + sha + "]"
}

// ParseDedupRef reports whether s is exactly a dedup marker and
// returns its sha.
func ParseDedupRef(s string) (string, bool) {
	if !strings.HasPrefix(s, "[dedup:ref sha=") || !strings.HasSuffix(s, "]") {
		return "", false
	}
	sha := s[len("[dedup:ref sha=") : len(s)-1]
	if sha == "" {
		return "", false
	}
	return sha, true
}

// DeduplicateSession replaces every repeated plain-content block
// (any role except user) with its [dedup:ref sha=…] marker when
// the store already holds that content for this session, and
// records first occurrences. A nil store or empty session id
// returns the messages unchanged. The input slice is not
// mutated: a new slice is returned.
func DeduplicateSession(msgs []provider.Message, store *SessionDedupStore, sessionID string) []provider.Message {
	if store == nil || sessionID == "" || len(msgs) == 0 {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	// Same within-request rule as ApplyCCR: a sha marked earlier
	// in this same call is a duplicate in this request, not a
	// cross-turn repeat — leave it unchanged so the lossless
	// dedup (dedupeSystem) collapses it instead of markering it.
	markedThisCall := map[string]bool{}
	for i, m := range out {
		if m.Role == "user" || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		if _, isMarker := ParseDedupRef(m.Content); isMarker {
			continue
		}
		sha := dedupContentSHA(m.Role, m.Content)
		if markedThisCall[sha] {
			continue
		}
		if store.Seen(sessionID, sha) {
			out[i].Content = DedupRefMarker(sha)
			continue
		}
		store.Mark(sessionID, sha)
		markedThisCall[sha] = true
	}
	return out
}

// ---- CCR (B4-24, Stage 1) ----
//
// the reference router's CCR engine (stackPriority 4, lossless):
// contextual-compression-retrieval — retrieved content is stored
// once and later occurrences are replaced by a [ccr:ref sha=…]
// marker, with a 24 h retention backed by SQLite. Same marker
// discipline as session-dedup (content-addressed by SHA-256 of
// role + content; the model still sees the original earlier in
// the conversation), but persistent across restarts: the store
// is a SQLite file, not process memory. A store failure (open,
// read or write) never fails a request — the engine skips and
// the messages go through unchanged. The live request-path
// wiring (the server opening the store and passing it into
// ApplyWithEngines) is a server-lane follow-up; the store,
// engine and registry entry are live here.

// CCRTTL is the retention for CCR markers: content older than
// this is expired (Get reports missing; Prune deletes it).
const CCRTTL = 24 * time.Hour

// CCRStore is the persistent retrieval-marker store: sha ->
// content, with a creation timestamp. One SQLite file, opened
// with database/sql + modernc.org/sqlite (no cgo — the same
// driver the ledger and cache use).
type CCRStore struct {
	mu sync.Mutex
	db *sql.DB
}

const ccrSchema = `CREATE TABLE IF NOT EXISTS ccr_markers(
	sha TEXT PRIMARY KEY,
	content TEXT NOT NULL,
	created_unix INTEGER NOT NULL
);`

// OpenCCRStore opens (creating it if needed) the SQLite file at
// path and prunes expired rows. An empty path is an error.
func OpenCCRStore(path string) (*CCRStore, error) {
	if path == "" {
		return nil, fmt.Errorf("compress: CCR store path is empty")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ccrSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &CCRStore{db: db}
	_, _ = s.Prune()
	return s, nil
}

// Close closes the store's database.
func (s *CCRStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Put stores content under sha (replacing any previous row).
func (s *CCRStore) Put(sha, content string) error {
	if s == nil || s.db == nil || sha == "" {
		return fmt.Errorf("compress: CCR store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO ccr_markers(sha, content, created_unix) VALUES(?, ?, ?)`,
		sha, content, time.Now().Unix())
	return err
}

// Get returns the stored content for sha, or false when missing
// or expired (older than CCRTTL — the row is deleted on sight).
func (s *CCRStore) Get(sha string) (string, bool) {
	if s == nil || s.db == nil || sha == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var content string
	var created int64
	err := s.db.QueryRow(`SELECT content, created_unix FROM ccr_markers WHERE sha = ?`, sha).Scan(&content, &created)
	if err != nil {
		return "", false
	}
	if time.Since(time.Unix(created, 0)) > CCRTTL {
		_, _ = s.db.Exec(`DELETE FROM ccr_markers WHERE sha = ?`, sha)
		return "", false
	}
	return content, true
}

// Prune deletes all expired rows. It returns the deleted count.
func (s *CCRStore) Prune() (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("compress: CCR store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM ccr_markers WHERE created_unix <= ?`, time.Now().Add(-CCRTTL).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CCRRefMarker renders the marker for a retrieval SHA (first 16
// hex chars, as with session-dedup).
func CCRRefMarker(sha string) string {
	if len(sha) > 16 {
		sha = sha[:16]
	}
	return "[ccr:ref sha=" + sha + "]"
}

// ParseCCRRef reports whether s is exactly a CCR marker and
// returns its sha.
func ParseCCRRef(s string) (string, bool) {
	if !strings.HasPrefix(s, "[ccr:ref sha=") || !strings.HasSuffix(s, "]") {
		return "", false
	}
	sha := s[len("[ccr:ref sha=") : len(s)-1]
	if sha == "" {
		return "", false
	}
	return sha, true
}

// ApplyCCR replaces every repeated plain-content block (any role
// except user) with its [ccr:ref sha=…] marker when the store
// already holds that content unexpired, and stores first
// occurrences. A nil store returns the messages unchanged; a
// store error on any single message is treated as a miss for
// that message (the store never fails a request). The input
// slice is not mutated.
//
// A block stored earlier in this same call is a within-request
// duplicate, not a cross-request repeat: it is left unchanged so
// the lossless dedup (dedupeSystem) collapses it. Markering it
// would break the dedup (the marker's content differs from the
// original) and trip the lossless verification, so Apply would
// return the originals uncompressed.
func ApplyCCR(msgs []provider.Message, store *CCRStore) []provider.Message {
	if store == nil || len(msgs) == 0 {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	storedThisCall := map[string]bool{}
	for i, m := range out {
		if m.Role == "user" || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		if _, isMarker := ParseCCRRef(m.Content); isMarker {
			continue
		}
		if _, isMarker := ParseDedupRef(m.Content); isMarker {
			continue
		}
		sha := dedupContentSHA(m.Role, m.Content)
		if storedThisCall[sha] {
			continue
		}
		if _, ok := store.Get(sha); ok {
			out[i].Content = CCRRefMarker(sha)
			continue
		}
		_ = store.Put(sha, m.Content)
		storedThisCall[sha] = true
	}
	return out
}

// ExpandCCR replaces [ccr:ref sha=…] markers with the stored
// content (for inspection/debugging — the send path keeps the
// markers). A marker whose content is missing or expired is
// left as-is.
func ExpandCCR(msgs []provider.Message, store *CCRStore) []provider.Message {
	if store == nil || len(msgs) == 0 {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		sha, ok := ParseCCRRef(m.Content)
		if !ok {
			continue
		}
		// The marker carries 16 hex chars; the store is keyed
		// by full SHA. A 16-char prefix cannot be resolved
		// exactly, so expansion looks the prefix up by scan.
		// (Markers are send-side only; expansion is a debug
		// aid, not the request path.)
		if content, found := store.GetByPrefix(sha); found {
			out[i].Content = content
		}
	}
	return out
}

// GetByPrefix returns the content of the single row whose sha
// starts with prefix, or false when there is none or more than
// one. Used only by ExpandCCR (debug aid).
func (s *CCRStore) GetByPrefix(prefix string) (string, bool) {
	if s == nil || s.db == nil || prefix == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT sha, content, created_unix FROM ccr_markers WHERE sha LIKE ?`, prefix+"%")
	if err != nil {
		return "", false
	}
	defer rows.Close()
	var content string
	found := 0
	for rows.Next() {
		var sha, rowContent string
		var created int64
		if err := rows.Scan(&sha, &rowContent, &created); err != nil {
			return "", false
		}
		if time.Since(time.Unix(created, 0)) > CCRTTL {
			continue
		}
		content = rowContent
		found++
	}
	if found != 1 {
		return "", false
	}
	return content, true
}

// ---- Codex Responses tool-output compression (B4-26, Stage 1) ----
//
// the reference router's Codex engine (stackPriority 12, lossless-first):
// compresses tool outputs from Codex Responses-style calls
// (code execution / file reads / long command output). This
// build has no Responses-API wire shape yet, so the engine
// targets what it can honestly identify: role "tool" messages
// carrying plain text. It does three things, in order:
//
//   - strip ANSI escape sequences (CSI + OSC): terminal
//     formatting, not content — the model loses no information.
//   - collapse consecutive duplicate lines to one occurrence
//     (exact repeats only — progress spam, repeated headers).
//   - when a rune budget is given (CodexMaxChars > 0) and the
//     output is still over it, keep head + tail with an inline
//     "[...codex output truncated...]" marker. Without a budget
//     there is no truncation: the engine is lossless-only.
//
// Provider-specific detection (Responses API item types) is a
// follow-up for when the wire shape lands; until then the
// engine never guesses a message is a Codex output — it applies
// the same transforms to every plain-text tool message, which
// are safe transforms for any tool output. Part-carrying
// messages are left alone. The live request-path wiring (the
// server passing a budget into ApplyWithEngines) is a
// server-lane follow-up; the engine and registry entry are live
// here.

// codexANSIPattern matches ANSI CSI sequences (ESC [ params
// final-byte) and OSC sequences (ESC ] ... BEL or ESC \).
var codexANSIPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

// codexTruncationMarker marks a head+tail cut in a Codex output.
const codexTruncationMarker = "[...codex output truncated...]"

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	return codexANSIPattern.ReplaceAllString(s, "")
}

// collapseDuplicateLines collapses runs of identical consecutive
// lines to a single occurrence, preserving order. It reports
// whether anything changed.
func collapseDuplicateLines(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s, false
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, ln := range lines {
		if len(out) > 0 && out[len(out)-1] == ln {
			changed = true
			continue
		}
		out = append(out, ln)
	}
	if !changed {
		return s, false
	}
	return strings.Join(out, "\n"), true
}

// truncateCodexMiddle keeps the head and tail of s, cutting the
// middle out and marking the cut. frac is the fraction of runes
// to keep (0 < frac < 1); the head takes 60% of the kept runes,
// the tail 40% — the end of a tool output usually carries the
// result or the error.
func truncateCodexMiddle(s string, frac float64) string {
	r := []rune(s)
	if frac <= 0 || frac >= 1 || len(r) == 0 {
		return s
	}
	keep := int(float64(len(r)) * frac)
	if keep < 2 {
		keep = 2
	}
	head := keep * 6 / 10
	tail := keep - head
	return string(r[:head]) + "\n" + codexTruncationMarker + "\n" + string(r[len(r)-tail:])
}

// CompressCodexResponses compresses role "tool" messages in
// msgs as described above. maxChars <= 0 means no truncation.
// It returns the messages and whether anything changed. The
// input slice is not mutated.
func CompressCodexResponses(msgs []provider.Message, maxChars int) ([]provider.Message, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	changed := false
	for i, m := range out {
		if m.Role != "tool" || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		content := stripANSI(m.Content)
		if c, dup := collapseDuplicateLines(content); dup {
			content = c
		}
		if maxChars > 0 {
			if r := []rune(content); len(r) > maxChars {
				content = truncateCodexMiddle(content, float64(maxChars)/float64(len(r)))
			}
		}
		if content != m.Content {
			out[i].Content = content
			changed = true
		}
	}
	return out, changed
}
