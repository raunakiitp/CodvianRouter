package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func getBenchmark(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodGet, "/dashboard/benchmark", "", "")
}

// TestDashboardBenchmarkRealRun serves the real measured 2026-11-18
// bench file (copied verbatim into testdata) and checks the feed
// reports those exact numbers with the right target verdicts —
// latency and RPS missed, idle and ready met. No value in the
// response is invented: every number traces to the file.
func TestDashboardBenchmarkRealRun(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-2026-11-18.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true for a real bench file")
	}
	if res.Date != "2026-11-17T18:32:15+00:00" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.RigCPUs != 4 || res.RigCPUModel == "" || res.RigGoVersion == "" {
		t.Errorf("rig = %+v, want the run's rig facts", res)
	}
	if len(res.Passes) != 2 || res.Passes[0].Pass != 1 || res.Passes[1].Pass != 2 {
		t.Fatalf("passes = %+v, want passes 1 and 2 in order", res.Passes)
	}
	p1 := res.Passes[0]
	if p1.AddedLatencyP50Ms != 0.677 || p1.AddedLatencyMet {
		t.Errorf("pass 1 added latency = %v met=%v, want 0.677 met=false", p1.AddedLatencyP50Ms, p1.AddedLatencyMet)
	}
	if p1.RPSOk != 1500.473 || p1.RPSMet {
		t.Errorf("pass 1 RPS = %v met=%v, want 1500.473 met=false", p1.RPSOk, p1.RPSMet)
	}
	if p1.Errors != 27811 || p1.Completed != 57825 {
		t.Errorf("pass 1 errors/completed = %d/%d, want 27811/57825", p1.Errors, p1.Completed)
	}
	if p1.ErrorRatePct < 48.0 || p1.ErrorRatePct > 48.2 {
		t.Errorf("pass 1 error rate = %v%%, want ~48.1%%", p1.ErrorRatePct)
	}
	if p1.CacheHits != 6739 {
		t.Errorf("pass 1 cache_hits = %d, want 6739", p1.CacheHits)
	}
	p2 := res.Passes[1]
	if p2.AddedLatencyP50Ms != 0.511 || p2.AddedLatencyMet {
		t.Errorf("pass 2 added latency = %v met=%v, want 0.511 met=false", p2.AddedLatencyP50Ms, p2.AddedLatencyMet)
	}
	if p2.RPSOk != 1424.593 || p2.RPSMet {
		t.Errorf("pass 2 RPS = %v met=%v, want 1424.593 met=false", p2.RPSOk, p2.RPSMet)
	}
	if res.IdleMB < 39.0 || res.IdleMB > 39.5 || !res.IdleMet {
		t.Errorf("idle = %v MB met=%v, want ~39.2 MB met=true", res.IdleMB, res.IdleMet)
	}
	if !res.ReadyMet || res.ReadyHealthzMs != 158.5 || res.ReadyModelsMs != 309.9 {
		t.Errorf("ready = %v/%v met=%v, want 158.5/309.9 met=true", res.ReadyHealthzMs, res.ReadyModelsMs, res.ReadyMet)
	}
	if res.Targets.AddedLatencyP50Ms != 0.5 || res.Targets.RPSOk != 3000 || res.Targets.IdleMB != 70 || res.Targets.ReadyMs != 10000 {
		t.Errorf("targets = %+v, want 0.5ms / 3000 RPS / 70 MB / 10000 ms", res.Targets)
	}
}

func TestDashboardBenchmarkNoRun(t *testing.T) {
	// No file configured at all.
	srv := newTestServer(t, &fakeEngine{})
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Recorded || res.Note == "" {
		t.Errorf("response = %+v, want recorded=false with a note", res)
	}
	// Configured path, file missing: same honest empty state.
	dir := t.TempDir()
	srv2 := newTestServer(t, &fakeEngine{}, WithBenchmarkFile(filepath.Join(dir, "benchmark.json")))
	rec2 := getBenchmark(t, srv2)
	var res2 benchmarkResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res2.Recorded || res2.Note == "" {
		t.Errorf("response = %+v, want recorded=false with a note for a missing file", res2)
	}
}

func TestDashboardBenchmarkCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "benchmark.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile(path))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a corrupt bench file: %s", rec.Code, rec.Body.String())
	}
}

// TestDashboardBenchmarkE1Comparison serves the real merged E1 file
// (testdata/benchmark-e1.json — the 2026-11-18 limiter-on vs
// limiter-off pair, trimmed to the fields the feed decodes, values
// untouched) and checks the feed serves both modes side by side
// with the recorded verdict. The pair is half-polluted (on pass 1
// failed the direct-mock canary), so clean=false and the verdict
// text says it is a pass-2-only verdict — the feed must surface
// that honestly, never as a fully clean comparison. Top-level
// passes/ready/idle describe the limiter-ON run (the default mode).
func TestDashboardBenchmarkE1Comparison(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-e1.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded || res.LimiterMode != "comparison" {
		t.Fatalf("recorded=%v limiter_mode=%q, want recorded=true comparison", res.Recorded, res.LimiterMode)
	}
	if res.LimiterComparison == nil {
		t.Fatal("limiter_comparison = nil, want the E1 pair")
	}
	comp := res.LimiterComparison
	if comp.Clean {
		t.Errorf("clean = true, want false (on pass 1 was polluted)")
	}
	if !strings.Contains(comp.Verdict, "NOT the RPS ceiling") || !strings.Contains(comp.Verdict, "pass 2 only") {
		t.Errorf("verdict = %q, want the pass-2-only NOT-the-ceiling verdict", comp.Verdict)
	}
	// Top level = limiter-ON run: pass 1 polluted (1.162 ms, 585 RPS).
	if len(res.Passes) != 2 || res.Passes[0].AddedLatencyP50Ms != 1.162 || res.Passes[0].RPSOk != 585.012 {
		t.Errorf("top-level passes = %+v, want the limiter-ON run (pass 1: 1.162 ms / 585.012 RPS)", res.Passes)
	}
	if len(comp.On) != 2 || len(comp.Off) != 2 {
		t.Fatalf("comparison passes on/off = %d/%d, want 2/2", len(comp.On), len(comp.Off))
	}
	on2, off2 := comp.On[1], comp.Off[1]
	if on2.AddedLatencyP50Ms != 0.68 || on2.RPSOk != 1324.192 || on2.Errors != 5141 {
		t.Errorf("on pass 2 = %+v, want 0.680 ms / 1324.192 RPS / 5141 errors", on2)
	}
	if on2.ErrorRatePct < 16.1 || on2.ErrorRatePct > 16.3 {
		t.Errorf("on pass 2 error rate = %v%%, want ~16.2%%", on2.ErrorRatePct)
	}
	if off2.AddedLatencyP50Ms != 0.781 || off2.RPSOk != 1419.285 || off2.Errors != 0 {
		t.Errorf("off pass 2 = %+v, want 0.781 ms / 1419.285 RPS / 0 errors", off2)
	}
	if off2.ErrorRatePct != 0 {
		t.Errorf("off pass 2 error rate = %v%%, want 0 (nothing shed with no limiter)", off2.ErrorRatePct)
	}
	if off2.DirectRPS != 19387.75 || on2.DirectRPS != 19221.508 {
		t.Errorf("pass-2 canaries on/off = %v/%v, want 19221.508/19387.75", on2.DirectRPS, off2.DirectRPS)
	}
}

// TestDashboardBenchmarkCPUProfile serves the real merged CPU file
// (testdata/benchmark-cpu.json — the 2026-11-18 hot-path before/after
// pair, trimmed to the fields the feed decodes, values untouched)
// and checks the feed serves both runs side by side with the
// recorded verdict. The pair is clean by the canary rule
// (direct-mock canary >= 12,000 RPS in every pass), so clean=true —
// but the RPS passes disagree (+138.5% pass 1, -14.9% pass 2), so
// the verdict text quotes both and the report (not the feed) says
// the RPS delta is load-dominated; the feed's job is only to serve
// the recorded numbers. Top-level passes/ready/idle describe the
// AFTER run (the current tree).
func TestDashboardBenchmarkCPUProfile(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-cpu.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true")
	}
	if res.CPUProfile == nil {
		t.Fatal("cpu_profile = nil, want the CPU before/after pair")
	}
	cpu := res.CPUProfile
	if !cpu.Clean {
		t.Errorf("clean = false, want true (all four canaries >= 12,000 RPS)")
	}
	if !strings.Contains(cpu.Verdict, "0.708 -> 0.697 ms") || !strings.Contains(cpu.Verdict, "692 -> 1650") || !strings.Contains(cpu.Verdict, "still MISSED") {
		t.Errorf("verdict = %q, want the recorded before/after deltas + still-MISSED targets", cpu.Verdict)
	}
	// Top level = AFTER run: pass 1 (0.697 ms, 1650.054 RPS).
	if len(res.Passes) != 2 || res.Passes[0].AddedLatencyP50Ms != 0.697 || res.Passes[0].RPSOk != 1650.054 {
		t.Errorf("top-level passes = %+v, want the AFTER run (pass 1: 0.697 ms / 1650.054 RPS)", res.Passes)
	}
	if len(cpu.Before) != 2 || len(cpu.After) != 2 {
		t.Fatalf("cpu passes before/after = %d/%d, want 2/2", len(cpu.Before), len(cpu.After))
	}
	b1, a1 := cpu.Before[0], cpu.After[0]
	if b1.AddedLatencyP50Ms != 0.708 || b1.RPSOk != 691.88 {
		t.Errorf("before pass 1 = %+v, want 0.708 ms / 691.88 RPS", b1)
	}
	if a1.AddedLatencyP50Ms != 0.697 || a1.RPSOk != 1650.054 {
		t.Errorf("after pass 1 = %+v, want 0.697 ms / 1650.054 RPS", a1)
	}
	a2 := cpu.After[1]
	if a2.AddedLatencyP50Ms != 0.57 || a2.RPSOk != 650.231 {
		t.Errorf("after pass 2 = %+v, want 0.570 ms / 650.231 RPS", a2)
	}
}

// TestDashboardBenchmarkDefinitive serves the real merged
// definitive file (testdata/benchmark-definitive.json — the
// 2026-11-18 re-measurement run with the E1 verdict + CPU-profile
// fixes in, copied verbatim, values untouched) and checks the feed
// serves the new run with its clean flag, verdict and load trace.
// The run is clean by the canary rule (direct-mock canary 15,205 /
// 16,756 RPS), but both Geekflare targets are still MISSED — the
// feed must surface the miss, never a pass. Top-level
// passes/ready/idle describe the new run, and the carried-over
// cpu_profile card is still served alongside it.
func TestDashboardBenchmarkDefinitive(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-definitive.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true")
	}
	if res.Definitive == nil {
		t.Fatal("definitive_remeasurement = nil, want the re-measurement record")
	}
	def := res.Definitive
	if !def.Clean {
		t.Errorf("clean = false, want true (both canaries >= 12,000 RPS)")
	}
	if !strings.Contains(def.Verdict, "0.908 ms pass 1") || !strings.Contains(def.Verdict, "still MISSED") {
		t.Errorf("verdict = %q, want the recorded pair + still-MISSED targets", def.Verdict)
	}
	if len(def.LoadTrace.Start) != 3 || def.LoadTrace.Start[0] != "1.45" || len(def.LoadTrace.End) != 3 || def.LoadTrace.End[0] != "4.84" {
		t.Errorf("load_trace = %+v, want start 1.45/... end 4.84/...", def.LoadTrace)
	}
	// Top level = the new run: pass 1 (0.908 ms, 1212.934 RPS).
	if len(res.Passes) != 2 || res.Passes[0].AddedLatencyP50Ms != 0.908 || res.Passes[0].RPSOk != 1212.934 {
		t.Errorf("top-level passes = %+v, want the new run (pass 1: 0.908 ms / 1212.934 RPS)", res.Passes)
	}
	p2 := res.Passes[1]
	if p2.AddedLatencyP50Ms != 0.711 || p2.RPSOk != 962.95 {
		t.Errorf("pass 2 = %+v, want 0.711 ms / 962.95 RPS", p2)
	}
	if p2.Errors != 4791 || p2.Completed != 24086 {
		t.Errorf("pass 2 errors/completed = %d/%d, want 4791/24086", p2.Errors, p2.Completed)
	}
	if res.Passes[0].AddedLatencyMet || res.Passes[0].RPSMet || p2.AddedLatencyMet || p2.RPSMet {
		t.Errorf("target verdicts = %+v, want all MISSED (0.908/0.711 ms, 1212.934/962.95 RPS)", res.Passes)
	}
	if res.Date != "2026-11-18T06:13:19+00:00" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.IdleMB < 43.4 || res.IdleMB > 43.6 || !res.IdleMet {
		t.Errorf("idle = %v MB met=%v, want ~43.5 MB met=true", res.IdleMB, res.IdleMet)
	}
	if !res.ReadyMet || res.ReadyHealthzMs != 464.2 || res.ReadyModelsMs != 642.9 {
		t.Errorf("ready = %v/%v met=%v, want 464.2/642.9 met=true", res.ReadyHealthzMs, res.ReadyModelsMs, res.ReadyMet)
	}
	// The carried-over CPU before/after card is still served.
	if res.CPUProfile == nil {
		t.Fatal("cpu_profile = nil, want the carried-over CPU before/after pair")
	}
	if !res.CPUProfile.Clean || len(res.CPUProfile.After) != 2 || res.CPUProfile.After[0].RPSOk != 1650.054 {
		t.Errorf("cpu_profile = %+v, want the clean carried-over pair (after pass 1: 1650.054 RPS)", res.CPUProfile)
	}
}

// TestDashboardBenchmarkKeyBudget serves the real merged
// key-budget file (testdata/benchmark-keybudget.json — the
// 2026-11-18 re-measurement run with the per-request key-budget
// memo in, copied verbatim, values untouched) and checks the feed
// serves the new run with its clean flag, verdict, load trace and
// the before/after GetKeyBudget profile shares. The run is clean
// by the canary rule (direct-mock canary 21,732 / 21,805 RPS),
// but both Geekflare targets are still MISSED — the feed must
// surface the miss, never a pass. Top-level passes/ready/idle
// describe the new run, and the carried-over definitive and CPU
// cards are still served alongside it.
func TestDashboardBenchmarkKeyBudget(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-keybudget.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true")
	}
	if res.KeyBudget == nil {
		t.Fatal("keybudget_remeasurement = nil, want the re-measurement record")
	}
	kb := res.KeyBudget
	if !kb.Clean {
		t.Errorf("clean = false, want true (both canaries >= 12,000 RPS)")
	}
	if !strings.Contains(kb.Verdict, "0.842 ms pass 1") || !strings.Contains(kb.Verdict, "still MISSED") {
		t.Errorf("verdict = %q, want the recorded pair + still-MISSED targets", kb.Verdict)
	}
	if !strings.Contains(kb.Verdict, "11.54% cum") || !strings.Contains(kb.Verdict, "7.52% cum") {
		t.Errorf("verdict = %q, want the before/after GetKeyBudget shares (11.54%% -> 7.52%%)", kb.Verdict)
	}
	if kb.GetKeyBudgetBeforePct != 11.54 || kb.GetKeyBudgetAfterPct != 7.52 {
		t.Errorf("get_key_budget before/after = %v/%v, want 11.54/7.52", kb.GetKeyBudgetBeforePct, kb.GetKeyBudgetAfterPct)
	}
	if !strings.Contains(kb.TopCostAfter, "CheckBudgetOrError") || !strings.Contains(kb.TopCostAfter, "8.20%") {
		t.Errorf("top_cost_after = %q, want the global-budget read (CheckBudgetOrError 8.20%%) as the new top named cost", kb.TopCostAfter)
	}
	if len(kb.LoadTrace.Start) != 3 || kb.LoadTrace.Start[0] != "1.05" || len(kb.LoadTrace.End) != 3 || kb.LoadTrace.End[0] != "0.91" {
		t.Errorf("load_trace = %+v, want start 1.05/... end 0.91/...", kb.LoadTrace)
	}
	// Top level = the new run: pass 1 (0.842 ms, 1451.386 RPS).
	if len(res.Passes) != 2 || res.Passes[0].AddedLatencyP50Ms != 0.842 || res.Passes[0].RPSOk != 1451.386 {
		t.Errorf("top-level passes = %+v, want the new run (pass 1: 0.842 ms / 1451.386 RPS)", res.Passes)
	}
	p2 := res.Passes[1]
	if p2.AddedLatencyP50Ms != 0.691 || p2.RPSOk != 1431.722 {
		t.Errorf("pass 2 = %+v, want 0.691 ms / 1431.722 RPS", p2)
	}
	if p2.Errors != 4394 || p2.Completed != 33113 {
		t.Errorf("pass 2 errors/completed = %d/%d, want 4394/33113", p2.Errors, p2.Completed)
	}
	if res.Passes[0].AddedLatencyMet || res.Passes[0].RPSMet || p2.AddedLatencyMet || p2.RPSMet {
		t.Errorf("target verdicts = %+v, want all MISSED (0.842/0.691 ms, 1451.386/1431.722 RPS)", res.Passes)
	}
	if res.Date != "2026-11-18T07:11:19+00:00" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.IdleMB < 43.8 || res.IdleMB > 43.9 || !res.IdleMet {
		t.Errorf("idle = %v MB met=%v, want ~43.8 MB met=true", res.IdleMB, res.IdleMet)
	}
	if !res.ReadyMet || res.ReadyHealthzMs != 441.1 || res.ReadyModelsMs != 558.6 {
		t.Errorf("ready = %v/%v met=%v, want 441.1/558.6 met=true", res.ReadyHealthzMs, res.ReadyModelsMs, res.ReadyMet)
	}
	// The carried-over definitive and CPU cards are still served.
	if res.Definitive == nil {
		t.Fatal("definitive_remeasurement = nil, want the carried-over definitive record")
	}
	if res.CPUProfile == nil {
		t.Fatal("cpu_profile = nil, want the carried-over CPU before/after pair")
	}
	if !res.CPUProfile.Clean || len(res.CPUProfile.After) != 2 || res.CPUProfile.After[0].RPSOk != 1650.054 {
		t.Errorf("cpu_profile = %+v, want the clean carried-over pair (after pass 1: 1650.054 RPS)", res.CPUProfile)
	}
}

// TestDashboardBenchmarkHistoryFixture serves a small directory of
// recorded files (testdata/history: one plain run, one merged CPU
// before/after file, one corrupt file) and checks the history feed
// lists every recorded run oldest first with its kind, clean flag,
// verdict and passes — and lists the corrupt file under "skipped"
// with a reason instead of failing the feed or showing it as data.
func TestDashboardBenchmarkHistoryFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkHistoryDir("testdata/history"))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/benchmark/history", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true for a directory of recorded files")
	}
	if len(res.Runs) != 2 {
		t.Fatalf("runs = %d, want 2 (plain run + merged CPU file)", len(res.Runs))
	}
	// Oldest first: the 2026-11-17 plain run, then the CPU file.
	plain := res.Runs[0]
	if plain.File != "benchmark-2026-11-17.json" || plain.Kind != "run" {
		t.Errorf("runs[0] = %+v, want the 2026-11-17 plain run", plain)
	}
	if plain.Date != "2026-11-17T17:55:45+00:00" {
		t.Errorf("plain date = %q, want the run's date", plain.Date)
	}
	// Plain run: clean by the canary rule (direct mock 15,000 /
	// 16,000 RPS), verdict is the server-computed target verdicts.
	if !plain.Clean {
		t.Errorf("plain clean = false, want true (both canaries >= 12,000 RPS)")
	}
	if !strings.Contains(plain.Verdict, "pass 1: latency MISSED, RPS MISSED") || !strings.Contains(plain.Verdict, "pass 2: latency MISSED, RPS MISSED") {
		t.Errorf("plain verdict = %q, want the computed per-pass target verdicts", plain.Verdict)
	}
	if len(plain.Passes) != 2 || plain.Passes[0].AddedLatencyP50Ms != 0.9 || plain.Passes[0].RPSOk != 1200.0 || plain.Passes[1].AddedLatencyP50Ms != 0.8 || plain.Passes[1].RPSOk != 1100.0 {
		t.Errorf("plain passes = %+v, want 0.9 ms / 1200 RPS then 0.8 ms / 1100 RPS", plain.Passes)
	}
	if plain.IdleMB < 38.9 || plain.IdleMB > 39.0 || plain.ReadyMs != 300.0 {
		t.Errorf("plain idle/ready = %v MB / %v ms, want ~38.9 MB / 300 ms", plain.IdleMB, plain.ReadyMs)
	}
	cpu := res.Runs[1]
	if cpu.File != "benchmark-2026-11-18-cpu.json" || cpu.Kind != "cpu" {
		t.Errorf("runs[1] = %+v, want the merged CPU file, kind cpu", cpu)
	}
	// Merged CPU file: the entry describes the AFTER run (the
	// primary under the single-run rule) and carries the recorded
	// verdict + clean flag.
	if !cpu.Clean || !strings.Contains(cpu.Verdict, "0.708 -> 0.697 ms") || !strings.Contains(cpu.Verdict, "still MISSED") {
		t.Errorf("cpu clean=%v verdict=%q, want the recorded clean verdict", cpu.Clean, cpu.Verdict)
	}
	if len(cpu.Passes) != 2 || cpu.Passes[0].AddedLatencyP50Ms != 0.697 || cpu.Passes[0].RPSOk != 1650.054 {
		t.Errorf("cpu passes = %+v, want the AFTER run (0.697 ms / 1650.054 RPS)", cpu.Passes)
	}
	// The corrupt file is skipped, visibly, with a reason.
	if len(res.Skipped) != 1 || res.Skipped[0].File != "benchmark-2026-11-18-broken.json" || !strings.Contains(res.Skipped[0].Reason, "corrupt") {
		t.Errorf("skipped = %+v, want the broken file listed as corrupt", res.Skipped)
	}
}

func TestDashboardBenchmarkHistoryNoDir(t *testing.T) {
	// No directory configured at all.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/benchmark/history", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Recorded || res.Note == "" {
		t.Errorf("response = %+v, want recorded=false with a note", res)
	}
	// Configured directory, missing on disk: same honest empty state.
	dir := t.TempDir()
	srv2 := newTestServer(t, &fakeEngine{}, WithBenchmarkHistoryDir(filepath.Join(dir, "missing")))
	rec2 := doReq(t, srv2, http.MethodGet, "/dashboard/benchmark/history", "", "")
	var res2 benchmarkHistoryResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res2.Recorded || res2.Note == "" {
		t.Errorf("response = %+v, want recorded=false with a note for a missing directory", res2)
	}
	// Directory exists but holds no benchmark files: same again.
	srv3 := newTestServer(t, &fakeEngine{}, WithBenchmarkHistoryDir(dir))
	rec3 := doReq(t, srv3, http.MethodGet, "/dashboard/benchmark/history", "", "")
	var res3 benchmarkHistoryResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &res3); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res3.Recorded || res3.Note == "" {
		t.Errorf("response = %+v, want recorded=false with a note for an empty directory", res3)
	}
}

// TestDashboardBenchmarkHistoryRealDocs serves the repo's actual
// recorded set (docs/benchmark-*.json) and checks every file
// parses into a history entry — no file skipped — with the four
// merged kinds classified from their recorded records. The count
// is not pinned (later runs add files); the kinds and the
// recorded verdicts/clean flags of the known merged files are.
func TestDashboardBenchmarkHistoryRealDocs(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkHistoryDir("../../docs"))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/benchmark/history", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true for docs/")
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none — every recorded file must parse", res.Skipped)
	}
	if len(res.Runs) < 30 {
		t.Fatalf("runs = %d, want at least 30 recorded runs", len(res.Runs))
	}
	byFile := map[string]benchmarkHistoryRun{}
	for _, run := range res.Runs {
		byFile[run.File] = run
		if run.Kind == "" || run.Date == "" || len(run.Passes) == 0 {
			t.Errorf("run %+v is missing kind/date/passes", run)
		}
	}
	// Oldest first: the first entry is the earliest dated run.
	if res.Runs[0].Date != "2026-11-17T17:55:45+00:00" {
		t.Errorf("first run date = %q, want the 2026-11-17 run first", res.Runs[0].Date)
	}
	// The merged files classify from their recorded records.
	kb, ok := byFile["benchmark-2026-11-18-keybudget.json"]
	if !ok || kb.Kind != "key-budget" || !kb.Clean || !strings.Contains(kb.Verdict, "0.842 ms pass 1") {
		t.Errorf("keybudget entry = %+v, want kind key-budget, clean, recorded verdict", kb)
	}
	def, ok := byFile["benchmark-2026-11-18-cpu2.json"]
	if !ok || def.Kind != "definitive" || !def.Clean || !strings.Contains(def.Verdict, "0.908 ms pass 1") {
		t.Errorf("cpu2 entry = %+v, want kind definitive, clean, recorded verdict", def)
	}
	led, ok := byFile["benchmark-2026-11-18-ledger.json"]
	if !ok || led.Kind != "ledger" || !led.Clean || !strings.Contains(led.Verdict, "Ledger write-path fix") || !strings.Contains(led.Fix, "multi-row INSERT") {
		t.Errorf("ledger entry = %+v, want kind ledger, clean, recorded verdict + fix", led)
	}
	e1, ok := byFile["benchmark-2026-11-18-e1-polluted.json"]
	if !ok || e1.Kind != "limiter" || e1.Clean || !strings.Contains(e1.Verdict, "NOT the RPS ceiling") {
		t.Errorf("e1 entry = %+v, want kind limiter, polluted, recorded verdict", e1)
	}
	// A plain run keeps its recorded numbers and its filename
	// (the "polluted" marker stays visible in the file name).
	plain, ok := byFile["benchmark-2026-11-18.json"]
	if !ok || plain.Kind != "run" || !plain.Clean || len(plain.Passes) != 2 || plain.Passes[0].AddedLatencyP50Ms != 0.677 || plain.Passes[0].RPSOk != 1500.473 {
		t.Errorf("plain entry = %+v, want kind run, clean, 0.677 ms / 1500.473 RPS pass 1", plain)
	}
}

// TestDashboardBenchmarkGlobalBudget serves the real merged
// global-budget file (testdata/benchmark-globalbudget.json — the
// 2026-11-18 re-measurement run with the per-request
// global-budget memo in, copied verbatim, values untouched) and
// checks the feed serves the new run with its clean flag, verdict,
// load trace and the before/after GetBudget profile shares. The
// run is clean by the canary rule (direct-mock canary 19,599 /
// 20,009 RPS), but both Geekflare targets are still MISSED — the
// feed must surface the miss, never a pass. Top-level
// passes/ready/idle describe the new run, and the carried-over
// definitive, CPU and key-budget cards are still served alongside
// it. The profile move is within noise by design (see
// internal/ledger/budget_memo.go: on today's hot paths the guard
// reads once per request, so the memo has no second read to save)
// — the test pins the measured shares, not a claimed win.
func TestDashboardBenchmarkGlobalBudget(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBenchmarkFile("testdata/benchmark-globalbudget.json"))
	rec := getBenchmark(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true")
	}
	if res.GlobalBudget == nil {
		t.Fatal("globalbudget_remeasurement = nil, want the re-measurement record")
	}
	gb := res.GlobalBudget
	if !gb.Clean {
		t.Errorf("clean = false, want true (both canaries >= 12,000 RPS)")
	}
	if !strings.Contains(gb.Verdict, "0.851 ms pass 1") || !strings.Contains(gb.Verdict, "still MISSED") {
		t.Errorf("verdict = %q, want the recorded pair + still-MISSED targets", gb.Verdict)
	}
	if !strings.Contains(gb.Verdict, "8.20% cum") || !strings.Contains(gb.Verdict, "7.96% cum") {
		t.Errorf("verdict = %q, want the before/after GetBudget shares (8.20%% -> 7.96%%)", gb.Verdict)
	}
	if gb.GetBudgetBeforePct != 8.20 || gb.GetBudgetAfterPct != 7.96 {
		t.Errorf("get_budget before/after = %v/%v, want 8.20/7.96", gb.GetBudgetBeforePct, gb.GetBudgetAfterPct)
	}
	if !strings.Contains(gb.TopCostAfter, "GetKeyBudget") || !strings.Contains(gb.TopCostAfter, "8.27%") {
		t.Errorf("top_cost_after = %q, want the key-budget read (GetKeyBudget 8.27%%) as the new top named cost", gb.TopCostAfter)
	}
	if len(gb.LoadTrace.Start) != 3 || gb.LoadTrace.Start[0] != "1.30" || len(gb.LoadTrace.End) != 3 || gb.LoadTrace.End[0] != "1.48" {
		t.Errorf("load_trace = %+v, want start 1.30/... end 1.48/...", gb.LoadTrace)
	}
	// Top level = the new run: pass 1 (0.851 ms, 1273.018 RPS).
	if len(res.Passes) != 2 || res.Passes[0].AddedLatencyP50Ms != 0.851 || res.Passes[0].RPSOk != 1273.018 {
		t.Errorf("top-level passes = %+v, want the new run (pass 1: 0.851 ms / 1273.018 RPS)", res.Passes)
	}
	p2 := res.Passes[1]
	if p2.AddedLatencyP50Ms != 0.777 || p2.RPSOk != 1173.999 {
		t.Errorf("pass 2 = %+v, want 0.777 ms / 1173.999 RPS", p2)
	}
	if p2.Errors != 5019 || p2.Completed != 28517 {
		t.Errorf("pass 2 errors/completed = %d/%d, want 5019/28517", p2.Errors, p2.Completed)
	}
	if res.Passes[0].AddedLatencyMet || res.Passes[0].RPSMet || p2.AddedLatencyMet || p2.RPSMet {
		t.Errorf("target verdicts = %+v, want all MISSED (0.851/0.777 ms, 1273.018/1173.999 RPS)", res.Passes)
	}
	if res.Date != "2026-11-18T07:52:12+00:00" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.IdleMB < 44.0 || res.IdleMB > 44.2 || !res.IdleMet {
		t.Errorf("idle = %v MB met=%v, want ~44.1 MB met=true", res.IdleMB, res.IdleMet)
	}
	if !res.ReadyMet || res.ReadyHealthzMs != 434.8 || res.ReadyModelsMs != 565.0 {
		t.Errorf("ready = %v/%v met=%v, want 434.8/565.0 met=true", res.ReadyHealthzMs, res.ReadyModelsMs, res.ReadyMet)
	}
	// The carried-over definitive, CPU and key-budget cards are
	// still served.
	if res.Definitive == nil {
		t.Fatal("definitive_remeasurement = nil, want the carried-over definitive record")
	}
	if res.CPUProfile == nil {
		t.Fatal("cpu_profile = nil, want the carried-over CPU before/after pair")
	}
	if !res.CPUProfile.Clean || len(res.CPUProfile.After) != 2 || res.CPUProfile.After[0].RPSOk != 1650.054 {
		t.Errorf("cpu_profile = %+v, want the clean carried-over pair (after pass 1: 1650.054 RPS)", res.CPUProfile)
	}
	if res.KeyBudget == nil {
		t.Fatal("keybudget_remeasurement = nil, want the carried-over key-budget record")
	}
	if !res.KeyBudget.Clean || res.KeyBudget.GetKeyBudgetBeforePct != 11.54 || res.KeyBudget.GetKeyBudgetAfterPct != 7.52 {
		t.Errorf("keybudget = %+v, want the clean carried-over record (11.54/7.52)", res.KeyBudget)
	}
}
