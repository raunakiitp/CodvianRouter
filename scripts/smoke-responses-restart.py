#!/usr/bin/env python3
"""Live restart smoke for Responses transcripts (CHECKLIST 315-316).

Real binary + one Python mock (always live). No fake data: every
assertion reads a live HTTP status, a live hit count, a live feed,
the live persistence file, or the mock's recorded request body.

Scenarios:
  T1 create mints resp_ id; retrieve works; the mock saw one user turn
  T2 responses.json exists in the data dir and holds the response id
  T3 RESTART on the same data dir: retrieve still serves 200 (not 404)
  T4 feed after restart lists the id and reports restored >= 1
  T5 tree after restart still shows the node
  T6 continuation after restart serves 200 with the prior transcript
     in context — the mock's recorded request carries the first user
     turn, the first assistant answer, and the new turn
  T7 dashboard serves the Responses blocks, no Playground
  T8 delete + second restart: the deleted response stays gone (404),
     never resurrected by the restore
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/responses-restart-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21561
MOCK_PORT = 21551
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); last_file = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        with open(last_file, "wb") as f: f.write(raw)
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
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

def req(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
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

def wait_ready():
    for _ in range(50):
        st, _ = req("GET", "/healthz")
        if st == 200:
            return True
        time.sleep(0.2)
    return False

def find_file(name):
    for root, _, files in os.walk(HOME):
        if name in files:
            return os.path.join(root, name)
    return None

def last_mock_messages(last_file):
    try:
        raw = json.load(open(last_file))
        return raw.get("messages", [])
    except Exception:
        return []

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
    check("T0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    last_file = os.path.join(HOME, "last_req.json")
    procs = []
    mock = subprocess.Popen([sys.executable, mock_py, str(MOCK_PORT), last_file],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    procs.append(mock)
    r = subprocess.run([BIN, "init"], env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("INIT FAILED:\n" + r.stdout + r.stderr); return 1
    raw_key = None
    for tok in r.stdout.replace('"', " ").split():
        if tok.startswith("crk_"): raw_key = tok
    if not raw_key:
        print("INIT did not print a key:\n" + r.stdout); return 1
    cfg_path = find_file("config.json")
    if not cfg_path:
        print("config.json not found under " + HOME); return 1
    cfg = json.load(open(cfg_path))
    cfg["prices"] = {"mock-model": {"input_usd_per_1m": 1, "output_usd_per_1m": 2}}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = None
    def start_router():
        nonlocal router
        router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        procs.append(router)
        return wait_ready()

    def stop_router():
        nonlocal router
        if router is None:
            return
        router.terminate()
        try:
            out, _ = router.communicate(timeout=10)
            open(os.path.join(HOME, "router.log"), "w").write(out or "")
        except Exception:
            router.kill()
        procs.remove(router)
        router = None
        time.sleep(0.5)

    try:
        if not start_router():
            print("ROUTER never became ready"); return 1
        check("T0b router ready (/healthz 200)", True)
        st, body = req("POST", "/api/providers",
                       {"name": "live", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
                        "api_key": "live-key"}, key=raw_key)
        check("T0c add provider live", st == 201, f"status={st} {body[:160]}")

        # T1: create on the live mock.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first turn"}, key=raw_key)
        try: obj1 = json.loads(body)
        except Exception: obj1 = {}
        rid1 = obj1.get("id", "")
        check("T1 create mints resp_ id",
              st == 200 and rid1.startswith("resp_") and obj1.get("status") == "completed",
              f"status={st} {body[:200]}")
        msgs = last_mock_messages(last_file)
        check("T1b mock saw one user turn ('first turn')",
              len(msgs) == 1 and msgs[0].get("content") == "first turn",
              f"messages={msgs}")

        # T2: the persistence file holds the response id.
        persist_path = find_file("responses.json")
        persisted = ""
        if persist_path:
            persisted = open(persist_path).read()
        check("T2 responses.json in data dir holds the response id",
              persist_path is not None and rid1 in persisted,
              f"path={persist_path}")

        # T3-T5: restart on the SAME data dir (same HOME).
        stop_router()
        if not start_router():
            print("ROUTER never became ready after restart"); return 1
        check("T3 router ready again after restart", True)
        st, body = req("GET", f"/v1/responses/{rid1}", key=raw_key)
        check("T3 retrieve after restart serves 200, same object",
              st == 200 and json.loads(body).get("id") == rid1
              and "smoke says hi" in body,
              f"status={st} {body[:200]}")
        st, body = req("GET", "/dashboard/responses", key=raw_key)
        try: feed = json.loads(body)
        except Exception: feed = {}
        ids = [e.get("id") for e in feed.get("responses", [])]
        check("T4 feed after restart lists the id, restored >= 1",
              st == 200 and rid1 in ids and feed.get("restored", 0) >= 1,
              f"status={st} restored={feed.get('restored')} stored={feed.get('stored')}")
        st, body = req("GET", "/dashboard/responses/tree", key=raw_key)
        try: tree = json.loads(body)
        except Exception: tree = {}
        tree_ids = []
        def walk(nodes):
            for n in nodes or []:
                tree_ids.append(n.get("id"))
                walk(n.get("children"))
        walk(tree.get("roots"))
        check("T5 tree after restart still shows the node",
              st == 200 and rid1 in tree_ids, f"status={st} tree_ids={tree_ids}")

        # T6: continuation after restart — 200, and the mock's
        # recorded request carries the prior transcript in
        # context (first user turn + first assistant answer +
        # new turn), proving the restored transcript was re-sent.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "second turn",
                        "previous_response_id": rid1}, key=raw_key)
        try: obj2 = json.loads(body)
        except Exception: obj2 = {}
        check("T6 continuation after restart serves 200, previous echoed",
              st == 200 and obj2.get("previous_response_id") == rid1
              and obj2.get("id", "").startswith("resp_") and obj2.get("id") != rid1,
              f"status={st} {body[:200]}")
        msgs = last_mock_messages(last_file)
        texts = [(m.get("role"), m.get("content")) for m in msgs]
        check("T6b mock request carries the restored transcript in context",
              texts == [("user", "first turn"), ("assistant", "smoke says hi"),
                        ("user", "second turn")],
              f"messages={texts}")

        # T7: dashboard serves the Responses blocks, no Playground,
        # and the JS names the restored count.
        st, body = req("GET", "/dashboard")
        check("T7 dashboard serves Responses blocks, no Playground",
              st == 200 and "responsesStats" in body and "responsesBody" in body
              and "restored after restart" in body and "Playground" not in body,
              f"status={st}")

        # T8: delete + second restart — the deleted response stays
        # gone, never resurrected by the restore.
        rid2 = obj2.get("id", "")
        st, body = req("DELETE", f"/v1/responses/{rid1}", key=raw_key)
        check("T8a delete the original response", st == 200, f"status={st} {body[:120]}")
        stop_router()
        if not start_router():
            print("ROUTER never became ready after second restart"); return 1
        st, body = req("GET", f"/v1/responses/{rid1}", key=raw_key)
        check("T8b deleted response still 404 after second restart",
              st == 404, f"status={st} {body[:120]}")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "third turn",
                        "previous_response_id": rid1}, key=raw_key)
        check("T8c continuation of deleted id still 404 after restart",
              st == 404, f"status={st} {body[:120]}")
        st, body = req("GET", f"/v1/responses/{rid2}", key=raw_key)
        check("T8d the surviving continuation still retrieves 200",
              st == 200 and json.loads(body).get("id") == rid2,
              f"status={st} {body[:160]}")

        return 0 if all(ok for _, ok, _ in checks) else 1
    finally:
        if router is not None:
            try:
                router.terminate()
                out, _ = router.communicate(timeout=5)
                open(os.path.join(HOME, "router.log"), "w").write(out or "")
            except Exception:
                router.kill()
        for p in procs:
            if p is router:
                continue
            try:
                p.terminate(); p.wait(timeout=5)
            except Exception:
                try: p.kill()
                except Exception: pass

if __name__ == "__main__":
    sys.exit(main())
