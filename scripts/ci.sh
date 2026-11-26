#!/usr/bin/env bash
# ci.sh — the CI gate for Codvian Router (Master Prompt v1, M0.3).
#
# Steps (each reported PASS/FAIL; all steps run even if one fails,
# so a single invocation shows every failure, not just the first):
#   1. gofmt  — no unformatted Go files under cmd/ and internal/
#   2. go build ./...
#   3. go vet ./...
#   4. go test ./... -race -count=1
#   5. docker go-version match — both Dockerfiles' golang:<ver>-bookworm
#      builder must equal the exact Go patch .github/workflows/release.yml
#      pins (PKG-6); a drift means the local image and the release image
#      are built by different toolchains.
#
# Optional benchmark smoke (NOT run by default — the full system
# benchmark is scripts/bench.sh, which needs a quiet machine):
#   CI_BENCH_SMOKE=1 ./scripts/ci.sh
# runs `go test -bench=. -benchtime=1x -run='^$' ./...` (one
# iteration per benchmark, no unit tests) as an extra step.
#
# Usage: ./scripts/ci.sh        (run from anywhere; it cds to the repo root)
# Exit:  0 when every step passes, 1 otherwise. Final line is
#        "CI: PASS" or "CI: FAIL (N step(s) failed)".
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
export PATH="$HOME/go/bin:$PATH"

FAILURES=0

# run_step <name> <command...>: run one gate step, print PASS/FAIL.
run_step() {
  local name="$1"
  shift
  echo "== $name =="
  if "$@"; then
    echo "PASS: $name"
  else
    local rc=$?
    echo "FAIL: $name (exit $rc)"
    FAILURES=$((FAILURES + 1))
  fi
  echo ""
}

# --- 1. gofmt: list unformatted files; any output means FAIL. ---
echo "== gofmt =="
UNFORMATTED="$(gofmt -l cmd internal)"
if [ -z "$UNFORMATTED" ]; then
  echo "PASS: gofmt"
else
  echo "unformatted files:"
  echo "$UNFORMATTED"
  echo "FAIL: gofmt (run: gofmt -w <file>)"
  FAILURES=$((FAILURES + 1))
fi
echo ""

# --- 2-4. build, vet, test. ---
run_step "go build ./..." go build ./...
run_step "scripts/check-secrets.sh" bash scripts/check-secrets.sh
run_step "go vet ./..." go vet ./...
# AUD-TEST-008 / P2-TEST-06: the spawn-capable route classification gate
# existed as a script but no CI job ran it. It is a real gate (the table
# must classify every spawn-capable route LOCAL_ONLY), so CI runs it.
run_step "scripts/check-route-guard.sh" sh scripts/check-route-guard.sh

# --- otel build tag (AUD-TEST-002): the tagged sources only compile with
# -tags otel, and nothing in CI ever built them.
run_step "go build -tags otel ./..." go build -tags otel ./...
run_step "go test -tags otel ./internal/observability/" go test -tags otel -count=1 ./internal/observability/

# --- 5. docker go-version match (PKG-6). ---
# The Dockerfiles build with golang:<ver>-bookworm; release.yml pins an
# exact Go patch (go-version). Fail closed when the pin cannot be read.
echo "== docker go-version match =="
PINNED_GO="$(sed -n 's/^[[:space:]]*go-version:[[:space:]]*"\([^"]*\)".*/\1/p' .github/workflows/release.yml | head -n 1)"
DOCKER_GO_OK=1
if [ -z "$PINNED_GO" ]; then
  echo "FAIL: no go-version pin found in .github/workflows/release.yml"
  DOCKER_GO_OK=0
fi
for DF in Dockerfile Dockerfile.release; do
  TAG="$(grep -o 'golang:[0-9][0-9.]*-bookworm' "$DF" | head -n 1 | sed 's/^golang://; s/-bookworm$//')"
  if [ "$TAG" != "$PINNED_GO" ]; then
    echo "FAIL: $DF builds with golang:$TAG-bookworm, release.yml pins $PINNED_GO"
    DOCKER_GO_OK=0
  fi
done
if [ "$DOCKER_GO_OK" -eq 1 ]; then
  echo "PASS: docker go-version match (golang:$PINNED_GO-bookworm in both Dockerfiles)"
else
  echo "FAIL: docker go-version match"
  FAILURES=$((FAILURES + 1))
fi
echo ""

# --- P2-DEPLOY-02: Docker FROM digest pinning check ---
# Reject any FROM line without a @sha256: digest so container images
# are reproducible and verifiable. Keep the tag as a comment for readability.
echo "== docker FROM digest pinning =="
DOCKER_FROM_OK=1
while IFS= read -r df; do
  if [ -f "$df" ]; then
    while IFS= read -r line; do
      case "$line" in
        FROM*)
          case "$line" in
            *@sha256:*) ;;
            *)
              echo "FAIL: $df: unpinned FROM: $line"
              DOCKER_FROM_OK=0
              ;;
          esac
          ;;
      esac
    done < "$df"
  fi
done < <(printf '%s\n' "Dockerfile" "Dockerfile.release")
if [ "$DOCKER_FROM_OK" -eq 1 ]; then
  echo "PASS: docker FROM digest pinning (both Dockerfiles use @sha256: digests)"
else
  echo "FAIL: docker FROM digest pinning"
  FAILURES=$((FAILURES + 1))
fi
echo ""

# --- Benchmark smoke (one iteration each, no unit tests).
# P2-TEST-01: benchmarks must execute in CI, not merely compile.
run_step "go test -bench=. -benchtime=1x -run=^\\$ ./... (bench-smoke)" \
  go test -bench=. -benchtime=1x -run='^$' ./...

# go test writes its own per-package output; the PASS/FAIL line
# above is the gate verdict for the whole suite.
run_step "go test ./... -race -count=1" go test ./... -race -count=1

if [ "$FAILURES" -eq 0 ]; then
  echo "CI: PASS"
  exit 0
else
  echo "CI: FAIL ($FAILURES step(s) failed)"
  exit 1
fi
