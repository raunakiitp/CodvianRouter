# Go GC tuning — the low-end profile

Date: 2026-11-17. Slice 8 (CHECKLIST items 83–84), from research-log
entry 144. No push made. Every number below was measured in this run
by `scripts/bench-gc-profile.sh`; nothing is quoted from the research
log's production case (its "p99 61% cut" is someone else's traffic,
not this router's).

## The rule

Go 1.27 ships the Green Tea collector by default. The tuning pair is
`GOGC=off` + `runtime.SetMemoryLimit` (GOMEMLIMIT): GOGC alone is a
ratio, not a budget — it has no idea where the cgroup wall is — while
the limit pacer still runs GC near the ceiling even with GC "off"
(`runtime/debug`: "A negative percentage effectively disables garbage
collection, unless the memory limit is reached"; "This limit will be
respected even if GOGC=off").

Single rule, both profiles (`internal/gctune`, `ComputeLimit`):

```
detected = min(cgroup memory limit if set and finite, host MemTotal)
limit    = 85% of detected, capped at 2 GiB (no floor)
```

- 15% headroom: the limit covers Go heap only; stacks, mmap, cgo and
  kernel buffers sit outside it (research log: 5–15% below budget).
- 2 GiB ceiling: this is a local gateway whose working set is tens of
  MB; a larger heap limit only delays GC and grows RSS.
- No floor: a floor would push the limit above 85% of detected RAM
  on a tiny machine (a 128 MiB floor on a 128 MiB box is 100% of
  RAM) — the exact failure the headroom exists to prevent.
- Detection reads `/sys/fs/cgroup/memory.max` (v2, `"max"` =
  unlimited), then `/sys/fs/cgroup/memory/memory.limit_in_bytes`
  (v1), then `/proc/meminfo` MemTotal, taking the smallest known
  value. On this VM no cgroup limit file exists, so host RAM is used.
- GOMAXPROCS is untouched: Go 1.25+ is container-aware by default.

## Profiles

| | default | low-end |
|---|---|---|
| Enable | always (serve) | `--low-end` flag or `CODVIAN_ROUTER_LOWEND=1` |
| Memory limit | set (auto-detected or explicit) | set (same rule) |
| GC percent | untouched — Go's normal behaviour (GOGC=100 unless the user set GOGC) | `GOGC=off` (`debug.SetGCPercent(-1)`) |

Explicit config wins over auto-detection, in this order:
`CODVIAN_ROUTER_MEMORY_LIMIT_MB` > `GOMEMLIMIT` > auto-detected.
An explicitly set `GOGC` env var is never overridden. **GOGC=off is
never applied without a memory limit**: if no limit can be
determined, low-end degrades to the default profile and says so in
the log line.

Startup log lines (verbatim, run 9):

```
gc: profile=default, memory limit=2048 MiB (85% of 7934 MiB detected RAM, capped at 2048 MiB; source=auto-detected (host-meminfo)), GOGC=default
gc: profile=low-end, memory limit=2048 MiB (85% of 7934 MiB detected RAM, capped at 2048 MiB; source=auto-detected (host-meminfo)), GOGC=off (limit pacer still runs GC)
```

On this VM 85% of 7,934 MiB would be 6,744 MiB, so the 2 GiB ceiling
is what actually binds. Worked examples of the rule: 1 GiB detected
→ 912,680,550 bytes; 256 MiB → 228,170,137 bytes; 512 MiB cgroup cap
on a 7.7 GiB host → 512 MiB (the cap wins).

## Measured numbers

Rig: this VM — Linux x86_64, 4 CPUs (1.5 shared), 7,934 MiB RAM,
Go 1.27.4, binary built from this worktree. Method
(`scripts/bench-gc-profile.sh`): two local stdlib-Python mock
upstreams with a fixed 2 ms delay, two providers offering the same
model `model-a`, fresh `init` + gateway key per profile, 3 s settle,
50-request warmup, then 3,000 requests at 10 concurrent via
`scripts/bench-loadgen.py` (50 lexically distinct prompts, so the
semantic cache and single-flight coalescing cannot collapse the load
into cache hits). Idle RSS is VmRSS after the settle; peak RSS is
VmRSS sampled every 100 ms during the load. Latencies are over
successful (HTTP 200) requests only; errors are counted separately.

Four clean runs (6, 7, 9 default-first; 8 low-end-first via
`REVERSE_ORDER=1`):

| Run | Profile | ok / errors | p50 (ms) | p95 (ms) | p99 (ms) | mean (ms) | rps | idle RSS | peak RSS |
|---|---|---|---|---|---|---|---|---|---|
| 6 | default | 2990 / 10 | 6.11 | 21.15 | 46.77 | 10.03 | 979 | 36 MiB | 54 MiB |
| 6 | low-end | 2951 / 49 | 6.99 | 28.01 | 52.55 | 11.75 | 837 | 36 MiB | 79 MiB |
| 7 | default | 2972 / 28 | 6.40 | 28.17 | 56.95 | 10.33 | 956 | 36 MiB | 53 MiB |
| 7 | low-end | 2929 / 71 | 6.81 | 29.38 | 47.41 | 10.82 | 908 | 36 MiB | 79 MiB |
| 8 | low-end | 2962 / 38 | 6.97 | 21.37 | 49.64 | 10.62 | 927 | 36 MiB | 79 MiB |
| 8 | default | 2977 / 23 | 7.49 | 34.04 | 58.10 | 12.63 | 781 | 36 MiB | 54 MiB |
| 9 | default | 2984 / 16 | 7.35 | 32.64 | 53.91 | 11.34 | 871 | 36 MiB | 53 MiB |
| 9 | low-end | 2957 / 43 | 6.87 | 28.20 | 52.07 | 10.81 | 909 | 36 MiB | 79 MiB |

What the numbers say:

- **Idle RSS is identical**: 36 MiB in both profiles, every run.
  The "tens of MB idle" target holds either way.
- **Peak RSS is higher in low-end, every run**: 53–54 MiB vs
  79 MiB (+25 MiB, stable across all four runs). GOGC=off lets the
  heap grow further before the limit pacer collects. On a
  memory-constrained machine that is a real cost to weigh against
  any latency gain.
- **Latency has no consistent winner**: p50 6.1–7.5 ms, p95
  21–34 ms, p99 47–58 ms in both profiles; the winner flips run to
  run and the run-to-run swing exceeds the profile difference.
  No p99 improvement of the kind in the research log's production
  case was observed — this workload's heap is far below the limit,
  so GC is rarely the bottleneck here.
- **Errors are higher in low-end in all four clean runs**
  (default 10–28 vs low-end 38–71 of 3,000). See below — this is
  not a GC failure, and its mechanism is not established.

## The errors are the adaptive limiter, not the GC

The non-200 responses are the router's generic 502 "upstream
provider error", returned in ~0 ms. Diagnosis, all verified in this
run:

- The mock alone serves 2000/2000 clean at 20 concurrent
  (Python urllib), and a plain Go `http.Client` against the same
  mock serves 2000/2000 — the mock is not the cause.
- Disabling TFO (`CODVIAN_ROUTER_TFO=0`) did **not** reduce the
  errors (149/2000 default, 137/2000 low-end with TFO off) — the
  TFO hypothesis was tested and falsified.
- The cause is the adaptive concurrency limiter (slice 7,
  Gradient2, cap starts at 20, floor 2): `internal/router/router.go`
  `callChat` returns `adaptive.ErrLimited` when `TryAcquire`
  refuses, and the engine surfaces it as a 502. The cap is
  `limit*gradient + 1` with `gradient = rttMin/rttSmooth`; on a
  noisy shared VM the smoothed RTT exceeds the minimum under load,
  the cap shrinks below the in-flight count, and excess requests
  are shed fast instead of queueing. That is the router working as
  designed. The benchmark therefore uses two providers (two mock
  upstreams, same model) so a shed on one fails over to the other,
  and 10 concurrent instead of 20 — but shedding is reduced, not
  eliminated, and its rate varies with VM load (single-provider
  runs at 20 concurrent showed 17–170 errors/2000 across runs).
- The 502s are spread evenly through each load run, not clustered
  at startup — steady-state shedding, not a transient.

Open question: why low-end sheds more (38–71 vs 10–28) is **not
established**. It survives the order swap (run 8, low-end first:
38 vs 23), so it is not an order effect, but the mechanism is
unknown — one candidate is GC-pause-related RTT noise feeding the
Gradient2 limiter; it could also be an interaction with hedging
(HedgeDelay 150 ms). Do not quote it as a low-end defect until it
is reproduced on a quiet machine with the limiter's per-provider
cap/rejected counts captured (available from the router's
concurrency stats). The honest summary is: at this load the
profiles are indistinguishable on latency, low-end costs ~25 MiB
peak RSS, and low-end shed more requests in this VM — measure
again on a quiet machine before choosing a default.

## Files

- `internal/gctune/gctune.go` — detection, rule, `Apply()`
- `internal/gctune/gctune_test.go` — rule/detection/Apply tests
- `cmd/codvian-router/main.go` — `--low-end` flag, `gctune.Apply`
  at the top of `runServe`, usage text
- `scripts/bench-gc-profile.sh`, `scripts/bench-mock-upstream.py`,
  `scripts/bench-loadgen.py` — the benchmark harness
