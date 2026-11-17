# Codvian Router — Roadmap

> Status: **in development**. Nothing in Phases 1–4 is built yet. This
> roadmap is the plan, and it will be updated as work lands.

## Targets that define the product

- **Failover:** detect a failed / rate-limited / out-of-quota model in
  2–3 seconds, switch to a live model in **0.1–0.3 seconds**.
- **Request circulation:** ~0.1 seconds for a request to be routed across
  the user's providers.
- **Local performance:** fast even on a low-end PC — small binary, ~1 s
  start, tens of MB idle.
- **Simplicity:** setup so clear that anyone can follow it. Premium,
  polished UI — curved cards, clear buttons, no clutter, nothing that
  looks half-finished.

## Phase 0 — Research (live now)

- Hourly research into LLM routers worldwide: best features, latency
  reduction, and low-end local performance.
- Hourly deep research into router GitHub repositories (OmniRoute,
  LiteLLM, Portkey, CLIProxyAPI, 9router, and others): best features
  extracted into a research log that builds draw from.
- Planned technical direction from research so far:
  - Go core for low latency (reference: Go gateways reporting ~11 µs
    overhead at thousands of requests per second).
  - Hedged-request failover: after ~150 ms, send the same request to a
    second provider and take whichever answers first — instead of
    waiting for the first to fail.
  - Three-layer caching: exact-match, provider prompt cache, semantic
    cache.
  - Auth on by default, no default credential, localhost-only bind.

## Phase 1 — MVP core

- OpenAI-compatible single base URL + one unified gateway key (per-key
  scopes/budgets, rotation, no default credential).
- One-click provider onboarding for any/custom provider: "Get your API
  key →" link, inline key test, auto-fetch model list, auto-detect wire
  format. Codvian as a provider option (login, or base URL + API key).
- Routing engine: fallback chains, quota-aware routing, 429 /
  `Retry-After` handling, circuit breaker, streaming-safe failover with
  mid-stream resume.
- Unified credit/usage ledger across all pooled keys; one budget cap;
  honest free-tier accounting.

## Phase 2 — Live UX + observability

- Premium UI: models here → connect here → routing here.
- Live routing view: which model just failed, which is live, where
  traffic is going right now.
- Prompt caching, cost/latency/quality routing, eval harness,
  Prometheus/OpenTelemetry observability.

## Phase 3 — Local-first performance + packaging

- Single small binary / npm / Docker / tray app; ~1 s start, tens of MB
  idle; fast on low-end PCs and on a server for life.
- Stable versioned config, key health / quota intelligence, hedging,
  graceful degradation.

## Phase 4 — Public launch

- Docs, install guides, demo, public GitHub repository, community
  (Discord/Telegram), release channels.

## What we will NOT copy from OmniRoute

- No inflated "1.62B free tokens" headline — free-tier numbers are
  reported honestly, pool by pool.
- No TLS stealth / MITM proxy.
- No long tail of flaky free pools.
- No kitchen-sink scope — the router does routing, and does it fast.
