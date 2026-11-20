# Slice report — items 137–138: warm HTTP/2 connection pools dashboard

Date: 2026-11-18. Worktree: `~/workspace/codvian-router/`. No GitHub writes made; nothing pushed.

## Row 138 (the spec for this slice)

> "Connection pools" nav item + section in the approved design system (dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground) — Little's Law sizing card (formula, λ, W, derived size, floor/ceiling, override state) + per-pool table (provider, key fingerprint, max idle, idle, total, requests, dials, reused, Warm/Cold) wired to the real endpoint `GET /dashboard/pools`, refreshed on open + every 5 s while open + Refresh button; honest empty states ("No providers connected yet", "Never dialed" rows with zero counters) and a "How it works" card stating the measured proof and the HTTP/2-multiplexing caveat; `node --check` on the dashboard JS clean; live smoke serves `#view-pools` + `loadPools` from the real binary.

## Verdict: nothing was missing — audit only, zero code changes

Every element of row 138 was already present and correct. I changed **no files and no lines**. The checklist rows were already marked Built ✅ / Verified ✅ by earlier work; this slice confirms that state rather than building it.

## What was already present (audited line by line against row 138)

### Dashboard HTML — `web/index.html`

- Nav item "Connection pools" — lines 537–539 (`data-view="pools"`).
- Section `#view-pools` — lines 1236–1280.
- Sizing card "Pool sizing — Little's Law" (lines 1250–1255): pill `#poolsSizePill` ("Derived: N idle" / "Override: N idle") plus `#poolsSizing` body rendered by JS with formula, λ (`expected_rps`), W (`expected_latency_ms`), derived size, floor (`min_conns`), ceiling (`max_conns`), and explicit-override state ("derivation not used").
- Per-pool table (lines 1257–1266): columns Provider, Key, Max idle, Idle, Total, Requests, Dials, Reused, Warm — exactly the row-138 column set.
- Refresh button `#poolsRefreshBtn` (lines 1243–1247).
- Honest empty states: `#poolsEmpty` — "No providers connected yet — add a provider and its warm pool will appear here once its first client is built. Nothing here is pre-filled." (line 1265); per-row "Never dialed" sub-line for `requests === 0` (JS line ~4047); `#poolsMeta` falls back to "No traffic yet".
- "How it works" card (lines 1269–1275): states the Little's Law formula, defaults (λ = 20 req/s, W = 1000 ms → N = 20), warm-up + sharing behaviour, the measured proof ("10 concurrent requests, 50 ms upstream, TLS mock: a pool of 2 re-dialled 8 times on the second burst; the sized pool re-dialled 0"), the HTTP/2-multiplexing caveat, the fingerprint note, and "A provider with no pool row has never been dialed — its counters are honest zeros."
- Design system: the section uses the shared `.card` (borderless, `border-radius: var(--radius)` = 26px, line 215/49), pure-black text `#000000` / pure-white surfaces `#FFFFFF`, dark-lime palette (`--green:#3F6212`, `--green-dot:#65A30D`). Zero occurrences of "Playground" in `web/index.html`.

### Dashboard JS — `web/index.html` lines 4012–4075

- `loadPools()` fetches the real `GET /dashboard/pools`, renders sizing + rows, hides/shows the empty state, and aggregates the meta line ("N pools · N re-dials avoided · N dials"). All values escaped via `escHtml`; numbers via `fmtNum`.
- Refresh on open: `go("pools")` calls `loadPools()` (lines 1907–1908).
- Refresh every 5 s while open: `setInterval(..., 5000)` guarded by `view-pools.classList.contains("on")` (lines 4071–4075).
- Refresh button wired: `poolsRefreshBtn.addEventListener("click", loadPools)` (lines 4067–4068).
- Fetch failure leaves the honest empty state / last values (`.catch` no-op).

### Server — `internal/server/server.go`, `internal/provider/pool.go`

- `GET /dashboard/pools` registered (line 795) and served by `handlePools` (lines 2377–2440): live rows from `poolStats.PoolStats()` joined with registry names, plus `poolSizing` (formula, inputs, derived size, floor/ceiling, override). Providers with no pool yet get an honest all-zero row ("never been dialed"); with no lister wired it answers an empty list with the default sizing — never invented pools.
- Wiring is live: `WithProviderRegistry` sets `s.poolStats = r` (lines 481–483), and `cmd/codvian-router/main.go:1393` passes the real registry. `WithPoolStats` exists separately for tests.
- Guard table: `{GET /dashboard/pools, TierManagement (= Tier 3), AuthExempt: true}` (`internal/server/guard.go:94`). OpenAPI is generated from the guard table, so the endpoint is in `GET /openapi.json` automatically — verified live (see below).
- Key fingerprint: `KeyFingerprint` (`internal/provider/pool.go:195–199`) returns the first 12 hex of SHA-256(key); the raw key is never stored in the pool map key or served. Counters are live from the counting dialer + wrapped conns (`reused = requests − dials`, `warmed` = ≥1 idle conn).

## Checks run (this slice)

- `go build ./...` — **PASS** (go1.27.4; only the pre-existing GOPATH/GOROOT warning). Go is at `~/go/bin/go`, not on the default PATH.
- `node --check` on the extracted inline dashboard JS (single `<script>` block, 175,119 chars) — **PASS**, clean.
- Live smoke (real binary `/tmp/codvian-router-smoke`, isolated `HOME=/tmp/smoke-home`, port 20199):
  - `GET /dashboard` — HTTP 200, 337,685 B; served HTML contains `view-pools` (2×), `loadPools` (4×), "Connection pools" (4×).
  - `GET /dashboard/pools` — HTTP 200, honest empty state:
    `{"pools":[],"sizing":{"formula":"N = ceil(expected_rps * expected_latency_ms / 1000)","expected_rps":20,"expected_latency_ms":1000,"derived_size":20,"min_conns":2,"max_conns":200,"per_host_override":0}}`
  - `GET /openapi.json` — contains `/dashboard/pools` with the guard-table summary.
  - Smoke server stopped afterwards; port 20199 confirmed free.
- `go test` was **not** run (per slice instructions — one full gate runs at the end).

## Notes / minor observations (not defects, no action taken)

- The "How it works" proof text states the dial-count proof (2 re-dialled 8 on burst 2; sized pool 0) but not the latency figures from row 137 (52.0 ms vs 69.1 ms). That matches the test's own comment (`TestWarmPoolAvoidsRedials`): latency is logged, not asserted — on localhost a TLS handshake is ~1 ms, so the dial count is the proof. Leaving the card as-is is the honest choice.
- Row 137's test used a 16-idle sized pool; the dashboard's default derived size is 20 (λ=20, W=1000 ms). Both are the same formula; the card correctly states the default derivation.
- One earlier smoke attempt raced the linker because `go build && ... &` backgrounded the whole chain; the rerun (build first, then start) succeeded. No code impact.

## Recommended CHECKLIST verdict

- **Item 137 (backend): READY** — feed, sizing, fingerprinting, guard table and OpenAPI all verified live; the proof tests are the gate's to re-run (`TestWarmPoolAvoidsRedials`, `TestWarmPoolSpeaksHTTP2`, `TestKeyFingerprint`).
- **Item 138 (dashboard): READY** — every clause of the row-138 spec is present in the shipped code and confirmed by the live smoke test. No changes were needed in this slice.
