# Codvian Router — Post-P1 Architecture Map

**Audited HEAD:** `08cadc03db43c27fe014f47a020134490ed2723e` (main) · read-only audit, no files modified.

Package-level map of the real repository (44 `internal/` packages, `cmd/`, `web/`, `scripts/`, `tests/`). File:line references are to entry points and important interfaces, not exhaustive listings.

---

## 1. Runtime entry points

| Entry point | Path | Role |
| --- | --- | --- |
| CLI | `cmd/codvian-router/main.go` (6,225 LOC) | dispatch at `main.go:624-694`: `serve`/`init`/`wizard`/`run`/`keygen`/`eval`/`eval-feedback`/`validate`/`tray`/`doctor`/`configure`/`setup`/`mcp`/`provider`/`catalog`/`verify-release`/`release`/`db`/`status`/`stop`/`restart`/`logs`/`health`/`update`/`clients`/`help`/`version` |
| HTTP gateway | `internal/server/server.go` (8,056 LOC) | `ServeHTTP` (`:2044`), route registration `register` (`internal/server/guard.go:721`), 239 `register` calls → 347 table routes |
| Routing engine | `internal/router/router.go` (7,882 LOC) | `ChatWithChainStrategy` (non-stream), `runStream` (stream), breakers, cooldown, context fit, budget gate |
| Bench/SBOM tools | `cmd/codvian-bench`, `cmd/release-sbom` | compression benchmark; SBOM fallback generator |
| Dashboard | `web/` (embedded via `web/web.go`) | static shell + `index.html`, `tryit.html` |

---

## 2. Subsystem map

### 2.1 Gateway and request lifecycle
`internal/server`: route table + tier guards (`guard.go`), chat handler (`server.go:5005`), streaming (`handleChatStream`, `:7324`), dialect handlers (`relay.go`, `responses.go`, `gemini_native.go`, `legacy_completions.go`, `embeddings.go`, `images.go`, `audio.go`, `media_verify.go`, `batches*.go`, `a2a.go`, `mcp*.go`), telemetry (`telemetry.go`), headers (`headers.go`), panic isolation (`noteServerPanic`, `writePanicFailure`).

### 2.2 Provider registry and adapters
`internal/provider` (12,000+ LOC across 40 files): `client.go` (2,519) HTTP client + wire selection, `registry.go` (2,339) provider/model registry, `pool.go` (transport pooling), `taxonomy.go` (error classification), `tfo.go`, `refresh.go`, `wire/`-facing encoders in `internal/wire` (5,549 LOC, one codec per dialect).
**Interfaces:** `provider.Client` (Chat/Embed/Rerank/Images/Audio/…), `provider.ChatRequest/ChatResponse`, `UpstreamError`/`RateLimitError`/`QuotaError`.

### 2.3 Model catalog and metadata
`internal/catalog` (12,349 LOC): `providers_reference.go` (6,742) generated reference table (483 OpenAI-wire entries, **132 `WireUnknown` with nil base URLs** — AUD-PROV-001), `providers_additional.go`, `auth.go` (auth-kind → header + wire resolution), `custom.go` (operator providers). `internal/modelsync` syncs models.dev prices (never overwriting operator prices). `internal/cataudit` self-audits catalog executability.

### 2.4 Authentication and credentials
`internal/config` gateway keys (hashed, `crypto/subtle` compare), `internal/server/server.go:2169` `requestKey` + `:2207` `authorized` + `:2078` `keyPolicyAllows` + `:2284` `callerKeyID` (**the identity split that is AUD-SEC-001**), `internal/oauth` (JWT + refresh tokens), `internal/providerlogin` (OpenRouter PKCE), `internal/crypto`.

### 2.5 Routing and fallback
`internal/router`: attempt/chain resolution (`chainFor`), `applyContextFit` (`contextfit.go`), `filterByRequiredCapabilities` + `modelToolsCapable` (P1.5 gate), fallback chain resolution (`fallback_chains.go`), breakers (`breaker_profile.go`, `breaker_store.go`), cooldown (`cooldown.go`), stream recovery (`streamrecovery.go`), hedging (`hedge*`), panning/fusion (`panel.go`, `combo.go`), `deadline_budget.go`, `waitbudget.go`, `watchdog.go`, `nadir.go`.

### 2.6 Streaming vs non-streaming execution
Non-streaming: `chatWithChainStrategy` → single dispatch per attempt. Streaming: `runStream` → SSE framing per dialect, `streamlifetime.go`, `streamrecovery.go` (≤4 same-target re-opens), redacting stream guardrail. Terminators verified per dialect (OpenAI `[DONE]`, Anthropic `message_stop`, Gemini bare `data:`).

### 2.7 Cost estimation and billing
`internal/server/cost_estimation.go` (`/api/cost/estimate`, `/api/cost/reconcile`, `callerKeyIDFromContext` carrier), `internal/ledger` price book integration (`ledger.go`, `keybudget.go`, `budget_memo.go`, `reservation.go`), `internal/config` `PriceBook`/`ModelPrice`.

### 2.8 Usage accounting
`internal/ledger`: `RecordUsage`, async writer (`async.go`, bounded queue, **drops failed batches** — AUD-REL-002), spend reservations (`reservation.go`, TOCTOU-serialised), drift summary, `usageapi.go` surfaces.

### 2.9 Quotas and rate limiting
`internal/adaptive` (`limiter.go`, `ratelimit.go` learned hints), `internal/server/keytraffic.go` (per-key RPM/TPM/RPD/token caps), `internal/ledger` key budgets, `internal/server/quota_intelligence.go` + `quotapools` (P1.1), quota schedules.

### 2.10 Free-tier enforcement
Catalog free-tier metadata + `quota_intelligence.go` enumeration; unpriced usage policy `fail_closed` by default for capped keys (`ledger/keybudget.go:67-75`).

### 2.11 Organization and team management
`internal/org` (members, teams, RBAC, audit log), `internal/server/teams.go` (the single `enforceTeamPolicy` gate, 9 dialect call sites + batch/A2A), `internal/server/org_api.go` (admin CRUD).

### 2.12 Capability discovery and registry
`internal/capability` (`registry.go` Build + provenance, `capability.go` helpers) served by `/api/capabilities`, `/api/providers/{id}/capabilities`, refresh endpoint; consumed by the router's `require_capabilities` filter. **Defects:** `WireUnknown → Streaming verified` (AUD-PROV-003), `time.Now()` in determinism claim (AUD-PROV-005).

### 2.13 Context management and token counting
`internal/tokencount` (5,000+ LOC): `Count`/`CountPrompt`/`CountPromptWire`/`CountPromptFull`, BPE memo (cap 4096, wholesale clear), char/4 fallback, wire framing (tools + multimodal reference frames).

### 2.14 Compression
`internal/compress` (8,635 LOC): strategy tiers, `evalharness` (fidelity/long-context/thresholds), `workerpool.go`, `modes.go` (honest "not implemented" for optional tiers).

### 2.15 MCP integration
`internal/mcp` (protocol), `internal/mcptools` (3,118 LOC, 32 tools), `internal/server/mcp.go` (stdio), `mcp_http.go` (2,514, streamable HTTP + SSE), MCP mirror of quota/cost/analytics tools.

### 2.16 A2A integration
`internal/a2a`: agent card (`.well-known/agent.json`), task store with owner isolation + eviction caps (`store.go`), lifecycle (`a2a.go`), 12 skills (`skills.go`, P1.2), A2A status API (`a2a_status.go`).

### 2.17 CLI
`cmd/codvian-router`: commands as §1 plus `setup.go` (13 agent config managers), `doctor_configure_mcp.go` (2,252), `lifecycle*.go`, `headless_run_wizard.go`, `catalog_audit.go`, `clients.go` (P1.3 matrix), `release*.go`, `update.go`.

### 2.18 Evaluation framework and scheduled evaluation
`internal/eval`: `eval.go` harness (per-case recover), `schedule.go` (Schedule/Store/Scheduler, regression detection, P1.9 `PassRateTrend` + `CaseFailure`), `permanent_cases.go`, `internal/server/eval_schedules_api.go` (CRUD/history/trends + scheduler startup). **Defects:** non-atomic persist + swallowed start error (AUD-REL-003), `Upsert` error swallowed (AUD-CODE-006), no top-level recover (AUD-REL-006).

### 2.19 Storage and persistence
`internal/ledger` (SQLite WAL, `busy_timeout=5000`, `SetMaxOpenConns(1)`), `internal/cache` (exact/semantic/reasoning; SQLite or Redis — **cache DBs lack WAL/busy_timeout**, AUD-REL-004), `internal/files` (temp+rename+fsync), `internal/health` (disk-journaled, atomic), `internal/router/breaker_store.go` (atomic, but rename error dropped).

### 2.20 Cache and caching policies
`internal/cache`: exact (hash), semantic (embedding similarity + scoped keys), reasoning cache; tenant-scoped keys for Bearer callers (**empty tenant for vendor-header callers**, AUD-SEC-001), per-minute sync, Redis option.

### 2.21 Admin and management APIs
`internal/server`: `admin_keys_api.go`, `org_api.go`, `teams.go`, `keybudgets.go`, `keytraffic.go`, `dbexport.go`, `provider_login.go`, `oauth_api.go`, `oauth_conn.go`, `totp.go`, `dashauth.go`, `guardrails_api.go`, `catalog_audit.go`, `fallback_chains_api.go`, `files_api.go`, `batches_api.go` — every mutating route is TierAlwaysProtected or LocalOnly.

### 2.22 Observability and telemetry
`internal/observability` (Prometheus collector; otel build tag), `internal/metrics` (recorder), `internal/server/telemetry.go` (per-request meta + headers), `internal/routetrace` (decision trace), `internal/audit` (append-only log), `/dashboard/trace`.

### 2.23 Background jobs and schedulers
Eval scheduler, model refresh (`provider/refresh.go`), cache sync, breaker persistence, cooldown sweeper, health probes (`internal/health`), learned-signal persistence (`router/ratelimit.go`), quota-schedule evaluation.

### 2.24 Webhooks and external integrations
`internal/webhooks` (HMAC-SHA256, egress-guarded, 3 retries on 5xx, **429 terminal**, in-memory pending — AUD-REL-007), `internal/egress` (allowlist/SSRF policy, no direct tests), `internal/providerlogin` (OAuth out).

### 2.25 Desktop/client integrations
`internal/tray` (Windows/Linux supervisor helpers), `bin/` npm wrapper (download + checksum/cosign verification, JS tests not runnable without release artifacts), `scripts/smoke-client-*.py` (5 client protocol drivers, not in CI).

### 2.26 Configuration and deployment
`internal/config` (4,243 LOC; validation rejects non-loopback listen, default loopback, auth on by default), `Dockerfile`/`Dockerfile.release` (distroless, non-root, HEALTHCHECK, base images not digest-pinned), `docker-compose.yml`, `.github/workflows/*` (6 workflows).

### 2.27 Security controls
Guard tiers + scope + key policy, redacting logger, SSRF-guarded egress, HMAC webhooks, constant-time comparisons, IP control (RemoteAddr only, never `X-Forwarded-For`), header sanitisation, panic redaction, per-key reservations.

### 2.28 Testing infrastructure
668 test files, 4,804 `func Test`, 23 benchmarks, 2 fuzz targets; `internal/apitest` fixtures; 98 files using `httptest.NewServer`.

### 2.29 CI/CD and release pipeline
`ci.yml` (gofmt/build/vet/`go test -race`), `browser-smoke.yml` (playwright), `supply-chain.yml` (SBOM + `govulncheck || true`), `bench-infra-smoke.yml` (compile-only), `bench-run.yml` (dispatch-only), `release.yml` (GoReleaser on tags).

---

## 3. Request lifecycle (chat, traced)

```
client → mux (method+path)
  → tier wrapper: requireLocalOnly / requireAuth{Always,UnlessRelaxed}
      → requireScope (inside auth)
          → requireKeyPolicy (inside scope)
              → handler handleChatCompletions
                  [1] method check
                  [2] panic recover (noteServerPanic; panic ledger row)
                  [3] context.WithTimeout(r.Context(), timeout)
                  [4] reject X-Codvian-Routing header
                  [5] preBodyAdmission           (blocked/over-limit key → refuse before body)
                  [6] MaxBytesReader(10 MiB)
                  [7] read + decode (raw kept for idempotency hash)
                  [8] intent/capability pre-checks (local capability readiness → 503)
                  [9] guardrails (sync)          → may block
                 [10] compression
                 [11] internal budgets
                 [12] P1.6 team/org policy       (server.go:5328) → 403/400 org_budget
                 [13] ledger budget + key-budget reservation (defer settle)
                 [14] async guardrail start
                 [15] branch ──► handleChatStream (SSE)  |  chatWithMCP (JSON)
                                      │
                                      └─ engine: chainFor → applyContextFit →
                                         filterByRequiredCapabilities →
                                         [non-stream only: modelToolsCapable] →
                                         per-attempt budgetGate → dispatch
                                         (breaker/cooldown aware; ≤MaxAttempts,
                                          ≤3 providers; Retry-After honoured)
                                              │
                          ledger usage row + reservation settle + telemetry + trace
```

Every other dialect re-enters at its own handler and shares the same gate set (team policy verified in all 9 + batch/A2A). Divergences documented: tool-capability pre-check (non-stream only), response-guardrail blocking (non-stream only), relay ledger row (absent).

---

## 4. Data flow (money and quota)

```
request → estimate/price (PriceBook; unpriced → ok=false, never $0)
        → budget gates (global, key, internal, org, quota pools/schedules)
        → reservation (reserveMu; spend_reservations row)
        → dispatch → upstream usage (prompt/completion/cached/cache-write)
        → RecordUsage (async writer, batched ≤8192 rows / ≤50 ms)
        → settle reservation (always, context.Background(), 10-min TTL backstop)
        → memo invalidation (per-request memo; GetBudgetMemoHit 52 ns)
        → surfaces: /api/quota/intelligence, /api/cost/*, dashboard, MCP mirrors
```

---

## 5. Authentication / authorization flow

```
credential: Authorization: Bearer | X-Api-Key | X-Goog-Api-Key   (requestKey, server.go:2169)
  → authorized(): verifier.VerifyKey | oauth token               (server.go:2207)
  → requireScope(): key scopes, admin implies all                (server.go:2183)
  → requireKeyPolicy(): per-key IP + endpoint policy             (server.go:2078)
  → handler
identity for policy/limits/tenancy: callerKeyID  ← **Bearer only** (AUD-SEC-001)
```

---

## 6. Persistence boundaries

| Store | Engine | Durability | Notes |
| --- | --- | --- | --- |
| Ledger (usage/budgets/reservations) | SQLite WAL, `busy_timeout=5000`, `MaxOpenConns(1)` | durable, batched writes | corrupt file is fatal (AUD-REL-005) |
| caches (exact/semantic/reasoning) | SQLite or Redis | best-effort | SQLite lacks WAL/busy_timeout (AUD-REL-004) |
| files | FS temp+rename+fsync | durable | 512 MiB/file, no aggregate quota |
| health journal | FS atomic | durable | |
| breakers | FS atomic | durable | rename error swallowed (AUD-CODE-013) |
| eval schedules + runs | JSON | **non-atomic** (AUD-REL-003) | |
| batches index | JSON | atomic-ish | owner-visible semantics (AUD-SEC-001) |
| org/teams + audit | FS JSON | durable | |
| TOTP/OAuth state | FS JSON | durable | revocation error swallowed (AUD-CODE-012) |

---

## 7. Background jobs

| Job | Trigger | Recovery | Panic safety |
| --- | --- | --- | --- |
| Eval scheduler | interval per schedule | store-backed, duplicate-prevented | **no top-level recover** (AUD-REL-006) |
| Model refresh | 24 h | registry persist under lock (AUD-PERF-012) | n/a |
| Cache sync | per minute | best-effort | n/a |
| Breaker persist | state change | atomic rename | error swallowed |
| Webhook delivery | event | 3 retries on 5xx | no recover; 429 terminal |
| Health probes | interval | disk journal | n/a |
| Learned rate limits | per upstream response | fire-and-forget goroutine (AUD-PERF-006) | n/a |

---

## 8. External dependencies

Go modules: `pkoukk/tiktoken-go`, `redis/go-redis/v9`, `golang.org/x/crypto`, `golang.org/x/sys`, `modernc.org/sqlite`. Runtime/ops: Redis (optional cache), provider APIs (OpenAI/Anthropic/Gemini-style + 700+ catalog entries), models.dev (price sync), GitHub (release/npm download), cosign (release verification), Playwright (browser smoke), node (dashboard JS checks).

---

## 9. Cross-cutting notes observed during mapping

- One decision path per concern is the codebase's dominant pattern (one guard table, one team gate, one budget memo, one breaker table) — this is why the few "only on one path" defects (AUD-PROV-004, AUD-CODE-014, relay ledger gap) stand out as anomalies rather than the norm.
- The guard table is the strongest structural control found: an unclassified route **panics at startup**, which is why no route is unguarded.
- Route-artifact parity is exact (347/347/347/347/347), so documentation drift in this repo is about *content* (error envelopes, competitor naming), never about missing routes.
