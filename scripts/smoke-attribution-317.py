#!/usr/bin/env python3
"""Live smoke for outcome attribution under concurrency (CHECKLIST 317-318).

Real binary + two Python OpenAI mocks. No fake data: every
assertion reads a live response header, a live feed, or a value
the mock actually received on the wire.

Mocks: "prov-a" serves model-a (fast), "prov-b" serves model-b
(slow: every chat POST sleeps 0.8 s, so the best-tier requests
are still in flight when the fast-tier ones finish — the exact
interleaving where the old global last-decision/last-trace read
misattributed).

Config: intent tiers fast -> model-a (low effort), best ->
model-b (high effort); DefaultModel model-b.

Flow (one server run):
  Phase A (parallel attribution): 8 parallel chats — 4x
    intent/fast + 4x intent/best, genuinely different prompts
    (the semantic cache must not collapse them). Every response
    must carry its OWN decision id (8 distinct ids), its OWN
    model + intent tier, and the learning loop
    (GET /dashboard/suggestions) must show exactly 4 requests in
    each of the (intent/fast, model-a) and (intent/best,
    model-b) cells — no cell with 8, none missing, no neighbour
    cell. On the old code the slow best-tier outcomes landed in
    the fast-tier cell (or vice versa) and the headers named the
    neighbour's decision id.
  Phase B (feedback): one classified "auto" chat's decision id
    accepts a signal — the POST must succeed (the id was
    registered for THIS request) and GET /dashboard/feedback must
    show it under the right decision id. (Intent decisions are
    not used here: they carry no classification tier, so the
    feedback store cannot register them — a pre-existing schema
    gap outside 317-318.)
  Phase C (streams): 2 parallel streams (one fast, one slow) —
    both 200, and the suggestions cells grow to 5 requests each
    (requests only: streams record as failures with reason
    upstream_error — a pre-existing classification quirk outside
    317-318, not asserted here).
  Phase D (dashboard): the served page carries no Playground.
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor

HOME = os.path.expanduser("~/.tmp/smoke-317-318")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21531
PROVA_PORT = 21532
PROVB_PORT = 21533
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, os, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; model = sys.argv[3]; delay = float(sys.argv[4])
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
        if delay:
            time.sleep(delay)
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln) if ln else b""
        try: req = json.loads(raw.decode() or "{}")
        except Exception: req = {}
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
        with urllib.request.urlopen(r, timeout=60) as resp:
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    except Exception as e:
        return 0, {}, str(e)

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
FAST_PROMPTS = [
    "Tell me a joke about a lighthouse keeper who counts the waves",
    "What is the best way to brew coffee at high altitude?",
    "Explain why the sky looks blue to a curious child",
    "Write a haiku about rain on a tin roof in autumn",
]
BEST_PROMPTS = [
    "Give me a one-sentence summary of how bicycles stay upright",
    "What should I pack for a three-day hike in the mountains?",
    "Tell me a short story about a cat that learned to open doors",
    "How do I fix a squeaky door hinge with household items?",
]

# Fresh prompts for the stream phase: reusing a Phase A prompt would
# let the semantic cache serve the stream without calling a provider
# (and a cache hit records no outcome), so these must be new topics.
STREAM_PROMPTS = [
    "What is the difference between a violin and a viola?",
    "Explain how a refrigerator keeps food cold in one paragraph",
]

# Fresh prompt for the feedback phase (model "auto"): an intent
# decision carries no classification tier, so the feedback store
# (which requires a known tier) cannot register it — a pre-existing
# schema gap outside 317-318, not a concurrency issue. A classified
# "auto" decision carries a real tier and registers, which is what
# this phase proves.
FEEDBACK_PROMPT = "Write a Python function that reverses a string"

def chat(model, prompt, key, stream=False):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if stream:
        body["stream"] = True
    return req("POST", "/v1/chat/completions", body, key)

def cell(stats, task, model):
    for s in stats:
        if s.get("task") == task and s.get("model") == model:
            return s
    return None

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
    for port, name, model, delay in ((PROVA_PORT, "prov-a", "model-a", "0"),
                                     (PROVB_PORT, "prov-b", "model-b", "0.8")):
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, model, delay],
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
        for name, port in (("prov-a", PROVA_PORT), ("prov-b", PROVB_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}",
                               "api_key": name + "-key"}, key=raw_key)
            check(f"A0 add provider {name}", st == 201, f"status={st} {body[:160]}")

        # ---- Phase A: 8 parallel chats, 4 fast + 4 best ----
        jobs = [("intent/fast", p) for p in FAST_PROMPTS] + \
               [("intent/best", p) for p in BEST_PROMPTS]
        with ThreadPoolExecutor(max_workers=8) as ex:
            results = list(ex.map(lambda j: chat(j[0], j[1], raw_key), jobs))
        all200 = all(st == 200 for st, _, _ in results)
        check("A1 all 8 parallel chats 200", all200,
              "statuses=" + ",".join(str(st) for st, _, _ in results))
        ids = [h.get("X-Codvian-Auto-Decision-Id", "") for _, h, _ in results]
        check("A2 8 distinct decision ids", len(set(ids)) == 8 and all(ids),
              f"ids={ids}")
        fast_ok = all(
            h.get("X-Codvian-Auto-Model") == "model-a" and
            h.get("X-Codvian-Intent-Tier") == "fast"
            for (_, h, _), (m, _) in zip(results, jobs) if m == "intent/fast")
        best_ok = all(
            h.get("X-Codvian-Auto-Model") == "model-b" and
            h.get("X-Codvian-Intent-Tier") == "best"
            for (_, h, _), (m, _) in zip(results, jobs) if m == "intent/best")
        check("A3 fast responses name model-a + tier fast", fast_ok)
        check("A4 best responses name model-b + tier best", best_ok)
        st, sugg = jget("/dashboard/suggestions", key=raw_key)
        stats = sugg.get("stats", []) if isinstance(sugg, dict) else []
        fcell = cell(stats, "intent/fast", "model-a")
        bcell = cell(stats, "intent/best", "model-b")
        check("A5 suggestions cell (intent/fast, model-a) has 4 requests",
              fcell is not None and fcell.get("requests") == 4 and fcell.get("successes") == 4,
              f"cell={fcell}")
        check("A6 suggestions cell (intent/best, model-b) has 4 requests",
              bcell is not None and bcell.get("requests") == 4 and bcell.get("successes") == 4,
              f"cell={bcell}")
        check("A7 no other outcome cells", len(stats) == 2, f"stats={stats}")

        # ---- Phase B: feedback attaches to this request's decision ----
        # A classified "auto" decision (real tier) registers in the
        # feedback store; the POST must succeed and the recent feed
        # must show it under that decision id.
        st, h, _ = chat("auto", FEEDBACK_PROMPT, raw_key)
        check("B0 auto chat 200 with a decision id", st == 200 and
              h.get("X-Codvian-Auto-Decision-Id", "") != "",
              f"status={st} id={h.get('X-Codvian-Auto-Decision-Id', '')}")
        auto_id = h.get("X-Codvian-Auto-Decision-Id", "")
        st, _, body = req("POST", "/dashboard/feedback",
                          {"decision_id": auto_id, "signal": "accept"}, key=raw_key)
        check("B1 feedback POST for own decision id succeeds", st in (200, 201),
              f"status={st} {body[:160]}")
        st, fb = jget("/dashboard/feedback", key=raw_key)
        recent = fb.get("recent", []) if isinstance(fb, dict) else []
        check("B2 feedback recent shows the decision id",
              any(ev.get("decision_id") == auto_id for ev in recent),
              f"recent={recent[:2]}")

        # ---- Phase C: 2 parallel streams ----
        with ThreadPoolExecutor(max_workers=2) as ex:
            sresults = list(ex.map(
                lambda j: chat(j[0], j[1], raw_key, stream=True),
                [("intent/fast", STREAM_PROMPTS[0]),
                 ("intent/best", STREAM_PROMPTS[1])]))
        check("C1 both parallel streams 200",
              all(st == 200 for st, _, _ in sresults),
              "statuses=" + ",".join(str(st) for st, _, _ in sresults))
        st, sugg = jget("/dashboard/suggestions", key=raw_key)
        stats = sugg.get("stats", []) if isinstance(sugg, dict) else []
        fcell = cell(stats, "intent/fast", "model-a")
        bcell = cell(stats, "intent/best", "model-b")
        check("C2 after streams: (intent/fast, model-a) has 5 requests",
              fcell is not None and fcell.get("requests") == 5, f"cell={fcell}")
        check("C3 after streams: (intent/best, model-b) has 5 requests",
              bcell is not None and bcell.get("requests") == 5, f"cell={bcell}")

        # ---- Phase D: dashboard has no Playground ----
        st, _, body = req("GET", "/dashboard")
        check("D1 dashboard 200, no Playground", st == 200 and "Playground" not in body,
              f"status={st}")
    finally:
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        for p in procs:
            try:
                p.wait(timeout=10)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
