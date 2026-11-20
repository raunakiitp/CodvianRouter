#!/usr/bin/env python3
"""Live smoke for per-key rolling 5h USD budgets (CHECKLIST 139-140).

Real binary + two Python mocks. No fake data: every assertion reads
a live counter, a live ledger feed, or a live HTTP status.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/kb-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21311
LIVE_PORT = 21301
DEAD_PORT = 21302
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MOCK_USAGE = {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
# mock-model priced $10/1M in + $30/1M out => $0.025 per call.
CALL_COST = 1000 * 10 / 1e6 + 500 * 30 / 1e6

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); mode = sys.argv[2]; hits_file = sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        self._hit()
        models = [{"id": "mock-model", "object": "model"}]
        if mode == "live":
            models.append({"id": "mystery-model", "object": "model"})
        body = json.dumps({"object": "list", "data": models}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(ln)
        self._hit()
        if mode == "dead":
            body = json.dumps({"error": {"message": "slow down", "type": "rate_limit"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "0"); self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": "mock-model",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "smoke says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
HTTPServer(("127.0.0.1", port), H).serve_forever()
'''

checks = []
def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}")

def req(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)

def hits(port_file):
    try:
        with open(port_file) as f: return int(f.read().strip() or 0)
    except Exception: return 0

def chat(model, prompt, key):
    return req("POST", "/v1/chat/completions",
               {"model": model, "messages": [{"role": "user", "content": prompt}]}, key)

def feed(key=None):
    st, body = req("GET", "/dashboard/key-budgets", key=key)
    if st != 200: return None
    return json.loads(body)

def set_budget(key_id, limit, policy=None, delete=False, key=None):
    b = {"key_id": key_id, "delete": delete}
    if not delete:
        b["limit_usd"] = limit
        if policy: b["unpriced_policy"] = policy
    return req("POST", "/dashboard/key-budgets", b, key=key)

def main():
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    # Build the real binary.
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN, "./cmd/codvian-router"],
                       cwd=os.path.expanduser("~/workspace/codvian-router"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr); return 1
    # Mocks.
    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    live_hits = os.path.join(HOME, "live.hits"); dead_hits = os.path.join(HOME, "dead.hits")
    open(live_hits, "w").write("0"); open(dead_hits, "w").write("0")
    procs = []
    for port, mode, hf in ((LIVE_PORT, "live", live_hits), (DEAD_PORT, "dead", dead_hits)):
        p = subprocess.Popen([sys.executable, mock_py, str(port), mode, hf],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        procs.append(p)
    # init: fresh config + first gateway key.
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
    key_id = cfg["gateway_keys"][0]["id"]
    # Prices (model-level) + rolling budget, before the first start:
    # startup applies key_budgets into the ledger.
    cfg["prices"] = {"mock-model": {"input_usd_per_1m": 10, "output_usd_per_1m": 30}}
    cfg["key_budgets"] = {key_id: {"limit_usd": 0.05, "unpriced_policy": "fail_closed"}}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    procs.append(router)
    try:
        # Wait for readiness.
        for _ in range(50):
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1

        # Add the two providers (dead FIRST in registry order, so the
        # chain for mock-model is dead -> live).
        for pid, port in (("dead", DEAD_PORT), ("live", LIVE_PORT)):
            st, body = req("POST", "/api/providers",
                           {"base_url": f"http://127.0.0.1:{port}",
                            "api_key": pid + "-key",
                            "models": [{"id": "mock-model"}] if pid == "dead" else
                                      [{"id": "mock-model"}, {"id": "mystery-model"}]},
                           key=raw_key)
            # The provider-add shape varies; accept 201, else retry with name.
            if st != 201:
                st, body = req("POST", "/api/providers",
                               {"name": pid, "base_url": f"http://127.0.0.1:{port}",
                                "api_key": pid + "-key"}, key=raw_key)
            check(f"add provider {pid}", st == 201, f"status={st} {body[:200]}")
        # The add above may not carry models; the chain only needs the
        # providers to exist — models come from the mock's /v1/models.
        # Give the registry a beat, then verify the dashboard + feed.
        st, body = req("GET", "/dashboard")
        check("K1 dashboard 200 + key-budget card", st == 200 and "Per-key rolling budgets" in body, f"status={st}")
        f = feed()
        row = (f["budgets"] or [None])[0] if f else None
        check("K2 feed: $0.05 fail_closed budget from config", bool(f) and row and row["has_budget"]
              and row["limit_usd"] == 0.05 and row["unpriced_policy"] == "fail_closed" and row["spent_usd"] == 0,
              f"row={row}")

        # Fallback FIRST, before any successful chat: both providers
        # have zero engine history, so the balanced strategy's scores
        # tie and the chain keeps registry order (dead -> live). The
        # dead attempt must consume no quota: spend grows by exactly
        # one live call ($0.025).
        dead_before, live_before = hits(dead_hits), hits(live_hits)
        st, body = chat("mock-model", "smoke fallback call", raw_key)
        check("K9 fallback call -> 200", st == 200, f"status={st} {body[:160]}")
        check("K9b dead hit once, live hit once more",
              hits(dead_hits) == dead_before + 1 and hits(live_hits) == live_before + 1,
              f"dead {dead_before}->{hits(dead_hits)}, live {live_before}->{hits(live_hits)}")
        time.sleep(0.5)  # async ledger flush (50 ms) + margin
        f = feed(); row = f["budgets"][0]
        check("K9c spent is $0.025 (dead attempt cost $0)",
              abs(row["spent_usd"] - 0.025) < 1e-9, f"spent={row['spent_usd']}")

        # One more priced call: $0.025 + $0.025 = $0.05, at the cap.
        st, _ = chat("mock-model", "smoke call two", raw_key)
        check("K3 call 2 -> 200", st == 200, f"status={st}")
        time.sleep(0.5)
        f = feed(); row = f["budgets"][0]
        check("K4b spent is exactly $0.05 after 2 priced calls", abs(row["spent_usd"] - 0.05) < 1e-9,
              f"spent={row['spent_usd']}")
        live_before = hits(live_hits)
        st, body = chat("mock-model", "smoke call three", raw_key)
        check("K5 capped key -> 402 pre-call", st == 402, f"status={st} {body[:160]}")
        check("K5b no provider call for the refused request", hits(live_hits) == live_before,
              f"live hits {live_before} -> {hits(live_hits)}")
        f = feed(); row = f["budgets"][0]
        check("K6 feed at_limit + remaining $0",
              row["at_limit"] and row["remaining_usd"] is not None and abs(row["remaining_usd"]) < 1e-9,
              f"row={row}")

        # count_as_zero: unpriced model routes; fail_closed: 400 naming it.
        st, _ = set_budget(key_id, 10, "count_as_zero", key=raw_key)
        check("K7 set limit $10 count_as_zero", st == 200, f"status={st}")
        st, body = chat("mystery-model", "smoke mystery one", raw_key)
        check("K7b unpriced model routes under count_as_zero", st == 200, f"status={st} {body[:160]}")
        st, _ = set_budget(key_id, 10, "fail_closed", key=raw_key)
        check("K8 set limit $10 fail_closed", st == 200, f"status={st}")
        live_before = hits(live_hits)
        st, body = chat("mystery-model", "smoke mystery two", raw_key)
        check("K8b unpriced model -> 400 naming mystery-model", st == 400 and "mystery-model" in body,
              f"status={st} {body[:200]}")
        check("K8c 400 made no provider call", hits(live_hits) == live_before,
              f"live hits {live_before} -> {hits(live_hits)}")

        f = feed(); row = f["budgets"][0]
        check("K10 unknown spend reported, not $0",
              row["unknown_events"] >= 1 and "mystery-model" in (row["unpriced_models"] or []),
              f"unknown_events={row['unknown_events']} unpriced={row['unpriced_models']}")

        # Restart persistence: tighten the cap, restart, budget +
        # spend must survive and still refuse.
        st, _ = set_budget(key_id, 0.05, "fail_closed", key=raw_key)
        check("K11a re-tighten cap to $0.05", st == 200, f"status={st}")
        router.terminate()
        try: router.wait(timeout=10)
        except Exception: router.kill()
        procs.remove(router)
        router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        procs.append(router)
        for _ in range(50):
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready after restart"); return 1
        f = feed(); row = f["budgets"][0]
        check("K11b budget + spend survived restart",
              row["has_budget"] and row["limit_usd"] == 0.05 and abs(row["spent_usd"] - 0.05) < 1e-9,
              f"row={row}")
        live_before = hits(live_hits)
        st, _ = chat("mock-model", "smoke after restart", raw_key)
        check("K11c still refused (402) after restart", st == 402, f"status={st}")
        check("K11d no provider call after restart refusal", hits(live_hits) == live_before,
              f"live hits {live_before} -> {hits(live_hits)}")
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
    print(f"\n{len(checks) - len(failed)}/{len(checks)} smoke checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
