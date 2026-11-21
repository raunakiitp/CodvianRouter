# Phase 3 Proposed Scope

| Block | Existing | Gap | Proposed work | Priority | Dependencies |
|---|---|---|---|---|---|
| P3-A | teams budgets enforced (B7) | no org/RBAC management, no team analytics | Team/org management + role-based access on top of existing budget enforcement; team usage analytics | 1 (must) | none in-repo |
| P3-B | eval harness + permanent cases (B8) | no scheduled/historical eval, no regression detection | Scheduled eval run + history capture + regression alerts | 2 (should) | eval harness (exists) |
| P3-C | cache savings (B5) | no token-usage prediction | optional cost-estimation before dispatch | 3 (optional) | pricing signals |
| P3-D | context_window + deprecation (B2) | limits not uniformly enforced | surface context/output-limit guardrails on request path | 3 (optional) | catalog (exists) |
| P3-E | benchmark rig (G-03) | fresh Linux numbers absent | publish a reproducible Linux bench run + results | 2 (should) | quiet Linux runner (external) |

Implementation order: P3-A -> P3-B -> P3-E -> P3-D -> P3-C.
Dependencies: none beyond a Linux runner for P3-E.
Acceptance per block: wired to real request paths, regression tests, served-UI verified, CI green
(skill 11-stage loop). Test strategy: reuse existing suites; add team-authz, scheduled-eval, and
bench-results tests.
Deferred: B3 (learned weights on-by-default - blocked by honest-signal rule), B1 task-complexity
classification (not justified), org-level analytics beyond teams.
Complexity: P3-A L, P3-B M, P3-C M, P3-D S, P3-E S.

## Recommendation
**CONDITIONALLY READY TO AUTHORIZE PHASE 3** - proceed on P3-A and P3-B (the genuine,
architecture-justified gaps), after (a) confirming the two empty-jobs CI workflows on a normal
runner and (b) P3-E requiring a Linux runner for real numbers. Phase 3 must NOT be started
without separate explicit authorization.
