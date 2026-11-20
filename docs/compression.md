# Lossless-first prompt/context compression (CHECKLIST 183–184)

Compressing a prompt before the provider call saves input tokens — but a
lossy summary must never silently change what the model is asked. This
slice compresses **lossless first**, and treats lossy trimming as a
separate, explicit, always-disclosed opt-in.

## The two modes

### Lossless (default when opted in)

Three steps, each defined so the compressed prompt means the same thing
as the original. The router verifies the result structurally before
sending it (see "Verification" below); if verification fails, the
original messages are sent and no compression is recorded.

1. **Dedupe repeated system/developer blocks.** Exact-duplicate
   system or developer messages (same role, same content) are collapsed
   to the first copy. User, assistant and tool messages are **never**
   deduped — a repeated user message is a real turn, not a copy.
2. **Strip redundant whitespace.** Outside fenced code blocks
   (`` ``` ``), trailing spaces/tabs are stripped per line, runs of
   blank lines collapse to at most one, and leading/trailing blank
   lines are trimmed. Inside fences the text is byte-identical —
   code and preformatted content are never touched.
3. **Canonicalise repeated tool results.** When several tool results
   share a `tool_call_id`, only the **last** is kept — earlier ones are
   stale replays of the same call.

### Lossy (per-call opt-in only)

Lossy is **extractive trimming, not abstractive summarisation**: the
router runs no summariser model, so it cannot rewrite a conversation
into a shorter summary. What it does, oldest first:

- Drop whole middle turns (pinned: all system/developer messages, the
  last user message, and the last message of any role).
- Then truncate the longest plain-text messages head+tail (60% head /
  40% tail) with an inline `"[…truncated by lossy compression…]"`
  marker.

Part-carrying (multimodal) messages are dropped whole or kept, never
partially rewritten. Lossy never empties a message (the server requires
system/developer/user messages to be non-empty). `after_tokens` may
still exceed the budget when the pinned messages alone do not fit —
the real count is reported, never forced to the budget.

## How to opt in

**Per request** — a `compression` object on `POST /v1/chat/completions`
(canonical, like the `routing` field; unknown keys are rejected with
400 naming the field):

```json
{
  "model": "gpt-4o",
  "messages": [ ... ],
  "compression": {
    "lossless": true,
    "lossy_max_prompt_tokens": 800
  }
}
```

- `"lossless": true` — run the lossless steps for this call.
- `"lossless": false` — explicitly opt OUT, even for a model listed in
  the config (see below).
- `"lossy_max_prompt_tokens": N` — run lossy trimming after the lossless
  steps, targeting at most N prompt tokens. N must be ≥ 0; negative
  values are rejected with 400. Omitting it means no lossy trimming.
- Omitting `compression` entirely means no compression for the call
  (unless the model is listed in the config).

**Per model** — the config file block (see `internal/config/SCHEMA.md`):

```json
"compression": { "lossless_models": ["gpt-4o", "claude-x"] }
```

Models listed here get the lossless steps on every call. **Lossy is
never configured** — it is per-call client opt-in only, so no operator
setting can silently trim a caller's prompt.

## Disclosure — never silent

When compression runs, the response says so:

- **Headers** (non-streaming and streaming):
  - `X-Codvian-Compression: lossless` | `lossy` | `lossless+lossy`
    (lossy always runs lossless first).
  - `X-Codvian-Compression-Saved-Tokens` — local-estimate tokens saved.
  - `X-Codvian-Compression-Prompt-Tokens` — prompt tokens as sent
    (after compression).
  - `X-Codvian-Compression-Lossy: true` — only when a lossy step ran.
  - Absent header = the request was not compressed.
- **Route trace** (`GET /dashboard/trace`, and the dashboard Live
  Routing feed): a `compression` block with `applied`, `lossless`,
  `lossy_applied`, `steps`, `before_tokens`, `after_tokens`,
  `saved_tokens`, `lossy_max_tokens`, `source` (`request` |
  `model-config` | `none`), and `verified`.
- **Ledger** (`usage_events`): `prompt_before_compression_tokens`
  and `compression_saved_tokens` on the successful attempt's row.
  The dashboard Overview totals them (`compression` block:
  `requests_with_compression`, `tokens_saved`) and the Recent
  requests / Logs tables show a Saved column per row.

## Measurement — honest local counts

Token savings are measured with the local counter
(`internal/tokencount`, items 145–146) — the same tiktoken BPE count
used for the context-fit check. That means:

- All counts are **local estimates**: the wrong encoding for
  non-OpenAI providers, no tool-schema or framing overhead.
- `before_tokens` is counted on the prompt as received;
  `after_tokens` on the prompt as sent. The provider's own reported
  usage is kept verbatim in the ledger and is never replaced by the
  local count; a material divergence between local and reported
  counts is surfaced by the existing reconciliation counters.
- Ledger compression totals count **successful attempts only**.
  A failed attempt never carries a saving (the engine records failed
  attempts with an empty usage), and shared (coalesced-waiter) rows
  are zeroed like every other token field — so each request is
  counted once. Failed-request savings, if any were measured, are
  visible in the route trace only.

## Verification — structural, not a semantic proof

`internal/compress.VerifyLossless` checks that the compressed form is
exactly the canonical normal form: `Normalise(original) ==
compressed` **and** `Normalise(compressed) == compressed`
(idempotent). This is a **structural guarantee, honestly documented
as not a semantic proof** — no test can prove two texts mean the same
thing. The semantic claim rests on the step definitions above
(identical duplicates removed, whitespace outside code fences,
stale tool replays), each covered by unit tests that assert the
equivalence property on the normalised form
(`internal/compress/compress_test.go`, 13 tests).

## Where compression runs

- **Server handler** (`internal/server/compression.go`):
  `applyCompression` runs after guardrails and before the budget
  gates, so every gate sees the prompt as it will actually be sent.
  It stamps `req.Compressed`.
- **Engine** (`internal/router/router.go`): `maybeCompress` runs at
  every entry point (`Chat`, `ChatStream*`, `ChatWithChainStrategy`,
  `ChatStreamWithChainStrategy`, `runStream`) **before** the
  coalescing key is computed, so the key is on the compressed
  prompt — two requests that differ only in compression must not
  share a flight unless their compressed prompts are identical.
  A server stamp for the same model is kept as-is (no re-measuring
  the saving against the already-compressed prompt); when the model
  changed since the stamp (an `auto/*` resolve), the decision is
  re-run with the stamp as the prior, so the saving stays measured
  against the original prompt.
- **Ledger adapter** (`cmd/codvian-router/main.go`): the production
  `ledgerAdapter` maps `PromptBeforeCompression` /
  `CompressionSavedTokens` from `provider.Usage` onto the row, like
  every other token field.

## Limits, honestly stated

- Lossy is extractive trimming, not abstractive summarisation; no
  summariser model exists in the router.
- All token counts are local tiktoken estimates (see above).
- `after_tokens` may exceed the lossy budget when pinned messages
  alone do not fit.
- `VerifyLossless` is structural, not a semantic proof.
- Exact-match cache key: server-side compression happens before the
  degrade cache lookup on the served path; a direct engine caller
  bypassing the server could share a cache entry across compression
  settings.
- Semantic cache (FIXED, items 193–194): the gateway's semantic
  cache is keyed on the last user message's text (model-scoped,
  text-only — see `internal/degrade` `semanticText`). Before the
  fix, a lossy-trimmed request's response could therefore be
  served to a later request with the same last user message and
  model — a trimmed-context answer presented as a full-context
  one. The fix EXCLUDES lossy requests from the semantic layer
  entirely (never stored, never served there;
  `degrade.lossyCompressed`, keyed on the applied lossy stamp
  and, fail-closed, on the per-request lossy opt-in). Fingerprint
  scoping was considered and rejected: a fingerprint would have
  to cover the whole (compressed) prompt, so a semantic hit would
  need exact fingerprint equality and the layer would degenerate
  into a second exact-match cache for lossy requests. For lossless
  compression the prompts are semantically identical (the lossless
  steps are exactly the canonical normal form), so lossless
  responses keep sharing semantically — proven by test. The
  exact-match cache is not affected: its key is computed on the
  messages as sent, so a compressed and an uncompressed prompt
  have different keys. The /v1/messages relay path needs no
  equivalent check: it never applies compression (the Anthropic
  shape carries no compression opt-in), so it can only ever store
  or look up uncompressed requests — and since lossy responses
  never enter the shared semantic cache, the relay can never
  serve one either. The dashboard Overview cache note states the
  rule.
- Per-model config matches the resolved model in the engine; the
  server's early pass matches the literal model name (`auto/*`
  resolves later; the engine re-decides with the prior stamp).
- Compression never fails a request: a counting failure leaves the
  messages untouched, and a verification failure sends the original
  messages with `Applied=false`.
