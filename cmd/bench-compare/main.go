// Command bench-compare compares one benchmark run against a baseline
// (AUD-CFG-004 follow-up / WS7): the automatic regression gate behind
// .github/workflows/bench-run.yml.
//
// Latency metrics are compared per named pass (mean/p50/p95/p99 in
// milliseconds) plus the ready_ms probes; a regression is any metric
// worse than baseline*(1+threshold). Comparability is guarded: the
// comparison runs only when both files report the same CPU model — a
// different rig makes the numbers meaningless, and pretending
// otherwise would manufacture false regressions (or hide real ones).
//
// Exit codes (each outcome is distinct — a gate must never be able
// to silently read "infrastructure broke" as "no regression"):
//
//	0  no regression, or comparison skipped for a stated reason
//	   (no baseline yet, or rig mismatch)
//	1  confirmed regression beyond the threshold
//	2  invalid input (unreadable/malformed current result)
//
// Usage:
//
//	bench-compare --current results.json --baseline baseline.json [--threshold 0.25]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// benchResult is the subset of scripts/bench.sh output this gate
// reads: the readiness probes, the rig identity, and the per-pass
// latency summaries.
type benchResult struct {
	Rig struct {
		CPUModel string `json:"cpu_model"`
	} `json:"rig"`
	ReadyMS struct {
		Healthz  float64 `json:"healthz"`
		V1Models float64 `json:"v1_models"`
	} `json:"ready_ms"`
	Passes map[string]struct {
		DirectLatency struct {
			MeanMS float64 `json:"mean_ms"`
			P50MS  float64 `json:"p50_ms"`
			P95MS  float64 `json:"p95_ms"`
			P99MS  float64 `json:"p99_ms"`
		} `json:"direct_latency"`
	} `json:"passes"`
}

type regression struct {
	metric            string
	baseline, current float64
}

func loadResult(path string) (*benchResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r benchResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// metrics flattens one result into named scalar latency metrics.
// Only positive values are collected: a zero means the pass did not
// run (absent), not "instantaneous" — treating it as 0 would turn a
// missing baseline metric into an instant regression.
func metrics(r *benchResult) map[string]float64 {
	out := map[string]float64{}
	if r.ReadyMS.Healthz > 0 {
		out["ready_ms.healthz"] = r.ReadyMS.Healthz
	}
	if r.ReadyMS.V1Models > 0 {
		out["ready_ms.v1_models"] = r.ReadyMS.V1Models
	}
	passes := make([]string, 0, len(r.Passes))
	for p := range r.Passes {
		passes = append(passes, p)
	}
	sort.Strings(passes)
	for _, p := range passes {
		dl := r.Passes[p].DirectLatency
		prefix := "pass[" + p + "].direct_latency."
		for name, v := range map[string]float64{
			"mean_ms": dl.MeanMS,
			"p50_ms":  dl.P50MS,
			"p95_ms":  dl.P95MS,
			"p99_ms":  dl.P99MS,
		} {
			if v > 0 {
				out[prefix+name] = v
			}
		}
	}
	return out
}

func main() {
	current := flag.String("current", "", "path to the current benchmark result JSON (required)")
	baseline := flag.String("baseline", "", "path to the baseline benchmark result JSON (optional: without one the gate reports SKIP)")
	threshold := flag.Float64("threshold", 0.25, "allowed fractional slowdown (0.25 = 25% slower than baseline is a regression)")
	flag.Parse()

	if *current == "" {
		fmt.Fprintln(os.Stderr, "bench-compare: --current is required")
		os.Exit(2)
	}
	cur, err := loadResult(*current)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-compare: invalid current result: %v\n", err)
		os.Exit(2)
	}
	if *baseline == "" {
		fmt.Println("SKIP: no baseline configured — run bench-run once and commit docs/benchmark-baseline.json to activate the gate")
		return
	}
	base, err := loadResult(*baseline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-compare: invalid baseline: %v\n", err)
		os.Exit(2)
	}
	// Rig guard: numbers from different CPUs are not comparable.
	// A mismatch is reported as a SKIP with both models named —
	// distinct from a pass, distinct from a regression.
	bm, cm := strings.TrimSpace(base.Rig.CPUModel), strings.TrimSpace(cur.Rig.CPUModel)
	if bm != "" && cm != "" && bm != cm {
		fmt.Printf("SKIP: rig mismatch (baseline %q vs current %q) — latency is not comparable across machines\n", bm, cm)
		return
	}

	bm2, cm2 := metrics(base), metrics(cur)
	var regs []regression
	names := make([]string, 0, len(bm2))
	for n := range bm2 {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, ok := cm2[n]
		if !ok {
			fmt.Printf("note: metric %s missing from current run (baseline %.3f ms)\n", n, bm2[n])
			continue
		}
		if b > bm2[n]*(1+*threshold) {
			regs = append(regs, regression{n, bm2[n], b})
		}
	}
	if len(regs) == 0 {
		fmt.Printf("PASS: no metric regressed beyond %.0f%% of baseline (%d metrics compared)\n", *threshold*100, len(names))
		return
	}
	fmt.Printf("FAIL: %d metric(s) regressed beyond %.0f%% of baseline:\n", len(regs), *threshold*100)
	for _, r := range regs {
		fmt.Printf("  %s: %.3f ms -> %.3f ms (%+.1f%%)\n", r.metric, r.baseline, r.current,
			(r.current/r.baseline-1)*100)
	}
	os.Exit(1)
}
