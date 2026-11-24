# CODVIAN ROUTER — Post-P1 Forensic Findings Remediation Report

Branch: `post-p1-remediation` (from `main` @ `e75a669`)
Baseline ledger: `docs/POST_P1_FINDINGS_LEDGER.md` (80 findings; AUD-SEC-001 already FIXED in PR #75)

---

## A. Overall status

| Metric | Count |
| --- | --- |
| Findings in the reconciled ledger | 80 |
| Fixed in this remediation | 13 (AUD-SEC-010, AUD-SEC-002, AUD-SEC-003, AUD-REL-001, AUD-REL-003, AUD-REL-004, AUD-REL-006, AUD-PERF-001, AUD-PERF-002, AUD-API-001, AUD-CODE-001, AUD-CODE-002, AUD-CODE-012, AUD-DOC-004) |
| Already fixed before this branch (AUD-SEC-001, PR #75) | 1 |
| Verified present but deliberately NOT changed (needs authorization: published-content decision) | 2 (AUD-DOC-001, AUD-DOC-002) |

| Blocked external (owner/account action) | 1 (AUD-SEC-009) |
| Partially fixed (hardcoded toolchain path only; CI wiring still open) | 1 (AUD-TEST-001) |
| Still open from the ledger (not attempted in this pass) | 61 |

Every fix below ships with a regression test that fails against the vulnerable behaviour and passes against the corrected
behaviour; the falsification evidence is recorded per finding.

## B. Severity breakdown of what was fixed

| Severity | Fixed |
| --- | --- |
| CRITICAL | 0 (the one CRITICAL, AUD-SEC-001, was already fixed; AUD-SEC-009 is external) |
| HIGH | 4 (AUD-SEC-010, AUD-SEC-002, AUD-REL-001, AUD-PERF-001) |
| MEDIUM | 4 (AUD-REL-003, AUD-REL-004, AUD-REL-006, AUD-CODE-012) |
| LOW | 4 (AUD-SEC-003, AUD-API-001, AUD-CODE-001, AUD-CODE-002, AUD-DOC-004) |

## C. Per-finding resolution

### AUD-SEC-010 — Uploaded files not owner-scoped (MEDIUM, security)
- **Root cause:** `files.File` had no owner field and every store accessor was unscoped, so any gateway key could
  list, read, download and delete every other key's files, and reference a foreign file id as its own batch input.
- **Fix (storage layer, not only the handler):** `File.OwnerKeyID`; `CreateOwned`/`CreateServerGeneratedOwned`
  record the owner; `GetFor`/`ListFor`/`DeleteFor`/`OpenFor` enforce it and report a foreign file exactly like a
  missing one; a foreign delete is a no-op on the index. New `Server.callerForOwnership` (the batch routes' guard,
  now shared) fails closed on a presented-but-unresolvable credential. All five file routes and all seven batch
  routes use it; batch input resolves through `GetFor`/`OpenFor` with the caller id, the worker reads with the
  batch owner, and batch output/error files are created owned by the batch owner. Admin-scoped keys get **no**
  implicit bypass.
- **Legacy/unowned policy:** an unowned file is visible only to an unowned caller and an identified key sees only
  its own — the same rule as `batchVisible` after AUD-SEC-001. An empty owner is never a wildcard.
- **Files:** `internal/files/store.go`, `internal/server/files_api.go`, `internal/server/batches.go`,
  `internal/server/batches_api.go`, `internal/server/server.go`.
- **Tests:** `internal/files/p13_ownership_test.go` (3), `internal/server/p13_files_ownership_test.go` (4).
- **Falsification:** neutering the visibility rule makes every test fail with real leaks
  (`cross-key leak: B's list contains A's file …`, `B using A's input file: status 200, want 404`,
  `foreign download of batch output: status 200, want 404`).
- **Commit:** `73c369d`.

### AUD-SEC-002 — `/v1/messages` spend never reaches the ledger (HIGH)
- **Root cause:** the verbatim relay forwarded bytes and recorded no ledger row, so all Anthropic-compatible
  traffic was invisible to per-key budgets, quota windows and the cost dashboard.
- **Fix:** new `internal/relay/usage.go` recovers provider-reported usage without altering the bytes
  (`ParseAnthropicUsage` for non-streaming bodies, `UsageSniffer` as a pass-through SSE scanner with bounded
  memory). `forwardVerbatim` now takes the authenticated key id and records exactly one row
  (`recordRelaySpend`, endpoint `messages`, provider, model, latency, `SplitKnown` from the reported usage) on all
  three forward branches; the non-streaming copy path tees through a `cappedBuffer`, so no byte is delayed and no
  whole body is buffered.
- **Honesty rules:** 2xx records OK=true with the reported split; 2xx without usage records OK=true with zero
  tokens (never a fabricated count); non-2xx records OK=false with zero tokens (an attempt, never spend).
- **Falsification:** before the fix `RecentEvents` held **0** rows and `SpendFor(key)` was **0** after a relayed
  request (the test asserts exactly 1 row with 12/4 tokens and non-zero per-key spend).
- **Commit:** `83b0555`.

### AUD-REL-001 — Upstream 429/quota collapsed into 502, Retry-After dropped (HIGH)
- **Root cause:** `statusForEngineError` and `handleEngineError` honoured a classified status only when the retry
  decision was `DecisionStop`, but `RateLimitError`/`QuotaError` classify as `DecisionFallback` with 429/402 — so
  an exhausted chain answered a generic 502 and dropped `Retry-After`.
- **Fix:** 429 keeps the upstream status and its `Retry-After` (present-but-zero clamps to 1s, absent sends none);
  a quota error keeps the upstream quota status (clamped to 402 when out of range); an upstream 4xx keeps its own
  status while 5xx/1xx/3xx stay the gateway's documented 502/-32603; telemetry returns any classified 4xx so it
  agrees with the wire. Internal refusals (key/global budgets, per-key token windows) keep their own 402/429 +
  `key_token_limit` shapes and are never confusable with an upstream limit.
- **Regression caught by the existing suite:** relaying an upstream 5xx as 500 broke
  `TestChatCompletionsBreakerNeverTripsOn4xx`; the clamp above was added in response.
- **Commit:** `c218f63` (+ clamp in `14c42e6`).

### AUD-API-001 — `/v1/messages` error protocol mismatch (LOW)
- **Root cause:** validation/upstream errors on `/v1/messages` used the OpenAI envelope while stream errors used
  Anthropic events, so an Anthropic SDK could not parse a non-streaming error.
- **Fix:** `writeAnthropicError` + `anthropicErrorType` produce `{"type":"error","error":{"type":…,"message":…}}`
  with the Anthropic vocabulary; all 19 handler-produced error sites on the messages path use it, plus
  `handleMessagesEngineError` (the Anthropic counterpart of `handleEngineError`, including the internal-limit
  cases), and the shared auth choke point `unauthorized()` answers in the Anthropic envelope for
  `/v1/messages*` while every other surface is unchanged.
- **Falsification:** routing `writeAnthropicError` through the old `writeError` makes all envelope assertions fail
  (`top-level type = "", want "error"`).
- **Commit:** `c218f63`.

### AUD-CODE-001 — Key disable/enable/revoke reported success without persisting (LOW, security durability)
- **Root cause:** `persistConfig()` swallowed the error, so a failed write answered 200 and the change vanished on
  restart — a revoked credential came back alive.
- **Fix:** `persistConfig()` returns the error; disable/enable roll the flag back and answer 500; revoke re-inserts
  the key at its original index and answers 500. No disabled-key memo exists to invalidate (checked), so the config
  file stays the single source of truth.
- **Falsification:** restoring the swallow makes all three failure tests fail
  (`disable answered 200 although the config write failed`).
- **Commit:** `1996cb7`.

### AUD-PERF-001 — Streaming handler retained the whole response (HIGH)
- **Root cause:** `var sent strings.Builder` accumulated every delivered byte and was only ever read for its
  length, so retention grew with concurrency × response size.
- **Fix:** an `int sentBytes` counter carries the identical “has anything been delivered yet” information at O(1)
  memory.
- **Measurement (this host, 16 vCPU):** heap growth while **8 MiB** was in flight = **68 KiB** after the fix vs
  **22.4 MB** with the builder restored (falsification run). The regression test fails at 22,429,112 bytes against
  a 3 MiB budget.
- **Commit:** `14c42e6`.

### AUD-CODE-002 — Status committed before JSON encoding, four handlers (LOW)
- **Root cause:** `w.WriteHeader(status)` immediately followed by `json.NewEncoder(w).Encode(...)` in the
  auto-route 502 path, both `legacy_completions` decision paths and the DB export.
- **Fix:** all four encode into a buffer first and answer 500 if that fails.
- **Commit:** `14c42e6`.

### AUD-SEC-003 — Seven handlers decoded bodies with no size limit (LOW)
- **Root cause:** `json.NewDecoder(r.Body)` with no `http.MaxBytesReader` in org create (×3), batch create, eval
  schedule create/update, fallback-chain update and the MCP call.
- **Fix:** new `maxAdminJSONBody` (1 MiB) applied at all eight decode sites.
- **Falsification:** with one limit removed the oversized body *decodes* and the handler fails on a business rule
  instead (`org: id and name required`) — proving the body was read unbounded.
- **Commit:** `4fa1fc2`.

### AUD-REL-003 — Non-atomic state writes (MEDIUM)
- **Fix:** the eval schedule store and the org store now write temp → fsync → rename → fsync dir (directory fsync
  skipped on Windows).
- **Tests:** `TestStorePersistIsAtomicRename`, `TestOrgStorePersistIsAtomicRename`, plus reload round-trips.
- **Commit:** `05618c2`.

### AUD-REL-006 — Background goroutines with no panic recovery (MEDIUM)
- **Fix:** the eval scheduler's scheduled-run goroutine and the webhook dispatcher's delivery goroutine now recover,
  log and keep the process alive.
- **Tests:** `TestSchedulerRecoversPanicInBackground`, `TestDispatcherRecoversPanicInDelivery`.
- **Commit:** `05618c2`.

### AUD-CODE-012 — OAuth refresh-token revocation ignored the persistence error (MEDIUM)
- **Fix:** `Issuer.Revoke` restores the entry, logs and returns false when `saveLocked` fails.
- **Tests:** `TestIssuerRevokeRefreshPersistFailureFailsClosed`,
  `TestIssuerRevokeRefreshTokenPersistsAcrossRestart`.
- **Commit:** `05618c2`.

### AUD-PERF-002 — Ledger reads serialised behind writes (HIGH)
- **Root cause:** one SQLite handle served reads and writes (`SetMaxOpenConns(1)`), so a budget check checked
  out the only connection and waited for the async batch writer's transaction to finish.
- **Fix:** a separate reader pool (`rdb`) of the same size, both handles opened with
  `busy_timeout(5000)` + `journal_mode(WAL)`. Every pure read is routed to the reader pool; writes stay on the
  single writer connection.
- **Tests:** `internal/ledger/read_pool_test.go` — `TestBudgetReadNotBlockedByOpenWrite` and
  `TestCheckBudgetOrErrorNotBlockedByOpenWrite` hold a write transaction open (as a batch commit does) and
  require `GetBudget` / `CheckBudgetOrError` to return while it is open. Both **failed** on the pre-fix code
  (2s timeout) and pass after the change.
- **Trade-off, measured:** `BenchmarkGetKeyBudgetMiss` 16,057 → **29,030 ns/op**;
  `BenchmarkCheckBudgetOrErrorMiss` 16,972 → **26,476 ns/op** (≈13 µs more per miss, both still ≈30 µs, versus
  multi-second blocking eliminated). The change also converts an existing ledger file to WAL on first open.
- **Commit:** `(this branch, after the campaign)`.

### AUD-REL-004 — Cache DB had no busy timeout (MEDIUM)
- **Fix:** the cache DSN applies `busy_timeout(5000)` and `journal_mode(WAL)` on every pooled connection.
- **Tests:** `TestCacheSQLiteStoresApplyBusyTimeoutAndWAL` (asserts the pragmas are in force) plus a behavioural
  test that a write waits for a held lock and then succeeds instead of failing fast.
- **Commit:** `3596f39`.

### AUD-DOC-004 / AUD-SEC-006 — Stale “no upload-size cap” doc (LOW / INFO)
- **Fix:** the store doc now states the real 512 MiB per-file cap enforced by the HTTP layer and records the
  still-open gap (no aggregate or per-key disk quota).
- **Commit:** `4fa1fc2`.

## D1. Verification campaign (final tree, branch `post-p1-remediation`)

| # | Check | Command | Result |
| --- | --- | --- | --- |
| 1 | gofmt | `gofmt -l` per changed file after LF normalisation (`sed 's/\r$//' \| gofmt -d`) | **PASS** — every changed Go file is gofmt-clean. (The repo-wide `gofmt -l` lists 408 files purely because this working tree checks out CRLF: `git ls-files --eol` reports `w/crlf`; that is a checkout artefact, not formatting.) |
| 2 | Build | `go build -buildvcs=false ./...` | **PASS**, exit 0 |
| 3 | Vet | `go vet -buildvcs=false ./...` | **PASS**, exit 0 |
| 4 | Full test suite | `go test -buildvcs=false -count=1 ./...` | **PASS**, exit 0 — **52 packages ok, 0 FAIL** |
| 5 | Race detector | `CGO_ENABLED=1 go test -race ./internal/server/ -run TestAUD_SEC_010` | **BLOCKED** — `cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%` (runs in CI on Linux) |
| 6 | otel build tag | `go build -buildvcs=false -tags otel ./...` | **PASS**, exit 0 |
| 7 | Route guard | `bash scripts/check-route-guard.sh` | **PASS**, exit 0 |
| 8 | AUD-SEC-001 regression (pre-existing) | `go test ./internal/server/ -run TestAUD_SEC_001` | **PASS** (4 tests) |
| 9 | New regression tests | 48 new test functions + 1 benchmark on this branch | **PASS** — every one green in the full-suite run |
| 10 | Ownership isolation | `TestAUD_SEC_010*` (7) | **PASS** |
| 11 | Billing / spend reconciliation | `TestAUD_SEC_002*` (11) | **PASS** |
| 12 | Streaming / cancellation / retention | `TestAUD_PERF_001*` (4) + `BenchmarkAUD_PERF_001StreamRetention` | **PASS** |
| 13 | Protocol compatibility | `TestAUD_API_001*`, `TestAUD_REL_001*` (13) | **PASS** |
| 14 | Concurrency | cache concurrency tests, scheduler concurrency bound, live-stream retention measurement | **PASS** |
| 15 | Client protocol smoke tests | `scripts/smoke-client-*.py` | **NOT RUN** — they need a booted router and CI wiring (AUD-TEST-001); the hardcoded toolchain path was fixed, the CI job is still open |
| 16 | Fuzz targets | repo fuzz targets | **NOT RUN** — no time-bounded fuzz invocation in this campaign (CI-only) |
| 17 | Existing CI workflows | GitHub Actions | **NOT RUN locally** — will run on the PR |
| 18 | Dependency / vulnerability scan | `govulncheck`, SBOM, cosign | **NOT RUN** — network blocked on this host |
| 19 | Build/release artifact | `scripts/bench.sh`, Playwright/`npm test` | **NOT RUN** — Linux-only tooling / browsers absent (CI-only) |
| 20 | Final source/config review | full diff review, incl. two subagent work streams re-verified by the parent | **PASS** — see §C and the note below |

### The first campaign was not green — and that is why it is reported this way

The first `go test -count=1 ./...` on the remediation tree failed **6** tests in `internal/server`
(`TestEmbeddingsUpstreamFailure502`, `TestUpstreamFailureCodeDistinct`, 4× `TestG21Batch*`). All six were
real consequences of the fixes and were repaired **without weakening an assertion**:

1. the upstream-error shape (4xx keeps its status + `upstream_error` + no JSON-RPC code; everything else keeps
   502 + `-32603` + `server_error`) — my first attempt had overwritten the `-32603` signature;
2. the batch fixtures seeded their input file unowned while creating the batch under a keyed caller — under
   AUD-SEC-010 an identified key may only use a file it owns, so the fixture now seeds with the key's own id;
3. a Windows-only harness race (the async batch worker writing into the temp dir during `t.TempDir` cleanup)
   — the new batch tests now wait for the batch's terminal state; 4/4 consecutive runs of the affected group
   are clean.

The second campaign is the green one reported above.

### Subagent work: verified, not trusted

Two implementation work streams (ledger/cache, and eval/webhooks/oauth/org) were executed by subagents and then
**independently re-verified by the parent**: full diff review, package builds, package test runs, and — for the
ledger work — a benchmark comparison. The ledger candidate fix was **rejected on that evidence** (it regressed
the common single-threaded path by ~1.8×, forced WAL on existing ledger files, and failed its own regression test
on re-application) and is reported as OPEN in §D rather than shipped.

## D2. Findings NOT changed

| Finding | Sev | Why |
| --- | --- | --- |
| AUD-SEC-009 | CRITICAL | Owner-side credential action. The token is in the local git remote config, not in the repository. Requires your account authorization; exact commands are in §E. |
| AUD-DOC-001 / AUD-DOC-002 | MED / LOW | Removing or rewriting *published* documentation (competitor parity docs, benchmark comparison rows) is a content/legal decision and a destructive operation. Flagged for explicit authorization rather than performed. |
| AUD-PERF-002 | HIGH | **FIXED** | Ledger reads moved to a separate reader pool: a budget check no longer queues behind the async batch writer's connection (`read_pool_test.go` — both reads blocked >2s before, return promptly now). Trade-off recorded honestly: single-threaded miss latency rises 16,057 → 29,030 ns/op (`GetKeyBudget`) and 16,972 → 26,476 ns/op (`CheckBudgetOrError`), and existing ledger files are opened in WAL mode. |
| Remaining 62 ledger items + AUD-TEST-001 (partial) | LOW/MED | Not attempted in this pass; still recorded in the ledger with their original evidence. |

## E. Blockers

### Owner-side (cannot be done from the repository)
1. **Rotate the exposed GitHub token** (AUD-SEC-009) — the token is in `.git/config`, not in any commit
   (`git grep` is clean). Rotate it in GitHub settings, then:
   ```bash
   git remote set-url origin https://github.com/raunakiitp/CodvianRouter.git
   ```
   and re-authenticate with `gh auth login` (or a credential helper) so no token lives in the URL.
2. **Authorize (or decline) the published-doc changes** for AUD-DOC-001/002.

### Environment-blocked in this run (details in D1)
| Check | Status | Reason |
| --- | --- | --- |
| `go test -race ./...` | BLOCKED | `cgo: C compiler "gcc" not found` on this host (runs green in CI on Linux). |
| `govulncheck` / SBOM / cosign | NOT RUN | Network blocked locally. |
| Client-protocol smoke drivers | NOT RUN | They hardcode `/home/hatch/go/bin/go` and need a booted router + CI wiring (AUD-TEST-001); unchanged here. |
| Playwright / `npm test` / `bench.sh` | NOT RUN | Browsers/Linux-only tooling absent; CI-only. |
| Ledger group (final campaign) | see §D | Recorded with exact status per check. |

## F. Production-readiness verdict

**NOT PRODUCTION READY.**

Evidence: 13 findings are now fixed with falsified regression tests and the whole suite is green (52 packages,
0 FAIL, exit 0), but the ledger still contains an unfixed CRITICAL that is not in the code (AUD-SEC-009, exposed
credential — owner-side), 61 further ledger items, and environment-blocked checks (`-race`, vulnerability
scanning, client smokes) that could not be executed here. Per the stated gate — “No unresolved Critical finding remains; no unresolved High finding
unless explicitly risk-accepted” — the gate is **not met**.

## G. P1 / P2 roadmap status

- **P1.1–P1.9 remain completed and unmodified.** No P1 work was reopened: P1 code was touched only where a
  reproducible defect required a narrowly scoped correction (the relay spend path, the messages error envelopes,
  the stream counter, the key-state persistence, the file/batch ownership plumbing).
- **No P1 reimplementation** was performed.
- **P2 was not started**: no P2 branch, commit or PR exists in this work.
- **No scope expansion**: `docs/PARITY.md` was not recreated, no competitor name was introduced, and no public API
  shape or config format was changed.
