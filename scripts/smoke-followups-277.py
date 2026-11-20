#!/usr/bin/env python3
"""Live smoke for the sprint-14 follow-ups batch (CHECKLIST 277-278).

Real binary + four Python mocks. No fake data: every assertion reads a
live HTTP status, a live mock hit/frame counter, a live feed, or a live
token-exchange count.

Mocks:
  Bedrock (SigV4 ignored, like the Go test mocks): GET /foundation-models
  lists two models; POST /model/{id}/converse-stream streams Amazon
  event-stream frames — a never-ending model ticks a text delta every
  100 ms forever, so the client abort is proven by the frame log going
  silent after the client closes the connection.
  OpenAI: /v1/models lists model-a; chat + embeddings answer.
  Vertex: GET .../publishers/google/models lists gemini-2.0-flash;
  :generateContent answers Gemini-shaped JSON; :streamGenerateContent
  answers SSE. The token server mints tok-vertex-abc for a JWT-bearer
  grant with a non-empty 3-part assertion and counts exchanges.
  The service-account JSON uses a real 2048-bit RSA key generated with
  openssl at smoke time (PKCS#8 PEM), never a static header.

Keys (added to config.json before start; hashes are hex SHA-256 of the
raw key, the same scheme the gateway uses):
  init key — uncapped, unscoped (control).
  rpm1 / rpm2 — requests_per_minute=1, for the pre-body admission proofs.
  scope — guardrails ["pii"] only, for the per-key scoping proofs.
Config guardrails block_pii + block_secrets are on; intent fast maps to
model-a, best is unmapped.

Checks:
  A provider adds (Bedrock wire bedrock-converse, Vertex wire vertex).
  B Bedrock abort: stream the never-ending model, read 3 chunks, close
    the client connection — the mock's frame log goes silent within
    ~1 s and the router stays healthy.
  C pre-body admission: a valid /v1/messages under rpm1, then a garbage
    body under rpm1 is 429 with zero extra mock chat hits; the same on
    /v1/embeddings under rpm2; the uncapped control answers 400.
  D tier naming: /v1/messages + /v1/responses with intent/best answer
    400 naming "best" with zero mock hits; intent/fast serves 200.
  E per-key scoping: a secret under the pii-only key serves 200, under
    the unscoped key is 403/-32001; PII under the scoped key is 403.
  F Vertex: a chat routes to the mock (Bearer tok-vertex-abc on the
    right project/location path), exactly one token exchange for two
    chats (cached ~1h).
  G dashboard: the Guardrails view carries the per-key scoping card,
    the Providers view the Vertex line, no Playground; the guardrails
    feed reports the scoped key with real counts.
"""
import hashlib
import http.client
import json
import os
import secrets
import shutil
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOME = os.path.expanduser("~/.tmp/smoke-277-278")
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21501
BEDROCK_PORT = 21502
OPENAI_PORT = 21503
VERTEX_PORT = 21504
TOKEN_PORT = 21505
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
AWS_KEY = "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
BEDROCK_MODEL = "anthropic.claude-3-haiku-20240307-v1:0"
NEVER_MODEL = "never-ending-model-v1:0"
VERTEX_MODEL = "gemini-2.0-flash"

checks = []


def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)


def frame(event_type, payload, msg_type="event"):
    body = json.dumps(payload).encode()
    headers = b""

    def hdr(name, value):
        nonlocal headers
        headers += bytes([len(name)]) + name.encode() + b"\x07"
        headers += struct.pack(">H", len(value)) + value.encode()

    hdr(":event-type", event_type)
    hdr(":message-type", msg_type)
    hdr(":content-type", "application/json")
    total = 12 + len(headers) + len(body) + 4
    out = struct.pack(">II", total, len(headers))
    out += struct.pack(">I", zlib.crc32(out) & 0xFFFFFFFF)
    out += headers + body
    out += struct.pack(">I", zlib.crc32(out) & 0xFFFFFFFF)
    return out


class State:
    bedrock_frames = []          # timestamps of Bedrock frames written
    bedrock_lock = threading.Lock()
    openai_chat_hits = 0
    openai_embed_hits = 0
    vertex_hits = 0
    vertex_last_auth = ""
    vertex_last_path = ""
    token_hits = 0
    token_last_assert = ""
    token_lock = threading.Lock()


def bedrock_handler():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            if self.path.startswith("/foundation-models"):
                body = json.dumps({"modelSummaries": [
                    {"modelId": BEDROCK_MODEL}, {"modelId": NEVER_MODEL}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self.send_response(404)
            self.end_headers()

        def do_POST(self):
            ln = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(ln)
            if self.path.endswith("/converse"):
                body = json.dumps({
                    "output": {"message": {"role": "assistant",
                                           "content": [{"text": "hi there"}]}},
                    "stopReason": "end_turn",
                    "usage": {"inputTokens": 12, "outputTokens": 8,
                              "totalTokens": 20},
                }).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if not self.path.endswith("/converse-stream"):
                self.send_response(404)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/vnd.amazon.eventstream")
            self.end_headers()
            try:
                for f in [frame("messageStart", {"role": "assistant"}),
                          frame("contentBlockStart", {"contentBlockIndex": 0, "start": {}})]:
                    self.wfile.write(f)
                    self.wfile.flush()
                    with State.bedrock_lock:
                        State.bedrock_frames.append(time.time())
                i = 0
                while True:
                    time.sleep(0.1)
                    i += 1
                    self.wfile.write(frame("contentBlockDelta",
                                           {"contentBlockIndex": 0,
                                            "delta": {"text": f"t{i} "}}))
                    self.wfile.flush()
                    with State.bedrock_lock:
                        State.bedrock_frames.append(time.time())
            except (BrokenPipeError, ConnectionResetError):
                return
    return H


def openai_handler():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            if self.path.startswith("/v1/models"):
                body = json.dumps({"object": "list", "data": [
                    {"id": "model-a", "object": "model"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self.send_response(404)
            self.end_headers()

        def do_POST(self):
            ln = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(ln)
            if self.path == "/v1/chat/completions":
                State.openai_chat_hits += 1
                body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion",
                                   "created": 1, "model": "model-a",
                                   "choices": [{"index": 0, "message": {"role": "assistant",
                                                "content": "mock says hi"}, "finish_reason": "stop"}],
                                   "usage": {"prompt_tokens": 3, "completion_tokens": 4,
                                             "total_tokens": 7}}).encode()
            elif self.path == "/v1/embeddings":
                State.openai_embed_hits += 1
                body = json.dumps({"object": "list",
                                   "data": [{"object": "embedding", "index": 0,
                                             "embedding": [0.1, 0.2, 0.3]}],
                                   "model": "model-a",
                                   "usage": {"prompt_tokens": 2, "total_tokens": 2}}).encode()
            else:
                self.send_response(404)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    return H


def vertex_handler():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            if self.path.endswith("/publishers/google/models"):
                body = json.dumps({"models": [{"name":
                    "projects/proj-1/locations/us-central1/publishers/google/models/"
                    "gemini-2.0-flash"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self.send_response(404)
            self.end_headers()

        def do_POST(self):
            ln = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(ln)
            State.vertex_hits += 1
            State.vertex_last_auth = self.headers.get("Authorization") or ""
            State.vertex_last_path = self.path
            if self.path.endswith(":generateContent"):
                body = json.dumps({"candidates": [{"content": {"role": "model",
                                      "parts": [{"text": "hi there"}]},
                                      "finishReason": "STOP"}],
                                   "usageMetadata": {"promptTokenCount": 12,
                                                     "candidatesTokenCount": 8,
                                                     "totalTokenCount": 20},
                                   "modelVersion": "gemini-2.0-flash-001"}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if self.path.endswith(":streamGenerateContent"):
                body = (b'data: {"candidates":[{"content":{"role":"model",'
                        b'"parts":[{"text":"hi "}]},"finishReason":"STOP"}]}\n\n'
                        b'data: {"candidates":[{"content":{"role":"model",'
                        b'"parts":[{"text":"there"}]},"finishReason":"STOP"}],'
                        b'"usageMetadata":{"promptTokenCount":12,'
                        b'"candidatesTokenCount":8,"totalTokenCount":20}}\n\n')
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self.send_response(404)
            self.end_headers()
    return H


def token_handler():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            ln = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(ln).decode()
            form = urllib.parse.parse_qs(raw)
            with State.token_lock:
                State.token_hits += 1
                State.token_last_assert = form.get("assertion", [""])[0]
            grant = form.get("grant_type", [""])[0]
            if grant != "urn:ietf:params:oauth:grant-type:jwt-bearer" \
                    or not State.token_last_assert:
                self.send_response(400)
                self.end_headers()
                return
            body = json.dumps({"access_token": "tok-vertex-abc",
                               "expires_in": 3600,
                               "token_type": "Bearer"}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    return H


def req(method, path, body=None, key=None, raw=None, headers=None):
    data = raw.encode() if raw is not None else (
        json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        r.add_header("Content-Type", "application/json")
    if key:
        r.add_header("Authorization", "Bearer " + key)
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
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


def chat(model, prompt, key):
    return req("POST", "/v1/chat/completions",
               {"model": model, "messages": [{"role": "user", "content": prompt}]}, key)


def make_key():
    raw = "crk_" + "".join(secrets.choice(
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
        for _ in range(32))
    return raw, hashlib.sha256(raw.encode()).hexdigest()


def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN,
                        "./cmd/codvian-router"],
                       cwd=ROOT, env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1

    # Real RSA key for the service-account JSON (openssl, PKCS#8 PEM).
    key_pem = os.path.join(HOME, "sa-key.pem")
    pkcs8_pem = os.path.join(HOME, "sa-key-pkcs8.pem")
    r = subprocess.run(["openssl", "genrsa", "-out", key_pem, "2048"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        print("openssl genrsa FAILED:\n" + r.stdout + r.stderr)
        return 1
    r = subprocess.run(["openssl", "pkcs8", "-topk8", "-nocrypt",
                        "-in", key_pem, "-out", pkcs8_pem],
                       capture_output=True, text=True)
    if r.returncode != 0:
        print("openssl pkcs8 FAILED:\n" + r.stdout + r.stderr)
        return 1
    with open(pkcs8_pem) as f:
        priv_pem = f.read()
    sa_json = json.dumps({
        "type": "service-account",
        "project_id": "proj-1",
        "client_email": "vertex-test@proj-1.iam.gserviceaccount.com",
        "private_key": priv_pem,
        "token_uri": f"http://127.0.0.1:{TOKEN_PORT}/token",
    })

    servers = []
    for port, handler in ((BEDROCK_PORT, bedrock_handler()),
                          (OPENAI_PORT, openai_handler()),
                          (VERTEX_PORT, vertex_handler()),
                          (TOKEN_PORT, token_handler())):
        srv = ThreadingHTTPServer(("127.0.0.1", port), handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        servers.append(srv)

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
    cfg = json.load(open(cfg_path))
    cfg["auto_router"] = {"intent_tiers": {"fast": {"model": "model-a"}}}
    cfg["guardrails"] = {"block_pii": True, "block_secrets": True}
    rpm1_raw, rpm1_hash = make_key()
    rpm2_raw, rpm2_hash = make_key()
    scope_raw, scope_hash = make_key()
    cfg.setdefault("gateway_keys", []).extend([
        {"id": "gk_smoke_rpm1", "name": "smoke-rpm1", "key_hash": rpm1_hash,
         "scopes": ["chat"], "budget_usd": 0, "requests_per_minute": 1},
        {"id": "gk_smoke_rpm2", "name": "smoke-rpm2", "key_hash": rpm2_hash,
         "scopes": ["chat"], "budget_usd": 0, "requests_per_minute": 1},
        {"id": "gk_smoke_scope", "name": "smoke-scope", "key_hash": scope_hash,
         "scopes": ["chat"], "budget_usd": 0, "guardrails": ["pii"]},
    ])
    json.dump(cfg, open(cfg_path, "w"), indent=2)

    router = subprocess.Popen(
        [BIN, "serve", "--listen", f"127.0.0.1:{ROUTER_PORT}"],
        env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        for _ in range(50):
            st, _, _ = req("GET", "/healthz")
            if st == 200:
                break
            time.sleep(0.2)
        else:
            print("ROUTER never became ready")
            return 1

        # ---- A: provider adds ----
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "bedrock", "base_url": f"http://127.0.0.1:{BEDROCK_PORT}",
                           "api_key": AWS_KEY}, key=raw_key)
        try:
            added = json.loads(body)
        except Exception:
            added = {}
        check("A1 add Bedrock provider via catalog",
              st == 201 and added.get("wire_format") == "bedrock-converse",
              f"status={st} body={body[:200]}")
        st, _, body = req("POST", "/api/providers",
                          {"name": "openai-mock", "base_url": f"http://127.0.0.1:{OPENAI_PORT}",
                           "api_key": "mock-key"}, key=raw_key)
        check("A2 add OpenAI mock provider", st == 201, f"status={st} body={body[:200]}")
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "vertex",
                           "base_url": f"http://127.0.0.1:{VERTEX_PORT}/v1/projects/proj-1/locations/us-central1",
                           "api_key": sa_json}, key=raw_key)
        try:
            added_v = json.loads(body)
        except Exception:
            added_v = {}
        check("A3 add Vertex provider via catalog (wire vertex)",
              st == 201 and added_v.get("wire_format") == "vertex",
              f"status={st} body={body[:300]}")

        # ---- B: Bedrock abort — never-ending stream, client closes ----
        conn = http.client.HTTPConnection("127.0.0.1", ROUTER_PORT, timeout=30)
        conn.request("POST", "/v1/chat/completions",
                     body=json.dumps({"model": NEVER_MODEL, "stream": True,
                                      "messages": [{"role": "user", "content": "Count forever."}]}),
                     headers={"Content-Type": "application/json",
                              "Authorization": "Bearer " + raw_key})
        resp = conn.getresponse()
        chunks = 0
        t0 = time.time()
        while time.time() - t0 < 5 and chunks < 3:
            line = resp.readline()
            if not line:
                break
            if line.startswith(b"data: ") and line.strip() != b"data: [DONE]":
                try:
                    d = json.loads(line[6:])
                except Exception:
                    continue
                if d.get("choices", [{}])[0].get("delta", {}).get("content"):
                    chunks += 1
        conn.close()
        close_at = time.time()
        time.sleep(1.5)
        with State.bedrock_lock:
            after = [t for t in State.bedrock_frames if t > close_at + 0.6]
        check("B1 never-ending Bedrock stream delivers chunks while open",
              resp.status == 200 and chunks >= 3,
              f"status={resp.status} content_chunks={chunks}")
        check("B2 upstream stops after client close (no frames >0.6s past close)",
              not after,
              f"frames_after_close={len(after)} total_frames={len(State.bedrock_frames)}")
        st, _, body = chat(BEDROCK_MODEL, "hello", raw_key)
        check("B3 router healthy + normal Bedrock chat after abort",
              st == 200 and "hi there" in body,
              f"status={st} body={body[:160]}")
        st, _, _ = req("GET", "/healthz")
        check("B4 /healthz 200 after abort", st == 200, f"status={st}")

        # ---- C: pre-body admission on relay + embeddings ----
        msg_body = {"model": "model-a", "max_tokens": 8,
                    "messages": [{"role": "user", "content": "hello"}]}
        st, _, body = req("POST", "/v1/messages", msg_body, key=rpm1_raw)
        check("C1 valid /v1/messages under rpm1 serves 200",
              st == 200, f"status={st} body={body[:160]}")
        chat_before = State.openai_chat_hits
        st, _, body = req("POST", "/v1/messages", raw='{"model":', key=rpm1_raw)
        check("C2 garbage /v1/messages under rpm1 is 429 pre-body",
              st == 429 and State.openai_chat_hits == chat_before,
              f"status={st} chat_hits={chat_before}->{State.openai_chat_hits}")
        st, _, body = req("POST", "/v1/embeddings",
                          {"model": "model-a", "input": "hello"}, key=rpm2_raw)
        check("C3 valid /v1/embeddings under rpm2 serves 200",
              st == 200, f"status={st} body={body[:160]}")
        emb_before = State.openai_embed_hits
        st, _, body = req("POST", "/v1/embeddings", raw='{"model":', key=rpm2_raw)
        check("C4 garbage /v1/embeddings under rpm2 is 429 pre-body",
              st == 429 and State.openai_embed_hits == emb_before,
              f"status={st} embed_hits={emb_before}->{State.openai_embed_hits}")
        st, _, body = req("POST", "/v1/messages", raw='{"model":', key=raw_key)
        check("C5 control: garbage /v1/messages under uncapped key is 400",
              st == 400, f"status={st} body={body[:160]}")
        st, _, body = req("POST", "/v1/embeddings", raw='{"model":', key=raw_key)
        check("C6 control: garbage /v1/embeddings under uncapped key is 400",
              st == 400, f"status={st} body={body[:160]}")
        st, feed = jget("/dashboard/guardrails")
        check("C7 early_refusals counter moved (admission counted)",
              st == 200 and feed.get("early_refusals", 0) >= 1,
              f"status={st} early_refusals={feed.get('early_refusals')}")

        # ---- D: intent tier naming on Messages + Responses ----
        chat_before = State.openai_chat_hits
        st, _, body = req("POST", "/v1/messages",
                          {"model": "intent/best", "max_tokens": 8,
                           "messages": [{"role": "user", "content": "hello"}]},
                          key=raw_key)
        try:
            msg = json.loads(body).get("error", {}).get("message", "")
        except Exception:
            msg = body
        check("D1 /v1/messages intent/best is 400 naming the tier",
              st == 400 and '"best"' in msg and "not mapped" in msg
              and State.openai_chat_hits == chat_before,
              f"status={st} msg={msg[:160]} chat_hits={State.openai_chat_hits}")
        st, _, body = req("POST", "/v1/messages",
                          {"model": "intent/fast", "max_tokens": 8,
                           "messages": [{"role": "user", "content": "hello"}]},
                          key=raw_key)
        check("D2 /v1/messages intent/fast serves 200",
              st == 200, f"status={st} body={body[:160]}")
        st, _, body = req("POST", "/v1/responses",
                          {"model": "intent/best", "input": "hello"}, key=raw_key)
        try:
            msg = json.loads(body).get("error", {}).get("message", "")
        except Exception:
            msg = body
        check("D3 /v1/responses intent/best is 400 naming the tier",
              st == 400 and '"best"' in msg and "not mapped" in msg,
              f"status={st} msg={msg[:160]}")
        st, _, body = req("POST", "/v1/responses",
                          {"model": "intent/fast", "input": "hello"}, key=raw_key)
        check("D4 /v1/responses intent/fast serves 200",
              st == 200, f"status={st} body={body[:160]}")

        # ---- E: per-key guardrail scoping ----
        secret = "my key is " + "sk-proj-" + "xxxxxxxxxxxxxxxxxxxx"
        st, _, body = chat("model-a", f"Please check this string: {secret}", scope_raw)
        check("E1 secret under pii-only key serves 200 (secret guardrail not run)",
              st == 200, f"status={st} body={body[:160]}")
        st, _, body = chat("model-a", f"Another note with this string: {secret}", raw_key)
        check("E2 secret under unscoped key is 403/-32001",
              st == 403 and "-32001" in body,
              f"status={st} body={body[:200]}")
        st, _, body = chat("model-a", "Reach me at smoke-tester@example.com please", scope_raw)
        check("E3 PII under pii-only key is 403 (its listed guardrail runs)",
              st == 403, f"status={st} body={body[:200]}")

        # ---- F: Vertex routes through the real binary ----
        st, _, body = chat(VERTEX_MODEL, "Hi Vertex", raw_key)
        check("F1 Vertex chat 200 + content",
              st == 200 and "hi there" in body, f"status={st} body={body[:200]}")
        # The add's key test already minted once (TestKey -> ListModels);
        # the first chat mints a second token on its own client, then
        # the token is cached: the second chat must not mint again.
        check("F2 token exchanges: 1 (add key test) + 1 (first chat)",
              State.token_hits == 2, f"token_hits={State.token_hits}")
        check("F3 Vertex request carried the minted token",
              State.vertex_last_auth == 'Bearer tok-vertex-abc',
              f"auth={State.vertex_last_auth!r}")
        check("F4 Vertex request hit the project/location path",
              State.vertex_last_path.endswith(
                  "/v1/projects/proj-1/locations/us-central1/publishers/google/models/"
                  "gemini-2.0-flash:generateContent"),
              f"path={State.vertex_last_path!r}")
        check("F5 token assertion is a 3-part JWT",
              len(State.token_last_assert.split(".")) == 3,
              f"parts={len(State.token_last_assert.split('.'))}")
        st, _, body = chat(VERTEX_MODEL, "Hi Vertex again", raw_key)
        check("F6 second Vertex chat 200, token still cached (2 exchanges)",
              st == 200 and State.token_hits == 2,
              f"status={st} token_hits={State.token_hits}")

        # ---- G: dashboard surfaces ----
        st, _, body = req("GET", "/dashboard")
        check("G1 dashboard carries the per-key scoping card + Vertex line, no Playground",
              st == 200 and "Per-key guardrail scoping" in body
              and "Vertex AI providers" in body and "Playground" not in body,
              f"status={st}")
        st, feed = jget("/dashboard/guardrails")
        ks = feed.get("key_scoping", {}) if isinstance(feed, dict) else {}
        check("G2 guardrails feed: 1 scoped key of 4, scoped_evals >= 1",
              st == 200 and ks.get("scoped_keys") == 1 and ks.get("total_keys") == 4
              and ks.get("scoped_evals", 0) >= 1,
              f"status={st} key_scoping={ks}")
        st, feed = jget("/dashboard/providers")
        provs = feed.get("providers", []) if isinstance(feed, dict) else []
        vx = [p for p in provs if p.get("wire_format") == "vertex"]
        check("G3 providers feed lists one vertex-wire provider",
              st == 200 and len(vx) == 1,
              f"status={st} vertex_providers={len(vx)}")
    finally:
        router.terminate()
        try:
            router.wait(timeout=10)
        except Exception:
            router.kill()
        for srv in servers:
            srv.shutdown()

    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks green")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
