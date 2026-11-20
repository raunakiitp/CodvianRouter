#!/usr/bin/env python3
"""Live smoke for the OpenRouter Auto Code mode (CHECKLIST 247-248).

Real binary + two Python mocks. No fake data: every assertion reads a
live header, a live feed, a live counter, or a live HTTP status.

Mocks: "cheap" serves model cheap-fail ($0.50/1M), "pricey" serves
model pricey-solve ($5.00/1M). Both answer chat with a fixed reply
naming themselves.

Flow (one server run, so the in-memory outcome tally survives):
  Phase 1 (no evidence): auto/code resolves cheap-fail (price order
    at the neutral default) and says so.
  Phase 2 (seed failure): 10 auto/code chats, all served by
    cheap-fail, each rated discard. At 10 outcomes cheap-fail's
    measured solve rate is 0.00, so the mode switches.
  Phase 3 (seed success): 10 auto/code chats, all served by
    pricey-solve, each rated accept. At 10 outcomes pricey-solve's
    measured solve rate is 1.00.
  Phase 4 (verify the flip): auto/code now resolves pricey-solve,
    the decision names the measured rates, the live chat is served
    by pricey-solve, and X-Codvian-Decision names strategy=code
    with reason=cost-per-solved-task — for model=auto/code and for
    the auto:code alias. The strategies feed lists "code", and the
    dashboard page carries the auto/code picker + evidence card.
  Phase 5 (pool narrowing): restart with code_exclude=[pricey-solve]
    — auto/code resolves cheap-fail again with the drop recorded,
    and the evidence feed is honestly empty again (the tally is
    in-memory and resets on restart; the raw signals persist in
    feedback.db).
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/smoke-247-248")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21441
CHEAP_PORT = 21442
PRICEY_PORT = 21443
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); name = sys.argv[2]; hits_file = sys.argv[3]; model = sys.argv[4]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
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

def last_decision():
    st, _, body = req("GET", "/dashboard/auto-route/last")
    try:
        d = json.loads(body)
    except Exception:
        return None
    if not d.get("has_decision"):
        return None
    return d.get("decision")

def auto_route(model, prompt):
    st, _, body = req("GET", "/dashboard/auto-route?model=" + urllib.request.quote(model) +
                      "&prompt=" + urllib.request.quote(prompt))
    try:
        return st, json.loads(body)
    except Exception:
        return st, {}

def code_outcomes():
    st, _, body = req("GET", "/dashboard/code-outcomes")
    try:
        return st, json.loads(body)
    except Exception:
        return st, {}

# Ten distinct coding prompts (the exact cache must not collapse them).
# Each carries a unique nonsense suffix so the semantic cache cannot
# collapse two coding prompts onto each other either.
PROMPTS = [
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
]
# Fresh prompts for the Phase 4 live chats: reusing a Phase 2/3 prompt
# would hit the exact cache (no provider call, no decision header).
PROMPT_F3 = "Write a Go function that reverses a linked list for the ivory anchor cache"
PROMPT_F6 = "Write a Rust function that parses a log line for the crimson anchor cache"
# Ten more distinct coding prompts for Phase 3: reusing Phase 2's
# prompts would hit the cache (keyed by requested model + messages)
# and no new decision would be served or recorded.
PROMPTS2 = [
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
]

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

def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN, "./cmd/codvian-router"],
                       cwd=os.path.expanduser("~/workspace/codvian-router"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1
    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    procs = []
    files = {}
    for port, name, model in ((CHEAP_PORT, "cheap", "cheap-fail"),
                              (PRICEY_PORT, "pricey", "pricey-solve")):
        hf = os.path.join(HOME, name + ".hits")
        open(hf, "w").write("0")
        files[name] = hf
        p = subprocess.Popen([sys.executable, mock_py, str(port), name, hf, model],
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
    # Tier models first (the auto-router is built at startup from
    # these; the mocks join as providers below).
    with open(cfg_path) as f:
        cfg = json.load(f)
    cfg["auto_router"] = {"tier_models": {"coding": ["cheap-fail", "pricey-solve"]}}
    with open(cfg_path, "w") as f:
        json.dump(cfg, f, indent=2)

    router = start_router(env)
    if router is None:
        return 1
    procs.append(router)
    try:
        ids = {}
        for name, port in (("cheap", CHEAP_PORT), ("pricey", PRICEY_PORT)):
            st, _, body = req("POST", "/api/providers",
                              {"name": name, "base_url": f"http://127.0.0.1:{port}",
                               "api_key": name + "-key"}, key=raw_key)
            check(f"A0 add provider {name}", st == 201, f"status={st} {body[:160]}")
            try:
                ids[name] = json.loads(body).get("id", name)
            except Exception:
                ids[name] = name
        cheap_id, pricey_id = ids["cheap"], ids["pricey"]

        # Prices need provider ids: restart with the price book.
        stop_router(router)
        procs.remove(router)
        with open(cfg_path) as f:
            cfg = json.load(f)
        cfg["provider_prices"] = {
            cheap_id: {"cheap-fail": {"input_usd_per_1m": 0.5, "output_usd_per_1m": 0.5}},
            pricey_id: {"pricey-solve": {"input_usd_per_1m": 5, "output_usd_per_1m": 5}},
        }
        with open(cfg_path, "w") as f:
            json.dump(cfg, f, indent=2)
        router = start_router(env)
        if router is None:
            return 1
        procs.append(router)

        # ---- Phase 1: no evidence — price order, honest default ----
        st, dec = auto_route("auto/code", PROMPTS[0])
        check("B1 no-evidence preview resolves cheap-fail",
              st == 200 and dec.get("model") == "cheap-fail",
              f"status={st} model={dec.get('model')!r}")
        check("B2 no-evidence flag + reason names the default",
              dec.get("code_no_evidence") is True and
              "no task-outcome evidence yet" in (dec.get("reason") or ""),
              f"reason={dec.get('reason')!r}")
        scores = dec.get("code_scores") or []
        check("B3 preview carries both candidates' CPS breakdown",
              len(scores) == 2 and scores[0].get("model") == "cheap-fail" and
              scores[0].get("evidence") is False,
              f"scores={scores}")
        st, ev = code_outcomes()
        check("B4 evidence feed honestly empty",
              st == 200 and ev.get("models") == [] and ev.get("default_solve_rate") == 0.5,
              f"status={st} body={ev}")

        # ---- Phase 2: seed 10 discards on cheap-fail ----
        served_cheap = 0
        for i, prompt in enumerate(PROMPTS):
            st, h, body = chat("auto/code", prompt, raw_key)
            if st != 200:
                check(f"C{i} cheap-fail chat {i}", False, f"status={st} body={body[:200]}")
                continue
            # The decision id rides on the response header; the
            # last-decision feed is only a fallback.
            dec_id = h.get("X-Codvian-Auto-Decision-Id") or (last_decision() or {}).get("id")
            if h.get("X-Codvian-Auto-Model") == "cheap-fail":
                served_cheap += 1
            if not dec_id:
                check(f"C{i} decision id present", False, "no header and no last decision")
                continue
            st2, _, b2 = req("POST", "/dashboard/feedback",
                             {"decision_id": dec_id, "signal": "discard"}, key=raw_key)
            if st2 != 201:
                check(f"C{i} discard recorded", False, f"status={st2} body={b2[:160]}")
        check("C10 all 10 seed chats served by cheap-fail", served_cheap == 10,
              f"served_cheap={served_cheap}")
        check("C11 cheap mock hit 10 times, pricey 0",
              hits(files["cheap"]) == 10 and hits(files["pricey"]) == 0,
              f"cheap={hits(files['cheap'])} pricey={hits(files['pricey'])}")
        st, ev = code_outcomes()
        rows = {m["model"]: m for m in ev.get("models", [])}
        check("C12 cheap-fail measured 0.00 after 10 discards",
              rows.get("cheap-fail", {}).get("attempts") == 10 and
              rows.get("cheap-fail", {}).get("solve_rate") == 0 and
              rows.get("cheap-fail", {}).get("evidence") is True,
              f"rows={rows}")

        # ---- Phase 3: the flip — pricey-solve serves now ----
        st, dec = auto_route("auto/code", PROMPTS[0])
        check("D1 preview flips to pricey-solve after cheap fails",
              st == 200 and dec.get("model") == "pricey-solve",
              f"status={st} model={dec.get('model')!r}")
        served_pricey = 0
        for i, prompt in enumerate(PROMPTS2):
            st, h, body = chat("auto/code", prompt, raw_key)
            if st != 200:
                check(f"E{i} pricey chat {i}", False, f"status={st} body={body[:200]}")
                continue
            dec_id = h.get("X-Codvian-Auto-Decision-Id") or (last_decision() or {}).get("id")
            if h.get("X-Codvian-Auto-Model") == "pricey-solve":
                served_pricey += 1
            if not dec_id:
                check(f"E{i} decision id present", False, "no header and no last decision")
                continue
            st2, _, b2 = req("POST", "/dashboard/feedback",
                             {"decision_id": dec_id, "signal": "accept"}, key=raw_key)
            if st2 != 201:
                check(f"E{i} accept recorded", False, f"status={st2} body={b2[:160]}")
        check("E10 all 10 seed chats served by pricey-solve", served_pricey == 10,
              f"served_pricey={served_pricey}")
        check("E11 pricey mock hit 10 times (20 cheap total)",
              hits(files["pricey"]) == 10 and hits(files["cheap"]) == 10,
              f"cheap={hits(files['cheap'])} pricey={hits(files['pricey'])}")

        # ---- Phase 4: verify the flip end to end ----
        st, dec = auto_route("auto/code", PROMPTS[1])
        scores = dec.get("code_scores") or []
        check("F1 preview resolves pricey-solve with measured rates",
              st == 200 and dec.get("model") == "pricey-solve" and
              dec.get("code_no_evidence") is not True and
              len(scores) == 2 and scores[0].get("model") == "pricey-solve" and
              scores[0].get("solve_rate") == 1 and scores[0].get("samples") == 10 and
              scores[1].get("model") == "cheap-fail" and
              scores[1].get("solve_rate") == 0,
              f"model={dec.get('model')!r} scores={scores}")
        check("F2 reason names the mode + measured solve rate",
              "auto/code" in (dec.get("reason") or "") and
              "measured solve rate 1.00 from 10 coding outcomes" in (dec.get("reason") or ""),
              f"reason={dec.get('reason')!r}")
        st, h, body = chat("auto/code", PROMPT_F3, raw_key)
        check("F3 live auto/code chat served by pricey-solve",
              st == 200 and "pricey says hi" in body,
              f"status={st} body={body[:200]}")
        check("F4 decision header names strategy=code, reason=cost-per-solved-task",
              h.get("X-Codvian-Decision") ==
              f"provider={pricey_id}; model=pricey-solve; strategy=code; reason=cost-per-solved-task",
              f"got={h.get('X-Codvian-Decision')!r}")
        check("F5 X-Codvian-Strategy is code",
              h.get("X-Codvian-Strategy") == "code",
              f"got={h.get('X-Codvian-Strategy')!r}")
        st, h, body = chat("auto:code", PROMPT_F6, raw_key)
        check("F6 auto:code alias serves pricey-solve with the same header",
              st == 200 and "pricey says hi" in body and
              h.get("X-Codvian-Decision") ==
              f"provider={pricey_id}; model=pricey-solve; strategy=code; reason=cost-per-solved-task",
              f"status={st} header={h.get('X-Codvian-Decision')!r}")
        st, _, body = req("GET", "/dashboard/strategies")
        try:
            feed = json.loads(body)
            strat = {s["id"]: s for s in feed.get("strategies", [])}
        except Exception:
            strat = {}
        check("F7 strategies feed lists code with its rule",
              "code" in strat and "cost per solved task" in strat["code"].get("rule", ""),
              f"ids={sorted(strat)}")
        st, _, body = req("GET", "/dashboard")
        check("F8 dashboard page carries the auto/code picker + evidence card",
              st == 200 and 'data-auto-model="auto/code"' in body and
              "Cost per solved task" in body,
              f"status={st}")
        st, ev = code_outcomes()
        rows = {m["model"]: m for m in ev.get("models", [])}
        check("F9 evidence feed shows both models' measured rates",
              rows.get("cheap-fail", {}).get("solve_rate") == 0 and
              rows.get("pricey-solve", {}).get("solve_rate") == 1,
              f"rows={rows}")

        # ---- Phase 5: pool narrowing live (restart) ----
        stop_router(router)
        procs.remove(router)
        with open(cfg_path) as f:
            cfg = json.load(f)
        cfg["auto_router"]["code_exclude"] = ["pricey-solve"]
        with open(cfg_path, "w") as f:
            json.dump(cfg, f, indent=2)
        router = start_router(env)
        if router is None:
            return 1
        procs.append(router)
        st, dec = auto_route("auto/code", PROMPTS[0])
        check("G1 excluded pricey-solve: auto/code resolves cheap-fail again",
              st == 200 and dec.get("model") == "cheap-fail",
              f"status={st} model={dec.get('model')!r}")
        check("G2 decision records the pool drop with its reason",
              (dec.get("code_pool_dropped") or {}).get("pricey-solve") ==
              "on the auto/code exclude list",
              f"dropped={dec.get('code_pool_dropped')}")
        st, ev = code_outcomes()
        check("G3 evidence feed honestly empty after restart (in-memory tally)",
              st == 200 and ev.get("models") == [],
              f"body={ev}")
    finally:
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        for p in procs:
            try:
                p.wait(timeout=5)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed")
    if failed:
        print("FAILURES:")
        for name, _, detail in failed:
            print(f"  - {name} {detail}")
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
