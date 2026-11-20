#!/usr/bin/env python3
"""Live smoke for Vertex effort forwarding (CHECKLIST 303-304):
the tier's effort reaches the Vertex wire as
generationConfig.thinkingConfig.thinkingBudget, resolved from
the operator-declared `reasoning.effort_budgets.vertex` entry
(never a "gemini" budget copied over, never an invented
default).

Real binary + two Python mocks: a Vertex API mock
(:generateContent / :streamGenerateContent?alt=sse, recording
the thinkingBudget of every chat POST) and an OAuth2 token
mock (JWT-bearer exchange -> fixed access token). The
provider key is a real service-account JSON whose RSA key is
generated with openssl for this run; nothing here talks to
Google.

Config: intent fast -> gemini-2.0-flash (low effort), best ->
gemini-1.5-pro (high effort); reasoning.effort_budgets
vertex {low: 1024, high: 16384}, gemini {low: 2048, high:
8192} — the gemini entry exists precisely so the smoke can
prove Vertex does NOT read it (fast must record 1024, the
vertex value, not 2048).

Flow (two server runs):
  Run 1 (table declared):
    Phase 1 (preview): intent/fast previews to
      gemini-2.0-flash at effort low, intent/best to
      gemini-1.5-pro at high.
    Phase 2 (wire proof): one intent/fast chat and one
      intent/best chat; the Vertex mock records budgets 1024
      and 16384 — the two differ by tier exactly as the
      operator's vertex table says, and neither is the gemini
      value. The route trace after each chat shows the serving
      attempt's effort_forwarding with the same value.
    Phase 3 (stream): a streaming intent/fast chat and a
      streaming intent/best chat carry the same budgets.
  Run 2 (table removed from the config file, same mocks, same
    provider): one intent/fast chat — the Vertex mock records
    NO thinkingConfig at all, and the trace says
    not-forwarded:no-operator-budget-table.
  Phase 4 (dashboard): the served page carries the Vertex
    forwarding copy and the decision-view forwarding text; no
    "Playground".
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-303-304")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21481
VERTEX_PORT = 21482
TOKEN_PORT = 21483
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

VERTEX_MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); budgets_file = sys.argv[2]
STREAM_BODY = ('data: {"candidates":[{"content":{"role":"model","parts":[{"text":"vertex says hi "}]},"finishReason":"STOP"}]}\n\n'
               'data: {"candidates":[{"content":{"role":"model","parts":[{"text":"there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}\n\n')
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path.endswith("/publishers/google/models"):
            body = json.dumps({"models": [
                {"name": "projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.0-flash"},
                {"name": "projects/proj-1/locations/us-central1/publishers/google/models/gemini-1.5-pro"}]}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln) if ln else b""
        try: req = json.loads(raw.decode() or "{}")
        except Exception: req = {}
        # Record exactly what the provider received on the wire.
        gc = (req.get("generationConfig") or {})
        tc = gc.get("thinkingConfig") or {}
        budget = tc.get("thinkingBudget")
        with open(budgets_file, "a") as f:
            f.write(("<absent>" if budget is None else str(budget)) + "\n")
        if ":streamGenerateContent" in self.path:
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            self.wfile.write(STREAM_BODY.encode()); self.wfile.flush()
            return
        if ":generateContent" in self.path:
            body = json.dumps({"candidates": [{"content": {"role": "model", "parts": [{"text": "vertex says hi"}]}, "finishReason": "STOP"}],
                "usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 8, "totalTokenCount": 20},
                "modelVersion": "gemini-2.0-flash-001"}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
'''

TOKEN_MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs
port = int(sys.argv[1])
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln) if ln else b""
        form = parse_qs(raw.decode() or "")
        if form.get("grant_type") != ["urn:ietf:params:oauth:grant-type:jwt-bearer"] or not form.get("assertion"):
            self.send_response(400); self.end_headers()
            self.wfile.write(b'{"error":"invalid_request"}')
            return
        body = json.dumps({"access_token": "tok-vertex-smoke", "expires_in": 3600, "token_type": "Bearer"}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
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

def budgets(fn):
    try:
        with open(fn) as f:
            return [ln.strip() for ln in f if ln.strip()]
    except Exception:
        return []

def chat(model, prompt, key, extra=None):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if extra:
        body.update(extra)
    return req("POST", "/v1/chat/completions", body, key)

def stream_chat(model, prompt, key):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}], "stream": True}
    return req("POST", "/v1/chat/completions", body, key)

def jget(path, key=None):
    st, _, body = req("GET", path, key=key)
    try:
        return st, json.loads(body)
    except Exception:
        return st, {}

def start_router(env):
    p = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                         env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for _ in range(50):
        st, _, _ = req("GET", "/healthz")
        if st == 200:
            return p
        time.sleep(0.2)
    print("ROUTER never became ready")
    try:
        print(p.stdout.read())
    except Exception:
        pass
    return None

def stop_router(p):
    p.terminate()
    try:
        p.wait(timeout=10)
    except Exception:
        p.kill()

# Genuinely different prompts: the semantic cache must not collapse
# two chats, so the prompts share no topic or wording, not just a
# trailing number.
PROMPTS = [
    "Tell me a joke about a lighthouse keeper who counts the waves",
    "What is the best way to brew coffee at high altitude?",
    "Explain why the sky looks blue to a curious child",
    "Write a haiku about rain on a tin roof in autumn",
    "Give me a one-sentence summary of how bicycles stay upright",
    "What should I pack for a three-day hike in the mountains?",
    "Tell me a short story about a cat that learned to open doors",
    "How do I fix a squeaky door hinge with household items?",
    "Why do we see lightning before we hear thunder?",
    "What makes a good sourdough starter smell the way it does?",
]

def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN, "./cmd/codvian-router"],
                       cwd=ROOT,
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1
    vertex_py = os.path.join(HOME, "mock-vertex.py")
    open(vertex_py, "w").write(VERTEX_MOCK_PY)
    token_py = os.path.join(HOME, "mock-token.py")
    open(token_py, "w").write(TOKEN_MOCK_PY)
    procs = []
    budgets_file = os.path.join(HOME, "vertex.budgets")
    open(budgets_file, "w").write("")
    for port, py, arg in ((VERTEX_PORT, vertex_py, budgets_file),
                          (TOKEN_PORT, token_py, None)):
        args = [sys.executable, py, str(port)]
        if arg:
            args.append(arg)
        procs.append(subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
    # Service-account key for this run only: a real RSA key from
    # openssl (PKCS#8 PEM, as the client requires), token_uri at
    # the token mock — nothing here talks to Google.
    key_pem = os.path.join(HOME, "sa-key.pem")
    r = subprocess.run(["openssl", "genpkey", "-algorithm", "RSA",
                        "-pkeyopt", "rsa_keygen_bits:2048", "-out", key_pem],
                       capture_output=True, text=True)
    if r.returncode != 0:
        print("OPENSSL KEYGEN FAILED:\n" + r.stdout + r.stderr)
        return 1
    with open(key_pem) as f:
        pem = f.read()
    sa_json = json.dumps({
        "type": "service-account",
        "project_id": "proj-1",
        "client_email": "vertex-smoke@proj-1.iam.gserviceaccount.com",
        "private_key": pem,
        "token_uri": f"http://127.0.0.1:{TOKEN_PORT}/token",
    })
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
    for root, _, fs in os.walk(HOME):
        for f in fs:
            if f == "config.json":
                cfg_path = os.path.join(root, f)
    if not cfg_path:
        print("config.json not found under " + HOME)
        return 1
    with open(cfg_path) as f:
        cfg = json.load(f)
    cfg["auto_router"] = {
        "tier_models": {"coding": ["gemini-2.0-flash", "gemini-1.5-pro"]},
        "default_model": "gemini-2.0-flash",
        "intent_tiers": {"fast": {"model": "gemini-2.0-flash", "effort": "low"},
                         "best": {"model": "gemini-1.5-pro", "effort": "high"}},
    }
    # The gemini entry exists so the smoke can prove Vertex does
    # NOT read it: fast must record the vertex low (1024), not
    # the gemini low (2048).
    cfg["reasoning"] = {
        "effort_budgets": {
            "vertex": {"low": 1024, "high": 16384},
            "gemini": {"low": 2048, "high": 8192},
        }
    }
    with open(cfg_path, "w") as f:
        json.dump(cfg, f, indent=2)

    router = start_router(env)
    if router is None:
        return 1
    procs.append(router)
    try:
        # Catalog provider "vertex" with a base_url override at
        # the mock: the wire format is the catalog's own
        # (vertex), the key is the service-account JSON.
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "vertex",
                           "base_url": f"http://127.0.0.1:{VERTEX_PORT}/v1/projects/proj-1/locations/us-central1",
                           "api_key": sa_json}, key=raw_key)
        check("A0 add provider vertex (wire vertex)", st == 201 and '"wire_format":"vertex"' in body.replace(" ", ""),
              f"status={st} {body[:300]}")

        # ---- Phase 1: preview ----
        st, dec = jget("/dashboard/auto-route?model=intent/fast")
        check("B1 preview intent/fast resolves gemini-2.0-flash at effort low",
              st == 200 and dec.get("model") == "gemini-2.0-flash" and dec.get("effort") == "low"
              and dec.get("intent_tier") == "fast",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")
        st, dec = jget("/dashboard/auto-route?model=intent/best")
        check("B2 preview intent/best resolves gemini-1.5-pro at effort high",
              st == 200 and dec.get("model") == "gemini-1.5-pro" and dec.get("effort") == "high"
              and dec.get("intent_tier") == "best",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")

        # ---- Phase 2: wire proof (mock records what it received) ----
        st, h, body = chat("intent/fast", PROMPTS[0], raw_key)
        check("C1 intent/fast chat 200 with X-Codvian-Intent-Effort: low",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "low"
              and h.get("X-Codvian-Intent-Tier") == "fast",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r} body={body[:120]!r}")
        check("C2 Vertex mock received thinkingConfig.thinkingBudget 1024 (vertex low, NOT gemini 2048)",
              budgets(budgets_file) == ["1024"],
              f"vertex budgets={budgets(budgets_file)}")
        st, h, body = chat("intent/best", PROMPTS[1], raw_key)
        check("C3 intent/best chat 200 with X-Codvian-Intent-Effort: high",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "high"
              and h.get("X-Codvian-Intent-Tier") == "best",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r} body={body[:120]!r}")
        check("C4 Vertex mock received thinkingConfig.thinkingBudget 16384 (vertex high, NOT gemini 8192)",
              budgets(budgets_file) == ["1024", "16384"],
              f"vertex budgets={budgets(budgets_file)}")
        check("C5 forwarded budgets differ by tier (1024 vs 16384)",
              budgets(budgets_file) == ["1024", "16384"],
              f"vertex budgets={budgets(budgets_file)}")

        # ---- Trace shows the forwarding per attempt ----
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_best = [a for a in atts if a.get("model") == "gemini-1.5-pro"
                       and a.get("outcome") == "success"]
        check("C6 trace: serving Vertex best attempt carries effort_forwarding forwarded:thinkingConfig.thinkingBudget=16384",
              st == 200 and len(served_best) == 1
              and served_best[0].get("effort_forwarding") == "forwarded:thinkingConfig.thinkingBudget=16384",
              f"status={st} attempts={atts}")
        st, h, body = chat("intent/fast", PROMPTS[2], raw_key)
        check("C7 second intent/fast chat 200", st == 200, f"status={st}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_fast = [a for a in atts if a.get("model") == "gemini-2.0-flash"
                       and a.get("outcome") == "success"]
        check("C8 trace: serving Vertex fast attempt carries effort_forwarding forwarded:thinkingConfig.thinkingBudget=1024",
              st == 200 and len(served_fast) == 1
              and served_fast[0].get("effort_forwarding") == "forwarded:thinkingConfig.thinkingBudget=1024",
              f"status={st} attempts={atts}")

        # ---- Phase 3: stream path carries the same budgets ----
        st, h, body = stream_chat("intent/fast", PROMPTS[3], raw_key)
        check("C9 streaming intent/fast chat 200 with SSE content",
              st == 200 and "vertex says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("C10 streaming chat carried budget 1024 to the Vertex mock",
              budgets(budgets_file)[-1] == "1024",
              f"vertex budgets={budgets(budgets_file)}")
        st, h, body = stream_chat("intent/best", PROMPTS[4], raw_key)
        check("C11 streaming intent/best chat 200 with SSE content",
              st == 200 and "vertex says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("C12 streaming chat carried budget 16384 to the Vertex mock",
              budgets(budgets_file)[-1] == "16384",
              f"vertex budgets={budgets(budgets_file)}")
    finally:
        stop_router(router)
        procs.remove(router)

    # ---- Run 2: no table declared — nothing is sent ----
    with open(cfg_path) as f:
        cfg = json.load(f)
    cfg.pop("reasoning", None)
    with open(cfg_path, "w") as f:
        json.dump(cfg, f, indent=2)
    router = start_router(env)
    if router is None:
        return 1
    procs.append(router)
    try:
        before = len(budgets(budgets_file))
        st, h, body = chat("intent/fast", PROMPTS[5], raw_key)
        check("D1 no-table intent/fast chat 200",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "low",
              f"status={st} body={body[:120]!r}")
        check("D2 Vertex mock received NO thinkingConfig without the table",
              len(budgets(budgets_file)) == before + 1
              and budgets(budgets_file)[-1] == "<absent>",
              f"vertex budgets={budgets(budgets_file)}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served = [a for a in atts if a.get("model") == "gemini-2.0-flash"
                  and a.get("outcome") == "success"]
        check("D3 trace: no-table attempt records not-forwarded:no-operator-budget-table",
              st == 200 and len(served) == 1
              and served[0].get("effort_forwarding") == "not-forwarded:no-operator-budget-table",
              f"status={st} attempts={atts}")

        # ---- Phase 4: dashboard copy ----
        st, _, dash = req("GET", "/dashboard")
        check("E1 dashboard shows Vertex forwarding copy, decision-view forwarding text, no Playground",
              st == 200 and "thinkingConfig.thinkingBudget" in dash
              and "Vertex" in dash
              and "no operator budget table" in dash
              and "Provider received effort" in dash
              and "Playground" not in dash,
              f"status={st}")
    finally:
        for p in procs:
            try:
                stop_router(p)
            except Exception:
                pass

    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
