# Codvian Router — Roadmap

> Status: **Phases 1–3 backend built & verified (2026-11-17)** — 224/224
> tests pass, 429 failover 52–55 ms on localhost mocks, `/metrics`,
> health watcher, graceful degradation, eval/tray/validate subcommands
> and packaging all wired and live-smoke-verified. The UI is PAUSED by
> Raunak (backend first; he supplies reference sites later), and the
> Phase 4 release (published binary, npm, Docker image) is still ahead.

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
- Hourly deep research into router GitHub repositories (sector baselines,
  comparable LLM gateways: best features
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

## Phase 1 — MVP core — BUILT & VERIFIED (2026-11-17)

- OpenAI-compatible single base URL + one unified gateway key (per-key
  scopes/budgets, rotation, no default credential).
- One-click provider onboarding for any/custom provider: "Get your API
  key →" link, inline key test, auto-fetch model list, auto-detect wire
  format. Codvian as a provider option (login, or base URL + API key).
- Routing engine: fallback chains, quota-aware routing, 429 /
  `Retry-After` handling, circuit breaker, streaming-safe failover with
  mid-stream resume. Hedged requests (150 ms hedge delay).
- Unified credit/usage ledger across all pooled keys; one budget cap;
  honest free-tier accounting — token splits recorded honestly, unknown
  splits cost $0, never invented.
- Hardening landed with the build: Argon2id key derivation (t=1, m=19 MiB,
  p=1) + optional passphrase, v2 seal format with v1 unseal compat;
  registry quota hints wired live into the engine; real local semantic
  cache (deterministic token-overlap similarity — not a neural model).

Measured (localhost mocks, upstream answering in 50 ms): 429 failover
52–56 ms (~1–2 ms router overhead); hedge switch ~204 ms; quota-hint-0
provider skipped entirely; startup to healthz-ready 73 ms; idle ~32 MB;
single binary 16.9 MB. Test gate: `go build` clean, `go vet` clean,
`gofmt` clean, `go test -count=1 ./...` 133/133 pass.

## Phase 2 — Live UX + observability — BACKEND BUILT & VERIFIED (2026-11-17)

- Premium UI: PAUSED by Raunak (2026-11-17) — he did not like the UI
  preview; no dashboard, mockups, or palette work until the backend is
  "1000% best" and he supplies reference websites/components. Eventual
  direction noted for then: light colour first (not dark navy/black),
  with a light/dark toggle.
- Live routing view: not built (UI, paused with the rest of the UI).
- Cost/latency/quality routing: BUILT — scoring package, 5 modes
  (Balanced/Fast/Cheap/Quality/Reliable), wired into the engine as
  `OrderHook` (ModeBalanced), driven by health-probe latency/success +
  quota hints. Cost/quality signals are honestly unknown (0) until
  prices/quality data exist, so they cannot distort ordering.
- Eval harness: BUILT — `eval` subcommand, smoke cases 3/3 vs live mocks.
- Prometheus/OpenTelemetry observability: BUILT (Prometheus part) —
  hand-rolled `/metrics` exposition (auth-gated), request/latency/
  in-flight/failover counters; JSON trace log opt-in
  (`CODVIAN_ROUTER_TRACE=1`). Honestly NOT the OTel SDK — no OTLP
  exporter yet.
- Semantic cache: real local implementation exists (Phase 1 hardening)
  but is still NOT wired into the request path.

## Phase 3 — Local-first performance + packaging — BUILT & VERIFIED (2026-11-17)

- Single small binary / npm / Docker / tray app: binary 17.3 MB,
  startup to healthz-ready 73 ms, idle ~32 MB. Dockerfile (distroless
  nonroot), docker-compose, npm wrapper, `.goreleaser.yml`,
  checksum-verified `install.sh` — all written; the Docker image was NOT
  built/run here (no Docker in the VM) and no release exists yet, so the
  npm/installer happy paths are untested.
- Stable versioned config: BUILT — unknown fields survive Load→Save,
  `ValidateFile`, `validate` subcommand.
- Key health / quota intelligence: BUILT — watcher probes all providers
  at startup + every 60 s, quota-type probe failures set hint 0.
- Hedging: BUILT (Phase 1), live-verified ~204 ms on a 2 s-slow provider.
- Graceful degradation: BUILT — stale exact-match responses served when
  every provider fails, marked `X-Codvian-Stale`; mid-stream resume
  preserved through the wrapper; live-verified.

## Phase 4 — Public launch

- Docs, install guides, demo, public GitHub repository, community
  (Discord/Telegram), release channels.
- Repository made public 2026-11-17 (by Raunak, for unlimited CI minutes);
  docs (INSTALL, DEMO, ARCHITECTURE, CHANGELOG, RELEASE-v0.2.0 draft)
  written 2026-11-17. Still ahead: the v0.2.0 release itself (published
  binary, npm package, Docker image), community channels.

## What we will NOT copy from the reference benchmark

- No inflated "1.62B free tokens" headline — free-tier numbers are
  reported honestly, pool by pool.
- No TLS stealth / MITM proxy.
- No long tail of flaky free pools.
- No kitchen-sink scope — the router does routing, and does it fast.
