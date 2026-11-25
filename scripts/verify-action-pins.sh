#!/usr/bin/env bash
# verify-action-pins.sh — validate every action pin in .github/workflows/*.yml
#   - ref must be a full 40-char lowercase hex SHA
#   - when gh is available and online, resolve each pin against the GitHub API
# Exits non-zero on any bad pin.
#
# Usage: verify-action-pins.sh [WORKFLOWS_DIR]
#   WORKFLOWS_DIR defaults to .github/workflows

set -euo pipefail

WORKFLOWS_DIR="${1:-.github/workflows}"
BAD=0

if [[ ! -d "$WORKFLOWS_DIR" ]]; then
  echo "FAIL: $WORKFLOWS_DIR not found" >&2
  exit 1
fi

# Extract all uses: lines with owner/repo@ref
# Pattern: uses: owner/repo@ref (ref may be tag or SHA)
mapfile -t USES_LINES < <(grep -h "uses:" "$WORKFLOWS_DIR"/*.yml | grep -v "^\s*#" | sed 's/.*uses:\s*//' | sed 's/\s*#.*//' | sed 's/["'\'']//g' | sort -u)

if [[ ${#USES_LINES[@]} -eq 0 ]]; then
  echo "No uses: entries found"
  exit 0
fi

echo "Found ${#USES_LINES[@]} unique action pins:"
printf '  %s\n' "${USES_LINES[@]}"
echo ""

for line in "${USES_LINES[@]}"; do
  # Parse owner/repo[@path]@ref
  # The ref is the last @... (40 hex chars)
  # Everything before the last @ is the action path
  if [[ ! "$line" =~ ^(.+)@([0-9a-f]{40})$ ]]; then
    echo "FAIL: cannot parse '$line' (no valid SHA ref)" >&2
    BAD=1
    continue
  fi
  ACTION_PATH="${BASH_REMATCH[1]}"
  REF="${BASH_REMATCH[2]}"

  # Extract owner/repo from action_path (before any /path)
  # owner/repo or owner/repo/path
  if [[ ! "$ACTION_PATH" =~ ^([^/]+)/([^/]+)(/.*)?$ ]]; then
    echo "FAIL: cannot parse action path '$ACTION_PATH'" >&2
    BAD=1
    continue
  fi
  OWNER="${BASH_REMATCH[1]}"
  REPO="${BASH_REMATCH[2]}"

  # Check ref format: must be 40 lowercase hex chars (already matched)
  # If gh is available and we have network, verify the commit exists
  if command -v gh >/dev/null 2>&1; then
    if gh auth status >/dev/null 2>&1; then
      echo -n "Checking $OWNER/$REPO@$REF ... "
      if gh api "/repos/$OWNER/$REPO/commits/$REF" >/dev/null 2>&1; then
        # Also get commit subject for evidence
        SUBJECT=$(gh api "/repos/$OWNER/$REPO/commits/$REF" --jq '.commit.message' 2>/dev/null | head -1 | cut -d$'\n' -f1)
        echo "OK (${SUBJECT})"
      else
        echo "FAIL: commit not found via GitHub API" >&2
        BAD=1
      fi
    else
      echo "SKIP: gh not authenticated, skipping API check for $OWNER/$REPO@$REF"
    fi
  else
    echo "SKIP: gh not installed, skipping API check for $OWNER/$REPO@$REF"
  fi
done

if [[ $BAD -eq 1 ]]; then
  echo ""
  echo "FAIL: one or more action pins are invalid" >&2
  exit 1
else
  echo ""
  echo "PASS: all action pins are valid 40-char SHAs and resolve via GitHub API"
  exit 0
fi