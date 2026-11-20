#!/usr/bin/env python3
"""Live smoke for the wired Responses session token cap (CHECKLIST 263-264).

Real binary + one Python mock (always live). No fake data: every
assertion reads a live HTTP status, a live hit count, a live feed,
the live dashboard HTML, or the router's own startup log.

Phase 1, cap set (responses.session_token_cap = 10):
  C1 startup log names the cap.
  C2 root create admitted (fresh sessions are never refused).
  C3 responses feed carries session_token_cap == 10 and the root
     entry's session_tokens > 0.
  C4 a long continuation (300 words, far over the 10-token cap)
     is refused with 429 session_token_limit naming the cap, and
     the mock is hit ZERO extra times (refusal before any provider
     is dialled).
  C5 responses feed shows session_admission_refused >= 1.
  C6 dashboard HTML serves the cap-rendering code (the stats line
     is rendered by JS from the same feed, so the served page must
     carry the "Session token cap: " literal and the
     session_token_cap feed field it reads).
Phase 2, cap unset (restart with the responses block removed):
  C7 startup log says counting only.
  C8 the same long continuation is ADMITTED (200) and the mock
     is hit once more.
  C9 feed shows session_token_cap == 0 and the continuation's
     session_tokens grew past the root's (counting still happens).
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/session-cap-263-smoke")
ROUTER_DIR = os.path.expanduser(os.environ.get(
    "ROUTER_DIR", "~/workspace/codvian-router"))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21761
MOCK_PORT = 21762
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
CAP = 10
LONG_INPUT = ("word " * 300).strip()

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); hits_file = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
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
    if data:
        r.add_header("Content-Type", "application/json")
    if key:
        r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


def hits(f):
    try:
        with open(f) as fh:
            return int(fh.read().strip() or 0)
    except Exception:
        return 0


def wait_ready():
    for _ in range(50):
        st, _ = req("GET", "/healthz")
        if st == 200:
            return True
        time.sleep(0.2)
    return False


def drain_log(proc):
    """Best-effort non-blocking read of what the router printed so far."""
    try:
        fd = proc.stdout.fileno()
        os.set_blocking(fd, False)
        try:
            return proc.stdout.read() or ""
        except Exception:
            return ""
    except Exception:
        return ""


def feed():
    st, body = req("GET", "/dashboard/responses")
    try:
        return json.loads(body)
    except Exception:
        return {}


def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN,
                        "./cmd/codvian-router"],
                       cwd=ROUTER_DIR, env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1
    check("C0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    mock_hits = os.path.join(HOME, "mock.hits")
    open(mock_hits, "w").write("0")
    procs = []
    mock = subprocess.Popen([sys.executable, mock_py, str(MOCK_PORT), mock_hits],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    procs.append(mock)

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
    for root, _, files in os.walk(HOME):
        for f in files:
            if f == "config.json":
                cfg_path = os.path.join(root, f)
    if not cfg_path:
        print("config.json not found under " + HOME)
        return 1
    cfg = json.load(open(cfg_path))
    cfg["prices"] = {"mock-model": {"input_usd_per_1m": 1, "output_usd_per_1m": 2}}
    cfg["responses"] = {"session_token_cap": CAP}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    def start_router():
        p = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                             env=env, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True)
        procs.append(p)
        return p

    router = start_router()
    try:
        if not wait_ready():
            print("ROUTER never became ready")
            return 1
        time.sleep(0.5)
        log = drain_log(router)
        check("C1 startup log names the cap",
              f"responses session token cap: {CAP} tokens per session" in log,
              f"log tail={log[-300:]!r}")
        st, body = req("POST", "/api/providers",
                       {"name": "live", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
                        "api_key": "live-key"}, key=raw_key)
        check("C0b add provider live", st == 201, f"status={st} {body[:160]}")

        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first",
                        "routing": {"order": ["live"]}}, key=raw_key)
        try:
            obj1 = json.loads(body)
        except Exception:
            obj1 = {}
        rid1 = obj1.get("id", "")
        check("C2 root create admitted (200, resp_ id)",
              st == 200 and rid1.startswith("resp_"),
              f"status={st} {body[:200]}")
        f = feed()
        entries = {e.get("id"): e for e in f.get("responses", [])}
        root_tokens = entries.get(rid1, {}).get("session_tokens", 0)
        check("C3 feed: session_token_cap == 10, root session_tokens > 0",
              f.get("session_token_cap") == CAP and root_tokens > 0,
              f"cap={f.get('session_token_cap')} session_tokens={root_tokens}")

        h_before = hits(mock_hits)
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": LONG_INPUT,
                        "previous_response_id": rid1,
                        "routing": {"order": ["live"]}}, key=raw_key)
        check("C4 long continuation refused 429 session_token_limit naming the cap",
              st == 429 and "session_token_limit" in body
              and f"{CAP}-token session cap" in body,
              f"status={st} {body[:240]}")
        check("C4b refused turn dialled no provider (mock hits unchanged)",
              hits(mock_hits) == h_before,
              f"hits={hits(mock_hits)} (was {h_before})")
        f = feed()
        check("C5 feed: session_admission_refused >= 1",
              f.get("session_admission_refused", 0) >= 1,
              f"session_admission_refused={f.get('session_admission_refused')}")
        st, body = req("GET", "/dashboard")
        check("C6 dashboard serves the cap-rendering code",
              st == 200 and "Session token cap: " in body
              and "session_token_cap" in body and "Playground" not in body,
              f"status={st} len={len(body)}")

        # Phase 2: restart with the cap unset — counting only.
        router.terminate()
        try:
            router.wait(timeout=10)
        except Exception:
            router.kill()
        procs.remove(router)
        cfg = json.load(open(cfg_path))
        cfg.pop("responses", None)
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = start_router()
        if not wait_ready():
            print("ROUTER never became ready after restart")
            return 1
        time.sleep(0.5)
        log = drain_log(router)
        check("C7 startup log says counting only",
              "unset (counting only" in log, f"log tail={log[-300:]!r}")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first",
                        "routing": {"order": ["live"]}}, key=raw_key)
        try:
            obj2 = json.loads(body)
        except Exception:
            obj2 = {}
        rid2 = obj2.get("id", "")
        check("C8 root create admitted after restart (200)",
              st == 200 and rid2.startswith("resp_"),
              f"status={st} {body[:200]}")
        h_before = hits(mock_hits)
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": LONG_INPUT,
                        "previous_response_id": rid2,
                        "routing": {"order": ["live"]}}, key=raw_key)
        try:
            obj3 = json.loads(body)
        except Exception:
            obj3 = {}
        check("C8b same long continuation ADMITTED with cap unset (200)",
              st == 200 and obj3.get("id", "").startswith("resp_"),
              f"status={st} {body[:200]}")
        check("C8c admitted turn dialled the provider once",
              hits(mock_hits) == h_before + 1,
              f"hits={hits(mock_hits)} (was {h_before})")
        f = feed()
        entries = {e.get("id"): e for e in f.get("responses", [])}
        cont_tokens = entries.get(obj3.get("id", ""), {}).get("session_tokens", 0)
        root2_tokens = entries.get(rid2, {}).get("session_tokens", 0)
        check("C9 feed: cap 0 and counting still happens (continuation > root)",
              f.get("session_token_cap") == 0
              and cont_tokens > root2_tokens > 0,
              f"cap={f.get('session_token_cap')} root={root2_tokens} cont={cont_tokens}")
    finally:
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        for p in procs:
            try:
                p.wait(timeout=5)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
    bad = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(bad)}/{len(checks)} checks passed")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
