# Memory profile under load — 2026-11-18 (CHECKLIST 235–236)

## Verdict

**No leak. Legitimate, bounded buffering.** The attempt-3 signal — peak RSS
98.8 MiB under load vs 38.4 MiB idle, p50-under-load 5.6 → 10.2 ms — is
explained by fixed one-time costs plus bounded per-request buffering, all
of it collected promptly: settled heap after each load wave is the same as
idle heap, and the retained heap is byte-identical across four independent
profiles. No production fix was needed, so none was landed in shared
packages; the churn hot spots below are documented as optimisation
follow-ups, not leak fixes.

## Method

Real binary under the geekflare-style RPS load, two waves (32 clients ×
20 s each, two `codvian-bench` mock upstreams, distinct prompts so the
cache cannot collapse the load), with a 10 s settle after each wave:

- `GET /dashboard/memstats` (new in this slice) polled every 100 ms:
  `runtime.MemStats`, VmRSS from `/proc/self/status`, goroutines,
  in-flight requests from the metrics recorder, GC counters.
- Binary pprof heap + goroutine profiles captured mid-load (in flight)
  and after settle, per wave, via `?profile=heap` / `?profile=goroutine`
  on the same endpoint; plus a shutdown heap profile via
  `CODVIAN_MEM_PROFILE` on graceful SIGTERM.
- Leak test: settled heap after wave 2 vs settled heap after wave 1 —
  growth wave-over-wave would be a leak; equality is boundedness.

Rig: shared VM, 4 CPUs, Go 1.27.4, `gc: profile=default, memory
limit=2048 MiB, GOGC=default`. Binary: HEAD `8448256` + this slice's
memstats feed only. The shared worktree would not build at profiling
time (the E1 limiter slice, items 233–234, had an uncommitted
`engCfg`-out-of-scope break in `cmd/codvian-router/main.go:2514`), so the
profile ran from a clean `git archive HEAD` tree with only the memstats
files added; the limiter is ON by default in both trees, so the load
shape is the same. Times below are UTC 2026-11-18 ~02:08 (≈07:38 IST).

Two runs were needed. The first run is DISCARDED: the first version of
the memstats feed read GOGC with `debug.SetGCPercent(-1)`, which in Go
1.27 does not read the value — it sets gcPercent to −1 and **disables
garbage collection** ("A negative percentage effectively disables garbage
collection", `runtime/debug` docs). Polled every 100 ms, the feed turned
the GC off for the whole run: NumGC froze at 1, heap grew 320 → 1,164
MiB across the two waves, RSS 1,251 MiB. That run measured the bug, not
the router. The feed was fixed (GOGC is now an environment read;
`SetMemoryLimit(-1)` is a documented safe read and stays), a regression
test pins the shape, and the run below is the re-run with the GC on.
The discarded run is recorded here so nobody re-discovers it as a
"1.2 GiB leak".

## Numbers (valid run)

| | Idle | Wave 1 mid-load peak | Wave 1 settled | Wave 2 mid-load peak | Wave 2 settled |
|---|---|---|---|---|---|
| HeapAlloc | 22.7 MiB | 36.3 MiB | 19.1 MiB | 35.8 MiB | 21.1 MiB |
| HeapSys | 27.5 MiB | 46.2 MiB | 46.1 MiB | 46.2 MiB | 46.2 MiB |
| RSS | 38.7 MiB | 73.1 MiB | 70.3 MiB | — | 71.8 MiB |
| Goroutines | 13 | 277 | 83 | — | 57 |
| In-flight (peak) | 0 | 32 | 0 | 25 | 0 |
| NumGC (cumulative) | 1 | — | 89 | — | 113 |

Load: wave 1 — 23,982 completed, 18,553 ok, 5,429 × HTTP 502 (22.6%,
adaptive-limiter shedding), RPS 924.6 ok, p50 12.8 ms. Wave 2 — 7,925
completed, 4,515 ok, 3,410 × 502 (43.0%), RPS 223.3 ok, p50 65.4 ms.
Wave 2 was much slower because other slices were benchmarking on the
same shared VM at the time (an E1 limiter run and a 50× race test);
its latency numbers are polluted and are not quoted as router
measurements. The memory numbers are robust to that noise: both waves
peak at the same heap and settle to the same heap.

GC over the run: 1 → 113 collections, GC CPU fraction 2.6–3.3%, total
pause time 886 ms over ~66 s (~1.3% of wall time), mean pause ~7.8 ms,
largest single pause 20.1 ms (the last one — the forced GC from the
heap-profile fetch itself).

## Where the memory goes

Retained heap, mid-load and settled, byte-identical across all four
profiles (mid-w1, settled-w1, mid-w2, shutdown):

| Holder | Size | Nature |
|---|---|---|
| `tokencount.embeddedLoader.LoadTiktokenBpe` + `tiktoken-go.NewCoreBPE` | 11.8 MiB (6.29 + 5.00) — 57–60% of retained heap | One-time: the cl100k BPE tables, loaded lazily on first token count, never per-request |
| `ledger.NewAsyncWriter` | 1.56 MiB | Async ledger writer batch buffers |
| `server.(*keyTrafficLimiter).tryAdmit` | 1.10 MiB | Per-key traffic limiter state |
| `bufio.NewReaderSize` / `bufio.NewWriterSize` | 1.03 MiB each | Per-connection HTTP buffers (pooled by net/http) |
| `cache.(*LocalSemanticCache).set` / `storeInSlotLocked` | 0.5–2.5 MiB | Semantic-cache entries (bounded, 500 entries) |
| `runtime.mallocgc` (transient) | 1.54 MiB | In-flight allocation, not retained |

Decomposition of the RSS delta (idle 38.7 → peak 73.1 MiB, +34.4 MiB):
heap arenas +18.7 MiB (HeapSys 27.5 → 46.2; HeapIdle ~18 MiB at
settle, ~7 MiB of it already returned to the OS — the runtime keeps
idle spans as high-water, which is normal Go behaviour), plus goroutine
stacks / connection buffers / thread stacks for the ~277 peak
goroutines. Settled RSS stays ~71 MiB because HeapSys does not shrink
below its high-water mark; the live heap underneath it is 19–21 MiB.

Goroutines mid-load: 153 total, 150 parked — 38 in `bufio.Reader`
fill/peek (HTTP keep-alive read waits, one per held connection), 32 in
`database/sql.(*DB).QueryContext` (one per in-flight request, ledger
queries), the rest runtime/watchers. Settled counts fall wave over wave
(83 → 57), consistent with idle-connection timeouts, not a leak; no
settled goroutine profile was captured, so the exact stragglers are not
named — see follow-ups.

## Allocation churn (not retained — this is what the GC collects)

693.5 MB allocated by the mid-wave-1 capture, ~24k requests in
(≈29 KB per request, almost all freed; in-use heap at that moment was
only ~20 MiB):

| Source | Churn | Per-request |
|---|---|---|
| `regexp2.newMatch` (tiktoken regex split) | 83.0 MB | ~3.5 KB |
| `cache.(*ExactCache).touch` | 71.1 MB | ~3.0 KB |
| `provider.(*Provider).Clone` | 61.5 MB | ~2.6 KB |
| `tiktoken-go.(*CoreBPE).encodeOrdinaryNative` (cum) | 140.0 MB | ~5.9 KB |
| `cache.embedText` | 21.5 MB | ~0.9 KB |
| `argon2.initBlocks` | 19.0 MB | occasional, not per-request at scale |
| `bytes.Clone` / `textproto.readMIMEHeader` | 14.0 / 13.5 MB | ~0.6 KB each |

## The p50-under-load signal (5.6 → 10.2 ms)

The GC is exonerated as the driver on these numbers: 2.6–3.3% GC CPU
and ~1.3% of wall time in pauses cannot produce a near-doubling of
p50. The churn above (≈29 KB/request) is the GC-pressure source, but
it is collected promptly — 89 GCs in wave 1 alone. The p50 rise itself
is not cleanly measurable on this shared VM (wave 2's p50 of 65.4 ms
is plainly VM noise from concurrent slices), so this slice makes no
p50 verdict; the mechanism the attempt-3 note proposed — the
closed-loop bench holding more requests in flight for longer under a
slower per-request path — is consistent with the in-flight peak of 32
(= the client concurrency) but is not proven here.

## Attempt-3 peak (98.8 MiB) vs this run (73.1 MiB)

Same shape (peak RSS ≈ 1.9× idle), lower absolute peak here. The
~26 MiB gap is NOT resolved to a single cause. Candidates, none
proven: attempt 3's process ran two full passes (latency + RPS each)
before the peak was sampled, so its HeapSys high-water had two passes
to grow; my waves were shorter and the second wave slower (fewer
completions); VM noise moves the high-water mark. What is proven is
the mechanism — fixed one-time tables + bounded buffers + Go heap
high-water — and that it does not grow wave over wave.

## What was built in this slice

- `GET /dashboard/memstats` (`internal/server/memstats.go`): live
  `runtime.MemStats`, RSS, goroutines, in-flight, GC counters, plus
  `?profile=heap` / `?profile=goroutine` binary pprof snapshots.
  One-line route registration in `internal/server/server.go` and one
  guard-table line in `internal/server/guard.go` (plumbing only).
- `internal/server/memstats_test.go`: feed shape, recorder in-flight,
  both profile endpoints, bad-profile 400, and
  `TestDashboardMemstatsBoundedAcrossWaves` — two waves of real chat
  completions through the test server, settled heap 11.8 → 11.8 MiB,
  NumGC 17 → 26, in-flight back to 0.
- Dashboard Benchmark view: a "Live memory (this process)" card fed by
  the new endpoint (heap, RSS, goroutines, in-flight, GC pauses).
- This report; `docs/BENCHMARK.md` appendix; README clauses.

## Follow-ups (optimisation opportunities, NOT leak fixes — no action required for the verdict)

1. `internal/cache/cache.go`, `(*ExactCache).touch` — 71 MB churn
   (~3 KB/request): check whether the LRU touch allocates per lookup
   and whether it can be allocation-free. Perf only.
2. `internal/provider/provider.go`, `(*Provider).Clone` — 61.5 MB
   churn (~2.6 KB/request): a per-request clone; check whether the
   clone can be avoided or pooled. Perf only.
3. `internal/tokencount` + `tiktoken-go` — token counting is the top
   allocator overall (regex 83 MB + encode cum 140 MB): a cheaper
   counting path (e.g. cached counts per prompt hash) would cut
   per-request churn the most. Perf only; the 11.8 MiB retained
   tables are a legitimate one-time cost.
4. Settled goroutines 83 → 57: capture a *settled* goroutine profile
   (the memstats endpoint already serves one — fetch it 10 s after
   load) to name the stragglers exactly.

## Honest limitations

- One valid run, two waves, on a shared VM with concurrent slices
  running; latency numbers from this run are polluted and not quoted
  as measurements. Memory numbers (peaks, settled heaps, profile
  contents) are stable across both waves and four profiles.
- Profiled tree is HEAD `8448256` + the memstats feed, not the live
  worktree (unbuildable at the time — see Method). The memstats feed
  itself is the only difference from HEAD.
- The ~26 MiB gap to attempt 3's 98.8 MiB peak is unexplained, not
  hand-waved: candidates listed above, none proven.
- Mid-load heap profiles are post-GC by construction
  (`WriteHeapProfile` forces a GC); the 100 ms memstats polling gives
  the true mid-load peaks (HeapAlloc peak 36.3 MiB) and is the number
  to quote, not the profile's post-GC total.
