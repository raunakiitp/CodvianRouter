# Changelog

All notable changes to Codvian Router will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **How to read this file:** numbers come only from the coordinator's
> test gate in `CHECKLIST.md` or from a test run recorded here. Nothing
> in this file is a claim about a release that has not happened — the
> first release (v0.1.0) has not been published yet (see
> [docs/INSTALL.md](docs/INSTALL.md)).

## [Unreleased]
### Added
- Phase 2 carry-over closure: exact-cache replay now preserves provider-reported usage details (`cached_tokens`, `reasoning_tokens`) end-to-end — store clamps malformed negatives, serialization is omitempty-backward-compatible (memory/SQLite/Redis entries unchanged), and replays report the same usage shape as the original live response. Regression tests: round-trip, negative clamp, JSON backward compatibility.
- Phase 2 carry-over closure: Responses API streaming usage accounting proven end-to-end — a new server test drives `/v1/responses` stream:true through a real engine and asserts both the `response.completed` usage block (input/output/total/cached) and the engine ledger row (reported split, not an estimate).
- Phase 2 carry-over closure: combo/pipeline streaming usage accounting proven — the panel executes non-streaming, records the winner's provider-reported split into the ledger (losers as cancellations), and replays the response verbatim with its usage on the Done event; regression test locks in one-success-row / no-drain-double-count.
### Observability
- Phase 2/T6 analytics accuracy: provider-REPORTED cost now surfaces next to the priced-computed spend wherever it exists — ledger `UsageSummary.reported_cost_usd/_rows`, `/api/analytics/summary|costs` fields, an explicit pricing-vs-billing delta note when they disagree, and a dashboard reported-cost suffix. Fixed a real import bug found by the new reconciliation test: `ImportEvents` silently dropped reported costs and the cancelled flag (hedge losers), so imported history never reconciled; it now mirrors `normalizeUsage` semantics (valid report wins on its row, invalid ignored).
- Phase 2/T7 metrics audit closed: `/metrics` now also reports provider-reported usage — `codvian_router_usage_tokens_total{provider,model,kind}` (prompt/completion/cached/reasoning) and `codvian_router_usage_cost_usd_total{provider,model,source="reported"}` — fed from the engine's single `recordAttempt` path (chat and streaming counted exactly once), with a fixed kind vocabulary and full label sanitization so no key or payload can ever reach the exposition. Hand-computed pricing is deliberately not duplicated in metrics (the ledger analytics API owns it).
- Phase 2/T7 webhook coverage: two new edge-triggered provider-health events, `provider.degraded` (CLOSED->OPEN breaker trip) and `provider.recovered` (OPEN breaker cleared), delivered best-effort by the existing HMAC-signed dispatcher with a payload limited to provider id + real consecutive/trip/open-for counts — no secret, key or prompt content. Fired from genuine breaker transitions via `router.SetBreakerEvents`; validated for configuration, in the webhook management API, and with a local-receiver delivery test.
- Request tracing wired end-to-end for `-tags otel` builds: every HTTP request gets a root `codvian.request` span (request id + route); when `tracing_otlp_endpoint` is configured in config, spans also ship to that OTLP/HTTP collector as OTLP/JSON from a bounded queue (never blocks requests, failed batches dropped and counted, invalid endpoints ignored). Verified against a local test collector decoding the real wire format, plus drop/invalid-endpoint behavior tests and a server-level wiring test. Default builds remain no-op tracing.
### Live Routing view depth + semantic-cache audit
- Live Routing rows now explain the real candidate outcomes the router emits: hedge loser (was a raw "context canceled"), circuit-breaker/cooldown/zero-quota/attempt-budget skips, and per-row hedge winner/loser tags derived from the request's actual hedge decision. Extend-only change to the existing feed renderer — same classes, palette and layout; a node-driven regression test runs the SERVED inline script (`node --check` + fixture attempts) so the labels cannot drift.
- Semantic cache tenant isolation audit (T9): confirmed the M6.11 scoped store closes cross-tenant paraphrase and cross-temperature leaks on Chat; added the previously missing stream-surface proof (cross-tenant identical text, cross-tenant paraphrase, and cross-temperature lookups on ChatStream all miss under a failing provider, with a same-tenant semantic-hit control); corrected the stale test-file documentation that still listed these as known failures.


### G1 — streaming usage accounting (2026-11-21)

- **OpenAI-wire streams now request `stream_options.include_usage`**,
  so upstreams that honor it (OpenAI itself included) send the final
  usage chunk. The client's own `stream_options.include_usage` is
  decoded and honored: a completed stream emits ONE final usage
  chunk (empty choices array) before `[DONE]` — only when the
  upstream actually reported usage; a local estimate is never
  presented as provider-reported.
- **Reported streaming usage reaches the ledger on every wire.**
  The engine drain carries `Done.Resp.Usage` (all wire clients
  already aggregate their usage events: OpenAI final chunk,
  Anthropic message_delta, Gemini usageMetadata, Cohere message-end,
  Bedrock metadata); a successful stream with a report is reconciled
  exactly like a non-streaming report (provider numbers verbatim,
  local estimate alongside, divergence calibration, reported cost
  preserved). No report keeps the labelled local-estimate fallback.
  Last report wins — never summed; one ledger row per attempt.
- **Streaming budget overages are now recorded**: the overage check
  shares one priced-actual core between streaming and non-streaming
  paths; the spend rides the telemetry trailer (headers are already
  committed on streams). Pre-send/mid-chain gates unchanged.
- Tests: provider/router/server `stream_usage_test.go` suites
  (request flag, aggregation, decode table, ledger reconciliation,
  fallback, usage-chunk emission rules, streaming overage feed).
  Non-streaming accounting, cancellation semantics and the degrade
  wrapper preserved. Details: docs/PROVIDER_QUIRKS_AUDIT.md §7.

### Phase 1 Provider Quirks Audit Slice (2026-11-21)

- **Fix: nested model IDs are addressable verbatim.** Aggregator model
  IDs listed in `/v1/models` (e.g. OpenRouter's `liquid/lfm-3-1.7b:free`)
  were mis-pinned to catalog providers by the B2-29 prefix rule and
  400'd with `routing.only: unknown provider`. The prefix is now only
  a pin when the named provider serves the bare model; a full ID a
  registered provider serves verbatim routes untouched. Explicit
  `openrouter/<id>` pins and all conflict rules are preserved.
  New registry queries `OffersModel`/`ProviderOffersModel`; tests:
  `TestPrefixedModelRoutingNestedIDs`, `TestRegistryOffersModel`.
- **Fix: reasoning-only completions are not empty streams.** A model
  that spent its whole budget thinking (`finish_reason: length`, no
  answer text) served real output; both the non-streaming
  (`isEmptyCompletion`) and streaming preread (`doneHasReasoning`)
  paths now commit it instead of failing over every target and
  erroring. Empty reasoning states still fail over. Tests:
  `reasoning-only-is-not-empty`, `TestStreamingReasoningOnlyIsNotEmpty`.
- **429 retry storms: root cause was the PR #20 pool-join bug**
  (single-key chain). The bounded, body-classified 429 policy
  (quota-exhausted / concurrency-limit / backoff ladder + per-key
  cooldowns) is documented with code references and unchanged.
- **Audit:** `docs/PROVIDER_QUIRKS_AUDIT.md` — wire/client coverage
  sweep (all declared wires have mock-tested clients), catalog-driven
  auth verification, and a prioritized backlog (streaming usage
  accounting, cross-account verification, quota-blocked completion).
  `docs/PROVIDER_AUTH_MATRIX.md` statuses aligned to the operator's
  evidence distinctions (cross-account failover NOT verified;
  completion QUOTA-BLOCKED / PENDING; Azure Entra MOCK-TESTED).

### Phase 1 follow-up: live verification + pool-join fix (2026-11-21)

- **Fix: provider-login pool join now registers the updated key pool.**
  Live verification with a real OpenRouter account found that the
  callback's pool-join path handed the routing hook the PRE-add provider
  snapshot, so the engine never registered the second key and every
  request kept using the first. `attachOpenRouterKey` now re-reads the
  provider after `AddKey`, and a pool join uses the key-changed hook
  (matching the key-management API). Regression test:
  `TestProviderLoginPoolJoinHookSeesUpdatedPool`.
- **Live evidence recorded** in `docs/PROVIDER_AUTH_MATRIX.md` (OpenRouter
  PKCE row promoted to real-provider-tested for exchange, validation,
  encrypted storage, 441-model discovery, pool join, restart persistence,
  per-key failover on 429/402, and zero secret leakage). Successful chat
  completion is NOT yet evidenced: the operator account's free tier
  returned persistent 429s and it has never purchased credits (tiny paid
  probe → honest upstream 402; $0 spent). Azure Entra ID stays
  mock-tested. Three quirks recorded for the provider-quirks audit
  slice (nested model-ID addressing, 429 retry storms, empty-content
  length finishes).

### Phase 1 provider authentication (2026-11-21)

- **OpenRouter account login (OAuth PKCE).** `codvian-router provider
  login openrouter` (headless code paste, works over SSH) and
  `POST /admin/provider/login` + `GET /admin/provider/login/callback`
  (loopback browser callback) implement OpenRouter's official PKCE flow:
  fresh S256 challenge and 256-bit single-use state per flow, 10-minute
  pending TTL, bounded pending map. The exchange returns a user-controlled
  API key (no refresh token exists in this flow); it is live-tested against
  the upstream, stored encrypted, and its models discovered — the same
  registry path as a pasted key. Further logins join the existing
  OpenRouter provider's key pool as additional accounts (per-key failover
  rotates across them). The callback route is loopback-only without a
  gateway key (the one-time PKCE state is its credential); off-host
  requests are 403. No code, state, verifier or key is logged or rendered.
  Route counts: 313 → 314 (LOCAL_ONLY 10 → 11); honest-501 list 64 → 63
  (`POST /admin/provider/login` is real now).
- **Azure OpenAI via Microsoft Entra ID client credentials.** Store
  `{tenant_id, client_id, client_secret}` JSON as the `azure-openai`
  provider key: the router mints a Bearer token at the tenant's
  `/oauth2/v2.0/token` endpoint (scope
  `https://cognitiveservices.azure.com/.default`), caches it in process
  memory with a 60 s expiry skew, and re-mints on expiry through the
  now expiry-aware auth cache. Client-credentials grants issue no refresh
  token — re-minting is the refresh behavior. Mint breaker (5 consecutive
  failures → 30 min cooldown), validated `token_uri`/`scope` overrides,
  tenant path-segment validation, and token-endpoint error bodies never
  echoed (status + AAD error code only). The static `api-key` variant is
  unchanged.
- **Preserved, not rewritten:** Bedrock SigV4, Vertex service-account JWT
  exchange, static-key pools (preferred/reserve, per-key failover),
  add/rotate live key tests, model discovery and refresh.
- **Evidence:** deterministic mock auth-host/upstream tests
  (`internal/providerlogin`, `internal/provider/entra_test.go`,
  `internal/server/provider_login_test.go` incl. a real-registry
  end-to-end multi-account + restart-persistence test). Both flows are
  **mock-tested**; `real-provider-tested` requires an operator account and
  is tracked per installation in docs/PROVIDER_AUTH_MATRIX.md. Usage and
  security properties: docs/PROVIDER_LOGIN.md.
- **Still unsupported (honest refusals):** subscription/browser-cookie
  sign-in (ToS-grey catalog entries), the OpenRouter `jwt` catalog variant,
  and any exchange kind without an implemented, tested minting path.

### Phase 0 security closure (2026-11-21)

- **Webhook SSRF / DNS rebinding fixed.** Delivery used a plain
  `http.Client`, so a URL validated at registration was resolved again
  at delivery and on every retry; a rebinding host could then reach
  loopback, private or cloud-metadata addresses. The egress guard now
  lives in `internal/egress` and is shared by `/v1/web/fetch` and webhook
  delivery: every connection (including retries) dials only the IP it
  validated, mixed public/private answers are refused, redirects are not
  followed, environment proxies are ignored, TLS still verifies the
  hostname. A blocked destination is terminal (no retries). Registration
  validates with the same policy, so reserved ranges such as TEST-NET are
  now refused there too, and config-file loopback webhooks are no longer
  delivered.
- **`/admin/update` with no published release.** GitHub's 404 for
  `releases/latest` is confirmed against the repository endpoint; only
  "repository exists, no release" answers 200 with `latest: null`,
  `release_available: false`, `status: no_published_release`. Rate
  limits (503 + Retry-After), timeouts (504), network, auth, 5xx,
  malformed responses and a missing repository (502) each keep their own
  `error.code` (`internal/release/latest.go`, shared with the CLI).

### Phase 0 cleanup (2026-11-21)

- **Removed fabricated endpoints:** `POST /v1/systemone` (returned a canned
  "processed successfully" without calling a model), `GET|POST
  /v1/video-bridge/drilldown` (fixed frame counts and scores for any id),
  `GET /v1/audio/voices` and `GET /v1/voices` (a static list presented as
  provider data). They now answer 404.
- **Security:** `/v1/web/fetch` dials only the IP it validated
  (`internal/server/egress_guard.go`): DNS rebinding, mixed public/private
  answers, IPv4-mapped/NAT64/6to4 tricks, redirects to private hosts and
  environment proxies can no longer reach internal addresses; TLS still
  verifies the hostname. DNS lookup failures no longer fail open.
- **`POST /admin/restart`** is a real graceful relaunch (supervised: exit
  code 75 and the tray supervisor restarts it; unsupervised: re-launches
  the same binary). It no longer falls back to shutdown while answering
  "restarting". Added to the spawn-capable patterns; the two guard tests
  pass.
- **`POST /admin/update`** is a real release check against the GitHub API
  (never installs; `codvian-router update` installs with signature +
  checksum checks).
- **`POST /v1/audio/text-to-speech/{voiceId}`** now writes the path voice
  into the request body (it was ignored before).
- **Fixed: audio, images, search, OCR, video and music answered 503 in
  the real server.** The serving wrapper (`degrade.StaleEngine`) did not
  forward `AudioPassthrough`, `Images` or `MediaPassthrough`, so the
  server's capability checks failed even though `router.Engine` serves
  them. The wrapper now forwards them (never cached, never stale).
- **64 routes answer `501 feature_unavailable`** with a reason and, where
  one exists, the route to use instead (`internal/server/unavailable_routes.go`):
  provider login, service install, plugin load, and 61 B8 dashboard
  routes whose settings were stored in memory and never read by the router.
- **Analytics:** heatmap and weekly pattern are computed from the ledger
  (new `Ledger.DayBuckets`, UTC calendar days); the budget table uses the
  enforced `KeyBudgetStatus` (it divided USD by token limits before); the
  scoring inspector and scoring modes report the scorer's real weights
  instead of invented values.
- `scripts/audit-fake.sh` §5b checks these contracts and fails on a known
  fake.

### Added

- Phase 2/3 backend packages, **wired into the binary and gate-verified
  by the coordinator on 2026-11-17** — `go build` exit 0, `go vet`
  clean, `gofmt -l .` clean, `go test -count=1 ./...` **224/224 PASS**
  (was 207/207 at docs time), `-race` clean on all touched/new
  packages, plus the live smoke below:
  - `internal/metrics` (8 tests) — Prometheus-style counters and a local
    JSON trace log, stdlib only. Mounted at `GET /metrics` (gateway-key
    auth required); the server records requests, latency, in-flight and
    failovers (from the route trace).
  - `internal/eval` (9 tests) — honest eval harness (substring +
    latency-budget pass criteria only). `eval --model MODEL` subcommand;
    smoke cases 3/3 vs live mocks.
  - `internal/health` (16 tests) — provider key/quota health checks;
    never invents a quota value. Watcher probes all providers at
    startup + every 60 s; quota-type probe failures set hint 0.
  - `internal/degrade` (23 tests) — opt-in serve-stale on total failure,
    always with a visible staleness marker (`X-Codvian-Stale` header on
    non-streaming responses, logged for streams). Enabled in `serve`.
    Implements `server.StreamResumer` (delegates, else replays with
    rune-accurate trimming) and forwards `LastTrace` for metrics.
  - `internal/scoring` (11 tests) — cost/latency/quality/reliability
    ranking for the fallback chain (`router.Config.OrderHook`).
    Wired as ModeBalanced, fed by health-probe latency/success +
    registry quota hints; cost/quality stay unknown (0) until
    prices/quality signals exist.
  - `internal/tray` (7 tests) — gateway supervisor + optional
    system-tray icon (tray build tag); backend only. `tray` subcommand;
    auto-restart live-verified.
  - `internal/config/SCHEMA.md` — config schema documentation; config
    tests grew 16 → 21. `validate` subcommand added.
  - `internal/routetrace` (new leaf package, no internal imports) —
    `RouteTrace`/`AttemptTrace` moved out of `internal/router` so
    server metrics and degrade forwarding cannot create an import cycle
    (`router_test` imports `server`).
- `serve` gains `--listen host:port` (explicit opt-in for a non-loopback
  bind: 127.0.0.1 / ::1 / 0.0.0.0 only; 0.0.0.0 prints a warning; never
  written to the config file) and `CODVIAN_ROUTER_TRACE=1` (JSON trace
  log to stderr, off by default).
- Packaging files (exist in the repo, **not published**): `Dockerfile`
  (multi-stage, distroless runtime), `docker-compose.yml`,
  `package.json` + `bin/codvian-router.js` (npm wrapper, Node >= 18,
  `"license": "MIT"`), `scripts/install.sh`, `.goreleaser.yml`
  (Linux/macOS/Windows × amd64/arm64, `checksums.txt`).
- Docs: `docs/INSTALL.md`, `docs/DEMO.md`, `docs/ARCHITECTURE.md`,
  `docs/RELEASE-v0.2.0.md` (draft), and this changelog.

### Verified (integration gate, 2026-11-17, real binary + Python mocks)

- 429 failover **5/5, 52.4–55.0 ms**; hedge **5/5, 203.8–205.9 ms**;
  quota-hint-0 skip **5/5, 52.4–52.8 ms**.
- `/metrics` after the failover run: requests counter 5 (winner
  `prov_mock_b`, `outcome="ok"`), failovers counter 5
  (`prov_mock_a` → `prov_mock_b`), `in_flight 0`; no key → 401.
- Stale degradation: both mocks killed → same prompt **200 with
  `X-Codvian-Stale: true`**; fresh prompt → 502; fresh requests carry
  no stale header. Ledger: failed attempts 0/0/0 `split_known=0`.
- `eval --model bench-model`: **3/3 passed (100%)**. `validate`: valid
  → 0, invalid → 1. `--listen` bad host → refused, exit 1.
  `tray`: child killed with `kill -9` → auto-restarted, healthz OK;
  SIGTERM → exit 0, child down.

### Known limitations (new in this gate)

- Docker image not built/run here (no Docker in the VM); no release
  exists yet, so npm/installer happy paths are untested.
- Semantic cache is built but still not wired into the request path.
- Scoring cost/quality signals are unknown (0) until prices/quality
  data exist.
- Metrics attribution is approximate under concurrency (most-recent
  trace); the ledger is the billing record.

## [0.1.0] - 2026-11-17

Phase 1 MVP backend, built and verified by the coordinator's test gate
on 2026-11-17 (Go 1.27.4). No release binary was published with this
entry — v0.1.0 is the version stamped in the binary (`Version = "0.1.0"`)
and the version recorded in the gate.

### Added

- Go module scaffold: single binary, **16.9 MB**, **~1 s start**
  (measured **73 ms** to healthz-ready in the gate run; previous run
  12 ms), **~32 MB** idle RSS.
- Versioned config file (JSON, v1), localhost-only bind
  (`127.0.0.1:20128` by default), auth ON by default; config at
  `~/.config/codvian-router/config.json`, data at
  `~/.local/share/codvian-router`.
- Gateway API key issue/verify (`crk_` + 32 characters), per-key scopes
  and budgets; CLI subcommands `serve` (default), `init`, `keygen`,
  `help`.
- Keys encrypted at rest: AES-256-GCM, key derived with Argon2id
  (t=1, m=19 MiB, p=1); `v2:` seal format, `v1:` unseal compatibility,
  `ReSeal` migration; passphrase mode.
- Provider registry: any base URL + API key, inline key test, auto-fetch
  model list, wire-format auto-detect (OpenAI-compatible first); Codvian
  as a provider option like any other.
- Routing engine: fallback chains, quota-aware routing (live registry
  hints — hint 0 = skipped, higher hint first), 429 / `Retry-After`
  handling with backoff + per-key cooldown, circuit breaker, hedged
  requests (backup attempt starts after the default 150 ms hedge delay),
  streaming-safe failover with mid-stream resume.
- Three-layer caching: exact-match (LRU + TTL, SHA-256 key) → provider
  prompt cache → semantic cache (deterministic hashed bag-of-words,
  256-dim, cosine ≥ 0.85, LRU + TTL) — explicitly NOT a neural embedding
  model, and not yet wired into the request path.
- Unified credit/usage ledger (SQLite, local): full prompt/completion
  split recorded when known; split-unknown attempts record total +
  `split_known=0` and cost 0, never invented. One budget cap +
  fail-closed "never bill me" mode.
- OpenAI-compatible server: `POST /v1/chat/completions`,
  `GET /v1/models`, `GET /healthz` (`{"status":"ok"}`).
- No telemetry by default.

### Verified (test gate, 2026-11-17)

- `go build ./...` exit 0; `go vet ./...` clean; `gofmt -l .` clean.
- `go test -count=1 ./...` — **133/133 PASS, 0 fail** (cache 21,
  config 16, crypto 15, ledger 12, provider 29, router 26, server 14).
- 5,629 lines non-test Go + 3,733 lines of tests, 21 Go files.
- Live benchmark (real binary, Python mock upstreams, router on
  127.0.0.1:20199, upstream answering in 50 ms):
  - A returns 429 instantly → failover to B: **5/5 OK, 52.4–55.8 ms**
    (~1–2 ms overhead vs 51.2 ms direct to B).
  - A slow (2,000 ms) → hedge at 150 ms, B wins: **5/5 OK,
    203.9–204.4 ms**.
  - A quota hint 0 → A skipped entirely, B serves: **5/5 OK,
    52.2–53.5 ms**.
  - Streaming with 429 failover: SSE chunks `bench ` + `ok` + `stop` +
    `[DONE]`.
  - No gateway key → 401; unknown model → clean JSON error, no crash.
  - Ledger verified against the SQLite file: failed A attempt records
    prompt 0 / completion 0 / total 0 with `split_known=0`; successful B
    attempt records prompt 10 / completion 5 / total 15 with
    `split_known=1` and cost $0 (no per-model prices configured); a
    hedged loser is recorded as a failed attempt with zero tokens; a
    quota-skipped provider leaves no row.

### Known limitations

- Master secret is a local 0600 file; rotation is manual
  (`Vault.ReSeal` each sealed value before replacing `master.key`);
  losing `kdf.salt` is equally fatal; a lost passphrase is
  unrecoverable by design.
- Streaming usage is unknown to the engine, so successful streams record
  zero tokens with cost 0.
- Registry quota hints are operator-set, and (since the Unreleased
  wiring) quota-type health-probe failures auto-set hint 0; hints are
  never auto-raised or invented from a successful probe.
- Semantic cache is not a neural embedding model and is not yet wired
  into the request path.
