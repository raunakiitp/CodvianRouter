# Routing decisions (A2 items 9a / 9b)

Durable decision records for two routing defaults that touch upstream load
and data egress. Each record states the decision, the reason, the load /
privacy implications, and the alternatives rejected, and points at the code
that implements it so the two never drift.

---

## D-A2-9a — Hedging stays ON at 150 ms for engine-computed chains; OFF for explicit order

**Decision.** Keep the default hedge — a second attempt launched after 150 ms
if the first has produced no bytes — ON for chains the ENGINE computes (flat
strategy-ordered chains: `cheapest`, `balanced`, `latency`, ...). Keep it OFF
for EXPLICIT-order chains (per-request `routing.order`, routing-group member
order, strategy-tree order).

**Why ON for engine-computed chains.** Those chains are a best-effort ranking
from measured signals (EWMA latency, success rate, price), not a stated
preference. A 150 ms wait is below a typical model's time-to-first-token, so
the hedge only fires when the first pick is genuinely slow; when it does, it
trades a bounded amount of extra in-flight load for a lower tail latency. That
is the only latency insurance an engine-computed chain has.

**Why OFF for explicit order.** When the operator or client states the order,
the first target is the answer, not a guess. Any delay is still a race, and the
first pick's expected latency is only an estimate — a first pick slower than
any fixed delay would still lose. Only disabling the hedge guarantees the
stated order holds. Implemented as `hedgeOff := explicitOrder || treeOrdered`
in `ChatWithChainStrategy`; the sequential failover on an actual failure is
unchanged.

**Load implications (explicit).** A hedge fires only when the first attempt has
not produced its first byte within 150 ms; when it fires it doubles in-flight
upstream load for that request. This is a deliberate, bounded trade, and it is
NOT changed silently: the default stays 150 ms, and the knob
(`Config.HedgeDelay`; a negative value disables hedging entirely) is unchanged.

**Alternatives rejected.**
- Flip the default OFF: removes the only latency insurance for engine-computed
  chains and silently worsens their tail latency. If a deployment wants that,
  it sets a negative `HedgeDelay` — an explicit choice, not a silent default.
- Longer default delay: still a race for explicit order, and a longer delay
  only reduces the insurance benefit for engine-computed chains.

**Recorded in code.** `internal/router/router.go` — `withDefaults`
(`HedgeDelay == 0` → 150 ms) and `ChatWithChainStrategy` (`hedgeOff`).

---

## D-A2-9b — Nadir stays fail-CLOSED; an external fail-open decisions API is refused

**Decision.** The `nadir` strategy never contacts an external decisions API
unless the operator explicitly configures one (`Config.NadirEndpoint`). With no
endpoint configured, or on any decision failure (timeout, non-2xx, malformed
response, a model outside the pool), it falls back to priority order with a
note, and no prompt leaves the machine.

**Why fail-CLOSED.** A fail-OPEN default — a hardcoded third-party endpoint
called even when unconfigured, with errors falling back to a remote decision —
would send the user's prompt to a party the operator never chose. That is a
data-egress and privacy violation and a hidden runtime dependency on a service
the operator cannot audit. Fail-closed keeps the request SERVED (only the
DECISION is refused) while guaranteeing no unconsented egress.

**What is deliberately refused.**
- Implementing a vendor "decisions" API as a built-in default (a hidden
  dependency on an external model-selection service).
- Shipping any default endpoint URL.
- Treating an unreachable endpoint as a reason to silently pick a model from a
  remote service.

**Behaviour when an endpoint IS configured.** One bounded POST per request
(default 2000 ms, max 30000 ms) carrying only the last user turn (capped) and
the pool's model ids — no system prompt, no history, no tools. A failed call
cools the endpoint down for 30 s so an unreachable endpoint costs one timeout,
not one per request. The selected model must be in the pool.

**Recorded in code.** `internal/router/nadir.go` (file header and `nadirSelect`).