# Codvian Router — Geekflare-style benchmark

Measured 2026-11-17 (~23:25 IST) on this VM, re-measured 2026-11-18
~00:02 IST (2026-11-17 18:32 UTC) after the hot-path profiling
fixes, and re-run again 2026-11-18 ~01:16 IST (2026-11-17 19:46 UTC)
with NO code change since the 00:02 run (see "2026-11-18 re-run"
below). Every number below comes from a run of the rig in
`scripts/bench-geekflare.sh` plus the `cmd/codvian-bench` driver;
nothing is estimated or copied from another gateway. Raw
machine-readable results: `docs/benchmark-2026-11-17.json` (pre-fix),
`docs/benchmark-2026-11-18.json` (post-fix),
`docs/benchmark-2026-11-18-rerun.json` (re-run; superseded by attempt 3, see below), and
`docs/benchmark-2026-11-18-before.json` (the 2026-11-18 benchmark
slice's before-run — see "2026-11-18 benchmark slice" below; the
slice's after-run could not be measured honestly, see that section).
Round 2 (2026-11-18, after the item-145 token-counting regression):
`docs/benchmark-2026-11-18-round2-baseline2.json` (baseline, 73.6 ms),
`docs/benchmark-2026-11-18-round2-phase2.json` (encoder-cache fix
only), `docs/benchmark-2026-11-18-round2-phase2b.json` (both fixes —
latest run, contaminated, see the round-2 section). Full narrative:
`docs/benchmark-round2-report.md`.
Attempt 3 (2026-11-18, CHECKLIST 225–226, see the attempt-3 section):
`docs/benchmark-2026-11-18-attempt3.json` (pass 2 clean — the after-run
record for items 149–150 at the time; superseded as latest by the
global-budget run, see "Latest verified numbers" below),
`docs/benchmark-2026-11-18-attempt3-polluted.json` and
`docs/benchmark-2026-11-18-attempt3b-polluted.json` (both fully polluted, kept for the record only).
Later clean runs, each with its own section below:
`docs/benchmark-2026-11-18-cpu2.json` (definitive re-measurement,
CHECKLIST 273–274), `docs/benchmark-2026-11-18-ledger-after.json`
(287–288), `docs/benchmark-2026-11-18-keybudget-run.json` (279–280),
and `docs/benchmark-2026-11-18-globalbudget-run.json` (299–300) — the
latest verified run. The E1 limiter pair is merged in
`docs/benchmark-2026-11-18-e1-polluted.json`.
The dashboard's
Benchmark section (GET /dashboard/benchmark) serves the latest
recorded run with the same target verdicts.

## Targets (stated once)

Targets derived from Geekflare's independent benchmark (research-log
entry 148; see "The Geekflare reference" below): **added latency
< 0.5 ms, RPS > 3,000 on 2 vCPU, idle memory < 70 MB, ready < 10 s.**
These four targets are the only verdicts in this file; every run
section judges itself against them, and the per-run tables keep the
verdict column. Earlier verdict tables in this file (the 2026-11-17
pre-fix table, the attempt-3c table) are history, not the current
verdict — the current verdict is the "Latest verified numbers" table
immediately below.

## Latest verified numbers — 2026-11-18 global-budget run (CHECKLIST 299–300)

Run: 2026-11-18 07:50:39–07:52:12 UTC (13:20–13:22 IST). Command:
`./scripts/bench-geekflare.sh` (the rig in "Methodology" below).
Loadavg 1.30 at start → 1.48 at end (`rig.loadavg_start` /
`rig.loadavg` in the JSON); direct-mock canaries 19,599 / 20,009 RPS
(both clean by the ≥ 12,000 rule); no `go test` / `go build` running;
HEAD `db5c626`. Source: `docs/benchmark-2026-11-18-globalbudget-run.json`
(raw) and merged `docs/benchmark-2026-11-18-globalbudget.json`
(merged file's `globalbudget_remeasurement.clean: true`, verdict
restates 0.851 / 0.777 ms and RPS 1,273 / 1,174).
Every number in this table traces to a field of that raw JSON
(`date`, `loadavg_start`, `loadavg`, `ready_ms`, `idle_rss_kib`,
`passes.1` / `passes.2` `added_latency_p50_ms` and `router_rps`).

| Metric | Geekflare target | Measured (pass 1 / pass 2) | Verdict |
|---|---|---|---|
| Added latency (median) | < 0.5 ms | **0.851 / 0.777 ms** | MISSED (~1.7x / ~1.6x) |
| RPS (32 clients x 20 s) | > 3,000 | **1,273 / 1,174** (ok 25,494 / 23,498; HTTP 502 14.8% / 17.6%) | MISSED (~2.4x / ~2.6x) |
| Idle memory (RSS) | < 70 MB | **42.1 MiB** (43,064 KiB = 44.1 MB) | MET |
| Peak RSS under load | — (not a target) | 78.2 / 108.8 MiB | — |
| Ready time | < 10 s | **435 ms** to first `/healthz` 200, 565 ms to first `/v1/models` 200 | MET (by ~18x) |

Correction to a figure that circulated for this run: idle was quoted
as "~38 MiB" in one place — that is the attempt-3c idle (38.4 MiB),
not this run's. This run's idle is 42.1 MiB (43,064 KiB in the JSON),
still MET. No stormed-run number is presented as final anywhere in
this file: polluted runs are labelled POLLUTED in their own sections
and never in this table.

## Known gaps (2026-11-19)

1. **Latency target missed.** Latest verified 0.851 / 0.777 ms vs
   < 0.5 ms (~1.7x). Every clean run since the 00:02 run sits in the
   0.51–0.91 ms band; no run has met the target.
2. **RPS target missed.** Latest verified 1,273 / 1,174 vs > 3,000
   (~2.4–2.6x). The clean-run band is 963–1,651 RPS; the binding
   constraint is diffuse per-request CPU on this 1.5-shared-vCPU VM
   (E1 verdict: the limiter is NOT the ceiling — removing it bought
   +7.2% on the clean pass-2 pair).
3. **Quiet re-run still pending for items 149/150 and 207/208**
   (CHECKLIST: Built, not Verified). Attempt 3c pass 2 was accepted
   as the after-run for items 225/226, and the later clean runs
   (key-budget 0.842/0.691 ms, 1,451/1,432 RPS; global-budget above)
   supersede it as the latest measurement — but a fully quiet window
   (loadavg < ~3 at start AND end, no other slice testing) with a
   coordinated fleet-wide bench pause has still never happened, and
   the fully-clean E1 pair (all four canaries ≥ 16k) is likewise
   still unmeasured.
4. **Peak RSS under load rose.** 98.8 MiB at attempt 3c vs 37.5 MiB
   at the 01:16 re-run. Profiling (CHECKLIST 235–236) found no leak
   — bounded buffering, tiktoken tables 11.8 MiB — but the ~26 MiB
   gap to attempt 3's peak is unexplained, not hand-waved (see the
   profiling section).
5. **No dedicated-box measurement.** Every number here is on the
   shared 1.5-vCPU VM, not Geekflare's dedicated 2-vCPU droplet; the
   absolute ceiling on a quiet dedicated box remains unmeasured.

**Provenance note.** Run JSONs written before the key-budget run
record a single `loadavg` array and no `loadavg_start`
(`loadavg_start` was added to the rig in the E1 slice, commit
`aa86a7f`); the current rig writes that array at merge time (end of
run — `scripts/bench-geekflare.sh`). Start-loadavg figures quoted
for those earlier runs (7.05 for the 00:02 run, 2.16 for the 01:16
re-run, 2.19 for the primary run) are therefore NOT in their JSON
files: they rest on the run narrative, not on repo evidence, and are
UNVERIFIED from repo evidence. Two blocks in this file have no raw
JSON at all and are flagged UNVERIFIED in place: the ~23:20 IST
earlier run ("Two runs, not one") and the diagnostic-probes table.

## 2026-11-18 benchmark round 2, phase 2 — two hot-path fixes, regression fixed, 01:16 level not restored

Full narrative with profiles: `docs/benchmark-round2-report.md`
(phase-2 appendix). Short version:

1. **Baseline (phase 1):** checklist item 145 (local token counting)
   regressed the router ~100x — added latency 0.541 ms → **73.6 ms**,
   RPS 1,483 → **41–51**, peak RSS 37.5 MiB → **349–363 MiB**. pprof
   proved the cause: `tiktoken-go.NewCoreBPE` rebuilt on every
   upstream-served request, 86.9% of all CPU.
2. **Fix 1 — encoder cache** (`internal/tokencount/tokencount.go`):
   one built `*tiktoken.Tiktoken` per encoding, cached for the life
   of the process. Concurrency safety verified against the
   dependency source first (`regexp2` documents concurrent safety
   and pools per-match runners; `encodeOrdinaryNative` only reads
   read-only state) and by a 6,400-call concurrent run under
   `-race` with zero races and identical counts. Bench run A
   (loadavg 1.57 at start): added latency **1.466 / 1.476 ms**, RPS
   **261 / 261**, peak RSS 83/86 MiB. The 73 ms floor was gone, but
   the 01:16 level was not back — a profile showed the next cost:
   `catalog.All` 42.7% of CPU, called 4x per request from the
   per-request readiness check (`checkLocalReadiness` →
   `RelayTargetForModel` → `resolveAuthHeaders` →
   `ResolveAuthForBaseURL` → `All()` twice, plus the wire fallback
   twice more).
3. **Fix 2 — auth-resolution memo** (`internal/provider/registry.go`):
   `resolveAuthHeaders` results memoized per provider ID, keyed by a
   SHA-256 fingerprint of every resolution input (key rotation
   changes the ciphertext, so it can never serve a stale header;
   errors are not cached). Bench run B (loadavg 3.60 at start,
   **19.35 at end — storm, see below**): added latency **1.013 /
   1.123 ms**, RPS **307 / 344**, peak RSS 65 MiB. A post-fix-2
   profile has **no `catalog.*`, `ResolveAuth*`, `tiktoken.*` or
   `tokencount.*` entry in the top 40 CPU nodes at all**; heap in
   use at shutdown 19.3 MB (was 124 MB at baseline). The remaining
   cost is diffuse (syscalls, semantic-cache Get, ledger SQLite) —
   there is no further single hot spot to cut.
4. **Why run B is not a clean number:** a VM storm started mid-run
   (another slice's full `go test ./...` gates). The direct-mock
   canary proves it: 5,422 / 4,628 RPS vs 10,338 / 11,919 in run A
   and 15,199 / 14,346 at the 01:16 re-run. Run B underestimates the
   router, most in pass 2. Also, other slices landed concurrent
   tree changes during phase 2 (internal/router `PriceFunc` /
   request-budget / `routetrace` rework; internal/reasoning and
   internal/server edits — the tree was broken mid-flight three
   times and had to be waited out), so run B measures that tree,
   not the two fixes in isolation.
5. **What remains:** a clean re-run of
   `./scripts/bench-geekflare.sh` in a genuinely quiet window
   (loadavg < ~3 at start AND end, direct-mock RPS back above
   ~14k) on a green tree. If the 01:16 level is still not restored
   then, the next step is limiter / admission work (502 rate still
   ~48%, under-load p50 29–49 ms vs 5.6 ms at 01:16), not another
   hot-path cut.

## 2026-11-18 benchmark slice (CHECKLIST 149–150) — fixes built, re-run BLOCKED by VM contention

Hot-path fixes from the 2026-11-18 pprof profile (profile
`$HOME/.tmp/pprof-2026-11-18/cpu.prof`, 19.41 s samples / 21.09 s
wall; semantic-cache Get 11.18% + set 6.96%, `sparseDot` 9.07%,
log redaction ~12.7% via `log.Printf` + `RedactLogLine` 7.83%,
ledger writer 6.29%, `mallocgc` 7.06%):

1. **Guardrail digit-shape gate** (`internal/guardrails/guardrails.go`,
   test `digitshapes_test.go`): the phone/card/aadhaar regexes ran on
   every access-log line because the line always contains a digit
   (`/v1/` + timestamp + status + latency) and the old prefilter only
   proved a digit exists. A single O(n) shape pass now gates them
   (sound: every real match exhibits the shape). `RedactLogLine`
   20,555 ns → ~4,790–5,287 ns/op (~4x, permanent
   `BenchmarkRedactLogLine`).
2. **Semantic-cache inverted index** (`internal/cache/cache.go`,
   tests `index_test.go`): postings per dimension replace the per-entry
   dot loop in `Get` (~3,000 posting updates vs 12,500 dot iterations
   at 500 entries); `set` dedups by exact-vector hash instead of an
   O(N) near-duplicate scan. `TestSemanticGetMatchesBruteForce`
   (300 churned inserts, evictions + rebuilds) proves Get returns
   exactly what the old full scan returned. Documented behaviour
   change: a merely near-identical vector (cos ≥ 0.9999, not
   identical) now takes its own slot, bounded by the LRU.
3. **Ledger budget-update aggregation** (`internal/ledger/async.go`,
   test `TestAsyncWriterAggregatesBudgetUpdates`): one UPDATE per
   gateway key per batch instead of one per row (a 75-row bench batch
   with one key: 75 → 1). 50 ms flush / durability window unchanged.

Before-run (this slice, `docs/benchmark-2026-11-18-before.json`,
2026-11-18 ~20:35–20:37 UTC, loadavg 1.76 at start): added latency
**0.578 / 0.769 ms**, RPS ok **1,178 / 1,470** (upstream 745/988 per
s), **48.5% / 48.3% HTTP 502**, cache hits 37% / 33%, idle 37.7 MiB,
ready 242.7 ms. Full suite after the fixes: `go test -count=1 ./...`
EXIT 0, 30 packages, 0 failures (log
`$HOME/.tmp/fulltest-2026-11-18.log`); `go build ./...` and
`go vet` clean on the touched packages.

**The after-run could not be measured honestly, and no after-numbers
are claimed.** The limiter experiment E1 (floor/initial/ceiling all
100, to test whether the Gradient2 floor binds RPS) was invalidated
twice by VM storms: attempt 1 (2026-11-18 ~20:49 UTC) ran at loadavg
12.35–15.39 with latency p50 131.7 ms and RPS 9.6/s; attempt 2
(21:11–21:19 UTC) at loadavg 3.70→5.87 with latency p50 331 ms and
RPS 7.2/s — it overlapped another slice's live `bench-geekflare.sh`
run plus several full test suites. A usability probe (21:26 UTC,
100 sequential requests, fresh router + mocks) returned p50 88 ms,
min 1.2 ms at loadavg 12.8: the router is fast when it gets CPU and
starved the rest of the time — a 1.5-shared-vCPU VM at loadavg ~12
cannot produce a meaningful latency/RPS number. Running the full
bench under those conditions would only publish starvation as if it
were the router, so it was not run. **Front-door admission control
was therefore NOT built**: with E1 inconclusive there is no evidence
that raising the floor or queueing at the front door raises
throughput — the pprof evidence says the router is CPU-bound on
cache/redaction/ledger work, and the 502s are the limiter shedding
load the CPU cannot serve (admission would turn 502s into 429s or
queued latency, not into more ok/s).

**What remains:** re-run E1 (limiter-off vs limiter-on back-to-back)
and then `scripts/bench-geekflare.sh` (with `CODVIAN_BENCH_OUT` set
so the dashboard serves it) in a quiet window — loadavg < ~3 and no
other slice benching or testing — then update this file, the README
benchmark sections, and CHECKLIST 149–150 (Built now, Verified only
after that run). The dashboard Benchmark section needs no code
change: it renders whatever `benchmark.json` the run writes,
misses included; no `benchmark.json` was written by this slice
because there is no honest run to record.

## 2026-11-18 quiet re-run attempt (CHECKLIST 207–208) — still no honest after-run; the VM never quieted

The re-run slice (items 207–208) ran `scripts/bench-geekflare.sh`
with `CODVIAN_BENCH_OUT` set on 2026-11-18 ~00:02 UTC (05:32 IST),
starting at loadavg 1.30 with no other slice benching or testing —
then a concurrent slice started `go test -run
TestHistoryPrunesOnWrite ./internal/health/` at 00:01, mid-run (the
>120 s fsync-heavy test), and loadavg ended at 7.32. That run's
numbers are therefore POLLUTED, not the after-run: added latency
**1.059 / 1.112 ms**, RPS ok **988 / 899** (upstream 984/899 per s),
**33.4% / 33.7% HTTP 502**, idle 38.4 MiB, ready 153.4 ms — worse
than the before-run on every axis, which is the signature of CPU
starvation, not a router regression (router p50 under load 14.3 ms
vs 5.5 ms before; no hot-path code changed between the before-run
and this attempt). Raw JSON is kept for the record as
`docs/benchmark-2026-11-18-after-polluted.json` — it is NOT the
published after-run and was NOT left in the dashboard feed: the
copy the harness wrote to the data dir was removed after a live
feed check, so `GET /dashboard/benchmark` again honestly answers
`recorded=false`.

The slice then waited ~25 minutes (eleven load checks, 00:02–00:27
UTC) for a quiet window. The VM never gave one: loadavg swung
1.2 → 23.2 → 1.2 while 3–8 concurrent `go test` / `go build`
processes (full suites, `-race` runs, provider tests) started every
1–2 minutes — the overnight fleet never pauses. No second bench was
started: starting one into a storm would only produce a second
polluted sample.

**What remains (unchanged):** the honest quiet-VM after-run for
items 149–150, plus the E1 limiter verdict, still wait for a window
with loadavg < ~3 and no other slice testing — that window did not
occur in this session. The dashboard plumbing was verified live in
this slice: with the polluted file in place, `GET
/dashboard/benchmark` served both passes with
`added_latency_met: false` / `rps_met: false` (misses shown as
misses, dashboard 200, the Benchmark section present in the page
HTML); after removing the file it returns the honest empty state.
Benchmark unit tests `TestDashboardBenchmarkRealRun` /
`TestDashboardBenchmarkNoRun` / `TestDashboardBenchmarkCorruptFile`
pass 3/3. No hot-path fix was attempted: there is no profile
evidence from a clean run justifying one, and the polluted numbers
prove nothing about the router.

## 2026-11-18 quiet re-run attempt 3 (CHECKLIST 225–226) — pass 2 clean, pass 1 polluted; targets still missed, and the tree moved

Third attempt at the honest after-run for items 149–150. The fleet never
paused — ~45 minutes of watching (01:02–01:47 UTC) with a 40-minute
60-second load log (`$HOME/.tmp/bench-quiet-watch-225.log`): loadavg swung
1.2 → 23.2 with 3–11 concurrent `go test` / `go build` processes for most
of it, full gates (227–228, then 229–230) starting every few minutes. Three
runs were made in this slice:

1. **Attempt 3a (01:37:45 UTC)** — launched at loadavg 1.32 with zero go
   processes, but the 227–228 slice's full `go test -count=1 -v ./...` +
   `go vet ./...` started ~6 seconds later and overlapped the whole run
   (loadavg 1.32 → 8.17). POLLUTED: added latency **0.968 / 0.839 ms**,
   RPS ok **1,001 / 1,018**, direct mock 8,923 / 14,552 RPS, ready
   814.5 / 1,207.9 ms (4–5× the normal 150–250 ms — the ready time alone
   proves contention). Raw JSON kept as
   `docs/benchmark-2026-11-18-attempt3-polluted.json`; NOT published.
2. **Attempt 3b (01:40:45 UTC)** — auto-launched by a 2-second poller the
   moment loadavg fell below 3 with zero go processes (load 2.84).
   POLLUTED again: added latency **0.694 / 0.888 ms**, RPS ok **869 /
   1,182**, direct mock **9,007 / 10,670 RPS** — the direct-mock canary
   (the rig's own contamination detector) degraded in BOTH passes, so the
   VM was contended throughout by something intermittent no `go test`
   check caught (a second slice was active — `router.go.fixed` was
   written mid-run at 01:42; the 229–230 gate itself started 01:43:21,
   after the run, so it was not the cause). Ready was normal (189.9 /
   301.1 ms). Raw JSON kept as
   `docs/benchmark-2026-11-18-attempt3b-polluted.json`; NOT published.
3. **Attempt 3c (01:45:00 UTC)** — the same poller launched within 2 s of
   the 229–230 gate ending (loadavg 1.31, zero go processes). **Pass 1
   POLLUTED** (direct mock 8,149 RPS — a second 229–230 verbose run
   overlapped it; added latency 0.915 ms, RPS ok 682, 34.9% 502, 183
   cache hits). **Pass 2 CLEAN — this is the after-run measurement:**
   direct mock **16,287 RPS** (the best direct number of the session —
   above the 01:16 re-run's 15,199 / 14,346, so the VM had the CPU during
   pass 2), direct latency p50 0.098 ms, no `go test` overlapping, ready
   125.6 / 249.3 ms, idle RSS 38.4 MiB. Pass 2: added latency **0.855
   ms**, RPS ok **1,079** (upstream 1,078/s — only 10 cache hits, so the
   headline is the upstream number this time), **22.1% HTTP 502**, p50
   under load 10.2 ms, peak RSS **98.8 MiB**. Raw JSON:
   `docs/benchmark-2026-11-18-attempt3.json` — this IS the published
   after-run record, and the dashboard Benchmark section serves it live
   (verified: `GET /dashboard/benchmark` on a real server returns both
   passes with `added_latency_met: false` / `rps_met: false`, ready/idle
   met; dashboard 200, Benchmark section present in the page HTML).

| Metric | Geekflare target | Attempt 3c pass 2 (clean) | Attempt 3c pass 1 (polluted) | Verdict (pass 2) |
|---|---|---|---|---|
| Added latency (median) | < 0.5 ms | **0.855 ms** | 0.915 ms | MISSED (~1.7x) |
| RPS (32 clients x 20 s) | > 3,000 | **1,079** | 682 | MISSED (~2.8x) |
| RPS errors | — | 6,141 / 27,796 (22.1%) HTTP 502 | 7,319 / 20,965 (34.9%) | — |
| Idle memory (RSS) | < 70 MB | **38.4 MiB** (40.2 MB) | 38.4 MiB | MET |
| Peak RSS under load | — | 98.8 MiB | 71.4 MiB | — |
| Ready time | < 10 s | **0.126 s** `/healthz`, 0.249 s `/v1/models` | — | MET |

What this means, honestly:

- **Both targets are still missed on the current tree — and by MORE than
  the before-run** (before-run 0.578/0.769 ms, 1,178/1,470 RPS). That is
  NOT a clean verdict on the 149–150 fixes: the tree moved enormously
  between the before-run and this run (binary 19 MB → 27 MB; ~30 slices
  landed — decision header on every response, routing groups, strategy
  trees, Responses API, plugin system, per-key traffic limits, catalog
  audit, and more). The before/after comparison for the three hot-path
  fixes is therefore confounded; what pass 2 measures is the current
  tree, not the fixes in isolation. Do not quote 0.855 ms / 1,079 RPS as
  "the fixes made it slower" — the fixes' effect is unmeasurable from
  these runs.
- **No hot-path fix was attempted in this slice.** There is no profile
  from a clean run justifying one; the round-2 finding stands (after the
  encoder cache + auth memo, no single hot spot remains — the cost is
  diffuse). A profiling slice on the current tree is the next step, not
  blind tuning.
- **Peak RSS 98.8 MiB under load (vs 37.5 MiB at the 01:16 re-run) is a
  real regression signal, not VM noise** — it appeared in all three
  attempt-3 runs (71–99 MiB) with a clean direct canary in pass 2. The
  mechanism is unproven, but the shape fits: p50 under load rose 5.6 →
  10.2 ms, so the closed-loop bench holds more requests in flight for
  longer, each holding buffers (and the bigger binary does more
  per-request work). Idle RSS is unchanged (38.4 MiB, still MET). A
  follow-up slice should profile memory under load; this slice does not
  claim the cause.
## Peak-RSS-under-load profiling — 2026-11-18 (CHECKLIST 235–236)

The follow-up slice profiled the attempt-3 signal above (peak RSS 98.8
MiB vs 38.4 MiB idle) with Go pprof against the real binary under the
same load, two waves. Full report: `docs/memory-profile-2026-11-18.md`.

**Verdict: no leak — legitimate, bounded buffering.** Settled heap after
each wave equals idle heap (wave 1: 19.1 MiB, wave 2: 21.1 MiB, idle
22.7 MiB); the retained heap is byte-identical across four profiles
(mid-load, settled, both waves, shutdown). Peak RSS in this run was
73.1 MiB (idle 38.7 MiB), peak HeapAlloc 36.3 MiB, peak in-flight 32
(= the 32-client concurrency), GC 1 → 113 collections at 2.6–3.3% GC
CPU. The retained heap is dominated by one-time costs — the tiktoken
cl100k BPE tables, 11.8 MiB (57–60% of retained heap), loaded once —
plus bounded buffers: ledger async-writer batching 1.56 MiB, per-key
traffic limiter 1.10 MiB, per-connection bufio 2×1.03 MiB, semantic
cache 0.5–2.5 MiB. The RSS delta is heap arenas held at high-water
(HeapSys 27.5 → 46.2 MiB) plus stacks/connection buffers. The GC is
exonerated as the p50 driver (2.6–3.3% CPU, ~1.3% of wall time paused);
the ~26 MiB gap to attempt 3's 98.8 MiB peak is unexplained, not
hand-waved — candidates (two full passes of HeapSys high-water, VM
noise) are listed in the report, none proven.

The slice also ships the tooling that produced the verdict:
`GET /dashboard/memstats` (live `runtime.MemStats`, RSS, goroutines,
in-flight, GC counters; `?profile=heap` / `?profile=goroutine` binary
pprof snapshots) and a "Live memory (this process)" card on the
Benchmark view fed by it. One profiling footgun is recorded in the
report and pinned by a test: `debug.SetGCPercent(-1)` DISABLES the GC
in Go 1.27 — it does not read the current value — and the first version
of the feed made exactly that mistake, growing the heap to 1.1 GiB
with NumGC frozen at 1 before the run was discarded.

- **The 502 rate fell (48% → 22%) because offered load halved**, not
  because shedding stopped: total completed fell 55,265 → 27,796
  (closed-loop client offers what the router completes; slower
  per-request work → fewer offered). The limiter is still shedding the
  excess.
- **The E1 limiter verdict (limiter-off vs limiter-on back-to-back) is
  still unmeasured** — the quiet window barely fit one run; it needs its
  own coordinated window.
- **The coordinated quiet window the 207–208 slice recommended (a
  fleet-wide bench pause) still never happened.** Three runs in one
  session, two fully polluted and one half-polluted, is the evidence:
  opportunistic waiting on this fleet does not produce clean runs. The
  next re-run should be scheduled by the Fleet Manager as a pause, or
  run on a dedicated box.

## 2026-11-18 re-run (no code change since the 00:02 run)

Same rig, same tree, same binary flags — the only difference is the
VM was quieter at the start (start loadavg 2.16; the 00:02 run's
start load is unknown — 7.05 is its end-of-run loadavg, not its
start, so this is not an exact start-to-start comparison). The run
completed cleanly end-to-end (exit 0); raw output:
`docs/benchmark-2026-11-18-rerun.json` (the 00:02 file is untouched).
Verified live afterwards: a real server with this run's JSON in its
data dir serves it at `GET /dashboard/benchmark` with
`added_latency_met: false` and `rps_met: false` — the dashboard
shows the misses as misses.

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.108 / 0.473 / 1.851 ms | 0.073 / 0.144 / 0.236 ms |
| Router latency p50 / p95 / p99 | 0.649 / 1.740 / 6.336 ms | 0.582 / 1.508 / 4.409 ms |
| Router latency mean / errors / cache hits | 0.900 ms / 0 / 10 of 1,000 | 0.798 ms / 0 / 97 of 1,000 |
| **Added latency (p50 diff)** | **0.541 ms** | **0.509 ms** |
| Direct RPS (32 x 20 s) | 15,199 (0 errors) | 14,346 (0 errors) |
| Router RPS (32 x 20 s) | **1,483** (29,717 ok, 27,470 x 502) | **1,426** (28,538 ok, 26,727 x 502) |
| Router RPS mean / p50 / p95 latency under load | 12.4 / 5.6 / 62.8 ms | 12.7 / 5.6 / 63.5 ms |
| Peak RSS during RPS | 37.5 MiB | 37.6 MiB |

Ready: `/healthz` 181.2 ms, `/v1/models` 326.3 ms. Idle RSS 37.5 MiB.
Rig: same shared VM, Go 1.27.4, load average 2.16 at run start
(10.58 recorded at run end — that is the bench's own 32-client load,
not background noise).

What moved vs the 00:02 run, and why — honestly:

- **Added latency pass 1: 0.677 → 0.541 ms.** No code changed between
  the two runs, so this is NOT a fix. The plausible cause is the
  quieter VM (start loadavg 2.16; the 00:02 run's 7.05 is its
  end-of-run loadavg, so the start-load comparison is not exact —
  see the provenance note at the top of this file): the direct-mock p50
  moved the same way (0.092 → 0.108 ms is noise, but the router p50
  0.769 → 0.649 ms is the whole move). Pass 2 is flat (0.511 →
  0.509 ms). The target is still missed, now by 0.041 ms on the
  clean pass.
- **RPS: flat.** 1,500 → 1,483 and 1,425 → 1,426 — inside run-to-run
  noise. The 502 rate is flat too (48.1% → 48.0%, 48.3% → 48.4%).
  Capacity on this VM is unchanged at ~1,500 ok/s.
- **Upstream RPS actually fell slightly:** 1,164 → 1,063 (pass 1)
  and 1,076 → 959 (pass 2). The headline "RPS (ok)" counts cache
  hits: pass 1 served 8,421 of 29,717 ok (28%) from cache, pass 2
  9,352 of 28,538 (33%) — up from 22%/24% in the 00:02 run, because
  the RPS mode reuses the prompt set and the cache now holds more
  of it. Real upstream completions were 21,296 (1,063/s) and
  19,186 (959/s). The headline RPS therefore overstates upstream
  throughput; both numbers are reported here.
- **p50 under load: 7.3/7.1 → 5.6/5.6 ms** — slightly better, same
  quieter-VM explanation; not a code win.
- **No limiter fix was attempted.** The 502s are the Gradient2
  limiter shedding offered load (~2,850 req/s offered vs ~1,500/s
  capacity) — the limiter working as designed. Raising its cap
  would queue requests, not create capacity: the binding
  constraint is per-request CPU (~0.54 ms sequential floor) on
  this 1.5-shared-vCPU VM. There is no cheap, evidence-backed
  limiter tuning that raises throughput, so none was made.
  Front-door admission control (reject overload before the
  expensive per-request work) remains the unbuilt candidate.

## 2026-11-18 run (after the hot-path fixes)

**Reconciled 2026-11-19 (CHECKLIST G2):** `docs/benchmark-2026-11-18.json`
was re-read field by field against this section and matches it exactly —
pass 1 added latency 0.677 ms, RPS 1,500.5 (30,014 ok / 27,811 × 502 =
48.1% of 57,825 completed), pass 2 0.511 ms, RPS 1,424.6 (28,551 ok /
26,630 × 502 = 48.3% of 55,181), ready 158.5 / 309.9 ms, idle 38,328 KiB,
date 2026-11-17T18:32:15Z, loadavg 7.05 at end of run (the harness
writes the END-of-run loadavg — `scripts/bench-geekflare.sh:201` —
and this file has no `loadavg_start`, so the start load is unknown;
an earlier wording of this note wrongly said "at start", fixed
2026-11-19 per the BENCHMARK-RECONCILIATION analyst finding). It is
therefore a validated record of this run, not an unvalidated interim
file, and is kept; no number in it is presented as a verdict anywhere
— the verdict against the targets is the one in the tables (both
targets missed). **Superseded as latest** by
`docs/benchmark-2026-11-18-globalbudget.json` (2026-11-18 07:52 UTC,
`globalbudget_remeasurement.clean: true`) — the latest verified
numbers are the table at the top of this file. The file itself is
VALID and KEPT (analyst verdict 2026-11-19): it remains the durable
record of the 00:02 run and must not be deleted.

pprof on the real request path (item 111) found the cost in three
places and each was fixed with evidence, not blind tuning:
`cache.cosine` (30.6% of samples) → sparse-index cosine + inlined
FNV-1a; guardrail regexes (16.4% backtracking) → sound prefilter
needles checked against one folded copy before any regex runs
(Unicode fold verified: Go `(?i)` really does match `ſecret` and
`api-Key`); synchronous per-request ledger/SQLite (17.7%) → async
batched writer (256 rows / 50 ms flush, ≤ 50 ms durability window
on crash, nothing lost on graceful stop). Full detail: the profile
logs in `/tmp/bench-*-2026-11-17.log` (ephemeral) and
`docs/benchmark-2026-11-18.json` (durable).

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.092 / 0.209 / 0.989 ms | 0.109 / 0.215 / 0.474 ms |
| Router latency p50 / p95 / p99 | 0.769 / 1.595 / 2.748 ms | 0.620 / 0.928 / 1.579 ms |
| Router latency mean / errors / cache hits | 0.872 ms / 0 / 10 of 1,000 | 0.659 ms / 0 / 88 of 1,000 |
| **Added latency (p50 diff)** | **0.677 ms** | **0.511 ms** |
| Direct RPS (32 x 20 s) | 15,658 (0 errors) | 16,232 (0 errors) |
| Router RPS (32 x 20 s) | **1,500** (30,014 ok, 27,811 x 502) | **1,425** (28,551 ok, 26,630 x 502) |
| Router RPS upstream (excl. cache hits) | **1,164 per s** (6,739 cache hits of 30,014 ok, 22%) | **1,076 per s** (6,993 of 28,551, 24%) |
| Router RPS mean / p50 / p95 latency under load | 12.2 / 7.3 / 50.0 ms | 12.8 / 7.1 / 55.2 ms |
| Peak RSS during RPS | 37 MiB | 38 MiB |

Ready: `/healthz` 158.5 ms, `/v1/models` 309.9 ms. Idle RSS 37 MiB.
Rig: same shared VM (end-of-run load average 7.05; start load
unknown — the JSON records no `loadavg_start`, see the reconciled
note above), Go 1.27.4.

Upstream split (from `docs/benchmark-2026-11-18.json`
`router_rps.upstream_rps` / `cache_hits`): the headline RPS
1,500 / 1,425 overstates upstream throughput by ~22–24% because
the RPS mode reuses the prompt set and the cache now holds more of
it (6,739 / 6,993 cache hits); real upstream completions are
1,164 / 1,076 per s. Quote the upstream number when the claim is
about the router serving upstream traffic.

Why the 502 rate ROSE to 48%: the bench client is closed-loop —
it offers as many requests as the router completes. Cheaper
per-request work means it offers ~2,900 req/s against ~1,500/s
capacity (total completed 57,825 vs 12,163 before), and the
Gradient2 limiter sheds the excess with 502s. The 502s are the
limiter working as designed; the number that improved is the
latency under load (p50 58 ms → 7 ms). The honest capacity read:
a ~0.65 ms sequential-latency floor implies at most ~2,300 RPS
on this 1.5-shared-vCPU box, so 3,000 RPS likely needs a
bigger/dedicated box or further work — front-door admission
control (reject overload before the expensive per-request work)
is the next candidate, unbuilt.

Pass 2's 88 cache hits are the same contamination as before
(pass 2 reuses pass 1's prompt set against the same router);
pass 1 (10 hits, 1%) is the clean number. A same-topic / same-frame
fuzzy hit was ruled out by test: bench prompts score 0.44–0.59
cosine, far below the 0.85 threshold
(`internal/cache/benchrepro_test.go`).

## Verdict against the Geekflare targets (2026-11-17 run, pre-fix)

Targets derived from Geekflare's independent benchmark (research-log
entry 148; see below): added latency < 0.5 ms, > 3,000 RPS on
2 vCPU, idle memory < 70 MB, ready < 10 s. **Two of four targets
are missed. The router is fast to start and light on memory, but
its per-request cost is ~3.4x the latency target and its throughput
on this machine is ~5-6x below the RPS target, with 1 in 5 requests
failing under the 32-client load.**

| Metric | Geekflare target | Measured (primary run) | Verdict |
|---|---|---|---|
| Added latency (median) | < 0.5 ms | **1.63 ms** (pass 1), 1.73 ms (pass 2) | MISSED (~3.4x) |
| RPS (32 clients x 20 s) | > 3,000 | **475** (pass 1), 558 (pass 2) | MISSED (~6x) |
| RPS errors | — (not a target, but reported) | 2,649 / 12,163 (21.8%) and 2,880 / 14,093 (20.4%) HTTP 502 | — |
| Idle memory (RSS) | < 70 MB | **37 MiB** (37,968 KiB = 38.8 MB) | MET |
| Peak RSS under load | — | 37 MiB (never exceeded idle; median 31–33 MiB during load) | — |
| Ready time | < 10 s | **0.19 s** to first `/healthz` 200, 0.30 s to first `/v1/models` 200 | MET (by ~50x) |

## The Geekflare reference (research-log entry 148)

Geekflare, tested 21 Oct 2026 on one DigitalOcean droplet (2 vCPU,
4 GB RAM, Docker, Ubuntu 24.04) against a stand-in instant NGINX
provider; median of 1,000 sequential requests; RPS = 32 clients x
20 s. Baseline with no gateway: 0.17 ms.
Source: https://geekflare.com/open-source/llm-gateway/

| Gateway | Version | Added latency | RPS (32 clients) | Idle memory | Ready in |
|---|---|---|---|---|---|
| agentgateway (Solo.io) | 1.6.0 | 0.46 ms | 3,336 | 8 MB | 4 s |
| Apache APISIX | 3.19.0 | 0.49 ms | 4,302 | 68 MB | 9 s |
| Bifrost | 2.3.2 | 1.2 ms | 970 | 180 MB | 19 s |
| Portkey | 1.15.2 | 2.8 ms | 615 | 36 MB | 8 s |
| LiteLLM | 1.106.2 | 18.1 ms | 55 | 642 MB | 61 s |

Codvian Router's measured numbers would sit between Bifrost and
Portkey on latency, below Portkey on RPS, and next to Portkey on
idle memory — but the machines differ (see caveats), so this is a
rough placement, not a ranking.

**Deliberate divergence — EQUAL ≠ COPY (2026-11-19, CHECKLIST G2):**
reference-style marketing presents gateway benchmarks as head-to-head
tables; we do not. Our numbers are measurements on this shared
1.5-vCPU VM only, and any cross-machine comparison is labelled a rough
placement, never a ranking — presenting it as a ranking would be a
misleading claim, and matching the reference router's capability/standard does not
mean copying its claims. Where our numbers miss the Geekflare targets,
the miss is shown as a miss (the verdict tables in this file and the
README benchmark section), never softened into a win.

## Methodology (the rig)

- **Stand-in instant provider:** `codvian-bench mock` — a Go
  `net/http` server answering `GET /v1/models` and
  `POST /v1/chat/completions` with a fixed tiny OpenAI-compatible
  reply and zero artificial delay (the NGINX role in Geekflare's
  test). Two mocks offer the same model `model-a`, so the router's
  adaptive-limiter shedding on one fails over to the other instead
  of deciding the outcome (the lesson of
  `scripts/bench-gc-profile.sh`).
- **Added latency:** median of 1,000 sequential `POST
  /v1/chat/completions` through the router minus the median of
  1,000 sequential POSTs direct to the mock. The subtraction
  cancels the mock's own cost. Every prompt is distinct (50 topics
  x 20 question frames = 1,000 unique combinations) so the
  router's exact/semantic cache and single-flight coalescing
  cannot collapse the load into cache hits; cache hits are counted
  from the `X-Codvian-Cache` response header and reported.
  25 warmup requests precede each run (not counted).
- **RPS:** 32 concurrent clients x 20 s through the router, and
  the same load direct to one mock as a mock-ceiling reference.
  The direct mock did 11,512–16,030 RPS, so the mock is not the
  bottleneck; the router numbers are the router's, not the mock's.
- **Idle memory:** VmRSS from `/proc/<pid>/status` after a 3 s
  settle. Peak RSS sampled every 100 ms during the RPS run.
- **Ready time:** wall time from process spawn to the first HTTP
  200 on `GET /healthz` (auth-exempt), plus spawn to first 200 on
  `GET /v1/models` with the gateway key as a second signal.
- **Repeats:** the whole sequence runs twice (pass 1 = reported
  run, pass 2 = stability check) because this is a shared, noisy
  VM. Reproduce with `./scripts/bench-geekflare.sh` (builds both
  binaries, starts the mocks, fresh `HOME` + `init` + two
  `provider add`s, runs everything, prints the table and writes a
  combined `bench-results.json` under its temp work dir).
- **Router under test:** `codvian-router` v0.1.0, built from this
  worktree with Go 1.27.4 (19 MB binary). Default settings
  throughout: TCP Fast Open on, adaptive concurrency (Gradient2)
  on, semantic cache on, single-flight coalescing on. The worktree
  has no `.git` checkout, so no commit hash is recorded; the
  prices-slice change that landed during this session is included
  in the primary run (see "Two runs" below).

## Machine

Linux 7.0.0-44-generic x86_64, AMD EPYC 9D25, 4 vCPUs visible,
8 GB RAM, Go 1.27.4. **This is a shared VM with a 1.5-shared-vCPU
budget, not a dedicated 2-vCPU droplet like Geekflare's, and other
slices were running in parallel: load average was 2.19 at the
start of the primary run and 8.45 by the end.** The RPS and
latency numbers are therefore a measurement *on this machine under
load*, not a clean-room result. The latency miss is still real
(the direct baseline on the same loaded VM is 0.09 ms), but the
RPS shortfall needs a re-run on a quiet dedicated box before it
is treated as final.

## Primary run (current tree, 2026-11-17 ~23:25 IST)

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.098 / 0.189 / 0.599 ms | 0.085 / 0.786 / 2.163 ms |
| Router latency p50 / p95 / p99 | 1.730 / 2.772 / 4.594 ms | 1.812 / 4.527 / 19.652 ms |
| Router latency mean / errors / cache hits | 1.857 ms / 0 / 10 of 1,000 | 2.369 ms / 0 / 92 of 1,000 |
| **Added latency (p50 diff)** | **1.632 ms** | **1.727 ms** |
| Direct RPS (32 x 20 s) | 14,093 (0 errors) | 11,512 (0 errors) |
| Router RPS (32 x 20 s) | **475** (9,514 ok, 2,649 x 502) | **558** (11,213 ok, 2,880 x 502) |
| Router RPS mean / p50 / p95 latency under load | 58.5 / 58.4 / 114.3 ms | 50.1 / 40.1 / 117.2 ms |
| Peak RSS during RPS | 37 MiB | 37 MiB |

Ready: `/healthz` 186.5 ms, `/v1/models` 298.5 ms. Idle RSS 37 MiB.

Pass 2's 92 cache hits are contamination, not a win: pass 2
reuses pass 1's prompt set against the same router process, and
the 500-entry cache still held some of pass 1's prompts. Even
with 9.2% of requests served from cache, pass 2's added latency
is 1.73 ms — the miss does not depend on the cache. Pass 1
(10 hits, 1%) is the clean number.

## Two runs, not one

An earlier run of the same rig (~23:20 IST, router binary built
just before the prices-slice change landed in the tree) measured
added latency 1.53 / 1.78 ms, router RPS 754 / 693 with 18.9% /
17.7% HTTP 502s, idle RSS 36 MiB, ready 114 ms / 221 ms. The
conclusions are the same; the RPS level moved between runs
(475–754 across four passes), which is the VM noise, not a code
regression — no cause is claimed for the difference.

## Diagnostic probes (fresh router, same rig, 10 s runs)

To characterise the 502s, RPS was probed at lower concurrency
against a fresh router (fresh cache; sequential latency p50 on
that instance: 1.78 ms, 10 cache hits, 0 errors):

| Concurrency | RPS (ok) | Errors (HTTP 502) | p50 latency under load |
|---|---|---|---|
| 5 | 665 | 19 / 6,674 (0.3%) | 5.4 ms |
| 10 | 652 | 319 / 6,873 (4.6%) | 9.0 ms |
| 20 | 613 | 858 / 6,994 (12.3%) | 19.9 ms |
| 32 | 384 | 1,136 / 4,998 (22.7%) | 75.0 ms |

Throughput is **flat at ~650 RPS from 5 to 20 clients and falls
at 32**, while latency under load grows with concurrency and the
502 rate climbs. That is the signature of a saturated hot path,
not a mock or network limit: at ~650 RPS the router's
per-request CPU (~1.5–2 ms of work per request on this VM) fills
the available CPU, queueing rises, the Gradient2 limiter sees
rising RTT, shrinks its cap and sheds (the 502s are the limiter
working as designed, per `scripts/bench-gc-profile.sh`'s
diagnosis — shedding is the symptom, saturation is the cause).
Even at 5 clients the router cannot go faster than ~665 RPS on
this machine.

## What this means / follow-ups (not done here)

1. **Latency:** ~1.6 ms of the ~1.7 ms router p50 is router work
   (the mock answers in ~0.1 ms). The prime suspect for the
   per-request cost is the synchronous per-request ledger/SQLite
   path plus scoring/metrics on the hot path — a suspect, not a
   finding. A profiling slice (pprof on the live path) is the
   next step; do not tune blind.
2. **RPS:** needs ~4–6x less per-request CPU to reach 3,000 on
   2 vCPU, or proof that this VM's shared-CPU cap is the whole
   story. Re-run this exact rig on a quiet dedicated 2-vCPU box
   before quoting any RPS figure.
3. **Rig fixes for the next run:** warmup prompts still share
   tokens with the measured "Explain … in one sentence" frame —
   10 of 25 warmup prompts scored at/above the 0.85 cache
   threshold (bounded impact: ~1% of requests, shifts the median
   by < 0.05 ms, but should be zero). Use a wholly different
   warmup topic set, and either restart the router or use a fresh
   prompt set per pass so pass 2 is as clean as pass 1.
4. **No target is claimed as met that was not measured,** and no
   number here is a claim about a real LLM provider — the
   upstream was an instant local mock by design, exactly as in
   Geekflare's test. Real-provider latency (hundreds of ms of
   model time) would dwarf the 1.6 ms gateway overhead; the
   overhead matters for the "fast on a weak PC" story, not for
   end-to-end chat latency.

## E1 limiter verdict (CHECKLIST 233–234) — the Gradient2 limiter is NOT the RPS ceiling

Measured 2026-11-18 02:29–02:53 UTC (07:59–08:23 IST), same harness
(`scripts/bench-geekflare.sh`), same tree, back-to-back: limiter-on
then limiter-off. The off mode is a real switch, not a config fudge:
`router.Config.DisableAdaptiveLimiter` leaves the engine's limiter
nil (the adaptive package's fail-open stub — every admission
granted), wired to `CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1` in
`serve` and to `CODVIAN_BENCH_LIMITER=off` in the rig;
`scripts/bench-e1-limiter.sh` runs both modes and merges them with
a verdict computed from the measured numbers. Raw JSON:
`docs/benchmark-2026-11-18-e1-on-polluted.json`,
`docs/benchmark-2026-11-18-e1-off-polluted.json` (both kept under
`-polluted` names — see why below), merged
`docs/benchmark-2026-11-18-e1-polluted.json` (the dashboard feed
serves this file as `benchmark.json`; its Benchmark section shows
both modes side by side with the verdict). The 28 MB router binary
is the same build in both modes — only the limiter differs.

| | Limiter ON, pass 1 | Limiter ON, pass 2 | Limiter OFF, pass 1 | Limiter OFF, pass 2 |
|---|---|---|---|---|
| Direct-mock RPS (canary) | **10,567 — POLLUTED** | **19,221 — clean** | 13,588 — mildly contended | **19,388 — clean** |
| Added latency (p50) | 1.162 ms | **0.680 ms** | 0.641 ms | **0.781 ms** |
| RPS (ok, 32 x 20 s) | 585 (upstream 552) | **1,324** (upstream 1,324) | 646 (upstream 646) | **1,419** (upstream 1,419) |
| HTTP 502 | 4,280 / 16,020 (26.7%) | **5,141 / 31,692 (16.2%)** | **0 / 12,943 (0%)** | **0 / 28,394 (0%)** |
| Router p50 under load | 13.4 ms | 9.3 ms | 40.9 ms | 11.0 ms |
| Peak RSS | 70.8 MiB | 98.6 MiB | 70.7 MiB | 81.8 MiB |

Ready/idle (both modes, MET): on ready 503.9 / 616.9 ms, idle
38.8 MiB; off ready 425.9 / 534.7 ms, idle 38.7 MiB. (Ready is
~2–4x the usual 125–250 ms in both modes — the VM had just
rebooted; see below. It is still ~16x inside the 10 s target.)

**Verdict: the limiter is NOT the RPS ceiling — and the verdict
rests on the pass-2 pair only.** Limiter-off sheds nothing (0
errors in 41,337 completed requests across both passes — the
switch works: nothing is shed when there is no limiter), yet RPS
rises only **+7.2%** on the clean pass-2 pair (1,324 → 1,419),
inside the ±10% run-to-run noise band, while added latency moves
only +0.101 ms (0.680 → 0.781 ms) and under-load p50 gets
*worse* (9.3 → 11.0 ms). If the Gradient2 cap were the ceiling,
removing it would have bought far more than 7% — instead the
throughput stayed ~1,300–1,400 ok/s and the overload that the
limiter used to shed as 502s (16.2%) now queues (p50 9.3 →
11.0 ms; pass 1 off, run under a starting full gate, shows the
shape at its worst: 0 errors but p50 40.9 ms). The binding
constraint is per-request CPU on this 1.5-shared-vCPU VM, exactly
as the round-2 profile said (no single hot spot left after the
encoder cache + auth memo; the cost is diffuse). The limiter was
only converting overload into 502s. **Consequence: do NOT tune
the limiter (raise the cap/floor) to chase 3,000 RPS — it cannot
create capacity that is not there. The next lever is
per-request CPU, a bigger/dedicated box, or front-door admission
(reject overload before the expensive work — the 502s would
become 429s or queued latency, not more ok/s).**

Why the pair is half-polluted, and why the verdict still stands:
the VM rebooted at ~02:27 UTC mid-wait, killing the first E1
wrapper; the on-run launched seconds after the reboot (loadavg
2.45, no `go test` running) straight into the fleet restarting —
pass 1's direct canary (10,567 RPS) and router numbers (585 RPS,
1.162 ms) are the reboot/fleet-restart contention, not the
limiter. Pass 2 of the same run, minutes later, is clean (direct
19,221 RPS — the best canary of the session). The off-run waited
for the next quiet moment (loadavg 0.08, no `go test`), but the
231–232 full `go test ./...` gate started seconds after it
launched (loadavg 0.08 → 2.25 across the run) — its pass 1 is
mildly contended (direct 13,588 RPS, between the polluted 8–11k
band and the clean 16–19k band; router p50 40.9 ms under load),
its pass 2 clean (direct 19,388 RPS). The merged file therefore
records `clean: false` and the verdict text says out loud that
it is a pass-2-only verdict; on pass 1 and off pass 1 are
excluded from it, and the `-polluted` filenames are kept so the
pair is never quoted as fully clean. The pass-1 numbers point
the same way (off 646 vs on 585 RPS, +10%, with 0% vs 26.7%
502s), so the verdict does not depend on discarding them — but
they are not what it rests on.

Two loose ends, stated plainly: (1) this is still the shared VM,
not a dedicated box — the 3,000 RPS target is judged on the same
rig as every run before it, so the comparison is apples-to-
apples with attempt 3, but the absolute ceiling on a quiet
dedicated 2-vCPU box remains unmeasured. (2) A fully clean pair
(two runs, all four canaries ≥ 16k) was not obtained — if the
Fleet Manager ever schedules a real bench pause, re-running
`scripts/bench-e1-limiter.sh` then would confirm the pass-2
verdict; until then the pass-2 pair is the evidence, and it is
decisive in the only direction that matters (removing the
limiter buys ~nothing).

## Hot-path CPU profiling — 2026-11-18 (CHECKLIST 249–250)

The E1 follow-up: per-request CPU is the RPS ceiling, so this slice
profiled the hot path under harness load (Go CPU pprof on the real
binary) and cut the biggest costs. Full report:
`docs/cpu-profile-2026-11-18.md`; merged before/after file:
`docs/benchmark-2026-11-18-cpu.json` (served by the Benchmark
view's "Hot-path CPU profile (249–250)" card).

**What the before profile showed** (worktree baseline, 03:27 UTC;
added latency 0.683/0.672 ms, RPS 1,313/930): token counting
`CountPromptWire` 4.30 s cum (11.6% of all CPU) — the same prompt
encoded 3× per request (traffic estimate, context-fit gate,
reconcileUsage) — and the pre-send budget guard
`CheckBudgetOrError` 4.09 s (11.1%), reading the same budget row
twice per request. `GetBudget` alone was 4.07 s; `_sqlite3Prepare`
3.49 s (every query re-parsed). `ExactCache.touch` 0.46 s (one
fresh-slice allocation per call). `Provider.Clone` 0.34 s (0.9%) —
measured, not a lever. `guardrails.scan` 0.97 s — named but not
touched (security slice 245–246 owns it).

**Fixes:** (1) token-count memo on `(encoding, fallback,
SHA-256(text))`, bounded 4,096 entries — collapses the 3 encodes
per request to 1 (memo hit 288 ns vs cold encode 50,702 ns);
(2) single budget-row read per guard (`checkBudgetDetailed`);
(3) prepared `GetBudget`/`GetKeyBudget` statements (no separately
measurable per-call win under contention — the ledger win is the
dedup, not the prepare; kept, not claimed); (4) allocation-free
`ExactCache.touch` (0 allocs per touch).

**Before/after pair** (clean HEAD tree ± fixes, 03:55–04:01 UTC;
clean by the canary rule — direct mock 16,218/15,560 before,
15,171/17,906 after): added latency 0.708 → 0.697 ms pass 1,
0.814 → 0.570 ms pass 2; RPS 692 → 1,650 pass 1, 764 → 650 pass 2.
**No RPS verdict is drawn:** the passes disagree (+138.5% /
−14.9%) while the load trace shows opposite regimes (before:
rising 1.7 → 9; after: falling 8.5 → 3.7) — the RPS delta is
load, not fix. What is solid is the profile-share comparison on
the same pair: `CountPromptWire` 11.55% → 2.95% of router CPU,
`CheckBudgetOrError` 11.75% → 8.59%, `ExactCache.touch` 1.32% →
0.92% (83 → 18 µs per request for token counting). Targets
still MISSED on the after run (0.697/0.570 ms vs < 0.5 ms;
1,650/650 RPS vs > 3,000). A definitive RPS re-measurement
needs the Fleet-Manager-coordinated bench pause that sections
above have now recommended three times without it happening.

## Definitive re-measurement — 2026-11-18 (CHECKLIST 273–274)

The E1 verdict (per-request CPU is the RPS ceiling) plus the
CPU-profile fixes (token-count memo, single budget read) are in —
this slice re-measures added latency / RPS on that tree, in the
quietest window obtainable. Merged file:
`docs/benchmark-2026-11-18-cpu2.json` (served by the Benchmark
view's "Definitive re-measurement (273–274)" card, with its clean
flag and verdict; the Hot-path CPU profile card is carried over
from `docs/benchmark-2026-11-18-cpu.json`).

**Verdict: targets still MISSED, in the same band as the
CPU-after run.** Clean by the canary rule (direct-mock canary
15,205 / 16,756 RPS, both ≥ 12,000):

| | Pass 1 | Pass 2 |
|---|---|---|
| Added latency (p50) | 0.908 ms | 0.711 ms |
| RPS (ok, 32 x 20 s) | 1,213 | 963 |
| Errors (502 shedding) | 4,511 / 28,797 (15.7%) | 4,791 / 24,086 (19.9%) |
| Router p50 under load | 12.1 ms | 14.6 ms |
| Peak RSS | 73.5 MiB | 94.2 MiB |

Ready 464.2 / 642.9 ms (MET, < 10 s), idle 41.5 MiB (MET,
< 70 MB). Targets: added latency < 0.5 ms, RPS > 3,000 — missed
on both passes, by ~1.8x on latency and ~2.5–3.1x on RPS.

**Comparison with the CPU-after run (03:58 UTC, 0.697/0.570 ms,
1,650/650 RPS):** the new pair sits in the same band — latency
0.71–0.91 ms vs 0.57–0.70 ms, RPS 963–1,213 vs 650–1,650. There
is no step-change from the CPU fixes on the harness: the fixes
removed the profile share they targeted (`CountPromptWire`
11.55% → 2.95% of router CPU), but the harness numbers did not
move out of the band, so the ceiling is the remaining diffuse
per-request CPU, not the two fixed hot spots. The E1 conclusion
stands — do NOT tune the limiter to chase 3,000 RPS.

**Honesty notes on this run.** The load trace rose during the
run (1.45 → 4.84 — a fleet gate started mid-run), and the two
passes disagree: RPS 1,213 vs 963 (−20.6%, outside the ±10%
run-to-run noise band), and pass 1's sequential-latency phase
took 2.77 s vs pass 2's 1.35 s (p95 17.6 ms vs 1.5 ms), so pass
1 is the noisier pass. The verdict rests on the canary rule, not
on a fully quiet window — no 6-minute window stayed quiet (see
the three polluted attempts below), and a fully quiet pair was
still not obtained. The numbers are the measurement; the RPS
spread is load, not signal.

**The three polluted attempts (kept separate, never the
verdict):**

| Attempt | Time (UTC) | Load start → end | Pass 1 | Pass 2 | Why polluted |
|---|---|---|---|---|---|
| 1 (`-cpu2-attempt1-polluted.json`) | 05:45:25 | 1.81 → 11.58 | 0.816 ms, 1,189 RPS, canary 19,377 | 0.782 ms, 247 RPS, canary 13,903 | Fleet `go build ./...` + catalog audit started mid-run; pass-2 router p50 47.9 ms |
| 2 (`-cpu2-attempt2-polluted.json`) | 05:56:16 | 2.42 → 3.90 | 0.889 ms, 329 RPS, canary 14,025 | 1.565 ms, 666 RPS, canary 6,180 | VM rebooted ~05:54 UTC; post-reboot fleet restart stormed the run; pass-2 canary fails the 12,000 rule |
| 3 (`-cpu2-attempt3-polluted.json`) | 06:05:21 | 1.32 → 8.53 | 1.976 ms, 598 RPS, canary 8,761 | 1.287 ms, 300 RPS, canary 14,870 | Fleet gate started seconds after the quiet launch; pass-1 canary fails the 12,000 rule |

Attempt 1's pass 1 was canary-clean (0.816 ms, 1,189 RPS) and
points the same way as the definitive run — it is quoted here
for the record only, not as the verdict. The fourth attempt
(06:13:19 UTC) is the one above: launched at loadavg 1.32 with
no `go test`/`go build` running, both canaries clean.

**Fresh profile — what is next:** the targets are still missed,
so a CPU profile of the new tree under harness load was run
after this measurement (`CODVIAN_CPU_PROFILE`, same switch as
249–250; 06:19–06:20 UTC, load 1.25 → 0.97, canaries
21,058/21,160 RPS — the cleanest window of the session). Full
report: `docs/cpu-profile-2026-11-18-cpu2.md`; raw run JSON:
`docs/benchmark-2026-11-18-cpu2-profile.json` (0.800/0.844 ms,
1,363/1,230 RPS — corroborates this run, not a second verdict).
The profile's top costs (42.12 s samples): syscalls 13.11%
flat (inherent I/O, not a lever); `GetKeyBudget` 11.54% cum —
the biggest named router cost and the next lever (cache the
key-budget row in memory, or combine the two budget reads);
the SQLite parse path under the ledger calls
(`_sqlite3RunParser` 10.00% cum, overlapping `GetKeyBudget`);
semantic-cache lookup `getScoped` 3.13% cum (already
investigated in 263–264 with no cheap win — do not re-try the
same skip); `guardrails.scan` 2.56% cum;
`CheckBudgetOrError` 6.17% cum (was 8.59%);
`CountPromptWire` 1.16% cum (was 2.95% — the memo is holding).
No single hot spot remains above ~12% cum: the remaining cost
is diffuse, so expect incremental gains, or a bigger/dedicated
box, or front-door admission.

---

## Ledger write path — multi-row INSERT (CHECKLIST 287–288, 2026-11-18)

**Finding.** The cpu2 profile put the SQLite parse path at 10.00%
cum (`_sqlite3RunParser`, overlapping the budget reads) and the
async writer's `commitBatch` at 6.22% cum, of which the per-row
`insert.ExecContext` was 2.09 s (80%). Reading the driver source
(modernc.org/sqlite v1.34.0, `sqlite.go`: `newStmt` only stores
the SQL string; `stmt.exec`/`stmt.query` call `prepareV2` —
`sqlite3_prepare_v2` — on EVERY call and finalize the statement
afterwards) showed there is no prepared-statement reuse in this
driver at all: `db.Prepare`/`tx.PrepareContext` cannot save a
parse. The only lever is fewer statements. The fix
(`internal/ledger/async.go`, `multiRowInsertChunk = 32`): one
multi-row INSERT per 32-row chunk instead of one INSERT per row
— 256 parses per 256-row batch become 8 (32 rows x 22 columns =
704 placeholders, under SQLite's 999-variable legacy limit).
The budget UPDATE stays one aggregated UPDATE per gateway key
(30 ms flat in the profile — not the cost; a CASE-folded single
UPDATE was considered and rejected as risk for no measurable
gain). The `getScoped` skip was NOT re-tried (263–264 already
proved no cheap win there). Semantics preserved: a row moves its
key's budget only when its INSERT chunk succeeded and its key's
UPDATE succeeded; a failed chunk counts exactly its rows as
failed (blast radius grows from 1 row to at most one 32-row
chunk; `usage_events` has no UNIQUE constraint a row can
violate, so a chunk failure in practice means a DB-level error
that would have failed every row anyway).

**Micro-benchmark** (`BenchmarkCommitBatch256`, 256-row batch,
`internal/ledger/async_bench_test.go`, same machine, 3 runs of
20 iterations each): before 100.3 / 66.9 / 58.3 ms/op, after
21.1 / 22.4 / 30.2 ms/op — about 3x faster per batch
(~67 ms → ~22 ms/op median). The win is real at batch level,
but it is a wall-time win dominated by fewer prepare/finalize
round-trips per batch, and the live path runs ~65-row batches
with the writer off the hot path — so it need not move the
harness, and it does not (see below).

**Profile before/after** (same pprof harness, `CODVIAN_CPU_PROFILE`,
latency + 20 s RPS load; before HEAD 7b80c6cb 06:44 UTC, after
HEAD 0f751af8 06:57 UTC — the tree moved between the profiles,
279–280's key-budget change landing in between, so the parse
share is confounded and no clean attribution is claimed):

| Metric | Before (18.70 s samples) | After (19.66 s samples) |
|---|---|---|
| `commitBatch` cum | 1.16 s / 6.20% | 1.19 s / 6.05% |
| Insert line cum (`insert.ExecContext` / `insertChunk`) | 0.96 s (83% of commitBatch) | 0.90 s (76% of commitBatch) |
| `_sqlite3RunParser` cum | 1.82 s / 9.73% | 1.55 s / 7.88% |
| `GetKeyBudget` cum (279–280 owns it — shown only so the parse share is not misread) | 1.40 s / 7.49% | 1.70 s / 8.65% |

The writer's share is unchanged (6.20% → 6.05%, within noise):
the per-row Exec was dominated by bind/step/finalize, not the
parse. The parse path fell 9.73% → 7.88% cum, but `GetKeyBudget`
moved the other way (7.49% → 8.65%) on a moved tree, so that
delta is not attributed to this fix.

**Harness before/after** (Geekflare-style,
`scripts/bench-geekflare.sh`; before
`docs/benchmark-2026-11-18-ledger-before.json`, after
`docs/benchmark-2026-11-18-ledger-after.json`, merged
`docs/benchmark-2026-11-18-ledger.json`). Both runs at the same
HEAD 0f751af8 — the only tree delta between them is this slice's
`internal/ledger/async.go` (other slices' uncommitted files were
present in both runs, recorded in the waiter logs).

| Pass | Before added latency / RPS (canary) | After added latency / RPS (canary) |
|---|---|---|
| 1 | 1.232 ms / 1,238 RPS (direct 12,719) | 0.680 ms / 1,393 RPS (direct 19,396) |
| 2 | 0.924 ms / 1,220 RPS (direct 19,215) | 0.624 ms / 1,086 RPS (direct 20,567) |

Load trace before: 2.28 at launch → 4.72 at end; after: 2.27
→ 3.57. Ready 771/1,008 ms before, 376/484 ms after (both
well under the 10 s target); idle RSS 41.6 MiB before, 41.3 MiB
after (target < 70 MB, met both runs). 502 rates: before
5,395/5,182 errors, after 3,927/4,590 — the limiter still sheds
under the closed-loop client.

**Verdict: no end-to-end win — within noise, and no win is
claimed.** RPS moved +12.5% / −11.0% in opposite directions
across the two passes, both around the ±10% run-to-run noise
band. Latency fell in both passes (−44.8% / −32.5%), but the
before run's pass 1 ran under rising load (its direct-latency
p50 was 0.100 ms vs 0.060 ms typical) and the after latencies
(0.680/0.624 ms) match the earlier half-polluted run's clean
latency passes (0.696/0.623 ms) — load noise, not a fix
effect. Geekflare targets still MISSED (0.680/0.624 ms vs
< 0.5 ms; 1,393/1,086 RPS vs > 3,000). The change is kept
because it is strictly better at the batch level (fewer
parses, same accounting, tests green) — not because it moved
the harness.

**The half-polluted first before-run (kept separate, never the
verdict):** `docs/benchmark-2026-11-18-ledger-before-polluted.json`
(06:52–06:53 UTC, HEAD 0f751af8): pass 1 clean (0.696 ms,
1,382 RPS, canary 19,511) but pass 2 starved mid-run (RPS
448, RPS-phase p50 32.5 ms vs 9.4 ms in pass 1) when another
slice's gate started. Its clean pass 1 is quoted above only as
the latency-noise cross-check, not as the baseline.

**Fresh profile — what is next:** the targets are still missed,
so the after-profile's top costs are the next levers (19.66 s
samples): syscalls 13.53% flat (inherent I/O, not a lever);
`GetKeyBudget` 8.65% cum — the biggest named router cost
(279–280 owns the budget-read path); `_sqlite3RunParser`
7.88% cum (overlapping the budget reads); `commitBatch` 6.05%
cum (this slice — no further cheap win here: the insert line
is now bind/step-bound, not parse-bound); semantic-cache
`getScoped` 2.80% cum (263–264 already closed — do not re-try);
`guardrails.scan` 3.36% cum. No single hot spot remains above
~9% cum: the remaining cost is diffuse, so expect incremental
gains, or a bigger/dedicated box, or front-door admission.

## Key-budget hot-path fix — per-request memo (CHECKLIST 279–280, 2026-11-18)

**Finding.** The 273–274 profile named `GetKeyBudget` the
biggest cost at 11.54% cum: a chat request hits the key-budget
guard twice — pre-body admission (`checkKeyBudgetForModel`,
model unknown) and the post-decode check (`checkKeyBudget`) —
and each hit was a full SQLite read of the `key_budgets` row
(prepared statement, but the modernc driver still parses per
call). `CheckKeyBudget` itself read the row twice when a budget
existed (once directly, once inside `KeyBudgetStatus`).

**Fix.** A per-request memo in the ledger
(`internal/ledger/keybudget.go`: `WithKeyBudgetMemo`,
`keyBudgetMemo`): the server attaches exactly one memo per HTTP
request in `withLogging` (the outermost wrapper), so the second
guard read per key is a map lookup. Only successful reads are
memoised (a found row, or `sql.ErrNoRows` for no row); real DB
errors are never cached. `SetKeyBudget`/`DeleteKeyBudget` through
the same request context invalidate that key's memo entry, so a
dashboard budget update (which reads the previous row, writes,
and can roll back on a failed config persist, all in one
request) never sees a stale row. `CheckKeyBudget` now passes the
row it already read into `keyBudgetStatusWithBudget` instead of
re-reading it. The memo lives only as long as the request — a
budget set or deleted mid-request is visible to the next
request, never to this one. No TTL, no cross-request cache, no
behaviour change without a memo (fresh DB read every call).

**Micro-benchmark** (`BenchmarkGetKeyBudgetMemoHit` vs
`BenchmarkGetKeyBudgetMiss`, `internal/ledger`): memo hit 87
ns/op vs uncached miss 21,665 ns/op (~250×). New tests
(`internal/ledger/keybudget_memo_test.go`) prove the memo by
mutating the database behind its back: a memoised read does not
see the change, a fresh context does, and a write through the
same context invalidates.

**Re-measurement (attempt 3, 07:09–07:11 UTC, HEAD
`ec029d5`, the clean run):** added latency 0.842 ms pass 1,
0.691 ms pass 2; RPS 1,451 pass 1, 1,432 pass 2 (errors
4,752/33,786 pass 1, 4,394/33,113 pass 2 — the limiter shedding
offered load); ready 441/559 ms, idle 41.8 MiB. Load 1.05 →
0.91 across the run, direct-mock canaries 21,732 / 21,805 RPS,
no `go test`/`go build` running. RPS passes agree within 1.4%;
latency passes differ by 18% (0.842 vs 0.691 ms — the same order
of spread as the definitive run's 0.908/0.711 ms, disclosed
here, not smoothed over). **Targets still MISSED on both
passes** (latency ~1.7× over on the worse pass, RPS ~2.1×
under). Raw run JSON:
`docs/benchmark-2026-11-18-keybudget-run.json`; merged file
(run + verdict + carried-over definitive/CPU cards):
`docs/benchmark-2026-11-18-keybudget.json`, served by the
Benchmark view's "Key-budget hot-path fix (279–280)" card.

**Comparison with the definitive run (273–274, 0.908/0.711 ms,
1,213/963 RPS):** the pair sits in the same band — latency a
touch better, RPS a touch better, both inside run-to-run noise
on this VM. The fix removed the profile share it targeted
(`GetKeyBudget` 11.54% → 7.52% cum on the clean run; 8.32% and
8.61% on the two polluted attempts in the same session), but
the harness numbers did not move out of the band, so the
ceiling is the remaining diffuse per-request CPU, not the
key-budget read. Per-request `GetKeyBudget` cost halved
(4.86 s / ~53,900 requests ≈ 90 µs before; 3.23 s / ~68,900
requests ≈ 47 µs after). `CountPromptWire` 1.21% cum (was
1.16% — the token-count memo is holding); `ExactCache.touch`
0.88% (was 1.00%).

**The two polluted attempts (kept separate, never the
verdict):**

| Attempt | Time (UTC) | Load start → end | Pass 1 | Pass 2 | Why polluted |
|---|---|---|---|---|---|
| 1 (`-keybudget-attempt1-polluted.json`) | 06:53 | 1.28 → 0.83 | 0.859 ms, 1,254 RPS, canary 11,963 | 0.923 ms, 480 RPS, canary 13,942 | Pass-1 canary marginally below the 12,000 rule; pass-2 RPS collapsed −62% vs pass 1 (p50 30.8 ms under load, peak RSS 103.7 MiB) |
| 2 (`-keybudget-attempt2-polluted.json`) | 07:08 | 1.06 → 3.69 | 0.816 ms, 523 RPS, canary 15,622 | 0.773 ms, 1,177 RPS, canary 17,463 | Load rose mid-run; RPS passes 523 vs 1,177, −55% apart |

Attempt 1's pass 1 (0.859 ms, 1,254 RPS) and attempt 2's pass 2
(0.773 ms, 1,177 RPS) point the same way as the verdict run —
they are quoted here for the record only, not as the verdict.
The RPS "good pass" across all three attempts (1,254 / 1,177 /
1,451 / 1,432) is consistently ~1,200–1,450; the "bad" pass
(480 / 523) is always the load-polluted one.

**Fresh profile — what is next:** the targets are still missed,
so the after-profile's top costs are the next levers (42.95 s
samples, full report `docs/cpu-profile-2026-11-18-keybudget.md`):
syscalls 13.62% flat (inherent I/O, not a lever);
`CheckBudgetOrError`/`GetBudget` 8.20% cum — the global-budget
read, now the biggest named router cost (memoise it per request
the same way, or combine it with the key-budget read);
`GetKeyBudget` 7.52% cum (one read per request, down from two);
the SQLite parse path (`_sqlite3RunParser` 8.29% cum,
overlapping every ledger query); `mallocgc` 7.05% cum;
semantic-cache `getScoped` 2.86% cum (263–264 already closed —
do not re-try); `guardrails.scan` 2.79% cum. No single hot spot
remains above ~9% cum: the remaining cost is diffuse, so expect
incremental gains, or a bigger/dedicated box, or front-door
admission.

## Global-budget hot-path fix — per-request memo (CHECKLIST 299–300, 2026-11-18)

**The fix:** the 279–280 fresh profile named
`CheckBudgetOrError`/`GetBudget` the biggest named router cost
at 8.20% cum. The fix is a per-request global-budget memo
(`internal/ledger/budget_memo.go`: `WithBudgetMemo`, attached in
`withLogging` next to the key-budget memo), the same shape as the
279–280 key-budget memo — only successful reads memoised (a found
row, or its absence as `sql.ErrNoRows`), real database errors
never cached, same staleness bound of one request, invalidation
on writes through the same context (`SetBudget`, `ResetSpent`,
and `RecordUsage` — the memoised row carries `SpentUSD`, unlike
the key-budget row, so a spend recorded mid-request must
invalidate too). Micro-benchmark: memo hit 228 ns/op (row) /
248 ns/op (miss) vs uncached miss 15,012 ns/op (~65×).

**Honest scope, found in the code before the slice landed:** on
today's hot paths the global-budget guard runs exactly once per
request — `enforcePromptBudget`, called once per chat /
responses / embeddings request, makes exactly one `GetBudget`
read (one of `CheckBudgetOrError` or `CheckBudgetForEstimate`,
never both; the pre-body admission checks only the key budget).
The key-budget memo saved a second read because that guard ran
twice; this memo has no second read to save on the chat path.
The memo is the dedup mechanism for any request that hits the
guard twice (a future pre-body global-budget admission check, a
retry inside one request), proven by unit tests that mutate the
database behind the memo's back — but the profile was expected
to show no win, and it shows none.

**The verdict run (07:50:39–07:52:12 UTC, HEAD `db5c626`, load
1.30 → 1.48, direct-mock canaries 19,599 / 20,009 RPS, no `go
test`/`go build` running — clean):** added latency 0.851 ms
pass 1, 0.777 ms pass 2; RPS 1,273 pass 1 (ok 25,494, errors
4,432/29,926, 14.8%), 1,174 pass 2 (ok 23,498, errors
5,019/28,517, 17.6%); ready 434.8/565.0 ms, idle 42.1 MiB, peak
RSS 78.2/108.8 MiB. Geekflare targets (< 0.5 ms, > 3,000 RPS)
still MISSED on both passes. Merged run:
`docs/benchmark-2026-11-18-globalbudget.json` (raw run
`docs/benchmark-2026-11-18-globalbudget-run.json`); the served
file carries the definitive, CPU and key-budget cards alongside
the new run, and the Benchmark view gains a "Global-budget
hot-path fix (299–300)" card.

**Fresh profile — within noise, by design (87.14 s, 39.31 s
samples, full report
`docs/cpu-profile-2026-11-18-globalbudget.md`):** `GetBudget`
8.20% → **7.96% cum**, `CheckBudgetOrError` 8.20% → **8.01%
cum**; per-request cost flat at ~51 µs (3.15 s / 61,443
requests, vs 3.52 s / ~68,900 before). `go tool pprof -peek
GetBudget` confirms the memo is in the served binary (the call
runs through `getBudgetMemoised` into `getBudgetUncached`, one
caller chain from `checkBudgetDetailed`). The harness pair sits
in the same band as the key-budget run (0.842/0.691 ms,
1,451/1,432 RPS) and the definitive run (0.908/0.711 ms,
1,213/963 RPS): latency and RPS both inside run-to-run noise,
targets still missed by ~1.7× on latency and ~2.4× on RPS. The
RPS passes are 12–18% below the key-budget run's; that is
neither claimed as a win for the memo nor as a regression from
it — the clean-run band spans 963–1,451 RPS and this run sits
inside it.

**Fresh profile — what is next:** the targets are still missed,
so the after-profile's top costs are the next levers (39.31 s
samples): syscalls 12.77% flat (inherent I/O, not a lever);
`GetKeyBudget` 8.27% cum — now the biggest named router cost
again (one read per request, memo holding; its share rose only
because total samples fell) — and `CheckBudgetOrError`/`GetBudget`
8.01%/7.96% cum right behind it; the SQLite parse path
(`_sqlite3RunParser` 7.78% cum, `_sqlite3Parser` 7.50%,
`_yy_reduce` 6.82%, overlapping every ledger query);
`mallocgc` 6.87% cum; semantic-cache `getScoped` 3.31% cum
(263–264 already closed — do not re-try); `guardrails.scan`
3.00% cum. The two budget reads together (8.27% + 7.96%) are
already one read per request each: the next lever is combining
them into one round trip (or a cross-request cache with an
explicit invalidation contract) — both change the staleness
bound, so they need their own slice. No single hot spot remains
above ~9% cum: the remaining cost is diffuse, so expect
incremental gains, or a bigger/dedicated box, or front-door
admission.
