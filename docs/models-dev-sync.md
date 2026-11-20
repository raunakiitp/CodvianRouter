# Live price/context sync from models.dev (CHECKLIST 179–180)

Opt-in, off by default. When the operator enables it (`models_dev`
block in the config file), the router fetches the public models.dev
catalog (`https://models.dev/api.json`) and fills in **prices** (USD
per 1M tokens) and **context windows** for the (provider, model)
pairs it serves — but only where the operator has not set a value
themselves. the reference router syncs the same data; this is the parity piece,
with the operator-wins rule made explicit.

## Opt-in, never phoning home

- The sync is OFF unless `models_dev.enabled: true` is in the config
  file. With it off the router makes no network call to models.dev —
  no fetch, no DNS, nothing.
- The fetch runs in the background after `serve` starts, never
  before: startup stays ~1s even if models.dev is slow or offline.
- The config URL, interval and timeout are validated at load whether
  or not the sync is enabled, so a typo fails loudly at startup
  instead of the day the sync is turned on.

## What models.dev reports (and what is used)

Per provider, per model, models.dev reports `cost` (USD per 1M
tokens: `input`, `output`, `cache_read`, `cache_write`) and
`limit.context` (the context window). The router uses:

- the **base tier only** of `cost`. Tiered prices
  (`context_over_200k`, per-tier entries) are ignored: the router
  cannot know pre-send which tier a request falls in, and guessing
  a tier would invent a price.
- `limit.context` as the context window, when positive.
- a price only when at least one billable leg (`input`/`output`) is
  present. A cached-input price alone does not make a price — the
  ledger and the scorer need the billable legs.
- nothing for models models.dev marks `"deprecated"` — they are
  skipped entirely.

A reported cost leg that is negative or non-finite invalidates the
price for that model (the window, if any, still syncs). A model
models.dev does not list is skipped, never invented; a local
provider models.dev has no entry for syncs nothing.

## Matching

- Provider match is exact: the local provider's `catalog_id` first,
  then its registry ID, against the models.dev provider ID. No fuzzy
  matching, no base-URL guessing.
- Model match is an exact model-ID match against the models the
  router knows the provider offers. The sync never invents a model
  the provider does not serve.

## Precedence — operator values always win

Lookup order for a (provider, model) price:

1. operator pair price (config `provider_prices` / price file pair),
2. operator model-level price (config `prices` / price file flat),
3. synced models.dev price.

The sync itself never writes a pair the operator priced at either
level (pair *or* model-level — a synced pair would otherwise shadow
an operator's model-level price through the pair-wins rule). If the
operator prices a pair later, the synced price drops out of the
layer on the next sync.

For context windows: a synced window fills in only when the
registry's window is 0 (unknown), or exactly the value the sync last
applied (refresh). Any other current window is treated as
operator-set and is never overwritten; if the operator later changes
a synced window, the synced record is dropped on the next sync.

## Failure and staleness

- A failed or offline fetch keeps the last good data — in the price
  book, in the registry, and in the persisted state — and records
  the failure (last error + last attempt time). The dashboard Models
  view shows "last sync failed at … — showing the last good data
  from …". Nothing is ever silently stale.
- Before the first successful sync there is no synced data at all
  (not stale data): the feeds say the sync never succeeded.
- The synced state persists in `models-dev-sync.json` in the data
  dir (0600, atomic write), so a restart keeps the last good
  prices/windows without re-fetching. A corrupt state file only
  warns at startup; the sync overwrites it on the next success.
- Synced entries for a provider that no longer exists in the
  registry are pruned on the next sync. Entries for a provider that
  still exists but whose model is absent from its list are kept —
  a model list can be transiently incomplete.

## Where synced values flow

- **Price book** (`internal/config`): the synced layer is a
  separate, copy-on-write layer consulted last at `Lookup` time.
  `GET /dashboard/prices` lists synced rows with source
  `"models.dev"` and the sync time.
- **Ledger**: `buildLedgerPrices` includes synced pairs (except
  where the operator has a model-level price), so spend is real
  where a synced price exists. A periodic re-sync rebuilds the map
  and installs it on the async writer and the server, so charging
  follows the new price.
- **Scoring / strategies / auto-router / budget gates**: they read
  the price book (`Lookup`), so synced prices apply there
  automatically — a synced $2 model outranks a synced $60 one
  exactly like an operator-priced one would.
- **Context fit** (CHECKLIST 159): the fit gate reads windows from
  the registry, so a synced window makes the pre-call fit check
  work for models whose window was previously unknown.
- **Dashboard Models view**: price source `"models.dev"` + sync
  time, context-window `"synced"` marker + sync time, and a sync
  status line (on/off, last sync, last error, stale note).

## Honest limits

- models.dev prices are **list prices**, not the operator's
  contracted prices: a provider may charge differently (volume
  tiers, negotiated rates, regional pricing). The synced price is
  the best public price the router knows, labelled with its source
  — it is never presented as the operator's own price.
- Tiered pricing is not applied (base tier only, see above), so a
  long-context request may in reality cost more than the synced
  price suggests. The ledger charges the synced price; spend stays
  a lower bound where pricing is tiered.
- Only models the router already knows about are synced. A provider
  whose model list was never fetched (empty model list) syncs
  nothing until its models are known.
- The sync never touches provider keys, base URLs, or any other
  config: it reads the public catalog and writes only the synced
  layer + the state file.
