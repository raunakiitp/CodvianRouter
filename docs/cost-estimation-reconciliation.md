# Pre-dispatch Cost Estimation (P1.7)

## Rules this block enforces

1. **An estimate is not an invoice.** The estimate is labelled as one,
   priced only from the price book, and never produced by calling a
   provider.
2. **Usage alone is not a reconciliation.** A request is `reconciled` only
   when *both* the estimated and the actual cost are known. Otherwise the
   response says which side is unknown and why.
3. **Unknown is not zero.** A missing price yields `cost_known: false`,
   a zero cost, and a warning — never a silent free request.

## Audit: what already existed

| Piece | Where | Status |
| --- | --- | --- |
| `POST /api/cost/estimate` with local token count + model prices | `internal/server/cost_estimation.go` | PARTIALLY IMPLEMENTED — ignored tools, multimodal parts, cached/write rates, provider selection and requested output tokens; no budget view |
| `CostReconciliation` / `reconcileCost` | same file | NOT IMPLEMENTED — dead code, and it marked `reconciled` whenever usage > 0 |
| `callerKeyIDFromContext` | same file | STUB — always returned "" |
| Pre-dispatch budget check primitive | `ledger.CheckBudgetForEstimate` | IMPLEMENTED (existed, unused by the estimate API) |
| Token-count drift (estimate vs reported prompt tokens) | `ledger.DriftSummary` + `UsageSummary.Drift` | IMPLEMENTED (already surfaced) |
| Cached-input / cache-write-tier prices | `config.ModelPrice` + ledger usage fields | IMPLEMENTED (stored, not used by estimation) |

## What this delivery implements

### `POST /api/cost/estimate` (reworked)
- **Token counting** in fidelity order: wire-framed calibrated tiktoken
  (`tokencount.CountPromptWireCalibrated` — counts tool definitions and
  multimodal parts) → plain tiktoken → char/4 fallback. The response
  reports `prompt_tokens_source` and `token_count_wire`, so a caller knows
  which estimator ran.
- **Pricing** by selected provider first (`pair`), then model-level
  (`model`), then the opt-in models.dev-synced pair; `price_source` is
  reported and `unknown` when nothing is booked.
- **Breakdown** carries every known rate — input, output, cached input,
  cache-write 5m/1h — and the priced input/output legs. The cached share
  cannot be known before the provider answers, so `cached_share_unknown`
  is `true` and no cached cost is invented.
- **Output tokens**: `output_tokens` → `max_tokens` → a documented
  heuristic, with `completion_tokens_source` naming which applied.
- **Optional budget check** (`budget_check: true`): read-only
  `ledger.CheckBudgetForEstimate` against the authenticated key. Reports
  `within_budget`, `remaining_usd`, `soft_warn` and a note. A negative
  remaining reports over-budget; a key with no cap reports "no budget cap"
  rather than an infinite figure; a ledger failure is reported as such.
  The endpoint never refuses — it estimates.
- **No provider call, ever.**

### `POST /api/cost/reconcile` (new)
Prices provider-reported usage and compares it with the estimate:
- input priced with the cached share at the cached rate (falling back to
  the plain input rate when no cached price exists — the honest fallback,
  not a guess of zero),
- cache writes at their 5m/1h tier rates when known,
- completion at the output rate,
- status `usage_missing` | `actual_unknown` | `estimate_unknown` |
  `reconciled`, and a variance (USD and %) **only** in the `reconciled`
  case.

### `callerKeyIDFromContext` (implemented)
The request middleware now carries a lazy caller-key holder in the
context; the accessor resolves the authenticated key ID once, on demand,
and returns "" for a context this server did not produce. It never
guesses an identity.

### `writeJSON` hardening (discovered while testing)
A response that cannot be marshalled (e.g. an `+Inf` remaining figure) used
to become a **silent empty 200**. `writeJSON` now encodes before writing
headers and answers `500 {"error":{"message":"response could not be
encoded"}}` on failure.

## Tests

| Test | Pins |
| --- | --- |
| `TestP17EstimateKnownPrice` | price source, cost math, cached rate reported but no cached cost invented, estimate note |
| `TestP17EstimateUnknownPriceIsNotZeroCost` | unknown price → `cost_known:false`, zero cost, warning, `price_source:"unknown"` |
| `TestP17EstimateCountsTools` | tool definitions raise the prompt count; wire framing is reported |
| `TestP17EstimateBudgetCheck` | within/over budget against the authenticated key; estimate still 200 when over |
| `TestP17ReconcileStatuses` | `usage_missing`, `actual_unknown` (no split), `actual_unknown` (no price), `estimate_unknown`, `reconciled` with cached-leg math and variance |
| `TestP17ReconcileCacheWriteTiers` | 5m/1h cache-write tiers priced at their rates |
| `TestP17CallerKeyIDFromContext` | context carrier resolves the authenticated key; a bare context yields "" |

## Deliberate limits

- The budget check is a **read-only view** for the estimator; the live
  request path keeps its own gates (key budget, team, org, internal
  budgets) and remains authoritative.
- Token-count variance already existed (`DriftSummary`); this block adds
  **cost** reconciliation and variance, and does not duplicate the token
  drift surface.
- Estimates use the price book. When a price is absent or stale the
  response says so; it never extrapolates from another model or vendor.