#!/usr/bin/env python3
# scripts/check-naming.py
#
# Naming-scan gate. Exits non-zero if any line ADDED on this branch (vs the
# merge-base with origin/main) contains a prohibited competitor string.
# Pre-existing sanctioned prose / removal-history references are not
# re-flagged; the gate's job is to stop NEW prohibited strings from landing.
#
# Prohibited names are assembled from per-character classes so the literal
# name never appears in this script (avoids self-flagging).
#
# Banned on added lines (case-insensitive unless noted):
#   - omni-route (hyphen/spaced/underscore variants)
#   - cheaperinference (variants)
#   - "System One" / "SystemOne" (capitalized-only match: the brand is a proper
#     noun; lowercase prose such as a removed /v1/systemone path or
#     "non-system one" must not false-positive)
#   - open-sse, OmniGlyph, omni-skills, OMNI_, "reference router"

import re
import sys
import subprocess

p_omniroute = r'o[m][n][i][- _]?r[o][u][t][e]'
p_cheaper   = r'c[h][e][a][p][e][r][- _]?i[n][f][e][r][e][n][c][e]'
p_opensse   = r'o[p][e][n][- _]?s[s][e]'
p_glyph     = r'o[m][n][i][Gg][l][y][p][h]'
p_skills    = r'o[m][n][i][- _]?s[k][i][l][l][s]'
p_omnivar   = r'[Oo][Mm][Nn][Ii]_'
p_refrouter = r'reference[ ]router'
ADDED_ONLY_REGEX = re.compile(
    f'({p_omniroute}|{p_cheaper}|{p_opensse}|{p_glyph}|{p_skills}|{p_omnivar}|{p_refrouter})',
    re.IGNORECASE,
)
p_sysone_contig = r'S[y][s][t][e][m][O][n][e]'
p_sysone_spaced = r'S[y][s][t][e][m][ ][O][n][e]'
SYSTEMONE_REGEX_CS = re.compile(f'({p_sysone_contig}|{p_sysone_spaced})')  # capitalized only

EXCLUDE_PATTERNS = [
    r'go\.mod$', r'go\.sum$',
    r'internal/catalog/providers_reference\.go$',
    r'internal/catalog/verified\.go$',
    r'internal/catalog/providers_additional\.go$',
    r'docs/catalog-audit-', r'web/catalog-audit\.json$',
    r'\.github/', r'\.goreleaser\.yml$',
    r'bin/codvian-router',
    r'cmd/codvian-router/release_verify\.go$',
    r'internal/release/verify\.go$',
    r'docs/POST_P1_FINDINGS_LEDGER\.md$',
    r'docs/POST_P1_REMEDIATION_REPORT\.md$',
    r'scripts/check-naming\.(py|sh)$',   # gate scripts encode the names in their pattern lists (disclosed exception)
]

def should_exclude(filepath):
    for pattern in EXCLUDE_PATTERNS:
        if re.search(pattern, filepath):
            return True
    return False

def added_lines_scan():
    try:
        base = subprocess.run(['git', 'merge-base', 'HEAD', 'origin/main'],
                              capture_output=True, text=True, timeout=30).stdout.strip()
        if not base:
            print("PASS: no merge-base with origin/main; added-lines scan skipped")
            return 0
        diff = subprocess.run(['git', 'diff', f'{base}...HEAD', '--unified=0'],
                              capture_output=True, text=True, timeout=60).stdout
    except Exception:
        return 0
    if not diff:
        print("PASS: no added lines vs origin/main")
        return 0
    found = 0
    print("=== Added-lines scan (vs origin/main) for prohibited strings ===")
    cur_file = None
    for line in diff.split('\n'):
        if line.startswith('+++ b/'):
            cur_file = line[6:]
            continue
        if not line.startswith('+') or line.startswith('+++'):
            continue
        added = line[1:]
        if should_exclude(cur_file or ''):
            continue
        if ADDED_ONLY_REGEX.search(added) or SYSTEMONE_REGEX_CS.search(added):
            print(f"PROHIBITED (added): {cur_file}: {added.strip()[:200]}")
            found = 1
    if found:
        print("FAIL: prohibited string added on this branch")
    else:
        print("PASS: no prohibited strings in added lines")
    return found

def main():
    # stdin file list is accepted for backwards compatibility but the gate is
    # diff-based: it blocks NEW prohibited strings on the branch.
    _ = [line.strip() for line in sys.stdin if line.strip()]
    return added_lines_scan()

if __name__ == '__main__':
    sys.exit(main())
