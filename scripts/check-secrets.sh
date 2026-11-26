#!/usr/bin/env bash
# scripts/check-secrets.sh
#
# Fail-closed secret scan over tracked files (P2-SEC-01).
#
# SECURITY.md used to claim this gate existed; it did not. The gate is
# deterministic, offline and dependency-free: no network, no service to
# authenticate, nothing to download. It scans every tracked file that git
# considers text, and exits 1 on a hit.
#
# The patterns are assembled from fragments so that this script does not
# match its own source (the same technique scripts/check-naming.sh uses).
#
# Usage:  scripts/check-secrets.sh            # scan every tracked file
#         scripts/check-secrets.sh --staged   # scan only staged additions
#
# Exit:   0 clean · 1 findings · 2 the scanner could not run (git missing,
#         not a repository). "Could not run" is never reported as "clean".

set -uo pipefail

if ! command -v git >/dev/null 2>&1; then
	echo "check-secrets: git is not available" >&2
	exit 2
fi
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "check-secrets: not inside a git work tree" >&2
	exit 2
fi

# --- high-precision credential shapes, built from fragments ---------------
p_ghptok='g''hp_[A-Za-z0-9]{30,}'
p_ghpat='github''_pat_[A-Za-z0-9_]{30,}'
p_gho='g''ho_[A-Za-z0-9]{30,}'
p_ghs='g''hs_[A-Za-z0-9]{30,}'
p_glpat='gl''pat-[A-Za-z0-9_-]{20,}'
p_aws='AK''IA[0-9A-Z]{16}'
p_googleapi='AI''za[0-9A-Za-z_-]{35}'
p_slack='xo''x[baprs]-[0-9A-Za-z-]{10,}'
p_anthropic='sk-''ant-[A-Za-z0-9_-]{24,}'
p_openai='sk-''[A-Za-z0-9]{48}'
p_privkey='-----BEGIN ''(RSA|EC|OPENSSH|PGP) PRIVATE KEY-----'
p_bearer='Authorization: *Bearer ''[A-Za-z0-9._-]{40,}'

PATTERNS="(${p_ghptok}|${p_ghpat}|${p_gho}|${p_ghs}|${p_glpat}|${p_aws}|${p_googleapi}|${p_slack}|${p_anthropic}|${p_openai}|${p_privkey}|${p_bearer})"

# Files that legitimately contain credential-SHAPED test data. Each entry is
# a path under the repository root. Keep this list tiny and justified —
# anything here is exempt from the scan, so an unjustified entry is a hole.
ALLOWLIST=(
	"internal/guardrails/guardrails_test.go" # synthetic redaction fixtures
)

MODE="${1:-}"
if [ "$MODE" = "--staged" ]; then
	FILES=$(git diff --cached --name-only --diff-filter=ACMR || exit 2)
else
	FILES=$(git ls-files || exit 2)
fi

allowlisted() {
	local f="$1" a
	for a in "${ALLOWLIST[@]}"; do
		[ "$f" = "$a" ] && return 0
	done
	return 1
}

found=0
scanned=0
while IFS= read -r file; do
	[ -z "$file" ] && continue
	allowlisted "$file" && continue
	# Binary files are skipped: a byte pattern match in an image or archive
	# is noise, not a credential.
	if [ -f "$file" ] && ! grep -Iq . "$file" 2>/dev/null; then
		continue
	fi
	scanned=$((scanned + 1))
	if grep -EnI "$PATTERNS" "$file" 2>/dev/null | head -5 | grep -q .; then
		echo "SECRET-SHAPED STRING: $file"
		grep -EnI "$PATTERNS" "$file" 2>/dev/null | head -3 | sed 's/\(:\).*/\1<redacted match>/'
		found=1
	fi
done <<EOF
$FILES
EOF

echo "check-secrets: scanned $scanned tracked file(s)"
if [ "$found" -eq 1 ]; then
	echo "FAIL: secret-shaped strings found in tracked files. Remove them and rotate the credential."
	exit 1
fi
echo "PASS: no secret-shaped strings found."
exit 0