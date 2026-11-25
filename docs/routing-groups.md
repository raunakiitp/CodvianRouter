# User-named routing groups (CHECKLIST 173–174)

An operator names a pool of providers once in the config file and
addresses it as one routing target — the same idea as OpenRouter's
provider ordering and comparable-gateway named provider configs: one name
for a pool, usable wherever a routing target is named.

## Config

```json
{
  "routing": {
    "groups": {
      "fast": ["prov-openai", "prov-groq"],
      "cheap": ["prov-deepseek", "prov-groq"]
    }
  }
}
```

Rules, enforced at config load (a violation fails startup with the
field and the name in the error — never a silent smaller pool):

- A group name is non-empty and contains neither `:` nor `/` (both
  are alias syntax).
- A group lists at least one provider; no duplicates in one group.
- Every member is a provider ID from the same config file. An
  unknown member is a startup error naming it.
- A provider may belong to several groups.

## Addressing a group in a request

Two spellings, same target:

1. `routing.group` in the request body, with the model as usual:

   ```json
   { "model": "gpt-4o", "routing": { "group": "fast" }, "messages": [...] }
   ```

2. The model alias `group:<name>/<model>` (only the first `/`
   splits, so a model ID may itself contain `/`, e.g.
   `group:fast/meta-llama/llama-3`):

   ```json
   { "model": "group:fast/gpt-4o", "messages": [...] }
   ```

When both spellings are present they must name the same group — a
conflict is a 400 naming both. A `group:<name>` alias with no model
(`group:fast`) is a 400 naming the alias: there is nothing to route
to without a model.

## What a group resolves to

The group's members, in config order, as `(provider, model)`
attempts — with the same membership rules as the fallback chain:
a member with no live client, a disabled member, or a member that
does not offer the requested model is skipped, never dialled. The
resolved chain then flows through the engine exactly like any other
chain: the same failover, the same per-request budget gate, the same
ledger rows, the same per-request contract filters (`order` /
`only` / `exclude` / `sort` / `max_price` / `data_collection`
filter the group chain), the same context-fit check, the same
circuit breakers and cooldowns.

## Honest failures

- **Unknown group** — a request naming a group that is not
  configured (or naming one when no groups are configured at all)
  is a **400 naming the group**, never a silent fallback to the
  full chain.
- **Group serves nothing for this model** — every member skipped
  (disabled, clientless, or not offering the model) is a **400
  naming the group and the model**.
- Both failures happen before any provider is dialled, are recorded
  on the route trace (`GET /dashboard/trace` `contract.group` +
  `contract.error`, no attempts), and are answered by the server
  like any other per-request contract rejection — never counted as
  an upstream failure.

## Groups in chains

A `refusal_chains` entry may be `group:<name>`: it expands to the
group's members, in order, for the refused model. The group name is
validated at config load (an unknown group is a startup error
naming it, because groups are config-internal — unlike provider IDs,
which may arrive later via a live provider add). A group with no
member serving the refused model contributes nothing to that
refusal chain.

## Dashboard

The Models view lists every group with its members in try order
and live member health from `GET /dashboard/groups`: enabled flag
and display name from the config, circuit-breaker state, and the
real attempt record from the engine (attempts, successes, success
rate, EWMA latency). A member with no attempts yet reads "No
attempts yet" — never a 0% success rate or a 0 ms latency it never
measured. The Live Routing trace renders the group of the last
request in its contract summary line (`group=fast`). With no groups
configured the card reads the honest empty state.

## Limits

- Groups are config-file only: there is no dashboard POST to create
  one (edit the config file and restart, or reload). A group that
  names a provider added live works once that provider is saved to
  the config file — group members are validated against the file's
  provider list at load.
- A group is a pool of *providers* for one model per request, not a
  pool of models: the model still comes from the request.
- A group member that stops offering a model (provider edit) is
  skipped at request time, not at load: the group keeps routing
  through its remaining members.
