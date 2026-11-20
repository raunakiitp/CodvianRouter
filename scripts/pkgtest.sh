#!/usr/bin/env bash
# pkgtest.sh <pkg> — Master Prompt v2 §5.1. The ONLY test builders run.
# Same flock as gate.sh, one package, NO -race. Usage: pkgtest.sh internal/router
set -u
[ $# -ge 1 ] || { echo "usage: pkgtest.sh <pkg-path e.g. internal/router>"; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"
export TMPDIR="$HOME/.tmp"
export GOMAXPROCS=2
mkdir -p "$TMPDIR"
exec 9>/tmp/codvian.gate.lock
flock -w 120 9 || { echo "PKGTEST: lock busy after 120s"; exit 2; }
cd "$ROOT" || exit 2
PKG="$1"; shift
case "$PKG" in ./*) ;; *) PKG="./$PKG";; esac
echo "PKGTEST $PKG $(date -u +%FT%TZ)"
nice -n 10 go test -count=1 "$@" "$PKG"
