# docs/STAGE1-VERIFY-QUEUE.md — Stage 1 verification queue (truth protocol)

Built 2026-11-20 ~11:10 IST by S1-VERIFY-QUEUE (read-only; no commits).
Source: docs/STAGE1-QUEUE.md (122 features = B2 44 + B3 26 + B4 52) + current
docs/PARITY.md statuses (grepped by row name, never a stored line number) +
`git show --stat` per builder commit + test-name existence checks against the
tree (4,333 `func Test*(` in `*_test.go`).

## How to use this file (verifiers, one feature per agent, batches of 10)

For each row, the verifier runs the v2 truth protocol: (1) code in the real
request path (files listed); (2) the named test FAILS with the feature off
(toggle-check — disable the feature, watch the test fail, restore);
(3) test run cold by the verifier (`pkgtest.sh` on the package, on the PC);
(4) flip the PARITY.md row ONLY with file:line + test name + "V: pass" + time.
A row marked NO TEST needs a test WRITTEN first (testers, by package, on the PC)
before any verifier can flip it — do not flip on code inspection alone.
A row marked UNCLEAR needs the verifier to judge the coverage question in its
entry and say so in its summary — do not guess a flip.
Never flip a row whose entry carries a DO NOT FLIP note.

## Counts

- Stage 1 queue: 122 features (117 B-numbered rows in the queue tables + 5 batch-1 rows already FULL: :523, :524, :553, :1038, :1040).
- Queued here for verification: **101** rows (PARTIAL/MISSING, built, not blocked).
- NO TEST (test must be written first): **18** rows: B2-14, B2-31, B3-04, B3-07, B3-20, B3-25, B4-01, B4-13, B4-23, B4-24, B4-26, B4-27, B4-28, B4-29, B4-31, B4-33, B4-49, B4-51.
- UNCLEAR (verifier must judge the coverage question): **3** rows: B2-16, B4-20, B4-21.
- Already FULL (skipped): B2-38 (:1173 hedging body-size guard, V-18 PASS, flip already landed).
- Blocked / decision / do-not-build (listed at the end, not queued): **15** items.

## B2 / Area 1 — Strategies / combos / auto scoring (21 rows)

### B2-01 — context-relay
- Row: PARITY.md:489 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: e6bb50f (summariser message selection); 170d771 (RELAY-CONFIG thresholds/TTL); 229b489 (MAIN-WIRE item 4).
- Files: internal/router/relay.go, internal/strategy/affinity.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/router.go (+1 more).
- Tests: TestRelayEngineHandoffCarriedToNextAttempt, TestRelayEngineUnknownProviderRecordsNothing.
- Note: Production summariser missing by design decision; no quota-use signal, no cross-request store — verify the built seam only; row cannot reach FULL until those land.

### B2-02 — reset-aware
- Row: PARITY.md:495 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestResetAwareScoreValues, TestConformanceResetAwareOrdersByScore.
- Note: Producer gap: no measured quota-fraction producer (only SetQuotaHint(0) on quota error) — verify ordering only.

### B2-03 — expiry-first
- Row: PARITY.md:497 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/strategy/quota.go:454; internal/strategy/wiring.go.
- Tests: TestExpiryFirstOrder, TestConformanceExpiryFirstMostUrgentFirst.
- Note: Producer gap: same as B2-02 (booleanised QuotaHint, not a measured fraction) — verify ordering only.

### B2-05 — auto (16-factor pool)
- Row: PARITY.md:501 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 3414845 (SCORER16 Hook16); b0c1c14 (SCORER-WIRE balanced->Hook16Default); cb0fa81 (AUTO-BALANCED ranker hook); 4d92d75 (AUTO-WIRE SetAutoRanker); 229b489 (MAIN-WIRE item 12).
- Files: internal/autorouter/autorouter.go, internal/scoring/scoring.go, internal/strategy/wiring.go, internal/autorouter/autovariants.go, internal/router/autodispatch.go (+4 more).
- Tests: TestSixteen* family (14 tests, verified present).
- Note: Shared 16-factor effort with B2-10/11/12/16 (one scorer, four PARITY facets).

### B2-06 — context-optimized
- Row: PARITY.md:503 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestConformanceDataLimitedStrategiesAreIdentityToday.
- Note: Context windows are not a hook input yet — returns declared order until window data lands.

### B2-07 — cache-optimized
- Row: PARITY.md:504 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestConformanceDataLimitedStrategiesAreIdentityToday.
- Note: Cache warmth is not a hook input yet — returns declared order until warmth data lands.

### B2-08 — quota-share (internal, 23rd)
- Row: PARITY.md:508 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 48cb268 (B208 validStrategies); 36c7348 (B208-TREE); fe0fc3e (B208-CASCADE); 229b489 (MAIN-WIRE item 14 QuotaShareFor).
- Files: internal/strategytree/tree.go, internal/router/tree.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: TestQuotaShareDRRRotatesAcrossRequests, TestQuotaShareDRRMatchesReferenceSequence.
- Note: Per-model bucket saturation has no live source (cannot be built without inventing data); QuotaShareFor config surface in MAIN-WIRE.

### B2-10 — auto 16-factor scoring (DEFAULT_WEIGHTS sum 1.0)
- Row: PARITY.md:510 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 3414845 (SCORER16); b0c1c14 (SCORER-WIRE).
- Files: internal/autorouter/autorouter.go, internal/scoring/scoring.go, internal/strategy/wiring.go.
- Tests: TestSixteen* family (14 tests, verified present).
- Note: Shared 16-factor effort (see B2-05).

### B2-11 — Router strategy rules (default)
- Row: PARITY.md:511 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 3414845 (SCORER16); b0c1c14 (SCORER-WIRE).
- Files: internal/autorouter/autorouter.go, internal/scoring/scoring.go, internal/strategy/wiring.go.
- Tests: TestSixteen* family (14 tests, verified present).
- Note: Shared 16-factor effort (see B2-05).

### B2-12 — Router strategy score
- Row: PARITY.md:512 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 3414845 (SCORER16); b0c1c14 (SCORER-WIRE).
- Files: internal/autorouter/autorouter.go, internal/scoring/scoring.go, internal/strategy/wiring.go.
- Tests: TestSixteen* family (14 tests, verified present).
- Note: Shared 16-factor effort (see B2-05).

### B2-14 — Live intelligence (Arena ELO + models.dev)
- Row: PARITY.md:524 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: a5d18f1 (Arena ELO sync); 593568c (ELO-WIRE taskFit); 229b489 (MAIN-WIRE item 13 arena_elo in config).
- Files: internal/modelsync/intelligence.go, internal/modelsync/modelsync.go, cmd/codvian-router/main.go, internal/scoring/scoring16.go, internal/config/config.go (+1 more).
- Tests: Arena ELO half: NO TEST. models.dev half: TestSyncer* family (shared with B4-03).
- Note: Arena ELO half: NO TEST and not live-verified (live fetch hit HTTP 429 from the VM; parser written against the documented contract). models.dev half: TestSyncer* (shared with B4-03). No ELO/tier signal in scoring yet; dormant until wired.

### B2-15 — Candidate-pool endpoint
- Row: PARITY.md:525 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d380b90 (B215 candidate-pool remainder); da1f044 (B215-REMAIN lockoutReporter+ModelLocked).
- Files: internal/server/autocombo_handler.go, internal/server/autocombo_route.go, internal/server/server.go.
- Tests: TestAutoCombo* (16 tests, internal/server/autocombo_handler_test.go: TestAutoComboHandler*).
- Note: ModelLocked was always false in serve mode until B232-36 added the StaleEngine ModelLockouts forwarder — verify end to end, not just the pool builder.

### B2-16 — auto (balanced, LKGP-sticky)
- Row: PARITY.md:527 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 3414845 (SCORER16); b0c1c14 (SCORER-WIRE).
- Files: internal/autorouter/autorouter.go, internal/scoring/scoring.go, internal/strategy/wiring.go.
- Tests: UNCLEAR: TestSixteen* (scorer half) + TestAutoVariant* (resolver half, 27 tests) — no test names balanced+LKGP-sticky.
- Note: Shared 16-factor effort (see B2-05). UNCLEAR: no test names balanced+LKGP-sticky; TestSixteen* covers the scorer half, TestAutoVariant* the resolver half — verifier must identify the stickiness test or write one.

### B2-17 — auto/chaos
- Row: PARITY.md:534 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestAutoVariantChaosPanelOfFiveOnePerProvider.
- Note: AUTO-FIX OrderedExplicit threaded through all 7 entry points — verify resolver order survives dispatch.

### B2-18 — auto/subscription
- Row: PARITY.md:535 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestAutoVariantSubscriptionFailClosed.
- Note: Subscription rung has no live Billing data source — fails closed by design; verify the fail-closed behaviour.

### B2-19 — auto/thrifty
- Row: PARITY.md:536 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestAutoVariantThriftyRungOrder.
- Note: Verify the 5-rung ladder order at dispatch (thrifty).

### B2-20 — auto/best-* family
- Row: PARITY.md:538 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestAutoVariantBestFamily.
- Note: Vision + reasoning fail open (no vision data, no quality threshold) — record at flip.

### B2-21 — auto/pro-* family
- Row: PARITY.md:539 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestAutoVariantProFamily.
- Note: pro-* fail open in production (no premium classification source) — record at flip.

### B2-22 — 4-tier fallback cascade (Subscription → API Key → Cheap → Free)
- Row: PARITY.md:542 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestCascadeOrderWithTierSource, TestCascadeOrderSubscriptionAPIKeyCheapFree.
- Note: Helper NOT wired (no per-attempt billing class/tier signal); verify the helper + record the unwired gap.

### B2-23 — Endpoint-specific combos (chat, search, embeddings, rerank, images, video, music, OCR, audio)
- Row: PARITY.md:543 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ee8c30c (AUTO-FIX OrderedExplicit).
- Files: internal/router/autodispatch.go, internal/router/router.go.
- Tests: TestEndpointProjectionEmbeddingsRejectsFusion, TestEndpointProjectionUnknownKindErrors, TestMediaPassthroughSearchVerbatim.
- Note: Audio has an engine surface but no provider client, so no audio call can succeed end to end — record at flip.

### B2-24 — Shared attempt budget maxGlobalAttempts, clamped [1, 200]
- Row: PARITY.md:544 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestMaxAttemptsClamp (verified present).
- Note: Already built (clamp [1,200] + config surface); row text stale.

## B2 / Area 2 — Failover, breakers, retries, hedging (4 rows)

### B2-27 — Connection cooldown (one key/account)
- Row: PARITY.md:579 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: e7d5cbe (:529F hard-mode plain-429 fallback).
- Files: internal/router/router.go.
- Tests: TestCooldownFeedLive, TestCooldownFeedLiveEngine, TestCooldownUpdate, TestCooldownUpdatePersists (verified present).
- Note: Stale cooldown_auth.go header comment fixed in MAIN-WIRE (item 15).

### B2-28 — Deadline-failure sub-budget ending combo with clean 504
- Row: PARITY.md:593 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/router/router.go:443-449 (+ config.go; internal/router/streamlifetime.go).
- Tests: TestDeadlineBudgetReturnsDeadlineExceeded, TestDeadlineBudgetStopsChain, TestDeadlineExceededMapsTo504, TestDeadlineBudgetStream (verified present).
- Note: Stream-path counting still a design decision — verify the chat-path sub-budget only.

### B2-29 — Default cross-provider policy: up to 3 provider attempts; prefixed provider/model never rerouted
- Row: PARITY.md:600 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/router/router.go (+ config.go knob).
- Tests: TestG11MaxThreeDistinctProviders, TestG11PrefixedProviderNeverRerouted (verified present).
- Note: No operator override knob (dead-knob pattern, correctly not added); plain chat path does not translate provider/model into routing.only.

### B2-30 — Failover-before-retry + retry knobs
- Row: PARITY.md:602 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 5403cf7 (retry knobs + FailoverBeforeRetry); 229b489 (MAIN-WIRE item 8).
- Files: internal/router/router.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: TestG11FailoverMaxRetriesRespected, TestG11FailoverMaxSetRetriesCapsSet, TestG11FailoverRetryDelayHonoured (verified present).
- Note: Config-file surface + main.go wiring in MAIN-WIRE item 8.

## B2 / Area 6 — Caching (7 rows)

### B2-31 — Semantic similarity method
- Row: PARITY.md:730 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d933d22 (NeuralEmbedder seam); dedfe63 (EMBED-WIRE client wiring).
- Files: internal/cache/cache.go, cmd/codvian-router/main.go.
- Tests: NO TEST (no test references NeuralEmbedder).
- Note: NO TEST. Provider client NOT wired yet (main.go/degrade) — EMBED-WIRE wired one embeddings call through engine.Embed; verify the closure path.

### B2-32 — Persistence tiers
- Row: PARITY.md:738 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 4c209cb (B232-36 exact-cache resume); d417837 (B232-36 injectReasoning+ModelLockouts).
- Files: internal/degrade/degrade.go.
- Tests: File refs, not named tests: m6_2_persist_test.go (10), M6.2b (22 new tests) — run the files.
- Note: Temp-gate divergence left as a recorded divergence. Tests are file refs (m6_2_persist_test.go, M6.2b) — run the files, not a named test.

### B2-33 — Cache analytics
- Row: PARITY.md:747 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: UNCLEAR — no commit recorded in the queue.
- Files: internal/server/cache_stats.go; internal/cache/persist.go.
- Tests: TestCacheStats* (8) + TestM62b* (5) — families, verified present.
- Note: Only gap is the /dashboard/cache page (M10, Stage 3).

### B2-34 — Cache-write tiers (5-min / 1-h)
- Row: PARITY.md:750 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: UNCLEAR — no commit recorded in the queue.
- Files: internal/ledger/ledger.go; internal/wire/anthropic.go; internal/config/prices.go.
- Tests: File ref, not named tests: cache_write_tiers_test.go (10 tests across ledger/wire/provider/config) — run the file.
- Note: Tests are a file ref (cache_write_tiers_test.go, 10 tests across ledger/wire/provider/config).

### B2-35 — Provider prompt-cache markers
- Row: PARITY.md:751 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: f790f58 (cache_control on image_url parts).
- Files: internal/wire/anthropic.go.
- Tests: TestM69c* (11 provider + 5 wire) — family, verified present.
- Note: Rest of the row already built (G13); f790f58 fixed only the dropped image_url cache_control.

### B2-36 — Reasoning cache (2 h)
- Row: PARITY.md:754 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 4c209cb (B232-36); d417837 (B232-36).
- Files: internal/degrade/degrade.go.
- Tests: TestM66* reasoning subset + TestM66b* (17) — families, verified present.
- Note: Tests are the TestM66* family (reasoning subset).

### B2-37 — Search cache (60 s)
- Row: PARITY.md:755 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: UNCLEAR — no commit recorded in the queue.
- Files: internal/cache/search_cache.go.
- Tests: TestM66* search subset (11) — family, verified present.
- Note: Tests are the TestM66* search subset (11).

## B3 / Area 4 — Keys, auth, policy (7 rows)

### B3-01 — MCP scope enforcement
- Row: PARITY.md:659 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/mcp/mcp.go.
- Tests: TestAllScopesKnown, TestCheckScopes, TestCheckKeyTokenLimitsScopes (verified present — verifier must confirm the MCP-scope half).
- Note: Row text stale — verify scopes fail-closed + ON by default.

### B3-02 — Per-key device tracking
- Row: PARITY.md:663 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0697bfd (AUTH-WEB1 Devices card); fe49e02 (SERVER-QUEUE DeviceCount feed); 665ec9a (WEB-DEVICES per-key count).
- Files: web/index.html, internal/server/server.go.
- Tests: TestKeyDeviceFingerprintDistinguishesUA, TestKeyDevicesRecorded (verified present).
- Note: Dashboard rendering landed in WEB-DEVICES (665ec9a).

### B3-03 — Key policy templates / key import
- Row: PARITY.md:664 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/server/guard.go:167.
- Tests: TestKeyImportRoundTrip, TestKeyTemplatesList (verified present).
- Note: Already built (admin_keys_api.go templates + import).

### B3-04 — Dashboard authentication
- Row: PARITY.md:669 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 8f572dc (DASHAUTH login/session).
- Files: internal/server/dashauth.go, internal/server/guard.go, internal/server/server.go, web/index.html.
- Tests: NO TEST (no dashauth test file exists).

### B3-05 — Self-service key introspection
- Row: PARITY.md:676 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/server/keystatus.go:57-143.
- Tests: TestMeSessions, TestMeSessionsEmpty, TestMeSessionsRequiresKey, TestMeSessionsRouteExists (verified present).
- Note: AK-3: Ledger.AgentSessions has ZERO production callers; /v1/me/sessions serves from responses.Record — DO NOT FLIP until AK-3 is wired or deleted.

### B3-06 — Key creation via API / dashboard
- Row: PARITY.md:677 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 308ef47 (AUTH-WEB2 comment fix (already built)).
- Files: web/index.html.
- Tests: TestKeyCreateDefaultsAndBudgetAlias, TestKeyCreateRoundTrip, TestKeyCreateTemplateValidation (verified present).
- Note: Already built; only a stale comment fixed.

### B3-07 — OAuth / JWT gateway auth
- Row: PARITY.md:678 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 15eaddb (OAUTH PKCE+JWT).
- Files: internal/oauth/jwt.go, internal/oauth/oauth.go, internal/server/guard.go, internal/server/oauth_api.go, internal/server/server.go.
- Tests: NO TEST (internal/oauth/ has no test files).
- Note: NO TEST (internal/oauth/ has no test files). "32 OAuth providers" upstream side NOT touched (PARITY.md:1368).

## B3 / Area 7 — Guardrails / PII (18 rows)

### B3-08 — Phone PII
- Row: PARITY.md:765 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18PhoneInternational (verified present).
- Note: Phone is AHEAD (8 country forms vs OmniRoute 2). 15-digit Amex never reaches validCardLuhn (regex is 16-digit groups) — record at flip.

### B3-09 — SSN
- Row: PARITY.md:766 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18SSN (verified present).
- Note: Already built (90f605c, G18); row stale MISSING.

### B3-10 — Credit card
- Row: PARITY.md:767 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18CardLuhn (verified present).
- Note: Card Luhn wired (see B3-08 note on Amex).

### B3-11 — CPF / CNPJ (BR IDs)
- Row: PARITY.md:769 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18CPFAndCNPJ (verified present).
- Note: Already built (90f605c, G18); row stale MISSING.

### B3-12 — IP address v4/v6
- Row: PARITY.md:770 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18IPAddresses (verified present).
- Note: Already built (90f605c, G18); row stale MISSING.

### B3-13 — IBAN + EU national IDs (8 GDPR types)
- Row: PARITY.md:771 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18IBANAndEUIds (verified present).
- Note: Already built (90f605c, G18); row stale MISSING.

### B3-14 — ZA IDs (4 POPIA types)
- Row: PARITY.md:772 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18ZAIds (verified present).
- Note: Already built (90f605c, G18); row stale MISSING.

### B3-15 — Provider-secret patterns
- Row: PARITY.md:773 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18ProviderSecrets (verified present).
- Note: Already built (90f605c, G18); 30 secret rules vs OmniRoute 28.

### B3-16 — Connection strings (mongo/postgres/mysql/redis/amqp)
- Row: PARITY.md:778 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: ?/T7.
- Tests: TestG18ProviderSecrets (verified present — connection strings covered by it per B316-CHECK).
- Note: Stale row; also update row :773 text when :778 flips (it still lists connection strings among missing items).

### B3-17 — Per-type toggles
- Row: PARITY.md:780 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: f583276 (GUARD-TOGGLES DisabledTypes); dd1401e (AUTH-WEB4 per-type switches); 229b489 (MAIN-WIRE item 1).
- Files: internal/config/config.go, internal/guardrails/async.go, internal/guardrails/guardrails.go, internal/guardrails/toggles.go, web/index.html (+2 more).
- Tests: TestG18PerTypeToggles (verified present).
- Note: Backend + dashboard both built; main.go wiring in MAIN-WIRE item 1.

### B3-18 — Custom regex patterns
- Row: PARITY.md:781 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18CustomPatterns (verified present).
- Note: No DONE note in the queue (PII3); rows verified built.

### B3-20 — Request rewrite before provider
- Row: PARITY.md:783 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0548730 (GUARD-SERVER WithRequestRewrite); 229b489 (MAIN-WIRE item 3).
- Files: internal/server/server.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: NO TEST (no Test*Rewrite* guardrail test).
- Note: NO TEST.

### B3-21 — Response sanitization, non-streaming
- Row: PARITY.md:784 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0548730 (GUARD-SERVER response sanitization); f329fbc (RESP-CHECKS stream tool-call args); 229b489 (MAIN-WIRE item 3).
- Files: internal/server/server.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: TestG18ResponseRedactModes, TestResponseGuardrailBlocksNonStreaming, TestResponseGuardrailStreamingLimitation (verified present).
- Note: Non-streaming response sanitization.

### B3-22 — Response sanitization, streaming (SSE)
- Row: PARITY.md:785 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0548730 (GUARD-SERVER streaming); d579514 (RELAY-B322 stream guard); 7eaadec (RELAY-RESP translated-response guard); f329fbc (RESP-CHECKS).
- Files: internal/server/server.go, internal/server/relay.go.
- Tests: TestG18StreamRedactor (verified present).
- Note: Stream half covered by ResponseStreamRedactor; translated-response half by RELAY-RESP (7eaadec).

### B3-23 — Prompt-injection detection
- Row: PARITY.md:786 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestG18InjectionScoring, TestG18InjectionScanBound (verified present).
- Note: Already built (PII3).

### B3-24 — Modality bridges (vision/audio/video reroute)
- Row: PARITY.md:795 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 20adefe (B324 modality bridges).
- Files: cmd/codvian-router/main.go, internal/modality/modality.go, internal/provider/provider.go, internal/provider/registry.go, internal/provider/removed_test.go (+2 more).
- Tests: TestBridgeStreamPanicAfterCommitEndsStream, TestBridgeStreamPanicBeforeCommitFailsOver (verified present — stream-panic tests; verifier must confirm they cover the modality bridge, not just the bridge stream).
- Note: Only VISION reroutes today (no audio/video capability source); describe/transcribe + bridge cache/stats NOT built.

### B3-25 — Dashboard + counters
- Row: PARITY.md:797 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 73a3059 (AUTH-WEB3 Detections-by-type card).
- Files: web/index.html.
- Tests: NO TEST for the card (no Detection* test in internal/server).
- Note: NO TEST for the card itself (no Detection* test in internal/server).

### B3-26 — Route coverage of checks
- Row: PARITY.md:798 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 69cd29b (ROUTECOVERAGE rerank docs scan).
- Files: internal/server/embeddings.go.
- Tests: TestRerankSuccess, TestRerankSuccessLedgerAndTrace, TestRerankExactlyOnceFirstFailureNeverCallsSecond (verified present — rerank path; verifier must confirm the guardrail-scan half).
- Note: Response checks on relay + all streaming responses still unchecked (follow-up).

## B4 / Area 3 — Provider catalog (7 rows)

### B4-01 — Providers with a routable base URL
- Row: PARITY.md:621 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: f5f8a59 (Routable flag in catalog feed).
- Files: internal/server/providers_add.go.
- Tests: NO TEST (TestCatalogFeed does not assert Routable).
- Note: NO TEST (TestCatalogFeed does not assert Routable).

### B4-03 — models.dev sync
- Row: PARITY.md:626 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 054d302 (CATALOG-SYNC LiteLLM sync source); 229b489 (MAIN-WIRE item 10).
- Files: internal/config/prices.go, internal/modelsync/modelsync.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: TestSyncer* family (verified present).
- Note: Main.go wiring in MAIN-WIRE item 10; hardcoded default prices deliberately NOT built (would be invented prices).

### B4-04 — Custom-provider prefix convention
- Row: PARITY.md:629 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: ec59149 (B404 ToProvider prefix); aa7f16b (B404-FOLLOWUP live add path).
- Files: internal/catalog/custom.go, internal/provider/registry.go.
- Tests: TestCustomProvider, TestCustomProviderApplyPrefix (verified present).
- Note: Live add path prescribes wire format from the prefix.

### B4-05 — Free-tier marking
- Row: PARITY.md:632 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/catalog/catalog.go.
- Tests: TestFreeTierPoolsAndRanking, TestFreeTierProviders (verified present).
- Note: Already built.

### B4-06 — Free-tier budget / pools / rankings
- Row: PARITY.md:633 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: f3567e2 (B406-WIRE free_tier block).
- Files: internal/server/providers_add.go.
- Tests: TestFreeTierPoolsAndRanking, TestFreeTierProviders (verified present).
- Note: API half was missing (zero callers); now wired end to end via B406-WIRE (IDs/counts only, no URLs).

### B4-07 — Key-creation links
- Row: PARITY.md:634 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: —.
- Tests: TestCatalogFeed, TestKeyCreationURLs (verified present).
- Note: Already built; only data gap: 18 seeded URLs vs 129 hints — never guess URLs.

### B4-08 — Deprecated / retired handling
- Row: PARITY.md:638 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 5e42881 (deprecated handling).
- Files: internal/server/providers_add.go.
- Tests: TestDeprecatedModelStillForwards (verified present).
- Note: Deprecated handling built.

## B4 / Area 11 — Ledger / analytics / billing (12 rows)

### B4-09 — Session / project / client attribution + /v1/me/sessions
- Row: PARITY.md:1129 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 (verified already built)).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go.
- Tests: TestAgentSessionsRollup, TestAttribution* family (verified present).
- Note: Verified already built by LEDGER2; main.go mapping in MAIN-WIRE item 11.

### B4-10 — Cost computation (tokens × price, all legs)
- Row: PARITY.md:1132 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/ledger/ledger.go:110.
- Tests: TestCostWithCacheTiersGolden, TestCostWithCacheTiersClamping, TestCostWithCachedUnchanged (verified present).
- Note: Already built.

### B4-11 — Gateway-reported cost precedence / exact-cost reconciliation
- Row: PARITY.md:1133 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 (verified already built)).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go.
- Tests: TestG16ReportedCostWinsAndDeltaRecorded, TestG16ReportedCostAbsentFallsBackToComputed, TestG16UsageWireReportedCost (verified present).
- Note: Gateway-reported cost precedence.

### B4-12 — Price coverage out of the box
- Row: PARITY.md:1135 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 054d302 (CATALOG-SYNC prices part); 229b489 (MAIN-WIRE item 10).
- Files: internal/config/prices.go, internal/modelsync/modelsync.go, cmd/codvian-router/main.go, internal/config/config.go, internal/router/cooldown_auth.go.
- Tests: TestPrices* family (verified present).
- Note: Prices part covered by CATALOG-SYNC.

### B4-13 — Flat-rate / subscription $0 treatment
- Row: PARITY.md:1136 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 (verified already built)).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go.
- Tests: NO TEST (no TestFlatRate* test).
- Note: Verified already built by LEDGER2.

### B4-14 — Cache-write tier accounting (5-min vs 1-hour)
- Row: PARITY.md:1137 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d3afaae (B414 exact write cost); d9a4c35 (CACHEBAND exact-number dashboard).
- Files: internal/ledger/ledger.go, internal/server/server.go, web/index.html.
- Tests: TestCacheWriteTierImport, TestCacheWriteTierMigration, TestCacheWriteTierRecordAndSummary (verified present).
- Note: Dashboard band fixed by CACHEBAND (exact number preferred, band only as labelled fallback).

### B4-15 — Refused / fallback-attempt billing
- Row: PARITY.md:1139 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d2d2b92 (B415 cancelled/billed-refusal rows).
- Files: cmd/codvian-router/main.go, internal/router/refusal.go, internal/router/router.go.
- Tests: TestRelayEngineHedgeDoesNotRecordCancelledLoser (verified present); billed-refusal half (failedUsage) — verifier must locate its test.
- Note: Tests asserting old failed-row recording may need updating (hedge_test, combo_test, decision_test, hedge_trace_test) — verifier must check the cancelled contract.

### B4-16 — Cost-saved reporting (cache / compression savings)
- Row: PARITY.md:1143 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 6bc67b9 (CacheTokensSaved seam).
- Files: internal/ledger/ledger.go.
- Tests: TestCostSavedHeader* family + TestSavingsAccounting (verified present).
- Note: CacheTokensSaved seam; async.go insertChunk fix in LEDGER-FIX (0ada264).

### B4-17 — Analytics endpoints
- Row: PARITY.md:1150 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 endpoints); 5ee599c (B417-FOLLOWUP ComboHealth+guard rows).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go, internal/server/guard.go, internal/server/server.go.
- Tests: TestAnalyticsSummaryValues, TestAnalyticsCostsComplete (verified present).
- Note: Docs regen done in test phase (DOCS-REGEN 7063cb2) — TestAPIReferenceCoversEveryRoute must pass with the 4 new routes.

### B4-18 — Retention + export
- Row: PARITY.md:1151 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 (verified already built)).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go.
- Tests: TestDBExport* family (verified present).
- Note: Verified already built by LEDGER2; export cap dbExportMaxEvents=500 already in dbexport.go.

### B4-19 — Idempotency-Key billing safety
- Row: PARITY.md:1153 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d20a143 (B419 Idempotency-Key in handleChatCompletions).
- Files: internal/server/server.go.
- Tests: TestIdempotency* family (18 tests, verified present).
- Note: Non-streaming only (streams cannot be replayed); store was already fully built (idempotency.go, 638 lines).

### B4-20 — Batch / flex service-tier pricing
- Row: PARITY.md:1154 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 22abdc4 (LEDGER2 (verified already built)).
- Files: internal/ledger/ledger.go, internal/provider/client.go, internal/server/analytics_api.go.
- Tests: UNCLEAR: TestBatches* family exists but covers the /v1/batches API, not flex pricing.
- Note: Verified already built by LEDGER2. UNCLEAR: TestBatches* covers the /v1/batches API, not flex pricing — verifier must judge whether that flips this row.

## B4 / Area 10 — Compression (25 rows)

### B4-21 — Engine registry / stacked pipelines
- Row: PARITY.md:1075 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0461bdd (registry); e2b8805 (COMPRESS-SERVERWIRE pre-pass); b58627b (SERVER-PREPASS a2a+legacy); dcce9f7 (RTK-FIELD).
- Files: internal/compress/compress.go, internal/server/server.go, internal/server/a2a.go, internal/server/legacy_completions.go, internal/compress/apply_options.go (+2 more).
- Tests: UNCLEAR: no dedicated registry test; TestApplyMode* + TestRunStacked* exercise it indirectly.
- Note: UNCLEAR: no dedicated registry test; TestApplyMode* + TestRunStacked* exercise the registry indirectly — verifier must judge.

### B4-22 — Lite whitespace/format cleanup
- Row: PARITY.md:1076 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: e2b8805 (COMPRESS-SERVERWIRE); b58627b (SERVER-PREPASS).
- Files: internal/server/server.go, internal/server/a2a.go, internal/server/legacy_completions.go.
- Tests: TestApplyMode* family (verified present).
- Note: Covered by the TestApplyMode* family (lite mode runs through the registry).

### B4-23 — Session-dedup (cross-turn, content-addressed, [dedup:ref sha=…])
- Row: PARITY.md:1077 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 6ab2d64 (session-dedup); e2b8805 (COMPRESS-SERVERWIRE); b58627b (SERVER-PREPASS).
- Files: internal/compress/compress.go, internal/server/server.go, internal/server/a2a.go, internal/server/legacy_completions.go.
- Tests: NO TEST (no test references DedupSession).
- Note: NO TEST (no test references DedupSession).

### B4-24 — CCR (retrieval markers, 24 h + SQLite)
- Row: PARITY.md:1078 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: c7a0b5b (CCR); 9715c6d (CCR Prune fix); e2b8805 (COMPRESS-SERVERWIRE); b58627b (SERVER-PREPASS).
- Files: internal/compress/compress.go, internal/server/server.go, internal/server/a2a.go, internal/server/legacy_completions.go.
- Tests: NO TEST (no test references ApplyCCR).
- Note: NO TEST (no test references ApplyCCR). LossyExactHit (design call) is about CCR behaviour but is not this row's test.

### B4-25 — RTK tool-output filter (56 JSON packs, ANSI/progress strip, failures+tails kept)
- Row: PARITY.md:1079 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: dcce9f7 (RTK-FIELD per-request level).
- Files: internal/compress/apply_options.go, internal/compress/compress.go, internal/provider/client.go, internal/server/compression.go, internal/server/server.go.
- Tests: TestApplyModeRTKRunsFilter, TestApplyModeStackedRunsPipeline (verified present).
- Note: RTK level field landed in RTK-FIELD (dcce9f7).

### B4-26 — Codex Responses tool-output compression
- Row: PARITY.md:1080 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 39a60c7 (Codex); e2b8805 (COMPRESS-SERVERWIRE); b58627b (SERVER-PREPASS).
- Files: internal/compress/compress.go, internal/server/server.go, internal/server/a2a.go, internal/server/legacy_completions.go.
- Tests: NO TEST (no test references Codex).
- Note: NO TEST (no test references Codex).

### B4-27 — Headroom tabular JSON → columnar (SmartCrusher, lossless)
- Row: PARITY.md:1081 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: cc3748d (COMPRESS-ENGINES SmartCrusher).
- Files: internal/compress/compress.go, internal/compress/ionizer.go, internal/compress/relevance.go, internal/compress/smartcrusher.go, internal/compress/tabular.go.
- Tests: NO TEST.
- Note: NO TEST. Lossless columnar, priority 15.

### B4-28 — Ionizer tabular row sampling
- Row: PARITY.md:1082 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: cc3748d (COMPRESS-ENGINES Ionizer).
- Files: internal/compress/compress.go, internal/compress/ionizer.go, internal/compress/relevance.go, internal/compress/smartcrusher.go, internal/compress/tabular.go.
- Tests: NO TEST.
- Note: NO TEST. Row sampling, priority 13, lossy-gated.

### B4-29 — Relevance extractive sentence scoring
- Row: PARITY.md:1083 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: cc3748d (COMPRESS-ENGINES Relevance).
- Files: internal/compress/compress.go, internal/compress/ionizer.go, internal/compress/relevance.go, internal/compress/smartcrusher.go, internal/compress/tabular.go.
- Tests: NO TEST.
- Note: NO TEST. Sentence scoring, priority 18, lossy-gated.

### B4-30 — Caveman rule-based prose condensation (levels lite/full/ultra, user-role default, min 50 chars)
- Row: PARITY.md:1084 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/caveman.go; internal/compress/compress.go.
- Tests: TestApplyModeStandardRunsCaveman (verified present).
- Note: Already built (M7 Caveman); no ultra level, no per-locale rules — record at flip.

### B4-31 — Aggressive summarize + progressive aging (~50% claim)
- Row: PARITY.md:1085 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: acce889 (COMPRESS-WIRE AgeMessages).
- Files: internal/compress/aging.go, internal/compress/compress.go, internal/compress/heuristic.go, internal/compress/modes.go, internal/compress/toolcap.go.
- Tests: NO TEST (no test references AgeMessages).
- Note: NO TEST. Progressive aging, recency window 4, wired into aggressive mode.

### B4-33 — Ultra heuristic pruning + code thinning + binary-search truncation (~75% claim)
- Row: PARITY.md:1087 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: acce889 (COMPRESS-WIRE PruneHeuristic).
- Files: internal/compress/aging.go, internal/compress/compress.go, internal/compress/heuristic.go, internal/compress/modes.go, internal/compress/toolcap.go.
- Tests: NO TEST (no test references PruneHeuristic; TestPrune* are A2A store tests).
- Note: NO TEST. PruneHeuristic wired into ultra mode. (TestPrune* hits are A2A store tests, not compression pruning.)

### B4-38 — MCP accessibility tool-result filtering (smartFilterText, truncation hints)
- Row: PARITY.md:1092 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: acce889 (COMPRESS-WIRE SmartFilterText).
- Files: internal/compress/aging.go, internal/compress/compress.go, internal/compress/heuristic.go, internal/compress/modes.go, internal/compress/toolcap.go.
- Tests: Indirect only: TestG22ToolResultCap* fixture exercises SmartFilterText (g22_wire_test.go:204).
- Note: Indirect coverage only: TestG22ToolResultCap* fixture exercises SmartFilterText; no dedicated test.

### B4-39 — Stacked pipeline RTK → Caveman
- Row: PARITY.md:1093 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/pipeline.go; internal/compress/compress.go.
- Tests: TestApplyModeStackedRunsPipeline, TestRunStacked* (verified present).
- Note: Stacked pipeline RTK→Caveman.

### B4-40 — Named modes/presets + auto-trigger (lite/standard/aggressive/ultra/rtk/stacked)
- Row: PARITY.md:1094 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/modes.go; internal/compress/compress.go; internal/server/….
- Tests: TestApplyMode* family + TestChatCompletionsCompressionInvalidMode (verified present).
- Note: Modes/presets; TestApplyMode* family.

### B4-41 — Adaptive budget dial + escalation ladder
- Row: PARITY.md:1095 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/modes.go.
- Tests: TestApplyModeAggressiveRunsAvailableSubset (verified present — verifier must confirm it covers the escalation ladder).
- Note: Escalation ladder; verify the ladder specifically, not just a mode run.

### B4-42 — Cache-aware compression (preserve system prompt, downgrade, prefixFreeze, QuantumLock)
- Row: PARITY.md:1096 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 258cfad (939-REMAIN engine half); 51b5fb6 (939-INSTALL observer); d2107b9 (939-SERVER ApplyWithOptions).
- Files: internal/compress/apply_options.go, internal/router/router.go, cmd/codvian-router/main.go, internal/server/compression.go.
- Tests: TestCacheAwareFrozenPrefix, TestCacheAwareFrozenPrefixDowngrades, TestPrefixFreezeFreezeChecker, TestPrefixFreeze* (verified present).
- Note: QuantumLock still unbuilt (no implementation exists, NOT invented) — row stays PARTIAL at best; verify engine+server+install wiring only.

### B4-43 — System-prompt preservation modes (always/whenNoCache/never)
- Row: PARITY.md:1097 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/compress.go.
- Tests: TestG22PreserveModes, TestG22ResolveCacheAwarePreserve (verified present).
- Note: System-prompt preservation modes.

### B4-44 — Hard-budget post-pass (compress to ≤ N tokens)
- Row: PARITY.md:1098 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: acce889 (COMPRESS-WIRE HardBudget in Apply).
- Files: internal/compress/aging.go, internal/compress/compress.go, internal/compress/heuristic.go, internal/compress/modes.go, internal/compress/toolcap.go.
- Tests: TestInflationRealCases, TestInflationGuardSkippedOnFidelityRefusal (verified present).
- Note: HardBudget called by Apply when no freeze.

### B4-45 — Protection guarantees (code/URLs/JSON/identifiers)
- Row: PARITY.md:1099 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/fidelity.go; internal/compress/compress.go.
- Tests: TestFidelityGateRefusesTruncatedParenURL, TestCheckLossyRefusesParenURLCutWithSurvivingTail (verified present).
- Note: Protection guarantees (M7-era tests).

### B4-46 — Fidelity gate / risk gate / validation
- Row: PARITY.md:1100 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/fidelity.go; internal/compress/compress.go.
- Tests: TestFidelityGateRefusesTruncatedParenURL, TestFidelityGateServesIntactParenURL (verified present).
- Note: Fidelity gate (M7-era tests).

### B4-48 — Eval harness + measured savings corpus
- Row: PARITY.md:1103 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: NO CODE CHANGE in Stage 1 (already built — verified by the Stage 1 builder; see queue notes).
- Files: internal/compress/evalharness/; docs/compression-savings-2026-11-18.md.
- Tests: TestHarnessFailsOnTamperedNumbers, TestGoldenInputTokens (verified present).
- Note: Eval harness + measured savings table.

### B4-49 — Output styles (Terse prose, Less code, Ponytail, "I have ADHD", Terse CJK)
- Row: PARITY.md:1104 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: d870565 (STYLES five output styles).
- Files: internal/compress/compress.go, internal/compress/styles.go.
- Tests: NO TEST (no Test*Style*/TestTerse*/TestPonytail* test).
- Note: NO TEST. All lossy, mutually exclusive (ApplyWithEngines only; plain Apply never runs them).

### B4-51 — Own-cache tokens_saved accounting
- Row: PARITY.md:1115 — MISSING (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 7eca1fa (TOKENS-SAVED ledger rows); dedfe63 (EMBED-WIRE SetLedgerRecorder).
- Files: internal/degrade/degrade.go, internal/ledger/ledger.go, cmd/codvian-router/main.go.
- Tests: NO TEST (no TestNoCharge*/TestLedgerRecorder* test).
- Note: NO TEST. SetLedgerRecorder wired in EMBED-WIRE; exact=prompt+completion, semantic=prompt only.

### B4-52 — Runtime calibration vs provider-reported usage
- Row: PARITY.md:1118 — PARTIAL (current, grepped 2026-11-20 ~11:10 IST).
- Commit: 0896341 (B452 (already fully built)).
- Files: internal/tokencount/calibration.go.
- Tests: TestCalibration* family + TestReconcileUsageFeedsCalibration (verified present).
- Note: Already fully built + wired; only stale comments fixed.

## NOT VERIFIABLE — blocked / decision / do-not-build

These Stage 1 rows are NOT in the verification queue. A verifier must not pick
them up. Each needs a decision (Raunak) or a separate slice first.

- B2-04 (PARITY.md:498, PARTIAL) — per-pair limit product decision (no source exists; headroom scores 1 until one does)
- B2-09 (PARITY.md:509, PARTIAL) — D2 per-key quota signal (#20561 cap check) — orderAccountAttempts has no cap check
- B2-13 (PARITY.md:523, PARTIAL) — M4 fault-injection only, nothing to build
- B2-25 (PARITY.md:545, PARTIAL) — DECISION: build combo-in-combo vs re-scope DIVERGENCE (trees are the deliberate alternative)
- B2-26 (PARITY.md:548, PARTIAL) — DECISION: hedging default ON 150 ms vs OmniRoute OFF/500 ms (guard itself is built)
- B2-40 (PARITY.md:1204, WRONG) — WRONG row; critic measured 0.6-1.0 ms (051c176, TestChatCompletionsFailoverSwitchTime); flip needs V pass only
- B3-19 (PARITY.md:782, MISSING) — PII token vault — no format agreed (multi-hour B3 slice)
- B4-02 (PARITY.md:624, PARTIAL) — seed data must come from a real source (models.dev), never invented
- B4-32 (PARITY.md:1086, MISSING) — LLMLingua-2 needs ONNX runtime + 57 MB TinyBERT — subsystem slice vs re-scope
- B4-34 (PARITY.md:1088, MISSING) — DO NOT BUILD — deliberate (OmniGlyph, Claude Fable 5 only); re-scope DIVERGENCE
- B4-35 (PARITY.md:1089, MISSING) — DO NOT BUILD — deliberate (LLM compressor = model call); re-scope DIVERGENCE
- B4-36 (PARITY.md:1090, MISSING) — read-lifecycle — no spec anywhere
- B4-37 (PARITY.md:1091, MISSING) — DO NOT BUILD — §2.7 MUST-NEVER-COPY (lossy Caveman on MCP tool descriptions); re-scope DIVERGENCE
- B4-47 (PARITY.md:1102, PARTIAL) — no genuine background consumer exists; needs a server-lane consumer or re-scope
- B4-50 (PARITY.md:1110, MISSING) — native tokenizers — multi-slice subsystem, design decision

## NOT VERIFIABLE — design call

- LossyExactHit (TestChatCompletionsLossyExactHitStillWorks, internal/server) —
  PRE-EXISTING failure, proven unfixable in one run (critic 10:00 IST): the lossy trim
  counts post-CCR token lengths, so keep/drop decisions depend on CCR store warmth and
  two identical lossy requests send different prompts (exact-cache key misses).
  Options in docs/PLAN.md: (A) key the exact cache on the pre-compression request, or
  (B) lossy trim counts markers at their stored-content length. Needs a decision;
  do NOT retry a fix without one.
