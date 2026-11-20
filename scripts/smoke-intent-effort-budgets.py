#!/usr/bin/env python3
"""Live smoke for intent-tier effort forwarding on non-OpenAI wires
(CHECKLIST 285-286): Anthropic numeric `thinking.budget_tokens`
and Gemini numeric `thinkingConfig.thinkingBudget` via an
OPERATOR-DECLARED effort->budget table (never invented defaults).

Real binary + two Python mocks (Anthropic Messages + Gemini
generateContent), added as catalog providers ("anthropic" /
"gemini") with a base_url override pointing at the mocks, so the
wire format is the catalog's own. No fake data: every assertion
reads a live header, a live feed, a live trace, or a value the
mock actually received on the wire.

Mocks record the thinking budget of every chat POST they receive
("<absent>" when the field is missing) into their budgets file.

Config: intent fast -> claude-sonnet-4-5 (low effort), best ->
gemini-2.0-flash (high effort); reasoning.effort_budgets
anthropic {low: 1024, high: 16384}, gemini {low: 2048, high: 8192}.

Flow (two server runs):
  Run 1 (table declared):
    Phase 1 (preview): intent/fast previews to claude-sonnet-4-5
      at effort low, intent/best to gemini-2.0-flash at high.
    Phase 2 (wire proof): one intent/fast chat and one
      intent/best chat; the Anthropic mock records budget 1024,
      the Gemini mock 8192 — the two differ exactly as the
      operator's table says. The route trace after each chat shows
      the serving attempt's effort_forwarding with the same value.
    Phase 3 (stream): a streaming intent/fast chat and a streaming
      intent/best chat carry the same budgets to the mocks.
  Run 2 (table removed from the config file, same mocks, same
    providers): one intent/fast chat — the Anthropic mock records
    NO thinking field at all, and the trace says
    not-forwarded:no-operator-budget-table.
  Phase 4 (dashboard): the served page carries the new forwarding
    copy and the decision-view forwarding text; no "Playground".
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-285-286")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21471
ANTH_PORT = 21472
GEM_PORT = 21473
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

ANTH_MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); budgets_file = sys.argv[2]
STREAM_BODY = ("event: message_start\n"
  "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":12}}}\n\n"
  "event: content_block_start\n"
  "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"
  "event: content_block_delta\n"
  "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic says hi\"}}\n\n"
  "event: content_block_stop\n"
  "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
  "event: message_delta\n"
  "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n"
  "event: message_stop\n"
  "data: {\"type\":\"message_stop\"}\n\n")
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"data": [{"id": "claude-sonnet-4-5"}, {"id": "claude-haiku-4-5"}]}).encode()
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
        th = req.get("thinking") or {}
        budget = th.get("budget_tokens")
        with open(budgets_file, "a") as f:
            f.write(("<absent>" if budget is None else str(budget)) + "\n")
        if req.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            self.wfile.write(STREAM_BODY.encode()); self.wfile.flush()
            return
        body = json.dumps({"id": "msg_1", "type": "message", "role": "assistant",
            "content": [{"type": "text", "text": "anthropic says hi"}],
            "model": "claude-sonnet-4-5", "stop_reason": "end_turn",
            "usage": {"input_tokens": 12, "output_tokens": 8}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
'''

GEM_MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); budgets_file = sys.argv[2]
STREAM_BODY = ('data: {"candidates":[{"content":{"role":"model","parts":[{"text":"gemini says hi "}]},"finishReason":"STOP"}]}\n\n'
               'data: {"candidates":[{"content":{"role":"model","parts":[{"text":"there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}\n\n')
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/models":
            body = json.dumps({"models": [{"name": "models/gemini-2.0-flash"}]}).encode()
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
        body = json.dumps({"candidates": [{"content": {"role": "model", "parts": [{"text": "gemini says hi"}]}, "finishReason": "STOP"}],
            "usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 8, "totalTokenCount": 20},
            "modelVersion": "gemini-2.0-flash-001"}).encode()
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
    anth_py = os.path.join(HOME, "mock-anthropic.py")
    open(anth_py, "w").write(ANTH_MOCK_PY)
    gem_py = os.path.join(HOME, "mock-gemini.py")
    open(gem_py, "w").write(GEM_MOCK_PY)
    procs = []
    files = {}
    for port, py, name in ((ANTH_PORT, anth_py, "anthropic"),
                           (GEM_PORT, gem_py, "gemini")):
        bf = os.path.join(HOME, name + ".budgets")
        open(bf, "w").write("")
        files[name] = bf
        p = subprocess.Popen([sys.executable, py, str(port), bf],
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
        "tier_models": {"coding": ["claude-sonnet-4-5", "gemini-2.0-flash"]},
        "default_model": "gemini-2.0-flash",
        "intent_tiers": {"fast": {"model": "claude-sonnet-4-5", "effort": "low"},
                         "best": {"model": "gemini-2.0-flash", "effort": "high"}},
    }
    cfg["reasoning"] = {
        "effort_budgets": {
            "anthropic": {"low": 1024, "high": 16384},
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
        # Catalog providers with a base_url override at the mocks:
        # the wire format is the catalog's own (anthropic/gemini).
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "anthropic",
                           "base_url": f"http://127.0.0.1:{ANTH_PORT}",
                           "api_key": "anth-key"}, key=raw_key)
        check("A0 add provider anthropic (wire anthropic)", st == 201 and '"wire_format":"anthropic"' in body.replace(" ", ""),
              f"status={st} {body[:200]}")
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "gemini",
                           "base_url": f"http://127.0.0.1:{GEM_PORT}",
                           "api_key": "gem-key"}, key=raw_key)
        check("A1 add provider gemini (wire gemini)", st == 201 and '"wire_format":"gemini"' in body.replace(" ", ""),
              f"status={st} {body[:200]}")

        # ---- Phase 1: preview ----
        st, dec = jget("/dashboard/auto-route?model=intent/fast")
        check("B1 preview intent/fast resolves claude-sonnet-4-5 at effort low",
              st == 200 and dec.get("model") == "claude-sonnet-4-5" and dec.get("effort") == "low"
              and dec.get("intent_tier") == "fast",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")
        st, dec = jget("/dashboard/auto-route?model=intent/best")
        check("B2 preview intent/best resolves gemini-2.0-flash at effort high",
              st == 200 and dec.get("model") == "gemini-2.0-flash" and dec.get("effort") == "high"
              and dec.get("intent_tier") == "best",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")

        # ---- Phase 2: wire proof (mocks record what they received) ----
        st, h, body = chat("intent/fast", PROMPTS[0], raw_key)
        check("C1 intent/fast chat 200 with X-Codvian-Intent-Effort: low",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "low"
              and h.get("X-Codvian-Intent-Tier") == "fast",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r} body={body[:120]!r}")
        check("C2 Anthropic mock received thinking.budget_tokens 1024 on the wire",
              budgets(files["anthropic"]) == ["1024"],
              f"anthropic budgets={budgets(files['anthropic'])}")
        st, h, body = chat("intent/best", PROMPTS[1], raw_key)
        check("C3 intent/best chat 200 with X-Codvian-Intent-Effort: high",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "high"
              and h.get("X-Codvian-Intent-Tier") == "best",
              f"status={st} effort={h.get('X-Codvian-Intent-Effort')!r} body={body[:120]!r}")
        check("C4 Gemini mock received thinkingConfig.thinkingBudget 8192 on the wire",
              budgets(files["gemini"]) == ["8192"],
              f"gemini budgets={budgets(files['gemini'])}")
        check("C5 forwarded budgets differ by tier (1024 vs 8192)",
              budgets(files["anthropic"]) == ["1024"] and budgets(files["gemini"]) == ["8192"],
              f"anthropic={budgets(files['anthropic'])} gemini={budgets(files['gemini'])}")

        # ---- Trace shows the forwarding per attempt ----
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_gem = [a for a in atts if a.get("model") == "gemini-2.0-flash"
                      and a.get("outcome") == "success"]
        check("C6 trace: serving Gemini attempt carries effort_forwarding forwarded:thinkingConfig.thinkingBudget=8192",
              st == 200 and len(served_gem) == 1
              and served_gem[0].get("effort_forwarding") == "forwarded:thinkingConfig.thinkingBudget=8192",
              f"status={st} attempts={atts}")
        st, h, body = chat("intent/fast", PROMPTS[2], raw_key)
        check("C7 second intent/fast chat 200", st == 200, f"status={st}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served_anth = [a for a in atts if a.get("model") == "claude-sonnet-4-5"
                       and a.get("outcome") == "success"]
        check("C8 trace: serving Anthropic attempt carries effort_forwarding forwarded:thinking.budget_tokens=1024",
              st == 200 and len(served_anth) == 1
              and served_anth[0].get("effort_forwarding") == "forwarded:thinking.budget_tokens=1024",
              f"status={st} attempts={atts}")

        # ---- Phase 3: stream path carries the same budgets ----
        st, h, body = stream_chat("intent/fast", PROMPTS[3], raw_key)
        check("C9 streaming intent/fast chat 200 with SSE content",
              st == 200 and "anthropic says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("C10 streaming chat carried budget 1024 to the Anthropic mock",
              budgets(files["anthropic"])[-1] == "1024",
              f"anthropic budgets={budgets(files['anthropic'])}")
        st, h, body = stream_chat("intent/best", PROMPTS[4], raw_key)
        check("C11 streaming intent/best chat 200 with SSE content",
              st == 200 and "gemini says hi" in body,
              f"status={st} body={body[:160]!r}")
        check("C12 streaming chat carried budget 8192 to the Gemini mock",
              budgets(files["gemini"])[-1] == "8192",
              f"gemini budgets={budgets(files['gemini'])}")
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
        before_anth = len(budgets(files["anthropic"]))
        st, h, body = chat("intent/fast", PROMPTS[5], raw_key)
        check("D1 no-table intent/fast chat 200",
              st == 200 and h.get("X-Codvian-Intent-Effort") == "low",
              f"status={st} body={body[:120]!r}")
        check("D2 Anthropic mock received NO thinking field without the table",
              len(budgets(files["anthropic"])) == before_anth + 1
              and budgets(files["anthropic"])[-1] == "<absent>",
              f"anthropic budgets={budgets(files['anthropic'])}")
        st, trace = jget("/dashboard/trace")
        atts = trace.get("attempts", []) if isinstance(trace, dict) else []
        served = [a for a in atts if a.get("model") == "claude-sonnet-4-5"
                  and a.get("outcome") == "success"]
        check("D3 trace: no-table attempt records not-forwarded:no-operator-budget-table",
              st == 200 and len(served) == 1
              and served[0].get("effort_forwarding") == "not-forwarded:no-operator-budget-table",
              f"status={st} attempts={atts}")

        # ---- Phase 4: dashboard copy ----
        st, _, dash = req("GET", "/dashboard")
        check("E1 dashboard shows forwarding copy, decision-view forwarding text, no Playground",
              st == 200 and "thinking.budget_tokens" in dash
              and "no operator budget table" in dash
              and "Provider received effort" in dash
              and "only the OpenAI wire sends" not in dash
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
