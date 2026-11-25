#!/usr/bin/env bash
# scripts/check-naming.sh
#
# Naming-scan gate: exits non-zero if any tracked file contains a prohibited
# competitor reference.
#
# The prohibited name MUST NOT appear as a contiguous string in this script
# (to avoid it being flagged by its own check). The patterns are assembled
# from per-character classes so the literal name never exists in source.
#
# Covered:
#   - Competitor product name: o[m][n][i]-r[o][u][t][e] (and hyphen/spaced/underscore variants)
#   - Competitor product name: c[h][e][a][p][e][r]-i[n][f][e][r][e][n][c][e]
#   - Competitor GitHub org (if different from this project's org)
#
# Also flags generic cross-product-tracking markers ONLY when they cannot
# false-positive on legitimate Codvian prose (checked against repo first).

set -euo pipefail

# Build the prohibited patterns from character classes (literal name never appears)
# Competitor product: omniroute (and variants: omni-route, omni_route, omni route)
p1='o[m][n][i][- _]?r[o][u][t][e]'
# Competitor product: cheaper-inference (and variants)
p2='c[h][e][a][p][e][r][- _]?i[n][f][e][r][e][n][c][e]'

# Combined regex for prohibited competitor names
# NOTE: This project's own GitHub org (raunakiitp) is NOT prohibited
PROHIBITED_REGEX="(${p1}|${p2})"

# Cross-product tracking markers that the cleanup removes.
# These are ONLY flagged if they appear in contexts that are clearly
# cross-product comparison (e.g., "126 classified feature entries",
# "parity inventory", "feature parity" as a section title).
# We restrict to lines that also contain classification totals or
# explicit comparison language to avoid false positives on legitimate
# Codvian prose like "parity of this implementation" or "feature parity check".
# NOTE: "parity tracker" is a legitimate internal tool name in this repo;
# only flag it when paired with cross-product comparison language.
PARITY_MARKERS_REGEX='(126 classified feature|parity inventory|feature[- ]parity report|feature-parity evaluation|corrected parity inventory|feature parity (matrix|table|classification))'

# Files to EXCLUDE from scanning (legitimate references that must stay):
# - go.mod (module path)
# - Provider catalog entries (real integrated upstream providers)
# - GitHub workflows / release configs (this project's own repo references)
# - Test fixtures referencing this project's module path
# - Coordinator-owned docs (they reconcile these last)
EXCLUDE_PATTERNS=(
    "go\.mod"
    "go\.sum"
    "internal/catalog/providers_reference\.go"
    "internal/catalog/verified\.go"
    "internal/catalog/providers_additional\.go"
    "docs/catalog-audit-"
    ".github/"
    ".goreleaser\.yml"
    "bin/codvian-router"
    "cmd/codvian-router/release_verify\.go"
    "internal/release/verify\.go"
    "docs/POST_P1_FINDINGS_LEDGER\.md"
    "docs/POST_P1_REMEDIATION_REPORT\.md"
)

# Search all tracked files (respects .gitignore and our excludes)
FILES=$(git ls-files | grep -v -E "$(IFS='|'; echo "${EXCLUDE_PATTERNS[*]}")" || true)

# Track matches
FOUND=0

echo "=== Scanning for prohibited competitor references ==="

# Check prohibited names
while IFS= read -r file; do
    [ -z "$file" ] && continue
    # Skip binary files
    if file "$file" 2>/dev/null | grep -q "binary"; then
        continue
    fi
    # Check for prohibited competitor names
    if grep -Eiq "$PROHIBITED_REGEX" "$file" 2>/dev/null; then
        echo "PROHIBITED: $file"
        grep -Eni "$PROHIBITED_REGEX" "$file" | head -5
        FOUND=1
    fi
    # Check for cross-product tracking markers (restricted context)
    if grep -Eiq "$PARITY_MARKERS_REGEX" "$file" 2>/dev/null; then
        # Verify it's not a false positive by checking context
        # Only flag if the line contains classification totals or explicit comparison
        if grep -Ei "$PARITY_MARKERS_REGEX" "$file" | grep -Eiq '(classified|entries|total|count|inventory|evaluation|report)'; then
            echo "PARITY MARKER: $file"
            grep -Eni "$PARITY_MARKERS_REGEX" "$file" | head -5
            FOUND=1
        fi
    fi
done <<< "$FILES"

if [ $FOUND -eq 1 ]; then
    echo ""
    echo "FAIL: Prohibited references found in tracked files."
    exit 1
else
    echo "PASS: No prohibited references found."
    exit 0
fi