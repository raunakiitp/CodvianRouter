# Codvian Router — Benchmark round 2, phase 1: baseline + profile

Measured 2026-11-18 ~02:45–03:40 IST (2026-11-17 21:15–22:10 UTC) on this
shared VM, same rig as before (`scripts/bench-geekflare.sh` +
`cmd/codvian-bench`, two mocks, `model-a`). No code was changed for this
phase — this is a pure measurement + profile of the current tree.

**Headline: the router regressed ~100x since the 2026-11-18 01:16 IST
re-run.** Added latency 0.541 ms → **73.6 ms**, RPS 1,483 → **41–51**,
peak RSS 37.5 MiB → **348–363 MiB**. The cause is identified with
evidence below (not a guess): checklist item 145 (local token counting,
landed after the 01:16 re-run) rebuilds the full tiktoken BPE encoder —
two compiled regexes, a 100,256-entry map inversion and a 100,256-element
sort — on **every** upstream-served request. That one call is 86.9% of
all CPU in the profile.

## Baseline runs

Two full runs of the rig were made. Run 1 is recorded but **invalid as a
baseline** (see why); run 2 is the baseline.

### Run 2 — the baseline (work dir `/tmp/codvian-geekflare-bench.1FQg8d`)

Raw JSON: `docs/benchmark-2026-11-18-round2-baseline2.json`.
Load average 2.37 at run start, 4.70 at run end (a `go test ./...` gate
from another slice was running in parallel during the first half; the
direct-mock numbers show the VM was usable — direct RPS 15,761/14,730,
in line with the 01:16 re-run's 15,199/14,346).

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.090 / 0.259 / 0.799 ms | 0.098 / 0.346 / 1.327 ms |
| Router latency p50 / p95 / p99 | 73.728 / 511.701 / 705.052 ms | 73.960 / 185.470 / 259.474 ms |
| Router latency mean / errors / cache hits | 156.6 ms / 0 / 10 of 1,000 | 86.2 ms / 0 / 92 of 1,000 |
| **Added latency (p50 diff)** | **73.638 ms** | **73.862 ms** |
| Direct RPS (32 x 20 s) | 15,761 (0 errors) | 14,730 (0 errors) |
| Router RPS (32 x 20 s) | **41.2** (858 ok, 994 x 502) | **50.7** (1,060 ok, 1,037 x 502) |
| Router RPS upstream (excl. cache hits) | 12.4/s (599 cache hits) | 13.5/s (778 cache hits) |
| Router RPS mean / p50 / p95 latency under load | 608.8 / 192.8 / 2,102.7 ms | 484.6 / 107.8 / 1,995.3 ms |
| Peak RSS during RPS | 348.9 MiB | 363.3 MiB |

Ready: `/healthz` 594.8 ms, `/v1/models` 1,110.0 ms. Idle RSS 39.5 MiB
(40,436 KiB). Note the ready times are ~3x the 01:16 re-run's
(181/326 ms) — the VM was loaded at spawn; ready is still far inside
the <10 s target.

The two passes agree closely on the number that matters (added latency
73.64 vs 73.86 ms), so the ~74 ms floor is stable, not noise. Pass 1's
higher mean/p95 is the parallel `go test` gate; pass 2 is the cleaner
pass and it shows the same floor.

### Run 1 — polluted, NOT the baseline (work dir `/tmp/codvian-geekflare-bench.qklQ9E`)

Raw JSON: `docs/benchmark-2026-11-18-round2-baseline1-polluted.json`.
Load average **17.09 at run start, 17.07 at run end** — another slice's
limiter-off experiment (`/home/hatch/.tmp/exp-limiter-off-2/`) plus
parallel `go test` gates were hammering the VM. Results: added latency
161.97 / 92.55 ms, router RPS 30.9 / 5.2, peak RSS 351 / 419 MiB. The
router's own log in that run already showed ~70–160 ms server-side
latencies per request with the router as the top CPU consumer, so the
regression was visible even through the contention — but the absolute
numbers are contention-dominated and are **not** used as the baseline.
Kept on file so nobody has to re-derive why there are two runs.

## Verdicts vs Geekflare targets at baseline (run 2)

| Metric | Target | Measured (run 2) | Verdict |
|---|---|---|---|
| Added latency (median) | < 0.5 ms | **73.6 ms** (pass 1), 73.9 ms (pass 2) | MISSED (~147x) |
| RPS (32 clients x 20 s) | > 3,000 | **41** (pass 1), 51 (pass 2) | MISSED (~60–70x) |
| Idle memory (RSS) | < 70 MB | **39.5 MiB** | MET |
| Peak RSS under load | — (not a target) | **348–363 MiB** (was 37–38 MiB) | REGRESSED ~10x — see profile |
| Ready time | < 10 s | **0.59 s** /healthz, 1.11 s /v1/models | MET |

## Profile evidence (the real request path, not a microbenchmark)

Method: fresh router built from this tree, started with
`CODVIAN_CPU_PROFILE=/tmp/profile-round2/cpu.prof` and
`CODVIAN_MEM_PROFILE=/tmp/profile-round2/mem.prof`, two mocks, then
`codvian-bench latency` (1,000 sequential, p50 **72.7 ms** — reproduces
the bench floor) followed by `codvian-bench rps` (32 x 20 s, 53.5 RPS,
1,138 x 502). Load average 3.85 at profile start, 2.35 at end. Profiles
written on SIGTERM shutdown. Full outputs:
`/tmp/profile-round2/cpu-top40.txt`, `mem-top25.txt` (ephemeral).

### CPU profile — 110.19 s of samples over 113.20 s

| Function | Flat | Cum | Share |
|---|---|---|---|
| `cmpbody` (byte-slice compare, inside sort) | 23.57 s | 23.57 s | 21.4% |
| `sort.partition_func` | 15.55 s | 56.05 s | 50.9% cum |
| `tiktoken-go.NewCoreBPE.func1` (sort closure) | 10.74 s | 35.60 s | 32.3% cum |
| `runtime.memmove` | 9.73 s | 9.73 s | 8.8% |
| `sort.pdqsort_func` | 0.37 s | 71.73 s | **65.1% cum** |
| **`tiktoken-go.NewCoreBPE`** | 1.52 s | **95.78 s** | **86.9% cum** |
| `Engine.ChatWithChainStrategy` | 0.02 s | 96.13 s | 87.2% cum |

Everything else is noise: the next router-owned entry after tiktoken is
`catalog.All` at 0.34 s flat / 1.94 s cum (1.8%), and the guardrail /
cache / ledger hot spots from round 1 are each < 1% — the round-1
fixes held; this is a new, single dominant cost. GC pressure
(`runtime.scanObject` 7.04 s cum, `tryDeferToSpanScan` 6.74 s cum) is
downstream of the same cause: every encoder build allocates tens of MB
transiently.

### Heap profile — 124.16 MB in use at shutdown

| Function | In-use |
|---|---|
| **`tiktoken-go.NewCoreBPE`** | **100.92 MB flat (81.3%)** |
| `catalog.All` | 8.81 MB (7.1%) |
| `tokencount.embeddedLoader.LoadTiktokenBpe` | 4.07 MB (3.3%) |
| via `GetEncoding` → `Engine.localPromptCount` | 105.49 MB cum (85.0%) |

This explains the peak-RSS regression too: under 32-client load many
encoder builds run concurrently, each holding tens of MB, so RSS
balloons to ~350 MiB while idle stays 39.5 MiB. The heap at shutdown
still holds ~101 MB of encoder structures from the last builds (GC had
not yet collected them, or they were still referenced).

### The call chain (read from the code, confirmed by the profile)

1. `internal/router/router.go:1402` — `Engine.reconcileUsage` calls
   `localPromptCount(model, req)` **unconditionally, first line**,
   on every successful attempt — even when the upstream *did* report
   usage (the count is then used only for the divergence comparison).
   The bench mock reports usage (`prompt_tokens: 8`), so this is not
   a fallback-only path: it runs per request.
2. `router.go:1371` — `localPromptCount` → `tokencount.CountPrompt`.
3. `internal/tokencount/tokencount.go:155` — `Count` calls
   `tiktoken.GetEncoding(encoding)` on **every call**. No cache.
4. `tiktoken-go v0.1.8, GetEncoding` → `NewCoreBPE` on **every call**.
   No cache in the library either. Each build: compile two `regexp2`
   patterns, invert the 100,256-entry encoder map into a decoder map,
   and `sort.Slice` 100,256 token byte-slices
   (`core_bpe.go:57` — the sort is the 65% in the profile). The
   embedded loader also re-parses the 1.68 MB `cl100k_base.tiktoken`
   file (base64 decode + map build) on every call.
5. `internal/server/server.go:2864` — the pre-send budget check
   (`enforcePromptBudget`) also calls `CountPrompt`, but for the
   bench's unpriced `model-a` it returns before counting
   (`price.InputUSDPer1M == nil` → skip). So the bench measures
   exactly **one** encoder build per upstream-served request.

Arithmetic check: profile run served ~1,320 upstream requests
(1,025 latency incl. warmup + 1,111 RPS ok − 816 cache hits; 502s and
cache hits never reach `reconcileUsage`). 95.78 s / 1,320 ≈ **72.6 ms
per `NewCoreBPE` call** — the same as the measured 72.7–73.9 ms p50
wall latency. The floor *is* the encoder build.

### Why this is a regression, not an old cost

Checklist item 145 (local token counting + ledger token-type split +
pre-send budget enforcement) landed in this sprint, after the 01:16
IST re-run that measured 0.541 ms / 1,483 RPS. `internal/tokencount/`
mtime is 2026-11-17 20:53 UTC; the 01:16 re-run predates it. The
round-1 hot spots (cache cosine, guardrail regexes, sync ledger) were
all fixed and stay fixed — none of them appears in this profile above
1%.

## Fix candidate for phase 2 (NOT applied yet)

Cache the built `*tiktoken.Tiktoken` per encoding name inside
`internal/tokencount` (only two encodings exist, both immutable):
build once with `sync.OnceValues`/`sync.Map`, then `Count` is a map
lookup + `EncodeOrdinary`. Expected effect, from the evidence above:
per-request cost ~73 ms → the encode itself (microseconds for a short
prompt), which should restore the ~0.5 ms added-latency floor and the
~1,500 RPS capacity, and remove the ~350 MiB RSS balloon (one encoder
per encoding stays resident, ~10–20 MB, instead of one per request).

Must verify in phase 2 before applying:

- **Concurrency safety of a shared encoder.** `tiktoken-go` has no
  mutex; `EncodeOrdinary` → `encodeOrdinaryNative` shares the
  `*regexp2.Regexp` objects built in `NewCoreBPE`. Check
  `dlclark/regexp2`'s concurrent-use guarantee (or the library's own
  docs); if it is not safe, guard `EncodeOrdinary` with a
  per-encoder mutex — a short prompt encode under a mutex is still
  microseconds and keeps the fix. Do not assume.
- **Scope.** The fix belongs in `internal/tokencount` only (the
  allowed hot-path set). `reconcileUsage`'s unconditional count is a
  second, smaller candidate — *only* if the cache alone does not
  restore the numbers: skipping the local count when the upstream
  reported usage would drop the divergence comparison, so it needs
  the coordinator's call, not a silent behaviour change. Do not do
  it unprompted.
- **Secondary, not in scope for phase 2 unless it shows up again:**
  `catalog.All` (1.8% CPU, 8.8 MB heap, via
  `ResolveAuthForBaseURL` per request) — measure after the tiktoken
  fix; it may become visible once the 87% is gone, but it is not the
  story now.

## What phase 2 must re-measure

Re-run `scripts/bench-geekflare.sh` (2 passes) after the fix and
compare against this baseline (run 2): added latency 73.64/73.86 ms,
RPS 41.2/50.7, peak RSS 348.9/363.3 MiB, ready 594.8/1,110.0 ms,
idle RSS 39.5 MiB. If the fix works, the numbers should return to
roughly the 01:16 re-run level (0.541/0.509 ms, 1,483/1,426 RPS) —
still MISSED on both Geekflare targets on this VM, honestly reported
as such. If they do not move, the fix gets reverted and the profile
re-run. The 3,000 RPS target remains out of reach on this 1.5-shared-
vCPU VM by the sequential-floor argument (a ~0.54 ms floor implies
~2,300 RPS max even with zero contention); that judgement stands
unless a quieter/dedicated box or a further hot-path cut changes the
floor.

## Files

- This report: `docs/benchmark-round2-report.md`
- Baseline JSON (run 2):
  `docs/benchmark-2026-11-18-round2-baseline2.json`
- Polluted run JSON (run 1, for the record):
  `docs/benchmark-2026-11-18-round2-baseline1-polluted.json`
- Profile artifacts (ephemeral, `/tmp`): `/tmp/profile-round2/`
  (`cpu.prof`, `mem.prof`, `cpu-top40.txt`, `mem-top25.txt`,
  `callers.txt`, `lat-router.json`, `rps-router.json`, `router.log`)
- No `docs/BENCHMARK.md` change in phase 1 — that update comes with
  the phase-2 numbers.

---

# Codvian Router — Benchmark round 2, phase 2: encoder cache + auth cache

Measured 2026-11-18 ~03:10–03:38 IST (2026-11-17 21:40–22:07 UTC) on
the same shared VM. Two fixes, two bench runs, two profiles. Headline:
**the 73 ms regression is fixed** (added latency 73.6 ms → 1.01 ms),
peak RSS 350 MiB → 65 MiB — but the 01:16 re-run level (0.54 ms,
1,483 RPS) is **not** restored, and the post-fix profile shows **no
further single hot spot** to cut. The remaining gap is diffuse cost
plus VM contention (see the contamination notes — read them before
quoting any number below).

## Fix 1 — cached tiktoken encoder (`internal/tokencount/tokencount.go`)

Concurrency safety was verified against the dependency source before
applying, not assumed:

- `regexp2` v1.10.0 `regexp.go:25` documents "A Regexp is safe for
  concurrent use by multiple goroutines", implemented via a
  mutex-guarded pool of per-match runners (`getRunner`/`putRunner`
  under `muRun`, `runner.go:1585-1612`) — concurrent matches on the
  shared `tlRegex` each get their own runner and share no mutable
  match state.
- `CoreBPE.encodeOrdinaryNative` only reads `bp.encoder` (read-only
  after `NewCoreBPE`) and calls `FindStringMatch` on `bp.tlRegex`;
  `bytePairEncode` works purely on locals plus read-only map lookups
  (`bpe.go`). `Tiktoken.EncodeOrdinary` touches no other encoder
  state.

So **no per-encoder mutex** is needed; the mutex guards only the
cache map, never an encode. Verified empirically with a scratch
program (removed afterwards): 6,400 concurrent `Count` calls across
32 goroutines under `-race`, zero races, counts identical to the
pinned values (`hello world` = 2, fox sentence = 9, CJK o200k = 2 /
cl100k = 4). First build 422 ms under `-race`, cached count 62.7 µs.

Change: `getEncoder()` memoizes one `*tiktoken.Tiktoken` per encoding
name (`sync.Mutex`-guarded map, at most 2 entries); `Count()` uses it
instead of `tiktoken.GetEncoding` on every call. Public behaviour is
identical (same counts, same fallback, same error text). A build
failure is not cached, so a later call retries. `go build ./...` and
`go vet` clean on the touched packages. (No `go test` was run in this
phase — the coordinator runs the one full gate at the end.)

## Bench run A — encoder fix only (loadavg 1.57 at start, 7.81 at end)

Raw JSON: `docs/benchmark-2026-11-18-round2-phase2.json`, work dir
`/tmp/codvian-geekflare-bench.xUU0mc`.

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.114 / 0.209 / 0.340 ms | 0.072 / 0.267 / 2.108 ms |
| Router latency p50 / p95 / p99 | 1.580 / 9.985 / 51.995 ms | 1.548 / 6.711 / 24.794 ms |
| Router latency mean / errors / cache hits | 3.736 ms / 0 / 10 of 1,000 | 2.728 ms / 0 / 61 of 1,000 |
| **Added latency (p50 diff)** | **1.466 ms** | **1.476 ms** |
| Direct RPS (32 x 20 s) | 10,338 (0 errors) | 11,919 (0 errors) |
| Router RPS (32 x 20 s) | **261.3** (5,242 ok, 4,873 x 502) | **261.5** (5,232 ok, 4,869 x 502) |
| Router RPS upstream (excl. cache hits) | 216.0/s (908 cache hits) | 216.6/s (898 cache hits) |
| Router RPS mean / p50 / p95 latency under load | 67.0 / 70.8 / 165.1 ms | 68.9 / 72.2 / 163.3 ms |
| Peak RSS during RPS | 83.4 MiB | 86.2 MiB |

Ready: `/healthz` 163.6 ms, `/v1/models` 282.8 ms. Idle RSS 39.6 MiB
(40,584 KiB).

The 73 ms floor is gone — the encoder fix held — but the numbers did
**not** return to the 01:16 level (0.541 ms / 1,483 RPS). A second
bottleneck was exposed, not a fix failure.

## Profile after fix 1 (`/tmp/profile-round2b/`, first profile in that dir)

Method as in phase 1 (fresh router + two mocks, `CODVIAN_CPU_PROFILE`
/ `CODVIAN_MEM_PROFILE`, latency + RPS, SIGTERM). Load average 4.92
at start. Latency p50 1.316 ms, RPS 338.3 (6,785 ok, 6,136 x 502,
upstream 293.8/s, 893 cache hits), under-load p50 55.3 ms.

CPU profile (20.98 s of samples over 23.33 s): **`catalog.All`
8.95 s cum, 42.66% of all CPU**, via `ResolveAuthForBaseURL` (9.30 s
cum, 44.33%); `Provider.Clone` 3.48 s cum (16.59%). GC churn
(`tryDeferToSpanScan` 18.02%, `scanObject` 18.26%) is downstream of
the cloning. Heap in use at shutdown 31.6 MB (vs 124.2 MB at
baseline): `NewCoreBPE` 12.8 MB flat is the ONE cached encoder
(one-time resident, expected), `catalog.All` 6.7 MB cum.

The call chain: `server.checkLocalReadiness` (`server.go:2718`, on
**every** `/v1/chat/completions` request) → `RelayTargetForModel`
(`registry.go:1010`) → `resolveAuthHeaders` → for a base-URL provider
(no `CatalogID`, as the bench mocks are): `ResolveAuthForBaseURL`
calls `catalog.All()` **twice**, and the not-found fallback
`catalogWireForBaseURL` calls it **twice more** — 4 full deep-clones
of the whole catalog per request. The `CatalogID` path is not exempt
either: `lookupCatalog` → `catalog.ByID` → `All()`. The readiness
check only needs `IsLocal`/`ProviderID`, but the full auth resolution
runs regardless. This was the "secondary" candidate flagged in phase
1 — it became the whole story once the 87% tiktoken share was gone.

## Fix 2 — memoized auth resolution (`internal/provider/registry.go`)

In-scope fix (`internal/provider` is in the allowed hot-path set;
`internal/catalog` is not, so `All()` itself was left alone).
`resolveAuthHeaders` is now a memoizing wrapper around
`resolveAuthHeadersUncached` (the old body): one cache entry per
provider ID, keyed by a SHA-256 fingerprint of every input the
resolution depends on (ID, BaseURL, CatalogID, AuthEndpoint,
AuthVariant, `APIKeyEncrypted` ciphertext). The plaintext key is
deliberately **not** an input: both call sites (`ClientFor`,
`RelayTargetForModel`) derive it as `decryptKey(p.APIKeyEncrypted)`,
so the ciphertext covers key rotation (a fresh nonce per seal changes
the fingerprint) while the secret never enters a map key or hash
input. `Enabled`/`QuotaHint` are not inputs (they never affect auth
or wire). The catalog is compile-time data with no runtime mutation
path. **Errors are not cached** — a config fix takes effect
immediately. Cached header maps are copied on store and on return,
preserving the fresh-map-per-call contract. `Remove()` evicts the
entry, so the map cannot grow across add/remove cycles (bounded by
provider count). The cache lock (`authMu`) is never held during the
resolution itself (check → compute → store), so there is no
lock-ordering hazard with `r.mu`. The fix does not change the
readiness check itself — it still resolves per request, but now from
the memo instead of from 4 catalog clones.

## Bench run B — both fixes (loadavg 3.60 at start, 19.35 at end)

Raw JSON: `docs/benchmark-2026-11-18-round2-phase2b.json`, work dir
`/tmp/codvian-geekflare-bench.DfblE9`.

| | Pass 1 | Pass 2 |
|---|---|---|
| Direct latency p50 / p95 / p99 | 0.080 / 0.307 / 1.194 ms | 0.093 / 0.885 / 3.565 ms |
| Router latency p50 / p95 / p99 | 1.093 / 5.327 / 50.756 ms | 1.216 / 13.693 / 63.001 ms |
| Router latency mean / errors / cache hits | 2.714 ms / 0 / 10 of 1,000 | 4.607 ms / 0 / 10 of 1,000 |
| **Added latency (p50 diff)** | **1.013 ms** | **1.123 ms** |
| Direct RPS (32 x 20 s) | 5,422 (0 errors) | 4,628 (0 errors) |
| Router RPS (32 x 20 s) | **306.9** (6,194 ok, 5,758 x 502) | **343.7** (6,898 ok, 6,570 x 502) |
| Router RPS upstream (excl. cache hits) | 219.2/s (1,769 cache hits) | 228.4/s (2,314 cache hits) |
| Router RPS mean / p50 / p95 latency under load | 56.9 / 48.9 / 153.7 ms | 49.3 / 29.4 / 113.2 ms |
| Peak RSS during RPS | 64.9 MiB | 65.5 MiB |

Ready: `/healthz` 408.6 ms, `/v1/models` 625.5 ms. Idle RSS 37.6 MiB
(38,492 KiB).

**Contamination warning for run B — do not quote it as a clean
number.** A VM storm started mid-run: load average 3.60 at start,
**19.35 at end**, and the canary proves it — the direct mock itself
could only do 5,422 / 4,628 RPS in run B vs 10,338 / 11,919 in run A
and 15,199 / 14,346 at the 01:16 re-run. The VM gave the bench roughly
a third of the CPU the 01:16 run had, so run B **underestimates** the
router on a quiet VM, most in pass 2 (direct latency mean 0.378 ms,
p95 0.885 ms — the mock itself was slow). What run B still shows,
even through the storm: added latency fell again (1.47 → 1.01/1.12
ms), peak RSS fell again (83/86 → 65 MiB), and headline RPS rose
(261 → 307/344) — but the upstream rate barely moved (216 → 219/228
per s); the headline gain is cache hits (1,769/2,314 vs ~900), not
more upstream capacity. The auth-cache fix removed the catalog CPU
(see the profile below); it did not by itself restore 01:16
throughput.

## Profile after fix 2 (`/tmp/profile-round2b/`, overwritten by the second run there)

Load average 17.36 at start (storm still running — absolutes are
depressed, shares are what matter). Latency p50 1.821 ms, RPS 298.7
(5,995 ok, 5,758 x 502, upstream 190.1/s, 2,180 cache hits),
under-load p50 54.8 ms. CPU profile: only 6.78 s of samples over
28 s wall (the router was starved), and the top 40 nodes contain
**no `catalog.*`, `ResolveAuth*`, `tiktoken.*` or `tokencount.*`
entry at all** — both fixes fully removed their costs from the
profile. What remains is diffuse: `Syscall6` 11.06% flat (syscalls
under contention, not a hot function), semantic-cache `Get` 2.65%
cum, GC `tryDeferToSpanScan` 3.24% (down from 18%), `regexp2`
runner `execute` 2.65% (the encode itself), SQLite
`_sqlite3VdbeExec` 4.57% cum (ledger). Heap in use at shutdown
19.3 MB (down from 31.6 MB post-fix-1, 124.2 MB at baseline):
`NewCoreBPE` 7.6 MB + `LoadTiktokenBpe` 6.3 MB cum is the one cached
encoder and its rank map (one-time resident); everything else is
≤ 1.2 MB.

## Verdicts vs Geekflare targets after phase 2 (run B, contaminated — see above)

| Metric | Target | Measured (run B) | Verdict |
|---|---|---|---|
| Added latency (median) | < 0.5 ms | **1.013 ms** (pass 1), 1.123 ms (pass 2) | MISSED (~2x) — better than baseline 73.6 ms by 73x, worse than 01:16's 0.541 ms |
| RPS (32 clients x 20 s) | > 3,000 | **307** (pass 1), 344 (pass 2) | MISSED (~9–10x) — better than baseline 41/51 by ~7x, worse than 01:16's 1,483 |
| Idle memory (RSS) | < 70 MB | **37.6 MiB** | MET |
| Peak RSS under load | — (not a target) | **64.9–65.5 MiB** (was 349–363, 01:16: 37.5) | IMPROVED 5x vs baseline, still ~1.7x the 01:16 level |
| Ready time | < 10 s | **0.41 s** /healthz, 0.63 s /v1/models | MET |

## What phase 2 did and did not establish

- **Established:** the 73 ms regression was the uncached tiktoken
  encoder build, and the second 43%-of-CPU cost was the uncached
  catalog walk in per-request auth resolution. Both are fixed with
  evidence (race-clean concurrent counts; `catalog.*` absent from
  the post-fix-2 CPU top 40; heap 124 MB → 19 MB). Added latency
  improved 73.6 → 1.47 → 1.01 ms across the two fixes; peak RSS
  350 → 83 → 65 MiB.
- **Not established:** the 01:16 throughput (1,483 RPS, 0.54 ms)
  is not restored, and there is no longer a single hot function to
  blame — the post-fix-2 profile is diffuse. Two confounders block
  a clean claim either way: (1) run B ran through a VM storm
  (direct-mock canary 5.4k vs 15.2k RPS), and (2) other slices
  landed concurrent tree changes during phase 2 (internal/router
  `PriceFunc`/request-budget/`routetrace` rework ~21:46–21:51 UTC;
  internal/reasoning + internal/server edits; the tree was broken
  mid-flight three times and had to be waited out). Run B measures
  that tree, not the two fixes in isolation.
- **What is needed next:** a clean re-run of
  `scripts/bench-geekflare.sh` in a genuinely quiet window
  (loadavg < ~3 at start AND end, direct-mock RPS back above
  ~14k) on a green tree, to say whether the 01:16 level is
  actually restored; if it is not, the next step is limiter /
  admission work (the 502 rate is still ~48%, and under-load p50
  is 29–49 ms vs 5.6 ms at 01:16), not another hot-path cut —
  there is no hot path left to cut.

## Files

- This report: `docs/benchmark-round2-report.md` (this appendix)
- Run A JSON (encoder fix only):
  `docs/benchmark-2026-11-18-round2-phase2.json`
- Run B JSON (both fixes):
  `docs/benchmark-2026-11-18-round2-phase2b.json`
- Profile artifacts (ephemeral, `/tmp`): `/tmp/profile-round2b/`
  (`cpu.prof`, `mem.prof`, `cpu-top40.txt`, `mem-top25.txt`,
  `lat-router.json`, `rps-router.json`, `router.log`) — note the
  directory was overwritten by the second (post-fix-2) profile run;
  the post-fix-1 profile's numbers are in the section above.
- Code changed: `internal/tokencount/tokencount.go` (encoder cache),
  `internal/provider/registry.go` (auth-resolution memo). Nothing
  else. No `CHECKLIST.md` edit, no GitHub writes, no dashboard
  (`web/` / `internal/server`) changes.
