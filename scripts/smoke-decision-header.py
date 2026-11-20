#!/usr/bin/env python3
"""Live smoke for decision transparency + catalog filters/shortlist/refresh
(CHECKLIST 199-200).

Real binary + two Python mocks. No fake data: every assertion reads a
live header, a live feed, a live counter, or a live HTTP status.

Mocks: "dead" answers 429 on chat and lists dead-model; "live" answers
chat (JSON + SSE), embeddings, and lists live-model (+ fresh-model
after the smoke rewrites its model file). The router is started twice:
first with a bare config (header checks), then with global_strategy
cheapest + pair prices + shortlist (cheapest header, filters,
shortlist, refresh checks).
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-199-200")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21431
DEAD_PORT = 21432
LIVE_PORT = 21433
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; hits_file = sys.argv[3]
models_file = sys.argv[4]; fail_chat = (len(sys.argv) > 5 and sys.argv[5] == "fail-chat")
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def _models(self):
        try:
            with open(models_file) as f: return json.load(f)
        except Exception: return []
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"object": "list", "data": [{"id": m, "object": "model"} for m in self._models()]}).encode()
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
        model = req.get("model", "live-model")
        if self.path.endswith("/embeddings"):
            body = json.dumps({"object": "list", "data": [{"object": "embedding", "embedding": [0.1, 0.2], "index": 0}], "model": model, "usage": {"prompt_tokens": 4, "total_tokens": 4}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        if fail_chat:
            body = json.dumps({"error": {"message": "rate limited by dead mock", "type": "rate_limit_error"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        if req.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            for text in ("Hello ", "world"):
                chunk = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": model,
                    "choices": [{"index": 0, "delta": {"content": text}, "finish_reason": None}]}).encode()
                self.wfile.write(b"data: " + chunk + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": name + " says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}).encode()
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

def hits(fn):
    try:
        with open(fn) as f: return int(f.read().strip() or 0)
    except Exception: return 0

def chat(model, prompt, key, routing=None, stream=False):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if routing: body["routing"] = routing
    if stream: body["stream"] = True
    return req("POST", "/v1/chat/completions", body, key)

def models_list(query="", key=None):
    st, _, body = req("GET", "/v1/models" + query, key=key)
    try: d = json.loads(body)
    except Exception: return st, []
    return st, d.get("data", [])

# Distinct prompts: the exact cache must not collapse the checks.
P1 = "The harbour master counts the fishing boats at dawn."
P2 = "A brass telescope stands on the old observatory desk."
P3 = "Winter apples ripen slowly in the valley orchard."
P4 = "The night train crosses the viaduct after midnight."
P5 = "Salt spray dries on the lighthouse keeper's coat."
P6 = "A paper kite tangles in the poplar branches."
P7 = "The mill wheel turns slowly beside the quiet river."

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
    procs = []
    files = {}
    model_files = {}
    for port, name, fail, models in (
            (DEAD_PORT, "dead", "fail-chat", ["dead-model", "live-model"]),
            (LIVE_PORT, "live", "ok", ["live-model", "old-model"])):
        hf = os.path.join(HOME, name + ".hits")
        open(hf, "w").write("0")
        mf = os.path.join(HOME, name + ".models.json")
        with open(mf, "w") as f: json.dump(models, f)
        files[name] = hf; model_files[name] = mf
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, hf, mf, fail],
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
        # Live is added first, so live-model's /v1 row (first
        # provider offering it) is live's — the cheap pair (blend
        # 2) the max_price and cheapest checks rely on.
        for name, port in (("live", LIVE_PORT), ("dead", DEAD_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}", "api_key": name + "-key"},
                              key=raw_key)
            check(f"A0 add provider {name}", st == 201, f"status={st} {body[:160]}")
            try: ids[name] = json.loads(body).get("id", name)
            except Exception: ids[name] = name
        dead_id, live_id = ids["dead"], ids["live"]

        # ---- Phase 1: decision header (bare config, balanced) ----
        # Contract order names providers by DISPLAY name (the
        # names clients see); the chain resolves them to IDs.
        st, h, body = chat("live-model", P1, raw_key,
                           routing={"order": ["dead", "live"]})
        check("B1 fallback chat 200 + live content", st == 200 and "live says hi" in body, f"status={st} body={body[:300]}")
        check("B2 header fallback-after-1 names live",
              h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=fallback-after-1",
              f"got={h.get('X-Codvian-Decision')!r}")
        check("B3 dead hit once, live hit once",
              hits(files["dead"]) == 1 and hits(files["live"]) == 1,
              f"dead={hits(files['dead'])} live={hits(files['live'])}")

        st, h, body = chat("live-model", P2, raw_key)
        check("C1 plain chat header reason=balanced",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=balanced",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        st, h, body = chat("live-model", P3, raw_key, stream=True)
        check("D1 stream 200 + both chunks + DONE",
              st == 200 and '"content":"Hello "' in body and '"content":"world"' in body and body.strip().endswith("data: [DONE]"),
              f"status={st}")
        check("D2 stream header set before first chunk",
              h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=balanced",
              f"got={h.get('X-Codvian-Decision')!r}")

        st, h, body = chat("live-model", P4, raw_key, routing={"sort": "price"})
        check("E1 contract sort header strategy=cheapest reason=contract",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=cheapest; reason=contract",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        st, h, body = req("POST", "/v1/responses", {"model": "live-model", "input": P5}, raw_key)
        check("F1 responses header present",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=balanced",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        st, h, body = req("POST", "/v1/embeddings", {"model": "live-model", "input": "hello"}, raw_key)
        check("F2 embeddings header present",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=balanced; reason=balanced",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        # Cache-hit absence: the identical prompt twice.
        before = hits(files["live"])
        st, h1, _ = chat("live-model", P6, raw_key)
        st2, h2, body2 = chat("live-model", P6, raw_key)
        check("G1 first identical prompt carries the header",
              st == 200 and h1.get("X-Codvian-Decision", "").startswith("provider="),
              f"status={st} got={h1.get('X-Codvian-Decision')!r}")
        check("G2 second identical prompt: header ABSENT, cache named, mock not hit again",
              st2 == 200 and h2.get("X-Codvian-Decision", "") == "" and h2.get("X-Codvian-Cache", "") != "" and hits(files["live"]) == before + 1,
              f"status={st2} decision={h2.get('X-Codvian-Decision')!r} cache={h2.get('X-Codvian-Cache')!r} live_hits={hits(files['live'])} before={before}")

        st, h, body = chat("dead-model", P7, raw_key)
        check("H1 dead-only model fails and carries NO decision header",
              st != 200 and h.get("X-Codvian-Decision", "") == "",
              f"status={st} decision={h.get('X-Codvian-Decision')!r}")

        # ---- Phase 2: restart with cheapest + pair prices + shortlist ----
        stop_router(router); procs.remove(router)
        with open(cfg_path) as f: cfg = json.load(f)
        cfg["routing"] = {"global_strategy": "cheapest"}
        cfg["provider_prices"] = {
            live_id: {"live-model": {"input_usd_per_1m": 1, "output_usd_per_1m": 3}},
            dead_id: {"live-model": {"input_usd_per_1m": 10, "output_usd_per_1m": 10}},
        }
        cfg["shortlist"] = ["live-model"]
        with open(cfg_path, "w") as f: json.dump(cfg, f, indent=2)
        router = start_router(env)
        if router is None: return 1
        procs.append(router)

        st, h, body = chat("live-model", P1 + " cheapest", raw_key)
        check("I1 cheapest header (live is the cheap pair)",
              st == 200 and h.get("X-Codvian-Decision") == f"provider={live_id}; model=live-model; strategy=cheapest; reason=cheapest",
              f"status={st} got={h.get('X-Codvian-Decision')!r}")

        st, rows = models_list("", raw_key)
        ids_list = [m["id"] for m in rows]
        check("J1 /v1/models lists live-model first, shortlisted",
              st == 200 and ids_list and ids_list[0] == "live-model" and rows[0].get("shortlisted") is True,
              f"status={st} first={ids_list[:3]}")
        check("J2 shortlist=true lists only live-model",
              models_list("?shortlist=true", raw_key)[1] and [m["id"] for m in models_list("?shortlist=true", raw_key)[1]] == ["live-model"],
              f"got={[m['id'] for m in models_list('?shortlist=true', raw_key)[1]]}")
        st, rows = models_list("?max_price=5", raw_key)
        check("J3 max_price=5 keeps live-model (blend 2), drops dead-model (unknown price)",
              st == 200 and [m["id"] for m in rows] == ["live-model"],
              f"got={[m['id'] for m in rows]}")
        st, rows = models_list("?provider=" + live_id, raw_key)
        check("J4 provider filter lists live's models only",
              st == 200 and sorted(m["id"] for m in rows) == ["live-model", "old-model"],
              f"got={[m['id'] for m in rows]}")
        st, rows = models_list("?min_context=1", raw_key)
        check("J5 min_context=1 lists nothing (no mock reports a window — unknown excluded, never assumed large)",
              st == 200 and rows == [], f"got={[m['id'] for m in rows]}")
        st, _, body = req("GET", "/v1/models?max_price=abc", key=raw_key)
        check("J6 malformed max_price answers 400 naming the parameter",
              st == 400 and "max_price" in body, f"status={st} body={body[:120]}")

        st, _, body = req("GET", "/dashboard/models?shortlist=true")
        try: d = json.loads(body)
        except Exception: d = {}
        drows = d.get("models", [])
        live_row = [r for r in drows if r.get("provider_id") == live_id]
        check("K1 dashboard feed shortlist=true: live-model rows shortlisted, live's pair price 1/3 per 1M",
              st == 200 and len(drows) == 2 and all(r.get("shortlisted") is True for r in drows)
              and len(live_row) == 1 and live_row[0].get("input_price_usd") == 1 and live_row[0].get("output_price_usd") == 3,
              f"status={st} rows={drows}")
        st, _, page = req("GET", "/dashboard")
        check("K2 dashboard page has the Models filter + refresh controls",
              st == 200 and "modelProviderFilter" in page and "modelRefreshBtn" in page and "modelShortlistOnly" in page,
              f"status={st}")

        # ---- Phase 3: live refresh ----
        with open(model_files["live"], "w") as f: json.dump(["live-model", "fresh-model"], f)
        st, _, body = req("POST", "/dashboard/models/refresh", {}, raw_key)
        try: d = json.loads(body)
        except Exception: d = {}
        provs = {p["provider_id"]: p for p in d.get("providers", [])}
        check("L1 refresh 200 with per-provider ok results",
              st == 200 and provs.get(live_id, {}).get("status") == "ok"
              and provs.get(live_id, {}).get("models") == ["live-model", "fresh-model"]
              and provs.get(dead_id, {}).get("status") == "ok",
              f"status={st} body={body[:300]}")
        st, rows = models_list("", raw_key)
        check("L2 catalog now lists fresh-model, old-model gone",
              st == 200 and "fresh-model" in [m["id"] for m in rows] and "old-model" not in [m["id"] for m in rows],
              f"got={[m['id'] for m in rows]}")

        # A dead provider's refresh failure keeps its last known list.
        for p in procs:
            pass
        # Stop the dead mock (it is procs[0]); the live mock stays up.
        dead_proc = procs[0]
        dead_proc.terminate()
        try: dead_proc.wait(timeout=5)
        except Exception: dead_proc.kill()
        st, _, body = req("POST", "/dashboard/models/refresh", {}, raw_key)
        try: d = json.loads(body)
        except Exception: d = {}
        provs = {p["provider_id"]: p for p in d.get("providers", [])}
        check("L3 dead mock down: refresh reports it failed AND keeps dead's list",
              st == 200 and provs.get(dead_id, {}).get("status") == "failed"
              and provs.get(dead_id, {}).get("error", "") != ""
              and provs.get(dead_id, {}).get("models") == ["dead-model", "live-model"],
              f"status={st} body={body[:300]}")
        st, rows = models_list("", raw_key)
        check("L4 dead-model still listed after the failed refresh",
              st == 200 and "dead-model" in [m["id"] for m in rows],
              f"got={[m['id'] for m in rows]}")
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
