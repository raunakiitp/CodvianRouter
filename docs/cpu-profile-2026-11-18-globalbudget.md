# Fresh CPU profile — 2026-11-18 (CHECKLIST 299–300)

## Verdict

**The targets are still missed, and the memo did what it could —
which, on the measured path, is nothing.** `GetBudget` moved
from 8.20% cum (279–280 profile) to **7.96% cum**, and
`CheckBudgetOrError` from 8.20% to **8.01% cum** — a within-noise
move, not a win, and none is claimed. The per-request cost is
unchanged: 3.15 s / 61,443 requests ≈ **51 µs/request**, exactly
the ~51 µs/request of the 279–280 profile (3.52 s / ~68,900).
The harness pair sits in the same band as the key-budget run —
added latency 0.851/0.777 ms and RPS 1,273/1,174, against the
key-budget run's 0.842/0.691 ms and 1,451/1,432 — so the ceiling
is still the remaining diffuse per-request CPU, not the
global-budget read. The reason was found in the code before the
slice landed and is recorded here rather than discovered after:
on today's hot paths the global-budget guard runs exactly once
per request (`enforcePromptBudget`, called once per chat /
responses / embeddings request, makes exactly one `GetBudget`
read — one of `CheckBudgetOrError` or `CheckBudgetForEstimate`,
never both; the pre-body admission checks only the key budget).
The key-budget memo saved a second read because that guard ran
twice; this memo has no second read to save. The memo is still
the specified dedup mechanism — any request that hits the guard
twice (a future pre-body global-budget admission check, a retry
inside one request) shares one read — and the micro-benchmark
proves the mechanism itself works (memo hit ~228 ns/op vs
~15,000 ns/op uncached, ~65×). The remaining cost is the one
SQLite read per request, which only a cross-request cache or a
combined budget read could remove — both change the staleness
bound and are out of scope here.

## Method

- Rig: the Geekflare-style harness, `scripts/bench-geekflare.sh`
  (two instant `codvian-bench` mock upstreams, 1,000 sequential
  requests for added latency, 32 clients × 20 s for RPS, two
  passes each). Shared VM, 4 CPUs, Go 1.27.4.
- CPU profile: the real binary under the same run, via the
  operator profile switch `CODVIAN_CPU_PROFILE=<path>` in
  `cmd/codvian-router/main.go` (writes a pprof CPU profile from
  serve start to graceful SIGTERM shutdown — the bench script
  kills the router with SIGTERM at the end, so the profile covers
  the whole run including both passes). Analysed with
  `go tool pprof -top` and `-peek`. Profile file:
  `cpu-globalbudget.prof` is NOT committed (binary pprof data);
  the numbers below are the record. Raw run JSON:
  `docs/benchmark-2026-11-18-globalbudget-run.json`; merged file
  (run + verdict + carried-over definitive / CPU /
  key-budget cards):
  `docs/benchmark-2026-11-18-globalbudget.json`.
- The verdict run (07:50:39–07:52:12 UTC) is clean: load
  1.30 → 1.48 across the run, direct-mock canaries 19,599 /
  20,009 RPS, no `go test`/`go build` running. RPS passes agree
  within 8% (1,273 vs 1,174); added-latency passes differ by 9%
  (0.851 vs 0.777 ms — the same order of spread as the
  key-budget run's 0.842/0.691 ms, disclosed here rather than
  smoothed over). Targets still MISSED on both passes.
- Profile totals: 87.14 s duration, 39.31 s total samples —
  directly comparable to the 279–280 profile (86.82 s, 42.95 s).
- HEAD at run time: `db5c626adc268c4754c59cbd3696bf1f90832834`
  (the tree at the verdict run is that HEAD plus this slice's
  uncommitted changes, built and benched from the clean
  worktree `$HOME/.tmp/wt-gb299` — the main worktree carried
  another slice's uncommitted, then-unbuildable changes, so the
  run was taken from the worktree; see the gate section in
  CHECKLIST.md).

## Top costs (fresh profile, cum share of 39.31 s)

Cum shares overlap — one sample can count under both a ledger
call and the SQLite function underneath it — so do NOT sum
rows. Flat is the function's own code; cum includes everything
it calls.

| Function | Flat | Cum | Note |
|---|---|---|---|
| `internal/runtime/syscall/linux.Syscall6` | 12.77% | 12.77% | Syscalls (network I/O) — inherent to proxying, not a lever |
| `ledger.(*Ledger).GetKeyBudget` | 0% | **8.27%** | Was 7.52% — one read per request, memo holding; the share rose only because total samples fell (39.31 s vs 42.95 s) and fewer requests ran |
| `ledger.(*Ledger).CheckBudgetOrError` | 0% | **8.01%** | Was 8.20% — within noise; per-request cost flat at ~51 µs |
| `ledger.(*Ledger).GetBudget` | 0% | **7.96%** | Was 8.20% — the single budget-row read behind it; same within-noise move |
| `modernc.org/sqlite/lib._sqlite3RunParser` | 0.10% | 7.78% | SQLite parse path under the ledger calls (overlaps both budget reads) |
| `modernc.org/sqlite/lib._sqlite3Parser` | 0% | 7.50% | Same SQLite path |
| `runtime.mallocgc` | 0.74% | 6.87% | Allocation churn |
| `modernc.org/sqlite/lib._yy_reduce` | 0.33% | 6.82% | Same SQLite path |
| `modernc.org/sqlite/lib._sqlite3VdbeExec` | 0.61% | 4.86% | SQLite execution under the ledger calls |
| `runtime.tryDeferToSpanScan` | 2.85% | 4.17% | GC span scan |
| `runtime.scanObject` | 0.53% | 4.07% | GC object scan |
| `cache.(*LocalSemanticCache).getScoped` | 2.62% | 3.31% | Semantic-cache lookup |
| `guardrails.scan` | 0.56% | 3.00% | Content-policy scan |
| `tokencount.CountPromptWire` | 0.03% | 1.32% | Was 1.21% — flat |
| `cache.(*ExactCache).touch` | 0.10% | 0.92% | Was 0.88% — flat |

## The memo is really in the served binary

`go tool pprof -peek GetBudget` on the verdict run's profile
shows the call coming only from `checkBudgetDetailed`, through
`getBudgetMemoised` into `getBudgetUncached` — the memo layer
is in the served path, and its `get`/`put` are below the
0.20 s node threshold (noise next to the SQLite parse under the
single remaining read). The micro-benchmark agrees: uncached
miss 15,012 ns/op, memo hit 228 ns/op (row) / 248 ns/op
(miss) — ~65×. The memo dedupes correctly when a request DOES
hit the guard twice (proven by `TestBudgetMemoSharesOneRead` /
`TestCheckBudgetOrErrorUsesMemo`, which mutate the database
behind the memo's back); the chat path simply never does today.

## Comparison with the 279–280 profile

| Function | Before (279–280) | After (299–300, clean run) |
|---|---|---|
| `GetBudget` | 8.20% | **7.96% cum** |
| `CheckBudgetOrError` | 8.20% | **8.01% cum** |
| `GetKeyBudget` | 7.52% | **8.27% cum** |
| `CheckKeyBudget` | — | 8.27% cum |
| `CountPromptWire` | 1.21% | 1.32% cum |
| `ExactCache.touch` | 0.88% | 0.92% cum |
| `_sqlite3RunParser` | 8.29% | 7.78% cum |
| `_sqlite3Parser` | 7.75% | 7.50% cum |
| `_yy_reduce` | 6.92% | 6.82% cum |
| `mallocgc` | 7.05% | 6.87% cum |
| `getScoped` | 2.86% | 3.31% cum |
| `guardrails.scan` | 2.79% | 3.00% cum |
| `Syscall6` | 13.62% flat | 12.77% flat |

Per-request `GetBudget` cost: before 3.52 s / ~68,900
requests ≈ 51 µs; after 3.15 s / 61,443 requests ≈ 51 µs —
flat, as designed (one read per request before and after).
No win is claimed beyond what these numbers show: the harness
pair (0.851/0.777 ms, 1,273/1,174 RPS) sits in the same band as
the key-budget run (0.842/0.691 ms, 1,451/1,432 RPS) and the
definitive run (0.908/0.711 ms, 1,213/963 RPS) — latency and
RPS both inside run-to-run noise on this VM, targets still
missed by ~1.7× on latency and ~2.4× on RPS. The RPS passes
are 12–18% below the key-budget run's; that is not attributed
to the memo (one map lookup per read cannot cost 12% of
throughput) and not claimed as a regression either — the
clean-run band across all three runs spans 963–1,451 RPS, and
this run sits inside it. Pass-2 peak RSS 108.8 MiB vs pass-1
78.2 MiB is heap/GC variance on the shared VM, recorded, not
explained away.

## What is next (for a future slice — not built here)

- The two budget reads together (`GetKeyBudget` 8.27% +
  `GetBudget` 7.96% cum, overlapping SQLite parse) are now the
  biggest named router cost, and each is already one read per
  request: the next lever is combining them into one round trip
  (or a cross-request cache with an explicit invalidation
  contract) — both change the staleness bound, so they need
  their own slice and their own honesty note.
- The SQLite parse path (`_sqlite3RunParser` 7.78% cum,
  overlapping every ledger query) is the next structural lever
  after the budget reads — it is shared across budget reads,
  usage recording and traffic-limit queries, so no single-call
  fix moves it.
- `getScoped` (3.31% cum) was already investigated in 263–264
  with no cheap win — do not re-try the same skip.
