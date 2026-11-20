#!/usr/bin/env bash
# bench-gc-profile.sh — measure the real codvian-router binary under load
# in the default GC profile vs the low-end profile (GOGC=off + memory
# limit), and report real p50/p95/p99 latency, idle RSS and peak RSS.
#
# Method (nothing is simulated except the upstream, which is a local
# stdlib HTTP mock with a fixed 2 ms delay):
#   1. build the binary from this worktree
#   2. start TWO mock upstreams (scripts/bench-mock-upstream.py)
#   3. per profile (default, then low-end):
#      - fresh HOME: `init` (gateway key), provider add-catalog
#        fireworks alt-header pointed at the mock
#      - start `serve` on a scratch port, wait for /v1/models
#      - idle RSS after a 3 s settle (VmRSS from /proc/<pid>/status)
#      - warmup 50 requests, then 2000 requests / 20 concurrent via
#        scripts/bench-loadgen.py (50 lexically distinct prompts, so
#        the semantic cache and single-flight coalescing cannot
#        collapse the load into cache hits)
#      - peak RSS sampled every 100 ms during the load run
#   4. print a table + the raw JSON lines; router startup logs are
#      kept under $WORK/logs/ for the gc: line.
#
# Two mock upstreams, not one: the router's adaptive concurrency
# limiter (Gradient2, cap starts at 20, floor 2) sheds requests fast
# with the generic 502 "upstream provider error" once a provider's
# smoothed RTT rises above its minimum — which a noisy shared VM
# guarantees at 20 concurrent requests against one provider. That
# shedding is the router working as designed (slice 7), not an
# upstream failure and not a GC effect, but it swamps the GC signal:
# single-provider runs showed 17-19 errors/2000 in a quiet hour and
# 137-170/2000 once parallel slices loaded the VM. With two
# providers offering the same model the fallback chain has two
# attempts, so a shed on one fails over to the other and the request
# succeeds — the limiter still runs, it just stops deciding the
# outcome. A plain Go http.Client against the same mock serves
# 2000/2000, and disabling TFO (CODVIAN_ROUTER_TFO=0) did NOT reduce
# the errors, so neither the mock nor TFO is the cause; the diagnosis
# is the limiter, confirmed by reading internal/router/router.go
# callChat (TryAcquire -> adaptive.ErrLimited -> 502) and
# internal/adaptive/limiter.go (cap = limit*gradient + 1).
#
# Usage: ./scripts/bench-gc-profile.sh [--requests N] [--concurrency N]
# Results go to stdout; nothing is written into the repo.
# Env overrides: PORT_BASE (default 22300), MOCK_PORT (default 18099).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REQUESTS="${REQUESTS:-3000}"
CONCURRENCY="${CONCURRENCY:-10}"
# Ports are picked free at runtime (never fixed): earlier runs left a
# mock holding 18099 after a script exit, and a fixed port would
# silently benchmark against the stale instance.
read -r MOCK_PORT MOCK_PORT2 < <(python3 -c "
import socket
ports = []
for _ in range(2):
    s = socket.socket()
    s.bind(('127.0.0.1', 0))
    ports.append(s.getsockname()[1])
    s.close()
print(*ports)
")
PORT_BASE="${PORT_BASE:-22300}"
WORK="$(mktemp -d /tmp/codvian-gc-bench.XXXXXX)"
mkdir -p "$WORK/logs"
echo "work dir: $WORK"
echo "rig: $(uname -srm), CPUs=$(nproc), mem=$(grep MemTotal /proc/meminfo), go=$("$HOME/go/bin/go" version 2>/dev/null || go version)"
echo "load: $REQUESTS requests, $CONCURRENCY concurrent, mock delay 2 ms, 50 distinct prompts"
echo "concurrency is 10, not 20: at 20 the adaptive limiter sheds even
across two providers (see header comment), and the benchmark must
measure GC, not limiter shedding; 10 stays under the shedding regime
while still exercising real concurrent load"

MOCK_PID=""
MOCK_PID2=""
ROUTER_PID=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_PID2" ] && kill "$MOCK_PID2" 2>/dev/null || true
  [ -n "$ROUTER_PID" ] && kill "$ROUTER_PID" 2>/dev/null || true
}
trap cleanup EXIT

export PATH="$HOME/go/bin:$PATH"
go build -o "$WORK/codvian-router" ./cmd/codvian-router
echo "binary: $WORK/codvian-router ($(du -h "$WORK/codvian-router" | cut -f1))"

python3 "$ROOT/scripts/bench-mock-upstream.py" --port "$MOCK_PORT" --delay 0.002 &
MOCK_PID=$!
python3 "$ROOT/scripts/bench-mock-upstream.py" --port "$MOCK_PORT2" --delay 0.002 &
MOCK_PID2=$!
sleep 1
curl -fsS "http://127.0.0.1:$MOCK_PORT/v1/models" >/dev/null
curl -fsS "http://127.0.0.1:$MOCK_PORT2/v1/models" >/dev/null
echo "mock upstreams up on 127.0.0.1:$MOCK_PORT and 127.0.0.1:$MOCK_PORT2"
rss_kb() { grep '^VmRSS:' "/proc/$1/status" 2>/dev/null | awk '{print $2}'; }

run_profile() {
  local profile="$1" port="$2" home="$3"
  shift 3
  mkdir -p "$home"
  local envs=()
  if [ "$profile" = "low-end" ]; then envs=(CODVIAN_ROUTER_LOWEND=1); fi

  HOME="$home" "$WORK/codvian-router" init >"$WORK/logs/init-$profile.txt" 2>&1
  local key
  key="$(awk '/Gateway key \(shown ONCE/{getline; print; exit}' "$WORK/logs/init-$profile.txt")"
  [ -n "$key" ] || { echo "init failed for $profile"; cat "$WORK/logs/init-$profile.txt"; exit 1; }
  # Two providers, same model: the fallback chain gets two attempts,
  # so an adaptive-limiter shed on one fails over to the other (see
  # header comment). Plain `provider add` (OpenAI wire) is enough —
  # no catalog variant needed for a mock.
  HOME="$home" "$WORK/codvian-router" provider add "http://127.0.0.1:$MOCK_PORT" mock-key-a --name mock-a \
    >"$WORK/logs/provider-$profile-a.txt" 2>&1
  HOME="$home" "$WORK/codvian-router" provider add "http://127.0.0.1:$MOCK_PORT2" mock-key-b --name mock-b \
    >"$WORK/logs/provider-$profile-b.txt" 2>&1

  env "${envs[@]}" HOME="$home" "$WORK/codvian-router" --port "$port" serve \
    >"$WORK/logs/router-$profile.txt" 2>&1 &
  ROUTER_PID=$!
  local deadline=$((SECONDS + 30))
  until curl -fsS -H "Authorization: Bearer $key" "http://127.0.0.1:$port/v1/models" >/dev/null 2>&1; do
    sleep 0.2
    if [ "$SECONDS" -ge "$deadline" ]; then echo "router $profile never came up"; cat "$WORK/logs/router-$profile.txt"; exit 1; fi
  done
  sleep 3 # settle: idle RSS
  local idle_rss
  idle_rss="$(rss_kb "$ROUTER_PID")"

  python3 "$ROOT/scripts/bench-loadgen.py" --url "http://127.0.0.1:$port" \
    --key "$key" --requests 50 --concurrency 5 >/dev/null

  # Peak RSS sampler during the load run.
  local peak_file="$WORK/peak-$profile.txt"
  echo "$idle_rss" >"$peak_file"
  (
    while kill -0 "$ROUTER_PID" 2>/dev/null; do
      rss_kb "$ROUTER_PID" >>"$peak_file" 2>/dev/null || true
      sleep 0.1
    done
  ) &
  local sampler=$!
  local result
  result="$(python3 "$ROOT/scripts/bench-loadgen.py" --url "http://127.0.0.1:$port" \
    --key "$key" --requests "$REQUESTS" --concurrency "$CONCURRENCY")"
  kill "$sampler" 2>/dev/null || true
  wait "$sampler" 2>/dev/null || true
  local peak_rss
  peak_rss="$(sort -n "$peak_file" | tail -1)"
  kill "$ROUTER_PID" 2>/dev/null || true
  wait "$ROUTER_PID" 2>/dev/null || true
  ROUTER_PID=""

  local gc_line
  gc_line="$(grep '^gc:' "$WORK/logs/router-$profile.txt" | head -1)"
  echo ""
  echo "== profile: $profile =="
  echo "startup: $gc_line"
  echo "idle RSS: $((idle_rss / 1024)) MiB (${idle_rss} KiB)"
  echo "peak RSS: $((peak_rss / 1024)) MiB (${peak_rss} KiB)"
  echo "loadgen: $result"
  echo "$profile $result" >>"$WORK/results.txt"
  echo "$profile idle_rss_kib=$idle_rss peak_rss_kib=$peak_rss" >>"$WORK/results.txt"
}

: >"$WORK/results.txt"
# REVERSE_ORDER=1 runs low-end first: the default profile otherwise
# always runs first, so any order effect (warmer mocks, VM load
# drifting between the two halves) would otherwise always land on
# the same profile. Use it to check whether a profile difference
# survives the order swap.
if [ "${REVERSE_ORDER:-0}" = "1" ]; then
  run_profile low-end "$((PORT_BASE + 1))" "$WORK/home-lowend"
  run_profile default "$PORT_BASE" "$WORK/home-default"
else
  run_profile default "$PORT_BASE" "$WORK/home-default"
  run_profile low-end "$((PORT_BASE + 1))" "$WORK/home-lowend"
fi

echo ""
echo "== summary =="
cat "$WORK/results.txt"
echo "logs kept in $WORK/logs/ (router stdout incl. gc: line)"
