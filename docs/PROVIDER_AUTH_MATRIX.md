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
| OpenRouter OAuth PKCE | Official flow returns a user-controlled API key (the catalog's separate `jwt` variant remains metadata) | **Implemented (Phase 1):** `internal/providerlogin` drives the documented PKCE flow (S256 challenge, 256-bit single-use state, 10-min pending TTL, headless + callback modes); `POST /admin/provider/login` + loopback callback route and `provider login openrouter` CLI store the exchanged key through the encrypted registry path (live test, model discovery); further logins join the existing provider's key pool as accounts. No refresh token is claimed — the flow issues an API key. | `internal/providerlogin/openrouter_test.go` (RFC 7636 vector, state single-use/expiry, exchange errors, cap, secret non-leakage) + `internal/server/provider_login_test.go` (guards, modes, real-registry end-to-end with mock auth host + upstream, multi-account pool, restart persistence) | **mock-tested**. **key-configured** per installation after a live login/add. **real-provider-tested:** not yet — requires an operator OpenRouter account. `jwt` variant: **unsupported**. |
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
