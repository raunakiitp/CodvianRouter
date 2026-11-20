# Slice 229-230 report — token-count accuracy audit

**Date:** 2026-11-18 · **Scope:** CHECKLIST items 229-230 (backend + dashboard, same slice).

## What was built

- **Audit harness** — `internal/tokencount/audit.go` (fixed 18-case
  corpus in 6 classes + drift statistics) and
  `internal/router/token_audit_test.go` (`TestTokenCountAudit`):
  the corpus is sent through all five wires with the real provider
  clients and the real engine against local mocks returning known
  provider-reported usage, and every sample is read back from the
  real ledger row. The test logs the drift table verbatim in the
  form `docs/token-count-audit.md` uses, and enforces per-cell
  bounds (mean error ≥ −5%, max |error| ≤ 20%).
- **Ledger drift plumbing** — `local_estimate_prompt_tokens`
  column on `usage_events` (schema + migration for old DBs),
  `ledger.Usage.LocalEstimatePromptTokens`,
  `provider.Usage.LocalEstimatePrompt` (set by
  `Engine.reconcileUsage` on every attempt, and by
  `streamFallbackUsage`), both INSERT paths (sync + async batch),
  `RecentEvents`, `Ledger.DriftSummary` (per-provider/model SQL
  aggregates), `UsageSummary.Drift` (so `GET /dashboard/usage`
  carries it with no new route), and the `ledgerAdapter`
  pass-through in `cmd/codvian-router/main.go`.
- **Estimator fix** — `tokencount.CountPromptWire` (message text +
  tool definitions + per-wire framing: OpenAI cookbook 3/message
  + 3 priming, native wires 1 role token/message, Bedrock tools
  JSON reshaped to the Converse `toolSpec` shape) and
  `CountPromptFull` (wire-agnostic reference) wired into the
  pre-send budget gates, the engine strict-budget gate, and the
  context-window fit check. `CountPrompt` itself is unchanged.
- **Dashboard** — "Estimate vs reported — token-count drift" card
  in the Usage view, fed by `summary.drift` from the live
  `/dashboard/usage` feed, with an honest empty state.
- **Docs** — `docs/token-count-audit.md` (method, both tables,
  per-wire budget verdicts, known residuals) and this report.

## Measured findings

- Baseline (old estimator): tool-call-heavy −78% to −81% mean
  error on **all five wires** (tool definitions never counted);
  OpenAI −6% to −19% on every class (framing never counted).
- After the fix: every one of the 30 (wire, class) cells is within
  the bounds — OpenAI 0.00% everywhere except tool-heavy −0.81%;
  worst cell Anthropic tool-heavy +5.01% (safe-direction
  overcount); Bedrock tool-heavy −2.31% (was −80.80%).
- Two mock bugs found and fixed during the audit (both in the
  harness, not production): `t.Fatalf` inside mock HTTP handler
  goroutines (killed connections, tripped the breaker), and
  wrapper-keyed tool blocks (`toolUse`/`toolResult`/
  `functionCall`/`functionResponse`) falling through to
  whole-block canonical JSON, which inflated tool turns by the
  wrapper.

## Gate results (this slice)

- `go build ./...` clean; `go vet` clean on touched packages;
  `gofmt -l` clean on touched files.
- New tests: `internal/tokencount` (corpus validity, drift stats,
  wire-count regressions), `internal/ledger` (drift column
  storage, `DriftSummary`, migration), `internal/router`
  (`TestTokenCountAudit`, 90 samples / 30 cells;
  `TestWireForFindsKeyedNativeClient`, sensitivity-proven),
  `internal/server` (usage drift feed + empty state).
- Full suite `go test -count=1 ./...` (log
  `$HOME/.tmp/full-gate-229-230.log`): **39/39 packages ok, 0
  fail**; verbose recount (log
  `$HOME/.tmp/full-verbose-229-230.log`): **2,000 PASS events
  (1,587 top-level + 413 subtests), 1 FAIL** —
  `TestTrainingPairs` in `internal/feedback`, a pre-existing
  ordering flake (map-iteration order under parallel load;
  passes 3/3 in isolation, untouched here).
- `go test -race` clean on all touched packages (tokencount,
  ledger, router, provider, server, cmd).
- Live smoke `scripts/smoke-token-count-audit.py` (real binary +
  3 Python mocks): **16/16 green** (second run; the first
  run's T0c 502s were the mocks being probed before they
  finished starting under a concurrent slice's full-suite run —
  mock readiness waits added, Bedrock key format fixed to the
  docs-example `AKID:SECRET` pair).
- Dashboard JS: `node --check` on the extracted inline script
  clean; "Playground" absent (0 occurrences).
- **Live-smoke finding (real bug, fixed in this slice):**
  `Engine.wireFor` did a bare-ID lookup on a clients map keyed
  by (provider, key) identities, so it missed every provider
  with a key pool and the estimator silently fell back to the
  OpenAI-reference framing on native wires — +44.44% on a
  two-message Anthropic/Bedrock request in the first smoke run
  (local 26 vs reported 18). Fixed (`wireFor` scans for any
  keyed client of the provider; the wire is a property of the
  provider, not the key) with a sensitivity-proven regression
  test; the smoke then re-measures 16/16 green (see
  `docs/token-count-audit.md`, "What the live smoke caught").
- **Stale expectations updated (this slice's change, not other
  slices' code):** 10 pre-existing tests encoded the old
  text-only estimate (3 tokens for "hello world", 0 for the
  local-estimate field on reported rows) and failed once the
  estimator counted framing + tools. Updated to the counter the
  engine actually uses (`CountPromptFull` / the per-wire count):
  `TestBudgetGateEstimateMath`, `TestRequestBudgetFailFastZeroCalls`,
  `TestRequestBudgetStrictStopsMidChain` (comment),
  `TestContextFitMathBoundaries`, `TestContextFitNoFitFailsHonestly`,
  `TestChatLedgerAndTokens`, `TestReconcileUsageFallbackEstimate`,
  `TestReconcileUsageKeepsReported`, `TestReconcileUsageDivergence`,
  `TestReconcileUsageSmallDifferenceNotMaterial`,
  `TestStreamSuccessRecordsLocalEstimate`,
  `TestMeStatusAfterTraffic`, `TestRequestBudgetPreSend402` — all
  pass; no production behaviour was changed to satisfy them, the
  new numbers are the audited ones (9 tokens for "hello world":
  3 text + 3 framing + 3 priming).

## Honest limitations

- **Vocabulary drift is unmeasured.** The mocks tokenize with
  the same BPE the router uses, so the audit measures coverage
  drift (what text the estimator ignores), not the difference
  between tokenizers. A real Anthropic/Gemini/Cohere/Bedrock
  tokenizer can split the same text differently; the live drift
  view is the tripwire for it.
- **Streaming is not audited.** The engine records streaming
  attempts as unknown usage, so there is no reported number to
  compare.
- **Residuals ≤ 1 token per request** remain on system-message
  requests (+2.98% worst cell, Anthropic/Gemini/Bedrock
  english) and on Anthropic/Gemini tool reshaping (+5.01% /
  +2.52%, safe direction). Deliberately left: chasing the last
  token would overfit the estimator to this mock.
- **Imported rows carry no estimate.** `import.go` uses an
  explicit column list, so DB-imported rows get
  `local_estimate_prompt_tokens = 0` ("not measured") and are
  excluded from drift, never treated as agreement.
