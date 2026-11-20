# Slice report — items 165–166: panic isolation on the request/stream path

Date: 2026-11-18 (IST). Builder slice: items 165–166, "panic isolation".
Worktree: `~/workspace/codvian-router/`. No GitHub writes made. No files
outside the worktree touched. No push (standing rule: no push until the
whole build is complete).

## What was built

A panic in a provider client, a wire chunk converter, a routing hook,
or the engine itself can never kill the gateway process. `recover()`
guards sit at every boundary of the request/stream path, innermost
first:

| Boundary | Location label | File |
|---|---|---|
| `callChat` (non-stream attempt) | `provider_chat` | `internal/router/router.go` |
| `bridgeStream` (stream attempt, incl. wire chunk converters) | `provider_stream` | `internal/router/router.go` |
| `ChatWithChainStrategy` (engine machinery: scoring/order hooks, preread) | `engine_chat` | `internal/router/router.go` |
| `runStream` (engine machinery) | `engine_stream` | `internal/router/router.go` |
| `handleChatCompletions` | `server_chat` | `internal/server/server.go` |
| `handleChatStream` | `server_stream` | `internal/server/server.go` |
| `handleMessages` | `server_messages` | `internal/server/relay.go` |
| `handleMessagesStream` | `server_messages_stream` | `internal/server/relay.go` |

Design (locked in, all in the code):

- **Typed panic.** A recovered panic is a `*router.PanicError`
  (`internal/router/panic.go`). `Error()` returns only a fixed
  location string, never the recovered value — so traces, the
  dashboard, and client errors stay clean. The value is available
  via `Value()` for the one redacted log line.
- **Failover, not terminal.** `ClassifyError` has no case for it,
  so it falls to the default `DecisionFallback` /
  `ReasonTransportError` — a panicking provider fails over like any
  other failure. The trace reason is overridden to
  `ReasonPanicRecovered` (`"panic_recovered"`, new in
  `internal/provider/taxonomy.go`) so the dashboard does not lie.
- **Counted exactly once.** Each panic is counted at the boundary
  that catches it, in the real counter
  `codvian_router_panics_total{location}` (`internal/metrics`:
  `IncPanic` + `Snapshot.Panics` + `panics_since_start` in
  `GET /dashboard/overview`; honest zero = no series rendered).
  Wired in `cmd/codvian-router/main.go` via
  `engine.SetPanicMetrics(recorder)`.
- **Logged exactly once, redacted.** The value rides the
  `PanicError` to the server, which logs it through
  `guardrails.Redact` — never request bodies/keys. `logPanicError`
  skips `server_*` locations (already logged by `noteServerPanic`
  at recovery).
- **Billing still records.** Engine guards take the normal failure
  path (ledger row, zero tokens/cost). `runStream` records a row +
  trace entry only when no trace entry was added this iteration.
  The server recover records a backstop row (provider `"unknown"`,
  failed, fresh background ctx — the request ctx's cancel already
  fired during unwind) iff the engine was never called or the trace
  is empty. No double rows: trace and ledger rows are written
  together, in that order.
- **Honest client errors.** Stream: terminal SSE error event +
  `[DONE]` (CHECKLIST 153 always-terminal); no failover after
  commit. Non-stream, engine panic: `"upstream provider error"`
  via `handleEngineError` (the provider client faulted, and it
  counts as an upstream failure). Non-stream, server's own recover:
  `"gateway internal error"` via `writePanicFailure` (the gateway
  faulted, deliberately NOT an upstream failure).
- **No new routes**, so no guard-table / OpenAPI changes.

Dashboard (item 166): the Provider health card gains a "Recovered
panics" footer (same footer style as the Live routing card) fed by
live `GET /dashboard/overview` `panics_since_start` — `0` reads
"No panics recovered since start — …" and `>0` reads "Recovered
panics on the request/stream path — …". The Live routing trace
renders `panic_recovered` attempts as "panic in provider —
recovered, fell back". `node --check` on the extracted inline
script clean; "Playground" absent (0 occurrences).

## Baseline check

`grep recover()` across `internal/` before building confirmed the
sweep's claim: `recover()` existed only in tests, in
`internal/eval/eval.go:172` (eval harness, item 26), and in
`internal/autorouter/autorouter.go:768` (ranker guard). No
`recover()` in production server/provider/router code.

## Tests (all new, all passing)

- `internal/router/panic_test.go` (8 tests):
  `TestCallChatPanicFailsOverToNextProvider`,
  `TestCallChatPanicSingleProviderReturnsPanicError`,
  `TestPanicErrorClassificationIsRetryable`,
  `TestBridgeStreamPanicBeforeCommitFailsOver`,
  `TestBridgeStreamPanicAfterCommitEndsStream`,
  `TestChatWithChainPanicInOrderHookRecovered`,
  `TestRunStreamPanicInOrderHookRecovered`,
  `TestPanicMetricsNilSinkIsSafe`,
  `TestEngineSurvivesRepeatedPanics` (10 panicking requests, 10
  counted — each targets a different provider so the circuit
  breaker, which correctly opens after 5 consecutive failures on
  one provider, never masks a panic),
  `TestPanicErrorValueNeverLeaksIntoTrace` (panic value is a
  key-shaped string; the trace never contains it).
- `internal/metrics/metrics_test.go`: `TestPanicsCounter`
  (per-location increments, honest zero = no series, total in
  `Snapshot`, label sanitisation).
- `internal/server/panic_test.go` (8 tests):
  `TestChatCompletionsSurvivesPanickingProvider` (real engine,
  200 via fallback, 2 ledger rows, `provider_chat:1`,
  `panics_since_start:1`, second request 200),
  `TestChatCompletionsPanicSingleProvider502AndLedgerRow` (502
  `"upstream provider error"`, failed ledger row, second request
  still answered),
  `TestChatStreamSurvivesMidStreamPanic` (SSE `"hel"` + terminal
  error frame + `[DONE]`, no fallback content after commit,
  `provider_stream:1`),
  `TestServerRecoversWhenEnginePanics` (engine panics once then
  succeeds: first 502 `"gateway internal error"`,
  `server_chat:1`, backstop ledger row provider `"unknown"`,
  second request 200),
  `TestServerRecoversWhenChunkEnginePanics` (SSE error frame
  `"gateway internal error"` + `[DONE]`, `server_stream:1`,
  second stream 200),
  `TestChatCompletionsHookPanicLeavesLedgerRow` (502, no engine
  rows, backstop row present, `engine_chat:1`),
  `TestOverviewPanicsZeroBeforeAnyPanic`,
  `TestPanicValueRedactedInLog` (key-shaped panic value appears
  in neither the 502 body nor the server log; the log carries
  "panic recovered in server_chat").

## Gate (2026-11-18, Go 1.27.4)

- `go build ./...` clean.
- `gofmt -l` clean on this slice's files.
- `go vet ./...`: **fails on 4 pre-existing `lostcancel`
  warnings in `internal/router/cancel_test.go`** (CHECKLIST 153,
  landed 2026-11-17 21:50, before this slice started) — not this
  slice's files, left untouched. Vet on this slice's touched
  packages (`server`, `metrics`, `provider`, `reasoning`, `cmd`)
  is clean.
- Full `go test -count=1 -v ./...`: **1,424 test events pass
  (1,131 top-level + 293 subtests), 0 fail, 31 packages**
  (`$HOME/.tmp/fulltest-v.out`, EXIT 0). The non-verbose
  confirmation run is also fully green (31/31).
- `go test -race` on this slice's own tests
  (`-run 'Panic|OverviewPanics'` on router/server/metrics):
  **clean**.
- Full `go test -race` on router/server/metrics/provider/cmd:
  **EXIT 1 — three failures, all pre-existing and outside this
  slice** (log `$HOME/.tmp/race.out`):
  1. `TestEngineSlowProviderCapShrinksAndSheds` (router,
     adaptive concurrency): timing-sensitive assertion, flaky
     under race — FAIL / ok / FAIL on three re-runs. No data race
     reported.
  2. `TestOpenAIStreamCancelStopsUpstreamReads` (provider):
     "client delivered 3 chunks, want exactly 2" — a chunk raced
     the cancel under race timing. Flaky — FAIL / ok / ok on
     three re-runs. No data race reported.
  3. `TestTunnelGuardAndValidation` (server): **a genuine,
     deterministic data race in `internal/server/tunnel.go`** —
     `tunnelManager.Start()` writes the listener/running state
     (`tunnel.go:123–128`) while a previously started
     `acceptLoop()` still reads it (`tunnel.go:191`). Reproduces
     on every race re-run. This is the tunnel slice's code
     (items 78–92); it was left untouched per the disjoint-files
     rule and is flagged here for the Fleet Manager / tunnel
     slice owner.
- Live smoke `scripts/smoke-panic-isolation.py`: **12/12 PASS**
  (real binary, isolated HOME, two Python mocks — one live, one
  500): failing-first request fails over to live (200, dead hit
  once / live hit once), second request 200, stream 200 with
  content + `[DONE]`, `/metrics` has no panics series,
  `panics_since_start == 0`, dashboard serves the "Recovered
  panics" card with its honest-zero text, trace feed serves
  attempts.

## Honest limitations

- **No HTTP-reachable input was found that panics the
  production chunk converter.** It is defensive:
  `wire.TranslateStream` returns errors, it does not panic. So
  panic behaviour is proven in-process by unit tests with
  deliberately panicking clients/hooks/engines; the live smoke
  covers the healthy path + the zero-count feed + survival across
  a failing first request. If a real panicking upstream input is
  ever found, the live smoke should be extended to drive it.
- `handleMessages`' recover writes the 502 unconditionally:
  headers are unsent in every reachable case except a
  post-response panic, which would produce one superfluous
  `WriteHeader` log line. Accepted.
- The stale-trace edge: if a request panics in a hook after a
  previous request already published a trace, the server's
  backstop-row check reads the *fresh* trace only because
  `ChatWithChainStrategy`'s recover now publishes its
  (attempt-less) trace via `setLastTrace`. Verified safe: no
  panic point in that function runs under `e.mu` (all lock sites
  are brief bookkeeping; hooks and clients run lock-free).

## Blockers found in other slices' work (flagged, not fixed)

1. **The tree was broken when this slice's gate ran.**
   `internal/router/reasoning.go` (CHECKLIST 163–164, landed
   2026-11-17 22:04, after this slice's first green build)
   references an `e.reasoning` field that did not exist on the
   Engine — the package did not compile, so no router test could
   run at all. The field definition was lost in a concurrent
   edit to `router.go`. **Minimal repair made in this slice**
   (documented in the code): added `reasoning *reasoning.Store`
   to the Engine struct + `reasoning.New()` in `NewEngine`
   (the documented defaults: TTL 2h, cap 200, per
   `reasoning.go`'s header) + the import. The type and the
   initialiser are unambiguous from `reasoning.go` itself
   (`Stats`/`Get`/`Put`/`NoteInjected` all exist on
   `*reasoning.Store`; every helper nil-checks the field). The
   163–164 slice owner should confirm the wiring matches their
   intent; the coordinator should check why the field edit was
   lost (stale-copy overwrite is the known failure mode).
2. **`go vet ./...` already fails** on the pre-existing
   `cancel_test.go` lostcancel warnings (see Gate).
3. **The tunnel data race** (see Gate) is a real bug in another
   slice's code, reproducible on every race run.
4. **CHECKLIST numbering:** the pre-slice summary claimed the
   table ended at item 150 and 151–164 did not exist. Wrong:
   rows 151–168 exist, appended after row 150 in landing order
   (not numeric order). Rows 165–166 were free and were appended
   after row 168.

## Files changed

- New: `internal/router/panic.go`, `internal/server/panic.go`,
  `internal/router/panic_test.go`,
  `internal/server/panic_test.go`,
  `docs/slice-165-166-panic-isolation-report.md` (this file),
  `scripts/smoke-panic-isolation.py`.
- Edited: `internal/router/router.go` (guards + trace reason +
  `reasoning` field restore — see blockers),
  `internal/server/server.go` (handler recovers, overview
  `panics_since_start`), `internal/server/relay.go` (Messages
  recovers), `internal/metrics/metrics.go` (+ test),
  `internal/provider/taxonomy.go` (`ReasonPanicRecovered`),
  `cmd/codvian-router/main.go` (`SetPanicMetrics` wiring),
  `web/index.html` (panic card, trace label),
  `CHECKLIST.md` (rows 165–166), `README.md`, `README.v2.md`
  (status line: 1,424 test events pass, 0 fail, 31 packages).

## What remains

- Nothing for this slice: items 165–166 are Built and Verified.
  The open items belong to other slices: the tunnel data race,
  the two flaky race-timing tests, the `cancel_test.go` vet
  warnings, and the 163–164 slice owner's confirmation of the
  restored `reasoning` field.
