#!/usr/bin/env python3
"""Live smoke for the stream-lifetime cap + (model, effort) groups (CHECKLIST 255-256).

Real binary + two Python mocks. No fake data: every assertion reads a
live header, a live feed, a live counter, or a live HTTP status.

Mocks: "stall" serves model stall-m — on a streaming chat it sends one
SSE chunk then holds the connection open forever (the held-stream shape
the cap exists for). "fast" serves model fast-m — its first chat POST
answers 429 (so a group addressed to it fails over inside the group)
and later POSTs answer 200; it records every chat request body so the
effort each attempt carried can be read back.

Flow (one server run for the checks, after a restart that applies the
routing config):
  B1  feed: GET /dashboard/stream-lifetime reports the configured
      2 s cap, enabled, 0 killed.
  B2  streaming chat to stall-m returns within ~5 s (not a hang):
      the streamed chunk, then an SSE error event naming the cap,
      then [DONE] — and the feed's killed_total is now 1.
  B3  group chat (routing.group "efforts", model fast-m) succeeds via
      the second entry: the mock saw reasoning_effort "low" then
      "high" — two distinct entries, not one deduped model.
  B4  the Live Routing trace names effort low and high on the two
      attempts.
  B5  the groups feed lists "efforts" with two members, efforts
      low and high, same provider.
  B6  a chat with reasoning_effort "bogus" is a 400 naming the field.
  B7  the served dashboard HTML carries the stream-lifetime line and
      the effort-chip loader (the dashboard surface ships).
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-255-256")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21451
STALL_PORT = 21452
FAST_PORT = 21453
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; model = sys.argv[3]
hits_file = sys.argv[4]; bodies_file = sys.argv[5]; stall = (sys.argv[6] == "stall")
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def _bump(self, fn):
        try:
            with open(fn) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(fn, "w") as f: f.write(str(n + 1))
        return n + 1
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.send_header("Content-Length", "0"); self.end_headers()
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln) if ln else b""
        n = self._bump(hits_file)
        with open(bodies_file, "a") as f: f.write(raw.decode(errors="replace") + "\n")
        try: req = json.loads(raw.decode() or "{}")
        except Exception: req = {}
        if stall and req.get("stream"):
            # One chunk, then hold the connection open forever.
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            chunk = ('data: {"id":"x","object":"chat.completion.chunk","model":"%s",'
                     '"choices":[{"index":0,"delta":{"role":"assistant","content":"held-"},'
                     '"finish_reason":null}]}\n\n' % model).encode()
            try:
                self.wfile.write(chunk); self.wfile.flush()
            except Exception:
                return
            # Hold until the client goes away (or 30 s, whichever
            # first) — the router's cap ends the request at 2 s;
            # this thread just lingers until the harness kills it.
            time.sleep(30)
            return
        if name == "fast" and req.get("reasoning_effort") == "low" and self.path.endswith("/chat/completions"):
            # The low-effort attempt 429s: the group must fail over
            # to its second entry (same model, high effort) in the
            # same request. Keyed on the effort in the body, not on
            # a hit count — a verification probe must not desync it.
            body = json.dumps({"error": {"message": "rate limited", "type": "rate_limit_error"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": name + " says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
'''

checks = []
def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

def req(method, path, body=None, key=None, timeout=30, raw_body=None):
    data = raw_body if raw_body is not None else (json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    except Exception as e:
        return 0, {}, str(e)

def stream_chat(model, key, timeout=15):
    """POST a streaming chat, reading the whole SSE body. Returns (status, body, seconds)."""
    data = json.dumps({"model": model, "messages": [{"role": "user", "content": "hi"}], "stream": True}).encode()
    r = urllib.request.Request(BASE + "/v1/chat/completions", data=data, method="POST")
    r.add_header("Content-Type", "application/json")
    r.add_header("Authorization", "Bearer " + key)
    start = time.time()
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.read().decode(), time.time() - start
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), time.time() - start
    except Exception as e:
        return 0, str(e), time.time() - start

def start_router(env):
    p = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                         env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for _ in range(50):
        st, _, _ = req("GET", "/healthz")
        if st == 200:
            return p
        time.sleep(0.2)
    print("ROUTER never became ready")
    try: print(p.stdout.read())
    except Exception: pass
    return None

def stop_router(p):
    p.terminate()
    try: p.wait(timeout=10)
    except Exception: p.kill()

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
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1
    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    procs = []
    files = {}
    for port, name, model, stall in ((STALL_PORT, "stall", "stall-m", "stall"),
                                     (FAST_PORT, "fast", "fast-m", "fast")):
        hf = os.path.join(HOME, name + ".hits"); open(hf, "w").write("0")
        bf = os.path.join(HOME, name + ".bodies"); open(bf, "w").write("")
        files[name] = (hf, bf)
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, model, hf, bf, stall],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        procs.append(p)
    r = subprocess.run([BIN, "init"], env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("INIT FAILED:\n" + r.stdout + r.stderr)
        return 1
    raw_key = None
    for tok in r.stdout.replace('"', " ").split():
        if tok.startswith("crk_"):
            raw_key = tok
    if not raw_key:
        print("INIT did not print a key:\n" + r.stdout)
        return 1
    cfg_path = None
    for root, _, fs in os.walk(HOME):
        for f in fs:
            if f == "config.json":
                cfg_path = os.path.join(root, f)
    if not cfg_path:
        print("config.json not found under " + HOME)
        return 1

    router = start_router(env)
    if router is None:
        return 1
    procs.append(router)
    try:
        ids = {}
        for name, port in (("stall", STALL_PORT), ("fast", FAST_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}",
                               "api_key": name + "-key"}, key=raw_key)
            check(f"A0 add provider {name}", st == 201, f"status={st} {body[:160]}")
            try: ids[name] = json.loads(body).get("id", name)
            except Exception: ids[name] = name
        fast_id = ids["fast"]

        # Apply the routing config: 2 s lifetime cap + the efforts group.
        stop_router(router)
        procs.remove(router)
        with open(cfg_path) as f:
            cfg = json.load(f)
        cfg["routing"] = {"stream_max_lifetime_ms": 2000,
                          "groups": {"efforts": [fast_id + "@low", fast_id + "@high"]}}
        with open(cfg_path, "w") as f:
            json.dump(cfg, f, indent=2)
        router = start_router(env)
        if router is None:
            return 1
        procs.append(router)

        # ---- B1: the feed reports the configured cap, 0 killed ----
        st, _, body = req("GET", "/dashboard/stream-lifetime")
        try: feed = json.loads(body)
        except Exception: feed = {}
        check("B1 feed reports the 2s cap, enabled, 0 killed",
              st == 200 and feed.get("enabled") is True and feed.get("max_lifetime_ms") == 2000
              and feed.get("killed_total") == 0,
              f"status={st} feed={feed}")

        # ---- B2: the held stream is killed at the cap ----
        st, sse, secs = stream_chat("stall-m", raw_key)
        check("B2 held stream returns promptly (cap, not a hang)",
              st == 200 and 1.0 < secs < 8.0, f"status={st} secs={secs:.2f}")
        check("B2 streamed chunk arrived before the cap",
              "held-" in sse, f"body={sse[:200]}")
        check("B2 terminal error event names the cap",
              "stream lifetime exceeded" in sse, f"body={sse[:400]}")
        check("B2 stream ends with [DONE] after the error",
              "[DONE]" in sse, f"body={sse[:400]}")
        st, _, body = req("GET", "/dashboard/stream-lifetime")
        try: feed = json.loads(body)
        except Exception: feed = {}
        check("B2 feed killed_total is now 1",
              feed.get("killed_total") == 1, f"feed={feed}")

        # ---- B3: the group resolves to two distinct efforts ----
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "fast-m", "messages": [{"role": "user", "content": "hi"}],
                           "routing": {"group": "efforts"}}, key=raw_key)
        check("B3 group chat succeeds via the second entry",
              st == 200 and "fast says hi" in body, f"status={st} body={body[:200]}")
        with open(files["fast"][1]) as f:
            efforts = []
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try: efforts.append(json.loads(line).get("reasoning_effort"))
                except Exception: pass
        check("B3 upstream saw low then high (two distinct entries)",
              efforts == ["low", "high"], f"efforts={efforts}")

        # ---- B4: the trace names both efforts ----
        st, _, body = req("GET", "/dashboard/trace")
        try: trace = json.loads(body)
        except Exception: trace = {}
        trace_efforts = [a.get("effort") for a in (trace.get("attempts") or [])]
        check("B4 trace records effort low and high",
              trace_efforts == ["low", "high"], f"trace_efforts={trace_efforts}")

        # ---- B5: the groups feed shows two effort entries ----
        st, _, body = req("GET", "/dashboard/groups")
        try: groups = json.loads(body)
        except Exception: groups = {}
        glist = groups.get("groups") or []
        members = []
        for g in glist:
            if g.get("name") == "efforts":
                members = g.get("members") or []
        check("B5 groups feed shows efforts as two entries",
              len(members) == 2 and members[0].get("effort") == "low"
              and members[1].get("effort") == "high"
              and members[0].get("provider_id") == members[1].get("provider_id"),
              f"members={members}")

        # ---- B6: unknown reasoning_effort is a 400 ----
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "fast-m", "messages": [{"role": "user", "content": "hi"}],
                           "reasoning_effort": "bogus"}, key=raw_key)
        check("B6 unknown reasoning_effort is a 400 naming the field",
              st == 400 and "reasoning_effort" in body, f"status={st} body={body[:200]}")

        # ---- B7: the dashboard ships the new surfaces ----
        st, _, body = req("GET", "/dashboard")
        check("B7 dashboard carries the stream-lifetime line + effort loader",
              st == 200 and 'id="streamLifetimeLine"' in body
              and "loadStreamLifetime" in body and "effort: " in body,
              f"status={st}")
    finally:
        for p in procs:
            try: p.terminate()
            except Exception: pass
        for p in procs:
            try: p.wait(timeout=5)
            except Exception:
                try: p.kill()
                except Exception: pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
