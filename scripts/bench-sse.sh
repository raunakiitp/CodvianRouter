#!/usr/bin/env bash
# bench-sse.sh — SSE token-delivery measurement (CHECKLIST items 135-136).
#
# Method:
#   * mock: a Python SSE server (this script writes it to $WORK)
#     that streams N tokens at a KNOWN cadence (1 token every
#     C ms), each token carrying the mock's emit timestamp
#     (nanoseconds, same machine clock) inside its content:
#     "tok0007@<emit_ns>". It also answers GET /v1/models so
#     `provider add` can live-test the key.
#   * client: a Python reader (also written to $WORK) that POSTs
#     a streaming chat completion to the router, reads each SSE
#     data line as it arrives, and records delivery latency =
#     receive_ns - emit_ns per token. Tokens are checked in
#     order with none missing.
#   * before/after: the SAME router binary is run twice — once
#     with CODVIAN_ROUTER_SSE_BATCH=0 (flush per token, the old
#     behaviour) and once with batching on (flush at most every
#     8 ms or every 3 tokens). Two cadences each: 10 ms (a slow
#     stream, where the 8 ms timer fires first) and 1 ms (a
#     fast stream, where the 3-token bound fires first).
#   * 200 tokens per run. Median / p99 / mean / max per run.
#
# What the numbers mean (read before quoting them): batching is
# a throughput trade, not a latency win. At a 10 ms cadence each
# token waits the full 8 ms timer — its successor is 10 ms away,
# so the 3-token bound never fires first — and the "after"
# median is ~8 ms WORSE than "before". At a 1 ms cadence the
# 3-token bound fires first and the wait is ~2 ms. The win is
# ~3x fewer Flush calls under fast streams, not faster tokens.
# There is NO latency target for token delivery, so no run is
# scored MET/MISSED anywhere — the dashboard shows these
# numbers as measurements.
#
# Usage: ./scripts/bench-sse.sh
# Results: a summary table on stdout, the combined results in
# $WORK/sse-results.json, and a copy in docs/sse-delivery-*.json
# (the repo record). Nothing else is written into the repo.
#
# CODVIAN_SSE_OUT=<path>: when set, the combined results are
# also copied there — point it at the router's data dir (e.g.
# ~/.local/share/codvian-router/streaming.json) so the
# dashboard's Benchmark section (GET /dashboard/streaming)
# shows the last measured run.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/go/bin:$PATH"
export TMPDIR="$HOME/.tmp"
mkdir -p "$TMPDIR"

read -r MOCK_PORT ROUTER_PORT < <(python3 -c "
import socket
ports = []
for _ in range(2):
    s = socket.socket()
    s.bind(('127.0.0.1', 0))
    ports.append(s.getsockname()[1])
    s.close()
print(*ports)
")

WORK="$(mktemp -d "$HOME/.tmp/codvian-sse-bench.XXXXXX")"
mkdir -p "$WORK/logs"
echo "work dir: $WORK"
echo "rig: $(uname -srm), CPUs=$(nproc), mem=$(grep MemTotal /proc/meminfo | awk '{print $2}') KiB, cpu_model=$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs), go=$(go version), loadavg=$(cat /proc/loadavg)"
echo "router version: $(grep -m1 '^const Version' "$ROOT/cmd/codvian-router/main.go" || echo unknown)"
echo "mock port: $MOCK_PORT (SSE, cadenced), router port: $ROUTER_PORT"

MOCK_PID="" ROUTER_PID=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$ROUTER_PID" ] && kill "$ROUTER_PID" 2>/dev/null || true
}
trap cleanup EXIT

# poll URL until HTTP 200 (or timeout); prints elapsed ms since
# $START_NS (nanosecond epoch passed as $2).
poll_until_200() {
  python3 - "$1" "$2" <<'PYEOF'
import sys, time, urllib.request
url, start_ns = sys.argv[1], int(sys.argv[2])
t0 = time.monotonic()
while True:
    try:
        with urllib.request.urlopen(url, timeout=2) as r:
            if r.status == 200:
                print(f"{(time.time_ns() - start_ns) / 1e6:.1f}")
                sys.exit(0)
    except Exception:
        pass
    if time.monotonic() - t0 > 30:
        print("TIMEOUT")
        sys.exit(1)
    time.sleep(0.002)
PYEOF
}

# ---- mock: cadenced SSE provider ---------------------------------------
cat >"$WORK/mock_sse.py" <<'PYEOF'
# Cadenced SSE mock: streams TOKENS tokens, one every CADENCE_MS,
# each token carrying its emit timestamp (ns) in its content so
# the client can measure delivery latency end to end.
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]); CADENCE_MS = float(sys.argv[2]); TOKENS = int(sys.argv[3])

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path == "/v1/models":
            self._json(200, {"data": [{"id": "sse-model"}], "object": "list"})
        else:
            self._json(404, {"error": "not found"})
    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self._json(404, {"error": "not found"}); return
        length = int(self.headers.get("Content-Length", 0))
        if length: self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for i in range(TOKENS):
            emit_ns = time.time_ns()
            content = "tok%04d@%d" % (i, emit_ns)
            chunk = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
                     "created": 0, "model": "sse-model",
                     "choices": [{"index": 0, "delta": {"content": content}, "finish_reason": None}]}
            self.wfile.write(("data: %s\n\n" % json.dumps(chunk)).encode())
            self.wfile.flush()
            if i < TOKENS - 1:
                time.sleep(CADENCE_MS / 1000.0)
        fin = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
               "created": 0, "model": "sse-model",
               "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
        self.wfile.write(("data: %s\n\ndata: [DONE]\n\n" % json.dumps(fin)).encode())
        self.wfile.flush()

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PYEOF

# ---- client: per-token delivery latency ----------------------------------
cat >"$WORK/sse_client.py" <<'PYEOF'
# Reads a streaming chat completion from the router and measures
# per-token delivery latency: receive_ns - emit_ns, where emit_ns
# is the timestamp the mock embedded in the token content.
import json, re, statistics, sys, time, urllib.request

BASE, KEY, MODEL, TOKENS, OUT, PROMPT = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5], sys.argv[6]
TOK = re.compile(r"^tok(\d+)@(\d+)$")

req = urllib.request.Request(
    BASE + "/v1/chat/completions",
    data=json.dumps({"model": MODEL, "messages": [{"role": "user", "content": PROMPT}], "stream": True}).encode(),
    headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
    method="POST")
lat = []
with urllib.request.urlopen(req, timeout=60) as resp:
    if resp.status != 200:
        print("router answered %d" % resp.status, file=sys.stderr); sys.exit(1)
    while True:
        line = resp.readline()
        if not line:
            break
        recv_ns = time.time_ns()
        line = line.decode("utf-8", "replace").strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            break
        try:
            content = json.loads(data)["choices"][0]["delta"].get("content", "")
        except Exception:
            continue
        m = TOK.match(content or "")
        if not m:
            continue
        idx, emit_ns = int(m.group(1)), int(m.group(2))
        if idx != len(lat):
            print("token order/count mismatch: got #%d, want #%d" % (idx, len(lat)), file=sys.stderr)
            sys.exit(1)
        lat.append((recv_ns - emit_ns) / 1e6)

if len(lat) != TOKENS:
    print("received %d tokens, want %d" % (len(lat), TOKENS), file=sys.stderr)
    sys.exit(1)
lat_s = sorted(lat)
def pct(p):
    return lat_s[min(len(lat_s) - 1, int(p * len(lat_s)))]
res = {"median_ms": round(statistics.median(lat), 3), "p99_ms": round(pct(0.99), 3),
       "mean_ms": round(statistics.mean(lat), 3), "max_ms": round(max(lat), 3),
       "received": len(lat)}
with open(OUT, "w") as f:
    json.dump(res, f)
print(json.dumps(res))
PYEOF

# ---- build + bring up ----------------------------------------------------
go build -o "$WORK/codvian-router" ./cmd/codvian-router
echo "binary: router $(du -h "$WORK/codvian-router" | cut -f1)"

start_mock() { # $1 = cadence_ms
  python3 "$WORK/mock_sse.py" "$MOCK_PORT" "$1" 200 >"$WORK/logs/mock-$1.txt" 2>&1 &
  MOCK_PID=$!
  # Poll, don't sleep-and-hope: Python startup under a loaded
  # VM can take longer than a fixed 0.5 s (a fixed sleep raced
  # and the /v1/models probe failed to connect on the 1 ms
  # restart in the first full run).
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$MOCK_PORT/v1/models" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "mock did not come up on 127.0.0.1:$MOCK_PORT"; cat "$WORK/logs/mock-$1.txt"; exit 1
}

HOME_DIR="$WORK/home"
mkdir -p "$HOME_DIR"
start_mock 10
HOME="$HOME_DIR" "$WORK/codvian-router" init >"$WORK/logs/init.txt" 2>&1
KEY="$(awk '/Gateway key \(shown ONCE/{getline; print; exit}' "$WORK/logs/init.txt")"
[ -n "$KEY" ] || { echo "init failed"; cat "$WORK/logs/init.txt"; exit 1; }
HOME="$HOME_DIR" "$WORK/codvian-router" provider add "http://127.0.0.1:$MOCK_PORT" mock-key --name mock-sse >"$WORK/logs/provider.txt" 2>&1
echo "provider: mock-sse (127.0.0.1:$MOCK_PORT), model sse-model"

run_scenario() { # $1 = batch label (off|on), $2 = cadence_ms, $3 = out json
  local label="$1" cadence="$2" out="$3"
  echo "--- batching $label, cadence ${cadence} ms ---"
  kill "$MOCK_PID" 2>/dev/null || true
  wait "$MOCK_PID" 2>/dev/null || true
  start_mock "$cadence"
  # A distinct prompt per scenario: identical prompts would hit
  # the router's exact-match cache and stream no upstream tokens.
  python3 "$WORK/sse_client.py" "http://127.0.0.1:$ROUTER_PORT" "$KEY" sse-model 200 "$out" "stream tokens please batching-$label cadence-${cadence} $(date +%s%N)"
}

start_router() { # $1 = batch env value (0 = off, unset = on)
  local batch="$1"
  START_NS="$(date +%s%N)"
  if [ "$batch" = "0" ]; then
    CODVIAN_ROUTER_SSE_BATCH=0 HOME="$HOME_DIR" "$WORK/codvian-router" --port "$ROUTER_PORT" serve >"$WORK/logs/router-off.txt" 2>&1 &
  else
    HOME="$HOME_DIR" "$WORK/codvian-router" --port "$ROUTER_PORT" serve >"$WORK/logs/router-on.txt" 2>&1 &
  fi
  ROUTER_PID=$!
  READY="$(poll_until_200 "http://127.0.0.1:$ROUTER_PORT/healthz" "$START_NS")"
  echo "router up (batching $batch): /healthz ${READY} ms"
  if [ "$batch" = "0" ]; then grep -m1 "sse delivery" "$WORK/logs/router-off.txt" || true
  else grep -m1 "sse delivery" "$WORK/logs/router-on.txt" || true; fi
  sleep 0.5
}

stop_router() {
  kill "$ROUTER_PID" 2>/dev/null || true
  wait "$ROUTER_PID" 2>/dev/null || true
  ROUTER_PID=""
}

# before: batching off
start_router 0
run_scenario off 10 "$WORK/before-10.json"
run_scenario off 1 "$WORK/before-1.json"
stop_router

# after: batching on
start_router 1
run_scenario on 10 "$WORK/after-10.json"
run_scenario on 1 "$WORK/after-1.json"
stop_router

# ---- combine -------------------------------------------------------------
DATE="$(date -Iseconds)"
python3 - "$WORK" "$DATE" "$ROOT" <<'PYEOF'
import json, os, platform, subprocess, sys

work, date, root = sys.argv[1], sys.argv[2], sys.argv[3]
def load(name):
    with open(os.path.join(work, name)) as f:
        return json.load(f)

out = {
    "rig": {
        "os": platform.system() + " " + platform.release() + " " + platform.machine(),
        "cpus": os.cpu_count(),
        "mem_total_kib": int(open("/proc/meminfo").readline().split()[1]),
        "cpu_model": [l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")][0],
        "go_version": subprocess.run(["go", "version"], capture_output=True, text=True).stdout.strip(),
        "loadavg": open("/proc/loadavg").read().split()[:3],
        "date": date,
        "note": "Shared VM (1.5 shared vCPU budget), not a dedicated box; one run per cell, 200 tokens each — a single sample, not a distribution over runs.",
    },
    "date": date,
    "batching": {"max_tokens": 3, "max_delay_ms": 8},
    "tcp_nodelay": {
        "verified": True,
        "note": "TCP_NODELAY verified set on both socket paths by GetsockoptInt probe (see provider/nodelay_test.go, server/nodelay_listener_test.go); Go's stdlib also sets it by default (net/tcpsock.go newTCPConn).",
    },
    "scenarios": [
        {"cadence_ms": 10, "tokens": 200,
         "before": load("before-10.json"), "after": load("after-10.json")},
        {"cadence_ms": 1, "tokens": 200,
         "before": load("before-1.json"), "after": load("after-1.json")},
    ],
    "note": "Per-token delivery latency = client receive time minus the mock's emit timestamp embedded in the token (same machine clock). Batching is a throughput trade, not a latency win: at a 10 ms cadence each token waits the full 8 ms timer (its successor is 10 ms away, so the 3-token bound never fires first); at a 1 ms cadence the 3-token bound fires first and the wait is ~2 ms. No latency target exists for token delivery, so no cell is scored MET/MISSED.",
}

with open(os.path.join(work, "sse-results.json"), "w") as f:
    json.dump(out, f, indent=2)

dest = os.path.join(root, "docs", "sse-delivery-%s.json" % date[:10])
with open(dest, "w") as f:
    json.dump(out, f, indent=2)
print("repo record: %s" % dest)

sse_out = os.environ.get("CODVIAN_SSE_OUT", "")
if sse_out:
    try:
        import shutil
        shutil.copyfile(os.path.join(work, "sse-results.json"), sse_out)
        print("streaming copy: %s" % sse_out)
    except OSError as e:
        print("streaming copy failed (%s): %s" % (sse_out, e), file=sys.stderr)

for sc in out["scenarios"]:
    print("\ncadence %(cadence_ms)s ms, %(tokens)s tokens:" % sc)
    for k, label in (("before", "off"), ("after", "on")):
        d = sc[k]
        print("  batching %(label)s: median %(median_ms)s ms, p99 %(p99_ms)s ms, mean %(mean_ms)s ms, max %(max_ms)s ms, received %(received)s" % dict(d, label=label))
print("\ncombined results: %s/sse-results.json" % work)
PYEOF
echo "logs kept in $WORK/logs/ (router stdout incl. the sse delivery line)"
