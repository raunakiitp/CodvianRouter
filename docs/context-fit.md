# Context-window pre-call fit check + dedicated fallback chain (CHECKLIST 159–160)

## What it does

Before dialling any provider, the engine counts the prompt locally
(tiktoken BPE, offline — `internal/tokencount`) and compares it
against each candidate model's context window, minus a reserve for
the response. An oversize prompt **never dials the too-small
model**: too-small attempts are removed from the chain before the
first call (route-trace outcome `skipped_context_fit`), so no
retry is ever burned on a model that cannot hold the prompt.

When no model in the primary chain fits, the request jumps
straight to the **dedicated context-window fallback chain**
(config `routing.context_fallback_models` — operator-configured
long-context models, in preference order). When no configured
model fits anywhere, the request fails honestly **pre-call** with
HTTP 400 naming the required tokens against the largest available
window — zero provider calls, no ledger row, no retry.

## The reserve (documented)

Required tokens = local prompt estimate + reserve. The reserve
covers the completion the model must still be able to emit:

- the request's own `max_tokens`, when the caller set one — the
  caller asked for that much room, so the check honours it;
- otherwise `routing.context_reserve_tokens` when configured;
- otherwise the default **4096** (`router.DefaultContextReserveTokens`).

A prompt that exactly fills the window is therefore refused: the
model would have nowhere to write its answer. A caller that sets
a very large `max_tokens` can make the requirement larger than
every window — the 400 then names that number honestly.

## Fit rules

- The same local estimate is used for every candidate (the count
  is an estimate: encoding from the model name, no tool schemas or
  per-message framing — see `internal/tokencount`).
- `required <= window` fits; `required == window + 1` does not.
- A model whose window is **unknown** (registry reports 0) is
  treated as fitting: unknown is not proof the model is too
  small. The decision records it as `window_known: false` and the
  dashboard shows "Unknown", never a guessed number.
- A counting failure never fails the request: the check is
  skipped, the full chain is tried, and the decision records
  `checked: false` with the reason (same rule as the pre-send
  budget gate, item 145).

## Where it runs

`router.Engine.applyContextFit` (`internal/router/contextfit.go`),
called once per request in `ChatWithChainStrategy`
(non-streaming) and in `runStream` (streaming), after the chain
is ordered and before the first dial — so hedging, 429 retry and
failover can only ever touch fitting models. The decision is kept
as `Engine.LastContextFit` and forwarded through
`degrade.StaleEngine`, so the server feeds serve it.

Config (`internal/config`, `routing` block, see `SCHEMA.md`):

```json
"routing": {
  "context_fallback_models": ["gpt-4o", "claude-sonnet-4-5"],
  "context_reserve_tokens": 2048
}
```

An entry no provider offers contributes nothing (it is not an
error — the model may arrive later via a live provider add).
Empty `context_fallback_models` means no dedicated chain.

## Server + dashboard

- `ErrContextTooLarge` maps to HTTP 400 in
  `server.handleEngineError` (a property of the request + the
  operator's configuration, not an upstream failure — it does not
  count as an upstream failure on `/metrics`).
- `GET /dashboard/context-fit` (Tier 3 MANAGEMENT, auth-exempt)
  serves the last decision (`has_decision: false` before any
  request reached the gate).
- `GET /dashboard/trace` embeds the same decision
  (`context_fit`), and `GET /dashboard/models` embeds it too.
- Models view: a **Fit** column per row (Fits / Too small /
  Unknown / —) plus a fit-summary line under the table.
- Live Routing trace: `skipped_context_fit` attempts render as
  "context too small for this prompt — skipped before dialling",
  with the fit summary above the attempts.

## Honest limitations

- The check covers `/v1/chat/completions` only. The verbatim
  relay (`POST /v1/messages`, item 143) is single-attempt by
  construction and does not run the fit gate — an oversize
  Anthropic-dialect request is forwarded as-is and the upstream
  decides.
- The local count is an estimate: a model whose real tokenizer
  is much denser than the tiktoken estimate could still be
  refused by the upstream at the margin, and a model whose real
  tokenizer is much sparser could pass the gate and still fit.
  The reserve (4096 by default) absorbs small estimation error;
  it does not absorb a different tokenizer family.
- A cache hit (exact or semantic) is served without calling any
  provider, so it never reaches the fit gate — correctly: a
  cached response needs no context window.
- The fallback chain is global (one list for every model), not
  per-model: a prompt for model A and a prompt for model B jump
  to the same long-context models.
