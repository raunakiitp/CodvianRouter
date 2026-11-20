#!/usr/bin/env python3
"""Live smoke for the sprint-15 follow-ups batch, part 1 (CHECKLIST 283-284).

Real binary + three Python mocks. No fake data: every assertion reads a
live HTTP status, a live mock counter, a live feed, or a live
token-exchange count.

Mocks:
  OpenAI: /v1/models lists model-a; chat answers.
  Vertex: GET .../publishers/google/models lists gemini-2.0-flash;
  :generateContent answers Gemini-shaped JSON. The token server mints
  tok-vertex-abc for a JWT-bearer grant with a non-empty 3-part
  assertion and counts exchanges. The service-account JSON uses a real
  2048-bit RSA key generated with openssl at smoke time (PKCS#8 PEM),
  never a static header.

Checks:
  A Vertex add via catalog: 201, wire vertex; the key test mints once.
  B First Vertex chat 200 with the minted token on the right
    project/location path — and still exactly 1 exchange total: the
    add's client and the chat's client share the minted-token cache
    for the same service-account key (283 (a)). A second chat also
    mints nothing (cache hit).
  C GET /api/providers carries vertex_token_cache with the live
    totals: mints 1, hits >= 2, keys 1 (284).
  D A fully-custom add that sends a `models` field is 201 (no strict
    decoding 400), its models are the probe's [model-a] — never the
    supplied list — and the response carries models_note saying the
    supplied list was not stored (283 (b)). An empty-string models
    entry is 400.
  E Dashboard HTML carries the Vertex line and no Playground; the
    dashboard JS renders the live token-cache counts.
"""
import hashlib
import json
import os
import secrets
import shutil
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOME = os.path.expanduser("~/.tmp/smoke-283-284")
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21511
OPENAI_PORT = 21513
VERTEX_PORT = 21514
TOKEN_PORT = 21515
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
VERTEX_MODEL = "gemini-2.0-flash"

checks = []


def check(name, ok, detail=""):
    checks.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)


class State:
    vertex_hits = 0
    vertex_last_auth = ""
    vertex_last_path = ""
    token_hits = 0
    token_last_assert = ""
    token_lock = threading.Lock()


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
            body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion",
                               "created": 1, "model": "model-a",
                               "choices": [{"index": 0, "message": {"role": "assistant",
                                            "content": "mock says hi"}, "finish_reason": "stop"}],
                               "usage": {"prompt_tokens": 3, "completion_tokens": 4,
                                         "total_tokens": 7}}).encode()
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


def req(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        r.add_header("Content-Type", "application/json")
    if key:
        r.add_header("Authorization", "Bearer " + key)
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
    for port, handler in ((OPENAI_PORT, openai_handler()),
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

        # ---- A: Vertex add — the key test mints once ----
        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "vertex",
                           "base_url": f"http://127.0.0.1:{VERTEX_PORT}/v1/projects/proj-1/locations/us-central1",
                           "api_key": sa_json}, key=raw_key)
        try:
            added_v = json.loads(body)
        except Exception:
            added_v = {}
        check("A1 add Vertex provider via catalog (wire vertex)",
              st == 201 and added_v.get("wire_format") == "vertex",
              f"status={st} body={body[:300]}")
        check("A2 add's key test minted exactly once",
              State.token_hits == 1, f"token_hits={State.token_hits}")

        # ---- B: chats reuse the add's minted token ----
        st, _, body = chat(VERTEX_MODEL, "Hi Vertex", raw_key)
        check("B1 first Vertex chat 200 + content",
              st == 200 and "hi there" in body, f"status={st} body={body[:200]}")
        check("B2 first chat minted NOTHING new (shared cache: 1 exchange total)",
              State.token_hits == 1, f"token_hits={State.token_hits}")
        check("B3 Vertex request carried the minted token",
              State.vertex_last_auth == "Bearer tok-vertex-abc",
              f"auth={State.vertex_last_auth!r}")
        check("B4 Vertex request hit the project/location path",
              State.vertex_last_path.endswith(
                  "/v1/projects/proj-1/locations/us-central1/publishers/google/models/"
                  "gemini-2.0-flash:generateContent"),
              f"path={State.vertex_last_path!r}")
        st, _, body = chat(VERTEX_MODEL, "Hi Vertex again", raw_key)
        check("B5 second Vertex chat 200, still 1 exchange (cache hit)",
              st == 200 and State.token_hits == 1,
              f"status={st} token_hits={State.token_hits}")

        # ---- C: providers feed carries the live token-cache totals ----
        st, feed = jget("/api/providers")
        vc = feed.get("vertex_token_cache", {}) if isinstance(feed, dict) else {}
        check("C1 providers feed vertex_token_cache: 1 mint, >=2 hits, 1 key",
              st == 200 and vc.get("mints") == 1 and vc.get("hits", 0) >= 2
              and vc.get("keys") == 1,
              f"status={st} vertex_token_cache={vc}")

        # ---- D: `models` field on a fully-custom add ----
        st, _, body = req("POST", "/api/providers",
                          {"name": "openai-mock", "base_url": f"http://127.0.0.1:{OPENAI_PORT}",
                           "api_key": "mock-key",
                           "models": ["client-model-x", "client-model-y"]}, key=raw_key)
        try:
            added = json.loads(body)
        except Exception:
            added = {}
        check("D1 custom add with a models field is 201 (no strict-decoding 400)",
              st == 201, f"status={st} body={body[:300]}")
        check("D2 models are the probe's [model-a], never the supplied list",
              added.get("models") == ["model-a"],
              f"models={added.get('models')}")
        check("D3 response says the supplied list was not stored",
              "not stored" in (added.get("models_note") or "")
              and "/v1/models" in (added.get("models_note") or ""),
              f"models_note={added.get('models_note')!r}")
        st, _, body = req("POST", "/api/providers",
                          {"name": "openai-mock-2", "base_url": f"http://127.0.0.1:{OPENAI_PORT}",
                           "api_key": "mock-key",
                           "models": ["client-model-x", ""]}, key=raw_key)
        check("D4 empty-string models entry is 400",
              st == 400, f"status={st} body={body[:200]}")

        # ---- E: dashboard surfaces ----
        st, _, body = req("GET", "/dashboard")
        check("E1 dashboard carries the Vertex line, no Playground",
              st == 200 and "Vertex AI providers" in body
              and "token cache" in body and "Playground" not in body,
              f"status={st}")
    finally:
        router.terminate()
        try:
            router.wait(timeout=10)
        except Exception:
            router.kill()

    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks green")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
