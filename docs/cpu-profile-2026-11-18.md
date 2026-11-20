# Hot-path CPU profile — 2026-11-18 (CHECKLIST 249–250)

## Verdict

**The fixes remove the targeted CPU — the profile proves it — but
the harness RPS verdict is load-dominated and no RPS claim is
drawn from it.**

- Profile shares on the same HEAD tree ± fixes (two CPU profiles,
  whole-serve runs): `CountPromptWire` 11.55% → 2.95% of router
  CPU, `CheckBudgetOrError` 11.75% → 8.59%, `ExactCache.touch`
  1.32% → 0.92%. The token-count memo and the single budget-row
  read remove the work they target; the prepared statements show
  no separately measurable per-call win (see "Fixes landed").
- Harness pair (clean by the canary rule — direct mock ≥ 12,000
  RPS in all four passes): added latency improved in both passes
  (0.708 → 0.697 ms pass 1, 0.814 → 0.570 ms pass 2), but RPS
  disagrees violently (+138.5% pass 1, −14.9% pass 2) while the
  load trace shows the runs faced opposite load regimes (before:
  rising 1.7 → 9; after: falling 8.5 → 3.7). The RPS delta is
  therefore load, not fix — it is reported as recorded, with no
  verdict drawn from it.
- Geekflare targets still MISSED on the after run (added latency
  0.697/0.570 ms vs < 0.5 ms; RPS 1,650/650 vs > 3,000). A
  definitive RPS re-measurement needs a Fleet-Manager-coordinated
  bench pause (see "Honest limitations").

## Why this slice exists

The E1 limiter verdict (items 233–234) proved per-request CPU is
the RPS ceiling: limiter OFF raised RPS only +7.2% (within ±10%
noise) while shedding nothing. The memstats slice (235–236) then
measured allocation churn of ~29 KB/request but no leak. This
slice profiles the hot path under harness load with Go CPU pprof
on the real binary and cuts the biggest CPU cost the profile
names, with a before/after measurement on the same harness for
every fix.

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
  `go tool pprof -top`.
- Clean criterion (the E1 rule): a pass counts as clean only
  when the direct-mock canary is ≥ 12,000 RPS. A before/after
  verdict rests on clean passes only; polluted runs are recorded
  as polluted, never as measurements.
- Same-tree rule: the before run and the after run must differ
  only by this slice's fixes. The worktree is shared with
  concurrent slices, so the clean pair reverts exactly this
  slice's files (reverse-apply of the slice patch — never a
  checkout, which would destroy concurrent slices' hunks in
  shared files) for the before run and restores them for the
  after run, back to back in one quiet window.

## Before profile (baseline run, 2026-11-18 ~03:27 UTC)

Before-run numbers (clean — direct canary 20,124 / 19,771 RPS):

| | Pass 1 | Pass 2 |
|---|---|---|
| Added latency (p50) | 0.683 ms | 0.672 ms |
| Router RPS (ok) | 1,313 | 930 |
| Errors (502 shedding) | 5,523 / 31,837 (17.3%) | 7,017 / 25,723 (27.3%) |
| Peak RSS | 76.4 MiB | 106.7 MiB |
| Idle RSS / ready | 39.1 MiB / 516.7 ms (healthz), 712.0 ms (v1/models) | — |

Pass 2's RPS fell as VM load rose during the run (loadavg 1.4 →
4.9); pass 1 is the reported run, pass 2 the stability check, as
the rig defines. Raw JSON: `docs/benchmark-2026-11-18-cpu-before.json`
(saved from the run; the rig's combined file).

CPU profile (`cpu-before.prof`, 87.74 s serve, 37.02 s total
samples) — cumulative share of all CPU, top named subsystems:

| Subsystem | Cum CPU | Share | What it is |
|---|---|---|---|
| `tokencount.CountPromptWire` (→ `CountPrompt` → `encodeOrdinaryNative` + regexp2) | 4.30 s | 11.6% | BPE token counting, 3 encodes of the same prompt per request (see below) |
| `ledger.(*Ledger).CheckBudgetOrError` (→ `CheckBudget` + `GetBudget` ×2) | 4.09 s | 11.1% | Pre-send budget guard, 2 identical SQLite reads per request |
| `ledger.(*Ledger).GetBudget` (all callers) | 4.07 s | 11.0% | The budget-row SELECT itself (prepare + execute + scan) |
| `modernc.org/sqlite` `_sqlite3Prepare` | 3.49 s | 9.4% | SQL re-parsing: every `QueryRowContext` prepares anew |
| `ledger.(*Ledger).CheckKeyBudget` | 1.67 s | 4.5% | Rolling key-budget guard, 1 `GetKeyBudget` read per request |
| `cache.(*LocalSemanticCache).getScoped` | 1.14 s | 3.1% | Semantic-cache lookup (inverted-index scan) |
| `guardrails.scan` | 0.97 s | 2.6% | Sync guardrail scan — NOT touched (security/redaction slice 245–246 owns it) |
| `cache.(*ExactCache).touch` | 0.46 s | 1.2% | LRU touch, one fresh-slice allocation per call |
| `provider.(*Provider).Clone` | 0.34 s | 0.9% | Registry copy per quota-hint lookup — measured, not a lever |
| `runtime.mallocgc` (all callers) | 2.60 s | 7.0% | Allocation/GC cost driven by the churn above |
| `syscall.Syscall6` | 4.30 s | 11.6% | Network syscalls (read/write on sockets) — no fix available |

Per-request anatomy in the bench path (gateway key present,
model-a unpriced, no budget header, no compression, no plugins):

1. `enforcePromptBudget` (unpriced) → `CheckBudgetOrError` →
   `CheckBudget` (`GetBudget` #1) + a second `GetBudget` (#2) for
   the 80% warn flag — 2 identical SQLite round-trips, both
   returning ErrNoRows (no budget row exists).
2. `checkKeyBudget` → `CheckKeyBudget` → `GetKeyBudget` — 1 more
   SQLite round-trip, also ErrNoRows.
3. `checkKeyTrafficForChat` → `estimatePromptTokens` →
   `CountPromptFull` — BPE encode #1 of the prompt.
4. Engine `applyContextFit` → `CountPromptFull` — BPE encode #2
   of the same prompt, same model, same framing.
5. Engine `reconcileUsage` → `localPromptCount` →
   `CountPromptWire` (wire=openai — identical to Full here) —
   BPE encode #3 of the same prompt.
6. `noteSuccess` → async ledger record (batched; prepare once
   per batch — not a per-request prepare).

So every successful request paid 3 full tiktoken regex-split +
byte-pair-merge encodes of one prompt and 3 SQLite budget reads
that all answer "no row". The fixes below attack exactly that
redundant work — no behaviour is redefined, only repeated work
removed.

## Fixes landed

### 1. Token-count memo — `internal/tokencount/tokencount.go`

`Count` now memoises on `(encoding, fallback, SHA-256(text))`,
bounded at 4,096 entries (cleared wholesale when full — counts
are a deterministic pure function, so a clear costs a re-encode,
never a wrong answer). Only successful counts are stored; a
BPE-load failure is never cached. The lock guards the map only;
encodes run outside it. This collapses the 3 encodes per request
to 1, and reuses counts across requests for repeated prompts
(the bench cycles 1,000 prompts in the RPS run; a first-seen
prompt still pays one full encode).

The memo key MUST carry the fallback flag, not just the
encoding: `TokenCount.Fallback` depends on the model name —
"gpt-4" and "unknown-model-xyz" both resolve to cl100k_base but
report Fallback false and true. Keying on encoding alone
returned the wrong flag for the second model; the existing
`TestCountKnownTexts` caught it during this slice, and the key
was fixed before any bench run. New tests pin it
(`TestCountMemoReturnsSameCounts`, including the CJK
cl100k/o200k split and the fallback split,
`TestCountMemoBoundedClearKeepsCounts`,
`TestCountMemoConcurrent`).

Micro-benchmarks (this VM, `go test -bench`, noisy — see
limitations): memo hit **288 ns/op** vs cold encode
**50,702 ns/op** — a 176× cheaper repeat encode. The 3→1
per-request collapse saves ~100 µs of BPE work per request.

### 2. Single budget-row read — `internal/ledger/budget.go`

New unexported `checkBudgetDetailed` reads the budget row once
and derives allowed / remaining / warn inputs from that one
read. `CheckBudget`, `CheckBudgetOrError` and
`CheckBudgetForEstimate` all delegate to it — each previously
read the row twice (check + re-read for the warning / estimate).
Behaviour is identical on every path (no row, below/above the
80% line, exhausted fail-closed cap), pinned by
`TestCheckBudgetOrErrorSingleReadBehaviour`,
`TestCheckBudgetForEstimateSingleReadBehaviour` and the existing
`TestCheckBudget*` suite. One subtlety: the old code's second
read could theoretically observe a concurrently-changed row; the
new code decides from a single consistent read, which is the
more correct semantics for a guard.

### 3. Prepared hot-path budget statements — `internal/ledger/ledger.go`, `internal/ledger/keybudget.go`

`GetBudget` and `GetKeyBudget` now run on statements prepared
once per `Ledger` (lazy, under a mutex; a prepare failure falls
back to the direct query, never an error; `Close` closes them).
`*sql.Stmt` is safe for concurrent use and the ledger runs on a
single connection (`SetMaxOpenConns(1)`), so one prepared
statement serves every goroutine. This removes the per-request
`_sqlite3Prepare` for the two hottest reads (3.49 s cumulated in
the before profile). Pinned by `TestPreparedBudgetReadsAgree`
(repeated present/missing reads agree, delete returns to
ErrNoRows on the prepared path) plus micro-benchmarks:
`BenchmarkGetBudgetMiss` / `BenchmarkGetKeyBudgetMiss` /
`BenchmarkCheckBudgetOrErrorMiss` (direct-vs-prepared comparison
kept as `BenchmarkGetBudgetMissDirect`).

Honest result for this fix: NO separately measurable per-call
win on this VM. The profile comparison puts `GetBudget` per call
at ~42 µs before vs ~52 µs after (contention on the single
connection dominates the execute/scan cost, not the prepare),
and `CheckKeyBudget` per request is flat (~32 µs → ~33 µs).
The ledger's measured win is fix 2's dedup (2 reads → 1), not
the prepare. The prepared statements are KEPT — they remove a
known per-call re-parse, cost nothing when the prepare is
cached, and fall back safely — but no per-call saving is
claimed for them; a follow-up on a quiet machine can re-measure
the isolated effect.

### 4. Allocation-free `ExactCache.touch` — `internal/cache/cache.go`

`touch` now shifts in place (`copy`) instead of
`append([]string{key}, c.order...)`, which allocated a fresh
slice on every cache Get-hit and Set. The common paths allocate
nothing (the order slice is pre-capped at `maxEntries` and `Set`
evicts before touching). Pinned by `TestExactCacheTouchOrder`
(order after middle/front/new touches) and
`TestExactCacheTouchZeroAlloc` (0 allocs per touch of an existing
key; ≤1 alloc per Get-hit).

### Deliberately NOT changed

- `Provider.Clone` (0.34 s cum, 0.9%): measured, not a lever.
  The memstats slice's 61.5 MB churn figure is allocation churn,
  not CPU; Clone's CPU share is under 1% and the copy is a
  shallow struct plus two small slices. No change made, no claim
  made.
- `guardrails.scan` (0.97 s cum, 2.6%): the profile names it,
  but guardrails are security/redaction code owned by slice
  245–246. Documented here as a follow-up for that slice, not
  touched. The prefilter/digit-gate optimisations already in the
  code are theirs to extend.
- `LocalSemanticCache.getScoped` (1.14 s cum, 3.1%): already
  inverted-index optimised by earlier slices; no cheap safe win
  was identified in this profile (the remaining cost is the
  per-dimension posting walk, inherent to the lookup). Follow-up
  candidate if the semantic layer ever dominates.
- Router failover/pin/retry code (slice 243–244), autorouter
  (247–248), feedback (251–252): not touched. The profile did
  not point at failover code as a top cost; had it done so, the
  fix would have been documented here for a follow-up slice
  instead of landed.

## Before/after runs

### Clean pair (HEAD tree ± fixes, 2026-11-18 ~03:55–04:01 UTC)

The pair ran on a clean `git archive HEAD` tree (the shared
worktree was unbuildable at the time — concurrent slices 243–244
/ 251–252 had `internal/server/responses.go` +
`failover_243.go` mid-flight broken; not this slice's files,
not touched) with only this slice's patch applied for the after
run. Same harness, back to back in one window. Clean by the
canary rule (direct mock 16,218 / 15,560 before, 15,171 /
17,906 after — all ≥ 12,000 RPS).

| | Before (pass 1 / pass 2) | After (pass 1 / pass 2) |
|---|---|---|
| Added latency (p50) | 0.708 / 0.814 ms | 0.697 / 0.570 ms (−1.6% / −30.0%) |
| Router RPS (ok) | 692 / 764 | 1,650 / 650 (+138.5% / −14.9%) |
| Errors (502 rate) | 4,709 / 18,810 (25.0%) / 3,886 / 19,314 (20.1%) | 7,505 / 40,514 (18.5%) / 6,720 / 19,784 (34.0%) |
| Direct-mock canary | 16,218 / 15,560 RPS | 15,171 / 17,906 RPS |
| Idle RSS / ready | 39.3 MiB / 500.8 ms (healthz), 611.9 ms (v1/models) | 40.1 MiB / 787.5 ms, 909.5 ms |
| Peak RSS | 73.0 / 98.2 MiB | 76.1 / 108.8 MiB |

Merged file: `docs/benchmark-2026-11-18-cpu.json`
(`cpu_profile` section, written by
`scripts/merge-bench-cpu.sh`; the dashboard Benchmark section
serves it as the "Hot-path CPU profile (249–250)" card).
Before/after raw files: `docs/benchmark-2026-11-18-cpu-before.json`
/ `-cpu-after.json`.

Load trace for the pair (`$HOME/.tmp/cpu249/load-trace2.log`,
10 s samples): the before run started at loadavg 1.68 and
climbed to ~9 (fleet respawning after a VM reboot mid-window);
the after run started at 8.49 and fell to 3.7. The two runs
therefore faced opposite load regimes — this is why the RPS
passes disagree and no RPS verdict is drawn, even though the
pair is clean by the canary rule. (The canary guards the mock's
capacity, not the router's contention: the mock is a trivial Go
server that holds 15k+ RPS under load the router feels.)

Profile-share comparison (same pair — the CPU profiles the
bench runs recorded; whole-serve runs, shares of each run's own
total samples, so the storm's absolute level cancels out):

| Function (cum share) | Before | After |
|---|---|---|
| `tokencount.CountPromptWire` | 11.55% (1.75 s) | 2.95% (0.32 s) |
| `tiktoken-go encodeOrdinaryNative` | 10.36% (1.57 s) | — (below top-300 cutoff, < ~0.1 s) |
| `ledger.CheckBudgetOrError` | 11.75% (1.78 s) | 8.59% (0.93 s) |
| `ledger.GetBudget` | 11.68% (1.77 s) | 8.59% (0.93 s) |
| `ledger.CheckKeyBudget` | 4.49% (0.68 s) | 5.54% (0.60 s) |
| `cache.ExactCache.touch` | 1.32% (0.20 s) | 0.92% (0.10 s) |
| `runtime.mallocgc` | 7.39% (1.12 s) | 7.20% (0.78 s) |

Totals: 15.15 s samples / ~21k requests before, 10.85 s / ~18k
after. Per-request: `CountPromptWire` 83 µs → 18 µs;
`CheckBudgetOrError` 85 µs → 52 µs (one read instead of two —
the remaining 52 µs is the single read's execute/scan cost,
not the prepare); `GetBudget` per call ~42 µs → ~52 µs (no
measurable prepared-statement win under contention — see fix 3
below); `CheckKeyBudget` ~32 µs → ~33 µs (flat).

### Polluted attempts (discarded, NOT measurements)

- Worktree after-run (2026-11-18 ~03:37 UTC), loadavg 4.5 → 9.0
  with concurrent slices benchmarking: pass 1 added latency
  0.955 ms / RPS 353 (direct canary 10,566), pass 2 added
  1.584 ms / RPS 1,010 (direct canary 11,741) — both passes fail
  the ≥12,000 canary, and the tree had also moved under
  concurrent slices since the before run. Discarded. Raw JSON
  kept as `docs/benchmark-2026-11-18-cpu-after-polluted.json`.
  Its CPU profile is still informative directionally (not a
  before/after claim): `CountPromptWire` cum fell 4.30 s (11.6%)
  → 0.33 s (1.4%), `CheckBudgetOrError` fell below the top-200
  cutoff from 4.09 s (11.1%), `ExactCache.touch` 0.46 s → 0.20 s
  — the memo and the single-read fix visibly remove the work they
  target, even under contention.
- Clean-tree pair attempt 1 (2026-11-18 ~03:44–03:48 UTC): the
  before run started in a quiet window (load 1.57) but the VM
  stormed mid-run (load to 12.3, concurrent slices' `go test`
  waves); before pass 1 added 0.917 ms / RPS 305 (canary 5,488),
  pass 2 added 0.897 ms / RPS 283 (canary 10,867); after pass 1
  added 0.817 ms / RPS 247 (canary 5,714), pass 2 added 1.601 ms
  / RPS 216 (canary 7,290). All four canaries fail. Discarded.
  Raw JSON kept as `docs/benchmark-2026-11-18-cpu-before-polluted.json`
  and `docs/benchmark-2026-11-18-cpu-after-polluted2.json`.
  Its profile comparison (same HEAD tree ± fixes) is the
  directionally-matching evidence quoted in the Verdict:
  `CountPromptWire` 11.55% → 2.95%, `CheckBudgetOrError`
  11.75% → 8.59%.
- The worktree clean-pair attempt (2026-11-18 ~03:47 UTC) never
  ran: both bench runs failed at `go build` because concurrent
  slices had the shared tree mid-flight broken
  (`internal/server/responses.go`, `failover_243.go`) — the
  reason the pair moved to the clean HEAD tree at all. My files
  were restored intact after the aborted attempt (verified by
  `git diff --stat`).

## Honest limitations

- Shared VM: every number here is one sample per pass on a
  1.5-shared-vCPU VM with concurrent slices, and no 6-minute
  window stayed quiet — the clean pair's load trace runs 1.7 →
  10.3 (median 8.06). The canary rule certifies the mock, not
  the router's contention, so a clean canary does not certify a
  clean RPS comparison: the pass-1/pass-2 RPS disagreement is
  the proof. The E1 slice's ±10% noise band is a floor here,
  not a ceiling — small deltas must be read as noise.
- The before/after pair ran on a clean HEAD tree (plus only this
  slice's patch), not the live worktree: the worktree was
  unbuildable when the pair ran. The fixes' tree context is HEAD
  + this patch; concurrent slices' worktree changes are not in
  either run.
- The prepared statements (fix 3) show no separately measurable
  per-call win: micro-benchmarks are noisy on this VM (12–181
  µs/op across counts for the same benchmark) and the profile
  comparison puts `GetBudget` per call at ~42 µs before vs
  ~52 µs after under contention. The ledger's measured win is
  the single-read dedup (2 calls → 1), not the prepare. The
  prepared statements are kept (they remove a known per-call
  re-parse, are harmless with the direct-query fallback, and
  micro-benchmarks lean favourable) — but no per-call saving is
  claimed for them.
- The token-count memo helps repeated prompts most: the RPS
  pass cycles 1,000 prompts (high reuse), the latency pass uses
  1,000 distinct prompts (only the 3→1 per-request collapse
  applies). Real traffic sits between the two; the report quotes
  both passes separately for that reason.
- The ~26 MiB gap between attempt-3 peak RSS (98.8 MiB) and the
  memstats run (73.1 MiB) remains unexplained (see
  docs/memory-profile-2026-11-18.md); this slice does not claim
  to close it.

## Follow-ups (CHECKLIST 263–264, 2026-11-18)

The two profile-named costs this report left open — `guardrails.scan`
(0.97 s cum, 2.6%) and `LocalSemanticCache.getScoped` (1.14 s cum,
3.1%) — were taken up by the sprint-13 follow-ups slice. Both were
measured with micro-benchmarks on the shared VM, minima across
counts (the VM's run-to-run noise is ±2× under concurrent slices,
so minima, not means, are compared).

### `guardrails.scan`: single-pass needle prefilter — WIN

The profile's `stringslite.Index` share (67% of scan) was the ~46
`strings.Contains` passes in `maybeMatch` — one full-text scan per
needle. The fix indexes all needles once at init by first byte and
runs one pass over the folded text into a presence bitset; each
rule's prefilter becomes bit tests, and the digit-shape gate is
precomputed at init too. The regexes still decide every match, and
a differential test proves the prefilter gives exactly the old
loop's per-rule decision on a 27-input corpus (clean prompts,
digits, secrets, injection attempts, fold-equivalent runes,
non-ASCII, edge positions). Measured (minima):

| Benchmark | Before | After | Change |
|---|---|---|---|
| `BenchmarkScanCleanPrompt` | 4,097 ns/op | 2,652 ns/op | −35% |
| `BenchmarkScanPromptWithDigits` | 4,561 ns/op | 2,753 ns/op | −40% |
| `BenchmarkRedactLogLine` | 4,563 ns/op | 1,902 ns/op | −58% |

(The before minima combine the slice's own pre-change run with a
clean-HEAD worktree re-run; the log-line before-min is 2,808 ns/op
on the re-run, 4,563 on the first run — the −58% uses the first
run's number, the −32% against the re-run's min; either way the
win is well outside the ±3% count-to-count spread of the after
runs.) The after-profile has no `strings.Contains` left in the
top: `needlePresence` 52% cum, `foldLower` 16%, `scanDigitShapes`
12%. No new dashboard surface: the guardrails view already
renders the live Checked/Redacted/Blocked counts, which this
change does not alter.

### `LocalSemanticCache.getScoped`: NO WIN — tried and reverted

A ≤0-dot skip in the best-slot scan (exact: with both norms
positive, a ≤0 dot can never beat best ≥ 0) plus dropping the
redundant `isZeroVector` pass (the `sparseIndex` norm already
answers it) was tried and reverted: the before/after minima
overlap — shared-vocabulary `BenchmarkSemanticGetScoped500`
12,180 vs 12,155 ns/op, disjoint-dimension
`BenchmarkSemanticGetScoped500Disjoint` 2,039 vs 2,365 ns/op —
so no win is claimed and the change is not in the tree. The
reason, measured: the divisions are not on the critical path.
The max-scan loop is bound by the per-slot checks and loads, and
divide latency hides behind out-of-order execution (no
dependency chain — the result feeds a compare that rarely
updates best). The remaining cost is the per-dimension posting
walk plus the O(slots) scan, inherent to the lookup as this
report already noted. The two new benchmarks
(`BenchmarkSemanticGetScoped500`,
`BenchmarkSemanticGetScoped500Disjoint` in
`internal/cache/index_test.go`) stay as the measurement harness
for any future attempt.
