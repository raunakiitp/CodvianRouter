# Provider account login and credential exchanges (Phase 1)

This document describes the provider authentication flows Codvian Router
implements itself, how to use them, and what is deliberately still
unsupported. Evidence tiers (`metadata-only`, `key-configured`,
`mock-tested`, `real-provider-tested`, `unsupported`) are defined in
[PROVIDER_AUTH_MATRIX.md](PROVIDER_AUTH_MATRIX.md); nothing here claims a
real-provider test that has not happened.

## 1. OpenRouter — OAuth PKCE account login

OpenRouter's official flow
(<https://openrouter.ai/docs/guides/overview/auth/oauth>) is PKCE-only and
returns a **user-controlled API key**, not an access/refresh token pair.
Codvian implements exactly that flow — no browser-cookie scraping, no
unofficial endpoints.

### CLI (headless, works over SSH)

```
codvian-router provider login openrouter
```

1. The router prints an authorization URL (`https://openrouter.ai/auth?...`
   with a fresh S256 `code_challenge` and `state`, `key_label=Codvian Router`).
2. You open it in a browser, sign in to OpenRouter and authorize. Because
   the CLI runs headlessly (no `callback_url`), OpenRouter **displays a
   single-use code** on screen (expires in 10 minutes).
3. You paste the code into the terminal. The router exchanges it at
   `POST https://openrouter.ai/api/v1/auth/keys` with the `code_verifier`
   that never left the process.
4. The returned API key goes through the **same registry path as a pasted
   key**: live-tested against OpenRouter (`/v1/models`), encrypted at rest
   (AES-256-GCM vault seal), model list discovered and persisted.
5. If an OpenRouter provider is already configured, the key is added to its
   **encrypted key pool as another account** (preferred role) instead of
   creating a duplicate provider. The router's per-key cooldowns/failover
   then rotate across accounts automatically.

### Local admin API (browser callback)

```
POST /admin/provider/login        (loopback-only, admin gateway key)
{"provider":"openrouter"}         → {authorization_url, state, expires_at, mode, callback_url}
```

- `mode` defaults to `callback` when a listen address is configured: the
  authorization URL carries
  `callback_url=http://127.0.0.1:<port>/admin/provider/login/callback`, so
  after authorizing, the browser lands back on the router and the key is
  stored automatically. `mode:"headless"` omits the callback (paste the
  displayed code via the CLI instead).
- Optional `key_label`, `limit` (USD, < 1,000,000, ≤ 2 decimals) and
  `expiration` (`1-hour` … `never`) prefill the key OpenRouter creates.
- `GET /admin/provider/login/callback?code=...&state=...` is loopback-only
  **without** a gateway key (a top-level browser redirect cannot carry
  one). Its credential is the single-use 256-bit `state` issued by the
  start route plus PKCE; off-host requests are refused 403 before anything
  else. The result is a small HTML page — no code, state, verifier or key
  is ever rendered or logged.

### Security properties

- Fresh 32-byte `code_verifier` per flow; only its SHA-256 challenge
  travels to OpenRouter. The verifier never appears in a URL, response,
  error or log.
- Fresh 256-bit `state` per flow, matched server-side, consumed exactly
  once; pending flows expire after 10 minutes and the pending map is
  bounded (64).
- The exchanged key is returned once to the storage path and never logged.
  Storage is the existing encrypted provider key pool; a key the upstream
  rejects is never persisted.
- **No refresh token exists in this flow.** The key lives until you revoke
  it or its chosen expiry passes; "refresh" means running the login again
  or rotating the key in the dashboard. The router does not pretend to
  renew a token OpenRouter never issued.

### Evidence

`internal/providerlogin/openrouter_test.go` (RFC 7636 appendix-B vector,
URL construction, headless mode, validation, single-use/expired/unknown
state, exchange error mapping, pending cap, secret non-leakage) and
`internal/server/provider_login_test.go` (guard tiers, modes, callback
happy path, bad flows, and a real-registry end-to-end test with a mock auth
host + mock upstream: live key test, encrypted pool, multi-account join,
restart persistence). All are **mock-tested**; a login with your own
OpenRouter account is the `real-provider-tested` step and is per
installation.

## 2. Azure OpenAI — Microsoft Entra ID client credentials

The catalog's `azure-openai` default surface (`/openai/v1`) accepts a
Microsoft Entra ID Bearer token. Codvian mints it with the OAuth2
client-credentials grant (RFC 6749 §4.4) — the provider-documented,
permitted service-to-service flow. The static `api-key` variant is
unchanged.

Store this JSON as the provider key (CLI `provider add-catalog azure-openai
'<json>' --base-url https://<resource>.openai.azure.com/openai/v1/`, or the
dashboard add form):

```json
{
  "tenant_id": "<directory tenant ID or domain>",
  "client_id": "<application (client) ID>",
  "client_secret": "<client secret value>",
  "token_uri": "optional sovereign-cloud/test override",
  "scope": "optional override, https resource scope ending /.default"
}
```

- Token endpoint default:
  `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token`; default
  scope `https://cognitiveservices.azure.com/.default`.
- The app registration needs the Azure OpenAI / Cognitive Services role
  (e.g. *Cognitive Services OpenAI User*) on the resource.
- The credential JSON is stored **encrypted** like any API key. The minted
  access token lives only in a bounded process-memory cache keyed by a
  SHA-256 fingerprint of the credential identity; it is re-minted shortly
  before expiry (60 s skew). Client-credentials grants issue **no refresh
  token** — expiry-aware re-minting is the refresh behavior.
- Repeated mint failures trip a breaker (5 failures → 30 min cooldown) so
  a revoked secret cannot hammer the token endpoint per request.
- `token_uri` overrides must be https (http only on loopback, for tests);
  `scope` overrides must be https `…/.default`. The tenant is validated as
  a URL path segment. Token-endpoint error bodies are never echoed (they
  can quote the secret); only the HTTP status and AAD error code surface.

Evidence: `internal/provider/entra_test.go` (parse validation, mint
form/scope, cache hit, expiry re-mint, breaker, secret non-leakage,
registry add → chat carries the minted Bearer, auth-cache expiry re-mint,
honest mint-failure refusal, api-key variant unaffected). **Mock-tested**;
a live Azure resource test is the `real-provider-tested` step.

## 3. Already implemented before Phase 1 (unchanged)

- **AWS Bedrock SigV4** — the client signs each request from the stored
  AWS credentials (`internal/provider/sigv4.go`).
- **Google Vertex AI service-account JSON** — RS256 JWT-bearer grant
  (RFC 7523), ~1 h token cache with expiry re-mint and a mint breaker
  (`internal/provider/vertex_client.go`).
- **Static-key providers** — endpoint-specific headers resolved from the
  catalog, live key test on add/rotate, encrypted multi-key pools with
  preferred/reserve roles and per-key failover, live model discovery and
  refresh.

## 4. Deliberately unsupported

- **Subscription/browser-session sign-in** (Claude Code, Codex, Copilot,
  Kiro, Cursor, Antigravity, web-cookie entries): the catalog marks these
  ToS-grey and the auth resolver refuses them as routing targets. Codvian
  does not scrape cookies or emulate consumer-app sessions. Revisit only
  with a provider-documented, permitted API flow.
- **OpenRouter `jwt` catalog variant**: metadata for short-lived signed
  JWTs; the implemented login uses the official PKCE→API-key flow on the
  default Bearer surface. The variant stays refused until a documented
  minting path is implemented and tested.
- Any other catalog exchange kind without an implemented, tested minting
  path: the resolver returns an honest exchange-required refusal — never a
  faked static header.
