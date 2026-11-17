# Codvian Router

> **One endpoint. Every provider. Never stop coding.**

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Status: In Development](https://img.shields.io/badge/status-in%20development-blue.svg)](ROADMAP.md)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

**Codvian Router is in development — the router itself is not built yet.**
Everything in this README describes the plan, not a shipped product. There
is no installable build yet, and no performance number here is a measured
result. Planned items are labelled **(planned)** throughout.

Codvian Router is an open-source LLM API router from Codvian. It is a
separate project from Codvian (the Windows desktop AI coding agent) — it
does not change Codvian, and it is not a feature of it.

---

## Table of contents

- [What is Codvian Router?](#what-is-codvian-router)
- [Why another router?](#why-another-router)
- [Planned features](#planned-features)
- [How routing works (planned)](#how-routing-works-planned)
- [Quick start (coming soon)](#quick-start-coming-soon)
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

- **Local-first (planned).** Install it on your own computer, or deploy it
  on your own server and run it for life. There is no hosted middleman in
  the request path.
- **Any provider (planned).** Any provider in the world, including custom
  ones. No fixed provider list. Add a key, and it joins the pool.
- **Your keys never leave your machine (planned).** Keys are stored
  locally, encrypted, and never sent to Codvian or anyone else. No
  telemetry by default. No default credential — ever.
- **Simple on purpose.** Other routers are confusing. Codvian Router is
  built so anyone can follow it: models here, connect here, routing here.
  A live view shows which model just failed, which one is live, and where
  your traffic is going right now.

---

## Why another router?

Good routers already exist — most notably OmniRoute, the strongest
reference for this project (MIT, TypeScript/Node.js; 88,214 stars and
~394 providers / ~1,491 models as read on 2026-11-17 — see the research
summary in [Comparison](#comparison)). Codvian Router exists because the
research found clear openings that no existing router closes:

| Problem in existing routers | What Codvian Router plans to do instead |
|---|---|
| Custom-provider onboarding is multi-step and hand-typed — add provider, add connection, paste key, type a model ID by hand | **One-click onboarding:** pick a provider or paste any base URL → a "Get your API key →" link to that provider's key-creation page → paste key → inline test → the router auto-fetches the model list, so you never type model IDs |
| Failover leaks errors: broken models surface 400/401 before fallback kicks in; streaming tool calls can break on fallback | **Streaming-safe failover:** pre-flight health checks, circuit breakers, and mid-stream resume, so a 429 never kills a long agentic run |
| No automatic provider-native prompt caching (OmniRoute open issue #813) | **Caching done properly (planned):** auto-injected cache markers per provider, with cache affinity preserved on fallback |
| Heavy: 8–10 GB RAM for one coding agent; narrow Node version window | **Light by design (planned):** a single small binary, ~1 s start, tens of MB idle — fast even on a low-end PC |
| Defaults that invite trouble: a dashboard password of literally `CHANGEME`, opt-in MCP scope enforcement | **Secure by default:** auth on, localhost-only bind, no default credentials, scoped access on by default |
| ToS-grey features: TLS-fingerprint stealth, MITM interception, inflated free-token headlines | **ToS-clean by design:** no stealth, no MITM, honest free-tier numbers with sources |

---

## Planned features

Everything in this table is **planned**, not shipped.

| Feature | What it means |
|---|---|
| One unified key + base URL | One gateway key issued by the router, one OpenAI-compatible base URL (`http://localhost:<port>/v1`), usable by any agent or app |
| Per-key scopes & budgets | One key for coding, one for chat — each with its own scope and cap; key rotation and revocation |
| One-click provider onboarding | "Get your API key →" link per provider (including free-model providers), inline key test, auto-fetched model list, auto-detected wire format |
| Codvian as a provider option | Sign in with your Codvian account, or paste Codvian's base URL + API key — the same as any other provider |
| Fallback chains | Ordered chains of models; when one fails, the next healthy one takes over |
| Quota-aware routing | Routing that prefers providers with quota headroom, and spends quota that would expire unused first |
| 429 / `Retry-After` handling | Honours `Retry-After`, exponential backoff with jitter, per-key cooldowns |
| Streaming-safe failover | Mid-stream resume / restart, so a failure mid-response does not kill the run |
| Unified credit / usage ledger | One ledger across all pooled keys: spend normalised to a single currency + token count, one monthly budget cap, fail-closed "never bill me" mode |
| Three-layer caching | Exact-match cache, provider prompt cache, semantic cache — with hit-rate and savings visible in the dashboard |
| Live routing view | See, live, which model just failed, which one is live, and where traffic is going |
| Observability & budgets | Per-provider / per-model / per-task cost, latency (p50/p95), error and cache-hit dashboards; Prometheus/OTel export |
| Premium, polished UI | Curved cards, clear buttons, no clutter — simple enough for anyone to follow |
| Smart routing modes | Planned `auto`-style modes: fast, cheap, quality-first, subscription-only (fail-closed) |
| Local-first install | Single small binary (Windows, macOS, Linux), one-command Docker / self-host path, local SQLite store |

---

## How routing works (planned)

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
   Targets (not measured results): detect failure in 2–3 seconds, switch
   in **0.1–0.3 seconds**. A measured failover-latency SLO will be
   published with a public benchmark once the router exists — existing
   routers only *claim* "milliseconds", and no independent measurement of
   those claims was found in our research.
5. **You can watch it happen.** The live view shows the current route,
   the last failure, and the reason for each switch.

```http
POST http://localhost:<port>/v1/chat/completions
Authorization: Bearer <your-codvian-router-key>
Content-Type: application/json

{
  "model": "auto",
  "messages": [{ "role": "user", "content": "Hello" }]
}
```

> The port, the `auto` model name, and the request above are the **planned**
> interface. They do not exist yet.

---

## Quick start (coming soon)

> There is no installable build yet. Do not try to install Codvian Router
> from this repository today — nothing here runs.

When the first release lands, installation will be published here as:

- a **single binary** (Windows, macOS, Linux),
- an **npm** install,
- and a **one-command Docker** / self-host path.

The intended first-run flow is: install → run → open the dashboard →
connect at least one provider → copy your gateway key → point your agent
at the router's base URL. That is the plan; the commands will appear here
only once they are real and tested.

---

## Roadmap

| Phase | Focus | Status |
|---|---|---|
| Phase 0 | Research — hourly, live | In progress |
| Phase 1 | MVP core: unified key + base URL, one-click onboarding, routing engine, unified ledger | Planned |
| Phase 2 | Live UX + observability: live routing view, prompt caching, cost/latency/quality routing, budgets | Planned |
| Phase 3 | Local-first performance + packaging: small binary, fast on low-end PCs, stable versioned config | Planned |
| Phase 4 | Public launch: docs, install guides, demo, public repository, community | Planned |

See [ROADMAP.md](ROADMAP.md) for the full plan, including the
"do NOT copy" list (no inflated free-token headline, no TLS stealth or
MITM, no flaky free pools, no kitchen-sink scope).

---

## Comparison

Planned differentiators only — Codvian Router is not built yet, so this
compares **intent** against what the research verified about rivals
(all rival facts as read on 2026-11-17; see the research report in the
project notes).

| Dimension | Codvian Router (planned) | OmniRoute | LiteLLM | OpenRouter | Portkey AI Gateway |
|---|---|---|---|---|---|
| Hosting | Self-hosted, local-first | Self-hosted, local-first | Self-hosted or SDK | Managed SaaS (cloud) | Self-host OSS or SaaS |
| License | MIT | MIT | MIT | Proprietary service | Apache-2.0 OSS core |
| Custom providers | One-click: key-creation link, inline test, auto-fetched models | Yes, but multi-step and hand-typed | Yes, but file-based config only | No — catalog only | Limited catalog (~30) |
| Single unified key / base URL | Yes, with per-key scopes & budgets | Yes | Yes | Yes (cloud) | Yes |
| Failover | Streaming-safe, mid-stream resume; published measured SLO | 22 strategies, 3-layer resilience — but leaks errors on broken models, and "milliseconds" is an unverified vendor claim | Priority fallbacks, retries | Retries on another provider on 429s | Weighted, fallbacks |
| Prompt caching | Auto-injected per provider, cache affinity on fallback | Semantic cache only; no auto prompt caching (#813) | Semantic caching available | Passthrough | Semantic caching (paid) |
| Unified credit ledger | One ledger + one budget cap across all keys | No — separate quotas per provider | Strongest spend control (virtual keys, budgets) | Single payment, passthrough pricing | Budgets, guardrails (paid) |
| Weight | Single small binary, ~1 s start (target) | 8–10 GB RAM for one coding agent | Python, production-proven | Zero-ops | Enterprise-ready |
| Security defaults | Auth on, no default credential, scoped by default | Defaults to `CHANGEME`; MCP scope opt-in | — | — | Strongest guardrails / audit |

**What Codvian Router will not copy from OmniRoute:** the "1.62B free
tokens" headline, TLS-fingerprint stealth and MITM interception,
lossy-compression marketing percentages, the long tail of flaky no-auth
pools, and kitchen-sink scope. A smaller catalog of *reliable* providers
plus effortless custom-provider onboarding beats hundreds of half-dead
ones.

---

## Contributing

Contributions are welcome — once the router exists, and for docs and
repo setup right now. Please read [CONTRIBUTING.md](CONTRIBUTING.md) and
the [Code of Conduct](CODE_OF_CONDUCT.md) first. Good first issues are
labelled `good first issue`.

## Security

If you find a vulnerability, please report it privately — do **not** open
a public issue. See [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 Codvian.
