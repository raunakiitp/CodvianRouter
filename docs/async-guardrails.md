# Async guardrails

Slow guardrails — an external classifier, a policy API, a content
moderation call — must not block the first token. In async mode a
guardrail is checked **concurrently** with the provider call: the
response streams immediately, and if the verdict later says BLOCK
the stream is cut with the terminal refusal frame. Blocking
guardrails (PII, secrets, prompt injection — the built-ins) stay
**synchronous**: a secret must never reach a provider, so those
kinds can never be async.

## Config

```json
{
  "guardrails": {
    "block_pii": true,
    "block_secrets": true,
    "block_injection": true,
    "external": [
      {
        "name": "toxicity",
        "kind": "toxicity",
        "mode": "async",
        "endpoint": "http://127.0.0.1:8080/check",
        "timeout_ms": 5000
      }
    ]
  }
}
```

One entry per external guardrail:

| Field | Meaning |
|---|---|
| `name` | Unique within `external`. Names the guardrail in the dashboard, the feed and the logs. |
| `kind` | The label for the verdict (`toxicity`, `spam`, a classifier kind). Must not be `pii`, `secret` or `injection` in async mode — those are blocking kinds (see below). |
| `mode` | `sync` or `async`. `sync` checks before dialling (BLOCK refuses outright, provider never called). `async` checks concurrently (BLOCK cuts the stream / withholds the response). |
| `endpoint` | The classifier URL. Must be a valid http(s) URL. |
| `timeout_ms` | One check bound in milliseconds. `0` = 5 s default. A verdict that arrives after the timeout is recorded as an error and the request continues (fail-open). |

Validation runs at config load **and** at startup: an async entry
with a blocking kind (`pii`, `secret`, `injection`), a duplicate
name, an empty name, an unknown mode, or a bad endpoint fails
startup (or the dashboard POST) with a 400 / startup error naming
the guardrail — never a silently accepted misconfiguration. A
dashboard POST that carries only the three toggles preserves the
configured external entries; a POST that carries `external` entries
replaces them and is validated the same way.

## The classifier protocol

The router POSTs the request text (the same text the sync check
sees) to the endpoint:

```json
{"text": "<the request text>"}
```

and expects HTTP 200 with:

```json
{"verdict": "allow", "reason": ""}
{"verdict": "block", "reason": "toxicity"}
```

- `verdict` must be exactly `allow` or `block`. Any other string,
  a non-200 status, or a malformed body is a **checker error**.
- No auth header is sent. If the classifier needs auth, put an
  operator-owned proxy in front of it.
- The response body is capped (a classifier that echoes the text
  back cannot make the gateway hold it) and an error body is never
  included in the returned error — only the status.

**Checker errors fail open.** A classifier outage must never take
the gateway down: the request continues, and the error is recorded
honestly (outcome `error`, counted in the dashboard and in
`/metrics`) — never hidden, never retried silently.

## What happens on BLOCK — `/v1/messages` (CHECKLIST 203)

The async run also covers the `POST /v1/messages` relay, on both
of its paths, with the same verdicts and the same timing labels —
and the feed records each verdict's surface (`"chat"` or
`"messages"`, field `surface` on `async_events`), so the dashboard
shows which path a verdict ran on.

- **Verbatim relay, streaming:** the upstream call runs under a
  child context the verdict can cancel. A BLOCK cuts the relayed
  stream with the Anthropic-protocol terminal error event — the
  exact frame, documented in `internal/server/relay.go`
  (`anthropicPolicyRefusalEvent`):

  ```
  event: error
  data: {"type":"error","error":{"type":"api_error","message":"guardrail blocked stream: <kind>"}}
  ```

  No `[DONE]` follows it and no `message_stop` precedes it:
  Anthropic streams never carry `[DONE]` (that is the OpenAI
  envelope — inventing one would break Anthropic clients), and a
  `message_stop` would claim the message completed normally. The
  error event alone is terminal. Anthropic defines no
  policy-refusal error type or code, so the frame keeps this
  path's existing `api_error` type and carries the kind in the
  message; the gateway's own `-32001` code lives on the plain-JSON
  403 refusal (before commit), not in this event.
- **Verbatim relay, non-streaming:** the body is buffered (capped
  at 10 MiB) before anything is written, so a BLOCK that landed
  during the upstream call still withholds the response with the
  plain 403 / `-32001` JSON refusal. A BLOCK that cancels the
  upstream call before it returns answers the same plain 403,
  because no headers were committed yet.
- **Translate path:** the same cut as chat completions, through
  the engine's cancel cause — a BLOCK cuts the Anthropic stream
  with the same terminal error event above, or withholds a
  non-streaming response with 403 / `-32001` before delivery. The
  attempt stays in the ledger as the engine recorded it.
- **No ledger row on the verbatim path, cut or not** — the relay's
  standing rule (a relayed request's spend is never recorded)
  applies unchanged. The refusal is recorded in the guardrail
  store and in `/metrics` (`phase="async"`), never as an upstream
  failure, and there is no failover.

## What happens on BLOCK

### Streaming

The response streams immediately; the first content chunk commits
at once (the preread holds only non-content leading events). If the
verdict lands after bytes reached the client, the cut goroutine
cancels the stream context and the client gets the terminal refusal
frame — the same shape as the sync refusal:

```json
{"error": {"message": "guardrail blocked stream: toxicity", "type": "policy_refusal", "code": -32001, "reason": "toxicity"}}
```

followed by `data: [DONE]`. The stream is always terminated: a
client waiting on the terminator never hangs.

If the verdict lands **before** any byte reached the client (the
cut fired while the provider was still slow to answer), nothing was
committed, so the client gets the plain 403 JSON refusal instead —
identical to the sync refusal. Same code, same reason, no content
leaked.

### Non-streaming

A BLOCK that lands while the provider call is in flight withholds
the response: the client gets 403 / `-32001` and the content is
never delivered. The provider **was** called and billed — the
ledger keeps the engine's success row (same treatment as a
response-guardrail block: what is withheld is only the delivery).

### No failover, no health signal

A policy cut is not a provider failure: the second target is never
dialled, no breaker opens, no cooldown starts, no success-rate
record is written, and `/metrics` shows `0` upstream failures for
the cut. The trace records one attempt with outcome and reason
`async_guardrail_blocked` (amber in the dashboard trace view, like
a content refusal — a policy cut, not a failure).

## Verdict outcomes (timing labels)

The dashboard, the feed (`GET /dashboard/guardrails`, fields
`async` / `async_events`) and `/metrics`
(`codvian_router_guardrail_evaluations_total{phase="async",...}`)
report one row per async verdict. The outcome is a **timing**
label — what the client experienced:

| Outcome | Meaning |
|---|---|
| `allowed` | The verdict was allow. The stream/response continued unchanged. |
| `blocked_after_start` | BLOCK landed after bytes reached the client: the live stream was cut with the refusal frame. |
| `blocked_before_delivery` | BLOCK landed before anything was delivered: the response was withheld (plain 403 for a stream, withheld JSON for non-streaming). |
| `blocked_after_delivery` | BLOCK landed after delivery (or after the handler returned): too late to act on — recorded honestly, not as a cut that never happened. |
| `error` | The checker errored or timed out. Fail-open: the request continued. |

A verdict that lands after the handler returned is recorded as
`blocked_after_delivery` — the run outlives the request on
background contexts with per-checker timeouts, so slow verdicts
still land in the feed instead of being dropped.

## Billing honesty

The ledger records only what was actually served:

- **Cut mid-stream:** the attempt is recorded failed, with the
  local prompt-token estimate (`LocalEstimate=true`) and
  completion tokens `0` — never invented. The client is billed
  only for what was served before the cut.
- **Cut before the engine dialled** (the verdict landed before
  the first attempt started): the trace has zero attempts and the
  ledger has zero rows — nothing was attempted, nothing was
  served, so there is nothing to bill, not even a prompt
  estimate. The refusal itself is recorded in the guardrail store
  (`blocked_before_delivery`), where it belongs. Note the timing
  next to it: if the attempt was already in flight when the
  verdict landed, the same cut records one failed attempt with
  the prompt estimate instead — the client sees the identical
  plain 403 in both cases, and the trace and the ledger always
  agree with each other; only the timing differs.
- **Withheld non-streaming response:** the provider was called,
  so the ledger keeps the engine's success row.

## Limits (honest)

- **Late-verdict race:** a BLOCK that lands in the window between
  the last chunk and `[DONE]` lets the stream complete; it is
  recorded as `blocked_after_delivery`, literally true as a timing
  label. The race is inherent — the verdict and the stream end
  are concurrent — and the label says so instead of claiming a
  cut that did not happen.
- **Double-block corner:** a response-guardrail refusal plus a
  late async BLOCK records `blocked_before_delivery`, literally
  true as a timing label (the response was withheld before
  delivery, by whichever refusal fired first).
- **No auth on the classifier endpoint** (see the protocol above).
- **One checker per guardrail**, registered at startup from the
  config file. A guardrail with no registered checker records an
  error verdict (`error`) on every check — visible in the
  dashboard as `checker: MISSING`, never as a working guardrail.
