# Push manifest — semantic-cache wiring slice (CHECKLIST.md items 55–58)

Date: 2026-11-17 (audit run ~20:36 IST)
Repo: https://github.com/raunakiitp/CodvianRouter — main branch only
Worktree: `~/workspace/codvian-router/` (Go module `github.com/raunakiitp/codvian-router`)
Auditor: subagent (no browser access — cannot check GitHub; see Status rules below)

## Status rules

- **CONFIRMED ON MAIN** — only for the 3 files named by the parent as already
  confirmed on main. The three confirmation commits supplied by the parent are
  `5b4819d`, `063a854`, `438c389` (per-file commit mapping was not supplied, so
  none is asserted here).
- **UNCONFIRMED** — push status unknown. The browser push task for these was
  interrupted. Do NOT treat these as on main until the parent verifies them on
  GitHub. The local worktree has no `.git` directory, so no local git state
  exists to compare against.

## Files (7 total)

| # | Repo path | Contents (one line) | Push status |
|---|---|---|---|
| 1 | `cmd/codvian-router/main.go` | Wires the slice at startup: `ServeCache` ON + `degrade.WrapWithSemantic(engine, exact, semantic, policy)` | CONFIRMED ON MAIN |
| 2 | `internal/cache/cache.go` | `ExactCache` + `LocalSemanticCache` (hashed bag-of-words, cosine ≥ 0.85); doc ties it to `WrapWithSemantic` + `Policy.ServeCache` | CONFIRMED ON MAIN |
| 3 | `internal/server/server.go` | Honest cache-hit signalling: `X-Codvian-Cache: exact\|semantic` (+ `X-Codvian-Cache-Similarity`), `provider="cache"` metrics attribution | CONFIRMED ON MAIN |
| 4 | `internal/degrade/degrade.go` | `Policy.ServeCache` + `WrapWithSemantic` proactive exact→semantic serving, `cacheable()` guard, stale-fallback safety fix (refuses tool-using requests) | UNCONFIRMED |
| 5 | `internal/degrade/degrade_test.go` | 7 new `ServeCache` tests (exact hit, semantic paraphrase, miss, tool-request skip, off-by-default, stream replay, fresh-success clears hit) | UNCONFIRMED |
| 6 | `internal/server/server_test.go` | 3 new server cache tests (cache headers + similarity header + `provider="cache"` metrics) | UNCONFIRMED |
| 7 | `CHECKLIST.md` | Items 55–58 marked Built/Verified + the semantic-cache wiring gate section | UNCONFIRMED |

## How the 7 were identified

- Narrow grep for `ServeCache` / `WrapWithSemantic` / `X-Codvian-Cache` hits
  exactly 6 Go files (the 6 above); `internal/metrics/metrics.go` also
  mentions `cache_hits_total` but its mtime is 2026-11-17 11:04–11:05, hours
  before this slice, so it is NOT part of it.
- `CHECKLIST.md` mtime 2026-11-17 14:51 sits inside the slice's tight
  mtime cluster (14:46–14:54) and carries items 55–58 + the gate section —
  it is the 7th file.
- Slice mtimes (UTC, `ls`): `main.go` 14:46, `cache.go` 14:46,
  `degrade.go`/`degrade_test.go` 14:47, `CHECKLIST.md` 14:51,
  `server.go`/`server_test.go` 14:54.

## Local gate re-run (this audit, Go 1.27.4)

- `go build ./...` — OK
- `go vet ./...` — OK
- `gofmt -l cmd internal web` — clean (no output)
- `go test -count=1 ./internal/degrade/... ./internal/server/... ./internal/cache/... ./cmd/...`
  — all 4 packages ok: **93 top-level tests PASS, 0 FAIL**
  (degrade 35, server 33, cache 21, cmd 4; +8 passing subtests)
- Nothing failed, so nothing was fixed. `CHECKLIST.md` was not edited
  (no gate result changed).

## Push order (suggested, for the parent's browser push)

Push the 4 UNCONFIRMED files; the 3 CONFIRMED files are already on main and
must not be re-pushed as new work:

1. `internal/degrade/degrade.go`
2. `internal/degrade/degrade_test.go`
3. `internal/server/server_test.go`
4. `CHECKLIST.md`

Note: `internal/server/server.go` (CONFIRMED) and
`internal/server/server_test.go` (UNCONFIRMED) are separate files — push only
the test file from that package.
