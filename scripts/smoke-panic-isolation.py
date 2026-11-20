#!/usr/bin/env python3
"""Live smoke for panic isolation (CHECKLIST 165-166).

Real binary + two Python mocks (one live, one failing). No fake
data: every assertion reads a live HTTP status, a live metrics
series, a live overview feed, or the live dashboard HTML.

Honest scope: no HTTP-reachable input was found that panics the
production chunk converter (it is defensive — wire.TranslateStream
returns errors, it does not panic), so the live smoke proves the
healthy path + the honest zero-count feed + survival across a
failing first request. Panic behaviour itself is proven in-process
by the unit tests with deliberately panicking clients/hooks/engines
(internal/router/panic_test.go, internal/server/panic_test.go).
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/panic-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21341
LIVE_PORT = 21331
DEAD_PORT = 21332
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); mode = sys.argv[2]; hits_file = sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        # GETs (model probes / health checks) are NOT counted: the
        # hit file measures chat attempts only, so P4b's "dead hit
        # once, live hit once" is exact.
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit()
        if mode == "dead":
            body = json.dumps({"error": {"message": "boom", "type": "server_error"}}).encode()
            self.send_response(500); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        try:
            stream = json.loads(raw or b"{}").get("stream", False)
        except Exception:
            stream = False
        if stream:
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            for piece in ("smoke ", "stream"):
                ev = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion.chunk",
                    "created": 1, "model": "mock-model",
                    "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]}).encode()
                self.wfile.write(b"data: " + ev + b"\n\n"); self.wfile.flush()
            self.wfile.write(b"data: [DONE]\n\n"); self.wfile.flush(); return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": "mock-model",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "smoke says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
HTTPServer(("127.0.0.1", port), H).serve_forever()
'''

checks = []
def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}")

def req(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)

def hits(f):
    try:
        with open(f) as fh: return int(fh.read().strip() or 0)
    except Exception: return 0

def main():
    # Fresh isolated HOME on every run: the previous run's config
    # would make `init` refuse to start over.
    import shutil
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN, "./cmd/codvian-router"],
                       cwd=os.path.expanduser("~/workspace/codvian-router"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr); return 1
    check("P1 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    live_hits = os.path.join(HOME, "live.hits"); dead_hits = os.path.join(HOME, "dead.hits")
    open(live_hits, "w").write("0"); open(dead_hits, "w").write("0")
    procs = []
    for port, mode, hf in ((LIVE_PORT, "live", live_hits), (DEAD_PORT, "dead", dead_hits)):
        p = subprocess.Popen([sys.executable, mock_py, str(port), mode, hf],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        procs.append(p)
    r = subprocess.run([BIN, "init"], env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("INIT FAILED:\n" + r.stdout + r.stderr); return 1
    raw_key = None
    for tok in r.stdout.replace('"', " ").split():
        if tok.startswith("crk_"): raw_key = tok
    if not raw_key:
        print("INIT did not print a key:\n" + r.stdout); return 1
    cfg_path = None
    for root, _, files in os.walk(HOME):
        for f in files:
            if f == "config.json": cfg_path = os.path.join(root, f)
    if not cfg_path:
        print("config.json not found under " + HOME); return 1
    cfg = json.load(open(cfg_path))
    cfg["prices"] = {"mock-model": {"input_usd_per_1m": 1, "output_usd_per_1m": 2}}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    procs.append(router)
    try:
        for _ in range(50):
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1
        check("P2 router ready (/healthz 200)", True)

        for pid, port in (("dead", DEAD_PORT), ("live", LIVE_PORT)):
            st, body = req("POST", "/api/providers",
                           {"base_url": f"http://127.0.0.1:{port}",
                            "api_key": pid + "-key",
                            "models": [{"id": "mock-model"}]},
                           key=raw_key)
            if st != 201:
                st, body = req("POST", "/api/providers",
                               {"name": pid, "base_url": f"http://127.0.0.1:{port}",
                                "api_key": pid + "-key"}, key=raw_key)
            check(f"P3 add provider {pid}", st == 201, f"status={st} {body[:160]}")

        # Failing first request: dead (500) first in the chain, live
        # serves the fallback. The gateway survives the failure.
        st, body = req("POST", "/v1/chat/completions",
                       {"model": "mock-model", "messages": [{"role": "user", "content": "hello"}]},
                       key=raw_key)
        check("P4 failing-first request fails over to live (200)",
              st == 200 and "smoke says hi" in body, f"status={st} {body[:160]}")
        check("P4b dead hit once, live hit once",
              hits(dead_hits) == 1 and hits(live_hits) == 1,
              f"dead={hits(dead_hits)} live={hits(live_hits)}")

        # Second request still works — the process stayed up.
        st, body = req("POST", "/v1/chat/completions",
                       {"model": "mock-model", "messages": [{"role": "user", "content": "again"}]},
                       key=raw_key)
        check("P5 second request after failure still 200",
              st == 200 and "smoke says hi" in body, f"status={st} {body[:160]}")

        # Streaming works end to end.
        st, body = req("POST", "/v1/chat/completions",
                       {"model": "mock-model", "messages": [{"role": "user", "content": "stream me"}],
                        "stream": True}, key=raw_key)
        check("P6 stream 200 with content + [DONE]",
              st == 200 and "smoke " in body and "stream" in body and "[DONE]" in body,
              f"status={st} {body[:200]}")

        # Honest zero: no panic has happened, so no panics series in
        # /metrics and panics_since_start == 0 in the overview feed.
        st, body = req("GET", "/metrics", key=raw_key)
        check("P7 /metrics has no panics series before any panic",
              st == 200 and "codvian_router_panics_total{" not in body, f"status={st}")
        st, body = req("GET", "/dashboard/overview?range=24h", key=raw_key)
        ov = json.loads(body) if st == 200 else {}
        check("P8 overview panics_since_start == 0",
              st == 200 and ov.get("panics_since_start") == 0, f"status={st} panics={ov.get('panics_since_start')}")

        # Dashboard serves the panic card with its honest-zero text.
        st, body = req("GET", "/dashboard")
        check("P9 dashboard serves Recovered panics card (honest zero)",
              st == 200 and "Recovered panics" in body and "No panics recovered since start" in body,
              f"status={st}")

        # Trace feed shows the failover attempt (reason upstream_error
        # for the 500), proving the trace path the panic reason rides.
        st, body = req("GET", "/dashboard/trace", key=raw_key)
        check("P10 trace feed serves attempts", st == 200 and "attempts" in body, f"status={st}")
        return 0 if all(ok for _, ok, _ in checks) else 1
    finally:
        for p in procs:
            p.terminate()
        for p in procs:
            try: p.wait(timeout=5)
            except Exception: p.kill()
        try:
            out, _ = router.communicate(timeout=5)
            open(os.path.join(HOME, "router.log"), "w").write(out or "")
        except Exception:
            pass

if __name__ == "__main__":
    sys.exit(main())
