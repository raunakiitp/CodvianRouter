# Client Compatibility Matrix (P1.3)

## Rule

A client is listed as **verified** only when a test in this repository
exercises that client's *own* requirement (its dialect, endpoint and
credential header). A generic OpenAI-compatible endpoint existing is not
evidence a client works, and this matrix never claims it is. The source of
truth is `codvian-router clients` (and `codvian-router clients --json`),
generated from `cmd/codvian-router/clients.go`; a test
(`TestP13ClientMatrixClaimsMapToRealTests`) fails the build if a
"verified" row names a test that does not exist.

## Credential headers accepted (P1.3 change)

Every gateway-key-protected route now accepts the key in any of three
forms, all carrying the same secret:

| Header | Sent by |
| --- | --- |
| `Authorization: Bearer <key>` | canonical form; what `keygen` prints; OpenAI-style clients |
| `X-Api-Key: <key>` | Anthropic-style clients (Cline in Anthropic mode, Claude Code, Zed) |
| `X-Goog-Api-Key: <key>` | Google Gemini SDKs / gemini-cli |

A non-Bearer `Authorization` scheme (`Basic ...`) is **not** a credential;
the API-key headers are still consulted. Auth, scope checks and the 401
diagnostics all read the key through one function (`requestKey`), so the
three presentations behave identically.

## Matrix

| Client | Dialect | Endpoint | Credential | Status | Evidence / caveat |
| --- | --- | --- | --- | --- | --- |
| Cline (Anthropic mode) | Anthropic Messages | `POST /v1/messages` | `x-api-key` | verified | `TestP13GatewayKeyXApiKeyAuthenticates`; live session not run in CI |
| Claude Code | Anthropic Messages | `POST /v1/messages` | `x-api-key` | verified | `TestP13GatewayKeyXApiKeyAuthenticates`; `count_tokens` served, no live CLI in CI |
| Cline / Continue / Cursor (OpenAI mode) | OpenAI Chat Completions | `POST /v1/chat/completions` | bearer | verified | `TestP13GatewayKeyBearerAuthenticates`; SSE framing + models list pinned by the server suites |
| Codex CLI (Responses) | OpenAI Responses | `POST /v1/responses` | bearer | verified | `TestP13GatewayKeyBearerAuthenticates`; responses SSE `[DONE]` pinned |
| Gemini SDKs / gemini-cli | Gemini native (v1beta) | `POST /v1beta/models/{model}:generateContent` | `x-goog-api-key` | verified | `TestP13GatewayKeyXGoogApiKeyAuthenticates`; live SDK call not run in CI |
| Aider | OpenAI Chat Completions | `POST /v1/chat/completions` | bearer | partial | streaming + model list work; Aider's `/v1/models` probing quirks are not pinned |
| Zed | OpenAI or Anthropic | either endpoint | bearer or `x-api-key` | partial | both wires exist; Zed's provider schema has not been exercised in CI |

## What "verified" means here — and what it does not

- Verified = the client's **protocol requirement** is pinned by a named
  test: the endpoint exists, the credential header authenticates, and (for
  streaming dialects) the SSE framing is asserted by the server suites.
- Verified does **not** mean a live client binary was driven in CI. No
  editor extension, SDK, or CLI agent is launched by the test suite; those
  remain manual smoke checks.
- A row marked **partial** is a real, honest gap, not a euphemism: the wire
  exists, but the client's specific behaviour is not pinned.

## Verifying locally (no paid API credits)

Protocol-level (zero cost, no network):

```sh
go test ./internal/server/ -run 'TestP13' -v          # credential forms
go test ./cmd/codvian-router/ -run 'TestP13' -v       # matrix claims map to real tests
codvian-router clients                                # the matrix itself
codvian-router clients --json                         # machine-readable
```

Live client smoke (optional, needs a running router and the client
installed; point the client at `http://127.0.0.1:<port>` and use a gateway
key from `codvian-router keygen`):

1. Start the router: `codvian-router serve`.
2. Point the client at the base URL with the credential form in the matrix
   (never paste the key on a command line — use the client's config file or
   its environment-variable field; the router accepts env-provided secrets
   so the key need not appear in shell history).
3. Send one short prompt in each mode you rely on (streaming and
   non-streaming, plus a tool call if the client uses tools).

These manual steps are not automated here because they require the client
binaries and a provider credential; claiming they run in CI would be false.