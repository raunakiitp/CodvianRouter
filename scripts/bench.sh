#!/usr/bin/env bash
# bench.sh — reproducible latency/RPS/memory run (Master Prompt v1,
# M0.3; reporting rules Part 5 §5: machine, Go version, exact
# command, p50/p95/p99, N runs, MET/MISSED per target).
#
# This is a thin wrapper, not a second benchmark rig. It REUSES
# scripts/bench-geekflare.sh (the Geekflare-style rig: two instant
# mock upstreams, 1,000 sequential latency POSTs direct vs through
# the router, 32 clients x 20 s RPS, idle/peak RSS, ready time;
# two passes, pass 1 = reported run, pass 2 = stability check) and
# turns its bench-results.json into one summary JSON on stdout.
#
# Targets (Geekflare-derived, docs/BENCHMARK.md):
#   added latency (router p50 - direct p50) < 0.5 ms
#   router RPS (32 clients x 20 s)           > 3,000
#   idle memory (RSS)                        < 70 MiB
#   ready time (spawn to first 200)          < 10 s
#
# Quiet gate (bench department rule): before the rig starts, this
# wrapper prints the exact command, git HEAD, Go version and the
# loadavg, takes the department flock ($ROOT/.bench-lock, shared
# with scripts/bench-idle-ready.sh), and REFUSES (exit 1, no result
# recorded) while the VM is not quiet — 1-min loadavg >= 3, or any
# `go test` / `go build` running. After the rig it prints the end
# loadavg and refuses again if a go test/go build overlapped the
# run (the raw rig JSON is then kept under a -polluted name, never
# the published record). The end loadavg is reported, not gated:
# the rig's own 32-client phase drives it up by itself.
#
# Honesty rule: if the rig cannot produce a field (missing file,
# corrupt JSON, missing key), that field is emitted as the string
# "UNVERIFIED" and its verdict is "UNVERIFIED" — never a made-up
# number. A failed rig run exits non-zero after printing a JSON
# object whose measurements are all "UNVERIFIED".
#
# Usage:
#   ./scripts/bench.sh [--out PATH] [--limiter on|off]
#     --out PATH     where to save the rig's raw bench-results.json
#                    (default: docs/benchmark-<UTC timestamp>.json)
#     --limiter      CODVIAN_BENCH_LIMITER mode for the rig
#                    (default: on; off disables the adaptive limiter)
#   The summary JSON goes to stdout; the raw rig JSON is the durable
#   record at --out. A short verdict table goes to stderr.
# Exit: 0 when the rig ran and the summary was produced, 1 otherwise.
#   (A MISSED target is a measurement, not a script failure: the
#   exit code stays 0 and the verdicts say MISSED.)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"

OUT=""
LIMITER="${CODVIAN_BENCH_LIMITER:-on}"
while [ $# -gt 0 ]; do
  case "$1" in
    --out)     [ $# -ge 2 ] || { echo "bench.sh: --out needs a path" >&2; exit 2; }
               OUT="$2"; shift 2 ;;
    --limiter) [ $# -ge 2 ] || { echo "bench.sh: --limiter needs on|off" >&2; exit 2; }
               LIMITER="$2"; shift 2 ;;
    -h|--help) sed -n '2,46p' "$0"; exit 0 ;;
    *) echo "bench.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$LIMITER" in
  on|off) ;;
  *) echo "bench.sh: --limiter must be on|off, got $LIMITER" >&2; exit 2 ;;
esac

if [ -z "$OUT" ]; then
  OUT="$ROOT/docs/benchmark-$(date -u +%Y-%m-%d-%H%M%S).json"
fi
# The rig copies its combined results to CODVIAN_BENCH_OUT, so the
# raw record lands exactly at OUT (see scripts/bench-geekflare.sh).
COMMAND="CODVIAN_BENCH_LIMITER=$LIMITER CODVIAN_BENCH_OUT=$OUT ./scripts/bench-geekflare.sh"

# --- Department lock + quiet gate. ---
# A benchmark measured under contention is a measurement of the
# contention, not of the router (docs/BENCHMARK.md: attempt 3a
# launched at loadavg 1.32 and was polluted end-to-end by a gate
# that started 6 s later). The lock ($ROOT/.bench-lock, shared with
# scripts/bench-idle-ready.sh) serialises benchmarks
# department-wide; a second bench refuses instead of queueing,
# because a queued bench would start into a stormed VM anyway.
# The gate runs BEFORE the rig starts, on both halves of the quiet
# rule. It runs again AFTER the rig, but only on the go test /
# go build half: the rig's own 32-client RPS phase drives the
# 1-min loadavg up by itself (the 01:16 re-run recorded 10.58 at
# end — the bench's own load, not background noise), so an
# end-of-run loadavg >= 3 proves nothing about contention. The end
# loadavg is still printed and recorded for the record. Mid-run
# transient contention that starts and ends inside the run is NOT
# caught by this gate; the rig's direct-mock canary is the detector
# for that (docs/BENCHMARK.md, attempt-3 section).
LOCK_FILE="$ROOT/.bench-lock"
load1() { awk '{print $1}' /proc/loadavg; }
load_ge_3() { awk -v l="$1" 'BEGIN { exit ((l + 0) >= 3) ? 0 : 1 }'; }
# `go test` / `go build` processes that are NOT us and NOT our
# ancestors: a coordinator shell that launched us may itself
# contain the words "go test" in its command line, and that is
# not contention — only foreign test/build activity is.
foreign_go_test_build() {
  local ancestors=" $$ " p="$PPID"
  while [ -n "$p" ] && [ "$p" != "0" ]; do
    ancestors="$ancestors$p "
    p="$(awk '/^PPid:/ {print $2}' "/proc/$p/status" 2>/dev/null || true)"
  done
  pgrep -af 'go (test|build)' 2>/dev/null | while read -r pid rest; do
    case "$ancestors" in *" $pid "*) ;; *) echo "$pid $rest";; esac
  done
  # Always return 0: pgrep exits 1 when nothing matches (the quiet
  # case), and under `set -euo pipefail` that 1 would kill bench.sh
  # at the FOREIGN=$(...) assignment — exactly when the VM is quiet.
  # (Found 2026-11-20 during Q1: bench.sh died silently at loadavg
  # 0.24 with no foreign process running.) The caller decides from
  # the printed lines, not from this status.
  return 0
}

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "bench.sh: REFUSED — another benchmark holds the department lock ($LOCK_FILE)." >&2
  echo "bench.sh: A benchmark is already running. Wait for it to finish, then retry." >&2
  exit 1
fi
if GIT_HEAD="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null)"; then
  _n="$(git -C "$ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$_n" = "0" ]; then GIT_STATE="clean"; else GIT_STATE="dirty ($_n files)"; fi
else
  GIT_HEAD="UNKNOWN"; GIT_STATE="unknown"
fi
GO_VERSION="$(go version 2>/dev/null || echo UNKNOWN)"
LOADAVG_START="$(cat /proc/loadavg)"
{
  echo "bench.sh: command: $COMMAND"
  echo "bench.sh: git HEAD: $GIT_HEAD ($GIT_STATE)"
  echo "bench.sh: go: $GO_VERSION"
  echo "bench.sh: loadavg at start: $LOADAVG_START"
} >&2
if load_ge_3 "$(load1)"; then
  echo "bench.sh: REFUSED — VM is not quiet: 1-min loadavg $(load1) >= 3." >&2
  echo "bench.sh: Wait for a quiet window (loadavg < 3, no go test/go build running), then retry. No result was recorded." >&2
  exit 1
fi
FOREIGN="$(foreign_go_test_build)"
if [ -n "$FOREIGN" ]; then
  echo "bench.sh: REFUSED — VM is not quiet: go test/go build processes are running:" >&2
  printf '%s\n' "$FOREIGN" | sed 's/^/bench.sh:   /' >&2
  echo "bench.sh: Wait for them to finish, then retry. No result was recorded." >&2
  exit 1
fi

# --- Run the rig. Its stdout/stderr stay visible (progress + table);
# the summary JSON below is the machine-readable verdict. ---
set +e
CODVIAN_BENCH_LIMITER="$LIMITER" CODVIAN_BENCH_OUT="$OUT" \
  "$ROOT/scripts/bench-geekflare.sh"
RIG_RC=$?
set -e

LOADAVG_END="$(cat /proc/loadavg)"
echo "bench.sh: loadavg at end: $LOADAVG_END" >&2
# End gate (go test/go build half only — see the comment above for
# why the loadavg half does not apply at the end). A test/build
# that overlapped the run means the numbers are polluted: keep the
# raw rig JSON for the record under a -polluted name and record
# nothing as a clean result.
FOREIGN="$(foreign_go_test_build)"
if [ -n "$FOREIGN" ]; then
  echo "bench.sh: REFUSED — a go test/go build overlapped this run; the result is POLLUTED, not a clean measurement:" >&2
  printf '%s\n' "$FOREIGN" | sed 's/^/bench.sh:   /' >&2
  if [ -f "$OUT" ]; then
    POLLUTED_OUT="${OUT%.json}-polluted.json"
    mv "$OUT" "$POLLUTED_OUT"
    echo "bench.sh: Raw rig JSON kept for the record as $POLLUTED_OUT (never the published record)." >&2
  fi
  echo "bench.sh: No summary was recorded. Re-run in a quiet window." >&2
  exit 1
fi

# --- Summarise the rig's JSON (or emit UNVERIFIED if it is absent). ---
# python3 reads $OUT and prints the summary JSON to stdout.
# argv: 1=OUT 2=COMMAND 3=RIG_RC 4=LIMITER 5=GIT_HEAD 6=GIT_STATE
#       7=LOADAVG_START 8=LOADAVG_END
python3 - "$OUT" "$COMMAND" "$RIG_RC" "$LIMITER" "$GIT_HEAD" "$GIT_STATE" "$LOADAVG_START" "$LOADAVG_END" <<'PYEOF'
import json, os, subprocess, sys

out_path, command, rig_rc, limiter = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
git_head, git_state = sys.argv[5], sys.argv[6]
loadavg_start_w, loadavg_end_w = sys.argv[7], sys.argv[8]
U = "UNVERIFIED"  # the honest value for any field the rig did not produce

def verdict(value, target, better):
    """MET/MISSED/UNVERIFIED. better='low' means value must be < target;
    better='high' means value must be > target."""
    if value is None or value == U:
        return U
    try:
        v = float(value)
    except (TypeError, ValueError):
        return U
    if better == "low":
        return "MET" if v < target else "MISSED"
    return "MET" if v > target else "MISSED"

def get(d, *keys):
    for k in keys:
        if not isinstance(d, dict) or k not in d:
            return None
        d = d[k]
    return d

raw = None
if rig_rc == 0 and os.path.exists(out_path):
    try:
        with open(out_path) as f:
            raw = json.load(f)
    except (OSError, ValueError):
        raw = None

summary = {
    "script": "scripts/bench.sh",
    "rig": "scripts/bench-geekflare.sh",
    "command": command,
    "limiter_mode": limiter,
    "status": "OK" if raw is not None else "FAILED",
    "rig_exit_code": rig_rc,
    "source_file": out_path,
    "git_head": git_head,
    "git_state": git_state,
    "loadavg_start_wrapper": loadavg_start_w.split()[:3],
    "loadavg_end_wrapper": loadavg_end_w.split()[:3],
}

if raw is None:
    # Rig failed or produced nothing readable: every measurement is
    # UNVERIFIED. Nothing is guessed from a partial or stale file.
    summary.update({
        "machine": U, "go_version": U, "date": U,
        "runs": U, "passes": U,
        "idle_mib": U, "ready_ms": U,
        "verdicts": {
            "added_latency_ms": U, "rps": U,
            "idle_mib": U, "ready_ms": U,
        },
    })
    print(json.dumps(summary, indent=2))
    sys.exit(1)

rig = raw.get("rig", {}) if isinstance(raw.get("rig", {}), dict) else {}
passes_raw = raw.get("passes", {}) if isinstance(raw.get("passes", {}), dict) else {}

def lat_block(d):
    if not isinstance(d, dict):
        return {"p50_ms": U, "p95_ms": U, "p99_ms": U,
                "mean_ms": U, "requests": U, "ok": U,
                "errors": U, "cache_hits": U}
    return {k: (d.get(k) if d.get(k) is not None else U)
            for k in ("p50_ms", "p95_ms", "p99_ms", "mean_ms",
                      "requests", "ok", "errors", "cache_hits")}

def mib(kib):
    if kib is None or kib == U:
        return U
    try:
        return round(float(kib) / 1024, 1)
    except (TypeError, ValueError):
        return U

summary["machine"] = {
    "os": rig.get("os", U), "cpus": rig.get("cpus", U),
    "mem_total_kib": rig.get("mem_total_kib", U),
    "cpu_model": rig.get("cpu_model", U),
    "loadavg_start": rig.get("loadavg_start", U),
    "loadavg_end": rig.get("loadavg", U),
    "note": rig.get("note", U),
}
summary["go_version"] = rig.get("go_version", U)
summary["date"] = rig.get("date", U)
summary["runs"] = len(passes_raw) if passes_raw else U

passes = {}
for p in ("1", "2"):
    pr = passes_raw.get(p)
    if not isinstance(pr, dict):
        passes[p] = {
            "router_latency_ms": lat_block(None),
            "direct_latency_ms": lat_block(None),
            "added_latency_p50_ms": U, "router_rps": U,
            "direct_rps": U, "peak_rss_mib": U,
        }
        continue
    rps_r = pr.get("router_rps") if isinstance(pr.get("router_rps"), dict) else {}
    rps_d = pr.get("direct_rps") if isinstance(pr.get("direct_rps"), dict) else {}
    passes[p] = {
        "router_latency_ms": lat_block(pr.get("router_latency")),
        "direct_latency_ms": lat_block(pr.get("direct_latency")),
        "added_latency_p50_ms": pr.get("added_latency_p50_ms", U),
        "router_rps": rps_r.get("rps", U),
        "router_rps_upstream": rps_r.get("upstream_rps", U),
        "router_rps_errors": rps_r.get("errors", U),
        "direct_rps": rps_d.get("rps", U),
        "peak_rss_mib": mib(pr.get("peak_rss_kib")),
    }
summary["passes"] = passes

summary["idle_mib"] = mib(raw.get("idle_rss_kib"))
ready = raw.get("ready_ms") if isinstance(raw.get("ready_ms"), dict) else {}
summary["ready_ms"] = {
    "healthz": ready.get("healthz", U),
    "v1_models": ready.get("v1_models", U),
}

# --- Verdicts. Overall = pass 1, the rig's reported run
# (pass 2 is the stability check); per-pass verdicts are listed too.
def pass_val(p, key):
    v = passes.get(p, {}).get(key)
    return None if v == U else v

def ready_worst():
    vals = [ready.get("healthz"), ready.get("v1_models")]
    vals = [v for v in vals if isinstance(v, (int, float))]
    return max(vals) if vals else None

verdicts = {
    "added_latency_ms": {
        "pass1": verdict(pass_val("1", "added_latency_p50_ms"), 0.5, "low"),
        "pass2": verdict(pass_val("2", "added_latency_p50_ms"), 0.5, "low"),
        "overall": verdict(pass_val("1", "added_latency_p50_ms"), 0.5, "low"),
        "target": "< 0.5 ms",
    },
    "rps": {
        "pass1": verdict(pass_val("1", "router_rps"), 3000, "high"),
        "pass2": verdict(pass_val("2", "router_rps"), 3000, "high"),
        "overall": verdict(pass_val("1", "router_rps"), 3000, "high"),
        "target": "> 3000",
    },
    "idle_mib": {
        "overall": verdict(summary["idle_mib"], 70, "low"),
        "target": "< 70 MiB",
    },
    "ready_ms": {
        "overall": verdict(ready_worst(), 10000, "low"),
        "target": "< 10000 ms (10 s)",
    },
}
summary["verdicts"] = verdicts

print(json.dumps(summary, indent=2))

# Short human table on stderr (stdout stays pure JSON).
def show(v):
    return str(v) if v != U else U
sys.stderr.write("\nbench.sh verdicts (target / pass 1 / pass 2):\n")
sys.stderr.write(f"  added latency < 0.5 ms : "
                 f"{show(pass_val('1', 'added_latency_p50_ms'))} / "
                 f"{show(pass_val('2', 'added_latency_p50_ms'))} ms -> "
                 f"{verdicts['added_latency_ms']['overall']}\n")
sys.stderr.write(f"  router RPS      > 3000 : "
                 f"{show(pass_val('1', 'router_rps'))} / "
                 f"{show(pass_val('2', 'router_rps'))} -> "
                 f"{verdicts['rps']['overall']}\n")
sys.stderr.write(f"  idle memory     < 70 MiB: {show(summary['idle_mib'])} MiB -> "
                 f"{verdicts['idle_mib']['overall']}\n")
sys.stderr.write(f"  ready           < 10 s  : "
                 f"{show(ready.get('healthz'))} / {show(ready.get('v1_models'))} ms -> "
                 f"{verdicts['ready_ms']['overall']}\n")
sys.stderr.write(f"  raw rig JSON: {out_path}\n")
PYEOF
