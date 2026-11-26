# Phase 1 Completion Ledger (autonomous execution, started 2026-11-21)

Base: main 62234a3 → 8a7c94e (PR #23) → 91bfd22 (PR #22). Approval: full Phase 1 blanket (user directive 2026-11-21 13:54 IST). Next checkpoint: ONLY after entire Phase 1 complete.

## Requirement inventory + status

| ID | Requirement | Status | Evidence level | PR/commit | Notes |
|---|---|---|---|---|---|
| P1-01 | OpenRouter OAuth PKCE login | DONE | real-provider-tested (exchange/validation/storage/discovery/pool/restart/per-key failover/hygiene) | #19 (1737833) | completion QUOTA-BLOCKED/PENDING; cross-account NOT verified |
| P1-02 | Azure Entra ID client credentials | DONE | mock-tested | #19 | live tier blocked: no Azure resource |
| P1-03 | Pool-join routing fix | DONE | real-provider-tested | #20 (e94f51f) | stale-snapshot bug found by live verification |
| P1-04 | Nested model-ID addressing | DONE | mock-tested + code-verified | #21 (62234a3) | live re-verify pending quota |
| P1-05 | Reasoning-only completions not empty | DONE | mock-tested | #21 | |
| P1-06 | 429 policy audit | DONE | documented, unchanged (root cause was P1-03) | #21 | |
| G1 | Streaming usage accounting | DONE | mock-tested + integration-tested | #22 (91bfd22; df1be12/6edf886/8eefaa5/4796c78) | include_usage all-wire carry, ledger reconciliation, client usage chunk + nested OpenAI details, streaming budget overages; live re-verify pending quota |
| G2 | Ollama native /api/chat wire | CLASSIFIED unsupported | justified: OpenAI /v1 compat path covers routing (joinAPIPath); native adds no routing capability | audit §5 G2 | explicit unsupported, not faked |
| G3 | Cohere native vs compat surfaces | DONE (audit) | both clients mock-tested; documented | #21 | no action needed |
| G4 | Cross-account failover live verification | BLOCKED | blocked-external-dependency (needs 2nd OpenRouter account) | — | mechanics account-agnostic; same-account cross-key verified |
| G5 | OpenRouter successful completion | BLOCKED | blocked-external-dependency (free quota exhausted; no credits; paid probes forbidden) | — | $0 spent |
| G6 | Azure Entra real-resource verification | BLOCKED | blocked-external-dependency (no Azure resource) | — | |
| P1-07 | Per-provider capability parity sweep | DONE | matrix capability table: tools/reasoning/discovery/errors/limits/media/key-pools/usage — implementation + suites per area | #23 (8a7c94e) | live per-provider verification stays blocked (G4/G5/G6); ToolCalling catalog metadata = documentation backlog |
| P1-08 | Remaining documented OAuth flows audit | DONE | all 31 CategoryOAuth = subscription/IDE flows; refusal regression tests added (copilot/codex/cursor/kiro/xai-oauth); internal/oauth = gateway client-auth AS; Perplexity MCP OAuth out of routing scope | #23 (8a7c94e) | policy preserved: no cookie/subscription reuse. **Correction (2026-11-27):** the per-entry shape was re-verified — 1 docs-verified surface (claude, external login only), 2 partially documented (github-copilot, clinepass), 28 metadata stubs; pinned by `TestCategoryOAuthInventoryPinsTheTruth`. See docs/PROVIDER_AUTH_MATRIX.md |
| P1-09 | Security review of Phase 1 subsystems | DONE | secrets scan 0; egress/SSRF unchanged + tests green; no new deps; no new routes; negative upstream token counts clamped at ledger/display/telemetry (new fix + tests) | #22 (4796c78) | found+fixed genuine gap: negative parent counts stored verbatim |
| P1-10 | Consolidated verification campaign + final report | DONE | merged main 91bfd22: build+vet clean; full suite 49 ok + 1 known Windows-only env failure (internal/release sha256sum.exe access denied; Linux CI passes it); main CI (race) SUCCESS; audit-fake PASS 314 routes 0 findings; secret/competitor-name scans 0; gen-api-docs no drift; real-binary runtime smoke ALL PASS (bootstrap→provider→stream usage chunk+nested details→no-flag omission→non-stream details→ledger split in analytics) | — | $0 vendor spend in the campaign (local mocks only) |

## Block log

### G1 — streaming usage accounting (DONE, PR #22 merged 91bfd22, CI green)
- provider: stream_options.include_usage on OpenAI-wire streams; client stream_options decoded (lenient unknown keys, 400 malformed).
- router: forwardStream/preread return Done resp; drain copies Usage; runStream reconciles reported split (verbatim numbers, estimate alongside, divergence calibration, reported cost); no-report → labelled fallback; last-report-wins; one row/attempt.
- server: one final usage chunk (empty choices) before [DONE] only on real report; projectClientUsage adds OpenAI nested details (cached/reasoning) for stream + non-stream; streaming budget overage recorded via shared priced-actual core.
- security: Usage.NonNegative + ledger parent-count clamping (negative upstream counts can't corrupt spend).
- Tests: 3 new stream_usage_test.go suites + ledger negative_usage_test.go; full router/provider/server/degrade/ledger packages green.

### P1-08 + P1-07 (DONE, PR #23 merged 8a7c94e, CI green)
- 5 new subscription-OAuth refusal cases; matrix OAuth audit + capability sweep sections.

### P1-09 security review (DONE)
- Secret/path scan on full Phase 1 diff: 0 hits. Competitor-name scan: 0 (Phase 0).
- Egress/SSRF: internal/egress + server egress_guard untouched by Phase 1 (empty diff); egress/web-fetch tests green; providerlogin fixed official host; entra token_uri validated (PR #19 tests).
- Guards: no new routes since PR #19 (tiered + guard-tested).
- Resource safety: bounded usage chunk, no new goroutines; malformed-usage hardening added.
- Supply chain: go.mod/go.sum unchanged.

## Remaining external dependencies (disclosed, not silently deferred)
- G4: second OpenRouter account (operator).
- G5: OpenRouter free-quota reset (~00:00 UTC) OR operator credits (forbidden without permission).
- G6: real Azure resource (operator).
- Vendor-side revocation of the two temp OpenRouter keys (operator 2-click at openrouter.ai/keys; local copies deleted).