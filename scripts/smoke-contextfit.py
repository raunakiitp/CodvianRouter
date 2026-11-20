#!/usr/bin/env python3
"""Live smoke for the context-window pre-call fit check + dedicated
fallback chain (CHECKLIST 159-160).

Real binary + two Python mocks. No fake data: every assertion reads
a live counter, a live feed, or a live HTTP status.

Setup: "small" offers small-model (8k window), "long" offers
long-model (1M window). The config file's routing block names
long-model as the dedicated context-window fallback chain.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/cf-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21321
SMALL_PORT = 21322
LONG_PORT = 21323
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
UNIT = "The quick brown fox jumps over the lazy dog while the river runs cold and clear. "
# C7/C8 need prompts with genuinely different WORDS, not a suffix on
# UNIT: the router's semantic cache (cosine similarity, 0.85
# threshold) would treat a near-identical prompt as a hit and serve
# the cached C3 response without calling any provider — correctly.
UNIT2 = "The old lighthouse keeper walks the cliff path each morning while gulls circle the grey harbour. "
UNIT3 = "Mira keeps a small garden behind the bakery where tomatoes climb the trellis every summer. "

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; hits_file = sys.argv[3]; stream_file = sys.argv[4]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self, fn):
        try:
            with open(fn) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(fn, "w") as f: f.write(str(n + 1))
    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": name, "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit(hits_file)
        try: req = json.loads(raw.decode())
        except Exception: req = {}
        if req.get("stream"):
            self._hit(stream_file)
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            chunk = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": name,
                "choices": [{"index": 0, "delta": {"content": name + " chunk"}, "finish_reason": None}]}).encode()
            self.wfile.write(b"data: " + chunk + b"\n\n")
            fin = json.dumps({"id": "c", "object": "chat.completion.chunk", "model": name,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}).encode()
            self.wfile.write(b"data: " + fin + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": name,
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

def chat(model, prompt, key, stream=False, max_tokens=None):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if stream: body["stream"] = True
    if max_tokens: body["max_tokens"] = max_tokens
    return req("POST", "/v1/chat/completions", body, key)

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
    for port, name in ((SMALL_PORT, "small-model"), (LONG_PORT, "long-model")):
        hf = os.path.join(HOME, name + ".hits"); sf = os.path.join(HOME, name + ".streams")
        open(hf, "w").write("0"); open(sf, "w").write("0")
        files[name] = (hf, sf)
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, hf, sf],
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

        # C0: fit feed is honestly empty before any request.
        st, body = req("GET", "/dashboard/context-fit")
        f = json.loads(body) if st == 200 else {}
        check("C0 context-fit feed empty before any request", st == 200 and f.get("has_decision") is False, f"status={st} {body[:120]}")

        # Add both providers. The add endpoint takes name + base_url
        # + key only (DisallowUnknownFields — a "models" field is a
        # 400); the model list comes from the mock's GET /v1/models
        # during the live key test, and the windows are added to the
        # config file below, then the router restarts.
        for pid, port in (("small", SMALL_PORT), ("long", LONG_PORT)):
            st, body = req("POST", "/api/providers",
                           {"name": pid, "base_url": f"http://127.0.0.1:{port}", "api_key": pid + "-key"},
                           key=raw_key)
            check(f"C1 add provider {pid}", st == 201, f"status={st} {body[:160]}")

        # Stop, edit config (windows + dedicated fallback chain), restart.
        router.terminate()
        try: router.wait(timeout=10)
        except Exception: router.kill()
        procs.remove(router)
        cfg = json.load(open(cfg_path))
        for p in cfg["providers"]:
            for m in p.get("models", []):
                if m["id"] == "small-model": m["context_window"] = 8192
                if m["id"] == "long-model": m["context_window"] = 1048576
        cfg["routing"] = {"context_fallback_models": ["long-model"]}
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        procs.append(router)
        for _ in range(50):
            st, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready after restart"); return 1

        # C2: small prompt fits the small model; fallback untouched.
        st, body = chat("small-model", "hello", raw_key)
        check("C2 small prompt -> 200 from small", st == 200 and "small-model says hi" in body, f"status={st} {body[:120]}")
        check("C2b long untouched", hits(files["long-model"][0]) == 0, f"long hits={hits(files['long-model'][0])}")

        # C3: oversize prompt (~400 reps x ~16 tokens ~= 6.4k tokens;
        # required ~10.5k > 8,192) jumps to the long model.
        big = UNIT * 400
        s_before, l_before = hits(files["small-model"][0]), hits(files["long-model"][0])
        st, body = chat("small-model", big, raw_key)
        check("C3 oversize prompt -> 200 from long", st == 200 and "long-model says hi" in body, f"status={st} {body[:120]}")
        check("C3b small never dialled for the oversize prompt",
              hits(files["small-model"][0]) == s_before, f"small {s_before}->{hits(files['small-model'][0])}")
        check("C3c long hit exactly once more",
              hits(files["long-model"][0]) == l_before + 1, f"long {l_before}->{hits(files['long-model'][0])}")

        # C4: the fit feed records the jump honestly.
        st, body = req("GET", "/dashboard/context-fit")
        f = json.loads(body) if st == 200 else {}
        d = f.get("decision") or {}
        check("C4 fit feed: chain_used context_fallback",
              st == 200 and f.get("has_decision") is True and d.get("chain_used") == "context_fallback",
              f"status={st} chain={d.get('chain_used')}")
        check("C4b required = prompt + 4096 reserve, largest 1M",
              d.get("required_tokens") == d.get("prompt_tokens", 0) + 4096 and d.get("largest_window") == 1048576
              and d.get("prompt_tokens", 0) > 4096,
              f"prompt={d.get('prompt_tokens')} required={d.get('required_tokens')}")
        ents = {e.get("model"): e for e in d.get("entries", [])}
        check("C4c entries: small too-small, long fits",
              ents.get("small-model", {}).get("fits") is False
              and ents.get("long-model", {}).get("fits") is True,
              f"entries={d.get('entries')}")

        # C5: the trace records the skip + the fit decision.
        st, body = req("GET", "/dashboard/trace")
        tr = json.loads(body) if st == 200 else {}
        atts = tr.get("attempts", [])
        check("C5 trace: [skipped_context_fit small, success long]",
              len(atts) == 2 and atts[0].get("outcome") == "skipped_context_fit"
              and atts[0].get("model") == "small-model" and atts[1].get("outcome") == "success"
              and atts[1].get("model") == "long-model",
              f"attempts={atts}")
        check("C5b trace embeds context_fit",
              (tr.get("context_fit") or {}).get("chain_used") == "context_fallback", f"body={body[:200]}")

        # C6: models feed embeds the decision; rows carry real windows.
        st, body = req("GET", "/dashboard/models")
        md = json.loads(body) if st == 200 else {}
        rows = {m.get("id"): m for m in md.get("models", [])}
        check("C6 models feed: real windows 8192 / 1048576",
              rows.get("small-model", {}).get("context_window") == 8192
              and rows.get("long-model", {}).get("context_window") == 1048576,
              f"rows={list(rows)}")
        check("C6b models feed embeds context_fit",
              (md.get("context_fit") or {}).get("chain_used") == "context_fallback", f"body={body[:160]}")

        # C7: streaming oversize prompt streams from the long model.
        # A DISTINCT prompt (unique suffix): the C3 prompt is already
        # in the exact cache, and a cached response is served without
        # calling any provider — correctly, but it would not prove
        # the streaming fit jump.
        s_before, l_before = hits(files["small-model"][0]), hits(files["long-model"][0])
        ls_before = hits(files["long-model"][1])
        st, body = chat("small-model", UNIT2 * 400, raw_key, stream=True)
        check("C7 streaming oversize -> long chunk",
              st == 200 and "long-model chunk" in body, f"status={st} {body[:160]}")
        check("C7b small never streamed",
              hits(files["small-model"][0]) == s_before and hits(files["small-model"][1]) == 0,
              f"small hits {s_before}->{hits(files['small-model'][0])}")
        check("C7c long streamed exactly once more",
              hits(files["long-model"][1]) == ls_before + 1, f"long streams {ls_before}->{hits(files['long-model'][1])}")

        # C8: no-fit via max_tokens reserve: prompt ~6.4k + 2,000,000
        # reserve > 1M window -> honest 400, zero provider calls.
        # A DISTINCT prompt again (the C3/C7 prompts are cached).
        s_before, l_before = hits(files["small-model"][0]), hits(files["long-model"][0])
        st, body = chat("small-model", UNIT3 * 400, raw_key, max_tokens=2000000)
        check("C8 no-fit -> 400 naming required vs largest",
              st == 400 and "largest available context window is 1048576" in body,
              f"status={st} {body[:200]}")
        check("C8b zero provider calls on the no-fit request",
              hits(files["small-model"][0]) == s_before and hits(files["long-model"][0]) == l_before,
              f"small {s_before}->{hits(files['small-model'][0])} long {l_before}->{hits(files['long-model'][0])}")

        # C9: dashboard serves the Models Fit column + summary, and
        # the guard feed lists the new route.
        st, body = req("GET", "/dashboard")
        check("C9 dashboard serves Fit column + fit summary",
              st == 200 and "<th>Fit</th>" in body and 'id="modelFitSummary"' in body
              and "context too small for this prompt" in body,
              f"status={st}")
        st, body = req("GET", "/dashboard/guard")
        g = json.loads(body) if st == 200 else {}
        routes = [r.get("path") for t in (g.get("tiers") or []) for r in (t.get("routes") or [])]
        check("C9b guard feed lists GET /dashboard/context-fit",
              "/dashboard/context-fit" in routes,
              f"status={st} routes={len(routes)}")
        # C10: no-fit decision recorded too (chain_used none).
        st, body = req("GET", "/dashboard/context-fit")
        f = json.loads(body) if st == 200 else {}
        d = f.get("decision") or {}
        check("C10 last decision is the no-fit (chain_used none)",
              d.get("chain_used") == "none" and d.get("error") != "",
              f"chain={d.get('chain_used')} error={str(d.get('error'))[:80]}")
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
