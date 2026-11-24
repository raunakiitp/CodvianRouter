# Scheduled Evaluation Dashboard (P1.9)

## Rule

The evaluation scheduler is preserved as it was; the dashboard grows a
**pass-rate trend** and **failed-case detail** on top of it. Nothing here
turns a mock run into a real-provider benchmark, and a failed run is
never read as a 0% quality score.

## Audit: what already existed (kept, not rebuilt)

| Piece | Where | Status |
| --- | --- | --- |
| Harness (substring+latency scoring, honest "not a quality score" doc) | `internal/eval/eval.go` | IMPLEMENTED |
| Schedule model + validation + `Due` | `internal/eval/schedule.go` | IMPLEMENTED |
| Store: in-memory or JSON-file persistence, restart recovery | `internal/eval/schedule.go` (`Open`, `persist`) | IMPLEMENTED |
| Regression detection (baseline, `MinRunsForBaseline`, drop threshold) | `Store.DetectRegression` | IMPLEMENTED |
| Concurrency bound + duplicate-run prevention (`inFlight`, LastRun bump under lock) | `Scheduler.tick` | IMPLEMENTED |
| Webhook notifications (`eval.completed` / `eval.regression` / `eval.run.failed`) | `Scheduler.execute` → `Server` `Notify` | IMPLEMENTED |
| Real-vs-mock engine choice (`UseRealEngine`, default mock) | `Scheduler.execute` | IMPLEMENTED |
| REST API: list/create/get/patch/delete/history | `internal/server/eval_schedules_api.go` | IMPLEMENTED |
| Authorization: guard rows for every eval route (admin scope) | `internal/server/guard.go` | IMPLEMENTED + this delivery adds the trends row |

## Confirmed gaps fixed

1. **Failed-case details were missing.** `Run.FailedCases` stored only case
   IDs, so an operator could see *that* `regression/observability` failed
   but not *why*, and had to re-run the suite to triage. `Run.Failures`
   now records, per failing case, the case ID, the engine/reason text, the
   missing expected substrings, and the latency. `FailedCases` is kept for
   compatibility; the two always agree in length.
2. **Pass-rate trends were missing.** There was no way to see whether a
   schedule was drifting. `GET /api/eval/schedules/{id}/trends` returns the
   chronological series plus a baseline comparison:
   - `points[]` — one per run (oldest first), including **failed** runs so a
     broken run is visible;
   - `baseline_pass_rate` — the mean of the completed runs *before* the
     latest completed one;
   - `latest_pass_rate`, `delta`, `direction` (`up` / `down` / `flat` /
     `unknown`);
   - `completed_runs` — the sample size behind the direction.
   `direction` is `unknown` with fewer than two completed runs: one number
   is a measurement, not a trend. A failed or case-less run never
   contributes to the baseline mean and never becomes the "latest" figure,
   so it is never misread as 0% quality.

## Endpoints (added/changed)

| Route | Tier | Behavior |
| --- | --- | --- |
| `GET /api/eval/schedules/{id}/trends` | MANAGEMENT (admin) | Pass-rate series + baseline delta/direction; `?limit=` default 20; unknown schedule → 404 |
| `GET /api/eval/schedules/{id}/history` | MANAGEMENT (admin) | now also returns `failures[]` per run |

## Honesty guarantees

- **No mock-as-benchmark:** a schedule without `use_real_engine` runs the
  local mock engine (zero credits); the run is recorded exactly as that.
  Real provider-backed evaluation stays explicit opt-in.
- **No fabricated trend:** direction is `unknown` until two completed runs
  exist; a failed run is excluded from the baseline and the latest figure.
- **No silent zero:** a failed run reports its `status` (`failed`) and
  `error`, not a `pass_rate` that reads as 0% quality.
- **Zero paid API usage in these tests:** every test drives a fake or mock
  engine.

## Tests

| Test | Pins |
| --- | --- |
| `TestP19TrendIsUnknownWithFewerThanTwoCompletedRuns` | one completed run → `unknown`, latest set, one point |
| `TestP19TrendDirectionsAndBaseline` | down/up/flat, baseline = mean of prior completed runs, points chronological |
| `TestP19TrendNonCompletedRunsNeverCountAsZero` | a failed run is visible but excluded from baseline/latest |
| `TestP19RunRecordsFailureDetails` | persisted `Failures` carry id + reason/missing; `FailedCases` agrees |
| `TestP19FailureDetailsSurviveRestart` | failure detail reloads after `Open` on the same path |
| `TestP19TrendsEndpointReportsSeriesAndDirection` | 200, direction, chronological points over HTTP |
| `TestP19TrendsEndpointUnknownScheduleIs404` | unknown schedule → 404 |
| `TestP19HistoryCarriesFailureDetails` | history view exposes `failures[]` |
| `TestRouteTableSpawnCapableAreLocalOnly` (updated) | trends route is classified; 347 routes, MANAGEMENT 280 |

## Deliberate limits

- **Provider/model comparisons** are not added: the permanent case set is
  model-tagged, but a single schedule run evaluates one engine, so a
  per-provider comparison would need a new multi-engine run shape. That is
  a separate feature, not a dashboard fix; inventing one here would
  duplicate or fabricate comparison data.
- Trends are computed on read from persisted runs (bounded by `?limit=`);
  no precomputed rollup table is stored. Run history is the source of truth.
- The scheduler remains a single in-process loop by design (no external
  scheduler), which is what makes duplicate-run prevention and restart
  recovery simple and verifiable.