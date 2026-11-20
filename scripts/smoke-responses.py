#!/usr/bin/env python3
"""Live smoke for POST /v1/responses (CHECKLIST 169-170).

Real binary + two Python mocks (one flaky, one always live). No fake
data: every assertion reads a live HTTP status, a live hit count, a
live feed, or the live dashboard HTML.

Scenarios:
  R1 create on the healthy first provider mints resp_ id; retrieve works.
     The chain order is fixed with the per-request routing contract
     (order [flaky, live] is final — no scoring re-ranks it), so the
     serving provider is deterministic despite the engine's live
     re-ranking after failures.
  R2 fresh create fails over (flaky 429s -> live serves 200, 1 hit each)
  R3 continuation pinned to the original provider: when it 429s the
     branched request fails over to the fallback in the same request
     (CHECKLIST 237) — 200 from live, one more hit on each
  R4 successful continuation resumes the conversation (200, feed shows
     the previous_response_id chain)
  R5 stream event order: response.created ... response.completed + [DONE];
     retrieve after the stream shows completed
  R6 feed GET /dashboard/responses shows ids + continuation chain + counts
  R7 dashboard serves the Responses blocks, no Playground
  R8 unknown response id -> 404; unauthenticated create -> 401
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/responses-smoke")
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
    check("R0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    flaky_hits = os.path.join(HOME, "flaky.hits"); live_hits = os.path.join(HOME, "live.hits")
    open(flaky_hits, "w").write("0"); open(live_hits, "w").write("0")
    set_flaky("dead")
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
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1
        check("R0b router ready (/healthz 200)", True)

        # Flaky first in the chain, live second (addition order).
        for name, port in (("flaky", FLAKY_PORT), ("live", LIVE_PORT)):
            st, body = req("POST", "/api/providers",
                           {"name": name, "base_url": f"http://127.0.0.1:{port}",
                            "api_key": name + "-key"},
                           key=raw_key)
            check(f"R0c add provider {name}", st == 201, f"status={st} {body[:160]}")

        # R1: create on the healthy first provider. The routing
        # contract fixes the chain order [flaky, live] (final — no
        # scoring re-ranks it), so flaky serves deterministically.
        set_flaky("live")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "first turn",
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj1 = json.loads(body)
        except Exception: obj1 = {}
        rid1 = obj1.get("id", "")
        check("R1 create on healthy first provider mints resp_ id",
              st == 200 and rid1.startswith("resp_") and obj1.get("status") == "completed",
              f"status={st} {body[:200]}")
        check("R1b flaky served it (hit +1), live untouched",
              hits(flaky_hits) == 1 and hits(live_hits) == 0,
              f"flaky={hits(flaky_hits)} live={hits(live_hits)}")
        st, body = req("GET", f"/v1/responses/{rid1}", key=raw_key)
        check("R1c retrieve by id returns the same response",
              st == 200 and json.loads(body).get("id") == rid1, f"status={st} {body[:160]}")

        # R2: fresh create, flaky 429s -> live serves (same fixed order).
        set_flaky("dead")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "hello",
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj2 = json.loads(body)
        except Exception: obj2 = {}
        check("R2 fresh create fails over (flaky 429 -> live 200, resp_ id)",
              st == 200 and obj2.get("id", "").startswith("resp_") and obj2.get("status") == "completed",
              f"status={st} {body[:200]}")
        check("R2b flaky hit once, live hit once",
              hits(flaky_hits) == 2 and hits(live_hits) == 1,
              f"flaky={hits(flaky_hits)} live={hits(live_hits)}")

        # R3: continuation pinned to flaky (which served R1); flaky
        # 429s -> the branched request fails over to live in the
        # SAME request (CHECKLIST 237: the transcript is
        # gateway-held, so replaying the branched input cannot
        # double-apply anything). One more hit on each provider.
        # A header-less 429 sets no cooldown and never trips the
        # breaker, so the pinned attempt is genuinely dialled
        # exactly once before the failover.
        fh_before, lh_before = hits(flaky_hits), hits(live_hits)
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "second turn",
                        "previous_response_id": rid1,
                        "routing": {"order": ["flaky", "live"]}}, key=raw_key)
        try: obj3 = json.loads(body)
        except Exception: obj3 = {}
        check("R3 pinned continuation 429s -> fails over to live (200)",
              st == 200 and obj3.get("previous_response_id") == rid1
              and obj3.get("status") == "completed",
              f"status={st} {body[:200]}")
        check("R3b exactly one pinned attempt, then one fallback hit",
              hits(flaky_hits) == fh_before + 1 and hits(live_hits) == lh_before + 1,
              f"flaky={hits(flaky_hits)} (was {fh_before}) live={hits(live_hits)} (was {lh_before})")

        # R4: same continuation with flaky healthy -> 200, resumes.
        set_flaky("live")
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "second turn",
                        "previous_response_id": rid1}, key=raw_key)
        try: obj4 = json.loads(body)
        except Exception: obj4 = {}
        rid4 = obj4.get("id", "")
        check("R4 continuation resumes (200, previous_response_id echoed)",
              st == 200 and rid4.startswith("resp_") and rid4 != rid1
              and obj4.get("previous_response_id") == rid1,
              f"status={st} {body[:200]}")

        # R5: stream event order + retrieve after stream.
        st, body = req("POST", "/v1/responses",
                       {"model": "mock-model", "input": "stream me", "stream": True},
                       key=raw_key)
        evs = [l for l in body.splitlines() if l.startswith("event: ")]
        try:
            created = json.loads(body.split("data: ", 1)[1].split("\n\n")[0])
            rid5 = created.get("response", {}).get("id", "")
        except Exception:
            rid5 = ""
        check("R5 stream event order created..completed + [DONE]",
              st == 200 and evs and evs[0] == "event: response.created"
              and evs[-1] == "event: response.completed" and "[DONE]" in body
              and "response.output_text.delta" in body and rid5.startswith("resp_"),
              f"status={st} events={evs} id={rid5}")
        st, body = req("GET", f"/v1/responses/{rid5}", key=raw_key)
        check("R5b retrieve after stream shows completed",
              st == 200 and json.loads(body).get("status") == "completed",
              f"status={st} {body[:160]}")

        # R6: feed shows ids + continuation chain + counts. Two
        # continuations of rid1 now exist (R3's failover success
        # and R4), so the chain has 2 entries — CHECKLIST 237.
        st, body = req("GET", "/dashboard/responses", key=raw_key)
        try: feed = json.loads(body)
        except Exception: feed = {}
        ids = [e.get("id") for e in feed.get("responses", [])]
        chain = [e for e in feed.get("responses", [])
                 if e.get("previous_response_id") == rid1]
        check("R6 feed lists ids + continuation chain + counts",
              st == 200 and rid1 in ids and rid4 in ids and len(chain) == 2
              and feed.get("created", 0) >= 5 and feed.get("continuations", 0) >= 2,
              f"status={st} created={feed.get('created')} continuations={feed.get('continuations')} ids={ids}")
        st, body = req("GET", "/dashboard/responses")
        check("R6b feed is auth-exempt", st == 200, f"status={st}")

        # R7: dashboard serves the Responses blocks, no Playground.
        st, body = req("GET", "/dashboard")
        check("R7 dashboard serves Responses blocks, no Playground",
              st == 200 and "responsesStats" in body and "responsesBody" in body
              and "No responses yet" in body and "Playground" not in body,
              f"status={st}")

        # R8: unknown id 404; no key 401.
        st, body = req("GET", "/v1/responses/resp_doesnotexist0000000000", key=raw_key)
        check("R8 unknown response id -> 404", st == 404, f"status={st} {body[:120]}")
        st, body = req("POST", "/v1/responses", {"model": "mock-model", "input": "x"})
        check("R8b unauthenticated create -> 401", st == 401, f"status={st}")

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
