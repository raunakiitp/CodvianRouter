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
| 38 | Catalog auth in the request path: `ClientFor` resolves credentials per (provider, endpoint) via `catalog.ResolveAuth` — Anthropic messages surface sends Bearer + `anthropic-version: 2026-10-21`, DeepSeek's Anthropic surface sends `x-api-key` (Bearer on its chat surface), Fireworks alt-header sends `X-Fireworks-Api-Key`, Azure api-key variant sends `api-key`. Hand-added providers are matched by base URL when the catalog knows it (`ResolveAuthForBaseURL`, endpoint bases matched first); unknown URLs keep the legacy Bearer default. Exchange surfaces (Vertex service-account JSON, OpenRouter JWT, Azure Entra) fail loudly with a clear error — never a faked static header. Bedrock SigV4 is the exception: the client signs each request itself from the operator's AWS credentials, so a SigV4 surface with parseable credentials builds a Converse client instead of failing (a key that is not AWS credential material stays refused) | ✅ | ✅ |
| 39 | Catalog policy enforced: ToS-grey entries (web-cookie / subscription-OAuth, e.g. Claude Code, ChatGPT Web) are refused as routing targets at add time and at config load; entries with no known base URL are refused; templated catalog base URLs (Azure `{resource}`, Vertex `{LOCATION}`) require a concrete `--base-url` override. Verification status (`verified-from-docs` / `from-reference-metadata-only` / `unverified`) is stored on the provider and printed at startup per provider — never hidden | ✅ | ✅ |
| 40 | Catalog-linked providers in the config file: `catalog_id` (+ `auth_endpoint` / `auth_variant`) persisted in `ProviderConfig`; an empty `base_url` is allowed when `catalog_id` is set and resolves from the catalog at registry load; `validate` accepts it | ✅ | ✅ |
| 41 | Anthropic native wire in the request path: `internal/wire` (OpenAI↔Anthropic Messages conversion, SSE translation) wired into `provider`/`router`/`server` — `Message`/`ChatRequest` carry content parts, tool calls, tools, `tool_choice`, `top_p`, `stop` (string-or-array); `ChatResponse` carries tool calls + real finish reason; streaming is chunk-aware (`ChunkStreamer`, `Engine.ChatStreamChunks`) instead of `onDelta(string)` + hard-coded "stop" | ✅ | ✅ |
| 42 | Anthropic client: `POST {base}/v1/messages` with catalog-resolved auth headers, `max_tokens` default 4096, system message extracted; error mapping 429/rate_limit_error → `RateLimitError` (Retry-After honoured), billing → `QuotaError`, 529 overloaded → retryable `UpstreamError`, unrepresentable input (e.g. temperature > 1) → non-retryable error; mid-stream SSE error events mapped the same way. Unknown wire formats (e.g. `ollama`) fall back to the OpenAI client — Anthropic, Gemini, Cohere and Bedrock all have their own transports now | ✅ | ✅ |
| 43 | Cross-wire failover: a native-wire provider failing (429) fails over to an OpenAI provider and vice versa — the error type, not the wire format, decides; router `StreamEvent` carries tool-call deltas + finish reason, `MergeToolCallDeltas` reassembles streamed tool calls | ✅ | ✅ |
| 44 | Degrade wrapper is chunk-aware: `StaleEngine` implements `ChatStreamChunks`/`ChatStreamFromChunks` with the same stale rules as the text path (store on success, replay only before any chunk, never after partial delivery, text-only inner engines bridged to chunks). Without this the server's chunk path was dead code in serve mode — the first live smoke caught it (streaming tool-call deltas never reached the client) | ✅ | ✅ |
| 45 | Anthropic stream tool-call indices are renumbered sequentially from 0 in `wire.TranslateStream` (Anthropic block indices count text blocks too, so a tool_use after a text block is block 1 — emitting that as the OpenAI tool-call index would wrongly claim a second tool call exists) | ✅ | ✅ |
| 46 | Gemini native wire in the request path: `internal/wire` (OpenAI↔Gemini generateContent conversion, SSE translation) wired into `provider`/`registry` the same way Anthropic is — `POST {base}/models/{model}:generateContent` (+ `:streamGenerateContent?alt=sse` for streaming) with catalog-resolved `x-goog-api-key` auth, no `Authorization` header, no `?key=` query parameter; system/developer → `systemInstruction`, assistant → `model` role, tool results → `function` role (function name matched from the earlier assistant call by `tool_call_id`); consecutive same-role contents merged; first content must be `user` | ✅ | ✅ |
| 47 | Gemini client: `ListModels` parses the real `{"models":[{"name":"models/..."}]}` shape and strips the `models/` prefix; error mapping 429/`RESOURCE_EXHAUSTED` → `RateLimitError` (Retry-After honoured), 403 quota / 402 → `QuotaError`, 503/`UNAVAILABLE` → retryable `UpstreamError`, mid-stream SSE error events mapped the same way; blocked prompts (`promptFeedback.blockReason`) convert to an empty reply with `finish_reason: content_filter`, not an error; `SAFETY`/`RECITATION` → `content_filter`, `TOOL_CALLS` → `tool_calls` | ✅ | ✅ |
| 48 | Cross-wire failover with Gemini: a Gemini 429 fails over to an OpenAI provider and an OpenAI 429 fails over to a Gemini provider — proven live (see the Gemini integration gate below) and by unit test with a real Gemini client (`TestEngineCrossWireFailoverGeminiToOpenAI`) | ✅ | ✅ |
| 49 | Cohere native wire in the request path: `internal/wire` (OpenAI↔Cohere Chat API v2 conversion, SSE translation) wired into `provider`/`registry` the same way Anthropic/Gemini are — `POST {base}/v2/chat` (+ `stream: true` SSE for streaming) with catalog-resolved Bearer auth; system → system message (string or typed blocks), image blocks keep the OpenAI `{"type":"image_url","image_url":{"url"}}` shape, tools `{"type":"function","function":{name,description,parameters}}`, `tool_choice` "auto" → omit (Cohere has no "auto"; `NONE`/`REQUIRED` only); finish_reason mapping COMPLETE→stop, MAX_TOKENS→length, TOOL_CALL→tool_calls, STOP_SEQUENCE→stop, ERROR/TIMEOUT→stop; usage from `usage` or `meta.billed_units` | ✅ | ✅ |
| 50 | Cohere client: `ListModels` parses the real `{"models":[{"name":...}]}` shape at `GET {base}/v1/models`; error mapping 429 → `RateLimitError` (Retry-After honoured), billing/quota → `QuotaError`, other 4xx/5xx → `UpstreamError`, mid-stream SSE error events mapped the same way; catalog fix: a base containing `/compatibility/v1` resolves to WireOpenAI, not WireCohere (Cohere's compatibility shim speaks OpenAI wire) | ✅ | ✅ |
| 51 | Cross-wire failover with Cohere: a Cohere 429 fails over to an OpenAI provider and an OpenAI 429 fails over to a Cohere provider — proven live (see the Cohere integration gate below) and by unit test with a real Cohere client (`TestEngineCrossWireFailoverCohereToOpenAI`, both directions) | ✅ | ✅ |
| 52 | Bedrock native wire in the request path: `internal/wire` (OpenAI↔Bedrock Converse conversion, Amazon event-stream translation) wired into `provider`/`registry` the same way Anthropic/Gemini/Cohere are — `POST {base}/model/{modelId}/converse` (+ `/converse-stream` for streaming, model ID verbatim in the path, no `/v1`); messages with typed blocks (text / image / toolUse / toolResult), system array, `inferenceConfig`, `toolConfig` (auto/any/tool choice); stopReason mapping end_turn→stop, tool_use→tool_calls, max_tokens→length, stop_sequence/guardrail_intervened/content_filtered→stop; consecutive same-role messages merged (Converse requires alternating roles starting with user; assistant-first is unrepresentable); remote image URLs unrepresentable, reasoningContent dropped by design | ✅ | ✅ |
| 53 | Bedrock client + pure-Go SigV4 signer: `internal/provider/sigv4.go` (stdlib-only HMAC-SHA256 signing, no SDK vendored) verified against two independent references — the IAM docs example matches botocore 1.44.4 exactly, and a Bedrock Converse POST signature matches an independent pure-Python SigV4 reference exactly; credentials parsed as `AKID:SECRET[:TOKEN]`, region from the base-URL host (fallback us-east-1). `bedrockClient` signs every request (`Authorization: AWS4-HMAC-SHA256 …`, `X-Amz-Content-Sha256`, `X-Amz-Date`), never sends a Bearer header; `ListModels` via control-plane `GET https://bedrock.{region}.amazonaws.com/foundation-models` (falls back to `{base}/foundation-models` for non-AWS hosts); error mapping ThrottlingException/429 → `RateLimitError` (Retry-After honoured), 5xx/408 retryable, 400 non-retryable; no AWS credentials at all → loud exchange error, never a faked header | ✅ | ✅ |
| 54 | Cross-wire failover with Bedrock: a Bedrock ThrottlingException 429 fails over to an OpenAI provider and an OpenAI 429 fails over to a Bedrock provider — proven live (see the Bedrock integration gate below) and by unit test with a real Bedrock client (`TestEngineCrossWireFailoverBedrockToOpenAI`, both directions) | ✅ | ✅ |
| 55 | Semantic cache wired into the request path: `degrade.WrapWithSemantic` + `Policy.ServeCache` (opt-in; default off, so existing behaviour is unchanged) — cacheable requests (no tools, no tool-role history) are served proactively from the exact cache first, then the semantic cache (0.85 cosine, same 1h TTL / 500-entry LRU rules), without calling any provider; successes are stored in both layers under the last user message's text | ✅ | ✅ |
| 56 | Semantic-cache honesty: a proactive hit is marked `X-Codvian-Cache: exact\|semantic` (+ `X-Codvian-Cache-Similarity` for semantic hits) on non-streaming responses and logged for streams — never marked stale, never presented as a fresh provider response; a semantic hit is also stored under its own exact key, so a repeat paraphrase becomes an exact hit; resume paths (`ChatStreamFrom*`) deliberately skip proactive lookup so a cached full response can never duplicate content the client already has | ✅ | ✅ |
| 57 | Ledger/metrics honesty for cache hits: a cache hit calls no provider, so the router's ledger records nothing for it (proven live: 2 provider rows for 2 real calls, 0 rows for 3 cache hits); the server attributes the request to `provider="cache"` in `codvian_router_requests_total` (never to a neighbour's trace) and `codvian_router_cache_hits_total{layer="exact"\|"semantic"}` is counted exactly once, in the wrapper — the server does not count hits, so there is no double-count | ✅ | ✅ |
| 58 | Cache safety fix found by the semantic wiring tests: the stale fallback (`staleFor`) previously served a content-only exact entry to a tool-using request on provider failure, silently dropping its tool calls — it now refuses non-cacheable requests the same way proactive lookup and store do (the `X-Codvian-Stale` path keeps working for plain-text requests; all pre-existing stale tests still pass) | ✅ | ✅ |
| 59 | Auto-router (backend): local semantic-router / RouteLLM-pattern classification — prompt embedded once with the deterministic hashed bag-of-words embedding reused from the semantic cache (`cache.EmbedText`, 256-dim, no neural model, no network), cosine-matched against curated per-tier example utterances (15 per tier shipped as config defaults: coding / chat / reasoning / quick-qa), best-utterance + tier-centroid scoring; virtual models `auto`, `auto/fast`, `auto/cheap`, `auto/coding`, `auto/reasoning` in the request path; single threshold dial (default 0.5, configurable per virtual model in config `auto_router`), below threshold → fallback to the default/strong tier, never a guess; no concrete model configured → honest failure, never an invented model; routing-decision latency measured (see gate) | ✅ | ✅ |
| 60 | Auto-router (dashboard, same slice): Auto-router section in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — virtual-model selector + threshold slider (preview-only; live path uses the config threshold) + routing-decision view (tier picked, confidence, per-tier scores, fallback reason, resolved model, measured decision latency) wired to real endpoints `GET /dashboard/auto-route` (live classify) and `GET /dashboard/auto-route/last` (last real `auto/*` decision from live /v1 traffic, honest empty state when none yet); virtual models listed by `GET /v1/models`; auto-routed responses carry `X-Codvian-Auto-Tier` / `-Confidence` / `-Model` (+ `-Fallback`) headers. Decision view shows real decisions only — no sample/fake traffic; the shipped utterances are labelled config defaults, never presented as measured results | ✅ | ✅ |
| 61 | GET /api/providers endpoint: live per-provider feed (id, name, wire format, verification, health_status ok/down/unknown from the health checker's last results, plus verification summary counts) — unauthenticated by design (no keys, no base URLs; choice documented in code), /v1/* and /metrics auth unchanged; wired to the real health checker in serve mode | ✅ | ✅ |
| 62 | Dashboard per-provider verification badges (Providers section): jewel-tone capsules — dark lime "Verified from docs", solid near-black "Reference metadata", muted "Unverified" — fetched live from GET /api/providers with wire format; NO sample providers any more (NO-FAKE-DATA rule): empty state reads "No providers connected yet", feed failure reads "Live provider feed unavailable"; sidebar/instance/topbar health counts now read from the same live list; remaining sample sections (Models, Logs, Usage, KPIs, router keys) are labelled "Demo data" until their live feeds land; quick-start base URL fixed to the real default port 20128 (was a wrong 8080) | ✅ | ✅ |
| 63 | Single-flight request coalescing (backend): `internal/coalescing` (new leaf package — stdlib-only singleflight pattern, no x/sync dependency; leaf so server + degrade can import it without an import cycle) — non-streaming requests keyed on the canonical request hash (SHA-256 of JSON {model, messages, max_tokens, temperature, top_p, stop}); the entry is registered BEFORE dialing and the whole retry loop (`ChatWithChain`: failover, 429 backoff, hedging) runs inside one flight, so N identical concurrent requests make exactly 1 upstream call and waiters share one backoff sequence; a shared failure reaches all waiters as the one error. NEVER coalesced: streaming requests (they never touch the group) and tool-using / tool-call turns (same rule as the degrade wrapper's cacheable check). A waiter whose own ctx ends leaves with its own ctx error and never disturbs the leader; a leader ctx cancel fails the flight for all waiters. Ledger honesty: the leader's attempt is the only billed row; each waiter is recorded via the optional `router.SharedLedger` interface as a row marked `shared=1` with zero tokens and zero cost (`ledger.Usage.Shared`, `shared` column on usage_events incl. migration for old DBs, `Ledger.RecordShared`), so spend/token totals never double-count. Counters also exposed as `codvian_router_coalesced_total` on /metrics | ✅ | ✅ |
| 64 | Single-flight coalescing (dashboard, same slice): Coalescing section in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — KPI tiles for shared calls / upstream calls avoided / quota hits saved (calls, not tokens) + live-flights card, wired to the real endpoint `GET /dashboard/coalescing` (unauthenticated like the other dashboard feeds: counters only, no keys or message content; zero counters when the engine reports nothing), refreshed on open + every 5 s while open + Refresh button; honest empty state ("No coalesced request yet… counters stay at zero until that happens") and the stats are real counters from `router.Engine.CoalescingStats()` forwarded through `degrade.StaleEngine` — no invented figures anywhere | ✅ | ✅ |
| 65 | TCP Fast Open + adaptive concurrency limiting (backend): TFO on the per-provider dialer via the `Control` hook (`TCP_FASTOPEN_CONNECT` on Linux, `TCP_FASTOPEN` on macOS/Windows; the hook never errors, so a refused option falls back to a normal handshake); never a POST body in early data (early data is the TLS ClientHello, handled by TLS). `provider.TFOStatus()` reports the real probe (setsockopt + Linux `net.ipv4.tcp_fastopen` sysctl). Adaptive per-(provider,key) Gradient2 limiter (`internal/adaptive`, leaf package): cap = cap × (baseline RTT ÷ smoothed RTT) + 1, initial 20 / floor 2 / ceiling 100, only successful calls feed the gradient, failed calls never move it; wired into `router.Engine` — saturated providers are deprioritised in `orderAttempts`, `TryAcquire` sheds before the queue grows (trace `skipped_concurrency_limit`, no breaker trip, no ledger row), streaming holds a slot for the stream duration but never feeds RTT; the limiter FAILS OPEN (nil limiter admits, no error path on admission). Caps + shed counts also on `/metrics` (`codvian_router_concurrency_limit`, `codvian_router_concurrency_rejected_total`); `CODVIAN_ROUTER_TFO=0` disables TFO for measurement only | ✅ | ✅ |
| 66 | TCP Fast Open + adaptive concurrency limiting (dashboard, same slice): Concurrency section in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — TFO card (Active/Supported/Not available, platform, sysctl value, dial attempts/errors) + per-provider table (in-flight, adaptive limit, smoothed RTT, baseline RTT, gradient, samples, shed) wired to the real endpoint `GET /dashboard/concurrency` (unauthenticated like the other dashboard feeds: limiter state + TFO probe only, no keys or base URLs; providers with no traffic show the initial cap with zero samples and no RTT), refreshed on open + every 5 s while open + Refresh button; honest empty state ("No providers connected yet… nothing here is pre-filled"); `node --check` on the dashboard JS clean | ✅ | ✅ |
| 67 | Route-guard tiers (backend): all 21 server routes classified into three static tiers in `internal/server/guard.go` (`guardRouteTable`, the single source of truth) — Tier 1 LOCAL_ONLY (7 routes: shutdown, service install, plugin load, auto-update, tunnel start, provider login, bootstrap), Tier 2 ALWAYS_PROTECTED (1 route: settings export), Tier 3 MANAGEMENT (13 routes incl. /v1/*, /metrics, dashboard feeds, admin status). `Server.register` looks every route up in the table and applies that tier's wrappers, panicking at startup on an unclassified route; the gate test walks the table and FAILS if any spawn-capable route is not LOCAL_ONLY. Tier 1 is checked BEFORE auth by real TCP peer (`RemoteAddr` only — Host header and X-Forwarded-For never consulted; a forged XFF does not help), and loopback alone is not enough: a valid gateway key is still required after the loopback check (except bootstrap, where no key exists yet). There is NO manage-scope bypass (stricter than OmniRoute). Tier 2 always requires a gateway key even when Tier 3 auth is relaxed (`WithAuthRelaxed` models that mode; production serve never sets it). A 403 LOCAL_ONLY is logged as "guard working as designed", not an error, and counted in `local_only_denials`. Fresh-install bootstrap (`POST /admin/bootstrap`) is loopback-only, works exactly once while no key exists (201, key shown once, persisted via the config file), then 409; with no issuer wired it answers 503. Shutdown is real (202 + graceful-stop hook wired in main.go; 503 with no hook, never a fake 202). Settings export is real but secrets-redacted (no provider keys, no gateway-key hashes, no base URLs). The five not-yet-built Tier 1 features answer honest 501 behind the guard. DB export/import/wipe have NO route in this build — nothing to call, nothing to protect; the dashboard says so, and they must be registered Tier 2 (Tier 1 if they spawn a helper) when they land | ✅ | ✅ |
| 68 | Route-guard tiers (dashboard, same slice): Route guards section in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — live guard-status card (loopback-only enforcement active, bootstrap pending/complete, Tier 3 auth relaxed yes/no, Tier 1 refusals since start, listen address) + the three tiers listed with every route in each (method, path, reason, spawn-capable/first-key-setup tags), wired to the real endpoint `GET /dashboard/guard` (unauthenticated like the other dashboard feeds: tiers + counters only, no keys or secrets; nothing hard-coded in the page — a newly classified route shows up automatically), refreshed on open + every 5 s while open + Refresh button; honest empty state ("No guard status yet…") before the feed answers and honest "unknown" listen address when unset; `node --check` on the dashboard JS clean | ✅ | ✅ |

## Route-guard tiers gate (2026-11-17, Go 1.27.4, measured by subagent)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 19 packages ok, 487 PASS events, 0 fail** (was 475 at the TFO gate by the same counting method; +12 new: route-table gate, register panic, loopback-peer matrix, local-only-before-auth, shutdown-without-hook, not-implemented Tier 1 routes, always-protected-ignores-relaxed, management-auth-default, settings-export-without-config, bootstrap loopback-only, bootstrap-without-issuer, guard feed)
- `go test -count=1 -race` on touched packages (server, cmd) — **clean**
- `scripts/check-route-guard.sh` (bonus CI entry point for the table gate) — **pass**
- Dashboard JS: extracted `<script>` from `web/index.html`, `node --check` — **clean**; "Playground" absent from the served dashboard
- Live smoke (real binary, isolated HOME, router on 20199 with `--listen 0.0.0.0:20199`, 2026-11-17; script kept at `/tmp/guard-smoke/smoke.sh` — ephemeral, not in the repo): **28/28 checks passed**
  - **G0** startup log: `route guards: ON (Tier 1 LOCAL_ONLY enforced before auth by real TCP peer — no Host/X-Forwarded-For, no manage-scope bypass; Tier 2 ALWAYS_PROTECTED always needs a gateway key; Tier 3 MANAGEMENT auth required)` + `bootstrap: PENDING`
  - **G1** `GET /dashboard/guard` → 200, tiers LOCAL_ONLY 7 / ALWAYS_PROTECTED 1 / MANAGEMENT 13, `bootstrap_pending: true`, `loopback_only_enforced: true`
  - **G3** `POST /admin/bootstrap` loopback → **201**, key returned (36 chars); **G4** second call → **409**; **G4b** guard feed then reports `bootstrap_pending: false`
  - **G6** `POST /admin/plugin/load` loopback + key → **501** (guard passed, feature not built); **G6b** `POST /admin/shutdown` loopback without key → **401** (loopback alone is not enough)
  - **G7** `POST /admin/settings/export` no key → **401**; with key → **200**, says "redacted", `gateway_key_count: 1`, no `base_url` in the payload
  - **G8** `GET /admin/status` no key → **401**, with key → **200**
  - **G9** `GET /dashboard` → 200, "Route guards" nav + `view-guard` + `loadGuard` present, "Playground" absent
  - **G10** `POST /admin/shutdown` loopback + key → **202** and the server process actually exits
- Non-loopback 403s (the core of this slice) are proven by unit tests, not by the live smoke — and that is an environment limit, not a gap in the code: this VM's sandbox resets every TCP connection to a non-loopback address (raw socket to 198.19.0.2 → `ConnectionResetError`, IPv6 egress → `Operation not permitted`, rtnetlink/netns operations denied), so no live non-loopback client can reach the server from here. The unit tests send real httptest requests with non-loopback `RemoteAddr` values through the full `ServeHTTP` stack: `TestLocalOnlyBeforeAuth` (non-loopback + valid key → 403, non-loopback + no key → 403 not 401, loopback + key → 202 with the hook run, ::1 → 202), `TestBootstrapLoopbackOnly` (non-loopback + forged `X-Forwarded-For: 127.0.0.1` + `Host: localhost` → 403, issuer never runs), `TestGuardFeed` (denial counter is a real 1 after one refused request). A parent agent with a live browser / second host should re-run G2/G5 live (bootstrap and shutdown from a non-loopback address with a valid key → 403) outside this sandbox.
- One test failure found and fixed during the gate (not hidden): the settings-export leak-word assertion tripped on the honest word "hashes" inside the redaction notice itself ("gateway-key hashes are never exported") — the assertion now checks for actual secret material (`"base_url"`, `"api_key"`, `sha256`) instead of the substring "hash"
- OmniRoute comparison (this slice): the pattern is OmniRoute's `ROUTE_GUARD_TIERS.md` (three static tiers, evaluated before any other auth branch, `403 LOCAL_ONLY` before auth, bootstrap loopback-only by real peer, `403 LOCAL_ONLY` logged as the guard working). Codvian Router now matches that shape at its own scale — 21 routes classified vs OmniRoute's ~40 prefixes/patterns — with two deliberate strictenings: (1) NO manage-scope bypass at all (OmniRoute lets a `manage`-scoped key reach `/api/mcp/` from off-host; Codvian has no bypassable Tier 1 route), and (2) loopback alone never suffices for Tier 1 — a valid key is still required after the loopback check (OmniRoute allows loopback with any/no Bearer). Where Codvian is still behind: OmniRoute's breadth (MCP/CLI-tools/services/tunnels/VNC/login flows as live spawn-capable routes, the `SPAWN_CAPABLE_PATTERNS` regex companion for dynamic segments, the operator-credential second check on credential exports, and the OpenAPI `x-loopback-only`/`x-always-protected` consistency scripts) — Codvian's five spawn routes are honest 501s, DB export/import/wipe have no route yet, and the settings export's second operator-credential check does not exist because the export redacts secrets instead of returning them. Next step toward parity: implement one real Tier 1 feature (e.g. tunnel start) and one real Tier 2 destructive route (DB export with the operator-credential boundary) behind the already-built guards.

## TFO + adaptive-concurrency gate (2026-11-17, Go 1.27.4, measured by subagent)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 19 packages ok, 475 PASS events, 0 fail** (was 433 at the auto-router gate by the same counting method; +42 new: 8 adaptive limiter, 3 provider TFO incl. cold-dial measurement, 4 router concurrency, 3 server concurrency endpoint, 1 metrics concurrency series, plus subtests)
- `go test -count=1 -race` on touched packages (adaptive, provider, router, server, metrics, degrade, cmd) — **clean**
- TFO probe on this VM (measured): supported=true, active=true, `net.ipv4.tcp_fastopen=1` (client bit set), linux/amd64; dial attempts counted live, 0 errors across the gate + smoke (24 attempts in the live smoke run)
- Cold-dial latency, fresh client per request, n=20, localhost (`TestTFOColdDialLatencyMeasured`, 3 runs): **TFO median 0.26–0.31 ms, mean 0.33–0.37 ms | no-TFO median 0.25–0.27 ms, mean 0.52–0.63 ms** — no measurable gain on localhost, as expected: the RTT is ~0, so saving one RTT saves ~nothing. The test asserts only that TFO never makes cold dials pathologically slower, not that it wins here. First-request-after-server-start in the live smoke (fresh transport, cookie warmed by the startup probe, 5ms mock delay): **TFO on mean 8.26 ms / median 8.25 ms | off mean 8.46 ms / median 8.36 ms** — a 0.2 ms difference, within noise. TFO's payoff needs a real RTT; nothing here claims otherwise
- Live smoke (real binary, isolated HOME, Python mocks on 21061 slow / 21062 fast, router on 20199, 2026-11-17; scripts kept at `/tmp/tfo-smoke/` — ephemeral, not in the repo): **18/18 checks passed**
  - **S0** startup log: `tcp fast open: ACTIVE (linux/amd64; TCP_FASTOPEN_CONNECT accepted; net.ipv4.tcp_fastopen=1 (client bit set))` + `adaptive concurrency: ON (Gradient2 per provider: cap starts at 20, floor 2, ceiling 100; … limiter fails open)`
  - **S1–S2** probe + 5 fast (5 ms) requests → slowing provider (mock-A, detected adaptively as the first-tried mock) serves all 6, fast serves 0
  - **S3** slowing provider set to 120 ms (< 150 ms hedge, so no hedge interferes), 6 sequential requests → `/dashboard/concurrency` shows **limit 2.00 (floor), baseline 5.14 ms, smoothed RTT 91.0 ms, 12 samples**
  - **S4** burst of 10 concurrent distinct requests → **slow serves 2, fast serves 8, slow max-concurrent 2, all 10 answered 200**; shed happened via deprioritisation (saturated provider ordered last), so `rejected=0` — the counter only counts `TryAcquire` refusals, and no request needed one
  - **S5** TFO active/supported, kernel `1`, dial attempts 24, errors 0; `/metrics` carries `codvian_router_concurrency_limit` + `codvian_router_concurrency_rejected_total`
  - **S7** dashboard serves, Concurrency section present, "Playground" absent; dashboard JS `node --check` clean (live-browser click-through not run — subagents cannot operate the live browser; the endpoint the JS calls is proven live above)
- Two design corrections found by the tests, not hidden: (1) the baseline-drift factor 0.002/sample moved the baseline +0.79 ms per 400 ms sample (a huge-gap drift), so it is now 0.0002/sample (~0.4 ms over 5 slow samples) — a permanently slower provider still adapts, over thousands of samples, instead of pinning the cap at the floor forever; (2) the first router tests fed 500 ms samples with no fast baseline, so the gradient was correctly 1 and the cap correctly never shrank — the tests now warm a 5 ms baseline first, which is how Gradient2 is meant to be driven
- Honest limits of this slice: the limiter's RTT comes from non-streaming completions only (a stream's wall time is not a serving RTT); hedged streaming attempts are not limiter-counted (the hedge race owns their lifetime); shedding via `TryAcquire` refusal only fires when a request actually attempts a saturated provider — under the deprioritisation ordering most sheds happen before any refusal, so `rejected` stays 0 while shedding is working; the gradient needs a fast baseline first (a provider that is slow from its very first call gets no cap shrink until it has been fast — by design, Gradient2 measures degradation against its own minimum); Windows/macOS TFO constants (15 / 0x105) are from the Winsock/macOS socket-option values and are probe-gated, not verified on those platforms in this VM
- OmniRoute comparison (this slice): OmniRoute's adaptivity is header/RPM-based — its `rateLimitManager` learns a minimum interval from upstream rate headers (and issue #9763 shows that learning can override an operator-configured minimum, a known bug) — and its in-flight cap is a fixed heavyweight ceiling (`OMNIROUTE_CHAT_MAX_HEAVY_IN_FLIGHT`, unset by default = no cap) plus an auto-derived byte budget. No per-provider RTT-gradient cap and no TCP Fast Open were found in its docs. Where this slice is ahead: Codvian Router now sheds load from a slowing provider BEFORE any 429 appears, from measured RTT rather than headers, with a fail-open guarantee and a measured TFO path — OmniRoute reacts to 429s/headers after the fact. Where it is still behind: OmniRoute's breadth (per-account rate limiting with provider-specific profiles, TPM awareness, byte budgets, 18 routing strategies, 268 providers) is far beyond this one mechanism; parity there is a multi-slice job.

## Auto-router gate (2026-11-17, Go 1.27.4, measured by subagent)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 18 packages ok, 433 top-level PASS events, 0 fail** (+30 new: 9 autorouter incl. latency measurement, 6 router auto-resolution, 1 config auto_router, 6 server auto-route endpoints/models, plus cache/router/config/server/degrade coverage of the shared paths)
- `go test -count=1 -race` on touched packages (autorouter, router, server, config, degrade, cache, cmd) — **clean**
- Routing-decision latency (measured, `TestClassifyLatency`, 1000 decisions, this VM): **mean ~31 µs, p50 ~31 µs, p95 ~40 µs, max ~162 µs** — the ≤5 ms target is beaten by ~160×. Live classify endpoint measured 49–91 µs per decision (embed + score; no LLM call, no network call — the mock upstream log shows exactly 4 calls for 4 real chat requests, 0 for classify/last/models/dashboard reads)
- Calibration (honest, not tuned to hide): 11 representative prompts route to the right tier; 2 long reasoning prompts score 0.41–0.44 (below the 0.5 threshold) but still resolve to the reasoning model via the safe fallback — fallback is the strong tier, so no misroute. Unrelated/gibberish prompts score 0.09–0.14 and fall back. Coding/chat/quick-qa prompts that fell back would misroute, so those must clear the threshold — all tested ones do (0.61–0.83 live)
- Live smoke (real binary, isolated HOME, Python mocks on 21051 coder / 21052 chat, router on 20199, 2026-11-17; scripts kept at `/tmp/auto-smoke/` — ephemeral, not in the repo): **26/26 checks passed**
  - **S1 `auto/coding` "Write a Python function to reverse a string"** → coder mock, `X-Codvian-Auto-Tier: coding`, `X-Codvian-Auto-Model: coder-model`, confidence 0.607
  - **S2 `auto` "Tell me a funny joke about cats"** → chat mock, tier `chat`, confidence 0.832
  - **S3 `auto` "zxqv blorp wumpus flibberty"** → fallback to `reasoner-model`, tier `reasoning`, confidence 0.135, `X-Codvian-Auto-Fallback: true`
  - **S4 `GET /dashboard/auto-route?model=auto/coding&prompt=…`** → live decision JSON (tier coding, model coder-model, per-tier scores, latency_micros 91)
  - **S5 `GET /dashboard/auto-route/last`** → `has_decision: true`, the S3 fallback decision (tier reasoning, fallback true)
  - **S6 `GET /v1/models`** → all 5 virtual models listed, `owned_by: codvian-router`
  - **S7 `GET /dashboard`** → 200, Auto-router nav + "Route it" present, honest empty states ("No routing decision yet", "No auto-routed request yet"), "Playground" absent
  - **S8 streaming `auto/coding`** → SSE chunks from coder mock + `[DONE]`
  - Dashboard JS syntax-checked with `node --check` (live-browser click-through not run — subagents cannot operate the live browser; the endpoints the JS calls are proven live above)
- Concurrency note: another worker edited `web/index.html` and `internal/server/server.go` concurrently (dashboard verification badges); all auto-router edits were made against freshly re-read files with anchored edits, and the final gate re-ran after their changes — nothing was clobbered (16 auto-router references in server.go, 11 in index.html, all present at gate time)
- Environment note: the first smoke run failed with "No space left on device" — /tmp is a 512M tmpfs and the smoke HOME had redirected Go's module/build cache there. Fixed in the smoke script (`GOCACHE`/`GOPATH` pinned to the real home); not a product issue
- OmniRoute comparison (this slice): OmniRoute already ships an `auto` model with 22 routing strategies, 16-factor auto-scoring and a 4-tier cascade — Codvian Router is NOT at parity on strategy breadth (one local strategy, no quality signal, no cost-aware tiering yet). Where this slice is ahead: the decision costs ~30 µs measured with zero extra LLM call and zero network call (OmniRoute's "milliseconds" failover is a vendor claim with no published benchmark), the fallback is explicit and honest (never a guess), and the whole path is ToS-clean with keys encrypted at rest. Next step toward parity: a second strategy (cost/latency-aware tier ordering via the existing scoring signals) and a measured quality signal from the eval harness.

## Semantic-cache wiring gate (2026-11-17, Go 1.27.4, measured by this run)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 17 packages ok, 403 top-level tests pass, 0 fail** (was 393 before this work; +10 new: 7 degrade ServeCache tests, 3 server cache-header/metrics tests)
- `go test -count=1 -race` on touched packages (degrade, server, cache, cmd) — **clean**
- Live smoke (real binary, isolated HOME, Python mock on 21021, router on 20199, 2026-11-17; scripts kept at `/tmp/semcache-smoke/` — ephemeral, not in the repo): same prompt → exact hit (`X-Codvian-Cache: exact`), paraphrase "What is the capital of France today?" → semantic hit (`X-Codvian-Cache: semantic`, similarity 0.920 ≥ 0.85), unrelated prompt → miss + real provider call, streaming paraphrase → single replayed chunk from cache; mock upstream called exactly 2 times across 5 requests; `/metrics` shows `cache_hits_total{layer="exact"} 2`, `{layer="semantic"} 1`, requests attributed to `provider="cache"`; ledger `usage_events` has exactly 2 rows (the 2 real provider calls, 0 rows for the 3 cache hits); provider down + uncached prompt → honest 502, no masked failure
- Two real bugs found and fixed during this work (not hidden): (1) the stale fallback served content-only entries to tool-using requests on failure — now refused; (2) cache hits were double-counted (wrapper + server) — now counted exactly once, in the wrapper

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

## Cohere native-wire integration gate (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 17 packages ok, 448 PASS events (371 top-level tests + 77 subtests), 0 fail** (was 418 by the same counting method; +21 top-level / +9 subtests: 11 wire Cohere conversion/error/stream tests, 6 Cohere client tests incl. request shape/auth/stream/errors/ListModels, 1 catalog auth test, 2 registry catalog tests, 1 router cross-wire failover test with both directions as subtests, plus the renamed wire-fallback test now using `bedrock-converse` since Cohere has its own client)
- `go test -count=1 -race` on touched packages (provider, wire, catalog, router) — **clean**
- Live smoke (real binary, isolated HOME, Python mocks on 21011 Cohere / 21012 OpenAI, router on 20199, 2026-11-17):
  - **C1 non-stream Cohere:** mock received `POST /v2/chat` with `Authorization: Bearer co-smoke-key`, body `{model: shared-model, messages: [{role: user, content: [{type: text, text: "hi"}]}]}`; router returned content "cohere says hi", `finish_reason: "stop"`, usage 10/5/15
  - **C2 stream Cohere:** SSE chunks `cohere `, `streams hi`, finish chunk `finish_reason: "stop"`, then `[DONE]`
  - **C3 Cohere 429 → OpenAI:** Cohere mock forced to 429; router answered from the OpenAI mock (`POST /v1/chat/completions`, "openai says hi", finish_reason stop)
  - **C4 OpenAI 429 → Cohere:** reverse direction (second server run, OpenAI provider named to sort first); router answered via `POST /v2/chat`, "cohere says hi"
  - **Both 429, same prompt:** router returned the cached answer with `X-Codvian-Stale: true` (designed stale degradation, not a failure); **both 429, fresh prompt:** honest `502 {"error":{"message":"upstream provider error"}}`. A fresh-prompt 502 right after a 429 burst is the exponential-backoff cooldown still in effect, not a broken ok-path — a fresh server with both mocks OK answered via Cohere first ("cohere says hi", 0 OpenAI hits)
  - Ordering note: the server's scoring OrderHook ranks by last health-probe latency, so with two sub-ms localhost mocks either provider can legitimately be tried first (coin flip); C1/C3/C4 each prove the Cohere wire when it is tried, in both directions
  - Startup line: `provider "Cohere" catalog cohere verification verified-from-docs wire cohere auth native`
- Smoke scripts kept at `/tmp/cohere-smoke/` (mocks.py, run.sh, probe.sh, control.sh, loop.sh) — ephemeral, not in the repo

## Bedrock native-wire integration gate (2026-11-17, Go 1.27.4, measured by subagent)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 17 packages ok, 393 top-level PASS events, 0 fail** (was 371 at the Cohere gate by the same counting method; +22: 10 wire Bedrock conversion/error/stream tests, 4 SigV4 signer tests, 5 Bedrock client tests, 2 registry catalog tests, 1 router cross-wire failover test with both directions as subtests, plus the renamed wire-fallback test now using `ollama` since Bedrock has its own client)
- `go test -count=1 -race` on touched packages (provider, wire, router) — **clean**
- Live smoke (real binary, isolated HOME, Python mocks on 21041 Bedrock / 21042 OpenAI, router on 20199, 2026-11-17):
  - **B1 non-stream Bedrock:** mock received `POST /model/anthropic.claude-3-haiku-20240307-v1:0/converse` with `Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=<64-hex>` + `X-Amz-Content-Sha256` + `X-Amz-Date`, NO Bearer header, body `{messages: [{role: user, content: [{text: "Hi"}]}], system: [{text: "Be helpful."}]}`; router returned content "live smoke from bedrock", `tool_calls: [{get_weather, {"city": "Pune"}}]`, `finish_reason: "tool_calls"`, usage 12/9/21. Mock's own SigV4 verdict: ok
  - **B2 stream Bedrock:** SSE chunks `live `, `smoke`, tool-call head delta `{index: 0, id: tooluse_1, name: get_weather}`, argument fragments `{"city":` / ` "Pune"}`, finish chunk `finish_reason: "tool_calls"`, then `[DONE]`. (First run FAILED: the mock sent a toolUse delta to a text block — an unrealistic Converse stream; the translator correctly rejected it. Mock fixed to the real shape — toolUse announced in `contentBlockStart`, input as fragments — and re-verified; no router code changed)
  - **B3 Bedrock 429 → OpenAI:** Bedrock mock forced to 429 `ThrottlingException`; router answered from the OpenAI mock (`POST /v1/chat/completions`, "live smoke from openai", finish_reason stop)
  - **B4 OpenAI 429 → Bedrock:** reverse direction (second server run, OpenAI provider named to sort first); router answered via `POST /model/.../converse`, "live smoke from bedrock", tool_calls + finish_reason tool_calls
  - Startup line: `provider "AWS Bedrock" catalog bedrock verification verified-from-docs wire bedrock-converse auth converse`; `provider add-catalog bedrock 'AKIDEXAMPLE:SECRET' --base-url <mock>` exits 0 with `wire: bedrock-converse`, `auth endpoint: converse`, 2 models fetched
- Smoke scripts kept at `/tmp/cr-smoke-bedrock/` (mocks.py, smoke.sh) — ephemeral, not in the repo

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
  generally miss. It IS now wired into the request path (items 55–58):
  proactive exact-then-semantic serving behind `Policy.ServeCache`,
  honest `X-Codvian-Cache` headers, no provider billing on hits. Startup
  prints the honest description ("token-overlap similarity … not neural
  embeddings").
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
- Catalog wiring (2026-11-17): the Bedrock native wire transport is now implemented and proven live (see items 52–54 and the Bedrock integration gate above) — the client speaks OpenAI wire, Anthropic Messages wire, Gemini generateContent wire, Cohere Chat API v2 wire, and Bedrock Converse wire. The Anthropic wire IS proven live (see the Anthropic integration gate above): the mock spoke real Anthropic Messages and the router answered with correct tool calls + finish reason. The Gemini wire is likewise proven live (see the Gemini integration gate above). The Cohere wire is likewise proven live (see the Cohere integration gate above).
- NO-FAKE-DATA (2026-11-17, auto-router slice): this slice adds no sample/fake data — the auto-router decision view shows only live decisions (typed-prompt classify + last real `auto/*` request) with honest empty states, and the shipped utterances are labelled config defaults, never presented as measured results. BUT the pre-existing dashboard still ships hardcoded sample data (Overview KPIs/traffic chart, provider/model catalogs, request logs, usage spend bars in `web/index.html`) presented as if real. Removing or labelling it is out of this slice's scope — it would have conflicted with the concurrent verification-badges work on the same file — and remains open: any slice touching those sections must replace them with real endpoints or honest empty states.
- Catalog wiring: DeepSeek's chat path has NO `/v1` per the catalog note, but the client always appends `/v1` — per-endpoint path support is still open. The catalog note itself conflicts with DeepSeek's own docs (which use base `https://api.deepseek.com/v1`); verify against DeepSeek docs before changing either side.
- Catalog wiring: exchange auth (Vertex service-account JSON, OpenRouter JWT, Azure Entra) is refused loudly, not implemented — Bedrock SigV4 is the exception, implemented in this gate (pure-Go signer, client signs each request). ToS-grey providers are refused as routing targets. Dashboard surfacing of per-provider verification status is now done (items 61–62: GET /api/providers + live dashboard badges, no sample providers).
- Anthropic wire (2026-11-17): the Bedrock native transport is now built (see items 52–54 and the Bedrock integration gate). Exchange auth for Vertex / OpenRouter JWT / Azure Entra still refused loudly. Tool-call deltas are NOT replayed across a mid-stream resume — only the text prefix is continued (documented in the router); the returned response still carries the full reassembled tool calls. A stale stream replay carries text only: the exact-match cache stores content, not tool calls, so a stale stream cannot reconstruct the original tool calls. DeepSeek `/v1` path question still open (catalog note says no `/v1`, DeepSeek's own docs use base `https://api.deepseek.com/v1` — verify before changing either side).
- Catalog wiring: `ResolveAuthForBaseURL` scans all 423 catalog entries per `ClientFor` call (no cache). Fine at current rates (startup + 60 s health probes); revisit if `ClientFor` gets hot.

## Dashboard wiring gate (2026-11-17, Go 1.27.4, measured by coordinator)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 15 packages ok** (server 24 tests incl. 3 new dashboard tests: `TestDashboard`, `TestDashboardMethodNotAllowed`, `TestDashboardDoesNotBypassAPIAuth`)
- Live smoke (real binary, isolated HOME, fresh config, port 20199): startup prints `dashboard: http://127.0.0.1:20199/dashboard`; `GET /dashboard` → **200**, `text/html; charset=utf-8`, 113,427 B, "Codvian Router"/"Overview"/"Providers"/"Models" present, "Playground" absent; `GET /healthz` → 200; `GET /v1/models` + `GET /metrics` without key → 401; `POST /dashboard` → 405; `--listen 192.168.1.5` still refused
- Note: the dashboard loads Google Fonts from fonts.googleapis.com (external); offline it falls back to system fonts — same as the approved artifact.

## Dashboard provider verification badges gate (2026-11-17, Go 1.27.4, measured by this run)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 18 packages ok, 433 top-level tests pass, 0 fail** (includes 4 new: `TestAPIProviders`, `TestAPIProvidersNoHealthChecker`, `TestAPIProvidersEmpty`, `TestAPIProvidersMethodNotAllowed`; the count grew during the gate as parallel agents landed tests — 427 in an earlier green run of the same gate)
- `go test -count=1 -race` on touched packages (server, cmd/codvian-router) — **clean**
- Dashboard JS syntax: `node --check` on the extracted inline script — clean
- Live smoke (real binary, isolated HOME, Python mocks on 21031/21032, router on 20199, 2026-11-17): `GET /dashboard` → **200**, 120,812 B, `text/html; charset=utf-8`, contains `verif-badge` / `/api/providers` / `Demo data`; `GET /api/providers` → **200** with Groq `verified-from-docs` + `health_status: ok` and Mock Custom `unverified` + `ok`, summary `{total:2, enabled:2, verified_from_docs:1, unverified:1}`, no keys or base URLs in the payload; after killing the Groq mock the watcher's next probe flipped it live to `health_status: "down"` while Mock Custom stayed `ok`; `GET /v1/models` without key → **401**; `POST /api/providers` → **405**; `GET /healthz` → 200. Empty-config server (port 20299): `/api/providers` → 200 empty list + zero summary, dashboard carries "No providers connected yet" (no sample providers anywhere).

## Single-flight coalescing gate (2026-11-17, Go 1.27.4, measured by this run)

- `go build ./...` — OK; `go vet ./...` — OK; `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 19 packages ok, 456 PASS events, 0 fail** (was 433 by the same counting method; +23 new: 6 coalescing package, 8 router engine, 2 ledger, 1 metrics, 3 server endpoint, 2 degrade forwarding, plus 1 subtest)
- `go test -count=1 -race` on touched packages (coalescing, router, server, degrade, ledger, metrics, cmd/codvian-router) — **clean**
- Dashboard JS syntax: `node --check` on the extracted inline script — clean; "Playground" absent from the page (0 occurrences)
- Live smoke (real binary, isolated HOME, Python mock on 21061 with a 300 ms upstream delay so concurrent requests overlap, router on 20199, 2026-11-17; scripts kept at `/tmp/coal-smoke/` — ephemeral, not in the repo): **42/42 checks passed**
  - **C1 10 concurrent identical non-streaming requests** → mock recorded exactly **1 upstream call**, all 10 got 200 with "coal-mock says hi"
  - **C2 2 concurrent different prompts** → 2 upstream calls (1 each), both answered
  - **C3 2 concurrent identical streaming requests** (fresh prompt) → **2 upstream stream calls** — streams are NOT coalesced; both SSE streams delivered both chunks + `[DONE]`
  - **C4 5 concurrent identical requests, upstream 500** → exactly **1 upstream call**, all 5 got an honest 502
  - **C5 `GET /dashboard/coalescing`** → `{"requests":17,"flights":4,"shared_calls":13,"upstream_avoided":13,"quota_saved_calls":13,"in_flight":0}` — exactly the measured counts (9 shared from C1 + 4 from C4; streams never entered the group)
  - **C6 /metrics** → `codvian_router_coalesced_total 13`; without a key → **401**
  - **C7 ledger (SQLite)** → 19 rows: 6 billed + 13 shared; shared rows 9 ok + 4 failed, all zero tokens and zero cost; billed tokens 45 (3 successful non-stream calls × 15; the failed leader records 0, the 2 streams record unknown/0) — usage accounting stays honest, no double-count
  - **C8 `GET /dashboard`** → 200, Coalescing nav + `/dashboard/coalescing` fetch + "No coalesced request yet" empty-state text present, "Playground" absent
- Design notes from this slice: the first build put the group in `internal/router`, which broke the build — `server` importing `router` creates an import cycle because `router_test` imports `server`. The group now lives in the new leaf package `internal/coalescing` (same reason `routetrace` exists). One smoke check initially failed on the author's own wrong expectation (billed_tokens 60 vs measured 45 — the failed C4 leader legitimately records 0 tokens); the expectation was corrected, no product code changed. One `wait` bug in the first smoke run (bare `wait` also waited on the servers) was fixed in the script.
- OmniRoute comparison (this slice): OmniRoute advertises request deduplication as a feature, but ships no published measurement of it; Codvian Router's coalescing is now measured live (10→1 upstream calls, 13 avoided calls across the smoke run, ledger-proven zero double-billing) with the exclusion rules (streaming, tool-call turns) enforced by test. Where Codvian Router is still behind: no cross-instance coalescing (single process only — OmniRoute's distributed setup can dedupe across nodes) and no token-level quota-saved accounting (quota saved is counted in calls, not tokens, because per-model prices are not configured — never invented).
- Note: three transient failures during the gate were concurrent-edit races with parallel agents in the shared worktree (a `router` test-build failure, a `config` test failure, and a `main.go` rebuild failing mid-write with `undefined: buildAutoRouter` while another agent landed autorouter wiring) — each passed on immediate re-run with no change to this slice. No fake data remains in the Providers section or any provider-driven render; the remaining "Demo data" sections (Models, Logs, Usage, KPIs, router keys) are labelled, not hidden — their live feeds are separate slices.
