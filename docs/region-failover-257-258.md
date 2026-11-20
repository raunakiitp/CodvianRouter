# Multi-region failover order (CHECKLIST 257–258)

Research-log entry 232 (Parasail multi-region routing): US-East →
APAC adds 180–220 ms; EU → US-East adds 80–110 ms. The cross-region
penalty is far cheaper than a model switch that rebuilds the prompt
cache, so failover order is region-aware:

1. the same model in another region (preserves the model and the
   prompt-cache shape);
2. an equivalent model from another provider;
3. a self-hosted open-weight model.

## Configuration (all operator-declared, never guessed)

| Setting | Meaning |
|---|---|
| `providers[].region` | Serving region for the provider (e.g. `us-east-1`); fallback for its models. |
| `providers[].models[].region` | Serving region for one model; wins over the provider's. |
| `routing.model_equivalents` | Model ID → equivalent model IDs, in preference order. |

`""` (absent) means unknown — never a default region, never inferred
from a base URL, a provider name, or a catalog entry (the catalog
carries no region for any provider, deliberately). A region value
with whitespace is rejected at load, naming the field; the value is
never verified against a vendor's region list. A model with no
`model_equivalents` entry has no equivalents; an entry no provider
offers contributes nothing; a model listing itself, or repeating a
model, is rejected at load.

The self-hosted last resort is appended by the server's
`chainFor` (cmd/codvian-router/main.go), not by the equivalents
table: a self-hosted open-weight model is not an equivalent of
the requested model, so it would otherwise never be in the
chain at all — the live smoke caught this (R3 502'd with the
local model never attempted). Every model offered by a
self-hosted provider is appended (deduped, registry order); the
engine's region tiering keeps them last and `MaxAttempts` bounds
how many are tried. The engine's default `chainFor` cannot do
this — it has no model lists — so the append lives only in the
server's chain builder.

Self-hosted is decided on positive identification only: a
catalog local provider (Ollama, LM Studio, vLLM, llama.cpp, ...)
or Ollama wire format. A loopback base URL alone does NOT make a
provider self-hosted — a loopback OpenAI-compatible endpoint may
be a local proxy forwarding to a hosted provider, and the tier
asks "is this an open-weight model served locally", not "does
data leave this machine" (the looser loopback test belongs to
the `data_collection: "deny"` rule, which asks that different
question). A custom loopback server with no catalog entry and no
Ollama wire is an ordinary provider: tried in its model tier,
never demoted to last on a URL guess.

## How it orders

`orderAttempts` (flat path only) stably partitions the chain after
the quota-hint sort and the strategy hook, keeping the primary
(first after scoring) first: same model in another declared region,
then same model in the same region (or unknown region), then
equivalent models, then self-hosted. Within a tier the scored order
stands. An explicit client order (per-request contract `order`) is
final — no region tiering re-ranks it — and a strategy tree orders
its own chain (the tree is explicit operator order too); the safety
partitions (zero-quota, adaptive saturation, soft-demotion, reserve
keys last) still apply everywhere.

The existing failover rules still apply inside each step: only
429/5xx/timeout fail over, Retry-After is honoured, pin escapes
still work (`pinFirst` runs after ordering).

## What the operator sees

- `X-Codvian-Decision` gains a trailing `; region_step=cross-region`
  (or `equivalent-model` / `self-hosted`) when the serving attempt
  reached its target by a region step; absent when the first target
  served. See `docs/decision-header.md`. The step is computed
  against the model the winner actually served, not the requested
  model: an equivalent-model or self-hosted winner serves a
  different model, and naming the step against the requested model
  compared a same-model failure with the same model and reported
  no step at all — the trace already named the step while the
  header was silent (caught live by the smoke, R2c/R3b).
- `GET /dashboard/trace`: every attempt carries `region` (effective
  region, `""` when unknown) and `failover_step` (the step that led
  to it, `""` when none); the Live Routing view renders both.
- `GET /dashboard/guardrails`: `failover_257` carries live counts
  (`cross_region`, `equivalent_model`, `self_hosted`, most recent
  step/target/time) since process start; the Guardrails view renders
  them in full and the Live Routing card shows the one-line counts.
  An engine that does not report them gets no block — absent, never
  zeroed counts presented as measured.

## Known limitations (honest, not hidden)

- Region tiering does not apply to strategy-tree-ordered chains or
  to explicit contract/group orders: those orders are stated
  explicitly, and the region order does not override a stated order.
- The primary pick is chosen by scoring (quota/strategy), not by
  region: if scoring picks a self-hosted target first, it serves
  first — the region order governs the failover after the primary,
  not the pick. In practice the balanced hook promotes never-failed
  targets, so after earlier requests fail A/B/C it can make C or
  the self-hosted D the primary; the live smoke asserts the
  first-target form then (no `region_step`, single-attempt trace,
  feed count unchanged) instead of failing a legitimate pick, and
  the exact tier order in that case rests on the unit tests'
  deterministic trace assertions.
- A same-model hop whose region is unknown on either side can never
  be proven cross-region, so it sorts as same-region; it is still
  tried, just not promoted.
- The 180–220 ms figure is Parasail's measured cross-region penalty
  (research-log entry 232), not a measurement of this router.
