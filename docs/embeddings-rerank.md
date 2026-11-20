# Embeddings + rerank passthrough (CHECKLIST 171–172)

`POST /v1/embeddings` and `POST /v1/rerank` are routed through the
same engine as `POST /v1/chat/completions` — chain resolution,
budgets, guardrails, ledger, route trace — but each call is
**replay-exempt: exactly one upstream attempt, never replayed onto
another target**.

## Why exactly once

An embeddings call is single-shot vector work and a rerank scores
the caller's documents once. Replaying either onto another target
after a failure would double-apply upstream work (and double-bill
it). So the first target that is actually dialled owns the outcome
— success or failure — and the request ends there.

What does **not** count as an attempt: a target skipped *before
dialling* — zero quota, breaker open, cooling, limiter shed, no
embeddings/rerank capability on that client, or no live client at
all. No upstream call was made, so moving to the next capable
target is selection, not a replay. A limiter-shed target is
skipped the same way the chat chain skips one.

## Shapes

- **Embeddings** is the OpenAI shape: `model`, `input` (a string,
  an array of strings, an array of token IDs, or an array of
  token-ID arrays), `dimensions`, `encoding_format` (`"float"` or
  `"base64"`), `user`. Vectors are returned **verbatim**
  (`json.RawMessage`) — a float array stays a float array and a
  base64 string stays a base64 string; the gateway never parses,
  truncates or re-encodes an embedding. Only the usage block is
  parsed, for the ledger.
- **Rerank** is the Cohere/LiteLLM shape (OpenAI has no
  `/v1/rerank`): `model`, `query`, `documents` (strings or objects,
  forwarded verbatim), `top_n`, `return_documents`. Results carry
  `index` + `relevance_score`, plus the document echo when the
  request asked for it. Usage is present only when the upstream
  reports it — Cohere reports `billed_units` in `meta`, which is
  not a token usage block and is not converted into one; a missing
  usage block means unknown, never zero usage.

## Selection: native-wire providers are skipped, never dialled

Only an OpenAI-wire client can serve these endpoints in this
slice. Native-wire clients (Anthropic, Gemini, Cohere, Bedrock)
inherit `Embed`/`Rerank` by embedding `*openAIClient`, so the
interface check alone would select them — and the call would fail
on the native base's 404. Selection therefore requires the
OpenAI wire format (`router.supportsEmbeddings` /
`router.supportsRerank`): a client that does not expose its wire
format is treated as OpenAI wire (the `Client` contract is
OpenAI-compatible by definition). A skipped target appears in the
route trace as `skipped_unsupported`, and the dashboard renders it
as "endpoint not offered by this target — skipped". A future slice
may add native transports (Cohere `/v1/embed`, Gemini
`embedContent`); that changes the selection check, not the call
sites.

When no target in the chain can serve the endpoint at all, the
engine returns `router.NoCapableProviderError` and the server
answers an honest **400** naming the endpoint — a property of the
request plus the operator's configuration, not a provider failure,
so no upstream-failure counter moves and the answer is never a
502. When capable targets existed but were all skipped (breaker,
cooldown, shed), the engine returns `ErrNoAvailableAttempt`, like
chat.

## The bookkeeping request

The engine never sees the embeddings/rerank shapes directly. The
handler builds a synthetic `provider.ChatRequest` carrying the
input text as user messages, the endpoint kind, the caller ID and
the per-request budget. It is **never sent upstream**; it exists so
the shared machinery works unchanged:

- `reconcileUsage` — the local-estimate fallback when the upstream
  reports no usage (the usual rerank case). The ledger row then
  carries the labelled local estimate, never a provider-reported
  split.
- `budgetGate` — the strict per-request budget, which stops the
  chain before dialling with a typed 402.
- `recordAttempt` — the ledger row, with the endpoint on it.

A token-ID embeddings input carries no text, so its bookkeeping
request has no messages: the local estimate and the strict gate
cannot price it, and the ledger records whatever the upstream
reported (or unknown). This is a documented limitation, not a
silent gap.

## Guardrails, budgets, ledger, trace

- **Request guardrails** run on the input text before any provider
  is dialled, exactly as on chat completions. A blocked request
  makes zero provider calls and answers 403 with the policy-refusal
  code `-32001`.
- **No response guardrail** runs on the results: embeddings are
  vectors, and rerank results echo the caller's own documents —
  scanning an echo would block the user on their own text.
- **Budgets**: the per-request budget (`X-Codvian-Budget`), the
  prompt budget (estimated from the input text) and the key budget
  are all refused 402/429 before dialling, with zero provider
  hits. The budget is copied onto the real request so the engine's
  strict gate sees it too.
- **Ledger**: every attempt records a row with an `endpoint`
  column (`""` = chat completions, `"embeddings"`, `"rerank"`).
  The column was added by migration (`ensureUsageColumns`): rows
  recorded before it existed read as `""`, which is honest — chat
  was the only surface then. Pricing is honest from the price book
  where known; an unknown price is unpriced, never `$0`-claimed.
  The export/import round-trip carries the endpoint, and the dedup
  fingerprint includes it — a chat row and an embeddings row for
  the same model at the same timestamp are different rows, not
  duplicates.
- **Trace**: `RouteTrace.Endpoint` names the surface, and the
  called attempt is stamped `preread: "exempt:<endpoint>"`, so the
  dashboard renders "exempt (embeddings) — never replayed" exactly
  like the streaming preread record. `GET /dashboard/trace`
  carries the endpoint through.
- **Metrics**: a dedicated `recordEmbedMetrics` — the chat
  `recordMetrics` is not reused, because a stale `LastCacheHit`
  from a previous chat request would misattribute an embeddings
  call to `"cache"`, and a single attempt has zero failovers by
  construction.
- **Degrade**: `StaleEngine` forwards `Embed`/`Rerank` to the
  wrapped engine with no cache lookup, no proactive serving and no
  stale fallback. An engine without the capability answers an
  honest error, never a faked vector or scoring; the server
  answers 503 in that case.
- **Errors**: a 405/501 from the single attempt surfaces as the
  upstream's own status (the taxonomy would fall back to another
  target, but the replay exemption forbids it — the one attempt
  that ran is the whole story). A content refusal is 403/`-32001`,
  a budget breach is 402, a deadline is 504, other upstream
  failures are 502/`-32603`.

## Dashboard (item 172)

- **Recent requests** (Overview) and **Request Logs** both gain an
  **Endpoint** column (after Model): `chat` / `embeddings` /
  `rerank`, from the live ledger rows — never guessed from the
  model name. The CSV export gains an `endpoint` column.
- **Live Routing feed**: a non-chat row reads
  "embeddings · via prov-a" instead of "via prov-a".
- **Live Routing trace**: a non-chat trace names its surface at
  the top — "Endpoint: embeddings — replay-exempt, exactly one
  attempt, never replayed" — and skipped targets render as
  "endpoint not offered by this target — skipped" /
  "no live client — skipped".
- Mobile: the Overview table keeps Model, Endpoint, Cached, Cost,
  Status visible; the Logs table keeps Time, Model, Endpoint,
  Provider, Cached, Latency, Cost, Status.

## Honest limitations

- Native-wire providers (Anthropic/Gemini/Cohere/Bedrock) are
  skipped for embeddings/rerank in this slice — no native
  transports built.
- Token-ID embeddings inputs get no local token estimate and no
  strict per-request-budget gate (no text to count).
- Rerank usage is usually absent upstream, so ledger rows carry
  the labelled local estimate (cost 0), not a provider-reported
  split.
- The `Summary` by-model roll-up merges the same model across
  endpoints (chat vs embeddings sharing a model ID); the endpoint
  label lives on log rows and the trace, not the roll-up.
- No context-window fit check for embeddings/rerank; no streaming
  form; no response guardrail (deliberate, see above). Pre-send
  budget uses the model-level price only (the serving provider is
  unknown before routing).
- No live-browser click-through was possible in this environment;
  the dashboard was verified by `node --check` on the extracted
  inline script and by the live smoke's HTTP checks of the feeds
  the dashboard reads.

## Gate (2026-11-18, Go 1.27.4, measured by this run)

- Full suite: **1,713 test events pass (1,371 top-level + 342
  subtests), 0 fail, 36 packages** — measured as two runs, because
  the single parallel `go test ./...` run hit the 10-minute timeout
  on another slice's `TestAsyncGuardrailCutBeforeCommit` (untracked
  `internal/server/async_guardrail_test.go`; passes alone in 0.59s,
  hangs only under full parallel load — not this slice, untouched
  here). 35 non-server packages ok in one run; server ok standalone
  in 112.8s. All 41 of this slice's new tests pass (9 provider, 11
  router, 5 ledger, 12 server, 4 degrade).
- `go test -race` clean on all touched packages (provider, router,
  ledger, degrade, routetrace, wire, cmd, server); `go vet` clean
  on touched packages; `gofmt -l` clean on this slice's files;
  `go build ./...` clean.
- `node --check` on the extracted dashboard inline script clean;
  "Playground" absent (0 occurrences).
- Live smoke **19/19** (`scripts/smoke-embeddings-rerank.py`, real
  binary + 2 Python mocks): embeddings 200 with vectors verbatim +
  usage + ledger row `endpoint: embeddings` priced $0.000008,
  rerank 200 with scores, trace `exempt:rerank`, first-target 429 →
  502 with exactly one attempt on both surfaces (fallback never
  dialled), guardrail 403/`-32001` with zero hits, strict budget
  402 pre-send with zero hits, dashboard Endpoint column present.

## Files

- `internal/provider/embeddings.go`, `internal/provider/embed_client.go` — shapes, validation, OpenAI-wire `Embed`/`Rerank`
- `internal/provider/taxonomy.go`, `internal/wire/retryable.go` — 405/501 retryable on `rerank` (and `embeddings`)
- `internal/router/embeddings.go` — `Engine.Embed`/`Rerank`, the exactly-once runner, `NoCapableProviderError`
- `internal/ledger/` — `endpoint` column: schema, migration, sync/async INSERTs, `RecentEvents`, import dedup/validate
- `internal/degrade/degrade.go` — `StaleEngine` forwarders (no cache, no stale fallback)
- `internal/server/embeddings.go` — handlers, guard/budget wiring, `recordEmbedMetrics`, 400 mapping
- `internal/server/guard.go`, `internal/server/trace.go` — two Tier 3 MANAGEMENT routes, trace endpoint passthrough
- `web/index.html` — Endpoint column, live-feed label, trace endpoint line, CSV column, mobile CSS
- `docs/embeddings-rerank.md` — this file
