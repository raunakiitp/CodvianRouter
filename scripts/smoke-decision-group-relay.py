#!/usr/bin/env python3
"""Live smoke for CHECKLIST 217-218 (group reason server test +
relay-stream header smoke).

Real binary + two Python mocks. No fake data: every assertion reads
a live header, a live body, or a live hit count.

Mocks:
  - "live" (OpenAI wire): /v1/models lists live-model; chat answers
    JSON + SSE. /v1/messages for live-model therefore TRANSLATES
    through the engine — the path that DOES carry X-Codvian-Decision.
  - "claude" (Anthropic wire, added via catalog_id "anthropic"
    with a base_url override): /v1/models lists claude-mock;
    /v1/messages answers the Anthropic envelope (JSON + SSE), so
    requests for claude-mock relay VERBATIM — the path that must
    never carry X-Codvian-Decision.

Checks:
  G1/G2  group reason live: routing.group "fast" and the
         group:fast/live-model alias both serve from the first
         member with reason=group on the real binary.
  R1     verbatim relay, non-streaming: 200, Anthropic body,
         X-Codvian-Relay: verbatim, NO X-Codvian-Decision.
  R2     verbatim relay STREAM: 200, Anthropic SSE flows,
         X-Codvian-Relay: verbatim, NO X-Codvian-Decision.
         (The relay bypasses routing entirely, so there is no
         decision to name; the stream's headers commit with the
         first upstream byte, before any routing could happen.)
  T1     translated /v1/messages, non-streaming: 200, decision
         header present with the serving provider/model.
  T2     translated /v1/messages STREAM: 200, Anthropic SSE flows,
         NO X-Codvian-Decision — the translated stream commits its
         headers (WriteHeader + message_start) BEFORE the engine
         runs, so the decision is not known yet when the headers
         go out; it cannot be added afterwards.
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-217-218")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21441
LIVE_PORT = 21442
CLAUDE_PORT = 21443
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); kind = sys.argv[2]; hits_file = sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            if kind == "anthropic":
                body = json.dumps({"data": [{"id": "claude-mock", "type": "model", "display_name": "Claude Mock"}]}).encode()
            else:
                body = json.dumps({"object": "list", "data": [{"id": "live-model", "object": "model"}]}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit()
        try: req = json.loads(raw.decode())
        except Exception: req = {}
        if kind == "anthropic" and self.path.endswith("/v1/messages"):
            model = req.get("model", "claude-mock")
            if req.get("stream"):
                self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
                events = [
                    ("message_start", {"type": "message_start", "message": {"id": "msg-smoke", "type": "message", "role": "assistant", "model": model, "content": [], "stop_reason": None, "usage": {"input_tokens": 3, "output_tokens": 0}}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "claude-mock says hi"}}),
                    ("message_stop", {"type": "message_stop"}),
                ]
                for ev, payload in events:
                    self.wfile.write(("event: %s\ndata: %s\n\n" % (ev, json.dumps(payload))).encode())
                    self.wfile.flush()
                return
            body = json.dumps({"id": "msg-smoke", "type": "message", "role": "assistant",
                "model": model, "content": [{"type": "text", "text": "claude-mock says hi"}],
                "stop_reason": "end_turn", "usage": {"input_tokens": 3, "output_tokens": 4}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        if self.path.endswith("/chat/completions"):
            model = req.get("model", "live-model")
            if req.get("stream"):
                self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
                for text in ("Hello ", "world"):
                    chunk = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": model,
                        "choices": [{"index": 0, "delta": {"content": text}, "finish_reason": None}]}).encode()
                    self.wfile.write(b"data: " + chunk + b"\n\n"); self.wfile.flush()
                self.wfile.write(b"data: [DONE]\n\n")
                return
            body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "live says hi"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
HTTPServer(("127.0.0.1", port), H).serve_forever()
'''

checks = []
def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

def req(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    except Exception as e:
        return 0, {}, str(e)

def hits(fn):
    try:
        with open(fn) as f: return int(f.read().strip() or 0)
    except Exception: return 0

def start_router(env):
    p = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                         env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for _ in range(50):
        st, _, _ = req("GET", "/healthz")
        if st == 200: return p
        time.sleep(0.2)
    print("ROUTER never became ready"); return None

def stop_router(p):
    p.terminate()
    try: p.wait(timeout=10)
    except Exception: p.kill()

def messages(model, key, stream=False, prompt="Say hello in one short sentence."):
    body = {"model": model, "max_tokens": 64,
            "messages": [{"role": "user", "content": prompt}]}
    if stream: body["stream"] = True
    return req("POST", "/v1/messages", body, key)

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
    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    procs = []
    files = {}
    for port, kind in ((LIVE_PORT, "openai"), (CLAUDE_PORT, "anthropic")):
        hf = os.path.join(HOME, kind + ".hits")
        open(hf, "w").write("0")
        files[kind] = hf
        p = subprocess.Popen([sys.executable, mock_py, str(port), kind, hf],
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
    for root, _, fs in os.walk(HOME):
        for f in fs:
            if f == "config.json": cfg_path = os.path.join(root, f)
    if not cfg_path:
        print("config.json not found under " + HOME); return 1

    router = start_router(env)
    if router is None: return 1
    procs.append(router)
    try:
        ids = {}
        st, _, body = req("POST", "/api/providers",
                          {"base_url": f"http://127.0.0.1:{LIVE_PORT}", "api_key": "live-key"},
                          key=raw_key)
        check("A0 add OpenAI provider live", st == 201, f"status={st} {body[:160]}")
        try: ids["live"] = json.loads(body).get("id", "live")
        except Exception: ids["live"] = "live"
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "anthropic", "base_url": f"http://127.0.0.1:{CLAUDE_PORT}", "api_key": "anthropic-mock-key"},
                          key=raw_key)
        check("A1 add Anthropic provider (catalog + base_url override)", st == 201, f"status={st} {body[:160]}")
        try: ids["claude"] = json.loads(body).get("id", "claude")
        except Exception: ids["claude"] = "claude"
        live_id, claude_id = ids["live"], ids["claude"]

        # Groups live in the config file and are read at startup:
        # restart with routing.groups fast=[live].
        stop_router(router); procs.remove(router)
        with open(cfg_path) as f: cfg = json.load(f)
        cfg["routing"] = {"groups": {"fast": [live_id]}}
        with open(cfg_path, "w") as f: json.dump(cfg, f, indent=2)
        router = start_router(env)
        if router is None: return 1
        procs.append(router)

        # ---- G: group reason, live ----
        st, h, body = req("POST", "/v1/chat/completions",
                          {"model": "live-model", "messages": [{"role": "user", "content": "Group check one."}],
                           "routing": {"group": "fast"}}, raw_key)
        check("G1 routing.group chat 200 + live content",
              st == 200 and "live says hi" in body, f"status={st} body={body[:200]}")
        check("G2 group header reason=group names the first member",
              h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=group",
              f"got={h.get('X-Codvian-Decision')!r}")
        st, h, body = req("POST", "/v1/chat/completions",
                          {"model": "group:fast/live-model", "messages": [{"role": "user", "content": "Group check two."}]}, raw_key)
        check("G3 group alias header reason=group, resolved model",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=group",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        # ---- R: verbatim relay (Anthropic mock) ----
        before = hits(files["anthropic"])
        st, h, body = messages("claude-mock", raw_key)
        check("R1 verbatim relay 200 + Anthropic body, mock hit once",
              st == 200 and "claude-mock says hi" in body and hits(files["anthropic"]) == before + 1,
              f"status={st} body={body[:200]} hits={hits(files['anthropic'])} before={before}")
        check("R2 verbatim relay names itself, carries NO decision header",
              h.get("X-Codvian-Relay") == "verbatim" and h.get("X-Codvian-Decision", "") == "",
              f"relay={h.get('X-Codvian-Relay')!r} decision={h.get('X-Codvian-Decision')!r}")
        st, h, body = messages("claude-mock", raw_key, stream=True,
                               prompt="Stream a greeting from the Anthropic mock.")
        check("R3 verbatim relay STREAM 200 + Anthropic SSE flows",
              st == 200 and "event: message_start" in body and "claude-mock says hi" in body and "event: message_stop" in body,
              f"status={st} body={body[:300]}")
        check("R4 relay STREAM names itself, carries NO decision header",
              h.get("X-Codvian-Relay") == "verbatim" and h.get("X-Codvian-Decision", "") == "",
              f"relay={h.get('X-Codvian-Relay')!r} decision={h.get('X-Codvian-Decision')!r}")

        # ---- T: translated /v1/messages (OpenAI mock) ----
        st, h, body = messages("live-model", raw_key)
        check("T1 translated messages 200 + live content",
              st == 200 and "live says hi" in body, f"status={st} body={body[:200]}")
        check("T2 translated messages CARRIES the decision header",
              h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=balanced",
              f"got={h.get('X-Codvian-Decision')!r}")
        check("T3 translated relay header names the translate reason",
              (h.get("X-Codvian-Relay") or "").startswith("translated"),
              f"got={h.get('X-Codvian-Relay')!r}")
        st, h, body = messages("live-model", raw_key, stream=True,
                               prompt="Stream this greeting slowly, one word at a time.")
        check("T4 translated STREAM 200 + content flows as Anthropic SSE",
              st == 200 and "Hello " in body and "world" in body,
              f"status={st} body={body!r}")
        check("T5 translated STREAM carries NO decision header (headers commit before routing)",
              h.get("X-Codvian-Decision", "") == "",
              f"decision={h.get('X-Codvian-Decision')!r}")
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
