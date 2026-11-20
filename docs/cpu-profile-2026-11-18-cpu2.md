# Fresh CPU profile — 2026-11-18 (CHECKLIST 273–274)

## Verdict

**The targets are still missed, and the fresh profile says the
remaining cost is diffuse — no single hot spot left above ~12%
cumulative, and the biggest named cost is now the per-key budget
read.** Token counting, the target of the 249–250 fixes, has
fallen further (1.16% cum, from 2.95% in the after-profile), so
the memo is holding. What is next is the budget path — above all
`GetKeyBudget` at 11.54% cum — then the semantic-cache lookup
(`getScoped`, 3.13%) and the SQLite parse/exec path underneath
the ledger calls. Nothing here is a step-change lever: expect
incremental gains, or a bigger/dedicated box, or front-door
admission.

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
  `go tool pprof -top`. Profile file: `docs/cpu2-cpu.prof` is
  NOT committed (binary pprof data); the numbers below are the
  record. Raw run JSON: `docs/benchmark-2026-11-18-cpu2-profile.json`.
- The profile run (06:19–06:20 UTC) is the cleanest window of the
  session: load 1.25 → 0.97 across the run, direct-mock canaries
  21,058 / 21,160 RPS. Its numbers corroborate the definitive
  run: added latency 0.800 / 0.844 ms, RPS 1,363 / 1,230
  (passes agree within 10%, unlike the definitive run's −20.6%).
  Targets still MISSED on both passes.
- Profile totals: 87.14 s duration, 42.12 s total samples.

## Top costs (fresh profile, cum share of 42.12 s)

Cum shares overlap — one sample can count under both a ledger
call and the SQLite function underneath it — so do NOT sum
rows. Flat is the function's own code; cum includes everything
it calls.

| Function | Flat | Cum | Note |
|---|---|---|---|
| `internal/runtime/syscall/linux.Syscall6` | 13.11% | 13.11% | Syscalls (network I/O) — inherent to proxying, not a lever |
| `ledger.(*Ledger).GetKeyBudget` | 0.05% | **11.54%** | Per-key budget read — the biggest named router cost |
| `modernc.org/sqlite/lib._sqlite3RunParser` | 0.21% | 10.00% | SQLite parse path under the ledger calls (overlaps GetKeyBudget) |
| `modernc.org/sqlite/lib._sqlite3Parser` | 0.33% | 9.40% | Same SQLite path |
| `modernc.org/sqlite/lib._yy_reduce` | 0.74% | 8.45% | Same SQLite path |
| `ledger.(*Ledger).CheckBudgetOrError` | 0.05% | 6.17% | Was 8.59% in the 249–250 after-profile |
| `ledger.(*Ledger).checkBudgetDetailed` | 0% | 6.15% | The single budget-row read (249–250 fix) |
| `ledger.(*Ledger).GetBudget` | 0.10% | 6.15% | Per-request budget read |
| `runtime.mallocgc` | 0.69% | 6.13% | Allocation churn (memstats slice: ~29 KB/request) |
| `modernc.org/sqlite/lib._sqlite3VdbeExec` | 0.38% | 5.41% | SQLite execution under the ledger calls |
| `runtime.scanObject` | 0.85% | 4.39% | GC scanning |
| `cache.(*LocalSemanticCache).getScoped` | 2.37% | 3.13% | Semantic-cache lookup — biggest single router function by flat |
| `guardrails.scan` | 0.64% | 2.56% | Content-policy scan |
| `cache.(*LocalSemanticCache).set` | 0.55% | 1.71% | Semantic-cache store |
| `tokencount.CountPromptWire` | 0% | 1.16% | Was 2.95% in the after-profile — memo holding |
| `cache.(*ExactCache).touch` | 0.10% | 1.00% | Was 0.92% — flat |
| `runtime.tryDeferToSpanScan` | 2.59% | 3.56% | GC span scanning |

## Comparison with the 249–250 after-profile

| Function | After-profile (249–250) | Fresh profile (273–274) |
|---|---|---|
| `CountPromptWire` | 2.95% | 1.16% cum |
| `CheckBudgetOrError` | 8.59% | 6.17% cum |
| `ExactCache.touch` | 0.92% | 1.00% cum |

The 249–250 fixes did what they targeted: token counting is now
a ~1% cost, and the budget guard's share fell by ~2.4 points.
The cost did not disappear — it moved into the open as the
per-key budget read and the SQLite path under it.

## What is next (for a future slice — not built here)

1. **Per-key budget read (`GetKeyBudget`, 11.54% cum).** The
   single biggest named cost. Candidates: cache the key-budget
   row in memory (it changes only when the ledger writes),
   or combine the key-budget and request-budget reads into one
   query. This is the only remaining named cost above 10%.
2. **Semantic-cache lookup (`getScoped`, 3.13% cum / 2.37%
   flat).** Already investigated in 263–264: a ≤0-dot skip in
   the best-slot scan was tried and reverted (before/after
   minima overlap), so do NOT re-try the same skip. A different
   approach (e.g. a cheaper pre-filter, or skipping the semantic
   lookup when the exact cache already missed on a cold prompt)
   would need its own measurement.
3. **SQLite parse path (`_sqlite3RunParser` 10.00% cum).**
   Check whether the prepared statements from 249–250 actually
   cover the hot queries, and whether the ledger's write path
   (usage inserts) can be batched or deferred off the request
   path. The parse share overlaps `GetKeyBudget`, so fix (1)
   first and re-measure before touching the parser path.
4. **No step-change lever in this profile.** The largest flat
   entry is syscalls (13.11%), which is the proxying itself.
   After (1)–(3), the remaining levers are the ones E1 already
   named: a bigger/dedicated box, or front-door admission
   (reject overload before the expensive work).

## Honest limitations

- One profile, one run, on the shared VM — shares are the
  signal, not absolute seconds; a second profile on another
  quiet window could move the small rows by a point or two.
- The profile run's numbers (0.800/0.844 ms, 1,363/1,230 RPS)
  are a corroboration of the definitive run, not a second
  verdict — the definitive record remains
  `docs/benchmark-2026-11-18-cpu2.json`.
- The pprof file itself (`cpu2-cpu.prof`) is not committed; it
  lives in the operator's tmp dir. The table above is the
  committed record.
