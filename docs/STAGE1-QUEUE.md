# docs/STAGE1-QUEUE.md — Stage 1 build queue (new working mode, Option 1 stage-wise)

Date: 2026-11-20 ~03:51 IST. Sources: PDF roadmap blocks B2+B3+B4
(`~/workspace/goals/codvian-router/files/codvian-router-roadmap-to-100-percent/codvian-router-roadmap-to-100-percent.pdf`),
`docs/PARITY.md` (re-parsed this run — see §Counts), `docs/BURNDOWN.md` (tiers/sizes/blockers).
Method: every feature below traces to a PARITY.md row (current line numbers, post
batch-1 flips). Files are from each row's own file:line column, plus the
BURNDOWN/overlap analysis where a row's file column is "—". Nothing guessed.

## Counts (verified this run)

- Stage 1 total: **122 features** = B2 44 + B3 26 + B4 52 (PDF roadmap).
- **ALREADY DONE: 5** — :523 (70dd402), :524 (01bbf10), :553 (a5866cb),
  :1038 (6ccea18; Area 12, NOT Area 1 — see correction below), :1040
  (core 8dc54da + wirings fd813a3; V-20 PASS, flip in batch-1 close).
- **Remaining to build: 117** — B2 39, B3 26, B4 52.
- Current parity (parity.sh MATCH, batch-1 close ~03:55 IST): FULL 248 /
  PARTIAL 186 / MISSING 216 / WRONG 2, comparable 652, strict **38.0%**,
  weighted **52.3%**.
- B2 = Area 1 (26) + Area 2 (7) + Area 6 (7) + Area 12 (4: 2 PARTIAL +
  1 WRONG + 1 FULL). B3 = Area 4 (7) + Area 7 (19). B4 = Area 3 (8) +
  Area 11 (12) + Area 10 (32).
- NOTE on line numbers: PARITY.md lines in the B2/B3/B4 tables below were
  read before commit 25abc18, which added 2 lines — add 2 when grepping
  those rows. Area 10 lines (fresh re-parse) are current.

## Corrections found while building this queue (all applied, parity.sh MATCH)

1. **:1038 flip mis-attributed (my error, commit 25abc18).** The
   "Latency-aware routing / scoring" row lives in **Area 12**, not Area 1.
   The flip commit 03bb059 wrongly updated the Area 1 header (38/25) instead
   of Area 12. Corrected: Area 1 back to FULL 37 / PARTIAL 26, weighted
   **79.4%** (M3 gate NOT met — the 80.2% figure was the header error);
   Area 12 now FULL 13 / PARTIAL 2, weighted 87.5%. Headline totals were
   unaffected (the two header errors cancelled in the sum).
2. **BURNDOWN Area 4 is stale (9 listed, 7 open).** Rows :611 (per-key policy
   fields) and :612 (per-key IP allowlist) are already **FULL** (V-12 PASS,
   B9 G21 work) — BURNDOWN still listed them as open. True Area 4 open = 7.
3. **Area 11 parser trap:** the "Committed vs settled spend" row (PARITY.md:1127)
   is **FULL**; the 12th open Area 11 row is "Analytics endpoints"
   (PARITY.md:1136, PARTIAL, internal/server/analytics_api.go).
4. **:1030 has TWO hedging rows.** V-18 PASSed the Area 12 row "Hedging
   (parallel 2nd attempt)" (PARITY.md:1171, PARTIAL) — flip still pending,
   root action, no build needed. The Area 2 row "Hedged / parallel requests"
   was already FULL and was only strengthened (31ab910).

## B2 — 44 features (39 remaining, 5 done)

### B2 / Area 1 — Strategies / combos / auto scoring: 26 remaining, 0 done

| # | Feature (PARITY.md row) | Line | Verdict | Size/Tier | Blocker | Files | Lane |
|---|---|---|---|---|---|---|---|
| B2-01 | context-relay | 473 | PARTIAL | M/T2 | summariser design | internal/router/relay.go; internal/strategy/affinity.go:354 | L-STRAT |
| B2-02 | reset-aware | 479 | PARTIAL | M/T2 | — | internal/strategy/quota.go:336; internal/strategy/wiring.go | L-STRAT |
| B2-03 | expiry-first | 481 | PARTIAL | M/T2 | expiry data source | internal/strategy/quota.go:454; internal/strategy/wiring.go | L-STRAT |
| B2-04 | headroom | 482 | PARTIAL | M/T2 | per-pair limit source | internal/strategy/quota.go:186; internal/strategy/wiring.go | L-STRAT |
| B2-05 | auto (16-factor pool) | 485 | PARTIAL | L/T3 | 16-factor scorer | internal/autorouter/autorouter.go:204-214,863 | L-STRAT* |
| B2-06 | context-optimized | 487 | PARTIAL | M/T2 | — | internal/strategy/affinity.go:203; internal/strategy/wiring.go | L-STRAT |
| B2-07 | cache-optimized | 488 | PARTIAL | M/T2 | — | internal/strategy/affinity.go:237; internal/strategy/wiring.go | L-STRAT |
| B2-08 | quota-share (internal, 23rd) | 492 | PARTIAL | M/T2 | operator share config | internal/router/cascade.go; internal/strategy/quotashare.go | L-STRAT |
| B2-09 | Account-level fallback subset (9 values) | 493 | PARTIAL | M/T2 | D2 per-key quota signal (#20561) | internal/router/account_fallback.go; internal/config/config.go; cmd/codvian-router/main.go | L-STRAT |
| B2-10 | auto 16-factor scoring (DEFAULT_WEIGHTS) | 494 | PARTIAL | L/T3 | 16-factor scorer | internal/scoring/scoring.go:138-142 | L-STRAT* |
| B2-11 | Router strategy rules (default) | 495 | PARTIAL | L/T3 | 16-factor scorer | internal/scoring/scoring.go | L-STRAT* |
| B2-12 | Router strategy score | 496 | PARTIAL | M/T2 | — | internal/scoring/scoring.go | L-STRAT* |
| B2-13 | Mode pack chaos-mode | 507 | PARTIAL | M/T2 | — | internal/scoring/scoring16.go:374; internal/strategy/wiring.go; internal/router/combo.go | L-STRAT |
| B2-14 | Live intelligence (Arena ELO + models.dev) | 508 | PARTIAL | M/T2 | — | internal/modelsync/ | L-STRAT |
| B2-15 | Candidate-pool endpoint | 509 | PARTIAL | M/T2 | — | internal/server/autocombo_route.go; internal/server/autocombo_handler.go | L-STRAT |
| B2-16 | auto (balanced, LKGP-sticky) | 511 | PARTIAL | L/T3 | 16-factor scorer | internal/autorouter/autorouter.go:206 | L-STRAT* |
| B2-17 | auto/chaos | 518 | PARTIAL | M/T2 | — | internal/router/combo.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-18 | auto/subscription | 519 | PARTIAL | M/T2 | — | internal/router/autodispatch.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-19 | auto/thrifty | 520 | PARTIAL | M/T2 | — | internal/router/autodispatch.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-20 | auto/best-* family | 522 | PARTIAL | M/T2 | — | internal/router/autodispatch.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-21 | auto/pro-* family | 523 | PARTIAL | M/T2 | — | internal/router/autodispatch.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-22 | 4-tier fallback cascade | 526 | PARTIAL | M/T2 | — | internal/router/cascade.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-23 | Endpoint-specific combos | 527 | PARTIAL | M/T2 | provider client | internal/router/cascade.go; internal/autorouter/autovariants.go | L-STRAT |
| B2-24 | Shared attempt budget maxGlobalAttempts clamp | 528 | PARTIAL | M/T2 | — | internal/router/router.go:286-289,569-570 | L-ROUTER |
| B2-25 | Nested combos (maxComboDepth 3, flatten) | 529 | PARTIAL | M/T2 | — | internal/strategytree; internal/router/tree.go | L-STRAT |
| B2-26 | Hedging config | 532 | PARTIAL | M/T2 | — | internal/router/router.go:290-293,572-573 | L-ROUTER |

\* The four 16-factor rows (B2-05, B2-10, B2-11, B2-16) are facets of ONE
shared effort — completing the 16-factor scorer (11 of 16 factors missing).
They go to ONE builder, not four. This is also the M3-gate unlock
(Area 1 weighted 79.4% → ≥80%).

### B2 / Area 2 — Failover, breakers, retries, hedging: 4 remaining, 3 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane | Status |
|---|---|---|---|---|---|---|---|
| B2-27 | Connection cooldown (one key/account) | 563 | PARTIAL | S/T1 | internal/router/router.go; internal/router/cooldown.go (+ config.go knob) | L-ROUTER | Built, NO code change; follow-up :529F DONE (e7d5cbe, hard-mode plain-429 fallback to per-auth cooldown base). Row stays PARTIAL until Stage 1 test phase verifies it. Stale comment in internal/router/cooldown_auth.go header owed — test phase. |
| B2-28 | Deadline-failure sub-budget, clean 504 | 577 | PARTIAL | S/T1 | internal/router/router.go:443-449 (+ config.go; internal/router/streamlifetime.go) | L-ROUTER | After B2-27 (same router.go) |
| B2-29 | Default cross-provider policy (3 attempts) | 584 | PARTIAL | S/T1 | internal/router/router.go (+ config.go knob) | L-ROUTER | After B2-28 |
| B2-30 | Failover-before-retry + retry knobs | 586 | PARTIAL | S/T1 | internal/router/router.go (+ config.go knob) | L-ROUTER | After B2-29 |
| — | 200-OK body validation predicate | 551 | FULL | — | — | — | DONE batch 1: :523, commit 70dd402, flip fd46313 (V-21 PASS) |
| — | Breaker states, persisted, lazy recovery | 531 | FULL | — | — | — | DONE batch 1: :524, commit 01bbf10, flip 086f5fc (V-19 PASS) |
| — | Fallback-chains CRUD API | 553 | FULL | — | — | — | DONE batch 1: :553, commit a5866cb, flip 5f2dabd (V-16 PASS; chainFor wiring is a follow-up) |

### B2 / Area 6 — Caching: 7 remaining, 0 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane |
|---|---|---|---|---|---|---|
| B2-31 | Semantic similarity method | 714 | PARTIAL | S/T4 | internal/cache/cache.go:330-360,557,623 | L-CACHE |
| B2-32 | Persistence tiers | 722 | PARTIAL | S/T4 | internal/cache/persist.go; internal/cache/semantic_persist.go; internal/degrade/degrade.go | L-CACHE† |
| B2-33 | Cache analytics | 731 | PARTIAL | S/T4 | internal/server/cache_stats.go; internal/cache/persist.go | L-CACHE |
| B2-34 | Cache-write tiers (5-min / 1-h) | 734 | PARTIAL | S/T4 | internal/ledger/ledger.go; internal/wire/anthropic.go; internal/config/prices.go | L-LEDGER |
| B2-35 | Provider prompt-cache markers | 735 | PARTIAL | S/T4 | internal/provider/anthropic_client.go; internal/wire/anthropic.go | L-CACHE |
| B2-36 | Reasoning cache (2 h) | 738 | PARTIAL | S/T4 | internal/cache/reasoning_cache.go; internal/degrade/degrade.go; cmd/codvian-router/main.go | L-CACHE† |
| B2-37 | Search cache (60 s) | 739 | PARTIAL | S/T4 | internal/cache/search_cache.go | L-CACHE |

† B2-32/B2-36 touch internal/degrade/degrade.go, owned by L-COMPRESS —
sequence their degrade.go lines after L-COMPRESS finishes its degrade.go work.

### B2 / Area 12 — Latency, startup, memory, binary: 2 remaining, 2 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane | Status |
|---|---|---|---|---|---|---|---|
| B2-38 | Hedging (parallel 2nd attempt) — body-size guard | 1173 | PARTIAL | S/T1 | internal/router/hedge.go; internal/router/router.go:290-293,572-573,3141,3441-3462 | L-ROUTER | V-18 PASS (commit c1b215e) — FLIP PENDING, root action, no build needed |
| B2-40 | Measured failover-switch time (WRONG) | 1190 | WRONG | L-LAT | (none — quiet benchmark window measurement) | L-LAT | NOT a builder task: measure in a quiet window on the PC; fix or re-scope the row |
| — | Latency-aware routing / scoring | — | FULL | — | — | — | DONE batch 1: :1038, commit 6ccea18, flip 03bb059 (V-15 PASS). Area 12, not Area 1. |
| — | Latency percentiles in telemetry (p99) | 1183 | FULL | — | — | — | DONE batch 1: :1040, core 8dc54da + wirings fd813a3, V-20 PASS, flip in batch-1 close (parity 38.0%/52.3%). |

## B3 — 26 features (26 remaining, 0 done)

### B3 / Area 4 — Keys, auth, policy: 7 remaining, 0 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane |
|---|---|---|---|---|---|---|
| B3-01 | MCP scope enforcement (read-only ops) | 643 | PARTIAL | M/T2 | internal/mcp/mcp.go | L-AUTH |
| B3-02 | Per-key device tracking (sessions) | 647 | MISSING | M/T2 | web/index.html (keys comment) | L-AUTH† |
| B3-03 | Key policy templates / key import | 648 | MISSING | M/T2 | internal/server/guard.go:167 | L-AUTH |
| B3-04 | Dashboard authentication | 653 | MISSING | L/T3 | NEW SUBSYSTEM (password/session/login) | L-AUTH |
| B3-05 | Self-service key introspection | 660 | PARTIAL | M/T2 | internal/server/keystatus.go:57-143 | L-AUTH |
| B3-06 | Key creation via API / dashboard | 661 | MISSING | M/T2 | web/index.html:5676-5680 | L-AUTH† |
| B3-07 | OAuth/JWT gateway auth | 662 | MISSING | L/T3 | NEW SUBSYSTEM (OAuth 2.0 PKCE + JWT) | L-AUTH |

† B3-02/B3-06 touch web/index.html — sequence with L-GUARD's B3-25 (:747)
and L-CATALOG's dashboard touches; web/index.html is a shared bottleneck.

### B3 / Area 7 — Guardrails / PII: 19 remaining, 0 done

| # | Feature | Line | Verdict | Size/Tier | Blocker | Files | Lane |
|---|---|---|---|---|---|---|---|
| B3-08 | Phone-number redaction | 749 | PARTIAL | S/T4 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-09 | SSN detection/redaction | 750 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-10 | Credit-card detection/redaction | 751 | PARTIAL | S/T4 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-11 | CPF/CNPJ (Brazilian tax IDs) | 753 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-12 | IP address (v4/v6) detection | 754 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-13 | IBAN + EU national IDs | 755 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-14 | South African IDs | 756 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-15 | Provider-secret patterns | 757 | PARTIAL | S/T4 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-16 | Connection strings (DB URLs) | 762 | MISSING | ?/T7 | STALE? critic fix be15f10 may have covered it — V re-check FIRST, do not build blind | internal/guardrails/guardrails.go | L-GUARD |
| B3-17 | Per-type toggles | 764 | MISSING | M/T2 | — | internal/guardrails/guardrails.go (+ config surface) | L-GUARD |
| B3-18 | Custom regex patterns | 765 | MISSING | M/T2 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-19 | Reversible PII token vault | 766 | MISSING | L/T3 | vault design (no format agreed yet) | NEW SUBSYSTEM | L-GUARD |
| B3-20 | Request rewrite (redact before sending) | 767 | MISSING | M/T2 | — | internal/server/server.go:4137 applyGuardrails | L-GUARD |
| B3-21 | Response sanitization, non-streaming | 768 | PARTIAL | S/T4 | — | internal/server/server.go:4252 applyResponseGuardrails | L-GUARD |
| B3-22 | Response sanitization, streaming | 769 | MISSING | M/T2 | — | internal/server/server.go (applyResponseGuardrails comment) | L-GUARD |
| B3-23 | Prompt-injection detection | 770 | PARTIAL | S/T4 | — | internal/guardrails/guardrails.go | L-GUARD |
| B3-24 | Modality bridges | 779 | MISSING | M/T2 | — | (routing; files TBD by builder — row has no file column) | L-GUARD |
| B3-25 | Dashboard + counters for guardrails | 781 | PARTIAL | S/T4 | — | web/index.html:1615 | L-GUARD† |
| B3-26 | Route coverage of checks | 782 | PARTIAL | S/T4 | — | internal/server/embeddings.go:213; internal/mcp/mcp.go:288 | L-GUARD |

## B4 — 52 features (52 remaining, 0 done)

### B4 / Area 3 — Provider catalog: 8 remaining, 0 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane |
|---|---|---|---|---|---|---|
| B4-01 | Routable base URL on catalog entries | 605 | PARTIAL | S/T4 | internal/catalog/catalog.go | L-CATALOG |
| B4-02 | Static per-provider model list | 608 | PARTIAL | S/T4 | internal/catalog/catalog.go:236; internal/server/providers_add.go:83-90 | L-CATALOG |
| B4-03 | models.dev sync | 610 | PARTIAL | S/T4 | internal/modelsync/modelsync.go:1-27 | L-CATALOG |
| B4-04 | Custom-provider prefix / slug | 613 | PARTIAL | S/T4 | internal/catalog/custom.go | L-CATALOG |
| B4-05 | Free-tier marking | 616 | PARTIAL | S/T4 | internal/catalog/catalog.go | L-CATALOG |
| B4-06 | Free-tier budget / pools / rankings | 617 | MISSING | S/T4 | (no file column — catalog + server by construction) | L-CATALOG |
| B4-07 | Key-creation links | 618 | MISSING | S/T4 | (no file column) | L-CATALOG |
| B4-08 | Deprecated / retired handling | 622 | PARTIAL | S/T4 | internal/catalog/deprecated.go | L-CATALOG |

### B4 / Area 11 — Ledger / analytics / billing: 12 remaining, 0 done

| # | Feature | Line | Verdict | Size/Tier | Files | Lane |
|---|---|---|---|---|---|---|
| B4-09 | Session / project / client attribution | 1113 | MISSING | S/T4 | (no file column — ledger + server by construction) | L-LEDGER |
| B4-10 | Cost computation | 1116 | PARTIAL | S/T4 | internal/ledger/ledger.go:110 | L-LEDGER |
| B4-11 | Gateway-reported cost precedence | 1117 | MISSING | S/T4 | internal/provider/client.go:747-779 | L-LEDGER |
| B4-12 | Price coverage | 1119 | PARTIAL | S/T4 | internal/config/prices.go | L-LEDGER |
| B4-13 | Flat-rate / subscription $0 | 1120 | MISSING | S/T4 | (no file column) | L-LEDGER |
| B4-14 | Cache-write tier accounting | 1121 | PARTIAL | S/T4 | internal/ledger/ledger.go:185-190; internal/server/server.go:1895-1896,2151-2174 | L-LEDGER |
| B4-15 | Refused / fallback-attempt billing | 1123 | PARTIAL | S/T4 | internal/router/router.go:2666,2782,3881,3990,4373,4416; cmd/codvian-router/main.go:701-746 | L-LEDGER* |
| B4-16 | Cost-saved reporting (cache / compression savings) | 1127 | PARTIAL | S/T4 | internal/ledger/ledger.go | L-LEDGER |
| B4-17 | Analytics endpoints | 1136 | PARTIAL | S/T4 | internal/server/analytics_api.go | L-LEDGER |
| B4-18 | Retention + export | 1137 | PARTIAL | S/T4 | internal/ledger/dbexport.go | L-LEDGER |
| B4-19 | Idempotency-Key for /v1/chat/completions | 1139 | MISSING | S/T4 | (no file column — server by construction) | L-LEDGER |
| B4-20 | Batch / flex pricing | 1140 | MISSING | S/T4 | (no file column — ledger + prices by construction) | L-LEDGER |

\* B4-15 touches internal/router/router.go — sequence AFTER L-ROUTER finishes
its Area 2 + Area 1 router.go work (B2-27…B2-30, B2-24, B2-26).


### B4 / Area 10 — Compression: 32 remaining, 0 done (3 flagged DO NOT BUILD)

Line numbers are current PARITY.md (re-parsed after commit 25abc18).

| # | Feature | Line | Verdict | Size/Tier | Files | Lane | Notes |
|---|---|---|---|---|---|---|---|
| B4-21 | Engine registry / stacked pipelines | 1068 | MISSING | M/T2 | internal/compress/compress.go:1-540 | L-COMPRESS | |
| B4-22 | Lite whitespace/format cleanup | 1069 | PARTIAL | M/T2 | internal/compress/compress.go | L-COMPRESS | |
| B4-23 | Session-dedup (cross-turn, content-addressed) | 1070 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-24 | CCR (retrieval markers, 24 h + SQLite) | 1071 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-25 | RTK tool-output filter (56 JSON packs, ANSI/progress strip) | 1072 | PARTIAL | M/T2 | internal/compress/rtk.go; internal/compress/compress.go | L-COMPRESS | |
| B4-26 | Codex Responses tool-output compression | 1073 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-27 | Headroom tabular JSON → columnar (SmartCrusher, lossless) | 1074 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-28 | Ionizer tabular row sampling | 1075 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-29 | Relevance extractive sentence scoring | 1076 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-30 | Caveman rule-based prose condensation (lite/full/ultra) | 1077 | PARTIAL | M/T2 | internal/compress/caveman.go; internal/compress/compress.go | L-COMPRESS | |
| B4-31 | Aggressive summarize + progressive aging (~50% claim) | 1078 | MISSING | M/T2 | internal/compress/compress.go | L-COMPRESS | |
| B4-32 | LLMLingua-2 ONNX semantic pruning (~57 MB TinyBERT, fail-open) | 1079 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-33 | Ultra heuristic pruning + code thinning + binary-search truncation | 1080 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-34 | OmniGlyph context-as-image (Claude Fable 5 only) | 1081 | MISSING | M/T2 | — | — | DO NOT BUILD — deliberate do-not-build (BURNDOWN); re-scope to DIVERGENCE |
| B4-35 | LLM compressor (model call to compress) | 1082 | MISSING | M/T2 | — | — | DO NOT BUILD — deliberate do-not-build (BURNDOWN); re-scope to DIVERGENCE |
| B4-36 | Read-lifecycle engine | 1083 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-37 | MCP tool/prompt/resource description compression, default ON | 1084 | MISSING | M/T2 | — | — | DO NOT BUILD — §2.7 MUST-NEVER-COPY (lossy Caveman compression of MCP tool descriptions); re-scope to DIVERGENCE |
| B4-38 | MCP accessibility tool-result filtering (smartFilterText, truncation hints) | 1085 | MISSING | M/T2 | internal/compress/compress.go canonicaliseToolResults | L-COMPRESS | |
| B4-39 | Stacked pipeline RTK → Caveman | 1086 | PARTIAL | M/T2 | internal/compress/pipeline.go; internal/compress/compress.go | L-COMPRESS | |
| B4-40 | Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked) | 1087 | PARTIAL | M/T2 | internal/compress/modes.go; internal/compress/compress.go; internal/server/… | L-COMPRESS | |
| B4-41 | Adaptive budget dial + escalation ladder | 1088 | PARTIAL | M/T2 | internal/compress/modes.go | L-COMPRESS | |
| B4-42 | Cache-aware compression (preserve system prompt, downgrade, prefixFreeze, QuantumLock) | 1089 | PARTIAL | M/T2 | internal/compress/apply_options.go:74,146-150; internal/compress/compress.go | L-COMPRESS | :939 — batch 1 moved MISSING→PARTIAL (eee5cb5, V-17 PASS). Follow-up: own a process-wide observer, call Observe once per request, pass FrozenPrefix in maybeCompress + server handler; QuantumLock has no implementation. |
| B4-43 | System-prompt preservation modes (always/whenNoCache/never) | 1090 | PARTIAL | M/T2 | internal/compress/compress.go | L-COMPRESS | |
| B4-44 | Hard-budget post-pass (compress to ≤ N tokens) | 1091 | PARTIAL | M/T2 | internal/compress/compress.go; internal/compress/postpass.go | L-COMPRESS | |
| B4-45 | Protection guarantees (code/URLs/JSON/identifiers) | 1092 | PARTIAL | M/T2 | internal/compress/fidelity.go; internal/compress/compress.go | L-COMPRESS | |
| B4-46 | Fidelity gate / risk gate / validation | 1093 | PARTIAL | M/T2 | internal/compress/fidelity.go; internal/compress/compress.go | L-COMPRESS | |
| B4-47 | Worker pool / async compression | 1095 | PARTIAL | M/T2 | internal/compress/workerpool.go | L-COMPRESS | |
| B4-48 | Eval harness + measured savings corpus | 1096 | PARTIAL | M/T2 | internal/compress/evalharness/; docs/compression-savings-2026-11-18.md | L-COMPRESS | |
| B4-49 | Output styles (Terse prose, Less code, Ponytail, "I have ADHD", Terse CJK) | 1097 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-50 | Native non-OpenAI tokenizers (Claude/Gemini/Qwen) | 1103 | MISSING | M/T2 | (no file — new) | L-COMPRESS | |
| B4-51 | Own-cache tokens_saved accounting | 1108 | MISSING | M/T2 | internal/degrade/degrade.go:710 | L-COMPRESS | degrade.go — sequence with L-CACHE B2-32/B2-36 |
| B4-52 | Runtime calibration vs provider-reported usage | 1111 | PARTIAL | M/T2 | internal/tokencount/calibration.go; internal/router/router.go | L-COMPRESS | router.go — sequence AFTER L-ROUTER finishes |

Excluded from the queue (not counted in the 32): "Language packs for rules
(11 locales)" (PARITY.md:1098, MISSING) — approved-scope exclusion
(SCOPE-PROPOSAL.md). Effective Area 10 build work: 29 rows (32 − 3 DO NOT BUILD).

## Lanes (one owner per file set — file-disjoint)

| Lane | Owns (files) | Features |
|---|---|---|
| L-ROUTER | internal/router/router.go, cooldown.go, hedge.go, tree.go, streamlifetime.go | B2-24, B2-26, B2-27…B2-30, B2-38 (flip only) |
| L-STRAT | internal/strategy/*, internal/scoring/*, internal/autorouter/*, internal/router/combo.go, cascade.go, autodispatch.go, relay.go, quotashare.go, account_fallback.go, internal/modelsync/, internal/server/autocombo_* | B2-01…B2-23, B2-25 |
| L-CACHE | internal/cache/*, internal/server/cache_stats.go, internal/provider/anthropic_client.go, internal/wire/anthropic.go | B2-31…B2-33, B2-35, B2-37 |
| L-LEDGER | internal/ledger/*, internal/provider/client.go, internal/config/prices.go, internal/server/analytics_api.go | B2-34, B4-09…B4-20 |
| L-GUARD | internal/guardrails/*, internal/server/server.go (guardrails apply sites), internal/server/embeddings.go | B3-08…B3-26 |
| L-AUTH | internal/mcp/mcp.go, internal/server/guard.go, internal/server/keystatus.go, web/index.html (keys parts) | B3-01…B3-07 |
| L-CATALOG | internal/catalog/*, internal/modelsync/modelsync.go, internal/server/providers_add.go | B4-01…B4-08 |
| L-COMPRESS | internal/compress/*, internal/degrade/*, internal/tokencount/calibration.go | B4-21…B4-52 |
| L-LAT | bench/measurement only (no code) | B2-40 (WRONG row — quiet-window measurement) |

## Shared-file sequencing (never two builders on the same file at once)

- **internal/config/config.go** — B2-27…B2-30 knobs (L-ROUTER) first, then
  B2-09 (L-STRAT, account_fallback), then B3-17 (L-GUARD, per-type toggles).
  One builder at a time.
- **internal/router/router.go** — L-ROUTER owns it (B2-24, B2-26…B2-30).
  B4-15 (L-LEDGER) and B4-52 (L-COMPRESS) wait until L-ROUTER is done.
- **web/index.html** — B3-02, B3-06 (L-AUTH) and B3-25 (L-GUARD) run one
  at a time, in that order.
- **internal/server/server.go** — B3-20…B3-22 (L-GUARD) first, then B4-14
  (L-LEDGER).
- **internal/degrade/degrade.go** — L-COMPRESS owns it; B2-32/B2-36 (L-CACHE)
  sequence their degrade.go lines after L-COMPRESS finishes its degrade.go work.
- **internal/strategy/wiring.go** — L-STRAT owns it; its builders sequence
  within the lane (quota/affinity batch first, then B2-13).
- **internal/compress/compress.go** — L-COMPRESS owns it; Area 10 rows run
  sequentially inside the lane (split by sub-file: modes.go, caveman.go,
  rtk.go, pipeline.go, fidelity.go, postpass.go, workerpool.go, evalharness).

## First batch — 12 builders (spawn now; all file-disjoint, parallel)

Build phase only: build the feature, `go build ./...` green on the PC,
commit per feature via scripts/commit.sh. NO tests in the build phase.

| # | Builder task | Rows | Files | Lane |
|---|---|---|---|---|
| 1 | Deadline-failure sub-budget, clean 504 | B2-28 (:543) | internal/router/router.go:443-449, internal/router/streamlifetime.go, config.go | L-ROUTER |
| 2 | Quota strategy batch (reset-aware, expiry-first, headroom, context/cache-optimized) | B2-02, B2-03, B2-04, B2-06, B2-07 | internal/strategy/quota.go, internal/strategy/affinity.go, internal/strategy/wiring.go | L-STRAT |
| 3 | 16-factor scorer (ONE builder — facets of one effort; M3-gate unlock) | B2-05, B2-10, B2-11, B2-12, B2-16 | internal/scoring/scoring.go, internal/autorouter/autorouter.go | L-STRAT |
| 4 | auto/* family + 4-tier cascade + endpoint combos | B2-17…B2-23 | internal/autorouter/autovariants.go, internal/router/autodispatch.go, internal/router/cascade.go, internal/router/combo.go | L-STRAT |
| 5 | Semantic similarity | B2-31 (:680) | internal/cache/cache.go | L-CACHE |
| 6 | Search cache (60 s) | B2-37 (:705) | internal/cache/search_cache.go | L-CACHE |
| 7 | Provider prompt-cache markers | B2-35 (:701) | internal/provider/anthropic_client.go, internal/wire/anthropic.go | L-CACHE |
| 8 | PII patterns batch 1 (SSN, CPF/CNPJ, IP v4/v6, IBAN+EU IDs, ZA IDs) | B3-09, B3-11, B3-12, B3-13, B3-14 | internal/guardrails/guardrails.go | L-GUARD |
| 9 | Catalog batch (routable base URL, static model list, free-tier marking, deprecated handling) | B4-01, B4-02, B4-05, B4-08 | internal/catalog/catalog.go, internal/server/providers_add.go, internal/catalog/deprecated.go | L-CATALOG |
| 10 | MCP scope enforcement | B3-01 (:609) | internal/mcp/mcp.go | L-AUTH |
| 11 | Ledger batch (cost computation, price coverage, cost-saved reporting) | B4-10, B4-12, B4-16 | internal/ledger/ledger.go | L-LEDGER |
| 12 | Compression core (pre-request/response/provider/model/request-type/user-level) | B4-21…B4-26 (first 6 Area 10 rows) | internal/compress/compress.go | L-COMPRESS |

Already running/done: :529 (B2-27) — built, NO code change; follow-up :529F
DONE (e7d5cbe, hard-mode plain-429 fallback). Row :529 stays PARTIAL until
the Stage 1 test phase verifies it.

## Wave 2 (spawn as first-batch builders finish — same lanes)

- L-ROUTER: B2-29 (:550), B2-30 (:552) — after builder 1; B2-24 (:494),
  B2-26 (:498) after those.
- L-STRAT: B2-01 (done e6bb50f), B2-08 quota-share (cascade.go;
  quotashare.go), B2-09 account fallback (D2-blocked, #20561),
  B2-13 mode pack chaos-mode (scoring16.go; wiring.go; combo.go —
  nothing to build, M4 fault-injection only), B2-14 (done a5d18f1),
  B2-15 candidate-pool endpoint (autocombo server files), B2-25 nested
  combos (strategytree; router/tree.go — DECISION NEEDED: build
  combo-in-combo vs re-scope DIVERGENCE). Row names/files per the
  table above; PARITY line numbers shift with every flip — grep the
  row name, never trust a stored line number. (CORRECTED 2026-11-20
  ~04:20 IST: the old list's names/files/lines were wrong and sent
  S1-STRAT2 to nonexistent files — see findings.)
- L-CACHE: B2-32 (:688), B2-33 (:697), B2-36 (:704) — degrade.go lines
  sequenced after L-COMPRESS.
- L-LEDGER: B4-09, B4-11, B4-13, B4-14 (after L-GUARD server.go),
  B4-15 (after L-ROUTER), B4-17, B4-18, B4-19, B4-20; B2-34 (:700).
- L-GUARD: B3-08, B3-10, B3-15, B3-17 (after config.go frees), B3-18,
  B3-20, B3-21, B3-22, B3-23, B3-24, B3-25 (web/index.html, sequenced),
  B3-26; B3-16 (:728) only after V re-checks whether critic fix be15f10
  already covered it; B3-19 (:732 vault) needs a vault design decision first.
- L-AUTH: B3-02, B3-03, B3-05, B3-06 (web/index.html, sequenced);
  B3-04 (:653 dashboard auth) and B3-07 (:662 OAuth/JWT) — each its own
  builder, L size, new subsystems.
- L-CATALOG: B4-03 (:576 modelsync.go), B4-04 (:579 custom.go), B4-06,
  B4-07 (no file column — builder greps first).
- L-COMPRESS: remaining Area 10 rows B4-27…B4-52 (minus the 3 DO NOT BUILD),
  sequenced inside the lane.
- L-LAT: B2-40 (WRONG failover-switch-time row) — quiet-window measurement
  on the PC, not a builder task.

## Stage 1 build findings (first batch, 2026-11-20 ~04:05 IST)

- B2-37 Search cache (60 s): NO CODE CHANGE — already built and wired
  (G13 merge wave; POST /v1/search → mediaPassthroughSearch →
  GetOrCoalesce, 60 s TTL; tests in g13_search_cache_test.go +
  m6_6_search_cache_test.go). PC build OK. PARITY.md row text stale
  ("still unwired") — test phase verifies + rewrites the row.
- B3-09/11/12/13/14 PII batch 1: NO CODE CHANGE — all 5 rows already
  fully built (commit 90f605c, G18 Area 7 wave; redactRules +
  patternValidators in guardrails.go/pii_types.go; 65 assertions in
  g18_area7_test.go). PARITY.md rows stale MISSING — test phase verifies
  (run g18_area7_test.go cold on the PC) + flips.
- Wave 2 spawned on the freed lanes: S1-CACHEANALYTICS (B2-33),
  S1-PII2 (B3-08/B3-10/B3-15).

## Stage 1 build findings (first batch, continued — 2026-11-20 ~04:06 IST)

- B2-35 prompt-cache markers: REAL FIX, commit f790f58 — client-supplied
  cache_control on image_url parts was dropped by convertImagePart
  (wire/anthropic.go); rest of the row was already built (G13). Row stays
  PARTIAL until test phase.
- B3-01 MCP scope enforcement: NO CODE CHANGE — fully built (scopes.go,
  handleWithIdentity, all 24 tools scoped, fail-closed, ON by default).
  Row text stale — test phase verifies + rewrites.
- B2-02/03/04/06/07 quota batch: NO CODE CHANGE — all 5 already built in
  strategy files. B2-06/B2-07 fully wired (stale row text). B2-02/B2-03
  have a real producer gap: no measured quota-fraction producer exists
  (only SetQuotaHint(0) on quota error) — provider/health-lane follow-up.
  B2-04 needs a per-pair limit (product decision, D2). Test phase verifies.
- B2-33 cache analytics: ALREADY BUILT — backend complete (cache_stats.go,
  persist.go SavingsTotals, live wiring); only gap is /dashboard/cache
  page (M10, Stage 3). Row stays PARTIAL until test phase.
- B2-28 deadline sub-budget: NO CODE CHANGE — counting + sub-budget
  already built and wired in the chat path (deadline_budget.go, 504 at
  server.go:6027). Remaining: stream-path counting (design decision),
  no config surface (main.go owner), hedge-branch counting, default-on
  divergence (ours 3 vs OmniRoute opt-in) — record at flip time.
- B3-08/10/15 PII batch 2: NO CODE CHANGE — all 3 already built
  (90f605c). Phone is AHEAD (8 country forms vs OmniRoute's 2); card Luhn
  wired; 30 secret rules vs OmniRoute's 28. Test-phase note: 15-digit
  Amex shape never reaches validCardLuhn (regex is 16-digit groups).
- Wave 2 spawned on the freed lanes: S1-B2-29, S1-B2-01, S1-B2-14,
  S1-PII3 (B3-18/23), S1-GUARD-SERVER (B3-20/21/22), S1-AUTH-KEYS
  (B3-03/05).
- B2-29 default cross-provider policy: NO CODE CHANGE — already built
  (G11 Area 2; failover_policy.go: 3-provider default + prefixed never
  rerouted, both loops; 4 existing G11 tests). Follow-ups: no operator
  override knob (dead-knob pattern, correctly not added); plain chat path
  does not translate "provider/model" into routing.only (server-side work).
  Row flippable in test phase.
- S1-CATALOG B4-01: REAL WORK, commit f5f8a59 — exposes computed
  routable flag in GET /api/catalog feed (catalog.Provider.Routable).
- PC refresh incident (~04:07 IST): `git archive` sync failed on the
  TRACKED binary `codvian-bench` (10.6 MB, committed in 8e3a41d) —
  "Can't unlink: Permission denied" on the PC. Fixed by `del /F` on the
  PC + re-sync (SYNC_EXIT:0, Routable marker verified). The binary
  should not be tracked — untrack it at the next clean point (it
  re-syncs on every refresh otherwise).
- S1-AUTO B2-17…B2-23: NO CODE CHANGE — all 7 already built (resolver +
  dispatch + engine path). REAL end-to-end gap found: the dispatched
  thrifty ladder is re-sorted in orderAttemptsWithContext (router.go:2430)
  — quota-hint sort + 16-factor cost-saver hook re-rank the chain,
  inverting the ladder (plan-first). Fix: thread an explicit-order flag
  from dispatchAutoVariant through chat/stream entry points (router.go
  change; CHECKLIST 173 precedent). Spawned as S1-AUTO-FIX. Other gaps:
  subscription rung has no live Billing data source (main.go/registry);
  cascade not in routing.global_strategy allowlist (config.go); audio
  provider client missing (endpoint combos).

## Stage 1 build findings (continued — 2026-11-20 ~04:12 IST)

- B3-03/B3-05 (AUTH-KEYS): NO CODE CHANGE — both already built. B3-03
  templates + import fully built (admin_keys_api.go). B3-05: /v1/me/sessions
  EXISTS (row stale) but Ledger.AgentSessions has ZERO production callers
  — AK-3 follow-up needs a mesessions.go owner; row must NOT flip until
  that wiring lands.
- B2-01 context-relay: REAL WORK, commit e6bb50f — summariser message
  selection (system-pinned, token-budget trim) in strategy + router
  mirror. Blocked (not built): production summariser (design decision),
  quota-use signal (D2), cross-request session store, threshold/TTL
  config surface, budget-gate handoff-token counting, Responses/Claude
  injection in the engine.
- B4-21…B4-26 compression core: 4 BUILT (0461bdd registry, 6ab2d64
  session-dedup, c7a0b5b+9715c6d CCR, 39a60c7 Codex), 2 ALREADY BUILT
  (B4-22, B4-25). ADMISSION: first CCR commit c7a0b5b landed while the PC
  build was red (Prune 2-value bug) — helper script exits 0 on build
  failure; fixed in 9715c6d, tree green now. Future briefs: builders must
  check the build exit code themselves before committing. Follow-ups:
  server wiring (session id + dedup store, CCR path, Codex budget, RTK
  level HTTP field).
- B4-10/12/16 ledger: B4-16 BUILT (6bc67b9, CacheTokensSaved seam).
  B4-10 already built. B4-12 cannot be built in ledger.go (needs
  prices.go/modelsync). Follow-ups: async.go insertChunk omits the new
  column (live path drops it — spawned S1-LEDGER-FIX), import.go,
  router/server call-site mapping (main.go).
- B2-31 semantic similarity: REAL WORK, commit d933d22 — NeuralEmbedder
  seam, fail-open to BoW, 7 APIs rewired, same-length guard. Follow-up:
  no provider client wired yet (main.go/degrade).
- B4-01/02/05/08 catalog: B4-01 f5f8a59 + B4-08 5e42881 built; B4-05
  already built; B4-02 PARTIAL no code (zero seed Models, zero
  StaticModels callers, probe-only add path) — follow-ups: seed data
  generation + registry fallback (spawned S1-B402); B4-04 spawned
  (S1-B404).
- PC copy pollution: several builders' porcelain syncs copied other
  builders' WIP files onto the PC. Refreshed to HEAD when no PC build
  was live (marker SelectMessagesForSummary verified). Builders must
  sync ONLY their own files from now on (briefs updated to explicit
  file lists).
- Wave 3 spawned: S1-LEDGER-FIX (async.go/import.go), S1-B402 (registry
  fallback), S1-B404 (custom.go).
- B3-20/21/22 guardrails server sites: REAL WORK, commit 0548730 —
  request rewrite (WithRequestRewrite), response sanitization
  non-streaming + streaming (WithResponseSanitization /
  WithResponseGuardMode, stream cut on block). All opt-in, off by
  default. Follow-ups: main.go wiring (features dormant until wired),
  relay.go:1485 needs the same B3-22 wiring.
- Stale refresh proc_24692f709d15 (launched ~03:50 IST to fd46313)
  finally reported: TIMEOUT, PC not responding, EXIT:255 — superseded;
  later refreshes to 5060ded and f5f8a59 succeeded, PC copy is past
  fd46313. No action.
- PC refresh skipped ~04:14 IST (PC_BUSY — a builder was mid-build);
  retry at the next wave boundary.
- Wave 4 spawned: S1-GUARD-TOGGLES (B3-17), S1-ROUTECOVERAGE (B3-26),
  S1-AUTH-WEB1 (B3-02), S1-OAUTH (B3-07). DASHAUTH (B3-04) held —
  sequenced after B3-02/B3-06 on web/index.html.
- S1-SCORER16 16-factor scorer: DONE, commit 3414845 — Hook16 /
  Hook16Default + Inputs16FromSignals; Decision.RankedPool exposes pool
  order. 16/16 factors live in code, BUT the default chain still scores
  5-factor until strategy/wiring.go maps StrategyBalanced →
  Hook16Default (spawned S1-SCORER-WIRE). Also: autovariants.go
  pickByStrategyWithPolicy default still returns pool[0] (spawned
  S1-AUTO-BALANCED). Rows must NOT flip until the test phase verifies
  the wired path. M3-gate unlock is one wiring away.
- B2-14 live intelligence: DONE, commit a5d18f1 — Arena ELO sync (24h,
  wulong + HF fallback), models.dev tier derivation, priority TaskFit
  store. Caveats: Arena live fetch hit HTTP 429 from the VM (parser
  written against the documented contract, not live-verified — test
  phase must verify or record upstream unreachable); no ELO/tier signal
  in scoring yet and no config/startup wiring (dormant until wired).
  Spawned S1-ELO-WIRE (scoring + main.go) and S1-CATALOG-SYNC (B4-03 +
  B4-12, modelsync + prices.go) now that modelsync/ is free.
- S1-LEDGER-FIX: DONE, commit 0ada264 — async.go insertChunk (33 cols)
  + import.go INSERT/validation/dedup now carry cache_tokens_saved;
  live AsyncWriter path no longer drops it. Build exit code checked
  directly (echo $?).
- Wave 5 spawned: S1-LEDGER2 (B4-09/11/13/17/18/20 — ledger.go,
  dbexport.go, analytics_api.go, provider/client.go). B4-14/B4-19 held
  (server.go — OAUTH still running); B4-15 held (router.go — AUTO-FIX
  running); B2-34 held (prices.go — CATALOG-SYNC running).
- S1-AUTO-FIX: DONE, commit ee8c30c — OrderedExplicit flag threaded
  through all 7 entry points; dispatched auto chains (thrifty ladder,
  subscription rungs, chaos panel, nadir) keep resolver order — no
  quota-hint sort, strategy hook, or hedge re-rank. router.go free now;
  spawned S1-B2-30 (failover-before-retry). B4-15 held (main.go —
  ELO-WIRE running); B4-52 held (router.go — B2-30 running).
- S1-B402: DONE, commit 328eea6 — registry falls back to StaticModels
  when the live probe yields no list (chat-only branch). Brief
  correction: the file is internal/provider/registry.go, not
  internal/catalog/registry.go. No seed lists invented (separate data
  task).
- S1-SCORER-WIRE: DONE, commit b0c1c14 — StrategyBalanced →
  Hook16Default in wiringHook. The default chain now scores 16-factor
  (the 11 factors with no 5-factor vote now vote). Behavior note:
  custom weights no longer apply to "balanced" (default 16-factor
  weights by definition); custom-weighted 16-factor hook is a later
  follow-up if operators need one. M3-gate unlock now wired in code;
  test-phase verification still owed.
- Wave 6 spawned: S1-STRAT2 (B2-08/13/15/25 — costs.go, tree.go,
  similarity.go, affinity.go). B2-09 held (blocked on D2 per-key quota
  signal).
- S1-B404: DONE, commit ec59149 — ToProvider applies the custom-provider
  prefix (wire format + slug from remainder). Caveat: the live add path
  (provider.Registry.AddContext) still does not consult the prefix
  convention — spawned S1-B404-FOLLOWUP (registry.go). B4-06/B4-07 held
  (server.go — OAUTH still running).
- S1-AUTH-WEB1: DONE, commit 0697bfd — "Devices on a key" card in Keys
  view (GET /v1/me/status, live backend already built in G15). Follow-up:
  per-key device count on every key card needs handleKeys in server.go
  (server lane). Spawned S1-AUTH-WEB2 (B3-06 budget display on keys).
- S1-GUARD-TOGGLES: DONE, commit f583276 — DisabledTypes config surface
  (guardrails.Config + config file, validated at load, round-trips).
  Follow-up: main.go buildGuardrailStore needs one line
  (gc.DisabledTypes = cfg.Guardrails.DisabledTypes) — held (main.go —
  ELO-WIRE running). main.go wiring follow-ups are accumulating
  (toggles, B3-18 patterns, B3-20/21/22 options, B2-01 thresholds,
  :523 response_validation, B2-28 deadline_failure_max) — one
  S1-MAIN-WIRE sweep builder at the next wave boundary.
- S1-ROUTECOVERAGE: DONE, commit 69cd29b — rerank object documents join
  the guardrail scan as raw JSON (real hole: object-shaped docs were
  invisible to the check). Embeddings request check + MCP tool-args
  check already built. Follow-up: response checks on relay + all
  streaming responses still unchecked (server.go — held, OAUTH
  running).
- S1-STRAT2: BLOCKED, NO CODE — root's brief was wrong (row names,
  files, lines all mismatched the queue table; costs.go/tree.go/
  similarity.go do not exist). Builder's read-only findings stand:
  B2-08 quota-share largely stale — saturation gating + DRR wired;
  gaps: QuotaShareFor never set in production (config+main.go),
  "quota-share" not in strategytree.validStrategies allowlist, P2C
  unclear (verifier re-check). B2-13 chaos-mode: nothing to build
  (M4 fault-injection only). B2-15: gaps all in server files
  (exclusion store, ModelLocked/FreeAccessExclusion sources, D2/LKGP
  503s) — held until OAUTH frees server.go. B2-25 nested combos:
  DECISION NEEDED — build combo-in-combo (router/combo.go) vs
  re-scope to DIVERGENCE (trees are the deliberate alternative).
  Spawned S1-B208 (quotashare.go + cascade.go + strategytree/tree.go;
  config.go part held — B2-30 running).
- S1-B404-FOLLOWUP: DONE, commit aa7f16b — live add path prescribes
  wire format from the custom-provider prefix in addProvider. B4-04
  flip waits for the test phase + checker.
- S1-CATALOG-SYNC: DONE, commit 054d302 — LiteLLM sync source support
  (per-source synced layer, source stamping, fail-closed state loading).
  Deliberately NOT built: hardcoded default prices (would be invented
  prices — record as divergence). Remaining: main.go wiring (construct
  LiteLLM syncer + loop, switch models.dev call sites to
  SetSyncedPricesForSource) — goes to the S1-MAIN-WIRE sweep.
- S1-B2-30: DONE, commit 5403cf7 — stream loop honors retry knobs +
  explicit FailoverBeforeRetry switch (both loops). Follow-up:
  config-file surface (routing.max_retries / retry_delay_ms /
  max_set_retries / failover_before_retry) + main.go wiring — goes to
  the S1-MAIN-WIRE sweep. Spawned S1-B224-26 (B2-24/26, router.go).
- S1-AUTH-WEB2: DONE, commit 308ef47 — B3-06 is "Key creation via
  API/dashboard", NOT "budget display" (brief title wrong again —
  verify row names against PARITY.md before spawning). Both the titled
  feature and the actual row are already built; only a stale comment
  fixed. Spawned S1-AUTH-WEB3 (B3-25 dashboard).
- S1-AUTO-BALANCED: DONE, commit cb0fa81 — balanced pick uses the
  ModelRanker hook when provided (nil/panicking/dropping ranker falls
  back to pool[0]). Gap: the live path still calls the nil-ranker
  variants (autodispatch.go:234, autocombo_route.go:141) — spawned
  S1-AUTO-WIRE (call-site switch + ranker getter).
- S1-OAUTH: DONE, commit 15eaddb — OAuth 2.0 PKCE + JWT gateway auth
  (Issuer, JWT HS256 stdlib-only, PKCE S256, rotating refresh tokens,
  jti denylist, oauth.json 0600; 6 endpoints; JWT in the real auth
  funnel with scope enforcement). Scope boundary: gateway auth only —
  the "32 OAuth providers" upstream side (PARITY.md:1368) NOT touched.
  server.go free now; spawned S1-B215 (B2-15). Queued on server.go:
  B4-06/07, B4-19, relay/stream response checks, handleKeys device
  counts.
- S1-B208: DONE, commit 48cb268 — "quota-share" added to
  strategytree.validStrategies (tree-ordered chains no longer ERROR).
  Verified: DRR + P2C + at-cap gating all wired; per-model bucket
  saturation has no live source (cannot be built without inventing
  data). Follow-up: tree-path DRR (evalTree ranks via StrategyHook,
  no quota-share case — spawned S1-B208-TREE, router/tree.go);
  QuotaShareFor config surface → MAIN-WIRE sweep.
- S1-B224-26: NO CODE CHANGE — B2-24 already built (clamp [1,200],
  config surface exists; row text stale, flippable in test phase).
  B2-26 guard already built; gaps: (1) no hedge config surface →
  MAIN-WIRE sweep; (2) DEFAULT DECISION NEEDED: ours ON at 150 ms vs
  OmniRoute OFF by default, 500 ms when enabled — match theirs or
  record ours as deliberate divergence. Row stays PARTIAL until decided.
  Spawned S1-B452 (B4-52, calibration.go + router.go).
- S1-LEDGER2: DONE, commit 22abdc4 — B4-17 analytics endpoints built
  (compression/diversity/auto-routing + auto_variant column seam).
  B4-09/11/13/18/20 verified already built. Follow-ups: (1) main.go
  mapping choke point (Cost/ServiceTier/attribution/AutoVariant +
  FlatRateAsZero surface) → MAIN-WIRE sweep; (2) async.go commitBatch
  never calls upsertAgentSessionTx + auto_variant missing in
  insertChunk/import.go — spawned S1-LEDGER-FIX2; (3) mesessions.go
  AK-3 (AgentSessions zero callers); (4) batch API + price tier;
  (5) export cap. Brief corrections: PARITY lines stale ~+16;
  dbexport.go is internal/server/dbexport.go.
- S1-AUTH-WEB3: DONE, commit 73a3059 — B3-25 "Detections by type"
  card (by_pattern grouped by kind). Follow-up: B3-17 dashboard half
  (per-type toggle UI) — spawned S1-AUTH-WEB4.
- S1-AUTO-WIRE: DONE, commit 4d92d75 — RankModels getter +
  SetAutoRanker supplier + both call sites. Follow-up: main.go one
  line (router.SetAutoRanker next to SetAutoCatalog ~1152) →
  MAIN-WIRE sweep.
- S1-ELO-WIRE: DONE, commit 593568c — TaskFit feeds 16-factor taskFit
  + arena_elo config block + ArenaSyncer wiring; RebuildTier now
  called (first caller). SHARP EDGE: arena_elo block is main.go-local,
  NOT in config.Config — config.Save drops it on the next admin config
  persist; move the field into config.Config → MAIN-WIRE sweep.
- Wave 7 spawned: S1-MAIN-WIRE (the accumulated main.go + config.go
  wiring sweep — list below), S1-B234 (cache-write tiers),
  S1-LEDGER-FIX2 (async/import auto_variant + upsert), S1-AUTH-WEB4
  (B3-17 dashboard half). B4-14/B4-15 held (server.go B215 / main.go
  MAIN-WIRE).
- S1-B208-TREE: DONE, commit 36c7348 — rankAttemptsWithContext
  intercepts "quota-share" before the StrategyHook (same as the flat
  path). Follow-up flagged: "cascade" has the identical latent gap in
  the tree path — spawned S1-B208-CASCADE (router/tree.go).
- S1-B208-CASCADE: DONE, commit fe0fc3e — rankAttemptsWithContext
  intercepts "cascade" before the StrategyHook (same as the flat path).
  L-STRAT lane now done except decisions/blocks: B2-25 (DECISION
  NEEDED: build combo-in-combo vs re-scope DIVERGENCE), B2-09
  (D2-blocked), B2-13 (M4 fault-injection only).
- S1-B234: NO CODE CHANGE — cache-write tiers already fully built end
  to end (wire split, provider carry, main.go mapping, ledger columns
  + costing, prices fields; 17 tests across 4 packages, row cites 10
  — stale). Remaining gap: dashboard shows a cost band (server.go
  passes only the total into cacheComboEconomics; web/index.html
  renders "est. $X–$Y") — fix needs server.go + web/index.html (held:
  B215 / AUTH-WEB4 running).
- S1-AUTH-WEB4: DONE, commit dd1401e — "Detection types" card with
  per-type switches (renders from the feed's types list, POSTs
  disabled_types). B3-17 backend + dashboard both built now; flip in
  test phase.
- S1-LEDGER-FIX2: DONE, commit 0c3f08e — commitBatch folds rows into
  the agent-session rollup in the same transaction; auto_variant in
  insertChunk (34 cols) + import INSERT/dedup (23 cols). L-LEDGER lane
  done except B4-14 (held: server.go B215) and B4-15 (held: main.go
  MAIN-WIRE / router.go B452).
- S1-B215: DONE, commit d380b90 — candidate-pool remainder: per-key
  exclusion store (auto-candidate-overrides.json, mtime reload);
  FreeAccessExclusion price-derived (approximation, documented);
  ModelLocked NOT built (engine exposes no lockout read — needs
  Engine.ModelLockouts() in router.go, folded into the B452 respawn).
  server.go free now; spawned S1-B414 (B4-14, ledger.go + server.go).
  Queued on server.go: B4-06/07, B4-19, relay/stream response checks,
  handleKeys device counts, dashboard cache band fix.
- S1-B452 RESPAWNED: the calibration builder is missing from the live
  agent list and its files are not dirty — it died (the flash Raunak
  saw). Respawned with B4-52 + the ModelLockouts follow-up.
- S1-B452 (respawn): DONE, commit 0896341 — B4-52 already fully built
  + wired (only 3 stale comments fixed); ModelLockouts() already
  exists (lockout.go:257-261, d21d9d1). Brief correction: calibration
  file is internal/tokencount/calibration.go, NOT
  internal/compress/calibration.go (queue table line 235 is right).
  Remaining B2-15 work (lockoutReporter assertion + ModelLocked in
  buildAutoComboPool) needs server.go + autocombo_route.go — held
  (B414 running). B4-15 held (main.go MAIN-WIRE).
- S1-B414: DONE, commit d3afaae — cache-write tier accounting: server
  cacheComboEconomics now returns the exact write cost
  (CacheWriteCostUSD, per-tier rates); ledger exports
  IsAnthropicCacheWriteTier. Follow-up: dashboard still renders the
  band — spawned S1-CACHEBAND (web/index.html, prefer exact number).
  Spawned S1-B406-07 (B4-06/07, catalog+server).
- GLITCH STILL FLASHING (Raunak, 2026-11-20 ~04:26 IST, voice call):
  "the work finished but I didn't receive a summary" appeared again.
  Checked live: NO builder dead — B414 had just finished cleanly
  (d3afaae), MAIN-WIRE live (activity 6 s ago). Likely source: the
  routine silent checks (watchdog 10 m, stall watch 4 m) finish with a
  null summary by design when all is well — harmless, no work lost.
  Ran the recovery cron immediately (cron.run) to verify; it stays
  silent when nothing is dead.
- S1-B406-07: DONE, NO CODE CHANGE — B4-07 already fully built
  (KeyCreationURL field + API + dashboard link, critic b15c2a7; only
  data gap: 18 seeded URLs vs OmniRoute's 129 hints, never guess
  URLs). B4-06 catalog half built (FreeTierPools/Ranking/Summarize),
  API half missing — zero callers; the wire lives in
  internal/server/providers_add.go (handleCatalog/catalogResponse),
  outside the builder's files. Spawned S1-B406-WIRE
  (providers_add.go only: free_tier block in catalogResponse, IDs +
  counts only, no URLs/notes so TestCatalogFeed still passes).
- S1-CACHEBAND: DONE, commit d9a4c35 — dashboard prefers the exact
  cache_write_cost_usd, band only as labelled fallback (totals line,
  per-combo cell, cache-page card). PC build exit 0, JS syntax OK.
  Note: first PC build failed on MAIN-WIRE's in-flight main.go
  (unused encoding/json) — builder waited, no commit on red build.
- S1-B406-WIRE: DONE, commit f3567e2 — free_tier block in GET
  /api/catalog (summary counts + pool_list + ranking, IDs/counts
  only). B4-06 now wired end to end; stays PARTIAL until test phase.
- Spawned S1-B232-36 (B2-32/36, degrade.go) + S1-DASHAUTH (B3-04
  dashboard login/session, web/index.html + server auth files).
  B2-15 remainder + B4-19 + response checks + handleKeys counts +
  B4-17 registration + export cap still queued on server.go behind
  DASHAUTH.
- S1-B415: DONE, commit d2d2b92 — B4-15 billed-refusal/fallback
  billing: hedge losers now record as cancelled rows (CancelledLedger
  interface + recordCancelledAttempt, both loser sites); billed
  refusals now record verbatim reported usage (failedUsage helper in
  noteFailure + noteRefusalPreread). Deviation: touched refusal.go
  (1 line, same package/lane — the content-refusal site lives there).
  Self-caught bad edit on main.go restored verbatim. Test-phase note:
  tests asserting old failed-row recording (hedge_test, combo_test,
  decision_test, hedge_trace_test) may need updating to the cancelled
  contract. Spawned S1-RELAY-CONFIG (item 4: router.go + relay.go +
  main.go).
- S1-DASHAUTH: DONE, commit 8f572dc — B3-04 dashboard login/session:
  Argon2id password in <data-dir>/dashboard_password.json (0600),
  setup (admin key, 409 once set) / login (HttpOnly 12 h session,
  5-fail 60 s throttle) / change (invalidates all sessions) /
  logout; feeds gated once a password is set (session OR gateway
  key), open until then; login overlay + Security card in dashboard.
  No default password (CHANGEME is must-never-copy). Routes now 214
  (was 210) — docs/API_REFERENCE.md still says 210, regen owed in
  test phase. OIDC arm NOT built (no provider configured). /api/*
  feeds NOT gated (separate scope decision). Spawned S1-B215-REMAIN
  (server.go + autocombo_route.go; server.go queue: B4-19, response
  checks, handleKeys counts, B4-17 registration, export cap).
- S1-B215-REMAIN: DONE, commit da1f044 — lockoutReporter + ModelLocked
  set in buildAutoComboPool (bare-model matching verified). GAP:
  production serving engine is degrade.StaleEngine, which forwards
  BreakerStates()/CooldownTargets() but has NO ModelLockouts()
  forwarder — in serve mode ModelLocked still reads false. Fix is
  one method in internal/degrade/degrade.go — sent as follow-up to
  the running S1-B232-36 builder (same file). Spawned S1-B419
  (B4-19 Idempotency-Key, server.go).
- S1-RELAY-CONFIG: DONE, commit 170d771 — relay thresholds/TTL
  config end to end: engine fields (router.go), relayState threading
  + resolving accessors (relay.go), routing.* block (config.go) +
  main.go mapping. Brief correction: the four real knobs are 0.85 /
  0.95 / 5h / 30 messages — no 30-minute TTL exists (the "30" is the
  summariser message cap). Builder waited out B232-36's mid-edit
  degrade.go red build, committed only on green. Spawned
  S1-QUOTA-PRODUCER (rate-limit header parsing), S1-B324 (B3-24),
  S1-B316-CHECK (read-only B3-16 staleness check).
- S1-B232-36: DONE, commits 4c209cb + d417837 (degrade.go only) —
  exact-cache serving on both resume paths (ChatStreamFrom +
  ChatStreamFromChunks, trimmed by skipChars) + injectReasoning on
  text ChatStreamFrom; ModelLockouts() forwarder on StaleEngine
  (savingsEngine inherits via embedding — verified). Brief
  correction: file is internal/degrade/degrade.go, not
  internal/router/degrade.go (queue table right). NOT built: Redis
  vector store (no Redis client in go.mod — separate subsystem
  slice, spawned S1-REDIS-VECTOR); temp-gate divergence left as a
  recorded divergence (behavior decision, not build-only).
  B2-32/B2-36 stay PARTIAL until test phase.
- S1-B316-CHECK: DONE, read-only — B3-16 is BUILT (stale row, no
  builder). Brief correction: the row name is "Connection strings
  (DB URLs)", NOT "unknown error classes" (that name matches nothing
  in PARITY.md). Row PARITY.md:778 MISSING; rule lives in
  guardrails.go:519 (10 schemes incl. mariadb/rediss/amqps, critic
  be15f10), covered by TestG18ProviderSecrets. Flip in test phase
  (cold run + toggle-check + checker). Related: row :773's text
  still lists "connection strings" among its missing items — update
  it when :778 flips.
- S1-B419: DONE, commit d20a143 — B4-19 Idempotency-Key wired into
  handleChatCompletions (server.go only): claim before every gate,
  replay returns stored bytes + X-Idempotency-Replayed, 409 on
  different-body/in-flight, release on every non-completion, finish
  stores exact bytes + real cost. Store was already fully built
  (idempotency.go, 638 lines, zero production call-sites). Scope:
  non-streaming only (streams cannot be replayed); fail-open
  preserved. Row stays PARTIAL until test phase. Spawned
  S1-RESP-CHECKS (server.go response checks) + S1-RELAY-B322
  (relay.go:1485 B3-22 wiring). :939 remainder queued after
  RESP-CHECKS.
- S1-REDIS-VECTOR: DONE, commit 9149db0 — Redis vector store:
  go-redis v9.23.0, RedisSemanticCache (write-through, in-memory
  similarity, plain Redis 7.0+ no modules), cache.redis_* config,
  main.go wiring (Redis set → SQLite not opened; Redis down →
  memory-only, warn). Side effect: go.mod go directive 1.22 →
  1.26.0 (go-redis minimum), x/sys 0.22 → 0.48. NO live Redis
  verification exists (no Redis on VM/PC) — test phase must run
  against a real Redis. Exact tier stays SQLite.
- S1-B324: DONE, commit 20adefe — B3-24 modality bridges: new
  internal/modality package (Detect/Supports/Decide), VisionKnown
  plumbing (models.dev sync sets it; ApplySyncedVision now takes
  (vision, known)), engine hook maybeBridgeModality in all 5 chat
  entry points. Honest limits: only VISION reroutes today (no
  audio/video capability source — follow-up from the models.dev
  modalities table); describe/transcribe + bridge cache/stats NOT
  built (need a call path that doesn't exist). Dashboard settings
  page (:1002) is a separate Stage 3 row. Row stays PARTIAL until
  test phase. Spawned S1-QUOTA-RESTORE (main.go quota-hint restore).
- S1-QUOTA-PRODUCER: DONE, commits c541f5b + 583b3fd (router.go +
  ratelimit.go) — measured quota fractions now learned in
  learnRateLimit (single funnel): sig.RemainingFraction() into the
  engine's in-memory override with expiry (window reset, capped 5
  min) so a 0-remaining provider is skipped then eligible again;
  Retry-After-only records nothing. All six wire families already
  parsed headers (no client change needed). Follow-ups: embeddings/
  images/audio/rerank clients parse no headers (spawned
  S1-QUOTA-EMBED); startup quota-hint restore from ledger is a small
  main.go addition — queued until B324 frees main.go.
- S1-RELAY-B322: DONE, commit d579514 — B3-22 streaming response
  guard in handleMessagesStream (internal/server/relay.go only).
  Brief correction: the file is internal/server/relay.go, NOT
  internal/router/relay.go (queue's "relay.go:1485" is server/relay.go
  :1485). Scope: handleMessagesTranslated (non-streaming) still has
  no applyResponseGuardrails call — belongs to S1-RESP-CHECKS, not
  here.
- Wave 8 spawned (05:21 IST, after stall-watch EMPTY-FLEET alert —
  fleet verified empty: last commit f329fbc 24 min ago, no dirty code,
  no go test/build or PC SSH processes; MAIN-WIRE had died silently
  with no DONE note/commit): S1-MAIN-WIRE respawn (15-item sweep,
  main.go + config.go) + S1-939-REMAIN (B4-42 observer + FrozenPrefix,
  compress + router.go + server.go). Server-go queue (handleKeys
  counts, B4-17, export cap) waits for :939 to free server.go. B2-34
  waits for CATALOG-SYNC (MAIN-WIRE item 10).
- S1-939-REMAIN: DONE, commit 258cfad — engine half of B4-42
  follow-up: SetPrefixFreezeHooks + ObservePrefix/
  PrefixFreezeChecker/AnyCachingProvider exported; maybeCompress
  observes once per request (only when no stamp yet) + passes
  FrozenPrefix. PC build exit 0. NOT done: server handler half
  (applyCompression is compression.go, not server.go) + observer
  install (main.go) + QuantumLock (no implementation, not invented).
  Spawned Wave 9: S1-939-SERVER (compression.go), S1-939-INSTALL
  (main.go), S1-SERVER-QUEUE (server.go: handleKeys counts, B4-17,
  export cap).
- S1-939-INSTALL: DONE, commit 51b5fb6 — observer installed in
  openStack (NewPrefixFreeze + SetPrefixFreezeHooks). S1-939-SERVER:
  DONE, commit d2107b9 — applyCompression via ApplyWithOptions
  (FrozenPrefix + AnyCachingProvider + ObservePrefix once per
  request). :939 now wired end to end (engine + server + install);
  stays PARTIAL until test phase. Owed: stale maybeCompress doc in
  router.go ("server handler still calls plain compress.Apply" — now
  landed, needs a comment touch, router.go). QuantumLock still unbuilt.
- S1-SERVER-QUEUE: PARTIAL-DONE, commit fe49e02 — handleKeys device
  counts DONE (DeviceCount in keys feed); export cap NO CHANGE
  (dbExportMaxEvents=500 already built); B4-17 BLOCKED on guard.go.
  Router.go stale maybeCompress comment fixed directly (commit
  5e8c52c, no builder — 1-line doc touch). Spawned Wave 10:
  S1-B417-FOLLOWUP (guard.go + server.go + analytics_api.go: 3 guard
  rows + 3 register lines + ComboHealth handler).
- S1-B417-FOLLOWUP: DONE, commit 5ee599c — ComboHealth handler
  rebuilt (per-combo liveness from ComboStore.view + matching_
  requests upper bound) + 4 guard rows + 4 s.register lines (brief
  said 3 — ComboHealth needed a row+line too; the other 3 handlers
  were rebuilt by LEDGER2 22abdc4). Docs follow-up:
  TestAPIReferenceCoversEveryRoute will flag the 4 new routes until
  API_REFERENCE.md + llm.txt regenerate (test-phase docs work).
- S1-SERVER-QUEUE: PARTIAL-DONE, commit fe49e02 — (1) handleKeys
  device counts DONE: routerKeyView gains DeviceCount
  (json:"device_count", always present in the keys feed) =
  len(keyDevicesFor(k.ID)), same G15 tracker as /v1/me/status; PC
  build exit 0. Dashboard rendering of the per-row count is a
  web/index.html follow-up (not this lane). (2) B4-17 registration
  BLOCKED: the three handlers exist (analytics_api.go) but
  s.register PANICS on routes with no guardRouteTable row
  (guard.go:568) and the rows live in guard.go (outside this
  lane) — follow-up: 3 guard rows (TierManagement, ["admin"],
  per LOG.md:985 decision + /api/analytics/summary precedent)
  then 3 s.register lines in server.go; ComboHealth handler
  does not exist at all (rebuild in analytics_api.go). (3) export
  cap: NO CODE CHANGE — dbExportMaxEvents=500 already in
  dbexport.go (present at LEDGER2 time too; follow-up was stale).
- Stage 1 build-completeness audit (06:47 IST, read-only): VERDICT
  NOT DONE — 104/122 built; 10 genuinely unbuilt (B4-27/28/29/31/
  32/33/36/49/50/51 — verified absent by grep) + B4-44/B4-47
  zero-caller + B3-22 relay-translated remainder. Build phase
  ~2h40m (cap 2h) — parent call: run Wave 11 (one more real build
  wave), then Wave 12 (sequential, compress.go lane), then test
  phase. Spawned Wave 11: S1-COMPRESS-ENGINES (B4-27/28/29),
  S1-RELAY-RESP (B3-22 remainder), S1-TOKENS-SAVED (B4-51),
  S1-WEB-DEVICES (B3-02 remainder), S1-COMPRESS-SERVERWIRE
  (B4-21…26). Blocked/decision list: B2-09, B2-13, B2-25, B2-40,
  B2-26, B2-04, B3-19, B4-32, B4-36, B4-50, AK-3, B4-02 seed data.
  Caveats: S1-PII3 has no DONE note (rows verified built);
  modes.go header stale (M7.6).
- Wave 11 (spawned 06:48 IST, 5 builders, file-disjoint):
  S1-WEB-DEVICES DONE, commit 665ec9a — renderRouterKeys() now
  shows per-key device_count (" · N device(s)", singular-aware)
  on every key card from the feed (fe49e02); stale "NOT here yet"
  comment fixed; PC build exit 0, node --check clean. Row stays
  PARTIAL until test phase. S1-RELAY-RESP DONE, commit 7eaadec —
  handleMessagesTranslated now calls applyResponseGuardrails on
  the provider response before ToAnthropicResponse (block withholds
  403/-32001, redact rewrites in place; stream half untouched,
  covered by ResponseStreamRedactor); PC build exit 0, gofmt
  clean. In flight: COMPRESS-ENGINES (new tabular.go seen),
  TOKENS-SAVED, COMPRESS-SERVERWIRE.
- S1-COMPRESS-ENGINES DONE, commit cc3748d (5 files, +668/−3) —
  SmartCrusher (lossless columnar, pri 15) + Ionizer (row sampling,
  13) + Relevance (sentence scoring, 18), all in DefaultRegistry;
  shared tabular detector in tabular.go. Lossy gating automatic
  (Ionizer/Relevance only under lossy opt-in); no remainder — all
  3 engines built; PC build exit 0.
- Critic gate fix landed, commit e0de263 — gate.sh timestamp bug
  (literal $(...) in gate-last.txt) fixed; confirm on the next
  gate run. In flight: TOKENS-SAVED, COMPRESS-SERVERWIRE.
- S1-COMPRESS-SERVERWIRE DONE, commit e2b8805 (server.go only,
  +85/−2) — dedupStore (eager) + lazy CCR store (<data-dir>/ccr.db);
  handleChatCompletions runs applyCompressionPrePass before
  applyCompression (DeduplicateSession via X-Codvian-Session,
  ApplyCCR, Codex under lossy opt-in + X-Codvian-Codex-Max-Chars);
  PC build exit 0, gofmt clean. NOT built: RTK level HTTP field
  (needs compression.go/provider change — compression-lane
  follow-up); same pre-pass needed in a2a.go:697 + legacy_
  completions.go:372 (their lane builders). In flight: TOKENS-
  SAVED only.
- WAVE 11 DONE 5/5 (07:13 IST) — S1-TOKENS-SAVED, commit 7eca1fa
  (degrade.go + ledger.go, +120/−10): StaleEngine records proactive
  own-cache hits as no-charge ledger rows (exact=prompt+completion,
  semantic=prompt only; stale serves untouched); ledger NoCharge
  forces $0 cost so no spend is invented for a call that never
  happened; PC build exit 0. FOLLOW-UP: main.go must wire it
  (se.SetLedgerRecorder(asyncLed) next to SetSavingsRecorder) —
  hook is nil until then. NOTE: /root/.ssh/known_hosts was wiped
  by a VM restart — /home/hatch/.ssh/known_hosts works instead.
- Wave 12 spawned (07:14 IST, 3 builders, file-disjoint):
  S1-COMPRESS-WIRE (B4-33/44/47/38/31, compress lane),
  S1-EMBED-WIRE (B2-31 NeuralEmbedder client + TOKENS-SAVED main.go
  wiring, main.go + degrade.go), S1-SERVER-PREPASS (pre-pass in
  a2a.go + legacy_completions.go). Wave 13 (sequential, after
  Wave 12 frees the lanes): S1-STYLES (B4-49) + S1-RTK-FIELD
  (RTK level HTTP field, compress.go + server.go).
- S1-SERVER-PREPASS DONE, commit b58627b (a2a.go + legacy_
  completions.go, +6/−1) — applyCompressionPrePass before
  applyCompression in both paths, same shape as e2b8805; PC build
  exit 0, gofmt clean. In flight: COMPRESS-WIRE, EMBED-WIRE.
- S1-EMBED-WIRE DONE, commit dedfe63 (main.go only, +59/−5) —
  NeuralEmbedder closure routes one embeddings call through
  engine.Embed (text-embedding-3-small), one shared config for
  memory/Redis/SQLite tiers, fails open to BoW on any failure;
  B4-51 SetLedgerRecorder(asyncLed) wired next to
  SetSavingsRecorder (unconditional); PC build exit 0, gofmt
  clean; degrade.go needed no change. In flight: COMPRESS-WIRE
  only.
- WAVE 12 DONE 3/3 (07:28 IST) — S1-COMPRESS-WIRE, commit
  acce889 (5 files, +540/−29): PruneHeuristic wired into ultra
  mode + AgeMessages (recency window 4) into aggressive mode,
  HardBudget called by Apply when no freeze, SmartFilterText +
  truncation hints naming omitted char count; PC build exit 0,
  gofmt clean. B4-47 BLOCKED (honest): no genuine background
  consumer exists — PARITY row forbids request-path wiring;
  needs a server-lane background consumer (e.g. post-response
  session pre-compression) or re-scope; no code faked.
- Wave 13 spawned (07:29 IST): S1-STYLES (B4-49 output styles,
  compress lane). S1-RTK-FIELD (RTK level HTTP field, compress.go
  + server.go) queued after STYLES frees compress.go.
- S1-STYLES DONE, commit d870565 (compress.go + styles.go,
  +419/−12) — five B4-49 output styles (Terse, Less code,
  Ponytail, "I have ADHD", Terse CJK) registered in
  DefaultRegistry (priorities 19–23), all lossy, mutually
  exclusive (ApplyWithEngines only; plain Apply never runs them);
  PC build exit 0, gofmt clean. Note: /tmp/tsproxy.py vanished
  mid-task (VM restart) — restored from the durable copy.
- S1-RTK-FIELD spawned (07:38 IST, compress.go + server.go).
  Tree note: docs/API_REFERENCE.md, docs/llm.txt, web/tryit.html,
  tests/fixtures/v1/m8.21/routes.json + dashboard_m10_14_test.go
  are dirty from another lane (critic docs regen) — do NOT touch.
- WAVE 13 DONE 2/2 (07:47 IST) — S1-RTK-FIELD, commit dcce9f7
  (5 files, +85/−8): per-request RTK level via body field
  compression.rtk_level + X-Codvian-RTK-Level header (valid header
  wins; unknown body value 400s, unknown header ignored), honoured
  by Apply in mode=rtk and stacked paths via ApplyOptions.RTKLevel
  / effectiveRTKLevel; PC build exit 0. Needed 3 files beyond the
  brief's two (ApplyOptions in apply_options.go, CompressionRequest
  in provider/client.go, applyCompression in server/compression.go)
  — critic's in-flight files untouched.
- STAGE 1 BUILD PHASE COMPLETE (07:48 IST): all buildable rows
  built (audit 104/122 + Wave 11/12/13: B4-27/28/29, B4-31/33/38/
  44, B4-49, B4-51, B3-02 remainder, B3-22 remainder, B2-31
  follow-up, B4-21…26 wiring + RTK field). Remaining: blocked/
  decision items only (B2-09/13/25/40/26/04, B3-19, B4-32/36/47/
  50, AK-3, B4-02). TEST PHASE STARTED: S1-TEST-BASELINE spawned
  (smoke check + full-suite baseline on PC, read-only).
- S1-TEST-BASELINE DONE (08:15 IST, read-only): SMOKE PASS
  (server builds/starts/binds; /healthz 200; auth 401 without
  key; bootstrap 201; routed chat request with 0 providers →
  honest 502 upstream error; /v1/models 200 empty list). FULL
  SUITE RED: go test -count=1 ./... exit 1, 4.9 min on PC — 29
  ok / 17 FAIL / 4 no-test-files; 185 distinct failing tests.
  internal/router never ran (compile failure). Buckets: A A2A
  SQLite WAL (43×, file: URI backslashes on Windows —
  pre-existing); B Windows TempDir file locks (86×, 80
  cleanup-only — pre-existing); C files fsync on dir (37× —
  pre-existing); D CLI tests vs real PC config (27 — environment,
  not a bug); E guard-table drift (4 — known); F router compile
  (4 call sites — Stage 1-caused); G small packages. GOOD NEWS:
  TestAPIReferenceCoversEveryRoute + TestDashboardM10_14NoFake-
  Controls PASSED — critic's uncommitted regen verified working.
  Raw logs: /tmp/gotest-baseline.log (UTF-16LE) + .utf8.log.
- Fix wave T1 spawned (08:16 IST, 5 builders, file-disjoint):
  S1-FIX-ROUTER (4 call sites), S1-FIX-A2A-DSN (store.go DSN),
  S1-FIX-FSYNC (internal/files), S1-FIX-CLI (cmd+tray+config
  isolation), S1-FIX-GUARD (guard table 224/210 + OAuth scope/
  exempt). Wave T2 queued: WINLOCKS (TempDir closes — after
  GUARD frees server files), small packages (compress mode-table
  decision, gctune, mcp, health, crypto, config, modelsync,
  provider, strategy, release testdata), 5 compression-dedup
  server regressions (check lock-artifact vs real).
- S1-FIX-FSYNC DONE, commit 22461e1 (store.go only, +9) —
  fsyncDir skips the directory fsync on Windows (content + index
  file fsyncs untouched); PC pkgtest internal/files PASS 1.5s,
  toggle-checked (original FAILs all 8 with the exact reported
  error); VM vet + tests PASS, gofmt clean. In flight: ROUTER,
  A2A-DSN, CLI, GUARD.
- S1-FIX-A2A-DSN DONE, commit 89ed81f (store.go only, +11/−1) —
  DSN now a canonical file:/// URI (filepath.ToSlash + leading
  "/") instead of the malformed file://C:%5C... URI; 43 WAL errors
  gone, PC pkgtest internal/a2a 27/28 pass (only
  TestStorePermissions still fails — Windows chmod + TempDir lock,
  WINLOCKS lane's); VM test ok 14.3s, vet + gofmt clean. In
  flight: ROUTER, CLI, GUARD.
- S1-FIX-GUARD DONE, commit 91b5e46 (guard.go + guard_test.go,
  +31/−12) — GET /oauth/authorize carries admin scope; route list
  210→224 (+14: 4 DASHAUTH, 4 analytics, 6 OAuth), tiers
  9/25/176→9/29/186, exempt 56→58 with a named allowlist for POST
  /oauth/token + POST /oauth/revoke only (public by design — client
  auth is code_verifier/client secret; any other mutating exempt
  route still fails); all 3 targeted tests PASS on PC, gofmt +
  vet clean. Full server package still red on other lanes
  (TempDir locks, dedup). In flight: ROUTER, CLI.
- S1-FIX-ROUTER DONE, commit ad3b45d (3 test files, +4/−4) —
  the 4 chatStreamDrainChunks call sites now pass
  dispatchExplicitOrder=false; PC package compiles + runs 67.9s,
  the 4 fixed tests pass. BLOCKED — 1 test still failing:
  TestGroupSameModelTwoEfforts. Root cause PROVEN (builder trace):
  attempt 1 (low) rate_limited, attempt 2 (high) skipped_cooldown
  — :529F (e7d5cbe) cools ClientKey("a","") 2s after a hint-less
  429 and the hard-mode loop-top skip (router.go:4780) skips the
  same-key sibling; fails identically on Linux + PC (real Stage 1
  regression, not Windows). DECISION (parent): test-side fix, not
  a code bug — rate limits are per (provider, key), not per
  effort, so the mock's same-key "429 then success" is unrealistic;
  the test's point is group resolution, the 429 is only the vehicle.
  Single-key same-model effort failover after a hint-less 429 is
  intentionally skipped by :529F's per-key cooling (documented
  design consequence; revisit only on a design call). FIX spawned:
  S1-FIX-ROUTER-GROUP (distinct KeyIDs k1/k2 in the test +
  register the client under both ClientKeys).
- S1-FIX-WINLOCKS spawned (08:22 IST, T2) — TempDir closes +
  5 dedup regressions; files: internal/server (except critic's
  two test files) + ledger + degrade + cache. T2 queued: small
  packages (compress mode-table decision, gctune, mcp, health,
  crypto, config, modelsync, provider, strategy, release
  testdata).
- Critic 08:21 IST: audit-fake PASS (0 findings, 224 routes);
  parity unchanged. Fix commit STILL BLOCKED (7th run) — now
  ALSO because API_REFERENCE/try-it fixture went stale after
  T1 FIX-GUARD (91b5e46) changed the guard table. TEST-PHASE
  DOCS TASK (owed): re-run the API_REFERENCE regen (scripts/
  gen-api-docs.py) after FIX-GUARD, then the critic's fix can
  commit once internal/server is green. Top gaps this run:
  row 782 PII token vault, row 827 provider detail page,
  row 816 update flow (all MISSING, no code).
- WATCHDOG SWEEP MISFIRE (08:44 IST): the v2 watchdog's
  "dirty paths >12 → sweep" rule fired while 3 fix builders
  were LIVE and swept 28 dirty paths (CLI + WINLOCKS +
  ROUTER-GROUP in-flight files, critic's 7 files, cron
  bookkeeping) into wip commit cf34347. The watchdog cannot
  see the root's live agents (visibility gap) so it cannot
  tell live in-flight files from dead agents' orphans.
  REPAIR (root, 08:47 IST): watchdog cron DISABLED (enabled:
  false — re-enable after Stage 1 test phase, when the fleet
  is quiet); sweep UNDONE via git reset HEAD~1 under the git
  lock (HEAD back to 7d295f8, all 28 paths dirty again,
  nothing lost); all 3 builders notified. Stall-watch (4-min,
  detect-only) + reporter + critic + dead-builder-recovery
  keep running. LESSON: the watchdog sweep must never run
  while live builders hold dirty files — its liveness check
  is blind; the dead-builder-recovery cron is the only safe
  harvest path (it checks liveness properly).
- S1-FIX-WINLOCKS DONE, commit 6213e6f (5 files, +59/−1) —
  Server.Close() closes the lazy ccr.db store; newTestServer
  registers t.Cleanup(srv.Close); fixture led.Close()
  cleanups; ApplyCCR + DeduplicateSession leave content
  stored/marked earlier in the SAME call unchanged (fixes
  the real dedup regression: e2b8805 pre-pass markered the
  2nd identical system message, dedupeSystem no longer
  matched, provider saw 3 messages want 2). PC targeted:
  7 compression tests ALL PASS (3 real dedup + 4 lock-only),
  2/3 cache tests PASS, lock samples (key import/rotate,
  groups feed, M10_7) ALL PASS. VM vet + gofmt clean;
  internal/compress still exactly its 4 pre-existing
  failures. Scope note: compress.go was outside the brief
  but the dedup fix had to live there; no collision.
  REMAINING (needs a design decision, not fixed):
  TestChatCompletionsLossyExactHitStillWorks — PRE-EXISTING
  (fails identically on the pristine tree, stash-checked):
  CCR pre-pass is stateful, so two identical requests get
  different post-pre-pass messages and the exact-cache key
  misses — fix needs a cache-keying vs CCR-markering call.
  Full internal/server re-run NOT done (SSH disconnect kills
  the child go test) — belongs to the test-phase re-baseline;
  LESSON: long PC runs must use nohup/setsid (detached) so a
  dropped SSH does not kill the test. In flight: CLI,
  ROUTER-GROUP.
- S1-FIX-CLI DONE, commit 06f0266 (12 files, +216/−57) —
  isolateUserDirs (HOME+USERPROFILE+AppData+XDG_CONFIG_HOME)
  isolates all 22 config-dependent cmd tests from Raunak's
  real PC config; Windows fixes (perms 0600/0755 guards,
  ENOTDIR→not-exist wording, fake-release zip, sh→test-
  binary, tray path separators, timeout→ping sleep); plus
  TWO REAL tray bugs found while verifying — PID file written
  before the running state is published (race), rename
  retried 5× (Defender transient hold). PC: tray ok 39.8s,
  config ok 2.6s, cmd 30/31 — only
  TestRegistryMCPSourceLazyLedgerStickyError fails (ledger
  TempDir lock). VM Linux: all three packages ok.
  CORRECTION: WINLOCKS's "phantom" claim about that test was
  WRONG — it exists since 035d1ed (M9 wave), verified by grep
  at dcce9f7; the 11th ledger.db lock in the baseline log was
  real. FIX spawned: S1-FIX-WINLOCKS2 (cmd test ledger close
  + investigate a2a TestStorePermissions). In flight:
  ROUTER-GROUP.
- S1-FIX-ROUTER-GROUP DONE, commit c7fabae (groups_test.go
  only, +7/−2) — TestGroupSameModelTwoEfforts now uses
  distinct keys k1/k2 (client registered under both
  ClientKeys), with a comment recording why; production code
  untouched, mock untouched. PC: FULL internal/router package
  PASS 67.7s, 0 FAILs — both the group test and the stream
  variant pass. T1 COMPLETE 5/5 (FSYNC, A2A-DSN, GUARD,
  ROUTER-compile, CLI) + ROUTER-GROUP + WINLOCKS. LESSON
  (SSH): silent long SSH commands drop at ~36s (tunnel idle
  timeout) — use `go test -v` streaming + ServerAliveInterval;
  PC .bat files need CRLF line endings. In flight: WINLOCKS2
  only. Next: small-packages T2 + full-suite re-baseline
  (detached, nohup/setsid) + docs regen after FIX-GUARD.
- Critic 08:51 IST: audit-fake PASS (0 findings, 224
  routes); parity unchanged; last 3 flips name tests ✓.
  Fix commit STILL BLOCKED (8th run) but the blocker SHRANK
  11→7 — WINLOCKS 6213e6f cleared 4/5 compression dedup
  regressions. Remaining 7 (all other lanes'): 3 docs/tryit
  (one cause: /oauth/authorize scope from FIX-GUARD — the
  regen-after-FIX-GUARD task covers it), M10_0, M10_10,
  scope login 401, LossyExactHit (design call). Top gaps
  unchanged: 782 PII vault, 827 provider detail, 816 update
  flow. Rows 669/678/783 read MISSING but code is built
  (dashboard auth, OAuth/JWT, request rewrite) — need V.
- S1-FIX-WINLOCKS2 DONE, commit a7ae328 (2 test files only,
  +25) — fresh source's cached ledger handle closed via
  t.Cleanup (StickyError); TestStorePermissions: defer
  s.Close() (first handle had no defer, a Fatalf left
  tasks.db open) + exact 0700/0600 assertions skipped on
  Windows (unrepresentable: Go reports 0777/0666, chmod only
  toggles read-only). PC: cmd 31/31, a2a 28/28 (both full
  packages green); VM both ok, gofmt + vet clean. ALL fix
  builders DONE (T1 5/5 + ROUTER-GROUP + WINLOCKS +
  WINLOCKS2).
- T2-small wave spawned (09:00 IST, 5 builders, file-
  disjoint): S1-FIX-SMALL-COMPRESS (mode-table decision:
  update tests to the deliberate COMPRESS-WIRE table),
  S1-FIX-SMALL-PLUMB (gctune/mcp/health/crypto/config/
  modelsync/provider/strategy), S1-FIX-SMALL-RELEASE
  (testdata refresh), S1-FIX-SERVER-SMALL (M10_0, M10_10,
  scope login 401 — NOT LossyExactHit, design call),
  S1-DOCS-REGEN (gen-api-docs.py after FIX-GUARD, 4 files
  only — NOT audit-fake.sh or guardrails_feed_types_test.go).
  Next after these: full-suite re-baseline (detached) +
  race + benchmark + flips + Stage 1 push.
- S1-DOCS-REGEN DONE, commit 7063cb2 (4 files only,
  +570/−4) — regen after FIX-GUARD: 224 routes, /oauth/
  authorize admin scope + analytics routes in; VM test
  TestAPIReferenceCoversEveryRoute PASS 0.025s. Critic's
  fix is now just 3 files (audit-fake.sh +
  guardrails_feed_types_test.go + dashboard_m10_14_test.go)
  — its commit unblocks once internal/server is green.
  In flight: COMPRESS, PLUMB, RELEASE, SERVER-SMALL.
- S1-FIX-SMALL-COMPRESS DONE, commit fd1e079 (2 test
  files only, +26/−11) — mode-table expectations updated
  for Wave 12 (aging/pruning live), G22 cap fixture lines
  made distinct, single-line check accepts the B4-38 count
  hint; PC full internal/compress PASS 1.78s, all 4 fixed
  tests PASS; VM also PASS, gofmt + vet clean. All 4
  failures traced to deliberate COMPRESS-WIRE acce889
  changes, no separate real issue. Observation (not
  changed, out of scope): cap step records
  StepToolResultCap when only SmartFilterText changed the
  text (filter-only, no truncation) — minor semantic drift
  in step recording. In flight: PLUMB, RELEASE,
  SERVER-SMALL.
- S1-FIX-SERVER-SMALL DONE, commit 2a94dfc (3 files,
  +29/−1) — M10_0: auth-headers sweep carves out
  login/change/logout (HttpOnly session-cookie auth, no
  Bearer token can exist); setup stays covered via
  adminHeaders(). M10_10: cache tier-note copy now says
  "a band the dashboard estimates, not a measured saving"
  (production copy fix, web/index.html 1 line — justified
  expansion). Scope login 401: carve-out expects the
  handler's own 401 "wrong dashboard password" (empty
  body), not a scope-gate failure. PC: all 3 targeted
  tests PASS 0.44s; VM vet + gofmt clean. Still open:
  LossyExactHit (design call). In flight: PLUMB, RELEASE.
- S1-FIX-SMALL-RELEASE DONE, commit 7b4fb5c (1 test file
  only, +25/−6) — CORRECTION to the baseline diagnosis:
  root cause was NOT testdata staleness (the "11 FAIL"
  lines are expected output of passing tamper/unsigned
  tests); the test's #!/bin/sh stubs can't run on
  Windows + a hardcoded `:` PATH separator. Fix: .bat
  stubs on Windows (CRLF, redirection-first), os.
  PathListSeparator, CRLF-normalized log comparison. PC
  full internal/release PASS 2.39s, VM also PASS, gofmt +
  vet clean; toggle-proven (original FAILs on PC,
  fixed PASSes). Verifier + testdata untouched — no
  checksums faked, no verifier weakened. In flight: PLUMB
  only.
- S1-FIX-SMALL-PLUMB DONE, commit 5961912 (11 files,
  +143/−17) — REAL production fixes: gctune Windows RAM
  detection via GlobalMemoryStatusEx (source "host-phys")
  — detection returned 0 on Windows, so no limit was ever
  set and low-end never armed; mcp memory store
  Search/Clear now error on unusable paths instead of
  silently returning empty (Windows reports a file-in-path
  as ENOENT, same as "file absent"). Test-side: exact
  0600/0700 perms assertions guarded off Windows (11
  assertions, 6 files) + platform-aware errno text. PC:
  all 8 packages PASS, all 13 target tests PASS; VM all 8
  pass, gofmt + vet clean, Windows cross-compile OK.
  Config's TestSaveLoadRoundTrip was already fixed by
  FIX-CLI (06f0266) — confirmed passing, no change.
  T2-small wave COMPLETE 5/5.
- S1-TEST-REBASELINE spawned (09:13 IST, read-only):
  smoke check + full suite verbose on the PC, detached
  (nohup/setsid), log /tmp/gotest-rebaseline.log. Only
  known remaining failure: LossyExactHit (design call).
  Next after it: race check once, benchmark once, flips
  (truth protocol), Stage 1 push.
- Critic 09:32 IST: audit-fake PASS (0 findings, 224
  routes); parity unchanged; last 3 flips name tests ✓.
  FIX COMMITTED 6f53a33 (3 files, +128/−8) — the 8-run
  blocker is CLEARED: audit-fake per-type-toggle fix
  (exempt data-pattern switches wired via #grTypes
  delegation + feed-types toggle test). Proof: script
  toggle HEAD 1 finding/FAIL vs fixed 0/PASS; test
  mutation FAILS, restored byte-identical PASSES 0.01s;
  targeted probe 12/13 PASS; full pkgtest internal/
  server 310s with exactly 1 failure — LossyExactHit,
  the known design-call failure. Root-verified: audit-
  fake.sh re-run by root → PASS, 0 findings, 224 routes.
  The tree is now CLEAN except cron bookkeeping
  (HEARTBEAT/LOG/PLAN) — no in-flight files left. Top
  gaps unchanged: 782 PII vault, 827 provider detail,
  816 update flow. Re-baseline still running.
- VM RESTART ~09:35 IST (reporter 09:44): VM uptime 9
  min, /tmp wiped (gate JSON evidence lost — the separate-
  JSON-per-mode fix is still owed). /tmp/tsproxy.py is
  gone again; the re-baseline agent knows the restore
  protocol (durable copy in hidden_files/tsproxy.py).
  Re-baseline survived (live, active 14s ago, ~32 min
  into its 60-min box).
- S1-TEST-REBASELINE DONE (~09:48 IST): smoke PASS
  (build OK, /healthz 200, no-key 401, bootstrap 201,
  routed → honest 502, /v1/models 200 empty). Packages:
  45 ok / 2 FAIL (mcptools, server) / 4 no-test-files,
  51 total (baseline: 29 ok / 17 FAIL). Passing: 4,254
  `--- PASS` lines. Failing: 74 distinct (baseline 185)
  — ALL 74 also failed at baseline, 0 new; 38 cleanup-
  only, 36 real. No panics, no data races. Root-verified
  from the log: 4,254 PASS / 74 FAIL lines; 45 ok / 2
  FAIL packages (mcptools, server); log saved durably
  at hidden_files/gotest-rebaseline-2026-11-20.log.
  Remaining, grouped: (1) A2A-server tasks.db lock 40 —
  37 cleanup-only + 3 prune tests hit the OLD malformed
  `file:` URI in test helper backdateA2ATaskUpdatedAt
  (a2a_prune_test.go:64), the one site FIX-A2A-DSN
  missed; (2) Batches store dir-fsync 16 —
  batches.go:403 d.Sync(), second site of the FSYNC fix;
  (3) mcptools stdio EOF 9 — `python3` on the PC is a
  dead Windows Store stub (real Python is C:\Python314\
  python.exe); (4) Windows clock granularity 4 —
  BudgetWallClock 1ns, AutoRoute latency 0µs, 2×
  ResponseHeaders latency header absent (header only set
  when latency>0; time.Since returns 0 over short busy
  spans on this PC; passes on VM Linux); (5) Memstats
  Linux-only RSS 2 — readRSSKiB is /proc-only; (6)
  Idempotency db lock cleanup-only 1; (7) Pricing config
  rename Access-denied 1 — concurrent Saves onto same
  config.json; (8) LossyExactHit 1 (design call).
  Buckets FULLY CLEARED since baseline: A (a2a 28/28),
  C-files, D CLI/config, E guard drift, F router compile,
  G small packages (all PASS). LESSON (AGENTS.md): PC
  detached runs must use WMI Win32_Process.Create
  (Session 0) — Start-Process children die on SSH
  disconnect; long-run logs to a durable path, never
  /tmp.
- T3 fix wave spawned (09:49 IST, 4 builders, file-
  disjoint): T3-A2A-PRUNE (a2a_prune_test.go only),
  T3-BATCHES-FSYNC (batches.go only), T3-MCPTOOLS-PYTHON
  (internal/mcptools/*), T3-SERVER-REST (rest of
  internal/server: clock 4 + memstats 2 + idempotency 1
  + pricing 1 — NOT LossyExactHit, design call). Next
  after T3: race check once, benchmark once, flips
  (truth protocol), Stage 1 push.
- S1-T3-A2A-PRUNE DONE, commit 8914cc7 (1 file, +10/−1)
  — backdateA2ATaskUpdatedAt DSN now canonical file:///
  URI (mirrors a2a.OpenWithLimits); PC: TestA2APruneLoop
  PrunesOldTerminalTask PASS 0.04s; toggle-proven
  (fix-off: all 3 failed `busy timeout: out of memory
  (1)` at the backdate call sites). CORRECTION to the
  re-baseline premise: only 3 of the 40 hit the helper.
  The other ~36 are cleanup-only tasks.db locks with a
  DIFFERENT cause — the A2A store opened by a2aService()
  is never closed: Server.Close() (server.go:4656)
  closes only the CCR store, and newA2ATestServer
  (a2a_test.go:43) registers no cleanup. FOLLOW-UP sent
  to T3-SERVER-REST (in its file scope): T3-A2A-CLOSE —
  Server.Close() must close the A2A store (+ stop prune
  loop) and newA2ATestServer must register t.Cleanup
  (srv.Close) (WINLOCKS pattern). In flight: BATCHES-
  FSYNC, MCPTOOLS-PYTHON, SERVER-REST (+ A2A-CLOSE).
- S1-T3-BATCHES-FSYNC DONE, commit b33c18f (1 file,
  +11) — saveLocked() in internal/server/batches.go
  skips the directory fsync on Windows (same FIX-FSYNC
  pattern; index-file fsync untouched). PC: all 16
  targeted batches tests PASS (21 PASS lines, 0 FAIL),
  ok internal/server 1.844s; toggle-proven (same 16
  failed in the re-baseline log with "sync … Access is
  denied"). VM gofmt + vet clean. Detached via WMI
  Win32_Process.Create; log at hidden_files/
  t3-batches-2026-11-20.log. In flight: MCPTOOLS-PYTHON,
  SERVER-REST (+ A2A-CLOSE).
- S1-T3-MCPTOOLS-PYTHON DONE, commit 8b16721 (2 test
  files only, +59/−30) — new pythonInterpreter(t)
  helper: $PYTHON, then python3/python/py, each verified
  by running --version and requiring exit 0 — the
  Windows Store stub for python3 passes LookPath but
  exits 9009, which is why every stdio spawn died with
  `initialize: EOF`. Both mock-writer helpers now return
  the verified interpreter; all 9 spawn sites use it.
  Test-only, no assertions weakened. PC: all 9
  previously-failing stdio tests PASS (package 41 PASS /
  1 FAIL); VM full package also PASS; toggle-proven
  (same 9 failed with `initialize: EOF` in the 09:48
  re-baseline). The 1 remaining PC failure is
  TestBudgetWallClockRefusesAfterExpiry — the clock-
  granularity test, which lives in internal/mcptools/
  controls_test.go:196 (NOT internal/server). FOLLOW-UP
  2 sent to T3-SERVER-REST: its scope is extended to
  that one test file (+ the production file if a minimal
  fix needs it). Cosmetic: the commit message lost the
  literal $PYTHON (shell expanded it) — code is fine.
  In flight: SERVER-REST only (+ A2A-CLOSE + clock
  test).
- Critic 10:00 IST: audit-fake PASS (0 findings, 224
  routes). NOTE: its parity-by-grep numbers (FULL 247 /
  PARTIAL 182 / MISSING 199 / WRONG 1, total 629) are a
  MISCOUNT — parity.sh (MATCH) still stands at FULL 249
  / PARTIAL 185 / MISSING 216 / WRONG 2 (652); do not
  quote the critic's grep numbers. LossyExactHit fix
  ATTEMPTED and PROVEN UNFIXABLE in one run — guard
  reverted, nothing committed. Diagnosis (probe, since
  deleted): identical lossy repeat sends a DIFFERENT
  prompt on call 2 because CCR rewrites depend on store
  warmth — call 1 (cold): provider saw 2 msgs [16,12];
  call 2 (warm): [30,12], the 16-char system message
  replaced by the 30-char [ccr:ref] marker, so the exact
  key differs → cache miss. Attempted guard ("never
  marker content shorter than the 30-char marker")
  passed its unit test but was INSUFFICIENT — re-probe:
  call 2 then sends 3 msgs [16,30,12] (the markered
  assistant turn now FITS the 40-token lossy budget,
  while call 1 dropped the full ~1000-char turn). Root
  cause: the lossy trim counts post-CCR token lengths,
  so its keep/drop decisions depend on CCR warmth.
  Candidate design fixes (PLAN.md, need a decision —
  do NOT retry without one): (A) key the exact cache on
  the pre-compression request (raw messages + compression
  params) — touches degrade scopedKey + server/engine
  plumbing; (B) make the lossy trim count markers at
  their stored-content length (expand via CCR store for
  counting only) — touches compress.Apply plumbing +
  server applyCompression opts. The reverted guard is a
  genuine micro-fix (a marker must never expand the
  prompt) and can be re-applied as part of whichever
  option is chosen.
- S1-T3-SERVER-REST DONE, commit e7be656 (7 files,
  +90/−20) — clock group (4): headers.go always emits
  X-Codvian-Latency-Ms when timing exists (0 ms is a real
  truncated measurement, not unknown); AutoRouteClassify
  accepts 0µs (sub-microsecond work on a coarse Windows
  clock); memstats RSS assertions skipped on Windows
  (/proc-only by design); BudgetWallClock gets a 2 ms
  sleep after building the 1ns-budget registry so the
  clock advances past the budget. A2A-CLOSE: Server.
  Close() now stops the prune loop (waits for exit) +
  closes the task store; cleanup registered in
  newA2ATestServer AND a2aAdmissionSetup.server — the 7
  stragglers used the latter (bare New(), no cleanup).
  Idempotency: the test now releases the other-key
  Proceed begin's store (it was leaked). PC: server ok
  5.5s + mcptools ok 1.8s — 81 PASS / 0 FAIL (all 13
  named tests pass, incl. the 7 A2A cleanup locks); VM
  gofmt + vet clean. NOT fixed (routed onward):
  TestPricingCloneSaveRace — config.Save's rename fails
  Access-denied on Windows; temp file IS closed before
  rename, no in-process watcher holds config.json, most
  likely a Defender transient hold (same pattern FIX-CLI
  worked around with a 5× rename retry on the tray PID
  file); fix belongs in internal/config/config.go (retry
  os.Rename). LossyExactHit untouched (design call).
  T3 wave COMPLETE 4/4.
- S1-T3-PRICING spawned (10:10 IST): config.Save
  rename retry (internal/config/config.go only), verify
  TestPricingCloneSaveRace 3× on the PC. After it: 2nd
  full-suite re-baseline (detached), then race check
  once, benchmark once, flips, Stage 1 push. Remaining
  known failures after T3: LossyExactHit (design call)
  + PricingCloneSaveRace (in flight).
- S1-T3-PRICING DONE, commit 003bb98 (1 file, +19/−1)
  — config.Save rename now retries 5× with 50 ms
  backoff via new renameFile helper (mirrors tray's
  renamePIDFile); cause verified in the re-baseline log
  — `rename ...\.config-*.tmp ...\config.json: Access
  is denied` (Defender transient hold). PC:
  TestPricingCloneSaveRace 3/3 consecutive PASS
  (2.03s/2.04s/2.04s), ok internal/server 6.456s; config
  package also PASS 2.937s; toggle-proven (same test
  failed on unfixed code in the 09:48 re-baseline with
  the exact rename error). VM gofmt + vet clean. Log at
  hidden_files/t3-pricing-2026-11-20.log. LossyExactHit
  untouched (design call).
- S1-TEST-REBASELINE2 spawned (10:14 IST, read-only):
  smoke check + full suite verbose on the PC, detached
  (WMI Win32_Process.Create), log saved durably at
  hidden_files/gotest-rebaseline2-2026-11-20.log. Only
  known remaining failure: LossyExactHit (design call).
  Next after it: race check once, benchmark once, flips
  (truth protocol), Stage 1 push.
- S1-TEST-REBASELINE2 DONE (~10:32 IST): smoke PASS
  (build OK, /healthz 200, no-key 401, bootstrap 201,
  routed → honest 502 with X-Codvian-Model: gpt-4o-mini,
  /v1/models 200 empty). Packages: 45 ok / 2 FAIL
  (router, server) / 4 no-test-files, 51 total.
  Passing: 4,326 `--- PASS` lines (09:48: 4,254).
  Failing: 3 distinct (09:48: 74) — 71 FIXED. No
  panics, no data races. Suite exit 1, ~5 min wall
  (server pkg 133.3s, was 243.9s; router 79.2s).
  Root-verified from the log: 4,326 PASS / 3 FAIL
  lines; FAIL packages router + server; 45 ok / 4
  no-test-files. Remaining: (1) LossyExactHit (design
  call, expected); (2) TestDashboardM10_11HealthEquals
  Feeds — memstats RSS assertion, 2nd Windows site
  SERVER-REST missed (dashboard_m10_11_test.go:344,
  "want a live RSS on Linux"; same skip pattern as the
  memstats fix); (3) TestBreakerStaleDegradedPersists
  AcrossReload — NEW but FLAKY, not a T3 regression
  (passed at 09:48, failed once here, passes 5/5 on
  immediate re-run; no T3 commit touched internal/
  router) — cause is the dropped os.Rename error in
  saveBreakerStore (internal/router/breaker_store.go,
  same Defender-transient-hold class T3-PRICING fixed
  for config.Save) — needs the same rename retry, not
  a test change. Smoke note: first keyed-chat attempt
  returned a false 400 from PowerShell mangling quoted
  JSON to curl (same artifact the baseline agent hit) —
  fixed by passing the body via file, NOT a router bug.
- T4 fix wave spawned (10:33 IST, 2 builders, file-
  disjoint): T4-M10-11 (dashboard_m10_11_test.go only),
  T4-BREAKER-RENAME (breaker_store.go only). After T4:
  race check once, benchmark once, flips, Stage 1 push.
  Only known remaining failure after T4: LossyExactHit
  (design call).
- S1-T4-M10-11 DONE, commit faf0b86 (1 file, +12/−2)
  — RSS assertion in TestDashboardM10_11HealthEquals
  Feeds now skipped on Windows (readRSSKiB is /proc-
  only by design, feed honestly reports RSS
  unavailable there; same runtime.GOOS != "windows"
  pattern as TestDashboardMemstatsLive). Cause verified
  from the re-baseline2 log first: `memstats rss = 0
  available=false`. PC: TestDashboardM10_11HealthEquals
  Feeds PASS (0.00s), ok internal/server 0.330s;
  toggle-proven (fix-off FAILs in the re-baseline2 log
  on the same PC, fix-on PASSes). VM test also PASS,
  gofmt + vet clean. T4-BREAKER-RENAME still in flight
  (breaker_store.go dirty).
- S1-T4-BREAKER-RENAME DONE, commit 8e04c23 (1 file,
  +22/−1) — saveBreakerStore's dropped os.Rename now
  retries 5× with 50 ms backoff via a renameBreaker
  StoreFile helper (mirrors config.Save's renameFile);
  one transient Defender hold was losing a failure save,
  so the reload saw 4 failures instead of 5. PC:
  TestBreakerStaleDegradedPersistsAcrossReload 5/5
  PASS; full internal/router package ok (68.3s). VM
  gofmt + vet clean. LossyExactHit untouched (design
  call). T4 wave COMPLETE 2/2.
- S1-TEST-RACE spawned (10:40 IST, read-only):
  `go test -count=1 -race ./...` verbose on the PC,
  detached (WMI Win32_Process.Create), log saved
  durably at hidden_files/gotest-race-2026-11-20.log.
  Only known remaining failure: LossyExactHit (design
  call). Next after it: benchmark once, flips (truth
  protocol), Stage 1 push.
- S1-TEST-RACE DONE (~10:58 IST, ~14 min wall): 45 ok
  / 2 FAIL (server, webhooks) / 4 no-test-files, 51
  total. Passing: 4,333 `--- PASS` lines (re-baseline2:
  4,326). Failing: 2 distinct (was 3) — M10_11 and
  Breaker now PASS. Root-verified from the log: 4,333
  PASS / 2 FAIL; FAIL packages server + webhooks; 45
  ok; 1 DATA RACE warning (line 14534). Remaining:
  (1) LossyExactHit (design call, expected); (2) 1
  DATA RACE in internal/webhooks, TEST-SIDE: noRetry
  Backoff (webhooks_test.go:113) writes the package-
  level webhookRetryBackoff while a leaked deliver
  goroutine from TestDispatcherDownEndpointDoesNotBlock
  reads it at webhooks.go:170 — that test's Notify
  returns immediately by design and never waits for
  delivery, so its retry goroutine outlives the test
  and races the next test's backoff swap. Production
  never reassigns the var, so no production race; fix
  belongs in the test (wait for delivery / per-
  Dispatcher backoff). It fails TestDispatcherRetries
  5xxThenSucceeds only because the detector fired
  during it — the test passed in the non-race
  re-baseline2. LOCATION CAVEAT: the race run ran on
  the VM, NOT the PC — the PC has no C compiler
  (CGO_ENABLED=0; no gcc, MSYS2, MinGW, TDM,
  Strawberry, or LLVM found), so `go test -race`
  cannot run there (`go: -race requires cgo`, exit 2
  — not a code failure). Installing a toolchain would
  reconfigure the PC, which the working mode forbids.
  VM has gcc 13.3.0, CGO_ENABLED=1 — race checks run
  on the VM.
- T5 fix spawned (10:59 IST): T5-WEBHOOKS-RACE
  (internal/webhooks/webhooks_test.go, test-side fix;
  verify with `go test -race` on the VM, 5×). After
  it: benchmark once, flips (truth protocol), Stage 1
  push. Only known remaining failure after T5:
  LossyExactHit (design call).
- S1-T5-WEBHOOKS-RACE DONE, commit 01dc8f9 (1 file,
  +33/−1, test-only) — TestDispatcherDownEndpointDoes
  NotBlock now zeroes the backoff and waits for the
  delivery goroutine's terminal "delivery failed after"
  log line before returning, so no goroutine outlives
  the test to race the next test's backoff swap; no
  assertion weakened, no production change. VM test:
  `go test -race -count=1 ./internal/webhooks/` 5/5
  PASS (fix-off reproduced the race first); plain
  package tests also PASS, gofmt + vet clean.
  LossyExactHit untouched (design call).
- S1-TEST-BENCH spawned (11:02 IST, read-only):
  benchmark once on the PC (Q2, same method/targets
  as Q1: added latency 1.775/1.778 ms MISSED, RPS
  588.53/508.87 MISSED, idle RSS 49.0 MiB MET),
  quiet window, raw record saved durably at
  hidden_files/benchmark-2026-11-20-q2.json. After
  it: flips (truth protocol), Stage 1 push. Only
  known remaining failure: LossyExactHit (design
  call).
- S1-TEST-BENCH DONE (~11:06 IST, ~116 s, exit 0,
  clean run, quiet window, loadavg start 0.18, all 8
  polluted flags false, limiter ON, HEAD 0f320ed):
  added latency 1.693/1.683 ms (Q1 1.775/1.778 —
  ~5% better, still MISSED target <0.5 ms); RPS
  579.8/524.5 (Q1 588.5/508.9 — flat, still MISSED
  target >3,000; http-502 errors rose 119→161 /
  71→171); idle RSS 51.0 MiB (Q1 49.0 — slightly
  worse, MET target <70 MiB; peak RSS 88.7/108.4
  MiB vs Q1 83.7/102.0); ready 1016.1/1129.8 ms
  (Q1 187.8/316.2 — ~5× worse, still MET target
  <10 s). LOCATION: ran on the VM, NOT the PC —
  the rig (bench.sh/bench-geekflare.sh) is Linux-
  only (/proc, flock, pgrep, nproc) and Q1 ran on
  this same VM, so Q2 here keeps the numbers
  comparable; PC has no native bash, WSL Ubuntu-
  22.04 exists (stopped) but would be a third,
  uncalibrated rig needing Go + repo inside WSL
  (PC reconfiguration, forbidden). Raw record:
  docs/benchmark-2026-11-20-q2.json (committed) +
  durable copy at hidden_files/benchmark-2026-11-
  20-q2.json; run log hidden_files/bench-q2-run-
  2026-11-20.log. No BENCHMARK.md Q2 narrative
  written yet (separate docs step). Next: flips
  (truth protocol), Stage 1 push. Only known
  remaining failure: LossyExactHit (design call).
- S1-VERIFY-QUEUE spawned (11:07 IST): one agent
  builds docs/STAGE1-VERIFY-QUEUE.md — per built
  Stage 1 row: PARITY row number + name (verified
  by grep), current status, feature, builder
  commit(s), files, covering test names (or NO
  TEST / UNCLEAR, never guessed). Blocked/decision
  rows + LossyExactHit listed once at the end as
  NOT VERIFIABLE. Verifiers then work in batches
  of 10 from that queue, one feature per verifier,
  flips only via the truth protocol. After flips:
  Stage 1 push (browser PR → merge).
- S1-VERIFY-QUEUE DONE (~11:20 IST): 101 rows
  queued, 18 NO TEST, 3 UNCLEAR, 15 blocked/
  decision, 1 already FULL — docs/STAGE1-VERIFY-
  QUEUE.md (782 lines, committed 92725fb).
  Arithmetic checks: 101 + 15 + 1 + 5 batch-1
  FULL = 122. 18 NO TEST rows need a test written
  first (B2-14 Arena half, B2-31, B3-04, B3-07,
  B3-20, B3-25, B4-01, B4-13, B4-23, B4-24, B4-26,
  B4-27, B4-28, B4-29, B4-31, B4-33, B4-49, B4-51).
  3 UNCLEAR (B2-16, B4-20, B4-21). Notable find:
  B2-13's PARITY test name is stale — actual test
  is TestSixteenModePackTablesRescaledForLiveFactors.
- VERIFY BATCH 1 spawned (11:21 IST, 9 verifiers,
  grouped by shared files, 21 rows): V-B201 (:489),
  V-B202 (:495), V-B203 (:497), V-B205-GROUP
  (:501/:510/:511/:512/:527 incl. B2-16 UNCLEAR),
  V-B206 (:503), V-B207 (:504), V-B208 (:508),
  V-B215 (:525), V-B217-GROUP (:534/:535/:536/:538/
  :539/:542/:543/:544). Verifiers do the full truth
  protocol (code in path, cold test run on the PC,
  toggle-check under the VM flock /tmp/codvian.
  verify.lock, 20-min timeout) and report the
  verdict + exact PARITY.md row text — NO PARITY.md
  edits, NO commits; root applies flips serially
  from the reports. After batch 1: next batch of
  10 from the queue, then the 18 NO-TEST rows need
  testers, then flips, then the Stage 1 push.
- V-B208 DONE (~11:27 IST) — verdict STAYS PARTIAL
  (DRR core verified; P2C/at-cap/tree/config-wiring
  implemented but untested in the engine path).
  Code in path: DRR orderQuotaShare (cascade.go:
  218) + at-cap deprioritisation (fail-open) + P2C
  top-two swap via live ConcurrencyStats in-flight
  counts; tree intercept rankAttemptsWithContext
  (tree.go:258) + quota-share in validStrategies
  (tree.go:96); QuotaShareFor wired from config
  quota_shares (main.go:3485). Cold on PC (hash-
  verified vs HEAD b7a2545): 3 tests PASS (0.392s).
  Toggle-check (under flock, restored, PC hash =
  HEAD): with orderQuotaShare disabled, both engine
  tests FAIL; pure-helper test PASSes as expected.
  Gap: no engine-path test covers at-cap gating,
  P2C, tree intercept, or config→engine wiring —
  so no FULL flip. Row text applied by root +
  committed 7371d74 (PARITY.md:508 stays PARTIAL,
  strengthened; parity.sh MATCH, counts unchanged).
  8 verifiers still running.
- V-B207 DONE (~11:28 IST) — verdict FLIP TO FULL
  (:504 cache-optimized). Live path: main.go:3097
  wires HookFactoryWithCacheAffinity (1-min ledger
  CachedTokenFraction snapshot); router.go:2672
  invokes StrategyHookForRequest per request;
  wiring.go:731-791 StrategyCacheOptimized feeds
  OrderByCacheAffinity (cache_affinity.go:276)
  with measured warmth + cacheability gate. The
  row's blocker ("warmth is not a hook input yet")
  is now FALSE. Cold on PC: TestM65CacheOptimized*
  (6), TestCacheAffinity* (9), TestConformance…
  ALL PASS. Toggle-check (flock-held): warmth
  disabled → TestM65CacheOptimizedReordersOnWarmth
  FAILS (got "abc", want "cab") + WarmthBeatsMarker
  Support FAILS. Restored clean both trees. Row-text
  corrections: named test no longer covers cache-
  optimized (left that list in M6.5-wiring);
  affinity.go:237 stale; covering tests are the
  M6.5 family. Row :752 already FULL with the same
  evidence — consistent. Root ran the 3 TestCache
  WarmthSnapshot* tests cold on the VM (0.013s PASS)
  since the verifier's summary didn't list them.
  LESSON (do not repeat): parity.sh counts from
  AREA HEADERS, not table rows — a flip MUST update
  the row AND the area header AND the headline, or
  the counts silently don't move. The first flip
  commit a53d813 changed only the row (parity.sh
  still MATCHed at the old counts — headers ==
  headline, table disagreed by one row); follow-up
  c0afdc4 updated the Area 1 header (FULL 37→38,
  PARTIAL 26→25, 60.3%/80.2%) + headline. PARITY
  NOW: FULL 250 / PARTIAL 184 / MISSING 216 /
  WRONG 2, strict 38.3%, weighted 52.5% (parity.sh
  MATCH). First parity move of the test phase.
  Follow-up (not a blocker): cache_affinity.go
  header claiming the production install "is still
  a coordinator step" is stale (it landed in
  main.go) — doc-touch owed.
- V-B203 DONE (~11:35 IST) — verdict STAYS PARTIAL
  (no flip). Code in path: OrderExpiryFirst/
  ExpiryFirstScore (quota.go:454) via HookFactory
  StrategyExpiryFirst case (wiring.go:662). Cold
  on PC: both tests PASS (0.298s/0.309s). Toggle-
  check (flock, ~2 min): disabling OrderExpiryFirst
  → identity makes BOTH tests FAIL. Restored clean
  (VM byte-identical to HEAD, PC hash == HEAD,
  test PASSes again). Why not FULL: no quota-expiry
  data exists anywhere — deadline falls back to the
  (real) reset time; G19 gives a measured fraction
  when the signal carries QuotaRemainingPct > 0,
  else booleanised QuotaHint. Row annotation
  applied by root + committed in a53d813 (:497
  stays PARTIAL). NOTE: PARITY.md:910 has a second
  `expiry-first` row (MISSING, UNVERIFIED) in
  another table — separate decision, out of scope.
- V-B201 DONE (~11:32 IST) — verdict STAYS PARTIAL
  (built seam verified live; row cannot reach FULL
  yet). Code in path: newRelayFor/relayAttemptReq/
  relayNote called from chatWithChainStrategy
  (router.go:4661/4896/4951/5109) and runStream
  (router.go:6049/6284/6566/6831); config wiring
  live (main.go:3469-3479, config.go:1430-1450 +
  validation). Cold on PC (hash-verified 6/6 vs
  HEAD b7a2545): both tests PASS (0.05s). Toggle-
  check (under flock, relayAttemptReq disabled):
  test 1 FAILS as expected ("second attempt got no
  handoff system message", relay_test.go:274).
  Toggle restored; VM + PC relay.go sha256 = HEAD
  hash; tree clean except cron bookkeeping. Still
  missing (row's own gaps, confirmed in code): no
  production summariser, no quota-use signal, no
  cross-request store, chat-format injection only,
  fixed thresholds/TTL, budget gate undercounts
  handoff tokens. Row annotation applied by root +
  committed f0d03b6 (:489 stays PARTIAL; parity
  unchanged 250/184, 38.3%/52.5%, MATCH). 5
  verifiers still running (V-B202, V-B205-GROUP,
  V-B206, V-B215, V-B217-GROUP).
- V-B206 DONE (~11:33 IST) — verdict FLIP TO FULL
  (:503 context-optimized). Code in path:
  OrderByContextOptimized (affinity.go:350)
  largest-window-first, stable, unknown-last,
  identity when no window known; HookFactory case
  StrategyContextOptimized (wiring.go:706);
  production signalsFn fills ContextWindow from
  the registry model entry (main.go:2595), fed by
  operator-set or models.dev sync (main.go:1504,
  1561–1575); StrategyHook = HookFactory(signalsFn,
  …) (main.go:3096). Row's blocker is STALE:
  "context windows are not a hook input yet" —
  G19 Area 1 made it a live hook input. Cold on
  PC at HEAD: all 5 tests PASS (ok internal/
  strategy 0.299s). Toggle-check (flock): P1
  wiring bypass → LiveWindow FAILS ("abc" want
  "bca"); P2 comparator inverted → all 4
  reordering tests FAIL; both restored, hash-
  verified. Caveat: the row's named test pins
  identity-when-no-data only — the flip rests on
  the LiveWindow + affinity unit tests. Row
  file:line also stale (affinity.go:203 → :350).
  Flip applied by root + committed e6a4ac9 (row +
  Area 1 header + headline in ONE commit — the
  header lesson applied). PARITY NOW: FULL 251 /
  PARTIAL 183 / MISSING 216 / WRONG 2, strict
  38.5%, weighted 52.5% (parity.sh MATCH). 4
  verifiers still running (V-B202, V-B205-GROUP,
  V-B215, V-B217-GROUP).
- V-B205-GROUP DONE (~11:54 IST) — all 5 rows
  STAYS PARTIAL, no flip. Cold (PC, HEAD-verified):
  4 pkgs ok, 913 PASS / 0 FAIL. Toggle-A:
  StrategyBalanced reverted to 5-factor
  HookWithWeights (wiring.go:543, under flock,
  PC hash verified toggled during the test) →
  913 PASS / 0 FAIL, identical — NO test pins the
  default 16-factor wiring. Code IS in the request
  path (read-verified): wiringHook StrategyBalanced
  →Hook16Default (wiring.go:543); main.go:3097
  StrategyHookForRequest=HookFactoryWithCacheAffinity;
  balanced is the default when nothing is configured;
  rankModels→RankModelsByBestPair16/DefaultWeights16
  (main.go:2849–2876), SetAutoRanker main.go:1284,
  autodispatch.go:265; LKGP session affinity layered
  via LatestHealthyRecord. B2-05 (:501) STAYS PARTIAL
  — pool ranking live in code + scorer tested, but
  no test fails if the ranker is uninstalled
  (rankModels closure untested; only stub-ranker
  plumbing tests). Row text "still pending" is
  stale — gap is test coverage, not code. B2-10
  (:510), B2-11 (:511), B2-12 (:512) STAYS PARTIAL
  — weights sum to 1.0, all 16 factors computed,
  scorer tested, but the "default uses it" half is
  unpinned per toggle-A. B2-16 (:527) STAYS PARTIAL
  — UNCLEAR RESOLVED: NO pool-level stickiness test
  exists. Factor math unit-tested
  (TestFactorSessionAffinity 1.0/0.7/0.3/0.5), LKGP
  plumbing tested (TestG19LKGP*,
  TestAutoVariantLKGPExplicit — auto/lkgp variant
  only), main.go layers the record into pool inputs,
  but no test runs the real pool ranking with an
  LKGP pair and asserts the last-good model leads.
  Missing test (tester lane, unlocks B2-10/11/12):
  TestBalancedHookOrdersBySixteenOnlyFactor —
  factory("balanced") on a chain where only a
  16-only factor discriminates. The scoring.go
  header promised exactly this test; no test in the
  tree calls Hook16Default/Hook16. Restore verified:
  wiring.go = HEAD on VM + PC (sha256 1ba934d2…);
  tree clean except cron bookkeeping. Logs:
  hidden_files/verify-b205-cold-2026-11-20.utf8.log
  + verify-b205-toggleA-2026-11-20.utf8.log.
  Caveats: toggle-A poll loop never saw the EXIT:
  line (detached PS died before append) — killed
  the script after ~14 min, restored manually,
  counts verified from the fetched log file. /tmp
  was wiped mid-task (VM restart) — tsproxy restored
  from hidden_files. Time-box exceeded (~45 min →
  ~2 h, mostly infra: tunnel wipe + scp quoting
  bugs). All 5 rows' stale text corrected by root
  + committed 268f68d (parity unchanged 251/183,
  38.5%/52.5%, MATCH). 3 verifiers still running
  (V-B202, V-B215, V-B217-GROUP).
- V-B217-GROUP DONE (~11:55 IST) — 8 rows verified,
  1 flip, 7 stay PARTIAL. Cold: all 16 named tests
  PASS on the PC at HEAD (autorouter 6/6, router
  10/10, 11:41 IST; post-run PC hashes match HEAD).
  Code in request path confirmed: OrderedExplicit
  set autodispatch.go:263/:305/:308, threaded through
  7 entry points (router.go:7101/:7108/:7113,
  :7154/:7181, :7206/:7229), consumed router.go:
  4516/:5572. B2-17 chaos :534 STAYS PARTIAL — panel
  of 5 + engine fan-out verified; fault-injection
  deliberately not implemented; Toggle A: no test
  fails with OrderedExplicit off, so dispatch-order
  preservation is unproven. B2-18 subscription :535
  STAYS PARTIAL — fail-closed, rung-0 only, 0 provider
  calls on empty rung verified; no billing data exists
  (by design); single-model so OrderedExplicit=false
  by construction, ee8c30c irrelevant here. B2-19
  thrifty :536 STAYS PARTIAL — 5-rung ladder + served
  rung-0 (sub-model) through Chat verified; Toggle A:
  engine test still passes with the flag off (fixture
  has no quota hints, no re-rank triggered) — needs a
  quota-hinted fixture for a FULL flip. B2-20 best-*
  :538 STAYS PARTIAL — resolver + dispatch live;
  vision/reasoning fail open (no vision data, no
  quality threshold). B2-21 pro-* :539 STAYS PARTIAL
  — resolver + dispatch live, fail-open pinned
  (TestAutoProCodingFailOpenPinned); no premium
  classification source. B2-22 cascade :542 STAYS
  PARTIAL — helper built + tested with injected tiers;
  NOT wired (no per-attempt billing signal); ee8c30c
  never touched cascade.go. B2-23 endpoint combos
  :543 STAYS PARTIAL — projection enforced at live
  entry points, fail-closed, 0 calls on rejection;
  audio has no provider client, can't succeed end to
  end. B2-24 maxGlobalAttempts :544 FLIP TO FULL —
  clamp [1,200] live in withDefaults (router.go:
  998-1007), applied by NewEngine (:1385);
  TestMaxAttemptsClamp PASS cold; Toggle B: clamp off
  → FAIL (MaxAttempts 500 -> 500, want 200 (clamped)).
  Row text "no upper clamp" + file:lines (286-289,
  569-570) are STALE (clamp landed d21d9d1,
  2026-11-18). Flip + 7 annotations applied by root
  + committed bb72232 (row + Area 1 header + headline
  in ONE commit). PARITY NOW: FULL 252 / PARTIAL 182 /
  MISSING 216 / WRONG 2, strict 38.7%, weighted 52.6%
  (parity.sh MATCH). DEVIATION (recorded plainly):
  toggles ran in an isolated VM git worktree
  (/tmp/vb217-wt, removed after restore), NOT via the
  shared-PC flock — the lock queue was blocked
  (V-B202 held 13 min, another waiter ahead; the
  20-min window would have expired). Same HEAD, same
  code/tests; feature-off behavior is platform-
  independent Go logic; main worktree never touched
  (verified clean). Cold runs were on the PC per the
  brief. ACCEPTED by root: the toggle evidence (clamp
  off → test FAILs) is valid; the deviation is a
  process shortcut under a blocked lock, not a
  weaker check. NOTE: V-B202 holding the verify lock
  13 min is a bottleneck — the flock queue needs
  watching in batch 2 (consider shorter toggle
  windows or a second lock for non-shared files).
  2 verifiers still running (V-B202, V-B215).
- Critic run 11:59 IST — audit-fake PASS (0 findings,
  224 routes); parity MATCH 252/182, 38.7%/52.6%; last
  3 flips name tests ✓ (:544, :503, :504). Gaps
  unchanged (:782 PII vault BLOCKED, :827 provider
  detail B8-blocked, :816 update flow B8-blocked).
  FIX COMMITTED 9100ec0 (root-verified: 1 file, comment-
  only): stale cache_affinity.go header corrected —
  install landed main.go:3084/3097 (V-B207's owed
  doc-touch). Proof: HEAD 1 hit "still a coordinator
  step", fixed 0 hits; go build OK; pkgtest internal/
  strategy ok 0.142s. No PARITY flip, no push.
  Checked, not fixed: S1-939 maybeCompress doc-touch
  already done; docs/gate-last.txt still shows stale
  pre-fix literal (regenerates on next gate run).
- V-B202 DONE (~12:03 IST) — verdict STAYS PARTIAL
  (ordering verified, toggle BLOCKED, row's gap
  confirmed real). Code in real request path: YES.
  ResetAwareScore (quota.go:366) + OrderResetAware
  (:389), wired via StrategyResetAware case
  (wiring.go:668) in HookFactory; production installs
  it as StrategyHook/StrategyHookForRequest
  (main.go:3096-3097); router calls the hook once per
  request on the request path (router.go:629-656).
  Cold run on PC (HEAD, 4/4 files hash-verified):
  both tests PASS, exit 0, 0.293s. Toggle-check: NOT
  DONE (BLOCKED). Lock congestion: another verifier's
  multi-cycle script held /tmp/codvian.verify.lock
  nearly continuously ~25 min; my flock waits (14 min
  + 10 min) both expired. One transient egress-proxy
  403 on scp. Toggle was inserted + restored twice;
  VM file verified clean (hash = HEAD) both times; PC
  file never modified (hash = HEAD). No fail-when-off
  evidence obtained — not claimed. Row's PARTIAL reason
  CONFIRMED still true: the only production writer of
  the registry quota hint is health.go:572 (0 on quota
  error); no measured positive fraction ever reaches
  the registry. The engine's measured learned hint
  (learnRateLimit → setLearnedQuotaHint) feeds only
  filterZeroQuota, not signalsFn's QuotaRemainingPct.
  In production the remaining component is booleanised
  (unknown → 1/full, quota-error → 0), exactly as
  PARITY.md:495 states. Mechanism is continuous-capable
  (remainingFraction, QuotaRemainingPct/100 in
  toQuotaSnapshots) but no measured producer feeds it
  — that is the missing piece for FULL. Row annotation
  applied by root + committed b5aee15 (:495 stays
  PARTIAL; parity unchanged 252/182, 38.7%/52.6%,
  MATCH). PROCESS FINDING (batch-1, second confirmation):
  one agent's multi-cycle script monopolises the verify
  lock — V-B202's two waits expired (14 + 10 min) and
  V-B217-GROUP bypassed the flock for the same reason.
  Batch 2 fix: cap a toggle hold at ~5 min per cycle,
  or split the lock per shared file (wiring.go vs
  others); do NOT let one agent queue-block the rest.
  (VM restart ~06:05 UTC wiped /tmp; tsproxy restored
  from hidden_files, chmod +x.) 1 verifier still
  running (V-B215).
- V-B215 DONE (~12:07 IST) — verdict STAYS PARTIAL
  (no flip). B2-15 (:525) built end-to-end, core
  tested, but 3 of the row's 4 named gaps are
  untested, proven by toggle-check. Cold run on PC
  at HEAD b7a2545: 23/23 PASS (16 TestAutoCombo* +
  7 TestG19AutoCombo*, ok internal/server 0.333s,
  EXIT:0). Route registered server.go:1758, handler
  server.go:3686, live pool + free/pro/category
  filters all asserted. Toggle A (route renamed):
  server panics at startup (guard.go:575, no
  guard-tier classification) → binary FAIL.
  Registration is load-bearing; endpoint is live in
  the request path. Toggle B (exclusions store off):
  23/23 PASS — store built (d380b90) + wired, but
  zero tests cover it (no test for
  autoCandidateOverrideStore/autoComboExcludedIDs).
  Toggle C (StaleEngine.ModelLockouts forwarder off):
  23/23 PASS — forwarder exists (degrade.go:2547,
  B232-36; serve-mode chain main.go:3981 →
  StaleEngine → router/lockout.go:257 verified by
  reading), but zero test call sites of ModelLockouts
  anywhere; route-test fake engine doesn't implement
  it. Toggle D (FreeAccessExclusion derivation off):
  23/23 PASS — derivation built + wired, but no
  HTTP-level test asserts the field (only pure-handler
  passthrough). subscription/lkgp 503 fail-closed by
  design, also untested at the endpoint. Needs 3 new
  tests before FULL: (1) exclusions-store HTTP test
  (excluded=true via overrides file), (2) ModelLocked
  end-to-end HTTP test (locked model → reachable=
  false), (3) FreeAccessExclusion HTTP assertion
  ($0 → nil, paid → "not-in-catalog"). All toggles
  restored — VM tree clean (only cron bookkeeping),
  PC copy re-verified at HEAD (8/8 hashes, then 3/3
  after a VM restart wiped /tmp mid-task). Notes:
  PARITY row's "16 tests" is stale (now 23);
  autocombo_route.go:48-55 comment claiming the
  degrade lane "does not yet forward ModelLockouts"
  is stale since B232-36 (one-line doc fix, not mine
  to make — owed). Toggle A2 (handler-level 503) not
  run — verify lock held by another verifier's `sleep
  1200` hold until ~06:43 UTC and my 45-min box
  ended; verdict doesn't depend on it. Row text
  applied by root + committed 8b63ae0 (:525 stays
  PARTIAL; parity unchanged 252/182, 38.7%/52.6%,
  MATCH).
- VERIFY BATCH 1 COMPLETE (12:07 IST, 9/9 verifiers).
  20 rows verified (spawn note said 21 — recount:
  1+1+1+5+1+1+1+1+8 = 20; the 21 was a slip, no row
  was skipped — the queue's B2 section is fully
  covered). Results: 3 FLIP TO FULL (:503 context-
  optimized, :504 cache-optimized, :544 maxGlobal
  Attempts clamp), 17 STAYS PARTIAL (with strengthened
  evidence + corrected stale text). Parity: 249/185
  → 252/182, strict 38.2% → 38.7%, weighted 52.4% →
  52.6% (parity.sh MATCH). Batch-1 process findings:
  (1) verify-lock congestion — one agent's multi-cycle
  script monopolised /tmp/codvian.verify.lock ~25 min
  (V-B202's waits expired; V-B217 bypassed via a VM
  worktree); batch 2 caps a toggle hold at ~5 min per
  cycle. (2) VM restarts wipe /tmp repeatedly (tsproxy
  restored each time from hidden_files). (3) Several
  rows were stale (B2-05/10/11/12/16 "5 factors",
  :544 "no upper clamp", :503/:504 blockers) — the
  verifiers corrected them. NEXT: batch 2 (next ~10
  rows from docs/STAGE1-VERIFY-QUEUE.md) + the 18
  NO-TEST rows need testers first.
- VERIFY BATCH 2 SPAWNED (12:08 IST, 5 verifiers,
  10 rows, 45-min boxes). Grouped by shared files:
  V-B227-GROUP (B2-27 :579, B2-28 :593, B2-29 :600,
  B2-30 :602 — all touch internal/router/router.go);
  V-B232-GROUP (B2-32 :738, B2-36 :754 — degrade.go);
  V-B234-GROUP (B2-34 :750, B2-35 :751 — anthropic.go);
  V-B233 (B2-33 :747); V-B237 (B2-37 :755). B2-31
  (:730) SKIPPED — NO TEST row, goes to the tester
  lane (needs a test written first, cannot flip on
  code inspection alone). Batch-2 brief adds the
  lock lesson: toggle hold capped at 5 min per cycle;
  wait up to 5 min for the lock, then report BLOCKED
  — never bypass with a worktree. Correction to the
  batch-1 count note: the queue's B2/Area 1 section
  header says 21 rows — that includes B2-38 (:1173,
  already FULL, skipped by design), so 20 verified +
  1 already-FULL = 21; nothing was skipped.
- SCALE-UP (Raunak, voice call ~12:30 IST): "zyada
  agents kyun nahi laga rahe, testing tezi se ho" —
  VERIFY BATCH 3 + TESTER LANE spawned together
  (9 new agents, 12:30 IST; live total 11 with the
  2 batch-2 verifiers). Batch 3 (B3 / Area 4, 7
  rows): V-B301 (B3-01 :659 MCP scopes), V-B302-GROUP
  (B3-02 :663 device tracking + B3-06 :677 key
  creation — web/index.html), V-B303 (B3-03 :664 key
  templates/import), V-B305 (B3-05 :676 introspection
  — DO NOT FLIP, AK-3 verify-only). Tester lane
  (NO-TEST rows, write tests first): T-B304 (B3-04
  :669 dashauth tests), T-B307 (B3-07 :678 oauth
  PKCE+JWT tests), T-B427-GROUP (B4-27/28/29/49
  compress-engine + styles tests), T-B451 (B4-51
  tokens-saved/NoCharge tests), T-B231 (B2-31
  NeuralEmbedder tests). Honest constraint told to
  him on the call: verification is lock-bound (one
  agent at a time can toggle a shared file), so more
  verifiers alone do not speed the toggle step —
  testers run lock-free in parallel, which is why
  the tester lane was added in the same scale-up.
- T-B307 DONE (~12:33 IST) — B3-07 OAuth/JWT gateway
  auth tests written, no production code touched.
  New files: internal/oauth/jwt_test.go (13 tests:
  JWT issue/verify round-trip, expiry, wrong key,
  tampered payload/signature, wrong alg/issuer,
  malformed, TokenExpiry, PKCE verifier/challenge
  incl. RFC 7636 vector) + internal/oauth/oauth_test.go
  (12 tests: full authorize→exchange→verify flow,
  wrong-verifier/single-use code, refresh rotation/
  replay, revoke access/refresh, authorize + register
  validation, confidential-client secret, delete,
  persistence across restart, TokenExpired). PC cold
  run: 25/25 PASS (ok internal/oauth 1.244s, exit 0);
  VM vet + gofmt clean. Commit e10a888 (2 files,
  +756, root-verified). Other untracked test files in
  the tree belong to parallel testers — left untouched.
  B3-07 now has tests; a verifier can flip it once
  batch 3 reaches it (V-B305's lane is :676, so :678
  needs a verifier pass — queue for batch 4 or a
  follow-up verifier).
- V-B301 DONE (~12:34 IST) — B3-01 MCP scope
  enforcement :659 STAYS PARTIAL, toggle NOT done
  (BLOCKED), no flip. Cold on PC at HEAD 304c129
  (4/4 hashes match): internal/mcp 131/131 PASS,
  0 FAIL; all named scope + enforcement tests PASS;
  TestCheckKeyTokenLimitsScopes PASS (ledger, 0.293s)
  — CORRECTION: that test is a LEDGER per-key
  token-budget test (M5.3), NOT an MCP-scope test;
  the MCP-scope half is scopes_test.go +
  enforcement_test.go. Code in live path YES
  (dispatch → scope check -32002 → guardrail →
  dispatch, mcp.go:694-736; enforcement ON by
  default, NO off switch; New() defaults Anonymous,
  zero scopes). Row text "no auth/scoping at all" /
  "Opt-in, default OFF" is STALE (false) but no flip
  without the toggle — toggle re-run owed. URGENT
  RESTORE DONE BY ROOT: the verifier's temporary
  toggle (scope check wrapped in `if false`) was
  still in internal/mcp/mcp.go when its restore call
  was refused (safety flag disabled its tools). Root
  ran `git checkout -- internal/mcp/mcp.go` — hash
  30a2be81 matches HEAD exactly, tree clean. PC copy
  hash-verified via ssh (SHA256 975ba8bb… == VM) —
  the toggle never reached the PC (its sync step was
  the blocked call), so the scp re-sync failures
  (tunnel "Connection closed") are moot; PC copy was
  already at HEAD. No PARITY.md edit, no commit by
  the verifier.
- V-B305 DONE (~12:35 IST) — B3-05 self-service key
  introspection :676 STAYS PARTIAL, no flip. Cold
  on PC at HEAD 6b9ce1a (4/4 hashes match): 4/4 PASS
  (TestMeSessions* — ok internal/server 0.399s).
  Toggle-check (flock, ~2 min): handler forced to
  return the empty list → TestMeSessions FAILs,
  other 3 PASS. Restored clean (VM + PC hashes =
  HEAD). AK-3 CONFIRMED still real: Ledger.
  AgentSessions (ledger/agent_sessions.go:259) has
  ZERO production callers; handleMeSessions
  (mesessions.go:51-90) serves from
  s.responses.Snapshot() (in-memory responses.Record)
  and never calls AgentSessions. Row text corrected
  (":676 says /v1/me/sessions does not exist" is
  false — the endpoint EXISTS, list + detail, and is
  tested) + annotation committed 34db63c (parity
  unchanged 257/177).
- T-B451 DONE (~12:36 IST) — B4-51 tokens-saved
  tests written, no production code touched. New
  files: internal/degrade/b451_nocharge_test.go
  (exact hit → 1 NoCharge row, prompt+completion
  saved; semantic hit → prompt-only saved; nil
  recorder → no panic) + internal/ledger/
  b451_nocharge_test.go (NoCharge row costs $0 with
  prices configured, control row prices > $0;
  reported cost on a NoCharge row ignored). PC cold
  run: 5/5 PASS, EXIT:0 (degrade 0.273s, ledger
  0.227s); VM run also 5/5 PASS; gofmt + vet clean.
  Commit 7fbd053 (2 files, +334, root-verified). PC
  log saved at hidden_files/b451-test-2026-11-20.log.
  Note: another lane's in-flight toggle (M internal/
  wire/anthropic.go — V-B234-GROUP) was present in
  the tree; left untouched. B4-51 now has tests; a
  verifier can flip it once the B4 verification
  round reaches it.
- T-B304 DONE (~12:37 IST) — B3-04 dashboard auth
  tests written, no production files changed. 9 new
  tests in internal/server/dashauth_test.go: setup
  (401/403/400/201/409), Argon2id hash persisted
  0600 + reload from disk, login success/wrong-
  password + HttpOnly 12h cookie checks, 5-fail 60s
  throttle (6th attempt 429 even with correct
  password; per-peer), logout invalidates session,
  change-password (session + admin-key paths, all
  sessions invalidated), feeds gated once password
  is set (401/200, shell stays open), login with no
  password set, session expiry. Commit 7347a4b
  (1 file, +483, root-verified). PC cold run: 9/9
  PASS, 3.875s, EXIT:0 (detached WMI launch; log at
  hidden_files/t-b304-dashauth-2026-11-20.log); VM
  run also 9/9 PASS, 6.5s. One fix during the task:
  renamed the dashSessionCookie helper (collided
  with the existing const). B3-04 row :669 now has
  test coverage — a verifier can flip it via the
  truth protocol (batch 4 or a follow-up verifier).
- V-B303 DONE (~12:38 IST) — B3-03 key policy
  templates / key import :664 FLIP TO FULL. Code in
  live path: GET /admin/key-templates (guard.go:430,
  admin scope) → handleKeyTemplates; POST /admin/
  keys/import (guard.go:418, TierAlwaysProtected) →
  handleKeyImport; template applied at creation via
  resolve() (admin_keys_api.go:150-175). Row's "no
  template or gateway-key import" is false — both
  built and tested; row's file:line guard.go:167 is
  stale (guard table now 224 routes; routes at
  :418/:430). Cold on PC at HEAD 41ea2e2 (3/3 files
  hash-verified): TestKeyTemplatesList + TestKeyImportRoundTrip PASS (ok internal/server
  0.456s, EXIT:0). Toggle A (templates off → empty
  list): TestKeyTemplatesList FAILS ("templates = 0,
  want 3"). Toggle B (import forced 503):
  TestKeyImportRoundTrip FAILS ("import: status =
  503, want 201"). Both under flock, each ≤5 min.
  Restored: VM + PC hashes = HEAD; post-restore
  re-run both PASS (0.422s). Import test is end-to-
  end: 201 + fingerprint, raw key never echoed,
  imported key authenticates a real chat request,
  idempotent retry 200, disagreeing retry 409. Flip
  applied (row + Area 4 header + headline in ONE
  commit) 38ae952 → parity 258/177, 39.6%/53.1%
  (parity.sh MATCH). Area 4 now: FULL 17 / PARTIAL
  2 / MISSING 4, 73.9%/78.3%.
- VERIFY BATCH 2 COMPLETE (~12:37 IST, 5/5
  verifiers done): 5 flips (:747,:755,:579,:593,
  :602), 5 stays (:600,:738,:754,:750,:751). V-B232-
  GROUP DONE: both rows STAYS PARTIAL, no flip —
  cold on PC 72/72 PASS (B2-32: 32 M62/M62b tests;
  B2-36: 36 M66/M66b tests + 4 G13 tests); B2-32
  :738 stays — SQLite persistence tested + live;
  Redis vector store BUILT + wired (9149db0). V-B234-
  GROUP DONE: both rows STAYS PARTIAL, no flip —
  cold on PC B2-34 15/15 PASS, B2-35 21/21 PASS,
  0 FAIL; B2-34 :750 stays — tier split live in path
  (wire parses usage.cache_creation 5m/1h,
  CacheWriteTiers() feeds ConvertRe…). Full verdicts
  for :738/:754/:750/:751 OWED via their completion
  handoffs (only truncated previews in the agent
  list so far) — apply annotations when they land.
  B2 verification tally now: 32 rows → 8 FLIP TO
  FULL + 1 already FULL (:1173) + 22 STAYS PARTIAL
  + 1 NO TEST (:730, T-B231 writing tests).
- B2-FIRST REDIRECT (Raunak, voice call ~12:39 IST):
  ALL agents on B2, block by block — BUILD first to
  100%, then TEST, then VERIFY, then B3. His words'
  substance: build all 44 B2 features properly and
  completely first, then test properly (his
  responsibility is mine), fix bugs, verify, pass
  the block, then move to B3 — same for every block;
  he will check the 44-feature count himself. V-B302-
  GROUP (B3 rows :663/:677) INTERRUPTED → now
  T-B2-STRAT (missing strategy tests: 16-factor
  pinning test unlocking :510/:511/:512 + pool-level
  LKGP stickiness test unlocking :527). T-B427-GROUP
  (B4 compress tests) INTERRUPTED → now T-B2-ROUTER
  (missing router tests: rankModels closure :501,
  quota-hinted thrifty :536, chaos dispatch-order
  :534, quota-share engine-path :508). B3/B4 work
  PAUSED, not cancelled — resumes at their turn;
  any partial B4 test files stay uncommitted in the
  tree (T-B2-ROUTER to report their paths). NEW:
  S1-B229-FIX (builder — B2-29 :600 code gap: wire
  "provider/model" prefix → routing.only on the
  plain chat path) + T-B525 (tester — :525's 3
  candidate-pool tests). T-B231 (B2-31 tests) already
  B2, continues. B2 fleet now 5 agents. B2
  remaining BUILD: :600 fix + missing tests
  (strategy 2, router 4, server 3, B2-31 in flight)
  + decision items (B2-09/13/25/40/26/04 — need
  Raunak; B2-40's fix exists via critic 051c176,
  needs V's pass). After build: full B2 test phase
  + re-verification of the 22 STAY rows, then a B2
  PASS confirmation to Raunak (44 features built +
  tested + passed) before B3 starts.
- T-B2-ROUTER DONE (~12:54 IST, commit 90e7ab5):
  14 new B2 router tests, all PASS cold on the PC
  (ok internal/router 0.378s, 14/0). 4 files, 561
  lines: b2_chaos_order_test.go, b2_quotashare_
  engine_test.go, b2_ranker_test.go, b2_thrifty_
  test.go. Ranker closure (:501, reversing-ranker
  + uninstall flip), quota-hinted thrifty (:536, 1%
  rung gated out, served sub-high + hintless
  control), chaos OrderedExplicit (:534, panel
  holds vs quota-hint sort + reversing hook, both
  with off-controls), quota-share engine path
  (:508, at-cap gating + fail-open, P2C swap both
  ways, tree intercept 2:1 rotation, model-specific
  share wiring). No production code touched. One
  fix during the run: chaos test named model "m1"
  instead of "auto/chaos" (test bug, not engine);
  fixed, re-ran green. Paused B4 files (uncommitted,
  in tree, PC-tested PASS in run 3): internal/
  compress/smartcrusher_test.go, ionizer_test.go,
  relevance_test.go, styles_test.go. Root verified
  the commit (4 files, all 14 test funcs present).
  The agent's 3 failed execs (1 unknown + 2 exit 1)
  are the test-bug iterations + the interrupted B4
  exec, explained by the report.
- V-B2-730 DONE (~13:00 IST) — FLIP TO FULL :730
  B2-31 NeuralEmbedder. Cold on PC at HEAD (prod
  files byte-identical to HEAD d6ea9ae): all 11
  tests in neural_embedder_test.go PASS, EXIT:0, ok
  internal/cache 2.074s — incl. the un-skipped
  TestPersistentSemanticCacheSetTextUsesNeuralEmbedder. Toggle under flock (held ~63s):
  six SetText* reverted to BoW → un-skipped test
  FAILS ("embedder calls = 0, want 1"); fix
  restored → PASS. VM + PC hashes = HEAD after
  restore (persist 8023194b…, redis 845125d8…).
  Code in live request path: server relay.go:744/
  747 + degrade.go:1018/1021 call SetTextForModel
  (Scoped) — the fixed methods are the persistent
  tiers' write path. Test file left DIRTY, diff =
  ONLY the removed t.Skip line (VM + PC hash
  83cf2431…). Known limits (environment, not
  missing code): Redis neural store path unproven
  end-to-end — no live Redis exists; main.go
  openStack closure untestable without a full-stack
  boot (contract it plugs into is tested). The
  agent's 1 failed exec (exit 1) is the designed
  toggle FAIL. Flip applied (row + Area 6 header +
  headline in ONE commit) 7ae4fcd → parity 259/176,
  39.7%/53.2% (parity.sh MATCH). Area 6 now: FULL
  27 / PARTIAL 4, 87.1%/93.5%.
- V-B2-525 DONE (~13:03 IST) — FLIP TO FULL :525
  B2-15 candidate-pool endpoint. Code in live path:
  guard.go:372 route → server.go:3686
  handleAutoComboCandidates → autocombo_route.go:105
  buildAutoComboPool (:122 lockouts, :146
  ModelLocked, :147 FreeAccessExclusion) →
  server.go:3713 ListAutoComboCandidates; exclusions
  store :591-605/:614/:712; ModelLocked forwarder
  :439-462 + production StaleEngine.ModelLockouts()
  degrade.go:2547 + router Engine.ModelLockouts()
  lockout.go:257; FreeAccessExclusion :744. Cold on
  PC at HEAD (8/8 files hash-verified = HEAD): 3/3
  PASS, 0.382s, exit 0 (commit b90b4a0). Toggles
  (each under flock, ≤5 min): A store off →
  exclusions test FAILS (Excluded:false, want
  true); B forwarder off → ModelLocked test FAILS
  (reachable=true, ModelLocked=false); C derivation
  off → FreeAccess test FAILS (glm "not-in-catalog",
  want nil). All 3 PASS on restore; VM + PC hashes
  = HEAD after each; zero toggle residue, tree
  clean. Row's stated gate ("Needs 3 tests before
  FULL") is met. Caveats for the flip annotation:
  auto/subscription + auto/lkgp 503 fail-closed
  path still untested at the endpoint (by design);
  FreeAccessExclusion is a documented price-only
  approximation (nil = known $0 by price, not
  allowance-verified). Stale autocombo_route.go:48-
  55 comment left untouched as briefed. Flip
  applied (row + Area 1 header + headline in ONE
  commit) aa9f32a → parity 260/175, 39.9%/53.3%
  (parity.sh MATCH). Area 1 now: FULL 41 / PARTIAL
  22, 65.1%/82.5%.
- B2-25 DECIDED — TREES, NO COPY (Raunak,
  2026-11-20 ~13:28 IST, voice call): combo-in-
  combo NOT built; our nestable strategy trees are
  best and do the same job — row :545 re-scoped
  PARTIAL → DIVERGENCE (ef4d4f9, parity.sh MATCH;
  Area 1 now FULL 53 / PARTIAL 9 / DIVERGENCE 1,
  85.5%/92.7%; headline FULL 273 / PARTIAL 162 /
  DIVERGENCE 11, comparable 651, 41.9%/54.4%).
  GENERAL RULE from him in the same call: do NOT
  ask him "which is best" — decide it myself; if
  our way does the same work and is better, use
  ours; do not copy everything. Recorded in
  ~/MEMORY.md. Applies to the remaining decision
  items (B2-26 hedging, B2-04 headroom) — decide
  on the merits, record the reasoning, tell him
  the call.
- PARITY.SH BUG FIXED (root, ~13:29 IST): the
  headline echo hard-coded comparable=652 (line
  43) — it printed 652 even after the B2-25 re-
  scope moved the true comparable to 651. Now
  parsed from the headline's "Comparable total"
  line (304b1ca). The MATCH check itself only
  compares the four verdict counts (unaffected).
  Verified: fresh parity.sh run — mechanical
  273/162/215/1 comp 651, headline 273/162/215/1
  comp 651, MATCH.
- CORRECTION — :600 / 42.5% CLAIM IS WRONG
  (2026-11-20 ~13:29 IST): a statement reached
  Raunak saying ":600 ki dobara jaanch FULL ho
  gayi, parity 42.5%, B2 failover 40/40". Verified
  false: :600 (PARITY.md:600) is still PARTIAL
  with the old V-B2-600 annotation; V-B2-600-RE
  is still RUNNING (475s, active 19s ago); HEAD
  ed0449e; parity.sh MATCH at 273/162, 41.9%/
  54.4%, comparable 651. No :600 flip has been
  applied (only the root applies flips, and none
  was applied). Told to Raunak plainly on the call.
- B2-26 DECIDED BY ROOT (Raunak's rule,
  2026-11-20 ~13:30 IST): keep our faster hedging
  default (ON at 150 ms — better for failover
  speed, deliberate) and BUILD only the missing
  body-size guard (never hedge bodies >256 KB,
  same threshold as OmniRoute — a genuine
  safeguard, not a copy). S1-B2-26-GUARD spawned.
  Once the guard is built + pinned, :548 can flip
  FULL with the deliberate-default note. The :945
  dashboard-control row (no UI control) is a
  separate dashboard item, NOT this build.
- B2-04 DECIDED — DIVERGENCE (root, Raunak's
  rule, 2026-11-20 ~13:30 IST): headroom ordering
  is built + registered (M3); no per-pair limit
  source exists in this local-first product
  (OmniRoute's util5h/util7d comes from its hosted
  usage tracking) — all targets score headroom 1,
  which is OmniRoute's own fail-open. The ordering
  activates automatically once a per-pair signal
  exists; building a per-pair usage pipeline is a
  new data source, not a missing feature. Row :498
  PARTIAL → DIVERGENCE (c9f7eff, parity.sh MATCH;
  Area 1 now FULL 53 / PARTIAL 8 / DIVERGENCE 2,
  86.9%/93.4%; headline FULL 273 / PARTIAL 161 /
  DIVERGENCE 12, comparable 650, 42.0%/54.4%).
  ALL SIX B2 DECISION ITEMS NOW DECIDED (09 built,
  13 building, 25 divergence, 26 guard building,
  04 divergence, 40 FULL). Remaining B2 work is
  tests + verification, not decisions.
- V-B2-489 DONE (~13:30 IST) — :489 STAYS
  PARTIAL, no flip. Code is real and in the
  request path, but no producer is pinned by any
  committed test. In-path, verified: summariser
  closure main.go:3214 (cheapest priced model,
  live client, 30 s/512 tok, structured prompt,
  error → raw-transcript degradation), wired :3309,
  called lazily via relayAttemptReq →
  maybeBuildHandoff; quota signal main.go:3288
  (LearnedQuotaHint → (100-hint)/100, clamped,
  unknown stays unknown), read per outcome in
  relayNote (relay.go:842-843); session store
  relay.go:680-751 + newRelayFor preload (:762),
  defer on both loops (router.go:4677/:6071),
  keyed by req.CallerID; AgentSessions not used.
  Cold on PC at HEAD: 14/14 TestRelay* PASS, ok
  internal/router 0.372s (PC copy hash-verified =
  HEAD on all 150 files; one transient torn read
  on router.go retried clean). Unpinned — grep
  over all *_test.go: relaySummariser 0,
  RelayQuotaUsedFor 0, relaySessions/
  storeRelayWantIfPending/loadRelayWant 0,
  parseRelayHandoffSummary 0; relay_test.go has
  zero "session" mentions. Existing relay tests
  inject fake summarisers — they pin the engine
  seam only. Negative toggle (flock): store
  disabled → still 14/14 PASS (ok 0.375s); restored,
  VM = HEAD = PC, zero residue. The other two
  closures live in package main — no engine test
  can reach them, so a toggle there is N/A by
  construction. Exact gap to flip: pinning tests
  still to be written — a package-main test for
  the summariser + quota signal, and an engine test
  driving two requests sharing one CallerID for
  the cross-request store. Secondary row gaps also
  untouched: chat-format injection only, fixed
  thresholds/TTL, budget gate undercounts handoff
  tokens. Row annotation updated (producers-built
  state, no count change). T-B2-489-PIN SPAWNED to
  write the pinning tests.
- S1-B2-13-FAULTINJ DONE (~13:31 IST, commit
  1403ffe, 3 files, +325/−5), gofmt clean.
  Built: ArmFault/DisarmFault/ClearFaults/
  ListFaults + callChat hook in internal/router/
  faultinject.go (new, ~290 lines); Engine registry
  fields faultMu/faults in router.go:1189; hook in
  callChat after handoffLabel, before limiter
  acquire; stale "NOT implemented" comment in
  combo.go updated. Behaviour: FaultFail returns a
  real provider error (429→RateLimitError,
  402→QuotaError, else UpstreamError) wrapped in
  faultInjectedError (unwraps, so taxonomy/breaker/
  lockout/quota-learning treat it exactly like the
  real failure; message names the injection);
  FaultDelay stalls then proceeds to the real call;
  per-(provider, model) match, MaxHits auto-disarm,
  ctx-aware stall. Enabled only by explicit
  ArmFault (test/operator API); OFF by default
  (empty registry = one read-lock probe, no
  behaviour change); no config, HTTP, or request-
  field surface. OmniRoute study: their chaos
  surface is panel fan-out + overrides/timeouts
  only — no fault injection exists there; this
  mechanism is our own design (EQUAL ≠ COPY,
  nothing copied). Scope note: hook covers chat
  completions via callChat (sequential chain +
  chaos/fusion panel fan-out); streaming/embeddings
  dial their own clients and are NOT covered — that
  belongs to the later harness. No tests written
  (build-only brief); T-B2-13-TEST SPAWNED to
  write the pinning tests (arm fault → fallback
  serves; disarm → normal). BUILD SCARE — FALSE
  ALARM: the agent reported full `go build ./...`
  RED, blaming applyLearnedQuota "declared-but-
  never-called" from 3b76fa9. Root verified: the
  closure IS called (main.go:2727/:2757, real call
  sites, not comments) and `go build ./...` on the
  VM is GREEN (exit 0). The agent's RED was a
  torn read on its PC copy during concurrent sync
  (known transient). No fixer needed; no action.
  Root verified commit 1403ffe (3 files, symbols
  ArmFault/DisarmFault present).
- S1-B2-26-GUARD DONE (~13:33 IST, commit
  21edef6, 1 file, +7, comment-only), PC build
  BUILD-OK, gofmt clean. Finding: the guard was
  ALREADY BUILT — internal/router/hedge.go (M4
  d21d9d1 + B2-1030 c1b215e): hedgeMaxBodyBytes =
  256 KiB (hedge.go:23), requestBodyBytes counts
  messages/parts/tool calls/tool definitions/tool
  choice, hedgeAllowed() gates BOTH firing sites —
  chat (router.go:4964) and stream (router.go:6374);
  hedgedChat/hedgedStream have no other callers;
  combo.go fires no hedges. Tests already exist:
  hedge_test.go (8) + m4_8_test.go (4 guard tests,
  recorded 4/4 PASS in c1b215e). Default kept: ON
  at 150 ms (router.go:1020-1021) — unchanged,
  deliberate. What the agent added: the deliberate-
  default comment at the guard site (hedge.go
  header). Row :548 text ("no body-size guard") is
  now STALE — ready for a verifier to flip FULL.
  V-B2-26 SPAWNED (cold PC tests + toggle). Root
  verified commit 21edef6 + guard symbols
  (hedgeMaxBodyBytes 256<<10, hedgeAllowed).
- T-B2-09-TEST DONE (~13:33 IST, commit
  f69fdbc, 1 file, 222 lines, test-only): 6 pinning
  tests in internal/router/b2_quota_cap_test.go
  (per-key hint recording, expiry-drop, startup
  restore, cap tail partition, fail-open, cap
  applied after strategy). Cold on PC: 6/6 PASS,
  exit 0, ok internal/router 0.323s (PC copy hash-
  verified = HEAD for all 4 feature files before
  the run). Toggle A (recorder no-op, under flock):
  5/6 FAIL — only FailOpen passes, as designed.
  Toggle B (cap check skipped, under flock): 2/6
  FAIL — exactly the two cap tests; signal tests
  pass. Restores verified: PC hashes = VM tree =
  HEAD for ratelimit.go + account_fallback.go after
  each restore; final re-run 6/6 PASS (one
  transient [build failed] mid-sequence from a
  concurrent PC sync/torn read — passed on retry
  with no changes). V-B2-09-CAP SPAWNED to verify
  + flip :509.
- REAL BUG IN 3b76fa9 — TOGGLE COMMITTED, NOW
  FIXED (root, ~13:35 IST): T-B2-09-TEST flagged
  that commit 3b76fa9 carries a stray verifier
  toggle in cmd/codvian-router/main.go —
  applyLearnedQuota() commented out on BOTH
  signalsFn return paths with TOGGLE-OFF comments,
  disabling the :495/:497 learned-quota overlay
  V-B2-QUOTA-GROUP verified. Root verified by
  reading the commit itself: `git show 3b76fa9:
  main.go` shows `// TOGGLE-OFF:
  applyLearnedQuota()` at :2727 and :2757; the
  parent had both calls live. CORRECTION to the
  6704df9 queue note: the S1-B2-13-FAULTINJ "build
  RED" report was REAL at committed HEAD (the
  closure would be declared-and-not-used) — the
  root's "false alarm" call was WRONG because it
  checked the dirty working tree, where someone
  had already restored the calls (uncommitted).
  The working-tree diff was exactly the two call
  restores, nothing else — committed as 8e4143c.
  `go build ./...` at the new HEAD is GREEN (exit
  0, VM). Lesson: verify a build claim against the
  COMMIT, not the dirty tree. V-B2-09-CAP's brief
  includes re-checking the restore.
- S1-B2-BREAKER-FIX DONE (~13:35 IST) — verdict:
  FLAKE, no code change, no commit. Baseline
  failure TestBreakerStaleDegradedPersistsAcrossReload
  (after reload CLOSED with 1 failure, want
  DEGRADED with 5 — 4 of 5 saves lost their rename
  under the 10–11-agent concurrent PC load). Step 1
  re-run (PC, alone, isolated D: copy at HEAD
  4f09e978): 3/3 PASS, exit 0, ~0.52 s each. Log:
  hidden_files/breaker-3x-2026-11-20.log. Mechanism
  (diagnosed): saveBreakerStore writes a fixed tmp
  (path + ".tmp") then renames with 5×50 ms retries
  (breaker_store.go:139); on Windows the
  destination can be briefly held by Defender/
  indexer; under heavy load the 250 ms budget is
  exceeded and consecutive saves lose their renames,
  leaving the file at 1 failure. Logic sound —
  breakerFailure saves unconditionally after every
  failure (router.go:3064-3070), saves serialized by
  breakerStoreMu, no in-process race. Caveat: re-
  runs were not zero-load, but on the isolated D:
  copy with far less contention — 3/3 clean passes
  vs 4-of-5 lost saves is strong flake evidence.
  Optional hardening (not done, per brief): unique
  tmp per save via os.CreateTemp + longer retry
  budget. B2 BASELINE NOW HAS EXACTLY ONE KNOWN
  FAILURE: LossyExactHit (design decision open).
- S1-B2-TEST-RACE2 DONE (~13:35 IST) — targeted
  `go test -race` on the VM, 7 packages, 647.8s,
  launched at HEAD ac1f95e (contains :489 d3abe14
  + :600 23b4189). Packages: 5 ok, 2 FAIL. ok:
  strategy (3.6s), scoring (3.6s), degrade (6.8s),
  cache (54.2s), provider (41.4s). FAIL: server
  (542.2s), router (80.7s). Distinct failures: 2
  only. DATA RACES: 0. (1)
  TestChatCompletionsLossyExactHitStillWorks
  (server, compression_cache_test.go:376) — KNOWN
  design call, pre-existing, untouched per brief.
  (2) TestBreakerStaleDegradedPersistsAcrossReload
  (router, breaker_lazy_test.go:102) — FLAKE, not
  real: failed in the full run (after reload CLOSED
  with 4 failures, want DEGRADED with 5) but PASSED
  3/3 alone with -race (ok, 2.585s). The PC
  baseline saw the same test fail differently
  (CLOSED with 1 failure) — the lost-save count
  varies with load. Consistent with S1-B2-BREAKER-
  FIX's independent FLAKE verdict (3/3 alone on
  PC). Caveat: 13 commits landed during the 10.8-
  min run, including the B2-09 cap (3b76fa9) and
  its tests (f69fdbc) — those were not in the
  launched tree, so a follow-up race pass is needed
  for the cap/tests. The 0-races verdict and the 2
  failures above are for the launched tree. Log:
  hidden_files/gotest-b2-race2-2026-11-20.log
  (1,429 lines + the 3× re-run appended). No fixes,
  no edits, no commits made. S1-B2-TEST-RACE3
  SPAWNED: targeted -race on internal/router only
  at current HEAD (covers the new cap + fault-
  injection code; server untouched by the new
  code, LossyExactHit not in this package).
- V-B2-26 DONE (~13:37 IST) — FLIP TO FULL:
  B2-26 :548 Hedging config. Guard in the real
  hedge path: hedgeAllowed() (hedge.go:
  hedgeMaxBodyBytes = 256 KiB, requestBodyBytes
  counts messages/parts/tool calls/tool definitions/
  tool choice) gates BOTH firing sites — chat
  (router.go:4964 → hedgedChat) and stream
  (router.go:6374 → hedgedStream). hedgedChat/
  hedgedStream have no other callers; combo.go fires
  no hedges (only noteHedgedLoser). Default ON at
  150 ms (router.go:1020-1021) — deliberate, better
  for failover speed. Cold on PC at HEAD 6704df9:
  11/11 PASS, ok internal/router 2.297s — all 7
  hedge_test.go tests (brief said 8; file has 7) +
  all 4 guard tests in m4_8_test.go. PC package
  127/127 files hash-verified = HEAD. Toggle
  (flock, one cycle): guard off (hedgeMaxBodyBytes
  = 1<<62, PC hash = VM hash) → TestHedgeBodySizeGuard
  FAILS (hedge fires, "fast" wins, want "slow");
  guard restored → PASSES. VM + PC hashes = HEAD
  after restore, zero residue. Row text corrected
  on flip: guard present (already built in M4
  d21d9d1 + B2-1030 c1b215e; builder commit 21edef6
  was comment-only); ON/150 ms is the deliberate
  default. Note: the :945 dashboard-control row (no
  UI control) is separate and stays PARTIAL — this
  flip is :548 only. Flip applied (parity.sh MATCH;
  Area 1 now FULL 54 / PARTIAL 7 / DIVERGENCE 2,
  88.5%/94.3%; headline FULL 274 / PARTIAL 160,
  comparable 650, 42.2%/54.5%).
- T-B2-QUOTA-PIN DONE (~13:38 IST, commit
  d402834, 1 file, test-only): cmd/codvian-router/
  b2_quota_pin_test.go (207 lines) —
  TestB2QuotaPinResetAwareOrdering (:495) +
  TestB2QuotaPinExpiryFirstLearnedDeadline (:497),
  driving the real openStack stack + mock upstreams.
  PC cold: 2/2 PASS (0.13s / 0.08s, ok cmd/codvian-
  router 0.529s); VM also 2/2 PASS, and 2/2 PASS at
  clean HEAD in a temp worktree (other agents'
  dirty router files excluded). Overlay-off FAIL
  proven: with both applyLearnedQuota() calls
  commented out, BOTH tests FAIL (reset-aware:
  half=50 served prov-full, want prov-half);
  restored after, PASS again. Fixes along the way:
  config needed version:1 + listen + quota_hint:-1
  (0 = "spent", drops the whole chain); Windows
  TempDir locks needed cleanup closing asyncLed/led/
  feedback + syncCachesOnShutdown (reasoning_cache.
  db). INCIDENT SOLVED: the agent's toggle-off lines
  were committed into 3b76fa9 by S1-B2-09-CAP while
  its proof ran (HEAD briefly had the overlay
  disabled) — NOT the builder's own toggle, a
  collision between the tester's in-flight toggle
  and the builder's commit. S1-B2-09-RESTORE fixed
  it in 8e4143c; the tester verified its restore is
  byte-identical to HEAD's main.go (sha256 63a39213…),
  no residue left. Root verified commit d402834
  (1 file, both test functions present).
  V-B2-QUOTA-RE SPAWNED to re-verify :495/:497
  (cold PC + toggle) for the flip.
- T-B2-13-TEST DONE (~13:39 IST, commit b99ffd6,
  8 pinning tests in internal/router/
  b2_faultinject_test.go, 275 lines). Cold on PC:
  8/8 PASS (ok internal/router 3.372s); the 3
  production files (faultinject.go, router.go,
  combo.go) hash-verified = HEAD on the PC before
  the run; test file hash-matched too. Toggle
  proven (VM): hook disabled → the 3 chain tests
  FAIL (failover serves from "a", no delay, no
  auto-disarm); restored clean, no residue. Tests:
  FailoverToFallback (faulted provider never
  dialled, fallback serves, every "a" trace attempt
  carries the injection error),
  DisarmRestoresNormalRouting, DelayStallsThenProceeds
  (150 ms stall, real call proceeds),
  MaxHitsAutoDisarm (applyFault-level, exactly 2 hits
  then disarmed), MaxHitsAutoDisarmAcrossRequests
  (MaxHits 8 → bee, bee, aye), ErrorTypes
  (429→RateLimitError, 402→QuotaError,
  500→UpstreamError, message names the injection),
  ModelNarrowing, OffByDefault (incl. ArmFault
  validation). Note for the verifier: MaxHits counts
  per attempt, and each request retries "a" 4× under
  the MaxAttempts=5 default — hence MaxHits=8 covers
  exactly two requests in the chain test. The
  scratch debug file (zz_debug_fault_test.go) is
  gone from the tree — cleaned up. V-B2-13 SPAWNED
  to verify :523 for the flip. Root verified commit
  b99ffd6 (1 file, 8 test functions) + row :523.
- S1-B2-TEST-RACE3 DONE (~13:39 IST) —
  internal/router `go test -race` PASS. Package: ok
  internal/router 84.597s, EXIT=0. Distinct
  failures: 0. DATA RACE: 0.
  TestBreakerStaleDegradedPersistsAcrossReload did
  not fail — no 3× re-run needed. First attempt died
  on a transient vet error (zz_debug_fault_test.go:
  no such file — a concurrent agent's file
  appearing/vanishing mid-compile); retried once,
  ran clean. Not a code failure. Coverage: launched
  at HEAD 41d0454 with all five new-code commits in
  tree (B2-09 cap 3b76fa9, its tests f69fdbc,
  overlay restore 8e4143c, fault-injection 1403ffe,
  B2-26 comment 21edef6). HEAD is now 0f3750b (queue
  notes landed during the run). Log: hidden_files/
  gotest-b2-race3-2026-11-20.log (2 lines — the
  brief's command had no -v, so only the package
  summary is recorded, no per-test PASS count).
  LossyExactHit untouched (internal/server, out of
  scope). No fixes, no edits, no commits. New-code
  race coverage is now complete: 0 races in
  internal/router at current HEAD.
- V-B2-09-CAP DONE (~13:40 IST) — FLIP TO FULL:
  B2-09 :509 (quota-share cap on account rotation).
  Real request path: orderAccountAttempts
  (router.go:2736, tree.go:107) → orderAccountCapCheck
  (account_fallback.go:232 → :272), after each
  strategy's ordering; per-key signal via
  setLearnedKeyQuotaHint / LearnedKeyQuotaHint
  (ratelimit.go), restore at startup (main.go).
  Cold on PC at HEAD 8e4143c: 6/6 named tests PASS,
  ok internal/router 0.326s (PC copy hash-verified
  = HEAD on all 4 files). Toggle under flock: cap
  function no-op → BOTH cap tests FAIL (tail order
  [k1 k2 k3], want [k1 k3 k2] / [k2 k3 k1]); restored
  → BOTH PASS (0.323s). VM = HEAD = PC hashes after
  restore, zero residue. (First call-site toggle
  only failed 1/2 — the direct test calls the
  function itself, so the function-level no-op is
  the correct toggle.) Restore verified:
  applyLearnedQuota() live at main.go:2727/:2757,
  no TOGGLE-OFF comments, `go build ./...`
  BUILD-OK. Row-text note applied: cap is measured
  (learned from real responses, deprioritises to
  tail) not configured-and-blocking like OmniRoute's
  — recorded as divergence on the flip. Flip applied
  (parity.sh MATCH; Area 1 now FULL 55 / PARTIAL 6 /
  DIVERGENCE 2, 90.2%/95.1%; headline FULL 275 /
  PARTIAL 159, comparable 650, 42.3%/54.5%).
- T-B2-489-PIN DONE (~13:42 IST, commit e2828ca,
  2 files, +565, test-only, no production changes).
  Engine test — internal/router/b2_relay_pin_test.go:
  31,117: TestB2RelayPinCrossRequestWantStoredAndPreloaded
  (request 1 codex success at used 0.90 → want
  stored, no handoff built; request 2 same CallerID
  → degraded handoff injected; request 3 same key →
  nothing, consumed once) +
  TestB2RelayPinWantKeyedByCallerID (key-B sees
  nothing, key-A still gets the want). Package-main
  test — cmd/codvian-router/b2_relay_pin_test.go:
  212,296,350: TestB2RelayPinProductionSummariserAndQuotaSignal
  (request 1 teaches hint 10 via real rate-limit
  headers → used 0.90; request 2 proves the
  production summariser dialled codex directly —
  exactly 1 call, model "m", max_tokens 512,
  structured prompt, "[1] SYSTEM:/[2] USER:" source
  messages — and the parsed summary injects as
  <context_handoff>, LastRelayStatus summarised, no
  degrade note), TestB2RelayPinQuotaSignalWindow
  (used 0.50 → no want, no summariser call),
  TestB2RelayPinWantKeyedByGatewayKey (key-B cold,
  key-A summarised). Cold PC: 5/5 PASS (internal/
  router 0.358s, cmd/codvian-router 0.567s), repeat
  run ok on both packages. PC production files hash-
  verified = HEAD before the run; only its 2 files
  synced. Fix worth knowing: the package-main tests
  first failed only at t.TempDir cleanup — openStack
  leaves SQLite handles open and Windows refuses
  removal (feedback.db, then ledger.db, then
  reasoning_cache.db in turn). Fixed test-side by
  mirroring the production shutdown in t.Cleanup:
  syncCachesOnShutdown + asyncLed/feedback/led closes.
  Flag (already handled): the agent flagged cmd/
  codvian-router/b2_quota_pin_test.go as uncommitted
  with the same openStack + TempDir shape and no
  shutdown — it is in fact COMMITTED (d402834) and
  T-B2-QUOTA-PIN reported fixing the same TempDir
  locks along the way (PC cold 2/2 PASS). No action.
  Pin strength: each test fails if its producer is
  off (summariser off → degraded handoff, summary
  assertions fail; signal off/unknown → no want, no
  handoff on request 2; store off → no handoff on
  request 2). No production-code toggle by the
  tester (test-only brief) — the toggle check is the
  re-verifier's job. V-B2-489-RE SPAWNED to re-verify
  :489 (cold PC + per-producer toggles) for the flip.
  Root verified commit e2828ca (2 files, 2+3 test
  functions).
- V-B2-13 DONE (~13:43 IST) — FLIP TO FULL:
  B2-13 :523 Mode pack chaos-mode. Real request path:
  callChat (router.go:3606) → handoffLabel (:3618)
  → applyFault hook (:3624) → limiter acquire
  (:3645). FaultFail returns real provider errors
  (429→RateLimitError, 402→QuotaError, else
  UpstreamError) wrapped in unwrapping
  faultInjectedError (faultinject.go:111-158);
  FaultDelay stalls then proceeds; per-(provider,
  model) match, MaxHits auto-disarm; OFF by default,
  armed only by explicit ArmFault. Cold on PC at
  HEAD: 8/8 TestB2FaultInject* PASS (4.427s). All
  128 committed internal/router/*.go files
  hash-verified byte-identical to HEAD before the
  run. Toggle (flock, one cycle ~57s): hook disabled
  → 3 chain tests FAIL (FailoverToFallback served
  "aye" not "bee"; DelayStalls 0s vs ≥120ms;
  MaxHitsAcrossRequests req 1 "aye" not "bee"); the
  other 5 pass either way (they pin registry/
  mapping/negative cases, incl. direct applyFault
  calls). Restored, re-run 8/8 PASS. Zero residue
  (VM router.go hash = HEAD, no toggle markers).
  Row text for the flip: fault-injection present;
  OmniRoute has no fault-injection (this is ours);
  streaming/embeddings not covered — that belongs
  to the later harness. Flip applied (parity.sh
  MATCH; Area 1 now FULL 56 / PARTIAL 5 /
  DIVERGENCE 2, 91.8%/95.9%; headline FULL 276 /
  PARTIAL 158, comparable 650, 42.5%/54.6%).
- SPEED-UP (Raunak, voice call ~13:43 IST —
  "verifying aur testing thoda speed up karo"): 3
  more agents spawned in parallel on the remaining
  B2 gaps — S1-B2-542-SELECTOR (config allowlist
  entry for global_strategy="cascade", the only
  remaining :542 gap; build + test + commit),
  T-B2-754-TEXT (text-resume injectReasoning test,
  the one untested :754 piece; test must FAIL with
  injection off), S1-B2-REDIS-PC (get a real Redis
  running ON the PC via WSL first, then Docker —
  so :738's resume exact-serving test can run cold
  on the PC; BLOCKED with exact evidence if neither
  exists). Running verifiers continue: V-B2-600-RE,
  V-B2-QUOTA-RE, V-B2-489-RE.
- T-B2-754-TEXT DONE (~13:47 IST) — no new test
  file was needed: the pinning test already exists,
  committed by the critic as 6716036
  (TestM66bTextStreamFromInjectsBeforeUpstream,
  internal/degrade/m6_6b_reasoning_test.go:405).
  The agent verified it instead of duplicating it.
  Cold on PC at HEAD 6f02a97: PASS (ok internal/
  degrade 0.281s); PC copy hash-verified = HEAD
  (test file + degrade.go); full TestM66b* file also
  ok on PC (0.287s). Toggle proof (VM): disabling
  ONLY the ChatStreamFrom inject call (degrade.go:
  2100) → test FAILS ("text resumer received
  reasoning <nil>, want restored text 'thinking
  about math'"); restored, tree clean, no residue.
  The :754 text-resume gap is now closed by a
  committed, toggle-proven test — ready for a
  verifier to flip :754. No commit from the agent
  (nothing new to commit). Root verified commit
  6716036 + test at :405. V-B2-754 SPAWNED to verify
  :754 (cold PC + inject toggle + DB-tier toggle
  attempt) for the flip.
- V-B2-QUOTA-RE DONE (~13:47 IST) — FLIP BOTH TO
  FULL: :495 reset-aware ordering + :497 expiry-first
  ordering. Code in real request path:
  applyLearnedQuota() defined main.go:2626, both call
  sites live (:2727/:2757, no TOGGLE markers);
  producer learnRateLimit ratelimit.go:48; mapping
  wiring.go:19/:24/:187; ExpiryFirstScore quota.go:
  461 spends ExpiresAt (:454). `go build ./...`
  green on VM. Cold PC at HEAD 0f3750bb: 2/2 PASS —
  TestB2QuotaPinResetAwareOrdering (0.13s) +
  TestB2QuotaPinExpiryFirstLearnedDeadline (0.07s),
  ok cmd/codvian-router 0.505s. PC hash-verified =
  HEAD via full 1509-file manifest (1 missing + 7
  changed synced, 106 EXTRA are scratch/untracked
  files). Toggle #1 (comment out both calls): build
  FAIL (`declared and not used: applyLearnedQuota`)
  — non-discriminating, superseded by #2. Toggle #2
  (discriminating, under flock): overlay body
  emptied, calls stay, VM build green. OFF → BOTH
  FAIL on order: :159 reset-aware, half=50: first =
  "prov-full", want prov-half and :205 expiry-first:
  first = "prov-full", want prov-half. ON (restored)
  → BOTH PASS (0.543s). The overlay is pinned.
  Restore verified: VM main.go 0 dirty lines after
  its cycles. A later 1-line diff at main.go:3268 is
  V-B2-489-RE's concurrent toggle (V-B2-489-RE
  TOGGLE: summariser disabled) — not mine, do not
  touch. HEAD moved 0f3750bb → 6f02a97 during
  verification; its files unchanged across that
  range (empty diff), so the evidence stands at
  current HEAD. Flip applied (parity.sh MATCH; Area
  1 now FULL 58 / PARTIAL 3 / DIVERGENCE 2,
  95.1%/97.5%; headline FULL 278 / PARTIAL 156,
  comparable 650, 42.8%/54.8%).
- S1-B2-542-SELECTOR DONE (~13:52 IST, commit
  2508342, 7 files, +261/−10). "cascade" now
  selectable: config allowlist entry
  (global_strategy + chain_strategies, both error
  lists), StrategyCascade in All (Parse/IsValid/
  picker), Describe entry, wiringHook identity case
  (Nadir precedent — a direct hook call never
  re-ranks under balanced); stale cascade.go header
  updated. Dashboard UI untouched (separate row).
  5 new tests: config loads global + chain
  "cascade" from real files; strategy Parse/All/
  Describe + hook-identity (balanced control proves
  the signals would re-rank); router end-to-end —
  real config file → loaded value →
  effectiveStrategy → cascade order + real dispatch
  serves subscription first. Proof: VM 5/5 PASS; PC
  cold 5/5 PASS (7/7 files hash-verified); PC `go
  build ./...` green; toggle — allowlist entry
  removed → config + router tests FAIL, restored
  clean. Verifier owed for the :542 flip. Root
  verified commit 2508342 (7 files). V-B2-542
  SPAWNED to verify :542 (cold PC + toggle) for the
  flip.
- V-B2-489-RE DONE (~13:54 IST) — FLIP TO FULL:
  :489 context-relay. Producers in the real request
  path: summariser closure main.go:3214 wired at
  :3309; quota signal RelayQuotaUsedFor main.go:
  3288, read per outcome in relayNote (relay.go:
  842-843); session store storeRelayWantIfPending
  relay.go:680 + loadRelayWant :736 + newRelayFor
  preload :762, called from router.go:4704/:6098
  keyed by req.CallerID. Cold PC at HEAD e2828ca (4
  files hash-verified = HEAD): 5/5 PASS —
  internal/router 2/2 (0.422s), cmd/codvian-router
  3/3 (0.616s). Toggle A — store off
  (storeRelayWantIfPending no-op): both engine tests
  FAIL ("request 2 got no handoff", "request 3
  (key-A) got no handoff") and the package-main
  production test FAILs — store pinned. Toggle B —
  summariser errors after dialling: production test
  FAILs on summary assertions (degraded handoff,
  "Unsummarised transcript" marker, lacks "user asks
  what 2+2 is") — summariser pinned. (A nil-wiring
  toggle doesn't compile — unused closure — so an
  error-return toggle after the dial was used; the
  mock still recorded the 1 call, isolating the
  failure to the summary output.) Toggle C — quota
  signal forced unknown: production test FAILs
  ("summariser calls = 0, want 1") — no want stored,
  trigger never fires — signal pinned. First attempt
  was blocked by another agent's toggled
  provider_v1.go on the shared PC copy (build
  failed, unrelated file); retried after the PC copy
  returned to HEAD and it passed cleanly. Toggle
  hygiene: all 3 cycles under flock /tmp/codvian.
  verify.lock (each ~40–100s); VM file backed up and
  restored per cycle, PC re-synced after each
  restore; zero residue — no toggle markers,
  relay.go/main.go clean vs HEAD. Row text keeps the
  remaining secondary gaps noted — chat-format
  injection only, fixed thresholds/TTL, budget gate
  undercounts handoff tokens. Flip applied (parity.
  sh MATCH; Area 1 now FULL 59 / PARTIAL 2 /
  DIVERGENCE 2, 96.7%/98.4%; headline FULL 280 /
  PARTIAL 154, comparable 650, 43.1%/54.9%).
- V-B2-754 DONE (~13:54 IST) — FLIP TO FULL:
  :754 Reasoning cache 2 h. Real request path:
  text-resume inject `req = e.injectReasoning(ctx,
  req)` at degrade.go:2100 in ChatStreamFrom, before
  delegating to the upstream resumer (:2102). DB
  tier: serve mode derives DataDir/reasoning_cache.
  db → wireReasoningCacheAtPath (main.go:2051 →
  OpenPersistentReasoningCache, :2060), fail-open to
  memory-only on open error, 1-min Sync ticker;
  called at main.go:4244. Cold on PC at HEAD 027e6f0:
  named test TestM66bTextStreamFromInjectsBeforeUpstream
  PASS (0.289s); full TestM66b file PASS (0.286s);
  G13 tests 4/4 PASS (0.412s). PC files hash-verified
  = HEAD (SHA256). Toggle 1 (flock, VM): inject call
  off → named test FAILS exactly as designed ("text
  resumer received reasoning <nil>, want restored
  text 'thinking about math'"); restored → PASS.
  Hash = HEAD after restore, zero residue. Toggle 2
  (flock, VM): DB tier off
  (OpenPersistentReasoningCache → memory-only) →
  TestG13ReasoningPersistsAcrossReload FAILS;
  restored → 4/4 PASS. Honest nuance:
  DeleteAndSyncDurable + ExpiredPrunedOnLoad pass
  even with the tier off (no-op/vacuous) — the
  reload test is the pin. Restore was once delayed
  seconds by a transient .git/index.lock from a
  concurrent commit; retried, hash = HEAD, tree
  clean. PC incident (resolved): mid-run another
  agent's in-flight toggle left provider_v1.go
  non-HEAD on the shared PC copy (build failed,
  `bare` unused); it restored itself to HEAD within
  ~60 s and the full-file run then passed. Row text:
  both "still missing" items now closed; temp-gate
  divergence stays recorded as-is. Flip applied
  (parity.sh MATCH; Area 6 now FULL 28 / PARTIAL 3,
  90.3%/95.2%; headline as above).
- V-B2-600-RE DONE (~13:55 IST) — FLIP TO FULL:
  B2-29 :600 (prefixed-model routing) verified at
  HEAD 8349ff3 (fix files provider_v1.go af1f5cf1dc14
  / provider_keys.go a220468aa3b1 identical to
  c9f7eff where it ran cold; G11 test file untouched
  by later commits). COLD (PC):
  TestG11PrefixedProviderNeverRerouted,
  TestG11ProviderAttemptAllowedUnit,
  TestG11MaxThreeDistinctProviders — 3/3 PASS, ok
  internal/router 0.371s, BUILD-EXIT:0. LIVE (PC,
  real server + fake upstreams): A prov_<id>/
  gate-model → 200 FAKE-OK; B fakeprov/gate-model →
  200 FAKE-OK; upstream bodies show bare "gate-model"
  (fakeprov 3 calls, deadprov 1); C dead-prefixed →
  502, otherprov NEVER called (no failover); D1 bare
  → 200; D2/D3 unknown/auto → 502, NOT pin-400
  (fail-open). TOGGLE (flock /tmp/codvian.verify.
  lock): revert bare-id rewrite → probe A 400
  "routing.only: no listed provider serves model"
  (exact pre-fix failure); restore → 200 FAKE-OK; PC
  hash af1f5cf1dc14 = HEAD, zero residue. (First
  revert attempt failed to build on unused `bare` —
  fixed with `prov, _, ok` in the reverted copy only;
  not a product bug.) Cleanup: probe processes
  stopped (0 listeners on 20274/19001/19002/19003),
  VM tree clean. Note: throwaway probe key
  crk_p3sQnKDjra6LPU7uBz8TEn36l783y8 (id gk_v600re01)
  added to the PC test config only. STALE FLAG
  CORRECTED: the verifier's note that 3b76fa9 left
  applyLearnedQuota() toggled OFF at main.go:2727/
  :2757 is OUTDATED — root verified at current HEAD
  35ea501: both call sites live, 0 TOGGLE-OFF
  markers (restore 8e4143c stands, re-verified by
  V-B2-09-CAP). No action. Flip applied (parity.sh
  MATCH; Area 2 now FULL 40 / PARTIAL 0 — the whole
  failover area is 100.0%/100.0%; headline FULL 281 /
  PARTIAL 153, comparable 650, 43.2%/55.0%).
- S1-WSL-FIX DONE (~14:08 IST) — WSL↔Windows path
  PARTIALLY FIXED: Windows now reaches the WSL Redis
  on 6379 via LOCALHOST forwarding (verified 2/2 from
  Windows PowerShell with a raw RESP probe: PING=+PONG,
  INFO redis_version:6.0.16 os:Linux 6.18.33.1-microsoft-
  standard-WSL2 — unambiguously the WSL Redis). Direct
  WSL-IP path STILL BLOCKED: 192.168.64.253:6379 fails
  2/2 with WSAEACCES ("access forbidden by access
  permissions" — Hyper-V firewall on the WSL switch);
  ICMP still dead both ways (ping "General failure").
  Diagnosis: WSL 2.7.8.0, Win 10.0.26300.9974; adapter
  "vEthernet (WSL (Hyper-V firewall))" Up, Win IP
  192.168.64.1/20, WSL eth0 192.168.64.253/20 UP,
  route 192.168.64.0/20 correct, HNS network "WSL
  (Hyper-V firewall)" type ICS; .wslconfig has no
  networkingMode (default NAT); no WSL-named firewall
  rules; all firewall profiles enabled. Repairs tried:
  (a) WSLService restart (LxssManager does not exist
  on this build — service is WSLService; stop accepted,
  auto-running again in 6 s, likely Docker Desktop
  holding it) — ping still dead; (b) disable+re-enable
  vEthernet (WSL) adapter — ping still dead. Localhost
  forwarding was dead before these repairs (S1-B2-REDIS-
  PC) and works 2/2 after them — revived by (a)+(b),
  exact step not isolated, NOT chased further. NO
  further resets done: mirrored mode (c) and firewall
  changes (d) NOT attempted — they risk breaking the
  working localhost path, and the Redis-arm test only
  needs 6379 reachable, which it now is (T-B2-738-PC
  testing against it). Helper probe left on PC:
  C:\Users\LOQ\redis-probe.ps1 (raw RESP PING/INFO,
  no install). Remaining known limits: direct-IP +
  ICMP blocked by the Hyper-V firewall — needs an
  interactive admin step if ever required (allow rule
  for the 192.168.64.0/20 subnet or switch firewall
  off), NOT done from SSH.
- V-B2-542 DONE (~13:59 IST) — FLIP TO FULL:
  B2-542 :542 4-tier fallback cascade. Selectable
  end to end: real config file → config.Load accepts
  global_strategy="cascade"; loaded value resolves
  via effectiveStrategy to "cascade"; engine
  intercepts it before the hook (router.go:2697-2703
  → applyCascadeOrder over live auto-catalog tiers);
  real dispatch serves subscription first
  ("from-sub"); balanced control keeps input order.
  Cold on PC at HEAD 8349ff3 (7/7 files
  hash-verified = HEAD): 5/5 PASS — config 2/2 (ok
  2.293s), router e2e 1/1 (ok 0.335s), strategy 2/2
  (ok 0.247s). Toggle under flock: allowlist entry
  removed (one-line diff in isStrategyName) → config
  2/2 FAIL + router e2e FAIL (Load rejects
  "cascade"); restored → all PASS. VM hash = PC hash
  = HEAD (e4f8e8da…), zero residue. Selector surface
  confirmed: StrategyCascade in All + Parse,
  Describe entry, wiringHook identity case
  (strategy.go, wiring.go). Row-text note: dashboard
  UI control is a separate row and stays out of this
  flip. Flip applied (parity.sh MATCH; Area 1 now
  FULL 60 / PARTIAL 1 / DIVERGENCE 2, 98.4%/99.2%;
  headline FULL 282 / PARTIAL 152, comparable 650,
  43.4%/55.1%). B2 remaining PARTIAL rows now 4:
  :524 (Arena ELO, S1-B2-524-SCOPE scoping), :738
  (Redis-on-PC attempt running), :750/:751
  (live-upstream limits, likely honest stays).
- S1-B2-REDIS-PC DONE (~13:58 IST) — SPLIT RESULT:
  (a) RESUME TEST COLD PASS ON PC:
  TestM66bChatStreamFromServesExactHitTrimmed PASS
  (0.00s, ok internal/degrade 0.285s) cold on the PC
  at HEAD 4d3494e; PC internal/degrade package 22/22
  files hash-verified = HEAD. The test itself is
  already committed (8c06111, critic). This closes
  the SECOND :738 gap (resume exact-serving
  untested) — only the Redis arm remains.
  (b) REDIS-ON-PC: BLOCKED, exact evidence. WSL2
  networking is dead BOTH ways on this PC: Windows
  → WSL IP 192.168.64.253 TCP 6379/6380 + ICMP all
  fail (ping 11050), WSL → Windows 192.168.64.1 ping
  100% loss, localhost forwarding dead for 6379 and
  6380. `wsl --shutdown` + restart did NOT revive
  it (same IP 192.168.64.253, same failures).
  Redis 6.0.16 IS up inside Ubuntu-22.04 (systemd
  service, PONG) and a second instance on 6380 bind
  0.0.0.0 answered PONG from inside WSL — both
  unreachable from Windows regardless. Docker
  Desktop 29.8.0 launched (WMI session 0) and the
  daemon is UP (`docker ps` works), but EVERY image
  pull fails: docker-credential-desktop AND
  docker-credential-wincred both fail under SSH with
  "A specified logon session does not exist. It may
  already have been terminated." Tried: credsStore
  removed, credsStore=wincred, custom context
  pc-redis → same error every time; `ctr` is not
  present in the docker-desktop distro. No Redis is
  reachable from Windows Go tests. Next options:
  (1) native Windows Redis (MSOpenTech 3.2 zip) —
  follow-up attempt; (2) fix the WSL virtual switch
  (admin / Windows Firewall — needs an interactive
  session); (3) run the Docker pull from an
  interactive logon session (the desktop credential
  helper needs one). Docker config restored to
  credsStore=desktop after the attempts; Docker
  Desktop left running; helper .ps1 files left in
  C:\Users\LOQ (docker-start, fix-redis-creds,
  set-wincred, set-desktop). No code or test files
  changed by this agent; nothing to commit except
  this note.
- S1-B2-REDIS-PC DONE (~14:00 IST) — SPLIT RESULT
  (commit 371d128, note only; no code changed).
  (1) Resume test cold PASS on PC:
  TestM66bChatStreamFromServesExactHitTrimmed PASS
  (ok internal/degrade 0.285s) at HEAD 4d3494e; PC
  internal/degrade package 22/22 files hash-verified
  = HEAD. Test already committed (8c06111). This
  closes :738's second gap (resume exact-serving);
  only the Redis arm remains. (2) Redis-on-PC:
  BLOCKED. WSL2 networking dead both ways
  (Windows→WSL IP 192.168.64.253 TCP+ICMP fail,
  WSL→Windows ping 100% loss, localhost forwarding
  dead for 6379/6380); `wsl --shutdown` did not
  revive it. Redis 6.0.16 IS up inside Ubuntu-22.04
  (systemd service, PONG) and a 6380 bind-0.0.0.0
  instance answered PONG from inside WSL — both
  unreachable from Windows. (3) Docker path also
  exhausted: Docker Desktop 29.8.0 launched (WMI
  session 0), daemon UP (`docker ps` works), but
  every image pull fails — docker-credential-desktop
  AND docker-credential-wincred both fail under SSH
  with "A specified logon session does not exist."
  Tried credsStore removed, wincred, custom context
  `pc-redis` — same error; `ctr` absent from
  docker-desktop distro. (4) Next options: (a)
  native Windows Redis (MSOpenTech 3.2 zip); (b) fix
  the WSL virtual switch (admin/Firewall — needs an
  interactive session); (c) run the Docker pull from
  an interactive logon session. Docker config
  restored to credsStore=desktop; Docker Desktop left
  running; helper .ps1 files left in C:\Users\LOQ.
  Root verified commit 371d128 (note only).
  S1-B2-REDIS-PC2 SPAWNED for path (a) — native
  Windows Redis 3.2; if that fails, :738 is decided
  on the VM-proof + SQLite + resume-test evidence
  under the general rule.
- S1-B2-524-SCOPE DONE (~14:00 IST) — :524 Live
  intelligence (Arena ELO + models.dev): scope
  report. WHAT EXISTS: full build already landed in
  commit a5d18f1 (1510-line intelligence.go + 40
  modelsync.go): ArenaSyncer (24h, wulong primary →
  HF fallback), models.dev tier derivation, priority
  TaskFit store (user_override > arena_elo >
  models_dev_tier), `arena_elo` config block, and
  real wiring — store always built at startup, Arena
  Start() in the watcher, RebuildTier from the
  models.dev loop, scoring hook sets TaskFitScore/
  TaskFitKnown in the ranking closure (main.go:2900),
  FactorTaskFit reads it first (scoring16.go:598-600).
  MODELS.DEV SOURCE: real — https://models.dev/api.json
  (public, no auth; prices + context windows per
  model; deprecated skipped; state file
  models-dev-sync.json 0600); second source LiteLLM
  cost map. OFF by default because local-first: no
  network unless the operator opts in; on =
  `models_dev: {enabled: true}` in config. ARENA
  SOURCE: real but indirect — Arena AI itself has NO
  public API; code uses community mirror api.wulong.
  dev (free, no auth; `.../v1/leaderboard?name=text`,
  "text"+"code" boards, ELO→task-fit 0.4+0.58·norm →
  [0.4,0.98], 7d row TTL) with HF fallback
  `lmarena-ai/leaderboard-dataset` via datasets-server.
  CAVEAT: a live fetch of the documented wulong
  endpoint returned HTTP 404 just now — needs a live
  probe; the HF fallback exists in code for exactly
  this case. Do not flip on the mirror's docs alone.
  WHAT IS MISSING: tests — ZERO test files anywhere
  reference ArenaSyncer/RebuildTier/NewIntelligence/
  TaskFit; the whole feature + wiring is unpinned.
  Also unverified: the claimed per-request category
  hook (only CategoryDefault is read in the ranking
  closure; no second TaskFit call found). HONEST
  VERDICT: FULL-able. Row text is STALE ("no Arena
  ELO, no ELO/tier signal in scoring" — both exist in
  code + wiring), but PARTIAL is still honest until
  test-pinned + live-proven. BUILD SIZE: S — no
  production code needed if the probe passes. Steps:
  (1) live-probe wulong + HF, (2) pinning tests
  (syncer vs httptest stand-ins — URL is
  configurable; tier derivation; TaskFit priority;
  scoring-hook consumption), (3) cold PC run + toggle
  (sync off → TaskFitKnown false, tag-Jaccard
  fallback), (4) verifier re-run → flip + correct row
  text. IF PROBE FAILS: M — re-point primary to the
  GitHub raw JSON (`oolong-tea-2026/arena-ai-
  leaderboards`, same mirror, documented) or HF-first,
  then re-verify; do NOT invent a source. No code
  written, no PARITY.md edits, no commits made. Root
  verified: a5d18f1 exists, zero test references
  (grep empty). T-B2-524-TEST SPAWNED (live probe +
  pinning tests + cold PC + toggle).
- V-B3-678 PAUSED — PARTIAL FINDINGS (2026-11-20
  ~14:35 IST): stopped cleanly per the B2-GATE-
  FIRST order. PC restored: all 7 files hash =
  HEAD (jwt.go, oauth.go, jwt_test.go, oauth_test.go,
  oauth_api.go, server.go, guard.go); temp e2e test
  file deleted from VM tree; nothing committed,
  PARITY.md untouched. COLD (PC, HEAD 5470524):
  internal/oauth 25/25 PASS, ok 1.293s. TOGGLE
  (flock, 44s): inverted HMAC check in VerifyJWT →
  10 FAIL / 15 PASS; restore → hash = HEAD, re-run
  25/25 PASS. HTTP e2e funnel NOT proven: temp
  test's 1st run failed on its own parse bug
  (create-client response uses "id", not
  "client_id"); 2nd run aborted by the stop order;
  funnel wiring code-reviewed only (authorized→
  oauthTokenValid server.go:1976, requireScope→
  oauthTokenScopes, adminScopeOK→oauthTokenScopes,
  callerKeyID→"oauth:<id>", 6 /oauth routes
  registered + guard_test.go asserts them).
  VERDICT: none — DO NOT flip :678 on this alone.
  ON RESUME (B3's turn): recreate the temp e2e test
  (register→authorize PKCE→exchange→Bearer JWT on
  /v1/models 200 / garbage 401 / narrow-JWT admin-
  route 403 / revoke→401 / refresh→200) and re-run
  it before deciding.
- B2 GATE FIRST (Raunak, voice call ~14:34 IST):
  speed up the 12-min gate — B3 work STOPPED, all
  agents on the B2 gate, B2 pushes first, then B3
  starts. V-B3-678 told to stop (interrupt), restore
  any toggled PC files to HEAD, report partial
  findings, end; resumes at B3's turn. S1-B2-GATE
  told the PC is now exclusive. Honest note: the
  gate is one sequential full-suite run — more agents
  cannot shorten its ~12 min; the speedup is an
  exclusive PC + push steps firing the moment it
  goes green. Recorded in ~/MEMORY.md.
- PUSH SCOPE MEASURED (2026-11-20 ~14:48 IST):
  local tracked 1,518 files vs public 120 files —
  1,407 only-local + 83 differing + 9 only-public
  (LICENSE, CODE_OF_CONDUCT, CONTRIBUTING, ROADMAP,
  SECURITY, issue/PR templates — these stay on main
  through a merge). Browser-only file-by-file
  upload is NOT practical at this scale (~1,490
  files; web UI cannot preserve directory structure
  for a flat multi-file upload). PROPOSED to
  Raunak: one-time GitHub token (fine-grained, this
  repo only, Contents: write) via secure entry —
  then terminal git pushes a branch + PR + merge
  via browser (NOT the GitHub connector, which
  stays banned). Awaiting his decision.
- GATE GREEN + B1+B2 PUSH STARTING (2026-11-20
  ~14:46 IST): B2 final gate PASS 4,474 / FAIL 0 /
  SKIP 8; 48 packages ok / 0 FAIL / 3 no-test-files;
  run 14:35:57→14:43:28 IST (~7.5 min), detached
  WMI PID 23480, HEAD c876704; LossyExactHit PASS,
  breaker stale-degraded PASS (no flake); the 15
  "FAIL" lines in the log are expected output inside
  passing release-verification tests (tampered/
  unsigned fixtures). Log: hidden_files/gotest-b2-
  final-gate-2026-11-20.log (verified by root:
  TEST-EXIT=0). README updated in 9d00154 (status
  43.8%/55.4% + What's new 2026-11-20). Browser
  push task starting: branch → PR → merge into
  main, browser-only, never connector; STOP if a
  sudo-mode code is requested.
- V-LOSSY PASS — B2 LAST BLOCKER CLEARED
  (2026-11-20 ~14:33 IST): LossyExactHit Option-A
  fix (bfcda4a) verified by a separate tester:
  6/6 files = HEAD; cold PC TestChatCompletions-
  LossyExactHitStillWorks PASS (2nd request got
  cache hit, exact layer) + FULL internal/server
  green 0 FAIL / 1,324 PASS / 0 SKIP, ok 117.741s;
  toggle (feature OFF) → test FAILS, restore →
  hash = HEAD, test PASSES; pin tests + TestStrategiesFeedEmpty PASS. B2 IS 44/44:
  42 done + Lossy fix verified; :750/:751 honest
  stays (dispositioned, not blockers). S1-B2-GATE
  SPAWNED — final B2 gate, full suite on PC (~12
  min); GREEN gates the B1+B2 browser push (README
  update first). 30-min target (14:34 IST) will be
  MISSED by the gate — told to Raunak honestly.
  V-B3-678 still running.
- :669 FLIP TO FULL (2026-11-20 ~14:30 IST, commit
  be803a6, parity.sh MATCH): V-B3-669 — dashboard
  authentication MISSING→FULL (cold PC 9/9 PASS,
  toggle gate-off → 3 FAIL, restore → 9/9; Argon2id
  0600, 12h HttpOnly cookie, 5-fail/60s throttle).
  Parity now FULL 285 / PARTIAL 150 / MISSING 214,
  43.8%/55.4%; Area 4 FULL 18 / MISSING 3, 78.3%/
  82.6%. B3's first flip. V-LOSSY + V-B3-678 still
  running.
- VPN-OFF TEST (Raunak did it, 2026-11-20 ~14:30
  IST): ping 192.168.64.253 now 4/4 Reply, 0% loss
  — VPN WAS blocking it. But TCP 6379 still refused
  from Windows AND from inside WSL to its own IP
  ("Connection refused") — Redis listens on
  127.0.0.1 only, not the WSL IP. CORRECTION to the
  FIX2 root cause: the HNS-switch-stuck theory was
  measured WITH VPN ON — the VPN, not the switch,
  was the ICMP blocker; ping passes now. Fix in
  progress: bind 0.0.0.0 + protected-mode no in
  /etc/redis/redis.conf + restart (root, via SSH).
- LOSSY-A DONE + V-LOSSY + B3 OVERLAP STARTED
  (2026-11-20 ~14:26 IST): S1-LOSSY-A COMMITTED
  bfcda4a (6 paths): Option A — exact cache keys on
  the pre-compression request (PreCompression snapshot
  on ChatRequest; Compression in cache KeyInput;
  degrade.scopedKey uses snapshot when present; 2 pin
  tests). TestChatCompletionsLossyExactHitStillWorks
  PASSES on VM and PC; full internal/server package
  GREEN on PC (ok 119.063s, 0 FAIL). Extra fix in the
  same commit: TestStrategiesFeedEmpty (stale 34-name
  list, "cascade" added by 2508342 was missing).
  V-LOSSY SPAWNED (separate tester, builder≠tester)
  — cold PC + toggle; its verdict gates the B2 PASS.
  V-B3-669 (dashauth :669, T-B304 tests 7347a4b) and
  V-B3-678 (OAuth/JWT :678, T-B307 tests e10a888)
  SPAWNED in parallel — PACE OVERRIDE overlap (PC-
  copy collision guard no longer applies: LOSSY-A's
  PC run is done). 30-min B2 target (14:34 IST):
  V-LOSSY verdict + final B2 gate (~12 min) may land
  AFTER 14:34 — report honestly, do not claim it fits.
- WSL-FIX2 DONE (2026-11-20 ~14:26 IST, commit
  0560e04): localhost 127.0.0.1:6379 PONG + Redis
  6.0.16 re-verified after restart cycle — Redis work
  unblocked. Direct-IP 192.168.64.253 STILL DEAD
  (TCP + ping fail both ways, after WSLService stop/
  start + distro relaunch). ROOT CAUSE: HNS ICS
  network "WSL (Hyper-V firewall)" passes ZERO
  traffic, even to its own gateway 192.168.64.1
  (100% loss) — virtual switch stuck. Corrections:
  adapter is "vEthernet (WSL (Hyper-V firewall))";
  no LxssManager service — WSLService refuses
  Restart-Service even elevated. EXACT STEPS FOR
  RAUNAK (he said 14:05 IST he is ready to do them
  himself — relay NOW): (a) disconnect Proton VPN
  once, re-test ping 192.168.64.253; (b) elevated
  PowerShell: Get-HnsNetwork | Where-Object {$_.Name
  -like "*WSL*"} | Remove-HnsNetwork, then
  wsl --shutdown, relaunch Ubuntu, re-test; (c)
  alternative: networkingMode=mirrored in
  C:\Users\LOQ\.wslconfig (can conflict with Proton
  VPN). Untouched: native Redis 3.2.100 on 6380,
  Docker Desktop running, no personal files, no
  installs. Recorded in ~/MEMORY.md.
- WSL-FIX STALLED + CLOSED (Raunak, voice call
  ~14:20 IST — "sab ruk gaye kya?"): verified live
  at 14:20 IST — S1-LOSSY-A RUNNING, last activity
  30s ago (fix work visible in tree: key.go/
  degrade.go/client.go/server.go/strategies_test.go
  modified, b2_lossy_exact_key_test.go written); NOT
  everything stopped. S1-WSL-FIX was RUNNING but
  silent 673s (~11 min, past the 8-min STALLED
  threshold) and did not answer the root's status
  request — CLOSED per the stall rule; its handoff
  confirms cancelled with no result. Its history
  shows several failed execs + one background session
  still running (proc_6cda04007419) — it was likely
  stuck waiting on a hung PC command. S1-WSL-FIX2
  SPAWNED (narrower: confirm current state per path
  — localhost 6379 already works; try ONE quick fix
  for the direct-IP path or report the EXACT
  interactive step for Raunak; report at least every
  5 min). If FIX2 also stalls, it is marked BLOCKED,
  never respawned again.
- S1-WSL-FIX2 DONE (~14:33 IST) — direct-IP path
  STILL DEAD, localhost path CONFIRMED WORKING.
  (1) 127.0.0.1:6379 (localhost forwarding) WORKS:
  PONG + INFO redis_version:6.0.16, os:Linux WSL2 —
  re-verified AFTER the restart cycle, so the Redis
  work stays unblocked; nothing further needed for
  it. (2) Direct IP 192.168.64.253:6379 STILL DEAD
  both ways after a full WSLService stop/start cycle
  + distro relaunch (TCP fail, ping fail status
  11050; WSL→gateway 192.168.64.1 ping 100% loss).
  (3) ROOT CAUSE found: the HNS ICS network "WSL
  (Hyper-V firewall)" is Up with correct routes
  (eth0 192.168.64.253/20, default via 192.168.64.1)
  but passes ZERO traffic even to its own gateway —
  the virtual switch is stuck. Adapter is named
  "vEthernet (WSL (Hyper-V firewall))", NOT
  "vEthernet (WSL)" — earlier agents looked for the
  wrong name. (4) Service model on this PC:
  WSLService (NO LxssManager service exists);
  Restart-Service fails even elevated
  (StopServiceFailed); `sc stop` only takes effect
  after `wsl --shutdown` and the service
  auto-restarts immediately. SSH session IS elevated
  (Administrator=True), so this is NOT a permissions
  problem. (5) .wslconfig exists ([wsl2]
  memory=10GB, processors=6, swap=6GB), NAT mode, no
  networkingMode. (6) ONE quick fix tried (the
  service-restart cycle) — did NOT fix the direct
  path; no second risky step taken (HNS reset could
  kill the working localhost path). EXACT STEPS FOR
  RAUNAK (interactive, in order): (a) no-risk test —
  disconnect Proton VPN once and re-test ping
  192.168.64.253 (VPN is Up and a known suspect for
  WSL2 subnets); (b) the fix — elevated PowerShell:
  `Get-HnsNetwork | Where-Object {$_.Name -like
  "*WSL*"} | Remove-HnsNetwork`, then `wsl
  --shutdown`, relaunch Ubuntu, re-test (recreates
  the WSL virtual switch); (c) alternative — add
  `networkingMode=mirrored` under [wsl2] in
  C:\Users\LOQ\.wslconfig then `wsl --shutdown`
  (can conflict with Proton VPN). Untouched: native
  Redis 3.2.100 still on 6380, Docker Desktop
  running, Docker config, no personal files, no
  installs.
- PUSH B1+B2 ON B2 PASS (Raunak, voice call ~14:18
  IST): the moment B2 is fully done (last 2 items
  finished, everything verified, everything OK), push
  B1+B2 to GitHub via the browser — proper PR, full
  push, then merge. Supersedes the per-stage push
  unit for this push: B1+B2 go first, B3/B4 push
  later at their turn. Recorded in ~/MEMORY.md.
  Push checklist (preconditions, in order): (1)
  S1-LOSSY-A commits the fix; (2) V-LOSSY (separate
  tester) passes cold PC + toggle; (3) final B2 gate
  (full suite, PC) GREEN; (4) README updated to match
  PARITY.md exactly (currently 43.7%/55.2%, FULL 284
  — README still shows older numbers, update owed at
  push time); (5) browser PR → merge into main
  (never the GitHub connector; STOP if a sudo-mode
  code is requested — codes only from Raunak
  dictating). Local/public history divergence still
  stands (public 5bc909b not an ancestor) — the
  browser upload route must handle it.
- PACE COMPLAINT (Raunak, voice call ~14:15 IST —
  angry): "kitna time lagega, ek hi block pe tab se
  pade ho, B2 hi nahi bana pa rahe, to B9 tak kaise
  pahunchega." Honest answer: B2's BUILD was done in
  Stage 1 (morning, 07:48 IST); the time since the
  12:33 IST block-wise redirect went into
  VERIFICATION — 42/44 done, only :750/:751 (honest
  stays) + LossyExactHit left. S1-LOSSY-A state
  verified 14:15 IST: fix + test file written
  (untracked internal/degrade/b2_lossy_exact_key_
  test.go; production files key.go/degrade.go/
  client.go/server.go modified in flight), waiting
  on its detached PC run (sleep 300 in progress;
  last activity 105s ago). S1-WSL-FIX quiet 397s —
  status requested by root (report diagnosis +
  repairs so far + the exact step for Raunak, if
  any). PACE OVERRIDE recorded in ~/MEMORY.md:
  overlap the next block's verification with the
  current block's tail; B3 verifiers spawn the moment
  LOSSY-A's PC run lands (NOT before — the PC copy
  currently holds LOSSY-A's uncommitted code and a
  verifier syncing HEAD files would break its run).
  B3/B4 builds are already done (Stage 1) — only
  verification remains, so their pace is faster than
  B2's build+verify; B5–B9 still need build+test+
  verify (days, not one night) — told to him plainly.
- V-B2-738 DONE (~14:14 IST) — FLIP TO FULL:
  :738 Persistence tiers. Cold on PC at HEAD 1717c4c
  (4/4 key files hash-verified = HEAD; TCP
  127.0.0.1:6379 reachable from Windows):
  TestB2RedisLiveSetGetSyncReopen PASS (0.01s,
  package ok 0.300s) — real Redis 6.0.16 (WSL,
  localhost forwarding); open → Set → Get hit →
  Sync → Close → reopen → hit again. Toggle under
  flock (env-only, zero residue): dead port
  127.0.0.1:6399 → FAIL with "connection refused"
  (not skip/pass); restore live addr → PASS; PC hash
  = HEAD hash after restore. Supporting evidence
  confirmed by re-run cold on PC: SQLite arm 32/32
  PASS (cache 20 + degrade 4 + server 5 + cmd 3;
  TestM62* 10 + TestM62b* 22); resume exact-serving
  PASS (TestM66bChatStreamFromServesExactHitTrimmed,
  0.00s). Row's "VM-only / resume untested" text is
  now stale on both counts. Honest caveats for the
  flip text: direct WSL IP 192.168.64.253:6379 still
  dead (WSL2 networking dead both ways; S1-WSL-FIX
  working on it); native Windows Redis 3.2.100
  excluded — product minimum Redis ≥ 4.0 (variadic
  HSet unsupported on 3.2). Flip applied (parity.sh
  MATCH; Area 6 now FULL 29 / PARTIAL 2, 93.5%/
  96.8%; headline FULL 284 / PARTIAL 150, comparable
  650, 43.7%/55.2%). B2 remaining: :750/:751 (honest
  STAYS on live-upstream limits) + LossyExactHit fix
  (S1-LOSSY-A still running; V-LOSSY spawns on its
  commit). 30-min target: 14:34 IST.
- T-B2-738-PC DONE (~14:11 IST) — :738 Redis arm
  proven cold on PC against a real Redis. Committed
  ac25d6c (test file only, 68 insertions). (1) 6379
  path that works: 127.0.0.1:6379 from Windows
  (localhost forwarding) — PONG 2/2, redis_version:
  6.0.16, os:Linux 6.18.33.1-microsoft-standard-WSL2,
  executable:/usr/bin/redis-server. Direct WSL IP
  192.168.64.253:6379 still dead (TCP fail). (2)
  6380 is the old native server — redis_version:
  3.2.100, os:Windows, D:\redis-3.2.100\redis-server.
  exe; not used. redis-cli is not on PATH in SSH
  sessions; found at D:\redis-3.2.100\redis-cli.exe.
  (3) PC package verified: 35/35 files in
  internal/cache hash-match HEAD (key.go is HEAD on
  the PC; the VM tree's uncommitted LossyExactHit
  Compression change in key.go was left untouched) +
  test file hash = VM file. (4) Cold PASS:
  TestB2RedisLiveSetGetSyncReopen PASS (0.01s),
  package ok 0.325s, CODVIAN_TEST_REDIS_ADDR=
  127.0.0.1:6379 (Set → Get hit → Sync → Close →
  reopen → Get hit again → cleanup). (5) Toggle
  FAILs as required: dead port 127.0.0.1:6399 → FAIL,
  dial tcp 127.0.0.1:6399: connectex: No connection
  could be made because the target machine actively
  refused it, exit 1 — not a silent pass or skip.
  (6) Header comment fixed (per follow-up): names
  WSL Redis 6.0.16 on 127.0.0.1:6379, product minimum
  Redis ≥ 4.0, 3.2 explicitly excluded — no longer
  documents the wrong server. (7) Only remaining
  dirty file is internal/cache/key.go (S1-LOSSY-A's
  in-flight work, not mine). Row text now stale in
  TWO ways ("no Redis is reachable from the PC" —
  false now; "Resume exact-serving also untested" —
  tested, cold PASS on PC) — verifier records the
  corrected flip text. Root verified commit ac25d6c.
  V-B2-738 SPAWNED (independent verifier, cold PC +
  toggle; flip owed on its PASS).
- V-B2-524 DONE (~14:10 IST) — FLIP TO FULL:
  B2-14 :524 Live intelligence (Arena ELO +
  models.dev). Cold on PC at HEAD 158c8ac (3/3
  files hash-verified = HEAD): 9/9 PASS —
  modelsync 6/6 (ok 12.365s), scoring 3/3 (ok
  0.292s). Toggle under flock: fetch order flipped
  to wulong-first → TestB2_524_HFFirst_WulongDead
  FAILS (`source with both live = "wulong", want
  huggingface`, test line 139); restored → PASS (ok
  6.335s). VM hash = PC hash = HEAD (c3de2670…),
  zero residue. Row text was stale ("no Arena ELO,
  no ELO/tier signal in scoring" — both exist since
  a5d18f1). Corrected flip text: Arena ELO sync live
  (HF-primary since 2026-11-20 — live probe: wulong
  404 dead on text/code/overall/coding, HF
  datasets-server 200 with 426 overall / 421 coding
  rows; models.dev api.json 200, 6.78 MB), models.dev
  tier derivation, TaskFit priority (user_override >
  arena_elo > models_dev_tier), scoring hook sets
  TaskFitScore/TaskFitKnown in the ranking closure
  (main.go:2900) and FactorTaskFit reads it first;
  models.dev sync opt-in OFF by default is deliberate
  (local-first, no network unless operator opts in).
  CategoryDefault-only lookup in the ranking closure
  is deliberate and documented in-tree (no
  per-request category exists there; the per-request
  hook layers one when present). Tests (9):
  TestB2_524_HFFirst_WulongDead, TestB2_524_
  WulongFallback_WhenHFDead, TestB2_524_SyncOff_
  TouchesNoNetwork, TestB2_524_TaskFitPriority,
  TestB2_524_TierDerivation, TestB2_524_ParseAnd_
  Transform (internal/modelsync) + TestB2_524_
  FactorTaskFitKnownWins, TestB2_524_FactorTaskFit_
  UnknownFallsBack, TestB2_524_FactorTaskFitKnown_
  InvalidFallsThrough (internal/scoring). No
  PARITY.md edits, no commits made by the verifier.
  Flip applied (parity.sh MATCH; Area 1 now FULL 61
  / PARTIAL 0 / DIVERGENCE 2 — 100.0%/100.0%; headline
  FULL 283 / PARTIAL 151, comparable 650,
  43.5%/55.2%). B2 remaining PARTIAL rows now 3:
  :738 (T-B2-738-PC running vs WSL Redis 6379),
  :750/:751 (live-upstream limits, honest stays).
  LossyExactHit fix (S1-LOSSY-A) still running.
- T-B2-524-TEST DONE (~14:07 IST) — commit 158c8ac,
  3 files (+581/−51). (1) Live probe (VM,
  2026-11-20): wulong 404 "Leaderboard not found" on
  text, code, overall, coding (dead as configured);
  HF datasets-server 200 — 426 overall / 421 coding
  rows, exact parser shape; models.dev api.json 200,
  6.78 MB; GitHub oolong-tea-2026/arena-ai-
  leaderboards exists, latest snapshot 2026-11-19
  (not used — needs date discovery, HF is the same
  dataset already parsed). (2) Production change
  (only one allowed): HF-first re-point —
  fetchLeaderboards now tries HF first, wulong mirror
  as fallback (split into fetchHFBoards/
  fetchWulongBoards + usableBoards); hfFilterURL
  const→var so tests can point at a stand-in; probe
  evidence recorded in the header/HF/DefaultArenaURL
  comments. (3) Tests (9 new):
  TestB2_524_HFFirst_WulongDead (both-live→
  huggingface, wulong-dead→huggingface, both-dead→
  error + last good kept), TestB2_524_WulongFallback_
  WhenHFDead (source wulong), TestB2_524_SyncOff_
  TouchesNoNetwork (disabled = zero network calls),
  TestB2_524_TaskFitPriority (override > arena >
  tier, variant hop, expiry, invalid rows),
  TestB2_524_TierDerivation (premium/standard/fast/
  budget + rebuild prunes), TestB2_524_ParseAnd_
  Transform (ELO→[0.4,0.98], bad row fails whole
  parse, vision/empty→0), + 3 FactorTaskFit tests in
  internal/scoring (known wins incl. known-0;
  unknown→Jaccard/0.5; NaN/out-of-range→falls
  through). (4) Cold PC at working tree (3/3 files
  hash-verified = VM): 9/9 PASS (modelsync 6/6,
  12.385s; scoring 3/3). VM also 9/9 PASS. (5)
  Toggle proven: wulong-first order →
  TestB2_524_HFFirst_WulongDead FAILS ("source
  wulong, want huggingface"); restored → PASS.
  Sync-off → no rows, TaskFit ok=false; TaskFitKnown=
  false → tag-Jaccard. (6) Verifier owed for the
  :524 flip — row text is stale ("no Arena ELO, no
  ELO/tier signal in scoring") and needs correcting
  on flip to record HF-primary + the 2026-11-20
  probe. (7) No other production files touched; no
  residue (temporary toggle edit reverted, diff
  verified = the intended re-point only). (8) Note:
  the HF-dead path costs ~6s per sync in 500-retries
  (3 attempts × 0.5/1.0/1.5s × 2 categories) —
  pre-existing retry behaviour, unchanged. Root
  verified commit 158c8ac (3 files, +581/−51).
  V-B2-524 SPAWNED (independent verifier, cold PC +
  toggle; flip owed on its PASS).
- S1-B2-REDIS-PC2 DONE (~14:06 IST) — BLOCKED:
  native Windows Redis is up but TOO OLD for our
  code. Native Redis MSOpenTech 3.2.100 UP:
  redis-server.exe PID 22660 (started 20-Nov-26
  2:03:56 PM), port 6380, PONG, INFO
  redis_version:3.2.100, os:Windows. Files at
  D:\redis-3.2.100 (zip D:\redis-3.2.100.zip,
  5,224,077 bytes, from MSOpenTech win-3.2.100
  release). Port 6379 was already occupied by the
  WSL Redis 6.0.16 (Windows listener PID 22308;
  Windows redis-cli INFO returns
  redis_version:6.0.16, os:Linux 6.18.33.1-
  microsoft-standard-WSL2) — first native launch on
  6379 failed to bind. Native runs on 6380 instead.
  Test written, uncommitted:
  internal/cache/b2_redis_live_test.go —
  TestB2RedisLiveSetGetSyncReopen, env-gated on
  CODVIAN_TEST_REDIS_ADDR (skips when unset, so the
  VM gate stays green); flow open → SetForModel
  (vector API, write-through) → GetForModel hit →
  Sync → Close → reopen same prefix → GetForModel
  hit again → cleanup via deleteAll. PC copy
  hash-verified = HEAD (4 files' SHA256 match the VM).
  Cold PC run — FAIL (EXIT:1, 0.00s):
  b2_redis_live_test.go:36: SetForModel: ERR wrong
  number of arguments for 'hset' command. Root cause
  proven directly on the native server: Redis 3.2's
  HSET takes exactly one field–value pair; variadic
  multi-field HSET arrived in Redis 4.0. Direct
  redis-cli proof on 6380: HSET proofkey f1 v1 f2 v2
  → ERR; HSET proofkey f1 v1 → 1; HMSET proofkey f1
  v1 f2 v2 → OK (proof key deleted afterwards). Only
  ONE HSet call site: internal/cache/semantic_redis.
  go:455 (writeRow). No other 4.0+ commands in the
  Redis tier (SCAN exists since 2.8). Per the brief
  it stopped — no commit, no production change, no
  other installs, no WSL/firewall/Docker changes.
  Left in place: test file uncommitted; native Redis
  still running on 6380 (PID 22660); WSL Redis
  untouched on 6379; Docker config untouched.
  ROOT DECISION: NO HMSet downgrade — product
  minimum is Redis ≥ 4.0 (2017+); HMSET is
  deprecated and downgrading production code for a
  dead 2016 Windows build is wrong. Fast path
  instead: the WSL Redis 6.0.16 on 6379 is reachable
  from Windows (INFO proof) — run the existing test
  against it. T-B2-738-PC SPAWNED (prove 6379 paths
  ×2, run test cold vs 6379, toggle vs dead port
  6399, commit test file if PASS + fix its stale
  header comment to name the WSL 6.0.16 server).
  WSL-FIX sent the new evidence (check whether its
  repair revived the 6379 path; do not break a
  working path). Root verified: b2_redis_live_test.
  go untracked in tree; HSet single call site :455;
  T-B2-524-TEST has already written
  b2_524_intelligence_test.go +
  b2_524_taskfit_test.go (untracked, in flight).
- BUILDER ≠ TESTER RULE (Raunak, voice call ~14:05
  IST): the agent that builds a feature NEVER tests
  it — testing is always a separate agent, on his PC,
  at full load, as fast as possible; as soon as a
  build finishes, a different tester picks it up.
  Recorded in ~/MEMORY.md. Applied to the 30-min
  run: S1-LOSSY-A's brief combined build+cold-test —
  corrected: its cold-PC run is a smoke check only;
  a DEDICATED tester (V-LOSSY) spawns the moment it
  commits and does the real cold-PC + toggle
  verification (the earlier "gate-only validation"
  plan is dropped). T-B2-524-TEST is already a
  tester-only agent; S1-B2-REDIS-PC2 tests the Redis
  arm it did not build (9149db0 built it). WSL-FIX
  step for Raunak: NOT yet known — agent still
  diagnosing (verified live 14:05 IST, last activity
  5s); the exact click goes to him the moment the
  agent reports it, nothing invented.
- 30-MIN TARGET (Raunak, voice call ~14:04 IST):
  B2 complete within 30 minutes (~14:34 IST). Fleet
  at target start: 4 agents live — T-B2-524-TEST
  (probe + pins), S1-B2-REDIS-PC2 (native Windows
  Redis), S1-WSL-FIX (repair WSL networking),
  S1-LOSSY-A (option A fix). Pipeline tightened:
  verifier spawns the moment each builder handoff
  lands; LossyExactHit validated by the final B2
  gate instead of a separate V cycle; :738 falls
  back to the recorded deviation (VM Redis proof +
  SQLite + resume test) if both PC Redis paths fail;
  :750/:751 already dispositioned as honest STAYS.
  Final B2 gate (full suite, PC) runs after the
  last flip. Risk to the 30-min target: WSL repair
  needing Raunak's interactive click.
- WSL/REDIS PROBLEM (Raunak, voice call ~14:01 IST
  — "kya problem hai, theek kar do, speed badhao"):
  WSL2 networking dead BOTH ways on the PC —
  Windows→WSL IP 192.168.64.253 TCP+ICMP fail,
  WSL→Windows ping 100% loss, localhost forwarding
  dead for 6379/6380; `wsl --shutdown` did not
  revive it. Redis 6.0.16 IS up inside Ubuntu-22.04
  (systemd service, PONG) and a 6380 bind-0.0.0.0
  instance answered PONG from inside WSL — both
  unreachable from Windows. Docker pulls also fail
  under SSH (credential helpers need an interactive
  logon session: "A specified logon session does not
  exist"). In plain words: the virtual network cable
  between Windows and WSL is broken — Redis is
  healthy inside Linux, but Windows cannot reach it
  at all, so the Redis-arm test cannot run from
  Windows. S1-WSL-FIX SPAWNED to repair it
  (diagnose adapter/firewall/.wslconfig → restart
  LxssManager → reset vEthernet (WSL) → mirrored
  mode → verify ping + TCP + redis-cli PING from
  Windows; if a step needs Raunak's interactive
  admin session, it reports the EXACT click).
  S1-B2-REDIS-PC2 (native Windows Redis 3.2) still
  running in parallel. SPEED-UP: S1-LOSSY-A SPAWNED
  — LossyExactHit fix, ROOT DECISION option A (key
  the exact cache on the pre-compression request:
  raw messages + compression params; the key must
  be a function of the logical request, not mutable
  pre-pass state; the reverted guard may be
  re-applied as part of it). :750/:751 disposition:
  V-B234 evidence stands (cold 15/15 + 21/21,
  toggles prove) — they stay PARTIAL honestly on the
  live-upstream limit, no new verifier needed.
- S1-B2-TEST-RACE2 SPAWNED (~13:23 IST): targeted
  -race on internal/server, internal/router,
  internal/strategy, internal/scoring,
  internal/degrade, internal/cache,
  internal/provider at current HEAD (the full race
  run did not cover :489 producers d3abe14 or
  possibly :600 bare-id rewrite 23b4189).
- S1-B2-09-CAP DONE (~13:27 IST, commit 3b76fa9,
  4 files, +200/−15), PC build BUILD-OK. (1)
  OmniRoute's cap (studied, not copied): quota
  pools carry per-API-key weight + absolute
  cap_value per allocation row; a key at its cap is
  blocked at rotation (enforce.ts model-cap pre-
  check). The #20561 diff itself is NOT in the
  shallow clone (single commit 4b9fc08a, no #20561)
  — behaviour taken from the comparison-log one-
  liner + quota-share docs, per the B14 note. (2)
  Per-key signal (ours): learnRateLimit now
  records the same measured fraction per key
  (setLearnedKeyQuotaHint, ratelimit.go) — the
  engine knows which key made each request;
  accessor LearnedKeyQuotaHint() mirrors the
  provider one with identical expiry-drop;
  RestoreLearnedKeyQuotaHint() replays the ledger's
  per-key rows at startup (main.go). No number
  invented: no signal = no cap. (3) Cap check:
  orderAccountCapCheck (account_fallback.go:189
  site) runs after each strategy's ordering —
  stable partition, at-cap (measured remaining 0,
  live entry) accounts sink to the tail; none/all
  at cap → order unchanged (fail-open). Every
  strategy's behaviour otherwise untouched. (4)
  Divergence recorded (in code comments):
  OmniRoute's cap is configured (operator sets
  cap_value) and blocks; ours is measured (learned
  from real responses, no config needed) and
  deprioritises — matching this tree's quota-share
  pool (saturated→tail, all-saturated fails open).
  A configured per-key cap_value surface is the
  follow-up (needs a new config schema;
  quota_shares is per (provider, model), not per
  key). (5) Proof: all 4 files hash-verified on
  the PC (one scp mis-drop to the repo root caught
  by hash check, strays deleted, re-synced per-
  file); go build ./... on the PC → BUILD-OK;
  gofmt clean on the VM. No tests written (build-
  only brief; a verifier must pin it). Root
  verified the commit (4 files, symbols present).
  T-B2-09-TEST SPAWNED to write the pinning tests
  (per-key hint recording + cap tail partition +
  fail-open); verifier follows.
- S1-B2-TEST-BASELINE DONE (~13:21 IST) — B2 full-
  suite baseline at HEAD d6ea9ae, PC copy D:\
  codvian-router-baseline (clean HEAD snapshot,
  separate from C:\codvian-router so verifiers were
  undisturbed). SMOKE 6/6 PASS (real server, port
  20273, isolated config+data on D: — Raunak's real
  PC config untouched): /healthz 200, no-key /v1/
  models 401, no-key chat 401, bootstrap 201 + key
  issued, authed /v1/models 200, authed routed chat
  → honest 502 "upstream provider error" (0
  providers). FULL SUITE RED: 46 pkgs ok / 2 FAIL
  (internal/router 73.9s, internal/server 120.2s)
  / 3 no-test-files; 4,395 top-level PASS / 2 FAIL
  / 8 SKIP (6,165 PASS: lines incl. subtests); 0
  panics, 0 compile breaks. DISTINCT FAILURES = 2
  only. (1) TestChatCompletionsLossyExactHitStillWorks (server) — KNOWN design call,
  pre-existing (compression_cache_test.go:376,
  X-Codvian-Cache="" want exact; CCR-warmth miss).
  Not a regression; needs Raunak's A/B decision,
  untouched per brief. (2) TestBreakerStaleDegradedPersistsAcrossReload (router,
  breaker_lazy_test.go:102) — after reload CLOSED
  with 1 failure, want DEGRADED with 5. Same
  mechanism as the T4-known rename-loss flake
  (saveBreakerStore drops rename errors; T4's
  5×50ms retry cites this exact test), but worse:
  4 of 5 saves lost, likely Windows file-lock
  contention under the 10–11-agent concurrent PC
  load. Needs a single-test re-run to separate
  flake vs real; if real, fix is in
  renameBreakerStoreFile (longer retry / unique
  tmp). Not fixed (baseline only). NO other
  buckets: A2A 0, fsync 0, CLI-vs-real-config 0,
  guard-table drift 0, small-package 0. The
  release-verify "FAIL checksums/bundle/signature"
  lines in the log are expected output of PASSING
  negative tests (tampered/unsigned releases must
  fail verification). Log: hidden_files/gotest-b2-
  baseline-2026-11-20.log (15,064 lines; PC copy
  D:\codvian-baseline-gotest.log). Harness notes:
  cmd `set VAR=... &&` appends a trailing space to
  the value (broke GOCACHE — fixed by using .cmd
  files); PowerShell strips quotes from curl.exe -d
  JSON (use -d @file). No code/test files edited,
  no commits. S1-B2-BREAKER-FIX SPAWNED (single-
  test re-run first, fix only if real).
- T-B2-489 DONE (~13:21 IST, commit d3abe14, 3
  files, +384/−46): all three :489 producers built.
  (1) Production summariser — main.go:
  relaySummariser closure (before engCfg) dials
  the cheapest priced model with a live client
  directly (fresh price-book scan per call;
  unpriced/no-client skipped), 30 s timeout, 512
  max tokens, structured SUMMARY/PROGRESS/
  DECISIONS/ENTITIES prompt + lenient parser; no
  candidate / failed call / empty reply = error →
  honest raw-transcript degradation. Never touches
  the engine, so no re-entry into the attempt loop.
  (2) Production quota-use signal — main.go engCfg
  RelayQuotaUsedFor: engine.LearnedQuotaHint()
  (3cca53b source, measured upstream rate-limit
  headers) converted to used fraction (100-hint)/
  100, clamped [0,1]; unknown stays unknown. No
  new signal, no invented number. (3) Cross-
  request session store — relay.go + router.go:
  engine-owned relaySessions map keyed by gateway
  key (req.CallerID); a request ending with a
  pending want stores the WANT (source messages +
  original expiry, never a built handoff) via defer
  on every loop exit; the next context-relay
  request preloads it in newRelayFor and its first
  attempt builds + injects the handoff through the
  existing lazy path. Cap 512 entries, expired-
  first sweep. AgentSessions was checked and NOT
  used — it holds usage rows only, no handoff
  content. PC build green: 3 files hash-verified
  on PC, go build ./... → PC_BUILD_OK; VM build +
  vet clean, gofmt clean. Design choices recorded:
  session key = gateway key id (no finer identity
  exists in the engine); an already-built handoff
  is never re-stored; summariser model = cheapest
  priced, per the brief's honest-default rule. Row
  :489 stays PARTIAL until a verifier passes it —
  no PARITY.md edit, no flip claimed. Root
  verified the commit (3 files, +384/−46).
  V-B2-489 SPAWNED to verify.
- V-B2-AUTOFIX-GROUP DONE (~13:21 IST) — 5 rows
  verified, 4 flips, 1 stays. Cold on PC at HEAD
  a2328d8 (7/7 files hash-verified): 5/5 tests,
  12/12 cases PASS, ok internal/router 0.376s. All
  5 toggle cycles under flock (each ≤5 min hold):
  OFF FAILs genuine, ON PASSes, VM + PC hashes =
  HEAD after every restore. No PARITY.md edits, no
  commits. :535 FLIP TO FULL —
  TestB2SubscriptionServesRungZeroThroughChat.
  Live path: relay.go:1324 → Engine.Chat →
  dispatchAutoVariant (autodispatch.go:237) →
  resolveSubscription (autovariants.go:709). Toggle
  A: rung disabled → Chat errors; restored → PASS.
  Ladder semantics fully proven both branches
  (rung-0 serves, 1 call, no billable fallback;
  empty rung fails closed). The missing live
  billing data is a documented by-design limit
  (main.go:1189-1195: "honest limits, not data"),
  not a missing feature — recorded in the row text.
  :538 FLIP TO FULL —
  TestB2BestVisionAndReasoningServeCapableModelThroughChat. Toggle B: capability filter off →
  both sub-cases FAIL with reason "no vision/
  reasoning-capable model… fail-open". Inputs are
  real production sources: Vision from models.dev
  sync (modelsync.go:196/609 → main.go:1242),
  Reasoning from category + modelsync intelligence.
  Positive + fail-open branches both proven. :539
  FLIP TO FULL —
  TestB2ProCodingServesPremiumPickThroughChat.
  Toggle C: tier filter off → both sub-cases FAIL
  with reason "no premium-tier model… fail-open".
  The tester's "main.go only sets Tier 'free'" is
  refuted by the code: main.go:1258 classifies Tier
  from real prices (ClassifyPriceTier: premium
  above $1/M input), so premium models exist in
  production. Positive + fail-open branches both
  proven. :542 STAYS PARTIAL —
  TestB2CascadeOrderAppliesAtDispatch. Toggle D:
  interception off → order stays input [provC
  provF provK provA provS]; restored → PASS. Wiring
  is proven at both seams (router.go:2671,
  tree.go:261) + real e2e dispatch. BUT nothing in
  the product can ever select "cascade": config
  allowlist rejects it (config.go:1790-1791,
  isStrategyName), dashboard/MCP strategy.Parse
  rejects it, no auto id maps to it, no client
  field carries it, the only internal caller passes
  "". Remaining gap: the selector — config.go
  allowlist entry for global_strategy="cascade"
  (Wave 3 owns it, per cascade.go comment). :543
  FLIP TO FULL —
  TestB2EndpointProjectionRejectsFusionOnMediaKinds. Toggle E: projection off → all 6
  kinds (images, search, video, music, OCR, audio)
  served instead of rejected; restored → PASS.
  Projection enforced at live entry points
  (router.go:4570 chat, :5598 stream,
  embeddings.go:302); all 9 endpoint kinds now
  proven. The missing audio provider client is a
  separate feature (audio calling), not this row's
  projection — kept noted in the row text. Process
  notes: one OFF run first failed on transient PC
  file-lock contention (vet.exe vs concurrent
  baseline run) — retried with -vet=off; one
  restore hit a transient .git/index.lock from a
  concurrent git process — restored manually,
  hashes re-verified. Flips applied (4 rows + :542
  annotation + Area 1 header + headline in ONE
  commit) ded675d → parity 273/163, 41.9%/54.4%
  (parity.sh MATCH). Area 1 now: FULL 53 / PARTIAL
  10, 84.1%/92.1%. B2 verification tally now: 32
  rows → 24 FULL (8 batch + 15 test-phase + 1
  already FULL) + 8 STAYS PARTIAL (:495/:497
  pinning test in progress, :489 in verification,
  :600 in re-verification, :542 selector gap,
  :738 Redis deviation, :754, :750/:751 live-
  upstream limits).
- V-B2-16F-GROUP DONE (~13:05 IST) — all 4 rows
  FLIP TO FULL. :510 auto 16-factor scoring: code
  in path wiringHook StrategyBalanced → Hook16Default (wiring.go:531-546) via HookFactory
  (strategy.go:537) wired at main.go:3096.
  TestBalancedHookOrdersBySixteenOnlyFactor PASS
  cold on PC at HEAD (0.301s, 3/3 file hashes =
  VM). Toggle A (5-factor HookWithWeights revert):
  test FAILS ("got [prov-a prov-b], want [prov-b
  prov-a]"); restore → PASS (verbose re-run).
  :511/:512 same scorer/wiring, same gap, same
  proof. :527 LKGP: registry + LatestHealthyRecord
  (lkgpregistry.go:99), layered into pool inputs
  in the rankModels closure (main.go:1284/2849-
  2876), RecordSuccess on inference.
  TestPoolRankingLKGPStickinessLeads PASS cold on
  PC. Toggle B (LatestHealthyRecord forced no-
  record): test FAILS ("got [model-a model-b], want
  [model-b model-a]"); restore → PASS (verbose re-
  run). Cleanliness: VM + PC hashes = HEAD for
  wiring.go, lkgpregistry.go, b2_strategy_pins_
  test.go after both restores; no PARITY.md edits,
  no commits. Notes: both toggle scripts exited 91
  on the agent's own grep bug (post-restore runs
  lacked -v, so no `--- PASS:` line; verbose re-
  runs confirm both PASS); toggle B's first attempt
  aborted harmlessly (nil-guard pattern matched
  twice — fixed by anchoring on the function
  signature, file untouched). Flips applied (4 rows
  + Area 1 header + headline in ONE commit)
  a2328d8 → parity 264/171, 40.5%/53.6% (parity.sh
  MATCH). Area 1 now: FULL 45 / PARTIAL 18,
  71.4%/85.7%. Parity crossed 40% strict.
- V-B233 DONE (~12:16 IST) — verdict FLIP TO FULL
  (:747 cache analytics). Code in live path:
  handleCacheStats (cache_stats.go) reads live
  exact/semantic Stats(), recorder Snapshot(),
  SavingsTotals() via PersistentCache (persist.go:
  428); route registered guard.go:251 + server.go:
  1648. Row's only gap is STALE: /dashboard/cache
  page EXISTS and is live (nav "Cache", view-cache
  section, loadCachePage() fetches /api/cache/stats,
  flush via DELETE /api/cache) — tested by 6
  TestDashboardM10_10* tests. Cold on PC at HEAD
  (hash-verified 6/6): 26/26 PASS — TestCacheStats*
  (8) + TestM62bCacheStats* (2) + TestDashboardM10_10*
  (6) in internal/server (ok 0.615s), TestM62b* (10)
  in internal/cache (ok 3.206s). Toggle-check
  (flock-held, ≤5 min — the cap WORKED): handler
  returning empty response → 8/8 TestCacheStats* +
  2/2 TestM62bCacheStats* + 3 feed-dependent dashboard
  tests FAIL. Restored clean: VM + PC hashes = HEAD,
  post-restore re-run ok 0.831s, lock handed to next
  verifier (V-B227). Caveat: no literal getCacheStats()
  function exists — the capability is handleCacheStats;
  naming difference only. Flip applied by root +
  committed c734dc9 (row + Area 6 header + headline
  in ONE commit). PARITY NOW: FULL 253 / PARTIAL 181 /
  MISSING 216 / WRONG 2, strict 38.8%, weighted 52.7%
  (parity.sh MATCH). Area 6: FULL 25 / PARTIAL 6,
  strict 80.6%, weighted 90.3%. 4 verifiers still
  running (V-B227-GROUP, V-B232-GROUP, V-B234-GROUP,
  V-B237).
- V-B237 DONE (~12:21 IST) — verdict FLIP TO FULL
  (:755 search cache). Code in live request path:
  mediaPassthroughSearch (media_verify.go:266) calls
  SearchCache.GetOrCoalesce around the single
  Perplexity passthrough, search only, after all
  gates; hit/miss via X-Codvian-Search-Cache. Row's
  "still unwired" claim is STALE — wiring landed in
  G13. Cold on PC at HEAD 28ecd17 (4/4 hashes match):
  16/16 PASS — TestM66Search* (11) + TestG13SearchCache*
  (5), incl. HitSecondCallFree (1 upstream call for 2
  identical searches). Toggle-check (flock, ≤5-min
  hold): cache bypassed (if true direct path) →
  HitSecondCallFree FAILS ("upstream calls = 2, want
  1"). Restored: VM + PC hashes = HEAD, test PASSes
  again. Lock released (next holder V-B232). Honest
  note: NFKC normalization gap (no x/text dep;
  full-width vs ASCII key differently — missed-sharing,
  never wrong serve). Flip applied by root + committed
  6746ad2 (row + Area 6 header + headline in ONE
  commit). PARITY NOW: FULL 254 / PARTIAL 180 /
  MISSING 216 / WRONG 2, strict 39.0%, weighted 52.8%
  (parity.sh MATCH). Area 6: FULL 26 / PARTIAL 5,
  strict 83.9%, weighted 91.9%. 3 verifiers still
  running (V-B227-GROUP, V-B232-GROUP, V-B234-GROUP).
  NOTE: M internal/router/failover_policy.go seen in
  git status during the flip — V-B227-GROUP's live
  toggle; left untouched (commit.sh lists only
  docs/PARITY.md).
- V-B227-GROUP DONE (~12:23 IST) — 3 flips, 1 stays
  PARTIAL. Cold on PC at HEAD c75676a: all 13 named
  tests PASS (router 8/8, server 5/5) + 3 extra G11
  tests PASS. All 5 toggles under flock (each ≤5 min
  hold), all files restored hash-verified to HEAD on
  VM + PC. B2-27 (:579) FLIP TO FULL — per-auth base
  (OAuth 5s/API-key 3s/Local 2s, router.go:4226
  fallback + soft ladder) + anti-thundering-herd
  stagger (effectiveCooldownUntil, cooldown.go:262 +
  router.go:3334) toggle-proven: base uniform → bases
  test FAILs ("API-key = 5s, want 3s"); stagger 0 →
  stagger test FAILs ("1 distinct, want ≥3"). B2-28
  (:593) FLIP TO FULL — sub-budget counting live
  (deadlineSubBudgetHit, called router.go:5157;
  default max 3) + DeadlineBudget → clean 504
  (withDeadline router.go:2215; server.go:6590).
  Toggle: counting off → TestG11DeadlineSubBudget-
  ExhaustsTo504 FAILs, outer-budget tests still PASS.
  Scope: sequential chat path only (hedge/stream are
  documented follow-ups). B2-29 (:600) STAYS PARTIAL
  — 3-provider default toggle-proven (cap 99 → 5
  attempts, want 3; prefixed test still PASSes),
  engine-level only-narrowing proven
  (TestG11PrefixedProviderNeverRerouted). NOT FULL:
  plain chat path does NOT translate a "provider/
  model" model string into routing.only (verified by
  grep + chain construction — chainForDispatch builds
  one attempt per provider for the same model string;
  req.Routing is never set from the model prefix on
  the plain path), so a prefixed model can still fail
  over on the plain path. The G11 report records this
  as unverified. B2-30 (:602) FLIP TO FULL — knobs
  exist in config (MaxRetries/RetryDelayMs/MaxSet-
  Retries/FailoverBeforeRetry, config.go:1403-1421,
  validated), wired main.go:3446-3455, consumed in the
  failover loop (router.go:4392, 5246; failover_policy.
  go). Toggles: cap off → MaxRetries test FAILs ("a
  called 4 times, want 2") + MaxSetRetries test FAILs;
  failover-before-retry forced on → MaxRetries test
  FAILs ("a called 1 time, want 2"); delay test still
  PASSes (specificity). Flips + annotation applied by
  root + committed 96b8437 (rows + Area 2 header +
  headline in ONE commit). PARITY NOW: FULL 257 /
  PARTIAL 177 / MISSING 216 / WRONG 2, strict 39.4%,
  weighted 53.0% (parity.sh MATCH). Area 2: FULL 39 /
  PARTIAL 1, strict 97.5%, weighted 98.8%. 2 verifiers
  still running (V-B232-GROUP, V-B234-GROUP). Note:
  M internal/degrade/degrade.go in git status is the
  other lane's in-flight work, not mine.
- Critic run 12:21 IST (HEAD 6b9ce1a) — audit-fake
  PASS (0 findings, 224 routes); parity MATCH 257/177,
  39.4%/53.0%; last 3 flips name tests ✓ (:579/:593/
  :602). Gaps unchanged (:782 PII vault BLOCKED,
  :827 provider detail B8-blocked, :816 update flow
  B8-blocked). FIX: none this run — no fixable problem
  in the critic lane; LossyExactHit design call still
  open (still FAILS on VM with known assertion).
  Rows 669/678/783 read MISSING in PARITY.md but code
  is BUILT (dashauth Argon2id+session, oauth_api.go
  PKCE S256+JWT, server.go B3-20 rewrite; already in
  STAGE1-VERIFY-QUEUE.md :285/:305/:391) — need V's
  pass, no flip. (Row-number drift in the critic's
  report: established numbers are 619/628/733; the
  substance is right — those three are built and
  queued for verification in batch 3, B3/Area 4.)
  Live: 2 verifiers + critic = 3/8 cap; VM go build
  ./... clean. No push, no flip.
- V-B232-GROUP DONE (~12:31 IST) — both rows STAYS
  PARTIAL, no flip. Cold on PC at HEAD (hash-verified):
  72/72 PASS (B2-32: 32 M62/M62b tests; B2-36: 36 M66/
  M66b tests + 4 G13 tests). B2-32 (:738) STAYS PARTIAL
  — SQLite persistence tested + live; Redis vector
  store BUILT + wired (9149db0, main.go:3915-3923) but
  ZERO tests reference it and no live Redis exists (PC:
  no redis-server, port 6379 closed; VM: none) —
  unverifiable here. Toggle A: resume exact-serving
  disabled (ChatStreamFrom + Chunks) → all 13 resume
  tests still PASS — the 4c209cb serving is untested.
  B2-36 (:754) STAYS PARTIAL — row text stale in 3
  places: DB tier EXISTS + wired in serve mode (G13
  OpenPersistentReasoningCache, SQLite reasoning_
  cache.db via wireReasoningCacheAtPath, main.go:3998;
  4 G13 tests PASS cold); budget DOES count signatures
  + redacted (TestG13ReasoningBudgetCountsSignatures-
  AndRedacted PASS); temp-gate is a RECORDED divergence
  (mitigated), not an open gap. Remaining gap: text-
  resume injectReasoning (4c209cb) has NO test —
  Toggle B: all 17 reasoning tests PASS with it
  disabled; control probe (chunk injection off) FAILS
  TestM66bChunkStreamInjectsBeforeUpstream, so the
  family is sensitive, not vacuous. DB-tier toggle
  BLOCKED (verify lock held >5 min by another
  verifier's long cycle — same batch-1 congestion).
  Needs 1 test before FULL: text ChatStreamFrom
  injection. Annotations applied by root + committed
  b0ffd5b (parity unchanged 257/177, 39.4%/53.0%,
  MATCH). PROCESS HOLES (batch-2, for the batch-3
  brief): (1) the 5-min lock cap is advisory, not
  enforced — a toggle script that runs long still holds
  the lock past 5 min (V-B232's DB-tier toggle BLOCKED
  on it); batch 3 must hard-time-box the toggle script
  itself. (2) The flock guards the VM file, NOT the
  shared PC copy — 2 cache-stats tests failed mid-run
  only while V-B233's toggle was live on the shared PC
  copy (PASS on re-run); batch 3 brief must say: before
  any cold run on the PC, re-sync your test files from
  HEAD and verify hashes — never assume the shared PC
  copy is clean. Logs: hidden_files/verify-b232-
  toggleA/B/B2.log (no toggleC log — flock timed out
  before the script ran). Time-box exceeded (~45 min →
  ~70 min, mostly lock waits + the V-B233 collision
  re-run). 1 verifier still running (V-B234-GROUP).
  NOTE: M internal/provider/anthropic_client.go +
  ?? internal/oauth/jwt_test.go in git status are
  V-B234-GROUP's in-flight work, not mine.
- V-B234-GROUP DONE (~12:37 IST) — both rows STAYS
  PARTIAL, no flip. Cold on PC at HEAD (9/9 hashes
  verified): B2-34 15/15 PASS, B2-35 21/21 PASS, 0 FAIL.
  B2-34 (:750) STAYS PARTIAL — tier split live in path
  (wire parses usage.cache_creation 5m/1h, CacheWrite-
  Tiers() feeds ConvertResponse + TranslateStream;
  ledger costs 1.25×/2× with explicit-rate-wins; config
  carries tier rates). Toggle 1: CacheWriteTiers() →
  0,0 → all 3 wire tier tests FAIL. Gap remains:
  dashboard still estimates write cost as a 1.25×–2.5×
  band; no live upstream verification (mock only).
  Queue counts stale (10 → 15 tests). B2-35 (:751)
  STAYS PARTIAL — auto-marker live + pinned (toggle 2:
  marker off → 6/13 provider tests FAIL, 7 negative/
  gate tests correctly PASS). But toggle 3 proves the
  f790f58 image fix is UNPINNED: reverting it → 21/21
  PASS (no test sends an image_url part with a client
  marker). Row text also stale: system/tools marking
  now exists (G13), client marker survives decode.
  Gaps remain: OpenRouter unwired, no TTL (1h tier
  never opted into), mock-only. Toggles run under
  flock (lock was held ~5 min by another verifier's
  script; cycle 1 held ~6.8 min — poll grep missed
  UTF-16LE DONE, fixed for cycles 2–3). All toggles
  restored; my files hash-verified = HEAD on VM + PC.
  Other lanes' dirty files observed, not mine
  (admin_keys_api.go, neural_embedder_test.go,
  ionizer_test.go). Logs in hidden_files/verify-b234-*-
  2026-11-20.utf8.log. Time-box exceeded (~45 → ~50
  min, mostly lock waiting). Annotations applied by
  root + committed 7d79875 (parity unchanged 257/177,
  39.4%/53.0%, MATCH).
- VERIFY BATCH 2 COMPLETE (12:37 IST, 5/5 verifiers).
  10 rows verified (Area 2: 4, Area 6: 6 — B2-31 NO TEST
  skipped to the tester lane). Results: 5 FLIP TO FULL
  (:747 cache analytics, :755 search cache, :579
  connection cooldown, :593 deadline sub-budget, :602
  failover retry knobs), 5 STAYS PARTIAL (:738, :754,
  :750, :751, :600). Parity: 252/182 → 257/177, strict
  38.7% → 39.4%, weighted 52.6% → 53.0% (parity.sh
  MATCH). BLOCK-WISE OVERRIDE (Raunak, 2026-11-20
  ~12:33 IST, voice call — SUPERSEDES stage-wise mode,
  recorded in MEMORY.md): no more stages. Work goes
  block by block — B1, then B2, then B3, then B4, and
  so on: build one block fully, test it properly, finish
  it, only then move to the next block. The Stage 1
  build (B2+B3+B4 together) is already done and cannot
  be un-built — the block-wise order now applies to the
  verification (B2 rows first, then B3, then B4) and to
  all future blocks (B5 build+test fully, then B6, etc.).
  Results are reported block-wise too. NEXT: batch 3 =
  B3 rows (Area 4 keys/auth/policy first, then Area 7
  guardrails/PII), per the block-wise order. The tester
  lane (spawned during the call, 5 testers) runs in
  parallel — its new test files are untracked in git
  status (neural_embedder_test.go, ionizer_test.go,
  relevance_test.go, smartcrusher_test.go, styles_test.go);
  do NOT commit them from here.
- Critic run 12:53 IST — audit-fake PASS (0 findings,
  224 routes); parity MATCH 258/177, 39.6%/53.1%; last
  3 flips name tests ✓ (:755, :747, :579). Top gaps:
  :510/:511/:512 16-factor pinning (T-B2-STRAT in
  flight), :501/:508/:534/:536 router tests (T-B2-ROUTER
  in flight), :525 tests done b90b4a0 (needs V pass).
  FIX COMMITTED 6716036 (root-verified: 1 file, test-
  only): :754 text-resume injectReasoning — added
  TestM66bTextStreamFromInjectsBeforeUpstream pinning
  ChatStreamFrom inject (degrade.go:2100), toggle-proven
  (test FAILS with line off, PASSES with it on), full
  internal/degrade pkgtest ok (1.65s). No PARITY.md flip
  (:754 still blocked on DB-tier toggle, lock congestion).
  This closes the V-B232 gap for :754 except the DB-tier
  toggle. NOTE: HEAD moved past this run — V-B2-730 flip
  :730 NeuralEmbedder PARTIAL→FULL (7ae4fcd, delegate's
  B2-first lane; 11/11 cold on PC, toggle BoW-revert
  FAILS). PARITY NOW (root-verified): FULL 259 / PARTIAL
  176 / MISSING 215 / WRONG 2, strict 39.7%, weighted
  53.2% (parity.sh MATCH). B2-first test phase is the
  delegate's lane (6 agents: 1 baseline + 5 verifiers) —
  do NOT spawn duplicate verifiers from here.
- Critic run 13:44 IST — audit-fake PASS (0 findings,
  224 routes); last 3 flips name tests ✓ (:535/:538/
  :543, V-B2-AUTOFIX-GROUP). Top gaps: :495/:497 quota-
  overlay pinning IN PROGRESS (untracked b2_quota_pin_
  test.go, 181 lines — hands off); :542 cascade selector
  (Wave 3, not a one-run fix); :738 Redis deviation
  (VM-only proof, no PC Redis). FIX COMMITTED 8c06111
  (root-verified: 1 file, test-only +62): :738 resume
  exact-cache serving — added TestM66bChatStreamFrom
  ServesExactHitTrimmed pinning resume exact-cache serve
  on ChatStreamFrom, toggle-proven (exactLookup off →
  FAILS 'upstream calls chat=1 stream=1, want 1/0';
  restored md5-verified → PASS), full internal/degrade
  pkgtest ok (3.13s). No PARITY flip (:738 still needs
  the Redis half + V's pass). This closes the V-B2-738
  resume-serving gap; only the Redis half remains.
  PARITY NOW (root-verified): FULL 273 / PARTIAL 161 /
  MISSING 215 / WRONG 1, comparable 650, strict 42.0%,
  weighted 54.4% (parity.sh MATCH). NOTE: denominator
  moved 652 → 650 because two rows re-scoped PARTIAL →
  DIVERGENCE (B2-04 :498 + B2-25 :545) — DIVERGENCE rows
  are excluded from comparable. All six B2 decision
  items now decided (B2-09 cap build, B2-13 fault-inj
  build, B2-25 trees/DIVERGENCE, B2-26 fast default +
  256KB guard, B2-04 DIVERGENCE, B2-40 already FULL).
  MEMORY rule: NEVER explain with M-labels to Raunak —
  plain words only (M-numbers in files/briefs only).
- Critic run 13:58 IST — audit-fake PASS (0 findings,
  224 routes); parity MATCH 281/153, 43.2%/55.0% at run
  time (verifiers flipped :489/:754/:600 during the
  run); last 3 flips name tests ✓. Top gaps: :782 PII
  token vault MISSING, zero code (BLOCKED, multi-hour
  B3 slice); :827 provider detail page + :816 update
  flow MISSING (B8-blocked). FIX COMMITTED 4d3494e
  (root-verified: 1 file, test-only +43): :751 image_url
  cache_control pin — added TestM69cConvertRequest
  PreservesImageMarker pinning f790f58 (data-URL +
  https-URL branches keep marker verbatim, unmarked
  image stays unmarked), toggle-proven (call-site
  p.CacheControl→nil FAILS both branches, restored
  byte-identical md5 → PASS), full internal/wire
  pkgtest ok (4.02s). No PARITY flip (:751 still needs
  OpenRouter wiring + 1h TTL opt-in + live-upstream
  proof — V's pass only). NOTE: HEAD moved past this
  run — V-B2-542 flip :542 cascade selector PARTIAL→
  FULL (ed62fe4, cold PC 5/5, toggle FAILs with
  allowlist entry removed). PARITY NOW (root-verified):
  FULL 282 / PARTIAL 152 / MISSING 215 / WRONG 1,
  strict 43.4%, weighted 55.1% (parity.sh MATCH).
  Area 2 now 100% FULL (6002647).
- Critic run 14:51 IST — audit-fake PASS (0 findings,
  224 routes); parity MATCH 285/150, 43.8%/55.4%
  (be803a6); last 3 flips name tests ✓ (:669, :738,
  :524). Top gaps MISSING: :782 reversible PII token
  vault (zero code); :827 provider detail page; :816
  update flow. FIX: :782 BLOCKED (third confirmation,
  PLAN.md:1029) — NOT fixable in one run (>20 min,
  multi-hour B3 L-GUARD slice); no second fix
  attempted. Progress: 1 of 16 stale rows flipped
  (:669 → FULL, V-B3-669, be803a6); remaining 15 queued
  for V. No commit this run (B1+B2 token push in
  flight; token permissions finalised 14:52 IST incl.
  Workflows Read+write). B2 GATE GREEN (e89e6b5):
  final gate PASS 4,474/0/8, 48 pkgs ok on PC; README
  updated 9d00154 (43.8%/55.4%). PUSH SCOPE measured
  (1ff7693): local 1,518 vs public 120 files — browser-
  only upload NOT practical; token route agreed with
  Raunak (custom.github secure entry, Git Data API
  push, token in vault, never in chat).
- Critic gate.sh fix LANDED (commit 2bd9693,
  ~11:29 IST, scripts/gate.sh only): per-mode
  evidence files (codvian.gate.full.json /
  codvian.gate.race.json + per-mode .err),
  docs/gate-last.txt names the mode, plus docs/
  gate-last-full.txt / gate-last-race.txt;
  toggle-proven in a scratch dir with a stub go.
  The separate-JSON-per-mode fix is DONE (no
  longer owed). This is the `M scripts/gate.sh`
  V-B207 saw mid-run — the critic's in-flight
  edit, not a verifier's. AGENTS.md updated.
- Critic 10:21 IST: audit-fake PASS (0 findings, 224
  routes); parity by parity.sh MATCH (FULL 249 / PARTIAL
  185 / MISSING 216 / WRONG 2 — correct numbers this
  run, not the 10:00 grep miscount); last 3 flips name
  tests ✓. FIX COMMITTED 051c176 (2 files, +100) —
  Area 12 row 18 WRONG (no true failover-switch-time
  measurement): new TestChatCompletionsFailoverSwitchTime
  (internal/server/failover_switch_time_test.go) times
  dead-429-response → fallback-dispatch on in-process
  mocks, measured 0.6–1.0 ms over 3 runs (target <300
  ms); toggle FAILS with fallback removed (502); full
  pkgtest internal/server 280s shows only the known
  LossyExactHit design-call failure. Measured number
  published in docs/INSTALL.md with the honest
  qualifier "NOT end-to-end" + "VM localhost mocks, 3
  runs". NO PARITY flip (needs V's pass — the row stays
  WRONG until then). Top gaps re-verified REAL: row 18
  (now measured, awaiting V), Area 8b 93 MISSING (B8
  blocked on Raunak's feature-list approval),
  LossyExactHit (design call open). T4 builders'
  files untouched by the critic.
- S1-MAIN-WIRE (respawn): NO CODE CHANGE — the sweep was already
  done; the "died silently" premise was wrong. First MAIN-WIRE
  finished cleanly 04:29:42 IST, commit 229b489 (14/15; item 4
  skipped, item 11 attribution deferred). Item 4 (B2-01 relay
  thresholds) then landed via RELAY-CONFIG 170d771 — verified
  present (main.go:3462-3480 + config validation). All 15 items
  verified in-tree by grep + read (DisabledTypes main.go:1846,
  custom_patterns 1859, guard opts 4664-4666, response_validation
  3422, deadline_failure_max 3433, max_providers 3438, failover
  knobs 3446-3454, hedge 3459, LiteLLM syncer 2367 + loop 4943 +
  SetSyncedPricesForSource 2349/2382, adapter Cost/ServiceTier/
  AutoVariant + FlatRateAsZero 2133, SetAutoRanker 1283, arena_elo
  in config.Config, QuotaShareFor 3486, :529F comment fixed).
  No PC build run: Go tree byte-identical to f329fbc (only this
  queue file changed since), whose PC build was verified green
  ~04:57 IST; a full PC refresh now would pollute the live :939
  builder's in-flight PC build. Remaining: attribution plumbing
  only (engine has no request-attribution concept — router-lane
  follow-up, outside main.go/config.go). Do NOT respawn MAIN-WIRE
  again.

## S1-MAIN-WIRE sweep list (main.go + config.go, one builder)

1. Guardrails DisabledTypes: gc.DisabledTypes = cfg.Guardrails.DisabledTypes (buildGuardrailStore ~1668).
2. B3-18 custom patterns: guardrails.custom_patterns config mapping in buildGuardrailStore.
3. B3-20/21/22: wire WithRequestRewrite / WithResponseSanitization / WithResponseGuardMode from config.
4. B2-01: threshold/TTL config surface (0.85/0.95/5h/30 defaults → config fields).
5. :523 response_validation: routing.response_validation → engine (main.go:2795).
6. B2-28: routing.deadline_failure_max operator surface.
7. B2-29: routing.max_providers_per_request knob (default 3).
8. B2-30: routing.max_retries / retry_delay_ms / max_set_retries / failover_before_retry + wire G11 engine fields.
9. B2-26: hedge config surface (config.go + main.go). Do NOT change the 150 ms default (decision pending).
10. CATALOG-SYNC: construct LiteLLM syncer + loop; switch models.dev call sites to SetSyncedPricesForSource.
11. LEDGER2: main.go mapping — Cost/ServiceTier/attribution/AutoVariant in RecordAttemptForKey/RecordAttemptWithProviderKey; FlatRateAsZero config surface.
12. AUTO-WIRE: router.SetAutoRanker(autoRouter.RankModels()) next to SetAutoCatalog (~1152).
13. ELO-WIRE: move arena_elo block into config.Config (config.Save drops it otherwise).
14. B2-08: QuotaShareFor config surface (config.go + main.go).
15. :529F: cooldown_auth.go header comment touch (stale "hard-mode 429 cooldowns are hint-driven").

## Stage 1 exit (per the new mode)

Build phase: 2-hour cap, then test what exists. Test phase: 2-min smoke
check first (start router, one real request, confirm it answers), then
separate tester agents split by package on the PC — tests for every feature,
full suite, race check once, benchmark once. A feature is FULL only after
its test passes AND a non-builder checker confirms it (batches of 10).
