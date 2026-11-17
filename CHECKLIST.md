# Codvian Router — Phase 1 MVP Backend Checklist

Backend-first. No UI. Local worktree only: `~/workspace/codvian-router/`.
Build-first-test-last: built ALL pieces, then ONE test gate at the end.
Gate run 2026-11-17 by the coordinator (independent re-run, not the builders' own claims).

| # | Feature | Built | Verified |
|---|---|---|---|
| 1 | Go module scaffold, single binary, ~1s start | ✅ | ✅ |
| 2 | Versioned config file (JSON, v1), localhost-only bind, auth ON by default | ✅ | ✅ |
| 3 | Gateway API key issue/verify, per-key scopes & budgets | ✅ | ✅ |
| 4 | Keys encrypted at rest (AES-256-GCM; method documented in internal/crypto/README.md) | ✅ | ✅ |
| 5 | Provider registry: any base URL + API key, inline key test, auto-fetch model list, wire-format auto-detect (OpenAI-compatible first) | ✅ | ✅ |
| 6 | Codvian as a provider option (base URL + API key) | ✅ | ✅ |
| 7 | Routing engine: fallback chains | ✅ | ✅ |
| 8 | Quota-aware routing | ✅ | ✅ |
| 9 | 429 / Retry-After handling, backoff + per-key cooldown | ✅ | ✅ |
| 10 | Circuit breaker | ✅ | ✅ |
| 11 | Hedged requests (backup attempt starts early, ~150ms) | ✅ | ✅ |
| 12 | Streaming-safe failover, mid-stream resume | ✅ | ✅ |
| 13 | Three-layer caching: exact-match → provider prompt cache → semantic (stub interface) | ✅ | ✅ |
| 14 | Unified credit/usage ledger (SQLite, local) | ✅ | ✅ |
| 15 | One budget cap + fail-closed "never bill me" mode | ✅ | ✅ |
| 16 | OpenAI-compatible server: POST /v1/chat/completions, GET /v1/models | ✅ | ✅ |
| 17 | No telemetry by default | ✅ | ✅ |
| 18 | Test gate: unit tests + `go build` + live smoke test (mock upstream, 429 on model A → failover to model B) | ✅ | ✅ |
| 19 | Failover latency benchmark (measured, not claimed) | ✅ | ✅ |
| 20 | Crypto hardening: Argon2id KDF (t=1, m=19 MiB, p=1) + passphrase mode, v2 seal format, v1 unseal compat, `ReSeal` migration | ✅ | ✅ |
| 21 | Registry quota hints wired into the engine (live `QuotaHintFunc`, hint 0 = skipped, higher hint first) | ✅ | ✅ |
| 22 | Ledger honest accounting: full prompt/completion split recorded; split-unknown attempts record total + `split_known=0`, cost 0 (never invented) | ✅ | ✅ |
| 23 | Semantic cache: real local implementation (deterministic hashed bag-of-words, 256-dim, cosine ≥ 0.85, LRU + TTL) — explicitly NOT a neural embedding model | ✅ | ✅ |
| 24 | Scoring: cost/latency/quality routing — 5 modes (Balanced/Fast/Cheap/Quality/Reliable), weights sum to 1, unknown quality never outranks known-good | ✅ | ✅ |
| 25 | Scoring wired into the engine: `OrderHook` (ModeBalanced) fed by health-probe latency/success + registry quota hints; cost/quality stay unknown (0) until prices/quality signals exist | ✅ | ✅ |
| 26 | Eval harness: sequential runner, per-case timeout, substring + latency checks only (does NOT judge quality), panic-safe; `eval` subcommand runs smoke cases against the live engine | ✅ | ✅ |
| 27 | Observability: hand-rolled Prometheus exposition (`/metrics`, gateway-key auth required), request/latency/in-flight/failover counters, JSON trace log (opt-in via `CODVIAN_ROUTER_TRACE=1`, honestly NOT the OTel SDK) | ✅ | ✅ |
| 28 | Health / quota intelligence: key probe (5 s timeout), never invents quota values, quota-type errors set hint 0; watcher probes all providers at startup + every 60 s | ✅ | ✅ |
| 29 | Graceful degradation: stale exact-match response served when every provider fails, only when enabled, marked `X-Codvian-Stale` (+ age) on non-streaming responses and logged for streams; never replays after partial delivery | ✅ | ✅ |
| 30 | Degradation preserves mid-stream resume: `StaleEngine` implements `server.StreamResumer` (delegates when the inner engine resumes, else replays with rune-accurate trimming) and forwards `LastTrace` for metrics | ✅ | ✅ |
| 31 | Stable versioned config: unknown top-level fields survive Load→Save, `ValidateFile` rejects duplicates/bad values naming the field, `Clone()` deep-copies; `validate` subcommand | ✅ | ✅ |
| 32 | Headless tray supervisor: child = gateway binary, own process group, one auto-restart after unexpected exit, no zombies; `tray` subcommand (`RunHeadless`). GUI tray icon honestly DEFERRED (systray needs GTK dev headers absent on this VM) | ✅ | ✅ |
| 33 | Packaging: multi-stage Dockerfile (distroless nonroot), docker-compose, npm wrapper, `.goreleaser.yml`, checksum-verified `install.sh` (refuses pipe-to-shell); `--listen` opt-in for non-loopback bind (127.0.0.1 / ::1 / 0.0.0.0 only, 0.0.0.0 warns) | ✅ | ✅ |
| 34 | Route trace moved to leaf package `internal/routetrace` (no internal imports) so server metrics + degrade forwarding cannot create an import cycle | ✅ | ✅ |
| 35 | Phase 4 docs: INSTALL, DEMO, ARCHITECTURE, CHANGELOG, RELEASE-v0.2.0 draft | ✅ | ✅ |
| 36 | Dashboard wired into the server: GET /dashboard serves the approved dashboard (web/index.html, embedded via //go:embed in web/web.go); GET-only (405 otherwise), no auth on the static dashboard, /v1/* and /metrics stay auth-gated; startup line prints the dashboard URL; NO Playground section anywhere | ✅ | ✅ |
| 37 | Catalog wired into the provider registry: `Registry.AddCatalog` / `AddCatalogWithOptions` add a provider by catalog ID (or alias) — base URL, wire format and auth scheme come from the catalog entry; only the ID + the operator's own key are needed. `provider add-catalog` CLI subcommand (with `--endpoint` / `--variant` / `--base-url` override) + `provider list` / `provider add` | ✅ | ✅ |
| 38 | Catalog auth in the request path: `ClientFor` resolves credentials per (provider, endpoint) via `catalog.ResolveAuth` — Anthropic messages surface sends Bearer + `anthropic-version: 2026-10-21`, DeepSeek's Anthropic surface sends `x-api-key` (Bearer on its chat surface), Fireworks alt-header sends `X-Fireworks-Api-Key`, Azure api-key variant sends `api-key`. Hand-added providers are matched by base URL when the catalog knows it (`ResolveAuthForBaseURL`, endpoint bases matched first); unknown URLs keep the legacy Bearer default. Exchange surfaces (Vertex service-account JSON, Bedrock SigV4, OpenRouter JWT, Azure Entra) fail loudly with a clear error — never a faked static header | ✅ | ✅ |
| 39 | Catalog policy enforced: ToS-grey entries (web-cookie / subscription-OAuth, e.g. Claude Code, ChatGPT Web) are refused as routing targets at add time and at config load; entries with no known base URL are refused; templated catalog base URLs (Azure `{resource}`, Vertex `{LOCATION}`) require a concrete `--base-url` override. Verification status (`verified-from-docs` / `from-reference-metadata-only` / `unverified`) is stored on the provider and printed at startup per provider — never hidden | ✅ | ✅ |
| 40 | Catalog-linked providers in the config file: `catalog_id` (+ `auth_endpoint` / `auth_variant`) persisted in `ProviderConfig`; an empty `base_url` is allowed when `catalog_id` is set and resolves from the catalog at registry load; `validate` accepts it | ✅ | ✅ |
| 41 | Anthropic native wire in the request path: `internal/wire` (OpenAI↔Anthropic Messages conversion, SSE translation) wired into `provider`/`router`/`server` — `Message`/`ChatRequest` carry content parts, tool calls, tools, `tool_choice`, `top_p`, `stop` (string-or-array); `ChatResponse` carries tool calls + real finish reason; streaming is chunk-aware (`ChunkStreamer`, `Engine.ChatStreamChunks`) instead of `onDelta(string)` + hard-coded "stop" | ✅ | ✅ |
| 42 | Anthropic client: `POST {base}/v1/messages` with catalog-resolved auth headers, `max_tokens` default 4096, system message extracted; error mapping 429/rate_limit_error → `RateLimitError` (Retry-After honoured), billing → `QuotaError`, 529 overloaded → retryable `UpstreamError`, unrepresentable input (e.g. temperature > 1) → non-retryable error; mid-stream SSE error events mapped the same way. Unknown wire formats fall back to the OpenAI client (Gemini/Cohere/Bedrock native transports not built yet) | ✅ | ✅ |
| 43 | Cross-wire failover: a native-wire provider failing (429) fails over to an OpenAI provider and vice versa — the error type, not the wire format, decides; router `StreamEvent` carries tool-call deltas + finish reason, `MergeToolCallDeltas` reassembles streamed tool calls | ✅ | ✅ |
| 44 | Degrade wrapper is chunk-aware: `StaleEngine` implements `ChatStreamChunks`/`ChatStreamFromChunks` with the same stale rules as the text path (store on success, replay only before any chunk, never after partial delivery, text-only inner engines bridged to chunks). Without this the server's chunk path was dead code in serve mode — the first live smoke caught it (streaming tool-call deltas never reached the client) | ✅ | ✅ |
| 45 | Anthropic stream tool-call indices are renumbered sequentially from 0 in `wire.TranslateStream` (Anthropic block indices count text blocks too, so a tool_use after a text block is block 1 — emitting that as the OpenAI tool-call index would wrongly claim a second tool call exists) | ✅ | ✅ |
| 46 | Gemini native wire in the request path: `internal/wire` (OpenAI↔Gemini generateContent conversion, SSE translation) wired into `provider`/`registry` the same way Anthropic is — `POST {base}/models/{model}:generateContent` (+ `:streamGenerateContent?alt=sse` for streaming) with catalog-resolved `x-goog-api-key` auth, no `Authorization` header, no `?key=` query parameter; system/developer → `systemInstruction`, assistant → `model` role, tool results → `function` role (function name matched from the earlier assistant call by `tool_call_id`); consecutive same-role contents merged; first content must be `user` | ✅ | ✅ |
| 47 | Gemini client: `ListModels` parses the real `{"models":[{"name":"models/..."}]}` shape and strips the `models/` prefix; error mapping 429/`RESOURCE_EXHAUSTED` → `RateLimitError` (Retry-After honoured), 403 quota / 402 → `QuotaError`, 503/`UNAVAILABLE` → retryable `UpstreamError`, mid-stream SSE error events mapped the same way; blocked prompts (`promptFeedback.blockReason`) convert to an empty reply with `finish_reason: content_filter`, not an error; `SAFETY`/`RECITATION` → `content_filter`, `TOOL_CALLS` → `tool_calls` | ✅ | ✅ |
| 48 | Cross-wire failover with Gemini: a Gemini 429 fails over to an OpenAI provider and an OpenAI 429 fails over to a Gemini provider — proven live (see the Gemini integration gate below) and by unit test with a real Gemini client (`TestEngineCrossWireFailoverGeminiToOpenAI`) | ✅ | ✅ |

## Anthropic native-wire integration gate (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 17 packages ok, 381 passing tests, 0 fail** (was 355 before this work; +26 new tests: Anthropic client request shape/auth/tool calls/stream/errors, Message/ChatRequest JSON round-trips, cross-wire failover both directions, chunk streaming with tool calls, server tool-call passthrough non-streaming + streaming, degrade chunk forwarding/stale replay/resume trim, server-through-degrade-wrapper regression test, wire tool-call index renumbering)
- `go test -count=1 -race` on touched packages (provider, router, server, degrade, health, wire) — **clean**
- Live smoke (real binary, isolated HOME, Python mocks on 21021 Anthropic / 21022 OpenAI, router on 20199, 2026-11-17):
  - **A1 non-stream Anthropic:** mock received `POST /v1/messages` with `Authorization: Bearer <redacted>` + `Anthropic-Version: 2026-10-21`, body `{model: claude-sonnet-4-5, max_tokens: 4096, messages: [{role: user, content: [{type: text, text: "Hi"}]}], system: "Be helpful."}`; router returned content "live smoke from anthropic", `tool_calls: [{get_weather, {"city": "Pune"}}]`, `finish_reason: "tool_calls"`, usage 12/9/21
  - **A2 stream Anthropic:** SSE chunks `live `, `smoke`, tool-call head delta `{index: 0, id: toolu_9, name: get_weather}`, arguments delta `{"city":"Pune"}`, finish chunk `finish_reason: "tool_calls"`, then `[DONE]` — tool-call deltas reach the client (this initially FAILED: the degrade wrapper lacked the chunk methods, so the server silently fell back to text-only; fixed in `internal/degrade/degrade.go` and re-verified)
  - **B Anthropic 429 → OpenAI:** Anthropic mock forced to 429 `rate_limit_error`; router answered from the OpenAI mock (`POST /v1/chat/completions`, "live smoke from openai", finish_reason stop)
  - **C OpenAI 429 → Anthropic:** reverse direction; router answered via `POST /v1/messages` with tool_calls + `finish_reason: tool_calls`
- Smoke scripts kept at `/tmp/cr-smoke/` (mocks.py, smoke.sh) — ephemeral, not in the repo

## Gemini native-wire integration gate (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 17 packages ok, 418 PASS events (350 top-level tests + 68 subtests), 0 fail** (was 381 by the earlier counting method; +37 new: 20 wire Gemini conversion/error/stream tests, 10 Gemini client tests incl. request shape/auth/stream/errors/ListModels, 1 registry catalog test, 2 router cross-wire subtests, plus the renamed wire-fallback test now using Cohere). Note: the earlier "381" figure counts tests by a different method than the `--- PASS` lines used here, so the two totals are not directly comparable — what matters is 0 failures in both.
- `go test -count=1 -race` on touched packages (provider, wire, router) — **clean**
- Live smoke (real binary, isolated HOME, Python mocks on 21031 Gemini / 21032 OpenAI, router on 20199, 2026-11-17):
  - **G1 non-stream Gemini:** mock received `POST /models/gemini-2.0-flash:generateContent` with `X-Goog-Api-Key: gem-smoke-key` and NO `Authorization` header, body `{contents: [{role: user, parts: [{text: "Hi"}]}], systemInstruction: {parts: [{text: "Be helpful."}]}}`; router returned content "live smoke from gemini", `tool_calls: [{get_weather, {"city": "Pune"}}]`, `finish_reason: "tool_calls"`, usage 12/9/21
  - **G2 stream Gemini:** SSE chunks `live `, `smoke`, tool-call head delta `{index: 0, id: call_0, name: get_weather}`, arguments delta `{"city":"Pune"}`, finish chunk `finish_reason: "tool_calls"`, then `[DONE]`
  - **G3 Gemini 429 → OpenAI:** Gemini mock forced to 429 `RESOURCE_EXHAUSTED`; router answered from the OpenAI mock (`POST /v1/chat/completions`, "live smoke from openai", finish_reason stop)
  - **G4 OpenAI 429 → Gemini:** reverse direction; router answered via `POST /models/gemini-2.0-flash:generateContent` with tool_calls + `finish_reason: tool_calls`
  - Startup line: `provider "Google Gemini" catalog gemini verification verified-from-docs wire gemini auth generate`
- Smoke scripts kept at `/tmp/cr-smoke-gemini/` (mocks.py, smoke.sh) — ephemeral, not in the repo

## Test gate results — catalog wiring (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — exit 0; `go vet ./...` — clean; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **295/295 PASS, 0 fail** (was 224; +71 incl. 13 new provider catalog tests, 1 new config test, 1 new cmd test, 1 extended catalog test; catalog itself still 22 tests, all green)
- `go test -race` — clean on all touched packages (provider, catalog, config, health, cmd/codvian-router)

## Live smoke — catalog wiring (real binary, Python mock upstreams, 2026-11-17, run by coordinator)

Isolated HOME, fresh config via `init`; mocks on 127.0.0.1:21011 (standing in for Fireworks) and 127.0.0.1:21012 (standing in for Anthropic); router on 127.0.0.1:20199, auth ON.

| Scenario | Result |
|---|---|
| `provider add-catalog fireworks … --endpoint chat --variant alt-header --base-url <mock>` | **exit 0** — `catalog: fireworks (verification: verified-from-docs)`, wire openai, 2 models fetched |
| `provider add-catalog anthropic … --endpoint messages --base-url <mock>` | **exit 0** — `catalog: anthropic (verification: verified-from-docs)`, wire anthropic, 2 models fetched |
| `provider add-catalog vertex …` (exchange auth) | **exit 1** — `credential exchange required … service-account-json … credential exchange is not supported yet` |
| Startup log (stderr) | `provider "Fireworks" catalog fireworks verification verified-from-docs wire openai auth chat (variant alt-header)` and `provider "Anthropic" catalog anthropic verification verified-from-docs wire anthropic auth messages` |
| `POST /v1/chat/completions` model `fw-model` | **200, "fireworks says hi"**; mock recorded exactly `X-Fireworks-Api-Key: fw-smoke-key` and NO `Authorization` header |
| `POST /v1/chat/completions` model `an-model` | **200, "anthropic says hi"**; mock recorded exactly `Authorization: Bearer sk-ant-smoke` + `Anthropic-Version: 2026-10-21` |
| Streaming `an-model` (`stream: true`) | SSE deltas + `[DONE]`, content "anthropic says hi" |
| `GET /v1/models` with gateway key | 3 models with correct owners (Fireworks ×2, Anthropic ×1); without key → **401** |
| `provider list` / `validate` | List shows catalog + verification + auth endpoint per provider; config OK |

Notes: the mocks stand in for the catalog entries' base URLs via the `--base-url` override (the real upstream hosts were not contacted). The two header-less `/v1/models` hits in the mock logs are the smoke script's own unauthenticated `curl` health checks, not router traffic — every router request carried the resolved headers. `ClientFor` exchange refusal for a stored Vertex provider is covered by unit test (`TestRegistryClientForCatalogExchangeError`); the CLI refusal above is the same error through the real binary.

## Test gate results (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — exit 0; `go vet ./...` — clean; `gofmt -l .` — clean
- `go test -count=1 ./...` — **224/224 PASS, 0 fail** (cache 21, config 21, crypto 15, degrade 23, eval 9, health 16, ledger 12, metrics 8, provider 29, router 26, routetrace 2, scoring 11, server 21, tray 7, cmd/codvian-router 3)
- `go test -race` — clean on all touched/new packages (cmd, degrade, server, metrics, health, scoring, eval)
- 8,634 lines non-test Go + 6,474 lines of tests, 38 Go files
- Binary: 17.3 MB single binary

## Live benchmark — Phase 2–4 integration gate (real binary, Python mock upstreams, 2026-11-17, run by coordinator)

Same setup as Phase 1 (Mock A 21001 + Mock B 21002 answering in 50 ms,
router on 127.0.0.1:20199, auth ON), plus the new wiring checks. Mocks were
made content-aware in the scratch harness only (capital question → "Paris…",
greeting → "Hello!…", else "ok") so the eval smoke cases are meaningful.

| Scenario | Result |
|---|---|
| A returns 429 instantly → failover to B (5 reqs) | **5/5 OK, 52.4–55.0 ms** |
| Failover switch cost | **~1–2 ms overhead** vs direct B |
| A slow (2,000 ms) → hedge at 150 ms, B wins (5 reqs) | **5/5 OK, 203.8–205.9 ms** |
| A quota hint 0 → A skipped entirely, B serves (5 reqs) | **5/5 OK, 52.4–52.8 ms** |
| `/metrics` after the failover run (auth required; no key → 401) | `codvian_router_requests_total{provider="prov_mock_b",outcome="ok"} 5`, `codvian_router_failovers_total{from_provider="prov_mock_a",to_provider="prov_mock_b"} 5`, `codvian_router_in_flight 0` |
| Stale degradation: fresh request, then both mocks killed | Same prompt → **200 with `X-Codvian-Stale: true`, age ~519 ms**; fresh prompt → **502**; fresh requests carry no stale header |
| `eval --model bench-model` (smoke cases vs live mocks) | **3/3 passed (100%)**, exit 0 |
| `validate` subcommand | Valid config → exit 0; invalid listen address → exit 1 with the field named |
| `--listen 192.168.1.5:20199` | Refused, exit 1, names `--listen`; `--listen 0.0.0.0:20198` serves + prints the all-interfaces warning |
| `tray` subcommand | Child serves healthz; `kill -9` on the child → auto-restarted, healthz OK again; SIGTERM on the supervisor → exit 0, child down |

Ledger verified against the SQLite file after the stale run: 2 successes
record prompt 10 / completion 5 / total 15 with `split_known=1`, cost $0;
the 2 post-kill requests each record 2 failed attempts (A then B) with
0/0/0, `split_known=0`, cost $0 — the stale response itself is served from
cache and adds no ledger row, which is honest: no provider was billed.

## Test gate results — Phase 1 (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — exit 0; `go vet ./...` — clean; `gofmt -l .` — clean
- `go test -count=1 ./...` — **133/133 PASS, 0 fail** (cache 21, config 16, crypto 15, ledger 12, provider 29, router 26, server 14)
- 5,629 lines non-test Go + 3,733 lines of tests, 21 Go files
- Binary: 16.9 MB single binary

## Live benchmark (real binary, Python mock upstreams, 2026-11-17, re-run after hardening)

Setup: Mock A (port 21001) + Mock B (port 21002), both serving `bench-model`;
B answers in 50ms. Router on 127.0.0.1:20199, auth ON. Config built with the
real Vault (Argon2id) sealing the mock keys; gateway key issued by the
router's own config code.

| Scenario | Result |
|---|---|
| Direct to Mock B (no router, 50ms upstream delay) | 51.2 ms (baseline floor, previous run) |
| A returns 429 instantly → failover to B (5 reqs) | **5/5 OK, 52.4–55.8 ms** |
| Failover switch cost | **~1–2 ms overhead** vs direct B |
| A slow (2,000 ms) → hedge at 150 ms, B wins (5 reqs) | **5/5 OK, 203.9–204.4 ms** (150 ms hedge + 51 ms B) |
| A quota hint 0 → A skipped entirely, B serves (5 reqs) | **5/5 OK, 52.2–53.5 ms** (quota wiring proven end-to-end) |
| Streaming with 429 failover | SSE chunks `bench ` + `ok` + `stop` + `[DONE]` — works (previous run) |
| No gateway key | 401 as expected |
| Unknown model | Clean JSON error, no crash |
| Startup to healthz-ready | **73 ms** (this run, 50 ms poll granularity; previous run 12 ms) |
| Idle memory (RSS) | **~32 MB** (this run; previous run ~10.6 MB — the rise is the Argon2id 19 MiB arena plus runtime; still tens of MB) |
| Binary size | 16.9 MB single binary |

Ledger verified against the SQLite file after the run: a failed A attempt
records prompt 0 / completion 0 / total 0 with `split_known=0`; a successful
B attempt records prompt 10 / completion 5 / total 15 with `split_known=1`
and cost $0 (no per-model prices configured — never invented). A hedged
loser is recorded as a failed attempt with zero tokens; a quota-skipped
provider is never attempted and leaves no row.

Targets (design goals, now measured against): detect failure 2–3 s, switch
0.1–0.3 s, route under 1 s — measured switch after a 429 is ~1–2 ms on
localhost mocks (real-world numbers will depend on upstream latency).

## Known limitations / follow-ups (not blockers)

- Crypto: master secret is still a local 0600 file — Argon2id only slows
  an attacker who has sealed values but not the secret. Rotation is manual:
  `Vault.ReSeal` each sealed value before replacing `master.key` (or
  changing the passphrase); losing `kdf.salt` is equally fatal. No
  passphrase by default and nothing prompts for one yet; a lost passphrase
  is unrecoverable by design.
- Ledger: streaming usage is unknown to the engine (`bridgeStream`
  discards the upstream `ChatResponse`, which can carry usage from a final
  SSE chunk), so successful streams record zero tokens with cost 0 —
  documented at the call site, never invented. Cost is 0 when no
  per-model prices are configured.
- Registry quota hints are wired into the engine (live read on every
  request) and now also auto-derived in one direction: the health watcher
  sets hint 0 when a key probe fails with a quota-type error (429 /
  `QuotaError`). Hints are never auto-raised and never invented from a
  successful probe; a quota reset is not detected until the next probe
  succeeds and the operator (or a future reset signal) restores the hint.
- Semantic cache is a deterministic hashed bag-of-words similarity cache,
  NOT a neural embedding model: paraphrases with different words will
  generally miss. It is not yet wired into the server/engine request path
  (still open).
- Scoring cost/quality signals are unknown (0) until per-model prices are
  configured and a quality signal exists, so ModeCheap/ModeQuality
  currently rank mostly on the other signals; cost and quality components
  score equally for every candidate and cannot distort ordering.
- Metrics attribution is approximate under concurrency: the server reads
  the engine's most recent RouteTrace, so a request may occasionally be
  attributed to a neighbour's trace. The ledger (not metrics) is the
  billing record. A streaming response served stale carries no
  `X-Codvian-Stale` header (staleness is known only after the stream
  ends); it is logged server-side instead.
- Packaging: the Docker image was NOT built or run in this environment
  (no Docker in the VM) and no release exists yet, so the npm wrapper and
  `install.sh` happy paths are untested (error paths only). The Dockerfile
  binds loopback by default; use `--listen 0.0.0.0` or
  `network_mode: host` deliberately, on a trusted network only.
- `go.mod` was concurrently edited by parallel agents mid-build; it has converged
  (`github.com/raunakiitp/codvian-router`, `go 1.22`, modernc.org/sqlite v1.34.0, golang.org/x/crypto v0.24.0).
- Catalog wiring (2026-11-17): native wire transports for Cohere / Bedrock are NOT implemented — the client speaks OpenAI wire, Anthropic Messages wire, and Gemini generateContent wire. Catalog auth resolves correctly per (provider, endpoint) for every provider, but a Cohere/Bedrock-wire provider will fail at the upstream (404/400) and fail over until its native wire support lands. The Anthropic wire IS now proven live (see the Anthropic integration gate above): the mock spoke real Anthropic Messages and the router answered with correct tool calls + finish reason. The Gemini wire is likewise proven live (see the Gemini integration gate above).
- Catalog wiring: DeepSeek's chat path has NO `/v1` per the catalog note, but the client always appends `/v1` — per-endpoint path support is still open. The catalog note itself conflicts with DeepSeek's own docs (which use base `https://api.deepseek.com/v1`); verify against DeepSeek docs before changing either side.
- Catalog wiring: exchange auth (Vertex service-account JSON, Bedrock SigV4, OpenRouter JWT, Azure Entra) is refused loudly, not implemented. ToS-grey providers are refused as routing targets. Dashboard surfacing of per-provider verification status is a later step (startup log + `provider list` cover it for now).
- Anthropic wire (2026-11-17): Cohere / Bedrock native transports are NOT built — a catalog provider with one of those wire formats falls back to the OpenAI client and will fail at the upstream and fail over until its native transport lands. (Gemini now has its own transport; see items 46–48 and the Gemini integration gate.) Exchange auth still refused loudly. Tool-call deltas are NOT replayed across a mid-stream resume — only the text prefix is continued (documented in the router); the returned response still carries the full reassembled tool calls. A stale stream replay carries text only: the exact-match cache stores content, not tool calls, so a stale stream cannot reconstruct the original tool calls. DeepSeek `/v1` path question still open (catalog note says no `/v1`, DeepSeek's own docs use base `https://api.deepseek.com/v1` — verify before changing either side).
- Catalog wiring: `ResolveAuthForBaseURL` scans all 423 catalog entries per `ClientFor` call (no cache). Fine at current rates (startup + 60 s health probes); revisit if `ClientFor` gets hot.

## Dashboard wiring gate (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 15 packages ok** (server 24 tests incl. 3 new dashboard tests: `TestDashboard`, `TestDashboardMethodNotAllowed`, `TestDashboardDoesNotBypassAPIAuth`)
- Live smoke (real binary, isolated HOME, fresh config, port 20199): startup prints `dashboard: http://127.0.0.1:20199/dashboard`; `GET /dashboard` → **200**, `text/html; charset=utf-8`, 113,427 B, "Codvian Router"/"Overview"/"Providers"/"Models" present, "Playground" absent; `GET /healthz` → 200; `GET /v1/models` + `GET /metrics` without key → 401; `POST /dashboard` → 405; `--listen 192.168.1.5` still refused
- Note: the dashboard loads Google Fonts from fonts.googleapis.com (external); offline it falls back to system fonts — same as the approved artifact.
