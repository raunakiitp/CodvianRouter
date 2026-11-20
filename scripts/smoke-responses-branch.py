#!/usr/bin/env python3
"""Live smoke for Responses branching / tree (CHECKLIST 237-238).

Real binary + two Python mocks (one flaky, one always live). No fake
data: every assertion reads a live HTTP status, a live hit count, a
live recorded upstream request body, a live feed, or the live
dashboard HTML.

Scenarios:
  B0 tree endpoint on a fresh server: roots [], total 0 (honest empty state)
  B1 root create on the healthy first provider mints resp_ id
  B2 branch 1 off the root: 200, previous_response_id echoed, and the
     serving mock received the parent transcript + the new turn
     ([user first, assistant <root reply>, user second branch])
  B3 branch 2 off the same root while the pinned provider 429s:
     fails over mid-branch to live in the same request (200), and
     live received the same branched input ([first, root reply,
     third branch]) — both children keep the parent's history
  B4 GET /v1/responses/{root} lists child_response_ids [child1, child2]
  B5 GET /dashboard/responses/tree shows root -> [child1, child2]
     (total 3, branched 2)
  B6 unknown previous_response_id -> 404 naming the id
  B7 dashboard serves the Response tree section, no Playground
  B8 delete child1: tree total 2, root keeps only child2
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/responses-branch-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21641
FLAKY_PORT = 21631
LIVE_PORT = 21632
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MODE_FILE = os.path.join(HOME, "flaky.mode")

MOCK_PY = r'''
import json, sys
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
    def _dead(self):
        if mode != "flaky": return False
        try:
            with open(mode_file) as f: return f.read().strip() == "dead"
        except Exception: return False
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
        if self._dead():
            body = json.dumps({"error": {"message": "slow down", "type": "rate_limit_error", "code": "rate_limit_exceeded"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
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
    check("B0 real binary builds", True)

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
        for _ in range(50):
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1

        for name, port in (("flaky", FLAKY_PORT), ("live", LIVE_PORT)):
            st, body = req("POST", "/api/providers",
                           {"name": name, "base_url": f"http://127.0.0.1:{port}",
                            "api_key": name + "-key"},
                           key=raw_key)
            check(f"B0c add provider {name}", st == 201, f"status={st} {body[:160]}")

        # B0: honest empty tree before any response exists.
        st, body = req("GET", "/dashboard/responses/tree")
        try: tree0 = json.loads(body)
        except Exception: tree0 = {}
        check("B0 tree endpoint empty on a fresh server (roots [], total 0)",
              st == 200 and tree0.get("roots") == [] and tree0.get("total") == 0
              and tree0.get("branched") == 0, f"status={st} {body[:200]}")

        # B1: root create on flaky (healthy), fixed chain order.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first",
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj1 = json.loads(body)
        except Exception: obj1 = {}
        rid1 = obj1.get("id", "")
        check("B1 root create mints resp_ id on the first provider",
              st == 200 and rid1.startswith("resp_") and obj1.get("status") == "completed",
              f"status={st} {body[:200]}")
        check("B1b flaky served it (hit +1), live untouched",
              hits(flaky_hits) == 1 and hits(live_hits) == 0,
              f"flaky={hits(flaky_hits)} live={hits(live_hits)}")

        # B2: branch 1 off the root; flaky still healthy, serves it.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "second branch",
                        "previous_response_id": rid1,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj2 = json.loads(body)
        except Exception: obj2 = {}
        rid2 = obj2.get("id", "")
        check("B2 branch 1 off the root (200, previous_response_id echoed)",
              st == 200 and rid2.startswith("resp_") and rid2 != rid1
              and obj2.get("previous_response_id") == rid1,
              f"status={st} {body[:200]}")
        msgs = last_body(flaky_body).get("messages", [])
        want = [("user", "first"), ("assistant", "smoke says hi"), ("user", "second branch")]
        got = [(m.get("role"), m.get("content")) for m in msgs]
        check("B2b branch 1 sent the parent transcript + new turn upstream",
              got == want, f"messages={got}")

        # B3: branch 2 off the same root while flaky 429s: the
        # branched request fails over mid-branch to live.
        set_flaky("dead")
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "third branch",
                        "previous_response_id": rid1,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj3 = json.loads(body)
        except Exception: obj3 = {}
        rid3 = obj3.get("id", "")
        check("B3 branch 2 fails over mid-branch (flaky 429 -> live 200)",
              st == 200 and rid3.startswith("resp_") and rid3 != rid1 and rid3 != rid2
              and obj3.get("previous_response_id") == rid1,
              f"status={st} {body[:200]}")
        check("B3b one pinned attempt, then one fallback hit",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before + 1,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")
        msgs = last_body(live_body).get("messages", [])
        want = [("user", "first"), ("assistant", "smoke says hi"), ("user", "third branch")]
        got = [(m.get("role"), m.get("content")) for m in msgs]
        check("B3c failover target got the same branched input",
              got == want, f"messages={got}")
        set_flaky("live")

        # B4: the parent retrieve names both children.
        st, body = req("GET", f"/v1/responses/{rid1}", key=raw_key)
        try: got_kids = json.loads(body).get("child_response_ids", [])
        except Exception: got_kids = None
        check("B4 retrieve root lists child_response_ids [child1, child2]",
              st == 200 and got_kids == [rid2, rid3],
              f"status={st} children={got_kids}")

        # B5: the tree endpoint shows root -> [child1, child2].
        st, body = req("GET", "/dashboard/responses/tree")
        try: tree = json.loads(body)
        except Exception: tree = {}
        roots = tree.get("roots", [])
        kids = roots[0].get("children", []) if roots else []
        check("B5 tree endpoint: root with two branches (total 3, branched 2)",
              st == 200 and tree.get("total") == 3 and tree.get("branched") == 2
              and len(roots) == 1 and roots[0].get("id") == rid1
              and [k.get("id") for k in kids] == [rid2, rid3],
              f"status={st} {body[:300]}")

        # B6: unknown parent is an honest 404 naming the id.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "x",
                        "previous_response_id": "resp_nope"}, key=raw_key)
        check("B6 unknown previous_response_id -> 404 naming the id",
              st == 404 and "resp_nope" in body, f"status={st} {body[:160]}")

        # B7: dashboard serves the tree section, no Playground.
        st, body = req("GET", "/dashboard")
        check("B7 dashboard has the Response tree section, no Playground",
              st == 200 and "Response tree" in body and "responsesTree" in body
              and "Playground" not in body,
              f"status={st} len={len(body)}")

        # B8: delete child1; the tree prunes it, root keeps child2.
        st, body = req("DELETE", f"/v1/responses/{rid2}", key=raw_key)
        st, body = req("GET", "/dashboard/responses/tree")
        try: tree = json.loads(body)
        except Exception: tree = {}
        roots = tree.get("roots", [])
        kids = roots[0].get("children", []) if roots else []
        check("B8 delete child1 prunes the tree (total 2, root -> [child2])",
              tree.get("total") == 2 and len(roots) == 1
              and [k.get("id") for k in kids] == [rid3],
              f"tree={body[:300]}")
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
