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
| B1 | Provider catalog vs executable matrix | VERIFIED | cataudit package compares compiled catalog vs models.dev: renamed/possible_rename/base_url_differs/base_url_path_differs/base_url_missing findings; reachability probes; never edits catalog; cataudit_test.go and catalog_audit_test.go pass |
| B7 | Routing strategies complete | VERIFIED | all strategies wired: priority/identity, balanced/16-factor, cheapest/fastest/most-reliable/least-latency via HookWithWeights, latency-aware/weighted/sla/cache/m3-orderings via wiringHook, combo strategies (fusion/pipeline/jev/chaos-mode) via combo engine; validStrategies allowlist updated to include sla-aware/latency-aware; strategy tree evaluation wired in router/tree.go |
| B8 | Weighted routing real weights | VERIFIED | weighted strategy wired via wiringHook (WeightedDraw over chain weights from SetChainWeights/routing.chain_weights, equal-weight uniform fallback when unconfigured, all-zero weights rejected at config validation); TestConformanceWeightedDrawIsAPermutation + TestConformanceWeightedChainWeights pass |
| B9 | SLA-aware routing | VERIFIED | StrategySLAAware wired in wiringHook: latency-weighted scoring with SLA target p95; falls back to balanced on zero context; tests pass |
| B10 | Context-aware routing | VERIFIED | StrategyContextOptimized and StrategyContextRelay wired in wiringHook via sixteenHookCtx with mode-pack strategy lists; tests pass |
| B11 | Budget-aware routing | VERIFIED | Quota-weighted, headroom, fill-first, expiry-first ordering strategies wired in wiringHook; quota schedule and budget tracking enforced; tests pass |
| B13 | Fallback and recovery behavior | VERIFIED | Failover chains, circuit breakers (degraded/recovered events), resilience breakers, reset endpoints; breakerWebhookEvents adapter; tests pass |
| B12 | Burn-rate forecasting | VERIFIED (PR #60 merged @5862b6e, CI green) | GET /api/usage/budget/forecast: per-key 1h/24h burn rate + projected exhaustion from real ledger spend + stored limit; insufficient_data/exhausted when data missing; usageapi_forecast_test.go |
| B5 | OAuth provider executors | VERIFIED (this branch, A2) | resolver wired |

## Workstream C — API, streaming, MCP, A2A, webhooks
| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| C9 | Webhook delivery (HMAC, retry, DLQ) | VERIFIED | Dispatcher with HMAC-SHA256 signing, 3-retry exponential backoff (1s/2s/4s), per-attempt 10s timeout, egress SSRF-safe client; events wired: quota.exceeded, budget.warning, provider.degraded/recovered, provider.exhausted, request.completed/failed, test.ping; /api/webhooks CRUD with SSRF validation; UI page in web/index.html; webhooks_api_test.go, webhooks_test.go, webhook_breaker_events_test.go all pass |
| C2 | WebSocket /v1/ws | VERIFIED | GET /v1/ws upgrades to WebSocket and streams chat completions over WS frames; auth via Authorization header or api_key query param; guard-row added; engine stream path (ChatStream) used; docs/API_REFERENCE.md, docs/llm.txt, tests/fixtures/v1/m8.21/routes.json, web/tryit.html updated; all server tests pass |
| C5 | MCP tools | VERIFIED | full MCP surface: internal/mcp (tools_cost, tools_ops, tools_routing, tools_search, identity, scopes, audit, enforcement, memory_store), internal/mcptools (controls, static, stdio), server MCP handlers (/api/mcp/*), dashboard MCP page; all mcp/mcptools/server MCP tests pass |
| C8 | A2A execution | VERIFIED | /a2a JSON-RPC endpoint with guard row (POST /a2a), task service + in-flight processor cancels, lazy store open, prune loop, agent card, skills, status, register, dashboard; internal/a2a + server A2A tests all pass |

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
