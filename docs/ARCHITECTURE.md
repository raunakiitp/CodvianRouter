# Codvian Router — architecture

> **Scope:** this document describes the system **as built** in the repo
> at `~/workspace/codvian-router/` as of 2026-11-17. Phases 1–3 backend
> is built, gate-verified, and wired into the binary (integration gate
> 2026-11-17, 224/224 tests). The Phase 2/3 packages are described in
> their own section with their wiring stated. No UI exists; UI is paused
> until Raunak supplies reference sites.

## Overview

Codvian Router is a local-first, self-hostable LLM gateway: one binary,
one OpenAI-compatible endpoint, one gateway API key. It sits between your
AI tools and every LLM provider you use, routes each request across the
providers that offer the requested model, fails over on errors, and
records honest usage in a local SQLite ledger. No hosted middleman is in
the request path. Auth is always on; the router binds localhost only.

Measured shape (coordinator's integration gate, 2026-11-17, Go 1.27.4):
single binary **17.3 MB**, **73 ms** to healthz-ready, **~32 MB** idle
RSS, **224/224** unit tests pass (Phase 1 gate was 133/133). See
[CHANGELOG.md](../CHANGELOG.md) and [CHECKLIST.md](../CHECKLIST.md).

## Module layout

```
cmd/codvian-router/      CLI entrypoint + wiring (coordinator-owned)
internal/
  config/                v1 JSON config + gateway keys (+ SCHEMA.md)
  crypto/                Vault: Argon2id + AES-256-GCM key sealing
  provider/              provider registry + OpenAI-compatible client
  router/                routing engine: chains, quota, 429s, hedge
  server/                OpenAI-compatible HTTP server
  ledger/                SQLite usage/budget ledger
  cache/                 three-layer cache (exact / prompt / semantic)
  metrics/               Prometheus-style counters + local JSON traces (Phase 2/3, wired: /metrics)
  eval/                  honest eval harness (Phase 2/3, wired: eval subcommand)
  health/                provider key/quota health checks (Phase 2/3, wired: watcher)
  degrade/               serve-stale-on-total-failure (Phase 2/3, wired: enabled in serve)
  scoring/               cost/latency/quality ranking (Phase 2/3, wired: OrderHook)
  tray/                  supervisor + optional system-tray icon (Phase 2/3, wired: tray subcommand)
  routetrace/            per-request attempt record (leaf package; breaks a would-be import cycle)
```

Dependencies (from `go.mod`): `golang.org/x/crypto v0.24.0`,
`modernc.org/sqlite v1.34.0` (pure-Go SQLite — no C toolchain needed).
Everything else is standard library.

## Packages and responsibilities

### `cmd/codvian-router` — CLI and wiring

- Subcommands: `serve` (default), `init`, `keygen`, `help`.
- Flags: `--config PATH`, `--port PORT`.
- `init` creates the config + first gateway key (scopes `chat`,
  `models`), prints the raw key **once**.
- `serve` wires the whole stack: config → Vault → SQLite ledger →
  provider registry → routing engine → HTTP server, then serves until
  SIGINT/SIGTERM (5 s graceful shutdown).
- Version constant: `0.1.0`.

### `internal/config` — versioned config, gateway keys

- Config file: JSON, `version` field, current version **v1**. Fields:
  `version`, `listen`, `data_dir`, `gateway_keys`, `providers`,
  `budget_usd`, `fail_closed`, `telemetry_enabled`.
- Defaults: listen `127.0.0.1:20128`; config path from Go's
  `os.UserConfigDir` (`~/.config/codvian-router/config.json` on Linux);
  data dir `~/.local/share/codvian-router`.
- Validation enforces: known version, parseable listen address,
  **loopback-only** listen host (127.0.0.1 or ::1) — the router is
  local-only by construction, not by convention.
- Gateway keys: raw format `crk_` + 32 random characters; only a hash is
  stored. Per-key scopes and budgets; key rotation and revocation.
  `budget_usd: 0` means no global cap set.
- `fail_closed`: when on, the "never bill me" mode refuses requests
  rather than risk unbudgeted spend. Telemetry defaults **off**.
- `internal/config/SCHEMA.md` documents the config schema (added by the
  config-stable builder).

### `internal/crypto` — key storage

- `crypto.Vault` seals provider API keys at rest with **AES-256-GCM**.
- The AES key is derived with **Argon2id (t=1, m=19 MiB, p=1)** from a
  machine-local master secret, with an optional passphrase mode.
  The Argon2id salt is a 16-byte random value stored in `kdf.salt`.
- `Seal` emits `v2:` values (Argon2id); `Unseal` accepts `v1:` and `v2:`;
  `ReSeal` upgrades sealed values to the current format.
- Limitation (honest): the master secret is still a local 0600 file —
  Argon2id only slows an attacker who has sealed values but not the
  secret. Rotation is manual: `Vault.ReSeal` each sealed value before
  replacing `master.key` (or changing the passphrase); losing `kdf.salt`
  is equally fatal. No passphrase by default and nothing prompts for one
  yet; a lost passphrase is unrecoverable by design. See
  `internal/crypto/README.md`.

### `internal/provider` — provider registry and client

- `ProviderConfig`: `id`, `name`, `base_url`, `api_key_encrypted`,
  `wire_format`, `models`, `enabled`, `quota_hint`, `created_at`. The
  config file is the single source of truth for providers.
- `Registry.Add` / `AddNamed`: validates the base URL, seals the key
  through the Vault cipher, runs an **inline key test** (a `GET
  /v1/models` call), **auto-fetches the model list**, and auto-detects
  the wire format (OpenAI-compatible first). If `/v1/models` 404s but
  chat answers, the provider is still added with an empty model list and
  wire format `openai` (some gateways only expose chat completions). Any
  other key-test failure (401, 429, 5xx, network error) rejects the add.
- `provider.Client`: `Chat` / `ChatStream` over `provider.ChatRequest` /
  `provider.ChatResponse`, with `Usage{PromptTokens, CompletionTokens,
  TotalTokens}`.
- Codvian is a provider option like any other: base URL + API key.
- `QuotaHint`: an operator-set float per provider, read live by the
  engine on every request (hint 0 = skipped, higher hint first). Nothing
  auto-derives hints from upstream quota headers yet.

### `internal/router` — routing engine

- `Engine` builds a fallback chain per request: enabled providers that
  offer the requested model, in registry order. Providers with no model
  list are candidates for any model (the upstream rejects unknown models
  and the engine fails over).
- **Quota-aware routing**: chains are filtered/sorted by live quota
  hints (`router.QuotaHintFunc` over the registry).
- **429 / `Retry-After` handling**: honours `Retry-After`, exponential
  backoff with jitter, per-key cooldowns.
- **Circuit breaker**: a repeatedly failing provider is taken out of
  rotation until it recovers.
- **Hedged requests**: the first attempt runs; if it has not returned
  within `HedgeDelay` (default **150 ms**), a backup attempt starts on
  the next provider and the first success wins. A hedged loser is
  recorded in the ledger as a failed attempt with zero tokens.
- **Streaming-safe failover**: mid-stream resume/restart, so a failure
  mid-response does not kill a long run.
- Extension points (frozen by `CONTRACTS.md`): `Config.OrderHook` —
  final deterministic reordering of the chain (must not drop/duplicate
  attempts, must not block); `Config.QuotaHint`; `Ledger.RecordAttempt`.
- Attempts are recorded through the ledger adapter: the full token split
  when the upstream reported it; otherwise total-only with
  `split_known=0` and cost 0 — never a guessed split. Cost is 0 when no
  per-model prices are configured.

### `internal/server` — OpenAI-compatible HTTP server

- Routes: `GET /healthz` (no auth; returns `{"status":"ok"}`),
  `GET /v1/models` (auth), `POST /v1/chat/completions` (auth).
- Auth: `Authorization: Bearer <gateway key>`; missing/unknown key → 401.
- Unknown model → clean JSON error, no crash.
- Streaming: SSE passthrough; the engine can resume a stream after a
  mid-stream failure (`bridgeStream` — see the ledger limitation below).
- A wrapper implementing `server.Engine` can sit between the engine and
  the server without touching either (`server.StreamResumer` for
  `ChatStreamFrom`).

### `internal/ledger` — usage and budget ledger

- SQLite (pure-Go `modernc.org/sqlite`), local file `ledger.db` in the
  data dir. Table `usage_events`: `id, ts, gateway_key_id, provider,
  model, prompt_tokens, completion_tokens, total_tokens,
  tokens_split_known, cost_usd, ok, latency_ms`.
- `RecordUsage(ctx, ledger.Usage{...}, ledger.Prices)`: full
  prompt/completion split recorded when known; split-unknown attempts
  record total + `split_known=0` and cost **0**, never invented.
- One budget cap (`budget_usd`) + fail-closed mode; per-key budget spend
  kept in sync with recorded usage.
- Limitation (honest): streaming usage is unknown to the engine
  (`bridgeStream` discards the upstream `ChatResponse`, which can carry
  usage from a final SSE chunk), so successful streams record zero tokens
  with cost 0 — documented at the call site, never invented. Cost is 0
  when no per-model prices are configured. `GatewayKeyID` is empty in
  engine-recorded rows: the engine does not know which gateway key a
  request arrived under.

### `internal/cache` — three-layer cache

1. **ExactCache** — in-memory LRU cache with per-entry TTL. Key =
   SHA-256 of the canonical request (`cache.ExactKey(model, messages)`).
2. **Provider prompt cache** — auto-injected cache markers per provider,
   with cache affinity preserved on fallback.
3. **SemanticCache** — a real local implementation: deterministic hashed
   bag-of-words (unigrams + bigrams) into a fixed **256-dimensional**
   float32 vector, cosine similarity ≥ **0.85** counts as a hit, LRU +
   TTL (default **500 entries, 1 h TTL**).

Honest limitation: the semantic cache is a token-overlap similarity
cache, **NOT a neural embedding model** — paraphrases with different
words will generally miss. It is **not yet wired into the
server/engine request path** (Phase 2 wiring); the interface exists and
is tested, the request path does not call it yet.

## Phase 2/3 packages — built, tested, and wired in

These packages landed in the tree during the Phase 2/3 build and were
wired into the binary by the coordinator on 2026-11-17: scoring
(`OrderHook`, ModeBalanced) fed by health-probe signals + quota hints;
the health watcher (startup + every 60 s); the metrics recorder
(`/metrics`, auth-gated) fed by the route trace; the degrade wrapper
(enabled in `serve`); the `eval`, `validate` and `tray` subcommands.
Integration gate: `go build` ✓, `go vet` clean, `gofmt -l .` clean,
`go test -count=1 ./...` → **224/224 PASS**, `-race` clean on all
touched/new packages, live mock-upstream smoke green (see
[CHECKLIST.md](../CHECKLIST.md)).

| Package | One line | Tests (docs-time run) |
|---|---|---|
| `internal/metrics` | Prometheus-style counters (hand-rolled text exposition, no Prometheus client lib) + a local JSON trace log; never sees secrets | 8 |
| `internal/eval` | Small honest eval harness: a case passes only if every expected substring appears in the response within the latency budget | 9 |
| `internal/health` | Checks each provider's key validity and quota state so dead/quota-exhausted providers can be skipped; never invents a quota value (only a quota-type failure ever lowers a hint, to 0) | 16 |
| `internal/degrade` | On total provider failure, serves a stale exact-match cached response — only when explicitly enabled, only with a visible staleness marker, never silently; implements `server.StreamResumer` so mid-stream resume still works through the wrapper | 23 |
| `internal/scoring` | Ranks chain candidates by cost, latency, quality, reliability; quality is 0/unknown by default (treated as neutral-minus-penalty 0.45) — no quality signal exists yet, so it cannot distort ordering | 11 |
| `internal/tray` | Supervises the gateway as a child process; with the `tray` build tag exposes a system-tray icon; backend only, never shows/logs/transmits secrets, only lifecycle state (`running`/`stopped`/`unknown`) | 7 |
| `internal/config` (extensions) | `SCHEMA.md` + config validation extensions; the v1 format itself is frozen | +5 tests vs the Phase 1 gate (16 → 21) |

Packaging (also landed, coordinator-owned for release): `Dockerfile`,
`docker-compose.yml`, `package.json` + `bin/codvian-router.js`,
`scripts/install.sh`, `.goreleaser.yml`. See
[INSTALL.md](INSTALL.md) — none of these is published yet.

## Request flow

```text
Client (any OpenAI-compatible app)
  │  POST /v1/chat/completions  +  Bearer <gateway key>
  ▼
server (auth check → 401 if no/unknown key)
  │  model ID from the request body
  ▼
router.Engine
  │  1. chainFor(model): enabled providers offering the model
  │  2. quota filter/sort via live QuotaHint (hint 0 = skipped)
  │  3. OrderHook (scoring, ModeBalanced — wired; signals from the
  │     health watcher's last probe + registry hints; cost/quality
  │     unknown (0) until prices/quality signals exist)
  │  4. attempt 1 → on 429/5xx/timeout: backoff, next in chain
  │     on slow first attempt (>150 ms): hedge on next provider
  │     circuit breaker skips providers that keep failing
  ▼
degrade.StaleEngine (enabled in serve: caches successes in the
  exact-match cache; on total failure serves a stale cached response
  marked X-Codvian-Stale, never after partial delivery)
  │
  ▼
provider.Client (per-provider OpenAI-compatible HTTP)
  │  upstream answers; usage parsed from the response
  ▼
ledger.RecordUsage (every attempt: ok or failed, tokens or 0/0/0)
  │
  ▼
server → JSON (or SSE stream) back to the client
  + metrics recorded (requests/latency/in-flight/failovers, from the
    route trace) and served at GET /metrics (gateway-key auth required)
```

Notes on the flow:

- The app never sees upstream keys — only the single gateway key and
  base URL.
- A provider whose key cannot be decrypted or whose base URL is invalid
  is skipped with a warning at startup; it must not take the whole
  gateway down.
- The `auto` model name (smart routing modes) is planned, not built —
  today the request must name a real model ID.

## Failure handling (measured)

All numbers below are the coordinator's gate measurements on localhost
mocks (2026-11-17, upstream answering in 50 ms). Failover, hedge,
quota-skip and stale rows are the Phase 2–4 integration gate; the
mid-stream row is the Phase 1 gate (same day, same mocks — the resume
path through the degrade wrapper is additionally unit-tested).
Real-world numbers depend on upstream latency.

| Failure | Behaviour | Measured |
|---|---|---|
| Upstream returns 429 instantly | Fail over to the next provider in the chain | 52.4–55.0 ms, 5/5 OK (~1–2 ms router overhead vs 51.2 ms direct) |
| Upstream slow (2,000 ms) | Hedge at 150 ms; first success wins | 203.8–205.9 ms, 5/5 OK |
| Provider quota hint 0 | Skipped entirely, never attempted, no ledger row | 52.4–52.8 ms, 5/5 OK |
| Failure mid-stream | Stream resumes/restarts on the next provider | SSE chunks `bench ` + `ok` + `stop` + `[DONE]` |
| No gateway key | 401 | as expected |
| Unknown model | Clean JSON error, no crash | as expected |
| All providers fail | Stale exact-match response served, marked `X-Codvian-Stale` (+ age); uncached prompt → 502 | Same prompt after mocks killed: 200 + `X-Codvian-Stale: true` (age ~519 ms); new prompt: 502 |

Design targets (not measurements): detect failure in 2–3 s, switch in
0.1–0.3 s, route under 1 s. The measured 429 switch on localhost mocks
is ~1–2 ms — far inside target, because the mocks are local.

## Honest limitations

Copied from `CHECKLIST.md` (Phase 1 known limitations) — these are not
blockers, but they are real:

- **Crypto:** master secret is still a local 0600 file — Argon2id only
  slows an attacker who has sealed values but not the secret. Rotation is
  manual (`Vault.ReSeal` each sealed value before replacing `master.key`
  or changing the passphrase); losing `kdf.salt` is equally fatal. No
  passphrase by default and nothing prompts for one yet; a lost
  passphrase is unrecoverable by design.
- **Ledger:** streaming usage is unknown to the engine, so successful
  streams record zero tokens with cost 0. Cost is 0 when no per-model
  prices are configured.
- **Registry quota hints** are wired into the engine (live read on every
  request) and auto-derived in one direction: the health watcher sets
  hint 0 when a key probe fails with a quota-type error. Hints are never
  auto-raised and never invented from a successful probe.
- **Semantic cache** is a deterministic hashed bag-of-words similarity
  cache, NOT a neural embedding model: paraphrases with different words
  will generally miss. It is not yet wired into the server/engine
  request path (still open).
- **`go.mod`** was concurrently edited by parallel agents mid-build; it
  has converged (`github.com/raunakiitp/codvian-router`, `go 1.22`,
  `modernc.org/sqlite v1.34.0`, `golang.org/x/crypto v0.24.0`).
