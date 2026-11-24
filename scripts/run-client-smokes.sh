#!/usr/bin/env bash
# run-client-smokes.sh — run the five M8.1 client-protocol smoke
# drivers (AUD-TEST-001) against a router the drivers boot themselves.
#
# Each of scripts/smoke-client-{claude-code,codex,cursor,cline,aider}.py
# builds the real router from the worktree, boots it against a localhost
# mock upstream, runs its protocol checks and exits non-zero on any FAIL
# (its final line is "N/N checks passed"). The drivers use fixed ports
# (router 21601..21641, mock +1), so they MUST run sequentially — never
# in parallel, or they collide.
#
# Requires: a Go toolchain on PATH (or GO_BIN) and python3 (or
# PYTHON_BIN). No network and no provider credential is used: every
# upstream is a localhost mock. Worktree override: CODVIA_ROUTER_WORKTREE.
#
# Skip policy (AUD-TEST-001): a driver is NEVER silently skipped. If a
# required interpreter is missing the whole run fails loudly, so a green
# run always means all five drivers actually executed and passed.
#
# Usage: bash scripts/run-client-smokes.sh   (run from anywhere)
# Exit:  0 when all five drivers report every check passed, 1 otherwise.
#        Final line is "client-smoke: PASS (5/5 drivers)" or
#        "client-smoke: FAIL (N driver(s) failed)".
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PY="${PYTHON_BIN:-python3}"
if ! command -v "$PY" >/dev/null 2>&1; then
  echo "FATAL: python3 not found; set PYTHON_BIN to a Python 3 interpreter" >&2
  exit 2
fi

DRIVERS=(
  smoke-client-claude-code.py
  smoke-client-codex.py
  smoke-client-cursor.py
  smoke-client-cline.py
  smoke-client-aider.py
)

FAILURES=0
for d in "${DRIVERS[@]}"; do
  echo "== $d =="
  if "$PY" "scripts/$d"; then
    echo "PASS: $d"
  else
    rc=$?
    echo "FAIL: $d (exit $rc)"
    FAILURES=$((FAILURES + 1))
  fi
  echo ""
done

if [ "$FAILURES" -eq 0 ]; then
  echo "client-smoke: PASS (5/5 drivers)"
  exit 0
else
  echo "client-smoke: FAIL ($FAILURES driver(s) failed)"
  exit 1
fi
