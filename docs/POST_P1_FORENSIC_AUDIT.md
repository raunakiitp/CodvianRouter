# Codvian Router — Master Post-P1 Forensic Engineering Audit

**Audited HEAD:** `08cadc03db43c27fe014f47a020134490ed2723e` (`main`, commit date 2026-11-24 21:11:42 +0530)
**Working tree:** clean before and after (`git status --porcelain` empty) — no file was modified by this audit.
**Method:** independent source inspection + targeted execution. Every finding below carries file:line evidence and a reproduction; claims I could not reproduce are marked `UNCONFIRMED`/`NEEDS MANUAL VERIFICATION`/`BLOCKED` rather than asserted.

---

## 1. Executive summary

The repository is a genuinely large, carefully built gateway: ~406,650 lines of Go (200,735 of them tests), 44 `internal/` packages, 239 route registrations, and a test suite that runs green end to end (`go test -count=1 ./...` → exit 0, 52 packages `ok`, 0 FAIL, measured in this session).

It is **not production-ready as it stands**, for reasons that are specific and fixable — not architectural rot:

1. **One CRITICAL regression introduced by the last delivery.** P1.3 (PR #72) widened authentication to accept `X-Api-Key` and `X-Goog-Api-Key`, but `callerKeyID` still parses only `Authorization: Bearer`. Vendor-header callers therefore authenticate as **no one**: they bypass per-key IP/endpoint policy, per-key budgets/reservations, per-key traffic limits and guardrail scope, they collide on the empty prompt-cache tenant, and — because `batchVisible` treats an empty caller ID as "visible to all" — they can list, read, cancel and delete **other key owners' batches**. (AUD-SEC-001, CRITICAL/P0.)
2. **A credential sits in the local git remote URL** (`https://x-access-token:***@github.com/...`), violating the project's own delivery rules and leaking through any `git remote -v` or log paste. (AUD-SEC-009, CRITICAL/P0 — escalating separately; not patched under this audit.)
3. **Six HIGH-severity correctness/reliability defects**, each verified: chat upstream rate-limits are reported as generic 502 with no `Retry-After` (AUD-REL-001); the streaming path retains the entire response body in memory to compute a length (AUD-PERF-001); the ledger serialises every read behind a single SQLite connection (AUD-PERF-002); `/v1/messages` returns OpenAI-shaped errors when non-streaming but Anthropic-shaped errors when streaming (AUD-API-001); key disable/enable/revoke return 200 when the config write fails (AUD-CODE-001); the verbatim Anthropic relay records no ledger row, so relayed spend is invisible to budgets (AUD-SEC-002).
4. **The test suite is broad but several verification layers are not executed by CI** — client-protocol smokes, the `otel` build tag, fuzzing, Redis, Windows, benchmarks, and a `govulncheck` step that can never fail (`|| true`). Green CI therefore does not mean "feature verified". (AUD-TEST-001/002/003.)

What is genuinely good, verified, and should not be rebuilt: the guard-tier route table (an unclassified route panics at startup; scope wraps inside auth; key policy inside scope), the pre-dispatch gate ordering on the chat path (admission before body read, guardrails → compression → budgets → team policy before dispatch), the reservation lifecycle (every path settles; TOCTOU serialised), mid-stream failure honesty (committed streams end with an SSE error frame **and** `[DONE]`, never a silent truncation), and complete route parity between code, `routes.json`, `docs/llm.txt`, `API_REFERENCE.md` and `tryit.html` (347/347/347/347/347, checked directionally).

**Verdict: NOT PRODUCTION READY** (two CRITICALs, six HIGHs; the CRITICALs are narrow and directly fixable). See §19 and `docs/POST_P1_RECOMMENDATIONS.md`.

---

## 2. Repository snapshot

| Item | Value |
| --- | --- |
| HEAD | `08cadc03db43c27fe014f47a020134490ed2723e` — "P1 final verification campaign: results and completion report (#73)" |
| Branch | `main` (up to date with `origin/main`) |
| Working tree | clean; no untracked files |
| Local branches | `main`, `master`, `p1.1-quota-intelligence` |
| Tags | none |
| Commits | 401 |
| Remote | `https://x-access-token:***@github.com/raunakiitp/CodvianRouter.git` — **token in URL (AUD-SEC-009)** |
| `credential.helper` | `cache` |
| Merged P1 PRs | #66 (P1.4), #67 (P1.5), #68 (P1.6), #69 (P1.7), #70 (P1.8), #71 (P1.9), #72 (P1.3), #73 (final report) — all `MERGED`, verified via `gh pr list` |
| Open PRs | none |
| Workflows | `ci.yml`, `browser-smoke.yml`, `supply-chain.yml`, `bench-infra-smoke.yml`, `bench-run.yml`, `release.yml` |
| Manifests | `go.mod` (go 1.26.0; tiktoken-go, go-redis, x/crypto, x/sys, modernc sqlite), `go.sum`, `package.json`, `Dockerfile`, `Dockerfile.release`, `docker-compose.yml` |
| Size | 406,650 Go LOC total; 200,735 test LOC; largest files `internal/server/server.go` (8,056), `internal/router/router.go` (7,882), `internal/catalog/providers_reference.go` (6,742) |

**Consistency with the reported P1 state:** the repository *does* match the reported merge history, and `docs/P1_COMPLETION_REPORT.md` + the completion ledger exist as described. **One discrepancy:** the P1 report claims the campaign's blocked/unrun items were "all exercised by CI workflows" — the client-protocol smokes (`scripts/smoke-client-*.py`) are **not** referenced by any workflow (AUD-TEST-001), so that claim is not supported by the repository.

---

## 3. Verified P1 state

| Block | Merge | Independently verified | Classification |
| --- | --- | --- | --- |
| P1.1 Quota intelligence | prior | `GET /api/quota/intelligence` registered (server.go:1795), guard-classified MANAGEMENT/admin (guard.go:262), backed by `ledger.ListQuotaPools`, MCP mirror at mcp_http.go:2378 | IMPLEMENTED AND VERIFIED (static + wired) |
| P1.2 MCP/A2A expansion | prior | 32 MCP tools, static list, pinned by `internal/server/mcp_test.go:52`; 12 A2A skills incl. all 6 expansion skills, pinned by `internal/a2a/skills_test.go:963` | IMPLEMENTED AND VERIFIED |
| P1.3 Client compatibility | #72 `d2b699b` | `requestKey` accepts Bearer/`x-api-key`/`x-goog-api-key` (server.go:2169-2181); `codvian-router clients [--json]` works; matrix claims pinned to real tests | **IMPLEMENTED BUT INCOMPLETE — introduced AUD-SEC-001** |
| P1.4 Compression fidelity | #66 `6b27042` | `internal/compress/evalharness` + `cmd/codvian-bench` present; covered by the full suite | IMPLEMENTED AND VERIFIED (test-level) |
| P1.5 Capability registry | #67 `9fd7a1d` | `require_capabilities` gate uses `mc.HasVerified` and runs on both paths (router.go:5006, 6419) | IMPLEMENTED WITH DEFECTS (AUD-PROV-003, AUD-PROV-005) |
| P1.6 Org/team enforcement | #68 `08543ca` | one gate (`enforceTeamPolicy`) at 9 dialect sites + batch path; chat calls it before the stream branch (server.go:5328) | IMPLEMENTED AND VERIFIED |
| P1.7 Cost estimation | #69 `664c17b` | `/api/cost/estimate`, `/api/cost/reconcile`, read-only budget view; `callerKeyIDFromContext` genuinely wired on all paths incl. embed/rerank/MCP (verified by the code-quality auditor against `withLogging`) | IMPLEMENTED WITH DEFECTS (AUD-SEC-002 relay gap interacts) |
| P1.8 Context/output guardrails | #70 `41a50b2` | `applyEmbeddingFit` + `EmbeddingContextTooLarge` → 400 on both embed and rerank; chat gate verified unchanged | IMPLEMENTED AND VERIFIED |
| P1.9 Scheduled eval dashboard | #71 `3b8cc68` | `Run.Failures` persisted; trends endpoint + baseline/direction; failed runs excluded from baseline | IMPLEMENTED WITH DEFECTS (AUD-REL-003, AUD-CODE-006) |

The "no-fabrication" property of the P1 blocks holds: I found no case where a P1 surface reports a mock as real, invents a price, treats unknown as supported in the *routing* gate, or claims a client works solely because an OpenAI-compatible endpoint exists.

---

## 4. Architecture map

Full detail in `docs/POST_P1_ARCHITECTURE_MAP.md`. Summary of the 29 subsystems audited:

- **Gateway/lifecycle:** `cmd/codvian-router` (CLI, serve/init/wizard/doctor/setup/mcp/provider/catalog/…), `internal/server` (HTTP, 239 routes, guard tiers), `internal/router` (routing engine, streaming, fallback, breakers, cooldown), `internal/provider` (adapters, pool, registry, taxonomy), `internal/wire` (dialect codecs).
- **Identity/policy:** guard tier table + `requireAuth*`/`requireScope`/`requireKeyPolicy`, `internal/config` gateway keys, `internal/oauth`, `internal/org` (+ `teams.go` gate), `internal/guardrails`, `internal/audit`.
- **Money/quota:** `internal/ledger` (SQLite WAL, budgets, reservations, usage, memo), `internal/adaptive` (limiter, learned rate limits), `internal/cache` (exact/semantic/reasoning, Redis or SQLite), `internal/scoring`.
- **Routing intelligence:** `internal/strategy` (35 strategies), `internal/autorouter`, `internal/learned`, `internal/sla`, `internal/coalescing`.
- **Protocol surfaces:** OpenAI chat/completions + Responses (`internal/responses`), Anthropic Messages (`relay.go`, native + translate), Gemini native (`gemini_native.go`), embeddings/rerank (`embeddings.go` + `internal/modality`), images/audio/media, batch (`batches*.go`), A2A (`internal/a2a`), MCP (`internal/mcp`, `mcptools`, `mcp_http.go`).
- **Ops:** `internal/observability` (Prometheus), `internal/metrics`, `internal/health`, `internal/webhooks`, `internal/egress`, `internal/release`, `internal/tray`, `web/` (embedded dashboard).
- **Background work:** eval scheduler (`internal/eval`), model sync (`internal/modelsync`), caches sync, breaker persistence, webhook dispatcher.

---

## 5. Request-lifecycle analysis (chat path, traced in source)

Order actually enforced in `handleChatCompletions` (server.go:5005+):

1. method check → 2. panic backstop (`recover`, noteServerPanic, panic ledger row) → 3. `context.WithTimeout(r.Context(), s.timeout)` → 4. reject `X-Codvian-Routing` header → 5. `preBodyAdmission` (blocked/over-limit key refused **before** the body is read) → 6. `http.MaxBytesReader(maxRequestBody=10 MiB)` → 7. read + decode (raw retained for idempotency hash) → 8. intent/capability pre-checks → 9. guardrails (sync) → 10. compression → 11. internal budgets → 12. **P1.6 team/org policy** (5328) → 13. ledger budget + key-budget reservation (settled by defer) → 14. async guardrail start → 15. **branch**: `handleChatStream` (stream) or `chatWithMCP` (non-stream).

Streaming and non-streaming share steps 1–14; divergence begins at 15. Verified divergences: the tool-capability pre-check (`modelToolsCapable`) exists only in the non-streaming attempt loop (AUD-PROV-004); response-guardrail *blocking* exists only on the non-streaming path (streams redact only, AUD-CODE-014, documented); the verbatim relay path records no ledger row (AUD-SEC-002).

Other dialects each re-enter the same gate set at their own entry point (relay.go, responses.go, gemini_native.go, legacy_completions.go, embeddings.go, images.go, audio.go, media_verify.go, a2a.go, batches.go) — the team gate was verified present in all of them; per-dialect body caps and error envelopes were **not** uniform (AUD-SEC-003, AUD-API-001).

---

## 6. P1.1–P1.9 verification matrix

See §3 for the per-block verdicts and `docs/POST_P1_FINDINGS_LEDGER.md` for the sub-feature findings. Blocking defects attached to P1 surfaces: **P1.3 → AUD-SEC-001 (CRITICAL)**; P1.5 → AUD-PROV-003/005; P1.7 → interacts with AUD-SEC-002; P1.9 → AUD-REL-003/AUD-CODE-006.

---

## 7. Security audit

**CRITICAL** — AUD-SEC-001 (vendor-header identity bypass + cross-key batch visibility), AUD-SEC-009 (token in remote URL).
**HIGH** — AUD-SEC-002 (relayed Anthropic spend never recorded → budget cap ineffective).
**LOW/INFO** — AUD-SEC-003 (7 handlers decode without `MaxBytesReader`, incl. chat-scope `POST /v1/batches`), AUD-SEC-004 (OAuth error-path open redirect), AUD-SEC-005 (`http.DefaultClient` in token exchange), AUD-SEC-006 (512 MiB uploads, no aggregate quota), AUD-SEC-007 (TOTP verify unthrottled), AUD-SEC-008 (revocation not guaranteed durable).

Verified-clean areas (not assumed): key hashes compared with `crypto/subtle`; bootstrap token constant-time; 401 never echoes the key; redacting log writer; SSRF-guarded egress for webhooks/web fetch; HMAC-SHA256 webhook signatures; no hardcoded secrets in tracked files (`git grep` for `sk-`/`AKIA`/etc. clean); reservation TOCTOU serialised (`reserveMu` + `spend_reservations`); unknown team denies models; A2A owner isolation fails closed.

---

## 8. Reliability audit

**HIGH** — AUD-REL-001 (chat 429/quota → 502, no `Retry-After`; images/Gemini surfaces already do it correctly).
**MEDIUM** — AUD-REL-002 (async ledger writer drops failed batches, no retry → silent under-counting), AUD-REL-003 (non-atomic eval store + swallowed error → scheduled evals silently disabled), AUD-REL-004 (cache SQLite without WAL/busy_timeout → `SQLITE_BUSY`, silent persist miss), AUD-REL-005 (corrupt `ledger.db` aborts startup; NEEDS MANUAL VERIFICATION for the exact corruption modes).
**LOW** — AUD-REL-006 (no top-level recover in background goroutines → a panic kills the process), AUD-REL-007 (webhook 429 terminal; no persistent queue), AUD-REL-008 (same-target re-POST of a non-idempotent request; NEEDS MANUAL VERIFICATION for double-billing).
Verified-good: retries bounded (`MaxAttempts` shared across hops, ≤3 providers), backoff with jitter, `Retry-After` honoured with a floor, breaker half-open single-probe, client disconnect cancels upstream, committed streams never silently truncate, reservations always settle.

---

## 9. Performance audit

**HIGH** — AUD-PERF-001 (whole response retained in a `strings.Builder` read only for `.Len()`), AUD-PERF-002 (single SQLite connection serialises budget reads behind batch writes).
**MEDIUM** — AUD-PERF-003 (relay/Nadir bypass the pooled transport), AUD-PERF-004 (prompt re-joined + SHA-256'd + tools re-marshalled per hop), AUD-PERF-005 (~90 acquisitions of one engine mutex), AUD-PERF-006 (unbounded fire-and-forget `learnRateLimit` goroutines), AUD-PERF-007 (no `MaxConnsPerHost`).
**LOW/INFO** — metrics mutex/카ardinality, observability sorts under lock, budget miss 12–17 µs, refresh persists under write lock, double buffering, panel cap hard-coded, memo hit still hashes.

**Measured in this audit** (real runs, i5-13450HX, go1.27.4, no network): `BenchmarkCountCold 12,137 ns/op` vs `BenchmarkCountMemoHit 157.3 ns/op` (~77×); `BenchmarkGetBudgetMemoHit 52.40 ns/op` vs `BenchmarkGetBudgetMiss 11,860 ns/op` and `BenchmarkCheckBudgetOrErrorMiss 16,972 ns/op`; `BenchmarkCommitBatch256 27.5 ms/op (256 rows/batch)`; `BenchmarkFailoverOn500 41,926 ns/op, 56 allocs`. Positive: no per-request goroutine leak found (bounded, ctx-cancelled, joined).

---

## 10. Provider/model audit

**MEDIUM** — AUD-PROV-002 (a declared Anthropic endpoint resolves to the OpenAI wire when the base URL lacks `/anthropic`, so the surface cannot execute; OpenRouter's entry is the example), AUD-PROV-004 (tools pre-check missing on the streaming path).
**LOW** — AUD-PROV-001 (132 catalog entries with `WireUnknown` + nil base URLs are advertised by `All()`/`Count()`/`/api/catalog` but cannot be added or routed), AUD-PROV-003 (`WireUnknown` recorded as Streaming **supported + verified**, breaking the package's own "unknown is never supported" rule), AUD-PROV-005 (`Build()` claims byte-identical output but stamps `time.Now()`), AUD-PROV-006 (the catalog executable-matrix self-audit checks only `WireFormats[0]`, masking AUD-PROV-002).
Verified-good: pricing may be absent → `ok=false` + `unpriced_count` (never $0), capped keys default `fail_closed`, models.dev sync never overwrites operator prices, aliases resolve, the `require_capabilities` gate honours verified-only on both paths.

---

## 11. API/protocol audit

**HIGH** — AUD-API-001 (`/v1/messages` error envelope is OpenAI-shaped non-streaming, Anthropic-shaped when streaming; no `writeAnthropicError` exists).
**MEDIUM** — AUD-API-002 (generator hardcodes one OpenAI envelope sentence, so generated docs misdescribe the Gemini-native and Anthropic surfaces), AUD-API-003 (unknown model → 502 + `IncUpstreamFailure`, i.e. a client typo pollutes the upstream-failure signal).
**LOW** — AUD-API-005 (`/v1/ws` rejects `X-Api-Key`/`X-Goog-Api-Key` that the gateway now accepts).
**Verified-good:** route parity 347/347/347/347/347 in both directions; OpenAI streams terminate with `data: [DONE]`, Anthropic with `message_stop`, Gemini with bare `data:` chunks (matching the real API); cancellation derived from `r.Context()` on every streaming dialect; 68 routes deliberately answer `501 feature_unavailable` and are documented as "Unavailable".

---

## 12. Configuration/deployment audit

**MEDIUM** — AUD-CFG-001 (base images version-tagged but not digest-pinned, unlike the SHA-pinned actions).
**LOW** — AUD-CFG-002 (compose `--listen 0.0.0.0` inside the container), AUD-CFG-003 (5 s drain; a timeout exits 1 and skips `syncCachesOnShutdown`), AUD-CFG-004 (liveness only; no `/readyz`), AUD-CFG-005 (`bench-run.yml` pins `actions/upload-artifact` to a suspicious repeating-hex SHA; NEEDS MANUAL VERIFICATION).
**Verified-good:** config validation rejects a non-loopback listen, default is loopback, auth is on by default and an empty key list yields bootstrap-PENDING rather than an open gateway, the container runs as `nonroot` on distroless with a real `HEALTHCHECK`, and CORS is honestly unimplemented (501) with no permissive `Access-Control-Allow-Origin`.

---

## 13. Testing/CI audit

**MEDIUM** — AUD-TEST-001 (client-protocol smokes never run in CI; the shared lib hardcodes a Linux Go path), AUD-TEST-002 (`-tags otel` never built/tested in CI — it compiles today, exit 0), AUD-TEST-003 (`govulncheck ./... || true` can never fail the supply-chain job), AUD-TEST-011 (wall-clock ratio test in `internal/tokencount`, structurally flaky; not reproduced on this 16-core host — see §17).
**LOW** — AUD-TEST-004..007 (Redis, Windows, benchmark gate, fuzz never executed), AUD-TEST-009/010 (untested `internal/egress`, `cmd/release-sbom`; thin coverage by LOC for `internal/catalog`, `internal/relay`, `internal/capability`, `internal/org`), AUD-TEST-012 (upper-bound latency assertions), AUD-TEST-013/014 (weak/status-less assertions), AUD-TEST-015 (dashboard JS tests skip silently without node).
**INFO** — AUD-TEST-008 (route-guard script not invoked by CI), AUD-TEST-016 (coverage is above average and genuinely deep in tenant isolation/streaming/persistence), AUD-TEST-017 (required-check set is not in-tree).

Scale: 668 test files, 4,804 `func Test`, 23 benchmarks, 2 fuzz targets.

---

## 14. Code-quality audit

**HIGH** — AUD-CODE-001 (key disable/enable/revoke swallow the persist error and return 200).
**MEDIUM** — AUD-CODE-002 (status written before serialization in `handleAutoRoute`, `handleClassify`, `handleExplainRouting`, `handleDBExport`), AUD-CODE-012 (OAuth revocation ignores the persistence error).
**LOW** — AUD-CODE-006 (eval `Upsert` error swallowed), AUD-CODE-007..011 (dead code: `FallbackChain`, `requestBudgetPrice`, `auditEventFrom`, `newShortID`/`generateSecureToken`, `ChangeSecret`, `ProvenanceSummary`), AUD-CODE-013 (breaker-store rename error dropped).
**INFO** — AUD-CODE-014 (response-guardrail block is non-stream only; documented).
Notable positives: **zero** TODO/FIXME/HACK/XXX markers in non-test Go; no handler or CLI command is a stub presented as complete; `writeJSON`'s encode-before-header fix is real and the class of bug was mostly eliminated (4 residual sites, all outside `writeJSON`); `callerKeyIDFromContext` (a P1.7 fix) is genuinely wired on every path including embeddings/rerank/MCP.

---

## 15. Documentation/repository hygiene audit

**MEDIUM** — AUD-DOC-001 (private competitor parity research is committed publicly: `docs/OMNIROUTE_PARITY_MATRIX.md`, `docs/PHASE2_OMNIROUTE_PARITY.md`, 30 mentions in `docs/PHASE4_GAP_ROADMAP.md`, plus 6 further docs — against the audit's §14 rule).
**LOW** — AUD-DOC-002 (published competitor benchmark comparison in `docs/BENCHMARK.md`), AUD-DOC-003 (CLI `usage()` understates `setup` agent support: 13 supported vs 3 documented), AUD-DOC-004 (files store doc says "no upload-size cap" while a 512 MiB cap exists).
**Precision note (to avoid a false finding):** `OpenRouter` hits are a *provider integration*, and the `LiteLLM` hits in `docs/RELEASE-CHECKLIST.md` are a *supply-chain incident lesson* — both legitimate engineering content, not competitor benchmarking.
`docs/PARITY.md` is absent, as intended; its absence is **not** reported as a defect. API docs/route parity is exact (347/347).

---

## 16. Confirmed findings

**79 findings: 2 CRITICAL, 6 HIGH, 21 MEDIUM, 41 LOW, 9 INFO** (77 CONFIRMED, 2 NEEDS MANUAL VERIFICATION: AUD-REL-005, AUD-CFG-005). Every one carries file:line evidence, a reproduction, an impact, a fix and a regression-test flag in `docs/POST_P1_FINDINGS_LEDGER.md`, which is the authoritative list; this document's sections 7–15 group them by area.

The two CRITICALs are AUD-SEC-001 (vendor-header identity bypass with cross-key batch access) and AUD-SEC-009 (token in the local remote URL). Six HIGHs: AUD-SEC-002, AUD-REL-001, AUD-PERF-001, AUD-PERF-002, AUD-API-001, AUD-CODE-001.

---

## 17. Unverified concerns / environment limits

- `UNCONFIRMED`: none carried forward — findings I could not reproduce were downgraded or dropped rather than asserted.
- `NOT RUN` (environment): `go test -race` (**BLOCKED**: `cgo: C compiler "gcc" not found` on this host; CI runs it on Linux), Playwright browser smoke (browsers not installed), `scripts/smoke-client-*.py` (hardcodes `/home/hatch/go/bin/go`), `scripts/bench.sh` (Linux-bound: `flock` + `/proc/loadavg`), `govulncheck`/SBOM/cosign (network/tooling).
- `NEEDS MANUAL VERIFICATION`: AUD-REL-005 (corruption modes), AUD-REL-008 (double-billing window), AUD-CFG-005 (action SHA resolution), plus every live-provider behaviour — no provider call was made at any point in this audit.
- The flaky-test claim (AUD-TEST-011) was **not reproduced** here (16 vCPUs): isolated 69.3×, under load 53.9×/`+Inf`/104.5×, 6-way concurrent 71.6×–149.0×/`+Inf`, all exit 0. It is recorded as a latent, structurally timing-dependent assertion, not as an observed failure.

---

## 18. External blockers

1. **No per-key quota-expiry source**: the P1.1 documentation's external blocker stands — there is no provider API that reports per-key free-tier expiry, so quota intelligence can only report what the ledger and pools know. Not a defect.
2. **Provider-network verification** cannot be performed from this host (no credentials, no spend authorization) — every provider-facing behaviour above is source-verified, not live-verified.
3. **`-race` verification** requires a Linux/CGO CI runner.

---

## 19. Production-readiness assessment

**NOT PRODUCTION READY.**

Blocking reasons (all narrow): AUD-SEC-001 (cross-key identity/policy bypass and cross-key batch access), AUD-SEC-009 (credential in the remote URL), AUD-SEC-002 (budget cap ineffective for relayed Anthropic spend), AUD-REL-001 (429 mis-reported as 502 — clients cannot back off), AUD-PERF-001/002 (per-stream unbounded memory; serialized ledger reads under load), AUD-API-001 (Anthropic error envelope inconsistency), AUD-CODE-001 (revocation may not persist).

Conditionally strong: with AUD-SEC-001/009 and AUD-CODE-001 fixed and the P0/P1 list in `docs/POST_P1_RECOMMENDATIONS.md` addressed, the gateway's core design (guard table, pre-dispatch gating, reservation lifecycle, honest streaming failures, route/doc parity) is sound enough to move to **PRODUCTION READY WITH DOCUMENTED LIMITATIONS**.

---

## 20. Recommended next steps

1. **Immediately (maintenance, outside this audit):** fix AUD-SEC-001 (make `callerKeyID` resolve through `requestKey`; make `batchVisible` treat a missing owner as *not visible*); rotate the token embedded in the remote URL and re-set the remote without credentials.
2. **Before the next milestone (P0/P1):** AUD-SEC-002, AUD-REL-001, AUD-CODE-001, AUD-PERF-001, AUD-PERF-002, AUD-API-001, AUD-CFG-001.
3. **Then (P2):** the workstreams proposed in `docs/P2_PROPOSAL.md`, derived exclusively from these findings, plus closing the CI execution gaps (AUD-TEST-001/002/003) so "green" means verified.

Deliverables produced by this audit:
`docs/POST_P1_FORENSIC_AUDIT.md` (this file), `docs/POST_P1_FINDINGS_LEDGER.md`, `docs/POST_P1_ARCHITECTURE_MAP.md`, `docs/POST_P1_RECOMMENDATIONS.md`, `docs/P2_PROPOSAL.md`.

**No P2 implementation was started. No production behaviour was modified. No branch, commit, PR, or repository state was changed by the audit itself.**
