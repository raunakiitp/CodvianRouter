#!/usr/bin/env bash
# commit.sh <task-id> "<msg>" <path>... — Master Prompt v2 §5.1.
# flock /tmp/codvian.git.lock, explicit paths only (NEVER git add -A),
# gofmt check on changed Go files, message "<lane>(<task-id>): <msg>".
# Lane from $LANE (default: v2).
# Pathspec guard (2026-11-20, B11 report): bare `git commit` commits EVERYTHING
# already staged — a watchdog wip sweep or another agent's `git add` could be
# swallowed into this commit (happened to B11-G5: 22 files in 6ccd7a2). So the
# commit itself names the paths, and any other staged file is only reported.
set -u
[ $# -ge 3 ] || { echo "usage: commit.sh <task-id> \"<msg>\" <path>..."; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TASK="$1"; MSG="$2"; shift 2
LANE="${LANE:-v2}"
exec 9>/tmp/codvian.git.lock
flock -w 60 9 || { echo "COMMIT: git lock busy after 60s"; exit 2; }
cd "$ROOT" || exit 2
for p in "$@"; do [ -e "$p" ] || { echo "COMMIT: path does not exist: $p"; exit 2; }; done
UNFMT=$(git diff --name-only -- "$@" 2>/dev/null | grep '\.go$' | xargs -r gofmt -l 2>/dev/null; git ls-files --others --exclude-standard -- "$@" | grep '\.go$' | xargs -r gofmt -l 2>/dev/null)
if [ -n "$UNFMT" ]; then echo "COMMIT: unformatted Go files:"; echo "$UNFMT"; exit 1; fi
git add -- "$@" || exit 1
STAGED_OTHER=$(comm -23 <(git diff --cached --name-only | sort) <(printf '%s\n' "$@" | sort) || true)
[ -n "$STAGED_OTHER" ] && { echo "COMMIT: note: other staged files left uncommitted:"; echo "$STAGED_OTHER"; }
git commit -m "${LANE}(${TASK}): ${MSG}" -- "$@" || { echo "COMMIT: nothing to commit or commit failed"; exit 1; }
git rev-parse --short HEAD
