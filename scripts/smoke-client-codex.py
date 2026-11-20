#!/usr/bin/env python3
"""Client smoke: OpenAI Codex CLI through Codvian Router (M8.1).

Scripted protocol smoke ONLY — no real `codex` CLI is invoked.
It sends POST /v1/responses exactly the way Codex does and asserts
the router answers in the Responses shape.

VERIFIED client shape (sources checked 2026-11-19):
  * Codex CLI talks to the OpenAI Responses API, POST /v1/responses;
    since 0.122 `wire_api = "responses"` is the only supported value
    for a custom provider ("chat" is rejected at config load), and
    base_url must end in /v1 because Codex appends /responses.
    - https://aitokensai.com/guides/codex-cli-custom-endpoint/
    - https://www.ccsub.net/en/integrations/codex-cli
    - https://docs.byesu.com/en/clients/codex-cli
    - https://qcode.cc/en/codex-cli-third-party-api-setup
  * The Responses API input is an array of input items and the
    response carries an id (resp_*), status, and output_text —
    per the OpenAI Responses API as exercised by the repo's own
    scripts/smoke-responses.py (real binary + mock upstream).
  * Codex streams Responses SSE events (response.created ...
    response.completed + [DONE]) — same event order the repo's
    responses smoke already proves through this router.

UNVERIFIED (marked, not presented as real behaviour):
  * The exact input-item JSON Codex sends (it includes its own
    instruction/tool items) was not captured from a live Codex
    session; the array-of-input_text-items form used here is the
    standard Responses API shape, and the `instructions` field is
    included because Codex is documented to send instructions —
    neither byte is claimed to be Codex's verbatim wire format.
  * The model id is the mock "mock-model", not a real GPT id.

Checks:
  X0 harness ready (real binary builds, router ready, provider added)
  X1 POST /v1/responses (input items + instructions) -> 200
  X2 Responses shape: resp_ id, status completed, output_text verbatim
  X3 trace recorded: mock attempt success for mock-model
  X4 mock hit exactly once, upstream saw model mock-model
  X5 streaming /v1/responses -> response.created..completed + [DONE]
  X6 retrieve GET /v1/responses/<id> -> 200, same id
  X7 response carries X-Codvian-Request-Id
  X8 unauthenticated -> 401
  X9 NEGATIVE: mock stopped -> fresh request fails fast (non-200),
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
PROMPT_1 = "codex smoke probe one: say hi"
PROMPT_STREAM = "codex smoke probe stream: say hi"
PROMPT_NEG = "codex smoke probe negative: say hi"


def responses_body(prompt, stream=False):
    return {"model": MODEL,
            "instructions": "You are a coding agent. Answer briefly.",
            "input": [{"role": "user",
                       "content": [{"type": "input_text", "text": prompt}]}],
            "stream": stream}


def main():
    c = Checklist()
    try:
        h = ClientHarness("codex", router_port=21611)
    except Exception as e:
        print(f"SETUP FAILED: {e}")
        return 1
    with h:
        c.check("X0 harness ready (binary builds, router ready, provider added)",
                h.key is not None and h.hits() == 0)

        st, rh, body = h.req("POST", "/v1/responses",
                             responses_body(PROMPT_1), key=h.key)
        try:
            obj = json.loads(body)
        except Exception:
            obj = {}
        rid = obj.get("id", "")
        c.check("X1 POST /v1/responses (input items + instructions) -> 200",
                st == 200, f"status={st} {body[:160]}")

        text = ""
        for item in obj.get("output") or []:
            for part in item.get("content") or []:
                if part.get("type") == "output_text":
                    text += part.get("text", "")
        if not text:
            text = obj.get("output_text", "")
        c.check("X2 Responses shape: resp_ id, status completed, "
                "output_text verbatim",
                rid.startswith("resp_") and obj.get("status") == "completed"
                and text == "smoke says hi",
                f"id={rid} status={obj.get('status')} text={text!r}")

        ok, detail = h.trace_recorded(MODEL)
        c.check("X3 trace recorded: mock attempt success for mock-model",
                ok, detail)

        last = h.last_upstream_request()
        c.check("X4 mock hit exactly once, upstream saw model mock-model",
                h.hits() == 1 and last.get("model") == MODEL,
                f"hits={h.hits()} upstream_model={last.get('model')}")

        st, _, body = h.req("POST", "/v1/responses",
                            responses_body(PROMPT_STREAM, stream=True),
                            key=h.key)
        evs = [l for l in body.splitlines() if l.startswith("event: ")]
        c.check("X5 streaming /v1/responses -> response.created.."
                "completed + [DONE]",
                st == 200 and evs and evs[0] == "event: response.created"
                and evs[-1] == "event: response.completed"
                and "[DONE]" in body,
                f"status={st} events={evs}")

        st, rh, body = h.req("GET", f"/v1/responses/{rid}", key=h.key)
        try:
            got = json.loads(body).get("id", "")
        except Exception:
            got = ""
        c.check("X6 retrieve GET /v1/responses/<id> -> 200, same id",
                st == 200 and got == rid, f"status={st} id={got}")

        # rh here is the retrieve's own headers (re-read at X7 time),
        # not the X1 create-response headers: every response must
        # carry the id, and a stale capture would silently change
        # what this asserts if the code between X1 and X7 grows.
        rid_hdr = {k.lower(): v for k, v in rh.items()}.get("x-codvian-request-id")
        c.check("X7 retrieve response carried X-Codvian-Request-Id",
                bool(rid_hdr), f"request_id={rid_hdr}")

        st, _, body = h.req("POST", "/v1/responses",
                            responses_body("no key probe"))
        c.check("X8 unauthenticated -> 401", st == 401, f"status={st}")

        # NEGATIVE: upstream down must fail fast, not hang or succeed.
        h.stop_mock()
        t0 = time.time()
        st, _, body = h.req("POST", "/v1/responses",
                            responses_body(PROMPT_NEG),
                            key=h.key, timeout=30)
        elapsed = time.time() - t0
        c.check("X9 NEGATIVE: mock stopped -> fresh request fails fast "
                "(non-200), never hangs or reports success",
                st != 200 and "smoke says hi" not in body and elapsed < 25,
                f"status={st} elapsed={elapsed:.1f}s {body[:120]}")
    return c.finish()


if __name__ == "__main__":
    sys.exit(main())
