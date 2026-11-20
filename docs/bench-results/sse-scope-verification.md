# SSE batching scope verification — "3 tokens / 8 ms"

Date: 2026-11-19 ~16:15 IST. Verifier: BENCH-RUN-3 (Benchmark Department).
Method: code reading only (grep + read of the handlers); no measurement,
no quiet window needed. Tree: HEAD `035d1ed` (M9 merge wave), Go 1.27.4.

## Verdict

The "3 tokens / 8 ms" hybrid batching is REAL, and its scope is **two**
stream handlers, not one:

1. `handleChatStream` — `POST /v1/chat/completions` (streaming),
   `internal/server/server.go:5981`, batcher built at lines 6032–6044.
2. `handleResponsesStream` — Responses API streaming,
   `internal/server/responses.go:1477`, batcher built at lines 1531–1543.

**Correction to this task's brief:** the brief said the claim is "real
ONLY for handleChatStream". That is out of date. `handleResponsesStream`
constructs the same `sseBatcher` with the same config
(`s.sseBatching.maxTokens`, `s.sseBatching.maxDelay`), the same
`writeSSE` (batched) / `writeSSENow` (immediate, terminal frames) split,
and the same `defer batch.Close()`. The startup banner in
`cmd/codvian-router/main.go:4078` and the `WithSSEBatching` comment in
`cmd/codvian-router/main.go:3775–3777` both already say "chat +
Responses streams".

**No over-broad user-facing claim was found in the current tree.** The
surfaces that state the scope already state the correct two-handler
scope:

- `README.md:77` and `README.v2.md:155`: "Token events are
  hybrid-batched **in the chat-completions stream handler
  (`handleChatStream`) and in the Responses API stream handler
  (`handleResponsesStream`), which shares the same batcher** — no other
  stream handler uses this batcher: one `Flush` per 3 tokens or 8 ms,
  whichever comes first (`[DONE]` and error events always flush
  immediately)".
- `web/index.html` streaming section (comment + `loadStreaming()` note):
  "Batching (in the chat-completions stream handler handleChatStream
  and the Responses stream handler handleResponsesStream only — no
  other stream handler uses this batcher)".
- `cmd/codvian-router/main.go:4078` (serve startup banner): "hybrid
  batching on the chat + Responses streams (flush ≤8ms or ≤3 tokens;
  CODVIAN_ROUTER_SSE_BATCH=0 disables for measurement)".

Nothing in README / README.v2 / web/index.html / the startup banner
claims batching for any other stream. No fix is needed in those files
on scope grounds (the docs agent owns them regardless; this file only
records the finding).

## The exact batching code

`internal/server/sse_batch.go` (package `server`):

- Constants, lines 43–51:
  - `sseBatchMaxTokens = 3` (line 45) — most events one `Flush` carries.
  - `sseBatchMaxDelay = 8 * time.Millisecond` (line 47) — longest a
    token waits for company.
- `newSSEBatcher(writeLine, flush, maxTokens, maxDelay, after)` (line
  91): builds the batcher; `after == nil` means `time.AfterFunc`.
- `(*sseBatcher).Write` (line 110): appends to `pending`; flushes
  synchronously when `len(pending) >= maxTokens`; arms the `maxDelay`
  timer when the batch goes from empty to one pending event.
- `(*sseBatcher).Flush` (line 131) / `Close` (line 140): deliver
  everything now; `Close` also marks the batcher closed so later
  writes go straight through. Both handlers `defer batch.Close()`,
  so no token is ever left buffered past handler return.
- `onTimer` (line 151) / `flushLocked` (line 163): timer fires at
  most 8 ms after the first pending event; writes all pending lines
  in order, then calls the underlying `Flush()` once.

Configuration / defaults:

- `internal/server/server.go:1299`: server default is
  `sseBatchConfig{enabled: true, maxTokens: sseBatchMaxTokens,
  maxDelay: sseBatchMaxDelay}`.
- `internal/server/streaming.go:41–49` (`WithSSEBatching`): the option
  sets `enabled` and re-pins `maxTokens`/`maxDelay` to the constants.
- `cmd/codvian-router/main.go:3447–3456` (`sseBatchingEnabled()`):
  batching is on by default; `CODVIAN_ROUTER_SSE_BATCH=0|false|off`
  disables it (measurement only — `scripts/bench-sse.sh` sets it to 0
  for the "before" run). Wired at `main.go:3780`
  (`server.WithSSEBatching(sseBatchingEnabled())`).

Terminal-frame bypass (both handlers, identical shape):

- `writeSSENow` (`server.go:6077–6094`, `responses.go:1569–1586`):
  flushes any pending batch first, then writes the line and flushes
  immediately. `[DONE]` and error events go through `writeSSENow`,
  never through `batch.Write`, so a stream close is never held up to
  8 ms. With batching disabled, `writeSSE` falls back to
  write-then-flush-per-token.

## Where batching does NOT apply — every other SSE emitter

Exhaustive grep for `text/event-stream` as a response `Content-Type`
across `internal/` + `cmd/` (non-test code) finds 8 sites. Only the
two above use the batcher. The other six flush per event/chunk:

| Handler | File:line (SSE header) | Route / purpose | Flush behaviour |
|---|---|---|---|
| `forwardVerbatim` | `internal/server/relay.go:1429` (header); flush loop ~1190–1202 | Byte-for-byte native relay of an upstream stream (verbatim passthrough) | `flusher.Flush()` after every 32 KiB upstream read chunk; no batcher |
| `handleMessagesStream` | `internal/server/relay.go:1429` (header); `flush()` ~1442 | Anthropic `/v1/messages` stream (translated) | Local `flush()` per emitted event; no batcher |
| `geminiGenerateStream` | `internal/server/gemini_native.go:1212` | Gemini native streaming | `emit` writes one event + `flusher.Flush()` per delta; no batcher |
| `handleA2AStream` | `internal/server/a2a.go:993` | A2A agent-to-agent stream | `fl.Flush()` per event; no batcher |
| `handleMCPSSEPost` | `internal/server/mcp_http.go:763` | MCP SSE (legacy POST endpoint) | `flusher.Flush()` per event; no batcher |
| `handleMCPStreamGet` | `internal/server/mcp_http.go:991` | MCP Streamable HTTP (GET) | `flusher.Flush()` per event; no batcher |

Notes on near-misses, checked and excluded:

- `handleMessagesTranslated` (`relay.go:1258`) translates an Anthropic
  request to the canonical form; its streaming half calls
  `handleMessagesStream` (relay.go:1305), its non-streaming half calls
  `s.engine.Chat` (relay.go:~1317) — it never touches the batcher
  itself.
- `responses.go:1763`, `responses.go:1875`, `server.go:6115–6116`,
  `server.go:6210–6257`, `server.go:6360–6370` are `writeSSENow`
  call sites *inside* the two batched handlers (terminal `[DONE]` /
  error / refusal frames) — expected bypasses, not separate paths.
- `internal/server/batches.go:848–849` flushes bufio writers for the
  Batch API file output, not a client SSE stream.

So a claim such as "the router batches SSE delivery at 3 tokens /
8 ms" is true only when read as "on the two OpenAI-compatible
streaming endpoints (chat completions + Responses)". It is NOT true
for the native relay, Anthropic messages stream, Gemini native
stream, A2A stream, or MCP streams — those are flush-per-event.

## Measured numbers (from the recorded run, not re-measured here)

Source: `docs/sse-delivery-2026-11-17.json` (rig `scripts/bench-sse.sh`,
200 tokens per cell, real binary + mock streaming at a known cadence,
2026-11-17 ~20:50 UTC; `batching: {max_tokens: 3, max_delay_ms: 8}`,
`tcp_nodelay.verified: true`). Per-token delivery latency = client
receive time minus the mock's emit timestamp embedded in the token.

| Mock cadence | Batching off (median / p99) | Batching on (median / p99) |
|---|---|---|
| 10 ms | 0.357 ms / 25.548 ms | 8.536 ms / 9.943 ms |
| 1 ms | 0.211 ms / 3.16 ms | 1.302 ms / 2.853 ms |

Honest reading (same as the code header, README, and dashboard):
batching is a throughput trade (~3x fewer `Flush` calls under fast
streams), NOT a latency win — at a 10 ms cadence each token waits the
full 8 ms timer (its successor is 10 ms away, so the 3-token bound
never fires first), so the "on" median is worse; the p99 tightens
because the timer regularises delivery against scheduler stalls.
There is no latency target for token delivery, so nothing here is
scored MET/MISSED. The run was taken on a storm-loaded shared VM
(loadavg 13.29 at run start), so the tail is noisy.

## Stale references found (minor; for the docs agent, not fixed here)

1. `internal/server/sse_batch.go:27` cites
   `docs/sse-delivery-2026-11-18.json` — that file does not exist.
   The recorded run is `docs/sse-delivery-2026-11-17.json` (also the
   file README.md:77, `docs/slice-sse-dashboard-report.md`, and
   `internal/server/testdata/streaming-2026-11-17.json` cite).
2. `CHECKLIST.md:142` (item 135 text) describes hybrid batching as
   "in `handleChatStream`" only — written before `handleResponsesStream`
   adopted the same batcher. The README/dashboard/main.go wording is
   already correct; the checklist row is the only stale scope
   statement found. (CHECKLIST.md is the coordinator's source of
   truth; left untouched by this verification.)

## Tests pinning the bounds

`internal/server/sse_batch_test.go`: 7 batcher-bound tests (fake
clock), plus end-to-end batched-stream and feed tests in the same
slice (CHECKLIST 135–136 gate: 15 new tests, all pass incl. `-race`).
The bounds are therefore pinned by test as well as by the constants
above.
