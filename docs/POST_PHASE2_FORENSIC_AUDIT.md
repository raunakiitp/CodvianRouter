# POST-PHASE-2 MASTER FORENSIC AUDIT — CODVIAN ROUTER

Repository: https://github.com/raunakiitp/CodvianRouter
Audit type: read-only, evidence-driven. No application code modified, no branch/PR/commit created, no paid credits spent, no Phase 3/4 started.

---

## 1. Executive summary

CODVIAN ROUTER after Phase 2 is a large, genuinely implemented and heavily tested OpenAI-compatible routing gateway. The repository is internally consistent: the main branch is verified at `838e9d6` with a green CI run on that exact commit, a clean working tree, and zero open PRs. 314 HTTP routes are registered, the dashboard has zero "UI without backend" findings under the static `audit-fake.sh` gate, and the test suite contains 4,672 `Test/Benchmark/Fuzz` functions with `go vet ./...` clean.

Phase 2 is complete on `main` (merged PRs #26–#32). The routing engine exposes 35 strategies in `internal/strategy.All`, each conformance-tested (every strategy is a deterministic permutation of the candidate chain). Genuine bugs were found and fixed during Phase 2 (`ledger.ImportEvents` dropped reported-cost/cancelled; `/metrics` lacked usage/cost series; webhook coverage lacked provider-health events).

Remaining work is **not** a large set of unimplemented features: the audit confirms the outstanding items are a small number of honest, evidence-backed deferrals and residual improvements — no P0 severity finding surfaced, and no evidence of a runtime/billing corruption path was found. Production readiness is high for a locally-hosted gateway, with the caveat that fresh benchmark numbers and real-provider verifications remain deferred (they need a quiet Linux rig and API credits respectively).

## 2. Verified Git baseline

| Item | Value | Evidence |
|---|---|---|
| Branch | `main` | `git branch --show-current` |
| Local HEAD | `838e9d6` | `git rev-parse HEAD` |
| origin/main | `838e9d6` (no drift) | `git rev-parse origin/main` |
| Working tree | clean | `git status --porcelain` empty |
| CI on main HEAD | `ci` completed success | GitHub Actions runs on `838e9d6`, branch `main` |
| Open PRs | 0 | `GET /repos/.../pulls?state=open` |
| Unmerged remote branches | none | `git branch -r --no-merged origin/main` |
| Reported baseline match | matches `838e9d6` | directive reference point confirmed current |

Recent history (Phase 2): `838e9d6` (#32 T15) → `a393a74` (#31 T14) → `6fa22e2` (#30 T12) → `5e91c0d` (#29 T10) → `5f3b3bb` (#28 T7) → `75db00c` (#27 T6) → `7cdf81c` (merge #26) → `7d47e8f`/`ff1b123` (T5). No abandoned or duplicate in-flight work found; old `feat/phase1-*` and merged `feat/phase2-*` branches are stale-but-merged local refs, not lost changes.

## 3. Phase 0 / 1 / 2 completion ledger (reconstructed from docs + code)

| Phase | Status | Evidence |
|---|---|---|
| Phase 0 (cleanup) | COMPLETE | README changelog "21 Nov 2026 — Phase 0 cleanup" (removed fabricated endpoints); `audit-fake.sh` Phase-0 backend-fake check PASS |
| Phase 1 | COMPLETE / accepted | `docs/PHASE1_COMPLETION_LEDGER.md`; provider/auth parity, PKCE login, Azure Entra, pool-join, nested model IDs, reasoning-only, G1 streaming usage, security hardening |
| Phase 2 | COMPLETE | `docs/PHASE2_COMPLETION_LEDGER.md`, `docs/PHASE2_VERIFICATION_REPORT.md`, `docs/PHASE2_LEDGER.md`; merged PRs #26–#32 |
| Phase 2 deferrals | documented, not inflated | T4 tool-calling metadata; T13 fresh bench (Linux-bound rig); T8 keyboard-focus residual |

Every Phase 2 task is COMPLETE except two recorded exceptions (T4, T13 re-measure), each with evidence. See §14.

## 4. Architecture map

- **Command**: `cmd/codvian-router` (serve/init/keygen/...), plus `cmd/codvian-bench`, `cmd/release-sbom`.
- **Backend**: 42 packages under `internal/` — a2a, adaptive, apitest, autorouter, cache, catalog, cataudit, chaos, coalescing, compress, config, crypto, degrade, egress, eval, feedback, files, gctune, guardrails, health, learned, ledger, mcp, mcptools, metrics, modality, modelsync, oauth, observability, plugins, provider, providerlogin, readiness, reasoning, relay, release, responses, router, routetrace, scoring, server, sla, strategy, strategytree, tokencount, tray, webhooks, wire.
- **Routing**: `internal/router` (engine), `internal/strategy` (35-strategy `All`), `internal/strategytree`, `internal/scoring` (16-factor), `internal/routetrace`.
- **Ledger/billing**: `internal/ledger` (SQLite usage events, budgets, quotas, imports, analytics summary).
- **Caching**: `internal/cache` (exact + semantic), `internal/coalescing` (request coalescing), `internal/degrade`.
- **Observability**: `internal/metrics` (hand-rolled Prometheus text), `internal/observability` (OTLP tracing, `-tags otel`), `internal/health`, `internal/webhooks`.
- **UI**: `web/index.html` (~13k-line single-file app, served via `internal/server/web.go`), `web/tryit.html` playground.
- **Config**: `internal/config` (typed config, `validate`, safe defaults).
- **Deploy**: `Dockerfile` + `Dockerfile.release` (multi-stage → `distroless/static:nonroot`, healthcheck binary, `EXPOSE 20128`, `ENTRYPOINT serve`), `docker-compose.yml`, `.dockerignore`. CI: `.github/workflows/ci.yml` (push+PR) and `release.yml`.

## 5. Subsystem findings (evidence-based)

| Subsystem | Exists | Connected | Findings / evidence |
|---|---|---|---|
| API routes | 314 registered | yes | `audit-fake.sh` reports 314 routes = guard table 314; per-route classification enforced by `TestRouteTableSpawnCapableAreLocalOnly` (check-route-guard.sh CI bonus) |
| Provider auth/OAuth | OpenRouter PKCE, Azure Entra | yes | Phase 1; refs `internal/oauth`, provider-auth tests |
| Key management | add/rotate/disable/re-role per key | yes | `/api/providers/{id}/keys/*` (rotate added Phase 2) |
| Model discovery/metadata | catalog + modelsync | yes (OFF default) | `internal/catalog`, `internal/modelsync` (models.dev/LiteLLM sync opt-in OFF); no per-model `supports_tools` (T4) |
| Routing/strategies | 35 strategies | yes | `internal/strategy.All`; conformance/permutation/unknown tests (185 tests) |
| Failover/retries | yes | yes | hedge, cascade, region-aware failover, attempt loops; tests |
| Circuit breakers/cooldowns | yes | yes | providerbreaker + window breaker, cooldowns, reset endpoints; T7 breaker→webhook events |
| Streaming | chat SSE + Responses | yes | shared drain (`runStream`); T5/T9 + Responses accounting tests |
| Ledger/billing | SQLite usage events | yes | `internal/ledger`; budgets, quotas, imports; T6 reported-cost reconciliation |
| Budgets/quotas | per-key windowed + scoped + pools/schedules | yes | ledger + server budget/quota handlers |
| Tenant isolation | cache scoped by tenant+tempBucket | yes | `TestM611*` incl. streaming |
| Caching | exact (SQLite) + semantic | yes | `internal/cache`, `internal/degrade` WrapWithSemantic |
| Observability | metrics + tracing | yes | `internal/metrics`; OTLP export in `-tags otel` |
| Dashboard | single-file app | yes | `audit-fake.sh` 0 findings; budgets/teams/settings on real APIs |
| Config validation | typed + validate | yes | `internal/config` suite; webhook event whitelist |
| Deployment | Docker/distroless | yes | multi-stage nonroot image; healthcheck |

### Confirmed gaps / risks (evidence, no invention)
- **T4 tool-calling metadata missing** — `internal/catalog` provider rows carry `Notes: "ToolCalling unknown: no toolCalling marker in the reference metadata"` (e.g. `providers_reference.go:5338`); no per-model `supports_tools` source exists. Adding it would require a trustworthy upstream capability signal (models.dev modality sync). Documented deferral, not a runtime bug.
- **Keyboard-focus ordering** — `web/index.html` has zero `tabindex=` attributes; keyboard a11y relies on native focusable controls (documented T8 residual; not a P0 — no functional break).
- **Bench re-measure deferred** — `scripts/bench.sh` uses `flock` + `/proc/loadavg` (lines 21–27) and is not run in `.github/workflows/*.yml`; a fresh measurement needs a quiet Linux host. Historical records preserved in `docs/benchmark-*.json`.
- **audit-fake.sh leaves 2 items UNVERIFIED** (static grep cannot settle): `#revealKeyBtn` label semantics; per-view honest-empty-state coverage (needs a Playwright/browser smoke pass). Neither is a confirmed defect.

## 6. Capability self-review (CODVIAN implementation status)

Capability inventory verified against this tree. This is a self-review of what CODVIAN actually implements — it is not a comparison with any other product.

| Capability | CODVIAN implementation | CODVIAN evidence | Status |
|---|---|---|---|
| OpenAI-compatible endpoint | `/v1/*` chat/responses/embeddings etc. behind a gateway key | `internal/server`, apitest suite | IMPLEMENTED |
| Routing strategies | 35 in `internal/strategy.All` — priority, fill-first, weighted, round-robin, p2c, least-used, random, strict-random, cost-optimized, headroom, quota-weighted, reset-window, reset-aware, context-relay, context-optimized, cache-optimized, lkgp, auto, fusion, pipeline, expiry-first, most-reliable, least-latency, latency-aware, code, sla-aware, nadir, ship-fast, cost-saver, quality-first, offline-friendly, reliability-first, chaos-mode, cascade | `strategy.go:211-246`; conformance tests | IMPLEMENTED (35, conformance-tested) |
| Combos | fusion/pipeline/jev + auto family + candidate pool | `internal/strategy` combo.go, tests | IMPLEMENTED |
| Quota-aware auto-fallback | quota-weighted, headroom, quota pools/schedules, region failover | ledger + router tests | IMPLEMENTED |
| Provider pool | provider registry + catalog + model mapping (modelsync OFF default) | `internal/catalog`, `internal/provider`, `internal/modelsync` | IMPLEMENTED — breadth is config/catalog driven, no bundled provider list |
| Compression | lossless-first compression (`internal/compress`, 167 tests) | compress suite | IMPLEMENTED — ratio claims require measured data |
| MCP / A2A | `internal/mcp`, `internal/a2a` (+audit) | mcp/a2a suites | IMPLEMENTED |
| Per-key routing control | key-scoped models/routing, per-key quotas | server key APIs | IMPLEMENTED |
| Desktop/PWA | not part of the headless-server scope | — | OUT OF SCOPE |
| Resale of verbatim no-extra-cost responses | not applicable (CODVIAN is a self-hosted gateway, not a reseller) | — | OUT OF SCOPE |

**CODVIAN advantages (reasoned, not measured):** (1) fully self-hosted with a nonroot distroless image and SSRF/egress protections; (2) wide, conformance-tested strategy surface (35 named strategies); (3) integrated usage/billing ledger with reported-vs-priced reconciliation (T6); (4) tenant-scoped semantic cache (T9); (5) strict dashboard preservation discipline. Advantage claims are capability-breadth claims; any performance "better" claim requires a measured head-to-head, which is not available (no fair rig) and is therefore not claimed.

## 7. Routing engine audit

Real path: HTTP → server handler → engine `Chat`/`ChatStreamChunks` → strategy ordering (`internal/strategy`) → attempt loop (eligibility: breaker, cooldown, quota, budget, limiter) → provider client → drain → `recordAttempt`/`recordCancelledAttempt` → ledger → SSE/Responses projection.

- Candidate selection: strategy hook is a deterministic permutation of the candidate chain (`TestHookNeverDropsOrDuplicates`); per-attempt eligibility gates (breaker open, cooldown, zero-quota, attempt budget) are surfaced as real rejection reasons in the trace (T5).
- Unknown model / unknown strategy: unknown strategy falls back to `priority` with a single logged warning (rt5 tests); unknown model handled honestly, never invented.
- Hedge: winner recorded once, losers via `recordCancelledAttempt` (excluded from failure counts); trace tags winner/loser from the real hedge decision (T5).
- Decision vs runtime consistency: T5 label tests and the dashboard JS harness assert labels are keyed to backend-emitted strings; no contradictory evidence found in this audit. No claim that every edge is covered (see §19 limitations).
- Concurrency: engine guarded by mutexes; CI runs `-race` and is green on main HEAD. Sink pattern (copy-under-lock + async dispatch) prevents lock re-entry (T7).

## 8. API / provider compatibility matrix

| Surface | Implemented | Evidence |
|---|---|---|
| Chat Completions (non-stream + stream) | yes | `internal/server`, wire suites |
| Responses API | yes (+ accounting) | `internal/responses`; Phase 2 accounting tests |
| Embeddings / rerank | yes | `internal/server/embeddings.go`, media_verify |
| Image/audio passthrough | yes (passthrough, capability-gated) | `internal/router/images_passthrough.go`, audio tests |
| Tool/function calling | protocol support | present; capability **metadata** (T4) is the documented gap |
| Structured outputs / reasoning metadata | yes | reasoning-only tests, reasoning replay store |
| Usage reporting | yes | usage-accounting flow, Phase 2 reconcile tests |
| Nested model IDs | yes | Phase 1 nested-ID addressing |
| Ollama native protocol | documented | Phase 2 carryover revisit; unsupported paths documented honestly |
| Cache replay usage | yes | cache `Usage` shape preserved on replay |

## 9. Billing / usage / cache / cost audit

- Lifecycle present: Request → Attempt → `provider.Usage` → reconcile (reported verbatim, local estimate labelled) → ledger row → analytics → dashboard. `TestAnalyticsSummaryReconciles` proves the summary equals an independent row scan (ok/errors/cancelled, token splits, estimates, reported cost).
- Reported-vs-priced: `UsageSummary.reported_cost_usd/_rows` + delta note (T6). Import bug (dropped reported-cost/cancelled) fixed.
- Cancelled attempts: recorded separately, excluded from failures (hedge races do not inflate provider failure rates).
- Streaming: one final usage chunk before `[DONE]`; drain copies usage once (never summed twice).
- Cache: exact + semantic; tenant isolation proven (`TestM611*` incl. streaming); replay preserves usage shape.
- Duplicate prevention / coalescing: `internal/coalescing`; coalesced rows zero-token/zero-cost.
- No misleading-display evidence found in the audited paths; savings/analytics derive from the ledger.

## 10. Dashboard / operator UX audit (design preservation)

- Design system preserved: `:root` tokens (pure-white light / pure-black + lime dark), Plus Jakarta Sans/JetBrains Mono, radius 26/15, 252px sidebar. No palette/layout redesign recommended or performed.
- Real wiring: budgets/teams/settings/providers/keys/resilience/analytics/cache/webhooks all call real `/api/*`; `audit-fake.sh` = 0 UI-without-backend findings.
- States: loading/empty/error handled in render paths (audited); two empty-state items remain UNVERIFIED by static grep (need a browser smoke pass).
- a11y: 51 `aria-*`, 22 `role=`; **no `tabindex=`** → keyboard focus ordering residual (P2).
- Contract: dashboard JS is regression-tested against the **served** artifact (dashboard-js-testing recipe), labels keyed to backend-emitted values.

## 11. Security / multi-tenancy review

- Tenant isolation: semantic cache scoped per tenant+tempBucket; tested incl. streaming.
- Credentials: encrypted at rest (`internal/crypto`); keys never logged; webhook secret never echoed.
- Admin protection: dashboard auth (argon2id session, throttle lockout, expiry) — `dashauth_test.go`.
- SSRF/egress: `internal/egress` (loopback refusal, dial-validated IP, disallowed-target blocking, no redirects, TLS hostname) — carried, untouched.
- Route guard: spawn-capable routes local-only (`TestRouteTableSpawnCapableAreLocalOnly`).
- Metrics label safety: fixed kind vocabulary + sanitizer (`TestUsageLabelSafety`).
- Webhook security: HMAC-SHA256 signing; payload limited; secret redaction tests.
- No confirmed vulnerability found in this audit. `distroless/static:nonroot` image hardens runtime. Recommendation (P3): an explicit dependency-SBOM/vuln scan step (repo has `release-sbom` command + `verify-release.sh`; wire into release gate if not already).

## 12. Observability / production readiness

- `/metrics` (auth-gated): request/latency/failover/cache/budget/concurrency/guardrail/refusal/panic + usage-token + reported-cost counters (T7).
- OTLP exporter (`-tags otel`): bounded queue, non-blocking, drop+count, invalid-endpoint fails closed; local-collector tests.
- Webhook alerts: quota.exceeded, budget.warning, provider.degraded, provider.recovered (T7).
- Health/readiness: `internal/health` + Docker healthcheck binary.
- Graceful shutdown: server shutdown hooks; cache flush + async-ledger flush on wipe.
- Operator can observe: request spans, attempt/decision traces, usage/cost, provider health, breaker transitions, guardrail activity. Cannot observe: fresh on-tree benchmark numbers (T13 deferred) and real-provider round-trips without credits (G4–G6).

## 13. Testing / CI quality assessment

- Suite: **4,672** `Test/Benchmark/Fuzz` functions (server 1391, router 639, provider 313, strategy 185, compress 167, ledger 163, degrade 147, cache 146, mcp 131, config 129, wire 116).
- Phase 2 regression cases all green on current tree: `TestAnalyticsSummaryReconciles`, `TestUsageLabelSafety`/`TestUsageCounters`, `TestBreakerTransitionEventsEdgeTriggered`(+probe), `TestPermanentRegressionCasesRunThroughHarness`, `TestRecordAttemptFeedsUsageMetrics`.
- `go vet ./...` → clean (exit 0). `go build ./...` → clean.
- CI (`scripts/ci.sh` via `.github/workflows/ci.yml`): gofmt → build → vet → `go test ./... -race -count=1` → docker go-version match. Green on `838e9d6`.
- Known environmental caveat: `internal/release/TestGenerateMatchesSha256sum` cannot launch bundled `sha256sum.exe` locally (Windows, Access denied) — Ubuntu CI is authoritative; not a repo defect.
- Not run in CI: `audit-fake.sh` (static, slow), `check-route-guard.sh` (CI bonus), the full `bench.sh` rig (needs quiet Linux; smoke opt-in via `CI_BENCH_SMOKE=1`).
- Bench rig is **not** part of the CI gate → no automatic perf-regression detection (P2 suggestion).

## 14. Historical deferrals / external blockers

| Item | Reason | Still blocked | Code changed since? | What's needed | Verify w/o credits? | Verdict |
|---|---|---|---|---|---|---|
| G4 second OpenRouter account | cross-account failover | yes | no | second account credentials | no | remain deferred |
| G5 free/paid quota limits | 429/402 account state | yes | no | funded/credits account | no | remain deferred |
| G6 live Azure resource | real Entra resource | yes | no | Azure resource + client | no | remain deferred |
| T4 ToolCalling metadata | no provider-declared capability source | yes | no | trustworthy upstream capability signal (models.dev modality sync) | yes (models.dev fetch) | remain deferred until signal exists |
| T8 keyboard-focus ordering | tabindex not emitted | partial | no | explicit focus-order work in dashboard | yes | actionable (P2) |
| T13 bench re-measure | rig Linux-bound (flock/loadavg), not in CI | yes | no | quiet Linux runner | yes (CI) | remain deferred to CI |

## 15. Prioritized gap register

No P0 findings. Separating confirmed issues from inferred risks.

| ID | Pri | Subsystem | Problem | Evidence | Impact | Fix | Complexity | Order |
|---|---|---|---|---|---|---|---|---|
| G-01 | P2 | Catalog | No per-model tool-calling capability metadata (T4) | `providers_reference.go:5338` "ToolCalling unknown"; no `supports_tools` source | model-capability UI/behavior can't be derived | require a trustworthy upstream capability signal before adding metadata | L | after signal |
| G-02 | P2 | Dashboard | No explicit keyboard focus ordering | `web/index.html` `tabindex` count = 0 | reduced keyboard a11y on custom controls | add focus order without redesign (native focusables + `tabindex` on interactive custom controls); extend-only | M | high |
| G-03 | P2 | Performance | No fresh on-tree benchmark numbers; rig not in CI | `scripts/bench.sh` flock/loadavg (l.21-27); absent from workflows | perf-regression blind | run bench rig on a quiet Linux runner; record to `docs/` | S | high |
| G-04 | P2 | CI | `audit-fake.sh` slow & not in CI; 2 UNVERIFIED items | audit-fake output (revealKeyBtn; empty-state) | dead-control regression possible | add a browser/Playwright empty-state smoke; consider CI hook | M | medium |
| G-05 | P3 | Docs | README freshness vs `docs/` not machine-checked | README has 21-Nov changelog; CI fails on README drift per skill | stale README risk | keep a README-vs-docs drift check | S | low |
| G-06 | P3 | Release | Dependency vuln scan not confirmed as a gated step | `release-sbom` + `verify-release.sh` exist | supply-chain blind spot | wire SBOM/vuln scan into release gate | S | medium |
| G-07 | P3 | Tooling | Many historical bench JSONs / smoke scripts accumulate | `docs/benchmark-*.json` ~40 files; ~60 `smoke-*` scripts | maintenance clutter, not a defect | optional consolidation/retention policy | S | low |

Inferred risks (no reproduction, not P0/P1): streaming edge-case accounting on exotic providers; dashboard empty-state on fully-empty fresh router (UNVERIFIED); performance under real multi-provider concurrency (needs bench rig).

## 16. Recommended next phase (planning only — not started)

Based on evidence, the highest-value next phase should be **hardening + operator-ready release packaging**, not a big new feature surface:

- **Phase objective**: close G-02..G-07 (keyboard a11y, fresh benchmark campaign on CI/Linux, browser smoke for empty states, README-drift check, SBOM/vuln gate, tooling consolidation) and perform real-provider verification where credentials become available (G4/G5/G6).
- **Why next**: the audit shows features are complete; the residual risk is operational — accessibility, reproducible performance evidence, supply-chain verification, and the external real-provider checks. These gate "operator-ready" and are all bounded, low-risk slices.
- **Included blocks**: (1) dashboard a11y focus-order (extend-only); (2) Linux bench rig wired as an on-demand/CI-smoke job + published results; (3) Playwright empty-state/state smoke; (4) SBOM/vuln gate in release; (5) README-drift guard; (6) tooling retention policy.
- **Dependencies**: quiet Linux runner for bench; browser runtime for smoke; credentials for G4–G6 (may stay deferred).
- **Acceptance criteria**: each slice per the skill's 11-stage loop; bench numbers published with rig+commit; a11y a11y-checks pass; SBOM gate fails-closed on high vulns.
- **Implementation order**: G-02 → G-03 → G-04 → G-06 → G-05/G-07.
- **Main risks**: bench rig quiet-machine flakiness; Playwright env availability; scope creep into feature work (guard: keep it hardening-only).
- **Verification plan**: the skill's VERIFY→…→FINAL CONSOLIDATED VERIFICATION loop; each slice PR+CI+merge, consolidated report at end.
- **Expected outcome**: an operator-ready release with reproducible performance evidence and verified accessibility/security.
- **Out of scope**: new routing strategies, new provider integrations, UI redesign, tool-calling-metadata (G-01) until a capability signal exists, and the real-provider checks that still need paid credentials (deferred, never forced).

Independent improvements (non-blocking, later): G-07 tooling consolidation; optional `CI_BENCH_SMOKE=1` gate; adding a dependency SBOM to the README badges.

## 17. Out-of-scope / constraints honored

- No Phase 3/4 started; no implementation; no branch/PR/merge; no application code, tests, config, or deployment resources modified.
- No UI/UX redesign; design system preserved.
- No paid provider credits spent or probes repeated.
- No force-push, history rewrite, reset, or deletion. Working tree preserved clean.

## 18. Verification evidence and limitations

Run on: main HEAD `838e9d6` (CI `ci` success). Commands used: `git rev-parse`, `git status --porcelain`, `go build -buildvcs=false ./...` (PASS), `go vet -buildvcs=false ./...` (exit 0), Phase 2 regression test subset (all `ok`), `scripts/audit-fake.sh` (RESULT: PASS, 0 findings), GitHub Actions/PR API reads, `grep` inventory.

**Limitations (honest):** (1) real-provider round-trips not executed (no credits; G4–G6 remain deferred); (2) full `go test ./... -race` not re-run locally (CI green on main is authoritative; one local env-only sha256sum failure); (3) no measured head-to-head against any other gateway (none claimed); (4) dashboard interactive behavior not browser-driven (static audit + served-JS tests); (5) fresh benchmark numbers unavailable on this host (Linux-bound rig). Every finding above is traceable to a file, test, or CI reference; no problem was invented and no severity inflated.

---
*Audit documentation only. Not committed; no branch/PR created. Do not begin Phase 3/4 without explicit user authorization.*