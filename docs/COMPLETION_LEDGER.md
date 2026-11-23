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
| A10 | Core reliability gate | IN PROGRESS | build/vet/test green locally; PR #59 CI pending |

## Workstream B — Provider execution, routing, auth matrix
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| B1 | Provider catalog vs executable matrix | NOT STARTED | recompute counts from current source |
| B7 | Routing strategies complete | PARTIALLY | recheck auto/fusion/pipeline/jev/weighted/sla/cache/combo |
| B8 | Weighted routing real weights | NOT VERIFIED | check equal-weight fallback |
| B12 | Burn-rate forecasting | NOT IMPLEMENTED (per prior audit) | implement |
| B5 | OAuth provider executors | VERIFIED (this branch, A2) | resolver wired |

## Workstream C — API, streaming, MCP, A2A, webhooks
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| C9 | Webhook delivery (HMAC, retry, DLQ) | PARTIALLY | WithWebhooks wired (main.go:5110); verify delivery worker + events |
| C2 | WebSocket /v1/ws | NOT VERIFIED | check existence |
| C5 | MCP tools | PARTIALLY | recompute count; verify execution |
| C8 | A2A execution | PARTIALLY | verify task execution path |

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
- main @ 2eb54c8: all workflows green (ci, browser-smoke, bench-infra-smoke, supply-chain).
- PR #59 (feat/ws-a-core-correctness): CI PENDING at ledger creation.

Ledger updated as workstreams complete. Last update: Workstream A fixes landed (A1, A2).
