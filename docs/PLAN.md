# PLAN.md — Codvian Router, Master Prompt v2 (6-hour finish plan)

T+0:00 = 2026-11-19 21:38 IST (v2 received in side chat). Hard stop T+6:00 = 2026-11-20 03:38 IST.
Parity at T+0:00 (parity.sh, mechanical): FULL 238 / PARTIAL 191 / MISSING 216 / WRONG 7,
comparable 652, strict 36.5%, weighted 51.2%. Approved-scope parity: not yet approved.

## Current block: Block 0 — Stabilise (T+0:00–0:20)
- [x] Verify zero live agents (subagent.list direct+descendants: active []).
- [x] Old crons: the 7 v1 crons are GONE from the scheduler (removed, not just disabled).
  Three v2 crons already exist and are enabled: v2-watchdog (10m), v2-reporter (20m),
  v2-critic (30m), goal-owned, bodies match v2 §4.5.
- [x] Scripts created: gate.sh, pkgtest.sh, commit.sh, wip.sh, parity.sh, watchdog.sh,
  status.sh. audit-fake.sh already existed (M0.4).
- [x] parity.sh first run: MATCH — reproduces 238/191/216/7 and 36.5%/51.2%.
  Rule: per-area headers, Area 14 via 14a–14d (rollup header contradicts its own
  parts on PARTIAL 41 vs 44 — A5 reconciliation item).
- [ ] First heartbeat line written (watchdog.sh test run).
- [ ] T+0:20 report posted.

## Next: Block A — Checkpoint (T+0:20–1:00)
A1 dirty tree (31 paths) · A2 benchmark JSON restore · A3 honest README ·
A4 full gate · A5 contradiction reconciliation · A6 tag checkpoint-1 + push package.
Do NOT begin Block 1 before the checkpoint tag exists.

## BLOCKED
(none yet)

---

# Previous plan (v1 era, 2026-11-18 — kept as history; the v2 plan above is current)

# docs/PLAN.md — Current milestone, wave, task list

Source of truth for what is being built now. Updated at the start and end of every wave
(master prompt §1.1). Milestones run in strict order; do not start M(n+1) features
before the M(n) exit gate passes, except tasks marked ∥ (safe to run in parallel with
the previous milestone). Sizes: S ≤ 20 min, M ≤ 60 min, L ≤ 3 h of agent time.

## Current milestone: M3 — Strategies & combos (in progress, 2026-11-18)

Status: IN PROGRESS. M0 DONE (gate 2,410 pass / 0 fail, commit 46f9091 + fix 2a8fbbb);
M1 DONE (gate 2,579 pass / 0 fail, commit 930bac3); M2 DONE (gate 2,039 top-level
PASS / 0 FAIL, commit 5072c04). M3 Phase A (S1–S4 builders) DONE and verified by
the coordinator; Phase B (wiring + conformance) DONE (gate 2,131 top-level PASS /
0 FAIL, commit 38ce88c); Phase C (data plumbing + S5 combos + conformance
extension) DONE, gate green — committing now, see the Wave M3 plan below.

| ID | Task | Size | Owner | Status |
|---|---|---|---|---|
| M0.1 | Install/verify Go toolchain. Run `go build ./... && go vet ./... && go test ./... -race -count=1`. Record the REAL test count + failures in LOG.md (replace the unverified "1,919"). | S | coordinator + parent gate | DONE 2026-11-18 (gate 2,409 pass / 1 fail → timezone fix → re-gate 2,410 pass / 0 fail, 39/39 packages; real count in LOG.md) |
| M0.2 | Create docs/GOAL.md, docs/PARITY.md (seed from §2.2 table + the report's per-area rows), docs/PLAN.md, docs/LOG.md. | S | M0.2 docs builder | DONE 2026-11-18 (this wave) |
| M0.3 | Make scripts/ci.sh (build, vet, test -race, benchmarks smoke) and scripts/bench.sh (reproducible latency/RPS/memory run, prints JSON). | S | M0.3 scripts builder | DONE 2026-11-18 (verified by coordinator: bash -n clean on both; bench.sh reuses the bench-geekflare rig, prints summary JSON, UNVERIFIED on missing fields) |
| M0.4 | Create scripts/audit-fake.sh: greps dashboard JS for hard-coded numbers/placeholder badges and lists every UI control whose backend route returns 404/501/static. Output = list of "UI without backend" (feeds PARITY as WRONG/MISSING). | M | M0.4 audit builder | DONE 2026-11-18 (verified by coordinator: re-ran ./scripts/audit-fake.sh myself — 89 routes = 89 guard table, 10 findings / 3 unverified / 1 review reproduced; output docs/audit-fake-2026-11-18.md, PRELIMINARY, re-run after gate) |

**M0 exit gate:** CI script green or failures listed; audit-fake output committed.

## Wave 1 plan — M1 P0 security + M2 telemetry (EXECUTION-READY, 2026-11-18 ~15:05 IST)

Wave 1 runs M1 and M2 together (M2 ∥ with M1 tail). Acceptance tests are written FIRST
for every M1 task; build-then-test waves, up to 20 parallel agents with DISJOINT file
ownership. Ownership below was VERIFIED against the tree by the coordinator
(2026-11-18 ~15:05 IST) — three corrections to the master prompt's Part 4 table:
(1) internal/pricing/ does NOT exist — buildLedgerPrices lives in
cmd/codvian-router/main.go:1094; (2) internal/tokens/ does NOT exist — the token
package is internal/tokencount/; (3) no internal/config/strategy* files exist —
strategy config lives in internal/config/config.go + internal/strategy/strategy.go.
Guard-table numbers verified: 89 routes, 54 AuthExempt (grep internal/server/guard.go).

SEQUENCING: Phase A first — A1, A2, A3, A4, A5, A6, A7 (all disjoint, see table).
Phase B after A1 merges — A8, A9 call-site wiring (they write their NEW files during
Phase A but touch server.go/guard.go only after A1 lands). SHARED-FILE RULE:
internal/server/server.go is touched ONLY by A1 in Phase A; internal/config/config.go
ONLY by A7 (strategy region). No two agents on the same file at once. Coordinator is
the only merger and verifies the tree green between waves. Wave 1 starts only after
the M0 gate passes and M0 is committed (one commit per task).

| Agent | Owns (verified paths) | Tasks |
|---|---|---|
| A1 | internal/config/gatewaykey.go (+tests), internal/server/guard.go (89-row table), internal/server/keystatus.go (audit first: rotation already FULL per PARITY — build disable + revoke-all only), route-registration region of internal/server/server.go | M1.1, M1.2, M1.3 |
| A2 | internal/cache/* (ExactKey in cache.go) | M1.4, M1.5 |
| A3 | cmd/codvian-router/main.go — buildLedgerPrices ONLY (:1094, called :1220/:1527) + internal/ledger/* Prices plumbing | M1.6 |
| A4 | internal/guardrails/* (PEM rule in guardrails.go); streaming/tool-args call-site wiring sequenced post-A1 if it touches server.go | M1.7 |
| A5 | internal/mcptools/* (stdio.go, mcptools.go — verify cited lines at build time) | M1.8 |
| A6 | internal/tokencount/* (CountPrompt lives here — verify at build time) | M1.9 |
| A7 | internal/strategy/strategy.go + internal/config/config.go strategy region ONLY (~:127, ~:507-509, ~:1366) + internal/router/tree.go default at :126 (minimal diff) | M1.10, M1.11 |
| A8 | internal/server/headers*.go (NEW) + tests; call-site wiring after A1 merges | M2.1, M2.2 |
| A9 | internal/observability/* (NEW); /metrics route registration after A1 merges | M2.3, M2.4 |
| A10 | scripts/*, docs/* | M0 leftovers (audit re-run, LOG.md test count when gate lands) + PARITY/PLAN/LOG updates with each feature |
| T1–T3 | tests only, in *_test.go / tests/ | independent verification agents who did NOT write the feature |
| R1 | read-only reviewer | checks every DONE against §0.3, flags fake work |

### M1 — P0 security & correctness (≈ 1 day or less; highest value per hour)

| ID | Task | Acceptance test (write FIRST) | Size | Status |
|---|---|---|---|---|
| M1.1 | Enforce scopes: wire HasScope into route registration/handlers for every /v1/* and admin route; scope table drives it (89-row guard table already exists). | Key without scope → 403 on every guarded route; key with scope → 200. Table-driven test over the whole guard table. | M | DONE 2026-11-18 (requireScope on every /v1 route; "admin" scope on all Tier 2 + mutating /api + dashboard POSTs; table-driven test over 90 guard-table routes; verified by coordinator — see M1.2 fallout note) |
| M1.2 | Close the 8 mutating AuthExempt POSTs (and review all 54 exempt routes; keep only genuinely public ones: health, openapi, static). | Unauthenticated POST to each → 401. | S | DONE 2026-11-18 (AuthExempt removed from exactly the 8 POST /dashboard/* routes; remaining 46 exempt routes all GET, read-only; verified by coordinator. FALLOUT: 39 pre-existing server tests asserting the old unauthenticated behaviour now fail (24×401, 15×403) — fix agent F1 queued; dashboard JS must send Authorization for the 8 POSTs — M10 follow-up) |
| M1.3 | Per-key disable / revoke / rotate + revoke-all; bootstrap flow stays working. | Disabled key → 401 immediately (no restart); audit entry written. | M | DONE 2026-11-18 (GatewayKey.Disabled + VerifyGatewayKeyDisabled; POST /admin/keys/{id} {"disabled":bool} with persist+rollback + audit; DELETE /admin/keys/{id} with typed phrase; rotation preserves Disabled; guard table now 90 rows, tiers 9/15/66; verified by coordinator. Leftover: routerKeyView doesn't surface Disabled yet — close-out; bootstrap key has only ["chat","models"] so it cannot call admin routes — close-out; disabling your only key locks the API, recovery is editing the config file directly) |
| M1.4 | Cache key = hash(gateway-key-id/tenant, model, messages, tools, temperature bucket, response_format, relevant params). Never cache temperature>0 unless explicitly allowed per-key; never cache finish_reason=length; add X-Codvian-Cache correctness. | Two keys, same prompt → no cross-hit; temp 1.0 vs 0 → separate; truncated answer not stored. | M | DONE 2026-11-18 (ScopedKey + ShouldStore wired into degrade.go/relay.go serving path by W1; 47 pass / 0 fail in internal/cache + W1 serving-path tests green; gate green. Caveat: semantic paraphrase layer still model-scoped — M6 work) |
| M1.5 | Cache persistence option (disk/SQLite) + POST /admin/cache/flush + per-key flush + savings accounting for own-cache hits (tokens, $ saved). | Restart keeps entries when enabled; flush works; savings row written on hit. | M | PARTIAL 2026-11-18 (PersistentCache + flush handler + RecordHit built + tested and WIRED end-to-end by W1 — flush route live, RecordHit on own-cache hits, gate green; REMAINS: serve-mode PersistentCache wiring (main.go + config) still missing, so a real server restart still loses entries — follow-up queued) |
| M1.6 | Fix buildLedgerPrices to carry CachedInputUSDPer1M; ledger and dashboard must show identical spend for same request. | Golden test with cached tokens: ledger cost == dashboard cost. | S | DONE 2026-11-18 (ModelPrice gained CachedPromptPerToken + CachedKnown; Prices.CostWithCached used by normalizeUsage; buildLedgerPrices carries the rate at pair/model/synced layers; 127 pass / 0 fail under -race in ledger + cmd/codvian-router, verified by coordinator) |
| M1.7 | PEM redaction: match BEGIN…END block fully incl. footer; apply to streaming responses and tool args. | Fuzz test with upper/lower/mixed 64-char lines + multi-chunk SSE. | M | DONE 2026-11-18 (whole-block PEM scanner + StreamRedactor wired into both stream paths by W1; 47 pass / 0 fail in internal/guardrails + split-PEM stream tests green; gate green) |
| M1.8 | MCP client: fix timeout nil-pointer (guard s.cmd after kill) and 64-char name-collision loop (deterministic suffix, bounded). | Race/timeout test; collision test with 1k colliding names terminates. | M | DONE 2026-11-18 (nil-safe killLocked on all startLocked error paths; Discover "__<n>" suffix truncated to 64 chars, bounded 10k tries + SHA-256 fallback; 23 pass / 0 fail under -race in internal/mcptools, verified by coordinator) |
| M1.9 | Token counting: fix array-form double count; legacy-model encoding fallback; cap input size; make BPE linear-ish (cache merges / heap) — keep drift table honest. | Tests with string vs array content equal counts; 1 MB input under N ms. | M | DONE 2026-11-18 (array-form now equals string-form; legacy models fall back to cl100k_base with Fallback=true, never error; exact BPE capped at 4 MiB, above that chars/4 Estimated; 1 MiB realistic prose 333–623 ms, slowest non-race run 1.40 s; drift 0.000% chunked vs unchunked; 22 pass / 0 fail under -race in internal/tokencount, verified by coordinator) |
| M1.10 | code strategy: either implement or remove from declared list consistently (config must accept what is declared). | Config round-trip test for every declared strategy name. | S | DONE 2026-11-18 ("code" already implemented in strategy.go; config validation now accepts it global + per-chain with JSON round-trip; 326 PASS / 0 FAIL in strategy+router, verified by coordinator) |
| M1.11 | Default strategy → priority (declared order) while keeping balanced available. Migration note in docs. | Default config routes in declared order; balanced still selectable. | S | DONE 2026-11-18 (priority declared first, config-accepted, tree + router + feed defaults → priority by W1; unknown → priority; strategytree allowlist includes priority + code; migration note in docs/LOG.md; gate green) |

**M1 exit gate:** all M1 tests green; scripts/audit-fake.sh shows 0 exempt mutating routes;
PARITY areas 4, 6, 7, 9, 10, 11 rows flipped honestly.

### M2 — Per-response telemetry + observability (cheap, high trust) ∥ with M1 tail

| ID | Task | Size | Status |
|---|---|---|---|
| M2.1 | Single choke-point response-header writer (stream and non-stream, errors included): X-Codvian-Request-Id, -Version, -Provider, -Model, -Cost-USD (10 dp), -Tokens-In, -Tokens-Out, -Tokens-Cached, -Latency-Ms, -Upstream-Latency-Ms, -Router-Overhead-Us, -Tokens-Per-Second, -Fallback-Attempts, -Fallback-Path, -Cache (+ similarity/saved-tokens/saved-USD), plus existing Decision/Budget headers. Streaming: send what is known at start in headers, rest in a trailer/final SSE comment and in the log row. | M | DONE 2026-11-18 (choke-point writer + ResponseMeta wired into chat/embeddings/rerank/responses/messages, stream + non-stream + errors; 14 contract tests pass; full server package PASS under -race, verified by coordinator. Leftover: no trailer on committed-error streams (pre-existing tests forbid provider ids / "fallback" in stream error bodies); no full telemetry headers on GET /v1/models + dashboard/admin paths — request-id/version only) |
| M2.2 | Correlation id propagation (accept inbound X-Request-Id, echo, log everywhere). | S | DONE 2026-11-18 (withLogging middleware accepts + echoes X-Request-Id; on access-log + stream success log rows; 14 contract tests pass incl. TestCorrelationIDPropagation, verified by coordinator. Leftover: request_id not on every scattered logger.Printf line) |
| M2.3 | Structured JSON logs + /metrics (Prometheus): per-provider/model p50/p95/p99, error classes, fallback counts, cache hit ratio, tokens, cost, breaker state. | M | DONE 2026-11-18 (Collector + Logger + Prometheus exposition wired into the request path: /metrics registered at internal/server/server.go:1288, obsCollector created at :1159, recordTelemetry at the A8 choke points in internal/server/telemetry.go; per-(provider,model) p50/p95/p99 + codvian_router_cost_usd_total; 13 pass / 0 fail under -race in internal/observability; M2 gate green. Known limitation: duplicate HELP/TYPE blocks when both recorder and collector present — Prometheus tolerates it) |
| M2.4 | OpenTelemetry traces (optional build tag, off by default, zero overhead when off). | M | DONE 2026-11-18 (dependency-free in-process tracer behind otel tag; off-by-default no-op proven by build-tag tests; spans codvian.request → auth → route → upstream → stream as one tree; 13 pass / 0 fail under -race, verified by coordinator. Leftover: OTLP export NOT included — no SDK in go.mod, needs a config decision; spans are in-memory only) |

**M2 exit gate:** header contract test; 100% of response paths covered (chat, embeddings,
rerank, responses, messages, errors, SSE). — MET 2026-11-18, commit 5072c04
(2,039 top-level PASS / 0 FAIL, 40/40 packages; PARITY FULL 141, strict 20.4%,
weighted 34.2%; P0-8 CLOSED).

## Wave M3 plan — Strategies & combos (STARTED 2026-11-18 ~17:00 IST)

Area 1 baseline: 63 rows, FULL 3, PARTIAL 24, MISSING 35, WRONG 1; strict 4.8%,
weighted 23.8%. Ours: 7 named strategies (priority, balanced, cheapest, fastest,
most-reliable, least-latency, weighted, code) vs sector-typical 22 + mode packs +
auto/* family. Exit gate: strategy conformance suite green; Area 1 ≥ 80% weighted.

SEQUENCING: Phase A — S1–S4 build in parallel, DISJOINT new files only, NO edits to
shared files (internal/config/config.go, internal/router/router.go,
internal/server/server.go, internal/strategy/strategy.go — coordinator wires those
in Phase B). Each builder writes acceptance tests FIRST (test fails when the
strategy is removed), reads sector baseline semantics from the cited reference files
(study only — never copy code), and reports file:line evidence + PARITY rows.
Phase B — coordinator wires registry/config/router, runs the conformance suite +
full gate, flips PARITY rows, commits M3. Phase C — S5 combos (fusion/pipeline/
jev/quota-share/chaos) + S6 conformance suite, same pattern.

| Agent | Owns (NEW files only) | Tasks |
|---|---|---|
| S1 | internal/strategy/orderings.go + orderings_test.go | round-robin (stateful), random, strict-random (deck), fill-first, least-used, p2c; fix `weighted` WRONG-NAME (per-target weight draw, reference targetSorters.ts:28) |
| S2 | internal/strategy/quota.go + quota_test.go | quota-weighted (draw score/(1+load)), headroom (1 − max(util5h, util7d)), expiry-first, reset-aware, reset-window — pure ordering over a QuotaSnapshot interface (no ledger writes) |
| S3 | internal/scoring/scoring16.go + scoring16_test.go | 16-factor auto scoring (add the 11 missing factors) + 6 mode packs (ship-fast, cost-saver, quality-first, offline-friendly, reliability-first, chaos-mode weights) |
| S4 | internal/strategy/affinity.go + affinity_test.go | lkgp (last-known-good-provider stickiness), context-optimized (largest context first), cache-optimized (affinity ordering), context-relay |
| S5 | (Phase C) internal/router/panel.go + internal/autorouter/* | fusion, pipeline, jev, quota-share, chaos-mode, auto/* family, candidate-pool endpoint, 4-tier cascade, endpoint-specific combos |
| S6 | (Phase C) tests only | Strategy conformance suite: every named strategy orders a fixed fixture deterministically; config round-trip for every declared name |

Phase A DONE 2026-11-18 (verified by coordinator, package tests green):
S1 orderings (15 tests), S2 quota (16 tests), S3 16-factor + packs (27 tests),
S4 affinity (24 tests, -race). Phase B DONE (same day, gate running):
wiring.go + conformance suite (wiring_test.go, 10 tests), registry/config/tree
allowlists extended to 31 strategies, "weighted" renamed to sector-typical
per-target draw with old custom-weights behaviour moved to "custom-weights"
(pre-1.0 rename, migration note in LOG.md). Phase B honest verdicts: 14 rows →
FULL (Area 1 FULL 3 → 17), 11 rows → PARTIAL (data-limited wiring), WRONG 1 → 0.
Area 1 after Phase B: FULL 17, PARTIAL 27, MISSING 19, WRONG 0; strict 27.0%,
weighted 48.4% (exit gate needs ≥ 80% weighted — NOT met yet; Phase C closes it).

Phase C DONE 2026-11-18 (coordinator wiring + 5 parallel builders, all verified
individually before merge): C1 quota signals (ledger.PairQuota + Signals
Attempts/Load/ResetAt; Util5h/Util7d stay -1 — no per-pair limit exists — and
ExpiresAt stays zero — no expiry data exists); C2 LKGP registry + persistence +
per-chain state; C3 chain per-target weights (routing.chain_weights); C4 fusion /
pipeline / jev primitives (panel.go); C5 auto/* variants + candidate-pool
handler + quota-share. Coordinator wiring: Signals → hooks (Attempts, Load,
ResetAt, Util, 16-factor ResetInSeconds), per-chain round-robin/deck
(DefaultChainStates), LKGP registry keyed by requested model with RecordSuccess
on every successful inference (recordTelemetry) + Load at startup / Save at
shutdown (lkgp.json), chain weights via SetChainWeights, PairQuota 10 s TTL
cache (one scan per pair per window, not one per candidate per request),
SCHEMA.md fixed (31 strategy names, chain_weights, custom-weights correction),
panel_test.go gofmt nit fixed. Phase C honest verdicts: 6 rows → FULL
(weighted, least-used, quota-weighted, reset-window, lkgp, Router-strategy
lkgp), 14 rows → PARTIAL (fusion, pipeline, jev, quota-share, candidate-pool
endpoint, auto/offline, auto/lkgp, auto/chaos, auto/subscription, auto/thrifty,
auto/best-*, auto/pro-*, 4-tier cascade, endpoint-specific combos). Area 1
after Phase C: FULL 23, PARTIAL 36, MISSING 4, WRONG 0; strict 36.5%, weighted
65.1% (exit gate needs ≥ 80% weighted — NOT met yet; Phase D closes it).
Headline: FULL 155 → 161, PARTIAL 194 → 203, MISSING 303 → 288, WRONG 7;
strict 22.4% → 23.3%, weighted 36.5% → 38.0%.

Phase D DONE 2026-11-18 (engine integration; D1/D2/D3 in parallel, D2X, then D4,
then D5, each builder → different reviewer → coordinator merge; all review
nits folded in by the coordinator): D1 candidate-pool route registered with the
live pool (breaker states, per-key connections, cooldowns; free/pro + category
filters from the live price book + tier membership — allowlist exclusions have
no store, ModelLocked always false, FreeAccessExclusion always nil,
subscription/lkgp 503 fail-closed); D2 auto/* engine dispatch (autodispatch.go,
18 tests; live catalog + PriceBook + provider latency/success stats; auto/smart
10% exploration seed-deterministic; chaos panel + chaos-mode handed to D3);
D2X dispatch smoke test (11 auto/* ids through Engine.Chat) + LKGP global-last
wiring (Latest() + SetAutoLastGood installer, e2e); D3 fusion/pipeline/jev
engine wiring (combo.go, 15 tests; HeuristicJudge default, LLM judge tested
but unwired — no judge model configured; JEV hold is a verdict row; stream
replays the combo result verbatim; chaos parallel fan-out proven,
fault-injection NOT implemented — M4 chaos harness); D4 quota-share DRR +
cascade helper + endpoint projection (cascade.go; DRR mirrors strategy.applyDRR
exactly; cascade helper tested but unwired — no billing/tier signal; projection
enforced at all three live entry points, fail-closed, unknown kinds error);
D5 context-relay engine integration (relay.go mirror + Config seams, 14 tests;
in-request on-error relay live; no production summariser/quota signal, no
cross-request store, chat-format only). Coordinator fixes: unknown EndpointKind
fails closed (+1 test); relayNote gated on res.called (+1 test, toggle-verified);
QuotaShareFor non-reentrancy documented; tree-ordered quota-share errors
(deferred — needs tree-allowlist + rankAttempts change). Phase D honest
verdicts: 11 rows → FULL (fusion, pipeline, jev, auto/coding, auto/fast,
auto/cheap, auto/smart, auto/lkgp, auto/offline, auto/chat,
auto/<category>:<tier>). Area 1 after Phase D: FULL 34, PARTIAL 25, MISSING 4,
WRONG 0; strict 54.0%, weighted 73.8% (recounted mechanically from the PARITY
rows, not copied). Headline: FULL 161 → 172, PARTIAL 203 → 192, MISSING 288,
WRONG 7; strict 23.3% → 24.9%, weighted 38.0% → 38.8%.
M3 exit gate NOT met: Area 1 needs ≥ 80% weighted; Phase D reaches 73.8%.
The residual is data-source gaps, not wiring: billing class, premium
classification, candidate-pool exclusions, 16-factor pool scoring, production
summariser + quota-use signal, cross-request session store, P2C + saturation
gating for quota-share. Remaining MISSING (4): account-level fallback subset,
sla-aware, nadir, preferred/reserve accounts. Honest blocks (product
decisions, not code gaps): headroom needs a per-pair limit source; expiry-first
needs a quota-expiry source. M4 is ∥ M3 per the roadmap, so M4 chains
immediately without waiting on the 80% number.

## Wave M4 plan — Failover (STARTED 2026-11-18 ~19:35 IST)

Area 2 RECOUNTED mechanically from docs/PARITY.md rows (2026-11-18, coordinator):
46 rows = FULL 14, PARTIAL 16, MISSING 13, WRONG 0, AHEAD 3; comparable
total 43 (the 3 AHEAD rows are NOT in the denominator per the PARITY.md rule).
Strict 14/43 = 32.6%, weighted (14+8)/43 = 51.2%. The section header's FULL 13
was stale (M2 flipped the fallback-attempts header row to FULL and the header
was never updated); the per-area table's Total 46 / 30.4% / 47.8% counted the
3 AHEAD rows in the denominator. Both are corrected in the M4 commit.
Exit gate: chaos harness proving 0% client-visible error rate for a
single-provider outage with ≥ 3 healthy fallbacks; added failover latency ≤
detection time + 2 ms; hedge cuts p99 ≥ 50% on a slow-provider scenario.

CONSTRAINT (coordinator is a depth-2 subagent, cannot spawn subagents): slices
below are executed sequentially by the coordinator itself, one at a time, with
tests written per slice (test fails when the feature is removed), compile-check
after each slice, and ONE full gate at the end of the wave (build-first-test-last).
Review discipline: every slice gets a toggle-verified test (fails with the
feature removed, passes with it) before merge; no file is ever moved, deleted,
renamed, or set aside, even temporarily.

| ID | Slice | Files (new unless noted) | Area 2 rows targeted |
|---|---|---|---|
| M4.1 | Chaos harness + fault injection | internal/chaos/*, internal/router/chaos_test.go | chaos-mode fault-injection (deferred from D3); exit gate |
| M4.2 | Breaker per-profile thresholds + backoff escalation + provider-level failure-window breaker + DEGRADED/HALF_OPEN states | internal/router/breaker_profile.go, internal/router/providerbreaker.go, router.go breaker funcs | breaker thresholds/cooldowns per auth profile; backoff escalation; provider-level failure-window breaker; breaker states |
| M4.3 | Model lockout (off by default; 403/404/429/502/503/504; base 120 s, max 30 min, max 10 steps) | internal/router/lockout.go, router.go noteFailure + loop | model lockout |
| M4.4 | Retry-After from JSON body + 429 quota-text distinction + concurrency-limit 429 distinction | internal/wire/retryable.go, internal/provider/retryafter_body.go, internal/provider/client.go, router.go noteFailure | Retry-After from JSON body; 429 quota-text; concurrency-limit 429 |
| M4.5 | Same-host transient retry (502/503/504, max 3, decorrelated full-jitter, base 200 ms / cap 2,000 ms) | internal/router/transient.go, router.go Chat loop | same-host transient retry |
| M4.6 | Throughput watchdog on stalled streams (30 s warmup / 30 s window / 4 B/s) | internal/router/watchdog.go, router.go forwardStream | throughput watchdog |
| M4.7a | Predictive TTFT breaker (skip target whose avg TTFT exceeds predictiveTtftMs; min 5 samples) | internal/router/ttft.go, router.go runStream + loop | predictive TTFT breaker |
| M4.7b | Token-refresh breaker (Vertex OAuth2 mint: 5 failures, 30 min cooldown, provider-wide) | internal/provider/vertex_client.go | token-refresh breaker |
| M4.7c | Per-target timeout (default 120 s) | router.go Config + callChat + stream start | per-target timeout |
| M4.7d | Identical-error early stop (3 consecutive identical model-shape 400s) | router.go Chat loop | identical-error early stop |
| M4.8 | Hedge body-size guard (>256 KB never hedged) + MaxAttempts clamp [1,200] + exhausted-provider skip set + stream-recovery early-retry max 4 + preferred-account response-start failover (opt-in, 20 s, one sibling) + opt-in wait budgets (90 s / 5 / 300 s) | router.go loop/hedge/preread regions, internal/router/waitbudget.go | hedging body-size guard; per-request attempt cap; exhausted-provider skip; stream recovery holdback; preferred-account response-start failover; wait budgets |
| M4.9 | Config surface: routing.max_attempts, per_target_timeout_ms, predictive_ttft_ms, model_lockout block, wait_budgets block; AuthProfileFor wiring from registry auth kind | internal/config/config.go, cmd/codvian-router/main.go | (supports M4.2/M4.3/M4.7/M4.8 rows) |
| M4.10 | Divergences recorded, NOT built: 402 retryable (deliberate — billing/quota is per-target); mid-stream continuation (deliberately removed); admission queue (deliberate — Gradient2 sheds instead of queueing) | docs/PARITY.md rows → DIVERGENCE | 402; mid-stream continuation; admission queue |

Deliberate non-goals (do NOT rebuild): mid-stream continuation, admission queue.
If any other gap turns out to be a deliberate divergence, record it as such in
PARITY.md, do not build it.

## Wave M4 close-out — Failover (DONE 2026-11-18; gate GREEN)

- Gate: `./scripts/ci.sh` → CI: PASS (log /tmp/m4-ci2.log); count run
  (`go test ./... -race -count=1 -v`, log /tmp/m4-gate-verbose.log): 2,389
  top-level PASS / 0 FAIL (3,041 incl. 652 subtests), 2 SKIP, 41/41 packages
  + 2 no-test-files (Phase D was 2,312 top-level / 2,932 incl. subtests,
  40 packages; +77 top-level, +109 total — reconciles: 76 new test funcs
  in M4 files + TestMapStatusError429BodyRetryAfter in client_test.go; +32
  subtests from table-driven cases; new package internal/chaos). The 15
  `FAIL` lines in the log are stdout from release-verification tests
  asserting tampered/unsigned fixtures fail verification — zero `--- FAIL`,
  zero failed packages, same known pattern as M2/M3. Gate history (honest):
  ci1 failed on gofmt (8 M4 files) + 1 server test
  (TestChatCompletionsStreamingPrereadGate/empty_stream_fails_over — the
  M4.8 early-retry change, dead hits 1 → 4); gofmt -w + the same
  expectation update the router-package tests already got (dead 1 → 4,
  trace 2 → 5, ledger +3 dead rows); ci2 PASS.

All 10 slices built sequentially by the coordinator (depth-2, no subagents;
toggle-verified tests per slice, compile-check after each slice, ONE full gate
at the end). M4.1 chaos harness: 0 errors over 100 requests with a dead-first
provider + 3 healthy fallbacks; added failover latency 12.9 µs vs the bound
(detection p50 110 ns + 2 ms); hedge cut p99 69.9% (501.1 ms → 150.7 ms) on
the slow-provider scenario — exit gate MET. M4.2 per-profile breaker thresholds
+ escalation + window breaker + DEGRADED/HALF_OPEN reporting (persistence still
missing — breaker state is in-memory only, recorded honestly). M4.3 model
lockout. M4.4 Retry-After from JSON body + quota-text/concurrency 429
distinctions. M4.5 same-host transient retry. M4.6 throughput watchdog. M4.7a–d
predictive TTFT breaker, Vertex token-refresh breaker, per-target timeout,
identical-error early stop. M4.8 hedge guard + attempt clamp + exhausted skip +
stream early-retry max 4 + preferred-account failover + wait budgets (reserve
guard `attemptsLeaveOne` found via real regressions: a same-target retry must
leave one attempt for the next target). M4.9 config surface
(routing.max_attempts, per_target_timeout_ms, predictive_ttft_ms,
model_lockout, wait_budgets, preferred_failover_after_ms; AuthProfileFor from
the registry auth kind) + Clone deep-copy of the new pointer blocks. M4.10
three divergences recorded (402, mid-stream continuation, admission queue).
PARITY Area 2: FULL 14 → 33, PARTIAL 16 → 7, MISSING 13 → 0, AHEAD 3,
DIVERGENCE 0 → 3; comparable 40; strict 32.6% → 82.5%, weighted 51.2% →
91.3% (recounted mechanically; Area 12 duplicates rows 14–16 flipped with
them). Remaining PARTIAL (7): 200-OK body predicate, breaker persistence,
connection cooldown, deadline-failure sub-budget, 3-attempt cross-provider
default, failover-before-retry knobs, fallback-chains CRUD API. Headline naive
sum NOT rewritten (pre-existing all-area recount divergence, out of scope);
M4 note added under the headline + per-area table instead.
Next: M5 budgets, quotas, pools — chains immediately.

## Wave M5 plan — Budgets, quotas, pools (STARTED 2026-11-18)

Area 5 RECOUNTED mechanically from docs/PARITY.md rows (2026-11-18, coordinator):
30 rows = FULL 7, PARTIAL 6, MISSING 15, WRONG 0, AHEAD 2; comparable
total 28 (the 2 AHEAD rows are NOT in the denominator per the PARITY.md rule).
Strict 7/28 = 25.0%, weighted (7+3)/28 = 35.7%. The section header's 23.3% /
33.3% counts the 2 AHEAD rows in the denominator (same stale convention the
M4 commit corrected for Area 2); corrected in the M5 commit. The Area 5
admission-queue row flips to DIVERGENCE in the M5 commit (decided M4.10;
Area 2 + Area 12 duplicates already flipped) → comparable 27 at M5 start.
Exit gate: concurrency race tests prove budgets cannot be exceeded by
parallel requests. That gate DEPENDS on M5.8 (committed-spend reservation):
today CheckKeyBudget reads spent-then-proceeds, so parallel requests can
all pass before any ledger row lands — the reservation is the fix, the
race tests are the proof. M5.14 runs last.
sector baseline evidence verified fresh 2026-11-18: apiKeys.ts fields
(daily/weekly/fiveHourUsageLimitUsd, maxRequestsPerDay/Minute,
throttleDelayMs, maxSessions, accessSchedule, rateLimits[]);
keyQuota.ts (pre-request gate tpm/rpm/monthly, fail-open on infra error,
post-response increment, calendar-month UTC, unpriced month fails closed);
fairShare.ts (work-conserving pools, hard/soft/burst, saturation 0.5,
absolute cap always enforced — DISTINCT from M3's chain-selection DRR);
budgetGuard.ts (InternalBudgetLimit: global/provider/model/pool ×
currency/tokens/requests × daily/weekly/monthly, allow/warn/deny).

CONSTRAINT (coordinator is a depth-2 subagent, cannot spawn subagents): slices
below are executed sequentially by the coordinator itself, one at a time, with
tests written per slice (test fails when the feature is removed), compile-check
after each slice, and ONE full gate at the end of the wave (build-first-test-last).
Review discipline: every slice gets a toggle-verified test before merge; no file
is ever moved, deleted, renamed, or set aside, even temporarily.

| ID | Slice | Files (new unless noted) | Area 5 rows targeted |
|---|---|---|---|
| M5.1 | Windowed per-key USD budgets: calendar daily/weekly/monthly (UTC) + custom resetInterval/resetTime; unpriced-month fail-closed | internal/ledger/keybudget.go (+ windowed tables), internal/config/gatewaykey.go | per-key daily / weekly / monthly USD; custom windows |
| M5.2 | Per-key request limits: RPD + generic {limit, window} rate-limit rules | internal/server/keytraffic.go | per-key request limits; per-key RPD |
| M5.3 | Per-key token limits: per-model/per-provider/global scoping + daily token cap; TPM post-response true-up (admission estimate replaced by billable actuals) | internal/server/keytraffic.go, internal/ledger | per-key token limits; per-key TPM |
| M5.4 | Fair-share quota pools: pool members (keys) with weight/cap/policy (hard/soft/burst), saturation threshold, absolute cap; pre-request gate | internal/ledger/poolshare.go (new), internal/server | quota-share pools |
| M5.5 | Per-account limits (upstream account quota) + accountQuotas in me/status | internal/ledger, internal/server/keystatus.go | per-account limits; (part of) me/status |
| M5.6 | Usage reporting: me/status daily/weekly spend + list-price cost; GET /v1/me/sessions | internal/server/keystatus.go, internal/server | me/status; me/sessions |
| M5.7 | Usage/budget management API: GET/POST /api/usage/budget, GET /api/usage/om-usage | internal/server | usage/budget management API |
| M5.8 | Committed-vs-settled spend: pre-send estimate reserved as committed, settled on response with actuals (the race-gate mechanism) | internal/ledger/budget.go, internal/ledger/keybudget.go | committed-vs-settled spend |
| M5.9 | Quota schedules: time windows allow/block, keep-free floor, quota.exceeded webhook | internal/ledger/schedules.go (new), internal/server | quota schedules |
| M5.10 | Per-key access schedule / throttle delay / max sessions | internal/config/gatewaykey.go, internal/server/keytraffic.go | per-key access schedule / throttle / max sessions |
| M5.11 | Internal scoped budgets: global/provider/model/pool × currency/tokens/requests × daily/weekly/monthly, allow/warn/deny + warningThreshold | internal/ledger/scopedbudget.go (new) | internal scoped budgets |
| M5.12 | Team cost centers + team budgets: teams as key groupings, 1d/7d/30d USD limits, cost-center reporting (data model + enforcement + API; dashboard UI is M10) | internal/ledger/teams.go (new), internal/server | team cost centers + team budgets |
| M5.13 | Upstream rate-limit learning: parse x-ratelimit-*/retry-after, persist learned limits per (provider, key), pace Gradient2 from them | internal/adaptive/limiter.go, internal/provider/client.go | upstream rate limiting |
| M5.14 | Concurrency race gate (EXIT GATE): race tests proving per-key budgets, global budget, request budget, TPM/RPM cannot be exceeded by parallel requests; fix any hole found | tests only (+ fixes in ledger/server) | (exit gate) |

Deliberate non-goals: admission queue (DIVERGENCE, M4.10 — Gradient2 sheds
instead of queueing). Team management UI is M10; M5.12 covers the team data
model, enforcement, and API only.

## M5 close-out (2026-11-18) — DONE, gate GREEN (see LOG.md)

Gate: `./scripts/ci.sh` → CI: PASS (log /tmp/m5-ci.log); count run
(`go test ./... -race -count=1 -v`, log /tmp/m5-gate-verbose.log):
2,523 top-level PASS / 0 FAIL (3,320 incl. 797 subtests), 2 SKIP, 42/42
ok packages + 2 no-test-files (M4 was 2,389 / 3,041, 41 packages;
+134 top-level reconciles exactly: 124 new test funcs in new M5 test
files + 10 added in modified test files; +279 total incl. 145 new
subtests). RUN 3,322 = PASS 3,320 + SKIP 2; the 15 `FAIL` lines are
the known release-verification stdout pattern, zero `--- FAIL`.
M5 exit gate MET (see M5.14 below).

M5.1–M5.13 built per the table above (all toggle-verified by their builders).
M5.14 (exit gate) audited all four race surfaces and found two real holes,
fixed in the same wave: (1) the cumulative (legacy) cap had no reservation —
ReserveSpend returned "" with no key-budget row, so parallel requests all
passed the settled-only pre-send check; now the reservation covers the
cumulative cap and checkBudgetDetailed counts reservations as committed
spend; (2) the strict request-budget hedge gate checked the second attempt
against spentUSD (0 at idx 0), letting a hedged pair jointly exceed the
budget — both loops now gate on spentUSD + first-attempt estimate
(toggle-verified: the hedge test fails with the fix reverted, "fast"
wins). TPM/RPM verified atomic (tryAdmit holds one mutex across
check + record) and race-proved. Per-key budgets were already race-proof
via M5.8 (TestReserveSpendParallel, TestReserveSpendParallelChat).
PARITY: Area 5 FULL 8 → 27, PARTIAL 6 → 0, MISSING 14 → 0, AHEAD 2,
DIVERGENCE 0 → 1 (admission queue); comparable 27; strict 100.0%,
weighted 100.0% (recounted mechanically). Section header corrected
(it claimed FULL 7 / MISSING 15 at M5 start; the mechanical count was
FULL 8 / MISSING 14 — it had missed the per-response-headers FULL row).
Duplicates flipped: Area 4 row 363, Area 11 rows 717/720/722/723 → FULL;
Area 11 row 724 stays PARTIAL (per-window total request counts missing).
Area 8b dashboard rows for the same features NOT flipped — dashboard
pages unverified, M10's job. Known limitations: hedged second attempt
bypasses the token-limit/internal-budget loop-top skips (same as
breaker/cooldown/lockout — the ledger still counts its tokens, so the
next request is refused); strict hedge bound is prompt-estimate only
(completion side can still overshoot, same as the sequential gate);
relay cannot reserve (no token estimate — pure check, which now counts
reservations); schedule reserves measured on pool consumption, not an
upstream saturation signal (we have none); me/status has no per-window
total request counts.

## Wave M6 plan — Caching 2.0 + token-cost engine (STARTED 2026-11-18)

Area 6 recount (mechanical, from docs/PARITY.md rows, verified 2026-11-18,
recounted + corrected in the M6 commit): 35 rows = FULL 24, PARTIAL 7,
MISSING 0, WRONG 0, AHEAD 3, DIVERGENCE 1; comparable total 31
(3 AHEAD + 1 DIVERGENCE rows NOT in denominator). Strict 24/31 = 77.4%,
weighted (24+3.5)/31 = 88.7%. The pre-M6 header (42.9% / 54.3%) used
the old all-rows denominator (15/35, 19/35) — same stale convention
M4/M5 corrected for Areas 2/5. The same commit mechanically recounted
ALL areas and rewrote the headline + every per-area header/total (see
the M6 note in docs/PARITY.md).

Exit gate (PLAN.md roadmap): cache correctness property tests (no
cross-tenant, no temp leaks); savings numbers reproducible from
ledger rows. The gate DEPENDS on M6.8 (analytics surfacing) for the
savings half and M6.11 (property tests) for the correctness half.

| ID | Slice | Rows targeted |
|---|---|---|
| M6.1 | Cache caps: per-entry size cap (256 KB) + total memory cap (2 MB tier) | per-entry size cap; total memory cap |
| M6.2 | Persistence tiers: SQLite-backed exact cache (survives restart) + semantic vector persistence; also fix the flush-all semantic gap (no flush API on LocalSemanticCache) | persistence tiers |
| M6.3 | Truncated-response guard audit: the guard EXISTS since M1.4 (ShouldStore rejects finish_reason=length, store() calls it, TestM14ShouldStoreRejectsTruncated) — the PARITY row is stale. Verify ALL store paths consult it (chat, streaming-assembly, responses); flip to FULL only if proven on every path | truncated-response guard |
| M6.4 | Bypass headers (X-Codvian-No-Cache, Cache-Control/Pragma no-cache) + per-request cache controls (key/type/threshold/ttl/no-store) | bypass headers; per-request cache controls |
| M6.5 | Prompt-cache affinity routing: cache-optimized ordering/pinning | prompt-cache affinity routing |
| M6.6 | Special-purpose caches: reasoning cache (2 h TTL) + search cache (60 s, coalesced) | reasoning cache; search cache |
| M6.7 | Semantic layer: threshold override (config + per-request header) + scope (last 3 turns, not just last user message) | semantic threshold + override; semantic scope |
| M6.8 | Cache analytics: stats endpoint + tokens_saved surfacing (dashboard page is M10) | cache analytics |
| M6.9 | Token-cost engine: cache-write tiers 5m/1h split + provider prompt-cache markers (extend beyond anthropic/claude/openrouter) | cache-write tiers; provider prompt-cache markers |
| M6.10 | Dedup/coalescing: per-key isolation + in-flight cap + timeout | dedup / coalescing |
| M6.11 | Correctness property tests (exit gate, correctness half): no cross-tenant, no temp leaks, no truncated serves; cross-provider SHARING pinned as documented (M6.12 decided DIVERGENCE — same model shared across providers by design) | (exit gate) |
| M6.12 | Per-provider isolation: the row says "Deliberately NO" but no decision record was found in LOG.md/PLAN.md — audit first. If genuinely deliberate → flip to DIVERGENCE with the evidence; else add provider to ScopedKey (sector baseline default-on) | per-provider isolation |

Deliberate non-goals / evaluate-don't-force: semantic similarity method
(neural embeddings) — wiring real provider embeddings into the semantic
layer is a cost/latency design decision; evaluate in-wave, keep the
honest hashed-BoW and leave the row PARTIAL if the cost is prohibitive.
Team/dashboard UI stays M10. No fake work; every flip needs file:line +
test evidence; BUILD ALL FIRST, TEST ONCE AT THE END of the wave.

## Wave M7 plan — Compression & token efficiency

Area 10 recount (mechanical, from docs/PARITY.md rows, M6 commit): 46 rows =
FULL 8, PARTIAL 7, MISSING 27, WRONG 0, AHEAD 4; comparable total 42
(4 AHEAD rows NOT in denominator). Strict 8/42 = 19.0%, weighted
(8+3.5)/42 = 27.4%.

Exit gate (PLAN.md roadmap): round-trip tests (code/JSON intact), inflation
guard test, measured savings table committed. Gate DEPENDS on M7.1 (eval
harness + measured savings corpus) for the savings half and M7.2/M7.3
(fidelity + inflation guards) for the correctness half. Dashboard UI stays
M10; every flip needs file:line + test evidence; no fake work.

**GATE WAVE DONE 2026-11-18 (M7.1 + M7.2 + M7.2-fix + M7.3 + wiring, committed
as the M7 gate-wave commit): exit gate MET.** M7.1 harness + savings table;
M7.2 fidelity gate wired into Apply (F1 paren-URL hole found by review FAIL,
fixed, re-reviewed PASS-WITH-NITS); M7.3 inflation guard wired after the
fidelity gate (single refusal step, toggle-proven). Area 10 now 47 rows:
FULL 9 / PARTIAL 7 / MISSING 27 / AHEAD 4 (comparable 43; strict 20.9%,
weighted 29.1%). **Second wave DONE 2026-11-19 (M7.4–M7.9 + merge wave,
commit pending):** M7.4 RTK, M7.5 Caveman-lite, M7.6 modes + dial, M7.7
hard-budget post-pass + cache-aware (built, unwired), M7.8 stacked pipeline
+ worker pool, M7.9 calibration + memo audit + data-URI — all built,
reviewed (M7.4 re-review FAIL → fixed; M7.8 re-review found + fixed the
pool-Submit mutex defect; M7.9 review PARTIAL, wiring closed by the merge),
and wired into Apply/router by the merge wave. Deliberate non-wirings:
dial/auto-trigger NOT the default (explicit modes only), compression pool
NOT wired (no genuine background consumer), cache-aware NOT wired into
Apply. Area 10 now FULL 10 / PARTIAL 13 / MISSING 20 / AHEAD 4
(comparable 43; strict 23.3%, weighted 38.4%). **M8 (API breadth) is next
— spawn M8.0 + M8.1 first, then M8.2–M8.21 in parallel, M8.22 last.**

Deliberate non-goals / do-not-build (from the MUST NEVER list + M6 review):
the legacy glyph set context-as-image (lossy, exempt from the fidelity gate) and LLM
compressor (model call to compress) are NEVER built. Lossy prose rewriting
is allowed only behind the fidelity gate (M7.2) — never silent.

| ID | Slice | Rows targeted |
|---|---|---|
| M7.1 | Eval harness + measured savings corpus: repeatable harness over a fixed corpus (code, JSON, prose, tool output, tabular); per-engine saved-tokens + saved-% table committed to docs/ (re-runnable by a documented command) | eval harness + measured savings corpus |
| M7.2 | Fidelity gate / risk gate on the lossy path + protection guarantees: round-trip tests proving code fences / URLs / JSON / identifiers survive intact; lossy head/tail truncation must not cut them mid-string; fail → send original | fidelity gate / risk gate / validation; protection guarantees (code/URLs/JSON/identifiers) |
| M7.3 | Inflation guard: compressed output must never cost MORE tokens than the input (guard test: output tokens ≤ input tokens on every corpus case; violation → send original + count it) | (exit gate) |
| M7.4 | RTK tool-output filter: command-output detection, 56 JSON packs, ANSI/progress strip, failures + tails kept | RTK tool-output filter (56 JSON packs, ANSI/progress strip, failures+tails kept) |
| M7.5 | Caveman-lite rule-based prose condensation (levels lite/full, user-role default, min 50 chars): extractive + rule-based only, no model calls, behind the M7.2 fidelity gate | Caveman rule-based prose condensation (levels lite/full/ultra, user-role default, min 50 chars) |
| M7.6 | Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked) + adaptive budget dial / escalation ladder | named modes/presets + auto-trigger; adaptive budget dial + escalation ladder |
| M7.7 | Hard-budget post-pass (compress to ≤ N tokens, honest when pinned messages alone don't fit) + cache-aware compression (preserve system prompt, downgrade, prefixFreeze) | hard-budget post-pass; cache-aware compression |
| M7.8 | Stacked pipeline RTK → Caveman + worker pool / async compression (off the request hot path where safe) | stacked pipeline RTK → Caveman; worker pool / async compression |
| M7.9 | Token-counting gaps (Area 10, counting half): runtime calibration vs provider-reported usage; Count memoization/perf audit; base64 data-URI inside text strings | runtime calibration vs provider-reported usage; count memoization / perf; base64 image data-URI exclusion |

Deferred (not in this wave, record the reason): SmartCrusher tabular
columnar + Ionizer row sampling (needs a tabular corpus + a columnar
round-trip proof — queue behind M7.1 harness, do not force); Codex
Responses tool-output compression (Responses-surface only, after M7.4
proves the filter); LLMLingua-2 ONNX pruning (~57 MB TinyBERT — binary
size + model-download cost vs a rule-based gate; evaluate, don't force);
aggressive summarize + progressive aging (needs a summmariser — the LLM
compressor is banned, so this is extractive-only or not at all);
MCP description compression (we never rewrite tool descriptions — that
is a CORRECTNESS position, keep it; the row stays MISSING-by-design, do
not "fix" it); output styles / language packs (cosmetic, M12 at most);
session-dedup cross-turn store + CCR retrieval markers (need a
cross-turn store design — queue behind M7.1, do not force).

## Roadmap M0–M12 (strict order)

| Milestone | Goal | Exit gate (summary) |
|---|---|---|
| M0 | Baseline + truth | CI script green or failures listed; audit-fake output committed |
| M1 | P0 security & correctness | All M1 tests green; 0 exempt mutating routes; PARITY areas 4, 6, 7, 9, 10, 11 flipped honestly |
| M2 | Per-response telemetry + observability ∥ M1 tail | Header contract test; 100% of response paths covered |
| M3 | Strategies & combos: the real core | Strategy conformance suite; PARITY Area 1 ≥ 80% weighted |
| M4 | Failover so good the agent never notices ∥ M3 | Chaos harness: 0% client-visible error rate for single-provider outage with ≥ 3 healthy fallbacks; added failover latency ≤ detection time + 2 ms; hedge cuts p99 ≥ 50% on slow-provider scenario |
| M5 | Budgets, quotas, pools | Concurrency race tests prove budgets cannot be exceeded by parallel requests |
| M6 | Caching 2.0 + token-cost engine | Cache correctness property tests (no cross-tenant, no temp leaks); savings numbers reproducible from ledger rows |
| M7 | Compression & token efficiency | Round-trip tests (code/JSON intact), inflation guard test, measured savings table committed |
| M8 | API surface breadth | Each endpoint has a contract test against recorded fixtures; claude-code/codex/cursor/cline/aider smoke scripts pass through Codvian |
| M9 | MCP / agent surface (security first) | A2A only after M9.1 passes security review |
| M10 | Dashboard: real, complete, honest (backend first!) | scripts/audit-fake.sh returns ZERO UI controls without live backend; Playwright smoke test clicks every control |
| M11 | Packaging, CLI, docs, ecosystem | Release verification (checksums, SBOM, signature), changelog fragments |
| M12 | BEYOND PARITY (only after M1–M9 gates pass) | Measured wins published for latency/RPS/memory, failover chaos benchmark, cache/compression savings, security defaults, explainable routing — each reproducible by scripts/bench.sh or a documented command |

### M9 slices (coordinator, 2026-11-19 — Area 9: 42 rows, FULL 4 / PARTIAL 7 / MISSING 27)

Current MCP state: stdio + SSE + Streamable HTTP server with 24 tools (3 built-in + 9 routing + 3 cost + 7 ops + 2 search), scopes enforced ON by default, audit log (stdio + serve mode), guardrail on tool calls (AHEAD), real stdio client side (AHEAD); protocol 2025-03-26, no A2A yet (Wave 3). Waves: Wave 1 (parallel now) M9.0 + M9.1 + M9.6; Wave 2 (after M9.1 lands — they consume its scope/audit API) M9.2 + M9.3 + M9.4 + M9.5 + M9.7; Wave 3 (only after M9.1 passes SECURITY REVIEW) M9.8, then M9.9; Wave 4 M9.10 exit gate.

| Slice | Scope | Files (disjoint) |
|---|---|---|
| M9.0 | MCP fixture harness (JSON-RPC recorded pairs over stdio) + Area 9 reconciliation | internal/apitest/mcp_fixture.go (new), tests/fixtures/mcp/ (new) |
| M9.1 | SECURITY BASELINE: per-tool scope system (own scope set, wildcard matching), enforcement ON by default (never copy sector-typical OFF default), audit log of tool calls (inputs hashed, outputs truncated), per-client identity; then an independent SECURITY REVIEW — A2A is gated on it | internal/mcp/scopes.go, internal/mcp/audit.go (new) |
| M9.2 | Remote transports: SSE (GET/POST /api/mcp/sse) + Streamable HTTP (POST/GET/DELETE /api/mcp/stream), gateway-key auth, forced scope enforcement for narrow keys | internal/server/mcp_http.go (new) |
| M9.3 | Routing/combo + cost/quota tools: list/create/switch combos, combo metrics, set_routing_strategy, explain_route, simulate_route, best_combo_for_task, pick_fastest_model, cost_report, check_quota, set_budget_guard (scoped manage) | internal/mcp/tools_routing.go, internal/mcp/tools_cost.go (new) |
| M9.4 | Cache + DB health + memory tools: cache_stats, cache_flush, db_health, db_stats, memory_search/add/clear — destructive ones (cache_flush, memory_clear, db autoRepair) ONLY under a separate destructive grant, never the default tool scope | internal/mcp/tools_ops.go (new) |
| M9.5 | Tool search + protocol version 2024-11-05 → 2025-03-26 + heartbeat/status telemetry + cardinality controls (allow/deny lists, maxTools) | internal/mcp/tools_search.go, internal/mcp/mcp.go (ProtocolVersion line only — no protocol.go shipped) |
| M9.6 | MCP client-side hardening: spend cap + cost estimate + allowlist for mcp_tools_execute (today: no cap, no estimate, no allowlist) | internal/mcptools/ (existing files only) |
| M9.7 | Dashboard MCP page/feed: audit view + transport toggles + tool list with scopes | web/index.html, internal/server/mcp.go, internal/server/server.go |
| M9.8 | A2A CORE (only after M9.1 security review passes): POST /a2a (message/send, message/stream, tasks/get, tasks/cancel), task persistence + states, auth ON by default (NEVER keyless), owner scoping (owner = key hash; no ownerless visibility), enable flag default false | internal/server/a2a.go, internal/a2a/ (new) |
| M9.9 | A2A skills (smart-routing, quota-management, provider-discovery, cost-analysis, health-report, list-capabilities) + agent card (/.well-known/agent.json) + /api/a2a/status + dashboard | internal/a2a/skills.go (new), internal/server/a2a.go |
| M9.10 | Exit gate: full CI + MCP fixture coverage audit + live stdio/SSE/Streamable-HTTP smoke + PARITY Area 9 flips | — (coordinator) |

Deliberately OUT of M9 (recorded, not forgotten): `route_request` spending tool — deliberate divergence, "a tool call can never spend quota", NEVER build; plugin install/activate/uninstall over MCP — contradicts compiled-in plugins (plugin/load is 501 by design: loading arbitrary code = arbitrary code execution); context-source tools (Notion/Obsidian/local-corpus) — external SaaS, not router core; gamification tools — not router core; skills execution / dynamic `skill_*` tools — arbitrary code execution surface, needs its own security design (M12); Perplexity research tool — third-party key, research feature (M12). NEVER-copy from the reference benchmark in M9: destructive MCP tools without a separate grant, lossy compression of MCP tool descriptions (ours stay byte-exact), client-controlled guardrail disable, keyless-by-default /v1 and A2A, cookie-session providers.

## Leftover / deferred from M0

- M0.1 gate running (parent's run, /tmp/m0-gate.log) — real test count lands in LOG.md
  when it finishes; then M0 commits (one per task) + M0 exit gate. M0.3/M0.4 DONE above.
- PARITY.md per-area rows for Areas 8a/8b are condensed (sub-tallies + key rows) — the full
  38 + 139 row-level detail lives in the source comparison report
  (`~/workspace/goals/codvian-router/hidden_files/reference-full-comparison-2026-11-18.md`).
  Expand to full row level only if a later task requires it; do not re-run the comparison.

## Raunak's order — one agent per feature (2026-11-19 ~21:57 IST, voice call)

- Model: every agent gets ONE feature (one PARITY row / one burn-down item),
  builds it fully, tests its own package (pkgtest.sh), commits, verifier checks
  it, then the agent takes the next feature from the queue. Build + test + merge,
  feature by feature — this is how Block 2 runs.
- Limits (honest, from the VM): max 8 live agents at a time (v2 cap) — 414
  features at once would need 414 agents and the 1.5-CPU VM already broke at
  20–60. Features that touch the same files are grouped (one agent per
  disjoint-file group) so two agents never edit the same file.
- Precondition: the gate is RED right now (2026-11-19 ~21:57 IST: 5,965 PASS,
  29 FAIL, 5 SKIP; 3 packages failing — server/ledger/relay, mostly AK-1 area
  where A1 is working). Fix the 29 failures first, get the tree green, then
  start the one-feature-per-agent queue. Merging features into a red tree
  breaks the others.

## Raunak's order — cap lifted, fix-first (2026-11-19 ~21:59 IST, voice call)

- The v2 8-agent cap came from an AI-written prompt; Raunak overrides it: raise
  the agent count as needed, up to the VM's real capacity, as long as work
  completes WITH accuracy. Full power, as many parallel agents as the VM can run.
- ORDER: fix-first. ALL agents on the 29 gate failures before anything else.
  No diversifying into new features until the tree is green. Then distribute
  features one-by-one (one agent per feature, previous order).
- 29 failures (gate 2026-11-19 ~21:56 IST, 5,965 PASS / 29 FAIL / 5 SKIP):
  server 21 (Messages contract 10 + route/docs/guard 9 + AK-1 2 owned by A1),
  relay 6 (Anthropic parse), ledger 2 (schedule). Fixers spawned 21:59 IST:
  FIX-SERVER-A (10), FIX-SERVER-B (9), FIX-RELAY (6), FIX-LEDGER (2).
  Live then: A1 + V-3 + 4 fixers = 6. Each fixer owns disjoint files only;
  cross-file root causes are reported BLOCKED, not edited across.
- NOTE: the user's screen still shows "gate still running" — stale display.
  The gate FINISHED at ~21:56 IST: RED, 29 fails, recorded in docs/gate-last.txt.

## Critic BLOCKED — 2026-11-19 22:21 IST (v2 critic run, no fix made)

- Provider-card "Connect" button (web/index.html:4029) — BLOCKED. The button is
  dead (toast-only) and the correct fix needs a NEW backend endpoint: the card
  is a configured-but-disabled provider (the grid feeds only from
  GET /api/providers; Connect shows iff enabled=false), and no route enables a
  provider — Registry.SetEnabled (internal/provider/registry.go:1020) has no
  HTTP surface, and re-adding via POST /api/providers would 409
  (ErrProviderExists, same base URL already configured). A new
  POST /api/providers/{id}/enabled needs a guard-table row + register +
  handler + tests, touching internal/server/server.go + guard.go while
  FIX-SERVER-A/B own the gate failures there (gate RED, 23 failures left;
  their 208-route pins + regenerated API docs just landed) — and it exceeds
  the critic 20-min fix budget. Do NOT fix UI-only: no existing route flips a
  disabled provider back to connected. Next: a server-lane agent takes it once
  the gate is green (endpoint first, then the UI wiring +
  TestDashboardM10_7ServedHTML assertions + toggle proof). No PARITY.md flip
  (needs V's independent pass). No push.

  FIXED 2026-11-19 ~23:55 IST (v2 critic run): the gate went green (A4
  6004/0/5) and no lane owned server.go, so the critic built it — new POST
  /api/providers/{id}/enabled (guard row, register, handler via
  Registry.SetEnabled + key-changed hook), dashboard Connect POSTs
  {enabled:true}, TestSetProviderEnabled + ServedHTML assertions, full
  server pkg PASS. Code in watchdog wip 27f54be; guard-test pin (209 /
  9/24/176) + LOG.md line in the critic commit. Rotate below still BLOCKED.

- Provider-key rotate Enter (web/index.html:4176) — BLOCKED. The per-key input's
  Enter handler only toasts ("Key rotation from the dashboard isn't wired
  yet"); no provider-key rotate route exists (only POST /api/providers/{id}/keys
  add, DELETE .../keys/{keyId}, POST .../keys/{keyId}/role — server.go:1488-1490).
  The correct fix needs a NEW backend endpoint (POST
  /api/providers/{id}/keys/{keyId}/rotate: guard-table row + register +
  handler + tests, touching internal/server/server.go + guard.go) — server-lane
  work, exceeds the critic 20-min fix budget, and a UI-only DELETE+POST-add
  workaround would change key identity and risk losing the key if the add
  fails. Next: a server-lane agent takes it once the gate is green (endpoint
  first, then the Enter wiring + ServedHTML assertions + toggle proof). No
  PARITY.md flip (needs V's independent pass). No push.

  FIXED 2026-11-20 ~00:33 IST (v2 critic run): the gate went green (A4
  6004/0/5) and no lane owned server.go/provider registry, so the critic
  built it — new Registry.RotateKey (live-test + in-place replace, ID/role
  kept, warm pool dropped, persist with rollback) + POST
  /api/providers/{id}/keys/{keyId}/rotate (guard row, register, handler
  via the key-changed hook), dashboard Enter POSTs {api_key} for the
  primary key, TestRotateKey* + TestRotateProviderKey + ServedHTML
  assertions, full provider + server pkg PASS, audit-fake now 0 findings
  (210 routes). No PARITY.md flip (needs V's independent pass). No push.

  BLOCKED 2026-11-20 ~01:25 IST (v2 critic run): Dashboard authentication
  (PARITY row 619) — no dashboard password, session, or login of any kind;
  protection is loopback-bind only (the per-action gateway-key fields are
  not a login; verified by grep — no session, cookie, or login surface in
  internal/server or web/index.html). The fix needs a full login/session
  system (password + session + optional OIDC, server + dashboard), hours
  of work — far over the critic 20-min fix budget — and it touches
  internal/server/server.go + guard.go, which the live B9 lane owns right
  now (tree dirty, B9 G21 running, HEAD 8e1ac3d). Next: a server-lane
  agent takes it once B9 lands (login endpoint + session cookie +
  dashboard login view + audit-fake re-run). No PARITY.md flip (needs V's
  independent pass). No push.
  RE-VERIFIED 2026-11-20 ~01:56 IST (v2 critic run): block still holds —
  gap re-verified REAL by grep (still no session/cookie/password/login
  surface in internal/server or web/index.html); B9 has landed (V-12 PASS,
  ecb4748 merged) but the lane owner is now B8-AK2 (tree dirty: guard.go
  +112 / guard_test.go +114 / server.go / danger.go / main.go, server.test
  active) — login/session fix still hours of work and still touches
  B8-owned server.go/guard.go. No fix this run; no second fix per v2 §4.5.

  RE-VERIFIED 2026-11-20 ~02:26 IST (v2 critic run): block still holds —
  gap re-verified REAL by grep (no Set-Cookie/requireLogin/CHANGEME/
  dashboard-password surface in internal/server or web/index.html;
  PARITY.md:619 still MISSING). What changed since 01:56: B8-AK2 has
  landed (291be80, V-14 PASS) and the tree is clean of code changes, so
  the lane-ownership part of the block has LIFTED — the block now rests
  solely on the time budget: a login/session system (password + session
  + optional OIDC per row 619) is hours of work, far over the critic
  20-min fix budget, and the internal/server pkgtest alone is ~320 s.
  Next: a dedicated server-lane agent takes it (login endpoint +
  session cookie + dashboard login view + audit-fake re-run); a critic
  run can never fit it. No fix this run; no second fix per v2 §4.5.
  No PARITY.md flip. No push.

  RE-VERIFIED 2026-11-20 ~03:00 IST (v2 critic run): dashboard-auth block
  still holds — gap re-verified REAL by grep (no Set-Cookie/requireLogin/
  CHANGEME/dashboard-password surface in internal/server or web/index.html;
  PARITY.md:619 still MISSING; rows 628 OAuth/JWT and 733 request-rewrite
  also still MISSING, verified by line read). No live agents, tree clean of
  code changes, so the block still rests solely on the time budget: a
  login/session system is hours of work, far over the critic 20-min fix
  budget.
  CORRECTION 2026-11-20 ~03:02 IST (root): the gate paragraph the critic
  wrote below this line is WRONG — it read stale pre-fix artifacts while
  the re-run was in flight. Authoritative re-run at HEAD 2882109 (root
  exec, finished ~03:01 IST): full gate GREEN PASS=6027 FAIL=0 SKIP=5,
  47/47 packages; race-hot GREEN PASS=3249 FAIL=0 SKIP=0, 5/5 packages.
  Gate 1 exit MET; no re-run, no fix, no BLOCKED gate. The '3245/4'
  numbers are the old 291be80 pre-fix race-hot run. ALSO: gate-last.txt
  timestamp is garbage (literal '$(__import__(...))' — gate.sh:49 python
  f-string bug); do NOT edit scripts/gate.sh while a gate is running
  (bash reads it incrementally).
  No fix this run; no second fix per v2 §4.5. No PARITY.md flip. No push.

  CORRECTION 2026-11-20 ~03:05 IST (same critic run): the gate claim in the
  ~03:00 entry above is WRONG and is withdrawn. The gate-last.txt read at
  02:53 (PASS=3245 FAIL=4, 5 pkgs) was a stale/mid-flight artifact, not a
  final verdict — its population (3249 tests, 5 pkgs) is the race-hot set,
  and its 4 "failures" are the pre-fix race-hot 4, now fixed and passing.
  FINAL, verified 03:03 IST: Gate 1 GREEN, HEAD 2882109 — race-hot
  PASS=3249 FAIL=0, 5/5 (read directly from gate-last.txt + JSON by this
  run); full gate PASS=6027 FAIL=0 SKIP=5, 47/47 per the recorded Gate 1
  result (its JSON was overwritten by race-hot — gate.sh reuses one
  /tmp/codvian.gate.json for both runs, so full-gate evidence is destroyed
  by the race-hot run; gate-last.txt keeps only the last run — process gap
  to fix after the gate, never mid-run). BLOCK 1 EXIT MET. The "gate
  failures BLOCKED, needs a full re-run" block is WITHDRAWN — no re-run
  needed. Dashboard-auth block (row 619) still stands as written above.

  RE-VERIFIED 2026-11-20 ~03:55 IST (v2 critic run): dashboard-auth block
  still holds — gap re-verified REAL by grep (Set-Cookie count 0 across
  internal/server + web/index.html; row 619 still MISSING; rows 628
  OAuth/JWT and 733 request-rewrite also still MISSING, verified by code
  read — applyGuardrails server.go:4994 only blocks/flags, never mutates
  req). Stage 1 build phase is LIVE (started ~03:51 IST, queue doc
  docs/STAGE1-QUEUE.md landed during this run; :529F e7d5cbe committed
  mid-run). No fix this run; no second fix per v2 §4.5. No PARITY.md flip.
  No push.

  BLOCKED 2026-11-20 ~03:55 IST (v2 critic run): response_validation
  config-file wiring (routing.response_validation → engine; the recorded
  follow-up of the :523 FULL flip, PARITY.md:557). The fix needs
  internal/config/config.go (new RoutingConfig field) + cmd/codvian-router/main.go
  (map into router.Config at engCfg :2795) — BOTH files are assigned to the
  L-STRAT lane in docs/STAGE1-QUEUE.md (B2-09, account-level fallback
  subset), and Stage 1 builders are live in the shared tree right now.
  The G11 report's "verbatim wiring" the code comment cites does not exist
  as a doc (grep: G11 only in PARITY.md/GAP-TRACK.md), so the mapping would
  be designed fresh, not copied. Next: a Stage 1 L-STRAT builder takes it
  as part of B2-09, or the critic retries once Stage 1's test phase frees
  the files. No second fix per v2 §4.5. No PARITY.md flip. No push.
