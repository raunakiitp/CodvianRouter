#!/usr/bin/env python3
"""Mock upstream for the GC-profile benchmark (bench-gc-profile.sh).

Stdlib only. Serves the two endpoints the router's provider probe and
chat path need:

  GET  /v1/models           -> {"data":[{"id":"model-a"}]}
  POST /v1/chat/completions -> a fixed small OpenAI-compatible reply

A fixed --delay (seconds, default 0.002) simulates upstream latency so
the numbers are not pure loopback noise. Usage:

    python3 bench-mock-upstream.py --port 18099 --delay 0.002
"""
import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = {"object": "list", "data": [{"id": "model-a", "object": "model"}]}
REPLY = {
    "id": "chatcmpl-mock",
    "object": "chat.completion",
    "model": "model-a",
    "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
    "usage": {"prompt_tokens": 8, "completion_tokens": 1, "total_tokens": 9},
}


class Handler(BaseHTTPRequestHandler):
    delay = 0.002
    # HTTP/1.1 keep-alive: the default (HTTP/1.0) closes the connection
    # after every response, and the router's Go client then sends the
    # next POST down a pooled dead connection (EOF -> router 502).
    # That failure is a mock artifact, not router behaviour.
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # keep stdout clean for the harness
        pass

    def _json(self, obj, status=200):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/v1/models":
            time.sleep(self.delay)
            self._json(MODELS)
        else:
            self._json({"error": "not found"}, status=404)

    def do_POST(self):
        if self.path == "/v1/chat/completions":
            n = int(self.headers.get("Content-Length") or 0)
            if n:
                self.rfile.read(n)
            time.sleep(self.delay)
            self._json(REPLY)
        else:
            self._json({"error": "not found"}, status=404)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18099)
    ap.add_argument("--delay", type=float, default=0.002)
    args = ap.parse_args()
    Handler.delay = args.delay

    class Server(ThreadingHTTPServer):
        daemon_threads = True
        allow_reuse_address = True
        # Default backlog (5) overflows under 20 concurrent clients and
        # the kernel refuses connections; 128 keeps the mock out of the
        # way of what is being measured (the router).
        request_queue_size = 128

    Server(("127.0.0.1", args.port), Handler).serve_forever()
