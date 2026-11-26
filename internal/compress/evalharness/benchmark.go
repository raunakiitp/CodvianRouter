package evalharness

// Reproducible compression benchmark (P1.4).
//
// The older harness (Run/RenderTable in evalharness.go) answers one
// question: how many tokens does the pipeline save, and is the
// lossless output structurally canonical? This file answers the
// questions P1.4 adds on top:
//
//   - latency: how long does one Apply take (min/p50/max over a fixed
//     repeat count), reported separately from the deterministic
//     token table because a duration is machine-dependent;
//   - fidelity: structural survival of instructions, tool calls,
//     tool results, parseable JSON, concrete facts and prose words
//     (MeasureFidelity — proxies, honestly labelled);
//   - long-context behaviour: a separate corpus for prompts far
//     larger than the standard cases (LongContextCorpus);
//   - machine-readable output: Report.JSON for CI/archival;
//   - explicit thresholds and fail conditions: ThresholdsFor(engine)
//     + Check, so a case that saves 80% of tokens while amputating a
//     system prompt FAILS instead of reading as a win.
//
// Determinism contract: everything except Latency is a pure function
// of (model, corpus, repeats-independent Apply). Report.JSON therefore
// carries latency, and Report.JSONStable omits it — the stable form is
// what the determinism test compares byte-for-byte.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Latency is measured wall-clock Apply time for one (case, engine).
type Latency struct {
	Repeats int     `json:"repeats"`
	MinMs   float64 `json:"min_ms"`
	P50Ms   float64 `json:"p50_ms"`
	MaxMs   float64 `json:"max_ms"`
}

// EngineReport is one (case, engine) measurement row.
type EngineReport struct {
	Engine           Engine   `json:"engine"`
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	SavedTokens      int      `json:"saved_tokens"`
	SavedPct         float64  `json:"saved_pct"`
	LosslessVerified bool     `json:"lossless_verified"`
	LossyApplied     bool     `json:"lossy_applied"`
	LossyBudget      int      `json:"lossy_budget"`
	Steps            []string `json:"steps,omitempty"`
	Fidelity         Fidelity `json:"fidelity"`
	Latency          Latency  `json:"latency"`
	Pass             bool     `json:"pass"`
	Failures         []string `json:"failures,omitempty"`
}

// CaseReport groups both engines' rows for one corpus case.
type CaseReport struct {
	Name     string         `json:"name"`
	Category string         `json:"category"`
	Engines  []EngineReport `json:"engines"`
}

// EngineTotals aggregates one engine across every case.
type EngineTotals struct {
	Engine      Engine  `json:"engine"`
	Cases       int     `json:"cases"`
	InputTokens int     `json:"input_tokens"`
	SavedTokens int     `json:"saved_tokens"`
	SavedPct    float64 `json:"saved_pct"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
}

// Report is the full machine-readable benchmark result.
type Report struct {
	Model      string            `json:"model"`
	Cases      []CaseReport      `json:"cases"`
	Totals     []EngineTotals    `json:"totals"`
	Thresholds []ThresholdsEntry `json:"thresholds"`
	Repeats    int               `json:"repeats"`
}

// ThresholdsEntry names the effective profile applied to one
// (engine, category) pair, so the JSON states its own pass criteria.
type ThresholdsEntry struct {
	Engine     Engine     `json:"engine"`
	Category   string     `json:"category"`
	Thresholds Thresholds `json:"thresholds"`
}

// Options configures RunBenchmark.
type Options struct {
	Model string
	Cases []Case
	// Repeats is the latency sample count per (case, engine).
	// Values < 1 are treated as 1. Latency is informational: it is
	// excluded from JSONStable and never affects Pass.
	Repeats int
	// Pool, when set, runs the per-(case, engine) fidelity
	// evaluations OFF the calling goroutine on the bounded worker
	// pool (P2-CMP-002 integration): the harness is background
	// quality work that may wait, exactly the pool's documented
	// purpose — a request that needs its compressed prompt now
	// must not be parked behind a queue, and nothing here does.
	// The pool's bounded queue provides backpressure (Submit
	// blocks until space frees), and a nil Pool selects the
	// original serial path — same report, no pool dependency.
	// LATENCY MEASUREMENT STAYS SERIAL even with a pool: parallel
	// workers measuring wall-clock latency inflate each other's
	// numbers under CPU contention, so the samples would lie.
	Pool *compress.CompressionPool
	// JobTimeout bounds one pooled evaluation (0 = pool default).
	JobTimeout time.Duration
}

// RunBenchmark runs both engines over every case, measures tokens,
// fidelity and latency, applies the per-engine thresholds and returns
// the report. It returns an error only when a measurement itself is
// impossible (counting failure, nil stamp, inflation) — threshold
// failures are recorded in EngineReport.Pass/Failures, not returned,
// so a failing case is still visible in the JSON.
func RunBenchmark(opts Options) (*Report, error) {
	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	repeats := opts.Repeats
	if repeats < 1 {
		repeats = 1
	}
	report := &Report{Model: model, Repeats: repeats}
	for _, eng := range []Engine{EngineLossless, EngineLossy} {
		report.Totals = append(report.Totals, EngineTotals{Engine: eng})
	}
	seenThresholds := map[string]bool{}

	// Evaluate every (case, engine) pair — through the pool when one
	// is wired (bounded parallelism, backpressure via the pool's
	// bounded queue), serially otherwise. results[case][engine].
	engines := []Engine{EngineLossless, EngineLossy}
	results := make([][]evalResult, len(opts.Cases))
	for i := range results {
		results[i] = make([]evalResult, len(engines))
	}
	if opts.Pool != nil {
		if err := evalPooled(model, opts, engines, results); err != nil {
			return report, err
		}
	} else {
		for ci, c := range opts.Cases {
			for ei, eng := range engines {
				er, err := runBenchmarkEval(model, c, eng)
				results[ci][ei] = evalResult{er: er, err: err}
			}
		}
	}

	for ci, c := range opts.Cases {
		cr := CaseReport{Name: c.Name, Category: c.Category}
		for ei, eng := range engines {
			res := results[ci][ei]
			if res.err != nil {
				return report, fmt.Errorf("case %s engine %s: %w", c.Name, eng, res.err)
			}
			// Latency stays SERIAL in both paths (measurement
			// integrity): parallel workers measuring wall-clock
			// latency inflate each other's samples under CPU
			// contention.
			er := res.er
			budget := 0
			if eng == EngineLossy {
				budget = c.LossyBudget
			}
			er.Latency = measureLatency(model, c, eng, budget, repeats)
			th := ThresholdsForCase(eng, c.Category)
			key := string(eng) + "|" + c.Category
			if !seenThresholds[key] {
				seenThresholds[key] = true
				report.Thresholds = append(report.Thresholds, ThresholdsEntry{
					Engine: eng, Category: c.Category, Thresholds: th,
				})
			}
			er.Failures = Check(eng, er, th)
			er.Pass = len(er.Failures) == 0
			for i := range report.Totals {
				if report.Totals[i].Engine != eng {
					continue
				}
				t := &report.Totals[i]
				t.Cases++
				t.InputTokens += er.InputTokens
				t.SavedTokens += er.SavedTokens
				if er.Pass {
					t.Passed++
				} else {
					t.Failed++
				}
			}
			cr.Engines = append(cr.Engines, er)
		}
		report.Cases = append(report.Cases, cr)
	}
	for i := range report.Totals {
		t := &report.Totals[i]
		if t.InputTokens > 0 {
			t.SavedPct = 100 * float64(t.SavedTokens) / float64(t.InputTokens)
		}
	}
	return report, nil
}

// evalResult carries one (case, engine) evaluation between the
// evaluation phase and the report assembly.
type evalResult struct {
	er  EngineReport
	err error
}

// evalPooled runs every (case, engine) fidelity evaluation on the
// pool (P2-CMP-002): all jobs are submitted up-front so the pool's
// bounded workers drain them with backpressure (Submit blocks while
// the bounded queue is full), then the futures are collected IN
// ORDER so the report is identical to the serial path. Each job
// writes exactly its own results slot — no shared mutable state
// beyond the slot, and nothing is read until every future has
// resolved. A job whose Run returns an error resolves the future
// with that error; a context end or pool shutdown abandons the wait
// and fails the benchmark honestly (never silently drops cases).
func evalPooled(model string, opts Options, engines []Engine, results [][]evalResult) error {
	ctx := context.Background()
	type slot struct {
		ci, ei int
		c      Case
		eng    Engine
	}
	var slots []slot
	var futures []*compress.CompressionFuture
	for ci := range opts.Cases {
		for ei, eng := range engines {
			s := slot{ci: ci, ei: ei, c: opts.Cases[ci], eng: eng}
			slots = append(slots, s)
			futures = append(futures, opts.Pool.Submit(ctx, compress.CompressionJob{
				Timeout: opts.JobTimeout,
				Run: func(ctx context.Context, msgs []provider.Message, cfg compress.StackedConfig) (compress.StackedResult, error) {
					er, err := runBenchmarkEval(model, s.c, s.eng)
					results[s.ci][s.ei] = evalResult{er: er, err: err}
					// The StackedResult shape is not what this
					// job produces; the slot above carries the
					// real outcome, and the error rides the
					// future so a failing job still surfaces.
					return compress.StackedResult{}, err
				},
			}))
		}
	}
	for i, f := range futures {
		r, err := f.WaitContext(ctx)
		if err != nil {
			return fmt.Errorf("pooled eval case %s: %w", slots[i].c.Name, err)
		}
		if r.Err != nil {
			return fmt.Errorf("case %s engine %s: %w", slots[i].c.Name, slots[i].eng, r.Err)
		}
	}
	return nil
}

// runBenchmarkEval is runBenchmarkOne without the latency samples:
// the fidelity/token evaluation runs pooled or serial; latency is
// measured serially afterwards in both paths (see RunBenchmark).
func runBenchmarkEval(model string, c Case, eng Engine) (EngineReport, error) {
	lossless := true
	req := &provider.CompressionRequest{Lossless: &lossless}
	budget := 0
	if eng == EngineLossy {
		budget = c.LossyBudget
		req.LossyMaxPromptTokens = budget
	}

	compressed, res := compress.Apply(model, c.Messages, req, nil, nil)
	if res == nil {
		return EngineReport{}, fmt.Errorf("compress.Apply returned a nil result stamp")
	}
	// Independent recount, same honesty rule as Run: the harness
	// refuses to publish a number that disagrees with the stamp.
	in, err := tokencount.CountPrompt(model, c.Messages)
	if err != nil {
		return EngineReport{}, fmt.Errorf("counting input: %w", err)
	}
	out, err := tokencount.CountPrompt(model, compressed)
	if err != nil {
		return EngineReport{}, fmt.Errorf("counting output: %w", err)
	}
	if in.Estimated || out.Estimated {
		return EngineReport{}, fmt.Errorf("count came back Estimated (oversize input) — this benchmark reports exact BPE counts only")
	}
	if res.BeforeTokens != in.Tokens || res.AfterTokens != out.Tokens || res.SavedTokens != in.Tokens-out.Tokens {
		return EngineReport{}, fmt.Errorf("compression stamp disagrees with recount: stamp before=%d after=%d saved=%d, recount before=%d after=%d saved=%d",
			res.BeforeTokens, res.AfterTokens, res.SavedTokens, in.Tokens, out.Tokens, in.Tokens-out.Tokens)
	}
	if out.Tokens > in.Tokens {
		return EngineReport{}, fmt.Errorf("inflation: output %d tokens > input %d", out.Tokens, in.Tokens)
	}

	er := EngineReport{
		Engine:       eng,
		InputTokens:  in.Tokens,
		OutputTokens: out.Tokens,
		SavedTokens:  in.Tokens - out.Tokens,
		LossyBudget:  budget,
		LossyApplied: res.LossyApplied,
		Steps:        append([]string(nil), res.Steps...),
	}
	if er.InputTokens > 0 {
		er.SavedPct = 100 * float64(er.SavedTokens) / float64(er.InputTokens)
	}
	if eng == EngineLossless {
		er.LosslessVerified = compress.VerifyLossless(c.Messages, compressed)
		if !er.LosslessVerified {
			// Same loud-failure rule as Run: never report a lossless
			// case we cannot vouch for.
			return EngineReport{}, fmt.Errorf("lossless output failed VerifyLossless")
		}
	}
	er.Fidelity = MeasureFidelity(eng, c.Messages, compressed)
	return er, nil
}

func measureLatency(model string, c Case, eng Engine, budget, repeats int) Latency {
	lossless := eng == EngineLossless
	req := &provider.CompressionRequest{Lossless: &lossless}
	if eng == EngineLossy {
		req.LossyMaxPromptTokens = budget
	}
	samples := make([]float64, 0, repeats)
	for i := 0; i < repeats; i++ {
		start := time.Now()
		_, _ = compress.Apply(model, c.Messages, req, nil, nil)
		samples = append(samples, float64(time.Since(start).Nanoseconds())/1e6)
	}
	sort.Float64s(samples)
	return Latency{
		Repeats: repeats,
		MinMs:   samples[0],
		P50Ms:   median(samples),
		MaxMs:   samples[len(samples)-1],
	}
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// JSON renders the report as indented JSON. Field order follows the
// struct definitions; no maps are serialised, so two runs with equal
// inputs produce identical bytes. Latency IS included here.
func (r *Report) JSON() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("nil report")
	}
	return json.MarshalIndent(r, "", "  ")
}

// JSONStable renders the report with latency zeroed — the
// determinism-safe form (durations are machine-dependent by nature).
func (r *Report) JSONStable() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("nil report")
	}
	cp := *r
	for i := range cp.Cases {
		for j := range cp.Cases[i].Engines {
			cp.Cases[i].Engines[j].Latency = Latency{Repeats: cp.Cases[i].Engines[j].Latency.Repeats}
		}
	}
	return json.MarshalIndent(&cp, "", "  ")
}

// RenderReportText renders a fixed-width text summary mirroring
// RenderTable, plus fidelity and pass/fail. Deterministic (no
// latency column — durations would break byte-identical reruns).
func RenderReportText(r *Report) string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "model %s  repeats %d  cases %d\n", r.Model, r.Repeats, len(r.Cases))
	fmt.Fprintf(&sb, "case                           cat            engine    in   out  saved  saved%%  instr  tools  json  facts  prose  result\n")
	for _, c := range r.Cases {
		for _, e := range c.Engines {
			result := "PASS"
			if !e.Pass {
				result = "FAIL"
			}
			fmt.Fprintf(&sb, "%-30s %-14s %-8s %4d  %4d  %5d  %5.1f%%  %5.2f  %5.2f  %5.2f  %5.2f  %5.2f  %s\n",
				c.Name, c.Category, e.Engine, e.InputTokens, e.OutputTokens, e.SavedTokens, e.SavedPct,
				e.Fidelity.InstructionPreservation, e.Fidelity.ToolCallPreservation,
				e.Fidelity.JSONPreserved, e.Fidelity.FactPreservation, e.Fidelity.ProsePreservation,
				result)
		}
	}
	for _, t := range r.Totals {
		fmt.Fprintf(&sb, "TOTAL (%s) cases %d in %d saved %d (%.1f%%) pass %d fail %d\n",
			t.Engine, t.Cases, t.InputTokens, t.SavedTokens, t.SavedPct, t.Passed, t.Failed)
	}
	return sb.String()
}
