# docs/GAP-INVENTORY.md — Pull-forward inventory (MISSING / PARTIAL / WRONG)

Scout inventory, 2026-11-19 ~13:30 IST. Read-only scout: this file is the only write.
Source: `docs/PARITY.md` (mechanical parse + hand verification) and, for Area 8b only,
the comparison report `~/workspace/goals/codvian-router/hidden_files/omniroute-full-comparison-2026-11-18.md`
(PARITY.md §8b is condensed — full row detail lives there).

Rule applied (Raunak, 2026-11-19 ~13:02 IST): anything MISSING/PARTIAL/WRONG in the
691-row PARITY comparison is parity work and is pulled forward into M1–M11. Only purely
beyond-parity items (things OmniRoute does not have at all) stay in M12 (paused).
By construction every gap row below compares against an OmniRoute feature, so bucket D is empty.

## 0. Headline verification (from PARITY.md, verified mechanically, not taken on trust)

Latest headline in the file is the **M8.22 exit-gate note** (PARITY.md §Headline), not the
pre-M8.22 numbers quoted in the task brief (PARTIAL 190 / MISSING 221 — those were correct
before the Batches flip, M8.22 flipped §14a Batches MISSING → PARTIAL):

| Figure | Verified value | How verified |
|---|---|---|
| Total rows | **691** | 578 table rows parsed mechanically + 113 Area 8b rows condensed out of PARITY.md (139 claimed − 26 key rows listed) = 691 |
| FULL | **234** | Sum of per-area headers = 234; mechanical recount of the tables = 234 |
| PARTIAL | **191** | Sum of per-area headers using the Area 14 header (41) = 188; mechanical recount of the tables = **191** — the Area 14 header line is stale by 3 (see §0.2) |
| MISSING | **220** | Sum of per-area headers = 220; mechanical recount = 220 (using the Area 8b header 91) |
| WRONG | **7** | Sum of per-area headers = 7; mechanical recount = 7 |
| AHEAD | **28** | Sum of per-area headers = 28; mechanical recount = 28 |
| DIVERGENCE | **10** | 9 in area tables + 1 in Area 8b sub-tallies (Settings) = 10 |
| Unclassified claims row (Area 14d test headline) | 1 | Counted outside the comparable total |
| Comparable total | **652** | 691 − 28 AHEAD − 10 DIVERGENCE − 1 unclassified = 652 |
| Strict parity | **35.9%** | 234 / 652 = 35.89% |
| Weighted parity | **50.5%** | (234 + 191/2) / 652 = 329.5 / 652 = 50.54% |
| Gap rows (PARTIAL + MISSING + WRONG) | **418** (headline) / **420 listed** | Listed total exceeds the headline by 2 — both are the known Area 8b excess (see §0.2) |

### 0.1 Work already in flight in the working tree (uncommitted — do NOT rebuild)

CORRECTION (gap coordinator, 2026-11-19 ~13:35 IST — the scout misattributed authorship; verified from coordinator spawn records and file headers): the four items below are GAP-TRACK work (builders G1/G3, spawned by the gap coordinator with those exact briefs), NOT the main chain's. They are still listed as gaps (PARITY.md still says MISSING — correct, nothing is flipped until it lands and is verified), and the gap track must NOT start duplicate slices for them — verify + flip instead, EXCEPT audio (see its row note): the client exists but the router adapter is missing, so the audio row must NOT be flipped; a small adapter slice is still needed.

| PARITY row | What exists in the tree (uncommitted) | Wiring evidence |
|---|---|---|
| Area 1 "Account-level fallback subset (9 values)" (PARITY.md:388, MISSING) | `internal/router/account_fallback.go` — per-provider account fallback strategies (Config `.AccountFallbackStrategy` + `.AccountFallbackStrategies`) | `internal/router/router.go:550-570` (config fields + comments reference account_fallback.go) |
| Area 1 "Router strategy sla-aware / sla" (PARITY.md:394, MISSING) | `internal/sla/sla.go` (+ tests) — measurement-only SLA scoring, "the local form of OmniRoute's sla-aware strategy" | Imported by `internal/router/router.go`, `internal/strategy/strategy.go`, `internal/strategy/wiring.go` |
| Area 1 "Router strategy nadir" (PARITY.md:396, MISSING) | `internal/router/nadir.go` (+ tests) — fail-closed Nadir strategy: requires a configured decision endpoint; unconfigured it falls back to `priority` and never calls out. The GAP row note "no equivalent source exists" is now false; the only remaining decision is whether an operator configures an endpoint. | `internal/router/autodispatch.go:168-172` (`nadirSelect`); strategy registered `internal/strategy/strategy.go:174,181,220,393-394`, `wiring.go:664-665`; Config `NadirEndpoint/NadirAPIKey/NadirTimeoutMs/NadirTransport` (`router.go:583-604`) |
| §14a "Audio" (PARITY.md:1080, MISSING) | `internal/provider/audio_client.go` — "the provider client the M8.6 slice left missing" (M8.6 merge follow-up, finding 1); speech/transcriptions/translations, verbatim bodies. TRANSPORT ONLY — the client implements a new `AudioPoster` interface (`PostAudio`), but the router still selects on `AudioPassthroughClient.AudioPassthrough` (`audio_passthrough.go:255-256,268,290`) and no production code implements it — audio is still dead behind the honest 400. DO NOT FLIP; adapter slice still needed (implement `AudioPassthrough` on *openAIClient delegating to `PostAudio`, or switch the router to assert `AudioPoster`) + end-to-end test. | `internal/router/audio_passthrough.go:290,307` calls `AudioPassthroughClient.AudioPassthrough` — cites the router side the new file does NOT satisfy |

Caveat: `git status` at scout time shows many more modified files (M9/M10 work: server, guard,
combos, files, media_verify, web/index.html, tryit.html, docs). Only the four items above were
verified as closing named PARITY rows; the rest was not inventoried. Re-run the §3 staleness
checks after the parallel chain commits — more rows will have flipped by then.

### 0.2 Mechanical recount vs area headers

Script: split tables on unescaped pipes, verdict from the Verdict column
(Area 12 technique table: 5th column; Area 14d: 3rd column; Area 12 measured-numbers
table excluded — measurements, not parity rows). Every area header matches the mechanical
recount exactly, EXCEPT:

| Area | Header claims | Mechanical recount | Discrepancy |
|---|---|---|---|
| 14 — Plugins, API, docs | FULL 15 / PARTIAL **41** / MISSING 28 / WRONG 1 / AHEAD 1 / UNVERIFIED 1 (90 rows) | FULL 15 / PARTIAL **44** / MISSING 28 / WRONG 1 / AHEAD 1 / UNVERIFIED 1 (90 rows) | Header PARTIAL is stale by 3: §14a 17 + §14b 21 + §14c 4 + §14d 2 = 44. The headline PARTIAL 191 is consistent with 44, so the headline is right and only the Area 14 header line is stale. |
| 8b — Dashboard (Routing/Keys/Budgets/Analytics/Logs/Settings) | 139 comparable rows: FULL 7 / PARTIAL 36 / MISSING 91 / WRONG 4 | 26 key rows listed in PARITY.md: FULL 7 / PARTIAL 3 / MISSING 11 / WRONG 3 / DIVERGENCE 1 / EXTRA 1 | Condensed by design. Report expansion (§2, Area 8b below): 153 rows — FULL 6 / PARTIAL 37 / MISSING 92 / WRONG 4 / EXTRA 13 / DIVERGENCE 1. Against the sub-tallies (152 rows: FULL 6 / PARTIAL 36 / MISSING 92 / WRONG 4 / EXTRA 13 / DIVERGENCE 1) the expansion has 1 extra row and 1 extra PARTIAL: combo-builder lists 32 rows vs sub-tally 31 (extra MISSING is plausibly "Candidate-pool endpoint", a duplicate of the Area 1 / §14a rows), and Keys parses PARTIAL 6 / MISSING 13 vs sub-tally PARTIAL 5 / MISSING 14 (one row classified differently — UNVERIFIED which). Net effect on the inventory: 8b gap rows listed 133 vs header 131 (+2). |
| 8b sub-tallies vs 8b header | Header FULL 7 / MISSING 91 | Sub-tallies FULL 6 / MISSING 92 | Known pre-existing ±1 (M6 note in PARITY.md); the headline uses the header numbers (FULL 234, MISSING 220). |

Per-area header counts (authoritative, from PARITY.md headers — mechanical recount agrees
except as noted above):

| Area | Rows | FULL | PARTIAL | MISSING | WRONG | AHEAD | DIV | Gaps (P+M+W) |
|---|---|---|---|---|---|---|---|---|
| 1 — Strategies / combos / auto scoring | 63 | 34 | 25 | 4 | 0 | 0 | 0 | **29** |
| 2 — Failover, breakers, retries, hedging | 46 | 33 | 7 | 0 | 0 | 3 | 3 | **7** |
| 3 — Providers / models catalog | 20 | 10 | 6 | 2 | 1 | 1 | 0 | **9** |
| 4 — Auth, keys, scopes, route guards | 32 | 14 | 3 | 6 | 0 | 9 | 0 | **9** |
| 5 — Budgets, rate limits, quota | 30 | 27 | 0 | 0 | 0 | 2 | 1 | **0** |
| 6 — Caching | 35 | 24 | 7 | 0 | 0 | 3 | 1 | **7** |
| 7 — Guardrails, PII redaction, content policy | 36 | 17 | 7 | 12 | 0 | 0 | 0 | **19** |
| 8a — Dashboard: Home / Providers / Models (+ Pric | 38 | 5 | 14 | 19 | 0 | 0 | 0 | **33** |
| 8b — Dashboard: Routing / Keys / Budgets / Analyt | 139 | 7 | 36 | 91 | 4 | 0 | 1 | **131** |
| 9 — MCP server / A2A server | 42 | 4 | 7 | 27 | 0 | 2 | 2 | **34** |
| 10 — Compression, token counting | 47 | 10 | 13 | 20 | 0 | 4 | 0 | **33** |
| 11 — Analytics, usage, ledger, cost | 28 | 16 | 7 | 5 | 0 | 0 | 0 | **12** |
| 12 — Latency, startup, memory, binary | 18 | 12 | 3 | 0 | 1 | 0 | 2 | **4** |
| 13 — Packaging, Docker, install, CLI | 27 | 6 | 12 | 6 | 0 | 3 | 0 | **18** |
| 14 — Plugins, API surface/endpoints, docs/README | 90 | 15 | 44 | 28 | 1 | 1 | 0 | **73** |
| **Total (headline)** | **691** | **234** | **191** | **220** | **7** | **28** | **10** | **418** |

Note on the per-area table above: Area 14 is shown with the mechanically-corrected PARTIAL 44
(header says 41 — stale). Area 8b is shown with its header counts; the row-level expansion in §2
lists 133 gap rows for 8b (header: 131). The de-duplication caveat from PARITY.md stands:
≈80–90 rows are duplicates across areas (mostly 8b re-counting backend features as dashboard
UI options), so ≈600 distinct features, not 691; no row-by-row de-duplication was done here —
duplicates are flagged DUP in the Note column instead.

## 1. Per-bucket totals

| Bucket | Scope | Gap rows |
|---|---|---|
| A — M1–M8 scope — gap track | 183 |
| B — M9 scope (MCP/A2A) — main coordinator | 36 |
| C — M10/M11 scope (dashboard / packaging-docs) — main coordinator chain | 201 |
| D — Pure beyond-parity — M12 (paused) | 0 |
| **Total listed** | | **420** |

Bucket D is **0 rows**: every MISSING/PARTIAL/WRONG row compares against an OmniRoute
feature, so none of it is purely beyond-parity. (The AHEAD rows — 28 — and the EXTRA rows
are ours-only, but they are not gaps.) Rows flagged DO-NOT-BUILD in the tables below are
still bucketed by domain per the pull-forward rule, but must NOT be built: they are
deliberate divergences whose PARITY verdict should be corrected to DIVERGENCE instead.
Count of DO-NOT-BUILD rows: 16 (15 full rows + the load half of the §14b Plugins row).
Rows flagged DECISION (needs a product/coordinator decision before any slice is cut): 15 (corrected 2026-11-19 — §1 previously said 16; only 15 DECISION flags exist in the §2 tables).
Rows flagged RELEASE-BLOCKED: 9 (8 in Area 13 + §14b Version-manager).
Rows flagged STALE? (row text looks out of date — see §3): 41. Rows flagged DUP: 6.

Per-area × bucket:

| Area | A | B | C | D | Total gaps |
|---|---|---|---|---|---|
| 1 | 29 | 0 | 0 | 0 | **29** |
| 2 | 7 | 0 | 0 | 0 | **7** |
| 3 | 9 | 0 | 0 | 0 | **9** |
| 4 | 9 | 0 | 0 | 0 | **9** |
| 5 | 0 | 0 | 0 | 0 | **0** |
| 6 | 7 | 0 | 0 | 0 | **7** |
| 7 | 19 | 0 | 0 | 0 | **19** |
| 8a | 0 | 0 | 33 | 0 | **33** |
| 8b | 0 | 0 | 133 | 0 | **133** |
| 9 | 0 | 34 | 0 | 0 | **34** |
| 10 | 33 | 0 | 0 | 0 | **33** |
| 11 | 12 | 0 | 0 | 0 | **12** |
| 12 | 3 | 0 | 1 | 0 | **4** |
| 13 | 0 | 0 | 18 | 0 | **18** |
| 14a | 29 | 0 | 0 | 0 | **29** |
| 14b | 26 | 2 | 4 | 0 | **32** |
| 14c | 0 | 0 | 8 | 0 | **8** |
| 14d | 0 | 0 | 4 | 0 | **4** |
| **Total** | **183** | **36** | **201** | **0** | **420** |

## 2. Gap rows by area

Columns: `PARITY.md` line (or comparison-report line for §8b expansion) · Feature ·
Verdict · OmniRoute reference · Our status (condensed from the PARITY row; see PARITY.md
for the full text) · Bucket · Note. "STALE?" means the row text looks out of date against
the current tree — evidence in §3; the verdict is NOT changed here (scout is read-only).

### Area 1 — Strategies / combos / auto scoring

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:368 | context-relay | PARTIAL | strategyRegistry.ts:143; executeTargetAttempt.ts:755 | Relay state + next-attempt context builder implemented and registered (M3); engine integration LIVE for the in-request on-error relay (Phase D, D5: outcomes → handoff → next attempt, lazy summariser-seam handoff, honest raw-transcript degradation, LastRelayStatus). Still missing… | A | Data gaps: no production summariser, no quota-use signal, no cross-request store. |
| PARITY.md:374 | reset-aware | PARTIAL | quotaStrategies.ts:552; quotaScoring.ts:362 | Ordering implemented + registered (M3); reset times now live from the ledger PairQuota roll-off (Phase C), but QuotaRemaining is still the booleanised QuotaHint, not a measured fraction — the remaining component is coarse, so the score cannot yet match OmniRoute's continuous rem… | A |  |
| PARITY.md:376 | expiry-first | PARTIAL | quotaStrategies.ts:727; quotaScoring.ts:579 | Ordering implemented + registered (M3); no expiry data exists anywhere, so the deadline falls back to the now-real reset time (Phase C); the usable fraction is still the booleanised QuotaHint, not a measured fraction | A | DECISION — needs a quota-expiry data source (none exists). |
| PARITY.md:377 | headroom | PARTIAL | headroomRanking.ts:54 (1 − max(util5h, util7d)) | Ordering implemented + registered (M3); utilisation is still unknown (-1, no per-pair limit exists anywhere) — all targets score headroom 1 (OmniRoute's own fail-open) until a per-pair limit source exists (product decision, not a code gap) | A | DECISION — needs a per-pair limit source (product decision, not a code gap). |
| PARITY.md:380 | auto (16-factor pool) | PARTIAL | resolveAutoStrategy.ts:186; autoCombo/engine.ts:221 | auto/* resolves a model via embedding tiers; the auto/* variant resolver exists with real pick rules (Phase C) and engine dispatch is live (Phase D, D2 autodispatch) — but 16-factor pool scoring is still pending | A |  |
| PARITY.md:382 | context-optimized | PARTIAL | comboStructure.ts:1050 (largest context first) | Largest-window-first ordering implemented + registered (M3); context windows are not a hook input yet, so it returns declared order until window data lands (Phase C) | A |  |
| PARITY.md:383 | cache-optimized | PARTIAL | applyStrategyOrdering.ts orderByCacheAffinity | Affinity ordering implemented + registered (M3); cache warmth is not a hook input yet, so it returns declared order until warmth data lands (Phase C) | A |  |
| PARITY.md:387 | quota-share (internal, 23rd) | PARTIAL | routingStrategies.ts:30-34; quotaShareStrategy.ts:284 (DRR + P2C) | DRR wired into the engine (Phase D, D4, internal/router/cascade.go): per-pool deficits, 2:1 rotation verified against the strategy reference sequence. Missing: saturation gating + P2C unwired (no per-member saturation signal — QuotaHint is a scalar; no in-flight counts); no oper… | A |  |
| PARITY.md:388 | Account-level fallback subset (9 values) | MISSING | routingStrategies.ts:56-65 | No per-account fallback-strategy concept | A | IN-TREE-NOW — see §0.1 (account_fallback.go + router.go:550-570, uncommitted); verify + flip, do not rebuild.|
| PARITY.md:389 | auto 16-factor scoring (DEFAULT_WEIGHTS sum 1.0) | PARTIAL | autoCombo/scoring.ts:62 | 5 factors only; taskFit, tierPriority/Affinity, specificityMatch, stability, health, context/cache/session affinity, connectionDensity, resetWindowAffinity all absent | A |  |
| PARITY.md:390 | Router strategy rules (default) | PARTIAL | AUTO_ROUTING_STRATEGY_VALUES; routerStrategy.ts | 5-factor balanced scorer is the analogue, 11 of 16 factors missing | A |  |
| PARITY.md:391 | Router strategy score | PARTIAL | Same source | Same 5-factor scorer | A |  |
| PARITY.md:394 | Router strategy sla-aware / sla | MISSING | Same source | No SLA concept anywhere | A | IN-TREE-NOW — see §0.1 (internal/sla wired into strategy/router, uncommitted); verify + flip, do not rebuild.|
| PARITY.md:396 | Router strategy nadir | MISSING | nadirStrategy.ts:2-16 (external Nadir API, fail-open) | No external decision API | A | DECISION — external decision API; no equivalent source exists. |
| PARITY.md:402 | Mode pack chaos-mode | PARTIAL | modePacks.ts (health 0.4, taskFit 0.1905; parallel panel) | Weight pack wired as a selectable strategy (M3); parallel fan-out now live via the D3 combo path; fault-injection still missing (M4 chaos harness) | A | Fault injection exists as a test harness (internal/chaos) but no non-test importer wires it into the strategy path — gap is wiring, not the harness. |
| PARITY.md:403 | Live intelligence (Arena ELO + models.dev) | PARTIAL | MODEL_INTELLIGENCE.md; Arena ELO sync 24h + models.dev tier data | models.dev sync exists but opt-in OFF by default; no Arena ELO, no ELO/tier signal in scoring | A |  |
| PARITY.md:404 | Candidate-pool endpoint | PARTIAL | GET /v1/auto-combo/{channel}/candidates (AUTO-COMBO.md:82-84) | Route registered GET /v1/auto-combo/{channel}/candidates (Phase D, D1) with the live pool: tier models, breaker states, per-key connections, cooldowns; free/pro + category filters applied from the live price book + tier membership. Gaps: allowlist exclusions have no store (exclu… | A | DUP of §14a row (same endpoint). |
| PARITY.md:406 | auto (balanced, LKGP-sticky) | PARTIAL | AUTO-COMBO.md:13-28 | auto picks any tier via embedding; no LKGP stickiness, no 16-factor pool | A |  |
| PARITY.md:413 | auto/chaos | PARTIAL | AUTO-COMBO.md:28; README:369 (parallel fan-out, panel of 5) | Panel of 5, one model per provider; parallel fan-out now engine-wired via the chaos-mode combo path (Phase D, D3); fault-injection deliberately not implemented — belongs in the M4 chaos harness | A | Same as chaos-mode: panel live, fault injection unwired. |
| PARITY.md:414 | auto/subscription | PARTIAL | SUBSCRIPTION_LADDER.md:71-82 (fail closed, never billable fallback) | Fail closed, rung-0 only, never a billable fallback; engine dispatch live (Phase D, D2) — still fails closed by design, no billing/subscription data exists anywhere | A | Fails closed by design; blocked on billing/subscription data existing anywhere. |
| PARITY.md:415 | auto/thrifty | PARTIAL | SUBSCRIPTION_LADDER.md:95-106 | 5-rung ladder (subscription→keyless→free→cheap→premium) live at dispatch (Phase D, D2: resolveThrifty → cost-saver strategy); exhausted rungs gated out by real quota signals | A |  |
| PARITY.md:417 | auto/best-* family | PARTIAL | builtinCatalog.ts:23-29,46 | Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); vision + reasoning fail open (no vision data, no quality threshold) | A |  |
| PARITY.md:418 | auto/pro-* family | PARTIAL | builtinCatalog.ts:30-34 | Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); pro-* fail open in production (no premium classification source) | A |  |
| PARITY.md:421 | 4-tier fallback cascade (Subscription → API Key → Cheap → Free) | PARTIAL | Deep-study §1.1 (exact wording not re-verified fresh) | Cascade ordering built and tested (Phase C); engine helper applyCascadeOrder built and tested with injected tier data (Phase D, D4, internal/router/cascade.go) but NOT wired — no per-attempt billing class/tier signal (Attempt carries ProviderID/Model/KeyID only), no engine trigg… | A | Blocked on a billing-class/tier signal per attempt (Attempt carries ProviderID/Model/KeyID only). |
| PARITY.md:422 | Endpoint-specific combos (chat, search, embeddings, rerank, images, video, music, OCR, audio) | PARTIAL | endpointCapabilities.ts:36 comboForEndpoint | Endpoint projection built and tested (Phase C); enforced at all three live engine entry points (Phase D, D4) — chat, stream, embeddings — fail-closed with zero provider calls on rejection; unknown endpoint kinds fail closed. Embeddings + rerank + images live-enforced (M8.5: Engi… | A |  |
| PARITY.md:423 | Shared attempt budget maxGlobalAttempts, clamped [1, 200] | PARTIAL | comboConfig.ts:137 default 30; comboPredicates.ts:130-137,176-186 | MaxAttempts default 5, no upper clamp | A |  |
| PARITY.md:424 | Nested combos (maxComboDepth 3, flatten) | PARTIAL | comboConfig.ts; comboPredicates.ts | No combo-in-combo. We have nestable strategy trees instead (different, arguably more expressive) | A |  |
| PARITY.md:425 | Preferred / reserve accounts for auto combos | MISSING | Deep-study §1.1; opt-in preferred-account response-start failover | No preferred/reserve account concept | A | M4.8 built opt-in preferred-account failover; the auto-combo account concept is still missing. |
| PARITY.md:427 | Hedging config (off by default, 500 ms, bodies >256 KB never hedged) | PARTIAL | comboConfig.ts:177; comboAttemptLoop.ts:76,359-374 | Hedging ON by default at 150 ms, disabled for explicit-order chains — faster default, but no body-size guard | A | STALE? — see §Stale flags #1 (guard exists). DUP of Area 12 row 1. |

### Area 2 — Failover, breakers, retries, hedging

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:452 | 200-OK body validation predicate → failover | PARTIAL | comboConfig.ts responseValidation per-combo predicate | No general 200-body predicate; only empty/error streams via preread | A |  |
| PARITY.md:453 | Breaker states CLOSED/DEGRADED/OPEN/HALF_OPEN, persisted, lazy recovery | PARTIAL | RESILIENCE_GUIDE.md:46-48,62 | CLOSED/DEGRADED/OPEN/HALF_OPEN all reported (M4.2, incl. DEGRADED at 5/7 and HALF_OPEN probe); backoff escalation present (M4.2); in-memory only, single half-open probe; no persistence | A |  |
| PARITY.md:458 | Connection cooldown (one key/account) | PARTIAL | settings.ts:105-116; constants.ts transientCooldown 5000/3000 | Hard cooldown 5 s doubling, capped 5 min; soft-demote mode; per-key dead-key cooldown 5 min. No per-auth base, no anti-thundering-herd guard | A |  |
| PARITY.md:472 | Deadline-failure sub-budget ending combo with clean 504 | PARTIAL | comboPredicates.ts:189-226; executeTargetAttempt.ts:1344-1438 | DeadlineBudget → clean 504 exists, but no readiness-deadline failure counting / sub-budget | A | Distinct from the Area 1 FULL combo-deadline row: this one is readiness-deadline failure counting / sub-budget. |
| PARITY.md:479 | Default cross-provider policy: up to 3 provider attempts; prefixed provider/model never rerouted | PARTIAL | docs/OMNIROUTE_PROVIDER_FAILOVER.md | No 3-attempt default; cap is MaxAttempts 5 over operator chain | A |  |
| PARITY.md:481 | Failover-before-retry + retry knobs | PARTIAL | comboConfig.ts:113,164-178 | Failover-first is default behaviour, but no maxRetries/retryDelayMs/maxSetRetries knobs | A |  |
| PARITY.md:482 | Declarative per-model fallback chains API (GET/POST/DELETE /api/fallback/chains) | PARTIAL | src/domain/fallbackPolicy.ts | Chains are operator config; no runtime CRUD API verified | A | DUP of §14b Fallback chains API row. |

### Area 3 — Providers / models catalog

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:500 | Providers with a routable base URL | PARTIAL | 281/282 registry entries have baseUrl(s) | 380/614 (62%) have BaseURLs; 16 templates/loopback; 234 cannot be added | A |  |
| PARITY.md:503 | Static per-provider model list | PARTIAL | Registry models: [] per provider; 1,628 pairs / 1,076 distinct (static) | No static list; models only from live /v1/models probe at add | A |  |
| PARITY.md:505 | models.dev sync | PARTIAL | modelsDevSync.ts; OFF by default; 86400 s interval; + LiteLLM sync | OFF by default; syncs prices + context windows only, operator values always win | A |  |
| PARITY.md:508 | Custom-provider prefix convention | PARTIAL | The 3 prefixes above | No openai-compatible-* prefix convention; generic custom only | A |  |
| PARITY.md:511 | Free-tier marking | PARTIAL | hasFree: true ×160; free-tier budget 465 entries / 34 pools / 51 free-forever | HasFreeTier ×154; flag stored but surfaced nowhere — zero references in server/dashboard | A | Flag exists (×154) but is surfaced nowhere; dashboard surfacing half is C (§8b). |
| PARITY.md:512 | Free-tier budget / pools / rankings | MISSING | As above, plus Radar catalog | No pools, no budget aggregation, no rankings; only per-provider flags + notes | A |  |
| PARITY.md:513 | Key-creation links | MISSING | 129 hint fields contain a key-creation URL; website on 381 entries | None: no field, no API exposure, no dashboard link | A | Data task: 129 key-creation hint links to collect (theirs has 129, ours 0). |
| PARITY.md:517 | Deprecated / retired handling | PARTIAL | providerRetirement.ts + REMOVED_PROVIDERS.md (5 removals) + blocklist test; deprecated ×4 | DeprecatedModels seed table, 16 entries (all OpenAI); /v1/models hides them by default; no provider-removal blocklist / takedown process | A |  |
| PARITY.md:518 | Verified-overlay data preservation | WRONG | n/a | Overlay replaces whole entry, dropping HasFreeTier for 11 providers (cerebras, cloudflare-ai, deepseek, fireworks, gemini, groq, mistral, nvidia, ollama-cloud, openrouter, vertex) | A | LIVE BUG (WRONG): All() replaces the whole entry (internal/catalog/catalog.go:402-414); verified entries set HasFreeTier only 3×. |

### Area 4 — Auth, keys, scopes, route guards

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:538 | MCP scope enforcement | PARTIAL | Opt-in, default OFF (scopeEnforcement.ts:113) | PARTIAL: MCP is stdio-only, 3 read-only tools, no spend path — but also no auth/scoping at all | A | STALE? — see §Stale flags #4 (M9.1 built MCP scopes; row says stdio-only, 3 tools). |
| PARITY.md:540 | Per-key policy fields | PARTIAL | ~40 fields on ApiKeyMetadata (apiKeys.ts:108-157) | PARTIAL: only expiry, RPM/TPM, per-key USD budget, per-key guardrails allowlist (genuinely enforced) | A |  |
| PARITY.md:541 | Per-key IP allowlist / blocklist | MISSING | ipAllowlist field (apiKeys.ts:137); enforcement not re-verified | MISSING: no IP field or check anywhere | A |  |
| PARITY.md:542 | Per-key device tracking | MISSING | Prior-report only: dashboard shows "{count} device(s)" — code not re-verified | MISSING: no device/IP/User-Agent tracking on keys | A |  |
| PARITY.md:543 | Key policy templates / key import | MISSING | Prior-report only (deep-study §1.7) — code not re-verified | MISSING: no template or gateway-key import | A |  |
| PARITY.md:548 | Dashboard authentication | MISSING | Password login (default CHANGEME), optional OIDC, requireLogin default true | MISSING: no dashboard password, session, or login of any kind; protection is loopback-bind only | A | DECISION — login system vs loopback-only model; backend auth work is A, UI is C. |
| PARITY.md:555 | Self-service key introspection | PARTIAL | GET /v1/me/status + GET /v1/me/sessions | PARTIAL: GET /v1/me/status is FULL and richer on budget/traffic; GET /v1/me/sessions does not exist | A | STALE? — see §Stale flags #5 (GET /v1/me/sessions exists since M5.6). |
| PARITY.md:556 | Key creation via API / dashboard | MISSING | Dashboard "Create API Key" flow + API creation | MISSING: no POST /admin/keys create route; dashboard "+ New router key" button explicitly unwired | A | Backend half is A (POST /admin/keys); dashboard button half is C (§8b). |
| PARITY.md:557 | OAuth / JWT gateway auth | MISSING | OAuth 2.0 PKCE + JWT + 32 OAuth providers | MISSING: gateway clients use gateway keys only; OAuth/JWT exist solely as upstream provider auth | A | DECISION — gateway-key model is by design; OAuth/JWT for gateway clients is a product decision. |

### Area 6 — Caching

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:609 | Semantic similarity method | PARTIAL | Real neural embeddings via provider: text-embedding-3-small, 1536-dim, 3 s timeout, fail-open | Hashed bag-of-words, 256-dim, unigrams+bigrams, cosine — NOT neural embeddings, honestly documented | A |  |
| PARITY.md:617 | Persistence tiers | PARTIAL | Dual-layer: in-memory LRU + SQLite + optional Redis vector store | SQLite persistence for both layers, write-through (every Set reaches disk synchronously), durable flush (disk rows deleted at once), final Sync + Close on clean shutdown; Redis vector store still missing (M6.2, M6.2b) | A | Remaining: Redis vector store (SQLite tiers live). |
| PARITY.md:626 | Cache analytics | PARTIAL | cache_metrics table; getCacheStats(); GET /api/cache/stats; /dashboard/cache | GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only); no /dashboard/cache page (M10) (M6.8, M6.2b) | A | API live (M6.8); remaining /dashboard/cache page is C (M10). |
| PARITY.md:629 | Cache-write tiers (5-min / 1-h) | PARTIAL | FULL. Anthropic ephemeral_5m/1h split into tokens_cache_creation_5m/_1h + tier status; costed per tier (1.25×… | Ledger stores + costs per tier (5m 1.25×, 1h 2×, explicit-rate-wins, claude-gated fallback); pipeline live wire → provider → ledger; dashboard still estimates write cost as 1.25×–2.5× band (M6.9, M6.9b) | A |  |
| PARITY.md:630 | Provider prompt-cache markers | PARTIAL | cache_control used in executors (base.ts, bedrock.ts, vertex.ts, chatCore.ts) | LIVE on Anthropic only (post-conversion annotation in anthropicClient, temp 0, no tools, ≥1024 est. tokens); OpenRouter unwired (OpenAI wire has no cache_control path; Anthropic-endpoint variant not guaranteed Claude); no system/tools marking; client-supplied cache_control dropp… | A |  |
| PARITY.md:633 | Reasoning cache (2 h) | PARTIAL | reasoningCache.ts:37 (2 h TTL) | Memory-only replay: 2 h TTL, 200 entries, tool-call + conversation keys, full signed state stored/injected (M6.6b shadowing fix), chunk-stream injection, serve-mode wired; no DB tier; temp-gate divergence (router store below still captures hot turns); ChatStreamFrom text-resume … | A |  |
| PARITY.md:634 | Search cache (60 s) | PARTIAL | searchCache.ts:12,150 (60 s TTL, with coalescing) | Semantics fully implemented + tested (generic SearchCache[T], 60 s TTL, 500 entries, GetOrCoalesce, ttl≤0 bypasses cache AND coalescing) but the cache itself is still unwired to the search path (M6.6); POST /v1/search is now live (M8.20, wired merge wave — guard.go:234, server.g… | A | Route now live (M8.20); remaining gap is wiring the cache to the search path. |

### Area 7 — Guardrails, PII redaction, content policy

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:644 | Phone PII | PARTIAL | phone_us, phone_br (piiSanitizer.ts:80,87) | phone rule: +91, +1, 3-3-4, 10-digit IN only; no BR/other countries | A |  |
| PARITY.md:645 | SSN | MISSING | ssn (piiSanitizer.ts:63), ssn_us request-side | No SSN pattern anywhere | A |  |
| PARITY.md:646 | Credit card | PARTIAL | credit_card with Luhn validation (piiSanitizer.ts:258) | card rule matches any 16 digits; no Luhn — false positives on any 16-digit run | A |  |
| PARITY.md:648 | CPF / CNPJ (BR IDs) | MISSING | Full, with checksum validators (piiSanitizer.ts:261,264) | None | A |  |
| PARITY.md:649 | IP address v4/v6 | MISSING | ip_address, ipv6_address | None | A |  |
| PARITY.md:650 | IBAN + EU national IDs (8 GDPR types) | MISSING | iban, eu_vat, es_dni_nie, it_codice_fiscale, pl_pesel, nl_bsn, de_steuer_id, se_personnummer | None | A |  |
| PARITY.md:651 | ZA IDs (4 POPIA types) | MISSING | za_id, za_phone, za_vat, za_tax_ref | None | A |  |
| PARITY.md:652 | Provider-secret patterns | PARTIAL | 28 named patterns in CREDENTIAL_PATTERNS (credentialPatterns.ts) | 20 named secret rules + generic-token rule. Missing: Replicate r8_, Linear, Notion, npm, Postman, Discord, Square, Twilio, Mailgun, connection strings, generic sk- | A |  |
| PARITY.md:657 | Connection strings (mongo/postgres/mysql/redis/amqp) | MISSING | connection_string pattern | None | A |  |
| PARITY.md:659 | Per-type toggles | MISSING | 23 builtin types (piiTypes.ts), per-type selection, 8 dashboard presets, GET/PUT /api/settings/pii | Only 3 kind toggles (BlockPII/BlockSecrets/BlockInjection); no per-type selection | A |  |
| PARITY.md:660 | Custom regex patterns | MISSING | Up to 20, safe-regex validated, test endpoint (piiCustomPatterns.ts) | None | A | Also cited by §14b guardrails-test row as still MISSING. |
| PARITY.md:661 | Reversible PII token vault | MISSING | Opt-in PII_REVERSIBLE_VAULT_ENABLED, tokens minted pre-call, restored at exit | None | A |  |
| PARITY.md:662 | Request rewrite before provider | MISSING | Credential/PII maskers return modifiedPayload — secrets stripped pre-call (opt-in) | Log-only by design: Check/CheckScoped never mutate the request; with blocking off a secret goes to the provider verbatim | A |  |
| PARITY.md:663 | Response sanitization, non-streaming | PARTIAL | Opt-in PII_RESPONSE_SANITIZATION, modes redact\|warn\|block (default redact) | Response checked but block-or-deliver only: no redact mode | A |  |
| PARITY.md:664 | Response sanitization, streaming (SSE) | MISSING | Opt-in SSE transforms: PII + credential stream transforms | Streaming responses are never response-checked; only async-guardrail cut can stop a stream | A |  |
| PARITY.md:665 | Prompt-injection detection | PARTIAL | 7 shared patterns (inputSanitizer.ts) + 2 guard patterns, severity-scored, modes block\|warn\|log, 16 KB scan b… | 11 phrase patterns. No severity scoring, no custom patterns; scans full text (stronger coverage, unbounded CPU) | A |  |
| PARITY.md:674 | Modality bridges (vision/audio/video reroute) | MISSING | Vision 5, audio 6, video 7 (registry.ts:308-310) | None | A |  |
| PARITY.md:676 | Dashboard + counters | PARTIAL | No guardrails page; PII card in Settings → Security; per-type detection counters (in-memory) | Full Guardrails page: blocking toggles, per-(phase,action) eval counts, per-kind counts, last 50 events, security refusals. Counters per-kind (3) only — no per-type/per-pattern totals | A | Remaining: per-type/per-pattern counters (per-kind only today). |
| PARITY.md:677 | Route coverage of checks | PARTIAL | Verified call sites only: pre-call chat.ts:881, post-call nonStreamingResponse.ts:830 (chat path) | Request check on chat, /v1/messages relay, /v1/responses, embeddings and rerank; MCP tool args; response check on chat + Responses only — relay responses and all streaming responses unchecked | A |  |

### Area 8a — Dashboard: Home / Providers / Models (+ Pricing)

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:689 | 4-step quick-start (create key → connect → point client → monitor) + Full Docs link | PARTIAL | HomePageClient.tsx:975-1060; en.json:1981-1989 | No 4-step guide. Overview has "Point your app at the router" code block; Providers has a 3-step strip. No create-key step, no monitor step, no Full Docs link | C |  |
| PARITY.md:690 | Quick-start show/hide toggle (default on) | MISSING | HomePageClient.tsx:225,239 | No toggle found | C |  |
| PARITY.md:691 | Provider topology widget | PARTIAL | HomeProviderTopologySection.tsx, wired :1060 | Provider health card with status chips + 24h uptime bars + p50 — no topology view, no per-provider model counts | C |  |
| PARITY.md:693 | System metrics cards (CPU, Memory, host + process) | MISSING | HomeSystemMetricsCards.tsx:188-230 | No CPU/memory card on Overview (memstats live in Benchmark view) | C |  |
| PARITY.md:694 | Provider quota widget | MISSING | ProviderQuotaWidget.tsx (component exists) | No quota widget on Overview | C |  |
| PARITY.md:695 | Update flow (Update Available, Check, Update Now, Restart & Install, Release Notes, downloads) | MISSING | HomePageClient.tsx:151-172; UpdateProgressDialog.tsx | No update UI anywhere on Overview | C |  |
| PARITY.md:699 | Provider groups as always-visible addable sections | PARTIAL | 15 groups, providers/page.tsx:1018-1853 | One flat searchable catalog table (configured pinned on top, Add/Manage per row, ToS-grey Add disabled with reason). No grouping | C |  |
| PARITY.md:700 | Per-group "Test all" buttons | MISSING | Per-group testAll* titles throughout | Zero "Test all" strings in our file | C |  |
| PARITY.md:701 | Provider Onboarding Wizard | MISSING | :914, providers/new (942 lines) | Single add-provider form only; no wizard | C |  |
| PARITY.md:702 | Import from file / OAuth export-import modals | MISSING | :946 | None | C |  |
| PARITY.md:703 | Repair env (restore missing OAuth defaults) | MISSING | :1124 | None | C |  |
| PARITY.md:704 | Add OpenAI / Anthropic / CC Compatible buttons | PARTIAL | Groups + addCcCompatibleLabel (:1853) | Custom mode = "any OpenAI-compatible base URL"; no explicit Anthropic- or CC-compatible add buttons | C |  |
| PARITY.md:705 | Per-provider card: status, Disabled, Rate-limited, error types, expiration banner | PARTIAL | :174-177, :398-412 | Health chips, verification badges, uptime bars, p50 — no error-type breakdown, no expiration banner | C |  |
| PARITY.md:706 | Provider detail page (connections, quota panel, cooling, models, override editors, playground, import modals) | MISSING | providers/[id] (1,027 lines) | No detail view; Manage scrolls to the grid card | C |  |
| PARITY.md:707 | Search (text + model search + category/serviceKind filter) | PARTIAL | :252-291 | Text search over name/id/alias; no category/serviceKind filter | C |  |
| PARITY.md:708 | Configured-only filter / configured count | PARTIAL | filterConfiguredProviderEntries (:42) | Configured pinned on top; no configured-only filter toggle | C |  |
| PARITY.md:709 | Free Tier Providers group | MISSING | :1277 | No free-tier grouping; catalog table has no free-tier column | C |  |
| PARITY.md:710 | Test Results modal ("{passed}/{total} passed") | MISSING | :1871 | None | C |  |
| PARITY.md:711 | Add-form fields (Provider, Connection name, Display name, Provider prefix, Region, Protocol, Base URL, Chat/M… | PARTIAL | providers/new, provider detail | Ours: Source, catalog select, Endpoint, Variant, Base-URL override, Display name, Provider API key, Gateway key. No Connection name, Provider prefix, Region, Protocol picker, Chat/Models path, Custom User-Agent | C |  |
| PARITY.md:712 | OAuth connect flow (sign-in, token rotation) | MISSING | OAuth groups + OAuth modals | API-key-only add; OAuth entries ToS-grey-disabled or API-key surfaced; no OAuth connect flow | C |  |
| PARITY.md:713 | Models title/subtitle "Browse and health-test models and combo routing pipelines" | PARTIAL | models/page.tsx | "Models / Only models from your connected providers" — no health-test or combo mention | C |  |
| PARITY.md:714 | Tabs Models (n) / Combos (n) | MISSING | CatalogTabs, page.tsx:718-720 | No tabs in Models view | C |  |
| PARITY.md:715 | Models table columns (Provider/Model/Type/Capabilities/Context/Max output/Flags/Custom/Free) | PARTIAL | page.tsx:900-910 | Ours: Model/Provider/Context/Input per 1M/Output per 1M/Price source/Requests/Fit/Status. Missing Type, Capabilities, Max output, Flags, Custom, Free | C |  |
| PARITY.md:716 | Filters (provider/type/capability/min-context/min-output/free, etc.) | PARTIAL | ModelCatalogFilters.tsx, CatalogFilterFields.tsx | Provider select, Category select, Max price, Min context, Shortlist only, Include deprecated, text search. Missing Type, Capabilities, Free-only | C |  |
| PARITY.md:717 | "{visible} visible · {hidden} hidden · {total} total" counts | PARTIAL | page.tsx:648,710 | #modelCount element exists; no visible/hidden split observed | C |  |
| PARITY.md:718 | Per-row visibility toggle | MISSING | onToggleVisibility, useCatalogVisibility.ts | None | C |  |
| PARITY.md:719 | Per-row health test + test-result badge | MISSING | testSingleModel, CatalogTestBadge.tsx | None per model (health exists per provider only) | C |  |
| PARITY.md:720 | Bulk test ("Start bulk test?" + quota warning) | MISSING | CatalogBulkActionBar.tsx | None | C |  |
| PARITY.md:722 | Empty states ("No models are available yet" / "No models match these filters" / "Clear filters") | PARTIAL | Prior report §2.2 | "No models yet — connect a provider first. [Add a provider]" (one line + button). No "no match" state, no Clear filters | C |  |
| PARITY.md:723 | Pricing as separate per-model rate table (Input/Output/Cached/Reasoning/Cache Write $/1M + By provider/Compar… | MISSING | costs/pricing/ (route exists; columns per prior report, not re-verified) | No Pricing page/view at all — zero "pricing" matches in our file | C |  |
| PARITY.md:724 | Pricing sync (Auto Sync, Sync Now, Last/Next sync, Synced Models, Clear Synced; sources User Override / Model… | PARTIAL | Prior report §2.2 | Only a status line #modelSyncStatus ("Live price/context sync: checking…"); no sync controls | C |  |
| PARITY.md:725 | Catalog Sections grouping | MISSING | page.tsx ("Catalog Sections") | Flat table, no sections | C |  |
| PARITY.md:726 | Pagination | MISSING | getCatalogPage, PAGE_SIZE | Not found in source (table renders full list) | C |  |

### Area 9 — MCP server / A2A server

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:796 | MCP SSE remote transport | MISSING | httpTransport.ts:8-9,405-435 — GET/POST /api/mcp/sse | None — stdio only | B | STALE? — see §Stale flags #6 (live since M9.2). |
| PARITY.md:797 | MCP Streamable HTTP remote transport | MISSING | httpTransport.ts:9,393-403 — POST/GET/DELETE /api/mcp/stream | None | B | STALE? — see §Stale flags #6 (live since M9.2). |
| PARITY.md:798 | Tool breadth | PARTIAL | 110 canonical (+1 perplexity + N dynamic skill_*) | 3 read-only tools | B | Genuinely PARTIAL: 24 tools live now (not 3) vs their ≥110 — the long tail remains. |
| PARITY.md:799 | Routing/combo tools | MISSING | omniroute_list/create/switch_combos, get_combo_metrics, set_routing_strategy, best_combo_for_task, explain_ro… | None | B | STALE? — see §Stale flags #6 (9 tools live in internal/mcp/tools_routing.go). |
| PARITY.md:800 | Inference/spend tool | MISSING | omniroute_route_request (real chat completion through routing) | None, deliberate — no sampling/chat tool, "a tool call can never spend quota" | B | DO-NOT-BUILD — deliberate divergence: "a tool call can never spend quota" (PLAN M9). Needs DIVERGENCE verdict, not a slice. |
| PARITY.md:801 | Cost/quota tools | MISSING | omniroute_cost_report, omniroute_check_quota, omniroute_set_budget_guard | None on MCP; per-request budget header exists only on HTTP chat | B | STALE? — see §Stale flags #6 (cost_report, check_quota, set_budget_guard live). |
| PARITY.md:802 | Cache tools | MISSING | omniroute_cache_stats, omniroute_cache_flush (destructive) | None | B | STALE? — see §Stale flags #6 (cache_stats, cache_flush live). |
| PARITY.md:803 | DB health + pricing sync | MISSING | omniroute_db_health_check (autoRepair:true rewrites DB rows); omniroute_sync_pricing | None | B | STALE? — see §Stale flags #6 (db_health, db_stats live; pricing sync still absent). |
| PARITY.md:804 | Memory tools | MISSING | omniroute_memory_search/add/clear (memory_clear destructive) | None | B | STALE? — see §Stale flags #6 (memory_search/add/clear live). |
| PARITY.md:805 | Skills tools | MISSING | omniroute_skills_list/enable/execute/executions, github_skills_*, agent-skills trio, dynamic skill_<name> | None | B | DO-NOT-BUILD — skills execution / dynamic skill_* is an arbitrary-code surface; needs its own security design (PLAN M9, M12). |
| PARITY.md:806 | Plugin tools | MISSING | plugin_list/install/activate/deactivate/uninstall/configure/executions/scan | None | B | DO-NOT-BUILD — plugin install/activate over MCP contradicts compiled-in plugins (PLAN M9). |
| PARITY.md:807 | Context-source tools | MISSING | Notion 6, Obsidian 22 (incl. delete_note, move_note, execute_command), local-corpus 3 | None | B | DO-NOT-BUILD — external SaaS (Notion/Obsidian/local-corpus), not router core (PLAN M9). |
| PARITY.md:808 | Pool/gamification/compression/CCR tools | MISSING | Pool 6, gamification 8, compression 5 + CCR 6 + RTK 2 | None | B | DO-NOT-BUILD — gamification/pool tools are not router core (PLAN M9). |
| PARITY.md:809 | Tool search | MISSING | omniroute_tool_search (toolSearch/register.ts, server.ts:671) | None (nothing to search at 3 tools) | B | STALE? — see §Stale flags #6 (tool_search + heartbeat live, internal/mcp/tools_search.go). |
| PARITY.md:810 | Perplexity research tool | MISSING | omniroute_perplexity_research, scope ["manage"] (server.ts:591) | None | B | DO-NOT-BUILD — third-party key, research feature (PLAN M9 → M12). |
| PARITY.md:812 | Scope system (33 tool scopes) | MISSING | 33 scopes verified fresh; per-tool scopes; * and trailing-* wildcard matching | No scope concept on MCP at all | B | STALE? — see §Stale flags #6 (scopes.go; enforcement ON by default, M9.1). |
| PARITY.md:813 | Scope enforcement default | MISSING | OFF by default (featureFlags.ts:127-136; MCP-SERVER.md:374,506) | No enforcement (nothing to enforce on 3 read-only tools) | B | STALE? — see §Stale flags #6 (ON by default, no off switch — scopes.go:10-11). |
| PARITY.md:814 | Forced enforcement for narrow HTTP keys (#13330) | MISSING | shouldForceScopeEnforcement() (scopeEnforcement.ts:120-124) | None (no remote transport, no per-key auth) | B | STALE? — see §Stale flags #6 (remote transports + per-key auth live since M9.2). |
| PARITY.md:816 | Per-client identity | PARTIAL | HTTP/SSE: per-key api_keys.scopes via httpAuthContext.ts; stdio: none | Stdio also has no per-client identity (same gap, but only 3 read-only tools) | B | STALE? — internal/mcp/identity.go exists (M9.1); verify stdio identity coverage before flipping. |
| PARITY.md:817 | Audit logging of tool calls | MISSING | SQLite mcp_tool_audit, inputs SHA-256-hashed, outputs truncated to 200 chars (audit.ts:1-12) | No MCP audit log; only in-process guardrail evaluation counters | B | STALE? — see §Stale flags #6 (internal/mcp/audit.go, M9.1). |
| PARITY.md:818 | Heartbeat / status telemetry | PARTIAL | runtimeHeartbeat.ts; getMcpHttpStatus() | GET /dashboard/mcp feed: same 3-tool list, live provider/model counts, selector state + counters; no heartbeat, no session tracking | B | STALE? — heartbeat tool live (tools_search.go:26); session tracking still to verify. |
| PARITY.md:819 | Tool-cardinality controls | PARTIAL | reduceToolManifest() supports allowScopes + maxTools + allow/deny, but env exposes only MCP_TOOL_DENY / MCP_T… | Server announces all 3 always; client side exceeds OmniRoute: selector mode injects exactly 2 meta-tools | B | STALE? — WithToolFilter allow/deny + WithMaxTools live (tools_search.go:88,375). |
| PARITY.md:821 | Spend controls on MCP tools | PARTIAL | route_request schema carries optional budget USD (schemas/tools.ts:342); handler enforcement not traced (unve… | Server: no spend possible by construction. Client: mcp_tools_execute has no spend cap, cost estimate or allowlist | B | STALE? — see §Stale flags #6 (allowlist + spend cap + cost estimate, internal/mcptools/controls.go, M9.6). |
| PARITY.md:824 | Dashboard MCP page/feed | PARTIAL | /dashboard/mcp + Endpoint tabs, MCP Audit page, transport toggles, "110 tools, 33 scopes" | GET /dashboard/mcp + "MCP & CLI" section incl. selector-mode card, audit view and transport toggles (POST /api/mcp/status); 24 tools, 11 scopes | B | STALE? — row itself already says "24 tools, 11 scopes"; needs re-verdict, not a build. |
| PARITY.md:825 | Protocol version | PARTIAL | SDK-negotiated; doc example 2025-03-26 (MCP-SERVER.md:72) | 2025-03-26 — current surface; neither side ships sampling/elicitation | B |  |
| PARITY.md:828 | A2A JSON-RPC endpoint | MISSING | POST /a2a: message/send, message/stream (SSE), tasks/get, tasks/cancel + v1.0 aliases (a2a/route.ts:217-337) | None — no /a2a route, no JSON-RPC agent endpoint | B | STALE? — see §Stale flags #7 (POST /a2a live, gateway-key auth, owner-scoped, M9.8). |
| PARITY.md:829 | A2A skills | MISSING | 6 handlers: smart-routing, quota-management, provider-discovery, cost-analysis, health-report, list-capabilit… | None | B | STALE? — see §Stale flags #7 (all 6 skills live, internal/a2a/skills.go:19-24, M9.9). |
| PARITY.md:830 | A2A REST task routes | MISSING | GET/POST /api/a2a/tasks, GET /api/a2a/tasks/[id], cancel, history, status | None | B | Genuinely still MISSING: no REST route registered (POST /a2a core exists; the only tree hit is a test asserting the dashboard body contains that string — internal/server/a2a_dashboard_test.go:48). |
| PARITY.md:831 | Agent Card / discovery | MISSING | GET /.well-known/agent.json (public, max-age=3600) + agent-card.json | None | B | STALE? — see §Stale flags #7 (GET /.well-known/agent.json live, internal/server/a2a_card.go, M9.9). |
| PARITY.md:832 | A2A auth | MISSING | Keyless by default — verified fresh (authenticate.ts); GHSA-v54m-6rm3-p565, GHSA-jcm5-6wpp-wjj8 | None — no A2A surface to mis-authenticate | B | STALE? — see §Stale flags #7 (auth ON by default, never keyless — the opposite of their default; M9.8). |
| PARITY.md:833 | A2A owner scoping | MISSING | Owner = SHA-256 prefix of API key; ownerless tasks visible to everyone, by design | None | B | STALE? — see §Stale flags #7 (owner = caller key hash, guard.go:241, M9.8). |
| PARITY.md:834 | Conductor delegation | MISSING | POST /api/a2a/tasks delegates coding work to OmniConductor fleet on server-side token | None | B | DO-NOT-BUILD — server-side fleet token delegation; MUST NEVER copy (PLAN M9). |
| PARITY.md:835 | A2A task persistence / states / streaming | MISSING | SQLite a2a_tasks + a2a_task_events; submitted → working → completed \| failed \| cancelled | None | B | STALE? — internal/a2a/store.go exists (M9.8); verify states/streaming coverage before flipping. |
| PARITY.md:836 | A2A enable flag + dashboard/audit | MISSING | a2aEnabled default false; /api/a2a/status; /dashboard/a2a; MCP/A2A audit pages | None | B | STALE? — a2a_enabled flag + /api/a2a/status exist (M9.8/M9.9); dashboard page state to verify. |

### Area 10 — Compression, token counting

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:847 | Engine registry / stacked pipelines | MISSING | 15 registered ids, stackPriority ordering, stackedPipeline config, strategySelector.ts | Fixed 3-step pipeline only; no registry, no per-engine selection | A |  |
| PARITY.md:848 | Lite whitespace/format cleanup | PARTIAL | Lite engine, <1ms, whitespace/dedup; tool-result truncation to cap only when opted in | Whitespace strip outside fences + system dedupe; no tool-result truncation cap | A |  |
| PARITY.md:849 | Session-dedup (cross-turn, content-addressed, [dedup:ref sha=…]) | MISSING | Engine, stackPriority 3, lossless | Only same-request duplicate system messages; no cross-turn store, no markers | A | Needs a cross-turn store design (queued behind M7.1 harness). |
| PARITY.md:850 | CCR (retrieval markers, 24 h + SQLite) | MISSING | Engine, stackPriority 4, lossless | None | A | Needs a cross-turn store design (queued behind M7.1 harness). |
| PARITY.md:851 | RTK tool-output filter (56 JSON packs, ANSI/progress strip, failures+tails kept) | PARTIAL | Engine, levels minimal/standard/aggressive, "60–90%" claim | Built + wired into Apply via mode=rtk / mode=stacked (explicit mode only; a bare lossy request stays extractive): FilterRTKMessages (rtk.go:811), 18 packs implemented (of OmniRoute's 56), ANSI/progress strip, failures+tails kept (first pass line kept as representative). Not full… | A | Genuine PARTIAL: 18/56 JSON packs; standard level only in Apply; no live upstream verification. |
| PARITY.md:852 | Codex Responses tool-output compression | MISSING | Engine, stackPriority 12, lossless-first | None | A |  |
| PARITY.md:853 | Headroom tabular JSON → columnar (SmartCrusher, lossless) | MISSING | Engine, stackPriority 15 | None | A | Deferred: needs a tabular corpus + columnar round-trip proof (PLAN M7). |
| PARITY.md:854 | Ionizer tabular row sampling | MISSING | Engine, stackPriority 13 | None | A | Deferred: needs a tabular corpus + row-sampling proof (PLAN M7). |
| PARITY.md:855 | Relevance extractive sentence scoring | MISSING | Engine, stackPriority 18, preserve-regex | None | A |  |
| PARITY.md:856 | Caveman rule-based prose condensation (levels lite/full/ultra, user-role default, min 50 chars) | PARTIAL | Engine, stackPriority 20, 59 en rule nodes, 11 locales | Built + wired into Apply via mode=standard / mode=stacked (explicit mode only): CondenseMessages (caveman.go:161), levels lite/full, user-role default, min 50 chars, extractive + rule-based only, no model calls. Not full: no ultra level, no per-request level, English-only rules … | A | Genuine PARTIAL: no ultra level, no per-request level, English-only rules. |
| PARITY.md:857 | Aggressive summarize + progressive aging (~50% claim) | MISSING | Engine, stackPriority 30; summarizer.ts:198 emits [COMPRESSED:summary] | Oldest-turn dropping only, no summariser (honestly documented) | A | Needs a summariser; LLM compressor is banned, so extractive-only or not at all (PLAN M7). |
| PARITY.md:858 | LLMLingua-2 ONNX semantic pruning (~57 MB TinyBERT, fail-open) | MISSING | Engine, stackPriority 35, worker thread | None | A | DECISION — evaluate, don't force: ~57 MB TinyBERT vs binary-size budget (PLAN M7). |
| PARITY.md:859 | Ultra heuristic pruning + code thinning + binary-search truncation (~75% claim) | MISSING | Engine, stackPriority 40 | None (lossy truncation is not Ultra) | A |  |
| PARITY.md:860 | OmniGlyph context-as-image (Claude Fable 5 only) | MISSING | Engine, stackPriority 90, experimental | None — deliberate do-not-build | A | DO-NOT-BUILD — lossy, exempt from the fidelity gate; NEVER build (PLAN M7). |
| PARITY.md:861 | LLM compressor (model call to compress) | MISSING | Engine, stackPriority 38, opt-in default-off | None — deliberate do-not-build | A | DO-NOT-BUILD — model call to compress; NEVER build (PLAN M7). |
| PARITY.md:862 | Read-lifecycle engine | MISSING | Engine, stackPriority 5 | None | A |  |
| PARITY.md:863 | MCP tool/prompt/resource description compression, default ON | MISSING | types.ts:429 (true); applied at registration (mcp-server/server.ts:258-310) | None — we never rewrite tool descriptions, so we do not carry this correctness risk | A | DO-NOT-BUILD — we never rewrite tool descriptions (correctness position); stays MISSING-by-design (PLAN M7). |
| PARITY.md:864 | MCP accessibility tool-result filtering (smartFilterText, truncation hints) | MISSING | engines/mcpAccessibility/index.ts, server.ts:265-285 | Only last-per-tool_call_id canonicalisation | A |  |
| PARITY.md:865 | Stacked pipeline RTK → Caveman | PARTIAL | stackedPipeline: [{rtk standard}, {caveman full}] in default config (enabled-only, default is off) | RunStacked wired into Apply via mode=stacked with DefaultAvailability (RTK+Caveman live; stale false/false fixed at merge): lossless → RTK → Caveman, per-stage accounting with inflation discard + fail-closed counting, fidelity/inflation gates on the final output. Not full: no li… | A |  |
| PARITY.md:866 | Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked) | PARTIAL | COMPRESSION_GUIDE.md:35-126; autoTriggerMode: "lite" | Modes parsed + validated (ParseMode; server 400s an unknown mode naming the field) and wired into Apply: rtk/standard/stacked run their engines, aggressive/ultra run their available subset (extractive pair only), a bare lossy request stays extractive, a mode alone never enables … | A |  |
| PARITY.md:867 | Adaptive budget dial + escalation ladder | PARTIAL | Ladder session-dedup→rtk→headroom→lite→caveman→aggressive→ultra | PickCheapestFitting + EscalateFrom built + tested (modes.go), ladder lite→standard→stacked; NOT wired as the default — mode selection is explicit-only (see modes row). Row stays PARTIAL with this reason | A |  |
| PARITY.md:868 | Cache-aware compression (preserve system prompt, downgrade, prefixFreeze, QuantumLock) | MISSING | cachingAware.ts, prefixFreeze.ts, preserveSystemPromptMode: "always" default | Built but UNWIRED (internal/degrade/cache_aware.go: PrefixFreeze, ResolveCacheAwareStrategy, ResolvePreserveSystemPrompt — tested, no caller in Apply; HardBudgetOptions.FreezeSystemPrompt stays false). The merge-wave wire list names RTK/Caveman/modes/hard-budget/calibration only… | A | Built but UNWIRED (internal/degrade/cache_aware.go) — wire it when a real caller lands; small slice. |
| PARITY.md:869 | System-prompt preservation modes (always/whenNoCache/never) | PARTIAL | types.ts:217-240, default always | System/developer always pinned in lossy; no configurability, no cache-conditional mode | A |  |
| PARITY.md:870 | Hard-budget post-pass (compress to ≤ N tokens) | PARTIAL | hardBudget.ts ("compress to ≤ N cl100k tokens") | Wired: post-pass runs after applyLossy when the count is still over budget (same transform class, so it also runs on the bare lossy path); the 4 real inflation cases (131→140, 131→137, 131→132, 565→571) now FIT via the post-pass (120/116/112/555, probe-measured); fidelity gate m… | A |  |
| PARITY.md:871 | Protection guarantees (code/URLs/JSON/identifiers) | PARTIAL | "Always preserves" list; validation.ts refuses rewrites dropping code/URLs/headings/versions/ALL-CAPS ids | Code fences protected in whitespace step; part-carrying messages never partially rewritten; M7.2 fidelity gate WIRED into Apply — lossy cuts that damage URLs/JSON/code are refused and the original is sent (24 CheckLossy tests incl. 6 paren-URL regression tests; paren-URL hole F1… | A |  |
| PARITY.md:872 | Fidelity gate / risk gate / validation | PARTIAL | Opt-in fidelityGate/riskGate, validation.ts, fidelityGateStep.ts | VerifyLossless structural equivalence check (normal-form + idempotence, fail → send original) for lossless; M7.2 fidelity gate WIRED on the lossy path — CheckLossy(out, after) in Apply, refusal returns the original with the single step fidelity_gate_refused, served via server ap… | A |  |
| PARITY.md:874 | Worker pool / async compression | PARTIAL | compressionWorkerPool.ts, compressionWorker.ts, ONNX worker | Built + unit-tested 10/10 under -race and independently probed (Submit mutex fix toggle-proven by TestPoolSecondBlockedSubmitterCancel; cancel, shutdown drain, goroutine baseline clean). NOT wired: no genuine background consumer exists (all go func background sites checked — rel… | A | Built + tested, deliberately UNWIRED — no genuine background consumer exists; keep PARTIAL, do not force. |
| PARITY.md:875 | Eval harness + measured savings corpus | PARTIAL | compression/eval/, harness/ (benchmark, replay, budgetGate); one verified 95.93% test | Eval harness internal/compress/evalharness/ (harness + fixed corpus + goldens; 8 tests, -race green, toggle-verified) + measured table docs/compression-savings-2026-11-18.md (13 cases; lossless 118/1,200 saved 9.8%; lossy 480/1,200 40.0%; no case inflated; lossy misses budget on… | A |  |
| PARITY.md:876 | Output styles (Terse prose, Less code, Ponytail, "I have ADHD", Terse CJK) | MISSING | 5 styles (outputStyles/catalog.ts:53-301) | None | A | Lowest priority — cosmetic (PLAN M7: M12 at most); pulled into A only by the pull-forward rule. |
| PARITY.md:877 | Language packs for rules (11 locales) | MISSING | rules/{de,en,es,fr,hu,id,it,ja,pt-BR,ru,zh}/ | None (English-only logic; no prose rules at all) | A | Lowest priority — cosmetic, English-only logic (PLAN M7: M12 at most). |
| PARITY.md:882 | Native non-OpenAI tokenizers (Claude/Gemini/Qwen) | MISSING | None — cl100k/o200k only | None — cl100k/o200k only, Fallback=true label | A |  |
| PARITY.md:887 | Own-cache tokens_saved accounting | MISSING | semanticCache.ts:59,368,389 — tokens_saved per entry + cache_metrics table | Own exact/semantic hits record zero tokens: degrade.go:710 ("the ledger … records nothing") — verified fresh | A | CONTRADICTORY — see §Stale flags #8 (Area 6 row 628 says this is FULL). |
| PARITY.md:890 | Runtime calibration vs provider-reported usage | PARTIAL | EMA per (provider, model, tools) bucket, α=0.25, ≥3 samples, clamped [0.15, 2.0] | Built + wired (M7.9 + merge wave): calibrator (calibration.go) matches OmniRoute's shape exactly (α=0.25, ≥3 samples, clamp [0.15, 2.0], 100-token floor); reconcileUsage records the UNCALIBRATED local vs the reported count (toggle-proven: feeding the calibrated local decays the … | A |  |

### Area 11 — Analytics, usage, ledger, cost

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:901 | Session / project / client attribution + /v1/me/sessions | MISSING | agentSessions.ts:29-36,152-180; GET /v1/me/sessions and /[id] | No session/project/client field anywhere in ledger or server; no /v1/me/sessions | A | STALE? — see §Stale flags #5 (GET /v1/me/sessions exists since M5.6; attribution half still genuine). |
| PARITY.md:904 | Cost computation (tokens × price, all legs) | PARTIAL | computeCostFromPricing (costCalculator.ts:247-274): input/output/cache-read/cache-creation/reasoning, incl. F… | Prices.Cost prices prompt + completion only; no cached-input leg, no per-endpoint price dimension | A |  |
| PARITY.md:905 | Gateway-reported cost precedence / exact-cost reconciliation | MISSING | calculateCostDetailed (costCalculator.ts:354-359): provider-reported exact cost wins; RecordedCostMatcher rec… | provider.Usage has no cost field at all; cost is always hand-computed; no reconciliation target | A |  |
| PARITY.md:907 | Price coverage out of the box | PARTIAL | Synced models.dev + LiteLLM catalogues fill prices automatically | No LiteLLM sync, no hardcoded defaults; unpriced model = unknown, cost routing blind until operator prices it | A |  |
| PARITY.md:908 | Flat-rate / subscription $0 treatment | MISSING | flatRateProviders.ts (Codex, Claude Code, Kimi-coding, etc.) + flatRateAsZero opt-in | No flat-rate/subscription classification anywhere; a subscription provider with no configured price is "unknown", never $0 | A |  |
| PARITY.md:909 | Cache-write tier accounting (5-min vs 1-hour) | PARTIAL | cacheWriteEstimate.ts:55-56 (5m = 1.25×, 1h = 2×); cacheHealth.ts:239-240,279-305. Caveat: "Diagnostic-only… … | Single undifferentiated CacheWriteTokens total; dashboard economics uses a 1.25×–2.5× band (high end exceeds Anthropic's documented 2× cited in the code's own comment) | A |  |
| PARITY.md:911 | Refused / fallback-attempt billing | PARTIAL | UNVERIFIED on their side (no "refused" handling found in sampled files) | Every attempt recorded: success rows carry real tokens+cost; failed attempts recorded as ok=false zero-token rows. Blemishes: hedge loser recorded as a failed attempt though cancelled; a refusal the upstream actually billed for is invisible — no reported-cost field | A |  |
| PARITY.md:915 | Cost-saved reporting (cache / compression savings) | PARTIAL | X-OmniRoute-Cost-Saved on cache hits; tokens_saved per cache entry + cache_metrics table | Compression saved tokens recorded per row; cache-read-vs-full economics in dashboard feed — but own exact/semantic cache hits record zero tokens | A | Same underlying gap as Area 10 Own-cache tokens_saved row (see §Stale flags #8). |
| PARITY.md:922 | Analytics endpoints | PARTIAL | FULL (existence-verified) — /api/analytics/auto-routing, /compression, /diversity + /api/usage/* suite. Route… | /dashboard/usage, /dashboard/key-budgets, /dashboard/prices + GET /api/analytics/summary, /by-provider, /by-model, /costs live (M8.17, wired merge wave); still no auto-routing, compression, or diversity counterpart, no combo-health/utilization surface — the four G17 handlers for these routes (handleAnalyticsAutoRouting, handleAnalyticsCompression, handleAnalyticsDiversity, handleAnalyticsComboHealth) were built and review-verified (GAP-TRACK.md §10 REVIEW G17, 2026-11-19 ~17:10 IST) but REMOVED as dead code in the M10-15 cleanup: never registered (no s.register in server.go), never guarded (no guard.go row), never reachable over HTTP | A | Remaining: auto-routing / compression / diversity / combo-health surface must be REBUILT from scratch (backend, A) — not a registration-pending step, the G17 handlers no longer exist in the tree; dashboard page is C (M10). |
| PARITY.md:923 | Retention + export | PARTIAL | Retention configurable, 7-day default fallback + startup cleanup; log export incl. BigQuery destination | No retention/TTL on usage_events — grows unbounded; only manual wipe/reset. Export = DB-export bundle capped at 500 recent events, typed-confirm gated | A |  |
| PARITY.md:925 | Idempotency-Key billing safety | MISSING | UNVERIFIED — not sampled in OmniRoute code this pass | MISSING — zero hits in internal/ + cmd/; client retries after a timeout can double-charge | A |  |
| PARITY.md:926 | Batch / flex service-tier pricing | MISSING | Flex priced at 0.5× in cost calc (costCalculator.ts:187-189); batch API not sampled | MISSING — no service_tier handling; no batch API, no batch/flex price tier in the price registry | A | STALE? — see §Stale flags #9 (batch API exists since M8.8; the price tier is the genuine gap). |

### Area 12 — Latency, startup, memory, binary

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:959 | Hedging (parallel 2nd attempt) | PARTIAL | hedging: false, hedgeDelayMs: 500; bodies >256 KB never hedged | ON, HedgeDelay 150 ms; no body-size guard | A | STALE? — see §Stale flags #1 (256 KiB guard exists, internal/router/hedge.go:14-16,44). |
| PARITY.md:967 | Latency-aware routing / scoring | PARTIAL | 16-factor, latencyInv 0.1143, pool-relative; latencyAware strategy; telemetry p50/p95/p99 per target | 5 factors/mode, balanced latency 0.25; health-probe latency re-ranks chains. Missing: latency-aware strategy, p2c latency term, p99, model-latency-stats endpoint | A |  |
| PARITY.md:969 | Latency percentiles in telemetry | PARTIAL | p50/p95/p99 per provider (GET /api/telemetry/summary, model-latency-stats) | p50/p95 only, no p99. Health probe ring 48 h / 2,880 samples — ahead on history, behind on p99 | A |  |
| PARITY.md:976 | Measured failover-switch time (a number to quote) | WRONG | None published | Claimed "~53 ms failover / 1–2 ms switch" — unverified/mislabelled (see measured-numbers table) | C | Docs-claim fix, not a feature: stop quoting "~53 ms / 1–2 ms" (already marked UNVERIFIED/WRONG in the measured-numbers table). |

### Area 13 — Packaging, Docker, install, CLI

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:991 | Prebuilt binary install | MISSING | npm install -g omniroute, bun add -g omniroute, npx -y omniroute (README.md:782–784,1041); Docker Hub diegoso… | No release exists; only build-from-source works | C | RELEASE-BLOCKED — needs the first public release (bulk push); nothing to build until then. |
| PARITY.md:993 | npm package | PARTIAL | Published omniroute package, bin: {omniroute, omniroute-reset-password}; npm-publish.yml with OIDC Trusted Pu… | package.json exists (name: codvian-router, bin, version 0.1.0) + bin/codvian-router.js downloader wrapper — unpublished, downloads from releases that don't exist yet | C | RELEASE-BLOCKED — unpublished; downloads from releases that do not exist yet. |
| PARITY.md:994 | Published Docker image | PARTIAL | Docker Hub diegosouzapw/omniroute:latest + :X.Y.Z + :next, ~250 MB, multi-arch amd64+arm64 | No published image; GHCR target ghcr.io/raunakiitp/codvian-router only in .goreleaser.yml dockers_v2 config. Local image 20.8 MB per slice-4 report only | C | RELEASE-BLOCKED — GHCR target configured; publish happens at first release. |
| PARITY.md:996 | docker-compose | PARTIAL | 4 files: docker-compose.yml (440 lines, profiles base/web + redis + browser), .prod.yml, .selfhost.yml, .olla… | 1 file: single service, command serve --listen 0.0.0.0:20128, 2 named volumes, healthcheck. Parses; never executed in this VM | C |  |
| PARITY.md:999 | CLI breadth | PARTIAL | 97 files in bin/cli/commands/, ~90 register* functions, 723 .command( call sites (sampled count, not a distin… | 17 top-level subcommands: serve, init, wizard, run, keygen, eval, eval-feedback, validate, tray, doctor, configure, mcp, provider, catalog, verify-release, release, db, help (incl. default serve) | C |  |
| PARITY.md:1000 | --version / -v | MISSING | Fast-path --version/-V before heavy imports (bin/omniroute.mjs:14–17,60–66) | No --version/-v flag and no version subcommand (only Version = "0.1.0" const, printed on serve/init startup line) | C | STALE? — see §Stale flags #10 (-v/--version/version all work, main.go:490,562). |
| PARITY.md:1002 | Eval tooling | PARTIAL | eval command + scripts/router-eval/ + scripts/compression-eval/ + eval watch TUI | eval (smoke-eval, --model) + eval-feedback (A/B feedback heuristic) — narrower, no eval framework/watch UI | C |  |
| PARITY.md:1004 | Launch/wrap external CLIs | PARTIAL | run + launch + launch-codex + 18 setup-* commands (claude/codex/cursor/cline/kilo/roo/crush/zed/goose/aider/q… | Single generic run <cli> --model MODEL [-- args...] (ephemeral headless gateway + fresh key) — no per-tool setup commands | C |  |
| PARITY.md:1005 | Provider management CLI | PARTIAL | providers, provider-cmd, test-provider, provider-crud, models, model-crud, model-validation, combo, pricing | provider list/add/add-catalog/remove/import — functional but far narrower (no test-all, no model CRUD) | C |  |
| PARITY.md:1007 | Tray | PARTIAL | tray command (show/hide/quit) + bin/cli/tray/ (autostart, detached tray, Windows systray/PowerShell) + serve … | tray subcommand exists but is only a headless supervisor (Start/Stop/Restart, auto-restart, no GUI icon, no menu, no autostart) | C |  |
| PARITY.md:1008 | Desktop app (Electron) | MISSING | Full electron/ app (main.js, preload.js, electron ^45, electron-builder ^26, build:win/mac/linux) + electron-… | None — no Electron dir, no desktop build, no .app/.exe packaging | C | DECISION — full Electron app is a large scope item; coordinator decision whether it is in M11 scope at all. |
| PARITY.md:1009 | Self-update mechanism | MISSING | update command, update-notifier, autoUpdate.ts, Electron autoUpdater | None. No self-update code; /admin/update is explicitly notImplementedAdmin("auto-update"). Only path is re-running scripts/install.sh after a future release | C |  |
| PARITY.md:1010 | Releases published | MISSING | 283 releases per prior crawl (not re-verified); current dev version 3.8.55; electron-release.yml + npm-publis… | 0 releases, 0 git tags; release pipeline exists only as config (.goreleaser.yml, .github/workflows/release.yml — sole workflow file, runs on v*.*.* tag) | C | RELEASE-BLOCKED — 0 releases, 0 tags by the bulk-push rule; pipeline config exists. |
| PARITY.md:1011 | SBOM | PARTIAL | CycloneDX SBOM for Docker image (docker-publish.yml:824–832) and npm (npm-publish.yml:266–292) | Configured: syft SPDX SBOM per archive (.goreleaser.yml sboms:) + dependency-free CycloneDX fallback (cmd/release-sbom, scripts/release-sbom.sh); snapshot dist/ contains 6 .sbom.json files (21–26 KB each). No published release, so live SBOM is UNVERIFIED | C | Partly RELEASE-BLOCKED — config + snapshot SBOMs exist; live SBOM unverified until a real release. |
| PARITY.md:1012 | Signing / provenance | PARTIAL | No cosign/Sigstore signing found in searched workflows (SBOM + npm provenance only) | Designed ahead, unproven live: cosign keyless over checksums.txt + each SBOM (.goreleaser.yml signs:), docker_signs, SLSA provenance via actions/attest-build-provenance, scripts/verify-release.sh + verify-image.sh; local tamper proof 4/4 with throwaway keypair. Keyless path UNVE… | C | Partly RELEASE-BLOCKED — cosign keyless path unverified (Sigstore POSTs hang from this VM); local tamper proof 4/4. |
| PARITY.md:1013 | Multi-arch build | PARTIAL | Published amd64+arm64 manifest; docker-publish.yml builds both | Configured: GoReleaser linux/amd64 + linux/arm64, setup-qemu-action v3.7.0 + setup-buildx-action v3.12.0 in release.yml; snapshot dist/ has all 6 platform binaries (3 OS × 2 arch). Real multi-arch image build UNVERIFIED — proof spec only (docs/ci-proof-multarch.md, gaps G3–G5+G9… | C | Partly RELEASE-BLOCKED — snapshot has all 6 binaries; real multi-arch image build unverified (docs/ci-proof-multarch.md gaps G3–G5+G9 open). |
| PARITY.md:1014 | Install script | PARTIAL | n/a (npm/Docker are the installers; no standalone shell installer found) | scripts/install.sh (10.5 KB): OS/arch detection, refuses pipe-to-shell, cosign-verify of checksums.txt when cosign present, SHA-256 check, CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 strict mode — complete script, but downloads from GitHub releases that do not exist yet | C | RELEASE-BLOCKED — complete script, but downloads from GitHub releases that do not exist yet. |
| PARITY.md:1015 | Extra install surfaces | MISSING | AUR (yay -S omniroute-bin + systemd user service), Nix flake, Podman/Quadlet, Fly.io (fly.toml), Helm chart +… | None of these exist (no deploy/, fly.toml, flake.nix, PWA manifest, AUR/Nix packaging) | C | DECISION — AUR / Nix / Helm / PWA / Termux / VSCode ext: pick which, if any, M11 covers. |

### Area 14a — /v1 public API

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:1067 | POST /v1/responses + GET/DELETE/list | PARTIAL | responses/route.ts, responses/[...path], responses/input_tokens | POST + GET {id} + GET list + DELETE {id} + GET {id}/input_items (M8.4) + GET /v1/responses/input_tokens (M8.3); no [...path] catch-all sub-actions | A |  |
| PARITY.md:1068 | GET /v1/me/status | PARTIAL | api/v1/me/status/route.ts | GET /v1/me/status — daily/weekly usage windows live (M5.6) alongside budget/traffic + account quotas; per-window total REQUEST counts now live (M8.11 — the gap this row cited is closed, Area 11 row 724 → FULL). Verdict held at PARTIAL: no reviewer assessment blessed FULL for thi… | A | Verdict held at PARTIAL pending a reviewer call (gap cited is closed; needs reviewer blessing for FULL). |
| PARITY.md:1069 | POST /v1/completions (legacy) | PARTIAL | HAVE | POST /v1/completions live (M8.2, wired merge wave): non-streaming prompt-string (or 1-element []string) → one user message, text_completion shape. Gaps: messages[] body → 400, multi-prompt arrays → 400, stream:true → 400, exactly 1 choice, no system_fingerprint | A |  |
| PARITY.md:1070 | POST /v1/multimodal-embeddings | MISSING | HAVE | — | A |  |
| PARITY.md:1071 | GET/POST /v1/models/query, /health, /[...model] | PARTIAL | HAVE (3 extra files) | GET /v1/models/query, /health live + GET /v1/models/{model...} live (M8.10, wired merge wave; {model...} wildcard serves virtual models incl. auto/cheap). Gaps: ?capability= answers 400 (catalog records no per-model capability flags); ?category=/?max_price=/?min_context=/?shortl… | A |  |
| PARITY.md:1072 | POST /v1/moderations | PARTIAL | HAVE | POST /v1/moderations live (M8.9, wired merge wave, scope chat): OpenAI shape, local guardrails only (PII/secret/injection); the 13 OpenAI categories are all false, score 0 — hate/violence/sexual/self-harm/illicit content is not detected. Notes: category_applied_input_types fixed… | A |  |
| PARITY.md:1073 | POST /v1/search (+ /search/analytics) | PARTIAL | HAVE | POST /v1/search live (M8.20, wired merge wave): Perplexity passthrough, verbatim, replay-exempt. /search/analytics not built | A |  |
| PARITY.md:1075 | POST /v1/systemone | MISSING | HAVE | — | A |  |
| PARITY.md:1076 | POST /v1/classify | MISSING | HAVE — POST Jina zero/few-shot classification proxy (classify/route.ts, body {model?, classifier_id?, input, … | GET /v1/classify live (M8.2, wired merge wave): local auto-router tiering via ResolveWithThreshold — same path, different method, body, backend, response; NOT OmniRoute's Jina proxy | A | MISSING as OmniRoute defines it (Jina proxy); local variant exists at same path — DECISION: build the proxy or record divergence. |
| PARITY.md:1077 | GET /v1/explain/routing | PARTIAL | HAVE — GET routing_explain object: recent routing EVENTS ring buffer + quality snapshot + otel stats (explain… | GET /v1/explain/routing live (M8.2, wired merge wave): per-prompt classification + configured strategy + last-served decision/trace; only the last-served/events concept overlaps OmniRoute's events ring | A |  |
| PARITY.md:1078 | POST /v1/segment | MISSING | HAVE — POST Jina segmenter proxy (segment/route.ts, body {content, tokenizer?, return_tokens?, return_chunks?… | GET /v1/segment live (M8.2, wired merge wave): local sentence-split + per-segment classification — entirely different semantics from OmniRoute's Jina proxy | A | Same as classify: local sentence-split exists; Jina proxy semantics missing — DECISION. |
| PARITY.md:1079 | Images: /v1/images/generations, /edits, /upscale | PARTIAL | HAVE (3 files) | POST /v1/images/generations + /edits live (M8.5, wired merge wave): engine path via singleAttempt, verbatim upstream errors, 429 Retry-After forwarded. /upscale NOT built: it is not an OpenAI endpoint (exists only in third-party OpenAI-compatible servers — LocalAI/Lemonade/Osaur… | A | /upscale is not an OpenAI endpoint (third-party only) — coordinator decision whether parity demands it. |
| PARITY.md:1080 | Audio: speech, transcriptions, translations, speech-to-text, text-to-speech/[voiceId], sound-generation, voic… | MISSING | HAVE (7 files) | — | A | IN-TREE-NOW — see §0.1 (audio_client.go wired into audio_passthrough.go:290,307, uncommitted); verify end-to-end + flip.|
| PARITY.md:1081 | Video: /v1/videos/generations, /v1/video-bridge/drilldown | PARTIAL | HAVE (2 files) | POST /v1/videos/generations + GET /v1/videos/{id} live (M8.20, wired merge wave): Runway passthrough, task polling by re-GET. /v1/video-bridge/drilldown not built | A |  |
| PARITY.md:1082 | Music: /v1/music, /generations, /plan, /detailed, /stream, /detailed/stream | PARTIAL | HAVE (6 files) | POST /v1/music/generations live (M8.20, wired merge wave): MiniMax passthrough, synchronous, 200-envelope verbatim. Only /generations of OmniRoute's 6 music routes; MiniMax closed to new users since 2026-08-20 (entitled keys only) | A |  |
| PARITY.md:1083 | Files: /v1/files, [id], [id]/content | PARTIAL | HAVE (3 files) | POST/GET /v1/files, GET/DELETE /v1/files/{id}, GET /v1/files/{id}/content live (M8.7, wired merge wave): OpenAI file objects, atomic store, traversal-safe. Gaps: no pagination, no expiry, no per-purpose file-type validation, status always "processed"; no 512 MiB upload cap yet (… | A |  |
| PARITY.md:1084 | Batches: /v1/batches, [id], [id]/cancel, delete-completed | PARTIAL | HAVE (4 files) | 6 routes live (POST /v1/batches, GET list/get, POST [id]/cancel, DELETE [id], DELETE delete-completed); real worker executes JSONL lines through the chat engine; cancel stops work; persisted batches.json; 9 fixtures, 13 tests, review PASS | A | FULL withheld on two points: ledger at sync rate (no 50% batch rate) + no input-file ownership (links to Area 11 batch-pricing row). |
| PARITY.md:1085 | Per-provider: /v1/providers/[provider]/chat/completions, /embeddings, /images/generations, /models, /limits, … | PARTIAL | HAVE (7 files) | POST /v1/providers/{provider}/chat/completions + GET /v1/providers/{provider}/models live (M8.12, wired merge wave; pin = routing.only + delegate to handleChatCompletions, real-engine failover within the pinned provider's keys) + GET /v1/providers/{provider}/status extra (not in… | A |  |
| PARITY.md:1086 | WebSocket /v1/ws | MISSING | HAVE (GET+OPTIONS, api/v1/ws/route.ts:144) | — | A |  |
| PARITY.md:1087 | Gemini-native /v1beta/models, /v1beta/models/[...path] | PARTIAL | HAVE (2 files) | GET /v1beta/models + POST /v1beta/models/{rest...} (:generateContent, :streamGenerateContent) live (M8.13, wired merge wave; pre-engine errors now Gemini-enveloped, fix re-reviewed PASS). Gaps: single-model GET /v1beta/models/{model} missing; no pagination (pageSize/pageToken, n… | A |  |
| PARITY.md:1088 | GET /v1/me/sessions, /me/sessions/[id] | PARTIAL | HAVE | GET /v1/me/sessions lists the caller's Responses sessions (root + all descendants), list-price cost + unpriced_count, newest first, cap 100 (M5.6); GET /v1/me/sessions/{id} detail live (M8.11, wired merge wave): root's turns, cumulative tokens + list-price cost, per-turn detail,… | A | Remaining: project/client attribution (links to Area 11 row 901). |
| PARITY.md:1089 | GET /v1/auto-combo/[channel]/candidates | PARTIAL | HAVE | Route registered (M3 Phase D, D1) with the live pool: tier models, breaker states, per-key connections, cooldowns; free/pro + category filters applied from the live price book + tier membership. Gaps: allowlist exclusions have no store (excluded always false); ModelLocked always… | A | DUP of Area 1 Candidate-pool endpoint row. |
| PARITY.md:1090 | Quotas/keys: /v1/quotas/check, /quota-recoveries/[id], /accounts/[id]/limits, /session-leases, /registered-ke… | PARTIAL | HAVE | POST /v1/quotas/check live (M8.11, wired merge wave): pre-flight verdict from the real ledger gates (rolling key budget + token limits), allowed + budget/token-limit/price numbers. Only /v1/quotas/check of the 5 listed; /quota-recoveries/[id], /accounts/[id]/limits, /session-lea… | A |  |
| PARITY.md:1091 | Relay: /v1/relay/chat/completions, …/bifrost | MISSING | HAVE | — (ours has only an internal relay resolver for /v1/messages) | A |  |
| PARITY.md:1092 | Vendor extras: /v1/fal/* (3), /v1/agents/* (5), /v1/antigravity, /v1/muse-code/models, /v1/api/chat | MISSING | HAVE | — | A | DECISION — third-party proxies (fal/agents/antigravity/muse-code); coordinator decision whether parity demands them. |
| PARITY.md:1093 | VSCode token-scoped surfaces /v1/vscode/** | MISSING | HAVE (~20 files) | — (deliberate scope: no VSCode extension) | A | DO-NOT-BUILD — deliberate scope: no VSCode extension. Needs DIVERGENCE verdict, not a slice. |
| PARITY.md:1095 | /v1/provider-plugin-manifest | MISSING | HAVE | — | A |  |
| PARITY.md:1096 | Mgmt proxies: /v1/management/proxies*, /proxy-subscriptions* | MISSING | HAVE (8 files) | — | A |  |
| PARITY.md:1097 | Web/media: /v1/web/fetch, /v1/web/map, /v1/media/[id], /v1/voices | MISSING | HAVE | — | A |  |

### Area 14b — Management / admin API

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:1106 | OpenAPI | PARTIAL | api/openapi/spec, api/openapi/try (2) | GET /openapi.json live from guard table + try-it page web/tryit.html (170 routes, tier/scope-correct) built but not yet served — no /tryit route; serving is follow-up (M10) | C | Remaining work is serving the try-it page (M10 follow-up); GET /openapi.json itself is live. |
| PARITY.md:1107 | Providers mgmt (61 files) | PARTIAL | FULL | POST+DELETE /api/providers, GET /api/providers, GET /api/catalog (add/delete/list/catalog only) | A |  |
| PARITY.md:1108 | Keys mgmt (14 files + 2 key-templates) | PARTIAL | FULL | Provider-key add/delete/role + gateway rotate/revoke-all; no per-key policy API, no import/reveal | A |  |
| PARITY.md:1110 | Combos (10 files) | PARTIAL | FULL | GET/POST/PATCH/DELETE /v1/combos live (M8.14, wired merge wave): named-chain CRUD over config groups + chain_strategies, one shared ComboStore, race-serialised via routingMu (M8.14 race fixes, soak-tested). Still no dashboard UI; duplicate/import/export/test/enable still missing… | A | Remaining: duplicate/import/export/test/enable (A); dashboard UI is C (§8b). |
| PARITY.md:1112 | Analytics (3 files + usage analytics) | PARTIAL | FULL | Dashboard overview/logs/usage feeds + GET /api/analytics/summary, /by-provider, /by-model, /costs live (M8.17, wired merge wave). Still no dashboard page (M10); no auto-routing/compression/diversity surface — the four G17 handlers for these routes (handleAnalyticsAutoRouting, handleAnalyticsCompression, handleAnalyticsDiversity, handleAnalyticsComboHealth) were built and review-verified (GAP-TRACK.md §10 REVIEW G17, 2026-11-19 ~17:10 IST) but REMOVED as dead code in the M10-15 cleanup: never registered (no s.register in server.go), never guarded (no guard.go row), never reachable over HTTP | A | Remaining: auto-routing/compression/diversity surface must be REBUILT from scratch (backend, A) — not a registration-pending step, the G17 handlers no longer exist in the tree; dashboard page is C (M10). |
| PARITY.md:1113 | Cache (4 files) | PARTIAL | FULL | GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only) (M6.8) + DELETE /api/cache live (M8.15, wired merge wave) with per-layer eviction counts; no /dashboard/cache page (M10); no invalidateByModel | A | Remaining: invalidateByModel (A); /dashboard/cache page is C (M10). |
| PARITY.md:1114 | Resilience (5 files) | PARTIAL | FULL | /dashboard/cooldown feed incl. breakers + GET /api/resilience live (M8.16, wired merge wave): live breaker/cooldown feed. No settings read/patch, no per-model cooldowns, no breaker reset; hedge counts honestly unknown | A |  |
| PARITY.md:1115 | Pricing (5 files incl. pricing/sync, PATCH /api/pricing) | PARTIAL | FULL | GET /dashboard/prices feed + GET /api/pricing and PATCH /api/pricing live (M8.15, wired merge wave; PATCH race-fixed, -race clean). Stays PARTIAL until pricing-sync lands | A |  |
| PARITY.md:1116 | MCP (7 files + stdio/SSE/Streamable-HTTP) | PARTIAL | FULL, 110 tools claimed (UNVERIFIED — static grep undercounts at 84; do not quote 84) | stdio server, 3 read-only tools + GET /dashboard/mcp + MCP client selector; no HTTP/SSE transport, no audit/restart API | B | STALE? — see §Stale flags #6 (24 tools, SSE + Streamable HTTP live, audit exists). |
| PARITY.md:1117 | Plugins (8 files) | PARTIAL | FULL (see §14c) | Feed + 501 load stub | C | Load half is DO-NOT-BUILD (ACE — compiled-in only); the row lives in C as M11 ecosystem surface. |
| PARITY.md:1118 | Auth mgmt (12 files: login/logout/MFA/OIDC/SSO/CSRF/status) | PARTIAL | FULL | Gateway keys + bootstrap only; no login/MFA/OIDC/SSO | A | DECISION — login/MFA/OIDC/SSO vs the gateway-key auth model. |
| PARITY.md:1119 | Settings (92 files) | PARTIAL | FULL | Config file + dashboard POSTs (strategies, cooldown, guardrails, key-budgets, feedback, intent-tiers, suggestions) + settings export | A | Breadth row; dashboard settings UI half is C (§8b). |
| PARITY.md:1120 | Tunnels (9 files: cloudflared/ngrok/tailscale) | PARTIAL | FULL | Local TCP tunnel only (start/stop/status) | A | DECISION — cloudflared/ngrok/tailscale integrations vs local TCP tunnel only. |
| PARITY.md:1121 | DB backup (4 files) | PARTIAL | FULL | POST /admin/db/export\|import, wipe arm+fire (no scheduled backups) | A |  |
| PARITY.md:1123 | Shutdown/restart | PARTIAL | api/shutdown, api/restart | POST /admin/shutdown real; no restart endpoint | A |  |
| PARITY.md:1124 | Logs | PARTIAL | api/logs/* (4) + usage/call-logs | /dashboard/logs feed + GET /api/logs live (M8.16, wired merge wave; capture wired in main.go, double-redacted). OmniRoute also has usage/call-logs, not exposed | A |  |
| PARITY.md:1125 | Compression mgmt (7 files) | PARTIAL | FULL | Internal only; summary in overview feed | A |  |
| PARITY.md:1127 | Version-manager (6 files) | MISSING | FULL | POST /admin/update is an honest 501 stub | C | Overlaps Area 13 Self-update mechanism row; RELEASE-BLOCKED in practice. |
| PARITY.md:1128 | Webhooks (5 files) | PARTIAL | FULL — GET/POST list/create, GET/PUT/DELETE per id | GET/POST /api/webhooks (list/create) + GET/DELETE /api/webhooks/{id} live (M8.19, wired merge wave); budget.warning wired live (threshold-crossing delivery proven, HMAC-signed, edge-triggered) + quota.exceeded; config validation now accepts both events (merge-wave fix). Remainde… | A | Remaining: PUT /api/webhooks/{id} (A); dashboard UI is C (M10). |
| PARITY.md:1129 | Translator (5 files) | MISSING | FULL — POST translate/detect verified | — | A | DECISION — translation API is a service, not router core. |
| PARITY.md:1130 | Discovery (4 files) | MISSING | FULL — scan, results, verify | — (catalog is static; no scan/verify API) | A |  |
| PARITY.md:1131 | A2A (5 files + Agent Card) | MISSING | FULL — JSON-RPC 2.0 + SSE, 6 skills re-verified fresh | — (zero A2A hits) | B | STALE? — see §Stale flags #7 (POST /a2a + agent card + skills live; REST task routes still missing). |
| PARITY.md:1132 | Teams / cost centers (4 files) | PARTIAL | FULL | GET/POST /api/teams + GET/PATCH/DELETE /api/teams/{id} live (M8.18, wired merge wave): full CRUD + live budget/member/consumption views + enforced gates. Field-for-field parity vs OmniRoute's 4 team files not verified | A | API live (M8.18); remaining is field-for-field parity verification; dashboard UI is C (M10). |
| PARITY.md:1133 | Quota-share pools/groups/plans (11 files) | PARTIAL | FULL | GET/POST /api/quota-pools + GET/PATCH/DELETE /api/quota-pools/{id} live (M8.18, wired merge wave): full CRUD + live views + enforced gates. Field-for-field parity vs OmniRoute's 11 pool files not verified; pools have no groups/plans concept at all | A | API live (M8.18); remaining is field-for-field parity verification + groups/plans concept (absent); dashboard UI is C. |
| PARITY.md:1134 | Embedded services (77 files) | MISSING | FULL | — (deliberate scope) | A | DO-NOT-BUILD — deliberate scope (77 files of embedded services are not router core). |
| PARITY.md:1135 | Gamification/skills/memory/conductor/chaos/evals | MISSING | FULL (dozens of files) | — (deliberate scope) | A | DO-NOT-BUILD — deliberate scope (not router core). |
| PARITY.md:1136 | Files/batches mgmt (4 files) | MISSING | FULL | /v1/files API live (M8.7, wired merge wave — see §14a Files row); stored Responses objects only before that; batches still missing (M8.8 not built) | A | STALE? — see §Stale flags #11 (batches built M8.8; evidence clause false). |
| PARITY.md:1137 | Sessions (1 file + v1 me/sessions) | PARTIAL | FULL | GET /v1/me/sessions list live (M5.6); GET /v1/me/sessions/{id} detail live (M8.11, wired merge wave); project/client attribution still missing | A |  |
| PARITY.md:1138 | Search mgmt (search/providers, search/stats) | MISSING | FULL | — | A |  |
| PARITY.md:1139 | Fallback chains API (fallback/chains) | MISSING | FULL | Config-only chains; no API | A | DUP of Area 2 fallback-chains row. |
| PARITY.md:1140 | OAuth mgmt (19 files) | MISSING | FULL | POST /admin/provider/login is an honest 501 stub | A | DECISION — login/MFA/OIDC/SSO vs the gateway-key auth model. |
| PARITY.md:1141 | Service install | MISSING | (embedded-services install routes) | POST /admin/service/install honest 501 | C |  |

### Area 14c — Plugins

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:1156 | Install / upgrade | MISSING | POST /api/plugins (local path), marketplace install; pluginManager.install() enforces strictly-newer semver, … | No install path. POST /admin/plugin/load = honest 501 by design (compiled-in only, no .so/Wasm/JS — loading arbitrary code from config would be ACE) | C | DO-NOT-BUILD — compiled-in only; loading arbitrary code from config would be ACE (501 by design). |
| PARITY.md:1157 | Activate / deactivate | MISSING | POST /api/plugins/[name]/activate, /deactivate; lifecycle hooks onActivate/onDeactivate/onUninstall (manager.… | None — enablement is config plugins.enabled list order at startup only | C |  |
| PARITY.md:1158 | Per-plugin config | PARTIAL | GET/PUT /api/plugins/[name]/config | Config-file only (plugins.cost_cap_usd_per_request, header-add header/value); no API | C |  |
| PARITY.md:1159 | Marketplace / scan | MISSING | GET /api/plugins/marketplace, /marketplace/install, /scan | None | C |  |
| PARITY.md:1160 | Hooks | PARTIAL | 4 pipeline hooks (hooks.ts:37-44,252-260): onRequest (blocking, can modify/block with 403), onResponse (chain… | 3 hooks: HookPreRequest (fail-closed 403/-32001 plugin_denied), HookPostResponse (fail-open), HookProviderAdapter (per-provider wire rewrite); in-process only, panic-isolated per plugin | C |  |
| PARITY.md:1161 | Built-ins shipped | PARTIAL | None documented (system is install-your-own + SDK) | 3 named built-ins: header-add, response-log, cost-cap (builtins.go:20-23, costcap.go:62; Builtins() map, config.go:1111-1112 validation) | C |  |
| PARITY.md:1162 | MCP plugin tools | MISSING | 8 plugin-management tools over MCP (tools/pluginTools.ts) | None | C | DO-NOT-BUILD — plugin management over MCP is never built (PLAN M9). |
| PARITY.md:1163 | Dashboard page | PARTIAL | /dashboard/plugins (marketplace + installs) | GET /dashboard/plugins (live registry snapshot, honest zero state; no install UI) | C |  |

### Area 14d — Docs / README

| Line | Feature | Verdict | OmniRoute ref | Our status | Bucket | Note |
|---|---|---|---|---|---|---|
| PARITY.md:1169 | "README rewritten to 593 lines in README.v2.md, in OmniRoute's structure" | WRONG | WRONG on count: README.v2.md is 260 lines, README.md is 179 lines (measured 2026-11-19). Structure claim part… |  | C |  |
| PARITY.md:1173 | Catalog "614 providers" in README | PARTIAL | 394 ref + 29 + 1 + 191 triage rounds 1–5; 570 + 44 round-5 = 614, arithmetic consistent. Not re-counted in th… |  | C |  |
| PARITY.md:1174 | Docs breadth | MISSING | OmniRoute: docs/ 10,416 .md files (10,206 i18n + 210 English), llm.txt 503 lines, docs/openapi.yaml 19,558 li… |  | C | STALE? — see §Stale flags #12 (docs/llm.txt + docs/API_REFERENCE.md now exist, M8.21; breadth gap remains). |
| PARITY.md:1175 | README structure mirroring | PARTIAL | v2's section order tracks OmniRoute's; content depth does not |  | C |  |

### Area 8b — Dashboard: Routing / Keys / Budgets / Analytics / Logs / Settings (+ Benchmark)

PARITY.md §8b is condensed (26 key rows). The rows below are the full expansion from the
comparison report §Area 8b (report lines 952–1159). **Caveat: these verdicts are the
2026-11-18 ~08:36 baseline and were NOT re-verified against M3–M9 backend work.** They are
dashboard-UI rows, so backend flips do not close them — but a few carry stale backend
claims (e.g. "code strategy rejected", "candidate-pool endpoint: None") and are flagged.
All 133 are bucket C (M10). PARITY.md key-row cross-check: the "22 routing strategies" key
row (MISSING 18 / WRONG 1 / PARTIAL 3) = the 22 strategy rows below; "Named combos CRUD",
"Endpoint behavior", "Default Auto Variant", "Session stickiness", "Create API key",
"Single-key disable/revoke", "Daily/Weekly/Monthly $ limit", "Quota-share pools",
"Team cost centers", "Cost Overview KPIs", "Monthly forecast", "Cost Explorer",
"Combo Health", "Proxy Logs / Console / Timeline / Activity / Conversations / Compression
log / Audit log", "Automatic failover / Prompt-cache pinning / Cost-aware routing / Health
checks / Request logging switches", "Listen address + Port" are the same features as rows
below (grouped under Routing-combo-builder, Keys, Budgets, Analytics, Logs, Settings).
Four PARITY key rows are NOT dashboard gaps and are excluded from this expansion:
Regenerate key (FULL), Provider keys (FULL), Scope enforcement (FULL), Rolling 5h budget
(FULL), Request logs table (FULL), Routing strategy select (FULL), Authz / route inventory
(FULL), Require login (DIVERGENCE), Benchmark page (EXTRA).

| Line | Feature | Verdict | OmniRoute ref | Our status (baseline, 2026-11-18) | Bucket | Note |
|---|---|---|---|---|---|---|
| report:956 [Routing — 22 strategies (ver] | `priority` (their default) | MISSING | `routingStrategies.ts:2`, `comboConfig.ts` default | No declared-order strategy; chain order destroyed by quota-hint sort unless contract `order` given | C |  |
| report:957 [Routing — 22 strategies (ver] | `weighted` | WRONG | Random draw ∝ per-target operator weight + sticky pin (`targetSorters.ts:28`) | `strategy.go:180` re-weights 5 scoring factors deterministically; no per-target weight, draw, or pin | C |  |
| report:958 [Routing — 22 strategies (ver] | `round-robin` | MISSING | Own loop `combo/roundRobinCombo.ts` + `rrState.ts` | Nothing | C |  |
| report:959 [Routing — 22 strategies (ver] | `context-relay` | MISSING | `strategyRegistry.ts:143`, handoff after success | Only reasoning-state handoff — different thing | C |  |
| report:960 [Routing — 22 strategies (ver] | `fill-first` | MISSING | Declared order kept until target fails/exhausts | No named strategy | C |  |
| report:961 [Routing — 22 strategies (ver] | `p2c` | MISSING | `targetSorters.ts:136` | Nothing | C |  |
| report:962 [Routing — 22 strategies (ver] | `random` | MISSING | Fisher-Yates shuffle | Nothing | C |  |
| report:963 [Routing — 22 strategies (ver] | `least-used` | MISSING | `targetSorters.ts:107` | Attempt counts exist, nothing orders by usage | C |  |
| report:964 [Routing — 22 strategies (ver] | `cost-optimized` | PARTIAL | Pure cheapest-first, unknown price = ∞ last (`targetSorters.ts:91`) | `cheapest` = cost 0.55 + other factors vote; unknown price neutral 0.5, never last | C |  |
| report:965 [Routing — 22 strategies (ver] | `reset-aware` | MISSING | `quotaStrategies.ts:552`, weights 0.35 session / 0.65 weekly | Scalar quota-hint int only; no reset data | C |  |
| report:966 [Routing — 22 strategies (ver] | `reset-window` | MISSING | `quotaStrategies.ts:596` | No reset-window data | C |  |
| report:967 [Routing — 22 strategies (ver] | `expiry-first` | MISSING | `quotaStrategies.ts:727` | No quota-expiry data | C |  |
| report:968 [Routing — 22 strategies (ver] | `headroom` | MISSING | `headroomRanking.ts:54` (`1 − max(util5h, util7d)`) | Quota hint is a single int, not utilisation | C |  |
| report:969 [Routing — 22 strategies (ver] | `quota-weighted` | MISSING | Draw weight `score/(1+load)` (`quotaStrategies.ts:971`) | No weighted draw; quota only 0.10 in score | C |  |
| report:970 [Routing — 22 strategies (ver] | `strict-random` | MISSING | Shuffle deck, mutex-protected | No deck/shuffle | C |  |
| report:971 [Routing — 22 strategies (ver] | `auto` (16-factor) | PARTIAL | `scoring.ts:62`, 16 factors, pool-relative | `auto/*` resolves a *model* via embedding tiers; no pool scoring, mode packs, or taskFit | C |  |
| report:972 [Routing — 22 strategies (ver] | `lkgp` | MISSING | `orderByLkgp`, persisted LKGP | `lastRouting` recorded for dashboard only, never used to reorder | C |  |
| report:973 [Routing — 22 strategies (ver] | `context-optimized` | PARTIAL | Largest context window first (`comboStructure.ts:1050`) | `contextfit.go:175` only *filters out* too-small models | C |  |
| report:974 [Routing — 22 strategies (ver] | `cache-optimized` | MISSING | `orderByCacheAffinity`, affinity owns ordering | Prompt-cache passthrough only; no affinity ordering | C |  |
| report:975 [Routing — 22 strategies (ver] | `fusion` | MISSING | `fusion.ts:269`, parallel panel + judge synthesis | No fan-out, no judge | C |  |
| report:976 [Routing — 22 strategies (ver] | `pipeline` | MISSING | `pipelineRouter.ts:219`, multi-stage | No multi-stage pipeline | C |  |
| report:977 [Routing — 22 strategies (ver] | `jev` | MISSING | `jevStrategy.ts:139`, external judge, floor 0.15 | No judge, no admitted/held sets | C |  |
| report:985 [Routing — combo builder & gl] | Named combos CRUD (create/duplicate/import/export/test/enable) | MISSING | `combos/page.tsx`, "Create Combo", Import/Export, Duplicate | No combos at all — chains are provider-model lists | C |  |
| report:986 [Routing — combo builder & gl] | Endpoint behavior per-capability tabs | MISSING | Combos builder: chat/search/embeddings/rerank/images/video/music/OCR/audio, empty capabilities not … | None | C |  |
| report:987 [Routing — combo builder & gl] | Default Auto Variant (10: rules/score/cost/eco/latency/fast/sla-aware/sla/lkgp/nadir) | PARTIAL | `AUTO_ROUTING_STRATEGY_VALUES`, `RoutingTab.tsx:942` LKGP variant | 6 virtual models (`auto, auto/fast, auto/cheap, auto/coding, auto/code, auto/reasoning` + `intent/*`); no router-strategy picker, no sla/nadir/lkgp | C |  |
| report:988 [Routing — combo builder & gl] | Zero-config auto-routing enable toggle | PARTIAL | Settings → Routing: "Enable automatic provider selection using the auto/ prefix" | `auto/*` always on via virtual models; no toggle | C |  |
| report:989 [Routing — combo builder & gl] | Sticky Limit (calls/account before RR switch) | MISSING | ComboDefaultsTab ("Round Robin plus Sticky Limit") | None | C |  |
| report:990 [Routing — combo builder & gl] | Concurrency/Model, Queue Timeout, Queue Depth | PARTIAL | ComboDefaultsTab + combo builder advanced settings | Deadline budget only (clean 504, shown live); no queue timeout/depth UI; concurrency lives on a separate page as adaptive limiter, not combo queue | C |  |
| report:991 [Routing — combo builder & gl] | Track Metrics toggle | PARTIAL | ComboDefaultsTab | Metrics always on; no toggle | C |  |
| report:992 [Routing — combo builder & gl] | Max Retries / Retry Delay (ms) | PARTIAL | Combo builder + ComboDefaultsTab | Backend has MaxAttempts 5 + backoff; no UI control | C |  |
| report:993 [Routing — combo builder & gl] | Max Nesting Depth | MISSING | ComboDefaultsTab | None in UI (backend `maxComboDepth` is theirs; ours unverified) | C |  |
| report:994 [Routing — combo builder & gl] | Provider Overrides (per-provider max retries) | MISSING | ComboDefaultsTab | None | C |  |
| report:995 [Routing — combo builder & gl] | Failover Before Retry toggle | PARTIAL | Combo builder advanced settings | Failover-first is default behaviour; no toggle | C |  |
| report:996 [Routing — combo builder & gl] | Max Set Retries / Set Retry Delay | MISSING | Combo builder advanced settings | None | C |  |
| report:997 [Routing — combo builder & gl] | Combo Quota Only Fallback toggle | MISSING | Combo builder | None | C |  |
| report:998 [Routing — combo builder & gl] | Reasoning Token Buffer toggle | MISSING | Combo builder | None | C |  |
| report:999 [Routing — combo builder & gl] | Response Validation editor | MISSING | Combo builder | None | C |  |
| report:1000 [Routing — combo builder & gl] | Session stickiness toggle | WRONG | Combo builder ("Disable session stickiness") | Settings "Prompt-cache pinning" switch exists but is **display-only with fake "· saved" toast** (see Settings) | C |  |
| report:1001 [Routing — combo builder & gl] | Auto-combo catalog variants (subscription fail-closed, thrifty, offline, smart, lkgp, chaos, `auto/… | PARTIAL | `AUTO-COMBO.md`, AutoComboCatalog | `auto/coding, auto/fast, auto/cheap` match; `auto/code, auto/reasoning, intent/*` are ours; offline/lkgp/chaos/subscription/thrifty/chat/cat:tier missing | C |  |
| report:1002 [Routing — combo builder & gl] | LKGP toggle + Clear LKGP Cache | MISSING | Settings → Routing | None | C |  |
| report:1003 [Routing — combo builder & gl] | System-block Transform Pipeline (ops, 50 max, JSON import/export) | MISSING | Settings → Routing, idempotent ops | None | C |  |
| report:1004 [Routing — combo builder & gl] | CLI Fingerprint Matching per provider | MISSING | Settings → Routing | None | C |  |
| report:1005 [Routing — combo builder & gl] | Client Cache Control (Always/Never/Auto preserve) | MISSING | Settings → Routing | None | C |  |
| report:1006 [Routing — combo builder & gl] | Echo requested model name | MISSING | Settings → Routing | None | C |  |
| report:1007 [Routing — combo builder & gl] | Web search routing model picker | MISSING | Settings → Routing | None | C |  |
| report:1008 [Routing — combo builder & gl] | Adaptive Volume Routing toggle | MISSING | Settings → Routing | None | C |  |
| report:1009 [Routing — combo builder & gl] | Antigravity Signature Cache Mode | MISSING | Settings → Routing | None (narrow/provider-specific) | C |  |
| report:1010 [Routing — combo builder & gl] | 16-factor scoring + 6 mode packs | MISSING | `scoring.ts` DEFAULT_WEIGHTS, `modePacks.ts` | 5 factors/mode, no taskFit/stability/health; no packs | C |  |
| report:1011 [Routing — combo builder & gl] | Scoring inspector (factor-level, read-only recompute) | PARTIAL | Combo Health: "Intelligent scoring inspector" | Auto-router decision card shows tier/confidence/per-tier scores — same idea, `auto/*` only | C |  |
| report:1012 [Routing — combo builder & gl] | Account fallback strategies (9 values) | MISSING | `ACCOUNT_FALLBACK_STRATEGY_VALUES` | Single global strategy only | C |  |
| report:1013 [Routing — combo builder & gl] | Hedging control (delay, on/off) | PARTIAL | Combo config `hedging:false`, `hedgeDelayMs:500` | Hedging ON, 150 ms, no UI control, no body-size guard | C | STALE? — 256 KiB body guard exists (internal/router/hedge.go); UI control is the gap. |
| report:1014 [Routing — combo builder & gl] | maxGlobalAttempts clamp [1,200] | PARTIAL | `comboPredicates.ts:132-137` | MaxAttempts default 5, no clamp, no UI | C |  |
| report:1015 [Routing — combo builder & gl] | Candidate-pool endpoint | MISSING | `GET /v1/auto-combo/{channel}/candidates` | None (data exists internally, feeds `/dashboard/auto-route` only) | C | STALE? — route live since M3 (D1); dashboard surfacing is the gap. DUP of Area 1 / §14a rows. |
| report:1016 [Routing — combo builder & gl] | Combo Studio live cascade | MISSING | `/dashboard/combos/live` | Strategy-trees card is read-only config display, not a live cascade | C |  |
| report:1024 [API Keys] | Create API key (dashboard) | PARTIAL | `api-manager/page.tsx` + `ApiManagerPageClient.tsx`, "Create API Key" | "+ New router key" button exists but **not wired** — honest toast "isn't wired yet — use bootstrap or edit the config file" (`index.html:5888`) | C |  |
| report:1025 [API Keys] | Per-key model restriction ("All models") | PARTIAL | Per-key card | Keys feed shows scopes/mask; no per-key model restriction UI | C |  |
| report:1026 [API Keys] | Exclusive Lease badge | MISSING | `[id]/access/tabs/GeneralTab.tsx:75` | None | C |  |
| report:1027 [API Keys] | JSON-stream default badge | MISSING | Per-key card | None | C |  |
| report:1028 [API Keys] | Device tracking (distinct IP/UA, last 30 min) | MISSING | Per-key card "{count} device(s)" | None | C |  |
| report:1029 [API Keys] | Requests + Last used on card | PARTIAL | Per-key card "reqs", "Last: {date}" / "Never used" | Usage/logs carry this, not the key card | C |  |
| report:1030 [API Keys] | Access schedule | MISSING | Per-key card "Schedule" | None | C |  |
| report:1031 [API Keys] | Show/Hide key | PARTIAL | "Show Key"/"Hide Key" | Raw shown once only at rotation; no per-key show/hide | C |  |
| report:1033 [API Keys] | Per-key delete | PARTIAL | "Delete this API key?" | Not wired — honest toast "Revoking keys from the dashboard isn't wired yet" | C |  |
| report:1034 [API Keys] | Save as template | MISSING | Per-key action | None | C |  |
| report:1035 [API Keys] | Edit permissions (key manage) | PARTIAL | Per-key access + routing sub-pages | Manage form (expiry, RPM, TPM, guardrails allowlist) with real save handler (:5783); no model/endpoint/connection allow-lists | C |  |
| report:1036 [API Keys] | Per-key routing page | MISSING | `/dashboard/api-manager/routing` | None | C |  |
| report:1037 [API Keys] | Per-key access (allowed models/connections/endpoints/combos) | MISSING | `/dashboard/api-manager/[id]/access` | None | C |  |
| report:1038 [API Keys] | Key import | MISSING | `POST /api/keys/import` | None | C |  |
| report:1039 [API Keys] | Key pools / fair-share quotas | MISSING | Per-key policy fields | None | C |  |
| report:1040 [API Keys] | IP allowlist/blocklist per key | MISSING | Per-key policy fields | None | C |  |
| report:1041 [API Keys] | **Scope enforcement** | MISSING | Enforced per route (`keyScopedRoute.ts`) | **P0: `HasScope` has zero non-test call sites — scopes stored/displayed only** | C |  |
| report:1042 [API Keys] | Single-key disable/revoke | MISSING | `is_active` / `revoked_at` per key | Only revoke-all exists | C | STALE? — backend per-key disable/revoke exists (M1.3); dashboard UI is the gap. |
| report:1043 [API Keys] | Management Access Tokens (read/write/admin, expiry, revoke, shown once) | MISSING | `/dashboard/settings/access-tokens` | None — gateway keys only, no CLI/management tokens | C |  |
| report:1052 [Budgets (Costs → Budget / Qu] | Daily $ limit per key | MISSING | `en.json:10816` "Daily Limit (USD)", Costs → Budget page | Only rolling 5h exists | C |  |
| report:1053 [Budgets (Costs → Budget / Qu] | Weekly $ limit per key | MISSING | Costs → Budget page | None | C |  |
| report:1054 [Budgets (Costs → Budget / Qu] | Monthly $ limit per key | MISSING | Costs → Budget page | None (only cumulative legacy cap, backend-only) | C |  |
| report:1055 [Budgets (Costs → Budget / Qu] | Warn at % | MISSING | Budget page "Warn at %" | None per key (80% warn on legacy cap only, no UI) | C |  |
| report:1056 [Budgets (Costs → Budget / Qu] | Reset Interval / Reset Time (UTC) / Next reset | MISSING | Budget page | None — rolling window, no calendar reset | C |  |
| report:1057 [Budgets (Costs → Budget / Qu] | Budget templates ($/month, $/day, apply to selected) | MISSING | Budget page "Templates" | None | C |  |
| report:1058 [Budgets (Costs → Budget / Qu] | Projection (linear, Proj EOM, "above {limit}/month") | MISSING | Budget page "Projection" | None | C |  |
| report:1059 [Budgets (Costs → Budget / Qu] | 30-day cost breakdown by provider (per key) | PARTIAL | Budget page "Cost breakdown (30 days)" | Spend-by-provider table exists in Usage (not per-key, no projection) | C |  |
| report:1060 [Budgets (Costs → Budget / Qu] | Budget table (Key/Today/Month/Daily lim/Monthly lim/Used%/Status) + sorts | MISSING | Budget page | Ours has a different table: 5h Key/Limit/Spent/Remaining/Window/Unknown | C | STALE? — backend daily/weekly/monthly budgets exist (M5.1); dashboard UI is the gap. |
| report:1062 [Budgets (Costs → Budget / Qu] | Per-key token limits (model/provider/global, most-restrictive-wins, 429) | PARTIAL | `quota/enforce.ts:142-186` | RPM + TPM only, prompt-only, in-memory, sliding-60s, no scoping | C |  |
| report:1063 [Budgets (Costs → Budget / Qu] | Quota-share pools (% allocations, hard/soft/burst, saturation 0.5) | MISSING | `/dashboard/costs/quota-share`, `fairShare.ts` | None | C | STALE? — backend fair-share pools exist (M5.4); dashboard UI is the gap. |
| report:1064 [Budgets (Costs → Budget / Qu] | Team cost centers (1d/7d/30d, soft budgets) | MISSING | `/dashboard/costs/teams` | None (deliberate-scope question, not a bug) | C | STALE? — backend teams exist (M5.12); dashboard UI is the gap. |
| report:1065 [Budgets (Costs → Budget / Qu] | Free-Tier Budget page | MISSING | `/dashboard/free-tiers` (route confirmed; detail sampled) | None | C |  |
| report:1066 [Budgets (Costs → Budget / Qu] | Free Provider Rankings (by model ELO) | MISSING | `/dashboard/free-provider-rankings` (route confirmed; detail sampled) | None | C |  |
| report:1068 [Budgets (Costs → Budget / Qu] | Provider Quota page | PARTIAL | `/dashboard/quota` (route confirmed) | Per-provider history/uptime + quota hints; no quota-limit UI | C |  |
| report:1076 [Analytics / Usage] | Cost Overview KPIs (Spend 1D/7D/30D, requests, active providers/models, avg cost/req) | PARTIAL | `/dashboard/costs` CostOverviewTab | Spend via 24h/7d/30d range seg + tokens + failovers; no side-by-side 1D/7D/30D, no avg cost/req | C |  |
| report:1077 [Analytics / Usage] | Token usage (total/in/out, in/out ratio) | PARTIAL | Costs Overview "Token Usage" | Total tokens routed; in/out split only per-request in Logs, no aggregate ratio | C |  |
| report:1078 [Analytics / Usage] | Routing efficiency (fallback reqs/rate, model coverage) | PARTIAL | Costs Overview | Failovers count only; no fallback rate %, no model coverage | C |  |
| report:1079 [Analytics / Usage] | Monthly forecast | MISSING | Costs Overview "Monthly Forecast" | None | C |  |
| report:1080 [Analytics / Usage] | Cost Trend / Provider Share / Top Providers / Top Models | PARTIAL | Costs Overview charts | Spend bars by provider + spend-by-model table; no trend chart | C |  |
| report:1081 [Analytics / Usage] | Cost by API Key | MISSING | Costs Overview table | None (per-key spend only in 5h budget table) | C |  |
| report:1082 [Analytics / Usage] | Cost by Account | MISSING | Costs Overview | None | C |  |
| report:1083 [Analytics / Usage] | Weekly pattern / 365-day heatmap / Period comparison | MISSING | Costs Overview | None | C |  |
| report:1084 [Analytics / Usage] | Cost Explorer (dimension filter, top 50, CSV/JSON export) | MISSING | Costs Overview | None (CSV export of logs only) | C |  |
| report:1085 [Analytics / Usage] | Pricing rate table (Input/Output/Cached/Reasoning/Cache Write $/1M, sync source/row) | PARTIAL | `/dashboard/costs/pricing`, `PricingTab.tsx:533-592` | `GET /dashboard/prices` feed exists; pricing-as-table presentation **not re-verified this pass** (prior report: 163-word prose paragraph at :1009) | C |  |
| report:1086 [Analytics / Usage] | Pricing sync (models.dev/LiteLLM, Sync Now, last/next sync, clear synced) | MISSING | Pricing page "Pricing Sync" | models.dev sync opt-in, backend-only; no UI | C |  |
| report:1087 [Analytics / Usage] | Combo Health (quota risk, forecast, Gini skew, autopilot, scoring inspector) | MISSING | `analytics/ComboHealthTab.tsx` (verified present) | None | C |  |
| report:1088 [Analytics / Usage] | Utilization (Global/Account split, quota snapshots, remaining capacity) | MISSING | `analytics/ProviderUtilizationTab.tsx` | Per-provider quota hints only | C |  |
| report:1089 [Analytics / Usage] | Cache analytics (hit rate, tokens saved, provider breakdown, 24h trend) | PARTIAL | `analytics/CacheHealthTab.tsx` | Overview cache hits/misses + prompt-cache summary; no dedicated page, no tokens-saved | C |  |
| report:1090 [Analytics / Usage] | Compression analytics | MISSING | `analytics/CompressionAnalyticsTab.tsx` | One summary line on Overview only | C |  |
| report:1091 [Analytics / Usage] | Search analytics | MISSING | `analytics/SearchAnalyticsTab.tsx` | No search | C |  |
| report:1092 [Analytics / Usage] | Evals | MISSING | `analytics/page.tsx` Evals tab (verified) | None | C |  |
| report:1093 [Analytics / Usage] | Provider Stats (latency/performance) | PARTIAL | `/dashboard/provider-stats` (route confirmed) | Per-provider p50 + uptime history; no stats page, no p99 | C |  |
| report:1094 [Analytics / Usage] | Telemetry p50/p95/p99 | PARTIAL | `GET /api/telemetry/summary` | p50/p95 only | C |  |
| report:1104 [Logs] | Time range filter | PARTIAL | Logs page "Time Range (Last {hours})" | Only All/ok/failed seg; no time-range picker | C |  |
| report:1105 [Logs] | Export (CSV/JSON) | PARTIAL | Logs page Export + `logExport` utils | CSV export button exists; no JSON export | C |  |
| report:1106 [Logs] | Proxy Logs (separate tab) | MISSING | `logs/proxy/page.tsx` (verified) | None | C |  |
| report:1107 [Logs] | Console output | MISSING | `logs/console/page.tsx` (verified) | None | C |  |
| report:1108 [Logs] | Timeline (visual request timeline) | PARTIAL | `logs/timeline/page.tsx` (verified) | Latest-route-trace card only (one request, attempt-by-attempt) — cousin, not a timeline | C |  |
| report:1109 [Logs] | Activity feed | MISSING | `/dashboard/activity` | None (Overview recent-requests is the cousin) | C |  |
| report:1110 [Logs] | Conversations (multi-turn) | MISSING | `/dashboard/conversations` | None | C |  |
| report:1111 [Logs] | Compression log tab | MISSING | `logs/CompressionLogTab.tsx` | None | C |  |
| report:1112 [Logs] | Audit log (+ MCP/A2A audit) | MISSING | `/dashboard/audit` | None — guard denials counted only, no audit view | C |  |
| report:1113 [Logs] | Log export destinations (scheduled, high-water mark) | MISSING | `/dashboard/log-export` | Manual DB export only | C |  |
| report:1125 [Settings] | Automatic failover / Prompt-cache pinning / Cost-aware routing / Health checks / Request logging sw… | WRONG | Settings toggles (various pages) | **All 5 are display-only.** One generic handler (`index.html:5902-5908`) flips `aria-checked` and toasts "· saved" — **no fetch, no persistence**. The word "saved" is false. | C |  |
| report:1126 [Settings] | Listen address + Port | WRONG | Settings → Server equivalents | Listen select has **no handler**; Port input hard-coded `value="8080"` (wrong — real default is 20128), no handler | C |  |
| report:1127 [Settings] | Resilience settings page (queue/cooldown/breaker/wait budgets) | MISSING | `/dashboard/settings/resilience` (route confirmed; fields per §2.3) | Cooldown page covers 2 modes only; no resilience settings page | C |  |
| report:1129 [Settings] | CORS allowed origins | MISSING | Settings → Security | None | C |  |
| report:1130 [Settings] | Blocked providers from `/v1/models` | MISSING | Settings → Security | None | C |  |
| report:1131 [Settings] | IP filter section | MISSING | Settings → Security `IPFilterSection` | None | C |  |
| report:1132 [Settings] | MFA | MISSING | Settings → Security `MfaSection` | None | C |  |
| report:1133 [Settings] | PII masking card (in Security) | PARTIAL | `PiiMaskingCard.tsx` in Security settings | Present as a full standalone Guardrails page (toggles wired via `POST /dashboard/guardrails`, :7700) — function present, placement differs; fold pending per item 296 | C |  |
| report:1135 [Settings] | Feature flags grid | MISSING | `/dashboard/settings/feature-flags` | None | C |  |
| report:1136 [Settings] | Appearance (Light/Dark/System, palettes, branding) | PARTIAL | `/dashboard/settings/appearance` | Light/dark toggle only; no System, no palettes, no branding | C |  |
| report:1137 [Settings] | AI Settings (thinking budget, system prompt, etc.) | MISSING | `/dashboard/settings/ai` | None (mostly theirs-specific) | C |  |
| report:1138 [Settings] | Modality Bridge settings | MISSING | `/dashboard/settings/modality-bridge` | None | C |  |
| report:1139 [Settings] | Cache settings UI (enable, threshold, TTL, engine, embedding provider/model, test) | PARTIAL | `/dashboard/settings/cache` | Backend-configured only; no Settings UI for TTL/threshold | C |  |
| report:1140 [Settings] | Sidebar customisation (presets, pinning, reorder) | MISSING | `/dashboard/settings/sidebar` | None — deliberate simple nav, do not copy sprawl | C |  |
| report:1141 [Settings] | Storage: DB stats, backups/restore, retention days, vacuum, export/import, reset usage | PARTIAL | `/dashboard/settings/general` | DB export/import/wipe exist (Route guards + danger zone); no backups/restore UI, no retention/vacuum UI | C |  |
| report:1142 [Settings] | Advanced (payload rules, body limit, model aliases, lockout card) | MISSING | `/dashboard/settings/advanced` | None | C |  |

## 3. Stale / contradictory rows flagged (verify, do not re-mark)

Each item was checked with a targeted grep against the current tree. "STALE" here means
the PARITY row text no longer matches the tree; the verdict still needs a reviewer/coordinator
call before any flip (this scout changes nothing).

1. **Hedge body-size guard — Area 1 "Hedging config" (PARITY.md:427) + Area 12 row 1
   (PARITY.md:959) + Area 8b "Hedging control" (report:1013): STALE.** Both PARITY rows say
   "no body-size guard". The guard exists: `internal/router/hedge.go:14-16`
   (`hedgeMaxBodyBytes = 256 << 10`), enforced at `hedge.go:44` and `router.go:4288`. Built in
   M4.8 (PLAN.md). The Area 1 / Area 12 rows need a flip to FULL (or a narrower PARTIAL for
   the remaining "off by default / 500 ms" differences — hedging is ON at 150 ms by design).
   The 8b row's gap is only the missing UI control.
2. **Area 9 is pervasively stale — M9 built it, PARITY.md was never reconciled.** M9.0 was
   tasked with "Area 9 reconciliation" and its review confirmed "5 stale Area 9 rows", but
   the table still shows the pre-M9 state even after the M9 merge wave (commit 035d1ed) and
   M9.8/M9.9. Verified live in the tree: 24 tools (3 built-in + 9 routing + 3 cost + 7 ops +
   2 search — `internal/mcp/tools_routing.go`, `tools_cost.go`, `tools_ops.go`,
   `tools_search.go`); SSE + Streamable HTTP transports (`internal/server/mcp_http.go`,
   `guard.go:133-137`); scope system with enforcement ON by default and no off switch
   (`internal/mcp/scopes.go:10-11`, `mcp.go:596`); audit log (`internal/mcp/audit.go`);
   per-client identity (`internal/mcp/identity.go`); tool search + heartbeat tool
   (`tools_search.go:26`); allow/deny filter + maxTools (`tools_search.go:88,375`); client
   spend cap + cost estimate + allowlist (`internal/mcptools/controls.go`, M9.6). Affected
   rows (all currently MISSING/PARTIAL, all need re-verdict, most need flips not builds):
   SSE transport, Streamable HTTP, Tool breadth (24/110, still PARTIAL), Routing/combo
   tools, Cost/quota tools, Cache tools, DB health + pricing sync (pricing sync still
   absent), Memory tools, Tool search, Scope system, Scope enforcement default, Forced
   enforcement, Audit logging, Heartbeat/status telemetry, Tool-cardinality controls, Spend
   controls, Dashboard MCP page/feed. Genuinely still missing in Area 9: A2A REST task
   routes (zero `/api/a2a/tasks` hits) and the tool long tail (24/110).
3. **A2A rows — Area 9 + §14b A2A row: STALE (built in M9.8/M9.9).** POST /a2a live with
   gateway-key auth, owner-scoped, behind `a2a_enabled` (`internal/server/guard.go:241`,
   `server.go:1434`, `internal/a2a/`); all 6 skills live (`internal/a2a/skills.go:19-24`);
   GET /.well-known/agent.json live (`internal/server/a2a_card.go`). Affected rows: A2A
   JSON-RPC endpoint, A2A skills, Agent Card/discovery, A2A auth, A2A owner scoping, A2A task
   persistence (`internal/a2a/store.go` — verify states/streaming before flipping), A2A
   enable flag + dashboard/audit, §14b A2A row ("zero A2A hits"). Conductor delegation
   remains genuinely absent AND must never be built.
4. **Area 4 "MCP scope enforcement" (PARITY.md:538): STALE wording.** Says "MCP is
   stdio-only, 3 read-only tools, no spend path — but also no auth/scoping at all". MCP now
   has HTTP transports, 24 tools and enforced scopes (see #2).
5. **me/sessions exists — Area 4 "Self-service key introspection" (PARITY.md:555) + Area 11
   row 901 (PARITY.md:901): STALE clause.** Both say GET /v1/me/sessions does not exist; it
   has existed since M5.6 (list) + M8.11 (detail). PARITY.md already records this for Area 11
   row 818 in the Area 14 reconciliation note. The attribution half (no session/project/
   client field in ledger/server) is still genuine.
6. **Area 6 "Own-cache savings accounting" (PARITY.md:628) is FULL but Area 10 "Own-cache
   tokens_saved accounting" (PARITY.md:887) is MISSING — CONTRADICTORY.** Area 6 says
   "RecordHit called on own-cache hits; tokens + $ saved recorded (M1.5)". The Area 10 row
   cites `internal/degrade/degrade.go:710` ("the ledger … records nothing") — that comment
   has drifted to `degrade.go:1744`, and RecordHit IS wired (`degrade.go:379`,
   `internal/cache/persist.go:391-415`, `cache_savings` table). The distinction is ledger-row
   tokens (still zero for the hit) vs the savings table (recorded). Needs a reconciliation
   decision: either the Area 10 row closes (savings table counts) or it stays MISSING with
   a corrected rationale (ledger row still records zero tokens). Same underlying gap as Area
   11 "Cost-saved reporting" (PARITY.md:915).
7. **Area 14b "Files/batches mgmt" (PARITY.md:1136): STALE evidence.** Says "batches still
   missing (M8.8 not built)". M8.8 built the batch API (6 routes, real worker) and M8.22
   exit-gated it (70/70 fixtures). Verdict MISSING for the mgmt surface needs a fresh call.
8. **Area 13 "--version / -v" (PARITY.md:1000): STALE — the flag exists.** `-v` / `--version`
   map to the `version` subcommand (`cmd/codvian-router/main.go:490,562`), printing
   `codvian-router v0.1.0`; help lists it (main.go:610). Needs a flip to FULL, not a build.
9. **Area 11 "Batch / flex pricing" (PARITY.md:926): STALE clause.** Says "no batch API". The
   batch API exists (M8.8/M8.22); only the 50% batch-rate price tier is genuinely missing
   (needs a batch flag through provider.ChatRequest → router.Attempt → attemptCost).
10. **Area 14d "Docs breadth" (PARITY.md:1174): STALE clause.** Says "no llm.txt, no
    API_REFERENCE.md". Both now exist, generated from the guard table (M8.21):
    `docs/llm.txt` (43,794 B) and `docs/API_REFERENCE.md` (209,292 B). The breadth gap
    (their 210 English docs + openapi.yaml + provider references vs our 63 .md files)
    remains genuine.
11. **Area 14d test-headline row (PARITY.md:1170): STALE.** Marked UNVERIFIED ("no Go toolchain
    in this env to re-run") with the "1,919" headline. Test counts have since been verified
    repeatedly by real gates (M0 2,410; M1 2,579; M4 2,389; M5 2,523 pass, 0 fail). The
    "1,919" figure itself is stale. Needs a docs update, not a build.
12. **Area 8b backend claims (PARITY.md key rows): PARTIALLY STALE wording, dashboard gap
    stands.** "Single-key disable/revoke — Only revoke-all exists": backend per-key disable
    + revoke exists since M1.3 (`internal/config/gatewaykey.go`, POST /admin/keys/{id}); the
    dashboard control is the gap. "Daily / Weekly / Monthly $ limit — Only rolling 5h
    exists": backend windowed budgets exist since M5.1; dashboard UI is the gap.
    "Quota-share pools — None" / "Team cost centers — None": backends exist since M5.4 /
    M5.12; dashboard UI is the gap. "22 routing strategies in dashboard picker — offers 7
    names": backend now has 31 strategy names (M3); whether the live feed exposes them all
    needs a live re-check during M10 — flagged, not asserted either way.
13. **Area 1 chaos rows: IMPRECISE wording, gap is real.** "Mode pack chaos-mode"
    (PARITY.md:402) and "auto/chaos" (PARITY.md:413) say fault injection is "still missing
    (M4 chaos harness)". The harness exists (`internal/chaos/`, M4.1) but has no non-test
    importer — it is not wired into the chaos-mode strategy or auto/chaos dispatch. The gap
    is the wiring, not the harness.
14. **Area 12 row 12 "Retry-After handling" (PARITY.md:970, FULL — AHEAD): stale note, not a
    gap.** Ours cell says "Missing only JSON-body hints", but M4.4 built Retry-After from
    the JSON body. Note needs a correction; verdict already FULL.
15. **Area 3 WRONG row "Verified-overlay data preservation" (PARITY.md:518): VERIFIED STILL
    TRUE, not stale.** `internal/catalog/catalog.go:402-414`: `All()` replaces the whole
    reference entry with the verified clone, so HasFreeTier is dropped where the verified
    entry does not set it (verified.go sets it only 3×). Live bug, keep WRONG.

## 4. Duplicates across areas (de-dup candidates — do not build twice)

- Fallback-chains CRUD API: Area 2 (PARITY.md:482) = §14b (PARITY.md:1139). One slice.
- Candidate-pool endpoint: Area 1 (PARITY.md:404) = §14a (PARITY.md:1089). One slice.
- Own-cache tokens_saved: Area 10 (PARITY.md:887) = Area 11 Cost-saved reporting
  (PARITY.md:915) — same underlying gap; see §3 #6.
- Batch/flex pricing: Area 11 (PARITY.md:926) = §14a Batches FULL-blockers (PARITY.md:1084).
- Cache analytics / dashboard cache page: Area 6 (PARITY.md:626) = §14b Cache (PARITY.md:1113)
  = §8b (no /dashboard/cache page). API slice is A; page is C.
- Hedging body-size guard: Area 1 (PARITY.md:427) = Area 12 row 1 (PARITY.md:959) — both
  stale, see §3 #1.
- MCP surface: Area 9 rows = §14b MCP row (PARITY.md:1116) = Area 4 MCP scope row
  (PARITY.md:538). One M9 reconciliation, not three slices.
- A2A surface: Area 9 A2A rows = §14b A2A row (PARITY.md:1131). One M9.8/M9.9 verification.
- Version-manager / self-update: §14b (PARITY.md:1127) = Area 13 (PARITY.md:1009).
- me/sessions + attribution: §14a (PARITY.md:1088) = §14b Sessions (PARITY.md:1137) = Area 11
  row 901 (PARITY.md:901).
- Dashboard strategy picker: Area 1 strategy rows = §8b 22-strategy rows (same features,
  backend vs dashboard-picker view). Backend slices close Area 1; the picker rows close only
  when the dashboard offers them.
- The whole of Area 8b (≈113 unlisted rows) re-counts backend features as dashboard UI
  options; PARITY.md's de-dup estimate is ≈80–90 duplicate rows overall.

## 5. Recommended build order — bucket A (gap track), smallest-first

Bucket A has 183 gap rows. Order below is by slice size and dependency (verify-only items
first — they close rows without new code). Sizes are rough slice counts, not time promises.

1. **Verify-only flips (0 new code, reviewer call):** hedge guard (§3 #1 — Areas 1 + 12),
   §14a me/status reviewer call (PARITY.md:1068), Area 12 Retry-After note (§3 #14).
   Closes/advances ~4 rows.
2. **Area 12 latency (3 rows):** latency-aware routing/scoring (latency-aware strategy, p2c
   latency term, model-latency-stats endpoint) + p99 percentiles in telemetry. Small,
   isolated in internal/scoring + internal/metrics + health probe ring.
3. **Area 3 catalog (9 rows):** WRONG overlay fix first (one function, `catalog.All()` —
   preserve HasFreeTier etc. across the overlay), then free-tier surfacing (API + flag),
   key-creation links data (129 links), models.dev sync depth, deprecated handling, static
   model lists. All inside internal/catalog + server feeds.
4. **Area 2 failover residuals (7 rows):** 200-OK body predicate, breaker persistence (only
   persistence is a bigger design item — consider scoping to a minimal durable store),
   connection cooldown hardening, deadline sub-budget counting, 3-attempt cross-provider
   default (config knob), failover-before-retry knobs, fallback-chains CRUD API (shared
   with §14b — one slice).
5. **Area 6 cache residuals (7 rows):** search-cache wiring (route exists, wire the cache),
   reasoning-cache DB tier, prompt-cache markers beyond Anthropic (OpenRouter), cache-write
   tier dashboard math, semantic method (neural embeddings — DECISION: cost/latency vs
   hashed-BoW; keep PARTIAL if prohibitive), persistence tiers (Redis vector — DECISION).
6. **Area 4 auth residuals (9 rows):** key-create API (POST /admin/keys) + per-key policy
   fields + IP allowlist/blocklist + key import/templates + device tracking. Dashboard-auth
   and OAuth/JWT gateway auth need product decisions first (DECISION flags).
7. **Area 11 ledger/analytics (12 rows):** batch/flex price tier (batch flag through the hot
   path — the known hard item), idempotency-key billing safety, cost legs (cached-input,
   per-endpoint prices), gateway-reported cost precedence, flat-rate/$0 treatment, price
   coverage (LiteLLM sync — DECISION), retention/export, analytics surface gaps. Do the
   tokens_saved reconciliation (§3 #6) first — it decides two rows at once.
8. **Area 7 guardrails (19 rows):** PII types (SSN, CPF/CNPJ, IP, IBAN+EU IDs, ZA IDs),
   provider-secret patterns incl. connection strings, custom regex patterns, per-type
   toggles, response sanitization incl. streaming (SSE), prompt-injection severity scoring,
   reversible PII token vault + request rewrite (DECISION: rewrite-before-provider is a
   behaviour change — log-only is by design today), modality bridges (DECISION).
9. **Area 1 strategies residual (29 rows):** mostly data-source gaps, not wiring: 16-factor
   pool scoring (needs the missing 11 factors wired to live data), context/cache/session
   affinity inputs (window + warmth hooks), quota-share saturation gating + P2C, LKGP
   stickiness for auto, candidate-pool exclusions store, 4-tier cascade (needs billing-tier
   signal), auto/* vision + premium-classification sources, live intelligence (Arena ELO —
   DECISION: new external sync). Nadir + SLA need decisions first. Chaos fault-injection
   wiring (harness exists).
10. **Area 10 compression (33 rows):** wire-only first (cache-aware, worker-pool consumer
    decision), then engines: session-dedup + CCR (need cross-turn store design — design
    before building), SmartCrusher + Ionizer (need tabular corpus + round-trip proof),
    relevance extractive scoring, Codex Responses compression, read-lifecycle engine, MCP
    accessibility filtering, RTK pack expansion (18→56), Caveman ultra level + locales,
    native tokenizers (Claude/Gemini/Qwen — DECISION: vendored tables vs provider APIs),
    risk-gate equivalent (secrets/migrations/diffs detection). Output styles + language
    packs last (cosmetic).
11. **Areas 14a + 14b, bucket-A rows (55 rows):** verify the in-tree audio client first (§0.2 —
    do NOT rebuild it), then remaining per-provider routes (embeddings,
    images, limits, voices), multimodal-embeddings, systemone, relay, web/media, mgmt
    proxies, provider-plugin-manifest, vendor extras (DECISION), classify/segment Jina
    proxies (DECISION), search analytics, video-bridge drilldown, music remaining routes,
    files pagination/expiry/caps, quotas remaining routes, translator (DECISION), discovery
    scan/verify, OAuth mgmt login flows, restart endpoint, scheduled DB backups,
    pricing-sync, invalidateByModel, usage/call-logs, compression mgmt API, resilience
    settings/patch + breaker reset, keys/providers mgmt breadth, combos duplicate/import/
    export/test/enable, webhooks PUT. Field-for-field verification for Teams/pools last.

Bucket B (36 rows) is owned by the M9 chain: first step is reconciliation (re-verify + flip
the ~20 stale rows in §3 #2/#3 — no new code), then the genuine remainder: tool long tail
(24→110), A2A REST task routes, protocol gaps (sampling/elicitation). Bucket C (201 rows)
is owned by the M10/M11 chain: Area 8a + 8b dashboard work first (M10 exit gate is zero
UI controls without live backend), then Area 13/14c/14d packaging + docs (M11), with the
8 RELEASE-BLOCKED rows waiting on the first public release.

---

Method note: PARITY.md parsed mechanically (split on unescaped pipes; verdict from the
Verdict column; Area 12 technique table 5th column; Area 14d 3rd column; Area 12
measured-numbers table excluded). Two rows needed manual verdict recovery because their
"Ours" cells contain unescaped pipes (`max |error|` in Area 10 "Measured accuracy audit";
`string|object|array` in §14b "Guardrails") — both verified by reading the raw lines
(PARITY.md:893, PARITY.md:1122). No test suite was run. Nothing was committed or pushed.
No file other than this one was written.
