# Fresh CPU profile — 2026-11-18 (CHECKLIST 279–280)

## Verdict

**The targets are still missed, and the fix did what it targeted —
no more.** `GetKeyBudget` fell from 11.54% cum (273–274 profile)
to **7.52% cum** on the clean run, and the per-request cost
halved (90 µs → 47 µs: two SQLite reads per request became one
read plus an 87 ns memo hit, measured by the micro-benchmark).
The harness numbers did not move out of their band — added
latency 0.842/0.691 ms and RPS 1,451/1,432, against the
definitive run's 0.908/0.711 ms and 1,213/963 — so the ceiling
is still the remaining diffuse per-request CPU, not the
key-budget read. The biggest named router cost is now the
*other* budget read, the global-budget guard
`CheckBudgetOrError`/`GetBudget` at 8.20% cum — the next lever
for a future slice (memoise it per request the same way, or
combine it with the key-budget read). Above it sits only
syscalls (13.62% flat, network I/O, inherent) and the SQLite
parse path spread across all ledger queries.

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
  `go tool pprof -top`. Profile file: `cpu-keybudget3.prof` is
  NOT committed (binary pprof data); the numbers below are the
  record. Raw run JSON:
  `docs/benchmark-2026-11-18-keybudget-run.json`; merged file
  (run + verdict + carried-over definitive/CPU cards):
  `docs/benchmark-2026-11-18-keybudget.json`.
- The verdict run (attempt 3, 07:09–07:11 UTC) is clean: load
  1.05 → 0.91 across the run, direct-mock canaries 21,732 /
  21,805 RPS, no `go test`/`go build` running. RPS passes agree
  within 1.4% (1,451 vs 1,432); added-latency passes differ by
  18% (0.842 vs 0.691 ms — the same order of spread as the
  273–274 definitive run's 0.908/0.711 ms, disclosed here rather
  than smoothed over). Targets still MISSED on both passes.
- Profile totals: 86.82 s duration, 42.95 s total samples —
  directly comparable to the 273–274 profile (87.14 s, 42.12 s).
- Two earlier attempts in the same session are kept as polluted
  records, never the verdict:
  `docs/benchmark-2026-11-18-keybudget-attempt1-polluted.json`
  (pass-1 canary 11,963 RPS, marginally below the 12,000 rule;
  pass-2 RPS collapsed to 480, −62% vs pass 1) and
  `docs/benchmark-2026-11-18-keybudget-attempt2-polluted.json`
  (load rose 1.06 → 3.69 mid-run; RPS passes 523 vs 1,177,
  −55% apart). Their profiles corroborate the fix independently:
  GetKeyBudget 8.32% cum (attempt 1, 29.91 s samples) and 8.61%
  cum (attempt 2, 29.38 s samples).
- HEAD at run time: `ec029d573e7c7d3e0960bc5f22ea3b51ae21592b`
  (the tree moved between attempts — another slice committed
  between attempt 2 and attempt 3; the measured tree is that
  HEAD plus this slice's uncommitted changes).

## Top costs (fresh profile, cum share of 42.95 s)

Cum shares overlap — one sample can count under both a ledger
call and the SQLite function underneath it — so do NOT sum
rows. Flat is the function's own code; cum includes everything
it calls.

| Function | Flat | Cum | Note |
|---|---|---|---|
| `internal/runtime/syscall/linux.Syscall6` | 13.62% | 13.62% | Syscalls (network I/O) — inherent to proxying, not a lever |
| `ledger.(*Ledger).CheckBudgetOrError` | 0% | **8.20%** | The global-budget read — now the biggest named router cost |
| `ledger.(*Ledger).GetBudget` | 0% | 8.20% | The single budget-row read behind it |
| `modernc.org/sqlite/lib._sqlite3RunParser` | 0.14% | 8.29% | SQLite parse path under the ledger calls (overlaps both budget reads) |
| `modernc.org/sqlite/lib._sqlite3Parser` | 0.28% | 7.75% | Same SQLite path |
| `ledger.(*Ledger).GetKeyBudget` | 0.02% | **7.52%** | Was 11.54% — one read per request now, down from two |
| `runtime.mallocgc` | 0.84% | 7.05% | Allocation churn |
| `modernc.org/sqlite/lib._sqlite3VdbeExec` | 0.54% | 4.91% | SQLite execution under the ledger calls |
| `modernc.org/sqlite/lib._yy_reduce` | 0.19% | 6.92% | Same SQLite path |
| `cache.(*LocalSemanticCache).getScoped` | 2.33% | 2.86% | Semantic-cache lookup |
| `guardrails.scan` | 0.40% | 2.79% | Content-policy scan |
| `cache.(*LocalSemanticCache).set` | 0.58% | 1.54% | Semantic-cache store |
| `tokencount.CountPromptWire` | 0.05% | 1.21% | Was 1.16% — the memo is holding |
| `cache.(*ExactCache).touch` | 0% | 0.88% | Was 1.00% — flat |

## The memo is really in the served binary

`go tool pprof -peek GetKeyBudget` on the verdict run's profile
shows the call coming only from `CheckKeyBudget`, with children
`QueryRowContext` 89.56% (attempt-1 profile; the one remaining
database read), `Scan`, and the memo itself:
`keyBudgetMemo.put` 2.01% and `keyBudgetMemo.get` 0.80% of
`GetKeyBudget`'s cum — the memo overhead is noise next to the
SQLite parse under the single remaining read. The
micro-benchmark agrees: uncached miss 21,665 ns/op,
memo hit 87 ns/op (~250×).

## Comparison with the 273–274 profile

| Function | Before (273–274) | After (279–280, clean run) |
|---|---|---|
| `GetKeyBudget` | 11.54% | **7.52% cum** (8.32% / 8.61% on the two polluted attempts) |
| `CheckBudgetOrError` | 6.17% | 8.20% cum |
| `GetBudget` | 6.15% | 8.20% cum |
| `CountPromptWire` | 1.16% | 1.21% cum |
| `ExactCache.touch` | 1.00% | 0.88% cum |
| `_sqlite3RunParser` | 10.00% | 8.29% cum |
| `getScoped` | 3.13% | 2.86% cum |

Per-request `GetKeyBudget` cost: before 4.86 s / ~53,900
requests ≈ 90 µs; after 3.23 s / ~68,900 requests ≈ 47 µs —
halved, as designed. The share fell less than the per-request
cost because the run completed 28% more requests (the quiet
window held) and the other ledger reads stayed put:
`CheckBudgetOrError` is 3.52 s absolute vs 2.60 s before, i.e.
~51 µs/request before and after — flat per request, its share
rose only because `GetKeyBudget`'s fell. No win is claimed
beyond what these numbers show: the harness pair (0.842/0.691
ms, 1,451/1,432 RPS) sits in the same band as the definitive
run (0.908/0.711 ms, 1,213/963 RPS) — latency a touch better,
RPS a touch better, both inside run-to-run noise on this VM,
targets still missed by ~1.7× on latency and ~2.1× on RPS.

## What is next (for a future slice — not built here)

- The global-budget read (`CheckBudgetOrError`/`GetBudget`,
  8.20% cum, one SQLite read per request) is now the biggest
  named router cost: memoise it per request exactly like the
  key-budget row, or combine the two budget reads into one
  round trip. It is the same shape of fix, with the same
  per-request staleness bound.
- The SQLite parse path (`_sqlite3RunParser` 8.29% cum,
  overlapping every ledger query) is the next structural lever
  after both budget reads are memoised — it is shared across
  budget reads, usage recording and traffic-limit queries, so
  no single-call fix moves it.
- `getScoped` (2.86% cum) was already investigated in 263–264
  with no cheap win — do not re-try the same skip.
