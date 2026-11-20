# Slice 215–216 — Bedrock event-stream incremental delivery

## What changed

`internal/wire/bedrock.go` — `readEventStream` was a buffered reader: it
did `io.ReadAll` on the whole ConverseStream body and only then parsed
frames and called `fn`. No chunk reached the client until the last byte
arrived, and a never-ending Bedrock stream yielded nothing at all — the
honest limitation recorded by the 205–206 slice.

It is now a true streaming parser:

- One frame at a time with `io.ReadFull`: 12-byte prelude, then the
  frame body (headers + payload + message CRC). `fn` is called as soon
  as that frame's bytes have arrived.
- Only the frame in flight is buffered — one frame, not the whole body.
- The prelude CRC is verified **before** the frame body is allocated,
  so a corrupt prelude can never trigger a huge allocation.
- Frame semantics are unchanged: same events, same errors (truncated
  prelude / truncated message, prelude CRC mismatch, message CRC
  mismatch, missing `:event-type`, mid-stream exception mapped via
  `MapBedrockStreamError`), same CRC32 checks, same header parsing.
- A clean end at a frame boundary returns nil; a partial prelude is
  the same "truncated … (prelude)" error as before; a short frame body
  is the same "truncated … (message)" error as before.

`internal/provider/bedrock_client.go` — the `emit` comment updated: it
no longer claims the reader buffers the whole body. The cancel rule
itself is unchanged (`ctx.Err()` checked before delivering each chunk,
CHECKLIST 205).

`internal/provider/cancel_test.go` — `TestBedrockStreamCancelAbortsInflightRead`
updated to the new semantics. Its old `seen == 0` assertion *was* the
limitation: with incremental delivery, chunks do arrive before the
abort. It now asserts ≥ 1 chunk delivered, `context.Canceled`,
return < 2 s, and the mock stops writing after the abort.
`TestBedrockStreamCancelStopsBufferedFrames` is unchanged and still
passes.

No dashboard change: no user-visible status changes (transport
behaviour only). README.md + README.v2.md status lines updated with
honest numbers. CHECKLIST.md items 215–216 marked Built + Verified.

## Tests

New `internal/wire/bedrock_stream_test.go` (5 tests):

- `TestReadEventStreamIdenticalToBufferedReader` — 9 recorded-stream
  cases (text turn, tool-use + reasoning turn, empty stream, truncated
  prelude, truncated message, bad prelude CRC, bad message CRC,
  mid-stream exception, frame with no `:event-type`) run through both
  the new reader and a test-only copy of the old `ReadAll` reader
  (`readEventStreamBufferedReference`): byte-for-byte identical event
  sequences (type + payload) and identical error strings.
- `TestReadEventStreamIncrementalDelivery` — frames 400 ms apart; the
  first event must arrive ≥ 500 ms before the ~1.6 s stream ends.
- `TestReadEventStreamNeverEndingStreamDelivers` — writer sends two
  frames then blocks forever; both arrive while the read stays open.
- `TestReadEventStreamSplitAcrossTinyReads` — 1-byte reads still parse
  to the identical event sequence.
- `TestTranslateBedrockStreamIncremental` — first content chunk emitted
  ≥ 500 ms before the ~2.4 s stream ends; reassembled content "hello".

New `internal/provider/bedrock_incremental_test.go` (2 tests):

- `TestBedrockStreamIncrementalDelivery` — trickling mock (400 ms
  between frame groups) through the real client; first content chunk
  ≥ 500 ms before completion; content "first second"; usage total 20.
- `TestBedrockStreamNeverEndingDeliversChunks` — mock ticks forever;
  ≥ 1 content chunk arrives while the stream is open, then cancel
  returns `context.Canceled` in < 2 s.

Sensitivity: with the old reader restored (`git stash` of
`internal/wire/bedrock.go` only), all 3 timing tests FAIL and pass
again with the fix. The first sensitivity run exposed a bug in the
test's own trickle reader — it dropped frame bytes when the caller's
buffer was smaller than a frame (io.ReadAll's 512-byte starter) — fixed
by resuming partial chunks, then the sensitivity re-run failed cleanly
on the timing assertions, not on CRC.

## Gate (2026-11-18, Go 1.27.4)

- `go build ./...` — OK. `go vet` clean on wire/provider.
  `gofmt` clean on this slice's files (a `gofmt -l` run that included
  the Python smoke script reported it as unparseable Go — expected,
  the script is not Go; the Go files are clean).
- Full `go test -count=1 ./...` — EXIT 0, 39 packages ok (log
  `$HOME/.tmp/fulltest-215-216.log`).
- Verbose re-run — 1,574 top-level + 413 subtest PASS, 1 FAIL (log
  `$HOME/.tmp/fulltest-215-216-verbose.log`): `TestChatLedgerAndTokens`
  in `internal/router`, caused by a concurrent slice's uncommitted
  CHECKLIST-229 change to `reconcileUsage` in
  `internal/router/router.go` (it now stamps `LocalEstimatePrompt` on
  reported rows, breaking the test's verbatim-passthrough expectation).
  It fails 3/3 in isolation. Not this slice's code, not touched here —
  left for that slice's owner.
- `go test -race` clean on `internal/wire` + `internal/provider`.
- Bedrock cancel tests 10/10 under `-race`
  (`TestBedrockStreamCancelStopsBufferedFrames`,
  `TestBedrockStreamCancelAbortsInflightRead`).
- Live smoke 7/7 green (real binary + Python Bedrock mock,
  `scripts/smoke-bedrock-incremental.py`): provider added via catalog
  (`bedrock-converse`, `routing_live: true`); first SSE chunk at 0.0 s
  into a 0.686 s trickle stream; 3 content chunks received from the
  never-ending stream before cancel; non-streaming chat + healthz
  green afterwards.

## Honest limits

- Delivery granularity is one event-stream frame: a single huge frame
  still arrives whole — frames are the atomic unit of the format, and
  a frame cannot be delivered before its CRC is verified.
- The smoke mock uses the AWS docs example key pair (as the Go test
  mocks do), never a real credential; it ignores SigV4 signatures.
- The 1 failure in the verbose re-run belongs to a concurrent slice
  (see Gate); the tree was green for the first full run.

## What remains

- Nothing for this slice. The 205–206 limitation is closed.
- The concurrent slice's `TestChatLedgerAndTokens` failure (CHECKLIST
  229, `internal/router/router.go`) needs its owner to reconcile the
  new `LocalEstimatePrompt` stamping with the test's expectation.
- Files are uncommitted in the working tree (CHECKLIST.md and the
  READMEs are shared with concurrent slices, so this slice did not
  commit — the coordinator owns the per-slice local commit).
