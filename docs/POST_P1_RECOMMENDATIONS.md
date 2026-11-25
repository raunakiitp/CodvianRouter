# Codvian Router — Post-P1 Recommendations

Derived exclusively from the findings in `docs/POST_P1_FINDINGS_LEDGER.md` (80 findings: 2 CRITICAL, 6 HIGH, 22 MEDIUM, 41 LOW, 9 INFO; AUD-SEC-001 fixed on request). Nothing here was implemented during the audit.

---

## A. Immediate maintenance (do this now, outside any programme)

These are not roadmap items; they are correctness/security defects that should not wait for a milestone.

| # | Action | Findings | Why now |
| --- | --- | --- | --- |
| A1 | **Fix the identity split.** Make `callerKeyID` resolve the credential through `requestKey(r)` (so `X-Api-Key`/`X-Goog-Api-Key` callers get their real key ID), and change `batchVisible` so an empty caller ID is **not** treated as visible to every owner. Add regression tests: (a) a key with an IP allowlist presented via `X-Api-Key` is denied from a non-allowed peer; (b) key B, authenticated via `X-Api-Key`, cannot list/get/cancel/delete key A's batches; (c) per-key budget and traffic limits still apply on vendor-header auth; (d) the prompt-cache tenant is not empty for vendor-header callers. | AUD-SEC-001 (CRITICAL) | Any valid key can currently escape its own restrictions and reach other key owners' batch records. |
| A2 | **Rotate the exposed token and clean the remote.** `git remote set-url origin https://github.com/raunakiitp/CodvianRouter.git` after rotating; push via `gh auth git-helper` (already the documented host workflow) so no credential ever lives in a URL again. | AUD-SEC-009 (CRITICAL) | The token is one paste away from disclosure. |
| A3 | **Make key revocation durable.** Check the `configPersist()` error in `handleKeyDisable/Enable/Revoke`; roll back the in-memory mutation and return 500 (mirror `admin_keys_api.go:656`). Same pattern for `oauth.Issuer.Revoke` (`oauth.go:556`). | AUD-CODE-001 (HIGH), AUD-CODE-012 (MED) | A key reported as revoked can come back after a restart — the worst possible failure direction for a security control. |
| A4 | **Separate competitor research from the public tree.** Move the 2 competitor-named parity documents and the parity content of `docs/PHASE4_GAP_ROADMAP.md` out of the repository (private location), and drop the third-party comparison rows from `docs/BENCHMARK.md`. | AUD-DOC-001 (MED/P1), AUD-DOC-002 | The audit brief is explicit that private competitor research/parity tracking must not be public. |

---

## B. Critical fixes (P0)

Covered by A1 and A2 above. Nothing else reaches CRITICAL.

---

## C. High-priority fixes (P1)

| # | Action | Findings |
| --- | --- | --- |
| C1 | **Record relayed Anthropic spend.** Parse `usage` from the verbatim relay response (and the stream's final message) and write a ledger row, so `/v1/messages` traffic counts against budgets; alternatively refuse verbatim relay for budgeted keys. | AUD-SEC-002 |
| C2 | **Return truthful status for upstream rate limits.** Add `RateLimitError → 429` (with `Retry-After` when known) and `QuotaError → its status` branches to `handleEngineError`, mirroring `handleImagesEngineError`/the Gemini handler. | AUD-REL-001 |
| C3 | **Stop retaining whole responses in memory.** Replace the `sent strings.Builder` with a byte counter/boolean in `handleChatStream`. | AUD-PERF-001 |
| C4 | **Give the ledger a read path.** Open a second connection (or a read pool) for budget/usage reads under WAL, keeping one writer; re-measure `BenchmarkCheckBudgetOrErrorMiss`. | AUD-PERF-002 |
| C5 | **Make `/v1/messages` errors Anthropic-shaped.** Add `writeAnthropicError` and use it on every `/v1/messages*` error path (non-stream, count_tokens, validation, upstream). | AUD-API-001 |
| C6 | **Wire the CI layers that are declared but not executed**: run the five `scripts/smoke-client-*.py` drivers (deriving the Go path via `shutil.which`), add `go build/test -tags otel`, and remove `|| true` from `govulncheck`. | AUD-TEST-001, AUD-TEST-002, AUD-TEST-003 |

Sequencing suggestion: C1+C2+C3 are small and independent; C5 needs a new helper and touches several call sites; C6 is CI-only and unlocks honest verification for everything after it.

---

## D. Medium-priority improvements (P2)

**Accounting and durability**
- Re-queue or durably fall back when an async ledger batch fails (AUD-REL-002).
- Make the eval store atomic (temp+rename+fsync) and log the scheduler-start failure; surface it in `/api/eval/*` (AUD-REL-003, AUD-CODE-006).
- Open cache SQLite with WAL + `busy_timeout` (AUD-REL-004).
- Quarantine a corrupt `ledger.db` instead of refusing to boot (AUD-REL-005 — confirm corruption modes first).

**Protocol correctness**
- Apply the tool-capability pre-check on the streaming path (AUD-PROV-004).
- Derive the provider wire from the declared endpoint name before the base-URL heuristic (AUD-PROV-002).
- Map an unknown model to 404/400 and stop counting it as an upstream failure (AUD-API-003).
- Emit per-dialect error-envelope documentation from the generator (AUD-API-002).

**Security hardening**
- Add `http.MaxBytesReader` to the seven body-decoding handlers, including chat-scope `POST /v1/batches` (AUD-SEC-003).
- Validate `redirect_uri` against registered URIs on the OAuth error path (AUD-SEC-004).
- Route the OAuth token exchange through the egress client (AUD-SEC-005).

**Performance and scale**
- Route relay/Nadir through the pooled transport (AUD-PERF-003).
- Count the prompt once per request and reuse it across hops (AUD-PERF-004).
- Bound the learned-rate-limit persistence fan-out (AUD-PERF-006); set `MaxConnsPerHost` (AUD-PERF-007); split the engine mutex (AUD-PERF-005).

**Code hygiene**
- Marshal before `WriteHeader` in the four residual handlers (AUD-CODE-002).
- Delete the six dead helpers or wire them (AUD-CODE-007..011); log the breaker-store rename error (AUD-CODE-013).
- Pin container base images by digest (AUD-CFG-001).

**Testing**
- De-flake the token-count memo assertion by asserting an absolute budget or converting it to a benchmark (AUD-TEST-011).
- Add direct tests for `internal/egress` and `cmd/release-sbom` (AUD-TEST-009); strengthen the five weakest assertions (AUD-TEST-013/014).

---

## E. Security improvements (beyond the P0/P1 items)

1. Add an aggregate/per-key upload quota and correct the stale files-store doc (AUD-SEC-006).
2. Rate-limit `POST /dashboard/auth/totp/verify` with the same throttle as password login (AUD-SEC-007).
3. Record the pinned action SHA audit for `bench-run.yml` and enforce real SHAs (AUD-CFG-005).
4. Keep the current good posture: constant-time comparisons, SSRF-guarded egress, HMAC webhooks, redacting logger, RemoteAddr-only IP control — do not regress these while fixing the above.
5. After A1 lands, add an auth-matrix test that asserts **identity** (not just 200/401) for every accepted credential header on every protected route class — this is the test that would have caught AUD-SEC-001.

---

## F. Performance improvements

1. C3 and C4 (above) are the two material ones.
2. Metrics/observability: shard the counter mutex, cap series cardinality, sort outside the lock (AUD-PERF-008/009/010).
3. Budget reads: consider a bounded-staleness cross-request cache now that the per-request memo exists (AUD-PERF-011).
4. Add a benchmark regression gate so these numbers stop drifting silently (AUD-TEST-006).

---

## G. Architecture improvements

1. **Single source of identity.** The AUD-SEC-001 root cause is two credential parsers. Collapse them: one function to extract the credential, one to resolve identity, used by auth, scope, policy, limits, tenancy and audit. Make the compiler help by removing direct `r.Header.Get("Authorization")` parsing outside that function.
2. **One ledger writer, many readers** (C4) plus a durable fallback path (D) makes budget enforcement trustworthy under load.
3. **Path parity as a testable property.** The three "only on one path" defects suggest a table-driven conformance test that asserts each cross-cutting control (team policy, capability filter, tool capability, context fit, budget reserve, ledger row) is invoked for every dialect × stream/non-stream pair.
4. **Dialect-accurate error typing.** Introduce an error-envelope abstraction per dialect rather than scattered `writeError` calls, which is what produced AUD-API-001/002.

---

## H. Deferred (deliberately not recommended now)

- **Per-key quota-expiry integration.** The P1.1 external blocker stands: no provider API exposes per-key free-tier expiry. Do not invent one; keep reporting what the ledger and pools know.
- **Wiring the otel exporter into production.** The build-tag gap should be closed in CI (C6), but turning on trace export is an operational decision with cost implications, not an audit finding.
- **Cross-process scheduler/queue for webhooks** — worth doing only once webhook delivery is a supported production feature (see P2 proposal WS-6).
- **Process-level `SIGKILL` durability for the async ledger writer** — the bounded window is documented and acceptable for now; revisit if budgets become billing-grade.

---

## I. Should NOT be implemented

1. **Do not rebuild the guard table, team gate, context-fit gate, capability filter, or the P1 report artefacts.** They are verified working; the defects are narrow and enumerated.
2. **Do not add a generic "compatibility" claim for clients.** AUD-TEST-001 shows the honest fix is *executing* the existing protocol drivers in CI, not adding new claims.
3. **Do not recreate `docs/PARITY.md` or any competitor-branded tracking.** The removal was intentional; A4 removes what remains.
4. **Do not "fix" the flaky test by deleting it or loosening it to always-pass.** Convert it to an absolute budget or a benchmark (AUD-TEST-011).
5. **Do not paper over AUD-API-001 by documenting the OpenAI envelope as correct for Anthropic.** The stream path is right; the non-stream path is wrong.
6. **Do not weaken or skip any test to make a fix land.** Every P1-or-above finding above names the regression test that should accompany it.

---

## J. Verification expectations for whatever gets fixed

- Every fix lands with the named regression test, and CI must actually execute it (C6 first makes that meaningful).
- Re-run the measured benchmarks from the audit (`BenchmarkCount`, `BenchmarkGetBudget*`, `BenchmarkCommitBatch256`) before/after the ledger and token-count work, and record the numbers.
- Re-run the full local campaign on the fix commit: `gofmt` (LF/committed content), `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `scripts/check-route-guard.sh` — and report real exit codes, with `-race` only on a CGO-capable runner.
