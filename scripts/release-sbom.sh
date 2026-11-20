#!/bin/sh
# release-sbom.sh — generate a CycloneDX SBOM for the current tree.
#
# Dependency-free local path: runs `go run ./cmd/release-sbom`, which
# renders `go list -m -json all` as CycloneDX 1.6 JSON. The release
# pipeline in CI prefers syft (via GoReleaser, richer: it also catalogs
# files inside the archives); use this script when syft is unavailable,
# or to inspect what the SBOM will contain before a release.
#
# Usage:
#   sh scripts/release-sbom.sh [VERSION] [OUT]
#     VERSION defaults to 0.0.0-dev (an unreleased tree has no version);
#     OUT defaults to sbom.cyclonedx.json in the current directory.
#
# Requires: the Go toolchain on PATH, run from anywhere inside the repo.
set -eu

VERSION="${1:-0.0.0-dev}"
OUT="${2:-sbom.cyclonedx.json}"

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

exec go run ./cmd/release-sbom --version "$VERSION" --out "$OUT"
