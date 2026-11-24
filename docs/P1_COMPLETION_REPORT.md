# P1 Completion Report (P1.1–P1.9)

Consolidated report for the P1 continuous-implementation programme. Every
claim below is tied to source, a commit, or a command whose exact result is
quoted. No live-provider verification is claimed anywhere.

## Consolidated final verification campaign

Run on `main` @ `d2b699b` (after the last P1 merge), Windows 11 / Go 1.27.4.

| Check | Command | Result |
| --- | --- | --- |
| gofmt | `git show HEAD:<f> \| tr -d '\r' \| gofmt -l` for every file touched by P1.x | **PASS** (no output) |
| build | `go build -buildvcs=false ./...` | **PASS** (exit 0) |
| vet | `go vet -buildvcs=false ./...` | **PASS** (exit 0, no diagnostics) |
| full tests | `go test -buildvcs=false -count=1 ./...` | **PASS** — `FULL_GO_TEST_EXIT=0`, **52** packages `ok`, **0** `FAIL`, 3 packages with no test files |
| race | `CGO_ENABLED=1 go test -race ./internal/tokencount/` | **BLOCKED** — `cgo: C compiler "gcc" not found`; this host has no C toolchain. CI runs `go test ./... -race -count=1` on Linux and passes. |
| route/guard consistency | `bash scripts/check-route-guard.sh` | **PASS** (`ok internal/server`) |
| MCP/A2A integration | part of `go test ./...` (`internal/a2a`, `internal/server` MCP suites) | **PASS** |
| CLI compatibility | `go test ./cmd/codvian-router/ -run TestP13` | **PASS** |
| compression fidelity | `internal/compress/evalharness` + `cmd/codvian-bench` (in `go test ./...`) | **PASS** |
| provider capability consistency | `internal/capability` (in `go test ./...`) | **PASS** |
| org/team isolation | P1.6 suites (in `go test ./...`) | **PASS** |
| cost estimation/reconciliation | P1.7 suites (in `go test ./...`) | **PASS** |
| context/output boundaries | P1.8 suites (in `go test ./...`) | **PASS** |
| eval persistence/regression | P1.9 suites (in `go test ./...`) | **PASS** |
| client protocol smokes | `scripts/smoke-client-*.py` | **NOT RUN** — the shared lib pins the CI Go path (`/home/hatch/go/bin/go`); scripted for the Linux runner, not portable to this host |
| dashboard JavaScript | `node --check bin/codvian-router.js` **PASS**; `node --test bin/codvian-router.test.js` | **NOT RUN** — the wrapper's 16 integrity tests require release artifacts (binaries + checksums.txt) that only exist after a release; they fail locally with no artifacts, which is not a router signal |
| browser smoke | `tests/playwright` | **NOT RUN** — needs `npx playwright install --with-deps chromium`; covered by the `browser-smoke` CI workflow |

Environment note: a bare `gofmt -l cmd internal` on this Windows checkout
flags every file because the working copy is CRLF while the index is LF
(`git ls-files --eol` → `i/lf w/crlf`). The gofmt gate is therefore
evaluated against the committed (LF) content, which is what CI checks.

## Per-block summary

### P1.1 — Free-tier budget intelligence & quota dashboard
- **State before:** quota/budget accounting existed in the ledger; no
  intelligence surface.
- **Gaps:** no API or dashboard view of quota headroom across providers.
- **Delivered:** `internal/server/quota_intelligence.go`,
  `GET /api/quota/intelligence`, dashboard section, tests.
- **Status:** COMPLETED (prior delivery; present on `main`, covered by the
  full suite run above).

### P1.2 — MCP/A2A ecosystem expansion
- **Gaps:** the MCP server exposed the core tools but not the ecosystem
  expansion set (provider/capability discovery, quota intelligence, budget
  forecasting, cost estimation, route preview, compression analytics, eval
  management, org reporting) or the A2A skills.
- **Delivered:** 8 expansion MCP tools + 6 A2A skills, tool order pinned by
  tests. Status: COMPLETED (prior delivery; in the full suite).

### P1.3 — CLI integration expansion — merge `d2b699b`, PR #72
- **Gaps (confirmed in source):** the gateway credential was accepted **only**
  as `Authorization: Bearer`, so Anthropic-style clients (`x-api-key`) and
  Gemini SDKs (`x-goog-api-key`) got 401 on `/v1/messages` and the Gemini
  surface with a valid key; there was no client compatibility matrix and no
  mechanism preventing an unsupported client from being claimed as supported.
- **Delivered:** one `requestKey()` accepting Bearer / `x-api-key` /
  `x-goog-api-key` (non-Bearer `Authorization` schemes are never keys), used
  by auth, scope enforcement and the 401 diagnostics;
  `codvian-router clients [--json]` printing dialect/endpoint/credential plus
  the test that pins each client's requirement;
  `TestP13ClientMatrixClaimsMapToRealTests` fails the build when a `verified`
  row cites a nonexistent test, a `partial` row lacks a caveat, or a
  credential is not one the server accepts; `docs/client-compatibility-matrix.md`.
- **Tests:** 6 server auth tests + 3 cmd matrix tests; `clients` and
  `clients --json` executed for real.
- **CI:** run 45423080751/45423102425 `ci` pass 7m14s/7m7s; playwright,
  sbom-and-vuln, bench-smoke all pass.
- **Limits:** no live client binary is driven in CI (stated in the matrix);
  Aider and Zed remain `partial`.

### P1.4 — Advanced compression & fidelity benchmark — merge `6b27042`, PR #66
- **Delivered:** `internal/compress/evalharness` (fidelity, long-context,
  explicit thresholds) + `cmd/codvian-bench compression`,
  `docs/compression-fidelity-benchmark.md`. Covered by the full suite.

### P1.5 — Provider capability metadata & discovery — merge `9fd7a1d`, PR #67
- **Delivered:** `internal/capability` registry with provenance,
  `GET /api/capabilities`, `POST /api/capabilities/refresh`,
  `GET /api/providers/{id}/capabilities`, and the
  `routing.require_capabilities` gate on streaming and non-streaming paths;
  `docs/provider-capability-registry.md`. Included in the full suite.

### P1.6 — Organization/team request-path enforcement — merge `08543ca`, PR #68
- **Delivered:** a single org/team policy gate evaluated before dispatch on
  chat, Messages, Responses, legacy completions, Gemini, embeddings, images,
  audio, media, A2A tasks and batch items; denial auditing and persistence;
  org budget (`400 org_budget`); negative tests per dialect;
  `docs/org-team-request-path-enforcement.md`.

### P1.7 — Pre-dispatch cost estimation — merge `664c17b`, PR #69
- **Gaps:** the estimate ignored tools, multimodal parts, cached/cache-write
  rates, provider selection and requested output tokens; `reconcileCost` was
  dead code that marked a run "reconciled" on usage alone;
  `callerKeyIDFromContext` always returned `""`.
- **Delivered:** wire-framed calibrated counting, provider → model →
  models.dev pricing with every known rate, explicit unknown-price path,
  output-token reservation reporting, read-only budget check;
  `POST /api/cost/reconcile` with `usage_missing` / `actual_unknown` /
  `estimate_unknown` / `reconciled` statuses and variance only when both
  sides are priced; implemented `callerKeyIDFromContext`; hardened
  `writeJSON` (encode before headers → 500 instead of a silent empty 200).
- **Evidence:** 7 focused tests green; full suite green; `docs/cost-estimation-reconciliation.md`.

### P1.8 — Context/output guardrails — merge `41a50b2`, PR #70
- **Gap:** embeddings/rerank resolved their chain and dialled directly, so an
  input larger than every known window was sent upstream (and failed there,
  after a provider call). The chat context-fit gate already existed.
- **Delivered:** `applyEmbeddingFit`/`filterEmbeddingFit`/`EmbeddingContextTooLarge`
  on both surfaces — refuse before dispatch when no *known* window fits,
  never refuse on an unknown window, drop only the too-small attempts; 400
  mapping in `handleEmbedEngineError`; `docs/context-output-guardrails.md`.
- **Evidence:** 5 router + 2 server tests green.

### P1.9 — Scheduled evaluation dashboard — merge `3b8cc68`, PR #71
- **State before:** the P3-B scheduler, store/persistence, regression
  detection, duplicate-run prevention, webhooks and CRUD/history already
  existed and were preserved.
- **Gaps:** `Run.FailedCases` held only IDs (a failure was visible but not
  triageable); no trend view.
- **Delivered:** `Run.Failures` (case ID, reason, missing substrings,
  latency), persisted and reloaded; `GET /api/eval/schedules/{id}/trends`
  with a chronological series, baseline mean, latest, delta and direction
  (`unknown` until two completed runs); a failed/case-less run never enters
  the baseline or the latest figure; guard row + 347-route pins;
  `docs/scheduled-evaluation-dashboard.md`.
- **Evidence:** 5 eval + 3 server tests green.

## NOT RUN / BLOCKED (explicit)

1. `go test -race` — **BLOCKED**: no C compiler on this host.
2. `scripts/smoke-client-*.py` — **NOT RUN**: CI-path-only script.
3. `npm test` (wrapper integrity) — **NOT RUN**: needs release artifacts.
4. Playwright browser smoke — **NOT RUN**: browsers not installed locally.

None of these was reported as PASS. They are all exercised by CI workflows
(`ci`, `browser-smoke`) on `main`.

## P0 deferred items

P0 was delivered in an earlier campaign (merge `07d48b3`, PR #65 and the
preceding P0 work). No P0 item was reopened or altered by this programme.

## P2

**P2 was not started.** No P2 work, branch, or commit exists in this
programme; all work above is P1.1–P1.9 plus the P0 fixes already on `main`.

## Programme status

All nine P1 blocks have implementation evidence wired into the live request
path, and the consolidated verification campaign passes on `main` except the
four environment-bound items above, each reported as NOT RUN/BLOCKED rather
than PASS.
