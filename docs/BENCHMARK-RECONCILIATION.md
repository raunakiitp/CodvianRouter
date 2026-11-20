# Benchmark Reconciliation — Codvian Router

Author: BENCH-ANALYST (Benchmark Reconciliation Analyst) — read-only analysis.
Date: 2026-11-19. Worktree: `~/workspace/codvian-router/`.
Scope: every `docs/benchmark-*.json` file (38 files), reconciled against
`docs/BENCHMARK.md`, `docs/cpu-profile-2026-11-18*.md`,
`docs/memory-profile-2026-11-18.md`, `docs/benchmark-round2-report.md`,
and the harness `scripts/bench-geekflare.sh`.

No file was edited or deleted. Every number below was read from the file
named next to it. Nothing here is estimated.

## 0. Rules used for the verdicts

- **Canary rule (the repo's own, from the E1/globalbudget verdicts):**
  a pass is clean iff its direct-mock RPS (the mock-ceiling canary) is
  ≥ 12,000 RPS. A file is clean iff every pass is clean.
- **Canary is necessary, not sufficient.** Three files pass the canary
  in both passes yet are polluted by a documented mid-run storm
  (load trace / RPS collapse / p50 blow-up). Those are marked POLLUTED
  with the reason.
- **`rig.loadavg` is the END-of-run load, not the start.** The harness
  (`scripts/bench-geekflare.sh:181,201-202`) reads `/proc/loadavg` into
  `rig.loadavg` when it merges the JSON at the end of the run, and
  captures `loadavg_start` separately at launch (line 69). Files made
  before the `loadavg_start` field was added have NO start-load record
  at all. Any text that quotes a file's `rig.loadavg` as "load at
  start" is wrong — see §3.4.
- **Headline RPS vs upstream RPS.** `rps` = ok responses per second
  (errors excluded — verified: 30,014 ok / 20.003 s = 1,500.5 in
  `benchmark-2026-11-18.json` pass 1). `upstream_rps` = ok minus
  cache hits per second. In RPS mode the prompt set is reused, so
  cache hits can be 22–37% of ok and the headline overstates real
  upstream throughput by that much. Both numbers are quoted wherever
  they differ materially.
- **502s are the Gradient2 limiter shedding offered load**, not mock
  failures (direct mock: 0 errors in every run). A 48% 502 rate does
  not make a file invalid — it makes the RPS number a measurement of
  a shedding regime, not of clean capacity.

Verdict scale: **VALID** (clean, quotable as a measurement of its
tree) · **MIXED** (one pass clean / one polluted, or clean canary
with documented contention — quote only the clean pass, with the
caveat) · **POLLUTED** (do not quote as a measurement; keep for the
record only).

## 1. Inventory — all 38 files

Times are UTC as recorded in each file's `rig.date`. Loadavg shown is
`rig.loadavg` (END of run) and, where recorded, `loadavg_start`.
Added latency = router p50 − direct p50, ms. RPS = ok/s (upstream in
brackets). 502% = errors / completed in RPS mode.

| # | File | Date (UTC) | What it measured | Load end → (start) | Added ms p1/p2 | RPS ok (upstream) p1/p2 | 502% p1/p2 | Canary p1/p2 | Verdict |
|---|---|---|---|---|---|---|---|---|---|
| 1 | `benchmark-2026-11-17.json` | 2026-11-17 17:55 | Primary pre-fix run | 8.45 (no start) | 1.632 / 1.727 | 475 (475) / 558 (557) | 21.8 / 20.4 | 14,093 / 11,512 | **MIXED** — p2 canary 11,512 fails rule; passes agree, usable with caveat |
| 2 | `benchmark-2026-11-18.json` | 2026-11-17 18:32 | Post-hot-path-fix run (00:02 IST) — **focus file, §3** | 7.05 (no start) | 0.677 / 0.511 | 1,500 (1,164) / 1,425 (1,076) | 48.1 / 48.3 | 15,658 / 16,232 | **VALID** |
| 3 | `benchmark-2026-11-18-rerun.json` | 2026-11-17 19:46 | Same tree, no code change, quieter start | 10.58 (no start) | 0.541 / 0.509 | 1,483 (1,063) / 1,426 (959) | 48.0 / 48.4 | 15,199 / 14,346 | **VALID** — corroborates #2 |
| 4 | `benchmark-2026-11-18-before.json` | 2026-11-17 20:37 | Before-run for checklist 149–150 fixes | 2.53 (no start) | 0.578 / 0.769 | 1,178 (745) / 1,470 (988) | 48.5 / 48.3 | 13,966 / 12,821 | **VALID** — headline RPS inflated by 37%/33% cache hits; quote upstream |
| 5 | `benchmark-2026-11-18-after-polluted.json` | 2026-11-18 00:02 | 149–150 after-run attempt (207–208) | 7.32 (no start) | 1.059 / 1.112 | 988 (984) / 899 (899) | 33.4 / 33.7 | 14,282 / 14,105 | **POLLUTED** — canary passes but a concurrent fsync-heavy test started mid-run; router p50 under load 14.3 ms vs 5.5 ms before |
| 6 | `benchmark-2026-11-18-round2-baseline1-polluted.json` | 2026-11-17 21:23 | Round-2 baseline attempt 1 (item-145 regression era) | 17.07 (no start) | 161.968 / 92.551 | 31 (11) / 5 (5) | 52.8 / 70.6 | 9,967 / 3,551 | **POLLUTED** — extreme storm, peak RSS 351–419 MiB |
| 7 | `benchmark-2026-11-18-round2-baseline2.json` | 2026-11-17 21:33 | Round-2 baseline attempt 2 — the 73.6 ms regression, measured | 4.23 (no start) | 73.638 / 73.862 | 41 (12) / 51 (13) | 53.7 / 49.5 | 15,761 / 14,730 | **VALID** — valid record *of the regression*; terrible numbers are the router's (profile-proven: `tiktoken-go.NewCoreBPE` rebuilt per request, 86.9% of CPU), not the VM's |
| 8 | `benchmark-2026-11-18-round2-phase2.json` | 2026-11-17 21:42 | Round-2 run A: encoder-cache fix only | 7.81 (no start) | 1.466 / 1.476 | 261 (216) / 261 (217) | 48.2 / 48.2 | 10,338 / 11,919 | **POLLUTED** — both canaries fail the 12,000 rule; directional only (73.6 ms → ~1.47 ms is far larger than VM noise and the profile proves the mechanism) |
| 9 | `benchmark-2026-11-18-round2-phase2b.json` | 2026-11-17 22:04 | Round-2 run B: encoder cache + auth memo | 19.35 (no start) | 1.013 / 1.123 | 307 (219) / 344 (228) | 48.2 / 48.8 | 5,422 / 4,628 | **POLLUTED** — storm mid-run (loadavg 19.35); underestimates the router |
| 10 | `benchmark-2026-11-18-attempt3-polluted.json` | 2026-11-18 01:39 | Quiet re-run attempt 3a (225–226) | 8.17 (no start) | 0.968 / 0.839 | 1,001 (1,000) / 1,018 (1,016) | 23.1 / 23.9 | 8,923 / 14,552 | **POLLUTED** — full `go test` overlapped the whole run |
| 11 | `benchmark-2026-11-18-attempt3b-polluted.json` | 2026-11-18 01:42 | Quiet re-run attempt 3b | 1.55 (no start) | 0.694 / 0.888 | 869 (869) / 1,182 (1,181) | 27.7 / 21.3 | 9,007 / 10,670 | **POLLUTED** — canary degraded in BOTH passes |
| 12 | `benchmark-2026-11-18-attempt3.json` | 2026-11-18 01:46 | Quiet re-run attempt 3c — published 149–150 after-run | 5.68 (no start) | 0.915 / 0.855 | 682 (673) / 1,079 (1,078) | 34.9 / 22.1 | 8,149 / 16,287 | **MIXED** — p1 polluted, **p2 VALID** (canary 16,287, best of the session); p2 is the citable number |
| 13 | `benchmark-2026-11-18-e1-on-polluted.json` | 2026-11-18 02:29 | E1 limiter-ON half | 0.63 (start 2.45) | 1.162 / 0.680 | 585 (552) / 1,324 (1,324) | 26.7 / 16.2 | 10,567 / 19,222 | **MIXED** — p1 polluted (reboot/fleet restart), p2 VALID |
| 14 | `benchmark-2026-11-18-e1-off-polluted.json` | 2026-11-18 02:52 | E1 limiter-OFF half (0 errors in 41,337 requests) | 2.25 (start 0.08) | 0.641 / 0.781 | 646 (646) / 1,419 (1,419) | 0.0 / 0.0 | 13,589 / 19,388 | **MIXED** — p1 mildly contended (full gate started at launch; router p50 40.9 ms), p2 VALID |
| 15 | `benchmark-2026-11-18-e1-polluted.json` | 2026-11-18 02:29–02:53 | E1 merged: on + off + summary + verdict | — | see #13/#14 | see #13/#14 | see #13/#14 | see #13/#14 | **MIXED** — top-level `clean: false`; verdict is explicitly pass-2-only (see §4) |
| 16 | `benchmark-2026-11-18-cpu-baseline.json` | 2026-11-18 03:29 | CPU-profile slice baseline (249–250) | 6.04 (start 1.31) | 0.683 / 0.672 | 1,313 (1,313) / 930 (920) | 17.3 / 27.3 | 20,124 / 19,771 | **VALID** |
| 17 | `benchmark-2026-11-18-cpu-before-polluted.json` | 2026-11-18 03:46 | CPU pair before-run, polluted attempt | 9.19 (start 1.29) | 0.917 / 0.897 | 305 (269) / 283 (266) | 42.2 / 45.3 | 5,488 / 10,867 | **POLLUTED** — both canaries fail |
| 18 | `benchmark-2026-11-18-cpu-before.json` | 2026-11-18 03:56 | CPU pair before-run (clean HEAD ± fixes) | 8.49 (start 1.68) | 0.708 / 0.814 | 692 (685) / 764 (753) | 25.0 / 20.1 | 16,218 / 15,560 | **VALID** |
| 19 | `benchmark-2026-11-18-cpu-after-polluted.json` | 2026-11-18 03:38 | CPU pair after-run, polluted attempt 1 | 9.55 (start 4.45) | 0.955 / 1.584 | 353 (213) / 1,010 (937) | 48.8 / 41.5 | 10,566 / 11,741 | **POLLUTED** — both canaries fail |
| 20 | `benchmark-2026-11-18-cpu-after-polluted2.json` | 2026-11-18 03:48 | CPU pair after-run, polluted attempt 2 | 13.53 (start 9.33) | 0.817 / 1.601 | 247 (168) / 216 (138) | 49.1 / 49.3 | 5,714 / 7,290 | **POLLUTED** — both canaries fail badly |
| 21 | `benchmark-2026-11-18-cpu-after.json` | 2026-11-18 03:58 | CPU pair after-run (token memo + single budget read) | 10.26 (start 8.49) | 0.697 / 0.570 | 1,650 (1,650) / 650 (604) | 18.5 / 34.0 | 15,171 / 17,906 | **VALID as a measurement, NO RPS VERDICT** — passes disagree +138.5% / −14.9% under opposite load regimes (before rising 1.7→9, after falling 8.5→3.7); the RPS delta is load, not fix. Latency 0.697/0.570 quotable |
| 22 | `benchmark-2026-11-18-cpu.json` | 2026-11-18 03:56–03:58 | CPU pair merged: before + after + cpu_profile | — | see #18/#21 | see #18/#21 | see #18/#21 | see #18/#21 | **VALID** (merged record; top-level passes identical to #21, `cpu_profile.before` identical to #18 — verified) |
| 23 | `benchmark-2026-11-18-cpu2-attempt1-polluted.json` | 2026-11-18 05:45 | Definitive re-measurement attempt 1 (273–274) | 11.58 (start 1.81) | 0.816 / 0.782 | 1,189 (1,184) / 247 (221) | 17.6 / 42.6 | 19,377 / 13,903 | **POLLUTED** — canary passes both passes, but p2 RPS collapsed −79% (fleet `go build` + catalog audit mid-run; p2 p50 47.9 ms). Proves the canary is necessary, not sufficient |
| 24 | `benchmark-2026-11-18-cpu2-attempt2-polluted.json` | 2026-11-18 05:56 | Definitive re-measurement attempt 2 | 3.90 (start 2.42) | 0.889 / 1.565 | 329 (304) / 666 (663) | 44.1 / 29.8 | 14,025 / 6,180 | **POLLUTED** — VM rebooted ~05:54 UTC; p2 canary fails |
| 25 | `benchmark-2026-11-18-cpu2-attempt3-polluted.json` | 2026-11-18 06:05 | Definitive re-measurement attempt 3 | 8.53 (start 1.32) | 1.976 / 1.287 | 598 (593) / 300 (277) | 25.7 / 36.1 | 8,761 / 14,870 | **POLLUTED** — fleet gate started seconds after launch; p1 canary fails |
| 26 | `benchmark-2026-11-18-cpu2-attempt4.json` | 2026-11-18 06:13 | Definitive re-measurement (273–274), the verdict run | 4.84 (start 1.45) | 0.908 / 0.711 | 1,213 (1,212) / 963 (962) | 15.7 / 19.9 | 15,205 / 16,756 | **VALID** — `clean: true` in merged file; load trace rose 1.45→4.84 and passes disagree −20.6% on RPS, disclosed in the verdict, not smoothed over |
| 27 | `benchmark-2026-11-18-cpu2-profile.json` | 2026-11-18 06:20 | Fresh profile run after the definitive run | 0.97 (start 1.25) | 0.800 / 0.844 | 1,363 (1,363) / 1,230 (1,229) | 10.7 / 13.6 | 21,058 / 21,160 | **VALID** — cleanest canaries of the session; corroborates #26, not a second verdict |
| 28 | `benchmark-2026-11-18-cpu2.json` | 2026-11-18 06:13 | Definitive merged: run + verdict + carried-over CPU card | — | 0.908 / 0.711 | 1,213 (1,212) / 963 (962) | 15.7 / 19.9 | 15,205 / 16,756 | **VALID** (top-level passes identical to #26 — verified) |
| 29 | `benchmark-2026-11-18-ledger-before-polluted.json` | 2026-11-18 06:53 | Ledger write-path before-run, first attempt | 0.82 (start 2.28) | 0.696 / 0.623 | 1,382 (1,381) / 448 (444) | 15.6 / 38.5 | 19,511 / 14,956 | **MIXED** — p1 VALID, p2 starved mid-run (another slice's gate); p1 quoted only as a latency cross-check |
| 30 | `benchmark-2026-11-18-ledger-before.json` | 2026-11-18 06:55 | Ledger write-path before-run (HEAD 0f751af8) | 4.72 (start 1.07) | 1.232 / 0.924 | 1,238 (1,237) / 1,220 (1,219) | 17.9 / 17.5 | 12,719 / 19,215 | **VALID with caveat** — p1 canary 12,719 only just above the rule; load rose 1.07→4.72 and p1 latency (1.232 ms) is likely load-inflated |
| 31 | `benchmark-2026-11-18-ledger-after.json` | 2026-11-18 07:00 | Ledger write-path after-run (multi-row INSERT) | 3.57 (start 2.27) | 0.680 / 0.624 | 1,393 (1,392) / 1,086 (1,081) | 12.3 / 17.4 | 19,396 / 20,567 | **VALID** |
| 32 | `benchmark-2026-11-18-ledger.json` | 2026-11-18 06:55–07:00 | Ledger merged: before + after + profile + `ledger_write_path` | — | see #30/#31 | see #30/#31 | see #30/#31 | see #30/#31 | **VALID** (top-level passes identical to #31, `cpu_profile.before` identical to #30 — verified) |
| 33 | `benchmark-2026-11-18-keybudget-attempt1-polluted.json` | 2026-11-18 06:54 | Key-budget memo attempt 1 (279–280) | 0.83 (start 1.28) | 0.859 / 0.923 | 1,254 (1,253) / 480 (476) | 16.3 / 36.2 | 11,963 / 13,942 | **POLLUTED** — p1 canary 11,963 marginally fails the rule; p2 RPS collapsed −62% |
| 34 | `benchmark-2026-11-18-keybudget-attempt2-polluted.json` | 2026-11-18 07:08 | Key-budget memo attempt 2 | 3.69 (start 1.06) | 0.816 / 0.773 | 523 (519) / 1,177 (1,177) | 31.6 / 19.8 | 15,622 / 17,463 | **POLLUTED** — canaries pass but load rose mid-run and RPS passes are −55% apart |
| 35 | `benchmark-2026-11-18-keybudget-run.json` | 2026-11-18 07:11 | Key-budget memo verdict run (HEAD ec029d5) | 0.91 (start 1.05) | 0.842 / 0.691 | 1,451 (1,451) / 1,432 (1,431) | 14.1 / 13.3 | 21,732 / 21,805 | **VALID** — cleanest run of the day; passes agree within 1.4% on RPS |
| 36 | `benchmark-2026-11-18-keybudget.json` | 2026-11-18 07:11 | Key-budget merged: run + verdict + carried-over definitive/CPU cards | — | 0.842 / 0.691 | 1,451 (1,451) / 1,432 (1,431) | 14.1 / 13.3 | 21,732 / 21,805 | **VALID** (top-level passes identical to #35 — verified) |
| 37 | `benchmark-2026-11-18-globalbudget-run.json` | 2026-11-18 07:52 | Global-budget memo verdict run (HEAD db5c626) — **latest verified** | 1.48 (start 1.30) | 0.851 / 0.777 | 1,273 (1,273) / 1,174 (1,173) | 14.8 / 17.6 | 19,599 / 20,009 | **VALID** |
| 38 | `benchmark-2026-11-18-globalbudget.json` | 2026-11-18 07:52 | Global-budget merged: run + verdict + definitive/CPU/key-budget cards | — | 0.851 / 0.777 | 1,273 (1,273) / 1,174 (1,173) | 14.8 / 17.6 | 19,599 / 20,009 | **VALID** — the current canonical benchmark record (top-level passes identical to #37 — verified) |

**Tally: 18 VALID · 6 MIXED · 14 POLLUTED.** No file was found to be
fabricated or internally inconsistent: in every merged file the
top-level passes are byte-identical in value to the raw run file
they carry (#22=#21, #28=#26, #32 top=#31, #36=#35, #38=#37,
#15 on/off = #13/#14), and the embedded verdict texts match the
numbers in their own files.

**Out of scope, noted so nobody confuses them:** `internal/server/testdata/`
holds 9 further `benchmark*.json` files (including `history/` and a
`-broken` fixture). Those are dashboard-test fixtures, not
measurements, and are not inventoried here.

**Other counts worth knowing:** 502s appear in 36 of the 38 files
(every file except the two E1 limiter-off runs, which shed nothing
by design). The 502 rate fell over the day — ~48% in the early
runs (#2, #3, #4) to ~13–20% in the latest clean runs (#35, #37)
— because offered load fell as per-request work got slower on a
bigger tree, not because shedding stopped. Idle RSS drifted up
across the clean runs: 37.0 MiB (#1) → 38.4 MiB (#12) → 41.5 MiB
(#26) → 42.1 MiB (#37). Peak RSS under load rose 37.5 MiB (#3) →
98.8 MiB (#12) → 108.8 MiB (#37); the memory-profile slice
(`docs/memory-profile-2026-11-18.md`) verdicts this "no leak —
legitimate, bounded buffering" (settled heap equals idle heap;
retained heap dominated by one-time tiktoken BPE tables, 11.8 MiB).

## 2. What "clean" actually means on this rig (read before quoting anything)

1. **The canary is the test, loadavg is not.** `rig.loadavg` is
   end-of-run and includes the bench's own 32-client load (e.g.
   #3 rerun: start 2.16, end 10.58 — the 10.58 is the bench, not
   background noise, per `docs/BENCHMARK.md`). A high end-loadavg
   alone never disqualifies a file; a failed canary does.
2. **Pass 2 is the stability check, not a second sample of the same
   thing.** Pass 2 reuses pass 1's prompt set against the same
   router, so its latency-mode cache hits (88–150 in several files)
   and RPS-mode cache hits (up to 9,630) are contamination, not
   signal. Pass 1 is the cleaner latency number wherever the two
   disagree.
3. **No clean run is a clean-room result.** Every VALID file above
   was measured on the shared 1.5-shared-vCPU VM. The numbers are
   comparable with each other (same rig, same harness) and are NOT
   comparable with Geekflare's dedicated 2-vCPU droplet except as
   the rough placement `docs/BENCHMARK.md` already labels them.
4. **The clean-run band is wide.** Across the VALID runs the
   added-latency band is 0.509–0.915 ms and the RPS band is
   650–1,650 ok/s. Any single number quoted without its band is
   misleading; the verdicts in §4 use the latest run plus the band.

## 3. Focus file: `docs/benchmark-2026-11-18.json`

### 3.1 Per-field verdict (each field read from the file, checked against `docs/BENCHMARK.md`)

| Field | File value | `docs/BENCHMARK.md` says | Verdict |
|---|---|---|---|
| `rig.date` | 2026-11-17T18:32:15+00:00 | 2026-11-17 18:32 UTC / 2026-11-18 ~00:02 IST | **TRUSTWORTHY** — matches |
| `rig.loadavg` | ["7.05", "2.98", "2.11"] | "load average 7.05 at run start" (§ "2026-11-18 run") and the G2 note repeats "loadavg 7.05 at start" | **MISLABELLED IN THE DOC, NOT IN THE FILE** — the harness writes end-of-run loadavg here (§0). 7.05 is the load at the END of the run (bench's own load included). Start load is NOT recorded in this file (no `loadavg_start` field — pre-dates it). See §3.4 |
| `rig` (os/cpu/go) | Linux 7.0.0-44-generic, AMD EPYC 9D25, Go 1.27.4, 4 cpus, 8,124,900 KiB | Same rig throughout | **TRUSTWORTHY** |
| `ready_ms` | healthz 158.5, v1_models 309.9 | 158.5 / 309.9 ms | **TRUSTWORTHY** — target < 10 s MET by ~30x |
| `idle_rss_kib` | 38,328 (37.4 MiB) | 38,328 KiB / 37 MiB | **TRUSTWORTHY** — target < 70 MB MET |
| Pass 1 added latency | 0.677 ms (router p50 0.769 − direct p50 0.092) | 0.677 ms | **TRUSTWORTHY** |
| Pass 2 added latency | 0.511 ms (0.620 − 0.109) | 0.511 ms | **TRUSTWORTHY** |
| Pass 1 RPS (headline) | 1,500.473 ok/s (30,014 ok / 20.003 s) | 1,500 | **TRUSTWORTHY as arithmetic, MISLEADING if quoted alone** — see §3.3 |
| Pass 2 RPS (headline) | 1,424.593 ok/s (28,551 ok / 20.042 s) | 1,425 | Same caveat as pass 1 |
| Pass 1 upstream RPS | 1,163.574/s | 1,164 (quoted only in the re-run section, not in the 2026-11-18 run table) | **TRUSTWORTHY — this is the real throughput number** |
| Pass 2 upstream RPS | 1,075.668/s | 1,076 (same) | **TRUSTWORTHY — this is the real throughput number** |
| Pass 1 502s | 27,811 / 57,825 = 48.1% | 27,811 × 502 = 48.1% | **TRUSTWORTHY** |
| Pass 2 502s | 26,630 / 55,181 = 48.3% | 26,630 × 502 = 48.3% | **TRUSTWORTHY** |
| Pass 1 RPS-mode cache hits | 6,739 (22.5% of ok) | Not mentioned in the 2026-11-18 run section | **TRUSTWORTHY, UNDISCLOSED IN THE DOC** — see §3.3 |
| Pass 2 RPS-mode cache hits | 6,993 (24.5% of ok) | Not mentioned | Same |
| Pass 1/2 latency-mode cache hits | 10 / 88 of 1,000 | 10 / 88 | **TRUSTWORTHY** — pass 2's 88 are the known pass-2 reuse contamination; pass 1 is the clean latency number |
| Direct-mock canaries | 15,658 / 16,232 RPS, 0 errors | 15,658 / 16,232 | **TRUSTWORTHY** — both pass the ≥12,000 clean rule |
| Peak RSS | 38,328 / 38,488 KiB (37.4 / 37.6 MiB) | 37 / 38 MiB | **TRUSTWORTHY** |

### 3.2 Is any number in it trustworthy? Yes — all of them, with two caveats

The file is a genuine record of the post-hot-path-fix tree
(00:02 IST run). It was independently re-read field by field for
this reconciliation and matches `docs/BENCHMARK.md`'s "2026-11-18
run" table exactly — the G2 note in `docs/BENCHMARK.md` claiming
that match is itself correct on the numbers. It is corroborated,
not contradicted, by the no-code-change rerun (#3: 0.541/0.509 ms,
1,483/1,426 RPS, same 48% 502 rate). The ~48% 502 rate does NOT
invalidate it: the 502s are the limiter shedding a closed-loop
client offering ~2,900 req/s against ~1,500 ok/s capacity, and the
latency phases ran with 0 errors. **The file's numbers are
trustworthy as a measurement of that tree under that load.**

The two caveats:

1. **The headline RPS overstates upstream throughput by ~22–24%.**
   6,739 / 6,993 of the ok responses were cache hits (the RPS
   mode reuses the prompt set, so the cache fills during the run).
   Real upstream throughput was 1,164 / 1,076 per s, not
   1,500 / 1,425. `docs/BENCHMARK.md`'s 2026-11-18 run table
   quotes only the headline; the upstream split is buried in the
   re-run section. Anyone quoting "1,500 RPS" from this file
   without the upstream figure is overstating by ~29%.
2. **The run-start load is unknown.** See §3.4. This does not
   change the numbers (both canaries pass), but it means the
   file cannot be used to claim "the VM was quiet at start".

### 3.3 What the file must never be quoted as

- Not the current tree: the tree has since grown (binary 19 MB →
  28 MB, ~30 slices between the before-run and attempt 3, then the
  CPU-profile, ledger, key-budget and global-budget fixes). The
  current numbers are §4's, not this file's.
- Not a before/after verdict on the hot-path fixes: the rerun
  (§ "2026-11-18 re-run" in `docs/BENCHMARK.md`) shows the
  latency move 0.677 → 0.541 ms with NO code change — VM noise,
  not a fix effect.
- Not capacity: 1,500 ok/s with 48% of offered load shed is a
  shedding-regime measurement. Capacity on this VM is the
  upstream ~1,164 / 1,076 per s with the limiter shedding the rest.

### 3.4 Correction found during this reconciliation (doc bug, not file bug)

`docs/BENCHMARK.md` states in two places that this file's loadavg
7.05 was "at run start" (the "2026-11-18 run" section and the
2026-11-19 G2 reconciliation note). That is wrong. Per
`scripts/bench-geekflare.sh:201-202`, `rig.loadavg` is read when
the JSON is merged at the END of the run; the start load lives in
`rig.loadavg_start`, a field this file predates and does not have.
For the rerun file the same doc gets it right ("10.58 recorded at
run end"). **Fix (for the coordinator / BENCHMARK.md owner, not
done here — this task is read-only): change both "7.05 at run
start" wordings to "7.05 at run end (bench's own load included);
start load not recorded in this file".** No number in the JSON
needs changing.

### 3.5 Recommendation: KEEP — do not delete; reconcile the surrounding text

- **KEEP `docs/benchmark-2026-11-18.json`.** It is validated,
  canary-clean, committed in git, corroborated by the rerun, and
  it is the only post-hot-path-fix record before the round-2
  regression (item 145) destroyed the tree's performance. Deleting
  it would erase the audit trail that proves the fixes' era
  measured 0.677/0.511 ms and 1,500/1,425 RPS (upstream
  1,164/1,076) — the baseline every later fix is judged against.
- **RECONCILE (text only, no re-run needed for this file):**
  (a) fix the two "7.05 at run start" wordings in
  `docs/BENCHMARK.md` per §3.4; (b) add the upstream-RPS split
  (1,164 / 1,076) and the RPS-mode cache hits (6,739 / 6,993)
  to the "2026-11-18 run" table, as the re-run section already
  does for its own run; (c) label the file in the doc as
  superseded-as-latest by `benchmark-2026-11-18-globalbudget.json`
  (§4), so it is never quoted as the current tree.
- **DELETE is rejected.** Nothing in the file is false, and the
  file's own history (48% 502s) is the reason it is informative,
  not a reason to remove it. A fresh quiet-window run is still
  needed — but for the CURRENT tree (§4), not to re-validate
  this file.

## 4. Numbers vs targets — latest VERIFIED numbers and what blocks each miss

Latest verified run: `docs/benchmark-2026-11-18-globalbudget.json`
(#38, 2026-11-18 07:52 UTC, HEAD db5c626, `clean: true`, canaries
19,599 / 20,009 RPS, load 1.30 → 1.48, no `go test`/`go build`
running). Raw run: `docs/benchmark-2026-11-18-globalbudget-run.json`.

| Metric | Geekflare target | Latest verified (globalbudget, p1 / p2) | Verdict |
|---|---|---|---|
| Added latency (median) | < 0.5 ms | **0.851 / 0.777 ms** | MISSED (~1.7x / ~1.6x) |
| RPS (32 clients × 20 s, ok) | > 3,000 | **1,273 / 1,174** (upstream 1,273 / 1,173 — only 10 cache hits, so headline ≈ upstream this time) | MISSED (~2.4x / ~2.6x) |
| RPS errors | — | 4,432 / 29,926 (14.8%) and 5,019 / 28,517 (17.6%) HTTP 502 | — |
| Idle memory (RSS) | < 70 MB | **42.1 MiB** (43,064 KiB) | MET |
| Peak RSS under load | — | 78.2 / 108.8 MiB | — (no target; no leak per memory-profile slice) |
| Ready time | < 10 s | **0.435 s** `/healthz`, 0.565 s `/v1/models` | MET (by ~18x) |

Correction to the brief: the brief's "idle ~38 MiB" is stale
(attempt-3 era, 38.4 MiB). The latest verified run records
43,064 KiB = 42.05 MiB — still MET, but idle RSS has drifted up
37.0 → 42.1 MiB across the clean runs as the binary grew. Quote
42.1 MiB, not ~38 MiB.

**Context — the band, not just the point:** the last four clean
verdict runs sit in the same band with no step-change between
them: definitive (#26) 0.908/0.711 ms, 1,213/963 RPS; ledger-after
(#31) 0.680/0.624 ms, 1,393/1,086 RPS; keybudget (#35) 0.842/0.691
ms, 1,451/1,432 RPS; globalbudget (#37) 0.851/0.777 ms, 1,273/1,174
RPS. Every hot-path fix in that sequence removed the profile share
it targeted and moved the harness by noise only. The ceiling is
diffuse per-request CPU, not any one fixed hot spot.

### Blockers — for the build department (analysis only; no code changed)

1. **Per-request CPU is the binding constraint; nothing else is.**
   E1 verdict (`docs/benchmark-2026-11-18-e1-polluted.json`,
   pass-2 pair only): limiter-off sheds 0% (vs 16.2% on) yet RPS
   rises only +7.2% (1,324 → 1,419), inside the ±10% noise band,
   while under-load p50 gets worse (9.3 → 11.0 ms). The Gradient2
   limiter is NOT the RPS ceiling — it only converts overload into
   502s. **Do not tune the limiter (cap/floor) to chase 3,000 RPS.**
2. **The sequential-latency floor caps RPS arithmetically.**
   Router sequential p50 is ~0.77–0.94 ms on this VM; the doc's
   own arithmetic puts the ceiling at ~2,300 RPS on 1.5 shared
   vCPU even at zero queueing. Reaching 3,000 needs ~2–2.4x less
   per-request CPU, or a bigger/dedicated box. No limiter or
   admission change can create that capacity.
3. **The two budget reads are now the biggest named router cost —
   together ~16% of CPU.** Fresh globalbudget profile
   (`docs/cpu-profile-2026-11-18-globalbudget.md`, 39.31 s
   samples): `GetKeyBudget` 8.27% cum + `GetBudget` 7.96% cum
   (via `CheckBudgetOrError` 8.01%), each one SQLite read per
   request; `_sqlite3RunParser` 7.78% cum overlapping every ledger
   query; `mallocgc` 6.87% cum. The per-request memos already
   removed the second read of each; what is left is one read each.
   The next lever named in the profile is combining the two budget
   reads into one round trip (or a cross-request cache with an
   explicit invalidation contract) — both change the staleness
   bound and need their own slice. The global-budget memo itself
   was proven within-noise by design (the guard runs once per
   request on today's hot paths; `GetBudget` 8.20% → 7.96%).
4. **The remaining cost is diffuse — no single hot spot above ~9%
   cum.** Syscalls ~12.8% flat (network I/O, inherent, not a
   lever); semantic-cache `getScoped` ~3.3% (already investigated
   in 263–264, no cheap win — do not re-try); `guardrails.scan`
   ~3.0%. Expect incremental gains only from further hot-path
   work; the step-change options are a bigger/dedicated box or
   front-door admission.
5. **Front-door admission control is still unbuilt** (checklist
   149–150 follow-up). It would reject overload BEFORE the
   expensive per-request work. Honest scope, per the E1 section:
   it turns 502s into 429s or queued latency — it does NOT raise
   ok/s. Build it for correctness of overload behaviour, not as
   a route to 3,000 RPS.
6. **The 502 rate is a symptom of the closed-loop client, not a
   separate bug.** The bench offers what the router completes, so
   offered load tracks capacity; the limiter sheds the excess.
   The rate fell 48% → ~15% across the day only because offered
   load fell as the tree got slower. Do not chase the 502 rate
   as a target; chase per-request CPU.
7. **Peak RSS under load (108.8 MiB) is not a leak and not a
   latency blocker.** Memory-profile verdict: settled heap equals
   idle heap; the delta is heap arenas held at high-water plus
   bounded buffers (tiktoken BPE tables 11.8 MiB one-time, ledger
   batching 1.56 MiB, per-key limiter 1.10 MiB, per-connection
   bufio ~2 MiB). GC is exonerated as the p50 driver (2.6–3.3%
   CPU). Watch idle-RSS drift (37.0 → 42.1 MiB), but do not spend
   a slice on peak RSS.
8. **Measurement blocker (process, not code): no fully quiet window
   has ever been obtained.** Every clean verdict above rests on
   the canary rule in a window the fleet did not stay quiet in
   (definitive run: load rose 1.45 → 4.84 mid-run; passes
   disagree up to −20.6% on RPS). A Fleet-Manager-coordinated
   bench pause — or a dedicated box — is still the unmet
   prerequisite for a definitive current-tree number, and for
   re-running E1 fully clean (two runs, all four canaries ≥16k).
   Until then, quote the band (§2.4), never a single point.

## 5. What a fresh run must look like (for the coordinator)

- Harness `scripts/bench-geekflare.sh`, two passes, quiet window:
  loadavg < ~3 at start AND end, no other slice benching or testing,
  direct-mock canary ≥ 12,000 RPS in every pass (target ≥ 16k for a
  fully clean pair).
- Record `loadavg_start` (the harness already does) and quote
  upstream RPS alongside headline RPS, with RPS-mode cache hits.
- Publish only via the merged file with an embedded verdict
  (`clean` flag + load trace), as #28/#36/#38 do; keep polluted
  attempts under `-polluted` names, never as the verdict.
- Do not present the result as a ranking against Geekflare's
  droplet numbers — same rig only, rough placement at most
  (deliberate divergence, `docs/BENCHMARK.md`).

## 6. Sources

- All 38 `docs/benchmark-*.json` files (read in full; per-pass
  canary, 502 rate, cache hits, loadavg computed from the files).
- `docs/BENCHMARK.md` (narrative + verdict tables; includes the
  2026-11-19 G2 note on the focus file).
- `docs/cpu-profile-2026-11-18.md`,
  `docs/cpu-profile-2026-11-18-cpu2.md`,
  `docs/cpu-profile-2026-11-18-keybudget.md`,
  `docs/cpu-profile-2026-11-18-globalbudget.md`,
  `docs/memory-profile-2026-11-18.md`,
  `docs/benchmark-round2-report.md`.
- `scripts/bench-geekflare.sh` (lines 69–72, 181, 201–202:
  loadavg capture points).
- Git: `docs/benchmark-2026-11-18.json` is committed (not a stray
  working-tree file); only `docs/BENCHMARK.md` is modified in the
  working tree at time of writing.
