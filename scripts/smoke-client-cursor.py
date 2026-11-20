#!/usr/bin/env python3
"""Client smoke: Cursor through Codvian Router (M8.1).

Scripted protocol smoke ONLY — no real Cursor editor is invoked.
It sends the requests Cursor sends when pointed at a custom
OpenAI-compatible endpoint and asserts the router answers them.

VERIFIED client shape (sources checked 2026-11-19):
  * Cursor exposes "Override OpenAI Base URL" under Settings ->
    Models; the base URL ends in /v1 and Cursor appends
    /chat/completions (and /v1/models for verification) itself,
    sending `Authorization: Bearer <key>` with the same JSON body
    OpenAI would receive.
    - https://qoraapi.com/blog/connect-cursor-cline-continue-custom-api-endpoint/
    - https://codecraftapi.com/blog/cursor-custom-api-setup
    - https://nbility.ai/blog/en/cursor-custom-openai-api
  * Cursor Chat/Agent streams SSE and, in Agent mode, needs tool
    calls as well as streaming — a plain non-streaming text reply
    proves only basic compatibility.
    - https://nbility.ai/blog/en/cursor-custom-openai-api
    - https://aitokensai.com/guides/cursor-anthropic-base-url/
  * Cursor's own docs: custom keys work only with chat models and
    Tab completion stays on Cursor's built-in models — so this
    smoke covers chat only, never Tab completion.
    - https://nbility.ai/blog/en/cursor-custom-openai-api

UNVERIFIED (marked, not presented as real behaviour):
  * Cursor's exact Agent-mode tool schemas and any Cursor-specific
    headers are not asserted here; the tool-call variant is
    exercised by the Cline smoke instead. This script covers
    Cursor's verified core: /v1/models verification, streaming
    chat, and multi-turn history.
  * The model id is the mock "mock-model", not a real model id.

Checks:
  U0 harness ready (real binary builds, router ready, provider added)
  U1 GET /v1/models (Cursor's verify call) -> 200, lists mock-model
  U2 streaming chat (system + user) -> 200 SSE, delta text verbatim,
     ends [DONE]
  U3 trace recorded: mock attempt success for mock-model
  U4 multi-turn (assistant reply in history, fresh prompt) -> 200,
     content verbatim
  U5 mock hit exactly twice (stream + multi-turn), upstream saw the
     assistant history message
  U6 response carries X-Codvian-Request-Id
  U7 unauthenticated chat -> 401
  U8 NEGATIVE: mock stopped -> fresh request fails fast (non-200),
     never hangs and never reports success
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
SYSTEM = "You are Cursor, an AI coding assistant in the editor."
PROMPT_STREAM = "cursor smoke probe stream: say hi"
PROMPT_MULTI = "cursor smoke probe multi-turn: say hi again"
PROMPT_NEG = "cursor smoke probe negative: say hi"


def chat_body(messages, stream=False):
    return {"model": MODEL, "messages": messages, "stream": stream}


def main():
    c = Checklist()
    try:
        h = ClientHarness("cursor", router_port=21621)
    except Exception as e:
        print(f"SETUP FAILED: {e}")
        return 1
    with h:
        c.check("U0 harness ready (binary builds, router ready, provider added)",
                h.key is not None and h.hits() == 0)

        st, _, body = h.req("GET", "/v1/models", key=h.key)
        try:
            models = [m.get("id") for m in json.loads(body).get("data", [])]
        except Exception:
            models = []
        c.check("U1 GET /v1/models (Cursor's verify call) -> 200, "
                "lists mock-model",
                st == 200 and MODEL in models,
                f"status={st} models={models}")

        st, rh, body = h.req(
            "POST", "/v1/chat/completions",
            chat_body([{"role": "system", "content": SYSTEM},
                       {"role": "user", "content": PROMPT_STREAM}],
                      stream=True),
            key=h.key)
        text = ""
        for line in body.splitlines():
            if line.startswith("data: ") and line != "data: [DONE]":
                try:
                    d = json.loads(line[6:])
                    text += (d.get("choices") or [{}])[0].get("delta", {}).get("content", "")
                except Exception:
                    pass
        c.check("U2 streaming chat (system + user) -> 200 SSE, delta text "
                "verbatim, ends [DONE]",
                st == 200 and text == "smoke says hi" and "[DONE]" in body,
                f"status={st} text={text!r}")

        ok, detail = h.trace_recorded(MODEL)
        c.check("U3 trace recorded: mock attempt success for mock-model",
                ok, detail)

        st, _, body = h.req(
            "POST", "/v1/chat/completions",
            chat_body([{"role": "system", "content": SYSTEM},
                       {"role": "user", "content": PROMPT_STREAM},
                       {"role": "assistant", "content": "smoke says hi"},
                       {"role": "user", "content": PROMPT_MULTI}]),
            key=h.key)
        try:
            obj = json.loads(body)
        except Exception:
            obj = {}
        content = (obj.get("choices") or [{}])[0].get("message", {}).get("content")
        c.check("U4 multi-turn (assistant reply in history, fresh prompt) "
                "-> 200, content verbatim",
                st == 200 and content == "smoke says hi",
                f"status={st} {body[:160]}")

        last = h.last_upstream_request()
        hist = [m.get("role") for m in last.get("messages") or []]
        c.check("U5 mock hit exactly twice (stream + multi-turn), upstream "
                "saw the assistant history message",
                h.hits() == 2 and hist == ["system", "user", "assistant", "user"],
                f"hits={h.hits()} upstream_roles={hist}")

        rid = {k.lower(): v for k, v in rh.items()}.get("x-codvian-request-id")
        c.check("U6 streaming response carried X-Codvian-Request-Id",
                bool(rid), f"request_id={rid}")

        st, _, body = h.req("POST", "/v1/chat/completions",
                            chat_body([{"role": "user", "content": "no key probe"}]))
        c.check("U7 unauthenticated chat -> 401", st == 401, f"status={st}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        h.stop_mock()
        t0 = time.time()
        st, _, body = h.req("POST", "/v1/chat/completions",
                            chat_body([{"role": "user", "content": PROMPT_NEG}]),
                            key=h.key, timeout=30)
        elapsed = time.time() - t0
        c.check("U8 NEGATIVE: mock stopped -> fresh request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
