# A4-7 decision record: proxy.set_aside and proxy.pool.exhausted — scoped out

**Status:** Scoped out (decision record, not implemented)

**Events:** `proxy.set_aside`, `proxy.pool.exhausted`

**Scope:** A4 item 7 asked for three webhook events that were missing
(hits = 0 on main today). This record covers the two *proxy*-family
events; `provider.reset_redeemed` is implemented separately with real
machinery.

## What was checked

- **No proxy-pool subsystem exists anywhere in this repo.** There is
  no `proxypool`, no proxy pool data structure, no proxy rotation, no
  per-proxy failure tracker, no set-aside/quarantine list, and no
  config key naming any outbound proxy. `grep -ri proxy internal/ cmd/`
  returns only unrelated hits: the SSRF-guard `internal/egress`
  package, provider *names* that contain "proxy" (e.g. the `xai.go`
  OAuth provider), comments, and test fixtures. None is a pool.
- **The router deliberately does NOT route through outbound proxies.**
  The egress SSRF layer (`internal/egress/egress.go`) disables
  environment proxies outright (`Proxy: nil` in the transport, with the
  comment "a proxy would resolve the host itself"), because an outbound
  proxy re-resolves the target and would bypass the validated-dial
  check. Both webhook delivery and every provider client build their
  HTTP transports on this egress guard — there is no concept of
  "route this request through proxy X" anywhere.
- **No upstream proxy configuration surface.** The config schema has no
  proxy list, no proxy host/port/credential, and no per-provider or
  global proxy field. A proxy pool has nothing to read from and no
  caller to hand a selected proxy to.

## Why the two events cannot exist as live paths today

Both events describe state transitions of a *proxy pool*:

- `proxy.set_aside` = "a proxy was set aside (quarantined) on repeated
  failure";
- `proxy.pool.exhausted` = "the pool has no usable proxy left".

A pool is only real if something consumes it — a caller asks the pool
for a usable proxy, the pool hands one out, the caller reports success
or failure, and repeated failures set the proxy aside until none remain.
This gateway has no such caller: it connects to each upstream directly
through the SSRF-guarded egress transport (which forbids proxies), so
there is no request path that could observe a proxy failing
repeatedly, set it aside, or exhaust the pool.

Building the pool as a standalone package with no caller would make the
two events *subscribable-but-never-emitted* in a running gateway: an
operator subscribing to `proxy.set_aside` would never receive it,
because nothing in the gateway ever drives a proxy through a failure.
That is exactly the "fake capability" the webhook event set forbids
(`internal/webhooks/webhooks.go` — "Every name here has a real emit
site in the gateway; do not add a name without one"). It would also be
an invented capability: the router would appear to manage a proxy pool
it does not have.

Adding a genuine proxy-pool subsystem (config surface, selection,
failure tracking, set-aside, exhaustion, and a caller that routes
requests through the selected proxy) is a real but large networking
feature that changes the SSRF posture of the whole gateway. That is out
of scope for the A4 webhook-event item, which is about wiring events
that already have underlying machinery. Bolt-on proxy support in an
event pass would be a half-subsystem: a pool that exists but is wired
to nothing.

## Decision

- **`proxy.set_aside` and `proxy.pool.exhausted` are scoped out.**
- They are **NOT** added to `webhooks.KnownEvents()` (and therefore not
  to the config validator or the registration API), because a known
  event with no real emit site is a fake capability.
- **Not** emitted from any no-op, and **not** covered by a mock emit
  test. A test that fires `proxy.set_aside` by hand without a pool
  behind it would be fake work.
- If and when a real proxy-pool subsystem lands (a caller that actually
  routes requests through a configurable set of outbound proxies, with
  real failure tracking), the two events should be implemented from its
  genuine set-aside and exhaustion transitions and added to
  `webhooks.KnownEvents()` at that point, following the same pattern as
  `provider.degraded` / `provider.recovered` / `provider.reset_redeemed`.

## What remains open

- The proxy-pool subsystem itself (out of A4 scope) — the prerequisite
  for both events.
- `provider.reset_redeemed` is implemented and subscribable in this
  item; see its emit tests (`TestProviderResetRedeemed*` in
  `internal/router/breaker_reset_redeemed_test.go`,
  `TestProviderResetRedeemedEventDeliversToLocalReceiver` in
  `internal/server/webhook_breaker_events_test.go`).