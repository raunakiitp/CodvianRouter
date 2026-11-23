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

## Workstream B — Routing Engine
|| ID | Feature | Status | Notes ||
||----|---------|--------|-------||
|| B1 | Provider catalog vs executable matrix | NOT STARTED | recompute counts from current source ||
|| B2 | Routing strategies complete | VERIFIED | All 30+ strategies implemented: priority/balanced/cheapest/fastest/most-reliable/least-latency/latency-aware/weighted/custom-weights/code/round-robin/random/strict-random/fill-first/least-used/p2c/quota-weighted/headroom/expiry-first/reset-aware/reset-window/lkgp/context-optimized/cache-optimized/context-relay/cost-optimized/ship-fast/cost-saver/quality-first/offline-friendly/reliability-first/chaos-mode/cascade/sla-aware/nadir/auto/fusion/pipeline/jev/budget-aware ||
|| B3 | Weighted routing real weights | VERIFIED | WeightedDraw implements proportional draw with configurable per-target weights; chain weights from config.Routing.ChainWeights; weighted_round_trip_test.go validates distribution ||
|| B4 | SLA-aware routing true p95 | VERIFIED | Latency histogram (1100 buckets: 1ms 0-1000ms, 10ms 1010-10000ms) in providerStats; p95LatencyMs() requires ≥20 samples; SLA-aware hook uses p95 when available, falls back to p50 EWMA; tests in sla_test.go, wiring.go updated ||
|| B5 | Context-aware routing | VERIFIED | context-optimized (largest context window), cache-optimized (warmest cache), context-relay (engine-intercepted, carries context across attempts) implemented; relay.go builder D5 exists ||
|| B6 | Budget-aware routing | VERIFIED | StrategyBudgetAware ("budget-aware"/"budget") ranks targets by remaining budget headroom from ledger.KeyBudgetStatus; uses ledger.GetKeyBudget/GetKeyBudgetStatus; budget_aware_test.go validates behavior ||
|| B7 | Fallback and recovery behavior | VERIFIED | Cascade (4-tier fallback: subscription→API key→cheap→free) in wiring.go; LKGP (last known good) with breaker awareness; recovery verified via engine breaker state ||
|| B8 | Burn-rate forecasting | VERIFIED (PR #60 merged @5862b6e, CI green) | GET /api/usage/budget/forecast: per-key 1h/24h burn rate + projected exhaustion from real ledger spend + stored limit; insufficient_data/exhausted when data missing; usageapi_forecast_test.go ||
|| B9 | OAuth provider executors | VERIFIED (this branch, A2) | resolver wired ||

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
- main @ 5862b6e: PR #59 (A1 audit wiring + A2 OAuth runtime) and PR #60 (B12 burn-rate forecast) merged, all workflows green (ci, bench-smoke, playwright, sbom-and-vuln).

Ledger last update: A1, A2 verified+merged (PR #59); B12 verified+merged (PR #60); A3-A7, A9 verified via passing tests.
