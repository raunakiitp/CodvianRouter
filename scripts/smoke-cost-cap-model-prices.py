#!/usr/bin/env python3
"""Live smoke for cost-cap model-level prices (CHECKLIST 319-320).

Real binary + one Python mock. No fake data: every assertion reads
a live HTTP status, a live hit count, or the live plugins feed.

Setup: plugins.enabled=["cost-cap"], cost_cap_usd_per_request=0.02,
model-level price for mock-model $5 in / $10 out per 1M tokens, and
a PAIR price only for pair-model ($1 in / $2 out, under provider id
"some-provider"). The pair price is keyed by a placeholder provider
id on purpose: the feed's has_pair_price keys on the MODEL (any pair
in the price book), and the refusal does not depend on the pair at
all — a pair price never satisfies the cap, whatever provider it
names. The mock serves both models (its /v1/models lists them).

Estimates (local tiktoken prompt count + assumed completion):
  - mock-model "hi": ~7 prompt tokens x $5/1M + 1024 assumed x
    $10/1M = ~$0.0103 < $0.02 -> allowed.
  - mock-model 600x sentence: ~$0.040 > $0.02 -> denied 402.
  - pair-model "hi": no model-level price -> denied 402 naming the
    missing price, even though a pair price exists for it.

Scenarios:
  C0 real binary builds, router ready, provider added
  C1 pair-model 402 naming the missing model-level price, mock hits 0
  C2 mock-model long prompt 402 naming estimated cost + cap, hits 0
  C3 mock-model short prompt 200, content verbatim, mock hit once
  C4 GET /dashboard/plugins cost_cap: enabled, cap 0.02, 2 models
     checked, unpriced_models == [pair-model, has_pair_price true]
  C5 served /dashboard HTML has the cost-cap card + Pair price column
  C6 unauthenticated chat -> 401
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/cost-cap-prices-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21561
MOCK_PORT = 21562
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); hits_file = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [
            {"id": "mock-model", "object": "model"},
            {"id": "pair-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(ln)
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
    check("C0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    hits_file = os.path.join(HOME, "mock.hits")
    open(hits_file, "w").write("0")
    procs = []
    p = subprocess.Popen([sys.executable, mock_py, str(MOCK_PORT), hits_file],
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
    cfg["prices"] = {"mock-model": {"input_usd_per_1m": 5, "output_usd_per_1m": 10}}
    cfg["provider_prices"] = {"some-provider": {"pair-model": {"input_usd_per_1m": 1, "output_usd_per_1m": 2}}}
    cfg["plugins"] = {"enabled": ["cost-cap"], "cost_cap_usd_per_request": 0.02}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    procs.append(router)
    try:
        for _ in range(50):
            st, _, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1
        check("C0b router ready (/healthz 200)", True)

        st, _, body = req("POST", "/api/providers",
                          {"name": "mock", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
                           "api_key": "mock-key"}, key=raw_key)
        check("C0c add provider mock", st == 201, f"status={st} {body[:160]}")

        # C1: pair-price-only model refused 402 naming the missing
        # model-level price, zero provider calls.
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "pair-model",
                           "messages": [{"role": "user", "content": "hi"}]}, key=raw_key)
        check("C1 pair-model 402 naming missing model-level price, mock hits 0",
              st == 402 and "no complete model-level price" in body and "pair-model" in body
              and hits(hits_file) == 0,
              f"status={st} hits={hits(hits_file)} {body[:240]}")

        # C2: long prompt on the priced model denied 402 over cap.
        long = "The quick brown fox jumps over the lazy dog. " * 600
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "mock-model",
                           "messages": [{"role": "user", "content": long}]}, key=raw_key)
        check("C2 long prompt 402 naming estimated cost + cap, mock hits unchanged",
              st == 402 and "estimated cost $" in body and "cap $0.020000" in body
              and hits(hits_file) == 0,
              f"status={st} hits={hits(hits_file)} {body[:240]}")

        # C3: short prompt on the priced model allowed, mock hit once.
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "mock-model",
                           "messages": [{"role": "user", "content": "hi"}]}, key=raw_key)
        try: obj = json.loads(body)
        except Exception: obj = {}
        content = (obj.get("choices") or [{}])[0].get("message", {}).get("content")
        check("C3 short prompt 200, content verbatim, mock hit once",
              st == 200 and content == "smoke says hi" and hits(hits_file) == 1,
              f"status={st} hits={hits(hits_file)} {body[:160]}")

        # C4: plugins feed cost_cap block names pair-model.
        st, _, body = req("GET", "/dashboard/plugins")
        try: feed = json.loads(body)
        except Exception: feed = {}
        cc = feed.get("cost_cap", {})
        unpriced = cc.get("unpriced_models", [])
        ok = (st == 200 and cc.get("enabled") is True
              and cc.get("cap_usd_per_request") == 0.02
              and cc.get("models_checked") == 2
              and unpriced == [{"model": "pair-model", "has_pair_price": True}])
        check("C4 feed cost_cap: enabled, cap 0.02, 2 checked, unpriced=[pair-model, pair=true]",
              ok, f"status={st} cost_cap={json.dumps(cc)[:400]}")

        # C5: served dashboard HTML carries the cost-cap card.
        st, _, body = req("GET", "/dashboard")
        check("C5 dashboard HTML has the cost-cap card + Pair price column",
              st == 200 and "Cost-cap — models without a model-level price" in body
              and "<th>Pair price?</th>" in body, f"status={st}")

        # C6: unauthenticated chat -> 401.
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": "mock-model",
                           "messages": [{"role": "user", "content": "hi"}]})
        check("C6 unauthenticated chat -> 401", st == 401, f"status={st}")
    finally:
        for pr in procs:
            pr.terminate()
        for pr in procs:
            try: pr.wait(timeout=5)
            except Exception: pr.kill()
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
