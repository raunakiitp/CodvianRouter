# Cross-instance coalescing — design note (2026-11-17)

Status: **design only — not implemented**. Single-instance coalescing
(`internal/coalescing`, items 63–64) is the shipped limit.

## The problem

Single-flight coalescing collapses N identical concurrent non-streaming
requests into one upstream call inside one process. With two or more router
instances behind a load balancer, identical requests landing on different
instances each make their own upstream call: the quota saving is lost and
waiters on different instances cannot share one flight.

## Design (if implemented)

- **Shared flight registry, not shared results.** Instances coordinate over a
  shared store (Redis, or the existing SQLite ledger DB in WAL mode with a
  `flights` table) holding: canonical request hash, leader instance id,
  leader heartbeat, result pointer (or error), expiry.
- **Leader election per hash:** first instance to claim the hash (atomic
  insert / SET NX with TTL) becomes leader and runs the retry loop; others
  subscribe and wait for the result, with a timeout that falls back to
  running the request themselves (fail-open: a coordination failure must
  never black out traffic).
- **Streaming stays excluded** (same rule as single-instance: waiters cannot
  share a half-delivered stream), as do tool-using turns.
- **Ledger honesty:** exactly one billed row (the leader's attempt); waiters
  on every instance record `shared=1` zero-token rows, as today.
- **Failure modes:** leader crash → heartbeat expiry → a waiter takes over
  (exactly one takeover, via the same atomic claim); store unreachable →
  degrade to single-instance coalescing only, never fail the request.

## Why not in this sprint

- It needs a shared store dependency (Redis or a shared DB) that a
  local-first single-binary install does not have; adding one changes the
  packaging story (item 75) and the "no external services" promise.
- The measured single-instance win (10→1 upstream calls, live smoke) already
  covers the common case: one router per machine, which is the documented
  deployment shape.
- Recorded honestly here as a known limitation, not papered over.
