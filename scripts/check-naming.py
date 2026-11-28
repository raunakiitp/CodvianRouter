#!/usr/bin/env python3
# scripts/check-naming.py
#
# Naming-scan gate. Exits non-zero if ANY tracked file (minus the disclosed
# EXCLUDE list) contains a prohibited competitor reference. The scan is
# whole-tree: the repository must be free of these strings everywhere --
# code, comments, docs, README, workflows.
#
# Prohibited names are assembled from per-character fragments, so the patterns
# exist only as regex classes here and the gate does not match its own source.
# This file and the Go mirror (internal/catalog/repo_brand_gate_test.go) are the
# only files the scan excludes -- a structural exclusion: they must carry the
# pattern tables. Everything else in the tree is scanned. See
# docs/naming-gate.md for the canonical enforcement and the sanctioned
# lowercase retired-path exception.
#
# Covered:
#   - reference router / reference-router / reference_router (prose + path)
#   - System One / SystemOne (capitalized-only: the brand is a proper noun,
#     so lowercase prose like "non-system one" or a removed /v1/systemone
#     path must not false-positive)
#   - the reference codebase's path prefix (open-sse)
#   - OmniGlyph, omni-skills, OMNI_ env vars
#   - the earlier router product name (omni-route variants)
#   - the retired cheaper-inference service name

import re
import sys


def _frag(*parts):
    return "".join(parts)


# --- case-insensitive classes (fragmented so this file does not self-flag) ---
P_REFROUTER = _frag(r"re", r"ference[ -_]?r", r"outer")
P_OPENSSE = _frag(r"o", r"pen[-_]s", r"se")          # literal hyphen/underscore only
P_GLYPH = _frag(r"o", r"mni ?g", r"lyph")
P_SKILLS = _frag(r"o", r"mni[-_ ]?s", r"kills")
P_OMNIVAR = _frag(r"O", r"M", r"N", r"I", r"_")
P_OMNIROUTE = _frag(r"o", r"mni[-_ ]?r", r"oute")
P_CHEAPER = _frag(r"ch", r"eaper[-_ ]?i", r"nference")

CI_REGEX = re.compile(
    "(" + "|".join([P_REFROUTER, P_OPENSSE, P_GLYPH, P_SKILLS,
                    P_OMNIVAR, P_OMNIROUTE, P_CHEAPER]) + ")",
    re.IGNORECASE,
)

# --- capitalized-only class: System One / SystemOne ---
P_SYS_SPACED = _frag(r"S", r"ystem ", r"O", r"ne")
P_SYS_CONTIG = _frag(r"S", r"ystem", r"O", r"ne")
SYS_REGEX = re.compile("(" + P_SYS_SPACED + "|" + P_SYS_CONTIG + ")")

EXCLUDE_PATTERNS = [
    r"go\.mod$", r"go\.sum$",
    r"internal/catalog/providers_reference\.go$",
    r"internal/catalog/verified\.go$",
    r"internal/catalog/providers_additional\.go$",
    r"docs/catalog-audit-", r"web/catalog-audit\.json$",
    r"\.github/", r"\.goreleaser\.yml$",
    r"bin/codvian-router$",
    r"cmd/codvian-router/release_verify\.go$",
    r"internal/release/verify\.go$",
    r"docs/POST_P1_FINDINGS_LEDGER\.md$",
    r"docs/POST_P1_REMEDIATION_REPORT\.md$",
    # The gate scripts and their Go mirror necessarily encode the banned
    # names in their pattern tables (a disclosed, documented exception).
    r"scripts/check-naming\.(py|sh)$",
    r"internal/catalog/repo_brand_gate_test\.go$",
]


def should_exclude(path):
    return any(re.search(p, path) for p in EXCLUDE_PATTERNS)


def is_binary(path):
    try:
        with open(path, "rb") as f:
            return b"\x00" in f.read(8192)
    except Exception:
        return True


def scan(files):
    found = 0
    print("=== Whole-tree scan for prohibited brand references ===")
    for path in files:
        if not path or should_exclude(path) or is_binary(path):
            continue
        try:
            with open(path, "r", encoding="utf-8", errors="ignore") as f:
                content = f.read()
        except Exception:
            continue
        hits = 0
        for i, line in enumerate(content.split("\n"), 1):
            if CI_REGEX.search(line) or SYS_REGEX.search(line):
                if hits == 0:
                    print("PROHIBITED: " + path)
                print("  " + str(i) + ": " + line.strip()[:200])
                hits += 1
                if hits >= 5:
                    break
        if hits:
            found = 1
    return found


def main():
    files = [ln.strip() for ln in sys.stdin if ln.strip()]
    if not files:
        print("FAIL: no file list on stdin (usage: git ls-files | check-naming.py)")
        return 2
    if scan(files):
        print("")
        print("FAIL: prohibited references found.")
        return 1
    print("PASS: no prohibited references in " + str(len(files)) + " tracked files.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
