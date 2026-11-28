# T02 — RT-1 to RT-5 routing-strategy gap fixes (GAP-TRACK §10)

- Slice: T02 (RT-1–RT-5)
- Date: 2026-11-19 IST (~19:15 IST)
- Worktree: ~/workspace/codvian-router/, HEAD 6831302 (dirty tree, M9 merge wave 035d1ed)
- Verifier role: verify-only. No code, docs (other than this file), commits or pushes touched.

## Verdict per row

| Row | Fix | Verdict |
|-----|-----|---------|
| RT-1 | Default `routing.deadline_budget_ms` = 300000 in config defaults | **NOT-LANDED** |
| RT-2 | LKGP pin TTL (15 min) + Clear on failure | **PASS** |
| RT-3 | Combo panel budget gate in `runComboStrategy` (sum panel estimates pre-fanout, refuse/trim on strict-budget exceed) | **NOT-LANDED** |
| RT-4 | Measured warmth plumbed into sixteenInputs + real weights for cacheAffinity / resetWindowAffinity / reliability | **PASS-WITH-NITS** |
| RT-5 | HookFactory warning log on unknown strategy name | **PASS** |

## What was verified (commands run + results)

All commands run with `export PATH=$HOME/go/bin:$PATH`, `TMPDIR=$HOME/.tmp`, from `~/workspace/codvian-router`. Targeted `-run` only; no full suite, no `-race`.

1. `go test -count=1 -run 'TestRT2|TestLKGP|TestG19|LKGP' ./internal/strategy/ ./internal/router/`
   → `ok internal/strategy 2.805s`, `ok internal/router 1.396s`
2. `go test -count=1 -run 'TestUnknownStrategy|TestNilSignalsEmitsNoWarning|TestKnownStrategyEmitsNoWarning' ./internal/strategy/`
   → `ok internal/strategy 0.597s`
3. `go test -count=1 -run 'TestDefaultWeights16|TestScore16|TestRank16|TestModePack|TestFactor' ./internal/scoring/`
   → `ok internal/scoring 0.107s`
4. `go test -count=1 -v -run 'TestDefaultConfig|TestConfig.*[Dd]efault|TestDeadline' ./internal/config/`
   → `TestDefaultConfig --- PASS`, `ok internal/config 0.303s` (passes, but asserts nothing about a deadline default — see RT-1)
5. `go test -count=1 -run 'TestRunCombo|TestCombo|TestFusion|TestJev|TestPipeline' ./internal/router/`
   → `ok internal/router 2.379s` (combo engine tests pass; they do not cover a budget gate, because none exists — see RT-3)

Code reads (grep + file reads) for every row; file:line evidence below.

## Evidence

### RT-1 — NOT-LANDED
- `internal/config/config.go:1334-1358` `DefaultConfig()` sets no `Routing` field at all — no `DeadlineBudgetMs: 300000` anywhere in the repo (repo-wide grep for `300000` returns zero hits).
- `internal/config/config.go:1024` declares `DeadlineBudgetMs int64`; the doc comment at `:871-872` still says "0 = disabled".
- `internal/config/config.go:1536-1537` Validate only rejects negatives; no defaulting.
- `cmd/codvian-router/main.go:2931-2932` applies the budget only `if routing.DeadlineBudgetMs > 0`, so on defaults the engine deadline is unset and `internal/router/router.go:1960-1964` (`withDeadline` returns ctx unchanged when unset) leaves the GAP-TRACK worst case (5 attempts × 120 s = 600 s) intact.
- `TestDefaultConfig` passes but does not assert any deadline default, so the passing test does not contradict this verdict.

### RT-2 — PASS
- TTL: `internal/strategy/affinity.go:129` `const LKGPRecordTTL = 15 * time.Minute`; `lkgpRecordFresh` at `:132-137`; `Lookup` (`:141-152`) reports expired pins missing; `Load` (`:234-235`, `:267-272`) drops expired records on restore; `OrderByLKGP` (`:279-297`) goes through `Lookup`, so expired pins never hoist. `Latest` likewise skips expired (covered by `TestRT2LatestSkipsExpiredPins`).
- Clear on failure: `Clear` at `affinity.go:155-159`; `ClearIfProvider` at `:169-189` (provider-scoped, returns bool). Engine wiring: `internal/router/router.go:2760-2810` `isHardLKGPFailure` (408, >= 500, timeout, transport error) + `invalidateLKGP`; called from `noteFailure` paths (`router.go:3762`, `:3821`) and stream paths (`:5276`, `:5305`, `:6069`, `:6093`).
- Tests green (command 1 above), including `TestRT2LookupExpiresAfterTTL`, `TestRT2LoadDropsExpiredPins`, `TestRT2ClearIfProviderOnlyClearsItsOwnProvider`, `TestRT2EngineFailureClearsPinEndToEnd`, `TestRT2EngineTimeoutClearsPinEndToEnd` (`internal/strategy/rt2_lkgp_test.go`), and `TestRT2NoteFailureInvalidatesPinOnHardFailuresOnly` (`internal/router/rt2_lkgp_test.go:38`).

### RT-3 — NOT-LANDED
- `internal/router/combo.go:196-276` `runComboStrategy` contains no budget logic at all: no reference to `RequestBudget`, priced estimates, `402`, refuse or trim on budget. The only trimming is the `MaxAttempts` cap (`combo.go:203-214`). `comboPanelCaller` (`combo.go:133-145`) calls `e.callChat` directly with no `budgetGate`, and `finishCombo` has no budget check. The strict-budget machinery it would need exists on the sequential path (`internal/router/requestbudget.go:1-60`, `budgetGate` in `router.go:4198, 5511`) but is never invoked from the combo path. A strict-budget fusion/jev request can therefore still spend up to 5× the budget, exactly as GAP-TRACK §10 describes.
- Combo tests pass (command 5), but none of them exercises a budget gate — the gate does not exist to test.

### RT-4 — PASS-WITH-NITS
- Real default weights: `internal/scoring/scoring16.go:378-383` `DefaultWeights16` now has `CacheAffinity: 0.0476`, `ResetWindowAffinity: 0.0476`, `Reliability: 0.03` (other thirteen rescaled ×0.8748, sum 1.0). Pinned by `TestSixteenDefaultWeightsAreRescaledForLiveFactors` (`internal/scoring/scoring16_test.go:74-93`) and `TestSixteenWeightsSumToOne` (`:125`), both green (command 3).
- Warmth plumbed: `internal/strategy/wiring.go:446-450` `sixteenInputsCtx` sets `in.CachedPromptFraction = w` from the warmth func when known; `cmd/codvian-router/main.go:1660-1680` `cacheWarmthSnapshot.warmth` backs it from the ledger's `CachedTokenFraction` (`internal/ledger/ledger.go:2346`). Reset plumbed: `wiring.go:407` `in.ResetInSeconds = time.Until(s.ResetAt).Seconds()`. Reliability plumbed: `wiring.go:381-388` measured `SuccessRate` (including measured 0) feeds `FactorReliability` (`scoring16.go:762-771`).
- NIT (does not fail the row as specified, but limits the real-world effect): the live 16-factor path ranks with mode packs — `internal/strategy/wiring.go:482` `scoring.Rank16(..., scoring.ModePack(packName))` via `sixteenHookCtx` (`wiring.go:910`) — and all six `ModePacks` still carry `CacheAffinity: 0` and `ResetWindowAffinity: 0` (`internal/scoring/scoring16.go:432, 439, 446, 453, 460, 467`; documented as verbatim the sector baseline at `:374-376`). So on the six live mode-pack strategies the plumbed warmth/reset inputs are multiplied by 0 and cannot change ranking; only `DefaultWeights16` (used for unknown packs / fallback, `scoring16.go:481-485, 805-809`) and the nonzero pack `Reliability` (0.03, 0.04 in reliability-first) actually vote. If the intent was for warmth/reset to influence mode-pack ranking, the packs need nonzero weights too.

### RT-5 — PASS
- `internal/strategy/strategy.go:500` `HookFactoryWithCacheAffinity` and `:550` `HookFactory` both `slog.Warn("strategy: unknown strategy name, falling back to priority", "strategy", strategyName, "fallback", "priority")` on `Parse` failure, then fall back to `StrategyPriority` (behaviour unchanged). Pinned by `TestUnknownStrategyLogsOneWarning`, `TestUnknownStrategyLogsOneWarningWithCacheAffinity`, `TestUnknownStrategyWarnsOncePerInvocation`, `TestKnownStrategyEmitsNoWarning`, `TestNilSignalsEmitsNoWarning` (`internal/strategy/rt5_unknown_strategy_test.go:29-148`), all green (command 2).

## Failures / nits
1. RT-1 NOT-LANDED: no 300000 default anywhere; `DefaultConfig` (`internal/config/config.go:1334`) must set `Routing.DeadlineBudgetMs = 300000` (and the "0 = disabled" doc at `:871-872` updated), plus a test asserting the default.
2. RT-3 NOT-LANDED: no panel-estimate sum / refuse-or-trim gate in `runComboStrategy` (`internal/router/combo.go:196-276`); strict-budget combo requests can still spend up to 5×.
3. RT-4 nit: `ModePacks` cacheAffinity/resetWindowAffinity still 0 (`internal/scoring/scoring16.go:432-467`) — warmth plumbing has no effect on the six live mode-pack strategies, only via `DefaultWeights16` fallback.
4. RT-2 minor scope note (not a fail): `isHardLKGPFailure` (`internal/router/router.go:2775-2794`) clears only on 408 / >=500 / timeout / transport error; 429s and other 4xx do not clear, and there is no slow-degradation (high-latency) clear trigger, although GAP-TRACK's fix text mentions "any failure class including timeouts and slow degradation". The implemented hard-failure set matches the pinned tests, so this is recorded as a scope gap for the coordinator, not a FAIL.

## Notes
- No GitHub interaction, no commits, no pushes, no edits outside this verdict file. CHECKLIST.md / parity tracker / TEST-VERDICTS.md untouched.
- The dirty working tree (HEAD 6831302) contains extensive uncommitted changes; verdicts are against the tree as found at ~19:15 IST.

---

## Fallback slice — M10-10 Cache fix (checked after T02 verdict)

- Date: 2026-11-19 IST (~19:45 IST)
- Verdict: **PASS-WITH-NITS** — the top-level Cache section exists and is wired; the earlier FAIL condition (Cache nested in Settings, page invisible) is gone from the tree. The "with-nits" is only that the dedicated Go regression tests could not be run to green here (see below) — no test-green proof from this run.

### Evidence (static, verified by reading + scripted parse of `web/index.html`)
- Top-level nav: `web/index.html:631` `<button class="nav-item" data-view="cache">Cache</button>` inside `#mainNav` (nav spans lines 547–668), a sibling of the Settings button (`:619`), not inside Settings.
- Top-level section: `web/index.html:2901` `<section class="view" id="view-cache">` with `<h1>Cache</h1>` (`:2904`), hit-rate/hits/misses KPIs (`:2922–2932`), Layers table (`:2938–2942`), savings card (`:2954–2957`), Refresh (`:2908`) and Flush (`:2912`) buttons.
- Nesting check (scripted parse): section order is `... settings (192254) → cache (220655) → webhooks ...`; `view-cache` starts exactly where `view-settings` ends — NOT nested inside Settings. Settings' own sub-nav contains only Routing / Providers / API Keys / Logging (`web/index.html:2489-2494`); the only cache mention inside Settings is a read-only "Prompt-cache pinning" status row (`:2504-2505`), not a Cache page.
- Wiring: `web/index.html:3630` titles map includes `cache:"Cache"`; `go()` at `:3675` runs `loadCachePage()` on open; `loadCachePage` (`:9217`) fetches `GET /api/cache/stats`; flush sends `DELETE /api/cache` (`:9229-9230` area, `fetch("/api/cache", {method: "DELETE", ...})`).
- Server routes registered: `internal/server/server.go:1519` `GET /api/cache/stats` → `handleCacheStats`; `:1524` `DELETE /api/cache` → `handleCacheDelete` (`:1674` also `POST /admin/cache/flush`).
- Dedicated regression tests exist: `internal/server/dashboard_m10_10_test.go` — `TestDashboardM10_10CacheRenders` (`:67`), `TestDashboardM10_10CacheNotNestedInSettings` (`:154`, whose header at `:144-149` documents the exact earlier FAIL: view-cache opened inside view-settings at old line 2457), `TestDashboardM10_10StatsEqualFeed` (`:233`), `TestDashboardM10_10FlushRoundTrip` (`:273`), plus pure-helper tests.

### Nit / caveat
- The `TestDashboardM10_10*` Go tests did NOT run to green in this session: three attempts (`go test -run 'TestDashboardM10_10...' ./internal/server/`, with and without `-vet=off`, up to 280 s each) produced no test output at all — the test binary never finished compiling/linking. The VM was heavily contended at the time (many sibling `go compile` processes for `internal/provider` / `internal/config` visible in `ps`), so this is a resource-contention timeout, not a test failure — but it is also not a PASS proof. The coordinator should re-run `go test -count=1 -run 'TestDashboardM10_10' ./internal/server/` on a quieter VM; the static evidence above is what this verdict rests on. No live-browser check was possible from this subagent (no live-browser access).
