# Phase 2 — Completion Ledger

Scope: the Phase 2 final execution directive (Master Prompt v1) against
`github.com/raunakiitp/CodvianRouter`. Baseline `main` = `7cdf81c` (PR #26).
Every row's status is evidence-based and carried in `docs/PHASE2_LEDGER.md`.

| Task | Status | Evidence / location |
|---|---|---|
| P2-T1 (carry-over G1/chat splits) | COMPLETE | Responses streaming usage + combo/pipeline accounting regression tests |
| P2-T2 (engine/AI parity) | COMPLETE | carried from PR #26; verified on tree |
| P2-T3 (controller compatibility) | COMPLETE | carried; conformance suites |
| P2-T4 (tool-calling metadata) | NOT_IMPLEMENTED (honest gap) | no trustworthy upstream capability source; documented deferral, never fabricated |
| P2-T5 (live routing depth) | COMPLETE | trace reason/role labels + node regression test |
| P2-T6 (analytics accuracy) | COMPLETE | PR #27: reported-cost reconciliation + ImportEvents fidelity fix |
| P2-T7 (observability) | COMPLETE | PR #28: usage/reported-cost metrics + provider-health webhook events |
| P2-T8 (dashboard premiumization) | COMPLETE (wiring verified) | budgets/teams/settings on real APIs; a11y residual documented |
| P2-T9 (semantic cache) | COMPLETE | cross-tenant/temp isolation + streaming proof |
| P2-T10 (routing parity) | COMPLETE | PR #29: 35 strategies, distinct/permutation/unknown tests |
| P2-T11 (pricing/quality signals) | COMPLETE | models.dev/LiteLLM sync (OFF default) + real feedback-learned loop |
| P2-T12 (eval harness) | COMPLETE | PR #30: 8 permanent regression cases |
| P2-T13 (performance campaign) | COMPLETE (procedure) + fresh re-measure deferred | rig is Linux-bound; blocked on this Windows host, runs in CI |
| P2-T14 (hardening verification) | COMPLETE | PR #31: PHASE2_SECURITY_REVIEW.md, all gates green |
| P2-T15 (consolidated verification + final report) | COMPLETE | this ledger + PHASE2_VERIFICATION_REPORT.md |

## Merged Phase 2 PRs (all into main, CI green)
- #27 (T6), #28 (T7), #29 (T10), #30 (T12), #31 (T14). Carried Phase-1
  merge history (#19–#26) unchanged.

## Deferred / blocked with evidence
- T4: tool-calling capability metadata — needs a trustworthy upstream
  capability signal (models.dev modality sync) before per-model
  `supports_tools` can be added without inventing data.
- T13 fresh re-measure: `scripts/bench.sh` requires Linux `flock` +
  `/proc/loadavg`; this dev host is Windows/git-bash, so the rig's own
  lock gate refused the run. Historical published records are preserved
  in `docs/benchmark-*.json`; fresh numbers are produced by the Linux/CI
  environment the rig targets. No local numbers were fabricated.

## Genuine bugs found and fixed this phase
- T6: `ledger.ImportEvents` silently dropped `ReportedCostUSD` and
  `Cancelled` on every imported row (reported billing never reconciled;
  hedge losers vanished from cancelled counts). Fixed to mirror
  `normalizeUsage`; regression-locked.
- T7: `/metrics` had no usage/cost series, and webhook event coverage
  had only quota/budget. Added usage/reported-cost metrics (fixed kind
  vocabulary, sanitized labels) and edge-triggered
  provider.degraded/provider.recovered events from genuine breaker
  transitions. All regression-locked.