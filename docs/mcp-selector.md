# MCP selector mode (CHECKLIST 141–142)

The router can expose operator-configured MCP servers to models.
Research-log entry 179 ("Inference Gateway") was not present in
this worktree or in the the reference router reference checkout available
here, so this slice implements the task brief as specified; the
entry could not be read and is not quoted.

## The problem

The naive way to give a model MCP tools is to inject every
discovered tool schema into every request. With N servers × M
tools the prompt grows without bound, and most of it is never
used. Selector mode fixes the size: exactly two meta-tools are
injected no matter how many servers or tools are configured.

## Modes

| Mode | What the model sees per request |
|---|---|
| `selector` (default) | `mcp_tools_get` + `mcp_tools_execute` — 2 tools, always |
| `direct` (opt-in) | Every discovered tool schema, qualified as `server__tool` |

`mcp_tools_get` lists the discovered tools (optional `server`
filter, optional `query` substring). `mcp_tools_execute` runs one
by qualified name (a plain name works when it is unique across
all servers) with an `arguments` object.

Tool names are qualified as `<server>__<tool>`, sanitised to the
OpenAI function-name alphabet `[A-Za-z0-9_-]` and truncated to 64
characters; sanitisation collisions get a `__2`, `__3`, …
suffix so no tool is silently dropped.

## Config

```json
{
  "mcp": {
    "mode": "selector",
    "max_iterations": 10,
    "servers": [
      {"name": "calc", "command": "python3", "args": ["calc-mcp.py"]}
    ]
  }
}
```

- `mode`: `"selector"` (default) or `"direct"`. Anything else
  fails config load, naming the field.
- `max_iterations`: the agent-loop cap, 1–50 (0 = default 10).
- `servers`: each needs a unique non-empty `name` and a
  `command`. The command is the operator's own choice from their
  own config file — it never arrives over HTTP. Servers are MCP
  servers speaking JSON-RPC 2.0 over stdio (protocol 2025-03-26).
- With no `mcp` block (or no servers) nothing is injected and
  the dashboard reports the honest empty state.

Tools are discovered once at startup (`tools/list` per server).
A server that fails to start or answer is logged, recorded as
down in the dashboard, and contributes no tools; the healthy
servers still load.

## The agent loop

When the model answers a non-streaming chat completion with a
tool call the registry can execute (a meta-tool or a discovered
tool), the gateway executes it and calls the model again with
the tool result appended — as an **in-process** call to the same
engine `Chat` function the HTTP handler calls. It never makes a
second HTTP request back through the gateway, so the middleware
chain (auth, guardrails, ledger, cache, coalescing) is entered
exactly once per client request, no matter how many iterations
the loop runs. There is no HTTP client in `internal/mcptools`
at all, so "no HTTP re-entry" is structural, not a convention.

- The loop runs only for **non-streaming** chat completions.
  Streaming requests pass through unchanged: the model never
  sees MCP tools on a stream (the loop cannot run while a
  response is already being delivered).
- A response that calls a tool the registry cannot execute (a
  tool the *client* defined) is returned to the client untouched
  — the normal OpenAI client-driven tool flow keeps working.
- Unknown tools and execution failures become an `error: …`
  tool result the model can see, never a failed request.
- **Cap:** default 10 follow-up model calls per request
  (configurable 1–50). Hitting the cap is an honest stop: the
  last model response is returned with `Capped` set and the
  response header `X-Codvian-MCP-Capped: true`. Responses also
  carry `X-Codvian-MCP-Mode` and, when the loop ran,
  `X-Codvian-MCP-Iterations`.
- Each follow-up model call is a real provider call (billed and
  ledgered exactly like the first one, by the engine itself).
- Tool results come from operator-configured local MCP servers;
  they are **not** re-scanned by the guardrail stage (the
  inbound request is scanned before any provider is dialled, as
  usual). MCP servers run with the operator's trust: a malicious
  MCP server can return arbitrary text to the model, the same as
  any tool the operator configures.

## Dashboard

The "MCP & CLI" section's "MCP selector mode" card shows, from
`GET /dashboard/mcp` (Tier 3, auth-exempt like the other
dashboard feeds; no keys, no base URLs, no command lines):

- mode (selector / direct / off) and the loop cap;
- configured servers (name, up/down, tool count, last error);
- what each mode exposes (2 meta-tools vs all N discovered);
- live tool-call counts: `mcp_tools_get` calls,
  `mcp_tools_execute` / direct executions, per-tool execution
  counts, agent-loop runs, follow-up model calls, and loops
  stopped at the cap. All counters are real, counted where the
  calls happen; with no traffic they read zero.

## What this slice does not do

- No MCP client over HTTP/SSE transports — stdio only.
- No tool-result caching: every `mcp_tools_execute` runs the
  tool again.
- No per-tool allow/deny list: every tool a configured server
  exposes is available to the model. The operator controls this
  by choosing which servers to configure.
- No streaming agent loop (see above).
- No `tools/list` refresh while running: the snapshot is taken
  at startup. Restart the router to pick up a server's new
  tools.
