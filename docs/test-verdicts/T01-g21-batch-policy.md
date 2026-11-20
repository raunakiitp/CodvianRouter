# T01 — G21 batch-policy gate — VERDICT: PASS

- Slice: G21 per-key model policy gate on POST /v1/batches (GAP-TRACK §9, G20 Blocker B1)
- Verdict: **PASS**
- Date: 2026-11-19 19:37 IST
- Tester: verify-only (T01); no code/doc edits made except this verdict file; no commits, no pushes.

## What was verified (commands run + results)

All commands run from `~/workspace/codvian-router/` with
`export PATH=$HOME/go/bin:$PATH`, `TMPDIR=$HOME/.tmp`, `-count=1` (cold, no cache).

1. `go test -count=1 -run 'TestG21' ./internal/server/` — **ok** (14.674s)
   Verbose re-run: all 4 pass:
   - `TestG21BatchRestrictedModelRefused` — PASS (3.82s)
   - `TestG21BatchForcedModelRefused` — PASS (2.94s)
   - `TestG21BatchAllowedModelAcceptedAndExecutes` — PASS (2.15s)
   - `TestG21BatchEndpointAllowlistUsesChatEndpoint` — PASS (3.89s)
2. `go test -count=1 -run 'TestBatches' ./internal/server/` — **ok** (10.504s);
   verbose count: **13 PASS, 0 FAIL** (all `TestBatches*` in
   `internal/server/batches_api_test.go:196-713`).
3. `go test -count=1 -v -run 'TestG20|TestG21|TestBatches' ./internal/server/` —
   **22 PASS, 0 FAIL** (5 G20 + 4 G21 + 13 Batches).
4. `go test -count=1 -run 'TestKeyTraffic|TestKeyBudget' ./internal/server/` —
   **ok** (58.021s). Named-test count in those two files is 23
   (8 `TestKeyTraffic*` + 15 `TestKeyBudget*`); combined with (3) that is
   45 tests green across the G20/G21/Batches/KeyTraffic/KeyBudget sets.
   Note on the builder's claimed "44/44": no single named filter in the
   task reproduces exactly 44, and my count of the same sets is 45 — the
   one-test difference is unaccounted for, but every test in every named
   set passed, so this is a counting ambiguity, not a failure.
5. Code read (no edits): `internal/server/batches_api.go`,
   `internal/server/g21_batches_model_policy_test.go`,
   `internal/server/batches.go` (worker + parser), `keytraffic.go`,
   `admin_keys_api.go` (checkKeyPolicy).

## Evidence (file:line)

- Gate runs before create: `internal/server/batches_api.go:233`
  `if refused := s.enforceBatchModelPolicy(w, r, fs, req.InputFileID); refused { return }`
  followed by `st.createBatch(...)` at `:236`. A refused create returns
  before any batch row exists — proven by the tests asserting 0 batch
  rows and 0 engine calls after refusal.
- Gate implementation: `internal/server/batches_api.go:306-366`
  (`enforceBatchModelPolicy`). Policy comment block at `:250-304`.
- Same parser as the worker: gate calls `validateBatchLines(rc)` at
  `batches_api.go:327`; the worker calls the same `validateBatchLines(rc)`
  at `internal/server/batches.go:755` and executes only validated lines.
  Parser requires `url "/v1/chat/completions"` and non-empty `body.model`
  (`batches.go:617` + checks at `:662-676` area — url/model required).
- Worker executes lines as chat: `batches.go:810`
  `req.Endpoint = provider.EndpointChatCompletions`; engine call at `:812`.
- Policy check is the shared helper, not a copy:
  `batches_api.go:358` `s.checkKeyPolicy(w, chatReq, key, model)` where
  `chatReq` is a clone of the request with URL path rewritten to
  `/v1/chat/completions` (`:350-356`). `checkKeyPolicy` defined at
  `internal/server/admin_keys_api.go:269`.
- Whole batch refused if ANY line names a disallowed model: loop over
  distinct models at `batches_api.go:357-360`, first refusal returns true.
  Proven by `TestG21BatchRestrictedModelRefused`
  (`g21_batches_model_policy_test.go:77`): file `model-a, model-b` with
  allowlist `[model-a]` → 403 `key_model_not_allowed`, 0 engine calls,
  0 batch rows.
- Forced model: proven by `TestG21BatchForcedModelRefused` (`:104`):
  `model-b` under `ForcedModel: model-a` → 403 `key_forced_model`, 0 rows;
  pinned `model-a` batch then completes (1/1, 1 engine call).
- Refusal consumes no slot: policy loop runs before any traffic
  admission (`batches_api.go:357-362`; `checkKeyTraffic` only at `:362`,
  after the loop). Proven by `TestG21BatchRestrictedModelRefused` with
  RPM=1: a chat with the listed model still returns 200 after the
  refused batch.
- Admitted create consumes exactly one slot: `checkKeyTraffic(w, chatReq, 0)`
  at `batches_api.go:362`. Proven by `TestG21BatchAllowedModelAcceptedAndExecutes`
  (`:135`) with RPM=1: after an admitted 2-line batch create, the next
  chat is 429; the batch still executes both lines (2/2, 2 engine calls).
- Endpoint axis uses the chat endpoint: proven by
  `TestG21BatchEndpointAllowlistUsesChatEndpoint` (`:177`): key allowed
  only on `/v1/chat/completions` may batch (200); key allowed only on
  `/v1/batches` is refused 403 `key_endpoint_not_allowed`, 0 rows.
- No-valid-lines falls through to worker `invalid_input_file`: gate
  returns false when `len(valid) == 0` (`batches_api.go:328-330`); the
  worker fails such a batch as `invalid_input_file` (`batches.go:762-775`).
  Covered by existing `TestBatchesZeroValidLinesFails`
  (`batches_api_test.go:445`), which passed in the `TestBatches` run.

## Failures / nits

- None. No failures, no nits found in the slice as landed.
- Transient observation (NOT a slice defect, recorded for the coordinator):
  at ~19:35 IST a `go test -run 'TestKeyTraffic|TestKeyBudget'` invocation
  failed to build with `internal/provider/provider.go:457:43: undefined:
  ModelListRefreshInterval`, while the symbol IS defined at
  `internal/provider/refresh.go:79`. A re-run minutes later passed
  (`ok`, 58.021s) and `go vet ./internal/provider/` is clean, so this
  was concurrent sibling churn mid-edit (or a stale build-cache race),
  not a G21 defect. It does show the tree is being edited live under
  testers — re-runs may flake the same way.

## Tree state at verification

- `internal/server/batches_api.go` — modified, uncommitted (M).
- `internal/server/g21_batches_model_policy_test.go` — new, untracked (??).
- HEAD `6831302`; no commits/pushes made by this tester.
