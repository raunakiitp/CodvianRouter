#!/usr/bin/env bash
# merge-bench-definitive.sh — merge a definitive re-measurement run
# into one file for the dashboard's Benchmark section
# (CHECKLIST 273-274).
#
# Usage: ./scripts/merge-bench-definitive.sh <run.json> <out.json> [cpu-merged.json]
#   run.json: combined bench-results.json from
#     scripts/bench-geekflare.sh — the re-measurement run with the
#     E1 verdict (per-request CPU is the ceiling) and the CPU-profile
#     fixes (token-count memo, single budget read) in.
#   out.json: merged file; its top level IS the run (passes/ready/
#     idle describe it), plus definitive_remeasurement {verdict,
#     clean, method, load_trace}. Point the router's data-dir
#     benchmark.json at it (or copy it there) and GET
#     /dashboard/benchmark serves the run with its clean flag and
#     verdict.
#   cpu-merged.json (optional): the merged CPU before/after file
#     (docs/benchmark-2026-11-18-cpu.json) — its cpu_profile block
#     is carried over verbatim so the served file keeps the
#     Hot-path CPU profile card alongside the new run. Nothing in
#     it is recomputed or altered.
#
# Verdict: computed here from the recorded numbers, never invented
# — pass-1 and pass-2 added latency and RPS against the Geekflare
# targets (< 0.5 ms, > 3,000 RPS), plus ready/idle. Clean: every
# pass's direct-mock canary >= 12,000 RPS (the E1 rule); a polluted
# run is still merged, but clean=false and the verdict says no
# verdict can be drawn from it — it must never be presented as
# the definitive measurement.
set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: $0 <run.json> <out.json> [cpu-merged.json]" >&2
  exit 2
fi
RUN="$1"; OUT="$2"; CPU_MERGED="${3:-}"

python3 - "$RUN" "$OUT" "$CPU_MERGED" <<'PYEOF'
import json, sys

run_path, out_path, cpu_path = sys.argv[1:4]
with open(run_path) as f:
    run = json.load(f)

def canary(r):
    return all(r["passes"][p]["direct_rps"]["rps"] >= 12000 for p in ("1", "2"))

clean = canary(run)
p1, p2 = run["passes"]["1"], run["passes"]["2"]

def targets(p):
    lat_met = p["added_latency_p50_ms"] < 0.5
    rps_met = p["router_rps"]["rps"] > 3000
    return lat_met, rps_met

l1, r1 = targets(p1)
l2, r2 = targets(p2)
ready_ms = max(run["ready_ms"]["healthz"], run["ready_ms"]["v1_models"])
idle_mb = run["idle_rss_kib"] * 1024 / 1e6

pair = (
    f"added latency {p1['added_latency_p50_ms']:.3f} ms pass 1, "
    f"{p2['added_latency_p50_ms']:.3f} ms pass 2; RPS "
    f"{p1['router_rps']['rps']:.0f} pass 1, {p2['router_rps']['rps']:.0f} pass 2 "
    f"(errors {p1['router_rps']['errors']}/{p1['router_rps']['completed']} pass 1, "
    f"{p2['router_rps']['errors']}/{p2['router_rps']['completed']} pass 2); "
    f"ready {ready_ms:.1f} ms, idle {idle_mb:.1f} MiB"
)
if clean:
    both_met = (l1 and r1) or (l2 and r2)
    verdict = (
        "Definitive re-measurement (CHECKLIST 273-274), E1 verdict + "
        "CPU-profile fixes (token-count memo, single budget read) in: "
        f"{pair}. Geekflare targets (added latency < 0.5 ms, RPS > 3,000): "
        f"{'MET' if both_met else 'still MISSED'} — "
        f"latency {'MET' if l1 else 'MISSED'} / RPS {'MET' if r1 else 'MISSED'} pass 1, "
        f"latency {'MET' if l2 else 'MISSED'} / RPS {'MET' if r2 else 'MISSED'} pass 2."
    )
else:
    verdict = (
        "POLLUTED run — at least one pass failed the direct-mock "
        "canary (>= 12,000 RPS), so no definitive verdict can be "
        f"drawn. Recorded numbers ({pair}) are shown for the record "
        "only; re-run in a quiet window."
    )

merged = dict(run)
merged["definitive_remeasurement"] = {
    "verdict": verdict,
    "clean": clean,
    "method": ("Same harness (scripts/bench-geekflare.sh), two passes; "
               "clean = every pass's direct-mock canary >= 12,000 RPS "
               "(the E1 rule). Load trace recorded alongside."),
    "load_trace": {
        "start": run["rig"].get("loadavg_start"),
        "end": run["rig"].get("loadavg"),
    },
}
if cpu_path:
    with open(cpu_path) as f:
        cpu_merged = json.load(f)
    if "cpu_profile" in cpu_merged:
        merged["cpu_profile"] = cpu_merged["cpu_profile"]
        print("carried over cpu_profile from", cpu_path)
with open(out_path, "w") as f:
    json.dump(merged, f, indent=2)
print(f"merged: {out_path} (clean={clean})")
print(f"verdict: {verdict}")
PYEOF
