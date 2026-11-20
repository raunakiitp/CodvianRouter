// Benchmark feed (GET /dashboard/benchmark).
//
// The dashboard's Benchmark section shows the last measured
// Geekflare-style bench run — the numbers recorded by
// scripts/bench-geekflare.sh (combined bench-results.json), copied
// to the router's data dir as benchmark.json (CODVIAN_BENCH_OUT).
// Every value comes from that file; nothing is estimated, and a
// miss is shown as a miss: each metric carries its Geekflare
// target and a server-computed met/missed verdict.
//
// E1 limiter verdict (CHECKLIST 233–234): scripts/bench-e1-limiter.sh
// writes a merged file whose top level carries limiter_comparison
// (the limiter-on run, the limiter-off run, a verdict computed
// from the measured numbers, and a clean flag). When the served
// file has one, the response carries both modes side by side; the
// top-level passes/ready/idle still describe the limiter-ON run
// (the default mode, comparable with earlier runs). A polluted
// comparison (clean=false) is served with its verdict text saying
// so — the verdict field is the script's, computed from the
// recorded numbers, never invented here.
//
// Hot-path CPU profile (CHECKLIST 249–250): scripts/merge-bench-cpu.sh
// merges a before/after bench pair into a file whose top level
// carries cpu_profile (the before run, the after run, a verdict
// computed from the measured numbers, and a clean flag). When the
// served file has one, the response carries both runs side by
// side; the top-level passes/ready/idle describe the AFTER run
// (the current tree).
//
// Definitive re-measurement (CHECKLIST 273–274):
// scripts/merge-bench-definitive.sh merges the re-measurement run
// into a file whose top level IS that run, plus
// definitive_remeasurement (the verdict computed from the recorded
// numbers, a clean flag, and the load trace). When the served file
// has one, the response carries it; a polluted run (clean=false)
// is served with its verdict text saying no verdict can be drawn
// from it — never presented as the definitive measurement.
//
// Key-budget re-measurement (CHECKLIST 279–280):
// scripts/merge-bench-keybudget.sh does the same for the run with
// the per-request key-budget memo in, plus
// keybudget_remeasurement (the verdict, clean flag, load trace,
// the before/after GetKeyBudget profile shares, and the new top
// cost from the fresh profile when the targets are still missed).
// A polluted run is served the same way: clean=false, no verdict
// drawn from it.
//
// Global-budget re-measurement (CHECKLIST 299–300):
// scripts/merge-bench-globalbudget.sh does the same for the run
// with the per-request global-budget memo in, plus
// globalbudget_remeasurement (the verdict, clean flag, load trace,
// the before/after GetBudget profile shares, and the new top cost
// from the fresh profile when the targets are still missed). A
// polluted run is served the same way: clean=false, no verdict
// drawn from it. The honest scope note (see
// internal/ledger/budget_memo.go): on today's hot paths the
// global-budget guard reads once per request, so the memo has no
// second read to save there — the profile shares are reported as
// measured, and a within-noise move is said so, never dressed up
// as a win.
//
// Ledger write path (CHECKLIST 287–288):
// the ledger merge (same shape as the CPU merge) produces a file
// whose top level IS the after run, plus cpu_profile (whose
// verdict text IS the ledger verdict) and ledger_write_path (the
// fix text and the before/after profile shares). The single-run
// feed serves it through the CPU pair; the history feed
// classifies it as kind "ledger".
//
// Benchmark history (CHECKLIST 309–310): GET
// /dashboard/benchmark/history serves EVERY recorded bench file
// in a directory (CODVIAN_BENCH_HISTORY_DIR, the repo's docs/ in
// practice) — one entry per benchmark-*.json file, oldest first,
// each with the run's kind (global-budget / key-budget /
// definitive / ledger / cpu / limiter / plain run), its clean
// flag, its verdict and its passes. The feed serves the recorded files verbatim: a merged
// file's entry describes its primary run under the same rule as
// the single-run feed, and carries the recorded verdict/clean of
// the record that names its kind; a plain run's clean flag is the
// E1 canary rule computed server-side from its own recorded
// direct-mock RPS (every pass >= 12,000 RPS), and its verdict is
// the server-computed target verdicts — never invented. A file
// that cannot be read or parsed does not kill the feed: it is
// listed under "skipped" with the reason, so a corrupt file is
// visible, not silently dropped and not shown as data.
//
// Targets (docs/BENCHMARK.md, research-log entry 148): added
// latency < 0.5 ms, RPS (ok, 32 clients x 20 s) > 3,000, idle RSS
// < 70 MB, ready < 10 s. RPS errors are reported, not a target —
// the adaptive limiter sheds excess load under the closed-loop
// bench client, so the error rate is shown alongside the RPS.
//
// No run recorded yet: an honest empty state (recorded=false + a
// note saying how to record one), never invented numbers. A
// corrupt file is a 500 — the operator should re-run the bench,
// not be shown stale or guessed values.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Geekflare-derived targets for the benchmark verdicts.
const (
	benchTargetAddedLatencyMs = 0.5
	benchTargetRPS            = 3000.0
	benchTargetIdleMB         = 70.0
	benchTargetReadyMs        = 10000.0
	// benchCanaryRPS is the E1 direct-mock canary: a pass is clean
	// only when the stand-in provider alone sustains at least this
	// many RPS, proving the rig (not the router) was quiet.
	benchCanaryRPS = 12000.0
)

// WithBenchmarkFile wires the bench-results file GET
// /dashboard/benchmark serves (the router's data dir
// benchmark.json, written by scripts/bench-geekflare.sh via
// CODVIAN_BENCH_OUT). Empty path (tests, servers without a data
// dir) means the endpoint always answers recorded=false.
func WithBenchmarkFile(path string) Option {
	return func(s *Server) { s.benchmarkFile = path }
}

// WithBenchmarkHistoryDir wires the directory GET
// /dashboard/benchmark/history reads (every benchmark-*.json in
// it, one history entry per file). Set from
// CODVIAN_BENCH_HISTORY_DIR in main; empty (the default) means
// the endpoint always answers recorded=false.
func WithBenchmarkHistoryDir(dir string) Option {
	return func(s *Server) { s.benchmarkHistoryDir = dir }
}

// benchFile is the on-disk shape written by
// scripts/bench-geekflare.sh (combined bench-results.json). Only
// the fields the feed reports are decoded. LimiterMode records
// which mode a single-mode run used ("on" or "off", E1);
// LimiterComparison is set only on the merged E1 file written by
// scripts/bench-e1-limiter.sh; CPUProfile is set only on the
// merged CPU-profile file written by scripts/merge-bench-cpu.sh
// (CHECKLIST 249-250: the before/after pair for the hot-path CPU
// fixes, with the verdict computed from the recorded numbers).
// Definitive is set only on the merged definitive file written
// by scripts/merge-bench-definitive.sh (CHECKLIST 273-274: the
// re-measurement run with the E1 verdict + CPU-profile fixes
// in, its verdict, clean flag and load trace).
// KeyBudget is set only on the merged key-budget file written by
// scripts/merge-bench-keybudget.sh (CHECKLIST 279-280: the
// re-measurement run with the per-request key-budget memo in, its
// verdict, clean flag, load trace, the before/after GetKeyBudget
// profile shares, and the new top cost from the fresh profile
// when the targets are still missed).
type benchFile struct {
	LimiterMode string `json:"limiter_mode,omitempty"`
	// LimiterComparison holds the E1 limiter-on vs limiter-off
	// pair plus the verdict the E1 script computed from the
	// recorded numbers.
	LimiterComparison *benchComparison `json:"limiter_comparison,omitempty"`
	// CPUProfile holds the hot-path CPU before/after pair plus
	// the verdict the merge script computed from the recorded
	// numbers.
	CPUProfile *benchCPUProfile `json:"cpu_profile,omitempty"`
	// Definitive holds the definitive re-measurement record
	// (CHECKLIST 273-274): the verdict the merge script computed
	// from the recorded numbers, whether every pass was clean
	// (direct-mock canary >= 12,000 RPS, the E1 rule), and the
	// load trace across the run.
	Definitive *benchDefinitive `json:"definitive_remeasurement,omitempty"`
	// KeyBudget holds the key-budget re-measurement record
	// (CHECKLIST 279-280): the verdict the merge script computed
	// from the recorded numbers, whether every pass was clean,
	// the load trace, and the before/after GetKeyBudget profile
	// shares plus the new top cost from the fresh profile.
	KeyBudget *benchKeyBudget `json:"keybudget_remeasurement,omitempty"`
	// GlobalBudget holds the global-budget re-measurement record
	// (CHECKLIST 299-300): the verdict the merge script computed
	// from the recorded numbers, whether every pass was clean,
	// the load trace, and the before/after GetBudget profile
	// shares plus the new top cost from the fresh profile.
	GlobalBudget *benchGlobalBudget `json:"globalbudget_remeasurement,omitempty"`
	// Ledger holds the ledger write-path record (CHECKLIST
	// 287-288): the fix text and the before/after profile
	// shares. It carries no verdict or clean flag of its own —
	// the ledger verdict lives in CPUProfile.Verdict.
	Ledger *benchLedger `json:"ledger_write_path,omitempty"`
	Rig    struct {
		Date     string `json:"date"`
		OS       string `json:"os"`
		CPUs     int    `json:"cpus"`
		CPUModel string `json:"cpu_model"`
		GoVer    string `json:"go_version"`
		Note     string `json:"note"`
	} `json:"rig"`
	ReadyMs struct {
		Healthz  float64 `json:"healthz"`
		V1Models float64 `json:"v1_models"`
	} `json:"ready_ms"`
	IdleRSSKiB int64 `json:"idle_rss_kib"`
	Passes     map[string]struct {
		AddedLatencyP50Ms float64 `json:"added_latency_p50_ms"`
		RouterLatency     struct {
			P50Ms     float64 `json:"p50_ms"`
			OK        int     `json:"ok"`
			Errors    int     `json:"errors"`
			CacheHits int     `json:"cache_hits"`
		} `json:"router_latency"`
		RouterRPS struct {
			RPS       float64 `json:"rps"`
			OK        int     `json:"ok"`
			Errors    int     `json:"errors"`
			Completed int     `json:"completed"`
			CacheHits int     `json:"cache_hits"`
		} `json:"router_rps"`
		DirectRPS struct {
			RPS float64 `json:"rps"`
		} `json:"direct_rps"`
		PeakRSSKiB int64 `json:"peak_rss_kib"`
	} `json:"passes"`
}

type benchmarkPass struct {
	Pass              int     `json:"pass"`
	AddedLatencyP50Ms float64 `json:"added_latency_p50_ms"`
	AddedLatencyMet   bool    `json:"added_latency_met"`
	RPSOk             float64 `json:"rps_ok"`
	RPSMet            bool    `json:"rps_met"`
	Errors            int     `json:"errors"`
	Completed         int     `json:"completed"`
	ErrorRatePct      float64 `json:"error_rate_pct"`
	CacheHits         int     `json:"cache_hits"`
	PeakRSSMiB        float64 `json:"peak_rss_mib"`
	RouterP50Ms       float64 `json:"router_p50_ms"`
	DirectRPS         float64 `json:"direct_rps"`
}

type benchmarkTargets struct {
	AddedLatencyP50Ms float64 `json:"added_latency_p50_ms"`
	RPSOk             float64 `json:"rps_ok"`
	IdleMB            float64 `json:"idle_mb"`
	ReadyMs           float64 `json:"ready_ms"`
}

// benchComparison is the on-disk E1 pair inside a merged file:
// the two full single-mode runs plus the verdict the E1 script
// computed from their measured numbers (never invented here) and
// whether every pass was clean (direct-mock canary >= 12,000 RPS).
type benchComparison struct {
	On      benchFile `json:"on"`
	Off     benchFile `json:"off"`
	Verdict string    `json:"verdict"`
	Clean   bool      `json:"clean"`
	Method  string    `json:"method,omitempty"`
}

// benchCPUProfile is the on-disk CPU before/after pair inside a
// merged file: the full before run, the full after run, the
// verdict the merge script computed from their measured numbers
// (never invented here) and whether every pass was clean
// (direct-mock canary >= 12,000 RPS, the E1 rule).
type benchCPUProfile struct {
	Before  benchFile `json:"before"`
	After   benchFile `json:"after"`
	Verdict string    `json:"verdict"`
	Clean   bool      `json:"clean"`
	Method  string    `json:"method,omitempty"`
}

// benchKeyBudget is the on-disk key-budget re-measurement
// record inside a merged file: the verdict the merge script
// computed from the recorded numbers (never invented here),
// whether every pass was clean, the load trace, the
// before/after GetKeyBudget profile shares, and the new top cost
// from the fresh profile when the targets are still missed.
type benchKeyBudget struct {
	Verdict               string  `json:"verdict"`
	Clean                 bool    `json:"clean"`
	Method                string  `json:"method,omitempty"`
	GetKeyBudgetBeforePct float64 `json:"get_key_budget_before_pct,omitempty"`
	GetKeyBudgetAfterPct  float64 `json:"get_key_budget_after_pct,omitempty"`
	TopCostAfter          string  `json:"top_cost_after,omitempty"`
	LoadTrace             struct {
		Start []string `json:"start"`
		End   []string `json:"end"`
	} `json:"load_trace,omitempty"`
}

// benchGlobalBudget is the on-disk global-budget re-measurement
// record inside a merged file: the verdict the merge script
// computed from the recorded numbers (never invented here),
// whether every pass was clean, the load trace, the
// before/after GetBudget profile shares, and the new top cost
// from the fresh profile when the targets are still missed.
type benchGlobalBudget struct {
	Verdict            string  `json:"verdict"`
	Clean              bool    `json:"clean"`
	Method             string  `json:"method,omitempty"`
	GetBudgetBeforePct float64 `json:"get_budget_before_pct,omitempty"`
	GetBudgetAfterPct  float64 `json:"get_budget_after_pct,omitempty"`
	TopCostAfter       string  `json:"top_cost_after,omitempty"`
	LoadTrace          struct {
		Start []string `json:"start"`
		End   []string `json:"end"`
	} `json:"load_trace,omitempty"`
}

// benchLedger is the on-disk ledger write-path record inside a
// merged file (CHECKLIST 287-288): the fix text, the before/after
// profile shares, and the HEADs the pair was measured on. No
// verdict or clean flag lives here — the ledger verdict is the
// cpu_profile verdict in the same file.
type benchLedger struct {
	Fix           string `json:"fix,omitempty"`
	BeforeHead    string `json:"before_head,omitempty"`
	AfterHead     string `json:"after_head,omitempty"`
	BeforeProfile string `json:"before_profile,omitempty"`
	AfterProfile  string `json:"after_profile,omitempty"`
}

// benchDefinitive is the on-disk definitive re-measurement
// record inside a merged file: the verdict the merge script
// computed from the recorded numbers (never invented here),
// whether every pass was clean, and the load trace. A polluted
// run (clean=false) is served with its verdict text saying
// so — never presented as the definitive measurement.
type benchDefinitive struct {
	Verdict   string `json:"verdict"`
	Clean     bool   `json:"clean"`
	Method    string `json:"method,omitempty"`
	LoadTrace struct {
		Start []string `json:"start"`
		End   []string `json:"end"`
	} `json:"load_trace,omitempty"`
}

// cpuProfileComparison is the served form of benchCPUProfile:
// both runs' passes with their server-computed target verdicts,
// the recorded verdict text, and the clean flag.
type cpuProfileComparison struct {
	Before  []benchmarkPass `json:"before"`
	After   []benchmarkPass `json:"after"`
	Verdict string          `json:"verdict"`
	Clean   bool            `json:"clean"`
	Method  string          `json:"method,omitempty"`
}

// benchmarkComparison is the served form of benchComparison: both
// modes' passes with their server-computed target verdicts, the
// recorded verdict text, and the clean flag.
type benchmarkComparison struct {
	On      []benchmarkPass `json:"on"`
	Off     []benchmarkPass `json:"off"`
	Verdict string          `json:"verdict"`
	Clean   bool            `json:"clean"`
	Method  string          `json:"method,omitempty"`
}

type benchmarkResponse struct {
	Recorded          bool                  `json:"recorded"`
	Note              string                `json:"note,omitempty"`
	Date              string                `json:"date,omitempty"`
	RigOS             string                `json:"rig_os,omitempty"`
	RigCPUs           int                   `json:"rig_cpus,omitempty"`
	RigCPUModel       string                `json:"rig_cpu_model,omitempty"`
	RigGoVersion      string                `json:"rig_go_version,omitempty"`
	RigNote           string                `json:"rig_note,omitempty"`
	ReadyHealthzMs    float64               `json:"ready_healthz_ms,omitempty"`
	ReadyModelsMs     float64               `json:"ready_v1_models_ms,omitempty"`
	ReadyMet          bool                  `json:"ready_met,omitempty"`
	IdleMB            float64               `json:"idle_mb,omitempty"`
	IdleMet           bool                  `json:"idle_met,omitempty"`
	Passes            []benchmarkPass       `json:"passes,omitempty"`
	Targets           benchmarkTargets      `json:"targets,omitempty"`
	LimiterMode       string                `json:"limiter_mode,omitempty"`
	LimiterComparison *benchmarkComparison  `json:"limiter_comparison,omitempty"`
	CPUProfile        *cpuProfileComparison `json:"cpu_profile,omitempty"`
	Definitive        *benchDefinitive      `json:"definitive_remeasurement,omitempty"`
	KeyBudget         *benchKeyBudget       `json:"keybudget_remeasurement,omitempty"`
	GlobalBudget      *benchGlobalBudget    `json:"globalbudget_remeasurement,omitempty"`
}

// benchmarkPasses converts one decoded bench file's passes into
// served passes with server-computed target verdicts, sorted by
// pass number. Shared by the single-run path and both E1 modes.
func benchmarkPasses(bf benchFile) []benchmarkPass {
	passes := make([]benchmarkPass, 0, len(bf.Passes))
	for key, p := range bf.Passes {
		n, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		rate := 0.0
		if p.RouterRPS.Completed > 0 {
			rate = float64(p.RouterRPS.Errors) / float64(p.RouterRPS.Completed) * 100
		}
		passes = append(passes, benchmarkPass{
			Pass:              n,
			AddedLatencyP50Ms: p.AddedLatencyP50Ms,
			AddedLatencyMet:   p.AddedLatencyP50Ms < benchTargetAddedLatencyMs,
			RPSOk:             p.RouterRPS.RPS,
			RPSMet:            p.RouterRPS.RPS > benchTargetRPS,
			Errors:            p.RouterRPS.Errors,
			Completed:         p.RouterRPS.Completed,
			ErrorRatePct:      rate,
			CacheHits:         p.RouterRPS.CacheHits,
			PeakRSSMiB:        float64(p.PeakRSSKiB) / 1024,
			RouterP50Ms:       p.RouterLatency.P50Ms,
			DirectRPS:         p.DirectRPS.RPS,
		})
	}
	sort.Slice(passes, func(i, j int) bool { return passes[i].Pass < passes[j].Pass })
	return passes
}

// benchmarkSummary is one decoded bench file reduced to its
// primary run plus the records it carries. Shared by the
// single-run feed and the history feed so the primary-run rule
// cannot drift between them: E1 merged file → limiter-ON run is
// primary; CPU merged file (no definitive record) → AFTER run is
// primary; definitive / key-budget / global-budget / ledger
// merged file → the top level IS the new run, so it stays
// primary (a carried-over CPU pair is served side by side only,
// never as the primary).
type benchmarkSummary struct {
	primary      benchFile
	limiterMode  string
	comparison   *benchmarkComparison
	cpu          *cpuProfileComparison
	definitive   *benchDefinitive
	keyBudget    *benchKeyBudget
	globalBudget *benchGlobalBudget
	ledger       *benchLedger
}

func summarizeBenchFile(bf benchFile) benchmarkSummary {
	sum := benchmarkSummary{primary: bf, limiterMode: bf.LimiterMode}
	if bf.LimiterComparison != nil {
		sum.primary = bf.LimiterComparison.On
		sum.limiterMode = "comparison"
		sum.comparison = &benchmarkComparison{
			On:      benchmarkPasses(bf.LimiterComparison.On),
			Off:     benchmarkPasses(bf.LimiterComparison.Off),
			Verdict: bf.LimiterComparison.Verdict,
			Clean:   bf.LimiterComparison.Clean,
			Method:  bf.LimiterComparison.Method,
		}
	}
	if bf.CPUProfile != nil {
		if bf.Definitive == nil {
			sum.primary = bf.CPUProfile.After
		}
		sum.cpu = &cpuProfileComparison{
			Before:  benchmarkPasses(bf.CPUProfile.Before),
			After:   benchmarkPasses(bf.CPUProfile.After),
			Verdict: bf.CPUProfile.Verdict,
			Clean:   bf.CPUProfile.Clean,
			Method:  bf.CPUProfile.Method,
		}
	}
	if bf.Definitive != nil {
		sum.definitive = bf.Definitive
	}
	if bf.KeyBudget != nil {
		sum.keyBudget = bf.KeyBudget
	}
	if bf.GlobalBudget != nil {
		sum.globalBudget = bf.GlobalBudget
	}
	if bf.Ledger != nil {
		sum.ledger = bf.Ledger
	}
	return sum
}

// benchCanaryClean is the E1 canary rule applied to served
// passes: every pass's direct-mock RPS >= 12,000. A run with no
// passes is not clean — there is no canary to vouch for it.
func benchCanaryClean(passes []benchmarkPass) bool {
	if len(passes) == 0 {
		return false
	}
	for _, p := range passes {
		if p.DirectRPS < benchCanaryRPS {
			return false
		}
	}
	return true
}

// benchPlainVerdict is the server-computed target verdict for a
// plain (unmerged) run: per pass, whether added latency and RPS
// met the Geekflare targets. Computed from the recorded numbers
// only — a plain run carries no recorded verdict text.
func benchPlainVerdict(passes []benchmarkPass) string {
	if len(passes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(passes))
	for _, p := range passes {
		lat := "MISSED"
		if p.AddedLatencyMet {
			lat = "MET"
		}
		rps := "MISSED"
		if p.RPSMet {
			rps = "MET"
		}
		parts = append(parts, fmt.Sprintf("pass %d: latency %s, RPS %s", p.Pass, lat, rps))
	}
	return strings.Join(parts, "; ")
}

// benchmarkHistoryRun is one recorded bench file as the history
// feed serves it: the file's primary run under the same rule as
// the single-run feed, its kind, its clean flag and its verdict.
type benchmarkHistoryRun struct {
	File    string          `json:"file"`
	Date    string          `json:"date,omitempty"`
	Kind    string          `json:"kind"`
	Clean   bool            `json:"clean"`
	Verdict string          `json:"verdict,omitempty"`
	Fix     string          `json:"fix,omitempty"`
	Passes  []benchmarkPass `json:"passes,omitempty"`
	IdleMB  float64         `json:"idle_mb,omitempty"`
	ReadyMs float64         `json:"ready_ms,omitempty"`
}

type benchmarkHistorySkipped struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type benchmarkHistoryResponse struct {
	Recorded bool                      `json:"recorded"`
	Note     string                    `json:"note,omitempty"`
	Runs     []benchmarkHistoryRun     `json:"runs,omitempty"`
	Skipped  []benchmarkHistorySkipped `json:"skipped,omitempty"`
}

// benchHistoryEntry reduces one decoded bench file to its history
// entry. The most specific record names the kind and its recorded
// verdict/clean win; a plain run gets the canary rule and the
// server-computed target verdicts. A ledger file's verdict is its
// cpu_profile verdict (the ledger record carries no verdict of
// its own) and its fix text rides along.
func benchHistoryEntry(file string, bf benchFile) benchmarkHistoryRun {
	sum := summarizeBenchFile(bf)
	passes := benchmarkPasses(sum.primary)
	run := benchmarkHistoryRun{
		File:   file,
		Date:   sum.primary.Rig.Date,
		Passes: passes,
		IdleMB: float64(sum.primary.IdleRSSKiB) * 1024 / 1e6,
	}
	readyMs := sum.primary.ReadyMs.Healthz
	if sum.primary.ReadyMs.V1Models > readyMs {
		readyMs = sum.primary.ReadyMs.V1Models
	}
	run.ReadyMs = readyMs
	switch {
	case sum.globalBudget != nil:
		run.Kind = "global-budget"
		run.Clean = sum.globalBudget.Clean
		run.Verdict = sum.globalBudget.Verdict
	case sum.keyBudget != nil:
		run.Kind = "key-budget"
		run.Clean = sum.keyBudget.Clean
		run.Verdict = sum.keyBudget.Verdict
	case sum.definitive != nil:
		run.Kind = "definitive"
		run.Clean = sum.definitive.Clean
		run.Verdict = sum.definitive.Verdict
	case sum.ledger != nil:
		run.Kind = "ledger"
		if sum.cpu != nil {
			run.Clean = sum.cpu.Clean
			run.Verdict = sum.cpu.Verdict
		}
		run.Fix = sum.ledger.Fix
	case sum.cpu != nil:
		run.Kind = "cpu"
		run.Clean = sum.cpu.Clean
		run.Verdict = sum.cpu.Verdict
	case sum.comparison != nil:
		run.Kind = "limiter"
		run.Clean = sum.comparison.Clean
		run.Verdict = sum.comparison.Verdict
	default:
		run.Kind = "run"
		run.Clean = benchCanaryClean(passes)
		run.Verdict = benchPlainVerdict(passes)
	}
	return run
}

// handleDashboardBenchmarkHistory serves GET
// /dashboard/benchmark/history: every recorded bench file in the
// history directory, oldest first. Files are served verbatim —
// the entry is a reduction of the recorded file, never invented
// numbers. A file that cannot be read or parsed, or records no
// passes, is listed under "skipped" with the reason instead of
// killing the feed or being shown as data.
func (s *Server) handleDashboardBenchmarkHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	noHistory := func() {
		writeJSON(w, http.StatusOK, benchmarkHistoryResponse{
			Recorded: false,
			Note:     "No benchmark history recorded yet — set CODVIAN_BENCH_HISTORY_DIR to the directory holding the recorded benchmark-*.json files (the repo's docs/ in practice).",
		})
	}
	if s.benchmarkHistoryDir == "" {
		noHistory()
		return
	}
	entries, err := os.ReadDir(s.benchmarkHistoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			noHistory()
			return
		}
		s.logger.Printf("benchmark history: cannot read %s: %v", s.benchmarkHistoryDir, err)
		writeError(w, http.StatusInternalServerError, "benchmark history directory unreadable")
		return
	}
	res := benchmarkHistoryResponse{Recorded: true}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "benchmark-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.benchmarkHistoryDir, name))
		if err != nil {
			res.Skipped = append(res.Skipped, benchmarkHistorySkipped{File: name, Reason: "unreadable: " + err.Error()})
			continue
		}
		var bf benchFile
		if err := json.Unmarshal(raw, &bf); err != nil {
			res.Skipped = append(res.Skipped, benchmarkHistorySkipped{File: name, Reason: "corrupt JSON: " + err.Error()})
			continue
		}
		run := benchHistoryEntry(name, bf)
		if len(run.Passes) == 0 {
			res.Skipped = append(res.Skipped, benchmarkHistorySkipped{File: name, Reason: "no passes recorded"})
			continue
		}
		res.Runs = append(res.Runs, run)
	}
	// Oldest first: the history reads as a timeline. A file
	// with no parseable date sorts after the dated runs; ties
	// break by filename so the order is stable.
	sort.SliceStable(res.Runs, func(i, j int) bool {
		di, erri := time.Parse(time.RFC3339, res.Runs[i].Date)
		dj, errj := time.Parse(time.RFC3339, res.Runs[j].Date)
		if erri != nil && errj != nil {
			return res.Runs[i].File < res.Runs[j].File
		}
		if erri != nil {
			return false
		}
		if errj != nil {
			return true
		}
		if di.Equal(dj) {
			return res.Runs[i].File < res.Runs[j].File
		}
		return di.Before(dj)
	})
	if len(res.Runs) == 0 && len(res.Skipped) == 0 {
		noHistory()
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleDashboardBenchmark serves GET /dashboard/benchmark.
func (s *Server) handleDashboardBenchmark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	noRun := func() {
		writeJSON(w, http.StatusOK, benchmarkResponse{
			Recorded: false,
			Note:     "No benchmark run recorded yet — run scripts/bench-geekflare.sh (with CODVIAN_BENCH_OUT=<data-dir>/benchmark.json) to record one.",
		})
	}
	if s.benchmarkFile == "" {
		noRun()
		return
	}
	raw, err := os.ReadFile(s.benchmarkFile)
	if err != nil {
		if os.IsNotExist(err) {
			noRun()
			return
		}
		s.logger.Printf("benchmark feed: cannot read %s: %v", s.benchmarkFile, err)
		writeError(w, http.StatusInternalServerError, "benchmark file unreadable")
		return
	}
	var bf benchFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		s.logger.Printf("benchmark feed: corrupt %s: %v", s.benchmarkFile, err)
		writeError(w, http.StatusInternalServerError, "benchmark file is corrupt — re-run scripts/bench-geekflare.sh")
		return
	}
	// The primary run and the records follow the shared rule in
	// summarizeBenchFile — the same rule the history feed uses,
	// so the two feeds cannot disagree about which run a merged
	// file's top level describes.
	sum := summarizeBenchFile(bf)
	primary := sum.primary
	limiterMode := sum.limiterMode
	comp := sum.comparison
	cpu := sum.cpu
	def := sum.definitive
	kb := sum.keyBudget
	gb := sum.globalBudget
	passes := benchmarkPasses(primary)
	readyMs := primary.ReadyMs.Healthz
	if primary.ReadyMs.V1Models > readyMs {
		readyMs = primary.ReadyMs.V1Models
	}
	writeJSON(w, http.StatusOK, benchmarkResponse{
		Recorded:       true,
		Date:           primary.Rig.Date,
		RigOS:          primary.Rig.OS,
		RigCPUs:        primary.Rig.CPUs,
		RigCPUModel:    primary.Rig.CPUModel,
		RigGoVersion:   primary.Rig.GoVer,
		RigNote:        primary.Rig.Note,
		ReadyHealthzMs: primary.ReadyMs.Healthz,
		ReadyModelsMs:  primary.ReadyMs.V1Models,
		ReadyMet:       readyMs < benchTargetReadyMs,
		IdleMB:         float64(primary.IdleRSSKiB) * 1024 / 1e6,
		IdleMet:        float64(primary.IdleRSSKiB)*1024/1e6 < benchTargetIdleMB,
		Passes:         passes,
		Targets: benchmarkTargets{
			AddedLatencyP50Ms: benchTargetAddedLatencyMs,
			RPSOk:             benchTargetRPS,
			IdleMB:            benchTargetIdleMB,
			ReadyMs:           benchTargetReadyMs,
		},
		LimiterMode:       limiterMode,
		LimiterComparison: comp,
		CPUProfile:        cpu,
		Definitive:        def,
		KeyBudget:         kb,
		GlobalBudget:      gb,
	})
}
