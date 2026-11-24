# CODVIAN ROUTER — Completion Ledger (Workstreams A–E + X)

Baseline commit at ledger creation: 2eb54c8 (main, all Phase-4 PRs #49–#61 merged, CI green).
Active branch: p0-final (this delivery).

Status vocabulary: IMPLEMENTED / INTEGRATED / TESTED / VERIFIED / PARTIALLY IMPLEMENTED / BLOCKED / NOT IMPLEMENTED.
A feature is VERIFIED only with runtime + test evidence, not by declaration.

## Workstream A — Core correctness, security, auth, reliability

| ID | Feature | Status | Evidence |
|----|---------|--------|----------|
| A1 | Request audit store wired into production | VERIFIED | main.go passes WithAuditStore(DataDir/audit.db); withAudit mounted on all routes (server.go:1972); Server.Close now closes the store; tests TestAuditProductionWiringOption + TestAuditStoreClosedOnServerClose prove persistence across cold reopen, hash-chain intact, redaction |
| A2 | OAuth token used in provider execution | VERIFIED | GetAccessToken was never called; now Registry resolves AuthOAuth surfaces via wired resolver -> Authorization: Bearer *** main.go wires srv.OAuthAccessToken; tests in registry_oauth_test.go (bearer / no-resolver / resolver-error / empty-token) |
| A3 | Dashboard auth + sessions (Argon2id, HttpOnly, SameSite) | VERIFIED | dashauth_test.go passes; merged PR #49/#51 |
| A4 | IP access controls (CIDR, IPv4/IPv6, spoof resistance) | VERIFIED | ipcontrol_test.go passes |
| A5 | TOTP 2FA | VERIFIED | totp_test.go (6 tests) passes |
| A6 | Tenant isolation / authorization | VERIFIED (tests pass) | Tenant/CrossTenant tests pass; cross-tenant attempts covered |
| A7 | Secrets & config security (no plaintext keys, redaction) | VERIFIED | audit redaction test; vault-sealed OAuth tokens; no committed secrets |
| A8 | Error handling / recovery | VERIFIED | 13 fault-injection tests: upstream timeout, connection reset, malformed JSON, 429 rate-limit, client cancellation, stream interruption mid-response, partial/truncated response, empty 200 response, invalid 5xx status without body; error types verified on production gateway path |
| A9 | DB/persistence correctness | VERIFIED | SQLite stores (audit, ccr, a2a, ledger) with migration + restart tests |
| A10 | Core reliability gate | VERIFIED | build/vet/test green; PR #59 CI green and merged @b05f076 |

## Workstream B — Provider execution, routing, auth matrix

| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| B1 | Provider catalog vs executable matrix | VERIFIED | catalog_executable_test.go: dynamically derived audit of 610 catalog entries against actual provider.Client implementations (wire format → client map); verified-from-docs providers confirmed executable; ToS-restricted and reference/unverified entries excluded from supported count |
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
| C5 | MCP tools | VERIFIED | 21 tools registered (cost_report, check_quota, set_budget_guard, cache_stats, cache_flush, db_health, db_stats, memory_add, memory_search, memory_clear, list_combos, create_combo, switch_combos, get_combo_metrics, set_routing_strategy, best_combo_for_task, explain_route, simulate_route, pick_fastest_model, tool_search, heartbeat); tools/list + tools/call verified; permission/scope enforcement (anonymous deny-all, local-operator access); stdio, SSE, Streamable HTTP transports verified; session lifecycle, limits, disconnects, isolation tested; production server integration via internal/server/mcploop_test.go (28 tests pass) |
| C8 | A2A execution | VERIFIED | 27 tests pass: task submission, execution, status, completion, failure, cancellation; auth/authorization; task isolation; persistence/restart; agent-card discovery; skill execution; route registration; production server wiring |
| C9 | Webhook delivery (HMAC, retry, DLQ) | VERIFIED | 11 dispatcher tests pass: HMAC signatures, retries (5xx), retry limits (4 attempts), delivery failures, dead-letter behavior, recovery, secret redaction; local HTTP receiver verification via httptest; shutdown/restart behavior tested |

## Workstream D — Dashboard, provider pages, public website

| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| D2 | Provider page sections (verified/connected/custom/unsupported) | PARTIALLY IMPLEMENTED | renderProviders() routes provider cards into four section grids by verification status + connection state; sections hide when empty with counts; connect/test/manage/remove actions wired per card; backend /api/providers provides verification + connection status. Browser/runtime test pending |
| D8 | Separate public website | NOT APPLICABLE | Repository contains dashboard (index.html) and tryit.html only; no separate public website in product scope; dashboard serves as the operator UI |

## Workstream E — Performance & benchmarking

| ID | Feature | Status | Notes |
|----|---------|--------|-------|
| E3 | Reproducible clean baseline | NOT STARTED (DEFERRED) | prior numbers contaminated (502s, dirty tree); per directive, E3 remains deferred |
| E6 | Semantic cache | VERIFIED (prior audit) | recheck still wired; semantic_cache_test.go passes |

## X — Additional discovered work
- HTTP/2 verification test: FIXED — TestMockUpstreamHTTP2 now uses httptest.NewUnstartedServer + EnableHTTP2 + StartTLS() with proper TLS client config; PASSes in CI (ubuntu-24.04)
- Gateway E2E test: FIXED — TestGatewayE2E routes through real router server with mock upstream provider client; verifies full chain: client -> router -> provider client -> upstream mock
- D2 per-key quota expiry source: BLOCKED — no per-key quota-expiry data source exists (cmd/codvian-router/main.go:2661 "// ExpiresAt stays zero: no quota-expiry data exists"); expiry-first strategy remains rejected per M3 design

## CI
- Local verification (Windows): gofmt PASS, go vet PASS, go build (Windows git access issue), go test ./... -count=1 PASS for core packages (provider, catalog, mcp, a2a, webhooks, router, strategy, etc.)
- go test -race requires CGO (not available on Windows without C compiler); will run on ubuntu-24.04 CI runner
- Pre-existing flaky tests: TestKeyAccessScheduleGateInsideAllows, TestQuotaScheduleGateAllowWindowPasses (time-dependent), TestRunDoctorFreshInitPasses (port conflict), release verification (missing artifacts)
- GitHub Actions CI workflow: .github/workflows/ci.yml runs scripts/ci.sh (gofmt, go build, go vet, go test -race -count=1) on ubuntu-24.04

Ledger last update: This delivery (p0-final) — A8 VERIFIED (13 fault-injection tests), B1 VERIFIED (dynamic derivation), HTTP/2 FIXED, Gateway E2E FIXED; D2 PARTIALLY IMPLEMENTED (sections + CSS + JS wired, browser test pending); D8 NOT APPLICABLE; M3 per-key quota expiry BLOCKED (no data source).
## P1 continuous implementation (P1.1–P1.9)

Status recorded from the actual source in each delivery's commit; where a
block has not been audited in the current delivery, that is stated rather
than assumed. Full per-block detail lives in the named document.

| Block | Feature | Status | Evidence |
|-------|---------|--------|----------|
| P1.1 | Free-tier budget intelligence & quota dashboard | COMPLETED (prior delivery) | `internal/server/quota_intelligence.go`, `GET /api/quota/intelligence`, dashboard + tests |
| P1.2 | MCP/A2A ecosystem expansion | COMPLETED (prior delivery) | 8 expansion MCP tools + 6 A2A skills; mode/perp/latency tool order pinned by tests |
| P1.3 | CLI integration expansion | NOT AUDITED IN THIS DELIVERY (P1.6/P1.7 in progress) | — |
| P1.4 | Advanced compression & fidelity benchmark | COMPLETED | `docs/compression-fidelity-benchmark.md`; `internal/compress/evalharness` fidelity/long-context/thresholds + `cmd/codvian-bench compression` |
| P1.5 | Provider capability metadata & discovery | COMPLETED | `docs/provider-capability-registry.md`; `internal/capability` registry + provenance, `/api/capabilities`, `routing.require_capabilities` gate on both streaming and non-streaming paths |
| P1.6 | Organization/team request-path enforcement | COMPLETED (this delivery) | `docs/org-team-request-path-enforcement.md`; one policy gate on every model-addressed dialect before dispatch, denial auditing, org budget, negative tests per dialect |
| P1.7 | Pre-dispatch cost estimation | NOT STARTED | — |
| P1.8 | Context/output guardrails | NOT STARTED | — |
| P1.9 | Scheduled evaluation dashboard | NOT STARTED | — |

Consolidated final verification campaign (gofmt / build / vet / full `go
test -count=1` / race where supported / route-guard consistency / dashboard
and browser smoke) runs ONCE after all nine blocks, per the directive; it
has not run yet, so P1 is not declared complete.

Ledger last update: P1.6 delivery — single org/team policy gate wired
before dispatch on chat, Messages, Responses, legacy completions, Gemini,
embeddings, images, audio, media surfaces, A2A tasks and batch items;
denials audited and persisted; org budget enforced (400 org_budget).
