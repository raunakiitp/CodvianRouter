#!/usr/bin/env python3
"""Live smoke for the forced-reasoning policy (CHECKLIST 311-312).

Real binary + two Python OpenAI mocks. No fake data: every
assertion reads a live trace, a live feed, or a value the mock
actually received on the wire.

Mocks: "flaky" serves model-a, "solid" serves model-b. Each mock
records the reasoning_effort of every chat POST it receives
("<absent>" when the field is missing) into its efforts file, so
the script can prove what the provider actually received — not
just what the gateway resolved.

Config: reasoning.forced_effort rules —
  model-a -> high (no override: the policy always wins),
  model-b -> high with allow_client_override,
  intent/best -> high (a route policy, matched on the model as
  requested). intent fast -> model-a (low effort), best ->
  model-b (high effort); DefaultModel model-b.

Flow (one server run):
  Phase A (policy wins): a model-a chat with client
    reasoning_effort low reaches the mock as high; the trace
    names the policy (forced:high:policy:model-a).
  Phase B (override flag): a model-b chat with client low
    reaches the mock as low; the trace records the client
    override (client-override:low:policy:model-b).
  Phase C (override, no client value): a model-b chat with no
    client effort still forces high.
  Phase D (policy beats tier): an intent/fast chat (tier pins
    low) with client medium reaches the mock as high — policy >
    tier > client.
  Phase E (route policy): an intent/best chat matches the rule
    on the requested route; the trace names intent/best.
  Phase F (stream): a streaming model-a chat with client low
    still carries high to the mock.
  Phase G (dashboard): the served page carries the forced-effort
    decision-view line and the effort_policy trace chip, and no
    Playground.
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-311-312")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21471
FLAKY_PORT = 21472
SOLID_PORT = 21473
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

def stream_chat(model, prompt, key, extra=None):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}], "stream": True}
    if extra:
        body.update(extra)
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

def trace_attempt(trace, model):
    atts = trace.get("attempts", []) if isinstance(trace, dict) else []
    served = [a for a in atts if a.get("model") == model
              and a.get("outcome") == "success"]
    return served[0] if len(served) == 1 else None

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
    cfg["reasoning"] = {
        "forced_effort": [
            {"model": "model-a", "effort": "high"},
            {"model": "model-b", "effort": "high", "allow_client_override": True},
            {"model": "intent/best", "effort": "high"},
        ],
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

        # ---- Phase A: policy wins over the client value ----
        st, h, body = chat("model-a", PROMPTS[0], raw_key,
                           extra={"reasoning_effort": "low"})
        check("A1 model-a chat with client low returns 200",
              st == 200, f"status={st} {body[:160]}")
        check("A2 model-a mock received high on the wire (policy beats client low)",
              efforts(files["flaky"]) == ["high"],
              f"flaky efforts={efforts(files['flaky'])}")
        st, trace = jget("/dashboard/trace")
        at = trace_attempt(trace, "model-a")
        check("A3 trace names the policy that set the effort",
              st == 200 and at is not None and at.get("effort") == "high"
              and at.get("effort_policy") == "forced:high:policy:model-a",
              f"status={st} attempt={at}")

        # ---- Phase B: allow_client_override lets the client win ----
        st, h, body = chat("model-b", PROMPTS[1], raw_key,
                           extra={"reasoning_effort": "low"})
        check("B1 model-b chat with client low returns 200",
              st == 200, f"status={st} {body[:160]}")
        check("B2 model-b mock received low on the wire (client overrides policy)",
              efforts(files["solid"]) == ["low"],
              f"solid efforts={efforts(files['solid'])}")
        st, trace = jget("/dashboard/trace")
        at = trace_attempt(trace, "model-b")
        check("B3 trace records the client override, never silently",
              st == 200 and at is not None and at.get("effort") == "low"
              and at.get("effort_policy") == "client-override:low:policy:model-b",
              f"status={st} attempt={at}")

        # ---- Phase C: override flag, no client value -> policy still forces ----
        st, h, body = chat("model-b", PROMPTS[2], raw_key)
        check("C1 model-b chat with no client effort returns 200",
              st == 200, f"status={st} {body[:160]}")
        check("C2 model-b mock received high (no client value, policy still forces)",
              efforts(files["solid"])[-1] == "high",
              f"solid efforts={efforts(files['solid'])}")
        st, trace = jget("/dashboard/trace")
        at = trace_attempt(trace, "model-b")
        check("C3 trace names the policy (forced, not override)",
              st == 200 and at is not None and at.get("effort") == "high"
              and at.get("effort_policy") == "forced:high:policy:model-b",
              f"status={st} attempt={at}")

        # ---- Phase D: policy beats the intent-tier effort ----
        st, h, body = chat("intent/fast", PROMPTS[3], raw_key,
                           extra={"reasoning_effort": "medium"})
        check("D1 intent/fast chat with client medium returns 200",
              st == 200, f"status={st} {body[:160]}")
        check("D2 model-a mock received high (policy beats tier low and client medium)",
              efforts(files["flaky"])[-1] == "high",
              f"flaky efforts={efforts(files['flaky'])}")
        st, trace = jget("/dashboard/trace")
        at = trace_attempt(trace, "model-a")
        check("D3 trace names the policy on the resolved model",
              st == 200 and at is not None and at.get("effort") == "high"
              and at.get("effort_policy") == "forced:high:policy:model-a",
              f"status={st} attempt={at}")

        # ---- Phase E: route policy matches the model as requested ----
        st, h, body = chat("intent/best", PROMPTS[4], raw_key,
                           extra={"reasoning_effort": "low"})
        check("E1 intent/best chat with client low returns 200",
              st == 200, f"status={st} {body[:160]}")
        check("E2 model-b mock received high on the wire",
              efforts(files["solid"])[-1] == "high",
              f"solid efforts={efforts(files['solid'])}")
        st, trace = jget("/dashboard/trace")
        at = trace_attempt(trace, "model-b")
        check("E3 trace names the route policy (intent/best), not the model rule",
              st == 200 and at is not None and at.get("effort") == "high"
              and at.get("effort_policy") == "forced:high:policy:intent/best",
              f"status={st} attempt={at}")

        # ---- Phase F: streaming carries the forced effort ----
        st, h, body = stream_chat("model-a", PROMPTS[5], raw_key,
                                  extra={"reasoning_effort": "low"})
        check("F1 streaming model-a chat 200 with SSE content",
              st == 200 and "says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("F2 streaming chat carried high to the mock",
              efforts(files["flaky"])[-1] == "high",
              f"flaky efforts={efforts(files['flaky'])}")

        # ---- Phase G: dashboard ----
        st, _, dash = req("GET", "/dashboard")
        check("G1 dashboard shows the forced-effort line and effort_policy chip, no Playground",
              st == 200 and "Forced effort" in dash and "effort_policy" in dash
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
