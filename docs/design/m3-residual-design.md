# M3 Residual Design — Area 1: the 4 MISSING rows + headroom / expiry-first data sources

- Status: DESIGN ONLY. No code was written for this document; no code file,
  `CHECKLIST.md`, `parity tracker`, or `PLAN.md` was modified.
- Date / as-read: 2026-11-19 ~13:30 IST. Worktree `~/workspace/codvian-router/`,
  HEAD `035d1ed` (M9 merge wave). The worktree is DIRTY at the time of writing
  (concurrent M9 builders have uncommitted changes, including in
  `internal/router/router.go` and `internal/router/autodispatch.go`), so every
  `file:line` below is as-read at that time. Builders MUST re-verify line
  numbers at build time; file names and symbol names are the stable part.
- Scope: parity tracker Area 1 (Strategies / combos / auto scoring), 63 rows,
  FULL 34 / PARTIAL 25 / MISSING 4 / WRONG 0; strict 54.0%, weighted 73.8%
  (`internal parity tracker:361`). The M3 exit gate is Area 1 ≥ 80% weighted
  (`internal plan:184-191`).
- the reference router reference: `the local reference clone`, study ONLY. No code
  copied, no bug copied. Every the reference router claim below cites a reference
  `file:line`; every Codvian claim cites a worktree `file:line`.

## The headline finding (read this before building anything)

**All four MISSING rows are already built in code. parity tracker is stale, not the
code.** What is actually missing is smaller, and different from what the
PARITY table says:

| PARITY row | Code state (verified 2026-11-19) | What is really missing |
|---|---|---|
| 388 account-level fallback subset | Built: 9 values, wired in `orderAttempts` | File-config surface (global + per-provider override) is never plumbed into `router.Config`; production always runs the `fill-first` default. 10th value (`expiry-first`) genuinely absent — blocked on a per-key expiry source. |
| 394 sla-aware | Built: `internal/sla` scorer + `sla-aware`/`sla` hook, wired per-request | File-config surface for the SLO policy is missing; production always runs the defaults. Breaker-OPEN candidates are not filtered/demoted by the hook. True p95 and latency-stddev signals do not exist yet. |
| 396 nadir | Built: `nadirSelect` + `auto/nadir` dispatch, fail-closed, tested | File-config surface is missing AND the endpoint is never set, so in production nadir is **dead code**: every `auto/nadir` request takes the unconfigured fallback. No failure cooldown (the reference router has one). |
| 425 preferred / reserve accounts | Built end-to-end: key roles, reserve-last partition, auto-combo path, opt-in response-start failover | Almost nothing for the row as written. The only genuine gap is the reference router's *per-request* preferred-connection list (`preferredConnectionIds`), which is a different concept from our static key role — that needs a product decision, not a build. |

**Arithmetic that matters.** Weighted parity = (FULL + 0.5 × PARTIAL) / 63.
Today: (34 + 12.5) / 63 = 73.8%. Flipping only the four rows MISSING → FULL
gives (38 + 12.5) / 63 = **80.2% — the exit gate, by 0.2 points**. Flipping
headroom + expiry-first PARTIAL → FULL as well gives (40 + 11.5) / 63 =
**81.7%**. There is no slack: a builder must not claim any of these flips
without the evidence the slice's Definition-of-Done demands, and the
coordinator (not the builder) flips PARITY rows after reviewing that evidence.

**The single biggest design risk** (also §8): flipping PARITY to FULL on code
no operator can switch on. Nadir is the sharpest case — fully built, fully
tested, and unreachable in production because `NadirEndpoint` is never set
(`cmd/codvian-router/main.go` builds `router.Config` at `:2437` and never
assigns `NadirEndpoint`, `NadirAPIKey`, `NadirTimeoutMs`, the SLA fields, or
the account-fallback fields). A FULL verdict on an unconfigured feature
certifies a dead path. Slices S1–S3 (config surfaces) are therefore
prerequisites for the flips, not polish after them.

**Nadir / account-deck landing state.** The task brief warned builders might
be concurrently landing nadir/account-deck work in `router.go`. Verified:
that work is FULLY LANDED, not half-landed. `internal/router/nadir.go`
(231 lines, `nadirSelect`, injectable transport) and
`internal/router/account_fallback.go` (435 lines, all 9 values,
`orderAccountAttempts`, `orderPreferredReserve`) exist with tests, and
`internal/router/router.go:2269` calls `orderAccountAttempts` inside
`orderAttemptsWithContext`. The stale PARITY citation
`internal/router/router.go:1203,1211` (row 388) points at nothing relevant
anymore; the live call site is `router.go:2269`.

**How to read the slices.** Each section follows the required five parts:
(1) what the reference router does, (2) what we have today, (3) the exact data source,
(4) the concrete builder slice, (5) deliberate divergence. §7 gives the
ordered build sequence and a go/no-go per slice. "GO" means: build now, no
prior decision needed. "NO-GO" means: do not build until the named decision
is made — the slice section says exactly what decision and who must make it
(the operator/product call is Raunak's; a builder must not invent it).

---

## S1 — Account-level fallback subset (PARITY row 388)

### (1) What the reference router actually does

- The value set is `ACCOUNT_FALLBACK_STRATEGY_VALUES`:
  `priority, fill-first, round-robin, p2c, random, least-used,
  cost-optimized, expiry-first, strict-random` — 9 values
  (`src/shared/constants/routingStrategies.ts:56-65`).
- It is a **per-provider setting**: `settings.fallbackStrategy`, overridable
  per provider via `providerStrategies[provider].fallbackStrategy`, default
  `"fill-first"` (`src/sse/services/auth.ts:1924,1930`).
- Semantics (from the value names and the auth call site): it orders the
  ACCOUNTS (connections / keys) of **one provider** when a request falls back
  between them. It is orthogonal to the chain strategy, which orders
  providers/models. `fill-first` drains the first account before the next;
  `expiry-first` orders accounts by soonest-expiring quota (see §D2 for its
  data source, `open-sse/services/combo/quotaStrategies.ts:721-800`).
- Note the value set has NO `weighted`. the reference router's account level cannot do a
  weighted draw; its weighted draw lives at chain level only.

### (2) What our code has today

- `internal/router/account_fallback.go` (whole file) implements the concept:
  9 values (`AccountFallback*`, `:69-110`) — the same 9 names EXCEPT
  `expiry-first` is replaced by `weighted` (see divergence below).
  Global `Config.AccountFallbackStrategy` + per-provider
  `Config.AccountFallbackStrategies`, default `fill-first`
  (`internal/router/router.go:550-570`); resolution in
  `accountFallbackStrategyFor` (`account_fallback.go:133-143`).
- Wired in production order: `orderAttemptsWithContext` calls
  `orderAccountAttempts` after the chain strategy hook and before the region
  tiers, skipped under an explicit client order (`preserveOrder`)
  (`internal/router/router.go:2269`; comment block just above it).
  Within each `(provider, model)` group only the accounts move; cross-provider
  order is untouched (`account_fallback.go:181-227`).
- Per-value mechanics: `priority` / `fill-first` / `cost-optimized` are
  declared order (exhausted accounts already filtered upstream by
  `filterZeroQuota`, `router.go:2106-2115`; same-provider accounts share one
  pair price, so cost-optimized ties); `round-robin` per-group rotation,
  `strict-random` per-group deck, `random` Fisher-Yates, `least-used` from the
  live per-`(provider, key)` attempt counter (`e.keyAttempts`),
  `p2c` / `weighted` draws (`account_fallback.go:229-435`).
- `expiry-first` is deliberately REJECTED by the parser
  (`ParseAccountFallbackStrategy`, `account_fallback.go:118-130`, file header
  `:54-68`): no per-key quota-expiry source exists, so accepting the name
  would fake an ordering. The engine falls back to `fill-first` for it.
- Tests exist: `internal/router/account_fallback_test.go`
  (`TestParseAccountFallbackStrategy`, `TestAccountFallbackStrategyFor`,
  `TestOrderAccountAttempts*`, `TestOrderAttemptsAppliesAccountFallbackThenReserveLast`,
  `TestOrderAttemptsPreserveOrderSkipsAccountFallback`).

### (3) The exact data source to add

- **For the 9 built values: no new measured signal.** The only missing piece
  is *configuration*, not telemetry:
  - Global + per-provider strategy names, from the config file (see slice).
  - `least-used` already reads the measured per-key attempt counter
    (`e.keyAttempts`, real attempts only; unmeasured counts as 0).
  - `p2c` scores accounts with the **provider-level** composite
    (`accountP2CScore`, `account_fallback.go:~330-350`: success rate +
    `1/log10(latency+10)`, breaker OPEN scores −Inf). Both accounts of one
    provider share that score, so the score almost never decides — the draw
    does. That is honest and documented in the file, but it means p2c at
    account level is currently "random draw with a breaker guard", not a
    scored choice. A real per-key score needs per-key latency/success
    signals, which do not exist anywhere in the tree. Do NOT invent them.
    Honest fallback until then: keep the current behaviour; record it as a
    known limitation in the slice's evidence (see §8, risk 2).
  - `weighted` draws uniformly (all weights 1) because no per-account weight
    source exists. Honest fallback: uniform draw; a per-account weight source
    is a future operator-config decision, not this slice.
- **For the 10th value (`expiry-first`): a per-KEY quota-expiry source.**
  Per-`(provider, model)` expiry (see §D2) is the WRONG granularity — every
  account of one provider serving one model would share it and tie. The
  source must be keyed by `(provider, key)`: either operator-declared
  per-key quota-expiry in config, or per-key quota fetchers. Until one
  exists, `expiry-first` stays rejected. This is a NO-GO sub-slice (§D2).

### (4) Concrete builder slice

- Files to touch:
  1. `internal/config/config.go` — add to `RoutingConfig` (near
     `PreferredFailoverAfterMs`, `:1006-1011`):
     `AccountFallbackStrategy string
     \`json:"account_fallback_strategy,omitempty"\`` and
     `AccountFallbackStrategies map[string]string
     \`json:"account_fallback_strategies,omitempty"\``.
     Validate: unknown names rejected at load (reuse
     `router.ParseAccountFallbackStrategy` — note the import direction:
     config must NOT import router; either duplicate the 9-name allowlist
     in config validation or move the name list to a shared leaf. The
     builder decides, but must not create an import cycle; the existing
     pattern for `ChainStrategies`/`CooldownMode` validation in the same
     file is the template).
  2. `internal/config/SCHEMA.md` — document both fields, the 9 valid values,
     the `fill-first` default, and that `expiry-first` is rejected (with the
     reason: no per-key expiry source) until §D2 lands.
  3. `cmd/codvian-router/main.go` — in the `engCfg` assignments (near
     `engCfg.PreferredFailoverAfter`, `:2739`), set
     `engCfg.AccountFallbackStrategy` /
     `engCfg.AccountFallbackStrategies` from the file config.
  4. Tests (new, in the existing test files — no new test-only scaffolding):
     - config round-trip: file value survives load; per-provider override
       beats global; unknown value fails `Validate` with the value named.
     - engine test: with `Config.AccountFallbackStrategy="round-robin"`,
       two requests rotate the leading key of a two-key provider
       (toggle-verified: test fails if `orderAccountAttempts` is removed
       from `orderAttemptsWithContext`).
     - `expiry-first` still rejected by `ParseAccountFallbackStrategy`
       (regression pin for the §D2 gate).
- Definition-of-Done (Master Prompt): `go test ./internal/config/...
  ./internal/router/...` green; `gofmt` clean; the config-surface test above
  proves an operator can actually select a non-default value end-to-end from
  a config file (not just in a unit test that constructs `router.Config`
  by hand); evidence pack lists the test names + the config snippet used.
  Coordinator then flips PARITY row 388 MISSING → FULL, citing this design
  (§S1) and the evidence pack. Builders do NOT edit `parity tracker` themselves.

### (5) Deliberate divergence from the reference router

- **We have `weighted`, the reference router does not (at account level).** Ours draws
  one leading account uniformly (no weight source). Keep it: it is a superset
  that degrades honestly to uniform, and removing it would break the
  9-value symmetry with the chain-level `weighted`. Document in SCHEMA.md
  that it is uniform until a per-account weight source exists.
- **We reject `expiry-first`, the reference router accepts it.** Ours is the honest side
  of this divergence: the reference router can compute it because its domain quota cache
  carries per-connection quota windows; we have no per-key expiry anywhere,
  so accepting the name would silently order on nothing. Keep rejecting
  until §D2 lands.
- **No behavioural divergence otherwise.** Default `fill-first`, per-provider
  override, grouping by `(provider, model)`, skip under explicit client
  order — all match the reference router's shape.

---

## S2 — Router strategy sla-aware / sla (PARITY row 394)

### (1) What the reference router actually does

- `SLAStrategyImpl`, name `"sla-aware"`, alias `"sla"`
  (`open-sse/services/autoCombo/routerStrategy.ts:286-294,394-403`).
  It is one of `AUTO_ROUTING_STRATEGY_VALUES`
  (`src/shared/constants/routingStrategies.ts:40-51`).
- Policy defaults: target p95 2000 ms, max error rate 0.05, no cost cap
  unless set, `hardConstraints` off unless set
  (`routerStrategy.ts:231-236,300-306`).
- Scoring (weights 0.35 latency / 0.35 error / 0.15 health / 0.10 cost /
  0.05 stability, `:313-330`): each signal scored by
  `scoreAtOrBelowThreshold` (threshold/value, 1 when at/below); a violation
  score accumulates the relative overrun per breached SLO plus 1 when the
  circuit breaker is OPEN (`getSlaViolationScore`, `:268-284`). With hard
  constraints, fewest violations win and score breaks ties; otherwise
  highest score wins (`:324-330`).
- **Pre-filter:** OPEN-breaker candidates are removed before scoring; only
  if every candidate is OPEN does it score the full pool (`:294-295`).
- **Health is breaker state** (`getHealthScore`, `:262-266`): CLOSED = 1,
  HALF_OPEN = 0.5, else 0. **Stability is real**: `inverseNormalized` of
  measured latency stddev against the pool max (`:309,316`).
- **Unknowns score as perfect in the reference**: a candidate with p95 0
  scores 1 on latency (`scoreAtOrBelowThreshold` with value clamped to
  0.000001), and a missing cost scores 1 via `inverseNormalized`
  (`:247-260`). That is the reference router treating "no measurement" as "meets the
  SLO" — see divergence below.

### (2) What our code has today

- Pure scorer: `internal/sla/sla.go` (whole file) — `Policy`,
  `DefaultPolicy()` (2000 ms / 5% / no cap / soft), `Candidate` with explicit
  `Has*` flags, `Score` returning `(violation, score)` with the same
  0.35/0.35/0.15/0.10/0.05 weights, `Better` for the hard-constraint rule.
  Tests: `internal/sla/sla_test.go` (compliant-beats-violating,
  unknown-is-neutral, zero-tolerance, cost-cap, policy defaults).
- Hook: `StrategySLAAware` / alias `"sla"` (`internal/strategy/strategy.go`,
  `StrategySLAAware` const, `aliases` map, `Describe`); wired in
  `wiringHook` (`internal/strategy/wiring.go`, `StrategySLAAware` case):
  policy from the per-request `router.StrategyHookContext`
  (`internal/router/router.go:318-330`); latency = measured p50 EWMA,
  error rate = 1 − success rate (only when attempts were measured), cost =
  pair price, health = observed serving health (1 if any success, 0 if all
  attempts failed). Per-request context built in `strategyHookContextFor`
  (`router.go:2294-2320`), which maps unset config to the defaults.
- Tests: `internal/strategy/sla_test.go`
  (`TestSLAAwareHookOrdersCompliantFirst`, `TestSLAAwareAlias`,
  `TestSLAAwareHookHardConstraints`,
  `TestSLAAwareHookUnknownSignalsKeepDeclaredOrder`).
- **What is missing (verified, not assumed):**
  1. No file-config surface: `config.RoutingConfig` (`internal/config/config.go:954-1075`)
     has no SLA fields, `internal/config/SCHEMA.md` mentions no SLA, and
     `cmd/codvian-router/main.go` never assigns `engCfg.SLATargetP95Ms` /
     `SLAMaxErrorRate` / `SLAMaxCostPer1M` / `SLAHardConstraints`
     (the `engCfg.*` assignments at `:2686-2761` cover strategy, timeouts,
     failover — not SLA). Production therefore always runs the defaults.
     An operator who wants a 500 ms target or a cost cap today cannot have
     one. The hook works; the policy is not operator-reachable.
  2. Zero-tolerance is not expressible through config: `strategyHookContextFor`
     maps `SLAMaxErrorRate <= 0` to the 0.05 default (`router.go:2294-2320`
     and the `StrategyHookContext` doc, `:318-330`). The pure `sla.Policy`
     supports zero-tolerance (`sla.go: scoreAtOrBelowThreshold`); the
     config path destroys it.
  3. The hook does NOT filter or demote breaker-OPEN candidates. the reference router
     filters OPEN before scoring (`routerStrategy.ts:294-295`). Our
     `orderAttempts` does not remove OPEN providers before the hook either
     (only zero-quota is filtered, `filterZeroQuota`, `router.go:2106-2115`;
     breaker state gates the hedge/serve path, e.g. `hedgeSecondAvailable`,
     `router.go:2667-2681`). Consequence is bounded — an OPEN provider ranked
     first is skipped at serve time — but the ranking itself is wrong: the
     hook can put a known-dead provider first.
  4. Latency fed to the scorer is the **p50 EWMA**, not p95 (same caveat as
     `sixteenInputs` in `wiring.go`). No latency histogram exists in the
     tree, so no true p95 can be computed today. Stability is hard-coded
     neutral 0.5 (`sla.go`: `const stabilityScore = 0.5`) — no stddev signal
     exists in either caller.

### (3) The exact data source to add

- **Policy (the slice):** operator-declared SLOs from the config file —
  `routing.sla.target_p95_ms`, `routing.sla.max_error_rate`,
  `routing.sla.max_cost_per_1m_tokens`, `routing.sla.hard_constraints`.
  These are operator intent, not measurements; no telemetry is needed.
  Use pointer fields (`*float64` / `*bool`) or an explicit sentinel so that
  *unset* (default 2000 / 0.05 / no cap / soft) is distinguishable from an
  explicitly configured **0** (zero-tolerance error rate, zero cost cap).
  Recommended: pointers; `nil` = default. Do NOT reuse the current
  `<= 0 means default` rule — it is what makes zero-tolerance
  inexpressible, and silently reinterpreting an operator's explicit 0 as
  "you meant 0.05" would be a fake.
- **Breaker state (the slice):** the engine's live breaker state, which
  already exists (`Engine.breakerOpen`, `router.go:2416`). It must reach the
  hook. Recommended path: add a `BreakerOpen bool` field to
  `scoring.Signals` (`internal/scoring/scoring.go:60-124`, next to
  `Attempts`) and fill it in `signalsFn` (`cmd/codvian-router/main.go:2036`)
  from the engine. That one field also fixes the 16-factor scorer's
  `Breaker: scoring.BreakerUnknown` neutral (see `sixteenInputs` in
  `wiring.go`) — one plumbing job, two consumers. Alternative (weaker):
  demote OPEN inside `orderAttempts` before the hook; do not do that —
  it would change ordering for every strategy, not just sla-aware.
  With the field, the sla-aware hook filters OPEN candidates first
  (falling back to the full pool when all are OPEN, exactly like the reference router),
  and scores the rest.
- **p95 and stddev (NOT this slice):** no source exists. A latency histogram
  (per-provider p50/p95/stddev from measured attempt latencies) is a
  separate plumbing job: record per-attempt latencies in the engine's
  provider stats, expose p95/stddev through `scoring.Signals`, feed
  `sla.Candidate` (which already has the shape — add `LatencyStdDevMs` /
  `HasLatencyStdDev` there when the source lands). Until then the honest
  fallback stays: p50 EWMA fed as the latency value, stability neutral 0.5,
  both documented in `internal/sla/sla.go`'s package doc and in the
  strategy's `Describe` text. Never relabel p50 as p95 in output, logs, or
  the decision header.

### (4) Concrete builder slice

- Files to touch:
  1. `internal/scoring/scoring.go` — add `BreakerOpen bool` to `Signals`
     (document: false means "not open OR unknown" — the hook must treat it
     as "no breaker information", i.e. score normally, never as proof of
     health; the OPEN-filter only removes candidates positively known OPEN).
  2. `cmd/codvian-router/main.go` — `signalsFn` (`:2036`): fill
     `sig.BreakerOpen` from the engine when available (guard the nil-engine
     case exactly as the existing `Load`/`Attempts` blocks do).
  3. `internal/strategy/wiring.go` — `StrategySLAAware` case: filter
     positively-OPEN candidates before scoring (all-OPEN → score the full
     pool, the reference router's rule); leave the scoring math untouched.
  4. `internal/sla/sla.go` — only if the pointer-policy design needs it:
     no change expected; `Policy.Resolve` already distinguishes negative
     (default) from 0 (zero-tolerance). Keep it.
  5. `internal/config/config.go` — add a `SLA *SLAConfig`-style block (or
     flat `SLA*` fields; follow the existing `ModelLockout`/`WaitBudgets`
     pointer-block pattern at `config.go:1000-1005`) under `RoutingConfig`,
     with pointer fields for target/error-rate/cost-cap and a bool for hard
     constraints. Validate: target > 0 when set; error rate in [0, 1];
     cost cap >= 0.
  6. `internal/config/SCHEMA.md` — document the block, defaults, and the
     nil-vs-0 rule (explicit 0 error rate = zero tolerance).
  7. `cmd/codvian-router/main.go` — `engCfg` assignments: set the four
     `SLA*` fields from the file config (near `engCfg.PreferredFailoverAfter`,
     `:2739`).
  8. `internal/router/router.go` — `strategyHookContextFor`: change the
     error-rate mapping so an explicitly configured 0 reaches the hook as
     0 (zero-tolerance) instead of being rewritten to 0.05. The zero
     `StrategyHookContext` (context-free callers) must still mean defaults —
     preserve that by keeping the mapping in `strategyHookContextFor` only,
     driven by whether the config block was present (plumb a resolved
     policy or a `SLAConfigured bool` through `router.Config`; simplest is
     to resolve the policy once in `main.go` and store the resolved values
     in `engCfg`, with a separate `SLAErrorRateSet bool` — builder's choice,
     but the distinction must survive to the hook).
- Tests required:
  - `internal/sla`: existing tests stay green (no math change).
  - `internal/strategy/sla_test.go`: new — OPEN-breaker candidate sorts
    last (and all-OPEN pool keeps declared order / still serves);
    toggle-verified against the filter.
  - config round-trip: SLA block survives load; explicit `max_error_rate: 0`
    loads as zero-tolerance (not 0.05); absent block loads as defaults.
  - engine test: `strategyHookContextFor` with configured SLA yields the
    configured values in the hook context; unconfigured yields defaults.
- Definition-of-Done: full `go test ./internal/sla/... ./internal/strategy/...
  ./internal/config/... ./internal/router/...` green; an operator can set a
  non-default SLO in a config file and observe a different ranking for the
  same fixture (evidence: config snippet + before/after order in the test);
  `Describe("sla-aware")` still discloses the p50-as-latency and
  neutral-stability limitations. Coordinator flips row 394 → FULL on that
  evidence. If the builder cannot land the pointer/nil-vs-0 distinction
  cleanly, the slice still ships with `<= 0 means default` documented and
  the row flips to FULL with zero-tolerance recorded as a known limitation —
  but the builder must say so in the evidence, not silently keep the old
  mapping while claiming the config surface is complete.

### (5) Deliberate divergence from the reference router

- **Unknown signals score NEUTRAL (0.5), never perfect.** the reference router gives
  an unmeasured candidate a perfect latency/cost score
  (`routerStrategy.ts:247-260` — p95 0 scores 1; missing cost scores 1).
  Ours scores unknown as 0.5 and adds no violation (`internal/sla/sla.go`
  package doc). This is the correct side of the divergence and must be
  kept: ranking an unmeasured candidate first is routing on no evidence,
  and in a local-first router with cold starts it would systematically
  prefer never-tried providers. The reference's behaviour here is a bug
  shape (it rewards the absence of telemetry); do not "fix" ours to match.
- **Health is observed serving health, not breaker state.** the reference router reads
  breaker state; our hook inputs do not carry it today (this slice adds it
  for the OPEN filter, but the 0.15 health *score* stays observed-serving:
  1 = served successfully, 0 = every observed attempt failed, 0.5 =
  unobserved). Keep: breaker state is a liveness gate, not a quality score.
- **Stability neutral until a stddev source exists.** the reference router has measured
  stddev; we do not. Neutral 0.5, documented, not a guessed value.
- **Fallback on total failure:** the reference router's SLA strategy throws when no
  candidates exist; ours never throws from the hook — an empty chain stays
  empty and the engine fails honestly upstream. Keep.

---

## S3 — Router strategy nadir (PARITY row 396)

### (1) What the reference router actually does

- `NadirStrategyImpl`, name `"nadir"`
  (`open-sse/services/autoCombo/nadirStrategy.ts:2-16,153-177`), registered
  with the rules strategy as fallback
  (`open-sse/services/autoCombo/routerStrategy.ts:406`).
- It asks an external Nadir decision API which MODEL the prompt needs:
  sends the last user turn (capped at 16,000 chars) plus the pool's model ids
  (max 100) to `POST /v1/bucket` (`nadirStrategy.ts:58-66, 137-275`).
  The decision picks the model; the fallback strategy picks the CONNECTION
  serving it. Nadir never picks an account.
- Config: combo `config.nadir = { apiKey, baseUrl, timeoutMs }`, with env
  fallbacks `NADIR_API_KEY` / `NADIR_BASE_URL`
  (`nadirStrategy.ts:25-38,183-185,216`). Default base URL is hardcoded:
  `NADIR_DEFAULT_BASE_URL = "https://api.getnadir.com"` (`:58`).
  Timeout default 2000 ms, max 30000 ms (`:59-60`).
- Guarantees (header, `:16-23`): opt-in (only combos with
  `routerStrategy: "nadir"` call out); **fail-open** — any error, timeout,
  non-2xx, missing prompt, or out-of-pool selection resolves to the fallback
  decision, never a 5xx; bounded (one POST per request under a hard timeout)
  **plus a 30 s failure cooldown** (`NADIR_FAILURE_COOLDOWN_MS = 30_000`,
  `:61`) so an unreachable endpoint costs one timeout per 30 s, not one per
  request; minimal egress (last user turn + model ids only).

### (2) What our code has today

- `internal/router/nadir.go` (whole file): `nadirSelect` — normalises the
  endpoint (`normalizeNadirBaseURL`, strips a pasted `/v1`, requires
  absolute http(s)), caps the prompt at 16,000 chars
  (`NadirMaxPromptChars`), caps the menu at 100 (`NadirMaxMenuItems`),
  one POST to `<base>/v1/bucket` with `X-API-Key` only when a key is
  configured, response body capped at 1 MiB, timeout default 2000 ms / max
  30000 ms (`NadirDefaultTimeoutMs`, `NadirMaxTimeoutMs`), injectable
  `NadirTransport` (`Config.NadirTransport`, `router.go:600-604`) so tests
  prove zero external calls.
- Dispatch: `auto/nadir` handled in `internal/router/autodispatch.go:171-184`
  via `nadirSelect`; the decision is recorded (`setLastAutoDecision`,
  attribution sink) and the ordered pool is built from the selection.
- Chain hook: `StrategyNadir` is the identity — the declared (priority)
  order stands (`internal/strategy/strategy.go`, `StrategyNadir` const +
  `Describe`; `internal/strategy/wiring.go`, `StrategyNadir` case with the
  comment explaining why returning nil would wrongly fall through to the
  balanced hook). Test: `TestNadirHookIsIdentity`
  (`internal/strategy/sla_test.go:73`).
- Tests: `internal/router/nadir_test.go`
  (`TestNadirUnconfiguredMakesZeroExternalCalls`,
  `TestNadirInvalidEndpointMakesZeroExternalCalls`,
  `TestNadirEmptyPromptMakesZeroExternalCalls`,
  `TestNadirConfiguredSelectsModel`, `TestNadirFailuresFallBackToPriority`,
  `TestNadirDispatchEndToEndUnconfigured`).
- **What is missing (verified):**
  1. No file-config surface and no wiring: `config.RoutingConfig` has no
     nadir fields, `SCHEMA.md` mentions none, and `main.go` never assigns
     `engCfg.NadirEndpoint` / `NadirAPIKey` / `NadirTimeoutMs`. The
     `router.Config` fields exist (`router.go:583-604`) but nothing ever
     sets them. In production, nadir is therefore permanently unconfigured
     and every `auto/nadir` request takes the fail-closed fallback. The
     feature is built, tested, and dead.
  2. **No failure cooldown.** the reference router skips the network for 30 s after a
     failure (`nadirStrategy.ts:61,201`). Ours has no cooldown field or
     logic: a down endpoint costs a full timeout (up to 30 s of configured
     timeout, default 2 s) on EVERY `auto/nadir` request until it recovers.
     At default timeout that is 2 s of added latency per request, unbounded
     in count.
  3. The fallback is priority (catalog) order. the reference router falls back to its
     `rules` strategy (its default scorer). See divergence below.

### (3) The exact data source to add

- **Configuration (the slice):** operator-declared endpoint + timeout from
  the config file; API key from the ENVIRONMENT, never the config file
  (secrets rule — a key in a config file gets committed, logged, or copied;
  the existing `NadirAPIKey` field on `router.Config` stays as the
  in-memory carrier, fed from env by `main.go`).
  - `routing.nadir.endpoint` (string, optional; empty = unconfigured =
    fail-closed, no external call ever).
  - `routing.nadir.timeout_ms` (int, optional; <= 0 = 2000 default,
    clamped to 30000 max — the existing `nadirTimeout()` clamp in
    `nadir.go` already does this; the config layer only passes the value
    through and `Validate` rejects negatives).
  - `CODVIAN_ROUTER_NADIR_API_KEY` env var (optional; empty = no header
    sent — keyless calls land on the provider's anonymous tier, as in
    the reference router). Do NOT add an env fallback for the endpoint: the reference router's
    `NADIR_BASE_URL` env fallback is a footgun in a local-first
    router — an env var set in one shell would silently point every
    request's prompt at a third party. The endpoint must be an explicit
    config-file choice, visible in the file the operator reviews.
- **Failure cooldown (the slice):** no new external data. Cooldown state is
  local and in-memory: last-failure timestamp + endpoint identity, held on
  the engine (same shape as the existing per-key cooldown state). 30 s,
  matching the reference router's `NADIR_FAILURE_COOLDOWN_MS`. While cooling down,
  `nadirSelect` returns the fallback immediately with a note naming the
  cooldown (no network call). A successful call clears it. The cooldown is
  per endpoint (if the operator reconfigures the endpoint, the old
  endpoint's cooldown must not apply).
- **No other data source.** The prompt text comes from the request itself
  (last user turn, already available at dispatch via `lastUserPrompt`); the
  menu comes from the live catalog (`liveAutoCatalog`). Nothing else is
  needed. If no endpoint is configured, the honest fallback stays: priority
  order + a note, zero external calls.

### (4) Concrete builder slice

- Files to touch:
  1. `internal/config/config.go` — add a `Nadir *NadirConfig`-style block
     (pointer = absent means unconfigured) under `RoutingConfig`:
     `endpoint string`, `timeout_ms int`. Validate: endpoint, when set,
     must parse as an absolute http(s) URL after the same normalisation
     `nadir.go` applies (trim, strip trailing `/v1`); timeout_ms must be
     >= 0 (0 = default) and <= 30000 when set — reject anything else at
     load with the value named. Do NOT add an api-key field to the config
     file.
  2. `internal/config/SCHEMA.md` — document the block, the fail-closed
     default, the env-var key, the timeout clamp, and the explicit warning
     that the prompt's last user turn leaves the box when configured.
  3. `cmd/codvian-router/main.go` — `engCfg` assignments: set
     `engCfg.NadirEndpoint` / `NadirTimeoutMs` from the file config;
     `engCfg.NadirAPIKey` from `os.Getenv("CODVIAN_ROUTER_NADIR_API_KEY")`.
  4. `internal/router/nadir.go` — add the failure cooldown: fields on
     `Engine` (or a small struct held by the engine; follow the existing
     cooldown-state locking pattern — `e.mu` — do not invent a new lock
     discipline). `nadirSelect` checks it before dialling and records
     failures (transport error, non-2xx, malformed response) against it.
     Success clears it. The fallback note must name the cooldown when it
     fires ("decision skipped: endpoint cooling down after failure until
     <time>").
  5. `internal/router/router.go` — only if the cooldown state needs a
     `Config` knob (e.g. a test seam): prefer a fixed 30 s constant in
     `nadir.go` (matching the reference router) with no config knob. Do not add a
     knob without a reason.
- Tests required:
  - config round-trip: nadir block survives load; invalid endpoint / bad
    timeout rejected; absent block = unconfigured.
  - `internal/router/nadir_test.go`: new — failure cooldown: fake transport
    fails once, second call within 30 s makes zero transport calls and
    returns the fallback with a cooldown note; after the cooldown expires
    (injectable clock or a test-only setter — do not sleep 30 s in a test)
    it dials again; success clears the cooldown.
  - existing nadir tests stay green, especially
    `TestNadirUnconfiguredMakesZeroExternalCalls` (the fail-closed
    guarantee must survive the config work untouched).
  - engine test: with endpoint configured via `router.Config`, `auto/nadir`
    selects the fake transport's model end-to-end (the configured path,
    which today has no production coverage at all).
- Definition-of-Done: `go test ./internal/router/... ./internal/config/...`
  green; a config file with `routing.nadir.endpoint` set to a local test
  server demonstrably changes `auto/nadir` routing (evidence: test name +
  the fake-transport assertion); the unconfigured path still makes zero
  external calls; SCHEMA.md carries the egress warning. Coordinator flips
  row 396 → FULL on that evidence. Until the config surface lands, the row
  MUST NOT flip: a tested-but-unreachable feature is not FULL.

### (5) Deliberate divergence from the reference router

- **Fail-CLOSED, not fail-open, on missing configuration.** the reference router has a
  hardcoded default endpoint (`https://api.getnadir.com`) and even an
  unconfigured combo calls it, sending the user's prompt to a third party
  the operator never chose (`nadirStrategy.ts:58,183-185`). Ours has NO
  default endpoint: unconfigured means no external call, ever
  (`internal/router/nadir.go` header, `Config.NadirEndpoint` doc,
  `router.go:583-591`). This is the most important divergence in this
  document. A local-first router must not exfiltrate prompts by default.
  Keep it unconditionally — no future slice may add a default endpoint.
- **No env fallback for the endpoint.** the reference router reads
  `NADIR_BASE_URL` (`nadirStrategy.ts:183`). Ours reads the
  endpoint from the config file only (see §3). Same reasoning: the egress
  destination must be a reviewed, visible choice.
- **Fallback is priority order, not the `rules` scorer.** the reference router falls
  back to its rules strategy (`routerStrategy.ts:406`). Ours falls back to
  catalog/priority order. Keep for this slice: priority is our default
  strategy (M1.11), it is what the chain hook does for `nadir` anyway, and
  it keeps the failure path deterministic and explainable in the decision
  notes. Revisit only if a later slice gives the fallback a per-chain
  strategy (i.e. "fall back to the chain's own configured strategy instead
  of priority") — that is a product decision, not part of this slice.
- **Failure cooldown is per-endpoint, in-memory, 30 s** — same number as
  the reference router, own implementation. Keep the number; do not copy the
  per-base-URL map shape blindly — one endpoint is configured at a time,
  so one cooldown slot is enough.

---

## S4 — Preferred / reserve accounts for auto combos (PARITY row 425)

### (1) What the reference router actually does

- The deep-study records it as: "preferred and reserve accounts for Auto
  combos; opt-in preferred-account response-start failover"
  (`the internal deep-study report`,
  §1.1). The reference itself splits the concept in two places, and the
  split matters:
  1. **Per-API-key soft preference list:** `pickPreferredConnection` over
     `options.preferredConnectionIds` (`src/sse/services/auth.ts:~1937`,
     in the same function that resolves `fallbackStrategy` at `:1930`) —
     the API key names preferred connections; the engine tries one first
     when no lease is active. This is a *per-request* preference, not a
     property of the key.
  2. **Opt-in response-start failover:** `preferredConnectionFailover.ts`
     (whole file) — off by default, default timeout 20,000 ms, per-provider
     / per-model overrides, min 500 ms / max 120,000 ms, max 64 overrides.
     A preferred account that produces no first byte within the deadline
     rotates to ONE sibling; the deadline is disarmed by the first byte.
     Only for connections selected through the API key's soft preference
     list, never forced pins / combo steps.
- No single reference file defines a "reserve account" for auto combos in
  the terms PARITY row 425 uses; the row's evidence is the deep-study
  summary plus the failover file. That is a documentation gap in the row
  itself, recorded here so the coordinator can tighten the citation when
  flipping it — the builder must not invent a citation.

### (2) What our code has today

- **Key roles (the account concept):** `internal/provider/keys.go` —
  `ProviderKey` roles `preferred` / `reserve` (`:16-35`),
  `EffectiveRole()` / `IsReserve()` (`:53-54,106-109`), `PreferredKeys()` /
  `ReserveKeys()` (`:164-200`). A legacy single key synthesises to
  preferred. Roles are operator-settable via the API (see
  `docs/API_REFERENCE.md:1407`, "Changes one provider key's role
  (preferred/reserve)").
- **Chain construction:** `attemptsForProvider`
  (`cmd/codvian-router/main.go:2180-2210`) expands one provider into one
  attempt per registered key, marking `Reserve: k.Role ==
  provider.KeyRoleReserve` (`:2195`); keys with no client are skipped.
- **Reserve-last ordering (flat path):** `applyTailPartitions`
  (`internal/router/router.go:2148-2170`) — the FINAL step, after scoring,
  adaptive partition and soft-demotion: a stable partition, preferred first,
  reserve last, never a drop (an all-reserve pool still serves). Reserve
  attempts are never hedge targets (`hedgeSecondAvailable`, `router.go:2667-2681`).
- **Reserve-last ordering (auto-combo path):** `orderPreferredReserve`
  (`internal/router/account_fallback.go:163-180`, pure function) applied in
  `chainForDispatch` (`internal/router/autodispatch.go:303-307`) for both
  the multi-model and single-model paths, idempotent with the later
  `orderAttempts` partition. This is the row-425 mechanism itself, already
  on the auto-combo path.
- **Response-start failover (the opt-in half):** `internal/router/preferred.go`
  (whole file) + the arming site (`internal/router/router.go:5403-5406`):
  opt-in via `Config.PreferredFailoverAfter`
  (`router.go:405-411`, default `PreferredFailoverAfterDefault` = 20 s,
  `preferred.go:44`); first streaming attempt only, rotates to ONE
  immediate sibling (same provider, different key), disarmed by the first
  byte, pre-commit only, breaker-neutral. File-config surface EXISTS for
  this half: `routing.preferred_failover_after_ms`
  (`internal/config/config.go:1006-1011`), wired in `main.go:2739`.
  parity tracker Area 2 already records it FULL
  (`internal parity tracker:471`, tests `TestPreferredFailover*`).
- **Tests:** `internal/router/keypool_test.go` (10 tests: preferred
  401/429/quota failover to the second key, per-key cooldown isolation,
  reserve-only-when-exhausted incl. reserve-listed-first-still-sorts-last,
  per-key ledger attribution, streaming pre-commit 401 failover) and
  `internal/router/account_fallback_test.go`
  (`TestChainForDispatchPreferredReserve`,
  `TestOrderAttemptsAppliesAccountFallbackThenReserveLast`).
  Live smoke 26/26 recorded in `docs/slice-175-176-keypool-report.md`.
- **What is genuinely missing:** only concept (1) above — a *per-request*
  preferred-connection list. Our "preferred" is a static role on the key;
  the reference router's is a soft preference carried by the API key / request. We
  have no per-request preference input anywhere (no API-key preference
  field, no routing-contract field for it).

### (3) The exact data source to add

- **For the row as written: none.** The row's mechanism (preferred first,
  reserve held back, on the auto-combo path) is fully sourced already:
  roles from the provider registry (operator-set, persisted), reserve flag
  carried on `Attempt` from chain construction, ordering from the two
  partitions above. No new measurement is needed and none should be added.
- **For the per-request preference list (follow-up, NOT this slice):** a
  preference input owned by the caller — either a per-API-key preference
  list stored with the key (new settings surface, needs auth/settings
  design) or a per-request routing-contract field (needs contract-schema
  design). Both are product decisions: they change the auth/settings
  surface and the contract schema, and neither is specified anywhere in
  the reference material beyond the `preferredConnectionIds` name. Do NOT
  build either in this residual.

### (4) Concrete builder slice

- This slice is **verification + documentation, not a build**. Files to touch:
  1. No production code file. If the verification below finds a real gap,
     stop and report it to the coordinator instead of improvising a fix —
     the gap becomes a new slice.
  2. Verification (run, do not assume):
     - `go test ./internal/router/ -run 'TestKeyPool|TestChainForDispatchPreferredReserve|TestOrderAttemptsAppliesAccountFallbackThenReserveLast|TestPreferredFailover'` green.
     - A live smoke (binary + mock provider, following
       `docs/slice-175-176-keypool-report.md` § smoke): preferred key 429
       → reserve serves; reserve listed first in the pool still sorts last;
       auto-combo dispatch (`auto/chat` or equivalent) over a two-key
       provider serves preferred first. Record the commands + results.
  3. Documentation (the deliverable of this slice): a short evidence note
     (new file under `docs/design/` or an appendix to this design — builder's
     choice, coordinator approves) stating, with the file:line citations
     from §2 above: the mechanism, the two paths it covers (flat +
     auto-combo), the failover half (already FULL in Area 2), and the
     explicit non-goal (per-request preference list, pending product
     decision). If the dashboard Providers view does not already show key
     roles, note it as a dashboard follow-up (M10), not an M3 blocker.
- Definition-of-Done: the verification commands above pass on the current
  tree; the evidence note exists with exact test names and smoke output.
  Coordinator flips row 425 MISSING → FULL on that evidence, and tightens
  the row's the reference router citation (deep-study §1.1 + `preferredConnectionFailover.ts`
  + `auth.ts:1930-1945`) at flip time. If verification FAILS anywhere, the
  row stays MISSING, the failure is reported verbatim, and a fix slice is
  written — no flip on a failed verification.

### (5) Deliberate divergence from the reference router

- **Preferred/reserve is a static key role, not a per-request list.** Ours
  is set by the operator on the key and persists; the reference router's preferred
  connection is chosen per request from the API key's preference list.
  Ours is simpler, auditable (the role is visible on the key), and needs no
  auth/settings surface. The cost is real: a caller cannot say "for THIS
  request, prefer connection X". That cost is accepted for M3; the
  per-request list is a named follow-up (§7, NO-GO until decided).
- **Reserve-last runs even under an explicit contract order.**
  (`router.go:2148-2170` comment: the contract names providers, never keys,
  so it cannot have ordered a reserve key first on purpose.) the reference router's
  failover file excludes forced pins/combo steps from rotation; ours
  partitions reserve last regardless. Keep: it is the safer reading — a
  reserve key is by definition held back, and no contract in our schema can
  name a key.
- **Failover rotates to one sibling, pre-commit only, breaker-neutral.**
  Matches the reference router's shape; the 20 s default matches exactly. No
  divergence beyond the static-role point above.

---

## D1 — Data source: per-pair limit for headroom (PARITY row: headroom, PARTIAL)

### (1) What the reference router actually does

- Formula: `headroom = 1 − max(util5h, util7d)`, most free capacity first
  (`open-sse/services/combo/headroomRanking.ts:54`, `computeHeadroom`;
  ranking `rankByHeadroom` in the same file).
- The utilisation comes from `getSaturation`
  (`src/lib/quota/saturationSignals.ts:543`): per provider, per connection,
  per window (5h / weekly). Sources are provider-specific fetchers —
  Codex quota fetcher, Bailian quota fetcher, Anthropic OAuth usage
  (`GET /api/oauth/usage`, five_hour / seven_day utilisation), falling back
  to per-minute request rate-limit headers, and a generic `getUsageForProvider`
  default (`saturationSignals.ts:2-20`). In-memory cache, 30 s TTL, bounded
  map of 4096 entries (`:52-60`); fail-open to 0 on any error.
- The orderer (`open-sse/services/combo/quotaStrategies.ts:824-900`,
  `orderTargetsByHeadroom`) expands targets across connections, fetches
  saturation once per unique connection with bounded concurrency, threads
  the decrypted connection snapshot through so credential-needing fetchers
  can actually read (comment at `:872-883`, issue #6379 — without this the
  fetchers fail open to 0 for every candidate and headroom cannot tell
  accounts apart), and ranks. Targets without a connection get full
  headroom. Any unexpected failure leaves the order unchanged.
- Key honesty point: the reference router's utilisation is a **measured fraction
  against a provider-reported limit** (OAuth plan windows, quota fetchers).
  It is not spend divided by a guessed cap.

### (2) What our code has today

- Pure ordering: `Headroom` / `OrderHeadroom`
  (`internal/strategy/quota.go:186-210`, `Headroom` at `:186`); constants
  mirror the reference. Missing utilisation is treated as 0 (full headroom)
  — the reference router's own fail-open (`quota.go` header + `clampUtil`).
- Wiring: `StrategyHeadroom` case in `wiring.go` via `toQuotaSnapshots`
  (feeds `s.Util5h` / `s.Util7d` from `scoring.Signals`).
- The signal is always unknown: `Ledger.PairQuota`
  (`internal/ledger/quotaquery.go:122-165`) sets `Util5h`/`Util7d` to
  `UnknownUtilisation` (-1) always, because no per-pair limit exists; the
  file header (`quotaquery.go:1-35`) states this explicitly, and
  `Utilisation(spent, limit)` (`:78-95`) refuses to compute without a
  positive finite limit. `signalsFn` (`cmd/codvian-router/main.go:2036`)
  passes the -1 through. `scoring.Signals.Util5h/Util7d`
  (`internal/scoring/scoring.go:84-97`) document -1 as unknown.
- Consequence: today every target scores headroom 1 and the strategy is a
  no-op that preserves declared order. That is the reference router's fail-open, but
  it is not headroom ranking — the row is PARTIAL for exactly this reason.
- Tests: `TestHeadroomOrder` (`internal/strategy/quota_test.go`) proves the
  pure ordering given known utils; no test can prove production ranking
  because production utils are always -1.

### (3) The exact data source to add — decision required (NO-GO until decided)

There is no measured per-pair limit anywhere in the system, and three
candidate denominators must NOT be confused:

| Candidate denominator | Verdict |
|---|---|
| Gateway-key budgets (`key_budgets`: rolling 5h USD cap; `budgets`: cap with opaque period) | INVALID. A gateway key's spend spans providers; its limit cannot be attributed to one `(provider, model)` pair (`quotaquery.go` header). Dividing pair spend by a key limit would understate utilisation for every pair on that key. |
| Quota pools (`internal/ledger/pool.go`, `EffectiveLimit`) | INVALID as a pair limit. A pool limit is a pool/key concept, not a pair concept. Do not reuse it. |
| Operator-declared per-pair limit (new config) | VALID, local, no network. `routing.pair_limits: {"provider/model": {"limit_usd_5h": N, "limit_usd_7d": M}}`. Spend is already measured (`PairQuota.Spent5h/Spent7d`, priced USD, lower bound when unpriced usage exists). `Utilisation(spent, limit)` already exists. This is the smallest honest source. |
| Provider quota fetchers (reference-faithful) | VALID, but large. Per-provider fetchers against upstream usage endpoints (OAuth usage, quota APIs), with credentials, a 30 s TTL cache, bounded concurrency, and fail-open to -1 (NOT 0 — see divergence). Needs a fetcher registry, credential threading (the reference router #6379 lesson), and per-provider work. A multi-slice project, not one builder slice. |

- **Recommended decision (for Raunak): option 3 first** — operator-declared
  per-pair limits in the config file. It makes headroom real for the pairs
  the operator cares about with one small slice, using spend the ledger
  already measures. Option 4 (fetchers) follows later, per provider, and
  when it lands it should OVERRIDE the declared limit for pairs it covers
  (measured beats declared) — the precedence must be specified in that
  later design, not guessed then.
- **Honest fallback until the decision lands:** keep `-1` (unknown) and
  keep headroom scoring 1 for all targets. Do NOT "fix" headroom by
  ranking on raw spend (cheapest-spend-first is not headroom; it would
  penalise the pair the operator uses most for legitimate reasons), and do
  NOT invent a limit (e.g. deriving one from the price book or from peak
  observed spend — peak spend is not a limit). The current no-op-with-
  fail-open is the correct holding position.
- **Granularity note:** the limit is per `(provider, model)` pair and per
  window (5h + 7d), matching `PairQuota`. A single limit for both windows
  would conflate the two utilisations the formula takes the max over.

### (4) Concrete builder slice (buildable ONLY after the §3 decision)

- Files to touch (assuming option 3, operator-declared limits):
  1. `internal/config/config.go` — `RoutingConfig.PairLimits`
     `map[string]PairLimit` (`limit_usd_5h`, `limit_usd_7d`, both > 0 when
     set; key form `provider/model`, validated against known providers at
     load with unknown pairs rejected-or-warned — reject, naming the pair,
     following the existing unknown-member rule for routing groups).
  2. `internal/config/SCHEMA.md` — document the field, the units (USD per
     window, matching ledger priced spend), and that unset pairs stay
     unknown (-1), never 0.
  3. `internal/ledger/quotaquery.go` — `PairQuota` takes the limit (or a
     `PairLimitFor func(provider, model string) (limit5h, limit7d float64,
     ok bool)`): compute `Util5h = Utilisation(Spent5h, limit5h)`,
     `Util7d = Utilisation(Spent7d, limit7d)`; when no limit, keep -1.
     No change to the spend query.
  4. `cmd/codvian-router/main.go` — `signalsFn` already passes `pq.Util5h`/
     `pq.Util7d` through; wire the limit lookup into the `pqCache` call
     (or the `PairQuota` call it wraps). No change to `wiring.go` or
     `quota.go` — the ordering already consumes the signals.
  5. Tests: ledger test — spend $60 of a $100 5h limit reads Util5h 0.6;
     no limit reads -1; limit 0 / negative reads -1 (never divide by zero,
     never invent). Strategy test — hook orders by headroom given configured
     limits (toggle-verified: fails if the limit lookup is removed).
     Config test — bad pair key / non-positive limit rejected.
- Definition-of-Done: the ledger test + hook test green; a configured pair
  demonstrably reorders under `headroom` in an engine test; unconfigured
  pairs still score headroom 1 (fail-open preserved). Coordinator flips
  headroom PARTIAL → FULL only when at least the pairs under test have real
  limits — a FULL verdict on a feature that is still -1 for every pair in
  the operator's actual config would repeat the S3 mistake (certifying a
  dead path). If the decision goes to option 4 (fetchers) instead, this
  slice is void and a new design is written for the fetcher registry.
- If the decision is "no limit source, keep headroom as fail-open forever":
  then the honest outcome is to record headroom as a deliberate
  DIVERGENCE (fail-open by design, declared order preserved) rather than
  leaving it PARTIAL indefinitely — but that is a coordinator + Raunak call,
  not a builder call.

### (5) Deliberate divergence from the reference router

- **Unknown utilisation must stay unknown (-1), and the fail-open must be
  visible, not silent.** the reference router's `getSaturation` fails open to 0 and
  `rankByHeadroom` ranks unknown as full headroom — the same numeric
  outcome as ours today, but the reference router usually HAS a measurement, so the
  fail-open rarely fires. Ours fires always. Keep the -1 sentinel in
  `Signals`/`PairQuota` (it already exists) so that logs, the decision
  header, and any future dashboard can distinguish "measured 0% utilised"
  from "no limit configured". The ordering's fail-open (unknown → 1) stays
  — changing it to "unknown sorts last" would be a different strategy, not
  headroom.
- **When fetchers land, fail-open must be to -1 (unknown), not 0.**
  the reference router's fetchers return 0 on error, which then ranks a broken fetcher
  as "most free capacity" — the exact account a failing fetcher hides is
  the one headroom would then prefer. Our `Utilisation` already returns -1
  for a missing limit; a fetcher error must produce -1 too, and the
  ordering must treat -1 as full headroom only at rank time (as today),
  never store it as a measured 0. Record this now so the future fetcher
  design does not copy the bug.

---

## D2 — Data source: quota expiry for expiry-first (PARITY rows: expiry-first PARTIAL; account-fallback expiry-first BLOCKED)

### (1) What the reference router actually does

- Score: `scoreExpiryFirstQuota`
  (`open-sse/services/combo/quotaScoring.ts:542-640`): "how much quota it
  must spend PER HOUR to avoid losing it at its deadline" — `usable /
  hoursUntilDeadline`, most urgent first. Exhausted accounts score 0 and
  are never preferred; with no window reporting a reset it falls back to
  weighted leftover (`usable` alone). With burn telemetry it projects the
  expiring tranche (`usable − burn × hours`, scaled by plan-capacity
  weight). Defaults: `minHours` 0.25, exhausted floor 1%, tie band 5%
  (`:413-430`, `EXPIRY_FIRST_DEFAULTS`).
- **The deadline is the window RESET, not a grant-expiry timestamp.**
  `resolveUsableAndReset` / `projectExpiryScore` (`:470-540`): the deadline
  is the nearest reset of a window LONGER than the session (weekly,
  monthly) when one exists. A 5h session refills many times before a weekly
  cap resets, so what is lost is what the longer window holds at ITS reset;
  the session only caps what can be spent right now. The session reset is
  the deadline only when it is the only window or no longer window reports
  one (comments citing #18541, #18552 — taking the session reset as the
  deadline made two subscription accounts trade places every few hours and
  ignore weekly resets two days apart).
- Source of the windows: the domain quota cache — per-connection quota
  windows with `percentUsed` / `fractionReported` / `resetAt`
  (`orderTargetsByExpiryFirst`, `quotaStrategies.ts:721-800`;
  `buildScopedExpiryQuotaView` builds the per-connection view). The
  orderer scores per connection, rotates leading ties within the 5% band
  (`rotateLeadingTies`), and when every score is 0 keeps expanded
  (priority) order with no rotation.
- At account level, `expiry-first` is one of the 9 fallback values
  (`routingStrategies.ts:56-65`) and orders accounts by this score.

### (2) What our code has today

- Pure ordering: `ExpiryFirstScore` / `OrderExpiryFirst`
  (`internal/strategy/quota.go:401-478`): score = `usable /
  max(0.25, hoursUntilDeadline)`; deadline = `ExpiresAt` when known, else
  `ResetAt`; no deadline → `usable` alone; spent (≤ 1%) scores 0;
  no-telemetry scores 0 and keeps input order. Constants mirror the
  reference (`QuotaExpiryMinHours` 0.25, `QuotaExhaustedFloor` 0.01).
- Wiring: `StrategyExpiryFirst` case in `wiring.go` via `toQuotaSnapshots`;
  `ExpiresAt` comes from `scoring.Signals.ExpiresAt`
  (`internal/scoring/scoring.go:105-109`).
- **The signal does not exist:** `signalsFn` (`main.go:2036`) never sets
  `ExpiresAt` — it stays zero everywhere. `quotaquery.go` header (`:28-35`):
  "Quota EXPIRY does not exist anywhere in the system: no key expiry, no
  quota-grant expiry, no subscription expiry is recorded in the ledger.
  There is no ExpiresAt source to read; callers leave it zero."
  So today the deadline falls back to `ResetAt` — the pair's 5h roll-off
  (`PairQuota.ResetAt`, oldest in-window priced spend + 5h) — and the
  usable fraction is the booleanised `QuotaHint`, not a measured fraction.
  The row is PARTIAL for exactly this reason, and the account-level
  `expiry-first` value is rejected for the same reason (see §S1).
- Tests: `TestExpiryFirstOrder`, `TestConformanceExpiryFirstMostUrgentFirst`
  (pure ordering, given known deadlines).

### (3) The exact data source to add — decision required (NO-GO until decided)

- **What "expiry" means here must be decided first, because the reference router's
  own word is misleading.** In the reference router, "expiry-first" is really
  "reset-first for the longest window": the deadline is a window reset, and
  a true grant-expiry timestamp is never the input. Our `ExpiresAt` field
  name bakes in the grant-expiry reading, which has no source. The design
  decision is therefore which of these the operator wants:
  1. **Operator-declared per-pair quota expiry (new config).** The operator
     writes, per `(provider, model)` pair, when that pair's quota grant
     expires (e.g. a monthly grant ending on a date) and/or the remaining
     fraction. Smallest slice; honest because the operator asserts it.
     Source: config file → `PairQuota` gains an `ExpiresAt` (and ideally a
     measured remaining fraction, replacing the booleanised `QuotaHint`
     for this strategy — see below).
  2. **Provider quota fetchers (reference-faithful).** Per-connection quota
     windows with `percentUsed` + `resetAt`, from upstream usage endpoints.
     This is the same fetcher project as §D1 option 4, and it delivers the
     *reset* deadlines the reference router actually uses — at which point our
     `ExpiryFirstScore` should adopt the reference router's longer-window deadline rule
     (see divergence below) instead of the single-`ResetAt` fallback.
  3. **Per-key expiry for the account-level value.** Same two options, but
     keyed by `(provider, key)`, not by pair. Needed before account-level
     `expiry-first` can be accepted. Note the granularity trap: a per-pair
     expiry source does NOT unblock the account-level value.
- **Trap to avoid — gateway-key `ExpiresAt` is NOT quota expiry.**
  `internal/config/gatewaykey.go:72` has an `ExpiresAt` on OUR gateway key
  (when our key expires). Reusing it as the upstream quota-grant expiry
  would silently equate "our credential expires" with "the provider's quota
  resets" — different events, different dates. Do not wire it in. If an
  operator ever wants key-expiry ordering, that is a different strategy
  with a different name, not `expiry-first`.
- **The usable-fraction half of the source.** `ExpiryFirstScore` divides a
  *usable fraction* by hours. Today that fraction is the booleanised
  `QuotaHint` (1 or 0). Even with a real deadline, a boolean usable makes
  the score a step function (all unexhausted pairs score `1/hours`). A
  real expiry-first needs a measured remaining fraction: from
  `1 − utilisation` once §D1 lands a limit, or from the operator-declared
  remaining in option 1, or from the fetcher's `percentUsed` in option 2.
  The slice that adds the deadline MUST also wire the measured fraction
  (via `QuotaRemaining` in `QuotaSnapshot`, which already exists as a
  field — the plumbing is `toQuotaSnapshots`, which booleanises today).
  Landing a deadline on a boolean usable would claim a precision the
  score does not have.
- **Honest fallback until the decision lands:** keep `ExpiresAt` zero;
  keep the `ResetAt` fallback (the 5h roll-off is a real, measured
  deadline-ish signal — the oldest spend ages out then); keep the
  booleanised usable. Document all three in the strategy's `Describe`
  text and in `wiring.go`'s data-honesty header (already done). Do NOT
  invent an expiry date from the reset time by adding a constant (e.g.
  "reset + 7d") — that would manufacture a deadline.

### (4) Concrete builder slice (buildable ONLY after the §3 decision)

- Files to touch (assuming option 1, operator-declared per-pair expiry):
  1. `internal/config/config.go` — `RoutingConfig.PairExpiry`
     `map[string]PairExpiry` (`expires_at` RFC3339, optional
     `remaining_fraction` 0..1). Validate: parseable time in the future
     (past expiry = the pair is spent — reject at load, naming the pair,
     rather than silently scoring it 0 forever); fraction in [0,1].
  2. `internal/config/SCHEMA.md` — document the field, the semantics
     ("when this pair's quota grant is thrown away"), and that it is an
     operator assertion, not a provider-reported value.
  3. `internal/ledger/quotaquery.go` — `PairQuota` gains `ExpiresAt` (and
     the measured remaining fraction when provided): the expiry comes from
     the config lookup, not from a ledger query — say so in the struct doc,
     because a future reader will otherwise assume the ledger measured it.
  4. `cmd/codvian-router/main.go` — `signalsFn`: set `sig.ExpiresAt` from
     the pair-expiry lookup; set the measured remaining fraction into the
     signal path so `toQuotaSnapshots` can use it (this needs a field:
     either reuse `QuotaHint` honestly — no, it is an int hint — or add a
     `QuotaRemainingPct float64` to `scoring.Signals` next to `Util5h`;
     recommended: the new field, -1 = unknown, matching the existing
     sentinel convention).
  5. `internal/strategy/wiring.go` — `toQuotaSnapshots`: use the measured
     remaining fraction when present, booleanised hint only as fallback.
     No change to `quota.go` scoring math.
  6. Tests: config round-trip + validation; ledger/pair test — declared
     expiry surfaces in `Signals.ExpiresAt`; strategy test — hook orders
     soonest-expiring first given declared expiries (toggle-verified);
     regression — undeclared pairs still fall back to `ResetAt`.
- Definition-of-Done: the tests above green; an engine test shows a
  declared expiry reordering a chain under `expiry-first` while an
  undeclared pair keeps the reset fallback; `Describe("expiry-first")`
  states the source ("operator-declared" vs "provider-reported") so the
  operator knows what the ranking is standing on. Coordinator flips
  expiry-first PARTIAL → FULL on that evidence. The account-level
  `expiry-first` value stays rejected until the per-KEY source (option 3)
  lands — that is a separate slice with its own design note, referencing
  this section.
- If the decision goes to option 2 (fetchers): this slice is void; the
  fetcher design must also adopt the longer-window deadline rule (§5) and
  the tie-band rotation, which are NOT in our `quota.go` today.

### (5) Deliberate divergence from the reference router — and one divergence to FIX

- **FIX (when a real deadline source lands): adopt the reference router's
  longer-window deadline rule.** Our `ExpiryFirstScore` today uses a single
  `ResetAt` — the 5h roll-off — as the deadline. the reference router explicitly
  warns against exactly this (#18541): a session reset is not where quota
  is lost; using it makes accounts trade places every few hours and ignores
  weekly resets days apart. When §D1/§D2 deliver per-window resets (or the
  operator declares a grant expiry), the scorer must take the LONGEST
  window's reset (or the declared grant expiry, which is by definition the
  longest horizon) as the deadline, and use the session/5h reset only when
  it is the sole window. This is a case where the reference router is RIGHT and our
  single-`ResetAt` fallback is a simplification that must not survive the
  arrival of real data. The fix belongs in the same slice that lands the
  source — do not land the source and leave the wrong deadline rule.
- **Tie-band rotation is missing (ours: stable sort only).** the reference router
  rotates leading ties within 5% (`rotateLeadingTies`,
  `quotaStrategies.ts:788-792`) so tied accounts share the lead over time.
  Ours keeps declared order on ties (`quota.go` header documents the
  choice: rotation is a coordinator concern). Keep the stable sort for the
  operator-declared slice (deterministic, testable); adopt rotation only
  with the fetcher slice, where per-connection scores actually tie often
  enough for it to matter. Record the choice; do not present stable-sort
  ties as rotation.
- **Burn projection is out of scope.** the reference router's metric-aware score
  subtracts observed burn (`quotaScoring.ts:561-570`). We have no burn-rate
  signal (spend rate per window could be derived from the ledger later,
  but it is not a source today). The legacy score (`usable / hours`) is
  the correct holding position; do not approximate burn from a single
  spend snapshot.

---

## §7 — Ordered build sequence and go / no-go per slice

Build in this order. Each slice is independently reviewable and committable
(local commits only — the bulk-push rule stands: no push until the whole
build is complete and professional, via the logged-in browser).

| # | Slice | Go / No-go | Why this order |
|---|---|---|---|
| S4 | Preferred/reserve: verification + evidence note (no code) | **GO — build first** | Zero code risk; if verification fails it changes what S1–S3 may assume about the engine. Also the only slice that can flip a row without a config decision. |
| S1 | Account fallback: config surface (global + per-provider) | **GO** | Smallest real build; unblocks the row-388 flip; no decision needed (9 values fixed, `expiry-first` stays rejected). |
| S2 | SLA-aware: config surface + breaker-OPEN filter | **GO** | No decision needed except the nil-vs-0 rule, which this design settles (pointers; explicit 0 = zero-tolerance). p95/stddev explicitly deferred, documented. |
| S3 | Nadir: config surface + failure cooldown | **GO** | No decision needed except the ones this design settles (config-file endpoint only, env-var key only, 30 s cooldown, fail-closed always). This is the slice that makes nadir real instead of dead. |
| D1 | Headroom: per-pair limit source | **NO-GO — needs Raunak's decision** | Choose operator-declared limits (recommended) vs provider fetchers vs keep fail-open as a recorded divergence. Do not build until decided; the wrong denominator is worse than no denominator. |
| D2 | Expiry-first: quota-expiry source (+ measured remaining fraction) | **NO-GO — needs Raunak's decision** | Choose operator-declared expiry vs fetchers; and confirm the longer-window deadline rule (§D2.5) is accepted. Account-level `expiry-first` needs the per-KEY variant of the same decision. |
| S5 | (Follow-up, not in this residual) Per-request preferred-connection list | **NO-GO — needs product decision** | Needs an auth/settings or contract-schema design first. Named here so it is not lost; not a builder slice. |
| S6 | (Follow-up) Latency histogram: true p95 + stddev for sla-aware and the 16-factor scorer | **NO-GO — needs sequencing decision** | Real plumbing (per-attempt latency recording → p95/stddev in `scoring.Signals`). Valuable, but it is not on the M3 exit-gate path — sla-aware flips to FULL without it, with the limitation documented. Sequence after D1/D2. |

**Expected outcome if S4+S1+S2+S3 all land with their DoDs met:** Area 1
FULL 34 → 38, MISSING 4 → 0, weighted 73.8% → **80.2% — the M3 exit gate
is met**. If D1+D2 then land as well: FULL 40, PARTIAL 23, weighted
**81.7%**. If any slice's DoD is not met, its row does not flip and the
gate number is recomputed honestly from the table — never adjusted to fit
the gate.

**Per-slice sequencing constraints (do not parallelise across these):**
S1, S2, S3 all touch `internal/config/config.go`, `SCHEMA.md`, and the
`engCfg` assignments in `cmd/codvian-router/main.go` — they must be built
sequentially or on disjoint branches merged in order, or the config-file
edits will collide. S4 touches no production code and can run in parallel
with any of them. D1 and D2 both touch `PairQuota` / `signalsFn` /
`toQuotaSnapshots` — they must be sequenced after each other, D1 first
(the measured remaining fraction D2 needs comes from the limit D1 adds).

---

## §8 — Risks, known limitations, and what was deliberately NOT designed

1. **Biggest risk: certifying dead code (see the headline finding).**
   Nadir is built and tested but unreachable; sla-aware and account-fallback
   run only on defaults. If PARITY flips on code evidence alone, the gate
   number (80.2%) would describe the test suite, not the product. Mitigation
   is structural, not exhortation: S1–S3's DoDs require a config-file-driven
   end-to-end proof (an operator-visible input changing a ranking), and the
   coordinator must not flip rows 388/394/396 on unit tests that construct
   `router.Config` by hand.
2. **Account-level p2c is a draw, not a scored choice.** Both accounts of
   one provider share the provider-level score
   (`account_fallback.go: orderAccountP2C` + `accountP2CScore`), so the
   score ties unless breaker state changes between the two reads. Per-key
   latency/success signals do not exist. This is documented in the code and
   in §S1.3; it is not a blocker for the row flip (the reference router's account p2c
   has the same shape — its per-connection scores come from its quota
   cache, which we do not have), but a builder must not claim p2c "picks
   the better account" in evidence. It picks a random account, guarded by
   the breaker.
3. **The 0.2-point margin.** The gate is met by 80.2% if and only if all
   four rows flip. Any single slice failing leaves the gate unmet (three
   flips: (37+12.5)/63 = 78.6%). There is no rounding up. If a slice
   fails, say so and let D1/D2 or the 16-factor pool work close the gap —
   do not re-grade a PARTIAL as FULL to cross the line.
4. **Concurrent-work hazard.** The worktree is dirty and other coordinators
   are landing M9 work in the same files (`router.go`, `autodispatch.go`,
   `main.go`). Every slice must re-verify its file:line citations at build
   time and rebase onto the current HEAD before testing. A test that passes
   against a stale checkout proves nothing.
5. **the reference router's "expiry" is a reset (see §D2.1).** Any builder who reads
   the row name literally and hunts for a grant-expiry timestamp in the
   reference will conclude the feature is unimplementable. The reference's
   deadline is a window reset; our design must carry that reading, and the
   longer-window rule with it, or the fetcher slice will reproduce the
   #18541 bug our current single-`ResetAt` fallback already has in
   miniature.
6. **What this design deliberately does NOT cover:** the 16-factor pool
   scoring gap (5 factors of 16 in the auto scorer), the production
   summariser / quota-use signal / cross-request session store for
   context-relay, candidate-pool exclusions, billing-class / premium
   classification for the 4-tier cascade and auto/thrifty — all named in
  `internal plan:184-191` as residual data-source gaps, all outside the four
   rows + two data sources this design was asked for. A builder must not
   expand scope into them from this document; each needs its own design
  with its own sources.
7. **Secret handling for S3.** The Nadir API key must never appear in a
   config file, a log line, a decision note, or a test fixture. `main.go`
   reads it from `CODVIAN_ROUTER_NADIR_API_KEY` only. If a test needs a
   key, it uses a fake transport with a literal `"test-key"` value inside
   the test file, never a real credential.

## Verification of this document

- Re-read against the sources (2026-11-19 ~13:30 IST): every `file:line`
  cited for our code was read in this session (`internal/strategy/strategy.go`,
  `internal/strategy/wiring.go`, `internal/strategy/quota.go`,
  `internal/sla/sla.go`, `internal/router/router.go`,
  `internal/router/nadir.go`, `internal/router/account_fallback.go`,
  `internal/router/preferred.go`, `internal/router/autodispatch.go`,
  `internal/ledger/quotaquery.go`, `internal/scoring/scoring.go`,
  `internal/config/config.go` (RoutingConfig), `cmd/codvian-router/main.go`
  (`signalsFn`, `engCfg`, `attemptsForProvider`), `internal parity tracker` Area 1,
  `internal plan` M3 section); every the reference router citation was read in
  `the local reference clone` (`src/shared/constants/routingStrategies.ts`,
  `src/sse/services/auth.ts`, `open-sse/services/autoCombo/routerStrategy.ts`,
  `open-sse/services/autoCombo/nadirStrategy.ts`,
  `open-sse/services/combo/headroomRanking.ts`,
  `open-sse/services/combo/quotaScoring.ts`,
  `open-sse/services/combo/quotaStrategies.ts`,
  `src/lib/quota/saturationSignals.ts`,
  `src/sse/services/preferredConnectionFailover.ts`).
- Honest gaps in this document itself: (a) no direct the reference router file:line
  was found for a "reserve account" in auto combos — row 425's citation
  rests on the deep-study summary plus the failover file (see §S4.1);
  (b) `headroomRanking.ts:54` is cited per PARITY and confirmed by reading
  the file's `computeHeadroom` — the exact line may have shifted in the
  reference clone; (c) line numbers in our tree will have shifted by the
  time a builder reads this — re-verify before building.
