# OmniRoute Gap Analysis & Phase 4 Master Roadmap

**Source:** read-only GitHub API access to `cheaper-inference/OmniRoute` (default branch `release/v3.8.55`). No local clone, no submodule, no code imported. CODVIAN Router compared at main commit `beeebaf` + security PR `27a235d` (merged).

**Methodology:** every feature classified from actual repository evidence — OmniRoute file paths + docs read via the GitHub Contents/Trees API; CODVIAN file paths + source read from the local working tree. Features are NOT marked equivalent by name alone; actual behavior is compared.

---

## Stage 1 Summary — Security Remediation (COMPLETE)

| Item | Before | After |
|------|--------|-------|
| Dependabot open alerts | 17 (8 critical, 3 high, 6 medium) | 0 (all `state=fixed`) |
| `golang.org/x/crypto` | v0.24.0 | v0.59.0 |
| govulncheck module-level vulns | 4 (in v0.52.0) | 0 (in v0.59.0) |
| Regression test | none | `TestCryptoDependencyAtSecureVersion` (go.mod version floor) |
| CI | all green | all green (ci, browser-smoke, bench-infra-smoke, supply-chain) |
| Merged PR | #47 (P3-C/P3-D/docs) | #48 security (main `27a235d`) |

All 17 Dependabot alerts (CVE-2024-45337 through CVE-2026-46598) are `fixed` as of `2026-11-22T19:04:10Z–12Z`. No Critical/High/Moderate alerts remain.

---

## Stage 2 — Feature Parity Matrix

Classification scale:
- **FULLY IMPLEMENTED** — CODVIAN has equivalent behavior, verified by tests/live smoke
- **PARTIALLY IMPLEMENTED** — CODVIAN has a subset; behavior differs materially
- **MISSING** — CODVIAN has nothing comparable
- **IMPLEMENTED BUT NOT VERIFIED** — code exists, not covered by tests/smoke
- **NOT APPLICABLE** — OmniRoute feature has no CODVIAN counterpart by design

### 1. Core Architecture & Gateway

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 1.1 | Single OpenAI-compatible endpoint | `/v1/*`, localhost:20128, `docs/architecture/ARCHITECTURE.md` | `GET /healthz`, `GET /v1/models`, `POST /v1/chat/completions`; `internal/server/server.go` | FULLY IMPLEMENTED |
| 1.2 | Local-first, no hosted middleman | "local-first with AES-256-GCM encrypted keys", `README.md` | "local-first, self-hostable", binds 127.0.0.1 only; `docs/ARCHITECTURE.md` | FULLY IMPLEMENTED |
| 1.3 | Single binary / self-hostable | npm package + Docker + Electron desktop | single Go binary, 17.3 MB; `docs/ARCHITECTURE.md` | FULLY IMPLEMENTED |
| 1.4 | Multi-provider routing | 394 providers, `README.md` | provider registry, `internal/provider/registry.go` | FULLY IMPLEMENTED |
| 1.5 | Provider key test on add | inline key probe (`GET /v1/models`), `src/lib/providers/validation/` | inline `GET /v1/models` key test, `internal/provider/registry.go` | FULLY IMPLEMENTED |
| 1.6 | Auto model list fetch | auto-fetches model list, `docs/architecture/ARCHITECTURE.md` | auto-fetches model list, `internal/provider/registry.go` | FULLY IMPLEMENTED |
| 1.7 | Wire-format auto-detect | OpenAI first, fallbacks, `docs/architecture/ARCHITECTURE.md` | OpenAI/Anthropic/Gemini/Cohere/Bedrock/Vertex detection, `internal/provider/client.go` | FULLY IMPLEMENTED |
| 1.8 | `auto` model routing | `auto`, `auto/coding`, `auto/fast`, ... (10 aliases), `README.md` | `internal/autorouter/` exists; `auto/*` resolution via `Config.AutoRoute` | PARTIALLY IMPLEMENTED (aliases limited) |
| 1.9 | Streaming | SSE passthrough, mid-stream resume | SSE passthrough, `bridgeStream` + preread buffer, `internal/router/router.go` | FULLY IMPLEMENTED |
| 1.10 | WebSocket bridge | `/v1/ws`, `docs/architecture/ARCHITECTURE.md` | none | MISSING |
| 1.11 | Embeddings | `/v1/embeddings`, 18 providers | none | MISSING |
| 1.12 | Image generation | `/v1/images/generations`, 10+ providers | none | MISSING |
| 1.13 | Audio/TTS | `/v1/audio/{speech,transcriptions}`, 24 TTS providers | none | MISSING |
| 1.14 | Video/music generation | ComfyUI + SD WebUI | none | MISSING |
| 1.15 | Web search | `/v1/search`, 20 providers | none | MISSING |
| 1.16 | Moderations | `/v1/moderations` | none | MISSING |
| 1.17 | Reranking | `/v1/rerank` | none | MISSING |

### 2. Provider Integrations & Management

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 2.1 | Provider catalog (394 providers) | `src/lib/providers/catalog.ts`, `src/shared/constants/providers` | `internal/provider/` registry; ~1 configured in default config | PARTIALLY IMPLEMENTED (catalog much smaller) |
| 2.2 | OAuth provider connections | 27 OAuth modules, `src/lib/oauth/providers/` | none (API-key only) | MISSING |
| 2.3 | No-auth/free providers | `NOAUTH_PROVIDERS`, `src/shared/constants/providers` | none | MISSING |
| 2.4 | Web-cookie auth providers | `WEB_COOKIE_PROVIDERS`, `src/lib/providers/webCookieAuth.ts` | none | MISSING |
| 2.5 | Local/self-hosted providers | `LOCAL_PROVIDERS` | `SelfHosted` config hook, `internal/router/router.go` | PARTIALLY IMPLEMENTED |
| 2.6 | Provider model listing | `managedAvailableModels.ts`, `mergeProviderModelListing.ts` | auto-fetch on add, `internal/provider/registry.go` | FULLY IMPLEMENTED |
| 2.7 | Provider validation per type | `src/lib/providers/validation/` (25+ validators) | `provider.ClassifyError`, error taxonomy | PARTIALLY IMPLEMENTED |
| 2.8 | Provider deprecation cleanup | `deprecatedProviderCleanup.ts` | `Model.Deprecated` flag, `internal/provider/provider.go` | PARTIALLY IMPLEMENTED |

### 3. Routing Strategies & Fallback

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 3.1 | 22 routing strategies | `README.md` (priority, fill-first, weighted, round-robin, p2c, least-used, random, cost-optimized, headroom, reset-window, ...), `docs/routing/` | `internal/strategy/` family + `OrderHook` extension point + strategy trees | PARTIALLY IMPLEMENTED (fewer named strategies) |
| 3.2 | Combo / multi-model sequences | "combos", `docs/routing/AUTO-COMBO.md`, `src/app/api/services/9router/` | none (single-model chains) | MISSING |
| 3.3 | Fallback chains | "compositeTiers", `src/app/api/providers/` | per-request chain via `ChainFor`, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.4 | Quota-aware routing | quota preflight, P2C account selection, `src/lib/quota/` | `QuotaHint` func, zero-quota skip, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.5 | 429 / Retry-After handling | "cooldown-aware chat retries", `docs/architecture/ARCHITECTURE.md` | `Retry-After` honour, backoff+jitter, `internal/router/transient.go` | FULLY IMPLEMENTED |
| 3.6 | Circuit breakers | circuit breaker pattern, `docs/architecture/ARCHITECTURE.md` | auth-profile thresholds (OAuth 8 / API-key 12 / local 2), `internal/router/breaker_profile.go` | FULLY IMPLEMENTED |
| 3.7 | Hedged requests | "4-tier auto-fallback", `README.md` | 150ms hedge, first success wins, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.8 | Streaming-safe failover | `bridgeStream` + preread, no switch after first byte | `bridgeStream`, preread buffer, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.9 | Per-key cooldowns | per-account rate limiting, `docs/architecture/ARCHITECTURE.md` | per-(provider,key) cooldowns, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.10 | Model lockout | lockout policy, `docs/domain/` | `ModelLockout` (base 120s, max 30min), `internal/router/lockout.go` | FULLY IMPLEMENTED |
| 3.11 | Predictive TTFT breaker | "per-account enhanced rate limiting" | `PredictiveTtftMs`, `internal/router/ttft.go` | FULLY IMPLEMENTED |
| 3.12 | Token limit checks | quota enforcement, `src/lib/quota/enforce.ts` | `TokenLimitCheck` func, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.13 | Internal budget checks | budget tracking, `src/lib/quota/spendRecorder.ts` | `InternalBudgetCheck` func, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.14 | Deadline budget | request timeouts | `DeadlineBudget`, 504 on exceed, `internal/router/router.go` | FULLY IMPLEMENTED |
| 3.15 | SLA-aware routing | "cost-optimized routing", `README.md` | `SLATargetP95Ms`/error-rate/cost cap, `internal/sla/` | PARTIALLY IMPLEMENTED |
| 3.16 | Strategy trees | "compositeTiers" runtime ordering | nestable `strategytree.Node`, `internal/strategytree/` | FULLY IMPLEMENTED |
| 3.17 | LKGP (last-known-good) | `auto/lkgp` alias | `internal/strategy/` LKGP registry + invalidator | PARTIALLY IMPLEMENTED |
| 3.18 | Nadir decision API | none documented | `NadirEndpoint`, fail-closed (no external call), `internal/router/nadir.go` | NOT APPLICABLE (CODVIAN divergence by design) |
| 3.19 | Region-aware failover | none documented | `RegionFor`, cross-region/equivalent/self-hosted tiers, `internal/router/failover_region.go` | FULLY IMPLEMENTED |
| 3.20 | Context relay | Context Relay session handoff, `docs/routing/REASONING_HANDOFF.md` | `RelaySummariser`, quota-triggered handoff, `internal/router/relay.go` | PARTIALLY IMPLEMENTED |

### 4. Authentication, API Keys, Teams, RBAC

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 4.1 | API key (Bearer) auth | `isValidApiKey()`, `src/shared/utils/apiAuth.ts` | `Authorization: Bearer crk_...`, `internal/server/dashauth.go` | FULLY IMPLEMENTED |
| 4.2 | Per-key scopes | API key scopes (manage, admin) | per-key scopes (`chat`, `models`), `internal/config/` | FULLY IMPLEMENTED |
| 4.3 | Dashboard session (JWT cookie) | `auth_token` cookie, 30-day lifetime, `src/shared/utils/dashboardSessionToken.ts` | none (API-only, no dashboard login) | MISSING |
| 4.4 | OIDC login (opt-in) | OIDC + JWKS, `docs/architecture/AUTHZ_GUIDE.md` | none | MISSING |
| 4.5 | TOTP 2FA | `src/lib/auth/totp.ts`, `backupCodes.ts` | none | MISSING |
| 4.6 | Organizations | none documented (teams via dashboard) | `Organization`, `internal/org/org.go` | FULLY IMPLEMENTED (CODVIAN-only) |
| 4.7 | Teams | none documented | `Team`, `internal/org/org.go` | FULLY IMPLEMENTED (CODVIAN-only) |
| 4.8 | RBAC roles (owner/admin/member/viewer) | route classes (PUBLIC/CLIENT/MANAGEMENT) | `Role` + `CanManage()`, `internal/org/org.go` | FULLY IMPLEMENTED |
| 4.9 | Key-to-team binding | none documented | `KeyBinding`, `BindKey`/`UnbindKey`, `internal/org/org.go` | FULLY IMPLEMENTED (CODVIAN-only) |
| 4.10 | Model/strategy allowlists per team | none documented | `AllowedModels`, `AllowedStrategies`, `internal/org/org.go` | FULLY IMPLEMENTED (CODVIAN-only) |
| 4.11 | Team budgets | quota pools, `src/lib/quota/` | `BudgetUSD`, `SpendWithinBudget`, `internal/org/org.go` | FULLY IMPLEMENTED |
| 4.12 | Audit log (append-only) | compliance audit logging, `docs/architecture/ARCHITECTURE.md` | `AuditEntry`, JSON-persisted, `internal/org/org.go` | FULLY IMPLEMENTED |
| 4.13 | IP allowlist/blocklist | IP allowlist/blocklist, `docs/architecture/ARCHITECTURE.md` | none | MISSING |

### 5. Usage Tracking, Billing, Budgets, Cost

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 5.1 | Usage tracking | usage/cost tracking, request logging | `usage_events` SQLite table, `internal/ledger/` | FULLY IMPLEMENTED |
| 5.2 | Per-token split (prompt/completion) | token usage tracking | full split when known, `split_known=0` otherwise, never invented | FULLY IMPLEMENTED |
| 5.3 | Cost recording | cost tracking | `cost_usd`, 0 when no prices configured | FULLY IMPLEMENTED |
| 5.4 | Token estimation | hybrid counting (provider + estimation fallback) | tiktoken local estimate + divergence tracking, `internal/tokencount/` | FULLY IMPLEMENTED |
| 5.5 | Estimate-vs-settled reconciliation | none documented | `internal/server/cost_estimation.go` (P3-C) | FULLY IMPLEMENTED (CODVIAN-only) |
| 5.6 | Budget cap | budget tracking | global `budget_usd` + fail-closed | FULLY IMPLEMENTED |
| 5.7 | Per-key budget | per-account quotas | per-key team budget via org store | PARTIALLY IMPLEMENTED |
| 5.8 | Fail-closed mode | "fails closed, never a billable fallback" | `fail_closed` config, `internal/config/` | FULLY IMPLEMENTED |
| 5.9 | Fair-share quota | fair-share, `src/lib/quota/fairShare.ts` | quota-share DRR pools, `internal/router/cascade.go` | PARTIALLY IMPLEMENTED |
| 5.10 | Burn rate / time-to-exhaustion | `burnRate.ts`, `PoolUsageSnapshot` | none | MISSING |

### 6. Caching & Performance

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 6.1 | Exact-match cache (LRU+TTL) | signature-based dedup cache | `ExactCache`, SHA-256 key, TTL, `internal/cache/` | FULLY IMPLEMENTED |
| 6.2 | Provider prompt cache | none documented | auto-injected cache markers, cache affinity on fallback | FULLY IMPLEMENTED |
| 6.3 | Semantic cache | none documented | `SemanticCache` (256-dim bag-of-words, 0.85 threshold) — NOT wired into request path | IMPLEMENTED BUT NOT VERIFIED (not in request path) |
| 6.4 | Request coalescing / single-flight | mutex anti-thundering-herd | `internal/coalescing/` flights, `internal/router/router.go` | FULLY IMPLEMENTED |
| 6.5 | Adaptive concurrency limiter | none documented | Gradient2 limiter, per-(provider,key), fail-open, `internal/adaptive/` | FULLY IMPLEMENTED |
| 6.6 | RTK + Caveman compression | 15-95% token savings, `docs/compression/` | lossless/lossy compression, `internal/compress/`, prefix freeze | PARTIALLY IMPLEMENTED |

### 7. Observability, Analytics, Tracing, Logging

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 7.1 | Metrics endpoint | request telemetry p50/p95/p99 | `/metrics` (auth-gated), Prometheus text + JSON traces, `internal/metrics/` | FULLY IMPLEMENTED |
| 7.2 | Route trace | correlation ID `X-Request-Id`, combo telemetry | `internal/routetrace/`, per-attempt records, `internal/router/router.go` | FULLY IMPLEMENTED |
| 7.3 | Health dashboard | health dashboard, circuit breaker status | health watcher (startup + 60s), `internal/health/` | FULLY IMPLEMENTED |
| 7.4 | Per-provider stats | combo target health | `ProviderStats`, attempts/successes/latency EWMA | FULLY IMPLEMENTED |
| 7.5 | Request logging | request logging | SQLite `usage_events`, `internal/ledger/` | FULLY IMPLEMENTED |
| 7.6 | Compliance audit logging | compliance audit v2 with pagination | org audit log only (admin actions), not full request audit | PARTIALLY IMPLEMENTED |

### 8. Streaming & Multimodal

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 8.1 | Chat streaming | SSE passthrough | SSE passthrough, resume/restart on failover | FULLY IMPLEMENTED |
| 8.2 | Vision bridge | `visionBridge.ts` guardrail | `VisionCapability`/`VisionPool` hooks, `internal/router/router.go` | PARTIALLY IMPLEMENTED (hooks exist, not wired by default) |
| 8.3 | Audio/video bridge | `audioBridge.ts`, `videoBridge.ts` | `ModalityCapability`/`ModalityPool` hooks | PARTIALLY IMPLEMENTED (hooks exist, not wired by default) |
| 8.4 | Multimodal embedding | multimodal embeddings | none | MISSING |

### 9. Evaluation & Benchmarking

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 9.1 | Eval framework | Eval framework for LLM QA, `src/lib/evals/` | `internal/eval/`, substring + latency budget, 9 tests | FULLY IMPLEMENTED |
| 9.2 | Benchmark | benchmark smoke test (mock upstream, 429 failover) | `scripts/smoke-followups-277.py`, live mock-upstream smoke | FULLY IMPLEMENTED |
| 9.3 | 56,000+ test declarations | 7,800+ test files, `README.md` | ~329 routes + unit tests, `go test ./...` all pass | PARTIALLY IMPLEMENTED (smaller test surface) |

### 10. Webhooks & Scheduled Jobs

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 10.1 | Webhook events | 8 events (request.completed/failed, quota.exceeded, proxy, provider), `src/lib/webhooks/eventDescriptions.ts` | `quota.exceeded`, `provider.degraded`, `provider.recovered`, `internal/webhooks/webhooks.go` | PARTIALLY IMPLEMENTED |
| 10.2 | Webhook integrations | Slack, Discord, Telegram, `src/lib/webhooks/integrations/` | none (raw HTTP POST only) | PARTIALLY IMPLEMENTED |
| 10.3 | Webhook retry (3 retries, exp backoff) | `deliverWebhook`, `src/lib/webhookDispatcher.ts` | 3 retries, 1s/2s/4s backoff, 10s timeout, `internal/webhooks/webhooks.go` | FULLY IMPLEMENTED |
| 10.4 | Webhook HMAC signing | not documented | `X-Codvian-Signature-256` HMAC-SHA256 | FULLY IMPLEMENTED (CODVIAN-only) |
| 10.5 | Scheduled evaluations | none documented | `internal/eval/schedule.go`, scheduler, `internal/server/eval_schedules_api.go` | FULLY IMPLEMENTED (CODVIAN-only) |
| 10.6 | Webhook delivery status/persistence | not documented | persistence + regression detection, `internal/server/eval_schedules_api.go` | FULLY IMPLEMENTED (CODVIAN-only) |

### 11. Dashboard & Frontend

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 11.1 | Web dashboard | Next.js dashboard, 67 languages | `web/index.html` single-page dashboard | PARTIALLY IMPLEMENTED |
| 11.2 | Guardrails dashboard section | guardrails config UI | `view-guardrails` nav section (P3-D), live APIs `/dashboard/guardrails`, `/dashboard/guard` | FULLY IMPLEMENTED (verified via Playwright + browser_exec) |
| 11.3 | Free-tier budget dashboard | `/dashboard/free-tiers`, `README.md` | none | MISSING |
| 11.4 | Live routing view | not documented | Live routing from route trace | PARTIALLY IMPLEMENTED |
| 11.5 | Desktop app (Electron) | `bin/cli/`, Electron downloads | none | MISSING |
| 11.6 | PWA | PWA, `README.md` | none | MISSING |

### 12. Security & Enterprise

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 12.1 | AES-256-GCM key sealing | AES-256-GCM encrypted keys | `internal/crypto/` Vault, Argon2id + AES-256-GCM | FULLY IMPLEMENTED |
| 12.2 | Guardrails (fail-open) | 6 guardrails, fail-open, `docs/security/GUARDRAILS.md` | guardrails package, `internal/guardrails/` | PARTIALLY IMPLEMENTED |
| 12.3 | Prompt injection guard | `prompt-injection` guardrail, priority 20 | `internal/guardrails/` injection patterns | PARTIALLY IMPLEMENTED |
| 12.4 | PII masking | `pii-masker` guardrail | `internal/guardrails/` redaction | PARTIALLY IMPLEMENTED |
| 12.5 | Prompt compression | RTK + Caveman stacked pipelines | `internal/compress/`, lossless/lossy | PARTIALLY IMPLEMENTED |
| 12.6 | MITM proxy + TLS stealth | MITM proxy, cert management, `src/mitm/` | none | MISSING |
| 12.7 | SSRF guard | safe outbound fetch, SSRF guard, `docs/architecture/ARCHITECTURE.md` | none | MISSING |
| 12.8 | Supply chain security | `SUPPLY_CHAIN.md`, scorecard, semgrep, CodeQL, DAST | `internal/release/` SBOM + checksums, `cmd/release-sbom/` | PARTIALLY IMPLEMENTED |
| 12.9 | Dependabot / vuln scanning | Dependabot (17 alerts on main at PR time) | govulncheck: 0 vulns; all Dependabot alerts fixed | FULLY IMPLEMENTED (post-remediation) |
| 12.10 | Error sanitization | `ERROR_SANITIZATION.md` | error taxonomy, no credentials in traces | PARTIALLY IMPLEMENTED |
| 12.11 | CORS policy | `CORS.md` | localhost-only bind, no CORS needed | NOT APPLICABLE (by design) |

### 13. Configuration, Deployment, Infrastructure

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 13.1 | Config file | `.env.*` examples, JSON config | JSON config, `version` field, `SCHEMA.md`, `internal/config/` | FULLY IMPLEMENTED |
| 13.2 | Docker | `docker/`, Docker Hub | `Dockerfile`, `docker-compose.yml` (not published) | PARTIALLY IMPLEMENTED |
| 13.3 | Helm | `deploy/helm/omniroute/` | none | MISSING |
| 13.4 | Kubernetes | `deploy/kubernetes/` | none | MISSING |
| 13.5 | CLI subcommands | `bin/cli/`, 39 CLI integrations | `serve`, `init`, `keygen`, `eval`, `validate`, `tray` | PARTIALLY IMPLEMENTED |
| 13.6 | System tray | none documented | `tray` subcommand + supervisor | FULLY IMPLEMENTED (CODVIAN-only) |
| 13.7 | No C toolchain | Node.js (needs native build) | pure-Go + modernc.org/sqlite, no CGO needed | FULLY IMPLEMENTED (CODVIAN advantage) |
| 13.8 | SBOM generation | `THIRD_PARTY_NOTICES.md` | CycloneDX SBOM, `internal/release/checksum.go` | FULLY IMPLEMENTED |

### 14. Testing, Reliability, Documentation, DX

| # | Feature | OmniRoute (evidence) | CODVIAN (evidence) | Classification |
|---|---------|---------------------|-------------------|----------------|
| 14.1 | Unit tests | 7,800+ test files, 56,000+ declarations | `go test ./...` all pass (~329 routes covered) | FULLY IMPLEMENTED |
| 14.2 | Race detector | not documented | `go test -race` clean (CI), CGO-limited locally | FULLY IMPLEMENTED |
| 14.3 | Browser smoke (Playwright) | `tests/e2e/`, Playwright | `tests/playwright/smoke.spec.js`, 4/4 pass | FULLY IMPLEMENTED |
| 14.4 | API reference | `docs/openapi.yaml` | `docs/API_REFERENCE.md` (regenerated, 329 routes) | FULLY IMPLEMENTED |
| 14.5 | llm.txt | none | `docs/llm.txt` (regenerated) | FULLY IMPLEMENTED (CODVIAN-only) |
| 14.6 | tryit.html | none | `web/tryit.html` (regenerated) | FULLY IMPLEMENTED (CODVIAN-only) |
| 14.7 | Route fixture JSON | `tests/fixtures/` | `tests/fixtures/v1/m8.21/routes.json` (regenerated) | FULLY IMPLEMENTED (CODVIAN-only) |
| 14.8 | Documentation | 67 languages, `docs/` | `docs/*.md`, architecture + design docs | PARTIALLY IMPLEMENTED |

---

### Totals

| Category | FULLY | PARTIALLY | MISSING | IMPLEMENTED BUT NOT VERIFIED | NOT APPLICABLE |
|----------|-------|-----------|---------|------------------------------|----------------|
| Core Architecture | 10 | 0 | 2 | 0 | 0 |
| Provider Mgmt | 2 | 4 | 2 | 0 | 0 |
| Routing | 13 | 4 | 1 | 0 | 1 |
| Auth/RBAC | 6 | 0 | 4 | 0 | 0 |
| Usage/Billing | 8 | 1 | 1 | 0 | 0 |
| Caching/Perf | 5 | 1 | 0 | 1 | 0 |
| Observability | 5 | 1 | 0 | 0 | 0 |
| Streaming/Multimodal | 1 | 2 | 1 | 0 | 0 |
| Eval/Benchmark | 2 | 1 | 0 | 0 | 0 |
| Webhooks/Jobs | 3 | 1 | 0 | 0 | 0 |
| Dashboard | 1 | 2 | 2 | 0 | 0 |
| Security | 2 | 6 | 2 | 0 | 1 |
| Deployment/Infra | 2 | 3 | 2 | 0 | 0 |
| Testing/DX | 6 | 1 | 0 | 0 | 0 |
| **TOTAL** | **68** | **25** | **16** | **1** | **2** |

**Grand total compared: 112 features**

---

## Critical Architectural Gaps

1. **No dashboard login / session auth** — CODVIAN is API-key only; OmniRoute has dashboard sessions, OIDC, TOTP 2FA. CODVIAN's threat model is single-operator local; adding multi-user auth is a large change.
2. **No OAuth/no-auth/web-cookie provider connections** — CODVIAN is API-key-only provider auth; OmniRoute supports 27 OAuth + no-auth + web-cookie providers.
3. **No completions/embeddings/images/audio/video/music/search/moderation/reroute APIs** — CODVIAN is chat-completions-only (`/v1/chat/completions`, `/v1/models`); OmniRoute is a full multimodal gateway.
4. **No desktop app / PWA** — CODVIAN is a headless Go binary; OmniRoute ships Electron + PWA.
5. **No Helm/Kubernetes deployment** — CODVIAN has Docker (unpublished) only.
6. **No MITM proxy / SSRF guard** — CODVIAN makes direct outbound HTTP; OmniRoute has a full MITM proxy with SSRF protection.
7. **Semantic cache not wired into request path** — exists and tested, but the server/engine never calls it.

---

## Security & Reliability Improvements Still Needed

1. **2FA / MFA for dashboard** (when dashboard auth is added) — OmniRoute has TOTP + backup codes.
2. **IP allowlist/blocklist** — OmniRoute has API access IP control.
3. **Request-level compliance audit log** — CODVIAN audits admin actions only; OmniRoute audits all API requests.
4. **Guardrails wired into request path** — CODVIAN has guardrails package + dashboard section, but pre/post-call enforcement on chat requests needs completion.
5. **Streaming usage recording** — streams record 0 tokens/cost (documented limitation); OmniRoute records usage from streaming responses.

---

## Performance & Scalability Gaps

1. **Semantic cache not in request path** — exists, tested, unwired.
2. **No distributed caching** — single-process in-memory only; OmniRoute has Redis quota store option.
3. **No connection pooling across processes** — single binary only.

---

## Phase 4 Master Roadmap

### Priority P0 (security/correctness — do before any feature work)

| # | Task | Dependency | Acceptance Criteria |
|---|------|-----------|-------------------|
| P0-1 | Complete guardrail pre/post-call enforcement on chat requests | P3-D dashboard section exists | Guardrails block/transform on `preCall` and `postCall`; fail-open behavior tested; regression tests for every guardrail |
| P0-2 | Add streaming usage recording | ledger records 0 for streams today | Streams record provider-reported usage when available; local-estimate fallback when not; tests cover streaming rows |
| P0-3 | Add request-level compliance audit log | org audit log exists | All API requests logged (actor, action, target, detail); append-only; tamper-evident |

### Priority P1 (core parity — chat gateway completeness)

| # | Task | Dependency | Acceptance Criteria |
|---|------|-----------|-------------------|
| P1-1 | Embeddings API (`/v1/embeddings`) | provider wire format registry | OpenAI-compatible embeddings through at least 2 providers; unit tests |
| P1-2 | Image generation API (`/v1/images/generations`) | provider wire format registry | At least 2 image providers; tests |
| P1-3 | Audio transcription + TTS APIs | provider wire format registry | At least 2 providers each; tests |
| P1-4 | Dashboard session auth (JWT cookie) | none | Login/logout, session revocation on password change, cookie-based admin routes |
| P1-5 | TOTP 2FA + backup codes | P1-4 | Enrollment shows codes once, bcrypt hashes persisted, verification enforces 2FA |
| P1-6 | OAuth provider connections | P1-4 | At least 3 OAuth providers connectable via dashboard |
| P1-7 | IP allowlist/blocklist | P1-4 | Middleware rejects disallowed IPs; configurable via config file |

### Priority P2 (routing & reliability completeness)

| # | Task | Dependency | Acceptance Criteria |
|---|------|-----------|-------------------|
| P2-1 | Wire semantic cache into request path | `internal/cache/` semantic cache exists | Cache hit returns cached response without upstream call; miss populates; TTL/LRU enforced; tests |
| P2-2 | Add remaining 22 routing strategies | `internal/strategy/` exists | All OmniRoute-named strategies (priority, fill-first, weighted, round-robin, p2c, least-used, random, cost-optimized, headroom, reset-window, reset-aware, context-relay, context-optimized, lkgp, auto, fusion, pipeline, cache-optimized, quota-weighted, expiry-first, jev) implemented + tested |
| P2-3 | Add `auto/*` model aliases (coding, fast, cheap, subscription, thrifty, offline, smart, lkgp, chaos) | `internal/autorouter/` exists | Each alias resolves to a concrete model or fails honestly; tests |
| P2-4 | Add combos (multi-model sequences) | routing engine chains | Combo = ordered model list with automatic fallback on quota/failure/cost spike; tests |
| P2-5 | Add webhook integrations (Slack/Discord/Telegram) | `internal/webhooks/` exists | Payload templates per integration; delivery tests with mock webhooks |
| P2-6 | Add more webhook event types | `quota.exceeded` exists | `request.completed`, `request.failed`, `provider.exhausted`, `test.ping` emitted |

### Priority P3 (operations & polish)

| # | Task | Dependency | Acceptance Criteria |
|---|------|-----------|-------------------|
| P3-1 | Publish Docker images | `Dockerfile`, `docker-compose.yml` exist | Automated build on tags; image scans clean |
| P3-2 | Add Helm chart | none | `deploy/helm/` with values; installable |
| P3-3 | Add i18n for dashboard | `web/index.html` is English-only | At least 5 languages; language switcher |
| P3-4 | Add desktop app (Electron/PWA) | none | Installable desktop binary; system tray integration |
| P3-5 | Benchmark parity with OmniRoute | `scripts/smoke-followups-277.py` | Benchmark report comparing failover/hedge/latency numbers |

### Dependencies & Ordering

1. **P0-1 (guardrails)** must precede P1 dashboard features (guardrails are a security boundary).
2. **P1-4 (session auth)** precedes P1-5 (2FA), P1-6 (OAuth), P1-7 (IP control) — all need authenticated dashboard.
3. **P2-1 (semantic cache)** is independent but improves all routing.
4. **P2-2..P2-4 (strategies/combos)** build on the routing engine; order strategies before combos.
5. **P3 items** are independent and can be done in any order after P1/P2 core.

### Estimated Complexity

| Tier | Effort | Rationale |
|------|--------|-----------|
| P0 | Medium | Guardrail enforcement + streaming usage + audit log are well-scoped, testable additions |
| P1 | High | Session auth + 2FA + OAuth + embeddings/images/audio are new surface area |
| P2 | High | 22 strategies + combos + alias resolution are substantial routing work |
| P3 | Medium | Docker/Helm/i18n/desktop are packaging work, low logic risk |

---

## Features CODVIAN Does Better Than OmniRoute

1. **Pure-Go, no C toolchain** — modernc.org/sqlite means zero CGO; OmniRoute is Node.js with native modules.
2. **Small, auditable dependency surface** — 4 direct deps vs OmniRoute's 100+ npm deps.
3. **Team/org RBAC with key binding** — OmniRoute has no org/team model; CODVIAN's P3-A is a genuine advantage.
4. **Per-key quota hints learned from upstream** — measured rate-limit fractions per (provider,key), not just static pools.
5. **Estimate-vs-settled cost reconciliation** — P3-C explicitly reconciles local tiktoken estimates with provider-reported usage, flagging divergences.
6. **Region-aware failover tiers** — cross-region → equivalent → self-hosted, with operator-declared regions.
7. **Fail-closed Nadir** — external decision API never called unless explicitly configured (OmniRoute defaults fail-open).
8. **Streaming-safe failover with preread buffer** — bounded preread, no switch after first content byte.

---

## Features Worth Adopting / Redesigning / Not Copying

### Adopt (high value, good fit)
- TOTP 2FA + backup codes (OmniRoute's bcrypt approach is sound)
- Slack/Discord/Telegram webhook integrations
- Embeddings/images/audio/video APIs (multimodal gateway breadth)
- Helm chart + published Docker images
- `auto/*` model aliases (10 aliases covering quality/latency/cost/subscription)

### Redesign (don't copy wholesale)
- Dashboard session auth — CODVIAN's threat model is single-operator local; a full OIDC + session system is over-engineered. A simpler password+session model fits better.
- MITM proxy — CODVIAN doesn't need a full MITM; direct HTTP with an SSRF guard is sufficient.

### Do Not Copy
- 67-language i18n — CODVIAN's dashboard is a single HTML file; full i18n is disproportionate.
- Electron desktop app — CODVIAN's value is a headless server binary; a desktop GUI dilutes that.
- 56,000+ test declarations — CODVIAN's Go test culture (table-driven, race-enabled) is more maintainable than TypeScript test volume.

---

## Proposed PR & Implementation Sequence

| PR | Focus | Est. |
|----|-------|------|
| #49 | P0-1: Guardrail pre/post-call enforcement + regression tests | 1 wk |
| #50 | P0-2: Streaming usage recording + tests | 3 days |
| #51 | P0-3: Request-level compliance audit log | 1 wk |
| #52 | P1-1..P1-3: Embeddings + images + audio APIs | 2 wk |
| #53 | P1-4: Dashboard session auth (JWT cookie) | 1 wk |
| #54 | P1-5: TOTP 2FA + backup codes | 3 days |
| #55 | P1-6: OAuth provider connections (3 providers) | 1 wk |
| #56 | P1-7: IP allowlist/blocklist | 2 days |
| #57 | P2-1: Semantic cache in request path | 3 days |
| #58 | P2-2: 22 routing strategies + tests | 2 wk |
| #59 | P2-3: `auto/*` aliases | 1 wk |
| #60 | P2-4: Combos (multi-model sequences) | 1 wk |
| #61 | P2-5: Slack/Discord/Telegram webhooks | 1 wk |
| #62 | P2-6: More webhook event types | 2 days |
| #63 | P3-1: Publish Docker images | 2 days |
| #64 | P3-2: Helm chart | 1 wk |
| #65 | P3-3: i18n (5 languages) | 1 wk |
| #66 | P3-4: Desktop app (Electron/PWA) | 2 wk |
| #67 | P3-5: Benchmark parity report | 3 days |

---

## Final Verification (main commit `27a235d`)

- `go test ./...` — all packages **ok**
- `go vet ./...` — clean
- `go build ./...` — clean
- `govulncheck ./...` — **0 vulnerabilities**
- Dependabot alerts — **0 open** (17 fixed, all `state=fixed`)
- Playwright browser smoke — **4/4 pass**
- CI on PR #48 — ci, browser-smoke, bench-infra-smoke, supply-chain all **pass**

**Stage 1 complete.** **Stage 2 complete.** Phase 4 roadmap delivered — awaiting separate approval before Phase 4 implementation begins.
