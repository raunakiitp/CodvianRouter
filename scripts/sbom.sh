#!/usr/bin/env bash
# sbom.sh - thin wrapper over scripts/sbom.py (G-06).
# Generate a CycloneDX-1.4 SBOM of the Go module graph (direct +
# transitive modules from `go list -m -json all`).
#
# Usage:
#   ./scripts/sbom.sh [--out PATH] [--self-check]
#     --out PATH     write the CycloneDX JSON here (default: stdout)
#     --self-check   validate generation + JSON shape for CI; exits
#                    non-zero on malformed output or zero components
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 "$ROOT/scripts/sbom.py" "$@"
