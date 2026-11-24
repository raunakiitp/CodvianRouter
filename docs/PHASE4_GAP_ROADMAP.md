# Phase 4 Gap Analysis & Master Roadmap

**Source:** CODVIAN Router's own feature inventory and gap analysis, taken at main commit `632f574`. Every CODVIAN-side row is evidenced by a file path in this repository; no external material is required to read or reproduce it.

**Methodology:** every feature classified from actual repository evidence — CODVIAN file paths + source read from the local working tree. A feature is only called implemented when the wiring exists and the behaviour was checked, never by name alone.

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

---

## Critical Architectural Gaps

1. **No dashboard login / session auth** — CODVIAN is API-key only; dashboard sessions, OIDC and TOTP 2FA are not implemented. CODVIAN's threat model is single-operator local; adding multi-user auth is a large change.
2. **No OAuth/no-auth/web-cookie provider connections** — CODVIAN is API-key-only provider auth; OAuth, no-auth and web-cookie provider connections are not implemented.
3. **No WebSocket bridge, video/music generation, web search, or moderations API** — CODVIAN has chat-completions plus embeddings/images/audio/reranking; those additional multimodal endpoints are not implemented.
4. **No desktop app / PWA** — CODVIAN is a headless Go binary; an Electron app and a PWA are out of scope.
5. **No Helm/Kubernetes deployment** — CODVIAN has Docker (unpublished) only.
6. **No MITM proxy / SSRF guard** — CODVIAN makes direct outbound HTTP; a MITM proxy and an SSRF guard are not implemented.
7. **Semantic cache not wired into request path** — exists and tested, but the server/engine never calls it.

---

## Security & Reliability Improvements Still Needed

1. **2FA / MFA for dashboard** (when dashboard auth is added) — TOTP + backup codes are not implemented yet.
2. **IP allowlist/blocklist** — API access IP control is not implemented.
3. **Request-level compliance audit log** — CODVIAN audits admin actions only; auditing every API request is not implemented.

---

## Performance & Scalability Gaps

1. **Semantic cache not in request path** — exists, tested, unwired.
2. **No distributed caching** — single-process in-memory only; a Redis-backed quota store option is not implemented.
3. **No connection pooling across processes** — single binary only.

---

## Phase 4 Master Roadmap

### Priority P0 — Security/correctness gaps (do before any feature work)

| ID | Task | Dependency | Acceptance Criteria |
|----|------|-----------|-------------------|
| P0-1 | Request-level compliance audit log | `audit.go` exists (guard denials/MCP only) | Every API request logged (actor, action, target, detail, redacted); **never stores API keys, authorization headers, passwords, or sensitive request/response bodies in plaintext**; append-only; tamper-evident (hash-chained or signed entries); covers `/v1/*` chat/embeddings/images/audio/responses |
| P0-2 | Semantic cache wired into request path | `internal/cache/cache.go` exists (`LocalSemanticCache`, `NeuralEmbedder`) | Cache hit returns cached response without upstream call; miss populates; TTL/LRU enforced; neural embedder opt-in; **tenant/team/key isolation verified — a cache hit must never leak another user's response**; tests prove end-to-end hit/miss through `/v1/chat/completions` |

### Priority P1 — Core chat-gateway capabilities

| ID | Task | Dependency | Acceptance Criteria |
|----|------|-----------|-------------------|
| P1-1 | Dashboard session auth (JWT cookie) | none | Simple password + server-side session; login/logout endpoints; session revocation on password change; cookie-based admin routes; no OIDC |
| P1-2 | TOTP 2FA + backup codes | P1-1 | Enrollment displays codes once (never stored plaintext); bcrypt hashes persisted; verification enforces 2FA; backup codes single-use |
| P1-3 | OAuth provider connections | P1-1 | ≥3 OAuth providers connectable via dashboard; token refresh; key stored encrypted at rest |
| P1-4 | IP allowlist/blocklist | P1-1 | Middleware rejects disallowed IPs; configurable via config file; allowlist and blocklist both supported |
| P1-5 | Remaining routing strategies | `internal/strategy/` exists (`HookFactory`, `HookFactoryWithCacheAffinity`) | All named routing strategies implemented + tested: priority, fill-first, weighted, round-robin, p2c, least-used, random, cost-optimized, headroom, reset-window, reset-aware, context-relay, context-optimized, lkgp, auto, fusion, pipeline, cache-optimized, quota-weighted, expiry-first, jev |
| P1-6 | `auto/*` model aliases | `internal/autorouter/` exists | Each alias (auto/coding, auto/fast, auto/cheap, auto/subscription, auto/thrifty, auto/offline, auto/smart, auto/lkgp, auto/chaos) resolves to concrete model or fails honestly; tests |
| P1-7 | Combos (multi-model sequences) | routing engine chains | Combo = ordered model list with automatic fallback on quota/failure/cost spike; each step tried once; tests |

### Priority P2 — Webhooks, operations, polish

| ID | Task | Dependency | Acceptance Criteria |
|----|------|-----------|-------------------|

### Priority P2 — Webhooks, operations, polish

| ID | Task | Dependency | Acceptance Criteria |
|----|------|-----------|-------------------|
| P2-1 | Webhook integrations (Slack/Discord/Telegram) | `internal/webhooks/` exists | Payload templates per integration; delivery tests with mock webhooks; HMAC signing option |
| P2-2 | More webhook event types | `quota.exceeded` exists | `request.completed`, `request.failed`, `provider.exhausted`, `test.ping` emitted |
| P2-3 | Publish Docker images | `Dockerfile`, `docker-compose.yml` exist | Automated build on tags; image scans clean |
| P2-4 | Benchmark report | `scripts/smoke-followups-277.py` | Report comparing failover/hedge/latency numbers across our own configurations |

## Dependencies & Ordering

```
P0-1 (audit log all API requests) ── no ordering dependency with P0-2
P0-2 (semantic cache in path) ─────┤
                                     │
P1-1 (session auth) ───────────────┼─ P1-2, P1-3, P1-4 depend on P1-1
P1-2 (TOTP 2FA) ───────────────────┤
P1-3 (OAuth providers) ────────────┤
P1-4 (IP allowlist/blocklist) ─────┤
P1-5 (remaining strategies) ───────┤ depends on routing engine (exists)
P1-6 (auto/* aliases) ─────────────┤ depends on internal/autorouter (exists)
P1-7 (combos) ─────────────────────┤ depends on routing engine chains (exists)
                                     │
P2-1 (webhook integrations) ───────┤ depends on internal/webhooks (exists)
P2-2 (more webhook events) ────────┤ depends on quota.exceeded (exists)
P2-3 (Docker publish) ─────────────┤ depends on Dockerfile/docker-compose (exist)
P2-4 (benchmark report) ───────────┤ depends on scripts/smoke-followups-277.py (exists)
```

**Execution sequence (PR order):**

| PR | Focus | Est. |
|----|-------|------|
| #49 | P0-1: Request-level compliance audit log (all API requests) | 1 wk |
| #50 | P0-2: Semantic cache wired into request path | 1 wk |
| #51 | P1-1: Dashboard session auth (password + JWT cookie) | 1 wk |
| #52 | P1-2: TOTP 2FA + backup codes | 3 days |
| #53 | P1-3: OAuth provider connections (≥3) | 1 wk |
| #54 | P1-4: IP allowlist/blocklist | 2 days |
| #55 | P1-5: Remaining routing strategies (full named set) | 2 wk |
| #56 | P1-6: `auto/*` model aliases (9 aliases) | 1 wk |
| #57 | P1-7: Combos (multi-model sequences) | 1 wk |
| #58 | P2-1: Webhook integrations (Slack/Discord/Telegram) | 1 wk |
| #59 | P2-2: More webhook event types | 2 days |
| #60 | P2-3: Publish Docker images (automated on tags) | 2 days |
| #61 | P2-4: Benchmark parity report | 3 days |

---

## What is NOT in Phase 4 (and why)

| Feature | Reason |
|---------|--------|
| Guardrails pre/post enforcement | Already implemented (10 call sites: chat, a2a, audio, embeddings, gemini_native, images, legacy_completions, media_verify, relay, responses) |
| Streaming usage recording | Already implemented (G1 tests in `stream_usage_test.go`; `checkRequestBudgetOverageStream()` in `requestbudget.go`) |
| Embeddings API | Already implemented (`internal/server/embeddings.go`) |
| Images API | Already implemented (`internal/server/images.go`) |
| Audio API | Already implemented (`internal/server/audio.go`) |
| Desktop app / PWA | Excluded by user decision — headless server architecture only |
| i18n (multi-language) | Excluded by user decision — English-only |
| Helm/Kubernetes | Excluded from Phase 4 by user decision — future deployment work |
| Video/music generation, web search | Excluded by user decision — existing multimodal APIs sufficient |
| OIDC login | Excluded by user decision — simple password + session instead |
| 67-language i18n | Disproportionate for single HTML dashboard |
| Electron desktop app | Dilutes headless server-binary value |
| MITM proxy + TLS stealth | Redesign instead: SSRF guard + direct HTTP suffices |
| 56,000+ test declarations | Go table-driven + race-enabled testing is more maintainable |

---

## Per-PR acceptance criteria summary

Each PR must include:
- Unit tests covering new behavior (table-driven where applicable)
- `go test ./...` all packages ok
- `go vet ./...` clean
- `gofmt -l .` clean on changed files
- CI checks pass (ci, browser-smoke, bench-infra-smoke, supply-chain)
- No paid API credits or external cloud resources
- No credentials or secrets committed

**No implementation code has been modified. No Phase 4 branch or PR created.** Waiting for your explicit approval before beginning Phase 4 implementation.
