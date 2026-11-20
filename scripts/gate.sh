#!/usr/bin/env bash
# gate.sh — Master Prompt v2 §5.1. Full gate, run by root with the fleet idle.
# flock /tmp/codvian.gate.lock; nice -n 10, GOMAXPROCS=2; go build/vet/test
# -p 1 ./... -count=1, NO -race by default. --race-hot runs -race only for
# router/server/cache/ledger/config. Prints PASS/FAIL/SKIP counts from
# go test -json and writes docs/gate-last.txt. Evidence is per-mode:
# full -> /tmp/codvian.gate.full.json + docs/gate-last-full.txt,
# --race-hot -> /tmp/codvian.gate.race.json + docs/gate-last-race.txt
# (one shared file let a race-hot run destroy the full-gate evidence and
# caused a wrong RED verdict to be read ~03:00 IST 2026-11-20).
# Max 12 min (timeout enforced by caller).
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"
export TMPDIR="$HOME/.tmp"
export GOMAXPROCS=2
mkdir -p "$TMPDIR"
exec 9>/tmp/codvian.gate.lock
flock -w 120 9 || { echo "GATE: lock busy after 120s — another gate/pkgtest is running"; exit 2; }
cd "$ROOT" || exit 2
MODE="${1:-}"
if [ "$MODE" = "--race-hot" ]; then MODE_NAME="race-hot"; FILEMODE="race"; else MODE_NAME="full"; FILEMODE="full"; fi
JSONFILE="/tmp/codvian.gate.${FILEMODE}.json"
ERRFILE="/tmp/codvian.gate.${FILEMODE}.err"
echo "GATE start $(date -u +%FT%TZ) HEAD=$(git rev-parse --short HEAD) mode=${MODE_NAME} json=${JSONFILE}"
nice -n 10 go build ./... || { echo "GATE: BUILD FAIL"; echo "BUILD FAIL $(date -u +%FT%TZ)" > docs/gate-last.txt; exit 1; }
nice -n 10 go vet ./... || { echo "GATE: VET FAIL"; echo "VET FAIL $(date -u +%FT%TZ)" > docs/gate-last.txt; exit 1; }
if [ "$MODE" = "--race-hot" ]; then
  PKGS="./internal/router/ ./internal/server/ ./internal/ledger/ ./internal/config/"
  [ -d internal/cache ] && PKGS="$PKGS ./internal/cache/"
  # shellcheck disable=SC2086
  nice -n 10 go test -race -count=1 -json $PKGS > "$JSONFILE" 2>"$ERRFILE"
else
  nice -n 10 go test -p 1 -count=1 -json ./... > "$JSONFILE" 2>"$ERRFILE"
fi
python3 - "$JSONFILE" <<'PYEOF'
import json, sys
tp = tf = ts = 0
pp = pf = 0
fails = []
for line in open(sys.argv[1], errors='replace'):
    try: ev = json.loads(line)
    except Exception: continue
    act = ev.get('Action'); test = ev.get('Test'); pkg = ev.get('Package','')
    if test:
        if act == 'pass': tp += 1
        elif act == 'fail': tf += 1; fails.append(f"{pkg}::{test}")
        elif act == 'skip': ts += 1
    elif act == 'pass' and pkg: pp += 1
    elif act == 'fail' and pkg: pf += 1
print(f"GATE tests: PASS={tp} FAIL={tf} SKIP={ts} | packages: PASS={pp} FAIL={pf}")
for f in fails[:20]: print("  FAIL:", f)
sys.exit(1 if tf or pf else 0)
PYEOF
RC=$?
if [ $RC -eq 0 ]; then echo "GATE: GREEN"; else echo "GATE: RED (see ${JSONFILE}, stderr ${ERRFILE})"; fi
python3 - "$JSONFILE" "$MODE_NAME" <<'PYEOF' > docs/gate-last.txt
import json, sys
tp = tf = ts = 0; pp = pf = 0
for line in open(sys.argv[1], errors='replace'):
    try: ev = json.loads(line)
    except Exception: continue
    act = ev.get('Action'); test = ev.get('Test'); pkg = ev.get('Package','')
    if test:
        if act == 'pass': tp += 1
        elif act == 'fail': tf += 1
        elif act == 'skip': ts += 1
    elif act == 'pass' and pkg: pp += 1
    elif act == 'fail' and pkg: pf += 1
print(f"GATE {__import__('datetime').datetime.now(__import__('datetime').timezone.utc).strftime('%Y-%m-%dT%H:%MZ')} mode={sys.argv[2]} tests PASS={tp} FAIL={tf} SKIP={ts} packages PASS={pp} FAIL={pf}")
PYEOF
cp docs/gate-last.txt "docs/gate-last-${FILEMODE}.txt"
exit $RC
