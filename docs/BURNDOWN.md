# docs/BURNDOWN.md — Codvian Router vs OmniRoute: open-row burndown (Block 2, v2 Part 8)

Date: 2026-11-20. Source: `docs/PARITY.md` only (grep-level parse; file never read in full). Counts verified with `bash scripts/parity.sh` this run: FULL 241 / PARTIAL 191 / MISSING 218 / WRONG 2, comparable 652, strict 37.0%, weighted 51.6%, MATCH.

## Scope and coverage gap (read first)

- Header-claimed open rows (what parity.sh sums): **409** (MISSING 218 + PARTIAL 191).
- Rows actually present as tables in PARITY.md: **295** (MISSING 137 + PARTIAL 158).
- **Coverage gap — Area 8b only:** the PARITY.md Area 8b table is condensed ("Key rows" only, 24 data rows). Header claims 93 MISSING + 36 PARTIAL = 129 open; only 12 MISSING + 3 PARTIAL = 15 are in the file. The other **114 open rows (81 MISSING + 33 PARTIAL) exist only in the comparison report** (`~/workspace/goals/codvian-router/hidden_files/omniroute-full-comparison-2026-11-18.md` §Area 8b) and are NOT itemized here. Follow-up: expand Area 8b from that report before the 8b burn-down can be scheduled honestly. No row was invented to close the gap. Every other area’s table rows match its header exactly.
- Excluded by approved scope (docs/SCOPE-PROPOSAL.md, APPROVED by Raunak 2026-11-20 ~01:21 IST — all 6 candidates): **5 rows** (1079 Desktop app, 1080 Self-update, 1086 Extra install surfaces, 1075 Launch/wrap external CLIs, 948 Language packs). Listed in §Excluded, NOT as work. Row 927 (Caveman locales) stays in the denominator (locale portion only out) and IS in the queue.
- Work queue below: **289 rows** (itemized open 295 − 5 excluded). Deferred (size L, unlocks nothing): **1 row(s)** (§Deferred). WRONG rows (2) are not queue items — see §Wrong.
- Approved-scope denominator: 647 rows; approved-scope parity 37.2% strict / 51.9% weighted. Raw parity (652) is always reported too.

## How size/value were judged (auditable rules, not guesses)

- Value: H = security/auth/budget/cost/latency or M3-gate blocker (Area 1, Area 2, Area 4, Area 7 PII, Area 12, quota-cap row 388, G22 cache-aware wiring, PKG-3 lifecycle); M = everything else that ships (dashboard, API endpoints, MCP tools, compression, analytics, packaging); L = docs/README only.
- Size: S = wiring/completion of existing code, one field/flag/input, one dashboard control; M = one new endpoint/tool/feature or a behaviour with a named missing piece; L = a new subsystem (dashboard auth, OAuth/JWT, 16-factor scorer, reversible PII vault, docs-breadth program).
- Size L rows are in the queue ONLY if they unlock an area; other size-L rows are in §Deferred. Size "?" means the row cannot be sized from PARITY.md (stale verdict suspected) — check first, do not build blind.
- The four Area 1 16-factor rows (PARITY.md:451/460/461/477) are facets of ONE shared effort — completing the 16-factor scorer — not four separate L builds; they share the blocker "16-factor scorer completion".
- Blocker is quoted from the row’s own status text where possible; "D2" = the pending per-key quota signal (no per-key quota source exists anywhere).
- Known data-quality note (pre-existing, not fixed here): Area 8b’s verdicts sum to 138 against 139 claimed comparable rows — one row unaccounted; no row was invented or dropped to close it.

## Open rows by area (itemized in this file vs header claim)

| Area | MISSING (file) | PARTIAL (file) | Open (file) | Header open | Gap (condensed away) |
|---|---|---|---|---|---|
| 1 | 0 | 26 | 26 | 26 | 0 |
| 2 | 0 | 7 | 7 | 7 | 0 |
| 3 | 2 | 6 | 8 | 8 | 0 |
| 4 | 6 | 3 | 9 | 9 | 0 |
| 5 | 0 | 0 | 0 | 0 | 0 |
| 6 | 0 | 7 | 7 | 7 | 0 |
| 7 | 12 | 7 | 19 | 19 | 0 |
| 8a | 19 | 14 | 33 | 33 | 0 |
| 8b | 12 | 3 | 15 | 129 | 114 |
| 9 | 27 | 7 | 34 | 34 | 0 |
| 10 | 20 | 13 | 33 | 33 | 0 |
| 11 | 5 | 7 | 12 | 12 | 0 |
| 12 | 0 | 3 | 3 | 3 | 0 |
| 13 | 6 | 12 | 18 | 18 | 0 |
| 14a | 12 | 17 | 29 | 29 | 0 |
| 14b | 11 | 21 | 32 | 32 | 0 |
| 14c | 4 | 4 | 8 | 8 | 0 |
| 14d | 1 | 1 | 2 | 2 | 0 |

Lowest areas (most work): **8b** (129 header-open, 114 of them condensed away), **14b** (32), **14a** (29), **9** (34), **10** (33), **7** (19), **8a** (33). Area 1 behaviours (26 PARTIAL, 0 MISSING) are the M3-gate blockers: Area 1 weighted 79.4% vs the 80% gate.

## Tier 1 — H+S (do first) — 11 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 2 | 200-OK body validation predicate → failover — No general 200-body predicate; only empty/error streams via preread | PARTIAL | S | H | — | PARITY.md:523 |
| 2 | Breaker states CLOSED/DEGRADED/OPEN/HALF_OPEN, persisted, lazy recovery — CLOSED/DEGRADED/OPEN/HALF_OPEN all reported (M4.2, incl. DEGRADED at 5/7 and HALF_OPEN probe); backoff esca... | PARTIAL | S | H | — | PARITY.md:524 |
| 2 | Connection cooldown (one key/account) — Hard cooldown 5 s doubling, capped 5 min; soft-demote mode; per-key dead-key cooldown 5 min. No per-auth ba... | PARTIAL | S | H | — | PARITY.md:529 |
| 2 | Deadline-failure sub-budget ending combo with clean 504 — DeadlineBudget → clean 504 exists, but no readiness-deadline failure counting / sub-budget | PARTIAL | S | H | — | PARITY.md:543 |
| 2 | Default cross-provider policy: up to 3 provider attempts; prefixed provider/model never rerouted — No 3-attempt default; cap is MaxAttempts 5 over operator chain | PARTIAL | S | H | — | PARITY.md:550 |
| 2 | Failover-before-retry + retry knobs — Failover-first is default behaviour, but no maxRetries/retryDelayMs/maxSetRetries knobs | PARTIAL | S | H | — | PARITY.md:552 |
| 2 | Declarative per-model fallback chains API (GET/POST/DELETE /api/fallback/chains) — Chains are operator config; no runtime CRUD API verified | PARTIAL | S | H | — | PARITY.md:553 |
| 10 | Cache-aware compression (preserve system prompt, downgrade, prefixFreeze, QuantumLock) — Built but UNWIRED (internal/degrade/cache_aware.go: PrefixFreeze, ResolveCacheAwareStrategy, ResolvePreserv... | MISSING | S | H | — | PARITY.md:939 |
| 12 | Hedging (parallel 2nd attempt) — ON, HedgeDelay 150 ms; no body-size guard | PARTIAL | S | H | — | PARITY.md:1030 |
| 12 | Latency-aware routing / scoring — 5 factors/mode, balanced latency 0.25; health-probe latency re-ranks chains. Missing: latency-aware strateg... | PARTIAL | S | H | — | PARITY.md:1038 |
| 12 | Latency percentiles in telemetry — p50/p95 only, no p99. Health probe ring 48 h / 2,880 samples — ahead on history, behind on p99 | PARTIAL | S | H | — | PARITY.md:1040 |

## Tier 2 — H+M — 67 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 1 | context-relay — Relay state + next-attempt context builder implemented and registered (M3); engine integration LIVE for the... | PARTIAL | M | H | summariser design (production summariser) | PARITY.md:439 |
| 1 | reset-aware — Ordering implemented + registered (M3); reset times now live from the ledger PairQuota roll-off (Phase C), ... | PARTIAL | M | H | — | PARITY.md:445 |
| 1 | expiry-first — Ordering implemented + registered (M3); no expiry data exists anywhere, so the deadline falls back to the n... | PARTIAL | M | H | expiry data source | PARITY.md:447 |
| 1 | headroom — Ordering implemented + registered (M3); utilisation is still unknown (-1, no per-pair limit exists anywhere... | PARTIAL | M | H | per-pair limit source (product decision) | PARITY.md:448 |
| 1 | context-optimized — Largest-window-first ordering implemented + registered (M3); context windows are not a hook input yet, so i... | PARTIAL | M | H | — | PARITY.md:453 |
| 1 | cache-optimized — Affinity ordering implemented + registered (M3); cache warmth is not a hook input yet, so it returns declar... | PARTIAL | M | H | — | PARITY.md:454 |
| 1 | quota-share (internal, 23rd) — DRR wired into the engine (Phase D, D4, internal/router/cascade.go): per-pool deficits, 2:1 rotation verifi... | PARTIAL | M | H | operator share config | PARITY.md:458 |
| 1 | Account-level fallback subset (9 values) — Account fallback strategies built + wired (M3 residual S1): 9 values via ParseAccountFallbackStrategy; glob... | PARTIAL | M | H | D2 per-key quota signal — no per-key quota source exists (OmniRoute #20561) Row 388; V-9 confirmed 2026-11-20. | PARITY.md:459 |
| 1 | Router strategy score — Same 5-factor scorer | PARTIAL | M | H | — | PARITY.md:462 |
| 1 | Mode pack chaos-mode — Weight pack wired as a selectable strategy (M3); parallel fan-out now live via the D3 combo path; fault-inj... | PARTIAL | M | H | — | PARITY.md:473 |
| 1 | Live intelligence (Arena ELO + models.dev) — models.dev sync exists but opt-in OFF by default; no Arena ELO, no ELO/tier signal in scoring | PARTIAL | M | H | — | PARITY.md:474 |
| 1 | Candidate-pool endpoint — Route registered GET /v1/auto-combo/{channel}/candidates (Phase D, D1) with the live pool: tier models, bre... | PARTIAL | M | H | — | PARITY.md:475 |
| 1 | auto/chaos — Panel of 5, one model per provider; parallel fan-out now engine-wired via the chaos-mode combo path (Phase ... | PARTIAL | M | H | — | PARITY.md:484 |
| 1 | auto/subscription — Fail closed, rung-0 only, never a billable fallback; engine dispatch live (Phase D, D2) — still fails close... | PARTIAL | M | H | — | PARITY.md:485 |
| 1 | auto/thrifty — 5-rung ladder (subscription→keyless→free→cheap→premium) live at dispatch (Phase D, D2: resolveThrifty → cos... | PARTIAL | M | H | — | PARITY.md:486 |
| 1 | auto/best-* family — Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); vision + reasoning fail open (no vis... | PARTIAL | M | H | — | PARITY.md:488 |
| 1 | auto/pro-* family — Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); pro-* fail open in production (no pr... | PARTIAL | M | H | — | PARITY.md:489 |
| 1 | 4-tier fallback cascade (Subscription → API Key → Cheap → Free) — Cascade ordering built and tested (Phase C); engine helper applyCascadeOrder built and tested with injected... | PARTIAL | M | H | — | PARITY.md:492 |
| 1 | Endpoint-specific combos (chat, search, embeddings, rerank, images, video, music, OCR, audio) — Endpoint projection built and tested (Phase C); enforced at all three live engine entry points (Phase D, D4... | PARTIAL | M | H | provider client | PARITY.md:493 |
| 1 | Shared attempt budget maxGlobalAttempts, clamped [1, 200] — MaxAttempts default 5, no upper clamp | PARTIAL | M | H | — | PARITY.md:494 |
| 1 | Nested combos (maxComboDepth 3, flatten) — No combo-in-combo. We have nestable strategy trees instead (different, arguably more expressive) | PARTIAL | M | H | — | PARITY.md:495 |
| 1 | Hedging config (off by default, 500 ms, bodies >256 KB never hedged) — Hedging ON by default at 150 ms, disabled for explicit-order chains — faster default, but no body-size guard | PARTIAL | M | H | — | PARITY.md:498 |
| 4 | MCP scope enforcement — PARTIAL: MCP is stdio-only, 3 read-only tools, no spend path — but also no auth/scoping at all | PARTIAL | M | H | — | PARITY.md:609 |
| 4 | Per-key policy fields — PARTIAL: only expiry, RPM/TPM, per-key USD budget, per-key guardrails allowlist (genuinely enforced) | PARTIAL | M | H | — | PARITY.md:611 |
| 4 | Per-key IP allowlist / blocklist — MISSING: no IP field or check anywhere | MISSING | M | H | — | PARITY.md:612 |
| 4 | Per-key device tracking — MISSING: no device/IP/User-Agent tracking on keys | MISSING | M | H | — | PARITY.md:613 |
| 4 | Key policy templates / key import — MISSING: no template or gateway-key import | MISSING | M | H | — | PARITY.md:614 |
| 4 | Self-service key introspection — PARTIAL: GET /v1/me/status is FULL and richer on budget/traffic; GET /v1/me/sessions does not exist | PARTIAL | M | H | — | PARITY.md:626 |
| 4 | Key creation via API / dashboard — MISSING: no POST /admin/keys create route; dashboard "+ New router key" button explicitly unwired | MISSING | M | H | — | PARITY.md:627 |
| 7 | SSN — No SSN pattern anywhere | MISSING | M | H | — | PARITY.md:716 |
| 7 | CPF / CNPJ (BR IDs) | MISSING | M | H | — | PARITY.md:719 |
| 7 | IP address v4/v6 | MISSING | M | H | — | PARITY.md:720 |
| 7 | IBAN + EU national IDs (8 GDPR types) | MISSING | M | H | — | PARITY.md:721 |
| 7 | ZA IDs (4 POPIA types) | MISSING | M | H | — | PARITY.md:722 |
| 7 | Per-type toggles — Only 3 kind toggles (BlockPII/BlockSecrets/BlockInjection); no per-type selection | MISSING | M | H | — | PARITY.md:730 |
| 7 | Custom regex patterns | MISSING | M | H | — | PARITY.md:731 |
| 7 | Request rewrite before provider — Log-only by design: Check/CheckScoped never mutate the request; with blocking off a secret goes to the prov... | MISSING | M | H | — | PARITY.md:733 |
| 7 | Response sanitization, streaming (SSE) — Streaming responses are never response-checked; only async-guardrail cut can stop a stream | MISSING | M | H | — | PARITY.md:735 |
| 7 | Modality bridges (vision/audio/video reroute) | MISSING | M | H | — | PARITY.md:745 |
| 9 | MCP SSE remote transport — None — stdio only | MISSING | M | H | — | PARITY.md:867 |
| 9 | MCP Streamable HTTP remote transport | MISSING | M | H | — | PARITY.md:868 |
| 9 | Tool breadth — 3 read-only tools | PARTIAL | M | H | — | PARITY.md:869 |
| 9 | Routing/combo tools | MISSING | M | H | — | PARITY.md:870 |
| 9 | Inference/spend tool — None, deliberate — no sampling/chat tool, "a tool call can never spend quota" | MISSING | M | H | — | PARITY.md:871 |
| 9 | Cost/quota tools — None on MCP; per-request budget header exists only on HTTP chat | MISSING | M | H | — | PARITY.md:872 |
| 9 | Cache tools | MISSING | M | H | — | PARITY.md:873 |
| 9 | DB health + pricing sync | MISSING | M | H | — | PARITY.md:874 |
| 9 | Memory tools | MISSING | M | H | — | PARITY.md:875 |
| 9 | Skills tools | MISSING | M | H | — | PARITY.md:876 |
| 9 | Plugin tools | MISSING | M | H | — | PARITY.md:877 |
| 9 | Context-source tools | MISSING | M | H | — | PARITY.md:878 |
| 9 | Pool/gamification/compression/CCR tools | MISSING | M | H | — | PARITY.md:879 |
| 9 | Tool search — None (nothing to search at 3 tools) | MISSING | M | H | — | PARITY.md:880 |
| 9 | Perplexity research tool | MISSING | M | H | — | PARITY.md:881 |
| 9 | Scope system (33 tool scopes) — No scope concept on MCP at all | MISSING | M | H | — | PARITY.md:883 |
| 9 | Scope enforcement default — No enforcement (nothing to enforce on 3 read-only tools) | MISSING | M | H | — | PARITY.md:884 |
| 9 | Forced enforcement for narrow HTTP keys (#13330) — None (no remote transport, no per-key auth) | MISSING | M | H | — | PARITY.md:885 |
| 9 | Per-client identity — Stdio also has no per-client identity (same gap, but only 3 read-only tools) | PARTIAL | M | H | — | PARITY.md:887 |
| 9 | Audit logging of tool calls — No MCP audit log; only in-process guardrail evaluation counters | MISSING | M | H | — | PARITY.md:888 |
| 9 | Heartbeat / status telemetry — GET /dashboard/mcp feed: same 3-tool list, live provider/model counts, selector state + counters; no heartb... | PARTIAL | M | H | — | PARITY.md:889 |
| 9 | Tool-cardinality controls — Server announces all 3 always; client side exceeds OmniRoute: selector mode injects exactly 2 meta-tools | PARTIAL | M | H | — | PARITY.md:890 |
| 9 | Spend controls on MCP tools — Server: no spend possible by construction. Client: mcp_tools_execute has no spend cap, cost estimate or all... | PARTIAL | M | H | — | PARITY.md:892 |
| 9 | Dashboard MCP page/feed — GET /dashboard/mcp + "MCP & CLI" section incl. selector-mode card, audit view and transport toggles (POST /... | PARTIAL | M | H | — | PARITY.md:895 |
| 9 | Protocol version — 2025-03-26 — current surface; neither side ships sampling/elicitation | PARTIAL | M | H | — | PARITY.md:896 |
| 9 | Agent Card / discovery | MISSING | M | H | — | PARITY.md:902 |
| 9 | Conductor delegation | MISSING | M | H | — | PARITY.md:905 |
| 10 | Read-lifecycle engine | MISSING | M | H | — | PARITY.md:933 |

## Tier 3 — H+L (area-unlocking only) — 7 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 1 | auto (16-factor pool) — auto/* resolves a model via embedding tiers; the auto/* variant resolver exists with real pick rules (Phase... | PARTIAL | L | H | 16-factor scorer completion (11 of 16 factors missing) Unlocks Area 1 ≥80% weighted (M3 gate). | PARITY.md:451 |
| 1 | auto 16-factor scoring (DEFAULT_WEIGHTS sum 1.0) — 5 factors only; taskFit, tierPriority/Affinity, specificityMatch, stability, health, context/cache/session ... | PARTIAL | L | H | 16-factor scorer completion (11 of 16 factors missing) Unlocks Area 1 ≥80% weighted (M3 gate). | PARITY.md:460 |
| 1 | Router strategy rules (default) — 5-factor balanced scorer is the analogue, 11 of 16 factors missing | PARTIAL | L | H | 16-factor scorer completion (11 of 16 factors missing) Unlocks Area 1 ≥80% weighted (M3 gate). | PARITY.md:461 |
| 1 | auto (balanced, LKGP-sticky) — auto picks any tier via embedding; no LKGP stickiness, no 16-factor pool | PARTIAL | L | H | 16-factor scorer completion (11 of 16 factors missing) Unlocks Area 1 ≥80% weighted (M3 gate). | PARITY.md:477 |
| 4 | Dashboard authentication — MISSING: no dashboard password, session, or login of any kind; protection is loopback-bind only | MISSING | L | H | none — new subsystem (password/session/login; loopback-bind only today) Unlocks dashboard trust; Area 8b use depends on it. | PARITY.md:619 |
| 4 | OAuth / JWT gateway auth — MISSING: gateway clients use gateway keys only; OAuth/JWT exist solely as upstream provider auth | MISSING | L | H | none — new subsystem (OAuth 2.0 PKCE + JWT) Unlocks gateway auth beyond gateway keys. | PARITY.md:628 |
| 7 | Reversible PII token vault | MISSING | L | H | vault design (reversible tokens, key management, restart survival) Unlocks Area 7 PII story. | PARITY.md:732 |

## Tier 4 — M+S — 175 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 3 | Providers with a routable base URL — 379/610 (62%) have BaseURLs; 16 templates/loopback; 231 cannot be added | PARTIAL | S | M | — | PARITY.md:571 |
| 3 | Static per-provider model list — No static list; models only from live /v1/models probe at add | PARTIAL | S | M | — | PARITY.md:574 |
| 3 | models.dev sync — OFF by default; syncs prices + context windows only, operator values always win | PARTIAL | S | M | — | PARITY.md:576 |
| 3 | Custom-provider prefix convention — No openai-compatible-* prefix convention; generic custom only | PARTIAL | S | M | — | PARITY.md:579 |
| 3 | Free-tier marking — HasFreeTier ×162; flag stored but surfaced nowhere — zero references in server/dashboard | PARTIAL | S | M | — | PARITY.md:582 |
| 3 | Free-tier budget / pools / rankings — No pools, no budget aggregation, no rankings; only per-provider flags + notes | MISSING | S | M | — | PARITY.md:583 |
| 3 | Key-creation links — None: no field, no API exposure, no dashboard link | MISSING | S | M | — | PARITY.md:584 |
| 3 | Deprecated / retired handling — DeprecatedModels seed table, 16 entries (all OpenAI); /v1/models hides them by default; no provider-removal... | PARTIAL | S | M | — | PARITY.md:588 |
| 6 | Semantic similarity method — Hashed bag-of-words, 256-dim, unigrams+bigrams, cosine — NOT neural embeddings, honestly documented | PARTIAL | S | M | — | PARITY.md:680 |
| 6 | Persistence tiers — SQLite persistence for both layers, write-through (every Set reaches disk synchronously), durable flush (di... | PARTIAL | S | M | — | PARITY.md:688 |
| 6 | Cache analytics — GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only); n... | PARTIAL | S | M | — | PARITY.md:697 |
| 6 | Cache-write tiers (5-min / 1-h) — Ledger stores + costs per tier (5m 1.25×, 1h 2×, explicit-rate-wins, claude-gated fallback); pipeline live ... | PARTIAL | S | M | — | PARITY.md:700 |
| 6 | Provider prompt-cache markers — LIVE on Anthropic only (post-conversion annotation in anthropicClient, temp 0, no tools, ≥1024 est. tokens)... | PARTIAL | S | M | — | PARITY.md:701 |
| 6 | Reasoning cache (2 h) — Memory-only replay: 2 h TTL, 200 entries, tool-call + conversation keys, full signed state stored/injected ... | PARTIAL | S | M | — | PARITY.md:704 |
| 6 | Search cache (60 s) — Semantics fully implemented + tested (generic SearchCache[T], 60 s TTL, 500 entries, GetOrCoalesce, ttl≤0 b... | PARTIAL | S | M | — | PARITY.md:705 |
| 7 | Phone PII — phone rule: +91, +1, 3-3-4, 10-digit IN only; no BR/other countries | PARTIAL | S | M | — | PARITY.md:715 |
| 7 | Credit card — card rule matches any 16 digits; no Luhn — false positives on any 16-digit run | PARTIAL | S | M | — | PARITY.md:717 |
| 7 | Provider-secret patterns — 20 named secret rules + generic-token rule. Missing: Replicate r8_, Linear, Notion, npm, Postman, Discord, ... | PARTIAL | S | M | — | PARITY.md:723 |
| 7 | Response sanitization, non-streaming — Response checked but block-or-deliver only: no redact mode | PARTIAL | S | M | — | PARITY.md:734 |
| 7 | Prompt-injection detection — 11 phrase patterns. No severity scoring, no custom patterns; scans full text (stronger coverage, unbounded ... | PARTIAL | S | M | — | PARITY.md:736 |
| 7 | Dashboard + counters — Full Guardrails page: blocking toggles, per-(phase,action) eval counts, per-kind counts, last 50 events, se... | PARTIAL | S | M | — | PARITY.md:747 |
| 7 | Route coverage of checks — Request check on chat, /v1/messages relay, /v1/responses, embeddings and rerank; MCP tool args; response ch... | PARTIAL | S | M | — | PARITY.md:748 |
| 8a | 4-step quick-start (create key → connect → point client → monitor) + Full Docs link — No 4-step guide. Overview has "Point your app at the router" code block; Providers has a 3-step strip. No c... | PARTIAL | S | M | — | PARITY.md:760 |
| 8a | Quick-start show/hide toggle (default on) — No toggle found | MISSING | S | M | — | PARITY.md:761 |
| 8a | Provider topology widget — Provider health card with status chips + 24h uptime bars + p50 — no topology view, no per-provider model co... | PARTIAL | S | M | — | PARITY.md:762 |
| 8a | System metrics cards (CPU, Memory, host + process) — No CPU/memory card on Overview (memstats live in Benchmark view) | MISSING | S | M | — | PARITY.md:764 |
| 8a | Provider quota widget — No quota widget on Overview | MISSING | S | M | — | PARITY.md:765 |
| 8a | Update flow (Update Available, Check, Update Now, Restart & Install, Release Notes, downloads) — No update UI anywhere on Overview | MISSING | S | M | — | PARITY.md:766 |
| 8a | Provider groups as always-visible addable sections — One flat searchable catalog table (configured pinned on top, Add/Manage per row, ToS-grey Add disabled with... | PARTIAL | S | M | — | PARITY.md:770 |
| 8a | Per-group "Test all" buttons — Zero "Test all" strings in our file | MISSING | S | M | — | PARITY.md:771 |
| 8a | Provider Onboarding Wizard — Single add-provider form only; no wizard | MISSING | S | M | — | PARITY.md:772 |
| 8a | Import from file / OAuth export-import modals | MISSING | S | M | — | PARITY.md:773 |
| 8a | Repair env (restore missing OAuth defaults) | MISSING | S | M | — | PARITY.md:774 |
| 8a | Add OpenAI / Anthropic / CC Compatible buttons — Custom mode = "any OpenAI-compatible base URL"; no explicit Anthropic- or CC-compatible add buttons | PARTIAL | S | M | — | PARITY.md:775 |
| 8a | Per-provider card: status, Disabled, Rate-limited, error types, expiration banner — Health chips, verification badges, uptime bars, p50 — no error-type breakdown, no expiration banner | PARTIAL | S | M | — | PARITY.md:776 |
| 8a | Provider detail page (connections, quota panel, cooling, models, override editors, playground, import modals) — No detail view; Manage scrolls to the grid card | MISSING | S | M | — | PARITY.md:777 |
| 8a | Search (text + model search + category/serviceKind filter) — Text search over name/id/alias; no category/serviceKind filter | PARTIAL | S | M | — | PARITY.md:778 |
| 8a | Configured-only filter / configured count — Configured pinned on top; no configured-only filter toggle | PARTIAL | S | M | — | PARITY.md:779 |
| 8a | Free Tier Providers group — No free-tier grouping; catalog table has no free-tier column | MISSING | S | M | — | PARITY.md:780 |
| 8a | Test Results modal ("{passed}/{total} passed") | MISSING | S | M | — | PARITY.md:781 |
| 8a | Add-form fields (Provider, Connection name, Display name, Provider prefix, Region, Protocol, Base URL, Chat/Models path, API key, Custom User-Agent) — Ours: Source, catalog select, Endpoint, Variant, Base-URL override, Display name, Provider API key, Gateway... | PARTIAL | S | M | — | PARITY.md:782 |
| 8a | OAuth connect flow (sign-in, token rotation) — API-key-only add; OAuth entries ToS-grey-disabled or API-key surfaced; no OAuth connect flow | MISSING | S | M | — | PARITY.md:783 |
| 8a | Models title/subtitle "Browse and health-test models and combo routing pipelines" — "Models / Only models from your connected providers" — no health-test or combo mention | PARTIAL | S | M | — | PARITY.md:784 |
| 8a | Tabs Models (n) / Combos (n) — No tabs in Models view | MISSING | S | M | — | PARITY.md:785 |
| 8a | Models table columns (Provider/Model/Type/Capabilities/Context/Max output/Flags/Custom/Free) — Ours: Model/Provider/Context/Input per 1M/Output per 1M/Price source/Requests/Fit/Status. Missing Type, Cap... | PARTIAL | S | M | — | PARITY.md:786 |
| 8a | Filters (provider/type/capability/min-context/min-output/free, etc.) — Provider select, Category select, Max price, Min context, Shortlist only, Include deprecated, text search. ... | PARTIAL | S | M | — | PARITY.md:787 |
| 8a | "{visible} visible · {hidden} hidden · {total} total" counts — #modelCount element exists; no visible/hidden split observed | PARTIAL | S | M | — | PARITY.md:788 |
| 8a | Per-row visibility toggle | MISSING | S | M | — | PARITY.md:789 |
| 8a | Per-row health test + test-result badge — None per model (health exists per provider only) | MISSING | S | M | — | PARITY.md:790 |
| 8a | Bulk test ("Start bulk test?" + quota warning) | MISSING | S | M | — | PARITY.md:791 |
| 8a | Empty states ("No models are available yet" / "No models match these filters" / "Clear filters") — "No models yet — connect a provider first. [Add a provider]" (one line + button). No "no match" state, no C... | PARTIAL | S | M | — | PARITY.md:793 |
| 8a | Pricing as separate per-model rate table (Input/Output/Cached/Reasoning/Cache Write $/1M + By provider/Compare views + coverage filters + sort) — No Pricing page/view at all — zero "pricing" matches in our file | MISSING | S | M | — | PARITY.md:794 |
| 8a | Pricing sync (Auto Sync, Sync Now, Last/Next sync, Synced Models, Clear Synced; sources User Override / Models.dev / LiteLLM / Built-in Default) — Only a status line #modelSyncStatus ("Live price/context sync: checking…"); no sync controls | PARTIAL | S | M | — | PARITY.md:795 |
| 8a | Catalog Sections grouping — Flat table, no sections | MISSING | S | M | — | PARITY.md:796 |
| 8a | Pagination — Not found in source (table renders full list) | MISSING | S | M | — | PARITY.md:797 |
| 8b | Endpoint behavior per-capability tabs | MISSING | S | M | — | PARITY.md:826 |
| 8b | Default Auto Variant (10 values) — 6 virtual models (auto, auto/fast, auto/cheap, auto/coding, auto/code, auto/reasoning + intent/*); no route... | PARTIAL | S | M | — | PARITY.md:827 |
| 8b | Session stickiness toggle — The display-only switch is gone: Settings shows a read-only status line (pinning not reported by any live f... | MISSING | S | M | — | PARITY.md:828 |
| 8b | Create API key (dashboard) — "+ New router key" button exists but not wired — honest toast "isn't wired yet" | PARTIAL | S | M | — | PARITY.md:829 |
| 8b | Single-key disable/revoke — Only revoke-all exists | MISSING | S | M | — | PARITY.md:833 |
| 8b | Daily / Weekly / Monthly $ limit per key — Only rolling 5h exists | MISSING | S | M | — | PARITY.md:834 |
| 8b | Quota-share pools | MISSING | S | M | — | PARITY.md:836 |
| 8b | Team cost centers | MISSING | S | M | — | PARITY.md:837 |
| 8b | Cost Overview KPIs (Spend 1D/7D/30D, requests, active providers/models, avg cost/req) — Spend via 24h/7d/30d range seg + tokens + failovers; no side-by-side 1D/7D/30D, no avg cost/req | PARTIAL | S | M | — | PARITY.md:838 |
| 8b | Monthly forecast | MISSING | S | M | — | PARITY.md:839 |
| 8b | Cost Explorer (dimension filter, top 50, CSV/JSON export) — None (CSV export of logs only) | MISSING | S | M | — | PARITY.md:840 |
| 8b | Combo Health (quota risk, forecast, Gini skew, autopilot, scoring inspector) | MISSING | S | M | — | PARITY.md:841 |
| 8b | Proxy Logs / Console output / Timeline / Activity feed / Conversations / Compression log / Audit log — None (latest-route-trace card only is the timeline cousin; guard denials counted only, no audit view) | MISSING | S | M | — | PARITY.md:843 |
| 8b | Automatic failover / Prompt-cache pinning / Cost-aware routing / Health checks / Request logging switches — The 5 display-only switches are gone: all five are honest read-only status lines fed by live endpoints; no ... | MISSING | S | M | — | PARITY.md:845 |
| 10 | Engine registry / stacked pipelines — Fixed 3-step pipeline only; no registry, no per-engine selection | MISSING | S | M | — | PARITY.md:918 |
| 10 | Lite whitespace/format cleanup — Whitespace strip outside fences + system dedupe; no tool-result truncation cap | PARTIAL | S | M | — | PARITY.md:919 |
| 10 | Session-dedup (cross-turn, content-addressed, [dedup:ref sha=…]) — Only same-request duplicate system messages; no cross-turn store, no markers | MISSING | S | M | — | PARITY.md:920 |
| 10 | CCR (retrieval markers, 24 h + SQLite) | MISSING | S | M | — | PARITY.md:921 |
| 10 | RTK tool-output filter (56 JSON packs, ANSI/progress strip, failures+tails kept) — Built + wired into Apply via mode=rtk / mode=stacked (explicit mode only; a bare lossy request stays extrac... | PARTIAL | S | M | — | PARITY.md:922 |
| 10 | Codex Responses tool-output compression | MISSING | S | M | — | PARITY.md:923 |
| 10 | Headroom tabular JSON → columnar (SmartCrusher, lossless) | MISSING | S | M | — | PARITY.md:924 |
| 10 | Ionizer tabular row sampling | MISSING | S | M | — | PARITY.md:925 |
| 10 | Relevance extractive sentence scoring | MISSING | S | M | — | PARITY.md:926 |
| 10 | Caveman rule-based prose condensation (levels lite/full/ultra, user-role default, min 50 chars) — Built + wired into Apply via mode=standard / mode=stacked (explicit mode only): CondenseMessages (caveman.g... | PARTIAL | S | M | — | PARITY.md:927 |
| 10 | Aggressive summarize + progressive aging (~50% claim) — Oldest-turn dropping only, no summariser (honestly documented) | MISSING | S | M | — | PARITY.md:928 |
| 10 | LLMLingua-2 ONNX semantic pruning (~57 MB TinyBERT, fail-open) | MISSING | S | M | — | PARITY.md:929 |
| 10 | Ultra heuristic pruning + code thinning + binary-search truncation (~75% claim) — None (lossy truncation is not Ultra) | MISSING | S | M | — | PARITY.md:930 |
| 10 | OmniGlyph context-as-image (Claude Fable 5 only) — None — deliberate do-not-build | MISSING | S | M | — | PARITY.md:931 |
| 10 | LLM compressor (model call to compress) — None — deliberate do-not-build | MISSING | S | M | — | PARITY.md:932 |
| 10 | MCP tool/prompt/resource description compression, default ON — None — we never rewrite tool descriptions, so we do not carry this correctness risk | MISSING | S | M | — | PARITY.md:934 |
| 10 | MCP accessibility tool-result filtering (smartFilterText, truncation hints) — Only last-per-tool_call_id canonicalisation | MISSING | S | M | — | PARITY.md:935 |
| 10 | Stacked pipeline RTK → Caveman — RunStacked wired into Apply via mode=stacked with DefaultAvailability (RTK+Caveman live; stale false/false ... | PARTIAL | S | M | — | PARITY.md:936 |
| 10 | Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked) — Modes parsed + validated (ParseMode; server 400s an unknown mode naming the field) and wired into Apply: rt... | PARTIAL | S | M | — | PARITY.md:937 |
| 10 | Adaptive budget dial + escalation ladder — PickCheapestFitting + EscalateFrom built + tested (modes.go), ladder lite→standard→stacked; NOT wired as th... | PARTIAL | S | M | — | PARITY.md:938 |
| 10 | System-prompt preservation modes (always/whenNoCache/never) — System/developer always pinned in lossy; no configurability, no cache-conditional mode | PARTIAL | S | M | — | PARITY.md:940 |
| 10 | Hard-budget post-pass (compress to ≤ N tokens) — Wired: post-pass runs after applyLossy when the count is still over budget (same transform class, so it als... | PARTIAL | S | M | — | PARITY.md:941 |
| 10 | Protection guarantees (code/URLs/JSON/identifiers) — Code fences protected in whitespace step; part-carrying messages never partially rewritten; M7.2 fidelity g... | PARTIAL | S | M | — | PARITY.md:942 |
| 10 | Fidelity gate / risk gate / validation — VerifyLossless structural equivalence check (normal-form + idempotence, fail → send original) for lossless;... | PARTIAL | S | M | — | PARITY.md:943 |
| 10 | Worker pool / async compression — Built + unit-tested 10/10 under -race and independently probed (Submit mutex fix toggle-proven by TestPoolS... | PARTIAL | S | M | — | PARITY.md:945 |
| 10 | Eval harness + measured savings corpus — Eval harness internal/compress/evalharness/ (harness + fixed corpus + goldens; 8 tests, -race green, toggle... | PARTIAL | S | M | — | PARITY.md:946 |
| 10 | Output styles (Terse prose, Less code, Ponytail, "I have ADHD", Terse CJK) | MISSING | S | M | — | PARITY.md:947 |
| 10 | Native non-OpenAI tokenizers (Claude/Gemini/Qwen) — None — cl100k/o200k only, Fallback=true label | MISSING | S | M | — | PARITY.md:953 |
| 10 | Own-cache tokens_saved accounting — Own exact/semantic hits record zero tokens: degrade.go:710 ("the ledger … records nothing") — verified fresh | MISSING | S | M | — | PARITY.md:958 |
| 10 | Runtime calibration vs provider-reported usage — Built + wired (M7.9 + merge wave): calibrator (calibration.go) matches OmniRoute's shape exactly (α=0.25, ≥... | PARTIAL | S | M | — | PARITY.md:961 |
| 11 | Session / project / client attribution + /v1/me/sessions — No session/project/client field anywhere in ledger or server; no /v1/me/sessions | MISSING | S | M | — | PARITY.md:972 |
| 11 | Cost computation (tokens × price, all legs) — Prices.Cost prices prompt + completion only; no cached-input leg, no per-endpoint price dimension | PARTIAL | S | M | — | PARITY.md:975 |
| 11 | Gateway-reported cost precedence / exact-cost reconciliation — provider.Usage has no cost field at all; cost is always hand-computed; no reconciliation target | MISSING | S | M | — | PARITY.md:976 |
| 11 | Price coverage out of the box — No LiteLLM sync, no hardcoded defaults; unpriced model = unknown, cost routing blind until operator prices it | PARTIAL | S | M | — | PARITY.md:978 |
| 11 | Flat-rate / subscription $0 treatment — No flat-rate/subscription classification anywhere; a subscription provider with no configured price is "unk... | MISSING | S | M | — | PARITY.md:979 |
| 11 | Cache-write tier accounting (5-min vs 1-hour) — Single undifferentiated CacheWriteTokens total; dashboard economics uses a 1.25×–2.5× band (high end exceed... | PARTIAL | S | M | — | PARITY.md:980 |
| 11 | Refused / fallback-attempt billing — Every attempt recorded: success rows carry real tokens+cost; failed attempts recorded as ok=false zero-toke... | PARTIAL | S | M | — | PARITY.md:982 |
| 11 | Cost-saved reporting (cache / compression savings) — Compression saved tokens recorded per row; cache-read-vs-full economics in dashboard feed — but own exact/s... | PARTIAL | S | M | — | PARITY.md:986 |
| 11 | Analytics endpoints — /dashboard/usage, /dashboard/key-budgets, /dashboard/prices + GET /api/analytics/summary, /by-provider, /by... | PARTIAL | S | M | — | PARITY.md:993 |
| 11 | Retention + export — No retention/TTL on usage_events — grows unbounded; only manual wipe/reset. Export = DB-export bundle cappe... | PARTIAL | S | M | — | PARITY.md:994 |
| 11 | Idempotency-Key billing safety — MISSING — zero hits in internal/ + cmd/; client retries after a timeout can double-charge | MISSING | S | M | — | PARITY.md:996 |
| 11 | Batch / flex service-tier pricing — MISSING — no service_tier handling; no batch API, no batch/flex price tier in the price registry | MISSING | S | M | — | PARITY.md:997 |
| 13 | Prebuilt binary install — No release exists; only build-from-source works | MISSING | S | M | — | PARITY.md:1062 |
| 13 | npm package — package.json exists (name: codvian-router, bin, version 0.1.0) + bin/codvian-router.js downloader wrapper —... | PARTIAL | S | M | — | PARITY.md:1064 |
| 13 | Published Docker image — No published image; GHCR target ghcr.io/raunakiitp/codvian-router only in .goreleaser.yml dockers_v2 config... | PARTIAL | S | M | — | PARITY.md:1065 |
| 13 | docker-compose — 1 file: single service, command serve --listen 0.0.0.0:20128, 2 named volumes, healthcheck. Parses; never e... | PARTIAL | S | M | — | PARITY.md:1067 |
| 13 | CLI breadth — 17 top-level subcommands: serve, init, wizard, run, keygen, eval, eval-feedback, validate, tray, doctor, co... | PARTIAL | S | M | — | PARITY.md:1070 |
| 13 | --version / -v — No --version/-v flag and no version subcommand (only Version = "0.1.0" const, printed on serve/init startup... | MISSING | S | M | — | PARITY.md:1071 |
| 13 | Eval tooling — eval (smoke-eval, --model) + eval-feedback (A/B feedback heuristic) — narrower, no eval framework/watch UI | PARTIAL | S | M | — | PARITY.md:1073 |
| 13 | Provider management CLI — provider list/add/add-catalog/remove/import — functional but far narrower (no test-all, no model CRUD) | PARTIAL | S | M | — | PARITY.md:1076 |
| 13 | Tray — tray subcommand exists but is only a headless supervisor (Start/Stop/Restart, auto-restart, no GUI icon, no... | PARTIAL | S | M | — | PARITY.md:1078 |
| 13 | Releases published — 0 releases, 0 git tags; release pipeline exists only as config (.goreleaser.yml, .github/workflows/release.... | MISSING | S | M | — | PARITY.md:1081 |
| 13 | SBOM — Configured: syft SPDX SBOM per archive (.goreleaser.yml sboms:) + dependency-free CycloneDX fallback (cmd/r... | PARTIAL | S | M | — | PARITY.md:1082 |
| 13 | Signing / provenance — Designed ahead, unproven live: cosign keyless over checksums.txt + each SBOM (.goreleaser.yml signs:), dock... | PARTIAL | S | M | — | PARITY.md:1083 |
| 13 | Multi-arch build — Configured: GoReleaser linux/amd64 + linux/arm64, setup-qemu-action v3.7.0 + setup-buildx-action v3.12.0 in... | PARTIAL | S | M | — | PARITY.md:1084 |
| 13 | Install script — scripts/install.sh (10.5 KB): OS/arch detection, refuses pipe-to-shell, cosign-verify of checksums.txt when... | PARTIAL | S | M | — | PARITY.md:1085 |
| 14a | POST /v1/responses + GET/DELETE/list — POST + GET {id} + GET list + DELETE {id} + GET {id}/input_items (M8.4) + GET /v1/responses/input_tokens (M8... | PARTIAL | S | M | — | PARITY.md:1138 |
| 14a | GET /v1/me/status — GET /v1/me/status — daily/weekly usage windows live (M5.6) alongside budget/traffic + account quotas; per-w... | PARTIAL | S | M | reviewer decision (verdict call, not a build) | PARITY.md:1139 |
| 14a | POST /v1/completions (legacy) — POST /v1/completions live (M8.2, wired merge wave): non-streaming prompt-string (or 1-element []string) → o... | PARTIAL | S | M | — | PARITY.md:1140 |
| 14a | GET/POST /v1/models/query, /health, /[...model] — GET /v1/models/query, /health live + GET /v1/models/{model...} live (M8.10, wired merge wave; {model...} wi... | PARTIAL | S | M | catalog per-model capability flags | PARITY.md:1142 |
| 14a | POST /v1/moderations — POST /v1/moderations live (M8.9, wired merge wave, scope chat): OpenAI shape, local guardrails only (PII/se... | PARTIAL | S | M | — | PARITY.md:1143 |
| 14a | POST /v1/search (+ /search/analytics) — POST /v1/search live (M8.20, wired merge wave): Perplexity passthrough, verbatim, replay-exempt. /search/an... | PARTIAL | S | M | — | PARITY.md:1144 |
| 14a | POST /v1/classify — GET /v1/classify live (M8.2, wired merge wave): local auto-router tiering via ResolveWithThreshold — same p... | MISSING | S | M | — | PARITY.md:1147 |
| 14a | GET /v1/explain/routing — GET /v1/explain/routing live (M8.2, wired merge wave): per-prompt classification + configured strategy + la... | PARTIAL | S | M | — | PARITY.md:1148 |
| 14a | POST /v1/segment — GET /v1/segment live (M8.2, wired merge wave): local sentence-split + per-segment classification — entirely... | MISSING | S | M | — | PARITY.md:1149 |
| 14a | Images: /v1/images/generations, /edits, /upscale — POST /v1/images/generations + /edits live (M8.5, wired merge wave): engine path via singleAttempt, verbatim... | PARTIAL | S | M | — | PARITY.md:1150 |
| 14a | Video: /v1/videos/generations, /v1/video-bridge/drilldown — POST /v1/videos/generations + GET /v1/videos/{id} live (M8.20, wired merge wave): Runway passthrough, task ... | PARTIAL | S | M | — | PARITY.md:1152 |
| 14a | Music: /v1/music, /generations, /plan, /detailed, /stream, /detailed/stream — POST /v1/music/generations live (M8.20, wired merge wave): MiniMax passthrough, synchronous, 200-envelope v... | PARTIAL | S | M | — | PARITY.md:1153 |
| 14a | Files: /v1/files, [id], [id]/content — POST/GET /v1/files, GET/DELETE /v1/files/{id}, GET /v1/files/{id}/content live (M8.7, wired merge wave): Op... | PARTIAL | S | M | — | PARITY.md:1154 |
| 14a | Batches: /v1/batches, [id], [id]/cancel, delete-completed — 6 routes live (POST /v1/batches, GET list/get, POST [id]/cancel, DELETE [id], DELETE delete-completed); rea... | PARTIAL | S | M | — | PARITY.md:1155 |
| 14a | Per-provider: /v1/providers/[provider]/chat/completions, /embeddings, /images/generations, /models, /limits, /voices, /voices/[voiceId] — POST /v1/providers/{provider}/chat/completions + GET /v1/providers/{provider}/models live (M8.12, wired mer... | PARTIAL | S | M | — | PARITY.md:1156 |
| 14a | Gemini-native /v1beta/models, /v1beta/models/[...path] — GET /v1beta/models + POST /v1beta/models/{rest...} (:generateContent, :streamGenerateContent) live (M8.13, ... | PARTIAL | S | M | — | PARITY.md:1158 |
| 14a | GET /v1/me/sessions, /me/sessions/[id] — GET /v1/me/sessions lists the caller's Responses sessions (root + all descendants), list-price cost + unpri... | PARTIAL | S | M | — | PARITY.md:1159 |
| 14a | GET /v1/auto-combo/[channel]/candidates — Route registered (M3 Phase D, D1) with the live pool: tier models, breaker states, per-key connections, coo... | PARTIAL | S | M | — | PARITY.md:1160 |
| 14a | Quotas/keys: /v1/quotas/check, /quota-recoveries/[id], /accounts/[id]/limits, /session-leases, /registered-keys (+[id], +[id]/revoke) — POST /v1/quotas/check live (M8.11, wired merge wave): pre-flight verdict from the real ledger gates (rollin... | PARTIAL | S | M | — | PARITY.md:1161 |
| 14a | Relay: /v1/relay/chat/completions, …/bifrost — — (ours has only an internal relay resolver for /v1/messages) | MISSING | S | M | — | PARITY.md:1162 |
| 14a | VSCode token-scoped surfaces /v1/vscode/** — — (deliberate scope: no VSCode extension) | MISSING | S | M | — | PARITY.md:1164 |
| 14b | OpenAPI — GET /openapi.json live from guard table + try-it page web/tryit.html (170 routes, tier/scope-correct) built... | PARTIAL | S | M | — | PARITY.md:1177 |
| 14b | Providers mgmt (61 files) — POST+DELETE /api/providers, GET /api/providers, GET /api/catalog (add/delete/list/catalog only) | PARTIAL | S | M | — | PARITY.md:1178 |
| 14b | Keys mgmt (14 files + 2 key-templates) — Provider-key add/delete/role + gateway rotate/revoke-all; no per-key policy API, no import/reveal | PARTIAL | S | M | — | PARITY.md:1179 |
| 14b | Combos (10 files) — GET/POST/PATCH/DELETE /v1/combos live (M8.14, wired merge wave): named-chain CRUD over config groups + chai... | PARTIAL | S | M | — | PARITY.md:1181 |
| 14b | Analytics (3 files + usage analytics) — Dashboard overview/logs/usage feeds + GET /api/analytics/summary, /by-provider, /by-model, /costs live (M8.... | PARTIAL | S | M | — | PARITY.md:1183 |
| 14b | Cache (4 files) — GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only) (M... | PARTIAL | S | M | — | PARITY.md:1184 |
| 14b | Resilience (5 files) — /dashboard/cooldown feed incl. breakers + GET /api/resilience live (M8.16, wired merge wave): live breaker/... | PARTIAL | S | M | — | PARITY.md:1185 |
| 14b | Pricing (5 files incl. pricing/sync, PATCH /api/pricing) — GET /dashboard/prices feed + GET /api/pricing and PATCH /api/pricing live (M8.15, wired merge wave; PATCH r... | PARTIAL | S | M | — | PARITY.md:1186 |
| 14b | MCP (7 files + stdio/SSE/Streamable-HTTP) — stdio server, 3 read-only tools + GET /dashboard/mcp + MCP client selector; no HTTP/SSE transport, no audit... | PARTIAL | S | M | — | PARITY.md:1187 |
| 14b | Plugins (8 files) — Feed + 501 load stub | PARTIAL | S | M | — | PARITY.md:1188 |
| 14b | Auth mgmt (12 files: login/logout/MFA/OIDC/SSO/CSRF/status) — Gateway keys + bootstrap only; no login/MFA/OIDC/SSO | PARTIAL | S | M | — | PARITY.md:1189 |
| 14b | Settings (92 files) — Config file + dashboard POSTs (strategies, cooldown, guardrails, key-budgets, feedback, intent-tiers, sugge... | PARTIAL | S | M | — | PARITY.md:1190 |
| 14b | Tunnels (9 files: cloudflared/ngrok/tailscale) — Local TCP tunnel only (start/stop/status) | PARTIAL | S | M | — | PARITY.md:1191 |
| 14b | DB backup (4 files) — POST /admin/db/export/import, wipe arm+fire (no scheduled backups) | PARTIAL | S | M | — | PARITY.md:1192 |
| 14b | Shutdown/restart — POST /admin/shutdown real; no restart endpoint | PARTIAL | S | M | — | PARITY.md:1194 |
| 14b | Logs — /dashboard/logs feed + GET /api/logs live (M8.16, wired merge wave; capture wired in main.go, double-redact... | PARTIAL | S | M | — | PARITY.md:1195 |
| 14b | Compression mgmt (7 files) — Internal only; summary in overview feed | PARTIAL | S | M | — | PARITY.md:1196 |
| 14b | Version-manager (6 files) — POST /admin/update is an honest 501 stub | MISSING | S | M | — | PARITY.md:1198 |
| 14b | Discovery (4 files) — — (catalog is static; no scan/verify API) | MISSING | S | M | — | PARITY.md:1201 |
| 14b | A2A (5 files + Agent Card) — — (zero A2A hits) | MISSING | S | M | — | PARITY.md:1202 |
| 14b | Teams / cost centers (4 files) — GET/POST /api/teams + GET/PATCH/DELETE /api/teams/{id} live (M8.18, wired merge wave): full CRUD + live bud... | PARTIAL | S | M | — | PARITY.md:1203 |
| 14b | Quota-share pools/groups/plans (11 files) — GET/POST /api/quota-pools + GET/PATCH/DELETE /api/quota-pools/{id} live (M8.18, wired merge wave): full CRU... | PARTIAL | S | M | — | PARITY.md:1204 |
| 14b | Embedded services (77 files) — — (deliberate scope) | MISSING | S | M | — | PARITY.md:1205 |
| 14b | Gamification/skills/memory/conductor/chaos/evals — — (deliberate scope) | MISSING | S | M | — | PARITY.md:1206 |
| 14b | Files/batches mgmt (4 files) — /v1/files API live (M8.7, wired merge wave — see §14a Files row); stored Responses objects only before that... | MISSING | S | M | — | PARITY.md:1207 |
| 14b | Sessions (1 file + v1 me/sessions) — GET /v1/me/sessions list live (M5.6); GET /v1/me/sessions/{id} detail live (M8.11, wired merge wave); proje... | PARTIAL | S | M | — | PARITY.md:1208 |
| 14b | Fallback chains API (fallback/chains) — Config-only chains; no API | MISSING | S | M | — | PARITY.md:1210 |
| 14b | OAuth mgmt (19 files) — POST /admin/provider/login is an honest 501 stub | MISSING | S | M | — | PARITY.md:1211 |
| 14b | Service install — POST /admin/service/install honest 501 | MISSING | S | M | — | PARITY.md:1212 |

## Tier 5 — M+M — 26 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 8b | Named combos CRUD (create/duplicate/import/export/test/enable) — No combos at all — chains are provider-model lists | MISSING | M | M | — | PARITY.md:825 |
| 9 | A2A JSON-RPC endpoint — None — no /a2a route, no JSON-RPC agent endpoint | MISSING | M | M | — | PARITY.md:899 |
| 9 | A2A skills | MISSING | M | M | — | PARITY.md:900 |
| 9 | A2A REST task routes | MISSING | M | M | — | PARITY.md:901 |
| 9 | A2A auth — None — no A2A surface to mis-authenticate | MISSING | M | M | — | PARITY.md:903 |
| 9 | A2A owner scoping | MISSING | M | M | — | PARITY.md:904 |
| 9 | A2A task persistence / states / streaming | MISSING | M | M | — | PARITY.md:906 |
| 9 | A2A enable flag + dashboard/audit | MISSING | M | M | — | PARITY.md:907 |
| 14a | POST /v1/multimodal-embeddings | MISSING | M | M | — | PARITY.md:1141 |
| 14a | POST /v1/systemone | MISSING | M | M | — | PARITY.md:1146 |
| 14a | Audio: speech, transcriptions, translations, speech-to-text, text-to-speech/[voiceId], sound-generation, voices | MISSING | M | M | — | PARITY.md:1151 |
| 14a | WebSocket /v1/ws | MISSING | M | M | — | PARITY.md:1157 |
| 14a | Vendor extras: /v1/fal/* (3), /v1/agents/* (5), /v1/antigravity, /v1/muse-code/models, /v1/api/chat | MISSING | M | M | — | PARITY.md:1163 |
| 14a | /v1/provider-plugin-manifest | MISSING | M | M | — | PARITY.md:1166 |
| 14a | Mgmt proxies: /v1/management/proxies*, /proxy-subscriptions* | MISSING | M | M | — | PARITY.md:1167 |
| 14a | Web/media: /v1/web/fetch, /v1/web/map, /v1/media/[id], /v1/voices | MISSING | M | M | — | PARITY.md:1168 |
| 14b | Translator (5 files) | MISSING | M | M | — | PARITY.md:1200 |
| 14b | Search mgmt (search/providers, search/stats) | MISSING | M | M | — | PARITY.md:1209 |
| 14c | Install / upgrade — No install path. POST /admin/plugin/load = honest 501 by design (compiled-in only, no .so/Wasm/JS — loading... | MISSING | M | M | — | PARITY.md:1227 |
| 14c | Activate / deactivate — None — enablement is config plugins.enabled list order at startup only | MISSING | M | M | — | PARITY.md:1228 |
| 14c | Per-plugin config — Config-file only (plugins.cost_cap_usd_per_request, header-add header/value); no API | PARTIAL | M | M | — | PARITY.md:1229 |
| 14c | Marketplace / scan | MISSING | M | M | — | PARITY.md:1230 |
| 14c | Hooks — 3 hooks: HookPreRequest (fail-closed 403/-32001 plugin_denied), HookPostResponse (fail-open), HookProviderA... | PARTIAL | M | M | — | PARITY.md:1231 |
| 14c | Built-ins shipped — 3 named built-ins: header-add, response-log, cost-cap (builtins.go:20-23, costcap.go:62; Builtins() map, co... | PARTIAL | M | M | — | PARITY.md:1232 |
| 14c | MCP plugin tools | MISSING | M | M | — | PARITY.md:1233 |
| 14c | Dashboard page — GET /dashboard/plugins (live registry snapshot, honest zero state; no install UI) | PARTIAL | M | M | — | PARITY.md:1234 |

## Tier 6 — L value (docs) + M+L — 1 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 14d | README structure mirroring — v2's section order tracks OmniRoute's; content depth does not | PARTIAL | M | L | — Content-depth rewrite; overlaps the row-1240 README fix. | PARITY.md:1246 |

## Tier 7 — ? size (check first, do not build blind) — 2 rows

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 7 | Connection strings (mongo/postgres/mysql/redis/amqp) | MISSING | ? | H | — STALE? critic be15f10 extended the connection-string guard (rediss/amqps/mariadb) — row may be stale; needs V re-check | PARITY.md:728 |
| 14b | Webhooks (5 files) — GET/POST /api/webhooks (list/create) + GET/DELETE /api/webhooks/{id} live (M8.19, wired merge wave); budget... | PARTIAL | ? | M | — STALE? critic run 2: PUT /api/webhooks/{id} is registered — remainder may be stale; needs V re-check, not a build | PARITY.md:1199 |

## Deferred — size L, unlocks no area

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 14d | Docs breadth — OmniRoute: docs/ 10,416 .md files (10,206 i18n + 210 English), llm.txt 503 lines, docs/openapi.yaml 19,558 ... | MISSING | L | L | — Docs program (API_REFERENCE, PROVIDER_REFERENCE, llm.txt, static OpenAPI), not one file. | PARITY.md:1245 |

## Excluded — approved scope (not work)

| Area | Description (feature — gap) | Verdict | Size | Value | Blocker | Source |
|---|---|---|---|---|---|---|

| 10 | Language packs for rules (11 locales) — None (English-only logic; no prose rules at all) | MISSING | — | — | approved-scope exclusion (SCOPE-PROPOSAL.md) | PARITY.md:948 |
| 13 | Launch/wrap external CLIs — Single generic run <cli> --model MODEL [-- args...] (ephemeral headless gateway + fresh key) — no per-tool ... | PARTIAL | — | — | approved-scope exclusion (SCOPE-PROPOSAL.md) | PARITY.md:1075 |
| 13 | Desktop app (Electron) — None — no Electron dir, no desktop build, no .app/.exe packaging | MISSING | — | — | approved-scope exclusion (SCOPE-PROPOSAL.md) | PARITY.md:1079 |
| 13 | Self-update mechanism — None. No self-update code; /admin/update is explicitly notImplementedAdmin("auto-update"). Only path is re-... | MISSING | — | — | approved-scope exclusion (SCOPE-PROPOSAL.md) | PARITY.md:1080 |
| 13 | Extra install surfaces — None of these exist (no deploy/, fly.toml, flake.nix, PWA manifest, AUR/Nix packaging) | MISSING | — | — | approved-scope exclusion (SCOPE-PROPOSAL.md) | PARITY.md:1086 |

## Wrong — present but behaving incorrectly (not queue items; fix or re-scope first)

| Area | Description | Verdict | Source |
|---|---|---|---|
| 12 | Measured failover-switch time (a number to quote) — Claimed "~53 ms failover / 1–2 ms switch" — unverified/mislabelled (see measured-numbers table) | WRONG | PARITY.md:1047 |
| 14d | "README rewritten to 593 lines in README.v2.md, in OmniRoute's structure" — WRONG on count: README.v2.md is 260 lines, README.md is 179 lines (measured 2026-11-19). Structure claim pa... | WRONG | PARITY.md:1240 |

- PARITY.md:1047 (Area 12, row 18): "Measured failover-switch time" — no true switch-time measurement exists; measure in a quiet benchmark window (L-LAT).
- PARITY.md:1240 (Area 14d): "README rewritten to 593 lines" — README.md is 179 lines, README.v2.md is 260 lines; rewrite to ~593 lines following OmniRoute structure, or formally re-scope the target (SCRIBE).

## Distinct blockers (rows they block)

- **16-factor scorer completion (11 of 16 factors missing)** — 4 row(s): 1:451, 1:460, 1:461, 1:477
- **summariser design (production summariser)** — 1 row(s): 1:439
- **expiry data source** — 1 row(s): 1:447
- **per-pair limit source (product decision)** — 1 row(s): 1:448
- **operator share config** — 1 row(s): 1:458
- **D2 per-key quota signal — no per-key quota source exists (OmniRoute #20561)** — 1 row(s): 1:459
- **provider client** — 1 row(s): 1:493
- **none — new subsystem (password/session/login; loopback-bind only today)** — 1 row(s): 4:619
- **none — new subsystem (OAuth 2.0 PKCE + JWT)** — 1 row(s): 4:628
- **vault design (reversible tokens, key management, restart survival)** — 1 row(s): 7:732
- **reviewer decision (verdict call, not a build)** — 1 row(s): 14a:1139
- **catalog per-model capability flags** — 1 row(s): 14a:1142

