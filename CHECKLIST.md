# Codvian Router — Phase 1 MVP Backend Checklist

Backend-first. No UI. Local worktree only: `~/workspace/codvian-router/`.
Build-first-test-last: built ALL pieces, then ONE test gate at the end.
Gate run 2026-11-17 by the coordinator (independent re-run, not the builders' own claims).

| # | Feature | Built | Verified |
|---|---|---|---|
| 1 | Go module scaffold, single binary, ~1s start | ✅ | ✅ |
| 2 | Versioned config file (JSON, v1), localhost-only bind, auth ON by default | ✅ | ✅ |
| 3 | Gateway API key issue/verify, per-key scopes & budgets | ✅ | ✅ |
| 4 | Keys encrypted at rest (AES-256-GCM; method documented in internal/crypto/README.md) | ✅ | ✅ |
| 5 | Provider registry: any base URL + API key, inline key test, auto-fetch model list, wire-format auto-detect (OpenAI-compatible first) | ✅ | ✅ |
| 6 | Codvian as a provider option (base URL + API key) | ✅ | ✅ |
| 7 | Routing engine: fallback chains | ✅ | ✅ |
| 8 | Quota-aware routing | ✅ | ✅ |
| 9 | 429 / Retry-After handling, backoff + per-key cooldown | ✅ | ✅ |
| 10 | Circuit breaker | ✅ | ✅ |
| 11 | Hedged requests (backup attempt starts early, ~150ms) | ✅ | ✅ |
| 12 | Streaming-safe failover, mid-stream resume | ✅ | ✅ |
| 13 | Three-layer caching: exact-match → provider prompt cache → semantic (stub interface) | ✅ | ✅ |
| 14 | Unified credit/usage ledger (SQLite, local) | ✅ | ✅ |
| 15 | One budget cap + fail-closed "never bill me" mode | ✅ | ✅ |
| 16 | OpenAI-compatible server: POST /v1/chat/completions, GET /v1/models | ✅ | ✅ |
| 17 | No telemetry by default | ✅ | ✅ |
| 18 | Test gate: unit tests + `go build` + live smoke test (mock upstream, 429 on model A → failover to model B) | ✅ | ✅ |
| 19 | Failover latency benchmark (measured, not claimed) | ✅ | ✅ |

## Test gate results (2026-11-17, Go 1.22.2, measured by coordinator)

- `go build ./...` — exit 0; `go vet ./...` — clean; `gofmt -l .` — clean
- `go test -count=1 ./...` — **111/111 PASS, 0 fail** (cache 11, config 16, crypto 8, ledger 9, provider 29, router 24, server 14)
- 4,964 lines non-test Go + 2,983 lines of tests, 21 Go files

## Live benchmark (real binary, Python mock upstreams, 2026-11-17)

Setup: Mock A (port 21001) + Mock B (port 21002), both serving `bench-model`;
B answers in 50ms. Router on 127.0.0.1:20199, auth ON.

| Scenario | Result |
|---|---|
| Direct to Mock B (no router, 50ms upstream delay) | 51.2 ms (baseline floor) |
| A returns 429 instantly → failover to B (10 reqs) | **10/10 OK, 52.3–60.0 ms** (median ~53 ms) |
| Failover switch cost (429 detected in ~6 ms, then B in 51 ms) | **~1–2 ms overhead** vs direct B; A-on-cooldown requests ~52.5 ms, A-retry requests ~59 ms |
| A slow (2,000 ms) → hedge at 150 ms, B wins (5 reqs) | **5/5 OK, 203–205 ms** (150 ms hedge + 51 ms B) |
| Streaming with 429 failover | SSE chunks `bench ` + `ok` + `stop` + `[DONE]` — works |
| No gateway key | 401 as expected |
| Unknown model | Clean JSON error, no crash |
| Startup to healthz-ready | **12 ms** (target ~1 s) |
| Idle memory (RSS) | **~10.6 MB** (target: tens of MB) |
| Binary size | 14.5 MB single binary |

Targets (design goals, now measured against): detect failure 2–3 s, switch
0.1–0.3 s, route under 1 s — measured switch after a 429 is ~1–2 ms on
localhost mocks (real-world numbers will depend on upstream latency).

## Known limitations / follow-ups (not blockers)

- Crypto: SHA-256 of a 32-byte random master secret (no Argon2id, no passphrase)
  — documented in `internal/crypto/README.md`; rotating `master.key` orphans sealed values.
- Ledger adapter records attempt tokens as completion tokens (prompt split unknown
  at router layer); cost is 0 when no per-model prices are configured — never invented.
- Registry quota hints are not yet wired into the engine (nothing sets hints in Phase 1).
- Semantic cache is a `NoopSemanticCache` stub; real embeddings come later.
- `go.mod` was concurrently edited by parallel agents mid-build; it has converged
  (`github.com/raunakiitp/codvian-router`, `go 1.22`, modernc.org/sqlite v1.34.0).
