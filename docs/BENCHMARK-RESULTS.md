# Codvian Router — Benchmark Results (BENCH-KEEPER)

**Keeper:** BENCH-KEEPER (Benchmark Department). **This file is the single consolidated record.**
Other runners write their own files under `docs/bench-results/` — this file only reads them and consolidates.
**Rule:** never fake a number; never promote a polluted run to verified; every row cites its source file.

**Status (2026-11-19 16:05 IST, file created):**
`latency 0.851/0.777 ms (MISSED), RPS 1,273/1,174 (MISSED), idle 42.1 MiB (MET), ready 434.8/565.0 ms (MET) — 0 fresh runs pending`
(`docs/bench-results/` exists but is empty as of 16:13 IST; no runner result has landed.
VM loadavg at last check 24.92 — storm conditions, no clean run possible right now.)

**Keeper note on idle:** the brief said "idle ~38 MiB". The paired verdict run that produced
0.851/0.777 ms records idle **42.1 MiB** (`docs/benchmark-2026-11-18-globalbudget.json`:
`idle_rss_kib` 43,064). The 38.4 MiB figure is the earlier attempt-3c pass-2 idle. Both are MET
(< 70 MB); the latest-verified row below uses the same run's own 42.1 MiB so the row is
internally consistent. Nothing is hidden: both figures are in the history.

**Reconciliation cross-check (2026-11-19, `docs/BENCHMARK-RECONCILIATION.md`):** the baseline
above is confirmed — `benchmark-2026-11-18-globalbudget.json` (#37/#38) is VALID, `clean: true`,
latest verified. `docs/benchmark-2026-11-18.json` verdict is **VALID — KEEP, do not delete**;
its headline RPS overstates real upstream throughput by ~22–24% (cache hits) — real upstream
**1,164 / 1,076 per s**, not 1,500 / 1,425. Quote upstream, not headline, when comparing
throughput. (Reconciliation tally: 18 VALID · 6 MIXED · 14 POLLUTED; no file was found to be
unusable.)

## (a) Targets

Derived from Geekflare's independent benchmark (research-log entry 148; see `docs/BENCHMARK.md`).
Rig: `./scripts/bench-geekflare.sh` + `cmd/codvian-bench` driver; added latency = router p50 −
direct-mock p50 over 1,000 sequential requests; RPS = 32 clients × 20 s through the router.
Machine: this shared VM (1.5-shared-vCPU budget), NOT a dedicated 2-vCPU droplet — RPS/latency
numbers are measurements on this machine under load, not clean-room results.

| Target | Threshold | Source |
|---|---|---|
| Added latency (median) | < 0.5 ms | Geekflare-derived, `docs/BENCHMARK.md` |
| RPS (32 clients × 20 s) | > 3,000 | Geekflare-derived, `docs/BENCHMARK.md` |
| Idle memory (RSS) | < 70 MB | Geekflare-derived, `docs/BENCHMARK.md` |
| Ready time (to first `/healthz` 200) | < 10 s | Geekflare-derived, `docs/BENCHMARK.md` |

## (b) Latest VERIFIED results (pre-midnight baseline — 2026-11-18)

Latest clean verdict run: **global-budget hot-path fix run, 2026-11-18 07:50:39–07:52:12 UTC**
(HEAD `db5c626`). Clean by the canary rule: direct-mock canaries 19,599 / 20,009 RPS, no
`go test` / `go build` running, load 1.30 → 1.48.

| Target | Latest verified | Date (run) | Loadavg at run time | Verdict | Command | Source result file |
|---|---|---|---|---|---|---|
| Added latency (median) | 0.851 ms pass 1 / 0.777 ms pass 2 | 2026-11-18 07:50–07:52 UTC | 1.30 → 1.48 | MISSED (~1.7x) | `./scripts/bench-geekflare.sh` | `docs/benchmark-2026-11-18-globalbudget.json` (raw: `docs/benchmark-2026-11-18-globalbudget-run.json`; narrative: `docs/BENCHMARK.md` § Global-budget 299–300) |
| RPS (32 × 20 s) | 1,273 pass 1 (ok 25,494; 4,432/29,926 = 14.8% HTTP 502) / 1,174 pass 2 (ok 23,498; 5,019/28,517 = 17.6% HTTP 502) | 2026-11-18 07:50–07:52 UTC | 1.30 → 1.48 | MISSED (~2.4x) | `./scripts/bench-geekflare.sh` | `docs/benchmark-2026-11-18-globalbudget.json` |
| Idle memory (RSS) | 42.1 MiB (43,064 KiB) | 2026-11-18 07:50–07:52 UTC | 1.30 → 1.48 | MET | `./scripts/bench-geekflare.sh` | `docs/benchmark-2026-11-18-globalbudget.json` |
| Ready time | 434.8 ms `/healthz` / 565.0 ms `/v1/models` | 2026-11-18 07:50–07:52 UTC | 1.30 → 1.48 | MET (by ~18x) | `./scripts/bench-geekflare.sh` | `docs/benchmark-2026-11-18-globalbudget.json` |

Peak RSS under load in this run: 78.2 / 108.8 MiB (not a target; recorded for the record).

## (c) Run history (append-only, newest first)

Clean runs only. Every number traces to `docs/BENCHMARK.md` and the cited JSON.
"Supersedes" = a newer clean run on a newer tree replaces the latest-verified row above.

### 2026-11-18 07:50–07:52 UTC — global-budget memo verdict run (CHECKLIST 299–300) — LATEST VERIFIED
- Added latency **0.851 / 0.777 ms**; RPS **1,273 / 1,174**; idle 42.1 MiB; ready 434.8/565.0 ms;
  peak RSS 78.2/108.8 MiB. Load 1.30 → 1.48, canaries 19,599/20,009 RPS. HEAD `db5c626`.
  Targets still MISSED. Source: `docs/benchmark-2026-11-18-globalbudget.json`.

### 2026-11-18 07:09–07:11 UTC — key-budget memo verdict run, attempt 3 (CHECKLIST 279–280)
- Added latency **0.842 / 0.691 ms**; RPS **1,451 / 1,432** (errors 4,752/33,786 pass 1,
  4,394/33,113 pass 2); idle 41.8 MiB; ready 441/559 ms. Load 1.05 → 0.91, canaries
  21,732/21,805 RPS. HEAD `ec029d5`. Targets still MISSED.
  Source: `docs/benchmark-2026-11-18-keybudget.json` (raw `docs/benchmark-2026-11-18-keybudget-run.json`).

### 2026-11-18 07:00:15 UTC — ledger multi-row INSERT after-run (CHECKLIST 287–288)
- Added latency **0.680 / 0.624 ms**; RPS **1,393 / 1,086**; idle 41.3 MiB (42,336 KiB);
  ready 376.0/484.0 ms. Load 2.27 → 3.57, canaries 19,396/20,567 RPS. Targets still MISSED.
  (Before-run pass 1 ran under rising load; no clean attribution of the delta is claimed.)
  Source: `docs/benchmark-2026-11-18-ledger.json` (same content as `docs/benchmark-2026-11-18-ledger-after.json`).

### 2026-11-18 06:55:49 UTC — ledger multi-row INSERT before-run (CHECKLIST 287–288)
- Added latency **1.232 / 0.924 ms**; RPS **1,238 / 1,220**; idle 41.6 MiB (42,616 KiB);
  ready 771.1/1,007.5 ms. Load 1.07 → 4.72, canaries 12,719/19,215 RPS.
  Pass 1 ran under rising load (direct-latency p50 0.100 ms vs 0.060 ms typical).
  Reconciliation #30: VALID with caveat — p1 canary 12,719 only just above the rule;
  p1 latency (1.232 ms) is likely load-inflated.
  Source: `docs/benchmark-2026-11-18-ledger-before.json`.

### 2026-11-18 ~06:13 UTC — definitive re-measurement, attempt 4 (CHECKLIST 273–274)
- Added latency **0.908 / 0.711 ms**; RPS **1,213 / 963** (15.7% / 19.9% HTTP 502);
  idle 41.5 MiB; ready 464.2/642.9 ms; peak RSS 73.5/94.2 MiB. Load 1.45 → 4.84
  (a fleet gate started mid-run); canaries 15,205/16,756 RPS — clean by the canary rule,
  but pass 1 is the noisier pass (RPS passes −20.6% apart). Targets still MISSED.
  Source: `docs/benchmark-2026-11-18-cpu2.json`.

### 2026-11-18 ~03:55–04:01 UTC — hot-path CPU profile after-run (CHECKLIST 249–250)
- Added latency **0.697 / 0.570 ms**; RPS **1,650 / 650**. No RPS verdict is drawn:
  passes disagree (+138.5% / −14.9%) while the load trace shows opposite regimes
  (before: rising 1.7 → 9; after: falling 8.5 → 3.7) — the RPS delta is load, not fix.
  Before-run on the same pair: 0.708/0.814 ms, 692/764 RPS. Canaries 15,171/17,906
  (after) vs 16,218/15,560 (before). Idle/ready not recorded for this run in
  `docs/BENCHMARK.md`. Targets still MISSED.
  Source: `docs/benchmark-2026-11-18-cpu.json`.

### 2026-11-18 ~01:45 UTC — quiet re-run attempt 3c, pass 2 CLEAN (CHECKLIST 225–226)
- Added latency **0.855 ms** (pass 2 only); RPS **1,079** (upstream 1,078/s; only 10 cache
  hits); idle 38.4 MiB; ready 125.6 ms `/healthz` / 249.3 ms `/v1/models`; peak RSS
  98.8 MiB. Launched at loadavg 1.31, zero go processes; direct-mock canary 16,287 RPS
  (best of the session). Pass 1 of the same run was POLLUTED — see section (d).
  Targets still MISSED. Note: the tree moved enormously between the before-run and this
  run (binary 19 MB → 27 MB, ~30 slices), so this is the current tree's measurement,
  not a clean before/after for the 149–150 fixes. Reconciliation #12: MIXED — p1 polluted,
  p2 VALID (canary 16,287, best of the session); p2 is the citable number.
  Source: `docs/benchmark-2026-11-18-attempt3.json`.

### 2026-11-18 ~02:05 IST (file: "2026-11-18 ~20:35–20:37 UTC") — benchmark-slice before-run (CHECKLIST 149–150)
- Added latency **0.578 / 0.769 ms**; RPS (ok) **1,178 / 1,470** (upstream 745/988 per s);
  48.5% / 48.3% HTTP 502; idle 37.7 MiB; ready 242.7 ms. Loadavg 1.76 at start.
  This is a PRE-FIX baseline for the 149–150 slice, not a verdict run — it does not
  supersede the latest-verified row. Date note: as written ("20:35 UTC") it would postdate
  the 01:45 UTC after-run; the keeper reads it as Nov 17 20:35 UTC = Nov 18 02:05 IST,
  i.e. the baseline taken before the attempt-3 after-run.
  Source: `docs/benchmark-2026-11-18-before.json`.

### 2026-11-17 19:46 UTC (2026-11-18 ~01:16 IST) — re-run, no code change since the 00:02 run
- Added latency **0.541 / 0.509 ms**; RPS **1,483 / 1,426** (upstream 1,063 / 959 per s);
  idle 37.5 MiB;
  ready 181.2 ms `/healthz` / 326.3 ms `/v1/models`; peak RSS 37.5/37.6 MiB.
  Loadavg 2.16 at run start (10.58 at end — the bench's own 32-client load).
  The 0.541 ms pass-1 improvement vs the 00:02 run is VM quietness, not a fix.
  Targets still MISSED (by 0.041 ms on the clean pass).
  Reconciliation #3: VALID — corroborates the 00:02 run.
  Source: `docs/benchmark-2026-11-18-rerun.json`.

### 2026-11-17 18:32 UTC (2026-11-18 ~00:02 IST) — post-hot-path-fix run
- Added latency **0.677 / 0.511 ms**; RPS **1,500 / 1,425** (48.1% / 48.3% HTTP 502);
  idle 38,328 KiB (~37.4 MiB); ready 158.5/309.9 ms; peak RSS 37/38 MiB.
  Loadavg 7.05 at run end (bench's own load included); start load not recorded in this file. Reconciled 2026-11-19 (CHECKLIST G2): JSON re-read field by
  field against `docs/BENCHMARK.md`, matches exactly. Targets still MISSED.
  **Reconciliation verdict (`docs/BENCHMARK-RECONCILIATION.md` #2): VALID — KEEP, do not delete.
  Caveat: headline RPS overstates real upstream throughput by ~22–24% (cache hits) — real
  upstream 1,164 / 1,076 per s. Quote upstream, not headline, when comparing throughput.**
  Source: `docs/benchmark-2026-11-18.json`.

### 2026-11-18 (round 2) — token-counting regression baseline (VALID record of the regression)
- Baseline (phase 1, item 145 regression): added latency **73.638 / 73.862 ms**,
  RPS **41 / 51** (upstream 12 / 13 per s), peak RSS 349–363 MiB. Cause proven by pprof:
  `tiktoken-go.NewCoreBPE` rebuilt on every upstream-served request (86.9% of CPU).
  Reconciliation #7: VALID — a valid record *of the regression*; the terrible numbers are
  the router's (profile-proven), not the VM's.
  Source: `docs/benchmark-2026-11-18-round2-baseline2.json`.
- Run A (encoder-cache fix only, `docs/benchmark-2026-11-18-round2-phase2.json`) is NOT in
  clean history: reconciliation #8 rates it **POLLUTED** — both direct-mock canaries
  (10,338 / 11,919 RPS) fail the 12,000 rule. Its numbers (1.466/1.476 ms, 261/261 RPS)
  are directional only (73.6 ms → ~1.47 ms is far larger than VM noise and the profile
  proves the mechanism). See section (d).

### 2026-11-17 ~23:25 IST — primary run (pre-fix)
- Added latency **1.632 / 1.727 ms**; RPS **475 / 558** (21.8% / 20.4% HTTP 502);
  idle 37 MiB (37,968 KiB); ready 186.5/298.5 ms; peak RSS 37 MiB.
  Loadavg 2.19 at start, 8.45 by end. Targets MISSED (~3.4x latency, ~6x RPS).
  Source: `docs/benchmark-2026-11-17.json`.

## (d) POLLUTED / DISCARDED — must NOT be quoted as results

Anything here is kept for the record only. A polluted run never promotes to section (b)
or to the "latest verified" row, even if one of its passes looks clean — unless
`docs/BENCHMARK.md` explicitly says a specific pass is the verdict (none below does,
except the E1 pass-2 pair, which is evidence for the limiter verdict only, not a
benchmark verdict run).

| File / run | Numbers (for the record only) | Why polluted |
|---|---|---|
| `docs/benchmark-2026-11-18-after-polluted.json` (207–208 attempt, 2026-11-18 ~00:02 UTC) | 1.059/1.112 ms, 988/899 RPS | Load 1.30 → 7.32; concurrent slice's fsync-heavy `go test` started mid-run |
| `docs/benchmark-2026-11-18-attempt3-polluted.json` (3a, 01:37:45 UTC) | 0.968/0.839 ms, 1,001/1,018 RPS | Load 1.32 → 8.17; 227–228 full `go test` + `go vet` overlapped whole run; ready 814.5/1,207.9 ms proves contention |
| `docs/benchmark-2026-11-18-attempt3b-polluted.json` (3b, 01:40:45 UTC) | 0.694/0.888 ms, 869/1,182 RPS | Direct-mock canary degraded in BOTH passes (9,007/10,670 RPS); contended throughout |
| `docs/benchmark-2026-11-18-attempt3.json` pass 1 (3c pass 1) | 0.915 ms, 682 RPS, 34.9% 502 | Direct-mock canary 8,149 RPS; second 229–230 verbose run overlapped it (pass 2 of this file IS the clean verdict — see history) |
| `docs/benchmark-2026-11-18-round2-phase2.json` (round-2 run A, encoder-cache fix only) | 1.466/1.476 ms, 261/261 RPS (for the record only) | Reconciliation #8: POLLUTED — both direct-mock canaries (10,338/11,919 RPS) fail the 12,000 rule; directional only, not a clean run |
| `docs/benchmark-2026-11-18-round2-phase2b.json` (round-2 run B) | 1.013/1.123 ms, 307/344 RPS | VM storm mid-run (another slice's full `go test ./...`); load ended 19.35; canary 5,422/4,628 vs 10,338/11,919 in run A |
| `docs/benchmark-2026-11-18-e1-on-polluted.json`, `docs/benchmark-2026-11-18-e1-off-polluted.json`, merged `docs/benchmark-2026-11-18-e1-polluted.json` | On pass 2: 0.680 ms, 1,324 RPS; off pass 2: 0.781 ms, 1,419 RPS (pass 1s polluted) | Merged file records `clean: false`. Verdict rests on the pass-2 pair ONLY: limiter is NOT the RPS ceiling (+7.2% off vs on, inside ±10% noise). Not a benchmark verdict run |
| `docs/benchmark-2026-11-18-cpu2-attempt1-polluted.json` (05:45:25 UTC) | 0.816/0.782 ms, 1,189/247 RPS | Fleet `go build ./...` + catalog audit mid-run; load 1.81 → 11.58; pass-2 p50 47.9 ms |
| `docs/benchmark-2026-11-18-cpu2-attempt2-polluted.json` (05:56:16 UTC) | 0.889/1.565 ms, 329/666 RPS | VM rebooted ~05:54 UTC; post-reboot fleet restart storm; load 2.42 → 3.90; pass-2 canary 6,180 fails the 12,000 rule |
| `docs/benchmark-2026-11-18-cpu2-attempt3-polluted.json` (06:05:21 UTC) | 1.976/1.287 ms, 598/300 RPS | Fleet gate started seconds after quiet launch; load 1.32 → 8.53; pass-1 canary 8,761 fails the 12,000 rule |
| `docs/benchmark-2026-11-18-keybudget-attempt1-polluted.json` (06:53 UTC) | 0.859/0.923 ms, 1,254/480 RPS | Pass-1 canary 11,963 marginally below the 12,000 rule; pass-2 RPS collapsed −62% |
| `docs/benchmark-2026-11-18-keybudget-attempt2-polluted.json` (07:08 UTC) | 0.816/0.773 ms, 523/1,177 RPS | Load rose mid-run (1.06 → 3.69); RPS passes 523 vs 1,177, −55% apart |
| `docs/benchmark-2026-11-18-ledger-before-polluted.json` (06:52–06:53 UTC) | Pass 1 clean-looking (0.696 ms, 1,382 RPS, canary 19,511); pass 2 starved (448 RPS, p50 32.5 ms) | Another slice's gate started mid-run; kept separate, never the verdict |
| `docs/benchmark-2026-11-18-cpu-after-polluted.json`, `docs/benchmark-2026-11-18-cpu-after-polluted2.json`, `docs/benchmark-2026-11-18-cpu-before-polluted.json`, `docs/benchmark-2026-11-18-round2-baseline1-polluted.json` | — (numbers not verified by the keeper; files kept for the record) | Named `-polluted` in the repo; do not quote |

## New files awaiting keeper review

(None. Runner files landing in `docs/bench-results/` are triaged here first if the keeper
cannot parse them confidently — they are never promoted to section (b) unreviewed.)
