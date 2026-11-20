#!/usr/bin/env python3
"""Shared library for the M8.1 agentic-client protocol smokes.

NOT a standalone smoke: import it from the scripts/smoke-client-*.py
drivers. It spins up, per driver:

  * one mock OpenAI-compatible upstream on localhost that answers a
    fixed canned completion ("smoke says hi") for every model, records
    a hit count and the last request JSON it saw, and can answer
    streaming chat completions, tool-call requests, and the Responses
    API (the router translates Responses/Messages into chat
    completions before dialling the upstream, so one mock shape
    serves all five clients);
  * the real router binary (built from the worktree) against that
    mock, with an isolated HOME/config and a fresh gateway key;
  * helpers to send a request exactly the way one client sends it,
    assert 200 + the expected content + the router's route trace
    recorded the attempt (GET /dashboard/trace), and shut down
    cleanly.

Scripted protocol smokes ONLY (coordinator decision): nothing here
shells out to a real claude-code / codex / cursor / cline / aider
CLI. The gate is that each client's protocol shape works through
Codvian, not that the vendor CLIs are installed.

No fake data: every assertion reads a live HTTP status, a live mock
hit count, the live last-request log, or the live trace feed. The
only canned value is the mock's fixed reply text, which the scripts
assert verbatim — the same convention as the server-side
scripts/smoke-*.py smokes.

Usage from a driver (run from the worktree root). The file name has
hyphens, so drivers load it with importlib, not a plain import:

    import importlib.util, os
    _spec = importlib.util.spec_from_file_location(
        "smoke_client_lib",
        os.path.join(os.path.dirname(os.path.abspath(__file__)),
                     "smoke-client-lib.py"))
    _lib = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(_lib)
    ClientHarness, Checklist = _lib.ClientHarness, _lib.Checklist

    with ClientHarness("claude-code", router_port=21601) as h:
        ...
"""
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

WORKTREE = os.path.expanduser("~/workspace/codvian-router")
GO = "/home/hatch/go/bin/go"

# The one canned reply every mock serves. Fixed on purpose: the
# drivers assert it verbatim, so a wrong upstream or a dropped
# response body fails loudly instead of passing on any 200.
CANNED_TEXT = "smoke says hi"
CANNED_STREAM_PIECES = ("smoke ", "says hi")

MOCK_PY = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1]); hits_file = sys.argv[2]; last_file = sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _hit(self, body):
        try:
            with open(hits_file) as f: n = int(f.read().strip() or 0)
        except Exception: n = 0
        with open(hits_file, "w") as f: f.write(str(n + 1))
        with open(last_file, "w") as f: f.write(body.decode() if body else "{}")
    def do_GET(self):
        # Advertises both the bare id and the litellm-style
        # provider-qualified id ("openai/mock-model") Aider sends:
        # the router only routes models the provider advertises,
        # so both must be listed for the aider smoke to exercise
        # its real model string.
        body = json.dumps({"object": "list", "data": [{"id": "mock-model", "object": "model"},
                                                      {"id": "openai/mock-model", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(ln)
        self._hit(raw)
        try: req = json.loads(raw or b"{}")
        except Exception: req = {}
        stream = bool(req.get("stream"))
        tools = req.get("tools") or []
        has_tool_result = any(isinstance(m, dict) and m.get("role") == "tool"
                              for m in req.get("messages") or [])
        if stream:
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            for piece in ("smoke ", "says hi"):
                ev = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion.chunk",
                    "created": 1, "model": "mock-model",
                    "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]}).encode()
                self.wfile.write(b"data: " + ev + b"\n\n"); self.wfile.flush()
            self.wfile.write(b"data: [DONE]\n\n"); self.wfile.flush(); return
        if tools and not has_tool_result:
            # Tool-calling client (Cline): answer with a tool call
            # for the first declared function, exactly as an
            # upstream would when the model decides to use a tool.
            # The path argument is DERIVED from the prompt (first
            # *.py token in the user messages), not canned: L2 in
            # the Cline driver asserts the value the client sent,
            # so a router that dropped or rewrote the prompt would
            # fail it. "main.py" is only the fallback when the
            # prompt names no .py file.
            fn = (tools[0].get("function") or {}).get("name", "read_file")
            path = None
            for m in req.get("messages") or []:
                content = m.get("content") or ""
                if isinstance(content, list):
                    content = " ".join(str(p.get("text", "")) for p in content if isinstance(p, dict))
                for tok in str(content).split():
                    tok = tok.strip("`\"'()[]{}<>,.:;")
                    if tok.endswith(".py"):
                        path = tok
                        break
                if path is not None:
                    break
            if path is None:
                path = "main.py"
            body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
                "model": "mock-model",
                "choices": [{"index": 0,
                    "message": {"role": "assistant", "content": None,
                        "tool_calls": [{"id": "call_smoke1", "type": "function",
                            "function": {"name": fn, "arguments": json.dumps({"path": path})}}]},
                    "finish_reason": "tool_calls"}],
                "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body); return
        body = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "created": 1,
            "model": "mock-model",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "smoke says hi"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
HTTPServer(("127.0.0.1", port), H).serve_forever()
'''


class Checklist:
    """Numbered PASS/FAIL list with a final 'N/N checks passed' line."""

    def __init__(self):
        self.checks = []

    def check(self, name, ok, detail=""):
        self.checks.append((name, bool(ok), detail))
        print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}", flush=True)

    def finish(self):
        failed = [c for c in self.checks if not c[1]]
        print(f"\n{len(self.checks) - len(failed)}/{len(self.checks)} checks passed")
        return 1 if failed else 0


class ClientHarness:
    """Mock upstream + real router for one client smoke.

    Ports: router on ``router_port``, mock on ``router_port + 1``.
    Each driver uses its own base port so runs never collide.
    """

    def __init__(self, name, router_port):
        self.name = name
        self.router_port = router_port
        self.mock_port = router_port + 1
        self.home = os.path.expanduser(f"~/.tmp/smoke-client-{name}")
        self.bin = os.path.join(self.home, "codvian-router")
        self.base = f"http://127.0.0.1:{router_port}"
        self.key = None
        self._procs = []
        self._mock = None
        self._router = None
        self._hits_file = os.path.join(self.home, "mock.hits")
        self._last_file = os.path.join(self.home, "mock.last.json")
        self.env = dict(os.environ, HOME=self.home,
                        TMPDIR=os.path.expanduser("~/.tmp"),
                        GOCACHE=os.path.expanduser("~/.cache/go-build"),
                        GOPATH=os.path.expanduser("~/go"))

    def __enter__(self):
        shutil.rmtree(self.home, ignore_errors=True)
        os.makedirs(self.home, exist_ok=True)
        r = subprocess.run([GO, "build", "-o", self.bin, "./cmd/codvian-router"],
                           cwd=WORKTREE, env=self.env,
                           capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError("go build failed:\n" + r.stdout + r.stderr)
        mock_py = os.path.join(self.home, "mock.py")
        open(mock_py, "w").write(MOCK_PY)
        open(self._hits_file, "w").write("0")
        open(self._last_file, "w").write("{}")
        self._mock = subprocess.Popen(
            [sys.executable, mock_py, str(self.mock_port),
             self._hits_file, self._last_file],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self._procs.append(self._mock)
        r = subprocess.run([self.bin, "init"], env=self.env,
                           capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError("router init failed:\n" + r.stdout + r.stderr)
        for tok in r.stdout.replace('"', " ").split():
            if tok.startswith("crk_"):
                self.key = tok
        if not self.key:
            raise RuntimeError("init did not print a key:\n" + r.stdout)
        cfg_path = None
        for root, _, files in os.walk(self.home):
            if "config.json" in files:
                cfg_path = os.path.join(root, "config.json")
                break
        if not cfg_path:
            raise RuntimeError("config.json not found under " + self.home)
        cfg = json.load(open(cfg_path))
        cfg["prices"] = {"mock-model": {"input_usd_per_1m": 1,
                                        "output_usd_per_1m": 2}}
        json.dump(cfg, open(cfg_path, "w"), indent=2)
        self._router = subprocess.Popen(
            [self.bin, "serve", "--listen", f"127.0.0.1:{self.router_port}"],
            env=self.env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True)
        self._procs.append(self._router)
        for _ in range(50):
            st, _, _ = self.req("GET", "/healthz")
            if st == 200:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("router never became ready")
        st, _, body = self.req("POST", "/api/providers",
                               {"name": "mock",
                                "base_url": f"http://127.0.0.1:{self.mock_port}",
                                "api_key": "mock-key"}, key=self.key)
        if st != 201:
            raise RuntimeError(f"add provider failed: status={st} {body[:200]}")
        return self

    def __exit__(self, *exc):
        for p in self._procs:
            p.terminate()
        for p in self._procs:
            try:
                p.wait(timeout=5)
            except Exception:
                p.kill()
        if self._router is not None:
            try:
                out, _ = self._router.communicate(timeout=5)
                open(os.path.join(self.home, "router.log"), "w").write(out or "")
            except Exception:
                pass
        # Remove the isolated HOME (binary + config + logs, ~32 MB per
        # run) now that router.log is written: the config holds
        # encrypted gateway-key material and must not accumulate
        # (~160 MB for the five drivers). router.log goes with it;
        # the check details are already in the driver's stdout.
        shutil.rmtree(self.home, ignore_errors=True)
        return False

    # -- HTTP ------------------------------------------------------
    def req(self, method, path, body=None, key=None, headers=None,
            timeout=30):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method)
        if data:
            r.add_header("Content-Type", "application/json")
        if key:
            r.add_header("Authorization", "Bearer " + key)
        for k, v in (headers or {}).items():
            r.add_header(k, v)
        try:
            with urllib.request.urlopen(r, timeout=timeout) as resp:
                return resp.status, dict(resp.headers), resp.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), e.read().decode()
        except Exception as e:
            return 0, {}, str(e)

    # -- mock observations ------------------------------------------
    def hits(self):
        try:
            with open(self._hits_file) as fh:
                return int(fh.read().strip() or 0)
        except Exception:
            return 0

    def last_upstream_request(self):
        """Last JSON body the mock upstream received (what the router
        forwarded after translation), or {} if none yet."""
        try:
            return json.load(open(self._last_file))
        except Exception:
            return {}

    def stop_mock(self):
        """Kill the mock upstream (negative-check setup)."""
        if self._mock is not None:
            self._mock.terminate()
            try:
                self._mock.wait(timeout=5)
            except Exception:
                self._mock.kill()
            self._mock = None

    # -- router observations ----------------------------------------
    def trace(self):
        """Parsed GET /dashboard/trace payload ({} on failure)."""
        st, _, body = self.req("GET", "/dashboard/trace")
        if st != 200:
            return {}
        try:
            return json.loads(body)
        except Exception:
            return {}

    def trace_recorded(self, model):
        """True when the trace feed shows a successful attempt for
        the given model on the mock provider — i.e. the router
        recorded the attempt (its ledger/trace), not just proxied
        bytes. Returns (ok, detail)."""
        tr = self.trace()
        attempts = tr.get("attempts") or []
        # NOTE: the trace records the provider as its internal id
        # (prov_...) rather than the display name ("mock"), so the
        # match is on model + outcome only; the provider id is
        # reported in the detail string.
        good = [a for a in attempts
                if a.get("model") == model
                and a.get("outcome") == "success"]
        if good:
            return True, (f"attempts={len(attempts)} provider="
                          f"{good[0].get('provider')} "
                          f"endpoint={tr.get('endpoint') or 'chat'}")
        return False, f"attempts={attempts} endpoint={tr.get('endpoint')}"
