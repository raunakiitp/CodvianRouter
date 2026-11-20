#!/usr/bin/env python3
"""Live smoke for intent-tier resolved effort onto the engine
attempt (CHECKLIST 269-270): the 259-260 slice's open gap.

Real binary + two Python OpenAI mocks. No fake data: every
assertion reads a live header, a live feed, a live trace, or a
value the mock actually received on the wire.

Mocks: "flaky" serves model-a, "solid" serves model-b. Each mock
records the reasoning_effort of every chat POST it receives
("<absent>" when the field is missing) into its efforts file, so
the script can prove what the provider actually received — not
just what the gateway resolved.

Config: intent fast -> model-a (low effort), best -> model-b
(high effort); coding tier candidates model-a + model-b;
DefaultModel model-b.

Flow (one server run):
  Phase 1 (preview): intent/fast previews to model-a at effort
    low, intent/best previews to model-b at effort high.
  Phase 2 (wire proof): one intent/fast chat and one intent/best
    chat; the mocks record "low" and "high" respectively — the
    two differ exactly as the tier definitions say. Response
    headers carry the same efforts. The route trace after each
    chat shows the serving attempt at that effort (the effort
    the provider actually received, in the dashboard's data).
  Phase 3 (precedence + stream): a tier-pinned effort overrides
    a client-supplied reasoning_effort on the wire; a streaming
    intent/best chat also carries "high" to the mock.
  Phase 4 (unpinned tier): remapping fast to no pinned effort
    lets the client's own reasoning_effort pass through, and the
    trace then records no effort for the attempt.
  Phase 5 (dashboard): the served page carries the new
    "Provider received effort" decision-view line and no longer
    carries the stale "carrying it onto the engine attempt is a
    follow-up" copy; no "Playground".
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-269-270")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21461
FLAKY_PORT = 21462
SOLID_PORT = 21463
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; efforts_file = sys.argv[3]; model = sys.argv[4]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}).encode()
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
        eff = req.get("reasoning_effort")
        with open(efforts_file, "a") as f:
            f.write(("<absent>" if eff is None else str(eff)) + "\n")
        if req.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            chunk = ('data: {"id":"x","object":"chat.completion.chunk","model":"%s",'
                     '"choices":[{"index":0,"delta":{"role":"assistant","content":"%s says hi"},'
                     '"finish_reason":null}]}\n\n' % (model, name)).encode()
            done = ('data: {"id":"x","object":"chat.completion.chunk","model":"%s",'
                    '"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n' % model).encode()
            self.wfile.write(chunk); self.wfile.write(done); self.wfile.flush()
            return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": name + " says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}).encode()
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

def efforts(fn):
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
# trailing number (near-identical prompts score ~0.97 similar and
# the second one is served from cache, never reaching the mock).
PROMPTS = [
    "Tell me a joke about a lighthouse keeper who counts the waves",
    "What is the best way to brew coffee at high altitude?",
    "Explain why the sky looks blue to a curious child",
    "Write a haiku about rain on a tin roof in autumn",
    "Give me a one-sentence summary of how bicycles stay upright",
    "What should I pack for a three-day hike in the mountains?",
    "Tell me a short story about a cat that learned to open doors",
    "How do I fix a squeaky door hinge with household items?",
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
    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    procs = []
    files = {}
    for port, name, model in ((FLAKY_PORT, "flaky", "model-a"),
                              (SOLID_PORT, "solid", "model-b")):
        ef = os.path.join(HOME, name + ".efforts")
        open(ef, "w").write("")
        files[name] = ef
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, ef, model],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        procs.append(p)
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
        "tier_models": {"coding": ["model-a", "model-b"]},
        "default_model": "model-b",
        "intent_tiers": {"fast": {"model": "model-a", "effort": "low"},
                         "best": {"model": "model-b", "effort": "high"}},
    }
    with open(cfg_path, "w") as f:
        json.dump(cfg, f, indent=2)

    router = start_router(env)
    if router is None:
        return 1
    procs.append(router)
    try:
        for name, port in (("flaky", FLAKY_PORT), ("solid", SOLID_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}",
                               "api_key": name + "-key"}, key=raw_key)
            check(f"A0 add provider {name}", st == 201, f"status={st} {body[:160]}")

        # ---- Phase 1: preview ----
        st, dec = jget("/dashboard/auto-route?model=intent/fast")
        check("B1 preview intent/fast resolves model-a at effort low",
              st == 200 and dec.get("model") == "model-a" and dec.get("effort") == "low"
              and dec.get("intent_tier") == "fast",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")
        st, dec = jget("/dashboard/auto-route?model=intent/best")
        check("B2 preview intent/best resolves model-b at effort high",
              st == 200 and dec.get("model") == "model-b" and dec.get("effort") == "high"
              and dec.get("intent_tier") == "best",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")

        # ---- Phase 2: wire proof (mock records what it received) ----
        st, h, body = chat("intent/fast", PROMPTS[0], raw_key)
        check("C1 intent/fast chat 200 with X-Codvian-Intent-Effort: low",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "low"
              and h.get("X-Codvian-Intent-Tier") == "fast",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r}")
        check("C2 model-a mock received reasoning_effort low on the wire",
              efforts(files["flaky"]) == ["low"],
              f"flaky efforts={efforts(files['flaky'])}")
        st, h, body = chat("intent/best", PROMPTS[1], raw_key)
        check("C3 intent/best chat 200 with X-Codvian-Intent-Effort: high",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "high"
              and h.get("X-Codvian-Intent-Tier") == "best",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r}")
        check("C4 model-b mock received reasoning_effort high on the wire",
              efforts(files["solid"]) == ["high"],
              f"solid efforts={efforts(files['solid'])}")
        check("C5 provider-received effort differs by tier (low vs high)",
              efforts(files["flaky"]) == ["low"] and efforts(files["solid"]) == ["high"],
              f"flaky={efforts(files['flaky'])} solid={efforts(files['solid'])}")

        # ---- Trace shows the effort the provider actually received ----
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_best = [a for a in atts if a.get("model") == "model-b"
                       and a.get("outcome") == "success"]
        check("C6 trace: serving attempt for model-b carries effort high",
              st == 200 and len(served_best) == 1 and served_best[0].get("effort") == "high",
              f"status={st} attempts={atts}")
        st, h, body = chat("intent/fast", PROMPTS[2], raw_key)
        check("C7 second intent/fast chat 200", st == 200, f"status={st}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_fast = [a for a in atts if a.get("model") == "model-a"
                       and a.get("outcome") == "success"]
        check("C8 trace: serving attempt for model-a carries effort low",
              st == 200 and len(served_fast) == 1 and served_fast[0].get("effort") == "low",
              f"status={st} attempts={atts}")

        # ---- Phase 3: precedence + stream ----
        st, h, body = chat("intent/fast", PROMPTS[3], raw_key,
                           extra={"reasoning_effort": "high"})
        check("C9 tier overrides client effort on the wire (client high -> mock low)",
              st == 200 and efforts(files["flaky"])[-1] == "low",
              f"status={st} flaky efforts={efforts(files['flaky'])}")
        st, h, body = stream_chat("intent/best", PROMPTS[4], raw_key)
        check("C10 streaming intent/best chat 200 with SSE content",
              st == 200 and "says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("C11 streaming chat carried effort high to the mock",
              efforts(files["solid"])[-1] == "high",
              f"solid efforts={efforts(files['solid'])}")

        # ---- Phase 4: unpinned tier passes the client value through ----
        st, _, body = req("POST", "/dashboard/intent-tiers",
                          {"tier": "fast", "model": "model-a"})
        check("D1 runtime remap fast -> model-a with no effort pinned",
              st == 200, f"status={st} {body[:160]}")
        st, h, body = chat("intent/fast", PROMPTS[5], raw_key,
                           extra={"reasoning_effort": "medium"})
        check("D2 unpinned tier passes client effort medium to the mock",
              st == 200 and efforts(files["flaky"])[-1] == "medium",
              f"status={st} flaky efforts={efforts(files['flaky'])}")
        check("D3 unpinned tier: response carries no X-Codvian-Intent-Effort",
              "X-Codvian-Intent-Effort" not in h,
              f"headers={h.get('X-Codvian-Intent-Effort')!r}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served = [a for a in atts if a.get("model") == "model-a"
                  and a.get("outcome") == "success"]
        check("D4 trace: unpinned attempt records no effort",
              st == 200 and len(served) == 1 and not served[0].get("effort"),
              f"status={st} attempts={atts}")

        # ---- Phase 5: dashboard copy ----
        st, _, dash = req("GET", "/dashboard")
        check("E1 dashboard shows Provider-received-effort line, stale copy gone, no Playground",
              st == 200 and "Provider received effort" in dash
              and "carrying it onto the engine attempt is a follow-up" not in dash
              and "Playground" not in dash,
              f"status={st}")
    finally:
        for p in procs:
            stop_router(p)

    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
