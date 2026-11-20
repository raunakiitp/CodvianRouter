#!/usr/bin/env bash
# wip.sh <task-id> "<msg>" <path>... — Master Prompt v2 §5.1/§6.
# Harvest-before-kill: same as commit.sh but message "wip(<task-id>): <msg>".
# Pathspec guard (2026-11-20, same as commit.sh): bare `git commit` would
# swallow anything already staged; the commit names the paths.
set -u
[ $# -ge 3 ] || { echo "usage: wip.sh <task-id> \"<msg>\" <path>..."; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TASK="$1"; MSG="$2"; shift 2
exec 9>/tmp/codvian.git.lock
flock -w 60 9 || { echo "WIP: git lock busy after 60s"; exit 2; }
cd "$ROOT" || exit 2
for p in "$@"; do [ -e "$p" ] || { echo "WIP: path does not exist: $p"; exit 2; }; done
git add -- "$@" || exit 1
git commit -m "wip(${TASK}): ${MSG}" -- "$@" || { echo "WIP: nothing to commit or commit failed"; exit 1; }
git rev-parse --short HEAD
