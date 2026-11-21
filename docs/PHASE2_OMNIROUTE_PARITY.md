# Phase 2 — OmniRoute Parity Matrix

Reference: `https://github.com/cheaper-inference/OmniRoute` (public repo/docs/behavior; study only — no code copying, assets or branding). Codvian baseline: `main` @ `33c2aaa` (Phase 1 accepted, PRs #19–#24 merged, main CI green).

**Status vocabulary** (evidence-based, never inflated): `NOT_IMPLEMENTED` · `PARTIALLY_IMPLEMENTED` · `IMPLEMENTED_NOT_WIRED` · `MOCK_TESTED` · `INTEGRATION_TESTED` · `RUNTIME_VERIFIED` · `REAL_PROVIDER_VERIFIED` · `BLOCKED_EXTERNAL_DEPENDENCY` · `UNSUPPORTED` · `COMPLETE`.

Rule: a row flips only with file:line + test/runtime evidence recorded in the Ledger (`docs/PHASE2_LEDGER.md`). Metadata rows, toast-only controls, mock responses and dead buttons never count as working features (`scripts/audit-fake.sh` gate).

---

## A. Live UX / Dashboard (directive §4.1–§4.3)

| Reference feature | Reference behavior | Codvian implementation | Gap | Status |
|---|---|---|---|---|
| Operator dashboard shell | Full web UI for providers/keys/usage/settings | `web/index.html` single-file app (~13k lines): `:root` token system (light = pure white surfaces + transparent borders + layered soft shadows; dark = pure black + bright lime `#BEF264`; accent lime `#3F6212`; status red `#8C1D18`; Plus Jakarta Sans / JetBrains Mono), sidebar 252px, ~30 page sections, dark-mode toggle (`data-theme`), embedded via `internal/server/web.go` | Depth/completeness of individual pages vs reference | PARTIALLY_IMPLEMENTED — verify page-by-page |
| Provider onboarding/management | Add/edit/disable/re-enable providers, key lifecycle | POST /api/providers, keys add/delete/role, **rotate** endpoint (added 2026-11-20), enable/disable endpoint | UI coverage + live-test/discovery flows in pages | verify page-by-page |
| Live routing view | Real request/attempt activity, decisions, failover, breakers, cache decisions | `/dashboard/trace`, `/dashboard/streaming`, decision headers (`X-Codvian-Decision`, strategy), trace JSON feed exist | Candidate-pool visibility with rejection reasons, hedge winner/loser, retry/cooldown explanations in UI | PARTIALLY_IMPLEMENTED |
| Usage/cost analytics | Per-provider/model usage, cost, tokens, budgets | Ledger + `/dashboard/overview`-style endpoints + `/api/cache/stats`, `/api/analytics/*` | G1 closed chat streaming splits; Responses/combo accounting + cache-replay details open (Ledger P2-T1) | PARTIALLY_IMPLEMENTED |
| Budgets/quotas/teams UI | Budget management + enforcement surfaces | Backend: per-key windowed budgets, internal scoped budgets, quota pools/schedules, teams (PLAN M5) | UI coverage verify | verify page-by-page |
| Settings/config UX | Config editing + validation feedback | Config stable + `validate` subcommand; dashboard settings pages | Wiring depth verify | verify page-by-page |

## B. Observability (§4.4)

| Reference feature | Codvian implementation | Gap | Status |
|---|---|---|---|
| Prometheus metrics | `/metrics` (auth-gated), per-(provider,model) p50/p95/p99, cost counters, breaker state | G1 usage/cost/streaming metric extensions | PARTIALLY_IMPLEMENTED |
| OpenTelemetry | `-tags otel` build-tagged in-process tracer (W3C traceparent, span tree, bounded ring) — **no OTLP exporter** | Configurable OTLP export (endpoint validation, sampling, failure handling); local/test collector verification | PARTIALLY_IMPLEMENTED |
| Structured logs + correlation | Access log rows, X-Request-Id accept/echo, trace JSON opt-in | Field standardization audit | INTEGRATION_TESTED (verify depth) |
| Webhook alerting | Delivery mechanism exists (`internal/server/webhooks_api.go`) | Event coverage audit (provider/routing/budget/ops failures) | verify |

## C. Routing intelligence (§5)

Codvian `internal/strategy/` is extensive (priority/balanced/cheapest/fastest/most-reliable/least-latency/weighted/custom-weights, round-robin, random/strict-random, fill-first, least-used, p2c, quota-weighted, headroom, expiry-first, reset-aware/reset-window, lkgp, context/cache affinity, context-relay, quotashare DRR, cascade, fusion/pipeline/jev combos, auto/* family + candidate pool, 16-factor scoring + mode packs). PLAN.md records Area-1 weighted 73.8% at M3 exit with residual = data-source gaps (billing class, premium classification, production summariser/quota signal, cross-request store), NOT wiring.

| Item | Status | Notes |
|---|---|---|
| Strategy registry breadth | MOCK_TESTED (31 names, conformance suites) | Verify each has distinct semantics vs reference (Ledger P2-T3) |
| auto/* dispatch + candidate pool | MOCK_TESTED | Tune exploration/quality with measured data |
| Data-source-dependent rows (headroom/expiry/premium class) | BLOCKED until sources exist | Honest unknowns; do not invent |
| Feedback-learned weights | IMPLEMENTED_NOT_WIRED (OFF by default) | Enable path + real feedback loop (P2-T4) |

## D. Caching (§6)

| Item | Status | Notes |
|---|---|---|
| Exact cache + persistence (SQLite) + flush + bypass headers + per-request controls | COMPLETE per PLAN M6 (gate green 2026-11-18) | Re-verify on current tree |
| Semantic cache (local token-overlap + Redis + persistent) | WIRED (degrade WrapWithSemantic; threshold config; last-3-turns scope) | Streaming-path behavior, isolation tests, savings accuracy (P2-T5) |
| Provider prompt-cache markers/affinity | MOCK_TESTED (OpenRouter marker suite) | Extend honestly where providers differ |
| Cache replay usage details | PARTIAL — basic counts only | P2-T1 |

## E. Performance (§7)

Benchmark rig exists (`scripts/bench.sh`, historical JSONs in docs/). Historical numbers are stale labels; Phase 2 builds a reproducible campaign (direct-upstream baseline, router overhead, warm/cold, contamination checks, limiter-shed separation) and reports misses honestly.

## F. Security/privacy (§8) — carried from Phase 1 gates

SSRF/egress (untouched, tested), encrypted credentials, scope enforcement, secret redaction (PEM + spend strings + log redaction), telemetry label cardinality, prompt/content non-leakage. Every Phase 2 block re-runs these lenses.

## G. Evaluation & diagnostics (§4.5)

`eval` subcommand exists. Phase 2 extends permanent regression cases: protocol compatibility, nested model IDs, reasoning-only, streaming/non-streaming usage, empty/failover, cache isolation, strategies/decision explanations (P2-T6).

---

## Comparison discipline

- Parity % is computed mechanically from THIS table's statuses; never counted from docs/UI/mocks.
- "Better than reference" requires a measured comparison (latency/RPS/memory/savings) recorded in the Ledger.
- Deliberate divergences (admission queue, mid-stream continuation, cookie-session providers, TLS spoofing) stay documented as divergences, not silently omitted.