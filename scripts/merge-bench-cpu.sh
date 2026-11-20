#!/usr/bin/env bash
# merge-bench-cpu.sh — merge a before/after Geekflare-style bench pair
# into one file for the dashboard's Benchmark section
# (CHECKLIST 249-250, hot-path CPU profiling).
#
# Usage: ./scripts/merge-bench-cpu.sh <before.json> <after.json> <out.json>
#   before.json / after.json: combined bench-results.json files from
#     scripts/bench-geekflare.sh — before = tree without the hot-path
#     CPU fixes, after = same tree plus the fixes, nothing else
#     changed between the runs.
#   out.json: merged file; its top level carries cpu_profile
#     {before, after, verdict, clean, method} and its top-level
#     passes/ready/idle describe the AFTER run (the current tree).
#     Point the router's data-dir benchmark.json at it (or copy it
#     there) and GET /dashboard/benchmark serves the pair.
#
# Verdict: computed here from the recorded numbers, never invented
# — pass-1 added latency and RPS deltas (pass 2 quoted as the
# stability check), plus whether the Geekflare targets are met.
# Clean: every pass's direct-mock canary >= 12,000 RPS (the E1
# rule); a polluted pair is still merged, but clean=false and the
# verdict says no verdict can be drawn from it.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <before.json> <after.json> <out.json>" >&2
  exit 2
fi
BEFORE="$1"; AFTER="$2"; OUT="$3"

python3 - "$BEFORE" "$AFTER" "$OUT" <<'PYEOF'
import json, sys

before_path, after_path, out_path = sys.argv[1:4]
with open(before_path) as f:
    before = json.load(f)
with open(after_path) as f:
    after = json.load(f)

def canary(run):
    return all(run["passes"][p]["direct_rps"]["rps"] >= 12000 for p in ("1", "2"))

clean = canary(before) and canary(after)

def pct(a, b):
    return (b - a) / a * 100 if a else 0.0

b1, a1 = before["passes"]["1"], after["passes"]["1"]
b2, a2 = before["passes"]["2"], after["passes"]["2"]
lat_d = pct(b1["added_latency_p50_ms"], a1["added_latency_p50_ms"])
rps_d = pct(b1["router_rps"]["rps"], a1["router_rps"]["rps"])
lat2_d = pct(b2["added_latency_p50_ms"], a2["added_latency_p50_ms"])
rps2_d = pct(b2["router_rps"]["rps"], a2["router_rps"]["rps"])

fixes = ("token-count memo (one BPE encode per distinct prompt), "
         "single budget-row read per pre-send guard, prepared "
         "GetBudget/GetKeyBudget statements, allocation-free "
         "ExactCache.touch")
if clean:
    verdict = (
        f"Hot-path CPU fixes ({fixes}): added latency "
        f"{b1['added_latency_p50_ms']:.3f} -> {a1['added_latency_p50_ms']:.3f} ms "
        f"({lat_d:+.1f}%) pass 1, {b2['added_latency_p50_ms']:.3f} -> "
        f"{a2['added_latency_p50_ms']:.3f} ms ({lat2_d:+.1f}%) pass 2; RPS "
        f"{b1['router_rps']['rps']:.0f} -> {a1['router_rps']['rps']:.0f} "
        f"({rps_d:+.1f}%) pass 1, {b2['router_rps']['rps']:.0f} -> "
        f"{a2['router_rps']['rps']:.0f} ({rps2_d:+.1f}%) pass 2. "
        f"Geekflare targets (added latency < 0.5 ms, RPS > 3,000): "
        f"{'MET' if a1['added_latency_p50_ms'] < 0.5 and a1['router_rps']['rps'] > 3000 else 'still MISSED'} "
        f"on the after run."
    )
else:
    verdict = (
        "POLLUTED pair — at least one pass failed the direct-mock "
        "canary (>= 12,000 RPS), so no before/after verdict can be "
        f"drawn. Recorded deltas (pass 1: added latency "
        f"{b1['added_latency_p50_ms']:.3f} -> {a1['added_latency_p50_ms']:.3f} ms, "
        f"RPS {b1['router_rps']['rps']:.0f} -> {a1['router_rps']['rps']:.0f}) "
        "are shown for the record only."
    )

merged = dict(after)
merged["cpu_profile"] = {
    "before": before,
    "after": after,
    "verdict": verdict,
    "clean": clean,
    "method": ("Same harness (scripts/bench-geekflare.sh), same tree "
               "except the hot-path CPU fixes; clean = every pass's "
               "direct-mock canary >= 12,000 RPS."),
}
with open(out_path, "w") as f:
    json.dump(merged, f, indent=2)
print(f"merged: {out_path} (clean={clean})")
print(f"verdict: {verdict}")
PYEOF
