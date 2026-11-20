# M3 Residual Design — Validation Review

- Review date: 2026-11-19 ~14:00 IST. Reviewer: design reviewer (docs-only).
- Design reviewed: `docs/design/m3-residual-design.md` (1,170 lines, written
  2026-11-19 ~13:30 IST against HEAD `035d1ed` + a dirty tree).
- Tree as reviewed: HEAD still `035d1ed`, but the working tree has moved
  since the design was written. Verified by `git status` at review time:
  `internal/router/account_fallback.go`, `internal/router/nadir.go`,
  `internal/router/account_fallback_test.go`, `internal/router/nadir_test.go`,
  `internal/sla/` and `internal/strategy/sla_test.go` are **untracked**
  (`??` — the M3 engine work is built but not committed); `internal/strategy/
  strategy.go`, `internal/strategy/wiring.go`, `internal/router/router.go`,
  `internal/config/config.go`, `internal/config/SCHEMA.md`,
  `cmd/codvian-router/main.go`, `internal/server/server.go`,
  `internal/server/guard.go`, `web/index.html` are **modified, uncommitted**.
  Every `file:line` below is as-read at review time; builders must still
  re-verify line numbers at build time.
- Method: read the design doc fully; re-read/grepped every cited worktree
  file; ran targeted tests with `~/go/bin/go test` (see §6); spot-checked
  9 the reference router reference files in `the local reference clone` (see §4).
- Scope of this review: validation only. No code file, no PARITY/CHECKLIST/
  PLAN edit, no push. One new file: this review.

## Headline: two material corrections, everything else holds

1. **S3's failure cooldown is ALREADY DONE — the design's §S3.2.2 claim
   ("No failure cooldown… no cooldown field or logic") is wrong against
   the current tree.** The cooldown exists, is wired into `nadirSelect`,
   and has a passing test (`TestNadirFailureCooldownSkipsCallsThenResumes`,
   run green in this review). S3 therefore shrinks to **config surface
   only**; its cooldown build step and cooldown DoD test must be struck.
   Whether the cooldown landed after the design was written (~13:30 IST)
   or the design missed it does not change the current-tree verdict.
2. **S2's DoD premise about `Describe("sla-aware")` is wrong.** The design
   says the DoD requires `Describe` to "still disclose" the p50-as-latency
   and neutral-stability limitations. It does not disclose them today:
   `strategy.go:391-392` says only "latency p95 … (default 2000 ms / 5% /
   no cost cap); unknown signals score neutral, never perfect" — no p50
   mention anywhere in `internal/strategy/strategy.go` or
   `internal/sla/sla.go` (grep for `p50` in both files is empty). The
   disclosure lives only in `wiring.go` comments and the `sla.go` package
   doc. The S2 builder must **add** the disclosure to `Describe`, not keep
   one that is not there.
3. **Everything else in the design verified as written.** S1, S2 (except
   the Describe point), S4, D1, D2 gaps are real, with the cited code in
   the cited shape; all four PARITY rows (388/394/396/425) are still
   MISSING in `internal parity tracker` and their Codvian-status cells are now
   factually stale (see §5); the 80.2% gate arithmetic checks out.
   The strategies count is 33 (`internal/strategy/strategy.go`: 33
   `Strategy =` constants in the `:100-240` block), consistent with the
   brief's "pin fixed 31 → 33".

## §1 — Per-slice verdict table

| Slice | Verdict | One-line reason |
|---|---|---|
| S1 — Account fallback config surface (row 388) | **STILL-NEEDED** | Engine built + wired; file-config surface absent everywhere (config, SCHEMA, main). |
| S2 — SLA-aware config + breaker-OPEN filter (row 394) | **STILL-NEEDED** | Scorer + hook built; config surface absent, no OPEN filter, zero-tolerance destroyed at the config boundary. One design premise corrected (Describe disclosure). |
| S3 — Nadir config surface + cooldown (row 396) | **STILL-NEEDED (reduced scope)** | Cooldown **ALREADY-DONE** (design claim stale); config surface still absent — nadir is still dead in production. |
| S4 — Preferred/reserve verification + note (row 425) | **STILL-NEEDED (verification task)** | Mechanism fully built and tested; the verification run + evidence note the slice *is* do not exist yet. No production code to write. |
| D1 — Headroom per-pair limit | **STILL-NEEDED, NO-GO stands** | No limit source exists; utilisation is -1 for every pair. Decision still required. |
| D2 — Expiry-first source (+ measured fraction) | **STILL-NEEDED, NO-GO stands** | No expiry source exists; `ExpiresAt` is zero everywhere. Decision still required. |
| S5 — Per-request preferred-connection list | **NO-GO, unchanged** | No per-request preference input exists in our tree (verified: no `preferredConnectionIds` counterpart in `internal/` or `cmd/`); product decision still required. |
| S6 — Latency histogram (true p95 + stddev) | **NO-GO, unchanged** | No histogram exists; not on the M3 exit-gate path. Sequencing decision still required. |

## §2 — Slice-by-slice evidence

### S1 — Account-level fallback subset (row 388): STILL-NEEDED

Gap is real — the config surface does not exist:

- `grep AccountFallback internal/config/config.go internal/config/SCHEMA.md
  cmd/codvian-router/main.go` returns **nothing**. No
  `AccountFallbackStrategy` / `AccountFallbackStrategies` field in
  `RoutingConfig`, no SCHEMA entry, no `engCfg.*` assignment.
- Engine side confirmed as the design says: `router.Config` carries the
  fields (`internal/router/router.go:550-570`); `orderAttemptsWithContext`
  calls `orderAccountAttempts` at `router.go:2269` (line exact);
  `internal/router/account_fallback.go` defines the 9 values
  (`:26-44`, incl. `weighted`, no `expiry-first`) and
  `ParseAccountFallbackStrategy` rejects `expiry-first`
  (`:118-130`, header `:53-68`); 11 tests in
  `internal/router/account_fallback_test.go`.
- Design claims verified: default `fill-first`, per-provider override
  shape, grouping, skip under explicit order — all as cited. No
  contradiction found.
- `expiry-first` sub-slice: still blocked. No per-key expiry source exists
  anywhere (see D2). The rejection must stay; the design's NO-GO for this
  sub-slice stands.
- Caveat: `internal/router/account_fallback.go` is **untracked** — the
  engine code S1 builds on is not committed. The S1 builder must confirm
  the file is present in its checkout; the coordinator should commit (or
  explicitly account for) the M3 engine files before/after the slices.
- Tests run in this review (all green, `internal/router`):
  `TestParseAccountFallbackStrategy`,
  `TestOrderAccountAttemptsRoundRobinRotates`.

### S2 — SLA-aware / sla (row 394): STILL-NEEDED (one premise corrected)

Gap is real — three missing pieces, all confirmed:

1. **No file-config surface.** `grep` for `SLATarget|SLAMaxError|SLAMaxCost|
   SLAHard|SLAConfig` in `internal/config/config.go` returns nothing;
   no `SLA` in `cmd/codvian-router/main.go` at all; no SLA in SCHEMA.md.
   Production always runs the defaults. (Engine fields exist:
   `router.go:571-580` `SLATargetP95Ms` / `SLAMaxErrorRate` / …)
2. **No breaker-OPEN filter.** The `StrategySLAAware` case in
   `internal/strategy/wiring.go:583+` was read in full: it builds
   `sla.Candidate`s and sorts by `sla.Better` — there is no OPEN filtering
   or demotion anywhere in the case, and `scoring.Signals`
   (`internal/scoring/scoring.go:55-130`) has **no `BreakerOpen` field**
   (fields end at `Attempts`). The design's proposed plumbing
   (`Signals.BreakerOpen` + `signalsFn` fill) is still the right shape.
3. **Zero-tolerance destroyed at the boundary.** `strategyHookContextFor`
   (`internal/router/router.go:2282-2304`) maps
   `SLAMaxErrorRate <= 0` to `0.05` (`:2301-2302`); the pure scorer
   supports zero-tolerance (`internal/sla/sla_test.go:38`
   `TestScoreZeroTolerance`, green). The design's pointer/nil-vs-0 fix is
   still required.
- p95/stddev confirmed absent: `wiring.go:280-287` feeds `LatencyP95Ms:
  -1` / `LatencyStdDevMs: -1` with the comment "true p95 is Phase C";
  `internal/sla/sla.go:150` hard-codes `stabilityScore = 0.5`.
- Scorer + hook confirmed built: `internal/sla/sla.go` (weights
  0.35/0.35/0.15/0.10/0.05, `:194`), `strategy.go:170-173,219,231`
  (`StrategySLAAware`, alias `"sla"`), `wiring.go:583+` case,
  `internal/strategy/sla_test.go` (4 SLA tests + `TestNadirHookIsIdentity`
  at `:73`, exactly as cited).
- **Correction — STALE premise in the design (§S2.4 DoD):** the design says
  "`Describe("sla-aware")` still discloses the p50-as-latency and
  neutral-stability limitations." It does not. `strategy.go:391-392`:
  "SLA-aware", "Most SLA-compliant first — latency p95, error rate and
  cost scored against the operator's objectives (default 2000 ms / 5% /
  no cost cap); unknown signals score neutral, never perfect." No p50
  disclosure; grep `p50` in `strategy.go` and `sla.go` is empty. The S2
  builder must **add** the p50/neutral-stability disclosure to `Describe`
  (the design's DoD intent is right; its "still" is wrong).
- Supporting evidence, new since the design: `TestDispatchSLAAwareHonours
  ConfiguredPolicy` exists (`internal/router/nadir_test.go:277`) — the
  engine-level configured policy path already works; only the *file-config*
  surface is missing. This strengthens the GO: the slice is pure plumbing.
- Tests run in this review (all green): `internal/sla`
  (`TestScoreCompliantBeatsViolating`, `TestScoreUnknownIsNeutral`,
  `TestScoreZeroTolerance`, `TestScoreCostCap`, `TestPolicyResolveDefaults`)
  and `internal/strategy` (`TestSLAAware*`).

### S3 — Nadir (row 396): STILL-NEEDED, scope reduced — cooldown ALREADY-DONE

The design's §S3.2.2 is **STALE/WRONG** against the current tree:

- Design claim (quoted): "**No failure cooldown.** the reference router skips the
  network for 30 s after a failure (`nadirStrategy.ts:61,201`). Ours has
  no cooldown field or logic: a down endpoint costs a full timeout … on
  EVERY `auto/nadir` request until it recovers."
- Contradicting evidence (current tree):
  - `internal/router/nadir.go:33-35` (file header): "After such a failure
    the endpoint is cooled down for 30 s (the reference router's
    `NADIR_FAILURE_COOLDOWN_MS`, same number, own code)".
  - `nadir.go:75-79`: `NadirFailureCooldownMs = 30000`.
  - `nadir.go:82-93`: `nadirFailureCooldown` var (test-shrinkable) +
    `nadirCooldownKey` (per-endpoint, held in the engine's existing
    hard-cooldown map, no new state — exactly the shape the design's §S3.3
    recommends).
  - `nadir.go` `nadirSelect`: `if e.onCooldown(key)` returns the fallback
    with a cooldown note *before* dialling; the `fail` closure arms
    `e.setCooldown(key, nadirFailureCooldown)` on transport error,
    non-2xx, malformed response, and out-of-pool selection.
  - Test: `TestNadirFailureCooldownSkipsCallsThenResumes`
    (`internal/router/nadir_test.go:231`) — **run green in this review**.
- Consequence for the slice: strike design §S3.4 item 4 (add the cooldown)
  and the cooldown half of the DoD. What remains — and is still
  STILL-NEEDED — is the **config surface only**:
  - `grep -i nadir internal/config/config.go internal/config/SCHEMA.md
    cmd/codvian-router/main.go` returns **nothing**. `router.Config`
    fields exist (`router.go:583-599`: `NadirEndpoint` / `NadirAPIKey` /
    `NadirTimeoutMs`) but nothing ever sets them, so in production nadir
    is still permanently unconfigured and every `auto/nadir` request takes
    the fail-closed fallback. The design's "dead code" headline still holds
    for the config half.
  - Engine/dispatch confirmed built: `nadir.go` (normalisation, 16,000-char
    cap, 100-item menu, 2,000/30,000 ms timeouts, injectable transport),
    `autodispatch.go:192-199` (`auto/nadir` via `nadirSelect`; design cited
    `:171-184` — line drift, substance identical), `StrategyNadir`
    identity hook + `TestNadirHookIsIdentity` (`sla_test.go:73`).
  - Tests run in this review (all green): `TestNadirUnconfiguredMakesZero
    ExternalCalls`, `TestNadirConfiguredSelectsModel`,
    `TestNadirFailureCooldownSkipsCallsThenResumes`.
- Caveat: `internal/router/nadir.go` is **untracked** (same uncommitted-
  state note as S1).
- The design's S3 divergences (fail-closed, no default endpoint, no env
  fallback for the endpoint, priority fallback) are all still correct and
  still to be preserved by the reduced slice. The cooldown implementation
  matches the design's §S3.5 preference (fixed 30 s, no config knob).

### S4 — Preferred / reserve accounts for auto combos (row 425): STILL-NEEDED (verification task)

The mechanism is fully built; the slice *is* verification + a note, and
that work has not been done yet:

- Confirmed built (all as the design cites, modulo line drift):
  - Key roles: `internal/provider/keys.go:30-35`
    (`KeyRolePreferred`/`KeyRoleReserve`), `EffectiveRole`/`IsReserve`
    (`:53-56`).
  - Chain construction: `attemptsForProvider` closure in
    `cmd/codvian-router/main.go:2185` (design cited `:2180-2210`; comment
    at `:2177`, `Reserve: k.Role == provider.KeyRoleReserve` at `:2195`).
  - Reserve-last (flat path): `applyTailPartitions`
    (`internal/router/router.go:2117/2125`; design cited `:2148-2170` —
    drift, substance identical).
  - Reserve-last (auto-combo path): `orderPreferredReserve`
    (`internal/router/account_fallback.go:163-180`, comment `:163`, func
    `:169`), applied in `autodispatch.go:332/334` (design cited
    `:303-307` — drift).
  - Response-start failover: config surface **exists**
    (`internal/config/config.go:1006-1011`,
    `cmd/codvian-router/main.go:2738-2739`), arming site
    (`router.go:5403-5406`, line exact).
  - Tests: `internal/router/keypool_test.go` (10 tests, incl. preferred
    401/429 failover, reserve-only-when-exhausted, per-key ledger
    attribution, streaming pre-commit 401 failover);
    `TestPreferredFailover*` in `internal/router/m4_8_test.go:416-557`;
    `TestChainForDispatchPreferredReserve`
    (`account_fallback_test.go:201`). Key tests run green in this review
    (`TestKeyPoolPreferred401FailsOverToSecondKey`,
    `TestChainForDispatchPreferredReserve`).
- The slice's deliverable does **not** exist yet: `docs/design/` contains
  only `m3-residual-design.md` — no S4 evidence note. The verification
  commands + live smoke in design §S4.4 have not been run/recorded on the
  current tree. Hence STILL-NEEDED as a task, ALREADY-DONE as a mechanism.
- Dashboard follow-up resolved in S4's favour: `web/index.html` **does**
  show key roles — role pill (`:2910`), reserve status chip
  (`:2902-2903`), Set-preferred toggle (`:2914-2915`). No M10 follow-up
  is needed on this point.
- Per-request preference list: genuinely absent (no
  `preferredConnectionIds` counterpart in `internal/` or `cmd/`,
  non-test code). The design's NO-GO for S5 stands.
- PARITY inconsistency to flag to the coordinator: row 425 still reads
  "No preferred/reserve account concept | MISSING" (`internal parity tracker:425`)
  while row 487 records the same concept as FULL
  (`internal parity tracker:487`, "preferred first, reserve sorted last, never
  hedge targets"). The S4 flip should also reconcile row 425's status cell
  with row 487; the design's flip instruction does not mention this.

### D1 — Headroom per-pair limit: STILL-NEEDED, NO-GO stands

- Gap confirmed: no `PairLimits`/`PairLimit` anywhere in `internal/config/
  config.go`, `internal/ledger/quotaquery.go`, `internal/scoring/
  scoring.go`, or `cmd/codvian-router/main.go` (grep empty).
- `PairQuota` always reports unknown utilisation:
  `internal/ledger/quotaquery.go:116-127` (`Util5h`/`Util7d` =
  `UnknownUtilisation` (-1), `:58`); `signalsFn`
  (`cmd/codvian-router/main.go:2037`, `:2057-2058`) passes the -1 through
  with the comment "the system holds no per-pair limit to divide the spend
  by … so utilisation is unknown, never invented."
- Pure ordering confirmed built: `Headroom` / `OrderHeadroom`
  (`internal/strategy/quota.go:178/186`); `TestHeadroomOrder` run green.
- No decision has been recorded anywhere in the tree: no operator-declared
  limits, no fetcher registry, no divergence record. The design's NO-GO
  (Raunak must choose option 3 vs option 4 vs recorded divergence) stands
  unchanged. The design's recommendation (option 3 first) is unaffected.

### D2 — Expiry-first source (+ measured fraction): STILL-NEEDED, NO-GO stands

- Gap confirmed: no `PairExpiry` anywhere (grep empty); `ExpiresAt` stays
  zero in `signalsFn` (`cmd/codvian-router/main.go:2061`, comment "no
  quota-expiry data exists anywhere"); `scoring.Signals.ExpiresAt`
  (`internal/scoring/scoring.go:105-109`) documents "Zero means unknown:
  the system records no quota-expiry data anywhere".
- Pure ordering confirmed built: `ExpiryFirstScore` / `OrderExpiryFirst`
  (`internal/strategy/quota.go:431/454`; constants
  `QuotaExpiryMinHours = 0.25`, `QuotaExhaustedFloor = 0.01`, `:124-129`);
  `TestExpiryFirstOrder` run green.
- Usable-fraction half confirmed still booleanised: `toQuotaSnapshots`
  (`internal/strategy/wiring.go:201-203,215`) sets `QuotaRemaining = 1`
  iff `QuotaHint > 0`, else 0 — the design's "step function" warning still
  applies.
- Trap confirmed live: `internal/config/gatewaykey.go:72` has an
  `ExpiresAt` on our gateway key — the design's warning not to reuse it
  as upstream quota expiry still stands.
- No decision recorded: no operator-declared expiry, no fetchers, no
  longer-window-rule adoption. NO-GO stands unchanged. The account-level
  `expiry-first` value stays rejected until the per-KEY variant (option 3)
  lands — that dependency is unchanged.

### S5 / S6 — follow-ups: NO-GO, unchanged, no new evidence

- S5 (per-request preferred-connection list): no per-request preference
  input exists in our tree (see S4). Still needs an auth/settings or
  contract-schema design + product decision.
- S6 (latency histogram: true p95 + stddev): no histogram exists
  (`wiring.go:280-287` still feeds -1 for p95/stddev). Still a sequencing
  decision, still not on the M3 exit-gate path.

## §3 — Re-graded go / no-go list

| # | Slice | Design's call | Re-graded call | What changed |
|---|---|---|---|---|
| S4 | Preferred/reserve verification + note | GO — build first | **GO — build first (unchanged)** | Nothing. Zero code risk; also resolves the row 425 vs 487 PARITY inconsistency. |
| S1 | Account fallback config surface | GO | **GO (unchanged)** | Nothing. 9 values fixed, `expiry-first` stays rejected. |
| S2 | SLA-aware config + OPEN filter | GO | **GO (unchanged, scope +1 line)** | Builder must ADD the p50/neutral-stability disclosure to `Describe("sla-aware")` (design wrongly assumed it was already there). |
| S3 | Nadir config surface + cooldown | GO | **GO (reduced scope)** | **Cooldown struck — already done** (see §2 S3). Slice is now config surface + configured-path engine test only. |
| D1 | Headroom per-pair limit | NO-GO — needs Raunak's decision | **NO-GO (unchanged)** | No decision recorded; no limit source landed. |
| D2 | Expiry-first source | NO-GO — needs Raunak's decision | **NO-GO (unchanged)** | No decision recorded; no expiry source landed. |
| S5 | Per-request preferred list | NO-GO — product decision | **NO-GO (unchanged)** | — |
| S6 | Latency histogram | NO-GO — sequencing decision | **NO-GO (unchanged)** | — |

## §4 — the reference router citation spot-check (9 files)

Every citation below was read in `the local reference clone` at
review time. Verdicts: CONFIRMED means the cited file:lines say what the
design claims; NOT-RECHECKED means the substance was verified nearby but
the exact lines were not re-read — the design's citation is plausible and
uncontradicted.

| # | Design citation | What the reference actually says | Verdict |
|---|---|---|---|
| 1 | `routingStrategies.ts:56-65` (9 account values, no `weighted`); `:40-51` (auto values incl. `sla-aware`/`sla`) | `ACCOUNT_FALLBACK_STRATEGY_VALUES` = priority, fill-first, round-robin, p2c, random, least-used, cost-optimized, expiry-first, strict-random (9, no weighted); `AUTO_ROUTING_STRATEGY_VALUES` = rules, score, cost, eco, latency, fast, sla-aware, sla, lkgp, nadir | CONFIRMED |
| 2 | `auth.ts:1924,1930` (per-provider override, default `fill-first`); `:~1937` (`preferredConnectionIds`) | `providerOverride.fallbackStrategy \|\| settings.fallbackStrategy \|\| "fill-first"`; `options.preferredConnectionIds` → `pickPreferredConnection(...)` in the same function | CONFIRMED |
| 3 | `routerStrategy.ts:286-294,394-403` (SLA impl + registry); `:231-236,300-306` (defaults 2000/0.05); `:294-295` (OPEN pre-filter); `:262-266` (health); `:313-330` (weights) | `DEFAULT_SLA_TARGET_P95_MS = 2_000`, `DEFAULT_SLA_MAX_ERROR_RATE = 0.05`; `HALF_OPEN → 0.5` health; violation `OPEN ? 1 : 0` + overruns; `healthy = pool.filter(c => c.circuitBreakerState !== "OPEN"); candidates = healthy.length > 0 ? healthy : pool`; registry sets `"sla-aware"` and `"sla"` to the same impl | CONFIRMED (substance; exact line numbers drifted a few lines, e.g. the pre-filter sits at the `select` head, not literally `:294-295` in this clone) |
| 4 | `nadirStrategy.ts:2-16,153-177` (impl); `:58` (default base URL); `:59-60` (timeout 2000/max 30000); `:61` (30 s cooldown); `:25-38,183-185,216` (config + env fallbacks) | Header `:16-23` states opt-in, fail-open, bounded + cooldown, minimal egress; `NADIR_DEFAULT_BASE_URL = "https://api.getnadir.com"`; `NADIR_DEFAULT_TIMEOUT_MS = 2000`, `NADIR_MAX_TIMEOUT_MS = 30_000`; `NADIR_FAILURE_COOLDOWN_MS = 30_000`; per-base-URL cooldown map `:161`, check `:190`, arm `:198`; env fallbacks `NADIR_API_KEY` / `NADIR_BASE_URL`; menu cap 100, prompt cap 16,000 | CONFIRMED |
| 5 | `headroomRanking.ts:54` (`computeHeadroom`) | `computeHeadroom = 1 − max(util5h, util7d)`; `rankByHeadroom` most-free-first, missing entry → full headroom (fail-open), stable ties | CONFIRMED |
| 6 | `quotaStrategies.ts:721-800` (expiry-first orderer); `:824-900` (headroom orderer); `:872-883` (credential threading, #6379) | `orderTargetsByExpiryFirst` (~`:721`, domain-quota-cache only, no network); `orderTargetsByHeadroom` (~`:824`, bounded-concurrency saturation fetch, fail-open to 0, order unchanged on failure) | CONFIRMED (orderers; the `:872-883` threading comment was NOT re-read line-exact) |
| 7 | `quotaScoring.ts:542-640` (expiry-first score); `:413-430` (defaults minHours 0.25, exhausted floor 1%, tie band 5%) | `EXPIRY_FIRST_DEFAULTS` + `resolveExpiryFirstConfig` with `minHours`/`tieBand`/`exhaustedFloor: 1%`; score doc: "usable / hoursUntilDeadline", burn projection, and the longer-window deadline rule verbatim — "Taking the session reset as the deadline made two subscription accounts trade places every few hours and ignore weekly resets two days apart" | CONFIRMED |
| 8 | `saturationSignals.ts:543` (`getSaturation`); `:52-60` (30 s TTL, bounded 4096) | `getSaturation` "Always fail-open: returns 0 on any error" (~`:543`); `_cache = boundedMap("saturation-cache", 4096, "ttl", CACHE_TTL_MS)` (`:50`) | CONFIRMED |
| 9 | `preferredConnectionFailover.ts` (whole file; default 20,000 ms, min 500, max 120,000, max 64 overrides) | `defaultTimeoutMs: 20_000` (`:15`), `MAX_MS = 120_000` (`:21`); 174 lines total | CONFIRMED (20 s default + 120 s max; the 500 ms min / 64-override cap were NOT re-read line-exact) |

Not re-checked line-exact (noted honestly, none contradicted): design's
`routerStrategy.ts:247-260` (unknown-scores-perfect via
`scoreAtOrBelowThreshold` / `inverseNormalized`) — the surrounding code
read is consistent (0.001 clamps for maxCost/maxStdDev) but those exact
lines were not re-read; design's `quotaStrategies.ts:872-883` threading
comment; the failover file's min-500 ms / max-64-overrides lines.
The design's own honest-gap note (§S4.1: no direct the reference router file:line for
a "reserve account" in auto combos) is confirmed — no such citation was
found in this review either; row 425's citation still rests on the
deep-study summary + the failover file.

## §5 — PARITY / gate arithmetic re-check

- Area 1 header confirmed: `internal parity tracker:361` — 63 rows, FULL 34 /
  PARTIAL 25 / MISSING 4 / WRONG 0; strict 54.0%, weighted 73.8%.
- All four rows still MISSING, and all four Codvian-status cells are now
  factually stale (the design itself says so; this review confirms it
  against the code):
  - Row 388 (`parity tracker:388`): "No per-account fallback-strategy
    concept" — wrong; 9 values built + wired (`router.go:2269`).
    Stale citation `router.go:1203,1211` points at nothing relevant.
  - Row 394 (`parity tracker:394`): "No SLA concept anywhere" — wrong;
    `internal/sla` scorer + `sla-aware` hook built + wired.
  - Row 396 (`parity tracker:396`): "No external decision API" — wrong;
    `nadirSelect` + `auto/nadir` dispatch built + tested (config surface
    is the missing half, not the API).
  - Row 425 (`parity tracker:425`): "No preferred/reserve account concept" —
    wrong; roles + reserve-last + failover built, and row 487 already
    records the same concept FULL.
- Gate arithmetic confirmed: (34 + 12.5)/63 = 73.8% today;
  flipping only the four rows → (38 + 12.5)/63 = 80.16% ≈ **80.2%**;
  D1+D2 as well → (40 + 11.5)/63 = 81.75% ≈ **81.7%**. No slack, as the
  design says. The S3 scope reduction does **not** change the arithmetic:
  the row-396 flip still requires the config surface (the cooldown alone
  does not make nadir operator-reachable), so all four flips are still
  needed for 80.2%.

## §6 — Tests run in this review (evidence, not claims)

`~/go/bin/go test` on the current (dirty + untracked) tree, `-count=1`:

- `go test ./internal/router/ -run 'TestNadirFailureCooldownSkipsCallsThen
  Resumes|TestNadirUnconfiguredMakesZeroExternalCalls|TestNadirConfigured
  SelectsModel|TestParseAccountFallbackStrategy|TestOrderAccountAttempts
  RoundRobinRotates|TestKeyPoolPreferred401FailsOverToSecondKey|
  TestChainForDispatchPreferredReserve'` → **ok** (2.216 s).
- `go test ./internal/sla/... ./internal/strategy/ -run 'TestSLAAware|
  TestScore|TestHeadroomOrder|TestExpiryFirstOrder'` → **ok**
  (`internal/sla` 0.015 s, `internal/strategy` 0.208 s).
- These runs prove the cited tests exist and pass on the current tree.
  They do not prove the slices' DoDs (config-file end-to-end proofs are
  still to be built by the slice builders).

## §7 — Ordered builder-ready slice list

Disjointness check against the named in-flight work (verified by
`git status` + file-set comparison):

- **W3-B / `internal/server/a2a.go`** (untracked, in-flight): no slice
  touches `internal/server/a2a*.go`. Disjoint ✓.
- **`internal/server/server.go` / `guard.go` registrations** (both dirty):
  no slice touches `internal/server/*`. Disjoint ✓.
- **`web/index.html` M10 regions** (dirty): no slice touches `web/*`.
  Disjoint ✓.

All four GO slices are therefore **builder-ready with respect to the
named in-flight work**. Two sequencing constraints remain (the design's
§7 constraint, still true, plus the dirty-tree hazard):

1. **S1, S2, S3 are NOT disjoint from each other**: all three touch
   `internal/config/config.go`, `internal/config/SCHEMA.md`, and the
   `engCfg` assignments in `cmd/codvian-router/main.go` (all three files
   currently dirty with uncommitted M9 edits). Build sequentially in the
   order below, or on disjoint branches merged in order. S4 touches no
   production code and can run in parallel with any of them.
2. **Dirty-tree hazard**: the engine files S1/S3 build on
   (`account_fallback.go`, `nadir.go`) are untracked, and `router.go`,
   `wiring.go`, `config.go`, `main.go` carry uncommitted M9 edits. Every
   slice must re-verify its `file:line` citations and rebase onto current
   HEAD before testing (design §8.4, still true).

Ordered build sequence (only STILL-NEEDED + GO slices):

| # | Slice | Files (as designed, re-verify at build time) | Notes |
|---|---|---|---|
| 1 | **S4** — Preferred/reserve verification + evidence note | No production code. New evidence note under `docs/design/` | GO, build first. Run the §S4.4 verification + live smoke; write the note; reconcile PARITY row 425 with row 487 (already FULL). Dashboard role display already exists — no M10 follow-up. |
| 2 | **S1** — Account fallback config surface | `internal/config/config.go`, `internal/config/SCHEMA.md`, `cmd/codvian-router/main.go` (+ tests) | GO. Global + per-provider, 9 values, `expiry-first` stays rejected. Import-cycle note in the design still applies (config must not import router). |
| 3 | **S2** — SLA-aware config surface + breaker-OPEN filter | `internal/scoring/scoring.go` (`BreakerOpen`), `cmd/codvian-router/main.go` (`signalsFn` + `engCfg`), `internal/strategy/wiring.go` (OPEN filter), `internal/config/config.go` + SCHEMA.md, `internal/router/router.go` (`strategyHookContextFor` nil-vs-0), `internal/strategy/strategy.go` (`Describe` disclosure — **added**, see §2 S2) | GO. p95/stddev explicitly deferred, documented. |
| 4 | **S3 (reduced)** — Nadir config surface only | `internal/config/config.go` (`Nadir` block: endpoint + timeout_ms, no API-key field), `internal/config/SCHEMA.md` (incl. egress warning), `cmd/codvian-router/main.go` (`engCfg.NadirEndpoint`/`NadirTimeoutMs` + `CODVIAN_ROUTER_NADIR_API_KEY` env) (+ tests: config round-trip + configured-path engine test) | GO. **Cooldown step struck** — already done (`nadir.go`, `TestNadirFailureCooldownSkipsCallsThenResumes`). Until this lands, row 396 must NOT flip. |

Not builder-ready: **D1, D2** (NO-GO — awaiting Raunak's decision on the
data source); **S5, S6** (NO-GO — awaiting product / sequencing decisions).
D1 must precede D2 if both are later approved (D2's measured remaining
fraction comes from D1's limit), per the design's §7 constraint.

## §8 — Line-number drift register (non-material, for builders)

Substance confirmed, line numbers moved since the design was written.
Builders must use the review-time lines below, re-verified at build time:

- `autodispatch.go` nadir dispatch: design `:171-184` → now `:192-199`.
- `applyTailPartitions`: design `router.go:2148-2170` → now `:2117-2125`.
- `orderPreferredReserve` application: design `autodispatch.go:303-307` →
  now `:332/334`.
- `strategyHookContextFor`: design `router.go:2294-2320` → now `:2282-2304`
  (mapping at `:2294-2304`).
- `routerStrategy.ts` SLA pre-filter: design `:294-295` → a few lines off
  in this reference clone (same `select` head).
- `parity tracker:388` stale citation `router.go:1203,1211` → live call site
  `router.go:2269`.
- Confirmed line-exact (no drift): `router.go:2269`
  (`orderAccountAttempts`), `router.go:550-570` (account-fallback Config),
  `router.go:5403-5406` (preferred-failover arming),
  `main.go:2738-2739` (preferred-failover wiring),
  `sla_test.go:73` (`TestNadirHookIsIdentity`), `config.go:1006-1011`
  (preferred-failover config), `nadir_test.go:231` (cooldown test).

## Counts

- Slices reviewed: **8** (S1, S2, S3, S4, D1, D2, S5, S6).
- Still-needed: **6** (S1, S2, S3-config-only, S4-verification, D1, D2).
- Already-done (whole slice): **0** — no slice is fully done; **1
  sub-slice already done** (S3 failure cooldown, incl. its test).
- Stale/wrong design claims: **2 material** (S3 "no failure cooldown";
  S2 "`Describe` still discloses p50/neutral-stability") + line-number
  drift in ~6 citations (non-material, §8).
- the reference router citations spot-checked: **9 files, all confirmed** (3 with
  minor exact-line caveats noted in §4).
- Go/no-go re-graded: S3 scope reduced (cooldown struck); all other calls
  unchanged — GO: S4, S1, S2, S3-reduced; NO-GO: D1, D2, S5, S6.
- Builder-ready slices (STILL-NEEDED + GO, disjoint from named in-flight
  work): **4** (S4, S1, S2, S3-reduced), in that order, sequential except
  S4 which is parallel-safe.
