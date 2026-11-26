# Proposed P2 Roadmap — derived from the Post-P1 Forensic Audit

**Status: IMPLEMENTED (WS1–WS5 complete). This proposal is superseded by the P2 implementation branch `p2-implementation`. WS-1 through WS-5 findings have all been implemented with tests and CI gates. See `P2_CHECKPOINT.md` for the current implementation status.**

Every workstream below traces to verified findings in `docs/POST_P1_FINDINGS_LEDGER.md` against `main` @ `08cadc0`. Work already covered by the "immediate maintenance" section of `docs/POST_P1_RECOMMENDATIONS.md` (AUD-SEC-001, AUD-SEC-009, AUD-CODE-001) is **not** repeated here — those are fixes, not a programme.

Sequencing rationale: WS-1 and WS-2 restore *trustworthiness* (verified CI + one identity). WS-3 and WS-4 restore *correctness of money and protocol*. WS-5..WS-8 are capacity, resilience and hygiene work that is only worth doing once the first four are true.

---

## WS-1 — Verification integrity: make "green" mean "executed"

**Problem statement.** Several verification layers exist but never run in CI: the five client-protocol drivers, the `otel` build tag, fuzzing, Redis, Windows, benchmarks; and `govulncheck` is neutralised by `|| true`. A green pipeline therefore does not imply the feature works (AUD-TEST-001/002/003/004/005/006/007/015, AUD-TEST-017).

**Existing implementation.** `scripts/smoke-client-*.py` (shared lib `smoke-client-lib.py`), `internal/observability` otel-tagged files, 2 fuzz targets, `b_c_redis_live_test.go` (skip-gated), `internal/tray` Windows tests, 27 benchmarks, `scripts/check-route-guard.sh`, 6 workflows.

**Verified gap.** `grep -rln "smoke-client" .github/workflows` → 0; `smoke-client-lib.py:57` hardcodes `/home/hatch/go/bin/go`; no workflow passes `-tags otel` or `-fuzz`; `supply-chain.yml:24-36` ends `|| true`.

**User/product impact.** Regressions in exactly the areas the audit found weak (client compatibility, tracing, fuzz-discovered redaction bugs, supply chain) can reach `main`.

**Proposed solution.** (a) Make the smoke lib resolve Go via `shutil.which` and add a job that boots the router and runs all five drivers; (b) add `-tags otel` build+test to `scripts/ci.sh`; (c) remove `|| true` from govulncheck and pin its version; (d) add a bounded fuzz job; (e) add a Redis service-container job; (f) add a `windows-latest` job for `internal/tray`; (g) make node a hard prerequisite for the dashboard JS tests instead of a silent skip; (h) commit the required-check list as policy documentation.

**Scope boundaries.** CI/scripts only — no production code change. Do not add new claims; execute existing ones.

**Dependencies.** None. Interacts with WS-7 (benchmark gate).

**Acceptance criteria.** Each named layer produces a workflow artifact/log; a deliberately broken smoke driver fails the job; `go build -tags otel ./...` runs in CI; a dependency CVE with a known fix fails the supply-chain job.

**Regression tests.** The CI jobs themselves, plus a negative test (inject a failing driver/otel compile error in a scratch branch and confirm red).

**Risks.** Runtime cost and flakiness on hosted runners (mitigate: bounded fuzz time, cache Go modules, keep drivers sequential).

**Complexity.** Medium. **Sequence:** first.

---

## WS-2 — One identity: credential → principal, used everywhere

**Problem statement.** Two credential parsers exist: `requestKey`/`authorized` accept three headers, `callerKeyID` accepts one. Vendor-header callers resolve to "no identity", which bypasses per-key policy/budgets/limits and collides on cache tenancy; `batchVisible` then treats the empty owner as all-visible (AUD-SEC-001).

**Existing implementation.** `requestKey` (`server.go:2169`), `authorized`/`bearerKey`/`requireScope`/`requireKeyPolicy`, `callerKeyID` (`:2284`), 60 `callerKeyID(r)` call sites, `KeyIDVerifier`, OAuth JWT principal naming.

**Verified gap.** `callerKeyID` re-parses `Authorization` and returns `""` for `X-Api-Key`/`X-Goog-Api-Key`; `batchVisible` (`batches.go:240-245`) returns true when `callerID == ""`.

**User/product impact.** A restricted key escapes its own restrictions; one key can read, cancel and delete another key's batches. This is a tenant-isolation break, not a cosmetic one.

**Proposed solution.** Introduce a single `principalFor(r)` (credential extraction + resolution + OAuth fallback) and route `callerKeyID`, `requireScope`, `requireKeyPolicy` and audit attribution through it; forbid direct `r.Header.Get("Authorization")` parsing outside that function (lint or review gate). Change `batchVisible` so a missing owner is visible only to a missing owner. Keep the audit log line shape stable.

**Scope boundaries.** Identity resolution only; do not redesign the guard tiers or the org/team store.

**Dependencies.** A1 (the immediate fix) should land as the minimal version of this; WS-2 is the structural completion (single function, no residual parsers).

**Acceptance criteria.** For every accepted credential header × every protected route class, a table-driven test asserts the resolved principal equals the key's stored ID; a key with IP/budget/traffic policy is enforced identically under all three headers; cross-key batch access fails for all header forms; the cache tenant is never empty for any authenticated request.

**Regression tests.** The auth-matrix identity test above; cross-tenant batch test; cache-tenancy isolation test.

**Risks.** Changing identity resolution touches 60 call sites — land behind a single function and rely on the existing tenant-isolation suites.

**Complexity.** Medium. **Sequence:** first (with WS-1).

---

## WS-3 — Trustworthy money: relay accounting, durable revocation, honest status codes

**Problem statement.** Three independent paths make spend/budget state untrustworthy: verbatim Anthropic relays record no ledger row, failed async batches are dropped, and revocation/config writes can fail silently. Separately, chat upstream 429/402/5xx collapse to 502 (AUD-SEC-002, AUD-REL-001/002, AUD-CODE-001/012).

**Existing implementation.** `internal/ledger` (usage, budgets, reservations, async writer, memo), `relay.go` (verbatim + translate paths), `handleEngineError` vs the correct `handleImagesEngineError`/Gemini handlers, `configPersist` + the correct rollback in `admin_keys_api.go:656`.

**Verified gap.** "No ledger row is recorded here … on success (standing relay gap)"; `commitBatch` logs `rows may be lost` without re-queue; `_ = s.configPersist()` then 200; `grep -c RateLimitError internal/server/server.go` = 0.

**User/product impact.** Budgets under-count real spend (so caps do not cap), and a rate-limited client is told "bad gateway" with no back-off signal — the two failure modes customers notice first.

**Proposed solution.** (a) Extract `usage` from relayed responses (JSON body and final SSE message) and write a ledger row, or gate verbatim relay for budgeted keys; (b) re-queue failed async batches with bounded retry and expose `FailedRows`; (c) add typed status mapping for rate-limit/quota in `handleEngineError` including `Retry-After`; (d) check-and-roll-back in the key mutation handlers and in `Issuer.Revoke`.

**Scope boundaries.** No billing-grade rewrite: keep SQLite, keep the async writer's bounded-loss contract *except* for the retry, and do not introduce a new ledger schema beyond what usage rows already carry.

**Dependencies.** WS-1 (so the new tests actually gate merges); C4 (`AUD-PERF-002`) should land nearby so the new relay writes do not contend worse.

**Acceptance criteria.** A relayed `/v1/messages` request increments the key's spend by the upstream-reported amount; a forced batch-commit failure retries and ultimately reports `FailedRows` ≠ 0 while the row remains durable; a rate-limited chat request answers 429 with `Retry-After`; a failed `configPersist` returns 500 and leaves the key's previous state intact across a simulated restart.

**Regression tests.** Relay-accounting test; async-writer failure-injection test; status-mapping table test per dialect; persistence-failure rollback test for disable/enable/revoke.

**Risks.** Relay usage parsing must not fabricate usage when the upstream omits it — record "unknown", never zero (the same rule the cost estimator already follows).

**Complexity.** Medium-high. **Sequence:** second.

---

## WS-4 — Protocol truthfulness per dialect

**Problem statement.** The Anthropic dialect returns OpenAI-shaped errors non-streaming while its streams are Anthropic-shaped, and the generator documents one envelope for all dialects; unknown models are reported as upstream failures; the declared Anthropic provider surface can resolve to the OpenAI wire (AUD-API-001/002/003, AUD-PROV-002).

**Existing implementation.** Per-dialect writers (`writeError`, `writeGeminiError`, relay error frames), `scripts/gen-api-docs.py` (347-route parity verified), `catalog/auth.go` wire resolution.

**Verified gap.** No `writeAnthropicError` exists (`grep -c` → 0); generator hardcodes one envelope sentence (`gen-api-docs.py:359-361`); empty chain → `ErrNoAvailableAttempt` → 502 + `IncUpstreamFailure`; wire resolution prefers `WireFormats[0]` + a `/anthropic` substring heuristic.

**User/product impact.** Anthropic-native SDKs mis-parse failures; client typos inflate the upstream-failure metric that health/alerting reads; a declared provider surface cannot execute.

**Proposed solution.** (a) `writeAnthropicError` used on all `/v1/messages*` error paths; (b) generator emits the envelope actually used per route dialect, and a test asserts docs match the writers; (c) map unknown-model/unroutable to 404/400 without the upstream-failure counter; (d) resolve wire from the declared endpoint name first.

**Scope boundaries.** No change to successful response shapes; no new dialects.

**Dependencies.** WS-1 (docs-parity check must run in CI).

**Acceptance criteria.** For each dialect, a test asserts the exact error envelope for validation failure, unknown model, upstream 429 and upstream 5xx — on both stream and non-stream paths; the generated docs match those assertions; `POST /api/providers` with a declared `messages` endpoint produces an Anthropic-wire client.

**Regression tests.** Per-dialect error-envelope matrix (stream + non-stream); unknown-model status test; wire-resolution test per catalog endpoint.

**Risks.** Changing error codes is client-visible (e.g. 502 → 429/404); document in `docs/API_REFERENCE.md` and the changelog.

**Complexity.** Medium. **Sequence:** third.

---

## WS-5 — Capacity: per-stream memory and ledger concurrency

**Problem statement.** Every in-flight stream retains its entire response in memory to compute a length, and the ledger serves all reads through one SQLite connection behind the batch writer (AUD-PERF-001/002; measured 12–17 µs per budget miss, 27.5 ms per 256-row batch).

**Existing implementation.** `handleChatStream` (`sent strings.Builder`), `ledger.Open` (`SetMaxOpenConns(1)`, WAL, `busy_timeout`), per-request budget memo (52 ns hit).

**Verified gap.** 3 writes / 0 `.String()` reads of `sent`; every budget gate and reservation shares the writer connection.

**User/product impact.** Memory grows with concurrency × completion size (an OOM vector under load); budget checks queue behind writes, so latency spikes exactly when traffic is highest.

**Proposed solution.** (a) Replace the builder with a byte counter; (b) add a read connection/pool for budget and usage reads under WAL, keep one writer, and re-benchmark; (c) optional bounded-staleness cross-request budget cache (only after (b) is measured).

**Scope boundaries.** No ledger schema change; no change to reservation semantics (the always-settle guarantee must hold).

**Dependencies.** WS-3 (relay writes) should land first to avoid re-measuring twice.

**Acceptance criteria.** A benchmark shows a long stream holding constant memory independent of completion size; `BenchmarkCheckBudgetOrErrorMiss` improves materially without weakening reservation correctness; existing concurrency/race tests pass (on a CGO runner).

**Regression tests.** Stream-memory test (assert no accumulation), ledger read/write concurrency test.

**Risks.** Multiple connections re-introduce `SQLITE_BUSY` if busy_timeout/ WAL handling regresses — cover with a contention test.

**Complexity.** Medium. **Sequence:** fourth.

---

## WS-6 — Resilience: background-job safety and durable event delivery

**Problem statement.** Background goroutines (eval scheduler, webhook dispatch) have no top-level recover, so an unexpected panic kills the gateway; webhook delivery treats 429 as terminal and keeps no cross-restart queue; the eval store is non-atomic and its start failure is swallowed (AUD-REL-003/006/007, AUD-CODE-006).

**Existing implementation.** Per-case recover in `Harness.runOne`; request-path panic isolation pattern (`noteServerPanic`); webhook retry on 5xx with 1/2/4 s backoff; eval JSON store; `internal/health` atomic journal as a model.

**Verified gap.** `grep -rln 'recover()' internal/eval internal/webhooks` → only `eval.go`; `if resp.StatusCode < 500 {` makes 429 terminal; `os.WriteFile` without rename/fsync; `if _, err := s.ensureEvalStore(); err != nil { return }`.

**User/product impact.** One panic in a scheduler/hook path takes down the gateway; operators miss degradation notifications exactly when they are rate-limited; scheduled evaluations can silently stop after a crash.

**Proposed solution.** (a) Wrap every background entry point in a logging recover that mirrors the request-path pattern; (b) treat 429 (honouring `Retry-After`) as retryable and add a bounded persistent pending queue; (c) make the eval store atomic and surface its failures in the API and logs.

**Scope boundaries.** No new job framework; no change to webhook payload/signature format.

**Dependencies.** WS-1 (failure-injection tests must run).

**Acceptance criteria.** An injected panic in a scheduler/webhook goroutine is logged, counted, and the process stays up; a 429 from a webhook endpoint is retried with backoff; a torn eval store is detected and reported instead of silently disabling schedules.

**Regression tests.** Panic-injection test per background entry point; webhook 429 retry test; eval-store torn-file recovery test.

**Risks.** Persistent webhook queues need a bounded on-disk format and a retention policy — keep it small and documented, or defer (see "not recommended").

**Complexity.** Medium. **Sequence:** fifth.

---

## WS-7 — Performance guardrails: measure in CI, then tune

**Problem statement.** 27 benchmarks exist but none run in CI, so the regressions the audit measured (prompt re-hashing per hop, unpooled relay transport, unbounded learned-signal goroutines, missing connection caps, coarse engine mutex) cannot be caught or proven fixed (AUD-PERF-003/004/005/006/007/008/009/010/011/013, AUD-TEST-006).

**Existing implementation.** Benchmarks in `internal/tokencount`, `internal/ledger`, `internal/router`, `internal/cache`; `bench-infra-smoke.yml` (compile-only); `bench.sh` (Linux-bound).

**Verified gap.** `bench-run.yml` is dispatch-only and swallows failures (`|| echo BENCH_REFUSED`).

**Proposed solution.** (a) Run a small, stable benchmark subset on a quiet runner and compare against a committed baseline with a tolerance; (b) apply the targeted fixes above as separate, individually-benchmarked changes; (c) count the prompt once per request (AUD-PERF-004) — the highest-leverage per-request fix.

**Scope boundaries.** No speculative optimisation without a measured before/after.

**Dependencies.** WS-1.

**Acceptance criteria.** A benchmark regression beyond tolerance fails CI; each fix ships with before/after numbers recorded in the PR.

**Regression tests.** The benchmark gate plus a per-fix benchmark assertion where stable.

**Risks.** Benchmark noise on shared runners — use a quiet self-hosted runner or a generous tolerance, and prefer structural assertions (allocation counts) where timing is noisy.

**Complexity.** Medium. **Sequence:** fifth/sixth.

---

## WS-8 — Hygiene: docs, dead code, deployment posture

**Problem statement.** Competitor parity research and third-party comparison tables are public; six dead helpers mislead readers; four handlers still commit a status before encoding; container bases are not digest-pinned; there is no readiness probe (AUD-DOC-001/002/003/004, AUD-CODE-002/007..011/013/014, AUD-CFG-001/002/003/004/005).

**Existing implementation.** `writeJSON` (the correct encode-then-header pattern), atomic stores to copy, `HEALTHCHECK` in the Dockerfile, a route-parity generator.

**Verified gap.** As tabulated in the ledger; e.g. `_ = json.NewEncoder(w).Encode(...)` after `w.WriteHeader(...)` in four handlers.

**User/product impact.** Mostly maintainability and operator trust; the pinning/readiness items matter for reproducible deploys.

**Proposed solution.** (a) Move competitor research out of the public tree (A4); (b) delete or wire the dead helpers; (c) marshal before writing the status in the four handlers; (d) pin base images by digest; (e) add `/readyz` checking ledger/engine and wire it into the container healthcheck; (f) separate drain and cache-sync timeouts.

**Scope boundaries.** No behaviour change to successful responses; no renaming sprees.

**Dependencies.** None (independent), but A4 should happen immediately.

**Acceptance criteria.** No competitor name appears anywhere under `docs/` (case-insensitive `git grep` over the docs tree is empty); `rg` finds no unreferenced exported helper in the audited packages; a forced encode failure yields 500 not a truncated 200; images are digest-pinned; `/readyz` returns 503 while the ledger is unavailable.

**Regression tests.** Encode-failure test per fixed handler; readiness test with an unhealthy ledger.

**Complexity.** Low-medium. **Sequence:** last (or opportunistically alongside the others).

---

## Explicitly NOT proposed for P2

1. **Per-key quota-expiry integration** — external blocker: no provider API exposes it. Keep the honest "what the ledger knows" reporting.
2. **Compliance with a competitor's feature list** — the audit brief forbids using another product as an implementation checklist, and the parity documents themselves must leave the public tree.
3. **Replacing SQLite with a client/server database** — the measured contention (WS-5) is addressable with a read connection; a migration is not justified by evidence.
4. **Turning on OTel export in production** — WS-1 only makes it *compile and test*; exporting traces is a cost/ops decision.
5. **SLAs/latency claims in public docs** — the audit measured internal benchmarks only; publishing comparative latency/memory tables (AUD-DOC-002) should stop, not expand.

---

## Suggested sequencing (single track)

1. **WS-1** (verification integrity) — nothing else can be trusted until this is true.
2. **WS-2** (one identity) — with the A1 hotfix landing ahead of it.
3. **WS-3** (trustworthy money).
4. **WS-4** (protocol truthfulness).
5. **WS-5** (capacity) then **WS-7** (benchmark gate) — capacity fixes need measurement.
6. **WS-6** (resilience) and **WS-8** (hygiene) — independent, can interleave.
