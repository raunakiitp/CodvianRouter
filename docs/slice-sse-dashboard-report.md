# Slice report — items 135–136: SSE delivery dashboard card

Date: 2026-11-18 (IST). Builder slice: items 135–136, "Streaming delivery (SSE)" card in the dashboard Benchmark section.
Worktree: `~/workspace/codvian-router/`. No GitHub writes made. No files outside the worktree touched.

## Row 136 spec (quoted from CHECKLIST.md)

> SSE delivery (dashboard, same slice): "Streaming delivery (SSE)" card in the Benchmark section fed by the new `GET /dashboard/streaming` endpoint (Tier 3 MANAGEMENT, auth-exempt, in the guard table — MANAGEMENT count 32→33; serves `streaming.json` from the data dir, `recorded=false` honest empty state, corrupt file → 500) — per-cadence table (median/p99, batching off vs on, tokens) + a note stating the batching bounds, the TCP_NODELAY verification, and that there is no latency target so the numbers are measurements, not a verdict; refreshed with the Benchmark view + Refresh button. `node --check` on the dashboard JS clean; "Playground" absent. Live smoke (real binary, 2026-11-17): feed 200 with the exact recorded numbers, dashboard serves the card + `loadStreaming`, guard feed lists the route, live 200-token stream through the binary with batching on delivers all tokens in order (median 8.648 ms, matching the recorded 8.536 ms)

## Verdict of this audit

**Item 136 was already fully present and correct. Nothing was missing, nothing was changed.**
**Item 135 (backend) was not re-audited in depth — it is out of this slice — but its artifacts the card depends on (`internal/server/sse_batch.go`, `docs/sse-delivery-2026-11-17.json`, `scripts/bench-sse.sh`) are present and the feed serves that run's exact numbers.**

## What was already present (audit, line by line against row 136)

| Row 136 requirement | Status | Evidence |
|---|---|---|
| "Streaming delivery (SSE)" card in the Benchmark section | ✅ present | `web/index.html:1746`, inside `<section id="view-benchmark">` (line 1702). Uses the approved design system (`.card`, no Playground). |
| Fed by `GET /dashboard/streaming` | ✅ present | `internal/server/server.go:809` registers it; `internal/server/streaming.go:132` `handleDashboardStreaming`; `loadStreaming()` in `web/index.html:3472` fetches it. |
| Tier 3 MANAGEMENT, auth-exempt, in the guard table | ✅ present | `internal/server/guard.go:109` — `Tier: TierManagement, AuthExempt: true`. Live guard feed (`GET /dashboard/guard`) lists it as tier MANAGEMENT, `auth_exempt: true`. |
| MANAGEMENT count 32→33 | ✅ historical; now 39 | The table has since grown with later slices: current counts are LOCAL_ONLY 10 / ALWAYS_PROTECTED 10 / MANAGEMENT 39 (56 routes total). The streaming route is counted in the table and in `guard_test.go`'s expected list (56 = 56). See the open finding below about the stale count pin. |
| Serves `streaming.json` from the data dir | ✅ present | `cmd/codvian-router/main.go:1411` — `server.WithStreamingFile(filepath.Join(st.cfg.DataDir, "streaming.json"))`. |
| `recorded=false` honest empty state | ✅ present + live-verified | `internal/server/streaming.go:138-145`. Live: with the file absent, feed answers 200 `{"recorded":false,"note":"No streaming run recorded yet — run scripts/bench-sse.sh …"}`. Dashboard shows `#sseEmpty` ("No streaming run recorded yet — run scripts/bench-sse.sh…") and clears the table. |
| Corrupt file → 500 | ✅ present + live-verified | `internal/server/streaming.go:160-164`. Live: file containing `{not json` → 500 `{"error":{"message":"streaming file is corrupt — re-run scripts/bench-sse.sh",…}}`. Unit test `TestDashboardStreamingCorruptFile` covers the same. |
| Per-cadence table (median/p99, batching off vs on, tokens) | ✅ present | Card table headers (`web/index.html:1750`): Mock cadence / Tokens / Median · batching off / Median · batching on / p99 · off / p99 · on; rows built by `loadStreaming()` (`web/index.html:3488-3500`). |
| Note: batching bounds | ✅ present | `#sseNote` text (`web/index.html:3503-3506`): "one Flush per 3 tokens or 8 ms, whichever comes first; [DONE] and error events always flush immediately." Bounds come from the file (`batching.max_tokens` / `max_delay_ms`), defaulting to 3 / 8. |
| Note: TCP_NODELAY verification | ✅ present | Same note: "TCP_NODELAY verified set on inbound + provider connections" when `tcp_nodelay.verified` is true; "not verified" otherwise. |
| Note: no latency target, measurements not a verdict | ✅ present | Same note: "No latency target exists for token delivery — these are measured values, not a pass/fail verdict." plus the file's own `note` appended. No MET/MISSED scoring anywhere on the card. |
| Refreshed with the Benchmark view + Refresh button | ✅ present | `web/index.html:1928` — `if(view === "benchmark"){ loadBenchmark(); loadStreaming(); }`; `web/index.html:3512` — `$("#benchRefreshBtn").addEventListener("click", loadStreaming)` (button at line 1709). |
| `node --check` on the dashboard JS clean | ✅ verified this slice | Extracted the single inline `<script>` block from `web/index.html` to `/tmp/dash.js`; `node --check` passes. |
| "Playground" absent | ✅ verified | `grep -c Playground web/index.html` → 0. |
| Feed serves the exact numbers in `docs/sse-delivery-2026-11-17.json` | ✅ verified live | Built binary to `/tmp`, isolated HOME, copied the docs file to `<data-dir>/streaming.json`. Live `GET /dashboard/streaming` returned 200 with the exact recorded numbers: 10 ms cadence off median 0.357 / p99 25.548, on median 8.536 / p99 9.943; 1 ms cadence off 0.211 / 3.16, on 1.302 / 2.853; 200 tokens each; `batching {3, 8}`, `tcp_nodelay.verified: true`. `internal/server/testdata/streaming-2026-11-17.json` is content-identical to the docs file (diff of canonical JSON: no differences), and `TestDashboardStreamingRealRun` pins those exact values. No number on the card is invented — every value traces to the file. |

## What was missing

Nothing. No code was changed in this slice: no edits to `web/index.html`, `internal/server/server.go`, `internal/server/guard.go`, or any other shared file; no new files except this report. The one cosmetic quirk noticed (the `recorded=false` response still carries `"batching":{"max_tokens":0,"max_delay_ms":0},"tcp_nodelay":{"verified":false}` because those are non-pointer structs without `omitempty` effect) is harmless — the dashboard keys off `recorded` — and was deliberately left alone per "do not rewrite working code for style".

## Checks run this slice

- `go build ./...` — **PASS** (go1.27.4; Go found at `~/go/bin/go`, not on default PATH in this VM).
- `node --check` on the extracted inline dashboard JS (`/tmp/dash.js`) — **PASS**.
- Live smoke (real binary `/tmp/codvian-router-smoke`, isolated `HOME=/tmp/sse-smoke-home`, port 18741):
  - `GET /dashboard/streaming` with the recorded run in place → **200**, exact numbers from `docs/sse-delivery-2026-11-17.json` (see table above).
  - `GET /dashboard` → **200**, serves the "Streaming delivery (SSE)" card (1 occurrence) and `loadStreaming` (3 occurrences).
  - `GET /dashboard/guard` → **200**, lists `GET /dashboard/streaming`, tier MANAGEMENT, auth-exempt.
  - File absent → **200**, `recorded:false` honest empty state.
  - File corrupt (`{not json`) → **500** with the re-run message.
  - File restored → **200**, `recorded:true` again.
- `go test` was **not** run (per slice instructions — one full gate runs at the end).

## Open finding (NOT this slice — for the coordinator)

`internal/server/guard_test.go:112` pins tier counts at LOCAL_ONLY 9 / ALWAYS_PROTECTED 9 / MANAGEMENT 38, but the current guard table has **10 / 10 / 39** (56 routes; the test's expected route list already has 56 entries and already includes `POST /admin/db/wipe` and `POST /admin/db/import`, so the table and the expected list agree — only the count pin is one behind in each tier). This looks like a concurrent builder's slice (DB wipe/import) that added routes without bumping the pin, or an in-flight edit. It is outside items 135–136 and was left untouched; the end-of-sprint gate (`go test ./...`) will surface it, and whichever slice added the newest route in each tier should bump the pin to 10/10/39.

## Recommended CHECKLIST verdict

- **Item 135: Built ✅ (already), Verified — leave to the coordinator/gate** (backend not re-audited here; its feed-facing behavior verified live through this slice's smoke).
- **Item 136: Built ✅, Verified ✅** — every clause of the row is present in code and was re-verified live in this slice (feed 200 with exact recorded numbers, card + `loadStreaming` served, guard feed lists the route, empty state and corrupt-file 500 both live-verified, `node --check` clean, Playground absent). No changes were needed.
