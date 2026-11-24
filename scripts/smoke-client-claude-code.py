#!/usr/bin/env python3
"""Client smoke: Claude Code through Codvian Router (M8.1).

Scripted protocol smoke ONLY — no real `claude` CLI is invoked.
It sends POST /v1/messages exactly the way Claude Code does and
asserts the router answers in the Anthropic Messages shape.

VERIFIED client shape (sources checked 2026-11-19):
  * Claude Code speaks the Anthropic Messages API only, POST
    /v1/messages; ANTHROPIC_BASE_URL is the server root WITHOUT
    /v1 (the client appends /v1/messages itself).
    - https://aitokensai.com/guides/claude-code-custom-api/
    - https://dev.to/midrelay/migrating-claude-code-to-a-custom-backend-in-2-lines-and-what-to-actually-watch-for-1e0g
    - https://www.codeilab.com/docs/agents/claude-code
  * The Anthropic SDK sends `anthropic-version: 2023-06-01` and
    `x-api-key` headers and streams Anthropic SSE events
    (message_start, content_block_delta, message_stop).
    - https://dev.to/midrelay/migrating-claude-code-to-a-custom-backend-in-2-lines-and-what-to-actually-watch-for-1e0g
  * Body shape: {"model", "max_tokens", "messages": [{"role":
    "user", "content": "..."}]} with an optional top-level
    "system" string — per the Anthropic Messages API reference
    as used in the curl examples at the sources above.

P1.3 AUTH CHANGE: Codvian now accepts the gateway key via
`x-api-key` (Anthropic-style), `x-goog-api-key` (Gemini SDKs),
or `Authorization: Bearer`. All three authenticate identically.
TestP13GatewayKeyXApiKeyAuthenticates pins this behaviour.

UNVERIFIED (marked, not presented as real behaviour):
  * The exact extra headers a live Claude Code session sends
    (anthropic-beta, x-stainless-*) are not asserted here.
  * The model id is the mock "mock-model", not a real Claude id.

Checks:
  K0 harness ready (real binary builds, router ready, provider added)
  K1 non-streaming /v1/messages -> 200
  K2 x-api-key alone (no Bearer) -> 200 (P1.3 vendor-header auth)
  K3 Anthropic response shape: type message, text content verbatim,
     stop_reason end_turn, usage present
  K4 trace recorded: endpoint "messages", mock attempt success
  K5 mock hit exactly twice (K1 + K2), upstream saw model mock-model
  K6 streaming /v1/messages -> Anthropic SSE (message_start,
     content_block_delta, message_stop)
  K7 response carries X-Codvian-Request-Id
  K8 unauthenticated -> 401 (robust: retry on transport errors)
  K9 NEGATIVE: mock stopped -> fresh request fails fast (non-200),
     never hangs and never reports success (unique prompt)
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
# Distinct prompts per request: the router's exact-match cache must
# never serve a stale hit in place of a live upstream call.
PROMPT_1 = "claude-code smoke probe one: say hi"
PROMPT_STREAM = "claude-code smoke probe stream: say hi"
# Unique negative prompt with ZERO word overlap with any prior prompt
# to avoid semantic-cache (token-overlap threshold 0.85) false hits.
PROMPT_NEG = "NEGATIVE CHECK: UNIQUE PROBE XYZ-98765 NO OVERLAP"


def messages_body(prompt, stream=False):
    return {"model": MODEL, "max_tokens": 64,
            "system": "You are a coding assistant. Answer briefly.",
            "messages": [{"role": "user", "content": prompt}],
            "stream": stream}


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
    try:
        h = ClientHarness("claude-code", router_port=21701)
    except Exception as e:
        print(f"SETUP FAILED: {e}")
        return 1
    with h:
        c.check("K0 harness ready (binary builds, router ready, provider added)",
                h.key is not None and h.hits() == 0)
        hdrs = {"anthropic-version": "2023-06-01",
                "x-api-key": h.key}

        st, rh, body = h.req("POST", "/v1/messages",
                             messages_body(PROMPT_1),
                             key=h.key, headers=hdrs)
        try:
            obj = json.loads(body)
        except Exception:
            obj = {}
        c.check("K1 non-streaming /v1/messages -> 200", st == 200,
                f"status={st} {body[:160]}")

        # P1.3: x-api-key alone NOW authenticates (vendor-header auth).
        st2, _, body2 = h.req("POST", "/v1/messages",
                              messages_body("auth probe"),
                              headers={"anthropic-version": "2023-06-01",
                                       "x-api-key": h.key})
        c.check("K2 x-api-key alone (no Bearer) -> 200 (P1.3 vendor-header auth)",
                st2 == 200, f"status={st2} {body2[:120]}")

        content = ""
        try:
            blocks = obj.get("content") or []
            content = "".join(b.get("text", "") for b in blocks
                              if b.get("type") == "text")
        except Exception:
            pass
        c.check("K3 Anthropic shape: type message, text verbatim, "
                "stop_reason end_turn, usage present",
                obj.get("type") == "message"
                and content == "smoke says hi"
                and obj.get("stop_reason") == "end_turn"
                and isinstance(obj.get("usage"), dict)
                and "input_tokens" in obj["usage"],
                f"{body[:240]}")

        ok, detail = h.trace_recorded(MODEL)
        tr = h.trace()
        c.check("K4 trace recorded: endpoint messages, mock attempt success",
                ok and tr.get("endpoint") == "messages",
                f"{detail} trace_endpoint={tr.get('endpoint')}")

        last = h.last_upstream_request()
        c.check("K5 mock hit exactly twice (K1 + K2), upstream saw model mock-model",
                h.hits() == 2 and last.get("model") == MODEL,
                f"hits={h.hits()} upstream_model={last.get('model')}")

        st, rh, body = h.req("POST", "/v1/messages",
                             messages_body(PROMPT_STREAM, stream=True),
                             key=h.key, headers=hdrs)
        c.check("K6 streaming /v1/messages -> Anthropic SSE "
                "(message_start, content_block_delta, message_stop)",
                st == 200 and "message_start" in body
                and "content_block_delta" in body
                and "message_stop" in body and "smoke" in body,
                f"status={st} {body[:240]}")

        st, rh, body = h.req("POST", "/v1/messages",
                             messages_body("request-id probe"),
                             key=h.key, headers=hdrs)
        rid = {k.lower(): v for k, v in rh.items()}.get("x-codvian-request-id")
        c.check("K7 response carries X-Codvian-Request-Id",
                st == 200 and bool(rid), f"status={st} request_id={rid}")

        # Robust unauthenticated check: retry on transport errors (status=0)
        ok, st, body = req_401_robust(h, "/v1/messages",
                                      messages_body("no key probe"))
        c.check("K8 unauthenticated -> 401 (robust)", ok,
                f"status={st} {body[:120]}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        # Use a unique prompt with zero overlap + cache bypass header to force
        # a genuine upstream call (which will fail since mock is stopped).
        h.stop_mock()
        neg_hdrs = dict(hdrs)
        neg_hdrs["X-Codvian-No-Cache"] = "true"
        t0 = time.time()
        st, _, body = h.req("POST", "/v1/messages",
                            messages_body(PROMPT_NEG),
                            key=h.key, headers=neg_hdrs, timeout=30)
        elapsed = time.time() - t0
        c.check("K9 NEGATIVE: mock stopped -> fresh request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
