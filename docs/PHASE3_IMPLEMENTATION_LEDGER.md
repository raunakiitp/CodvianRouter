# Phase 3 Implementation Ledger (honest status)

## Readiness (Phase 3 stage-0)
- Fixed browser-smoke + bench-infra-smoke workflows: removed the paths filter that caused
  empty-jobs placeholder failures; added pull_request + workflow_dispatch. bench-infra-smoke now
  PASSES in CI (real job, compile-only bench validation). browser-smoke runs a REAL job; its
  per-test result is admin-log-gated from this sandbox (403 on job logs).
- Added bench-run.yml (workflow_dispatch): executes scripts/bench.sh on ubuntu, records env
  metadata, uploads raw JSON. Dispatched once; the shared runner run failed (no artifact) and
  logs are admin-gated - real Linux benchmark numbers remain EXTERNALLY BLOCKED (need a quiet
  Linux host + log access).

## P3-B Scheduled Evaluations - PARTIAL (core implemented + tested, API unwired)
- IMPLEMENTED + TESTED: internal/eval/schedule.go (Schedule, Store with JSON persistence +
  restart recovery, Scheduler loop with atomic LastRun bump and per-schedule concurrency bound,
  Run history, DetectRegression with MinRunsForBaseline and missing!=0) + schedule_test.go
  (validation, due, persistence round-trip, scheduler records history, regression thresholds,
  concurrency/no-dup). go test ./internal/eval -> ok. PR #41.
- Genuine bug found & fixed: Scheduler.tick panicked on nil inFlight map when called directly;
  lazy-init added.
- NOT YET WIRED: server REST API (/api/eval/schedules CRUD, /history, /regression), scheduler
  goroutine start in server.New, guard-table routes, dashboard view, webhook notification wiring.

## P3-A Organization/Team/RBAC - NOT_IMPLEMENTED (deferred this session)
No org/role model, no RBAC enforcement, no team-key isolation beyond existing team budgets.
Required but not completed in this session.

## P3-C Pre-dispatch cost estimation - NOT_IMPLEMENTED (deferred)
## P3-D Context/output guardrails - PARTIAL (catalog data exists; per-path enforcement deferred)
## P3-E Linux benchmark - BLOCKED_EXTERNAL (needs quiet Linux host + admin log access)

## Honest completion statement
Phase 3 is NOT fully complete. P3-B core is implemented+tested and in PR #41; readiness workflow
fixes are merged. P3-A, P3-B server-wiring, P3-C, P3-D, and the real Linux benchmark remain.
No completion is claimed beyond what is evidenced above.
