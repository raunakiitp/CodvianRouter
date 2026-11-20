#!/usr/bin/env python3
"""Live smoke for intent tiers + metadata-only learning suggestions
(CHECKLIST 259-260, research-log entry 234 — Aiport).

Real binary + two Python mocks. No fake data: every assertion reads a
live header, a live feed, a live counter, or a live HTTP status.

Mocks: "flaky" serves model-a, "solid" serves model-b. Both answer
/v1/models and chat. A mode file flips both mocks between failing
(HTTP 500) and succeeding (200) so the same process can record
failures first and successes later.

Config: intent fast -> model-b (low effort) at startup; coding tier
candidates model-a + model-b; DefaultModel model-b.

Flow (one server run, so the in-memory outcome evidence survives):
  Phase 1 (preview + feeds): intent/fast previews to model-b at
    effort low with an operator-defined reason; intent/best
    (unmapped) previews 502 naming "best" and chats 400 naming
    "best"; the intent-tiers feed shows fast mapped / best
    unmapped; GET /v1/models lists all three intent/* names;
    the suggestions feed is honestly empty; the dashboard page
    carries the Intent tiers + Learning suggestions cards and no
    "Playground".
  Phase 2 (good evidence): 12 intent/fast chats, all served by
    model-b (mode ok) — 12 successes for (intent/fast, model-b).
  Phase 3 (bad evidence): remap fast -> model-a at runtime, flip
    the mocks to failing, 10 intent/fast chats fail (10 failures
    for (intent/fast, model-a)) and 24 auto/coding chats fail
    (demote evidence for the coding tier).
  Phase 4 (suggest + approve + revert): GET /dashboard/suggestions
    shows an intent_remap fast model-a -> model-b with real
    evidence counts (10 vs 12) and at least one tier_demote for
    coding. Approving the remap moves fast back to model-b (feed
    + preview + a live chat served by model-b with
    X-Codvian-Intent-Tier: fast / X-Codvian-Intent-Effort: low --
    the script waits out solid's breaker, tripped by the failure
    phases, before that chat, or the breaker skips it);
    reverting restores model-a and the suggestion is pending
    again. Approving a demote removes its model from the tier's
    candidates (the suggestion leaves pending); reverting it
    brings the suggestion back.
  Phase 5 (revert really routes): with the mocks failing, one
    intent/fast chat is a 502 served by model-a only (flaky hits +1
    and the failure is recorded on (intent/fast, model-a), now 11
    requests) — the reverted mapping is what routes. (The solid
    mock may also be hit by failover after model-a fails; the
    assertion is on model-a, not on solid staying untouched.)
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-259-260")
# Build the tree this script lives in (the gate worktree when run there),
# never a hardcoded worktree: the smoke must prove THIS snapshot.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21451
FLAKY_PORT = 21452
SOLID_PORT = 21453
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MODE_FILE = os.path.join(HOME, "mock-mode")

MOCK_PY = r'''
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; hits_file = sys.argv[3]; model = sys.argv[4]; mode_file = sys.argv[5]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
    def _mode(self):
        try:
            with open(mode_file) as f: return f.read().strip()
        except Exception: return "ok"
    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit()
        if self._mode() == "fail":
            body = json.dumps({"error": {"message": "mock upstream failure", "type": "server_error", "code": "server_error"}}).encode()
            self.send_response(500); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        try: req = json.loads(raw.decode())
        except Exception: req = {}
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

def chat(model, prompt, key):
    return req("POST", "/v1/chat/completions",
               {"model": model, "messages": [{"role": "user", "content": prompt}]}, key)

def jget(path, key=None):
    st, _, body = req("GET", path, key=key)
    try:
        return st, json.loads(body)
    except Exception:
        return st, {}

def set_mode(mode):
    with open(MODE_FILE, "w") as f: f.write(mode)

def wait_breaker_closed(name, timeout=45):
    """Poll GET /dashboard/cooldown until the named provider's circuit
    breaker is not open (or no breaker state is reported for it).
    The breaker opens after 5 consecutive failures and stays open
    30s: after the failure phases, a live chat that must reach a
    mock would otherwise be skipped by the breaker (502 with a
    skipped_breaker outcome) instead of proving the routing. A
    plain 500 with no Retry-After sets no hard cooldown, so the
    breaker is the only thing to wait out. Returns True when the
    breaker is closed, False on timeout (the caller still chats --
    the check then fails honestly instead of hanging)."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        st, feed = jget("/dashboard/cooldown")
        brs = feed.get("breakers", []) if isinstance(feed, dict) else []
        hit = [b for b in brs
               if b.get("name") == name or b.get("provider") == name]
        if not hit or not hit[0].get("open"):
            return True
        time.sleep(1)
    return False

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
FAST_OK = [
    "Tell me a joke about a lighthouse keeper who counts the waves",
    "What is the best way to brew coffee at high altitude?",
    "Explain why the sky looks blue to a curious child",
    "Write a haiku about rain on a tin roof in autumn",
    "Give me a one-sentence summary of how bicycles stay upright",
    "What should I pack for a three-day hike in the mountains?",
    "Tell me a short story about a cat that learned to open doors",
    "How do I fix a squeaky door hinge with household items?",
    "Explain the difference between a virus and a bacterium simply",
    "What is a good beginner recipe using only potatoes and onions?",
    "Why do we see lightning before we hear thunder?",
    "Suggest a name for a small bakery that sells only sourdough",
]
FAST_FAIL = [
    "What is the capital of France?",
    "How many days are there in a leap year?",
    "Who wrote the play Hamlet?",
    "What is the chemical symbol for gold?",
    "In what year did humans first land on the Moon?",
    "What is the largest ocean on Earth?",
    "How many sides does a hexagon have?",
    "What language is spoken in Brazil?",
    "What is the boiling point of water at sea level?",
    "Who painted the Mona Lisa?",
]
CODE_FAIL = [
    "Write a Python function that sorts a list of numbers for the zebra ledger",
    "Fix this bug in my code, it throws a null pointer error in the copper kettle module",
    "Refactor this function to be cleaner and faster for the velvet compass service",
    "Write a SQL query to join these two tables for the marble lantern report",
    "Review my code and suggest improvements for the silent banjo parser",
    "Debug this crash, the program exits with an error in the paper falcon job",
    "Convert this JavaScript code to Python for the amber telescope tool",
    "Write unit tests for this function in the quiet harbor scheduler",
    "Explain this stack trace and tell me what went wrong in the brass orchard worker",
    "Optimize this slow loop for large inputs in the tin lighthouse indexer",
    "Write a Go function that reverses a linked list for the ivory anchor cache",
    "Write a Rust function that parses a log line for the crimson anchor cache",
    "Write a Python function that finds prime numbers for the cobalt ledger",
    "Fix this bug in my code, it throws an index out of range error in the linen kettle module",
    "Refactor this class to be cleaner and faster for the scarlet compass service",
    "Write a SQL query to count orders by customer for the granite lantern report",
    "Review my pull request and suggest improvements for the hollow banjo parser",
    "Debug this crash, the server exits with a segfault in the cedar falcon job",
    "Convert this TypeScript code to Python for the opal telescope tool",
    "Write integration tests for this endpoint in the misty harbor scheduler",
    "Explain this error message and tell me what went wrong in the pewter orchard worker",
    "Optimize this database query for large tables in the slate lighthouse indexer",
    "Write a Python function that deduplicates a list for the pearl beacon ledger",
    "Fix this bug in my code, it throws a key error in the willow kettle module",
]

def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    set_mode("ok")
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
        hf = os.path.join(HOME, name + ".hits")
        open(hf, "w").write("0")
        files[name] = hf
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, hf, model, MODE_FILE],
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
        "intent_tiers": {"fast": {"model": "model-b", "effort": "low"}},
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

        # ---- Phase 1: preview + feeds ----
        st, dec = jget("/dashboard/auto-route?model=intent/fast")
        check("B1 preview intent/fast resolves model-b at effort low",
              st == 200 and dec.get("model") == "model-b" and dec.get("effort") == "low"
              and dec.get("intent_tier") == "fast",
              f"status={st} model={dec.get('model')!r} effort={dec.get('effort')!r}")
        check("B2 preview reason says operator-defined, no classification",
              "operator-defined" in (dec.get("reason") or ""),
              f"reason={dec.get('reason')!r}")
        st, dec = jget("/dashboard/auto-route?model=intent/best")
        check("B3 preview intent/best is 502 naming the tier",
              st == 502 and dec.get("model") in (None, "") and '"best"' in (dec.get("reason") or ""),
              f"status={st} reason={dec.get('reason')!r}")
        st, _, body = chat("intent/best", "hello", raw_key)
        try:
            msg = json.loads(body).get("error", {}).get("message", "")
        except Exception:
            msg = ""
        check("B4 live chat intent/best is 400 naming the tier",
              st == 400 and '"best"' in msg and "not mapped" in msg,
              f"status={st} body={body[:200]}")
        st, tiers = jget("/dashboard/intent-tiers")
        by_tier = {t["tier"]: t for t in tiers.get("tiers", [])}
        check("B5 intent-tiers feed: fast mapped to model-b, best unmapped",
              st == 200 and by_tier.get("fast", {}).get("mapped") is True
              and by_tier.get("fast", {}).get("model") == "model-b"
              and by_tier.get("best", {}).get("mapped") is False
              and tiers.get("persisted") is False,
              f"status={st} tiers={tiers.get('tiers')}")
        st, models = jget("/v1/models", key=raw_key)
        ids = [m.get("id") for m in models.get("data", [])]
        check("B6 /v1/models lists all three intent/* names",
              st == 200 and all(n in ids for n in ("intent/fast", "intent/balanced", "intent/best")),
              f"ids={ids}")
        st, sugg = jget("/dashboard/suggestions")
        check("B7 suggestions feed honestly empty before traffic",
              st == 200 and sugg.get("suggestions") == [] and sugg.get("stats") == [],
              f"status={st} body={sugg}")
        st, _, dash = req("GET", "/dashboard")
        check("B8 dashboard carries Intent tiers + Learning suggestions cards, no Playground",
              st == 200 and "Intent tiers" in dash and "Learning suggestions" in dash
              and "Playground" not in dash,
              f"status={st}")

        # ---- Phase 2: 12 successes for (intent/fast, model-b) ----
        ok = 0
        for prompt in FAST_OK:
            st, _, _ = chat("intent/fast", prompt, raw_key)
            if st == 200:
                ok += 1
        check("C1 12 intent/fast chats served by model-b", ok == 12, f"served={ok}")
        check("C2 solid mock hit 12 times, flaky untouched",
              hits(files["solid"]) == 12 and hits(files["flaky"]) == 0,
              f"solid={hits(files['solid'])} flaky={hits(files['flaky'])}")

        # ---- Phase 3: remap fast -> model-a, fail everything ----
        st, _, body = req("POST", "/dashboard/intent-tiers",
                          {"tier": "fast", "model": "model-a", "effort": "low"})
        check("D1 runtime remap fast -> model-a", st == 200, f"status={st} {body[:160]}")
        set_mode("fail")
        failed = 0
        for prompt in FAST_FAIL:
            st, _, _ = chat("intent/fast", prompt, raw_key)
            if st == 502:
                failed += 1
        check("D2 10 intent/fast chats fail (502) on model-a", failed == 10, f"failed={failed}")
        code_failed = 0
        for prompt in CODE_FAIL:
            st, _, _ = chat("auto/coding", prompt, raw_key)
            if st == 502:
                code_failed += 1
        check("D3 24 auto/coding chats fail (502)", code_failed == 24, f"failed={code_failed}")

        # ---- Phase 4: suggestions + approve/revert round-trip ----
        st, sugg = jget("/dashboard/suggestions")
        remaps = [s for s in sugg.get("suggestions", []) if s.get("kind") == "intent_remap"]
        demotes = [s for s in sugg.get("suggestions", []) if s.get("kind") == "tier_demote"]
        check("E1 one intent_remap: fast model-a -> model-b",
              len(remaps) == 1 and remaps[0].get("current_model") == "model-a"
              and remaps[0].get("suggested_model") == "model-b",
              f"suggestions={sugg.get('suggestions')}")
        if remaps:
            ev = remaps[0].get("evidence", {})
            check("E2 remap evidence carries real counts (10 vs 12)",
                  ev.get("current", {}).get("requests") == 10
                  and ev.get("challenger", {}).get("requests") == 12
                  and ev.get("current", {}).get("success_rate") == 0.0
                  and ev.get("challenger", {}).get("success_rate") == 1.0,
                  f"evidence={ev}")
        check("E3 at least one tier_demote for coding",
              len([d for d in demotes if d.get("task") == "coding"]) >= 1,
              f"demotes={demotes}")
        remap_id = remaps[0]["id"] if remaps else ""
        set_mode("ok")
        st, _, body = req("POST", "/dashboard/suggestions",
                          {"action": "approve", "suggestion_id": remap_id})
        check("E4 approve remap applies", st == 200, f"status={st} {body[:200]}")
        st, tiers = jget("/dashboard/intent-tiers")
        by_tier = {t["tier"]: t for t in tiers.get("tiers", [])}
        check("E5 feed shows fast back on model-b after approve",
              by_tier.get("fast", {}).get("model") == "model-b",
              f"tiers={tiers.get('tiers')}")
        # D3 tripped solid's breaker (5 consecutive failures open it
        # for 30s); wait it out or this chat is breaker-skipped.
        wait_breaker_closed("solid")
        st, h, body = chat("intent/fast", "Explain how a rainbow forms, using only words a ten-year-old knows", raw_key)
        check("E6 live intent/fast chat served by model-b with intent headers",
              st == 200 and h.get("X-Codvian-Intent-Tier") == "fast"
              and h.get("X-Codvian-Intent-Effort") == "low"
              and "solid says hi" in body,
              f"status={st} headers={h.get('X-Codvian-Intent-Tier')}/{h.get('X-Codvian-Intent-Effort')}")
        st, sugg = jget("/dashboard/suggestions")
        check("E7 remap leaves pending after approve; log records it",
              not [s for s in sugg.get("suggestions", []) if s.get("kind") == "intent_remap"]
              and len(sugg.get("log", [])) == 1 and sugg["log"][0].get("reverted") is False,
              f"log={sugg.get('log')}")
        change_id = sugg["log"][0]["id"] if sugg.get("log") else ""
        st, _, body = req("POST", "/dashboard/suggestions",
                          {"action": "revert", "change_id": change_id})
        check("E8 revert restores model-a", st == 200, f"status={st} {body[:200]}")
        st, tiers = jget("/dashboard/intent-tiers")
        by_tier = {t["tier"]: t for t in tiers.get("tiers", [])}
        check("E9 feed shows fast on model-a after revert; suggestion pending again",
              by_tier.get("fast", {}).get("model") == "model-a",
              f"tiers={tiers.get('tiers')}")
        st, sugg = jget("/dashboard/suggestions")
        check("E10 remap pending again after revert; log marks reverted",
              len([s for s in sugg.get("suggestions", []) if s.get("kind") == "intent_remap"]) == 1
              and sugg["log"][0].get("reverted") is True,
              f"suggestions={sugg.get('suggestions')} log={sugg.get('log')}")
        if demotes:
            demote_id = demotes[0]["id"]
            st, _, body = req("POST", "/dashboard/suggestions",
                              {"action": "approve", "suggestion_id": demote_id})
            check("E11 approve demote applies", st == 200, f"status={st} {body[:200]}")
            st, sugg = jget("/dashboard/suggestions")
            check("E12 demote leaves pending after approve",
                  demote_id not in [s.get("id") for s in sugg.get("suggestions", [])],
                  f"suggestions={sugg.get('suggestions')}")
            demote_change = [c for c in sugg.get("log", []) if c.get("suggestion_id") == demote_id]
            check("E13 demote change logged", len(demote_change) == 1, f"log={sugg.get('log')}")
            if demote_change:
                st, _, body = req("POST", "/dashboard/suggestions",
                                  {"action": "revert", "change_id": demote_change[0]["id"]})
                check("E14 revert demote restores the candidate", st == 200, f"status={st} {body[:200]}")
                st, sugg = jget("/dashboard/suggestions")
                check("E15 demote pending again after revert",
                      demote_id in [s.get("id") for s in sugg.get("suggestions", [])],
                      f"suggestions={sugg.get('suggestions')}")

        # ---- Phase 5: the reverted mapping is what routes ----
        set_mode("fail")
        # D2 tripped flaky's breaker; wait it out so the trial
        # request reaches the mock (in fail mode it fails there,
        # which is the proof -- a breaker skip proves nothing).
        wait_breaker_closed("flaky")
        before_flaky, before_solid = hits(files["flaky"]), hits(files["solid"])
        st, _, _ = chat("intent/fast", "How do bees find their way back to the hive?", raw_key)
        st2, sugg2 = jget("/dashboard/suggestions")
        fast_a = [s for s in sugg2.get("stats", [])
                  if s.get("task") == "intent/fast" and s.get("model") == "model-a"]
        check("F1 reverted intent/fast routes to model-a (502, flaky +1, recorded on model-a)",
              st == 502 and hits(files["flaky"]) == before_flaky + 1
              and fast_a and fast_a[0].get("requests") == 11,
              f"status={st} flaky={before_flaky}->{hits(files['flaky'])} solid={before_solid}->{hits(files['solid'])} intent/fast+model-a={fast_a}")
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
    failed_checks = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed_checks)}/{len(checks)} checks passed")
    return 1 if failed_checks else 0

if __name__ == "__main__":
    sys.exit(main())
