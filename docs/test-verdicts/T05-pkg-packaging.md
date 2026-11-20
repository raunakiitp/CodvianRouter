# T05 — PKG-1/2/4/5 Packaging Fixes (GAP-TRACK §8) — VERDICT

- **Date:** 2026-11-19 IST (~19:15–19:25 IST)
- **Tester:** verify-only subagent (T05)
- **Worktree:** `~/workspace/codvian-router/`, HEAD `6831302`, dirty tree (PKG files modified vs HEAD; see below)
- **Overall verdict: PASS** (all four rows PASS; one nit on PKG-1, one note on untracked files)

## Verdict per row

| Slice | Verdict |
|---|---|
| PKG-1 Dockerfile + Dockerfile.release VOLUME for config dir + header example mounts both named volumes | **PASS** (nit: see below) |
| PKG-2 `.github/workflows/ci.yml` runs `scripts/ci.sh` on push + PR, pins match `release.yml` | **PASS** |
| PKG-4 `scripts/install.sh` + `bin/codvian-router.js` fail CLOSED on unreachable `checksums.txt` (retry + backoff, then refuse) and re-verify cached binary sha256 on every run | **PASS** |
| PKG-5 Makefile (help/ci/build/test/docker → `scripts/ci.sh`) + `.goreleaser.yml` nfpms (deb/rpm) and brews blocks | **PASS** |

## What was verified (commands run + results)

1. `export PATH=$HOME/go/bin:$PATH; export TMPDIR=$HOME/.tmp; go version`
   → `go version go1.27.4 linux/amd64` (matches the 1.27.4 pin in `release.yml` / `ci.yml`).
2. `gofmt -l cmd/codvian-router/ internal/` → flagged `internal/autorouter/autovariants_test.go` and `internal/mcp/mcp_test.go`.
   These are sibling M9/A2A test files, NOT T05 files (T05 touches no Go files: Dockerfile, Dockerfile.release, ci.yml, install.sh, codvian-router.js, Makefile, .goreleaser.yml). Recorded as sibling-churn note, not a T05 failure. `gofmt` on T05's own files is N/A (no Go files touched).
3. `make help` → exit 0, lists build / ci / clean / docker / help / release-check / test / vet with descriptions.
4. ci.sh step 5 (docker go-version match) logic reproduced: `PINNED_GO=1.27.4` read from `release.yml`; `grep -c "golang:1.27.4-bookworm"` → `Dockerfile:1`, `Dockerfile.release:1`. PASS.
5. `bash -n scripts/install.sh` → syntax OK. `node --check bin/codvian-router.js` → syntax OK.
6. `python3 -c "import yaml; ..."` → `YAML OK: ci.yml + .goreleaser.yml parse`.
7. `make release-check` → `goreleaser not installed — falling back to YAML parse` / `YAML OK: .goreleaser.yml parses` (exit 0).
8. `node --test bin/codvian-router.test.js` — run TWICE:
   - Run 1 (37 s wall for the tail I captured): 14/16 passed; 2 failures were the real-binary smokes (`install.sh + real binary`, `npm wrapper + real binary`) failing at `go build` with `internal/router/combo.go:214:15: e.comboPanelBudgetCheck undefined` — sibling churn breaking the tree, NOT PKG-4 code.
   - Run 2 (212 s): **16/16 passed**, including both real-binary smokes (124.8 s + 37.0 s) — the sibling breakage was fixed in the tree between the two runs. Direct PKG-4 tests all green in both runs: `unreachable checksums.txt is refused after retries (fail closed)` (both JS launcher and install.sh variants), `tampered cached binary is caught and re-downloaded, never executed`, `cached binary without a checksum sidecar is re-downloaded`, `tampered archive/checksums refused`, cosign valid/invalid/missing-signature cases.
9. Docker build NOT run — no Docker daemon in this VM. Honest limit: the `VOLUME` declarations and header examples were verified by reading the files only; no `docker build` / `docker run --rm init` was executed. Do NOT treat this verdict as proof a Docker build passes.

## Evidence (file:line)

- **PKG-1:** `Dockerfile:117` — `VOLUME ["/home/nonroot/.local/share/codvian-router", "/home/nonroot/.config/codvian-router"]`. `Dockerfile.release:94` — identical VOLUME line. Header examples in `Dockerfile:11-12` (`docker run --rm ... init` mounting both `codvian-router-data` and `codvian-router-config`), `Dockerfile:15-17` (serve, both volumes), `Dockerfile:21-23` (published port, both volumes); `Dockerfile:26-31` explains both volumes are required on every run and the gateway key dies if the config volume is omitted.
- **PKG-2:** `.github/workflows/ci.yml` — `on: push` + `pull_request`; `runs-on: ubuntu-24.04`; `actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0`; `actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5.6.0` with `go-version: "1.27.4"`; final step `run: bash scripts/ci.sh`. Pins verified identical to `.github/workflows/release.yml:115,118,123,125` (same SHAs, same Go 1.27.4, same runner).
- **PKG-4 install.sh:** retry loop `scripts/install.sh:153-159` (`curl -fsSL -o "$TMPDIR/checksums.txt"`, 3 attempts, `sleep $((ATTEMPT * 2))` backoff); fail-closed `scripts/install.sh:163` — `die "could not download checksums.txt for $TAG after 3 attempts — refusing to install a release whose manifest cannot be checked..."`. Tarball sha256 verified at `scripts/install.sh:180-186` (`EXPECTED` from checksums.txt, die if absent, `Checksum verified (sha256)`). Note: install.sh has no binary cache (no `cache` references found) — the cached-binary re-verification requirement applies to the JS launcher, which is where the cache lives.
- **PKG-4 codvian-router.js:** manifest retry `bin/codvian-router.js:249-272` (3 attempts, 1s/2s backoff, then throws `could not download checksums.txt for ${tag} after 3 attempts ... refusing to install`); sidecar write `bin/codvian-router.js:205-210` (`writeBinaryChecksum`); re-verification `bin/codvian-router.js:212-230` (`verifyCachedBinary`, returns false on missing/malformed/mismatched sidecar); call site on every cached run `bin/codvian-router.js:370-390` (`if (fs.existsSync(binaryPath)) { if (verifyCachedBinary(binaryPath)) { ... return binaryPath; } ... discard + re-download }`).
- **PKG-5:** `Makefile` — targets `help`, `build`, `test`, `vet`, `ci`, `clean`, `docker`, `release-check`; `ci` target runs `./scripts/ci.sh`. `.goreleaser.yml:188-198` — `nfpms:` block (`id: codvian-router`, `formats: [deb, rpm]`); `.goreleaser.yml:210` — `brews:` block (`name: codvian-router`, `directory: dist/brew`).
- **Git state:** `git diff --stat HEAD` shows PKG changes in `.goreleaser.yml`, `Dockerfile`, `Dockerfile.release`, `bin/codvian-router.js`, `scripts/install.sh` (modified, uncommitted). `Makefile` and `.github/workflows/ci.yml` are **untracked** (`??` in `git status --short`) — present in the worktree, not yet committed.

## Failures / nits

- **NIT (PKG-1):** `Dockerfile.release` has NO header `docker run` example at all (its only `docker run` mention is `Dockerfile.release:103`, a comment about ENTRYPOINT/CMD). The functional requirement (VOLUME for both dirs) is met in both files, and the header examples that do exist (in `Dockerfile`) mount both named volumes — but a user reading `Dockerfile.release` alone gets no example showing both `-v` mounts. Suggest adding the same two-volume example to the `Dockerfile.release` header. Not a FAIL.
- **NOTE (sibling churn, not T05):** `gofmt -l` flags `internal/autorouter/autovariants_test.go` and `internal/mcp/mcp_test.go` (modified vs HEAD by other work); and the first `node --test` run hit `internal/router/combo.go:214:15: e.comboPanelBudgetCheck undefined` breaking `go build` of the real binary — fixed in-tree before the second run (16/16 green). Neither is a PKG defect.
- **LIMIT:** Docker build/run not verified in this VM (no daemon). `VOLUME` + examples verified by file read only.

## Fallback slice

See `docs/test-verdicts/T05-pkg3-lifecycle.md` (written after this verdict).
