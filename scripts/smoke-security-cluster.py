#!/usr/bin/env python3
"""Live smoke for the security cluster (CHECKLIST 245-246).

Real binary + one Python mock. No fake data: every assertion reads
a live HTTP status, a live header, a live hit count, or a live feed.

Setup: mock provider with model mock-model; the deployment pins
temperature 0.0 for mock-model (admin config outranks the client
body); the admin has NOT opted in to client Vertex overrides.

Scenarios:
  S0 real binary builds, router ready, provider added
  S1 chat with vertex_location -> 400 naming the field, mock hits 0
  S2 /v1/responses with vertex_project -> 400 naming the field
  S3 key update with guardrails ["toxicty-screen"] -> 400 naming it,
      then ["pii"] -> 200 and the key view shows pii
  S4 GET /dashboard/guard counts vertex-override=2, unknown-guardrail=1
  S5 chat temperature 0.9 -> 200, header X-Codvian-Deployment-Override
      "temperature", mock received temperature 0.0 (admin pin wins)
  S6 clean chat 200, content verbatim, mock hit increments
  S7 served /dashboard HTML carries the security-refusal rows
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/security-cluster-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21561
MOCK_PORT = 21562
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); hits_file = sys.argv[2]; last_file = sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        with open(last_file, "w") as f: f.write(raw.decode("utf-8", "replace"))
        self._hit()
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
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    except Exception as e:
        return 0, {}, str(e)

def hits(f):
    try:
        with open(f) as fh: return int(fh.read().strip() or 0)
    except Exception: return 0

def last_body(f):
    try:
        return json.loads(open(f).read())
    except Exception:
        return {}

def wait_ready():
    for _ in range(50):
        st, _, _ = req("GET", "/healthz")
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
    check("S0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    hits_file = os.path.join(HOME, "mock.hits")
    last_file = os.path.join(HOME, "mock.last")
    open(hits_file, "w").write("0")
    procs = []
    p = subprocess.Popen([sys.executable, mock_py, str(MOCK_PORT), hits_file, last_file],
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

    router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    procs.append(router)
    try:
        if not wait_ready():
            print("ROUTER never became ready"); return 1
        check("S0b router ready (/healthz 200)", True)

        st, _, body = req("POST", "/api/providers",
                          {"name": "mock", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
                           "api_key": "mock-key"}, key=raw_key)
        check("S0c add provider mock", st == 201, f"status={st} {body[:160]}")
        if st != 201: return 1

        # Pin temperature 0.0 for mock-model in the config file, then
        # restart so the router loads the pin.
        router.terminate(); router.wait(timeout=10); procs.remove(router)
        cfg = json.load(open(cfg_path))
        key_id = cfg["gateway_keys"][0]["id"]
        pinned = False
        for prov in cfg.get("providers", []):
            for m in prov.get("models", []):
                if m.get("id") == "mock-model":
                    # 0.2, not 0.0: the wire omits a zero temperature
                    # (omitempty), so a 0.0 pin is invisible on the
                    # wire even though it applies.
                    m["fixed_params"] = {"temperature": 0.2}
                    pinned = True
        if not pinned:
            print("mock-model not in config providers; cannot pin"); return 1
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        procs.append(router)
        if not wait_ready():
            print("ROUTER never became ready after restart"); return 1

        # S1: Vertex location override rejected, zero provider calls.
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "mock-model", "messages": [{"role": "user", "content": "hi"}],
                           "vertex_location": "us-east1"}, key=raw_key)
        check("S1 vertex_location 400 naming the field, mock hits 0",
              st == 400 and "vertex_location" in body and "us-east1" not in body
              and hits(hits_file) == 0,
              f"status={st} hits={hits(hits_file)} {body[:200]}")

        # S2: Vertex project override rejected on /v1/responses.
        st, _, body = req("POST", "/v1/responses",
                          {"model": "mock-model", "input": "hi",
                           "vertex_project": "my-project"}, key=raw_key)
        check("S2 responses vertex_project 400 naming the field",
              st == 400 and "vertex_project" in body and "my-project" not in body,
              f"status={st} {body[:200]}")

        # S3: unknown guardrail name on a key write fails loudly.
        st, _, body = req("POST", f"/admin/keys/{key_id}",
                          {"guardrails": ["toxicty-screen"]}, key=raw_key)
        check("S3a key update with typo'd guardrail 400 naming it",
              st == 400 and "toxicty-screen" in body, f"status={st} {body[:200]}")
        st, _, body = req("POST", f"/admin/keys/{key_id}",
                          {"guardrails": ["pii"]}, key=raw_key)
        try: upd = json.loads(body)
        except Exception: upd = {}
        check("S3b key update with registered guardrail 200, view shows pii",
              st == 200 and upd.get("key", {}).get("guardrails") == ["pii"],
              f"status={st} {body[:200]}")

        # S4: guard feed counts both refusals.
        st, _, body = req("GET", "/dashboard/guard")
        try: feed = json.loads(body)
        except Exception: feed = {}
        sr = (feed.get("status") or {}).get("security_refusals") or {}
        check("S4 guard feed: vertex-override=2, unknown-guardrail=1",
              st == 200 and sr.get("vertex-override") == 2 and sr.get("unknown-guardrail") == 1,
              f"status={st} security_refusals={sr}")

        # S5: deployment pin outranks the client body field.
        st, hdrs, body = req("POST", "/v1/chat/completions",
                             {"model": "mock-model", "messages": [{"role": "user", "content": "hi"}],
                              "temperature": 0.9}, key=raw_key)
        upstream_temp = last_body(last_file).get("temperature")
        check("S5 pin wins: 200, header names temperature, mock got 0.2",
              st == 200 and hdrs.get("X-Codvian-Deployment-Override") == "temperature"
              and upstream_temp == 0.2 and hits(hits_file) == 1,
              f"status={st} header={hdrs.get('X-Codvian-Deployment-Override')} upstream_temp={upstream_temp} hits={hits(hits_file)}")

        # S6: clean chat still works end to end. A different prompt
        # from S5, so the gateway cache cannot answer it.
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "mock-model", "messages": [{"role": "user", "content": "hi again"}]},
                          key=raw_key)
        try: obj = json.loads(body)
        except Exception: obj = {}
        content = (obj.get("choices") or [{}])[0].get("message", {}).get("content")
        check("S6 clean chat 200, content verbatim",
              st == 200 and content == "smoke says hi" and hits(hits_file) == 2,
              f"status={st} hits={hits(hits_file)} {body[:160]}")

        # S7: served dashboard HTML carries the security-refusal rows.
        st, _, body = req("GET", "/dashboard")
        check("S7 dashboard HTML has security-refusal rows",
              st == 200 and "grSecurityVertex" in body and "grSecurityGuardrail" in body
              and "Security refusals since start" in body,
              f"status={st}")
    finally:
        for pr in procs:
            try:
                pr.terminate()
            except Exception:
                pass
        for pr in procs:
            try:
                pr.wait(timeout=10)
            except Exception:
                try: pr.kill()
                except Exception: pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
