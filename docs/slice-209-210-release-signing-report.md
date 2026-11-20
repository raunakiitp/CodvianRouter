# Slice 209–210 — release signing pipeline: gate report

Date: 2026-11-18. Go 1.27.4. cosign v3.1.4 (downloaded for this gate;
keyless signing itself could not run on this VM — see Limitations).
No tag or release was created. Nothing was pushed.

## What was built

- **Choice: Sigstore cosign keyless** (over minisign/GPG). No long-term
  signing key exists anywhere: the certificate is minted per release by
  Sigstore, bound to the release workflow + tag ref, and logged in the
  public Rekor transparency log. Minisign was rejected because its
  long-term key in a GitHub Secret is exactly the stealable secret in
  the LiteLLM Oct 2026 compromise. Full justification + rejected
  alternatives: `docs/RELEASE-SIGNING.md`.
- **`.goreleaser.yml`**: `signs` over `checksums.txt` AND each SBOM
  (Sigstore bundle `<artifact>.sigstore.json`, the documented GoReleaser
  keyless pattern); `sboms: - artifacts: archive` (syft, SPDX-JSON).
- **`.github/workflows/release.yml`** (new): tag `v*.*.*` only; cosign +
  syft installed; GoReleaser publishes; `actions/attest-build-provenance`
  attests every artifact (SLSA); a second `verify` job downloads the
  published release and runs `scripts/verify-release.sh` with retries.
- **`scripts/verify-release.sh`** (new): signature → checksums → SBOM
  signature + JSON → attestation; remote mode, `--local DIR` mode,
  `--key PUBKEY` keypair mode (test-only). Exits non-zero on any failure.
- **`internal/release`** (new, stdlib only): checksum manifest
  generate/verify (sha256sum-compatible, sorted; rejects malformed,
  duplicate, traversal, non-canonical entries) + CycloneDX 1.6 SBOM
  generation from `go list -m -json all` (replace directives honoured,
  unknown versions never invented).
- **`cmd/release-sbom`** (new) + **`scripts/release-sbom.sh`** (new):
  dependency-free local SBOM path (syft fallback).
- **`codvian-router verify-release [DIR]`** (new subcommand): offline
  checksum check; documented as integrity-only, never a substitute for
  the signature.
- **`scripts/install.sh`**: verifies the cosign signature over
  `checksums.txt` when cosign is present (refuses on failure, before
  trusting any checksum); warns loudly + continues checksum-only
  without cosign, unless `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses.
- **Docs**: `docs/RELEASE-SIGNING.md` (design, threat model, exact
  verify commands, compromise procedure, limitations);
  `docs/INSTALL.md` §§5–6 + status block now point at verification
  FIRST; README install table notes signed releases (honest: none yet).
- **Dashboard**: no new UI (releases are not served by the gateway) —
  the README note + docs are the status note, per the task.

## Gate (measured by this run)

- `go build ./...` — OK; `go vet ./...` — clean; `gofmt -l` clean on
  this slice's files (3 files flagged belong to concurrent slices —
  `internal/plugins/builtins.go`, `internal/plugins/plugins.go`,
  `internal/server/server.go` — left untouched).
- YAML: `.goreleaser.yml` + `.github/workflows/release.yml` parse OK.
- `sh -n` clean on `verify-release.sh`, `install.sh`,
  `release-sbom.sh`.
- `go test -count=1 ./...` — **exit 0, 38 packages ok, 0 FAIL**
  (log `$HOME/.tmp/fulltest-209-210.log`); verbose recount
  **1,919 PASS events (1,515 top-level + 404 subtests), 0 FAIL**
  (log `$HOME/.tmp/fulltest-209-210-verbose.log`). The count moved
  from earlier gates (1,714) because concurrent slices landed tests
  while this gate ran; the numbers above are this run's own recount.
- `go test -count=1 -race` on touched packages
  (`internal/release`, `cmd/codvian-router`) — clean.
- This slice's tests: 12 top-level funcs in `internal/release`
  (incl. 10 malformed-manifest subtests), incl.
  `TestSBOMFromRealModuleGraph` (SBOM smoke on the real module graph:
  root + `golang.org/x/crypto` + `modernc.org/sqlite` present) and
  `TestGenerateMatchesSha256sum` (the real `sha256sum -c` accepts our
  manifest). SBOM smoke via `scripts/release-sbom.sh v0.1.0`: 34
  components, valid JSON, root correct.
- Two real bugs caught by this slice's own tests (both fixed in-slice):
  (1) `effectiveVersion` returned the replaced-away version for a
  local-path replace instead of "unknown"; (2) `ParseChecksums`
  stripped all trailing newlines, so a trailing blank line was
  accepted. Both are test-proven (fail before, pass after).

## Tamper proof (real cosign, real binary, local test files)

Fixture: a real built binary tarred as
`codvian-router_0.1.0_linux_amd64.tar.gz` + CycloneDX SBOM +
`checksums.txt`, in `$HOME/.tmp/tamper-209-210/` (scratch, not in the
repo). Signatures made with a throwaway local cosign keypair
(cosign v2.4.3 — see Limitations for why v3 could not sign here).

- **A — untampered:** `verify-release.sh --local … --key …` →
  `VERIFIED`, exit 0 (signature + checksums + SBOM signature + JSON
  all OK). `codvian-router verify-release` → `verified: 2 file(s)`,
  exit 0.
- **B — tampered archive** (one byte appended): script exit 1,
  `SHA-256 MISMATCH`; subcommand exit 1 naming the file with expected
  vs actual hash. The valid signature over `checksums.txt` did NOT
  save the tampered file — the checksum layer caught it.
- **C — tampered SBOM, SBOM listed in checksums.txt:** script exit 1,
  SBOM checksum `FAILED`.
- **D — tampered SBOM, SBOM *not* in checksums.txt:** script exit 1 at
  the SBOM signature step — cosign `invalid signature when validating
  ASN.1 encoded signature`. This is the case that proves signing the
  SBOMs directly (not just via checksums.txt) matters.

## Limitations (honest)

- **No live release exists.** The keyless path (GitHub OIDC identity,
  Rekor entry, `gh attestation verify`) is proven only by construction
  — the documented GoReleaser/cosign flags, a YAML-valid workflow, and
  the workflow's own `verify` job. The first real release is the first
  live proof.
- **This VM cannot talk to Sigstore's write path.** POSTs to
  `timestamp.sigstore.dev` and `rekor.sigstore.dev` hang (curl exit 28
  after 15 s) while GETs to the same hosts answer in ~2 s, so cosign
  v3.1.4 `sign-blob --bundle` cannot complete here and v3/v2
  `verify-blob` cannot do its default Rekor lookup here. The local
  proof therefore used cosign v2.4.3 keypair bundles
  (`--tlog-upload=false`) with `--insecure-ignore-tlog` in the
  script's `--key` mode only. The keyless path in the script never
  passes that flag. CI (GitHub's network) is unaffected.
- **Keypair mode is test-only.** It verifies a raw Ed25519 signature;
  there is no Rekor entry for a local key. Real releases always use
  the keyless bundles.
- **Docker image unsigned** (GoReleaser publishes no image today) and
  **the npm wrapper does not verify signatures** (it relies on
  `install.sh`'s checksum step). Both are follow-ups, recorded in
  `docs/RELEASE-SIGNING.md`.
- **`install.sh` without cosign installs checksum-only** (loud
  warning; `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses). Deliberate
  usability trade-off; the docs tell users to run `verify-release.sh`
  first.
- **GoReleaser is unpinned** (`version: latest` in the workflow).
- **CHECKLIST numbering:** rows 191–208 do not exist in CHECKLIST.md
  (other slices' numbering); rows 209–210 were appended after row 172
  at the end of the table.
- **No commit made by this slice.** The coordinator owns local
  commits; the tree had 85 changed entries from concurrent slices at
  gate time, so committing here would have swept in other slices'
  in-flight work. Files changed by this slice are listed below.
- **Approval notices:** four "approval not answered in time" runtime
  notices arrived during this task, each with no identifiable action
  attached (no tool call of this slice was blocked by them — all exec
  calls returned normally). Flagged for the parent to identify; nothing
  was retried on their basis.

## Files changed (this slice only)

- New: `internal/release/checksum.go`, `internal/release/sbom.go`,
  `internal/release/release_test.go`, `cmd/release-sbom/main.go`,
  `cmd/codvian-router/release_verify.go`,
  `.github/workflows/release.yml`, `scripts/verify-release.sh`,
  `scripts/release-sbom.sh`, `docs/RELEASE-SIGNING.md`,
  `docs/slice-209-210-release-signing-report.md`
- Edited: `.goreleaser.yml` (header + `sboms` + `signs`),
  `scripts/install.sh` (signature step + header),
  `docs/INSTALL.md` (status block, §§5–6), `README.md` + `README.v2.md`
  (status lines, install table), `CHECKLIST.md` (rows 209–210),
  `cmd/codvian-router/main.go` (dispatch case + usage lines only).
- Shared-file note: `.goreleaser.yml`, `scripts/install.sh`,
  `cmd/codvian-router/main.go`, `README.md`, `README.v2.md`,
  `docs/INSTALL.md`, `CHECKLIST.md` are shared with concurrent slices;
  edits were minimal and confined to this slice's concerns. No other
  slice's files were touched.
