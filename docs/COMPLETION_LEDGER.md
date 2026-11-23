# CODVIAN ROUTER — Completion Ledger (Workstreams A–E + X)

Baseline commit at ledger creation: 2eb54c8 (main, all Phase-4 PRs #49–#61 merged, CI green).
Active branch: feat/ws-a-core-correctness (PR #59).

Status vocabulary: IMPLEMENTED / INTEGRATED / TESTED / VERIFIED / PARTIALLY IMPLEMENTED / BLOCKED / NOT IMPLEMENTED.
A feature is VERIFIED only with runtime + test evidence, not by declaration.

## Workstream A — Core correctness, security, auth, reliability

| ID | Feature | Status | Evidence |
|----|---------|--------|----------|
| A1 | Request audit store wired into production | VERIFIED (fixed this branch) | main.go passes WithAuditStore(DataDir/audit.db); withAudit mounted on all routes (server.go:1972); Server.Close now closes the store; tests TestAuditProductionWiringOption + TestAuditStoreClosedOnServerClose prove persistence across cold reopen, hash-chain intact, redaction |
| A2 | OAuth token used in provider execution | VERIFIED (fixed this branch) | GetAccessToken was never called; now Registry resolves AuthOAuth surfaces via wired resolver -> Authorization: Bearer <token>; main.go wires srv.OAuthAccessToken; tests in registry_oauth_test.go (bearer / no-resolver / resolver-error / empty-token) |
| A3 | Dashboard auth + sessions (Argon2id, HttpOnly, SameSite) | VERIFIED | dashauth_test.go passes; merged PR #49/#51 |
| A4 | IP access controls (CIDR, IPv4/IPv6, spoof resistance) | VERIFIED | ipcontrol_test.go passes |
| A5 | TOTP 2FA | VERIFIED | totp_test.go (6 tests) passes |
| A6 | Tenant isolation / authorization | VERIFIED (tests pass) | Tenant/CrossTenant tests pass; cross-tenant attempts covered |
| A7 | Secrets & config security (no plaintext keys, redaction) | VERIFIED | audit redaction test; vault-sealed OAuth tokens; no committed secrets |
| A8 | Error handling / recovery | PARTIALLY VERIFIED | core paths tested; fault-injection coverage partial |
| A9 | DB/persistence correctness | VERIFIED | SQLite stores (audit, ccr, a2a, ledger) with migration + restart tests |
| A10 | Core reliability gate | VERIFIED | build/vet/test green; PR #59 CI green and merged @b05f076 |

## Workstream B — Provider execution, routing, auth matrix
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| B1 | Provider catalog vs executable matrix | NOT STARTED | recompute counts from current source |
| B5 | OAuth provider executors | VERIFIED (PR #52) | resolver wired |
| B7 | Routing strategies complete | VERIFIED (PR #62 merged @5bba7cf, CI green) | auto/fusion/pipeline/jev/weighted/sla/cache/combo all wired and tested |
| B8 | Weighted routing real weights | VERIFIED (PR #62 merged @5bba7cf, CI green) | chain weights, equal-weight fallback verified |
| B9 | SLA-aware routing | VERIFIED (PR #62 merged @5bba7cf, CI green) | p95 target handling via SLAAware strategy |
| B10 | Context-aware routing | VERIFIED (PR #62 merged @5bba7cf, CI green) | context-optimized selection + context relay |
| B11 | Budget-aware routing | VERIFIED (PR #62 merged @5bba7cf, CI green) | quota-weighted, headroom, fill-first, expiry-first |
| B12 | Burn-rate forecasting | VERIFIED (PR #60 merged @5862b6e, CI green) | GET /api/usage/budget/forecast: per-key 1h/24h burn rate + projected exhaustion from real ledger spend + stored limit; insufficient_data/exhausted when data missing; usageapi_forecast_test.go |
| B13 | Fallback and recovery | VERIFIED (PR #62 merged @5bba7cf, CI green) | cascade failover chains, circuit breakers, recovery events |

## Workstream C — API, streaming, MCP, A2A, webhooks
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| C2 | WebSocket /v1/ws | VERIFIED (PR #61 merged @75f3b98, CI green) | GET /v1/ws upgrades to WebSocket, streams ChatStream frames, auth via Bearer or api_key, guard row registered |
| C5 | MCP tools | PARTIALLY | recompute count; verify execution |
| C8 | A2A execution | PARTIALLY | verify task execution path |
| C9 | Webhook delivery (HMAC, retry, DLQ) | PARTIALLY | WithWebhooks wired (main.go:5110); verify delivery worker + events |

## Workstream D — Dashboard, provider pages, public website
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| D2 | Provider page sections (verified/connected/custom/unsupported) | NOT VERIFIED | recheck renderProviders |
| D8 | Separate public website | NOT VERIFIED | check if exists |

## Workstream E — Performance & benchmarking
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| E3 | Reproducible clean baseline | NOT STARTED | prior numbers contaminated (502s, dirty tree) |
| E6 | Semantic cache | VERIFIED (prior audit) | recheck still wired |

## X — Additional discovered work
Pending full re-scan after A–E.

## CI
- main @ f537048: PR #59 (A1/A2), PR #60 (B12), PR #61 (C2 WebSocket + strategy tree allowlist), PR #62 (Workstream B routing engine: B7/B8/B9/B10/B11/B13) merged, all workflows green (ci, bench-smoke, playwright, sbom-and-vuln).

Ledger last update: B7, B8, B9, B10, B11, B13 verified+merged (PR #62); C2 verified+merged (PR #61); B5 verified (PR #52); A1, A2 verified+merged (PR #59); B12 verified+merged (PR #60); A3-A7, A9 verified via passing tests.
