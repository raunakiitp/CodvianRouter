# Provider / authentication support matrix — Phase 1 baseline

**Evidence snapshot:** CodvianRouter `main` at `a1afa7c567fc6985a21b4d349b29727c35eda6a9` (2026-11-21). This is a code-and-test inventory, not a promise that every catalog entry works.

## Status definitions

- **metadata-only** — a provider/surface is present in the static catalog. This does not prove that a URL is routable, credentials are accepted, or the route is wired.
- **key-configured** — this particular installation has a credential that passed Codvian's live add/rotate test. It is per installation and may become stale; the public repository cannot claim any user's providers are configured.
- **mock-tested** — the auth/client/wire behavior has deterministic tests against local test servers or fixtures. This is not a live vendor test.
- **real-provider-tested** — a request reached the named vendor and the provider-specific behavior passed. Evidence must include the provider, capability, date, and test path; never infer this from documentation.
- **unsupported** — deliberately refused or not implemented. Keep it distinct from metadata and test statuses.

`catalog.Provider.Verification` (`verified-from-docs`, reference metadata, or unverified) records **source quality**, not runtime support status. The two dimensions must not be collapsed.

## Catalog inventory at the evidence snapshot

| Measure | Count | Meaning |
|---|---:|---|
| Active catalog entries | 610 | Catalog rows, not working integrations |
| Concrete routable base URLs | 370 | Can be addressed without filling a template; still requires valid auth and compatible wire behavior |
| Template-only base URLs | 9 | Operator must supply the resource/region/account-specific URL |
| API-capable entries without any base URL | 138 | Metadata exists but cannot be directly routed as recorded |
| `verified-from-docs` | 65 | Base/auth/wire facts were checked against docs; not real-service tests |
| Reference-metadata-only | 541 | Metadata-only evidence |
| Unverified | 4 | Not verified even at the documentation level |

These are independently computed from `catalog.All()` and its routability/verification fields. Catalog counts are inventory counts, not “supported providers.”

## Authentication and capability matrix

| Provider / surface | Catalog/auth description | Current implementation evidence | Test evidence at baseline | Status / Phase 1 action |
|---|---|---|---|---|
| Conventional API-key providers (Bearer, `x-api-key`, `api-key`, `x-goog-api-key`, custom headers) | Endpoint-specific auth in the catalog; custom base URL path also exists | Catalog auth resolution, live credential test on add/rotate, encrypted provider storage, key pools with preferred/reserve roles, per-key routing/failover, and live model refresh exist | Auth resolver, registry, provider client and wire tests use deterministic test servers; default CI has no vendor credentials | **mock-tested** for covered generic behavior. **key-configured** only after this installation's live add test. **real-provider-tested:** no default-CI evidence; do not claim globally. Phase 1 audits per-provider quirks and adds live smoke gates where credentials are available. |
| OpenRouter API key | Bearer API key is an ordinary static-key path | Catalog registration, encrypted storage, model discovery and OpenAI-compatible routing path exist | Covered by local registry/client tests; no vendor key is committed or used in default CI | Same statuses as conventional API keys. |
| OpenRouter OAuth PKCE | Official flow returns a user-controlled API key (the catalog's separate `jwt` variant remains metadata) | **Implemented (Phase 1):** `internal/providerlogin` drives the documented PKCE flow (S256 challenge, 256-bit single-use state, 10-min pending TTL, headless + callback modes); `POST /admin/provider/login` + loopback callback route and `provider login openrouter` CLI store the exchanged key through the encrypted registry path (live test, model discovery); further logins join the existing provider's key pool as accounts. No refresh token is claimed — the flow issues an API key. | Mock suite (RFC 7636 vector, state single-use/expiry, exchange errors, cap, secret non-leakage, real-registry end-to-end) **plus live operator verification 2026-11-21** (see log below): two real browser authorizations, real exchange, encrypted storage, 441-model discovery, pool join, restart persistence, per-key failover on 429/402, zero secret leakage. | **real-provider-tested (2026-11-21)** for: authorization + PKCE exchange, live key validation, encrypted storage, model discovery (441 models), multi-key pool join, restart persistence, per-key failover, secret/log hygiene. **Cross-account failover: NOT verified** — both keys belong to the SAME account (no second account was available); only same-account cross-KEY failover is evidenced. **Successful real chat completion: QUOTA-BLOCKED / PENDING** — the operator account's free tier returned persistent 429s and the account has never purchased credits (paid probe → honest 402, $0 spent); inference is NOT fully verified. `jwt` variant: **unsupported**. Quirks found by this verification were audited and fixed in the Provider Quirks Audit Slice (see [PROVIDER_QUIRKS_AUDIT.md](PROVIDER_QUIRKS_AUDIT.md)); the fixes are mock-tested + code-verified, live re-verification pending the same quota unblock. |
| Azure OpenAI API-key variant | `api-key` header and resource URL template | Static API-key auth is available when the operator supplies the concrete resource URL | Generic auth/client tests; no Azure account used | API-key path can be **mock-tested** / **key-configured**; real status remains per installation. |
| Azure OpenAI Microsoft Entra ID (`entra-id`) | Client-credentials exchange, scope `https://cognitiveservices.azure.com/.default` | **Implemented (Phase 1):** `internal/provider/entra.go` mints the Bearer token from stored `{tenant_id, client_id, client_secret}` JSON (encrypted at rest), caches it in memory with 60 s expiry skew and re-mints on expiry via the expiry-aware auth cache; mint breaker (5 failures → 30 min); token-endpoint error bodies never echoed; `token_uri`/`scope` overrides validated. Client-credentials has no refresh token — re-minting is the refresh behavior. | `internal/provider/entra_test.go`: parse validation, mint form/scope, cache hit, expiry re-mint (unit + registry auth-cache), breaker, secret non-leakage, add→chat carries minted Bearer, honest mint-failure refusal, api-key variant unaffected | **mock-tested**, not **real-provider-tested** (no Azure resource used). |
| AWS Bedrock Converse (SigV4) | AWS access/secret/session credentials; region-specific signature | Stdlib SigV4 signer and registry/client wiring exist; credentials use the encrypted provider credential store | Local signing/client tests; no AWS account or live Bedrock inference used | **mock-tested**, not **real-provider-tested**. Keep current implementation; do not rewrite. |
| Google Vertex AI service account | Service-account JSON exchanged for a short-lived OAuth access token | JWT-bearer token minting, expiry cache, refresh-before-expiry and Vertex-native client exist | Local token-endpoint and Vertex client tests; no GCP project was used | **mock-tested**, not **real-provider-tested**. Keep current implementation; do not rewrite. |
| Claude Code subscription OAuth | Catalog marks this route ToS-grey and points to a non-vendor source | Resolver refuses it as a routing target | Refusal path is tested | **unsupported**. Do not bypass the refusal or emulate browser cookies. |
| Web-session/cookie providers | Cookie/browser-session auth appears in catalog metadata | Resolver deliberately refuses cookie auth | Refusal path is tested | **unsupported**. |
| Other provider rows with no wired exchange, missing/templated endpoints, or reference-only data | Static catalog rows | Depends on the exact endpoint/auth/wire; not implied by category or row count | Varies; must be traced provider-by-provider | **metadata-only** until a concrete endpoint, supported auth, adapter, and relevant tests are evidenced. |

## Existing API-key behavior to preserve

- Adding or rotating a provider credential tests it against the upstream before persisting it.
- Provider credentials are encrypted at rest; API-key pools already support preferred/reserve selection and per-key failover.
- Provider add-time and manual refresh paths discover upstream models and record when the list was fetched.
- Native or endpoint-specific clients already exist for OpenAI-compatible, Anthropic, Gemini, Cohere, Bedrock and Vertex surfaces. Phase 1 must extend only proven gaps, not replace working adapters.

## Phase 1 verification rule

Every new OAuth flow must have tests for PKCE/state or token acquisition, invalid/expired credentials, expiry refresh/reacquisition, cancellation/timeouts, persistence/restart, multi-account key selection, model discovery, and its declared wire capabilities. Local mock tests earn only `mock-tested`. A vendor account test earns `real-provider-tested` only for the exact capability exercised. No key, refresh token, OAuth code, or secret may appear in logs, docs, fixtures, or commits.

## Phase 1 implementation log (2026-11-21)

- OpenRouter PKCE login: `internal/providerlogin/openrouter.go` (+ tests), `internal/server/provider_login.go` (+ tests), `provider login openrouter` CLI, guard-table callback route (LOCAL_ONLY 10 → 11; routes 313 → 314; honest-501 list 64 → 63). Usage and security properties: [PROVIDER_LOGIN.md](PROVIDER_LOGIN.md).
- Azure Entra ID client credentials: `internal/provider/entra.go` (+ tests), expiry-aware `cachedAuth` in the registry, `entraIDHeaders` wired into `AddCatalogWithOptions` and both resolution paths; catalog `entra-id` exchange now records `TokenTTL: 1h` and the implemented credential shape.
- Existing working adapters (SigV4, Vertex, static-key pools, model discovery/refresh) were preserved, not rewritten.
- Evidence tier for both new flows: **mock-tested** (deterministic local auth-host/upstream servers). **real-provider-tested** remains open per installation: it needs an operator OpenRouter account / Azure resource, and no vendor credentials are committed or used in default CI.

## Live verification log — OpenRouter PKCE (2026-11-21, operator account)

Environment: merged `main` `1737833` binary, fresh config, loopback server `127.0.0.1:20128`, callback mode (no secret passed through chat or CLI argv). Two browser authorizations by the operator on the SAME OpenRouter account (a second account was not available; the pool-join mechanics are identical, the evidence is labelled accordingly).

| Step | Evidence |
|---|---|
| Official authorization + PKCE exchange | Two `POST /admin/provider/login` starts returned real `openrouter.ai/auth` URLs (S256 challenge, 43-char state); browser redirects landed on the loopback callback; server log: `provider login completed … account_key=false routing_live=true` (login 1) and `account_key=true routing_live=true` (login 2). |
| Key validation + encrypted storage | Add-time live key test passed (provider `health_status: ok`, real probe p50 591 ms); pool shows key IDs + 12-hex fingerprints only; `config.json` scan: **0** occurrences of `sk-or-v1-`. |
| Real model discovery | **441 models** fetched live from OpenRouter `/v1/models` (11 `:free`); `models_fetched_at: 2026-11-21T12:00:20Z` on the model detail feed. |
| Second login → pool | One provider, two keys (`key_78a365ba…`, `key_c2db0f74…`), distinct fingerprints, both `preferred`/`active`; no duplicate provider. |
| Restart persistence | Server killed and restarted on the same config: 2 keys + 441 models + health ok restored. |
| Routing / per-key failover | Route traces show real cross-key attempts: 429 on `key_c2db…` → attempt on `key_78a3…`; 402 (insufficient credits) on `key_78a3…` → attempt on `key_c2db…`; repeated 429s put both keys into hard cooldown (`skipped_cooldown` attempts observed) — the per-key cooldown/failover machinery works live. |
| Successful completion | **Blocked by account state, not the router**: every `:free` model returned persistent 429 (free-tier quota), and a single tiny paid probe (`openai/gpt-4o-mini`, max_tokens=10) returned the upstream's honest 402 "This account never purchased credits" on both keys. **$0 spent.** Two earlier free-tier requests reached the model but ended `finish_reason: length` with empty content at max_tokens=30 (recorded as a quirk to investigate, not claimed as success). |
| Secret/log leakage | Scans of both server logs + config: 0 occurrences of API-key material, either state value, `code_verifier`, authorization-code query strings, or the gateway key. Callback access-log lines carry path/status/latency/request-id only. |

**Bug found by this live verification and fixed:** the callback's pool-join path passed the PRE-add provider snapshot to the routing hook, so the engine never registered the second key and every attempt kept using the first. Fix: re-read the provider after `AddKey` and use the key-changed hook for pool joins (`internal/server/provider_login.go`), with regression test `TestProviderLoginPoolJoinHookSeesUpdatedPool`. The cross-key failover traces above were recorded AFTER the fix and a restart.

**Quirks recorded for the provider-quirks audit slice — RESOLVED 2026-11-21 (see [PROVIDER_QUIRKS_AUDIT.md](PROVIDER_QUIRKS_AUDIT.md)):**
1. Model addressing: `/v1/models` lists nested OpenRouter IDs bare (`liquid/lfm-3-1.7b:free`), but chat parsed the first `/` segment as a provider pin — copying a listed ID verbatim failed with `routing.only: unknown provider`. **Fixed**: a full model ID that a registered provider serves verbatim is no longer treated as a pin; explicit `openrouter/<id>` pins still work. Regression tests: `TestPrefixedModelRoutingNestedIDs` (+ conflict/legacy-adder tests), `TestRegistryOffersModel`.
2. Free-tier 429 retry storms: **root cause was the pool-join stale-snapshot bug above** (single-key chain → same-target retries with no next target). The 429 policy itself is bounded and classified (quota-exhausted / concurrency-limit / backoff ladder, per-key hard cooldowns) — documented with code references in the audit; no policy change justified. Cross-key failover traces after the fix prove the intended behavior.
3. `finish_reason: length` with empty content at small `max_tokens`: **fixed** — a reasoning-only completion (model spent its budget thinking) is real output, not an empty stream; both the non-streaming (`isEmptyCompletion`) and streaming preread (`doneHasReasoning`) paths now commit it instead of failing over. Regression tests: `reasoning-only-is-not-empty`, `empty-reasoning-state-still-fails-over`, `TestStreamingReasoningOnlyIsNotEmpty`. Live re-verification of all three fixes is pending the account-quota unblock (no paid probes per operator instruction).

Azure Entra ID remains **mock-tested**: no real Azure resource was available; nothing in this log changes its status.

## Provider Quirks Audit Slice (2026-11-21)

Full findings, code references, wire/client coverage sweep and the prioritized backlog: [PROVIDER_QUIRKS_AUDIT.md](PROVIDER_QUIRKS_AUDIT.md). Evidence distinctions preserved exactly:

1. OpenRouter OAuth PKCE: **REAL account verified** (exchange, validation, storage, discovery, pool join, restart, per-key failover, hygiene).
2. Encrypted key storage, live model discovery (441 models), restart persistence: **verified**.
3. Two-key cross-key routing/failover: **verified**, but both keys belong to the SAME account.
4. Cross-account failover: **NOT verified**.
5. Successful real chat completion: **QUOTA-BLOCKED / PENDING** — inference is not fully verified.
6. Azure Entra ID: **MOCK-TESTED**.

## OAuth flow audit (P1-08, 2026-11-21; classification re-verified 2026-11-27)

All 31 `CategoryOAuth` catalog entries are subscription/IDE session flows (ChatGPT, GitHub Copilot, Codex, Claude Code, Cursor, Kiro, Antigravity, GitLab Duo, Zed, Cline, Devin, ...), not permitted API-key OAuth. Their verified catalog shape is pinned by `TestCategoryOAuthInventoryPinsTheTruth` (`internal/catalog/oauth_inventory_test.go`):

- `claude` (the only docs-verified one, and the only one with an auth surface) is **ToS-grey** with a documented `oauth-pkce` exchange → refused at add time (tested). Its catalog entry is dialable *only* with a user-obtained token; the router does not perform the login.
- `github-copilot` and `clinepass` carry a probed base URL but **no endpoints and no auth surface** — partially documented, not connectable through the catalog.
- The other 28 are reference-metadata-only stubs (no base URL, no endpoints, `WireUnknown`) → refused at add time as "credential exchange is not supported yet".

Regression-tested for `github-copilot`, `codex`, `cursor`, `kiro`, `xai-oauth` (`TestRegistryAddCatalogRefusals`): a subscription flow can never be silently turned into a static key or a faked token.
- Policy: only official, documented, permitted provider flows are implemented. The four implemented exchanges: OpenRouter PKCE (**real-provider-tested**), Azure Entra client credentials (**mock-tested**), Bedrock SigV4 (**mock-tested**), Vertex service-account JSON (**mock-tested**). No browser-cookie scraping, no private subscription session reuse.
- `internal/oauth` is the **gateway's own** OAuth 2.0 authorization server for gateway clients (authorization-code + PKCE → HS256 JWT access tokens, single-use rotated refresh tokens) — a client-auth surface, not provider auth; tested (`oauth_test.go`, `jwt_test.go`).
- Perplexity's documented MCP OAuth 2.1+PKCE is an **upstream MCP tool-server** flow, not routing auth: the gateway's MCP surface authenticates with gateway keys (OAuth explicitly NOT APPLICABLE — `internal/mcp/tools_search.go`), and Perplexity routing uses the Bearer API-key path. Status: metadata-only for the MCP OAuth surface; out of Phase 1 routing-auth scope.

## Capability parity sweep (P1-07, 2026-11-21)

Repository-evidence sweep of the capability areas the Phase 1 directive names. Every area has a real implementation plus dedicated deterministic suites across provider/router/server; per-provider LIVE verification is separately blocked (G4/G5/G6) and never inferred from these tests.

| Capability | Implementation evidence | Test evidence | Status |
|---|---|---|---|
| Tool/function calling | per-wire tool translation + capability gating + relay tool gate | `provider/ct1_tools_test.go`, `router/ct1_tool_capability_test.go`, `server/m6_3_relay_tool_gate_test.go` | mock-tested |
| Reasoning support | cross-wire capture/handoff, forced/intent effort policies, reasoning-only completions commit (PR #21) | `provider/reasoning_test.go`, `provider/effort_budget_test.go`, `router/reasoning_test.go`, `router/forced_effort_test.go`, `router/intent_effort_test.go`, `router/cancel_test.go` | mock-tested |
| Model discovery + refresh | add-time live probe, manual/periodic refresh, `models_fetched_at` | `provider/refresh_test.go`, `provider/dashboard_models_test.go`, `server/models*_test.go` | mock-tested; discovery **real-provider-tested** via OpenRouter (441 models) |
| Error classification + rate limits | uniform taxonomy across wires, Retry-After body classification (quota/concurrency/plain), header learning, breaker-4xx rule | `provider/taxonomy_test.go`, `provider/retryafter_body_test.go`, `provider/ratelimit_headers_test.go`, `router/ratelimit_test.go`, `router/retryafter_test.go` | mock-tested; 429/402 failover **real-provider-tested** (live traces) |
| Context/output limits | context-fit chain jumps, effort/output budgets, per-request USD budget (pre-send/mid-chain/post-hoc, now incl. streams — G1), internal budgets | `router/contextfit_test.go`, `server/contextfit_test.go`, `router/requestbudget_test.go`, `router/internalbudget_test.go`, `server/stream_usage_test.go` | mock-tested + integration-tested |
| Image/audio/video | images/audio clients, passthrough, media verification, modality bridges | `provider/images_client_test.go`, `provider/audio_client_test.go`, `provider/media_verify_test.go`, `router/images_passthrough_test.go`, `router/audio_passthrough_test.go`, `router/media_verify_test.go` | mock-tested |
| Key pools / rotation / account selection | preferred/reserve roles, AddKey/RemoveKey/SetKeyRole, per-key cooldowns + failover | `provider/keys_test.go`, `provider/pool_test.go`, `router/keypool_test.go`, `server/provider_keys_test.go` | mock-tested; 2-key pool + restart **real-provider-tested** (same account) |
| Streaming usage accounting | G1 (PR #22): include_usage, per-wire aggregation carried to ledger/budget/client | `provider/router/server stream_usage_test.go` | mock-tested + integration-tested |
| Catalog `ToolCalling` metadata | informational only (`SupportsTools` is not a routing filter); unset on most verified entries | catalog validation tests | metadata backlog (documentation, not a feature) |
| Per-provider live capability verification | needs operator credentials/accounts/resources | — | **blocked-external-dependency** (G4/G5/G6) |
