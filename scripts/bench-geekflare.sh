#!/usr/bin/env bash
# bench-geekflare.sh — Geekflare-style benchmark rig for Codvian Router
# (CHECKLIST items 105-106, research-log entry 148).
#
# Method — the same rig Geekflare used, adapted to this repo:
#   * stand-in instant provider: codvian-bench mock (Go net/http,
#     zero artificial delay — the NGINX role in Geekflare's test).
#     TWO mocks offer the same model, so the router's adaptive
#     limiter shedding on one fails over to the other instead of
#     deciding the outcome (see scripts/bench-gc-profile.sh).
#   * added latency: median of 1,000 sequential POSTs through the
#     router minus the median of 1,000 sequential POSTs direct to
#     the mock (Geekflare's baseline was 0.17 ms direct to NGINX;
#     the subtraction cancels the mock's own cost either way).
#     Every prompt is distinct (50 topics x 20 frames) so the
#     router's exact/semantic cache and single-flight coalescing
#     cannot collapse the load into cache hits; cache hits are
#     counted from the X-Codvian-Cache header and reported.
#   * RPS: 32 concurrent clients x 20 s, through the router and
#     direct to one mock (the direct run is the mock-ceiling
#     reference: if the router's RPS ever approached it, the mock
#     would be the bottleneck and the router number a lower bound).
#   * idle memory: VmRSS after a 3 s settle; peak RSS sampled
#     every 100 ms during the RPS run.
#   * ready time: process spawn to first HTTP 200 on GET /healthz
#     (auth-exempt liveness probe), plus spawn to first 200 on
#     GET /v1/models with the gateway key as a second signal.
#
# The whole sequence runs twice (pass 1 = the reported run, pass 2
# = stability check) because this is a shared, noisy VM and a
# single pass would be one sample of a moving target.
#
# Usage: ./scripts/bench-geekflare.sh
# Results: one JSON object per measurement on stdout, a summary
# table, and a combined $WORK/bench-results.json (path printed).
# Nothing is written into the repo by this script.
#
# CODVIAN_BENCH_OUT=<path>: when set, the combined
# bench-results.json is also copied there — point it at the
# router's data dir (e.g. ~/.local/share/codvian-router/benchmark.json) so the
# dashboard's Benchmark section (GET /dashboard/benchmark) shows
# the last measured run with its target verdicts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"

read -r MOCK_PORT MOCK_PORT2 ROUTER_PORT < <(python3 -c "
import socket
ports = []
for _ in range(3):
    s = socket.socket()
    s.bind(('127.0.0.1', 0))
    ports.append(s.getsockname()[1])
    s.close()
print(*ports)
")

WORK="$(mktemp -d "${TMPDIR:-/tmp}/codvian-geekflare-bench.XXXXXX")"
mkdir -p "$WORK/logs"
# E1 limiter verdict (CHECKLIST 233): CODVIAN_BENCH_LIMITER=off runs
# the router with the Gradient2 limiter disabled
# (CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1); default on. The mode is
# recorded in bench-results.json ("limiter_mode") so a run can
# never be mistaken for the other mode.
LIMITER_MODE="${CODVIAN_BENCH_LIMITER:-on}"
case "$LIMITER_MODE" in on|off) ;; *) echo "bench-geekflare: CODVIAN_BENCH_LIMITER must be on|off, got $LIMITER_MODE" >&2; exit 2;; esac
if [ "$LIMITER_MODE" = "off" ]; then export CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1; fi
LOADAVG_START="$(cat /proc/loadavg)"
echo "limiter: $LIMITER_MODE (CODVIAN_BENCH_LIMITER), loadavg at start: $LOADAVG_START"
echo "work dir: $WORK"
echo "rig: $(uname -srm), CPUs=$(nproc), mem=$(grep MemTotal /proc/meminfo | awk '{print $2}') KiB, cpu_model=$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs), go=$(go version), loadavg=$(cat /proc/loadavg)"
echo "router version: $(grep -m1 '^const Version' "$ROOT/cmd/codvian-router/main.go" || echo unknown)"
echo "mock ports: $MOCK_PORT $MOCK_PORT2 (instant, zero delay), router port: $ROUTER_PORT"

MOCK_PID="" MOCK_PID2="" ROUTER_PID=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_PID2" ] && kill "$MOCK_PID2" 2>/dev/null || true
  [ -n "$ROUTER_PID" ] && kill "$ROUTER_PID" 2>/dev/null || true
}
trap cleanup EXIT

rss_kb() { grep '^VmRSS:' "/proc/$1/status" 2>/dev/null | awk '{print $2}'; }

# poll URL until HTTP 200 (or timeout_s); prints elapsed ms since
# $START_NS (nanosecond epoch passed as $3). $2 is an optional
# "Authorization: Bearer <key>" header value (empty = none).
poll_until_200() {
  python3 - "$1" "$2" "$3" <<'PYEOF'
import sys, time, urllib.request
url, auth, start_ns = sys.argv[1], sys.argv[2], int(sys.argv[3])
t0 = time.monotonic()
while True:
    try:
        req = urllib.request.Request(url)
        if auth:
            req.add_header("Authorization", auth)
        with urllib.request.urlopen(req, timeout=2) as r:
            if r.status == 200:
                print(f"{(time.time_ns() - start_ns) / 1e6:.1f}")
                sys.exit(0)
    except Exception:
        pass
    if time.monotonic() - t0 > 30:
        print("TIMEOUT")
        sys.exit(1)
    time.sleep(0.002)
PYEOF
}

go build -o "$WORK/codvian-router" ./cmd/codvian-router
go build -o "$WORK/codvian-bench" ./cmd/codvian-bench
echo "binaries: router $(du -h "$WORK/codvian-router" | cut -f1), bench $(du -h "$WORK/codvian-bench" | cut -f1)"

"$WORK/codvian-bench" mock --port "$MOCK_PORT" >"$WORK/logs/mock-a.txt" 2>&1 &
MOCK_PID=$!
"$WORK/codvian-bench" mock --port "$MOCK_PORT2" >"$WORK/logs/mock-b.txt" 2>&1 &
MOCK_PID2=$!
sleep 0.5
curl -fsS "http://127.0.0.1:$MOCK_PORT/v1/models" >/dev/null
curl -fsS "http://127.0.0.1:$MOCK_PORT2/v1/models" >/dev/null
echo "mock upstreams up on 127.0.0.1:$MOCK_PORT and 127.0.0.1:$MOCK_PORT2"

HOME_DIR="$WORK/home"
mkdir -p "$HOME_DIR"
HOME="$HOME_DIR" "$WORK/codvian-router" init >"$WORK/logs/init.txt" 2>&1
KEY="$(awk '/Gateway key \(shown ONCE/{getline; print; exit}' "$WORK/logs/init.txt")"
[ -n "$KEY" ] || { echo "init failed"; cat "$WORK/logs/init.txt"; exit 1; }
HOME="$HOME_DIR" "$WORK/codvian-router" provider add "http://127.0.0.1:$MOCK_PORT" mock-key-a --name mock-a >"$WORK/logs/provider-a.txt" 2>&1
HOME="$HOME_DIR" "$WORK/codvian-router" provider add "http://127.0.0.1:$MOCK_PORT2" mock-key-b --name mock-b >"$WORK/logs/provider-b.txt" 2>&1
echo "providers: mock-a (127.0.0.1:$MOCK_PORT), mock-b (127.0.0.1:$MOCK_PORT2), model model-a"

START_NS="$(date +%s%N)"
HOME="$HOME_DIR" "$WORK/codvian-router" --port "$ROUTER_PORT" serve >"$WORK/logs/router.txt" 2>&1 &
ROUTER_PID=$!
READY_HEALTHZ_MS="$(poll_until_200 "http://127.0.0.1:$ROUTER_PORT/healthz" "" "$START_NS")"
READY_MODELS_MS="$(poll_until_200 "http://127.0.0.1:$ROUTER_PORT/v1/models" "Bearer $KEY" "$START_NS")"
echo "ready: /healthz ${READY_HEALTHZ_MS} ms, /v1/models ${READY_MODELS_MS} ms (spawn to first 200)"
sleep 3 # settle: idle RSS
IDLE_RSS_KB="$(rss_kb "$ROUTER_PID")"
echo "idle RSS: $((IDLE_RSS_KB / 1024)) MiB (${IDLE_RSS_KB} KiB)"

run_pass() {
  local pass="$1"
  echo ""
  echo "== pass $pass =="
  "$WORK/codvian-bench" latency --base "http://127.0.0.1:$MOCK_PORT" --model model-a --requests 1000 --warmup 25 >"$WORK/lat-direct-$pass.json"
  echo "direct latency: $(cat "$WORK/lat-direct-$pass.json")"
  "$WORK/codvian-bench" latency --base "http://127.0.0.1:$ROUTER_PORT" --key "$KEY" --model model-a --requests 1000 --warmup 25 >"$WORK/lat-router-$pass.json"
  echo "router latency: $(cat "$WORK/lat-router-$pass.json")"
  "$WORK/codvian-bench" rps --base "http://127.0.0.1:$MOCK_PORT" --model model-a --concurrency 32 --duration 20s --warmup 25 >"$WORK/rps-direct-$pass.json"
  echo "direct rps: $(cat "$WORK/rps-direct-$pass.json")"
  # Peak RSS sampler during the router RPS run.
  local peak_file="$WORK/peak-$pass.txt"
  echo "$IDLE_RSS_KB" >"$peak_file"
  (
    while kill -0 "$ROUTER_PID" 2>/dev/null; do
      rss_kb "$ROUTER_PID" >>"$peak_file" 2>/dev/null || true
      sleep 0.1
    done
  ) &
  local sampler=$!
  "$WORK/codvian-bench" rps --base "http://127.0.0.1:$ROUTER_PORT" --key "$KEY" --model model-a --concurrency 32 --duration 20s --warmup 25 >"$WORK/rps-router-$pass.json"
  kill "$sampler" 2>/dev/null || true
  wait "$sampler" 2>/dev/null || true
  local peak_rss
  peak_rss="$(sort -n "$peak_file" | tail -1)"
  echo "router rps: $(cat "$WORK/rps-router-$pass.json")"
  echo "peak RSS during router rps (pass $pass): $((peak_rss / 1024)) MiB (${peak_rss} KiB)"
  echo "$peak_rss" >"$WORK/peak-kb-$pass.txt"
}

run_pass 1
run_pass 2

kill "$ROUTER_PID" 2>/dev/null || true
wait "$ROUTER_PID" 2>/dev/null || true
ROUTER_PID=""

python3 - "$WORK" "$READY_HEALTHZ_MS" "$READY_MODELS_MS" "$IDLE_RSS_KB" "$LIMITER_MODE" "$LOADAVG_START" <<'PYEOF'
import json, os, platform, subprocess, sys

work = sys.argv[1]
def load(name):
    with open(os.path.join(work, name)) as f:
        return json.load(f)

def peak(name):
    with open(os.path.join(work, name)) as f:
        return int(f.read().strip())

out = {
    "limiter_mode": sys.argv[5],
    "rig": {
        "os": platform.system() + " " + platform.release() + " " + platform.machine(),
        "cpus": os.cpu_count(),
        "mem_total_kib": int(open("/proc/meminfo").readline().split()[1]),
        "cpu_model": [l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")][0],
        "go_version": subprocess.run(["go", "version"], capture_output=True, text=True).stdout.strip(),
        "loadavg": open("/proc/loadavg").read().split()[:3],
        "loadavg_start": sys.argv[6].split()[:3],
        "date": subprocess.run(["date", "-Iseconds"], capture_output=True, text=True).stdout.strip(),
        "note": "Shared VM (1.5 shared vCPU budget), not a dedicated 2-vCPU droplet like Geekflare's; numbers are one sample per pass, pass 2 is the stability check.",
    },
    "ready_ms": {"healthz": float(sys.argv[2]), "v1_models": float(sys.argv[3])},
    "idle_rss_kib": int(sys.argv[4]),
    "passes": {},
}
for p in ("1", "2"):
    lat_d, lat_r = load(f"lat-direct-{p}.json"), load(f"lat-router-{p}.json")
    rps_d, rps_r = load(f"rps-direct-{p}.json"), load(f"rps-router-{p}.json")
    out["passes"][p] = {
        "direct_latency": lat_d, "router_latency": lat_r,
        "added_latency_p50_ms": round(lat_r["p50_ms"] - lat_d["p50_ms"], 3),
        "direct_rps": rps_d, "router_rps": rps_r,
        "peak_rss_kib": peak(f"peak-kb-{p}.txt"),
    }

with open(os.path.join(work, "bench-results.json"), "w") as f:
    json.dump(out, f, indent=2)

# CODVIAN_BENCH_OUT: also copy the combined results where the
# dashboard's Benchmark section reads them (the router's data dir
# benchmark.json). Best-effort — a failed copy must not fail the
# bench itself; the run's own $WORK copy is the record.
dest = os.environ.get("CODVIAN_BENCH_OUT", "")
if dest:
    try:
        import shutil
        shutil.copyfile(os.path.join(work, "bench-results.json"), dest)
        print(f"benchmark copy: {dest}")
    except OSError as e:
        print(f"benchmark copy failed ({dest}): {e}", file=sys.stderr)

# Summary table on stdout.
for p in ("1", "2"):
    d = out["passes"][p]
    print(f"\n== pass {p} summary ==")
    print(f"added latency (router p50 - direct p50): {d['added_latency_p50_ms']} ms "
          f"(router p50 {d['router_latency']['p50_ms']} ms, direct p50 {d['direct_latency']['p50_ms']} ms; "
          f"router cache_hits {d['router_latency']['cache_hits']}/{d['router_latency']['ok']}, "
          f"errors {d['router_latency']['errors']})")
    print(f"router RPS (32 clients x 20 s): {d['router_rps']['rps']} "
          f"(upstream {d['router_rps']['upstream_rps']}, ok {d['router_rps']['ok']}, "
          f"errors {d['router_rps']['errors']}, cache_hits {d['router_rps']['cache_hits']}); "
          f"direct mock RPS: {d['direct_rps']['rps']}")
print(f"\nready: /healthz {out['ready_ms']['healthz']} ms, /v1/models {out['ready_ms']['v1_models']} ms")
print(f"idle RSS: {out['idle_rss_kib'] / 1024:.1f} MiB; peak RSS pass 1: {out['passes']['1']['peak_rss_kib'] / 1024:.1f} MiB, pass 2: {out['passes']['2']['peak_rss_kib'] / 1024:.1f} MiB")
print(f"\ncombined results: {work}/bench-results.json")
PYEOF
echo "logs kept in $WORK/logs/ (router stdout incl. gc: line)"
