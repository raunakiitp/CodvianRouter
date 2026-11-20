#!/usr/bin/env bash
# bench-idle-ready.sh — standalone idle-RSS + ready-time measurement
# (bench department; companion to scripts/bench.sh and the
# scripts/bench-geekflare.sh rig).
#
# What it measures, and nothing else:
#   * ready: wall time from process spawn to the first HTTP 200 on
#     GET /healthz (auth-exempt liveness), plus spawn to first 200
#     on GET /v1/models with the gateway key — the same two signals
#     the rig measures (docs/BENCHMARK.md, "Methodology").
#   * idle: VmRSS from /proc/<pid>/status after the server has sat
#     idle for 60 s (default; --idle-seconds to change), with the
#     server still answering /healthz at the end of the wait.
#
# Method: builds the router binary ONCE from this worktree, starts
# it on a fixed port (default 127.0.0.1:21337) with a fixed minimal
# config (fresh temp HOME, `init`, ZERO providers — no upstream is
# needed for a ready/idle measurement, and no provider means no
# upstream can move the numbers), polls for readiness, waits out
# the idle period while sampling the loadavg every 5 s, reads RSS,
# kills the server and removes the temp dir.
#
# Quiet rule (same as scripts/bench.sh): the script takes the
# department flock ($ROOT/.bench-lock), prints the exact command,
# git HEAD, Go version and loadavg, and REFUSES (exit 1, nothing
# recorded) while the VM is not quiet at start — 1-min loadavg
# >= 3, or any `go test` / `go build` running. The build itself
# loads the VM, so after building it waits (up to 180 s) for the
# loadavg to fall back below 3 before spawning the server. During
# the idle wait it samples loadavg every 5 s and watches for
# go test/go build: if any sample shows loadavg >= 3 or a test/
# build process, the numbers are printed with "clean": false and
# the verdict POLLUTED, and the exit code is 1 — a stormed idle
# window is evidence, never a quotable result.
#
# Targets (Geekflare-derived, docs/BENCHMARK.md):
#   ready (spawn to first /healthz 200)  < 10 s
#   idle memory (RSS after 60 s idle)   < 70 MiB
# A MISSED target is a measurement, not a script failure: the exit
# code stays 0 and the verdict says MISSED. Only a polluted window
# or a failure to start/measure exits non-zero.
#
# Usage:
#   ./scripts/bench-idle-ready.sh [--out PATH] [--port PORT]
#       [--idle-seconds N]
#     --out PATH        write the JSON record here (default: stdout
#                       only; a polluted run writes to
#                       <PATH without .json>-polluted.json instead)
#     --port PORT       fixed listen port (default: 21337)
#     --idle-seconds N  idle wait before the RSS read (default: 60)
#   A JSON record goes to stdout (and --out when given); a short
#   human table goes to stderr.
# Exit: 0 when a clean measurement was produced, 1 otherwise.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"

PORT="${CODVIAN_BENCH_IDLE_PORT:-21337}"
IDLE_SECONDS="${CODVIAN_BENCH_IDLE_SECONDS:-60}"
OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out)          [ $# -ge 2 ] || { echo "bench-idle-ready.sh: --out needs a path" >&2; exit 2; }
                    OUT="$2"; shift 2 ;;
    --port)         [ $# -ge 2 ] || { echo "bench-idle-ready.sh: --port needs a number" >&2; exit 2; }
                    PORT="$2"; shift 2 ;;
    --idle-seconds) [ $# -ge 2 ] || { echo "bench-idle-ready.sh: --idle-seconds needs a number" >&2; exit 2; }
                    IDLE_SECONDS="$2"; shift 2 ;;
    -h|--help) sed -n '2,53p' "$0"; exit 0 ;;
    *) echo "bench-idle-ready.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$PORT" in ''|*[!0-9]*) echo "bench-idle-ready.sh: --port must be a number, got $PORT" >&2; exit 2;; esac
case "$IDLE_SECONDS" in ''|*[!0-9]*) echo "bench-idle-ready.sh: --idle-seconds must be a number, got $IDLE_SECONDS" >&2; exit 2;; esac
if [ -n "$OUT" ]; then
  case "$OUT" in /*) ;; *) OUT="$PWD/$OUT";; esac
fi
cd "$ROOT"

COMMAND="$0 --port $PORT --idle-seconds $IDLE_SECONDS${OUT:+ --out $OUT}"
LOCK_FILE="$ROOT/.bench-lock"

load1() { awk '{print $1}' /proc/loadavg; }
load_ge_3() { awk -v l="$1" 'BEGIN { exit ((l + 0) >= 3) ? 0 : 1 }'; }
# `go test` / `go build` processes that are NOT us and NOT our
# ancestors: a coordinator shell that launched us may itself
# contain the words "go test" in its command line, and that is
# not contention — only foreign test/build activity is.
foreign_go_test_build() {
  local ancestors=" $$ " p="$PPID"
  while [ -n "$p" ] && [ "$p" != "0" ]; do
    ancestors="$ancestors$p "
    p="$(awk '/^PPid:/ {print $2}' "/proc/$p/status" 2>/dev/null || true)"
  done
  pgrep -af 'go (test|build)' 2>/dev/null | while read -r pid rest; do
    case "$ancestors" in *" $pid "*) ;; *) echo "$pid $rest";; esac
  done
}

# --- Department lock, header, start gate. ---
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "bench-idle-ready.sh: REFUSED — another benchmark holds the department lock ($LOCK_FILE)." >&2
  echo "bench-idle-ready.sh: A benchmark is already running. Wait for it to finish, then retry." >&2
  exit 1
fi
if GIT_HEAD="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null)"; then
  _n="$(git -C "$ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$_n" = "0" ]; then GIT_STATE="clean"; else GIT_STATE="dirty ($_n files)"; fi
else
  GIT_HEAD="UNKNOWN"; GIT_STATE="unknown"
fi
GO_VERSION="$(go version 2>/dev/null || echo UNKNOWN)"
LOADAVG_START="$(cat /proc/loadavg)"
{
  echo "bench-idle-ready.sh: command: $COMMAND"
  echo "bench-idle-ready.sh: git HEAD: $GIT_HEAD ($GIT_STATE)"
  echo "bench-idle-ready.sh: go: $GO_VERSION"
  echo "bench-idle-ready.sh: port: 127.0.0.1:$PORT, idle wait: ${IDLE_SECONDS}s, minimal config: fresh HOME + init, zero providers"
  echo "bench-idle-ready.sh: loadavg at start: $LOADAVG_START"
} >&2
if load_ge_3 "$(load1)"; then
  echo "bench-idle-ready.sh: REFUSED — VM is not quiet: 1-min loadavg $(load1) >= 3." >&2
  echo "bench-idle-ready.sh: Wait for a quiet window (loadavg < 3, no go test/go build running), then retry. No result was recorded." >&2
  exit 1
fi
FOREIGN="$(foreign_go_test_build)"
if [ -n "$FOREIGN" ]; then
  echo "bench-idle-ready.sh: REFUSED — VM is not quiet: go test/go build processes are running:" >&2
  printf '%s\n' "$FOREIGN" | sed 's/^/bench-idle-ready.sh:   /' >&2
  echo "bench-idle-ready.sh: Wait for them to finish, then retry. No result was recorded." >&2
  exit 1
fi

# --- Work dir + server lifecycle. Everything lives under $WORK
# (fresh HOME, logs, binary) and is removed on exit, whatever
# happens; the server is killed (SIGTERM, then SIGKILL after 5 s).
WORK="$(mktemp -d "${TMPDIR:-/tmp}/codvian-idle-ready-bench.XXXXXX")"
mkdir -p "$WORK/logs" "$WORK/home"
SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    if kill -0 "$SERVER_PID" 2>/dev/null; then
      kill "$SERVER_PID" 2>/dev/null || true
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        kill -0 "$SERVER_PID" 2>/dev/null || break
        sleep 0.5
      done
      kill -9 "$SERVER_PID" 2>/dev/null || true
    fi
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# Fixed port must be free, or the measurement is meaningless.
if ! python3 -c "import socket; s=socket.socket(); s.bind(('127.0.0.1', $PORT)); s.close()" 2>/dev/null; then
  echo "bench-idle-ready.sh: REFUSED — port 127.0.0.1:$PORT is already in use. Pick another --port." >&2
  exit 1
fi

# (a) Build the binary once.
echo "bench-idle-ready.sh: building the router binary once..." >&2
go build -o "$WORK/codvian-router" ./cmd/codvian-router
echo "bench-idle-ready.sh: binary: $(du -h "$WORK/codvian-router" | cut -f1)" >&2

# (b) Fixed minimal config: fresh HOME, `init`, zero providers.
HOME="$WORK/home" "$WORK/codvian-router" init >"$WORK/logs/init.txt" 2>&1
KEY="$(awk '/Gateway key \(shown ONCE/{getline; print; exit}' "$WORK/logs/init.txt")"
[ -n "$KEY" ] || { echo "bench-idle-ready.sh: init failed"; cat "$WORK/logs/init.txt"; exit 1; }
echo "bench-idle-ready.sh: minimal config ready (fresh HOME, zero providers)" >&2

# The build just loaded the VM; wait for the loadavg to fall back
# below 3 (and no test/build running) before spawning, or the ready
# time measures our own build's tail, not the router.
WAITED=0
while :; do
  if ! load_ge_3 "$(load1)" && [ -z "$(foreign_go_test_build)" ]; then break; fi
  if [ "$WAITED" -ge 180 ]; then
    echo "bench-idle-ready.sh: REFUSED — VM never quieted within 180 s after the build (loadavg $(load1)). No result was recorded." >&2
    exit 1
  fi
  sleep 5; WAITED=$((WAITED + 5))
done
[ "$WAITED" -gt 0 ] && echo "bench-idle-ready.sh: waited ${WAITED}s for quiet after build" >&2

# poll URL until HTTP 200 (or 30 s timeout); prints elapsed ms since
# $START_NS (nanosecond epoch passed as $3). $2 is an optional
# "Authorization: Bearer <key>" header value (empty = none).
# Same poller as scripts/bench-geekflare.sh, so the numbers are
# comparable with the rig's ready times.
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

# (b/c) Spawn and measure ready: spawn to first 200 on /healthz,
# then spawn to first 200 on /v1/models (same two signals as the rig).
START_NS="$(date +%s%N)"
HOME="$WORK/home" "$WORK/codvian-router" --port "$PORT" serve >"$WORK/logs/router.txt" 2>&1 &
SERVER_PID=$!
if ! READY_HEALTHZ_MS="$(poll_until_200 "http://127.0.0.1:$PORT/healthz" "" "$START_NS")"; then
  echo "bench-idle-ready.sh: FAILED — /healthz never answered 200 within 30 s of spawn." >&2
  tail -20 "$WORK/logs/router.txt" >&2 || true
  exit 1
fi
if ! READY_MODELS_MS="$(poll_until_200 "http://127.0.0.1:$PORT/v1/models" "Bearer $KEY" "$START_NS")"; then
  echo "bench-idle-ready.sh: FAILED — /v1/models never answered 200 within 30 s of spawn." >&2
  tail -20 "$WORK/logs/router.txt" >&2 || true
  exit 1
fi
echo "bench-idle-ready.sh: ready: /healthz ${READY_HEALTHZ_MS} ms, /v1/models ${READY_MODELS_MS} ms (spawn to first 200)" >&2

# Pollution flags for the measurement window (ready + idle wait).
# Any sample at/after spawn that shows loadavg >= 3 or a foreign
# go test/go build means the window was not quiet.
POLLUTED_LOAD=0
POLLUTED_PROC=0
READY_LOAD="$(load1)"
if load_ge_3 "$READY_LOAD"; then POLLUTED_LOAD=1; fi
FOREIGN="$(foreign_go_test_build)"
if [ -n "$FOREIGN" ]; then POLLUTED_PROC=1; fi

# (c) Idle wait: sample loadavg every 5 s, watch for test/build,
# and confirm the server stays alive throughout.
echo "bench-idle-ready.sh: idling ${IDLE_SECONDS}s (sampling loadavg every 5 s)..." >&2
MAX_LOAD="$READY_LOAD"
ELAPSED=0
while [ "$ELAPSED" -lt "$IDLE_SECONDS" ]; do
  REM=$((IDLE_SECONDS - ELAPSED))
  if [ "$REM" -gt 5 ]; then STEP=5; else STEP="$REM"; fi
  sleep "$STEP"
  ELAPSED=$((ELAPSED + STEP))
  L="$(load1)"
  if awk -v a="$L" -v b="$MAX_LOAD" 'BEGIN { exit (a > b) ? 0 : 1 }'; then MAX_LOAD="$L"; fi
  if load_ge_3 "$L"; then POLLUTED_LOAD=1; fi
  FOREIGN="$(foreign_go_test_build)"
  if [ -n "$FOREIGN" ]; then POLLUTED_PROC=1; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "bench-idle-ready.sh: FAILED — server died during the idle wait." >&2
    tail -20 "$WORK/logs/router.txt" >&2 || true
    exit 1
  fi
done

# Server must still be healthy at the end of the wait; then read RSS.
if ! curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
  echo "bench-idle-ready.sh: FAILED — server stopped answering /healthz after the idle wait." >&2
  tail -20 "$WORK/logs/router.txt" >&2 || true
  exit 1
fi
IDLE_RSS_KB="$(grep '^VmRSS:' "/proc/$SERVER_PID/status" 2>/dev/null | awk '{print $2}')"
[ -n "$IDLE_RSS_KB" ] || { echo "bench-idle-ready.sh: FAILED — could not read RSS for pid $SERVER_PID." >&2; exit 1; }
LOADAVG_END="$(cat /proc/loadavg)"

POLLUTION=""
[ "$POLLUTED_LOAD" = 1 ] && POLLUTION="1-min loadavg >= 3 during the window (max $MAX_LOAD)"
[ "$POLLUTED_PROC" = 1 ] && POLLUTION="${POLLUTION:+$POLLUTION; }a go test/go build process ran during the window"
CLEAN=true
[ -n "$POLLUTION" ] && CLEAN=false

# --- Record: one JSON object on stdout (and --out when given),
# human table on stderr. A MISSED target is a measurement (exit 0);
# only a polluted window exits non-zero.
RECORD="$(python3 - "$READY_HEALTHZ_MS" "$READY_MODELS_MS" "$IDLE_RSS_KB" \
  "$CLEAN" "$POLLUTION" "$COMMAND" "$GIT_HEAD" "$GIT_STATE" "$GO_VERSION" \
  "$PORT" "$IDLE_SECONDS" "$LOADAVG_START" "$MAX_LOAD" "$LOADAVG_END" <<'PYEOF'
import json, sys

ready_h, ready_m = float(sys.argv[1]), float(sys.argv[2])
rss_kib = int(sys.argv[3])
clean = sys.argv[4] == "true"
pollution = sys.argv[5] or None
(command, git_head, git_state, go_version) = sys.argv[6:10]
port, idle_seconds = int(sys.argv[10]), int(sys.argv[11])
load_start, load_max, load_end = sys.argv[12], sys.argv[13], sys.argv[14]

idle_mib = round(rss_kib / 1024, 1)
ready_worst = max(ready_h, ready_m)

def verdict(value, target, better):
    if not clean:
        return "POLLUTED"
    if better == "low":
        return "MET" if value < target else "MISSED"
    return "MET" if value > target else "MISSED"

record = {
    "script": "scripts/bench-idle-ready.sh",
    "command": command,
    "git_head": git_head,
    "git_state": git_state,
    "go_version": go_version,
    "port": port,
    "idle_seconds": idle_seconds,
    "config": "fresh HOME + init, zero providers, default settings (adaptive limiter on)",
    "clean": clean,
    "pollution": pollution,
    "loadavg_start": load_start.split()[:3],
    "loadavg_max_during_window": load_max.split()[:3] if load_max else None,
    "loadavg_end": load_end.split()[:3],
    "ready_ms": {"healthz": ready_h, "v1_models": ready_m},
    "idle_rss_kib": rss_kib,
    "idle_mib": idle_mib,
    "verdicts": {
        "ready_ms": {"overall": verdict(ready_worst, 10000, "low"), "target": "< 10000 ms (10 s)"},
        "idle_mib": {"overall": verdict(idle_mib, 70, "low"), "target": "< 70 MiB"},
    },
}
print(json.dumps(record, indent=2))
PYEOF
)"
echo "$RECORD"
if [ -n "$OUT" ]; then
  if [ "$CLEAN" = true ]; then DEST="$OUT"; else DEST="${OUT%.json}-polluted.json"; fi
  printf '%s\n' "$RECORD" > "$DEST"
  echo "bench-idle-ready.sh: record: $DEST" >&2
fi
{
  echo ""
  echo "bench-idle-ready.sh verdicts:"
  echo "  ready  < 10 s  : /healthz ${READY_HEALTHZ_MS} ms, /v1/models ${READY_MODELS_MS} ms"
  echo "  idle   < 70 MiB: ${IDLE_RSS_KB} KiB"
  echo "  loadavg start: $LOADAVG_START | max during window: $MAX_LOAD | end: $LOADAVG_END"
  if [ "$CLEAN" = true ]; then
    echo "  window: CLEAN"
  else
    echo "  window: POLLUTED — $POLLUTION"
    echo "  These numbers are evidence of a stormed window, never a quotable result."
  fi
} >&2
if [ "$CLEAN" = true ]; then exit 0; else exit 1; fi
