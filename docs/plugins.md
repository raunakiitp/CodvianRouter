# Plugin system (CHECKLIST 201–202)

Codvian Router's plugin system: in-process Go plugins with three hook
points — pre-request, post-response, provider adapter. Competitor
context: LiteLLM / Portkey / Kong expose plugin hooks for guardrails,
transforms, and provider adapters (research-log entries 94, 137, 144);
this is the same shape, minus the dynamic-loading footgun.

## The one decision that matters: no dynamic loading

Plugins are **compiled-in Go code only**, registered at startup from
the config file's `plugins.enabled` list (names, in order). There is
deliberately no plugin path, URL, `.so`, Wasm module, or JS file in
the config:

- Loading arbitrary code from a config file is arbitrary code
  execution by anyone who can write the config file. The gateway's
  threat model treats the config file as operator input, not as a
  code-signing boundary.
- A plugin therefore ships only as Go code reviewed with the tree,
  in `internal/plugins` (the registry + built-ins) or any package
  that registers at startup in `main`.
- `POST /admin/plugin/load` answers **501 by design** (Tier 1
  LOCAL_ONLY guard still applies), and its guard-table description
  says so. It is not an unimplemented feature; it is a refused one.
- Hot-loadable Wasm plugins (Higress-style, research-log entry 137)
  are a future step, not this one: they need a sandbox, a signing
  story, and a capability model first. Until then, adding a plugin
  means rebuilding the binary — the same trade Respan makes with
  natively compiled Rust plugins (entry 144).

## Hook points

| Hook | When | May do | Failure semantics |
|---|---|---|---|
| `pre_request` | After built-in guardrails, before compression / budgets / any provider dial | Allow, deny with a reason, or transform the request in place (model rewrite, add outbound headers) | Error **fails closed** (403, `-32001`, reason `plugin_denied`); panic **fails closed as a gateway panic** (502, `-32603`, the 165–166 rule) |
| `post_response` | Non-streaming: before delivery. Streaming: after the stream completes | Log / count / observe (never veto) | Error or panic **fails open**: logged (redacted) + counted, response still delivered |
| `provider_adapter` | Per outgoing upstream HTTP request, for one provider ID | Rewrite the request (URL, headers, body) for a custom wire | Error or panic **fails closed for that attempt**: the unadapted request is never sent; the engine's normal failover may still try the next provider |

Ordering: pre-request and post-response hooks run in registration
order (the config list order for built-ins). The first denial, error,
or panic in the pre-request chain stops the chain — a later plugin
never sees a request an earlier one refused. Adapter hooks are looked
up by provider ID; at most one adapter per provider ID.

Panic isolation: every plugin call runs behind its own recover
boundary. A panicking plugin is counted (per-hook `panics`), not
disabled: the next request calls it again, so a transient panic does
not silently change the pipeline, and the counts make a flapping
plugin visible in `GET /dashboard/plugins`.

## Built-in plugins

Two ship in the binary as proof the hooks work end to end:

- `header-add` (pre-request): adds `X-Codvian-Plugin: header-add`
  to every upstream request. The header is fixed, never derived from
  prompt content. A mock upstream can assert it arrived.
- `response-log` (post-response): records one observation per
  delivered response — model, finish reason, content length, token
  counts — keeping the most recent 20 for the dashboard. Never
  prompt or response content.

Enable them in the config file:

```json
"plugins": { "enabled": ["header-add", "response-log"] }
```

Unknown names, empty entries, and duplicates fail config load naming
the entry. An absent block means no plugins; the gateway works
exactly as before and the dashboard reports zero plugins honestly.

## cost-cap: the first real built-in plugin (CHECKLIST 239–240)

`cost-cap` is a pre-request guardrail that refuses a chat request
before any provider is dialled when the request's estimated cost
exceeds a configured per-request cap — so an over-budget request
costs exactly $0 and makes zero provider calls.

```json
"plugins": {
  "enabled": ["cost-cap"],
  "cost_cap_usd_per_request": 0.01
}
```

The estimate, honestly:

- **Prompt tokens** — the local tiktoken estimate
  (`tokencount.CountPromptFull`: message text + tool definitions +
  OpenAI-rule framing). It overcounts a native-wire request by a
  few tokens — the safe direction for a fail-closed gate — and the
  provider's own count is what is eventually billed, so the number
  in the refusal is an *estimate*, not a bill (see
  `docs/token-count-audit.md` for the measured drift).
- **Completion tokens** — the request's own `max_tokens` when set,
  else 1,024 assumed tokens (`DefaultAssumedCompletionTokens`). The
  refusal names the assumed count, so the assumption is visible.
- **Prices** — the MODEL-LEVEL price for the model (input and
  output USD per 1M tokens, from the config `prices` block). The
  plugin runs before routing, so it cannot know which provider
  will serve the request and cannot use a per-(provider, model)
  pair price.

Failure semantics:

- Estimate over the cap → the request is refused **402** with a
  message naming the estimated cost and the cap, e.g.
  `cost_cap: estimated cost $0.040240 exceeds per-request cap
  $0.020000 (model "gpt-4o", 6007 prompt tokens + 1024 assumed
  completion tokens)`. The message carries numbers and the model
  name only — never prompt content — so the server echoes it to
  the client (via the `RefusalDetails` interface); other plugin
  denials still get the generic 403 `plugin_denied`.
- No complete model-level price for the model (either side
  missing) → refused **402** naming the missing price. A price
  is unknown, never free — the same fail-closed rule the key
  budgets use for unpriced models. Add a model-level price in the
  `prices` block to unblock the model.
- Cap absent or 0 → unlimited: the plugin registers, counts its
  calls in the feed, and never denies. A negative cap fails
  config load; a cap set without `"cost-cap"` in `enabled` also
  fails load (an inert cap would silently do nothing).

Scope: chat surfaces only (`POST /v1/chat/completions` and
`POST /v1/messages`). Embeddings, rerank, images and Responses
requests pass through untouched — they have a different cost
shape (no completion, or a different price unit) and the plugin
never invents a chat-shaped estimate for them. A request the
plugin cannot cost (nil request, token-count failure) fails
closed with an error, never allowed through uncosted.

The dashboard Plugins view shows the plugin's description (with
the configured cap), and its Allowed / Denials columns are the
real decisions since process start — allowed = pre-request calls
minus denials. `GET /dashboard/plugins` carries the same counts,
plus a `cost_cap` block (CHECKLIST 319–320): `enabled`, the cap,
`models_checked`, and `unpriced_models` — the served models
without a complete model-level price, each with `has_pair_price`
(a pair price exists for the model somewhere in the price book).
The Plugins view renders that list as "Cost-cap — models without
a model-level price", with honest states for not-enabled, no cap,
and all-priced. A pair price never satisfies the cap: an operator
who priced only pairs must add a model-level price in the `prices`
block for the model.

Layer choice (CHECKLIST 319): the refusal stays at request time
— a capped model without a model-level price is refused 402
naming the model — not a startup error. The served-model set
changes as providers are added live, and refusing to start the
whole gateway because one model lacks a price would take down
every other model; the `cost_cap` feed is the proactive surface
instead, so the operator sees the missing prices before traffic
hits them.

## How headers reach the provider

A pre-request plugin writes into `plugins.Request.Headers`. The
server attaches that map to the request context
(`provider.WithExtraHeaders`) and the provider client applies it in
`newRequest`, after auth headers. Reserved headers can never be
overridden through this path: `authorization`, `content-type`,
`content-length`, `accept`, `host`. An adapter hook runs after the
headers are set, so it sees the final request.

## Provider adapter wiring

The adapter is applied through a plain func type,
`provider.HTTPAdapterFunc`, set on the registry by `main` from the
plugins registry — the provider package never imports the plugins
package (no import cycle). `Registry.ClientFor` attaches the hook and
the provider ID to every client it builds (promoted through the
embedded `*openAIClient` to all native-wire wrappers). Key-test
probe clients do **not** carry the adapter: a probe is not a chat
request, and an adapter that rewrites chat bodies must not rewrite
probes.

No built-in adapter ships yet: the hook is defined, tested (registry
unit tests + provider client test), and wired — the first real
adapter lands with the first provider that needs a custom wire.

## Embeddings and rerank surfaces (CHECKLIST 219–220)

The pre-request and post-response hooks also run on
`POST /v1/embeddings` and `POST /v1/rerank`, with the same
failure semantics as on chat. They run on the synthetic
bookkeeping request (the input text as user messages), after the
built-in guardrails and before any budget gate or provider dial —
so a denial happens before the engine is called and can never
cause a retry: the replay exemption (exactly one upstream
attempt, never replayed) is preserved. Plugin-added headers
travel to the upstream on the request context, exactly as on
chat; the provider adapter also applies, since embeddings/rerank
go through the same `newRequest`.

Two honest limitations of the synthetic shape:

- A model rewrite by a pre-request plugin is honoured (it is
  synced onto the real embeddings/rerank request before the
  engine resolves its chain). A message-content rewrite is not:
  the embeddings input / rerank documents travel verbatim
  (`json.RawMessage`) and are never rebuilt from the bookkeeping
  messages.
- The post-response hook sees a synthesised chat shape: the real
  model and usage, an empty Content (there is no chat content —
  `ContentChars` is 0), and finish reason `"stop"`. A rerank
  response with no upstream usage block keeps zero usage, never
  an invented estimate.

The dashboard Plugins view counts these calls in the same
per-hook rows as chat (the counts are per hook point across all
surfaces, not per surface); the feed note says so.

## Dashboard

Plugins view (`GET /dashboard/plugins`, Tier 3): one row per
registered plugin with hook points and live per-hook
calls/errors/denials/panics, plus an Allowed column (calls minus
denials — the real allow count for a pre-request plugin), plus
the response-log plugin's recent response shapes. All numbers
are real counts since process start.

## Writing a plugin

Implement the interfaces in `internal/plugins` (`Plugin` +
`PreRequestPlugin` / `PostResponsePlugin` /
`ProviderAdapterPlugin`), register it in `main.buildPluginRegistry`
(or `plugins.LoadBuiltins` for a built-in), and add tests proving
the deny/error/panic semantics for your hook. Rules that apply to
every plugin:

- Never log or store prompt/response content or key material. The
  response-log plugin's shape-only events are the pattern.
- Never block on the network in a pre-request hook without a
  timeout: the hook runs before the first token.
- An adapter must key off `req.URL.Path` — the same client builds
  chat, stream, models, and probe requests.
- Clone the request before mutating it in an adapter; never mutate
  the request you were handed.
