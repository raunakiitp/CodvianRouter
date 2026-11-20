#!/usr/bin/env bash
# bench-quiet.sh — run scripts/bench.sh only in a quiet window.
#
# The VM is shared and heavily stormed by other departments' test
# gates. A benchmark number taken while the VM is busy measures the
# storm, not the router, so this script is the ONLY sanctioned way
# to run a measurement:
#
#   1. Takes an exclusive lock (flock on .bench-lock in the repo
#      root) so only one measurement runs at a time across the
#      whole Benchmark Department. A second invocation fails fast
#      instead of queueing behind the first.
#   2. Loops (sleep 60 s, max 90 min) until the VM is quiet:
#        - 1-min loadavg < 3, AND
#        - no `go test` / `go build` in the process table, AND
#        - no full gate in flight (scripts/ci.sh), AND
#        - no other benchmark run in flight (bench.sh,
#          bench-geekflare.sh, codvian-bench latency/rps/mock).
#      Each wait prints the timestamp, the loadavg, and what is
#      busy, so a stuck wait is diagnosable, not silent.
#   3. Runs scripts/bench.sh with the given args, then prints the
#      loadavg again.
#
# The harness itself (cmd/codvian-bench) ALSO gates each
# measurement: if the VM gets busy between this script's check and
# a measurement, the measurement refuses loudly and the rig aborts
# instead of producing a polluted number. This script is the outer
# guard; the in-binary gate is the inner one.
#
# Usage:
#   ./scripts/bench-quiet.sh [--out PATH] [--limiter on|off]
#     All arguments are passed through to scripts/bench.sh.
#     ./scripts/bench-quiet.sh --help prints bench.sh usage at once.
# Exit: the exit code of scripts/bench.sh (0 = rig ran and summary
#   produced; a MISSED target is a measurement, not a failure, and
#   still exits 0 — see bench.sh). Exits 1 if no quiet window
#   appears within 90 minutes, or if another measurement holds
#   the lock. No measurement is taken in either case.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"
LOCK_FILE="$ROOT/.bench-lock"
MAX_WAIT_MIN=90
LOADAVG_MAX="3"

# --help passthrough: no lock, no waiting, just usage.
for a in "$@"; do
  case "$a" in
    -h|--help) exec "$ROOT/scripts/bench.sh" "$@" ;;
  esac
done

echo "bench-quiet.sh: command: $0 $*"
echo "bench-quiet.sh: git HEAD: $(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
echo "bench-quiet.sh: go: $(go version 2>/dev/null || echo unknown)"
echo "bench-quiet.sh: loadavg at start: $(cat /proc/loadavg)"

# --- Exclusive lock. Held (fd 9) for the whole script, including
# the bench.sh run, so a second measurement cannot start while
# this one waits or runs. Fail fast if another measurement holds
# it; the lock file itself is empty and untracked in the repo.
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "bench-quiet.sh: another measurement already holds $LOCK_FILE; refusing to run concurrently." >&2
  echo "bench-quiet.sh: if no measurement is actually running, remove the stale lock file and retry." >&2
  exit 1
fi
echo "bench-quiet.sh: lock acquired ($LOCK_FILE)"

# busy_processes prints one line per competing process (pid + full
# command). The bracket trick ([g]o, [c]i, ...) keeps grep from
# matching its own command line.
busy_processes() {
  ps -eo pid,args | awk -v self="$$" '$1 != self' \
    | grep -E '[g]o (test|build)( |$)|[c]i\.sh|[b]ench-geekflare\.sh|[b]ench\.sh|[c]odvian-bench (latency|rps|mock)' \
    || true
}

loadavg1() { awk '{print $1}' /proc/loadavg; }

is_quiet() {
  local la1
  la1="$(loadavg1)"
  awk -v la="$la1" -v max="$LOADAVG_MAX" 'BEGIN { if (la + 0 < max + 0) exit 0; exit 1 }' || return 1
  [ -z "$(busy_processes)" ] || return 1
  return 0
}

# --- Wait for a quiet window (max 90 minutes). ---
waited=0
while [ "$waited" -lt "$MAX_WAIT_MIN" ]; do
  if is_quiet; then
    echo "bench-quiet.sh: quiet window found after ${waited} min: loadavg $(cat /proc/loadavg)"
    break
  fi
  waited=$((waited + 1))
  if [ "$waited" -ge "$MAX_WAIT_MIN" ]; then
    echo "bench-quiet.sh: no quiet window in $MAX_WAIT_MIN min; aborting, no measurement taken." >&2
    echo "bench-quiet.sh: loadavg now: $(cat /proc/loadavg)" >&2
    busy_processes | head -10 | sed 's/^/bench-quiet.sh: busy: /' >&2 || true
    exit 1
  fi
  echo "[$(date -Iseconds)] bench-quiet.sh: waiting for quiet ($waited/$MAX_WAIT_MIN min): loadavg $(cat /proc/loadavg)"
  busy_processes | head -5 | sed 's/^/  busy: /' || true
  sleep 60
done

# --- Run the measurement, then report loadavg again. ---
# Release our lock first: bench.sh takes the same department lock
# for the run itself, and a held fd would make it refuse ("another
# benchmark holds the lock"). The wait-phase serialisation is done;
# a second wrapper racing in now either sees the rig's `go build`
# in its busy check and keeps waiting, or its bench.sh refuses on
# the lock — both fail-safe. (Fixed 2026-11-20 during Q1: the
# wrapper held the lock across the bench.sh call, so every run
# through bench-quiet.sh was refused.)
exec 9>&-
set +e
"$ROOT/scripts/bench.sh" "$@"
RC=$?
set -e
echo "bench-quiet.sh: loadavg at end: $(cat /proc/loadavg)"
echo "bench-quiet.sh: bench.sh exit code: $RC"
exit "$RC"
