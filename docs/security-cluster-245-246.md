# Security cluster — CHECKLIST 245–246 (LiteLLM v1.111.0-rc.1, research-log entry 226)

Four LiteLLM fixes, one rule stated four ways: **client input never
overrides admin/routing config, and a typo never silently leaves
something unguarded.**

## The precedence rule

Precedence, highest first:

1. **Admin deployment config** (`providers[].models[].fixed_params`).
   A pinned value is what the request is sent with, whatever the
   client asked for. The response carries
   `X-Codvian-Deployment-Override: temperature,max_tokens` naming the
   applied fields, so a displaced client value is visible, never
   silent. A pin is the deployment's value, not just a ceiling: it
   applies even when the client sent nothing for that field.
2. **Client body fields**, when nothing is pinned for that field.
3. **Provider defaults**, when neither pins nor the client sets a value.

Pins live on the model because a model entry is this router's
deployment: one provider + one model id is the unit the admin
configures and the client addresses. The first provider in config
order whose entry for the requested model carries pins wins (the
operator controls that order). Virtual models (`auto/*`, `group:*`)
name no single deployment entry, so no pin applies to them.

Example — a deployment that always answers deterministically:

```json
{"id": "groq", "base_url": "https://api.groq.com/openai/v1",
 "models": [{"id": "llama-3.3-70b-versatile",
             "fixed_params": {"temperature": 0.0, "max_tokens": 1024}}]}
```

A client sending `temperature: 0.9` gets `temperature: 0.0` upstream
and `X-Codvian-Deployment-Override: temperature` on the response.
Out-of-range pins (`temperature` outside [0, 2], `top_p` outside
(0, 1], `max_tokens` < 1) fail at config load, naming the field.

Enforcement runs on every translated surface — chat completions,
Responses, translated Messages, embeddings/rerank (Vertex check
only: embeddings carry no sampling fields) — before any provider is
dialled.

## Client-supplied Vertex location/project — rejected unless the admin opts in

A request body carrying `vertex_location`, `vertex_project`,
`vertex_ai_location` or `vertex_ai_project` is rejected with a 400
naming the field, before any provider is dialled:

```
client-supplied vertex_location is not allowed: the Vertex
location/project a request lands in is set by the admin's provider
config, not by the caller (admin opt-in: allow_client_vertex_override)
```

The rejected value is never logged and never echoed back. The
opt-in is `allow_client_vertex_override: true` in the config file
(default `false`). An opted-in field is admitted but still has no
routing effect in this build — no Vertex provider consumes it; the
location/project always come from the admin's provider config.
Absent, `null` or empty-string fields are not overrides: there is
nothing to choose.

## Log redaction matches secret shapes, not just named fields

The redactor (`internal/guardrails`, always on, no off switch)
already masked labelled secrets (`api_key: …`, `sk-…`, `Bearer …`,
`AKIA…`, `ghp_…`, JWTs, …) and PII. This slice extends it to secret
*shapes* — a token-shaped value in an arbitrary field is masked even
though no rule names that field:

| Shape | Pattern label | What matches |
|---|---|---|
| AWS session key | `aws-session-key` | `ASIA` + 16 uppercase/digit chars |
| SendGrid key | `sendgrid-key` | `SG.` + 22 chars + `.` + 43 chars (both segments — a partial mask would leak the rest) |
| DigitalOcean token | `digitalocean-token` | `dop_v1_` + 64 hex chars |
| Generic token | `generic-token` | A run of ≥ 32 `[A-Za-z0-9_-]` chars mixing all three classes: a digit, a lowercase and an uppercase letter |

The generic-token rule is deliberately shaped, not a length check:
a 40-hex commit SHA and a 32-hex UUID have digits and lowercase only
(no uppercase) and are left alone; a long English word has no digit;
a URL path never forms one run (slashes and dots break runs). A run
that fails the mix is left untouched, never partially masked. The
rule runs after the named secret rules, so a labelled secret is
already its placeholder and is never re-matched. It is gated on a
single-pass shape scan, so a clean log line pays no extra scan.

What is NOT redacted: ordinary prose, hex SHAs, UUIDs, long
lowercase words, URL paths. What IS redacted: any token-shaped value
wherever it appears — a pasted key in `notes`, a token in an unknown
JSON key, a secret in free prose.

## Guardrail names are validated at every write that accepts one

The registered guardrail names are the three built-in kinds (`pii`,
`secret`, `injection`) plus the configured external guardrails
(`guardrails.external[].name`). Every write path that accepts a
guardrail name validates against that registry:

- **Gateway keys** (`gateway_keys[].guardrails` in the config file,
  `POST /admin/keys/{id}` with a `guardrails` list): an unknown name
  fails loudly — at load, or with a 400 naming the name at update
  time — instead of silently leaving the key believing it is guarded
  by a guardrail that does not exist. Empty names and duplicates
  fail the same way. An empty/absent list names no guardrail: the
  key is guarded by the global blocking config exactly like a key
  without the field. Rotation inherits the key's list (rotation
  replaces the credential, never the policy).
- **External guardrail registration** (`guardrails.external[]`,
  config file or `POST /dashboard/guardrails`): duplicate names were
  already rejected by `Config.Validate`; the registry is what key
  writes validate against.

A key naming a guardrail is guaranteed that guardrail runs for its
requests: the built-in kinds are the global blocking stage and the
external guardrails run for every request. Per-key *scoping* (only
the named guardrails run for that key) is not implemented — the list
is a validated declaration, not a filter.

## Dashboard — security refusals with real counts

Both refusals are counted per reason label since process start and
surfaced live:

- **Route guards view** (`GET /dashboard/guard` →
  `status.security_refusals`): "Security refusals since start:
  client Vertex overrides rejected N · writes naming an unknown
  guardrail M".
- **Guardrails view** (`GET /dashboard/guardrails` →
  `security_refusals`): the same two counts next to the policy
  refusals / upstream failures.
- The guardrails recent-events feed also carries one value-free
  event per refusal (reason label only — no rejected value, no
  location, no key id).

Zero is the honest empty state: no refusal has happened yet, not a
broken feed.

## Known limitations (honest, not hidden)

- **Verbatim Anthropic relay** (`POST /v1/messages` forwarded
  verbatim) forwards the client body unchanged by design, so
  deployment pins do not apply there. The Vertex-override rejection
  does apply (it runs on the raw body before the relay decision).
- **Opted-in Vertex fields have no routing effect yet**: there is no
  Vertex provider client in this build, so an admitted override
  changes nothing about where the request lands. The rejection is
  the protection; the opt-in admits the field without effect.
- **Per-key guardrail scoping is not implemented**: a key's
  guardrail list is validated at write time and the named guardrails
  run globally for every request, but the list does not filter which
  guardrails run for that key.
- **Generic-token shape is heuristic**: a token that is all
  lowercase + digits (e.g. a 40-char lowercase hex API key) does
  not mix all three classes and is not masked by the generic rule
  (it is still masked if it matches a named rule). A 32+ char
  mixed-case identifier with a digit in a log line will be masked
  even if it is not a secret — that is the intended fail-closed
  trade: log context is cheaper than a leaked key.
