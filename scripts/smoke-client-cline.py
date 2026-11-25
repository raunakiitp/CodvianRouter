#!/usr/bin/env python3
"""Client smoke: Cline (VS Code extension) through Codvian Router (M8.1).

Scripted protocol smoke ONLY — no real Cline/VS Code is invoked.
It sends the requests Cline sends on its "OpenAI Compatible"
provider and asserts the router answers them, including a full
tool-call round trip.

VERIFIED client shape (sources checked 2026-11-19):
  * Cline's API Provider "OpenAI Compatible" takes a Base URL
    ending in /v1, an API key, and a Model ID; it appends the
    chat path itself and sends OpenAI chat-completions requests.
    - https://docs.qcode.cc/en/docs/ide/cline
    - https://apimodels.app/cline-api-setup
    - https://docs.nebul.io/docs/integrations/cline
  * Cline is an agentic loop (read -> think -> edit -> verify):
    it needs tool calling, and its tool names include read_file,
    write_file and execute_command; tool use is unchanged when
    pointed at an OpenAI-compatible endpoint.
    - http://blog.juscode.co/cline-custom-endpoint
    - https://enterprisedna.co/resources/guides/guide-cline-tutorial/
  * Cline streams model responses and parses tool calls from them.
    - https://enterprisedna.co/resources/guides/guide-cline-tutorial/

UNVERIFIED (marked, not presented as real behaviour):
  * The exact JSON Schema Cline declares for each tool (its full
    `parameters` object) was not captured from a live Cline
    session; the schema below is a minimal, standard OpenAI
    function shape for read_file. Only the tool NAME (read_file)
    and the round-trip mechanics are asserted as Cline's.
  * The model id is the mock "mock-model", not a real model id.

Checks:
  L0 harness ready (real binary builds, router ready, provider added)
  L1 chat + tools (read_file) -> 200, finish_reason tool_calls
  L2 tool call names the read_file function with a path argument
  L3 upstream saw the tools array in the forwarded request
  L4 trace recorded: mock attempt success for mock-model
  L5 tool-result turn (role tool) -> 200, final text verbatim
  L6 mock hit exactly twice (tool call + tool result), upstream saw
     the tool role message
  L7 response carries X-Codvian-Request-Id
  L8 unauthenticated chat -> 401
  L9 NEGATIVE: mock stopped -> fresh tool request fails fast
     (non-200), never hangs or reports success
"""
import importlib.util
import json
import os
import sys
import time

_spec = importlib.util.spec_from_file_location(
    "smoke_client_lib",
    os.path.join(os.path.dirname(os.path.abspath(__file__)),
                 "smoke-client-lib.py"))
_lib = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_lib)
ClientHarness, Checklist = _lib.ClientHarness, _lib.Checklist

MODEL = "mock-model"
PROMPT_1 = "cline smoke probe one: read main.py and say hi"
# Unique negative prompt with ZERO word overlap with any prior prompt
# to avoid semantic-cache (token-overlap threshold 0.85) false hits.
PROMPT_NEG = "NEGATIVE CHECK: UNIQUE PROBE XYZ-98765 NO OVERLAP"

TOOLS = [{"type": "function",
          "function": {"name": "read_file",
                       "description": "Read the contents of a file.",
                       "parameters": {"type": "object",
                                      "properties": {
                                          "path": {"type": "string",
                                                   "description": "Path to the file."}},
                                      "required": ["path"]}}}]


def req_401_robust(h, path, body, headers=None, retries=5, delay=0.8):
    """Assert 401, retrying on transport errors (status=0)."""
    for i in range(retries):
        st, _, body = h.req("POST", path, body, headers=headers)
        if st == 401:
            return True, st, body
        if st == 0:
            time.sleep(delay)
            continue
        return False, st, body
    return False, st, body


def main():
    c = Checklist()
    h = ClientHarness("cline", router_port=21731)
    with h:
        c.check("L0 harness ready (binary builds, router ready, provider added)",
                h.key is not None and h.hits() == 0)

        st, rh, body = h.req(
            "POST", "/v1/chat/completions",
            {"model": MODEL,
             "messages": [{"role": "user", "content": PROMPT_1}],
             "tools": TOOLS, "tool_choice": "auto"},
            key=h.key)
        try:
            obj = json.loads(body)
        except Exception:
            obj = {}
        ch = (obj.get("choices") or [{}])[0]
        c.check("L1 chat + tools (read_file) -> 200, finish_reason tool_calls",
                st == 200 and ch.get("finish_reason") == "tool_calls",
                f"status={st} {body[:240]}")

        tc = (ch.get("message", {}).get("tool_calls") or [{}])[0]
        fn = tc.get("function", {})
        try:
            args = json.loads(fn.get("arguments") or "{}")
        except Exception:
            args = {}
        c.check("L2 tool call names the read_file function with a path argument",
                fn.get("name") == "read_file" and args.get("path") == "main.py"
                and tc.get("id", "").startswith("call_"),
                f"function={fn.get('name')} args={args}")

        last = h.last_upstream_request()
        upstream_tools = [t.get("function", {}).get("name")
                          for t in last.get("tools") or []]
        c.check("L3 upstream saw the tools array in the forwarded request",
                upstream_tools == ["read_file"],
                f"upstream_tools={upstream_tools}")

        ok, detail = h.trace_recorded(MODEL)
        c.check("L4 trace recorded: mock attempt success for mock-model",
                ok, detail)

        tool_msg = {"role": "assistant", "content": None,
                    "tool_calls": ch.get("message", {}).get("tool_calls")}
        st, _, body = h.req(
            "POST", "/v1/chat/completions",
            {"model": MODEL,
             "messages": [{"role": "user", "content": PROMPT_1},
                          tool_msg,
                          {"role": "tool", "tool_call_id": tc.get("id"),
                           "content": "print('hi')\n"}]},
            key=h.key)
        try:
            obj2 = json.loads(body)
        except Exception:
            obj2 = {}
        content = (obj2.get("choices") or [{}])[0].get("message", {}).get("content")
        c.check("L5 tool-result turn (role tool) -> 200, final text verbatim",
                st == 200 and content == "smoke says hi",
                f"status={st} {body[:160]}")

        last = h.last_upstream_request()
        roles = [m.get("role") for m in last.get("messages") or []]
        c.check("L6 mock hit exactly twice (tool call + tool result), "
                "upstream saw the tool role message",
                h.hits() == 2 and roles == ["user", "assistant", "tool"],
                f"hits={h.hits()} upstream_roles={roles}")

        rid = {k.lower(): v for k, v in rh.items()}.get("x-codvian-request-id")
        c.check("L7 tool-call response carried X-Codvian-Request-Id",
                bool(rid), f"request_id={rid}")

        # Robust unauthenticated check: retry on transport errors (status=0)
        ok, st, body = req_401_robust(h, "/v1/chat/completions",
                                      {"model": MODEL,
                                       "messages": [{"role": "user", "content": "no key probe"}],
                                       "tools": TOOLS})
        c.check("L8 unauthenticated chat -> 401 (robust)", ok,
                f"status={st} {body[:120]}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        # Use a unique prompt with zero overlap + cache bypass header to force
        # a genuine upstream call (which will fail since mock is stopped).
        h.stop_mock()
        time.sleep(1.0)  # allow OS to fully reap mock process
        t0 = time.time()
        st, _, body = h.req(
            "POST", "/v1/chat/completions",
            {"model": MODEL,
             "messages": [{"role": "user", "content": PROMPT_NEG}],
             "tools": TOOLS, "tool_choice": "auto"},
            key=h.key, headers={"X-Codvian-No-Cache": "true"}, timeout=30)
        elapsed = time.time() - t0
        c.check("L9 NEGATIVE: mock stopped -> fresh tool request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
