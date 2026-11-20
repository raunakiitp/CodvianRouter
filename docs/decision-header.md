# Decision transparency + catalog filters / shortlist / refresh (CHECKLIST 199–200)

## X-Codvian-Decision — the decision header

Every response the gateway serves from a provider carries one header
naming the decision behind it:

```
X-Codvian-Decision: provider=prov-a; model=gpt-4o; strategy=cheapest; reason=fallback-after-2
```

Four key=value pairs, semicolon-separated, machine-readable. Values
are sanitised (`;`, `=` and control characters become `_`), so an
operator-configured name can never smuggle a second pair into the
header.

| Field | Meaning |
|---|---|
| `provider` | The registry ID of the provider that served this response. |
| `model` | The concrete model that served it (the resolved model for an `auto/*` request, never the virtual name). |
| `strategy` | The strategy that ordered the chain (`cheapest`, `balanced`, `fastest`, …). |
| `reason` | Why this provider served it (vocabulary below). |
| `region_step` | The region-aware failover step of the serving attempt (CHECKLIST 257–258) — present only when the request actually failed over to a region tier: `cross-region` (same model, another declared region), `equivalent-model` (a different model from another provider), `self-hosted` (a self-hosted open-weight model). Absent when the first target served. It is a trailing key, after `reason`, so a client that parses only the first four pairs is unaffected. |

### Reason vocabulary and precedence

The reason is computed by the engine at the decision moment
(non-streaming success, stream commit, or the single
embeddings/rerank attempt) and recorded on the engine's routing
record, so the header and the dashboard's Live Routing view can
never disagree.

| Reason | Meaning |
|---|---|
| `fallback-after-N` | N earlier targets in the chain did not serve this request (failed, or skipped before dialling) before this provider did. A hedged loser is NOT counted: it ran in parallel and lost the race. A zero-quota skip and a breaker skip count exactly like a 429. |
| `group` | The request named a routing group, and the first member served it (no fallback). |
| `contract` | A per-request routing contract (`order` / `only` / `exclude` / `sort` / `max_price` / `data_collection`) shaped the chain and the first target served it (no fallback). |
| `tree` | A nestable strategy tree ordered the chain and the first target served it (no fallback). |
| `cheapest` / `latency` / `reliability` / `cost-per-solved-task` / `balanced` / `weighted` / … | No more specific reason applies — the strategy itself is the reason: `cheapest` → `cheapest`, `fastest` / `least-latency` → `latency`, `most-reliable` → `reliability`, `code` → `cost-per-solved-task` (the auto/code mode picked the model by expected cost per solved task), anything else reports its own name. |

Precedence, highest first: `fallback-after-N` > `group` > `contract`
> `tree` > strategy-derived. A fallback is the most specific fact
about why *this* provider served (the first choice did not), so it
wins over group/contract/tree; group wins over contract (a group
*is* the contract's chain); tree is last before the strategy,
because a tree orders the chain but does not choose the serving
target.

### Surfaces

| Surface | Header |
|---|---|
| `POST /v1/chat/completions` (non-streaming) | Set on the response, after the upstream response is known. |
| `POST /v1/chat/completions` (`stream: true`) | Set before the first SSE chunk is written — the engine publishes the decision at stream commit, immediately before the first event is sent. An empty/error stream that never commits carries no decision. |
| `POST /v1/responses` (non-streaming) | Set on the response. |
| `POST /v1/responses` (`stream: true`) | Set before the `response.created` event. |
| `POST /v1/embeddings`, `POST /v1/rerank` | Set on the response (single-attempt path). |
| `POST /v1/messages` (translated) | Set on the response. |

### When the header is absent — and why

An absent header is itself the honest answer; each case has a
documented reason, and no value is ever invented:

- **Served from cache** — `X-Codvian-Cache` names the layer
  instead. No provider was called for this response, so there is
  no provider to name.
- **Served stale** after every provider failed — `X-Codvian-Stale`
  names it instead. No provider served *this* response.
- **The request failed** before any provider served it (contract
  rejection, budget stop, guardrail block, exhausted chain) — there
  is no serving provider.
- **The request bypassed routing** — the `/v1/messages` verbatim
  relay forwards to one fixed provider (`X-Codvian-Relay` names it
  instead). The relay *stream* commits its headers before routing,
  so it cannot carry the header either.
- **The engine's record could not be matched to this request**
  (model mismatch — e.g. a concurrent request's record) — the
  header is withheld rather than guessed. For an `auto/*` request
  the record names the resolved concrete model, which is what the
  header carries.

## Catalog filters — `GET /v1/models` and `GET /dashboard/models`

Both feeds share the same query parameters (server-side filtering):

| Parameter | Meaning |
|---|---|
| `provider=` | A provider registry ID or display name (case-insensitive). On `/v1/models` each model ID is listed once, under the first provider offering it in registry order, so the filter matches that listed provider; the dashboard feed lists per-(provider, model) pairs and matches exactly. |
| `category=` | The catalog category of the model's provider (`api-key`, `free`, `local`, `no-auth`, `oauth`, …; `custom` for a hand-added provider), exact, lowercased. |
| `max_price=` | USD per 1M tokens, blended (`(input+output)/2`). A row passes when its cheapest known blended price is ≤ the value. A row with no known blended price is **excluded, never assumed cheap** — the same rule the per-request `routing.max_price` contract uses. |
| `min_context=` | Minimum context window in tokens. A row with an unknown window (0) is **excluded, never assumed large**. |
| `shortlist=true` | Only operator-pinned models. |

A malformed `max_price` / `min_context` / `shortlist` answers
400 naming the parameter, never a silently unfiltered list.

Price units: all prices are USD per 1M tokens. The filter reads
the operator price book's resolved price for the (provider, model)
pair first (pair, then model-level, then synced — a genuinely-free
`$0` price passes any `max_price`), else the row's own
provider-reported legs when both are reported. A half-known
configured price (one leg only) leaves the blend unknown.

## Shortlist — config `shortlist`

The operator's curated shortlist (`config.shortlist`: an array of
strings). Pinned rows sort first in both feeds, in config order,
marked `shortlisted: true`.

An entry pins (a) any row whose model ID equals the entry
(`"gpt-4o"`, matched wherever it is offered) and (b) any row whose
`"providerID/modelID"` equals the entry (matched only for that
provider). Both readings of an entry containing `/` apply, so a
model ID that itself contains `/` keeps working as a plain pin.

Entries must be non-empty with no duplicates (rejected on load).
Entries naming models no provider offers are ignored, never an
error — the shortlist is a display pin, not a routing rule, and a
pinned model that disappears is simply not shown.

## Live refresh — `POST /dashboard/models/refresh`

Re-probes every provider's model list live and returns one result
per provider, in registry order:

```json
{
  "refreshed_at": "2026-11-18T00:00:00Z",
  "providers": [
    {"provider_id": "prov-a", "provider_name": "Provider A", "status": "ok", "models": ["m1", "m2"]},
    {"provider_id": "prov-b", "provider_name": "Provider B", "status": "failed", "models": ["old"], "error": "upstream HTTP 500"},
    {"provider_id": "prov-c", "provider_name": "Provider C", "status": "skipped", "models": ["m3"], "error": "provider disabled"}
  ],
  "total": 3, "ok": 1, "failed": 1, "skipped": 1
}
```

- `ok` — the live list was fetched and stored.
- `failed` — the live list could not be fetched; the stored list is
  kept untouched (a failed refresh never empties the catalog), and
  `error` names the reason: an upstream HTTP failure is its status
  only (`upstream HTTP 429` — never the response body), anything
  else is the transport error's own message.
- `skipped` — the provider was not probed (disabled), with the
  reason.

A successful fetch replaces the stored list, merged so per-model
metadata the upstream does not report (context window,
provider-reported prices, the operator's deprecated flag) survives
for models still listed; models the upstream no longer lists are
dropped; genuinely new models arrive with unknown window/prices
(never guessed). The refreshed lists are persisted in one save; a
persist failure rolls the in-memory lists back and answers 500 with
the per-provider results and the persist error — never a success
for a refresh that was not saved. Newly discovered models become
auto-router tier candidates, exactly as live-added providers'
models do (candidacy only — the ranker still decides by real
cost/latency/quality signals).

With no refresher wired the endpoint answers 503, never a faked
refresh. The dashboard Models view has a Refresh button that posts
to it and renders the per-provider results, then reloads the
catalog.

## Dashboard Models view

Filter controls (provider, category, max price, min context,
shortlist-only) pass the same query parameters to
`/dashboard/models`; pinned rows carry a ★ Shortlist badge and the
count line names how many rows are shortlisted; the Refresh button
shows per-provider results (ok with model count, failed with its
reason and "kept its last known list", skipped with its reason).

Display fix in this slice: the Models table's price formatter
multiplied the feed's per-1M values by 1e6, showing a $2/1M price
as $2,000,000 — it now displays the per-1M value as-is. (The same
×1e6 pattern also existed in the auto-route decision rendering
around `web/index.html` line 5083, which formats the
`input_usd_per_1m` fields of the per-provider price list — fixed
in CHECKLIST items 213-214, which also audited every other price
rendering on the dashboard: the Models table (`moneyPerM`), the
Strategies chain cost (`fmtCost`) and the decision's main blended
cost all already rendered per-1M values as-is; the per-provider
pair lines were the only remaining ×1e6 site and now use the
same `moneyPerM` formatter. The `fmtMoney2`/`fmtMoney4` spend
figures are real USD totals from the ledger, not per-1M rates,
so they are unaffected.)

## Coverage notes (CHECKLIST 217–218)

Two gaps the 199–200 slice left open are now closed:

- **`reason=group` at server level (217).** The router unit test
  proved the reason derivation; the server tests
  (`internal/server/decision_group_test.go`) now prove the header
  a client actually sees: a `routing.group` request and the
  `group:<name>/<model>` alias both carry
  `provider=<first member>; model=<model>; strategy=balanced;
  reason=group`, and a fallback inside the group still reports
  `fallback-after-1` — fallback outranks group by the precedence
  above. The live smoke (`scripts/smoke-decision-group-relay.py`,
  checks G1–G3) proves the same against the real binary.
- **Relay-stream header absence, live (218).** The verbatim relay
  stream (`POST /v1/messages`, `stream: true`, Anthropic-wire
  provider) carries NO `X-Codvian-Decision` — proven live
  (smoke checks R3–R4): the relay bypasses routing entirely, so
  there is no decision to name, and its headers commit with the
  first upstream byte. The translated stream likewise carries none
  (smoke check T5): `handleMessagesStream` commits its headers
  (`WriteHeader` + `message_start`) BEFORE the engine runs, so the
  decision is not known yet when the headers go out and cannot be
  added afterwards. The translated NON-streaming path DOES carry
  the header (smoke check T2) — it sets the header after the
  engine response is known, like a chat completion.
