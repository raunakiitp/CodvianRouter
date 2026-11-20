#!/usr/bin/env bash
# watchdog.sh — Master Prompt v2 §4.5/§6. Prints loadavg, minutes since last
# commit, dirty path count, top-3 CPU processes. Appends one line to
# docs/HEARTBEAT.log. Runs in <2 min. (Live agent list comes from the
# watchdog cron worker via the agent-list tool, not from bash.)
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2
LOAD=$(cut -d' ' -f1-3 /proc/loadavg)
LAST=$(git log -1 --format=%ct 2>/dev/null || echo 0)
NOW=$(date +%s)
AGE_MIN=$(( (NOW - LAST) / 60 ))
DIRTY=$(git status --porcelain 2>/dev/null | wc -l)
HEAD=$(git rev-parse --short HEAD 2>/dev/null || echo "?")
TOP=$(ps -eo comm,%cpu --sort=-%cpu | head -4 | tail -3 | tr '\n' ';' | sed 's/  */ /g')
LINE="$(date -u +%FT%TZ) load=$LOAD last_commit=${AGE_MIN}m_ago HEAD=$HEAD dirty=$DIRTY top_cpu=[$TOP]"
echo "$LINE"
echo "$LINE" >> docs/HEARTBEAT.log
