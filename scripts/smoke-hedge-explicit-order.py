#!/usr/bin/env python3
"""Live smoke for hedge vs explicit chain order (CHECKLIST 191-192).

Real binary + two Python mocks. No fake data: every assertion reads
a live counter, a live feed, or a live HTTP status.

Setup: "slow" answers model m after a 300 ms delay, "fast" answers
instantly. The 150 ms default hedge would race fast in and let it
win — unless the chain order is EXPLICIT (contract order, group
order, or strategy-tree order), in which case no hedge launches
and the slow first pick serves.

Phase A (tree + prices + group configured): contract order, tree
order and group order each serve slow with hedge
"disabled-explicit-order" and zero fast hits. Phase B (tree
removed, prices kept): a flat cheapest chain (contract sort
"price" — engine-computed order, NOT explicit) still hedges, so
fast wins with a hedged_cancelled row for slow and hedge "armed".
A streaming contract order also serves slow with no hedge.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/hedge-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21331
SLOW_PORT = 21332
FAST_PORT = 21333
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; hits_file = sys.argv[3]; delay = float(sys.argv[4])
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "m", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit()
        try: req = json.loads(raw.decode())
        except Exception: req = {}
        time.sleep(delay)
        if req.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            chunk = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": "m",
                "choices": [{"index": 0, "delta": {"content": name + " chunk"}, "finish_reason": None}]}).encode()
            self.wfile.write(b"data: " + chunk + b"\n\n")
            fin = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": "m",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}).encode()
            self.wfile.write(b"data: " + fin + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": "m",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": name + " says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}).encode()
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
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)

def hits(fn):
    try:
        with open(fn) as f: return int(f.read().strip() or 0)
    except Exception: return 0

def chat(model, prompt, key, routing=None, stream=False):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if routing: body["routing"] = routing
    if stream: body["stream"] = True
    return req("POST", "/v1/chat/completions", body, key)

def trace():
    st, body = req("GET", "/dashboard/trace")
    return json.loads(body) if st == 200 else {}

def no_hedged_cancelled(tr):
    return all(a.get("outcome") != "hedged_cancelled" for a in tr.get("attempts", []))

def has_hedged_cancelled(tr, provider=None):
    for a in tr.get("attempts", []):
        if a.get("outcome") == "hedged_cancelled" and (provider is None or a.get("provider") == provider):
            return True
    return False

# Distinct prompts: the exact/semantic cache must not collapse the
# checks into one another.
P1 = "The harbour master counts the fishing boats at dawn."
P2 = "A brass telescope stands on the old observatory desk."
P3 = "Winter apples ripen slowly in the valley orchard."
P4 = "The night train crosses the viaduct after midnight."
P5 = "Salt spray dries on the lighthouse keeper's coat."
P6 = "A paper kite tangles in the poplar branches."

def start_router(env):
    p = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                         env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for _ in range(50):
        st, _ = req("GET", "/healthz")
        if st == 200: return p
        time.sleep(0.2)
    print("ROUTER never became ready"); return None

def main():
    import shutil
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
    files = {}
    procs = []
    for port, name, delay in ((SLOW_PORT, "slow", 0.3), (FAST_PORT, "fast", 0.0)):
        hf = os.path.join(HOME, name + ".hits")
        open(hf, "w").write("0")
        files[name] = hf
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, hf, str(delay)],
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
        for pid, port in (("slow", SLOW_PORT), ("fast", FAST_PORT)):
            st, body = req("POST", "/api/providers",
                           {"name": pid, "base_url": f"http://127.0.0.1:{port}", "api_key": pid + "-key"},
                           key=raw_key)
            check(f"A0 add provider {pid}", st == 201, f"status={st} {body[:160]}")

        # Phase A config: tree cheapest-of[slow, fast] (slow is the
        # cheap pair price), group cheap-first [slow, fast].
        router.terminate()
        try: router.wait(timeout=10)
        except Exception: router.kill()
        procs.remove(router)
        cfg = json.load(open(cfg_path))
        ids = {p["name"]: p["id"] for p in cfg["providers"]}
        slow_id, fast_id = ids["slow"], ids["fast"]
        cfg["routing"] = {
            "strategy_tree": {"strategy": "cheapest", "children": [
                {"provider": slow_id, "model": "m"},
                {"provider": fast_id, "model": "m"}]},
            "groups": {"cheap-first": [slow_id, fast_id]},
        }
        cfg["provider_prices"] = {
            slow_id: {"m": {"input_usd_per_1m": 1, "output_usd_per_1m": 2}},
            fast_id: {"m": {"input_usd_per_1m": 50, "output_usd_per_1m": 100}},
        }
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = start_router(env)
        if router is None: return 1
        procs.append(router)

        # S1: contract order [slow, fast] — slow serves, fast untouched.
        f_before = hits(files["fast"])
        st, body = chat("m", P1, raw_key, routing={"order": ["slow", "fast"]})
        check("S1 contract order -> 200 from slow", st == 200 and "slow says hi" in body, f"status={st} {body[:120]}")
        check("S1b fast never dialled", hits(files["fast"]) == f_before, f"fast {f_before}->{hits(files['fast'])}")
        tr = trace()
        check("S1c trace hedge disabled-explicit-order, no hedged_cancelled",
              tr.get("hedge") == "disabled-explicit-order" and no_hedged_cancelled(tr),
              f"hedge={tr.get('hedge')} attempts={tr.get('attempts')}")

        # S2: tree order (no contract) — cheapest picks slow, slow serves.
        f_before = hits(files["fast"])
        st, body = chat("m", P2, raw_key)
        check("S2 tree order -> 200 from slow", st == 200 and "slow says hi" in body, f"status={st} {body[:120]}")
        check("S2b fast never dialled", hits(files["fast"]) == f_before, f"fast {f_before}->{hits(files['fast'])}")
        tr = trace()
        check("S2c trace hedge disabled-explicit-order, no hedged_cancelled",
              tr.get("hedge") == "disabled-explicit-order" and no_hedged_cancelled(tr),
              f"hedge={tr.get('hedge')} attempts={tr.get('attempts')}")
        check("S2d tree path ends at the slow provider",
              (tr.get("strategy_tree_path") or [])[-1:] == [slow_id],
              f"path={tr.get('strategy_tree_path')}")

        # S3: group order — slow serves, fast untouched.
        f_before = hits(files["fast"])
        st, body = chat("m", P3, raw_key, routing={"group": "cheap-first"})
        check("S3 group order -> 200 from slow", st == 200 and "slow says hi" in body, f"status={st} {body[:120]}")
        check("S3b fast never dialled", hits(files["fast"]) == f_before, f"fast {f_before}->{hits(files['fast'])}")
        tr = trace()
        check("S3c trace hedge disabled-explicit-order, no hedged_cancelled",
              tr.get("hedge") == "disabled-explicit-order" and no_hedged_cancelled(tr),
              f"hedge={tr.get('hedge')} attempts={tr.get('attempts')}")

        # Phase B: remove the tree (keep the prices). A flat cheapest
        # chain is engine-computed order, NOT explicit — the hedge
        # must still fire.
        router.terminate()
        try: router.wait(timeout=10)
        except Exception: router.kill()
        procs.remove(router)
        cfg = json.load(open(cfg_path))
        cfg["routing"].pop("strategy_tree", None)
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = start_router(env)
        if router is None: return 1
        procs.append(router)

        # S4: flat cheapest (contract sort "price") — fast wins the hedge.
        s_before, f_before = hits(files["slow"]), hits(files["fast"])
        st, body = chat("m", P4, raw_key, routing={"sort": "price"})
        check("S4 flat cheapest -> 200 from fast (hedge winner)",
              st == 200 and "fast says hi" in body, f"status={st} {body[:120]}")
        check("S4c both providers were dialled (hedge raced)",
              hits(files["slow"]) == s_before + 1 and hits(files["fast"]) == f_before + 1,
              f"slow {s_before}->{hits(files['slow'])} fast {f_before}->{hits(files['fast'])}")
        tr = trace()
        # The hedge proof is the trace's own latencies, not wall
        # time: this is the first chat after a restart, so the
        # wall clock also pays the one-time local token-counter
        # load. Fast served in 22 ms while the 300 ms slow pick
        # was still in flight — the response did not wait it out.
        win = [a for a in tr.get("attempts", []) if a.get("provider") == fast_id]
        check("S4b hedge served fast without waiting out the slow pick",
              len(win) == 1 and win[0].get("outcome") == "success" and win[0].get("latency_ms", 9999) < 300,
              f"winner={win}")
        check("S4d trace hedge armed at 150 ms, slow hedged_cancelled",
              tr.get("hedge") == "armed" and tr.get("hedge_delay_ms") == 150
              and has_hedged_cancelled(tr, slow_id),
              f"hedge={tr.get('hedge')} delay={tr.get('hedge_delay_ms')} attempts={tr.get('attempts')}")

        # S5: streaming contract order — slow streams, fast untouched.
        f_before = hits(files["fast"])
        st, body = chat("m", P5, raw_key, routing={"order": ["slow", "fast"]}, stream=True)
        check("S5 streaming contract order -> slow chunk", st == 200 and "slow chunk" in body, f"status={st} {body[:160]}")
        check("S5b fast never dialled", hits(files["fast"]) == f_before, f"fast {f_before}->{hits(files['fast'])}")
        tr = trace()
        check("S5c trace hedge disabled-explicit-order, no hedged_cancelled",
              tr.get("hedge") == "disabled-explicit-order" and no_hedged_cancelled(tr),
              f"hedge={tr.get('hedge')} attempts={tr.get('attempts')}")

        # S6: dashboard serves the hedge line for the last trace and
        # no Playground anywhere.
        st, body = req("GET", "/dashboard")
        check("S6 dashboard serves the hedge line",
              st == 200 and "Hedging off" in body and "never raced by a hedge" in body,
              f"status={st}")
        check("S6b no Playground on the dashboard", st == 200 and "Playground" not in body, f"status={st}")
    finally:
        for p in procs:
            try:
                p.terminate()
                p.wait(timeout=5)
            except Exception:
                try: p.kill()
                except Exception: pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
