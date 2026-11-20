#!/usr/bin/env python3
"""Live smoke for the token-count accuracy audit (CHECKLIST 229-230).

Real binary + three Python mocks (OpenAI, Anthropic, Bedrock wires)
that bill the wire payload they receive, tokenized with tiktoken
(cl100k_base) under the same rules as the audit harness
(internal/router/token_audit_test.go) — a simulated provider, not a
real one, so this proves the audit numbers end to end, not the
real providers' tokenizers. Every assertion reads a live HTTP
status, a live hit count, a live ledger row (read back from the
real ledger.db the binary wrote), or a live dashboard feed.

Scenarios:
  T1 OpenAI chat 200; ledger prompt_tokens == mock-reported, local
     estimate within 6% (audit: 0.00% on english)
  T2 Anthropic chat 200 (translated /v1/messages); same, within 6%
     (audit: +2.98% english — a safe-direction +1 token on the
     system message)
  T3 Bedrock chat 200 (Converse); same, within 6%
  T4 tool-call-heavy OpenAI chat 200; local within 6% of reported
     (audit: -0.81% — the old text-only estimator was -80.71%)
  T5 pre-send 402 proof: "hello world" (3 text tokens, 9 full)
     with a $0.00005 strict budget is refused before dialling —
     the old estimator would have priced it at $0.00003 and let
     it through
  T6 GET /dashboard/usage drift rows: one per provider, compared
     >= 1, mean |error| <= 6%
  T7 dashboard serves the drift card ("Estimate vs reported"),
     no Playground
  T8 unauthenticated chat -> 401
"""
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.request
import urllib.error

HOME = os.path.expanduser("~/.tmp/token-audit-smoke")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21641
OPENAI_PORT = 21631
CLAUDE_PORT = 21632
BEDROCK_PORT = 21633
BASE = f"http://127.0.0.1:{ROUTER_PORT}"

MOCK_PY = r'''
import json, sys
import tiktoken
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); kind = sys.argv[2]; reported_file = sys.argv[3]
enc = tiktoken.get_encoding("cl100k_base")

def bpe(text):
    return len(enc.encode(text if isinstance(text, str) else ""))

def canon(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)

def part_text(block):
    # Semantic content of a native-wire content block: text as
    # text; tool blocks as name + arguments/response JSON. Wrapper
    # keys (type/id/tool_use_id) are transport framing, not billed
    # content — the same rule as the Go audit harness.
    if isinstance(block, str):
        return block
    if not isinstance(block, dict):
        return canon(block)
    if "text" in block:
        return block["text"]
    if "toolUse" in block:
        u = block["toolUse"]
        return (u.get("name", "") + " " + canon(u.get("input", {}))).strip()
    if "toolResult" in block:
        r = block["toolResult"]
        c = r.get("content", "")
        if isinstance(c, dict):
            return canon(c.get("json", c))
        if isinstance(c, list):
            return " ".join(part_text(x) for x in c)
        return str(c)
    if "tool_use" in block:
        u = block["tool_use"]
        return (u.get("name", "") + " " + canon(u.get("input", {}))).strip()
    if "tool_result" in block:
        c = block["tool_result"]
        if isinstance(c, dict):
            return canon(c.get("content", c))
        return str(c)
    if "functionCall" in block:
        f = block["functionCall"]
        return (f.get("name", "") + " " + canon(f.get("args", {}))).strip()
    if "functionResponse" in block:
        fr = block["functionResponse"]
        return canon(fr.get("response", fr))
    return canon(block)

def count_openai(req):
    n = 0
    for m in req.get("messages", []):
        n += 3
        if m.get("name"):
            n += 1
        c = m.get("content")
        if isinstance(c, str):
            n += bpe(c)
        elif isinstance(c, list):
            for part in c:
                if isinstance(part, dict) and part.get("type") == "text":
                    n += bpe(part.get("text", ""))
        for tc in m.get("tool_calls", []) or []:
            fn = tc.get("function", {}) if isinstance(tc, dict) else {}
            n += bpe(fn.get("arguments", ""))
    n += 3  # reply priming
    if req.get("tools"):
        n += bpe(canon(req["tools"]))
    return n

def count_native(system_text, messages, tools_json):
    n = bpe(system_text)
    for m in messages:
        n += bpe(m.get("role", ""))
        content = m.get("content")
        if isinstance(content, str):
            n += bpe(content)
        elif isinstance(content, list):
            for block in content:
                n += bpe(part_text(block))
    if tools_json is not None:
        n += bpe(canon(tools_json))
    return n

def record(prompt_tokens, completion_tokens):
    with open(reported_file, "w") as f:
        json.dump({"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens}, f)

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        if kind == "bedrock" and self.path.startswith("/foundation-models"):
            self._json(200, {"modelSummaries": [{"modelId": "bedrock-mock"}]}); return
        if self.path.startswith("/v1/models"):
            mid = {"openai": "openai-mock", "anthropic": "claude-mock"}.get(kind, "bedrock-mock")
            self._json(200, {"object": "list", "data": [{"id": mid, "object": "model"}]}); return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        req = json.loads(self.rfile.read(ln).decode() or "{}")
        if kind == "openai" and self.path.endswith("/chat/completions"):
            n = count_openai(req)
            record(n, 2)
            self._json(200, {"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
                "model": "openai-mock",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "openai says hi"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": n, "completion_tokens": 2, "total_tokens": n + 2}})
            return
        if kind == "anthropic" and self.path.endswith("/v1/messages"):
            system = req.get("system", "")
            if isinstance(system, list):
                system = " ".join(b.get("text", "") for b in system if isinstance(b, dict))
            n = count_native(system, req.get("messages", []), req.get("tools"))
            record(n, 3)
            self._json(200, {"id": "msg-smoke", "type": "message", "role": "assistant",
                "model": "claude-mock", "content": [{"type": "text", "text": "claude says hi"}],
                "stop_reason": "end_turn", "usage": {"input_tokens": n, "output_tokens": 3}})
            return
        if kind == "bedrock" and "/converse" in self.path:
            system = " ".join(b.get("text", "") for b in req.get("system", []) if isinstance(b, dict))
            n = count_native(system, req.get("messages", []), req.get("toolConfig"))
            record(n, 3)
            self._json(200, {"output": {"message": {"role": "assistant", "content": [{"text": "bedrock says hi"}]}},
                "stopReason": "end_turn",
                "usage": {"inputTokens": n, "outputTokens": 3, "totalTokens": n + 3}})
            return
        self.send_response(404); self.end_headers()
HTTPServer(("127.0.0.1", port), H).serve_forever()
'''

checks = []
def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

def req(method, path, body=None, key=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data: r.add_header("Content-Type", "application/json")
    if key: r.add_header("Authorization", "Bearer " + key)
    for k, v in (headers or {}).items(): r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    except Exception as e:
        return 0, {}, str(e)

def reported(f):
    try:
        return json.load(open(f))
    except Exception:
        return {}

def ledger_rows():
    db = None
    for root, _, files in os.walk(HOME):
        if "ledger.db" in files:
            db = os.path.join(root, "ledger.db")
            break
    if not db:
        return []
    try:
        con = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=5)
        rows = con.execute(
            "SELECT provider, model, prompt_tokens, local_estimate_prompt_tokens "
            "FROM usage_events ORDER BY id").fetchall()
        con.close()
        return [{"provider": r[0], "model": r[1], "prompt_tokens": r[2],
                 "local_estimate_prompt_tokens": r[3]} for r in rows]
    except Exception as e:
        print("ledger read failed:", e, flush=True)
        return []

def wait_for_row(pred, tries=30):
    for _ in range(tries):
        for row in ledger_rows():
            if pred(row):
                return row
        time.sleep(0.2)
    return None

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
        print("BUILD FAILED:\n" + r.stdout + r.stderr); return 1
    check("T0 real binary builds", True)

    mock_py = os.path.join(HOME, "mock.py")
    open(mock_py, "w").write(MOCK_PY)
    rep = {k: os.path.join(HOME, f"reported-{k}.json") for k in ("openai", "anthropic", "bedrock")}
    procs = []
    for port, kind in ((OPENAI_PORT, "openai"), (CLAUDE_PORT, "anthropic"), (BEDROCK_PORT, "bedrock")):
        p = subprocess.Popen([sys.executable, mock_py, str(port), kind, rep[kind]],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        procs.append(p)
    # Wait for each mock to answer its model list before the
    # router's key test dials it — under a loaded VM the tiktoken
    # import + bind can lag the first add attempt.
    for port, kind in ((OPENAI_PORT, "openai"), (CLAUDE_PORT, "anthropic"), (BEDROCK_PORT, "bedrock")):
        path = "/foundation-models" if kind == "bedrock" else "/v1/models"
        for _ in range(50):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=5) as resp:
                    if resp.status == 200: break
            except Exception:
                time.sleep(0.2)
        else:
            print(f"MOCK {kind} never became ready"); return 1
        check(f"T0a mock {kind} ready", True)
    r = subprocess.run([BIN, "init"], env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("INIT FAILED:\n" + r.stdout + r.stderr); return 1
    raw_key = None
    for tok in r.stdout.replace('"', " ").split():
        if tok.startswith("crk_"): raw_key = tok
    if not raw_key:
        print("INIT did not print a key:\n" + r.stdout); return 1
    cfg_path = None
    for root, _, files in os.walk(HOME):
        if "config.json" in files:
            cfg_path = os.path.join(root, "config.json")
    if not cfg_path:
        print("config.json not found under " + HOME); return 1
    cfg = json.load(open(cfg_path))
    cfg["prices"] = {"openai-mock": {"input_usd_per_1m": 10, "output_usd_per_1m": 30}}
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = subprocess.Popen([BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    procs.append(router)
    try:
        for _ in range(50):
            st, _, _ = req("GET", "/healthz")
            if st == 200: break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready"); return 1
        check("T0b router ready (/healthz 200)", True)

        adds = [
            ("openai", {"base_url": f"http://127.0.0.1:{OPENAI_PORT}", "api_key": "openai-mock-key"}, "openai"),
            ("anthropic", {"catalog_id": "anthropic", "base_url": f"http://127.0.0.1:{CLAUDE_PORT}", "api_key": "anthropic-mock-key"}, "anthropic"),
            ("bedrock", {"catalog_id": "bedrock", "base_url": f"http://127.0.0.1:{BEDROCK_PORT}", "api_key": "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "bedrock-converse"),
        ]
        for name, body, want_wire in adds:
            st, _, b = req("POST", "/api/providers", body, key=raw_key)
            try: added = json.loads(b)
            except Exception: added = {}
            check(f"T0c add provider {name} (wire {want_wire}, routing live)",
                  st == 201 and added.get("wire_format") == want_wire and added.get("routing_live") is True,
                  f"status={st} {b[:200]}")

        prompt = {"system": "You are a helpful assistant.",
                  "user": "Explain why the sky is blue in two sentences."}
        body = {"model": "openai-mock", "messages": [
            {"role": "system", "content": prompt["system"]},
            {"role": "user", "content": prompt["user"]}]}

        # T1: OpenAI wire.
        st, _, b = req("POST", "/v1/chat/completions", body, key=raw_key)
        rep_o = reported(rep["openai"])
        row = wait_for_row(lambda r: r["model"] == "openai-mock")
        err = None if not row or not rep_o.get("prompt_tokens") else \
            100.0 * (row["local_estimate_prompt_tokens"] - rep_o["prompt_tokens"]) / rep_o["prompt_tokens"]
        check("T1 OpenAI chat 200; ledger prompt == mock-reported, local within 6%",
              st == 200 and row is not None and row["prompt_tokens"] == rep_o.get("prompt_tokens")
              and row["local_estimate_prompt_tokens"] > 0 and err is not None and abs(err) <= 6,
              f"status={st} reported={rep_o} row={row} err={err}%")

        # T2: Anthropic wire (translated).
        body["model"] = "claude-mock"
        st, _, b = req("POST", "/v1/chat/completions", body, key=raw_key)
        rep_a = reported(rep["anthropic"])
        row = wait_for_row(lambda r: r["model"] == "claude-mock")
        err = None if not row or not rep_a.get("prompt_tokens") else \
            100.0 * (row["local_estimate_prompt_tokens"] - rep_a["prompt_tokens"]) / rep_a["prompt_tokens"]
        check("T2 Anthropic chat 200; ledger prompt == mock-reported, local within 6%",
              st == 200 and row is not None and row["prompt_tokens"] == rep_a.get("prompt_tokens")
              and row["local_estimate_prompt_tokens"] > 0 and err is not None and abs(err) <= 6,
              f"status={st} reported={rep_a} row={row} err={err}%")

        # T3: Bedrock wire (Converse).
        body["model"] = "bedrock-mock"
        st, _, b = req("POST", "/v1/chat/completions", body, key=raw_key)
        rep_b = reported(rep["bedrock"])
        row = wait_for_row(lambda r: r["model"] == "bedrock-mock")
        err = None if not row or not rep_b.get("prompt_tokens") else \
            100.0 * (row["local_estimate_prompt_tokens"] - rep_b["prompt_tokens"]) / rep_b["prompt_tokens"]
        check("T3 Bedrock chat 200; ledger prompt == mock-reported, local within 6%",
              st == 200 and row is not None and row["prompt_tokens"] == rep_b.get("prompt_tokens")
              and row["local_estimate_prompt_tokens"] > 0 and err is not None and abs(err) <= 6,
              f"status={st} reported={rep_b} row={row} err={err}%")

        # T4: tool-call-heavy on the OpenAI wire.
        tools = [{"type": "function", "function": {
            "name": "get_weather",
            "description": "Get the current weather for a city.",
            "parameters": {"type": "object", "properties": {
                "city": {"type": "string", "description": "City name"},
                "units": {"type": "string", "enum": ["celsius", "fahrenheit"]}},
                "required": ["city"]}}}]
        tool_body = {"model": "openai-mock", "tools": tools, "messages": [
            {"role": "user", "content": "What is the weather in Paris?"},
            {"role": "assistant", "content": None, "tool_calls": [
                {"id": "call_1", "type": "function", "function": {
                    "name": "get_weather", "arguments": "{\"city\": \"Paris\"}"}}]},
            {"role": "tool", "tool_call_id": "call_1",
             "content": "{\"temperature\": 18, \"condition\": \"sunny\"}"},
            {"role": "user", "content": "Thanks, now tell me a joke about the weather."}]}
        before = len([r for r in ledger_rows() if r["model"] == "openai-mock"])
        st, _, b = req("POST", "/v1/chat/completions", tool_body, key=raw_key)
        rep_o2 = reported(rep["openai"])
        row = None
        for _ in range(30):
            rows = [r for r in ledger_rows() if r["model"] == "openai-mock"]
            if len(rows) > before:
                row = rows[-1]
                break
            time.sleep(0.2)
        err = None if not row or not rep_o2.get("prompt_tokens") else \
            100.0 * (row["local_estimate_prompt_tokens"] - rep_o2["prompt_tokens"]) / rep_o2["prompt_tokens"]
        check("T4 tool-call-heavy OpenAI 200; local within 6% of reported (old estimator: -80.71%)",
              st == 200 and row is not None and row["prompt_tokens"] == rep_o2.get("prompt_tokens")
              and err is not None and abs(err) <= 6,
              f"status={st} reported={rep_o2} row={row} err={err}%")

        # T5: pre-send 402 — the full count (9 tokens, $0.00009)
        # exceeds the $0.00005 budget; the text-only count (3
        # tokens, $0.00003) would have passed.
        hits_before = len(ledger_rows())
        st, _, b = req("POST", "/v1/chat/completions",
                       {"model": "openai-mock", "messages": [{"role": "user", "content": "hello world"}]},
                       key=raw_key, headers={"X-Codvian-Budget": "0.00005",
                                             "X-Codvian-Budget-Strict": "true"})
        time.sleep(0.5)
        check("T5 strict $0.00005 budget on 'hello world' -> 402, zero provider calls",
              st == 402 and len(ledger_rows()) == hits_before,
              f"status={st} rows_before={hits_before} rows_after={len(ledger_rows())} {b[:160]}")

        # T6: the usage feed's drift rows, live.
        st, _, b = req("GET", "/dashboard/usage?range=24h", key=raw_key)
        drift = []
        try: drift = json.loads(b).get("summary", {}).get("drift", [])
        except Exception: drift = []
        by_model = {d.get("model"): d for d in drift}
        ok = st == 200 and all(m in by_model for m in ("openai-mock", "claude-mock", "bedrock-mock")) \
            and all(by_model[m].get("compared", 0) >= 1
                    and abs(by_model[m].get("mean_abs_error_pct", 99)) <= 6 for m in by_model)
        check("T6 /dashboard/usage drift rows for all 3 wires, compared>=1, mean |error|<=6%",
              ok, f"status={st} drift={drift}")

        # T7: the served dashboard carries the drift card.
        st, _, b = req("GET", "/dashboard", key=raw_key)
        check("T7 dashboard serves the drift card, no Playground",
              st == 200 and "Estimate vs reported" in b and "Playground" not in b,
              f"status={st} card={'Estimate vs reported' in b} playground={'Playground' in b}")

        # T8: unauthenticated chat is refused.
        st, _, b = req("POST", "/v1/chat/completions", body)
        check("T8 unauthenticated chat -> 401", st == 401, f"status={st}")
    finally:
        for p in procs:
            try: p.terminate()
            except Exception: pass
    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks passed", flush=True)
    return 1 if failed else 0

if __name__ == "__main__":
    sys.exit(main())
