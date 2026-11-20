#!/usr/bin/env bash
# status.sh — Master Prompt v2 §4.5/§9.1. Prints the numbers the reporter
# cron posts. Every number from a command run in this cycle.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2
echo "== PARITY =="
bash scripts/parity.sh || true
echo "== COMMITS =="
echo "HEAD: $(git log -1 --format='%h %ci %s')"
echo "commits last 20 min: $(git log --since='20 minutes ago' --oneline | wc -l)"
echo "dirty paths: $(git status --porcelain | wc -l)"
echo "== LAST GATE =="
[ -f docs/gate-last.txt ] && cat docs/gate-last.txt || echo "none recorded yet"
echo "== LOAD =="
echo "loadavg: $(cat /proc/loadavg)"
LAST=$(git log -1 --format=%ct); NOW=$(date +%s); echo "last commit age: $(( (NOW-LAST)/60 )) min"
echo "== HEARTBEAT (last 3) =="
[ -f docs/HEARTBEAT.log ] && tail -3 docs/HEARTBEAT.log || echo "no heartbeat lines yet"
