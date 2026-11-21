# Phase 3 Gap Analysis (B1-B8)

Evidence from current source (main 6f0567e), not README/feature names. Statuses:
IMPLEMENTED / PARTIAL / NOT_IMPLEMENTED / IMPLEMENTED_NOT_WIRED / BLOCKED_EXTERNAL / NOT_NEEDED.

## B1 Adaptive Routing Intelligence - IMPLEMENTED
35 strategies (internal/strategy.All), auto/<axis> + 16-factor scoring (internal/scoring,
internal/autorouter), adaptive concurrency limiter (internal/adaptive, Gradient2), breakers +
cooldowns + region failover, attempt budget. Wired into engine/router.go. Conformance tests.
Gap: no per-request task-complexity classification beyond model/variant selection. Proposal: OPTIONAL.

## B2 Provider and Model Intelligence - PARTIAL
Model capability registry (internal/catalog Provider.Models + modality), ContextWindow
(provider/dashboard.go:152), deprecation (internal/catalog/deprecated.go; auto-router rejects
deprecated), models.dev/LiteLLM price sync (internal/modelsync) OFF by default (safe default).
Gap: context/output-limit enforcement on the request path is not surfaced uniformly; price
sync is opt-in OFF (honest, not a defect). Proposal: surface limits consistently + document
stale-price handling (already fails-closed). OPTIONAL.

## B3 Quality and Feedback Intelligence - IMPLEMENTED_NOT_WIRED (by default)
Feedback-learned weights (internal/learned) + /api/feedback loop reach the router
(TestFeedbackWeightsReachTheRouter) but are OFF unless auto_router.feedback_learned_weights
is enabled; learned auto-router OFF by default. Honest: no fabricated learning. Proposal:
only operator enablement/documentation. LOW.

## B4 Advanced Multi-Model Orchestration - IMPLEMENTED
Combo fusion/pipeline/jev (combo.go) + cascade (b2_cascade_selector_test.go) + conditional
model chains (strategy chainstate). Cancellation/hedge/cost controls present. Proposal: NONE.

## B5 Cost and Performance Optimization - IMPLEMENTED/PARTIAL
Cost-aware strategies (cheapest, cost-optimized, cost-saver), budgets + quota pools/schedules,
reported-vs-priced reconciliation (T6), exact+semantic cache with savings via /api/cache/stats.
Gap: no token-usage prediction model; savings are post-hoc, not predictive. Proposal: OPTIONAL.

## B6 Advanced Operator Control Plane - IMPLEMENTED
Dashboard settings/resilience/budgets/teams surfaces wired to real APIs (audit-fake 0 findings),
per-key routing, webhook events, metrics. Proposal: NONE (hardening-only follow-ups).

## B7 Enterprise / Multi-Tenant - PARTIAL
Team budgets enforced (checkTeamBudget in server/a2a.go, audio.go); per-key quotas. No full
org/RBAC management or org-level analytics. Proposal: PHASE 3 candidate (teams/RBAC) but scope
to operator-driven team budget + audit; avoid speculative org features.

## B8 Evaluation and Continuous Intelligence - PARTIAL
Eval harness (internal/eval) + permanent regression cases (T12); eval subcommand. No automated
scheduled model/provider comparison or quality-regression detection. Proposal: PHASE 3 candidate
(run eval on a schedule, capture history) - bounded.

## Excluded (already implemented / unnecessary)
- B4 orchestration (done). B6 control plane (done). B1 strategy breadth (done).
- B2 price sync (safe OFF default is correct; no stale prices claimed).
- B3 enabling learned weights by default (would invent learning signal - not authorized).
