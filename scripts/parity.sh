#!/usr/bin/env bash
# parity.sh — Master Prompt v2 §5.1. Mechanical parity count from docs/PARITY.md.
# Rule: sum the per-area tallies in the section headers (### Area N — ... FULL x,
# PARTIAL y, MISSING z, WRONG w). For Area 14 use the four sub-area headers
# (#### 14a–14d) instead of the Area 14 rollup header, because the rollup
# contradicts its own parts (PARTIAL 41 vs 17+21+4+2=44) — the sub-headers are
# the more granular canonical tallies. Cross-checks against the headline block
# and exits 1 on mismatch. First-run target: 238/191/216/7, 36.5%/51.2%.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
F="$ROOT/docs/PARITY.md"
read -r MF MP MM MW COMP STRICT WEIGHTED <<EOF2
$(python3 - "$F" <<'PYEOF'
import re, sys
f = 0; p = 0; m = 0; w = 0
for line in open(sys.argv[1]):
    if re.match(r'^### Area [0-9]+[a-z]? — ', line) and not line.startswith('### Area 14 — '):
        pass
    elif re.match(r'^#### 14[abcd]\. ', line):
        pass
    else:
        continue
    for key, acc in (('FULL', 'f'), ('PARTIAL', 'p'), ('MISSING', 'm'), ('WRONG', 'w')):
        mm = re.search(rf'{key} ([0-9]+)', line)
        if mm:
            if key == 'FULL': f += int(mm.group(1))
            elif key == 'PARTIAL': p += int(mm.group(1))
            elif key == 'MISSING': m += int(mm.group(1))
            else: w += int(mm.group(1))
comp = f + p + m + w
print(f"{f} {p} {m} {w} {comp} {100*f/comp:.1f} {100*(f+0.5*p)/comp:.1f}")
PYEOF
)
EOF2
echo "PARITY mechanical (area headers; Area 14 via 14a-14d): FULL=$MF PARTIAL=$MP MISSING=$MM WRONG=$MW comparable=$COMP"
echo "strict=${STRICT}% weighted=${WEIGHTED}%"
HF=$(grep -oE '^FULL [0-9]+' "$F" | head -1 | awk '{print $2}')
HP=$(grep -oE 'PARTIAL [0-9]+' "$F" | head -1 | awk '{print $2}')
HM=$(grep -oE 'MISSING [0-9]+' "$F" | head -1 | awk '{print $2}')
HW=$(grep -oE 'WRONG [0-9]+' "$F" | head -1 | awk '{print $2}')
HS=$(grep -oE 'Strict parity [0-9.]+%' "$F" | head -1 | grep -oE '[0-9.]+')
HWD=$(grep -oE 'Weighted parity [0-9.]+%' "$F" | head -1 | grep -oE '[0-9.]+')
HC=$(grep -oE 'Comparable total [0-9]+' "$F" | head -1 | awk '{print $3}')
echo "PARITY headline (file lines 28-29): FULL=$HF PARTIAL=$HP MISSING=$HM WRONG=$HW comparable=$HC strict=${HS}% weighted=${HWD}%"
if [ "$MF" = "$HF" ] && [ "$MP" = "$HP" ] && [ "$MM" = "$HM" ] && [ "$MW" = "$HW" ]; then
  echo "PARITY CHECK: MATCH — mechanical count reproduces headline."
  exit 0
else
  echo "PARITY CHECK: MISMATCH — mechanical ($MF/$MP/$MM/$MW) vs headline ($HF/$HP/$HM/$HW). File needs A5 reconciliation; do not quote either as settled."
  exit 1
fi
