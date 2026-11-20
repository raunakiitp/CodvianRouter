// Package evalharness is a repeatable harness that measures what the
// CURRENT compression pipeline (internal/compress.Apply) actually
// saves, over a fixed corpus, using real tokencount BPE counts.
//
// Honesty contract — what a number in the table does and does not
// prove:
//
//   - InputTokens / OutputTokens are exact local BPE counts
//     (internal/tokencount.CountPrompt) of the messages before and
//     after Apply, counted INDEPENDENTLY by this harness — never
//     copied from compress.CompressionResult. If the harness's
//     recount disagrees with the result stamp, or if
//     Saved+Output != Input, Run fails loudly instead of printing
//     a table.
//   - SavedTokens = InputTokens - OutputTokens. A negative value
//     (compression made the prompt BIGGER) is an error, not a
//     clamped zero — the inflation guard (M7.3) owns the fix, this
//     harness only reports it honestly.
//   - "lossless" means the lossless steps only (whitespace strip,
//     system dedupe, tool-result canonicalisation). LosslessVerified
//     is compress.VerifyLossless(original, output): the output is
//     exactly the canonical normal form of the input. It is a
//     structural guarantee, not a semantic proof.
//   - "lossy" means lossless first, then extractive trimming
//     toward the case's fixed LossyBudget (whole middle turns
//     dropped oldest-first, then head+tail truncation). Lossy
//     output is NOT verified lossless by definition — meaning may
//     have changed, and the fidelity gate (M7.2) owns that check.
//   - Counts are LOCAL BPE estimates for the harness model
//     (DefaultModel), never provider-reported usage. A provider may
//     bill a different number; runtime calibration is M7.9.
//   - The corpus is fixed []provider.Message literals in
//     corpus.go. No network, no randomness, no clock: two runs
//     produce byte-identical tables (pinned by TestDeterminism).
//
// Entry point (one, documented): run the table-printing test:
//
//	go test ./internal/compress/evalharness/ -run TestPrintMeasuredSavingsTable -v -count=1
//
// The measured table it prints is the source for
// docs/compression-savings-2026-11-18.md — the doc is a copy of
// harness output, never hand-written numbers.
package evalharness

import (
	"errors"
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// DefaultModel is the model the harness counts and compresses for.
// gpt-4o maps to cl100k_base, the same encoding the compress
// package's own tests use, so harness numbers are comparable with
// the package's fixtures.
const DefaultModel = "gpt-4o"

// Engine names a compression engine measured by the harness.
type Engine string

const (
	// EngineLossless runs the lossless steps only.
	EngineLossless Engine = "lossless"
	// EngineLossy runs lossless first, then extractive trimming
	// toward the case's LossyBudget.
	EngineLossy Engine = "lossy"
)

// Result is the measured outcome of running one engine over one
// corpus case. Every token number is the harness's own recount.
type Result struct {
	CaseName     string
	Category     string
	Engine       Engine
	InputTokens  int
	OutputTokens int
	SavedTokens  int
	// SavedPct is 100*Saved/Input, 0 when Input is 0.
	SavedPct float64
	// LosslessVerified is compress.VerifyLossless(original, output)
	// for the lossless engine; false for lossy (not applicable —
	// lossy output is not a normal form of the input by design).
	LosslessVerified bool
	// LossyApplied reports compress's own LossyApplied stamp.
	LossyApplied bool
	// Steps names the steps compress reported, in order.
	Steps []string
	// LossyBudget is the token budget the lossy run trimmed
	// toward (0 for the lossless engine).
	LossyBudget int
}

// Run runs both engines over every case in order and returns the
// results (lossless first, then lossy, per case, in corpus order).
// It fails loudly — returns a non-nil error and no usable table —
// if any case's recount disagrees with the compression stamp, if
// the arithmetic identity Saved+Output==Input fails, or if a run
// inflates a prompt (Output > Input). A counting error from
// tokencount is also an error, never a silent zero.
func Run(model string, cases []Case) ([]Result, error) {
	if model == "" {
		model = DefaultModel
	}
	var out []Result
	var errs []error
	for _, c := range cases {
		for _, eng := range []Engine{EngineLossless, EngineLossy} {
			r, err := runOne(model, c, eng)
			if err != nil {
				errs = append(errs, fmt.Errorf("case %s engine %s: %w", c.Name, eng, err))
				continue
			}
			out = append(out, r)
		}
	}
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// RunCorpus runs both engines over the built-in corpus.
func RunCorpus(model string) ([]Result, error) {
	return Run(model, Corpus())
}

func runOne(model string, c Case, eng Engine) (Result, error) {
	lossless := true
	req := &provider.CompressionRequest{Lossless: &lossless}
	budget := 0
	if eng == EngineLossy {
		budget = c.LossyBudget
		req.LossyMaxPromptTokens = budget
	}
	compressed, res := compress.Apply(model, c.Messages, req, nil, nil)
	if res == nil {
		return Result{}, errors.New("compress.Apply returned a nil result stamp")
	}
	r, err := Measure(model, c, eng, c.Messages, compressed)
	if err != nil {
		return Result{}, err
	}
	r.LossyApplied = res.LossyApplied
	r.Steps = append([]string(nil), res.Steps...)
	r.LossyBudget = budget
	// Cross-check the stamp against the independent recount: the
	// stamp must agree on before/after/saved, or the harness
	// refuses to report the case.
	if res.BeforeTokens != r.InputTokens || res.AfterTokens != r.OutputTokens || res.SavedTokens != r.SavedTokens {
		return Result{}, fmt.Errorf("compression stamp disagrees with harness recount: stamp before=%d after=%d saved=%d, recount before=%d after=%d saved=%d",
			res.BeforeTokens, res.AfterTokens, res.SavedTokens, r.InputTokens, r.OutputTokens, r.SavedTokens)
	}
	if eng == EngineLossless {
		r.LosslessVerified = compress.VerifyLossless(c.Messages, compressed)
		if !r.LosslessVerified {
			return Result{}, errors.New("lossless output failed VerifyLossless — the transform is not the canonical normal form of the input")
		}
	}
	return r, nil
}

// Measure recounts original and compressed with real BPE counts
// and builds the Result. It is exported so tests can prove the
// harness fails loudly on tampered numbers: pass a Result whose
// SavedTokens you altered to VerifyResult and it errors.
func Measure(model string, c Case, eng Engine, original, compressed []provider.Message) (Result, error) {
	in, err := tokencount.CountPrompt(model, original)
	if err != nil {
		return Result{}, fmt.Errorf("counting input: %w", err)
	}
	out, err := tokencount.CountPrompt(model, compressed)
	if err != nil {
		return Result{}, fmt.Errorf("counting output: %w", err)
	}
	if in.Estimated || out.Estimated {
		return Result{}, errors.New("count came back Estimated (oversize input) — this harness reports exact BPE counts only")
	}
	r := Result{
		CaseName:     c.Name,
		Category:     c.Category,
		Engine:       eng,
		InputTokens:  in.Tokens,
		OutputTokens: out.Tokens,
		SavedTokens:  in.Tokens - out.Tokens,
	}
	if r.SavedTokens < 0 {
		return Result{}, fmt.Errorf("inflation: compressed output costs %d tokens, input %d — compression made the prompt bigger", out.Tokens, in.Tokens)
	}
	if r.InputTokens > 0 {
		r.SavedPct = 100 * float64(r.SavedTokens) / float64(r.InputTokens)
	}
	return r, nil
}

// VerifyResult re-counts original and compressed and checks that
// r's numbers match the recount exactly and satisfy
// Saved+Output==Input. Any mismatch is an error — this is the
// loud-failure path the harness tests exercise by tampering.
func VerifyResult(model string, c Case, r Result, original, compressed []provider.Message) error {
	fresh, err := Measure(model, c, r.Engine, original, compressed)
	if err != nil {
		return err
	}
	if fresh.InputTokens != r.InputTokens || fresh.OutputTokens != r.OutputTokens || fresh.SavedTokens != r.SavedTokens {
		return fmt.Errorf("case %s engine %s: reported in=%d out=%d saved=%d, recount in=%d out=%d saved=%d",
			r.CaseName, r.Engine, r.InputTokens, r.OutputTokens, r.SavedTokens,
			fresh.InputTokens, fresh.OutputTokens, fresh.SavedTokens)
	}
	if r.SavedTokens+r.OutputTokens != r.InputTokens {
		return fmt.Errorf("case %s engine %s: arithmetic identity failed: saved(%d)+output(%d) != input(%d)",
			r.CaseName, r.Engine, r.SavedTokens, r.OutputTokens, r.InputTokens)
	}
	return nil
}

// Totals aggregates one engine's results.
type Totals struct {
	Engine      Engine
	Cases       int
	InputTokens int
	SavedTokens int
	SavedPct    float64
}

// Total sums the results of one engine.
func Total(results []Result, eng Engine) Totals {
	t := Totals{Engine: eng}
	for _, r := range results {
		if r.Engine != eng {
			continue
		}
		t.Cases++
		t.InputTokens += r.InputTokens
		t.SavedTokens += r.SavedTokens
	}
	if t.InputTokens > 0 {
		t.SavedPct = 100 * float64(t.SavedTokens) / float64(t.InputTokens)
	}
	return t
}

// RenderTable renders results as a fixed-width plain-text table.
// Deterministic: same results in, byte-identical text out (no
// timestamps, no map iteration, fixed column widths and %.1f%%).
// Rows are in corpus order, lossless then lossy per case, followed
// by one TOTAL row per engine.
func RenderTable(results []Result) string {
	var sb strings.Builder
	sb.WriteString("case                           engine    in   out  saved  saved%  verified/lossy\n")
	for _, r := range results {
		note := "-"
		if r.Engine == EngineLossless {
			if r.LosslessVerified {
				note = "verified"
			} else {
				note = "NOT-VERIFIED"
			}
		} else if r.LossyApplied {
			note = "lossy-applied"
		}
		fmt.Fprintf(&sb, "%-30s  %-8s %4d  %4d  %5d  %5.1f%%  %s\n",
			r.CaseName, r.Engine, r.InputTokens, r.OutputTokens, r.SavedTokens, r.SavedPct, note)
	}
	for _, eng := range []Engine{EngineLossless, EngineLossy} {
		t := Total(results, eng)
		fmt.Fprintf(&sb, "%-30s  %-8s %4d  %4d  %5d  %5.1f%%\n",
			"TOTAL ("+string(eng)+")", "", t.InputTokens, t.InputTokens-t.SavedTokens, t.SavedTokens, t.SavedPct)
	}
	return sb.String()
}
