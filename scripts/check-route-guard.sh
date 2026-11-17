#!/bin/sh
# Codvian Router — route-guard gate (CI bonus, mirrors the unit gate).
#
# Enumerates the server's route table and FAILS if any spawn-capable
# route is not classified LOCAL_ONLY. The real check is the unit test
# TestRouteTableSpawnCapableAreLocalOnly in internal/server/guard_test.go;
# this script is the CI entry point for it.
#
# Usage: sh scripts/check-route-guard.sh   (from the repo root)
set -eu
cd "$(dirname "$0")/.."
export PATH="$HOME/go/bin:$PATH"
go test -count=1 -run 'TestRouteTableSpawnCapableAreLocalOnly|TestRegisterPanicsOnUnknownRoute|TestIsLoopbackPeer|TestLocalOnlyBeforeAuth|TestLocalOnlyNotImplementedRoutes|TestAlwaysProtectedIgnoresRelaxed|TestBootstrapLoopbackOnly|TestGuardFeed' ./internal/server/
