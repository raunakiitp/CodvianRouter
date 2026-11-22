# Codvian Router — API Reference

Generated from the guard route table (`internal/server/guard.go`,
`guardRouteTable`) by `scripts/gen-api-docs.py` — do not hand-edit
the per-route sections; regenerate instead:

```sh
python3 scripts/gen-api-docs.py
```

Coverage: **326 routes** (169 GET, 108 POST, 22 DELETE, 22 PATCH, 5 PUT) —
11 `LOCAL_ONLY`, 56 `ALWAYS_PROTECTED`,
259 `MANAGEMENT`; 58 auth-exempt, 257 scope-gated, 11 key-only (gateway key, no scope).
Enforced by `TestAPIReferenceCoversEveryRoute`
(`internal/server/api_reference_test.go`): the test reads the guard
table and this doc and FAILS if any route is missing. That test is
the enforcement for the M8 exit gate.

## Auth

Every request except the auth-exempt routes below carries a gateway
key as `Authorization: Bearer <key>`. A key that authenticates but
lacks the route's scope gets 403; scope `admin` implies every scope.
`LOCAL_ONLY` routes are additionally reachable only from loopback
(real TCP peer address, never `Host` / `X-Forwarded-For`) — a valid
key over a non-loopback connection still gets 403. The one exception
is `POST /admin/bootstrap`, the fresh-install first-key route: it
takes no key because no key exists yet, and only answers while no
gateway key exists (a second call is 409).

Destructive routes additionally require a typed confirmation phrase
in the JSON body (`{"confirm": "<phrase>"}`) — see the individual
sections. The phrase is part of the request; a key alone cannot
perform the action.

## The `/openapi.json` gap (read before trusting the spec alone)

`GET /openapi.json` is generated live from the same guard table and
its consistency is enforced by `TestOpenAPIConsistency` — every route
is present, no stale operations, and the `x-loopback-only` /
`x-always-protected` / `x-spawn-capable` extensions match the tier.
What it does NOT carry, and this doc exists to supply:

- **Scopes.** The spec carries no structured scope information —
  no scopes field, no security scheme: a client cannot learn from
  `/openapi.json` that `GET /v1/models` needs scope `models` or that
  the dashboard mutating POSTs need `admin`. (A few one-line
  summaries mention the word "scope" in prose — e.g. "(admin scope)"
  — but prose is not a machine-readable scope; the gap is about the
  missing field, and `TestOpenAPIGapIsDocumented` checks for the
  field, not the substring.)
- **Auth-exempt flag.** Nothing in the spec says which routes need
  no key (`/healthz`, `/openapi.json`, the dashboard feeds).
- **Request/response schemas.** Operations carry only a one-line
  summary; there are no schemas for bodies or responses.

So: use `/openapi.json` for the route list and tier extensions, and
use this doc (or `docs/llm.txt`) for scopes, auth, and shapes.

## Error envelope

All errors are JSON with the OpenAI-shaped envelope
(`writeError` / `writeServerError` / `writeUpstreamError` in
`internal/server/server.go`):

```json
{"error": {"message": "method not allowed", "type": "invalid_request_error"}}
```

`type` is `invalid_request_error` for client errors, `server_error`
for server failures, `upstream_error` for upstream refusals.
`401` unauthenticated, `403` wrong scope or non-loopback on a
`LOCAL_ONLY` route, `404` unknown route/id, `501` honest
not-implemented stub (four Tier 1 routes — see below).

## Try it

`web/tryit.html` is a standalone try-it page served from the same
router (same design system as the dashboard). It embeds the same
route table as JSON and sends real requests to the router it was
served from — never to a live provider. Paste a gateway key into
its key field; the key is kept only in page memory, never stored.


## Health, metrics & meta

### `GET /healthz`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Liveness probe — no data, no auth.
- **Request:** no body.
- **Response (example):**

  ```json
  {"status": "ok"}
  ```

  Source: Exact body, verified by TestHealthz (internal/server/server_test.go) and handleHealthz.

  ```sh
  curl -X GET http://127.0.0.1:8080/healthz
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /openapi.json`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** OpenAPI spec for the management surface, generated live from this route table. Every operation carries x-loopback-only / x-always-protected / x-spawn-capable extensions matching its tier — the consistency check is TestOpenAPIConsistency.
- **Request:** no body.
- **Response (example):**

  ```json
  {
    "openapi": "3.0.3",
    "info": {"title": "Codvian Router API", "version": "<version>"},
    "servers": [{"url": "http://127.0.0.1:<port>"}],
    "paths": {"/healthz": {"get": {"summary": "...", "x-loopback-only": true}}}}
  Note: x-loopback-only / x-always-protected / x-spawn-capable extensions only — no scopes, no auth-exempt flag, no schemas (see the /openapi.json gap note at the top of this doc).
  ```

  Source: Shape from internal/server/openapi.go (openAPISpec).

  ```sh
  curl -X GET http://127.0.0.1:8080/openapi.json
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /metrics`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Prometheus exposition — gateway-key auth required.
- **Request:** no body.
- **Response:** Prometheus exposition text (not JSON).

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/metrics
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1beta/models`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Gemini-native model list — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1beta/models
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1beta/models/{rest...}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Gemini-native generateContent / streamGenerateContent — gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1beta/models/{rest...}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /a2a`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** A2A JSON-RPC endpoint (message/send, message/stream, tasks/get, tasks/cancel + v1.0 SendMessage/SendStreamingMessage aliases) — gateway-key auth required, owner-scoped to the caller's key; disabled unless config a2a_enabled is true.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/a2a
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /.well-known/agent.json`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** A2A agent card (discovery document for the POST /a2a task interface): name, description, url, version, input/output modes, capabilities and the six skill descriptors — public, no auth; no task data, no keys, no provider details.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/.well-known/agent.json
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /oauth/clients`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Registers one OAuth 2.0 client (PKCE public or confidential) for gateway auth; a confidential client's secret is returned once in the response and never logged. Auth always required — it creates a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/oauth/clients
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /oauth/clients`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Lists registered OAuth clients (no secrets). Auth always required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/oauth/clients
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /oauth/clients/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Deletes one OAuth client and revokes its outstanding codes and refresh tokens. Auth always required — it destroys a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/oauth/clients/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /oauth/authorize`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** OAuth 2.0 authorization endpoint (code + PKCE S256): the operator's gateway key approves the grant; the granted scopes must be a subset of the operator key's scopes. Issues a single-use code by 302 redirect to the client's registered redirect_uri. Auth always required — it mints a credential.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/oauth/authorize
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /oauth/token`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** OAuth 2.0 token endpoint: authorization_code (PKCE) and refresh_token grants; issues HS256 JWT access tokens (1h) + rotating refresh tokens (30d). No gateway key — client authentication is the code_verifier and the client secret.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST http://127.0.0.1:8080/oauth/token
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /oauth/revoke`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** OAuth 2.0 revocation endpoint (RFC 7009): revokes a refresh token or a JWT access token; always answers 200, even for a token that was never issued. No gateway key.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST http://127.0.0.1:8080/oauth/revoke
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

## OpenAI-compatible API (`/v1/*`)

### `GET /v1/models`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Model list — gateway-key auth required.
- **Query:** `include=deprecated` — re-lists retired models, with `deprecated: true` set.
- **Request:** no body.
- **Response (example):**

  ```json
  {
    "object": "list",
    "data": [
      {
        "id": "<model-id>",
        "object": "model",
        "created": 0,
        "owned_by": "codvian-router"
      }
    ]
  }
  ```

  Source: Shape from internal/server/server.go Model struct (id, object, created, owned_by, deprecated?, provider_id?). List each model id once; ?include=deprecated re-lists retired rows.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/models
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/models/query`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Model list filtered by provider and free-tier-only; capability filter answers 400 — the catalog records no per-model capability flags. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/models/query
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/models/health`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Per-model health from the health-probe ring — each model's status is its provider's last probe result; unprobed models report unknown. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/models/health
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/models/{model...}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Single model detail — catalog entry for one model (price, base-URL presence, free-tier flag); unknown models answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/models/{model...}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/providers/{provider}/chat/completions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Chat completions pinned to one provider — gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/providers/{provider}/chat/completions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/providers/{provider}/models`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Model list for one provider — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/providers/{provider}/models
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/providers/{provider}/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Status for one provider (configured, key count, breaker, cooling) — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/providers/{provider}/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/chat/completions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Chat completions — gateway-key auth required.
- **Request body (JSON):**

  ```json
  {
    "model": "<model-id>",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false,
    "max_tokens": 256,
    "temperature": 0.7
  }
  ```

  Source: Fields from internal/provider/client.go chatCompletionRequest (model, messages, stream, max_tokens, temperature, top_p, reasoning_effort, stop, tools, tool_choice, prompt_cache_key).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"model": "<model-id>", "messages": [{"role": "user", "content": "Hello"}], "stream": false, "max_tokens": 256, "temperature": 0.7}' http://127.0.0.1:8080/v1/chat/completions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/embeddings`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Embeddings — OpenAI-compatible, routed through the same engine as chat completions (budgets, guardrails, ledger, route trace) but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body (JSON):**

  ```json
  {
    "model": "<model-id>",
    "input": "Hello, world",
    "dimensions": 1536
  }
  ```

  Source: Fields from internal/provider/embeddings.go EmbeddingRequest (model, input, dimensions, encoding_format, user).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"model": "<model-id>", "input": "Hello, world", "dimensions": 1536}' http://127.0.0.1:8080/v1/embeddings
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/multimodal-embeddings`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Multimodal embeddings (text + image/media inputs) — routed through the engine, replay-exempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/multimodal-embeddings
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/rerank`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Rerank — Cohere/LiteLLM shape (model, query, documents, top_n, return_documents), routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body (JSON):**

  ```json
  {
    "model": "<model-id>",
    "query": "What is Codvian Router?",
    "documents": ["doc one", "doc two"],
    "top_n": 1,
    "return_documents": true
  }
  ```

  Source: Fields from internal/provider/embeddings.go RerankRequest (model, query, documents, top_n, return_documents).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"model": "<model-id>", "query": "What is Codvian Router?", "documents": ["doc one", "doc two"], "top_n": 1, "return_documents": true}' http://127.0.0.1:8080/v1/rerank
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/completions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Legacy completions — gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/completions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/relay/chat/completions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Chat completions explicitly routed with context-relay strategy. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/relay/chat/completions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/classify`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Routing classification — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/classify
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/explain/routing`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Routing explanation — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/explain/routing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/segment`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Prompt segmentation + classification — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/segment
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/images/generations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Images generations — OpenAI-compatible, routed through the same engine as chat completions (budgets, guardrails, ledger, route trace) but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/images/generations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/images/edits`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Images edits — OpenAI-compatible multipart/form-data, forwarded verbatim (never decoded and rebuilt), routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/images/edits
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/speech`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Audio speech (text-to-speech) — OpenAI-compatible, routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/speech
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/text-to-speech`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Alias of POST /v1/audio/speech — not an OpenAI endpoint; gateway route name, same handler and operation. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/text-to-speech
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/text-to-speech/{voiceId}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Text-to-speech with the voice id in the URL path: the id is written into the JSON body's voice field (a conflicting body voice is refused 400), then the request follows POST /v1/audio/speech. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/text-to-speech/{voiceId}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/transcriptions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Audio transcriptions (speech-to-text) — OpenAI-compatible multipart upload, replay-exempt: exactly one upstream attempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/transcriptions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/speech-to-text`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Alias of POST /v1/audio/transcriptions — not an OpenAI endpoint; gateway route name, same handler and operation. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/speech-to-text
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/audio/translations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Audio translations (translate audio to English) — OpenAI-compatible multipart upload, replay-exempt: exactly one upstream attempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/audio/translations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/moderations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Moderations — OpenAI-shaped, backed ONLY by the router's local guardrails (PII / secret / injection patterns); no upstream call, no input stored or echoed. The 13 OpenAI categories are not judged (all false, score 0); the real verdict is in results[].local_guardrails and results[].flagged reflects it. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/moderations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/search`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Search — Perplexity Search API passthrough, verbatim, replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/search
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/web/fetch`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Fetches and extracts clean text from a web URL with SSRF protection. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/web/fetch
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/ocr`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** OCR — Mistral OCR API passthrough, verbatim, replay-exempt: exactly one upstream attempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/ocr
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/videos/generations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Video generations — Runway text-to-video passthrough, returns a task id; replay-exempt: exactly one upstream attempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/videos/generations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/videos/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Video task status — Runway task poll by id; unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/videos/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/music/generations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Music generations — MiniMax music generation passthrough, synchronous; replay-exempt: exactly one upstream attempt. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/music/generations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/messages`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Anthropic Messages dialect — byte-for-byte native relay to a Messages provider (only the model field rewritten; cache_control and the SSE envelope pass through untouched), translate path through the engine when a feature needs the canonical form. Gateway-key auth required.
- **Request body (JSON):**

  ```json
  {
    "model": "<model-id>",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello"}]
  }
  ```

  Source: Anthropic Messages shape, relayed byte-for-byte (only the model field is rewritten) — see guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"model": "<model-id>", "max_tokens": 256, "messages": [{"role": "user", "content": "Hello"}]}' http://127.0.0.1:8080/v1/messages
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/messages/count_tokens`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Token count for a Messages request body — counted locally, never sent upstream. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/messages/count_tokens
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/responses`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** OpenAI Responses API — translated to the canonical chat request and routed through the same engine as chat completions (failover, budgets, guardrails, ledger); creates fail over pre-commit, including branched continuations, which replay the same branched input against the next target (the original provider is tried first). Gateway-key auth required.
- **Request body (JSON):**

  ```json
  {
    "model": "<model-id>",
    "input": "Hello"
  }
  ```

  Source: OpenAI Responses shape, translated to the canonical chat request — see guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"model": "<model-id>", "input": "Hello"}' http://127.0.0.1:8080/v1/responses
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/responses/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Retrieve one served Responses object by gateway response id — the stored object, byte-identical to what the create served, plus child_response_ids (the branches off it, from the ledger tree); unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/responses/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/responses`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List served Responses objects, newest first, paginated (limit + after cursor) — the full stored objects, byte-identical to what each create served; an unknown cursor is a 400. Gateway-key auth required.
- **Query:** `limit` + `after` cursor — newest first, paginated; an unknown cursor is a 400.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/responses
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /v1/responses/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Deletes one stored response (transcript + served object) so continuations of it answer 404; deleting an unknown id is an honest 404, never a silent success. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/responses/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/responses/{id}/input_items`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Input items of one stored Responses object, paginated (limit + after cursor, asc/desc); unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/responses/{id}/input_items
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/responses/input_tokens`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Token count for a stored response's input — counted locally, never sent upstream. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/responses/input_tokens
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/files`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Upload a file (multipart form-data: file + purpose) to the local disk store; returns the OpenAI-shaped file object.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/files
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/files`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List stored files, newest first.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/files
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/files/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Retrieve one stored file's metadata by id; unknown ids answer 404.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/files/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /v1/files/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete one stored file (content + index entry); unknown ids answer 404, never a silent success.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/files/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/files/{id}/content`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Download one stored file's bytes; unknown ids answer 404.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/files/{id}/content
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/batches`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Create a batch from an input file id (endpoint must be /v1/chat/completions, completion_window must be 24h); returns the Batch object in status validating.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/batches
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/batches`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List the caller's batches, newest first (?limit= default 20, max 100).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/batches
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/batches/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Retrieve one batch by id; unknown or foreign ids answer 404.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/batches/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/batches/{id}/cancel`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Cancel a running batch: cancelling -> cancelled, the worker stops starting new lines; already-terminal batches answer 409.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/batches/{id}/cancel
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /v1/batches/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete one terminal batch record; non-terminal batches answer 409, unknown or foreign ids answer 404.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/batches/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /v1/batches/delete-completed`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete all of the caller's terminal (completed/cancelled/failed/expired) batch records; in-progress batches are never deleted.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/batches/delete-completed
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/me/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Authenticated gateway-key introspection: the caller's key id/fingerprint, created/expiry, spend this period, budget remaining, traffic limits and live usage — never the key value itself. Gateway-key auth required; an expired key answers 401 naming the expiry.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/me/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/me/sessions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List agent sessions opened by the calling API key: Responses chains (one row per session root, no previous_response_id, with turns, cumulative tokens and the list-price cost of its requests) plus ledger agent sessions (one row per coding-agent session, with request/error counts, token split and record-time cost). unpricedCount counts turns/requests with no known catalog price. Every row is filtered to the calling key — other keys' sessions are never listed. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/me/sessions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/me/sessions/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One agent session of the calling API key: a Responses session (the session root's turns, cumulative tokens and list-price cost, plus the per-turn detail) or a ledger agent session (request/error counts, token split, record-time cost, no per-turn detail); unknown ids, other keys' sessions and keyless records answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/me/sessions/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/quotas/check`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `chat` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Pre-flight quota/budget check for one (provider, model, estimated tokens) request: allowed plus the budget, token-limit and price numbers the verdict was judged against, from the same ledger machinery that enforces live requests. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/quotas/check
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/auto-combo/{channel}/candidates`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Auto-combo candidate pool for one auto/* channel: live tier models with per-key connections and breaker/cooldown state — a read-only transparency listing, never a dispatch preview. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/auto-combo/{channel}/candidates
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/combos`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List named routing combos (chains): one row per combo with steps, strategy and usability. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/combos
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Create a named routing combo (ordered provider/model steps + strategy), mirrored into the routing groups so it routes immediately. Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/combos/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One named routing combo by id; unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/combos/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /v1/combos/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Update one named routing combo (chain, strategy, rename). Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/combos/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /v1/combos/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete one named routing combo (record + group mirror + strategy mirror). Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos/import`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Imports named routing combos from an export file (one at a time); imported combos are mirrored into routing groups like created ones. Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/import
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/combos/export`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Exports all named routing combos in the importable shape. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/combos/export
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /v1/combos/{id}/export`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Exports one named routing combo in the importable shape; unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/v1/combos/{id}/export
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos/{id}/duplicate`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Duplicates one named routing combo under a new id (disabled copies stay unmirrored). Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/{id}/duplicate
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos/{id}/test-run`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `models` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Simulates one combo run step by step (provider key presence per step); never calls upstream, never spends quota. Gateway-key auth required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/{id}/test-run
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos/{id}/enable`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Enables one named routing combo and re-mirrors its stored chain + strategy. Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/{id}/enable
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /v1/combos/{id}/disable`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Disables one named routing combo (unmirrors it from routing). Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/v1/combos/{id}/disable
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

## Management API (`/api/*`)

### `GET /api/mcp/audit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** MCP tool-call audit (inputs SHA-256-hashed, outputs truncated to 200 chars — no raw arguments), paginated, newest first. Entries are redacted as stored, so the feed cannot leak a secret.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/mcp/audit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/mcp/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** MCP remote-transport toggles + live session counts — flags and counts only, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/mcp/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/mcp/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Enables/disables one MCP remote transport (sse/streamable). Local-operator dashboard action on the localhost-bound server — carries no secrets; the transports themselves are served by M9.2.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/mcp/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/mcp/sse`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** MCP SSE transport: opens the server-to-client event stream (first event is the endpoint carrying the session id). Gateway-key auth; the mcp: scopes on the key govern tool calls inside.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/mcp/sse
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/mcp/sse`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** MCP SSE transport: sends one JSON-RPC message for a session opened by GET; answers 202, the response arriving on the stream. Gateway-key auth; the mcp: scopes on the key govern tool calls inside.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/mcp/sse
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/mcp/stream`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** MCP Streamable HTTP transport: one JSON-RPC message per POST; an initialize opens a session (Mcp-Session-Id header). Gateway-key auth; the mcp: scopes on the key govern tool calls inside.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/mcp/stream
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/mcp/stream`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** MCP Streamable HTTP transport: opens the server-to-client stream for a session (closing it does not terminate the session). Gateway-key auth; the mcp: scopes on the key govern tool calls inside.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/mcp/stream
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/mcp/stream/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** MCP Streamable HTTP transport: terminates one session (204; unknown or foreign ids are 404). Gateway-key auth; the mcp: scopes on the key govern tool calls inside.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/mcp/stream/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/providers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live provider feed — no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/providers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/providers`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Adds a provider: tests the supplied key live against the upstream, encrypts and persists it, and adds it to the live registry with no restart. Auth always required — an unauthenticated add could redirect future LLM traffic to an attacker-chosen base URL.
- **Request body (JSON):**

  ```json
  {
    "catalog_id": "<catalog-provider-id>",
    "api_key": "<provider-api-key>",
    "base_url": "",
    "endpoint": "",
    "variant": "",
    "name": "",
    "models": []
  }
  ```

  Source: Fields from internal/server/providers_add.go addProviderRequest (catalog_id, api_key, base_url, endpoint, variant, name, models). Note: models is accepted, validated, then NOT stored — the live model list comes from the /v1/models probe (see that file).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"catalog_id": "<catalog-provider-id>", "api_key": "<provider-api-key>", "base_url": "", "endpoint": "", "variant": "", "name": "", "models": []}' http://127.0.0.1:8080/api/providers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/providers/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Removes a provider: deletes it from the live registry and the config file, drops its live engine client, unregisters its models from the auto-router tiers (only models no remaining provider serves), and prunes its probe history. Auth always required, plus the typed confirmation phrase DELETE <id> in the JSON body — an unauthenticated delete could take a provider offline.
- **Request body (JSON):**

  ```json
  {
    "confirm": "DELETE <id>"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description (<id> is the provider id in the path).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "DELETE <id>"}' http://127.0.0.1:8080/api/providers/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/providers/{id}/keys`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Adds an API key to a provider's key pool: tests the key live against the upstream, then encrypts and persists it. Auth always required — it plants a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/providers/{id}/keys
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/providers/{id}/keys/{keyId}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Removes one API key from a provider's key pool (the last key cannot be removed — remove the provider instead). Auth always required — it drops a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/providers/{id}/keys/{keyId}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/providers/{id}/keys/{keyId}/role`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Changes one provider key's role (preferred/reserve). Auth always required — it changes which credential routes.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/providers/{id}/keys/{keyId}/role
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/providers/{id}/keys/{keyId}/rotate`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Rotates one provider key's credential in place: tests the replacement key live against the upstream, then replaces the stored ciphertext keeping the key's ID and role. Auth always required — it plants a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/providers/{id}/keys/{keyId}/rotate
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/providers/{id}/enabled`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Enables or disables a provider: a disabled provider stops routing (the dashboard shows it as Not connected) and re-enabling returns it to the pool with no restart. Auth always required — it changes what routes.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/providers/{id}/enabled
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/catalog`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live provider catalog for the add-provider selector (id, name, verification, ToS-grey flag, base-URL availability) — catalog facts only, no keys, no base URLs.
- **Request:** no body.
- **Response (example):**

  ```json
  {
    "providers": [
      {
        "id": "<catalog-id>",
        "name": "<name>",
        "verification": "<verification>",
        "tos_grey": false,
        "has_base_url": true,
        "templated_base_url": false
      }
    ],
    "total": 0
  }
  ```

  Source: Shape from internal/server/providers_add.go catalogEntryView / catalogResponse (id, name, alias?, verification, tos_grey, has_base_url, templated_base_url; providers, total).

  ```sh
  curl -X GET http://127.0.0.1:8080/api/catalog
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/usage/budget`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Cost summary for one gateway key (apiKeyId query param): calendar limits, warning threshold, reset cadence, last/next reset, spend today / this month / this budget period, the active limit and the allow/warn/deny budget check, plus the rolling 5h cap. Management auth (admin scope) required.
- **Query:** `apiKeyId` — the gateway key id to report on.
- **Request:** no body.
- **Response (example):**

  ```json
  {
    "api_key_id": "<key-id>",
    "daily_limit_usd": 0,
    "warning_threshold": 0,
    "reset_interval": "",
    "reset_time": "",
    "total_cost_today": 0,
    "total_cost_month": 0,
    "active_limit_usd": 0,
    "budget_check": {"status": "allow", "reason": ""},
    "limit_usd": 0,
    "spent_usd": 0,
    "remaining_usd": 0,
    "at_limit": false
  }
  ```

  Source: Shape from internal/server/usageapi.go usageBudgetResponse (api_key_id, daily/weekly/monthly_limit_usd, warning_threshold, reset_interval, reset_time, budget_reset_at, next_reset_at, total_cost_today/month/period, active_limit_usd, budget_check, limit_usd, spent_usd, remaining_usd, at_limit).

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/usage/budget
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/usage/budget`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Set the calendar budget for one gateway key: daily/weekly/monthly limits, warning threshold, reset interval/time. Absent fields keep their stored values; the 5h cap and unpriced policy are untouched. Ledger-first, then config persist with rollback. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/usage/budget
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/summary`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Usage analytics summary for one calendar window: request/token/latency totals, priced spend + cost_status. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/summary
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/by-provider`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Usage analytics roll-up by provider for one calendar window, with priced cost and unpriced-request count. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/by-provider
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/by-model`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Usage analytics roll-up by model for one calendar window, with priced cost and unpriced-request count. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/by-model
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/costs`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Priced spend + unpriced remainder for one calendar window; deliberately no total-cost field. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/costs
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/compression`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Compression savings roll-up by provider for one calendar window (prompt tokens before compression, tokens saved, saving rate). Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/compression
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/diversity`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Traffic diversity for one calendar window: provider/model spread with Herfindahl concentration index. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/diversity
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/auto-routing`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Auto-routed traffic for one calendar window, grouped by variant, with serving providers. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/auto-routing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/combo-health`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Per-combo health for one calendar window: step liveness from the live provider config plus matching usage requests (upper bound — a row served through a combo is indistinguishable from one served directly). Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/combo-health
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/fallback/chains`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Fallback chains: every stored (model → provider priority list) chain, or one chain with ?model=. Provider and model names plus priorities only — no keys, no message content. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/fallback/chains
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/fallback/chains`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Creates or updates one fallback chain (model + ordered provider entries). Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/fallback/chains
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/fallback/chains`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Deletes one fallback chain by ?model=; 404 when the model has no chain. Management auth (admin scope) required — it rewrites routing state.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/fallback/chains
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/resilience`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Live resilience feed: breaker states + cooldowns per provider (unknown when unprobed); hedge counts honestly unknown. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/resilience
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/resilience/settings`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Live resilience settings: effective cooldown mode (global + per-model overrides), cooling targets and breaker states from the engine, plus the list of settings with no runtime backing. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/resilience/settings
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/resilience/settings`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Updates resilience settings: cooldown mode (global + per-model overrides) is applied live and persisted; breaker_threshold, breaker_open_for_seconds and cooldown_seconds are validated for range first (400 on a bad range) and then answer 501 naming the field — they have no runtime backing. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/resilience/settings
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/resilience/breakers/reset`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Resets one provider's circuit breaker, or all breakers with {"all": true}; unknown provider is 404. A real reset against the production engine — router.Engine implements ResetBreaker/ResetAllBreakers (internal/router/router.go) — 501 only when the engine lacks the breaker-reset interface. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/resilience/breakers/reset
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/resilience/cooldowns`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Sets one model's cooldown mode (hard / soft-demote); an empty mode is 400 — clearing an override is DELETE /api/resilience/cooldowns/{model...}. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/resilience/cooldowns
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/resilience/cooldowns/{model...}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Clears one model's cooldown-mode override so it inherits the global mode again; idempotent — a model with no override still answers 200. The model id uses the multi-segment wildcard because model ids contain a slash. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/resilience/cooldowns/{model...}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Recent server log lines, double-redacted, newest first (?limit, default 50, max 500); captured:false with an empty list until log capture is wired. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/guardrails/test`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Dry-run probe of the local guardrails against sample text: findings + would-block verdict; records nothing, sends nothing upstream. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/guardrails/test
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/teams`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List team cost centers with live budget/member/consumption views. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/teams
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/teams`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Create a team cost center (name, budget, duration, members). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/teams
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/teams/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One team cost center by id; unknown ids answer 404. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/teams/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/teams/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Update one team cost center (budget, members, status). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/teams/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/teams/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete one team cost center. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/teams/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/quota-pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** List fair-share quota pools with live pool-wide consumption. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/quota-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/quota-pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Create a fair-share quota pool (dimensions, allocations, saturation threshold). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/quota-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/quota-pools/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One fair-share quota pool by id; unknown ids answer 404. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/quota-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/quota-pools/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Update one fair-share quota pool (dimensions, allocations). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/quota-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/quota-pools/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Delete one fair-share quota pool. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/quota-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/webhooks`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Registers a webhook URL + event list (quota.exceeded, budget.warning): validates the URL (http/https only, no private/loopback targets), mints a wh_ id, persists to the config file and rebuilds the live dispatcher. Auth always required — it points the server at an operator-chosen URL and stores a signing secret.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/webhooks
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/webhooks`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Lists webhook registrations (id, url, events, has_secret only — secrets never returned). Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/webhooks
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/webhooks/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One webhook registration by id (secret never returned); unknown id is 404. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/webhooks/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/webhooks/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Removes a webhook registration, persists the config file and rebuilds the live dispatcher; unknown id is 404. Auth always required — it silently drops an operator alert channel.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/webhooks/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PUT /api/webhooks/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Updates a webhook registration (partial update: url, events and secret are each optional, only provided fields change; the URL is re-validated when changed). Persists to the config file and rebuilds the live dispatcher; unknown id is 404. Auth always required — it can re-point an operator alert channel.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PUT -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/webhooks/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/usage/om-usage`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Terminal-friendly usage command for the calling gateway key (@@om-usage): spend today / this week / this month, the active limit and next reset — plain text by default, JSON with ?format=json. Only for keys the operator enabled allow_usage_command on (403 otherwise). Gateway-key auth required.
- **Query:** `format=json` — JSON instead of the default plain text.
- **Request:** no body.
- **Response:** plain text by default, JSON with `?format=json` (spend today / this week / this month, active limit, next reset).

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/usage/om-usage
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/cache/stats`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live cache analytics — per-layer entries/bytes/hits/misses/hit-ratio for the exact + semantic caches, totals, single-flight coalescing counters, and aggregated savings (tokens + USD) when a savings recorder is wired; savings omitted with a note when not. Counts only, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/cache/stats
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/telemetry/model-latency-stats`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Per-(provider, model) measured latency percentiles p50/p95/p99 from recorded request samples, with sample counts; optional ?provider= / ?model= exact-match filters. Read-only counts and latencies, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/telemetry/model-latency-stats
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/pricing`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Operator price book: per-(provider, model) rates with their source (ledger, book, synced, model fallback). Read-only, rates and sources only, no secrets.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/api/pricing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/pricing`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Replaces price-book rates for (provider, model) pairs (all-or-nothing, validated, persisted to the config file with rollback). Auth always required — it changes what every request is charged.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/pricing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/cache`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Flushes every cache layer (exact, semantic, reasoning) and reports per-layer evicted counts. Auth always required — it discards cached answers.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/cache
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/a2a/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** A2A operator status: enabled flag, live per-state task counts (operator-only aggregates across all owners), the six skill ids and the protocol version. Management auth (admin scope) required; while A2A is disabled it answers enabled:false with all-zero counts and never touches the store.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/a2a/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/features`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these feature flags were not wired to the features they name). Use instead: the config file (each feature's own setting).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/features
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/features`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these feature flags were not wired to the features they name). Use instead: the config file (each feature's own setting).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/features
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/cors`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (CORS origin settings are not implemented: the stored list was never applied).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/cors
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/cors`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (CORS origin settings are not implemented: the stored list was never applied).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/cors
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/blocked-providers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (the stored blocked-provider list was never applied). Use instead: POST /api/providers/{id}/enabled (disable a provider).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/blocked-providers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/blocked-providers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (the stored blocked-provider list was never applied). Use instead: POST /api/providers/{id}/enabled (disable a provider).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/blocked-providers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/ai`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these AI defaults were never applied to requests).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/ai
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/ai`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these AI defaults were never applied to requests).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/ai
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/modality`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (modality bridge settings are not implemented).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/modality
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/modality`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (modality bridge settings are not implemented).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/modality
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/pricing/sync`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (this route reported a sync without fetching any prices). Use instead: PATCH /api/pricing (set rates) or GET /api/pricing (current book).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/pricing/sync
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/pricing/sync/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (no pricing sync runs, so there is no status). Use instead: GET /api/pricing.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/pricing/sync/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/transform`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (the transform pipeline is not implemented: stored operations were never applied).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/transform
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PUT /api/settings/transform`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (the transform pipeline is not implemented: stored operations were never applied).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PUT -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/transform
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/sidebar`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (sidebar customisation is not implemented: the dashboard never read it).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/sidebar
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PUT /api/settings/sidebar`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (sidebar customisation is not implemented: the dashboard never read it).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PUT -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/sidebar
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/advanced`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these advanced settings (body cap, lockout, aliases) were never applied).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/advanced
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/advanced`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these advanced settings (body cap, lockout, aliases) were never applied).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/advanced
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/lkgp/cache`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Clears LKGP learned routing cache (B8-69).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/lkgp/cache
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/routing/strategies`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Lists all supported routing strategies (B8-35..56).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/routing/strategies
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/stickiness`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (session stickiness settings are not implemented: stored values were never applied).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/stickiness
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/stickiness`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (session stickiness settings are not implemented: stored values were never applied).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/stickiness
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/switches`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these global switches were not wired to anything).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/switches
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/switches`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (these global switches were not wired to anything).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/switches
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/response-validation`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (response validation rules are not implemented: stored rules were never evaluated).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/response-validation
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PUT /api/settings/response-validation`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (response validation rules are not implemented: stored rules were never evaluated).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PUT -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/response-validation
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/cache/{model}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Evicts cache entries for a specific model.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/cache/{model}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/keys/{id}/devices`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Device tracking per key (B8-87).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/keys/{id}/devices
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/ip-filter`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (a global IP filter is not implemented: the stored lists filtered nothing). Use instead: per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/ip-filter
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/ip-filter`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (a global IP filter is not implemented: the stored lists filtered nothing). Use instead: per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/ip-filter
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/cost-explorer`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Cost explorer analytics (B8-19).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/cost-explorer
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/spend-projection`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Monthly spend projection (B8-18,103).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/spend-projection
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/budget-table`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Key budget tracking table (B8-105).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/budget-table
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/cost-by-key`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Spend grouped by gateway key (B8-113).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/cost-by-key
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/cost-by-account`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (spend is not partitioned by provider account in the ledger). Use instead: GET /api/analytics/cost-by-key.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/cost-by-account
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/weekly-pattern`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Weekly usage patterns (B8-115).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/weekly-pattern
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/heatmap`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Activity heatmap (B8-115).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/heatmap
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/utilization`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Provider quota utilization (B8-118).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/utilization
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/search`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Search analytics (B8-121).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/search
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/free-rankings`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Free model rankings (B8-108).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/free-rankings
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/scoring-inspector`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** 16-factor scoring breakdown (B8-78).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/scoring-inspector
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/analytics/candidates`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Candidate targets for auto-combo (B8-82).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/analytics/candidates
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/proxy`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (no proxy log is recorded). Use instead: GET /dashboard/logs (ledger-backed request log).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/proxy
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/console`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (no console feed is recorded here). Use instead: GET /api/logs (captured server log lines).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/console
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/timeline/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (per-request step timelines are not recorded). Use instead: GET /dashboard/trace (route trace of the last request).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/timeline/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/activity`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (no activity feed is recorded). Use instead: GET /dashboard/logs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/activity
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/conversations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (conversation transcripts are not recorded here). Use instead: GET /v1/responses (stored Responses API conversations).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/conversations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/compression`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (no per-request compression log is recorded). Use instead: GET /api/analytics/compression.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/compression
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/audit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (this audit feed was never written). Use instead: GET /dashboard/audit.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/audit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/export`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Export logs JSON (B8-126).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/export
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/destinations`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented: nothing shipped logs to them). Use instead: GET /api/logs/export.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/destinations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/logs/destinations`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/logs/destinations
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/logs/destinations/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/destinations/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/logs/destinations/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/logs/destinations/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/logs/destinations/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/logs/destinations/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/combos/{id}/capabilities`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (capability-specific combo behaviour is not implemented).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/combos/{id}/capabilities
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PUT /api/combos/{id}/capabilities`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (capability-specific combo behaviour is not implemented).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PUT -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/combos/{id}/capabilities
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/combo-tuning`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (combo tuning settings are not implemented: stored values never reached the router).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/combo-tuning
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/combo-tuning`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (combo tuning settings are not implemented: stored values never reached the router).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/combo-tuning
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/scoring-modes`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Scoring factors and mode packs (B8-77).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/scoring-modes
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/settings/account-fallback`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (account fallback strategy selection is not implemented).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/account-fallback
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/settings/account-fallback`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (account fallback strategy selection is not implemented).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/settings/account-fallback
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/combos/{id}/studio-cascade`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Live cascade view for combo studio (B8-83).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/combos/{id}/studio-cascade
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/eval/schedules`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Lists recurring evaluation schedules (id, suite, interval, enabled, regression threshold). Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/eval/schedules
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/eval/schedules`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Creates a recurring evaluation schedule. Management auth (admin scope) required — it starts periodic evaluation work.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/eval/schedules
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/eval/schedules/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** One evaluation schedule by id; unknown ids answer 404. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/eval/schedules/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /api/eval/schedules/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Partial update of one schedule (interval, enabled, regression threshold, concurrency). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/eval/schedules/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /api/eval/schedules/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Deletes one schedule (its history is preserved); unknown ids answer 404. Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/eval/schedules/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/eval/schedules/{id}/history`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Run history for one schedule, newest first (?limit=, default 100). Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/eval/schedules/{id}/history
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/org`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Lists organizations and teams (id, name, budget, allowed models/strategies, roles). Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/org
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/org`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Creates (bootstraps) an organization. Management auth (admin scope) required — it creates a tenant.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/org
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/org/teams`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Creates a team under an org. Management auth (admin scope) required — it changes tenant isolation.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/org/teams
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/org/teams/{id}/bind`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Binds a gateway key to a team (team-key isolation + team budget). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/org/teams/{id}/bind
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /api/org/teams/{id}/unbind`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Removes a key's team binding (isolation off). Management auth (admin scope) required.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/api/org/teams/{id}/unbind
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /api/org/audit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Append-only admin audit log, newest first (?limit=, default 200). Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/api/org/audit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

## Dashboard feeds & actions (`/dashboard/*`)

### `GET /dashboard`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Static dashboard UI — no live data of its own.
- **Request:** no body.
- **Response:** the static dashboard HTML page (web/index.html).

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/auth/setup`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Sets the dashboard operator password for the first time (only while none is set); admin gateway key required. The password is stored only as an Argon2id hash in the data dir; no default password exists.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/auth/setup
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/auth/login`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Operator login with the dashboard password; on success sets an HttpOnly session cookie (12h). Failed attempts are throttled per peer (5 failures → 60s lockout).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/auth/login
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/auth/logout`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Invalidates the dashboard session cookie; 401 when the caller has neither a session nor a gateway key.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/auth/logout
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/auth/change`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Changes the dashboard operator password; requires a valid session plus the current password, or an admin gateway key. All sessions are invalidated.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/auth/change
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/providers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live provider facts (verification, wire, health) — no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/providers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/overview`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live overview KPIs + traffic buckets from the ledger/recorder — counts and totals only, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/overview
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/models`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live model catalog from the registry + per-model request counts — no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/models
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/catalog-audit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Last committed catalog freshness audit vs live models.dev (audit date + drift counts) — public catalog facts only, no keys, no operator-configured providers.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/catalog-audit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/release-verify`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Last recorded `release verify` run (verified tag, archive/SBOM counts, bundle status) — the committed snapshot proof only, never a live re-run or an invented pass.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/release-verify
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/models/refresh`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Re-probes every provider's model list live and returns one result per provider (ok / failed / skipped, with model counts and reasons); a failed provider keeps its last known list. Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/models/refresh
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/logs`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Recent request attempts from the ledger (provider, model, tokens, cost, latency, outcome) — no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/logs
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/usage`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live spend/token roll-up from the ledger — totals only, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/usage
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/keys`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Gateway-key facts from the config (name, scopes, budget, created) — masked values only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/keys
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/auto-route`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live auto-router classification for a typed prompt — no provider is called.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/auto-route
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/auto-route/last`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Last real auto/* routing decision, or an honest empty state.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/auto-route/last
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/feedback`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Feedback-learned routing weights: per-tier signal tallies + nudges + recent signals — counts only, no prompts, no answers, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/feedback
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/feedback`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Records one accept/edit/discard/regenerate signal against a served routing decision id (decision id + signal only; no content). Local-operator dashboard action on the localhost-bound server.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/feedback
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/learned`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Learned auto-router status: off / learning / active, evidence count, held-out calibration error + log-loss, per-tier sample counts — counts only, no prompts, no answers, no message content, never a fake accuracy.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/learned
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/intent-tiers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Intent-tier mappings (fast / balanced / best) to their operator-defined (model, effort) pairs — model names and effort levels only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/intent-tiers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/intent-tiers`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Sets one intent tier's (model, effort) mapping at runtime (tier, model and effort names only). Local-operator dashboard action on the localhost-bound server — carries no secrets; applies at runtime only, never to the config file.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/intent-tiers
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/suggestions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Metadata-only learning suggestions: pending routing suggestions with their real evidence counts, per-(task, model) latency/success stats and the applied-change log — counts and model names only, no prompts, no answers, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/suggestions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/suggestions`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Approves (applies) or reverts one routing suggestion by id — an explicit operator action recorded in the applied-change log. Local-operator dashboard action on the localhost-bound server — carries no secrets; applies at runtime only, never to the config file.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/suggestions
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/code-outcomes`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Auto Code mode evidence: per-model recorded coding-task outcomes (attempts, solved credit, measured solve rate or the documented neutral default) — counts only, no prompts, no answers, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/code-outcomes
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/coalescing`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Single-flight counters only — no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/coalescing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/reasoning`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Reasoning replay counters (captures, restores, hits, misses, evictions) — counts only, never reasoning content, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/reasoning
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/responses`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live Responses feed: recent response ids with model, status, previous id (the continuation chain), serving provider and creation time, plus store counts — identifiers and counts only, never input or output text.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/responses
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/responses/tree`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Responses tree: roots with their branches nested (parent → children, from the ledger tree, or the in-memory store when no ledger is wired) — identifiers and counts only, never input or output text; an empty forest is roots [] with total 0.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/responses/tree
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/concurrency`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Adaptive limiter state + TFO probe — no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/concurrency
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Warm HTTP/2 pool state per (provider, key) + Little's Law sizing — counters and key fingerprints only, no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/guard`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Guard-tier table + live guard status — no keys, no secrets.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/guard
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/audit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Audit feed: guard-denial counts (live counters), key-lifecycle events, management POSTs, and the MCP tool-call audit join — counts + redacted identifiers only, no prompts, no keys, no message content. Management auth (admin scope) required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/dashboard/audit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/mcp`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** MCP tool list + live provider/model counts — no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/mcp
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/guardrails`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live guardrail state (blocking config, redaction counts, recent redacted events) — no prompts, no keys.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/guardrails
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/guardrails`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Replaces the guardrail blocking config (toggles only; log redaction is always on and not toggleable). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/guardrails
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/plugins`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live plugin registry: registered plugins, hook points, per-hook call/error/denial/panic counts, recent response shapes — counts only, no prompts, no keys, no response content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/plugins
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/verification`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Honest verification status: what is proven in this VM vs what still needs CI on the first tag, a live browser, a second host, or a real Docker host — statuses only, no keys, no secrets.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/verification
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/strategies`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live routing strategies (global strategy, per-chain overrides, deadline budget, last live strategy) — no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/strategies
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/strategies`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Replaces the global routing strategy and/or per-chain strategy overrides (strategy names only). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/strategies
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/cooldown`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live cooldown mode (global + per-chain overrides) + currently cooling/demoted targets with remaining seconds — mode names and counters only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/cooldown
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/cooldown`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Replaces the global cooldown mode and/or per-chain cooldown-mode overrides (mode names only: hard / soft-demote). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/cooldown
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/groups`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Operator-defined routing groups (routing.groups) with live member health — group names, member provider IDs, enabled flags, breaker state and real attempt counters only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/groups
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/stream-lifetime`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Stream-lifetime cap the engine enforces (effective cap + how many streams it has terminated since this process started) — a duration and a count only, no keys, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/stream-lifetime
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/key-budgets`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Per-key rolling 5-hour USD budgets from the ledger (spent/limit, window, unknown spend) — key ids and names only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/key-budgets
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/key-budgets`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Sets or deletes one key's rolling 5-hour USD budget (key id, limit, unpriced policy only). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/key-budgets
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/quota-pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Fair-share quota pools from the ledger (dimensions with live pool-wide consumption, member allocations) — pool ids, key ids and counts only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/quota-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/quota-pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Creates, replaces or deletes one fair-share quota pool (dimensions, allocations, saturation threshold). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/quota-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/quota-schedules`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Quota schedules from the ledger (time windows, keep-free reserves, per-window budgets, live verdict) — pool and schedule ids only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/quota-schedules
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/quota-schedules`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Creates, replaces or deletes one quota schedule (allow/block window, reserves, budget). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/quota-schedules
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/internal-budgets`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Internal scoped budgets from the ledger (scope, period, limit type, live usage and decision) — budget ids only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/internal-budgets
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/internal-budgets`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Creates, replaces or deletes one internal budget (scope, period, limit type, value, warning threshold). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/internal-budgets
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/teams`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Team cost centers from the ledger (members, budget window, list + metered cost) — team and key ids only, never a raw key or hash.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/teams
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/teams`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Creates, replaces or deletes one team cost center (name, budget, duration, status). Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/teams
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /dashboard/teams/{id}/members`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Assigns a gateway key to a team, or unassigns it. Local-operator dashboard action on the localhost-bound server — carries no secrets.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/dashboard/teams/{id}/members
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/request-budgets`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Per-request USD budget outcomes since server start (refusals pre-send / mid-chain, overages, recent events) — counts and costs only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/request-budgets
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/trace`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Route trace of the most recently finished request: every attempt in order with outcome + error-taxonomy fallback reason (model_not_found, rate_limited, request_invalid, ...) — redacted error strings only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/trace
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/context-fit`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Context-window fit-check decision of the most recently finished request that reached the pre-call fit gate: local prompt estimate, response reserve, required tokens, per-model fit verdicts, and which chain served it (primary / context_fallback / none) — token counts and model names only, no keys, no base URLs, no message content.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/context-fit
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/prices`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Operator-configured per-(provider, model) prices (USD per 1M tokens) from the config file and/or local price file — unknown prices reported as unknown, never $0.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/prices
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/benchmark`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Last measured Geekflare-style bench run from the data dir's benchmark.json (scripts/bench-geekflare.sh output) with per-metric target verdicts — measured values only; recorded=false when no run exists.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/benchmark
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/benchmark/history`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Every recorded Geekflare-style bench run (benchmark-*.json in CODVIAN_BENCH_HISTORY_DIR, oldest first) with kind, clean flag, verdict and passes — recorded files only, never invented; unreadable or corrupt files are listed as skipped, not shown as data.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/benchmark/history
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/memstats`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live memory of this router process (Go heap from runtime.MemStats, RSS, goroutines, in-flight requests, GC counters) + ?profile=heap / ?profile=goroutine binary pprof snapshots — live values only, nothing estimated.
- **Query:** `profile=heap` / `profile=goroutine` — binary pprof snapshot instead of the JSON summary.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/memstats
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/headless`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Headless-mode status + the exact terminal commands for serve --headless, init --wizard and run <cli> --model — no keys, no secrets.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/headless
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/readiness`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Live local-capability readiness: every gateway-provided local capability with its ready state and the Retry-After a client should honour while it is not ready — capability names and states only, no keys, no base URLs.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/readiness
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/streaming`

- **Tier:** `MANAGEMENT`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — auth-exempt (Tier 3, explicitly off)
- **Spawn-capable:** no
- **Description:** Last measured SSE token-delivery run from the data dir's streaming.json (scripts/bench-sse.sh output): median/p99 before (batching off) and after (batching on) at each mock cadence — measured values only, no target verdict; recorded=false when no run exists.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/dashboard/streaming
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/keys/{id}/routing`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Per-key routing dashboard page (B8-94).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/dashboard/keys/{id}/routing
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /dashboard/keys/{id}/access`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Per-key access configuration page (B8-95).
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/dashboard/keys/{id}/access
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

## Admin API (`/admin/*`)

### `POST /admin/shutdown`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Terminates the server process.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/shutdown
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/restart`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Graceful restart: answers 202, stops the HTTP server, flushes caches, then starts a replacement process (under the tray supervisor: exits with the restart code and the supervisor relaunches the gateway; unsupervised: re-launches the same binary with the same arguments). 503 when the process has no restart hook.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/restart
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/service/install`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (system service installation is not supported in this build). Use instead: `codvian-router restart` starts a supervised background gateway.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/service/install
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/plugin/load`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (dynamic plugin loading is unsupported by design: plugins are compiled-in and enabled from config at startup (docs/plugins.md)).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/plugin/load
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/update`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Update check: asks the GitHub releases API for the latest release and answers {current, latest, release_available, update_available, release_url}. When the repository exists but has no published release it answers 200 with latest null, release_available false and status no_published_release (no tag or URL invented). Failures carry their kind in error.code: 503 rate_limited (with Retry-After), 504 timeout, 502 for network, auth, upstream 5xx, malformed response or repository not found. It never downloads or installs anything; installing is `codvian-router update` (signature + checksum verified). 503 when no update checker is wired.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/update
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/tunnel/start`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Starts the local TCP tunnel: forwards a loopback port to the gateway's listen address (real TCP proxy, loopback-only, stdlib only).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/tunnel/start
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/tunnel/stop`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Stops the running local TCP tunnel and reports the bytes it forwarded.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/tunnel/stop
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/tunnel/status`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Live tunnel status (running/stopped, ports, bytes forwarded) — loopback-only like the other tunnel routes.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/tunnel/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/provider/login`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required, AND loopback-only (checked before auth)
- **Spawn-capable:** yes
- **Description:** Starts a provider account login flow (Phase 1: OpenRouter OAuth PKCE) and returns {provider, mode, authorization_url, state, expires_at}. The operator opens the authorization URL in a browser; the exchanged API key is live-tested and stored encrypted through the provider registry (into the existing OpenRouter key pool when one is configured). Loopback-only + admin gateway key. The code_verifier never leaves the server; no secret is logged or returned.
- **Request body (JSON):**

  ```json
  {
    "provider": "openrouter",
    "mode": "headless",
    "key_label": "Codvian Router",
    "limit": "",
    "expiration": ""
  }
  ```

  Source: Fields from internal/server/provider_login.go providerLoginRequest (provider, mode, key_label, limit, expiration). mode: callback (default when a listen address is configured) or headless. Response: {provider, mode, authorization_url, state, expires_at, callback_url?} — never the code_verifier.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"provider": "openrouter", "mode": "headless", "key_label": "Codvian Router", "limit": "", "expiration": ""}' http://127.0.0.1:8080/admin/provider/login
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/provider/login/callback`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — fresh-install first key (no key exists yet); loopback-only
- **Spawn-capable:** yes
- **Bootstrap:** yes — first-key route, only while no gateway key exists
- **Description:** Browser redirect target of the provider login flow: exchanges the one-time authorization code (bound to the PKCE state issued by POST /admin/provider/login) for the user's OpenRouter API key and stores it through the registry. Loopback-only WITHOUT a gateway key — a top-level browser navigation cannot carry one; the single-use 256-bit state plus PKCE is the credential, and off-host requests are refused 403 before anything else. Answers a small HTML result page; no code, state, verifier or key is logged or rendered.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET http://127.0.0.1:8080/admin/provider/login/callback
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/bootstrap`

- **Tier:** `LOCAL_ONLY`
- **Scope(s):** any authenticated key (no scope listed)
- **Auth:** None — fresh-install first key (no key exists yet); loopback-only
- **Spawn-capable:** yes
- **Bootstrap:** yes — first-key route, only while no gateway key exists
- **Description:** Fresh-install first-key setup — only while no gateway key exists; claims with the one-shot bootstrap token printed to stdout at first boot (loopback-only).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -d '{}' http://127.0.0.1:8080/admin/bootstrap
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/settings/export`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Exports the full settings snapshot. Secrets are redacted and never exported — provider keys and gateway-key hashes stay on disk. Requires gateway-key auth AND the typed confirmation phrase EXPORT SETTINGS (the operator-credential second check).
- **Request body (JSON):**

  ```json
  {
    "confirm": "EXPORT SETTINGS"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "EXPORT SETTINGS"}' http://127.0.0.1:8080/admin/settings/export
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/db/export`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Exports the config + ledger usage history as a downloadable JSON file. All secrets redacted (no provider keys, no gateway-key hashes, no base URLs); usage history is read-only, never reset. Requires gateway-key auth AND the typed confirmation phrase EXPORT.
- **Request body (JSON):**

  ```json
  {
    "confirm": "EXPORT"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "EXPORT"}' http://127.0.0.1:8080/admin/db/export
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/db/wipe/arm`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Issues a single-use DB-wipe confirmation token (5-minute TTL, stored hashed, never logged). Requires gateway-key auth AND the typed confirmation phrase WIPE DATABASE — the token is the operator-credential second check for the wipe.
- **Request body (JSON):**

  ```json
  {
    "confirm": "WIPE DATABASE"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "WIPE DATABASE"}' http://127.0.0.1:8080/admin/db/wipe/arm
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/db/wipe`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Irreversibly wipes usage history: ledger usage events, budget running spend, feedback decisions/signals, probe history (ring + file + backup). Providers, provider keys, gateway keys, prices and all other config are preserved. Requires gateway-key auth AND the typed phrase WIPE DATABASE AND the single-use confirmation token from /admin/db/wipe/arm — a key alone, or a key plus phrase, cannot wipe.
- **Request body (JSON):**

  ```json
  {
    "confirm": "WIPE DATABASE",
    "confirmation_token": "<single-use-token-from-/admin/db/wipe/arm>"
  }
  ```

  Source: Typed confirmation phrase + single-use token from the guard-table description; field name confirmation_token verified in internal/server/dbwipe.go (dbWipeRequest).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "WIPE DATABASE", "confirmation_token": "<single-use-token-from-/admin/db/wipe/arm>"}' http://127.0.0.1:8080/admin/db/wipe
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/db/import`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Imports a DB export file (codvian-router-db-export/v1) and merges its usage history into the ledger — never resets or wipes existing history; duplicate events are skipped, never double-counted; the file's config section is ignored. Requires gateway-key auth AND the typed confirmation phrase IMPORT.
- **Request body:** multipart/form-data: confirm=IMPORT, plus the export file field

  Source: NOT a JSON body: the handler reads the confirm phrase from a multipart form field (internal/server/dbimport.go) together with the uploaded export file.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" # body is not JSON — see the section above http://127.0.0.1:8080/admin/db/import
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/revoke-all`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Revokes every gateway key (including the caller's) and persists the empty key list; bootstrap becomes pending so a new first key can be issued loopback-only. Requires the typed confirmation phrase REVOKE ALL.
- **Request body (JSON):**

  ```json
  {
    "confirm": "REVOKE ALL"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "REVOKE ALL"}' http://127.0.0.1:8080/admin/keys/revoke-all
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Updates one gateway key's expiry and per-key traffic limits (requests-per-minute, tokens-per-minute) and persists the change. Auth always required — it changes which credential can authenticate and how much traffic it may send.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}/rotate`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Regenerates one gateway key: the old key is revoked at rotation and a new key is issued in its place (same name/scopes/budget/limits unless overridden), returned exactly once and never logged. Auth always required — it revokes a credential and mints a new one.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}/rotate
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Mints one new gateway key via config.IssueKey and persists it; the raw key is returned once in the response and never logged. Auth always required — it creates a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/import`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Stores an externally-issued key definition under its hash; the raw key is never logged and never returned. Auth always required — it installs a credential.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/import
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /admin/keys/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Permanently revokes one gateway key: removes it from the config (persisted) so it stops authenticating at once; the reversible kill-switch is POST /admin/keys/{id} with {"disabled":true}. Auth always required — it destroys a credential, plus the typed confirmation phrase DELETE <id>.
- **Request body (JSON):**

  ```json
  {
    "confirm": "DELETE <id>"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description (<id> is the key id in the path).
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "DELETE <id>"}' http://127.0.0.1:8080/admin/keys/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/config/reset`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Resets the config to defaults (providers, gateway keys, budget, routing, guardrails, auto-router) and persists it; listen address, data dir and unknown fields are preserved, usage history is not wiped. Requires the typed confirmation phrase RESET CONFIG.
- **Request body (JSON):**

  ```json
  {
    "confirm": "RESET CONFIG"
  }
  ```

  Source: Typed confirmation phrase from the guard-table description.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{"confirm": "RESET CONFIG"}' http://127.0.0.1:8080/admin/config/reset
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/cache/flush`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Flushes the gateway exact-match cache: an empty body flushes every entry; {"key_id"} flushes only that gateway key's entries. Auth always required — it discards cached answers.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/cache/flush
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/status`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Live server status (uptime, provider count, guard state) — gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/status
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/key-templates`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Built-in gateway-key templates (names, scopes, budgets); secret-free by construction — a template holds no key material. Gateway-key auth required.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/key-templates
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}/disable`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Disables a single gateway key (B8-10).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}/disable
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}/enable`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Enables a single gateway key (B8-10).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}/enable
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}/revoke`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Revokes a single gateway key (B8-10).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}/revoke
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /admin/keys/{id}/budget`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (this route stored limits in memory and nothing enforced them). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/keys/{id}/budget
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /admin/keys/{id}/budget-warn`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (this route stored a threshold in memory and nothing read it). Use instead: POST /api/usage/budget (warning threshold field).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/keys/{id}/budget-warn
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /admin/keys/{id}/budget-reset`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (this route stored a reset window in memory and nothing read it). Use instead: POST /api/usage/budget (reset interval field).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/keys/{id}/budget-reset
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/budget-templates`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/budget-templates
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/budget-templates`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/budget-templates
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/budget-templates/{id}/apply`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/budget-templates/{id}/apply
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/keys/{id}/lease`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (exclusive key leases are not implemented: nothing enforced the stored flag).
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/keys/{id}/lease
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/management-tokens`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented: issued tokens never authenticated). Use instead: POST /admin/keys with the admin scope.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/management-tokens
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/management-tokens`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented: issued tokens never authenticated). Use instead: POST /admin/keys with the admin scope.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/management-tokens
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/management-tokens/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented). Use instead: POST /admin/keys with the admin scope.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/management-tokens/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /admin/management-tokens/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented). Use instead: DELETE /admin/keys/{id}.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/management-tokens/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/key-pools`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented: stored pools never affected routing). Use instead: GET /api/quota-pools.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/key-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `POST /admin/key-pools`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X POST -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/key-pools
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `GET /admin/key-pools/{id}`

- **Tier:** `MANAGEMENT`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required (unless auth was explicitly relaxed)
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: GET /api/quota-pools.
- **Request:** no body.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X GET -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/key-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `PATCH /admin/key-pools/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X PATCH -H "Authorization: Bearer $CODVIAN_KEY" http://127.0.0.1:8080/admin/key-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.

### `DELETE /admin/key-pools/{id}`

- **Tier:** `ALWAYS_PROTECTED`
- **Scope(s):** `admin` — scope `admin` implies every scope
- **Auth:** Gateway key required — always, even if auth were relaxed for Tier 3
- **Spawn-capable:** no
- **Description:** Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.
- **Request body:** JSON object for the action described above. The exact field list lives in the handler and is NOT re-specified here — do not send fields guessed from this doc.
- **Response:** JSON feed/object as described above. The exact field list lives in the handler and is NOT re-specified here — do not assume fields beyond the description.

  ```sh
  curl -X DELETE -H "Authorization: Bearer $CODVIAN_KEY" -d '{}' http://127.0.0.1:8080/admin/key-pools/{id}
  ```

  Errors use the OpenAI-shaped envelope `{"error": {"message": "...", "type": "invalid_request_error"}}` (see `writeError` in internal/server/server.go); server/upstream failures use type `server_error` / `upstream_error`. Unauthenticated: 401; authenticated but wrong scope: 403; non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.
