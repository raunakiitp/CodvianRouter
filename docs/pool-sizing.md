# Warm HTTP/2 connection pools, sized by Little's Law

## The trap

Go's `http.DefaultTransport` keeps at most `MaxIdleConnsPerHost = 2`
idle connections per host. A router serving N concurrent requests to
one provider opens N connections for the first burst, then keeps only
2 of them — the next burst re-dials (TCP + TLS, 2+ RTTs on a real
network) for every request beyond those 2. This is research-log
entry 80's trap, and it bites hardest exactly when it hurts most:
on a failover, where the fresh connection is the whole point.

## The fix

One `http.Transport` per (provider, API key), created at startup
(not lazily on the first request) and warmed with a real request
(`TestKey` = `GET /v1/models`) at boot, so the first user request
never pays a cold dial. Every client built for the same pair —
engine clients, health probes, live-added providers — shares that
pair's transport, so connections stay warm across requests. The key
is never stored in the pool: pools are keyed by provider ID plus the
first 12 hex characters of SHA-256(key), and the dashboard serves
only that fingerprint.

## Sizing — Little's Law

The mean number of requests in the system is **L = λ × W**, where λ
is the mean arrival rate (requests/second) and W is the mean time a
request holds a connection (seconds). A pool holding fewer than L
idle connections re-dials under steady load; a pool holding L
(rounded up, clamped) does not:

```
N = ceil(λ × W)
```

Inputs (config `pools` block, all optional, 0 = default):

| Input | Default | Meaning |
|---|---|---|
| `expected_rps` (λ) | 20 req/s | mean request rate the pool is sized for |
| `expected_latency_ms` (W) | 1000 ms | mean time a request holds a connection |
| `max_idle_per_host` | 0 = derive | explicit size override, still clamped |
| `min_conns` | 2 | floor (never below Go's own default) |
| `max_conns` | 200 | ceiling (an idle connection costs a socket + TLS state) |

Defaults give **N = ceil(20 × 1.0) = 20 idle connections** per
provider+key. The formula, its inputs and the derived size are
recorded in code (`internal/provider/pool.go`), in the config
`SCHEMA.md`, on the dashboard (`GET /dashboard/pools`), and in the
startup log — never a magic constant without its derivation.

## Measured proof

`TestWarmPoolAvoidsRedials` (`internal/provider/pool_test.go`):
two bursts of 10 concurrent requests (50 ms upstream each) against
the same TLS mock (HTTPS + HTTP/1.1 — one connection per concurrent
request, so the idle pool is the only thing between burst 2 and a
fresh dial). Measured 2026-11-18 on this VM (Go 1.27.4):

| Pool | Burst-2 dials | Burst-2 mock accepts | Burst 2 wall time |
|---|---|---|---|
| Sized (16 idle) | **0** | **0** | 52.0 ms |
| Go default (2 idle) | **8** | **8** | 69.1 ms |

The dial count is the assertion; the mock's accept count is the
independent cross-check (a dial the transport counts must arrive as
an accept at the mock). The latency difference is small on
localhost (a TLS handshake is ~1 ms here) and is logged, not
asserted — on a real network each avoided dial saves 2+ RTTs.

`BenchmarkWarmPoolBurst`: 10-request burst over the warm sized
pool, 20 iterations — **51.2 ms/op, 0 dials/burst** (one 50 ms
upstream delay, all concurrent, no re-dial).

`TestWarmPoolSpeaksHTTP2`: the same transport negotiates HTTP/2
against an h2 mock and serves 5 sequential chats on **1 dial**
(`Requests 5, Reused 4`).

## Honest caveats

- **HTTP/2 multiplexing.** Under HTTP/2 with generous stream
  limits, a few connections carry any burst — the pool size cannot
  matter for concurrency there (measured: with unlimited
  multiplexing, a pool of 2 dialled 0 times on the second burst).
  The Little's Law size is the defence for HTTP/1.1 upstreams,
  stream-capped HTTP/2 upstreams, and server-side connection churn
  (GOAWAY / idle close): it sizes the *retained idle set*, which is
  what a burst finds waiting. The sizing is derived from *expected*
  load, not measured load — the router does not yet adapt the pool
  from live traffic. The counters (`requests`, `dials`, `reused`,
  `idle`/`total`) are the instrumentation such an adaptation would
  read.
- **Pool size is per (provider, key), not per host.** Two providers
  on the same host (e.g. two keys for one gateway) get separate
  pools, so the total idle connections to one host can exceed N.
  That is deliberate: different keys must never share a transport
  (a shared transport could send one key's request on a connection
  authenticated as another's — in practice Go sends auth per
  request, but the separation keeps the accounting honest).
- **Idle timeout is Go's default (90 s).** A pool warmed at boot
  stays warm across the health checker's 60 s probe cadence, so
  probes re-warm it before it can go cold. A provider with no
  traffic for > 90 s goes cold and re-dials once — the dashboard
  shows that as `warmed: false` until the next request.
- **Removing a provider drops its pool** (idle connections closed),
  but a config reset via the danger zone does not currently drop
  pools for providers removed that way — they linger until restart.
  (The danger-zone reset removes providers from the registry
  without going through `Registry.Remove`.)
