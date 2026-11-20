# POST /v1/responses — Responses API (CHECKLIST 169–170)

The gateway implements the OpenAI Responses API surface as a **translation
over the canonical chat engine** — not as a passthrough to an upstream
Responses endpoint. A Responses request is decoded, converted into a
`provider.ChatRequest`, routed through the same engine as
`/v1/chat/completions` (failover, budgets, guardrails, ledger, trace),
and the `ChatResponse` is converted back into a Responses object.
Because the upstream never sees a Responses request, Responses
conversations work on **any** provider — including native-wire
Anthropic, Gemini, Cohere and Bedrock targets — not only
OpenAI-compatible ones.

## Routes (Tier 3 MANAGEMENT, gateway-key auth)

| Route | Purpose |
| --- | --- |
| `POST /v1/responses` | Create a response (streaming when `stream: true`) |
| `GET /v1/responses/{id}` | Retrieve a stored response by id — plus `child_response_ids` (the ids that branched off it, from the ledger tree) |
| `GET /v1/responses` | List stored responses, newest first, paginated (`limit`, default 20, clamped to 100; `after` cursor) — the full stored objects |
| `DELETE /v1/responses/{id}` | Delete a stored response (transcript + served object); continuations and retrieves of it then answer 404; deleting an unknown id is an honest 404 |
| `GET /dashboard/responses` | Feed for the dashboard (auth-exempt): ids, model, status, `previous_response_id`, provider, stream flag, created/continuation counts — never input or output text |
| `GET /dashboard/responses/tree` | Tree for the dashboard (auth-exempt, CHECKLIST 238): roots with branches nested (parent → children), from the ledger tree (or the in-memory store when no ledger is wired) — ids and counts only; an empty forest is `roots: []`, `total: 0` |

All routes are in the guard table, so they appear in the generated
OpenAPI spec automatically.

## List + delete (CHECKLIST 204)

`GET /v1/responses` answers the OpenAI list shape:

```json
{
  "object": "list",
  "data": [ { "...": "the full stored response object, as served" } ],
  "has_more": true,
  "first_id": "resp_…",
  "last_id": "resp_…"
}
```

- Newest first. `limit` defaults to 20 and clamps to 100; `limit`
  below 1 or non-numeric is a 400. `after=<id>` continues after
  that response in newest-first order; an unknown cursor is a 400
  naming it, never a silent restart from the top. Only `order=desc`
  is served; anything else is a 400.
- `DELETE /v1/responses/{id}` answers `{"id": "resp_…", "object":
  "response", "deleted": true}`. It deletes the stored transcript,
  so a later create naming the id as `previous_response_id` answers
  404 (`response_not_found`) instead of resuming a conversation
  that no longer exists, and a later retrieve answers 404 too.
  Deleting an id that never existed (or already expired, evicted or
  deleted) is an honest 404, never a silent success.
- The dashboard Logs view lists the recent responses with a Delete
  button per row (gateway key typed fresh in the card, never stored).

## Request (supported subset)

```json
{
  "model": "gpt-4o",
  "input": "Write a haiku about rain.",
  "instructions": "Answer in exactly three lines.",
  "previous_response_id": "resp_…",
  "stream": false,
  "max_output_tokens": 512,
  "temperature": 0.7,
  "top_p": 1,
  "tools": [{ "type": "function", "name": "get_weather",
              "description": "…", "parameters": { "type": "object", "properties": {} } }],
  "tool_choice": "auto",
  "metadata": { "customer": "acme" },
  "routing": { "only": ["provider-a"] }
}
```

- `model` — required. `input` — required: a plain string, or an array
  of message items (`role` + `content`, where content is a string or
  an array of `input_text` / `output_text` / `text` parts), `function_call`
  items (`call_id`, `name`, `arguments`) and `function_call_output`
  items (`call_id`, `output`). Any other item type is a 400.
- `instructions` — a per-turn system message. It is sent to the model
  for this turn only and is **not** stored into the conversation
  transcript, so a later continuation does not re-apply it.
- `previous_response_id` — resumes the stored conversation: the
  stored transcript is replayed as the message history, the new
  input is appended, and the request is pinned to the provider that
  served the original response. Unknown id → 404. A stored response
  whose previous turn failed is still resumable (the failed turn is
  not in the transcript).
- `stream` — `true` selects the SSE event stream (below).
- `max_output_tokens`, `temperature`, `top_p` — passed through to the
  engine like chat completions.
- `tools` — function tools only; any other tool `type` is a 400.
  `tool_choice` accepts `"auto"` / `"none"` / `"required"` or
  `{"type": "function", "name": "<name>"}` (translated to the chat
  shape). A named tool that does not exist in `tools` is a 400.
- `metadata` — `map[string]string`, echoed verbatim on the response
  object.
- `routing` — the same per-request routing contract as chat
  completions (items 157–158): strict decode, unknown keys are a 400
  naming the key.
- Unknown top-level fields are ignored (lenient decode, like chat
  completions), except unknown keys inside `routing`.

**Not honoured — ignored, never faked as applied:** `reasoning`,
`text.format` (structured output), `parallel_tool_calls`. The gateway
does not claim to apply them. There is no MCP loop for Responses:
tool calls are returned as `function_call` output items for the
caller to execute and feed back as `function_call_output` items in
the next turn.

## Response object

```json
{
  "id": "resp_0123456789abcdef01234567",
  "object": "response",
  "created_at": 1763438400,
  "status": "completed",
  "model": "gpt-4o",
  "output": [
    { "id": "msg_…", "type": "message", "role": "assistant",
      "status": "completed",
      "content": [{ "type": "output_text", "text": "…", "annotations": [] }] },
    { "id": "fc_…", "type": "function_call", "status": "completed",
      "call_id": "call_…", "name": "get_weather", "arguments": "{…}" }
  ],
  "usage": { "input_tokens": 12, "output_tokens": 30, "total_tokens": 42,
             "input_tokens_details": { "cached_tokens": 4 },
             "output_tokens_details": { "reasoning_tokens": 0 } },
  "previous_response_id": "resp_…",
  "metadata": { "customer": "acme" }
}
```

- `status` — `completed` (finish `stop`), `incomplete`
  (`max_output_tokens` reached → `incomplete_details: {reason:
  "max_output_tokens"}`, content filter → `reason:
  "content_filter"`), `failed` (upstream error / failed stream),
  `in_progress` (the placeholder stored while a stream is running).
- `usage` — provider-reported tokens only. `cached_tokens` and
  `reasoning_tokens` appear only when the upstream reported them
  (> 0); nothing is estimated or invented.
- `previous_response_id` — `null` for a fresh create.
- The response id is gateway-minted (`resp_` + 24 hex chars);
  message items are `msg_…`, function calls `fc_…`. No upstream id
  is reused, because the upstream never sees a Responses request.

## Streaming events (SSE, `stream: true`)

In order: `response.created` (id + `in_progress` object),
`response.output_item.added`, `response.output_text.delta` (per text
chunk) / `response.function_call_arguments.delta` (per tool-call
chunk), `response.output_text.done` / `response.function_call_arguments.done`,
`response.output_item.done`, `response.completed` (final object),
then `data: [DONE]`. A failure **before** any content is a plain JSON
error; a failure **after** content is an SSE error event + `[DONE]`.
After the first chunk the stream never switches provider — failover
happens before commit only.

## Semantics that matter

- **Fresh creates fail over like chat.** A fresh `POST /v1/responses`
  is replay-safe pre-commit (the upstream has seen nothing yet), so
  a 429 / 5xx / transport error on the first target moves to the
  next target, exactly as chat completions do.
- **Continuations fail over like chat.** A create naming
  `previous_response_id` is replay-safe pre-commit: the gateway
  holds the conversation transcript itself and re-sends it as
  plain chat messages, so no upstream-held state exists for a
  replay to double-apply. The engine tries the original provider
  first (the pin, from the route trace of the parent), and a 429 /
  5xx / transport error there replays the same branched input
  (parent transcript + new turn) against the next target — exactly
  as chat completions do. (CHECKLIST 169 used to exempt
  continuations from replay; the 237–238 slice removed that
  exemption once the gateway-held transcript made it safe.)
- **Branching is a tree, not a chain.** Two creates naming the
  same `previous_response_id` are two branches of that parent:
  each mints its own id and its own transcript copy, and both keep
  the parent's history. The tree (parent → children) is kept in
  the ledger (`response_tree` table — ids, model, provider,
  status, created_at, stream flag; never input or output text),
  with the table created on first open of an older database (no
  backfill for pre-tree responses). `GET /v1/responses/{id}`
  reports `child_response_ids`; `GET /dashboard/responses/tree`
  serves the whole forest for the dashboard tree view. Deleting a
  response removes its tree node; its children keep naming it as
  parent and render as roots flagged parent-missing — never
  silently re-parented.
- **No cache, no coalescing, no stale fallback.** Responses creates
  are excluded from the gateway response cache, from single-flight
  coalescing, and from the stale fallback. Two concurrent identical
  creates mint two different response ids (verified by test); a
  cached chat completion must never be served as a Responses
  object.
- **Budgets and guardrails** apply exactly as on chat completions
  (request/prompt/key budgets, blocking guardrails before the
  provider call, response guardrails after).
- **Per-session token cap** (CHECKLIST 263–264): the operator
  sets `responses.session_token_cap` in the config file (see
  `internal/config/SCHEMA.md`). A session whose counted tokens
  plus the next turn's prompt estimate would exceed the cap
  refuses the turn with 429 (`session_token_limit`) before any
  provider is dialled, and the refusal is counted on the
  Responses feed (`session_admission_refused`). 0 (the default,
  or an absent `responses` block) means counting-only: turns
  are counted and reported, never refused. A negative cap is
  rejected at load. The cap is read at startup — changing it
  needs a restart — and the startup line and the dashboard
  Responses card name the cap in force.
- **The ledger and trace** record Responses attempts like chat
  attempts, with the endpoint recorded as `responses`, so the
  dashboard and spend accounting see them.

## Storage — honest limits

Responses are stored in an **in-memory** store
(`internal/responses`): TTL 2 hours, cap 200 records, oldest-first
eviction. Since CHECKLIST 315–316 the store also **persists to
`responses.json` in the data dir** (the same directory as the
ledger's `ledger.db`), rewritten atomically on every mutation —
create, stream terminal store, delete — so the file is always a
complete copy of the store, including evictions and deletes. A
gateway restart restores the live records from the file, so a
later continuation resumes the restored transcript instead of
answering 404, and a retrieve returns the same object the create
served. Records older than the TTL, or over the cap, are not
restored — a restart never resurrects a response the live store
would already have dropped — and the dashboard feed reports how
many records the restore loaded (`restored`). A corrupt file is
logged and the gateway starts with an empty store rather than
failing the boot. The store keeps the full transcript plus the
served object; the dashboard feed strips both, so feeds never
carry content. The **tree index** is the durable parent/child
index alongside it: the `response_tree` table in the ledger
records each response's parent link, so the tree still shows
nodes after a restart — and now the transcripts behind those
nodes survive too.

Two honest corners. A stream interrupted by a restart leaves an
`in_progress` record whose transcript is empty (the stream
never reached its terminal store): the record is restored and
retrievable, but a continuation of it proceeds with only the
new input — the pre-restart turns are gone, because they were
never stored. And responses created before this slice (whose
transcripts were never written to the file) still answer 404
after a restart: there is no backfill, only what was recorded
since.

## What this is not

- Not an upstream Responses passthrough: the gateway never sends a
  Responses-shaped request to any provider, so provider-native
  Responses features (e.g. OpenAI's server-side tools, `reasoning`
  effort, structured outputs) are not available through this
  endpoint. What the endpoint promises is the Responses *shape* on
  top of the router's chat engine, with the failover, budgets and
  ledger that shape normally does not get.
- Not a conversation API beyond one tree: `previous_response_id`
  resumes the transcript of that response, and branches of one
  parent form a tree (`GET /dashboard/responses/tree`,
  `child_response_ids` on retrieve) — but there is no server-side
  summarisation, no pruning of old branches, and no cross-tree
  search; the tree is the parent/children graph only.
  Listing (`GET /v1/responses`, newest first, `limit` + `after`
  cursor) and delete (`DELETE /v1/responses/{id}`, honest 404 for
  unknown ids) exist since CHECKLIST 204; deleting a response makes
  its continuations 404.
