# Slice 3 — Dashboard live feeds ("Demo data" killed) — report

Date: 2026-11-17. Scope: CHECKLIST items 73 (backend) + 74 (dashboard).
Status: **built, gate green**. Nothing pushed to GitHub (per task: build only,
no GitHub connector, no browser).

## What was built

Five new unauthenticated (Tier 3 MANAGEMENT, auth-exempt, like the other
dashboard feeds) endpoints in `internal/server`, all backed by real router
state, all answering honest zeros/empty lists before any traffic:

| Endpoint | Source of truth |
|---|---|
| `GET /dashboard/overview?range=24h\|7d\|30d` | Ledger range summaries (current + previous equal window) + time buckets; recorder snapshot for since-start p50/p95, cache hits/misses, failovers |
| `GET /dashboard/models` | `provider.Registry.DashboardModels()` (live catalog: context window + per-token prices, 0 = unknown) joined with real per-model request counts from the ledger |
| `GET /dashboard/logs?limit=50&filter=all\|ok\|failed` | `ledger.RecentEvents` — one row per real attempt (a failover appears as its failed attempt + its successful retry; shared rows carry zero tokens/cost) |
| `GET /dashboard/usage?range=24h\|7d\|30d` (default 30d) | Ledger summary + per-provider/per-model breakdowns; failovers since start from the recorder; global budget cap from config |
| `GET /dashboard/keys` | `config.GatewayKeys` — id, name, scopes, budget, created; value is the fixed mask `crk_••••••••`, never the raw key (shown once at issue) and never its hash |

New read-side plumbing (no new dependencies, stdlib + existing SQLite):

- `internal/metrics`: `Recorder.Snapshot()` — totals by outcome, failovers,
  cache hits/misses, per-provider breakdowns, mean/p50/p95 read off the
  recorded histogram buckets (buckets are cumulative; the first
  implementation double-counted them and was fixed by the test).
- `internal/ledger`: `RecentEvents(limit, filter)`, `Summary(since, until)`
  with a real end bound (needed for the previous-window comparison),
  `Buckets(since, width)` — whole-window coverage, no future bucket, and
  the edge row (same-millisecond truncation at the `now` edge) is clamped
  into the last bucket instead of dropped.
- `internal/provider`: `Registry.DashboardModels()` + `DashboardModel`.
- `internal/server`: `WithLedger(*ledger.Ledger)`; wired in
  `cmd/codvian-router/main.go` (`server.WithLedger(st.led)`).
- `web/index.html`: every remaining demo section wired to its endpoint
  (load on open + 5 s refresh while open + Refresh button, same pattern as
  the existing live sections). KPI sparklines are redrawn from the live
  buckets; the traffic chart draws live buckets (requests, mean latency,
  errors); the token-mix donut shows prompt / completion / split-unknown;
  the live-routing feed shows the newest real ledger rows; Quick Start's
  base URL comes from `window.location.origin`; the code sample uses the
  placeholder `crk_YOUR_ROUTER_KEY`.

## Honest limitations (by design, stated in the UI, not hidden)

- **Spend is a lower bound.** A model with no configured price records $0
  (ledger rule); the Usage page says so. Cost is never guessed.
- **Latency p50/p95 are since server start**, not range-limited — the
  recorder keeps no timestamps. The KPI note says so.
- **"Cheapest route share" KPI removed.** The router does not record which
  route was cheapest per request, so it cannot be shown honestly.
- **Models table lost Throughput + Capabilities columns**, and the
  capability filter seg is gone: the registry reports neither.
- **Last-used is not tracked** for router keys; rows say "not tracked".
- **Per-key spend is unavailable**: the engine's ledger adapter records an
  empty `gateway_key_id`, so usage is global only.
- **Issuing/revoking keys from the dashboard is not wired** (no endpoint
  exists); the buttons now say so instead of inventing a key or faking a
  revocation. Same for the Quick-Start Reveal button (raw keys are shown
  once at issue and never stored).

## Files changed

- `internal/metrics/metrics.go` (+ `Snapshot`, `ProviderSnapshot`,
  `percentileFromBuckets`), `internal/metrics/snapshot_test.go` (new)
- `internal/ledger/ledger.go` (`UsageEvent`, `RecentEvents`, `UsageSummary`,
  `Summary`, `TimeBucket`, `Buckets`), `internal/ledger/feeds_test.go` (new)
- `internal/provider/dashboard.go` (`DashboardModel`, `DashboardModels`),
  `internal/provider/dashboard_models_test.go` (new)
- `internal/server/server.go` (5 handlers, `WithLedger`, `WithLedger` wiring,
  `dashboardModelLister`, previous-window token/cost fields),
  `internal/server/livefeeds_test.go` (new)
- `internal/server/guard.go` (5 new Tier 3 auth-exempt table entries; see
  "Concurrent slices" for the shared fix below)
- `internal/server/guard_test.go` (expected route list + MANAGEMENT tier
  count updated for the new routes — and for the concurrent slices' routes)
- `internal/server/server_test.go` (`TestDashboardDoesNotBypassAPIAuth`:
  GET on the POST-only `/v1/chat/completions` now answers 405 from the mux
  instead of 401 — method rejection, still no bypass; see below)
- `cmd/codvian-router/main.go` (`WithLedger(st.led)`)
- `web/index.html` (all demo sections wired live; see sample-value list)

## Gate evidence

- `go build ./...` — clean, exit 0.
- `go vet ./...` — clean, exit 0. (One transient vet-only "import cycle"
  warning appeared mid-run while a concurrent slice was editing server.go;
  it does not reproduce — see "Concurrent slices".)
- `gofmt -l .` — clean (no files listed).
- `go test -count=1 ./...` — **all 23 packages ok, 560 `--- PASS` events,
  0 fail** (verbose run; the baseline at slice start was 487 PASS events,
  but other slices landed tests concurrently, so the delta is not all this
  slice's). This slice added 6 new top-level tests, all passing:
  `TestSnapshotEmpty`, `TestSnapshotTotalsAndPercentiles`,
  `TestLedgerFeeds`, `TestRegistryDashboardModels`,
  `TestDashboardLiveFeeds`, `TestDashboardLiveFeedsEmpty`.
  One full-suite run showed a single transient `cmd/codvian-router` FAIL
  with no failing test name; the immediate re-run of that package and the
  full suite both passed — attributed to a concurrent slice mid-edit, not
  to this slice (no cmd files were touched by this slice except the
  one-line `WithLedger` wiring, which the passing re-runs cover).
- `node --check` on the dashboard JS (script extracted from
  `web/index.html`, 74,837 bytes) — clean.
- Static cross-check: all 140 static `#id` references in the dashboard JS
  resolve to ids present in the HTML; all `data-kpi` refs resolve.

## Live smoke (real binary, 2026-11-17)

Scripts kept at `/tmp/slice3-smoke/` (ephemeral, not in the repo):
`mock.py` (OpenAI-compatible mock: fixed reply, usage 12/9/21),
`smoke.sh`. Isolated HOME, fresh `init` (gateway key issued once),
mock on 21071 added as a Groq catalog provider with `--base-url`
override, router on 20199. **10/10 checks passed:**

- **S0 (empty state, before any traffic):** overview = 0 requests, $0,
  0 tokens, 24 one-hour buckets all zero; logs = `[]`; usage = 0
  requests/$0; models = live registry rows (≥1); keys = 1 key, masked
  `crk_••••••••`. Dashboard HTML contains no "Demo data", no `48,210`,
  no `cr_live_`, no `12.4M`.
- **S1 (one real request):** `POST /v1/chat/completions` with model
  `mock-model` returned the mock's `smoke reply`, usage 12/9/21; the
  mock log shows exactly 1 upstream call.
- **S2 (after the request):** overview = 1 request, 1 ok, 0 errors,
  21 total tokens (12 prompt / 9 completion), buckets sum to 1,
  p95 > 0; logs = 1 row (model `mock-model`, provider `prov_…`
  registry id, 21 tokens, ok); usage = 1 request / 21 tokens, 1
  provider row; models row for `mock-model` shows `requests: 1`;
  keys = 1 key. Keys feed contains neither the raw key nor its SHA-256.
- **Not verified here:** a rendered-browser visual check. Headless
  Chromium in this VM refuses to start (`Hatch WebGL domain policy is
  required for browser startup`), so no DOM render was possible from
  this environment; endpoints are proven live, JS is syntax-checked,
  and all id/kpi cross-references resolve. A visual pass in the real
  browser is still worth doing.

## Every sample value removed

Grepped `web/index.html` for `demo-tag|Demo data|cr_live_|48,210|42,890|
12.4M|128 models|reqPool|feedModels|Cache saved|cheaper than direct|
Tuesday, 17 November|9f2ka|51md02|11 Nov 2026|14 Nov 2026|2 min ago|
1 h ago|600 rpm` — **zero matches remain**. Accounted for, one by one:

- Overview header "Demo data" tag + hard-coded date "Tuesday, 17 November
  2026" → live date from JS + "Live · updated just now" status.
- KPI values 48,210 / 12.4M / 842 ms / $18.42 + pills ↑12.4% / ↑8.1% /
  ↓6.2% faster / "Cache saved $6.18" + notes "vs 42,890…", "9.1M prompt
  incl. 3.3M cached · 3.3M completion", "p50 212 ms · first token 148 ms",
  "25% cheaper than direct API use" → live KPIs from
  `/dashboard/overview` (real previous-window comparisons; "—" when no
  previous window exists). Sparkline paths (4 hard-coded polylines) →
  redrawn from live buckets.
- Traffic-chart `ranges` object (24h/7d/30d arrays of invented requests,
  latencies, errors, KPI sets) + hard-coded axis labels 3.0k/2.0k/1.0k,
  1200ms/800/400 and axis ticks 00:00…Now → live buckets; right axis is
  now honestly "mean latency", not p95.
- Token-mix donut 12.4M + legend 5.81M/3.29M/3.30M + "Cached" slice +
  "36%" cache rate + "$6.18 saving" note → live prompt/completion/
  split-unknown donut + real cache hit/miss counters; savings are not
  estimated (stated in the UI).
- Live-routing feed `feedModels` (7 invented model/provider pairs) +
  random latency `140 + Math.random()*900` every 5 s → newest real
  ledger rows, refreshed with Recent requests.
- Recent-requests `reqPool` (9 invented rows incl. a fake failover
  "OpenAI 429 → rerouted" and a fake 30 s timeout error) → live
  `/dashboard/logs` rows; "Requested model"/"Routed to" columns renamed
  to the honest "Model"/"Provider" (the ledger records the attempt's
  model/provider, not the requested model).
- Models `models` array (12 invented models with invented prices,
  throughputs, capabilities) + "128 models in catalog" + capability
  filter (Tools/JSON/Reasoning/Vision) → live `/dashboard/models`;
  Throughput/Capabilities columns removed (unreported by the registry).
- Logs filter "Failovers"/"Errors" (fake failover category) → "Failed
  attempts" (failed ledger rows); CSV export now exports the live rows.
- Usage KPIs $96.30 / "Cache saved $41.07" / "Across 6 providers ·
  Nov 1 – 17" / 57% "Cheapest route share" / ↑9.3% / 23 failovers /
  "0 user-visible errors" + `spend` array (6 invented provider amounts)
  + "November 2026" label → live `/dashboard/usage` (spend/tokens/
  failovers, spend-by-provider bars, spend-by-model table, range seg).
- Router keys "Default key / Codvian app (dev)" rows with invented
  `cr_live_…` values, "Created 11 Nov 2026 · Last used 2 min ago",
  "Rate limit 600 rpm" + Quick-Start `cr_live_9f2ka7d3x41d` (value, Copy
  button, Reveal button, code sample) → live `/dashboard/keys` (masked
  values, real names/scopes/budgets/created dates, "Last used: not
  tracked"); Quick-Start base URL from `location.origin`; code sample
  uses `crk_YOUR_ROUTER_KEY`.
- New-key button (invented a `cr_live_…` key client-side) and Revoke
  buttons (faked "Key revoked" + dimmed the row) → honest "not wired
  yet" toasts.
- `.demo-tag` CSS rule removed (no users left).

## Concurrent slices (what changed under this slice)

Three other slices landed in the same files while this slice ran; each
was re-read and re-applied around:

- **Guardrails slice:** added `GET + POST /dashboard/guardrails`
  (server.go, guard.go, dashboard section). This exposed a latent bug:
  `register()` called `mux.HandleFunc(path, h)` ignoring the method, so
  the second same-path registration panicked at startup (`pattern
  "/dashboard/guardrails" conflicts with itself`). Fix (shared, one
  line): `mux.HandleFunc(method+" "+path, h)`. Consequence: a wrong-
  method request now gets the mux's 405 before the handler's auth
  wrapper runs, so `TestDashboardDoesNotBypassAPIAuth` was updated —
  GET on the POST-only `/v1/chat/completions` asserts 405 (method
  rejection, still no bypass) while GET on `/v1/models` and `/metrics`
  still assert 401.
- **MCP slice:** added `GET /dashboard/mcp` (guard table + dashboard
  section).
- **Strategies slice:** added `GET + POST /dashboard/strategies` and an
  `internal/router` import in server.go (`router.RoutingInfo`,
  `router.ErrDeadlineExceeded`). At one point `go vet ./...` reported a
  test-only import cycle (`router_test` → `server` → `router`); it does
  not reproduce — `go vet` and the full suite are green, including
  `internal/router` (3.4 s) — so it was a mid-edit transient, not a
  standing break. Left untouched: the strategies slice owns that import.
- `guard_test.go` expected-route list + MANAGEMENT tier count were updated
  twice as routes landed: final counts **7 LOCAL_ONLY / 1
  ALWAYS_PROTECTED / 21 MANAGEMENT = 29 routes**.

## Out of scope / remaining (not this slice)

- Settings page still claims fake outcomes on its danger buttons ("All
  router keys revoked", "Configuration reset to defaults"), its switches
  toast "saved" without persisting, and its Port input defaults to 8080
  while the real default listen port is 20128. Flagged, not changed —
  Settings is not one of this slice's five sections.
- No visual browser check was possible from this environment (see smoke).

## CHECKLIST rows for items 73–74

| 73 | Overview / Models / Logs / Usage / Keys live feeds (backend): `GET /dashboard/overview` (ledger range KPIs + previous-window comparison + time buckets; recorder snapshot for since-start p50/p95, cache hits/misses, failovers), `GET /dashboard/models` (live registry catalog + per-model request counts), `GET /dashboard/logs` (recent ledger attempts, filters all/ok/failed), `GET /dashboard/usage` (spend/token roll-up, per-provider + per-model, failovers, budget cap), `GET /dashboard/keys` (config gateway keys, fixed mask only — never a raw key or hash); all Tier 3 MANAGEMENT auth-exempt; all answer honest zeros/empty lists with no traffic; spend is a lower bound (unknown-price models record $0); `metrics.Recorder.Snapshot()` (totals, per-provider, histogram p50/p95) and ledger `RecentEvents` / `Summary(since, until)` / `Buckets` back them; wired in main via `WithLedger` | ✅ | ✅ |
| 74 | Overview / Models / Logs / Usage / Keys live feeds (dashboard, same slice): Overview KPIs + traffic chart + token-mix donut + live-routing feed + Recent requests, Models catalog, Request Logs, Usage & Cost (spend by provider + by model), Router keys + Quick Start — all in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — wired to the real endpoints above, refreshed on open + every 5 s while open + Refresh button; every hard-coded sample value and every "Demo data" label removed (see the removal list in the slice report); honest empty states ("No requests yet…") before any traffic; Quick Start base URL from `location.origin`; key issue/revoke buttons say "not wired yet" instead of inventing keys; `node --check` on the dashboard JS clean; live smoke with the real binary proves one served request appears in every section | ✅ | ✅ |
