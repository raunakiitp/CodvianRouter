# Tunnel data race fix + timing-flake hardening (CHECKLIST 189–190)

## What was wrong

Three failures flagged by three separate slices, all real, none purely
"the VM was slow":

### 1. A deterministic data race in `internal/server/tunnel.go` (item 189)

`tunnelManager.Start()` wrote `m.ln` (plus `running`/`listen`/`target`)
while a previously started `acceptLoop()` still read `m.ln` without the
lock (`m.ln.Accept()`, old line 191). The interleaving is a restart:
`Start` spawns the loop and returns; `Stop` closes the listener and
returns without waiting for the loop; the next `Start` writes `m.ln`
while the stale loop has not been scheduled yet — it then reads the NEW
listener and Accepts on it, so two loops own one listener. Beyond the
detector, that is a real bug (a stale loop stealing connections from the
new tunnel).

Fix at the root: `acceptLoop` now takes the listener as a parameter
(`acceptLoop(ln)`) and never reads `m.ln`. `Start`/`Stop` still touch
`m.ln` only under `mu`. No sleeps, no test-only workaround.

New regression test `TestTunnelRapidRestartNoRace`
(`internal/server/tunnel_test.go`): 100 start/stop cycles on a real
loopback target, dialling the tunnel every 10th cycle. It trips the race
detector 5/5 runs on the old code (4 `DATA RACE` warnings per run) and
passes 10/10 under `-race` on the fix.

### 2. A real shed-path gap, not timing — `TestEngineSlowProviderCapShrinksAndSheds` (item 189)

The test failed ~2/3 of standalone runs with a burst request returning
"adaptive concurrency limit reached" while the fast provider stood idle.
Root cause: `provider.ClassifyError` classified `adaptive.ErrLimited`
as `DecisionStop`, so `isTerminalErr` broke `ChatWithChainStrategy` out
of the attempt loop on the first shed. The shed is per-target capacity,
not a property of the request — the failure only showed when a burst
request happened to order `slow` first (i.e. it computed its order
before slow's two slots filled), which is why it looked timing-flaky.

Fix: `ErrLimited` now classifies `DecisionFallback`
(`ReasonConcurrencyLimited`, status 0, unchanged). When every target
sheds, the loop still ends with `lastErr == ErrLimited`, so
`TestEngineAllProvidersAtCapFailsOpenHonestly` is unchanged. No other
`DecisionStop` consumer is affected: the server's terminal-4xx branch
needs status 400–499 and `ErrLimited` has status 0; breaker/cooldown
paths use the reason only. Pinned by two new `TestClassifyError` cases
(bare and wrapped `ErrLimited` → fallback).

Sensitivity proof: with the old `DecisionStop` restored, the test failed
1/5 runs in one batch (2/3 in the pre-fix runs); with the fix it passes
10/10 under `-race`.

Test hardening (same test, assertions unchanged): the 10 burst
goroutines now start behind a barrier, so the burst is one contention
window instead of ten launches smeared across scheduling jitter.

### 3. Cancel-stream flakes (item 190)

(a) `TestOpenAIStreamCancelStopsUpstreamReads` — "delivered 3 chunks,
want 2". Root cause in product code: `bufio.Scanner` returns
already-buffered SSE lines without touching the network, so a chunk
buffered before the abort was delivered after it. Fix: the OpenAI
client's `emit` (and the Anthropic client's `emit`, same shape via
`wire.readSSE`) now returns `ctx.Err()` unwrapped before invoking
`onChunk` when the caller's ctx is done. The error passes through
`mapAnthropicError` unwrapped, so the router still recognises the
cancellation. Test fix: the brittle fixed total bound (`totalWrites <=
6`, which assumed the client kept up with the 30 ms producer) is now
time-derived — pre-abort lines bounded by elapsed time / 30 ms tick +
2, at most 3 lines inside the 500 ms grace, zero after it. The
exact-2-delivered and zero-writes-after-grace assertions are unchanged.

(b) `TestStreamCancelStopsUpstreamReads` (router) — the buffered-channel
send race in `bridgeStream`: `send` could still deliver a stale delta
after the abort whenever the send case won the select (the channel is
buffered 16). Fix: `send` checks `ctx.Err()` before enqueueing. Test
fix: exact-equality `got == "t1 t2 "` becomes a prefix assertion plus a
delivery-side no-delta-after-grace check (no delta arrival after
`cancelAt` + 150 ms). A delta already in flight when the cancel landed
may still arrive — that is not a violation — but nothing may arrive
after the grace window. The upstream-emissions-after-grace,
terminal-`context.Canceled`, no-failover and trace-`cancelled`
assertions are unchanged.

## Files changed

- `internal/server/tunnel.go` — `acceptLoop(ln net.Listener)`.
- `internal/server/tunnel_test.go` — `TestTunnelRapidRestartNoRace`.
- `internal/provider/taxonomy.go` — `ErrLimited` → `DecisionFallback`
  (+ `ReasonConcurrencyLimited` doc).
- `internal/provider/taxonomy_test.go` — two pinning cases.
- `internal/provider/client.go` — OpenAI `emit` ctx check.
- `internal/provider/anthropic_client.go` — Anthropic `emit` ctx check.
- `internal/router/router.go` — `bridgeStream.send` ctx pre-check.
- `internal/router/concurrency_test.go` — burst start barrier.
- `internal/provider/cancel_test.go` — time-derived total bound.
- `internal/router/cancel_test.go` — prefix + no-delta-after-grace.
- `CHECKLIST.md` — rows 189–190. `README.md`, `README.v2.md` —
  status line (this slice's gate numbers).

No dashboard change: no user-visible status changes in this slice (no
new feed, no new card). No sample/fake data anywhere.

## Gate

- `go test -race -count=1 ./...` — exit 1, **33 packages ok, 0 data races
  anywhere in the run** (log `$HOME/.tmp/race-gate-189-190.log`, ~11 min).
  NOT fully green — the 3 failing packages are all outside this slice,
  each re-verified in isolation afterwards:
  - `internal/server` — `TestAsyncGuardrailCutsStream`,
    `TestChatCompletionsCompressionLossless`,
    `TestRouteTableSpawnCapableAreLocalOnly` (duplicate key-routes in
    the guard table, 72 entries vs 64 expected — a concurrent slice
    landing key-management routes mid-gate). All 3 pass 3/3 in
    isolation under `-race`. The tunnel tests in the same package run
    all pass under `-race`.
  - `internal/feedback` — `TestTrainingPairs` (pairs returned swapped).
    Passes 3/3 in isolation under `-race`.
  - `internal/health` — `TestHistoryPrunesOnWrite` hit the 10 min
    test timeout (9m40s in the test). Pre-existing and unrelated: the
    test does 2,980 whole-file rewrites with fsync (+ backup copy) and
    takes >120 s even WITHOUT `-race` in isolation on this VM. Flagged
    for the health slice owner; not touched here (out of scope).
- Previously-flaky tests, each 10/10 under `-race`:
  `TestEngineSlowProviderCapShrinksAndSheds`,
  `TestStreamCancelStopsUpstreamReads`,
  `TestOpenAIStreamCancelStopsUpstreamReads` (+ Anthropic sibling).
- Tunnel tests 10/10 under `-race` (`TestTunnelRapidRestartNoRace`
  included).
- Full non-race suite — `go test -count=1 -v ./...` exit 1, **1,711 test
  events pass (1,369 top-level + 342 subtests), 34/36 packages ok**
  (log `$HOME/.tmp/full-gate-189-190.log`). The 2 failing packages are
  the same other-slice tests as in the race gate:
  `internal/feedback` `TestTrainingPairs` and `internal/server`
  `TestAsyncGuardrailCutBeforeCommit` ("trace is nil") — both pass 3/3
  in isolation. `internal/health` passed this run (315.9 s — slow, not
  hung). `go vet` clean on this slice's packages; `gofmt -l` clean on
  this slice's files.

## Honest limitations

- The shared worktree churned throughout this slice: concurrent slices
  broke the build several times mid-gate (`router_test.go`
  `ExemptResponses`, `server.go` duplicate `modelsDevStatusJSON` /
  missing `responses` handlers, `registry.go` arity, `embeddings.go`
  `RouteTrace.Endpoint`, `compression_test.go` field/method collision).
  Each passed on re-run after the owning slice landed; none were in
  this slice's files and none were touched here.
- `internal/health` `TestHistoryPrunesOnWrite` exceeds the 10 min test
  timeout under `-race` (and takes >120 s without `-race`): 2,980
  whole-file rewrites with fsync per record. Pre-existing, unrelated
  to this slice (health imports only `internal/provider` types; none of
  this slice's changes touch its I/O path), flagged for the health
  slice owner rather than fixed here.
- The tunnel race never reproduced in this slice's own isolated runs
  (single test, full server package) — it was reproduced deterministically
  (5/5, 4 warnings/run) only by the new rapid-restart test on the old
  code. The 165 slice's report (exact lines cited) plus the code
  inspection agree on the mechanism.
- Gemini/Cohere/Bedrock stream readers share the buffered-delivery
  shape (`wire.readSSE` has no ctx parameter; Bedrock reads its event
  stream off the same ctx-bound body). Only the OpenAI and Anthropic
  clients were fixed — those are the ones with flaky cancel tests.
  The same `emit`-level ctx check would apply there if their cancel
  tests ever flake.
- No live smoke for this slice beyond the race gate: the tunnel still
  works end to end via its existing tests (`TestTunnelForwardsRealRequest`
  forwards a real HTTP request through the tunnel with byte counts).
