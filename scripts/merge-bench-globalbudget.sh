#!/usr/bin/env bash
# merge-bench-globalbudget.sh — merge a global-budget hot-path re-measurement
# run into one file for the dashboard's Benchmark section
# (CHECKLIST 299-300).
#
# Usage: ./scripts/merge-bench-globalbudget.sh <run.json> <out.json> <before-pct> <after-pct> <top-cost-after> [keybudget-merged.json]
#   run.json: combined bench-results.json from
#     scripts/bench-geekflare.sh — the re-measurement run with the
#     per-request global-budget memo (GetBudget read once per
#     request per key, shared by every guard on that request) in.
#   out.json: merged file; its top level IS the run (passes/ready/
#     idle describe it), plus globalbudget_remeasurement {verdict,
#     clean, method, load_trace, get_budget_before_pct,
#     get_budget_after_pct, top_cost_after}. Point the router's
#     data-dir benchmark.json at it (or copy it there) and GET
#     /dashboard/benchmark serves the run with its clean flag and
#     verdict.
#   before-pct / after-pct: the GetBudget cumulative profile
#     shares (%) from the 279-280 before-profile
#     (docs/cpu-profile-2026-11-18-keybudget.md, 8.20%) and the
#     fresh after-profile — measured with go tool pprof -top,
#     never invented.
#   top-cost-after: the new top cost from the fresh profile
#     (e.g. "Syscall6 13.1% flat (network I/O, inherent)"), shown
#     when the targets are still missed; "" when the targets are
#     met.
#   keybudget-merged.json (optional): the merged key-budget
#     file — its definitive_remeasurement, cpu_profile and
#     keybudget_remeasurement blocks are carried over verbatim so
#     the served file keeps those cards alongside the new run.
#     Nothing in it is recomputed or altered.
#
# Verdict: computed here from the recorded numbers, never invented
# — pass-1 and pass-2 added latency and RPS against the Geekflare
# targets (< 0.5 ms, > 3,000 RPS), plus ready/idle, plus the
# before/after GetBudget shares. Clean: every pass's
# direct-mock canary >= 12,000 RPS (the E1 rule); a polluted run
# is still merged, but clean=false and the verdict says no
# verdict can be drawn from it — it must never be presented as
# the measurement.
set -euo pipefail

if [ "$#" -lt 5 ] || [ "$#" -gt 6 ]; then
  echo "usage: $0 <run.json> <out.json> <before-pct> <after-pct> <top-cost-after> [keybudget-merged.json]" >&2
  exit 2
fi
RUN="$1"; OUT="$2"; BEFORE_PCT="$3"; AFTER_PCT="$4"; TOP_COST="$5"; DEF_MERGED="${6:-}"

python3 - "$RUN" "$OUT" "$BEFORE_PCT" "$AFTER_PCT" "$TOP_COST" "$DEF_MERGED" <<'PYEOF'
import json, sys

run_path, out_path, before_pct, after_pct, top_cost, def_path = sys.argv[1:7]
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
profile = (
    f"GetBudget {float(before_pct):.2f}% cum (279-280 profile) -> "
    f"{float(after_pct):.2f}% cum (fresh profile)"
)
if clean:
    both_met = (l1 and r1) or (l2 and r2)
    verdict = (
        "Global-budget hot-path fix (CHECKLIST 299-300), per-request "
        "global-budget memo in (GetBudget read once per request per "
        "key, shared by every guard on that request): "
        f"{pair}. Geekflare targets (added latency < 0.5 ms, RPS > 3,000): "
        f"{'MET' if both_met else 'still MISSED'} — "
        f"latency {'MET' if l1 else 'MISSED'} / RPS {'MET' if r1 else 'MISSED'} pass 1, "
        f"latency {'MET' if l2 else 'MISSED'} / RPS {'MET' if r2 else 'MISSED'} pass 2. "
        f"{profile}."
    )
    if not both_met and top_cost:
        verdict += f" New top cost from the fresh profile: {top_cost}."
else:
    verdict = (
        "POLLUTED run — at least one pass failed the direct-mock "
        "canary (>= 12,000 RPS), so no verdict can be "
        f"drawn. Recorded numbers ({pair}) are shown for the record "
        "only; re-run in a quiet window."
    )

merged = dict(run)
merged["globalbudget_remeasurement"] = {
    "verdict": verdict,
    "clean": clean,
    "method": ("Same harness (scripts/bench-geekflare.sh), two passes; "
               "clean = every pass's direct-mock canary >= 12,000 RPS "
               "(the E1 rule). Load trace recorded alongside. "
               "GetBudget shares from go tool pprof -top on the "
               "before/after CPU profiles. Honest scope: on today's "
               "hot paths the global-budget guard reads once per "
               "request, so the memo has no second read to save "
               "there — a within-noise move is reported as such."),
    "load_trace": {
        "start": run["rig"].get("loadavg_start"),
        "end": run["rig"].get("loadavg"),
    },
    "get_budget_before_pct": float(before_pct),
    "get_budget_after_pct": float(after_pct),
    "top_cost_after": top_cost,
}
if def_path:
    with open(def_path) as f:
        def_merged = json.load(f)
    for key in ("definitive_remeasurement", "cpu_profile", "keybudget_remeasurement"):
        if key in def_merged:
            merged[key] = def_merged[key]
            print(f"carried over {key} from", def_path)
with open(out_path, "w") as f:
    json.dump(merged, f, indent=2)
print(f"merged: {out_path} (clean={clean})")
print(f"verdict: {verdict}")
PYEOF
