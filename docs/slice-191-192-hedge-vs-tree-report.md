# Slice 191–192 — Hedge vs explicit chain order

## The tension (found live by the 177–178 slice)

The engine's default 150 ms hedge races the second attempt in the
chain. When the chain order is a *stated preference* — a strategy
tree's cheapest-first pick, a contract `order`, a group member order —
a first pick slower than 150 ms loses to the hedge: the 177–178
slice's live trace showed `hedged_cancelled` for the tree's first
pick. The operator asked for cheapest-first and got fastest-first
instead, silently. The same happens on a flat cheapest chain — but
there the order is engine-computed, not stated (see the boundary
below).

## Decision: explicit-order chains get NO hedge

When the chain order is EXPLICIT (per-request contract `order`,
routing-group member order, or strategy-tree order), the engine
launches no hedge at all — on both the non-streaming and streaming
paths. The alternative considered and rejected was extending the
hedge delay beyond the first pick's expected latency: any delay is
still a race, the first pick's expected latency is only an EWMA
estimate, and any first pick slower than the extended delay would
still lose — so it cannot guarantee the stated order. Only disabling
the hedge guarantees it.

The cost is explicit and bounded: an explicit-order request loses
the hedge's latency insurance and waits out a slow first pick;
sequential failover on an actual failure is unchanged. The default
(engine-computed) chain keeps the 150 ms hedge exactly as before.
The boundary is *who stated the order*: a flat cheapest chain still
hedges, because the engine computed that order itself — an operator
who wants cheapest-first *guaranteed* uses a contract order, a group,
or a tree, and the hedge is off by design. This is the same rule as
the existing preserveOrder fix (items 157/173): scoring may not
re-rank an explicit order; hedging is the same class of silent
override, at runtime instead of at ordering time.

## What changed

- `internal/routetrace/routetrace.go` — `RouteTrace` gains `Hedge`
  (`"armed"` | `"disabled-explicit-order"` | `"disabled-exempt"`,
  `""` when no hedge decision was recorded — rejected contract,
  empty chain, single-attempt chain) and `HedgeDelayMs` (the real
  configured delay when armed, 0 otherwise), plus the three
  constants. The value is a policy decision stamped before the
  attempt loop; whether the hedge actually fired is visible in the
  attempts (`hedged_cancelled`).
- `internal/router/router.go` — `ChatWithChainStrategy` and
  `runStream` compute `hedgeOff := explicitOrder || treeOrdered`
  and add `!hedgeOff` to both hedge launch conditions; both stamp
  the trace via the new `Engine.stampHedgeMode` helper. The decision
  is documented at the `hedgeOff` definition in
  `ChatWithChainStrategy`. No other behaviour changed: failover,
  budget gate, breaker, cooldown, tail partitions, and the refusal
  chain are untouched (the hedge only ever fires at idx 0, before
  any refusal switch).
- `internal/server/trace.go` — `GET /dashboard/trace` carries
  `hedge` + `hedge_delay_ms`.
- `web/index.html` — Live Routing trace (`#liveTrace`, `#logTrace`)
  renders one hedge line above the attempts: "Hedging armed — the
  first attempt is raced against the second after 150 ms." (real
  delay from the feed, never hard-coded), "Hedging off — explicit
  chain order (contract, group, or strategy tree): the first pick
  is never raced by a hedge.", or "Hedging off — replay-exempt
  request, exactly one attempt." No line when no decision was
  recorded. `node --check` on the extracted inline script clean;
  "Playground" absent.
- `scripts/smoke-hedge-explicit-order.py` — live smoke (below).

## Tests

New: `internal/router/hedge_test.go` (7 tests) +
`internal/server/hedge_trace_test.go` (1 test).

- `TestExplicitOrderContractChainNeverHedges` — contract order
  [slow 300 ms, fast], hedge 20 ms: slow serves, fast 0 calls, no
  `hedged_cancelled`, trace `hedge=disabled-explicit-order`.
- `TestExplicitOrderGroupChainNeverHedges` — same via group "fast".
- `TestExplicitOrderTreeChainNeverHedges` — cheapest-of[a, b] tree
  picks slow b first: slow serves, fast 0 calls, no
  `hedged_cancelled`, tree path ends at b.
- `TestDefaultChainStillHedges` — flat chain: fast wins, `hedged_cancelled`
  for slow, `hedge=armed`, `hedge_delay_ms=20`.
- `TestFlatCheapestChainStillHedges` — flat cheapest (engine-computed
  order): fast still wins the hedge — pins the decision boundary.
- `TestExplicitOrderStreamNeverHedges` / `TestDefaultStreamStillHedges`
  — streaming analogues of the contract-order and default cases.
- `TestDashboardTraceCarriesHedge` — the feed carries `hedge` /
  `hedge_delay_ms` for armed and explicit-order traces, and omits
  them before any request.

Sensitivity (all four explicit-order tests fail with the rule
removed — `!hedgeOff` deleted from both launch conditions — the
hedge wins and "fast" serves; all pass with it restored).

## Live smoke

`scripts/smoke-hedge-explicit-order.py` — real binary + 2 Python
mocks (slow 300 ms, fast instant, model `m`), 21/21 checks green:

- S1 contract order [slow, fast]: 200 from slow, fast 0 hits,
  trace `hedge=disabled-explicit-order`, no `hedged_cancelled`.
- S2 tree order (cheapest, slow priced $1 vs fast $50): 200 from
  slow, fast 0 hits, same trace, tree path ends at the slow
  provider.
- S3 group order `cheap-first`: 200 from slow, fast 0 hits, same
  trace.
- S4 flat cheapest (`routing.sort="price"`, engine-computed):
  200 from fast in 3 ms, both providers dialled, trace
  `hedge=armed`, `hedge_delay_ms=150`, slow `hedged_cancelled`.
  (The hedge proof is the trace's own latencies, not wall time:
  S4 is the first chat after a restart, so the wall clock also
  pays the one-time local token-counter load.)
- S5 streaming contract order: slow chunk, fast 0 hits, trace
  `hedge=disabled-explicit-order`, no `hedged_cancelled`.
- S6 dashboard serves the "Hedging off … never raced by a hedge"
  line; no Playground.

## Gate

- `go build ./...` clean; `go vet` clean on `internal/router`,
  `internal/routetrace` (see the concurrent-slice notes below for
  the server vet caveat).
- `go test -race -count=1 ./internal/router/ ./internal/routetrace/`
  clean (router 49.9 s, routetrace 1.5 s).
- Full suite (verbose, 2026-11-18, Go 1.27.4): `go test -count=1
  -v ./...` — EXIT 0, **1,869 PASS events (1,483 top-level + 386
  subtests), 0 fail, 38/38 packages** (log
  `$HOME/.tmp/fulltest-191-192-verbose.log`). The non-verbose run
  just before it measured 36/38 with two failures in other slices'
  in-flight code — a health test-main reference to a not-yet-landed
  test (`TestHistoryCorruptPrimaryDoesNotOverwriteBackup`) and the
  route-table count (77 vs 76: the plugins slice's new
  `/dashboard/plugins` route landed before its count update) — both
  healed while the verbose run was going (the plugins slice updated
  its count; the health reference landed), and the verbose re-run is
  fully green.
- `gofmt -l` clean on this slice's files.

## Concurrent-slice breakage (not this slice, flagged)

The shared tree went red three times during this slice's gate,
each time on another slice's in-flight files, each time healing
itself when that slice landed:

1. CHECKLIST 195 key-management slice: route registrations for
   `GET /v1/me/status`, `POST /admin/keys/{id}`,
   `POST /admin/keys/{id}/rotate` + a `keyFingerprint` call landed
   before the handlers. This slice briefly added honest-501 stubs
   in `internal/server/checklist195_buildfix.go` to unblock its
   gate, then deleted the file the moment the real handlers landed
   in `internal/server/keystatus.go` (verified: the stubs are gone,
   the real file is in place, nothing of this slice remains there).
2. CHECKLIST 201 plugins slice: `GET /dashboard/plugins`
   registered before `handlePlugins` landed in
   `internal/server/plugins.go` (~90 s later).
3. Health-history slice: `internal/health/history.go` referencing
   `persistCount` / `lastDurable` / `persistHistoryDurable` fields
   that had not landed yet, breaking `internal/health` and, through
   it, `degrade`, `server`, and `cmd/codvian-router` — and
   `internal/server/server.go` with a half-written function (missing
   `func` header at the `handleChatCompletions` site) mid-save.
   The full-suite gate ran after these healed: the non-verbose run
   caught two of them mid-flight (see Gate above) and the verbose
   re-run is fully green.

Pre-existing, unrelated: `go vet ./internal/server/` reports a
test-only import cycle through another slice's untracked
`internal/server/compression_cache_test.go` (imports `degrade`,
which imports `server`); `go test ./internal/server/` itself
builds and passes. Not this slice's file, left untouched.

## Honest limitations

- An explicit-order request that names a slow first pick now waits
  for that pick's own failure/timeout instead of being raced —
  that is the decision, not a bug, but an operator who configures
  a tree over a flaky slow provider will feel the latency.
- `hedge=armed` is a policy stamp: it says the hedge was armed,
  not that it fired. A fast first pick on a default chain shows
  `armed` with no `hedged_cancelled` row — the attempts are the
  record of whether it fired.
- The smoke's S4 wall-time check was replaced by a trace-latency
  check (see S4 note): wall time after a restart is not a clean
  hedge signal on this VM.
