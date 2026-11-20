# docs/PARITY.md — Feature matrix: Codvian Router vs OmniRoute

Rule (master prompt §1.1): updated in the same commit as the feature. Columns:
feature → OmniRoute evidence → Codvian status (FULL/PARTIAL/MISSING/WRONG/AHEAD/DIVERGENCE)
→ file:line → test name. Scoring: FULL counts 1, PARTIAL counts 0.5, MISSING/WRONG count 0.
AHEAD and DIVERGENCE rows are NOT in the denominator. Where the comparison gives no
verified file:line or test name, this file writes UNVERIFIED — never invent one.

## Sources

- Baseline: CODVIAN_MASTER_PROMPT v1 (2026-11-18), Part 2 — full comparison, 2026-11-18,
  code-verified by 15 area agents. OmniRoute clone `~/workspace/omniroute-reference/`
  (package.json v3.8.55, commit `4b9fc08a`); Codvian worktree read 2026-11-18 ~13:24–14:10 IST.
- Detail: `~/workspace/goals/codvian-router/hidden_files/omniroute-full-comparison-2026-11-18.md`
  (1,803 lines). Per-area tables below are seeded from that report; where an area table is
  condensed, the report is the authoritative row-level source.
- Verdicts: FULL = has the feature, working as specified. PARTIAL = present but
  incomplete/narrower/different in a material way (never counted as done). MISSING = absent.
  WRONG = present under the wrong name, or present but behaving incorrectly (incl. live bugs).
  AHEAD = ours-only capability exceeding OmniRoute, measured numbers only. DIVERGENCE =
  deliberate difference (e.g. playground removed, A2A excluded in Phase 1). EXTRA = ours-only
  dashboard content with no OmniRoute counterpart. UNVERIFIED = could not be checked.

## Headline (naive sum — read the de-duplication note before quoting)

691 rows (M6 mechanical recount 688 + M7 gate wave 1 row + M8.2 split
of the lumped classify/explain/segment row into three rows).
FULL 286, PARTIAL 150, MISSING 213, WRONG 1, AHEAD 28, DIVERGENCE 12,
plus 1 unclassified claims row (Area 14) and 1 row unaccounted inside
Area 8b's own header (its verdicts sum to 138 against 139 claimed).
Comparable total 650 (rows − AHEAD − DIVERGENCE − unclassified).
Strict parity 44.0%. Weighted parity 55.5%.

- Strict parity (FULL/comparable): 286/650 = **44.0%**
- Weighted parity ((FULL + 0.5×PARTIAL)/comparable): (286 + 75)/650 = **55.5%**

- M4 note (2026-11-18): the headline naive sum above is STALE — it still
  carries the pre-M4 Area 2 counts. M4 flipped 19 Area 2 rows → FULL, 3 →
  DIVERGENCE (402, mid-stream continuation, admission queue), and 3 Area 12
  duplicates with them (rows 14–16), moving Area 2 to FULL 33 / PARTIAL 7 /
  MISSING 0 / AHEAD 3 / DIVERGENCE 3 (comparable 40; strict 82.5%, weighted
  91.3%). The headline is NOT rewritten here: a mechanical recount of ALL
  areas diverged from the claimed headline (pre-existing, out of scope) and
  only a full recount can restate it honestly.

- M5 note (2026-11-18): M5 (budgets, quotas, pools) flipped 19 Area 5
  rows → FULL (M5.1–M5.13) and 1 → DIVERGENCE (admission queue, decided
  M4.10), moving Area 5 to FULL 27 / PARTIAL 0 / MISSING 0 / AHEAD 2 /
  DIVERGENCE 1 (comparable 27; strict 100.0%, weighted 100.0%). The
  section header it replaced was itself stale: a mechanical recount at
  M5 start gave FULL 8 / PARTIAL 6 / MISSING 14 (not the header's
  FULL 7 / MISSING 15 — it had missed the per-response-headers FULL
  row). Duplicates flipped with Area 5: Area 4 row 363 (per-key
  schedule/throttle → FULL) and Area 11 rows 717, 720, 722, 723 (→
  FULL); Area 11 row 724 (me/status) stays PARTIAL — per-window total
  request counts are still missing. Those two section headers and
  the per-area Total row are stale by these flips (mechanical
  recounts now: Area 4, 31 rows = FULL 14 / PARTIAL 3 / MISSING 6 /
  AHEAD 8; Area 11, 26 rows = FULL 13 / PARTIAL 8 / MISSING 5 — both
  headers were already stale before M5: Area 4 claimed 32 rows,
  Area 11 claimed 28). Area 8b
  dashboard rows for the same features (e.g. daily/weekly/monthly
  limits, quota-share pools, team cost centers) are NOT flipped here:
  the dashboard pages themselves are unverified and belong to M10.
  M5.14 (exit gate) fixed two real races found by audit: the
  cumulative cap had no reservation (ReserveSpend no-op without a
  key-budget row; the pre-send check read settled spend only), and
  the strict request-budget hedge gate checked the second attempt
  against spentUSD alone, letting a hedged pair jointly exceed the
  budget. Both are fixed and race-proved (ledger + server parallel
  tests, hedge-budget engine tests incl. a toggle-verified failure
  without the fix). The headline is NOT rewritten here, for the same
  reason as M4.

- M6 note (2026-11-18): M6 (caching 2.0 + token-cost engine) flipped 9 Area 6
  rows → FULL (per-entry size cap, total memory cap, truncated-response guard,
  bypass headers, per-request cache controls, semantic threshold, semantic
  scope, prompt-cache affinity, dedup/coalescing), 3 → PARTIAL (persistence
  tiers, reasoning cache, search cache), 1 → DIVERGENCE (per-provider
  isolation, decided M6.12); P0-2 and P0-3 both CLOSED (coalescing key
  tenant-scoped in the live path, M6.10; semantic layer tenant- + temperature-
  scoped with legacy fail-closed, M6.11-fix; serve-mode persistence wired,
  M6.2b). Area 6 is now FULL 24 / PARTIAL 7 / MISSING 0 / AHEAD 3 /
  DIVERGENCE 1 (comparable 31; strict 77.4%, weighted 88.7%). Area 12 rows
  6 (coalescing) and 17 (cache as latency saver) also flipped → FULL: the
  gaps their Ours cells listed are all closed by M1.4/M6. This commit ALSO
  rewrites the headline and every per-area header/total from a mechanical
  recount (script: split on unescaped pipes, verdict from the Verdict
  column, comparable = rows − AHEAD − DIVERGENCE; the convention Areas 2/5
  already used — Areas 3/10/13 previously used the all-rows denominator,
  which understated their strict/weighted figures; no feature changed
  status by the convention fix alone). Recount findings: Area 4 is 32 rows
  (FULL 14, not the header's 13/6/4); Area 7 is 36 rows (FULL 17, not
  37/FULL 18); Area 9 is 42 rows (MISSING 27, not 28); Area 11 is 28 rows
  (FULL 15, not the header's 9/11/8); Area 14 is 88 rows (FULL 10, MISSING
  47, AHEAD 1, DIVERGENCE 1, plus 1 unclassified claims row). NOT re-verified
  and left as claimed: Area 8b (condensed section — full detail lives in
  the comparison report; its header claims 139 rows but its own sub-tallies
  sum to 138 verdicts, and its FULL/MISSING/WRONG sub-tallies disagree with
  its header by one each) and the Area 12 measured-numbers table
  (measurements, not parity rows). Headline now: 688 rows, FULL 226 /
  PARTIAL 169 / MISSING 246 / WRONG 7 / AHEAD 28 / DIVERGENCE 10 (+1
  unclassified claims row); comparable 649; strict 34.8%, weighted 47.8%.
  The de-duplication estimate below was calibrated to the old 691 count and
  is NOT restated — a row-by-row de-duplication was not done.

- M7 note (2026-11-18): M7 gate wave (eval harness + fidelity/inflation
  guards) adds 1 Area 10 row → FULL (inflation guard, wired into Apply
  after the fidelity gate; 4 real inflation cases refused end-to-end,
  toggle-proven) and refreshes 3 rows' evidence, all staying PARTIAL
  (eval harness — harness + measured savings table committed; fidelity
  gate — CheckLossy WIRED into Apply, F1 paren-URL hole closed with
  balanced-delimiter extraction, review PASS-WITH-NITS; protection
  guarantees — URL/JSON cuts now refused). Area 10 is now FULL 9 /
  PARTIAL 7 / MISSING 27 / AHEAD 4 (comparable 43; strict 20.9%,
  weighted 29.1%). Headline now: 689 rows, FULL 227 / PARTIAL 169 /
  MISSING 246 / WRONG 7 / AHEAD 28 / DIVERGENCE 10 (+1 unclassified
  claims row); comparable 650; strict 34.9%, weighted 47.9%. M7.4–M7.9
  (RTK, Caveman-lite, presets, hard-budget post-pass, stacked pipeline,
  token-counting gaps) are NOT in this commit — they follow as the
  second M7 wave. The M7 exit gate (round-trip tests, inflation guard
  test, measured savings table) is MET by this wave.

- M7 merge-wave note (2026-11-19): M7.4–M7.9 wired into Apply +
  router (RTK, Caveman, named modes, hard-budget post-pass, runtime
  calibration) and the stale DefaultAvailability (RTK/Caveman false)
  fixed at merge. Flips, all in Area 10: 7 rows MISSING → PARTIAL
  (RTK, Caveman, stacked pipeline, named modes, budget dial, worker
  pool, runtime calibration) and 1 row PARTIAL → FULL (base64 data-URI
  exclusion); hard-budget stays PARTIAL with refreshed evidence
  (the 4 real inflation cases now FIT via the post-pass); cache-aware
  stays MISSING with refreshed evidence (built, unwired — no caller in
  Apply); count memoization stays AHEAD with refreshed evidence
  (duplicate hash+lock per cold count removed). Deliberate
  non-wirings, recorded in code + LOG.md: the dial/auto-trigger is NOT
  the default (mode selection is explicit-only — a bare lossy opt-in
  must not gain prose rewriting); the compression pool is NOT wired
  (no genuine background consumer exists); cache-aware is NOT wired
  into Apply. Area 10 is now FULL 10 / PARTIAL 13 / MISSING 20 /
  AHEAD 4 (comparable 43; strict 23.3%, weighted 38.4%). Headline now:
  689 rows, FULL 228 / PARTIAL 175 / MISSING 239 / WRONG 7 / AHEAD 28
  / DIVERGENCE 10 (+1 unclassified claims row); comparable 650;
  strict 35.1%, weighted 48.5%. No new rows: the headline move is
  arithmetic on the M7 headline (the Area 10 recount above is
  mechanical and exact; a naive full-file recount diverges on the
  non-standard verdict spellings in Areas 3/5/7/9/12/14, e.g. "FULL —
  AHEAD", "MISSING (deliberate)", Area 12's metric tables).

- M8.0 note (2026-11-19): Area 14 reconciliation only — no feature
  built, no rows added or removed. Flips, all in Area 14: 1 row
  PARTIAL → FULL (14b usage/budget), 1 row MISSING → FULL (14b scopes
  enforced), 3 rows MISSING → PARTIAL (14a me/sessions list, 14a
  auto-combo candidates,
  14b Sessions — the last is the 14a me/sessions row's duplicate in
  §14b, flipped with it so the file does not contradict itself).
  14b per-response headers was already FULL (flipped M2.1, recorded in
  the changelog; no change). 14b Cache stays PARTIAL with corrected
  evidence (GET /api/cache/stats live, M6.8; no /dashboard/cache page,
  M10 — same verdict as the Area 6 cache-analytics row). 14a me/status
  stays PARTIAL with corrected evidence (daily/weekly windows live,
  M5.6; per-window total request counts still missing — Area 11 row
  724, M8.11's job). Area 14 is now FULL 12 / PARTIAL 29 / MISSING 44 /
  WRONG 1 / AHEAD 1 / DIVERGENCE 0 / UNVERIFIED 1 (88 rows, comparable
  87; strict 13.8%, weighted 30.5%). Headline now: 689 rows, FULL 230 /
  PARTIAL 177 / MISSING 236 / WRONG 7 / AHEAD 28 / DIVERGENCE 10 (+1
  unclassified claims row); comparable 650; strict 35.4%, weighted
  49.0% — arithmetic on the M7 headline for the flipped rows only.
  MISSING is 236, not 235 (review correction): the pre-flip 239
  inherited Area 14's undercount (its row said MISSING 47, true 48),
  so the true -4 delta lands at 236 — and with 236 the headline parts
  sum exactly to the 689-row total (230+177+236+7+28+10+1). The
  headline's other staleness (M4–M6 notes) is unchanged.

- M8 note (2026-11-19): M8 API-breadth PARITY flips, all per the
  LOG.md reviewer verdicts (not builder claims); the routes were wired
  by the M8 merge wave (106 → 170 routes, full server suite green).
  Flips: MISSING → FULL 2 (§14a OCR, §14a token-count); PARTIAL → FULL
  2 (Area 11 me/status, §14b guardrails-test); MISSING → PARTIAL 14
  (§14a completions, models/query, moderations, search, images, video,
  music, files, per-provider, gemini-native, quotas; §14b webhooks,
  teams, quota-share pools); the M8.2 lumped row
  (classify/explain/segment) split into three rows — classify + segment
  stay MISSING as OmniRoute defines them (Jina proxies; local variants
  exist at the same path), explain → PARTIAL. Evidence-only updates, no
  verdict change: Area 1 row 371 (images/search/video/music/OCR/audio
  engine surface), Area 6 Flush + Search cache, Area 11 Analytics
  endpoints, §14a responses + me/status, §14b Combos/Analytics/Cache/
  Resilience/Pricing/Logs/Sessions/Files-batches. NOT flipped: §14a
  audio (M8.6 merge gate — provider audio client still missing),
  §14a batches (M8.8 not built), §14b fallback chains API + Area 8b
  combos CRUD + Area 7 row 609 (reviewers: stay MISSING). §14a
  me/status evidence corrected (its cited gap is closed, Area 11 row
  724 → FULL) but the verdict is held at PARTIAL — no reviewer blessed
  FULL for that row. Headline now: 691 rows, FULL 234 / PARTIAL 190 /
  MISSING 221 / WRONG 7 / AHEAD 28 / DIVERGENCE 10 (+1 unclassified
  claims row); comparable 652; strict 35.9%, weighted 50.5%. Area 14 is
  now FULL 15 / PARTIAL 41 / MISSING 28 / WRONG 1 / AHEAD 1 /
  DIVERGENCE 0 / UNVERIFIED 1 (90 rows, comparable 89; strict 16.9%,
  weighted 39.9%). §14a is now 36 rows (FULL 7 / PARTIAL 16 / MISSING
  13); §14b is now FULL 6 / PARTIAL 21 / MISSING 11 / AHEAD 1 (39 rows).
  Area 11 is now FULL 16 / PARTIAL 7 / MISSING 5 (28 rows; strict
  57.1%, weighted 69.6%). Recount is mechanical (script: split on
  unescaped pipes, verdict from the Verdict column, comparable = rows −
  AHEAD − DIVERGENCE − unclassified; Area 8b left as claimed per the M6
  note, Area 12's measured-numbers table is measurements, not rows).
  One LOG.md contradiction recorded, verdict unaffected: the M8.4 review
  (LOG.md:661-662) says input_tokens is "still missing" on the §14a
  responses row while the M8.3 review (LOG.md:845) says the input_tokens
  gap is closed — both agree the row stays PARTIAL, and the row now
  records both endpoints as live with the [...path] catch-all still
  missing.

  M8.22 exit gate (2026-11-19): §14a Batches row 1072 MISSING → PARTIAL.
  Surface complete (6 routes live, real worker, 9 fixtures, 13 tests,
  review PASS) but two material gaps argue against FULL: ledger records
  batch lines at the sync rate (no 50% batch rate — needs a flag through
  provider.ChatRequest → router.Attempt → attemptCost) and input-file
  ownership is unenforceable (files.File has no owner). Area 11
  batch/flex pricing row stays MISSING (no batch-rate price tier).
  Headline now: 691 rows, FULL 234 / PARTIAL 191 / MISSING 220 / WRONG 7
  / AHEAD 28 / DIVERGENCE 10 (+1 unclassified claims row); comparable
  652; strict 35.9%, weighted 50.5%. §14a is now 36 rows (FULL 7 /
  PARTIAL 17 / MISSING 12).

De-duplication: the naive 689 double-counts features two areas both counted (≈80–90 rows,
mostly Area 8b re-counting backend features as dashboard UI options). The old estimate
(removing ~87 identifiable duplicates → ≈600 distinct features, strict ≈28%, weighted
≈44–45%) was calibrated to the old 691 count and is NOT restated here — a row-by-row
de-duplication was not done. Quote the per-area figures as the authoritative numbers, and
the overall only with this caveat.

The gap is breadth, plus a short list of live correctness/security bugs. The core request
path (chat completions, embeddings, rerank, messages, responses, failover, catalog head)
is at parity or ahead.

## Parity per area (weighted; baseline — master prompt §2.2)

| # | Area | Total | FULL | PARTIAL | MISSING | WRONG | Strict | Weighted |
|---|---|---|---|---|---|---|---|---|
| 1 | Strategies / combos / auto scoring | 63 | 34 | 25 | 4 | 0 | 54.0% | 73.8% |
| 2 | Failover, breakers, retries, hedging | 40 | 36 | 4 | 0 | 0 | 90.0% | 95.0% |
| 3 | Providers / models catalog | 19 | 10 | 6 | 2 | 1 | 52.6% | 68.4% |
| 4 | Auth, keys, scopes, route guards | 23 | 14 | 3 | 6 | 0 | 60.9% | 67.4% |
| 5 | Budgets, rate limits, quota | 27 | 27 | 0 | 0 | 0 | 100.0% | 100.0% |
| 6 | Caching | 31 | 24 | 7 | 0 | 0 | 77.4% | 88.7% |
| 7 | Guardrails, PII, content policy | 36 | 17 | 7 | 12 | 0 | 47.2% | 56.9% |
| 8a | Dashboard: Home/Providers/Models | 38 | 5 | 14 | 19 | 0 | 13.2% | 31.6% |
| 8b | Dashboard: Routing/Keys/Budgets/Analytics/Logs/Settings | 139 | 7 | 36 | 91 | 4 | 5.0% | 18.0% |
| 9 | MCP server / A2A | 38 | 4 | 7 | 27 | 0 | 10.5% | 19.7% |
| 10 | Compression, token counting | 43 | 10 | 14 | 19 | 0 | 23.3% | 39.5% |
| 11 | Analytics, usage, ledger, cost | 28 | 15 | 8 | 5 | 0 | 53.6% | 67.9% |
| 12 | Latency, startup, memory, binary | 16 | 15 | 0 | 0 | 1 | 93.8% | 93.8% |
| 13 | Packaging, Docker, install, CLI | 24 | 6 | 12 | 6 | 0 | 25.0% | 50.0% |
| 14 | Plugins, API surface, docs | 87 | 12 | 29 | 44 | 1 | 13.8% | 30.5% |
| **Total** | | **650** | **236** | **172** | **235** | **7** | **36.3%** | **49.5%** |

- Every area's Total above is now the COMPARABLE count (rows − AHEAD −
  DIVERGENCE), per this file's rule that AHEAD/DIVERGENCE are not in the
  denominator — unified by the M6 mechanical recount (previously Areas 2/5
  used comparable while Areas 3/10/13 used the all-rows denominator). The
  Total row's Total column (650) is the comparable headline total. The
  per-area Total column sums to 652, two more than 650, because Area 8b's
  row carries its claimed 139 while its own sub-tallies account for 138
  verdicts (see the M6 note) and because Area 14's comparable is 87, not
  86: the old 86 subtracted a DIVERGENCE 1 that no Area 14 table row
  carries (M8.0 reconciliation — see the Area 14 header note). The
  per-area MISSING column sums to 236, and the Total row + headline use
  236: the pre-flip 239 inherited Area 14's undercount (its row said
  MISSING 47, true 48), so the true -4 delta lands at 236 — and with
  236 the headline parts sum exactly to the 689-row total
  (230+177+236+7+28+10+1); the Total
  row uses 650, which reconciles with the headline (689 rows − 28 AHEAD
  − 10 DIVERGENCE − 1 unclassified claims row).

Best areas: caching (89% weighted), latency techniques (84%), strategies (74%),
catalog (68%), analytics/ledger (68%). Worst: dashboard 8b (18%), MCP/A2A (20%),
plugins/API (27%). Compression left the worst list at 38% after the M7 merge wave.

## P0 live bugs — fix first (master prompt §2.3)

| # | Bug | Evidence | Status |
|---|---|---|---|
| P0-1 | Scopes display-only. HasScope has zero non-test call sites; any gateway key can call any route incl. destructive Tier-2 routes. 54 routes AuthExempt, incl. 8 POSTs that mutate live state with no credential. No single-key disable/revoke. No dashboard auth. (Server side closed; dashboard JS Authorization for the 8 POSTs is an M10 follow-up.) | `internal/config/gatewaykey.go:141` | CLOSED 2026-11-18 (Wave 1) |
| P0-2 | Cache has no tenant isolation. ExactKey = SHA-256 of {model, messages} only; coalescing key excludes gateway keys → one tenant's cached answer can be served to another. | `internal/cache/key.go:141` (ScopedKey: tenant + temp bucket in key), `internal/coalescing/coalescing.go:407` (RequestKeyForTenant), `internal/router/router.go:5985` (live call), `internal/degrade/degrade.go:1064` + `internal/server/relay.go:636` (scoped key on both paths) | CLOSED 2026-11-18 (M6: coalescing key tenant-scoped in the live path, M6.10; semantic layer tenant-scoped with legacy fail-closed, M6.11-fix; 17/17 property tests pass) |
| P0-3 | Cache ignores temperature. cacheable() never reads temperature; also caches finish_reason=length truncated answers; in-memory only, no flush endpoint; own-cache hit records nothing in savings. | `internal/cache/key.go:207` (ShouldStore: refuses length + temp>0 w/o allowance), `internal/degrade/degrade.go:1166,1563` (lookup/store temp gates), `internal/degrade/degrade.go:367` (recordCacheHit), `cmd/codvian-router/main.go:3027,3042` (serve-mode persistence wiring) | CLOSED 2026-11-18 (M6: truncated guard on all store paths, M6.3-fix; serve-mode persistence wired, M6.2b; temp gating + flush + savings from M1.4/M1.5) |
| P0-4 | Ledger prices cached input wrongly. buildLedgerPrices drops CachedInputUSDPer1M, so ledger charges cached tokens at full input price while the dashboard uses the cached rate. | `internal/ledger/ledger.go:60-66`, `cmd/codvian-router/main.go:1089+` | CLOSED 2026-11-18 (Wave 1) |
| P0-5 | PEM private-key redaction leaks the body. Rule matches only the BEGIN header; body lines and END footer pass through; streaming responses are never sanitised. | `internal/guardrails/guardrails.go` (private-key rule) | CLOSED 2026-11-18 (Wave 1) |
| P0-6 | MCP client bugs: timeout nil-pointer panic (stdio.go:164/185/191 after killLocked() nils s.cmd); 64-char collision infinite loop (mcptools.go:279-291). | `internal/mcptools/stdio.go`, `internal/mcptools/mcptools.go:279-291` | CLOSED 2026-11-18 (Wave 1) |
| P0-7 | Token counting bugs: CountPrompt double-counts array-form messages ~2x; legacy-model encoding errors; Count unbounded, O(n²) BPE, no cap. | `internal/tokencount/tokencount.go`, `internal/provider/client.go:109-116` | CLOSED 2026-11-18 (Wave 1) |
| P0-8 | No per-response cost/token/latency headers (0 matches). Their single choke-point emits cost, tokens in/out, latency, request-id, version, fallback attempts, tokens/s on every response. | grep: zero hits in `internal/` | CLOSED 2026-11-18 (Wave 1, M2.1/M2.2 — choke-point writer + correlation id; M2 gate green 2,039 PASS / 0 FAIL) |
| P0-9 | code strategy is declared but rejected by config (latent bug). | `internal/strategy/strategy.go:119-126` vs `internal/config/config.go:1358-1368` | CLOSED 2026-11-18 (Wave 1) |
| P0-10 | Default strategy is balanced (re-ranks every request); OmniRoute's default is priority (honours operator order). Ours is backwards for operators. | `internal/config/config.go:507-509` | CLOSED 2026-11-18 (Wave 1) |
| P0-11 | Test headline "1,919 pass" is UNVERIFIED — nobody re-ran it with a Go toolchain. Run it first and record the real number. | M0.1 | CLOSED 2026-11-18 (Wave 1) |

## Where we are already AHEAD (protect and extend; keep numbers honest)

- Latency/throughput: latest clean runs 0.85/0.78 ms added latency (target <0.5 ms MISSED),
  RPS 1,273/1,174 (target >3,000 MISSED); idle ≈ 42 MiB (MET); ready ≈ 435/565 ms (MET).
  OmniRoute publishes no latency/RPS/memory benchmark numbers. Failover measured: 429
  failover 52.4–55.8 ms (≈1–2 ms router overhead vs 51.2 ms direct); hedge 203.9–204.4 ms
  vs a 2,000 ms slow provider.
- Security defaults: auth ON by default; no default password; log/ledger redaction always
  on; typed-phrase danger zone with server enforcement; live route-guard tier tables.
- Catalog: 610 providers vs their 394 (all 394 IDs present; 29 we have that they lack);
  65 docs-verified with 3 verification statuses; audit tooling; whole-provider DELETE.
- Guardrails: Aadhaar rule, generic-token catch-all, labelled-password rule, blocking with
  -32001, per-key allowlist scoping, cannot be disabled by a client header (theirs can),
  38-phrase content-policy refusal chain, async external classifiers.
- Caching: stale/expired serving with honest X-Codvian-Stale, routing-contract exclusion,
  lossy-compression exclusion from semantic layer.
- Failover behaviour: Retry-After ≥ 60 s → abandon + immediate failover; 405/501 retryable
  on select endpoints; 529 retryable; per-model 404/400 split.
- Ledger: estimate-vs-reported drift column; X-Codvian-Budget-Spent; async ledger write path;
  usage import with dedup; token-count drift table.
- MCP: guardrail tool_args phase; MCP client with selector mode (OmniRoute has none).
- Ops: benchmark page with honest missed-target verdicts; release verification; per-request
  budgets; headless mode + first-run wizard; 50 live dashboard feeds with honest empty states.

- M3 S4 note (2026-11-19): M3 residual S4 verified, no build needed — preferred/reserve
  accounts for auto combos (row 425) was already built: key roles preferred/reserve,
  preferred first / reserve sorted last on the flat path (applyTailPartitions) and the
  auto-combo path (orderPreferredReserve in chainForDispatch), plus the opt-in
  response-start failover (20 s, already FULL in Area 2). Evidence:
  docs/design/m3-s4-evidence-2026-11-19.md — 16/16 router tests green
  (TestKeyPool*, TestPreferredFailover*, TestChainForDispatchPreferredReserve,
  TestOrderAttemptsAppliesAccountFallbackThenReserveLast) + 9/9 live smoke
  (preferred 429 → reserve serves; reserve-first still sorts last; auto-combo
  serves preferred first). Dashboard already shows key roles (role pill, reserve
  chip, role toggle). Row 425 flips MISSING → FULL; its OmniRoute citation is
  tightened (deep-study §1.1 + preferredConnectionFailover.ts + auth.ts:1930-1945)
  — no "reserve account" citation invented, none exists. Area 1 is now FULL 35 /
  PARTIAL 25 / MISSING 3 (comparable 63; strict 55.6%, weighted 75.4%). Headline
  now: 691 rows, FULL 235 / PARTIAL 191 / MISSING 219 / WRONG 7 / AHEAD 28 /
  DIVERGENCE 10 (+1 unclassified claims row, +1 Area 8b unaccounted); comparable
  652; strict 36.0%, weighted 50.7%. Per-request preferred-connection list stays
  out (design §S4(5), product decision pending — no such input exists anywhere:
  grep for preferredConnectionIds returns nothing).

- M3 S1 note (2026-11-19): M3 residual S1 built — account-level fallback config surface
  (row 388): 9 values (priority, fill-first, round-robin, p2c, random, least-used,
  cost-optimized, strict-random, weighted) via ParseAccountFallbackStrategy; global +
  per-provider config fields (routing.account_fallback_strategy /
  account_fallback_strategies) with load-time validation; expiry-first rejected at
  both levels (no per-key quota-expiry source, D2 pending). Evidence: config-file
  end-to-end proof (real config file → engine rotation observed,
  TestAccountFallbackConfigFileEndToEnd) + TestConfigAccountFallbackRoundTrip (all 9
  values survive Load) + toggle-proof (removing the orderAccountAttempts call fails
  the new round-robin tests). Row 388 flips MISSING → FULL. Area 1 is now FULL 36 /
  PARTIAL 25 / MISSING 2 (comparable 63; strict 57.1%, weighted 77.0%). Headline
  now: 691 rows, FULL 236 / PARTIAL 191 / MISSING 218 / WRONG 7 / AHEAD 28 /
  DIVERGENCE 10 (+1 unclassified claims row, +1 Area 8b unaccounted); comparable
  652; strict 36.2%, weighted 50.8%. Known limits (design §S1(3)): p2c is a random
  draw with a breaker guard (no per-key score source exists, none invented);
  weighted draws uniformly (no per-account weight source — operator-config decision);
  fill-first ≈ priority order (the drain is enforced by retrying the first target
  until it fails); filterZeroQuota filters exhausted PROVIDERS on the provider-level
  quotaHint, not individual accounts — no per-key quota source exists.

- M3 S2 note (2026-11-19): M3 residual S2 built — sla-aware config surface
  (row 431): file config wired into the existing router SLA fields
  (SLATargetP95Ms/SLAMaxErrorRate/SLAMaxCostPer1M/SLAHardConstraints, read by
  Engine.slaPolicy()); BreakerOpen pre-filter built (OPEN candidates last, never
  dropped; all-OPEN scores the full pool); zero-tolerance via SLAMaxErrorRateSet;
  Describe("sla-aware") discloses p50 EWMA stands in for p95 + stability neutral
  0.5. Evidence: 3/3 config + 7/7 strategy + 2/2 router tests green, both
  negative controls executed via -overlay (OPEN-filter bypass; zero-tolerance
  guard removed — both fail as expected). Row 431 flips MISSING → FULL. Known
  limits: true p95/stddev deferred + disclosed. Follow-up (not gate-blocking):
  direct unit test for the main.go wiring (helper extraction).

- M3 S3 note (2026-11-19): M3 residual S3 built — nadir config surface, reduced
  scope per design §S3 (row 433): NadirConfig {endpoint, timeout_ms} with
  load-time validation (endpoint must be an absolute http(s) URL, timeout_ms
  [0, 30000]); wired in main.go into the existing engine fields only; API key
  from CODVIAN_ROUTER_NADIR_API_KEY env only. The 30s failure cooldown was NOT
  rebuilt — verified already in code (NadirFailureCooldownMs=30000,
  per-endpoint key, check-before-dial + arm-on-failure,
  TestNadirFailureCooldownSkipsCallsThenResumes green). Deliberate divergence:
  no default endpoint, no env fallback for the endpoint (fail-closed) vs
  OmniRoute's fail-open hardcoded https://api.getnadir.com. Evidence: 2/2 config
  + 11/11 router tests green, negative control executed via -overlay (JSON tag
  renamed → round-trip FAILS as expected). Row 433 flips MISSING → FULL.

- M3 EXIT GATE MET (2026-11-19): Area 1 is now FULL 38 / PARTIAL 25 /
  MISSING 0 (comparable 63; strict 60.3%, weighted 80.2% ≥ 80%). Headline now:
  691 rows, FULL 238 / PARTIAL 191 / MISSING 216 / WRONG 7 / AHEAD 28 /
  DIVERGENCE 10 (+1 unclassified claims row, +1 Area 8b unaccounted); comparable
  652; strict 36.5%, weighted 51.2%.

- Block 2 Batch 1 flip (2026-11-20): PARITY.md:553 fallback-chains CRUD API
  PARTIAL → FULL (V-16 PASS, commit a5866cb; TestG11FallbackChainsAPIThroughMux
  cold + toggle-red with routes unregistered). Headline now: FULL 244 / PARTIAL
  189 / MISSING 217 / WRONG 2; comparable 652; strict 37.4%, weighted 51.9%.
  Caveat recorded on the row: chainFor does not yet consult stored chains
  (separate follow-up).

- Block 2 Batch 1 flip (2026-11-20): PARITY.md:524 breaker states/persistence/
  lazy recovery PARTIAL → FULL (V-19 PASS, commit 01bbf10; 4 new toggle tests
  cold). Headline now: FULL 245 / PARTIAL 188 / MISSING 217 / WRONG 2;
  comparable 652; strict 37.6%, weighted 52.0%.

- Block 2 Batch 1 flip (2026-11-20): PARITY.md:1038 latency-aware routing/
  scoring PARTIAL → FULL (V-15 PASS, commit 6ccea18; toggle test cold).
  Headline now: FULL 246 / PARTIAL 187 / MISSING 217 / WRONG 2; comparable
  652; strict 37.7%, weighted 52.1%. CORRECTION (2026-11-20): the row flipped
  here lives in Area 12, not Area 1 — the Area 1 header was wrongly updated in
  this commit (now corrected back to FULL 37 / PARTIAL 26, weighted 79.4%).
  Area 12 is now FULL 13 / PARTIAL 2, weighted 87.5%. The M3 gate (Area 1
  ≥80% weighted) is NOT met: Area 1 weighted is 79.4%. Headline totals were
  unaffected (the two header errors cancelled in the sum).

- Block 2 Batch 1 (2026-11-20): PARITY.md:939 cache-aware compression
  MISSING → PARTIAL (V-17: builder's Apply-side seam verified + toggle-tested,
  commit eee5cb5; NOT FULL — no production caller of Observe/FreezeChecker,
  QuantumLock absent). PARITY.md:1030 hedging strengthened in place (V-18 PASS,
  commit c1b215e; Tools + ToolChoice now count toward the 256 KiB body budget;
  row already FULL, no count change). Headline now: FULL 246 / PARTIAL 188 /
  MISSING 216 / WRONG 2; comparable 652; strict 37.7%, weighted 52.1%.

- Block 2 Batch 1 flip (2026-11-20): PARITY.md:523 200-OK body validation
  predicate PARTIAL → FULL (V-21 PASS, commit 70dd402; 6 new tests cold,
  toggle-checked). Headline now: FULL 247 / PARTIAL 187 / MISSING 216 /
  WRONG 2; comparable 652; strict 37.9%, weighted 52.2%. Follow-up on the
  row: config-file wiring routing.response_validation → engine still missing.

- Block 2 Batch 1 flip (2026-11-20): PARITY.md:1040 latency percentiles in
  telemetry PARTIAL → FULL (V-20 PASS, commit fd813a3; p99 live on
  model-latency-stats, provider history, overview; /api/telemetry/summary
  itself absent, equivalents cover it). Headline now: FULL 248 / PARTIAL 186 /
  MISSING 216 / WRONG 2; comparable 652; strict 38.0%, weighted 52.3%.
  Batch 1 COMPLETE: all 7 builders done, all 7 verifiers passed.

- Block 2 Batch 1 correction (2026-11-20): PARITY.md:1173 Hedging (parallel
  2nd attempt, Area 12) PARTIAL → FULL — V-18's evidence (commit c1b215e;
  4 guard tests cold, toggle-checked) belongs to this row, whose gap was
  'no body-size guard'; the Area 2 hedging row was already FULL. Headline now:
  FULL 249 / PARTIAL 185 / MISSING 216 / WRONG 2; comparable 652;
  strict 38.2%, weighted 52.4%.

## MUST NEVER be copied from OmniRoute

CHANGEME default password; keyless-by-default /v1; keyless A2A; anonymous fallback keys.
MCP scope enforcement OFF by default; manage/admin scope disjoint from tool scopes. Lossy
(Caveman) compression of MCP tool descriptions (defaults ON on their side). Client-controlled
guardrail disable (x-omniroute-disabled-guardrails, disabledGuardrails body field).
plugin_install / plugin_activate loading third-party code into the request pipeline from an
MCP client; destructive MCP tools shipped to every client with no separate grant
(omniroute_memory_clear, omniroute_cache_flush, db_health_check(autoRepair),
sync_pricing, obsidian execute/delete). Announcing a 110-tool catalog to every client by
default; ownerless tasks visible to everyone. A spend tool with no per-call budget / dry-run /
cost estimate; Conductor-style delegation running coding work with a server-side fleet token;
client-controlled _meta scopes on stdio. TLS stealth/fingerprint spoofing, MITM root CA,
cookie-session providers, inflated "free-token" headlines.

## Corrections — do NOT repeat these wrong claims

README.v2.md is 260 lines (not 593); README.md 179. OmniRoute README ≈ 1,844 lines / 76
headings; ours 11 headings. Startup "12 ms" is WRONG (ready is 125–643 ms). Binary size:
quote a range 17.3–28 MB / ≈20–22 MB measured, never a single invented point. "OmniRoute
idle Docker ~541 MB" UNVERIFIED — do not quote. Catalog: 610 providers, 379 with base URLs,
231 (38%) have no base URL → cannot be added. Key-creation hint links: theirs 129, ours 0.
CHECKLIST item 87 ("423 entries") is stale. OmniRoute does set X-OmniRoute-Cache: HIT etc.
(cache headers are parity, not our advantage). OmniRoute does have response-start failover
(opt-in, 20 s deadline, rotate to one sibling). Breaker thresholds (OmniRoute docs): OAuth
8× / API-key 12× / local 2×, reset 60 s / 30 s / 15 s. Ours: OAuth degrade 5 / open 8 /
reset 60 s; API-key 7/12/30 s; local 2/15 s. Compression is OFF by default in OmniRoute
(RTK→Caveman is the configured stack); savings percentages are marketing ranges (15–95%),
not measured benchmarks. Don't copy those numbers; measure ours. SSE batching (3 tokens /
8 ms) lives in chat stream AND Responses stream; /v1/messages streaming does not batch.
Auto-router classify ≈ 71 µs/op (not 31 µs). Guard table = 89 rows (Tier1 9, Tier2 14,
Tier3 66). OmniRoute MCP tool count: README claims 110; static grep undercounts (84).
Quote "≥ 110 (runtime)".

---

## Per-area rows

Test column: a test name is listed only where the comparison report names one; otherwise
UNVERIFIED (no test name recorded in the report — do not invent one). File:line values are
as-read in the comparison (builders were actively editing; line numbers may have shifted).

### Area 1 — Strategies / combos / auto scoring (63 total: FULL 61, PARTIAL 0, MISSING 0, WRONG 0, DIVERGENCE 2; comparable 61; strict 100.0%, weighted 100.0%)

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| priority (default) | routingStrategies.ts:2; comboConfig.ts:113 | Declared-order strategy; default is priority since M1.11 (duplicate of the "Default strategy = priority" row below) | FULL | internal/strategy/strategy.go; internal/router/tree.go | TestPriorityHookPreservesDeclaredOrder, TestW1EffectiveStrategyDefaultsToPriority |
| weighted | targetSorters.ts:28 selectWeightedTarget | Per-target weight draw implemented + wired (M3); chain per-target weights now live via routing.chain_weights (Phase C) — SetChainWeights at startup, Resolve in the weighted hook; an unconfigured chain draws uniformly. Old operator weight-set behaviour renamed to "custom-weights" | FULL | internal/strategy/wiring.go; internal/strategy/chainweights.go; internal/strategy/orderings.go:178 | TestConformanceWeightedDrawIsAPermutation, TestConformanceWeightedChainWeights |
| round-robin | roundRobinCombo.ts + rrState.ts | Stateful per-chain rotation (Phase C: DefaultChainStates, one rotation per chain), cycles evenly, survives target-list changes (M3) | FULL | internal/strategy/orderings.go:240; internal/strategy/wiring.go | TestOrderingRoundRobinCyclesEvenly, TestConformanceRoundRobinCycles |
| context-relay | strategyRegistry.ts:143; executeTargetAttempt.ts:755 | Relay state + next-attempt context builder implemented and registered (M3); engine integration LIVE for the in-request on-error relay (Phase D, D5: outcomes → handoff → next attempt, lazy summariser-seam handoff, honest raw-transcript degradation, LastRelayStatus). Producers built (d3abe14) and LIVE in the request path: production summariser (relaySummariser main.go:3214, cheapest priced model, live client, 30 s/512 tok, error → raw-transcript degradation), production quota-use signal (RelayQuotaUsedFor main.go:3288, learned hint → used fraction, unknown stays unknown), cross-request session store (relaySessions relay.go:680-751, keyed by gateway key, preload in newRelayFor :762). All three producers now PINNED by committed tests (e2828ca, 5 tests): store off → engine + production tests FAIL; summariser errors → production test FAILs on summary assertions; signal forced unknown → production test FAILs (summariser calls = 0). V-B2-489-RE pass 2026-11-20 ~13:54 IST — cold PC 5/5 PASS (internal/router 2/2, cmd/codvian-router 3/3). Still open (secondary): chat-format injection only, fixed thresholds/TTL, budget gate undercounts handoff tokens. | FULL | internal/router/relay.go; internal/strategy/affinity.go:354 | TestB2RelayPinCrossRequestWantStoredAndPreloaded, TestB2RelayPinWantKeyedByCallerID, TestB2RelayPinProductionSummariserAndQuotaSignal, TestB2RelayPinQuotaSignalWindow, TestB2RelayPinWantKeyedByGatewayKey; V: pass 2026-11-20 ~13:54 IST |
| fill-first | applyStrategyOrdering.ts | Quota-drain ordering wired (M3): targets with quota lead in declared order, exhausted targets demoted | FULL | internal/strategy/orderings.go:128; internal/strategy/wiring.go | TestOrderingFillFirstDemotesExhausted, TestConformanceFillFirstDemotesNoQuota |
| p2c | targetSorters.ts:136 | Power-of-two-choices wired (M3): two random targets, better 5-factor score first | FULL | internal/strategy/orderings.go:142; internal/strategy/wiring.go | TestOrderingP2CBetterScoreFirst |
| random | applyStrategyOrdering.ts (Fisher-Yates) | Fisher-Yates shuffle wired (M3) | FULL | internal/strategy/orderings.go:100; internal/strategy/wiring.go | TestOrderingRandomChangesOrder |
| least-used | targetSorters.ts:107 | Ordering implemented + wired (M3); attempt counts now live from the engine's provider stats (Phase C) — fewest attempts first, unmeasured targets sort as never used | FULL | internal/strategy/orderings.go:114; internal/strategy/wiring.go | TestOrderingLeastUsedAscending, TestConformanceLeastUsedFewestFirst |
| cost-optimized | targetSorters.ts:91 (pure cheapest-first, unknown price = ∞ last) | Pure cheapest-first strategy "cost-optimized" wired (M3); unknown price sorts last, never cheapest | FULL | internal/strategy/wiring.go | TestConformanceCostOptimizedCheapestFirst |
| reset-aware | quotaStrategies.ts:552; quotaScoring.ts:362 | Ordering implemented + registered (M3); reset times live from the ledger PairQuota roll-off (Phase C). The measured fraction now reaches the registry: Engine.LearnedQuotaHint() (ratelimit.go) exposes the engine's recorded quota-hint entry (measured fraction 0..100 + expiry), and signalsFn overlays it on both return paths (applyLearnedQuota, main.go:2630-2636, :2727/:2757) so QuotaRemainingPct carries the measured fraction from real upstream rate-limit headers. Overlay now PINNED by a committed test (d402834): TestB2QuotaPinResetAwareOrdering drives the real openStack stack + mock upstreams and asserts the order changes with the learned hint. V-B2-QUOTA-RE pass 2026-11-20 ~13:47 IST — cold PC 2/2 PASS; toggle: overlay body emptied → BOTH tests FAIL on order (half=50 served prov-full, want prov-half), restored → PASS. | FULL | internal/strategy/quota.go:336; internal/strategy/wiring.go; cmd/codvian-router/main.go:2630 | TestB2QuotaPinResetAwareOrdering, TestResetAwareScoreValues, TestConformanceResetAwareOrdersByScore; V: pass 2026-11-20 ~13:47 IST |
| reset-window | quotaStrategies.ts:596; quotaScoring.ts:698 | Ordering implemented + registered (M3); reset times now live from the ledger PairQuota roll-off (Phase C) — soonest reset first | FULL | internal/strategy/quota.go:401; internal/strategy/wiring.go | TestResetWindowOrder, TestConformanceResetWindowSoonestFirst |
| expiry-first | quotaStrategies.ts:727; quotaScoring.ts:579 | Ordering implemented + registered (M3). Expiry data now exists: the same overlay sets Signals.ExpiresAt to the learned window-reset instant (earliest x-ratelimit-reset-* from actual provider responses), the deadline ExpiryFirstScore spends against (quota.go:466-470), instead of falling back to the ledger roll-off reset time. Overlay now PINNED by a committed test (d402834): TestB2QuotaPinExpiryFirstLearnedDeadline drives the real openStack stack + mock upstreams. V-B2-QUOTA-RE pass 2026-11-20 ~13:47 IST — cold PC 2/2 PASS; toggle: overlay body emptied → BOTH tests FAIL on order, restored → PASS. | FULL | internal/strategy/quota.go:454; internal/strategy/wiring.go; cmd/codvian-router/main.go:2630 | TestB2QuotaPinExpiryFirstLearnedDeadline, TestExpiryFirstOrder, TestConformanceExpiryFirstMostUrgentFirst; V: pass 2026-11-20 ~13:47 IST |
| headroom | headroomRanking.ts:54 (1 − max(util5h, util7d)) | Ordering implemented + registered (M3). DELIBERATE: no per-pair limit source exists in this local-first product (OmniRoute's util5h/util7d comes from its hosted usage tracking) — all targets score headroom 1, which is OmniRoute's own fail-open. The ordering activates automatically once a per-pair signal exists; building a per-pair usage pipeline is a new data source, not a missing feature. Raunak's rule 2026-11-20 ~13:28 IST: decide on the merits, do not copy what does not exist here. | DIVERGENCE | internal/strategy/quota.go:186; internal/strategy/wiring.go | TestHeadroomOrder |
| quota-weighted | quotaStrategies.ts:1085; draw score/(1+load) | Draw implemented + wired (M3); load now live from the engine's adaptive limiter (Phase C) — the draw keys on score/(1+load) | FULL | internal/strategy/quota.go:269; internal/strategy/wiring.go | TestQuotaWeightedDrawSinglePositiveWeight, TestConformanceQuotaWeightedIsAPermutation |
| strict-random | applyStrategyOrdering.ts (deck, mutex) | Shuffled deck wired (M3); per-chain deck (Phase C): no repeat until exhausted, reshuffle on list change | FULL | internal/strategy/orderings.go:288; internal/strategy/wiring.go | TestOrderingStrictRandomNoRepeatUntilExhausted |
| auto (16-factor pool) | resolveAutoStrategy.ts:186; autoCombo/engine.ts:221 | auto/* resolves a model via embedding tiers; the auto/* variant resolver exists with real pick rules (Phase C) and engine dispatch is live (Phase D, D2 autodispatch). 16-factor pool ranking IS live in code (wiringHook StrategyBalanced→Hook16Default, wiring.go:543; rankModels→RankModelsByBestPair16, main.go:2849–2876; SetAutoRanker main.go:1284) and the ranker closure is now pinned: dispatchAutoVariant → e.autoRanker() → rankedFirst (autovariants.go:918); toggle (SetAutoRanker no-op) FAILS the ranker tests. | FULL | internal/autorouter/autorouter.go:204-214,863; internal/strategy/wiring.go:543; cmd/codvian-router/main.go:2849-2876; internal/router/autodispatch.go:119 | TestB2RankerDrivesBalancedPick, TestB2RankerUninstalledAfterInstall, TestB2BalancedPickFallsBackToCatalogOrderWithoutRanker, TestB2NonPermutationRankerIgnored (4/4 PASS cold on PC; toggle: ranker no-op → 2 FAIL); V-B2-ROUTER-GROUP pass 2026-11-20 ~13:15 IST (PC) |
| lkgp | applyStrategyOrdering.ts orderByLkgp; persisted LKGP | LKGP store + ordering implemented + registered (M3); RecordSuccess now called by the server on every successful inference (Phase C) and the store persists across restarts (lkgp.json, Load at startup / Save at shutdown) — the last-good provider for each model leads the chain | FULL | internal/strategy/affinity.go:85; internal/strategy/lkgpregistry.go; internal/strategy/wiring.go; internal/server/telemetry.go | TestConformanceLKGPOrdersByLastGood |
| context-optimized | comboStructure.ts:1050 (largest context first) | Largest-window-first ordering implemented + registered (M3); ContextWindow is a live hook input (G19 Area 1): signalsFn reads the registry model window (operator-set or models.dev synced, 0 = unknown, never guessed) and the hook reorders largest-first; unknown windows sort last, no-known-window returns declared order (OmniRoute's `?? 0` early return) | FULL | internal/strategy/affinity.go:350; internal/strategy/wiring.go:706; cmd/codvian-router/main.go:2595,3096 | TestContextOptimizedOrdersByLiveWindow, TestAffinityContextOptimizedLargestFirst, TestAffinityContextOptimizedTiesKeepInputOrder, TestAffinityContextOptimizedUnknownWindowSortsLast, TestConformanceDataLimitedStrategiesAreIdentityToday; V: pass 2026-11-20 11:33 IST (PC cold, toggle-check P1+P2) |
| cache-optimized | applyStrategyOrdering.ts orderByCacheAffinity | OrderByCacheAffinity (measured warmth → marker support → fallback score) wired into the live routing path via StrategyHookForRequest + 1-min warmth snapshot over ledger CachedTokenFraction; gate: prompt ≥1024 tokens, no tools, temp 0, no pinned contract; returns declared order when no target has measured warmth | FULL | internal/strategy/cache_affinity.go:276; internal/strategy/wiring.go:731; internal/router/router.go:2672; cmd/codvian-router/main.go:3097 | TestM65CacheOptimized* (6), TestCacheAffinity* (9), TestCacheWarmthSnapshot* (3) — V: pass 2026-11-20 11:28 IST (PC cold run + toggle-check) |
| fusion | fusion.ts:269 (parallel panel + judge) | Panel fan-out + judge wired into the engine (Phase D, D3, internal/router/combo.go): parallel calls + HeuristicJudge default; LLM judge via leaf callChat with heuristic fallback (tested but unwired — no judge model configured anywhere); JEV hold is a verdict row, never a fake answer; stream replays the combo result verbatim | FULL | internal/router/combo.go; internal/router/panel.go | TestFusionHeuristicPicksLongestNonRefusal, TestFusionCustomJudgeUsed, TestFusionAllTargetsFailNamesFailures + internal/router/combo_test.go (15 engine-path tests) |
| pipeline | pipelineRouter.ts:219 (multi-stage) | Multi-stage pipeline wired into the engine (Phase D, D3, internal/router/combo.go): stage results feed the next stage, last successful stage served when the last stage is optional-and-skipped | FULL | internal/router/combo.go; internal/router/panel.go | TestPipelinePassesOutputsInOrder, TestPipelineStageFailureAborts + internal/router/combo_test.go (15 engine-path tests) |
| jev | jevStrategy.ts:139 (judge, floor 0.15, conf 0.65, max 4) | Judge/admit/hold wired into the engine (Phase D, D3, internal/router/combo.go — floor 0.15, confidence 0.65, max 4 admitted); hold is a verdict row, never a fake answer | FULL | internal/router/combo.go; internal/router/panel.go | TestJevAdmitHoldHandComputed, TestJevMaxAdmittedDominated + internal/router/combo_test.go (15 engine-path tests) |
| quota-share (internal, 23rd) | routingStrategies.ts:30-34; quotaShareStrategy.ts:284 (DRR + P2C) | DRR wired into the engine (Phase D, D4, internal/router/cascade.go): per-pool deficits, 2:1 rotation verified against the strategy reference sequence. At-cap deprioritisation + P2C between the top two DRR candidates now pinned on the engine path (orderQuotaShare cascade.go:218, quotaShareDRR cascade.go:261 via Config.QuotaShareFor, main.go:3487); tree intercept (tree.go:237/256) pinned too — toggle (early return chain) FAILS all 5 tests. Operator share source wired (config quota_shares → QuotaShareFor). Quota-bucket saturation deliberately not re-gated: filterZeroQuota drops zero-hint members upstream (documented divergence). | FULL | internal/router/cascade.go:218,261; internal/strategy/quotashare.go; internal/router/tree.go:237,256; cmd/codvian-router/main.go:3487 | TestB2QuotaShareAtCapGating, TestB2QuotaShareP2CSwapsToLessLoaded, TestB2QuotaShareTreeIntercept, TestB2QuotaShareRankAttemptsIntercept, TestB2QuotaShareConfigSharesAreModelSpecific (5/5 PASS cold on PC; toggle: early return → 5 FAIL); V-B2-ROUTER-GROUP pass 2026-11-20 ~13:15 IST (PC) |
| Account-level fallback subset (9 values) | routingStrategies.ts:56-65 | Account fallback strategies built + wired (M3 residual S1): 9 values via ParseAccountFallbackStrategy; global + per-provider config surface with load-time validation; expiry-first rejected (no per-key quota-expiry source, D2 pending). Known limits: p2c is a random draw with breaker guard (no per-key score source exists, none invented); weighted draws uniformly (no per-account weight source). Config-file end-to-end proof: real config file → engine rotation observed. Quota-share cap LIVE (3b76fa9): per-key quota signal learned from real responses (setLearnedKeyQuotaHint/LearnedKeyQuotaHint, ratelimit.go; restore at startup, main.go); orderAccountCapCheck (account_fallback.go:232) runs after each strategy's ordering — at-cap accounts sink to the tail; none/all at cap → order unchanged (fail-open). DIVERGENCE from OmniRoute #20561: theirs is configured (operator sets cap_value) and blocks; ours is measured (learned, no config needed) and deprioritises. V-B2-09-CAP pass 2026-11-20 ~13:40 IST — cold PC 6/6 PASS; toggle: cap no-op → both cap tests FAIL, restored → PASS. | FULL | internal/router/account_fallback.go; internal/router/ratelimit.go; cmd/codvian-router/main.go | TestB2LearnRateLimitRecordsPerKeyHint, TestB2LearnedKeyQuotaHintDropsExpired, TestB2RestoreLearnedKeyQuotaHint, TestB2CapCheckSinksAtCapAccountsToTail, TestB2CapCheckFailOpen, TestB2OrderAccountAttemptsAppliesCapAfterStrategy; V: pass 2026-11-20 ~13:40 IST |
| auto 16-factor scoring (DEFAULT_WEIGHTS sum 1.0) | autoCombo/scoring.ts:62 | 16 factors computed, weights sum to 1.0, and the default wiring is now pinned: factory("balanced") orders on a 16-only factor (ResetAt) — TestBalancedHookOrdersBySixteenOnlyFactor PASS cold on PC; toggle-A (StrategyBalanced reverted to 5-factor HookWithWeights) FAILS. | FULL | internal/strategy/wiring.go:531-546; internal/scoring/scoring.go:138-142 | TestBalancedHookOrdersBySixteenOnlyFactor + TestSixteenWeightsSumToOne, TestSixteenFactorsCoverAllSixteenWeights, TestSixteenScoreIsWeightedSum, TestSixteenDefaultWeightsAreOmniRouteRescaledForLiveFactors; V-B2-16F-GROUP pass 2026-11-20 ~13:05 IST (PC) |
| Router strategy rules (default) | AUTO_ROUTING_STRATEGY_VALUES; routerStrategy.ts | Same 16-factor scorer as :510 (weights sum 1.0, all 16 factors computed, scorer tested) — the "default uses it" half is now pinned by the same toggle-proven test as :510. | FULL | internal/strategy/wiring.go:531-546; internal/scoring/scoring.go | TestBalancedHookOrdersBySixteenOnlyFactor; V-B2-16F-GROUP pass 2026-11-20 ~13:05 IST (PC) |
| Router strategy score | Same source | Same 16-factor scorer as :510 (weights sum 1.0, all 16 factors computed, scorer tested) — the "default uses it" half is now pinned by the same toggle-proven test as :510. | FULL | internal/strategy/wiring.go:531-546; internal/scoring/scoring.go | TestBalancedHookOrdersBySixteenOnlyFactor; V-B2-16F-GROUP pass 2026-11-20 ~13:05 IST (PC) |
| Router strategy cost / eco | Same source | Pure cheapest-first strategy "cost-optimized" wired (M3) + "eco" alias | FULL | internal/strategy/wiring.go | TestConformanceCostOptimizedCheapestFirst |
| Router strategy latency / fast | Same source | fastest + least-latency + ship-fast 16-factor pack all wired (M3); "fast" alias maps to fastest | FULL | internal/strategy/strategy.go; internal/strategy/wiring.go | TestConformanceModePacksDiffer |
| Router strategy sla-aware / sla | Same source | SLA config surface wired into the existing router SLA fields (M3 residual S2): SLATargetP95Ms/SLAMaxErrorRate/SLAMaxCostPer1M/SLAHardConstraints read by Engine.slaPolicy(); BreakerOpen pre-filter (OPEN candidates last, never dropped); zero-tolerance via SLAMaxErrorRateSet; Describe("sla-aware") discloses p50 EWMA stands in for p95 + stability neutral 0.5. Known limits: true p95/stddev deferred + disclosed (p50 EWMA is the stand-in). | FULL | internal/config/config.go:969,1075,1459-1475,2943; internal/router/router.go:610-621,2497-2505; internal/router/autodispatch.go:157-163; internal/router/wiring.go:661; internal/scoring/scoring.go:134; internal/strategy/strategy.go:412; cmd/codvian-router/main.go:2804-2821 | TestConfigSLARoundTrip, TestConfigSLAExplicitZeroIsZeroTolerance, TestConfigSLAValidation, TestSLAAwareHookOrdersCompliantFirst, TestSLAAwareHookFiltersBreakerOpenCandidates, TestSLAAwareHookZeroToleranceHonoured, TestDescribeSLAAwareDisclosesP50StandIn, TestDispatchSLAAwareHonoursConfiguredPolicy |
| Router strategy lkgp | Same source | LKGP ordering fully wired (Phase C): record on success + order + persistence — see the lkgp row above | FULL | internal/strategy/wiring.go; internal/strategy/lkgpregistry.go | TestConformanceLKGPOrdersByLastGood |
| Router strategy nadir | nadirStrategy.ts:2-16 (external Nadir API, fail-open) | Nadir config surface (M3 residual S3, reduced scope): NadirConfig {endpoint, timeout_ms} with load-time validation; wired in main.go into the existing engine fields; 30s failure cooldown already in code (NadirFailureCooldownMs=30000, per-endpoint key, check-before-dial + arm-on-failure). Deliberate divergence: NO default endpoint and NO env fallback for the endpoint (fail-closed; egress must be an explicit reviewed config choice) vs OmniRoute's fail-open hardcoded https://api.getnadir.com; API key from CODVIAN_ROUTER_NADIR_API_KEY env only, never the config file. | FULL | internal/config/config.go:1008,1117,1528-1535,3005-3011,3429-3454; cmd/codvian-router/main.go:2819-2840; internal/router/router.go:644-665; internal/router/nadir.go:75-79,82-96,208-221 | TestConfigNadirRoundTrip, TestConfigNadirValidation, TestNadirConfiguredValuesReachEngine, TestDispatchNadirHonoursConfiguredEndpoint, TestNadirFailureCooldownSkipsCallsThenResumes |
| Mode pack ship-fast | modePacks.ts (latencyInv 0.3048, health 0.2667) | Verbatim pack table wired as a selectable strategy via the 16-factor scorer (M3) | FULL | internal/scoring/scoring16.go:374; internal/strategy/wiring.go | TestSixteenModePackTablesMatchReference, TestConformanceModePacksDiffer |
| Mode pack cost-saver | modePacks.ts (costInv 0.3324) | Verbatim pack table wired as a selectable strategy via the 16-factor scorer (M3) | FULL | internal/scoring/scoring16.go:374; internal/strategy/wiring.go | TestSixteenModePackTablesMatchReference, TestConformanceModePacksDiffer |
| Mode pack quality-first | modePacks.ts (taskFit 0.3524, stability 0.1429) | Verbatim pack table wired as a selectable strategy via the 16-factor scorer (M3) | FULL | internal/scoring/scoring16.go:374; internal/strategy/wiring.go | TestSixteenModePackTablesMatchReference |
| Mode pack offline-friendly | modePacks.ts (quota 0.3324) | Verbatim pack table wired as a selectable strategy via the 16-factor scorer (M3) | FULL | internal/scoring/scoring16.go:374; internal/strategy/wiring.go | TestSixteenModePackTablesMatchReference |
| Mode pack reliability-first | modePacks.ts (health 0.3524, stability 0.1905) | Verbatim pack table wired as a selectable strategy via the 16-factor scorer (M3) | FULL | internal/scoring/scoring16.go:374; internal/strategy/wiring.go | TestSixteenModePackTablesMatchReference |
| Mode pack chaos-mode | modePacks.ts (health 0.4, taskFit 0.1905; parallel panel) | Weight pack wired as a selectable strategy (M3); parallel fan-out live via the D3 combo path; fault-injection LIVE (1403ffe): ArmFault/DisarmFault/ClearFaults/ListFaults + callChat hook (router.go:3624) — FaultFail returns a real provider error (429→RateLimitError, 402→QuotaError, else UpstreamError), FaultDelay stalls then proceeds; per-(provider, model) match, MaxHits auto-disarm, OFF by default. OmniRoute has NO fault-injection (their chaos surface is panel fan-out + overrides/timeouts only) — this is our own design. Scope: chat completions via callChat only; streaming/embeddings not covered (later harness). V-B2-13 pass 2026-11-20 ~13:43 IST — cold PC 8/8 PASS; toggle: hook off → 3 chain tests FAIL, on → 8/8 PASS. | FULL | internal/router/faultinject.go; internal/router/router.go:3606,3624; internal/scoring/scoring16.go:374 | TestB2FaultInjectFailoverToFallback, TestB2FaultInjectDisarmRestoresNormalRouting, TestB2FaultInjectDelayStallsThenProceeds, TestB2FaultInjectMaxHitsAutoDisarm, TestB2FaultInjectMaxHitsAutoDisarmAcrossRequests, TestB2FaultInjectErrorTypes, TestB2FaultInjectModelNarrowing, TestB2FaultInjectOffByDefault; V: pass 2026-11-20 ~13:43 IST |
| Live intelligence (Arena ELO + models.dev) | MODEL_INTELLIGENCE.md; Arena ELO sync 24h + models.dev tier data | Arena ELO sync live (HF-primary since 2026-11-20 — live probe: wulong 404 dead on text/code/overall/coding, HF datasets-server 200 with 426 overall / 421 coding rows; models.dev api.json 200, 6.78 MB), models.dev tier derivation, TaskFit priority (user_override > arena_elo > models_dev_tier), scoring hook sets TaskFitScore/TaskFitKnown in the ranking closure (main.go:2900) and FactorTaskFit reads it first; models.dev sync opt-in OFF by default is deliberate (local-first, no network unless operator opts in). CategoryDefault-only lookup in the ranking closure is deliberate and documented in-tree (no per-request category exists there; the per-request hook layers one when present). V-B2-524 pass 2026-11-20 ~14:10 IST — cold PC 9/9 PASS (modelsync 6/6, scoring 3/3); toggle: fetch order flipped to wulong-first → TestB2_524_HFFirst_WulongDead FAILS, restored → PASS. | FULL | internal/modelsync/intelligence.go; internal/modelsync/modelsync.go; internal/config/config.go | TestB2_524_HFFirst_WulongDead, TestB2_524_WulongFallback_WhenHFDead, TestB2_524_SyncOffTouchesNoNetwork, TestB2_524_TaskFitPriority, TestB2_524_TierDerivation, TestB2_524_ParseAndTransform, TestB2_524_FactorTaskFitKnownWins, TestB2_524_FactorTaskFitUnknownFallsBack, TestB2_524_FactorTaskFitKnownInvalidFallsThrough; V: pass 2026-11-20 ~14:10 IST |
| Candidate-pool endpoint | GET /v1/auto-combo/{channel}/candidates (AUTO-COMBO.md:82-84) | Route registered GET /v1/auto-combo/{channel}/candidates (Phase D, D1) with the live pool: tier models, breaker states, per-key connections, cooldowns; free/pro + category filters applied from the live price book + tier membership. All three gap-closers now tested: exclusions store, ModelLocked forwarder (StaleEngine.ModelLockouts), FreeAccessExclusion derivation — 3/3 PASS cold on PC, each toggle-proven (off → its test FAILS). Caveats: subscription/lkgp 503 fail-closed by design, untested at the endpoint; FreeAccessExclusion is a documented price-only approximation (nil = known $0 by price, not allowance-verified). | FULL | internal/server/autocombo_route.go:105, internal/server/server.go:3686, internal/degrade/degrade.go:2547 | TestB525AutoCombo* (3) + TestAutoCombo* (16) + TestG19AutoCombo* (7) = 26 tests, V-B2-525: pass 2026-11-20 ~13:03 IST (PC, HEAD d6ea9ae) |
| Decision header | X-OmniRoute-Decision on every response | X-Codvian-Decision is richer: provider/model/strategy/reason + region_step | FULL | internal/server/decision.go:3-37,55-100,169,200 | UNVERIFIED |
| auto (balanced, LKGP-sticky) | AUTO-COMBO.md:13-28 | LKGP session affinity layered into pool inputs (LatestHealthyRecord, main.go) and factor math unit-tested (TestFactorSessionAffinity 1.0/0.7/0.3/0.5); pool-level stickiness now tested: TestPoolRankingLKGPStickinessLeads runs the real pool ranking with an LKGP pair and asserts the last-good model leads — PASS cold on PC; toggle-B (no-record) FAILS. | FULL | internal/autorouter/autorouter.go:206; cmd/codvian-router/main.go:1284,2849-2876; internal/strategy/lkgpregistry.go:99 | TestPoolRankingLKGPStickinessLeads + TestFactorSessionAffinity, TestG19LKGP*, TestAutoVariantLKGPExplicit; V-B2-16F-GROUP pass 2026-11-20 ~13:05 IST (PC) |
| auto/coding | AUTO-COMBO.md:22 quality-first | auto/coding + auto/code resolve in the engine (Phase D, D2 autodispatch): real catalog price/latency/quality rules, dispatch-level tested (18 dispatch tests) | FULL | internal/router/autodispatch.go; internal/autorouter/autorouter.go:208-209 | TestAutoVariantCodeNarrowsToCodingReasoning |
| auto/fast | AUTO-COMBO.md:23 low-latency weights | auto/fast resolves in the engine (Phase D, D2 autodispatch): real low-latency rules, dispatch-level tested (18 dispatch tests) | FULL | internal/router/autodispatch.go; internal/autorouter/autorouter.go:206 | internal/router/autodispatch_test.go (18 dispatch tests) |
| auto/cheap | AUTO-COMBO.md:24 cost-optimized | auto/cheap resolves in the engine (Phase D, D2 autodispatch): real cost rules, dispatch-level tested (18 dispatch tests) | FULL | internal/router/autodispatch.go; internal/autorouter/autorouter.go:207 | TestAutoVariantCheapUnknownPriceSortsLast |
| auto/offline | AUTO-COMBO.md:25 most quota/headroom first | Resolver + engine dispatch live (Phase D, D2 + D2X): offline-friendly, most headroom first; covered by the 11-id dispatch smoke test through Engine.Chat | FULL | internal/router/autodispatch.go; internal/autorouter/autovariants.go | TestAutoVariantOfflineMostHeadroomFirst |
| auto/smart | AUTO-COMBO.md:26 quality-first + 10% exploration | auto/smart resolves in the engine (Phase D, D2 autodispatch): quality-first + 10% exploration draw at dispatch, seed-deterministic (seed 9 explores, seed 0 exploits), dispatch-level tested (18 dispatch tests) | FULL | internal/router/autodispatch.go; internal/autorouter/autorouter.go:210 | TestAutoVariantSmartNotesExploration |
| auto/lkgp | AUTO-COMBO.md:27 explicit LKGP | Resolver + engine dispatch live (Phase D, D2 + D2X): global last-good wired via the SetAutoLastGood installer reading DefaultLKGPRegistry.Latest(); e2e through Engine.Chat (newest record wins, empty registry fails closed); errors when none recorded | FULL | internal/router/autodispatch.go; internal/router/autodispatch_lkgp_test.go; internal/autorouter/autovariants.go | TestAutoVariantLKGPExplicit |
| auto/chaos | AUTO-COMBO.md:28; README:369 (parallel fan-out, panel of 5) | Panel of 5, one model per provider; parallel fan-out engine-wired via the chaos-mode combo path (Phase D, D3); dispatch-order preservation now pinned both halves: panel branch (autodispatch.go:300-305) → router.go:7101/7108/7113 → chatWithChainStrategy (router.go:4516); toggle (OrderedExplicit off + explicitOrder off) FAILS all 3 tests. Fault-injection deliberately not implemented — belongs in the M4 chaos harness. | FULL | internal/router/combo.go; internal/autorouter/autovariants.go; internal/router/autodispatch.go:300-305 | TestB2ChaosDispatchReturnsExplicitPanel, TestB2ExplicitOrderSurvivesQuotaHintSort, TestB2ExplicitOrderSurvivesStrategyHook (3/3 PASS cold on PC; toggle: OrderedExplicit off → 3 FAIL); V-B2-ROUTER-GROUP pass 2026-11-20 ~13:15 IST (PC) |
| auto/subscription | SUBSCRIPTION_LADDER.md:71-82 (fail closed, never billable fallback) | Fail closed, rung-0 only, never a billable fallback; engine dispatch live (Phase D, D2). Ladder semantics now pinned end to end: TestB2SubscriptionServesRungZeroThroughChat drives relay.go:1324 → Engine.Chat → dispatchAutoVariant (autodispatch.go:237) → resolveSubscription (autovariants.go:709); toggle (rung disabled) → Chat errors, restored → PASS. Rung-0 serves, 1 call, no billable fallback; empty rung fails closed. Live billing/subscription data remains absent by design (main.go:1189-1195: "honest limits, not data") — a documented limit, not a missing feature. | FULL | internal/router/autodispatch.go; internal/autorouter/autovariants.go | TestB2SubscriptionServesRungZeroThroughChat (PASS cold on PC; toggle: rung off → FAILS); V-B2-AUTOFIX-GROUP pass 2026-11-20 ~13:21 IST (PC) |
| auto/thrifty | SUBSCRIPTION_LADDER.md:95-106 | 5-rung ladder (subscription→keyless→free→cheap→premium) live at dispatch (Phase D, D2: resolveThrifty → cost-saver strategy); exhausted rungs gated out by real quota signals — liveAutoCatalog enrichment (autodispatch.go:175-180, HasQuota → RemainingPercent) now pinned: toggle (enrichment off) FAILS the quota-hinted test; hintless control passes. | FULL | internal/router/autodispatch.go:175-180; internal/autorouter/autovariants.go | TestB2ThriftyQuotaHintsGateExhaustedRung, TestB2ThriftyWithoutHintsServesExhaustedRung (2/2 PASS cold on PC; toggle: enrichment off → 1 FAIL); V-B2-ROUTER-GROUP pass 2026-11-20 ~13:15 IST (PC) |
| auto/chat | builtinCatalog.ts:37 (auto/chat → balanced default) | auto/chat resolves via the variant resolver (balanced) with engine dispatch live (Phase D, D2 + D2X 11-id smoke test through Engine.Chat) | FULL | internal/router/autodispatch.go; internal/autorouter/autorouter.go | TestAutoVariantChatThriftyComposition |
| auto/best-* family | builtinCatalog.ts:23-29,46 | Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); vision + reasoning now served from real production sources — Vision from models.dev sync (modelsync.go:196/609 → main.go:1242), Reasoning from category + modelsync intelligence. TestB2BestVisionAndReasoningServeCapableModelThroughChat drives both through Chat; toggle (capability filter off) → both sub-cases FAIL with "no vision/reasoning-capable model… fail-open". Positive + fail-open branches both proven. | FULL | internal/router/autodispatch.go; internal/autorouter/autovariants.go | TestB2BestVisionAndReasoningServeCapableModelThroughChat (PASS cold on PC; toggle: filter off → 2 FAIL); V-B2-AUTOFIX-GROUP pass 2026-11-20 ~13:21 IST (PC) |
| auto/pro-* family | builtinCatalog.ts:30-34 | Family resolver + engine dispatch live (Phase D, D2 + D2X smoke test); premium classification now from real prices — main.go:1258 classifies Tier via ClassifyPriceTier (premium above $1/M input), so premium models exist in production. TestB2ProCodingServesPremiumPickThroughChat drives pro-coding through Chat; toggle (tier filter off) → both sub-cases FAIL with "no premium-tier model… fail-open". Positive + fail-open branches both proven. | FULL | internal/router/autodispatch.go; internal/autorouter/autovariants.go | TestB2ProCodingServesPremiumPickThroughChat (PASS cold on PC; toggle: tier filter off → 2 FAIL); V-B2-AUTOFIX-GROUP pass 2026-11-20 ~13:21 IST (PC) |
| auto/<category>:<tier> composition | AUTO-COMBO.md:30-45; builtinCatalog.ts:64-74 | Suffix composition implemented in the resolver (Phase C) with engine dispatch live for the tested compositions (Phase D, D2X smoke test over category:tier ids through Engine.Chat) | FULL | internal/router/autodispatch.go; internal/autorouter/intent.go:8-10 | TestAutoVariantCategoryTierComposition |
| Default strategy = priority; unknown → priority | comboConfig.ts:113; routingStrategies.ts:76-80 | Priority declared first, config-accepted, tree + router + feed defaults → priority; unknown names fall back to priority (Wave 1, M1.11) | FULL | internal/strategy/strategy.go; internal/router/router.go; internal/router/tree.go | TestPriorityHookPreservesDeclaredOrder, TestW1EffectiveStrategyDefaultsToPriority |
| 4-tier fallback cascade (Subscription → API Key → Cheap → Free) | Deep-study §1.1 (exact wording not re-verified fresh) | Cascade ordering built and tested (Phase C); engine helper applyCascadeOrder built and tested with injected tier data (Phase D, D4, internal/router/cascade.go). Selector now LIVE (2508342): config allowlist entry (global_strategy + chain_strategies), StrategyCascade in All (Parse/IsValid/picker), Describe entry, wiringHook identity case. Note: the helper sorts unplaced attempts last and reports no exhaustion, diverging from OrderCascade's omit-and-report-Exhausted semantics (deliberate — the engine must not drop attempts). V-B2-542 pass 2026-11-20 ~13:59 IST — cold PC 5/5 PASS (config 2/2, router e2e 1/1, strategy 2/2); real config file → loaded value → effectiveStrategy → cascade order + real dispatch serves subscription first; toggle: allowlist entry removed → config 2/2 FAIL + router e2e FAIL, restored → PASS. Dashboard UI control is a separate row and stays out of this flip. | FULL | internal/router/cascade.go; internal/config/config.go; internal/strategy/strategy.go | TestB2CascadeGlobalStrategyLoads, TestB2CascadeChainStrategyLoads, TestB2CascadeSelectorRoutesFromConfigFile, TestB2CascadeParse, TestB2CascadeHookIsIdentity, TestCascadeOrderWithTierSource, TestCascadeOrderSubscriptionAPIKeyCheapFree, TestB2CascadeOrderAppliesAtDispatch; V: pass 2026-11-20 ~13:59 IST |
| Endpoint-specific combos (chat, search, embeddings, rerank, images, video, music, OCR, audio) | endpointCapabilities.ts:36 comboForEndpoint | Endpoint projection built and tested (Phase C); enforced at all three live engine entry points (Phase D, D4) — chat, stream, embeddings — fail-closed with zero provider calls on rejection; unknown endpoint kinds fail closed. Embeddings + rerank + images live-enforced (M8.5: Engine.Images → singleAttempt, internal/router/embeddings.go:248, projection via checkEndpointStrategy, internal/router/cascade.go:308); search/video/music/OCR have an engine surface (M8.20: Engine.MediaPassthrough, internal/router/media_verify.go:158); audio has an engine surface (M8.6: Engine.AudioPassthrough, internal/router/audio_passthrough.go:352) but no provider client, so no audio call can succeed end to end. Projection now pinned on all 9 kinds: TestB2EndpointProjectionRejectsFusionOnMediaKinds — toggle (projection off) → all 6 media kinds (images, search, video, music, OCR, audio) served instead of rejected; restored → PASS. Projection enforced at live entry points (router.go:4570 chat, :5598 stream, embeddings.go:302). Audio has no provider client — a separate feature (audio calling), not this row's projection. | FULL | internal/router/cascade.go; internal/autorouter/autovariants.go | TestB2EndpointProjectionRejectsFusionOnMediaKinds (PASS cold on PC; toggle: projection off → 6 FAIL); V-B2-AUTOFIX-GROUP pass 2026-11-20 ~13:21 IST (PC) |
| Shared attempt budget maxGlobalAttempts, clamped [1, 200] | comboConfig.ts:137 default 30; comboPredicates.ts:130-137,176-186 | Clamped [1, 200] live in withDefaults (router.go:998-1007), applied by NewEngine (router.go:1385); 0→5 default, 500→200, 3→3, 200→200 (TestMaxAttemptsClamp, toggle-proven: clamp off → 500→500 FAIL). Note: ours defaults 5, OmniRoute 30. V: pass 2026-11-20 11:50 IST | FULL | internal/router/router.go:998-1007,1385 | TestMaxAttemptsClamp |
| Nested combos (maxComboDepth 3, flatten) | comboConfig.ts; comboPredicates.ts | No combo-in-combo — DELIBERATE: we have nestable strategy trees instead (internal/strategytree: a node picks among its children by its own strategy, leaves are provider/model; each level can use a different rule), which do the same job and more. Building combo-in-combo would add a second system doing the same work. Raunak's call 2026-11-20 ~13:28 IST: use our trees, do not copy everything that does the same job. | DIVERGENCE | internal/strategytree; internal/router/tree.go | — |
| Preferred / reserve accounts for auto combos | Deep-study §1.1; preferredConnectionFailover.ts (opt-in response-start failover, 20 s default); auth.ts:1930-1945 (per-key soft preference list — ours is a static operator-set key role, not per-request; design §S4(5)) | Provider key roles preferred/reserve; preferred first, reserve sorted last on flat + auto-combo paths; opt-in response-start failover (20 s, already FULL in Area 2); verified 16/16 router tests + 9/9 live smoke (docs/design/m3-s4-evidence-2026-11-19.md) | FULL | internal/provider/keys.go; internal/router/account_fallback.go:163-180; internal/router/autodispatch.go:330-336; internal/router/preferred.go | TestKeyPool*, TestPreferredFailover*, TestChainForDispatchPreferredReserve, TestOrderAttemptsAppliesAccountFallbackThenReserveLast |
| Deadline-failure sub-budget → clean 504 | comboConfig.ts comboTimeoutMs; comboPredicates.ts:189-226 | Per-combo deadline sub-budget returning clean 504 is built and verified | FULL | internal/router/router.go (ErrDeadlineExceeded); internal/server/server.go:5134 | UNVERIFIED |
| Hedging config (off by default, 500 ms, bodies >256 KB never hedged) | comboConfig.ts:177; comboAttemptLoop.ts:76,359-374 | Hedging ON by default at 150 ms (deliberate — faster failover), disabled for explicit-order chains; body-size guard LIVE: hedgeMaxBodyBytes 256 KiB, requestBodyBytes counts messages/parts/tool calls/tool definitions/tool choice, hedgeAllowed() gates both firing sites (chat router.go:4964, stream router.go:6374). V-B2-26 pass 2026-11-20 ~13:37 IST — cold PC 11/11 PASS (7 hedge tests + 4 guard tests); toggle: guard off → TestHedgeBodySizeGuard FAILS, guard on → PASSES. | FULL | internal/router/hedge.go; internal/router/router.go:4964,6374 | TestHedgeBodySizeGuard + 7 hedge tests + 4 guard tests (m4_8_test.go); V: pass 2026-11-20 ~13:37 IST |

AHEAD (Area 1, ours-only, not in the 63 denominator): auto/code cost-per-solved-task
ranking (`internal/autorouter/autorouter.go:972`); nestable strategy trees (MaxDepth 8);
user-named routing groups as addressable targets (`internal/router/groups.go`); local
auto-router decision speed ~71 µs/op (BenchmarkClassify, measured); router added latency
0.842/0.691 ms vs OmniRoute's claimed "1–2 ms router overhead". (The `code`-strategy
config rejection, P0-9, was fixed in M1.10 — `code` is now accepted.)

### Area 2 — Failover, breakers, retries, hedging (46 rows: FULL 40, PARTIAL 0, MISSING 0, WRONG 0, AHEAD 3, DIVERGENCE 3; comparable 40; strict 100.0%, weighted 100.0%)

- M4 close-out (2026-11-18): recount verified mechanically from the table (46 rows). M4 flipped 19 rows → FULL (Retry-After JSON body, concurrency 429, quota-text 429, model lockout, per-profile breaker thresholds, backoff escalation, window breaker, transient retry, watchdog, predictive TTFT, token-refresh breaker, per-target timeout, identical-error stop, hedge guard, stream early-retry, attempt cap, exhausted skip, wait budgets, preferred-account failover) and 3 rows → DIVERGENCE (402, mid-stream continuation, admission queue). Remaining PARTIAL (7): 200-OK body predicate, breaker persistence, connection cooldown, deadline-failure sub-budget, 3-attempt cross-provider default, failover-before-retry knobs, fallback-chains CRUD API.

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| 400 → stop, except body signals model-not-found → fallback | failureClassification.ts:78 | 400 with IsModelNotFoundSignal falls back, plain 400 stops | FULL | internal/provider/taxonomy.go:317-330 | UNVERIFIED |
| 401/403 → stop | failureClassification.ts:65-68 | 401/403 stop; 401 also per-key 5-min cooldown in multi-key pools | FULL | internal/provider/taxonomy.go:306-315; internal/router/router.go:1203,1211 | UNVERIFIED |
| 402 → quota, never retried on same provider | failureClassification.ts:70 | 402 fails over to the next target, never same-target retried, breaker-neutral. OmniRoute stops the chain (quota_exhausted, retryable:false); DELIBERATE: billing/quota is per-target — A's empty balance says nothing about B | DIVERGENCE | internal/wire/retryable.go:252; internal/provider/taxonomy.go:408-414 | TestPaymentRequired402FailsOver |
| 404 → model-unavailable handling | errorClassifier.ts:468-493 | 404 always falls back | FULL | internal/provider/taxonomy.go:334-341 | UNVERIFIED |
| 408 / 504 timeout → retryable, trips breaker | failureClassification.ts:69 | 408 retryable; excluded from 4xx-neutral rule so it counts toward breaker | FULL | internal/wire/retryable.go:183 | UNVERIFIED |
| 429 → failover; quota-text 429 → quota_exhausted, non-retryable | failureClassification.ts:71-74; QUOTA_MESSAGE regex :26 | Quota-text 429 → quota_exhausted: abandoned (never same-target retried), honoured wait or 60 s cooldown; plain 429 fails over; concurrency 429 separate (below) | FULL | internal/wire/retryable.go (IsQuotaExhaustedSignal); internal/router/retryafter.go; internal/router/router.go noteFailure | TestQuota429AbandonsTarget, TestQuota429FailsOverImmediately |
| 500/502/503/504 (+529) → retryable | failureClassification.ts:83 | 500/502/503/504/529 all retryable; 529 is ours-only | FULL | internal/wire/retryable.go:183 | UNVERIFIED |
| 405/501 retryable only on select endpoints | failureClassification.ts (unknown, non-retryable) | 405/501 retryable on /v1/responses, /v1/embeddings, /v1/rerank | AHEAD | internal/wire/retryable.go:195 | UNVERIFIED |
| Network / transport errors retryable | failureClassification.ts:83 | Untyped transport failure fails open to fallback | FULL | internal/provider/taxonomy.go:376 | UNVERIFIED |
| Empty stream / 200-no-content → pre-commit failover | errorClassifier.ts empty-content guard | Preread 8 events / 256 KiB / 5 s holdback, always on | FULL | internal/router/preread.go:67-77,147,201,238 | UNVERIFIED |
| 200-OK body validation predicate → failover | comboConfig.ts responseValidation per-combo predicate | Non-streaming path (ValidateResponseBody in callChat) + streaming path now live: validatePrereadPrefix checks every preread commit point (complete-JSON error payload / empty choices, forbidden substrings); failure fails over pre-commit with the same UpstreamError. Built-in predicate enabled by default. Follow-ups: config-file wiring routing.response_validation → engine (main.go mapping) missing; split error payloads not caught (inherent pre-commit limit); jsonPathPredicates deliberately unimplemented. | FULL | internal/router/preread.go:279; internal/router/response_validation.go:28; internal/router/router.go:3394,368 | TestPrereadErrorPayloadFailsOver, TestPrereadValidationDisabledAcceptsBadBody, TestPrereadForbiddenSubstringFailsOver, TestPrereadValidBodyCommits, TestPrereadToolCallExemptsJSONCheck, TestValidatePrereadPrefix — V-21: pass 2026-11-19T22:15Z |
| Breaker states CLOSED/DEGRADED/OPEN/HALF_OPEN, persisted, lazy recovery | RESILIENCE_GUIDE.md:46-48,62 | CLOSED/DEGRADED/OPEN/HALF_OPEN all reported (M4.2); backoff escalation present; persistence now live: lastFailureAt persisted in breaker store and BreakerStorePath wired in production (main.go, DataDir/breaker_state.json); stale DEGRADED decays to CLOSED on read after a quiet hold, next failure after a quiet hold counts fresh; OPEN/HALF_OPEN keep trip count and escalation. | FULL | internal/router/router.go:1024,2640,2864,2870; internal/router/breaker_store.go:90,111,135,159; cmd/codvian-router/main.go:2957 | TestBreakerStaleDegradedDecaysToClosedOnRead, TestBreakerNextFailureAfterQuietHoldCountsFresh, TestBreakerStaleDegradedPersistsAcrossReload, TestBreakerOpenKeepsEscalationAfterQuietHold, TestBreakerDegradedState, TestBreakerEscalationEngine — V-19: pass 2026-11-19T22:08Z |
| Breaker trips ONLY on [408,500,502,503,504]; never on 401/403/429 | RESILIENCE_GUIDE.md:60,1103 | isClientErrorFailure + breakerNeutral; 429s/4xx/hedge losers neutral | FULL | internal/router/router.go | UNVERIFIED |
| Breaker thresholds/cooldowns per auth profile | constants.ts:296-344 (OAuth 5/8/60s; API-key 7/12/30s; Local 2/15s) | Per-profile thresholds live (OAuth open 8 / reset 60 s / degrade 5; API-key 12 / 30 s / 7; local 2 / 15 s); AuthProfileFor wired from the registry auth kind (M4.9) | FULL | internal/router/breaker_profile.go:80-119; internal/router/breaker_profile.go:174; cmd/codvian-router/main.go AuthProfileFor | TestBreakerProfilesMatchOmniRoute, TestBreakerPerProfileThresholds, TestAuthProfileForKind |
| Provider-level failure-window breaker | constants.ts:304-306,319-324,342-344 | Window breaker per provider (OAuth 10/15 min/5 min; API-key 15/30 min/10 min; local 2/5 min/1 min); window-open refuses attempts alongside the consecutive breaker; successes never erase window events, only time prunes them | FULL | internal/router/providerbreaker.go:40-79 | TestProviderWindowBreaker |
| Breaker backoff escalation (reset × up to 8× OAuth / 4× API-key) | constants.ts:309-310,326-328 | escalatedResetTimeout doubles the reset every 2 (OAuth) / 3 (API-key) trips, capped ×8 / ×4; success resets the ladder; explicit BreakerOpenFor overrides stay fixed | FULL | internal/router/breaker_profile.go:131 | TestBreakerEscalationLadder, TestBreakerEscalationEngine, TestBreakerExplicitOverride |
| Connection cooldown (one key/account) | settings.ts:105-116; constants.ts transientCooldown 5000/3000 | Hard cooldown 5 s doubling, capped 5 min; soft-demote mode; per-key dead-key cooldown 5 min. Per-auth base live (B2-529F, e7d5cbe): OAuth 5 s / API-key 3 s / Local 2 s, used as the hint-less 429 fallback (router.go:4226) and the soft-ladder start; anti-thundering-herd release stagger live (CooldownStaggerFor, effectiveCooldownUntil at cooldown.go:262 + router.go:3334). | FULL | internal/router/router.go; internal/router/cooldown.go; internal/router/cooldown_auth.go:55,93 | TestCooldownFeedLive, TestCooldownFeedLiveEngine, TestCooldownUpdate, TestCooldownUpdatePersists, TestG11CooldownBasesDifferPerAuthProfile, TestG11CooldownStaggerSpreadsReadmission; V: pass 2026-11-20 12:20 IST (PC cold + toggle-check) |
| Retry-After honored (seconds + HTTP-date + X-RateLimit-Reset) | accountFallback.ts:1540,1581-1618; clamped 30 days | parseRetryAfter (int, fractional, HTTP-date); honoured floored 10 s / capped 30 s | FULL | internal/provider/client.go:1217; internal/router/retryafter.go:79-102 | UNVERIFIED |
| Retry-After ≥60 s → abandon target, fail over immediately | Opposite: waits it out within 90 s/5-attempt/300 s budget | RetryAfterAbandonAt = 60s; 429 fails over immediately | AHEAD | internal/router/retryafter.go:79-83 | UNVERIFIED |
| Retry-After from JSON body (Gemini RetryInfo, "retry in Xs", ISO, "resets in XhYmZs") | accountFallback.ts:1581-1618 + retryAfterJson.ts | ParseRetryAfterBody: Gemini retryDelay, "retry in Xs", "resets in 1h2m3s", ISO 8601; header wins, body is the fallback; wired into mapStatusErrorForEndpoint and both Gemini mappers | FULL | internal/provider/retryafter_body.go:53; internal/provider/client.go mapStatusErrorForEndpoint; internal/wire/gemini.go | TestParseRetryAfterBody, TestMapStatusError429BodyRetryAfter, TestMapGeminiError |
| Concurrency-limit 429 → fail over now, 1 s cool, no backoff climb, no breaker failure | concurrencyLimit429.ts (COOLDOWN 1,000 ms) | Concurrency-limit 429 → 1 s hard + forced 1 s soft cooldown, consecutive429 NOT incremented, breaker-neutral; multi-word phrase match only ("concurrency limit", "too many concurrent", ...) | FULL | internal/wire/retryable.go (IsConcurrencyLimitSignal); internal/router/retryafter.go; internal/router/cooldown.go setSoftCooldownUntil | TestConcurrency429CoolsOneSecond, TestConcurrency429NoBackoffClimb |
| Model lockout (provider+connection+model; off by default; 403/404/429/502/503/504; base 120 s, max 30 min, max 10 steps) | modelLockoutSettings.ts (enabled: false) | ModelLockout per (provider, key, model); off by default; ladder base 120 s ×2 to max 30 min, max 10 steps; success clears; expiry restarts the ladder; config surface routing.model_lockout (M4.9) | FULL | internal/router/lockout.go:92-167; internal/config/config.go ModelLockoutConfig | TestModelLockoutSkipsDeadModel, TestModelLockoutSuccessClears, TestModelLockoutPerModel, TestConfigM49RoundTrip |
| Same-host transient retry (502/503/504, max 3, decorrelated full-jitter, base 200 ms / cap 2,000 ms) | transientBackendRetry.ts | Decorrelated full-jitter same-host retry for 502/503/504 only, max 3, base 200 ms / cap 2,000 ms; Chat + stream loops; reserve guard leaves one attempt for the next target | FULL | internal/router/transient.go:42-78 | TestTransientRetryRecovers, TestTransientRetryMaxThree, TestTransientRetryOnly502_503_504, TestTransientRetryStream |
| Same-target 429 backoff when no hint | Base 5 s OAuth / 3 s API-key / 2 s Local; ×2 per level | rateLimitWait — exp backoff base 200 ms ×2ⁿ, cap 5 s, jitter [0.5×,1×] | FULL | internal/router/router.go:2224-2250,575-579 | UNVERIFIED |
| Wait budgets (90 s/wait, 5 retries, 300 s total; never on quota/auth/not-found) | settings.ts:143-159 | Opt-in wait budgets (defaults 90 s / 5 / 300 s); waits instead of failing over when the hint fits; never on quota/concurrency; config surface routing.wait_budgets (M4.9). Fail-fast remains the default | FULL | internal/router/waitbudget.go:78 | TestWaitBudgetWaitsInsteadOfFailingOver, TestWaitBudgetHonoursHint, TestWaitBudgetNeverOnQuota, TestConfigM49RoundTrip |
| Hedged / parallel requests | comboConfig.ts:177-178 (off, 500 ms; bodies >256 KB never hedged) | ON by default, 150 ms; loser cancelled, breaker-neutral; OFF on explicit-order chains; bodies >256 KB never hedged (M4.8), now counting tool definitions + ToolChoice toward the 256 KiB budget (commit c1b215e). Deliberate default difference: OmniRoute defaults hedge off at 500 ms; ours defaults on at 150 ms | FULL | internal/router/hedge.go:16,23-43,51-52; internal/router/router.go:4613,5998 | TestHedgeBodySizeGuard, TestHedgeBodySizeGuardBoundary, TestHedgeBodySizeGuardCountsParts, TestHedgeBodySizeGuardCountsTools — V-18: pass 2026-11-19T22:08Z |
| Stream recovery holdback (750 ms / 64 KB, off by default; early-retry max 4) | constants.ts STREAM_RECOVERY; streamRecovery.ts:5 | Early-retry max 4 re-opens the same target on truncation/empty/transport blips (M4.8), reserve-guarded; preread 8 events / 256 KiB / 5 s always on (deliberate: OmniRoute holdback off by default at 750 ms / 64 KB) | FULL | internal/router/streamrecovery.go:47-66 | TestStreamEarlyRetryReopens, TestStreamEarlyRetryMaxFour, TestStreamEarlyRetryEmptyStream, TestStreamEarlyRetrySkips429 |
| Mid-stream continuation / post-commit resume (opt-in, min 8-char overlap) | streamRecovery.ts:594-664 | None — automatic resume-after-commit deliberately removed; explicit entry points only. Post-commit the stream never switches (a resume IS a switch after commit, and the preread rule forbids it) | DIVERGENCE | internal/router/router.go:4120-4132 | — |
| Throughput watchdog on stalled streams (30 s warmup / 30 s window / 4 B/s) | constants.ts STREAM_THROUGHPUT_WATCHDOG | Throughput watchdog in forwardStream: 30 s warmup / 30 s window / 4 B/s; aborts pre-commit (failover) or post-commit (terminal); preread separately bounded at 5 s | FULL | internal/router/watchdog.go:37-45,85-108 | TestWatchdogAbortsStalledStream, TestWatchdogFailsOverPreCommit, TestWatchdogTrickleBelowFloor |
| Preferred-account response-start failover (opt-in; 20 s deadline; rotate to ONE sibling) | preferredConnectionFailover.ts; chat.ts:2507 | Opt-in (default off); 20 s default deadline; first streaming attempt with no first byte rotates to ONE immediate sibling; disarmed by first byte; config surface routing.preferred_failover_after_ms (M4.9) | FULL | internal/router/preferred.go:44 | TestPreferredFailoverRotatesToSibling, TestPreferredFailoverOffByDefault, TestPreferredFailoverDisarmedByFirstByte, TestPreferredFailoverOnlyOneSibling |
| Deadline-failure sub-budget ending combo with clean 504 | comboPredicates.ts:189-226; executeTargetAttempt.ts:1344-1438 | DeadlineBudget → clean 504 (withDeadline, router.go:2215; ErrDeadlineExceeded → 504, server.go:6590) + readiness-deadline sub-budget counting live (deadlineSubBudgetHit, deadline_budget.go; called in the chat loop router.go:5157; default max 3, DefaultDeadlineFailureMax). Scope: counting lives in the sequential chat path only; hedged first attempts and the stream loop are documented follow-ups. | FULL | internal/router/router.go:2215,5157; internal/router/deadline_budget.go; internal/server/server.go:6590 | TestDeadlineBudgetReturnsDeadlineExceeded, TestDeadlineBudgetStopsChain, TestDeadlineExceededMapsTo504, TestDeadlineBudgetStream, TestG11DeadlineSubBudgetExhaustsTo504; V: pass 2026-11-20 12:20 IST (PC cold + toggle-check) |
| Per-combo deadline budget → clean 504 (comboTimeoutMs, 0 = disabled) | comboConfig.ts:183-195; comboAttemptLoop.ts:515-525,606 | DeadlineBudget → ErrDeadlineExceeded → 504; 0 = disabled | FULL | internal/router/router.go:443-449; internal/server/server.go:5134 | UNVERIFIED |
| Per-target timeout (default 120 s) | comboConfig.ts:32 | Config.PerTargetTimeout, default 120 s, negative disables; Chat attempts run under the child ctx; stream attempts bounded by preread (5 s) + watchdog (~65 s); config surface routing.per_target_timeout_ms (M4.9) | FULL | internal/router/pertarget.go:55-67 | TestPerTargetTimeoutFailsOver, TestPerTargetTimeoutDefaults, TestPerTargetTimeoutRequestDeadlineWins |
| Per-request attempt cap (maxGlobalAttempts default 30, clamped [1,200]) | comboPredicates.ts:132-137,176-186 | MaxAttempts clamped [1,200] in withDefaults; config surface routing.max_attempts (M4.9, validated 1–200). Deliberate default difference: ours defaults to 5, OmniRoute to 30 | FULL | internal/router/router.go withDefaults; internal/config/config.go MaxAttempts | TestMaxAttemptsClamp, TestConfigM49Validation |
| Identical-error early stop (3 consecutive identical model-shape 400s) | comboPredicates.ts:147 | identicalErrorTracker: 3 consecutive identical model-shape 400s (exact string equality, status 400 + ReasonModelNotFound) stop the chain in Chat + stream loops | FULL | internal/router/identical_error.go:35-55 | TestIdenticalErrorEarlyStop, TestIdenticalErrorDifferentMessagesKeepGoing, TestIdenticalErrorStreakResets, TestIdenticalErrorStream |
| Exhausted-provider / exhausted-connection skip for rest of request | comboPredicates.ts:100-128 | breakerAllow at the loop top skips later attempts of a tripped provider (consecutive breaker, window breaker, hard cooldown) for the rest of the request — the breaker IS the exhausted-provider signal | FULL | internal/router/router.go breakerAllow | TestExhaustedProviderSkippedForRestOfRequest |
| No failover after first streamed byte | Failover doc | Once first content byte is sent, stream NEVER switches | FULL | internal/router/router.go:3502-3530 | UNVERIFIED |
| Default cross-provider policy: up to 3 provider attempts; prefixed provider/model never rerouted | docs/OMNIROUTE_PROVIDER_FAILOVER.md | 3-provider default live (cap proven by toggle: 5 attempts with cap off, want 3); engine-level only-narrowing proven (TestG11PrefixedProviderNeverRerouted). Prefixed-model routing FIXED (23b4189): applyPrefixedModelRouting rewrites req.Model to the bare id on both success paths; resolveProviderV1 accepts a provider NAME (ID first, then Name fallback: live registry → config → catalog, exact then case-insensitive); List() added to ProviderKeyManager. V-B2-600-RE pass 2026-11-20 ~13:55 IST — cold PC 3/3 PASS (TestG11PrefixedProviderNeverRerouted, TestG11ProviderAttemptAllowedUnit, TestG11MaxThreeDistinctProviders); LIVE on PC (real server + fake upstreams): prov_<id>/gate-model → 200 FAKE-OK, fakeprov/gate-model → 200 FAKE-OK, upstream receives the BARE id, dead-prefixed → 502 with NO failover (otherprov never called), bare → 200, unknown/auto → 502 fail-open (not pin-400); toggle: bare-id rewrite reverted → probe A 400 "routing.only: no listed provider serves model" (exact pre-fix failure), restored → 200 FAKE-OK. | FULL | internal/router/router.go; cmd/codvian-router/main.go | TestG11MaxThreeDistinctProviders, TestG11PrefixedProviderNeverRerouted, TestG11ProviderAttemptAllowedUnit; V: pass 2026-11-20 ~13:55 IST |
| Fallback-attempts response header (x-omniroute-fallback-attempts) | Failover doc | X-Codvian-Fallback-Attempts + X-Codvian-Fallback-Path on every response via the single choke-point writer (M2.1) | FULL | internal/server/headers.go:74-75,360 | TestResponseHeadersChatNonStream |
| Failover-before-retry + retry knobs | comboConfig.ts:113,164-178 | Failover-first + retry knobs live: MaxRetries / RetryDelayMs / MaxSetRetries / FailoverBeforeRetry in config (config.go:1403-1421, validated), wired in main.go:3446-3455, consumed in the failover loop (router.go:4392, 5246; failover_policy.go transientRetryAllowed/failoverMaxRetries). Defaults: MaxRetries 3, RetryDelay 200 ms (deliberate divergence from OmniRoute's 2000 ms combo default — a 2 s floor would make the failover-first path pointless). | FULL | internal/router/router.go:4392,5246; internal/router/failover_policy.go; internal/config/config.go:1403-1421; cmd/codvian-router/main.go:3446-3455 | TestG11FailoverMaxRetriesRespected, TestG11FailoverMaxSetRetriesCapsSet, TestG11FailoverRetryDelayHonoured; V: pass 2026-11-20 12:20 IST (PC cold + toggle-check) |
| Declarative per-model fallback chains API (GET/POST/DELETE /api/fallback/chains) | src/domain/fallbackPolicy.ts | Runtime CRUD API live through mux: GET/POST/DELETE /api/fallback/chains, admin gateway key, 401/201/200/404/400 verified; engine store round-trips via FallbackChainFor. CAVEAT (V-16): live chat routing (chainFor, router.go:6687) does NOT yet consult stored chains — separate follow-up, precedence vs cfg.ChainFor TBD. | FULL | internal/server/fallback_chains_api.go:110,129,156,192; internal/router/fallback_chains.go:54,83,97,111,126; internal/server/server.go:1523-1525 | TestG11FallbackChainsAPIThroughMux, TestG11FallbackChainsAPIRoundTrip, TestG11FallbackChainsAPIWithoutStore — V-16: pass 2026-11-19T22:02Z |
| Token-refresh breaker (provider-wide, 5 failures, 30 min cooldown) | settings.ts tokenRefreshBreaker | Vertex OAuth2 mint breaker: 5 consecutive mint failures → 30 min cooldown per service-account key; a live cached token still serves during cooldown; minting only is gated | FULL | internal/provider/vertex_client.go:89-140 | TestVertexMintBreakerTripsAfterFiveFailures, TestVertexMintBreakerCooldownExpires, TestVertexMintBreakerSuccessResets |
| Predictive TTFT breaker (skip target whose avg latency exceeds predictiveTtftMs; min 5 samples) | comboConfig.ts:183; comboPredicates.ts:228-244 | ttftSlow: avg TTFT over a 20-sample ring exceeding PredictiveTtftMs (min 5 samples) → skip while another target remains; lone slow provider still serves; config surface routing.predictive_ttft_ms (M4.9) | FULL | internal/router/ttft.go:58-86 | TestPredictiveTTFTSkipsSlowTarget, TestPredictiveTTFTNeedsFiveSamples, TestPredictiveTTFTLoneSlowProviderServes, TestPredictiveTTFTRecovery |
| Admission queue for combos (queueDepth 20, queueTimeoutMs 120 s, maxQueueDepth 100) | comboConfig.ts:100-113 | None — Gradient2 adaptive limiter sheds immediately instead of queueing (DELIBERATE): queueing requests the CPU cannot serve only delays the 502; E1 inconclusive on queueing as a fix | DIVERGENCE | docs/BENCHMARK.md (items 149–150) | — |
| Measured failover latency published | None found (grep failover README.md = 0) | Measured, localhost mocks: 429 failover 52.4–55.8 ms (~1–2 ms router overhead vs 51.2 ms direct); hedge 203.9–204.4 ms vs 2,000 ms slow provider | AHEAD | docs/INSTALL.md:137 | TestChatCompletionsSameRequestFailoverDeadModel |
| Preferred / reserve accounts (preferred first, reserve held back) | Multi-key pooling with preferred/reserve accounts | Provider.Keys roles preferred/reserve; preferred first, reserve sorted last, never hedge targets | FULL | internal/router/router.go | UNVERIFIED |

### Area 3 — Providers / models catalog (20 total: FULL 11, PARTIAL 6, MISSING 2, WRONG 0, AHEAD 1; comparable 19; strict 57.9%, weighted 73.7%)

Counts (fresh): OmniRoute 394 providers (281/282 with baseUrl; 110 no executor entry; 160
hasFree; 129 key-creation hint URLs; static models 1,628 pairs / 1,076 distinct IDs).
Codvian 610 providers (379 with base URL, 231 without = 37.9%; 65 docs-verified; 541
reference-only; 4 unverified; 162 HasFreeTier; 38 ToSGrey; 29 providers OmniRoute lacks;
all 394 OmniRoute IDs present).

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Total provider count | 394 distinct id: fields; derived-facts.json | 610 merged; all 394 OmniRoute IDs present | FULL — AHEAD | internal/catalog/catalog.go:402 All() | UNVERIFIED |
| Providers with a routable base URL | 281/282 registry entries have baseUrl(s) | 379/610 (62%) have BaseURLs; 16 templates/loopback; 231 cannot be added | PARTIAL | internal/catalog/catalog.go | UNVERIFIED |
| Verification labelling | None — no verification status field | 3 statuses; 65 docs / 541 reference / 4 unverified; docs⇒URL enforced | FULL — AHEAD | internal/catalog/verified.go | TestVerifiedEntriesHaveBaseURLs |
| Category/family coverage | 10 families | All 10 families present; api-key 476 vs their 262 | FULL | internal/catalog/catalog.go | UNVERIFIED |
| Static per-provider model list | Registry models: [] per provider; 1,628 pairs / 1,076 distinct (static) | No static list; models only from live /v1/models probe at add | PARTIAL | internal/catalog/catalog.go:236 (no Models field); internal/server/providers_add.go:83-90 | UNVERIFIED |
| Live model discovery | liveCatalogAuthoritative (default true); passthroughModels on 116 entries | Live /v1/models probe at add; GET /v1/models, POST /dashboard/models/refresh | FULL | internal/server/models.go:243 | UNVERIFIED |
| models.dev sync | modelsDevSync.ts; OFF by default; 86400 s interval; + LiteLLM sync | OFF by default; syncs prices + context windows only, operator values always win | PARTIAL | internal/modelsync/modelsync.go:1-27 | UNVERIFIED |
| Add provider by catalog ID | Dashboard wizard + API | POST /api/providers {catalog_id, api_key, …} → AddCatalogWithOptions, live key test, 201, no restart | FULL | internal/server/providers_add.go:1-24; guard.go:133 | UNVERIFIED |
| Fully-custom provider (any base URL) | Prefix convention openai-compatible-, anthropic-compatible-, anthropic-compatible-cc- | POST /api/providers {base_url, …} → AddContext; concrete http(s) URL required, templates refused | FULL | internal/catalog/custom.go:18-40 | UNVERIFIED |
| Custom-provider prefix convention | The 3 prefixes above | No openai-compatible-* prefix convention; generic custom only | PARTIAL | internal/catalog/custom.go | UNVERIFIED |
| Provider DELETE | DELETE /api/providers/[id] — deletes a connection | DELETE /api/providers/{id} — whole-provider removal + config prune + typed DELETE confirmation | FULL — AHEAD | internal/server/providers_delete.go; guard.go:134 | UNVERIFIED |
| ToS-grey catalogued + refused | 37 web-cookie + 50 subscriptionRisk; 12 in free-budget avoid list | 38 flagged ToSGrey; refused at add/load/resolve; included + flagged in GET /api/catalog | FULL | internal/provider/registry.go:656,773,1683 | UNVERIFIED |
| Free-tier marking | hasFree: true ×160; free-tier budget 465 entries / 34 pools / 51 free-forever | HasFreeTier ×162; flag stored but surfaced nowhere — zero references in server/dashboard | PARTIAL | internal/catalog/catalog.go | UNVERIFIED |
| Free-tier budget / pools / rankings | As above, plus Radar catalog | No pools, no budget aggregation, no rankings; only per-provider flags + notes | MISSING | — | UNVERIFIED |
| Key-creation links | 129 hint fields contain a key-creation URL; website on 381 entries | None: no field, no API exposure, no dashboard link | MISSING | — | UNVERIFIED |
| Catalog freshness / audit | Catalog is TS code, updated by releases; models.dev sync; no in-repo audit tool found | cataudit CLI (audit-only); dashboard catalog-audit card; triage 228/228 complete | FULL — AHEAD | internal/cataudit/cataudit.go:1-17; internal/server/catalog_audit.go | UNVERIFIED |
| Catalog visible as addable list in dashboard | /dashboard/providers grouped sections incl. "Free Tier Providers" | Catalog section + add-provider selector fed live from GET /api/catalog | FULL | web/index.html:951-965,1002-1012 | UNVERIFIED |
| Providers the other lacks | — | 29 additional providers, 0 overlap with OmniRoute (verified by set difference) | AHEAD | internal/catalog/providers_additional.go | UNVERIFIED |
| Deprecated / retired handling | providerRetirement.ts + REMOVED_PROVIDERS.md (5 removals) + blocklist test; deprecated ×4 | DeprecatedModels seed table, 16 entries (all OpenAI); /v1/models hides them by default; no provider-removal blocklist / takedown process | PARTIAL | internal/catalog/deprecated.go | UNVERIFIED |
| Verified-overlay data preservation | n/a | Overlay now calls mergeVerified: reference HasFreeTier and FreeTierNote survive for all 11 providers (cerebras, cloudflare-ai, deepseek, fireworks, gemini, groq, mistral, nvidia, ollama-cloud, openrouter, vertex); a verified entry can only add, never drop, the reference free-tier flag. V: pass 2026-11-20 00:32 IST (V-8; toggle-check proven) | FULL | internal/catalog/catalog.go:591,643-692 | TestVerifiedOverlayPreservesReferenceFreeTier |

### Area 4 — Auth, keys, scopes, route guards (32 total: FULL 19, PARTIAL 2, MISSING 2, WRONG 0, AHEAD 9; comparable 23; strict 82.6%, weighted 87.0%)

Guard table: 89 rows — Tier 1: 9, Tier 2: 14, Tier 3: 66. 54 routes AuthExempt,
including 8 POSTs that mutate live state.

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Auth ON by default on /v1 | Keyless by default (REQUIRE_API_KEY unset → disabled) | ON, fail-closed: requireAuth rejects with no verifier/empty header | AHEAD | internal/server/server.go:1269-1290 | UNVERIFIED |
| Default credentials | INITIAL_PASSWORD=CHANGEME in .env.example:31, publicly known | No default password or key anywhere in code | AHEAD | — | UNVERIFIED |
| Bootstrap / first-key setup | Fresh-install bootstrap token only (bootstrapToken.ts:13) | POST /admin/bootstrap, Tier 1 loopback-only, works exactly once, second call 409 | AHEAD | internal/server/guard.go:452-476; internal/server/danger.go:131,174 | UNVERIFIED |
| Key issuance format | key_hash stored; regenerateApiKey (apiKeys.ts:940) | crk_ + 32 crypto-random chars, SHA-256 hash only, raw shown once, constant-time verify | FULL | internal/config/gatewaykey.go:57-60,86-139,125-135 | UNVERIFIED |
| Key rotation | regenerateApiKey (apiKeys.ts:940-965) | POST /admin/keys/{id}/rotate (Tier 2), old key revoked at rotation, new key returned once, persist-failure rollback | FULL | internal/server/keystatus.go:338-443 | UNVERIFIED |
| Key expiry | expiresAt field, checked in usability (apiKeys.ts:631) | ExpiresAt enforced at single auth funnel, 401 names the expiry time | FULL | internal/config/gatewaykey.go:36; internal/server/server.go:184-200,1300-1320 | UNVERIFIED |
| Single-key disable / revoke | isActive, revokedAt, isBanned (apiKeys.ts:128,135,139) | DELETE /admin/keys/{id} with typed phrase + audit; GatewayKey.Disabled + VerifyGatewayKeyDisabled; disabled key → 401 immediately (Wave 1, M1.3) | FULL | internal/config/gatewaykey.go; internal/server/keystatus.go | TestKeyDisableImmediate401, TestKeyDeleteRevokesPermanently |
| Revoke-all | Per-key revoke only (no single revoke-all op verified) | POST /admin/keys/revoke-all (Tier 2, typed phrase REVOKE ALL, caller's own key revoked too) | FULL | internal/server/danger.go:105-137 | UNVERIFIED |
| Scopes stored | scopes: string[] on key metadata + row + SQL (apiKeys.ts:138,256,516) | GatewayKey.Scopes []string; surfaced in me/status + dashboard keys feed | FULL | internal/config/gatewaykey.go:37 | UNVERIFIED |
| Scopes enforced on /v1 routes | keyScopedRoute.ts (enforceApiKeyPolicyWithScope) | HasScope wired via requireScope on every /v1 route; table-driven test over all 90 guard-table routes (Wave 1, M1.1) | FULL | internal/server/server.go (requireScope); internal/config/gatewaykey.go:141 | TestScopeEnforcedTableDriven |
| Scopes enforced on management / destructive routes | MANAGEMENT_API_KEY_SCOPES {manage, admin}; hasManageScope gates management routes | "admin" scope on all Tier 2, /api mutating, dashboard POSTs, /admin/status; "admin" implies all scopes (Wave 1, M1.1) | FULL | internal/server/guard.go; internal/server/server.go | TestScopeEnforcedTableDriven |
| MCP scope enforcement | Opt-in, default OFF (scopeEnforcement.ts:113) | PARTIAL: MCP is stdio-only, 3 read-only tools, no spend path — but also no auth/scoping at all | PARTIAL | internal/mcp/mcp.go | UNVERIFIED |
| Scope-hierarchy coherence (manage ⇒ tools) | WRONG on their side: manage-scope paradox (scopeMatches has no hierarchy) | No equivalent surface yet, so paradox cannot occur — but no hierarchy design exists either | AHEAD (by absence) | — | UNVERIFIED |
| Per-key policy fields | ~40 fields on ApiKeyMetadata (apiKeys.ts:108-157) | FULL — expiry, RPM/TPM, per-key USD budget, per-key guardrails allowlist, IP allowlist/blocklist, endpoint allowlist, model allowlist all enforced on every authenticated route (spend and bare-read alike) via requireKeyPolicy (G21 hook, B9); no-key/relaxed requests pass untouched | FULL | internal/config/gatewaykey.go:36-40,141-147,466-486,489-530; internal/server/server.go:1767-1840; internal/server/admin_keys_api.go:274 | TestB9BareReadIPPolicyEnforced, TestB9BareReadEndpointPolicyEnforced, TestB9BareReadBatchListIPPolicyEnforced (V-12 PASS 2026-11-20 01:43 IST) |
| Per-key IP allowlist / blocklist | ipAllowlist field (apiKeys.ts:137); enforcement not re-verified | FULL — ip_allowlist/ip_blocklist fields on GatewayKey (exact IPs or CIDR), AllowedIP enforced on every authenticated route via requireKeyPolicy (B9) and on spend paths via checkKeyPolicy; blocklist wins, empty lists pass | FULL | internal/config/gatewaykey.go:141-147,466-486; internal/server/server.go:1767-1840 | TestB9BareReadIPPolicyEnforced, TestB9BareReadBatchListIPPolicyEnforced, TestB9BareReadAllowedIPPasses (V-12 PASS 2026-11-20 01:43 IST) |
| Per-key device tracking | Prior-report only: dashboard shows "{count} device(s)" — code not re-verified | MISSING: no device/IP/User-Agent tracking on keys | MISSING | web/index.html (keys comment) | UNVERIFIED |
| Key policy templates / key import | Prior-report only (deep-study §1.7) — code not re-verified | Built-in key policy templates (read-only, standard, admin) served by GET /admin/key-templates (guard.go:430, admin scope) and applied at creation via POST /admin/keys "template" field (resolve(), admin_keys_api.go:150-175); gateway-key import live via POST /admin/keys/import (guard.go:418, TierAlwaysProtected, admin scope) — stores the externally-issued key under its hash, raw key never logged or returned, idempotent retry 200, disagreeing retry 409. Row's "no template or gateway-key import" is stale — both built and tested. Note: row's file:line guard.go:167 is stale (guard table now 224 routes; the routes are at :418 and :430). | FULL | internal/server/admin_keys_api.go; internal/server/guard.go:418,430 | TestKeyTemplatesList, TestKeyImportRoundTrip — V: pass 2026-11-20 ~12:36 IST (PC cold 2/2 PASS + toggle-check) |
| Per-key ban / disable flag | isActive + isBanned (see row 7) | GatewayKey.Disabled; disable → 401 naming disabled; DELETE revokes permanently (Wave 1, M1.3) | FULL | internal/config/gatewaykey.go; internal/server/keystatus.go | TestGatewayKeyDisabled, TestKeyDisableImmediate401 |
| Route-guard tiers (3-tier model) | routeGuard.ts, 478 lines (counts sampled, not fully counted) | FULL: 89-row table, panic at startup on any unclassified route; live table at GET /dashboard/guard | FULL | internal/server/guard.go:80-174,318-330 | UNVERIFIED |
| Guard-before-auth, by real peer | Host-header based loopback check (routeGuard.ts:301) + LAN carve-out | FULL: Tier 1 checked before auth by real TCP peer (RemoteAddr only; Host/XFF never consulted) | AHEAD | internal/server/guard.go:218-228 | UNVERIFIED |
| Manage-scope bypass of LOCAL_ONLY | LOCAL_ONLY_MANAGE_SCOPE_BYPASS_PREFIXES = ["/api/mcp/"] (routeGuard.ts:175) | AHEAD (stricter): no bypass exists | AHEAD | internal/server/guard.go:8-33 | UNVERIFIED |
| Dashboard authentication | Password login (default CHANGEME), optional OIDC, requireLogin default true | FULL: Argon2id password (0600 hash), 12h HttpOnly session cookie, 5-failure/60s throttle; POST /dashboard/auth/setup\|login\|change\|logout + requireDashboardSession feed gate (guard.go:633-644); no default password by design (CHANGEME is must-never-copy; setup needs an admin gateway key); no OIDC (row lists it "optional"); feeds open until a password is set, then gated | FULL | internal/server/dashauth.go; internal/server/guard.go:633-644 | TestDashAuthSetupFirstPassword, TestDashAuthSetupPersistsArgon2idHash0600, TestDashAuthLoginSuccessWrongPasswordAndCookie, TestDashAuthLoginThrottleLocksOutAfterFiveFailures, TestDashAuthLogoutInvalidatesSession, TestDashAuthChangePassword, TestDashAuthFeedsGatedOncePasswordSet, TestDashAuthLoginWithoutPasswordSet, TestDashAuthSessionExpires (9/9 cold on PC, V-B3-669; toggle gate-off → 3 FAIL, restore → 9/9) |
| /metrics auth | Not verified for OmniRoute this pass | FULL: /metrics is Tier 3 non-exempt — gateway-key auth required; nil recorder → 503 | FULL | internal/server/guard.go:139; internal/server/server.go:1218,1648-1650 | UNVERIFIED |
| Dashboard mutating POSTs auth | Management routes use requireManagementAuth | AuthExempt removed from exactly the 8 POST /dashboard/* routes; remaining 46 exempt routes all GET, read-only (Wave 1, M1.2) | FULL | internal/server/guard.go | TestMutatingDashboardPostsRequireAuth, TestExemptRoutesAreReadOnlyGet |
| Keyless postures (/v1, A2A, MCP stdio) | WRONG: /v1 keyless by default; A2A keyless by default (GHSA-v54m-6rm3-p565); MCP stdio gives all 110 tools to any local process | FULL: no keyless posture anywhere; MCP stdio is 3 read-only tools; A2A does not exist | AHEAD | internal/server/server.go | UNVERIFIED |
| Non-loopback bind guard | Warn-only, never blocks boot (nonLoopbackApiKeyGuard.ts) | FULL: only 127.0.0.1/::1/0.0.0.0 accepted; other host is a loud startup error; auth stays ON | AHEAD | cmd/codvian-router/main.go:889-894 | UNVERIFIED |
| Encryption at rest | AES-256-GCM (encryption.ts:28-30, prior report) | FULL: AES-256-GCM via Argon2id-derived key, master.key 0600 | FULL | internal/crypto | UNVERIFIED |
| Per-key guardrails allowlist | MISSING: no per-key guardrail scoping in their key policy | FULL: per-key Guardrails allowlist, unknown names fail loudly, enforced per request | AHEAD | internal/config/gatewaykey.go:41-55; internal/server/guardrail_scope_277.go | UNVERIFIED |
| Self-service key introspection | GET /v1/me/status + GET /v1/me/sessions | PARTIAL: GET /v1/me/status is FULL and richer on budget/traffic; GET /v1/me/sessions EXISTS (list + detail, guard.go:368, mesessions.go:51, quotas.go:59) and is tested (TestMeSessions* 4/4 PASS cold on PC), but serves from the in-memory Responses store, not Ledger.AgentSessions — AK-3 unwired (AgentSessions has zero production callers), so the ledger-backed session rollup is not what the endpoint returns. Toggle-proven: handler forced empty → TestMeSessions FAILs. V: pass 2026-11-20 12:35 IST | PARTIAL | internal/server/keystatus.go:57-143 | TestMeSessions, TestMeSessionsEmpty, TestMeSessionsRequiresKey, TestMeSessionsRouteExists |
| Key creation via API / dashboard | Dashboard "Create API Key" flow + API creation | MISSING: no POST /admin/keys create route; dashboard "+ New router key" button explicitly unwired | MISSING | web/index.html:5676-5680 | UNVERIFIED |
| OAuth / JWT gateway auth | OAuth 2.0 PKCE + JWT + 32 OAuth providers | FULL: OAuth 2.0 PKCE (S256, RFC 7636 vector) + HS256 JWT gateway auth — authorize/code/token/refresh/revoke funnel live; JWT Bearer gates /v1/chat/completions and /v1/models with scope enforcement; temp e2e funnel PASS cold on PC (client create 201 → authorize 302 → token 200 → chat 200 via engine, refresh rotates, revoke 401s, tampered JWT 401s); toggle wrong-HMAC-key → 7 oauth FAILs + e2e FAIL, restore hash-matched. V: pass 2026-11-20 ~15:42 IST. Note: create-client response names the client "id", not "client_id" (follow-up); routed request used the in-process fake engine (standard test bar), not a live upstream. | FULL | internal/oauth/jwt.go, internal/oauth/oauth.go; internal/server/oauth_api.go, internal/server/server.go:1976,2025,2070,2111; internal/server/guard.go:410-429 | TestSignJWTVerifyRoundTrip, TestVerifyJWT*, TestIssuerAuthorizeExchangeVerifyRoundTrip, TestIssuerRefreshRotates, TestIssuerRevokeAccessToken, TestOAuthJWTEndToEndFunnelVerify (temp, PC) |
| Per-key schedule / sessions / throttle | accessSchedule, maxSessions, throttleDelayMs, maxRequestsPerDay/Minute, rateLimits[] (apiKeys.ts:126-134) | FULL — accessSchedule enforced first (403), throttle delay, max-sessions cap; RPD + generic {limit, window} rules alongside RPM/TPM (Area 5 row, same feature — see there for the full evidence) | FULL | internal/server/keyaccess.go:42; internal/server/keytraffic.go:213 | TestKeyAccessScheduleGateBlocksChat, TestKeyTrafficLimiterRPD |

### Area 5 — Budgets, rate limits, quota (30 total: FULL 27, PARTIAL 0, MISSING 0, WRONG 0, AHEAD 2, DIVERGENCE 1; strict 100.0%, weighted 100.0% — comparable 27)

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Per-key rolling 5h USD budget | fiveHourUsageLimitUsd (apiKeys.ts:151); apiKeyUsageLimits.ts:646-648,611-620 | FULL — RollingWindow = 5h, enforced pre-call, 402 over-cap, fail_closed unpriced → 400, NextRolloff honest reset time | FULL | internal/ledger/keybudget.go:44-46,226,350,476; internal/server/keybudgets.go | UNVERIFIED |
| Per-key daily USD budget | dailyUsageLimitUsd (apiKeys.ts:149) | FULL — Calendar daily window (UTC, resetTime) enforced pre-call via the active window; fallback to daily when weekly/monthly unset; spend from usage_events | FULL | internal/ledger/budgetwindow.go:152; internal/ledger/keybudget.go:339 | TestKeyBudgetWindowedEnforcement, TestKeyBudgetActiveWindowFallback, TestKeyBudgetResetTimeShiftsWindow |
| Per-key weekly USD budget | weeklyUsageLimitUsd (apiKeys.ts:150) | FULL — Calendar weekly window (Monday resetTime to next Monday) enforced pre-call via the active window; fallback to daily when unset | FULL | internal/ledger/budgetwindow.go:152; internal/ledger/keybudget.go:339 | TestKeyBudgetWindowedEnforcement, TestKeyBudgetActiveWindowFallback |
| Per-key monthly USD budget | PARTIAL on their side — only via cost-rules /api/usage/budget (monthlyLimitUsd) | FULL — Calendar monthly window (1st resetTime to next 1st) enforced pre-call; monthly cap set + unpriced usage in the month + fail_closed policy refuses ALL models until month end | FULL | internal/ledger/budgetwindow.go:152; internal/ledger/keybudget.go:339 | TestKeyBudgetWindowedEnforcement, TestKeyBudgetMonthClosedByUnpriced |
| Global / cumulative USD cap ("never bill me") | budgetGuard.ts:1-60 (global\|provider\|model\|pool, daily/weekly/monthly) | FULL — Config.BudgetUSD + FailClosed (default true); CheckBudgetOrError, 402 refusal, 80% warn; cumulative with manual reset, no calendar auto-reset; race-proof via committed-spend reservations (M5.14: pre-send checks count reservations, ReserveSpend enforces the cap atomically) | FULL | internal/config/config.go:67-68,793-794; internal/ledger/budget.go | UNVERIFIED |
| Per-key token limits, scoped model/provider/global | Per-(key,model) caps, hourly, 429 model-cap (quota/enforce.ts:142-186) | FULL — Scoped model/provider/global limits with resetInterval/resetTime; ledger-seeded counters; most-restrictive breach wins; engine skips breached targets per attempt, server pre-check covers global+model (provider unknown pre-call) | FULL | internal/ledger/tokenlimit.go:71; internal/router/tokenlimit.go | TestCheckKeyTokenLimitsScopes, TestCheckKeyTokenLimitsMostRestrictive, TestEngineTokenLimitSkipsBreachedTarget |
| Per-key request limits (RPM rules + RPD) | maxRequestsPerMinute, maxRequestsPerDay (apiKeys.ts:140-141); rateLimits[] | FULL — RPM + RPD (UTC calendar day) + generic {limit, window} sliding rules; first hit wins; honest 429 + Retry-After | FULL | internal/server/keytraffic.go:213 | TestKeyTrafficLimiterRPD, TestKeyTrafficLimiterGenericRules, TestKeyTrafficLimiterParallelRace |
| Per-key TPM (tokens/min) | Per-connection rpm/tpm/tpd overrides (rateLimitManager.ts:92) | FULL — Sliding 60 s prompt-token window at admission + post-response true-up to provider-reported prompt tokens; in-memory (resets on restart, local-first design) | FULL | internal/server/keytraffic.go:213 | TestKeyTrafficLimiterTrueUp, TestKeyTrafficLimiterParallelRace |
| Per-key RPD (requests/day) | maxRequestsPerDay (apiKeys.ts:140) | FULL — UTC calendar-day request cap; 429 carries Retry-After to midnight; resets at the day boundary | FULL | internal/server/keytraffic.go:213 | TestKeyTrafficLimiterRPD |
| Custom windows / resetInterval + resetTime | /api/usage/budget resetInterval daily\|weekly\|monthly + resetTime HH:MM | FULL — resetInterval daily/weekly/monthly + resetTime HH:MM on key budgets and token limits; weekly Monday, monthly 1st, all UTC | FULL | internal/ledger/budgetwindow.go:152 | TestBudgetWindowFor, TestNormalizeBudgetResetTime, TestKeyBudgetResetTimeShiftsWindow |
| Warning thresholds | Configurable warningThreshold 0–1 in /api/usage/budget | FULL — warningThreshold operator-configurable 0–1 (default 0.8) on key budgets and internal budgets, surfaced via X-Codvian-Budget-Warning; fixed 80% SoftWarnThreshold on the cumulative cap | FULL | internal/ledger/budget.go:20; internal/server/server.go:3998 | UNVERIFIED |
| Per-request budget (client-supplied cap) | No equivalent surface verified | AHEAD — X-Codvian-Budget + -Strict + -Spent; pre-send 402, strict mid-chain gate, overage recorded; GET /dashboard/request-budgets | AHEAD | internal/server/requestbudget.go:73-74,86,226,335 | UNVERIFIED |
| Quota-share pools | fairShare.ts (186 lines); enforce.ts (611 lines); dashboard /dashboard/costs/quota-share | FULL — Pool members (keys) with weight/cap/policy (hard/soft/burst), saturation threshold, absolute cap always enforced; pre-request gate; per-account limit x N backing accounts as the effective limit | FULL | internal/ledger/pool.go:198 | TestCheckPoolFairShareGenerous, TestCheckPoolFairShareStrict, TestCheckPoolFairSharePerAccountMultiplier, TestQuotaPoolGateBlocksChat |
| Team cost centers + team budgets | team_cost_centers table; teams.ts; teamUsageLimits.ts (1d/7d/30d) | FULL — Teams as key groupings with 1d/7d/30d USD list-price budgets, membership history, fail-closed on unpriced usage; data model + enforcement + API (dashboard UI is M10) | FULL | internal/ledger/team.go:394; internal/server/teams.go | TestTeamBudgetGateBlocksChat, TestTeamBudgetGateUnpricedFailsClosed, TestTeamsDashboardRoundTrip |
| Per-account limits (upstream account quota) | perAccountLimit × N connections (enforce.ts:199); accountQuotas in me/status | FULL — Per-account limit x N backing accounts = pool effective limit; accountQuotas in me/status for the caller's own pools (no self:account-quota scope exists in our code, so the scope gate is not ported — own pools only, no cross-key leakage) | FULL | internal/ledger/pool.go:198; internal/server/keystatus.go | TestCheckPoolFairSharePerAccountMultiplier, TestMeStatusAccountQuotas |
| Upstream rate limiting (RPM/TPM/TPD pacing) | Bottleneck per provider+connection; defaults 60 RPM / 350 ms / 6 concurrent; learns + persists limits from x-ratelimit-*/retry-after | FULL — x-ratelimit-*/retry-after parsed on all 6 wire clients (OpenAI + Anthropic triples), persisted per (provider, key) with 5-min TTL, Gradient2 paced from the learned signal (only ever shrinks the cap; recovery via Record) | FULL | internal/adaptive/ratelimit.go:137; internal/provider/client.go | TestParseRateLimitHeadersOpenAI, TestLimiterApplyLearnedLimitPacesCap, TestChatResponseCarriesRateLimitHeaders, TestEngineLearnsRateLimitFromChatResponse |
| Adaptive concurrency limiting | Static Bottleneck buckets; no gradient adaptation | AHEAD (design) — Gradient2 latency-gradient adaptation, fail-open; no fresh measured limiter numbers in this pass | AHEAD | internal/adaptive/limiter.go | UNVERIFIED |
| Admission queue (queue instead of shedding) | maxWaitMs default 30,000 ms, execution backstop 600,000 ms | DIVERGENCE — Gradient2 sheds immediately; no queue by design (M4.10). A hedged pair's joint prompt estimate is still bounded by the strict request-budget gate (M5.14) | DIVERGENCE | internal/adaptive/limiter.go | UNVERIFIED |
| Over-budget behaviour (refuse vs degrade) | Fail-closed default; unpriced blocks under fail_closed, $0 under count_as_zero | FULL — FailClosed on → 402; off → soft warn; unpriced fail_closed → 400 / count_as_zero — same two policies | FULL | internal/ledger/budget.go:84; internal/ledger/keybudget.go:48-59 | UNVERIFIED |
| Retry-After on budget/rate-limit refusal | 429 carries reset timing / resetAtIso per window | FULL — key-traffic 429 carries Retry-After; 5h budget NextRolloff | FULL | internal/server/keytraffic.go; internal/ledger/keybudget.go:448 | UNVERIFIED |
| Usage reporting: GET /v1/me/status | usage.cost/daily/weekly + limits + accountQuotas (apiKeySelfService.ts:404-435) | FULL — Budget status + traffic + usage windows (cost/daily/weekly with list-price cost + completeness flag) + account_quotas + team section | FULL | internal/server/keystatus.go:249 | TestMeStatusUsageWindows, TestMeStatusAccountQuotas, TestMeStatusTeamSection |
| Usage reporting: GET /v1/me/sessions | Present — per-key agent sessions, project filter, costUsd + unpricedCount | FULL — GET /v1/me/sessions lists the caller's Responses sessions (root + all descendants), list-price cost at current catalog prices + unpriced_count, newest first, cap 100 | FULL | internal/server/mesessions.go:51 | TestMeSessions, TestMeSessionsRequiresKey, TestMeSessionsEmpty |
| Usage/budget management API (/api/usage/budget, /api/usage/om-usage) | GET/POST /api/usage/budget; GET /api/usage/om-usage | FULL — GET/POST /api/usage/budget (admin; summary + checkBudget allow/warn/deny; limitUsd required for a fresh key) + GET /api/usage/om-usage (allow_usage_command flag, JSON or plain text) | FULL | internal/server/usageapi.go:91 | TestUsageBudgetGet, TestUsageBudgetPost, TestOmUsage |
| Per-response cost/token/latency headers | attachOmniRouteMetaHeaders (omnirouteResponseMeta.ts:129-224) | Single choke-point writer emits X-Codvian-Request-Id, -Version, -Provider, -Model, -Cost-USD (10 dp), -Tokens-In/-Out/-Cached, -Latency-Ms, -Upstream-Latency-Ms, -Router-Overhead-Us, -Tokens-Per-Second, -Fallback-Attempts/-Path, -Cache on chat/embeddings/rerank/responses/messages, stream + non-stream + errors (M2.1) | FULL | internal/server/headers.go:360 | TestResponseHeadersChatNonStream, TestResponseHeadersErrors |
| Committed-vs-settled spend | Team level: soft_committed_usage (teamUsageLimits.ts:17,117,215-244) | FULL — Pre-send prompt estimate reserved as committed, settled after the usage row lands; race-proof for per-key AND cumulative caps (M5.14: reservation now covers the cumulative cap, pre-send checks count reservations) | FULL | internal/ledger/reservation.go:57; internal/ledger/budget.go:90 | TestReserveSpendParallel, TestReserveSpendParallelCumulativeCap, TestReserveSpendParallelChat, TestCumulativeBudgetRaceParallelChat |
| Quota schedules (time windows, allow/block, keep-free floor, webhook) | schedules.ts:71-74; enforce.ts:406-411,489-500; quota.exceeded webhook | FULL — Allow/block windows with overnight wrap + IANA tz, keep-free reserve, schedule budget, quota.exceeded webhook (HMAC-signed, async, never blocks); reserves measured on pool consumption vs the effective limit and budgets on window consumption (documented divergences from the upstream-signal measurement) | FULL | internal/ledger/schedule.go:315; internal/webhooks/webhooks.go:58 | TestCheckPoolSchedulesReserveAndBudget, TestQuotaScheduleGateBlackoutBlocksChat, TestQuotaScheduleGateFiresWebhook |
| Per-key disable / revoke | isActive, revokedAt, expiresAt, isBanned (apiKeys.ts:135-141) | Disabled field + single-key DELETE/disable exist; rotation preserves Disabled (Wave 1, M1.3) | FULL | internal/config/gatewaykey.go; internal/server/keystatus.go | TestKeyDisableImmediate401, TestKeyDeleteRevokesPermanently |
| Per-key expiry | expiresAt (apiKeys.ts:141) | FULL — ExpiresAt, expired key → 401 naming the expiry | FULL | internal/config/gatewaykey.go:26,134-139 | UNVERIFIED |
| Per-key access schedule / throttle delay / max sessions | accessSchedule, throttleDelayMs, maxSessions (apiKeys.ts:136-142) — enforcement not traced | FULL — accessSchedule enforced first (403, OmniRoute message), throttle delay per request, max-sessions cap on concurrent in-flight requests; OmniRoute stores throttle/maxSessions without enforcing them, so enforcement is ours (documented) | FULL | internal/server/keyaccess.go:42; internal/server/keysessions.go:58 | TestKeyAccessScheduleGateBlocksChat, TestKeyThrottleDelay, TestKeyMaxSessionsBlocksSecond |
| Internal scoped budgets (global/provider/model/pool × currency/tokens/requests × daily/weekly/monthly) | budgetGuard.ts:1-60 (InternalBudgetLimit) | FULL — Scoped budgets with allow/warn/deny + warningThreshold, enforced server-side (global/model/pool) and per-target in the engine (provider scope); OmniRoute's evaluator has no gateway enforcement wired in | FULL | internal/ledger/internalbudget.go:211 | TestInternalBudgetEvaluate, TestInternalBudgetGateDenyBlocksChat, TestEngineInternalBudgetSkipsBreachedProvider |

Note (Area 5): GetKeyBudget "90µs → 47µs" NOT verified — dropped as comparison evidence.
Measured evidence is GetKeyBudget 11.54% cum → 7.52% cum; added latency 0.908/0.711 ms →
0.842/0.691 ms; RPS 1,451/1,432 — targets still MISSED.

### Area 6 — Caching (35 total: FULL 29, PARTIAL 2, MISSING 0, WRONG 0, AHEAD 3, DIVERGENCE 1; comparable 31; strict 93.5%, weighted 96.8%)

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Exact-match cache | SHA-256 signature, memory LRU + SQLite (semanticCache.ts:380+) | ExactCache, LRU, 1h TTL | FULL | internal/cache/cache.go:99-204; internal/degrade/degrade.go:57 | UNVERIFIED |
| Exact-key contents (gen params in key) | Schema v2 + model + messages + temperature + top_p + tools + 31 generation params (semanticCache.ts:170,196-330) | ScopedKey = SHA-256 of {tenant, model, messages, tools, tool_choice, temp/top_p buckets, max_tokens, stop, response_format, seed, reasoning_effort}, wired into degrade.go/relay.go serving path (Wave 1, M1.4) | FULL | internal/cache/key.go; internal/degrade/degrade.go | TestM14ScopedKeyTenantIsolation, TestW1TwoKeysSamePromptNoCrossHit |
| Temperature gating (read + write) | Strict: explicit numeric temperature: 0 only (semanticCache.ts:578,596) | ShouldStore refuses temp>0 unless explicitly allowed per-key; temperature bucketed in ScopedKey; truncated (finish_reason=length) never stored (Wave 1, M1.4). M6.11: semantic layer enforces TemperatureBucket per entry on read + write (scoped store/lookup, legacy fail-closed) | FULL | internal/cache/key.go; internal/degrade/degrade.go; internal/cache/cache.go | TestM14TemperatureGating, TestW1TemperatureOneNeverStoredOrServed; TestM611TempSemanticCrossTempWithAllowance, TestM611FixTempCrossTemp, TestM611FixScopedTemperatureIsolation |
| Per-key / per-tenant isolation | YES. apiKeyId plaintext-prefixed to signature (semanticCache.ts:304-330) | Tenant in ScopedKey; two keys, same prompt → no cross-hit through the serving path (Wave 1, M1.4). M6.11: semantic layer tenant-scoped (scoped store/lookup, legacy entries fail closed) — the M6.11 property tests prove it end to end | FULL | internal/cache/key.go; internal/degrade/degrade.go; internal/cache/cache.go | TestM14ScopedKeyTenantIsolation, TestW1TwoKeysSamePromptNoCrossHit; TestM611CrossTenantSemanticParaphraseGap, TestM611FixScopedTenantIsolation, TestM611FixCrossTenantParaphraseStream |
| Per-model isolation | YES, default-on cacheByModel: true | YES, exact case-sensitive | FULL | internal/cache/cache.go:1139; internal/degrade/degrade.go:501-568 | UNVERIFIED |
| Per-provider isolation | YES, default-on cacheByProvider: true | DIVERGENCE (deliberate, decided M6.12): same model id shared across providers. Provider is unknown at key-derivation time (lookup runs before routing picks a provider; ChatRequest carries no Provider field; semantic slots carry model only). Recorded as DECISION (M6.12) in internal/cache/key.go | DIVERGENCE | internal/cache/key.go | TestM612SameModelSharedAcrossProviders, TestM612KeyInputHasNoProviderField |
| Semantic similarity method | Real neural embeddings via provider: text-embedding-3-small, 1536-dim, 3 s timeout, fail-open | Neural embeddings via provider (text-embedding-3-small, 1536-dim, 3 s timeout, fail-open to BoW on error/timeout/malformed); persistent store path embeds neurally via embedTextCtx, matching the read path | FULL | internal/cache/semantic_persist.go:532, internal/cache/cache.go:330-360 | TestPersistentSemanticCacheSetTextUsesNeuralEmbedder (11/11 PASS cold on PC; toggle: BoW revert FAILS; V: pass 2026-11-20 13:00 IST) |
| Semantic threshold + override | 0.8 default; per-request x-omniroute-cache-threshold header | 0.85 floor enforced at every layer (config validated [0.85, 1.0], constructor floors sub-0.85, per-call plumbing, per-request header with identical max() composition, persistent-store post-filter narrows identically); serve mode honours the configured floor (M6.7) | FULL | internal/cache/cache.go; internal/config/config.go; internal/degrade/degrade.go; cmd/codvian-router/main.go | TestM67* (16 across cache/config/degrade), TestM67ConstructorFloorsSubFloorThreshold |
| Semantic scope (what text is matched) | Last 3 turns embedded, 50-turn bypass cap, optional system-prompt exclusion | Last 3 non-system turns, single shared helper (internal/provider/semantic_window.go) on engine + relay; >50 raw messages bypass semantic lookup on both paths (OmniRoute-exact); store also skips semantic entries past 50 turns — deliberate extension beyond OmniRoute, recorded; system/developer excluded unconditionally (M6.7, M6.7b, M6.7c) | FULL | internal/provider/semantic_window.go; internal/degrade/degrade.go; internal/server/relay.go | TestM67b* (5), TestM67c* (cap boundary + engine/relay lookup/store) |
| Exact TTL | 30 min (config) vs 1 h (lib default) — they disagree with themselves | 1 h, self-consistent | FULL | internal/degrade/degrade.go:57 | UNVERIFIED |
| Semantic TTL | Same 30 min / 1 h split | 1 h | FULL | internal/cache/cache.go:378 | UNVERIFIED |
| Entry caps | Memory LRU 50; vector store 1000; SQLite unbounded except prune job | Exact 500 in serve mode (1000 default), semantic 500 | FULL | internal/cache/cache.go:78,373 | UNVERIFIED |
| Per-entry size cap | 256 KB (semanticCache.ts:110) | 256 KiB per entry on both layers, oversize refused (M6.1) | FULL | internal/cache/cache.go; internal/cache/caps_test.go | TestExactCachePerEntrySizeCap, TestSemanticCachePerEntrySizeCap |
| Total memory cap | 2 MB memory tier (semanticCache.ts:127) | 2 MiB total on both layers, LRU eviction, Stats() bytes on both layers (M6.1) | FULL | internal/cache/cache.go; internal/cache/caps_test.go | TestExactCacheTotalBytesCap, TestSemanticCacheTotalBytesCap, TestExactCacheStatsAndFlush |
| Persistence tiers | Dual-layer: in-memory LRU + SQLite + optional Redis vector store | SQLite persistence for both layers, write-through, durable flush, final Sync + Close on clean shutdown; exact-only proactive serving on resume paths (4c209cb) + injectReasoning on text ChatStreamFrom. Redis vector store BUILT + wired (9149db0: semantic_redis.go, main.go:3915-3923, config cache.redis_*) and LIVE-PROVEN cold on the PC against a REAL Redis 6.0.16 (WSL, via localhost forwarding 127.0.0.1:6379): TestB2RedisLiveSetGetSyncReopen PASS (open → SetForModel → GetForModel hit → Sync → Close → reopen → hit again, ac25d6c); toggle dead port 6399 → FAILs with connection refused, no silent fail-open. Caveats: direct WSL IP 192.168.64.253:6379 still dead (WSL2 networking dead both ways); native Windows Redis 3.2.100 excluded, product minimum Redis ≥ 4.0. V-B2-738 pass 2026-11-20 ~14:14 IST (PC). | FULL | internal/cache/persist.go; internal/cache/semantic_persist.go; internal/degrade/degrade.go; internal/cache/semantic_redis.go; cmd/codvian-router/main.go:3915 | TestB2RedisLiveSetGetSyncReopen, TestM62* (10), TestM62b* (22) re-run 32/32 PASS cold on PC, TestM66bChatStreamFromServesExactHitTrimmed PASS cold on PC; V: pass 2026-11-20 ~14:14 IST |
| Truncated-response guard | YES. isTruncatedCompletion rejects length/max_tokens + cut tool-call args + status: incomplete | ShouldStore refuses finish_reason=length (wire normalises max_tokens→length); drain fallback + bridge Done event propagate length for text-only clients; relay store gated the same way; all tool-carrying responses refused (stricter than cut-args) (M1.4, M6.3) | FULL | internal/cache/key.go:207; internal/degrade/degrade.go:951; internal/router/router.go:6036,6086; internal/server/relay.go:786,1036 | TestM63TruncatedChatStreamNotStored, TestM63TextOnlyStreamKeepsLengthFinishReason, TestM63AsyncRelayToolRequestNotStored |
| Tool-carrying response guard | Never stored (responseCarriesToolCalls), but tools are in the signature | Tool requests excluded from cache entirely — safer, loses hits | FULL | internal/degrade/degrade.go:355-372 | UNVERIFIED |
| Routing-contract exclusion | No equivalent found | YES — non-empty routing contract never consults/stores | AHEAD | internal/degrade/degrade.go:374,501-568; internal/coalescing/coalescing.go:199-206 | UNVERIFIED |
| Lossy-compression exclusion (semantic) | No equivalent found | YES — lossy-trimmed requests never touch semantic layer | AHEAD | internal/degrade/degrade.go:465-499 | UNVERIFIED |
| Stale / expired serving | NONE (stale appears only in comments) | YES — exact-match only, X-Codvian-Stale + X-Codvian-Stale-Age, gated; never after partial delivery | AHEAD | internal/server/server.go:3524-3525; internal/degrade/degrade.go:69-105,624,784+ | UNVERIFIED |
| Streaming cache | Cached after assembly; hits return JSON | Same: hit replayed as single delta/chunk; streaming stored after assembly | FULL | internal/degrade/degrade.go:784+ | UNVERIFIED |
| Bypass headers | X-OmniRoute-No-Cache, Cache-Control: no-cache, Pragma: no-cache, x-omniroute-cache-no-store | All four honoured on read + write (lookup skipped, store skipped); X-Codvian-No-Cache accepted as an alias of X-OmniRoute-No-Cache (M6.4) | FULL | internal/server/cache_controls.go; internal/degrade/degrade.go; internal/server/server.go | TestM64BypassSkipsLookupAndStore, TestM64RelayLookupHonoursBypass |
| Flush / invalidation | clearCache, invalidateByModel/Signature/Stale; DELETE /api/cache; prune job | POST /admin/cache/flush (admin scope) + FlushTenant/FlushAll wired (Wave 1, M1.5); DELETE /api/cache live (M8.15, wired merge wave) with per-layer eviction counts. LocalSemanticCache.FlushAll() exists (internal/cache/semantic_flush.go:21); PersistentSemanticCache.FlushAll() exists (internal/cache/semantic_persist.go:562) but the persistent-mode adapter (persistentSemanticStore) exposes no FlushAll, and no engine exposes a reasoning flush — both report honest flushed:false (M8.15) | FULL | internal/cache/flush.go; internal/server/guard.go | TestW1CacheFlushEndpoint, TestM15FlushAllAndPerKey, TestM815CacheDeleteFixture, TestM815CacheDeleteExactFlushStoreEngine |
| Cache analytics | cache_metrics table; getCacheStats(); GET /api/cache/stats; /dashboard/cache | GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only); /dashboard/cache page live (M10-10: nav Cache, fetches /api/cache/stats, flush via DELETE /api/cache) — all four sources built and tested (M6.8, M6.2b). Note: no literal getCacheStats() function — the capability is handleCacheStats (internal/server/cache_stats.go), registered guard.go:251 + server.go:1648. | FULL | internal/server/cache_stats.go; internal/cache/persist.go; web/index.html | TestCacheStats* (8), TestM62bCacheStats* (2), TestDashboardM10_10* (6), TestM62b* (10, internal/cache) — V: pass 2026-11-20 12:16 IST (PC cold 26/26 PASS + toggle-check) |
| Provider cache-read accounting | tokens_cache_read columns + analytics | CachedTokens per attempt; dashboard prompt-cache table with cached-read cost | FULL | internal/ledger/ledger.go:144-190; internal/server/server.go:1911-1921 | UNVERIFIED |
| Own-cache savings accounting | Per-entry tokens_saved + cache_metrics totals, surfaced via stats + X-OmniRoute-Cost-Saved | RecordHit called on own-cache hits; tokens + $ saved recorded (Wave 1, M1.5) | FULL | internal/cache/persist.go; internal/degrade/degrade.go | TestM15SavingsRecordedOnHit, TestW1RecordHitOnOwnCacheHit |
| Cache-write tiers (5-min / 1-h) | FULL. Anthropic ephemeral_5m/1h split into tokens_cache_creation_5m/_1h + tier status; costed per tier (1.25× / 2×). Note: their tier estimate is diagnostic-only — "Never used by calculateCost, quotas, budgets or deductions" | Ledger stores + costs per tier (5m 1.25×, 1h 2×, explicit-rate-wins, claude-gated fallback); pipeline live wire → provider → ledger (wire parses usage.cache_creation, CacheWriteTiers() feeds ConvertResponse + TranslateStream); dashboard still estimates write cost as 1.25×–2.5× band (M6.9, M6.9b); no live upstream verification (mock httptest only) | PARTIAL | internal/ledger/ledger.go; internal/wire/anthropic.go; internal/config/prices.go | cache_write_tiers_test.go (config 2 + ledger 8 + wire 5 = 15); V: pass 2026-11-20 ~12:45 IST (PC cold 15/15; toggle: CacheWriteTiers() off → 3 wire tier tests FAIL) |
| Provider prompt-cache markers | cache_control used in executors (base.ts, bedrock.ts, vertex.ts, chatCore.ts) | LIVE on Anthropic only: auto-marker places ephemeral on last-user text block + system + last tool (G13, max 4/request), gated temp 0, no tool calls, ≥1024 est. tokens; client-supplied cache_control preserved verbatim (text + system, G13); image_url parts preserve it too (f790f58) BUT untested — no test sends an image_url part with a client marker (toggle: revert f790f58 → 21/21 PASS). OpenRouter unwired (OpenAI wire has no cache_control path); no TTL set — 1-hour tier never opted into; mock-only, no live upstream verification (M6.9, M6.9c) | PARTIAL | internal/provider/anthropic_client.go; internal/wire/anthropic.go | TestM69c* (13 provider + 8 wire = 21); V: pass 2026-11-20 ~12:45 IST (PC cold 21/21; toggle: auto-marker off → 6 provider tests FAIL; toggle: f790f58 reverted → 21/21 PASS, unpinned) |
| Prompt-cache affinity routing | cache-optimized strategy, orderByCacheAffinity, cacheAffinityOwnsOrdering: true | OrderByCacheAffinity (measured warmth → marker support → fallback score) wired into the live routing path via StrategyHookForRequest + 1-min warmth snapshot over ledger CachedTokenFraction; gate: prompt ≥1024 tokens, no tools, temp 0, no pinned contract; no live upstream call made (M6.5) | FULL | internal/strategy/cache_affinity.go; internal/strategy/wiring.go; internal/router/router.go; internal/ledger/ledger.go; cmd/codvian-router/main.go | TestCacheAffinity* (9), TestM65CacheOptimized* (6), TestCacheWarmthSnapshot* (3) |
| Dedup / coalescing (single-flight) | deduplicate(), temp ≤ 0.1 only, tenantId prefix, max 1,000 in-flight, 60 s timeout, non-streaming only | Group.Do, non-streaming only; tenant-scoped key wired in live Engine.Chat (router.go:5954); max 1,000 in-flight, 60 s flight timeout; temp IS in the key (stricter). Divergences: cap runs newcomer uncoalesced (theirs evicts oldest); timeout fails the flight (theirs only deletes the map entry); tenant hashed (theirs plaintext prefix); ours coalesces any temp (theirs ≤0.1 only) (M6.10) | FULL | internal/coalescing/coalescing.go; internal/router/router.go:5954 | TestGroupDifferentTenantsNeverShare, TestEngineCoalescesIsolatedPerTenant |
| Reasoning cache (2 h) | reasoningCache.ts:37 (2 h TTL) | 2 h TTL, 200 entries, tool-call + conversation keys, full signed state stored/injected (M6.6b shadowing fix), chunk-stream injection, serve-mode wired. DB tier EXISTS + wired (G13: OpenPersistentReasoningCache, SQLite reasoning_cache.db via wireReasoningCacheAtPath, main.go:3998; restart reloads entries; fail-open to memory-only). Budget counts signatures + redacted payloads. Temp-gate divergence is RECORDED (degrade store applies CacheableTemperature; router store below has no gate — mitigated). Text-resume injectReasoning now PINNED (critic 6716036): inject call at degrade.go:2100 in ChatStreamFrom, before the upstream resumer; DB tier toggle also proven (tier off → TestG13ReasoningPersistsAcrossReload FAILS). V-B2-754 pass 2026-11-20 ~13:54 IST — cold PC: named test PASS + full TestM66b file PASS + G13 4/4 PASS; toggles: inject off → named test FAILS, DB tier off → reload test FAILS, restored → PASS. | FULL | internal/cache/reasoning_cache.go; internal/cache/reasoning_persist.go; internal/degrade/degrade.go; cmd/codvian-router/main.go:2051,3998 | TestM66bTextStreamFromInjectsBeforeUpstream, TestM66Reasoning* (10 cache + 9 degrade), TestM66b* (17), TestG13Reasoning* (4); V: pass 2026-11-20 ~13:54 IST |
| Search cache (60 s) | searchCache.ts:12,150 (60 s TTL, with coalescing) | SearchCache[T] wired into the live POST /v1/search path (G13, internal/server/media_verify.go): GetOrCoalesce around the single Perplexity passthrough, 60 s TTL, 500 entries, keyed by SHA-256 of the search parameters; repeated identical search inside the TTL served with zero upstream calls (X-Codvian-Search-Cache: hit), concurrent identical searches share one fetch; errors never stored; ttl<=0 / no-store header bypasses cache AND coalescing. V: pass 2026-11-20 ~12:20 IST (PC cold 16/16 PASS + toggle-check) | FULL | internal/cache/search_cache.go; internal/server/media_verify.go:266 | TestM66Search* (11), TestG13SearchCache* (5) — V: pass 2026-11-20 ~12:20 IST (PC cold run + toggle-check: cache bypassed → HitSecondCallFree FAILs, 2 upstream calls want 1; restored, test PASSes again) |
| Per-request cache controls | x-omniroute-cache-key, -type, -threshold (floor), -ttl (capped), -no-store | All five honoured under X-Codvian-* spellings (key override tenant-scoped, type exact/semantic/none, threshold floor max-only-tightens, TTL capped, no-store); NOT an extension — every header exists in OmniRoute (semanticCacheManager.ts:300,302,307,425,460); x-omniroute-* spellings not accepted, except X-OmniRoute-No-Cache for bypass (M6.4) | FULL | internal/server/cache_controls.go; internal/degrade/degrade.go | TestM64KeyOverrideIsTenantScoped, TestM64ThresholdOverrideChangesBoundary, TestM64TypeSemanticSkipsExact, TestM64ParseCacheControls |
| Cache response headers | X-OmniRoute-Cache: HIT + Similarity + Latency: synthetic + savings tokens + Cost-Saved | X-Codvian-Cache: exact\|semantic + Similarity (3 dp); stale headers as above. Ours names the layer; theirs adds cost-saved/latency | FULL (parity, not AHEAD) | internal/server/server.go:3545-3549; internal/server/decision.go:112 | UNVERIFIED |
| Embeddings / rerank cache exemption | Not verified in this pass | YES — Embed/Rerank forward only, no lookup/store/stale | FULL | internal/degrade/degrade.go; internal/server/embeddings.go:31 | UNVERIFIED |

### Area 7 — Guardrails, PII redaction, content policy (36 total: FULL 17, PARTIAL 7, MISSING 12, WRONG 0; strict 47.2%, weighted 56.9%)

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Email PII | piiSanitizer.ts:57, builtin type email | email rule | FULL | internal/guardrails/guardrails.go | UNVERIFIED |
| Phone PII | phone_us, phone_br (piiSanitizer.ts:80,87) | phone rule: +91, +1, 3-3-4, 10-digit IN only; no BR/other countries | PARTIAL | internal/guardrails/guardrails.go | UNVERIFIED |
| SSN | ssn (piiSanitizer.ts:63), ssn_us request-side | No SSN pattern anywhere | MISSING | — | UNVERIFIED |
| Credit card | credit_card with Luhn validation (piiSanitizer.ts:258) | card rule matches any 16 digits; no Luhn — false positives on any 16-digit run | PARTIAL | internal/guardrails/guardrails.go | UNVERIFIED |
| Aadhaar | No Aadhaar pattern | aadhaar rule (\d{4}\s\d{4}\s\d{4}), digit-shape gated | FULL (AHEAD) | internal/guardrails/guardrails.go | UNVERIFIED |
| CPF / CNPJ (BR IDs) | Full, with checksum validators (piiSanitizer.ts:261,264) | None | MISSING | — | UNVERIFIED |
| IP address v4/v6 | ip_address, ipv6_address | None | MISSING | — | UNVERIFIED |
| IBAN + EU national IDs (8 GDPR types) | iban, eu_vat, es_dni_nie, it_codice_fiscale, pl_pesel, nl_bsn, de_steuer_id, se_personnummer | None | MISSING | — | UNVERIFIED |
| ZA IDs (4 POPIA types) | za_id, za_phone, za_vat, za_tax_ref | None | MISSING | — | UNVERIFIED |
| Provider-secret patterns | 28 named patterns in CREDENTIAL_PATTERNS (credentialPatterns.ts) | 20 named secret rules + generic-token rule. Missing: Replicate r8_, Linear, Notion, npm, Postman, Discord, Square, Twilio, Mailgun, connection strings, generic sk- | PARTIAL | internal/guardrails/guardrails.go | UNVERIFIED |
| Generic/shape-based secret catch-all | None — only api_key_generic requiring sk\|pk\|api\|key\|token prefix | redactGenericTokens: any ≥32-char mixed run is masked, any field | FULL (AHEAD) | internal/guardrails/guardrails.go (minGenericTokenLen=32) | UNVERIFIED |
| PEM / private-key whole block | Whole-block pattern BEGIN…END → [REDACTED:private_key] (credentialPatterns.ts:74-80); streaming CredentialOverflow withholds unclosed PEM | Whole-block BEGIN…END redaction incl. footer, lowercase headers, RSA/EC/ENCRYPTED labels, unclosed-to-EOF; StreamRedactor wired into both stream paths; fuzz-tested (Wave 1, M1.7) | FULL | internal/guardrails/pem.go; internal/guardrails/pem_stream.go | TestRedactPEMWholeBlock, TestW1ChatStreamRedactsSplitPEM, TestW1MessagesStreamRedactsSplitPEM |
| JWT | jwt pattern | jwt rule | FULL | internal/guardrails/guardrails.go | UNVERIFIED |
| Bearer / auth-header | auth_header pattern + header-key redaction (credentialMasker.ts) | bearer-token, auth-header rules | FULL | internal/guardrails/guardrails.go | UNVERIFIED |
| Connection strings (mongo/postgres/mysql/redis/amqp) | connection_string pattern | None | MISSING | — | UNVERIFIED |
| Labelled password | No password pattern in credential catalog or PII sanitizer | password rule (password\|passwd\|pwd\s*[:=]…) | FULL (AHEAD) | internal/guardrails/guardrails.go | UNVERIFIED |
| Per-type toggles | 23 builtin types (piiTypes.ts), per-type selection, 8 dashboard presets, GET/PUT /api/settings/pii | Only 3 kind toggles (BlockPII/BlockSecrets/BlockInjection); no per-type selection | MISSING | — | UNVERIFIED |
| Custom regex patterns | Up to 20, safe-regex validated, test endpoint (piiCustomPatterns.ts) | None | MISSING | — | UNVERIFIED |
| Reversible PII token vault | Opt-in PII_REVERSIBLE_VAULT_ENABLED, tokens minted pre-call, restored at exit | None | MISSING | — | UNVERIFIED |
| Request rewrite before provider | Credential/PII maskers return modifiedPayload — secrets stripped pre-call (opt-in) | Log-only by design: Check/CheckScoped never mutate the request; with blocking off a secret goes to the provider verbatim | MISSING | internal/server/server.go:4137 applyGuardrails | UNVERIFIED |
| Response sanitization, non-streaming | Opt-in PII_RESPONSE_SANITIZATION, modes redact\|warn\|block (default redact) | Response checked but block-or-deliver only: no redact mode | PARTIAL | internal/server/server.go:4252 applyResponseGuardrails | UNVERIFIED |
| Response sanitization, streaming (SSE) | Opt-in SSE transforms: PII + credential stream transforms | Streaming responses are never response-checked; only async-guardrail cut can stop a stream | MISSING | internal/server/server.go (applyResponseGuardrails comment) | UNVERIFIED |
| Prompt-injection detection | 7 shared patterns (inputSanitizer.ts) + 2 guard patterns, severity-scored, modes block\|warn\|log, 16 KB scan bound | 11 phrase patterns. No severity scoring, no custom patterns; scans full text (stronger coverage, unbounded CPU) | PARTIAL | internal/guardrails/guardrails.go | UNVERIFIED |
| Per-kind blocking + refusal code | Injection block mode only; PII/credential never block, only redact | 3 opt-in toggles; block → HTTP 403 with -32001, type: policy_refusal, reason naming kind | FULL (AHEAD) | internal/guardrails/guardrails.go (PolicyRefusalCode); internal/server/server.go writePolicyRefusal | UNVERIFIED |
| Fail-open on guardrail/classifier error | Registry catches per-guardrail throw → logs failed open, continues | Sync external classifier error → request continues, logged redacted; built-ins are pure regex | FULL | internal/server/server.go applyGuardrails | UNVERIFIED |
| Log redaction default-on, no off switch | PII/credential redaction opt-in, off by default (Hard Rule #20) | Always on: redactWriter wraps every server log line, no disable path; logSafeError logs status-code-only | FULL (AHEAD) | internal/guardrails/redact_log.go:77 | UNVERIFIED |
| Ledger / spend-log redaction | Not verified on their side | SanitizeSpendString on all 5 ledger identity fields; prompt text never stored | FULL | internal/ledger/ledger.go:612-616 | BenchmarkRedactLogLine (1,881 ns/op re-run) |
| Upstream-error sanitization | sanitizeErrorMessage, buildErrorBody | logSafeError: upstream body never logged, status code only | FULL | internal/guardrails/redact_log.go | UNVERIFIED |
| Per-key guardrail scoping | Per-key disabledGuardrails — client can disable guardrails via header/body/metadata (registry.ts) — a client-controlled off switch | Admin-configured per-key allowlist; unknown name → 400 + counted security refusal; no client disable path | FULL (AHEAD) | internal/server/guardrail_scope_277.go:21 | UNVERIFIED |
| Content-policy refusal chain | None: 400/403 non-retryable in resilience layer | 38-phrase signal list; 400/403/422 with content signal → separate refusal chain; dashboard renders content_refused amber | FULL (AHEAD) | internal/wire/retryable.go:114; internal/provider/taxonomy.go:306-315; internal/router/refusal.go | UNVERIFIED |
| Distinct refusal codes | No policy-vs-upstream code distinction found | -32001 policy refusal vs -32603 upstream failure | FULL (AHEAD) | internal/guardrails/guardrails.go | UNVERIFIED |
| Modality bridges (vision/audio/video reroute) | Vision 5, audio 6, video 7 (registry.ts:308-310) | None | MISSING | — | UNVERIFIED |
| Async external classifiers + cut-before-commit | No async guardrail mode found; guardrails run sync pre-call | async.go: sync/async per-guardrail mode, per-check timeout, fail-open on error, blocked_before_delivery outcome | FULL (AHEAD) | internal/guardrails/async.go | UNVERIFIED |
| Dashboard + counters | No guardrails page; PII card in Settings → Security; per-type detection counters (in-memory) | Full Guardrails page: blocking toggles, per-(phase,action) eval counts, per-kind counts, last 50 events, security refusals. Counters per-kind (3) only — no per-type/per-pattern totals | PARTIAL | web/index.html:1615 | UNVERIFIED |
| Route coverage of checks | Verified call sites only: pre-call chat.ts:881, post-call nonStreamingResponse.ts:830 (chat path) | Request check on chat, /v1/messages relay, /v1/responses, embeddings and rerank; MCP tool args; response check on chat + Responses only — relay responses and all streaming responses unchecked | PARTIAL | internal/server/embeddings.go:213; internal/mcp/mcp.go:288 | UNVERIFIED |
| Hot-path latency, measured | No published per-call guardrail latency found | BenchmarkRedactLogLine 4,563 → 1,902 ns/op; re-run this session on verbatim copy: 1,881 ns/op; package tests pass (36 test funcs) | FULL (AHEAD, measured) | internal/guardrails/ | BenchmarkRedactLogLine |

### Area 8a — Dashboard: Home / Providers / Models (+ Pricing) (38 comparable rows: FULL 5, PARTIAL 14, MISSING 19; strict 13.2%, weighted 31.6%)

Condensed: full row-level detail (49 rows incl. 11 EXTRA) is in the source comparison
report §Area 8a. Sub-tallies: Home/Overview 14 rows (3 FULL / 2 PARTIAL / 4 MISSING /
5 EXTRA); Providers 18 rows (1 FULL / 6 PARTIAL / 8 MISSING / 3 EXTRA); Models (+ Pricing)
17 rows (1 FULL / 6 PARTIAL / 7 MISSING / 3 EXTRA).

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| 4-step quick-start (create key → connect → point client → monitor) + Full Docs link | HomePageClient.tsx:975-1060; en.json:1981-1989 | No 4-step guide. Overview has "Point your app at the router" code block; Providers has a 3-step strip. No create-key step, no monitor step, no Full Docs link | PARTIAL | web/index.html:857-910,931-945 | UNVERIFIED |
| Quick-start show/hide toggle (default on) | HomePageClient.tsx:225,239 | No toggle found | MISSING | — | UNVERIFIED |
| Provider topology widget | HomeProviderTopologySection.tsx, wired :1060 | Provider health card with status chips + 24h uptime bars + p50 — no topology view, no per-provider model counts | PARTIAL | web/index.html:830-845 | UNVERIFIED |
| Recent requests | HomeRecentRequests.tsx, wired :1066 | Recent requests table + honest "No requests yet" empty state | FULL | web/index.html | UNVERIFIED |
| System metrics cards (CPU, Memory, host + process) | HomeSystemMetricsCards.tsx:188-230 | No CPU/memory card on Overview (memstats live in Benchmark view) | MISSING | — | UNVERIFIED |
| Provider quota widget | ProviderQuotaWidget.tsx (component exists) | No quota widget on Overview | MISSING | — | UNVERIFIED |
| Update flow (Update Available, Check, Update Now, Restart & Install, Release Notes, downloads) | HomePageClient.tsx:151-172; UpdateProgressDialog.tsx | No update UI anywhere on Overview | MISSING | — | UNVERIFIED |
| Traffic chart (requests/hour + mean latency + errors) | Prior report §2.2 (not re-sampled) | Traffic SVG chart: requests, mean-latency right axis, errors/bucket, 24h/7d/30d range selector | FULL | web/index.html | UNVERIFIED |
| Refresh + Add provider buttons | Present on their pages | Both present | FULL | web/index.html (#refreshBtn) | UNVERIFIED |
| "Add your first provider" empty state → catalog IS the content | providers/page.tsx:906 + 15 always-visible groups | Catalog card + add form always shown; no empty page | FULL | web/index.html | UNVERIFIED |
| Provider groups as always-visible addable sections | 15 groups, providers/page.tsx:1018-1853 | One flat searchable catalog table (configured pinned on top, Add/Manage per row, ToS-grey Add disabled with reason). No grouping | PARTIAL | web/index.html:3203-3245 | UNVERIFIED |
| Per-group "Test all" buttons | Per-group testAll* titles throughout | Zero "Test all" strings in our file | MISSING | — | UNVERIFIED |
| Provider Onboarding Wizard | :914, providers/new (942 lines) | Single add-provider form only; no wizard | MISSING | — | UNVERIFIED |
| Import from file / OAuth export-import modals | :946 | None | MISSING | — | UNVERIFIED |
| Repair env (restore missing OAuth defaults) | :1124 | None | MISSING | — | UNVERIFIED |
| Add OpenAI / Anthropic / CC Compatible buttons | Groups + addCcCompatibleLabel (:1853) | Custom mode = "any OpenAI-compatible base URL"; no explicit Anthropic- or CC-compatible add buttons | PARTIAL | web/index.html | UNVERIFIED |
| Per-provider card: status, Disabled, Rate-limited, error types, expiration banner | :174-177, :398-412 | Health chips, verification badges, uptime bars, p50 — no error-type breakdown, no expiration banner | PARTIAL | web/index.html (#provGrid) | UNVERIFIED |
| Provider detail page (connections, quota panel, cooling, models, override editors, playground, import modals) | providers/[id] (1,027 lines) | No detail view; Manage scrolls to the grid card | MISSING | — | UNVERIFIED |
| Search (text + model search + category/serviceKind filter) | :252-291 | Text search over name/id/alias; no category/serviceKind filter | PARTIAL | web/index.html (#catalogSearch) | UNVERIFIED |
| Configured-only filter / configured count | filterConfiguredProviderEntries (:42) | Configured pinned on top; no configured-only filter toggle | PARTIAL | web/index.html | UNVERIFIED |
| Free Tier Providers group | :1277 | No free-tier grouping; catalog table has no free-tier column | MISSING | — | UNVERIFIED |
| Test Results modal ("{passed}/{total} passed") | :1871 | None | MISSING | — | UNVERIFIED |
| Add-form fields (Provider, Connection name, Display name, Provider prefix, Region, Protocol, Base URL, Chat/Models path, API key, Custom User-Agent) | providers/new, provider detail | Ours: Source, catalog select, Endpoint, Variant, Base-URL override, Display name, Provider API key, Gateway key. No Connection name, Provider prefix, Region, Protocol picker, Chat/Models path, Custom User-Agent | PARTIAL | web/index.html | UNVERIFIED |
| OAuth connect flow (sign-in, token rotation) | OAuth groups + OAuth modals | API-key-only add; OAuth entries ToS-grey-disabled or API-key surfaced; no OAuth connect flow | MISSING | — | UNVERIFIED |
| Models title/subtitle "Browse and health-test models and combo routing pipelines" | models/page.tsx | "Models / Only models from your connected providers" — no health-test or combo mention | PARTIAL | web/index.html | UNVERIFIED |
| Tabs Models (n) / Combos (n) | CatalogTabs, page.tsx:718-720 | No tabs in Models view | MISSING | — | UNVERIFIED |
| Models table columns (Provider/Model/Type/Capabilities/Context/Max output/Flags/Custom/Free) | page.tsx:900-910 | Ours: Model/Provider/Context/Input per 1M/Output per 1M/Price source/Requests/Fit/Status. Missing Type, Capabilities, Max output, Flags, Custom, Free | PARTIAL | web/index.html | UNVERIFIED |
| Filters (provider/type/capability/min-context/min-output/free, etc.) | ModelCatalogFilters.tsx, CatalogFilterFields.tsx | Provider select, Category select, Max price, Min context, Shortlist only, Include deprecated, text search. Missing Type, Capabilities, Free-only | PARTIAL | web/index.html | UNVERIFIED |
| "{visible} visible · {hidden} hidden · {total} total" counts | page.tsx:648,710 | #modelCount element exists; no visible/hidden split observed | PARTIAL | web/index.html | UNVERIFIED |
| Per-row visibility toggle | onToggleVisibility, useCatalogVisibility.ts | None | MISSING | — | UNVERIFIED |
| Per-row health test + test-result badge | testSingleModel, CatalogTestBadge.tsx | None per model (health exists per provider only) | MISSING | — | UNVERIFIED |
| Bulk test ("Start bulk test?" + quota warning) | CatalogBulkActionBar.tsx | None | MISSING | — | UNVERIFIED |
| Refresh (Models) | models/page.tsx | "Refresh model lists" button | FULL | web/index.html | UNVERIFIED |
| Empty states ("No models are available yet" / "No models match these filters" / "Clear filters") | Prior report §2.2 | "No models yet — connect a provider first. [Add a provider]" (one line + button). No "no match" state, no Clear filters | PARTIAL | web/index.html | UNVERIFIED |
| Pricing as separate per-model rate table (Input/Output/Cached/Reasoning/Cache Write $/1M + By provider/Compare views + coverage filters + sort) | costs/pricing/ (route exists; columns per prior report, not re-verified) | No Pricing page/view at all — zero "pricing" matches in our file | MISSING | — | UNVERIFIED |
| Pricing sync (Auto Sync, Sync Now, Last/Next sync, Synced Models, Clear Synced; sources User Override / Models.dev / LiteLLM / Built-in Default) | Prior report §2.2 | Only a status line #modelSyncStatus ("Live price/context sync: checking…"); no sync controls | PARTIAL | web/index.html | UNVERIFIED |
| Catalog Sections grouping | page.tsx ("Catalog Sections") | Flat table, no sections | MISSING | — | UNVERIFIED |
| Pagination | getCatalogPage, PAGE_SIZE | Not found in source (table renders full list) | MISSING | — | UNVERIFIED |

EXTRA (Area 8a, ours-only): 3-step "Pick / Paste key / Route" strip; verification badges;
per-provider uptime bars + p50 latency; token-mix donut + prompt-cache hit rate; live-routing
feed + strategy line + route trace + region failover + reasoning replay + responses +
compression summaries; KPI row (exactly 4); time-range selector 24h/7d/30d; local capabilities /
routing groups / catalog audit cards + Fit column; shortlist-only + include-deprecated +
max-price filters; per-model request counts. Caveat: Overview carries ~11 top-level cards vs
the item-295 cap of 4 — KPI row met, card cap NOT met. Baseline corrections: "230 <p> tags"
WRONG — 174 true <p> tags (109 static + 65 in JS); "0 providers = empty everywhere" now FALSE
for Providers and Models in source; catalog "531" stale — 610.

### Area 8b — Dashboard: Routing / Keys / Budgets / Analytics / Logs / Settings (+ Benchmark) (139 comparable rows: FULL 9, PARTIAL 36, MISSING 93, WRONG 0; strict 6.5%, weighted 19.4%)

Condensed: full row-level detail (152 options incl. 13 EXTRA, 1 DIVERGENCE) is in the
source comparison report §Area 8b. Sub-tallies: Routing strategies 22 rows (1 FULL /
3 PARTIAL / 18 MISSING / 0 WRONG); Routing combo builder & global settings 31 rows
(0 FULL / 10 PARTIAL / 20 MISSING / 1 WRONG); Keys 21 rows (2 FULL / 5 PARTIAL /
14 MISSING); Budgets 17 rows (1 FULL / 3 PARTIAL / 12 MISSING / 1 EXTRA); Analytics
21 rows (0 FULL / 8 PARTIAL / 11 MISSING / 1 EXTRA); Logs 14 rows (1 FULL / 3 PARTIAL /
7 MISSING / 3 EXTRA); Settings 22 rows (3 FULL / 4 PARTIAL / 12 MISSING / 0 WRONG /
2 EXTRA / 1 DIVERGENCE); Benchmark 6 rows (all EXTRA).

Key rows:

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| 22 routing strategies in dashboard picker | routingStrategies.ts:1-22 | Picker is feed-driven: buildStrategyOptions builds options purely from GET /dashboard/strategies (strategy.All, now 34 strategies, exceeding the row's 22); no hard-coded name list. V: pass 2026-11-20 00:32 IST (V-8) | FULL | web/index.html:11237,11284; internal/server/server.go:5325 | TestStrategiesFeedLive, TestStrategiesFeedEmpty, TestStrategiesFeedCostAndQuality |
| Named combos CRUD (create/duplicate/import/export/test/enable) | combos/page.tsx | No combos at all — chains are provider-model lists | MISSING | — | UNVERIFIED |
| Endpoint behavior per-capability tabs | Combos builder (chat/search/embeddings/rerank/images/video/music/OCR/audio) | None | MISSING | — | UNVERIFIED |
| Default Auto Variant (10 values) | AUTO_ROUTING_STRATEGY_VALUES; RoutingTab.tsx:942 | 6 virtual models (auto, auto/fast, auto/cheap, auto/coding, auto/code, auto/reasoning + intent/*); no router-strategy picker, no sla/nadir/lkgp | PARTIAL | web/index.html | UNVERIFIED |
| Session stickiness toggle | Combo builder ("Disable session stickiness") | The display-only switch is gone: Settings shows a read-only status line (pinning not reported by any live feed); no session-stickiness control exists anywhere — feature MISSING, not wrong. V: pass 2026-11-20 00:32 IST (V-8, feature-absent confirmed) | MISSING | web/index.html:2504 | UNVERIFIED |
| Create API key (dashboard) | api-manager/page.tsx + ApiManagerPageClient.tsx | "+ New router key" button exists but not wired — honest toast "isn't wired yet" | PARTIAL | web/index.html:5888 | UNVERIFIED |
| Regenerate key (confirm dialog) | ApiManagerPageClient.tsx:493 | HAVE — rotate button + handler + shown-once box | FULL | web/index.html:5836 | UNVERIFIED |
| Provider keys (per-provider, add/delete/role) | Connections per provider | HAVE — provider-keys card, add-key form (Preferred/Reserve), POST/DELETE/role endpoints | FULL | web/index.html:2792 | UNVERIFIED |
| Scope enforcement (keys) | Enforced per route (keyScopedRoute.ts) | HasScope wired via requireScope on every /v1 + admin route (Wave 1, M1.1) | FULL | internal/config/gatewaykey.go:141; internal/server/server.go | TestScopeEnforcedTableDriven |
| Single-key disable/revoke | is_active / revoked_at per key | Only revoke-all exists | MISSING | — | UNVERIFIED |
| Daily / Weekly / Monthly $ limit per key | Costs → Budget page; en.json:10816 | Only rolling 5h exists | MISSING | — | UNVERIFIED |
| Rolling 5h USD budget per key | apiKeys.ts:151 fiveHourUsageLimitUsd | HAVE, parity — GET/POST /dashboard/key-budgets, fail-closed unpriced | FULL | internal/server/keybudgets.go | UNVERIFIED |
| Quota-share pools | /dashboard/costs/quota-share, fairShare.ts | None | MISSING | — | UNVERIFIED |
| Team cost centers | /dashboard/costs/teams | None | MISSING | — | UNVERIFIED |
| Cost Overview KPIs (Spend 1D/7D/30D, requests, active providers/models, avg cost/req) | /dashboard/costs CostOverviewTab | Spend via 24h/7d/30d range seg + tokens + failovers; no side-by-side 1D/7D/30D, no avg cost/req | PARTIAL | web/index.html | UNVERIFIED |
| Monthly forecast | Costs Overview "Monthly Forecast" | None | MISSING | — | UNVERIFIED |
| Cost Explorer (dimension filter, top 50, CSV/JSON export) | Costs Overview | None (CSV export of logs only) | MISSING | — | UNVERIFIED |
| Combo Health (quota risk, forecast, Gini skew, autopilot, scoring inspector) | analytics/ComboHealthTab.tsx | None | MISSING | — | UNVERIFIED |
| Request logs table | logs/page.tsx | HAVE — all columns + Reasoning/Cached/Saved; GET /dashboard/logs, All/ok/failed seg | FULL | web/index.html | UNVERIFIED |
| Proxy Logs / Console output / Timeline / Activity feed / Conversations / Compression log / Audit log | logs/proxy, logs/console, logs/timeline, /dashboard/activity, /dashboard/conversations, CompressionLogTab, /dashboard/audit | None (latest-route-trace card only is the timeline cousin; guard denials counted only, no audit view) | MISSING | — | UNVERIFIED |
| Routing strategy select (wired) | Settings → Routing | HAVE — #strategySel built from live feed, change POSTs real global strategy | FULL | web/index.html:5912,7106 | UNVERIFIED |
| Automatic failover / Prompt-cache pinning / Cost-aware routing / Health checks / Request logging switches | Settings toggles | The 5 display-only switches are gone: all five are honest read-only status lines fed by live endpoints; no toggle backends exist — feature MISSING, not wrong. V: pass 2026-11-20 00:32 IST (V-8, feature-absent confirmed) | MISSING | web/index.html:2500-2533 | UNVERIFIED |
| Listen address + Port | Settings → Server equivalents | Listen address is read-only, fed live from GET /dashboard/guard (default 127.0.0.1:20128); no Port input and no "8080" anywhere in the dashboard. V: pass 2026-11-20 00:32 IST (V-8) | FULL | web/index.html:2528,12178-12184 | TestGuardFeed |
| Authz / route inventory | Settings → Security AuthzSection | HAVE, exceeds theirs — Route guards page: live Tier 1/2/3 tables from GET /dashboard/guard + tunnel + DB export | FULL | internal/server/server.go:1188 | UNVERIFIED |
| Require login / password change | Settings → Security | No password login at all — gateway-key auth model instead | DIVERGENCE | — | UNVERIFIED |
| Benchmark page (ours-only) | No benchmark page | Live from GET /dashboard/benchmark + history + memstats + release-verify; missed-target verdicts shown honestly | EXTRA | web/index.html | UNVERIFIED |
| `priority` (their default) | `routingStrategies.ts:2`, `comboConfig.ts` default | No declared-order strategy; chain order destroyed by quota-hint sort unless contract `order` given | MISSING | — | UNVERIFIED |
| `round-robin` | Own loop `combo/roundRobinCombo.ts` + `rrState.ts` | Nothing | MISSING | — | UNVERIFIED |
| `context-relay` | `strategyRegistry.ts:143`, handoff after success | Only reasoning-state handoff — different thing | MISSING | — | UNVERIFIED |
| `fill-first` | Declared order kept until target fails/exhausts | No named strategy | MISSING | — | UNVERIFIED |
| `p2c` | `targetSorters.ts:136` | Nothing | MISSING | — | UNVERIFIED |
| `random` | Fisher-Yates shuffle | Nothing | MISSING | — | UNVERIFIED |
| `least-used` | `targetSorters.ts:107` | Attempt counts exist, nothing orders by usage | MISSING | — | UNVERIFIED |
| `cost-optimized` | Pure cheapest-first, unknown price = ∞ last (`targetSorters.ts:91`) | `cheapest` = cost 0.55 + other factors vote; unknown price neutral 0.5, never last | PARTIAL | — | UNVERIFIED |
| `reset-aware` | `quotaStrategies.ts:552`, weights 0.35 session / 0.65 weekly | Scalar quota-hint int only; no reset data | MISSING | — | UNVERIFIED |
| `reset-window` | `quotaStrategies.ts:596` | No reset-window data | MISSING | — | UNVERIFIED |
| `expiry-first` | `quotaStrategies.ts:727` | No quota-expiry data | MISSING | — | UNVERIFIED |
| `headroom` | `headroomRanking.ts:54` (`1 − max(util5h, util7d)`) | Quota hint is a single int, not utilisation | MISSING | — | UNVERIFIED |
| `quota-weighted` | Draw weight `score/(1+load)` (`quotaStrategies.ts:971`) | No weighted draw; quota only 0.10 in score | MISSING | — | UNVERIFIED |
| `strict-random` | Shuffle deck, mutex-protected | No deck/shuffle | MISSING | — | UNVERIFIED |
| `auto` (16-factor) | `scoring.ts:62`, 16 factors, pool-relative | `auto/*` resolves a *model* via embedding tiers; no pool scoring, mode packs, or taskFit | PARTIAL | — | UNVERIFIED |
| `lkgp` | `orderByLkgp`, persisted LKGP | `lastRouting` recorded for dashboard only, never used to reorder | MISSING | — | UNVERIFIED |
| `context-optimized` | Largest context window first (`comboStructure.ts:1050`) | `contextfit.go:175` only *filters out* too-small models | PARTIAL | — | UNVERIFIED |
| `cache-optimized` | `orderByCacheAffinity`, affinity owns ordering | Prompt-cache passthrough only; no affinity ordering | MISSING | — | UNVERIFIED |
| `fusion` | `fusion.ts:269`, parallel panel + judge synthesis | No fan-out, no judge | MISSING | — | UNVERIFIED |
| `pipeline` | `pipelineRouter.ts:219`, multi-stage | No multi-stage pipeline | MISSING | — | UNVERIFIED |
| `jev` | `jevStrategy.ts:139`, external judge, floor 0.15 | No judge, no admitted/held sets | MISSING | — | UNVERIFIED |
| Zero-config auto-routing enable toggle | Settings → Routing: "Enable automatic provider selection using the auto/ prefix" | `auto/*` always on via virtual models; no toggle | PARTIAL | — | UNVERIFIED |
| Sticky Limit (calls/account before RR switch) | ComboDefaultsTab ("Round Robin plus Sticky Limit") | None | MISSING | — | UNVERIFIED |
| Concurrency/Model, Queue Timeout, Queue Depth | ComboDefaultsTab + combo builder advanced settings | Deadline budget only (clean 504, shown live); no queue timeout/depth UI; concurrency lives on a separate page as adaptive limiter, not combo queue | PARTIAL | — | UNVERIFIED |
| Track Metrics toggle | ComboDefaultsTab | Metrics always on; no toggle | PARTIAL | — | UNVERIFIED |
| Max Retries / Retry Delay (ms) | Combo builder + ComboDefaultsTab | Backend has MaxAttempts 5 + backoff; no UI control | PARTIAL | — | UNVERIFIED |
| Max Nesting Depth | ComboDefaultsTab | None in UI (backend `maxComboDepth` is theirs; ours unverified) | MISSING | — | UNVERIFIED |
| Provider Overrides (per-provider max retries) | ComboDefaultsTab | None | MISSING | — | UNVERIFIED |
| Failover Before Retry toggle | Combo builder advanced settings | Failover-first is default behaviour; no toggle | PARTIAL | — | UNVERIFIED |
| Max Set Retries / Set Retry Delay | Combo builder advanced settings | None | MISSING | — | UNVERIFIED |
| Combo Quota Only Fallback toggle | Combo builder | None | MISSING | — | UNVERIFIED |
| Reasoning Token Buffer toggle | Combo builder | None | MISSING | — | UNVERIFIED |
| Response Validation editor | Combo builder | None | MISSING | — | UNVERIFIED |
| Auto-combo catalog variants (subscription fail-closed, thrifty, offline, smart, lkgp, chaos, `auto/<cat>:<tier>`, best-*/pro-*) | `AUTO-COMBO.md`, AutoComboCatalog | `auto/coding, auto/fast, auto/cheap` match; `auto/code, auto/reasoning, intent/*` are ours; offline/lkgp/chaos/subscription/thrifty/chat/cat:tier missing | PARTIAL | — | UNVERIFIED |
| LKGP toggle + Clear LKGP Cache | Settings → Routing | None | MISSING | — | UNVERIFIED |
| System-block Transform Pipeline (ops, 50 max, JSON import/export) | Settings → Routing, idempotent ops | None | MISSING | — | UNVERIFIED |
| CLI Fingerprint Matching per provider | Settings → Routing | None | MISSING | — | UNVERIFIED |
| Client Cache Control (Always/Never/Auto preserve) | Settings → Routing | None | MISSING | — | UNVERIFIED |
| Echo requested model name | Settings → Routing | None | MISSING | — | UNVERIFIED |
| Web search routing model picker | Settings → Routing | None | MISSING | — | UNVERIFIED |
| Adaptive Volume Routing toggle | Settings → Routing | None | MISSING | — | UNVERIFIED |
| Antigravity Signature Cache Mode | Settings → Routing | None (narrow/provider-specific) | MISSING | — | UNVERIFIED |
| 16-factor scoring + 6 mode packs | `scoring.ts` DEFAULT_WEIGHTS, `modePacks.ts` | 5 factors/mode, no taskFit/stability/health; no packs | MISSING | — | UNVERIFIED |
| Scoring inspector (factor-level, read-only recompute) | Combo Health: "Intelligent scoring inspector" | Auto-router decision card shows tier/confidence/per-tier scores — same idea, `auto/*` only | PARTIAL | — | UNVERIFIED |
| Account fallback strategies (9 values) | `ACCOUNT_FALLBACK_STRATEGY_VALUES` | Single global strategy only | MISSING | — | UNVERIFIED |
| Hedging control (delay, on/off) | Combo config `hedging:false`, `hedgeDelayMs:500` | Hedging ON, 150 ms, no UI control, no body-size guard | PARTIAL | — | UNVERIFIED |
| maxGlobalAttempts clamp [1,200] | `comboPredicates.ts:132-137` | MaxAttempts default 5, no clamp, no UI | PARTIAL | — | UNVERIFIED |
| Candidate-pool endpoint | `GET /v1/auto-combo/{channel}/candidates` | None (data exists internally, feeds `/dashboard/auto-route` only) | MISSING | — | UNVERIFIED |
| Combo Studio live cascade | `/dashboard/combos/live` | Strategy-trees card is read-only config display, not a live cascade | MISSING | — | UNVERIFIED |
| Per-key model restriction ("All models") | Per-key card | Keys feed shows scopes/mask; no per-key model restriction UI | PARTIAL | — | UNVERIFIED |
| Exclusive Lease badge | `[id]/access/tabs/GeneralTab.tsx:75` | None | MISSING | — | UNVERIFIED |
| JSON-stream default badge | Per-key card | None | MISSING | — | UNVERIFIED |
| Device tracking (distinct IP/UA, last 30 min) | Per-key card "{count} device(s)" | None | MISSING | — | UNVERIFIED |
| Requests + Last used on card | Per-key card "reqs", "Last: {date}" / "Never used" | Usage/logs carry this, not the key card | PARTIAL | — | UNVERIFIED |
| Access schedule | Per-key card "Schedule" | None | MISSING | — | UNVERIFIED |
| Show/Hide key | "Show Key"/"Hide Key" | Raw shown once only at rotation; no per-key show/hide | PARTIAL | — | UNVERIFIED |
| Per-key delete | "Delete this API key?" | Not wired — honest toast "Revoking keys from the dashboard isn't wired yet" | PARTIAL | — | UNVERIFIED |
| Save as template | Per-key action | None | MISSING | — | UNVERIFIED |
| Edit permissions (key manage) | Per-key access + routing sub-pages | Manage form (expiry, RPM, TPM, guardrails allowlist) with real save handler (:5783); no model/endpoint/connection allow-lists | PARTIAL | — | UNVERIFIED |
| Per-key routing page | `/dashboard/api-manager/routing` | None | MISSING | — | UNVERIFIED |
| Per-key access (allowed models/connections/endpoints/combos) | `/dashboard/api-manager/[id]/access` | None | MISSING | — | UNVERIFIED |
| Key import | `POST /api/keys/import` | None | MISSING | — | UNVERIFIED |
| Key pools / fair-share quotas | Per-key policy fields | None | MISSING | — | UNVERIFIED |
| IP allowlist/blocklist per key | Per-key policy fields | None | MISSING | — | UNVERIFIED |
| Management Access Tokens (read/write/admin, expiry, revoke, shown once) | `/dashboard/settings/access-tokens` | None — gateway keys only, no CLI/management tokens | MISSING | — | UNVERIFIED |
| Warn at % | Budget page "Warn at %" | None per key (80% warn on legacy cap only, no UI) | MISSING | — | UNVERIFIED |
| Reset Interval / Reset Time (UTC) / Next reset | Budget page | None — rolling window, no calendar reset | MISSING | — | UNVERIFIED |
| Budget templates ($/month, $/day, apply to selected) | Budget page "Templates" | None | MISSING | — | UNVERIFIED |
| Projection (linear, Proj EOM, "above {limit}/month") | Budget page "Projection" | None | MISSING | — | UNVERIFIED |
| 30-day cost breakdown by provider (per key) | Budget page "Cost breakdown (30 days)" | Spend-by-provider table exists in Usage (not per-key, no projection) | PARTIAL | — | UNVERIFIED |
| Budget table (Key/Today/Month/Daily lim/Monthly lim/Used%/Status) + sorts | Budget page | Ours has a different table: 5h Key/Limit/Spent/Remaining/Window/Unknown | MISSING | — | UNVERIFIED |
| Per-key token limits (model/provider/global, most-restrictive-wins, 429) | `quota/enforce.ts:142-186` | RPM + TPM only, prompt-only, in-memory, sliding-60s, no scoping | PARTIAL | — | UNVERIFIED |
| Free-Tier Budget page | `/dashboard/free-tiers` (route confirmed; detail sampled) | None | MISSING | — | UNVERIFIED |
| Free Provider Rankings (by model ELO) | `/dashboard/free-provider-rankings` (route confirmed; detail sampled) | None | MISSING | — | UNVERIFIED |
| Provider Quota page | `/dashboard/quota` (route confirmed) | Per-provider history/uptime + quota hints; no quota-limit UI | PARTIAL | — | UNVERIFIED |
| Token usage (total/in/out, in/out ratio) | Costs Overview "Token Usage" | Total tokens routed; in/out split only per-request in Logs, no aggregate ratio | PARTIAL | — | UNVERIFIED |
| Routing efficiency (fallback reqs/rate, model coverage) | Costs Overview | Failovers count only; no fallback rate %, no model coverage | PARTIAL | — | UNVERIFIED |
| Cost Trend / Provider Share / Top Providers / Top Models | Costs Overview charts | Spend bars by provider + spend-by-model table; no trend chart | PARTIAL | — | UNVERIFIED |
| Cost by API Key | Costs Overview table | None (per-key spend only in 5h budget table) | MISSING | — | UNVERIFIED |
| Cost by Account | Costs Overview | None | MISSING | — | UNVERIFIED |
| Weekly pattern / 365-day heatmap / Period comparison | Costs Overview | None | MISSING | — | UNVERIFIED |
| Pricing rate table (Input/Output/Cached/Reasoning/Cache Write $/1M, sync source/row) | `/dashboard/costs/pricing`, `PricingTab.tsx:533-592` | `GET /dashboard/prices` feed exists; pricing-as-table presentation **not re-verified this pass** (prior report: 163-word prose paragraph at :1009) | PARTIAL | — | UNVERIFIED |
| Pricing sync (models.dev/LiteLLM, Sync Now, last/next sync, clear synced) | Pricing page "Pricing Sync" | models.dev sync opt-in, backend-only; no UI | MISSING | — | UNVERIFIED |
| Utilization (Global/Account split, quota snapshots, remaining capacity) | `analytics/ProviderUtilizationTab.tsx` | Per-provider quota hints only | MISSING | — | UNVERIFIED |
| Cache analytics (hit rate, tokens saved, provider breakdown, 24h trend) | `analytics/CacheHealthTab.tsx` | Overview cache hits/misses + prompt-cache summary; no dedicated page, no tokens-saved | PARTIAL | — | UNVERIFIED |
| Compression analytics | `analytics/CompressionAnalyticsTab.tsx` | One summary line on Overview only | MISSING | — | UNVERIFIED |
| Search analytics | `analytics/SearchAnalyticsTab.tsx` | No search | MISSING | — | UNVERIFIED |
| Evals | `analytics/page.tsx` Evals tab (verified) | None | MISSING | — | UNVERIFIED |
| Provider Stats (latency/performance) | `/dashboard/provider-stats` (route confirmed) | Per-provider p50 + uptime history; no stats page, no p99 | PARTIAL | — | UNVERIFIED |
| Telemetry p50/p95/p99 | `GET /api/telemetry/summary` | p50/p95 only | PARTIAL | — | UNVERIFIED |
| Time range filter | Logs page "Time Range (Last {hours})" | Only All/ok/failed seg; no time-range picker | PARTIAL | — | UNVERIFIED |
| Export (CSV/JSON) | Logs page Export + `logExport` utils | CSV export button exists; no JSON export | PARTIAL | — | UNVERIFIED |
| Log export destinations (scheduled, high-water mark) | `/dashboard/log-export` | Manual DB export only | MISSING | — | UNVERIFIED |
| Resilience settings page (queue/cooldown/breaker/wait budgets) | `/dashboard/settings/resilience` (route confirmed; fields per §2.3) | Cooldown page covers 2 modes only; no resilience settings page | MISSING | — | UNVERIFIED |
| CORS allowed origins | Settings → Security | None | MISSING | — | UNVERIFIED |
| Blocked providers from `/v1/models` | Settings → Security | None | MISSING | — | UNVERIFIED |
| IP filter section | Settings → Security `IPFilterSection` | None | MISSING | — | UNVERIFIED |
| MFA | Settings → Security `MfaSection` | None | MISSING | — | UNVERIFIED |
| PII masking card (in Security) | `PiiMaskingCard.tsx` in Security settings | Present as a full standalone Guardrails page (toggles wired via `POST /dashboard/guardrails`, :7700) — function present, placement differs; fold pending per item 296 | PARTIAL | — | UNVERIFIED |
| Feature flags grid | `/dashboard/settings/feature-flags` | None | MISSING | — | UNVERIFIED |
| Appearance (Light/Dark/System, palettes, branding) | `/dashboard/settings/appearance` | Light/dark toggle only; no System, no palettes, no branding | PARTIAL | — | UNVERIFIED |
| AI Settings (thinking budget, system prompt, etc.) | `/dashboard/settings/ai` | None (mostly theirs-specific) | MISSING | — | UNVERIFIED |
| Modality Bridge settings | `/dashboard/settings/modality-bridge` | None | MISSING | — | UNVERIFIED |
| Cache settings UI (enable, threshold, TTL, engine, embedding provider/model, test) | `/dashboard/settings/cache` | Backend-configured only; no Settings UI for TTL/threshold | PARTIAL | — | UNVERIFIED |
| Sidebar customisation (presets, pinning, reorder) | `/dashboard/settings/sidebar` | None — deliberate simple nav, do not copy sprawl | MISSING | — | UNVERIFIED |
| Storage: DB stats, backups/restore, retention days, vacuum, export/import, reset usage | `/dashboard/settings/general` | DB export/import/wipe exist (Route guards + danger zone); no backups/restore UI, no retention/vacuum UI | PARTIAL | — | UNVERIFIED |
| Advanced (payload rules, body limit, model aliases, lockout card) | `/dashboard/settings/advanced` | None | MISSING | — | UNVERIFIED |

Dropdown inventory (Area 8b, verified fresh): 15 dropdowns in web/index.html, 11/15 fully
styled per design system; 4 still native-arrow (model provider filter, model category filter,
per-provider add-key role, intent-tier effort). #11 (Listen address) is styled but fake
(no handler). Hardcoded fake Models nav badge `128` FIXED (live-bound). `code` strategy
rejected by config RE-VERIFIED fresh (isStrategyName accepts only balanced, cheapest, cheap,
fastest, fast, most-reliable, reliable, least-latency, latency, weighted).

### Area 9 — MCP server / A2A server (42 total: FULL 4, PARTIAL 7, MISSING 27, WRONG 0, AHEAD 2, DIVERGENCE 2; comparable 38; strict 10.5%, weighted 19.7%)

OmniRoute tool count 110 CONFIRMED (independent regex over open-sse/mcp-server/; runtime
≥110 with perplexity + dynamic skill_* tools). 33 tool scopes CONFIRMED. Codvian MCP server:
3 tools — router_status, provider_list, model_list. A2A: zero.

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| MCP stdio transport | startMcpStdio() (server.ts:1039-1070); omniroute --mcp | mcp.Serve(), 1 MiB line cap; codvian-router mcp | FULL | internal/mcp/mcp.go; cmd/codvian-router/doctor_configure_mcp.go:403,518-519 | UNVERIFIED |
| MCP SSE remote transport | httpTransport.ts:8-9,405-435 — GET/POST /api/mcp/sse | None — stdio only | MISSING | — | UNVERIFIED |
| MCP Streamable HTTP remote transport | httpTransport.ts:9,393-403 — POST/GET/DELETE /api/mcp/stream | None | MISSING | — | UNVERIFIED |
| Tool breadth | 110 canonical (+1 perplexity + N dynamic skill_*) | 3 read-only tools | PARTIAL (3/110) | internal/mcp/mcp.go Tools() | UNVERIFIED |
| Routing/combo tools | omniroute_list/create/switch_combos, get_combo_metrics, set_routing_strategy, best_combo_for_task, explain_route, simulate_route, pick_fastest_model | None | MISSING | — | UNVERIFIED |
| Inference/spend tool | omniroute_route_request (real chat completion through routing) | None, deliberate — no sampling/chat tool, "a tool call can never spend quota" | MISSING | internal/mcp/mcp.go:1-22 | UNVERIFIED |
| Cost/quota tools | omniroute_cost_report, omniroute_check_quota, omniroute_set_budget_guard | None on MCP; per-request budget header exists only on HTTP chat | MISSING | — | UNVERIFIED |
| Cache tools | omniroute_cache_stats, omniroute_cache_flush (destructive) | None | MISSING | — | UNVERIFIED |
| DB health + pricing sync | omniroute_db_health_check (autoRepair:true rewrites DB rows); omniroute_sync_pricing | None | MISSING | — | UNVERIFIED |
| Memory tools | omniroute_memory_search/add/clear (memory_clear destructive) | None | MISSING | — | UNVERIFIED |
| Skills tools | omniroute_skills_list/enable/execute/executions, github_skills_*, agent-skills trio, dynamic skill_<name> | None | MISSING | — | UNVERIFIED |
| Plugin tools | plugin_list/install/activate/deactivate/uninstall/configure/executions/scan | None | MISSING | — | UNVERIFIED |
| Context-source tools | Notion 6, Obsidian 22 (incl. delete_note, move_note, execute_command), local-corpus 3 | None | MISSING | — | UNVERIFIED |
| Pool/gamification/compression/CCR tools | Pool 6, gamification 8, compression 5 + CCR 6 + RTK 2 | None | MISSING | — | UNVERIFIED |
| Tool search | omniroute_tool_search (toolSearch/register.ts, server.ts:671) | None (nothing to search at 3 tools) | MISSING | — | UNVERIFIED |
| Perplexity research tool | omniroute_perplexity_research, scope ["manage"] (server.ts:591) | None | MISSING | — | UNVERIFIED |
| Prompts / resources | None registered — tools only | None — tools only (initialize/tools/list/tools/call/ping only) | FULL (both tools-only) | internal/mcp/mcp.go | UNVERIFIED |
| Scope system (33 tool scopes) | 33 scopes verified fresh; per-tool scopes; * and trailing-* wildcard matching | No scope concept on MCP at all | MISSING | — | UNVERIFIED |
| Scope enforcement default | OFF by default (featureFlags.ts:127-136; MCP-SERVER.md:374,506) | No enforcement (nothing to enforce on 3 read-only tools) | MISSING | — | UNVERIFIED |
| Forced enforcement for narrow HTTP keys (#13330) | shouldForceScopeEnforcement() (scopeEnforcement.ts:120-124) | None (no remote transport, no per-key auth) | MISSING | — | UNVERIFIED |
| Manage-scope paradox | Verified fresh, still present (MCP-SERVER.md:370-374) | No equivalent — no manage scope, no disjoint scope namespaces on MCP | DELIBERATE-DIVERGENCE | — | UNVERIFIED |
| Per-client identity | HTTP/SSE: per-key api_keys.scopes via httpAuthContext.ts; stdio: none | Stdio also has no per-client identity (same gap, but only 3 read-only tools) | PARTIAL | internal/mcp/mcp.go | UNVERIFIED |
| Audit logging of tool calls | SQLite mcp_tool_audit, inputs SHA-256-hashed, outputs truncated to 200 chars (audit.ts:1-12) | No MCP audit log; only in-process guardrail evaluation counters | MISSING | internal/mcp/mcp.go | UNVERIFIED |
| Heartbeat / status telemetry | runtimeHeartbeat.ts; getMcpHttpStatus() | GET /dashboard/mcp feed: same 3-tool list, live provider/model counts, selector state + counters; no heartbeat, no session tracking | PARTIAL | internal/server/mcp.go | UNVERIFIED |
| Tool-cardinality controls | reduceToolManifest() supports allowScopes + maxTools + allow/deny, but env exposes only MCP_TOOL_DENY / MCP_TOOL_ALLOW | Server announces all 3 always; client side exceeds OmniRoute: selector mode injects exactly 2 meta-tools | PARTIAL | internal/mcptools/mcptools.go MetaTools() | UNVERIFIED |
| Description compression | Lossy Caveman ruleset applied to tool/prompt/resource descriptions by default (opt-out via OMNIROUTE_MCP_COMPRESS_DESCRIPTIONS) | None — descriptions served byte-exact | DELIBERATE-DIVERGENCE | — | UNVERIFIED |
| Spend controls on MCP tools | route_request schema carries optional budget USD (schemas/tools.ts:342); handler enforcement not traced (unverified) | Server: no spend possible by construction. Client: mcp_tools_execute has no spend cap, cost estimate or allowlist | PARTIAL | internal/mcptools/mcptools.go | UNVERIFIED |
| Guardrail on tool calls | Zero matches for "guardrail" anywhere in open-sse/mcp-server/ | tool_args phase: tool name + JSON args scanned before dispatch, blocked → JSON-RPC -32001 | AHEAD | internal/mcp/mcp.go WithGuardrail | UNVERIFIED |
| MCP client side (consume external MCP servers) | No MCP-client package found — server-side only | Real stdio JSON-RPC 2.0 client (30 s/call timeout, selector mode, agent loop cap 10 default / max 50) | AHEAD | internal/mcptools/stdio.go, mcptools.go | UNVERIFIED |
| Dashboard MCP page/feed | /dashboard/mcp + Endpoint tabs, MCP Audit page, transport toggles, "110 tools, 33 scopes" | GET /dashboard/mcp + "MCP & CLI" section incl. selector-mode card, audit view and transport toggles (POST /api/mcp/status); 24 tools, 11 scopes | PARTIAL | internal/server/mcp.go | UNVERIFIED |
| Protocol version | SDK-negotiated; doc example 2025-03-26 (MCP-SERVER.md:72) | 2025-03-26 — current surface; neither side ships sampling/elicitation | PARTIAL | internal/mcp/mcp.go ProtocolVersion | UNVERIFIED |
| C1 — mcptools stdio timeout nil-pointer panic | N/A | FIXED: nil-safe killLocked() on all startLocked error paths; callLocked captures stdin/stdout as locals (Wave 1, M1.8) | FULL | internal/mcptools/stdio.go | TestStdioServerSlowHandshakeTimeoutDoesNotPanic |
| C2 — mcptools 64-char collision infinite loop | N/A | FIXED: deterministic "__<n>" suffix truncated to 64 chars, bounded 10,000 tries + SHA-256 fallback; 1,000 colliding names terminate unique (Wave 1, M1.8) | FULL | internal/mcptools/mcptools.go | TestDiscoverThousandColliding64CharNamesTerminates |
| A2A JSON-RPC endpoint | POST /a2a: message/send, message/stream (SSE), tasks/get, tasks/cancel + v1.0 aliases (a2a/route.ts:217-337) | None — no /a2a route, no JSON-RPC agent endpoint | MISSING (deliberate, Phase 1 exclusion) | — | UNVERIFIED |
| A2A skills | 6 handlers: smart-routing, quota-management, provider-discovery, cost-analysis, health-report, list-capabilities | None | MISSING (deliberate) | — | UNVERIFIED |
| A2A REST task routes | GET/POST /api/a2a/tasks, GET /api/a2a/tasks/[id], cancel, history, status | None | MISSING (deliberate) | — | UNVERIFIED |
| Agent Card / discovery | GET /.well-known/agent.json (public, max-age=3600) + agent-card.json | None | MISSING (deliberate) | — | UNVERIFIED |
| A2A auth | Keyless by default — verified fresh (authenticate.ts); GHSA-v54m-6rm3-p565, GHSA-jcm5-6wpp-wjj8 | None — no A2A surface to mis-authenticate | MISSING (deliberate; keyless default must never be copied) | — | UNVERIFIED |
| A2A owner scoping | Owner = SHA-256 prefix of API key; ownerless tasks visible to everyone, by design | None | MISSING (deliberate) | — | UNVERIFIED |
| Conductor delegation | POST /api/a2a/tasks delegates coding work to OmniConductor fleet on server-side token | None | MISSING (deliberate) | — | UNVERIFIED |
| A2A task persistence / states / streaming | SQLite a2a_tasks + a2a_task_events; submitted → working → completed \| failed \| cancelled | None | MISSING (deliberate) | — | UNVERIFIED |
| A2A enable flag + dashboard/audit | a2aEnabled default false; /api/a2a/status; /dashboard/a2a; MCP/A2A audit pages | None | MISSING (deliberate) | — | UNVERIFIED |

### Area 10 — Compression, token counting (47 total: FULL 10, PARTIAL 14, MISSING 19, WRONG 0, AHEAD 4; comparable 43; strict 23.3%, weighted 39.5%)

Corrections: compression defaults OFF on their side (RTK→Caveman is the configured stacked
default, not on-by-default). 15 engine ids registered (not 12). 56 RTK filter packs.
59 Caveman en rule nodes, 11 locales. Savings % are NOT measured benchmarks (only empirical
anchor: one unit test at 95.93% on 300 identical lines). MCP description compression defaults ON.

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Engine registry / stacked pipelines | 15 registered ids, stackPriority ordering, stackedPipeline config, strategySelector.ts | Fixed 3-step pipeline only; no registry, no per-engine selection | MISSING | internal/compress/compress.go:1-540 | UNVERIFIED |
| Lite whitespace/format cleanup | Lite engine, <1ms, whitespace/dedup; tool-result truncation to cap only when opted in | Whitespace strip outside fences + system dedupe; no tool-result truncation cap | PARTIAL | internal/compress/compress.go | UNVERIFIED |
| Session-dedup (cross-turn, content-addressed, [dedup:ref sha=…]) | Engine, stackPriority 3, lossless | Only same-request duplicate system messages; no cross-turn store, no markers | MISSING | — | UNVERIFIED |
| CCR (retrieval markers, 24 h + SQLite) | Engine, stackPriority 4, lossless | None | MISSING | — | UNVERIFIED |
| RTK tool-output filter (56 JSON packs, ANSI/progress strip, failures+tails kept) | Engine, levels minimal/standard/aggressive, "60–90%" claim | Built + wired into Apply via mode=rtk / mode=stacked (explicit mode only; a bare lossy request stays extractive): FilterRTKMessages (rtk.go:811), 18 packs implemented (of OmniRoute's 56), ANSI/progress strip, failures+tails kept (first pass line kept as representative). Not full: standard level only in Apply (no per-request level), no live upstream verification | PARTIAL | internal/compress/rtk.go; internal/compress/compress.go | TestApplyModeRTKRunsFilter, TestApplyModeStackedRunsPipeline |
| Codex Responses tool-output compression | Engine, stackPriority 12, lossless-first | None | MISSING | — | UNVERIFIED |
| Headroom tabular JSON → columnar (SmartCrusher, lossless) | Engine, stackPriority 15 | None | MISSING | — | UNVERIFIED |
| Ionizer tabular row sampling | Engine, stackPriority 13 | None | MISSING | — | UNVERIFIED |
| Relevance extractive sentence scoring | Engine, stackPriority 18, preserve-regex | None | MISSING | — | UNVERIFIED |
| Caveman rule-based prose condensation (levels lite/full/ultra, user-role default, min 50 chars) | Engine, stackPriority 20, 59 en rule nodes, 11 locales | Built + wired into Apply via mode=standard / mode=stacked (explicit mode only): CondenseMessages (caveman.go:161), levels lite/full, user-role default, min 50 chars, extractive + rule-based only, no model calls. Not full: no ultra level, no per-request level, English-only rules (no 11 locales), no live verification | PARTIAL | internal/compress/caveman.go; internal/compress/compress.go | TestApplyModeStandardRunsCaveman |
| Aggressive summarize + progressive aging (~50% claim) | Engine, stackPriority 30; summarizer.ts:198 emits [COMPRESSED:summary] | Oldest-turn dropping only, no summariser (honestly documented) | MISSING | internal/compress/compress.go | UNVERIFIED |
| LLMLingua-2 ONNX semantic pruning (~57 MB TinyBERT, fail-open) | Engine, stackPriority 35, worker thread | None | MISSING | — | UNVERIFIED |
| Ultra heuristic pruning + code thinning + binary-search truncation (~75% claim) | Engine, stackPriority 40 | None (lossy truncation is not Ultra) | MISSING | — | UNVERIFIED |
| OmniGlyph context-as-image (Claude Fable 5 only) | Engine, stackPriority 90, experimental | None — deliberate do-not-build | MISSING | — | UNVERIFIED |
| LLM compressor (model call to compress) | Engine, stackPriority 38, opt-in default-off | None — deliberate do-not-build | MISSING | — | UNVERIFIED |
| Read-lifecycle engine | Engine, stackPriority 5 | None | MISSING | — | UNVERIFIED |
| MCP tool/prompt/resource description compression, default ON | types.ts:429 (true); applied at registration (mcp-server/server.ts:258-310) | None — we never rewrite tool descriptions, so we do not carry this correctness risk | MISSING | — | UNVERIFIED |
| MCP accessibility tool-result filtering (smartFilterText, truncation hints) | engines/mcpAccessibility/index.ts, server.ts:265-285 | Only last-per-tool_call_id canonicalisation | MISSING | internal/compress/compress.go canonicaliseToolResults | UNVERIFIED |
| Stacked pipeline RTK → Caveman | stackedPipeline: [{rtk standard}, {caveman full}] in default config (enabled-only, default is off) | RunStacked wired into Apply via mode=stacked with DefaultAvailability (RTK+Caveman live; stale false/false fixed at merge): lossless → RTK → Caveman, per-stage accounting with inflation discard + fail-closed counting, fidelity/inflation gates on the final output. Not full: no live end-to-end verification through a request; no background pool caller | PARTIAL | internal/compress/pipeline.go; internal/compress/compress.go | TestApplyModeStackedRunsPipeline, TestRunStacked* (21/21 pipeline+pool under -race) |
| Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked) | COMPRESSION_GUIDE.md:35-126; autoTriggerMode: "lite" | Modes parsed + validated (ParseMode; server 400s an unknown mode naming the field) and wired into Apply: rtk/standard/stacked run their engines, aggressive/ultra run their available subset (extractive pair only), a bare lossy request stays extractive, a mode alone never enables lossy. Not full: PickCheapestFitting/AutoTrigger have no live call site — mode selection is explicit-only by design (a bare lossy opt-in must not gain prose rewriting); no live verification | PARTIAL | internal/compress/modes.go; internal/compress/compress.go; internal/server/server.go | TestApplyMode*, TestChatCompletionsCompressionInvalidMode |
| Adaptive budget dial + escalation ladder | Ladder session-dedup→rtk→headroom→lite→caveman→aggressive→ultra | PickCheapestFitting + EscalateFrom built + tested (modes.go), ladder lite→standard→stacked; NOT wired as the default — mode selection is explicit-only (see modes row). Row stays PARTIAL with this reason | PARTIAL | internal/compress/modes.go | TestDialCheapestFitting, TestEscalateFrom, TestEscalateFromRTKStart |
| Cache-aware compression (preserve system prompt, downgrade, prefixFreeze, QuantumLock) | cachingAware.ts, prefixFreeze.ts, preserveSystemPromptMode: "always" default | Apply-side seam built and toggle-verified (V-17, commit eee5cb5): ApplyOptions.FrozenPrefix + resolveFreeze, PrefixFreeze.FreezeChecker adapter, cache-aware downgrade + preserve-mode resolvers live in Apply. NOT live in production yet: zero production callers of Observe/FreezeChecker, maybeCompress and the server handler pass no FrozenPrefix; QuantumLock has no implementation. Follow-up: own a process-wide observer, call Observe once per request, pass FrozenPrefix in maybeCompress + server handler; QuantumLock needs its own scope decision. | PARTIAL | internal/compress/apply_options.go:74,146-150; internal/compress/compress.go:230,319,446; internal/degrade/cache_aware.go:274 | TestCacheAwareFrozenPrefix, TestCacheAwareFrozenPrefixDowngrades, TestPrefixFreezeFreezeChecker, TestPrefixFreeze*, TestResolveCacheAwareStrategy, TestResolvePreserveSystemPrompt — V-17: pass 2026-11-19T22:14Z |
| System-prompt preservation modes (always/whenNoCache/never) | types.ts:217-240, default always | System/developer always pinned in lossy; no configurability, no cache-conditional mode | PARTIAL | internal/compress/compress.go | UNVERIFIED |
| Hard-budget post-pass (compress to ≤ N tokens) | hardBudget.ts ("compress to ≤ N cl100k tokens") | Wired: post-pass runs after applyLossy when the count is still over budget (same transform class, so it also runs on the bare lossy path); the 4 real inflation cases (131→140, 131→137, 131→132, 565→571) now FIT via the post-pass (120/116/112/555, probe-measured); fidelity gate moved to the final lossy output, conditioned on lossyChanged. Honest when pinned messages alone don't fit | PARTIAL | internal/compress/compress.go; internal/compress/postpass.go | TestInflationRealCases, TestInflationGuardSkippedOnFidelityRefusal |
| Protection guarantees (code/URLs/JSON/identifiers) | "Always preserves" list; validation.ts refuses rewrites dropping code/URLs/headings/versions/ALL-CAPS ids | Code fences protected in whitespace step; part-carrying messages never partially rewritten; M7.2 fidelity gate WIRED into Apply — lossy cuts that damage URLs/JSON/code are refused and the original is sent (24 CheckLossy tests incl. 6 paren-URL regression tests; paren-URL hole F1 closed with balanced-delimiter extraction). Still PARTIAL: headings/version strings/ALL-CAPS ids not protected; detection floors (sub-8-char fragments, >4 KiB sparse windows) remain | PARTIAL | internal/compress/fidelity.go; internal/compress/compress.go | TestFidelityGateRefusesTruncatedParenURL, TestCheckLossyRefusesParenURLCutWithSurvivingTail |
| Fidelity gate / risk gate / validation | Opt-in fidelityGate/riskGate, validation.ts, fidelityGateStep.ts | VerifyLossless structural equivalence check (normal-form + idempotence, fail → send original) for lossless; M7.2 fidelity gate WIRED on the lossy path — CheckLossy(out, after) in Apply, refusal returns the original with the single step fidelity_gate_refused, served via server applyCompression; 24 tests, -race green, toggle-proven. Still PARTIAL: no risk-gate equivalent (no secrets/migrations/diffs detection — OmniRoute riskGate/riskGateStep.ts, riskPatterns.ts); inflation guard is a separate row (wired, FULL) | PARTIAL | internal/compress/fidelity.go; internal/compress/compress.go | TestFidelityGateRefusesTruncatedParenURL, TestFidelityGateServesIntactParenURL |
| Inflation guard (compressed output never costs more than input) | Pipeline emits `pipeline-inflation-guard:` validation warnings surfaced by the compression preview fallback path (preview/route.ts:314,332); no emitter found in src/ under that spelling, live-path enforcement unverified | CheckNoInflation(model, orig, comp): strictly after > before, fail-closed on counting error or Estimated; wired into Apply after the fidelity gate — violation returns the original with the single step inflation_guard_refused (never both refusal steps). The 4 real inflation cases (131→140, 131→137, 131→132, 565→571) now FIT via the HardBudget post-pass (120/116/112/555, probe-measured, M7 merge wave); refusal is proven by TestInflationRefusalShortMessage (23-token prompt, budgets 22 and 18 both refuse, toggle-proven) | FULL | internal/compress/inflation.go; internal/compress/compress.go | TestInflationRealCases, TestInflationRefusalShortMessage, TestInflationGuardSkippedOnFidelityRefusal |
| Worker pool / async compression | compressionWorkerPool.ts, compressionWorker.ts, ONNX worker | Built + unit-tested 10/10 under -race and independently probed (Submit mutex fix toggle-proven by TestPoolSecondBlockedSubmitterCancel; cancel, shutdown drain, goroutine baseline clean). NOT wired: no genuine background consumer exists (all go func background sites checked — relay, tunnel, panel fan-out, ratelimit, hedging — none handles prompts for pre-compression); wiring it onto the request path would add latency and violate background-only. Wire it when a real background consumer lands | PARTIAL | internal/compress/workerpool.go | TestPool* (10/10 under -race) |
| Eval harness + measured savings corpus | compression/eval/, harness/ (benchmark, replay, budgetGate); one verified 95.93% test | Eval harness internal/compress/evalharness/ (harness + fixed corpus + goldens; 8 tests, -race green, toggle-verified) + measured table docs/compression-savings-2026-11-18.md (13 cases; lossless 118/1,200 saved 9.8%; lossy 480/1,200 40.0%; no case inflated; lossy misses budget on 7/13). Still PARTIAL: no benchmark/replay/budgetGate equivalent, no eval watch UI | PARTIAL | internal/compress/evalharness/; docs/compression-savings-2026-11-18.md | TestHarnessFailsOnTamperedNumbers, TestGoldenInputTokens |
| Output styles (Terse prose, Less code, Ponytail, "I have ADHD", Terse CJK) | 5 styles (outputStyles/catalog.ts:53-301) | None | MISSING | — | UNVERIFIED |
| Language packs for rules (11 locales) | rules/{de,en,es,fr,hu,id,it,ja,pt-BR,ru,zh}/ | None (English-only logic; no prose rules at all) | MISSING | — | UNVERIFIED |
| Disclosure: headers + ledger + trace | X-OmniRoute-Compression: <mode>; source=<source>; per-request stats + analytics by mode/engine | X-Codvian-Compression (lossless/lossy/lossless+lossy) + saved/prompt-tokens headers + lossy flag; ledger columns + route trace | FULL | internal/server/compression.go:41-90 | UNVERIFIED |
| Safe defaults (lossy opt-in only) | Compression off by default; lossy engines disabled unless enabled — except MCP description compression defaults ON | Lossless off unless requested or model-listed; lossy only via explicit per-request lossy_max_prompt_tokens, never from config | FULL | internal/compress/compress.go Decide; internal/config/config.go:328-334 | UNVERIFIED |
| Exact BPE counting (offline) | js-tiktoken, cl100k/o200k, cached encoders; exact only ≤8,000 chars | Vendored BPE tables embedded via go:embed, offline by construction; Count exact BPE | FULL | internal/tokencount/tokencount.go | UNVERIFIED |
| Per-model encoding selection | Only Codex→o200k, everything else cl100k; never errors | Only vendored cl100k/o200k ever returned; legacy models → cl100k_base, Fallback=true, never errors (Wave 1, M1.9) | FULL | internal/tokencount/tokencount.go | TestCountLegacyModelEncodingFallback |
| Native non-OpenAI tokenizers (Claude/Gemini/Qwen) | None — cl100k/o200k only | None — cl100k/o200k only, Fallback=true label | MISSING | — | UNVERIFIED |
| Array-form message counting | n/a (their counter takes extracted text) | Parts authoritative when set; string vs array content give equal counts (Wave 1, M1.9) | FULL | internal/tokencount/tokencount.go | TestCountPromptArrayFormEqualsStringForm |
| Tool definitions counted | Context-guard estimator is chars/4 — tool-schema JSON density diverges up to ~5×; mitigated only by learned EMA | CountPromptWire BPE-counts the tools JSON as sent; post-fix audit residuals ≤5.01% mean | AHEAD | internal/tokencount/tokencount.go; docs/token-count-audit.md:133-186 | token_audit_test.go |
| Per-message framing counted | No framing model in the estimator (chars/4 + calibration) | OpenAI cookbook 3/message + 3 priming; native wires +1 role token/message; audit residuals 0.00% on OpenAI non-tool classes | AHEAD | internal/tokencount/tokencount.go | token_audit_test.go |
| Cached-token accounting (read/write are subsets of input, never double-counted) | tokenLimitCounter.ts:65-69 (explicit double-count warning) | Ledger CachedTokens/CacheWriteTokens columns, same subset rule | FULL | internal/ledger/ledger.go:144-190,632-636 | UNVERIFIED |
| Own-cache tokens_saved accounting | semanticCache.ts:59,368,389 — tokens_saved per entry + cache_metrics table | Own exact/semantic hits record zero tokens: degrade.go:710 ("the ledger … records nothing") — verified fresh | MISSING | internal/degrade/degrade.go:710 | UNVERIFIED |
| Large-input bound / DoS safety | MAX_EXACT_TOKEN_COUNT_CHARS=8000, run/non-ASCII caps, falls back to chars/4 estimate | Exact BPE capped at 4 MiB, above that chars/4 with Estimated=true, never an error; chunked counting bounds O(n²); 1 MiB realistic prose 333–623 ms, drift 0.000% (Wave 1, M1.9) | FULL | internal/tokencount/tokencount.go | TestCountOneMBCompletesQuickly, TestCountChunkedMatchesUnchunked |
| Base64 image data-URI exclusion | Strips data:image/*;base64,… before counting | A data URI inside a text string is deleted before counting (M7.9, messageSegments); image parts were already skipped. Count (full counter) still counts the URI — honest scope, the exclusion is the prompt-estimate path | FULL | internal/tokencount/tokencount.go | TestCountPromptStripsDataURIInText, TestCountPromptStripsMixedCaseDataURI, TestCountStillCountsDataURIInFull |
| Runtime calibration vs provider-reported usage | EMA per (provider, model, tools) bucket, α=0.25, ≥3 samples, clamped [0.15, 2.0] | Built + wired (M7.9 + merge wave): calibrator (calibration.go) matches OmniRoute's shape exactly (α=0.25, ≥3 samples, clamp [0.15, 2.0], 100-token floor); reconcileUsage records the UNCALIBRATED local vs the reported count (toggle-proven: feeding the calibrated local decays the factor 1.2→1.1087 and fails TestReconcileUsageFeedsCalibration); calibrated local used for fallback/divergence/stream fallback. Not full: no live provider verification yet | PARTIAL | internal/tokencount/calibration.go; internal/router/router.go | TestReconcileUsageFeedsCalibration, TestCalibration* |
| Count memoization / perf | Encoder cache only (2 encoders in a Map) | Count memo: SHA-256-keyed, 4,096 entries; M7.9 removed the duplicate SHA-256 + memo lock per cold count (countExactUncached) and added hit/miss counters (memoHits/memoMisses, audit-measured) | AHEAD | internal/tokencount/tokencount.go | TestMemoAuditMeasured |
| Reasoning-token accounting | Reasoning is a subset of output unless reported separately | Ledger ReasoningTokens tracked | FULL | internal/ledger/ledger.go:545 | UNVERIFIED |
| Measured accuracy audit (per wire × class, enforced gates) | No equivalent published audit found | docs/token-count-audit.md: 18-case corpus × 5 wires, post-fix every cell mean error ≥ −5% and max |error| ≤ 20%, enforced by test | AHEAD | docs/token-count-audit.md | internal/router/token_audit_test.go |

### Area 11 — Analytics, usage, ledger, cost (28 total: FULL 16, PARTIAL 7, MISSING 5, WRONG 0; strict 57.1%, weighted 69.6%)

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Usage recording per key / provider / model / endpoint | usage_history/usageEvents per key+provider+model+connection; endpoint-specific accounting modules | usage_events schema (gateway_key_id, provider_key_id, provider, model, endpoint, latency_ms, ok, shared); single write path Engine.recordAttempt | FULL | internal/ledger/ledger.go; internal/router/router.go:2357-2372 | UNVERIFIED |
| Usage attribution per provider key (multi-key pool) | Per-connection rows; allowedConnections, per-connection quotas | ProviderKeyID on every row; "spend attributed per key, never pooled" | FULL | internal/ledger/ledger.go; cmd/codvian-router/main.go:758 | UNVERIFIED |
| Session / project / client attribution + /v1/me/sessions | agentSessions.ts:29-36,152-180; GET /v1/me/sessions and /[id] | No session/project/client field anywhere in ledger or server; no /v1/me/sessions | MISSING | — | UNVERIFIED |
| Token-split recording (prompt / completion / cached / reasoning / cache-write) | costCalculator.ts:247-274 legs incl. cache-read, cache-creation, reasoning | All five legs stored per row; cached/reasoning/cache-write clamped to their parent split | FULL | internal/ledger/ledger.go:144-190 | UNVERIFIED |
| Local-estimate vs reported split + drift measurement | Not found in sampled files | LocalEstimate + LocalEstimatePromptTokens per row; drift computable from live data | FULL (AHEAD) | internal/ledger/ledger.go; internal/provider/client.go:747-779 | internal/ledger/drift_test.go |
| Cost computation (tokens × price, all legs) | computeCostFromPricing (costCalculator.ts:247-274): input/output/cache-read/cache-creation/reasoning, incl. Flex 0.5× | Prices.Cost prices prompt + completion only; no cached-input leg, no per-endpoint price dimension | PARTIAL | internal/ledger/ledger.go:110 | UNVERIFIED |
| Gateway-reported cost precedence / exact-cost reconciliation | calculateCostDetailed (costCalculator.ts:354-359): provider-reported exact cost wins; RecordedCostMatcher reconciles | provider.Usage has no cost field at all; cost is always hand-computed; no reconciliation target | MISSING | internal/provider/client.go:747-779 | UNVERIFIED |
| Price provenance; unknown ≠ free; free = $0 | Resolution order user > models.dev > LiteLLM-synced (opt-in) > hardcoded defaults; unpriced → priced:false, never $0 | Operator-configured only + opt-in models.dev sync (OFF default), operator always wins; price_known flag separates free-$0 from unknown; unknown spend reported separately, never $0 | FULL | internal/config/prices.go; internal/ledger/ledger.go:110-131; cmd/codvian-router/main.go:1089+ | UNVERIFIED |
| Price coverage out of the box | Synced models.dev + LiteLLM catalogues fill prices automatically | No LiteLLM sync, no hardcoded defaults; unpriced model = unknown, cost routing blind until operator prices it | PARTIAL | internal/config/prices.go | UNVERIFIED |
| Flat-rate / subscription $0 treatment | flatRateProviders.ts (Codex, Claude Code, Kimi-coding, etc.) + flatRateAsZero opt-in | No flat-rate/subscription classification anywhere; a subscription provider with no configured price is "unknown", never $0 | MISSING | — | UNVERIFIED |
| Cache-write tier accounting (5-min vs 1-hour) | cacheWriteEstimate.ts:55-56 (5m = 1.25×, 1h = 2×); cacheHealth.ts:239-240,279-305. Caveat: "Diagnostic-only… Never used by calculateCost, quotas, budgets or deductions" | Single undifferentiated CacheWriteTokens total; dashboard economics uses a 1.25×–2.5× band (high end exceeds Anthropic's documented 2× cited in the code's own comment) | PARTIAL | internal/ledger/ledger.go:185-190; internal/server/server.go:1895-1896,2151-2174 | UNVERIFIED |
| Cached-input pricing inside ledger cost | Cached-read leg priced at cached rate in computeCostFromPricing | ModelPrice gained CachedPromptPerToken + CachedKnown; buildLedgerPrices carries the rate; ledger cost == dashboard cost for cached tokens (Wave 1, M1.6) | FULL | internal/ledger/ledger.go; cmd/codvian-router/main.go:1094 | TestBuildLedgerPricesCarriesCachedInputRate, TestRecordUsageCachedTokensGolden |
| Refused / fallback-attempt billing | UNVERIFIED on their side (no "refused" handling found in sampled files) | Every attempt recorded: success rows carry real tokens+cost; failed attempts recorded as ok=false zero-token rows. Blemishes: hedge loser recorded as a failed attempt though cancelled; a refusal the upstream actually billed for is invisible — no reported-cost field | PARTIAL | internal/router/router.go:2666,2782,3881,3990,4373,4416; cmd/codvian-router/main.go:701-746 | UNVERIFIED |
| Coalesced / shared-request billing | Dedup per-key isolated, temp ≤ 0.1, 1,000 in-flight cap | Leader's row carries real tokens+cost; waiter rows always zero tokens/zero cost; spend can never be double-counted | FULL | internal/ledger/ledger.go; internal/ledger/async.go:188 | UNVERIFIED |
| Committed vs settled spend in budget gauges | PARTIAL on their side — team-level only: soft_committed_usage (teamUsageLimits.ts:17,117-244) | FULL — Pre-send estimate reserved as committed, settled after the usage row lands; race-proof for per-key AND cumulative caps (M5.14) | FULL | internal/ledger/reservation.go:57 | TestReserveSpendParallel, TestReserveSpendParallelCumulativeCap, TestCumulativeBudgetRaceParallelChat |
| Per-response cost / token / latency headers | Single choke-point attachOmniRouteMetaHeaders (omnirouteResponseMeta.ts:129-224) | Same choke-point writer as Area 5 (M2.1): full cost/token/latency header set on every response path incl. ledger-backed cost | FULL | internal/server/headers.go:360 | TestResponseHeadersChatNonStream |
| Cost-saved reporting (cache / compression savings) | X-OmniRoute-Cost-Saved on cache hits; tokens_saved per cache entry + cache_metrics table | Compression saved tokens recorded per row; cache-read-vs-full economics in dashboard feed — but own exact/semantic cache hits record zero tokens | PARTIAL | internal/ledger/ledger.go | UNVERIFIED |
| Budgets: rolling 5h / daily / weekly per key | FULL — all three per-key fields (apiKeyUsageLimits.ts:34-48) | FULL — Rolling 5h + calendar daily/weekly/monthly windows (UTC, resetTime), one active window enforced with fallback | FULL | internal/ledger/budgetwindow.go:152; internal/ledger/keybudget.go:339 | TestKeyBudgetWindowedEnforcement, TestKeyBudgetActiveWindowFallback |
| Unpriced-usage policy + reporting | fail_closed default / count_as_zero; unpricedUsage.ts admin report | Same policy pair, same default; unknown events/tokens/unpriced-models surfaced in /v1/me/status | FULL | internal/ledger/keybudget.go; internal/server/keystatus.go:57-106 | UNVERIFIED |
| Per-key token limits (per model / provider / global) | Per-(key,model) caps, hourly, 429 model-cap (quota/enforce.ts:142-186) | FULL — Scoped model/provider/global limits with resetInterval/resetTime; most-restrictive breach wins; engine skips breached targets | FULL | internal/ledger/tokenlimit.go:71 | TestCheckKeyTokenLimitsScopes, TestEngineTokenLimitSkipsBreachedTarget |
| Quota-share pools / team cost centers | fairShare.ts pools; team_cost_centers table + teamUsageLimits.ts | FULL — Fair-share pools (weight/cap/policy, saturation, absolute cap) + teams with 1d/7d/30d list-price budgets; enforcement + API (dashboard UI is M10) | FULL | internal/ledger/pool.go:198; internal/ledger/team.go:394 | TestCheckPoolFairShareGenerous, TestTeamBudgetGateBlocksChat |
| GET /v1/me/status self-service usage | FULL — daily/weekly USD + requests/tokens, list-price, limits, account quotas | FULL — Today/week totals, tokens totals, daily/weekly limits, list-price cost, account quotas and team present (M5.5/M5.6/M5.12); per-window total REQUEST counts now live (M8.11: meStatusUsageWindow.TotalRequests, internal/server/keystatus.go:102, accumulated w.TotalRequests += sl.Rows in usageWindow's slices loop, :224; wired merge wave) | FULL | internal/server/keystatus.go:249 | TestMeStatusUsageWindows, TestMeStatusAccountQuotas, TestMeStatusTotalRequestsFixture |
| Metrics endpoint + latency percentiles | GET /api/telemetry/summary claimed p50/p95/p99 per provider — UNVERIFIED this pass | Prometheus /metrics, gateway-key auth; Collector emits per-(provider,model) codvian_router_request_latency_ms quantiles 0.5/0.95/0.99 plus codvian_router_cost_usd_total (ledger-backed), error classes, fallback counts, cache hit ratio, breaker state (M2.3) | FULL | internal/observability/collector.go:440-454,506-508; internal/server/server.go:1288 | TestCollectorPercentilesP50P95P99, TestCollectorTokensAndCost |
| Analytics endpoints | FULL (existence-verified) — /api/analytics/auto-routing, /compression, /diversity + /api/usage/* suite. Route contents not read | /dashboard/usage, /dashboard/key-budgets, /dashboard/prices + GET /api/analytics/summary, /by-provider, /by-model, /costs live (M8.17, wired merge wave); still no auto-routing, compression, or diversity counterpart, no combo-health/utilization surface | PARTIAL | internal/server/analytics_api.go | TestAnalyticsSummaryValues, TestAnalyticsCostsComplete |
| Retention + export | Retention configurable, 7-day default fallback + startup cleanup; log export incl. BigQuery destination | No retention/TTL on usage_events — grows unbounded; only manual wipe/reset. Export = DB-export bundle capped at 500 recent events, typed-confirm gated | PARTIAL | internal/ledger/ledger.go:1366; internal/server/dbexport.go:46,129 | UNVERIFIED |
| Usage import (external events, dedup) | Not sampled this pass (their import surface not checked) | ImportEvents with validation + dedup on event identity | FULL (ours; no OmniRoute comparison claimed) | internal/ledger/import.go:17,136-150,289-301 | UNVERIFIED |
| Idempotency-Key billing safety | UNVERIFIED — not sampled in OmniRoute code this pass | MISSING — zero hits in internal/ + cmd/; client retries after a timeout can double-charge | MISSING | — | UNVERIFIED |
| Batch / flex service-tier pricing | Flex priced at 0.5× in cost calc (costCalculator.ts:187-189); batch API not sampled | MISSING — no service_tier handling; no batch API, no batch/flex price tier in the price registry | MISSING | internal/relay/relay.go:521 | UNVERIFIED |

Unverified / open (Area 11, do not treat as findings): OmniRoute telemetry p50/p95/p99 and
refused-then-fallback billing not found in files sampled; OmniRoute idempotency and per-key
committed spend not sampled. Ours, router.go:4346: a failed streaming attempt records
streamFallbackUsage — whether that row can ever carry priced tokens was not fully resolved.

### Area 12 — Latency, startup, memory, binary (18 techniques: FULL 16, PARTIAL 0, MISSING 0, WRONG 0, DIVERGENCE 2; comparable 16; strict 100.0%, weighted 100.0%)

Measured-numbers table (rig: scripts/bench-geekflare.sh + cmd/codvian-bench, shared VM
1.5 shared vCPU, AMD EPYC 9D25, Go 1.27.4, instant local mock upstream, 1,000 sequential
requests for latency, 32 clients × 20 s for RPS; "clean" = direct-mock canary ≥12,000 RPS):

| Metric | Target | OmniRoute published | Ours (source + conditions) | Verdict |
|---|---|---|---|---|
| Added latency (median, p50 diff router − direct mock) | < 0.5 ms | None published | Latest clean run (global-budget, 299–300): 0.851 / 0.777 ms (docs/benchmark-2026-11-18-globalbudget.json, 2026-11-18 07:52 UTC, canaries 19,599/20,009 RPS). Key-budget run: 0.842 / 0.691 ms. Definitive re-measurement: 0.908 / 0.711 ms. Clean-run band Nov 18: 0.57–0.91 ms | MISSED on every clean run, ~1.7× over on the latest |
| RPS (32 clients × 20 s, ok/s) | > 3,000 | None published | Latest: 1,273 / 1,174. Key-budget run (best clean pair): 1,451 / 1,432. Definitive: 1,213 / 963. Clean-run band: 963–1,451 | MISSED, ~2.1–2.4× under on the latest |
| HTTP 502 under RPS load (limiter shedding) | — (no target) | n/a | Global-budget: 14.8% / 17.6%. Key-budget: ~14.1% / 13.3%. Definitive: 15.7% / 19.9%. E1 verdict: limiter-off clean pass 2 gave only +7.2% RPS with 0% 502 but p50 under load worse (9.3→11.0 ms) — the limiter is NOT the RPS ceiling; per-request CPU is | — |
| Idle memory (RSS) | < 70 MB | None published (only Docker RAM guidance: heap 1,024 MB / container ≥2 GiB light chat) | Latest: 42.1 MiB (43,064 KiB, globalbudget JSON). Key-budget 41.8 MiB; definitive 41.5 MiB; attempt-3 38.4 MiB. Idle crept 37→42 MiB as tree grew | MET (all runs) |
| Peak RSS under load | — (no target) | None published | Global-budget 78.2 / 108.8 MiB; key-budget 78.5/109.7 MiB; definitive 73.5/94.2 MiB. No leak — settled heap after load equals idle heap (19–22 MiB); retained heap dominated by one-time tiktoken BPE tables 11.8 MiB (57–60%) | — |
| Ready time (spawn → first /healthz 200) | < 10 s | None published | Global-budget 434.8 / 565.0 ms; key-budget 441/559 ms; definitive 464/643 ms; attempt-3 clean pass 125.6 / 249.3 ms | MET by ~16–20× |
| Binary size (single binary) | — | n/a — Next.js monolith, 3,111 TS files; no binary exists | 17.3 MB per docs/ARCHITECTURE.md:20 and CHECKLIST.md:550 (older snapshot); docs/BENCHMARK.md records 19 MB (2026-11-17) and 28 MB (E1 run). Quote as 17.3–28 MB range, not a single current figure | — |
| Docker image size | — | None published (Docker Hub link only) | 20.8 MB (codvian-router:local, CHECKLIST.md:75). Caveat: assembled by docker import, not a literal docker build (no Docker daemon in this VM) | — |
| OmniRoute idle Docker memory | — | — | ~541 MB: UNVERIFIED — do not quote | UNVERIFIED |
| Startup time ("12 ms") | — | None published | "12 ms": UNVERIFIED — no source found. Measured ready-to-/healthz is 125–643 ms (73 ms only in older ARCHITECTURE.md snapshot) | UNVERIFIED |
| LiteLLM added latency (reference point, NOT OmniRoute's) | — | n/a | 18.1 ms — Geekflare independent benchmark, 21 Oct 2026, 2-vCPU DigitalOcean droplet, LiteLLM 1.106.2, 55 RPS, 642 MB idle, 61 s ready. Our best clean added latency (0.578 ms before-run; 0.691 ms best recent pass) is ~26–31× lower — an edge over LiteLLM, not over OmniRoute, which publishes no number | Edge vs LiteLLM stands |
| Failover speed ("~53 ms 429 failover", "switch 1–2 ms") | — | None published | Both UNVERIFIED — do not quote. "~53 ms" appears only in deep-study §5.7 with no source file; "1–2 ms switch" is a router-overhead figure, not a switch time, and its cited source docs/INSTALL.md:137 does not exist in this clone | UNVERIFIED / WRONG as quoted |
| Top remaining per-request CPU costs (latest profile, global-budget run) | — | — | docs/cpu-profile-2026-11-18-globalbudget.md (39.31 s samples): syscalls 12.77% flat; GetKeyBudget 8.27% cum; CheckBudgetOrError/GetBudget 8.01%/7.96%; SQLite parse path ~7.5–7.8%; mallocgc 6.87%; semantic-cache getScoped 3.31%; guardrails.scan 3.00%. No single hot spot above ~9% cum — remaining cost is diffuse | — |

Latency-technique table:

| # | Technique | OmniRoute | Ours | Verdict | File:line | Test |
|---|---|---|---|---|---|---|
| 1 | Hedging (parallel 2nd attempt) | hedging: false, hedgeDelayMs: 500; bodies >256 KB never hedged | ON, HedgeDelay 150 ms; bodies >256 KB never hedged (M4.8), counting tool definitions + ToolChoice toward the 256 KiB budget (commit c1b215e). Loser cancelled, breaker-neutral; OFF on explicit-order chains. | FULL | internal/router/hedge.go:16,51-52; internal/router/router.go:4613,5998 | TestHedgeBodySizeGuard, TestHedgeBodySizeGuardBoundary, TestHedgeBodySizeGuardCountsParts, TestHedgeBodySizeGuardCountsTools — V-18: pass 2026-11-19T22:08Z |
| 2 | Warm HTTP/2 connection pools | None found (providerConnectionPool.ts is a credential pool, not an HTTP socket pool) | Warm pool per (provider, key), MaxIdleConnsPerHost sized by Little's Law, TFO dialer | FULL — AHEAD | internal/provider/pool.go:1,231-250,367-369 | UNVERIFIED |
| 3 | TCP Fast Open | None found | tfoDialer/tfoTransport, WithTCPFastOpen default enabled, graceful fallback | FULL — AHEAD | internal/provider/tfo.go:100-133; internal/provider/registry.go:335 | UNVERIFIED |
| 4 | TCP_NODELAY explicit | None found | Enforced/proven by real GetsockoptInt probes; no latency win claimed (unmeasurable on loopback) | FULL | internal/provider/nodelay.go:1,40-52 | UNVERIFIED |
| 5 | SSE batching (fewer flushes) | None found | Max 3 events or 8 ms per flush; in handleChatStream AND handleResponsesStream (scope correction: brief's "only in handleChatStream" is WRONG). Throughput trade (~3× fewer flushes), NOT a latency win — median worse, p99 tightens (25.5→9.9 ms) | FULL | internal/server/sse_batch.go:10-11,44-47; internal/server/server.go:5245,5292-5301; internal/server/responses.go:1327,1372-1381 | UNVERIFIED |
| 6 | Single-flight coalescing | requestDedup.ts — non-streaming only, temp ≤ 0.1 only, tenantId-prefixed, max 1,000 in-flight, 60 s timeout | Group.Do single-flight, non-streaming only; tenant-scoped key wired in live Engine.Chat; max 1,000 in-flight, 60 s flight timeout; temp in key. Divergences: cap runs newcomer uncoalesced (theirs evicts oldest); timeout fails the flight (theirs only deletes the map entry); tenant hashed (theirs plaintext); ours coalesces any temp (theirs ≤0.1 only) (M6.10) | FULL | internal/coalescing/coalescing.go; internal/router/router.go:5954 | TestGroupDifferentTenantsNeverShare, TestEngineCoalescesIsolatedPerTenant |
| 7 | Adaptive concurrency (Gradient2) | Static Bottleneck buckets: 60 RPM, 350 ms min gap, 6 concurrent; learns/persists upstream limits | Gradient2 per (provider, key): init 20 / min 2 / max 100, smoothing 0.2; fail-open; E1 proved it is not the RPS ceiling | FULL — AHEAD | internal/adaptive/limiter.go:1-14,41-58 | UNVERIFIED |
| 8 | Pre-commit stream holdback / failover before first byte | streamRecovery.ts — holdback 750 ms / 64 KB, OFF by default, early-retry max 4 | Always on, up to 8 events / 256 KiB / 5 s; "once first content byte is sent, stream NEVER switches" | FULL — AHEAD | internal/router/preread.go:8,18,70,77; internal/router/router.go:3502-3530 | UNVERIFIED |
| 9 | Latency-aware routing / scoring | 16-factor, latencyInv 0.1143, pool-relative; latencyAware strategy; telemetry p50/p95/p99 per target | Latency-aware strategy + p2c latency term live and toggle-verified: ModeLatencyAware (latency 0.70), P2CLatencyTerm, StrategyLatencyAware via HookFactory on the real request path. p99 + model-latency-stats endpoint are row :1040's scope (committed fd813a3, V-20 pending). | FULL | internal/scoring/scoring.go:236,263,523,548; internal/strategy/strategy.go:133,204,307; internal/strategy/orderings.go:153,177; cmd/codvian-router/main.go:2799 | TestLatencyAwareToggleOnOff, TestLatencyAwareModePicksLowerLatencyAtEqualCost, TestP2CLatencyTerm, TestPickP2CChoosesLowerLatency — V-15: pass 2026-11-19T22:09Z |
| 10 | Per-response latency/cost/token headers | One choke-point: Latency-Ms, Response-Cost (10 dp), Tokens-In/Out, Version + conditional fields | Choke-point writer emits the full set incl. Latency-Ms, Cost-USD (10 dp), Tokens-In/Out/Cached, Request-Id, Version, Fallback-Attempts, Tokens-Per-Second (M2.1) | FULL | internal/server/headers.go:360 | TestResponseHeadersChatNonStream |
| 11 | Latency percentiles in telemetry | p50/p95/p99 per provider (GET /api/telemetry/summary, model-latency-stats) | p50/p95/p99 now live on three surfaces: GET /api/telemetry/model-latency-stats (p99 per provider/model), provider history (p99_ms), GET /dashboard/overview (latency_p99_ms_since_start). Health probe ring 48 h / 2,880 samples. GET /api/telemetry/summary itself does not exist — equivalents cover the same data. | FULL | internal/health/health.go:357; internal/metrics/metrics.go:819; internal/observability/collector.go:396; internal/server/server.go | TestModelLatencyStatsHandler, TestHistoryReportsP99, TestHistoryP99NilWithNoSuccessfulProbes — V-20: pass 2026-11-19T22:24Z |
| 12 | Retry-After handling (latency edge) | Honoured verbatim as cooldown, clamped to 30 days; waits it out; parses JSON-body hints | Honoured, floored 10 s, capped 30 s, abandon target when ≥60 s, fail over immediately. Missing only JSON-body hints. Deliberate divergence — the latency edge | FULL — AHEAD | internal/provider/client.go:1217; internal/router/retryafter.go:79-83,92-102 | UNVERIFIED |
| 13 | Same-target transient retry (502/503/504, jittered backoff) | 502/503/504 only, max 3, decorrelated full-jitter, base 200 ms / cap 2,000 ms, worst ~7 s; 429 intentionally NOT retried here | Exp backoff base 200 ms ×2ⁿ, cap 5 s, jitter [0.5×,1×]; same-target 429 backoff also exists | FULL | internal/router/router.go:575-579,2228 | UNVERIFIED |
| 14 | Post-commit stream resume / continuation | Opt-in mid-stream continuation, min 8-char overlap | None — automatic resume-after-commit deliberately removed; explicit entry points only | DIVERGENCE | internal/router/router.go:4120-4132 | — |
| 15 | Stalled-stream throughput watchdog | 30 s warmup / 30 s window / 4 B/s | Throughput watchdog in forwardStream: 30 s warmup / 30 s window / 4 B/s; aborts pre-commit (failover) or post-commit (terminal) | FULL | internal/router/watchdog.go:37-45,85-108 | TestWatchdogAbortsStalledStream, TestWatchdogFailsOverPreCommit, TestWatchdogTrickleBelowFloor |
| 16 | Front-door admission control | Partial: queue-wait budget maxWaitMs 30,000 ms, execution backstop 600,000 ms, maxQueueDepth 0 = disabled, wedge watchdog | None — NOT built (DELIBERATE). E1 inconclusive; 502s are the limiter shedding load the CPU cannot serve | DIVERGENCE | docs/BENCHMARK.md (items 149–150) | — |
| 17 | Cache as a latency saver (exact + semantic) | Dual-layer (memory LRU + SQLite + optional Redis vector), temp = 0 only, per-key isolated, 30 min / 1 h TTL, 256 KB / 2 MB caps | Present and fast — exact 1 h TTL, semantic 1 h / threshold 0.85, plus stale-serving; temp-gated, per-key isolated, ScopedKey, truncated guard, SQLite persistence (M1.4, M6.2b, M6.3-fix, M6.11-fix). Semantic method remains hashed BoW, not neural (see Area 6) | FULL | internal/cache/cache.go; internal/degrade/degrade.go | UNVERIFIED |
| 18 | Measured failover-switch time (a number to quote) | None published | Measured 0.54–1.06 ms over 3 runs (cold PC re-run: 1.0554 ms, 545.9 µs, 542.6 µs), far under the 300 ms target — published in docs/INSTALL.md:142 with the honest qualifier "NOT end-to-end" + "VM localhost mocks, 3 runs". Test drives POST /v1/chat/completions through the server → router engine, dead provider 429s, fallback dispatches (fallback_path "dead/gate-model -> fallback/gate-model", fallback_attempts: 1). | FULL | docs/INSTALL.md:142 | TestChatCompletionsFailoverSwitchTime (3/3 PASS cold on PC; toggle: fallback removed → FAILS; V-B2-1047 pass 2026-11-20 ~13:16 IST (PC)) |

### Area 13 — Packaging, Docker, install, CLI (27 total: FULL 6, PARTIAL 12, MISSING 6, WRONG 0, AHEAD 3; comparable 24; strict 25.0%, weighted 50.0%)

Baseline corrections: binary is 20–22 MB, not 17.3 MB (dist/: linux/amd64 22,081,696 B
(21.1 MiB), linux/arm64 20,840,608 B; all six snapshot binaries 20–22 MB, stripped,
statically linked). OmniRoute clone version is 3.8.55 (past the 3.8.54 "latest" in the
baseline); "283 releases" was NOT re-verified this session. 0 releases, 0 git tags in the
worktree; dist/metadata.json says 0.0.0-SNAPSHOT-none; Version = "0.1.0" is only a code
constant. Docker image 20.8 MB is from the slice-4 report (assembled via docker import +
chroot — no Docker daemon in this VM), NOT re-measured; first literal docker run is
backlogged (LV-05/LV-06/LV-07).

| Feature | OmniRoute evidence | Codvian status | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Prebuilt binary install | npm install -g omniroute, bun add -g omniroute, npx -y omniroute (README.md:782–784,1041); Docker Hub diegosouzapw/omniroute | No release exists; only build-from-source works | MISSING | docs/INSTALL.md:3–14 | UNVERIFIED |
| Build from source | npm install && npm run dev; Makefile; flake.nix dev shell | git clone …; go build -o codvian-router ./cmd/codvian-router; Go 1.22+, 2 direct deps, CGO-free | FULL | docs/INSTALL.md:21–45 | UNVERIFIED |
| npm package | Published omniroute package, bin: {omniroute, omniroute-reset-password}; npm-publish.yml with OIDC Trusted Publishing + provenance | package.json exists (name: codvian-router, bin, version 0.1.0) + bin/codvian-router.js downloader wrapper — unpublished, downloads from releases that don't exist yet | PARTIAL | package.json; bin/codvian-router.js | UNVERIFIED |
| Published Docker image | Docker Hub diegosouzapw/omniroute:latest + :X.Y.Z + :next, ~250 MB, multi-arch amd64+arm64 | No published image; GHCR target ghcr.io/raunakiitp/codvian-router only in .goreleaser.yml dockers_v2 config. Local image 20.8 MB per slice-4 report only | PARTIAL | .goreleaser.yml | UNVERIFIED |
| Dockerfiles (local build) | Dockerfile (452 lines, node:26.12.1, stages base/builder/runner-base/runner-web/runner-cli(-no-docker), non-root USER node, healthcheck) + Dockerfile.bun (169 lines) | Dockerfile (Go 1.22 builder → distroless static-debian12:nonroot, inline healthcheck binary, ENTRYPOINT+CMD serve, EXPOSE 20128) + Dockerfile.release | FULL | Dockerfile; Dockerfile.release | UNVERIFIED |
| docker-compose | 4 files: docker-compose.yml (440 lines, profiles base/web + redis + browser), .prod.yml, .selfhost.yml, .ollama-local.yml | 1 file: single service, command serve --listen 0.0.0.0:20128, 2 named volumes, healthcheck. Parses; never executed in this VM | PARTIAL | docker-compose.yml | UNVERIFIED |
| Docker image size | ~250 MB (DOCKER_GUIDE.md:501) | 20.8 MB per slice-4 report (not re-measured — no daemon) | AHEAD | docs/slice-4-docker-report.md:11,190 | UNVERIFIED |
| Single-binary size / runtime weight | Node.js app; image pins 1 GiB heap default, coding-agent workloads need 8–12 GiB heap | Static stripped Go binary 20.8–22.1 MB, no runtime, no heap tuning needed | AHEAD | dist/ | UNVERIFIED |
| CLI breadth | 97 files in bin/cli/commands/, ~90 register* functions, 723 .command( call sites (sampled count, not a distinct top-level count) | 17 top-level subcommands: serve, init, wizard, run, keygen, eval, eval-feedback, validate, tray, doctor, configure, mcp, provider, catalog, verify-release, release, db, help (incl. default serve) | PARTIAL | cmd/codvian-router/main.go:515–552 | UNVERIFIED |
| --version / -v | Fast-path --version/-V before heavy imports (bin/omniroute.mjs:14–17,60–66) | No --version/-v flag and no version subcommand (only Version = "0.1.0" const, printed on serve/init startup line) | MISSING | cmd/codvian-router/main.go:74 | UNVERIFIED |
| Core lifecycle subcommands (serve/init/keygen/validate/doctor/configure) | serve, doctor, configure, restart/stop/status/health/logs | All present and wired: serve (default), init (+--key-file 0600, --wizard), keygen, validate, doctor, configure (interactive or --provider/--api-key[-env]) | FULL | cmd/codvian-router/main.go:1–27,515–552 | UNVERIFIED |
| Eval tooling | eval command + scripts/router-eval/ + scripts/compression-eval/ + eval watch TUI | eval (smoke-eval, --model) + eval-feedback (A/B feedback heuristic) — narrower, no eval framework/watch UI | PARTIAL | cmd/codvian-router/main.go | UNVERIFIED |
| MCP stdio server | --mcp flag + bin/mcp-server.mjs | mcp subcommand, stdio, tools router_status, provider_list, model_list | FULL | cmd/codvian-router/main.go:18,540 | UNVERIFIED |
| Launch/wrap external CLIs | run + launch + launch-codex + 18 setup-* commands (claude/codex/cursor/cline/kilo/roo/crush/zed/goose/aider/qwen/gemini/…) + fcc-claude | Single generic run <cli> --model MODEL [-- args...] (ephemeral headless gateway + fresh key) — no per-tool setup commands | PARTIAL | cmd/codvian-router/main.go:524 | UNVERIFIED |
| Provider management CLI | providers, provider-cmd, test-provider, provider-crud, models, model-crud, model-validation, combo, pricing | provider list/add/add-catalog/remove/import — functional but far narrower (no test-all, no model CRUD) | PARTIAL | cmd/codvian-router/main.go:3500–3615 | UNVERIFIED |
| Config file handling | .env + DATA_DIR/storage.sqlite, getDefaultDataDir(); settings store, encrypted columns | Single config.json at os.UserConfigDir()/codvian-router/config.json, --config override everywhere; vault secret + ledger in data dir; init refuses if config exists | FULL | internal/config/config.go:768–771 | UNVERIFIED |
| Tray | tray command (show/hide/quit) + bin/cli/tray/ (autostart, detached tray, Windows systray/PowerShell) + serve --tray; community OmniRouteTray macOS menu-bar via brew | tray subcommand exists but is only a headless supervisor (Start/Stop/Restart, auto-restart, no GUI icon, no menu, no autostart) | PARTIAL | internal/tray/supervisor.go:87–160 | UNVERIFIED |
| Desktop app (Electron) | Full electron/ app (main.js, preload.js, electron ^45, electron-builder ^26, build:win/mac/linux) + electron-release.yml | None — no Electron dir, no desktop build, no .app/.exe packaging | MISSING | — | UNVERIFIED |
| Self-update mechanism | update command, update-notifier, autoUpdate.ts, Electron autoUpdater | None. No self-update code; /admin/update is explicitly notImplementedAdmin("auto-update"). Only path is re-running scripts/install.sh after a future release | MISSING | internal/server/server.go:1236 | UNVERIFIED |
| Releases published | 283 releases per prior crawl (not re-verified); current dev version 3.8.55; electron-release.yml + npm-publish.yml + docker-publish.yml all trigger on v* tags | 0 releases, 0 git tags; release pipeline exists only as config (.goreleaser.yml, .github/workflows/release.yml — sole workflow file, runs on v*.*.* tag) | MISSING | .goreleaser.yml; .github/workflows/release.yml | UNVERIFIED |
| SBOM | CycloneDX SBOM for Docker image (docker-publish.yml:824–832) and npm (npm-publish.yml:266–292) | Configured: syft SPDX SBOM per archive (.goreleaser.yml sboms:) + dependency-free CycloneDX fallback (cmd/release-sbom, scripts/release-sbom.sh); snapshot dist/ contains 6 .sbom.json files (21–26 KB each). No published release, so live SBOM is UNVERIFIED | PARTIAL | .goreleaser.yml; dist/ | UNVERIFIED |
| Signing / provenance | No cosign/Sigstore signing found in searched workflows (SBOM + npm provenance only) | Designed ahead, unproven live: cosign keyless over checksums.txt + each SBOM (.goreleaser.yml signs:), docker_signs, SLSA provenance via actions/attest-build-provenance, scripts/verify-release.sh + verify-image.sh; local tamper proof 4/4 with throwaway keypair. Keyless path UNVERIFIED (Sigstore POSTs hang from this VM; no live release) | PARTIAL | .goreleaser.yml; docs/slice-209-210-report.md; docs/slice-221-222-report.md | UNVERIFIED |
| Multi-arch build | Published amd64+arm64 manifest; docker-publish.yml builds both | Configured: GoReleaser linux/amd64 + linux/arm64, setup-qemu-action v3.7.0 + setup-buildx-action v3.12.0 in release.yml; snapshot dist/ has all 6 platform binaries (3 OS × 2 arch). Real multi-arch image build UNVERIFIED — proof spec only (docs/ci-proof-multarch.md, gaps G3–G5+G9 open) | PARTIAL | .goreleaser.yml; .github/workflows/release.yml; dist/ | UNVERIFIED |
| Install script | n/a (npm/Docker are the installers; no standalone shell installer found) | scripts/install.sh (10.5 KB): OS/arch detection, refuses pipe-to-shell, cosign-verify of checksums.txt when cosign present, SHA-256 check, CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 strict mode — complete script, but downloads from GitHub releases that do not exist yet | PARTIAL | scripts/install.sh | UNVERIFIED |
| Extra install surfaces | AUR (yay -S omniroute-bin + systemd user service), Nix flake, Podman/Quadlet, Fly.io (fly.toml), Helm chart + K8s manifests, PWA, Termux/Android, VS Code OmniCopilot extension | None of these exist (no deploy/, fly.toml, flake.nix, PWA manifest, AUR/Nix packaging) | MISSING | — | UNVERIFIED |
| Release verification command | No equivalent CLI found (verification lives in CI workflows) | release verify <tag> (download + cosign + checksums + SBOM + attestation + image signature) + verify-release [DIR] (offline checksum check); snapshot self-check: checksums 12/12, archives 6/6, SBOM 6/6, signatures 0/7 as expected for unsigned snapshot | AHEAD | docs/release-verify-2026-11-18-snapshot.json | UNVERIFIED |
| In-image healthcheck | HEALTHCHECK CMD ["node","healthcheck.mjs"], 30s/5s/15s/3 (Dockerfile:330–331) | HEALTHCHECK via compiled Go helper binary against /healthz, 30s/3s/5s/3; healthcheck binary exit 0 proven in chroot smoke (slice-4 report) | FULL | Dockerfile; Dockerfile.release | UNVERIFIED |

### Area 14 — Plugins, API surface/endpoints, docs/README (90 total: FULL 16, PARTIAL 43, MISSING 28, WRONG 1, AHEAD 1, DIVERGENCE 0, UNVERIFIED 1; comparable 89; strict 18.0%, weighted 42.1%)

Scale (measured): OmniRoute 810 route files / 1,254 handlers (GET 482, POST 424, PUT 70,
PATCH 29, DELETE 114, OPTIONS 129, HEAD 6) vs ours 89 routes (53 GET, 33 POST, 3 DELETE —
50 /dashboard/* feeds, 10 /v1/*, 7 /api/*, 19 /admin/*, plus /healthz, /metrics,
/openapi.json). OmniRoute README 1,844 lines vs ours 179/260. OmniRoute docs 10,416 .md
(10,206 i18n + 210 English) vs ours 63. Their /v1 subtree: 119 route files / 221 handlers.

M8.0 reconciliation (2026-11-19) — 89-vs-88 finding: the comparison
report's Area 14 total of 89 comes from its §1a subtotal, "35 rows — 5
FULL, 2 PARTIAL, 28 MISSING", but its own §1a table lists only 34 rows
(5 FULL, 2 PARTIAL, 27 MISSING) — the report over-counted MISSING by 1
in its subtotal arithmetic. PARITY.md's §14a lists the same 34 rows, so
no row was dropped here and none is added: the correct Area 14 total is
88 (34 + 39 + 8 + 7), and the report's 89 is the error. Two further
header corrections from a mechanical recount of the tables as they
stand: MISSING is 48 pre-flip, not the header's 47 (27 + 16 + 4 + 1),
and DIVERGENCE is 0, not 1 — no Area 14 table row carries a DIVERGENCE
verdict; the old DIVERGENCE 1 was balancing the undercounted MISSING
(the §14c install row's "(deliberate design divergence…)" note is
counted MISSING in §14c, not DIVERGENCE). The §14b header was also
stale before M8.0: it claimed FULL 2 / MISSING 17, but the per-response
headers row was already flipped to FULL in M2.1 (FULL 3 / MISSING 16).
Post-flip (§14a FULL 5 / PARTIAL 4 / MISSING 25; §14b FULL 5 /
PARTIAL 19 / MISSING 14 / AHEAD 1; §14c 0 / 4 / 4; §14d FULL 2 /
PARTIAL 2 / MISSING 1 / WRONG 1 / UNVERIFIED 1): FULL 12, PARTIAL 29,
MISSING 44, WRONG 1, AHEAD 1, DIVERGENCE 0, UNVERIFIED 1 — the
UNVERIFIED row is the 14d test-headline row, called "unclassified" in
the headline note. Still stale, out of M8.0 scope: the "ours 89 routes" scale count above
predates the M5.6/M5.7/M6.8 routes added since the comparison — a
mechanical count of guardRouteTable today gives 106 routes (62 GET, 40
POST, 4 DELETE: 59 /dashboard/*, 12 /v1/*, 11 /api/*, 21 /admin/*, plus
/healthz, /metrics, /openapi.json); updating the scale paragraph
belongs to a later pass, not to this verdict reconciliation.
Also stale, out of M8.0 scope: Area 11 row 818's evidence clause
("no /v1/me/sessions") is now factually false — the endpoint is live
since M5.6 (guard.go:170) — not just its MISSING verdict stale; the
row needs a refresh when Area 11 is reconciled.

#### 14a. /v1 public API (36 rows: FULL 7, PARTIAL 17, MISSING 12)

| Endpoint | OmniRoute | Ours | Verdict | File:line | Test |
|---|---|---|---|---|---|
| POST /v1/chat/completions | api/v1/chat/completions/route.ts | guard.go Tier 3, server.go | FULL | internal/server/guard.go | UNVERIFIED |
| POST /v1/messages (Anthropic) | api/v1/messages/route.ts | POST /v1/messages, byte-for-byte relay | FULL | internal/server/guard.go | UNVERIFIED |
| POST /v1/embeddings | route.ts (18 providers per prior study) | POST /v1/embeddings | FULL | internal/server/guard.go | UNVERIFIED |
| POST /v1/rerank | api/v1/rerank/route.ts | POST /v1/rerank | FULL | internal/server/guard.go | UNVERIFIED |
| GET /v1/models (list) | api/v1/models/route.ts | GET /v1/models | FULL | internal/server/guard.go | UNVERIFIED |
| POST /v1/responses + GET/DELETE/list | responses/route.ts, responses/[...path], responses/input_tokens | POST + GET {id} + GET list + DELETE {id} + GET {id}/input_items (M8.4) + GET /v1/responses/input_tokens (M8.3); no [...path] catch-all sub-actions | PARTIAL | internal/server/guard.go | TestM84InputItemsFixture200, TestM83ResponsesInputTokensFixture |
| GET /v1/me/status | api/v1/me/status/route.ts | GET /v1/me/status — daily/weekly usage windows live (M5.6) alongside budget/traffic + account quotas; per-window total REQUEST counts now live (M8.11 — the gap this row cited is closed, Area 11 row 724 → FULL). Verdict held at PARTIAL: no reviewer assessment blessed FULL for this §14a row; needs a reviewer call | PARTIAL | internal/server/keystatus.go:249 | TestMeStatusUsageWindows, TestMeStatusAccountQuotas |
| POST /v1/completions (legacy) | HAVE | POST /v1/completions live (M8.2, wired merge wave): non-streaming prompt-string (or 1-element []string) → one user message, text_completion shape. Gaps: messages[] body → 400, multi-prompt arrays → 400, stream:true → 400, exactly 1 choice, no system_fingerprint | PARTIAL | internal/server/legacy_completions.go | TestLegacyCompletionsFixture200, TestLegacyCompletionsPromptBecomesUserMessage |
| POST /v1/multimodal-embeddings | HAVE | — | MISSING | — | UNVERIFIED |
| GET/POST /v1/models/query, /health, /[...model] | HAVE (3 extra files) | GET /v1/models/query, /health live + GET /v1/models/{model...} live (M8.10, wired merge wave; {model...} wildcard serves virtual models incl. auto/cheap). Gaps: ?capability= answers 400 (catalog records no per-model capability flags); ?category=/?max_price=/?min_context=/?shortlist= silently ignored on query; health is provider-level (probe_scope: provider), no per-model probe | PARTIAL | internal/server/models_detail.go | TestM810ModelDetailFixture, TestM810ModelsQueryFixture, TestM810ModelsHealthFixture, TestM810LiteralRoutesBeatDetailWildcard |
| POST /v1/moderations | HAVE | POST /v1/moderations live (M8.9, wired merge wave, scope chat): OpenAI shape, local guardrails only (PII/secret/injection); the 13 OpenAI categories are all false, score 0 — hate/violence/sexual/self-harm/illicit content is not detected. Notes: category_applied_input_types fixed to the per-category object (13 keys, each ["text"]); results[].flagged is true on a local finding while all 13 categories read false, breaking OpenAI's flagged == OR(categories) invariant (kept deliberately — the only design where a plain client gets any signal; disclosed in-band via notes + local_only + local_guardrails). Disclosure (F6): moderation traffic pollutes chat guardrail metrics — each call goes through Store.CheckScoped, incrementing the shared store's checked/byKind/blocked counters and appending an in-memory Event with a redacted-text preview (internal/guardrails/guardrails.go:1161-1172) | PARTIAL | internal/server/moderations.go | TestModerationsPIIFixture, TestModerationsCategoryAppliedInputTypes |
| POST /v1/search (+ /search/analytics) | HAVE | POST /v1/search live (M8.20, wired merge wave): Perplexity passthrough, verbatim, replay-exempt. /search/analytics not built | PARTIAL | internal/server/media_verify.go; internal/router/media_verify.go | TestMediaFixtureSearch200, TestMediaPassthroughSearchVerbatim |
| POST /v1/ocr | HAVE | POST /v1/ocr live (M8.20, wired merge wave): Mistral OCR passthrough, single endpoint | FULL | internal/server/media_verify.go; internal/router/media_verify.go | TestMediaFixtureOCR200 |
| POST /v1/systemone | HAVE | — | MISSING | — | UNVERIFIED |
| POST /v1/classify | HAVE — POST Jina zero/few-shot classification proxy (classify/route.ts, body {model?, classifier_id?, input, labels?} → api.jina.ai/v1/classify) | GET /v1/classify live (M8.2, wired merge wave): local auto-router tiering via ResolveWithThreshold — same path, different method, body, backend, response; NOT OmniRoute's Jina proxy | MISSING | internal/server/legacy_completions.go | TestClassifyFixture200, TestClassifyReturnsRealDecision |
| GET /v1/explain/routing | HAVE — GET routing_explain object: recent routing EVENTS ring buffer + quality snapshot + otel stats (explain/routing/route.ts) | GET /v1/explain/routing live (M8.2, wired merge wave): per-prompt classification + configured strategy + last-served decision/trace; only the last-served/events concept overlaps OmniRoute's events ring | PARTIAL | internal/server/legacy_completions.go | TestExplainRoutingFixture200, TestExplainRoutingNeverCallsUpstream |
| POST /v1/segment | HAVE — POST Jina segmenter proxy (segment/route.ts, body {content, tokenizer?, return_tokens?, return_chunks?, max_chunk_length?} → segment.jina.ai) | GET /v1/segment live (M8.2, wired merge wave): local sentence-split + per-segment classification — entirely different semantics from OmniRoute's Jina proxy | MISSING | internal/server/legacy_completions.go | TestSegmentFixture200, TestSegmentSplitsParagraphsAndCaps |
| Images: /v1/images/generations, /edits, /upscale | HAVE (3 files) | POST /v1/images/generations + /edits live (M8.5, wired merge wave): engine path via singleAttempt, verbatim upstream errors, 429 Retry-After forwarded. /upscale NOT built: it is not an OpenAI endpoint (exists only in third-party OpenAI-compatible servers — LocalAI/Lemonade/Osaurus/Doop); whether OmniRoute parity ever demands it is a coordinator decision | PARTIAL | internal/server/images.go; internal/router/images_passthrough.go | TestImagesGenerations200Fixture, TestImagesEdits200Fixture, TestImagesMalformedUpstream200Is502 |
| Audio: speech, transcriptions, translations, speech-to-text, text-to-speech/[voiceId], sound-generation, voices | HAVE (7 files) | — | MISSING | — | UNVERIFIED |
| Video: /v1/videos/generations, /v1/video-bridge/drilldown | HAVE (2 files) | POST /v1/videos/generations + GET /v1/videos/{id} live (M8.20, wired merge wave): Runway passthrough, task polling by re-GET. /v1/video-bridge/drilldown not built | PARTIAL | internal/server/media_verify.go; internal/router/media_verify.go | TestMediaFixtureVideosGenerations200, TestMediaFixtureVideosGet200, TestMediaVideoTaskUnknownID404 |
| Music: /v1/music, /generations, /plan, /detailed, /stream, /detailed/stream | HAVE (6 files) | POST /v1/music/generations live (M8.20, wired merge wave): MiniMax passthrough, synchronous, 200-envelope verbatim. Only /generations of OmniRoute's 6 music routes; MiniMax closed to new users since 2026-08-20 (entitled keys only) | PARTIAL | internal/server/media_verify.go; internal/router/media_verify.go | TestMediaFixtureMusic200 |
| Files: /v1/files, [id], [id]/content | HAVE (3 files) | POST/GET /v1/files, GET/DELETE /v1/files/{id}, GET /v1/files/{id}/content live (M8.7, wired merge wave): OpenAI file objects, atomic store, traversal-safe. Gaps: no pagination, no expiry, no per-purpose file-type validation, status always "processed"; no 512 MiB upload cap yet (reviewer verdict: land at merge — still open) | PARTIAL | internal/server/files_api.go | TestFilesUploadRoundTrip, TestFilesFixtureList200 |
| Batches: /v1/batches, [id], [id]/cancel, delete-completed | HAVE (4 files) | 6 routes live (POST /v1/batches, GET list/get, POST [id]/cancel, DELETE [id], DELETE delete-completed); real worker executes JSONL lines through the chat engine; cancel stops work; persisted batches.json; 9 fixtures, 13 tests, review PASS | PARTIAL | internal/server/batches_api.go; internal/server/batches.go | TestBatchesFixtures, TestBatchesCreateCompletesWithRealOutput |
| Per-provider: /v1/providers/[provider]/chat/completions, /embeddings, /images/generations, /models, /limits, /voices, /voices/[voiceId] | HAVE (7 files) | POST /v1/providers/{provider}/chat/completions + GET /v1/providers/{provider}/models live (M8.12, wired merge wave; pin = routing.only + delegate to handleChatCompletions, real-engine failover within the pinned provider's keys) + GET /v1/providers/{provider}/status extra (not in the row's list). Remainder: /embeddings, /images/generations, /limits, /voices, /voices/[voiceId] (5 of 7) | PARTIAL | internal/server/provider_v1.go | TestProviderV1ChatPinnedFailoverFixture, TestProviderV1ModelsFixture, TestProviderV1StatusFixture |
| WebSocket /v1/ws | HAVE (GET+OPTIONS, api/v1/ws/route.ts:144) | — | MISSING | — | UNVERIFIED |
| Gemini-native /v1beta/models, /v1beta/models/[...path] | HAVE (2 files) | GET /v1beta/models + POST /v1beta/models/{rest...} (:generateContent, :streamGenerateContent) live (M8.13, wired merge wave; pre-engine errors now Gemini-enveloped, fix re-reviewed PASS). Gaps: single-model GET /v1beta/models/{model} missing; no pagination (pageSize/pageToken, no nextPageToken) | PARTIAL | internal/server/gemini_native.go | TestM813ModelsFixture, TestM813GenerateContentFixture, TestM813PreEngineErrorsGeminiEnvelope |
| GET /v1/me/sessions, /me/sessions/[id] | HAVE | GET /v1/me/sessions lists the caller's Responses sessions (root + all descendants), list-price cost + unpriced_count, newest first, cap 100 (M5.6); GET /v1/me/sessions/{id} detail live (M8.11, wired merge wave): root's turns, cumulative tokens + list-price cost, per-turn detail, cap 1000 turns. Gap closed except project/client attribution, genuinely absent | PARTIAL | internal/server/quotas.go; internal/server/mesessions.go:51 | TestMeSessions, TestMeSessionsRequiresKey, TestMeSessionsEmpty, TestMeSessionDetailFixture200 |
| GET /v1/auto-combo/[channel]/candidates | HAVE | Route registered (M3 Phase D, D1) with the live pool: tier models, breaker states, per-key connections, cooldowns; free/pro + category filters applied from the live price book + tier membership. Gaps: allowlist exclusions have no store (excluded always false); ModelLocked always false; FreeAccessExclusion always nil; subscription/lkgp answer 503 fail-closed | PARTIAL | internal/server/guard.go:171; internal/server/server.go:3026 | TestAutoCombo* (16 tests) |
| Quotas/keys: /v1/quotas/check, /quota-recoveries/[id], /accounts/[id]/limits, /session-leases, /registered-keys (+[id], +[id]/revoke) | HAVE | POST /v1/quotas/check live (M8.11, wired merge wave): pre-flight verdict from the real ledger gates (rolling key budget + token limits), allowed + budget/token-limit/price numbers. Only /v1/quotas/check of the 5 listed; /quota-recoveries/[id], /accounts/[id]/limits, /session-leases, /registered-keys (+[id], +[id]/revoke) not built. Caveat: never consults the legacy cumulative budget cap (header claim narrowed at merge); auto/* virtual models unresolved; provider is caller-asserted | PARTIAL | internal/server/quotas.go | TestQuotasCheckFixtureAllowed, TestQuotasCheckFixtureDenied, TestQuotasCheckValidation |
| Relay: /v1/relay/chat/completions, …/bifrost | HAVE | — (ours has only an internal relay resolver for /v1/messages) | MISSING | — | UNVERIFIED |
| Vendor extras: /v1/fal/* (3), /v1/agents/* (5), /v1/antigravity, /v1/muse-code/models, /v1/api/chat | HAVE | — | MISSING | — | UNVERIFIED |
| VSCode token-scoped surfaces /v1/vscode/** | HAVE (~20 files) | — (deliberate scope: no VSCode extension) | MISSING | — | UNVERIFIED |
| Token-count: /v1/messages/count_tokens, /v1/responses/input_tokens | HAVE | POST /v1/messages/count_tokens + GET /v1/responses/input_tokens live (M8.3, wired merge wave; post-registration smoke test through the guard: count_tokens → 200). Both endpoints with fixture contracts; local-only | FULL | internal/server/count_tokens.go | TestM83MessagesCountTokensFixture, TestM83ResponsesInputTokensFixture, TestM83ArrayContentCountsOnce |
| /v1/provider-plugin-manifest | HAVE | — | MISSING | — | UNVERIFIED |
| Mgmt proxies: /v1/management/proxies*, /proxy-subscriptions* | HAVE (8 files) | — | MISSING | — | UNVERIFIED |
| Web/media: /v1/web/fetch, /v1/web/map, /v1/media/[id], /v1/voices | HAVE | — | MISSING | — | UNVERIFIED |

#### 14b. Management / admin API (39 rows: FULL 6, PARTIAL 21, MISSING 11, AHEAD 1)

| Area (OmniRoute route files) | OmniRoute | Ours | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Health | api/health/* (3) | GET /healthz (auth-exempt) | FULL | internal/server/guard.go | UNVERIFIED |
| Metrics | api/system/metrics, api/telemetry/summary | GET /metrics (gateway-key auth) | FULL | internal/server/guard.go | UNVERIFIED |
| Auth-on-by-default / default credential | Keyless by default; INITIAL_PASSWORD=CHANGEME, .env.example:28-31 re-verified fresh ("publicly known") | Auth ON whenever a key exists; no default password; once-only loopback bootstrap | AHEAD (ours) | internal/server/guard.go:452 | UNVERIFIED |
| OpenAPI | api/openapi/spec, api/openapi/try (2) | GET /openapi.json live from guard table + try-it page web/tryit.html (170 routes, tier/scope-correct) built but not yet served — no /tryit route; serving is follow-up (M10) | PARTIAL | internal/server/guard.go | TestTryItRoutesMatchGuardTable, TestOpenAPIGapIsDocumented |
| Providers mgmt (61 files) | FULL | POST+DELETE /api/providers, GET /api/providers, GET /api/catalog (add/delete/list/catalog only) | PARTIAL | internal/server/providers_add.go | UNVERIFIED |
| Keys mgmt (14 files + 2 key-templates) | FULL | Provider-key add/delete/role + gateway rotate/revoke-all; no per-key policy API, no import/reveal | PARTIAL | internal/server/keystatus.go | UNVERIFIED |
| Scopes enforced | Enforced per route (keyScopedRoute.ts) | HasScope wired via requireScope on every /v1 route; table-driven test over all guard-table routes (M1.1, P0-1 CLOSED) | FULL | internal/server/server.go:1476; internal/config/gatewaykey.go:227 | TestScopeEnforcedTableDriven |
| Combos (10 files) | FULL | GET/POST/PATCH/DELETE /v1/combos live (M8.14, wired merge wave): named-chain CRUD over config groups + chain_strategies, one shared ComboStore, race-serialised via routingMu (M8.14 race fixes, soak-tested). Still no dashboard UI; duplicate/import/export/test/enable still missing (Area 8b row, "Named combos CRUD") | PARTIAL | internal/server/combos_api.go | TestCombosCreate201Fixture, TestCombosRoundTrip, TestCombosRaceSoak |
| Usage/budget (33 files) | FULL | GET/POST /api/usage/budget (admin; summary + checkBudget allow/warn/deny; limitUsd required for a fresh key) + GET /api/usage/om-usage (allow_usage_command flag, JSON or plain text) (M5.7) | FULL | internal/server/guard.go:155-157; internal/server/usageapi.go:91 | TestUsageBudgetGet, TestUsageBudgetPost, TestOmUsage |
| Analytics (3 files + usage analytics) | FULL | Dashboard overview/logs/usage feeds + GET /api/analytics/summary, /by-provider, /by-model, /costs live (M8.17, wired merge wave). Still no dashboard page (M10); no auto-routing/compression/diversity surface | PARTIAL | internal/server/analytics_api.go | TestAnalyticsSummaryValues, TestAnalyticsCostsComplete |
| Cache (4 files) | FULL | GET /api/cache/stats live (hit/miss, tokens_saved when persistence wired; savings_note when memory-only) (M6.8) + DELETE /api/cache live (M8.15, wired merge wave) with per-layer eviction counts; no /dashboard/cache page (M10); no invalidateByModel | PARTIAL | internal/server/guard.go:158; internal/server/cache_stats.go:119 | TestCacheStats* (8), TestM62b* (5), TestM815CacheDeleteFixture |
| Resilience (5 files) | FULL | /dashboard/cooldown feed incl. breakers + GET /api/resilience live (M8.16, wired merge wave): live breaker/cooldown feed. No settings read/patch, no per-model cooldowns, no breaker reset; hedge counts honestly unknown | PARTIAL | internal/server/resilience_api.go | TestM816ResilienceFixture, TestM816ResilienceLiveState |
| Pricing (5 files incl. pricing/sync, PATCH /api/pricing) | FULL | GET /dashboard/prices feed + GET /api/pricing and PATCH /api/pricing live (M8.15, wired merge wave; PATCH race-fixed, -race clean). Stays PARTIAL until pricing-sync lands | PARTIAL | internal/server/pricing_api.go | TestM815PricingGetFixture, TestM815PricingPatchFixtureRoundTrip, TestM815PricingPatchConcurrentRace |
| MCP (7 files + stdio/SSE/Streamable-HTTP) | FULL, 110 tools claimed (UNVERIFIED — static grep undercounts at 84; do not quote 84) | stdio server, 3 read-only tools + GET /dashboard/mcp + MCP client selector; no HTTP/SSE transport, no audit/restart API | PARTIAL | internal/mcp/mcp.go:100-113 | UNVERIFIED |
| Plugins (8 files) | FULL (see §14c) | Feed + 501 load stub | PARTIAL | internal/plugins/plugins.go | UNVERIFIED |
| Auth mgmt (12 files: login/logout/MFA/OIDC/SSO/CSRF/status) | FULL | Gateway keys + bootstrap only; no login/MFA/OIDC/SSO | PARTIAL | internal/server/guard.go | UNVERIFIED |
| Settings (92 files) | FULL | Config file + dashboard POSTs (strategies, cooldown, guardrails, key-budgets, feedback, intent-tiers, suggestions) + settings export | PARTIAL | internal/server/server.go | UNVERIFIED |
| Tunnels (9 files: cloudflared/ngrok/tailscale) | FULL | Local TCP tunnel only (start/stop/status) | PARTIAL | internal/server/tunnel.go | UNVERIFIED |
| DB backup (4 files) | FULL | POST /admin/db/export\|import, wipe arm+fire (no scheduled backups) | PARTIAL | internal/server/dbexport.go | UNVERIFIED |
| Guardrails (2 files incl. guardrails/test) | FULL | Dashboard feed + toggles + POST /api/guardrails/test live (M8.16, wired merge wave): text-only probe over local built-ins (PII/secrets/injection), never records, never sends upstream. Qualification: OmniRoute's guardrails/test runs the full pre-call pipeline over string|object|array input with per-guardrail results + masked payload; ours is text-only, local built-ins only (external/async deliberately not run). Area 7 row "Custom regex patterns" is a different row and stays MISSING | FULL | internal/server/guardrails_test_api.go | TestM816GuardrailsTestFixture, TestM816GuardrailsTestDoesNotRecord |
| Shutdown/restart | api/shutdown, api/restart | POST /admin/shutdown real; no restart endpoint | PARTIAL | internal/server/server.go | UNVERIFIED |
| Logs | api/logs/* (4) + usage/call-logs | /dashboard/logs feed + GET /api/logs live (M8.16, wired merge wave; capture wired in main.go, double-redacted). OmniRoute also has usage/call-logs, not exposed | PARTIAL | internal/server/logs_api.go | TestM816LogsFixture, TestM816LogsRedacted |
| Compression mgmt (7 files) | FULL | Internal only; summary in overview feed | PARTIAL | internal/server/server.go | UNVERIFIED |
| Per-response cost/token/latency headers | X-OmniRoute-Response-Cost, -Tokens-In/-Out, -Tokens-Per-Second, -Cost-Saved, -Fallback-Attempts (headers.ts:21-24; omnirouteResponseMeta.ts:224) re-verified fresh | Same choke-point writer (M2.1): Cost-USD, Tokens-In/Out/Cached, Tokens-Per-Second, Latency-Ms, Fallback-Attempts all emitted; Cost-Saved via X-Codvian-Cache saved-tokens/saved-USD fields | FULL | internal/server/headers.go:360 | TestResponseHeadersChatNonStream, TestWriteResponseHeadersCache |
| Version-manager (6 files) | FULL | POST /admin/update is an honest 501 stub | MISSING | internal/server/server.go:1236 | UNVERIFIED |
| Webhooks (5 files) | FULL — GET/POST list/create, GET/PUT/DELETE per id | GET/POST /api/webhooks (list/create) + GET/DELETE /api/webhooks/{id} live (M8.19, wired merge wave); budget.warning wired live (threshold-crossing delivery proven, HMAC-signed, edge-triggered) + quota.exceeded; config validation now accepts both events (merge-wave fix). Remainder: PUT /api/webhooks/{id} + dashboard UI (M10) | PARTIAL | internal/server/webhooks_api.go | TestM819CreateListGetDelete, TestM819BudgetWarningFiresOnThresholdCrossing |
| Translator (5 files) | FULL — POST translate/detect verified | — | MISSING | — | UNVERIFIED |
| Discovery (4 files) | FULL — scan, results, verify | — (catalog is static; no scan/verify API) | MISSING | — | UNVERIFIED |
| A2A (5 files + Agent Card) | FULL — JSON-RPC 2.0 + SSE, 6 skills re-verified fresh | — (zero A2A hits) | MISSING | — | UNVERIFIED |
| Teams / cost centers (4 files) | FULL | GET/POST /api/teams + GET/PATCH/DELETE /api/teams/{id} live (M8.18, wired merge wave): full CRUD + live budget/member/consumption views + enforced gates. Field-for-field parity vs OmniRoute's 4 team files not verified | PARTIAL | internal/server/teams_api.go | TestAPITeamsCreateGetDeleteRoundTrip, TestAPITeamsBudgetEnforcedByGate |
| Quota-share pools/groups/plans (11 files) | FULL | GET/POST /api/quota-pools + GET/PATCH/DELETE /api/quota-pools/{id} live (M8.18, wired merge wave): full CRUD + live views + enforced gates. Field-for-field parity vs OmniRoute's 11 pool files not verified; pools have no groups/plans concept at all | PARTIAL | internal/server/pools_api.go | TestAPIQuotaPoolsCreateGetDeleteRoundTrip, TestAPIQuotaPoolsFairShareEnforcedByGate |
| Embedded services (77 files) | FULL | — (deliberate scope) | MISSING | — | UNVERIFIED |
| Gamification/skills/memory/conductor/chaos/evals | FULL (dozens of files) | — (deliberate scope) | MISSING | — | UNVERIFIED |
| Files/batches mgmt (4 files) | FULL | /v1/files API live (M8.7, wired merge wave — see §14a Files row); stored Responses objects only before that; batches still missing (M8.8 not built) | MISSING | internal/server/files_api.go | TestFilesUploadRoundTrip |
| Sessions (1 file + v1 me/sessions) | FULL | GET /v1/me/sessions list live (M5.6); GET /v1/me/sessions/{id} detail live (M8.11, wired merge wave); project/client attribution still missing | PARTIAL | internal/server/guard.go:170; internal/server/mesessions.go:51 | TestMeSessions, TestMeSessionDetailFixture200 |
| Search mgmt (search/providers, search/stats) | FULL | — | MISSING | — | UNVERIFIED |
| Fallback chains API (fallback/chains) | FULL | Config-only chains; no API | MISSING | — | UNVERIFIED |
| OAuth mgmt (19 files) | FULL | POST /admin/provider/login is an honest 501 stub | MISSING | internal/server/server.go:1240 | UNVERIFIED |
| Service install | (embedded-services install routes) | POST /admin/service/install honest 501 | MISSING | internal/server/server.go:1234 | UNVERIFIED |

Four of our 170 routes are honest 501 stubs (guard.go:273-279: service/install,
plugin/load, update, provider/login) — counted MISSING, not WRONG-BEHAVIOUR, because they
say so. Our 59 /dashboard/* routes are all Tier 3; 46 of them are auth-exempt (the
read-only feeds), while 13 mutating POSTs require admin auth. 52 of our 170 routes are
auth-exempt by design (loopback-bound dashboard assumption), vs their requireManagementAuth
on management routes — that exemption is itself a gap for the exempted reads (the mutating
dashboard POSTs — /dashboard/key-budgets, /strategies, /guardrails, /feedback, /cooldown —
do require auth).

#### 14c. Plugins (8 rows: FULL 0, PARTIAL 4, MISSING 4)

| Capability | OmniRoute | Ours | Verdict | File:line | Test |
|---|---|---|---|---|---|
| Install / upgrade | POST /api/plugins (local path), marketplace install; pluginManager.install() enforces strictly-newer semver, staging + cleanup (manager.ts:129-217) | No install path. POST /admin/plugin/load = honest 501 by design (compiled-in only, no .so/Wasm/JS — loading arbitrary code from config would be ACE) | MISSING (deliberate design divergence, not an oversight) | internal/plugins/plugins.go | UNVERIFIED |
| Activate / deactivate | POST /api/plugins/[name]/activate, /deactivate; lifecycle hooks onActivate/onDeactivate/onUninstall (manager.ts:33-41) | None — enablement is config plugins.enabled list order at startup only | MISSING | — | UNVERIFIED |
| Per-plugin config | GET/PUT /api/plugins/[name]/config | Config-file only (plugins.cost_cap_usd_per_request, header-add header/value); no API | PARTIAL | internal/config/config.go | UNVERIFIED |
| Marketplace / scan | GET /api/plugins/marketplace, /marketplace/install, /scan | None | MISSING | — | UNVERIFIED |
| Hooks | 4 pipeline hooks (hooks.ts:37-44,252-260): onRequest (blocking, can modify/block with 403), onResponse (chained transform), onError (fire-and-forget), onStreamComplete (usage/timing) + 4 lifecycle hooks; child-process IPC loader | 3 hooks: HookPreRequest (fail-closed 403/-32001 plugin_denied), HookPostResponse (fail-open), HookProviderAdapter (per-provider wire rewrite); in-process only, panic-isolated per plugin | PARTIAL (no onError/onStreamComplete equivalents; no lifecycle; no out-of-process isolation) | internal/plugins/plugins.go | UNVERIFIED |
| Built-ins shipped | None documented (system is install-your-own + SDK) | 3 named built-ins: header-add, response-log, cost-cap (builtins.go:20-23, costcap.go:62; Builtins() map, config.go:1111-1112 validation) | PARTIAL (different shapes — theirs is a platform, ours ships batteries; neither dominates) | internal/plugins/builtins.go:20-23 | UNVERIFIED |
| MCP plugin tools | 8 plugin-management tools over MCP (tools/pluginTools.ts) | None | MISSING | — | UNVERIFIED |
| Dashboard page | /dashboard/plugins (marketplace + installs) | GET /dashboard/plugins (live registry snapshot, honest zero state; no install UI) | PARTIAL (no install UI) | internal/server/server.go | UNVERIFIED |

#### 14d. Docs / README (7 rows: FULL 3, PARTIAL 1, MISSING 1, WRONG 1, UNVERIFIED 1)

| Claim / item | Finding | Verdict | File:line | Test |
|---|---|---|---|---|
| "README rewritten to 593 lines in README.v2.md, in OmniRoute's structure" | WRONG on count: README.v2.md is 260 lines, README.md is 179 lines (measured 2026-11-19). Structure claim partly true: both files follow OmniRoute's order; v2 adds three sections OmniRoute lacks an equivalent for. Depth ~1/7th of OmniRoute's README (1,844 lines, 76 headings vs ours 11 headings each) | WRONG (line count) / PARTIAL (structure) — counted once as WRONG | README.v2.md; README.md | UNVERIFIED |
| Test headline "1,919 test events pass, 0 fail, 38 packages" | UNVERIFIED fresh (no Go toolchain in this env to re-run). Internally consistent: 1,515 top-level + 404 subtests = 1,919; cited gates check arithmetically (1,131+305=1,436; 1,133+293=1,426). "1,436 headline found false" is stale/misframed — 1,436 is a genuine per-slice recount, never the headline. Current headline is 1,919. The headline itself still needs one independent re-run | UNVERIFIED | — | UNVERIFIED |
| SSE "3 tokens / 8 ms" batching claim | Bounds TRUE in code: sseBatchMaxTokens = 3, sseBatchMaxDelay = 8 ms. Scope correction: NOT only in handleChatStream — also in Responses streaming; /v1/messages streaming does NOT batch. README frames it honestly as a throughput trade, not a latency win | FULL (with scope correction) | internal/server/sse_batch.go; internal/server/responses.go:1372; internal/server/server.go:5292 | UNVERIFIED |
| Benchmark numbers in README | Latest attempt-3 clean pass shown as missed (latency 0.855 ms vs <0.5 ms; RPS 1,079 vs >3,000), with polluted/clean distinction, 502-rate and cache-hit caveats. Honest | FULL | README.md | UNVERIFIED |
| Catalog "610 providers" in README | 394 ref + 29 + 1 + 191 triage rounds 1–5; 570 + 44 round-5 = 614, minus 4 CT-3 retirements = 610. Re-counted 2026-11-19 (CT-8): measured 610 total, 379 with base URL, 370 routable, 231 without, 162 free-tier. V: pass 2026-11-19 22:19 IST (A5+V-3; TestBaseURLCoverage pins 610/379/370/9; README quotes 610/370) | FULL | internal/catalog/g7_test.go:72 | TestBaseURLCoverage |
| Docs breadth | OmniRoute: docs/ 10,416 .md files (10,206 i18n + 210 English), llm.txt 503 lines, docs/openapi.yaml 19,558 lines, API_REFERENCE.md, PROVIDER_REFERENCE.md, FREE_TIERS.md, OPENAI_COMPATIBILITY.md, etc. Ours: docs/ 63 .md files, no llm.txt, no API_REFERENCE.md, no static OpenAPI file (live GET /openapi.json only), plus CONTRACTS.md (frozen builder contracts — no OmniRoute equivalent checked) | MISSING (as parity) | docs/ | UNVERIFIED |
| README structure mirroring | v2's section order tracks OmniRoute's; content depth does not | PARTIAL | README.v2.md | UNVERIFIED |

---

## Changelog for this file

- 2026-11-18 (M0.2): seeded from master prompt §2.2 + omniroute-full-comparison-2026-11-18.md
  (1,803 lines, read in full). Headline 691 / FULL 115 / PARTIAL 194 / MISSING 330 /
  WRONG 20; strict 16.6%, weighted 30.7%. Areas 8a/8b condensed (sub-tallies + key rows);
  all other areas carry full row-level tables from the report. Test names listed only
  where the report names one; otherwise UNVERIFIED. No test-count number is asserted here
  (M0.1 gate pending).

### Wave 1 — 2026-11-18 (M1 P0 security, gate green)

- Gate: `go build ./... && go vet ./... && go test ./... -race -count=1 -v` → exit 0, gofmt clean; 2,579 PASS events (2,005 top-level) / 0 FAIL / 2 SKIP, 39/39 packages (was 2,410/0 at M0 re-gate; +169 new tests).
- Flips: 20 rows → FULL (Area 4: 5, Area 5: 1, Area 8b: 1, Area 6: 5, Area 7: 1, Area 9: 2, Area 10: 3, Area 11: 1, Area 1: 1). New totals: FULL 135, PARTIAL 192, MISSING 324, WRONG 8; strict 19.5%, weighted 33.4%.
- P0: CLOSED 8 (P0-1, P0-4, P0-5, P0-6, P0-7, P0-9, P0-10, P0-11), PARTIAL 2 (P0-2 coalescing key still unscoped; P0-3 serve-mode persistence unwired), OPEN 1 (P0-8 per-response headers — M2.1).
- M1.5 stays PARTIAL overall: flush + savings wired end-to-end, but serve-mode PersistentCache wiring (main.go + config) is still missing.

### Wave 1 — 2026-11-18 (M2 telemetry, gate green)

- Gate: `go build ./... && go vet ./... && go test ./... -race -count=1 -v` → exit 0 (log /tmp/m2-gate.log, 16:45 IST); 2,039 top-level PASS / 0 FAIL (2,605 incl. subtests), 40/40 packages + 2 no-test-files (M1 gate was 2,579 PASS events / 0 FAIL, 39/39 packages).
- Flips: 6 rows → FULL (Area 2: fallback-attempts header; Area 5: per-response headers; Area 11: per-response headers + metrics p50/p95/p99; Area 12: per-response headers; Area 14: per-response headers). New totals: FULL 141, PARTIAL 191, MISSING 319, WRONG 8; strict 20.4%, weighted 34.2%.
- P0: CLOSED 9 (P0-8 per-response headers now closed via M2.1/M2.2), PARTIAL 2 (P0-2 coalescing key still unscoped; P0-3 serve-mode persistence unwired), OPEN 0.
- Known limitation: when both the legacy metrics recorder and the observability collector are present, /metrics contains duplicate HELP/TYPE blocks for shared families — Prometheus parsers tolerate it; not a gate failure. M2.4 OTLP export still open (tracer in-memory only).

### M3 Phase B — 2026-11-18 (strategies wiring + conformance, gate green)

- Gate: `go build ./... && go vet ./... && go test ./... -race -count=1 -v` → exit 0 (log /tmp/m3-gate-v.log); 2,131 top-level PASS / 0 FAIL (2,701 incl. subtests), 2 SKIP, 40/40 packages (M2 gate was 2,039 top-level / 0 FAIL). First gate run failed on 3 server tests pinning the old 8-strategy list; fixed (31-strategy feed, custom-weights entry, unknown-name case) and re-gated green.
- Flips: 14 rows → FULL, 11 rows → PARTIAL, WRONG 1 → 0. Area 1: FULL 3 → 17, PARTIAL 24 → 27, MISSING 35 → 19, WRONG 1 → 0; strict 4.8% → 27.0%, weighted 23.8% → 48.4%. Headline: FULL 141 → 155, PARTIAL 191 → 194, MISSING 303, WRONG 7; strict 20.4% → 22.4%, weighted 34.2% → 36.5%.
- M3 exit gate NOT met: Area 1 needs ≥ 80% weighted; Phase B reaches 48.4%. Phase C (data plumbing + combos) chained immediately.
- MIGRATION (M3, pre-1.0): "weighted" now means the per-target weight draw; old operator weight-set behaviour is "custom-weights".

### M3 Phase C — 2026-11-18 (data plumbing + S5 combos + conformance extension, gate green)

- Gate: `./scripts/ci.sh` → CI: PASS (log /tmp/m3-phasec-ci5.log); 2,239 top-level PASS / 0 FAIL (2,837 incl. subtests), 2 SKIP, 40/40 packages (Phase B was 2,131 top-level / 0 FAIL; +108 new tests).
- Flips: 6 rows → FULL (weighted, least-used, quota-weighted, reset-window, lkgp, Router strategy lkgp), 14 rows → PARTIAL (fusion, pipeline, jev, quota-share, candidate-pool endpoint, auto/offline, auto/lkgp, auto/chaos, auto/subscription, auto/thrifty, auto/best-*, auto/pro-*, 4-tier cascade, endpoint-specific combos). Area 1: FULL 17 → 23, PARTIAL 27 → 36, MISSING 19 → 4, WRONG 0; strict 27.0% → 36.5%, weighted 48.4% → 65.1%. Headline: FULL 155 → 161, PARTIAL 194 → 203, MISSING 303 → 288, WRONG 7; strict 22.4% → 23.3%, weighted 36.5% → 38.0%.
- M3 exit gate NOT met: Area 1 needs ≥ 80% weighted; Phase C reaches 65.1%. Phase D (engine integration) chained immediately.
- Gate note: the gate needed 5 CI runs — three pre-existing load-flaky timing tests (tokencount 1 MB bounds under -race and non-race, mcptools 200 ms init timeout) were re-scaled to full-gate measurements with documented rationale, and one real race was fixed (async guardrail verdict recorded before the cut wake-up, so stats can no longer read all-zero after a 403). No Phase C code was the cause of any of the four failures. Detail in LOG.md.

### M6 — 2026-11-18 (caching 2.0 + token-cost engine, gate green)

- Gate: `./scripts/ci.sh` → CI: PASS (gofmt, build, vet, `go test ./... -race -count=1`); verbose re-run (log `~/workspace/goals/codvian-router/hidden_files/m6-gate-verbose.log`): **2,773 top-level PASS / 0 FAIL (2,771 Test* + 2 examples), 864 subtest PASS / 0 FAIL, 3,637 total PASS events, 42/42 packages ok** (+ 2 no-test-files: cmd/release-sbom, web), 2 SKIP (the pre-existing release-verify skips). The 15 `FAIL` lines in the log are the release-verify tamper fixtures' expected-failure stdout inside a passing package, not failures (0 `--- FAIL`, 0 package `FAIL` lines). M5 gate was 2,523 top-level / 0 FAIL; +250 from M6's new tests.
- Exit gate MET: correctness half — 17/17 M6.11 property tests pass unmodified (M6.11 + M6.11-fix, both BLOCKED defects toggle-proven closed); savings half — savings numbers reproducible from ledger rows (M6.9 tier split + M6.2b SavingsTotals, hand-computed test) and surfaced via GET /api/cache/stats when persistence is wired.
- Flips: 9 rows → FULL (Area 6: per-entry size cap, total memory cap, truncated-response guard, bypass headers, per-request cache controls, semantic threshold, semantic scope, prompt-cache affinity, dedup/coalescing; Area 12: rows 6 + 17), 3 rows → PARTIAL (Area 6: persistence tiers, reasoning cache, search cache), 1 row → DIVERGENCE (Area 6: per-provider isolation, decided M6.12). P0-2 and P0-3 CLOSED. Area 6: FULL 15 → 24, PARTIAL 8 → 7, MISSING 9 → 0; strict 46.9% → 77.4%, weighted 59.4% → 88.7% (comparable denominator).
- Headline + every per-area header/total rewritten from a mechanical recount (see the M6 note under the headline): 688 rows, FULL 226, PARTIAL 169, MISSING 240, WRONG 7, AHEAD 28, DIVERGENCE 10 (+1 unclassified claims row); comparable 649; strict 34.8%, weighted 47.8%. M6.4 correction recorded: the per-request THRESHOLD header is NOT a Codvian extension — OmniRoute has x-omniroute-cache-threshold with the same max() composition (verified in the reference clone, semanticCacheManager.ts:307-311); all five per-request controls exist there (key :300/:432, type :302, threshold :307, no-store :425, ttl :460), so the row is FULL (parity), not AHEAD.
- Known limitations carried forward (not fixed): Redis vector store still missing; reasoning/search caches memory-only (no DB tier); search cache unwired (no search endpoint); dashboard still estimates cache-write cost as a 1.25×–2.5× band; no /dashboard/cache page (M10); per-model prompt-cache floor (4096 for Haiku 4.5 / Opus 4.5+) is a follow-up; no live Anthropic call made for tier/marker verification (mock httptest only).
- M7 (compression & token efficiency) chained immediately.

### M7 merge wave — 2026-11-19 (M7.4–M7.9 wiring, gate green)

- Gate: `./scripts/ci.sh` → CI: PASS. Verbose (`go test -race -count=1 -v ./...`, log /tmp/m7-merge-gate-verbose.log): **2,931 top-level PASS / 0 FAIL, 992 subtest PASS, 3,923 total PASS events, 43/43 packages ok** (+ 2 no-test-files: cmd/release-sbom, web), 2 SKIP (the pre-existing release-verify skips). The 15 `FAIL` lines in the log are the release-verify tamper fixtures' expected-failure stdout inside a passing package, not failures (0 `--- FAIL`, 0 package `FAIL` lines — same as M6). M7 gate wave was 2,773 top-level / 0 FAIL; +158 from M7.4–M7.9 + merge-wave tests.
- Independent merge-wave review: PASS-WITH-NITS, no blocking defects. 1,664 pass / 0 fail across the 6 affected suites under -race; 4 toggle proofs all FAIL as required (RTK off, Caveman off, HardBudget off, calibrated-local feed → factor 1.2→1.1086788); stale-premise rewrites judged contract-preserving; the three non-wirings confirmed in code. 7 nits found, all fixed in the same commit (RTK "56 packs" → 18 implemented of OmniRoute's 56; pool "20/20" → 10/10; inflation-guard row refreshed — the 4 cases now FIT via the post-pass; CompressionResult.Mode json tag added; client.go dial doc comment corrected; TestResolveModeAvailabilityToggle first sub-case now flips Caveman OFF; row 353 pipeline.go:280 → 273).
- Flips: 7 rows MISSING → PARTIAL (Area 10: RTK, Caveman, stacked pipeline, named modes, budget dial, worker pool, runtime calibration) and 1 row PARTIAL → FULL (base64 data-URI exclusion). Hard-budget stays PARTIAL with refreshed evidence; cache-aware stays MISSING with refreshed evidence (built, unwired); count memoization stays AHEAD with refreshed evidence. Area 10: FULL 9 → 10, PARTIAL 7 → 13, MISSING 27 → 20, AHEAD 4; strict 20.9% → 23.3%, weighted 29.1% → 38.4%. Headline: FULL 227 → 228, PARTIAL 169 → 175, MISSING 246 → 239; strict 34.9% → 35.1%, weighted 47.9% → 48.5% (689 rows, comparable 650).
- M7 COMPLETE (M7.1–M7.9 all done + merged). M8 (API breadth) chained immediately.

### M8.0 — 2026-11-19 (fixture harness + Area 14 reconciliation, gate green)

- Gate: `go build ./...` OK; `go vet ./internal/apitest/` clean;
  `gofmt -l` clean on the new files; `go test -race -count=1
  ./internal/apitest/` green (12 tests). Toggle proof: the example
  fixture with `assert.status` flipped 200 → 201 FAILS
  TestRunChatCompletions200 as required; restored fixture passes.
- Built: `internal/apitest/fixture.go` (LoadFixture / Run /
  CheckResponse; stdlib only; loader rejects `fixture != true`,
  `schema_version != 1`, and path traversal in slice/name) +
  `internal/apitest/fixture_test.go` (12 tests incl. wrong-status,
  wrong-JSON-path, and missing-required-field negatives) +
  `tests/fixtures/v1/m8.0/chat-completions-200.json` (recorded mock
  `POST /v1/chat/completions` 200 against a mock upstream, labelled as
  a fixture) + `docs/API_FIXTURES.md` (schema, layout, naming, required
  fields, the fixture-is-mock-data rule). Every other M8 slice's DoD
  depends on this schema.
- Flips (Area 14 only, verdicts + evidence, no rebuild): 2 rows
  PARTIAL → FULL (14b usage/budget, 14b scopes), 3 rows MISSING →
  PARTIAL (14a me/sessions list, 14a auto-combo candidates, 14b
  Sessions duplicate); 14b headers already FULL (M2.1, no change);
  14b Cache stays PARTIAL with corrected evidence; 14a me/status stays
  PARTIAL with corrected evidence (daily/weekly windows live; per-window
  total request counts still missing — Area 11 row 724, M8.11's job).
  Area 14 is now FULL 12 / PARTIAL 29 / MISSING 44 / WRONG 1 / AHEAD 1 /
  DIVERGENCE 0 / UNVERIFIED 1 (88 rows, comparable 87; strict 13.8%,
  weighted 30.5%). Headline: FULL 230 / PARTIAL 177 / MISSING 236
  (689 rows, comparable 650; strict 35.4%, weighted 49.0%).
- 89-vs-88 finding: the comparison report's §1a subtotal claims 35
  rows (5 FULL, 2 PARTIAL, 28 MISSING) but its own §1a table lists 34
  (5/2/27) — the report over-counted MISSING by 1. No row was dropped
  from PARITY.md and none is added; 88 is the correct Area 14 total.
  Also corrected: pre-flip MISSING was 48, not the header's 47, and the
  header's DIVERGENCE 1 was phantom (no Area 14 row carries a DIVERGENCE
  verdict); the §14b header was stale (FULL 2 / MISSING 17 — the
  headers row was already FULL since M2.1). Detail in the Area 14
  header note + LOG.md.
- Honest limits: the Area 14 "ours 89 routes" scale count is still
  stale (guardRouteTable counts 106 routes today: 62 GET, 40 POST, 4
  DELETE) — left for a later pass; the 14a me/sessions row is PARTIAL,
  not FULL, because the /me/sessions/[id] detail endpoint does not
  exist (list only); Area 11 row 818 (session/project/client
  attribution + /v1/me/sessions) still says MISSING and belongs to
  Area 11, not this reconciliation.

### M8 — 2026-11-19 (API breadth, PARITY flips per LOG.md reviews)

- Flips (reviewer verdicts, not builder claims; routes wired by the M8
  merge wave, 106 → 170 routes, full server suite green): MISSING → FULL
  2 (§14a OCR — single POST /v1/ocr; §14a token-count — both endpoints,
  post-registration smoke test through the guard); PARTIAL → FULL 2
  (Area 11 me/status — TotalRequests 2-line patch; §14b guardrails-test
  — text-only, local built-ins only, qualified); MISSING → PARTIAL 14
  (§14a completions, models/query, moderations, search, images, video,
  music, files, per-provider, gemini-native, quotas; §14b webhooks,
  teams, quota-share pools); M8.2 lumped row split into three
  (classify + segment stay MISSING as OmniRoute defines them — Jina
  proxies, local variants at the same path noted; explain → PARTIAL).
  Evidence-only updates: Area 1 row 371, Area 6 Flush + Search cache,
  Area 11 Analytics endpoints, §14a responses + me/status, §14b
  Combos/Analytics/Cache/Resilience/Pricing/Logs/Sessions/Files-batches.
  NOT flipped: §14a audio (M8.6 merge gate — provider audio client
  still missing), §14a batches (M8.8 not built), §14b fallback chains
  API + Area 8b combos CRUD + Area 7 row 609 (stay MISSING per
  reviewers). §14a me/status evidence corrected but verdict held at
  PARTIAL (no reviewer blessed FULL for that row).
- Headline now: 691 rows, FULL 234 / PARTIAL 190 / MISSING 221 / WRONG 7
  / AHEAD 28 / DIVERGENCE 10 (+1 unclassified); comparable 652; strict
  35.9%, weighted 50.5%. Area 14 now FULL 15 / PARTIAL 41 / MISSING 28
  (90 rows, comparable 89; strict 16.9%, weighted 39.9%). Recount
  mechanical (script saved at
  ~/workspace/goals/codvian-router/hidden_files/parity-recount-m8.py —
  split on unescaped pipes, verdict from the Verdict column; Area 8b
  left as claimed).
- Known stale, left for a later pass: the §14b trailing paragraph ("Four
  of our 89 routes are honest 501 stubs … 54 of our 89 routes are
  auth-exempt") still quotes the pre-M8 route total; the merge wave
  counts 170 routes. The M8.21 doc overclaim ("served from the same
  router" — web/tryit.html is still unserved, no /tryit route) is
  recorded in LOG.md, not fixed here.

### P0-2/P0-3 reconciliation — 2026-11-19 (docs-only, no code touched)

- Apparent conflict: the Wave 1 (M1) and M2 changelog entries above still
  read "P0-2 PARTIAL (coalescing key still unscoped); P0-3 PARTIAL
  (serve-mode persistence unwired)", while the P0 table and the M6 note
  record both CLOSED. Resolution: the M1/M2 lines were TRUE AT THE TIME
  (pre-M6 snapshots) and are left verbatim as history — they are not
  stale, and neither they nor the append-only LOG.md M1/M2 entries were
  rewritten. The M6 closure is the current truth, re-verified 2026-11-19
  against the live code (see below). No P0 verdict was flipped.
- Ground truth, P0-2 (tenant isolation): exact key is ScopedKey
  (`internal/cache/key.go:141`, tenant + temp bucket in the hash), used
  on both live paths (`internal/degrade/degrade.go:1064`,
  `internal/server/relay.go:636`); coalescing is tenant-scoped in the
  live path (`internal/coalescing/coalescing.go:407`,
  `internal/router/router.go:5985`); semantic layer records tenant +
  temp bucket per entry and legacy unscoped rows are never served by a
  scoped lookup (`internal/cache/semantic_persist.go:225-231`). Tests
  re-run 2026-11-19 (`go test -race -count=1`, TMPDIR=$HOME/.tmp):
  cache 17/17 PASS (7 TestM611* + 6 TestM611Fix* + 4 TestM14*),
  degrade 13/13 TestM611* PASS, router
  TestEngineCoalescesIsolatedPerTenant PASS.
- Ground truth, P0-3 (temperature/truncation/persistence/flush/savings):
  ShouldStore (`internal/cache/key.go:207`) refuses finish_reason=length
  and temp>0 without the per-key allowance at the single store site
  (`internal/degrade/degrade.go:1563`); lookup gates on
  CacheableTemperature (`internal/degrade/degrade.go:1166`); savings
  recorded per own-cache hit (`internal/degrade/degrade.go:367`, called
  on exact + semantic hits); serve-mode persistence wired when cache
  paths are configured (`cmd/codvian-router/main.go:3027,3042`); flush
  endpoints live (`internal/server/guard.go:332` POST /admin/cache/flush,
  `:214` DELETE /api/cache). Tests re-run 2026-11-19: degrade 5/5
  M6.2b/flush tests PASS (TestM62bDegradeWriteThrough,
  TestM62bDegradeFlushDurable, TestM62bDegradeSavingsViaDecorator,
  TestM62bTypedNilCacheIsPassthrough,
  TestW1FlushTenantOnlyRemovesThatTenantsEntries).
- Nuances (honest, not verdict-changing): (a) `cacheable()` itself still
  never reads temperature — the gating lives at lookup/store, which is
  what closes the bug behaviourally; (b) persistence is wired but
  opt-in — with empty cache paths serve mode stays memory-only by
  design; (c) `internal/cache/key.go:7-11` still says ExactKey is kept
  for existing degrade/relay callers that "must migrate" — no live
  ExactKey call site remains (both paths use ScopedKey), so that comment
  is stale; Go files were out of scope for this task, flagged here for
  the coordinator, not fixed.
- What changed in this pass: only the Evidence cells of the P0-2/P0-3
  rows above (old citations pointed at moved code — cache.go:216-224 is
  now LRU eviction, coalescing.go:27 a base-key comment, degrade.go:
  355-372,569 savings/flush code — not the key/gating code) plus this
  note and a LOG.md entry. CHECKLIST.md untouched: M6 rows 325/327/340/
  341/342 already read ✅/✅ and match the evidence. Other P0s spot-
  checked, no verdict change: P0-1 HasScope now has a live call site
  (`internal/server/server.go:1659`); P0-4 CachedInputUSDPer1M honoured
  (`internal/server/server.go:2541`); P0-5 PEM redaction + stream
  redactor exist (`internal/guardrails/pem.go:159`,
  `pem_stream.go:35`); P0-6 stdio timeout-nil guard
  (`internal/mcptools/stdio.go:168`); P0-9 `code` in the config
  allowlist (`internal/config/config.go:1329`); P0-10 default
  `priority` (`internal/config/config.go:153`); P0-8
  writeResponseHeaders call sites live; P0-7 CountPrompt exists
  (`internal/tokencount/tokencount.go:725`); P0-11 test count re-run at
  M0. Those were code-presence checks only, no test runs.
- Process note: mid-verification the tree briefly did not build (a
  concurrent agent's in-flight edit: `internal/router` referenced
  `NadirTransport`/`accountDeck` before `internal/router/nadir.go` /
  `account_fallback.go` landed, then `internal/autorouter` imported a
  not-yet-present `internal/sla` package). No P0 code was involved and
  the tree built clean again minutes later; the test counts above are
  from the green tree.

- 2026-11-19 22:19 IST (Block A, A5+V-3): CT-8 row (14d catalog "610 providers") PARTIAL -> FULL. TestBaseURLCoverage (internal/catalog/g7_test.go:72) pins 610 total / 379 with base URL / 370 routable / 9 template-only; README quotes 610/370 (A2-A3, V-2 PASS). 14d header FULL 2->3, PARTIAL 2->1; Area 14 rollup FULL 15->16, PARTIAL 41->43 (also fixing the old rollup-vs-parts contradiction), strict 16.9%->18.0%, weighted 39.9%->42.1%. Headline FULL 238->239, PARTIAL 191->190, strict 36.5%->36.7%, weighted 51.2% (unchanged).
- Gap reconciliation (Block A, A5 report hidden_files/A5-reconcile-2026-11-19.md; V-3 PASS 22:19 IST, 30/30 named tests cold + toggle-checks): RT-3 DONE, CT-1 DONE, CT-5 DONE, CT-7 DONE; RT-1 PARTIAL (fresh-config 300 s default exists, Load() has no fill-in -- Block 1 B1), CT-4 PARTIAL (ToolCalling on 6/10 no-auth providers), WT PARTIAL (WT-01..16 pinned, WT-17..32 do not exist). These GAP items have NO dedicated PARITY.md rows, so no verdict cells change for them and the denominator stays 652 -- recorded here, not flipped. Do not flip an unrelated row for them.

### Block 1 — 2026-11-20 (WRONG-7 reconciliation + row 388 correction, V-8/V-9)

- 2026-11-20 00:32 IST (Block 1, B13+V-8): the 7 WRONG rows enumerated (B13 report): 5 were STALE — the wrong behaviour was already fixed in code and the rows never updated. 3 flip to FULL (Area 3 verified-overlay preservation :589, TestVerifiedOverlayPreservesReferenceFreeTier; Area 8b strategies picker :824, now 34 strategies via GET /dashboard/strategies; Area 8b listen address :846, TestGuardFeed, no 8080), 2 re-scope to MISSING (Area 8b stickiness toggle :828 and 5 settings switches :845 — switches gone, read-only status lines, but the underlying controls/backends do not exist). 2 stay WRONG: Area 12 row 1047 (no true failover-switch-time measurement exists) and Area 14d row 1240 (README 179 lines / README.v2.md 260 lines, not the claimed 593). Area 8b header FULL 7->9, PARTIAL 36, MISSING 91->93, WRONG 4->0, strict 5.0%->6.5%, weighted 18.0%->19.4%; Area 3 header FULL 10->11, WRONG 1->0, strict 52.6%->57.9%, weighted 68.4%->73.7%.
- 2026-11-20 00:32 IST (Block 1, B14+V-9): row 388 (Area 1, account-level fallback subset) FULL -> PARTIAL. The 9 strategy values exist and are tested (4/4 PASS cold), but in-request account rotation has no quota-share cap check — orderAccountAttempts never checks caps, no per-key quota source exists (OmniRoute #20561 gap; B14 review). Area 1 header FULL 38->37, PARTIAL 25->26, strict 60.3%->58.7%, weighted 80.2%->79.4%.
- Headline: FULL 239->241, PARTIAL 190->191, MISSING 216->218, WRONG 7->2; strict 36.7%->37.0%, weighted 51.2%->51.6%. Denominator unchanged at 652. parity.sh MATCH after the edits.
- Known gap (pre-existing, NOT fixed here): Area 8b's verdicts still sum to 138 against 139 claimed comparable rows. The Settings sub-tally now sums correctly to 22 (was 21); the remaining 1-row gap is A5-reconciliation territory — no row was invented or dropped to close it.
- 2026-11-20 01:43 IST (Block 1, B9+V-12): Area 4 row 14 (per-key policy fields) PARTIAL -> FULL and row 15 (per-key IP allowlist/blocklist) MISSING -> FULL. B9 (commit ecb4748) wired requireKeyPolicy into register() so IP/endpoint/model policy is enforced on every authenticated route, spend and bare-read alike (6 TestB9BareRead* tests, toggle red-then-green PROVEN by V-12; full internal/server pkgtest ok 316s). Row 15's old claim ("no IP field or check anywhere") was stale — the ip_allowlist/ip_blocklist fields and AllowedIP predate B9; B9 closed the enforcement gap on bare reads, so both rows flip on the same V-12 evidence. Area 4 header FULL 14->16, PARTIAL 3->2, MISSING 6->5, strict 60.9%->69.6%, weighted 67.4%->73.9%. Headline: FULL 241->243, PARTIAL 191->190, MISSING 218->217; strict 37.0%->37.3%, weighted 51.6%->51.8%. Approved-scope (647 rows): FULL 243, PARTIAL 189, MISSING 213, strict 37.6%, weighted 52.2%.
