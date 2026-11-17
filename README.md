# Codvian Router

> One endpoint. Every provider. Never stop coding.

**Codvian Router is in development.** This repository is being set up — the
router itself is not built yet. Everything below describes the plan, not a
shipped product.

Codvian Router is an open-source LLM API router from Codvian. It is a
separate project from Codvian (the Windows desktop AI coding agent) — it does
not change Codvian. It is the router Codvian itself uses to reach many
providers through one place, released as open source.

## What it is

A local-first, self-hostable gateway that sits between your AI tools and
every LLM provider you use. You add your own provider API keys, and Codvian
Router gives you **one API key and one OpenAI-compatible base URL** to plug
into any AI agent or app.

- **Local-first** — install it on your own computer, or deploy it on your
  own server and run it for life. There is no hosted middleman in the
  request path.
- **Any provider** — any provider in the world, including custom ones. No
  fixed provider list. Add a key, and it joins the pool.
- **Your keys never leave your machine** — keys are stored locally, never
  sent to Codvian or anyone else. No telemetry by default.
- **Failover that actually fails over** — when a model goes down, hits a
  rate limit (429), or runs out of quota, routing switches to a live model
  in a fraction of a second. Target: detect failure in 2–3 seconds, switch
  in 0.1–0.3 seconds.
- **Simple on purpose** — other routers are confusing. Codvian Router is
  built so anyone can follow it: models here, connect here, routing here.
  A live view shows which model just failed, which one is live, and where
  your traffic is going right now.

## Planned features

- Single OpenAI-compatible base URL + one unified gateway key (with
  per-key scopes, budgets, and rotation — no default credential).
- One-click provider onboarding: a link to create the provider's API key,
  an inline key test, automatic model-list fetch, and automatic wire-format
  detection. Codvian itself is also a provider option (sign in with your
  Codvian account, or paste Codvian's base URL + API key).
- Routing engine: fallback chains, quota-aware routing, 429 /
  `Retry-After` handling, circuit breakers, and streaming-safe failover
  with mid-stream resume. Research direction: hedged requests and a Go
  core for low latency.
- A unified credit/usage ledger across all your pooled keys, with one
  budget cap — and honest accounting of free tiers, never an inflated
  headline number.
- Three-layer caching (exact-match, provider prompt cache, semantic
  cache) as a planned performance direction.
- A premium, polished UI: curved cards, clear buttons, no clutter.

See [ROADMAP.md](ROADMAP.md) for the full plan.

## Install

> Coming soon — the repository is in setup. There is no installable build
> yet. Installation instructions (single binary, npm, Docker) will be
> published here once the first release lands.

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
and the [Code of Conduct](CODE_OF_CONDUCT.md) first. Good first issues are
labelled `good first issue`.

## Security

If you find a vulnerability, please report it privately — do **not** open a
public issue. See [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 Codvian.
