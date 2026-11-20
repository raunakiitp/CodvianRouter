# Codvian Router — Windows benchmark package (CHECKLIST 149 / 150 / 207 / 208)

## Status: HELD — pairing declined for now

**Do not run this package yet.** Raunak declined the Tailscale + OpenSSH
pairing for now (2026-11-19 ~15:40 IST, voice call): his PC is busy with
other work and he does not want it disturbed. **No Tailscale/SSH setup,
nothing runs on his computer; work stays VM-only.**

This package is **deferred preparation**, kept ready for whenever Raunak
approves pairing in future. When that happens, the executor is **the agent
over SSH** (Tailscale + Windows OpenSSH Server — see
`~/workspace/goals/codvian-router/hidden_files/pairing-research-2026-11-19.md`),
not Raunak running steps by hand: every command below is non-interactive,
SSH-runnable, and captures its output to files. Until then, nothing here
is executed on his machine.

## What this package is

A self-contained re-run of the Geekflare-style benchmark rig
(`scripts/bench-geekflare.sh`, `docs/BENCHMARK.md`) on Raunak's Windows PC,
to produce the honest quiet-window after-run that the VM could never give:

- **Item 149** (backend): hot-path fixes built (guardrail digit-shape gate,
  semantic-cache inverted index, ledger budget-update aggregation); the
  **after-run was never measured honestly** — E1 was invalidated twice by VM
  storms. Needs: a clean run of the full rig (added latency, RPS, idle
  memory, ready time) on a quiet machine.
- **Item 150** (dashboard): no dashboard code change — the Benchmark section
  already renders passes/targets/missed verdicts from `benchmark.json` via
  `GET /dashboard/benchmark`. Needs: live verification that the new run's
  numbers (misses included) show in the dashboard.
- **Item 207** (backend): the quiet re-run itself — the 2026-11-18 ~00:02 UTC
  attempt was **polluted** (loadavg ended 7.32 after a concurrent slice
  started a fsync-heavy test mid-run); ~25 minutes of waiting never produced
  a quiet VM window. Needs: the same rig, in a genuinely quiet window.
- **Item 208** (dashboard): live verification of a real quiet run's numbers
  in the dashboard (same blocker as 207).

One run of `run-bench-windows.ps1` covers all four items: it measures the
numbers 149/207 need and performs the dashboard-feed check 150/208 need.

## Targets (Geekflare-derived, research-log entry 148)

| Metric | Target | What measures it |
|---|---|---|
| Added latency (median, router p50 − direct-mock p50, 1,000 sequential POSTs) | **< 0.5 ms** | `codvian-bench latency` direct vs through router, x2 passes |
| RPS (32 clients x 20 s) | **> 3,000** | `codvian-bench rps` through router, x2 passes (direct-mock run is the canary) |
| Idle memory (RSS after 3 s settle) | **< 70 MB** | router process WorkingSet after settle |
| Ready time (spawn → first 200) | **< 10 s** | `GET /healthz` and `GET /v1/models` |

## Latest measured numbers (before this package runs)

Latest clean run on the VM: `docs/benchmark-2026-11-18-globalbudget.json`,
2026-11-18T07:52:12Z, limiter on, direct-mock canary 19,599 / 20,009 RPS.

| Metric | Pass 1 | Pass 2 | Verdict |
|---|---|---|---|
| Added latency | 0.851 ms | 0.777 ms | **MISSED** (~1.6x) |
| RPS (ok) | 1,273 | 1,174 | **MISSED** (~2.4x) |
| Idle memory | 43,064 KiB (42.1 MiB) | — | **MET** |
| Ready | /healthz 434.8 ms, /v1/models 565.0 ms | — | **MET** |

The earlier documented after-run in `docs/BENCHMARK.md` (attempt 3c pass 2,
2026-11-18 ~01:45 UTC) was 0.855 ms / 1,079 RPS, idle 38.4 MiB — same
verdicts. **The targets are NOT met. Do not claim otherwise.** The re-run
on Raunak's quiet PC is the next measurement, not a fix — no hot-path fix
is attempted in this package, and no code change is justified by a run
that has not happened yet.

## Binaries in this package (prebuilt, no Go toolchain needed on the PC)

Built 2026-11-19 from the worktree at HEAD `035d1ed`
(`035d1edeff8d78fbf6a129dcc43fdf809501837a`), Go 1.27.4,
`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath`:

| File | Size | SHA-256 |
|---|---|---|
| `codvian-bench.exe` | 10,705,920 bytes | `ad8943380b81b1fcf6d390af2c631868f2626fff0d9a59d17d8c54e40caeda06` |
| `codvian-router.exe` | 35,164,672 bytes | `f16e3eab732823493814cfcf92cf60a8162bbc2e97f908eba3f2f0a7547770fc` |

Both are PE (`MZ`) binaries; the bench binary's subcommands
(`mock` / `latency` / `rps`) were verified working on the VM with a Linux
build of the same source (mock serves `/v1/models`, latency + rps emit JSON
— see "VM verification" below). The Windows binaries themselves have **not**
been executed anywhere yet (no Windows machine available to this agent);
their first run is the future run on Raunak's PC.

**Honesty caveat — dirty-tree build.** The worktree had uncommitted changes
from sibling slices at build time, so these binaries are **not a clean-HEAD
build**: they are HEAD `035d1ed` plus whatever uncommitted edits were in
the tree at 2026-11-19 ~15:37–15:39 IST (including edits to
`cmd/codvian-router/main.go` and `internal/*`). If a clean-HEAD build is
wanted, rebuild from a clean checkout with the commands below and replace
the files, recording the new hashes here.

**Transient build failure observed.** The first `codvian-router.exe` build
failed because a sibling slice had momentarily broken a shared package
(`internal/config/gatewaykey.go: "net" and "strings" imported and not
used`). Per the package rules the file was NOT fixed by this agent; a
retry ~90 s later succeeded. The first build attempt also failed with
"no space left on device" because the VM's `/tmp` (512 MB tmpfs) was 98%
full — the successful builds used `TMPDIR=$HOME/.tmp`.

### Rebuild commands (from the worktree, if the binaries need refreshing)

```sh
export PATH=$HOME/go/bin:$PATH TMPDIR=$HOME/.tmp
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
  -o docs/packages/benchmark-windows/codvian-bench.exe ./cmd/codvian-bench
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
  -o docs/packages/benchmark-windows/codvian-router.exe ./cmd/codvian-router
```

Rebuild is only needed if the tree moves again before the future run; the
agent rebuilding must record the new HEAD hash, date, and SHA-256s here.
No Go toolchain is needed on Raunak's PC — the `.exe` files are the whole
runtime dependency (plus the PowerShell script below).

## Preconditions (future run, on Raunak's PC)

- Windows 10/11, PowerShell 5.1 or later. No Go install needed.
- Quiet machine: no other heavy work running — no builds, tests, video
  renders, or large downloads during the run. This is the Windows
  equivalent of the VM's loadavg < 3 rule. The script checks this itself
  (see below) and aborts if a `go.exe` process is running or average CPU
  is above 30% over a 5 s sample; it warns at 10–30%.
- The agent reaches the PC over SSH (Tailscale + OpenSSH Server, after
  Raunak approves the one-time setup). The agent copies this whole
  directory to the PC first (e.g. `C:\codvian-bench\`).
- Ports: three free loopback ports, picked dynamically by the script.
- Expected wall time: ~3–5 minutes (two passes of 1,000 sequential
  latency requests + 2 x 20 s RPS runs, direct and through the router).
- The script uses only loopback networking (`127.0.0.1`), a fresh isolated
  config + data dir under its work dir, and two instant mock upstreams —
  it never touches a real provider, real API keys, or any existing router
  install on the PC. The gateway key it creates is a throwaway, written to
  a file in the work dir only.

## How the future run is executed (agent over SSH)

Transfer this directory to the PC, then, over SSH, non-interactively:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\codvian-bench\run-bench-windows.ps1 -WorkDir C:\codvian-bench\run-2026-11-19
```

That is the whole run. The script then, with no prompts:

1. Preflight: verifies both `.exe` files exist, records machine info (CPU
   model, RAM, Windows version, `go version` if Go happens to be installed
   — "not installed" is fine and expected), samples CPU, aborts if a
   `go.exe` process is running or CPU averages > 30%.
2. Picks three free loopback ports; starts two `codvian-bench mock`
   upstreams (both serve model `model-a`, zero artificial delay).
3. Creates a fresh isolated home under the work dir and runs
   `codvian-router init --key-file` (throwaway gateway key, file only —
   never printed), then `provider add` for both mocks.
4. Starts the router (`--port <router-port> serve`), polls
   `GET /healthz` and `GET /v1/models` to first 200 (ready times), waits
   3 s, records idle RSS.
5. Runs pass 1 and pass 2: `latency` direct + through router (1,000
   requests, 25 warmup), `rps` direct + through router (32 clients, 20 s,
   25 warmup), sampling router peak RSS every 100 ms during the router
   RPS runs.
6. Merges everything into `bench-results.json`, prints the summary table.
7. Dashboard check (items 150/208): copies `bench-results.json` into the
   run's data dir as `benchmark.json`, restarts the router briefly, and
   records `GET /dashboard/benchmark` (and `GET /dashboard`) responses —
   the feed must serve both passes with the missed verdicts shown as
   misses, or the honest empty state if the file is absent.
8. Stops all processes it started. Everything it produced stays under the
   work dir for `scp` back to the VM.

## Exact commands, per item (manual fallback / what the script runs)

All commands are PowerShell, run from the package directory
(`C:\codvian-bench\` in the examples — the script's `-WorkDir` is where
results land). `<MOCK_A>`, `<MOCK_B>`, `<ROUTER>` are the three free ports
the script picks; the manual commands below use 21001 / 21002 / 21003 as
examples only — pick free ports if those are taken.

Setup (all four items share this):

```powershell
# Two instant mock upstreams (background jobs; logs to the work dir)
Start-Process .\codvian-bench.exe -ArgumentList 'mock','--port','21001' -RedirectStandardOutput .\run\logs\mock-a.txt -RedirectStandardError .\run\logs\mock-a.err.txt -WindowStyle Hidden -PassThru
Start-Process .\codvian-bench.exe -ArgumentList 'mock','--port','21002' -RedirectStandardOutput .\run\logs\mock-b.txt -RedirectStandardError .\run\logs\mock-b.err.txt -WindowStyle Hidden -PassThru

# Fresh isolated home + config (nothing touches an existing install)
$env:USERPROFILE = "$PWD\run\home"
.\codvian-router.exe --config "$PWD\run\config.json" init --key-file "$PWD\run\gateway.key"
$KEY = (Get-Content "$PWD\run\gateway.key" -Raw).Trim()
.\codvian-router.exe --config "$PWD\run\config.json" provider add http://127.0.0.1:21001 mock-key-a --name mock-a
.\codvian-router.exe --config "$PWD\run\config.json" provider add http://127.0.0.1:21002 mock-key-b --name mock-b

# Start the router (note: --config and --port come BEFORE "serve")
Start-Process .\codvian-router.exe -ArgumentList '--config',"$PWD\run\config.json",'--port','21003','serve' -RedirectStandardOutput .\run\logs\router.txt -RedirectStandardError .\run\logs\router.err.txt -WindowStyle Hidden -PassThru
```

Items 149 + 207 (the measurement — added latency, RPS, idle, ready):

```powershell
# Pass 1 (repeat all four lines for pass 2; --warmup 25, 1,000 requests)
.\codvian-bench.exe latency --base http://127.0.0.1:21001 --model model-a --requests 1000 --warmup 25
.\codvian-bench.exe latency --base http://127.0.0.1:21003 --key $KEY --model model-a --requests 1000 --warmup 25
.\codvian-bench.exe rps --base http://127.0.0.1:21001 --model model-a --concurrency 32 --duration 20s --warmup 25
.\codvian-bench.exe rps --base http://127.0.0.1:21003 --key $KEY --model model-a --concurrency 32 --duration 20s --warmup 25
```

Added latency = router `p50_ms` − direct-mock `p50_ms` (per pass).
RPS = the router run's `rps` field (also record `upstream_rps`, `ok`,
`errors`, `cache_hits`). The direct-mock RPS is the **canary**: on the VM
a clean run shows ~14,000–21,000 RPS direct; a pass whose canary drops far
below the other pass's canary is contended and must be reported as
polluted, not as a result. **No hard canary threshold is set for Raunak's
PC** — the VM's 12,000 rule is calibrated to the VM; on his machine the
absolute level will differ, so compare the two passes against each other
and record both numbers.

Idle memory and ready time are measured by the script (WorkingSet64 after
a 3 s settle; spawn-to-first-200 polling). Manually: idle RSS is
`(Get-Process -Id <router-pid>).WorkingSet64` 3 s after both ready probes
succeed; ready is the stopwatch time from process start to first HTTP 200
on `/healthz` and on `/v1/models` (with `Authorization: Bearer <key>`).

Items 150 + 208 (dashboard live verification):

```powershell
Copy-Item .\run\bench-results.json "$PWD\run\home\.local\share\codvian-router\benchmark.json"
# (re)start the router as above, then:
Invoke-WebRequest -Uri http://127.0.0.1:21003/dashboard/benchmark -UseBasicParsing
Invoke-WebRequest -Uri http://127.0.0.1:21003/dashboard -UseBasicParsing
```

The feed must serve both passes with `added_latency_met: false` /
`rps_met: false` when the targets are missed (misses shown as misses),
and the dashboard page must contain the Benchmark section. Record the
HTTP status and the response bodies; if the feed does not answer 200,
record that honestly — do not claim the check passed.

Stop everything when done:

```powershell
Get-Process codvian-bench, codvian-router -ErrorAction SilentlyContinue | Stop-Process
```

## What to capture (output files, all under the work dir)

| File | Contents |
|---|---|
| `bench-results.json` | Combined machine-readable run record (rig, ready, idle, both passes, canary, limiter mode) — the record of the run |
| `summary.txt` | The printed summary table (added latency, RPS, idle, ready, per pass) |
| `logs\mock-a.txt`, `logs\mock-b.txt` | Mock stdout/stderr |
| `logs\router.txt` | Router stdout/stderr for the measured run |
| `logs\init.txt`, `logs\provider-a.txt`, `logs\provider-b.txt` | Setup command output |
| `logs\dashboard-benchmark.json` | Raw `GET /dashboard/benchmark` response body |
| `logs\dashboard.html` | Raw `GET /dashboard` response body |
| `lat-direct-1.json`, `lat-router-1.json`, `rps-direct-1.json`, `rps-router-1.json` (+ `-2` for pass 2) | Per-measurement JSON from `codvian-bench` |
| `peak-kb-1.txt`, `peak-kb-2.txt` | Peak RSS (KiB) sampled during each router RPS run |
| `gateway.key` | Throwaway gateway key (file only; delete after the run — it is useless once the work dir is gone) |
| `config.json` | Throwaway router config for the run |

Report back: the filled-in `RESULTS-TEMPLATE.md` in this package plus the
whole work dir (or at least `bench-results.json`, `summary.txt`, and the
dashboard logs) copied back to the VM.

## Bench tool flags (verified from `cmd/codvian-bench/main.go`, not invented)

- `codvian-bench mock --port <n>` — instant stand-in upstream; serves
  `GET /v1/models` and `POST /v1/chat/completions` (model `model-a`),
  zero artificial delay. `--port` is required.
- `codvian-bench latency --base <url> [--key <gateway-key>] [--model model-a] [--requests 1000] [--warmup 25]` — sequential requests; prints one JSON
  object (`p50_ms`, `p95_ms`, `p99_ms`, `mean_ms`, `ok`, `errors`,
  `error_kinds`, `cache_hits`). No `--key` = no auth header (direct to mock).
- `codvian-bench rps --base <url> [--key <gateway-key>] [--model model-a] [--concurrency 32] [--duration 20s] [--warmup 25]` — closed-loop load;
  prints one JSON object (`rps`, `upstream_rps`, `ok`, `errors`,
  `error_kinds`, `cache_hits`, `p50_ms`, `p95_ms`, `p99_ms`, `completed`).
- There is no `-h` / `--help` subcommand and no other subcommand
  (`mock|latency|rps` only); running it bare prints
  `usage: codvian-bench <mock|latency|rps> [flags]`.
- Router CLI (verified from `cmd/codvian-router/main.go`): global flags
  `--config PATH` and `--port N` come **before** the subcommand;
  `init [--key-file PATH]` (fails if the config already exists — hence the
  fresh work dir); `provider add <base-url> <api-key> [--name NAME]`;
  `serve` (default subcommand). Data dir is
  `%USERPROFILE%\.local\share\codvian-router` and config defaults to
  `%APPDATA%\codvian-router\config.json` — the script overrides both with
  the isolated work dir.

## VM verification done for this package (2026-11-19)

- Linux build of `cmd/codvian-bench` from the same source: builds clean.
- Mock started on 127.0.0.1:21991, served `GET /v1/models`
  (`{"object":"list","data":[{"id":"model-a","object":"model"}]}`).
- `latency --requests 20 --warmup 5` direct to the mock: 20 ok, 0 errors,
  p50 0.11 ms. `rps --concurrency 4 --duration 3s`: 4,819 ok, 0 errors.
  This is a **smoke check only** — it proves the tool builds and runs, not
  any benchmark number.
- **No full benchmark re-run was done on the VM** — the VM is loaded and
  this is explicitly a quiet-window-only job. The full re-run is deferred
  to the future run on Raunak's PC.
- The PowerShell script in this package has **never been executed on
  Windows** (no Windows machine was available to this agent). It is a
  faithful port of `scripts/bench-geekflare.sh`, which has run many times
  on Linux; the first Windows execution is the future run, and any
  Windows-specific failure in it must be fixed then and recorded, not
  papered over.

## Honest limits of this package

- Binaries are a dirty-tree build (see above), not clean HEAD.
- The Windows quiet check is a CPU-% approximation, not Linux loadavg;
  there is no exact Windows equivalent of the loadavg < 3 rule.
- The direct-mock canary has no calibrated clean threshold for Raunak's
  PC; passes are judged against each other.
- If the future run is polluted (a background job, Windows Update, a
  second bench overlapping), the numbers are recorded as polluted and the
  run is repeated — a polluted run is never published as the after-run.
- The E1 limiter verdict (limiter on vs off, items 233–234) is already
  measured and is **not** re-run by this package; the limiter stays ON
  (default) for this re-run.
