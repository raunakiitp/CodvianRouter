#!/usr/bin/env python3
"""Live smoke for Bedrock event-stream incremental delivery (CHECKLIST 215-216).

Real binary + one Python Bedrock mock. No fake data: every assertion
reads a live SSE chunk, a live timestamp, a live HTTP status, or a
live mock write log.

Mock: a Bedrock Converse stand-in (SigV4 headers ignored, like the Go
test mocks). GET /foundation-models lists one model (the add-provider
key test). POST /model/{id}/converse-stream answers with an Amazon
event-stream body:
  - trickle mode (default): messageStart + contentBlockStart + delta
    "first " flushed immediately, then a 600 ms pause, then delta
    "second" + contentBlockStop + metadata + messageStop. The first
    SSE chunk must reach the client >= 300 ms before the response
    completes — the old ReadAll reader delivered nothing until the
    body completed.
  - never-ending mode (model never-ending-model-v1:0): ticks a text
    delta every 100 ms forever. The client must receive chunks while
    the stream is still open, then cancel by closing the connection.
"""
import json
import os
import shutil
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOME = os.path.expanduser("~/.tmp/smoke-215-216")
BIN = os.path.join(HOME, "codvian-router")
ROUTER_PORT = 21471
MOCK_PORT = 21472
BASE = f"http://127.0.0.1:{ROUTER_PORT}"
MOCK = f"http://127.0.0.1:{MOCK_PORT}"
MODEL = "anthropic.claude-3-haiku-20240307-v1:0"
NEVER_MODEL = "never-ending-model-v1:0"
# The AWS docs example key pair, as used by the Go test mocks — not a
# real credential; the mock never verifies the signature.
AWS_KEY = "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

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


FRAMES_START = [
    frame("messageStart", {"role": "assistant"}),
    frame("contentBlockStart", {"contentBlockIndex": 0, "start": {}}),
    frame("contentBlockDelta", {"contentBlockIndex": 0, "delta": {"text": "first "}}),
]
FRAMES_END = [
    frame("contentBlockDelta", {"contentBlockIndex": 0, "delta": {"text": "second"}}),
    frame("contentBlockStop", {"contentBlockIndex": 0}),
    frame("metadata", {"usage": {"inputTokens": 12, "outputTokens": 8, "totalTokens": 20}}),
    frame("messageStop", {"stopReason": "end_turn"}),
]

WRITES_LOG = os.path.join(HOME, "mock-writes.log")


def log_write():
    with open(WRITES_LOG, "a") as f:
        f.write(f"{time.time():.3f}\n")


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path.startswith("/foundation-models"):
            body = json.dumps(
                {"modelSummaries": [{"modelId": MODEL},
                                    {"modelId": NEVER_MODEL}]}
            ).encode()
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
        if self.path.endswith("/converse-stream"):
            self.send_response(200)
            self.send_header("Content-Type",
                             "application/vnd.amazon.eventstream")
            self.end_headers()
            # The model ID rides in the path: NEVER_MODEL selects the
            # never-ending tick mode, any other model trickles.
            never = NEVER_MODEL in self.path
            if never:
                try:
                    for f in FRAMES_START:
                        self.wfile.write(f)
                        self.wfile.flush()
                        log_write()
                    i = 0
                    while True:
                        time.sleep(0.1)
                        i += 1
                        self.wfile.write(frame(
                            "contentBlockDelta",
                            {"contentBlockIndex": 0,
                             "delta": {"text": f"t{i} "}}))
                        self.wfile.flush()
                        log_write()
                except (BrokenPipeError, ConnectionResetError):
                    return
                return
            try:
                for f in FRAMES_START:
                    self.wfile.write(f)
                    self.wfile.flush()
                    log_write()
                time.sleep(0.6)
                for f in FRAMES_END:
                    self.wfile.write(f)
                    self.wfile.flush()
                    log_write()
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        self.send_response(404)
        self.end_headers()


def req(method, path, body=None, key=None, base=BASE):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(base + path, data=data, method=method)
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


def writes():
    try:
        with open(WRITES_LOG) as f:
            return [float(x) for x in f.read().split() if x]
    except Exception:
        return []


def main():
    shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    open(WRITES_LOG, "w").write("")
    env = dict(os.environ, HOME=HOME, TMPDIR=os.path.expanduser("~/.tmp"),
               GOCACHE=os.path.expanduser("~/.cache/go-build"),
               GOPATH=os.path.expanduser("~/go"))
    r = subprocess.run(["/home/hatch/go/bin/go", "build", "-o", BIN,
                        "./cmd/codvian-router"],
                       cwd=os.path.expanduser("~/workspace/codvian-router"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("BUILD FAILED:\n" + r.stdout + r.stderr)
        return 1
    mock = ThreadingHTTPServer(("127.0.0.1", MOCK_PORT), H)
    threading.Thread(target=mock.serve_forever, daemon=True).start()

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

        st, _, body = req("POST", "/api/providers",
                          {"catalog_id": "bedrock", "base_url": MOCK,
                           "api_key": AWS_KEY}, key=raw_key)
        try:
            added = json.loads(body)
        except Exception:
            added = {}
        check("A1 add Bedrock provider via catalog",
              st == 201 and added.get("wire_format") == "bedrock-converse"
              and added.get("routing_live") is True,
              f"status={st} body={body[:300]}")

        # ---- S1: trickle stream, first chunk before completion ----
        import http.client
        conn = http.client.HTTPConnection("127.0.0.1", ROUTER_PORT, timeout=30)
        payload = json.dumps({
            "model": MODEL, "stream": True,
            "messages": [{"role": "user", "content": "Say first second."}],
        })
        conn.request("POST", "/v1/chat/completions", body=payload, headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + raw_key})
        resp = conn.getresponse()
        start = time.time()
        first_at = None
        content = ""
        done = False
        while True:
            line = resp.readline()
            if not line:
                break
            if line.startswith(b"data: ") and line.strip() != b"data: [DONE]":
                try:
                    d = json.loads(line[6:])
                except Exception:
                    continue
                delta = d.get("choices", [{}])[0].get("delta", {})
                if delta.get("content"):
                    if first_at is None:
                        first_at = time.time()
                    content += delta["content"]
            if line.strip() == b"data: [DONE]":
                done = True
                break
        end = time.time()
        conn.close()
        check("S1 streaming chat 200 + [DONE]",
              resp.status == 200 and done, f"status={resp.status} done={done}")
        check("S2 content reassembled 'first second'",
              content == "first second", f"content={content!r}")
        check("S3 first chunk >= 300 ms before completion (incremental)",
              first_at is not None and first_at - start < (end - start) - 0.3,
              f"first_at={None if first_at is None else round(first_at - start, 3)}s "
              f"total={round(end - start, 3)}s")

        # ---- S4: never-ending stream still streams (via the router) ----
        # NEVER_MODEL makes the mock tick a text delta every 100 ms
        # forever. The client must receive content chunks while the
        # stream is still open, then cancel by closing the
        # connection; the router must stay healthy afterwards.
        before = len(writes())
        conn = http.client.HTTPConnection("127.0.0.1", ROUTER_PORT, timeout=30)
        payload = json.dumps({
            "model": NEVER_MODEL, "stream": True,
            "messages": [{"role": "user", "content": "Count forever."}],
        })
        conn.request("POST", "/v1/chat/completions", body=payload, headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + raw_key})
        resp = conn.getresponse()
        chunks = 0
        t0 = time.time()
        first_at = None
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
                    if first_at is None:
                        first_at = time.time()
        conn.close()
        time.sleep(0.5)
        check("S4 never-ending stream delivers chunks while open",
              resp.status == 200 and chunks >= 3 and first_at is not None
              and first_at - t0 < 2,
              f"status={resp.status} content_chunks={chunks} "
              f"first_at={None if first_at is None else round(first_at - t0, 3)}s "
              f"mock_frames_since_start={len(writes()) - before}")

        # ---- S5: non-streaming still works + router healthy ----
        st, _, body = req("POST", "/v1/chat/completions",
                          {"model": MODEL,
                           "messages": [{"role": "user", "content": "hi"}]},
                          key=raw_key)
        check("S5 non-streaming chat 200 + content",
              st == 200 and "hi there" in body, f"status={st} body={body[:200]}")
        st, _, _ = req("GET", "/healthz")
        check("S6 router healthy after streams", st == 200, f"status={st}")
    finally:
        router.terminate()
        try:
            router.wait(timeout=10)
        except Exception:
            router.kill()
        mock.shutdown()

    failed = [c for c in checks if not c[1]]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} checks green")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
