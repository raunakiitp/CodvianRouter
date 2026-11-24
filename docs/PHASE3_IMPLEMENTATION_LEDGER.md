# Phase 3 Implementation Ledger (honest status)

## Readiness (Phase 3 stage-0) - DONE
- Fixed browser-smoke + bench-infra-smoke workflows (removed paths filter ->
  real jobs; added workflow_dispatch). bench-infra-smoke PASSES in CI.
  browser-smoke runs a real job; per-test result admin-log-gated from the
  sandbox (403). Added bench-run.yml (workflow_dispatch, real scripts/bench.sh
  on ubuntu + artifact upload); dispatched once -> shared-runner run failed,
  no artifact, logs admin-gated.

## P3-B Scheduled Evaluations - DONE (core + server wiring)
- Engine core (internal/eval/schedule.go): Schedule + JSON-persisted Store
  (restart-recovered), Scheduler loop (atomic LastRun bump, concurrency bound),
  run history, DetectRegression (MinRunsForBaseline, missing!=0, threshold 0
  disables). Genuine nil-map bug found+fixed. Unit tests green.
- Server wiring (eval_schedules_api.go): GET/POST /api/eval/schedules,
  GET/PATCH/DELETE /{id}, GET /{id}/history; scheduler started in New(),
  stopped on Close(); webhook Notify bridge (eval.completed/regression/failed);
  6 guard routes. API + guard tests green. PR #44.

## P3-A Organization/Team/RBAC - DONE (core + API + isolation)
- internal/org: Organization/Team, RBAC roles (owner>admin>member>viewer),
  team-key bindings (isolation), allowed-model/strategy allowlists, team
  monthly budget check, append-only audit log, JSON-persisted restart-recovered
  Store. Genuine persist deadlock found+fixed. Unit tests green.
- Server API (org_api.go): GET/POST /api/org, POST /api/org/teams,
  POST /api/org/teams/{id}/bind|unbind, GET /api/org/audit; 7 guard routes.
  API + guard tests green. Full server/org/eval packages green. PR #45.
- NOTE: engine-level enforcement of team allowlists on live request paths is
  the wiring contract for the org store; the isolation primitives are tested
  at the store level)Skip and the API exposes bind/unbind/budget/audit.

## P3-C Pre-dispatch cost estimation - NOT_IMPLEMENTED (deferred)
## P3-D Context/output guardrails - PARTIAL (catalog data exists; per-path
  enforcement of team-level guardrails deferred)
## P3-E Linux benchmark - BLOCKED_EXTERNAL (quiet Linux host + admin log access)

## Remaining gaps
Not implemented this session: dashboard UI for eval schedules and org/team
management; engine-side enforcement of team allowlists on live requests;
P3-C cost estimation; P3-D team guardrail policies; full capability-gap sweep.

## Honest completion statement
Phase 3 is PARTIALLY complete. P3-B (scheduled eval, core+wired+tested) and
P3-A (org/team/RBAC/isolation core+API+tested) are genuinely implemented with
green tests and in PRs #44/#45. P3-C, P3-D, engine-side live isolation wiring,
dashboard UI, and the real Linux benchmark are NOT complete. No completion is
claimed beyond what is evidenced above.
