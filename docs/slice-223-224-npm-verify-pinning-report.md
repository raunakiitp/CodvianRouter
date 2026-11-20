# Slice 223–224 — npm wrapper signature verification + GoReleaser pinning

Gate: 2026-11-18 (Go 1.27.4, Node v24.22.1). Closes the two honest
limitations from items 209–210: the npm wrapper did not verify
signatures, and GoReleaser was unpinned (`version: latest`).

## What was built

### (a) npm wrapper verifies before installing — `bin/codvian-router.js`

Same verification-first rule as `scripts/install.sh`, in the same
order, before anything is extracted or executed:

1. Download `checksums.txt` for the release.
2. If `cosign` is installed: verify the Sigstore keyless signature
   over `checksums.txt` (identity pinned to the release workflow +
   this exact tag:
   `https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/v<version>`)
   and **refuse on any failure** — including a bundle that cannot be
   downloaded. The signature is checked *before* the archive's hash
   is trusted.
3. Verify the archive's SHA-256 (Node `crypto`, stdlib only) against
   the manifest and **refuse on mismatch or on a missing entry**.

A failed verification deletes the downloaded archive and aborts
before extraction. No new dependencies: the wrapper stays
zero-dependency (Node stdlib only).

No-cosign behaviour (honest, mirrors `install.sh`): without cosign
the wrapper warns loudly and continues checksum-only, unless
`CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` is set, which refuses instead.
Checksum-only proves the download matches the manifest — it does
*not* prove the manifest came from the project.

Two further honest limits, also mirrored from `install.sh` and
documented in the wrapper header + `docs/RELEASE-SIGNING.md`:

- If `checksums.txt` itself cannot be downloaded, the wrapper warns
  loudly and installs **without** verification (a release without
  checksums is unexpected and should be reported, but blocking the
  install entirely would strand users on a transient GitHub error
  for that one file). The signature is checked whenever the manifest
  downloads; the gap is only the manifest being unreachable at all.
- A cached binary (`bin/.cache/`) is **not** re-verified on later
  runs — it was verified when downloaded. Delete `bin/.cache/` to
  force a fresh download + verification.

Also documented: the wrapper checks the binary's authentication
(signed `checksums.txt` → SHA-256), not the SBOM signature
(auditors use `scripts/verify-release.sh`), and
`CODVIAN_ROUTER_BINARY` bypasses download **and** verification
entirely (operator's own binary, their responsibility).

New testing-only overrides (used by the test suite, not normal use):
`CODVIAN_ROUTER_DOWNLOAD_BASE_URL` (download from a local server
instead of GitHub) and `CODVIAN_ROUTER_CACHE_DIR` (cache elsewhere).
Whatever they point at is still signature- + checksum-verified
exactly as a GitHub download would be. The download function now
supports `http:` URLs as well as `https:` for the same reason (the
local test server is plain HTTP).

### (b) Release toolchain pinned — `.github/workflows/release.yml`

No `latest`, no floating major tag, no floating runner image. Every
`uses:` entry is pinned to the full 40-char commit SHA of a tagged
release, with the version in a comment next to it:

| Tool | Pinned to | Commit SHA |
|---|---|---|
| `actions/checkout` | v4.4.0 | `11d5960a326750d5838078e36cf38b85af677262` |
| `actions/setup-go` | v5.6.0 | `40f1582b2485089dde7abd97c1529aa768e1baff` |
| Go (via setup-go) | 1.27.4 (exact patch) | — |
| `sigstore/cosign-installer` | v3.10.1 | `7e8b541eb2e61bf99390e1afd4be13a184e9ebc5` |
| `anchore/sbom-action/download-syft` | v0.24.3 | `66cbf4bc1f1c0d2edc94016e65bc221b6bb0ad6c` |
| `goreleaser/goreleaser-action` | v6.4.0 | `e435ccd777264be153ace6237001ef4d979d3a7a` |
| GoReleaser (via the action) | v2.20.2 (exact tag) | `1bf59683fabedd7b446d089ec31b1e343b472ce6` |
| `actions/attest-build-provenance` | v2.4.0 | `e8998f949152b193b063cb0ec769d69d929409be` |
| `docker/login-action` | v3.7.0 | `c94ce9fb468520275223c153574b00df6fe4bcc9` |
| Runner | `ubuntu-24.04` (both jobs) | — |

All SHAs were verified against the upstream repos twice
(`api.github.com/repos/<owner>/<repo>/commits/<tag>` and
`git ls-remote ... refs/tags/<tag>`; annotated tags dereferenced
with `^{}` to the commit — this caught one real discrepancy: the
`sbom-action` v0.24.3 tag ref points at the annotated tag object
`fffec3d8…`, the commit is `66cbf4bc…`; the commit is what `uses:`
needs).

Deliberate scope choice: the majors pinned are the ones the
workflow was built and reviewed against (checkout v4, setup-go v5,
cosign-installer v3, attest v2, goreleaser-action v6). Newer majors
exist (checkout v7, setup-go v7, cosign-installer v4, attest v4,
goreleaser-action v7) and were *not* adopted: a major bump is
untestable until the first real tag, so it would only fail at
release time. A major bump is a deliberate, reviewed change; the
how-to (resolve SHA, update SHA + comment together) is in the
workflow header.

Residual, stated honestly: the actions download their tool binaries
(GoReleaser, cosign, syft) over HTTPS from GitHub releases, and none
of those actions documents an independent checksum check of the
download — verified in source for `goreleaser-action` v6.4.0
(`src/goreleaser.ts` and `src/github.ts` contain no checksum/sha256
check). The pin covers the action code and the exact tool version,
not a hash of the tool binary. The release job's own `verify` job is
the tripwire: it re-verifies the published artifacts exactly as a
user would, and a green build with a red verify is a failed release.

`install.sh` gained one testing-only override,
`CODVIAN_ROUTER_DOWNLOAD_BASE_URL`, so the same local harness can
prove the installer path against controlled artifacts. Default
behaviour is unchanged; whatever the override points at is still
signature- + checksum-verified exactly as a GitHub download would
be. The `latest` version-resolution default in `install.sh` /
`verify-release.sh` was deliberately left alone: it resolves through
the GitHub API to a concrete tag *before* any download, and
everything downloaded for that tag is then signature-verified — it
is version resolution, not an unpinned tool.

### (c) Tests — `bin/codvian-router.test.js` (`npm test`, 9 tests)

Stdlib-only `node:test` suite. A local HTTP server serves a real
`tar.gz` (containing a stub binary that prints a marker) + a real
`checksums.txt` (real SHA-256) + a dummy Sigstore bundle, so the
tests run the **real** wrapper and the **real** `install.sh`
against controlled artifacts — no network, no published release.
Cosign is a stub script on PATH (records argv, exits 0 or 1): a
test double for an external tool, not a fake verification — the
wrapper's own code paths are what is under test.

| # | Test | Result |
|---|---|---|
| 1 | Untampered download verifies (checksum-only, no cosign) and runs | ✅ |
| 2 | Tampered archive is refused (SHA-256 mismatch, binary never runs, archive removed from cache) | ✅ |
| 3 | Tampered checksums entry is refused | ✅ |
| 4 | `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses without cosign | ✅ |
| 5 | Cosign present + valid signature verifies with the pinned identity (asserts `verify-blob` argv contains the exact pinned workflow/tag identity) | ✅ |
| 6 | Cosign present + invalid signature refuses | ✅ |
| 7 | Cosign present + missing signature bundle refuses | ✅ |
| 8 | `install.sh` installs the untampered artifact | ✅ |
| 9 | `install.sh` refuses a tampered download | ✅ |

## Gate results

- `npm test` — **9/9 pass, 0 fail** (Node v24.22.1; `node --check`
  on the wrapper clean). `package.json` `test` now runs the suite.
- Workflow YAML parses (`yaml.safe_load`, jobs `release` +
  `verify`); all 11 `uses:` entries are full 40-char SHAs; no
  `latest` tool version remains (`grep latest` hits only the
  pinning-policy comment, the `:latest` Docker image tag note — see
  below — and the `install.sh` version-resolution default, which is
  out of scope as above).
- `go build ./...` — OK. `go vet ./...` — clean. (No Go code was
  touched; this is a tree sanity check. The GOPATH/GOROOT warning
  is a pre-existing environment quirk.)
- Live proof: tests 1, 2, 5, 8, 9 above run the real install paths
  against local untampered + tampered artifacts — untampered
  installs and executes, tampered is refused before extraction.

## Real bugs found by this slice's own work

1. **Test-suite deadlock (found, fixed):** the first version of
   the suite used blocking `spawnSync` to run the wrapper while the
   local HTTP server lived in the same process — the wrapper waited
   for the server to answer its download while the server waited for
   `spawnSync` to return. The run hung with zero output for 2+
   minutes (diagnosed from `/proc`: wrapper sleeping with a 0-byte
   archive open, server holding the unread request). Fixed with an
   async `spawn` helper (`runAsync`); the comment in the test file
   records why async is load-bearing, not style. All 9 tests then
   passed in ~3 s.
2. **Fixture tar gid vs this VM's userns (found, fixed):** the
   fixture tar recorded gid 65534 (inherited from the setgid
   `$HOME/.tmp` directory) and extraction-side `chown` to it fails
   here ("Operation not permitted") — the same userns limitation
   recorded for Docker in CHECKLIST item 75. Fixed in the test
   fixture only (`tar --owner=0 --group=0`); no product code changed.

## Concurrent-slice interaction (flagged, not owned)

While this slice worked, a concurrent slice landed Docker image
signing in `.github/workflows/release.yml` (+ `docker_signs` in
`.goreleaser.yml`, `scripts/verify-image.sh`). Its
`docker/login-action` entries were already SHA-pinned; the SHA was
independently verified here (`v3.7.0` →
`c94ce9fb468520275223c153574b00df6fe4bcc9` via `git ls-remote`) and
is included in the pin table above. No file owned by that slice was
edited. One item was left as that slice's decision: the published
image also carries a `:latest` tag (`.goreleaser.yml` image
templates). That is a user-facing convenience tag for the image,
not a tool version — but it is a floating tag in the release path,
and changing it would change what that slice publishes.

## Honest limitations (what is still unproven)

- **No release exists yet.** The pinned workflow cannot be
  exercised until the first real tag is pushed (Raunak's call
  only). The `verify` job is the tripwire if it fails at release
  time; the local tamper proof used test files, not a GitHub
  release — same limitation as items 209–210, unchanged.
- **Tool-binary downloads carry no independent checksum check**
  (see residual above). The pin covers action code + exact tool
  version.
- **No tag, release, or npm publish was created** by this slice.
- Dashboard: no new UI — releases are not served by the gateway
  (same as items 209–210). The README install-table note + docs
  are the status note.

## Files changed

| File | Change |
|---|---|
| `bin/codvian-router.js` | Verification-first install: cosign signature (when present, refuse on failure) + SHA-256, refuse on mismatch/missing entry; `CODVIAN_ROUTER_REQUIRE_SIGNATURE`, `CODVIAN_ROUTER_DOWNLOAD_BASE_URL`, `CODVIAN_ROUTER_CACHE_DIR` overrides; `http:` downloads for the local test server |
| `bin/codvian-router.test.js` | New: 9-test `node:test` suite (local-server tamper proof for wrapper + install.sh) |
| `package.json` | `test` script now runs the suite |
| `scripts/install.sh` | Testing-only `CODVIAN_ROUTER_DOWNLOAD_BASE_URL` override; default behaviour unchanged |
| `.github/workflows/release.yml` | All actions SHA-pinned, GoReleaser v2.20.2, Go 1.27.4, `ubuntu-24.04`; pinning-policy header comment |
| `docs/RELEASE-SIGNING.md` | Pinned-toolchain table, npm-wrapper verification section, limitations updated (209–210 items closed, residuals stated) |
| `docs/INSTALL.md` | §4 npm section describes verification-first install + overrides |
| `CHECKLIST.md` | Items 223–224 → Built, Verified |
| `README.md`, `README.v2.md` | Status line: items 223–224 |
| `docs/slice-223-224-npm-verify-pinning-report.md` | This report |
