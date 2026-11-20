#!/usr/bin/env python3
"""Live smoke for the LiteLLM failover cluster (CHECKLIST 243-244).

Real binary + two Python mocks (one flaky, one always live). No fake
data: every assertion reads a live HTTP status, a live hit count, a
live recorded upstream request body, a live feed, or the live
dashboard HTML.

Scenarios (in order; the router restarts once, for the RPM=1 key):
  F1 pin escape (a): root Responses create on flaky, then flaky 429s
     on the pinned continuation -> live serves it in the same request
     (200), one failed attempt on flaky + one hit on live, and the
     guardrails feed shows pin_escapes >= 1 with last_pin flaky and
     last_escaped_to live.
  F2 no replay (b): root create whose input is a lone function_call
     item (call_smoke, no result) -> the continuation's upstream
     request carries NO tool call call_smoke (stripped from the
     fallback request), the responses feed shows stripped_tool_calls
     >= 1, and the stored transcript still holds the call.
  F3 session counting (f): the continuation's responses-feed entry
     carries session_tokens > 0 (the turn's tokens accumulated).
  F4 stream error names the deployment (h): a stream that emits 10
     deltas then closes abruptly mid-stream ends with an SSE error
     frame naming "deployment":"flaky" (after the deltas already
     reached the client) and stream_errors_named >= 1 on the feed.
  F5 dashboard (244): the served page has the Failover card and the
     Session tokens column, no Playground.
  F6 early refusal (g), after a restart with the key's RPM set to 1:
     one valid request is admitted (200, consumes the slot); a second
     request with a MALFORMED body answers 429 (not 400) — admission
     ran before the body was read — the engine is hit exactly once,
     and the guardrails feed shows early_refusals >= 1.
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/failover-243-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21751
FLAKY_PORT = 21741
LIVE_PORT = 21742
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MODE_FILE = os.path.join(HOME, "flaky.mode")

MOCK_PY = r'''
import json, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); mode = sys.argv[2]; hits_file = sys.argv[3]
mode_file = sys.argv[4] if len(sys.argv) > 4 else ""
body_file = sys.argv[5] if len(sys.argv) > 5 else ""
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def _mode(self):
        if mode != "flaky": return "live"
        try:
            with open(mode_file) as f: return f.read().strip()
        except Exception: return "live"
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit()
        if body_file:
            with open(body_file, "wb") as f: f.write(raw)
        m = self._mode()
        try: req = json.loads(raw or b"{}")
        except Exception: req = {}
        if m == "dead":
            body = json.dumps({"error": {"message": "slow down", "type": "rate_limit_error", "code": "rate_limit_exceeded"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        if m == "streamfail" and req.get("stream"):
            # 10 SSE deltas (the engine commits after 8), then the
            # connection dies mid-body: Content-Length lies, so the
            # client sees a truncated stream and errors after the
            # deltas were already delivered.
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", "1000000")
            self.end_headers()
            for i in range(10):
                ev = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion.chunk",
                    "created": 1, "model": "mock-model",
                    "choices": [{"index": 0, "delta": {"content": "tok%d " % i},
                                 "finish_reason": None}]})
                self.wfile.write(("data: " + ev + "\n\n").encode())
                self.wfile.flush()
                time.sleep(0.02)
            self.close_connection = True
            return
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
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

def req(method, path, body=None, key=None, raw_body=None):
    data = raw_body.encode() if raw_body is not None else (json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)

def hits(f):
    try:
        with open(f) as fh: return int(fh.read().strip() or 0)
    except Exception: return 0

def last_body(f):
    try:
        with open(f, "rb") as fh: return json.loads(fh.read() or b"{}")
    except Exception:
        return {}

def set_flaky(mode):
    open(MODE_FILE, "w").write(mode)

def wait_ready(proc_env):
    for _ in range(50):
        st, _ = req("GET", "/healthz")
        if st == 200: return True
        time.sleep(0.2)
    return False

def main():
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
    check("F0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    flaky_hits = os.path.join(HOME, "flaky.hits"); live_hits = os.path.join(HOME, "live.hits")
    flaky_body = os.path.join(HOME, "flaky.body"); live_body = os.path.join(HOME, "live.body")
    open(flaky_hits, "w").write("0"); open(live_hits, "w").write("0")
    set_flaky("live")
    procs = []
    for port, mode, hf, bf in ((FLAKY_PORT, "flaky", flaky_hits, flaky_body),
                               (LIVE_PORT, "live", live_hits, live_body)):
        p = subprocess.Popen([sys.executable, mock_py, str(port), mode, hf, MODE_FILE, bf],
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
        if not wait_ready(env):
            print("ROUTER never became ready"); return 1
        for name, port in (("flaky", FLAKY_PORT), ("live", LIVE_PORT)):
            st, body = req("POST", "/api/providers",
                           {"name": name, "base_url": f"http://127.0.0.1:{port}",
                            "api_key": name + "-key"},
                           key=raw_key)
            check(f"F0c add provider {name}", st == 201, f"status={st} {body[:160]}")

        # F1: pin escape — root on flaky, continuation pins flaky,
        # flaky 429s, live serves in the same request.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first",
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj1 = json.loads(body)
        except Exception: obj1 = {}
        rid1 = obj1.get("id", "")
        check("F1 root create on flaky (200, resp_ id)",
              st == 200 and rid1.startswith("resp_"),
              f"status={st} {body[:200]}")
        set_flaky("dead")
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "second turn",
                        "previous_response_id": rid1,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj2 = json.loads(body)
        except Exception: obj2 = {}
        check("F1b pinned continuation escapes flaky 429 to live (200)",
              st == 200 and obj2.get("id", "").startswith("resp_")
              and obj2.get("previous_response_id") == rid1,
              f"status={st} {body[:200]}")
        check("F1c one failed pinned attempt on flaky, one hit on live",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before + 1,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")
        st, body = req("GET", "/dashboard/guardrails")
        try: grd = json.loads(body)
        except Exception: grd = {}
        fo = grd.get("failover_243") or {}
        check("F1d guardrails feed: pin_escapes >= 1, flaky -> live",
              fo.get("pin_escapes", 0) >= 1 and fo.get("last_pin") == "flaky"
              and fo.get("last_escaped_to") == "live",
              f"failover_243={fo}")
        set_flaky("live")

        # F2: no replay — root whose input is a lone delivered tool
        # call (no result); the continuation must not carry it upstream.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model",
                        "input": [{"type": "function_call", "call_id": "call_smoke",
                                   "name": "get_weather", "arguments": "{}"}],
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj3 = json.loads(body)
        except Exception: obj3 = {}
        rid3 = obj3.get("id", "")
        check("F2 root create with a lone delivered tool call (200)",
              st == 200 and rid3.startswith("resp_"),
              f"status={st} {body[:200]}")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "continue",
                        "previous_response_id": rid3,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        check("F2b continuation of the tool-call response (200)",
              st == 200, f"status={st} {body[:200]}")
        msgs = last_body(flaky_body).get("messages", [])
        saw_call = any(tc.get("id") == "call_smoke"
                       for m in msgs for tc in (m.get("tool_calls") or []))
        check("F2c upstream continuation carries NO delivered call call_smoke",
              not saw_call, f"messages={json.dumps(msgs)[:400]}")
        st, body = req("GET", "/dashboard/responses")
        try: feed = json.loads(body)
        except Exception: feed = {}
        check("F2d responses feed: stripped_tool_calls >= 1",
              feed.get("stripped_tool_calls", 0) >= 1,
              f"stripped_tool_calls={feed.get('stripped_tool_calls')}")
        # F2e: a SECOND continuation strips the call again — the
        # stored transcript kept it (had the strip rewritten history,
        # there would be nothing left to strip the second time).
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "continue again",
                        "previous_response_id": rid3,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        st, body = req("GET", "/dashboard/responses")
        try: feed = json.loads(body)
        except Exception: feed = {}
        check("F2e stored transcript keeps the call: a second continuation strips it again (stripped_tool_calls >= 3)",
              feed.get("stripped_tool_calls", 0) >= 3,
              f"stripped_tool_calls={feed.get('stripped_tool_calls')}")

        # F3: session counting — the continuation's feed entry carries
        # cumulative session tokens.
        st, body = req("GET", "/dashboard/responses")
        try: feed = json.loads(body)
        except Exception: feed = {}
        entries = {e.get("id"): e for e in feed.get("responses", [])}
        cont = entries.get(obj2.get("id", ""), {})
        check("F3 continuation entry carries session_tokens > 0",
              cont.get("session_tokens", 0) > 0,
              f"session_tokens={cont.get('session_tokens')}")

        # F4: stream error names the deployment — 10 deltas, then the
        # stream dies; the error frame must name flaky.
        set_flaky("streamfail")
        st, body = req("POST", "/v1/chat/completions",
                       {"model": "mock-model",
                        "messages": [{"role": "user", "content": "hello"}],
                        "stream": True,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        check("F4 stream fails after content with deployment named",
              st == 200 and '"deployment":"flaky"' in body
              and "upstream provider error" in body and "tok0" in body,
              f"status={st} {body[:400]}")
        st, body = req("GET", "/dashboard/guardrails")
        try: grd = json.loads(body)
        except Exception: grd = {}
        check("F4b guardrails feed: stream_errors_named >= 1",
              grd.get("stream_errors_named", 0) >= 1,
              f"stream_errors_named={grd.get('stream_errors_named')}")
        set_flaky("live")

        # F5: dashboard shows the failover card + session column.
        st, body = req("GET", "/dashboard")
        check("F5 dashboard has the Failover card + Session tokens column, no Playground",
              st == 200 and "Failover — pins escape" in body
              and "Session tokens" in body and "Playground" not in body,
              f"status={st} len={len(body)}")

        # F6: early refusal — restart with the key capped at RPM 1.
        router.terminate()
        try: router.wait(timeout=10)
        except Exception: router.kill()
        procs.remove(router)
        cfg = json.load(open(cfg_path))
        for k in cfg.get("gateway_keys", []):
            k["requests_per_minute"] = 1
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        procs.append(router)
        if not wait_ready(env):
            print("ROUTER never became ready after restart"); return 1
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, body = req("POST", "/v1/chat/completions",
                       {"model": "mock-model",
                        "messages": [{"role": "user", "content": "hello"}],
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        check("F6 first request admitted (200, consumes the RPM slot)",
              st == 200, f"status={st} {body[:160]}")
        st, body = req("POST", "/v1/chat/completions", None, key=raw_key,
                       raw_body='{"model":')
        check("F6b malformed body + over-traffic key -> 429, not 400 (admission before I/O)",
              st == 429, f"status={st} {body[:200]}")
        check("F6c engine hit exactly once across both requests",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")
        st, body = req("GET", "/dashboard/guardrails")
        try: grd = json.loads(body)
        except Exception: grd = {}
        check("F6d guardrails feed: early_refusals >= 1",
              grd.get("early_refusals", 0) >= 1,
              f"early_refusals={grd.get('early_refusals')}")
    finally:
        for p in procs:
            try: p.terminate()
            except Exception: pass
        for p in procs:
            try: p.wait(timeout=5)
            except Exception:
                try: p.kill()
                except Exception: pass
    bad = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(bad)}/{len(checks)} checks passed")
    return 1 if bad else 0

if __name__ == "__main__":
    sys.exit(main())
