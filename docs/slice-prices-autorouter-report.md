# Slice: real price source + cost/latency-aware auto-router (items 80–82)

Date: 2026-11-17. Build-first, one test gate at the end. No push (coordinator owns pushes).

## What landed

### 1. Real price source (`internal/config/prices.go`, `config.go`, `SCHEMA.md`)

- Config file gains a `prices` block (model ID → USD per 1M tokens:
  `input_usd_per_1m`, `output_usd_per_1m`, optional
  `cached_input_usd_per_1m`) plus an optional `price_file` (local JSON
  file, same shape, read once at startup).
- Price fields are pointers: absent = unknown, present 0 = genuinely
  free. A half-known price (only input or only output set) has an
  unknown blended price — the missing side is never guessed.
- Validation (named fields): negative / NaN / infinite prices rejected
  on load, for the `prices` block, the price file, and the legacy
  per-token prices on provider models. A set price file that cannot be
  read or fails validation fails startup (fail-closed).
- Precedence: config-file entries override the price file for the same
  model. `PriceBook` records each entry's source (`config` /
  `price-file`) for the dashboard.
- No network fetch, no embedded price table, no invented prices.
  Unknown-price models stay explicitly unknown (neutral in scoring,
  $0 in the ledger, labelled as a lower bound wherever spend is shown).

### 2. Cost/latency-aware tier ordering + measured quality

- **Quality, defined exactly**: the measured error-free completion rate
  over real attempts (successes / attempts, real attempts only — hedge
  losers and limiter-shed calls never counted). It is NOT a benchmark
  score, NOT an eval judgement, NOT a claimed capability rating, and
  says nothing about answer quality — only about how often attempts
  completed without error. 0 always means "no attempts yet" (unknown,
  neutral-minus-penalty). This definition is documented in
  `internal/scoring`, `internal/strategy`, the server feed comments,
  and the dashboard "How it works" text.
- `scoring.Hook` / `HookWithWeights` and `strategy.HookFactory` now
  take `SignalsFunc(providerID, model string)`: cost is per model, so
  the same provider ranks differently for a cheap model and an
  expensive one. (A parallel slice had already changed `main.go`'s
  `signalsFn` to the 2-arg form; this slice completed the scoring /
  strategy side so the tree builds.)
- `main.go` `signalsFn`: cost from the price book for the requested
  model (fallback: that model's legacy per-token prices on the provider
  entry); quality set ONLY from real engine attempts
  (`ProviderStats`, samples > 0) — never from the health probe.
- `autorouter.Config.RankModels` (`ModelRanker`): each tier's candidate
  models are ranked by real cost + measured latency/success + measured
  quality under balanced weights before the first one wins. Balanced,
  not cheapest/fastest: the tier already encodes the virtual model's
  axis (auto/cheap restricts to the cheap tiers, auto/fast to the fast
  ones). The ranker must return a permutation; a ranker that drops,
  duplicates or panics falls back to list order. `main.go` wires it
  from the price book + live engine stats (sample-weighted across the
  providers offering each model).
- Ledger spend is now real where prices exist: the adapter builds
  `ledger.Prices` from the price book (per-1M ÷ 1e6) with legacy
  per-token prices filling gaps; unpriced models still record $0.

### 3. Dashboard (`internal/server`, `web/index.html`)

- New `GET /dashboard/prices`: the price book verbatim (input/output/
  cached/blended per 1M, source), honest empty state when nothing is
  configured. Registered in the guard table (TierManagement).
- `GET /dashboard/strategies` now carries per-chain `cost_usd_per_1m`
  (null = price unknown), `quality` + `quality_samples` (null/0 = no
  attempts yet), plus `priced_models` and `price_source`. Dashboard
  quality is the ledger's all-time per-provider success rate,
  sample-weighted across the providers offering a model, reported only
  when the model has recorded attempts of its own (so one model's
  attempts are never attributed to another model from the same
  provider).
- `GET /dashboard/auto-route` and `/dashboard/auto-route/last` now
  carry `model_cost_usd_per_1m`, `model_price_known`,
  `model_quality`, `model_quality_samples` for the resolved model.
- Dashboard: Strategies table gains Cost / 1M tokens and Quality
  (measured) columns ("Price unknown" / "No attempts yet" states);
  Auto-router decision + last-decision cards gain a cost/quality line;
  both "How it works" texts document the quality definition and the
  unknown-price rule. `node --check` on the dashboard JS is clean.

## Tests

- `go vet` clean on all touched packages; `go test -count=1 ./...`:
  **all 25 packages ok, 0 fail**. Touched packages alone: 182
  top-level PASS, 0 FAIL.
- New tests: config price validation / file load / precedence /
  round-trip+clone (7); scoring per-model cost hook (1); autorouter
  ranker reorder / bad-ranker fallback / nil baseline (3); server
  prices endpoint / strategies cost+quality / auto-route enrichment /
  measured-quality arithmetic (4). Also fixed a pre-existing failing
  guard test (route table was stale by the 2 feedback routes; now
  36 routes, tiers 7/3/26).

## Live smoke (real binary, isolated HOME, Python mocks 21031/21032, router 20199)

- Config: CheapMock `cheap-model` $1/$3 per 1M (blended $2),
  ExpMock `expensive-model` $50/$70 (blended $60), `mystery-model`
  unpriced; global strategy `cheapest`; quick-qa tier lists
  expensive first.
- `GET /dashboard/prices` → both priced models with blended $2/$60.
- `GET /dashboard/auto-route?model=auto/cheap` → `cheap-model`
  (ranker flipped list order), cost $2, price known.
- Live `POST /v1/chat/completions` model `auto/cheap` → served by
  `cheap-model` (`X-Codvian-Auto-Model: cheap-model`); `auto/fast` →
  `mystery-model` (unknown price, neutral).
- Reversed list order (expensive listed first in the chat tier) still
  resolves `auto/fast` to `mystery-model` — the ranker, not list
  order, decides; unknown beats $60 but loses to $2.
- `GET /dashboard/strategies` → cheap $2 quality 100% (1 sample),
  expensive $60 quality unknown, mystery cost unknown quality 100%.
- `GET /dashboard/usage` → cost **$0.00025** for the cheap-model
  request (100 × $1/1M + 50 × $3/1M, exact), mystery-model $0.
- Dashboard HTML serves with the new Cost / Quality columns.

## Honest limitations

- Prices are keyed by model ID, not by (provider, model): two
  providers offering the same model ID share one price. Per-reseller
  pricing is not expressible.
- The request-path quality signal and the dashboard quality come from
  different windows of the same attempts: the engine's in-memory
  stats (since process start) vs the ledger's all-time roll-up.
  They agree in the long run and differ after restarts.
- Dashboard per-model quality is a provider aggregate (the engine
  keeps stats per provider), gated on the model having its own
  recorded attempts; a provider's rate covers its attempts across the
  models it serves.
- A provider with 100% failed attempts reports quality "unknown"
  (scoring cannot distinguish a measured 0 from no signal); the
  success-rate component still scores it 0, so it ranks last on
  reliability regardless.
- `cmd/codvian-router/main.go` was touched for wiring only (price-book
  load, signals, ranker, ledger prices, `WithPriceBook`) — it was not
  in this slice's package list, but the live smoke the task requires
  is impossible without it. `doctor_configure_mcp.go` got two
  nil-ranker call-site fixes for the same reason.
- No rendered-browser visual check of the new dashboard columns
  (headless Chromium will not start in this VM); the JS passes
  `node --check` and the endpoints were verified live.
