# Reasoning replay — continuity across failover (CHECKLIST 163–164)

Reasoning models do not think in the open: Anthropic returns signed
`thinking` blocks, Gemini returns `thought` parts with per-part
`thoughtSignature`s, DeepSeek-style OpenAI models return
`reasoning_content`, Bedrock returns `reasoningContent`. A router
that drops that state on a failover silently degrades the next
turn — the replacement model restarts its reasoning from scratch,
or worse, the router replays the reasoning as fresh user content
and the model treats its own thoughts as instructions.

This slice preserves reasoning state across turns, APIs, and
provider switches, and labels every hop honestly in the Live
Routing trace.

## Design

### The state: `internal/reasoning`

A `reasoning.State` is the wire-independent carrier: an ordered
list of `Block`s, each one a kind plus its payload:

| Kind | Meaning | Payload |
|---|---|---|
| `thinking` | Anthropic thinking block | text + cryptographic signature |
| `redacted_thinking` | Anthropic redacted thinking block | encrypted bytes, verbatim |
| `thought` | Plain reasoning text (DeepSeek `reasoning_content`, Gemini thought text) | text, optional signature |
| `thought_signature` | Gemini thought signature | signature, name = function name when attached to a `functionCall` part |
| `bedrock` | Bedrock `reasoningContent` | raw JSON, verbatim |

Two rules govern everything:

1. **Reasoning is never replayed as fresh user content.** It is
   re-attached to the assistant message it came from, in the
   target wire's own format.
2. **A drop is never silent.** Text that survives in another form
   with an opaque signature dropped is recorded in the trace;
   text that cannot be carried fails the hop.

`reasoning.Check(state, wire, model) (Status, detail, err)` is
the single source of truth for translation policy — wire
converters call it before emitting, the router calls it to label
attempts, so the label and the behaviour cannot drift. Statuses:
`none` (no state), `preserved` (verbatim replay),
`translated` (same text, new wire), `dropped` (state carried
minus a piece that cannot travel, with the reason), and
`unrepresentable` (the hop fails honestly — see below).

### Translation matrix

| From \ To | Anthropic | Gemini | OpenAI (replay models) | Bedrock | Cohere |
|---|---|---|---|---|---|
| Anthropic thinking (signed) | preserved | translated to thought part; signature dropped (foreign signatures are rejected) | translated to `reasoning_content` | unrepresentable (no Anthropic signature) | unrepresentable |
| Anthropic redacted | preserved | dropped with reason | dropped with reason | dropped with reason | unrepresentable |
| Plain thought text | unrepresentable (no Anthropic signature) | translated to thought part | translated to `reasoning_content` | unrepresentable (no signature) | unrepresentable |
| Gemini thought_signature | dropped with reason (function call itself preserved) | preserved on its part | dropped with reason | dropped with reason | unrepresentable |
| Bedrock reasoningContent | unrepresentable | translated to thought part | translated to `reasoning_content` | preserved verbatim | unrepresentable |
| Bedrock redacted-only | dropped with reason | dropped with reason | dropped with reason | preserved verbatim | unrepresentable |

Mixed states fail as a whole: any fatal block makes the hop
`unrepresentable` rather than partially translating the rest.
Drops are recorded in the detail string; detail names only block
kinds, wires, and counts — never reasoning or prompt text.

**OpenAI replay models** follow OmniRoute's list
(`docs/routing/REASONING_REPLAY.md`, v3.8.40): deepseek-r1,
deepseek-reasoner, deepseek-chat, deepseek-v4-flash/pro,
(deepseek|zen/deepseek)-v4, kimi-k\d, qwq, qwen.*think,
glm.*think, ^mimo-v\d (case-insensitive). OpenAI's own o1/o3/o4
and gpt-5 are deliberately NOT on it — their reasoning travels
on the Responses API, not chat completions. A non-replay model
asked to carry reasoning text fails the hop honestly instead of
dropping it.

### The store: capture and restore

`reasoning.Store` is in-memory only, TTL 2h, cap 200 entries
(oldest-created eviction) — OmniRoute's defaults. Keys are
SHA-256 digests of the canonical transcript (`caller id` + turns:
role, text, tool calls) — the reasoning state itself is
deliberately NOT part of the key, so capture and injection meet.

- **Capture:** after a response returns, the router stores its
  reasoning under the key of (transcript + response turn).
- **Restore:** before sending a request, the router walks the
  assistant messages; for each one without reasoning attached it
  looks up the key of the transcript up to and including that
  message and re-attaches a clone.
- **Injection is per-assistant-message**, never into user
  messages — so a restored turn cannot leak into a prompt.
- **Restart loses the store** (documented limitation): the next
  turn misses, never wrongly replays.

### Failover semantics

A `reasoning.UnrepresentableError` is a **hop failure, not a
request failure**: it is breaker-neutral (no cooldown, no
demotion), the chain continues to the next target, and only when
every target is unrepresentable does the request fail — with
HTTP 400 naming the wires ("reasoning state cannot be
represented by the selected provider(s)"), never a 502, and
never counted as an upstream failure. The taxonomy maps it to
`reasoning_unrepresentable` with fallback decision.

Streaming capture happens on the server-facing drain path only:
every stream path (live, drained, hedged winner) flows through
the router's stream bridge, and only a stream that reached Done
captures. `ChatStreamWithChain` direct callers get no stream
capture — documented limitation.

Replay-exempt requests (`router.ReplayExemption`, CHECKLIST
133–134) are untouched: no capture, no injection.

### Bedrock: the item-52 gap is closed

CHECKLIST 52 recorded that Bedrock dropped `reasoningContent`
by design. That was true of the old implementation; the Converse
API supports `reasoningContent` in requests — `text` plus
`signature`, and the docs require the signature and all previous
messages to be included in subsequent requests, unchanged. This
slice replays Bedrock blocks verbatim (text, signature, and
redacted content) in requests, captures them from responses and
from `ConverseStream` reasoning deltas, and translates Bedrock
text to other wires where the matrix allows. The item-52 note
stands as history; the behaviour no longer drops it.

### Anthropic signatures

An Anthropic thinking block is only replayable with its original
cryptographic signature, unchanged. A signature from another
wire is never attached to a Gemini thought part — Gemini would
reject a foreign signature — and a Gemini signature is never
attached to an Anthropic block. The wire converters enforce
this: only Gemini-origin signatures attach to Gemini parts.

### The dashboard

- **Live Routing trace:** every attempt carries a `reasoning`
  label — `preserved`, `translated: …`, `dropped: …`, or
  `unrepresentable: …` — rendered per hop, mirroring the preread
  label style. An attempt that carried no reasoning state shows
  no label.
- **Reasoning replay block:** `GET /dashboard/reasoning`
  serves counts only (entries, captures, hits, misses, injected,
  evicted), refreshed on the same 5s cycle as the trace.
  Reasoning content is never served and never rendered; before
  any capture the block reads "No reasoning captured yet".

## Limits (honest)

- The store is in-memory: a restart loses all captured state.
  The next turn misses rather than replaying stale state.
- Stream capture happens only on the server-facing drain path
  (`ChatStreamWithChain` direct callers get none).
- A client must never echo `reasoning_content` back: the
  provider client never emits it on its own wire; the router
  re-attaches it from the store. If a client sends reasoning in
  a user message, it is treated as user text — the router does
  not strip it.
- Gemini signature attach is best-effort by name: a named
  `functionCall` gets its matching signature; otherwise the
  first part carries it. A bare signature with no matching call
  attaches in order.
- Cohere Chat API v2 has no reasoning replay field: any state
  fails that hop honestly.
- The trace label and the wire behaviour share `reasoning.Check`
  as the single source of truth, but a client that does not
  expose its wire format gets no label at all — guessing would
  mislabel a native-wire hop.
