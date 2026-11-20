# Slice 211–212 — routing contract on /v1/messages: gate report

Date: 2026-11-18. Go 1.27.4. Nothing was pushed to GitHub; no remote
exists in this worktree (local commits only, per the slice rules —
no commit was made by this slice: the tree holds other slices'
uncommitted work, so committing would sweep theirs in).

## The gap

The 203–204 live smoke found it and recorded it as a finding, not
its own code: `anthropicToChatRequest` dropped `routing`
(`internal/server/relay.go`), so a client-supplied `routing.order`
on `/v1/messages` was silently ignored on the translate path. This
slice closes it.

## What was built

- **Parse** (`internal/relay/relay.go`): `ParseAnthropicDetailed`
  decodes the canonical `routing` field from the Anthropic request
  body into `ParseResult.Routing` — strict, with
  `DisallowUnknownFields`, exactly like chat completions. The error
  string is identical to `provider.ChatRequest.UnmarshalJSON`'s for
  the same mistake (verified by a test that decodes the same bad
  body both ways and compares the messages). A malformed contract
  fails the parse even for a request that would otherwise relay
  verbatim: fail-closed, before any provider is dialled or any byte
  forwarded. An absent/`null`/empty `{}` contract is the same as no
  contract.
- **Forward** (`internal/server/relay.go`): `handleMessages` copies
  `parsed.Routing` onto the canonical request and sets
  `req.Endpoint = provider.EndpointMessages`, so the engine
  validates the contract (unknown provider → 400 naming it, empty
  result → 400, price ceiling → 402) and records the applied
  contract on the route trace — the same code path as chat
  completions, not a parallel implementation. The
  `X-Codvian-Routing` header form is rejected with the identical
  400 as on chat.
- **Verbatim stays contract-free — by construction, not by trust**
  (`internal/relay/relay.go`, `Decide`): new
  `Features.RoutingPresent` forces translation, so a request
  carrying a non-empty contract can never reach the verbatim path.
  A contract on a verbatim request is therefore impossible, never
  silently ignored; that is the pinned rule, documented in the
  relay package comment, the `Decide` comment, and the server
  handler comment, and covered by unit + server + live tests. Note
  the reason precedence in `Decide`: a dialect mismatch is
  reported first, so on an OpenAI target the relay header names
  the mismatch while the contract is still honoured; the
  "routing contract present" reason appears exactly where it is
  the deciding factor — a matching Anthropic target (proven live
  in the smoke, C1).
- **Cache parity** (`internal/server/relay.go`): a contracted
  messages request never consults the gateway cache — a cached
  entry holds content only, with no record of which provider
  produced it, so serving it could hand back an answer from a
  provider the contract forbade. This is the same rule the
  engine's cache wrapper enforces on chat (`internal/degrade`,
  `hasRoutingContract`); the messages path does its own lookup
  before the relay decision, so it needed its own guard, and now
  has it (pinned by `TestMessagesContractNeverServedFromCache`).
- **Trace surface** (`internal/provider/taxonomy.go`,
  `internal/router/router.go`, `internal/server/trace.go`): new
  `provider.EndpointMessages = "messages"`. The engine stamps it
  on the route trace (`traceEndpointFor`, used at all four trace
  publishes on the chat paths: non-streaming, stream worker, and
  both stream early-return publishes for rejected group/contract)
  — messages only, so chat keeps `""` (rendered as chat) and a
  fresh Responses create is NOT stamped (it fails over like chat,
  so stamping it would make the dashboard's replay-exempt line
  lie). `ReplayExemption` is untouched: messages requests are
  NOT replay-exempt and fail over like chat; `wireName()` returns
  `""` for the new kind, so the 405/501 rule is the chat rule.
- **Dashboard** (`web/index.html`): the Live Routing endpoint line
  branches on `"messages"` — "Endpoint: messages — Anthropic
  Messages dialect, translated through the engine; failover
  applies like chat" — instead of the replay-exempt line, which
  would be false for messages. The contract block itself needed
  no change: it renders from the live `GET /dashboard/trace`
  `contract` block, which the engine now fills for messages
  requests. The Overview explainer now says the contract works on
  `/v1/chat/completions` and on `/v1/messages` (translated only;
  the verbatim relay never carries one). `node --check` on the
  extracted inline script is clean; "Playground" absent (0
  occurrences).

## Tests (new: 21)

- `internal/relay/relay_test.go` (4): full contract parsed
  (order/only/sort/max_price/data_collection/group); absent /
  empty / null routing; unknown field fails closed with the
  chat-identical message; `Decide` with `RoutingPresent` forces
  translation, without it verbatim stays available.
- `internal/router/contract_messages_test.go` (4): trace endpoint
  `"messages"` on non-streaming, streaming, and rejected-contract
  (both paths) traces; chat (`EndpointChatCompletions` and zero)
  keeps `""`.
- `internal/server/messages_routing_test.go` (13): order / only /
  exclude / sort / max_price-402 / unknown-provider-400 /
  unknown-field-400 (message identical to chat's, asserted by
  decoding both error bodies) / header-400 / streaming-only /
  data_collection-deny / never-served-from-cache / verbatim-stays-
  contract-free (byte-identical + no trace contract) /
  empty-contract-is-no-contract / dashboard serves the messages
  endpoint label. Every test runs the real `Server` + real
  `router.Engine` + real provider clients against local mocks.

## Gate

- `go build ./...` — OK. `go vet` clean on touched packages.
  `gofmt -l` clean on this slice's files.
- `go test -count=1 -v ./...` — NOT fully green: **10 failures,
  all in the concurrent 229–230 (token-count audit) slice's
  in-flight code, none in this slice's code or tests** (see
  Limitations). This slice's 21 tests all pass; `go test -race`
  is clean on the touched packages (relay, router, provider,
  server targeted runs).
- Live smoke (real binary + 3 Python mocks, scratch script
  `~/workspace/.build-tmp/smoke-211-212.py`, not committed):
  **30/30 green on two consecutive runs** — verbatim
  byte-identical + no trace contract; order/only/exclude/sort
  honoured with exact mock hit counts; max_price 402 with zero
  dials and 402-trace, max_price 10 serving the cheap mock;
  unknown-provider / unknown-field / header 400s with zero dials
  and the messages 400 identical to chat's; streaming only;
  contract on the single Anthropic target translating (upstream
  body is the engine's re-encoding, `future_field` gone) and the
  next contract-free request verbatim again; dashboard label
  present, no Playground.

## Limitations

- **Full suite is red on another slice's in-flight work
  (items 229–230, token-count audit).** Their `CountPromptFull`
  change (framing + tool definitions) makes the pre-send budget
  gates count ~3x the old message-text-only estimate, so 10
  tests that encode the old counts fail in the verbose gate run
  (`go test -count=1 -v ./...`, log
  `$HOME/.tmp/full-211-212-verbose.log`: **1,976 PASS events
  (1,571 top-level + 405 subtests), 10 failing top-level tests
  (+ 8 failing subtests), 37/39 packages ok**):
  `TestBudgetGateEstimateMath`, `TestChatLedgerAndTokens`,
  `TestContextFitMathBoundaries`,
  `TestContextFitNoFitFailsHonestly`, `TestMeStatusAfterTraffic`,
  `TestReconcileUsageDivergence`,
  `TestReconcileUsageFallbackEstimate`,
  `TestRequestBudgetFailFastZeroCalls`,
  `TestRequestBudgetPreSend402`,
  `TestStreamSuccessRecordsLocalEstimate`
  (router + server packages). Those tests were already failing
  before this slice's gate in the same way (verified standalone
  mid-slice: `TestChatLedgerAndTokens` fails on the untouched
  tree with `LocalEstimatePrompt: 2` vs want `0`). The 229–230
  owner needs to update the expectations to the new counts (or
  fix the counts); this slice changed none of those paths.
  (`TestTokenCountAudit`, the 11th failure in an earlier
  non-verbose run the same day, PASSED in the verbose run —
  load-sensitive, not counted here.)
- **Minimal fix in a shared file, flagged here (allowed by the
  slice rules).** The 229–230 slice landed a build break
  mid-gate: their new `tokencount.CountPromptFull(model,
  req provider.ChatRequest)` takes a value, but the two call
  sites they updated (`internal/server/server.go:3757`,
  `internal/server/requestbudget.go:247`) pass `req`, which is a
  `*provider.ChatRequest` there. The tree would not build at
  all. I changed exactly those two call sites from `req` to
  `*req` (2 characters each) so the gate could run. That is
  their code to own: if they change the signature to take a
  pointer, the dereference must go with it. Nothing else of
  theirs was touched.
- **Benchmark: NOT re-run.** No hot-path code changed (one
  struct field parsed per messages request, one string compare
  per trace publish), so the published numbers stand.
- The trace feed after a verbatim request still shows the
  previous engine request's trace: a verbatim request never
  reaches the engine, so it publishes no trace of its own.
  That is pre-existing behaviour (the feed is the last
  *engine* request), not something this slice introduced — but
  an operator watching the feed after a verbatim request sees
  stale contract data, not an explicit "verbatim, no trace"
  state. Worth a follow-up, out of this slice's scope.
