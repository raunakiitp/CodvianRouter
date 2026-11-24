# Context/Output Guardrails (P1.8)

## Why this block exists

Chat surfaces already had a pre-dispatch context-fit gate (`internal/router/contextfit.go`): a local tiktoken count of the prompt (tools + reference framing) compared against each candidate model's context window minus a response reserve; too-small attempts are dropped before any dial. The embeddings and rerank surfaces resolved their chain and dialled directly, so an input larger than every known context window used to be sent upstream — and failed there, after a provider call.

## What lands

- **Embeddings/rerank context gate** (`internal/router/embeddings.go`):
  - `applyEmbeddingFit` runs before dispatch: counts the concatenated input texts, refuses before any provider call when the input exceeds every KNOWN context window (no completion leg, so no reserve). Unknown windows are kept — unknown is not proof the model is too small, matching the chat gate.
  - Errors: `EmbeddingContextTooLarge` → 400 with the honest message ("no provider was called"), both with and without a known largest window.
  - Server error mapping (`internal/server/embeddings.go`): `handleEmbedEngineError` catches `EmbeddingContextTooLarge` and returns a 400 (never a 502).
  - Chat context-fit error mapping already existed (`ErrContextTooLarge` → 400).
- **Chat gate unchanged** (it was already complete):
  - Local tiktoken count (`tokencount.CountPromptFull` → tools + framing) + reserve (requested `max_tokens` or 4096).
  - Unknown window = tried (never refused on unknown).
  - Dedicated long-context fallback chain (`Config.ContextFallbackChain`) when nothing in the primary chain fits.
  - Honest failure `ErrContextTooLarge` → 400 pre-call.
- **Output reserve** already correct: the request's `max_tokens` is honoured as the reserve; the default (4096) is a conservative safety margin. `Config.ContextReserveTokens` can set a global default.
- **Output controls / budget awareness**:
  - The org/team budget gates (P1.6) run before dispatch and refuse on estimated list cost, so no overspend can be dispatched.
  - Token-count variance already surfaced (`ledger.DriftSummary`); cost reconciliation (P1.7) adds variance on cost.
- **No truncation**: the engine refuses before any truncation. The compression engine (P1.4) is lossless-first with disclosure; the context-fit gate is a hard boundary.

## What is NOT in P1.8

- **Per-model max output tokens**: P1.5 records `CapMaxOutputTokens` from operator pins, but there is no per-model cap in the provider metadata. A future block could wire that into the reserve.
- **Multimodal token counting edge cases**: data URIs are stripped, tool call *arguments* are counted, tool *results* (role=tool) have their `Content` counted. Tool call *names/descriptions* are not counted — they are a few tokens; the safe direction is to overcount via framing, which the chat gate already does. A future block could add explicit tool-name/desc counting.
- **Caching / memoization of counts**: the engine counts on every request. The `tokencount` package is fast (single-pass BPE) and the prompt is already in memory; caching would add complexity for marginal gain.
- **Rerank object documents**: the rerank handler already extracts text from object documents for guardrails and the token count — but an object with no string text fields is counted as zero. Acceptable.

## Guarantees pinned by tests

| Test | Pins |
| --- | --- |
| `TestP18EmbeddingContextGateRefusesBeforeDispatch` | input > known window → 400, zero provider calls, honest message |
| `TestP18EmbeddingContextGateUnknownWindowDoesNotRefuse` | unknown window → chain untried (never refused), provider called |
| `TestP18EmbeddingContextGateFitsWithinWindow` | input fits → provider called |
| `TestP18RerankContextGateRefusesBeforeDispatch` | same gate on rerank |
| `TestP18EmbeddingGateDropsOnlyTheTooSmallAttempt` | a chain with a too-small + a fitting target serves the fitting one; too-small never dialled |
| `TestP18EmbeddingContextTooLargeIsBadRequest` | server error mapping → 400, honest message with largest window |
| `TestP18EmbeddingContextTooLargeWithoutKnownWindowStillBadRequest` | no known window → 400, unknown-window wording |

## Local evidence

```
go test -buildvcs=false -count=1 ./internal/router/ ./internal/server/ -run 'TestP18'
GO_TEST_EXIT=0  →  router 0.3s, server 0.3s all ok
```

No test skipped or weakened; no paid API credits used.