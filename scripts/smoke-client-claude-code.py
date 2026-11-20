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

AUTH NOTE (honest limitation, verified by check K2 below):
Codvian's gateway auth is `Authorization: Bearer <gateway key>`
only; the Anthropic SDK's `x-api-key` header alone does NOT
authenticate. A real Claude Code pointed at Codvian must therefore
supply the gateway key as ANTHROPIC_AUTH_TOKEN (which the SDK
sends as Bearer), not only as ANTHROPIC_API_KEY. This script sends
both headers, the way a gateway-configured client would.

UNVERIFIED (marked, not presented as real behaviour):
  * The exact extra headers a live Claude Code session sends
    (anthropic-beta, x-stainless-*) are not asserted here.
  * The model id is the mock "mock-model", not a real Claude id.

Checks:
  K0 harness ready (real binary builds, router ready, provider added)
  K1 non-streaming /v1/messages -> 200
  K2 x-api-key alone (no Bearer) -> 401 (auth-shape gap, see above)
  K3 Anthropic response shape: type message, text content verbatim,
     stop_reason end_turn, usage present
  K4 trace recorded: endpoint "messages", mock attempt success
  K5 mock hit exactly once, upstream saw model mock-model
  K6 streaming /v1/messages -> Anthropic SSE (message_start,
     content_block_delta, message_stop)
  K7 response carries X-Codvian-Request-Id
  K8 unauthenticated -> 401
  K9 NEGATIVE: mock stopped -> fresh request fails fast (non-200),
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
# Distinct prompts per request: the router's exact-match cache must
# never serve a stale hit in place of a live upstream call.
PROMPT_1 = "claude-code smoke probe one: say hi"
PROMPT_STREAM = "claude-code smoke probe stream: say hi"
PROMPT_NEG = "claude-code smoke probe negative: say hi"


def messages_body(prompt, stream=False):
    return {"model": MODEL, "max_tokens": 64,
            "system": "You are a coding assistant. Answer briefly.",
            "messages": [{"role": "user", "content": prompt}],
            "stream": stream}


def main():
    c = Checklist()
    try:
        h = ClientHarness("claude-code", router_port=21601)
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

        # Auth-shape gap: x-api-key alone must NOT authenticate.
        st2, _, body2 = h.req("POST", "/v1/messages",
                              messages_body("auth probe"),
                              headers={"anthropic-version": "2023-06-01",
                                       "x-api-key": h.key})
        c.check("K2 x-api-key alone (no Bearer) -> 401",
                st2 == 401, f"status={st2} {body2[:120]}")

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
        c.check("K5 mock hit exactly once, upstream saw model mock-model",
                h.hits() == 1 and last.get("model") == MODEL,
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

        st, _, body = h.req("POST", "/v1/messages",
                            messages_body("no key probe"))
        c.check("K8 unauthenticated -> 401", st == 401, f"status={st}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        h.stop_mock()
        t0 = time.time()
        st, _, body = h.req("POST", "/v1/messages",
                            messages_body(PROMPT_NEG),
                            key=h.key, headers=hdrs, timeout=30)
        elapsed = time.time() - t0
        c.check("K9 NEGATIVE: mock stopped -> fresh request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
