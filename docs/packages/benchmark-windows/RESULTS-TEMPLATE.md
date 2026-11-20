# Benchmark re-run — results (CHECKLIST 149 / 150 / 207 / 208)

Fill this in from the future run on Raunak's PC (`run-bench-windows.ps1`).
One run covers all four items. Do not fill in numbers from any other run.

## Machine (fill in from the run — do NOT assert anything here)

- Windows version / build:
- CPU model:
- GPU model: _(leave blank if unknown — the GPU is irrelevant to this CPU-bound bench; do not guess a model)_
- RAM:
- Disk / power state (plugged in? laptop on battery?):
- Run date/time (IST):
- Work dir on the PC:
- Binaries used: `codvian-bench.exe` / `codvian-router.exe`, HEAD `035d1ed`
  (or the rebuilt HEAD + new SHA-256s, if rebuilt — record them here)
- Limiter mode: on (default)
- Quiet check at start: CPU avg ___% over 5 s; `go.exe` running? yes / no
- Quiet check at end: CPU avg ___%
- Anything else running during the run (if anything: name it — the run may
  be polluted):

## Item 149 — backend after-run (hot-path fixes)

| Metric | Target | Pass 1 | Pass 2 | Pass / Fail |
|---|---|---|---|---|
| Added latency (router p50 − direct p50) | < 0.5 ms | ___ ms | ___ ms | |
| RPS (ok, 32 clients x 20 s) | > 3,000 | ___ | ___ | |
| RPS upstream (excl. cache hits) | — (report) | ___ | ___ | |
| RPS errors (HTTP 502) | — (report) | ___ / ___ (___%) | ___ / ___ (___%) | |
| Router p50 under load | — (report) | ___ ms | ___ ms | |
| Idle memory (RSS) | < 70 MB | ___ MiB (___ KiB) | — | |
| Peak RSS under load | — (report) | ___ MiB | ___ MiB | |
| Ready — /healthz | < 10 s | ___ ms | — | |
| Ready — /v1/models | < 10 s | ___ ms | — | |
| Direct-mock RPS (canary) | — (report; compare passes) | ___ | ___ | |

Notes (pollution? canary degraded on a pass? anything odd?):

## Item 150 — dashboard (Benchmark section serves the new run)

- `GET /dashboard/benchmark` HTTP status: ___
- Response file: `logs\dashboard-benchmark.json` — attached? yes / no
- `added_latency_met` / `rps_met` in the feed: ___ / ___
- `GET /dashboard` HTTP status: ___; Benchmark section present in page HTML? yes / no
- Notes:

## Item 207 — quiet re-run (backend)

- Was the window genuinely quiet (no other heavy work, CPU check above)? yes / no
- If NO: which pass(es) are polluted, and why (evidence, not a guess):
- Verdict on the re-run (clean / partially polluted / fully polluted):

## Item 208 — dashboard (quiet run's numbers live)

- Same checks as item 150, on this run's numbers: done? yes / no
- Notes:

## Raw files to attach (whole work dir, or at minimum these)

- [ ] `bench-results.json`
- [ ] `summary.txt`
- [ ] `logs\run-transcript.txt`
- [ ] `logs\dashboard-benchmark.json`
- [ ] `logs\dashboard.html`
- [ ] `lat-direct-1.json`, `lat-router-1.json`, `rps-direct-1.json`, `rps-router-1.json`
- [ ] `lat-direct-2.json`, `lat-router-2.json`, `rps-direct-2.json`, `rps-router-2.json`
- [ ] `peak-kb-1.txt`, `peak-kb-2.txt`
- [ ] `logs\router.txt`, `logs\mock-a.txt`, `logs\mock-b.txt`

## Previous measured numbers (for comparison — NOT this run's results)

Latest VM run, `docs/benchmark-2026-11-18-globalbudget.json`,
2026-11-18T07:52:12Z: added latency 0.851 / 0.777 ms (MISSED),
RPS 1,273 / 1,174 (MISSED), idle 42.1 MiB (MET),
ready 434.8 / 565.0 ms (MET).
