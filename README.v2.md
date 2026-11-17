# Codvian Router

> **One endpoint. Every provider. Never stop coding.**

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Status: In Development](https://img.shields.io/badge/status-in%20development-blue.svg)](ROADMAP.md)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

**Phases 1–3 backend is built and verified (2026-11-17).** The Go routing
engine, unified key + base URL, provider onboarding, fallback/hedging,
quota-aware routing, three-layer caching, the unified ledger, scoring,
eval harness, Prometheus observability, health/quota intelligence,
graceful degradation, stable config, tray supervisor and packaging all
exist and pass their test gate (224 tests — see [Roadmap](#roadmap)).
There is no release binary yet — build from source (below). The UI is
paused (backend first). Planned items are labelled **(planned)**
throughout; built items are labelled **(built)**.

Codvian Router is an open-source LLM API router from Codvian. It is a
separate project from Codvian (the Windows desktop AI coding agent) — it
does not change Codvian, and it is not a feature of it.

---

## Table of contents

- [What is Codvian Router?](#what-is-codvian-router)
- [Why another router?](#why-another-router)
- [Features](#features)
- [How routing works](#how-routing-works)
- [Quick start (build from source)](#quick-start-build-from-source)
- [Roadmap](#roadmap)
- [Comparison](#comparison)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

---

## What is Codvian Router?

Codvian Router is a **local-first, self-hostable gateway** that sits between
your AI tools and every LLM provider you use.

You add your own provider API keys. The router gives you **one API key and
one OpenAI-compatible base URL** to plug into any AI agent or app — no code
changes in the app, just a different base URL and key.

- **Local-first (built).** Install it on your own computer, or deploy it
  on your own server and run it for life. There is no hosted middleman in
  the request path. Single Go binary (16.9 MB), ~73 ms to healthz-ready,
  ~32 MB idle (measured 2026-11-17 on localhost mocks).
- **Any provider (built).** Any provider in the world, including custom
  ones. No fixed provider list. Add a key, and it joins the pool.
- **Your keys never leave your machine (built).** Keys are stored
  locally, encrypted at rest (AES-256-GCM, key derived with Argon2id from
  a machine-local secret, optional passphrase), and never sent to Codvian
  or anyone else. No telemetry by default. No default credential — ever.
- **Simple on purpose.** Other routers are confusing. Codvian Router is
  built so anyone can follow it: models here, connect here, routing here.
  A live view (planned) shows which model just failed, which one is live,
  and where your traffic is going right now.

---

## Why another router?

Good routers already exist — most notably OmniRoute, the strongest
reference for this project (MIT, TypeScript/Node.js; 88,214 stars and
~394 providers / ~1,491 models as read on 2026-11-17 — see the research
summary in [Comparison](#comparison)). Codvian Router exists because the
research found clear openings that no existing router closes:

| Problem in existing routers | What Codvian Router plans to do instead |
|---|---|
| Custom-provider onboarding is multi-step and hand-typed — add provider, add connection, paste key, type a model ID by hand | **One-click onboarding (built):** pick a provider or paste any base URL → a "Get your API key →" link to that provider's key-creation page → paste key → inline test → the router auto-fetches the model list, so you never type model IDs |
| Failover leaks errors: broken models surface 400/401 before fallback kicks in; streaming tool calls can break on fallback | **Streaming-safe failover (built):** pre-flight health checks, circuit breakers, and mid-stream resume, so a 429 never kills a long agentic run — measured 52–56 ms for a 429 failover on localhost mocks (50 ms upstream), hedge at 150 ms |
| No automatic provider-native prompt caching (OmniRoute open issue #813) | **Caching done properly:** auto-injected cache markers per provider (built), with cache affinity preserved on fallback; exact-match + semantic layers built, semantic layer is a local token-overlap cache, not a neural model |
| Heavy: 8–10 GB RAM for one coding agent; narrow Node version window | **Light by design (built):** a single small Go binary, 16.9 MB, ~73 ms start, ~32 MB idle — fast even on a low-end PC |
| Defaults that invite trouble: a dashboard password of literally `CHANGEME`, opt-in MCP scope enforcement | **Secure by default (built):** auth on, localhost-only bind, no default credentials, scoped access on by default |
| ToS-grey features: TLS-fingerprint stealth, MITM interception, inflated free-token headlines | **ToS-clean by design:** no stealth, no MITM, honest free-tier numbers with sources |

---

## Features

Phase 1–3 backend rows are **built** (verified 2026-11-17, 224/224 tests
pass); the UI rows are **planned** (UI paused by the founder until the
backend is best).

| Feature | Status | What it means |
|---|---|---|
| One unified key + base URL | Built | One gateway key issued by the router, one OpenAI-compatible base URL (`http://localhost:<port>/v1`), usable by any agent or app |
| Per-key scopes & budgets | Built | One key for coding, one for chat — each with its own scope and cap; key rotation and revocation |
| One-click provider onboarding | Built | "Get your API key →" link per provider (including free-model providers), inline key test, auto-fetched model list, auto-detected wire format |
| Codvian as a provider option | Built | Sign in with your Codvian account, or paste Codvian's base URL + API key — the same as any other provider |
| Fallback chains | Built | Ordered chains of models; when one fails, the next healthy one takes over |
| Quota-aware routing | Built | Routing that prefers providers with quota headroom, and spends quota that would expire unused first; registry quota hints wired live into the engine (hint 0 = skipped) |
| 429 / `Retry-After` handling | Built | Honours `Retry-After`, exponential backoff with jitter, per-key cooldowns |
| Streaming-safe failover | Built | Mid-stream resume / restart, so a failure mid-response does not kill the run |
| Unified credit / usage ledger | Built | One ledger across all pooled keys: spend normalised to a single currency + token count, one monthly budget cap, fail-closed "never bill me" mode; token splits recorded honestly, unknown splits cost $0, never invented |
| Three-layer caching | Built | Exact-match cache wired into the request path (incl. stale degradation when every provider fails); provider prompt cache; semantic cache built (local token-overlap similarity — not a neural embedding model) but not yet wired into the request path — hit-rate/savings dashboard still planned with the UI |
| Live routing view | Planned | See, live, which model just failed, which one is live, and where traffic is going (UI paused) |
| Observability & budgets | Built | Prometheus `/metrics` (auth-gated): per-provider/model request, latency, in-flight and failover counters; JSON trace log opt-in (`CODVIAN_ROUTER_TRACE=1`); per-task cost/latency dashboards still planned with the UI (OTel export not built — the trace log is honestly not the OTel SDK) |
| Premium, polished UI | Planned | Curved cards, clear buttons, no clutter — simple enough for anyone to follow (UI paused until the backend is best; founder supplies reference sites later) |
| Smart routing modes | Built | 5 scoring modes — Balanced, Fast, Cheap, Quality, Reliable — wired into the engine (ModeBalanced default); the `auto` model name is still planned |
| Health & quota intelligence | Built | Key probe + watcher (startup + every 60 s); quota-type probe failures set the registry hint to 0 so exhausted providers are skipped |
| Graceful degradation | Built | Stale exact-match response served when every provider fails, marked `X-Codvian-Stale`; mid-stream resume preserved |
| Eval harness | Built | `eval` subcommand: smoke cases against the live engine (3/3 vs localhost mocks) |
| Headless tray supervisor | Built | `tray` subcommand: supervises the gateway, one auto-restart after an unexpected exit (GUI tray icon deferred — needs GTK dev headers) |
| Local-first install | Built | Single small binary (17.3 MB; Windows, macOS, Linux), Dockerfile, docker-compose, npm wrapper, `install.sh`, one-command Docker path files — no release published yet, so the packaged happy paths are untested; build from source for now |

---

## How routing works

1. **You connect providers.** Add any provider's API key — from the
   catalog, or any custom base URL. Codvian Router tests the key inline
   and fetches the provider's model list for you.
2. **You get one key and one base URL.** Point any AI agent or app at the
   router's OpenAI-compatible endpoint with the gateway key. The app never
   sees your upstream keys.
3. **Every request is routed.** The router picks the best eligible model
   for the request — by cost, latency, quota headroom, and quality —
   from the keys you connected.
4. **Failures switch, they don't stop you.** If a model goes down, hits a
   rate limit (429), or runs out of quota, routing moves to a live model.
   Measured on localhost mocks (2026-11-17, upstream answering in 50 ms):
   a 429 failover completes in **52–56 ms** (~1–2 ms of router overhead),
   and a hedged slow-provider switch completes in **~204 ms** (150 ms
   hedge delay + 50 ms upstream). Real-world numbers will depend on
   upstream latency; these are mock measurements, not a public benchmark.
5. **You can watch it happen (planned).** The live view will show the
   current route, the last failure, and the reason for each switch.

```http
POST http://localhost:<port>/v1/chat/completions
Authorization: Bearer <your-codvian-router-key>
Content-Type: application/json

{
  "model": "auto",
  "messages": [{ "role": "user", "content": "Hello" }]
}
```

> The endpoint and request shape above exist in Phase 1. The `auto`
> model name is still **planned** (Phase 2 smart modes) — today you
> request a real model ID that one of your providers offers.

---

## Quick start (build from source)

There is no release binary yet — build it from source (Go 1.22+):

```sh
git clone https://github.com/raunakiitp/CodvianRouter
cd CodvianRouter
go build -o codvian-router ./cmd/codvian-router
./codvian-router init      # creates config + your first gateway key (shown once)
./codvian-router serve     # listens on 127.0.0.1:20128, auth ON
```

Then add providers to the config file (base URL + API key, sealed at rest
with Argon2id + AES-256-GCM) and point any agent at
`http://127.0.0.1:20128/v1` with your gateway key. A packaged binary,
npm install, and one-command Docker path files exist (Dockerfile,
docker-compose, npm wrapper, `install.sh`) but no release is published
yet, so build from source for now; there is no dashboard yet (UI paused).

---

## Roadmap

| Phase | Focus | Status |
|---|---|---|
| Phase 0 | Research — hourly, live | In progress |
| Phase 1 | MVP core: unified key + base URL, one-click onboarding, routing engine, unified ledger | **Built & verified 2026-11-17** — 133/133 tests, 429 failover 52–56 ms on localhost mocks |
| Phase 2 | Backend observability: cost/latency/quality routing, eval harness, Prometheus metrics, health/quota intelligence | **Built & verified 2026-11-17** — 224/224 tests incl. Phases 2–3; live routing view + UI paused |
| Phase 3 | Local-first performance + packaging: small binary, fast on low-end PCs, stable versioned config, graceful degradation, tray, Docker/npm | **Built & verified 2026-11-17** — Docker image not built/run here; no release published yet |
| Phase 4 | Public launch: docs, install guides, demo, public repository, community | Partially done — repository made public 2026-11-17; docs written 2026-11-17; v0.2.0 release + community still ahead |

See [ROADMAP.md](ROADMAP.md) for the full plan, including the
"do NOT copy" list (no inflated free-token headline, no TLS stealth or
MITM, no flaky free pools, no kitchen-sink scope).

---

## Comparison

Phase 1 is built; Phase 2+ rows are still intent. This compares the
built-or-planned Codvian Router against what the research verified about
rivals (all rival facts as read on 2026-11-17; see the research report in
the project notes).

| Dimension | Codvian Router (Phase 1 built) | OmniRoute | LiteLLM | OpenRouter | Portkey AI Gateway |
|---|---|---|---|---|---|
| Hosting | Self-hosted, local-first | Self-hosted, local-first | Self-hosted or SDK | Managed SaaS (cloud) | Self-host OSS or SaaS |
| License | MIT | MIT | MIT | Proprietary service | Apache-2.0 OSS core |
| Custom providers | One-click: key-creation link, inline test, auto-fetched models | Yes, but multi-step and hand-typed | Yes, but file-based config only | No — catalog only | Limited catalog (~30) |
| Single unified key / base URL | Yes, with per-key scopes & budgets | Yes | Yes | Yes (cloud) | Yes |
| Failover | Streaming-safe, mid-stream resume; measured 52–56 ms on localhost mocks | 22 strategies, 3-layer resilience — but leaks errors on broken models, and "milliseconds" is an unverified vendor claim | Priority fallbacks, retries | Retries on another provider on 429s | Weighted, fallbacks |
| Prompt caching | Auto-injected per provider, cache affinity on fallback; semantic layer is a local token-overlap cache, not a neural model | Semantic cache only; no auto prompt caching (#813) | Semantic caching available | Passthrough | Semantic caching (paid) |
| Unified credit ledger | One ledger + one budget cap across all keys; honest split accounting, unknown splits cost $0 | No — separate quotas per provider | Strongest spend control (virtual keys, budgets) | Single payment, passthrough pricing | Budgets, guardrails (paid) |
| Weight | Single small binary, 16.9 MB, ~73 ms start, ~32 MB idle (measured) | 8–10 GB RAM for one coding agent | Python, production-proven | Zero-ops | Enterprise-ready |
| Security defaults | Auth on, no default credential, scoped by default | Defaults to `CHANGEME`; MCP scope opt-in | — | — | Strongest guardrails / audit |

**What Codvian Router will not copy from OmniRoute:** the "1.62B free
tokens" headline, TLS-fingerprint stealth and MITM interception,
lossy-compression marketing percentages, the long tail of flaky no-auth
pools, and kitchen-sink scope. A smaller catalog of *reliable* providers
plus effortless custom-provider onboarding beats hundreds of half-dead
ones.

---

## Contributing

Contributions are welcome — for the Phase 1 engine, docs, and repo
setup right now. Please read [CONTRIBUTING.md](CONTRIBUTING.md) and
the [Code of Conduct](CODE_OF_CONDUCT.md) first. Good first issues are
labelled `good first issue`.

## Security

If you find a vulnerability, please report it privately — do **not** open
a public issue. See [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 Codvian.
