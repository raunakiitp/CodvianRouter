// Package compress — M7.8 stacked pipeline RTK → Caveman
// (parity tracker Area 10, "Stacked pipeline RTK → Caveman", MISSING).
//
// This file is the EXECUTION of the stacked pipeline: it runs the
// engines in order and accounts for every stage. It does NOT wire
// itself into Apply — the coordinator does that at merge time.
// Nothing here calls a model, does network I/O, or reads config
// files.
//
// What was read in the reference router (study only — no code copied):
//
//   - open-sse/services/compression/types.ts —
//     DEFAULT_COMPRESSION_CONFIG: enabled false, defaultMode
//     "off", stackedPipeline [{rtk standard}, {caveman full}],
//     every engine toggle enabled:false by default.
//   - open-sse/services/compression/deriveDefaultPlan.ts —
//     masterEnabled=false or no engines on → mode "off"; exactly
//     one single-mode engine on → that engine's standalone mode;
//     otherwise mode "stacked" with the enabled engines sorted
//     by stackPriority.
//   - open-sse/services/compression/engineCatalog.ts —
//     stackPriority: rtk 10, caveman 20 (RTK runs before Caveman
//     in the reference router's order).
//   - open-sse/services/compression/compressionWorker.ts /
//     compressionWorkerPool.ts / compressionWorkerProtocol.ts —
//     the worker-thread pool (see workerpool.go).
//
// Stage order HERE is lossless-first, then RTK, then Caveman:
// the M7.6 modes decision (modes.go ResolveMode, ModeStacked)
// runs the lossless steps first and only then RTK → Caveman.
// That is a DELIBERATE DIVERGENCE from the reference router's RTK →
// Caveman order, and this pipeline matches modes.go — the
// coordinator must not "fix" it back silently. The lossless
// stage reuses the exported Normalise seam from compress.go
// (applyLossless's exported form); no new seam was needed and
// compress.go was not edited.
//
// Safety rules, enforced by RunStacked:
//
//   - A stage that would INFLATE (tokens after > tokens before)
//     is SKIPPED: its output is discarded, the pre-stage
//     messages stand, and the skip is recorded in StageResult.
//     An inflated stage result is never shipped.
//   - A stage whose engine is unavailable under the configured
//     EngineAvailability is SKIPPED with the reason recorded —
//     never run, never faked. Availability comes from modes.go
//     (DefaultAvailability when the config leaves it unset);
//     this file duplicates no availability table.
//   - The whole pipeline result passes through CheckLossy
//     (fidelity, M7.2) and CheckNoInflation (inflation, M7.3).
//     The fidelity gate compares against the post-lossless
//     output (Apply's M7.2 contract: the gate runs on the
//     lossy transform only); the inflation guard compares
//     against the original input. On either refusal the
//     ORIGINAL messages are returned with the refusal
//     recorded — a lost saving, never a corrupted or more
//     expensive prompt.
//   - The input slice is never mutated: every engine already
//     copies, and RunStacked counts and gates on copies.
package compress

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Stage engine ids, in pipeline order. The lossless stage id
// reuses the step names from compress.go so the coordinator can
// merge pipeline accounting with Apply's step list.
const (
	StageLossless = "lossless"    // whitespace + system dedupe + tool-result canonicalisation (Normalise)
	StageRTK      = EngineRTK     // "rtk"
	StageCaveman  = EngineCaveman // "caveman"
)

// defaultStackedModel is the model name used for local BPE token
// counting when StackedConfig leaves Model empty. It only picks
// the encoding (o200k_base for gpt-4o-era names, cl100k_base
// otherwise); both sides of every comparison are counted under
// the SAME encoding, so the comparison stands whatever the name
// is. It is a counting default, not a routing decision.
const defaultStackedModel = "gpt-4o"

// StackedConfig tunes RunStacked. Zero values take the defaults:
// Model "gpt-4o", RTK standard, Caveman full (the reference router's stacked
// default), lossless stage on, availability from
// DefaultAvailability().
type StackedConfig struct {
	// Model is the model name for local BPE token counting.
	// "" means defaultStackedModel.
	Model string
	// RTKLevel is the RTK intensity. "" means RTKLevelStandard.
	RTKLevel RTKLevel
	// CavemanLevel is the Caveman intensity. "" means LevelFull
	// (the reference router's stacked default). Any other value than
	// LevelFull behaves as lite inside the Caveman engine.
	CavemanLevel Level
	// SkipLossless omits the lossless stage (Normalise). False
	// by default: the lossless stage runs first. Set true only
	// to run the RTK → Caveman core on its own (the coordinator
	// composes the pipeline with the M7.6 mode, which already
	// includes lossless). A plain bool cannot default to true,
	// so this is phrased as the opt-out.
	SkipLossless bool
	// Availability names which engines may run. Nil means
	// "unset": DefaultAvailability() from modes.go. A
	// non-nil pointer is used as given — including a pointer
	// to the all-false struct, which means every engine is
	// off. RTK and Caveman are true in DefaultAvailability
	// since the M7 merge wave wired them into Apply.
	Availability *EngineAvailability
}

// DefaultStackedConfig returns the default stacked config:
// the reference router's stacked default (RTK standard, Caveman full),
// lossless first, model default, availability unset (→
// DefaultAvailability()).
func DefaultStackedConfig() StackedConfig {
	return StackedConfig{
		RTKLevel:     RTKLevelStandard,
		CavemanLevel: LevelFull,
	}
}

func (c StackedConfig) effective() StackedConfig {
	if c.Model == "" {
		c.Model = defaultStackedModel
	}
	if c.RTKLevel == "" {
		c.RTKLevel = RTKLevelStandard
	}
	if c.CavemanLevel == "" {
		c.CavemanLevel = LevelFull
	}
	if c.Availability == nil {
		def := DefaultAvailability()
		c.Availability = &def
	}
	return c
}

// StageResult is the per-stage accounting for one pipeline run:
// the engine, its token counts, whether it changed anything, and
// — when it did not run — why not.
type StageResult struct {
	Engine       string
	TokensBefore int
	TokensAfter  int
	Applied      bool   // output differs from stage input
	Skipped      bool   // stage did not run (see SkipReason)
	SkipReason   string // "" when the stage ran
}

// StackedResult is the outcome of RunStacked.
type StackedResult struct {
	Messages []provider.Message
	Stages   []StageResult
	// Applied is true when the returned messages differ from the
	// input messages.
	Applied bool
	// Refused is true when a final gate refused the pipeline
	// result: Messages is then the ORIGINAL input, Applied is
	// false, and RefusalGate/RefusalReason name the gate.
	Refused       bool
	RefusalGate   string // "fidelity", "inflation", or "counting"
	RefusalReason string
	BeforeTokens  int
	AfterTokens   int
}

// countPipelineTokens counts msgs under model, returning -1 and the error
// on failure (a counting failure is never treated as zero).
func countPipelineTokens(model string, msgs []provider.Message) (int, error) {
	c, err := tokencount.CountPrompt(model, msgs)
	if err != nil {
		return -1, err
	}
	if c.Estimated {
		// An estimated (chars/4, over the 4 MiB exact-count cap)
		// count is untrusted for a before/after comparison:
		// treat it as a counting failure so the pipeline fails
		// closed instead of vouching for a saving it cannot
		// prove — the same fail-closed rule CheckNoInflation
		// applies.
		return -1, fmt.Errorf("token count for model %q is estimated (over exact-count cap), refusing to vouch for a saving", model)
	}
	return c.Tokens, nil
}

// applyStage runs one engine stage with inflation protection: it
// counts before/after, and when the stage output would cost MORE
// tokens than its input it discards the output, keeps the input,
// and records the skip. A stage that changes nothing records
// Applied=false and is not a skip. The stage function receives
// the current messages and returns its candidate output; it must
// not mutate its input.
func applyStage(cur []provider.Message, engine, model string, apply func([]provider.Message) []provider.Message) ([]provider.Message, StageResult) {
	sr := StageResult{Engine: engine}
	before, err := countPipelineTokens(model, cur)
	if err != nil {
		// Uncountable input: the stage cannot prove it did not
		// inflate, so it does not run. The caller (RunStacked)
		// turns this into a counting refusal.
		sr.TokensBefore, sr.TokensAfter = -1, -1
		sr.Skipped = true
		sr.SkipReason = fmt.Sprintf("cannot count tokens for stage %q: %v", engine, err)
		return cur, sr
	}
	sr.TokensBefore = before
	out := apply(cur)
	after, err := countPipelineTokens(model, out)
	if err != nil {
		sr.TokensAfter = -1
		sr.Skipped = true
		sr.SkipReason = fmt.Sprintf("cannot count tokens for stage %q output: %v", engine, err)
		return cur, sr
	}
	sr.TokensAfter = after
	if after > before {
		sr.Skipped = true
		sr.SkipReason = fmt.Sprintf("stage %q would inflate: %d tokens after vs %d before; stage output discarded", engine, after, before)
		sr.TokensAfter = before
		return cur, sr
	}
	sr.Applied = !reflect.DeepEqual(cur, out)
	return out, sr
}

// skipStage records a stage that did not run at all (engine
// unavailable, disabled, or invalid configuration) without
// counting anything.
func skipStage(engine, reason string, tokens int) StageResult {
	return StageResult{
		Engine: engine, TokensBefore: tokens, TokensAfter: tokens,
		Skipped: true, SkipReason: reason,
	}
}

// finalGates runs the two whole-pipeline gates on candidate:
// CheckLossy (fidelity) against fidelityOriginal — the
// post-lossless output, per the M7.2 contract that the gate
// runs on the lossy transform only — first, then
// CheckNoInflation (inflation) against inflationOriginal —
// the pipeline's original input, since total cost must not
// exceed the original prompt. Same order as Apply, so a result
// refused by both is reported as a fidelity refusal. It
// returns refused=true plus the gate name and reason on refusal.
// A counting failure inside the inflation guard fails closed
// (refused, gate "inflation").
func finalGates(model string, fidelityOriginal, candidate, inflationOriginal []provider.Message) (refused bool, gate, reason string) {
	if ok, reasons := CheckLossy(fidelityOriginal, candidate); !ok {
		return true, "fidelity", fmt.Sprintf("fidelity gate refused stacked result: %s", strings.Join(reasons, "; "))
	}
	if inflated, before, after := CheckNoInflation(model, inflationOriginal, candidate); inflated {
		return true, "inflation", fmt.Sprintf("inflation guard refused stacked result: %d tokens after vs %d before", after, before)
	}
	return false, "", ""
}

// RunStacked runs the stacked pipeline on msgs: lossless
// (Normalise) first when configured, then RTK, then Caveman.
// Each stage reports its own step and tokens-before/after; a
// stage that would inflate is skipped with the skip recorded; a
// stage whose engine is unavailable is skipped with the reason
// recorded. The whole result passes through the fidelity and
// inflation gates; on either refusal the ORIGINAL messages are
// returned with the refusal recorded. The input slice is never
// mutated. An empty input returns an empty, unapplied result.
func RunStacked(msgs []provider.Message, cfg StackedConfig) StackedResult {
	cfg = cfg.effective()
	orig := make([]provider.Message, len(msgs))
	copy(orig, msgs)
	res := StackedResult{Messages: orig}
	if len(msgs) == 0 {
		return res
	}
	before, err := countPipelineTokens(cfg.Model, orig)
	if err != nil {
		res.Refused = true
		res.RefusalGate = "counting"
		res.RefusalReason = fmt.Sprintf("cannot count input tokens: %v", err)
		res.BeforeTokens, res.AfterTokens = -1, -1
		return res
	}
	res.BeforeTokens = before
	cur := orig
	curTokens := before

	// Stage 1: lossless (Normalise = applyLossless, exported
	// seam in compress.go). Lossless never waits behind a lossy
	// engine — the M7.6 lossless-first rule. The post-lossless
	// output is the fidelity gate's reference input (see below):
	// the M7.2 gate runs on the lossy transform only, so
	// lossless changes (whitespace, dedupe) must never trip it.
	losslessOut := orig
	if !cfg.SkipLossless {
		if cfg.Availability.Lossless {
			var sr StageResult
			cur, sr = applyStage(cur, StageLossless, cfg.Model, func(in []provider.Message) []provider.Message {
				return Normalise(in)
			})
			res.Stages = append(res.Stages, sr)
			if !sr.Skipped {
				curTokens = sr.TokensAfter
			}
		} else {
			res.Stages = append(res.Stages, skipStage(StageLossless, "lossless steps unavailable under configured EngineAvailability", curTokens))
		}
		losslessOut = cur
	}

	// Stage 2: RTK (tool-role messages only, inside the engine).
	if cfg.Availability.RTK {
		if _, ok := ParseRTKLevel(string(cfg.RTKLevel)); !ok {
			res.Stages = append(res.Stages, skipStage(StageRTK, fmt.Sprintf("invalid RTK level %q", string(cfg.RTKLevel)), curTokens))
		} else {
			level := cfg.RTKLevel
			var sr StageResult
			cur, sr = applyStage(cur, StageRTK, cfg.Model, func(in []provider.Message) []provider.Message {
				out, _ := FilterRTKMessages(in, level)
				return out
			})
			res.Stages = append(res.Stages, sr)
			if !sr.Skipped {
				curTokens = sr.TokensAfter
			}
		}
	} else {
		res.Stages = append(res.Stages, skipStage(StageRTK, "RTK engine unavailable under configured EngineAvailability", curTokens))
	}

	// Stage 3: Caveman (user-role messages by default, inside
	// the engine).
	if cfg.Availability.Caveman {
		level := cfg.CavemanLevel
		var sr StageResult
		cur, sr = applyStage(cur, StageCaveman, cfg.Model, func(in []provider.Message) []provider.Message {
			return CondenseMessages(in, level).Messages
		})
		res.Stages = append(res.Stages, sr)
		if !sr.Skipped {
			curTokens = sr.TokensAfter
		}
	} else {
		res.Stages = append(res.Stages, skipStage(StageCaveman, "Caveman engine unavailable under configured EngineAvailability", curTokens))
	}

	// Whole-pipeline gates, in Apply's order: fidelity first,
	// then inflation. The fidelity gate compares the final
	// result against the POST-LOSSLESS output (the M7.2
	// contract: the gate runs on the lossy transform only, so
	// a lossless whitespace/dedupe change can never trip it —
	// e.g. stripping trailing spaces inside a pretty-printed
	// JSON value changes the value's bytes without cutting it,
	// which the gate would otherwise read as a cut). The
	// inflation guard compares against the ORIGINAL input
	// (total cost must not exceed the original prompt). On
	// either refusal the ORIGINAL messages are returned.
	if refused, gate, reason := finalGates(cfg.Model, losslessOut, cur, orig); refused {
		return StackedResult{
			Messages: orig, Stages: res.Stages,
			Refused: true, RefusalGate: gate, RefusalReason: reason,
			BeforeTokens: before, AfterTokens: before,
		}
	}

	res.Messages = cur
	res.AfterTokens = curTokens
	res.Applied = !reflect.DeepEqual(orig, cur)
	return res
}
