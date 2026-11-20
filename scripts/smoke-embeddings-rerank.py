#!/usr/bin/env python3
"""Live smoke for POST /v1/embeddings + POST /v1/rerank (CHECKLIST 171-172).

Real binary + two Python mocks (one flaky, one always live). No fake
data: every assertion reads a live HTTP status, a live hit count, a
live feed, or the live dashboard HTML.

Scenarios:
  E1 embeddings 200, vectors verbatim + usage, X-Codvian-Endpoint header
  E2 logs feed shows the embeddings row with endpoint "embeddings"
  E3 rerank 200 with relevance scores, no invented usage
  E4 trace names the endpoint + exempt:rerank on the attempt
  E5 first-target 429 on embeddings -> honest 502, exactly one attempt
     (second mock never dialled — never replayed)
  E6 same on rerank (the other surface)
  E7 guardrail block on the input text -> 403/-32001, zero provider hits
  E8 strict per-request budget the estimate exceeds -> 402, zero hits
  E9 dashboard serves the Endpoint column, no Playground
  E10 unauthenticated -> 401; invalid body -> 400
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/embeddings-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21541
FLAKY_PORT = 21531
LIVE_PORT = 21532
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MODE_FILE = os.path.join(HOME, "flaky.mode")

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); mode = sys.argv[2]; hits_file = sys.argv[3]
mode_file = sys.argv[4] if len(sys.argv) > 4 else ""
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
        if self._dead():
            body = json.dumps({"error": {"message": "slow down", "type": "rate_limit_error", "code": "rate_limit_exceeded"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        if self.path == "/v1/embeddings":
            body = json.dumps({"object": "list",
                "data": [{"object": "embedding", "embedding": [0.1, 0.2, 0.3], "index": 0}],
                "model": "mock-model",
                "usage": {"prompt_tokens": 8, "total_tokens": 8}}).encode()
        elif self.path == "/v1/rerank":
            body = json.dumps({"id": "rr-smoke", "model": "mock-model",
                "results": [{"index": 1, "relevance_score": 0.92},
                            {"index": 0, "relevance_score": 0.31}]}).encode()
        else:
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

def req(method, path, body=None, key=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    for k, v in (headers or {}).items(): r.add_header(k, v)
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
    check("E0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    flaky_hits = os.path.join(HOME, "flaky.hits"); live_hits = os.path.join(HOME, "live.hits")
    open(flaky_hits, "w").write("0"); open(live_hits, "w").write("0")
    set_flaky("live")
    procs = []
    for port, mode, hf in ((FLAKY_PORT, "flaky", flaky_hits), (LIVE_PORT, "live", live_hits)):
        p = subprocess.Popen([sys.executable, mock_py, str(port), mode, hf, MODE_FILE],
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
            st, _, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1
        check("E0b router ready (/healthz 200)", True)

        # Flaky first in the chain, live second (addition order).
        for name, port in (("flaky", FLAKY_PORT), ("live", LIVE_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}",
                               "api_key": name + "-key"},
                              key=raw_key)
            check(f"E0c add provider {name}", st == 201, f"status={st} {body[:160]}")

        # E1: embeddings 200, vectors verbatim + usage, endpoint header.
        st, hdrs, body = req("POST", "/v1/embeddings",
                             {"model": "mock-model", "input": "hello world"}, key=raw_key)
        try: obj = json.loads(body)
        except Exception: obj = {}
        data = obj.get("data", [])
        check("E1 embeddings 200, vector verbatim + usage + endpoint header",
              st == 200 and len(data) == 1 and data[0].get("embedding") == [0.1, 0.2, 0.3]
              and obj.get("usage", {}).get("prompt_tokens") == 8
              and obj.get("usage", {}).get("total_tokens") == 8
              and hdrs.get("X-Codvian-Endpoint") == "embeddings",
              f"status={st} header={hdrs.get('X-Codvian-Endpoint')} {body[:200]}")
        check("E1b flaky hit once, live untouched",
              hits(flaky_hits) == 1 and hits(live_hits) == 0,
              f"flaky={hits(flaky_hits)} live={hits(live_hits)}")

        # E2: the logs feed shows the embeddings row with its endpoint.
        # The ledger writer flushes asynchronously, so poll briefly
        # for the row instead of asserting on the first read.
        emb = []
        for _ in range(25):
            st, _, body = req("GET", "/dashboard/logs?limit=100&filter=all", key=raw_key)
            try: feed = json.loads(body)
            except Exception: feed = {}
            evs = feed.get("events", [])
            emb = [e for e in evs if e.get("endpoint") == "embeddings"]
            if emb: break
            time.sleep(0.2)
        check("E2 logs feed shows the embeddings row (endpoint + tokens)",
              st == 200 and len(emb) == 1 and emb[0].get("model") == "mock-model"
              and emb[0].get("prompt_tokens") == 8,
              f"status={st} emb={emb}")

        # E3: rerank 200 with scores, no invented usage.
        st, hdrs, body = req("POST", "/v1/rerank",
                             {"model": "mock-model", "query": "capital?",
                              "documents": ["Paris is the capital", "London is big"],
                              "top_n": 2, "return_documents": True}, key=raw_key)
        try: obj = json.loads(body)
        except Exception: obj = {}
        res = obj.get("results", [])
        check("E3 rerank 200, scores in order, no invented usage",
              st == 200 and len(res) == 2 and res[0].get("index") == 1
              and res[0].get("relevance_score") == 0.92 and res[1].get("relevance_score") == 0.31
              and "usage" not in obj and hdrs.get("X-Codvian-Endpoint") == "rerank",
              f"status={st} {body[:200]}")
        check("E3b flaky hit again, live still untouched",
              hits(flaky_hits) == 2 and hits(live_hits) == 0,
              f"flaky={hits(flaky_hits)} live={hits(live_hits)}")

        # E4: the trace names the endpoint + the exemption.
        st, _, body = req("GET", "/dashboard/trace", key=raw_key)
        try: trace = json.loads(body)
        except Exception: trace = {}
        atts = trace.get("attempts", [])
        check("E4 trace endpoint rerank + exempt:rerank on the attempt",
              st == 200 and trace.get("endpoint") == "rerank" and len(atts) == 1
              and atts[0].get("preread") == "exempt:rerank" and atts[0].get("outcome") == "success",
              f"status={st} {body[:240]}")

        # E5: first-target 429 on embeddings -> honest 502, exactly
        # one attempt, the fallback never dialled (never replayed).
        set_flaky("dead")
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, _, body = req("POST", "/v1/embeddings",
                          {"model": "mock-model", "input": "hello again"}, key=raw_key)
        check("E5 first-target 429 on embeddings -> honest 502 (never replays)",
              st == 502, f"status={st} {body[:200]}")
        check("E5b exactly one attempt, fallback never dialled",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")

        # E6: the same on rerank.
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, _, body = req("POST", "/v1/rerank",
                          {"model": "mock-model", "query": "capital?",
                           "documents": ["Paris is the capital"]}, key=raw_key)
        check("E6 first-target 429 on rerank -> honest 502 (never replays)",
              st == 502, f"status={st} {body[:200]}")
        check("E6b exactly one attempt, fallback never dialled",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")
        set_flaky("live")

        # E7: guardrail block on the input text, zero provider hits.
        st, _, body = req("POST", "/dashboard/guardrails",
                          {"block_pii": False, "block_secrets": True, "block_injection": False})
        if st != 200:
            check("E7 guardrail toggle on", False, f"status={st} {body[:120]}")
        else:
            fh_before, lh_before = hits(flaky_hits), hits(live_hits)
            st, _, body = req("POST", "/v1/embeddings",
                              {"model": "mock-model", "input": "my api_key=abcdefgh12345678"},
                              key=raw_key)
            check("E7 guardrail block -> 403/-32001, zero provider hits",
                  st == 403 and "-32001" in body and "secret" in body
                  and hits(flaky_hits) == fh_before and hits(live_hits) == lh_before,
                  f"status={st} flaky={hits(flaky_hits)} live={hits(live_hits)} {body[:160]}")

        # E8: strict budget the estimate exceeds -> 402 pre-send.
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        long_input = "hello world " * 100
        st, _, body = req("POST", "/v1/embeddings",
                          {"model": "mock-model", "input": long_input}, key=raw_key,
                          headers={"X-Codvian-Budget": "0.00001",
                                   "X-Codvian-Budget-Strict": "true"})
        check("E8 strict budget exceeded -> 402 pre-send, zero provider hits",
              st == 402 and hits(flaky_hits) == fh_before and hits(live_hits) == lh_before,
              f"status={st} flaky={hits(flaky_hits)} live={hits(live_hits)} {body[:160]}")

        # E9: dashboard serves the Endpoint column, no Playground.
        st, _, body = req("GET", "/dashboard")
        check("E9 dashboard serves Endpoint column, no Playground",
              st == 200 and "<th>Endpoint</th>" in body and "Playground" not in body,
              f"status={st}")

        # E10: unauthenticated -> 401; invalid body -> 400.
        st, _, body = req("POST", "/v1/embeddings", {"model": "mock-model", "input": "x"})
        check("E10 unauthenticated embeddings -> 401", st == 401, f"status={st}")
        st, _, body = req("POST", "/v1/embeddings", {"input": "x"}, key=raw_key)
        check("E10b missing model -> 400", st == 400, f"status={st} {body[:120]}")

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
