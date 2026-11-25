#!/usr/bin/env python3
"""Client smoke: Aider through Codvian Router (M8.1).

Scripted protocol smoke ONLY — no real `aider` CLI is invoked.
It sends the requests Aider sends when pointed at an
OpenAI-compatible endpoint and asserts the router answers them.

VERIFIED client shape (sources checked 2026-11-19):
  * Aider is a terminal pair-programming tool that edits the local
    git repo; it works through an OpenAI-compatible base URL set
    via OPENAI_API_BASE + OPENAI_API_KEY, and the model id must be
    prefixed `openai/` so Aider routes through its OpenAI-compatible
    provider path. OPENAI_API_BASE must NOT append /chat/completions
    — Aider adds the chat path itself.
    - https://docs.venice.ai/guides/integrations/aider
  * The OpenAI SDK (which Aider uses via litellm) appends
    /chat/completions to a base URL ending in /v1 and sends
    `Authorization: Bearer <key>`.
    - https://aitokensai.com/guides/openai-sdk-custom-endpoint/
  * Aider's characteristic request is a system message carrying
    repo context (its repo map / coding instructions) plus the
    user's edit request, sent non-streaming as a chat completion.

UNVERIFIED (marked, not presented as real behaviour):
  * The exact system-prompt text Aider sends (its full repo map
    and instruction block) was not captured from a live Aider
    session; the short repo-context system message below is a
    representative stand-in, not Aider's verbatim prompt.
  * Whether a live Aider session streams by default is not
    asserted; this script pins `stream: false`, which is the
    shape that distinguishes Aider here from Cursor's streaming
    chat.
  * The model id is "openai/mock-model": the `openai/` prefix is
    Aider's verified convention, the mock-model id is the test
    stand-in, not a real model.

ROUTER BEHAVIOUR FOUND WHILE WRITING THIS SMOKE (verified
2026-11-19 with the shared mock): Codvian only routes a model id
the provider advertises via GET /v1/models. An unadvertised id —
with or without a slash — fails closed with 502 ("upstream
provider error") BEFORE the upstream is dialled (mock hits stay
0). The shared mock therefore advertises "openai/mock-model"
alongside "mock-model" (see smoke-client-lib.py). A real Aider
pointed at Codvian must use a model id the configured provider
actually advertises; a typo'd or unlisted id will 502, not
fall through to another provider.

Checks:
  A0 harness ready (real binary builds, router ready, provider added)
  A1 chat (system repo-map + user, stream false, model
     openai/mock-model) -> 200, content verbatim
  A2 response usage present with total_tokens 8 (mock usage passes
     through unaltered)
  A3 trace recorded: mock attempt success for model openai/mock-model
  A4 upstream saw model openai/mock-model forwarded verbatim
  A5 mock hit exactly once
  A6 response carries X-Codvian-Request-Id
  A7 unauthenticated chat -> 401
  A8 NEGATIVE: mock stopped -> fresh request fails fast (non-200),
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

# Aider's openai/ provider prefix (verified, see header).
MODEL = "openai/mock-model"
SYSTEM = ("You are an expert coding assistant. Repository files:\n"
          "main.py\nREADME.md\n"
          "Edit the code only as requested and reply briefly.")
PROMPT_1 = "aider smoke probe one: say hi"
# Unique negative prompt with ZERO word overlap with any prior prompt
# to avoid semantic-cache (token-overlap threshold 0.85) false hits.
PROMPT_NEG = "NEGATIVE CHECK: UNIQUE PROBE XYZ-98765 NO OVERLAP"


def req_401_robust(h, path, body, headers=None, retries=3, delay=0.5):
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
    try:
        h = ClientHarness("aider", router_port=21741)
    except Exception as e:
        print(f"SETUP FAILED: {e}")
        return 1
    with h:
        c.check("A0 harness ready (binary builds, router ready, provider added)",
                h.key is not None and h.hits() == 0)

        st, rh, body = h.req(
            "POST", "/v1/chat/completions",
            {"model": MODEL, "stream": False,
             "messages": [{"role": "system", "content": SYSTEM},
                          {"role": "user", "content": PROMPT_1}]},
            key=h.key)
        try:
            obj = json.loads(body)
        except Exception:
            obj = {}
        content = (obj.get("choices") or [{}])[0].get("message", {}).get("content")
        c.check("A1 chat (system repo-map + user, stream false, model "
                "openai/mock-model) -> 200, content verbatim",
                st == 200 and content == "smoke says hi",
                f"status={st} {body[:160]}")

        usage = obj.get("usage") or {}
        c.check("A2 response usage present with total_tokens 8 (mock usage "
                "passes through unaltered)",
                usage.get("total_tokens") == 8
                and usage.get("prompt_tokens") == 5
                and usage.get("completion_tokens") == 3,
                f"usage={usage}")

        ok, detail = h.trace_recorded(MODEL)
        c.check("A3 trace recorded: mock attempt success for model "
                "openai/mock-model",
                ok, detail)

        last = h.last_upstream_request()
        c.check("A4 upstream saw model openai/mock-model forwarded verbatim",
                last.get("model") == MODEL,
                f"upstream_model={last.get('model')}")

        c.check("A5 mock hit exactly once",
                h.hits() == 1, f"hits={h.hits()}")

        rid = {k.lower(): v for k, v in rh.items()}.get("x-codvian-request-id")
        c.check("A6 response carries X-Codvian-Request-Id",
                bool(rid), f"request_id={rid}")

        # Robust unauthenticated check: retry on transport errors (status=0)
        ok, st, body = req_401_robust(h, "/v1/chat/completions",
                                      {"model": MODEL, "stream": False,
                                       "messages": [{"role": "user",
                                                     "content": "no key probe"}]})
        c.check("A7 unauthenticated chat -> 401 (robust)", ok,
                f"status={st} {body[:120]}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        # Use a unique prompt with zero overlap + cache bypass header to force
        # a genuine upstream call (which will fail since mock is stopped).
        h.stop_mock()
        t0 = time.time()
        st, _, body = h.req(
            "POST", "/v1/chat/completions",
            {"model": MODEL, "stream": False,
             "messages": [{"role": "system", "content": SYSTEM},
                          {"role": "user", "content": PROMPT_NEG}]},
            key=h.key, headers={"X-Codvian-No-Cache": "true"}, timeout=30)
        elapsed = time.time() - t0
        c.check("A8 NEGATIVE: mock stopped -> fresh request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
