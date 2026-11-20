# Retry-After abandon rule + floor

CHECKLIST 167–168 (competitor sweep gap; Portkey: Retry-After ≥ 60s
ends retries for that target; CLIProxyAPI 10s floor, research-log
entry 146). Item 9 already honoured Retry-After — but a poisoned or
huge value could park a request indefinitely. These two rules bound
it.

## The rules

**Abandon — a Retry-After ≥ 60s ends retries for THAT target.**
The engine moves to the next target immediately, or fails honestly
if none remain. It never sleeps out the wait and then retries the
same target. The abandon is per-target, never per-request: healthy
targets are still tried in the same request.

**Floor — a present Retry-After below 10s is treated as 10s in the
retry path.** The same-target retry wait, the hard cooldown and the
soft-demotion cooldown all use the floored value. A poisoned or tiny
hint can never cause a retry storm.

## Present vs absent — the distinction that makes the floor safe

A Retry-After that is absent or unparsable is **not** floored: it
means "no hint", and the engine keeps exponential backoff for it
(`BackoffBase * 2^(n-1)`, capped at `BackoffMax`, jittered). The
distinction is carried end to end:

- `parseRetryAfter` (provider + wire) returns `(duration, present)`.
- `provider.RateLimitError.RetryAfterPresent` (and the same field on
  the four wire error structs) is true when a header was present and
  parsed — including an explicit `"0"`.
- `internal/router.rateLimitHint` / `classifyRetryAfter` decide from
  `(duration, present)`.

So an explicit `Retry-After: 0` floors to 10s, while a 429 with no
header backs off in milliseconds. Treating "no hint" as a 10s wait
would stall every header-less 429 for 10s — the opposite of the
router's fail-fast design (a 429 fails over immediately without
waiting out Retry-After).

## Numbers

| Hint (present) | Decision | Same-target retry wait | Cross-request cooldown |
|---|---|---|---|
| 0s / 1s / 5s / 9.999s | `floored` | 10s | 10s |
| 10s | `honoured` | 10s | 10s |
| 20s | `honoured` | 20s | 20s |
| 45s / 59s (cap 30s) | `capped` | 30s | 30s |
| 60s / 120s / 3600s | `abandoned` | — (never retried) | 30s (the cap) |
| absent | — (backoff) | exponential backoff | 5s soft base / backoff |

`RetryAfterCap` (default 30s) still bounds the wait for a honoured
hint, and it bounds the cross-request cooldown of an abandoned target
(a 120s hint cools the target for 30s — the router never honours
more than the cap of waiting). The cap never turns an abandon into a
retry: ≥ 60s abandons however small the cap is.

## Where it binds

- Non-streaming (`ChatWithChainStrategy`): the no-further-target
  branch breaks on abandon instead of sleeping and retrying; with
  further targets it already moves on immediately.
- Streaming (`runStream`, `noteStreamStartFailure`): the same-provider
  retry is gated on `retryWait > 0`, and the abandon yields a 0 wait,
  so it never fires for an abandoned target.
- Soft-demotion cooldown (`softCooldownDuration`): the base is the
  upstream's Retry-After floored at 10s (a present 7s hint cools for
  10s); an absent hint keeps the 5s base.
- Hard cooldown: set from the floored/capped wait, so an abandoned
  target still cools (at most `RetryAfterCap`) and the next request
  does not hammer it immediately.

## What it does not cover

- The health checker's `QuotaResetsAt` (`internal/health`) still
  records the raw Retry-After as the quota-reset time — that is probe
  bookkeeping, not a retry wait, and a 90s value there is the
  provider's own statement, not a sleep.
- The readiness gate's `Retry-After: 30` on 503s (item 143) is a
  fixed server-side value the router sends, not a hint it honours.
- The abandon applies to the in-request retry only. In soft-demote
  mode a target is still demoted (not skipped) across requests, per
  that mode's contract — the abandon stops the same-request retry,
  not the next request's one demoted attempt.

## Dashboard

The Live Routing trace (`GET /dashboard/trace`) carries the record
per rate-limited attempt — `retry_after: {seconds, decision,
wait_seconds}` — and the Live Routing card renders it, e.g.
`Retry-After 120s — target abandoned (≥60s), moved on` or
`Retry-After 2s — floored to 10s`. Attempts with no hint carry no
`retry_after` field at all (an absent hint is not a 0s hint). The
card also states the two rules in one line.

## Tests

- `TestClassifyRetryAfter` — the full decision table (0s/1s/5s
  floor, 10s/20s honour, 45s/59s cap, 60s/120s/3600s abandon,
  absent backs off, cap never rescues an abandon).
- `TestRateLimitWaitFloorAndAbandon` — the engine's wait function.
- `TestRetryAfterAbandonMovesToNextTarget` — 120s on target A,
  target B serves the same request, A hit once, fast.
- `TestRetryAfterFiftyNineSecondsRetriesSameTarget` — 59s retries
  (cap 100ms keeps the test fast); `TestRetryAfterSixtySecondsAbandonsSingleTarget`
  — 60s and 120s fail honestly with one hit, fast.
- `TestRetryAfterAbandonStreaming` — both streaming directions.
- `TestRetryAfterFloorSoftCooldown` — a present 2s hint cools soft
  for 10s. `TestChatRetryAfterHonoured` (pre-existing, converted) —
  a 30ms hint actually sleeps ~10s.
- `TestParseRetryAfter` — explicit `"0"` parses as present, absent
  as not present.
- `TestChatCompletionsRetryAfterAbandonTrace` (server) — a real
  `POST /v1/chat/completions` against httptest mocks with a real
  `Retry-After: 120` header, and the `retry_after` record visible
  from `GET /dashboard/trace`.
