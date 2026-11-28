// Package compress — M7.6 named modes / auto-trigger / adaptive budget
// dial (parity tracker Area 10, "Named modes/presets + auto-trigger" and
// "Adaptive budget dial + escalation ladder", both MISSING).
//
// This file is PURE DECISION LOGIC: it maps a named mode to an ordered
// list of engine steps, picks a mode from prompt size, and walks the
// escalation ladder against a token budget. It applies NO compression
// itself and touches no messages: the coordinator wires ResolveMode
// into Decide/Apply and the config at merge time. Nothing here calls
// a model, does network I/O, or reads config files.
//
// What the sector baseline does (studied, never copied — see the file list in
// the M7.6 report): modes off/lite/standard/aggressive/ultra/rtk/
// stacked in docs/compression/COMPRESSION_GUIDE.md:35-126; the
// auto-trigger fields autoTriggerMode/autoTriggerTokens (guide
// §"API", e.g. autoTriggerMode "stacked", autoTriggerTokens 32000);
// the default escalation ladder in
// the earlier implementation
// (session-dedup → rtk → headroom → lite → caveman → aggressive →
// ultra) with per-engine expected reduction factors. Our shapes are
// our own; only the mode names and the ladder ORDER are taken from
// the sector baseline, because parity tracker names them as the parity target.
//
// Honesty rules enforced by this file:
//
//   - A mode step naming an engine that does not exist in this repo
//     is listed with Available=false and a reason — never silently
//     dropped, never faked as working. A mode whose steps are not
//     all available reports Available=false and names them in
//     Unavailable, so a caller can see the gap instead of assuming
//     the mode ran.
//   - Engines that exist today: the three lossless steps in
//     compress.go (whitespace strip, system dedupe, tool-result
//     canonicalisation), the extractive lossy steps (whole-turn
//     drop, head+tail truncate), RTK (M7.4, landed), Caveman
//     (M7.5, landed), message aging (B4-31, Wave 12) and
//     heuristic pruning (B4-33, Wave 12). Everything else is
//     unavailable: session-dedup (store exists, flag off by
//     default), headroom (no tabular engine wired),
//     summarisation (aggressive's fallback; there is no
//     summariser model in-router by design) and the SLM tier
//     (ultra's optional tier, not implemented).
//   - Estimates are labelled by source. The lite factor (0.902, i.e.
//     9.8% saved) is MEASURED: the M7.1 corpus total in
//     docs/compression-savings-2026-11-18.md (1200 → 1082 tokens,
//     gpt-4o, local BPE counts). Per-prompt savings vary widely
//     (0–52% in that corpus), so it is a planning estimate, never a
//     per-request promise. Every other factor is the sector-typical
//     PUBLISHED planning factor (ladder.ts REDUCTION_FACTOR / the
//     guide's "~30%" for standard), labelled as such and NEVER
//     presented as measured here — those engines do not exist yet,
//     so no measurement is possible. The stacked factor is derived,
//     not invented: rtk 0.85 × caveman 0.70 = 0.595.
//   - The dial reports Fit as an ESTIMATE (DialResult.Fit, with
//     EstimateSource naming the basis). It never claims a fit that
//     did not happen: when no available mode's estimate fits, it
//     stops at the strongest available mode and reports Fit=false.
//     The real fit is proven only by running Apply (M7.7 hard-budget
//     post-pass owns the honest check); lossy modes trim TOWARD a
//     budget and pinned messages alone can exceed it.
package compress

import (
	"fmt"
	"strings"
)

// Mode is a named compression preset. The names match the sector-typical
// modes (COMPRESSION_GUIDE.md "Compression Modes") so the parity
// target is spelled the same on both sides.
type Mode string

const (
	ModeOff        Mode = "off"
	ModeLite       Mode = "lite"
	ModeStandard   Mode = "standard"
	ModeAggressive Mode = "aggressive"
	ModeUltra      Mode = "ultra"
	ModeRTK        Mode = "rtk"
	ModeStacked    Mode = "stacked"
)

// AllModes lists every named mode in strength order (weakest first),
// which is also the order the budget dial walks. RTK is NOT in this
// order: it is orthogonal (tool-output filtering), selected by the
// auto-trigger for tool-heavy prompts, not by strength. Stacked is
// strongest because it runs the most engines.
var AllModes = []Mode{ModeOff, ModeLite, ModeStandard, ModeAggressive, ModeUltra, ModeStacked}

// modeRank is the strength order index; -1 for unknown (RTK is
// deliberately unranked — see AllModes).
func modeRank(m Mode) int {
	for i, v := range AllModes {
		if v == m {
			return i
		}
	}
	return -1
}

// ParseMode normalises a mode name (case-insensitive, surrounding
// whitespace ignored). Unknown names are an error that lists the
// valid modes — an unknown mode must never silently become "off"
// or "lite".
func ParseMode(s string) (Mode, error) {
	norm := Mode(strings.ToLower(strings.TrimSpace(s)))
	switch norm {
	case ModeOff, ModeLite, ModeStandard, ModeAggressive, ModeUltra, ModeRTK, ModeStacked:
		return norm, nil
	default:
		return "", fmt.Errorf("unknown compression mode %q (valid: off, lite, standard, aggressive, ultra, rtk, stacked)", s)
	}
}

// Engine ids for steps whose engine does not exist yet. Existing
// steps reuse the Step* constants from compress.go.
const (
	EngineLite           = "lite"
	EngineSessionDedup   = "session-dedup"
	EngineRTK            = "rtk"
	EngineHeadroom       = "headroom"
	EngineCaveman        = "caveman"
	EngineMessageAging   = "message-aging"
	EngineSummarization  = "summarization"
	EngineHeuristicPrune = "heuristic-pruning"
	EngineSLMTier        = "slm-tier"
)

// EngineAvailability records which compression engines exist in
// this build. DefaultAvailability is today's truth; callers (and
// tests) may construct an override to model a build where an
// engine is switched off — the mapping logic keys off this value,
// never off a hardcoded assumption that an engine exists.
type EngineAvailability struct {
	Lossless          bool // whitespace strip + system dedupe + tool-result canonicalisation (compress.go)
	LossyDropTruncate bool // extractive lossy drop/truncate (compress.go applyLossy)
	RTK               bool // M7.4 RTK tool-output filter
	Caveman           bool // M7.5 Caveman-lite prose condensation
	SessionDedup      bool // cross-turn session dedup (no store exists)
	Headroom          bool // tabular JSON compaction
	MessageAging      bool // aggressive: progressive compression of older turns
	Summarization     bool // aggressive: fallback summariser (none in-router by design)
	HeuristicPruning  bool // ultra: score-based token pruning
	SLMTier           bool // ultra: optional small-local-model refine tier
}

// DefaultAvailability returns the engines that exist in this repo
// today: the lossless steps and the extractive lossy steps from
// compress.go, plus the M7.4 RTK tool-output filter, the M7.5
// Caveman-lite prose condensation (both landed and wired into
// Apply by the M7 merge wave), the Wave 12 heuristic pruning
// (B4-33) and progressive aging (B4-31) engines. Still false:
// session-dedup (store exists, flag off by default), headroom
// (no tabular engine wired), summarisation (no summariser model
// runs in-router by design), SLM tier (not implemented).
func DefaultAvailability() EngineAvailability {
	return EngineAvailability{
		Lossless:          true,
		LossyDropTruncate: true,
		RTK:               true,
		Caveman:           true,
		SessionDedup:      false,
		Headroom:          false,
		MessageAging:      true,
		Summarization:     false,
		HeuristicPruning:  true,
		SLMTier:           false,
	}
}

// engineAvailable reports whether one engine step can run under
// this availability. The lossless step ids share one flag because
// they ship as one unit in compress.go.
func (a EngineAvailability) engineAvailable(engine string) bool {
	switch engine {
	case StepWhitespace, StepDedupeSystem, StepToolResults, EngineLite:
		return a.Lossless
	case StepLossyDropped, StepLossyTruncated:
		return a.LossyDropTruncate
	case EngineRTK:
		return a.RTK
	case EngineCaveman:
		return a.Caveman
	case EngineSessionDedup:
		return a.SessionDedup
	case EngineHeadroom:
		return a.Headroom
	case EngineMessageAging:
		return a.MessageAging
	case EngineSummarization:
		return a.Summarization
	case EngineHeuristicPrune:
		return a.HeuristicPruning
	case EngineSLMTier:
		return a.SLMTier
	default:
		return false
	}
}

// unavailableReason explains, for one engine, why it cannot run.
// It is only meaningful when engineAvailable is false; available
// engines return "".
func (a EngineAvailability) unavailableReason(engine string) string {
	if a.engineAvailable(engine) {
		return ""
	}
	switch engine {
	case StepWhitespace, StepDedupeSystem, StepToolResults, EngineLite:
		return "lossless steps not available in this build"
	case StepLossyDropped, StepLossyTruncated:
		return "extractive lossy drop/truncate not available in this build"
	case EngineRTK:
		return "RTK tool-output filter not available under this availability"
	case EngineCaveman:
		return "Caveman-lite prose condensation not available under this availability"
	case EngineSessionDedup:
		return "no cross-turn session store: content-addressed dedup across turns not built"
	case EngineHeadroom:
		return "no tabular JSON compaction engine"
	case EngineMessageAging:
		return "message aging (progressive compression of older turns) not implemented"
	case EngineSummarization:
		return "no summariser: aggressive's fallback summarizer does not exist and no summariser model runs in-router by design"
	case EngineHeuristicPrune:
		return "heuristic token pruning not implemented"
	case EngineSLMTier:
		return "optional small-local-model refine tier not implemented"
	default:
		return fmt.Sprintf("unknown engine %q", engine)
	}
}

// ModeStep is one engine in a mode's pipeline, in application
// order. BudgetTokens is the per-step token budget: the maximum
// tokens the step may leave behind. 0 means uncapped — the step
// runs to completion. Only the lossy drop/truncate steps take a
// budget (the caller's token budget, passed to ResolveMode);
// lossless steps only remove redundancy, so they are never capped.
type ModeStep struct {
	Engine            string
	Available         bool
	UnavailableReason string // "" when Available
	Lossy             bool   // true when the step may drop or rewrite content
	BudgetTokens      int    // 0 = uncapped
}

// ModePipeline is a mode resolved to its ordered steps.
type ModePipeline struct {
	Mode        Mode
	Steps       []ModeStep
	Available   bool     // true only when every step is available
	Unavailable []string // engine ids of unavailable steps, in pipeline order
	Lossy       bool     // true when any step is lossy
	// RequiresLossyOptIn is true when the mode may change meaning
	// (any lossy step, including Caveman prose rewriting and RTK
	// output filtering). Such a mode must never run without the
	// per-request lossy opt-in — the same rule compress.go enforces
	// for lossy_max_prompt_tokens.
	RequiresLossyOptIn bool
}

// step builds a ModeStep for one engine under an availability,
// attaching the budget only to lossy drop/truncate steps.
func buildStep(engine string, budgetTokens int, avail EngineAvailability) ModeStep {
	lossy := engine == StepLossyDropped || engine == StepLossyTruncated ||
		engine == EngineRTK || engine == EngineCaveman ||
		engine == EngineMessageAging || engine == EngineSummarization ||
		engine == EngineHeuristicPrune || engine == EngineSLMTier
	budget := 0
	if (engine == StepLossyDropped || engine == StepLossyTruncated) && budgetTokens > 0 {
		budget = budgetTokens
	}
	return ModeStep{
		Engine:            engine,
		Available:         avail.engineAvailable(engine),
		UnavailableReason: avail.unavailableReason(engine),
		Lossy:             lossy,
		BudgetTokens:      budget,
	}
}

// losslessSteps is the lite prefix every non-off mode starts from:
// whitespace strip, system dedupe, tool-result canonicalisation,
// in Apply's order (compress.go applyLossless).
func losslessSteps(avail EngineAvailability) []ModeStep {
	return []ModeStep{
		buildStep(StepWhitespace, 0, avail),
		buildStep(StepDedupeSystem, 0, avail),
		buildStep(StepToolResults, 0, avail),
	}
}

// lossySteps is the extractive lossy pair (drop whole turns, then
// head+tail truncate) that aggressive/ultra/stacked fall back on
// until their own engines land. It is the honest subset: it trims,
// it does not age, summarise, or prune.
func lossySteps(budgetTokens int, avail EngineAvailability) []ModeStep {
	return []ModeStep{
		buildStep(StepLossyDropped, budgetTokens, avail),
		buildStep(StepLossyTruncated, budgetTokens, avail),
	}
}

// ResolveMode maps a named mode to its ordered pipeline steps.
// budgetTokens (0 = no target) is attached as the per-step budget
// of the lossy drop/truncate steps; negative budgets are an error.
// NOTE: budget 0 means UNCAPPED here — unlike the dials
// (PickCheapestFitting/EscalateFrom), which reject budgetTokens
// <= 0; do not pass a dial budget into ResolveMode expecting the
// same meaning.
// Unknown modes are an error. The returned pipeline ALWAYS lists
// every step the mode nominally runs, including unavailable ones —
// a caller decides what to do with an unavailable mode (refuse,
// fall back, or escalate) from Pipeline.Available/Unavailable,
// never from a silently shortened list.
func ResolveMode(mode Mode, budgetTokens int, avail EngineAvailability) (ModePipeline, error) {
	if budgetTokens < 0 {
		return ModePipeline{}, fmt.Errorf("budgetTokens %d must be >= 0", budgetTokens)
	}
	var steps []ModeStep
	switch mode {
	case ModeOff:
		steps = nil
	case ModeLite:
		// the sector baseline lite: collapseWhitespace, dedupSystemPrompt,
		// compressToolResults (+ removeRedundantContent and
		// replaceImageUrls, which have no equivalent here and are
		// therefore NOT listed — listing them would fake them).
		steps = losslessSteps(avail)
	case ModeStandard:
		// the sector baseline standard IS Caveman (filler removal). The lite
		// prefix runs first (lossless-first rule), then Caveman.
		steps = append(losslessSteps(avail), buildStep(EngineCaveman, 0, avail))
	case ModeAggressive:
		// the sector baseline aggressive: message aging + tool-result
		// compression + summarisation. Ours today: lite prefix,
		// then the extractive lossy pair, then message aging
		// (landed, Wave 12) and the summariser marked
		// unavailable — the mode is NOT Available until the
		// summariser question is settled (no summariser model
		// runs in-router by design).
		steps = append(losslessSteps(avail), lossySteps(budgetTokens, avail)...)
		steps = append(steps,
			buildStep(EngineMessageAging, 0, avail),
			buildStep(EngineSummarization, 0, avail))
	case ModeUltra:
		// the sector baseline ultra: heuristic pruning (+ optional SLM tier),
		// independent of aggressive (no aging, no summariser).
		// Ours today: lite prefix, extractive lossy pair, then
		// heuristic pruning (landed, Wave 12) and the SLM tier
		// marked unavailable.
		steps = append(losslessSteps(avail), lossySteps(budgetTokens, avail)...)
		steps = append(steps,
			buildStep(EngineHeuristicPrune, 0, avail),
			buildStep(EngineSLMTier, 0, avail))
	case ModeRTK:
		// RTK mode is exactly the RTK engine (command-output
		// filtering). It has no lite prefix: on a tool-output
		// prompt the filter IS the mode. Available since M7.4.
		steps = []ModeStep{buildStep(EngineRTK, 0, avail)}
	case ModeStacked:
		// the sector baseline stacked default is RTK → Caveman. Ours runs
		// the lossless steps FIRST (the lossless-first rule in
		// compress.go — lossless never waits behind a lossy
		// engine), then RTK, then Caveman. Deliberate divergence
		// from the sector-typical order, documented here so the
		// coordinator does not "fix" it back silently.
		steps = append(losslessSteps(avail), lossySteps(budgetTokens, avail)...)
		steps = append(steps,
			buildStep(EngineRTK, 0, avail),
			buildStep(EngineCaveman, 0, avail))
	default:
		return ModePipeline{}, fmt.Errorf("unknown compression mode %q (valid: off, lite, standard, aggressive, ultra, rtk, stacked)", string(mode))
	}
	p := ModePipeline{Mode: mode, Steps: steps, Available: true}
	for _, s := range steps {
		if s.Lossy {
			p.Lossy = true
			p.RequiresLossyOptIn = true
		}
		if !s.Available {
			p.Available = false
			p.Unavailable = append(p.Unavailable, s.Engine)
		}
	}
	return p, nil
}

// LadderStep is one stage of the escalation ladder, in the sector-typical
// order (adaptiveCompression/ladder.ts DEFAULT_LADDER):
// session-dedup → rtk → headroom → lite → caveman → aggressive →
// ultra. Intensity mirrors the sector-typical per-stage intensity where
// one is defined (rtk "standard", caveman "full"); empty elsewhere.
type LadderStep struct {
	Engine            string
	Intensity         string
	Available         bool
	UnavailableReason string // "" when Available
}

// EscalationLadder returns the full ladder with live availability
// per stage. Stages naming engines that do not exist are present
// with Available=false and a reason — the ladder never silently
// skips them; the dial records each skipped stage explicitly.
func EscalationLadder(avail EngineAvailability) []LadderStep {
	engines := []struct {
		engine    string
		intensity string
	}{
		{EngineSessionDedup, ""},
		{EngineRTK, "standard"},
		{EngineHeadroom, ""},
		{EngineLite, ""}, // "lite" stage: the lite pipeline (whitespace + dedupe + tool-results)
		{EngineCaveman, "full"},
		{EngineMessageAging, ""},   // "aggressive" stage: aging is its defining step
		{EngineHeuristicPrune, ""}, // "ultra" stage: heuristic pruning is its defining step
	}
	// NOTE on the "lite" stage: the stage is named "lite" directly
	// and is Available exactly when the lossless steps are. The
	// dial works on MODES, not ladder engines, so this naming does
	// not leak into mode resolution.
	out := make([]LadderStep, 0, len(engines))
	for _, e := range engines {
		out = append(out, LadderStep{
			Engine:            e.engine,
			Intensity:         e.intensity,
			Available:         avail.engineAvailable(e.engine),
			UnavailableReason: avail.unavailableReason(e.engine),
		})
	}
	return out
}

// AutoTriggerConfig tunes the auto-trigger. Zero values take the
// defaults (DefaultAutoTriggerConfig).
type AutoTriggerConfig struct {
	// ThresholdTokens is the prompt size at which compression
	// engages at all. Below it, AutoTrigger returns ModeOff.
	ThresholdTokens int
	// RTKToolShare is the fraction of prompt tokens that must be
	// tool output for a tool-heavy prompt to select RTK/stacked
	// instead of the size-based mode (0.5 = half the prompt).
	RTKToolShare float64
}

// DefaultAutoTriggerConfig returns the default auto-trigger tuning.
// ThresholdTokens 8000 is a CHOSEN DEFAULT (an operator default,
// not a measurement): small enough to engage on long sessions,
// large enough that ordinary prompts pass through untouched.
// the sector-typical documented example uses autoTriggerTokens 32000;
// ours defaults lower because lite is safe to engage earlier.
func DefaultAutoTriggerConfig() AutoTriggerConfig {
	return AutoTriggerConfig{ThresholdTokens: 8000, RTKToolShare: 0.5}
}

func (c AutoTriggerConfig) effective() AutoTriggerConfig {
	def := DefaultAutoTriggerConfig()
	if c.ThresholdTokens <= 0 {
		c.ThresholdTokens = def.ThresholdTokens
	}
	if c.RTKToolShare <= 0 || c.RTKToolShare > 1 {
		c.RTKToolShare = def.RTKToolShare
	}
	return c
}

// AutoTriggerInput is what the auto-trigger decides from: token
// counts only. No model calls, no content inspection — the decision
// is a deterministic function of size.
type AutoTriggerInput struct {
	PromptTokens int // total prompt tokens (local estimate)
	ToolTokens   int // tokens in tool-role messages (0 when none / unknown)
}

// AutoTrigger selects a mode from prompt size (and tool share).
// It returns the mode plus a human-readable reason naming the rule
// that fired. The caller MUST check ResolveMode(mode).Available
// before running it: AutoTrigger can select a mode whose engine
// has not landed yet (e.g. RTK for a tool-heavy prompt), and that
// is reported by the pipeline, not hidden by falling back here.
// Rules, in order (threshold T = cfg.ThresholdTokens):
//
//	empty prompt (<= 0 tokens)            -> off
//	prompt < T                             -> off (under threshold)
//	tool-heavy (>= RTKToolShare of prompt) and prompt >= 8T -> stacked
//	tool-heavy and prompt >= 2T            -> rtk
//	prompt >= 8T                           -> ultra
//	prompt >= 4T                           -> aggressive
//	prompt >= 2T                           -> standard
//	prompt >= T                            -> lite
func AutoTrigger(in AutoTriggerInput, cfg AutoTriggerConfig) (Mode, string) {
	cfg = cfg.effective()
	if in.PromptTokens <= 0 {
		return ModeOff, "empty prompt: no compression"
	}
	t := cfg.ThresholdTokens
	if in.PromptTokens < t {
		return ModeOff, fmt.Sprintf("prompt %d tokens under auto-trigger threshold %d", in.PromptTokens, t)
	}
	toolHeavy := in.ToolTokens > 0 && float64(in.ToolTokens) >= cfg.RTKToolShare*float64(in.PromptTokens)
	if toolHeavy && in.PromptTokens >= 8*t {
		return ModeStacked, fmt.Sprintf("prompt %d tokens (>= 8x threshold %d) and tool-heavy (%d tool tokens): stacked", in.PromptTokens, t, in.ToolTokens)
	}
	if toolHeavy && in.PromptTokens >= 2*t {
		return ModeRTK, fmt.Sprintf("prompt %d tokens (>= 2x threshold %d) and tool-heavy (%d tool tokens): rtk", in.PromptTokens, t, in.ToolTokens)
	}
	switch {
	case in.PromptTokens >= 8*t:
		return ModeUltra, fmt.Sprintf("prompt %d tokens >= 8x threshold %d: ultra", in.PromptTokens, t)
	case in.PromptTokens >= 4*t:
		return ModeAggressive, fmt.Sprintf("prompt %d tokens >= 4x threshold %d: aggressive", in.PromptTokens, t)
	case in.PromptTokens >= 2*t:
		return ModeStandard, fmt.Sprintf("prompt %d tokens >= 2x threshold %d: standard", in.PromptTokens, t)
	default:
		return ModeLite, fmt.Sprintf("prompt %d tokens >= threshold %d: lite", in.PromptTokens, t)
	}
}

// Estimate sources: what an EstimateAfterTokens number is based on.
const (
	// EstimateUnknown is an unvalidated mode reaching the estimator
	// without going through ParseMode/ResolveMode: the factor is 1.0
	// but the source says so, instead of silently claiming
	// "exact-no-compression".
	EstimateUnknown = "unknown-mode"
	// EstimateMeasured is the M7.1 corpus measurement (lite only).
	EstimateMeasured = "measured-m7.1-corpus"
	// EstimatePublishedPlanningFactor is the sector-typical published planning
	// factor for an engine that does not exist here yet — a
	// planning number, never a measurement.
	EstimatePublishedPlanningFactor = "published-planning-factor"
	// EstimateDerived is a factor derived arithmetically from two
	// published factors (stacked = rtk x caveman).
	EstimateDerived = "derived-from-published-factors"
	// EstimateExact is no compression at all (off): the estimate
	// IS the prompt size.
	EstimateExact = "exact-no-compression"
)

// estimatedFactor returns the planning factor (output/input token
// ratio) for a mode and the source label for that factor. The lite
// factor 0.902 is MEASURED (M7.1 corpus total: 1200 -> 1082
// tokens, gpt-4o). All other factors are reference-published
// (ladder.ts REDUCTION_FACTOR: rtk 0.85, caveman 0.70, aggressive
// 0.55, ultra 0.40; guide "~30%" for standard == caveman 0.70).
// Stacked 0.595 is rtk 0.85 x caveman 0.70, derived.
// NOTE: aggressive 0.55 / ultra 0.40 are the LADDER values; the
// guide disagrees (aggressive "~50%", ultra "~75%") — the ladder
// wins because it is the executable source, not prose.
func estimatedFactor(mode Mode) (float64, string) {
	switch mode {
	case ModeOff:
		return 1.0, EstimateExact
	case ModeLite:
		return 0.902, EstimateMeasured
	case ModeStandard:
		return 0.70, EstimatePublishedPlanningFactor
	case ModeAggressive:
		return 0.55, EstimatePublishedPlanningFactor
	case ModeUltra:
		return 0.40, EstimatePublishedPlanningFactor
	case ModeRTK:
		return 0.85, EstimatePublishedPlanningFactor
	case ModeStacked:
		return 0.595, EstimateDerived
	default:
		return 1.0, EstimateUnknown
	}
}

// EstimateAfterTokens estimates the prompt size after a mode runs,
// as promptTokens * the mode's factor (integer truncation). It is
// a PLANNING estimate: use EstimateSource to say which kind. The
// estimate never claims a fit — DialResult.Fit is the estimate
// compared against the budget, and the real fit is proven only by
// running Apply.
func EstimateAfterTokens(mode Mode, promptTokens int) (after int, source string) {
	if promptTokens <= 0 {
		return 0, EstimateExact
	}
	factor, source := estimatedFactor(mode)
	return int(float64(promptTokens) * factor), source
}

// Escalation records one step up the strength order while chasing
// a budget fit.
type Escalation struct {
	From   Mode
	To     Mode
	Reason string // why the previous mode was left (estimate vs budget).
	// An entry can also record a FALLBACK to a weaker mode when the
	// start mode is unavailable — read Reason, not just From/To,
	// to tell an escalation from a fallback.
}

// ModeSkip records a mode that was passed over because it is not
// available in this build — with the reason, so the skip is
// explicit in the result, never silent.
type ModeSkip struct {
	Mode   Mode
	Reason string // which engine(s) are missing
}

// DialResult is the outcome of the adaptive budget dial.
type DialResult struct {
	StartMode            Mode
	SelectedMode         Mode
	PromptTokens         int
	BudgetTokens         int
	EstimatedAfterTokens int
	EstimateSource       string
	// Fit is an ESTIMATED fit (estimate <= budget at SelectedMode).
	// False means no available mode's estimate fits: the prompt is
	// sent at the strongest available mode and the overage is
	// reported honestly, never hidden.
	Fit bool
	// Escalations lists every step up the strength order, in order.
	Escalations []Escalation
	// SkippedUnavailable lists every unavailable mode passed over
	// while escalating, with the missing engine(s).
	SkippedUnavailable []ModeSkip
	// StrongestAvailable is the strongest mode whose pipeline is
	// fully available in this build (ModeOff when nothing is).
	StrongestAvailable Mode
}

// strongestAvailableMode returns the strongest fully-available
// mode in strength order (RTK excluded — orthogonal, not ranked).
func strongestAvailableMode(avail EngineAvailability) Mode {
	strongest := ModeOff
	for _, m := range AllModes {
		p, err := ResolveMode(m, 0, avail)
		if err != nil {
			continue
		}
		if p.Available {
			strongest = m
		}
	}
	return strongest
}

// recordSkip appends a mode to SkippedUnavailable unless it is
// already recorded — the escalation walk scans the same rungs from
// more than one place, and a duplicate skip would be noise.
func (r *DialResult) recordSkip(m Mode, reason string) {
	for _, s := range r.SkippedUnavailable {
		if s.Mode == m {
			return
		}
	}
	r.SkippedUnavailable = append(r.SkippedUnavailable, ModeSkip{Mode: m, Reason: reason})
}

// unavailableModeReason joins the missing engines of a mode's
// pipeline into one reason string.
func unavailableModeReason(p ModePipeline) string {
	if p.Available {
		return ""
	}
	return fmt.Sprintf("mode %q unavailable: missing engine(s) %s", string(p.Mode), strings.Join(p.Unavailable, ", "))
}

// PickCheapestFitting implements the adaptive budget dial: given a
// token budget, pick the CHEAPEST (weakest) available mode whose
// estimated output fits the budget. It scans the strength order
// from off upward. Modes that are unavailable are recorded in
// SkippedUnavailable, not silently skipped. When no available
// mode's estimate fits, it selects the strongest available mode
// with Fit=false and the overage visible in EstimatedAfterTokens.
// Errors: promptTokens < 0, budgetTokens <= 0 (a dial needs a
// budget).
func PickCheapestFitting(promptTokens, budgetTokens int, avail EngineAvailability) (DialResult, error) {
	if promptTokens < 0 {
		return DialResult{}, fmt.Errorf("promptTokens %d must be >= 0", promptTokens)
	}
	if budgetTokens <= 0 {
		return DialResult{}, fmt.Errorf("budgetTokens %d must be > 0", budgetTokens)
	}
	res := DialResult{
		StartMode:          ModeOff,
		PromptTokens:       promptTokens,
		BudgetTokens:       budgetTokens,
		StrongestAvailable: strongestAvailableMode(avail),
	}
	// Already fits with no compression at all: cheapest is off.
	if promptTokens <= budgetTokens {
		res.SelectedMode = ModeOff
		res.EstimatedAfterTokens = promptTokens
		res.EstimateSource = EstimateExact
		res.Fit = true
		return res, nil
	}
	for _, m := range AllModes {
		p, err := ResolveMode(m, budgetTokens, avail)
		if err != nil {
			continue
		}
		if !p.Available {
			res.recordSkip(m, unavailableModeReason(p))
			continue
		}
		after, source := EstimateAfterTokens(m, promptTokens)
		if after <= budgetTokens {
			res.SelectedMode = m
			res.EstimatedAfterTokens = after
			res.EstimateSource = source
			res.Fit = true
			return res, nil
		}
	}
	// Nothing available fits: strongest available, honest miss.
	res.SelectedMode = res.StrongestAvailable
	after, source := EstimateAfterTokens(res.SelectedMode, promptTokens)
	res.EstimatedAfterTokens = after
	res.EstimateSource = source
	res.Fit = false
	return res, nil
}

// EscalateFrom implements the escalation ladder from a starting
// mode: while the current mode's estimate does not fit the budget,
// step to the next stronger AVAILABLE mode, recording each step.
// Unavailable modes on the way up are recorded in
// SkippedUnavailable with their reasons. It stops at the strongest
// available mode — never loops, never claims a fit that did not
// happen (Fit=false when the strongest available estimate still
// exceeds the budget). When the prompt already fits at the start
// mode, no escalation happens. When the start mode itself is
// unavailable, that is recorded as a skip and escalation begins
// from the next stronger available mode (falling back to the
// strongest available mode overall when no stronger one exists).
// Errors: unknown start mode, promptTokens < 0, budgetTokens <= 0.
func EscalateFrom(start Mode, promptTokens, budgetTokens int, avail EngineAvailability) (DialResult, error) {
	if _, err := ParseMode(string(start)); err != nil {
		return DialResult{}, err
	}
	if promptTokens < 0 {
		return DialResult{}, fmt.Errorf("promptTokens %d must be >= 0", promptTokens)
	}
	if budgetTokens <= 0 {
		return DialResult{}, fmt.Errorf("budgetTokens %d must be > 0", budgetTokens)
	}
	res := DialResult{
		StartMode:          start,
		PromptTokens:       promptTokens,
		BudgetTokens:       budgetTokens,
		StrongestAvailable: strongestAvailableMode(avail),
	}
	// Already fits with no compression: stay at the start mode
	// only if it is available; otherwise the cheapest honest
	// answer is off (it fits and is available).
	if promptTokens <= budgetTokens {
		if p, _ := ResolveMode(start, budgetTokens, avail); p.Available {
			res.SelectedMode = start
		} else {
			res.SelectedMode = ModeOff
			res.recordSkip(start, unavailableModeReason(p))
		}
		res.EstimatedAfterTokens = promptTokens
		res.EstimateSource = EstimateExact
		res.Fit = true
		return res, nil
	}

	startRank := modeRank(start)
	// RTK has no rank: it cannot be a rung on the strength ladder.
	// Treat an RTK start as "escalate from the strongest mode at
	// or below the tool-output tier" — in practice, begin the walk
	// at lite (the weakest lossy-capable rung is standard, but
	// lite is the weakest available compression rung) and record
	// the RTK start as skipped-with-reason when unavailable, or
	// as the current mode when available.
	current := start
	if start == ModeRTK {
		if p, _ := ResolveMode(ModeRTK, budgetTokens, avail); p.Available {
			after, source := EstimateAfterTokens(ModeRTK, promptTokens)
			if after <= budgetTokens {
				res.SelectedMode = ModeRTK
				res.EstimatedAfterTokens = after
				res.EstimateSource = source
				res.Fit = true
				return res, nil
			}
			// RTK available but does not fit: escalate past it
			// to stacked (its only stronger peer for tool output).
			res.Escalations = append(res.Escalations, Escalation{
				From:   ModeRTK,
				To:     ModeStacked,
				Reason: fmt.Sprintf("rtk estimate %d tokens exceeds budget %d", after, budgetTokens),
			})
			current = ModeStacked
			startRank = modeRank(ModeStacked)
		} else {
			res.recordSkip(ModeRTK, unavailableModeReason(p))
			// Record the fallback as an escalation entry (same
			// shape as the generic unavailable-start path below):
			// the RTK start was left for lite, which is weaker.
			res.Escalations = append(res.Escalations, Escalation{
				From:   ModeRTK,
				To:     ModeLite,
				Reason: fmt.Sprintf("start mode %q unavailable (%s); fell back to weakest available compression rung %q", string(ModeRTK), unavailableModeReason(p), string(ModeLite)),
			})
			current = ModeLite
			startRank = modeRank(ModeLite)
		}
	}

	// If the start mode is unavailable, record the skip and begin
	// at the next stronger available mode.
	if p, _ := ResolveMode(current, budgetTokens, avail); !p.Available {
		res.recordSkip(current, unavailableModeReason(p))
		next := ModeOff
		found := false
		for _, m := range AllModes {
			if modeRank(m) <= startRank {
				continue
			}
			if q, _ := ResolveMode(m, budgetTokens, avail); q.Available {
				next = m
				found = true
				break
			} else {
				res.recordSkip(m, unavailableModeReason(q))
			}
		}
		if !found {
			// No stronger available mode: fall back to the
			// strongest available mode overall (weaker than the
			// start) and report the fallback honestly.
			strongest := res.StrongestAvailable
			res.Escalations = append(res.Escalations, Escalation{
				From:   current,
				To:     strongest,
				Reason: fmt.Sprintf("start mode %q unavailable and no stronger available mode exists; fell back to strongest available %q", string(current), string(strongest)),
			})
			current = strongest
		} else {
			res.Escalations = append(res.Escalations, Escalation{
				From:   current,
				To:     next,
				Reason: fmt.Sprintf("start mode %q unavailable (%s); escalated to next available mode %q", string(current), unavailableModeReason(p), string(next)),
			})
			current = next
		}
	}

	// Walk up while the current mode's estimate does not fit.
	for {
		after, source := EstimateAfterTokens(current, promptTokens)
		if after <= budgetTokens {
			res.SelectedMode = current
			res.EstimatedAfterTokens = after
			res.EstimateSource = source
			res.Fit = true
			return res, nil
		}
		// Find the next stronger available mode.
		next := ModeOff
		found := false
		for _, m := range AllModes {
			if modeRank(m) <= modeRank(current) {
				continue
			}
			q, _ := ResolveMode(m, budgetTokens, avail)
			if q.Available {
				next = m
				found = true
				break
			}
			res.recordSkip(m, unavailableModeReason(q))
		}
		if !found {
			res.SelectedMode = current
			res.EstimatedAfterTokens = after
			res.EstimateSource = source
			res.Fit = false
			return res, nil
		}
		res.Escalations = append(res.Escalations, Escalation{
			From:   current,
			To:     next,
			Reason: fmt.Sprintf("%s estimate %d tokens exceeds budget %d", string(current), after, budgetTokens),
		})
		current = next
	}
}
