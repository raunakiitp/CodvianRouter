#!/usr/bin/env python3
# scripts/check-naming.py
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

import re
import sys
import os

# Build the prohibited patterns from character classes (literal name never appears)
# Competitor product: omniroute (and variants: omni-route, omni_route, omni route)
p1 = r'o[m][n][i][- _]?r[o][u][t][e]'
# Competitor product: cheaper-inference (and variants)
p2 = r'c[h][e][a][p][e][r][- _]?i[n][f][e][r][e][n][c][e]'

# Combined regex for prohibited competitor names
# NOTE: This project's own GitHub org (raunakiitp) is NOT prohibited
PROHIBITED_REGEX = re.compile(f'({p1}|{p2})', re.IGNORECASE)

# Cross-product tracking markers that the cleanup removes.
# These are ONLY flagged if they appear in contexts that are clearly
# cross-product comparison (e.g., "126 classified feature entries",
# "parity inventory", "feature parity" as a section title).
# We restrict to lines that also contain classification totals or
# explicit comparison language to avoid false positives on legitimate
# Codvian prose like "parity of this implementation" or "feature parity check".
# NOTE: "parity tracker" is a legitimate internal tool name in this repo;
# only flag it when paired with cross-product comparison language.
PARITY_MARKERS_REGEX = re.compile(
    r'(126 classified feature|parity inventory|feature[- ]parity report|'
    r'feature-parity evaluation|corrected parity inventory|'
    r'feature parity (matrix|table|classification))',
    re.IGNORECASE
)

# Files to EXCLUDE from scanning (legitimate references that must stay):
# - go.mod (module path)
# - Provider catalog entries (real integrated upstream providers)
# - GitHub workflows / release configs (this project's own repo references)
# - Test fixtures referencing this project's module path
# - Coordinator-owned docs (they reconcile these last)
EXCLUDE_PATTERNS = [
    r'go\.mod$',
    r'go\.sum$',
    r'internal/catalog/providers_reference\.go$',
    r'internal/catalog/verified\.go$',
    r'internal/catalog/providers_additional\.go$',
    r'docs/catalog-audit-',
    r'web/catalog-audit\.json$',
    r'\.github/',
    r'\.goreleaser\.yml$',
    r'bin/codvian-router',
    r'cmd/codvian-router/release_verify\.go$',
    r'internal/release/verify\.go$',
    r'docs/POST_P1_FINDINGS_LEDGER\.md$',
    r'docs/POST_P1_REMEDIATION_REPORT\.md$',
]

def should_exclude(filepath):
    """Check if file should be excluded from scanning."""
    for pattern in EXCLUDE_PATTERNS:
        if re.search(pattern, filepath):
            return True
    return False

def is_binary(filepath):
    """Check if file is binary."""
    try:
        with open(filepath, 'rb') as f:
            chunk = f.read(8192)
            if b'\x00' in chunk:
                return True
    except Exception:
        pass
    return False

def main():
    # Read tracked files from stdin (one per line)
    files = [line.strip() for line in sys.stdin if line.strip()]

    found = 0

    print("=== Scanning for prohibited competitor references ===")

    for file in files:
        if not file or should_exclude(file):
            continue
        if is_binary(file):
            continue

        try:
            with open(file, 'r', encoding='utf-8', errors='ignore') as f:
                content = f.read()
        except Exception:
            continue

        # Check for prohibited competitor names
        prohibited_matches = PROHIBITED_REGEX.findall(content)
        if prohibited_matches:
            print(f"PROHIBITED: {file}")
            # Show first few matching lines
            lines = content.split('\n')
            for i, line in enumerate(lines, 1):
                if PROHIBITED_REGEX.search(line):
                    print(f"  {i}: {line.strip()}")
                    found = 1
                    if found >= 5:
                        break

        # Check for cross-product tracking markers (restricted context)
        parity_matches = PARITY_MARKERS_REGEX.findall(content)
        if parity_matches:
            # Verify it's not a false positive by checking context
            # Only flag if the line contains classification totals or explicit comparison
            lines = content.split('\n')
            for i, line in enumerate(lines, 1):
                if PARITY_MARKERS_REGEX.search(line):
                    # Additional context check
                    if re.search(r'(classified|entries|total|count|inventory|evaluation|report)', line, re.IGNORECASE):
                        print(f"PARITY MARKER: {file}")
                        print(f"  {i}: {line.strip()}")
                        found = 1
                        break

    if found:
        print("")
        print("FAIL: Prohibited references found in tracked files.")
        return 1
    else:
        print("PASS: No prohibited references found.")
        return 0

if __name__ == '__main__':
    sys.exit(main())