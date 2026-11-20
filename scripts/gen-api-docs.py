#!/usr/bin/env python3
"""M8.21 generator: API docs + try-it page from the guard route table.

Source of truth: internal/server/guard.go (guardRouteTable). Nothing
here hand-writes an endpoint: every route, method, tier, scope,
auth-exempt flag, spawn flag and description is parsed out of the
table, and the generated files carry that data verbatim.

Regenerate after any guard-table change:
    python3 scripts/gen-api-docs.py

Outputs:
    docs/API_REFERENCE.md            one section per guard-table route
    docs/llm.txt                     compact machine-readable summary
    tests/fixtures/v1/m8.21/routes.json   the parsed table as JSON
    web/tryit.html                   standalone try-it page with the
                                     route JSON embedded between the
                                     TRYIT-ROUTES-JSON markers

Request/response shapes: only shapes verified from handler code are
spelled out (see VERIFIED_BODIES / VERIFIED_RESPONSES below, each
traced to a file:line in the doc). Every other route documents its
request as "no body" (GET) or "JSON body — field list lives in the
handler, not re-specified here", and its response as "JSON feed as
described above". No field is ever invented.
"""

import json
import re
import sys
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GUARD = ROOT / "internal" / "server" / "guard.go"

# --- Verified request bodies (handler file:line traced in the doc). ---
# Key = "METHOD path". Value = (json_example, source_note).
VERIFIED_BODIES = {
    "POST /v1/chat/completions": (
        '{\n  "model": "<model-id>",\n  "messages": [{"role": "user", "content": "Hello"}],\n'
        '  "stream": false,\n  "max_tokens": 256,\n  "temperature": 0.7\n}',
        "Fields from internal/provider/client.go chatCompletionRequest "
        "(model, messages, stream, max_tokens, temperature, top_p, "
        "reasoning_effort, stop, tools, tool_choice, prompt_cache_key).",
    ),
    "POST /v1/embeddings": (
        '{\n  "model": "<model-id>",\n  "input": "Hello, world",\n  "dimensions": 1536\n}',
        "Fields from internal/provider/embeddings.go EmbeddingRequest "
        "(model, input, dimensions, encoding_format, user).",
    ),
    "POST /v1/rerank": (
        '{\n  "model": "<model-id>",\n  "query": "What is Codvian Router?",\n'
        '  "documents": ["doc one", "doc two"],\n  "top_n": 1,\n  "return_documents": true\n}',
        "Fields from internal/provider/embeddings.go RerankRequest "
        "(model, query, documents, top_n, return_documents).",
    ),
    "POST /v1/messages": (
        '{\n  "model": "<model-id>",\n  "max_tokens": 256,\n'
        '  "messages": [{"role": "user", "content": "Hello"}]\n}',
        "Anthropic Messages shape, relayed byte-for-byte (only the model "
        "field is rewritten) — see guard-table description.",
    ),
    "POST /v1/responses": (
        '{\n  "model": "<model-id>",\n  "input": "Hello"\n}',
        "OpenAI Responses shape, translated to the canonical chat request "
        "— see guard-table description.",
    ),
    "POST /api/providers": (
        '{\n  "catalog_id": "<catalog-provider-id>",\n  "api_key": "<provider-api-key>",\n'
        '  "base_url": "",\n  "endpoint": "",\n  "variant": "",\n  "name": "",\n  "models": []\n}',
        "Fields from internal/server/providers_add.go addProviderRequest "
        "(catalog_id, api_key, base_url, endpoint, variant, name, models). "
        "Note: models is accepted, validated, then NOT stored — the live "
        "model list comes from the /v1/models probe (see that file).",
    ),
    "POST /admin/settings/export": (
        '{\n  "confirm": "EXPORT SETTINGS"\n}',
        "Typed confirmation phrase from the guard-table description.",
    ),
    "POST /admin/db/export": (
        '{\n  "confirm": "EXPORT"\n}',
        "Typed confirmation phrase from the guard-table description.",
    ),
    "POST /admin/db/wipe/arm": (
        '{\n  "confirm": "WIPE DATABASE"\n}',
        "Typed confirmation phrase from the guard-table description.",
    ),
    "POST /admin/db/wipe": (
        '{\n  "confirm": "WIPE DATABASE",\n  "confirmation_token": "<single-use-token-from-/admin/db/wipe/arm>"\n}',
        "Typed confirmation phrase + single-use token from the "
        "guard-table description; field name confirmation_token verified "
        "in internal/server/dbwipe.go (dbWipeRequest).",
    ),
    "POST /admin/db/import": (
        'multipart/form-data: confirm=IMPORT, plus the export file field',
        "NOT a JSON body: the handler reads the confirm phrase from a "
        "multipart form field (internal/server/dbimport.go) together "
        "with the uploaded export file.",
    ),
    "POST /admin/keys/revoke-all": (
        '{\n  "confirm": "REVOKE ALL"\n}',
        "Typed confirmation phrase from the guard-table description.",
    ),
    "DELETE /admin/keys/{id}": (
        '{\n  "confirm": "DELETE <id>"\n}',
        "Typed confirmation phrase from the guard-table description "
        "(<id> is the key id in the path).",
    ),
    "DELETE /api/providers/{id}": (
        '{\n  "confirm": "DELETE <id>"\n}',
        "Typed confirmation phrase from the guard-table description "
        "(<id> is the provider id in the path).",
    ),
    "POST /admin/config/reset": (
        '{\n  "confirm": "RESET CONFIG"\n}',
        "Typed confirmation phrase from the guard-table description.",
    ),
}

# Verified response examples (same keying). Everything else gets the
# generic "JSON feed as described" note — no invented fields.
VERIFIED_RESPONSES = {
    "GET /healthz": (
        '{"status": "ok"}',
        "Exact body, verified by TestHealthz "
        "(internal/server/server_test.go) and handleHealthz.",
    ),
    "GET /v1/models": (
        '{\n  "object": "list",\n  "data": [\n    {\n      "id": "<model-id>",\n      "object": "model",\n'
        '      "created": 0,\n      "owned_by": "codvian-router"\n    }\n  ]\n}',
        "Shape from internal/server/server.go Model struct "
        "(id, object, created, owned_by, deprecated?, provider_id?). "
        "List each model id once; ?include=deprecated re-lists retired rows.",
    ),
    "GET /openapi.json": (
        '{\n  "openapi": "3.0.3",\n  "info": {"title": "Codvian Router API", "version": "<version>"},\n'
        '  "servers": [{"url": "http://127.0.0.1:<port>"}],\n'
        '  "paths": {"/healthz": {"get": {"summary": "...", "x-loopback-only": true}}}}\n'
        "Note: x-loopback-only / x-always-protected / x-spawn-capable "
        "extensions only — no scopes, no auth-exempt flag, no schemas "
        "(see the /openapi.json gap note at the top of this doc).",
        "Shape from internal/server/openapi.go (openAPISpec).",
    ),
    "GET /api/catalog": (
        '{\n  "providers": [\n    {\n      "id": "<catalog-id>",\n      "name": "<name>",\n'
        '      "verification": "<verification>",\n      "tos_grey": false,\n'
        '      "has_base_url": true,\n      "templated_base_url": false\n    }\n  ],\n  "total": 0\n}',
        "Shape from internal/server/providers_add.go catalogEntryView / "
        "catalogResponse (id, name, alias?, verification, tos_grey, "
        "has_base_url, templated_base_url; providers, total).",
    ),
    "GET /api/usage/budget": (
        '{\n  "api_key_id": "<key-id>",\n  "daily_limit_usd": 0,\n'
        '  "warning_threshold": 0,\n  "reset_interval": "",\n  "reset_time": "",\n'
        '  "total_cost_today": 0,\n  "total_cost_month": 0,\n  "active_limit_usd": 0,\n'
        '  "budget_check": {"status": "allow", "reason": ""},\n'
        '  "limit_usd": 0,\n  "spent_usd": 0,\n  "remaining_usd": 0,\n  "at_limit": false\n}',
        "Shape from internal/server/usageapi.go usageBudgetResponse "
        "(api_key_id, daily/weekly/monthly_limit_usd, warning_threshold, "
        "reset_interval, reset_time, budget_reset_at, next_reset_at, "
        "total_cost_today/month/period, active_limit_usd, budget_check, "
        "limit_usd, spent_usd, remaining_usd, at_limit).",
    ),
}

# Query params named in guard-table descriptions (nothing else claimed).
VERIFIED_QUERY = {
    "GET /api/usage/budget": "`apiKeyId` — the gateway key id to report on.",
    "GET /api/usage/om-usage": "`format=json` — JSON instead of the default plain text.",
    "GET /v1/responses": "`limit` + `after` cursor — newest first, paginated; an unknown cursor is a 400.",
    "GET /dashboard/memstats": "`profile=heap` / `profile=goroutine` — binary pprof snapshot instead of the JSON summary.",
    "GET /v1/models": "`include=deprecated` — re-lists retired models, with `deprecated: true` set.",
}


# GuardTier constant values (guard.go): the table writes the Go
# identifiers (TierLocalOnly, …) but RouteTable() exposes their
# string values (LOCAL_ONLY, …) — the docs/tests compare against
# the values, so map identifiers to values here. An unknown
# identifier fails loudly instead of silently emitting a wrong tier.
TIER_VALUES = {
    "TierLocalOnly": "LOCAL_ONLY",
    "TierAlwaysProtected": "ALWAYS_PROTECTED",
    "TierManagement": "MANAGEMENT",
}


def parse_guard_table():
    """Parse every {Method: ..., Path: ..., Tier: ...} entry in guard.go.

    Each table entry is one source line. Parse per line: the old
    whole-file regex truncated the per-entry tail at the first '}'
    (the closing brace of Scopes: []string{...}), which silently
    dropped every scope, and it kept the Go tier identifiers instead
    of their string values. Both bugs made the generated docs disagree
    with RouteTable() — the coverage tests caught them.
    """
    routes = []
    entry_re = re.compile(
        r'\{Method: "(?P<method>[A-Z]+)", Path: "(?P<path>[^"]+)", '
        r'Tier: (?P<tier>\w+),'
        r'(?: SpawnCapable: (?P<spawn>true|false),)?'
        r' Description: "(?P<desc>(?:[^"\\]|\\.)*)"'
    )
    text = GUARD.read_text()
    for line in text.splitlines():
        if "{Method:" not in line:
            continue
        m = entry_re.search(line)
        if not m:
            raise SystemExit(f"guard-table parse failed on line: {line.strip()[:120]}")
        tier_id = m.group("tier")
        if tier_id not in TIER_VALUES:
            raise SystemExit(f"unknown guard tier identifier {tier_id!r} — update TIER_VALUES")
        scopes = []
        sm = re.search(r'Scopes: \[\]string\{([^}]*)\}', line)
        if sm:
            scopes = re.findall(r'"([^"]+)"', sm.group(1))
        # Descriptions are Go string literals: decode escapes (\" → ")
        # so the doc carries the description verbatim as RouteTable()
        # returns it — the coverage test compares verbatim.
        try:
            desc = json.loads('"' + m.group("desc") + '"')
        except json.JSONDecodeError:
            raise SystemExit(f"description is not a decodable Go string literal: {m.group('desc')[:80]!r}")
        routes.append({
            "method": m.group("method"),
            "path": m.group("path"),
            "tier": TIER_VALUES[tier_id],
            "spawn_capable": m.group("spawn") == "true",
            "description": desc,
            "auth_exempt": "AuthExempt: true" in line,
            "scopes": scopes,
            "bootstrap": "Bootstrap: true" in line,
        })
    if len(routes) != text.count("{Method:"):
        raise SystemExit(
            f"guard-table parse mismatch: parsed {len(routes)} of "
            f"{text.count('{Method:')} entries — fix the regex, do not hand-edit output"
        )
    return routes


def auth_text(r):
    if r["bootstrap"]:
        return "None — fresh-install first key (no key exists yet); loopback-only"
    if r["auth_exempt"]:
        return "None — auth-exempt (Tier 3, explicitly off)"
    if r["tier"] == "LOCAL_ONLY":
        return "Gateway key required, AND loopback-only (checked before auth)"
    if r["tier"] == "ALWAYS_PROTECTED":
        return "Gateway key required — always, even if auth were relaxed for Tier 3"
    return "Gateway key required (unless auth was explicitly relaxed)"


def scope_text(r):
    if not r["scopes"]:
        return "any authenticated key (no scope listed)"
    return ", ".join("`%s`" % s for s in r["scopes"]) + \
        " — scope `admin` implies every scope"


def family(path):
    if path.startswith("/v1/"):
        return "OpenAI-compatible API (`/v1/*`)"
    if path.startswith("/api/"):
        return "Management API (`/api/*`)"
    if path.startswith("/admin/"):
        return "Admin API (`/admin/*`)"
    if path.startswith("/dashboard"):
        return "Dashboard feeds & actions (`/dashboard/*`)"
    return "Health, metrics & meta"


def curl_example(r):
    base = "http://127.0.0.1:8080"  # placeholder; real port is the listen address
    url = base + r["path"]
    auth = "" if (r["auth_exempt"] or r["bootstrap"]) else ' -H "Authorization: Bearer $CODVIAN_KEY"'
    body = ""
    key = f'{r["method"]} {r["path"]}'
    if key in VERIFIED_BODIES:
        try:
            one_line = json.dumps(json.loads(VERIFIED_BODIES[key][0]))
            body = f" -d '{one_line}'"
        except (json.JSONDecodeError, ValueError):
            body = " # body is not JSON — see the section above"
    elif r["method"] in ("POST", "DELETE") and not r["auth_exempt"]:
        body = " -d '{}'"
    return f"curl -X {r['method']}{auth}{body} {url}"


def route_section(r):
    key = f'{r["method"]} {r["path"]}'
    lines = [f"### `{key}`", ""]
    lines.append(f"- **Tier:** `{r['tier']}`")
    lines.append(f"- **Scope(s):** {scope_text(r)}")
    lines.append(f"- **Auth:** {auth_text(r)}")
    lines.append(f"- **Spawn-capable:** {'yes' if r['spawn_capable'] else 'no'}")
    if r["bootstrap"]:
        lines.append("- **Bootstrap:** yes — first-key route, only while no gateway key exists")
    lines.append(f"- **Description:** {r['description']}")
    if key in VERIFIED_QUERY:
        lines.append(f"- **Query:** {VERIFIED_QUERY[key]}")
    # Request
    if r["method"] == "GET":
        lines.append("- **Request:** no body.")
    elif key in VERIFIED_BODIES:
        body, note = VERIFIED_BODIES[key]
        if body.lstrip().startswith("{"):
            lines.append("- **Request body (JSON):**")
            lines.append("")
            lines.append("  ```json")
            lines.append("  " + body.replace("\n", "\n  "))
            lines.append("  ```")
        else:
            lines.append(f"- **Request body:** {body}")
        lines.append("")
        lines.append(f"  Source: {note}")
    else:
        lines.append("- **Request body:** JSON object for the action described above. "
                     "The exact field list lives in the handler and is NOT re-specified "
                     "here — do not send fields guessed from this doc.")
    # Response
    if key in VERIFIED_RESPONSES:
        resp, note = VERIFIED_RESPONSES[key]
        lines.append("- **Response (example):**")
        lines.append("")
        lines.append("  ```json")
        lines.append("  " + resp.replace("\n", "\n  "))
        lines.append("  ```")
        lines.append("")
        lines.append(f"  Source: {note}")
    elif r["path"] == "/metrics":
        lines.append("- **Response:** Prometheus exposition text (not JSON).")
    elif r["path"] == "/dashboard":
        lines.append("- **Response:** the static dashboard HTML page (web/index.html).")
    elif r["path"] == "/api/usage/om-usage":
        lines.append("- **Response:** plain text by default, JSON with `?format=json` "
                     "(spend today / this week / this month, active limit, next reset).")
    else:
        lines.append("- **Response:** JSON feed/object as described above. The exact field "
                     "list lives in the handler and is NOT re-specified here — do not "
                     "assume fields beyond the description.")
    lines.append("")
    lines.append("  ```sh")
    lines.append("  " + curl_example(r))
    lines.append("  ```")
    lines.append("")
    lines.append("  Errors use the OpenAI-shaped envelope "
                 "`{\"error\": {\"message\": \"...\", \"type\": \"invalid_request_error\"}}` "
                 "(see `writeError` in internal/server/server.go); server/upstream "
                 "failures use type `server_error` / `upstream_error`. "
                 "Unauthenticated: 401; authenticated but wrong scope: 403; "
                 "non-loopback to a LOCAL_ONLY route: 403 `LOCAL_ONLY`.")
    lines.append("")
    return "\n".join(lines)


def build_api_reference(routes):
    n = len(routes)
    # General method counter (was: hardcoded GET/POST/DELETE/PATCH sums,
    # which silently dropped PUT — the 1 PUT route never appeared in the
    # header breakdown). Every method present in the table is counted and
    # listed, most-common first (ties alphabetical), so a future method
    # cannot silently drop out either; the parts always sum to n.
    method_counts = Counter(r["method"] for r in routes)
    ordered = sorted(method_counts, key=lambda m: (-method_counts[m], m))
    breakdown = ", ".join(f"{method_counts[m]} {m}" for m in ordered)
    assert sum(method_counts.values()) == n, (
        f"method breakdown sums to {sum(method_counts.values())}, not {n}"
    )
    tiers = {}
    for r in routes:
        tiers[r["tier"]] = tiers.get(r["tier"], 0) + 1
    exempt = sum(1 for r in routes if r["auth_exempt"])
    scoped = sum(1 for r in routes if r["scopes"])
    keyonly = n - exempt - scoped  # gateway key, no scope
    header = f"""# Codvian Router — API Reference

Generated from the guard route table (`internal/server/guard.go`,
`guardRouteTable`) by `scripts/gen-api-docs.py` — do not hand-edit
the per-route sections; regenerate instead:

```sh
python3 scripts/gen-api-docs.py
```

Coverage: **{n} routes** ({breakdown}) —
{tiers.get("LOCAL_ONLY", 0)} `LOCAL_ONLY`, {tiers.get("ALWAYS_PROTECTED", 0)} `ALWAYS_PROTECTED`,
{tiers.get("MANAGEMENT", 0)} `MANAGEMENT`; {exempt} auth-exempt, {scoped} scope-gated, {keyonly} key-only (gateway key, no scope).
Enforced by `TestAPIReferenceCoversEveryRoute`
(`internal/server/api_reference_test.go`): the test reads the guard
table and this doc and FAILS if any route is missing. That test is
the enforcement for the M8 exit gate.

## Auth

Every request except the auth-exempt routes below carries a gateway
key as `Authorization: Bearer <key>`. A key that authenticates but
lacks the route's scope gets 403; scope `admin` implies every scope.
`LOCAL_ONLY` routes are additionally reachable only from loopback
(real TCP peer address, never `Host` / `X-Forwarded-For`) — a valid
key over a non-loopback connection still gets 403. The one exception
is `POST /admin/bootstrap`, the fresh-install first-key route: it
takes no key because no key exists yet, and only answers while no
gateway key exists (a second call is 409).

Destructive routes additionally require a typed confirmation phrase
in the JSON body (`{{"confirm": "<phrase>"}}`) — see the individual
sections. The phrase is part of the request; a key alone cannot
perform the action.

## The `/openapi.json` gap (read before trusting the spec alone)

`GET /openapi.json` is generated live from the same guard table and
its consistency is enforced by `TestOpenAPIConsistency` — every route
is present, no stale operations, and the `x-loopback-only` /
`x-always-protected` / `x-spawn-capable` extensions match the tier.
What it does NOT carry, and this doc exists to supply:

- **Scopes.** The spec carries no structured scope information —
  no scopes field, no security scheme: a client cannot learn from
  `/openapi.json` that `GET /v1/models` needs scope `models` or that
  the dashboard mutating POSTs need `admin`. (A few one-line
  summaries mention the word "scope" in prose — e.g. "(admin scope)"
  — but prose is not a machine-readable scope; the gap is about the
  missing field, and `TestOpenAPIGapIsDocumented` checks for the
  field, not the substring.)
- **Auth-exempt flag.** Nothing in the spec says which routes need
  no key (`/healthz`, `/openapi.json`, the dashboard feeds).
- **Request/response schemas.** Operations carry only a one-line
  summary; there are no schemas for bodies or responses.

So: use `/openapi.json` for the route list and tier extensions, and
use this doc (or `docs/llm.txt`) for scopes, auth, and shapes.

## Error envelope

All errors are JSON with the OpenAI-shaped envelope
(`writeError` / `writeServerError` / `writeUpstreamError` in
`internal/server/server.go`):

```json
{{"error": {{"message": "method not allowed", "type": "invalid_request_error"}}}}
```

`type` is `invalid_request_error` for client errors, `server_error`
for server failures, `upstream_error` for upstream refusals.
`401` unauthenticated, `403` wrong scope or non-loopback on a
`LOCAL_ONLY` route, `404` unknown route/id, `501` honest
not-implemented stub (four Tier 1 routes — see below).

## Try it

`web/tryit.html` is a standalone try-it page served from the same
router (same design system as the dashboard). It embeds the same
route table as JSON and sends real requests to the router it was
served from — never to a live provider. Paste a gateway key into
its key field; the key is kept only in page memory, never stored.

"""
    fams = ["Health, metrics & meta", "OpenAI-compatible API (`/v1/*`)",
            "Management API (`/api/*`)", "Dashboard feeds & actions (`/dashboard/*`)",
            "Admin API (`/admin/*`)"]
    body = [header]
    for fam in fams:
        body.append(f"## {fam}\n")
        for r in routes:
            if family(r["path"]) == fam:
                body.append(route_section(r))
    return "\n".join(body)


def build_llm_txt(routes):
    lines = [
        "# Codvian Router API — llm.txt",
        "# Machine-readable API summary for LLM clients. Generated from",
        "# internal/server/guard.go (guardRouteTable) by",
        "# scripts/gen-api-docs.py — regenerate, do not hand-edit.",
        "# Base URL: the router's listen address (default loopback).",
        "# Auth: Authorization: Bearer <gateway-key>, unless auth-exempt.",
        "# Scope `admin` implies every scope. LOCAL_ONLY = loopback-only",
        "# even with a valid key. Destructive routes need a typed",
        "# {\"confirm\": \"<phrase>\"} body in addition to the key.",
        f"# Routes: {len(routes)}",
        "",
        "## Auth",
        "header: Authorization: Bearer <gateway-key>",
        "unauthenticated: 401; wrong scope: 403; non-loopback LOCAL_ONLY: 403",
        "error envelope: {\"error\": {\"message\": \"...\", \"type\": \"invalid_request_error\"}}",
        "bootstrap: POST /admin/bootstrap issues the first key (loopback-only, no key, 409 once a key exists)",
        "",
        "## Routes (METHOD path | tier | scopes | auth | description)",
    ]
    for r in routes:
        scopes = ",".join(r["scopes"]) if r["scopes"] else "-"
        auth = "exempt" if r["auth_exempt"] else ("bootstrap" if r["bootstrap"] else "key")
        lines.append(f"{r['method']} {r['path']} | {r['tier']} | scopes={scopes} | auth={auth} | {r['description']}")
    lines += [
        "",
        "## Examples (one per family)",
        "health: curl http://127.0.0.1:8080/healthz  # -> {\"status\":\"ok\"}",
        "chat: curl -X POST -H \"Authorization: Bearer $CODVIAN_KEY\" -H \"Content-Type: application/json\" "
        "-d '{\"model\":\"<model-id>\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}' "
        "http://127.0.0.1:8080/v1/chat/completions",
        "models: curl -H \"Authorization: Bearer $CODVIAN_KEY\" http://127.0.0.1:8080/v1/models",
        "providers: curl -H \"Authorization: Bearer $CODVIAN_KEY\" http://127.0.0.1:8080/api/providers",
        "status: curl -H \"Authorization: Bearer $CODVIAN_KEY\" http://127.0.0.1:8080/admin/status",
        "openapi: curl http://127.0.0.1:8080/openapi.json  # route list + tier extensions only; no scopes/schemas",
        "",
        "## Notes",
        "- /openapi.json carries no scopes, no auth-exempt flag, no schemas; this file and docs/API_REFERENCE.md do.",
        "- Four Tier 1 routes are honest 501 stubs: POST /admin/service/install, POST /admin/plugin/load, "
        "POST /admin/update, POST /admin/provider/login.",
        "- Dashboard feeds (GET /dashboard/*) are auth-exempt by design; the mutating dashboard POSTs need scope admin.",
        "- Never invent a model, price, cap, or endpoint not listed here or in GET /v1/models / GET /api/catalog.",
    ]
    return "\n".join(lines) + "\n"


def build_tryit(routes):
    blob = json.dumps(routes, indent=2)
    return TRYIT_TEMPLATE.replace("/*__ROUTES_JSON__*/", blob)


TRYIT_TEMPLATE = r"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover" />
<meta name="color-scheme" content="light dark" />
<title>Codvian Router — API Try-It</title>
<link rel="preconnect" href="https://fonts.googleapis.com" />
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet" />
<style>
  /* Design tokens copied from web/index.html (same design system):
     light #FFFFFF / dark #000000, dark-lime palette, Plus Jakarta Sans /
     JetBrains Mono, pill buttons, 26px card corners. Content is new. */
  :root{
    color-scheme: light;
    --bg:#FFFFFF; --surface:#FFFFFF; --surface-2:#FFFFFF;
    --grid:rgba(0,0,0,.08); --ring:rgba(0,0,0,.10);
    --border:transparent; --text:#000000; --text-2:#4A4A4A; --text-3:#9A9A9A;
    --blue:#3F6212; --blue-bg:#F2F7E3;
    --green:#3F6212; --green-bg:#EFF6DC; --green-dot:#65A30D;
    --amber:#171717; --amber-bg:#FFFFFF; --amber-dot:#171717;
    --red:#8C1D18; --red-bg:#FFFFFF; --red-dot:#8C1D18;
    --btn:#000000; --btn-text:#FFFFFF;
    --shadow:0 1px 2px rgba(0,0,0,.05), 0 10px 24px -12px rgba(0,0,0,.12), 0 24px 44px -24px rgba(0,0,0,.14), inset 0 1px 0 rgba(255,255,255,.9);
    --radius:26px; --radius-sm:15px;
  }
  html[data-theme="dark"]{
    color-scheme: dark;
    --bg:#000000; --surface:#000000; --surface-2:#000000;
    --grid:rgba(255,255,255,.09); --ring:rgba(255,255,255,.14);
    --border:transparent; --text:#FFFFFF; --text-2:#B5B5B5; --text-3:#767676;
    --blue:#BEF264; --blue-bg:#1E2A08;
    --green:#BEF264; --green-bg:#1E2A08; --green-dot:#A3E635;
    --amber:#FFFFFF; --amber-bg:#000000; --amber-dot:#FFFFFF;
    --red:#F0A8A2; --red-bg:#000000; --red-dot:#E0655C;
    --btn:#FFFFFF; --btn-text:#000000;
    --shadow:inset 0 0 0 1px rgba(255,255,255,.09), 0 14px 30px -12px rgba(0,0,0,.75), 0 30px 56px -28px rgba(0,0,0,.85);
  }
  *{box-sizing:border-box}
  html,body{margin:0;padding:0}
  body{font-family:"Plus Jakarta Sans",-apple-system,"Segoe UI",sans-serif;background:var(--bg);color:var(--text);font-size:14px;line-height:1.55;-webkit-font-smoothing:antialiased}
  .mono{font-family:"JetBrains Mono",ui-monospace,monospace}
  button{font-family:inherit;cursor:pointer;min-height:36px}
  input,select,textarea{min-height:36px;font-family:inherit}
  .wrap{max-width:1020px;margin:0 auto;padding:28px 20px 64px}
  header{display:flex;flex-wrap:wrap;gap:12px;align-items:center;justify-content:space-between;margin-bottom:8px}
  h1{font-size:26px;line-height:1.2;margin:0;letter-spacing:-.02em}
  .sub{color:var(--text-2);margin:4px 0 20px;max-width:720px}
  .pill{display:inline-block;border-radius:999px;padding:6px 14px;background:var(--btn);color:var(--btn-text);border:0;font-weight:700;font-size:13px}
  .ghost{background:var(--surface);color:var(--text);box-shadow:inset 0 0 0 1px var(--ring)}
  .card{background:var(--surface);border-radius:var(--radius);box-shadow:var(--shadow);padding:20px;margin:0 0 16px}
  .card h2{font-size:16px;margin:0 0 6px}
  .card p{color:var(--text-2);margin:0 0 12px}
  .row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
  .row label{font-weight:600;font-size:13px}
  input[type=text],input[type=password],select{flex:1;min-width:180px;border-radius:999px;border:0;box-shadow:inset 0 0 0 1px var(--ring);background:var(--surface);color:var(--text);padding:8px 14px}
  textarea{width:100%;border-radius:var(--radius-sm);border:0;box-shadow:inset 0 0 0 1px var(--ring);background:var(--surface);color:var(--text);padding:12px;font-family:"JetBrains Mono",monospace;font-size:12.5px;line-height:1.5;resize:vertical}
  .method{display:inline-block;min-width:64px;text-align:center;border-radius:999px;padding:3px 10px;font-family:"JetBrains Mono",monospace;font-size:12px;font-weight:600}
  .m-GET{background:var(--green-bg);color:var(--green)}
  .m-POST{background:var(--blue-bg);color:var(--blue)}
  .m-DELETE{background:var(--red-bg);color:var(--red)}
  .badge{display:inline-block;border-radius:999px;padding:2px 9px;font-size:11.5px;font-weight:600;box-shadow:inset 0 0 0 1px var(--ring)}
  .path{font-family:"JetBrains Mono",monospace;font-size:13px;word-break:break-all}
  .desc{color:var(--text-2);margin:8px 0 12px}
  .meta{color:var(--text-3);font-size:12.5px;margin:0 0 12px}
  .resp{white-space:pre-wrap;word-break:break-word;background:var(--surface-2);box-shadow:inset 0 0 0 1px var(--ring);border-radius:var(--radius-sm);padding:12px;font-family:"JetBrains Mono",monospace;font-size:12.5px;max-height:420px;overflow:auto;margin-top:12px}
  .status{font-weight:700}
  .warn{background:var(--amber-bg);color:var(--amber);box-shadow:inset 0 0 0 1px var(--ring);border-radius:var(--radius-sm);padding:12px 14px;margin:0 0 16px}
  .filters{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:16px}
  .filters button{border-radius:999px;border:0;padding:8px 14px;background:var(--surface);color:var(--text);box-shadow:inset 0 0 0 1px var(--ring);font-weight:600;font-size:13px}
  .filters button.on{background:var(--btn);color:var(--btn-text);box-shadow:none}
  details{margin-top:12px}
  summary{cursor:pointer;font-weight:600}
  .hint{font-size:12.5px;color:var(--text-3)}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div>
      <h1>Codvian Router — API Try-It</h1>
      <p class="sub">Every route the router serves, generated from its guard
      route table. Requests go to <span class="mono">this router only</span> —
      never to a live provider. Destructive routes need their typed
      confirmation phrase as well as a key.</p>
    </div>
    <button class="pill ghost" id="themeBtn" type="button">Toggle theme</button>
  </header>

  <div class="warn"><strong>Your gateway key stays in this page's memory only.</strong>
  It is never stored (no localStorage, no cookies) and never sent anywhere
  except this router, as an <span class="mono">Authorization: Bearer</span> header.
  Close the tab and it is gone.</div>

  <div class="card">
    <h2>Connection</h2>
    <p>Base URL defaults to the origin this page was served from. Override it
    only to point at another router you operate.</p>
    <div class="row">
      <label for="baseUrl">Base URL</label>
      <input id="baseUrl" type="text" class="mono" value="" placeholder="http://127.0.0.1:8080" />
      <label for="apiKey">Gateway key</label>
      <input id="apiKey" type="password" class="mono" placeholder="paste gateway key (kept in memory only)" autocomplete="off" />
    </div>
    <p class="hint">Auth-exempt routes work with the key field empty. Scoped routes
    answer 403 without the right scope; <span class="mono">LOCAL_ONLY</span> routes
    answer 403 unless you open this page from loopback.</p>
  </div>

  <div class="filters" id="filters">
    <button type="button" data-f="all" class="on">All</button>
    <button type="button" data-f="/v1/">/v1</button>
    <button type="button" data-f="/api/">/api</button>
    <button type="button" data-f="/dashboard">/dashboard</button>
    <button type="button" data-f="/admin">/admin</button>
    <button type="button" data-f="other">other</button>
    <input id="search" type="text" placeholder="Filter routes…" aria-label="Filter routes" />
  </div>
  <p class="hint" id="count"></p>
  <div id="routes"></div>
  <p class="hint">Route list generated from <span class="mono">internal/server/guard.go</span>
  by <span class="mono">scripts/gen-api-docs.py</span>. Full reference:
  <span class="mono">docs/API_REFERENCE.md</span>; machine-readable summary:
  <span class="mono">docs/llm.txt</span>; live spec: <span class="mono">GET /openapi.json</span>
  (route list + tier extensions only — no scopes, no schemas).</p>
</div>

<script>
// Route table embedded at build time by scripts/gen-api-docs.py.
// Regenerate: python3 scripts/gen-api-docs.py
// BEGIN TRYIT-ROUTES-JSON
const ROUTES = /*__ROUTES_JSON__*/;
// END TRYIT-ROUTES-JSON

const $ = (id) => document.getElementById(id);
const baseUrlInput = $("baseUrl"), keyInput = $("apiKey"), routesEl = $("routes"), countEl = $("count");
if (!baseUrlInput.value) baseUrlInput.value = window.location.origin;

function themeInit() {
  const saved = null; // no storage: theme follows the OS by default
  const dark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
}
themeInit();
$("themeBtn").addEventListener("click", () => {
  const cur = document.documentElement.getAttribute("data-theme");
  document.documentElement.setAttribute("data-theme", cur === "dark" ? "light" : "dark");
});

function exampleBody(r) {
  if (r.method === "GET" || r.method === "DELETE" && !r.description.includes("confirmation phrase")) return null;
  if (r.description.includes("EXPORT SETTINGS")) return {confirm: "EXPORT SETTINGS"};
  if (r.path === "/admin/db/wipe/arm") return {confirm: "WIPE DATABASE"};
  if (r.description.includes("WIPE DATABASE")) return {confirm: "WIPE DATABASE", confirmation_token: "<single-use-token-from-/admin/db/wipe/arm>"};
  if (r.description.includes("phrase EXPORT")) return {confirm: "EXPORT"};
  if (r.description.includes("phrase IMPORT")) return {confirm: "IMPORT"};
  if (r.description.includes("REVOKE ALL")) return {confirm: "REVOKE ALL"};
  if (r.description.includes("RESET CONFIG")) return {confirm: "RESET CONFIG"};
  if (r.description.includes("DELETE <id>")) return {confirm: "DELETE <id>"};
  if (r.path === "/v1/chat/completions") return {model: "<model-id>", messages: [{role: "user", content: "Hello"}]};
  if (r.path === "/v1/embeddings") return {model: "<model-id>", input: "Hello, world"};
  if (r.path === "/v1/rerank") return {model: "<model-id>", query: "Hello", documents: ["doc one"], top_n: 1};
  if (r.path === "/v1/messages") return {model: "<model-id>", max_tokens: 256, messages: [{role: "user", content: "Hello"}]};
  if (r.path === "/v1/responses") return {model: "<model-id>", input: "Hello"};
  if (r.path === "/api/providers") return {catalog_id: "<catalog-provider-id>", api_key: "<provider-api-key>"};
  return {};
}

function authText(r) {
  if (r.bootstrap) return "No key (bootstrap only)";
  if (r.auth_exempt) return "No key (auth-exempt)";
  if (r.tier === "LOCAL_ONLY") return "Key + loopback-only";
  if (r.tier === "ALWAYS_PROTECTED") return "Key (always)";
  return "Key (unless relaxed)";
}

let filter = "all", query = "";
function visible(r) {
  const f = filter === "all" ? true
    : filter === "other" ? !["/v1/", "/api/", "/dashboard", "/admin"].some(p => r.path.startsWith(p))
    : r.path.startsWith(filter);
  const q = query.trim().toLowerCase();
  return f && (!q || (r.method + " " + r.path + " " + r.description).toLowerCase().includes(q));
}

function render() {
  const list = ROUTES.filter(visible);
  countEl.textContent = list.length + " of " + ROUTES.length + " routes shown.";
  routesEl.innerHTML = "";
  list.forEach((r, i) => {
    const card = document.createElement("div");
    card.className = "card";
    const params = [...r.path.matchAll(/\{([^}]+)\}/g)].map(m => m[1]);
    const body = exampleBody(r);
    card.innerHTML =
      '<div class="row"><span class="method m-' + r.method + '">' + r.method + '</span>' +
      '<span class="path">' + r.path + '</span></div>' +
      '<p class="desc"></p>' +
      '<p class="meta">Tier ' + r.tier +
      ' &middot; Scopes ' + (r.scopes && r.scopes.length ? r.scopes.join(", ") : "any authenticated key") +
      ' &middot; Auth: ' + authText(r) +
      (r.spawn_capable ? ' &middot; spawn-capable' : '') +
      (r.bootstrap ? ' &middot; bootstrap' : '') + '</p>';
    card.querySelector(".desc").textContent = r.description;
    const form = document.createElement("div");
    params.forEach(p => {
      const lab = document.createElement("label");
      lab.textContent = "{" + p + "}: ";
      const inp = document.createElement("input");
      inp.type = "text"; inp.className = "mono"; inp.placeholder = p;
      inp.dataset.param = p;
      lab.appendChild(inp); form.appendChild(lab);
    });
    if (body !== null) {
      const ta = document.createElement("textarea");
      ta.rows = Math.min(12, JSON.stringify(body, null, 2).split("\n").length + 1);
      ta.value = JSON.stringify(body, null, 2);
      ta.setAttribute("aria-label", "Request JSON body");
      form.appendChild(ta);
    }
    const send = document.createElement("button");
    send.className = "pill"; send.type = "button"; send.textContent = "Send request";
    const out = document.createElement("div");
    send.addEventListener("click", async () => {
      let path = r.path;
      form.querySelectorAll("[data-param]").forEach(inp => {
        path = path.replace("{" + inp.dataset.param + "}", encodeURIComponent(inp.value || "<" + inp.dataset.param + ">"));
      });
      const base = (baseUrlInput.value || window.location.origin).replace(/\/$/, "");
      const headers = {};
      const key = keyInput.value.trim();
      if (key) headers["Authorization"] = "Bearer " + key;
      const opts = {method: r.method, headers};
      const ta = form.querySelector("textarea");
      if (ta && ta.value.trim()) { opts.body = ta.value; headers["Content-Type"] = "application/json"; }
      out.innerHTML = '<div class="resp">Sending…</div>';
      try {
        const res = await fetch(base + path, opts);
        const text = await res.text();
        out.innerHTML = "";
        const st = document.createElement("div");
        st.innerHTML = '<span class="status"></span> <span class="mono"></span>';
        st.querySelector(".status").textContent = res.status + " " + res.statusText;
        st.querySelector(".mono").textContent = res.url;
        const pre = document.createElement("div");
        pre.className = "resp"; pre.textContent = text || "(empty body)";
        out.appendChild(st); out.appendChild(pre);
      } catch (e) {
        out.innerHTML = "";
        const pre = document.createElement("div");
        pre.className = "resp"; pre.textContent = "Request failed: " + e.message +
          "\n(This page only talks to the base URL above. Check the router is running and the base URL is right.)";
        out.appendChild(pre);
      }
    });
    form.appendChild(send);
    card.appendChild(form);
    card.appendChild(out);
    routesEl.appendChild(card);
  });
}

document.querySelectorAll("#filters button").forEach(b => {
  b.addEventListener("click", () => {
    document.querySelectorAll("#filters button").forEach(x => x.classList.remove("on"));
    b.classList.add("on"); filter = b.dataset.f; render();
  });
});
$("search").addEventListener("input", (e) => { query = e.target.value; render(); });
render();
</script>
</body>
</html>
"""


def main():
    routes = parse_guard_table()
    fixtures = ROOT / "tests" / "fixtures" / "v1" / "m8.21"
    fixtures.mkdir(parents=True, exist_ok=True)
    (fixtures / "routes.json").write_text(json.dumps(routes, indent=2) + "\n")
    (ROOT / "docs" / "API_REFERENCE.md").write_text(build_api_reference(routes))
    (ROOT / "docs" / "llm.txt").write_text(build_llm_txt(routes))
    (ROOT / "web" / "tryit.html").write_text(build_tryit(routes))
    print(f"routes: {len(routes)}")
    print("wrote tests/fixtures/v1/m8.21/routes.json")
    print("wrote docs/API_REFERENCE.md")
    print("wrote docs/llm.txt")
    print("wrote web/tryit.html")


if __name__ == "__main__":
    sys.exit(main())
